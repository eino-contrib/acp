# ACP

`ACP` is a Go SDK for the Agent Client Protocol (ACP).

This repository is an SDK, not an end-user agent application. It packages ACP
protocol types, typed client/agent connection wrappers, transport
implementations, and a remote server so Go applications can embed either side
of an ACP connection.

## Protocol References

- Official ACP protocol overview:
  <https://agentclientprotocol.com/protocol/overview>
- RFD draft for Streamable HTTP and WebSocket transport:
  <https://github.com/agentclientprotocol/agent-client-protocol/pull/721/>

## Highlights

- Generated ACP protocol types from the checked-in schema and meta files
- Generated `Agent` and `Client` interfaces plus wire-method constants
- Strict `BaseAgent` and `BaseClient` defaults for optional ACP methods
- Typed bidirectional RPC wrappers in [`conn/`](conn/)
- `stdio`, Streamable HTTP, and WebSocket transports
- Hertz-based remote server for HTTP and WebSocket exposure
- Support for custom `_`-prefixed extension requests and notifications
- Runnable agent and client examples in [`examples/`](examples/)

## Requirements

- Go `1.24+`
- Module path: `github.com/eino-contrib/acp`

Install:

```bash
go get github.com/eino-contrib/acp@latest
```

## What The SDK Provides

At a high level, the SDK covers these areas:

- ACP protocol types, enums, unions, and helpers in the root `acp` package
- Generated `Agent` and `Client` interfaces for the protocol surface
- Typed connection wrappers for both directions in `conn/`
- JSON-RPC transport implementations for `stdio`, Streamable HTTP, and WebSocket
- A remote `server.ACPServer` for serving ACP over Hertz
- A schema-driven generator in `cmd/generate/`

Generated protocol coverage currently includes:

- Agent-side RPCs such as `authenticate`, `initialize`, `session/list`,
  `session/load`, `session/new`, `session/prompt`, `session/set_mode`,
  `session/set_config_option`, and `session/cancel`
- Client-side reverse calls such as `fs/read_text_file`,
  `fs/write_text_file`, `session/request_permission`, `session/update`,
  `terminal/create`, `terminal/output`, `terminal/wait_for_exit`,
  `terminal/kill`, and `terminal/release`

## Package Layout

- [`acp`](.): generated protocol types, generated interfaces, `BaseAgent`,
  `BaseClient`, extension helpers, and protocol error types
- [`conn/`](conn/): typed inbound dispatch plus outbound request/notification
  helpers for both client and agent sides
- [`transport/stdio/`](transport/stdio/): newline-delimited JSON over standard
  I/O
- [`transport/http/client/`](transport/http/client/): Streamable HTTP client
  transport with header management, SSE response handling, and optional GET SSE
  reconnect
- [`transport/ws/`](transport/ws/): WebSocket client transport
- [`server/`](server/): Hertz-based remote ACP server for Streamable HTTP and
  WebSocket
- [`cmd/generate/`](cmd/generate/): schema loader, generator, and verification
  code
- [`examples/agent`](examples/agent) and
  [`examples/client`](examples/client): minimal runnable examples

## Connection Model

The main entry points are:

- `conn.NewAgentConnectionFromTransport(...)` for agent-side connections over
  `stdio` or WebSocket-style transports
- `conn.NewClientConnection(...)` for client-side typed calls into an agent
- `server.NewACPServer(...)` for serving Streamable HTTP and WebSocket on the
  same ACP endpoint

Notable behavior in the current implementation:

- `ClientConnection` preserves ordering for `session/update` notifications
- For Streamable HTTP, `ClientConnection.NewSession(...)` and
  `ClientConnection.LoadSession(...)` automatically start GET SSE listeners so
  server-initiated messages can flow back to the client
- `server.ACPServer` creates one `acp.Agent` per remote connection
- If an agent implements `server.ConnectionAwareAgent`, the server injects the
  per-connection `*conn.AgentConnection` so the agent can call back into the
  client

## SDK Usage Demo

