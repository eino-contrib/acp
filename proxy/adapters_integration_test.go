package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	acpproxy "github.com/eino-contrib/acp/proxy"
	proxygin "github.com/eino-contrib/acp/proxy/gin"
	proxyhertz "github.com/eino-contrib/acp/proxy/hertz"
	"github.com/eino-contrib/acp/stream"
	acptransport "github.com/eino-contrib/acp/transport"
	ginframework "github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"
)

const (
	adapterContractHeader            = "X-Adapter-Contract"
	adapterContractFirstFrameTimeout = 200 * time.Millisecond
	adapterContractReadTimeout       = 150 * time.Millisecond
	adapterContractMaxMessageSize    = 64
)

type adapterContractFactoryCall struct {
	meta     map[string]string
	streamer *adapterContractStreamer
}

type adapterContractFactory struct {
	calls chan adapterContractFactoryCall
}

func newAdapterContractFactory() *adapterContractFactory {
	return &adapterContractFactory{calls: make(chan adapterContractFactoryCall, 4)}
}

func (f *adapterContractFactory) NewStreamer(_ context.Context, meta map[string]string) (stream.Streamer, error) {
	s := newAdapterContractStreamer()
	metaCopy := make(map[string]string, len(meta))
	for key, value := range meta {
		metaCopy[key] = value
	}
	f.calls <- adapterContractFactoryCall{meta: metaCopy, streamer: s}
	return s, nil
}

type adapterContractStreamer struct {
	inbound    chan []byte
	downstream chan []byte
	closed     chan struct{}
	closeOnce  sync.Once
}

func newAdapterContractStreamer() *adapterContractStreamer {
	return &adapterContractStreamer{
		inbound:    make(chan []byte, 4),
		downstream: make(chan []byte, 4),
		closed:     make(chan struct{}),
	}
}

