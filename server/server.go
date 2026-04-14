package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/route"
	"github.com/hertz-contrib/websocket"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	acplog "github.com/eino-contrib/acp/internal/log"
	acphttp "github.com/eino-contrib/acp/transport/http"
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

// Option configures an ACPServer.
type Option func(*ACPServer)

// WithEndpoint overrides the HTTP endpoint. The default is /acp.
func WithEndpoint(endpoint string) Option {
	return func(s *ACPServer) {
		if endpoint != "" {
			s.endpoint = endpoint
		}
	}
}

// WithLogger enables server-side transport logging.
func WithLogger(logger acplog.Logger) Option {
	return func(s *ACPServer) {
		if logger == nil {
			logger = acplog.Default()
		}
		s.logger = logger
	}
}

// WithWebSocketUpgrader overrides the WebSocket upgrader.
// The default is a zero-value websocket.HertzUpgrader.
func WithWebSocketUpgrader(upgrader websocket.HertzUpgrader) Option {
	return func(s *ACPServer) {
		s.upgrader = upgrader
	}
}

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
// when no GET SSE stream is bound yet. Default is 256.
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

// ACPServer exposes ACP over Streamable HTTP and WebSocket.
//
// Each remote connection gets its own Agent instance and AgentConnection,
// which keeps JSON-RPC request IDs scoped correctly and makes extension
// requests/notifications unambiguous.
type ACPServer struct {
	factory               AgentFactory
	endpoint              string
	logger                acplog.Logger
	upgrader              websocket.HertzUpgrader
	requestTimeout        time.Duration
	connectionIdleTimeout time.Duration
	pendingQueueSize      int
	maxHTTPMessageSize    int

	conns      *connTable
	done       chan struct{}
	rootCtx    context.Context
	rootCancel context.CancelFunc
	once       sync.Once
}

// NewACPServer builds a remote ACP server without mounting it.
//
// The factory returns an acp.Agent. If the returned agent also implements
// ConnectionAwareAgent, the server injects the per-connection
// AgentConnection automatically so the agent can make reverse calls.
//
// Call Mount or HertzHandler to attach it to a Hertz router.
func NewACPServer(factory AgentFactory, opts ...Option) (*ACPServer, error) {
	if factory == nil {
		return nil, fmt.Errorf("acp: agent factory must not be nil")
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	s := &ACPServer{
		factory:               factory,
		endpoint:              acphttp.DefaultACPEndpointPath,
		logger:                acplog.Default(),
		requestTimeout:        defaultRequestTimeout,
		connectionIdleTimeout: defaultConnectionIdleTimeout,
		done:                  make(chan struct{}),
		rootCtx:               rootCtx,
		rootCancel:            rootCancel,
		upgrader:              websocket.HertzUpgrader{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.conns = newConnTable(s.connectionIdleTimeout, s.logger)
	return s, nil
}

// Mount registers the remote ACP endpoint on the given Hertz router.
func (s *ACPServer) Mount(router route.IRoutes) {
	if router == nil {
		return
	}
	router.Any(s.endpoint, s.Handler())
}

// protocolServer builds the strategy object consumed by internal/httpserver
// for the shared Streamable HTTP POST/GET/DELETE handlers.
func (s *ACPServer) protocolServer() acphttpserver.ProtocolServer {
	return acphttpserver.ProtocolServer{
		CreateConnection: func(ctx context.Context) (*acphttpserver.ProtocolConnection, int, error) {
			conn, err := s.newHTTPConnection(ctx)
			if err != nil {
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
		Logger:            s.logger,
	}
}

// Close terminates every active remote connection.
func (s *ACPServer) Close() error {
	s.once.Do(func() {
		if s.rootCancel != nil {
			s.rootCancel()
		}
		close(s.done)
		s.conns.close()
	})
	return nil
}
