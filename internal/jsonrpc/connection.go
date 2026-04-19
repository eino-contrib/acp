package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/eino-contrib/acp"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/transport"
)

// Transport is an alias for transport.Transport kept for internal compatibility.
type Transport = transport.Transport

// MethodHandler handles an incoming JSON-RPC request.
type MethodHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// NotificationHandler handles an incoming JSON-RPC notification.
type NotificationHandler func(ctx context.Context, method string, params json.RawMessage) error

// defaultRequestWorkers is the size of the per-connection worker pool that
// consumes Requests and unordered Notifications. The read loop never blocks
// on dispatch (queues are unbounded), so this bounds only concurrent handler
// execution. See WithRequestWorkers to override.
const defaultRequestWorkers = 8

// defaultMaxPendingDispatch caps the intake queue depth by default so a
// misbehaving or flooding peer cannot grow the dispatch queue without bound.
// Reached-cap triggers the ErrServerBusy overflow path and terminates the
// connection — shedding load is preferable to silent memory growth inside
// the host process. Callers who want the legacy unbounded behavior set
// WithMaxPendingDispatch to a negative value.
const defaultMaxPendingDispatch = 4096

// defaultShutdownTimeout bounds how long Close/Start will wait for queued
// notification handlers to drain after the connection context is cancelled.
// When the deadline expires the drain context is cancelled (so cooperative
// handlers can observe the shutdown) and wg.Wait is abandoned (so a single
// stuck handler cannot pin the connection forever). Configurable via
// WithShutdownTimeout.
const defaultShutdownTimeout = 30 * time.Second

const genericInternalErrorMessage = "internal error"

// ConnectionOption configures a JSON-RPC connection.
type ConnectionOption func(*Connection)

// WithOrderedNotificationMatcher configures which notification methods must be
// processed sequentially. When matcher returns true, notifications for that
// method are queued and drained in receive order.
func WithOrderedNotificationMatcher(matcher func(method string) bool) ConnectionOption {
	return func(c *Connection) {
		if matcher != nil {
			c.orderedNotificationMatcher = matcher
		}
	}
}

// WithMaxConsecutiveParseErrors sets the threshold for consecutive invalid
// messages (parse errors or invalid JSON-RPC versions) before the connection
// is terminated. Zero means unlimited (default for backward compatibility).
func WithMaxConsecutiveParseErrors(n int) ConnectionOption {
	return func(c *Connection) {
		if n > 0 {
			c.maxConsecutiveParseErrors = n
		}
	}
}

// WithRequestTimeout sets a per-request timeout applied to the context passed
// to request handlers. This protects WS/stdio connections from handlers that
// never return. Zero disables the timeout (default).
func WithRequestTimeout(d time.Duration) ConnectionOption {
	return func(c *Connection) {
		if d > 0 {
			c.requestTimeout = d
		}
	}
}

// WithRequestWorkers overrides the size of the per-connection worker pool.
// Non-positive values are ignored. Workers consume incoming Requests and
// unordered Notifications from an unbounded intake queue; this cap bounds
// concurrent handler execution without blocking the read loop.
func WithRequestWorkers(n int) ConnectionOption {
	return func(c *Connection) {
		if n > 0 {
			c.requestWorkers = n
		}
	}
}

// WithMaxPendingDispatch caps the depth of each dispatch queue (the shared
// request/notification intake and the ordered-notification queue). When a new
// frame would push a queue beyond this limit, the connection is terminated
// with a ServerBusy error.
//
// Default is defaultMaxPendingDispatch — safe ceiling on memory growth when a
// peer floods faster than handlers can drain. Pass a positive value to tune;
// pass a negative value to explicitly disable the cap (unbounded — the legacy
// "never drop" behavior, which accepts unbounded memory growth under
// overload). Zero is a no-op.
func WithMaxPendingDispatch(n int) ConnectionOption {
	return func(c *Connection) {
		switch {
		case n > 0:
			c.maxPendingDispatch = n
		case n < 0:
			c.maxPendingDispatch = 0
		}
	}
}

// WithShutdownTimeout bounds how long Close/Start wait for queued
// notification handlers to drain after the connection context is cancelled.
// When the deadline expires the drain context is cancelled so cooperative
// handlers can exit, and wg.Wait is abandoned so a single stuck handler
// cannot pin the caller forever. A positive duration sets the timeout;
// zero selects the default (defaultShutdownTimeout); a negative value
// disables the bound entirely (legacy "wait forever" behavior).
func WithShutdownTimeout(d time.Duration) ConnectionOption {
	return func(c *Connection) {
		switch {
		case d > 0:
			c.shutdownTimeout = d
		case d < 0:
			c.shutdownTimeout = -1
		}
	}
}

