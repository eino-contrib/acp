package acp

import (
	acplog "github.com/eino-contrib/acp/internal/log"
)

// Logger is the interface ACP uses for diagnostic logging. Implementations
// must be safe for concurrent use. All methods are Printf-style.
type Logger = acplog.Logger

// Level is the minimum severity emitted by ACP's package-level logging
// helpers.
type Level = acplog.Level

const (
	LevelDebug    = acplog.LevelDebug
	LevelInfo     = acplog.LevelInfo
	LevelWarn     = acplog.LevelWarn
	LevelError    = acplog.LevelError
	LevelDisabled = acplog.LevelDisabled
)

// SetLogger installs a process-wide logger used by all ACP transports,
// connections, and servers. Passing nil restores the default standard
// library-backed logger. Safe to call at any time; subsequent log calls pick
// up the new logger via an atomic load.
func SetLogger(l Logger, level Level) {
	acplog.Set(l)
	acplog.SetLevel(level)
}

// GetLogger returns the currently installed logger. Never nil.
func GetLogger() Logger {
	return acplog.Get()
}
