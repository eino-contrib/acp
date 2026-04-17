package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	acptransport "github.com/eino-contrib/acp/transport"
)

type testLogger struct{}

func (testLogger) Debug(string, ...interface{})                      {}
func (testLogger) Info(string, ...interface{})                       {}
func (testLogger) Warn(string, ...interface{})                       {}
func (testLogger) Error(string, ...interface{})                      {}
func (testLogger) CtxDebug(context.Context, string, ...interface{})  {}
func (testLogger) CtxInfo(context.Context, string, ...interface{})   {}
func (testLogger) CtxWarn(context.Context, string, ...interface{})   {}
func (testLogger) CtxError(context.Context, string, ...interface{})  {}

func TestClientTransportWithClientLogger(t *testing.T) {
	t.Parallel()

	logger := testLogger{}
	client := NewClientTransport("http://example.invalid", WithClientLogger(logger))
	defer client.Close()

	if got := acplog.OrDefault(client.logger); got != logger {
		t.Fatalf("logger = %#v, want %#v", got, logger)
	}
}

func TestClientTransportPersistsCookies(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch call {
		case 0:
			if got := r.Header.Get(acptransport.HeaderProtocolVersion); got != "" {
				http.Error(w, "unexpected protocol version header on initialize", http.StatusBadRequest)
				return
			}
			call++
			http.SetCookie(w, &http.Cookie{Name: "acp", Value: "sticky", Path: "/"})
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set(acptransport.HeaderConnectionID, "conn-1")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1}}\n\n")
		case 1:
			if !strings.Contains(r.Header.Get("Cookie"), "acp=sticky") {
				http.Error(w, "missing sticky cookie", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get(acptransport.HeaderProtocolVersion); got != "1" {
				http.Error(w, "missing sticky protocol version header", http.StatusBadRequest)
				return
			}
			call++
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set(acptransport.HeaderSessionID, "sess-1")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"sess-1\"}}\n\n")
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := NewClientTransport(server.URL)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)); err != nil {
		t.Fatalf("initialize write: %v", err)
	}
	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("initialize read: %v", err)
	}

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`)); err != nil {
		t.Fatalf("session/new write: %v", err)
	}
	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("session/new read: %v", err)
	}
}

func TestClientTransportCloseDeletesRemoteConnection(t *testing.T) {
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method
		switch r.Method {
		case http.MethodPost:
			if got := r.Header.Get(acptransport.HeaderProtocolVersion); got != "" {
				http.Error(w, "unexpected protocol version header on initialize", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set(acptransport.HeaderConnectionID, "conn-close")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1}}\n\n")
		case http.MethodDelete:
			if got := r.Header.Get(acptransport.HeaderConnectionID); got != "conn-close" {
				http.Error(w, "missing connection id", http.StatusBadRequest)
				return
			}
			if got := r.Header.Get(acptransport.HeaderProtocolVersion); got != "1" {
				http.Error(w, "missing delete protocol version header", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := NewClientTransport(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)); err != nil {
		t.Fatalf("initialize write: %v", err)
	}
	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("initialize read: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close transport: %v", err)
	}

	if got := <-requests; got != http.MethodPost {
		t.Fatalf("first method = %s, want POST", got)
	}
	if got := <-requests; got != http.MethodDelete {
		t.Fatalf("second method = %s, want DELETE", got)
	}
}

func TestClientTransportClosePropagatesDeleteFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set(acptransport.HeaderConnectionID, "conn-close")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1}}\n\n")
		case http.MethodDelete:
			http.Error(w, "delete failed", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := NewClientTransport(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)); err != nil {
		t.Fatalf("initialize write: %v", err)
	}
	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("initialize read: %v", err)
	}

	err := client.Close()
	if err == nil {
		t.Fatal("expected close to report DELETE failure")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("close error = %v, want HTTP 500", err)
	}
}

func TestClientTransportSessionNewDoesNotReuseLatestSessionHeader(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(acptransport.HeaderSessionID, "sess-new")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"sess-new\"}}\n\n")
	}))
	defer server.Close()

	client := NewClientTransport(server.URL)
	client.state.SetConnectionID("conn-1")
	client.state.SetProtocolVersion("1")
	client.state.SetSessionID("sess-old")
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`)); err != nil {
		t.Fatalf("session/new write: %v", err)
	}
	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("session/new read: %v", err)
	}

	req := <-requests
	if got := req.Header.Get(acptransport.HeaderSessionID); got != "" {
		t.Fatalf("session/new header %s = %q, want empty", acptransport.HeaderSessionID, got)
	}
}

