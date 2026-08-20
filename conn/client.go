package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
)

// ClientConnection wraps a JSON-RPC connection for the client side.
// It dispatches incoming requests/notifications to the Client implementation,
// and provides methods to send requests to the agent.
type ClientConnection struct {
	conn                 *jsonrpc.Connection
	listenerHook         *connspi.SessionListenerHook // nil when transport has no listener capability
	client               acp.Client
	listenerErrHandler   func(sessionID string, err error)
	requestHandlers      map[string]requestDispatcher
	notificationHandlers map[string]notificationDispatcher
}

func isOrderedClientNotification(method string) bool {
	return method == acp.MethodClientSessionUpdate
}

// ClientConnectionOption configures a ClientConnection at construction time.
type ClientConnectionOption interface {
	applyClientConnectionOption(*clientConnectionConfig)
}

type clientConnectionConfig struct {
	listenerErrHandler         func(sessionID string, err error)
	orderedNotificationMatcher func(method string) bool
	rpcOpts                    []jsonrpc.ConnectionOption
}

type clientConnectionOptionFunc func(*clientConnectionConfig)

func (f clientConnectionOptionFunc) applyClientConnectionOption(cfg *clientConnectionConfig) {
	f(cfg)
}

func withJSONRPCClientConnectionOption(opt jsonrpc.ConnectionOption) ClientConnectionOption {
	return clientConnectionOptionFunc(func(cfg *clientConnectionConfig) {
		cfg.rpcOpts = append(cfg.rpcOpts, opt)
	})
}

// WithSessionListenerErrorHandler registers a callback invoked when a
// session-establishing RPC (new/load/resume) succeeds on the wire but the
// local GET SSE listener fails to start. This surfaces listener failures
// without conflating them with the RPC error.
//
// If no handler is registered, the listener error is logged at warning level.
func WithSessionListenerErrorHandler(fn func(sessionID string, err error)) ClientConnectionOption {
	return clientConnectionOptionFunc(func(cfg *clientConnectionConfig) {
		cfg.listenerErrHandler = fn
	})
}

// WithOrderedNotificationMatcher marks additional notification methods that
// must be processed sequentially. The built-in ordered handling for
// session/update is always retained.
func WithOrderedNotificationMatcher(matcher func(method string) bool) ClientConnectionOption {
	return clientConnectionOptionFunc(func(cfg *clientConnectionConfig) {
		cfg.orderedNotificationMatcher = matcher
	})
}

// WithMaxConsecutiveParseErrors terminates the connection after n consecutive
// invalid messages. Zero keeps the default unlimited behavior.
func WithMaxConsecutiveParseErrors(n int) ClientConnectionOption {
	return withJSONRPCClientConnectionOption(jsonrpc.WithMaxConsecutiveParseErrors(n))
}

// WithRequestTimeout sets a per-request timeout applied to inbound handler
// contexts. Zero keeps the default disabled behavior.
func WithRequestTimeout(d time.Duration) ClientConnectionOption {
	return withJSONRPCClientConnectionOption(jsonrpc.WithRequestTimeout(d))
}

// WithRequestWorkers overrides the per-connection worker pool size used for
// requests and unordered notifications.
func WithRequestWorkers(n int) ClientConnectionOption {
	return withJSONRPCClientConnectionOption(jsonrpc.WithRequestWorkers(n))
}

// WithConnectionLabel tags JSON-RPC diagnostic logs with an identifier.
func WithConnectionLabel(label string) ClientConnectionOption {
	return withJSONRPCClientConnectionOption(jsonrpc.WithConnectionLabel(label))
}

// WithNotificationErrorHandler registers a callback invoked when a
// notification handler returns an error (or panics). Since notifications
// have no response, failures would otherwise be visible only in SDK logs.
// Use this hook to feed metrics, alerts, or custom recovery policies.
//
// The callback runs synchronously from the dispatch goroutine; keep it
// short (emit a metric, enqueue to a channel, etc.). Panics inside the
// callback are recovered and logged by the SDK.
func WithNotificationErrorHandler(fn func(method string, err error)) ClientConnectionOption {
	return withJSONRPCClientConnectionOption(jsonrpc.WithNotificationErrorHandler(fn))
}

