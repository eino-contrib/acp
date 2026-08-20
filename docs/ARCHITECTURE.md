# ACP Go SDK Architecture Overview

This document gives new contributors a concise mental model of the SDK's layers, framework boundary, runtime flows, and lifecycle.

---

## 1. What This Project Is

`github.com/eino-contrib/acp` is a Go SDK for [Agent Client Protocol (ACP)](https://agentclientprotocol.com/). ACP is a bidirectional RPC protocol built on JSON-RPC 2.0. It carries prompts, session updates, file-system calls, permission requests, and other messages between a Client (host or IDE) and an Agent (AI service).

Applications implement the business-facing `acp.Agent` and `acp.Client` interfaces. The SDK supplies protocol dispatch, request/response correlation, transports, multi-connection server state, proxying, and shutdown coordination.

---

## 2. System Architecture

```text
 Client / host                                                   Agent service
 ┌──────────────────┐                                      ┌──────────────────┐
 │    acp.Client    │                                      │    acp.Agent     │
 └────────┬─────────┘                                      └────────▲─────────┘
          │ L3: typed ACP dispatch / reverse RPC                     │
 ┌────────▼─────────┐                                      ┌────────┴─────────┐
 │ ClientConnection│                                      │ AgentConnection  │
 └────────┬─────────┘                                      └────────▲─────────┘
          │ L2: JSON-RPC envelopes, ids, pending requests            │
 ┌────────▼─────────┐                                      ┌────────┴─────────┐
 │jsonrpc.Connection│                                     │ connection sender│
 └────────┬─────────┘                                      └────────▲─────────┘
          │ L1: bytes                                                │
          ├── stdio (NDJSON) ────────────────────────────────────────┤
          ├── WebSocket ─────────────────────────────────────────────┤
          └── Streamable HTTP: POST + GET SSE + DELETE ──────────────┘

 Remote hosting boundary
 ┌────────────────────────────────────────────────────────────────────────────┐
 │ Host router owns the route                                                │
 │   Hertz -> server/hertz or proxy/hertz                                    │
 │   Gin   -> server/gin   or proxy/gin (standard net/http + Gorilla WS)     │
 │                         │                                                  │
 │        shared HTTP and WebSocket adapter contracts                        │
 │                         ▼                                                  │
 │   framework-neutral server.ACPServer or proxy.ACPProxy core               │
 └────────────────────────────────────────────────────────────────────────────┘
```

For stdio and WebSocket, both peers use `jsonrpc.Connection`. Streamable HTTP is asymmetric: the client transport feeds the normal JSON-RPC connection, while the server directly dispatches POST messages and sends reverse traffic through per-session GET SSE streams.

---

## 3. Core Abstractions at a Glance

| Abstraction | Package | Responsibility |
|---|---|---|
| `acp.Agent` / `acp.Client` | root | User-implemented business interfaces |
| `BaseAgent` / `BaseClient` | root | Defaults that report unimplemented requests and notifications |
| `conn.AgentConnection` | `conn/` | Agent-side typed dispatch and reverse calls to the Client |
| `conn.ClientConnection` | `conn/` | Client-side typed dispatch and calls to the Agent |
| `transport.Transport` | `transport/` | Byte-message contract: `ReadMessage`, `WriteMessage`, and `Close` |
| `server.ACPServer` | `server/` | Framework-neutral Streamable HTTP and WebSocket runtime |
| `proxy.ACPProxy` | `proxy/` | Framework-neutral northbound WebSocket byte proxy |
| `stream.Streamer` / `StreamerFactory` | `stream/` | User-defined southbound connection from the proxy to an AgentServer |
| Server adapters | `server/hertz`, `server/gin` | Framework-native handlers for one `ACPServer` core |
| Proxy adapters | `proxy/hertz`, `proxy/gin` | Framework-native WebSocket handlers for one `ACPProxy` core |

The server and proxy cores own protocol state, admissions, connections, limits, and lifecycle. They do not own an HTTP router or route path. WebSocket origin checks, compression, buffers, subprotocols, and native upgrader configuration stay in the adapter.

---

## 4. Framework Boundary and Route Registration

The host application registers a framework-native handler on `server.DefaultEndpoint` or `proxy.DefaultEndpoint` (both conventionally `/acp`), or on any custom route chosen by the application. Custom paths are router configuration, not core configuration. If server and proxy roles share one process, register them on distinct routes.

| Role | Hertz handler | Gin handler |
|---|---|---|
| Agent server | `server/hertz.New(core)` | `server/gin.New(core)` |
| Byte proxy | `proxy/hertz.New(core)` | `proxy/gin.New(core)` |

Typical registration is explicit:

```go
// Hertz ACPServer (Streamable HTTP + WebSocket)
srv := hertzserver.New(
    hertzserver.WithHostPorts(":8080"),
    hertzserver.WithStreamBody(true),
)
srv.NoHijackConnPool = true
srv.Any(acpserver.DefaultEndpoint, acphertz.New(core))

// Gin
router.Any(acpserver.DefaultEndpoint, acpgin.New(core))
```

- **Hertz Streamable HTTP:** prefer `WithStreamBody(true)` so the ACP adapter can enforce `server.WithMaxHTTPMessageSize` while reading chunked or unknown-length bodies. Without it, Hertz buffers first and applies its host limit (4 MiB by default) before the handler, which can preempt the SDK's 10 MiB default and 413 behavior. If streaming is unavailable, set `WithMaxRequestBodySize(...)` no lower than the SDK limit, accepting that Hertz may buffer the complete body. These are host-wide settings.
- **Hertz:** every host serving these WebSocket handlers must set `NoHijackConnPool = true`; otherwise Hertz can reclaim the hijacked connection after upgrade. This applies to both server and proxy adapters.
- **Gin:** the adapter uses Gin's standard `net/http` request/response path and Gorilla WebSocket. WebSocket support is the HTTP/1.1 upgrade flow; RFC 8441 WebSocket over HTTP/2 is not promised.
- **Both:** shared handshake checks live in `internal/wsupgrade`; native connections are wrapped behind `internal/wsconn`. Adapter options configure the Hertz or Gorilla upgrader without leaking framework types into either core.

---

## 5. End-to-End Flows

### 5.1 WebSocket

```text
ClientConnection -> jsonrpc.Connection -> transport/ws
       <============== one full-duplex WebSocket ==============>
adapter -> server WebSocket admission -> internal/wsserver -> AgentConnection
```

One TCP connection carries requests, responses, reverse RPC, and notifications. The server adapter validates and upgrades the request, then hands a wrapped `wsconn.Conn` to the core admission. No Streamable HTTP connection table or sticky routing is involved.

### 5.2 Streamable HTTP

```text
ClientTransport              Host adapter + ACPServer

POST initialize/request  ->  framework-neutral HTTP protocol dispatcher
                         <-  SSE response with final JSON-RPC response
POST notification/response -> 202 Accepted
GET session listener     ->  long-lived SSE for reverse requests/notifications
POST reverse response    ->  resolves the server-side pending reverse request
DELETE                   ->  closes the logical connection
```

One logical connection spans many HTTP requests and is keyed by `Acp-Connection-Id`. Session-scoped reverse traffic is routed by `Acp-Session-Id`. The server's HTTP path uses direct dispatch plus an `httpAgentSender`; it does not create a server-side `jsonrpc.Connection`.

POST retains compatibility with clients that omit `Accept`, but an explicit value must effectively permit both `application/json` and `text/event-stream`. Media-range precedence is honored and `q=0` means unacceptable. GET follows the stricter Active RFD rule: it must explicitly accept `text/event-stream`; otherwise the server returns `406 Not Acceptable` before connection/session lookup or SSE output.

### 5.3 stdio

```text
Parent ClientConnection -> stdin  -> child AgentConnection
Parent ClientConnection <- stdout <- child AgentConnection
```

The parent starts the Agent process and exchanges newline-delimited JSON-RPC over stdin/stdout. Both sides use the standard `jsonrpc.Connection` runtime.

### 5.4 Proxy

```text
ACP Client <-- northbound WebSocket --> ACPProxy <-- Streamer --> AgentServer
                    up pump / down pump; byte payloads only
```

The proxy does not parse ACP or own Streamable HTTP state. Each admitted northbound WebSocket gets one downstream `Streamer`. The up pump accepts text or binary data frames and forwards their payload bytes unchanged. Because `Streamer` carries payloads rather than WebSocket frame types, the down pump emits every downstream payload as a text frame. Control frames remain at the WebSocket boundary. `proxy.WithMetadataExtractor` creates the metadata passed to `StreamerFactory.NewStreamer`. `proxy.ForwardHeaders(...)` is the convenience extractor for selected authentication, trace, or tenant headers.

---

## 6. Connection Ownership and Core Lifecycle

### 6.1 Server State

```text
ACPServer
├── HTTP connTable: connection id -> httpRemoteConnection (idle-reaped)
│   └── sessions: pending buffer + at most one active GET SSE writer each
└── WebSocketAdmission registry: pending upgrades and active WS handlers
    └── internal/wsserver.Transport over wsconn.Conn
```

`AgentFactory` creates one Agent for each remote logical connection. If that Agent implements `ConnectionAwareAgent`, the core injects its `AgentConnection` so it can issue reverse requests and notifications. The HTTP table glues separate requests into a logical connection and reaps idle state. WebSocket lifecycle is tracked by admissions, including the race window before and during upgrade.

### 6.2 Proxy State

`ACPProxy` tracks every admission from acceptance through upgrade, downstream factory creation, active pumping, and cleanup. The admission registry also enforces the connection limit and prevents shutdown from reporting completion while an upgrade or factory call is still outstanding.

### 6.3 `Close` and `Shutdown`

Both `ACPServer` and `ACPProxy` expose the same lifecycle shape:

- `Close()` is idempotent. It atomically rejects new admissions, broadcasts cancellation, closes active resources, and starts asynchronous convergence. It intentionally does not wait for handlers, WebSockets, Streamers, or user factories to return.
- `Shutdown(ctx)` starts the same close path, then waits for the core registry to drain. It returns `ctx.Err()` if the caller's deadline expires first.
- Framework adapters have no independent lifecycle. Applications call core `Close`, shut down the host Hertz or `net/http` server, then call core `Shutdown(ctx)` to observe registry convergence. Host-server shutdown alone may not account for every hijacked WebSocket.

---

## 7. Streamable HTTP Deployment Constraints

Streamable HTTP depends on genuinely streaming SSE behavior. A reverse proxy or load balancer in front of it must be configured accordingly:

1. **Disable SSE response buffering.** Flush `text/event-stream` data immediately; disable proxy buffering, caching, compression transformations that coalesce chunks, and any middleware that waits for a complete response body.
2. **Allow long-lived GET responses.** The GET SSE listener is intentionally long-lived and receives periodic keepalive comments. Proxy read/idle timeouts must exceed the expected SSE lifetime or be disabled for this route.
3. **Allow long-running POST responses.** A request POST can remain open until the Agent returns its final SSE response. Align ingress, reverse-proxy, host-server, SDK request, and application deadlines so an outer timeout does not cut off valid work first.
4. **Use sticky routing.** Every POST, GET, and DELETE carrying the same `Acp-Connection-Id` must reach the same backend process, because logical connection and session state are in memory. Cookie affinity, connection-id hashing, or another deterministic policy is required when replicas are used.
5. **Preserve ACP headers and methods.** Forward `Acp-Connection-Id`, `Acp-Session-Id`, protocol-version headers, `Accept`, and `Content-Type`, and permit POST, GET, and DELETE on the selected route.

These constraints apply to both Hertz and Gin hosting because they come from the Streamable HTTP protocol, not the web framework.

---

## 8. Internal Request Dispatch

For stdio and WebSocket, `jsonrpc.Connection` has one read loop that classifies messages. Responses wake the matching pending request by JSON-RPC id. Requests and unordered notifications enter the worker queue; ordered notifications use a single-consumer queue. Writes happen from caller or worker goroutines through the transport rather than through a symmetric global write loop. Typed ACP method knowledge lives in `conn.*Connection`, above this generic JSON-RPC runtime.

For Streamable HTTP on the server, `internal/httpserver` parses and validates POST/GET/DELETE, manages connection/session state, and directly invokes the Agent dispatcher. POST requests return their final result as SSE. Server-originated messages use the session's GET SSE writer, with bounded pending/outbox buffers and send timeouts so a slow or absent listener cannot grow memory without limit.

Method types, interfaces, outbound calls, handlers, and method metadata are generated from `cmd/generate/schema/schema.json`. The connection assembly and transport runtimes remain handwritten.

---

## 9. Directory Cheat Sheet

| Path | Purpose |
|---|---|
| root `*.go` | ACP types, interfaces, base implementations, errors, extensions, and logging |
| `conn/` | Typed Client/Agent connections, dispatch, and reverse RPC |
| `transport/stdio`, `transport/ws`, `transport/http/client` | Concrete client/direct transports |
| `internal/jsonrpc` | Generic JSON-RPC connection, queues, pending correlation, and workers |
| `internal/httpserver` | Framework-neutral Streamable HTTP protocol and `HandlerContext`, plus Hertz and standard `net/http` context bridges |
| `internal/wsserver` | Server WebSocket transport over the common connection contract |
| `internal/wsconn` | Common WebSocket contract, Hertz/Gorilla wrappers, and error normalization |
| `internal/wsupgrade` | Shared RFC 6455 HTTP/1.1 handshake detection and validation |
| `server/` | Framework-neutral `ACPServer` state and lifecycle |
| `server/hertz`, `server/gin` | Host-framework server adapters |
| `proxy/` + `stream/` | Framework-neutral byte proxy and downstream Streamer abstraction |
| `proxy/hertz`, `proxy/gin` | Host-framework proxy adapters |
| `cmd/generate/` | Schema-driven code generation |
| `examples/{agent,client,proxy}` | Runnable role examples |

---

## 10. Suggested Reading Paths

- **Agent service:** `examples/agent`, `server/server.go`, one of `server/{hertz,gin}`, then `conn/agent.go`.
- **Client or host:** `examples/client`, `conn/client.go`, then `transport/{ws,http/client,stdio}`.
- **Gateway:** `examples/proxy`, `proxy/proxy.go`, one of `proxy/{hertz,gin}`, then `stream/streamer.go`.
- **Framework boundary:** `internal/httpserver`, `internal/wsupgrade`, and `internal/wsconn`.
- **Protocol changes:** `cmd/generate` and `cmd/generate/schema/schema.json`.
