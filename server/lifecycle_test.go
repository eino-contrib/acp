package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	acptransport "github.com/eino-contrib/acp/transport"
)

func TestCloseAbortsPendingWebSocketAdmission(t *testing.T) {
	var factoryCalls atomic.Int32
	s := newLifecycleServer(t, func(context.Context) acp.Agent {
		factoryCalls.Add(1)
		return &acp.BaseAgent{}
	})
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown before upgrade outcome = %v, want deadline", err)
	}
	cancel()
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
	if got := len(s.wsAdmissions); got != 1 {
		t.Fatalf("pending admissions = %d, want 1 until adapter outcome", got)
	}
	// Adapter cleanup may race with core Close; Abort remains idempotent.
	admission.Abort()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after Abort: %v", err)
	}
}

func TestAbortPendingWebSocketAdmissionDoesNotCreateAgent(t *testing.T) {
	var factoryCalls atomic.Int32
	s := newLifecycleServer(t, func(context.Context) acp.Agent {
		factoryCalls.Add(1)
		return &acp.BaseAgent{}
	})
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}

	admission.Abort()
	admission.Abort()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("AgentFactory calls = %d, want 0", got)
	}
}

func TestCloseRacesPendingWebSocketAdmissionWithoutLeak(t *testing.T) {
	const iterations = 64
	for i := 0; i < iterations; i++ {
		var factoryCalls atomic.Int32
		s, err := NewACPServer(func(context.Context) acp.Agent {
			factoryCalls.Add(1)
			return &acp.BaseAgent{}
		})
		if err != nil {
			t.Fatalf("iteration %d: NewACPServer: %v", i, err)
		}

		start := make(chan struct{})
		type admitResult struct {
			admission *WebSocketAdmission
			err       error
		}
		admitDone := make(chan admitResult, 1)
		closeDone := make(chan error, 1)
		go func() {
			<-start
			admission, err := s.AdmitWebSocket(context.Background())
			admitDone <- admitResult{admission: admission, err: err}
		}()
		go func() {
			<-start
			closeDone <- s.Close()
		}()
		close(start)

		result := <-admitDone
		if result.err != nil && !errors.Is(result.err, ErrServerClosed) {
			t.Fatalf("iteration %d: AdmitWebSocket error = %v", i, result.err)
		}
		if err := <-closeDone; err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		if result.admission != nil {
			result.admission.Abort()
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = s.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: Shutdown: %v", i, err)
		}
		if got := factoryCalls.Load(); got != 0 {
			t.Fatalf("iteration %d: AgentFactory calls = %d, want 0", i, got)
		}
		s.lifecycleMu.Lock()
		pending := len(s.wsAdmissions)
		s.lifecycleMu.Unlock()
		if pending != 0 {
			t.Fatalf("iteration %d: pending admissions = %d, want 0", i, pending)
		}
	}
}

func TestShutdownWaitsForServingWebSocket(t *testing.T) {
	enteredFactory := make(chan struct{})
	releaseFactory := make(chan struct{})
	s := newLifecycleServer(t, func(context.Context) acp.Agent {
		close(enteredFactory)
		<-releaseFactory
		return &acp.BaseAgent{}
	})
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}
	conn := newBlockingWSConn()
	serveDone := make(chan error, 1)
	go func() { serveDone <- admission.Serve(conn) }()
	select {
	case <-enteredFactory:
	case <-time.After(time.Second):
		t.Fatal("AgentFactory was not entered")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before serving admission exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseFactory)
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after factory was released")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after serving admission exited")
	}
}

