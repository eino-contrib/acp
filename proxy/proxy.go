package proxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/stream"
)

var (
	// ErrClosed is returned when admission is attempted after proxy shutdown
	// has begun. Framework adapters translate it to HTTP 503.
	ErrClosed = errors.New("proxy is shutting down")

	// ErrTooManyConnections is returned when the configured admission limit is
	// full. Framework adapters translate it to HTTP 503.
	ErrTooManyConnections = errors.New("too many concurrent connections")
)

// ACPProxy is the framework-neutral ACP WebSocket proxy runtime. It owns the
// Streamer factory, admission limit, and every admitted connection from the
// moment an adapter accepts it until upgrade/factory/pump cleanup completes.
// HTTP routing and WebSocket upgrading belong to proxy/hertz and proxy/gin.
type ACPProxy struct {
	factory stream.StreamerFactory
	opts    options

	sem chan struct{}

	mu      sync.Mutex
	closing bool
	conns   map[string]*Admission
	drained chan struct{}

	closeOnce sync.Once
	drainOnce sync.Once
}

// NewACPProxy constructs a framework-neutral proxy runtime. factory is
// required and is invoked once for every successfully upgraded connection.
func NewACPProxy(factory stream.StreamerFactory, opts ...Option) (*ACPProxy, error) {
	if isNilFactory(factory) {
		return nil, fmt.Errorf("proxy: streamer factory must not be nil")
	}
	resolved := defaultOptions()
	for _, option := range opts {
		if option != nil {
			option(&resolved)
		}
	}
	p := &ACPProxy{
		factory: factory,
		opts:    resolved,
		conns:   make(map[string]*Admission),
		drained: make(chan struct{}),
	}
	if resolved.maxConcurrent > 0 {
		p.sem = make(chan struct{}, resolved.maxConcurrent)
	}
	return p, nil
}

func isNilFactory(factory stream.StreamerFactory) bool {
	if factory == nil {
		return true
	}
	value := reflect.ValueOf(factory)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Admit atomically admits and registers a WebSocket attempt. Adapters call it
// only after the shared WebSocket handshake validation succeeds. Every
// successful Admission must be completed with Serve or Abort.
func (p *ACPProxy) Admit(parent context.Context, headers HeaderGetter) (*Admission, error) {
	if parent == nil {
		parent = context.Background()
	}

	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if !p.tryAcquireLocked() {
		p.mu.Unlock()
		return nil, ErrTooManyConnections
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	a := &Admission{
		proxy:  p,
		id:     uuid.NewString(),
		ctx:    ctx,
		cancel: cancel,
	}
	p.conns[a.id] = a
	p.mu.Unlock()

	meta, err := p.extractMetadata(a.ctx, headers)
	if err != nil {
		acplog.CtxError(parent, "proxy[%s]: metadata extraction failed: %v", a.id, err)
		a.Abort()
		return nil, err
	}
	a.meta = meta
	a.metaKeys = metaKeyList(meta)
	a.ready.Store(true)
	if a.isClosing() {
		a.Abort()
		return nil, ErrClosed
	}
	return a, nil
}

// Close atomically stops admission, cancels all admitted work, and starts
// asynchronous resource convergence. It is idempotent and does not wait for
// WebSocket, Streamer, or factory implementations to return.
func (p *ACPProxy) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		admissions := make([]*Admission, 0, len(p.conns))
		for _, admission := range p.conns {
			admissions = append(admissions, admission)
		}
		if len(admissions) == 0 {
			p.signalDrainedLocked()
		}
		p.mu.Unlock()

		for _, admission := range admissions {
			admission.beginShutdown()
		}
	})
	return nil
}

