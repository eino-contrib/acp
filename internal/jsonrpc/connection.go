package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
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

// WithLogger sets the logger used by the connection for diagnostic messages
// (parse errors, transport close failures, handler panics). A nil logger is
// ignored and the default is retained.
func WithLogger(logger acplog.Logger) ConnectionOption {
	return func(c *Connection) {
		if logger != nil {
			c.logger = logger
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

	// logger is the diagnostic sink for parse errors, handler panics, and
	// transport close failures. Never nil after construction.
	logger acplog.Logger
	// label is a human-readable connection identifier (e.g. a session or
	// connection ID) attached to panic and error log lines for correlation.
	label string
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
		logger:              acplog.Default(),
		requestWorkers:      defaultRequestWorkers,
		requestQueue:        newUnboundedQueue(),
		ready:               make(chan struct{}),
		done:                make(chan struct{}),
		readDone:            make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.orderedNotificationMatcher != nil {
		c.orderedNotificationQueue = newUnboundedQueue()
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
		c.ctx, c.cancel = context.WithCancel(ctx)
		c.started.Store(true)
		close(c.ready)

		c.wg.Add(1)
		safe.GoWithLogger(c.logger, c.readLoop)
		if c.orderedNotificationMatcher != nil {
			c.wg.Add(1)
			safe.GoWithLogger(c.logger, c.processOrderedNotifications)
		}
		for i := 0; i < c.requestWorkers; i++ {
			c.wg.Add(1)
			safe.GoWithLogger(c.logger, c.requestWorkerLoop)
		}
	}
	connCtx := c.ctx
	c.startMu.Unlock()

	<-connCtx.Done()
	if err := c.closeTransport(); err != nil {
		c.logf("close transport: %v", err)
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
	c.wg.Wait()
	if err := c.TerminalError(); err != nil {
		return err
	}
	return connCtx.Err()
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
		c.wg.Wait()
	}
	return err
}

// Done returns a channel that's closed when the connection is done.
// Safe to call before Start — the returned channel will close once
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
			// Per JSON-RPC 2.0 spec, send a parse error response with id: null.
			errResp := NewErrorResponse(nil, acp.NewRPCError(int(acp.ErrorCodeParseError), "parse error", nil))
			if writeErr := c.writeMessage(c.ctx, errResp); writeErr != nil {
				c.logf("write parse error response: %v", writeErr)
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
			c.dispatchRequest(&msg)
		} else if msg.IsNotification() {
			c.dispatchNotification(&msg)
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
		c.logf("late or unexpected response for request %s (already timed out or cancelled)", key)
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
		respond(NewErrorResponse(msg.ID, acp.ErrInternalError(genericInternalErrorMessage)))
		return
	}
	respond(resp)
}

func (c *Connection) handleRequestSafely(ctx context.Context, msg *Message, respond func(*Message)) {
	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("request handler panic: method=%q %s: %v\n%s", msg.Method, c.panicContext(msg), r, debug.Stack())
			c.reportError(msg.Method, panicErr)
			respond(NewErrorResponse(msg.ID, acp.ErrInternalError(genericInternalErrorMessage)))
		}
	}()

	c.handleRequest(ctx, msg, respond)
}

// dispatchRequest hands a Request off to the worker pool. Non-blocking: the
// read loop never waits for a worker to become available. A worker picks it
// up and runs runRequestHandler.
func (c *Connection) dispatchRequest(msg *Message) {
	if msg == nil {
		return
	}
	msgCopy := *msg
	c.requestQueue.Push(&msgCopy)
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
		safe.GoWithLogger(c.logger, func() {
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
				ctx = context.WithoutCancel(c.ctx)
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
		c.reportError(msg.Method, fmt.Errorf("notification handler not configured"))
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
	defer func() {
		if r := recover(); r != nil {
			c.reportError(msg.Method, fmt.Errorf("notification handler panic: method=%q %s: %v\n%s", msg.Method, c.panicContext(msg), r, debug.Stack()))
		}
	}()

	c.handleNotification(ctx, msg)
}

// dispatchNotification hands a Notification off to a queue. Ordered methods
// go to the single-consumer ordered queue; everything else joins Requests on
// the shared worker-pool intake queue. Non-blocking in both cases.
func (c *Connection) dispatchNotification(msg *Message) {
	if msg == nil {
		return
	}

	msgCopy := *msg
	if c.shouldOrderNotification(msgCopy.Method) {
		c.orderedNotificationQueue.Push(&msgCopy)
		return
	}
	c.requestQueue.Push(&msgCopy)
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
			ctx = context.WithoutCancel(c.ctx)
		}
		c.handleNotificationSafely(ctx, msg)
	}
}

func (c *Connection) reportError(method string, err error) {
	c.logf("jsonrpc error on method %q: %v", method, err)
	if c.OnError != nil {
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

func (c *Connection) logf(format string, args ...any) {
	c.logger.Error(format, args...)
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

	return acp.ErrInternalError(genericInternalErrorMessage)
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
		return zero, fmt.Errorf("unmarshal response for %s: %w", method, err)
	}
	return result, nil
}
