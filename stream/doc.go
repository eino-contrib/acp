// Package stream defines the Streamer abstraction used by the ACP Proxy to
// move ACP JSON-RPC payloads between the Proxy (north-bound WebSocket) and a
// user-implemented AgentServer RPC service (south-bound).
//
// The SDK does NOT bind a specific south-bound RPC technology: users implement
// Streamer on top of gRPC, Kitex, TTHeader, or any other bidirectional stream
// transport. The Proxy only sees this interface.
//
// On the AgentServer side, NewPipe adapts a Streamer into (io.ReadCloser,
// io.WriteCloser) so the existing stdio.NewTransport can consume it without
// changes — keeping the Agent implementation identical to examples/agent.
package stream
