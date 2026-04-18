package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
)

// Direct dispatch POST handler.

type directRequestOutcome struct {
	rpcErr   *acp.RPCError
	writeErr error
}

// handleDirectPost handles an HTTP POST on an established connection using
// direct agent dispatch (no BridgeTransport / PostStream).
func handleDirectPost(ctx HandlerContext, server ProtocolServer, conn *ProtocolConnection,
	msg *ParsedPost, sessionID string) {

	if msg.IsRequest {
		effectiveSessionID, err := RequestSessionID(msg.Method, sessionID, msg.Params)
		if err != nil {
			ctx.WriteError(http.StatusBadRequest, err.Error())
			return
		}
		handleDirectRequestPost(ctx, server, conn, msg, effectiveSessionID)
		return
	}

	if msg.IsNotification {
		effectiveSessionID, err := RequestSessionID(msg.Method, sessionID, msg.Params)
		if err != nil {
			ctx.WriteError(http.StatusBadRequest, err.Error())
			return
		}
		handleDirectNotification(ctx, conn, msg, effectiveSessionID)
		return
	}

	if msg.IsResponse {
		handleDirectClientResponse(ctx, conn, msg)
		return
	}

	ctx.WriteError(http.StatusBadRequest, "invalid JSON-RPC message")
}

