package connspi

import "context"

// SessionListenerHook holds the start/stop functions for session listeners.
// The struct type lives in internal/ so external users cannot reference it.
type SessionListenerHook struct {
	Start func(ctx context.Context, sessionID string) error
	Stop  func()
}

// GetSessionListenerHook extracts session listener hooks from a transport,
// if the transport supports session listeners. Returns nil otherwise.
//
// The matching method (SessionListenerHook) is defined as a locally-scoped
// interface so that no exported interface leaks to external users. Transports
// that support session listeners just need to implement:
//
//	func (t *T) SessionListenerHook() *connspi.SessionListenerHook
func GetSessionListenerHook(transport interface{}) *SessionListenerHook {
	type provider interface {
		SessionListenerHook() *SessionListenerHook
	}
	if p, ok := transport.(provider); ok {
		return p.SessionListenerHook()
	}
	return nil
}
