package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	acp "github.com/eino-contrib/acp"
	acpserver "github.com/eino-contrib/acp/server"
	acpgin "github.com/eino-contrib/acp/server/gin"
	acphertz "github.com/eino-contrib/acp/server/hertz"
	acptransport "github.com/eino-contrib/acp/transport"
	acphttpclient "github.com/eino-contrib/acp/transport/http/client"
	ginframework "github.com/gin-gonic/gin"
)

const (
	httpContractMaxMessageSize              = 512
	httpContractSessionID                   = "http-contract-session"
	httpContractLargeMessageLimit           = 5 * 1024 * 1024
	httpContractAboveHertzDefaultBodySize   = 4*1024*1024 + 1
	httpContractInitialKeepAliveReadTimeout = 2 * time.Second
)

type streamableHTTPAdapter struct {
	name  string
	start func(*testing.T, *acpserver.ACPServer) string
}

func streamableHTTPAdapters() []streamableHTTPAdapter {
	return []streamableHTTPAdapter{
		{name: "Hertz", start: startHertzHTTPAdapter},
		{name: "Gin", start: startGinHTTPAdapter},
	}
}

func TestStreamableHTTPAdapterContract(t *testing.T) {
	for _, adapter := range streamableHTTPAdapters() {
		t.Run(adapter.name, func(t *testing.T) {
			var factoryCalls atomic.Int32
			core, err := acpserver.NewACPServer(
				func(context.Context) acp.Agent {
					factoryCalls.Add(1)
					return httpContractInitializeAgent{}
				},
				acpserver.WithMaxHTTPMessageSize(httpContractMaxMessageSize),
			)
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			t.Cleanup(func() {
				if err := core.Close(); err != nil {
					t.Errorf("close ACP server: %v", err)
				}
			})

			baseURL := adapter.start(t, core)
			runStreamableHTTPContract(t, baseURL, &factoryCalls)
		})
	}
}

func TestStreamableHTTPAdapterDoesNotPreRejectSDKAllowedBody(t *testing.T) {
	for _, adapter := range streamableHTTPAdapters() {
		t.Run(adapter.name, func(t *testing.T) {
			core, err := acpserver.NewACPServer(
				func(context.Context) acp.Agent { return httpContractInitializeAgent{} },
				acpserver.WithMaxHTTPMessageSize(httpContractLargeMessageLimit),
			)
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			t.Cleanup(func() {
				if err := core.Close(); err != nil {
					t.Errorf("close ACP server: %v", err)
				}
			})

			baseURL := adapter.start(t, core)
			assertAboveHertzDefaultBodyReachesACPParser(t, baseURL)
		})
	}
}

type httpContractInitializeAgent struct{ acp.BaseAgent }

func (httpContractInitializeAgent) Initialize(_ context.Context, req acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: req.ProtocolVersion,
		AgentInfo: &acp.Implementation{
			Name:    "http-adapter-contract-agent",
			Version: "1.0.0",
		},
	}, nil
}

func (httpContractInitializeAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionID: acp.SessionID(httpContractSessionID)}, nil
}