// NewClientConnection creates a new client-side connection.
//
// client and transport must be non-nil. Passing nil panics at construction
// time with a clear message; this is a programmer error, not a runtime
// condition.
//
// ClientConnectionOption values configure both ClientConnection-specific
// behavior and the underlying JSON-RPC connection. Ordered processing for
// session/update is always enabled, even when WithOrderedNotificationMatcher is
// used to add more ordered notification methods.
func NewClientConnection(client acp.Client, transport jsonrpc.Transport, opts ...ClientConnectionOption) *ClientConnection {
	if client == nil {
		panic("acp: NewClientConnection called with nil client")
	}
	if transport == nil {
		panic("acp: NewClientConnection called with nil transport")
	}

	cfg := clientConnectionConfig{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt.applyClientConnectionOption(&cfg)
	}

	csc := &ClientConnection{
		client:             client,
		listenerErrHandler: cfg.listenerErrHandler,
	}
	csc.listenerHook = connspi.GetSessionListenerHook(transport)
	csc.requestHandlers = newClientRequestHandlers(client)
	csc.notificationHandlers = newClientNotificationHandlers(client)

	orderedMatcher := isOrderedClientNotification
	if cfg.orderedNotificationMatcher != nil {
		userMatcher := cfg.orderedNotificationMatcher
		orderedMatcher = func(method string) bool {
			return isOrderedClientNotification(method) || userMatcher(method)
		}
	}
	allOpts := append([]jsonrpc.ConnectionOption{
		jsonrpc.WithOrderedNotificationMatcher(orderedMatcher),
	}, cfg.rpcOpts...)
	csc.conn = jsonrpc.NewConnection(
		transport,
		csc.handleRequest,
		csc.handleNotification,
		allOpts...,
	)
	return csc
}

// reportListenerError is invoked when a session-establishing RPC succeeds but the
// local GET SSE listener fails to start. The failure is surfaced via the
// registered handler or, if none is set, logged at warning level.
func (c *ClientConnection) reportListenerError(sessionID string, err error) {
	if c.listenerErrHandler != nil {
		c.listenerErrHandler(sessionID, err)
		return
	}
	acplog.Warn("session listener failed to start for session %s: %v", sessionID, err)
}

// startSessionListener starts a transport-level session listener (e.g. a GET
// SSE stream for Streamable HTTP) for the given session. Transports that do
// not support listeners are silently skipped. Startup and runtime failures
// are routed through reportListenerError so callers never observe them as
// RPC errors.
//
// Called by the generated new/load/resume wrappers to keep session
// listener lifecycle owned by ClientConnection rather than the generated
// call sites.
func (c *ClientConnection) startSessionListener(ctx context.Context, sessionID string) {
	if c.listenerHook == nil || sessionID == "" {
		return
	}
	onFailure := func(err error) { c.reportListenerError(sessionID, err) }
	if err := c.listenerHook.Start(ctx, sessionID, onFailure); err != nil {
		c.reportListenerError(sessionID, err)
	}
}

func (c *ClientConnection) beginSessionClose(sessionID string) bool {
	if c.listenerHook != nil && c.listenerHook.BeginClose != nil && sessionID != "" {
		return c.listenerHook.BeginClose(sessionID)
	}
	return false
}

func (c *ClientConnection) completeSessionClose(sessionID string) {
	if c.listenerHook != nil && c.listenerHook.CompleteClose != nil && sessionID != "" {
		c.listenerHook.CompleteClose(sessionID)
	}
}

func (c *ClientConnection) forceSessionClose(sessionID string) {
	if c.listenerHook != nil && c.listenerHook.ForceClose != nil && sessionID != "" {
		c.listenerHook.ForceClose(sessionID)
		return
	}
	c.completeSessionClose(sessionID)
}

func (c *ClientConnection) abortSessionClose(ctx context.Context, sessionID string, listenerExisted bool) {
	if !listenerExisted || c.listenerHook == nil || c.listenerHook.AbortClose == nil || sessionID == "" {
		return
	}
	if !c.listenerHook.AbortClose(sessionID) {
		// The listener disconnected after terminal intent was set and therefore
		// did not reconnect. An explicit RPC rejection keeps the session active,
		// so restore its reverse channel now.
		c.startSessionListener(ctx, sessionID)
	}
}

