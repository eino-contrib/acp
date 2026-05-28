// Package ws implements the ACP WebSocket transport.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hclient "github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/google/uuid"
	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/internal/endpoint"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsutil"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	// DefaultPingInterval is the default interval between client-initiated Ping frames.
	DefaultPingInterval = 30 * time.Second

	// DefaultReadTimeout is the default read deadline applied to the WebSocket
	// connection. Approximately 2.5x DefaultPingInterval, tolerating 1-2 missed pongs.
	DefaultReadTimeout = 75 * time.Second
)

// DefaultPongTimeout is a deprecated alias for DefaultReadTimeout.
//
// Deprecated: Use DefaultReadTimeout instead.
const DefaultPongTimeout = DefaultReadTimeout

// WebSocketClientTransport implements the Transport interface over an ACP WebSocket.
type WebSocketClientTransport struct {
	baseURL       string
	endpointPath  string
	hClient       *hclient.Client
	hUpgrader     *websocket.ClientUpgrader
	cookieJar     http.CookieJar
	customHeaders map[string]string

	// Heartbeat configuration
	pingInterval time.Duration
	readTimeout  time.Duration

	connectMu sync.Mutex

	wsConn    websocketConn
	connected bool
	closed    atomic.Bool
	hReq      *protocol.Request
	hResp     *protocol.Response

	inbox    chan json.RawMessage
	done     chan struct{}
	readDone chan struct{} // closed when readLoop exits
	pingDone chan struct{} // closed when pingPump exits (nil if not started)
	once     sync.Once

	localConnID string // local connection ID for log correlation

	termErr atomic.Pointer[error] // stores the first terminal error

	writePermit chan struct{}
}

type websocketConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	SetWriteDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetReadLimit(limit int64)
	SetPongHandler(h func(appData string) error)
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

var _ acptransport.Transport = (*WebSocketClientTransport)(nil)

// ClientTransportOption configures a WebSocket client transport.
type ClientTransportOption func(*WebSocketClientTransport)

// WithCustomHeaders sets custom HTTP headers sent with the WebSocket upgrade
// request. This can be used for authentication tokens or other metadata. The
// provided map is snapshotted at option time; later mutations by the caller
// do not affect the transport. Keys collide-and-override any built-in headers
// with the same name (Set semantics, not Add).
func WithCustomHeaders(headers map[string]string) ClientTransportOption {
	return func(t *WebSocketClientTransport) {
		if len(headers) == 0 {
			t.customHeaders = nil
			return
		}
		snapshot := make(map[string]string, len(headers))
		for k, v := range headers {
			snapshot[k] = v
		}
		t.customHeaders = snapshot
	}
}

// WithEndpointPath sets the WebSocket endpoint path used by the client
// transport. The final request URL is always baseURL + endpoint path.
// The default is "/acp".
func WithEndpointPath(path string) ClientTransportOption {
	return func(t *WebSocketClientTransport) {
		if path != "" {
			t.endpointPath = endpoint.NormalizePath(path)
		}
	}
}

// WithPingInterval sets the interval between client-initiated Ping frames.
// Zero disables the ping pump entirely — this is an advanced/debug-only
// setting; if the peer (Server or Proxy) has ReadTimeout > 0, idle connections
// will be torn down. Negative values are ignored (warn logged). Default: 30s.
func WithPingInterval(d time.Duration) ClientTransportOption {
	return func(t *WebSocketClientTransport) {
		if d < 0 {
			acplog.Warn("[ws] role=client option=WithPingInterval value=%v constraint=\"must be >= 0\" action=ignored", d)
			return
		}
		if d > 0 && d < time.Second {
			acplog.Warn("[ws] role=client option=WithPingInterval value=%v constraint=\"production value should be >= 1s\"", d)
		}
		t.pingInterval = d
	}
}

// WithReadTimeout sets the read deadline applied to the WebSocket connection.
// If no Pong (or data frame) arrives within this window, the connection is
// considered dead. Recommended: >= 2 × PingInterval (default 75s = 2.5 × 30s).
// Zero disables the read deadline (not recommended).
// Negative values are ignored (warn logged). Default: 75s.
//
// Deprecated: WithPongTimeout is an alias kept for backward compatibility.
func WithReadTimeout(d time.Duration) ClientTransportOption {
	return func(t *WebSocketClientTransport) {
		if d < 0 {
			acplog.Warn("[ws] role=client option=WithReadTimeout value=%v constraint=\"must be >= 0\" action=ignored", d)
			return
		}
		if d > 0 && d < time.Second {
			acplog.Warn("[ws] role=client option=WithReadTimeout value=%v constraint=\"production value should be >= 1s\"", d)
		}
		t.readTimeout = d
	}
}

