package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	"github.com/eino-contrib/acp/internal/wsutil"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	// defaultRequestTimeout bounds a single Streamable HTTP POST waiting for
	// its final response. 5 min accommodates long-running agent turns (tool
	// loops, model calls) while preventing goroutine leaks from stalled peers.
	defaultRequestTimeout = 5 * time.Minute
	// defaultConnectionIdleTimeout evicts HTTP remote connections with no
	// recent POST or GET activity. 5 min matches typical HTTP keepalive
	// windows and releases per-connection state after clients disappear.
	defaultConnectionIdleTimeout = 5 * time.Minute
	// defaultWSInitializeTimeout bounds how long a freshly upgraded WebSocket
	// client has to send its initialize request before the server tears the
	// connection down. Prevents idle half-open upgrades from holding state.
	defaultWSInitializeTimeout = 15 * time.Second
)

// AgentFactory creates a new Agent for a single remote ACP connection.
// The context carries connection bootstrap values (for example request-scoped
// authentication or trace metadata) but is detached from request cancellation
// before being used as the long-lived parent of the remote connection.
type AgentFactory func(ctx context.Context) acp.Agent

// ConnectionAwareAgent is an optional interface. If an Agent returned by
// AgentFactory also implements this interface, the server automatically
// injects the per-connection AgentConnection so the agent can make reverse
// calls (e.g. ReadTextFile, SessionUpdate) back to the client.
type ConnectionAwareAgent interface {
	SetClientConnection(*acpconn.AgentConnection)
}

// HTTPContext is the framework-neutral HTTP/SSE request contract consumed by
// ServeHTTP. Framework adapters implement it over their native request and
// response objects; application code normally does not need to implement it.
type HTTPContext interface {
	Context() context.Context
	RequestHeader(key string) string
	RequestBody() ([]byte, error)
	// RequestBodyLimited must stop reading after maxBytes+1 bytes and return
	// ErrRequestBodyTooLarge when the body exceeds maxBytes.
	RequestBodyLimited(maxBytes int64) ([]byte, error)
	SetResponseHeader(key, value string)
	WriteError(code int, msg string)
	SetStatusCode(code int)
	Flush()
	Done() <-chan struct{}
	WriteSSEEvent(msg json.RawMessage) error
	WriteSSEKeepAlive() error
	CloseSSE()
}

// WebSocketConn is the framework-neutral, already-upgraded WebSocket contract
// accepted by WebSocketAdmission. The built-in Hertz and Gin adapters provide
// implementations; custom adapters may implement the same method set.
type WebSocketConn interface {
	ReadMessage() (messageType int, payload []byte, err error)
	WriteMessage(messageType int, payload []byte) error
	WriteControl(messageType int, payload []byte, deadline time.Time) error
	SetReadLimit(limit int64)
	SetReadDeadline(deadline time.Time) error
	SetWriteDeadline(deadline time.Time) error
	SetPingHandler(handler func(appData string) error)
	Close() error
}

// Option configures an ACPServer.
type Option func(*ACPServer)

// ErrServerClosed is returned when a new connection is attempted after the
// server has begun shutting down.
var ErrServerClosed = errors.New("acp: server is closed")

// ErrRequestBodyTooLarge is returned by HTTPContext.RequestBodyLimited when a
// request body exceeds the maximum size passed by ACPServer.
var ErrRequestBodyTooLarge = acphttpserver.ErrRequestBodyTooLarge

// DefaultEndpoint is the conventional route on which an adapter may be
// registered. The route itself is owned by the host router; ACPServer does
// not store or mount an endpoint.
const DefaultEndpoint = acptransport.DefaultACPEndpointPath

// WithRequestTimeout sets the maximum duration for a single Streamable HTTP
// POST request to wait for its final response. Zero disables the timeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(s *ACPServer) {
		s.requestTimeout = d
	}
}

// WithConnectionIdleTimeout sets how long an HTTP remote connection may remain
// idle before it is evicted. Zero or a negative value disables idle eviction.
func WithConnectionIdleTimeout(d time.Duration) Option {
	return func(s *ACPServer) {
		s.connectionIdleTimeout = d
	}
}

// WithPendingQueueSize sets the per-session pending message buffer size used
// when no GET SSE stream is bound yet. Default is 1024.
func WithPendingQueueSize(size int) Option {
	return func(s *ACPServer) {
		if size > 0 {
			s.pendingQueueSize = size
		}
	}
}

// WithMaxHTTPMessageSize caps the size (in bytes) of a single Streamable HTTP
// POST body. A non-positive value selects transport.DefaultMaxMessageSize
// (10MB). Bodies exceeding the cap are rejected with HTTP 413.
func WithMaxHTTPMessageSize(size int) Option {
	return func(s *ACPServer) {
		s.maxHTTPMessageSize = size
	}
}

