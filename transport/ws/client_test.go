package ws

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
	"github.com/cloudwego/hertz/pkg/app/server"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/internal/wsserver"
	acptransport "github.com/eino-contrib/acp/transport"
	"github.com/hertz-contrib/websocket"
)

type testLogger struct{}

func (testLogger) Debug(string, ...interface{})                     {}
func (testLogger) Info(string, ...interface{})                      {}
func (testLogger) Warn(string, ...interface{})                      {}
func (testLogger) Error(string, ...interface{})                     {}
func (testLogger) CtxDebug(context.Context, string, ...interface{}) {}
func (testLogger) CtxInfo(context.Context, string, ...interface{})  {}
func (testLogger) CtxWarn(context.Context, string, ...interface{})  {}
func (testLogger) CtxError(context.Context, string, ...interface{}) {}

type blockingWriteMessageConn struct {
	readCh       chan struct{}
	writes       chan []byte
	closed       chan struct{}
	writeStarted chan struct{}
	allowWrite   chan struct{}
}

func newBlockingWriteMessageConn() *blockingWriteMessageConn {
	return &blockingWriteMessageConn{
		readCh:       make(chan struct{}),
		writes:       make(chan []byte, 1),
		closed:       make(chan struct{}),
		writeStarted: make(chan struct{}, 1),
		allowWrite:   make(chan struct{}),
	}
}

func (c *blockingWriteMessageConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, io.EOF
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

func (c *blockingWriteMessageConn) SetReadLimit(int64) {}

func (c *blockingWriteMessageConn) SetWriteDeadline(time.Time) error { return nil }

func TestNewWebSocketClientTransportWithLogger(t *testing.T) {
	t.Parallel()

	logger := testLogger{}
	transport, err := NewWebSocketClientTransport("ws://example.invalid", WithLogger(logger))
	if err != nil {
		t.Fatalf("NewWebSocketClientTransport: %v", err)
	}
	defer transport.Close()

	if got := acplog.OrDefault(transport.logger); got != logger {
		t.Fatalf("logger = %#v, want %#v", got, logger)
	}
}

func TestWebSocketClientTransportUsesHertzClientByDefault(t *testing.T) {
	serverTransport := wsserver.New()
	baseURL, shutdown := startHertzWebSocketServer(t, serverTransport)
	defer shutdown()

	clientTransport, err := NewWebSocketClientTransport(baseURL)
	if err != nil {
		t.Fatalf("NewWebSocketClientTransport: %v", err)
	}
	if clientTransport.hClient == nil || clientTransport.hUpgrader == nil {
		t.Fatal("expected hertz websocket client dependencies to be initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := clientTransport.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer clientTransport.Close()

	initialize := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err := clientTransport.WriteMessage(ctx, initialize); err != nil {
		t.Fatalf("WriteMessage initialize: %v", err)
	}

	serverMsg, err := serverTransport.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("server ReadMessage: %v", err)
	}
	if string(serverMsg) != string(initialize) {
		t.Fatalf("server received %s, want %s", string(serverMsg), string(initialize))
	}

	response := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	if err := serverTransport.WriteMessage(ctx, response); err != nil {
		t.Fatalf("server WriteMessage: %v", err)
	}

	clientMsg, err := clientTransport.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("client ReadMessage: %v", err)
	}
	if string(clientMsg) != string(response) {
		t.Fatalf("client received %s, want %s", string(clientMsg), string(response))
	}
}

func startHertzWebSocketServer(t *testing.T, transport *wsserver.Transport) (string, func()) {
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