The snippets below show the current SDK entry points for embedding an ACP
client or exposing an ACP server in your own Go application. For full runnable
versions, see [`examples/client`](examples/client) and
[`examples/agent`](examples/agent).

### Client

The following example connects to a remote ACP server over Streamable HTTP,
receives `session/update` notifications, and sends a prompt:

```go
package main

import (
	"context"
	"fmt"
	"log"

	acp "github.com/eino-contrib/acp"
	acpconn "github.com/eino-contrib/acp/conn"
	acphttpclient "github.com/eino-contrib/acp/transport/http/client"
)

type Client struct{ acp.BaseClient }

func (c *Client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	if chunk, ok := n.Update.AsAgentMessageChunk(); ok {
		if text, ok := chunk.Content.AsText(); ok {
			log.Printf("agent message: %s", text.Text)
		}
	}
	return nil
}

func main() {
	ctx := context.Background()

	transport := acphttpclient.NewClientTransport("http://127.0.0.1:8080")
	conn := acpconn.NewClientConnection(&Client{}, transport)
	if err := conn.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ClientInfo: &acp.Implementation{
			Name:    "demo-client",
			Version: "0.1.0",
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("protocol=%d\n", initResp.ProtocolVersion)

	sessionResp, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        ".",
		MCPServers: []acp.MCPServer{},
	})
	if err != nil {
		log.Fatal(err)
	}

	_, err = conn.Prompt(ctx, acp.PromptRequest{
		SessionID: sessionResp.SessionID,
		Prompt: []acp.ContentBlock{
			acp.NewContentBlockText(acp.TextContent{Text: "hello from client"}),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

Notes:

- For WebSocket, create the transport with
  `transport/ws.NewWebSocketClientTransport(...)`, call `Connect(ctx)`, then
  pass it to `conn.NewClientConnection(...)`.
- For stdio/subprocess mode, wrap the child process pipes with
  `transport/stdio.NewTransport(...)` and use the same
  `conn.NewClientConnection(...)` entry point.
- On Streamable HTTP, `NewSession(...)` and `LoadSession(...)` automatically
  open the GET SSE listener used for reverse calls such as `session/update`.

### Server

The following example exposes an ACP agent over Hertz. `server.ACPServer`
serves Streamable HTTP and WebSocket upgrades on the same endpoint:

```go
package main

import (
	"context"
	"log"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"

	acp "github.com/eino-contrib/acp"
	acpserver "github.com/eino-contrib/acp/server"
	"github.com/google/uuid"
)

type Agent struct{ acp.BaseAgent }

func (a *Agent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersion(acp.CurrentProtocolVersion),
		AgentInfo: &acp.Implementation{
			Name:    "demo-agent",
			Version: "0.1.0",
		},
	}, nil
}

func (a *Agent) NewSession(_ context.Context, _ acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{
		SessionID: acp.SessionID(uuid.NewString()),
	}, nil
}

