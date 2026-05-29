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

// IsControlWriteContention reports whether a WriteControl error is a transient
// timeout caused by losing the race for the connection's shared internal write
// lock against an in-flight data frame — rather than a genuine connection
// failure. The underlying websocket library returns a net.Error with
// Timeout()==true in this case.
//
// Such a timeout is NOT evidence that the peer is dead: a large data frame can
// hold the write lock for up to the data write deadline (e.g. 30s), well past
// the 5s ControlWriteDeadline. The read deadline is the authoritative liveness
// check, so callers should swallow these errors and keep the connection alive
// instead of tearing it down.
func IsControlWriteContention(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
