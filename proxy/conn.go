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

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/stream"
	acptransport "github.com/eino-contrib/acp/transport"
)

// proxyConn owns one active ACP WS ↔ Streamer bridge. It is created after a
// successful upgrade + NewStreamer and torn down when either side fails.
type proxyConn struct {
	id       string
	ws       WebSocketConn
	streamer stream.Streamer

	// wsWriteMu serialises data-frame writes on the underlying WebSocket
	// because gorilla/hertz websocket.Conn is not safe for concurrent
	// WriteMessage calls. Control frames (Close, Pong) bypass this mutex via
	// WriteControl, but still share the websocket library's internal write lock
	// with data-frame writes. Always use WriteControl with a short deadline to
	// avoid being blocked by long data-frame writes. A proxyConn is always
	// used by pointer and never copied, so a value mutex is safe.
	wsWriteMu      sync.Mutex
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
	closeReason   string
}

// installHeartbeat wires the PingHandler and initial read deadline. The proxy
// no longer sends Ping frames — heartbeat is driven by the Client SDK.
func (pc *proxyConn) installHeartbeat() {
	// Echo Pong for every inbound Ping. Before the first data frame, only echo
	// without refreshing the read deadline; after it, also refresh. The shared
	// PongResponder centralises the contention-swallow rule (see wsutil).
	pc.ws.SetPingHandler(wsutil.PongResponder{
		WriteControl:    pc.ws.WriteControl,
		SetReadDeadline: pc.ws.SetReadDeadline,
		ReadTimeout:     pc.readTimeout,
		RefreshDeadline: pc.firstFrameReceived.Load,
		OnContention: func(err error) {
			acplog.Warn("role=proxy conn_id=%s reason=pong_write_contention err=%v", pc.id, err)
		},
		OnWriteFailed: func(err error) {
			acplog.Warn("role=proxy conn_id=%s reason=pong_write_failed err=%v", pc.id, err)
		},
		WrapWriteFailed: func(err error) error {
			return fmt.Errorf("%w: %v", errPongWriteFailed, err)
		},
	}.Handler())

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
	action := classifyPumpErr(first)

	// Log WARN for timeout events per observability spec.
	if errors.Is(first, errFirstFrameTimeout) {
		acplog.Warn("role=proxy conn_id=%s reason=first_frame_timeout timeout=%v", pc.id, pc.firstFrameTimeout)
	} else if errors.Is(first, errReadTimeout) {
		acplog.Warn("role=proxy conn_id=%s reason=read_timeout timeout=%v", pc.id, pc.readTimeout)
	} else if action.Code == wsconn.CloseInternalServerErr {
		// The wire reason is deliberately generic ("internal error"); record
		// the full error here, correlated by conn_id, for diagnosis.
		acplog.Warn("role=proxy conn_id=%s reason=internal_error err=%v", pc.id, first)
	}

	pc.close(action)

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
		case wsconn.TextMessage, wsconn.BinaryMessage:
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
		if err := pc.writeWSMessage(wsconn.TextMessage, payload); err != nil {
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

// close tears down both the streamer and the ws. Idempotent. The supplied
// closeAction decides whether a close frame is written to the peer and which
// close code / reason it carries.
func (pc *proxyConn) close(action closeAction) {
	pc.closeOnce.Do(func() {
		pc.setCloseReason(action.Reason)
		// Skip the close frame when the connection is already broken (e.g. a
		// pong write failure) — writing a frame onto a dead socket is pointless.
		if action.SendFrame {
			_ = writeControlSafely(pc.ws, wsconn.CloseMessage,
				wsconn.FormatCloseMessage(action.Code, wsutil.SafeCloseReason(action.Reason)),
				time.Now().Add(wsutil.ControlWriteDeadline))
		}
		if err := closeWebSocketSafely(pc.ws); err != nil && !isBenignCloseErr(err) {
			acplog.Debug("proxy[%s]: ws close returned: %v", pc.id, err)
		}
		if err := closeStreamerSafely(pc.streamer, action.Reason); err != nil {
			acplog.Warn("proxy[%s]: streamer close returned: %v", pc.id, err)
		}
	})
}

func writeControlSafely(ws WebSocketConn, messageType int, payload []byte, deadline time.Time) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("websocket control write panic: %v", recovered)
		}
	}()
	return ws.WriteControl(messageType, payload, deadline)
}

