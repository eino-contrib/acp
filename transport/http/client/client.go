package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/peerstate"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
	acphttp "github.com/eino-contrib/acp/transport/http"
)

// ACP JSON-RPC method names that trigger state transitions.
const (
	methodInitialize = acptransport.MethodInitialize
)

// maxErrorBodySize is the maximum number of bytes read from an HTTP error
// response body. Error bodies are diagnostic — keeping them bounded prevents
// memory issues when a server returns unexpectedly large error payloads.
const maxErrorBodySize = 4096

// readErrorBody reads up to maxErrorBodySize bytes from an error response body
// and returns it as a string, truncating with an ellipsis if exceeded.
// If the read fails before any bytes are read, the returned string reports the
// read failure so the caller's error message is not silently empty.
func readErrorBody(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBodySize+1))
	if err != nil && len(data) == 0 {
		return fmt.Sprintf("(failed to read error body: %v)", err)
	}
	if len(data) > maxErrorBodySize {
		return string(data[:maxErrorBodySize]) + "... (truncated)"
	}
	return string(data)
}

// maxJSONResponseSize is the maximum size of a single JSON-RPC response read
// from the non-SSE fallback path. A single response should be much smaller
// than the 50MB SSE stream limit; 8MB is generous for any well-formed reply.
const maxJSONResponseSize = 8 * 1024 * 1024

// ClientTransportOption configures a ClientTransport.
type ClientTransportOption func(*ClientTransport)

// WithClientLogger sets the logger used by the HTTP client transport.
func WithClientLogger(logger acplog.Logger) ClientTransportOption {
	return func(t *ClientTransport) {
		if logger == nil {
			logger = acplog.Default()
		}
		t.logger = logger
	}
}

// WithHTTPClient sets the HTTP client used for all requests.
func WithHTTPClient(client *http.Client) ClientTransportOption {
	return func(t *ClientTransport) {
		if client != nil {
			t.httpClient = ensureHTTPClientHasCookieJar(client)
		}
	}
}

// WithInboxSize sets the buffered channel capacity for incoming messages.
// Default is 100.
func WithInboxSize(size int) ClientTransportOption {
	return func(t *ClientTransport) {
		if size > 0 {
			t.inboxSize = size
		}
	}
}

// WithClientEndpointPath sets the HTTP endpoint path used by the client
// transport. The final request URL is always baseURL + endpoint path.
func WithClientEndpointPath(path string) ClientTransportOption {
	return func(t *ClientTransport) {
		if path != "" {
			t.endpointPath = normalizeClientEndpointPath(path)
		}
	}
}

func WithCustomHeaders(headers map[string]string) ClientTransportOption {
	return func(t *ClientTransport) {
		t.customHeaders = headers
	}
}

// sseReconnectConfig controls automatic reconnection for GET SSE listeners.
type sseReconnectConfig struct {
	maxRetries int           // negative for unlimited retries
	baseDelay  time.Duration // initial backoff delay
	maxDelay   time.Duration // upper bound for backoff delay
}

const (
	defaultSSEReconnectBaseDelay = time.Second
	defaultSSEReconnectMaxDelay  = 30 * time.Second
)

// ensureSSEReconnect initializes the reconnect config with default backoff if
// not already set. Called from each WithSSEReconnect* option so any of them
// enables reconnection.
func (t *ClientTransport) ensureSSEReconnect() *sseReconnectConfig {
	if t.reconnect == nil {
		t.reconnect = &sseReconnectConfig{
			baseDelay: defaultSSEReconnectBaseDelay,
			maxDelay:  defaultSSEReconnectMaxDelay,
		}
	}
	return t.reconnect
}

// WithSSEReconnect enables automatic reconnection for GET SSE listeners with
// default exponential backoff (1s → 30s, unlimited retries). Use
// WithSSEReconnectMaxAttempts or WithSSEReconnectBackoff to tune.
func WithSSEReconnect() ClientTransportOption {
	return func(t *ClientTransport) {
		cfg := t.ensureSSEReconnect()
		cfg.maxRetries = -1
	}
}

// WithSSEReconnectMaxAttempts caps the number of consecutive reconnect
// attempts before the client gives up and surfaces the disconnect. A negative
// value means unlimited retries; 0 disables reconnect entirely.
func WithSSEReconnectMaxAttempts(n int) ClientTransportOption {
	return func(t *ClientTransport) {
		cfg := t.ensureSSEReconnect()
		cfg.maxRetries = n
	}
}

