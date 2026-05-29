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
		if !log.ValidateDuration("server", "WithReadTimeout", d, time.Second) {
			return
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
		if !log.ValidateDuration("server", "WithInitializeTimeout", d, time.Second) {
			return
		}
		t.initializeTimeout = d
	}
}
