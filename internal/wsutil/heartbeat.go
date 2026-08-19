package wsutil

import (
	"time"

	"github.com/eino-contrib/acp/internal/wsconn"
)

// PongResponder builds a WebSocket PingHandler that echoes a Pong frame for
// every inbound Ping and, optionally, refreshes the read deadline. Both the
// server transport and the proxy need exactly this behaviour — including the
// subtle rule that a Pong write which loses the race for the connection's
// shared internal write lock (a transient timeout) must NOT tear the
// connection down, because the read deadline is the authoritative liveness
// check. Centralising it here keeps the two call sites from drifting apart.
//
// WriteControl is required; every other field is optional.
type PongResponder struct {
	// WriteControl writes the echoed Pong control frame to the peer. Required.
	WriteControl func(messageType int, data []byte, deadline time.Time) error

	// SetReadDeadline refreshes the read deadline. Consulted only when
	// ReadTimeout > 0 and RefreshDeadline reports true.
	SetReadDeadline func(time.Time) error

	// ReadTimeout is the window applied when the deadline is refreshed. Zero
	// disables deadline refresh entirely.
	ReadTimeout time.Duration

	// RefreshDeadline reports whether the read deadline should be refreshed on
	// this Pong. Servers refresh only after initialize completes; the proxy
	// refreshes only after the first data frame. Nil means never refresh.
	RefreshDeadline func() bool

	// OnContention runs when the Pong write loses the shared write-lock race
	// (transient; the connection is kept alive). Typically a warn log.
	OnContention func(err error)

	// OnWriteFailed runs when the Pong write fails for a non-contention reason
	// (the connection is considered broken). It executes before the handler
	// returns, so it is the place to record side effects such as "a close
	// frame must be suppressed". Typically a warn log.
	OnWriteFailed func(err error)

	// WrapWriteFailed transforms the non-contention write error before the
	// handler returns it (and thus before ReadMessage surfaces it). Nil returns
	// the error unchanged; the proxy uses this to tag the error with its own
	// sentinel for close-code classification.
	WrapWriteFailed func(err error) error
}

// Handler returns a function suitable for wsconn.Conn.SetPingHandler.
func (r PongResponder) Handler() func(appData string) error {
	return func(appData string) error {
		if err := r.WriteControl(wsconn.PongMessage, []byte(appData), time.Now().Add(ControlWriteDeadline)); err != nil {
			if IsControlWriteContention(err) {
				if r.OnContention != nil {
					r.OnContention(err)
				}
				// Receiving the Ping is itself proof the peer is alive; the
				// Pong write only lost the race for the shared write lock.
				// Refresh the read deadline so persistent contention does
				// not let the deadline expire on a healthy connection.
				r.refreshReadDeadlineIfNeeded()
				return nil
			}
			if r.OnWriteFailed != nil {
				r.OnWriteFailed(err)
			}
			if r.WrapWriteFailed != nil {
				return r.WrapWriteFailed(err)
			}
			return err
		}
		r.refreshReadDeadlineIfNeeded()
		return nil
	}
}

// refreshReadDeadlineIfNeeded extends the read deadline by ReadTimeout when
// the responder is fully configured for it and RefreshDeadline reports true.
// It is safe to call from both the success path and the write-lock
// contention path.
func (r PongResponder) refreshReadDeadlineIfNeeded() {
	if r.ReadTimeout > 0 && r.SetReadDeadline != nil && r.RefreshDeadline != nil && r.RefreshDeadline() {
		_ = r.SetReadDeadline(time.Now().Add(r.ReadTimeout))
	}
}