func runStreamableHTTPContract(t *testing.T, baseURL string, factoryCalls *atomic.Int32) {
	t.Helper()

	observer := newHTTPContractObserver()
	transport := acphttpclient.NewClientTransport(
		baseURL,
		acphttpclient.WithHTTPClient(&http.Client{
			Transport: observer,
			Timeout:   5 * time.Second,
		}),
	)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = transport.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initialize := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if err := transport.WriteMessage(ctx, initialize); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	message, err := transport.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read initialize response: %v", err)
	}

	var response struct {
		JSONRPC string                 `json:"jsonrpc"`
		ID      json.RawMessage        `json:"id"`
		Result  acp.InitializeResponse `json:"result"`
		Error   json.RawMessage        `json:"error"`
	}
	if err := json.Unmarshal(message, &response); err != nil {
		t.Fatalf("decode initialize response %s: %v", message, err)
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		t.Fatalf("initialize response contains error: %s", response.Error)
	}
	if response.JSONRPC != "2.0" {
		t.Fatalf("initialize jsonrpc = %q, want 2.0", response.JSONRPC)
	}
	if string(response.ID) != "1" {
		t.Fatalf("initialize id = %s, want 1", response.ID)
	}
	if response.Result.ProtocolVersion != acp.ProtocolVersion(acp.CurrentProtocolVersion) {
		t.Fatalf("initialize protocolVersion = %d, want %d", response.Result.ProtocolVersion, acp.CurrentProtocolVersion)
	}
	if response.Result.AgentInfo == nil || response.Result.AgentInfo.Name != "http-adapter-contract-agent" {
		t.Fatalf("initialize agentInfo = %#v, want contract agent", response.Result.AgentInfo)
	}

	connectionID := transport.ConnectionID()
	if connectionID == "" {
		t.Fatal("initialize response omitted Acp-Connection-Id")
	}
	initializeExchange := observer.singleExchange(t, http.MethodPost)
	if initializeExchange.status != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d", initializeExchange.status, http.StatusOK)
	}
	if initializeExchange.contentType != "text/event-stream; charset=utf-8" {
		t.Fatalf("initialize Content-Type = %q, want %q", initializeExchange.contentType, "text/event-stream; charset=utf-8")
	}
	if initializeExchange.responseConnectionID != connectionID {
		t.Fatalf("initialize response connection ID = %q, transport stored %q", initializeExchange.responseConnectionID, connectionID)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("AgentFactory calls = %d, want 1", got)
	}
	if got := observer.exchangeCount(http.MethodDelete); got != 0 {
		t.Fatalf("DELETE calls before Close = %d, want 0", got)
	}
	sessionID := createHTTPContractSession(t, ctx, transport)
	assertInitialSSEKeepAlive(t, baseURL, connectionID, sessionID, transport.ProtocolVersion())

	assertUnknownLengthOversizedRequest(t, baseURL)

	if err := transport.Close(); err != nil {
		t.Fatalf("close client transport: %v", err)
	}
	closed = true
	deleteExchange := observer.singleExchange(t, http.MethodDelete)
	if deleteExchange.status != http.StatusAccepted {
		t.Fatalf("DELETE status = %d, want %d", deleteExchange.status, http.StatusAccepted)
	}
	if deleteExchange.requestConnectionID != connectionID {
		t.Fatalf("DELETE connection ID = %q, want %q", deleteExchange.requestConnectionID, connectionID)
	}
}