// WithSSEReconnectBackoff overrides the reconnect backoff window. base is the
// initial delay; max caps the exponential growth. Non-positive values fall
// back to the defaults (1s base, 30s max).
func WithSSEReconnectBackoff(base, max time.Duration) ClientTransportOption {
	return func(t *ClientTransport) {
		cfg := t.ensureSSEReconnect()
		if base > 0 {
			cfg.baseDelay = base
		}
		if max > 0 {
			cfg.maxDelay = max
		}
	}
}

// ClientTransport implements the Transport interface for the client side of
// ACP Streamable HTTP.
//
// Protocol flow:
//  1. Client POSTs the "initialize" request to {baseURL}. The server responds
//     with an Acp-Connection-Id header which the client stores.
//  2. All subsequent POSTs include the Acp-Connection-Id header.
//  3. When the server responds to "session/new", it returns an Acp-Session-Id
//     header which the client stores and includes on session-scoped methods.
//  4. JSON-RPC requests (messages with an "id" field) receive SSE-streamed
//     responses. The stream carries the final JSON-RPC response for that
//     request. Server-initiated notifications arrive on the GET SSE listener.
//  5. JSON-RPC notifications and responses sent by the client receive HTTP 202
//     with no body.
//  6. The client may open a GET SSE listener to receive server-initiated
//     messages (notifications/requests pushed by the server).
//
// All messages read from SSE streams or the GET listener are delivered through
// a single inbox channel, which ReadMessage drains.
type ClientTransport struct {
	baseURL       string
	endpointPath  string
	httpClient    *http.Client
	inboxSize     int
	logger        acplog.Logger
	customHeaders map[string]string
	reconnect     *sseReconnectConfig // nil means no reconnect

	// inbox receives all inbound JSON-RPC messages (from POST SSE responses
	// and from the GET SSE listener).
	inbox chan json.RawMessage
	done  chan struct{}

	closeOnce sync.Once
	closeErr  error

	state        *peerstate.State
	listeners    clientListenerRegistry
	activeBodies activeBodyRegistry
}

type clientListener struct {
	sessionID string
	cancel    context.CancelFunc
	body      io.Closer
}

func closeClientListener(listener *clientListener, logger acplog.Logger) {
	if listener == nil {
		return
	}
	if listener.cancel != nil {
		listener.cancel()
	}
	if listener.body != nil {
		if err := listener.body.Close(); err != nil {
			if logger == nil {
				logger = acplog.Default()
			}
			logger.Error("close HTTP listener body for session %s: %v", listener.sessionID, err)
		}
	}
}

var _ acptransport.Transport = (*ClientTransport)(nil)

// NewClientTransport creates a new ACP Streamable HTTP client transport.
//
// baseURL is the server origin (e.g. "http://localhost:8080").
// Any path on baseURL is ignored; the final request
// URL is built by combining baseURL with the configured endpoint path. Use
// WithClientEndpointPath only when the server is mounted on a non-default path.
func NewClientTransport(baseURL string, opts ...ClientTransportOption) *ClientTransport {
	t := &ClientTransport{
		baseURL:      strings.TrimSpace(baseURL),
		endpointPath: acphttp.DefaultACPEndpointPath,
		httpClient:   newDefaultHTTPClient(),
		inboxSize:    acptransport.DefaultInboxSize,
		logger:       acplog.Default(),
		state:        peerstate.New(),
		listeners:    newClientListenerRegistry(),
		activeBodies: newActiveBodyRegistry(),
	}
	for _, opt := range opts {
		opt(t)
	}
	t.baseURL = normalizeACPBaseURL(t.baseURL, t.endpointPath)
	t.inbox = make(chan json.RawMessage, t.inboxSize)
	t.done = make(chan struct{})
	return t
}

func normalizeACPBaseURL(baseURL, endpointPath string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(trimmed, "/")
	}

	parsed.Path = normalizeClientEndpointPath(endpointPath)
	return parsed.String()
}

func normalizeClientEndpointPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed == "/" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// ReadMessage returns the next inbound JSON-RPC message.
//
// Messages arrive from two sources:
//   - SSE streams returned by POST requests (responses to JSON-RPC requests).
//   - The optional GET SSE listener for server-initiated messages.
//
// ReadMessage blocks until a message is available, the context is cancelled,
// or the transport is closed.
func (t *ClientTransport) ReadMessage(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg, ok := <-t.inbox:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

// WriteMessage sends a JSON-RPC message to the server via HTTP POST.
//
// The message is inspected to determine its type:
//   - Requests (have "id" and "method"): POST returns an SSE stream. The
//     stream is consumed in a background goroutine and all messages are
//     delivered to the inbox.
//   - Notifications (no "id", have "method") and responses (have "id", no
//     "method"): POST returns 202 Accepted with no body.
//
// Special handling:
//   - The first request must be "initialize". The Acp-Connection-Id is
//     extracted from the response and stored for subsequent requests.
//   - When a "session/new" response is received, the Acp-Session-Id is
//     extracted from the response headers and stored.
func (t *ClientTransport) WriteMessage(ctx context.Context, data json.RawMessage) error {
	select {
	case <-t.done:
		return acptransport.ErrTransportClosed
	default:
	}

	// Parse the outgoing message to understand its shape.
	msg, err := jsonrpc.ParseEnvelope(data)
	if err != nil {
		acplog.OrDefault(t.logger).Error("parse outbound HTTP message metadata: %v", err)
	}
	t.rememberProtocolVersionFromInitialize(msg.Method, msg.Params.ProtocolVersion)

	isRequest := msg.IsRequest()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.applyCustomHeaders(req)

	// Attach connection and session IDs when available.
	if connectionID := t.state.ConnectionID(); connectionID != "" {
		req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	}
	if protocolVersion := t.protocolVersionForOutboundMessage(msg.Method); protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}
	if sessID := t.sessionIDForOutboundMessage(msg.ID, msg.Method, msg.Params.Raw); sessID != "" {
		req.Header.Set(acptransport.HeaderSessionID, sessID)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", t.baseURL, err)
	}

	// Handle error status codes.
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		bodyStr := readErrorBody(resp.Body)
		return fmt.Errorf("POST %s: HTTP %d: %s", t.baseURL, resp.StatusCode, bodyStr)
	}

	// Extract Acp-Connection-Id from response (set on initialize response).
	if hdr := resp.Header.Get(acptransport.HeaderConnectionID); hdr != "" {
		t.state.SetConnectionID(hdr)
	}
	// Extract Acp-Session-Id from response (set on session/new response).
	if hdr := resp.Header.Get(acptransport.HeaderSessionID); hdr != "" {
		t.state.SetSessionID(hdr)
	}

	// If 202 Accepted or the message was not a request, there is no SSE
	// stream to read — the server accepted the notification/response.
	if resp.StatusCode == http.StatusAccepted || !isRequest {
		resp.Body.Close()
		return nil
	}

	// The server returned an SSE stream for this request. Consume it in a
	// background goroutine so WriteMessage does not block the caller.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		safe.GoWithLogger(acplog.OrDefault(t.logger), func() {
			t.consumeRequestSSEStream(msg.ID, resp.Body)
		})
		return nil
	}

	// Fallback: single JSON response (Content-Type: application/json).
	// Read the entire body as a single message. Read one extra byte so we
	// can distinguish a body that fits within the limit from one that was
	// silently truncated by io.LimitReader (which returns EOF without error).
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseSize+1))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxJSONResponseSize {
		return fmt.Errorf("response exceeded max JSON size (%d bytes)", maxJSONResponseSize)
	}
	if len(body) > 0 {
		if !t.enqueue(json.RawMessage(body)) {
			return acptransport.ErrTransportClosed
		}
	}
	return nil
}

// SessionListenerHook returns the start/stop hooks that internal/connspi
// uses to wire up GET SSE listeners. The return type lives in
// internal/connspi so external callers cannot consume it; the method is
// public only because the cross-package type assertion in internal/connspi
// requires an exported method name.
func (t *ClientTransport) SessionListenerHook() *connspi.SessionListenerHook {
	return &connspi.SessionListenerHook{
		Start: func(ctx context.Context, sessionID string) error {
			if sessionID == "" {
				return fmt.Errorf("session ID is required to start a listener")
			}
			return t.startListener(ctx, sessionID)
		},
		Stop: func() {
			t.listeners.StopAll(acplog.OrDefault(t.logger))
		},
	}
}

