package acp

import (
	"encoding/json"
	"fmt"

	acplog "github.com/eino-contrib/acp/internal/log"
)

// RPCError represents a JSON-RPC 2.0 error object.
//
// This is the single error type used across the entire ACP stack — from
// Agent/Client handler return values to the JSON-RPC wire format. The
// internal/jsonrpc layer serialises and deserialises RPCError directly.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type internalErrorData struct {
	Error       string `json:"error,omitempty"`
	OriginError string `json:"originError,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s, data: %s", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// RPCErrorCode returns the numeric JSON-RPC error code.
func (e *RPCError) RPCErrorCode() int { return e.Code }

// RPCErrorMessage returns the human-readable error description.
func (e *RPCError) RPCErrorMessage() string { return e.Message }

// Additional error codes not present in the generated ErrorCode enum.
// The generated enum already provides ErrorCodeRequestCancelled (British
// spelling, matching the schema); do not duplicate it here.
const (
	ErrorCodeServerBusy ErrorCode = -32001
)

// NewRPCError creates an RPCError with an optional data payload.
// Pass nil for data to omit the data field.
//
// If data is already a json.RawMessage or []byte that contains valid JSON it
// is used as-is (avoiding a double-encode), which is the common case when
// callers want to pass through a payload they already marshalled. Invalid
// bytes are re-encoded as a JSON string so the wire payload stays valid JSON
// and an operator can still see the original bytes in the error data.
func NewRPCError(code int, message string, data any) *RPCError {
	e := &RPCError{Code: code, Message: message}
	if data == nil {
		return e
	}
	switch v := data.(type) {
	case json.RawMessage:
		if json.Valid(v) {
			e.Data = v
		} else {
			acplog.Error("NewRPCError: json.RawMessage data is not valid JSON (code=%d, len=%d); re-encoding as string to keep wire payload well-formed", code, len(v))
			if raw, err := json.Marshal(string(v)); err == nil {
				e.Data = raw
			}
		}
	case []byte:
		if json.Valid(v) {
			e.Data = json.RawMessage(v)
		} else {
			acplog.Error("NewRPCError: []byte data is not valid JSON (code=%d, len=%d); re-encoding as string to keep wire payload well-formed", code, len(v))
			if raw, err := json.Marshal(string(v)); err == nil {
				e.Data = raw
			}
		}
	default:
		raw, err := json.Marshal(data)
		if err != nil {
			// Data payload failed to marshal — log so the developer can
			// diagnose the misuse, but do not expose the marshal error
			// to the caller over the wire.
			acplog.Error("NewRPCError: failed to marshal data field (code=%d, type=%T): %v", code, data, err)
			return e
		}
		e.Data = raw
	}
	return e
}

// ErrMethodNotFound returns an RPCError with the standard JSON-RPC
// method-not-found code (-32601).
func ErrMethodNotFound(method string) *RPCError {
	return &RPCError{
		Code:    int(ErrorCodeMethodNotFound),
		Message: fmt.Sprintf("method not found: %s", method),
	}
}

// ErrInvalidParams returns an RPCError with the standard JSON-RPC
// invalid-params code (-32602).
func ErrInvalidParams(msg string) *RPCError {
	return &RPCError{
		Code:    int(ErrorCodeInvalidParams),
		Message: msg,
	}
}

// ErrInternalError returns an RPCError with the standard JSON-RPC
// internal-error code (-32603). If data is an error, it wraps it in a
// structured payload with "error" and "originError" fields for debugging.
func ErrInternalError(msg string, data any) *RPCError {
	var payload any
	switch v := data.(type) {
	case nil:
		payload = nil
	case error:
		payload = internalErrorData{Error: "internal error", OriginError: v.Error()}
	default:
		payload = data
	}
	return NewRPCError(int(ErrorCodeInternalError), msg, payload)
}

// ErrServerBusy returns an RPCError with the ACP server-busy code (-32001).
func ErrServerBusy(msg string) *RPCError {
	return &RPCError{
		Code:    int(ErrorCodeServerBusy),
		Message: msg,
	}
}

// ErrRequestCanceled returns an RPCError with the ACP request-canceled
// code (-32800). The generated constant uses the British "Cancelled"
// spelling from the schema; the helper keeps the American spelling used
// throughout the SDK.
func ErrRequestCanceled(msg string) *RPCError {
	return &RPCError{
		Code:    int(ErrorCodeRequestCancelled),
		Message: msg,
	}
}
