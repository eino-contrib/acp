package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hertz-contrib/websocket"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/stream"
)

const (
	// controlWriteDeadline is the deadline for all control frame writes
	// (Pong, Close). Set to 5s to tolerate contention with concurrent data
	// frame writes that share the same internal write lock.
	controlWriteDeadline = 5 * time.Second

	// CloseCodeFirstFrameTimeout is a custom close code sent when the client
	// fails to send the first data frame within the configured timeout.
	CloseCodeFirstFrameTimeout = 4001
)

// proxyConn owns one active ACP WS ↔ Streamer bridge. It is created after a
// successful upgrade + NewStreamer and torn down when either side fails.
type proxyConn struct {
	id       string
	ws       *websocket.Conn
	streamer stream.Streamer

	// wsWriteMu serialises data-frame writes on the underlying WebSocket
	// because gorilla/hertz websocket.Conn is not safe for concurrent
	// WriteMessage calls. Control frames (Close, Pong) bypass this mutex via
	// WriteControl, but still share the websocket library's internal write lock
	// with data-frame writes. Always use WriteControl with a short deadline to
	// avoid being blocked by long data-frame writes.
	wsWriteMu      *sync.Mutex
	wsWriteTimeout time.Duration

	// Heartbeat configuration
	readTimeout       time.Duration
	firstFrameTimeout time.Duration

	// firstFrameReceived is set to true once the first data frame arrives.
	// Before this, PingHandler only echoes Pong without refreshing deadline.
	firstFrameReceived atomic.Bool

	// maxMessageSize caps a single south-bound payload read from the Streamer
	// before it is relayed to the north-bound WS. North-bound reads are
	// capped by SetReadLimit on the underlying *websocket.Conn; this field
	// is only consulted on the down path. <= 0 disables the cap.
	maxMessageSize int

	closeOnce     sync.Once
	closeReasonMu sync.Mutex
	closeReasonS  string
}

// installHeartbeat wires the PingHandler and initial read deadline. The proxy
// no longer sends Ping frames — heartbeat is driven by the Client SDK.
func (pc *proxyConn) installHeartbeat() {
	// Install PingHandler that echoes Pong. Before the first data frame,
	// only echo Pong without refreshing the read deadline. After first frame,
	// also refresh read deadline.
	pc.ws.SetPingHandler(func(appData string) error {
		if err := pc.ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(controlWriteDeadline)); err != nil {
			acplog.Warn("role=proxy conn_id=%s reason=pong_write_failed err=%v", pc.id, err)
			return fmt.Errorf("%w: %v", errPongWriteFailed, err)
		}
		// Only refresh read deadline after the first data frame
		if pc.firstFrameReceived.Load() && pc.readTimeout > 0 {
			_ = pc.ws.SetReadDeadline(time.Now().Add(pc.readTimeout))
		}
		return nil
	})

	// Set initial deadline: only firstFrameTimeout controls the first-frame window.
	// When firstFrameTimeout is 0 (disabled), no initial deadline is set —
	// readTimeout only takes effect after the first data frame arrives.
	if pc.firstFrameTimeout > 0 {
		_ = pc.ws.SetReadDeadline(time.Now().Add(pc.firstFrameTimeout))
	}
}

// run drives the pumps and returns after the two terminal pumps have exited.
// It always closes the connection before returning.
func (pc *proxyConn) run(ctx context.Context) {
	errCh := make(chan error, 2)

	// Each pump MUST deliver exactly one terminal value to errCh, including
	// when it panics — otherwise the second `<-errCh` below hangs forever,
	// leaking the connection goroutine and its concurrency slot. safe.Go's
	// default recover only logs; use GoRecover so panics are converted into
	// synthetic errors on errCh.
	runPump := func(name string, fn func(context.Context) error) {
		safe.GoRecover(
			func() { errCh <- fn(ctx) },
			func(r any) { errCh <- fmt.Errorf("%s panic: %v", name, r) },
		)
	}
	runPump("upPump", pc.upPump)
	runPump("downPump", pc.downPump)

	first := <-errCh
	code, reason := classifyPumpErr(first)

	// Log WARN for timeout events per observability spec.
	if errors.Is(first, errFirstFrameTimeout) {
		acplog.Warn("role=proxy conn_id=%s reason=first_frame_timeout timeout=%v", pc.id, pc.firstFrameTimeout)
	} else if errors.Is(first, errReadTimeout) {
		acplog.Warn("role=proxy conn_id=%s reason=read_timeout timeout=%v", pc.id, pc.readTimeout)
	}

	pc.close(code, reason)

	// Drain the second pump. It will see either the closed WS (upPump) or
	// the closed Streamer (downPump) and return promptly.
	<-errCh
}

// errFirstFrameTimeout is a sentinel that classifyPumpErr maps to 4001.
var errFirstFrameTimeout = errors.New("first frame timeout")

