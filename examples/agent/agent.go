package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/server"
	"github.com/google/uuid"
)

var _ acp.Agent = (*Agent)(nil)
var _ acp.ExtMethodHandler = (*Agent)(nil)
var _ acp.ExtNotificationHandler = (*Agent)(nil)
var _ server.ConnectionAwareAgent = (*Agent)(nil)

// Agent is a minimal example ACP agent used by examples/agent.
type Agent struct {
	acp.BaseAgent
	agentConn *acpconn.AgentConnection
}

// NewAgent constructs the example agent.
func NewAgent() *Agent {
	return &Agent{}
}

func (a *Agent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersion(acp.CurrentProtocolVersion),
		AgentInfo: &acp.Implementation{
			Name:    "echo-agent",
			Version: "0.1.0",
		},
	}, nil
}

func (a *Agent) NewSession(_ context.Context, _ acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{
		SessionID: acp.SessionID(uuid.NewString()),
	}, nil
}

func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	userText := extractPromptText(params.Prompt)
	a.simulateSessionUpdates(ctx, params.SessionID, userText)
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func extractPromptText(blocks []acp.ContentBlock) string {
	var text string
	for _, b := range blocks {
		if t, ok := b.AsText(); ok {
			text += t.Text
		}
	}
	if text == "" {
		return "<non-text content>"
	}
	return text
}

func (a *Agent) simulateSessionUpdates(ctx context.Context, sessionID acp.SessionID, userText string) {
	a.agentConn.SessionUpdate(ctx, acp.SessionNotification{
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

	a.agentConn.SessionUpdate(ctx, acp.SessionNotification{
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
	a.agentConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.NewSessionUpdateToolCallUpdate(acp.ToolCallUpdate{
			ToolCallID: toolCallID,
			Title:      "read_file",
			Status:     &statusCompleted,
			Kind:       &kindExecute,
		}),
	})

	randomSleep()

	a.agentConn.SessionUpdate(ctx, acp.SessionNotification{
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

func (a *Agent) SetClientConnection(conn *acpconn.AgentConnection) {
	a.agentConn = conn
}

func (a *Agent) HandleExtMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	fmt.Fprintf(os.Stderr, "Agent HandleExtMethod: method=%s params=%s\n", method, params)
	return nil, nil
}

func (a *Agent) HandleExtNotification(_ context.Context, method string, params json.RawMessage) error {
	fmt.Fprintf(os.Stderr, "Agent HandleExtNotification: method=%s params=%s\n", method, params)
	return nil
}
