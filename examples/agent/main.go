// Example ACP agent that echoes prompts back.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acpserver "github.com/eino-contrib/acp/server"
	"github.com/eino-contrib/acp/transport/stdio"
)

func runStdioTransport(ctx context.Context, agent acp.Agent) error {
	transport := stdio.NewTransport(os.Stdin, os.Stdout)
	conn := acpconn.NewAgentConnectionFromTransport(agent, transport)
	if aware, ok := agent.(acpserver.ConnectionAwareAgent); ok {
		aware.SetClientConnection(conn)
	}
	if err := conn.Start(ctx); err != nil {
		return err
	}
	<-conn.Done()
	return nil
}

func runHTTPTransport(listenAddr string) {
	srv := hertzserver.New(hertzserver.WithHostPorts(listenAddr))
	srv.NoHijackConnPool = true
	remote, err := acpserver.NewACPServer(func(_ context.Context) acp.Agent { return NewAgent() })
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ACP server: %v\n", err)
		os.Exit(1)
	}
	remote.Mount(srv)
	fmt.Fprintf(os.Stderr, "Listening on %s\n", listenAddr)

	srv.Spin()
}

func main() {
	transportMode := flag.String("transport", "stdio", "transport mode: stdio or http")
	listenAddr := flag.String("listen", ":8080", "listen address when -transport=http")
	flag.Parse()

	agent := NewAgent()
	ctx := context.Background()

	switch *transportMode {
	case "stdio":
		if err := runStdioTransport(ctx, agent); err != nil {
			fmt.Fprintf(os.Stderr, "agent error: %v\n", err)
			os.Exit(1)
		}
	case "http":
		runHTTPTransport(*listenAddr)
	default:
		fmt.Fprintf(os.Stderr, "unsupported transport: %s\n", *transportMode)
		os.Exit(1)
	}
}