// errReadTimeout is a sentinel that classifyPumpErr maps to 1001.
var errReadTimeout = errors.New("read timeout")

// errPongWriteFailed is a sentinel for PingHandler pong write failures.
// classifyPumpErr maps this to no close frame (connection is already broken).
var errPongWriteFailed = errors.New("pong write failed")

// upPump: north-bound WS → south-bound Streamer.
func (pc *proxyConn) upPump(ctx context.Context) error {
	for {
		msgType, data, err := pc.ws.ReadMessage()
		if err != nil {
			// Distinguish timeout errors for proper close code classification.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if !pc.firstFrameReceived.Load() {
					return fmt.Errorf("ws read: %w", errFirstFrameTimeout)
				}
				return fmt.Errorf("ws read: %w", errReadTimeout)
			}
			return fmt.Errorf("ws read: %w", err)
		}
		switch msgType {
		case websocket.TextMessage, websocket.BinaryMessage:
			// Ping / pong / close frames are handled by the websocket
			// library; we only see data frames here.
		default:
			continue
		}

		// On the first data frame, switch from first-frame deadline to normal
		// read deadline and mark the connection as having received first frame.
		if !pc.firstFrameReceived.Load() {
			pc.firstFrameReceived.Store(true)
			if pc.readTimeout > 0 {
				_ = pc.ws.SetReadDeadline(time.Now().Add(pc.readTimeout))
			} else {
				// Clear the first-frame deadline
				_ = pc.ws.SetReadDeadline(time.Time{})
			}
		} else {
			// Refresh read deadline on subsequent data frames.
			if pc.readTimeout > 0 {
				_ = pc.ws.SetReadDeadline(time.Now().Add(pc.readTimeout))
			}
		}

		acplog.Access(ctx, "proxy-up", acplog.AccessDirectionRecv, data)
		var writeCtx context.Context
		var writeCancel context.CancelFunc
		if pc.wsWriteTimeout > 0 {
			writeCtx, writeCancel = context.WithTimeout(ctx, pc.wsWriteTimeout)
		} else {
			writeCtx, writeCancel = ctx, func() {}
		}
		err = pc.streamer.WritePayload(writeCtx, data)
		writeCancel()
		if err != nil {
			return fmt.Errorf("streamer write: %w", err)
		}
	}
}

// errPayloadTooLarge is returned by downPump when the Streamer yields a
// payload larger than the configured ceiling. Exported via classifyPumpErr as
// a 1009 (MessageTooBig) close code so the client sees a clear signal instead
// of a generic server fault.
var errPayloadTooLarge = errors.New("payload exceeds max message size")

// downPump: south-bound Streamer → north-bound WS.
func (pc *proxyConn) downPump(ctx context.Context) error {
	for {
		payload, err := pc.streamer.ReadPayload(ctx)
		if err != nil {
			return fmt.Errorf("streamer read: %w", err)
		}
		if pc.maxMessageSize > 0 && len(payload) > pc.maxMessageSize {
			return fmt.Errorf("streamer payload %d bytes: %w", len(payload), errPayloadTooLarge)
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

// close tears down both the streamer and the ws. Idempotent. code is the
// WebSocket close code delivered to the peer; reason is the corresponding
// human-readable text (safely truncated before being put on the wire).
func (pc *proxyConn) close(code int, reason string) {
	pc.closeOnce.Do(func() {
		pc.setCloseReason(reason)
		// Send close frame unless code == -1 (connection already broken,
		// e.g. pong write failure).
		if code >= 0 {
			_ = pc.ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(code, wsutil.SafeCloseReason(reason)),
				time.Now().Add(controlWriteDeadline))
		}

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

// classifyPumpErr maps a terminal pump error to the WebSocket close code and
// reason the proxy should return to the client.
func classifyPumpErr(err error) (int, string) {
	if err == nil {
		return websocket.CloseNormalClosure, ""
	}
	if errors.Is(err, errFirstFrameTimeout) {
		return CloseCodeFirstFrameTimeout, "first frame timeout"
	}
	if errors.Is(err, errReadTimeout) {
		return websocket.CloseGoingAway, "read timeout"
	}
	if errors.Is(err, errPongWriteFailed) {
		// Connection is already broken; skip close frame entirely.
		return -1, "pong_write_failed"
	}
	if errors.Is(err, websocket.ErrReadLimit) || errors.Is(err, errPayloadTooLarge) {
		return websocket.CloseMessageTooBig, "message too big"
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived:
			return websocket.CloseNormalClosure, fmt.Sprintf("client closed (code=%d)", ce.Code)
		}
		return websocket.CloseInternalServerErr, err.Error()
	}
	if errors.Is(err, io.EOF) {
		return websocket.CloseNormalClosure, "upstream eof"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return websocket.CloseGoingAway, "canceled"
	}
	return websocket.CloseInternalServerErr, err.Error()
}

// isBenignCloseErr filters noise from the WS close path.
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
