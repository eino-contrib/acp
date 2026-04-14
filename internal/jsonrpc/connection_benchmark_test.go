package jsonrpc

import (
	"context"
	"encoding/json"
	"testing"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
)

func BenchmarkConnectionRequestRoundTrip(b *testing.B) {
	transport := newChannelTransport()
	conn := NewConnection(transport, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	safe.GoWithLogger(acplog.Default(), func() {
		_ = conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		b.Fatalf("wait until started: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case raw := <-transport.outbox:
				var msg Message
				if err := json.Unmarshal(raw, &msg); err != nil {
					continue
				}
				if !msg.IsRequest() {
					continue
				}
				resp, err := NewResponse(msg.ID, map[string]bool{"ok": true})
				if err != nil {
					continue
				}
				data, err := json.Marshal(resp)
				if err != nil {
					continue
				}
				transport.inbox <- data
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.SendRequest(context.Background(), "bench/request", map[string]int{"n": i}); err != nil {
			b.Fatalf("SendRequest: %v", err)
		}
	}
	b.StopTimer()

	_ = conn.Close()
}
