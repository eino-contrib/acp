// Package server implements the framework-neutral ACP server runtime.
//
// An ACPServer owns protocol state and connection lifecycle but does not own
// an HTTP route or WebSocket upgrader. Applications adapt it with
// server/hertz or server/gin and register the returned handler on their host
// router. DefaultEndpoint provides the conventional "/acp" route.
//
// Close starts asynchronous resource convergence. Shutdown additionally waits
// for admitted Streamable HTTP and WebSocket work to drain, bounded by the
// caller's context.
package server
