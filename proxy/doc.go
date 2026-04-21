// Package proxy implements a transparent ACP WebSocket proxy that forwards
// payloads between an ACP Client and a user-implemented AgentServer RPC
// service (reached via stream.Streamer).
//
// The Proxy does not parse ACP JSON-RPC messages, maintain session state, or
// interpret business semantics. It is a pure byte-level payload pump.
//
// Deployment constraint: proxy.ACPProxy and server.ACPServer are mutually
// exclusive node roles. Within a single hertz process, /acp must be served by
// exactly one of them. Both default to "/acp", so mounting both on the same
// hertz router deliberately fails at route-registration time.
package proxy
