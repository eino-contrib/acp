package httpserver

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acplog "github.com/eino-contrib/acp/internal/log"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Connection represents a single active ACP connection identified by
// Acp-Connection-Id. It tracks sessions and provides message routing for
// outgoing agent messages.
type Connection struct {
	ConnectionID string
	CreatedAt    time.Time
	// lastActivity stores the time of the most recent POST/GET/DELETE activity.
	// A *time.Time is used (rather than Unix nanos) so the monotonic clock
	// component is preserved; this keeps IsIdle correct across NTP adjustments
	// and VM suspend/resume events.
	lastActivity atomic.Pointer[time.Time]

	// activeRequests tracks the number of in-flight POST request handlers.
	// The idle reaper skips connections with active requests to avoid
	// evicting connections that are busy processing long-running handlers.
	activeRequests atomic.Int64

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

	closeOnce sync.Once
	closed    atomic.Bool

	Logger acplog.Logger
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
const DefaultPendingQueueSize = 256

// Session tracks a single session within a connection.
// Each session has at most one active GET SSE stream, bound via BindStream.
//
// When no GET SSE stream is bound, outgoing messages are queued in a pending
// buffer (up to pendingQueueSize). The queue is flushed when a new
// stream is bound via BindStream.
type Session struct {
	SessionID string

	mu               sync.Mutex
	writeFn          func(json.RawMessage) error // GET SSE write function; nil = no active stream
	streamGen        uint64                      // incremented on each BindStream; used by UnbindStream
	streamEvict      chan struct{}               // closed when the current stream is evicted by a new BindStream
	pending          []json.RawMessage           // messages queued while no stream is bound
	pendingQueueSize int                         // max buffered messages; 0 means DefaultPendingQueueSize
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

// ErrSessionClosed is returned by Send and BindStream after the session has
// been closed (explicitly via CloseSession or implicitly via pending-queue
// overflow). The error is sentinel so callers can distinguish a permanent
// failure from a transient write error.
var ErrSessionClosed = fmt.Errorf("session closed")

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
// The flush happens while holding s.mu so that concurrent Send() calls queue
// to pending until the backlog is drained, preserving FIFO order between the
// pre-attached backlog and new sends that arrive during bind. Send() itself
// only holds s.mu to read writeFn; the actual network write happens outside
// the lock, so a slow flush here does not block other sessions and a slow
// Send elsewhere does not block bind.
func (s *Session) BindStream(writeFn func(json.RawMessage) error) (uint64, <-chan struct{}) {
	if s.closed.Load() {
		evict := make(chan struct{})
		close(evict)
		return 0, evict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		evict := make(chan struct{})
		close(evict)
		return 0, evict
	}
	if s.streamEvict != nil {
		close(s.streamEvict)
	}
	s.streamEvict = make(chan struct{})
	myEvict := s.streamEvict
	s.streamGen++
	gen := s.streamGen

	for i, msg := range s.pending {
		if err := writeFn(msg); err != nil {
			// Keep the unsent tail queued and do NOT bind writeFn, so the
			// caller treats the stream as not-yet-attached and a later
			// BindStream can retry.
			s.pending = append([]json.RawMessage(nil), s.pending[i:]...)
			return gen, myEvict
		}
	}

	s.pending = nil
	s.writeFn = writeFn
	return gen, myEvict
}

// UnbindStream removes the GET SSE write function only if the current stream
// generation matches gen. This prevents a finishing GET request from clearing a
// writeFn that was already replaced by a newer GET request.
func (s *Session) UnbindStream(gen uint64) {
	s.mu.Lock()
	if s.streamGen == gen {
		s.writeFn = nil
	}
	s.mu.Unlock()
}

// Send sends a message to the client via the bound GET SSE stream.
// If no stream is currently bound, the message is queued in a pending buffer
// and will be flushed when a stream is bound via BindStream. When the pending
// queue overflows, the session is closed — overflow means the client is no
// longer consuming messages, so keeping the session alive would silently drop
// messages and leave callers blocked on responses that will never arrive.
//
// The network write runs outside s.mu so that a slow client cannot block
// other operations on the session (BindStream, UnbindStream, CloseSession,
// or concurrent Sends queueing to pending). Concurrent Sends to the same
// bound stream serialize on the writeFn's own internal mutex, so SSE frame
// integrity is preserved.
func (s *Session) Send(msg json.RawMessage) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	cloned := acptransport.CloneMessage(msg)
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	fn := s.writeFn
	if fn == nil {
		limit := s.pendingQueueSize
		if limit <= 0 {
			limit = DefaultPendingQueueSize
		}
		if len(s.pending) >= limit {
			s.closed.Store(true)
			s.pending = nil
			s.writeFn = nil
			if s.streamEvict != nil {
				close(s.streamEvict)
				s.streamEvict = nil
			}
			s.mu.Unlock()
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
	s.mu.Unlock()
	return fn(cloned)
}

// CloseSession closes the session, unbinding any active stream.
func (s *Session) CloseSession() {
	s.closed.Store(true)
	s.doneOnce.Do(func() { close(s.done) })
	s.mu.Lock()
	s.writeFn = nil
	if s.streamEvict != nil {
		close(s.streamEvict)
		s.streamEvict = nil
	}
	s.mu.Unlock()
}

// IsClosed reports whether the connection has been closed.
func (c *Connection) IsClosed() bool {
	if c == nil {
		return true
	}
	return c.closed.Load()
}

// Touch updates the last activity timestamp.
func (c *Connection) Touch() {
	now := time.Now()
	c.lastActivity.Store(&now)
}

// BeginRequest increments the active request counter and touches the
// connection. Call EndRequest when the request handler finishes.
func (c *Connection) BeginRequest() {
	c.activeRequests.Add(1)
	c.Touch()
}

// EndRequest decrements the active request counter and touches the
// connection.
func (c *Connection) EndRequest() {
	c.activeRequests.Add(-1)
	c.Touch()
}

// IsIdle reports whether the connection has been idle for at least the given
// timeout duration. A connection with active requests is never considered idle.
// A zero or negative timeout always returns false.
func (c *Connection) IsIdle(timeout time.Duration) bool {
	if c == nil || timeout <= 0 {
		return false
	}
	if c.activeRequests.Load() > 0 {
		return false
	}
	last := c.lastActivity.Load()
	if last == nil {
		return time.Since(c.CreatedAt) >= timeout
	}
	return time.Since(*last) >= timeout
}

// NewConnection creates a new Connection. The caller should set ConnectionID
// to a unique identifier (typically the same ID used by the server-level
// connection object).
func NewConnection() *Connection {
	now := time.Now()
	conn := &Connection{
		CreatedAt: now,
		sessions:  make(map[string]*Session),
	}
	conn.lastActivity.Store(&now)
	return conn
}

// EnsureSession ensures a session exists for the given session ID, creating one
// if needed. Returns nil if the connection has already been closed, so that
// late-arriving requests cannot resurrect sessions in a closed connection.
func (c *Connection) EnsureSession(sessionID string) *Session {
	if sessionID == "" {
		return nil
	}
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	if c.closed.Load() {
		return nil
	}
	sess, ok := c.sessions[sessionID]
	if ok {
		return sess
	}
	sess = &Session{SessionID: sessionID, pendingQueueSize: c.PendingQueueSize, done: make(chan struct{})}
	sess.remove = func() { c.detachSession(sessionID, sess) }
	c.sessions[sessionID] = sess
	return sess
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
func (c *Connection) RemoveSession(sessionID string) {
	c.sessionsMu.Lock()
	sess, ok := c.sessions[sessionID]
	if ok {
		delete(c.sessions, sessionID)
	}
	c.sessionsMu.Unlock()
	if ok {
		sess.CloseSession()
	}
}

// SessionCount returns the number of active sessions.
func (c *Connection) SessionCount() int {
	c.sessionsMu.RLock()
	defer c.sessionsMu.RUnlock()
	return len(c.sessions)
}

// CloseConnection closes all listeners for a connection and removes all
// session references so pending message buffers can be garbage-collected.
func CloseConnection(conn *Connection) {
	conn.closeOnce.Do(func() {
		conn.closed.Store(true)

		conn.sessionsMu.Lock()
		for id, sess := range conn.sessions {
			sess.CloseSession()
			delete(conn.sessions, id)
		}
		conn.sessionsMu.Unlock()
	})
}