func createHTTPContractSession(t *testing.T, ctx context.Context, transport *acphttpclient.ClientTransport) string {
	t.Helper()

	request := json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`)
	if err := transport.WriteMessage(ctx, request); err != nil {
		t.Fatalf("write session/new: %v", err)
	}
	message, err := transport.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read session/new response: %v", err)
	}

	var response struct {
		JSONRPC string                 `json:"jsonrpc"`
		ID      json.RawMessage        `json:"id"`
		Result  acp.NewSessionResponse `json:"result"`
		Error   json.RawMessage        `json:"error"`
	}
	if err := json.Unmarshal(message, &response); err != nil {
		t.Fatalf("decode session/new response %s: %v", message, err)
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		t.Fatalf("session/new response contains error: %s", response.Error)
	}
	if response.JSONRPC != "2.0" || string(response.ID) != "2" {
		t.Fatalf("session/new envelope = jsonrpc %q id %s, want jsonrpc 2.0 id 2", response.JSONRPC, response.ID)
	}
	sessionID := string(response.Result.SessionID)
	if sessionID != httpContractSessionID {
		t.Fatalf("session/new sessionId = %q, want %q", sessionID, httpContractSessionID)
	}
	if got := transport.SessionID(); got != sessionID {
		t.Fatalf("transport session ID = %q, want %q", got, sessionID)
	}
	return sessionID
}

func assertInitialSSEKeepAlive(t *testing.T, baseURL, connectionID, sessionID, protocolVersion string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), httpContractInitialKeepAliveReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+acpserver.DefaultEndpoint, nil)
	if err != nil {
		t.Fatalf("create GET SSE request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	req.Header.Set(acptransport.HeaderSessionID, sessionID)
	if protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}

	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: httpTransport}
	defer httpTransport.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open GET SSE stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET SSE status = %d, want %d: %s", resp.StatusCode, http.StatusOK, body)
	}
	if got, want := resp.Header.Get("Content-Type"), "text/event-stream; charset=utf-8"; got != want {
		t.Fatalf("GET SSE Content-Type = %q, want %q", got, want)
	}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read initial GET SSE keepalive: %v", err)
	}
	if line != ":keep-alive\n" {
		t.Fatalf("initial GET SSE line = %q, want %q", line, ":keep-alive\n")
	}
}

func assertAboveHertzDefaultBodyReachesACPParser(t *testing.T, baseURL string) {
	t.Helper()

	body := strings.NewReader(strings.Repeat("x", httpContractAboveHertzDefaultBodySize))
	req, err := http.NewRequest(http.MethodPost, baseURL+acpserver.DefaultEndpoint, body)
	if err != nil {
		t.Fatalf("create above-host-default request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send above-host-default request: %v", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read above-host-default response: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("above-host-default status = %d, want %d: %s", resp.StatusCode, http.StatusBadRequest, responseBody)
	}
	if got := strings.TrimSpace(string(responseBody)); got != "invalid JSON" {
		t.Fatalf("above-host-default response = %q, want ACP parser error %q", got, "invalid JSON")
	}
}

func assertUnknownLengthOversizedRequest(t *testing.T, baseURL string) {
	t.Helper()

	body := io.NopCloser(strings.NewReader(strings.Repeat("x", httpContractMaxMessageSize+1)))
	req, err := http.NewRequest(http.MethodPost, baseURL+acpserver.DefaultEndpoint, body)
	if err != nil {
		t.Fatalf("create oversized request: %v", err)
	}
	// An explicit negative length forces net/http to frame the request as
	// chunked, exercising the streaming limit rather than Content-Length
	// validation at the protocol boundary.
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send unknown-length oversized request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("unknown-length oversized status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

type observedHTTPExchange struct {
	method               string
	status               int
	contentType          string
	requestConnectionID  string
	responseConnectionID string
}

type httpContractObserver struct {
	base *http.Transport

	mu        sync.Mutex
	exchanges []observedHTTPExchange
}

func newHTTPContractObserver() *httpContractObserver {
	return &httpContractObserver{base: http.DefaultTransport.(*http.Transport).Clone()}
}

func (o *httpContractObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.exchanges = append(o.exchanges, observedHTTPExchange{
		method:               req.Method,
		status:               resp.StatusCode,
		contentType:          resp.Header.Get("Content-Type"),
		requestConnectionID:  req.Header.Get(acptransport.HeaderConnectionID),
		responseConnectionID: resp.Header.Get(acptransport.HeaderConnectionID),
	})
	o.mu.Unlock()
	return resp, nil
}

func (o *httpContractObserver) CloseIdleConnections() {
	o.base.CloseIdleConnections()
}

func (o *httpContractObserver) exchangeCount(method string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, exchange := range o.exchanges {
		if exchange.method == method {
			count++
		}
	}
	return count
}

func (o *httpContractObserver) singleExchange(t *testing.T, method string) observedHTTPExchange {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var matches []observedHTTPExchange
	for _, exchange := range o.exchanges {
		if exchange.method == method {
			matches = append(matches, exchange)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("observed %d %s exchanges, want 1: %#v", len(matches), method, o.exchanges)
	}
	return matches[0]
}

func startHertzHTTPAdapter(t *testing.T, core *acpserver.ACPServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for Hertz: %v", err)
	}

	srv := hertzserver.New(
		hertzserver.WithListener(listener),
		hertzserver.WithTransport(standard.NewTransporter),
		hertzserver.WithStreamBody(true),
	)
	// Keep Hertz's hijacked-connection pool disabled as required for hosts
	// that share the endpoint between ordinary HTTP and WebSocket traffic.
	srv.NoHijackConnPool = true
	srv.Any(acpserver.DefaultEndpoint, acphertz.New(core))
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run() }()

	baseURL := "http://" + listener.Addr().String()
	waitForHTTPServer(t, baseURL+acpserver.DefaultEndpoint, runErr)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("shutdown Hertz server: %v", err)
		}
		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "closed network connection") {
				t.Errorf("Hertz Run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Hertz Run did not return after shutdown")
		}
	})
	return baseURL
}

func startGinHTTPAdapter(t *testing.T, core *acpserver.ACPServer) string {
	t.Helper()
	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Any(acpserver.DefaultEndpoint, acpgin.New(core))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitForHTTPServer(t *testing.T, endpoint string, runErr <-chan error) {
	t.Helper()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-runErr:
			t.Fatalf("HTTP server exited before becoming ready: %v", err)
		default:
		}

		resp, err := client.Get(endpoint)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("HTTP server did not become ready: %s", endpoint)
}
