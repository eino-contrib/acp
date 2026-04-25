# ACP Go SDK 架构总览

本文档面向第一次接触本项目的读者，目标是在短时间内建立对 **代码分层**、**核心抽象** 和 **运行时链路** 的整体认知，不深入协议细节。

---

## 1. 项目定位

`github.com/eino-contrib/acp` 是 [Agent Client Protocol (ACP)](https://agentclientprotocol.com/) 的 Go SDK。ACP 是一个基于 **JSON-RPC 2.0** 的 **双向 RPC 协议**，在 Client（宿主 / IDE）与 Agent（AI 服务）之间传递 prompt、session 更新、文件访问、权限请求等消息。

SDK 的核心价值：把协议、连接、传输、多路复用、代理这些横切关注点封装好，让使用者只需要实现业务接口 `acp.Agent` / `acp.Client`。

---

## 2. 整体架构图

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

> 左栏是 Client 侧（IDE / 宿主程序），右栏是 Agent 侧（AI Agent 服务）；中间层次从上到下：L3 ACP 协议端点 → L2 JSON-RPC 引擎 → L1 字节管道；三种传输 (A)(B)(C) 可任选其一，或者通过 (D) Proxy 透传。

**从上到下一句话概括**：用户实现 `acp.Agent` / `acp.Client` → 由 `conn.*Connection` 做 ACP 层的方法派发与反向 RPC → 下沉到 `internal/jsonrpc` 做编解码与请求-响应匹配 → 最终通过 `transport.Transport` 的某个实现（stdio / WS / HTTP+SSE）把字节送出去。服务端场景用 `server.ACPServer` 聚合多连接，网关场景用 `proxy.ACPProxy` 透传字节。

---

## 3. 核心抽象一句话速记

| 抽象 | 所在包 | 职责 |
|---|---|---|
| `acp.Agent` / `acp.Client` | 根包 | 业务接口：由使用者实现 |
| `BaseAgent` / `BaseClient` | 根包 | 默认实现：Request 方法返回 `method not found: <method>`，Notification 方法返回 `notification handler not implemented: <method>`（让缺失的协议钩子不静默消失） |
| `conn.AgentConnection` | `conn/` | Agent 侧的协议端点，派发 inbound、提供反向 RPC |
| `conn.ClientConnection` | `conn/` | Client 侧的协议端点，派发 inbound、发起 prompt 等 |
| `transport.Transport` | `transport/` | 抽象 `ReadMessage/WriteMessage/Close` 的字节管道 |
| `server.ACPServer` | `server/` | 多连接远端服务：同一 `/acp` 路由同时承载 HTTP(POST+SSE) 和 WS |
| `proxy.ACPProxy` | `proxy/` | 透传代理：只搬字节，不解析协议 |
| `stream.Streamer` / `StreamerFactory` | `stream/` | Proxy 到下游 AgentServer 的南向传输抽象（用户可用 gRPC/Kitex/WS 自己实现） |

---

## 4. 三种端到端链路

### 4.1 直连模式

三种直连传输共享上层（业务接口 + `*Connection` + `jsonrpc.Conn`），只在 L1 传输层不同。下面分别画出各自的链路。

#### 4.1.1 WebSocket（全双工长连接）

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

特点：一条 TCP 连接承载双向全部流量（请求 + 响应 + 反向 RPC + 通知），无需粘滞路由。
```

#### 4.1.2 Streamable HTTP（POST + GET (SSE) + DELETE）

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

特点：一条 逻辑连接 拆成多次 HTTP 请求，服务端用 connTable 按 Acp-Connection-Id 粘合
      成一条 AgentConnection（内部持有自己的 jsonrpc.Connection + pending map）。
      服务端 POST 响应统一走 SSE（Request）或 202 Accepted（Notification），
      不会返回 application/json。
      负载均衡必须做 粘滞路由，让同一 connID 的 POST/GET/DELETE 到同一后端实例。
```

#### 4.1.3 stdio（父子进程）

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

特点：父进程通过 exec.Cmd 拉起子进程，stdin/stdout 各承担一个方向；典型用于 IDE 本地拉起 Agent。
```

### 4.2 服务端多连接链路（`server.ACPServer`）

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

- `connTable` **只存 HTTP 远端连接**（`server/conn_table.go` 的 `connTable` 类型注释里写明 `WebSocket connections are intentionally not tracked here`），因为 Streamable HTTP 需要按 `Acp-Connection-Id` 把多次 POST/GET/DELETE 粘合成同一条逻辑连接，且需要 idle reaper 回收客户端消失后遗留的会话。
- **WS 连接**由独立的 `wsConns` 管理：WS 本身就是长连接，生命周期跟随 TCP，不需要 idle 回收表；它只是为了在 `ACPServer.Close` 时能集中关闭。
- `AgentFactory` 为每条远端连接创建一个 Agent 实例；若 Agent 实现 `ConnectionAwareAgent.SetClientConnection(*AgentConnection)`，服务端自动注入连接句柄，Agent 就能反向调用 Client（如 `fs/read_text_file`、`session/update`、`session/request_permission`）。

### 4.3 Proxy 透传链路（`proxy.ACPProxy`）

```
┌────────┐   WS bytes   ┌─────────────────┐   Streamer   ┌────────────────────┐
│ Client │◄────────────►│  ACPProxy       │◄────────────►│ Upstream           │
│        │  /acp (WS)   │  up-pump        │  user-chosen │ AgentServer        │
│        │              │  down-pump      │  gRPC/Kitex/ │ (e.g. stdio→Agent) │
│        │              │  HeaderForwarder│  WS/...      │                    │
└────────┘              └─────────────────┘              └────────────────────┘

          Proxy 只是字节管道，不解析 ACP；一条北向 WS ↔ 一个 Streamer ↔ 一个下游会话
```

- 仅支持北向 WebSocket。
- `up-pump` / `down-pump` 是 `proxy/conn.go` 里对每条 WS 连接启动的两个 goroutine（对应函数 `upPump` / `downPump`）：前者把北向 WS 读到的字节搬到下游 Streamer，后者把 Streamer 下行的字节写回北向 WS。两者都只搬字节，不做 JSON-RPC 解析。
- `HeaderForwarder`（`proxy/options.go`，通过 `proxy.WithHeaderForwarder` 安装）把北向 HTTP 请求头投影成一个 `map[string]string`，作为 `meta` 透传给下游 Streamer，让下游可以拿到鉴权/追踪之类的头信息。
- 下游通常是使用者自建的 RPC 服务；典型实现里下游把收到的字节喂给 `stdio.Transport`，再由 `conn.NewAgentConnectionFromTransport` 驱动 `acp.Agent`。
- Proxy 与 `ACPServer` 默认都挂在 `/acp`：若都使用默认路径则**同一 Hertz 进程只能部署一个**，需通过 `proxy.WithEndpoint` / `server.WithEndpoint` 配成不同路径才能共存。

---

## 5. 一次请求的内部流转（以 WebSocket 为例）

以 Client 调用 `Prompt(ctx, req)` 为例，一次「前向请求 + 等待响应」的完整链路如下（编号代表时间顺序）：

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

响应方向（⑦ → ⑩）：

```
 ⑦ agent.Prompt 返回 PromptResponse
     │
     ▼
   worker 把它打包成 JSON-RPC Response（复用原 id），直接 transport.WriteMessage 写回
     │
     ▼
 ⑧ Client 侧 readLoop 读到 Response，按 id 在 sync.Map pending 里找到等待 channel 并写入
     │
     ▼
 ⑨ ① 处挂起的 cc.Prompt 被唤醒，解出 PromptResponse
     │
     ▼
 ⑩ resp, err := cc.Prompt(...) 返回给应用
```

**反向 RPC 同理**：Agent 侧调用 `AgentConnection.ReadTextFile / RequestPermission` 这类 **Request** 走一模一样的 ①—⑩ 流程，只是方向反过来（Agent → Client）；`AgentConnection.SessionUpdate` 这类 **Notification** 只走 ①—⑥（发送方发完即忘、无响应等待，但接收侧仍经 readLoop 分类和 worker pool / ordered drain 执行 handler；不会产生 ⑦—⑩ 的响应回传）。两者都复用**同一条 WS 连接**。

**几个关键事实（对照 `internal/jsonrpc/connection.go` 中 `Connection` 类型上方的 `Synchronization overview` 与 `Dispatch model` 注释块）：**

1. **读方向只有一个 goroutine：`readLoop`**。它同步调用 `transport.ReadMessage` 拉一条消息，就地分类；**没有对称的 `writeLoop / outbox`** —— 写方向由调用方 goroutine 直接 `transport.WriteMessage`，是否在传输层再排队由具体 Transport 自己决定（例如 WS 实现内部可能有自己的发送 channel）。需要注意的是，除了 `readLoop`，连接还常驻一组 worker pool goroutine 和一个 ordered drain goroutine（见下一条）。
2. **readLoop 绝不阻塞在 handler 上**：
   - `Response` 就地 O(1) 处理（查 `pending` sync.Map 并送值），不经过任何队列；
   - `Request` / 无序 `Notification` 入 **intake 无界队列**，由固定大小的 **worker pool** 并发消费；
   - 有序 `Notification`（例如 `session/update`，必须保序）入 **另一条单消费者队列**，由单独的 drain goroutine 串行执行。
3. **请求–响应配对靠 `pending` (`sync.Map`)**：发请求时以 JSON-RPC id 为 key 注册一个 `chan response`，收到响应时按 id 唤醒调用方。Notification 没有 id，发完即忘。
4. **L2（jsonrpc）不认识 ACP 方法语义**，只管 envelope、id 分配、pending 匹配、队列与 worker pool；**L3（`*Connection`）** 才知道 `acp.MethodAgentPrompt` 这类方法常量与参数类型，负责编解码 + 注册 handler。

> Streamable HTTP 的**客户端**与 stdio/WS 一样，通过 `transport/http/client` 接到同一套 `jsonrpc.Connection + readLoop + worker pool` 上；但**服务端**不同：`internal/httpserver.ProtocolConnection` 独立处理 POST/GET/DELETE 的协议语义，并**不使用** `jsonrpc.Connection`（见 `server/connection.go` "Create AgentConnection with the sender (no jsonrpc.Connection)" 注释）。ACPServer 通过 `httpAgentSender` 把反向 Request / Notification 投递到 per-session 的 SSE outbox。stdio 两端则仍是对称的 `jsonrpc.Connection`，只是字节通道是父子进程的 stdin/stdout。

关于代码生成与 Transport 扩展：
1. **方法派发完全由代码生成**。`cmd/generate` 读 `schema.json` 产出：
   - 根目录：`types_gen.go`、`agent_gen.go`、`client_gen.go`、`base.go`（含 `BaseAgent/BaseClient` 默认实现）
   - `conn/`：`agent_outbound_gen.go`、`client_outbound_gen.go`、`handlers_gen.go`
   - `internal/methodmeta/metadata_gen.go`

   手写维护的是 `conn/dispatch.go` 的派发骨架、`conn/{agent,client}.go` 的连接装配、以及传输层。
2. **Transport 只管字节**：增加一种新传输只需要实现 `transport.Transport` 的 `ReadMessage/WriteMessage/Close`。

---

## 6. 目录速查

| 路径 | 作用 |
|---|---|
| `*.go` (根目录) | 协议类型、接口、BaseAgent/BaseClient、错误、扩展方法、方法元数据查询（`methods.go`）、全局 Logger 装配（`logger.go`） |
| `conn/` | `AgentConnection` / `ClientConnection` 的派发与反向 RPC |
| `transport/` | `transport.go` 定义公共基石：`Transport` 接口、sentinel errors（`ErrTransportClosed` 等）与共享常量（`HeaderConnectionID`、`DefaultACPEndpointPath` 等）。子包 `stdio` / `ws` / `http/client` 是具体实现——`stdio` 两端对称复用，`ws` / `http/client` 仅客户端使用（对应 `internal/{wsserver,httpserver}` 的服务端实现） |
| `internal/jsonrpc` | JSON-RPC 2.0 引擎（envelope / queue / connection） |
| `internal/httpserver` | 服务端 Streamable HTTP（POST/GET(SSE)/DELETE）适配 Hertz |
| `internal/wsserver` | 服务端 WebSocket 适配 Hertz |
| `internal/connspi`, `peerstate`, `methodmeta`, `endpoint`, `safe`, `log`, `wsutil` | 内部 SPI、peer 协议版本 / 会话元数据追踪、方法元数据、路径归一化、panic 恢复与 goroutine 工具、日志、WebSocket 关闭原因工具 |
| `server/` | `ACPServer`（多连接、HTTP+WS 共用 `/acp`） |
| `proxy/` + `stream/` | 透传代理 + 南向 Streamer 抽象 |
| `cmd/generate/` | 由 `schema.json` 生成类型 / 接口 / 派发 / 元数据代码 |
| `examples/{agent,client,proxy}` | 三类角色的可运行示例 |

---

## 7. 给不同读者的入口建议

- **写一个 Agent 服务**：看 `examples/agent/*` + `server/server.go` + `conn/agent.go`。
- **写一个 Client（宿主）**：看 `examples/client/*` + `conn/client.go` + `transport/{ws,http/client,stdio}`。
- **写一个网关/代理**：看 `examples/proxy/*` + `proxy/proxy.go` + `stream/streamer.go`。
- **给协议加扩展方法**：看根包 `extension.go` 与 `conn/dispatch.go`。
- **改协议/重新生成代码**：看 `cmd/generate/` 与 `cmd/generate/schema/schema.json`。
