package peerstate

import (
	"container/list"
	"sync"
	"time"
)

// TrackResponseMetadata updates peer state from an inbound JSON-RPC response.
// Both HTTP and WebSocket client transports call this when a response is
// received, keeping the shared logic in one place.
func (s *State) TrackResponseMetadata(protocolVersion, sessionID string) {
	if protocolVersion != "" {
		s.SetProtocolVersion(protocolVersion)
	}
	if sessionID != "" {
		s.SetSessionID(sessionID)
	}
}

// requestSessionEntry stores a session ID associated with a pending reverse-call
// request, along with the time it was stored so stale entries can be evicted.
type requestSessionEntry struct {
	sessionID string
	storedAt  time.Time
	key       string // map key, stored for O(1) removal from list
}

// requestSessionTTL is the default maximum time a request-session mapping is
// considered valid. After this duration the entry is silently dropped.
const defaultRequestSessionTTL = 5 * time.Minute

// defaultMaxRequestSessions is the default hard cap on stored request-session
// mappings. When the limit is reached, the oldest entry is evicted.
const defaultMaxRequestSessions = 10000

// State tracks peer metadata observed over ACP transports.
//
// It is used by multiple transport implementations so connection/session state
// evolves consistently across HTTP and WebSocket clients.
//
// All fields are protected by a single mu lock.
type State struct {
	mu              sync.RWMutex
	connectionID    string
	latestSessionID string
	protocolVersion string

	requestSessionTTL     time.Duration
	maxRequestSessions    int

	// requestSessions uses a map + doubly-linked list for O(1) insert, lookup,
	// and LRU eviction. The list is ordered oldest→newest (front = oldest).
	requestSessions    map[string]*list.Element
	requestSessionList *list.List
}

// Option configures a State.
type Option func(*State)

// WithRequestSessionTTL sets the maximum time a request-session mapping is
// considered valid. Zero uses the default (5 minutes).
func WithRequestSessionTTL(d time.Duration) Option {
	return func(s *State) {
		if d > 0 {
			s.requestSessionTTL = d
		}
	}
}

// WithMaxRequestSessions sets the hard cap on stored request-session mappings.
// Zero uses the default (10000).
func WithMaxRequestSessions(n int) Option {
	return func(s *State) {
		if n > 0 {
			s.maxRequestSessions = n
		}
	}
}

// New constructs a peer state tracker.
func New(opts ...Option) *State {
	s := &State{
		requestSessionTTL:  defaultRequestSessionTTL,
		maxRequestSessions: defaultMaxRequestSessions,
		requestSessions:    make(map[string]*list.Element),
		requestSessionList: list.New(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *State) ConnectionID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connectionID
}

func (s *State) SetConnectionID(connectionID string) {
	if s == nil || connectionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.connectionID != connectionID
	s.connectionID = connectionID
	if changed {
		// Clear all request-session mappings on connection change.
		s.requestSessionList.Init()
		clear(s.requestSessions)
	}
}

func (s *State) SessionID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latestSessionID
}

func (s *State) SetSessionID(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestSessionID = sessionID
}

func (s *State) ProtocolVersion() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.protocolVersion
}

func (s *State) SetProtocolVersion(protocolVersion string) {
	if s == nil || protocolVersion == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.protocolVersion = protocolVersion
}

func (s *State) StoreRequestSession(requestIDKey, sessionID string) {
	if s == nil || requestIDKey == "" || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// If key already exists, update it and move to back (newest).
	if elem, ok := s.requestSessions[requestIDKey]; ok {
		entry := elem.Value.(*requestSessionEntry)
		entry.sessionID = sessionID
		entry.storedAt = time.Now()
		s.requestSessionList.MoveToBack(elem)
		return
	}

	// Evict stale entries from the front (oldest) — O(k) where k is stale count.
	s.evictStaleLocked()

	// Enforce size cap by removing the oldest entry.
	if len(s.requestSessions) >= s.maxRequestSessions {
		s.evictOldestLocked()
	}

	entry := &requestSessionEntry{
		sessionID: sessionID,
		storedAt:  time.Now(),
		key:       requestIDKey,
	}
	elem := s.requestSessionList.PushBack(entry)
	s.requestSessions[requestIDKey] = elem
}

func (s *State) TakeRequestSession(requestIDKey string) (string, bool) {
	if s == nil || requestIDKey == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	elem, ok := s.requestSessions[requestIDKey]
	if !ok {
		return "", false
	}
	entry := elem.Value.(*requestSessionEntry)
	s.requestSessionList.Remove(elem)
	delete(s.requestSessions, requestIDKey)

	// Don't return entries that have expired.
	if time.Since(entry.storedAt) > s.requestSessionTTL {
		return "", false
	}
	return entry.sessionID, true
}

// evictStaleLocked removes entries older than requestSessionTTL from the front
// of the list. Since the list is ordered by insertion time, we stop at the first
// non-stale entry. O(k) where k is the number of stale entries.
// Must be called with s.mu held.
func (s *State) evictStaleLocked() {
	now := time.Now()
	for {
		front := s.requestSessionList.Front()
		if front == nil {
			return
		}
		entry := front.Value.(*requestSessionEntry)
		if now.Sub(entry.storedAt) <= s.requestSessionTTL {
			return // remaining entries are newer, stop
		}
		s.requestSessionList.Remove(front)
		delete(s.requestSessions, entry.key)
	}
}

// evictOldestLocked removes the oldest entry (front of list). O(1).
// Must be called with s.mu held.
func (s *State) evictOldestLocked() {
	front := s.requestSessionList.Front()
	if front == nil {
		return
	}
	entry := front.Value.(*requestSessionEntry)
	s.requestSessionList.Remove(front)
	delete(s.requestSessions, entry.key)
}