func (s *adapterContractStreamer) WritePayload(ctx context.Context, payload []byte) error {
	copyOfPayload := append([]byte(nil), payload...)
	select {
	case s.inbound <- copyOfPayload:
		return nil
	case <-s.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *adapterContractStreamer) ReadPayload(ctx context.Context) ([]byte, error) {
	select {
	case payload := <-s.downstream:
		return append([]byte(nil), payload...), nil
	case <-s.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *adapterContractStreamer) Close(string) error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

var _ stream.Streamer = (*adapterContractStreamer)(nil)

type adapterContractHarness struct {
	httpURL      string
	wsURL        string
	ginAborted   *atomic.Bool
	ginTailCalls *atomic.Int32
}

type adapterContractServer struct {
	name  string
	start func(*testing.T, *acpproxy.ACPProxy) adapterContractHarness
}

var adapterContractServers = []adapterContractServer{
	{name: "hertz", start: startHertzAdapterContractServer},
	{name: "gin", start: startGinAdapterContractServer},
}

func TestProxyFrameworkAdapterContract(t *testing.T) {
	for _, tt := range adapterContractServers {
		t.Run(tt.name, func(t *testing.T) {
			factory := newAdapterContractFactory()
			core, err := acpproxy.NewACPProxy(factory,
				acpproxy.WithMetadataExtractor(acpproxy.ForwardHeaders(adapterContractHeader)),
				acpproxy.WithWebSocketFirstFrameTimeout(2*time.Second),
			)
			if err != nil {
				t.Fatalf("NewACPProxy: %v", err)
			}
			t.Cleanup(func() { _ = core.Close() })

			harness := tt.start(t, core)
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Get(harness.httpURL)
			if err != nil {
				t.Fatalf("ordinary HTTP request: %v", err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("ordinary HTTP status = %d, want %d", response.StatusCode, http.StatusBadRequest)
			}
			select {
			case call := <-factory.calls:
				t.Fatalf("ordinary HTTP unexpectedly created streamer with metadata %v", call.meta)
			default:
			}

			if harness.ginAborted != nil {
				if !harness.ginAborted.Load() {
					t.Fatal("Gin middleware did not observe an aborted context")
				}
				if got := harness.ginTailCalls.Load(); got != 0 {
					t.Fatalf("Gin handler after proxy ran %d times, want 0", got)
				}
			}

			headers := http.Header{}
			headers.Set(adapterContractHeader, "forwarded-value")
			conn, upgradeResponse, err := gorillaws.DefaultDialer.Dial(harness.wsURL, headers)
			if err != nil {
				closeAdapterContractResponse(upgradeResponse)
				t.Fatalf("WebSocket dial: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			if got := upgradeResponse.Header.Get(acptransport.HeaderConnectionID); got == "" {
				t.Fatal("successful WebSocket handshake omitted Acp-Connection-Id")
			}

			var call adapterContractFactoryCall
			select {
			case call = <-factory.calls:
			case <-time.After(2 * time.Second):
				t.Fatal("streamer factory was not called after WebSocket upgrade")
			}
			if got := call.meta[adapterContractHeader]; got != "forwarded-value" {
				t.Fatalf("forwarded metadata = %q, want %q (all metadata: %v)", got, "forwarded-value", call.meta)
			}

			frames := []struct {
				messageType int
				payload     []byte
			}{
				{messageType: gorillaws.TextMessage, payload: []byte(`{"jsonrpc":"2.0","method":"contract.text"}`)},
				{messageType: gorillaws.BinaryMessage, payload: []byte{0x00, 0x01, 0x7f, 0x80, 0xff}},
			}
			for _, frame := range frames {
				if err := conn.WriteMessage(frame.messageType, frame.payload); err != nil {
					t.Fatalf("WriteMessage(type=%d): %v", frame.messageType, err)
				}
				select {
				case got := <-call.streamer.inbound:
					if !bytes.Equal(got, frame.payload) {
						t.Fatalf("streamer payload = %v, want %v for message type %d", got, frame.payload, frame.messageType)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("streamer did not receive message type %d", frame.messageType)
				}
			}

			downstream := []byte{0x00, 'd', 'o', 'w', 'n', 0xff}
			select {
			case call.streamer.downstream <- downstream:
			case <-time.After(2 * time.Second):
				t.Fatal("could not queue downstream payload")
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			messageType, gotDownstream, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read downstream WebSocket message: %v", err)
			}
			if messageType != gorillaws.TextMessage {
				t.Fatalf("downstream WebSocket message type = %d, want TextMessage (%d)", messageType, gorillaws.TextMessage)
			}
			if !bytes.Equal(gotDownstream, downstream) {
				t.Fatalf("downstream WebSocket payload = %v, want %v", gotDownstream, downstream)
			}

			if err := core.Close(); err != nil {
				t.Fatalf("Close proxy: %v", err)
			}
			_, rejectedResponse, err := gorillaws.DefaultDialer.Dial(harness.wsURL, headers)
			if err == nil {
				t.Fatal("WebSocket handshake unexpectedly succeeded after proxy Close")
			}
			defer closeAdapterContractResponse(rejectedResponse)
			if rejectedResponse == nil {
				t.Fatalf("closed proxy handshake returned no HTTP response: %v", err)
			}
			if rejectedResponse.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("closed proxy handshake status = %d, want %d", rejectedResponse.StatusCode, http.StatusServiceUnavailable)
			}
		})
	}
}

// TestProxyFrameworkAdapterHeartbeatAndSizeContract runs the same wire-level
// timeout, heartbeat, and size-limit contract against both supported HTTP
// framework adapters. Every assertion crosses a real TCP listener and checks
// the close code observed by a Gorilla WebSocket client.
func TestProxyFrameworkAdapterHeartbeatAndSizeContract(t *testing.T) {
	for _, framework := range adapterContractServers {
		t.Run(framework.name, func(t *testing.T) {
			factory := newAdapterContractFactory()
			core, err := acpproxy.NewACPProxy(factory,
				acpproxy.WithWebSocketFirstFrameTimeout(adapterContractFirstFrameTimeout),
				acpproxy.WithWebSocketReadTimeout(adapterContractReadTimeout),
				acpproxy.WithMaxMessageSize(adapterContractMaxMessageSize),
			)
			if err != nil {
				t.Fatalf("NewACPProxy: %v", err)
			}
			t.Cleanup(func() { _ = core.Close() })

			harness := framework.start(t, core)

			t.Run("first frame timeout closes with 4001", func(t *testing.T) {
				conn, _ := dialAdapterContractConnection(t, harness, factory)
				defer conn.Close()

				assertAdapterContractCloseCode(t, conn, 2*time.Second, acptransport.WSCloseFirstFrameTimeout)
			})

			t.Run("Ping before first frame does not extend timeout", func(t *testing.T) {
				conn, _ := dialAdapterContractConnection(t, harness, factory)
				defer conn.Close()

				var pingCount atomic.Int32
				stopPings := make(chan struct{})
				pingsDone := make(chan struct{})
				go func() {
					defer close(pingsDone)
					ticker := time.NewTicker(adapterContractFirstFrameTimeout / 8)
					defer ticker.Stop()
					for {
						select {
						case <-stopPings:
							return
						case <-ticker.C:
							if err := conn.WriteControl(gorillaws.PingMessage, []byte("pre-first-frame"), time.Now().Add(100*time.Millisecond)); err != nil {
								return
							}
							pingCount.Add(1)
						}
					}
				}()
				defer func() {
					close(stopPings)
					<-pingsDone
				}()

				// Pings continue for the whole wait. If any Ping refreshed the
				// first-frame deadline, this read would hit its one-second client
				// deadline instead of observing the proxy's 4001 close frame.
				assertAdapterContractCloseCode(t, conn, time.Second, acptransport.WSCloseFirstFrameTimeout)
				if got := pingCount.Load(); got < 3 {
					t.Fatalf("sent %d Pings before close, want at least 3", got)
				}
			})

			t.Run("Ping after first frame keeps connection alive", func(t *testing.T) {
				conn, streamer := dialAdapterContractConnection(t, harness, factory)
				defer conn.Close()

				warmup := []byte(`{"jsonrpc":"2.0","method":"warmup"}`)
				writeAndAwaitAdapterContractInbound(t, conn, streamer, warmup)

				pingEvery := adapterContractReadTimeout / 5
				keepAliveFor := 3 * adapterContractReadTimeout
				for deadline := time.Now().Add(keepAliveFor); time.Now().Before(deadline); {
					time.Sleep(pingEvery)
					if err := conn.WriteControl(gorillaws.PingMessage, []byte("post-first-frame"), time.Now().Add(100*time.Millisecond)); err != nil {
						t.Fatalf("Ping while keeping connection alive: %v", err)
					}
				}

				// The idle interval above is three times the configured read
				// timeout. A second data frame reaching the Streamer proves that
				// only the intervening Ping frames kept the connection alive.
				probe := []byte(`{"jsonrpc":"2.0","method":"still-alive"}`)
				writeAndAwaitAdapterContractInbound(t, conn, streamer, probe)
			})

			t.Run("no Ping after first frame closes with 1001", func(t *testing.T) {
				conn, streamer := dialAdapterContractConnection(t, harness, factory)
				defer conn.Close()

				warmup := []byte(`{"jsonrpc":"2.0","method":"warmup"}`)
				writeAndAwaitAdapterContractInbound(t, conn, streamer, warmup)
				assertAdapterContractCloseCode(t, conn, 2*time.Second, gorillaws.CloseGoingAway)
			})

			t.Run("oversized northbound frame closes with 1009", func(t *testing.T) {
				conn, streamer := dialAdapterContractConnection(t, harness, factory)
				defer conn.Close()

				payload := bytes.Repeat([]byte{0xa5}, adapterContractMaxMessageSize+1)
				if err := conn.WriteMessage(gorillaws.BinaryMessage, payload); err != nil {
					t.Fatalf("write oversized northbound frame: %v", err)
				}
				assertAdapterContractCloseCode(t, conn, 2*time.Second, gorillaws.CloseMessageTooBig)
				select {
				case got := <-streamer.inbound:
					t.Fatalf("oversized northbound payload reached Streamer: %d bytes", len(got))
				default:
				}
			})

			t.Run("oversized downstream payload closes with 1009", func(t *testing.T) {
				conn, streamer := dialAdapterContractConnection(t, harness, factory)
				defer conn.Close()

				warmup := []byte(`{"jsonrpc":"2.0","method":"warmup"}`)
				writeAndAwaitAdapterContractInbound(t, conn, streamer, warmup)
				payload := bytes.Repeat([]byte{0x5a}, adapterContractMaxMessageSize+1)
				select {
				case streamer.downstream <- payload:
				case <-time.After(time.Second):
					t.Fatal("could not queue oversized downstream payload")
				}
				assertAdapterContractCloseCode(t, conn, 2*time.Second, gorillaws.CloseMessageTooBig)
			})
		})
	}
}

func TestProxyGinMiddlewareContract(t *testing.T) {
	factory := newAdapterContractFactory()
	core, err := acpproxy.NewACPProxy(factory)
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })

	t.Run("already aborted request skips proxy", func(t *testing.T) {
		ginframework.SetMode(ginframework.TestMode)
		router := ginframework.New()
		router.Any("/", func(c *ginframework.Context) {
			c.AbortWithStatus(http.StatusUnauthorized)
		}, proxygin.New(core))
		recorder := serveGinAdapterRequest(router)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
		}
	})

	t.Run("proxy aborts later handler", func(t *testing.T) {
		var tailCalls atomic.Int32
		router := ginframework.New()
		router.Any("/", proxygin.New(core), func(c *ginframework.Context) {
			tailCalls.Add(1)
			c.Status(http.StatusTeapot)
		})
		recorder := serveGinAdapterRequest(router)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
		}
		if got := tailCalls.Load(); got != 0 {
			t.Fatalf("later handler calls = %d, want 0", got)
		}
	})

	select {
	case call := <-factory.calls:
		t.Fatalf("middleware-only requests unexpectedly created streamer with metadata %v", call.meta)
	default:
	}
}

