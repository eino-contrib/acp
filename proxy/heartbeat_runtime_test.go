package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hclient "github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/stream"
	"github.com/hertz-contrib/websocket"
	"io"
)

// mockStreamer implements stream.Streamer for proxy heartbeat tests.
type mockStreamer struct {
	mu        sync.Mutex
	payloads  [][]byte
	readCh    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newMockStreamer() *mockStreamer {
	return &mockStreamer{
		readCh: make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (m *mockStreamer) WritePayload(_ context.Context, payload []byte) error {
	m.mu.Lock()
	m.payloads = append(m.payloads, append([]byte(nil), payload...))
	m.mu.Unlock()
	return nil
}

func (m *mockStreamer) ReadPayload(_ context.Context) ([]byte, error) {
	select {
	case p, ok := <-m.readCh:
		if !ok {
			return nil, io.EOF
		}
		return p, nil
	case <-m.closed:
		return nil, io.EOF
	}
}

func (m *mockStreamer) Close(_ string) error {
	m.closeOnce.Do(func() { close(m.closed) })
	return nil
}

var _ stream.Streamer = (*mockStreamer)(nil)

// proxyTestEnv holds a hertz WS server for creating real ws connection pairs.
type proxyTestEnv struct {
	addr   string
	connCh chan *websocket.Conn
	srv    *server.Hertz
}

func newProxyTestEnv(t *testing.T) *proxyTestEnv {
	t.Helper()

	addr := randomTestAddress(t)
	connCh := make(chan *websocket.Conn, 4)
	blockCh := make(chan struct{})

	srv := server.New(server.WithHostPorts(addr))
	srv.NoHijackConnPool = true
	upgrader := &websocket.HertzUpgrader{}

	srv.GET("/ws", func(ctx context.Context, c *app.RequestContext) {
		err := upgrader.Upgrade(c, func(conn *websocket.Conn) {
			connCh <- conn
			// Block until the test is done to keep the upgrade handler alive.
			<-blockCh
		})
		if err != nil {
			c.SetStatusCode(http.StatusInternalServerError)
		}
	})

	errCh := make(chan error, 1)
	safe.Go(func() { errCh <- srv.Run() })
	waitForReady(t, "http://"+addr+"/ws")

	t.Cleanup(func() {
		close(blockCh)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	return &proxyTestEnv{addr: addr, connCh: connCh, srv: srv}
}

// dialClient connects to the test server and returns a client-side *websocket.Conn.
func (e *proxyTestEnv) dialClient(t *testing.T) *websocket.Conn {
	t.Helper()

	client, err := hclient.NewClient(hclient.WithDialer(standard.NewDialer()))
	if err != nil {
		t.Fatalf("create hertz client: %v", err)
	}

	req := protocol.AcquireRequest()
	resp := protocol.AcquireResponse()
	req.SetRequestURI("http://" + e.addr + "/ws")
	req.SetMethod(consts.MethodGet)

	upgrader := &websocket.ClientUpgrader{}
	upgrader.PrepareRequest(req)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Do(ctx, req, resp); err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn, err := upgrader.UpgradeResponse(req, resp)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return conn
}

func randomTestAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitForReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not become ready: %s", url)
}

// TestProxyFirstFrameTimeout verifies that the proxy disconnects when no data
// frame arrives within firstFrameTimeout.
func TestProxyFirstFrameTimeout(t *testing.T) {
	env := newProxyTestEnv(t)

	clientConn := env.dialClient(t)
	defer clientConn.Close()
	serverConn := <-env.connCh

	streamer := newMockStreamer()
	pc := &proxyConn{
		id:                "test-first-frame",
		ws:                serverConn,
		streamer:          streamer,
		wsWriteMu:         &sync.Mutex{},
		firstFrameTimeout: 50 * time.Millisecond,
		readTimeout:       500 * time.Millisecond,
	}
	pc.installHeartbeat()

	done := make(chan struct{})
	go func() {
		pc.run(context.Background())
		close(done)
	}()

	// Client sends only Pings (no data frame). Proxy should timeout.
	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(15 * time.Millisecond)
			_ = clientConn.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(time.Second))
		}
	}()

	select {
	case <-done:
		// Good - proxy timed out because no data frame arrived.
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not disconnect after first-frame timeout")
	}

	// Verify the client received close code 4001 (first-frame timeout).
	_, _, err := clientConn.ReadMessage()
	if err == nil {
		t.Fatal("expected close error from client read")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket.CloseError, got %T: %v", err, err)
	}
	if closeErr.Code != CloseCodeFirstFrameTimeout {
		t.Fatalf("expected close code %d, got %d", CloseCodeFirstFrameTimeout, closeErr.Code)
	}
}

// TestProxyPingBeforeFirstFrame verifies that Ping before the first data frame
// echoes Pong but does NOT refresh the read deadline.
func TestProxyPingBeforeFirstFrame(t *testing.T) {
	env := newProxyTestEnv(t)

	clientConn := env.dialClient(t)
	defer clientConn.Close()
	serverConn := <-env.connCh

	streamer := newMockStreamer()
	pc := &proxyConn{
		id:                "test-ping-before",
		ws:                serverConn,
		streamer:          streamer,
		wsWriteMu:         &sync.Mutex{},
		firstFrameTimeout: 80 * time.Millisecond,
		readTimeout:       500 * time.Millisecond,
	}
	pc.installHeartbeat()

	done := make(chan struct{})
	go func() {
		pc.run(context.Background())
		close(done)
	}()

	// Verify Pong is echoed back.
	pongReceived := make(chan string, 1)
	clientConn.SetPongHandler(func(appData string) error {
		select {
		case pongReceived <- appData:
		default:
		}
		return nil
	})

	// Send Ping from client
	_ = clientConn.WriteControl(websocket.PingMessage, []byte("hello"), time.Now().Add(time.Second))

	// Must read to process Pong
	go func() {
		for {
			_, _, err := clientConn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	// Verify Pong was echoed
	select {
	case payload := <-pongReceived:
		if payload != "hello" {
			t.Fatalf("pong payload mismatch: got %q, want %q", payload, "hello")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("did not receive Pong")
	}

	// Connection should still timeout at ~80ms (Ping did NOT extend deadline)
	select {
	case <-done:
		// Expected - proxy timed out
	case <-time.After(2 * time.Second):
		t.Fatal("proxy should have timed out; Ping before first frame should not refresh deadline")
	}
}

// TestProxyPingAfterFirstFrame verifies that Ping after the first data frame
// refreshes the read deadline, keeping the connection alive.
func TestProxyPingAfterFirstFrame(t *testing.T) {
	env := newProxyTestEnv(t)

	clientConn := env.dialClient(t)
	defer clientConn.Close()
	serverConn := <-env.connCh

	streamer := newMockStreamer()
	pc := &proxyConn{
		id:                "test-ping-after",
		ws:                serverConn,
		streamer:          streamer,
		wsWriteMu:         &sync.Mutex{},
		firstFrameTimeout: 200 * time.Millisecond,
		readTimeout:       60 * time.Millisecond,
	}
	pc.installHeartbeat()

	done := make(chan struct{})
	go func() {
		pc.run(context.Background())
		close(done)
	}()

	// Send a data frame to pass the first-frame gate.
	_ = clientConn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`))
	time.Sleep(10 * time.Millisecond)

	// Now send Pings at intervals shorter than readTimeout (60ms).
	// Send every 30ms for 5 iterations (~150ms total).
	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(30 * time.Millisecond)
			_ = clientConn.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(time.Second))
		}
	}()

	// Connection should stay alive during Ping interval (~150ms).
	// It should NOT timeout before 100ms.
	select {
	case <-done:
		t.Fatal("proxy should not have timed out while Pings are refreshing deadline")
	case <-time.After(100 * time.Millisecond):
		// Good - still alive
	}

	// After Pings stop (~150ms), should timeout ~60ms later (total ~210ms).
	select {
	case <-done:
		// Good - timed out after Pings stopped
	case <-time.After(2 * time.Second):
		t.Fatal("proxy should have timed out after Pings stopped")
	}

	// Verify the client received close code 1001 (going away / read timeout).
	_, _, err := clientConn.ReadMessage()
	if err == nil {
		t.Fatal("expected close error from client read")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket.CloseError, got %T: %v", err, err)
	}
	if closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("expected close code %d (GoingAway), got %d", websocket.CloseGoingAway, closeErr.Code)
	}
}