func (t *ClientTransport) startListener(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session ID is required to start a listener")
	}

	// Detach from the caller's RPC context so the long-lived SSE listener is
	// not cancelled when the NewSession/LoadSession call completes or times
	// out. Values (e.g. trace metadata) are preserved; cancellation is not.
	listenerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	body, err := t.dialSSEListener(listenerCtx, sessionID)
	if err != nil {
		cancel()
		return err
	}

	select {
	case <-t.done:
		body.Close()
		cancel()
		return acptransport.ErrTransportClosed
	default:
	}

	listener := &clientListener{
		sessionID: sessionID,
		cancel:    cancel,
		body:      body,
	}
	replaced := t.listeners.Replace(sessionID, listener)
	closeClientListener(replaced, acplog.OrDefault(t.logger))

	safe.GoWithLogger(acplog.OrDefault(t.logger), func() {
		t.readSSELoopWithReconnect(listenerCtx, cancel, sessionID, body)
	})
	return nil
}

// dialSSEListener performs a single GET SSE connection attempt. On success it
// returns the response body for stream reading. The caller owns closing it.
func (t *ClientTransport) dialSSEListener(ctx context.Context, sessionID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create GET SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	t.applyCustomHeaders(req)

	if connectionID := t.state.ConnectionID(); connectionID != "" {
		req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	}
	if protocolVersion := t.ProtocolVersion(); protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}
	if sessionID != "" {
		req.Header.Set(acptransport.HeaderSessionID, sessionID)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET SSE connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		bodyStr := readErrorBody(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET SSE connect: HTTP %d: %s", resp.StatusCode, bodyStr)
	}
	return resp.Body, nil
}

// readSSELoopWithReconnect reads SSE events from a GET listener. When the
// stream disconnects and reconnect is configured, it retries with exponential
// backoff. It owns the lifecycle of the listener registry entry.
//
// If the stream cannot be kept alive — no reconnect configured and the stream
// ended, or reconnect retries are exhausted — the listener is torn down for
// this session only; the transport remains usable for other sessions and for
// POSTs on this session. See failListener for the detailed failure semantics.
func (t *ClientTransport) readSSELoopWithReconnect(ctx context.Context, cancel context.CancelFunc, sessionID string, body io.ReadCloser) {
	// Use a variable so the deferred cleanup always targets the latest body.
	currentBody := body
	defer func() {
		t.listeners.RemoveIfBody(sessionID, currentBody)
	}()

	// First read pass — use the already-opened body.
	if t.readSSEOnce(body) {
		return // transport closed
	}

	cfg := t.reconnect
	if cfg == nil {
		// No reconnect configured. The stream ended and we have no way to
		// recover, so surface it by closing the transport.
		t.failListener(ctx, sessionID, fmt.Errorf("SSE listener disconnected and reconnect is not configured"))
		return
	}

	delay := cfg.baseDelay
	for attempt := 0; cfg.maxRetries < 0 || attempt < cfg.maxRetries; attempt++ {
		// Check for cancellation / transport shutdown before sleeping.
		select {
		case <-ctx.Done():
			return
		case <-t.done:
			return
		default:
		}

		acplog.OrDefault(t.logger).CtxInfo(ctx, "GET SSE listener for session %s disconnected, reconnecting in %v (attempt %d)", sessionID, delay, attempt+1)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		case <-t.done:
			return
		}

		newBody, err := t.dialSSEListener(ctx, sessionID)
		if err != nil {
			if ctx.Err() != nil || t.isClosed() {
				return
			}
			acplog.OrDefault(t.logger).CtxError(ctx, "GET SSE reconnect for session %s failed: %v", sessionID, err)
			delay = nextBackoff(delay, cfg.maxDelay)
			continue
		}

		// Update the listener registry so Close/StopAll can close this body
		// and cancel the context.
		listener := &clientListener{
			sessionID: sessionID,
			cancel:    cancel,
			body:      newBody,
		}
		t.listeners.Replace(sessionID, listener)
		currentBody = newBody

		// Reset backoff on successful reconnect.
		delay = cfg.baseDelay
		attempt = -1 // will be incremented to 0 at loop top

		if t.readSSEOnce(newBody) {
			return // transport closed
		}
	}

	t.failListener(ctx, sessionID, fmt.Errorf("max reconnect retries (%d) exhausted", cfg.maxRetries))
}

