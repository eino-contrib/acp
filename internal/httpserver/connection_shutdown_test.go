package httpserver

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConnectionCloseWaitsForBlockedSessionWriter(t *testing.T) {
	conn := NewConnection()
	session := conn.EnsureSession("blocked-shutdown-writer")
	session.writerStopTimeout = 10 * time.Millisecond

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	session.BindStream(func(json.RawMessage) error {
		close(writeStarted)
		<-releaseWrite
		return nil
	})
	if err := session.Send(json.RawMessage(`"message"`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("session writer did not start")
	}

	closeReturned := make(chan struct{})
	go func() {
		CloseConnection(conn)
		close(closeReturned)
	}()
	select {
	case <-closeReturned:
		close(releaseWrite)
		t.Fatal("connection close completed while its session writer was still blocked")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseWrite)
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("connection close did not finish after the writer exited")
	}
}
