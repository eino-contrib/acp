package conn

import (
	"context"
	"encoding/json"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	"github.com/eino-contrib/acp/internal/safe"
)

// AgentConnectionOption configures an AgentConnection at construction time.
type AgentConnectionOption interface {
	applyAgentConnectionOption(*agentConnectionConfig)
}

type agentConnectionConfig struct {
	rpcOpts []jsonrpc.ConnectionOption
}

type agentConnectionOptionFunc func(*agentConnectionConfig)

func (f agentConnectionOptionFunc) applyAgentConnectionOption(cfg *agentConnectionConfig) {
	f(cfg)
}

func withJSONRPCAgentConnectionOption(opt jsonrpc.ConnectionOption) AgentConnectionOption {
	return agentConnectionOptionFunc(func(cfg *agentConnectionConfig) {
		cfg.rpcOpts = append(cfg.rpcOpts, opt)
	})
}

// WithAgentMaxConsecutiveParseErrors terminates the connection after n
// consecutive invalid messages. Zero keeps the default unlimited behavior.
func WithAgentMaxConsecutiveParseErrors(n int) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithMaxConsecutiveParseErrors(n))
}

// WithAgentRequestTimeout sets a per-request timeout applied to inbound
// handler contexts. Zero keeps the default disabled behavior.
func WithAgentRequestTimeout(d time.Duration) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithRequestTimeout(d))
}

// WithAgentRequestWorkers overrides the per-connection worker pool size used
// for requests and unordered notifications.
func WithAgentRequestWorkers(n int) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithRequestWorkers(n))
}

// WithAgentMaxPendingDispatch caps the depth of each dispatch queue (the
// shared request/notification intake and the ordered-notification queue).
// Exceeding the cap terminates the connection with a ServerBusy error.
//
// Default is an internal safe ceiling; pass a positive value to tune, or a
// negative value to explicitly disable the cap (unbounded).
func WithAgentMaxPendingDispatch(n int) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithMaxPendingDispatch(n))
}

// WithAgentShutdownTimeout bounds how long Close/Start wait for queued
// notification handlers to drain after shutdown. A positive duration sets the
// timeout; zero selects the default; a negative value waits forever.
func WithAgentShutdownTimeout(d time.Duration) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithShutdownTimeout(d))
}

// WithAgentConnectionLabel tags JSON-RPC diagnostic logs with an identifier.
func WithAgentConnectionLabel(label string) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithConnectionLabel(label))
}

// WithAgentNotificationErrorHandler registers a callback invoked when a
// notification handler returns an error (or panics). Since notifications
// have no response, failures would otherwise be visible only in SDK logs.
// Use this hook to feed metrics, alerts, or custom recovery policies.
//
// The callback runs synchronously from the dispatch goroutine; keep it
// short (emit a metric, enqueue to a channel, etc.). Panics inside the
// callback are recovered and logged by the SDK.
func WithAgentNotificationErrorHandler(fn func(method string, err error)) AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithNotificationErrorHandler(fn))
}

// WithAgentSanitizedErrors opts into wire error sanitization: generic handler
// errors, handler panics, and response marshal failures are sent without host
// internals (no originError data, panic text, or stack). Deliberately returned
// *acp.RPCError values keep their data, and local logging is unchanged.
func WithAgentSanitizedErrors() AgentConnectionOption {
	return withJSONRPCAgentConnectionOption(jsonrpc.WithSanitizedErrors())
}

// AgentConnection wraps a send-capable endpoint for the agent side.
// It dispatches incoming requests to the Agent implementation, and provides
// methods to send requests/notifications to the client.
type AgentConnection struct {
	sender               connspi.Sender
	jsonrpcConn          *jsonrpc.Connection // non-nil only for WS/stdio (has read loop)
	agent                acp.Agent
	requestHandlers      map[string]requestDispatcher
	notificationHandlers map[string]notificationDispatcher
}

// NewAgentConnectionFromTransport creates a new agent-side connection backed by a
// JSON-RPC transport. Used for WebSocket and stdio transports.
//
// agent and transport must be non-nil. Passing nil panics at construction time
// with a clear message; this is a programmer error, not a runtime condition.
//
// AgentConnectionOption values configure the underlying JSON-RPC connection
// (dispatch queue limits, shutdown wait, per-request timeout and workers,
// connection label, notification-error handler, wire error sanitization).
func NewAgentConnectionFromTransport(agent acp.Agent, transport jsonrpc.Transport, opts ...AgentConnectionOption) *AgentConnection {
	if agent == nil {
		panic("acp: NewAgentConnectionFromTransport called with nil agent")
	}
	if transport == nil {
		panic("acp: NewAgentConnectionFromTransport called with nil transport")
	}
	cfg := agentConnectionConfig{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt.applyAgentConnectionOption(&cfg)
	}
	asc := &AgentConnection{
		agent: agent,
	}
	asc.requestHandlers = newAgentRequestHandlers(agent, asc)
	asc.notificationHandlers = newAgentNotificationHandlers(asc)
	conn := jsonrpc.NewConnection(transport, asc.handleRequest, asc.handleNotification, cfg.rpcOpts...)
	asc.jsonrpcConn = conn
	asc.sender = conn // *jsonrpc.Connection satisfies connspi.Sender
	return asc
}

