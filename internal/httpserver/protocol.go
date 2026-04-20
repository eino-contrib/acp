package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	acplog "github.com/eino-contrib/acp/internal/log"
	acptransport "github.com/eino-contrib/acp/transport"
)

// ProtocolConnection.

// ProtocolConnection is the per-connection state needed by the shared
// Streamable HTTP POST/GET/DELETE handlers.
type ProtocolConnection struct {
	connectionID string
	httpConn     *Connection

	// Direct-dispatch inbound closures.
	dispatcher  connspi.Dispatcher
	pendingReqs *PendingRequests

	// dispatchSlots is a counting semaphore that caps the number of concurrent
	// in-flight direct-dispatch handlers on this connection. Both request and
	// notification paths acquire a slot before invoking the user handler and
	// release it when the handler returns. Overflow (non-blocking acquire
	// fails) fails the POST fast with ErrServerBusy, mirroring the overflow
	// behavior of jsonrpc.Connection for WS/stdio — without this cap a burst
	// of slow handlers could spawn unbounded goroutines via safe.Go() inside
	// handleDirectRequestPost, since RequestTimeout only unblocks the outer
	// HTTP handler and does not abort the background dispatch goroutine.
	//
	// nil when the connection is created without a cap (MaxInflightDispatch
	// <= 0 intentionally disables the limit).
	dispatchSlots chan struct{}

	done    <-chan struct{}
	closeFn func() error
}

// ProtocolConnectionConfig holds the configuration for creating a ProtocolConnection.
type ProtocolConnectionConfig struct {
	ConnectionID string
	HTTPConn     *Connection
	Dispatcher   connspi.Dispatcher
	PendingReqs  *PendingRequests
	Done         <-chan struct{}
	Close        func() error
	// MaxInflightDispatch caps the number of concurrent direct-dispatch
	// handlers. Zero selects DefaultMaxInflightDispatch. Negative disables
	// the cap.
	MaxInflightDispatch int
}

// DefaultMaxInflightDispatch is the default cap on concurrent direct-dispatch
// handlers per HTTP connection. Chosen to match jsonrpc.defaultMaxPendingDispatch
// so the two transports shed load at comparable thresholds.
const DefaultMaxInflightDispatch = 4096

// NewProtocolConnection creates a ProtocolConnection from the given config.
func NewProtocolConnection(cfg ProtocolConnectionConfig) *ProtocolConnection {
	pc := &ProtocolConnection{
		connectionID: cfg.ConnectionID,
		httpConn:     cfg.HTTPConn,
		dispatcher:   cfg.Dispatcher,
		pendingReqs:  cfg.PendingReqs,
		done:         cfg.Done,
		closeFn:      cfg.Close,
	}
	switch {
	case cfg.MaxInflightDispatch == 0:
		pc.dispatchSlots = make(chan struct{}, DefaultMaxInflightDispatch)
	case cfg.MaxInflightDispatch > 0:
		pc.dispatchSlots = make(chan struct{}, cfg.MaxInflightDispatch)
	}
	return pc
}

// tryAcquireDispatch attempts to reserve a dispatch slot without blocking.
// Returns a release func on success and nil on overflow. When no cap is
// configured (dispatchSlots == nil) acquire always succeeds and the release
// func is a no-op.
func (c *ProtocolConnection) tryAcquireDispatch() func() {
	if c == nil || c.dispatchSlots == nil {
		return func() {}
	}
	select {
	case c.dispatchSlots <- struct{}{}:
		slots := c.dispatchSlots
		return func() { <-slots }
	default:
		return nil
	}
}

// ConnectionID returns the connection identifier.
func (c *ProtocolConnection) ConnectionID() string {
	if c == nil {
		return ""
	}
	return c.connectionID
}

// Done returns a channel that is closed when the connection is no longer usable.
func (c *ProtocolConnection) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

// IsValid reports whether the protocol connection is usable. A closed
// underlying connection is not considered valid, so lookups that race with
// CloseConnection or a failed initialize cannot hand the connection back out.
func (c *ProtocolConnection) IsValid() bool {
	return c != nil && c.httpConn != nil && !c.httpConn.IsClosed()
}

// Touch records activity on the underlying HTTP connection.
func (c *ProtocolConnection) Touch() {
	if c != nil && c.httpConn != nil {
		c.httpConn.Touch()
	}
}

// LookupSession looks up a session bound to the underlying HTTP connection.
func (c *ProtocolConnection) LookupSession(sessionID string) (*Session, bool) {
	if c == nil || c.httpConn == nil {
		return nil, false
	}
	return c.httpConn.LookupSession(sessionID)
}

