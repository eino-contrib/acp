package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
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
// ACP Streamable HTTP spec. Media types are matched case-insensitively per
// RFC 9110 §8.3. Quality values and media-range precedence are honored so an
// explicitly unacceptable representation is not re-enabled by a wildcard.
func ValidatePostHeaders(contentType, accept string) (string, int) {
	if !contentTypeIsJSON(contentType) {
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

// parseErrorBodyPreviewBytes bounds the request-body preview attached to
// the debug log emitted when ParsePostBody fails. Large enough to catch
// most envelope/header typos, small enough to avoid log floods when a
// misconfigured client spams invalid bodies.
const parseErrorBodyPreviewBytes = 512

// ParsePostBody checks for batch requests and parses the JSON-RPC envelope.
//
// When parsing fails, the caller-facing error stays intentionally generic
// ("invalid JSON") to avoid leaking internals; a richer diagnostic (the
// underlying parse error plus a truncated body preview) is emitted via
// ctx debug logging so operators can investigate without turning the
// client-facing response into an information-disclosure surface.
func ParsePostBody(ctx context.Context, body []byte) (*ParsedPost, string, int) {
	trimmed := trimLeftWhitespace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		acplog.CtxDebug(ctx, "http-server: reject batch request body (%d bytes)", len(body))
		return nil, "batch requests are not supported", http.StatusBadRequest
	}

	envelope, err := jsonrpc.ParseEnvelope(body)
	if err != nil {
		acplog.CtxDebug(ctx, "http-server: parse post body failed: %v, body preview (%d bytes): %s",
			err, len(body), truncateBodyPreview(body, parseErrorBodyPreviewBytes))
		return nil, "invalid JSON", http.StatusBadRequest
	}
	if envelope.JSONRPC != jsonrpc.Version {
		acplog.CtxDebug(ctx, "http-server: reject request with jsonrpc=%q (want %q), body preview (%d bytes): %s",
			envelope.JSONRPC, jsonrpc.Version, len(body), truncateBodyPreview(body, parseErrorBodyPreviewBytes))
		return nil, "invalid jsonrpc version", http.StatusBadRequest
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

// truncateBodyPreview renders data as a string for logging, capped at maxLen
// bytes with an ellipsis marker on truncation.
func truncateBodyPreview(data []byte, maxLen int) string {
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "...(truncated)"
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
// inbound HTTP request. For any session-scoped method, both the Acp-Session-Id
// header and a top-level params.sessionId may carry the session identifier;
// when both are set they must agree, otherwise the caller would see a split
// between the routing session (header) and the handler input (params) — the
// request could execute against one session while responses and reverse calls
// flow through another.
func RequestSessionID(method string, headerSessionID string, params json.RawMessage) (string, error) {
	paramsSessionID := ""
	if method != "" && acp.IsSessionScopedMethod(method) {
		paramsSessionID = acptransport.ExtractSessionID(params)
	}
	if headerSessionID != "" && paramsSessionID != "" && headerSessionID != paramsSessionID {
		return "", fmt.Errorf("Acp-Session-Id header %q does not match params.sessionId %q", headerSessionID, paramsSessionID)
	}
	if headerSessionID != "" {
		return headerSessionID, nil
	}
	return paramsSessionID, nil
}

// contentTypeIsJSON reports whether a Content-Type header advertises
// application/json. Parameters (e.g. charset=utf-8) and case variants are
// accepted per RFC 9110.
func contentTypeIsJSON(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

// acceptsJSON checks if an Accept header includes application/json.
func acceptsJSON(accept string) bool {
	return acceptsMediaType(accept, "application", "json")
}

// acceptsSSE checks if an Accept header includes text/event-stream.
func acceptsSSE(accept string) bool {
	return acceptsMediaType(accept, "text", "event-stream")
}

// acceptsMediaType returns true if the Accept header lists a media range that
// makes type/subtype acceptable. A missing Accept defaults to "*/*" per RFC
// 9110 §12.5.1; callers whose protocol requires an explicit Accept header must
// reject the empty value before calling this helper.
//
// For a particular representation, the most specific matching media range
// determines its quality (exact type/subtype, then type wildcard, then */*).
// This matters for headers such as "*/*;q=1, text/event-stream;q=0": the
// exact range overrides the wildcard and makes SSE unacceptable. Invalid
// media ranges and malformed qvalues are ignored.
func acceptsMediaType(accept, wantType, wantSubtype string) bool {
	if accept == "" {
		return true
	}

	bestSpecificity := -1
	bestQuality := -1
	for _, entry := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(entry)
		if err != nil {
			continue
		}

		quality := 1000
		if qvalue, hasQuality := params["q"]; hasQuality {
			var ok bool
			quality, ok = parseQValue(qvalue)
			if !ok {
				continue
			}
		}

		specificity, ok := matchingMediaRangeSpecificity(mediaType, wantType, wantSubtype)
		if !ok {
			continue
		}
		if specificity > bestSpecificity || (specificity == bestSpecificity && quality > bestQuality) {
			bestSpecificity = specificity
			bestQuality = quality
		}
	}
	return bestQuality > 0
}

// matchingMediaRangeSpecificity reports whether mediaType matches the wanted
// representation and, if so, its RFC 9110 media-range precedence. The only
// valid wildcard forms are type/* and */*.
func matchingMediaRangeSpecificity(mediaType, wantType, wantSubtype string) (int, bool) {
	typeAndSubtype := strings.Split(mediaType, "/")
	if len(typeAndSubtype) != 2 || typeAndSubtype[0] == "" || typeAndSubtype[1] == "" {
		return 0, false
	}

	gotType, gotSubtype := typeAndSubtype[0], typeAndSubtype[1]
	switch {
	case gotType == "*" && gotSubtype == "*":
		return 0, true
	case strings.EqualFold(gotType, wantType) && gotSubtype == "*":
		return 1, true
	case strings.EqualFold(gotType, wantType) && strings.EqualFold(gotSubtype, wantSubtype):
		return 2, true
	default:
		return 0, false
	}
}

// parseQValue parses an explicitly supplied HTTP qvalue into thousandths. RFC
// 9110 limits qvalues to 0..1 with at most three fractional digits.
func parseQValue(value string) (int, bool) {
	if value == "0" {
		return 0, true
	}
	if value == "1" {
		return 1000, true
	}
	if len(value) < 2 || value[1] != '.' || (value[0] != '0' && value[0] != '1') {
		return 0, false
	}

	fraction := value[2:]
	if len(fraction) > 3 {
		return 0, false
	}
	quality := 0
	for _, digit := range fraction {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		quality = quality*10 + int(digit-'0')
	}
	for digits := len(fraction); digits < 3; digits++ {
		quality *= 10
	}
	if value[0] == '1' {
		if quality != 0 {
			return 0, false
		}
		return 1000, true
	}
	return quality, true
}

// trimLeftWhitespace trims leading whitespace bytes from a byte slice.
func trimLeftWhitespace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}
