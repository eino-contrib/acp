package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	acplog "github.com/eino-contrib/acp/internal/log"
)

func TestWebSocketClientTransportCloseDoesNotBlockOnBusyWriter(t *testing.T) {
	conn := newBlockingWriteMessageConn()

	transport := &WebSocketClientTransport{
		wsConn:      conn,
		connected:   true,
		inbox:       make(chan json.RawMessage, 1),
		done:        make(chan struct{}),
		logger:      acplog.Default(),
		writePermit: make(chan struct{}, 1),
	}
	transport.writePermit <- struct{}{}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- transport.WriteMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	}()

	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- transport.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Close() blocked behind an in-flight websocket write")
	}

	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("WriteMessage unexpectedly succeeded after transport close")
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("WriteMessage error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WriteMessage did not unblock after Close")
	}
}
