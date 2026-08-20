package wsutil

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eino-contrib/acp/internal/wsconn"
)

// websocketWriteLockTimeoutErr is a local stand-in for the unexported
// Gorilla-family write-lock sentinel. It is kept inline so this test remains
// self-contained.
type websocketWriteLockTimeoutErr struct{}

func (websocketWriteLockTimeoutErr) Error() string   { return "websocket: write timeout" }
func (websocketWriteLockTimeoutErr) Timeout() bool   { return true }
func (websocketWriteLockTimeoutErr) Temporary() bool { return true }

// TestPongResponderContentionKeepsConnectionAlive verifies the connection-
// survival contract for Pong write-lock contention documented in
// docs/feature-2026-05-28-ws-ping-pong.md: receiving a Ping is itself proof
// the peer is alive, so a Pong write that loses the shared write-lock race
// must NOT propagate an error to the read loop and MUST refresh the read
// deadline (otherwise persistent contention would let the deadline expire
// on a healthy connection). OnContention runs; OnWriteFailed and
// WrapWriteFailed must not.
func TestPongResponderContentionKeepsConnectionAlive(t *testing.T) {
	var (
		contentionCount   atomic.Int32
		writeFailedCount  atomic.Int32
		wrapWriteFailedHt atomic.Int32
		setDeadlineCount  atomic.Int32
		refreshAsked      atomic.Int32
	)

	r := PongResponder{
		WriteControl: func(messageType int, data []byte, deadline time.Time) error {
			if messageType != wsconn.PongMessage {
				t.Fatalf("expected PongMessage, got %d", messageType)
			}
			return websocketWriteLockTimeoutErr{}
		},
		SetReadDeadline: func(time.Time) error {
			setDeadlineCount.Add(1)
			return nil
		},
		ReadTimeout: 50 * time.Millisecond,
		RefreshDeadline: func() bool {
			refreshAsked.Add(1)
			return true
		},
		OnContention: func(err error) {
			if !IsControlWriteContention(err) {
				t.Errorf("OnContention called with non-contention err: %v", err)
			}
			contentionCount.Add(1)
		},
		OnWriteFailed: func(error) {
			writeFailedCount.Add(1)
		},
		WrapWriteFailed: func(err error) error {
			wrapWriteFailedHt.Add(1)
			return err
		},
	}

	if err := r.Handler()("ping-payload"); err != nil {
		t.Fatalf("contention must not propagate error to read loop, got: %v", err)
	}

	if got := contentionCount.Load(); got != 1 {
		t.Errorf("expected OnContention to fire exactly once, got %d", got)
	}
	if got := writeFailedCount.Load(); got != 0 {
		t.Errorf("expected OnWriteFailed not to fire on contention, got %d", got)
	}
	if got := wrapWriteFailedHt.Load(); got != 0 {
		t.Errorf("expected WrapWriteFailed not to fire on contention, got %d", got)
	}
	if got := refreshAsked.Load(); got != 1 {
		t.Errorf("expected RefreshDeadline to be consulted exactly once, got %d", got)
	}
	if got := setDeadlineCount.Load(); got != 1 {
		t.Errorf("expected SetReadDeadline to be called once on contention, got %d", got)
	}
}

// TestPongResponderContentionDoesNotRefreshWhenDisabled verifies the
// deadline-refresh guards: when ReadTimeout==0 OR RefreshDeadline==nil OR
// RefreshDeadline()==false, the contention path must NOT touch the read
// deadline. This protects e.g. the proxy's "do not refresh before the first
// data frame" rule from being short-circuited by a contention path.
func TestPongResponderContentionDoesNotRefreshWhenDisabled(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*PongResponder)
		askedMin int32
	}{
		{
			name: "read_timeout_zero",
			mutate: func(r *PongResponder) {
				r.ReadTimeout = 0
				r.RefreshDeadline = func() bool { return true }
			},
		},
		{
			name: "refresh_nil",
			mutate: func(r *PongResponder) {
				r.ReadTimeout = 50 * time.Millisecond
				r.RefreshDeadline = nil
			},
		},
		{
			name: "refresh_returns_false",
			mutate: func(r *PongResponder) {
				r.ReadTimeout = 50 * time.Millisecond
				r.RefreshDeadline = func() bool { return false }
			},
			askedMin: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				setDeadlineCount atomic.Int32
				askedCount       atomic.Int32
			)
			r := PongResponder{
				WriteControl: func(int, []byte, time.Time) error {
					return websocketWriteLockTimeoutErr{}
				},
				SetReadDeadline: func(time.Time) error {
					setDeadlineCount.Add(1)
					return nil
				},
				OnContention: func(error) {},
			}
			tc.mutate(&r)
			if r.RefreshDeadline != nil {
				orig := r.RefreshDeadline
				r.RefreshDeadline = func() bool {
					askedCount.Add(1)
					return orig()
				}
			}

			if err := r.Handler()(""); err != nil {
				t.Fatalf("contention must not propagate, got: %v", err)
			}
			if got := setDeadlineCount.Load(); got != 0 {
				t.Errorf("expected SetReadDeadline not to fire when refresh is disabled, got %d", got)
			}
			if tc.askedMin > 0 && askedCount.Load() < tc.askedMin {
				t.Errorf("expected RefreshDeadline to be consulted at least %d time(s), got %d", tc.askedMin, askedCount.Load())
			}
		})
	}
}

