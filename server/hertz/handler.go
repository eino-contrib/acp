// Package hertz adapts a framework-neutral server.ACPServer to a native
// CloudWeGo Hertz handler. The host application owns route registration.
package hertz

import (
	"context"
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	hertzwebsocket "github.com/hertz-contrib/websocket"

	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsupgrade"
	acpserver "github.com/eino-contrib/acp/server"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Option configures a Hertz handler adapter. Options are applied while New is
// constructing the immutable handler and must not be mutated concurrently.
type Option func(*adapter)

// WithUpgrader replaces the Hertz WebSocket upgrader used by the adapter.
// Origin, buffer, compression, and subprotocol settings therefore remain at
// the framework boundary instead of leaking into ACPServer.
func WithUpgrader(upgrader hertzwebsocket.HertzUpgrader) Option {
	return func(a *adapter) { a.upgrader = upgrader }
}

type adapter struct {
	core     *acpserver.ACPServer
	upgrader hertzwebsocket.HertzUpgrader
}

// New returns a native Hertz handler for Streamable HTTP and WebSocket ACP.
// Register it with the host router, for example:
//
//	router.Any(acpserver.DefaultEndpoint, acphertz.New(core))
func New(core *acpserver.ACPServer, opts ...Option) app.HandlerFunc {
	a := &adapter{core: core}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a.handle
}

func (a *adapter) handle(ctx context.Context, c *app.RequestContext) {
	if c == nil {
		return
	}
	c.Abort()
	if a.core == nil {
		acphttpserver.WriteHertzText(c, http.StatusServiceUnavailable, "ACP server unavailable")
		return
	}
	req := wsupgrade.Request{
		Method:       string(c.Method()),
		Header:       func(name string) string { return string(c.GetHeader(name)) },
		HeaderValues: func(name string) []string { return hertzHeaderValues(c, name) },
	}
	if !wsupgrade.IsAttempt(req) {
		a.core.ServeHTTP(acphttpserver.NewHertzHandlerContext(ctx, c), req.Method)
		return
	}
	if err := wsupgrade.Validate(req); err != nil {
		acphttpserver.WriteHertzText(c, websocketValidationStatus(err), err.Error())
		return
	}
	admission, err := a.core.AdmitWebSocket(ctx)
	if err != nil {
		acphttpserver.WriteHertzText(c, http.StatusServiceUnavailable, "ACP server is shutting down")
		return
	}
	// HertzUpgrader copies pre-populated response headers into the 101
	// handshake. If it rejects the handshake, remove the provisional ID so a
	// failed response never exposes a connection identifier.
	c.Response.Header.Set(acptransport.HeaderConnectionID, admission.ConnectionID())
	defer func() {
		if recovered := recover(); recovered != nil {
			admission.Abort()
			c.Response.Header.Del(acptransport.HeaderConnectionID)
			panic(recovered)
		}
	}()
	err = a.upgrader.Upgrade(c, func(native *hertzwebsocket.Conn) {
		if serveErr := admission.Serve(wsconn.WrapHertz(native)); serveErr != nil {
			acplog.CtxWarn(ctx, "serve Hertz websocket: %v", serveErr)
		}
	})
	if err != nil {
		admission.Abort()
		c.Response.Header.Del(acptransport.HeaderConnectionID)
		acplog.CtxWarn(ctx, "Hertz websocket upgrade failed: %v", err)
	}
}

func hertzHeaderValues(c *app.RequestContext, name string) []string {
	values := c.Request.Header.PeekAll(name)
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}

func websocketValidationStatus(err error) int {
	if errors.Is(err, wsupgrade.ErrMethod) {
		return http.StatusBadRequest
	}
	return http.StatusBadRequest
}
