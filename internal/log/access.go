package log

import "context"

// Transport access log directions.
const (
	AccessDirectionSend = "send"
	AccessDirectionRecv = "recv"
)

// Access emits a debug-level transport access log entry for a single
// JSON-RPC frame. channel identifies the transport (e.g. "stdio",
// "ws-client", "ws-server", "http-client", "http-server"); direction is
// one of AccessDirectionSend / AccessDirectionRecv; data is the raw
// JSON payload as it appeared on (or will appear on) the wire.
//
// ctx may be nil; when non-nil its values propagate to structured
// logging backends via CtxDebug.
func Access(ctx context.Context, channel, direction string, data []byte) {
	if !enabled(LevelDebug) {
		return
	}
	if ctx != nil {
		CtxDebug(ctx, "[ACP/%s][%s] %s", channel, direction, string(data))
		return
	}
	Debug("[ACP/%s][%s] %s", channel, direction, string(data))
}
