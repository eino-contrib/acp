package hertz

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hertzclient "github.com/cloudwego/hertz/pkg/app/client"
	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	hertzwebsocket "github.com/hertz-contrib/websocket"

	acp "github.com/eino-contrib/acp"
	acpserver "github.com/eino-contrib/acp/server"
	acptransport "github.com/eino-contrib/acp/transport"
)

func TestNewNilCoreReturnsServiceUnavailable(t *testing.T) {
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodGet)
	New(nil)(context.Background(), c)
	if got := c.Response.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestOrdinaryRequestUsesSharedHTTPProtocol(t *testing.T) {
	core, calls := newCountingServer(t)
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodPut)
	New(core)(context.Background(), c)

	if got := c.Response.StatusCode(); got != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", got, http.StatusMethodNotAllowed)
	}
	if got := string(c.Response.Header.Peek("Allow")); got != "GET, POST, DELETE" {
		t.Fatalf("Allow = %q, want %q", got, "GET, POST, DELETE")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestHandlerAbortsLaterHertzHandlers(t *testing.T) {
	core, _ := newCountingServer(t)
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodPut)
	New(core)(context.Background(), c)
	if !c.IsAborted() {
		t.Fatal("ACP Hertz handler did not abort the remaining handler chain")
	}
}

func TestMalformedWebSocketAttemptDoesNotCreateAgent(t *testing.T) {
	core, calls := newCountingServer(t)
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodGet)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Sec-WebSocket-Version", "13")

	New(core)(context.Background(), c)
	if got := c.Response.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if got := string(c.Response.Header.Peek(acptransport.HeaderConnectionID)); got != "" {
		t.Fatalf("failed validation exposed connection ID %q", got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestUpgraderFailureDoesNotCreateAgent(t *testing.T) {
	core, calls := newCountingServer(t)
	upgrader := hertzwebsocket.HertzUpgrader{
		CheckOrigin: func(*app.RequestContext) bool { return false },
	}
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodGet)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Sec-WebSocket-Version", "13")
	c.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	c.Request.Header.Set("Origin", "https://rejected.example")

	New(core, WithUpgrader(upgrader))(context.Background(), c)

	if got := c.Response.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
	if got := string(c.Response.Header.Peek(acptransport.HeaderConnectionID)); got != "" {
		t.Fatalf("failed upgrade exposed connection ID %q", got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := core.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown after failed upgrade: %v", err)
	}
}

func TestUpgraderCheckOriginPanicReleasesAdmissionAndRepanics(t *testing.T) {
	core, _ := newCountingServer(t)
	panicValue := &struct{ label string }{label: "hertz server origin panic"}
	upgrader := hertzwebsocket.HertzUpgrader{
		CheckOrigin: func(*app.RequestContext) bool { panic(panicValue) },
	}
	c := validHertzWebSocketRequestContext()
	c.Request.Header.Set("Origin", "https://panic.example")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		New(core, WithUpgrader(upgrader))(context.Background(), c)
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
	}
	if got := string(c.Response.Header.Peek(acptransport.HeaderConnectionID)); got != "" {
		t.Fatalf("panic cleanup left provisional connection ID %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := core.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after upgrader panic: %v", err)
	}
}

func TestUpgraderErrorHookPanicReleasesAdmissionAndRepanics(t *testing.T) {
	core, _ := newCountingServer(t)
	panicValue := &struct{ label string }{label: "hertz server error panic"}
	upgrader := hertzwebsocket.HertzUpgrader{
		CheckOrigin: func(*app.RequestContext) bool { return false },
		Error:       func(*app.RequestContext, int, error) { panic(panicValue) },
	}
	c := validHertzWebSocketRequestContext()
	c.Request.Header.Set("Origin", "https://rejected.example")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		New(core, WithUpgrader(upgrader))(context.Background(), c)
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
	}
	if got := string(c.Response.Header.Peek(acptransport.HeaderConnectionID)); got != "" {
		t.Fatalf("panic cleanup left provisional connection ID %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := core.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after upgrader error-hook panic: %v", err)
	}
}

func validHertzWebSocketRequestContext() *app.RequestContext {
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodGet)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Sec-WebSocket-Version", "13")
	c.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return c
}

func TestClosingServerRejectsWebSocket(t *testing.T) {
	core, calls := newCountingServer(t)
	if err := core.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodGet)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Sec-WebSocket-Version", "13")
	c.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	New(core)(context.Background(), c)
	if got := c.Response.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestRealHertzWebSocketUpgradeCreatesAgentAndInitializes(t *testing.T) {
	var calls atomic.Int32
	core, err := acpserver.NewACPServer(func(context.Context) acp.Agent {
		calls.Add(1)
		return initializeAgent{}
	})
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}

	addr := randomAddress(t)
	srv := hertzserver.New(hertzserver.WithHostPorts(addr))
	srv.NoHijackConnPool = true
	srv.Any(acpserver.DefaultEndpoint, New(core))
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run() }()
	waitReady(t, "http://"+addr+acpserver.DefaultEndpoint)
	t.Cleanup(func() {
		_ = core.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case runErr := <-errCh:
			if runErr != nil && !errors.Is(runErr, net.ErrClosed) && !strings.Contains(runErr.Error(), "closed network connection") {
				t.Logf("Hertz Run: %v", runErr)
			}
		case <-time.After(time.Second):
		}
	})

	req := protocol.AcquireRequest()
	resp := protocol.AcquireResponse()
	defer protocol.ReleaseRequest(req)
	defer protocol.ReleaseResponse(resp)
	req.SetRequestURI("http://" + addr + acpserver.DefaultEndpoint)
	req.Header.SetMethod(consts.MethodGet)
	upgrader := &hertzwebsocket.ClientUpgrader{}
	upgrader.PrepareRequest(req)
	client, err := hertzclient.NewClient(hertzclient.WithDialer(standard.NewDialer()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Do(context.Background(), req, resp); err != nil {
		t.Fatalf("HTTP upgrade request: %v", err)
	}
	conn, err := upgrader.UpgradeResponse(req, resp)
	if err != nil {
		t.Fatalf("UpgradeResponse: %v (status=%d body=%s)", err, resp.StatusCode(), resp.Body())
	}
	defer conn.Close()
	if got := string(resp.Header.Peek(acptransport.HeaderConnectionID)); got == "" {
		t.Fatal("successful handshake omitted Acp-Connection-Id")
	}

	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if err := conn.WriteMessage(hertzwebsocket.TextMessage, initialize); err != nil {
		t.Fatalf("WriteMessage initialize: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage initialize response: %v", err)
	}
	if messageType != hertzwebsocket.TextMessage {
		t.Fatalf("message type = %d, want TextMessage", messageType)
	}
	var response struct {
		Result struct {
			ProtocolVersion int `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal response %s: %v", payload, err)
	}
	if response.Result.ProtocolVersion != 1 {
		t.Fatalf("initialize response = %s, want protocolVersion 1", payload)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("AgentFactory calls = %d, want 1", got)
	}
}

type initializeAgent struct{ acp.BaseAgent }

func (initializeAgent) Initialize(_ context.Context, req acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: req.ProtocolVersion}, nil
}

func randomAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Hertz server did not become ready: %s", url)
}

func newCountingServer(t *testing.T) (*acpserver.ACPServer, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	core, err := acpserver.NewACPServer(func(context.Context) acp.Agent {
		calls.Add(1)
		return &acp.BaseAgent{}
	})
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	return core, &calls
}
