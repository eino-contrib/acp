package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/transport"
)

// Transport implements the Transport interface for the server side of an ACP
// WebSocket connection.
//
// It upgrades a single HTTP GET request to a WebSocket and uses text frames
// for bidirectional JSON-RPC messaging. The client must send "initialize" as
// the first message.
//
// Each Transport is created fresh per WebSocket upgrade (see
// server.newWSConn), so at most one ServeConn call is ever in flight. The
// activeConn pointer is kept under an atomic to synchronize WriteMessage
// callers with ServeConn's setup/teardown. A second ServeConn call on the
// same Transport is rejected via the serving flag — catching future misuse
// rather than silently corrupting state.
type Transport struct {
	inbox chan json.RawMessage
	done  chan struct{}
	once  sync.Once

	serving    atomic.Bool
	activeConn atomic.Pointer[activeServerConnection]

	// Heartbeat configuration
	readTimeout       time.Duration
	initializeTimeout time.Duration
}

const defaultMaxReadMessageSize int64 = int64(transport.DefaultMaxMessageSize)

// defaultOutboxSendTimeout caps the time a WriteMessage call will wait for the
// outbox to accept a message when the caller provided no deadline. Without
// this cap a slow peer (or a peer that stopped reading) would back-pressure
// into handler goroutines — because jsonrpc.Connection.respond writes using
// the connection-level context, which has no deadline — and eventually exhaust
// the worker pool.
const defaultOutboxSendTimeout = 10 * time.Second

// defaultSocketWriteTimeout caps the time a single ws.WriteMessage call may
// spend pushing bytes to the socket. A hung TCP write is the other half of
// the slow-peer problem and this deadline guarantees the writer goroutine
// cannot be stuck forever on a single frame.
const defaultSocketWriteTimeout = 30 * time.Second

type activeServerConnection struct {
	outbox chan json.RawMessage
	done   chan struct{} // closed when this connection is deactivated
	once   sync.Once
}

// messageConn is the full set of *websocket.Conn methods the server transport
// depends on. Keeping it as one interface (rather than a base interface plus a
// handful of optional ones probed via type assertion at runtime) lets the
// production *websocket.Conn and test fakes satisfy a single explicit contract,
// and removes per-frame assertions from the hot read path.
type messageConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	Close() error
	SetReadLimit(int64)
	SetWriteDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetPingHandler(h func(appData string) error)
	WriteControl(messageType int, data []byte, deadline time.Time) error
}

var _ transport.Transport = (*Transport)(nil)

