package httpserver

import (
	"context"
	"encoding/json"
	"testing"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	acptransport "github.com/eino-contrib/acp/transport"
)

func TestHandleProtocolPostPassesRequestContextToAgent(t *testing.T) {
	type traceKey struct{}

	var gotTrace any
	var gotSessionID string

	conn := NewProtocolConnection(ProtocolConnectionConfig{
		ConnectionID: "conn-1",
		HTTPConn:     NewConnection(),
		Dispatcher: connspi.Dispatcher{
			Request: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				gotTrace = ctx.Value(traceKey{})
				gotSessionID = connspi.SessionIDFromContext(ctx)
				return map[string]any{"ok": true}, nil
			},
		},
		Done: make(chan struct{}),
	})

	server := ProtocolServer{
		LookupConnection: func(connectionID string) (*ProtocolConnection, bool) {
			if connectionID != "conn-1" {
				return nil, false
			}
			return conn, true
		},
	}

	ctx := newStubHandlerContext()
	ctx.ctx = context.WithValue(context.Background(), traceKey{}, "trace-123")
	ctx.requestHeaders["Content-Type"] = "application/json"
	ctx.requestHeaders["Accept"] = "application/json, text/event-stream"
	ctx.requestHeaders[acptransport.HeaderConnectionID] = "conn-1"
	ctx.requestHeaders[acptransport.HeaderSessionID] = "session-42"
	ctx.body = []byte(`{"jsonrpc":"2.0","id":1,"method":"` + acp.MethodAgentPrompt + `","params":{}}`)

	HandleProtocolPost(ctx, server)

	if gotTrace != "trace-123" {
		t.Fatalf("request context value = %#v, want %q", gotTrace, "trace-123")
	}
	if gotSessionID != "session-42" {
		t.Fatalf("session id from context = %q, want %q", gotSessionID, "session-42")
	}
	if len(ctx.sseEvents) != 1 {
		t.Fatalf("SSE event count = %d, want 1", len(ctx.sseEvents))
	}
}

// The HTTP session map owns GET SSE routing, not Agent business-session
// truth. A session-scoped POST with valid headers must still reach the Agent
// when no GET transport session is registered; the Agent decides whether the
// business session exists and returns the JSON-RPC result/error.
func TestHandleProtocolPostDoesNotUseSSESessionMapAsBusinessAuthority(t *testing.T) {
	dispatchCalls := 0
	conn := NewProtocolConnection(ProtocolConnectionConfig{
		ConnectionID: "conn-agent-authority",
		HTTPConn:     NewConnection(),
		Dispatcher: connspi.Dispatcher{
			Request: func(context.Context, string, json.RawMessage) (any, error) {
				dispatchCalls++
				return map[string]any{"agentAccepted": true}, nil
			},
		},
		Done: make(chan struct{}),
	})
	server := ProtocolServer{
		LookupConnection: func(connectionID string) (*ProtocolConnection, bool) {
			return conn, connectionID == conn.ConnectionID()
		},
	}

	ctx := newStubHandlerContext()
	ctx.requestHeaders["Content-Type"] = "application/json"
	ctx.requestHeaders["Accept"] = "application/json, text/event-stream"
	ctx.requestHeaders[acptransport.HeaderConnectionID] = conn.ConnectionID()
	ctx.requestHeaders[acptransport.HeaderSessionID] = "agent-owned-session"
	ctx.body = []byte(`{"jsonrpc":"2.0","id":1,"method":"` + acp.MethodAgentPrompt + `","params":{"sessionId":"agent-owned-session","prompt":[]}}`)

	HandleProtocolPost(ctx, server)

	if dispatchCalls != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatchCalls)
	}
	if ctx.statusCode != 200 || len(ctx.sseEvents) != 1 {
		t.Fatalf("response status/events = %d/%d, want 200/1 (error=%q)", ctx.statusCode, len(ctx.sseEvents), ctx.errMessage)
	}
}

