package httpserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	// SSEKeepaliveInterval is the interval between SSE keepalive comments
	// on GET listener streams.
	SSEKeepaliveInterval = 30 * time.Second
)

// ParsedPost holds the parsed envelope from an ACP POST request body.
type ParsedPost struct {
	Body           json.RawMessage
	ID             *json.RawMessage
	Method         string
	Params         json.RawMessage
	IsRequest      bool // has ID and Method
	IsNotification bool // no ID, has Method
	IsResponse     bool // has ID, no Method
}

// ValidatePostHeaders checks Content-Type and Accept headers required by the
// ACP Streamable HTTP spec.
func ValidatePostHeaders(contentType, accept string) (string, int) {
	if !strings.HasPrefix(contentType, "application/json") {
		return "Content-Type must be application/json", http.StatusUnsupportedMediaType
	}
	if !acceptsJSON(accept) || !acceptsSSE(accept) {
		return "Accept must include application/json and text/event-stream", http.StatusNotAcceptable
	}
	return "", 0
}

// validatePostContentLength rejects POSTs that advertise a Content-Length
// exceeding maxSize before the body is read into memory. Missing or malformed
// Content-Length headers fall through; the post-read length check enforces
// the cap for chunked or unlabeled requests.
func validatePostContentLength(header string, maxSize int) (string, int) {
	if header == "" || maxSize <= 0 {
		return "", 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64)
	if err != nil || n < 0 {
		return "", 0
	}
	if n > int64(maxSize) {
		return "request body exceeds maximum size", http.StatusRequestEntityTooLarge
	}
	return "", 0
}

// ParsePostBody checks for batch requests and parses the JSON-RPC envelope.
func ParsePostBody(body []byte) (*ParsedPost, string, int) {
	trimmed := trimLeftWhitespace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return nil, "batch requests are not supported", http.StatusBadRequest
	}

	envelope, err := jsonrpc.ParseEnvelope(body)
	if err != nil {
		return nil, "invalid JSON", http.StatusBadRequest
	}

	return &ParsedPost{
		Body:           json.RawMessage(body),
		ID:             envelope.ID,
		Method:         envelope.Method,
		Params:         envelope.Params.Raw,
		IsRequest:      envelope.IsRequest(),
		IsNotification: envelope.IsNotification(),
		IsResponse:     envelope.IsResponse(),
	}, "", 0
}

// ValidateSessionScopeMethod validates session scope against the built-in ACP
// method set.
func ValidateSessionScopeMethod(method, sessionID string) (string, int) {
	if method != "" && acp.IsSessionScopedMethod(method) && sessionID == "" {
		return "Acp-Session-Id header required for session-scoped methods", http.StatusBadRequest
	}
	return "", 0
}

// RequestSessionID resolves the session ID that should be associated with an
// inbound HTTP request.
func RequestSessionID(method string, headerSessionID string, params json.RawMessage) string {
	if headerSessionID != "" {
		return headerSessionID
	}
	if method == acp.MethodAgentLoadSession {
		return acptransport.ExtractSessionID(params)
	}
	return ""
}

// acceptsJSON checks if an Accept header includes application/json.
func acceptsJSON(accept string) bool {
	return strings.Contains(accept, "application/json") || strings.Contains(accept, "*/*")
}

// acceptsSSE checks if an Accept header includes text/event-stream.
func acceptsSSE(accept string) bool {
	return strings.Contains(accept, "text/event-stream") || strings.Contains(accept, "*/*")
}

// trimLeftWhitespace trims leading whitespace bytes from a byte slice.
func trimLeftWhitespace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}
