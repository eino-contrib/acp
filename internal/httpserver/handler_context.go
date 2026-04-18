package httpserver

import (
	"context"
	"encoding/json"
)

// HandlerContext abstracts the HTTP request/response for framework-agnostic
// handler logic. The current implementation is the Hertz adapter (see
// adapter_hertz.go); additional adapters can be added without touching the
// core protocol logic.
type HandlerContext interface {
	// Context returns the request-scoped context so protocol handlers can
	// preserve request values, traces, and cancellation semantics.
	Context() context.Context
	// RequestHeader returns a request header value.
	RequestHeader(key string) string
	// RequestBody returns the raw request body (may be called once).
	RequestBody() ([]byte, error)
	// SetResponseHeader sets a response header before WriteHeader/Flush.
	SetResponseHeader(key, value string)
	// WriteError writes a plain-text error response and completes the request.
	WriteError(code int, msg string)
	// SetStatusCode sets the HTTP response status.
	SetStatusCode(code int)
	// Flush flushes buffered response data to the client.
	Flush()
	// Done returns a channel that's closed when the request context ends.
	Done() <-chan struct{}

	// --- SSE support ---

	// WriteSSEEvent writes a single SSE "event: message" with the given data.
	WriteSSEEvent(msg json.RawMessage) error
	// WriteSSEKeepAlive writes an SSE comment as a keepalive.
	WriteSSEKeepAlive() error
	// CloseSSE closes the SSE writer if applicable.
	CloseSSE()
}
