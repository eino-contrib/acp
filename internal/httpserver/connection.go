package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Connection represents a single active ACP connection identified by
// Acp-Connection-Id. It tracks sessions and provides message routing for
// outgoing agent messages.
type Connection struct {
	ConnectionID string
	CreatedAt    time.Time
	// lastActivityElapsed holds the activity-since-CreatedAt duration, in
	// nanoseconds, from the most recent POST/GET/DELETE touch.
	// Anchoring on CreatedAt (which carries Go's monotonic clock reading)
	// keeps IsIdle correct across NTP adjustments and VM suspend/resume
	// events without allocating a *time.Time on every Touch().
	lastActivityElapsed atomic.Int64

	// activeHandlers tracks the number of in-flight POST handlers (both
	// requests and notifications). The idle reaper skips connections with
	// active handlers to avoid evicting connections that are busy
	// processing long-running work.
	activeHandlers atomic.Int64
	handlersMu     sync.Mutex
	handlersDone   chan struct{}

	// protocolVersion is the negotiated ACP protocol version extracted from
	// the initialize response. It is set in Acp-Protocol-Version headers on
	// subsequent HTTP responses.
	protocolVersion atomic.Value // string

	// PendingQueueSize overrides the default pending message buffer size for
	// new sessions. Zero means DefaultPendingQueueSize.
	PendingQueueSize int

	// sessionsMu protects sessions.
	sessionsMu sync.RWMutex
	sessions   map[string]*Session
	// writers tracks every session writer ever created for this connection,
	// including generations belonging to a session removed from sessions after
	// pending overflow. Connection shutdown waits on this owner-level registry.
	writersMu     sync.Mutex
	writers       sync.WaitGroup
	writersClosed bool

	closeOnce sync.Once
	closed    atomic.Bool
	closeDone chan struct{}
}

func (c *Connection) setProtocolVersion(version string) {
	if version == "" {
		return
	}
	c.protocolVersion.Store(version)
}

