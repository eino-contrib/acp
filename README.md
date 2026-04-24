# ACP Go SDK Integration Guide

[中文版本](./README.zh-CN.md)

## 1. Background

`github.com/eino-contrib/acp` is the Go SDK for [Agent Client Protocol](https://agentclientprotocol.com/), providing:

- **Bidirectional RPC abstraction**: `conn.ClientConnection` / `conn.AgentConnection` hide the JSON-RPC 2.0 details;
- **Three transport layers**: `stdio` (subprocess), Streamable HTTP (POST + SSE), and WebSocket; the HTTP/WS server side is implemented on top of [CloudWeGo Hertz](https://github.com/cloudwego/hertz);
- **Remote server**: `server.ACPServer` serves both HTTP and WebSocket upgrade on a single route;
- **Transparent proxy**: `proxy.ACPProxy` transparently forwards northbound WS traffic to downstream services (your custom RPC-based AgentServer implementation);
- **Protocol extensibility**: supports custom Request / Notification methods prefixed with `_` ([ACP Extensibility](https://agentclientprotocol.com/protocol/extensibility#custom-requests)).

## 2. Installation

```bash
go get github.com/eino-contrib/acp@latest
```

Requirements:

- Go **1.24+**
- Module path: `github.com/eino-contrib/acp`

## 3. Core Concepts

### 3.1 Roles

| Role | Type | Responsibility |
| --- | --- | --- |
| **Agent** | `acp.Agent` interface | Receives client prompts, manages sessions, and calls back into the client (read files, request permission, terminal access, etc.) |
| **Client** | `acp.Client` interface | Sends prompts and receives streaming notifications such as `session/update` |
| **Proxy** | `proxy.ACPProxy` + your `stream.StreamerFactory` implementation | Accepts northbound client WebSocket traffic and transparently forwards bytes to a downstream AgentServer without parsing ACP; provides only the WS northbound entry, and is responsible for auth header forwarding, keepalive, concurrency, and timeout control |

`BaseAgent` / `BaseClient` do **not** silently succeed for unimplemented methods. They return `method not found` (`-32601`) or `notification handler not implemented: <method>` by default, which means they **actively fail** until you override the methods your application needs.

Agent and Client are **protocol endpoints** (parse JSON-RPC and handle method calls), while Proxy is a **pass-through node** (moves bytes only and does not inspect the protocol). Their responsibilities do not overlap.

### 3.2 Connections

- `conn.NewClientConnection(client, transport, opts...)`: client-side connection.
- `conn.NewAgentConnectionFromTransport(agent, transport, opts...)`: agent-side connection for transports with a read loop (`stdio` / WebSocket).
- HTTP servers do not need to call `NewAgentConnectionFromTransport`: `server.ACPServer` automatically builds an `AgentConnection` for each connection and injects it into the agent via the `ConnectionAwareAgent` interface (`SetClientConnection(*conn.AgentConnection)`). If your agent implements that interface, it can get the current connection and use it for reverse calls (see [4.1.1 Agent (Server)](#411-agentserver)).

### 3.3 Import Alias Convention

All code examples below use the following import aliases, so later snippets omit the import block:

```go
import (
	acp           "github.com/eino-contrib/acp"
	acpconn       "github.com/eino-contrib/acp/conn"
	acpserver     "github.com/eino-contrib/acp/server"
	acpproxy      "github.com/eino-contrib/acp/proxy"
	acpstream     "github.com/eino-contrib/acp/stream"
	stdio         "github.com/eino-contrib/acp/transport/stdio"
	acphttpclient "github.com/eino-contrib/acp/transport/http/client"
	acpws         "github.com/eino-contrib/acp/transport/ws"
)
```

Where tables mention `conn.WithXxx` or `server.WithXxx`, the actual code in examples uses `acpconn.WithXxx` or `acpserver.WithXxx`. Hertz symbols such as `websocket` and `app` are used directly from their original packages (`github.com/hertz-contrib/websocket` and `github.com/cloudwego/hertz/pkg/app`).

## 4. Quick Start

Below are the four most common setups:

1. **WebSocket mode**: a remote `ACPServer` exposes your agent, and the client connects via WebSocket.
2. **Streamable HTTP mode**: a remote `ACPServer` serves HTTP (POST + SSE), and the client connects over HTTP and receives reverse messages through SSE.
3. **stdio subprocess mode**: the client spawns the agent as a child process and communicates through stdin/stdout.
4. **Proxy mode**: the proxy node accepts northbound client WebSocket traffic and transparently forwards the byte stream to a downstream AgentServer through your `stream.StreamerFactory`.

Build once first to get `bin/agent`, `bin/client`, and `bin/proxy`:

```bash
make build
```

### 4.1 WebSocket Mode

```
┌──────────────────────┐                              ┌──────────────────────────┐
│       Client         │                              │   ACPServer (Hertz)      │
│  ┌────────────────┐  │   ws://host:port/acp         │  ┌────────────────────┐  │
│  │ acp.Client     │  │  ◄────── Upgrade ─────►      │  │ acp.Agent          │  │
│  │ BaseClient     │  │                              │  │ BaseAgent          │  │
│  └────────────────┘  │  ═══ full-duplex frames ═══► │  └────────────────────┘  │
│         ▲            │                              │            ▲             │
│         │ read loop  │  ◄══ session/update ═══════  │            │ reverse RPC │
│         │            │  ◄══ fs/read · permission ═  │            │             │
│  ┌──────┴─────────┐  │                              │  ┌─────────┴──────────┐  │
│  │ ws.Transport   │  │  ══ initialize/prompt ═════► │  │ AgentConnection    │  │
│  └────────────────┘  │                              │  └────────────────────┘  │
└──────────────────────┘                              └──────────────────────────┘
```

<a id="411-agentserver"></a>
#### 4.1.1 Agent (Server)

See the repository examples for a complete demo:

- Agent implementation: [`examples/agent/agent.go`](./examples/agent/agent.go)
- Hertz mount and entrypoint: [`examples/agent/main.go`](./examples/agent/main.go)

> ⚠️ **Hertz WebSocket requires `srv.NoHijackConnPool = true`**, otherwise Hertz will reclaim the upgraded connection and the WS connection will be closed immediately.

#### 4.1.2 Client

See the repository examples for a complete demo:

- Client implementation: [`examples/client/client.go`](./examples/client/client.go)
- WebSocket entrypoint: [`examples/client/main.go`](./examples/client/main.go) (`-transport=ws`)

#### 4.1.3 Run the Demo

```bash
# Terminal A: start the Agent (HTTP + WS share the same /acp route, listening on :18080)
./bin/agent -transport=http -listen=:18080

# Terminal B: connect the Client via WebSocket
./bin/client -transport=ws ws://127.0.0.1:18080

# Run both in one shot (start agent + client sequentially in one process and clean up automatically)
make run-ws
# Custom port: make run-ws AGENT_ADDR=:9090
```

### 4.2 Streamable HTTP Mode

```
┌──────────────────────┐                                    ┌──────────────────────────┐
│       Client         │                                    │   ACPServer (Hertz)      │
│  ┌────────────────┐  │                                    │  ┌────────────────────┐  │
│  │ acp.Client     │  │  ─── POST /acp  (JSON-RPC req) ──► │  │ acp.Agent          │  │
│  │ BaseClient     │  │  ◄── 200 JSON / SSE response ────  │  │ BaseAgent          │  │
│  └────────────────┘  │                                    │  └────────────────────┘  │
│         ▲            │  ─── GET  /acp  (SSE listener) ──► │            ▲             │
│         │ SSE recv   │  ◄═══ session/update  ═════════    │            │ reverse RPC │
│         │            │  ◄═══ fs/read · permission ═══     │            │             │
│  ┌──────┴─────────┐  │                                    │  ┌─────────┴──────────┐  │
│  │ http.Client    │  │  ─── POST /acp  (reverse resp) ──► │  │ AgentConnection    │  │
│  │ (cookie jar)   │  │  ─── DELETE /acp (close) ───────►  │  │  + pending queue   │  │
│  └────────────────┘  │    headers: Acp-Connection-Id      │  └────────────────────┘  │
│                      │             Acp-Session-Id         │                          │
└──────────────────────┘                                    └──────────────────────────┘
```

> ⚠️ **Important: sticky routing is required**
>
> Streamable HTTP uses both:
> - `POST /acp` to send requests (and responses)
> - `GET /acp` to establish the SSE reverse channel (used to receive reverse Request/Notification messages from Agent to Client)
>
> If you deploy behind a load balancer or reverse proxy, you must ensure that `POST /acp` and `GET /acp` for the same ACP connection are routed to the **same** backend instance, for example through cookie-based sticky routing, header hashing, or consistent routing by `Acp-Connection-Id`. Otherwise connection state will diverge, and reverse messages may be lost or requests may fail.

#### 4.2.1 Agent (Server)

`ACPServer` supports both WebSocket and Streamable HTTP on the same route (default `/acp`), so the server-side implementation does not need to change. You can directly reuse the code from [4.1.1 Agent (Server)](#411-agentserver).

#### 4.2.2 Client

See the repository examples for a complete demo:

- Client implementation: [`examples/client/client.go`](./examples/client/client.go)
- HTTP + SSE entrypoint: [`examples/client/main.go`](./examples/client/main.go) (`-transport=http`)

#### 4.2.3 Run the Demo

```bash
# Terminal A: start the Agent in HTTP mode (same binary as WS mode)
./bin/agent -transport=http -listen=:18080

# Terminal B: use HTTP + SSE from the Client
./bin/client -transport=http http://127.0.0.1:18080

# Run both in one shot
make run-http
```

### 4.3 stdio Subprocess Mode

```
┌──────────────────────────┐                        ┌──────────────────────────┐
│  Client (Parent Process) │                        │  Agent (Child Process)   │
│  ┌────────────────────┐  │                        │  ┌────────────────────┐  │
│  │ acp.Client         │  │                        │  │ acp.Agent          │  │
│  │ BaseClient         │  │                        │  │ BaseAgent          │  │
│  └────────────────────┘  │                        │  └────────────────────┘  │
│           ▲              │                        │            ▲             │
│  ┌────────┴───────────┐  │   stdin  (NDJSON req)  │  ┌─────────┴──────────┐  │
│  │ stdio.Transport    │  │  ═══════════════════►  │  │ os.Stdin           │  │
│  │  reader = stdout   │  │                        │  │ os.Stdout          │  │
│  │  writer = stdin    │  │   stdout (NDJSON resp) │  │ stdio.Transport    │  │
│  └────────────────────┘  │  ◄═══════════════════  │  │  reader = Stdin    │  │
│           │              │   + session/update     │  │  writer = Stdout   │  │
│           │              │   + reverse RPC        │  └────────────────────┘  │
│  exec.Cmd / StdinPipe    │                        │                          │
└──────────────────────────┘                        └──────────────────────────┘
		parent process spawn ────────── fork/exec ──────────► child process
```

#### 4.3.1 Client (Parent Spawns Child)

The client spawns a child process and communicates through its stdin/stdout. You can reuse the `Client` implementation from the WebSocket example above:

See the repository examples for a complete demo:

- Client implementation: [`examples/client/client.go`](./examples/client/client.go)
- Spawn entrypoint: [`examples/client/main.go`](./examples/client/main.go) (`-transport=spawn`)

#### 4.3.2 Agent (Inside the Child Process)

On the agent side (inside the child process, where `agent` is your agent instance, for example `&Agent{}`):

See the repository examples for a complete demo:

- Agent implementation: [`examples/agent/agent.go`](./examples/agent/agent.go)
- stdio entrypoint: [`examples/agent/main.go`](./examples/agent/main.go) (`-transport=stdio`)

#### 4.3.3 Run the Demo

```bash
# The Client spawns the Agent subprocess directly and communicates over stdin/stdout
./bin/client -transport=spawn ./bin/agent

# Run in one shot
make run-stdio
```

### 4.4 Proxy Mode

```
┌────────────────────┐            ┌──────────────────────────┐            ┌──────────────────────────┐
│      Client        │            │    Proxy (ACPProxy)      │            │   Upstream AgentServer   │
│                    │            │                          │            │                          │
│  ┌──────────────┐  │            │  ┌────────────────────┐  │            │  ┌────────────────────┐  │
│  │ acp.Client   │  │            │  │ Hertz /acp WS      │  │            │  │ user RPC           │  │
│  │ BaseClient   │  │            │  │                    │  │            │  │ (gRPC / Kitex /    │  │
│  └──────────────┘  │            │  │  up-pump           │  │            │  │  custom WS / ...)  │  │
│         ▲          │            │  │  down-pump         │  │            │  └────────────────────┘  │
│         │          │  WS bytes  │  └────────────────────┘  │  Streamer  │            │             │
│         │          ├───────────►│                          ├───────────►│            ▼             │
│         │          │◄───────────┤  HeaderForwarder         │◄───────────┤  ┌────────────────────┐  │
│  ┌──────┴───────┐  │            │  WS keepalive            │            │  │ AgentConnection    │  │
│  │ ws.Transport │  │            │  Max-conn cap            │            │  │ acp.Agent          │  │
│  └──────────────┘  │            │                          │            │  │ BaseAgent          │  │
│                    │            │                          │            │  └────────────────────┘  │
└────────────────────┘            └──────────────────────────┘            └──────────────────────────┘

					  Proxy moves bytes only and does not parse ACP
					  one client WS ↔ one Streamer ↔ one downstream session
```

See the repository examples for a complete demo:

- Proxy entrypoint: [`examples/proxy/main.go`](./examples/proxy/main.go)
- Proxy runtime logic: [`examples/proxy/proxy_runner.go`](./examples/proxy/proxy_runner.go)
- Upstream AgentServer: [`examples/proxy/agent_server.go`](./examples/proxy/agent_server.go)
- Example `StreamerFactory`: [`examples/proxy/factory.go`](./examples/proxy/factory.go)
- Example `Streamer`: [`examples/proxy/ws_streamer.go`](./examples/proxy/ws_streamer.go)
- Example Agent: [`examples/proxy/echo_agent.go`](./examples/proxy/echo_agent.go)

> ⚠️ Constraints:
> - Proxy **supports only WebSocket** as the northbound entrypoint (no Streamable HTTP).
> - `ACPServer` and `ACPProxy` both default to `/acp`; if you mount them on the same Hertz router, you must explicitly configure different endpoints.
> - You still need `srv.NoHijackConnPool = true`, otherwise Hertz will reclaim upgraded WebSocket connections and disconnect them.

The Proxy is intentionally "bytes only, protocol blind": it forwards WS frames from the external client to the downstream service (typically your own AgentServer implementation). The downstream service then feeds those bytes into ACP's stdio transport, and your agent is ultimately driven by `acpconn.NewAgentConnectionFromTransport(...)`.

#### 4.4.1 Downstream AgentServer (Upstream from the Proxy's Perspective)

Minimal runnable example built into the repository: start a WS upstream at `/acp-upstream` for the Proxy to dial:

```bash
./bin/proxy -role=agent-server -listen=:9090
```

#### 4.4.2 Proxy Node (Northbound `/acp` -> Southbound Upstream)

Start the Proxy (northbound path fixed at `/acp`) and forward each inbound client WS connection to `ws://127.0.0.1:9090/acp-upstream`:

```bash
./bin/proxy -role=proxy -listen=:8080 -upstream=ws://127.0.0.1:9090/acp-upstream
```

#### 4.4.3 Client (Connecting to the Proxy)

The client still uses WebSocket mode; only the target address changes to the Proxy (the default endpoint path remains `/acp`):

The complete demo can be reused directly:

- Client implementation: [`examples/client/client.go`](./examples/client/client.go)
- WebSocket entrypoint: [`examples/client/main.go`](./examples/client/main.go) (`-transport=ws`, target changed to the Proxy)

You can also run the full local chain with one command (start upstream + proxy together):

```bash
./bin/proxy -role=all
```

#### 4.4.4 Run the Demo

```bash
# Option 1: start upstream AgentServer and Proxy separately, then start the Client
./bin/proxy -role=agent-server -listen=:9090                                      # Terminal A
./bin/proxy -role=proxy -listen=:8080 -upstream=ws://127.0.0.1:9090/acp-upstream  # Terminal B
./bin/client -transport=ws ws://127.0.0.1:8080                                    # Terminal C

# Option 2: start Proxy + upstream AgentServer in one process (role=all), then start the Client
./bin/proxy -role=all -proxy-listen=:8080 -agent-listen=:9090                     # Terminal A
./bin/client -transport=ws ws://127.0.0.1:8080                                    # Terminal B

# Run the full chain in one shot (agent-server + proxy + client orchestrated in one process)
make run-proxy
# Custom ports: make run-proxy PROXY_LISTEN=:8080 PROXY_AGENT_LISTEN=:9090
```

## 5. Configuration

### 5.1 Connection Options

The following `conn.With...` options are the **public options of `conn.NewClientConnection(...)`**. They are transport-agnostic and apply to the client side for WebSocket, stdio, and Streamable HTTP:

| Option | Default | Description |
| --- | --- | --- |
| `conn.WithRequestTimeout(d)` | 0 | ctx deadline for each inbound handler; `0` = unlimited |
| `conn.WithRequestWorkers(n)` | 8 | worker pool size per connection |
| `conn.WithMaxConsecutiveParseErrors(n)` | 0 | close the connection after N consecutive parse failures (defense against malicious peers); `0` = unlimited |
| `conn.WithConnectionLabel(label)` | empty | attach a label to logs for troubleshooting |
| `conn.WithOrderedNotificationMatcher(fn)` | built-in `session/update` | specify which notifications must be delivered in **strict order** |
| `conn.WithSessionListenerErrorHandler(fn)` | built-in warn log | callback for HTTP GET SSE listener failures (HTTP only) |
| `conn.WithNotificationErrorHandler(fn)` | built-in error log | callback when a notification handler returns an error or panics |

> Notes:
> - `conn.WithSessionListenerErrorHandler` and `conn.WithOrderedNotificationMatcher` are **ClientConnection-only**.
> - `conn.NewAgentConnectionFromTransport(...)` does **not currently expose** options publicly: its `opts ...jsonrpc.ConnectionOption` parameter type lives in an `internal/` package and cannot be constructed externally, so the agent side can only use defaults. If your agent is served through `server.ACPServer`, use the `server.With...` options from [5.3 ACPServer](#53-acpserver) instead; request timeout and notification error handling are configured there.

Shared defaults (`transport` package constants):

| Constant | Value |
| --- | --- |
| `transport.DefaultMaxMessageSize` | 10 MB |
| `transport.DefaultInboxSize` | 1024 |
| `transport.DefaultOutboxSize` | 1024 |
| `transport.DefaultACPEndpointPath` | `/acp` |

Example usage:

```go
conn := acpconn.NewClientConnection(client, transport,
	acpconn.WithRequestTimeout(60*time.Second),
	acpconn.WithRequestWorkers(16),
	acpconn.WithMaxConsecutiveParseErrors(10),
	acpconn.WithConnectionLabel("client#42"),
	acpconn.WithSessionListenerErrorHandler(func(sid string, err error) {
		metrics.Inc("acp_listener_fail", sid)
	}),
	acpconn.WithNotificationErrorHandler(func(method string, err error) {
		log.Printf("notify handler err: %s %v", method, err)
	}),
)
```

### 5.2 Client Transports

#### 5.2.1 stdio

```go
t := stdio.NewTransport(reader, writer,
	stdio.WithMaxMessageSize(10*1024*1024), // max size per NDJSON message, default 10 MB
	stdio.WithInitialBufSize(64*1024),      // initial Scanner buffer, default 64 KB
)
```

Characteristics:

- **Protocol**: newline-delimited JSON (one message per line).
- **Startup strategy**: the read goroutine starts on the first `ReadMessage` call; the writer goroutine starts on the first `WriteMessage` call. Reading and writing use independent goroutines.
- **Write timeout**: if the caller does not set a ctx deadline, the transport uses a default **30s** fallback timeout to prevent handlers from blocking forever when the downstream pipe is full.
- **Concurrency safety**: `WriteMessage` sends through `writeCh` to a dedicated writer goroutine, so concurrent calls from multiple goroutines are safe.
- **No keepalive / no reconnect**: the lifecycle is fully bound to the subprocess pipe. If the child process exits, `ReadMessage` returns `io.EOF`.
- **Close**: idempotent; closes the reader/writer if they implement `io.Closer`.

**Client-side usage:**

```go
cmd := exec.CommandContext(ctx, "/path/to/agent")
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
_ = cmd.Start()

// Note: pass the child's stdout as reader, and stdin as writer
t := stdio.NewTransport(stdout, stdin)
conn := acpconn.NewClientConnection(client, t)
_ = conn.Start(ctx)
```

**Agent-side usage:**

```go
t := stdio.NewTransport(os.Stdin, os.Stdout)
conn := acpconn.NewAgentConnectionFromTransport(agent, t)
if aware, ok := agent.(acpserver.ConnectionAwareAgent); ok {
	aware.SetClientConnection(conn)
}
_ = conn.Start(ctx)
<-conn.Done()
```

stdio options / defaults:

| Option | Default |
| --- | --- |
| `stdio.WithMaxMessageSize(n)` | 10 MB |
| `stdio.WithInitialBufSize(n)` | 64 KB |
| built-in write timeout (when ctx has no deadline) | 30 s |

#### 5.2.2 Streamable HTTP

[Streamable HTTP transport](https://agentclientprotocol.com/protocol/transports#streamable-http) defines the following model:

- **Request**: `POST {endpoint}` with a JSON-RPC message in the body.
- **Response**: the server usually returns SSE (at minimum containing the final JSON-RPC response); the client also accepts a single JSON response as a fallback.
- **Reverse channel**: `GET {endpoint}` where the server pushes reverse Request / Notification messages through SSE; the client responds through POST.
- **Session headers**: `Acp-Connection-Id`, `Acp-Session-Id`, `Acp-Protocol-Version`.

The SDK provides:
- Client: `transport/http/client.ClientTransport`
- Server: `server.ACPServer` (shared HTTP + WS server, see [5.3 ACPServer](#53-acpserver))

**Client initialization:**

```go
// Only the options worth tuning are shown here; omitting the rest uses defaults (see the table below)
t := acphttpclient.NewClientTransport("http://127.0.0.1:18080",
	acphttpclient.WithCustomHeaders(map[string]string{"X-Token": "..."}),
	acphttpclient.WithSSEReconnect(),                                        // enable GET SSE reconnect (default: unlimited retries, exponential backoff from 1s to 30s)
	acphttpclient.WithSSEReconnectMaxAttempts(10),                           // optional: retry at most 10 times; unlimited if unset
	acphttpclient.WithSSEReconnectBackoff(2*time.Second, time.Minute),       // optional: override the default backoff window
)

conn := acpconn.NewClientConnection(client, t)
_ = conn.Start(ctx)
```

Internal behavior:

- `conn.NewSession(...)` / `conn.LoadSession(...)` **automatically start the GET SSE listener**, so the application does not need to worry about when the reverse channel becomes ready.
- The max size for a non-SSE JSON response is **8 MB**; a single SSE event is limited to **10 MB**; error bodies are read up to **4 KB** only (to avoid large bodies blowing up memory).
- After `WithSSEReconnect()` is enabled, reconnect uses exponential backoff (default `1s -> 30s`). Failures are passed to the handler registered by `conn.WithSessionListenerErrorHandler`; they are **not** surfaced to the caller as RPC errors.

**Cookie / authentication:**

`ClientTransport` internally uses a `net/http/cookiejar`. Cookies sent by the server through `Set-Cookie` are retained for later POST/GET requests, which makes cookie-based sticky routing or authentication possible.

To inject Authorization headers:

```go
t := acphttpclient.NewClientTransport("http://...",
	acphttpclient.WithCustomHeaders(map[string]string{
		"Authorization": "Bearer xxx",
		"X-Tenant-Id":   "acme",
	}),
)
```

> `WithCustomHeaders` uses **Set** semantics and overwrites existing headers with the same name, rather than Add.

**Event flow summary:**

```
Client                           Server
  | --- POST initialize ---->      |
  |   (returns 200 SSE resp) <-----|  Acp-Connection-Id returned
  | --- POST session/new ---->     |
  |   (returns 200 SSE resp) <-----|  SessionID generated
  | --- GET  (SSE stream) ---->    |  reverse push channel established
  |                         <------|  session/update event
  | --- POST session/prompt ---->  |
  |   (returns 200 SSE resp) <-----|
```

HTTP client options / defaults (`transport/http/client`):

| Option | Default |
| --- | --- |
| `WithHTTPClient(c)` | `http.DefaultClient` |
| `WithClientEndpointPath(p)` | `/acp` |
| `WithCustomHeaders(m)` | empty |
| `WithInboxSize(n)` | 1024 |
| `WithSSEReconnect()` | disabled |
| `WithSSEReconnectMaxAttempts(n)` | effective only after calling `WithSSEReconnect()`; default is `-1` (unlimited), and `0` disables reconnect |
| `WithSSEReconnectBackoff(base, max)` | 1 s / 30 s |
| built-in non-SSE JSON limit | 8 MB |
| built-in SSE event limit | 10 MB |
| built-in error body read limit | 4 KB |

#### 5.2.3 WebSocket

**Client initialization:**

```go
t, err := acpws.NewWebSocketClientTransport("ws://127.0.0.1:18080",
	acpws.WithEndpointPath("/acp"),                         // default /acp
	acpws.WithCustomHeaders(map[string]string{"X-Token": "..."}),
)
if err != nil { ... }

if err := t.Connect(ctx); err != nil { // explicitly perform the WS handshake
	...
}
conn := acpconn.NewClientConnection(client, t)
_ = conn.Start(ctx)
```

Characteristics:

- **Based on Hertz**: the client uses `hclient.Client` + `websocket.ClientUpgrader`, staying in the same ecosystem as the server.
- **URL normalization**: supports `http://`, `https://`, `ws://`, `wss://`, and even bare `host:port`; the SDK automatically fills in the scheme (default `ws://`) and the endpoint path.
- **Origin only**: the path / query / fragment in `baseURL` is discarded. The final URL is `origin + endpointPath`. To change the path, use `WithEndpointPath`.
- **Cookie jar**: the handshake request sends the built-in `cookiejar`, and `Set-Cookie` from the response is written back into the jar. Each WS transport instance performs only one handshake, so the jar mainly exists for API consistency and has limited practical effect.
- **Write-timeout fallback**: if the caller does not set a ctx deadline, each write uses a default **30s** deadline. During `Close`, writing the close frame waits only **100ms** to acquire the write lock; if it cannot, the socket is closed directly to avoid hanging behind another blocked write.
- **Close order**: `Close` sends the close frame, closes the socket, waits for the read loop to exit, and then releases Hertz request/response objects, preventing use-after-free.
- **No automatic reconnect**: the application is responsible for recreating the transport + connection if needed.

**Server side:**

WebSocket server support is built into `server.ACPServer`; see [5.3 ACPServer](#53-acpserver). Under the same `/acp` route, ACPServer automatically routes to the WS upgrader when it sees the `Upgrade: websocket` header.

**Common pitfalls:**

1. **`srv.NoHijackConnPool = true`**: by default Hertz returns hijacked connections to the pool, which breaks WebSocket connections. You **must** set this flag when deploying ACPServer.
2. **Oversized frames**: the server read limit is **10 MB** and closes the connection directly with `1009 MessageTooBig` if exceeded; the client uses the same limit.
3. **10 consecutive parse failures**: the WS server closes the connection after **10** consecutive JSON-RPC parse errors to defend against malicious peers.
4. **Concurrent writes are safe**: ACP's `Transport` interface requires `WriteMessage` to be concurrency-safe. The WS client uses a `writePermit` semaphore internally, so application code can call it concurrently.

WebSocket client options / defaults (`transport/ws`):

| Option | Default |
| --- | --- |
| `WithEndpointPath(p)` | `/acp` |
| `WithCustomHeaders(m)` | empty |
| built-in single-write deadline (when ctx has no deadline) | 30 s |
| built-in wait for Close to acquire write lock | 100 ms |
| built-in close-frame write deadline | 500 ms |

<a id="53-acpserver"></a>
### 5.3 ACPServer

#### 5.3.1 Option Details

All parameters are injected through `Option`:

```go
remote, err := acpserver.NewACPServer(factory,
	acpserver.WithEndpoint("/acp"),
	acpserver.WithRequestTimeout(5 * time.Minute),
	acpserver.WithConnectionIdleTimeout(5 * time.Minute),
	acpserver.WithMaxHTTPMessageSize(10 * 1024 * 1024),
	acpserver.WithPendingQueueSize(1024),
	acpserver.WithMaxInflightDispatch(0), // 0 = use default (4096); negative = unlimited
	acpserver.WithWebSocketUpgrader(websocket.HertzUpgrader{
		CheckOrigin: func(ctx *app.RequestContext) bool { return true },
	}),
	acpserver.WithNotificationErrorHandler(func(method string, err error) {
		metrics.Inc("acp_notify_err", method, err.Error())
	}),
)
```

| Option | Default | Description |
| --- | --- | --- |
| `server.WithEndpoint(path)` | `/acp` | route path; normalized automatically (adds leading `/`, removes trailing `/`) |
| `server.WithRequestTimeout(d)` | 5 min | ctx deadline for each inbound handler; also applies to HTTP POST final-response wait time and each request handled by WS `AgentConnection`; `0` = unlimited |
| `server.WithConnectionIdleTimeout(d)` | 5 min | HTTP connection idle eviction; `0` or negative = disabled |
| `server.WithMaxHTTPMessageSize(n)` | 10 MB | POST body limit; returns `413` when exceeded |
| `server.WithPendingQueueSize(n)` | 1024 | message buffer after session creation and before GET SSE is established |
| `server.WithMaxInflightDispatch(n)` | 4096 | max concurrent dispatches per HTTP connection; returns `503` when exceeded; negative = unlimited |
| `server.WithWebSocketUpgrader(u)` | `websocket.HertzUpgrader{}` | custom subprotocols / origin checks |
| `server.WithNotificationErrorHandler(fn)` | none | callback for WS notification failures (not triggered for HTTP, because HTTP direct-dispatch has no read loop and notification failures are logged only) |

> ⚠️ Note: different options treat `0` differently:
> - `WithRequestTimeout(0)` / `WithConnectionIdleTimeout(0)` -> disabled (unlimited)
> - `WithMaxInflightDispatch(0)` -> use the default value `4096`; use `-1` for unlimited
>
> Passing `0` to `WithMaxInflightDispatch` assuming it means "unlimited" actually gives you the default cap of `4096`.

Built-in values (not configurable):

| Item | Value | Location |
| --- | --- | --- |
| SSE keepalive comment interval | 30 s | `internal/httpserver/parse.go` |
| Idle reaper interval | `min(idleTimeout/2, 30 s)` | `server/conn_table.go` |
| WS read limit | 10 MB | `internal/wsserver/server.go` |
| WS max consecutive parse errors | 10 | `server/remote_conn_ws.go` |

#### 5.3.2 Streamable HTTP Routing Rules

`ACPServer` routes based on HTTP method and headers:

| Method | Scenario | Behavior |
| --- | --- | --- |
| `POST /acp` | new connection (without `Acp-Connection-Id`) | creates a connection and returns a new connection ID in the response header; the body contains the first JSON-RPC request |
| `POST /acp` | existing connection (with `Acp-Connection-Id`) | reuses that connection and dispatches the body directly to it |
| `GET /acp` | with `Acp-Connection-Id` and `Acp-Session-Id` | opens the SSE listener for that session so the server can push reverse Request/Notification messages |
| `DELETE /acp` | with `Acp-Connection-Id` | closes the connection and releases resources |

What the pending queue (default `1024`) does: after a session is created but before the client opens the GET SSE listener, the server temporarily buffers reverse messages so they are not lost. Once the client connects via GET, those messages are flushed immediately. If the number of unconsumed messages exceeds `WithPendingQueueSize`, the session is closed and an error is returned, rather than silently dropping a single message. If your application expects many reverse messages, increase `WithPendingQueueSize`.

<a id="54-acpproxy"></a>
### 5.4 ACPProxy

`proxy.ACPProxy` is positioned as **byte-forwarding without protocol awareness**.

Use cases: forward external client WebSocket traffic to downstream services, usually your own AgentServer RPC service. Common scenarios include gateway layers, auth interception, multi-tenant routing, and canarying.

#### 5.4.1 Deployment Constraints

> **`server.ACPServer` and `proxy.ACPProxy` both use `/acp` by default.** If you mount them on the same Hertz router without changing the endpoint, route registration will conflict. If they must coexist, configure different paths explicitly.

#### 5.4.2 Basic Usage

```go
import hertzserver "github.com/cloudwego/hertz/pkg/app/server"

func main() {
	factory := &MyStreamerFactory{...} // implements acpstream.StreamerFactory

	p, err := acpproxy.NewACPProxy(factory,
		acpproxy.WithEndpoint("/acp"),
		acpproxy.WithHeaderForwarder(acpproxy.ForwardHeaders("Authorization", "X-Tenant-Id")),
		acpproxy.WithMaxConcurrentConnections(10000),
		acpproxy.WithHandshakeTimeout(15*time.Second),
		acpproxy.WithWebSocketWriteTimeout(30*time.Second),
		acpproxy.WithWebSocketPingInterval(30*time.Second),
		acpproxy.WithWebSocketPongTimeout(75*time.Second),
		acpproxy.WithMaxMessageSize(10*1024*1024),
	)
	if err != nil { log.Fatal(err) }

	srv := hertzserver.New(hertzserver.WithHostPorts(":8080"))
	srv.NoHijackConnPool = true
	p.Mount(srv)
	srv.Spin()
}
```

#### 5.4.3 Streamer Interface

The Proxy binds each client WS connection to one `Streamer`. A `Streamer` is a **bidirectional byte pipe** implemented by the user on top of their own RPC stack (gRPC, Kitex, TTHeader, Thrift streaming, WebSocket to AgentServer, etc.):

```go
type Streamer interface {
	WritePayload(ctx context.Context, payload []byte) error
	ReadPayload(ctx context.Context) ([]byte, error)
	Close(reason string) error
}

type StreamerFactory interface {
	NewStreamer(ctx context.Context, meta map[string]string) (Streamer, error)
}
```

Contract requirements (must be followed, or behavior is undefined):

- **Framing**: one `WritePayload` on one side corresponds to one `ReadPayload` on the other side; frame boundaries are your responsibility.
- **Concurrency**: `WritePayload` and `ReadPayload` may be called concurrently from two goroutines. `Close` may also race with them.
- **Close must be idempotent**; once triggered, all in-flight reads/writes must unblock quickly and return an error.
- **Do not swallow errors**: network errors, auth failures, and peer closes must be returned **as-is**.
- **Do not add your own timeouts**: the ctx only constrains the current call; long-lived connection lifecycle is controlled entirely by `Close`.
- **Return `io.EOF` for clean close** so the caller can identify it with `errors.Is(err, io.EOF)`.

#### 5.4.4 HeaderForwarder

The Proxy itself does not parse ACP, but it often needs to forward auth / tenant / trace headers to downstream services:

```go
acpproxy.WithHeaderForwarder(acpproxy.ForwardHeaders("Authorization", "X-Tenant-Id", "X-Request-Id"))
```

Or customize it:

```go
acpproxy.WithHeaderForwarder(func(c *app.RequestContext) map[string]string {
	meta := map[string]string{
		"trace_id": genTraceID(c),
	}
	if tok := string(c.GetHeader("Authorization")); tok != "" {
		meta["token"] = tok
	}
	return meta
})
```

Notes:
- The callback runs in the same goroutine as the Hertz handler, so **do not do expensive work**.
- The returned map becomes owned by the Proxy afterward; the callback must not mutate it again.

#### 5.4.5 Keepalive and Connection Health

The Proxy implements WS-level heartbeats:

- sends a Ping every `WithWebSocketPingInterval` (default `30s`);
- closes the connection if no frame (data or pong) is received within `WithWebSocketPongTimeout` (default `75s`);
- **receiving a Pong refreshes the read deadline**, so healthy connections should never time out.

> `WithWebSocketPingInterval(0)` only disables active Ping sending; the read deadline is still controlled by the Pong timeout.
> `WithWebSocketPongTimeout(0)` disables the read deadline entirely, so half-open connections will hold a concurrency slot **forever**. This is **not recommended**.

#### 5.4.6 Backpressure and Limits

| Dimension | Option | Default | Description |
| --- | --- | --- | --- |
| Max concurrent connections | `proxy.WithMaxConcurrentConnections(n)` | 10000 | returns `503` when exceeded |
| Handshake timeout | `proxy.WithHandshakeTimeout(d)` | 15 s | deadline for the WS handshake stage |
| WS write timeout | `proxy.WithWebSocketWriteTimeout(d)` | 30 s | deadline for a single write |
| Max message size | `proxy.WithMaxMessageSize(n)` | 10 MB | closes the connection when exceeded |

The key invariant of Proxy is: **one client WS <-> one Streamer**, with two dedicated up/down pump goroutines, and no cross-connection interference.

#### 5.4.7 Northbound WS Only, No HTTP Support

The Proxy intentionally **does not support** Streamable HTTP as the northbound entrypoint: Streamable HTTP consists of multiple independent HTTP requests (`POST` / `GET` / `DELETE`) and requires sticky routing by `Acp-Connection-Id` to the same backend. Without parsing the protocol, Proxy cannot guarantee that affinity, which conflicts with its design goal of **moving bytes only and not inspecting the protocol**. Non-WS requests return `400 Bad Request` directly:

```
proxy endpoint only supports WebSocket
```

If you need both HTTP support and proxying, have the downstream service expose `ACPServer` directly; Proxy is only responsible for the WS path.

## 6. Miscellaneous

### 6.1 Extension Methods (Custom Request / Notification)

ACP officially supports custom methods prefixed with `_` ([Extensibility](https://agentclientprotocol.com/protocol/extensibility#custom-requests)). On top of that, the SDK exposes two interfaces that can be implemented by either side, Agent or Client:

```go
// Custom Request (has a response)
type ExtMethodHandler interface {
	HandleExtMethod(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// Custom Notification (no response)
type ExtNotificationHandler interface {
	HandleExtNotification(ctx context.Context, method string, params json.RawMessage) error
}
```

#### 6.1.1 Sending Extension Messages

```go
// Client -> Agent
raw, err := clientConn.CallExtRequest(ctx, "_myvendor.getStats", map[string]any{
	"sessionId": sid,
	"scope":     "last-24h",
})
// raw is json.RawMessage; the application unmarshals it itself

_ = clientConn.CallExtNotification(ctx, "_myvendor.heartbeat", map[string]any{
	"ts": time.Now().Unix(),
})

// Agent -> Client (fully symmetric)
_ = agentConn.CallExtNotification(ctx, "_myvendor.toast", map[string]any{
	"sessionId": sid,
	"message":   "task completed",
})
```

The SDK enforces only one rule: **the method name must start with `_`**. Otherwise it returns an error directly.

#### 6.1.2 Receiving Extension Messages

As long as Agent or Client implements the two interfaces above, the SDK automatically dispatches non-built-in methods to them:

```go
type MyAgent struct { acp.BaseAgent }

func (a *MyAgent) HandleExtMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "_myvendor.getStats":
		var req acp.CustomExtRequest // {sessionId, _meta, data}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, acp.ErrInvalidParams(err.Error())
		}
		return map[string]any{
			"sessionId": req.SessionID,
			"stats":     gatherStats(req.SessionID),
		}, nil
	}
	return nil, acp.ErrMethodNotFound(method)
}

func (a *MyAgent) HandleExtNotification(_ context.Context, method string, params json.RawMessage) error {
	log.Printf("ext notify: %s %s", method, string(params))
	return nil
}
```

#### 6.1.3 `sessionId` Convention for Streamable HTTP

Streamable HTTP is a **multiplexed shared-connection mode**: the same TCP connection may carry multiple sessions or concurrent request flows. So if your extension message must be routed to a specific session, **you must include a top-level `sessionId` field in `params`**:

```json
{
  "sessionId": "sess-123",
  "data": {}
}
```

The SDK provides helper types:

```go
type CustomExtRequest struct {
	Meta      map[string]any  `json:"_meta,omitempty"`
	SessionID SessionID       `json:"sessionId"`
	Data      json.RawMessage `json:"data"`
}

type CustomExtNotification = CustomExtRequest
```

If you do not follow this convention, messages in HTTP mode may be routed to the wrong session.

WebSocket / stdio are point-to-point single-connection single-session modes, so `sessionId` is not required there, though including it is harmless.

### 6.2 Error Handling

#### 6.2.1 Unified Error Type

From handler return values all the way down to the wire protocol, the SDK uses the same `RPCError` type:

```go
type RPCError struct {
	Code    int             // JSON-RPC error code
	Message string
	Data    json.RawMessage // optional additional payload
}
```

Common constructors:

| Constructor | Code | Purpose |
| --- | --- | --- |
| `acp.ErrMethodNotFound(m)` | -32601 | unimplemented method |
| `acp.ErrInvalidParams(msg)` | -32602 | parameter validation failed |
| `acp.ErrInternalError(msg, data)` | -32603 | internal error; `data` can be an error, struct, or any serializable type |
| `acp.ErrServerBusy(msg)` | -32001 | server busy |
| `acp.ErrRequestCanceled(msg)` | -32800 | request canceled (ACP custom code) |
| `acp.NewRPCError(code, msg, data)` | custom | fully custom error |

`NewRPCError` is defensive about `data`:
- `json.RawMessage` / `[]byte` + valid JSON -> passed through directly;
- invalid JSON -> re-encoded as a JSON string so the on-wire payload stays valid;
- other types -> marshaled with `json.Marshal`; if marshaling fails, the SDK logs a warning and drops `data`.

#### 6.2.2 Error Propagation Principle

The SDK follows a strict **do not swallow errors** policy:

- If a handler returns an `error` that is already a `*RPCError`, the wire protocol uses it as-is; otherwise it is wrapped into `ErrInternalError`, **but the original error string is preserved** for diagnosis.
- Transport-level failures (parse error, write timeout, EOF, SSE disconnect, etc.) are propagated to the application through `Err()` / `Done()`.
- Notifications have no response channel, so failures go to `WithNotificationErrorHandler` (if registered) or to logs.

#### 6.2.3 Sentinel Errors (for `errors.Is`)

```go
transport.ErrTransportClosed  // transport already closed
transport.ErrConnNotStarted   // connection not started
transport.ErrConnClosed       // connection already closed
transport.ErrNoSessionID      // cannot route (usually extension message in HTTP mode missing sessionId)
transport.ErrPendingCancelled // reverse call canceled (pending tracker closed)
transport.ErrSenderClosed     // sender closed while reverse requests were still waiting
transport.ErrUnknownSession   // routed session does not exist or has expired
```

### 6.3 Logging

```go
// The default logger uses the standard library with reasonable prefixes; you can override it
acp.SetLogger(myLogger) // myLogger implements the acp.Logger interface (Printf-style)

l := acp.GetLogger() // returns the current logger and is never nil
```

`acp.Logger` is expected to provide `Debug / Info / Warn / Error` and `CtxDebug / CtxInfo / CtxWarn / CtxError`, all in Printf-style forms (see `logger.go` for the interface definition).

- **The default logger does not filter by level**: the default implementation under `internal/log` writes every level, including full Debug JSON-RPC payloads, directly through `log.Printf`. The SDK does not provide a `SetLevel` API. To suppress Debug logs, inject your own logger with `acp.SetLogger(...)` and implement level filtering there.
- **Access logs**: when Debug is enabled, the transport layer logs message send/receive events with the direction (`send` / `recv`) and the transport name, which is useful for traffic replay and debugging. If a custom logger does not implement `DebugEnabled() bool`, the SDK **conservatively disables** access payload logging so it does not keep copying 10 MB-class JSON-RPC frames while Debug is effectively off. If you want full payload logging, also add `DebugEnabled() bool` to your logger and return `true`. The SDK detects it through Go's structural typing:

  ```go
  type myLogger struct{ /* ... */ }

  // Implement the Debug/Info/... methods required by acp.Logger as needed by your project

  // Let the SDK enable access logs
  func (*myLogger) DebugEnabled() bool { return true }
  ```

### 6.4 Directory Layout at a Glance

```
acp/
├── types_gen.go / agent_gen.go / client_gen.go   // protocol generation
├── base.go                                        // BaseAgent / BaseClient
├── extension.go                                   // extension protocol helpers
├── errors.go                                      // RPCError
├── logger.go                                      // SetLogger / GetLogger
├── conn/                                          // bidirectional JSON-RPC wrapper
├── transport/
│   ├── stdio/                                     // newline-delimited JSON
│   ├── http/client/                               // Streamable HTTP client
│   └── ws/                                        // WebSocket client
├── server/                                        // Hertz server (HTTP + WS)
├── proxy/                                         // transparent WS proxy
├── stream/                                        // Streamer abstraction between Proxy and AgentServer
├── examples/                                      // runnable examples for agent / client / proxy
└── cmd/generate/                                  // schema-driven code generation
```

## 7. FAQ

- **"The request timed out, but the Agent actually finished the work"**: check server-side `WithRequestTimeout` (default `5min` for HTTP) and the client ctx deadline. For long HTTP jobs, increase the server timeout.
- **"`session/update` is missing"**: most likely the HTTP GET SSE listener was not established before the notification was sent. The SDK first buffers through `pendingQueue` (default `1024`). If it fills up, the SDK does not just drop one message: it closes the session and returns an error. Increase `WithPendingQueueSize` or ensure you call `NewSession` before pushing notifications.
- **"WebSocket disconnects immediately after connect"**: 99% of the time, Hertz is missing `NoHijackConnPool = true`.
- **"Goroutines leak after Close"**: make sure you call `conn.Close()`; for stdio, also make sure the underlying reader/writer is closed (`cmd.Wait()` reaps the subprocess pipes).
- **"An extension message is routed to the wrong session"**: in HTTP mode, make sure `params` includes `sessionId`.
