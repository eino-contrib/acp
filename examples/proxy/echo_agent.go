package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/google/uuid"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/server"
)

// echoAgent is a minimal acp.Agent that echoes the user's prompt after a
// simulated tool-call. Structure mirrors examples/agent/agent.go so users
// see the same shape in both the local-agent demo and the proxy demo.
type echoAgent struct {
	acp.BaseAgent
	clientConn *acpconn.AgentConnection
}

var (
	_ acp.Agent                   = (*echoAgent)(nil)
	_ acp.ExtMethodHandler        = (*echoAgent)(nil)
	_ acp.ExtNotificationHandler  = (*echoAgent)(nil)
	_ server.ConnectionAwareAgent = (*echoAgent)(nil)
)

func newEchoAgent() *echoAgent {
	return &echoAgent{}
}

func (a *echoAgent) SetClientConnection(c *acpconn.AgentConnection) {
	a.clientConn = c
}

func (a *echoAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersion(acp.CurrentProtocolVersion),
		AgentInfo: &acp.Implementation{
			Name:    "echo-agent-proxied",
			Version: "0.1.0",
		},
	}, nil
}

func (a *echoAgent) NewSession(_ context.Context, _ acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{
		SessionID: acp.SessionID(uuid.NewString()),
	}, nil
}

func (a *echoAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	userText := extractText(req.Prompt)
	a.simulateSessionUpdates(ctx, req.SessionID, userText)

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *echoAgent) simulateSessionUpdates(ctx context.Context, sessionID acp.SessionID, userText string) {
	a.clientConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.NewSessionUpdateAgentThoughtChunk(acp.ContentChunk{
			Content: acp.NewContentBlockText(acp.TextContent{
				Text: fmt.Sprintf("用户问的是「%s」，我需要先查一下相关文件再回答。", userText),
			}),
		}),
	})

	randomSleep()

	toolCallID := acp.ToolCallID(uuid.NewString())
	statusInProgress := acp.ToolCallStatusInProgress
	kindExecute := acp.ToolKindExecute

	a.clientConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.NewSessionUpdateToolCall(acp.ToolCall{
			ToolCallID: toolCallID,
			Title:      "read_file",
			Status:     &statusInProgress,
			Kind:       &kindExecute,
		}),
	})

	randomSleep()

	statusCompleted := acp.ToolCallStatusCompleted
	a.clientConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.NewSessionUpdateToolCallUpdate(acp.ToolCallUpdate{
			ToolCallID: toolCallID,
			Title:      "read_file",
			Status:     &statusCompleted,
			Kind:       &kindExecute,
		}),
	})

	randomSleep()

	a.clientConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.NewSessionUpdateAgentMessageChunk(acp.ContentChunk{
			Content: acp.NewContentBlockText(acp.TextContent{
				Text: fmt.Sprintf("根据查到的内容，关于「%s」的回答如下：这是一个示例回复，实际场景中会返回更详细的结果。", userText),
			}),
		}),
	})
}

func randomSleep() {
	d := time.Duration(1500+rand.Intn(1500)) * time.Millisecond
	time.Sleep(d)
}

func (a *echoAgent) HandleExtMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	fmt.Fprintf(os.Stderr, "[agent] ext method: %s %s\n", method, params)
	return nil, nil
}

func (a *echoAgent) HandleExtNotification(_ context.Context, method string, params json.RawMessage) error {
	fmt.Fprintf(os.Stderr, "[agent] ext notification: %s %s\n", method, params)
	return nil
}

func extractText(blocks []acp.ContentBlock) string {
	var out string
	for _, b := range blocks {
		if t, ok := b.AsText(); ok {
			out += t.Text
		}
	}
	if out == "" {
		return "<non-text>"
	}
	return out
}
