package jsonrpc

import (
	"container/list"
	"context"
	"sync"
)

// unboundedQueue is an unbounded FIFO of *Message entries.
//
// Push is non-blocking and is used from the read loop. The read loop MUST
// never block: blocking it starves Response dispatch and can deadlock
// handlers that are waiting on SendRequest responses on the same connection.
// An unbounded queue is the explicit trade-off for "never drop a message" —
// memory grows if consumers lag, but the protocol invariant is preserved.
//
// Pop blocks until an item is available, ctx is canceled, or the queue is
// closed. Consumers that want to drain remaining items during shutdown
// should pass context.Background() and rely on Close() to signal termination.
type unboundedQueue struct {
	mu       sync.Mutex
	notEmpty chan struct{} // closed to wake waiters, replaced with a fresh chan on each signal
	items    *list.List
	closed   bool
}

func newUnboundedQueue() *unboundedQueue {
	return &unboundedQueue{
		items:    list.New(),
		notEmpty: make(chan struct{}),
	}
}

// Push appends m to the queue. Never blocks. Returns false if the queue has
// already been closed (in which case m is dropped — this only happens after
// Close and is by design: shutdown is the only allowed dropping point).
//
// The wakeup channel is only rotated on the empty→non-empty boundary: once a
// waiter has been woken and the queue stays non-empty, subsequent Push calls
// skip the allocation+close. This keeps the hot path allocation-free under
// backpressure.
func (q *unboundedQueue) Push(m *Message) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	wasEmpty := q.items.Len() == 0
	q.items.PushBack(m)
	var signal chan struct{}
	if wasEmpty {
		signal = q.notEmpty
		q.notEmpty = make(chan struct{})
	}
	q.mu.Unlock()
	if signal != nil {
		close(signal)
	}
	return true
}

// Pop returns the oldest item. If the queue is empty, it blocks until an
// item becomes available, ctx is canceled, or the queue is closed and drained.
// Returns (nil, false) on ctx cancel or drained-and-closed.
func (q *unboundedQueue) Pop(ctx context.Context) (*Message, bool) {
	for {
		q.mu.Lock()
		if q.items.Len() > 0 {
			front := q.items.Front()
			q.items.Remove(front)
			q.mu.Unlock()
			return front.Value.(*Message), true
		}
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		wait := q.notEmpty
		q.mu.Unlock()

		select {
		case <-wait:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// Close marks the queue closed and wakes any blocked Pop callers. Subsequent
// Push calls become no-ops. Pending items are NOT discarded; Pop continues
// to return them in order until the queue is empty, then returns (nil, false).
func (q *unboundedQueue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	signal := q.notEmpty
	q.notEmpty = make(chan struct{})
	q.mu.Unlock()
	close(signal)
}

// Len returns the current queue depth. Intended for metrics / high-water alerts.
func (q *unboundedQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items.Len()
}
