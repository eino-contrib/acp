package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
	"github.com/hertz-contrib/websocket"
)

type stubMessageConn struct {
	readErr   error
	readLimit int64
}

type scriptedMessageConn struct {
	readCh    chan readResult
	writes    chan []byte
	closed    chan struct{}
	readLimit int64
}

type blockingWriteMessageConn struct {
	readCh       chan readResult
	writes       chan []byte
	closed       chan struct{}
	writeStarted chan struct{}
	allowWrite   chan struct{}
	readLimit    int64
}

type readResult struct {
	messageType int
	data        []byte
	err         error
}

type testLogger struct{}

func (testLogger) Debug(string, ...interface{})                      {}
func (testLogger) Info(string, ...interface{})                       {}
func (testLogger) Warn(string, ...interface{})                       {}
func (testLogger) Error(string, ...interface{})                      {}
func (testLogger) CtxDebug(context.Context, string, ...interface{})  {}
func (testLogger) CtxInfo(context.Context, string, ...interface{})   {}
func (testLogger) CtxWarn(context.Context, string, ...interface{})   {}
func (testLogger) CtxError(context.Context, string, ...interface{})  {}

func (c *stubMessageConn) ReadMessage() (int, []byte, error) {
	return 0, nil, c.readErr
}

func (c *stubMessageConn) WriteMessage(int, []byte) error {
	return nil
}

func (c *stubMessageConn) Close() error {
	return nil
}

func (c *stubMessageConn) SetReadLimit(limit int64) {
	c.readLimit = limit
}

func newScriptedMessageConn() *scriptedMessageConn {
	return &scriptedMessageConn{
		readCh: make(chan readResult, 1),
		writes: make(chan []byte, 4),
		closed: make(chan struct{}),
	}
}

func newBlockingWriteMessageConn() *blockingWriteMessageConn {
	return &blockingWriteMessageConn{
		readCh:       make(chan readResult, 1),
		writes:       make(chan []byte, 1),
		closed:       make(chan struct{}),
		writeStarted: make(chan struct{}, 1),
		allowWrite:   make(chan struct{}),
	}
}

func (c *scriptedMessageConn) ReadMessage() (int, []byte, error) {
	select {
	case result := <-c.readCh:
		return result.messageType, result.data, result.err
	case <-c.closed:
		return 0, nil, io.EOF
	}
}

func (c *scriptedMessageConn) WriteMessage(_ int, data []byte) error {
	c.writes <- append([]byte(nil), data...)
	return nil
}

func (c *scriptedMessageConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *scriptedMessageConn) SetReadLimit(limit int64) {
	c.readLimit = limit
}

func (c *blockingWriteMessageConn) ReadMessage() (int, []byte, error) {
	select {
	case result := <-c.readCh:
		return result.messageType, result.data, result.err
	case <-c.closed:
		return 0, nil, io.EOF
	}
}

func (c *blockingWriteMessageConn) WriteMessage(_ int, data []byte) error {
	select {
	case c.writeStarted <- struct{}{}:
	default:
	}
	select {
	case <-c.allowWrite:
	case <-c.closed:
		return net.ErrClosed
	}
	select {
	case c.writes <- append([]byte(nil), data...):
		return nil
	case <-c.closed:
		return net.ErrClosed
	}
}

func (c *blockingWriteMessageConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *blockingWriteMessageConn) SetReadLimit(limit int64) {
	c.readLimit = limit
}

func (c *blockingWriteMessageConn) SetWriteDeadline(time.Time) error {
	return nil
}

// dialHertzWebSocket creates a WebSocket connection using the hertz client.
func dialHertzWebSocket(t *testing.T, rawURL string) *websocket.Conn {
	t.Helper()

	hc, err := client.NewClient(client.WithDialer(standard.NewDialer()))
	if err != nil {
		t.Fatalf("create hertz client: %v", err)
	}

	// Convert ws:// to http:// for the upgrade request.
	httpURL := strings.Replace(rawURL, "ws://", "http://", 1)
	httpURL = strings.Replace(httpURL, "wss://", "https://", 1)

	req := protocol.AcquireRequest()
	resp := protocol.AcquireResponse()
	req.SetRequestURI(httpURL)
	req.SetMethod(consts.MethodGet)

	upgrader := &websocket.ClientUpgrader{}
	upgrader.PrepareRequest(req)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := hc.Do(ctx, req, resp); err != nil {
		t.Fatalf("hertz ws dial: %v", err)
	}

	conn, err := upgrader.UpgradeResponse(req, resp)
	if err != nil {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
		t.Fatalf("hertz ws upgrade: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
	})

	return conn
}

func TestServerTransportRequiresInitializeFirst(t *testing.T) {
	transport := New()
	baseURL, shutdown := startHertzWebSocketServer(t, transport)
	defer shutdown()

	wsURL := "ws" + strings.TrimPrefix(baseURL, "http")
	conn := dialHertzWebSocket(t, wsURL)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","method":"prompt","params":{}}`)); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}

	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("expected websocket connection to be closed")
	}
	if closeErr, ok := err.(*websocket.CloseError); ok && closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.ClosePolicyViolation)
	}
}

