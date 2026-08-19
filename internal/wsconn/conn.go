// Package wsconn defines the WebSocket connection contract used by the
// framework-neutral server and proxy cores. Concrete WebSocket libraries are
// kept behind the adapters in this package.
package wsconn

import "time"

// Conn is the subset of a WebSocket connection required by the ACP
// transports. Implementations must follow the usual WebSocket concurrency
// rule: at most one concurrent reader and one concurrent writer.
type Conn interface {
	ReadMessage() (messageType int, payload []byte, err error)
	WriteMessage(messageType int, payload []byte) error
	WriteControl(messageType int, payload []byte, deadline time.Time) error
	SetReadLimit(limit int64)
	SetReadDeadline(deadline time.Time) error
	SetWriteDeadline(deadline time.Time) error
	SetPingHandler(handler func(appData string) error)
	Close() error
}
