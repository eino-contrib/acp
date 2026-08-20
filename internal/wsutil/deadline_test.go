package wsutil

import (
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// fakeNetError is a net.Error stub used to drive IsControlWriteContention
// without depending on either adapter's WebSocket library internals.
type fakeNetError struct {
	msg     string
	timeout bool
}

func (e *fakeNetError) Error() string   { return e.msg }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return e.timeout }

func TestIsControlWriteContention(t *testing.T) {
	t.Run("nil_error", func(t *testing.T) {
		if IsControlWriteContention(nil) {
			t.Fatal("nil should not be classified as contention")
		}
	})

	t.Run("non_net_error", func(t *testing.T) {
		if IsControlWriteContention(errors.New("boom")) {
			t.Fatal("plain error should not be classified as contention")
		}
	})

	t.Run("net_error_not_timeout", func(t *testing.T) {
		err := &fakeNetError{msg: "websocket: write timeout", timeout: false}
		if IsControlWriteContention(err) {
			t.Fatal("non-timeout net.Error must not be classified as contention")
		}
	})

	// Both supported Gorilla-family WebSocket libraries return errWriteTimeout (a
	// net.Error with msg="websocket: write timeout") only when WriteControl
	// could not acquire the connection's shared internal write lock within
	// the deadline. The connection is still usable in that case, so this
	// is the only Timeout() path we are willing to swallow.
	t.Run("websocket_write_lock_timeout_is_contention", func(t *testing.T) {
		err := &fakeNetError{msg: "websocket: write timeout", timeout: true}
		if !IsControlWriteContention(err) {
			t.Fatal("websocket write-lock timeout must be classified as contention")
		}
	})

	t.Run("websocket_write_lock_timeout_wrapped", func(t *testing.T) {
		inner := &fakeNetError{msg: "websocket: write timeout", timeout: true}
		err := fmt.Errorf("ping write: %w", inner)
		if !IsControlWriteContention(err) {
			t.Fatal("wrapped websocket write-lock timeout must still be classified as contention")
		}
	})

	// A real socket write deadline expiry surfaces as a *net.OpError
	// wrapping os.ErrDeadlineExceeded — also Timeout()==true, but the
	// connection is broken. This MUST NOT be swallowed, otherwise Client
	// ping_write_failed / Server-Proxy pong_write_failed convergence
	// (documented in feature-2026-05-28-ws-ping-pong) is silently lost.
	t.Run("socket_write_deadline_is_not_contention", func(t *testing.T) {
		err := &net.OpError{
			Op:  "write",
			Net: "tcp",
			Err: os.ErrDeadlineExceeded,
		}
		if !err.Timeout() {
			t.Fatalf("precondition: net.OpError should report Timeout()==true, got %v", err)
		}
		if IsControlWriteContention(err) {
			t.Fatal("real socket write deadline expiry must not be classified as contention")
		}
	})

	t.Run("generic_timeout_message_is_not_contention", func(t *testing.T) {
		// Anything that is Timeout() but does not carry the lock-wait
		// sentinel is treated as a real failure.
		err := &fakeNetError{msg: "i/o timeout", timeout: true}
		if IsControlWriteContention(err) {
			t.Fatal("generic timeout net.Error must not be classified as contention")
		}
	})

	t.Run("timeout_message_with_prefix_is_not_contention", func(t *testing.T) {
		err := &fakeNetError{msg: "wrapped: websocket: write timeout", timeout: true}
		if IsControlWriteContention(err) {
			t.Fatal("prefixed timeout message must not be classified as contention")
		}
	})

	t.Run("timeout_message_with_suffix_is_not_contention", func(t *testing.T) {
		err := &fakeNetError{msg: "websocket: write timeout: socket write failed", timeout: true}
		if IsControlWriteContention(err) {
			t.Fatal("suffixed timeout message must not be classified as contention")
		}
	})
}

// Ensure the public deadline constant has not been accidentally re-tuned
// without a coordinated update to the documented error-classification table
// (5s tolerates contention with concurrent data frame writes that share the
// same internal write lock; data frames may hold it for up to the 30s data
// write deadline).
func TestControlWriteDeadlineConstant(t *testing.T) {
	if ControlWriteDeadline != 5*time.Second {
		t.Fatalf("ControlWriteDeadline drifted to %v; update docs and metric tables before changing", ControlWriteDeadline)
	}
}
