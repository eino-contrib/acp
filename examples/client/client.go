package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	acp "github.com/eino-contrib/acp"
)

var _ acp.Client = (*Client)(nil)
var _ acp.ExtMethodHandler = (*Client)(nil)
var _ acp.ExtNotificationHandler = (*Client)(nil)

// Client is a minimal example ACP client used by examples/client.
type Client struct{ acp.BaseClient }

// New constructs the example client.
func New() *Client {
	return &Client{}
}

func (c *Client) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	update := params.Update
	if v, ok := update.AsAgentThoughtChunk(); ok {
		if text, ok := v.Content.AsText(); ok {
			fmt.Fprintf(os.Stderr, "[💭 Thought] %s\n", text.Text)
		}
	} else if v, ok := update.AsAgentMessageChunk(); ok {
		if text, ok := v.Content.AsText(); ok {
			fmt.Fprintf(os.Stderr, "[💬 Message] %s\n", text.Text)
		}
	} else if v, ok := update.AsToolCall(); ok {
		status := ""
		if v.Status != nil {
			status = string(*v.Status)
		}
		fmt.Fprintf(os.Stderr, "[🔧 ToolCall] id=%s title=%s status=%s\n", v.ToolCallID, v.Title, status)
	} else if v, ok := update.AsToolCallUpdate(); ok {
		status := ""
		if v.Status != nil {
			status = string(*v.Status)
		}
		fmt.Fprintf(os.Stderr, "[🔄 ToolCallUpdate] id=%s title=%s status=%s\n", v.ToolCallID, v.Title, status)
	} else {
		data, err := json.MarshalIndent(params, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal session update: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[📦 SessionUpdate] %s\n", data)
	}
	return nil
}

func (c *Client) HandleExtMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	fmt.Fprintf(os.Stderr, "Client HandleExtMethod: method=%s params=%s\n", method, params)
	return nil, nil
}

func (c *Client) HandleExtNotification(_ context.Context, method string, params json.RawMessage) error {
	fmt.Fprintf(os.Stderr, "Client HandleExtNotification: method=%s params=%s\n", method, params)
	return nil
}
