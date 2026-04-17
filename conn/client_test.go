package conn

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
)

type orderedSessionUpdateClient struct {
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

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"first","update":{"sessionUpdate":"current_mode_update"}}}`)
	select {
	case <-client.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first session/update did not start")
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"second","update":{"sessionUpdate":"current_mode_update"}}}`)
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