// EnsureSession registers a session on the underlying HTTP connection.
func (c *ProtocolConnection) EnsureSession(sessionID string) {
	if c == nil || c.httpConn == nil {
		return
	}
	c.httpConn.EnsureSession(sessionID)
}

// ProtocolVersion returns the negotiated protocol version for this connection.
func (c *ProtocolConnection) ProtocolVersion() string {
	if c == nil || c.httpConn == nil {
		return ""
	}
	return c.httpConn.getProtocolVersion()
}

// CloseConnection closes the protocol connection and releases its resources.
func (c *ProtocolConnection) CloseConnection() {
	if c == nil {
		return
	}
	if c.closeFn != nil {
		if err := c.closeFn(); err != nil {
			acplog.Warn("httpserver: close connection %s: %v", c.connectionID, err)
		}
		return
	}
	if c.httpConn != nil {
		CloseConnection(c.httpConn)
	}
}

// ProtocolServer.

// ProtocolServer carries the connection-lifecycle strategy used by the shared
// Streamable HTTP POST/GET/DELETE handlers. Consumers (server.ACPServer) fill
// in closures instead of implementing an interface; this keeps their public
// API from growing method surface that users never call.
type ProtocolServer struct {
	CreateConnection  func(ctx context.Context) (*ProtocolConnection, int, error)
	LookupConnection  func(connectionID string) (*ProtocolConnection, bool)
	DeleteConnection  func(connectionID string) bool
	RequestTimeout    time.Duration
	KeepAliveInterval time.Duration
	// MaxMessageSize caps the size (in bytes) of a single Streamable HTTP POST
	// body. Zero means transport.DefaultMaxMessageSize.
	MaxMessageSize int
}

// HandleProtocolPost runs the Streamable HTTP POST logic using direct agent dispatch.
func HandleProtocolPost(ctx HandlerContext, server ProtocolServer) {
	if errMsg, code := ValidatePostHeaders(
		ctx.RequestHeader("Content-Type"),
		ctx.RequestHeader("Accept")); errMsg != "" {
		ctx.WriteError(code, errMsg)
		return
	}

	maxSize := server.MaxMessageSize
	if maxSize <= 0 {
		maxSize = acptransport.DefaultMaxMessageSize
	}
	if errMsg, code := validatePostContentLength(ctx.RequestHeader("Content-Length"), maxSize); errMsg != "" {
		ctx.WriteError(code, errMsg)
		return
	}

	body, err := ctx.RequestBody()
	if err != nil {
		ctx.WriteError(http.StatusBadRequest, "failed to read body")
		return
	}
	if len(body) > maxSize {
		ctx.WriteError(http.StatusRequestEntityTooLarge, "request body exceeds maximum size")
		return
	}

	msg, errMsg, code := ParsePostBody(baseHandlerContext(ctx), body)
	if errMsg != "" {
		ctx.WriteError(code, errMsg)
		return
	}

	acplog.Access(baseHandlerContext(ctx), "http-server", acplog.AccessDirectionRecv, msg.Body)

	connectionID := ctx.RequestHeader(acptransport.HeaderConnectionID)
	sessionID := ctx.RequestHeader(acptransport.HeaderSessionID)

	// Initialize: no connection ID yet.
	if connectionID == "" {
		if !msg.IsRequest || msg.Method != acp.MethodAgentInitialize {
			ctx.WriteError(http.StatusBadRequest, "first request must be initialize")
			return
		}
		handleInitializePost(ctx, server, msg)
		return
	}

	// Existing connection.
	conn, ok := server.LookupConnection(connectionID)
	if !ok || !conn.IsValid() {
		ctx.WriteError(http.StatusNotFound, "unknown connection")
		return
	}
	conn.Touch()

	if errMsg, code := ValidateSessionScopeMethod(msg.Method, sessionID); errMsg != "" {
		ctx.WriteError(code, errMsg)
		return
	}

	handleDirectPost(ctx, server, conn, msg, sessionID)
}

