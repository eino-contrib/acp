package conn

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	acptransport "github.com/eino-contrib/acp/transport"
)

type orderedSessionUpdateClient struct {
	acp.BaseClient

	firstStarted chan struct{}
	releaseFirst chan struct{}
	secondDone   chan struct{}
}

type orderedExtNotificationClient struct {
	acp.BaseClient

	firstStarted chan struct{}
	releaseFirst chan struct{}
	secondDone   chan struct{}
}

func (c *orderedSessionUpdateClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	switch notification.SessionID {
	case "first":
		close(c.firstStarted)
		<-c.releaseFirst
	case "second":
		close(c.secondDone)
	}
	return nil
}

func (c *orderedExtNotificationClient) HandleExtNotification(_ context.Context, method string, params json.RawMessage) error {
	if method != "_ordered" {
		return nil
	}

	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return err
	}

	switch payload.ID {
	case "first":
		close(c.firstStarted)
		<-c.releaseFirst
	case "second":
		close(c.secondDone)
	}
	return nil
}

func TestClientConnectionProcessesSessionUpdateNotificationsInOrder(t *testing.T) {
	transport := newChannelTransport()
	client := &orderedSessionUpdateClient{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		secondDone:   make(chan struct{}),
	}
	conn := NewClientConnection(client, transport)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"first","update":{"sessionUpdate":"current_mode_update","currentModeId":"m1"}}}`)
	select {
	case <-client.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first session/update did not start")
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"second","update":{"sessionUpdate":"current_mode_update","currentModeId":"m1"}}}`)
	select {
	case <-client.secondDone:
		t.Fatal("second session/update ran before first completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(client.releaseFirst)

	select {
	case <-client.secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second session/update did not run after first completed")
	}

	cancel()
	select {
	case <-conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop")
	}
}

func TestClientConnectionProcessesCustomOrderedNotificationsInOrder(t *testing.T) {
	transport := newChannelTransport()
	client := &orderedExtNotificationClient{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		secondDone:   make(chan struct{}),
	}
	conn := NewClientConnection(client, transport, WithOrderedNotificationMatcher(func(method string) bool {
		return method == "_ordered"
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"_ordered","params":{"id":"first"}}`)
	select {
	case <-client.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first ordered notification did not start")
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"_ordered","params":{"id":"second"}}`)
	select {
	case <-client.secondDone:
		t.Fatal("second ordered notification ran before first completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(client.releaseFirst)

	select {
	case <-client.secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second ordered notification did not run after first completed")
	}
}

type sessionListenerTransport struct {
	*channelTransport
	began      chan string
	completed  chan string
	aborted    chan string
	forced     chan string
	started    chan string
	beginFound bool
	abortFound bool
}

func (t *sessionListenerTransport) SessionListenerHook(connspi.SessionListenerHookKey) *connspi.SessionListenerHook {
	return &connspi.SessionListenerHook{
		Start: func(_ context.Context, sessionID string, _ func(error)) error {
			t.started <- sessionID
			return nil
		},
		BeginClose: func(sessionID string) bool {
			t.began <- sessionID
			return t.beginFound
		},
		CompleteClose: func(sessionID string) {
			t.completed <- sessionID
		},
		ForceClose: func(sessionID string) {
			t.forced <- sessionID
		},
		AbortClose: func(sessionID string) bool {
			t.aborted <- sessionID
			return t.abortFound
		},
		StopAll: func() {},
	}
}

func newSessionListenerTransport() *sessionListenerTransport {
	return &sessionListenerTransport{
		channelTransport: newChannelTransport(),
		began:            make(chan string, 1),
		completed:        make(chan string, 1),
		aborted:          make(chan string, 1),
		forced:           make(chan string, 1),
		started:          make(chan string, 1),
		beginFound:       true,
		abortFound:       true,
	}
}

func TestSessionResumeStartsClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := connection.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := connection.ResumeSession(ctx, acp.ResumeSessionRequest{
			SessionID: "session-to-resume", Cwd: "/tmp", MCPServers: []acp.MCPServer{},
		})
		callDone <- err
	}()
	request := <-transport.outbox
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(request, &envelope); err != nil {
		t.Fatalf("decode outbound request: %v", err)
	}
	response := append([]byte(`{"jsonrpc":"2.0","id":`), envelope.ID...)
	response = append(response, []byte(`,"result":{}}`)...)
	transport.inbox <- response

	if err := <-callDone; err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	select {
	case got := <-transport.started:
		if got != "session-to-resume" {
			t.Fatalf("listener session = %q, want session-to-resume", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ResumeSession did not start a GET SSE listener")
	}
}

func TestSuccessfulSessionCloseStopsClientListener(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *ClientConnection) error
	}{
		{
			name: "close",
			call: func(ctx context.Context, c *ClientConnection) error {
				_, err := c.CloseSession(ctx, acp.CloseSessionRequest{SessionID: "session-to-stop"})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := newSessionListenerTransport()
			connection := NewClientConnection(acp.BaseClient{}, transport)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := connection.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}

			callDone := make(chan error, 1)
			go func() { callDone <- tt.call(ctx, connection) }()
			select {
			case got := <-transport.began:
				if got != "session-to-stop" {
					t.Fatalf("stopped session = %q, want session-to-stop", got)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("session close did not mark terminal intent before sending the RPC")
			}
			request := <-transport.outbox
			var envelope struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(request, &envelope); err != nil {
				t.Fatalf("decode outbound request: %v", err)
			}
			response := append([]byte(`{"jsonrpc":"2.0","id":`), envelope.ID...)
			response = append(response, []byte(`,"result":{}}`)...)
			transport.inbox <- response

			select {
			case err := <-callDone:
				if err != nil {
					t.Fatalf("session termination RPC: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("session termination RPC did not return")
			}
			select {
			case got := <-transport.completed:
				if got != "session-to-stop" {
					t.Fatalf("completed session = %q, want session-to-stop", got)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("successful close did not complete listener termination")
			}
			select {
			case got := <-transport.started:
				t.Fatalf("successful session termination restarted listener %q", got)
			default:
			}
		})
	}
}

func TestExplicitlyRejectedSessionCloseRestoresClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := connection.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := connection.CloseSession(ctx, acp.CloseSessionRequest{SessionID: "session-to-restore"})
		callDone <- err
	}()
	select {
	case <-transport.began:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("failed session termination did not pause listener before sending")
	}
	request := <-transport.outbox
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(request, &envelope); err != nil {
		t.Fatalf("decode outbound request: %v", err)
	}
	response := append([]byte(`{"jsonrpc":"2.0","id":`), envelope.ID...)
	response = append(response, []byte(`,"error":{"code":-32603,"message":"close failed"}}`)...)
	transport.inbox <- response

	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("CloseSession error = nil, want RPC failure")
		}
	case <-time.After(time.Second):
		t.Fatal("failed session termination RPC did not return")
	}
	select {
	case got := <-transport.aborted:
		if got != "session-to-restore" {
			t.Fatalf("aborted close session = %q, want session-to-restore", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("explicitly rejected close did not abort terminal intent")
	}
	select {
	case got := <-transport.started:
		t.Fatalf("explicit rejection restarted live listener %q", got)
	default:
	}
}

func TestExplicitlyRejectedSessionCloseRestartsDisconnectedListener(t *testing.T) {
	transport := newSessionListenerTransport()
	transport.abortFound = false
	connection := NewClientConnection(acp.BaseClient{}, transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := connection.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := connection.CloseSession(ctx, acp.CloseSessionRequest{SessionID: "session-disconnected"})
		callDone <- err
	}()
	select {
	case <-transport.began:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("close did not mark terminal intent")
	}
	request := <-transport.outbox
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(request, &envelope); err != nil {
		t.Fatalf("decode outbound request: %v", err)
	}
	response := append([]byte(`{"jsonrpc":"2.0","id":`), envelope.ID...)
	response = append(response, []byte(`,"error":{"code":-32603,"message":"close failed"}}`)...)
	transport.inbox <- response
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("CloseSession error = nil, want explicit rejection")
		}
	case <-time.After(time.Second):
		t.Fatal("CloseSession did not return")
	}
	select {
	case got := <-transport.started:
		if got != "session-disconnected" {
			t.Fatalf("restarted session = %q, want session-disconnected", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("explicit rejection did not restart disconnected listener")
	}
}

func TestOutcomeUncertainSessionCloseDoesNotRestoreClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	if err := connection.Start(parentCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(parentCtx, 20*time.Millisecond)
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, err := connection.CloseSession(ctx, acp.CloseSessionRequest{SessionID: "session-unknown"})
		callDone <- err
	}()
	select {
	case got := <-transport.began:
		if got != "session-unknown" {
			t.Fatalf("stopped session = %q, want session-unknown", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome-uncertain close did not mark terminal intent before sending")
	}
	select {
	case <-transport.outbox:
	case <-time.After(time.Second):
		t.Fatal("close request was not written")
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CloseSession error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("outcome-uncertain close did not return")
	}
	select {
	case got := <-transport.forced:
		if got != "session-unknown" {
			t.Fatalf("force-closed session = %q, want session-unknown", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome-uncertain close did not force listener termination")
	}
	select {
	case got := <-transport.started:
		t.Fatalf("outcome-uncertain close restarted listener %q", got)
	default:
	}
}

func TestUnsentSessionCloseKeepsClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	_, err := connection.CloseSession(context.Background(), acp.CloseSessionRequest{
		SessionID: "session-unsent",
		Meta:      map[string]any{"invalid": make(chan struct{})},
	})
	if err == nil {
		t.Fatal("CloseSession error = nil, want params marshal failure")
	}
	select {
	case got := <-transport.began:
		t.Fatalf("unserializable close marked terminal intent for %q", got)
	default:
	}
	select {
	case <-transport.outbox:
		t.Fatal("unserializable close request was written")
	default:
	}
}

func TestPreCanceledSessionCloseKeepsClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := connection.CloseSession(ctx, acp.CloseSessionRequest{SessionID: "session-pre-canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CloseSession error = %v, want context canceled", err)
	}
	select {
	case got := <-transport.began:
		t.Fatalf("pre-canceled close marked terminal intent for %q", got)
	default:
	}
}

func TestNotStartedSessionCloseRestoresClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	_, err := connection.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: "session-not-started"})
	if !errors.Is(err, acptransport.ErrConnNotStarted) {
		t.Fatalf("CloseSession error = %v, want ErrConnNotStarted", err)
	}
	select {
	case got := <-transport.began:
		if got != "session-not-started" {
			t.Fatalf("stopped session = %q, want session-not-started", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("not-started close did not mark terminal intent")
	}
	select {
	case got := <-transport.aborted:
		if got != "session-not-started" {
			t.Fatalf("aborted close session = %q, want session-not-started", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("not-started close did not abort terminal intent")
	}
}

func TestSessionDeleteDoesNotChangeClientListener(t *testing.T) {
	transport := newSessionListenerTransport()
	connection := NewClientConnection(acp.BaseClient{}, transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := connection.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := connection.DeleteSession(ctx, acp.DeleteSessionRequest{SessionID: "active-deleted-session"})
		callDone <- err
	}()
	var request json.RawMessage
	select {
	case request = <-transport.outbox:
	case <-time.After(time.Second):
		t.Fatal("delete request was not written")
	}
	select {
	case got := <-transport.began:
		t.Fatalf("DeleteSession marked terminal intent for %q", got)
	default:
	}
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(request, &envelope); err != nil {
		t.Fatalf("decode outbound request: %v", err)
	}
	response := append([]byte(`{"jsonrpc":"2.0","id":`), envelope.ID...)
	response = append(response, []byte(`,"result":{}}`)...)
	transport.inbox <- response
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeleteSession did not return")
	}
	select {
	case got := <-transport.completed:
		t.Fatalf("DeleteSession completed listener %q", got)
	default:
	}
	select {
	case got := <-transport.aborted:
		t.Fatalf("DeleteSession aborted close intent for %q", got)
	default:
	}
	select {
	case got := <-transport.started:
		t.Fatalf("DeleteSession restarted listener %q", got)
	default:
	}
}
