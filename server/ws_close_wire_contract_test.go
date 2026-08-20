package server_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	ginframework "github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"

	acp "github.com/eino-contrib/acp"
	acpserver "github.com/eino-contrib/acp/server"
	acpgin "github.com/eino-contrib/acp/server/gin"
	acphertz "github.com/eino-contrib/acp/server/hertz"
)

func TestServerCloseSendsNormalCloseAcrossFrameworks(t *testing.T) {
	tests := []struct {
		name  string
		start func(*testing.T, *acpserver.ACPServer) string
	}{
		{name: "Hertz", start: startCloseWireHertzServer},
		{name: "Gin", start: startCloseWireGinServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, err := acpserver.NewACPServer(func(context.Context) acp.Agent {
				return httpContractInitializeAgent{}
			})
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			wsURL := tt.start(t, core)
			conn, response, err := gorillaws.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				closeAdapterResponse(response)
				t.Fatalf("dial WebSocket: %v", err)
			}
			defer conn.Close()
			closeAdapterResponse(response)

			initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
			if err := conn.WriteMessage(gorillaws.TextMessage, initialize); err != nil {
				t.Fatalf("write initialize: %v", err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("set initialize deadline: %v", err)
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatalf("read initialize response: %v", err)
			}

			if err := core.Close(); err != nil {
				t.Fatalf("core Close: %v", err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("set close deadline: %v", err)
			}
			_, _, err = conn.ReadMessage()
			var closeErr *gorillaws.CloseError
			if !errors.As(err, &closeErr) {
				t.Fatalf("close read error = %T %v, want CloseError(1000)", err, err)
			}
			if closeErr.Code != gorillaws.CloseNormalClosure {
				t.Fatalf("close code = %d (%q), want 1000", closeErr.Code, closeErr.Text)
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := core.Shutdown(shutdownCtx); err != nil {
				t.Fatalf("core Shutdown: %v", err)
			}
		})
	}
}

func startCloseWireHertzServer(t *testing.T, core *acpserver.ACPServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Hertz: %v", err)
	}
	host := hertzserver.New(hertzserver.WithListener(listener), hertzserver.WithTransport(standard.NewTransporter))
	host.NoHijackConnPool = true
	host.Any(acpserver.DefaultEndpoint, acphertz.New(core))
	runErr := make(chan error, 1)
	go func() { runErr <- host.Run() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "closed network connection") {
				t.Errorf("Hertz Run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Hertz host did not stop")
		}
	})
	return "ws://" + listener.Addr().String() + acpserver.DefaultEndpoint
}

func startCloseWireGinServer(t *testing.T, core *acpserver.ACPServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Gin: %v", err)
	}
	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Any(acpserver.DefaultEndpoint, acpgin.New(core))
	host := &http.Server{Handler: router}
	runErr := make(chan error, 1)
	go func() { runErr <- host.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Gin Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Gin host did not stop")
		}
	})
	return "ws://" + listener.Addr().String() + acpserver.DefaultEndpoint
}

func closeAdapterResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
