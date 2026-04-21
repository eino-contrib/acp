// Package transport defines the Transport interface for JSON-RPC message I/O.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const (
	// DefaultInboxSize is the default buffered channel size for inbound message queues.
	DefaultInboxSize = 1024
	// DefaultOutboxSize is the default buffered channel size for outbound message queues.
	DefaultOutboxSize = 1024
)

// ACP transport header names shared across HTTP and WebSocket transports.
const (
	HeaderConnectionID    = "Acp-Connection-Id"
	HeaderSessionID       = "Acp-Session-Id"
	HeaderProtocolVersion = "Acp-Protocol-Version"
)

// DefaultMaxMessageSize is the maximum size of a single JSON-RPC message (10MB).
const DefaultMaxMessageSize = 10 * 1024 * 1024

// DefaultACPEndpointPath is the default ACP endpoint path used by every ACP
// transport (Streamable HTTP, WebSocket, and the proxy). It is protocol-level
// shared state; keeping it here (instead of under transport/http) lets the WS
// and proxy layers reference it without reaching into the HTTP transport.
const DefaultACPEndpointPath = "/acp"

// MethodInitialize is the JSON-RPC method name for the ACP initialize request.
// Defined here so transport packages can reference it without importing the
// root acp package (which carries protocol-level type definitions).
const MethodInitialize = "initialize"

// Sentinel errors for stable errors.Is checks across packages.
//
// These errors are intentionally defined in the public transport package so
// callers do not need to import internal packages to perform classification.
var (
	// ErrTransportClosed indicates a transport has been closed and can no longer
	// read/write.
	ErrTransportClosed = errors.New("transport closed")
	// ErrConnNotStarted indicates a JSON-RPC connection has not been started yet.
	ErrConnNotStarted = errors.New("connection not started")
	// ErrConnClosed indicates a JSON-RPC connection has been closed.
	ErrConnClosed = errors.New("connection closed")
	// ErrNoSessionID indicates a message could not be routed because no session
	// ID was found in params or the call-site context. Agent authors get this
	// when calling SendRequest/SendNotification on a Streamable HTTP connection
	// without passing the routing key.
	ErrNoSessionID = errors.New("no session ID available for routing")
	// ErrPendingCancelled indicates a pending reverse-call was cancelled before
	// a response arrived (e.g. the connection's pending tracker was closed).
	ErrPendingCancelled = errors.New("pending request cancelled")
	// ErrSenderClosed indicates the sender was closed while a reverse-call was
	// waiting for its response.
	ErrSenderClosed = errors.New("sender closed")
	// ErrUnknownSession indicates the session ID resolved for routing does not
	// correspond to an active session on the connection.
	ErrUnknownSession = errors.New("unknown session")
)

// Transport is the interface for reading/writing JSON-RPC messages.
//
// Implementations MUST be safe for concurrent WriteMessage calls from
// multiple goroutines. The jsonrpc.Connection relies on this property to
// avoid serializing writes at the connection layer — doing so would let a
// single slow write block unrelated callers (including parse-error and
// invalid-version responses emitted from the read loop). Implementations
// handle their own backpressure and serialization (outbox channel,
// semaphore, or an internal mutex).
//
// ReadMessage is called only from a single read-loop goroutine and does not
// need to be concurrent-safe.
type Transport interface {
	ReadMessage(ctx context.Context) (json.RawMessage, error)
	WriteMessage(ctx context.Context, data json.RawMessage) error
	Close() error
}

// CloneMessage creates a stable copy of a JSON-RPC message buffer.
func CloneMessage(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	clone := make(json.RawMessage, len(data))
	copy(clone, data)
	return clone
}

// ExtractSessionID extracts the top-level sessionId field from a JSON params or result blob.
func ExtractSessionID(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.SessionID
}

// ExtractProtocolVersion extracts the top-level protocolVersion field in a header-safe form.
func ExtractProtocolVersion(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var raw struct {
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	return NormalizeProtocolVersion(raw.ProtocolVersion)
}

// NormalizeProtocolVersion converts a raw JSON protocolVersion value (which may
// be a JSON string or number) into a plain string. Returns empty for nil/null.
func NormalizeProtocolVersion(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if value == "" || value == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return s
	}
	return value
}