// handleDirectRequestPost handles a JSON-RPC request via direct agent dispatch.
// The agent handler is called synchronously and the response is written as an
// SSE event on the same HTTP response.
func handleDirectRequestPost(ctx HandlerContext, server ProtocolServer, conn *ProtocolConnection,
	msg *ParsedPost, sessionID string) directRequestOutcome {

	outcome := directRequestOutcome{}

	// Cap concurrent direct-dispatch handlers. Acquired before BeginRequest /
	// SSE headers so overflow can surface as a clean HTTP 503 instead of a
	// half-written SSE stream, and released by the background dispatch
	// goroutine (below) so the slot stays reserved for the actual handler
	// lifetime — not just for the outer HTTP request lifetime, which may end
	// earlier if RequestTimeout fires.
	releaseSlot := conn.tryAcquireDispatch()
	if releaseSlot == nil {
		ctx.SetResponseHeader(acptransport.HeaderConnectionID, conn.ConnectionID())
		ctx.WriteError(http.StatusServiceUnavailable, "server busy: too many inflight requests")
		outcome.rpcErr = acp.ErrServerBusy("server busy: too many inflight requests")
		return outcome
	}
	// slotReleased guards the slot against double-release: the background
	// dispatch goroutine releases on exit; the outer function also releases
	// in the rare aborted-before-spawn paths. CAS-style via sync.Once.
	var slotOnce sync.Once
	release := func() { slotOnce.Do(releaseSlot) }
	spawnedDispatch := false
	defer func() {
		if !spawnedDispatch {
			release()
		}
	}()

	// BeginRequest is bracketed by the background handler goroutine (see
	// below), not by this outer function. When RequestTimeout fires, the
	// outer function returns so the HTTP response can be finalised, but the
	// user handler may still be running. Decrementing activeHandlers here
	// would let the idle reaper reclaim the connection while a background
	// handler holds references to it (Session.Send, sseWriter, …), producing
	// use-after-close failures on long-lived connections.
	conn.httpConn.BeginRequest()

	// Set up SSE response headers.
	ctx.SetResponseHeader(acptransport.HeaderConnectionID, conn.ConnectionID())
	if pv := conn.ProtocolVersion(); pv != "" {
		ctx.SetResponseHeader(acptransport.HeaderProtocolVersion, pv)
	}
	if sessionID != "" {
		ctx.SetResponseHeader(acptransport.HeaderSessionID, sessionID)
	}
	ctx.SetResponseHeader("Content-Type", "text/event-stream")
	ctx.SetResponseHeader("Cache-Control", "no-cache")
	ctx.SetResponseHeader("Connection", "keep-alive")
	ctx.SetStatusCode(http.StatusOK)
	ctx.Flush()

	sseWriter := NewSSEWriter(ctx)
	defer sseWriter.Close()
	defer ctx.CloseSSE()

	// Build request context with session ID injected.
	// Notifications from agent handlers go via GET SSE (through httpAgentSender),
	// so we no longer inject SSEWriter into the context.
	reqCtx := connspi.WithSessionID(baseHandlerContext(ctx), sessionID)

	// Apply request timeout.
	if timeout := server.RequestTimeout; timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, timeout)
		defer cancel()
	}

	reqCtx, cancel := context.WithCancel(reqCtx)
	defer cancel()

	// Monitor connection/request close.
	safe.CancelOnDone(cancel, reqCtx.Done(), ctx.Done(), conn.Done())

	// Pre-register the session only for session/load, where the client
	// provides the session ID and handler callbacks (notifications, reverse
	// requests) during load must route to that session entry.
	//
	// We intentionally do NOT EnsureSession for:
	//   - session/new: the authoritative sessionId is returned by the
	//     handler; pre-registering from a client-supplied header would let
	//     a stray header masquerade as the real session.
	//   - other session-scoped methods: the session must already exist from
	//     a prior session/new or session/load. Creating one here would let
	//     any client spawn ghost sessions that bypass the normal lifecycle.
	if sessionID != "" && msg.Method == acp.MethodAgentLoadSession {
		conn.EnsureSession(sessionID)
	}

	// Dispatch the handler in a separate goroutine so that a configured
	// RequestTimeout can proactively return a timeout response even when the
	// user handler does not observe ctx.Done(). This mirrors the WS/stdio
	// semantics in internal/jsonrpc.runRequestHandler; without it, a
	// misbehaving handler would hang the HTTP POST indefinitely and clients
	// would see different timeout behavior across transports.
	//
	// EndRequest fires from inside the goroutine so activeHandlers stays
	// positive for as long as the user handler is actually running. Pairing
	// it with BeginRequest on the outer function would release the
	// connection to the idle reaper as soon as the HTTP response is
	// finalised, even if the handler is still alive and about to touch
	// connection-owned state.
	type dispatchResult struct {
		result any
		err    error
	}
	resultCh := make(chan dispatchResult, 1)
	spawnedDispatch = true
	safe.Go(func() {
		defer release()
		defer conn.httpConn.EndRequest()
		r, e := dispatchRequestSafely(reqCtx, conn.dispatcher.Request, msg.Method, msg.Params)
		resultCh <- dispatchResult{result: r, err: e}
	})

	var (
		result     any
		handlerErr error
		aborted    bool
	)
	select {
	case res := <-resultCh:
		result = res.result
		handlerErr = res.err
	case <-reqCtx.Done():
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			handlerErr = acp.ErrRequestCanceled("request deadline exceeded on server")
			acplog.CtxWarn(reqCtx, "direct dispatch timeout on method %q after %v", msg.Method, server.RequestTimeout)
		} else {
			// Client disconnected or connection is shutting down; there is
			// no one to receive a response. Let the handler drain through
			// the buffered channel and return without writing to SSE.
			acplog.CtxDebug(reqCtx, "direct dispatch aborted on method %q: %v", msg.Method, reqCtx.Err())
			aborted = true
		}
	}

	if aborted {
		return outcome
	}

	// Convert error to RPC error.
	if handlerErr != nil {
		outcome.rpcErr = jsonrpc.ToResponseError(handlerErr)
		if jsonrpc.IsHiddenInternalError(outcome.rpcErr) {
			acplog.CtxError(reqCtx, "direct dispatch handler error on method %q: %v", msg.Method, handlerErr)
		}
	}

	// For session-creating methods where the session ID was not known before
	// dispatch (e.g. agent/newSession generates the ID), register the session
	// from the result so that a subsequent GET SSE listener from the client
	// finds it already present.
	if outcome.rpcErr == nil && sessionID == "" && acp.IsSessionCreatingMethod(msg.Method) {
		if sid := extractSessionIDFromResult(result); sid != "" {
			conn.EnsureSession(sid)
		}
	}

	if outcome.rpcErr == nil && msg.Method == acp.MethodAgentInitialize {
		if pv := extractProtocolVersionFromResult(result); pv != "" {
			conn.httpConn.setProtocolVersion(pv)
		}
	}

	// Write the final JSON-RPC response.
	if writeErr := sseWriter.WriteResponse(msg.ID, result, outcome.rpcErr); writeErr != nil {
		outcome.writeErr = writeErr
		acplog.CtxError(reqCtx, "failed to write direct dispatch response: %v", writeErr)
	}

	return outcome
}

// handleDirectInitializePost handles the initialize request via direct dispatch.
func handleDirectInitializePost(ctx HandlerContext, server ProtocolServer, conn *ProtocolConnection, msg *ParsedPost) directRequestOutcome {
	return handleDirectRequestPost(ctx, server, conn, msg, "")
}