// closeSession owns the Streamable HTTP listener transition around
// session/close. BeginClose marks terminal intent without closing the current
// stream, so the Agent's close handler can still make reverse calls. A stream
// that disconnects while intent is set will not reconnect. A wire RPCError is
// an explicit rejection, so intent is aborted and a lost listener is restored.
// Transport errors, caller deadlines, connection shutdown, and malformed
// success results are outcome-uncertain: the Agent may already have closed the
// session, so they complete the local terminal state instead of reconnecting.
func (c *ClientConnection) closeSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	var response acp.CloseSessionResponse
	if err := ctx.Err(); err != nil {
		return response, err
	}
	if _, err := json.Marshal(params); err != nil {
		return response, fmt.Errorf("marshal params: %w", err)
	}
	sessionID := string(params.SessionID)
	listenerExisted := c.beginSessionClose(sessionID)

	raw, err := c.conn.SendRequest(ctx, acp.MethodAgentCloseSession, params)
	if err != nil {
		var rpcErr *acp.RPCError
		if errors.As(err, &rpcErr) || errors.Is(err, acptransport.ErrConnNotStarted) {
			c.abortSessionClose(ctx, sessionID, listenerExisted)
		} else {
			c.forceSessionClose(sessionID)
		}
		return response, err
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		c.forceSessionClose(sessionID)
		acplog.CtxError(ctx, "unmarshal response error: %v, raw response length: %d", err, len(raw))
		return response, acp.ErrInternalError("unmarshal response for "+acp.MethodAgentCloseSession, err)
	}
	c.completeSessionClose(sessionID)
	return response, nil
}

// Start begins processing messages in the background. It spawns the read
// loop and returns once the connection is ready to send/receive, or when
// ctx is cancelled.
//
// ctx is the connection lifetime context: cancelling it shuts down the
// connection. Use Done() to observe termination.
func (c *ClientConnection) Start(ctx context.Context) error {
	safe.Go(func() {
		_ = c.conn.Start(ctx)
	})
	return c.conn.WaitUntilStarted(ctx)
}

// Close shuts down the connection. If the underlying transport supports
// session listeners, any active GET SSE listeners are stopped first.
func (c *ClientConnection) Close() error {
	if c.listenerHook != nil && c.listenerHook.StopAll != nil {
		c.listenerHook.StopAll()
	}
	return c.conn.Close()
}

// Done returns a channel that's closed when the connection is done.
func (c *ClientConnection) Done() <-chan struct{} {
	return c.conn.Done()
}

// Err returns the terminal error that caused the connection to shut down,
// or nil if the connection closed cleanly (or has not yet terminated).
// Should be called after Done() has fired.
func (c *ClientConnection) Err() error {
	return c.conn.TerminalError()
}

// --- Extension outbound ---

// CallExtRequest sends a custom extension request to the agent.
//
// When this connection uses Streamable HTTP and multiple sessions or pending
// request streams may coexist, include `sessionId` in params so the remote side
// can route the request unambiguously.
func (c *ClientConnection) CallExtRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := acp.ValidateExtMethod(method); err != nil {
		return nil, err
	}
	return c.conn.SendRequest(ctx, method, params)
}

// CallExtNotification sends a custom extension notification to the agent.
//
// When this connection uses Streamable HTTP and routing is session-sensitive,
// include `sessionId` in params so the notification reaches the intended
// session.
func (c *ClientConnection) CallExtNotification(ctx context.Context, method string, params any) error {
	if err := acp.ValidateExtMethod(method); err != nil {
		return err
	}
	return c.conn.SendNotification(ctx, method, params)
}

// --- Inbound request dispatch ---

func (c *ClientConnection) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	var extHandler acp.ExtMethodHandler
	if h, ok := c.client.(acp.ExtMethodHandler); ok {
		extHandler = h
	}
	return dispatchRequest(ctx, method, params, c.requestHandlers, extHandler)
}

// --- Inbound notification dispatch ---

func (c *ClientConnection) handleNotification(ctx context.Context, method string, params json.RawMessage) error {
	var extHandler acp.ExtNotificationHandler
	if h, ok := c.client.(acp.ExtNotificationHandler); ok {
		extHandler = h
	}
	return dispatchNotification(ctx, method, params, c.notificationHandlers, extHandler)
}