// WithConnectionLabel tags panic and handler-error logs with an identifier
// (e.g. a connection ID) so operators can correlate log lines to the
// originating connection.
func WithConnectionLabel(label string) ConnectionOption {
	return func(c *Connection) {
		c.label = label
	}
}

// WithNotificationErrorHandler registers a callback invoked when a
// notification handler returns an error or a handler panics. Since
// notifications have no response, errors would otherwise be visible only in
// logs. The callback is invoked synchronously from the dispatch goroutine;
// panics inside it are recovered and logged. Passing nil clears the hook.
func WithNotificationErrorHandler(fn func(method string, err error)) ConnectionOption {
	return func(c *Connection) {
		c.OnError = fn
	}
}

// Connection manages a JSON-RPC 2.0 session over a Transport.
//
// Synchronization overview:
//   - startMu: guards the one-time initialization in Start (ctx, cancel, ready)
//   - errMu: guards termErr (RWMutex — many readers, rare writer)
//   - pending: sync.Map for in-flight request→response matching
//   - closed: atomic.Bool for idempotent transport close
//   - closeOnce: ensures done channel is closed exactly once
//   - started: atomic.Bool for fast pre-Start checks
//   - wg: tracks background goroutines (readLoop, worker pool, ordered drain,
//     per-request timeout watchers)
//
// Outbound writes are not serialized at this layer: Transport implementations
// are required to be safe for concurrent WriteMessage calls and must handle
// their own backpressure (outbox channel, semaphore, etc.). Serializing here
// would simply duplicate what every transport already does and would let a
// single slow WriteMessage block unrelated callers (including error/parse
// responses) under contention.
//
// Dispatch model:
//   - The read loop never blocks on dispatch. Responses are handled inline
//     (O(1), never blocks); Requests and unordered Notifications are pushed
//     onto an unbounded intake queue consumed by a fixed worker pool; ordered
//     Notifications are pushed onto a separate unbounded queue drained by a
//     single goroutine. This is required because blocking the read loop
//     would starve Response dispatch and can deadlock handlers that call
//     SendRequest on the same connection.
type Connection struct {
	// transport is the underlying framed byte stream used to read and write
	// JSON-RPC messages. Its lifetime is bound to the connection: Close is
	// called exactly once via the `closed` guard below.
	transport Transport
	// requestHandler dispatches incoming JSON-RPC Requests. When nil, every
	// request is answered with MethodNotFound.
	requestHandler MethodHandler
	// notificationHandler dispatches incoming JSON-RPC Notifications. When
	// nil, notifications are silently dropped (spec-compliant behavior).
	notificationHandler NotificationHandler

	// OnError is called when a notification handler returns an error.
	// Since notifications have no response, errors would otherwise be lost.
	OnError func(method string, err error)

	// nextID is a monotonically increasing counter that mints unique IDs for
	// outbound requests. Only accessed via atomic ops.
	nextID atomic.Int64
	// pending maps an outbound request ID (string) to its *pendingResponse.
	// Populated before the request is written and drained by readLoop when
	// the matching response arrives (or by Close on termination).
	pending sync.Map // map[string]*pendingResponse
	// startMu serializes Start so only one goroutine performs the one-time
	// initialization of ctx, cancel, and the ready channel.
	startMu sync.Mutex
	// closed ensures Transport.Close is invoked at most once. Every code
	// path that wants to close the transport CAS-flips this flag first.
	closed atomic.Bool
	// errMu guards termErr. RWMutex because termErr is read frequently via
	// WaitErr but written at most once on termination.
	errMu sync.RWMutex

	// requestQueue carries Requests and unordered Notifications to the
	// worker pool. Unbounded so the read loop never blocks on Push.
	requestQueue *unboundedQueue
	// orderedNotificationQueue serializes notifications whose method
	// matches orderedNotificationMatcher (e.g. ACP session/update chunks).
	orderedNotificationQueue *unboundedQueue
	// orderedNotificationMatcher decides which notification methods are
	// routed to orderedNotificationQueue. Returning true means "preserve
	// receive order for this method".
	orderedNotificationMatcher func(method string) bool
	// maxConsecutiveParseErrors is the threshold of back-to-back invalid
	// frames (parse errors or bad JSON-RPC versions) tolerated before the
	// connection is terminated. Zero means unlimited.
	maxConsecutiveParseErrors int
	// requestTimeout is the per-request deadline applied to the context
	// handed to request handlers. Zero disables the timeout.
	requestTimeout time.Duration
	// requestWorkers caps the size of the worker pool that drains
	// requestQueue, bounding concurrent handler execution.
	requestWorkers int
	// maxPendingDispatch caps the depth of each dispatch queue. Zero means
	// unbounded. When the cap is exceeded the connection terminates with
	// ErrServerBusy instead of silently growing memory.
	maxPendingDispatch int
	// shutdownTimeout caps how long Close/Start wait for queued notification
	// handlers to drain after ctx cancellation. When the deadline expires
	// the drain context is cancelled and wg.Wait is abandoned. Zero selects
	// defaultShutdownTimeout; a negative value disables the bound.
	shutdownTimeout time.Duration

	// drainCtxOnce guards the lazy creation of drainCtx/drainCancel. The
	// drain context is created the first time a worker observes c.ctx as
	// cancelled and handed to handlers in place of context.WithoutCancel so
	// cooperative handlers can observe a bounded shutdown.
	drainCtxOnce sync.Once
	drainCtx     context.Context
	drainCancel  context.CancelFunc

	// label is a human-readable connection identifier (e.g. a session or
	// connection ID) attached to panic and error log lines for correlation.
	label string
	// activeHandlers tracks the number of user handler invocations (request
	// + notification) currently in flight. Inc'd around
	// handleRequestSafely/handleNotificationSafely and observed by the
	// shutdown path to report how many handlers pinned wg when
	// waitWithShutdownTimeout fires. Purely diagnostic — does not gate any
	// code path.
	activeHandlers atomic.Int32
	// parentCtx is the caller-supplied context passed to Start. Kept so the
	// Start return path can distinguish a caller-side cancellation (parent
	// Err() non-nil) from a local Close()-driven shutdown (parent Err() nil).
	// Never mutated after Start's one-time init.
	parentCtx context.Context
	// ctx is the per-connection context. It is cancelled on Close or on
	// transport EOF and is propagated into every handler invocation.
	ctx context.Context
	// cancel cancels ctx. Invoked from Close and from readLoop termination;
	// safe to call multiple times.
	cancel context.CancelFunc
	// wg tracks every background goroutine owned by the connection
	// (readLoop, worker pool, ordered-notification drainer, per-request
	// timeout watchers). Close blocks on wg before returning so callers
	// observe a fully quiesced connection.
	wg sync.WaitGroup
	// started is a fast-path flag used to reject Send/Close calls issued
	// before Start has run.
	started atomic.Bool
	// ready is closed once Start has finished initializing ctx/cancel and
	// has spun up the background goroutines. Callers that race with Start
	// can block on it to observe a fully constructed connection.
	ready chan struct{}
	// done is closed exactly once when the connection has fully shut down.
	// External callers block on it via Wait to learn about termination.
	done chan struct{}
	// readDone is closed when readLoop returns. Used internally to sequence
	// shutdown (drain queues, then close `done`).
	readDone chan struct{}
	// closeOnce guards the one-time closing of the `done` channel so that
	// Wait never sees a double-close panic.
	closeOnce sync.Once
	// termErr is the final termination error (nil on clean close). Written
	// once under errMu and surfaced through WaitErr.
	termErr error
}