func (c *Connection) getProtocolVersion() string {
	if c == nil {
		return ""
	}
	if v := c.protocolVersion.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// DefaultPendingQueueSize is the maximum number of messages buffered when no
// GET SSE stream is bound to a session. This covers the window between
// session creation and the client establishing its GET SSE listener.
const DefaultPendingQueueSize = 1024

// defaultOutboxSize is the buffered channel size for the per-session outbox
// that decouples Send() callers from the SSE network write. Aligned with
// transport.DefaultOutboxSize used by the WebSocket server transport.
const defaultOutboxSize = 1024

// defaultOutboxSendTimeout caps the time Send() will wait for the outbox to
// accept a message. Without this cap a slow peer would back-pressure into
// handler goroutines indefinitely.
const defaultOutboxSendTimeout = 10 * time.Second

// defaultWriterStopTimeout caps how long awaitWriterStop waits for the SSE
// writer goroutine to exit. SSE writes (WriteSSEEvent + Flush) are not
// interruptible from Go, so if the underlying network write is stuck on a
// dead/slow peer the writer goroutine may remain blocked inside writeFn. A
// listener handoff stops waiting after this deadline rather than pinning
// BindStream / UnbindStream / CloseSession; connection shutdown separately
// tracks every writer and waits for actual termination.
const defaultWriterStopTimeout = 5 * time.Second

// Session tracks a single session within a connection.
// Each session has at most one active GET SSE stream, bound via BindStream.
//
// When no GET SSE stream is bound, outgoing messages are queued in a pending
// buffer (up to pendingQueueSize). The queue is flushed when a new
// stream is bound via BindStream.
//
// When a stream IS bound, Send() writes to a buffered outbox channel and
// a dedicated writer goroutine drains the outbox into the SSE writeFn.
// This isolates business goroutines from slow-client network writes,
// mirroring the outbox model used by the WebSocket server transport.
type Session struct {
	SessionID string
	// writerStopTimeout overrides the bounded listener-handoff wait in tests.
	// Zero uses defaultWriterStopTimeout. Connection shutdown separately waits
	// for actual writer termination and does not use this bound.
	writerStopTimeout time.Duration

	// bindMu serializes the complete listener handoff across BindStream and
	// UnbindStream, including intervals where either operation drops mu while
	// waiting for the previous writer/senders to exit. Without this guard a new
	// Bind can publish its writer before an old Unbind restores accepted outbox
	// messages to pending. CloseSession does not need this mutex: streamEvict is
	// cleared under mu before a handoff starts waiting.
	bindMu      sync.Mutex
	mu          sync.Mutex
	writeFn     func(json.RawMessage) error // GET SSE write function; nil = no active stream
	streamGen   uint64                      // incremented on each BindStream; used by UnbindStream
	streamEvict chan struct{}               // closed when the current stream is evicted by a new BindStream
	// activeWriterGen is the streamGen of the writer goroutine currently
	// authorised to call writeFn, or 0 when no writer is attached. It is
	// published under s.mu in startWriterLocked and cleared in
	// detachWriterLocked. The writer reads it without holding s.mu before each
	// network write and exits early when it no longer matches its own
	// generation — this fences any messages still buffered in an orphaned
	// outbox against leaking onto a writeFn that has already been evicted by
	// a newer BindStream. Using atomic (rather than the existing writerStop
	// close signal) eliminates the select-race where a ready writerStop and a
	// ready outbox can cause Go's runtime to pick the outbox case and emit
	// one more message on the old stream before observing the stop.
	activeWriterGen atomic.Uint64
	// outbox is the buffered channel consumed by the writer goroutine. It is
	// deliberately NEVER closed: concurrent Send callers may have observed
	// the pointer under s.mu and released the lock before their channel
	// send, and sending on a closed channel panics. Writer shutdown is
	// signalled via writerStop. Listener replacement drains messages already
	// accepted into the detached outbox back into pending. Terminal close uses
	// an in-band drain marker so messages already accepted by Send are written
	// before the listener is evicted; overflow may still discard them because
	// the session is already irrecoverably over capacity.
	outbox           chan sessionOutboxEntry
	outboxSenders    *sync.WaitGroup   // sends that captured this outbox generation
	writerStop       chan struct{}     // closed to signal writer goroutine exit
	writerDone       chan struct{}     // closed when writer goroutine exits
	writers          sync.WaitGroup    // every writer, including timed-out detached generations
	connection       *Connection       // owner-level tracker survives removal from Connection.sessions
	pending          []json.RawMessage // messages queued while no stream is bound
	pendingQueueSize int               // max buffered messages; 0 means DefaultPendingQueueSize
	done             chan struct{}
	doneOnce         sync.Once

	// closed flips to true once the session is permanently unusable (client
	// closed the connection, overflow tripped, etc.). Send/BindStream reject
	// with ErrSessionClosed after the flag is set.
	closed atomic.Bool
	// remove detaches this session from its parent Connection.sessions map.
	// Set by EnsureSession; nil-guarded in case a Session is constructed
	// directly in tests.
	remove func()
}

// sessionOutboxEntry is either one SSE payload or an in-band terminal drain
// marker. The marker shares the writer's FIFO, so observing its acknowledgement
// proves every message accepted before it has completed writeFn.
type sessionOutboxEntry struct {
	message json.RawMessage
	drained chan struct{}
}

// ErrSessionClosed is returned by Send and BindStream after the session has
// been closed (explicitly via CloseSession or implicitly via pending-queue
// overflow). The error is sentinel so callers can distinguish a permanent
// failure from a transient write error.
var ErrSessionClosed = errors.New("session closed")

// ErrSessionSendTimeout is returned by Send when the outbox is full and the
// write times out. Unlike ErrSessionClosed this is a transient backpressure
// signal — the session is still usable and subsequent sends may succeed.
var ErrSessionSendTimeout = errors.New("session send timeout")

// ErrSessionNoActiveStream is returned by SendLive when the session has no
// bound GET SSE stream. Reverse-request senders use this path so they fail
// fast instead of waiting on a response that can only arrive over a stream
// that does not exist.
var ErrSessionNoActiveStream = errors.New("session has no active stream")

// Done returns a channel that is closed when the session is closed.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// BindStream binds a GET SSE write function to this session and flushes any
// messages that were queued while no stream was bound.
//
// Only one GET stream is active per session: any stream previously bound is
// evicted. The caller receives a generation token (passed to UnbindStream)
// and an evict channel that is closed when a subsequent BindStream replaces
// this stream; the GET handler selects on it to exit promptly instead of
// continuing to send keepalives on a stream that no longer owns writeFn.
//
// A dedicated writer goroutine is spawned to drain the session outbox into
// writeFn. This isolates Send() callers from slow network writes. The writer
// exits when writerStop is signalled (UnbindStream/eviction) or the session
// is closed.
//
// FIFO is preserved without holding s.mu during network writes: each drain
// pass takes a snapshot of s.pending, clears it, and invokes writeFn outside
// the lock. While the write is in progress writeFn stays unbound, so any
// concurrent Send() observes nil and appends to s.pending — which the next
// pass will pick up. The loop exits once a pass finds s.pending empty and we
// can publish writeFn atomically under the lock.
func (s *Session) BindStream(writeFn func(json.RawMessage) error) (uint64, <-chan struct{}) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()

	if s.closed.Load() {
		evict := make(chan struct{})
		close(evict)
		return 0, evict
	}
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		evict := make(chan struct{})
		close(evict)
		return 0, evict
	}
	// Evict previous stream: close evict channel and shut down previous outbox.
	if s.streamEvict != nil {
		// Clear the shared pointer before dropping s.mu to wait for the old
		// writer. CloseSession may run during that wait; leaving the already
		// closed channel published lets it close the same channel again and
		// panic the process. The old GET handler already owns its returned copy.
		oldEvict := s.streamEvict
		s.streamEvict = nil
		close(oldEvict)
	}
	oldOutbox, writerDone, oldSenders := s.detachWriterLocked()
	if writerDone != nil || oldSenders != nil {
		s.mu.Unlock()
		s.awaitWriterStop(writerDone)
		if oldSenders != nil {
			oldSenders.Wait()
		}
		s.mu.Lock()
		if s.closed.Load() {
			s.mu.Unlock()
			evict := make(chan struct{})
			close(evict)
			return 0, evict
		}
		// Send/SendLive transfers ownership when it successfully enqueues a
		// message. Preserve anything the old writer had not consumed by moving
		// the detached outbox in front of messages queued while we waited.
		s.prependDetachedOutboxLocked(oldOutbox)
	}

	s.streamEvict = make(chan struct{})
	myEvict := s.streamEvict
	s.streamGen++
	// Generation zero is reserved as the inactive activeWriterGen sentinel.
	// Skip it if the counter ever wraps so a live writer is never mistaken
	// for an inactive one.
	if s.streamGen == 0 {
		s.streamGen++
	}
	gen := s.streamGen

	// Drain pending into writeFn synchronously (same as before) to preserve
	// FIFO ordering for messages queued before the stream was established.
	for {
		batch := s.pending
		s.pending = nil
		if len(batch) == 0 {
			// Pending drained. Start the replacement writer.
			s.writeFn = writeFn
			s.startWriterLocked(writeFn)
			s.mu.Unlock()
			return gen, myEvict
		}
		s.mu.Unlock()

		var writeErr error
		sent := 0
		for i, msg := range batch {
			if err := writeFn(msg); err != nil {
				writeErr = err
				sent = i
				break
			}
			sent = i + 1
		}

		s.mu.Lock()
		if s.closed.Load() {
			s.mu.Unlock()
			return gen, myEvict
		}
		if writeErr != nil {
			// Re-queue un-sent tail ahead of anything that arrived during
			// the flush so FIFO is preserved for the next BindStream, and
			// do NOT bind writeFn — the caller treats the stream as
			// not-yet-attached.
			remaining := batch[sent:]
			if len(s.pending) > 0 {
				merged := make([]json.RawMessage, 0, len(remaining)+len(s.pending))
				merged = append(merged, remaining...)
				merged = append(merged, s.pending...)
				s.pending = merged
			} else {
				s.pending = append([]json.RawMessage(nil), remaining...)
			}
			// The listener never became active. Retire this generation and
			// wake its GET handler; a later BindStream will allocate a fresh
			// generation and retry the pending tail. CloseSession may have
			// already cleared the channel while writeFn was running, so only
			// close the channel when it is still owned by this bind.
			s.writeFn = nil
			if s.streamGen == gen && s.streamEvict == myEvict {
				close(myEvict)
				s.streamEvict = nil
			}
			s.mu.Unlock()
			return gen, myEvict
		}
		// Loop: drain any messages enqueued during the just-finished flush.
	}
}