func TestHandleProtocolPostRegistersResumedSession(t *testing.T) {
	httpConn := NewConnection()
	conn := NewProtocolConnection(ProtocolConnectionConfig{
		ConnectionID: "conn-resume-session",
		HTTPConn:     httpConn,
		Dispatcher: connspi.Dispatcher{
			Request: func(context.Context, string, json.RawMessage) (any, error) {
				return acp.ResumeSessionResponse{}, nil
			},
		},
		Done: make(chan struct{}),
	})
	server := ProtocolServer{
		LookupConnection: func(connectionID string) (*ProtocolConnection, bool) {
			return conn, connectionID == conn.ConnectionID()
		},
	}

	ctx := newStubHandlerContext()
	ctx.requestHeaders["Content-Type"] = "application/json"
	ctx.requestHeaders["Accept"] = "application/json, text/event-stream"
	ctx.requestHeaders[acptransport.HeaderConnectionID] = conn.ConnectionID()
	ctx.requestHeaders[acptransport.HeaderSessionID] = "session-to-resume"
	ctx.body = []byte(`{"jsonrpc":"2.0","id":1,"method":"` + acp.MethodAgentResumeSession + `","params":{"sessionId":"session-to-resume","cwd":"/tmp","mcpServers":[]}}`)

	HandleProtocolPost(ctx, server)

	if ctx.statusCode != 200 {
		t.Fatalf("status = %d, want 200 (error=%q)", ctx.statusCode, ctx.errMessage)
	}
	if _, ok := conn.LookupSession("session-to-resume"); !ok {
		t.Fatal("successful resume did not register its transport session")
	}
}

func TestHandleProtocolPostRollsBackFailedResumeSession(t *testing.T) {
	httpConn := NewConnection()
	conn := NewProtocolConnection(ProtocolConnectionConfig{
		ConnectionID: "conn-failed-resume",
		HTTPConn:     httpConn,
		Dispatcher: connspi.Dispatcher{
			Request: func(context.Context, string, json.RawMessage) (any, error) {
				return nil, acp.ErrInternalError("resume failed", nil)
			},
		},
		Done: make(chan struct{}),
	})
	server := ProtocolServer{
		LookupConnection: func(connectionID string) (*ProtocolConnection, bool) {
			return conn, connectionID == conn.ConnectionID()
		},
	}

	ctx := newStubHandlerContext()
	ctx.requestHeaders["Content-Type"] = "application/json"
	ctx.requestHeaders["Accept"] = "application/json, text/event-stream"
	ctx.requestHeaders[acptransport.HeaderConnectionID] = conn.ConnectionID()
	ctx.requestHeaders[acptransport.HeaderSessionID] = "failed-session"
	ctx.body = []byte(`{"jsonrpc":"2.0","id":1,"method":"` + acp.MethodAgentResumeSession + `","params":{"sessionId":"failed-session","cwd":"/tmp","mcpServers":[]}}`)

	HandleProtocolPost(ctx, server)

	if _, ok := conn.LookupSession("failed-session"); ok {
		t.Fatal("failed resume left a provisional transport session routable")
	}
}

func TestHandleProtocolPostClosesFailedInitializeConnection(t *testing.T) {
	type traceKey struct{}

	var createTrace any
	closeCount := 0

	conn := NewProtocolConnection(ProtocolConnectionConfig{
		ConnectionID: "conn-init",
		HTTPConn:     NewConnection(),
		Dispatcher: connspi.Dispatcher{
			Request: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				return nil, acp.ErrMethodNotFound(method)
			},
		},
		Done: make(chan struct{}),
		Close: func() error {
			closeCount++
			return nil
		},
	})

	server := ProtocolServer{
		CreateConnection: func(ctx context.Context) (*ProtocolConnection, int, error) {
			createTrace = ctx.Value(traceKey{})
			return conn, 0, nil
		},
	}

	ctx := newStubHandlerContext()
	ctx.ctx = context.WithValue(context.Background(), traceKey{}, "init-trace")
	ctx.requestHeaders["Content-Type"] = "application/json"
	ctx.requestHeaders["Accept"] = "application/json, text/event-stream"
	ctx.body = []byte(`{"jsonrpc":"2.0","id":1,"method":"` + acp.MethodAgentInitialize + `","params":{}}`)

	HandleProtocolPost(ctx, server)

	if createTrace != "init-trace" {
		t.Fatalf("create connection context value = %#v, want %q", createTrace, "init-trace")
	}
	if closeCount != 1 {
		t.Fatalf("close count = %d, want 1", closeCount)
	}
	if len(ctx.sseEvents) != 1 {
		t.Fatalf("SSE event count = %d, want 1", len(ctx.sseEvents))
	}
}
