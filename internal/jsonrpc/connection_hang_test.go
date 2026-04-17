package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type dummyTransport struct {
	readCh chan []byte
}

func (t *dummyTransport) ReadMessage(ctx context.Context) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg, ok := <-t.readCh:
		if !ok {
			return nil, fmt.Errorf("closed")
		}
		return msg, nil
	}
}

func (t *dummyTransport) WriteMessage(ctx context.Context, data json.RawMessage) error {
	return nil
}

func (t *dummyTransport) Close() error {
	return nil
}

func TestSendRequestHang(t *testing.T) {
	trans := &dummyTransport{readCh: make(chan []byte)}
	
	// Create connection with a handler that hangs forever, ignoring context done!
	conn := NewConnection(trans, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		t.Log("Handler triggered, hanging forever ignoring ctx...")
		time.Sleep(5 * time.Second) // Hang forever
		t.Log("Handler ctx done")
		return nil, nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	
	// Start connection
	go conn.Start(ctx)
	
	err := conn.WaitUntilStarted(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Trigger the hanging handler via an incoming request
	trans.readCh <- []byte(`{"jsonrpc":"2.0", "id": 1, "method": "test"}`)

	time.Sleep(50 * time.Millisecond)

	// Now try to send a request in a separate goroutine
	reqDone := make(chan struct{})
	go func() {
		t.Log("Sending request...")
		// Intentionally not passing a timeout context
		_, err := conn.SendRequest(context.Background(), "call", nil)
		t.Log("SendRequest returned:", err)
		close(reqDone)
	}()

	time.Sleep(50 * time.Millisecond)

	// We DON'T cancel ctx or call Close(). We just close the transport (like EOF)
	t.Log("Closing transport...")
	close(trans.readCh)

	// Wait to see if SendRequest unblocks
	select {
	case <-reqDone:
		t.Log("SUCCESS: SendRequest unblocked!")
	case <-time.After(1 * time.Second):
		t.Log("FAIL: SendRequest is stuck forever!")
		t.FailNow()
	}
	cancel() // cleanup
}