type pendingResponse struct {
	ch chan responseResult
}

type responseResult struct {
	result json.RawMessage
	err    error
}

// NewConnection creates a new Connection.
// requestHandler handles incoming requests (must not be nil).
// notificationHandler handles incoming notifications (may be nil; a missing
// handler is reported through OnError when notifications are received).
func NewConnection(transport Transport, requestHandler MethodHandler, notificationHandler NotificationHandler, opts ...ConnectionOption) *Connection {
	c := &Connection{
		transport:           transport,
		requestHandler:      requestHandler,
		notificationHandler: notificationHandler,
		requestWorkers:      defaultRequestWorkers,
		maxPendingDispatch:  defaultMaxPendingDispatch,
		shutdownTimeout:     defaultShutdownTimeout,
		requestQueue:        newUnboundedQueue("request"),
		ready:               make(chan struct{}),
		done:                make(chan struct{}),
		readDone:            make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.orderedNotificationMatcher != nil {
		c.orderedNotificationQueue = newUnboundedQueue("ordered-notification")
	}
	return c
}

// Start begins reading from the transport. Blocks until the connection closes.
//
// Start must be called exactly once. If called concurrently, additional callers
// safely join the first caller's lifetime — they block on the same context and
// return when the connection ends. No duplicate read loops are created.
func (c *Connection) Start(ctx context.Context) error {
	c.startMu.Lock()
	if !c.started.Load() {
		c.parentCtx = ctx
		c.ctx, c.cancel = context.WithCancel(ctx)
		c.started.Store(true)
		close(c.ready)

		c.wg.Add(1)
		safe.Go(c.readLoop)
		if c.orderedNotificationMatcher != nil {
			c.wg.Add(1)
			safe.Go(c.processOrderedNotifications)
		}
		for i := 0; i < c.requestWorkers; i++ {
			c.wg.Add(1)
			safe.Go(c.requestWorkerLoop)
		}
	}
	connCtx := c.ctx
	c.startMu.Unlock()

	<-connCtx.Done()
	if err := c.closeTransport(); err != nil {
		c.logDebugf("close transport: %v", err)
	}
	// Signal done BEFORE waiting for handler goroutines. This unblocks
	// SendRequest callers immediately when the connection is shutting down,
	// even if some request handlers are still running (e.g. ignoring ctx).
	c.closeOnce.Do(func() { close(c.done) })
	// Wait for readLoop to exit before closing queues. This avoids a race
	// where Push would hit a closed queue and silently drop messages; we
	// guarantee "no drops while the read loop is running".
	<-c.readDone
	c.requestQueue.Close()
	if c.orderedNotificationQueue != nil {
		c.orderedNotificationQueue.Close()
	}
	c.waitWithShutdownTimeout()
	if err := c.TerminalError(); err != nil {
		return err
	}
	// Distinguish caller-side cancellation from a local Close(). The parent
	// context is the caller's ctx; cancelling our derived `connCtx` does NOT
	// propagate back up, so `parentCtx.Err() != nil` means the caller's own
	// context fired (timeout/cancel) — surface that error. Otherwise the
	// shutdown was driven by Close() and we return nil so callers don't have
	// to filter context.Canceled out of their error paths.
	if c.parentCtx != nil {
		if err := c.parentCtx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// WaitUntilStarted waits until Start has initialized the connection.
func (c *Connection) WaitUntilStarted(ctx context.Context) error {
	if c.started.Load() {
		return nil
	}

	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close shuts down the connection.
//
// If Start is running in another goroutine, Close cancels it and waits for
// all background goroutines (readLoop, workers, ordered drain, request-timeout
// watchers) to exit before returning. This prevents handler goroutines from
// outliving Close and racing with upstream teardown (e.g. WS server finalizing
// the websocket, agent cleanup).
func (c *Connection) Close() error {
	c.startMu.Lock()
	cancel := c.cancel
	started := c.started.Load()
	c.startMu.Unlock()

	if cancel != nil {
		cancel()
	}
	err := c.closeTransport()
	c.closeOnce.Do(func() { close(c.done) })
	if started {
		c.waitWithShutdownTimeout()
	}
	return err
}

// waitWithShutdownTimeout blocks on c.wg with a bound dictated by
// shutdownTimeout. The drain context carries the same deadline so
// cooperative handlers eventually observe cancellation; if any handler
// ignores ctx and refuses to return, wg.Wait is abandoned with a log line
// and the handler is allowed to continue on its own goroutine — preferable
// to pinning the caller (and the transport close path) forever.
//
// Caveat: the waiter goroutine spawned below still calls c.wg.Wait() and
// therefore blocks until every stuck handler eventually returns (Go offers
// no way to cancel wg.Wait). That waiter shares the handler's fate — both
// exit when the handler finally finishes, or both leak forever if it
// doesn't. Abandoning the caller after the timeout is the best we can do
// here; the active-handler count is logged on timeout so operators can spot
// the leaked handler(s) responsible.
//
// A negative shutdownTimeout restores the legacy unbounded behaviour.
func (c *Connection) waitWithShutdownTimeout() {
	timeout := c.shutdownTimeout
	if timeout < 0 {
		c.wg.Wait()
		c.releaseDrainCtx()
		return
	}
	if timeout == 0 {
		timeout = defaultShutdownTimeout
	}

	waited := make(chan struct{})
	safe.Go(func() {
		c.wg.Wait()
		close(waited)
	})
	select {
	case <-waited:
	case <-time.After(timeout):
		c.logf("shutdown timeout (%v) exceeded waiting for handlers; abandoning wait (active handlers=%d)", timeout, c.activeHandlers.Load())
	}
	c.releaseDrainCtx()
}

// getDrainCtx returns a context handed to notification handlers that are
// still draining after c.ctx has been cancelled. The context is derived from
// context.Background() so queued handlers do not observe the shutdown
// immediately, but it carries a deadline equal to shutdownTimeout so a
// well-behaved handler eventually sees cancellation and can exit. Handlers
// that ignore ctx still run to completion, but they no longer block the
// caller indefinitely (see waitWithShutdownTimeout).
func (c *Connection) getDrainCtx() context.Context {
	c.drainCtxOnce.Do(func() {
		timeout := c.shutdownTimeout
		if timeout < 0 {
			// Legacy behaviour: never cancel the drain context. Handlers
			// that ignore ctx will still let wg.Wait run unbounded.
			c.drainCtx = context.WithoutCancel(c.ctx)
			c.drainCancel = func() {}
			return
		}
		if timeout == 0 {
			timeout = defaultShutdownTimeout
		}
		c.drainCtx, c.drainCancel = context.WithTimeout(context.WithoutCancel(c.ctx), timeout)
	})
	return c.drainCtx
}

// releaseDrainCtx releases the drain context's timer and cancel resources
// once the shutdown path has stopped waiting on it. Safe to call when the
// drain context was never materialised — getDrainCtx lazily creates a
// no-op cancel in that case.
func (c *Connection) releaseDrainCtx() {
	_ = c.getDrainCtx()
	if c.drainCancel != nil {
		c.drainCancel()
	}
}

// Done returns a channel that is closed when the connection is no longer
// usable: the transport has been closed or Close was called. This is an
// entered-shutdown signal, not a "fully stopped" barrier — background
// worker and drain goroutines may still be running briefly after the
// channel closes. Safe to call before Start; the channel stays open until
// the connection eventually shuts down.
func (c *Connection) Done() <-chan struct{} {
	return c.done
}

// TerminalError returns the transport/read-loop error that caused the
// connection to terminate, if any.
func (c *Connection) TerminalError() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.termErr
}

func (c *Connection) closeTransport() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.transport.Close()
}

// SendRequest sends a JSON-RPC request and waits for the response.
func (c *Connection) SendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if !c.started.Load() {
		return nil, transport.ErrConnNotStarted
	}

	id := NewIntID(c.nextID.Add(1))

	msg, err := NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}

	pr := &pendingResponse{ch: make(chan responseResult, 1)}
	c.pending.Store(id.String(), pr)

	if err := c.writeMessage(ctx, msg); err != nil {
		c.pending.Delete(id.String())
		return nil, err
	}

	select {
	case res := <-pr.ch:
		return res.result, res.err
	case <-ctx.Done():
		c.pending.Delete(id.String())
		return nil, ctx.Err()
	case <-c.done:
		c.pending.Delete(id.String())
		return nil, c.connectionClosedError()
	}
}

