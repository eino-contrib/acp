package proxy

import (
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/websocket"

	acphttp "github.com/eino-contrib/acp/transport/http"
)

const (
	// DefaultMaxConcurrentConnections bounds how many active north-bound
	// WebSocket connections the proxy will serve simultaneously. Past the
	// cap, additional upgrade attempts are rejected with HTTP 503.
	DefaultMaxConcurrentConnections = 256

	// DefaultHandshakeTimeout bounds how long StreamerFactory.NewStreamer
	// may block before the proxy gives up on a south-bound dial.
	DefaultHandshakeTimeout = 15 * time.Second

	// DefaultWebSocketWriteTimeout caps a single WS write on the north-bound
	// side so a stalled Client cannot freeze the down-pump goroutine.
	DefaultWebSocketWriteTimeout = 30 * time.Second
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
		out := make(map[string]string, len(snapshot))
		for _, name := range snapshot {
			if v := string(c.GetHeader(name)); v != "" {
				out[name] = v
			}
		}
		return out
	}
}

// Option configures an ACPProxy.
type Option func(*options)

type options struct {
	endpoint         string
	upgrader         websocket.HertzUpgrader
	headerForwarder  HeaderForwarder
	maxConcurrent    int
	handshakeTimeout time.Duration
	wsWriteTimeout   time.Duration
}

func defaultOptions() options {
	return options{
		endpoint:         acphttp.DefaultACPEndpointPath,
		maxConcurrent:    DefaultMaxConcurrentConnections,
		handshakeTimeout: DefaultHandshakeTimeout,
		wsWriteTimeout:   DefaultWebSocketWriteTimeout,
	}
}

// WithEndpoint overrides the route path (default: "/acp"). The proxy's
// default and server.ACPServer's default match on purpose — mounting both on
// the same hertz router will hard-fail at route registration, enforcing the
// "one role per process on /acp" deployment contract.
func WithEndpoint(path string) Option {
	return func(o *options) {
		if path != "" {
			o.endpoint = path
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
// or a negative value disables the cap (use with care). Default: 256.
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

// WithWebSocketWriteTimeout caps a single north-bound WS write. Zero disables
// the deadline (unbounded wait on a blocked socket — not recommended).
// Default: 30s.
func WithWebSocketWriteTimeout(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.wsWriteTimeout = d
		}
	}
}