// WithPongTimeout is a deprecated alias for WithReadTimeout.
// The timeout controls the read deadline for the entire connection (any frame
// — including Pong and data frames — refreshes it), not just Pong responses.
//
// Deprecated: Use WithReadTimeout instead.
func WithPongTimeout(d time.Duration) ClientTransportOption {
	return WithReadTimeout(d)
}

// NewWebSocketClientTransport creates a WebSocket client transport.
// baseURL is the server origin (e.g. "ws://localhost:8080").
// The input URL may use either http(s):// or ws(s)://. Only the scheme and
// authority (host[:port]) of baseURL are honored — any path, query, or
// fragment on baseURL is ignored. The final URL is built by combining
// baseURL's origin with the configured endpoint path (default: /acp). Use
// WithEndpointPath only when the server is mounted on a non-default path.
func NewWebSocketClientTransport(baseURL string, opts ...ClientTransportOption) (*WebSocketClientTransport, error) {
	transport := &WebSocketClientTransport{
		baseURL:      normalizeWebSocketURL(baseURL),
		endpointPath: acptransport.DefaultACPEndpointPath,
		pingInterval: DefaultPingInterval,
		readTimeout:  DefaultReadTimeout,
		inbox:        make(chan json.RawMessage, acptransport.DefaultInboxSize),
		done:         make(chan struct{}),
		writePermit:  make(chan struct{}, 1),
	}
	transport.writePermit <- struct{}{}
	for _, opt := range opts {
		opt(transport)
	}
	transport.baseURL = normalizeWSBaseURL(transport.baseURL, transport.endpointPath)

	// Configuration invariant warnings
	if transport.pingInterval == 0 && transport.readTimeout > 0 {
		acplog.Warn("[ws] role=client option=WithReadTimeout value=%v constraint=\"PingInterval must be >0 when ReadTimeout is set\"", transport.readTimeout)
	}
	if transport.pingInterval > 0 && transport.readTimeout > 0 && transport.readTimeout < 2*transport.pingInterval {
		acplog.Warn("[ws] role=client option=WithReadTimeout value=%v constraint=\"ReadTimeout >= 2*PingInterval(%v)\"", transport.readTimeout, transport.pingInterval)
	}

	client, err := hclient.NewClient(hclient.WithDialer(standard.NewDialer()))
	if err != nil {
		return nil, fmt.Errorf("create hertz client: %w", err)
	}

	jar, _ := cookiejar.New(nil)
	transport.hClient = client
	transport.hUpgrader = &websocket.ClientUpgrader{}
	transport.cookieJar = jar
	return transport, nil
}

// Connect establishes the WebSocket connection.
func (t *WebSocketClientTransport) Connect(ctx context.Context) error {
	if t.closed.Load() {
		return acptransport.ErrTransportClosed
	}

	t.connectMu.Lock()
	defer t.connectMu.Unlock()

	if t.closed.Load() {
		return acptransport.ErrTransportClosed
	}

	// If the transport has a terminal error (e.g. pong timeout, ping write
	// failure), it is no longer usable. Return the terminal error instead of
	// silently succeeding when connected is already cleared by markDisconnected.
	if err := t.getTerminalError(); err != nil {
		return err
	}

	if t.connected {
		return nil
	}

	return t.connectWithHertz(ctx)
}

func (t *WebSocketClientTransport) connectWithHertz(ctx context.Context) error {
	req := protocol.AcquireRequest()
	resp := protocol.AcquireResponse()
	httpURL := normalizeHTTPRequestURL(t.baseURL)
	req.SetRequestURI(httpURL)
	req.SetMethod(consts.MethodGet)
	t.attachHertzCookies(req, httpURL)
	t.applyCustomHeaders(req)
	t.hUpgrader.PrepareRequest(req)

	if err := t.hClient.Do(ctx, req, resp); err != nil {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
		return fmt.Errorf("websocket dial: %w", err)
	}

	t.storeHertzCookies(resp, httpURL)

	conn, err := t.hUpgrader.UpgradeResponse(req, resp)
	if err != nil {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
		return fmt.Errorf("websocket upgrade: %w", err)
	}

	t.wsConn = conn
	t.hReq = req
	t.hResp = resp
	t.readDone = make(chan struct{})
	t.localConnID = generateConnID()
	t.connected = true
	t.installHeartbeat(conn)
	t.startReadLoop()
	return nil
}

func (t *WebSocketClientTransport) ensureConnected(ctx context.Context) error {
	return t.Connect(ctx)
}