func (a *Agent) Prompt(_ context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	log.Printf("session=%s prompt_blocks=%d", req.SessionID, len(req.Prompt))
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func main() {
	srv := hertzserver.New(hertzserver.WithHostPorts(":8080"))

	remote, err := acpserver.NewACPServer(func(_ context.Context) acp.Agent {
		return &Agent{}
	})
	if err != nil {
		log.Fatal(err)
	}

	remote.Mount(srv) // default endpoint: /acp
	srv.Spin()
}
```

If your agent needs to call back into the client (for example
`session/update`, `fs/read_text_file`, or terminal methods), implement
[`server.ConnectionAwareAgent`](server/server.go) and store the injected
`*conn.AgentConnection`, as shown in [`examples/agent`](examples/agent).

## Examples

Build the example binaries:

```bash
make build
```

Run a local stdio pair:

```bash
./bin/client -transport=spawn ./bin/agent
```

Run the example agent as a remote server:

```bash
go run ./examples/agent -transport=http -listen=:8080
```

Connect over Streamable HTTP:

```bash
go run ./examples/client -transport=http http://127.0.0.1:8080
```

The HTTP client uses the default ACP endpoint path `/acp`, so the example above
targets `http://127.0.0.1:8080/acp`.

Connect over WebSocket:

```bash
go run ./examples/client -transport=ws ws://127.0.0.1:8080/acp
```

The example server exposes both Streamable HTTP and WebSocket upgrades on the
same endpoint.

### One-command demo

`make run-ws` builds both binaries, kills any previous process on the listen
port, starts the agent in the background, and runs the client over WebSocket:

```bash
make run-ws
```

The default listen address is `:18080`. Override it with:

```bash
make run-ws AGENT_ADDR=:9090
```

You can also start the agent and client separately:

```bash
make run-agent            # start agent only
make run-client           # connect client via WebSocket
```

## Code Generation

Checked-in schema inputs live under [`cmd/generate/schema/`](cmd/generate/schema/).

Regenerate the SDK from the checked-in schema files:

```bash
make gen
```

`make gen` currently regenerates:

- `types_gen.go`
- `agent_gen.go`
- `client_gen.go`
- `internal/methodmeta/metadata_gen.go`
- `conn/agent_outbound_gen.go`
- `conn/client_outbound_gen.go`
- `conn/handlers_gen.go`

By default, `make gen` uses the checked-in schema files and does not download
fresh inputs.

To refresh `schema.json` and `meta.json` from the upstream ACP repository
before regenerating:

```bash
make gen-refresh
```

If you need to tune the network budget, invoke the generator directly:

```bash
go run ./cmd/generate -download=true -download-timeout=30s
```

## Quality Checks

```bash
make vet
make test
make test-race
make build
```

## Configuration & Limits

The transport layer exposes knobs for message size, timeouts, buffers, worker
concurrency, and reconnect behavior. All defaults can be overridden with the
`With*` option functions at construction time. Paths below are relative to the
repository root.

### Common (applies to every transport)

These options are attached to `conn.NewClientConnection(...)` and shape the
shared JSON-RPC connection underneath every transport.

| Option | Default | Notes |
| --- | --- | --- |
| `conn.WithRequestTimeout(d)` | `0` (disabled) | Per-request deadline for inbound handlers |
| `conn.WithRequestWorkers(n)` | `8` | Worker pool size per connection |
| `conn.WithMaxConsecutiveParseErrors(n)` | `0` (unbounded) | Close after N consecutive parse / bad-JSON-RPC errors |
| `conn.WithConnectionLabel(label)` | empty | Label attached to connection logs |
| `conn.WithOrderedNotificationMatcher(fn)` | `session/update` matcher | Selects notifications dispatched in strict order |
| `conn.WithSessionListenerErrorHandler(fn)` | noop | Receives GET SSE listener errors (HTTP only) |

Shared constants (defined in `transport/transport.go`):

| Constant | Default |
| --- | --- |
| `transport.DefaultMaxMessageSize` | `10 MB` |
| `transport.DefaultInboxSize` | `1024` |
| `transport.DefaultOutboxSize` | `1024` |

### Stdio

Constructed with `stdio.NewTransport(reader, writer, opts...)`.

| Option | Default | Notes |
| --- | --- | --- |
| `stdio.WithMaxMessageSize(n)` | `10 MB` | Max size of a single inbound NDJSON line |
| `stdio.WithInitialBufSize(n)` | `64 KB` | Initial scanner buffer |

Stdio has no keepalive, request timeout, or reconnect; lifecycle is bound to
the subprocess pipes.

### Streamable HTTP

Server-side options (via `server.NewACPServer(..., opts...)`):

| Option | Default | Notes |
| --- | --- | --- |
| `server.WithEndpoint(path)` | `/acp` | Mount path |
| `server.WithRequestTimeout(d)` | `5 min` | Deadline for a single POST waiting for its response; `0` disables |
| `server.WithConnectionIdleTimeout(d)` | `5 min` | HTTP connection idle eviction; `0` disables |
| `server.WithMaxHTTPMessageSize(n)` | `10 MB` | POST body limit; oversize returns HTTP 413 |
| `server.WithPendingQueueSize(n)` | `2048` | Messages buffered between session creation and the first GET SSE listener |

Server-side internals (defined by the HTTP handler, not user-tunable):

| Setting | Default | Location |
| --- | --- | --- |
| SSE keepalive interval | `30 s` | `internal/httpserver/parse.go` |
| Idle-reaper interval | `min(timeout/2, 30 s)` | `server/conn_table.go` |

Client-side options (via `httpclient.NewClientTransport(baseURL, opts...)`,
package `transport/http/client`):

| Option | Default | Notes |
| --- | --- | --- |
| `httpclient.WithHTTPClient(c)` | `http.DefaultClient` | Override underlying `*http.Client` |
| `httpclient.WithClientEndpointPath(p)` | `/acp` | Target endpoint path |
| `httpclient.WithCustomHeaders(m)` | empty | Extra request headers |
| `httpclient.WithInboxSize(n)` | `1024` | Inbound message channel buffer |
| `httpclient.WithSSEReconnect()` | off | Enable GET SSE auto-reconnect with exp backoff (1 s → 30 s) |
| `httpclient.WithSSEReconnectMaxAttempts(n)` | unlimited | Cap reconnect attempts; negative = infinite |
| `httpclient.WithSSEReconnectBackoff(base, max)` | `1 s` / `30 s` | Exponential backoff bounds |

Client-side internals:

| Setting | Default | Location |
| --- | --- | --- |
| Non-SSE JSON response cap | `8 MB` | `transport/http/client/client.go` |
| SSE single-event cap | `10 MB` | `transport/http/client/client.go` |
| SSE scanner buffer | `64 KB` → `10 MB` | `transport/http/client/client.go` |
| Error-body read cap | `4 KB` | `transport/http/client/client.go` |

### WebSocket

Server-side options (same `server.NewACPServer(...)` constructor):

| Option | Default | Notes |
| --- | --- | --- |
| `server.WithWebSocketUpgrader(u)` | Hertz default upgrader | Customize upgrade (subprotocols, origin check, ...) |

Server-side internals:

| Setting | Default | Location |
| --- | --- | --- |
| WebSocket read limit | `10 MB` | `internal/wsserver/server.go` |
| Max consecutive parse errors | `10` | `server/remote_conn_ws.go` |

The per-request timeout and worker-pool size are inherited from the common
`conn.With*` options. `server.WithRequestTimeout` only applies to HTTP POSTs;
WebSocket requests are governed by the common JSON-RPC layer and context
propagation.

Client-side options (via `ws.NewWebSocketClientTransport(baseURL, opts...)`,
package `transport/ws`):

| Option | Default | Notes |
| --- | --- | --- |
| `ws.WithCustomHeaders(m)` | empty | Extra headers on the upgrade request |

Client-side internals:

| Setting | Default | Location |
| --- | --- | --- |
| Close-frame write-permit wait | `100 ms` | `transport/ws/client.go` |
| Close-frame write deadline | `500 ms` | `transport/ws/client.go` |

### Cross-transport comparison

| Aspect | Stdio | Streamable HTTP | WebSocket |
| --- | --- | --- | --- |
| Max inbound message | `10 MB` | `10 MB` (POST) | `10 MB` |
| Per-request timeout | none (context only) | `5 min` | common layer (`conn.WithRequestTimeout`) |
| Connection idle eviction | none | `5 min` | none |
| Keepalive | none | SSE comment every `30 s` | none built in |
| Reconnect | none | SSE client auto-reconnect (`1 s` → `30 s`) | none built in |
| Parse-error close threshold | unbounded | unbounded | `10` |
| Worker pool per connection | `8` | `8` | `8` |
| Inbox / Outbox buffers | `1024` / `1024` | `1024` + `2048` pending | `1024` / `1024` |

## Extension Methods

Custom extension methods are supported across `conn.ClientConnection` and
`conn.AgentConnection` via `_`-prefixed method names.

For Streamable HTTP, include `sessionId` in extension `params` when multiple
sessions or concurrent request streams may share the same connection, so the
receiver can route the message unambiguously.
