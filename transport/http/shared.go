// Package http exposes the public constants shared by the ACP Streamable HTTP
// transport (client and server).
//
// For the full client implementation see transport/http/client; for the server
// side use server.ACPServer (Hertz-based) which owns connection lifecycle.
package http

// DefaultACPEndpointPath is the default ACP Streamable HTTP endpoint path
// (single endpoint handling POST, GET, and DELETE per the spec).
const DefaultACPEndpointPath = "/acp"
