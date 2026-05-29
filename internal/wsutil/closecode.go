package wsutil

// Custom WebSocket close codes for ACP protocol.
// These are in the 4000–4999 range reserved for application use (RFC 6455 §7.4.2).
const (
	// CloseCodeInitializeTimeout is sent when the client fails to send
	// "initialize" within the configured timeout (server-side).
	CloseCodeInitializeTimeout = 4000

	// CloseCodeFirstFrameTimeout is sent when the client fails to send
	// the first data frame within the configured timeout (proxy-side).
	CloseCodeFirstFrameTimeout = 4001
)
