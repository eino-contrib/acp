package httpserver

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestConnectionCloseTracksWriterAfterSessionOverflowRemoval(t *testing.T) {
	conn := NewConnection()
	conn.PendingQueueSize = 1
	session := conn.EnsureSession("overflow-with-detached-writer")
	session.writerStopTimeout = 10 * time.Millisecond

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	_, oldEvicted := session.BindStream(func(json.RawMessage) error {
		close(writeStarted)
		<-releaseWrite
		return nil
	})
	if err := session.Send(json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("Send first: %v", err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("old writer did not start")
	}

	replacementBound := make(chan uint64, 1)
	go func() {
		gen, _ := session.BindStream(func(json.RawMessage) error { return nil })
		replacementBound <- gen
	}()
	select {
	case <-oldEvicted:
	case <-time.After(time.Second):
		close(releaseWrite)
		t.Fatal("old listener was not evicted")
	}
	var replacementGen uint64
	select {
	case replacementGen = <-replacementBound:
	case <-time.After(time.Second):
		close(releaseWrite)
		t.Fatal("replacement listener was not bound after bounded handoff wait")
	}
	session.UnbindStream(replacementGen)

	if err := session.Send(json.RawMessage(`"pending"`)); err != nil {
		close(releaseWrite)
		t.Fatalf("fill pending queue: %v", err)
	}
	if err := session.Send(json.RawMessage(`"overflow"`)); !errors.Is(err, ErrSessionClosed) {
		close(releaseWrite)
		t.Fatalf("overflow Send = %v, want ErrSessionClosed", err)
	}
	if got := conn.SessionCount(); got != 0 {
		close(releaseWrite)
		t.Fatalf("session count after overflow = %d, want 0", got)
	}

	closeReturned := make(chan struct{})
	go func() {
		CloseConnection(conn)
		close(closeReturned)
	}()
	select {
	case <-closeReturned:
		close(releaseWrite)
		t.Fatal("connection close completed while a removed session writer was blocked")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseWrite)
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("connection close did not finish after removed writer exited")
	}
}
