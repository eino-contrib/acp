// Package main is an example runnable that demonstrates the ACP Proxy SDK
// end-to-end.
//
// The binary has three roles, selected via the -role flag:
//
//   - proxy        : start an ACP Proxy on -listen, dialing -upstream for each
//                    incoming Client connection.
//   - agent-server : start an example AgentServer on -listen, accepting
//                    Streamer connections from the Proxy, running the echo
//                    agent against stdio.NewTransport fed by stream.NewPipe.
//   - all          : run both in one process (convenient for local testing).
//
// The example uses a small WebSocket-based Streamer (see ws_streamer.go) as
// its south-bound transport. In production, users replace this with their
// own gRPC/Kitex/TTHeader implementation — the Proxy only sees
// stream.StreamerFactory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	role := flag.String("role", "all", "role: proxy | agent-server | all")
	listen := flag.String("listen", "", "listen addr (defaults: proxy=:8080, agent-server=:9090)")
	upstream := flag.String("upstream", "ws://127.0.0.1:9090/acp-upstream", "agent-server upstream URL (proxy only)")
	proxyListen := flag.String("proxy-listen", ":8080", "proxy listen addr when -role=all")
	agentListen := flag.String("agent-listen", ":9090", "agent-server listen addr when -role=all")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch *role {
	case "proxy":
		addr := *listen
		if addr == "" {
			addr = ":8080"
		}
		if err := runProxy(ctx, addr, *upstream); err != nil {
			log.Fatalf("proxy: %v", err)
		}

	case "agent-server":
		addr := *listen
		if addr == "" {
			addr = ":9090"
		}
		if err := runAgentServer(ctx, addr); err != nil {
			log.Fatalf("agent-server: %v", err)
		}

	case "all":
		errCh := make(chan error, 2)
		go func() {
			errCh <- fmt.Errorf("agent-server: %w", runAgentServer(ctx, *agentListen))
		}()
		// Give the agent-server a beat to bind before the proxy dials it on
		// first connection. We don't strictly need a sleep — NewStreamer will
		// just retry-fail its first attempt — but this keeps the demo log
		// output clean.
		go func() {
			errCh <- fmt.Errorf("proxy: %w", runProxy(ctx, *proxyListen,
				fmt.Sprintf("ws://127.0.0.1%s/acp-upstream", *agentListen)))
		}()
		for i := 0; i < 2; i++ {
			if err := <-errCh; err != nil {
				log.Printf("%v", err)
			}
		}

	default:
		log.Fatalf("unknown role: %q (want proxy | agent-server | all)", *role)
	}
}
