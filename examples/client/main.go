// Example ACP client that connects to an agent and sends a prompt.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime/debug"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acphttpclient "github.com/eino-contrib/acp/transport/http/client"
	"github.com/eino-contrib/acp/transport/stdio"
	acpws "github.com/eino-contrib/acp/transport/ws"
)

func safeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic: %v\n%s", r, debug.Stack())
			}
		}()
		f()
	}()
}

// httpResult bundles the unified connection with the HTTP transport so callers
// can access HTTP-specific helpers (listener management, connection metadata)
// directly on the transport.
type httpResult struct {
	conn      *acpconn.ClientConnection
	transport *acphttpclient.ClientTransport
}

func connectSpawn(ctx context.Context, client acp.Client, agentBinary string, args []string) (*acpconn.ClientConnection, error) {
	cmd := exec.CommandContext(ctx, agentBinary, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	transport := stdio.NewTransport(stdoutPipe, stdinPipe)
	conn := acpconn.NewClientConnection(client, transport)
	safeGo(func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("agent process exited: %v", err)
		}
		if err := conn.Close(); err != nil {
			log.Printf("close client connection: %v", err)
		}
	})
	return conn, nil
}

func connectHTTP(_ context.Context, client acp.Client, baseURL string) (*httpResult, error) {
	transport := acphttpclient.NewClientTransport(baseURL)
	conn := acpconn.NewClientConnection(client, transport)
	return &httpResult{conn: conn, transport: transport}, nil
}

func connectWebSocket(ctx context.Context, client acp.Client, baseURL string) (*acpconn.ClientConnection, error) {
	transport, err := acpws.NewWebSocketClientTransport(baseURL)
	if err != nil {
		return nil, fmt.Errorf("create websocket transport: %w", err)
	}
	if err := transport.Connect(ctx); err != nil {
		return nil, fmt.Errorf("websocket connect: %w", err)
	}
	conn := acpconn.NewClientConnection(client, transport)
	return conn, nil
}

func main() {
	transportMode := flag.String("transport", "spawn", "transport mode: spawn, http, or ws")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [-transport=spawn] <agent-binary> [args...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "   or: %s -transport=http <base-url>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "   or: %s -transport=ws <base-url>\n", os.Args[0])
		os.Exit(1)
	}

	ctx := context.Background()
	client := New()

	var (
		conn *acpconn.ClientConnection
		err  error
	)

	switch *transportMode {
	case "spawn":
		conn, err = connectSpawn(ctx, client, flag.Arg(0), flag.Args()[1:])
	case "http":
		var hr *httpResult
		hr, err = connectHTTP(ctx, client, flag.Arg(0))
		if hr != nil {
			conn = hr.conn
		}
	case "ws":
		conn, err = connectWebSocket(ctx, client, flag.Arg(0))
	default:
		fmt.Fprintf(os.Stderr, "unsupported transport: %s\n", *transportMode)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if err = conn.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "start connection: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ClientInfo: &acp.Implementation{
			Name:    "example-client",
			Version: "0.1.0",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Connected to agent: protocol=%d\n", initResp.ProtocolVersion)

	sessionResp, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: ".", MCPServers: []acp.MCPServer{}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "new session: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Session: %s\n\n", sessionResp.SessionID)

	promptText := "帮我看一下当前项目的目录结构"
	fmt.Fprintf(os.Stderr, "[👤 User] %s\n", promptText)

	_, err = conn.Prompt(ctx, acp.PromptRequest{
		SessionID: sessionResp.SessionID,
		Prompt:    []acp.ContentBlock{acp.NewContentBlockText(acp.TextContent{Text: promptText})},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "prompt: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "\n✅ Done.\n")
}
