package acp

import (
	acplog "github.com/eino-contrib/acp/internal/log"
)

// Logger is the interface ACP uses for diagnostic logging. Implementations
// must be safe for concurrent use. All methods are Printf-style.
type Logger = acplog.Logger

// SetLogger installs a process-wide logger used by all ACP transports,
// connections, and servers. Passing nil restores the default standard
// library-backed logger. Safe to call at any time; subsequent log calls pick
// up the new logger via an atomic load.
func SetLogger(l Logger) {
	acplog.Set(l)
}

// GetLogger returns the currently installed logger. Never nil.
func GetLogger() Logger {
	return acplog.Get()
}