// startReadLoop launches readLoop in a goroutine that captures panics into
// termErr so they surface to callers via ReadMessage instead of being silently
// swallowed by a generic recover.
func (t *WebSocketClientTransport) startReadLoop() {
	go func() {
		defer close(t.readDone)
		defer t.closeDone()
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("readLoop panic: %v", r)
				acplog.Error("[ws] readLoop recovered from panic: %v", err)
				t.setTerminalError(err)
			}
		}()
		defer t.markDisconnected()
		t.readLoop()
	}()
}

// markDisconnected clears the connected flag so that subsequent Connect()
// calls do not falsely return nil on a dead transport.
func (t *WebSocketClientTransport) markDisconnected() {
	t.connectMu.Lock()
	t.connected = false
	t.connectMu.Unlock()
}

func (t *WebSocketClientTransport) readLoop() {

	// Capture wsConn under lock so we don't race with Close() which nils it.
	t.connectMu.Lock()
	conn := t.wsConn
	t.connectMu.Unlock()
	if conn == nil {
		return
	}

	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// If the connection was closed locally via Close(), do not record
			// the resulting read error as a terminal error.
			if t.closed.Load() {
				conn.Close()
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				acplog.Warn("[ws] role=client local_conn_id=%s reason=read_timeout timeout=%v err=%v", t.localConnID, t.readTimeout, err)
			}
			t.setTerminalError(err)
			// Close the underlying connection to release resources immediately,
			// consistent with pingPump's behavior on write failure.
			conn.Close()
			return
		}
		if messageType != websocket.TextMessage {
			continue // ignore binary frames per spec
		}

		// Refresh read deadline on every data frame so active connections
		// are not killed by the pong timeout.
		if t.readTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(t.readTimeout))
		}

		select {
		case t.inbox <- acptransport.CloneMessage(data):
		case <-t.done:
			return
		}
	}
}

// ReadMessage reads the next JSON-RPC message from the WebSocket.
func (t *WebSocketClientTransport) ReadMessage(ctx context.Context) (json.RawMessage, error) {
	if err := t.ensureConnected(ctx); err != nil {
		return nil, err
	}

	select {
	case msg, ok := <-t.inbox:
		if !ok {
			if err := t.getTerminalError(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		acplog.Access(ctx, "ws-client", acplog.AccessDirectionRecv, msg)
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		if err := t.getTerminalError(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
}

// defaultWriteTimeout caps the time a single WebSocket write may take when the
// caller provided no deadline. This mirrors the server-side defaultSocketWriteTimeout
// in internal/wsserver to prevent slow/stalled peers from blocking writes indefinitely.
const defaultWriteTimeout = 30 * time.Second

// WriteMessage sends a JSON-RPC message over the WebSocket.
func (t *WebSocketClientTransport) WriteMessage(ctx context.Context, data json.RawMessage) error {
	if err := t.ensureConnected(ctx); err != nil {
		return err
	}

	if err := t.acquireWritePermit(ctx); err != nil {
		return err
	}
	defer t.releaseWritePermit()

	// Read wsConn under connectMu so we don't race with Close().
	t.connectMu.Lock()
	conn := t.wsConn
	t.connectMu.Unlock()
	if conn == nil {
		return acptransport.ErrTransportClosed
	}

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(defaultWriteTimeout)
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		acplog.Debug("set websocket write deadline: %v", err)
	}
	defer conn.SetWriteDeadline(time.Time{})
	acplog.Access(ctx, "ws-client", acplog.AccessDirectionSend, data)
	return conn.WriteMessage(websocket.TextMessage, data)
}

// Close closes the WebSocket connection.
func (t *WebSocketClientTransport) Close() error {
	t.closed.Store(true)
	t.closeDone()

	t.connectMu.Lock()
	conn := t.wsConn
	t.wsConn = nil
	readDone := t.readDone
	pingDone := t.pingDone
	t.connected = false
	t.connectMu.Unlock()

	if conn != nil {
		// Best-effort close frame via WriteControl with a short deadline.
		// WriteControl uses the library's internal write lock independently of
		// the application-layer writePermit, so it won't hang behind a long
		// data frame write.
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		if err := conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(wsutil.ControlWriteDeadline)); err != nil {
			acplog.Debug("write websocket close frame: %v", err)
		}
		conn.Close()
	}

	// Wait for readLoop and pingPump to exit before releasing resources.
	if readDone != nil {
		<-readDone
	}
	if pingDone != nil {
		<-pingDone
	}

	t.connectMu.Lock()
	req := t.hReq
	resp := t.hResp
	t.hReq = nil
	t.hResp = nil
	t.connectMu.Unlock()

	if req != nil {
		protocol.ReleaseRequest(req)
	}
	if resp != nil {
		protocol.ReleaseResponse(resp)
	}
	return nil
}

// installHeartbeat sets up the read deadline, PongHandler, and starts the
// pingPump goroutine if PingInterval > 0.
func (t *WebSocketClientTransport) installHeartbeat(conn websocketConn) {
	conn.SetReadLimit(int64(acptransport.DefaultMaxMessageSize))

	if t.readTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(t.readTimeout))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(t.readTimeout))
		})
	}

	if t.pingInterval > 0 {
		t.pingDone = make(chan struct{})
		go t.pingPump(conn)
	}
}

