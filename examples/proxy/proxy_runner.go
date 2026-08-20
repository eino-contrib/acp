package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	ginframework "github.com/gin-gonic/gin"

	acpproxy "github.com/eino-contrib/acp/proxy"
	acpproxygin "github.com/eino-contrib/acp/proxy/gin"
	acpproxyhertz "github.com/eino-contrib/acp/proxy/hertz"
)

const proxyShutdownTimeout = 5 * time.Second

// runProxy starts an ACPProxy on listen, dialing upstreamURL via the example
// WebSocket StreamerFactory. This matches P1 §6.1 posture A: a single binary
// whose role is chosen at startup.
func runProxy(ctx context.Context, listen, upstreamURL, framework string) error {
	factory, err := newWSStreamerFactory(upstreamURL)
	if err != nil {
		return fmt.Errorf("build streamer factory: %w", err)
	}

	core, err := acpproxy.NewACPProxy(factory,
		acpproxy.WithMetadataExtractor(acpproxy.ForwardHeaders("Authorization", "X-Tenant-Id")),
	)
	if err != nil {
		return fmt.Errorf("build proxy: %w", err)
	}

	switch framework {
	case "hertz":
		return runHertzProxy(ctx, listen, upstreamURL, core)
	case "gin":
		return runGinProxy(ctx, listen, upstreamURL, core)
	default:
		_ = core.Close()
		return fmt.Errorf("unsupported HTTP framework %q (want hertz or gin)", framework)
	}
}

func runHertzProxy(ctx context.Context, listen, upstreamURL string, core *acpproxy.ACPProxy) error {
	srv := hertzserver.New(hertzserver.WithHostPorts(listen))
	// Hertz must not recycle a hijacked WebSocket connection.
	srv.NoHijackConnPool = true
	srv.Any(acpproxy.DefaultEndpoint, acpproxyhertz.New(core))

	fmt.Printf("[proxy] listening on %s with Hertz (path=%s) → upstream %s\n", listen, acpproxy.DefaultEndpoint, upstreamURL)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run() }()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), proxyShutdownTimeout)
		defer cancel()
		_ = core.Shutdown(shutdownCtx)
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), proxyShutdownTimeout)
		defer cancel()
		closeErr := core.Close()
		hostErr := srv.Shutdown(shutdownCtx)
		coreErr := core.Shutdown(shutdownCtx)
		return errors.Join(
			wrapProxyShutdownError("ACP proxy close", closeErr),
			wrapProxyShutdownError("ACP proxy", coreErr),
			wrapProxyShutdownError("Hertz", hostErr),
		)
	}
}

func runGinProxy(ctx context.Context, listen, upstreamURL string, core *acpproxy.ACPProxy) error {
	router := ginframework.New()
	router.Any(acpproxy.DefaultEndpoint, acpproxygin.New(core))
	srv := &http.Server{Addr: listen, Handler: router}

	fmt.Printf("[proxy] listening on %s with Gin (path=%s) → upstream %s\n", listen, acpproxy.DefaultEndpoint, upstreamURL)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), proxyShutdownTimeout)
		defer cancel()
		_ = core.Shutdown(shutdownCtx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), proxyShutdownTimeout)
		defer cancel()
		closeErr := core.Close()
		hostErr := srv.Shutdown(shutdownCtx)
		coreErr := core.Shutdown(shutdownCtx)
		return errors.Join(
			wrapProxyShutdownError("ACP proxy close", closeErr),
			wrapProxyShutdownError("ACP proxy", coreErr),
			wrapProxyShutdownError("Gin HTTP server", hostErr),
		)
	}
}

func wrapProxyShutdownError(component string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("shut down %s: %w", component, err)
}
