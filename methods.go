package acp

import "github.com/eino-contrib/acp/internal/methodmeta"

// IsSessionScopedMethod reports whether the ACP method requires an
// Acp-Session-Id header.
func IsSessionScopedMethod(method string) bool {
	meta, ok := methodmeta.Lookup(method)
	return ok && meta.SessionHeaderRequired
}

// IsSessionCreatingMethod reports whether the ACP method establishes or rebinds
// a session and therefore needs session-aware response handling.
func IsSessionCreatingMethod(method string) bool {
	meta, ok := methodmeta.Lookup(method)
	return ok && meta.SessionCreating
}