// NewAgentConnectionSPI constructs an agent-side connection backed by an SDK
// internal Sender. This constructor is sealed: it requires a capability
// token (connspi.AgentSPIKey) whose type lives in internal/connspi and so
// cannot be named — let alone constructed — by code outside this module.
//
// Used for HTTP direct-dispatch where there is no JSON-RPC read loop.
// External callers should use NewAgentConnectionFromTransport instead.
//
// agent and sender must be non-nil; passing nil panics.
func NewAgentConnectionSPI(_ connspi.AgentSPIKey, agent acp.Agent, sender connspi.Sender) *AgentConnection {
	if agent == nil {
		panic("acp: NewAgentConnectionSPI called with nil agent")
	}
	if sender == nil {
		panic("acp: NewAgentConnectionSPI called with nil sender")
	}
	asc := &AgentConnection{
		agent:  agent,
		sender: sender,
	}
	asc.requestHandlers = newAgentRequestHandlers(agent, asc)
	asc.notificationHandlers = newAgentNotificationHandlers(asc)
	return asc
}

// Start begins processing messages in the background. It spawns the read
// loop and returns once the connection is ready to send/receive, or when
// ctx is cancelled.
//
// ctx is the connection lifetime context: cancelling it shuts down the
// connection. Use Done() to observe termination. For HTTP mode there is no
// read loop and Start is a no-op.
func (a *AgentConnection) Start(ctx context.Context) error {
	if a.jsonrpcConn == nil {
		return nil
	}
	safe.Go(func() {
		_ = a.jsonrpcConn.Start(ctx)
	})
	return a.jsonrpcConn.WaitUntilStarted(ctx)
}

// Close shuts down the connection.
func (a *AgentConnection) Close() error {
	if a.jsonrpcConn != nil {
		return a.jsonrpcConn.Close()
	}
	return a.sender.Close()
}

// Done returns a channel that's closed when the connection is done.
func (a *AgentConnection) Done() <-chan struct{} {
	if a.jsonrpcConn != nil {
		return a.jsonrpcConn.Done()
	}
	return a.sender.Done()
}

// Err returns the terminal error that caused the connection to shut down,
// or nil if the connection closed cleanly (or has not yet terminated).
// Should be called after Done() has fired. HTTP direct-dispatch connections
// have no long-lived read loop and always return nil.
func (a *AgentConnection) Err() error {
	if a.jsonrpcConn != nil {
		return a.jsonrpcConn.TerminalError()
	}
	return nil
}

// --- Extension outbound ---

// CallExtRequest sends a custom extension request to the client.
//
// When this connection uses Streamable HTTP and multiple sessions or pending
// request streams may coexist, include `sessionId` in params so the remote side
// can route the request unambiguously.
func (a *AgentConnection) CallExtRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := acp.ValidateExtMethod(method); err != nil {
		return nil, err
	}
	return a.sender.SendRequest(ctx, method, params)
}

// CallExtNotification sends a custom extension notification to the client.
//
// When this connection uses Streamable HTTP and routing is session-sensitive,
// include `sessionId` in params so the notification reaches the intended
// session.
func (a *AgentConnection) CallExtNotification(ctx context.Context, method string, params any) error {
	if err := acp.ValidateExtMethod(method); err != nil {
		return err
	}
	return a.sender.SendNotification(ctx, method, params)
}

// --- Inbound dispatch ---

// InboundDispatcherSPI returns the request/notification dispatch closures
// bound to this connection. The method is sealed with a capability token
// from internal/connspi so external code cannot name or construct its
// argument; only SDK glue (the HTTP direct-dispatch path in
// internal/httpserver) can drive the connection through this hook.
func (a *AgentConnection) InboundDispatcherSPI(_ connspi.AgentSPIKey) connspi.Dispatcher {
	return connspi.Dispatcher{
		Request:      a.handleRequest,
		Notification: a.handleNotification,
	}
}

// --- Inbound request dispatch ---

func (a *AgentConnection) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	var extHandler acp.ExtMethodHandler
	if h, ok := a.agent.(acp.ExtMethodHandler); ok {
		extHandler = h
	}
	return dispatchRequest(ctx, method, params, a.requestHandlers, extHandler)
}

// --- Inbound notification dispatch ---

func (a *AgentConnection) handleNotification(ctx context.Context, method string, params json.RawMessage) error {
	var extHandler acp.ExtNotificationHandler
	if h, ok := a.agent.(acp.ExtNotificationHandler); ok {
		extHandler = h
	}
	return dispatchNotification(ctx, method, params, a.notificationHandlers, extHandler)
}
