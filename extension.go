package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ExtMethodHandler is an optional interface for Agent/Client to handle
// custom "_" prefixed request methods.
//
// For Streamable HTTP transports, custom messages that need to be routed to a
// specific session should include `sessionId` in params once more than one
// session or pending request may exist on the same connection.
type ExtMethodHandler interface {
	HandleExtMethod(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// ExtNotificationHandler is an optional interface for Agent/Client to handle
// custom "_" prefixed notification methods.
//
// For Streamable HTTP transports, custom notifications should include
// `sessionId` in params when routing would otherwise be ambiguous.
type ExtNotificationHandler interface {
	HandleExtNotification(ctx context.Context, method string, params json.RawMessage) error
}

// ValidateExtMethod checks that a method name follows the "_" prefix convention.
func ValidateExtMethod(method string) error {
	if !strings.HasPrefix(method, "_") {
		return fmt.Errorf("extension method must start with '_', got %q", method)
	}
	return nil
}

// IsExtMethod returns true if the method name is a custom extension method.
func IsExtMethod(method string) bool {
	return strings.HasPrefix(method, "_")
}

// CustomExtRequest is a convenience type for parsing custom extension method
// params. Extension handlers receive json.RawMessage; use this type to
// unmarshal params that follow the ACP convention of carrying _meta, sessionId,
// and an opaque data payload.
//
// Example:
//
//	func (a *MyAgent) HandleExtMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
//	    var req acp.CustomExtRequest
//	    if err := json.Unmarshal(params, &req); err != nil { ... }
//	    // use req.SessionID, req.Data
//	}
type CustomExtRequest struct {
	Meta      map[string]any  `json:"_meta,omitempty"`
	SessionID SessionID       `json:"sessionId"`
	Data      json.RawMessage `json:"data"`
}

// CustomExtNotification is an alias for CustomExtRequest, usable for
// extension notification params that follow the same convention.
type CustomExtNotification = CustomExtRequest