func TestProxyHertzHandlerAbortsRemainingChain(t *testing.T) {
	core, err := acpproxy.NewACPProxy(newAdapterContractFactory())
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	c := &app.RequestContext{}
	c.Request.Header.SetMethod(http.MethodGet)
	proxyhertz.New(core)(context.Background(), c)
	if !c.IsAborted() {
		t.Fatal("proxy Hertz handler did not abort the remaining handler chain")
	}
}

type failingAdapterFactory struct{}

func (failingAdapterFactory) NewStreamer(context.Context, map[string]string) (stream.Streamer, error) {
	return nil, errors.New("factory must not run")
}

func TestProxyGinUpgraderPanicReleasesAdmissionAndRepanics(t *testing.T) {
	tests := []struct {
		name     string
		upgrader func(any) gorillaws.Upgrader
	}{
		{
			name: "CheckOrigin",
			upgrader: func(value any) gorillaws.Upgrader {
				return gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { panic(value) }}
			},
		},
		{
			name: "Error",
			upgrader: func(value any) gorillaws.Upgrader {
				return gorillaws.Upgrader{
					CheckOrigin: func(*http.Request) bool { return false },
					Error:       func(http.ResponseWriter, *http.Request, int, error) { panic(value) },
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, err := acpproxy.NewACPProxy(failingAdapterFactory{}, acpproxy.WithMaxConcurrentConnections(1))
			if err != nil {
				t.Fatalf("NewACPProxy: %v", err)
			}
			panicValue := &struct{ label string }{label: tt.name}
			recorder := httptest.NewRecorder()
			c, _ := ginframework.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			c.Request.Header.Set("Connection", "Upgrade")
			c.Request.Header.Set("Upgrade", "websocket")
			c.Request.Header.Set("Sec-WebSocket-Version", "13")
			c.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			c.Request.Header.Set("Origin", "https://panic.example")

			var recovered any
			func() {
				defer func() { recovered = recover() }()
				proxygin.New(core, proxygin.WithUpgrader(tt.upgrader(panicValue)))(c)
			}()
			if recovered != panicValue {
				t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
			}

			admission, err := core.Admit(context.Background(), nil)
			if err != nil {
				t.Fatalf("Admit after upgrader panic: %v", err)
			}
			admission.Abort()
			if err := core.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
		})
	}
}

