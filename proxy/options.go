package proxy

import (
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/internal/endpoint"
	acplog "github.com/eino-contrib/acp/internal/log"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	// DefaultMaxConcurrentConnections bounds how many active north-bound
	// WebSocket connections the proxy will serve simultaneously. Past the
	// cap, additional upgrade attempts are rejected with HTTP 503.
	DefaultMaxConcurrentConnections = 10000

	// DefaultHandshakeTimeout bounds how long StreamerFactory.NewStreamer
	// may block before the proxy gives up on a south-bound dial.
	DefaultHandshakeTimeout = 15 * time.Second

	// DefaultWebSocketWriteTimeout caps a single WS write on the north-bound
	// side so a stalled Client cannot freeze the down-pump goroutine.
	DefaultWebSocketWriteTimeout = 30 * time.Second

	// DefaultWebSocketReadTimeout bounds how long the proxy will wait on a
	// silent north-bound socket before tearing the connection down. The read
	// deadline is refreshed on every Ping and data frame. Default is 0
	// (disabled) to avoid breaking old Clients that do not send Ping.
	DefaultWebSocketReadTimeout = 0

	// DefaultWebSocketFirstFrameTimeout bounds how long the proxy waits for
	// the first data frame after downstream streamer creation. Prevents resource
	// exhaustion from connections that only send Ping without business traffic.
	DefaultWebSocketFirstFrameTimeout = 15 * time.Second

	// DefaultMaxMessageSize caps a single north-bound WebSocket message
	// payload (and a single south-bound Streamer payload relayed back).
	// Aligned with acptransport.DefaultMaxMessageSize so the proxy enforces
	// the same ceiling as the rest of the SDK, preventing a hostile or
	// broken peer from pushing arbitrary-size frames into memory at the
	// entry layer.
	DefaultMaxMessageSize = acptransport.DefaultMaxMessageSize
)

// HeaderForwarder selects which north-bound request headers propagate into
// the meta map passed to StreamerFactory.NewStreamer. Return a nil or empty
// map if no headers should be forwarded.
//
// The callback runs synchronously on the hertz handler goroutine; keep it
// allocation-light. The returned map must not be retained or mutated by the
// forwarder after return — the proxy owns it from that point.
type HeaderForwarder func(c *app.RequestContext) map[string]string

// ForwardHeaders returns a HeaderForwarder that copies the named request
// headers (case-insensitive) into meta verbatim. Headers absent from the
// request are omitted from the meta map.
func ForwardHeaders(names ...string) HeaderForwarder {
	if len(names) == 0 {
		return nil
	}
	snapshot := make([]string, len(names))
	copy(snapshot, names)
	return func(c *app.RequestContext) map[string]string {
		var out map[string]string
		for _, name := range snapshot {
			v := string(c.GetHeader(name))
			if v == "" {
				continue
			}
			if out == nil {
				// Lazy allocation: avoid paying map overhead on handshakes
				// where none of the configured headers are present, since
				// the proxy hot path runs on every WS upgrade.
				out = make(map[string]string, len(snapshot))
			}
			out[name] = v
		}
		return out
	}
}

// Option configures an ACPProxy.
type Option func(*options)

type options struct {
	endpoint          string
	upgrader          websocket.HertzUpgrader
	headerForwarder   HeaderForwarder
	maxConcurrent     int
	handshakeTimeout  time.Duration
	wsWriteTimeout    time.Duration
	wsReadTimeout     time.Duration
	firstFrameTimeout time.Duration
	maxMessageSize    int

}

func defaultOptions() options {
	return options{
		endpoint:          acptransport.DefaultACPEndpointPath,
		maxConcurrent:     DefaultMaxConcurrentConnections,
		handshakeTimeout:  DefaultHandshakeTimeout,
		wsWriteTimeout:    DefaultWebSocketWriteTimeout,
		wsReadTimeout:     DefaultWebSocketReadTimeout,
		firstFrameTimeout: DefaultWebSocketFirstFrameTimeout,
		maxMessageSize:    DefaultMaxMessageSize,
	}
}

