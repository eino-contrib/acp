# ACP Go SDK 架构总览

本文档面向第一次接触本项目的读者，目标是在短时间内建立对 **代码分层**、**核心抽象** 和 **运行时链路** 的整体认知，不深入协议细节。

---

## 1. 项目定位

`github.com/eino-contrib/acp` 是 [Agent Client Protocol (ACP)](https://agentclientprotocol.com/) 的 Go SDK。ACP 是一个基于 **JSON-RPC 2.0** 的 **双向 RPC 协议**，在 "Client (宿主/IDE)" 与 "Agent (AI 服务)" 之间传递 prompt、session 更新、文件访问、权限请求等消息。

SDK 的核心价值：把协议、连接、传输、多路复用、代理这些横切关注点封装好，让使用者只需要实现业务接口 `acp.Agent` / `acp.Client`。

---

## 2. 整体架构图

```
                         ┌───────────────────────────────────────────────────────┐
                         │                 ACP Go SDK 架构总览                    │
                         └───────────────────────────────────────────────────────┘

   ┌─────────────────────┐                                      ┌─────────────────────┐
   │      CLIENT 侧       │                                      │       AGENT 侧       │
   │   (IDE / 宿主程序)    │                                      │    (AI Agent 服务)    │
   │                     │                                      │                     │
   │  ┌───────────────┐  │        业务接口（用户实现）             │  ┌───────────────┐  │
   │  │  acp.Client   │  │                                      │  │  acp.Agent    │  │
   │  └───────┬───────┘  │                                      │  └───────▲───────┘  │
   │          │          │                                      │          │          │
   │  ┌───────▼────────┐ │     L3: ACP 协议端点 (方法派发/反向RPC) │ ┌────────┴───────┐ │
   │  │ Client-        │ │                                      │ │ AgentConnection│ │
   │  │ Connection     │◄┼──────────────────────────────────────┼►│                │ │
   │  └───────┬────────┘ │                                      │ └────────▲───────┘ │
   │          │          │                                      │          │          │
   │  ┌───────▼────────┐ │     L2: JSON-RPC 2.0 引擎             │ ┌────────┴───────┐ │
   │  │ jsonrpc.Conn   │ │     (envelope / queue / pending)     │ │ jsonrpc.Conn   │ │
   │  └───────┬────────┘ │                                      │ └────────▲───────┘ │
   │          │          │                                      │          │          │
   │  ┌───────▼────────┐ │     L1: Transport (字节管道)           │ ┌────────┴───────┐ │
   │  │   Transport    │ │                                      │ │   Transport    │ │
   │  └───────┬────────┘ │                                      │ └────────▲───────┘ │
   └──────────┼──────────┘                                      └──────────┼──────────┘
              │                                                            │
              │              —— 三选一传输 (或经 Proxy) ——                    │
              │                                                            │
   ┌──────────┴────────────────────────────────────────────────────────────┴──────────┐
   │                                                                                 │
   │   (A) stdio       父子进程      stdin/stdout NDJSON                              │
   │   (B) WebSocket   全双工        hertz-contrib/websocket                         │
   │   (C) Streamable  POST+SSE      hertz (internal/httpserver)                     │
   │        HTTP       GET 建 SSE 反向通道 / DELETE 关闭                              │
   │                                                                                 │
   │   (D) Proxy 模式: Client ─WS─► ACPProxy ══Streamer══► 下游 AgentServer ─► Agent │
   │                   (仅搬字节，不解析 ACP；南向由用户自选 gRPC/Kitex/WS)             │
   │                                                                                 │
   └─────────────────────────────────────────────────────────────────────────────────┘

   ┌───────────────────── 服务端多连接聚合 (server.ACPServer) ─────────────────────┐
   │                                                                             │
   │   Hertz /acp                                                                │
   │     ├─ WS upgrade   ──►  每条 WS 连接独立生命周期（不进 connTable）            │
   │     └─ HTTP POST/GET/DELETE ──► connTable (按 Acp-Connection-Id 索引 + idle 回收) │
   │                                                                             │
   │   每个远端连接都会生成一个 AgentConnection + 一个由 AgentFactory() 产出的     │
   │   Agent；若 Agent 实现 ConnectionAwareAgent，即可拿到连接句柄做反向 RPC       │
   │   （connTable 仅服务于 Streamable HTTP：因为 HTTP 是多次无状态请求，必须按 ID │
   │    粘合同一逻辑连接；WS 本身就是长连接，无需额外索引）                          │
   └─────────────────────────────────────────────────────────────────────────────┘
```

**从上到下一句话概括**：用户实现 `acp.Agent` / `acp.Client` → 由 `conn.*Connection` 做 ACP 层的方法派发与反向 RPC → 下沉到 `internal/jsonrpc` 做编解码与请求-响应匹配 → 最终通过 `transport.Transport` 的某个实现（stdio / WS / HTTP+SSE）把字节送出去。服务端场景用 `server.ACPServer` 聚合多连接，网关场景用 `proxy.ACPProxy` 透传字节。

---

## 3. 核心抽象一句话速记

| 抽象 | 所在包 | 职责 |
|---|---|---|
| `acp.Agent` / `acp.Client` | 根包 | 业务接口：由使用者实现 |
| `BaseAgent` / `BaseClient` | 根包 | 默认实现，未覆盖的方法返回 `method not found` |
| `conn.AgentConnection` | `conn/` | Agent 侧的协议端点，派发 inbound、提供反向 RPC |
| `conn.ClientConnection` | `conn/` | Client 侧的协议端点，派发 inbound、发起 prompt 等 |
| `transport.Transport` | `transport/` | 抽象 `Send/Recv` 的字节管道 |
| `server.ACPServer` | `server/` | 多连接远端服务：同一 `/acp` 路由同时承载 HTTP(POST+SSE) 和 WS |
| `proxy.ACPProxy` | `proxy/` | 透传代理：只搬字节，不解析协议 |
| `stream.Streamer` / `StreamerFactory` | `stream/` | Proxy 到下游 AgentServer 的南向传输抽象（用户可用 gRPC/Kitex/WS 自己实现）|

---

## 4. 三种端到端链路

### 4.1 直连模式

三种直连传输共享上层（业务接口 + `*Connection` + `jsonrpc.Conn`），只在 L1 传输层不同。下面分别画出各自的链路。

#### 4.1.1 WebSocket（全双工长连接）

```
┌────────────────────────┐                                   ┌────────────────────────┐
│  Client 进程            │                                   │  Agent 进程             │
│                        │                                   │                        │
│  acp.Client (业务)      │                                   │  acp.Agent (业务)       │
│       ▲                │                                   │       ▲                │
│       │ 派发/出站        │                                   │       │ 派发/反向 RPC    │
│  ClientConnection      │                                   │  AgentConnection       │
│       ▲                │                                   │       ▲                │
│  jsonrpc.Conn          │                                   │  jsonrpc.Conn          │
│       ▲                │                                   │       ▲                │
│  ws.Transport          │◄═══════ WS 单连接，双向帧 ═══════►│  ws.Transport          │
│  (hertz-contrib/ws)    │          ws://host/acp            │  (服务端 upgrader)      │
└────────────────────────┘                                   └────────────────────────┘

特点：一条 TCP 连接承载双向全部流量（请求 + 响应 + 反向 RPC + 通知），无需粘滞路由。
```

#### 4.1.2 Streamable HTTP（POST + SSE + DELETE）

```
┌────────────────────────┐                                   ┌────────────────────────────────┐
│  Client 进程            │                                   │  Agent 服务 (ACPServer)         │
│                        │                                   │                                │
│  acp.Client            │                                   │  acp.Agent                     │
│       ▲                │                                   │       ▲                        │
│  ClientConnection      │                                   │  AgentConnection               │
│       ▲                │                                   │       ▲                        │
│  jsonrpc.Conn          │                                   │  jsonrpc.Conn                  │
│       ▲                │                                   │       ▲                        │
│  http.Client           │   POST   /acp  (JSON-RPC req) ──► │  connTable[Acp-Connection-Id] │
│  (cookie jar)          │ ◄── 200 JSON  或  SSE response ── │  + pending requests queue     │
│                        │                                   │                                │
│                        │   GET    /acp  (建 SSE 反向通道) ─►│  反向 Request/Notification     │
│                        │ ◄══ session/update · fs/read ══   │  通过 SSE 事件流推回            │
│                        │                                   │                                │
│                        │   DELETE /acp  (关闭连接) ───────► │  从 connTable 删除 + 清理       │
│                        │   Header: Acp-Connection-Id       │                                │
└────────────────────────┘                                   └────────────────────────────────┘

特点：一条"逻辑连接"拆成多次 HTTP 请求，服务端用 connTable 按 Acp-Connection-Id 粘合；
      负载均衡必须做 **粘滞路由**，让同一 connID 的 POST/GET/DELETE 到同一后端实例。
```

#### 4.1.3 stdio（父子进程）

```
┌───────────────────────────┐                            ┌───────────────────────────┐
│  Client 进程（父）          │        fork / exec         │  Agent 进程（子）           │
│                           │ ─────────────────────────► │                           │
│  acp.Client               │                            │  acp.Agent                │
│       ▲                   │                            │       ▲                   │
│  ClientConnection         │                            │  AgentConnection          │
│       ▲                   │                            │       ▲                   │
│  jsonrpc.Conn             │                            │  jsonrpc.Conn             │
│       ▲                   │                            │       ▲                   │
│  stdio.Transport          │                            │  stdio.Transport          │
│    writer = child.Stdin   │ ══ NDJSON 请求/通知 ═══════►│    reader = os.Stdin      │
│    reader = child.Stdout  │ ◄══ NDJSON 响应/反向 RPC ══ │    writer = os.Stdout     │
└───────────────────────────┘                            └───────────────────────────┘

特点：父进程通过 exec.Cmd 拉起子进程，stdin/stdout 各承担一个方向；典型用于 IDE 本地拉起 Agent。
```

### 4.2 服务端多连接链路（`server.ACPServer`）

```
              ┌───────────────── ACPServer (Hertz) ─────────────────┐
              │   同一路由 /acp 同时处理 HTTP 和 WS upgrade            │
              │                                                    │
              │   ┌─────────────────┐       ┌───────────────────┐  │
  WS 客户端 ──►│   │  wsConns (map)  │       │  connTable        │  │◄── HTTP 客户端
              │   │  id → wsConn    │       │  (按 idle 回收)    │  │
              │   │  (长连接，无回收) │       │  connID →         │  │
              │   └────────┬────────┘       │   httpRemoteConn  │  │
              │            │                └─────────┬─────────┘  │
              │            └─────────┬────────────────┘            │
              │                      ▼ 每个连接一个                 │
              │          conn.AgentConnection                      │
              │          (反向 RPC 句柄，注入给 Agent)                │
              │                      ▲                             │
              │          AgentFactory().(ConnectionAwareAgent)     │
              └────────────────────────────────────────────────────┘
```

- `connTable` **只存 HTTP 远端连接**（[conn_table.go](file:///data00/home/fanlv/acp/server/conn_table.go#L13-16) 明确注释 *WebSocket connections are intentionally not tracked here*），因为 Streamable HTTP 需要按 `Acp-Connection-Id` 把多次 POST/GET/DELETE 粘合成同一条逻辑连接，且需要 idle reaper 回收客户端消失后遗留的会话。
- **WS 连接**由独立的 `wsConns` 管理：WS 本身就是长连接，生命周期跟随 TCP，不需要 idle 回收表；它只是为了在 `ACPServer.Shutdown` 时能集中关闭。
- `AgentFactory` 为每条远端连接创建一个 Agent 实例；若 Agent 实现 `ConnectionAwareAgent.SetClientConnection(*AgentConnection)`，服务端自动注入连接句柄，Agent 就能反向调用 Client（如 `fs/read_text_file`、`session/update`、`session/request_permission`）。

### 4.3 Proxy 透传链路（`proxy.ACPProxy`）

```
┌────────┐   WS bytes   ┌─────────────────┐   Streamer   ┌──────────────────┐
│ Client │◄────────────►│  ACPProxy       │◄────────────►│ Upstream         │
│        │  /acp (WS)   │  up-pump        │  用户自定义    │ AgentServer      │
│        │              │  down-pump      │  gRPC/Kitex/  │ (例: stdio → Agent)
│        │              │  HeaderForwarder│  WS/...       │                  │
└────────┘              └─────────────────┘              └──────────────────┘

          Proxy 只是字节管道，不解析 ACP；一条上行 WS ↔ 一个 Streamer ↔ 一个下游会话
```

- 仅支持北向 WebSocket。
- 下游通常是使用者自建的 RPC 服务；典型实现里下游把收到的字节喂给 `stdio.Transport`，再由 `conn.NewAgentConnectionFromTransport` 驱动 `acp.Agent`。
- Proxy 与 `ACPServer` 默认都挂在 `/acp`，**同一 Hertz 进程只能有其一**。

---

## 5. 一次请求的内部流转（以 WebSocket 为例）

以 Client 调用 `Prompt(ctx, req)` 为例，一次「前向请求 + 等待响应」的完整链路如下（编号代表时间顺序）：

```
 ┌──────────────────────── Client 进程 ────────────────────────┐         ┌──────────────────────── Agent 进程 ────────────────────────┐
 │                                                            │         │                                                            │
 │  ① 应用代码（调用方 goroutine，此处会阻塞等响应）              │         │  ⑥ worker pool goroutine                                    │
 │     resp, err := cc.Prompt(ctx, req)                       │         │     dispatchRequest(method → handler)                      │
 │                  │                                         │         │     → agent.Prompt(ctx, req)  ◄─── 业务方法真正执行         │
 │                  ▼                                         │         │                  ▲                                          │
 │  ② ClientConnection.Prompt (生成代码)                       │         │                  │ 按 method 名查派发表、解码 params          │
 │     jsonrpc.SendRequestTyped[PromptResponse](...)          │         │                                                            │
 │                  │ 编码 params，分配 request id              │         │  ⑤ readLoop 把分类后的消息分发：                              │
 │                  ▼                                         │         │     · Response   → 就地 O(1) 唤醒 pending（见 ⑧）            │
 │  ③ jsonrpc.Connection.SendRequest (调用方 goroutine 内同步)  │         │     · Request    → 推入 intake 队列 → worker pool            │
 │     - 注册 pending[id] = chan response                     │         │     · 无序 Notify → 推入 intake 队列 → worker pool           │
 │     - transport.WriteMessage(ctx, envelope)                │         │     · 有序 Notify → 推入 ordered 队列 → 单 goroutine 串行      │
 │     - 然后调用方 goroutine 在 pending chan 上阻塞等响应       │         │                  ▲                                          │
 │                  │                                         │         │                  │                                          │
 │                  ▼                                         │         │  ④ readLoop goroutine（jsonrpc.Connection 启动时 go 出来）    │
 │  ws.Transport.WriteMessage                                 │         │     for { data := transport.ReadMessage(ctx)               │
 │  （WS 帧序列化 + 写 socket）                                  │         │            json.Unmarshal(data, &msg) }                    │
 │                  │                                         │         │                  ▲                                          │
 └──────────────────┼─────────────────────────────────────────┘         └──────────────────┼──────────────────────────────────────────┘
                    │                                                                      │
                    └──────────────── 同一条 WS 全双工连接 ─────────────────────────────────┘
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

**反向 RPC 同理**：Agent 侧调用 `AgentConnection.ReadTextFile / SessionUpdate / RequestPermission` 走一模一样的 ①—⑩ 流程，只是方向反过来（Agent → Client），复用**同一条 WS 连接**。

**几个关键事实（对照 [internal/jsonrpc/connection.go](file:///data00/home/fanlv/acp/internal/jsonrpc/connection.go#L155-186) 源码注释）：**

1. **只有一个常驻 goroutine：`readLoop`**。它同步调用 `transport.ReadMessage` 拉一条消息，就地分类；**没有** "writeLoop / outbox" —— 写方向由调用方 goroutine 直接 `transport.WriteMessage`，是否在传输层再排队由具体 Transport 自己决定（例如 WS 实现内部可能有自己的发送 channel）。
2. **readLoop 绝不阻塞在 handler 上**：
   - `Response` 就地 O(1) 处理（查 `pending` sync.Map 并送值），不经过任何队列；
   - `Request` / 无序 `Notification` 入 **intake 无界队列**，由固定大小的 **worker pool** 并发消费；
   - 有序 `Notification`（例如 `session/update`，必须保序）入 **另一条单消费者队列**，由单独的 drain goroutine 串行执行。
3. **请求–响应配对靠 `pending` (`sync.Map`)**：发请求时以 JSON-RPC id 为 key 注册一个 `chan response`，收到响应时按 id 唤醒调用方。Notification 没有 id，发完即忘。
4. **L2（jsonrpc）不认识 ACP 方法语义**，只管 envelope、id 分配、pending 匹配、队列与 worker pool；**L3（`*Connection`）** 才知道 `acp.MethodAgentPrompt` 这类方法常量与参数类型，负责编解码 + 注册 handler。

> Streamable HTTP 和 stdio 的核心调度模型与此相同（同一个 `jsonrpc.Connection` + readLoop + worker pool），仅 L1 字节通道不同（HTTP 由 ACPServer 端把每个 POST/GET/DELETE 粘合回同一个 `AgentConnection`；stdio 直接用父子进程的 stdin/stdout）。

关键点：
1. **方法派发完全由代码生成**（`cmd/generate` 读 `schema.json` 产出 `types_gen.go`、`handlers_gen.go`、`*_outbound_gen.go`、`methodmeta`）。人手写的是 `BaseAgent/BaseClient` 的默认行为、`conn.dispatch` 的骨架、以及传输层。
2. **L2 与 L3 严格解耦**：`internal/jsonrpc` 不认识 ACP 方法语义，只管 envelope + 队列 + pending response 匹配；`conn.*Connection` 才知道方法元数据、session 作用域等。
3. **Transport 只管字节**：增加一种新传输只需要实现 `transport.Transport` 的 `Send/Recv/Close`。

---

## 6. 目录速查

| 路径 | 作用 |
|---|---|
| `*.go` (根目录) | 协议类型、接口、BaseAgent/BaseClient、错误、扩展方法 |
| `conn/` | `AgentConnection` / `ClientConnection` 的派发与反向 RPC |
| `transport/{stdio,ws,http/client}` | 三种客户端传输实现 |
| `internal/jsonrpc` | JSON-RPC 2.0 引擎（envelope / queue / connection） |
| `internal/httpserver` | 服务端 Streamable HTTP（POST + SSE）适配 Hertz |
| `internal/wsserver` | 服务端 WebSocket 适配 Hertz |
| `internal/connspi`, `peerstate`, `methodmeta`, `endpoint`, `safe`, `log` | 内部 SPI、状态机、方法元数据、路径处理、公共工具 |
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
