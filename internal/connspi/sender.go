// Package connspi holds internal SPI types that bridge the public SDK
// packages (acp, conn, server, transport/*) with the SDK's transport-specific
// implementations. The types here intentionally live in an internal package so
// external users cannot construct or reference them.
package connspi

import (
	"context"
	"encoding/json"
)

// Sender abstracts how an agent connection pushes messages back to the client.
// For WS/stdio this is satisfied by *jsonrpc.Connection directly; for HTTP the
// server package provides an implementation backed by the per-connection SSE
// stream.
type Sender interface {
	SendNotification(ctx context.Context, method string, params any) error
	SendRequest(ctx context.Context, method string, params any) (json.RawMessage, error)
	Done() <-chan struct{}
	Close() error
}

// Dispatcher carries the inbound request/notification dispatch closures bound
// to a specific AgentConnection. The HTTP direct-dispatch path in
// internal/httpserver consumes it. The type lives in an internal package so
// external callers cannot construct or pass the value to public APIs.
type Dispatcher struct {
	Request      func(ctx context.Context, method string, params json.RawMessage) (any, error)
	Notification func(ctx context.Context, method string, params json.RawMessage) error
}

// AgentSPIKey is a capability token required by the sealed AgentConnection
// constructors and accessors in the conn package. Because this type is
// declared in an internal package and has an unexported field, no code
// outside this module can name it or construct it; the SDK's server glue
// passes it when wiring up HTTP connections. See internal/connspi for the
// same pattern applied to session listener hooks.
type AgentSPIKey struct{ _ struct{} }

// NewAgentSPIKey returns the capability token used to call the sealed
// AgentConnection SPI methods. Callable only from within this module.
func NewAgentSPIKey() AgentSPIKey { return AgentSPIKey{} }
