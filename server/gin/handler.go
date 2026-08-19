// Package gin adapts a framework-neutral server.ACPServer to a native Gin
// handler. The host application owns route registration.
package gin

import (
	"net/http"

	ginframework "github.com/gin-gonic/gin"
	gorillawebsocket "github.com/gorilla/websocket"

	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsupgrade"
	acpserver "github.com/eino-contrib/acp/server"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Option configures a Gin handler adapter. Options are applied while New is
// constructing the immutable handler and must not be mutated concurrently.
type Option func(*adapter)

// WithUpgrader replaces the Gorilla WebSocket upgrader used by the adapter.
// Origin, buffer, compression, and subprotocol settings therefore remain at
// the framework boundary instead of leaking into ACPServer.
func WithUpgrader(upgrader gorillawebsocket.Upgrader) Option {
	return func(a *adapter) { a.upgrader = upgrader }
}

type adapter struct {
	core     *acpserver.ACPServer
	upgrader gorillawebsocket.Upgrader
}

// New returns a native Gin handler for Streamable HTTP and WebSocket ACP.
// Register it with the host router, for example:
//
//	router.Any(acpserver.DefaultEndpoint, acpgin.New(core))
func New(core *acpserver.ACPServer, opts ...Option) ginframework.HandlerFunc {
	a := &adapter{core: core}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a.handle
}

func (a *adapter) handle(c *ginframework.Context) {
	if c == nil || c.IsAborted() {
		return
	}

	// The ACP handler owns the endpoint response. In particular, no later Gin
	// handler may write after Gorilla has hijacked the connection.
	c.Abort()

	if a.core == nil {
		writeText(c, http.StatusServiceUnavailable, "ACP server unavailable")
		return
	}
	if c.Request == nil {
		writeText(c, http.StatusBadRequest, "invalid HTTP request")
		return
	}

	req := wsupgrade.Request{
		Method:       c.Request.Method,
		Header:       c.Request.Header.Get,
		HeaderValues: c.Request.Header.Values,
	}
	if !wsupgrade.IsAttempt(req) {
		a.core.ServeHTTP(
			acphttpserver.NewHTTPHandlerContext(c.Writer, c.Request),
			req.Method,
		)
		return
	}
	if err := wsupgrade.Validate(req); err != nil {
		writeText(c, http.StatusBadRequest, err.Error())
		return
	}

	admission, err := a.core.AdmitWebSocket(c.Request.Context())
	if err != nil {
		writeText(c, http.StatusServiceUnavailable, "ACP server is shutting down")
		return
	}
	defer admission.Abort()

	responseHeader := make(http.Header, 1)
	responseHeader.Set(acptransport.HeaderConnectionID, admission.ConnectionID())
	native, err := a.upgrader.Upgrade(c.Writer, c.Request, responseHeader)
	if err != nil {
		acplog.CtxWarn(c.Request.Context(), "Gin websocket upgrade failed: %v", err)
		return
	}

	if err := admission.Serve(wsconn.WrapGorilla(native)); err != nil {
		acplog.CtxWarn(c.Request.Context(), "serve Gin websocket: %v", err)
	}
}

func writeText(c *ginframework.Context, status int, body string) {
	c.Data(status, "text/plain; charset=utf-8", []byte(body))
}
