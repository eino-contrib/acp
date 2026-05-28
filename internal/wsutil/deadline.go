package wsutil

import "time"

// ControlWriteDeadline is the deadline used for all WriteControl calls
// (Ping, Pong, Close frames). Set to 5s to tolerate contention with
// concurrent data frame writes that share the same internal write lock
// (data frames may hold it for up to 30s via the write timeout).
const ControlWriteDeadline = 5 * time.Second