func TestShutdownHonorsContextWhileServingWebSocket(t *testing.T) {
	enteredFactory := make(chan struct{})
	releaseFactory := make(chan struct{})
	s := newLifecycleServer(t, func(context.Context) acp.Agent {
		close(enteredFactory)
		<-releaseFactory
		return &acp.BaseAgent{}
	})
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}
	conn := newBlockingWSConn()
	serveDone := make(chan error, 1)
	go func() { serveDone <- admission.Serve(conn) }()
	select {
	case <-enteredFactory:
	case <-time.After(time.Second):
		t.Fatal("AgentFactory was not entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	close(releaseFactory)
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after factory was released")
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	if err := s.Shutdown(drainCtx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestHTTPConnectionCreationAfterCloseReturnsServiceUnavailable(t *testing.T) {
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return &acp.BaseAgent{} })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.AdmitWebSocket(context.Background()); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("AdmitWebSocket error = %v, want ErrServerClosed", err)
	}
	ctx := &lifecycleHTTPContext{ctx: context.Background()}
	s.ServeHTTP(ctx, http.MethodPost)
	if ctx.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", ctx.status, http.StatusServiceUnavailable)
	}
	if _, err := s.newHTTPConnection(context.Background()); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("newHTTPConnection error = %v, want ErrServerClosed", err)
	}
}

func TestCloseIsIdempotentAndNonBlocking(t *testing.T) {
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return &acp.BaseAgent{} })
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Close blocked for %v", elapsed)
	}
	admission.Abort()
}

