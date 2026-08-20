package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/network/standard"
	ginframework "github.com/gin-gonic/gin"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acpserver "github.com/eino-contrib/acp/server"
	acpgin "github.com/eino-contrib/acp/server/gin"
	acphertz "github.com/eino-contrib/acp/server/hertz"
	acptransport "github.com/eino-contrib/acp/transport"
	acphttpclient "github.com/eino-contrib/acp/transport/http/client"
)

type sessionLifecycleWireAgent struct {
	acp.BaseAgent
	next              atomic.Int32
	connection        *acpconn.AgentConnection
	sendUpdateOnClose bool
}

type sessionResumeWireAgent struct {
	acp.BaseAgent
	connection *acpconn.AgentConnection
}

func (a *sessionResumeWireAgent) SetClientConnection(connection *acpconn.AgentConnection) {
	a.connection = connection
}

func (*sessionResumeWireAgent) Initialize(_ context.Context, request acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: request.ProtocolVersion}, nil
}

func (*sessionResumeWireAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}

func (a *sessionResumeWireAgent) sendUpdate(ctx context.Context, sessionID string) error {
	return a.connection.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: acp.SessionID(sessionID),
		Update: acp.NewSessionUpdateAgentMessageChunk(acp.ContentChunk{
			Content: acp.NewContentBlockText(acp.TextContent{Text: "established"}),
		}),
	})
}

func (a *sessionLifecycleWireAgent) SetClientConnection(connection *acpconn.AgentConnection) {
	a.connection = connection
}

func (a *sessionLifecycleWireAgent) Initialize(_ context.Context, request acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: request.ProtocolVersion}, nil
}

func (a *sessionLifecycleWireAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionID: acp.SessionID("wire-session-" + string(rune('a'+a.next.Add(1)-1)))}, nil
}

func (a *sessionLifecycleWireAgent) CloseSession(ctx context.Context, request acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	if a.sendUpdateOnClose {
		err := a.connection.SessionUpdate(ctx, acp.SessionNotification{
			SessionID: request.SessionID,
			Update: acp.NewSessionUpdateAgentMessageChunk(acp.ContentChunk{
				Content: acp.NewContentBlockText(acp.TextContent{Text: "closing"}),
			}),
		})
		if err != nil {
			return acp.CloseSessionResponse{}, err
		}
	}
	return acp.CloseSessionResponse{}, nil
}

func (a *sessionLifecycleWireAgent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, nil
}