// New creates a new WebSocket server transport.
func New(opts ...Option) *Transport {
	t := &Transport{
		inbox: make(chan json.RawMessage, transport.DefaultInboxSize),
		done:  make(chan struct{}),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// ServeConn serves a WebSocket connection that has already been upgraded.
// This is useful for multi-connection scenarios (e.g. server.ACPServer)
// where the caller manages connection IDs and upgrades externally.
//
// It blocks until the connection closes or the context is cancelled.
// The caller is responsible for closing the WebSocket connection afterward.
//
// A Transport serves at most one connection in its lifetime: repeated
// ServeConn calls (concurrent or sequential) log an error and return
// immediately. Callers must construct a fresh Transport per upgrade.
func (t *Transport) ServeConn(ctx context.Context, ws messageConn) {
	if !t.serving.CompareAndSwap(false, true) {
		log.CtxError(ctx, "ws-server: ServeConn called more than once on the same Transport; construct a fresh Transport per upgrade")
		return
	}

	connID := connspi.ConnectionIDFromContext(ctx)
	if connID == "" {
		connID = uuid.NewString()
	}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &serveSession{
		t:          t,
		ws:         ws,
		connID:     connID,
		ctx:        serveCtx,
		connState:  t.activateConnection(),
		writerDone: make(chan struct{}),
	}

	ws.SetReadLimit(defaultMaxReadMessageSize)
	s.installHeartbeat()
	s.startWriter()
	s.startCloseWatcher()

	// teardown runs before cancel() (defers are LIFO): deactivate the
	// connection, close the socket, then wait for the writer to drain.
	defer s.teardown()
	s.runReadLoop()
}

// serveSession owns the per-connection state for a single ServeConn call. It
// is created fresh per upgrade, never copied, and torn down via teardown.
type serveSession struct {
	t      *Transport
	ws     messageConn
	connID string
	ctx    context.Context

	connState  *activeServerConnection
	writerDone chan struct{}

	// initialized flips to true once the first message passes initialize
	// validation; the Ping responder only refreshes the read deadline after
	// this point.
	initialized atomic.Bool
	// closeSent tracks whether a specific close frame was already written
	// (e.g. timeout, policy violation, or pong write failure). closeWS checks
	// it to avoid sending a redundant 1000 NormalClosure over a broken or
	// already-signalled connection.
	closeSent atomic.Bool
	closeOnce sync.Once
}

// installHeartbeat wires the Ping responder and the initialize read deadline.
// Before initialize completes the responder only echoes Pong (does NOT refresh
// the read deadline); after initialize it refreshes the deadline too.
func (s *serveSession) installHeartbeat() {
	s.ws.SetPingHandler(wsutil.PongResponder{
		WriteControl:    s.ws.WriteControl,
		SetReadDeadline: s.ws.SetReadDeadline,
		ReadTimeout:     s.t.readTimeout,
		RefreshDeadline: s.initialized.Load,
		OnContention: func(err error) {
			log.CtxWarn(s.ctx, "role=server conn_id=%s reason=pong_write_contention err=%v", s.connID, err)
		},
		OnWriteFailed: func(err error) {
			log.CtxWarn(s.ctx, "role=server conn_id=%s reason=pong_write_failed err=%v", s.connID, err)
			// Mark closeSent so closeWS does not send a 1000 NormalClosure
			// frame on top of a broken connection.
			s.closeSent.Store(true)
		},
	}.Handler())

	if s.t.initializeTimeout > 0 {
		_ = s.ws.SetReadDeadline(time.Now().Add(s.t.initializeTimeout))
	}
}

// startWriter launches the writer goroutine that drains the connection outbox
// onto the socket as WS text frames.
func (s *serveSession) startWriter() {
	safe.Go(func() {
		// Close connState.done on every writer exit path. Once the writer is
		// gone the outbox has no consumer, so WriteMessage's post-send re-check
		// must observe a closed done and report ErrTransportClosed instead of
		// nil. Relying solely on the reader teardown (deactivateConnection) to
		// close it leaves a window where a message enqueued during shutdown is
		// reported as sent but never flushed.
		defer s.connState.once.Do(func() { close(s.connState.done) })
		defer close(s.writerDone)
		for {
			select {
			case msg, ok := <-s.connState.outbox:
				if !ok {
					return
				}
				log.Access(s.ctx, "ws-server", log.AccessDirectionSend, msg)
				if err := s.ws.SetWriteDeadline(time.Now().Add(defaultSocketWriteTimeout)); err != nil {
					log.CtxDebug(s.ctx, "set websocket write deadline: %v", err)
				}
				if err := s.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
					log.CtxError(s.ctx, "write websocket message: %v", err)
					return
				}
			case <-s.connState.done:
				return
			case <-s.ctx.Done():
				return
			case <-s.t.done:
				return
			}
		}
	})
}

// startCloseWatcher closes the WebSocket once the context is cancelled, the
// transport is closed, or the writer exits — unblocking the reader's
// ReadMessage so ServeConn can return.
func (s *serveSession) startCloseWatcher() {
	safe.Go(func() {
		select {
		case <-s.ctx.Done():
		case <-s.t.done:
		case <-s.writerDone:
		}
		s.closeWS()
	})
}

// runReadLoop reads WS text frames and forwards them to the transport inbox.
// It returns when the connection fails, is closed, or the transport shuts down.
func (s *serveSession) runReadLoop() {
	validatedFirstMessage := false
	for {
		messageType, data, err := s.ws.ReadMessage()
		if err != nil {
			s.handleReadError(err, validatedFirstMessage)
			return
		}
		if messageType != websocket.TextMessage {
			continue // ignore binary frames per spec
		}

		if !validatedFirstMessage {
			if err := validateInitialWebSocketMessage(data); err != nil {
				log.CtxWarn(s.ctx, "reject websocket connection: %v", err)
				// Use a fixed wire reason rather than echoing the peer's
				// (untrusted) input back on the close frame; the detailed
				// validation error is logged above for diagnosis.
				s.sendCloseFrame(websocket.ClosePolicyViolation, "invalid initialize request")
				return
			}
			validatedFirstMessage = true
			s.initialized.Store(true)
			s.applyReadDeadlineAfterInit()
		} else if s.t.readTimeout > 0 {
			// Refresh read deadline on every data frame.
			_ = s.ws.SetReadDeadline(time.Now().Add(s.t.readTimeout))
		}

		log.Access(s.ctx, "ws-server", log.AccessDirectionRecv, data)

		select {
		case s.t.inbox <- transport.CloneMessage(data):
		case <-s.ctx.Done():
			return
		case <-s.t.done:
			return
		}
	}
}

// handleReadError classifies a terminal ReadMessage error and, for timeouts,
// emits the appropriate close frame. Benign EOF / normal-close errors are
// silent; unexpected errors are logged at debug.
func (s *serveSession) handleReadError(err error, validatedFirstMessage bool) {
	if errors.Is(err, io.EOF) ||
		websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return
	}
	var ne net.Error
	isTimeout := errors.As(err, &ne) && ne.Timeout()
	switch {
	case isTimeout && !validatedFirstMessage:
		log.CtxWarn(s.ctx, "role=server conn_id=%s reason=initialize_timeout timeout=%v err=%v", s.connID, s.t.initializeTimeout, err)
		s.sendCloseFrame(transport.WSCloseInitializeTimeout, "initialize timeout")
	case isTimeout && validatedFirstMessage:
		log.CtxWarn(s.ctx, "role=server conn_id=%s reason=read_timeout timeout=%v err=%v", s.connID, s.t.readTimeout, err)
		s.sendCloseFrame(websocket.CloseGoingAway, "read timeout")
	default:
		log.CtxDebug(s.ctx, "read websocket message: %v", err)
	}
}

