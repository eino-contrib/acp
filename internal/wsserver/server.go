package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

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
// callers with ServeConn's setup/teardown.
type Transport struct {
	inbox chan json.RawMessage
	done  chan struct{}
	once  sync.Once

	activeConn atomic.Pointer[activeServerConnection]

	// Logger, if non-nil, receives debug/error messages.
	Logger log.Logger
}

const defaultMaxReadMessageSize int64 = int64(transport.DefaultMaxMessageSize)

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
func (t *Transport) ServeConn(ctx context.Context, ws messageConn) {
	logger := t.logger()
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
				logger.CtxError(serveCtx, "close websocket server connection: %v", err)
			}
		})
	}

	// Writer goroutine: reads from outbox and sends as WS text frames.
	writerDone := make(chan struct{})
	safe.GoWithLogger(logger, func() {
		defer close(writerDone)
		for {
			select {
			case msg, ok := <-connState.outbox:
				if !ok {
					return
				}
				if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
					logger.CtxError(serveCtx, "write websocket message: %v", err)
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
	safe.GoWithLogger(logger, func() {
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
				logger.CtxError(serveCtx, "read websocket message: %v", err)
			}
			return
		}
		if messageType != websocket.TextMessage {
			continue // ignore binary frames per spec
		}

		if !validatedFirstMessage {
			if err := validateInitialWebSocketMessage(data); err != nil {
				logger.CtxError(serveCtx, "reject websocket connection: %v", err)
				if writeErr := ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error())); writeErr != nil {
					logger.CtxError(serveCtx, "write websocket close frame: %v", writeErr)
				}
				return
			}
			validatedFirstMessage = true
		}

		select {
		case t.inbox <- transport.CloneMessage(data):
		case <-serveCtx.Done():
			return
		case <-t.done:
			return
		}
	}
}

func (t *Transport) logger() log.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return log.Default()
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

	select {
	case conn.outbox <- msg:
		return nil
	case <-conn.done:
		return transport.ErrTransportClosed
	case <-ctx.Done():
		return ctx.Err()
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