func TestClientTransportLoadSessionUsesExplicitTargetSessionHeader(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(acptransport.HeaderSessionID, "sess-target")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"sessionId\":\"sess-target\"}}\n\n")
	}))
	defer server.Close()

	client := NewClientTransport(server.URL)
	client.state.SetConnectionID("conn-1")
	client.state.SetProtocolVersion("1")
	client.state.SetSessionID("sess-old")
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"sess-target","cwd":"/tmp","mcpServers":[]}}`)); err != nil {
		t.Fatalf("session/load write: %v", err)
	}
	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("session/load read: %v", err)
	}

	req := <-requests
	if got := req.Header.Get(acptransport.HeaderSessionID); got != "sess-target" {
		t.Fatalf("session/load header %s = %q, want %q", acptransport.HeaderSessionID, got, "sess-target")
	}
}

type scriptedReadStep struct {
	data string
	err  error
}

type scriptedReadCloser struct {
	steps []scriptedReadStep
}

func (r *scriptedReadCloser) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	if step.data != "" {
		n := copy(p, step.data)
		if n < len(step.data) {
			r.steps = append([]scriptedReadStep{{data: step.data[n:], err: step.err}}, r.steps...)
			return n, nil
		}
		if step.err != nil {
			r.steps = append([]scriptedReadStep{{err: step.err}}, r.steps...)
		}
		return n, nil
	}
	if step.err != nil {
		return 0, step.err
	}
	return 0, io.EOF
}

func (*scriptedReadCloser) Close() error { return nil }

func TestClientTransportRequestStreamEOFEnqueuesSyntheticError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"sess-1\"}}\n\n")
	}))
	defer server.Close()

	client := NewClientTransport(server.URL)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WriteMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"prompt","params":{"sessionId":"sess-1"}}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	first, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read first message: %v", err)
	}
	var notice struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(first, &notice); err != nil {
		t.Fatalf("unmarshal first message: %v", err)
	}
	if notice.Method != "session/update" {
		t.Fatalf("first method = %q, want session/update", notice.Method)
	}

	second, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read synthetic error: %v", err)
	}
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(second, &response); err != nil {
		t.Fatalf("unmarshal synthetic error: %v", err)
	}
	if response.Error == nil {
		t.Fatalf("expected synthetic error response, got %s", string(second))
	}
	if got := jsonrpc.RawIDToKey(&response.ID); got != "n:7" {
		t.Fatalf("response id = %s, want n:7", got)
	}
	if !strings.Contains(response.Error.Message, "final response") {
		t.Fatalf("error message = %q, want mention of final response", response.Error.Message)
	}
}

func TestClientTransportRequestStreamAbortEnqueuesSyntheticError(t *testing.T) {
	client := NewClientTransport("http://example.invalid")
	defer client.Close()

	rawID := json.RawMessage(`7`)
	body := &scriptedReadCloser{steps: []scriptedReadStep{
		{data: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"sess-1\"}}\n\n"},
		{err: io.ErrUnexpectedEOF},
	}}

	client.consumeRequestSSEStream(&rawID, body)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := client.ReadMessage(ctx); err != nil {
		t.Fatalf("read first message: %v", err)
	}

	second, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read synthetic error: %v", err)
	}

	var response struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(second, &response); err != nil {
		t.Fatalf("unmarshal synthetic error: %v", err)
	}
	if response.Error == nil {
		t.Fatalf("expected synthetic error response, got %s", string(second))
	}
	if response.Error.Code != -32603 {
		t.Fatalf("error code = %d, want -32603", response.Error.Code)
	}
	if got := jsonrpc.RawIDToKey(&response.ID); got != "n:7" {
		t.Fatalf("response id = %s, want n:7", got)
	}
}

func TestClientTransportPartialSSEFinalResponseDoesNotEnqueueSyntheticError(t *testing.T) {
	client := NewClientTransport("http://example.invalid")
	defer client.Close()

	rawID := json.RawMessage(`7`)
	body := &scriptedReadCloser{steps: []scriptedReadStep{
		{data: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n"},
	}}

	client.consumeRequestSSEStream(&rawID, body)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read final response: %v", err)
	}
	var response struct {
		ID     json.RawMessage `json:"id"`
		Result map[string]bool `json:"result"`
	}
	if err := json.Unmarshal(msg, &response); err != nil {
		t.Fatalf("unmarshal final response: %v", err)
	}
	if got := jsonrpc.RawIDToKey(&response.ID); got != "n:7" {
		t.Fatalf("response id = %s, want n:7", got)
	}
	if !response.Result["ok"] {
		t.Fatalf("unexpected response payload: %s", string(msg))
	}

	shortCtx, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := client.ReadMessage(shortCtx); err == nil {
		t.Fatal("expected no synthetic follow-up error after complete partial SSE response")
	}
}

func TestNormalizeACPBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		baseURL      string
		endpointPath string
		want         string
	}{
		{
			name:         "default endpoint on origin",
			baseURL:      "http://localhost:8080",
			endpointPath: "/acp",
			want:         "http://localhost:8080/acp",
		},
		{
			name:         "custom endpoint replaces trailing slash",
			baseURL:      "http://localhost:8080/",
			endpointPath: "/rpc",
			want:         "http://localhost:8080/rpc",
		},
		{
			name:         "base url path is ignored",
			baseURL:      "http://localhost:8080/custom",
			endpointPath: "/acp",
			want:         "http://localhost:8080/acp",
		},
		{
			name:         "base url query is preserved",
			baseURL:      "http://localhost:8080/custom?debug=1",
			endpointPath: "/acp",
			want:         "http://localhost:8080/acp?debug=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeACPBaseURL(tt.baseURL, tt.endpointPath); got != tt.want {
				t.Fatalf("normalizeACPBaseURL(%q, %q) = %q, want %q", tt.baseURL, tt.endpointPath, got, tt.want)
			}
		})
	}
}

func TestNewClientTransportBuildsFinalURLFromOriginAndEndpointPath(t *testing.T) {
	t.Parallel()

	client := NewClientTransport("http://localhost:8080/custom", WithClientEndpointPath("/rpc"))
	defer client.Close()

	if got := client.baseURL; got != "http://localhost:8080/rpc" {
		t.Fatalf("client.baseURL = %q, want %q", got, "http://localhost:8080/rpc")
	}
}