// startWriterLocked spawns the writer goroutine that drains outbox → writeFn.
// Must be called with s.mu held. The writer exits on writerStop (set by
// detachWriterLocked), s.done (CloseSession), or a writeFn error. outbox is
// NOT closed by the shutdown path — see the field doc on s.outbox.
func (s *Session) startWriterLocked(writeFn func(json.RawMessage) error) {
	outbox := make(chan sessionOutboxEntry, defaultOutboxSize)
	writerStop := make(chan struct{})
	writerDone := make(chan struct{})
	s.outbox = outbox
	s.outboxSenders = &sync.WaitGroup{}
	s.writerStop = writerStop
	s.writerDone = writerDone

	myGen := s.streamGen
	myEvict := s.streamEvict
	s.activeWriterGen.Store(myGen)

	done := s.done
	s.writers.Add(1)
	if s.connection != nil && !s.connection.beginWriter() {
		// Connection shutdown fenced new writers before this generation could be
		// published. Undo the session-local count and leave no live state behind.
		s.writers.Done()
		s.activeWriterGen.Store(0)
		s.writeFn = nil
		s.outbox = nil
		s.outboxSenders = nil
		s.writerStop = nil
		s.writerDone = nil
		if s.streamEvict == myEvict && myEvict != nil {
			close(myEvict)
			s.streamEvict = nil
		}
		close(writerDone)
		return
	}
	safe.Go(func() {
		if s.connection != nil {
			defer s.connection.endWriter()
		}
		defer s.writers.Done()
		defer close(writerDone)
		for {
			// Give detachment priority before waiting for another message. This
			// keeps an old writer from consuming the replacement's backlog after
			// it finishes an in-flight network write.
			select {
			case <-writerStop:
				return
			default:
			}
			select {
			case <-writerStop:
				return
			case <-done:
				return
			case entry := <-outbox:
				if entry.drained != nil {
					close(entry.drained)
					return
				}
				msg := entry.message
				// Double-check the stop signal before invoking writeFn so a
				// writerStop that became ready concurrently with this outbox
				// receive does not cause one last message to be emitted on an
				// evicted stream (Go's select picks pseudo-randomly among
				// ready cases).
				select {
				case <-writerStop:
					// The receive raced with detachment. Route the accepted
					// message through current session state: pending while the
					// replacement binds, or its new outbox after a slow detach.
					_ = s.send(msg, false)
					return
				default:
				}
				// Fence against the residual race where writerStop closes
				// between the double-check above and writeFn below: every
				// attached writer holds a unique generation token, and
				// detachWriterLocked clears activeWriterGen atomically under
				// s.mu. Bailing here guarantees an evicted writer never
				// emits on writeFn. Route the accepted message through the
				// current session state for the replacement stream.
				if s.activeWriterGen.Load() != myGen {
					_ = s.send(msg, false)
					return
				}
				if err := writeFn(msg); err != nil {
					acplog.Debug("sse outbox write: %v", err)
					// Retire the failed generation asynchronously. A concurrent
					// BindStream may already hold bindMu while waiting for this
					// writer's writerDone; doing the handoff inline would deadlock
					// that replacement. The identity checks below make cleanup a
					// no-op when replacement or close won ownership first.
					safe.Go(func() {
						s.deactivateFailedWriter(myGen, outbox, writerStop, writerDone, myEvict)
					})
					return
				}
			}
		}
	})
}