func serveGinAdapterRequest(handler http.Handler) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	return recorder
}

func startHertzAdapterContractServer(t *testing.T, core *acpproxy.ACPProxy) adapterContractHarness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for Hertz: %v", err)
	}
	srv := server.New(server.WithListener(listener), server.WithTransport(standard.NewTransporter))
	srv.NoHijackConnPool = true
	srv.Any(acpproxy.DefaultEndpoint, proxyhertz.New(core))
	go func() { _ = srv.Run() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = listener.Close()
	})

	httpURL := "http://" + listener.Addr().String() + acpproxy.DefaultEndpoint
	return adapterContractHarness{httpURL: httpURL, wsURL: "ws://" + listener.Addr().String() + acpproxy.DefaultEndpoint}
}

func startGinAdapterContractServer(t *testing.T, core *acpproxy.ACPProxy) adapterContractHarness {
	t.Helper()
	ginframework.SetMode(ginframework.TestMode)
	aborted := &atomic.Bool{}
	tailCalls := &atomic.Int32{}
	router := ginframework.New()
	router.Use(func(c *ginframework.Context) {
		c.Next()
		if c.IsAborted() {
			aborted.Store(true)
		}
	})
	router.Any(acpproxy.DefaultEndpoint, proxygin.New(core), func(c *ginframework.Context) {
		tailCalls.Add(1)
		c.Status(http.StatusTeapot)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for Gin: %v", err)
	}
	httpServer := &http.Server{Handler: router}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		_ = listener.Close()
	})

	httpURL := "http://" + listener.Addr().String() + acpproxy.DefaultEndpoint
	return adapterContractHarness{
		httpURL:      httpURL,
		wsURL:        "ws://" + listener.Addr().String() + acpproxy.DefaultEndpoint,
		ginAborted:   aborted,
		ginTailCalls: tailCalls,
	}
}

