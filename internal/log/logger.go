// Package log defines the Logger interface and the process-wide global logger
// used by ACP transports.
package log

import (
	"context"
	"log"
	"math/rand"
	"sync/atomic"

	"github.com/eino-contrib/acp/internal/connspi"
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

// prefix is prepended to every format string emitted through the package-level
// helpers below. Keeping it centralised means neither the default logger nor
// user-installed loggers need to know about the SDK marker at construction
// time — every message that flows through the SDK carries it.
const prefix = "[ACP-SDK] "

// Level is the minimum severity emitted by the package-level helpers.
// Entries below the configured threshold are dropped before any formatting or
// payload copying work is done.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelDisabled
)

// defaultLogger wraps the standard log package. Level markers are inlined here
// because the standard library logger has no notion of severity; structured
// loggers installed via Set typically attach their own level tags.
//
// Debug output is intentionally FULL-FIDELITY on the default logger: every
// JSON-RPC frame that passes through Access() is emitted at Debug level while
// the global log level remains at its default LevelDebug setting.
type defaultLogger struct{}

func (defaultLogger) Debug(format string, v ...interface{}) {
	log.Printf("[DEBUG] "+format, v...)
}
func (defaultLogger) Info(format string, v ...interface{}) {
	log.Printf("[INFO] "+format, v...)
}
func (defaultLogger) Warn(format string, v ...interface{}) {
	log.Printf("[WARN] "+format, v...)
}
func (defaultLogger) Error(format string, v ...interface{}) {
	log.Printf("[ERROR] "+format, v...)
}

func (defaultLogger) CtxDebug(ctx context.Context, format string, v ...interface{}) {
	log.Printf("[DEBUG] "+format, v...)
}
func (defaultLogger) CtxInfo(_ context.Context, format string, v ...interface{}) {
	log.Printf("[INFO] "+format, v...)
}
func (defaultLogger) CtxWarn(_ context.Context, format string, v ...interface{}) {
	log.Printf("[WARN] "+format, v...)
}
func (defaultLogger) CtxError(_ context.Context, format string, v ...interface{}) {
	log.Printf("[ERROR] "+format, v...)
}

// globalLogger holds the process-wide Logger. Callers swap it via Set; every
// log call inside the SDK reads it through Get. The stored value is always a
// *loggerHolder so atomic.Value sees a single concrete type across Stores.
var globalLogger atomic.Value // *loggerHolder
var globalLevel atomic.Int32

type loggerHolder struct{ Logger }

func init() {
	globalLogger.Store(&loggerHolder{Logger: defaultLogger{}})
	globalLevel.Store(int32(LevelDebug))
}

// Get returns the current global logger. Never nil.
func Get() Logger {
	if v := globalLogger.Load(); v != nil {
		if h, ok := v.(*loggerHolder); ok && h != nil && h.Logger != nil {
			return h.Logger
		}
	}
	return defaultLogger{}
}

// Set replaces the global logger. Passing nil restores the default standard
// library-backed logger so callers can reset to the built-in behavior.
func Set(l Logger) {
	if l == nil {
		globalLogger.Store(&loggerHolder{Logger: defaultLogger{}})
		return
	}
	globalLogger.Store(&loggerHolder{Logger: l})
}

// SetLevel updates the process-wide minimum log level used by the package-level
// helpers. Entries below this threshold are discarded before they reach the
// installed Logger.
func SetLevel(level Level) {
	globalLevel.Store(int32(normalizeLevel(level)))
}

// GetLevel returns the current process-wide minimum log level.
func GetLevel() Level {
	return Level(globalLevel.Load())
}

func normalizeLevel(level Level) Level {
	switch {
	case level < LevelDebug:
		return LevelDebug
	case level > LevelDisabled:
		return LevelDisabled
	default:
		return level
	}
}

func enabled(level Level) bool {
	return level >= GetLevel()
}

// Package-level helpers forward to the current global logger. Every entry
// carries the shared [ACP-SDK] prefix so log scrapers can trace messages back
// to the SDK regardless of which backend is installed.

func Debug(format string, v ...interface{}) {
	if !enabled(LevelDebug) {
		return
	}
	Get().Debug(prefix+format, v...)
}
func Info(format string, v ...interface{}) {
	if !enabled(LevelInfo) {
		return
	}
	Get().Info(prefix+format, v...)
}
func Warn(format string, v ...interface{}) {
	if !enabled(LevelWarn) {
		return
	}
	Get().Warn(prefix+format, v...)
}
func Error(format string, v ...interface{}) {
	if !enabled(LevelError) {
		return
	}
	Get().Error(prefix+format, v...)
}

// ctxPrefix extracts well-known context fields (ConnectionID, SessionID) and
// renders them as a bracketed prefix so every Ctx* log entry carries the
// request-scoped identifiers without each call site having to format them.
func ctxPrefix(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	cid := connspi.ConnectionIDFromContext(ctx)
	sid := connspi.SessionIDFromContext(ctx)
	switch {
	case cid != "" && sid != "":
		return "[conn=" + cid + " session=" + sid + "] "
	case cid != "":
		return "[conn=" + cid + "] "
	case sid != "":
		return "[session=" + sid + "] "
	}
	return ""
}

func CtxDebug(ctx context.Context, format string, v ...interface{}) {
	if !enabled(LevelDebug) {
		return
	}
	Get().CtxDebug(ctx, prefix+ctxPrefix(ctx)+format, v...)
}
func CtxInfo(ctx context.Context, format string, v ...interface{}) {
	if !enabled(LevelInfo) {
		return
	}
	Get().CtxInfo(ctx, prefix+ctxPrefix(ctx)+format, v...)
}
func CtxWarn(ctx context.Context, format string, v ...interface{}) {
	if !enabled(LevelWarn) {
		return
	}
	Get().CtxWarn(ctx, prefix+ctxPrefix(ctx)+format, v...)
}
func CtxError(ctx context.Context, format string, v ...interface{}) {
	if !enabled(LevelError) {
		return
	}
	Get().CtxError(ctx, prefix+ctxPrefix(ctx)+format, v...)
}

func SampledDebug(rate int, format string, v ...interface{}) {
	if rate > 0 && rand.Intn(rate) == 0 {
		Debug(format, v...)
	}
}
