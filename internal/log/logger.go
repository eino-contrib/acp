// Package log defines the Logger interface used by ACP transports.
package log

import (
	"context"
	"log"
)

// Logger is the interface used by ACP transports for diagnostic logging.
// Implementations must be safe for concurrent use.
//
// All methods are Printf-style: the first argument is a format string and the
// trailing values are formatted via fmt.Sprintf. Ctx* variants additionally
// thread a context.Context through for structured logging backends.
type Logger interface {
	Debug(format string, v ...interface{})
	Info(format string, v ...interface{})
	Warn(format string, v ...interface{})
	Error(format string, v ...interface{})
	CtxDebug(ctx context.Context, format string, v ...interface{})
	CtxInfo(ctx context.Context, format string, v ...interface{})
	CtxWarn(ctx context.Context, format string, v ...interface{})
	CtxError(ctx context.Context, format string, v ...interface{})
}

// Default returns the default Logger implementation used by ACP.
// It writes via the standard log package using Printf semantics.
func Default() Logger { return defaultLogger{} }

// OrDefault returns l if non-nil, otherwise the default Logger. Transports
// accept a nil Logger to mean "use the default"; call OrDefault at the call
// site instead of duplicating the nil check.
func OrDefault(l Logger) Logger {
	if l != nil {
		return l
	}
	return Default()
}

// defaultLogger wraps the standard log package.
type defaultLogger struct{}

func (defaultLogger) Debug(format string, v ...interface{}) { log.Printf(format, v...) }
func (defaultLogger) Info(format string, v ...interface{})  { log.Printf(format, v...) }
func (defaultLogger) Warn(format string, v ...interface{})  { log.Printf("WARN: "+format, v...) }
func (defaultLogger) Error(format string, v ...interface{}) { log.Printf("ERROR: "+format, v...) }
func (defaultLogger) CtxDebug(_ context.Context, format string, v ...interface{}) {
	log.Printf(format, v...)
}
func (defaultLogger) CtxInfo(_ context.Context, format string, v ...interface{}) {
	log.Printf(format, v...)
}
func (defaultLogger) CtxWarn(_ context.Context, format string, v ...interface{}) {
	log.Printf("WARN: "+format, v...)
}
func (defaultLogger) CtxError(_ context.Context, format string, v ...interface{}) {
	log.Printf("ERROR: "+format, v...)
}
