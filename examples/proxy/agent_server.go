package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/hertz-contrib/websocket"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/server"
	"github.com/eino-contrib/acp/stream"
	"github.com/eino-contrib/acp/transport/stdio"
)

// runAgentServer starts an example AgentServer on listen, exposing a
// WebSocket endpoint at /acp-upstream that the Proxy dials. It demonstrates
// the AgentServer-side wiring described in the proxy design notes:
//
//     wsStreamer  →  stream.NewPipe  →  stdio.NewTransport
//                 →  acpconn.NewAgentConnectionFromTransport(agent, t)
//
// The Agent implementation is identical in shape to examples/agent/agent.go.
func runAgentServer(ctx context.Context, listen string) error {
	srv := hertzserver.New(hertzserver.WithHostPorts(listen))
	srv.NoHijackConnPool = true

	upgrader := websocket.HertzUpgrader{}

	srv.GET("/acp-upstream", func(reqCtx context.Context, c *app.RequestContext) {
		// Snapshot any meta the Proxy forwarded. This example doesn't use it,
		// but production agents might consume e.g. Authorization / X-Tenant-Id
		// to pick per-tenant state.
		_ = reqCtx

		err := upgrader.Upgrade(c, func(wsConn *websocket.Conn) {
			serveAgentSession(ctx, wsConn)
		})
		if err != nil {
			c.String(500, fmt.Sprintf("upgrade failed: %v", err))
		}
	})

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("[agent-server] listening on %s (path=/acp-upstream)\n", listen)
	srv.Spin()
	return nil
}

// serveAgentSession wires one upgraded WebSocket into the ACP stdio transport
// and runs a fresh agent against it.
func serveAgentSession(parent context.Context, wsConn *websocket.Conn) {
	sessionCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()

	s := newWSStreamer(wsConn, 30*time.Second)
	defer func() { _ = s.Close("agent session ended") }()

	// 1. Adapt the Streamer to io.Reader / io.Writer for the stdio transport.
	r, w := stream.NewPipe(sessionCtx, s)
	defer r.Close()
	defer w.Close()

	// 2. Build the stdio transport over the pipe. This is the same call the
	//    examples/agent binary uses for its own stdin/stdout; only the source
	//    is different.
	t := stdio.NewTransport(r, w)

	// 3. Build the ACP agent connection the same way examples/agent/main.go
	//    does. The Agent itself is implementation-swappable.
	ag := newEchoAgent()
	conn := acpconn.NewAgentConnectionFromTransport(ag, t)
	if aware, ok := any(ag).(server.ConnectionAwareAgent); ok {
		aware.SetClientConnection(conn)
	}

	if err := conn.Start(sessionCtx); err != nil {
		fmt.Printf("[agent-server] start connection: %v\n", err)
		return
	}
	<-conn.Done()
	if err := conn.Err(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Printf("[agent-server] session ended with error: %v\n", err)
		return
	}
	fmt.Printf("[agent-server] session ended gracefully\n")
}

// Compile-time check: the example agent still satisfies acp.Agent (sanity
// check should the SDK's interface evolve).
var _ acp.Agent = (*echoAgent)(nil)
