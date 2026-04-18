package conn

import (
	"context"
	"encoding/json"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	"github.com/eino-contrib/acp/internal/safe"
)

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
func NewAgentConnectionFromTransport(agent acp.Agent, transport jsonrpc.Transport, opts ...jsonrpc.ConnectionOption) *AgentConnection {
	if agent == nil {
		panic("acp: NewAgentConnectionFromTransport called with nil agent")
	}
	if transport == nil {
		panic("acp: NewAgentConnectionFromTransport called with nil transport")
	}
	asc := &AgentConnection{
		agent: agent,
	}
	asc.requestHandlers = newAgentRequestHandlers(agent, asc)
	asc.notificationHandlers = newAgentNotificationHandlers(asc)
	conn := jsonrpc.NewConnection(transport, asc.handleRequest, asc.handleNotification, opts...)
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
