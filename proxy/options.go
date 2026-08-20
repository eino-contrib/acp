package proxy

import (
	"context"
	"time"

	"github.com/eino-contrib/acp/internal/wsutil"
	acptransport "github.com/eino-contrib/acp/transport"
)

// WebSocketConn is the framework-neutral, already-upgraded WebSocket contract
// accepted by Admission. Built-in Hertz and Gin adapters wrap their native
// connections; custom adapters may implement the same method set.
type WebSocketConn interface {
	ReadMessage() (messageType int, payload []byte, err error)
	WriteMessage(messageType int, payload []byte) error
	WriteControl(messageType int, payload []byte, deadline time.Time) error
	SetReadLimit(limit int64)
	SetReadDeadline(deadline time.Time) error
	SetWriteDeadline(deadline time.Time) error
	SetPingHandler(handler func(appData string) error)
	Close() error
}

const (
	// DefaultEndpoint is the conventional route on which an ACP proxy is
	// registered. The proxy core does not own a route; applications pass this
	// value (or any other path) to their Hertz or Gin router.
	DefaultEndpoint = acptransport.DefaultACPEndpointPath

	// DefaultMaxConcurrentConnections bounds how many admitted north-bound
	// WebSocket connections the proxy will serve simultaneously. Past the cap,
	// additional attempts are rejected with HTTP 503.
	DefaultMaxConcurrentConnections = 10000

	// DefaultHandshakeTimeout bounds how long StreamerFactory.NewStreamer may
	// block before the proxy reports a downstream creation failure.
	DefaultHandshakeTimeout = 15 * time.Second

	// DefaultWebSocketWriteTimeout caps a single north-bound WebSocket write or
	// south-bound Streamer write.
	DefaultWebSocketWriteTimeout = 30 * time.Second

	// DefaultWebSocketReadTimeout bounds how long the proxy waits on a silent
	// north-bound socket after the first data frame. Zero disables it.
	DefaultWebSocketReadTimeout = 0

	// DefaultWebSocketFirstFrameTimeout bounds how long the proxy waits for the
	// first data frame after the downstream Streamer has been created.
	DefaultWebSocketFirstFrameTimeout = 15 * time.Second

	// DefaultMaxMessageSize caps one north-bound WebSocket payload and one
	// south-bound Streamer payload relayed back to the client.
	DefaultMaxMessageSize = acptransport.DefaultMaxMessageSize
)

// HeaderGetter reads one request header without exposing a framework request
// object. Hertz and Gin adapters construct a getter over their native request.
// Header names are interpreted case-insensitively by the underlying framework.
type HeaderGetter func(name string) string

// Get returns the value for name. A nil getter behaves like an empty header
// collection.
func (g HeaderGetter) Get(name string) string {
	if g == nil {
		return ""
	}
	return g(name)
}

// MetadataExtractor builds the metadata passed to
// stream.StreamerFactory.NewStreamer. It receives the request context and a
// read-only, framework-neutral header accessor. The proxy takes an immediate
// copy of the returned map; callers may therefore reuse their own storage.
type MetadataExtractor func(context.Context, HeaderGetter) map[string]string

// ForwardHeaders returns a MetadataExtractor that copies the named request
// headers into downstream metadata. Missing or empty headers are omitted. The
// supplied spelling is retained as the metadata key.
func ForwardHeaders(names ...string) MetadataExtractor {
	if len(names) == 0 {
		return nil
	}
	snapshot := append([]string(nil), names...)
	return func(_ context.Context, headers HeaderGetter) map[string]string {
		var out map[string]string
		for _, name := range snapshot {
			value := headers.Get(name)
			if value == "" {
				continue
			}
			if out == nil {
				out = make(map[string]string, len(snapshot))
			}
			out[name] = value
		}
		return out
	}
}

// Option configures an ACPProxy. Framework-specific settings such as route
// paths, origin checks, buffers, compression, and WebSocket upgraders belong
// to proxy/hertz or proxy/gin rather than to the core.
type Option func(*options)

type options struct {
	metadataExtractor MetadataExtractor
	maxConcurrent     int
	handshakeTimeout  time.Duration
	wsWriteTimeout    time.Duration
	wsReadTimeout     time.Duration
	firstFrameTimeout time.Duration
	maxMessageSize    int
}

func defaultOptions() options {
	return options{
		maxConcurrent:     DefaultMaxConcurrentConnections,
		handshakeTimeout:  DefaultHandshakeTimeout,
		wsWriteTimeout:    DefaultWebSocketWriteTimeout,
		wsReadTimeout:     DefaultWebSocketReadTimeout,
		firstFrameTimeout: DefaultWebSocketFirstFrameTimeout,
		maxMessageSize:    DefaultMaxMessageSize,
	}
}

// WithMetadataExtractor installs the framework-neutral extractor used to
// build metadata for StreamerFactory.NewStreamer.
func WithMetadataExtractor(extractor MetadataExtractor) Option {
	return func(o *options) { o.metadataExtractor = extractor }
}

// WithMaxConcurrentConnections sets the admitted connection cap. Zero or a
// negative value disables the cap.
func WithMaxConcurrentConnections(n int) Option {
	return func(o *options) { o.maxConcurrent = n }
}

// WithHandshakeTimeout bounds StreamerFactory.NewStreamer. Zero disables the
// timeout. A factory must observe its context; Shutdown reports its own
// deadline if a factory ignores cancellation and never returns.
func WithHandshakeTimeout(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.handshakeTimeout = d
		}
	}
}

// WithWebSocketWriteTimeout caps both north-bound WebSocket writes and
// south-bound Streamer writes. Zero disables both deadlines.
func WithWebSocketWriteTimeout(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.wsWriteTimeout = d
		}
	}
}

// WithWebSocketReadTimeout bounds north-bound inactivity after the first data
// frame. Ping and data frames refresh it. Zero disables the deadline.
func WithWebSocketReadTimeout(d time.Duration) Option {
	return func(o *options) {
		if !wsutil.ValidateDuration("proxy", "WithWebSocketReadTimeout", d, time.Second) {
			return
		}
		o.wsReadTimeout = d
	}
}

// WithWebSocketFirstFrameTimeout bounds the wait for the first data frame
// after downstream Streamer creation. Zero disables the deadline.
func WithWebSocketFirstFrameTimeout(d time.Duration) Option {
	return func(o *options) {
		if !wsutil.ValidateDuration("proxy", "WithWebSocketFirstFrameTimeout", d, time.Second) {
			return
		}
		o.firstFrameTimeout = d
	}
}

// WithWebSocketPingInterval is retained for source compatibility with the
// pre-heartbeat-refactor runtime configuration. The proxy does not initiate
// Ping frames, so this option only validates d and otherwise has no effect.
//
// Deprecated: liveness is driven by client Ping frames and
// WithWebSocketReadTimeout.
func WithWebSocketPingInterval(d time.Duration) Option {
	return func(_ *options) {
		_ = wsutil.ValidateDuration("proxy", "WithWebSocketPingInterval", d, time.Second)
	}
}

// WithWebSocketPongTimeout aliases WithWebSocketReadTimeout.
//
// Deprecated: use WithWebSocketReadTimeout.
func WithWebSocketPongTimeout(d time.Duration) Option {
	return WithWebSocketReadTimeout(d)
}

// WithMaxMessageSize caps a single payload in bytes. Zero or a negative value
// disables the cap.
func WithMaxMessageSize(size int) Option {
	return func(o *options) { o.maxMessageSize = size }
}