// SendNotification sends a JSON-RPC notification (no response expected).
func (c *Connection) SendNotification(ctx context.Context, method string, params any) error {
	if !c.started.Load() {
		return transport.ErrConnNotStarted
	}

	msg, err := NewNotification(method, params)
	if err != nil {
		return err
	}
	return c.writeMessage(ctx, msg)
}

func (c *Connection) writeMessage(ctx context.Context, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	// Transport implementations are required to be safe for concurrent
	// WriteMessage calls (see Connection docstring).
	return c.transport.WriteMessage(ctx, data)
}

func (c *Connection) readLoop() {
	defer c.wg.Done()
	defer c.cancel()
	defer close(c.readDone)

	var consecutiveParseErrors int

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		data, err := c.transport.ReadMessage(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				c.setTerminalError(err)
			}
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			consecutiveParseErrors++
			if c.maxConsecutiveParseErrors > 0 && consecutiveParseErrors >= c.maxConsecutiveParseErrors {
				c.setTerminalError(fmt.Errorf("too many consecutive parse errors (%d), closing connection", consecutiveParseErrors))
				return
			}
			// Per JSON-RPC 2.0 spec, -32700 (Parse Error) is only for JSON
			// that cannot be tokenised. JSON that is syntactically valid but
			// violates JSON-RPC semantics (invalid id type, result+error both
			// present, etc.) is -32600 (Invalid Request). When the id can be
			// recovered from a lightweight envelope we echo it back; otherwise
			// we use null per spec.
			var (
				errCode = acp.ErrorCodeParseError
				errMsg  = "parse error"
				errID   *ID
			)
			if json.Valid(data) {
				errCode = acp.ErrorCodeInvalidRequest
				errMsg = "invalid request: " + err.Error()
				errID = recoverRequestID(data)
			}
			errResp := NewErrorResponse(errID, acp.NewRPCError(int(errCode), errMsg, nil))
			if writeErr := c.writeMessage(c.ctx, errResp); writeErr != nil {
				c.logf("write %s response: %v", errMsg, writeErr)
			}
			continue
		}

		if msg.JSONRPC != Version {
			consecutiveParseErrors++
			if c.maxConsecutiveParseErrors > 0 && consecutiveParseErrors >= c.maxConsecutiveParseErrors {
				c.setTerminalError(fmt.Errorf("too many consecutive parse errors (%d), closing connection", consecutiveParseErrors))
				return
			}
			errResp := NewErrorResponse(msg.ID, acp.NewRPCError(int(acp.ErrorCodeInvalidRequest), "invalid jsonrpc version", nil))
			if writeErr := c.writeMessage(c.ctx, errResp); writeErr != nil {
				c.logf("write invalid version response: %v", writeErr)
			}
			continue
		}

		// Valid message received — reset the consecutive parse error counter.
		consecutiveParseErrors = 0

		if msg.IsResponse() {
			if msg.ID == nil {
				if msg.Error != nil {
					c.reportError(msg.Method, msg.Error)
				} else {
					c.reportError(msg.Method, fmt.Errorf("response with null id"))
				}
				continue
			}
			c.handleResponse(&msg)
		} else if msg.IsRequest() {
			if !c.dispatchRequest(&msg) {
				return
			}
		} else if msg.IsNotification() {
			if !c.dispatchNotification(&msg) {
				return
			}
		} else {
			errResp := NewErrorResponse(msg.ID, acp.NewRPCError(int(acp.ErrorCodeInvalidRequest), "invalid message: not a request, notification, or response", nil))
			if writeErr := c.writeMessage(c.ctx, errResp); writeErr != nil {
				c.logf("write invalid message response: %v", writeErr)
			}
		}
	}
}