// deactivateFailedWriter atomically retires a writer whose writeFn failed.
// The identity checks are essential because BindStream may detach this writer,
// time out waiting for its blocked writeFn, and install a newer generation
// before the old writeFn eventually returns. In that case the old writer must
// not clear the replacement's state. CloseSession follows the same detach
// path, so a writer that loses ownership simply has nothing left to clean up.
//
// The failed writer schedules this handoff asynchronously immediately before it
// returns. bindMu serializes the outbox recovery with replacement and Unbind;
// if either already detached the generation, the identity checks make this a
// no-op because that path owns the same recovery.
func (s *Session) deactivateFailedWriter(
	gen uint64,
	outbox chan sessionOutboxEntry,
	writerStop chan struct{},
	writerDone chan struct{},
	evict chan struct{},
) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()

	s.mu.Lock()
	if s.streamGen != gen ||
		s.outbox != outbox ||
		s.writerStop != writerStop ||
		s.writerDone != writerDone {
		s.mu.Unlock()
		return
	}

	// Clear the generation fence before unpublishing the outbox, mirroring
	// detachWriterLocked. Send and SendLive inspect the outbox under s.mu,
	// so calls beginning after this critical section observe one coherent
	// inactive state.
	s.activeWriterGen.Store(0)
	s.writeFn = nil
	s.outbox = nil
	senders := s.outboxSenders
	s.outboxSenders = nil
	s.writerStop = nil
	s.writerDone = nil
	if s.streamEvict == evict && evict != nil {
		close(evict)
		s.streamEvict = nil
	}
	s.mu.Unlock()

	// Send transfers ownership only after enqueueing successfully. Wait for
	// every sender that captured this generation before draining its outbox, so
	// no accepted tail message can arrive after the handoff snapshot.
	if senders != nil {
		senders.Wait()
	}
	s.mu.Lock()
	if !s.closed.Load() {
		s.prependDetachedOutboxLocked(outbox)
	}
	s.mu.Unlock()
}