// HandleProtocolGet runs the shared Streamable HTTP GET logic.
// It establishes a long-lived SSE stream for the server to push messages to the client.
func HandleProtocolGet(ctx HandlerContext, server ProtocolServer) {
	connectionID := ctx.RequestHeader(acptransport.HeaderConnectionID)
	sessionID := ctx.RequestHeader(acptransport.HeaderSessionID)

	if connectionID == "" || sessionID == "" {
		ctx.WriteError(http.StatusBadRequest, "Acp-Connection-Id and Acp-Session-Id headers required")
		return
	}

	conn, ok := server.LookupConnection(connectionID)
	if !ok || !conn.IsValid() {
		ctx.WriteError(http.StatusNotFound, "unknown connection")
		return
	}

	sess, ok := conn.LookupSession(sessionID)
	if !ok {
		ctx.WriteError(http.StatusNotFound, "unknown session")
		return
	}
	conn.Touch()

	// Write SSE response headers before binding the stream so that any
	// backlog flushed by BindStream is sent after the correct headers.
	ctx.SetResponseHeader("Content-Type", "text/event-stream")
	ctx.SetResponseHeader("Cache-Control", "no-cache")
	ctx.SetResponseHeader("Connection", "keep-alive")
	if pv := conn.ProtocolVersion(); pv != "" {
		ctx.SetResponseHeader(acptransport.HeaderProtocolVersion, pv)
	}
	ctx.SetStatusCode(http.StatusOK)
	ctx.Flush()
	if err := ctx.WriteSSEKeepAlive(); err != nil {
		return
	}
	ctx.Flush()
	defer ctx.CloseSSE()

	// Build a thread-safe write function that writes directly to the SSE stream.
	var mu sync.Mutex
	writeFn := func(msg json.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.WriteSSEEvent(msg); err != nil {
			return err
		}
		ctx.Flush()
		conn.Touch()
		return nil
	}

	// Bind the write function to the session. Only one GET stream per
	// session: a later BindStream closes the evict channel so this handler
	// exits instead of continuing to emit keepalives on a stream it no
	// longer owns.
	streamGen, evicted := sess.BindStream(writeFn)
	defer sess.UnbindStream(streamGen)

	// Bracket the listener with Begin/End so the idle reaper cannot evict a
	// connection whose only current activity is a long-lived GET SSE stream.
	// Without this the reaper would tear down healthy listeners whenever
	// WithConnectionIdleTimeout is configured below the SSE keepalive
	// interval, since keepalive Touches only refresh activity every
	// SSEKeepaliveInterval.
	conn.httpConn.BeginRequest()
	defer conn.httpConn.EndRequest()

	// Keep-alive ticker.
	keepAlive := server.KeepAliveInterval
	if keepAlive <= 0 {
		keepAlive = SSEKeepaliveInterval
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()

	// Block until the stream should close.
	for {
		select {
		case <-ticker.C:
			mu.Lock()
			err := ctx.WriteSSEKeepAlive()
			ctx.Flush()
			mu.Unlock()
			if err != nil {
				return
			}
			conn.Touch()
		case <-evicted:
			return
		case <-sess.Done():
			return
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		}
	}
}

// HandleProtocolDelete runs the shared Streamable HTTP DELETE logic.
func HandleProtocolDelete(ctx HandlerContext, server ProtocolServer) {
	connectionID := ctx.RequestHeader(acptransport.HeaderConnectionID)
	if connectionID == "" {
		ctx.WriteError(http.StatusBadRequest, "Acp-Connection-Id header required")
		return
	}

	if !server.DeleteConnection(connectionID) {
		ctx.WriteError(http.StatusNotFound, "unknown connection")
		return
	}

	ctx.SetStatusCode(http.StatusAccepted)
}

func handleInitializePost(ctx HandlerContext, server ProtocolServer, msg *ParsedPost) {
	conn, status, err := server.CreateConnection(baseHandlerContext(ctx))
	if err != nil {
		if status == 0 {
			status = http.StatusInternalServerError
		}
		acplog.Warn("create protocol connection failed: status=%d err=%v", status, err)
		ctx.WriteError(status, connectionCreateErrorMessage(status, err))
		return
	}

	outcome := handleDirectInitializePost(ctx, server, conn, msg)
	if outcome.rpcErr != nil || outcome.writeErr != nil {
		// Evict from the connection table so a client that received the
		// Acp-Connection-Id header on the failed initialize cannot reuse a
		// closed connection on subsequent requests. DeleteConnection already
		// calls Close; only fall back to CloseConnection when the server did
		// not wire a delete hook or the connection wasn't in the table.
		if server.DeleteConnection == nil || !server.DeleteConnection(conn.ConnectionID()) {
			conn.CloseConnection()
		}
	}
}

func connectionCreateErrorMessage(status int, err error) string {
	if err == nil {
		return "failed to create connection"
	}
	if status >= 500 {
		return "failed to create connection"
	}
	return err.Error()
}
