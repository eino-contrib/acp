package wsutil

import (
	"errors"
	"net"
	"time"
)

// ControlWriteDeadline is the deadline used for all WriteControl calls
// (Ping, Pong, Close frames). Set to 5s to tolerate contention with
// concurrent data frame writes that share the same internal write lock
// (data frames may hold it for up to 30s via the write timeout).
const ControlWriteDeadline = 5 * time.Second

// hertzWriteLockTimeoutMsg is the message produced by hertz-contrib/websocket
// when WriteControl times out waiting for the connection's shared internal
// write lock (`errWriteTimeout` in conn.go). It is a stable string surfaced
// via net.Error and is the only signal we have to distinguish a write-lock
// contention timeout from a real socket write deadline expiry — both surface
// as net.Error with Timeout()==true, but only the former leaves the
// connection usable. The string is matched verbatim and contains no dynamic
// fields, so it is a safe sentinel even though the underlying type is
// unexported.
const hertzWriteLockTimeoutMsg = "websocket: write timeout"

// IsControlWriteContention reports whether a WriteControl error is a transient
// timeout caused by losing the race for the connection's shared internal write
// lock against an in-flight data frame — rather than a genuine connection
// failure.
//
// Discrimination is necessary because hertz-contrib/websocket surfaces *both*
// "waited too long for the write lock" and "the underlying socket Write hit
// its deadline" as net.Error with Timeout()==true. Treating every Timeout()
// the same swallows real socket-write failures, which would otherwise drive
// Client ping_write_failed / Server-Proxy pong_write_failed convergence and
// the documented error-classification metrics. We therefore only treat the
// stable lock-wait sentinel as contention; everything else (including socket
// write deadline expiry) is reported as a real failure.
//
// Such a contention timeout is NOT evidence that the peer is dead: a large
// data frame can hold the write lock for up to the data write deadline (e.g.
// 30s), well past the 5s ControlWriteDeadline. The read deadline is the
// authoritative liveness check, so callers should swallow these errors and
// keep the connection alive instead of tearing it down.
func IsControlWriteContention(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		return false
	}
	// hertz-contrib/websocket's lock-wait timeout is the only Timeout()
	// path that leaves the connection usable. Match its stable message
	// verbatim — anything else (e.g. a wrapped *net.OpError from socket
	// write deadline expiry) is treated as a real write failure.
	return ne.Error() == hertzWriteLockTimeoutMsg
}
