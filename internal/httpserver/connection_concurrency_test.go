package httpserver

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// A listener replacement must preserve messages that SendLive already
// accepted into the old writer's outbox. Returning nil transfers ownership to
// the session; abandoning that outbox would silently lose notifications and
// leave reverse requests waiting for responses to messages the client never
// received.
func TestListenerReplacementPreservesAcceptedOutboxMessages(t *testing.T) {
	sess := NewConnection().EnsureSession("replace-preserves-outbox")
	defer sess.CloseSession()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	_, oldEvicted := sess.BindStream(func(msg json.RawMessage) error {
		if string(msg) == `"first"` {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})

	if err := sess.SendLive(json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("send first message: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("old listener did not start its first write")
	}

	if err := sess.SendLive(json.RawMessage(`"second"`)); err != nil {
		t.Fatalf("send queued message: %v", err)
	}

	received := make(chan json.RawMessage, 1)
	bindDone := make(chan struct{})
	go func() {
		sess.BindStream(func(msg json.RawMessage) error {
			received <- append(json.RawMessage(nil), msg...)
			return nil
		})
		close(bindDone)
	}()

	select {
	case <-oldEvicted:
	case <-time.After(time.Second):
		t.Fatal("replacement did not evict the old listener")
	}
	close(releaseFirst)
	select {
	case <-bindDone:
	case <-time.After(time.Second):
		t.Fatal("replacement listener did not finish binding")
	}

	select {
	case got := <-received:
		if string(got) != `"second"` {
			t.Fatalf("replacement received %s, want second message", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("accepted outbox message was lost during listener replacement")
	}
}

func TestListenerUnbindPreservesAcceptedOutboxMessages(t *testing.T) {
	sess := NewConnection().EnsureSession("unbind-preserves-outbox")
	defer sess.CloseSession()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	gen, _ := sess.BindStream(func(msg json.RawMessage) error {
		if string(msg) == `"first"` {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})
	if err := sess.SendLive(json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("send first message: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("listener did not start its first write")
	}
	if err := sess.Send(json.RawMessage(`"second"`)); err != nil {
		t.Fatalf("send queued message: %v", err)
	}

	unbound := make(chan struct{})
	go func() {
		sess.UnbindStream(gen)
		close(unbound)
	}()
	for {
		sess.mu.Lock()
		detached := sess.outbox == nil
		sess.mu.Unlock()
		if detached {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseFirst)
	select {
	case <-unbound:
	case <-time.After(time.Second):
		t.Fatal("listener did not unbind")
	}

	received := make(chan json.RawMessage, 1)
	replacementGen, _ := sess.BindStream(func(msg json.RawMessage) error {
		received <- append(json.RawMessage(nil), msg...)
		return nil
	})
	defer sess.UnbindStream(replacementGen)
	select {
	case got := <-received:
		if string(got) != `"second"` {
			t.Fatalf("replacement received %s, want second message", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("accepted outbox message was lost during listener unbind")
	}
}

// Removing a session after a successful session/close must not discard
// notifications that the close handler already handed to the active SSE
// generation. Send returning nil transfers ownership to the session, so the
// terminal removal has to drain the accepted tail before evicting the stream.
func TestRemoveSessionDrainsAcceptedOutboxMessages(t *testing.T) {
	conn := NewConnection()
	sess := conn.EnsureSession("close-drains-outbox")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	received := make(chan string, 2)
	_, evicted := sess.BindStream(func(msg json.RawMessage) error {
		if string(msg) == `"first"` {
			close(firstStarted)
			<-releaseFirst
		}
		received <- string(msg)
		return nil
	})

	if err := sess.Send(json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("send first message: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("session writer did not start the first write")
	}
	if err := sess.Send(json.RawMessage(`"second"`)); err != nil {
		t.Fatalf("send accepted tail message: %v", err)
	}

	removed := make(chan struct{})
	go func() {
		conn.RemoveSession(sess.SessionID)
		close(removed)
	}()

	select {
	case <-removed:
		t.Fatal("session removal returned before the in-flight SSE write completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("session removal did not finish after the SSE writer drained")
	}

	for _, want := range []string{`"first"`, `"second"`} {
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("received %s, want %s", got, want)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("accepted message %s was lost during session removal", want)
		}
	}
	select {
	case <-evicted:
	default:
		t.Fatal("terminal session removal did not evict the SSE listener")
	}
}

func TestConcurrentListenerUnbindAndReplacementPreservesAcceptedOutboxMessages(t *testing.T) {
	sess := NewConnection().EnsureSession("concurrent-unbind-replacement")
	defer sess.CloseSession()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	oldGen, _ := sess.BindStream(func(msg json.RawMessage) error {
		if string(msg) == `"first"` {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})
	if err := sess.SendLive(json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("send first message: %v", err)
	}
	<-firstStarted
	if err := sess.Send(json.RawMessage(`"second"`)); err != nil {
		t.Fatalf("send queued message: %v", err)
	}

	unbound := make(chan struct{})
	go func() {
		sess.UnbindStream(oldGen)
		close(unbound)
	}()
	for {
		sess.mu.Lock()
		detached := sess.outbox == nil
		sess.mu.Unlock()
		if detached {
			break
		}
		time.Sleep(time.Millisecond)
	}

	received := make(chan json.RawMessage, 1)
	replacementBound := make(chan uint64, 1)
	go func() {
		gen, _ := sess.BindStream(func(msg json.RawMessage) error {
			received <- append(json.RawMessage(nil), msg...)
			return nil
		})
		replacementBound <- gen
	}()
	select {
	case <-replacementBound:
		t.Fatal("replacement bound before UnbindStream completed the old-generation handoff")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-unbound:
	case <-time.After(time.Second):
		t.Fatal("old listener did not finish unbinding")
	}
	var replacementGen uint64
	select {
	case replacementGen = <-replacementBound:
	case <-time.After(time.Second):
		t.Fatal("replacement did not bind after UnbindStream completed the handoff")
	}
	defer sess.UnbindStream(replacementGen)
	select {
	case got := <-received:
		if string(got) != `"second"` {
			t.Fatalf("replacement received %s, want second message", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("accepted old outbox message was stranded after concurrent Unbind/Bind")
	}
}

// A replacement listener drops Session.mu while it waits for the old writer.
// Closing the session in that window must not close the old eviction channel a
// second time: an unhandled panic in an HTTP handler goroutine terminates the
// entire process.
func TestSessionCloseDuringListenerReplacementDoesNotPanic(t *testing.T) {
	sess := NewConnection().EnsureSession("concurrent-replacement")
	sess.writerStopTimeout = 10 * time.Millisecond
	writeStarted := make(chan struct{})
	unblockWrite := make(chan struct{})
	var startedOnce sync.Once

	_, oldEvicted := sess.BindStream(func(json.RawMessage) error {
		startedOnce.Do(func() { close(writeStarted) })
		<-unblockWrite
		return nil
	})
	if err := sess.Send(json.RawMessage(`"message"`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("old listener did not start its write")
	}

	rebindDone := make(chan struct{})
	go func() {
		defer close(rebindDone)
		sess.BindStream(func(json.RawMessage) error { return nil })
	}()
	select {
	case <-oldEvicted:
	case <-time.After(time.Second):
		t.Fatal("replacement listener did not evict the old listener")
	}

	closeDone := make(chan any, 1)
	go func() {
		defer func() { closeDone <- recover() }()
		sess.CloseSession()
	}()
	select {
	case recovered := <-closeDone:
		if recovered != nil {
			t.Fatalf("CloseSession panicked during listener replacement: %v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseSession blocked during listener replacement")
	}

	close(unblockWrite)
	select {
	case <-rebindDone:
	case <-time.After(time.Second):
		t.Fatal("replacement listener did not return after old write completed")
	}
}

// Listener replacements must retain call order even though each replacement
// temporarily drops Session.mu while an old network writer winds down. Two
// concurrent BindStream calls used to observe and close the same streamEvict
// channel, producing the same process-fatal close-of-closed-channel panic as a
// concurrent CloseSession.
func TestConcurrentSessionListenerReplacementsDoNotPanic(t *testing.T) {
	sess := NewConnection().EnsureSession("concurrent-rebind")
	defer sess.CloseSession()

	writeStarted := make(chan struct{})
	unblockWrite := make(chan struct{})
	var startedOnce sync.Once
	_, oldEvicted := sess.BindStream(func(json.RawMessage) error {
		startedOnce.Do(func() { close(writeStarted) })
		<-unblockWrite
		return nil
	})
	if err := sess.Send(json.RawMessage(`"message"`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("old listener did not start its write")
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		sess.BindStream(func(json.RawMessage) error { return nil })
	}()
	select {
	case <-oldEvicted:
	case <-time.After(time.Second):
		t.Fatal("first replacement did not evict the old listener")
	}

	secondDone := make(chan struct{})
	secondStarted := make(chan struct{})
	go func() {
		defer close(secondDone)
		close(secondStarted)
		sess.BindStream(func(json.RawMessage) error { return nil })
	}()
	<-secondStarted
	select {
	case <-secondDone:
		t.Fatal("second replacement completed while the first was still winding down")
	case <-time.After(20 * time.Millisecond):
		// The second call is queued behind bindMu until the first replacement
		// finishes waiting for the old writer. Before the fix it entered the
		// unprotected window and closed the same streamEvict channel again.
	}

	close(unblockWrite)
	for name, done := range map[string]<-chan struct{}{
		"first replacement":  firstDone,
		"second replacement": secondDone,
	} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s did not return", name)
		}
	}
}