// applyReadDeadlineAfterInit switches from the initialize deadline to the
// steady-state read deadline (or clears it when readTimeout is disabled).
func (s *serveSession) applyReadDeadlineAfterInit() {
	if s.t.readTimeout > 0 {
		_ = s.ws.SetReadDeadline(time.Now().Add(s.t.readTimeout))
	} else {
		_ = s.ws.SetReadDeadline(time.Time{})
	}
}

// sendCloseFrame writes a specific close frame to the peer and marks closeSent
// so the final closeWS does not overwrite it with a 1000 NormalClosure. It is
// the single path for every non-normal server-initiated close.
func (s *serveSession) sendCloseFrame(code int, reason string) {
	s.closeSent.Store(true)
	_ = s.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(wsutil.ControlWriteDeadline))
}

// closeWS closes the WebSocket exactly once, preventing double-close across the
// goroutines that may trigger shutdown. When no specific close frame was sent
// yet it first emits a 1000 NormalClosure.
func (s *serveSession) closeWS() {
	s.closeOnce.Do(func() {
		if !s.closeSent.Load() {
			_ = s.ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(wsutil.ControlWriteDeadline))
		}
		if err := s.ws.Close(); err != nil {
			log.CtxDebug(s.ctx, "close websocket server connection: %v", err)
		}
	})
}

// teardown deactivates the connection, closes the socket, and waits for the
// writer goroutine to exit. Safe to call exactly once at the end of ServeConn.
func (s *serveSession) teardown() {
	s.t.deactivateConnection(s.connState)
	s.closeWS()
	<-s.writerDone
}

func validateInitialWebSocketMessage(data []byte) error {
	var msg struct {
		JSONRPC string           `json:"jsonrpc"`
		Method  string           `json:"method,omitempty"`
		ID      *json.RawMessage `json:"id,omitempty"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("first websocket message must be valid JSON-RPC initialize request: %w", err)
	}
	if msg.JSONRPC != "2.0" {
		return fmt.Errorf("first websocket message must have jsonrpc=\"2.0\", got %q", msg.JSONRPC)
	}
	if msg.Method != transport.MethodInitialize || msg.ID == nil {
		return fmt.Errorf("first websocket message must be initialize request, got method=%q", msg.Method)
	}
	return nil
}

// ReadMessage reads the next JSON-RPC message received from the WebSocket.
// Implements Transport.
func (t *Transport) ReadMessage(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg, ok := <-t.inbox:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

// WriteMessage sends a JSON-RPC message over the WebSocket.
// Implements Transport.
//
// If the caller provided no ctx deadline, the outbox wait is capped at
// defaultOutboxSendTimeout so a slow or stalled peer cannot indefinitely
// block the caller (most notably jsonrpc.Connection.respond, which uses the
// connection-level context that has no deadline).
func (t *Transport) WriteMessage(ctx context.Context, data json.RawMessage) error {
	select {
	case <-t.done:
		return transport.ErrTransportClosed
	default:
	}

	conn := t.currentConnection()
	if conn == nil {
		return transport.ErrTransportClosed
	}
	msg := transport.CloneMessage(data)

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultOutboxSendTimeout)
		defer cancel()
	}

	select {
	case conn.outbox <- msg:
		// Re-check close signals to close the ambiguity window: Go's select
		// picks at random when multiple cases are ready, so a concurrently
		// closed conn.done / t.done could race with the send case. Without
		// this verification a message that lands in an orphaned outbox
		// (writer goroutine already exited) would be silently dropped while
		// the caller sees nil.
		select {
		case <-conn.done:
			return transport.ErrTransportClosed
		case <-t.done:
			return transport.ErrTransportClosed
		default:
			return nil
		}
	case <-conn.done:
		return transport.ErrTransportClosed
	case <-ctx.Done():
		return fmt.Errorf("ws-server: outbox send blocked: %w", ctx.Err())
	case <-t.done:
		return transport.ErrTransportClosed
	}
}

// Close closes the transport.
func (t *Transport) Close() error {
	t.once.Do(func() {
		close(t.done)
	})
	return nil
}

func (t *Transport) activateConnection() *activeServerConnection {
	conn := &activeServerConnection{
		outbox: make(chan json.RawMessage, transport.DefaultOutboxSize),
		done:   make(chan struct{}),
	}
	t.activeConn.Store(conn)
	return conn
}

func (t *Transport) deactivateConnection(conn *activeServerConnection) {
	if conn == nil {
		return
	}
	conn.once.Do(func() { close(conn.done) })
	t.activeConn.CompareAndSwap(conn, nil)
}

func (t *Transport) currentConnection() *activeServerConnection {
	return t.activeConn.Load()
}
