package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"

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
		effectiveSessionID := RequestSessionID(msg.Method, sessionID, msg.Params)
		handleDirectRequestPost(ctx, server, conn, msg, effectiveSessionID)
		return
	}

	if msg.IsNotification {
		effectiveSessionID := RequestSessionID(msg.Method, sessionID, msg.Params)
		handleDirectNotification(ctx, conn, msg, effectiveSessionID, server.Logger)
		return
	}

	if msg.IsResponse {
		handleDirectClientResponse(ctx, conn, msg, server.Logger)
		return
	}

	ctx.WriteError(http.StatusBadRequest, "invalid JSON-RPC message")
}

// handleDirectRequestPost handles a JSON-RPC request via direct agent dispatch.
// The agent handler is called synchronously and the response is written as an
// SSE event on the same HTTP response.
func handleDirectRequestPost(ctx HandlerContext, server ProtocolServer, conn *ProtocolConnection,
	msg *ParsedPost, sessionID string) directRequestOutcome {

	conn.httpConn.BeginRequest()
	defer conn.httpConn.EndRequest()

	outcome := directRequestOutcome{}

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
	safe.CancelOnDone(server.Logger, cancel, reqCtx.Done(), ctx.Done(), conn.Done())

	// Pre-register session before dispatch so that reverse calls and
	// notifications from the agent handler can find the session via
	// LookupSession. Without this, handlers for session-scoped methods
	// (e.g. agent/loadSession) would fail to send messages because the
	// session is not yet registered on the connection.
	if sessionID != "" {
		conn.EnsureSession(sessionID)
	}

	// Dispatch to agent handler with panic recovery.
	result, err := dispatchRequestSafely(reqCtx, conn.dispatcher.Request, msg.Method, msg.Params, server.Logger)

	// Convert error to RPC error.
	if err != nil {
		outcome.rpcErr = jsonrpc.ToResponseError(err)
		if jsonrpc.IsHiddenInternalError(outcome.rpcErr) {
			if server.Logger != nil {
				server.Logger.CtxError(reqCtx, "direct dispatch handler error on method %q: %v", msg.Method, err)
			}
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
		acplog.OrDefault(server.Logger).Warn("failed to write direct dispatch response: %v", writeErr)
	}

	return outcome
}

// handleDirectInitializePost handles the initialize request via direct dispatch.
func handleDirectInitializePost(ctx HandlerContext, server ProtocolServer, conn *ProtocolConnection, msg *ParsedPost) directRequestOutcome {
	return handleDirectRequestPost(ctx, server, conn, msg, "")
}

// handleDirectNotification handles a JSON-RPC notification from the client
// via direct dispatch.
func handleDirectNotification(ctx HandlerContext, conn *ProtocolConnection, msg *ParsedPost, sessionID string, logger acplog.Logger) {
	// Dispatch to agent notification handler.
	if conn.dispatcher.Notification != nil {
		notifCtx, cancel := context.WithCancel(connspi.WithSessionID(baseHandlerContext(ctx), sessionID))
		safe.CancelOnDone(logger, cancel, notifCtx.Done(), conn.Done())
		defer cancel()

		if err := dispatchNotificationSafely(notifCtx, conn.dispatcher.Notification, msg.Method, msg.Params, logger); err != nil {
			acplog.OrDefault(logger).CtxWarn(notifCtx, "notification %s dispatch error: %v", msg.Method, err)
		}
	}

	ctx.SetStatusCode(http.StatusAccepted)
}

// handleDirectClientResponse handles a JSON-RPC response from the client
// (in response to an agent reverse call sent via GET SSE).
func handleDirectClientResponse(ctx HandlerContext, conn *ProtocolConnection, msg *ParsedPost, logger acplog.Logger) {
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
		acplog.OrDefault(logger).Warn("no pending request for response %s", idKey)
	}

	ctx.SetStatusCode(http.StatusAccepted)
}

// Helpers.

// dispatchRequestSafely calls the agent dispatcher with panic recovery.
func dispatchRequestSafely(ctx context.Context, dispatch func(context.Context, string, json.RawMessage) (any, error), method string, params json.RawMessage, logger acplog.Logger) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("request handler panic: %v\n%s", r, debug.Stack())
			acplog.OrDefault(logger).CtxError(ctx, "%v", panicErr)
			result = nil
			err = acp.ErrInternalError("internal error")
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
func dispatchNotificationSafely(ctx context.Context, dispatch func(context.Context, string, json.RawMessage) error, method string, params json.RawMessage, logger acplog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("notification handler panic: method=%q %v\n%s", method, r, debug.Stack())
			acplog.OrDefault(logger).CtxError(ctx, "%v", panicErr)
			err = acp.ErrInternalError("internal error")
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