func (c *Connection) setTerminalError(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.termErr == nil {
		c.termErr = err
	}
}

func (c *Connection) handleResponse(msg *Message) {
	key := msg.ID.String()
	val, ok := c.pending.LoadAndDelete(key)
	if !ok {
		// The pending entry is deleted on timeout/ctx-cancel/close, so a late
		// response from the peer arriving here is expected under load and not
		// actionable. Log at debug level instead of surfacing through OnError.
		c.logDebugf("late or unexpected response for request %s (already timed out or cancelled)", key)
		return
	}
	pr := val.(*pendingResponse)
	if msg.Error != nil {
		pr.ch <- responseResult{err: msg.Error}
	} else {
		pr.ch <- responseResult{result: msg.Result}
	}
}

func (c *Connection) handleRequest(ctx context.Context, msg *Message, respond func(*Message)) {
	if c.requestHandler == nil {
		respond(NewErrorResponse(msg.ID, acp.ErrMethodNotFound(msg.Method)))
		return
	}

	result, err := c.requestHandler(ctx, msg.Method, msg.Params)
	if err != nil {
		rpcErr := ToResponseError(err)
		if IsHiddenInternalError(rpcErr) {
			c.logf("jsonrpc handler error on method %q: %v", msg.Method, err)
		}
		respond(NewErrorResponse(msg.ID, rpcErr))
		return
	}

	resp, err := NewResponse(msg.ID, result)
	if err != nil {
		c.logf("jsonrpc response error on method %q: marshal result: %v", msg.Method, err)
		respond(NewErrorResponse(msg.ID, acp.ErrInternalError(genericInternalErrorMessage, err)))
		return
	}
	respond(resp)
}