// failListener surfaces a permanent GET SSE listener failure by unregistering
// the listener for the affected session and logging the cause. The transport
// remains usable for other sessions and for outbound POSTs on this session;
// the upper layer can observe that server-initiated deliveries for the
// session have stopped by the absence of pushes (and, server-side, by the
// per-session pending queue filling up).
//
// No-op when the context is already cancelled (listener explicitly stopped)
// or the transport is already closing.
func (t *ClientTransport) failListener(ctx context.Context, sessionID string, cause error) {
	if ctx.Err() != nil || t.isClosed() {
		return
	}
	acplog.OrDefault(t.logger).CtxError(ctx, "GET SSE listener for session %s permanently failed: %v; server-initiated messages for this session will stop", sessionID, cause)
	t.listeners.StopSession(sessionID, acplog.OrDefault(t.logger))
}

// readSSEOnce reads a single SSE stream until it ends or the transport is
// closed. Returns true if the transport is closed (caller should stop).
func (t *ClientTransport) readSSEOnce(body io.ReadCloser) (transportClosed bool) {
	defer body.Close()

	stopped := false
	_ = t.readSSE(body, func(msg json.RawMessage) bool {
		if !t.enqueue(msg) {
			stopped = true
			return true
		}
		return false
	})
	return stopped || t.isClosed()
}

// nextBackoff doubles the delay, adds random jitter [0, delay/2), and caps at maxDelay.
// The jitter prevents thundering herd when many clients reconnect simultaneously.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	// Add jitter: [0, next/2).
	if next > 0 {
		next += time.Duration(rand.Int64N(int64(next / 2)))
	}
	if next > max {
		next = max
	}
	return next
}

// ConnectionID returns the current Acp-Connection-Id, or empty if the
// initialize handshake has not completed.
func (t *ClientTransport) ConnectionID() string {
	return t.state.ConnectionID()
}

// SessionID returns the current Acp-Session-Id, or empty if no session
// has been created.
func (t *ClientTransport) SessionID() string {
	return t.state.SessionID()
}

// ProtocolVersion returns the currently negotiated ACP protocol version, or
// empty if the initialize handshake has not completed yet.
func (t *ClientTransport) ProtocolVersion() string {
	return t.state.ProtocolVersion()
}

// Close shuts down the transport. It stops active listeners, signals
// background goroutines to exit, and force-closes active response streams.
func (t *ClientTransport) Close() error {
	t.closeOnce.Do(func() {
		var closeErrs []error
		close(t.done)
		t.listeners.StopAll(acplog.OrDefault(t.logger))
		if err := t.closeRemoteConnection(); err != nil {
			closeErrs = append(closeErrs, err)
			acplog.OrDefault(t.logger).Error("close remote HTTP connection: %v", err)
		}
		// Force-close active SSE bodies so that background goroutines unblock
		// and stop writing to the inbox.
		t.activeBodies.CloseAll(acplog.OrDefault(t.logger))
		// Drain the inbox so that any goroutine still writing to it does not
		// block forever. We spin-drain until the channel is empty; the closed
		// `done` channel and closed bodies prevent new writes from arriving.
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			for {
				select {
				case _, ok := <-t.inbox:
					if !ok {
						return
					}
				default:
					return
				}
			}
		}()
		// Wait for the drain to finish with a bounded timeout so Close never
		// blocks indefinitely.
		select {
		case <-drainDone:
		case <-time.After(100 * time.Millisecond):
		}
		if t.httpClient != nil {
			t.httpClient.CloseIdleConnections()
		}
		t.closeErr = errors.Join(closeErrs...)
	})
	return t.closeErr
}

func (t *ClientTransport) closeRemoteConnection() error {
	connectionID := t.state.ConnectionID()
	if connectionID == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.baseURL, nil)
	if err != nil {
		return fmt.Errorf("create DELETE request: %w", err)
	}
	req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	if protocolVersion := t.ProtocolVersion(); protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}
	t.applyCustomHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", t.baseURL, err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		acplog.OrDefault(t.logger).Error("drain DELETE response body: %v", err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("DELETE %s: HTTP %d", t.baseURL, resp.StatusCode)
	}
	return nil
}

