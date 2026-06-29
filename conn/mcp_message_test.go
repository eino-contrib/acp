package conn

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
)

// mcpMessageAgent records inbound mcp/message traffic and answers the request
// form with a fixed inner MCP result, so tests can assert the request reaches a
// handler (and is not method-not-found) and that the notification form still
// dispatches as a notification.
type mcpMessageAgent struct {
	acp.BaseAgent

	gotRequest      chan acp.MessageMCPRequest
	gotNotification chan acp.MessageMCPNotification
}

func (a *mcpMessageAgent) UnstableMCPMessage(_ context.Context, req acp.MessageMCPRequest) (acp.MessageMCPResponse, error) {
	a.gotRequest <- req
	return acp.MessageMCPResponse(`{"ok":true}`), nil
}

func (a *mcpMessageAgent) UnstableMCPMessageNotification(_ context.Context, n acp.MessageMCPNotification) error {
	a.gotNotification <- n
	return nil
}

// TestAgentDispatchesMCPMessageRequest verifies that an mcp/message *request*
// (JSON-RPC id present) reaches the request handler and produces a result,
// rather than failing with method-not-found. This is the regression guard for
// the codegen bug where mcp/message was generated as notification-only.
func TestAgentDispatchesMCPMessageRequest(t *testing.T) {
	transport := newChannelTransport()
	agent := &mcpMessageAgent{
		gotRequest:      make(chan acp.MessageMCPRequest, 1),
		gotNotification: make(chan acp.MessageMCPNotification, 1),
	}
	conn := NewAgentConnectionFromTransport(agent, transport)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"mcp/message","params":{"connectionId":"c1","method":"tools/list"}}`)

	select {
	case req := <-agent.gotRequest:
		if req.ConnectionID != "c1" || req.Method != "tools/list" {
			t.Fatalf("handler saw unexpected request: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mcp/message request never reached the request handler")
	}

	select {
	case raw := <-transport.outbox:
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode response: %v (raw=%s)", err, raw)
		}
		if resp.Error != nil {
			t.Fatalf("mcp/message request returned an error response: %+v (raw=%s)", resp.Error, raw)
		}
		if string(resp.Result) != `{"ok":true}` {
			t.Fatalf("unexpected result: %s", resp.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no response written for mcp/message request")
	}
}

// TestAgentDispatchesMCPMessageNotification verifies that the notification form
// (no JSON-RPC id) still dispatches to the notification handler.
func TestAgentDispatchesMCPMessageNotification(t *testing.T) {
	transport := newChannelTransport()
	agent := &mcpMessageAgent{
		gotRequest:      make(chan acp.MessageMCPRequest, 1),
		gotNotification: make(chan acp.MessageMCPNotification, 1),
	}
	conn := NewAgentConnectionFromTransport(agent, transport)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"mcp/message","params":{"connectionId":"c1","method":"notifications/initialized"}}`)

	select {
	case n := <-agent.gotNotification:
		if n.ConnectionID != "c1" || n.Method != "notifications/initialized" {
			t.Fatalf("handler saw unexpected notification: %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mcp/message notification never reached the notification handler")
	}
}