// handleDirectNotification handles a JSON-RPC notification from the client
// via direct dispatch.
func handleDirectNotification(ctx HandlerContext, conn *ProtocolConnection, msg *ParsedPost, sessionID string) {
	// Share the inflight-dispatch budget with request handlers: notification
	// handlers can also block arbitrarily and would otherwise accumulate
	// hertz worker goroutines unbounded. Overflow surfaces as HTTP 503 since
	// notifications have no JSON-RPC response channel.
	release := conn.tryAcquireDispatch()
	if release == nil {
		ctx.WriteError(http.StatusServiceUnavailable, "server busy: too many inflight requests")
		return
	}
	defer release()

	// Bracket the notification handler with Begin/End so the idle reaper
	// does not evict the connection while a long-running notification is
	// still processing. This mirrors the request path.
	conn.httpConn.BeginRequest()
	defer conn.httpConn.EndRequest()

	// Dispatch to agent notification handler.
	if conn.dispatcher.Notification != nil {
		notifCtx, cancel := context.WithCancel(connspi.WithSessionID(baseHandlerContext(ctx), sessionID))
		safe.CancelOnDone(cancel, notifCtx.Done(), conn.Done())
		defer cancel()

		if err := dispatchNotificationSafely(notifCtx, conn.dispatcher.Notification, msg.Method, msg.Params); err != nil {
			acplog.CtxError(notifCtx, "notification %s dispatch error: %v", msg.Method, err)
		}
	}

	ctx.SetStatusCode(http.StatusAccepted)
}

// handleDirectClientResponse handles a JSON-RPC response from the client
// (in response to an agent reverse call sent via GET SSE).
func handleDirectClientResponse(ctx HandlerContext, conn *ProtocolConnection, msg *ParsedPost) {
	if conn.pendingReqs == nil {
		ctx.WriteError(http.StatusBadRequest, "no pending requests tracker")
		return
	}

	envelope, err := jsonrpc.ParseEnvelope(msg.Body)
	if err != nil {
		ctx.WriteError(http.StatusBadRequest, "invalid response envelope")
		return
	}

	idKey := jsonrpc.RawIDToKey(envelope.ID)
	if idKey == "" {
		ctx.WriteError(http.StatusBadRequest, "response missing id")
		return
	}

	resp := ReverseCallResponse{
		Result: envelope.Result.Raw,
		Error:  envelope.Error,
	}

	if !conn.pendingReqs.Deliver(idKey, resp) {
		acplog.Debug("no pending request for response %s", idKey)
	}

	ctx.SetStatusCode(http.StatusAccepted)
}

// Helpers.

// dispatchRequestSafely calls the agent dispatcher with panic recovery.
func dispatchRequestSafely(ctx context.Context, dispatch func(context.Context, string, json.RawMessage) (any, error), method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("request handler panic: %v", r)
			acplog.CtxError(ctx, "request handler panic on method %q: %v", method, r)
			acplog.CtxDebug(ctx, "request handler panic stack on method %q: %s", method, safe.TruncatedStack())
			result = nil
			err = acp.ErrInternalError("internal error panic", panicErr)
		}
	}()

	if dispatch == nil {
		return nil, acp.ErrMethodNotFound(method)
	}
	return dispatch(ctx, method, params)
}

// dispatchNotificationSafely calls the notification dispatcher with panic
// recovery. Mirrors dispatchRequestSafely so HTTP direct-dispatch gives the
// same crash containment guarantees for notifications as for requests.
func dispatchNotificationSafely(ctx context.Context, dispatch func(context.Context, string, json.RawMessage) error, method string, params json.RawMessage) (err error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("notification handler panic: method=%q %v", method, r)
			acplog.CtxError(ctx, "notification handler panic on method %q: %v", method, r)
			acplog.CtxDebug(ctx, "notification handler panic stack on method %q: %s", method, safe.TruncatedStack())
			err = acp.ErrInternalError("internal error panic method="+method, panicErr)
		}
	}()

	return dispatch(ctx, method, params)
}

func baseHandlerContext(ctx HandlerContext) context.Context {
	if ctx == nil {
		return context.Background()
	}
	base := ctx.Context()
	if base == nil {
		return context.Background()
	}
	return base
}

// protocolVersionProvider is implemented by generated response types that
// carry a ProtocolVersion field (e.g. acp.InitializeResponse), allowing
// protocol version extraction without marshal/unmarshal round-trips.
type protocolVersionProvider interface {
	GetProtocolVersion() acp.ProtocolVersion
}

// extractSessionIDFromResult extracts the sessionId from an agent handler
// result. Delegates to connspi.ExtractSessionIDFromAny which uses the
// generated SessionIDProvider interface as a fast path.
func extractSessionIDFromResult(result any) string {
	return connspi.ExtractSessionIDFromAny(result)
}

// extractProtocolVersionFromResult extracts the protocolVersion from an agent
// handler result using the generated GetProtocolVersion() accessor.
func extractProtocolVersionFromResult(result any) string {
	if p, ok := result.(protocolVersionProvider); ok {
		return strconv.FormatInt(int64(p.GetProtocolVersion()), 10)
	}
	return ""
}
