package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
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

type messageConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	Close() error
}

type readLimitSetter interface {
	SetReadLimit(int64)
}

// writeDeadliner is satisfied by *websocket.Conn and is used by the writer
// goroutine to bound the time spent on a single socket write.
type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

var _ transport.Transport = (*Transport)(nil)

// New creates a new WebSocket server transport.
func New() *Transport {
	return &Transport{
		inbox: make(chan json.RawMessage, transport.DefaultInboxSize),
		done:  make(chan struct{}),
	}
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

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	connState := t.activateConnection()

	if limiter, ok := ws.(readLimitSetter); ok {
		limiter.SetReadLimit(defaultMaxReadMessageSize)
	}

	// closeWS safely closes the WebSocket exactly once, preventing double-close
	// across the multiple goroutines that may trigger shutdown.
	var closeOnce sync.Once
	closeWS := func() {
		closeOnce.Do(func() {
			if err := ws.Close(); err != nil {
				log.CtxDebug(serveCtx, "close websocket server connection: %v", err)
			}
		})
	}

	// Writer goroutine: reads from outbox and sends as WS text frames.
	writerDone := make(chan struct{})
	deadliner, hasDeadliner := ws.(writeDeadliner)
	safe.Go(func() {
		defer close(writerDone)
		for {
			select {
			case msg, ok := <-connState.outbox:
				if !ok {
					return
				}
				log.Access(serveCtx, "ws-server", log.AccessDirectionSend, msg)
				if hasDeadliner {
					if err := deadliner.SetWriteDeadline(time.Now().Add(defaultSocketWriteTimeout)); err != nil {
						log.CtxDebug(serveCtx, "set websocket write deadline: %v", err)
					}
				}
				if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
					log.CtxError(serveCtx, "write websocket message: %v", err)
					return
				}
			case <-connState.done:
				return
			case <-serveCtx.Done():
				return
			case <-t.done:
				return
			}
		}
	})

	// Closer goroutine: ensures the WebSocket is closed when the context is
	// cancelled or the transport is closed, unblocking the reader loop.
	safe.Go(func() {
		select {
		case <-serveCtx.Done():
			closeWS()
		case <-t.done:
			closeWS()
		case <-writerDone:
			// Writer exited on its own (write error or outbox closed).
			// Close the WebSocket so the reader's ReadMessage unblocks.
			closeWS()
		}
	})

	// Reader loop: reads WS text frames and forwards to the transport inbox.
	defer func() {
		t.deactivateConnection(connState)
		closeWS()
		<-writerDone
	}()

	validatedFirstMessage := false
	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) &&
				!websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.CtxDebug(serveCtx, "read websocket message: %v", err)
			}
			return
		}
		if messageType != websocket.TextMessage {
			continue // ignore binary frames per spec
		}

		if !validatedFirstMessage {
			if err := validateInitialWebSocketMessage(data); err != nil {
				log.CtxWarn(serveCtx, "reject websocket connection: %v", err)
				if writeErr := ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error())); writeErr != nil {
					log.CtxDebug(serveCtx, "write websocket close frame: %v", writeErr)
				}
				return
			}
			validatedFirstMessage = true
		}

		log.Access(serveCtx, "ws-server", log.AccessDirectionRecv, data)

		select {
		case t.inbox <- transport.CloneMessage(data):
		case <-serveCtx.Done():
			return
		case <-t.done:
			return
		}
	}
}

func validateInitialWebSocketMessage(data []byte) error {
	var msg struct {
		Method string           `json:"method,omitempty"`
		ID     *json.RawMessage `json:"id,omitempty"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("first websocket message must be valid JSON-RPC initialize request: %w", err)
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
