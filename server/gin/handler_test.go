package gin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
	acpserver "github.com/eino-contrib/acp/server"
	acptransport "github.com/eino-contrib/acp/transport"
	ginframework "github.com/gin-gonic/gin"
	gorillawebsocket "github.com/gorilla/websocket"
)

func TestNewNilCoreReturnsServiceUnavailable(t *testing.T) {
	recorder := serveRequest(t, New(nil), http.MethodGet, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Body.String(); got != "ACP server unavailable" {
		t.Fatalf("body = %q, want %q", got, "ACP server unavailable")
	}
}

func TestOrdinaryRequestUsesHTTPProtocol(t *testing.T) {
	core, calls := newCountingServer(t)
	recorder := serveRequest(t, New(core), http.MethodPut, nil)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
	if got := recorder.Header().Get("Allow"); got != "GET, POST, DELETE" {
		t.Fatalf("Allow = %q, want %q", got, "GET, POST, DELETE")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestMalformedWebSocketAttemptDoesNotCreateAgent(t *testing.T) {
	core, calls := newCountingServer(t)
	headers := http.Header{
		"Connection":            {"Upgrade"},
		"Upgrade":               {"websocket"},
		"Sec-Websocket-Version": {"13"},
	}
	recorder := serveRequest(t, New(core), http.MethodGet, headers)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !strings.Contains(recorder.Body.String(), "Sec-WebSocket-Key") {
		t.Fatalf("body = %q, want stable key validation error", recorder.Body.String())
	}
	if got := recorder.Header().Get(acptransport.HeaderConnectionID); got != "" {
		t.Fatalf("failed validation exposed connection ID %q", got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestUpgraderFailureDoesNotCreateAgent(t *testing.T) {
	core, calls := newCountingServer(t)
	upgrader := gorillawebsocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return false },
	}
	headers := validWebSocketHeaders()
	headers.Set("Origin", "https://rejected.example")
	recorder := serveRequest(t, New(core, WithUpgrader(upgrader)), http.MethodGet, headers)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	if got := recorder.Header().Get(acptransport.HeaderConnectionID); got != "" {
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
	panicValue := &struct{ label string }{label: "gin server origin panic"}
	upgrader := gorillawebsocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { panic(panicValue) },
	}
	recorder := httptest.NewRecorder()
	c, _ := ginframework.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header = validWebSocketHeaders()
	c.Request.Header.Set("Origin", "https://panic.example")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		New(core, WithUpgrader(upgrader))(c)
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := core.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after upgrader panic: %v", err)
	}
}

func TestUpgraderErrorHookPanicReleasesAdmissionAndRepanics(t *testing.T) {
	core, _ := newCountingServer(t)
	panicValue := &struct{ label string }{label: "gin server error panic"}
	upgrader := gorillawebsocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return false },
		Error: func(http.ResponseWriter, *http.Request, int, error) {
			panic(panicValue)
		},
	}
	recorder := httptest.NewRecorder()
	c, _ := ginframework.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header = validWebSocketHeaders()
	c.Request.Header.Set("Origin", "https://rejected.example")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		New(core, WithUpgrader(upgrader))(c)
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := core.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after upgrader error-hook panic: %v", err)
	}
}

func TestClosingServerRejectsWebSocketWithServiceUnavailable(t *testing.T) {
	core, calls := newCountingServer(t)
	if err := core.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recorder := serveRequest(t, New(core), http.MethodGet, validWebSocketHeaders())
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Body.String(); got != "ACP server is shutting down" {
		t.Fatalf("body = %q, want shutdown error", got)
	}
	if got := recorder.Header().Get(acptransport.HeaderConnectionID); got != "" {
		t.Fatalf("rejected handshake exposed connection ID %q", got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestWebSocketUpgradeUsesCustomUpgraderAndConnectionID(t *testing.T) {
	core, calls := newCountingServer(t)
	upgrader := gorillawebsocket.Upgrader{
		Subprotocols: []string{"acp.test"},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	router := newRouter(New(core, WithUpgrader(upgrader)))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	headers := http.Header{
		"Origin":                 {"https://cross-origin.example"},
		"Sec-Websocket-Protocol": {"acp.test"},
	}
	conn, response, err := gorillawebsocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		headers,
	)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if got := response.Header.Get(acptransport.HeaderConnectionID); got == "" {
		t.Fatal("successful handshake omitted Acp-Connection-Id")
	}
	if got := conn.Subprotocol(); got != "acp.test" {
		t.Fatalf("subprotocol = %q, want %q", got, "acp.test")
	}

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("AgentFactory calls = %d, want 1 after successful upgrade", got)
	}
}

func TestAbortedMiddlewareSkipsACPHandler(t *testing.T) {
	core, calls := newCountingServer(t)
	router := ginframework.New()
	router.Any("/", func(c *ginframework.Context) {
		c.AbortWithStatus(http.StatusUnauthorized)
	}, New(core))

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/", nil)
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestACPHandlerStopsLaterMiddleware(t *testing.T) {
	core, _ := newCountingServer(t)
	var laterCalls atomic.Int32
	router := ginframework.New()
	router.Any("/", New(core), func(c *ginframework.Context) {
		laterCalls.Add(1)
		c.Status(http.StatusTeapot)
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/", nil)
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
	if got := laterCalls.Load(); got != 0 {
		t.Fatalf("later middleware calls = %d, want 0", got)
	}
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

func serveRequest(t *testing.T, handler ginframework.HandlerFunc, method string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	router := newRouter(handler)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/", nil)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	router.ServeHTTP(recorder, req)
	return recorder
}

func newRouter(handler ginframework.HandlerFunc) *ginframework.Engine {
	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Any("/", handler)
	return router
}

func validWebSocketHeaders() http.Header {
	return http.Header{
		"Connection":            {"Upgrade"},
		"Upgrade":               {"websocket"},
		"Sec-Websocket-Version": {"13"},
		"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
	}
}