func (a *sessionLifecycleWireAgent) Prompt(ctx context.Context, request acp.PromptRequest) (acp.PromptResponse, error) {
	err := a.connection.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: request.SessionID,
		Update: acp.NewSessionUpdateAgentMessageChunk(acp.ContentChunk{
			Content: acp.NewContentBlockText(acp.TextContent{Text: "still active"}),
		}),
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

type sessionLifecycleWireClient struct {
	acp.BaseClient
	updates chan acp.SessionNotification
}

func (c *sessionLifecycleWireClient) SessionUpdate(_ context.Context, update acp.SessionNotification) error {
	c.updates <- update
	return nil
}

type sessionLifecycleHost struct {
	name  string
	start func(*testing.T, *acpserver.ACPServer, *atomic.Int32, *atomic.Int32) string
}

func sessionLifecycleHosts() []sessionLifecycleHost {
	return []sessionLifecycleHost{
		{name: "Hertz", start: startSessionLifecycleHertzHost},
		{name: "Gin", start: startSessionLifecycleGinHost},
	}
}

func TestSessionCloseConvergesSSEAcrossFrameworks(t *testing.T) {
	for _, host := range sessionLifecycleHosts() {
		t.Run(host.name, func(t *testing.T) {
			agent := &sessionLifecycleWireAgent{sendUpdateOnClose: true}
			core, err := acpserver.NewACPServer(func(context.Context) acp.Agent { return agent })
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			t.Cleanup(func() { _ = core.Close() })

			var activeGET, totalGET atomic.Int32
			baseURL := host.start(t, core, &activeGET, &totalGET)
			client := &sessionLifecycleWireClient{updates: make(chan acp.SessionNotification, 1)}
			transport := acphttpclient.NewClientTransport(baseURL,
				acphttpclient.WithSSEReconnect(),
				acphttpclient.WithSSEReconnectBackoff(5*time.Millisecond, 10*time.Millisecond),
			)
			connection := acpconn.NewClientConnection(client, transport)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := connection.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			initializeSessionLifecycleConnection(t, ctx, connection)

			created, err := connection.NewSession(ctx, acp.NewSessionRequest{Cwd: "/tmp", MCPServers: []acp.MCPServer{}})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			waitForAtomicValue(t, &activeGET, 1, "active GET listener")
			if _, err := connection.CloseSession(ctx, acp.CloseSessionRequest{SessionID: created.SessionID}); err != nil {
				t.Fatalf("CloseSession: %v", err)
			}
			select {
			case update := <-client.updates:
				if update.SessionID != created.SessionID {
					t.Fatalf("close-handler update session = %q, want %q", update.SessionID, created.SessionID)
				}
			case <-time.After(time.Second):
				t.Fatal("close handler could not use the existing reverse channel")
			}
			waitForAtomicValue(t, &activeGET, 0, "closed GET listener")

			getCount := totalGET.Load()
			time.Sleep(75 * time.Millisecond)
			if got := totalGET.Load(); got != getCount {
				t.Fatalf("GET attempts after successful close = %d, want no reconnect beyond %d", got, getCount)
			}
			assertClosedSessionGETNotFound(t, baseURL, transport.ConnectionID(), string(created.SessionID), transport.ProtocolVersion())
		})
	}
}

func TestSessionCloseEvictsExistingRawSSEAcrossFrameworks(t *testing.T) {
	for _, host := range sessionLifecycleHosts() {
		t.Run(host.name, func(t *testing.T) {
			agent := &sessionLifecycleWireAgent{}
			core, err := acpserver.NewACPServer(func(context.Context) acp.Agent { return agent })
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			t.Cleanup(func() { _ = core.Close() })

			var activeGET, totalGET atomic.Int32
			baseURL := host.start(t, core, &activeGET, &totalGET)
			transport := acphttpclient.NewClientTransport(baseURL)
			connection := acpconn.NewClientConnection(acp.BaseClient{}, transport)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := connection.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			initializeSessionLifecycleConnection(t, ctx, connection)
			created, err := connection.NewSession(ctx, acp.NewSessionRequest{Cwd: "/tmp", MCPServers: []acp.MCPServer{}})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}

			// Replace the SDK-managed listener with a raw stream that CloseSession
			// cannot stop locally. This proves the server itself evicts the GET.
			rawResponse := openRawSessionSSE(t, ctx, baseURL, transport.ConnectionID(), string(created.SessionID), transport.ProtocolVersion())
			defer rawResponse.Body.Close()
			waitForAtomicValue(t, &activeGET, 1, "replacement raw GET listener")
			postRawSessionClose(t, ctx, baseURL, transport.ConnectionID(), string(created.SessionID), transport.ProtocolVersion())
			waitForAtomicValue(t, &activeGET, 0, "server-evicted raw GET listener")

			readDone := make(chan error, 1)
			go func() {
				_, err := bufio.NewReader(rawResponse.Body).ReadByte()
				readDone <- err
			}()
			select {
			case err := <-readDone:
				if err == nil {
					t.Fatal("raw SSE produced data after session/close instead of ending")
				}
			case <-time.After(time.Second):
				t.Fatal("raw SSE did not end after session/close")
			}
		})
	}
}

func TestSessionDeleteKeepsActiveSSEAcrossFrameworks(t *testing.T) {
	for _, host := range sessionLifecycleHosts() {
		t.Run(host.name, func(t *testing.T) {
			agent := &sessionLifecycleWireAgent{}
			core, err := acpserver.NewACPServer(func(context.Context) acp.Agent { return agent })
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			t.Cleanup(func() { _ = core.Close() })

			var activeGET, totalGET atomic.Int32
			baseURL := host.start(t, core, &activeGET, &totalGET)
			client := &sessionLifecycleWireClient{updates: make(chan acp.SessionNotification, 1)}
			transport := acphttpclient.NewClientTransport(baseURL)
			connection := acpconn.NewClientConnection(client, transport)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := connection.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			initializeSessionLifecycleConnection(t, ctx, connection)

			created, err := connection.NewSession(ctx, acp.NewSessionRequest{Cwd: "/tmp", MCPServers: []acp.MCPServer{}})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			waitForAtomicValue(t, &activeGET, 1, "active GET listener")
			if _, err := connection.DeleteSession(ctx, acp.DeleteSessionRequest{SessionID: created.SessionID}); err != nil {
				t.Fatalf("DeleteSession: %v", err)
			}
			if got := activeGET.Load(); got != 1 {
				t.Fatalf("active GET after session/delete = %d, want 1", got)
			}
			if _, err := connection.Prompt(ctx, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{}}); err != nil {
				t.Fatalf("Prompt after session/delete: %v", err)
			}
			select {
			case update := <-client.updates:
				if update.SessionID != created.SessionID {
					t.Fatalf("update session = %q, want %q", update.SessionID, created.SessionID)
				}
			case <-time.After(time.Second):
				t.Fatal("active session did not deliver update after session/delete")
			}
		})
	}
}

