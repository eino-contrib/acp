package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/google/uuid"
	"github.com/hertz-contrib/websocket"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/stream"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	headerUpgrade    = "Upgrade"
	upgradeWebSocket = "websocket"
)

// ACPProxy is a transparent ACP WebSocket proxy handler that can be mounted
// on any hertz router. It forwards JSON-RPC payloads byte-for-byte between
// the north-bound WS client and the south-bound Streamer — see package doc.
type ACPProxy struct {
	factory stream.StreamerFactory
	opts    options

	rootCtx    context.Context
	rootCancel context.CancelFunc

	// sem bounds concurrent connections. nil when the cap is disabled
	// (maxConcurrent <= 0).
	sem chan struct{}

	connsMu sync.Mutex
	conns   map[string]*proxyConn

	closeOnce sync.Once
}

// NewACPProxy builds a proxy handler. factory is required and MUST NOT be
// nil; it is invoked once per incoming WS connection to dial the
// south-bound Streamer.
func NewACPProxy(factory stream.StreamerFactory, opts ...Option) (*ACPProxy, error) {
	if factory == nil {
		return nil, fmt.Errorf("proxy: streamer factory must not be nil")
	}
	resolved := defaultOptions()
	for _, o := range opts {
		if o != nil {
			o(&resolved)
		}
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	p := &ACPProxy{
		factory:    factory,
		opts:       resolved,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
		conns:      make(map[string]*proxyConn),
	}
	if resolved.maxConcurrent > 0 {
		p.sem = make(chan struct{}, resolved.maxConcurrent)
	}
	return p, nil
}

// Mount registers the proxy endpoint on the given hertz router. Passing nil
// is a no-op.
func (p *ACPProxy) Mount(r route.IRoutes) {
	if r == nil {
		return
	}
	r.Any(p.opts.endpoint, p.handler())
}

// Handler returns the hertz handler without registering it on a router. Use
// this when the caller manages routing explicitly.
func (p *ACPProxy) Handler() app.HandlerFunc {
	return p.handler()
}

// Close terminates every active proxied connection and rejects new upgrades.
// Idempotent.
func (p *ACPProxy) Close() error {
	p.closeOnce.Do(func() {
		p.rootCancel()
		p.connsMu.Lock()
		conns := make([]*proxyConn, 0, len(p.conns))
		for _, c := range p.conns {
			conns = append(conns, c)
		}
		p.conns = make(map[string]*proxyConn)
		p.connsMu.Unlock()
		for _, c := range conns {
			c.close(websocket.CloseGoingAway, "proxy shutdown")
		}
	})
	return nil
}

func (p *ACPProxy) handler() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		// Fail fast on any non-WebSocket request. The proxy deliberately does
		// not support Streamable HTTP north-bound (see P1 §2.3); surfacing a
		// crisp 400 avoids silently accepting a broken client configuration.
		if !strings.EqualFold(string(c.GetHeader(headerUpgrade)), upgradeWebSocket) {
			c.String(http.StatusBadRequest, "proxy endpoint only supports WebSocket")
			return
		}
		if !c.IsGet() {
			// WebSocket upgrade must be a GET per RFC 6455.
			c.String(http.StatusBadRequest, "WebSocket upgrade requires GET")
			return
		}
		if p.isClosed() {
			c.String(http.StatusServiceUnavailable, "proxy is shutting down")
			return
		}
		if !p.tryAcquire() {
			c.String(http.StatusServiceUnavailable, "too many concurrent connections")
			return
		}
		// The semaphore is released either:
		//   - inline below when the upgrader itself fails (before any pump
		//     goroutine gets a chance to run), OR
		//   - at the end of serveConn when the connection ends normally.
		released := false
		releaseOnce := func() {
			if !released {
				released = true
				p.release()
			}
		}

		cid := uuid.NewString()
		c.Response.Header.Set(acptransport.HeaderConnectionID, cid)

		var meta map[string]string
		if p.opts.headerForwarder != nil {
			meta = p.opts.headerForwarder(c)
		}

		err := p.opts.upgrader.Upgrade(c, func(wsConn *websocket.Conn) {
			defer releaseOnce()
			p.serveConn(ctx, cid, meta, wsConn)
		})
		if err != nil {
			releaseOnce()
			acplog.CtxWarn(ctx, "proxy[%s]: websocket upgrade failed: %v", cid, err)
		}
	}
}

