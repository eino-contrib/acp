package wsserver

import (
	"time"

	"github.com/eino-contrib/acp/internal/log"
)

// Option configures a wsserver Transport.
type Option func(*Transport)

// WithReadTimeout sets the read deadline for the WebSocket connection after
// initialization completes. If no frame (Ping or data) arrives within this
// window, the connection is closed. Zero disables the deadline (default).
// Negative values are ignored.
func WithReadTimeout(d time.Duration) Option {
	return func(t *Transport) {
		if d < 0 {
			log.Warn("[ws] role=server option=WithReadTimeout value=%v constraint=\"must be >= 0\" action=ignored", d)
			return
		}
		if d > 0 && d < time.Second {
			log.Warn("[ws] role=server option=WithReadTimeout value=%v constraint=\"production value should be >= 1s\"", d)
		}
		t.readTimeout = d
	}
}

// WithInitializeTimeout sets the deadline for the client to send the
// initialize request after WebSocket upgrade. Zero disables the deadline.
// Negative values are ignored.
// Note: this transport has no built-in default; server.ACPServer passes 15s.
func WithInitializeTimeout(d time.Duration) Option {
	return func(t *Transport) {
		if d < 0 {
			log.Warn("[ws] role=server option=WithInitializeTimeout value=%v constraint=\"must be >= 0\" action=ignored", d)
			return
		}
		if d > 0 && d < time.Second {
			log.Warn("[ws] role=server option=WithInitializeTimeout value=%v constraint=\"production value should be >= 1s\"", d)
		}
		t.initializeTimeout = d
	}
}
