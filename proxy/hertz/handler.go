// Package hertz adapts the framework-neutral proxy runtime to Hertz.
package hertz

import (
	"context"
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	hertzws "github.com/hertz-contrib/websocket"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsupgrade"
	acpproxy "github.com/eino-contrib/acp/proxy"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Option configures the Hertz-specific WebSocket adapter.
type Option func(*options)

type options struct {
	upgrader hertzws.HertzUpgrader
}

// WithUpgrader replaces the Hertz WebSocket upgrader. Framework concerns such
// as origin checks, compression, subprotocols, and buffers belong here.
func WithUpgrader(upgrader hertzws.HertzUpgrader) Option {
	return func(o *options) { o.upgrader = upgrader }
}

// New returns a Hertz-native handler backed by core. Route registration and
// the endpoint path remain the application's responsibility. Hertz servers
// serving upgraded connections must set NoHijackConnPool to true.
func New(core *acpproxy.ACPProxy, opts ...Option) app.HandlerFunc {
	resolved := options{}
	for _, option := range opts {
		if option != nil {
			option(&resolved)
		}
	}
	return func(ctx context.Context, request *app.RequestContext) {
		if request == nil {
			return
		}
		request.Abort()
		if core == nil {
			request.String(http.StatusServiceUnavailable, "proxy runtime is nil")
			return
		}
		headers := acpproxy.HeaderGetter(func(name string) string {
			return string(request.GetHeader(name))
		})
		handshake := wsupgrade.Request{
			Method:       string(request.Method()),
			Header:       headers.Get,
			HeaderValues: func(name string) []string { return hertzHeaderValues(request, name) },
		}
		if !wsupgrade.IsAttempt(handshake) {
			request.String(http.StatusBadRequest, "proxy endpoint only supports WebSocket")
			return
		}
		if err := wsupgrade.Validate(handshake); err != nil {
			request.String(http.StatusBadRequest, err.Error())
			return
		}
		admission, err := core.Admit(ctx, headers)
		if err != nil {
			writeAdmissionError(request, err)
			return
		}
		request.Response.Header.Set(acptransport.HeaderConnectionID, admission.ConnectionID())
		defer func() {
			if recovered := recover(); recovered != nil {
				admission.Abort()
				request.Response.Header.Del(acptransport.HeaderConnectionID)
				panic(recovered)
			}
		}()
		err = resolved.upgrader.Upgrade(request, func(conn *hertzws.Conn) {
			admission.Serve(wsconn.WrapHertz(conn))
		})
		if err != nil {
			admission.Abort()
			request.Response.Header.Del(acptransport.HeaderConnectionID)
			acplog.CtxWarn(ctx, "Hertz proxy websocket upgrade failed: %v", err)
		}
	}
}

func hertzHeaderValues(request *app.RequestContext, name string) []string {
	values := request.Request.Header.PeekAll(name)
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}

func writeAdmissionError(request *app.RequestContext, err error) {
	switch {
	case errors.Is(err, acpproxy.ErrClosed), errors.Is(err, acpproxy.ErrTooManyConnections):
		request.String(http.StatusServiceUnavailable, err.Error())
	default:
		request.String(http.StatusInternalServerError, "proxy admission failed")
	}
}
