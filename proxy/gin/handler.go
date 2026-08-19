// Package gin adapts the framework-neutral proxy runtime to Gin and Gorilla
// WebSocket.
package gin

import (
	"errors"
	"net/http"

	ginframework "github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsupgrade"
	acpproxy "github.com/eino-contrib/acp/proxy"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Option configures the Gin/Gorilla-specific WebSocket adapter.
type Option func(*options)

type options struct {
	upgrader gorillaws.Upgrader
}

// WithUpgrader replaces the Gorilla WebSocket upgrader. Framework concerns
// such as origin checks, compression, subprotocols, and buffers belong here.
func WithUpgrader(upgrader gorillaws.Upgrader) Option {
	return func(o *options) { o.upgrader = upgrader }
}

// New returns a Gin-native handler backed by core. Route registration and the
// endpoint path remain the application's responsibility. Middleware values
// needed by the proxy must be placed in c.Request.Context or request headers.
func New(core *acpproxy.ACPProxy, opts ...Option) ginframework.HandlerFunc {
	resolved := options{}
	for _, option := range opts {
		if option != nil {
			option(&resolved)
		}
	}
	return func(c *ginframework.Context) {
		if c == nil || c.IsAborted() {
			return
		}
		c.Abort()
		if core == nil {
			c.String(http.StatusServiceUnavailable, "proxy runtime is nil")
			return
		}
		if c.Request == nil {
			c.String(http.StatusBadRequest, "invalid HTTP request")
			return
		}

		headers := acpproxy.HeaderGetter(c.Request.Header.Get)
		handshake := wsupgrade.Request{
			Method:       c.Request.Method,
			Header:       headers.Get,
			HeaderValues: c.Request.Header.Values,
		}
		if !wsupgrade.IsAttempt(handshake) {
			c.String(http.StatusBadRequest, "proxy endpoint only supports WebSocket")
			return
		}
		if err := wsupgrade.Validate(handshake); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}

		admission, err := core.Admit(c.Request.Context(), headers)
		if err != nil {
			writeAdmissionError(c, err)
			return
		}
		defer admission.Abort()
		responseHeader := http.Header{}
		responseHeader.Set(acptransport.HeaderConnectionID, admission.ConnectionID())
		conn, err := resolved.upgrader.Upgrade(c.Writer, c.Request, responseHeader)
		if err != nil {
			acplog.CtxWarn(c.Request.Context(), "Gin proxy websocket upgrade failed: %v", err)
			return
		}
		admission.Serve(wsconn.WrapGorilla(conn))
	}
}

func writeAdmissionError(c *ginframework.Context, err error) {
	switch {
	case errors.Is(err, acpproxy.ErrClosed), errors.Is(err, acpproxy.ErrTooManyConnections):
		c.String(http.StatusServiceUnavailable, err.Error())
	default:
		c.String(http.StatusInternalServerError, "proxy admission failed")
	}
}
