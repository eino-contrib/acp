package server

import (
	"context"
	"sync"
	"time"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
)

// connTable stores HTTP remote connections and evicts idle ones.
// WebSocket connections are intentionally not tracked here.
type connTable struct {
	mu          sync.RWMutex
	conns       map[string]*httpRemoteConnection
	idleTimeout time.Duration
	logger      acplog.Logger

	done       chan struct{}
	closeOnce  sync.Once
	reaperOnce sync.Once
	reaperWg   sync.WaitGroup
}

func newConnTable(idleTimeout time.Duration, logger acplog.Logger) *connTable {
	return &connTable{
		conns:       make(map[string]*httpRemoteConnection),
		idleTimeout: idleTimeout,
		logger:      logger,
		done:        make(chan struct{}),
	}
}

func (t *connTable) add(c *httpRemoteConnection) {
	t.mu.Lock()
	t.conns[c.id] = c
	t.mu.Unlock()
}

// get returns the connection for id. If present but idle, it is evicted and
// (nil, false) is returned.
func (t *connTable) get(id string) (*httpRemoteConnection, bool) {
	t.mu.RLock()
	c, ok := t.conns[id]
	t.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if c.IsIdle(t.idleTimeout) {
		t.delete(id)
		return nil, false
	}
	return c, true
}

// delete removes and closes the connection for id.
func (t *connTable) delete(id string) (*httpRemoteConnection, bool) {
	t.mu.Lock()
	c, ok := t.conns[id]
	if ok {
		delete(t.conns, id)
	}
	t.mu.Unlock()
	if !ok {
		return nil, false
	}
	if err := c.Close(); err != nil {
		t.logger.CtxInfo(context.Background(), "close connection %s: %v", id, err)
	}
	return c, true
}

// startReaper spawns the idle reaper on first call. rootCtx is an external
// stop signal so server shutdown also stops the reaper.
func (t *connTable) startReaper(rootCtx context.Context) {
	t.reaperOnce.Do(func() {
		interval := reaperInterval(t.idleTimeout)
		if interval <= 0 {
			return
		}
		t.reaperWg.Add(1)
		safe.GoWithLogger(t.logger, func() {
			defer t.reaperWg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					t.evictIdle()
				case <-t.done:
					return
				case <-rootCtx.Done():
					return
				}
			}
		})
	})
}

func (t *connTable) evictIdle() {
	if t.idleTimeout <= 0 {
		return
	}
	var staleIDs []string
	t.mu.RLock()
	for id, c := range t.conns {
		if c.IsIdle(t.idleTimeout) {
			staleIDs = append(staleIDs, id)
		}
	}
	t.mu.RUnlock()
	for _, id := range staleIDs {
		if c, ok := t.evictIfIdle(id); ok && c != nil {
			c.Logger().CtxInfo(context.Background(), "evicted stale remote HTTP connection %s", id)
		}
	}
}

// evictIfIdle deletes a connection only if it is still idle at the moment of
// deletion, preventing a TOCTOU race where a connection becomes active
// between idle detection and eviction.
func (t *connTable) evictIfIdle(id string) (*httpRemoteConnection, bool) {
	t.mu.Lock()
	c, ok := t.conns[id]
	if !ok {
		t.mu.Unlock()
		return nil, false
	}
	if !c.IsIdle(t.idleTimeout) {
		t.mu.Unlock()
		return nil, false
	}
	delete(t.conns, id)
	t.mu.Unlock()
	if err := c.Close(); err != nil {
		t.logger.CtxInfo(context.Background(), "close connection %s: %v", id, err)
	}
	return c, true
}

// close stops the reaper and closes every tracked connection.
func (t *connTable) close() {
	t.closeOnce.Do(func() {
		close(t.done)
	})
	t.reaperWg.Wait()
	t.mu.Lock()
	conns := make([]*httpRemoteConnection, 0, len(t.conns))
	for id, c := range t.conns {
		conns = append(conns, c)
		delete(t.conns, id)
	}
	t.mu.Unlock()
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.logger.CtxInfo(context.Background(), "close connection: %v", err)
		}
	}
}

func reaperInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	interval := timeout / 2
	if interval <= 0 {
		interval = timeout
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	return interval
}