// detachWriterLocked signals the writer goroutine to exit and clears the
// writer-related fields (outbox/writerStop/writerDone). Must be called with
// s.mu held. It returns the detached outbox and writerDone channel so a
// listener replacement can wait for the writer, then preserve any messages
// that were accepted but not consumed.
//
// outbox is deliberately NOT closed. Concurrent Send callers read the outbox
// pointer under s.mu and release the lock before their channel send;
// closing the channel here would turn a benign race into a runtime panic
// (send on closed channel). Writer shutdown is signalled exclusively via
// writerStop plus activeWriterGen, which the writer checks before each
// network write.
//
// Callers MUST release s.mu before calling awaitWriterStop(writerDone),
// otherwise a writer blocked inside writeFn on a wedged peer would deadlock
// any Send / BindStream caller contending for s.mu.
func (s *Session) detachWriterLocked() (chan sessionOutboxEntry, <-chan struct{}, *sync.WaitGroup) {
	if s.writerStop == nil {
		return nil, nil, nil
	}
	// Clear the gen fence first: any writer that observes a post-detach
	// store will bail before touching writeFn, even if its select loop has
	// already pulled a message out of the now-orphaned outbox.
	s.activeWriterGen.Store(0)
	close(s.writerStop)
	outbox := s.outbox
	writerDone := s.writerDone
	senders := s.outboxSenders
	s.outbox = nil
	s.outboxSenders = nil
	s.writerStop = nil
	s.writerDone = nil
	return outbox, writerDone, senders
}

// prependDetachedOutboxLocked drains the buffered tail of a stopped writer and
// places it before messages queued after detachment. The writer must have
// exited before this runs, so it cannot consume concurrently.
func (s *Session) prependDetachedOutboxLocked(outbox <-chan sessionOutboxEntry) {
	if outbox == nil {
		return
	}
	remaining := make([]json.RawMessage, 0, len(outbox))
	for {
		select {
		case entry := <-outbox:
			if entry.drained != nil {
				close(entry.drained)
				continue
			}
			remaining = append(remaining, entry.message)
		default:
			if len(remaining) == 0 {
				return
			}
			merged := make([]json.RawMessage, 0, len(remaining)+len(s.pending))
			merged = append(merged, remaining...)
			merged = append(merged, s.pending...)
			s.pending = merged
			return
		}
	}
}

// awaitWriterStop blocks (without holding s.mu) until the writer goroutine
// exits or defaultWriterStopTimeout elapses. Safe to call with a nil
// writerDone — returns immediately in that case. On timeout the writer
// goroutine is abandoned; the connection-level close path (which tears down
// the HTTP response) will eventually unblock writeFn.
func (s *Session) awaitWriterStop(writerDone <-chan struct{}) {
	if writerDone == nil {
		return
	}
	timeout := s.writerStopTimeout
	if timeout <= 0 {
		timeout = defaultWriterStopTimeout
	}
	timer := time.NewTimer(timeout)
	select {
	case <-writerDone:
		timer.Stop()
	case <-timer.C:
		acplog.Debug("sse stop writer timed out after %v for session %s; writer goroutine remains tracked for shutdown", timeout, s.SessionID)
	}
}

