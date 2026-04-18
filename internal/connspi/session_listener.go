package connspi

import "context"

// SessionListenerHook holds the start/stop functions for session listeners.
// The struct type lives in internal/ so external users cannot reference it.
//
// onFailure, if non-nil, is invoked when the listener for this session
// permanently fails at runtime (e.g. the GET SSE stream disconnects and
// reconnect retries are exhausted, or reconnect is disabled). Startup
// failures are reported through Start's return value instead and must not
// also trigger onFailure, so the caller sees each failure exactly once.
// onFailure is not invoked when the listener is stopped explicitly via
// Stop or when the transport is closing.
type SessionListenerHook struct {
	Start func(ctx context.Context, sessionID string, onFailure func(error)) error
	Stop  func()
}

// SessionListenerHookKey is the argument type transports must accept on
// their SessionListenerHook accessor method. Because this type is declared
// in an internal package, no code outside the module can name it, and
// because it has an unexported field it cannot be constructed from
// outside this package. The combination seals the accessor: external code
// cannot even call the method, let alone implement it.
type SessionListenerHookKey struct{ _ struct{} }

// GetSessionListenerHook extracts session listener hooks from a transport,
// if the transport implements the session-listener provider contract.
// Returns nil otherwise.
//
// Transports that support session listeners implement:
//
//	func (t *T) SessionListenerHook(connspi.SessionListenerHookKey) *connspi.SessionListenerHook
//
// The SessionListenerHookKey parameter acts as a capability token: it can
// only be constructed inside this package, so only this package can invoke
// the method on behalf of the rest of the module.
func GetSessionListenerHook(transport interface{}) *SessionListenerHook {
	type provider interface {
		SessionListenerHook(SessionListenerHookKey) *SessionListenerHook
	}
	if p, ok := transport.(provider); ok {
		return p.SessionListenerHook(SessionListenerHookKey{})
	}
	return nil
}
