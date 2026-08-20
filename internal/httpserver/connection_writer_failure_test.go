package httpserver

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A network write failure ends the lifetime of that SSE listener. Once its
// writer has exited, SendLive must fail fast and ordinary Send must queue for
// the next listener instead of writing into the abandoned outbox.
func TestSessionWriterFailureMakesGenerationInactive(t *testing.T) {
	sess := NewConnection().EnsureSession("writer-failure")
	defer sess.CloseSession()

	writeErr := errors.New("broken SSE stream")
	gen, evicted := sess.BindStream(func(json.RawMessage) error {
		return writeErr
	})

	// Capture writerDone only to establish that the failed writer has fully
	// exited before checking the state it leaves behind.
	sess.mu.Lock()
	writerDone := sess.writerDone
	sess.mu.Unlock()
	if writerDone == nil {
		t.Fatal("BindStream did not start a writer")
	}

	if err := sess.SendLive(json.RawMessage(`"triggers failure"`)); err != nil {
		t.Fatalf("initial SendLive: %v", err)
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("failed writer did not exit")
	}
	waitForSignal(t, evicted, "failed generation eviction")

	sess.mu.Lock()
	if sess.writeFn != nil || sess.outbox != nil || sess.writerStop != nil || sess.writerDone != nil || sess.streamEvict != nil {
		sess.mu.Unlock()
		t.Fatal("failed writer left live stream state published")
	}
	sess.mu.Unlock()
	if got := sess.activeWriterGen.Load(); got != 0 {
		t.Fatalf("active writer generation after failure = %d, want 0", got)
	}

	if err := sess.SendLive(json.RawMessage(`"must fail fast"`)); !errors.Is(err, ErrSessionNoActiveStream) {
		t.Fatalf("SendLive after writer failure = %v, want ErrSessionNoActiveStream", err)
	}

	queued := json.RawMessage(`"queued for replacement"`)
	if err := sess.Send(queued); err != nil {
		t.Fatalf("Send after writer failure: %v", err)
	}

	received := make(chan json.RawMessage, 1)
	replacementGen, _ := sess.BindStream(func(msg json.RawMessage) error {
		received <- append(json.RawMessage(nil), msg...)
		return nil
	})
	defer sess.UnbindStream(replacementGen)

	select {
	case got := <-received:
		if string(got) != string(queued) {
			t.Fatalf("replacement listener received %s, want %s", got, queued)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement listener did not receive pending message")
	}

	// The old generation token must not unbind the replacement listener.
	sess.UnbindStream(gen)
	if err := sess.SendLive(json.RawMessage(`"replacement remains live"`)); err != nil {
		t.Fatalf("SendLive after stale UnbindStream: %v", err)
	}
}

// A write failure must not discard messages that later Send calls already
// transferred to the failed generation's outbox. The failed in-flight message
// has an indeterminate delivery outcome, but the buffered tail was never
// presented to writeFn and must be handed to the next listener in FIFO order.
func TestSessionWriterFailurePreservesAcceptedOutboxTail(t *testing.T) {
	sess := NewConnection().EnsureSession("writer-failure-preserves-tail")
	defer sess.CloseSession()

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	_, evicted := sess.BindStream(func(msg json.RawMessage) error {
		if string(msg) != `"first"` {
			t.Errorf("failed listener received %s, want first message", msg)
		}
		close(writeStarted)
		<-releaseWrite
		return errors.New("broken SSE stream")
	})

	if err := sess.SendLive(json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("send first message: %v", err)
	}
	waitForSignal(t, writeStarted, "failed listener write")

	// The writer is deterministically blocked on the first message, so this
	// successful send leaves second buffered in the old generation's outbox.
	if err := sess.Send(json.RawMessage(`"second"`)); err != nil {
		t.Fatalf("send first buffered tail message: %v", err)
	}
	if err := sess.Send(json.RawMessage(`"third"`)); err != nil {
		t.Fatalf("send second buffered tail message: %v", err)
	}
	close(releaseWrite)
	waitForSignal(t, evicted, "failed generation eviction")

	received := make(chan json.RawMessage, 2)
	replacementGen, _ := sess.BindStream(func(msg json.RawMessage) error {
		received <- append(json.RawMessage(nil), msg...)
		return nil
	})
	defer sess.UnbindStream(replacementGen)

	for _, want := range []string{`"second"`, `"third"`} {
		select {
		case got := <-received:
			if string(got) != want {
				t.Fatalf("replacement received %s, want %s", got, want)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("accepted outbox tail lost after writer failure; waiting for %s", want)
		}
	}
}

// BindStream writes pre-listener pending messages synchronously. A failure in
// that phase must retire the generation just like an asynchronous writer
// failure, while preserving the failed message and messages concurrently
// queued during the write for the replacement listener.
func TestSessionPendingFlushFailureMakesGenerationInactive(t *testing.T) {
	sess := NewConnection().EnsureSession("pending-flush-failure")
	defer sess.CloseSession()

	first := json.RawMessage(`"first"`)
	second := json.RawMessage(`"second"`)
	third := json.RawMessage(`"third"`)
	if err := sess.Send(first); err != nil {
		t.Fatalf("queue first message: %v", err)
	}

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	type bindResult struct {
		gen     uint64
		evicted <-chan struct{}
	}
	bindDone := make(chan bindResult, 1)
	go func() {
		gen, evicted := sess.BindStream(func(json.RawMessage) error {
			close(writeStarted)
			<-releaseWrite
			return errors.New("pending flush failed")
		})
		bindDone <- bindResult{gen: gen, evicted: evicted}
	}()
	waitForSignal(t, writeStarted, "pending flush write")

	// writeFn remains unpublished during a synchronous pending drain, so this
	// message joins pending and must follow the failed first message.
	if err := sess.Send(second); err != nil {
		t.Fatalf("queue during failed flush: %v", err)
	}
	close(releaseWrite)

	var failed bindResult
	select {
	case failed = <-bindDone:
	case <-time.After(time.Second):
		t.Fatal("failed BindStream did not return")
	}
	waitForSignal(t, failed.evicted, "failed pending-flush generation eviction")
	if err := sess.SendLive(json.RawMessage(`"must fail fast"`)); !errors.Is(err, ErrSessionNoActiveStream) {
		t.Fatalf("SendLive after pending flush failure = %v, want ErrSessionNoActiveStream", err)
	}
	if err := sess.Send(third); err != nil {
		t.Fatalf("queue after failed flush: %v", err)
	}

	var (
		receivedMu sync.Mutex
		received   []string
	)
	replacementGen, _ := sess.BindStream(func(msg json.RawMessage) error {
		receivedMu.Lock()
		received = append(received, string(msg))
		receivedMu.Unlock()
		return nil
	})
	defer sess.UnbindStream(replacementGen)

	receivedMu.Lock()
	got := append([]string(nil), received...)
	receivedMu.Unlock()
	want := []string{string(first), string(second), string(third)}
	if len(got) != len(want) {
		t.Fatalf("replacement listener received %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replacement listener received %v, want %v", got, want)
		}
	}

	// A stale cleanup from the failed bind cannot retire the replacement.
	sess.UnbindStream(failed.gen)
	if err := sess.SendLive(json.RawMessage(`"replacement remains live"`)); err != nil {
		t.Fatalf("SendLive after stale pending-flush UnbindStream: %v", err)
	}
}

// A replacement may detach an old writer while that writer is blocked inside
// writeFn. If the wait later ends with an error, its cleanup must not clear the
// replacement generation installed by BindStream.
func TestSessionWriterFailureRacingRebindKeepsReplacementLive(t *testing.T) {
	sess := NewConnection().EnsureSession("writer-failure-rebind")
	defer sess.CloseSession()

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	oldGen, oldEvicted := sess.BindStream(func(json.RawMessage) error {
		close(writeStarted)
		<-releaseWrite
		return errors.New("old listener failed")
	})
	if err := sess.SendLive(json.RawMessage(`"old listener message"`)); err != nil {
		t.Fatalf("SendLive to old listener: %v", err)
	}
	waitForSignal(t, writeStarted, "old listener write")

	replacementReceived := make(chan json.RawMessage, 1)
	type bindResult struct {
		gen     uint64
		evicted <-chan struct{}
	}
	rebindDone := make(chan bindResult, 1)
	go func() {
		gen, evicted := sess.BindStream(func(msg json.RawMessage) error {
			replacementReceived <- append(json.RawMessage(nil), msg...)
			return nil
		})
		rebindDone <- bindResult{gen: gen, evicted: evicted}
	}()

	// Closing oldEvicted proves BindStream detached the old writer before its
	// writeFn returns an error and attempts the conditional cleanup.
	waitForSignal(t, oldEvicted, "old listener eviction by replacement")
	close(releaseWrite)

	var replacement bindResult
	select {
	case replacement = <-rebindDone:
	case <-time.After(time.Second):
		t.Fatal("replacement BindStream did not return")
	}
	defer sess.UnbindStream(replacement.gen)

	sess.UnbindStream(oldGen)
	message := json.RawMessage(`"new listener message"`)
	if err := sess.SendLive(message); err != nil {
		t.Fatalf("SendLive to replacement: %v", err)
	}
	select {
	case got := <-replacementReceived:
		if string(got) != string(message) {
			t.Fatalf("replacement received %s, want %s", got, message)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not receive live message")
	}
	if got := sess.activeWriterGen.Load(); got != replacement.gen {
		t.Fatalf("active writer generation = %d, want replacement %d", got, replacement.gen)
	}
}

// CloseSession and a write failure can discover the same old writer at nearly
// the same time. Exactly one path owns shared teardown; the other must no-op
// without double-closing channels or reviving the session.
func TestSessionWriterFailureRacingCloseLeavesSessionClosed(t *testing.T) {
	sess := NewConnection().EnsureSession("writer-failure-close")
	sess.writerStopTimeout = 10 * time.Millisecond

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	_, evicted := sess.BindStream(func(json.RawMessage) error {
		close(writeStarted)
		<-releaseWrite
		return errors.New("listener failed during close")
	})
	if err := sess.SendLive(json.RawMessage(`"message"`)); err != nil {
		t.Fatalf("SendLive: %v", err)
	}
	waitForSignal(t, writeStarted, "listener write before close")

	closeDone := make(chan struct{})
	go func() {
		sess.CloseSession()
		close(closeDone)
	}()
	waitForSignal(t, evicted, "listener eviction during close")
	close(releaseWrite)
	waitForSignal(t, closeDone, "CloseSession completion")

	if err := sess.Send(json.RawMessage(`"after close"`)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Send after close = %v, want ErrSessionClosed", err)
	}
	if err := sess.SendLive(json.RawMessage(`"after close"`)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("SendLive after close = %v, want ErrSessionClosed", err)
	}
	sess.mu.Lock()
	if sess.writeFn != nil || sess.outbox != nil || sess.writerStop != nil || sess.writerDone != nil || sess.streamEvict != nil {
		sess.mu.Unlock()
		t.Fatal("closed session retained writer state")
	}
	sess.mu.Unlock()
	if got := sess.activeWriterGen.Load(); got != 0 {
		t.Fatalf("active writer generation after close = %d, want 0", got)
	}
}