// UnbindStream removes the GET SSE write function only if the current stream
// generation matches gen. This prevents a finishing GET request from clearing a
// writeFn that was already replaced by a newer GET request.
func (s *Session) UnbindStream(gen uint64) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()

	s.mu.Lock()
	var (
		outbox     chan sessionOutboxEntry
		writerDone <-chan struct{}
		senders    *sync.WaitGroup
	)
	if s.streamGen == gen {
		s.writeFn = nil
		outbox, writerDone, senders = s.detachWriterLocked()
	}
	s.mu.Unlock()
	s.awaitWriterStop(writerDone)
	if senders != nil {
		senders.Wait()
	}
	if outbox != nil {
		s.mu.Lock()
		if !s.closed.Load() {
			s.prependDetachedOutboxLocked(outbox)
		}
		s.mu.Unlock()
	}
}

// Send sends a message to the client via the bound GET SSE stream's outbox.
// If no stream is currently bound, the message is queued in a pending buffer
// and will be flushed when a stream is bound via BindStream. When the pending
// queue overflows, the session is closed — overflow means the client is no
// longer consuming messages, so keeping the session alive would silently drop
// messages and leave callers blocked on responses that will never arrive.
//
// When a stream is bound, the message is written to a buffered outbox channel
// and a dedicated writer goroutine performs the actual network write. This
// decouples Send() callers from slow-client SSE writes, so handler goroutines
// are not blocked by network I/O.
func (s *Session) Send(msg json.RawMessage) error {
	return s.send(msg, false)
}

// SendLive sends a message to the client but requires a GET SSE stream to be
// bound. If no stream is bound, it returns ErrSessionNoActiveStream instead of
// queuing the message. This is the correct path for reverse JSON-RPC requests
// because those callers wait for a response that can only arrive through an
// active stream — queuing silently would leave them blocked until the caller's
// context deadline expires.
func (s *Session) SendLive(msg json.RawMessage) error {
	return s.send(msg, true)
}

func (s *Session) send(msg json.RawMessage, requireStream bool) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	cloned := acptransport.CloneMessage(msg)
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	outbox := s.outbox
	senders := s.outboxSenders
	if outbox == nil {
		if requireStream {
			s.mu.Unlock()
			return fmt.Errorf("session %s: %w", s.SessionID, ErrSessionNoActiveStream)
		}
		// No stream bound — queue to pending.
		limit := s.pendingQueueSize
		if limit <= 0 {
			limit = DefaultPendingQueueSize
		}
		if len(s.pending) >= limit {
			s.closed.Store(true)
			s.pending = nil
			s.writeFn = nil
			_, writerDone, _ := s.detachWriterLocked() // nil when no writer is running
			if s.streamEvict != nil {
				close(s.streamEvict)
				s.streamEvict = nil
			}
			s.mu.Unlock()
			s.awaitWriterStop(writerDone)
			s.doneOnce.Do(func() { close(s.done) })
			if s.remove != nil {
				s.remove()
			}
			return fmt.Errorf("pending message queue full for session %s (limit %d); %w", s.SessionID, limit, ErrSessionClosed)
		}
		s.pending = append(s.pending, cloned)
		s.mu.Unlock()
		return nil
	}
	senders.Add(1)
	s.mu.Unlock()
	defer senders.Done()
	// Stream bound — send to outbox with a timeout so a backed-up outbox
	// (slow client) does not block the caller indefinitely.
	timer := time.NewTimer(defaultOutboxSendTimeout)
	defer timer.Stop()
	select {
	case outbox <- sessionOutboxEntry{message: cloned}:
		// Re-check s.done to close the ambiguity window: Go's select picks
		// at random when multiple cases are ready, so a concurrent
		// CloseSession that closed s.done could race with the send case.
		// Without this verification a message landing in an orphaned outbox
		// (writer goroutine already exited) would be silently dropped while
		// the caller sees nil.
		select {
		case <-s.done:
			return ErrSessionClosed
		default:
			return nil
		}
	case <-timer.C:
		return fmt.Errorf("sse outbox send timeout for session %s: %w", s.SessionID, ErrSessionSendTimeout)
	case <-s.done:
		return ErrSessionClosed
	}
}