func (c *Connection) handleRequestSafely(ctx context.Context, msg *Message, respond func(*Message)) {
	c.activeHandlers.Add(1)
	defer c.activeHandlers.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("request handler panic: method=%q %s: %v\n%s", msg.Method, c.panicContext(msg), r, safe.TruncatedStack())
			c.reportError(msg.Method, panicErr)
			respond(NewErrorResponse(msg.ID, acp.ErrInternalError(genericInternalErrorMessage, panicErr)))
		}
	}()

	c.handleRequest(ctx, msg, respond)
}

// dispatchRequest hands a Request off to the worker pool. Non-blocking: the
// read loop never waits for a worker to become available. A worker picks it
// up and runs runRequestHandler. Returns false when the configured dispatch
// cap (WithMaxPendingDispatch) is exceeded; in that case the overflow path
// has already replied to the peer with ErrServerBusy and set the connection's
// terminal error, so the read loop should exit.
func (c *Connection) dispatchRequest(msg *Message) bool {
	if msg == nil {
		return true
	}
	if c.maxPendingDispatch > 0 && c.requestQueue.Len() >= c.maxPendingDispatch {
		c.handleDispatchOverflow(msg, "request")
		return false
	}
	msgCopy := *msg
	c.requestQueue.Push(&msgCopy)
	return true
}

