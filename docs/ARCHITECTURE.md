# ACP Go SDK Architecture Overview

This document is for readers who are new to the project. Its goal is to help you build a quick mental model of the **layered design**, **core abstractions**, and **runtime message flow** without diving into protocol minutiae.

---

## 1. What This Project Is

`github.com/eino-contrib/acp` is the Go SDK for [Agent Client Protocol (ACP)](https://agentclientprotocol.com/). ACP is a **bidirectional RPC protocol** built on top of **JSON-RPC 2.0**, used to exchange prompts, session updates, file access calls, permission requests, and similar messages between the Client (host / IDE) and the Agent (AI service).

The core value of the SDK is that it packages the cross-cutting concerns for you: protocol framing, connection lifecycle, transports, multiplexing, and proxying. Users mainly need to implement the business-facing interfaces `acp.Agent` and `acp.Client`.

---

## 2. System Architecture

```
   ┌────────────────────────┐                                   ┌────────────────────────┐
   │      Client Side       │                                   │       Agent Side       │
   │    (IDE / Host App)    │                                   │   (AI Agent Server)    │
   │                        │                                   │                        │
   │  ┌──────────────────┐  │   Business API (user impl)        │  ┌──────────────────┐  │
   │  │    acp.Client    │  │                                   │  │    acp.Agent     │  │
   │  └─────────┬────────┘  │                                   │  └─────────▲────────┘  │
   │            │           │                                   │            │           │
   │  ┌─────────▼────────┐  │   L3: ACP endpoint                │  ┌─────────┴────────┐  │
   │  │ ClientConnection │◄─┼── dispatch / reverse RPC ─────────┼─►│ AgentConnection  │  │
   │  └─────────┬────────┘  │                                   │  └─────────▲────────┘  │
   │            │           │                                   │            │           │
   │  ┌─────────▼────────┐  │   L2: JSON-RPC 2.0 engine         │  ┌─────────┴────────┐  │
   │  │jsonrpc.Connection│  │      (envelope/queue/pending)     │  │jsonrpc.Connection│  │
   │  └─────────┬────────┘  │                                   │  └─────────▲────────┘  │
   │            │           │                                   │            │           │
   │  ┌─────────▼────────┐  │   L1: Transport (byte pipe)       │  ┌─────────┴────────┐  │
   │  │    Transport     │  │                                   │  │    Transport     │  │
   │  └─────────┬────────┘  │                                   │  └─────────▲────────┘  │
   └────────────┼───────────┘                                   └────────────┼───────────┘
                │                                                            │
                │      === pick one transport below (or via Proxy) ===       │
                │                                                            │
   ┌────────────┴────────────────────────────────────────────────────────────┴────────
   │
   │   (A) stdio         parent/child process    stdin/stdout NDJSON
   │   (B) WebSocket     full-duplex             hertz-contrib/websocket
   │   (C) Streamable HTTP (Hertz, internal/httpserver)
   │           POST    JSON-RPC request
   │           GET     opens reverse SSE channel
   │           DELETE  closes the connection
   │
   │   (D) Proxy deployment (not a transport):
   │           Client --WS--> ACPProxy --Streamer--> Upstream AgentServer --> Agent
   │           (bytes-only, no ACP parsing;
   │            southbound Streamer is user-chosen: gRPC/Kitex/WS/...)
   └──────────────────────────────────────────────────────────────────────────────────

   ┌────────────── Server-side multi-connection (server.ACPServer) ──────────────
   │
   │   Hertz /acp
   │     ├─ WS upgrade           ──► each WS conn owns its lifecycle
   │     │                            (NOT tracked in connTable)
   │     └─ HTTP POST/GET/DELETE ──► connTable, indexed by Acp-Connection-Id,
   │                                 reaped when idle
   │
   │   Each remote connection yields one AgentConnection plus one Agent
   │   produced by AgentFactory(). If the Agent implements
   │   ConnectionAwareAgent, the server injects the connection so the
   │   Agent can do reverse RPC.
   │
   │   (connTable serves Streamable HTTP only: one logical session spans
   │    many POST/GET/DELETE and must be glued by connID. WS is already
   │    long-lived and needs no extra index.)
   └─────────────────────────────────────────────────────────────────────────────
```

> The left side is the Client (IDE / host application), and the right side is the Agent (AI service). From top to bottom, the stack is: L3 ACP endpoint -> L2 JSON-RPC engine -> L1 byte transport. You can choose any of the three transports `(A)(B)(C)`, or pass traffic through `(D) Proxy`.

**One-sentence summary from top to bottom**: the user implements `acp.Agent` / `acp.Client` -> `conn.*Connection` handles ACP-level dispatch and reverse RPC -> `internal/jsonrpc` handles encoding, decoding, and request-response correlation -> a concrete `transport.Transport` implementation (`stdio`, WS, or HTTP+SSE) moves the bytes. In server deployments, `server.ACPServer` aggregates many remote connections. In gateway deployments, `proxy.ACPProxy` forwards bytes transparently.

---

## 3. Core Abstractions at a Glance

| Abstraction | Package | Responsibility |
|---|---|---|
| `acp.Agent` / `acp.Client` | root package | Business interfaces implemented by users |
| `BaseAgent` / `BaseClient` | root package | Default implementation: Request methods return `method not found: <method>`, and Notification methods return `notification handler not implemented: <method>`, so missing hooks fail loudly instead of disappearing silently |
| `conn.AgentConnection` | `conn/` | The Agent-side protocol endpoint: dispatches inbound messages and offers reverse RPC |
| `conn.ClientConnection` | `conn/` | The Client-side protocol endpoint: dispatches inbound messages and initiates prompt calls |
| `transport.Transport` | `transport/` | The byte pipe abstraction with `ReadMessage`, `WriteMessage`, and `Close` |
| `server.ACPServer` | `server/` | Multi-connection remote server: serves both HTTP (POST+SSE) and WebSocket on one `/acp` route |
| `proxy.ACPProxy` | `proxy/` | Transparent proxy: moves bytes only and does not parse ACP |
| `stream.Streamer` / `StreamerFactory` | `stream/` | Southbound transport abstraction from Proxy to the downstream AgentServer; users can implement it with gRPC, Kitex, WebSocket, etc. |

---

## 4. End-to-End Flows

### 4.1 Direct Connection Modes

All three direct transports share the same upper layers (business interface + `*Connection` + `jsonrpc.Conn`) and differ only at the L1 transport layer.

#### 4.1.1 WebSocket (full-duplex long-lived connection)

```
┌──────────────────────────────────┐                                   ┌──────────────────────────────────┐
│  Client process                  │                                   │  Agent process                   │
│                                  │                                   │                                  │
│  acp.Client (user)               │                                   │  acp.Agent (user)                │
│       ▲                          │                                   │       ▲                          │
│       │ dispatch/out             │                                   │       │ dispatch/rpc             │
│  ClientConnection                │                                   │  AgentConnection                 │
│       ▲                          │                                   │       ▲                          │
│  jsonrpc.Connection              │                                   │  jsonrpc.Connection              │
│       ▲                          │                                   │       ▲                          │
│  ws.WebSocketClientTransport     │◄═══ WS single conn, duplex ══════►│  wsserver.Transport              │
│  (transport/ws, hertz-contrib)   │           ws://host/acp           │  (internal/wsserver, server side)│
└──────────────────────────────────┘                                   └──────────────────────────────────┘

Characteristic: one TCP connection carries all bidirectional traffic, including requests, responses, reverse RPC, and notifications. No sticky routing is needed.
```

#### 4.1.2 Streamable HTTP (POST + GET (SSE) + DELETE)

```
┌────────────────────────────────┐                                   ┌────────────────────────────────┐
│  Client process                │                                   │  Agent service (ACPServer)     │
│                                │                                   │                                │
│  acp.Client                    │                                   │  acp.Agent                     │
│       ▲                        │                                   │       ▲                        │
│  ClientConnection              │                                   │  AgentConnection               │
│       ▲                        │                                   │       ▲                        │
│  jsonrpc.Connection            │                                   │  jsonrpc.Connection (pending)  │
│       ▲                        │                                   │       ▲                        │
│  client.ClientTransport        │   POST /acp  (Request)        ──► │  connTable[Acp-Connection-Id]  │
│  (transport/http/client)       │ ◄── 200 text/event-stream  ────── │   → AgentConnection            │
│  (Conn-Id in header after      │                                   │                                │
│   initialize)                  │   POST /acp  (Notification)   ──► │                                │
│                                │ ◄── 202 Accepted  ─────────────── │                                │
│                                │                                   │                                │
│                                │   GET  /acp  (open reverse SSE)──►│  per-session outbox flushed    │
│                                │ ◄── 200 text/event-stream ─────── │  into SSE: reverse Request /   │
│                                │ ◄── session/update · fs/read ──── │  Notification events           │
│                                │                                   │                                │
│                                │   DELETE /acp                  ──►│  drop from connTable + cleanup │
│                                │   Header: Acp-Connection-Id       │                                │
│                                │ ◄── 202 Accepted  ─────────────── │                                │
└────────────────────────────────┘                                   └────────────────────────────────┘

Characteristic: one logical connection is split across multiple HTTP requests. The server glues them into a single `AgentConnection` by `Acp-Connection-Id` using `connTable`, which internally owns its own `jsonrpc.Connection` plus the pending map. Server POST responses are always SSE (for Request) or `202 Accepted` (for Notification), never `application/json`. Load balancers must provide sticky routing so that the POST/GET/DELETE requests for the same connection ID all reach the same backend instance.
```

#### 4.1.3 stdio (parent and child process)

```
┌───────────────────────────┐                            ┌───────────────────────────┐
│  Client process (parent)  │        fork / exec         │  Agent process (child)    │
│                           │ ─────────────────────────► │                           │
│  acp.Client               │                            │  acp.Agent                │
│       ▲                   │                            │       ▲                   │
│  ClientConnection         │                            │  AgentConnection          │
│       ▲                   │                            │       ▲                   │
│  jsonrpc.Connection       │                            │  jsonrpc.Connection       │
│       ▲                   │                            │       ▲                   │
│  stdio.Transport          │                            │  stdio.Transport          │
│    writer = child.Stdin   │ ══ NDJSON request/notify ═►│    reader = os.Stdin      │
│    reader = child.Stdout  │ ◄══ NDJSON response/rpc ══ │    writer = os.Stdout     │
└───────────────────────────┘                            └───────────────────────────┘

Characteristic: the parent process spawns the child via `exec.Cmd`, and stdin/stdout each carry one direction. This is the typical mode for a local IDE launching an agent.
```

### 4.2 Server-Side Multi-Connection Flow (`server.ACPServer`)

```
              ┌──────────────── ACPServer (Hertz) ────────────────┐
              │   Same /acp route handles both HTTP and WS        │
              │                                                   │
              │   ┌─────────────────┐       ┌──────────────────┐  │
  WS client ─►│   │  wsConns (map)  │       │  connTable       │  │◄─ HTTP client
              │   │  id → wsConn    │       │  (idle-reaped)   │  │
              │   │  (long-lived,   │       │  connID →        │  │
              │   │   not reaped)   │       │  httpRemoteConn  │  │
              │   └────────┬────────┘       └─────────┬────────┘  │
              │            │                          │           │
              │            └────────────┬─────────────┘           │
              │                         ▼  one per connection     │
              │            conn.AgentConnection                   │
              │            (reverse-RPC handle, injected to Agent)│
              │                         ▲                         │
              │            agent := AgentFactory()                │
              │            a, ok := agent.(ConnectionAwareAgent)  │
              │            if ok { a.SetClientConnection(c) }     │
              └───────────────────────────────────────────────────┘
```

- `connTable` stores **HTTP remote connections only**. The comment on the `connTable` type in `server/conn_table.go` explicitly says `WebSocket connections are intentionally not tracked here`. Streamable HTTP needs `Acp-Connection-Id` to glue multiple POST/GET/DELETE requests into one logical connection and also needs the idle reaper to clean up orphaned sessions.
- **WebSocket connections** are managed separately by `wsConns`. WS is already long-lived and bound to the underlying TCP connection, so it does not need an idle-reaped table. The map mainly exists so `ACPServer.Close` can close them all together.
- `AgentFactory` creates one Agent instance per remote connection. If the Agent implements `ConnectionAwareAgent.SetClientConnection(*AgentConnection)`, the server injects the handle automatically so the Agent can call back into the Client, for example via `fs/read_text_file`, `session/update`, or `session/request_permission`.

### 4.3 Proxy Pass-Through Flow (`proxy.ACPProxy`)

```
┌────────┐   WS bytes   ┌─────────────────┐   Streamer   ┌────────────────────┐
│ Client │◄────────────►│  ACPProxy       │◄────────────►│ Upstream           │
│        │  /acp (WS)   │  up-pump        │  user-chosen │ AgentServer        │
│        │              │  down-pump      │  gRPC/Kitex/ │ (e.g. stdio→Agent) │
│        │              │  HeaderForwarder│  WS/...      │                    │
└────────┘              └─────────────────┘              └────────────────────┘

          Proxy is only a byte pipe and does not parse ACP; one northbound WS ↔ one Streamer ↔ one downstream session
```

- Only northbound WebSocket is supported.
- `up-pump` and `down-pump` are the two goroutines started per WS connection in `proxy/conn.go` (`upPump` and `downPump`). The former copies bytes read from the northbound WS into the downstream `Streamer`, and the latter writes downstream bytes back to the northbound WS. Neither side parses JSON-RPC.
- `HeaderForwarder` in `proxy/options.go`, installed via `proxy.WithHeaderForwarder`, projects inbound HTTP headers into a `map[string]string` and passes it downstream as `meta`, so downstream services can receive auth, tracing, or tenant headers.
- The downstream side is usually a user-built RPC service. In a typical implementation, the received bytes are fed into `stdio.Transport`, and `conn.NewAgentConnectionFromTransport` then drives the `acp.Agent`.
- `Proxy` and `ACPServer` both default to `/acp`. If both use the default path, **only one of them can be mounted in the same Hertz process**. Use `proxy.WithEndpoint` and `server.WithEndpoint` to assign different paths when they need to coexist.

---

## 5. Internal Lifecycle of a Request (WebSocket Example)

Take `Prompt(ctx, req)` called from the Client side. The full lifecycle of one forward request plus its response looks like this, in time order:

```
 ┌──────────────────────── Client process ─────────────────────┐        ┌──────────────────────── Agent process ──────────────────────┐
 │                                                             │        │                                                             │
 │  ① caller goroutine (blocks until response arrives)         │        │  ⑥ worker pool goroutine                                    │
 │     resp, err := cc.Prompt(ctx, req)                        │        │     dispatchRequest(method → handler)                       │
 │                  │                                          │        │     → agent.Prompt(ctx, req)   ◄── user handler runs here   │
 │                  ▼                                          │        │                  ▲                                          │
 │  ② ClientConnection.Prompt (generated code)                 │        │                  │ look up method table, decode params      │
 │     jsonrpc.SendRequestTyped[PromptResponse](...)           │        │                                                             │
 │                  │ encodes params, allocates request id     │        │  ⑤ readLoop dispatches classified messages:                 │
 │                  ▼                                          │        │     · Response   → O(1) wake pending (see ⑧)                │
 │  ③ jsonrpc.Connection.SendRequest                           │        │     · Request    → push intake queue → worker pool          │
 │     (sync in caller goroutine)                              │        │     · Notify(*)  → push intake queue → worker pool          │
 │     - register pending[id] = chan response                  │        │     · Notify(ordered) → push ordered queue → single worker  │
 │     - transport.WriteMessage(ctx, envelope)                 │        │                  ▲                                          │
 │     - caller blocks on pending chan until response          │        │                  │                                          │
 │                  │                                          │        │  ④ readLoop goroutine (spawned by jsonrpc.Connection.Start) │
 │                  ▼                                          │        │     for { data := transport.ReadMessage(ctx)                │
 │  ws.WebSocketClientTransport.WriteMessage                   │        │            json.Unmarshal(data, &msg) }                     │
 │  (serializes WS frame + writes socket)                      │        │                  ▲                                          │
 └──────────────────┼──────────────────────────────────────────┘        └──────────────────┼──────────────────────────────────────────┘
                    │                                                                      │
                    └──────────────── same full-duplex WS connection ──────────────────────┘
                                                 ws://host/acp
```

Response direction (`⑦ -> ⑩`):

```
 ⑦ agent.Prompt returns PromptResponse
     │
     ▼
   The worker wraps it into a JSON-RPC Response (reusing the original id) and writes it back via transport.WriteMessage
     │
     ▼
 ⑧ The Client-side readLoop receives the Response, looks up the waiting channel in the `pending` sync.Map by id, and writes the value there
     │
     ▼
 ⑨ The suspended `cc.Prompt` call from step ① wakes up and decodes PromptResponse
     │
     ▼
 ⑩ `resp, err := cc.Prompt(...)` returns to the application
```

**Reverse RPC works the same way**. When the Agent calls a **Request** such as `AgentConnection.ReadTextFile` or `RequestPermission`, the exact same `①-⑩` flow happens in the opposite direction (Agent -> Client). A **Notification** such as `AgentConnection.SessionUpdate` only goes through `①-⑥`: the sender fires and forgets, the receiver still dispatches it through `readLoop` plus the worker pool or ordered drain, but no `⑦-⑩` response path exists. Both directions reuse **the same WebSocket connection**.

**Key facts to keep in mind** (matching the `Synchronization overview` and `Dispatch model` comment blocks above the `Connection` type in `internal/jsonrpc/connection.go`):

1. **There is only one goroutine on the read side: `readLoop`.** It synchronously calls `transport.ReadMessage`, reads one message, and classifies it in place. There is **no symmetric `writeLoop` or shared outbox**. Writes happen directly in the caller's goroutine through `transport.WriteMessage`, and whether writes are internally queued is decided by the concrete transport implementation. In addition to `readLoop`, the connection also keeps a worker pool and an ordered-drain goroutine alive.
2. **`readLoop` never blocks on user handlers**:
   - `Response` messages are handled in O(1) on the spot by looking up `pending` and sending the value;
   - `Request` and unordered `Notification` messages go into an **unbounded intake queue** consumed concurrently by a fixed-size **worker pool**;
   - ordered `Notification` messages such as `session/update` go into a **separate single-consumer queue**, drained serially by one goroutine.
3. **Request-response matching relies on `pending` (`sync.Map`)**. Sending a request registers `pending[id] = chan response`, and receiving the response wakes the original caller by id. Notifications have no id and therefore no response wait path.
4. **L2 (`jsonrpc`) does not know ACP method semantics**. It only manages envelopes, ids, `pending` correlation, queues, and the worker pool. **L3 (`*Connection`)** is where ACP method constants such as `acp.MethodAgentPrompt` and their typed parameters are understood, decoded, and registered as handlers.

> On the **client side**, Streamable HTTP still plugs into the same `jsonrpc.Connection + readLoop + worker pool` stack through `transport/http/client`, just like stdio and WS. On the **server side**, however, it is different: `internal/httpserver.ProtocolConnection` handles the POST/GET/DELETE protocol semantics itself and does **not use** `jsonrpc.Connection` (see the `Create AgentConnection with the sender (no jsonrpc.Connection)` comment in `server/connection.go`). `ACPServer` uses `httpAgentSender` to deliver reverse Request / Notification messages into the per-session SSE outbox. stdio remains symmetric on both ends and still uses `jsonrpc.Connection`, with stdin/stdout as the byte channel.

About code generation and transport extensibility:
1. **Method dispatch is generated entirely from schema**. `cmd/generate` reads `schema.json` and produces:
   - root directory: `types_gen.go`, `agent_gen.go`, `client_gen.go`, `base.go` (including default `BaseAgent` / `BaseClient` implementations)
   - `conn/`: `agent_outbound_gen.go`, `client_outbound_gen.go`, `handlers_gen.go`
   - `internal/methodmeta/metadata_gen.go`

   What remains handwritten is the dispatch skeleton in `conn/dispatch.go`, the connection assembly in `conn/{agent,client}.go`, and the transport layer.
2. **`Transport` only deals with bytes**. Adding a new transport only requires implementing `transport.Transport` with `ReadMessage`, `WriteMessage`, and `Close`.

---

## 6. Directory Cheat Sheet

| Path | Purpose |
|---|---|
| `*.go` (root) | Protocol types, interfaces, `BaseAgent` / `BaseClient`, errors, extension helpers, method metadata lookup (`methods.go`), and global logger wiring (`logger.go`) |
| `conn/` | Dispatch and reverse RPC for `AgentConnection` / `ClientConnection` |
| `transport/` | `transport.go` defines the common foundation: the `Transport` interface, sentinel errors such as `ErrTransportClosed`, and shared constants like `HeaderConnectionID` and `DefaultACPEndpointPath`. Subpackages `stdio`, `ws`, and `http/client` are the concrete implementations. `stdio` is symmetric on both ends, while `ws` and `http/client` are client-side only and pair with the server-side implementations in `internal/{wsserver,httpserver}` |
| `internal/jsonrpc` | The JSON-RPC 2.0 engine: envelope, queues, and connection runtime |
| `internal/httpserver` | Server-side Streamable HTTP adapter for Hertz: POST / GET(SSE) / DELETE |
| `internal/wsserver` | Server-side WebSocket adapter for Hertz |
| `internal/connspi`, `peerstate`, `methodmeta`, `endpoint`, `safe`, `log`, `wsutil` | Internal SPI, peer protocol version and session metadata tracking, method metadata, endpoint normalization, panic recovery and goroutine helpers, logging, and WebSocket close-reason helpers |
| `server/` | `ACPServer`, which serves many connections and shares `/acp` across HTTP and WS |
| `proxy/` + `stream/` | Transparent proxying plus the southbound `Streamer` abstraction |
| `cmd/generate/` | Schema-driven generation of types, interfaces, dispatch code, and method metadata |
| `examples/{agent,client,proxy}` | Runnable examples for the three major roles |

---

## 7. Suggested Reading Paths

- **Building an Agent service**: start with `examples/agent/*`, then `server/server.go`, then `conn/agent.go`.
- **Building a Client / host application**: start with `examples/client/*`, then `conn/client.go`, then `transport/{ws,http/client,stdio}`.
- **Building a gateway / proxy**: start with `examples/proxy/*`, then `proxy/proxy.go`, then `stream/streamer.go`.
- **Adding protocol extension methods**: look at the root `extension.go` and `conn/dispatch.go`.
- **Changing the protocol and regenerating code**: inspect `cmd/generate/` and `cmd/generate/schema/schema.json`.