// WithEndpoint overrides the route path (default: "/acp"). The proxy's
// default and server.ACPServer's default match on purpose — mounting both on
// the same hertz router will hard-fail at route registration, enforcing the
// "one role per process on /acp" deployment contract. The path is normalized
// (leading '/' ensured, trailing '/' stripped) so it matches server.WithEndpoint
// and the WS client transport exactly.
func WithEndpoint(path string) Option {
	return func(o *options) {
		if path != "" {
			o.endpoint = endpoint.NormalizePath(path)
		}
	}
}

// WithUpgrader overrides the WebSocket upgrader. The zero value is used
// when this option is not set.
func WithUpgrader(u websocket.HertzUpgrader) Option {
	return func(o *options) { o.upgrader = u }
}

// WithHeaderForwarder installs the HeaderForwarder used to build the meta map
// supplied to StreamerFactory.NewStreamer.
func WithHeaderForwarder(f HeaderForwarder) Option {
	return func(o *options) { o.headerForwarder = f }
}

// WithMaxConcurrentConnections sets the concurrent WS connection cap. Zero
// or a negative value disables the cap (use with care). Default: 10000.
func WithMaxConcurrentConnections(n int) Option {
	return func(o *options) { o.maxConcurrent = n }
}

// WithHandshakeTimeout bounds StreamerFactory.NewStreamer. Zero disables the
// timeout (only the connection-level ctx applies). Default: 15s.
func WithHandshakeTimeout(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.handshakeTimeout = d
		}
	}
}

// WithWebSocketWriteTimeout caps both north-bound WS writes (Streamer → Client)
// and south-bound Streamer writes (Client → AgentServer). Specifically:
//   - downPump: applied as SetWriteDeadline on the WebSocket connection;
//   - upPump: applied as context timeout on Streamer.WritePayload.
//
// Zero disables both deadlines (unbounded wait on a blocked socket — not
// recommended). Default: 30s.
func WithWebSocketWriteTimeout(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.wsWriteTimeout = d
		}
	}
}

// WithWebSocketReadTimeout bounds how long the proxy waits for any north-bound
// frame (data or Ping) before tearing the connection down. This timeout only
// takes effect after the first data frame has been received. Before that, the
// connection is protected solely by WithWebSocketFirstFrameTimeout; if first-frame
// timeout is disabled (zero), there is no deadline until the first data frame
// arrives. Without this, a half-open socket holds its concurrency slot
// indefinitely. Recommended: >= 2 × Client PingInterval (e.g. 75s for 30s
// PingInterval). Enabling this when upstream clients do not send Ping or
// periodic data frames will cause idle connections to be disconnected.
// Zero disables the deadline. Default: 0 (disabled, to avoid breaking old
// Clients).
func WithWebSocketReadTimeout(d time.Duration) Option {
	return func(o *options) {
		if !acplog.ValidateDuration("proxy", "WithWebSocketReadTimeout", d, time.Second) {
			return
		}
		o.wsReadTimeout = d
	}
}

// WithWebSocketFirstFrameTimeout bounds how long the proxy waits for the first
// data frame after the downstream streamer is created and heartbeat is installed.
// During streamer creation the connection is bounded by WithHandshakeTimeout.
// Zero disables the timeout. Default: 15s.
func WithWebSocketFirstFrameTimeout(d time.Duration) Option {
	return func(o *options) {
		if !acplog.ValidateDuration("proxy", "WithWebSocketFirstFrameTimeout", d, time.Second) {
			return
		}
		o.firstFrameTimeout = d
	}
}

// WithMaxMessageSize caps a single WebSocket payload in bytes (north-bound
// inbound frames via SetReadLimit and south-bound Streamer payloads relayed
// back). Zero or a negative value disables the cap — not recommended for
// untrusted clients, as a single oversized frame could otherwise allocate
// unbounded memory at the proxy layer. Default:
// transport.DefaultMaxMessageSize (10MB).
func WithMaxMessageSize(size int) Option {
	return func(o *options) { o.maxMessageSize = size }
}