// enqueue delivers a message to the inbox. Returns false if the transport
// is closed (signals the caller to stop).
func (t *ClientTransport) enqueue(msg json.RawMessage) bool {
	t.trackInboundMessage(msg)
	select {
	case t.inbox <- msg:
		return true
	case <-t.done:
		return false
	}
}

func (t *ClientTransport) sessionIDForOutboundMessage(rawID *json.RawMessage, method string, params json.RawMessage) string {
	if method != "" {
		if sessionID := acptransport.ExtractSessionID(params); sessionID != "" {
			return sessionID
		}
		if acp.IsSessionScopedMethod(method) {
			return t.SessionID()
		}
		return ""
	}
	if rawID == nil {
		return ""
	}

	key := jsonrpc.RawIDToKey(rawID)
	sessionID, ok := t.state.TakeRequestSession(key)
	if ok {
		return sessionID
	}
	return ""
}

func (t *ClientTransport) protocolVersionForOutboundMessage(method string) string {
	if method == methodInitialize {
		return ""
	}
	return t.ProtocolVersion()
}

func (t *ClientTransport) rememberProtocolVersionFromInitialize(method, protocolVersion string) {
	if method != methodInitialize {
		return
	}
	if protocolVersion != "" {
		t.state.SetProtocolVersion(protocolVersion)
	}
}

func (t *ClientTransport) trackInboundMessage(msg json.RawMessage) {
	envelope, err := jsonrpc.ParseEnvelope(msg)
	if err != nil {
		return
	}

	if envelope.IsRequest() {
		if sessionID := envelope.Params.SessionID; sessionID != "" {
			t.state.StoreRequestSession(jsonrpc.RawIDToKey(envelope.ID), sessionID)
		}
		return
	}

	if envelope.IsResponse() {
		t.state.TrackResponseMetadata(
			envelope.Result.ProtocolVersion,
			envelope.Result.SessionID,
		)
	}
}

func (t *ClientTransport) applyCustomHeaders(req *http.Request) {
	for key, value := range t.customHeaders {
		req.Header.Add(key, value)
	}
}

// Client listener and body registries.

type clientListenerRegistry struct {
	mu        sync.Mutex
	listeners map[string]*clientListener
}

func newClientListenerRegistry() clientListenerRegistry {
	return clientListenerRegistry{
		listeners: make(map[string]*clientListener),
	}
}

func (r *clientListenerRegistry) Replace(sessionID string, listener *clientListener) *clientListener {
	r.mu.Lock()
	replaced := r.listeners[sessionID]
	r.listeners[sessionID] = listener
	r.mu.Unlock()
	return replaced
}

func (r *clientListenerRegistry) StopAll(logger acplog.Logger) {
	r.mu.Lock()
	all := r.listeners
	r.listeners = make(map[string]*clientListener)
	r.mu.Unlock()

	for _, listener := range all {
		closeClientListener(listener, logger)
	}
}

func (r *clientListenerRegistry) StopSession(sessionID string, logger acplog.Logger) {
	r.mu.Lock()
	listener, ok := r.listeners[sessionID]
	if ok {
		delete(r.listeners, sessionID)
	}
	r.mu.Unlock()
	if ok {
		closeClientListener(listener, logger)
	}
}

func (r *clientListenerRegistry) RemoveIfBody(sessionID string, body io.Closer) {
	r.mu.Lock()
	if listener, ok := r.listeners[sessionID]; ok && listener.body == body {
		delete(r.listeners, sessionID)
	}
	r.mu.Unlock()
}

type activeBodyRegistry struct {
	mu     sync.Mutex
	bodies map[io.ReadCloser]struct{}
}

func newActiveBodyRegistry() activeBodyRegistry {
	return activeBodyRegistry{
		bodies: make(map[io.ReadCloser]struct{}),
	}
}

func (r *activeBodyRegistry) Add(body io.ReadCloser) {
	r.mu.Lock()
	r.bodies[body] = struct{}{}
	r.mu.Unlock()
}

func (r *activeBodyRegistry) Remove(body io.ReadCloser) {
	r.mu.Lock()
	delete(r.bodies, body)
	r.mu.Unlock()
}

