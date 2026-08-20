package server_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	ginframework "github.com/gin-gonic/gin"
	gorillawebsocket "github.com/gorilla/websocket"
	hertzwebsocket "github.com/hertz-contrib/websocket"

	acp "github.com/eino-contrib/acp"
	acpserver "github.com/eino-contrib/acp/server"
	acpgin "github.com/eino-contrib/acp/server/gin"
	acphertz "github.com/eino-contrib/acp/server/hertz"
)

func newReviewServer(t *testing.T) *acpserver.ACPServer {
	t.Helper()
	core, err := acpserver.NewACPServer(func(context.Context) acp.Agent {
		return httpContractInitializeAgent{}
	})
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	t.Cleanup(func() {
		_ = core.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := core.Shutdown(ctx); err != nil {
			t.Errorf("shutdown ACP server: %v", err)
		}
	})
	return core
}

// TestWebSocketOriginPolicyDefaultsAndExplicitOverride contrasts the secure
// zero-value upgrader with an explicit cross-origin override.
func TestWebSocketOriginPolicyDefaultsAndExplicitOverride(t *testing.T) {
	tests := []struct {
		name        string
		start       func(*testing.T, bool) string
		allowOrigin bool
		wantStatus  int
	}{
		{name: "Hertz/default", start: startReviewHertzOriginHost, wantStatus: http.StatusForbidden},
		{name: "Hertz/explicit CheckOrigin true", start: startReviewHertzOriginHost, allowOrigin: true, wantStatus: http.StatusSwitchingProtocols},
		{name: "Gin/default", start: startReviewGinOriginHost, wantStatus: http.StatusForbidden},
		{name: "Gin/explicit CheckOrigin true", start: startReviewGinOriginHost, allowOrigin: true, wantStatus: http.StatusSwitchingProtocols},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wsURL := tt.start(t, tt.allowOrigin)
			headers := http.Header{"Origin": {"https://evil.example"}}
			conn, response, err := gorillawebsocket.DefaultDialer.Dial(wsURL, headers)
			if conn != nil {
				defer conn.Close()
			}
			if response != nil {
				defer response.Body.Close()
			}
			if tt.wantStatus == http.StatusSwitchingProtocols {
				if err != nil {
					t.Fatalf("evil-Origin handshake rejected: %v", err)
				}
				if response == nil || response.StatusCode != tt.wantStatus {
					t.Fatalf("handshake status = %v, want 101", reviewResponseStatus(response))
				}
				// Ensure the framework's post-upgrade callback has entered the core
				// before cleanup races server shutdown with the successful handshake.
				initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
				if err := conn.WriteMessage(gorillawebsocket.TextMessage, initialize); err != nil {
					t.Fatalf("write initialize after successful handshake: %v", err)
				}
				if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatalf("set initialize read deadline: %v", err)
				}
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Fatalf("read initialize response: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("evil-Origin handshake unexpectedly succeeded")
			}
			if response == nil || response.StatusCode != tt.wantStatus {
				t.Fatalf("handshake status = %v, want %d (err=%v)", reviewResponseStatus(response), tt.wantStatus, err)
			}
		})
	}
}

func reviewResponseStatus(response *http.Response) any {
	if response == nil {
		return nil
	}
	return response.StatusCode
}

func startReviewHertzOriginHost(t *testing.T, allowOrigin bool) string {
	t.Helper()
	core := newReviewServer(t)
	upgrader := hertzwebsocket.HertzUpgrader{}
	if allowOrigin {
		upgrader.CheckOrigin = func(*app.RequestContext) bool { return true }
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Hertz: %v", err)
	}
	srv := hertzserver.New(hertzserver.WithListener(listener), hertzserver.WithTransport(standard.NewTransporter))
	srv.NoHijackConnPool = true
	srv.Any(acpserver.DefaultEndpoint, acphertz.New(core, acphertz.WithUpgrader(upgrader)))
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run() }()
	t.Cleanup(func() {
		_ = core.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Error("Hertz origin host did not stop")
		}
	})
	return "ws://" + listener.Addr().String() + acpserver.DefaultEndpoint
}

func startReviewGinOriginHost(t *testing.T, allowOrigin bool) string {
	t.Helper()
	core := newReviewServer(t)
	upgrader := gorillawebsocket.Upgrader{}
	if allowOrigin {
		upgrader.CheckOrigin = func(*http.Request) bool { return true }
	}
	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Any(acpserver.DefaultEndpoint, acpgin.New(core, acpgin.WithUpgrader(upgrader)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Gin: %v", err)
	}
	srv := &http.Server{Handler: router}
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = core.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Error("Gin origin host did not stop")
		}
	})
	return "ws://" + listener.Addr().String() + acpserver.DefaultEndpoint
}

func TestDocumentationDoesNotDisableOriginProtection(t *testing.T) {
	for _, path := range []string{"../README.md", "../README.zh-CN.md"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, unsafe := range []string{
			"CheckOrigin: func(ctx *app.RequestContext) bool { return true }",
			"CheckOrigin: func(r *http.Request) bool { return true }",
		} {
			if strings.Contains(string(contents), unsafe) {
				t.Errorf("%s contains an unconditional WebSocket origin bypass: %s", path, unsafe)
			}
		}
	}
}

// TestMakeRunTargetsOnlyCleanUpOwnedProcesses is static by design: it verifies
// the destructive boundary without executing a target or sending a signal.
func TestMakeRunTargetsOnlyCleanUpOwnedProcesses(t *testing.T) {
	contents, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, target := range []string{"run-http", "run-ws", "run-proxy"} {
		block := makeTargetBlock(t, string(contents), target)
		if strings.Contains(block, "lsof -t -i") || strings.Contains(block, "kill -9") {
			t.Errorf("%s may kill an unrelated process:\n%s", target, block)
		}
		if !strings.Contains(block, "$$!") || !strings.Contains(block, "trap cleanup") {
			t.Errorf("%s does not track and clean up its own child PID:\n%s", target, block)
		}
	}
}

func makeTargetBlock(t *testing.T, contents, target string) string {
	t.Helper()
	start := strings.Index(contents, "\n"+target+":")
	if start < 0 {
		t.Fatalf("Makefile does not contain %s", target)
	}
	block := contents[start+1:]
	if end := strings.Index(block, "\n\n"); end >= 0 {
		block = block[:end]
	}
	return block
}
