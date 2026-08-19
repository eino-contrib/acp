# ACP Go SDK 架构总览

本文档帮助新贡献者快速理解 SDK 的分层、框架边界、运行链路与生命周期。

---

## 1. 项目定位

`github.com/eino-contrib/acp` 是 [Agent Client Protocol (ACP)](https://agentclientprotocol.com/) 的 Go SDK。ACP 是基于 JSON-RPC 2.0 的双向 RPC 协议，在 Client（宿主或 IDE）与 Agent（AI 服务）之间传递 prompt、session 更新、文件系统调用、权限请求等消息。

应用实现业务接口 `acp.Agent` 与 `acp.Client`；SDK 提供协议派发、请求响应匹配、传输、多连接服务状态、代理与关闭协调。

---

## 2. 整体架构

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

stdio 与 WebSocket 的两端都使用 `jsonrpc.Connection`。Streamable HTTP 是非对称的：客户端传输仍接入常规 JSON-RPC connection；服务端直接派发 POST 消息，并通过每个 session 的 GET SSE 流发送反向消息。

---

## 3. 核心抽象速记

| 抽象 | 所在包 | 职责 |
|---|---|---|
| `acp.Agent` / `acp.Client` | 根包 | 由用户实现的业务接口 |
| `BaseAgent` / `BaseClient` | 根包 | 对未实现的 request 与 notification 给出明确错误的默认实现 |
| `conn.AgentConnection` | `conn/` | Agent 侧类型化派发，以及对 Client 的反向调用 |
| `conn.ClientConnection` | `conn/` | Client 侧类型化派发，以及对 Agent 的调用 |
| `transport.Transport` | `transport/` | 字节消息契约：`ReadMessage`、`WriteMessage`、`Close` |
| `server.ACPServer` | `server/` | 框架无关的 Streamable HTTP 与 WebSocket 运行时 |
| `proxy.ACPProxy` | `proxy/` | 框架无关的北向 WebSocket 字节代理 |
| `stream.Streamer` / `StreamerFactory` | `stream/` | 用户定义的 Proxy 到 AgentServer 的南向连接 |
| Server adapters | `server/hertz`、`server/gin` | 把一个 `ACPServer` core 适配为框架原生 handler |
| Proxy adapters | `proxy/hertz`、`proxy/gin` | 把一个 `ACPProxy` core 适配为框架原生 WebSocket handler |

Server 与 Proxy core 持有协议状态、admission、连接、限制与生命周期，但不持有 HTTP router 或路由路径。WebSocket 的 origin、压缩、buffer、subprotocol 与原生 upgrader 配置留在 adapter。

---

## 4. 框架边界与路由注册

宿主应用把框架原生 handler 注册到 `server.DefaultEndpoint` 或 `proxy.DefaultEndpoint`（两者惯例值都是 `/acp`），也可以直接注册到应用选择的任意自定义路由。自定义路径属于 router 配置，不属于 core 配置。Server 与 Proxy 若共处一个进程，应注册到不同路由。

| 角色 | Hertz handler | Gin handler |
|---|---|---|
| Agent server | `server/hertz.New(core)` | `server/gin.New(core)` |
| 字节代理 | `proxy/hertz.New(core)` | `proxy/gin.New(core)` |

典型注册方式是显式的：

```go
// Hertz ACPServer（Streamable HTTP + WebSocket）
srv := hertzserver.New(
    hertzserver.WithHostPorts(":8080"),
    hertzserver.WithStreamBody(true),
)
srv.NoHijackConnPool = true
srv.Any(acpserver.DefaultEndpoint, acphertz.New(core))

// Gin
router.Any(acpserver.DefaultEndpoint, acpgin.New(core))
```

- **Hertz Streamable HTTP：**首选启用 `WithStreamBody(true)`，让 ACP adapter 在读取 chunked 或未知长度 body 时执行 `server.WithMaxHTTPMessageSize`。否则 Hertz 会先缓冲，并在 handler 前应用宿主上限（默认 4 MiB），从而可能抢先于 SDK 默认 10 MiB 上限及其 413 语义拒绝请求。若无法启用流式 body，则把 `WithMaxRequestBodySize(...)` 设为不小于 SDK 上限，但要接受 Hertz 可能缓冲完整 body。这些配置作用于整个宿主。
- **Hertz：**任何提供这些 WebSocket handler 的宿主都必须设置 `NoHijackConnPool = true`，否则 Hertz 可能在 upgrade 后回收 hijack 连接。Server 与 Proxy adapter 都有此要求。
- **Gin：**adapter 使用 Gin 底层标准 `net/http` 请求响应链路和 Gorilla WebSocket。WebSocket 支持的是 HTTP/1.1 upgrade 流程，不承诺 RFC 8441 的 HTTP/2 WebSocket。
- **两者共有：**握手检查位于 `internal/wsupgrade`；原生连接包装在 `internal/wsconn` 后面。Adapter option 配置 Hertz 或 Gorilla upgrader，不让框架类型进入任一 core。

---

## 5. 端到端链路

### 5.1 WebSocket

```text
ClientConnection -> jsonrpc.Connection -> transport/ws
       <============== one full-duplex WebSocket ==============>
adapter -> server WebSocket admission -> internal/wsserver -> AgentConnection
```

一条 TCP 连接承载 request、response、反向 RPC 与 notification。Server adapter 校验并升级请求，再把包装后的 `wsconn.Conn` 交给 core admission；该链路不使用 Streamable HTTP connection table，也不需要粘滞路由。

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

一条逻辑连接跨越多次 HTTP 请求，以 `Acp-Connection-Id` 为键；session 范围的反向消息按 `Acp-Session-Id` 路由。服务端 HTTP 链路使用直接派发与 `httpAgentSender`，不会创建服务端 `jsonrpc.Connection`。

为兼容已有客户端，POST 仍允许省略 `Accept`；但显式值的最终协商结果必须同时允许 `application/json` 和 `text/event-stream`。媒体范围按具体度决定优先级，`q=0` 表示不可接受。GET 遵循更严格的 Active RFD 规则：必须显式接受 `text/event-stream`，否则服务端会在查找 connection/session 或输出 SSE 之前返回 `406 Not Acceptable`。

### 5.3 stdio

```text
Parent ClientConnection -> stdin  -> child AgentConnection
Parent ClientConnection <- stdout <- child AgentConnection
```

父进程启动 Agent 子进程，并通过 stdin/stdout 交换按行分隔的 JSON-RPC。两端都使用标准 `jsonrpc.Connection` 运行时。

### 5.4 Proxy

```text
ACP Client <-- northbound WebSocket --> ACPProxy <-- Streamer --> AgentServer
                    up pump / down pump; byte payloads only
```

Proxy 不解析 ACP，也不持有 Streamable HTTP 状态。每个获准的北向 WebSocket 对应一个下游 `Streamer`。上行 pump 接受 text 或 binary data frame，并原样转发其 payload 字节；由于 `Streamer` 只携带 payload、不携带 WebSocket frame type，下行 pump 始终把下游 payload 写成 text frame。Control frame 保留在 WebSocket 边界。`proxy.WithMetadataExtractor` 生成传给 `StreamerFactory.NewStreamer` 的 metadata；`proxy.ForwardHeaders(...)` 是选择性转发鉴权、trace 或 tenant header 的便捷 extractor。

---

## 6. 连接归属与 Core 生命周期

### 6.1 Server 状态

```text
ACPServer
├── HTTP connTable: connection id -> httpRemoteConnection (idle-reaped)
│   └── sessions: pending buffer + at most one active GET SSE writer each
└── WebSocketAdmission registry: pending upgrades and active WS handlers
    └── internal/wsserver.Transport over wsconn.Conn
```

`AgentFactory` 为每条远端逻辑连接创建一个 Agent。若该 Agent 实现 `ConnectionAwareAgent`，core 会注入对应的 `AgentConnection`，供其发起反向 request 与 notification。HTTP table 把多次请求粘合成逻辑连接并回收空闲状态；WebSocket 生命周期由 admission 跟踪，其中包括 upgrade 前和 upgrade 中的竞态窗口。

### 6.2 Proxy 状态

`ACPProxy` 从接收 admission 开始，一直跟踪 upgrade、下游 factory 创建、活跃 pump 到清理完成。Admission registry 同时执行连接数限制，并防止 upgrade 或 factory 调用尚未退出时误报 shutdown 已完成。

### 6.3 `Close` 与 `Shutdown`

`ACPServer` 与 `ACPProxy` 具有相同的生命周期形态：

- `Close()` 幂等地拒绝新 admission、广播取消、关闭活跃资源并启动异步收敛；它刻意不等待 handler、WebSocket、Streamer 或用户 factory 返回。
- `Shutdown(ctx)` 触发同一条 close 链路，然后等待 core registry 排空；若调用方 deadline 先到，则返回 `ctx.Err()`。
- Framework adapter 没有独立生命周期。应用先调用 core `Close`，再关闭 Hertz 或 `net/http` 宿主，最后调用 core `Shutdown(ctx)` 确认 registry 收敛；仅关闭宿主 server 未必能覆盖所有 hijack WebSocket。

---

## 7. Streamable HTTP 部署约束

Streamable HTTP 依赖真正的 SSE 流式传输。其前置反向代理或负载均衡必须相应配置：

1. **关闭 SSE 响应缓冲。** 立即 flush `text/event-stream` 数据；关闭 proxy buffering、缓存、会合并 chunk 的压缩转换，以及等待完整响应体的中间件。
2. **允许长时间 GET 响应。** GET SSE listener 本来就是长连接，并定期接收 keepalive comment。Proxy 的 read/idle timeout 必须大于预期 SSE 生命周期，或对该路由关闭。
3. **允许长时间 POST 响应。** Request POST 可能一直打开到 Agent 以 SSE 返回最终结果。应对齐 ingress、反向代理、宿主 server、SDK request 与应用 deadline，避免外层 timeout 先截断合法任务。
4. **使用粘滞路由。** 携带同一 `Acp-Connection-Id` 的 POST、GET、DELETE 必须到达同一后端进程，因为逻辑连接与 session 状态保存在内存中。多副本部署需要 cookie affinity、connection-id hash 或其他确定性策略。
5. **保留 ACP header 与 method。** 转发 `Acp-Connection-Id`、`Acp-Session-Id`、协议版本 header、`Accept` 与 `Content-Type`，并允许所选路由上的 POST、GET、DELETE。

这些约束来自 Streamable HTTP 协议，与使用 Hertz 还是 Gin 无关。

---

## 8. 内部请求派发

对 stdio 与 WebSocket，`jsonrpc.Connection` 使用一个 read loop 分类消息。Response 按 JSON-RPC id 唤醒对应 pending request；request 与无序 notification 进入 worker queue；有序 notification 进入单消费者队列。写操作由 caller 或 worker goroutine 直接经过 transport，不存在对称的全局 write loop。类型化 ACP 方法语义位于更上层的 `conn.*Connection`。

对服务端 Streamable HTTP，`internal/httpserver` 解析并校验 POST/GET/DELETE，管理 connection/session 状态并直接调用 Agent dispatcher。POST request 的最终结果以 SSE 返回；服务端发起的消息通过 session 的 GET SSE writer 发送。Pending/outbox buffer 与发送 timeout 都有上界，避免慢 listener 或缺失 listener 导致内存无限增长。

方法类型、接口、outbound call、handler 与方法元数据由 `cmd/generate/schema/schema.json` 生成；连接装配与传输运行时仍由手写代码维护。

---

## 9. 目录速查

| 路径 | 作用 |
|---|---|
| 根目录 `*.go` | ACP 类型、接口、基础实现、错误、扩展与日志 |
| `conn/` | 类型化 Client/Agent connection、派发与反向 RPC |
| `transport/stdio`、`transport/ws`、`transport/http/client` | 具体的客户端或直连传输 |
| `internal/jsonrpc` | 通用 JSON-RPC connection、队列、pending 匹配与 worker |
| `internal/httpserver` | 框架无关的 Streamable HTTP 协议与 `HandlerContext`，以及 Hertz 和标准 `net/http` context bridge |
| `internal/wsserver` | 基于公共连接契约的服务端 WebSocket transport |
| `internal/wsconn` | 公共 WebSocket 契约、Hertz/Gorilla wrapper 与错误归一化 |
| `internal/wsupgrade` | 公共 RFC 6455 HTTP/1.1 握手识别与校验 |
| `server/` | 框架无关的 `ACPServer` 状态与生命周期 |
| `server/hertz`、`server/gin` | 宿主框架 Server adapter |
| `proxy/` + `stream/` | 框架无关的字节代理与下游 Streamer 抽象 |
| `proxy/hertz`、`proxy/gin` | 宿主框架 Proxy adapter |
| `cmd/generate/` | schema 驱动的代码生成 |
| `examples/{agent,client,proxy}` | 各角色的可运行示例 |

---

## 10. 阅读入口建议

- **Agent 服务：**`examples/agent`、`server/server.go`、`server/{hertz,gin}` 之一，再看 `conn/agent.go`。
- **Client 或宿主：**`examples/client`、`conn/client.go`，再看 `transport/{ws,http/client,stdio}`。
- **网关：**`examples/proxy`、`proxy/proxy.go`、`proxy/{hertz,gin}` 之一，再看 `stream/streamer.go`。
- **框架边界：**`internal/httpserver`、`internal/wsupgrade` 与 `internal/wsconn`。
- **协议变更：**`cmd/generate` 与 `cmd/generate/schema/schema.json`。
