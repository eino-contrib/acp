package stream

import "context"

// Streamer is the minimal bidirectional payload pipe exchanged between the
// ACP Proxy and the user-implemented AgentServer RPC service.
//
// Each ACP WebSocket connection terminated by the Proxy corresponds to
// exactly one Streamer instance. The Proxy does not inspect the payload
// bytes — they are opaque ACP JSON-RPC messages.
//
// Implementations MUST satisfy the following contract:
//
//   - Payload boundaries: a single WritePayload call on one end corresponds
//     to a single ReadPayload call on the other end. The underlying transport
//     is responsible for framing.
//   - Concurrency: WritePayload and ReadPayload MUST be safe to invoke from
//     two independent goroutines at the same time. Close may be called
//     concurrently with both.
//   - Close semantics: Close must be idempotent; only the first call has an
//     effect. After Close, any in-flight WritePayload / ReadPayload calls
//     must unblock and return an error promptly.
//   - Error propagation: the underlying transport error (network RST, auth
//     failure, peer closed, ...) must be returned verbatim — implementations
//     MUST NOT swallow errors, per the SDK's "no silent failures" guarantee.
//   - Context: implementations MUST NOT add their own implicit timeouts. The
//     ctx passed in governs only the current call; long-lived connection
//     lifetime is managed through Close — the Proxy invokes Streamer.Close
//     when the ACP connection ends.
type Streamer interface {
	// WritePayload sends one complete ACP JSON-RPC message to the peer.
	// payload is owned by the caller after the call returns; implementations
	// must copy if they need to retain bytes past the call.
	WritePayload(ctx context.Context, payload []byte) error

	// ReadPayload blocks until the next complete ACP JSON-RPC message is
	// available or the stream fails. Returned slices are owned by the caller.
	// On clean peer close, ReadPayload returns io.EOF (callers can use
	// errors.Is to detect graceful shutdown).
	ReadPayload(ctx context.Context) ([]byte, error)

	// Close tears down the stream. reason is forwarded to logs and, when the
	// underlying transport supports it, to the peer as a close cause. Close is
	// idempotent; a nil return means the first call shut the stream down, any
	// subsequent call is a no-op.
	Close(reason string) error
}

// StreamerFactory is the dialer the Proxy calls once per incoming ACP
// WebSocket connection.
//
//   - ctx governs ONLY the dial / initialization step. Implementations MUST
//     NOT retain it for connection-lifetime use (e.g. as a long-lived cancel
//     signal on the returned Streamer): the Proxy may wrap it in a handshake
//     timeout, so the ctx can fire well before the ACP connection ends.
//     Teardown is driven exclusively through Streamer.Close, which the Proxy
//     invokes when the ACP connection ends.
//   - meta carries the north-bound headers that the Proxy's HeaderForwarder
//     decided to propagate to the AgentServer (authn token, tenant id, trace
//     id, ...). Implementations attach these to the south-bound RPC request
//     however the chosen RPC framework expects (metadata, headers, ...).
//
// NewStreamer MUST block until the stream is ready for payload flow. A nil
// error guarantees the Proxy can immediately start pumping payloads.
type StreamerFactory interface {
	NewStreamer(ctx context.Context, meta map[string]string) (Streamer, error)
}
