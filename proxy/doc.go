// Package proxy implements the framework-neutral runtime for a transparent
// ACP WebSocket proxy. Use proxy/hertz or proxy/gin to adapt the runtime to a
// native HTTP handler. The proxy forwards payloads between an ACP Client and
// a user-implemented AgentServer RPC service reached via stream.Streamer.
//
// The Proxy does not parse ACP JSON-RPC messages, maintain session state, or
// interpret business semantics. It preserves data-frame payload bytes: text
// and binary inputs are accepted, while downstream Streamer payloads are
// emitted as text frames because Streamer does not carry a frame type.
//
// Route registration and endpoint selection belong to the host application.
// DefaultEndpoint provides the conventional "/acp" path. ACPProxy and
// server.ACPServer are normally mutually exclusive north-bound node roles; if
// both are used in one process, register them on distinct routes.
package proxy
