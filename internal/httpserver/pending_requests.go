package httpserver

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// ReverseCallResponse holds a response delivered by the remote side in reply
// to a reverse call (e.g. an agent-to-client request).
type ReverseCallResponse struct {
	Result json.RawMessage
	Error  error // typically *acp.RPCError
}

// PendingRequests tracks outstanding agent-to-client requests. When the agent
// calls ReadTextFile etc., a pending entry is registered. When the client POSTs
// back a response, it is delivered here.
type PendingRequests struct {
	mu      sync.RWMutex
	pending map[string]chan ReverseCallResponse
	nextID  atomic.Int64
	closed  atomic.Bool
}

// NewPendingRequests creates a new PendingRequests tracker.
func NewPendingRequests() *PendingRequests {
	return &PendingRequests{
		pending: make(map[string]chan ReverseCallResponse),
	}
}

// NextID allocates a new monotonically increasing request ID.
func (p *PendingRequests) NextID() int64 {
	return p.nextID.Add(1)
}

// Register creates a pending response channel for the given request ID key.
// The caller should defer Cancel(idKey) to clean up if the context is cancelled
// before a response arrives.
// Returns nil if the PendingRequests has been closed.
func (p *PendingRequests) Register(idKey string) chan ReverseCallResponse {
	if p.closed.Load() {
		return nil
	}
	ch := make(chan ReverseCallResponse, 1)
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return nil
	}
	p.pending[idKey] = ch
	p.mu.Unlock()
	return ch
}

// Deliver delivers a client response to the pending request identified by idKey.
// Returns true if the request was found and the response was delivered.
func (p *PendingRequests) Deliver(idKey string, resp ReverseCallResponse) bool {
	p.mu.Lock()
	ch, ok := p.pending[idKey]
	if ok {
		delete(p.pending, idKey)
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	ch <- resp
	return true
}

// Cancel removes a pending request without delivering a response and closes
// the channel. This unblocks any goroutine waiting on the channel.
func (p *PendingRequests) Cancel(idKey string) {
	p.mu.Lock()
	ch, ok := p.pending[idKey]
	if ok {
		delete(p.pending, idKey)
	}
	p.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Close cancels all pending requests by closing their channels.
func (p *PendingRequests) Close() {
	p.mu.Lock()
	p.closed.Store(true)
	for idKey, ch := range p.pending {
		close(ch)
		delete(p.pending, idKey)
	}
	p.mu.Unlock()
}