// WithMaxInflightDispatch caps the number of concurrent direct-dispatch
// handlers (both requests and notifications) on a single HTTP connection.
// Zero selects acphttpserver.DefaultMaxInflightDispatch; a negative value
// disables the cap. Overflow surfaces as HTTP 503 so misbehaving peers or
// long-running handlers cannot grow goroutines without bound.
func WithMaxInflightDispatch(n int) Option {
	return func(s *ACPServer) {
		s.maxInflightDispatch = n
	}
}

// WithNotificationErrorHandler registers a callback invoked when a
// client-to-agent notification handler returns an error (or panics) on a
// WebSocket/stdio-backed connection. Notifications have no response, so
// failures would otherwise only be visible in SDK logs. Use this hook to
// feed metrics or custom recovery policies.
//
// The callback is NOT invoked for HTTP direct-dispatch notifications: that
// transport does not run a background JSON-RPC read loop. HTTP notification
// errors continue to be logged at error level.
//
// The callback runs synchronously from the dispatch goroutine; keep it
// short. Panics inside the callback are recovered and logged.
func WithNotificationErrorHandler(fn func(method string, err error)) Option {
	return func(s *ACPServer) {
		s.notificationErrorHandler = fn
	}
}

// WithWebSocketReadTimeout sets the read deadline for WebSocket connections
// after initialization completes. If no frame (data or Ping) arrives within
// this window, the connection is closed. Zero disables the deadline (default).
// Recommended: >= 2 × Client PingInterval (e.g. 75s for 30s PingInterval).
// Enabling this when upstream clients do not send Ping or periodic data frames
// will cause idle connections to be disconnected.
// Production environments should set this to 75s once all clients support Ping.
func WithWebSocketReadTimeout(d time.Duration) Option {
	return func(s *ACPServer) {
		if !wsutil.ValidateDuration("server", "WithWebSocketReadTimeout", d, time.Second) {
			return
		}
		s.wsReadTimeout = d
	}
}

// WithWebSocketInitializeTimeout sets the deadline for clients to send the
// initialize request after WebSocket upgrade. Zero disables the deadline.
// Default: 15s.
func WithWebSocketInitializeTimeout(d time.Duration) Option {
	return func(s *ACPServer) {
		if !wsutil.ValidateDuration("server", "WithWebSocketInitializeTimeout", d, time.Second) {
			return
		}
		s.wsInitializeTimeout = d
	}
}

// ACPServer exposes ACP over Streamable HTTP and WebSocket.
//
// Each remote connection gets its own Agent instance and AgentConnection,
// which keeps JSON-RPC request IDs scoped correctly and makes extension
// requests/notifications unambiguous.
type ACPServer struct {
	factory                  AgentFactory
	requestTimeout           time.Duration
	connectionIdleTimeout    time.Duration
	pendingQueueSize         int
	maxHTTPMessageSize       int
	maxInflightDispatch      int
	notificationErrorHandler func(method string, err error)
	wsReadTimeout            time.Duration
	wsInitializeTimeout      time.Duration

	conns      *connTable
	done       chan struct{}
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// lifecycleMu is the admission boundary shared by Close, HTTP connection
	// registration, and WebSocket registration. Once closing flips to true,
	// no connection can be added to either registry.
	lifecycleMu  sync.Mutex
	closing      bool
	wsAdmissions map[*WebSocketAdmission]struct{}
	active       sync.WaitGroup
	closeOnce    sync.Once
	drained      chan struct{}
}

// NewACPServer builds a remote ACP server without mounting it.
//
// The factory returns an acp.Agent. If the returned agent also implements
// ConnectionAwareAgent, the server injects the per-connection
// AgentConnection automatically so the agent can make reverse calls.
//
// Use server/hertz or server/gin to create a framework-native handler, then
// register that handler on a route owned by the host application.
func NewACPServer(factory AgentFactory, opts ...Option) (*ACPServer, error) {
	if factory == nil {
		return nil, fmt.Errorf("acp: agent factory must not be nil")
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	s := &ACPServer{
		factory:               factory,
		requestTimeout:        defaultRequestTimeout,
		connectionIdleTimeout: defaultConnectionIdleTimeout,
		wsInitializeTimeout:   defaultWSInitializeTimeout,
		done:                  make(chan struct{}),
		drained:               make(chan struct{}),
		rootCtx:               rootCtx,
		rootCancel:            rootCancel,
		wsAdmissions:          make(map[*WebSocketAdmission]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	s.conns = newConnTable(s.connectionIdleTimeout)
	return s, nil
}

// createAgent invokes user code behind a panic boundary. A factory panic is a
// connection-local setup failure: HTTP callers receive the existing generic
// 5xx response and an already-upgraded WebSocket receives a generic 1011.
// The recovered value is retained in the internal error for diagnostics but
// is never written to the peer.
func (s *ACPServer) createAgent(ctx context.Context) (agent acp.Agent, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			agent = nil
			err = fmt.Errorf("agent factory panic: %v", recovered)
		}
	}()
	agent = s.factory(ctx)
	if isNilAgent(agent) {
		return nil, fmt.Errorf("agent factory returned nil")
	}
	return agent, nil
}

// setClientConnection invokes the optional connection hook behind the same
// connection-setup panic boundary as AgentFactory. Implementations are user
// code, so a panic must fail only this connection rather than escape through
// the HTTP or WebSocket serving stack.
func setClientConnection(agent acp.Agent, conn *acpconn.AgentConnection) (err error) {
	aware, ok := agent.(ConnectionAwareAgent)
	if !ok {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("set client connection panic: %v", recovered)
		}
	}()
	aware.SetClientConnection(conn)
	return nil
}