// pingPump periodically sends Ping frames via WriteControl. It exits when the
// done channel is closed or on write failure. On write failure it sets a
// terminal error and closes the underlying connection to unblock readLoop.
func (t *WebSocketClientTransport) pingPump(conn websocketConn) {
	defer close(t.pingDone)

	ticker := time.NewTicker(t.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			// Re-check done after ticker fires to avoid writing to a
			// connection that is being closed locally.
			if t.closed.Load() {
				return
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsutil.ControlWriteDeadline)); err != nil {
				// If closed locally between the check above and WriteControl,
				// do not treat the write failure as a terminal error.
				if t.closed.Load() {
					return
				}
				acplog.Warn("[ws] role=client local_conn_id=%s reason=ping_write_failed err=%v", t.localConnID, err)
				t.setTerminalError(fmt.Errorf("ping write: %w", err))
				conn.Close()
				return
			}
		}
	}
}

func generateConnID() string {
	return uuid.NewString()
}

func (t *WebSocketClientTransport) acquireWritePermit(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-t.done:
		return acptransport.ErrTransportClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-t.writePermit:
		return nil
	}
}

func (t *WebSocketClientTransport) releaseWritePermit() {
	select {
	case t.writePermit <- struct{}{}:
	default:
	}
}

func (t *WebSocketClientTransport) closeDone() {
	t.once.Do(func() {
		close(t.done)
	})
}

func (t *WebSocketClientTransport) setTerminalError(err error) {
	if err != nil {
		t.termErr.CompareAndSwap(nil, &err)
	}
}

func (t *WebSocketClientTransport) getTerminalError() error {
	if p := t.termErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (t *WebSocketClientTransport) attachHertzCookies(req *protocol.Request, rawURL string) {
	if t.cookieJar == nil {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	for _, cookie := range t.cookieJar.Cookies(parsed) {
		req.Header.SetCookie(cookie.Name, cookie.Value)
	}
}

func (t *WebSocketClientTransport) storeHertzCookies(resp *protocol.Response, rawURL string) {
	if t.cookieJar == nil {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	var cookies []*http.Cookie
	resp.Header.VisitAllCookie(func(_, value []byte) {
		cookie, err := http.ParseSetCookie(string(value))
		if err == nil {
			cookies = append(cookies, cookie)
		}
	})
	if len(cookies) > 0 {
		t.cookieJar.SetCookies(parsed, cookies)
	}
}

func (t *WebSocketClientTransport) applyCustomHeaders(req *protocol.Request) {
	for key, value := range t.customHeaders {
		req.Header.Set(key, value)
	}
}

func normalizeWebSocketURL(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}

	// Handle schemeless inputs like "localhost:8080" or "example.com:443/acp".
	// url.Parse would otherwise treat the "localhost:" prefix as the scheme,
	// leaving Host empty and producing a non-dialable URL. Require at least
	// one digit in the port and a host component before the colon so we do
	// not hijack inputs that genuinely start with a scheme.
	if !strings.Contains(rawURL, "://") && hasHostPortPrefix(rawURL) {
		rawURL = "ws://" + rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "":
		parsed.Scheme = "ws"
	}

	return parsed.String()
}

// hasHostPortPrefix reports whether s starts with "<host>:<port>" where host
// is non-empty and port is all digits. Used to detect the schemeless address
// form accepted by net.Dial so we can prepend "ws://" before url.Parse.
func hasHostPortPrefix(s string) bool {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return false
	}
	host := s[:colon]
	if strings.ContainsAny(host, "/?#") {
		return false
	}
	rest := s[colon+1:]
	end := len(rest)
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; c == '/' || c == '?' || c == '#' {
			end = i
			break
		}
	}
	port := rest[:end]
	if port == "" {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	return true
}

func normalizeWSBaseURL(baseURL, endpointPath string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(trimmed, "/")
	}

	// baseURL is defined as the server origin (scheme + authority). Stripping
	// query and fragment here keeps behaviour consistent with the documented
	// "path/query/fragment on baseURL are ignored" contract; otherwise a
	// stray `?x=y` on baseURL would silently change gateway routing / auth.
	parsed.Path = endpoint.NormalizePath(endpointPath)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func normalizeHTTPRequestURL(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	switch strings.ToLower(parsed.Scheme) {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "":
		parsed.Scheme = "http"
	}

	return parsed.String()
}