// TestPongResponderNonContentionFailureSurfaces verifies the converse path:
// a non-contention WriteControl failure (e.g. a real socket write deadline
// expiry) must invoke OnWriteFailed, run WrapWriteFailed exactly once, and
// surface the (possibly wrapped) error so the read loop can tear the
// connection down. OnContention and the deadline-refresh path must NOT run.
func TestPongResponderNonContentionFailureSurfaces(t *testing.T) {
	var (
		contentionCount  atomic.Int32
		writeFailedCount atomic.Int32
		wrappedCount     atomic.Int32
		setDeadlineCount atomic.Int32
	)

	sentinel := errors.New("real write failure")
	wrapped := errors.New("wrapped: real write failure")

	r := PongResponder{
		WriteControl: func(int, []byte, time.Time) error {
			return sentinel
		},
		SetReadDeadline: func(time.Time) error {
			setDeadlineCount.Add(1)
			return nil
		},
		ReadTimeout:     50 * time.Millisecond,
		RefreshDeadline: func() bool { return true },
		OnContention: func(error) {
			contentionCount.Add(1)
		},
		OnWriteFailed: func(err error) {
			if !errors.Is(err, sentinel) {
				t.Errorf("OnWriteFailed got unexpected err: %v", err)
			}
			writeFailedCount.Add(1)
		},
		WrapWriteFailed: func(err error) error {
			if !errors.Is(err, sentinel) {
				t.Errorf("WrapWriteFailed got unexpected err: %v", err)
			}
			wrappedCount.Add(1)
			return wrapped
		},
	}

	got := r.Handler()("")
	if got != wrapped {
		t.Fatalf("expected wrapped error to surface, got: %v", got)
	}

	if c := contentionCount.Load(); c != 0 {
		t.Errorf("expected OnContention not to fire on non-contention failure, got %d", c)
	}
	if c := writeFailedCount.Load(); c != 1 {
		t.Errorf("expected OnWriteFailed to fire once, got %d", c)
	}
	if c := wrappedCount.Load(); c != 1 {
		t.Errorf("expected WrapWriteFailed to fire once, got %d", c)
	}
	if c := setDeadlineCount.Load(); c != 0 {
		t.Errorf("expected SetReadDeadline not to fire on non-contention failure, got %d", c)
	}
}

// TestPongResponderSuccessRefreshesReadDeadline verifies the happy path:
// a successful Pong write refreshes the read deadline (when configured) and
// returns nil. None of the failure callbacks should run.
func TestPongResponderSuccessRefreshesReadDeadline(t *testing.T) {
	var (
		setDeadlineCount atomic.Int32
		writeFailedCount atomic.Int32
		contentionCount  atomic.Int32
	)

	r := PongResponder{
		WriteControl: func(int, []byte, time.Time) error { return nil },
		SetReadDeadline: func(time.Time) error {
			setDeadlineCount.Add(1)
			return nil
		},
		ReadTimeout:     50 * time.Millisecond,
		RefreshDeadline: func() bool { return true },
		OnContention:    func(error) { contentionCount.Add(1) },
		OnWriteFailed:   func(error) { writeFailedCount.Add(1) },
	}

	if err := r.Handler()(""); err != nil {
		t.Fatalf("expected nil error on success, got: %v", err)
	}
	if got := setDeadlineCount.Load(); got != 1 {
		t.Errorf("expected SetReadDeadline to fire once on success, got %d", got)
	}
	if got := contentionCount.Load(); got != 0 {
		t.Errorf("expected OnContention not to fire on success, got %d", got)
	}
	if got := writeFailedCount.Load(); got != 0 {
		t.Errorf("expected OnWriteFailed not to fire on success, got %d", got)
	}
}
