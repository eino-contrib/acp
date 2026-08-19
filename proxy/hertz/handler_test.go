package hertz

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	hertzws "github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/proxy"
	"github.com/eino-contrib/acp/stream"
	acptransport "github.com/eino-contrib/acp/transport"
)

type failingFactory struct{}

func (failingFactory) NewStreamer(context.Context, map[string]string) (stream.Streamer, error) {
	return nil, errors.New("factory must not run")
}

func TestUpgraderPanicReleasesAdmissionAndRepanics(t *testing.T) {
	tests := []struct {
		name     string
		upgrader func(any) hertzws.HertzUpgrader
	}{
		{
			name: "CheckOrigin",
			upgrader: func(value any) hertzws.HertzUpgrader {
				return hertzws.HertzUpgrader{CheckOrigin: func(*app.RequestContext) bool { panic(value) }}
			},
		},
		{
			name: "Error",
			upgrader: func(value any) hertzws.HertzUpgrader {
				return hertzws.HertzUpgrader{
					CheckOrigin: func(*app.RequestContext) bool { return false },
					Error:       func(*app.RequestContext, int, error) { panic(value) },
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, err := proxy.NewACPProxy(failingFactory{}, proxy.WithMaxConcurrentConnections(1))
			if err != nil {
				t.Fatalf("NewACPProxy: %v", err)
			}
			panicValue := &struct{ label string }{label: tt.name}
			request := validWebSocketRequestContext()

			var recovered any
			func() {
				defer func() { recovered = recover() }()
				New(core, WithUpgrader(tt.upgrader(panicValue)))(context.Background(), request)
			}()
			if recovered != panicValue {
				t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
			}
			if got := string(request.Response.Header.Peek(acptransport.HeaderConnectionID)); got != "" {
				t.Fatalf("panic cleanup left provisional connection ID %q", got)
			}

			admission, err := core.Admit(context.Background(), nil)
			if err != nil {
				t.Fatalf("Admit after upgrader panic: %v", err)
			}
			admission.Abort()
			if err := core.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
		})
	}
}

func validWebSocketRequestContext() *app.RequestContext {
	request := &app.RequestContext{}
	request.Request.Header.SetMethod(http.MethodGet)
	request.Request.Header.Set("Connection", "Upgrade")
	request.Request.Header.Set("Upgrade", "websocket")
	request.Request.Header.Set("Sec-WebSocket-Version", "13")
	request.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Request.Header.Set("Origin", "https://panic.example")
	return request
}
