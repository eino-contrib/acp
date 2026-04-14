// Package acp provides protocol type definitions, interfaces, and metadata for
// the Agent Client Protocol (ACP).
//
// ACP defines a JSON-RPC 2.0 based protocol for communication between AI agents
// and their client applications. This package provides:
//
//   - Generated types for all ACP protocol messages (types_gen.go)
//   - Agent and Client interfaces (agent_gen.go, client_gen.go)
//   - Method metadata for routing and session-scope checks (methods_gen.go, methods.go)
//   - BaseAgent and BaseClient with strict default method-not-found behavior
//   - Extension method support via ExtMethodHandler and ExtNotificationHandler
//   - RPCError for protocol-level error codes
//
// Connection implementations that wire the interfaces to a JSON-RPC transport
// are in the conn sub-package:
//
//   - conn.AgentConnection dispatches inbound requests to an Agent
//   - conn.ClientConnection dispatches inbound requests to a Client
//
// The server sub-package provides ACPServer for multi-client HTTP/WebSocket
// serving.
//
// Transport implementations are in sub-packages:
//
//   - transport/http: Streamable HTTP client and server transports
//   - transport/ws: WebSocket client and server transports
//   - transport/stdio: Standard I/O transport for subprocess communication
package acp
