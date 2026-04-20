package main

import (
	"context"
	"fmt"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"

	"github.com/eino-contrib/acp/proxy"
)

// runProxy starts an ACPProxy on listen, dialing upstreamURL via the example
// WebSocket StreamerFactory. This matches P1 §6.1 posture A: a single binary
// whose role is chosen at startup.
func runProxy(ctx context.Context, listen, upstreamURL string) error {
	factory, err := newWSStreamerFactory(upstreamURL)
	if err != nil {
		return fmt.Errorf("build streamer factory: %w", err)
	}

	p, err := proxy.NewACPProxy(factory,
		proxy.WithHeaderForwarder(proxy.ForwardHeaders("Authorization", "X-Tenant-Id")),
	)
	if err != nil {
		return fmt.Errorf("build proxy: %w", err)
	}

	srv := hertzserver.New(hertzserver.WithHostPorts(listen))
	srv.NoHijackConnPool = true
	p.Mount(srv)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = p.Close()
	}()

	fmt.Printf("[proxy] listening on %s (path=/acp) → upstream %s\n", listen, upstreamURL)
	srv.Spin()
	return nil
}
