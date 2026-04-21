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
// Debug entries. The built-in default logger implements DebugEnabler and
// returns true (full-fidelity debug is the project default). Custom loggers
// that do not implement DebugEnabler are assumed to NOT emit Debug — this is
// the conservative choice because copying the full JSON-RPC frame (up to
// 10MB) per call is a measurable cost when Debug is actually silent.
func debugEnabled() bool {
	l := Get()
	if probe, ok := l.(DebugEnabler); ok {
		return probe.DebugEnabled()
	}
	return false
}