func closeWebSocketSafely(ws WebSocketConn) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("websocket close panic: %v", recovered)
		}
	}()
	return ws.Close()
}

func closeStreamerSafely(streamer stream.Streamer, reason string) (err error) {
	if isNilStreamer(streamer) {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("streamer close panic: %v", recovered)
		}
	}()
	return streamer.Close(reason)
}

func (pc *proxyConn) setCloseReason(reason string) {
	pc.closeReasonMu.Lock()
	if pc.closeReason == "" {
		pc.closeReason = reason
	}
	pc.closeReasonMu.Unlock()
}

func (pc *proxyConn) getCloseReason() string {
	pc.closeReasonMu.Lock()
	defer pc.closeReasonMu.Unlock()
	return pc.closeReason
}

// closeAction describes how the proxy should close a connection: whether to
// emit a WebSocket close frame and, if so, which close code and reason to put
// on the wire. Keeping SendFrame separate from Code avoids overloading the
// (uint16) close-code space with an out-of-band sentinel.
type closeAction struct {
	// SendFrame reports whether a close frame should be written to the peer.
	// It is false when the connection is already broken (e.g. a pong write
	// failure) and a frame would never reach the peer anyway.
	SendFrame bool
	// Code is the WebSocket close code; only meaningful when SendFrame is true.
	Code int
	// Reason is the human-readable close reason (safely truncated before being
	// put on the wire). It is also recorded as the connection's close reason.
	Reason string
}

// classifyPumpErr maps a terminal pump error to the close action the proxy
// should take toward the client.
func classifyPumpErr(err error) closeAction {
	send := func(code int, reason string) closeAction {
		return closeAction{SendFrame: true, Code: code, Reason: reason}
	}
	if err == nil {
		return send(wsconn.CloseNormalClosure, "")
	}
	if errors.Is(err, errFirstFrameTimeout) {
		return send(acptransport.WSCloseFirstFrameTimeout, "first frame timeout")
	}
	if errors.Is(err, errReadTimeout) {
		return send(wsconn.CloseGoingAway, "read timeout")
	}
	if errors.Is(err, errPongWriteFailed) {
		// Connection is already broken; skip the close frame entirely.
		return closeAction{SendFrame: false, Reason: "pong_write_failed"}
	}
	if errors.Is(err, wsconn.ErrReadLimit) {
		// Both supported WebSocket libraries already send 1009 when their read
		// limit is exceeded. Do not write a second close frame over the closing
		// connection; just converge local resources.
		return closeAction{SendFrame: false, Reason: "message too big"}
	}
	if errors.Is(err, errPayloadTooLarge) {
		return send(wsconn.CloseMessageTooBig, "message too big")
	}
	if ce, ok := wsconn.AsCloseError(err); ok {
		switch ce.Code {
		case wsconn.CloseNormalClosure, wsconn.CloseGoingAway, wsconn.CloseNoStatusReceived:
			return send(wsconn.CloseNormalClosure, fmt.Sprintf("client closed (code=%d)", ce.Code))
		}
		return send(wsconn.CloseInternalServerErr, "internal error")
	}
	if errors.Is(err, io.EOF) {
		return send(wsconn.CloseNormalClosure, "upstream eof")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return send(wsconn.CloseGoingAway, "canceled")
	}
	// Default: never leak the raw internal error (which may reference upstream
	// addresses or internal details) onto the wire. The full error is logged
	// against the conn_id in run() for diagnosis.
	return send(wsconn.CloseInternalServerErr, "internal error")
}

// isBenignCloseErr filters noise from the WS close path.
func isBenignCloseErr(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	// crypto/tls does not expose a sentinel; fall back to string match.
	msg := err.Error()
	return msg == "tls: use of closed connection"
}
