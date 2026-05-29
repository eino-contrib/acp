package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/stream"
	ws "github.com/eino-contrib/acp/transport/ws"
)

// These tests exercise the client → proxy heartbeat seam over a real TCP
// connection: a real WebSocketClientTransport (with its own ping pump) talks
// to a real ACPProxy mounted on hertz, backed by an echo Streamer. The
// per-role unit tests cover each side in isolation; these verify the composed
// behaviour, in particular that client-initiated Ping frames passing through
// the proxy correctly refresh the proxy's read deadline.

// echoStreamer relays every payload written to it straight back out, so a
// client write produces a matching client read through the proxy. This gives
// integration tests a public-API liveness probe via a request/response round
// trip.
type echoStreamer struct {
	out       chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newEchoStreamer() *echoStreamer {
	return &echoStreamer{
		out:    make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (s *echoStreamer) WritePayload(ctx context.Context, payload []byte) error {
	cp := append([]byte(nil), payload...)
	select {
	case s.out <- cp:
		return nil
	case <-s.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *echoStreamer) ReadPayload(ctx context.Context) ([]byte, error) {
	select {
	case p := <-s.out:
		return p, nil
	case <-s.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *echoStreamer) Close(string) error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

var _ stream.Streamer = (*echoStreamer)(nil)

type echoFactory struct{}

func (echoFactory) NewStreamer(context.Context, map[string]string) (stream.Streamer, error) {
	return newEchoStreamer(), nil
}

// newIntegrationProxy stands up a hertz server with an ACPProxy mounted on the
// default /acp endpoint, backed by an echo Streamer. It returns the listen
// address ("host:port").
func newIntegrationProxy(t *testing.T, opts ...Option) string {
	t.Helper()

	addr := randomTestAddress(t)
	p, err := NewACPProxy(echoFactory{}, opts...)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	srv := server.New(server.WithHostPorts(addr))
	srv.NoHijackConnPool = true
	p.Mount(srv)

	errCh := make(chan error, 1)
	safe.Go(func() { errCh <- srv.Run() })
	// A non-WebSocket GET to /acp returns 400, but a non-nil HTTP response is
	// enough to confirm the listener is up.
	waitForReady(t, "http://"+addr+"/acp")

	t.Cleanup(func() {
		_ = p.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	return addr
}

func mustReadWithin(t *testing.T, tr *ws.WebSocketClientTransport, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if _, err := tr.ReadMessage(ctx); err != nil {
		t.Fatalf("expected echo within %v, got error: %v", d, err)
	}
}

// TestClientPingKeepsProxyConnAlive verifies that client-initiated Ping frames
// keep the proxied connection alive well past the proxy's read timeout. The
// proxy's read deadline is far shorter than the idle window, so the connection
// only survives if each Ping refreshes it.
func TestClientPingKeepsProxyConnAlive(t *testing.T) {
	addr := newIntegrationProxy(t,
		WithWebSocketFirstFrameTimeout(3*time.Second),
		WithWebSocketReadTimeout(300*time.Millisecond),
	)

	tr, err := ws.NewWebSocketClientTransport("ws://"+addr,
		ws.WithPingInterval(25*time.Millisecond),
		ws.WithReadTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("new client transport: %v", err)
	}
	defer tr.Close()

	ctx := context.Background()
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Send one data frame to pass the proxy's first-frame gate so the 300ms
	// read timeout (not the first-frame timeout) governs from now on, then
	// drain the echo.
	probe := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"warmup"}`)
	if err := tr.WriteMessage(ctx, probe); err != nil {
		t.Fatalf("warmup write: %v", err)
	}
	mustReadWithin(t, tr, time.Second)

	// Idle for 3x the proxy read timeout. No data frames flow — only the
	// client's Ping frames. If the proxy refreshes its read deadline on each
	// Ping, the connection survives.
	time.Sleep(900 * time.Millisecond)

	// A fresh round trip must still succeed, proving the connection is alive.
	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := tr.WriteMessage(rctx, probe); err != nil {
		t.Fatalf("post-idle write failed; proxy likely tore down the connection despite client pings: %v", err)
	}
	if _, err := tr.ReadMessage(rctx); err != nil {
		t.Fatalf("post-idle read failed; proxy likely tore down the connection despite client pings: %v", err)
	}
}

// TestNoClientPingTriggersProxyReadTimeout verifies that without client Ping
// frames the proxy tears the connection down once its read timeout elapses,
// and the client observes the resulting close as a read error (not a caller
// context timeout).
func TestNoClientPingTriggersProxyReadTimeout(t *testing.T) {
	addr := newIntegrationProxy(t,
		WithWebSocketFirstFrameTimeout(3*time.Second),
		WithWebSocketReadTimeout(200*time.Millisecond),
	)

	// pingInterval=0 disables the client ping pump; readTimeout=0 keeps the
	// client passive so the proxy's read timeout is the only thing that can
	// tear the connection down.
	tr, err := ws.NewWebSocketClientTransport("ws://"+addr,
		ws.WithPingInterval(0),
		ws.WithReadTimeout(0),
	)
	if err != nil {
		t.Fatalf("new client transport: %v", err)
	}
	defer tr.Close()

	ctx := context.Background()
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Pass the first-frame gate so the read timeout (not the first-frame
	// timeout) is what fires, then drain the echo.
	probe := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"warmup"}`)
	if err := tr.WriteMessage(ctx, probe); err != nil {
		t.Fatalf("warmup write: %v", err)
	}
	mustReadWithin(t, tr, time.Second)

	// Now go idle with no pings. The proxy must tear the connection down
	// ~200ms later; the client read should return that close as an error.
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = tr.ReadMessage(rctx)
	if err == nil {
		t.Fatal("expected read error after proxy read timeout, got nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connection was not torn down within the window; read timed out instead: %v", err)
	}
}