// Shutdown starts the same close path as Close and waits until every admitted
// upgrade, downstream factory call, and active pump has left the registry. If
// ctx expires first, its error is returned.
func (p *ACPProxy) Shutdown(ctx context.Context) error {
	_ = p.Close()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-p.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ACPProxy) extractMetadata(ctx context.Context, headers HeaderGetter) (meta map[string]string, err error) {
	if p.opts.metadataExtractor == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			meta = nil
			err = fmt.Errorf("proxy: metadata extractor panic: %v", recovered)
		}
	}()
	extracted := p.opts.metadataExtractor(ctx, headers)
	if len(extracted) == 0 {
		return nil, nil
	}
	meta = make(map[string]string, len(extracted))
	for key, value := range extracted {
		meta[key] = value
	}
	return meta, nil
}

func (p *ACPProxy) tryAcquireLocked() bool {
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

func (p *ACPProxy) untrack(admission *Admission) {
	p.mu.Lock()
	if current, ok := p.conns[admission.id]; ok && current == admission {
		delete(p.conns, admission.id)
	}
	if p.closing && len(p.conns) == 0 {
		p.signalDrainedLocked()
	}
	p.mu.Unlock()
}

func (p *ACPProxy) signalDrainedLocked() {
	p.drainOnce.Do(func() { close(p.drained) })
}

// Admission bridges one framework upgrade into the proxy runtime. Its methods
// are safe to race with one another and with ACPProxy.Close.
type Admission struct {
	proxy    *ACPProxy
	id       string
	meta     map[string]string
	metaKeys []string

	ctx     context.Context
	cancel  context.CancelFunc
	ready   atomic.Bool
	claimed atomic.Bool

	mu       sync.Mutex
	closing  atomic.Bool
	ws       WebSocketConn
	active   *proxyConn
	rawClose sync.Once

	finishOnce sync.Once
}

// ConnectionID is the value an adapter includes in a successful WebSocket
// handshake response.
func (a *Admission) ConnectionID() string {
	if a == nil {
		return ""
	}
	return a.id
}

// Abort completes an admission whose framework upgrade failed. It is safe to
// call more than once; if Serve already claimed the admission, Abort is a
// no-op.
func (a *Admission) Abort() {
	if a == nil || !a.claimed.CompareAndSwap(false, true) {
		return
	}
	a.cancel()
	a.finish()
}

// Serve claims a successfully upgraded connection, creates its downstream
// Streamer, and runs the shared proxy pump. It blocks until all admitted work
// has converged. Adapter implementations should invoke it exactly once.
func (a *Admission) Serve(ws WebSocketConn) {
	if a == nil {
		closeOrphanWS(ws)
		return
	}
	if !a.claimed.CompareAndSwap(false, true) {
		if a.isClosing() {
			closeDetachedWS(ws, closeAction{SendFrame: true, Code: wsconn.CloseGoingAway, Reason: "proxy shutdown"})
		} else {
			closeOrphanWS(ws)
		}
		return
	}
	defer a.finish()
	if ws == nil {
		acplog.CtxWarn(a.ctx, "proxy[%s]: adapter supplied nil websocket connection", a.id)
		return
	}

	a.mu.Lock()
	a.ws = ws
	alreadyClosing := a.closing.Load()
	a.mu.Unlock()
	if alreadyClosing {
		a.closeRaw(closeAction{SendFrame: true, Code: wsconn.CloseGoingAway, Reason: "proxy shutdown"})
		return
	}

	a.serve(ws)
}

func (a *Admission) serve(ws WebSocketConn) {
	resultCh := make(chan factoryResult, 1)
	dialCtx := a.ctx
	dialCancel := func() {}
	if a.proxy.opts.handshakeTimeout > 0 {
		dialCtx, dialCancel = context.WithTimeout(a.ctx, a.proxy.opts.handshakeTimeout)
	}
	defer dialCancel()

	safe.Go(func() {
		resultCh <- callFactory(a.proxy.factory, dialCtx, a.meta)
	})

	var result factoryResult
	select {
	case result = <-resultCh:
	case <-dialCtx.Done():
		// Close the upgraded side promptly, but keep this admission registered
		// until the factory actually returns. This makes Shutdown truthful when
		// an implementation ignores cancellation.
		err := dialCtx.Err()
		if !a.isClosing() {
			acplog.CtxError(a.ctx, "proxy[%s]: new streamer failed: %v", a.id, err)
		}
		a.closeRaw(a.factoryFailureAction(err, false))
		result = <-resultCh
		if !isNilStreamer(result.streamer) {
			if err := a.closeRawBeforeStreamer(
				a.factoryFailureAction(err, false),
				result.streamer,
				"downstream creation completed after cancellation",
			); err != nil {
				acplog.CtxWarn(a.ctx, "proxy[%s]: close late streamer failed: %v", a.id, err)
			}
		}
		return
	}
	if err := dialCtx.Err(); err != nil {
		if !a.isClosing() {
			acplog.CtxError(a.ctx, "proxy[%s]: new streamer failed: %v", a.id, err)
		}
		a.closeRaw(a.factoryFailureAction(err, false))
		if !isNilStreamer(result.streamer) {
			if err := a.closeRawBeforeStreamer(
				a.factoryFailureAction(err, false),
				result.streamer,
				"downstream creation completed after timeout",
			); err != nil {
				acplog.CtxWarn(a.ctx, "proxy[%s]: close timed-out streamer failed: %v", a.id, err)
			}
		}
		return
	}

	if result.err != nil || isNilStreamer(result.streamer) {
		err := result.err
		if err == nil {
			err = errors.New("streamer factory returned nil streamer")
		}
		acplog.CtxError(a.ctx, "proxy[%s]: new streamer failed: %v", a.id, err)
		action := a.factoryFailureAction(err, result.panicked)
		if !isNilStreamer(result.streamer) {
			if closeErr := a.closeRawBeforeStreamer(action, result.streamer, "downstream creation failed"); closeErr != nil {
				acplog.CtxWarn(a.ctx, "proxy[%s]: close failed streamer failed: %v", a.id, closeErr)
			}
		} else {
			a.closeRaw(action)
		}
		return
	}

	pc := &proxyConn{
		id:                a.id,
		ws:                ws,
		streamer:          result.streamer,
		wsWriteTimeout:    a.proxy.opts.wsWriteTimeout,
		readTimeout:       a.proxy.opts.wsReadTimeout,
		firstFrameTimeout: a.proxy.opts.firstFrameTimeout,
		maxMessageSize:    a.proxy.opts.maxMessageSize,
	}
	a.mu.Lock()
	if a.closing.Load() {
		a.mu.Unlock()
		if err := a.closeRawBeforeStreamer(
			closeAction{SendFrame: true, Code: wsconn.CloseGoingAway, Reason: "proxy shutdown"},
			result.streamer,
			"proxy shutdown",
		); err != nil {
			acplog.CtxWarn(a.ctx, "proxy[%s]: close streamer during shutdown failed: %v", a.id, err)
		}
		return
	}
	// Configure the connection before publishing it as active. Holding the
	// admission lock keeps Close from concurrently closing the socket while
	// SetReadLimit, SetPingHandler, or the initial deadline is being installed.
	if a.proxy.opts.maxMessageSize > 0 {
		ws.SetReadLimit(int64(a.proxy.opts.maxMessageSize))
	}
	pc.installHeartbeat()
	a.active = pc
	a.mu.Unlock()

	acplog.CtxInfo(a.ctx, "proxy[%s]: connection opened (meta_keys=%v)", a.id, a.metaKeys)
	started := time.Now()
	defer func() {
		acplog.CtxInfo(a.ctx, "proxy[%s]: connection closed (duration=%s, reason=%s)", a.id, time.Since(started), pc.getCloseReason())
	}()

	pc.run(a.ctx)
}

func (a *Admission) factoryFailureAction(err error, panicked bool) closeAction {
	if a.isClosing() {
		return closeAction{SendFrame: true, Code: wsconn.CloseGoingAway, Reason: "proxy shutdown"}
	}
	return factoryCloseAction(err, panicked)
}

// closeRawBeforeStreamer enforces the teardown order for every upgraded
// admission that obtained a Streamer. Streamer.Close is user code and may
// block, so it must never delay closing the north-bound WebSocket.
func (a *Admission) closeRawBeforeStreamer(action closeAction, streamer stream.Streamer, reason string) error {
	a.closeRaw(action)
	return closeStreamerSafely(streamer, reason)
}

func (a *Admission) beginShutdown() {
	if a == nil {
		return
	}
	a.closing.Store(true)
	a.cancel()
	// An adapter may still be inside a synchronous upgrade or waiting for an
	// asynchronous Hertz hijack callback. Keep that admission registered until
	// the adapter reports its real outcome through Serve or Abort; otherwise
	// Shutdown could report success before a late upgraded socket is closed.
	if !a.claimed.Load() {
		return
	}
	// Socket and Streamer closure can invoke user or network code. Keep it off
	// the Close caller's goroutine so Close only marks state and broadcasts
	// cancellation; Shutdown is the API that waits for full convergence.
	safe.Go(func() {
		a.mu.Lock()
		active := a.active
		hasWS := a.ws != nil
		a.mu.Unlock()
		if active != nil {
			active.close(closeAction{SendFrame: true, Code: wsconn.CloseGoingAway, Reason: "proxy shutdown"})
		} else if hasWS {
			a.closeRaw(closeAction{SendFrame: true, Code: wsconn.CloseGoingAway, Reason: "proxy shutdown"})
		}
	})
}

func (a *Admission) isClosing() bool {
	return a.closing.Load()
}

func (a *Admission) closeRaw(action closeAction) {
	a.mu.Lock()
	ws := a.ws
	a.mu.Unlock()
	if ws == nil {
		return
	}
	a.rawClose.Do(func() {
		if action.SendFrame {
			_ = writeControlSafely(ws, wsconn.CloseMessage,
				wsconn.FormatCloseMessage(action.Code, wsutil.SafeCloseReason(action.Reason)),
				time.Now().Add(wsutil.ControlWriteDeadline))
		}
		_ = closeWebSocketSafely(ws)
	})
}

func (a *Admission) finish() {
	a.finishOnce.Do(func() {
		a.cancel()
		a.proxy.untrack(a)
		a.proxy.release()
	})
}

type factoryResult struct {
	streamer stream.Streamer
	err      error
	panicked bool
}

func callFactory(factory stream.StreamerFactory, ctx context.Context, meta map[string]string) (result factoryResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result.streamer = nil
			result.err = fmt.Errorf("streamer factory panic: %v", recovered)
			result.panicked = true
		}
	}()
	result.streamer, result.err = factory.NewStreamer(ctx, meta)
	return result
}

func isNilStreamer(streamer stream.Streamer) bool {
	if streamer == nil {
		return true
	}
	value := reflect.ValueOf(streamer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func factoryCloseAction(err error, panicked bool) closeAction {
	reason := "upstream: internal error"
	if !panicked {
		reason = fmt.Sprintf("upstream: %v", err)
	}
	return closeAction{SendFrame: true, Code: wsconn.CloseInternalServerErr, Reason: reason}
}

func closeOrphanWS(ws WebSocketConn) {
	closeDetachedWS(ws, closeAction{SendFrame: true, Code: wsconn.CloseInternalServerErr, Reason: "internal error"})
}

func closeDetachedWS(ws WebSocketConn, action closeAction) {
	if ws == nil {
		return
	}
	if action.SendFrame {
		_ = ws.WriteControl(wsconn.CloseMessage,
			wsconn.FormatCloseMessage(action.Code, wsutil.SafeCloseReason(action.Reason)),
			time.Now().Add(wsutil.ControlWriteDeadline))
	}
	_ = ws.Close()
}

func metaKeyList(meta map[string]string) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
