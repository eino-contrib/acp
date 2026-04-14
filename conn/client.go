package conn

import (
	"context"
	"encoding/json"
	"fmt"

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
type ClientConnectionOption func(*ClientConnection)

// WithClientLogger sets the logger used by the ClientConnection for listener
// bootstrap diagnostics. The underlying jsonrpc.Connection has its own logger
// option (jsonrpc.WithLogger) applied via the opts variadic passed through.
func WithClientLogger(logger acplog.Logger) ClientConnectionOption {
	return func(c *ClientConnection) {
		c.logger = acplog.OrDefault(logger)
	}
}

// WithSessionListenerErrorHandler registers a callback invoked when a
// session-creating RPC (NewSession/LoadSession) succeeds on the wire but the
// local GET SSE listener fails to start. This surfaces listener failures
// without conflating them with the RPC error.
//
// If no handler is registered, the listener error is logged at warning level.
func WithSessionListenerErrorHandler(fn func(sessionID string, err error)) ClientConnectionOption {
	return func(c *ClientConnection) {
		c.listenerErrHandler = fn
	}
}

// NewClientConnection creates a new client-side connection.
//
// client and transport must be non-nil. Passing nil panics at construction
// time with a clear message; this is a programmer error, not a runtime
// condition.
//
// Extra jsonrpc.ConnectionOption values (e.g. WithRequestTimeout, WithLogger,
// WithMaxConsecutiveParseErrors) are applied on top of the built-in ordered
// notification matcher for session/update. ClientConnectionOption values
// configure ClientConnection-level behavior (e.g. listener error handling).
func NewClientConnection(client acp.Client, transport jsonrpc.Transport, opts ...any) *ClientConnection {
	if client == nil {
		panic("acp: NewClientConnection called with nil client")
	}
	if transport == nil {
		panic("acp: NewClientConnection called with nil transport")
	}

	csc := &ClientConnection{client: client, transport: transport, logger: acplog.Default()}

	var rpcOpts []jsonrpc.ConnectionOption
	for _, opt := range opts {
		switch o := opt.(type) {
		case jsonrpc.ConnectionOption:
			rpcOpts = append(rpcOpts, o)
		case ClientConnectionOption:
			o(csc)
		default:
			panic(fmt.Sprintf("acp: NewClientConnection received unsupported option type %T", opt))
		}
	}

	csc.requestHandlers = newClientRequestHandlers(client)
	csc.notificationHandlers = newClientNotificationHandlers(client)
	allOpts := append([]jsonrpc.ConnectionOption{
		jsonrpc.WithOrderedNotificationMatcher(isOrderedClientNotification),
	}, rpcOpts...)
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
	safe.GoWithLogger(acplog.Default(), func() {
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
