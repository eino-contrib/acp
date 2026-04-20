package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hertz-contrib/websocket"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/stream"
)

// proxyConn owns one active ACP WS ↔ Streamer bridge. It is created after a
// successful upgrade + NewStreamer and torn down when either side fails.
type proxyConn struct {
	id       string
	ws       *websocket.Conn
	streamer stream.Streamer

	// wsWriteMu serialises writes on the underlying WebSocket because
	// gorilla/hertz websocket.Conn is not safe for concurrent WriteMessage
	// calls. Both the down-pump (sending payloads to the client) and the
	// close path (sending a CloseMessage) go through this mutex.
	wsWriteMu      *sync.Mutex
	wsWriteTimeout time.Duration

	closeOnce   sync.Once
	closeReasonMu sync.Mutex
	closeReasonS  string
}

// run drives the two pumps and returns after both have exited. It always
// closes the connection before returning.
func (pc *proxyConn) run(ctx context.Context) {
	errCh := make(chan error, 2)

	safe.Go(func() { errCh <- pc.upPump(ctx) })
	safe.Go(func() { errCh <- pc.downPump(ctx) })

	first := <-errCh
	pc.close(errReason(first))

	// Drain the second pump. It will see either the closed WS (upPump) or
	// the closed Streamer (downPump) and return promptly.
	<-errCh
}

// upPump: north-bound WS → south-bound Streamer.
func (pc *proxyConn) upPump(ctx context.Context) error {
	for {
		msgType, data, err := pc.ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		switch msgType {
		case websocket.TextMessage, websocket.BinaryMessage:
			// Ping / pong / close frames are handled by the websocket
			// library; we only see data frames here.
		default:
			continue
		}
		acplog.Access(ctx, "proxy-up", acplog.AccessDirectionRecv, data)
		if err := pc.streamer.WritePayload(ctx, data); err != nil {
			return fmt.Errorf("streamer write: %w", err)
		}
	}
}

// downPump: south-bound Streamer → north-bound WS.
func (pc *proxyConn) downPump(ctx context.Context) error {
	for {
		payload, err := pc.streamer.ReadPayload(ctx)
		if err != nil {
			return fmt.Errorf("streamer read: %w", err)
		}
		acplog.Access(ctx, "proxy-down", acplog.AccessDirectionSend, payload)
		if err := pc.writeWSMessage(websocket.TextMessage, payload); err != nil {
			return fmt.Errorf("ws write: %w", err)
		}
	}
}

// writeWSMessage serialises WS writes under wsWriteMu and applies the
// per-message write deadline when configured. Must be used for every
// WriteMessage call, not just the pump.
func (pc *proxyConn) writeWSMessage(msgType int, data []byte) error {
	pc.wsWriteMu.Lock()
	defer pc.wsWriteMu.Unlock()

	if pc.wsWriteTimeout > 0 {
		if err := pc.ws.SetWriteDeadline(time.Now().Add(pc.wsWriteTimeout)); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}
	return pc.ws.WriteMessage(msgType, data)
}

// close tears down both the streamer and the ws. Idempotent.
func (pc *proxyConn) close(reason string) {
	pc.closeOnce.Do(func() {
		pc.setCloseReason(reason)
		// Best-effort close frame to help the Client SDK report the cause.
		safeReason := reason
		if len(safeReason) > 120 {
			safeReason = safeReason[:120] + "..."
		}
		_ = pc.writeWSMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, safeReason))

		if err := pc.streamer.Close(reason); err != nil {
			acplog.Warn("proxy[%s]: streamer close returned: %v", pc.id, err)
		}
		if err := pc.ws.Close(); err != nil && !isBenignCloseErr(err) {
			acplog.Debug("proxy[%s]: ws close returned: %v", pc.id, err)
		}
	})
}

func (pc *proxyConn) setCloseReason(reason string) {
	pc.closeReasonMu.Lock()
	if pc.closeReasonS == "" {
		pc.closeReasonS = reason
	}
	pc.closeReasonMu.Unlock()
}

func (pc *proxyConn) closeReason() string {
	pc.closeReasonMu.Lock()
	defer pc.closeReasonMu.Unlock()
	return pc.closeReasonS
}

func errReason(err error) string {
	if err == nil {
		return "unknown"
	}
	// Recognise the standard graceful-close WebSocket codes so logs don't
	// read like an error when the client simply disconnected normally.
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway:
			return fmt.Sprintf("client closed normally (code=%d)", ce.Code)
		}
	}
	return err.Error()
}

// isBenignCloseErr filters noise from the WS close path: after the other
// side has already closed, Close can return "use of closed network
// connection" or io.EOF variants. These are expected during teardown and
// should not pollute the info-level log.
func isBenignCloseErr(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	msg := err.Error()
	return msg == "use of closed network connection" ||
		msg == "tls: use of closed connection"
}