// runRequestHandler is called by a worker. It runs the user handler
// synchronously and guarantees that the client receives exactly one response.
// When WithRequestTimeout is configured, a watcher goroutine proactively
// sends a timeout response if the handler exceeds the deadline — this
// protects clients from handlers that do not observe the context. The
// worker still blocks until the handler returns; its eventual response
// attempt is suppressed by an atomic responded flag.
func (c *Connection) runRequestHandler(msg *Message) {
	var reqCtx context.Context
	var cancel context.CancelFunc
	if c.requestTimeout > 0 {
		reqCtx, cancel = context.WithTimeout(c.ctx, c.requestTimeout)
	} else {
		reqCtx, cancel = context.WithCancel(c.ctx)
	}
	defer cancel()

	requestID := msg.ID
	method := msg.Method
	var responded atomic.Bool
	respond := func(resp *Message) {
		if !responded.CompareAndSwap(false, true) {
			return
		}
		if err := c.writeMessage(c.ctx, resp); err != nil {
			c.reportError(method, err)
		}
	}

	if c.requestTimeout > 0 {
		c.wg.Add(1)
		safe.Go(func() {
			defer c.wg.Done()
			select {
			case <-reqCtx.Done():
				if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
					respond(NewErrorResponse(requestID, acp.ErrRequestCanceled("request deadline exceeded on server")))
				}
			case <-c.ctx.Done():
			}
		})
	}

	c.handleRequestSafely(reqCtx, msg, respond)
}

// requestWorkerLoop consumes the intake queue. Workers run until the queue
// is drained and closed. Using context.Background() as the Pop ctx ensures
// we process every buffered message on shutdown rather than silently
// dropping them — upholding the "never drop" invariant.
func (c *Connection) requestWorkerLoop() {
	defer c.wg.Done()
	for {
		msg, ok := c.requestQueue.Pop(context.Background())
		if !ok {
			return
		}
		switch {
		case msg.IsRequest():
			c.runRequestHandler(msg)
		case msg.IsNotification():
			ctx := c.ctx
			if ctx.Err() != nil {
				ctx = c.getDrainCtx()
			}
			c.handleNotificationSafely(ctx, msg)
		}
	}
}

func (c *Connection) connectionClosedError() error {
	if err := c.TerminalError(); err != nil {
		return fmt.Errorf("%w: %v", transport.ErrConnClosed, err)
	}
	return transport.ErrConnClosed
}

func (c *Connection) handleNotification(ctx context.Context, msg *Message) {
	if c.notificationHandler == nil {
		return
	}
	if ctx == nil {
		ctx = c.ctx
	}
	err := c.notificationHandler(ctx, msg.Method, msg.Params)
	if err != nil {
		c.reportError(msg.Method, err)
	}
}

func (c *Connection) handleNotificationSafely(ctx context.Context, msg *Message) {
	c.activeHandlers.Add(1)
	defer c.activeHandlers.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			c.reportError(msg.Method, fmt.Errorf("notification handler panic: method=%q %s: %v\n%s", msg.Method, c.panicContext(msg), r, safe.TruncatedStack()))
		}
	}()

	c.handleNotification(ctx, msg)
}

// dispatchNotification hands a Notification off to a queue. Ordered methods
// go to the single-consumer ordered queue; everything else joins Requests on
// the shared worker-pool intake queue. Returns false when the configured
// dispatch cap (WithMaxPendingDispatch) is exceeded; in that case the
// overflow path has set the terminal error and the read loop should exit.
func (c *Connection) dispatchNotification(msg *Message) bool {
	if msg == nil {
		return true
	}

	msgCopy := *msg
	if c.shouldOrderNotification(msgCopy.Method) {
		if c.maxPendingDispatch > 0 && c.orderedNotificationQueue.Len() >= c.maxPendingDispatch {
			c.handleDispatchOverflow(msg, "ordered-notification")
			return false
		}
		c.orderedNotificationQueue.Push(&msgCopy)
		return true
	}
	if c.maxPendingDispatch > 0 && c.requestQueue.Len() >= c.maxPendingDispatch {
		c.handleDispatchOverflow(msg, "notification")
		return false
	}
	c.requestQueue.Push(&msgCopy)
	return true
}

// handleDispatchOverflow is invoked when the dispatch cap is exceeded for
// the given incoming frame. For Requests it attempts a best-effort
// ErrServerBusy response so the peer sees a clean failure; for Notifications
// there is no response channel, so it only logs. It always records a terminal
// error so Start/WaitErr surface the overload condition to the caller.
func (c *Connection) handleDispatchOverflow(msg *Message, kind string) {
	limit := c.maxPendingDispatch
	termErr := fmt.Errorf("dispatch queue overflow (kind=%s limit=%d)", kind, limit)
	c.setTerminalError(termErr)
	if msg != nil && msg.IsRequest() {
		errResp := NewErrorResponse(msg.ID, acp.ErrServerBusy("dispatch queue full"))
		if writeErr := c.writeMessage(c.ctx, errResp); writeErr != nil {
			c.logDebugf("write server-busy response: %v", writeErr)
		}
	}
	c.logf("dispatch queue overflow (kind=%s method=%q limit=%d): terminating connection", kind, func() string {
		if msg == nil {
			return ""
		}
		return msg.Method
	}(), limit)
}

