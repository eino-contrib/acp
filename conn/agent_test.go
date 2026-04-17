package conn

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
)

type channelTransport struct {
	inbox  chan json.RawMessage
	outbox chan json.RawMessage
	closed chan struct{}
	once   sync.Once
}

func newChannelTransport() *channelTransport {
	return &channelTransport{
		inbox:  make(chan json.RawMessage, 16),
		outbox: make(chan json.RawMessage, 16),
		closed: make(chan struct{}),
	}
}

func (t *channelTransport) ReadMessage(context.Context) (json.RawMessage, error) {
	select {
	case msg := <-t.inbox:
		return msg, nil
	case <-t.closed:
		return nil, io.EOF
	}
}

func (t *channelTransport) WriteMessage(_ context.Context, data json.RawMessage) error {
	select {
	case t.outbox <- append(json.RawMessage(nil), data...):
		return nil
	case <-t.closed:
		return io.EOF
	}
}

func (t *channelTransport) Close() error {
	t.once.Do(func() {
		close(t.closed)
	})
	return nil
}

type blockingPromptAgent struct {
	acp.BaseAgent

	started       chan struct{}
	sessionCancel chan struct{}
}

func (a *blockingPromptAgent) Prompt(ctx context.Context, _ acp.PromptRequest) (acp.PromptResponse, error) {
	a.started <- struct{}{}
	<-ctx.Done()
	return acp.PromptResponse{}, ctx.Err()
}

func (a *blockingPromptAgent) SessionCancel(context.Context, acp.CancelNotification) error {
	select {
	case a.sessionCancel <- struct{}{}:
	default:
	}
	return nil
}

func TestAgentSideConnectionReportsUnknownNotification(t *testing.T) {
	transport := newChannelTransport()
	agent := &blockingPromptAgent{
		started:       make(chan struct{}, 1),
		sessionCancel: make(chan struct{}, 1),
	}
	conn := NewAgentConnectionFromTransport(agent, transport)
	reported := make(chan error, 1)
	conn.jsonrpcConn.OnError = func(method string, err error) {
		reported <- err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/unknown","params":{"sessionId":"session-1"}}`)

	select {
	case err := <-reported:
		if err == nil || err.Error() != "unsupported notification: session/unknown" {
			t.Fatalf("reported err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected unknown notification to be reported")
	}
}

func TestAgentSideConnectionReportsUnimplementedSessionCancel(t *testing.T) {
	transport := newChannelTransport()
	conn := NewAgentConnectionFromTransport(acp.BaseAgent{}, transport)
	reported := make(chan error, 1)
	conn.jsonrpcConn.OnError = func(method string, err error) {
		reported <- err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"session-1"}}`)

	select {
	case err := <-reported:
		if err == nil || err.Error() != "notification handler not implemented: "+acp.MethodAgentSessionCancel {
			t.Fatalf("reported err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected session/cancel to report unimplemented handler")
	}
}
