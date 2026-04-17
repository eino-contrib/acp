package conn

import (
	"context"
	"encoding/json"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
)

// ClientConnection wraps a JSON-RPC connection for the client side.
// It dispatches incoming requests/notifications to the Client implementation,
// and provides methods to send requests to the agent.
type ClientConnection struct {
	conn                 *jsonrpc.Connection
	transport            jsonrpc.Transport
	client               acp.Client
	logger               acplog.Logger
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
	logger                     acplog.Logger
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

// WithClientLogger sets the logger used by the ClientConnection for listener
// bootstrap diagnostics and the underlying jsonrpc.Connection.
func WithClientLogger(logger acplog.Logger) ClientConnectionOption {
	return clientConnectionOptionFunc(func(cfg *clientConnectionConfig) {
		cfg.logger = acplog.OrDefault(logger)
		cfg.rpcOpts = append(cfg.rpcOpts, jsonrpc.WithLogger(logger))
	})
}

// WithSessionListenerErrorHandler registers a callback invoked when a
// session-creating RPC (NewSession/LoadSession) succeeds on the wire but the
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

	cfg := clientConnectionConfig{
		logger: acplog.Default(),
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt.applyClientConnectionOption(&cfg)
	}

	csc := &ClientConnection{
		client:             client,
		transport:          transport,
		logger:             cfg.logger,
		listenerErrHandler: cfg.listenerErrHandler,
	}
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

// reportListenerError is invoked when a session-creating RPC succeeds but the
// local GET SSE listener fails to start. The failure is surfaced via the
// registered handler or, if none is set, logged at warning level.
func (c *ClientConnection) reportListenerError(sessionID string, err error) {
	if c.listenerErrHandler != nil {
		c.listenerErrHandler(sessionID, err)
		return
	}
	acplog.OrDefault(c.logger).Warn("session listener failed to start for session %s: %v", sessionID, err)
}

// Start begins processing messages in the background. It spawns the read
// loop and returns once the connection is ready to send/receive, or when
// ctx is cancelled.
//
// ctx is the connection lifetime context: cancelling it shuts down the
// connection. Use Done() to observe termination.
func (c *ClientConnection) Start(ctx context.Context) error {
	safe.GoWithLogger(c.logger, func() {
		_ = c.conn.Start(ctx)
	})
	return c.conn.WaitUntilStarted(ctx)
}

// Close shuts down the connection. If the underlying transport supports
// session listeners, any active GET SSE listeners are stopped first.
func (c *ClientConnection) Close() error {
	if hook := connspi.GetSessionListenerHook(c.transport); hook != nil {
		hook.Stop()
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
