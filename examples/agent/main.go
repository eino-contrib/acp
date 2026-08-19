// Example ACP agent that echoes prompts back.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	ginframework "github.com/gin-gonic/gin"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acpserver "github.com/eino-contrib/acp/server"
	acpgin "github.com/eino-contrib/acp/server/gin"
	acphertz "github.com/eino-contrib/acp/server/hertz"
	"github.com/eino-contrib/acp/transport/stdio"
)

const shutdownTimeout = 5 * time.Second

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

func runHTTPTransport(ctx context.Context, listenAddr, framework string) error {
	core, err := acpserver.NewACPServer(func(_ context.Context) acp.Agent { return NewAgent() })
	if err != nil {
		return fmt.Errorf("create ACP server: %w", err)
	}

	switch framework {
	case "hertz":
		return runHertzServer(ctx, listenAddr, core)
	case "gin":
		return runGinServer(ctx, listenAddr, core)
	default:
		_ = core.Close()
		return fmt.Errorf("unsupported HTTP framework %q (want hertz or gin)", framework)
	}
}

func runHertzServer(ctx context.Context, listenAddr string, core *acpserver.ACPServer) error {
	srv := hertzserver.New(
		hertzserver.WithHostPorts(listenAddr),
		hertzserver.WithStreamBody(true),
	)
	// Hertz must not recycle a hijacked WebSocket connection.
	srv.NoHijackConnPool = true
	srv.Any(acpserver.DefaultEndpoint, acphertz.New(core))

	fmt.Fprintf(os.Stderr, "Listening on %s with Hertz (path=%s)\n", listenAddr, acpserver.DefaultEndpoint)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run() }()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = core.Shutdown(shutdownCtx)
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		closeErr := core.Close()
		hostErr := srv.Shutdown(shutdownCtx)
		coreErr := core.Shutdown(shutdownCtx)
		return errors.Join(
			wrapShutdownError("ACP server close", closeErr),
			wrapShutdownError("ACP server", coreErr),
			wrapShutdownError("Hertz", hostErr),
		)
	}
}

func runGinServer(ctx context.Context, listenAddr string, core *acpserver.ACPServer) error {
	router := ginframework.New()
	router.Any(acpserver.DefaultEndpoint, acpgin.New(core))
	srv := &http.Server{Addr: listenAddr, Handler: router}

	fmt.Fprintf(os.Stderr, "Listening on %s with Gin (path=%s)\n", listenAddr, acpserver.DefaultEndpoint)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = core.Shutdown(shutdownCtx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		closeErr := core.Close()
		hostErr := srv.Shutdown(shutdownCtx)
		coreErr := core.Shutdown(shutdownCtx)
		return errors.Join(
			wrapShutdownError("ACP server close", closeErr),
			wrapShutdownError("ACP server", coreErr),
			wrapShutdownError("Gin HTTP server", hostErr),
		)
	}
}

func wrapShutdownError(component string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("shut down %s: %w", component, err)
}

func main() {
	transportMode := flag.String("transport", "stdio", "transport mode: stdio or http")
	listenAddr := flag.String("listen", ":8080", "listen address when -transport=http")
	httpFramework := flag.String("http-framework", "hertz", "HTTP framework when -transport=http: hertz or gin")
	flag.Parse()

	agent := NewAgent()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *transportMode {
	case "stdio":
		if err := runStdioTransport(ctx, agent); err != nil {
			fmt.Fprintf(os.Stderr, "agent error: %v\n", err)
			os.Exit(1)
		}
	case "http":
		if err := runHTTPTransport(ctx, *listenAddr, *httpFramework); err != nil {
			fmt.Fprintf(os.Stderr, "agent server error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unsupported transport: %s\n", *transportMode)
		os.Exit(1)
	}
}