func TestWebSocketFactoryPanicSendsGenericInternalErrorClose(t *testing.T) {
	const secret = "sensitive factory panic"
	s := newLifecycleServer(t, func(context.Context) acp.Agent { panic(secret) })
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}
	conn := &recordingWSConn{}
	if err := admission.Serve(conn); err == nil {
		t.Fatal("Serve error = nil, want recovered factory panic")
	}
	if !conn.closed.Load() {
		t.Fatal("upgraded connection was not closed after setup failure")
	}
	if conn.controlType != 8 {
		t.Fatalf("control type = %d, want CloseMessage", conn.controlType)
	}
	if len(conn.controlPayload) < 2 {
		t.Fatalf("close payload = %v, want code and reason", conn.controlPayload)
	}
	if got := int(binary.BigEndian.Uint16(conn.controlPayload[:2])); got != 1011 {
		t.Fatalf("close code = %d, want 1011", got)
	}
	reason := string(conn.controlPayload[2:])
	if reason != "failed to create connection" {
		t.Fatalf("close reason = %q, want generic setup failure", reason)
	}
	if strings.Contains(reason, secret) {
		t.Fatalf("close reason leaked panic value: %q", reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestHTTPConnectionSetterPanicIsSetupErrorAndCleansUnregisteredConnection(t *testing.T) {
	const secret = "sensitive HTTP setter panic"
	agent := &panickingConnectionAwareAgent{panicValue: secret}
	var connectionContextDone <-chan struct{}
	s := newLifecycleServer(t, func(ctx context.Context) acp.Agent {
		connectionContextDone = ctx.Done()
		return agent
	})
	request := &lifecycleHTTPContext{
		ctx: context.Background(),
		headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json, text/event-stream",
		},
		body: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`),
	}

	s.ServeHTTP(request, http.MethodPost)

	if request.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", request.status, http.StatusInternalServerError)
	}
	if request.errMessage != "failed to create connection" {
		t.Fatalf("error response = %q, want generic setup failure", request.errMessage)
	}
	if strings.Contains(request.errMessage, secret) {
		t.Fatalf("error response leaked panic value: %q", request.errMessage)
	}
	if agent.connection == nil {
		t.Fatal("SetClientConnection did not receive the temporary connection")
	}
	select {
	case <-agent.connection.Done():
	default:
		t.Fatal("temporary AgentConnection was not closed after setter panic")
	}
	select {
	case <-connectionContextDone:
	default:
		t.Fatal("connection context was not canceled after setter panic")
	}
	s.conns.mu.RLock()
	registered := len(s.conns.conns)
	s.conns.mu.RUnlock()
	if registered != 0 {
		t.Fatalf("registered HTTP connections = %d, want 0", registered)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestWebSocketConnectionSetterPanicReturnsSetupErrorAndClosesSocket(t *testing.T) {
	const secret = "sensitive WebSocket setter panic"
	agent := &panickingConnectionAwareAgent{panicValue: secret}
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return agent })
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}
	conn := &recordingWSConn{}

	if err := admission.Serve(conn); err == nil {
		t.Fatal("Serve error = nil, want recovered setter panic")
	}
	if agent.connection == nil {
		t.Fatal("SetClientConnection did not receive the temporary connection")
	}
	select {
	case <-agent.connection.Done():
	default:
		t.Fatal("temporary AgentConnection was not closed after setter panic")
	}
	if !conn.closed.Load() {
		t.Fatal("upgraded connection was not closed after setup failure")
	}
	if conn.controlType != 8 {
		t.Fatalf("control type = %d, want CloseMessage", conn.controlType)
	}
	if len(conn.controlPayload) < 2 {
		t.Fatalf("close payload = %v, want code and reason", conn.controlPayload)
	}
	if got := int(binary.BigEndian.Uint16(conn.controlPayload[:2])); got != 1011 {
		t.Fatalf("close code = %d, want 1011", got)
	}
	reason := string(conn.controlPayload[2:])
	if reason != "failed to create connection" {
		t.Fatalf("close reason = %q, want generic setup failure", reason)
	}
	if strings.Contains(reason, secret) {
		t.Fatalf("close reason leaked panic value: %q", reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

type panickingConnectionAwareAgent struct {
	acp.BaseAgent
	panicValue any
	connection *acpconn.AgentConnection
}

func (a *panickingConnectionAwareAgent) SetClientConnection(connection *acpconn.AgentConnection) {
	a.connection = connection
	panic(a.panicValue)
}

type typedNilAgent struct{ acp.BaseAgent }

type blockingInitializeAgent struct {
	acp.BaseAgent
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func (a *blockingInitializeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	close(a.entered)
	<-a.release
	close(a.exited)
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersion(acp.CurrentProtocolVersion)}, nil
}

func TestShutdownWaitsForHTTPDispatchHandler(t *testing.T) {
	agent := &blockingInitializeAgent{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		exited:  make(chan struct{}),
	}
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return agent })
	request := &lifecycleHTTPContext{
		ctx: context.Background(),
		headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json, text/event-stream",
		},
		body: []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1}}"),
	}
	serveDone := make(chan struct{})
	go func() {
		s.ServeHTTP(request, http.MethodPost)
		close(serveDone)
	}()
	select {
	case <-agent.entered:
	case <-time.After(time.Second):
		t.Fatal("initialize handler did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned while HTTP dispatch handler was still running: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(agent.release)
	select {
	case <-agent.exited:
	case <-time.After(time.Second):
		t.Fatal("initialize handler did not exit")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown after handler exit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after HTTP handler exited")
	}
	<-serveDone
}

func TestHTTPConnectionCloseDoesNotWaitForBlockedSSEWriter(t *testing.T) {
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return &acp.BaseAgent{} })
	connection, err := s.newHTTPConnection(context.Background())
	if err != nil {
		t.Fatalf("newHTTPConnection: %v", err)
	}
	session := connection.httpConn.EnsureSession("blocked-writer")
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	session.BindStream(func(json.RawMessage) error {
		close(writeStarted)
		<-releaseWrite
		return nil
	})
	if err := session.Send(json.RawMessage("\"message\"")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("SSE writer did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- connection.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(releaseWrite)
		t.Fatal("connection Close blocked on SSE writer")
	}
	close(releaseWrite)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after SSE writer exit: %v", err)
	}
}

type sessionTerminatingAgent struct{ acp.BaseAgent }

func (*sessionTerminatingAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (*sessionTerminatingAgent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, nil
}

func TestHTTPSessionCloseRemovesTransportSession(t *testing.T) {
	s, connection, session, evicted := newHTTPTransportSession(t)
	request := sessionLifecycleRequest(connection.id, session.SessionID,
		`{"jsonrpc":"2.0","id":1,"method":"session/close","params":{"sessionId":"session-lifecycle"}}`)

	s.ServeHTTP(request, http.MethodPost)

	if request.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (error=%q)", request.status, http.StatusOK, request.errMessage)
	}
	if _, ok := connection.httpConn.LookupSession(session.SessionID); ok {
		t.Fatal("successful session/close left the HTTP transport session routable")
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("successful session/close left the HTTP session listener alive")
	}
	select {
	case <-evicted:
	default:
		t.Fatal("successful session/close did not evict the active GET SSE stream")
	}
}

func TestHTTPSessionDeleteKeepsActiveTransportSession(t *testing.T) {
	s, connection, session, evicted := newHTTPTransportSession(t)
	request := sessionLifecycleRequest(connection.id, session.SessionID,
		`{"jsonrpc":"2.0","id":1,"method":"session/delete","params":{"sessionId":"session-lifecycle"}}`)

	s.ServeHTTP(request, http.MethodPost)

	if request.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (error=%q)", request.status, http.StatusOK, request.errMessage)
	}
	if got, ok := connection.httpConn.LookupSession(session.SessionID); !ok || got != session {
		t.Fatal("session/delete removed an active HTTP transport session")
	}
	select {
	case <-session.Done():
		t.Fatal("session/delete closed an active HTTP session listener")
	default:
	}
	select {
	case <-evicted:
		t.Fatal("session/delete evicted an active GET SSE stream")
	default:
	}
}

func newHTTPTransportSession(t *testing.T) (*ACPServer, *httpRemoteConnection, *acphttpserver.Session, <-chan struct{}) {
	t.Helper()
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return &sessionTerminatingAgent{} })
	connection, err := s.newHTTPConnection(context.Background())
	if err != nil {
		t.Fatalf("newHTTPConnection: %v", err)
	}
	session := connection.httpConn.EnsureSession("session-lifecycle")
	gen, evicted := session.BindStream(func(json.RawMessage) error { return nil })
	t.Cleanup(func() { session.UnbindStream(gen) })
	return s, connection, session, evicted
}

func sessionLifecycleRequest(connectionID, sessionID, body string) *lifecycleHTTPContext {
	return &lifecycleHTTPContext{
		ctx: context.Background(),
		headers: map[string]string{
			"Content-Type":                  "application/json",
			"Accept":                        "application/json, text/event-stream",
			acptransport.HeaderConnectionID: connectionID,
			acptransport.HeaderSessionID:    sessionID,
		},
		body: []byte(body),
	}
}

type blockingKeepAliveHTTPContext struct {
	*lifecycleHTTPContext
	started chan struct{}
	release chan struct{}
}

func (c *blockingKeepAliveHTTPContext) WriteSSEKeepAlive() error {
	close(c.started)
	<-c.release
	return nil
}

func TestShutdownWaitsForHTTPGetInitialKeepAlive(t *testing.T) {
	s := newLifecycleServer(t, func(context.Context) acp.Agent { return &acp.BaseAgent{} })
	connection, err := s.newHTTPConnection(context.Background())
	if err != nil {
		t.Fatalf("newHTTPConnection: %v", err)
	}
	const sessionID = "blocked-initial-keepalive"
	connection.httpConn.EnsureSession(sessionID)
	request := &blockingKeepAliveHTTPContext{
		lifecycleHTTPContext: &lifecycleHTTPContext{
			ctx: context.Background(),
			headers: map[string]string{
				"Accept":                        "text/event-stream",
				acptransport.HeaderConnectionID: connection.id,
				acptransport.HeaderSessionID:    sessionID,
			},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	handlerDone := make(chan struct{})
	go func() {
		s.ServeHTTP(request, http.MethodGet)
		close(handlerDone)
	}()
	select {
	case <-request.started:
	case <-time.After(time.Second):
		t.Fatal("GET initial keepalive did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	var earlyShutdown error
	returnedEarly := false
	select {
	case earlyShutdown = <-shutdownDone:
		returnedEarly = true
	case <-time.After(20 * time.Millisecond):
	}
	close(request.release)
	if returnedEarly {
		t.Fatalf("Shutdown returned while GET initial keepalive was still blocked: %v", earlyShutdown)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("GET handler did not return after keepalive was released")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown after GET handler exit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after GET handler exited")
	}
}

func TestAgentFactoryTypedNilFailsCleanly(t *testing.T) {
	s := newLifecycleServer(t, func(context.Context) acp.Agent {
		var agent *typedNilAgent
		return agent
	})
	if _, err := s.newHTTPConnection(context.Background()); err == nil {
		t.Fatal("HTTP connection accepted typed-nil Agent")
	}
	admission, err := s.AdmitWebSocket(context.Background())
	if err != nil {
		t.Fatalf("AdmitWebSocket: %v", err)
	}
	conn := &recordingWSConn{}
	if err := admission.Serve(conn); err == nil {
		t.Fatal("WebSocket connection accepted typed-nil Agent")
	}
	if !conn.closed.Load() {
		t.Fatal("typed-nil WebSocket setup failure did not close connection")
	}
}

func newLifecycleServer(t *testing.T, factory AgentFactory) *ACPServer {
	t.Helper()
	s, err := NewACPServer(factory)
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type lifecycleHTTPContext struct {
	ctx        context.Context
	headers    map[string]string
	body       []byte
	status     int
	errMessage string
}

func (c *lifecycleHTTPContext) Context() context.Context        { return c.ctx }
func (c *lifecycleHTTPContext) RequestHeader(key string) string { return c.headers[key] }
func (c *lifecycleHTTPContext) RequestBody() ([]byte, error)    { return c.body, nil }
func (c *lifecycleHTTPContext) RequestBodyLimited(int64) ([]byte, error) {
	return c.RequestBody()
}
func (c *lifecycleHTTPContext) SetResponseHeader(string, string) {}
func (c *lifecycleHTTPContext) WriteError(code int, msg string) {
	c.status = code
	c.errMessage = msg
}
func (c *lifecycleHTTPContext) SetStatusCode(code int)              { c.status = code }
func (c *lifecycleHTTPContext) Flush()                              {}
func (c *lifecycleHTTPContext) Done() <-chan struct{}               { return c.ctx.Done() }
func (c *lifecycleHTTPContext) WriteSSEEvent(json.RawMessage) error { return nil }
func (c *lifecycleHTTPContext) WriteSSEKeepAlive() error            { return nil }
func (c *lifecycleHTTPContext) CloseSSE()                           {}

var _ acphttpserver.HandlerContext = (*lifecycleHTTPContext)(nil)

type blockingWSConn struct {
	closed chan struct{}
	once   sync.Once
}

type recordingWSConn struct {
	controlType    int
	controlPayload []byte
	closed         atomic.Bool
}

func (*recordingWSConn) ReadMessage() (int, []byte, error) { return 0, nil, errors.New("unused") }
func (*recordingWSConn) WriteMessage(int, []byte) error    { return nil }
func (c *recordingWSConn) WriteControl(messageType int, payload []byte, _ time.Time) error {
	c.controlType = messageType
	c.controlPayload = append([]byte(nil), payload...)
	return nil
}
func (*recordingWSConn) SetReadLimit(int64)                {}
func (*recordingWSConn) SetReadDeadline(time.Time) error   { return nil }
func (*recordingWSConn) SetWriteDeadline(time.Time) error  { return nil }
func (*recordingWSConn) SetPingHandler(func(string) error) {}
func (c *recordingWSConn) Close() error                    { c.closed.Store(true); return nil }

func newBlockingWSConn() *blockingWSConn { return &blockingWSConn{closed: make(chan struct{})} }
func (c *blockingWSConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, errors.New("closed")
}
func (c *blockingWSConn) WriteMessage(int, []byte) error            { return nil }
func (c *blockingWSConn) WriteControl(int, []byte, time.Time) error { return nil }
func (c *blockingWSConn) SetReadLimit(int64)                        {}
func (c *blockingWSConn) SetReadDeadline(time.Time) error           { return nil }
func (c *blockingWSConn) SetWriteDeadline(time.Time) error          { return nil }
func (c *blockingWSConn) SetPingHandler(func(string) error)         {}
func (c *blockingWSConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
