// Package conn provides bidirectional JSON-RPC connection implementations for the
// Agent Client Protocol (ACP).
//
// AgentConnection dispatches incoming client requests to an acp.Agent
// implementation and provides outbound methods to call the connected client.
//
// ClientConnection dispatches incoming agent requests to an acp.Client
// implementation and provides outbound methods to call the connected agent.
//
// Both connection types support custom extension methods via the
// acp.ExtMethodHandler and acp.ExtNotificationHandler interfaces.
package conn