func (r *activeBodyRegistry) CloseAll(logger acplog.Logger) {
	r.mu.Lock()
	bodies := make([]io.ReadCloser, 0, len(r.bodies))
	for body := range r.bodies {
		bodies = append(bodies, body)
	}
	r.mu.Unlock()

	for _, body := range bodies {
		if err := body.Close(); err != nil && logger != nil {
			logger.Error("close POST SSE body: %v", err)
		}
	}
}

// Client SSE stream consumption.

func (t *ClientTransport) consumeRequestSSEStream(rawID *json.RawMessage, body io.ReadCloser) {
	release := t.trackActiveSSEBody(body)
	defer release()

	requestIDKey := jsonrpc.RawIDToKey(rawID)
	resolved := false
	transportClosed := false

	err := t.readSSE(body, func(msg json.RawMessage) bool {
		if !t.enqueue(msg) {
			transportClosed = true
			return true
		}
		envelope, parseErr := jsonrpc.ParseEnvelope(msg)
		if parseErr == nil && requestIDKey != "" && envelope.IsResponse() && jsonrpc.RawIDToKey(envelope.ID) == requestIDKey {
			resolved = true
			return true
		}
		return false
	})
	if resolved || transportClosed || t.isClosed() || requestIDKey == "" {
		return
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	if injectErr := t.enqueueRequestFailure(rawID, err); injectErr != nil && !t.isClosed() {
		acplog.OrDefault(t.logger).Error("enqueue synthetic HTTP request failure for %s: %v", requestIDKey, injectErr)
	}
}

func (t *ClientTransport) trackActiveSSEBody(body io.ReadCloser) func() {
	t.activeBodies.Add(body)

	return func() {
		t.activeBodies.Remove(body)
		if err := body.Close(); err != nil && !t.isClosed() {
			acplog.OrDefault(t.logger).Error("close POST SSE body: %v", err)
		}
	}
}

func (t *ClientTransport) readSSE(body io.Reader, handle func(json.RawMessage) bool) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, jsonrpc.InitialBufSize), acptransport.DefaultMaxMessageSize)

	var dataLines []string
	var dataLinesSize int // accumulated byte size of all data lines
	var eventType string
	flush := func() bool {
		defer func() {
			eventType = ""
			dataLinesSize = 0
		}()
		if len(dataLines) == 0 {
			return false
		}
		// ACP uses "event: message". Per SSE spec, omitting the event field
		// defaults to "message". Ignore any other event types.
		if eventType != "" && eventType != "message" {
			dataLines = nil
			return false
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		return handle(json.RawMessage(payload))
	}

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			if flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case !strings.HasPrefix(line, "data:"):
			continue
		default:
			data := strings.TrimPrefix(line, "data:")
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			dataLinesSize += len(data)
			if dataLinesSize > acptransport.DefaultMaxMessageSize {
				dataLines = nil
				dataLinesSize = 0
				return fmt.Errorf("SSE event exceeded max message size (%d bytes)", acptransport.DefaultMaxMessageSize)
			}
			dataLines = append(dataLines, data)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan SSE stream: %w", err)
	}
	if flush() {
		return nil
	}
	return nil
}

func (t *ClientTransport) enqueueRequestFailure(rawID *json.RawMessage, cause error) error {
	if rawID == nil {
		return nil
	}

	var id jsonrpc.ID
	if err := json.Unmarshal(*rawID, &id); err != nil {
		return fmt.Errorf("parse request id: %w", err)
	}

	msg := jsonrpc.NewErrorResponse(&id, acp.ErrInternalError(
		fmt.Sprintf("streamable HTTP request stream terminated before final response: %v", cause),
		cause,
	))

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal request failure response: %w", err)
	}

	if !t.enqueue(json.RawMessage(data)) {
		return acptransport.ErrTransportClosed
	}
	return nil
}

func (t *ClientTransport) isClosed() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// HTTP client helpers.

func newDefaultHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func ensureHTTPClientHasCookieJar(client *http.Client) *http.Client {
	if client == nil {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar}
	}
	if client.Jar != nil {
		return client
	}
	jar, _ := cookiejar.New(nil)
	clone := *client
	clone.Jar = jar
	return &clone
}