// CloseSession closes the session, draining messages already accepted by the
// active writer before evicting its stream. New Send/Bind calls are rejected as
// soon as close begins. The drain remains bounded by writerStopTimeout so a
// wedged network writer cannot make terminal cleanup block forever.
func (s *Session) CloseSession() {
	s.closed.Store(true)
	s.mu.Lock()
	outbox := s.outbox
	var writerDone <-chan struct{} = s.writerDone
	senders := s.outboxSenders
	s.mu.Unlock()

	// A sender increments this generation's WaitGroup while holding s.mu,
	// before close can publish the closed flag and snapshot the generation.
	// Waiting here guarantees the terminal marker is ordered after every Send
	// that can still return success.
	if senders != nil {
		senders.Wait()
	}
	s.drainWriter(outbox, writerDone)

	s.mu.Lock()
	s.writeFn = nil
	_, writerDone, _ = s.detachWriterLocked()
	if s.streamEvict != nil {
		close(s.streamEvict)
		s.streamEvict = nil
	}
	s.pending = nil
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.done) })
	s.awaitWriterStop(writerDone)
}

// drainWriter places a terminal marker behind every accepted outbox payload
// and waits for the writer to acknowledge it. It returns early if the writer
// has already failed and uses the same bounded wait as listener handoff.
func (s *Session) drainWriter(outbox chan sessionOutboxEntry, writerDone <-chan struct{}) {
	if outbox == nil || writerDone == nil {
		return
	}
	timeout := s.writerStopTimeout
	if timeout <= 0 {
		timeout = defaultWriterStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	drained := make(chan struct{})
	select {
	case outbox <- sessionOutboxEntry{drained: drained}:
	case <-writerDone:
		return
	case <-timer.C:
		acplog.Debug("sse terminal drain enqueue timed out after %v for session %s", timeout, s.SessionID)
		return
	}

	select {
	case <-drained:
	case <-writerDone:
	case <-timer.C:
		acplog.Debug("sse terminal drain timed out after %v for session %s", timeout, s.SessionID)
	}
}

// WaitWriters waits for every writer generation to actually exit, including
// a detached writer that exceeded the bounded listener-handoff wait. It is
// used only by connection shutdown so Close remains asynchronous while
// Shutdown can truthfully report whether all SDK-owned work has converged.
func (s *Session) WaitWriters() {
	if s != nil {
		s.writers.Wait()
	}
}

// IsClosed reports whether the connection has been closed.
func (c *Connection) IsClosed() bool {
	if c == nil {
		return true
	}
	return c.closed.Load()
}

// Touch updates the last activity timestamp. The stored value is the
// nanoseconds elapsed between CreatedAt and now, using Go's monotonic clock
// (time.Since uses the monotonic reading when both operands carry one), which
// avoids the heap allocation a *time.Time atomic.Pointer would incur.
func (c *Connection) Touch() {
	c.lastActivityElapsed.Store(int64(time.Since(c.CreatedAt)))
}

// BeginRequest increments the active handler counter and touches the
// connection. Call EndRequest when the handler finishes. Notifications use
// the same pair so the idle reaper does not evict a connection whose only
// activity is a long-running notification handler.
func (c *Connection) BeginRequest() bool {
	c.handlersMu.Lock()
	if c.closed.Load() {
		c.handlersMu.Unlock()
		return false
	}
	if c.activeHandlers.Add(1) == 1 {
		c.handlersDone = make(chan struct{})
	}
	c.handlersMu.Unlock()
	c.Touch()
	return true
}

// EndRequest decrements the active handler counter and touches the
// connection.
func (c *Connection) EndRequest() {
	c.handlersMu.Lock()
	if c.activeHandlers.Add(-1) == 0 && c.handlersDone != nil {
		close(c.handlersDone)
	}
	c.handlersMu.Unlock()
	c.Touch()
}

func (c *Connection) WaitHandlers() {
	if c == nil {
		return
	}
	c.handlersMu.Lock()
	done := c.handlersDone
	active := c.activeHandlers.Load()
	c.handlersMu.Unlock()
	if active > 0 && done != nil {
		<-done
	}
}

// IsIdle reports whether the connection has been idle for at least the given
// timeout duration. A connection with active handlers is never considered idle.
// A zero or negative timeout always returns false.
func (c *Connection) IsIdle(timeout time.Duration) bool {
	if c == nil || timeout <= 0 {
		return false
	}
	if c.activeHandlers.Load() > 0 {
		return false
	}
	// idleFor = time.Since(CreatedAt) - lastActivityElapsed
	// (time.Since uses monotonic diff when CreatedAt carries monotonic).
	idleFor := time.Since(c.CreatedAt) - time.Duration(c.lastActivityElapsed.Load())
	return idleFor >= timeout
}

// NewConnection creates a new Connection. The caller should set ConnectionID
// to a unique identifier (typically the same ID used by the server-level
// connection object).
func NewConnection() *Connection {
	return &Connection{
		CreatedAt: time.Now(),
		sessions:  make(map[string]*Session),
		closeDone: make(chan struct{}),
	}
}

// EnsureSession ensures a session exists for the given session ID, creating one
// if needed. Returns nil if the connection has already been closed, so that
// late-arriving requests cannot resurrect sessions in a closed connection.
func (c *Connection) EnsureSession(sessionID string) *Session {
	sess, _ := c.ensureSession(sessionID)
	return sess
}

func (c *Connection) ensureSession(sessionID string) (*Session, bool) {
	if sessionID == "" {
		return nil, false
	}
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	if c.closed.Load() {
		return nil, false
	}
	sess, ok := c.sessions[sessionID]
	if ok {
		return sess, false
	}
	sess = &Session{
		SessionID:        sessionID,
		pendingQueueSize: c.PendingQueueSize,
		done:             make(chan struct{}),
		connection:       c,
	}
	sess.remove = func() { c.detachSession(sessionID, sess) }
	c.sessions[sessionID] = sess
	return sess, true
}

// detachSession removes a session from the connection map if the stored
// session pointer still matches the one that requested detachment. Used by
// Session.Send on overflow so the overflowed object cannot be looked up and
// re-accumulate pending messages. The caller must already have invoked
// CloseSession-equivalent cleanup on the session itself.
func (c *Connection) detachSession(sessionID string, sess *Session) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	if existing, ok := c.sessions[sessionID]; ok && existing == sess {
		delete(c.sessions, sessionID)
	}
}