func isNilAgent(agent acp.Agent) bool {
	if agent == nil {
		return true
	}
	value := reflect.ValueOf(agent)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// ServeHTTP dispatches one Streamable HTTP request through the shared ACP
// protocol implementation. This is an adapter-facing SPI; framework adapters
// construct the internal HTTP context and pass the native request method.
func (s *ACPServer) ServeHTTP(ctx HTTPContext, method string) {
	if s == nil || ctx == nil {
		return
	}
	if s.IsClosing() {
		ctx.WriteError(http.StatusServiceUnavailable, "ACP server is shutting down")
		return
	}
	acphttpserver.ServeHTTPProtocol(ctx, s.protocolServer(), method)
}

// protocolServer builds the strategy object consumed by internal/httpserver
// for the shared Streamable HTTP POST/GET/DELETE handlers.
func (s *ACPServer) protocolServer() acphttpserver.ProtocolServer {
	return acphttpserver.ProtocolServer{
		CreateConnection: func(ctx context.Context) (*acphttpserver.ProtocolConnection, int, error) {
			conn, err := s.newHTTPConnection(ctx)
			if err != nil {
				if errors.Is(err, ErrServerClosed) {
					return nil, http.StatusServiceUnavailable, err
				}
				return nil, http.StatusInternalServerError, err
			}
			return conn.ProtocolConnection(), 0, nil
		},
		LookupConnection: func(connectionID string) (*acphttpserver.ProtocolConnection, bool) {
			conn, ok := s.conns.get(connectionID)
			if !ok {
				return nil, false
			}
			return conn.ProtocolConnection(), true
		},
		DeleteConnection: func(connectionID string) bool {
			_, ok := s.conns.delete(connectionID)
			return ok
		},
		RequestTimeout:    s.requestTimeout,
		KeepAliveInterval: acphttpserver.SSEKeepaliveInterval,
		MaxMessageSize:    s.maxHTTPMessageSize,
	}
}

// beginAdmission reserves one lifecycle slot. The returned release function
// is idempotent so setup failure and connection Close paths may safely race.
func (s *ACPServer) beginAdmission() (func(), error) {
	if s == nil {
		return nil, ErrServerClosed
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return nil, ErrServerClosed
	}
	s.active.Add(1)
	var once sync.Once
	return func() { once.Do(s.active.Done) }, nil
}

// IsClosing reports whether Close or Shutdown has started. Adapters normally
// use WebSocket admission rather than checking this value themselves.
func (s *ACPServer) IsClosing() bool {
	if s == nil {
		return true
	}
	s.lifecycleMu.Lock()
	closing := s.closing
	s.lifecycleMu.Unlock()
	return closing
}

// Close atomically stops admission, cancels connection contexts, and starts
// resource cleanup. It is idempotent and intentionally does not wait for
// connection handlers or user factories to return; use Shutdown to wait.
func (s *ACPServer) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closing = true
		if s.rootCancel != nil {
			s.rootCancel()
		}
		close(s.done)
		wsAdmissions := make([]*WebSocketAdmission, 0, len(s.wsAdmissions))
		for admission := range s.wsAdmissions {
			wsAdmissions = append(wsAdmissions, admission)
		}
		s.lifecycleMu.Unlock()

		go func() {
			s.conns.close()
			for _, admission := range wsAdmissions {
				admission.closeFromServer()
			}
			s.active.Wait()
			close(s.drained)
		}()
	})
	return nil
}

// Shutdown starts Close and waits until all admitted HTTP and WebSocket
// connections have released their lifecycle slots, or ctx expires.
func (s *ACPServer) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_ = s.Close()
	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ACPServer) releaseWSAdmission(admission *WebSocketAdmission) {
	s.lifecycleMu.Lock()
	delete(s.wsAdmissions, admission)
	s.lifecycleMu.Unlock()
	s.active.Done()
}