func TestSessionResumeEstablishesSSEAcrossFrameworks(t *testing.T) {
	for _, host := range sessionLifecycleHosts() {
		t.Run(host.name, func(t *testing.T) {
			agent := &sessionResumeWireAgent{}
			core, err := acpserver.NewACPServer(func(context.Context) acp.Agent { return agent })
			if err != nil {
				t.Fatalf("NewACPServer: %v", err)
			}
			t.Cleanup(func() { _ = core.Close() })

			var activeGET, totalGET atomic.Int32
			baseURL := host.start(t, core, &activeGET, &totalGET)
			client := &sessionLifecycleWireClient{updates: make(chan acp.SessionNotification, 1)}
			transport := acphttpclient.NewClientTransport(baseURL)
			connection := acpconn.NewClientConnection(client, transport)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := connection.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			initializeSessionLifecycleConnection(t, ctx, connection)
			const sessionID = "wire-resumed-session"
			_, err = connection.ResumeSession(ctx, acp.ResumeSessionRequest{
				SessionID: sessionID, Cwd: "/tmp", MCPServers: []acp.MCPServer{},
			})
			if err != nil {
				t.Fatalf("ResumeSession: %v", err)
			}
			waitForAtomicValue(t, &activeGET, 1, "active GET listener")
			if err := agent.sendUpdate(ctx, sessionID); err != nil {
				t.Fatalf("send update: %v", err)
			}
			select {
			case update := <-client.updates:
				if string(update.SessionID) != sessionID {
					t.Fatalf("update session = %q, want %q", update.SessionID, sessionID)
				}
			case <-time.After(time.Second):
				t.Fatal("established session did not receive reverse update")
			}
		})
	}
}

func initializeSessionLifecycleConnection(t *testing.T, ctx context.Context, connection *acpconn.ClientConnection) {
	t.Helper()
	if _, err := connection.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion(acp.CurrentProtocolVersion)}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func waitForAtomicValue(t *testing.T, value *atomic.Int32, want int32, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for value.Load() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := value.Load(); got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

func assertClosedSessionGETNotFound(t *testing.T, baseURL, connectionID, sessionID, protocolVersion string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+acpserver.DefaultEndpoint, nil)
	if err != nil {
		t.Fatalf("create closed-session GET: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	req.Header.Set(acptransport.HeaderSessionID, sessionID)
	if protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("closed-session GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("closed-session GET status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func openRawSessionSSE(t *testing.T, ctx context.Context, baseURL, connectionID, sessionID, protocolVersion string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+acpserver.DefaultEndpoint, nil)
	if err != nil {
		t.Fatalf("create raw GET SSE: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	req.Header.Set(acptransport.HeaderSessionID, sessionID)
	if protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open raw GET SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("raw GET SSE status = %d: %s", resp.StatusCode, body)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		resp.Body.Close()
		t.Fatalf("read raw GET SSE keepalive: %v", err)
	}
	if line != ":keep-alive\n" {
		resp.Body.Close()
		t.Fatalf("raw GET SSE first line = %q, want keepalive", line)
	}
	return resp
}

func postRawSessionClose(t *testing.T, ctx context.Context, baseURL, connectionID, sessionID, protocolVersion string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      999,
		"method":  acp.MethodAgentCloseSession,
		"params":  acp.CloseSessionRequest{SessionID: acp.SessionID(sessionID)},
	})
	if err != nil {
		t.Fatalf("marshal raw session close: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+acpserver.DefaultEndpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create raw session close: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(acptransport.HeaderConnectionID, connectionID)
	req.Header.Set(acptransport.HeaderSessionID, sessionID)
	if protocolVersion != "" {
		req.Header.Set(acptransport.HeaderProtocolVersion, protocolVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw session close: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw session close status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func startSessionLifecycleHertzHost(t *testing.T, core *acpserver.ACPServer, active, total *atomic.Int32) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Hertz: %v", err)
	}
	host := hertzserver.New(
		hertzserver.WithListener(listener),
		hertzserver.WithTransport(standard.NewTransporter),
		hertzserver.WithStreamBody(true),
	)
	host.NoHijackConnPool = true
	host.Any(acpserver.DefaultEndpoint, func(ctx context.Context, c *app.RequestContext) {
		if string(c.Method()) == http.MethodGet {
			total.Add(1)
			active.Add(1)
			defer active.Add(-1)
		}
		c.Next(ctx)
	}, acphertz.New(core))
	runErr := make(chan error, 1)
	go func() { runErr <- host.Run() }()
	baseURL := "http://" + listener.Addr().String()
	waitForHTTPServer(t, baseURL+acpserver.DefaultEndpoint, runErr)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "closed network connection") {
				t.Errorf("Hertz Run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Hertz host did not stop")
		}
	})
	return baseURL
}

func startSessionLifecycleGinHost(t *testing.T, core *acpserver.ACPServer, active, total *atomic.Int32) string {
	t.Helper()
	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Any(acpserver.DefaultEndpoint, func(c *ginframework.Context) {
		if c.Request.Method == http.MethodGet {
			total.Add(1)
			active.Add(1)
			defer active.Add(-1)
		}
		c.Next()
	}, acpgin.New(core))
	host := httptest.NewServer(router)
	t.Cleanup(host.Close)
	return host.URL
}
