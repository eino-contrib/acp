package log

import "context"

// Transport access log directions.
const (
	AccessDirectionSend = "send"
	AccessDirectionRecv = "recv"
)

// DebugEnabler is an optional interface a Logger may implement to advertise
// whether its Debug level is live. Access uses it to short-circuit before
// allocating a string copy of the frame payload (JSON-RPC frames can be up
// to 10MB — a needless copy per frame is a measurable cost when the default
// no-op Debug is in use).
type DebugEnabler interface {
	DebugEnabled() bool
}

// Access emits a debug-level transport access log entry for a single
// JSON-RPC frame. channel identifies the transport (e.g. "stdio",
// "ws-client", "ws-server", "http-client", "http-server"); direction is
// one of AccessDirectionSend / AccessDirectionRecv; data is the raw
// JSON payload as it appeared on (or will appear on) the wire.
//
// ctx may be nil; when non-nil its values propagate to structured
// logging backends via CtxDebug.
func Access(ctx context.Context, channel, direction string, data []byte) {
	if !debugEnabled() {
		return
	}
	if ctx != nil {
		CtxDebug(ctx, "[ACP/%s][%s] %s", channel, direction, string(data))
		return
	}
	Debug("[ACP/%s][%s] %s", channel, direction, string(data))
}

// debugEnabled reports whether the current global logger will actually emit
// Debug entries. The built-in default logger has no-op Debug paths and
// returns false; custom loggers may opt in by implementing DebugEnabler.
// Loggers that do not implement the interface are assumed to NOT emit Debug
// (safe default — avoids copying full payloads when the logger's intent is
// unknown; implement DebugEnabler to opt in).
func debugEnabled() bool {
	l := Get()
	if _, isDefault := l.(defaultLogger); isDefault {
		return false
	}
	if probe, ok := l.(DebugEnabler); ok {
		return probe.DebugEnabled()
	}
	return false
}