func (c *Connection) shouldOrderNotification(method string) bool {
	if c == nil || c.orderedNotificationMatcher == nil {
		return false
	}
	return c.orderedNotificationMatcher(method)
}

// processOrderedNotifications drains the ordered-notification queue
// sequentially, ensuring matching notifications are processed in the order
// they were received. The loop exits only when Close drains the queue —
// guaranteeing buffered notifications are NOT dropped on shutdown.
func (c *Connection) processOrderedNotifications() {
	defer c.wg.Done()

	for {
		msg, ok := c.orderedNotificationQueue.Pop(context.Background())
		if !ok {
			return
		}
		ctx := c.ctx
		if ctx.Err() != nil {
			ctx = c.getDrainCtx()
		}
		c.handleNotificationSafely(ctx, msg)
	}
}

func (c *Connection) reportError(method string, err error) {
	c.logf("jsonrpc error on method %q: %v", method, err)
	if c.OnError != nil {
		// OnError is a user-supplied callback. A panic here must not abort
		// the caller (e.g. handleRequestSafely still needs to send a response
		// after reportError).
		defer func() {
			if r := recover(); r != nil {
				c.logf("jsonrpc OnError callback panic on method %q: %v\n%s", method, r, safe.TruncatedStack())
			}
		}()
		c.OnError(method, err)
	}
}

// panicContext returns a short "conn=<label> id=<id> session=<sid>"
// descriptor appended to panic logs so failures can be correlated to a
// specific connection, pending call, and (when available) ACP session.
// Fields that are not populated are omitted. The sessionId field is
// extracted best-effort from the raw JSON params.
func (c *Connection) panicContext(msg *Message) string {
	var parts []string
	if c.label != "" {
		parts = append(parts, "conn="+c.label)
	}
	if msg != nil && msg.ID != nil {
		parts = append(parts, "id="+msg.ID.String())
	}
	if msg != nil && len(msg.Params) > 0 {
		var probe struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(msg.Params, &probe); err == nil && probe.SessionID != "" {
			parts = append(parts, "session="+probe.SessionID)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}

// truncateBytes returns data as a string, truncated to at most maxLen bytes.
// If truncation occurs, an ellipsis marker is appended.
func truncateBytes(data []byte, maxLen int) string {
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "...(truncated)"
}

func (c *Connection) logf(format string, args ...any) {
	if c.label != "" {
		acplog.Error("[conn=%s] "+format, append([]any{c.label}, args...)...)
		return
	}
	acplog.Error(format, args...)
}

func (c *Connection) logDebugf(format string, args ...any) {
	if c.label != "" {
		acplog.Debug("[conn=%s] "+format, append([]any{c.label}, args...)...)
		return
	}
	acplog.Debug(format, args...)
}

// ToResponseError converts an error to an RPCError suitable for a JSON-RPC
// response. Context cancellation is mapped to request-canceled, errors that
// are already *RPCError are returned as-is, and everything else becomes an
// internal error.
func ToResponseError(err error) *acp.RPCError {
	switch {
	case errors.Is(err, context.Canceled):
		return acp.ErrRequestCanceled("request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return acp.ErrRequestCanceled("request deadline exceeded")
	}

	var rpcErr *acp.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}

	return acp.ErrInternalError(genericInternalErrorMessage, err)
}

// IsHiddenInternalError reports whether rpcErr is the opaque "internal error"
// that ToResponseError substitutes for unknown errors. Callers should log the
// original err when this is true so the underlying cause isn't lost.
func IsHiddenInternalError(rpcErr *acp.RPCError) bool {
	return rpcErr != nil && rpcErr.Message == genericInternalErrorMessage
}

// RequestSender abstracts the ability to send a JSON-RPC request and receive
// a raw JSON response. Both *Connection and agentsender.Sender satisfy this
// interface, enabling a single generic SendRequestTyped implementation.
type RequestSender interface {
	SendRequest(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// SendRequestTyped sends a request via the given sender and unmarshals the
// response into type T.
func SendRequestTyped[T any](sender RequestSender, ctx context.Context, method string, params any) (T, error) {
	var zero T
	raw, err := sender.SendRequest(ctx, method, params)
	if err != nil {
		return zero, err
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		acplog.CtxError(ctx, "unmarshal response error: %v, raw response length: %d, preview: %s", err, len(raw), truncateBytes(raw, 256))
		return zero, acp.ErrInternalError(fmt.Sprintf("unmarshal response for %s", method), err)
	}
	return result, nil
}
