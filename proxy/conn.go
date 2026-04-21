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
	"github.com/eino-contrib/acp/internal/wsutil"
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

	// Heartbeat knobs: pingInterval is how often pingPump emits a Ping frame;
	// pongTimeout is the read deadline refreshed on every pong and every
	// successfully-read data frame. Either <= 0 disables that half of the
	// heartbeat independently.
	pingInterval time.Duration
	pongTimeout  time.Duration

	// maxMessageSize caps a single south-bound payload read from the Streamer
	// before it is relayed to the north-bound WS. North-bound reads are
	// capped by SetReadLimit on the underlying *websocket.Conn; this field
	// is only consulted on the down path. <= 0 disables the cap.
	maxMessageSize int

	closeOnce   sync.Once
	closeReasonMu sync.Mutex
	closeReasonS  string
}

// installHeartbeat wires the read deadline and pong handler so a silent
// north-bound socket cannot hold a concurrency slot forever. Safe to call
// with pongTimeout <= 0 (no-op).
func (pc *proxyConn) installHeartbeat() {
	if pc.pongTimeout <= 0 {
		return
	}
	_ = pc.ws.SetReadDeadline(time.Now().Add(pc.pongTimeout))
	pc.ws.SetPongHandler(func(string) error {
		// Every pong extends the deadline by another full pongTimeout, so a
		// client that keeps acknowledging pings stays alive indefinitely.
		return pc.ws.SetReadDeadline(time.Now().Add(pc.pongTimeout))
	})
}

// run drives the pumps and returns after the two terminal pumps have exited.
// It always closes the connection before returning. A best-effort pingPump is
// started alongside and torn down in step with the terminal pumps.
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

	// pingPump is best-effort and not a terminal pump: it exits once
	// pingCtx is cancelled (either when run returns or when the close path
	// tears the socket down and pingWrite fails). We still Wait for it so
	// the caller's goroutine accounting stays exact.
	pingCtx, pingCancel := context.WithCancel(ctx)
	var pingWG sync.WaitGroup
	if pc.pingInterval > 0 {
		pingWG.Add(1)
		safe.GoRecover(
			func() { defer pingWG.Done(); pc.pingPump(pingCtx) },
			func(r any) { defer pingWG.Done(); acplog.Warn("proxy[%s]: pingPump panic: %v", pc.id, r) },
		)
	}

	first := <-errCh
	code, reason := classifyPumpErr(first)
	pc.close(code, reason)
	pingCancel()

	// Drain the second pump. It will see either the closed WS (upPump) or
	// the closed Streamer (downPump) and return promptly.
	<-errCh
	pingWG.Wait()
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
		// WS read deadlines are absolute, not sliding: a successful data-frame
		// read does NOT auto-extend the deadline. An actively-sending peer that
		// happens not to emit pongs within pongTimeout would otherwise be
		// reaped as idle. Refresh here so any inbound traffic counts as
		// liveness, with PongHandler as the silent-peer fallback.
		if pc.pongTimeout > 0 {
			_ = pc.ws.SetReadDeadline(time.Now().Add(pc.pongTimeout))
		}
		acplog.Access(ctx, "proxy-up", acplog.AccessDirectionRecv, data)
		if err := pc.streamer.WritePayload(ctx, data); err != nil {
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
			// Refuse to forward an oversized payload: a custom Streamer may
			// not enforce its own cap, and relaying an arbitrary-size frame
			// back to the WS client would defeat the north-bound SetReadLimit
			// protection and risk the peer's own buffers too.
			return fmt.Errorf("streamer payload %d bytes: %w", len(payload), errPayloadTooLarge)
		}
		acplog.Access(ctx, "proxy-down", acplog.AccessDirectionSend, payload)
		if err := pc.writeWSMessage(websocket.TextMessage, payload); err != nil {
			return fmt.Errorf("ws write: %w", err)
		}
	}
}

// pingPump emits an application-level Ping frame at pingInterval. It is not a
// terminal pump: write failures or ctx cancellation just end the loop. The
// matching Pong refreshes the read deadline via SetPongHandler (installed in
// installHeartbeat), so a silent peer will eventually trip upPump's read
// deadline and tear the whole connection down through the normal path.
func (pc *proxyConn) pingPump(ctx context.Context) {
	t := time.NewTicker(pc.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := pc.writeWSMessage(websocket.PingMessage, nil); err != nil {
				// Either the socket was closed by the terminal pumps or the
				// peer is no longer reachable. In both cases the terminal
				// pumps will return shortly; no need to escalate here.
				return
			}
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
		// Best-effort close frame to help the Client SDK report the cause.
		_ = pc.writeWSMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, wsutil.SafeCloseReason(reason)))

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
// reason the proxy should return to the client. The close code is the key
// signal clients use to decide whether a shutdown was clean (no retry) or
// abnormal (retry / alert), so this mapping matters more than the reason
// string.
func classifyPumpErr(err error) (int, string) {
	if err == nil {
		return websocket.CloseNormalClosure, ""
	}
	// Size-cap breach on either direction — echo a 1009 so the peer can
	// distinguish "too big" from a generic server fault. The north-bound
	// library already best-effort-sent its own 1009 frame, but our close
	// path still runs and a matching code keeps logs consistent.
	if errors.Is(err, websocket.ErrReadLimit) || errors.Is(err, errPayloadTooLarge) {
		return websocket.CloseMessageTooBig, "message too big"
	}
	// Peer-initiated graceful close surfaces as a CloseError from
	// ws.ReadMessage; echo a matching code so the handshake looks symmetric.
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived:
			return websocket.CloseNormalClosure, fmt.Sprintf("client closed (code=%d)", ce.Code)
		}
		// Unusual peer close code — treat as abnormal but preserve the peer's
		// reason for diagnostics.
		return websocket.CloseInternalServerErr, err.Error()
	}
	// Streamer signalled graceful EOF — normal shutdown from upstream.
	if errors.Is(err, io.EOF) {
		return websocket.CloseNormalClosure, "upstream eof"
	}
	// Connection-scoped cancellation (parent ctx or proxy shutdown): the
	// server side is going away, not the client.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return websocket.CloseGoingAway, "canceled"
	}
	// Anything else is a server-side / upstream fault.
	return websocket.CloseInternalServerErr, err.Error()
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