func dialAdapterContractConnection(
	t *testing.T,
	harness adapterContractHarness,
	factory *adapterContractFactory,
) (*gorillaws.Conn, *adapterContractStreamer) {
	t.Helper()
	conn, response, err := gorillaws.DefaultDialer.Dial(harness.wsURL, nil)
	if err != nil {
		closeAdapterContractResponse(response)
		t.Fatalf("WebSocket dial: %v", err)
	}
	closeAdapterContractResponse(response)

	select {
	case call := <-factory.calls:
		return conn, call.streamer
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		t.Fatal("streamer factory was not called after WebSocket upgrade")
		return nil, nil
	}
}

func writeAndAwaitAdapterContractInbound(
	t *testing.T,
	conn *gorillaws.Conn,
	streamer *adapterContractStreamer,
	payload []byte,
) {
	t.Helper()
	if err := conn.WriteMessage(gorillaws.TextMessage, payload); err != nil {
		t.Fatalf("write northbound data frame: %v", err)
	}
	select {
	case got := <-streamer.inbound:
		if !bytes.Equal(got, payload) {
			t.Fatalf("streamer payload = %q, want %q", got, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("streamer did not receive northbound data frame")
	}
}

func assertAdapterContractCloseCode(t *testing.T, conn *gorillaws.Conn, timeout time.Duration, want int) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set WebSocket read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("WebSocket read returned nil, want close code %d", want)
	}
	var closeErr *gorillaws.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("WebSocket read error = %T %v, want close code %d", err, err, want)
	}
	if closeErr.Code != want {
		t.Fatalf("WebSocket close code = %d (%q), want %d", closeErr.Code, closeErr.Text, want)
	}
}

func closeAdapterContractResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