func TestServerTransportAcceptsInitializeFirst(t *testing.T) {
	transport := New()
	baseURL, shutdown := startHertzWebSocketServer(t, transport)
	defer shutdown()

	wsURL := "ws" + strings.TrimPrefix(baseURL, "http")
	conn := dialHertzWebSocket(t, wsURL)

	message := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, err := transport.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if string(got["method"]) != `"initialize"` {
		t.Fatalf("method = %s, want %q", string(got["method"]), `"initialize"`)
	}
	if got["id"] == nil {
		t.Fatal("expected initialize request id to be present")
	}
}

func TestServerTransportHertzHandlerAcceptsInitializeFirst(t *testing.T) {
	transport := New()
	baseURL, shutdown := startHertzWebSocketServer(t, transport)
	defer shutdown()

	wsURL := "ws" + strings.TrimPrefix(baseURL, "http")
	conn := dialHertzWebSocket(t, wsURL)

	message := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, err := transport.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if string(got["method"]) != `"initialize"` {
		t.Fatalf("method = %s, want %q", string(got["method"]), `"initialize"`)
	}
	if got["id"] == nil {
		t.Fatal("expected initialize request id to be present")
	}
}

func TestServerTransportServeConnReturnsAfterReaderError(t *testing.T) {
	transport := New()
	done := make(chan struct{})

	go func() {
		defer close(done)
		transport.ServeConn(context.Background(), &stubMessageConn{readErr: io.EOF})
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		_ = transport.Close()
		t.Fatal("ServeConn did not return after reader error")
	}
}

func TestServerTransportWriteMessageClonesDataBeforeAsyncWrite(t *testing.T) {
	transport := New()
	conn := newBlockingWriteMessageConn()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		transport.ServeConn(ctx, conn)
	}()

	conn.readCh <- readResult{
		messageType: websocket.TextMessage,
		data:        []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
	}

	readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	if _, err := transport.ReadMessage(readCtx); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	payload := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"value":"original"}}`)
	if err := transport.WriteMessage(context.Background(), payload); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}

	copy(payload, []byte(`{"jsonrpc":"2.0","id":1,"result":{"value":"mutated!"}}`))
	close(conn.allowWrite)

	select {
	case written := <-conn.writes:
		if got := string(written); !strings.Contains(got, `"original"`) {
			t.Fatalf("written payload = %s, want original bytes", got)
		}
		if strings.Contains(string(written), `"mutated!"`) {
			t.Fatalf("written payload unexpectedly used mutated bytes: %s", string(written))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket write")
	}

	cancel()
	conn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ServeConn did not stop")
	}
}

func TestServerTransportWriteMessageWithoutActiveConnectionFails(t *testing.T) {
	transport := New()
	defer transport.Close()

	if err := transport.WriteMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0"}`)); !errors.Is(err, acptransport.ErrTransportClosed) {
		t.Fatalf("WriteMessage error = %v, want errors.Is(ErrTransportClosed)", err)
	}
}

func TestServerTransportServeConnUsesConnectionScopedOutbox(t *testing.T) {
	transport := New()
	conn := newScriptedMessageConn()
	done := make(chan struct{})

	go func() {
		defer close(done)
		transport.ServeConn(context.Background(), conn)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for transport.currentConnection() == nil {
		if time.Now().After(deadline) {
			t.Fatal("active connection was not registered")
		}
		time.Sleep(time.Millisecond)
	}

	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	if err := transport.WriteMessage(context.Background(), msg); err != nil {
		t.Fatalf("WriteMessage error = %v", err)
	}

	select {
	case written := <-conn.writes:
		if string(written) != string(msg) {
			t.Fatalf("written message = %s, want %s", string(written), string(msg))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for websocket write")
	}

	if conn.readLimit != defaultMaxReadMessageSize {
		t.Fatalf("read limit = %d, want %d", conn.readLimit, defaultMaxReadMessageSize)
	}

	conn.readCh <- readResult{err: io.EOF}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeConn did not exit")
	}

	if err := transport.WriteMessage(context.Background(), msg); !errors.Is(err, acptransport.ErrTransportClosed) {
		t.Fatalf("WriteMessage after close error = %v, want errors.Is(ErrTransportClosed)", err)
	}
}

func startHertzWebSocketServer(t *testing.T, transport *Transport) (string, func()) {
	t.Helper()

	addr := randomHertzTestAddress(t)
	srv := server.New(server.WithHostPorts(addr))
	srv.NoHijackConnPool = true
	upgrader := &websocket.HertzUpgrader{}
	srv.GET("/ws", func(ctx context.Context, c *app.RequestContext) {
		connID := "test-conn-id"
		c.Response.Header.Set(acptransport.HeaderConnectionID, connID)
		err := upgrader.Upgrade(c, func(conn *websocket.Conn) {
			transport.ServeConn(ctx, conn)
		})
		if err != nil {
			c.SetStatusCode(http.StatusInternalServerError)
		}
	})

	errCh := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		errCh <- srv.Run()
	})

	baseURL := "http://" + addr + "/ws"
	waitForHertzReady(t, baseURL)

	return baseURL, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case err := <-errCh:
			if err != nil && !strings.Contains(err.Error(), "closed network connection") && !errors.Is(err, net.ErrClosed) {
				t.Logf("hertz shutdown: %v", err)
			}
		case <-time.After(time.Second):
		}
	}
}

func randomHertzTestAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func waitForHertzReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not become ready: %s", url)
}