// serveConn dials the south-bound Streamer and, on success, pumps payloads
// until either side closes. The WS upgrade has already completed by the
// time this runs.
func (p *ACPProxy) serveConn(parentCtx context.Context, cid string, meta map[string]string, wsConn *websocket.Conn) {
	// Detach from the hertz request ctx: once the handler returns, hertz may
	// cancel that ctx, which would terminate an otherwise-healthy long-lived
	// WS session. Base on parentCtx so request-scoped values (trace id,
	// tenant, auth, ...) survive into the long-lived connection, then wire
	// p.rootCtx.Done() so proxy-wide shutdown still tears the conn down.
	connCtx, connCancel := context.WithCancel(context.WithoutCancel(parentCtx))
	defer connCancel()
	safe.CancelOnDone(connCancel, connCtx.Done(), p.rootCtx.Done())

	dialCtx := connCtx
	if p.opts.handshakeTimeout > 0 {
		var dialCancel context.CancelFunc
		dialCtx, dialCancel = context.WithTimeout(connCtx, p.opts.handshakeTimeout)
		defer dialCancel()
	}

	s, err := p.factory.NewStreamer(dialCtx, meta)
	if err != nil {
		acplog.CtxError(parentCtx, "proxy[%s]: new streamer failed: %v", cid, err)
		// Surface the upstream failure through a WS close frame so the Client
		// SDK can include the reason in its diagnostic instead of silently
		// swallowing the error. A short write deadline bounds the failure
		// path: a stalled peer TCP buffer must not pin the handler goroutine
		// (and its concurrency slot) indefinitely.
		reason := wsutil.SafeCloseReason(fmt.Sprintf("upstream: %v", err))
		if p.opts.wsWriteTimeout > 0 {
			_ = wsConn.SetWriteDeadline(time.Now().Add(p.opts.wsWriteTimeout))
		}
		_ = wsConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, reason))
		_ = wsConn.Close()
		return
	}

	pc := &proxyConn{
		id:             cid,
		ws:             wsConn,
		streamer:       s,
		wsWriteTimeout: p.opts.wsWriteTimeout,
		pingInterval:   p.opts.wsPingInterval,
		pongTimeout:    p.opts.wsPongTimeout,
		maxMessageSize: p.opts.maxMessageSize,
	}
	pc.wsWriteMu = &sync.Mutex{}
	// SetReadLimit makes the WS library abort ReadMessage with a 1009
	// (MessageTooBig) close error the moment a frame exceeds the cap,
	// preventing the proxy from allocating buffers for hostile / broken
	// peers. Skipped when the cap is disabled.
	if p.opts.maxMessageSize > 0 {
		wsConn.SetReadLimit(int64(p.opts.maxMessageSize))
	}
	pc.installHeartbeat()

	p.trackConn(pc)
	defer p.untrackConn(cid)

	metaKeys := metaKeyList(meta)
	acplog.CtxInfo(connCtx, "proxy[%s]: connection opened (meta_keys=%v)", cid, metaKeys)
	start := time.Now()
	defer func() {
		acplog.CtxInfo(connCtx, "proxy[%s]: connection closed (duration=%s, reason=%s)", cid, time.Since(start), pc.closeReason())
	}()

	// Propagate root shutdown by closing the connection when rootCtx is done.
	// Also bind to connCtx.Done() so the watcher goroutine exits once the
	// connection ends naturally — otherwise it would survive until proxy
	// shutdown, accumulating one leaked goroutine (and one live *proxyConn
	// reference) per historical connection. pc.close is idempotent
	// (closeOnce), so being woken on the already-closed path is a safe no-op.
	safe.CancelOnDone(
		func() { pc.close(websocket.CloseGoingAway, "proxy shutdown") },
		connCtx.Done(),
		p.rootCtx.Done(),
	)

	pc.run(connCtx)
}

func (p *ACPProxy) trackConn(pc *proxyConn) {
	p.connsMu.Lock()
	p.conns[pc.id] = pc
	p.connsMu.Unlock()
}

func (p *ACPProxy) untrackConn(id string) {
	p.connsMu.Lock()
	delete(p.conns, id)
	p.connsMu.Unlock()
}

func (p *ACPProxy) tryAcquire() bool {
	if p.sem == nil {
		return true
	}
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *ACPProxy) release() {
	if p.sem == nil {
		return
	}
	select {
	case <-p.sem:
	default:
	}
}

func (p *ACPProxy) isClosed() bool {
	select {
	case <-p.rootCtx.Done():
		return true
	default:
		return false
	}
}

func metaKeyList(meta map[string]string) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	// Deterministic order keeps diagnostic logs stable across restarts so
	// operators can diff / grep them without noise from Go's randomised map
	// iteration.
	sort.Strings(keys)
	return keys
}