// LookupSession looks up a session by ID.
func (c *Connection) LookupSession(sessionID string) (*Session, bool) {
	c.sessionsMu.RLock()
	defer c.sessionsMu.RUnlock()
	sess, ok := c.sessions[sessionID]
	return sess, ok
}

// RemoveSession removes and closes a single session by ID.
func (c *Connection) RemoveSession(sessionID string) bool {
	c.sessionsMu.Lock()
	sess, ok := c.sessions[sessionID]
	if ok {
		delete(c.sessions, sessionID)
	}
	c.sessionsMu.Unlock()
	if ok {
		sess.CloseSession()
	}
	return ok
}

// SessionCount returns the number of active sessions.
func (c *Connection) SessionCount() int {
	c.sessionsMu.RLock()
	defer c.sessionsMu.RUnlock()
	return len(c.sessions)
}

// CloseConnection closes all listeners for a connection and removes all
// session references so pending message buffers can be garbage-collected.
//
// The sessions map is snapshotted and cleared under sessionsMu, then each
// Session is closed after the lock has been released. CloseSession performs a
// bounded handoff wait, then CloseConnection waits for all writer generations
// to actually exit. Holding sessionsMu across either wait would block concurrent
// LookupSession/EnsureSession/RemoveSession/detachSession callers.
func CloseConnection(conn *Connection) {
	conn.closeOnce.Do(func() {
		defer close(conn.closeDone)
		conn.handlersMu.Lock()
		conn.closed.Store(true)
		conn.handlersMu.Unlock()

		conn.sessionsMu.Lock()
		sessions := conn.sessions
		conn.sessions = make(map[string]*Session)
		conn.sessionsMu.Unlock()
		conn.writersMu.Lock()
		conn.writersClosed = true
		conn.writersMu.Unlock()

		for _, sess := range sessions {
			sess.CloseSession()
		}
		conn.writers.Wait()
	})
}

func (c *Connection) beginWriter() bool {
	c.writersMu.Lock()
	defer c.writersMu.Unlock()
	if c.writersClosed {
		return false
	}
	c.writers.Add(1)
	return true
}

func (c *Connection) endWriter() {
	c.writers.Done()
}

// WaitClosed blocks until CloseConnection has finished closing every session
// and its SSE writer. A nil channel is tolerated for directly constructed test
// connections that predate NewConnection.
func (c *Connection) WaitClosed() {
	if c != nil && c.closeDone != nil {
		<-c.closeDone
	}
}
