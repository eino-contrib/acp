package httpserver

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBindStreamRejectedAfterConnectionWriterAdmissionCloses(t *testing.T) {
	conn := NewConnection()
	session := conn.EnsureSession("writer-admission-closed")

	conn.writersMu.Lock()
	conn.writersClosed = true
	conn.writersMu.Unlock()

	gen, evicted := session.BindStream(func(json.RawMessage) error {
		t.Fatal("rejected writer unexpectedly wrote a message")
		return nil
	})
	if gen == 0 {
		t.Fatal("BindStream did not allocate a generation")
	}
	select {
	case <-evicted:
	case <-time.After(time.Second):
		t.Fatal("rejected writer generation was not evicted")
	}
	if err := session.SendLive(json.RawMessage(`"message"`)); err == nil {
		t.Fatal("SendLive succeeded after writer admission was rejected")
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.writeFn != nil || session.outbox != nil || session.streamEvict != nil {
		t.Fatalf("rejected writer left live state: writeFn=%v outbox=%v evict=%v",
			session.writeFn != nil, session.outbox != nil, session.streamEvict != nil)
	}
}
