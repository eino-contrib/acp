# ACP Go SDK 接入文档

## 1. 背景

`github.com/eino-contrib/acp` 是 [Agent Client Protocol](https://agentclientprotocol.com/) 的 Go 语言 SDK，提供：

- **协议类型代码生成**：从官方 `schema.json` / `meta.json` 自动生成 `Agent`、`Client` 接口和所有消息结构体；
- **双向 RPC 封装**：`conn.ClientConnection` / `conn.AgentConnection` 屏蔽 JSON-RPC 2.0 细节；
- **三套传输层**：`stdio`（子进程）、Streamable HTTP（POST + SSE）、WebSocket；HTTP/WS 服务端基于 [CloudWeGo Hertz](https://github.com/cloudwego/hertz) 实现；
- **远端 Server**：`server.ACPServer` 一条路由同时支持 HTTP 和 WebSocket 升级；
- **透明 Proxy**：`proxy.ACPProxy` 负责把外部 WS 流量透传到下游（用户自定义 RPC 实现的 AgentServer）；
- **扩展协议**：支持 `_` 前缀的自定义 Request / Notification（[ACP Extensibility](https://agentclientprotocol.com/protocol/extensibility#custom-requests)）。

## 2. 安装

```bash
go get github.com/eino-contrib/acp@latest
```

环境要求：

- Go **1.24+**
- 模块路径：`github.com/eino-contrib/acp`

## 3. 核心概念

### 3.1 角色

| 角色 | 对应类型 | 职责 |
| --- | --- | --- |
| **Agent** | `acp.Agent` 接口 | 接收客户端 Prompt、管理 Session、向客户端反向调用（读文件、请求权限、Terminal 等） |
| **Client** | `acp.Client` 接口 | 发起 Prompt、接收 `session/update` 等流式通知 |
| **Proxy** | `proxy.ACPProxy` + 用户实现的 `stream.StreamerFactory` | 承接北向 Client WebSocket 流量，按字节透明转发到下游 AgentServer（不解析 ACP 协议）；只做 WS 北向入口，负责鉴权 header 转发、心跳、并发/超时控制 |

`BaseAgent` / `BaseClient` 对所有「未实现方法」默认返回 `method not found`（-32601）或 `notification handler not implemented`——不是静默成功，而是 **主动报错**。业务方按需覆盖需要支持的方法。

Agent / Client 是 **协议端点**（解析 JSON-RPC、处理方法调用），Proxy 是 **透传节点**（只搬字节、不看协议），三者定位互不重叠。

### 3.2 连接

- `conn.NewClientConnection(client, transport, opts...)`：Client 侧连接。
- `conn.NewAgentConnectionFromTransport(agent, transport, opts...)`：Agent 侧连接（基于有读循环的传输：stdio / WebSocket）。
- HTTP 服务端不需要调用 `NewAgentConnectionFromTransport`：`server.ACPServer` 内部会自动构造每条连接的 `AgentConnection`，并通过 `ConnectionAwareAgent` 接口（`SetClientConnection(*conn.AgentConnection)`）注入到 Agent，Agent 实现该接口即可拿到本连接用于反向调用（见 [4.1.1 Agent（Server）](#411-agentserver)）。

### 3.3 Import alias 约定

下文代码示例统一使用以下 import alias，后续示例不再重复 import 语句：

```go
import (
    acp         "github.com/eino-contrib/acp"
    acpconn     "github.com/eino-contrib/acp/conn"
    acpserver   "github.com/eino-contrib/acp/server"
    acpproxy    "github.com/eino-contrib/acp/proxy"
    acpstream   "github.com/eino-contrib/acp/stream"
    stdio       "github.com/eino-contrib/acp/transport/stdio"
    acphttpclient "github.com/eino-contrib/acp/transport/http/client"
    acpws       "github.com/eino-contrib/acp/transport/ws"
)
```

表格里写的 `conn.WithXxx` / `server.WithXxx` 等是裸包名，对应到代码示例里就是 `acpconn.WithXxx` / `acpserver.WithXxx`。

## 4. 快速开始

下面给出四套最常见的组合：

1. **WebSocket 模式**：远端 `ACPServer` 暴露 Agent，Client 通过 WebSocket 连接。
2. **Streamable HTTP 模式**：远端 `ACPServer` 走 HTTP（POST + SSE），Client 通过 HTTP 连接并用 SSE 接收反向消息。
3. **stdio 子进程模式**：Client spawn Agent 子进程，通过 stdin/stdout 通信。
4. **Proxy 模式**：Proxy 节点承接北向 Client WebSocket，并把字节流透明转发到下游 AgentServer（你实现的 `stream.StreamerFactory`）。

先编译一次得到 `bin/agent` / `bin/client` / `bin/proxy`：

```bash
make build
```

### 4.1 WebSocket 模式

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
#### 4.1.1 Agent（Server）

完整 Demo 直接看仓库示例：

- Agent 实现：[`examples/agent/agent.go`](./examples/agent/agent.go)
- Hertz 挂载与入口：[`examples/agent/main.go`](./examples/agent/main.go)

> ⚠️ **Hertz WebSocket 必须设置 `srv.NoHijackConnPool = true`**，否则 upgrade 后 Hertz 会回收连接导致 WS 立即断开。

#### 4.1.2 Client

完整 Demo 直接看仓库示例：

- Client 实现：[`examples/client/client.go`](./examples/client/client.go)
- WebSocket 连接入口：[`examples/client/main.go`](./examples/client/main.go)（`-transport=ws`）

#### 4.1.3 运行 Demo

```bash
# 终端 A：启动 Agent（HTTP + WS 同一路由 /acp，监听 :18080）
./bin/agent -transport=http -listen=:18080

# 终端 B：Client 用 WebSocket 连上
./bin/client -transport=ws ws://127.0.0.1:18080

# 一键跑（同进程串行起 agent + client，结束后自动清理）
make run-ws
# 自定义端口：make run-ws AGENT_ADDR=:9090
```

### 4.2 Streamable HTTP 模式

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

> ⚠️ **重要：需要 sticky 路由（会话粘滞）**
>
> Streamable HTTP 会同时使用：
> - `POST /acp` 发送请求（以及回响应）
> - `GET /acp` 建立 SSE 反向通道（用于接收 Agent→Client 的反向 Request/Notification）
>
> 如果你在负载均衡 / 反向代理后面部署，必须保证同一个 ACP 连接的 `POST /acp` 和 `GET /acp` 会命中**同一台**后端服务（例如基于 cookie 的 sticky、header hash、或按 `Acp-Connection-Id` 做一致性路由）。否则会出现连接状态不一致，导致反向消息收不到或请求失败。

#### 4.2.1 Agent（Server）

`ACPServer` 同时支持 WebSocket 和 Streamable HTTP，两者复用同一条路由（默认 `/acp`），所以服务端实现无需改动，直接复用 [4.1.1 Agent（Server）](#411-agentserver) 的代码即可。

#### 4.2.2 Client

完整 Demo 直接看仓库示例：

- Client 实现：[`examples/client/client.go`](./examples/client/client.go)
- HTTP + SSE 连接入口：[`examples/client/main.go`](./examples/client/main.go)（`-transport=http`）

#### 4.2.3 运行 Demo

```bash
# 终端 A：Agent 照样起 HTTP（与 WS 共用同一个二进制）
./bin/agent -transport=http -listen=:18080

# 终端 B：Client 走 HTTP + SSE
./bin/client -transport=http http://127.0.0.1:18080

# 一键跑
make run-http
```

### 4.3 stdio 子进程模式

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
        父进程 spawn ─────────────── fork/exec ────────────► 子进程
```

#### 4.3.1 Client（父进程 spawn 子进程）

Client 方 spawn 子进程并用它的 stdin/stdout 做通信（可复用上面 WebSocket 模式里的 `Client` 实现）：

完整 Demo 直接看仓库示例：

- Client 实现：[`examples/client/client.go`](./examples/client/client.go)
- spawn 子进程入口：[`examples/client/main.go`](./examples/client/main.go)（`-transport=spawn`）

#### 4.3.2 Agent（子进程内）

Agent 侧（在子进程内，`agent` 为你的 Agent 实例，例如 `&Agent{}`）：

完整 Demo 直接看仓库示例：

- Agent 实现：[`examples/agent/agent.go`](./examples/agent/agent.go)
- stdio 入口：[`examples/agent/main.go`](./examples/agent/main.go)（`-transport=stdio`）

#### 4.3.3 运行 Demo

```bash
# Client 直接 spawn Agent 子进程，通过 stdin/stdout 通信
./bin/client -transport=spawn ./bin/agent

# 一键跑
make run-stdio
```

### 4.4 Proxy 模式

```
┌────────────────────┐            ┌──────────────────────────┐            ┌──────────────────────────┐
│      Client        │            │    Proxy (ACPProxy)      │            │   Upstream AgentServer   │
│                    │            │                          │            │                          │
│  ┌──────────────┐  │            │  ┌────────────────────┐  │            │  ┌────────────────────┐  │
│  │ acp.Client   │  │            │  │ Hertz /acp WS      │  │            │  │ user RPC           │  │
│  │ BaseClient   │  │            │  │                    │  │            │  │ (gRPC / Kitex /    │  │
│  └──────────────┘  │            │  │  up-pump           │  │            │  │  WS / ...)         │  │
│         ▲          │            │  │  down-pump         │  │            │  └────────────────────┘  │
│         │          │  WS bytes  │  └────────────────────┘  │  Streamer  │            │             │
│         │          ├───────────►│                          ├───────────►│            ▼             │
│         │          │◄───────────┤  HeaderForwarder         │◄───────────┤  ┌────────────────────┐  │
│  ┌──────┴───────┐  │            │  WS keepalive            │            │  │ AgentConnection    │  │
│  │ ws.Transport │  │            │  Max-conn cap            │            │  │ acp.Agent          │  │
│  └──────────────┘  │            │                          │            │  │ BaseAgent          │  │
│                    │            │                          │            │  └────────────────────┘  │
└────────────────────┘            └──────────────────────────┘            └──────────────────────────┘

                          Proxy 只看字节，不解析 ACP 协议
                          一条 Client WS ↔ 一个 Streamer ↔ 一条下游会话
```

完整 Demo 直接看仓库示例：

- Proxy 入口：[`examples/proxy/main.go`](./examples/proxy/main.go)
- Proxy 运行逻辑：[`examples/proxy/proxy_runner.go`](./examples/proxy/proxy_runner.go)
- 上游 AgentServer：[`examples/proxy/agent_server.go`](./examples/proxy/agent_server.go)
- 示例 `StreamerFactory`：[`examples/proxy/factory.go`](./examples/proxy/factory.go)
- 示例 `Streamer`：[`examples/proxy/ws_streamer.go`](./examples/proxy/ws_streamer.go)
- 示例 Agent：[`examples/proxy/echo_agent.go`](./examples/proxy/echo_agent.go)

> ⚠️ 约束：
> - Proxy **只支持 WebSocket** 作为北向入口（不支持 Streamable HTTP）。
> - `ACPServer` 和 `ACPProxy` 默认都占用 `/acp`；如果要挂在同一套 Hertz 路由器上，必须显式改成不同 endpoint。
> - 仍然需要 `srv.NoHijackConnPool = true`，否则 WebSocket 会被 Hertz 回收导致断连。

Proxy 的作用是「只看字节，不看协议」：它把外部 Client 的 WS 数据帧转发给下游（通常是你自建的 AgentServer），下游再把字节喂给 ACP 的 stdio 传输，最终由 `acpconn.NewAgentConnectionFromTransport(...)` 驱动你的 Agent。

#### 4.4.1 下游 AgentServer（Upstream）

最小可运行示例（仓库内置）：启动一个 WS 上游，监听 `/acp-upstream`，供 Proxy dial：

```bash
./bin/proxy -role=agent-server -listen=:9090
```

#### 4.4.2 Proxy 节点（北向 /acp → 南向 upstream）

启动 Proxy（北向路径固定为 `/acp`），把每条入站 Client WS 连接转发到 `ws://127.0.0.1:9090/acp-upstream`：

```bash
./bin/proxy -role=proxy -listen=:8080 -upstream=ws://127.0.0.1:9090/acp-upstream
```

#### 4.4.3 Client（连接到 Proxy）

Client 侧仍然按 WebSocket 模式连接，只是把目标地址改成 Proxy（默认 endpoint path 仍为 `/acp`）：

完整 Demo 可直接复用：

- Client 实现：[`examples/client/client.go`](./examples/client/client.go)
- WebSocket 入口：[`examples/client/main.go`](./examples/client/main.go)（`-transport=ws`，目标地址改为 Proxy）

也可以一条命令本地跑全链路（同时起 upstream + proxy）：

```bash
./bin/proxy -role=all
```

#### 4.4.4 运行 Demo

```bash
# 方式一：分别起上游 AgentServer 和 Proxy，再起 Client
./bin/proxy -role=agent-server -listen=:9090                                      # 终端 A
./bin/proxy -role=proxy -listen=:8080 -upstream=ws://127.0.0.1:9090/acp-upstream  # 终端 B
./bin/client -transport=ws ws://127.0.0.1:8080                                    # 终端 C

# 方式二：同进程起 Proxy + 上游 AgentServer（role=all），再起 Client
./bin/proxy -role=all -proxy-listen=:8080 -agent-listen=:9090                     # 终端 A
./bin/client -transport=ws ws://127.0.0.1:8080                                    # 终端 B

# 一键跑全链路（agent-server + proxy + client 同进程编排）
make run-proxy
# 自定义端口：make run-proxy PROXY_LISTEN=:8080 PROXY_AGENT_LISTEN=:9090
```

## 5. 参数配置

### 5.1 连接配置

下面这组 `conn.With...` 是 **`conn.NewClientConnection(...)` 的公开 Option**（跨传输可用，适用于 WebSocket / stdio / Streamable HTTP 的 Client 侧）：

| Option | 默认 | 说明 |
| --- | --- | --- |
| `conn.WithRequestTimeout(d)` | 0 | 每个 inbound handler 的 ctx deadline；0 = 不限 |
| `conn.WithRequestWorkers(n)` | 8 | 每条连接的 worker pool 大小 |
| `conn.WithMaxConsecutiveParseErrors(n)` | 0 | 连续 N 次解析失败关连接（防御恶意 peer）；0 = 不限 |
| `conn.WithConnectionLabel(label)` | 空 | 给日志打上标签方便排查 |
| `conn.WithOrderedNotificationMatcher(fn)` | 内置 `session/update` | 指定哪些通知要**严格顺序**投递 |
| `conn.WithSessionListenerErrorHandler(fn)` | 内置 warn 日志 | HTTP GET SSE listener 失败回调（仅 HTTP） |
| `conn.WithNotificationErrorHandler(fn)` | 内置 error 日志 | 通知 handler 报错/panic 时的回调 |

> 注意：
> - `conn.WithSessionListenerErrorHandler` / `conn.WithOrderedNotificationMatcher` 是 **ClientConnection 专属**。
> - `conn.NewAgentConnectionFromTransport(...)` 也支持请求超时、worker 数、解析错误阈值、连接标签、通知错误回调等能力，但其参数类型是底层 `jsonrpc.ConnectionOption`，不是上表这组 `ClientConnectionOption`。

共享默认值（`transport` 包常量）：

| 常量 | 值 |
| --- | --- |
| `transport.DefaultMaxMessageSize` | 10 MB |
| `transport.DefaultInboxSize` | 1024 |
| `transport.DefaultOutboxSize` | 1024 |
| `transport.DefaultACPEndpointPath` | `/acp` |

使用示例：

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

### 5.2 客户端传输

#### 5.2.1 stdio

```go
import "github.com/eino-contrib/acp/transport/stdio"

t := stdio.NewTransport(reader, writer,
    stdio.WithMaxMessageSize(10*1024*1024), // 单条 NDJSON 上限，默认 10 MB
    stdio.WithInitialBufSize(64*1024),      // Scanner 初始 buffer，默认 64 KB
)
```

特点：

- **协议**：newline-delimited JSON（每条消息一行）。
- **启动策略**：`ReadMessage` 首次调用时才启动 read goroutine；`WriteMessage` 首次调用时才启动 writer goroutine。读写各一条独立 goroutine。
- **写超时**：如果调用方未给 ctx 设置 deadline，默认 **30s** 作为兜底；防止下游管道满时 handler 被永久阻塞。
- **并发安全**：`WriteMessage` 内部通过 `writeCh` 派给单独的 writer goroutine，所以多 goroutine 可以安全并发调用。
- **无 keepalive / 无 reconnect**：生命周期完全绑定子进程管道。子进程退出 → `ReadMessage` 返回 `io.EOF`。
- **Close**：幂等，关闭 reader/writer（如果它们实现了 `io.Closer`）。

**Client 侧使用：**

```go
cmd := exec.CommandContext(ctx, "/path/to/agent")
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
_ = cmd.Start()

// 注意：reader 要传子进程的 stdout，writer 要传子进程的 stdin
t := stdio.NewTransport(stdout, stdin)
conn := acpconn.NewClientConnection(client, t)
_ = conn.Start(ctx)
```

**Agent 侧使用：**

```go
t := stdio.NewTransport(os.Stdin, os.Stdout)
conn := acpconn.NewAgentConnectionFromTransport(agent, t)
if aware, ok := agent.(acpserver.ConnectionAwareAgent); ok {
    aware.SetClientConnection(conn)
}
_ = conn.Start(ctx)
<-conn.Done()
```

stdio Option / 默认值：

| Option | 默认 |
| --- | --- |
| `stdio.WithMaxMessageSize(n)` | 10 MB |
| `stdio.WithInitialBufSize(n)` | 64 KB |
| （内置）write timeout（无 deadline 时） | 30 s |

#### 5.2.2 Streamable HTTP

[Streamable HTTP 传输](https://agentclientprotocol.com/protocol/transports#streamable-http) 定义的模型：

- **请求**：`POST {endpoint}`，body 是 JSON-RPC 消息。
- **响应**：服务端通常返回 SSE（至少携带最终 JSON-RPC 响应）；客户端也兼容单个 JSON 响应作为 fallback。
- **反向通道**：`GET {endpoint}`，服务端通过 SSE 推送反向 Request / Notification；客户端通过 POST 回响应。
- **会话头**：`Acp-Connection-Id`、`Acp-Session-Id`、`Acp-Protocol-Version`。

SDK 提供：
- 客户端：`transport/http/client.ClientTransport`
- 服务端：`server.ACPServer`（HTTP + WS 复用，见 [5.3 服务端节点：ACPServer](#53-服务端节点acpserver)）

**客户端初始化：**

```go
import acphttpclient "github.com/eino-contrib/acp/transport/http/client"

t := acphttpclient.NewClientTransport("http://127.0.0.1:8080",
    acphttpclient.WithHTTPClient(http.DefaultClient),          // 可替换 HTTP client
    acphttpclient.WithClientEndpointPath("/acp"),              // 默认 /acp
    acphttpclient.WithCustomHeaders(map[string]string{"X-Token": "..."}),
    acphttpclient.WithInboxSize(1024),                         // 默认 1024
    acphttpclient.WithSSEReconnect(),                          // 开启 GET SSE 断线重连
    acphttpclient.WithSSEReconnectMaxAttempts(-1),             // 负数 = 不限次数
    acphttpclient.WithSSEReconnectBackoff(time.Second, 30*time.Second),
)

conn := acpconn.NewClientConnection(client, t)
_ = conn.Start(ctx)
```

内部行为：

- `conn.NewSession(...)` / `conn.LoadSession(...)` **自动启动 GET SSE listener**，业务不用关心反向通道何时就绪。
- Non-SSE JSON 响应上限 **8 MB**；SSE 单事件上限 **10 MB**；错误 body 只读前 **4 KB**（避免大 body 撑爆内存）。
- `WithSSEReconnect()` 打开后采用指数退避（默认 1s → 30s）。失败时把错误交给 `conn.WithSessionListenerErrorHandler` 注册的 handler，**不会** 把它当作 RPC 错误抛给调用方。

**Cookie / 鉴权：**

`ClientTransport` 内部绑定了 `net/http/cookiejar`，Server 下发的 `Set-Cookie` 会被保留用于后续 POST/GET，这样就能满足基于 cookie 的会话粘滞/鉴权。

如果需要注入 Authorization：

```go
t := acphttpclient.NewClientTransport("http://...",
    acphttpclient.WithCustomHeaders(map[string]string{
        "Authorization": "Bearer xxx",
        "X-Tenant-Id":   "acme",
    }),
)
```

> `WithCustomHeaders` 会 **Set**（覆盖）同名 header，而不是 Add。

**事件流程简述：**

```
Client                           Server
  | --- POST initialize ---->      |
  |   (返回 200 SSE 响应)   <------|  Acp-Connection-Id 回传
  | --- POST session/new ---->     |
  |   (返回 200 SSE 响应) <--------|  生成 SessionID
  | --- GET  (SSE stream) -->      |  开启反向推送通道
  |                         <------|  session/update 事件
  | --- POST session/prompt >      |
  |   (返回 200 SSE 响应)   <------|
```

HTTP 客户端 Option / 默认值 (`transport/http/client`)：

| Option | 默认 |
| --- | --- |
| `WithHTTPClient(c)` | `http.DefaultClient` |
| `WithClientEndpointPath(p)` | `/acp` |
| `WithCustomHeaders(m)` | 空 |
| `WithInboxSize(n)` | 1024 |
| `WithSSEReconnect()` | 关 |
| `WithSSEReconnectMaxAttempts(n)` | 不限 |
| `WithSSEReconnectBackoff(base, max)` | 1 s / 30 s |
| （内置）非 SSE JSON 上限 | 8 MB |
| （内置）SSE 单事件上限 | 10 MB |
| （内置）错误 body 读取上限 | 4 KB |

#### 5.2.3 WebSocket

**客户端初始化：**

```go
import acpws "github.com/eino-contrib/acp/transport/ws"

t, err := acpws.NewWebSocketClientTransport("ws://127.0.0.1:8080",
    acpws.WithEndpointPath("/acp"),                                   // 默认 /acp
    acpws.WithCustomHeaders(map[string]string{"X-Token": "..."}),
)
if err != nil { ... }

if err := t.Connect(ctx); err != nil { // 显式建立 WS 握手
    ...
}
conn := acpconn.NewClientConnection(client, t)
_ = conn.Start(ctx)
```

特点：

- **基于 Hertz**：客户端用 `hclient.Client` + `websocket.ClientUpgrader`，与服务端同一套生态。
- **URL 归一化**：支持 `http://` / `https://` / `ws://` / `wss://` / 甚至 `host:port` 纯地址；SDK 会自动补全 scheme（默认 `ws://`）和 endpoint path。
- **只用 origin**：`baseURL` 的 path / query / fragment 会被丢弃，最终 URL = `origin + endpointPath`。想改路径只能用 `WithEndpointPath`。
- **Cookie Jar**：握手请求会附带内置 `cookiejar`，并把响应里的 `Set-Cookie` 写回 jar。WS 每个 transport 实例只握手一次，jar 主要是为了接口一致性保留，实际用途有限。
- **写超时兜底**：调用方未给 ctx deadline 时，单次写默认 **30s** deadline；`Close` 尝试发 close frame 时仅等 **100ms** 抢写锁，抢不到就直接关 socket（避免被其他阻塞写卡死）。
- **Close 顺序**：`Close` 会先发送 close frame → 关 socket → 等 read loop 退出 → 释放 Hertz request/response 对象，保证无 use-after-free。
- **不自动重连**：业务方按需自行重建 transport + connection。

**服务端：**

WebSocket 服务端是 `server.ACPServer` 内置能力，见 [5.3 服务端节点：ACPServer](#53-服务端节点acpserver)。ACPServer 在同一条 `/acp` 路由下根据 `Upgrade: websocket` header 自动路由到 WS 升级器。

**常见坑位：**

1. **`srv.NoHijackConnPool = true`**：Hertz 默认会把 hijack 的连接送回池子，这会把 WebSocket 连接断开。部署 ACPServer 时**一定要**设置这个标志。
2. **超大帧**：服务端读限制 **10 MB**，超限直接关连接（1009 MessageTooBig）；客户端同限制。
3. **10 次连续解析失败**：WS 服务端连续 **10** 次 JSON-RPC 解析失败会主动关断连接，防止恶意 peer。
4. **并发写安全**：ACP 的 `Transport` 接口要求 `WriteMessage` 并发安全；WS 客户端内部用 `writePermit` 信号量实现互斥，业务方放心并发调用即可。

WebSocket 客户端 Option / 默认值 (`transport/ws`)：

| Option | 默认 |
| --- | --- |
| `WithEndpointPath(p)` | `/acp` |
| `WithCustomHeaders(m)` | 空 |
| （内置）单次写 deadline（无 ctx deadline 时） | 30 s |
| （内置）Close 抢写锁等待 | 100 ms |
| （内置）Close frame 写 deadline | 500 ms |

### 5.3 服务端节点：ACPServer

#### 5.3.1 参数详解

所有参数通过 `Option` 注入：

```go
remote, err := acpserver.NewACPServer(factory,
    acpserver.WithEndpoint("/acp"),
    acpserver.WithRequestTimeout(5 * time.Minute),
    acpserver.WithConnectionIdleTimeout(5 * time.Minute),
    acpserver.WithMaxHTTPMessageSize(10 * 1024 * 1024),
    acpserver.WithPendingQueueSize(1024),
    acpserver.WithMaxInflightDispatch(0), // 0 = 用内部默认；负数 = 不限
    acpserver.WithWebSocketUpgrader(websocket.HertzUpgrader{
        CheckOrigin: func(ctx *app.RequestContext) bool { return true },
    }),
    acpserver.WithNotificationErrorHandler(func(method string, err error) {
        metrics.Inc("acp_notify_err", method, err.Error())
    }),
)
```

| Option | 默认 | 说明 |
| --- | --- | --- |
| `WithEndpoint(path)` | `/acp` | 路由路径；自动规范化（补前导 `/`、去尾 `/`） |
| `WithRequestTimeout(d)` | 5 min | 单个 inbound handler 的 ctx deadline，同时作用于 HTTP POST 的最终响应等待时间与 WS `AgentConnection` 的每个请求处理；0 = 不限 |
| `WithConnectionIdleTimeout(d)` | 5 min | HTTP 连接空闲驱逐；0 或负值 = 不驱逐 |
| `WithMaxHTTPMessageSize(n)` | 10 MB | POST body 上限；超过返回 413 |
| `WithPendingQueueSize(n)` | 1024 | 会话创建后、GET SSE 建立前的消息缓冲 |
| `WithMaxInflightDispatch(n)` | 4096 | 单条 HTTP 连接并发 dispatch 上限；超限返回 503；负数 = 不限 |
| `WithWebSocketUpgrader(u)` | `websocket.HertzUpgrader{}` | 自定义 subprotocols / origin 校验 |
| `WithNotificationErrorHandler(fn)` | 无 | WS/stdio 通知失败回调（HTTP 不触发——HTTP direct-dispatch 无读循环，通知错只会记日志） |

内置（不可配）：

| 项 | 值 | 位置 |
| --- | --- | --- |
| SSE keepalive 注释间隔 | 30 s | `internal/httpserver/parse.go` |
| Idle-reaper 间隔 | `min(idleTimeout/2, 30s)` | `server/conn_table.go` |
| WS 读上限 | 10 MB | `internal/wsserver/server.go` |
| WS 最大连续解析错误 | 10 | `server/remote_conn_ws.go` |

#### 5.3.2 Streamable HTTP 路由规则

ACPServer 内部根据 HTTP 方法和 header 做路由：

| 方法 | 场景 | 行为 |
| --- | --- | --- |
| `POST /acp` | 新连接（无 `Acp-Connection-Id`） | 创建连接，返回响应头带新的 connection ID；body 是首条 JSON-RPC 请求 |
| `POST /acp` | 已有连接（带 `Acp-Connection-Id`） | 复用连接，将 body 直接投递给该连接 |
| `GET /acp` | 带 `Acp-Connection-Id` 和 `Acp-Session-Id` | 开启该 Session 的 SSE listener，用于服务端推送反向 Request/Notification |
| `DELETE /acp` | 带 `Acp-Connection-Id` | 关闭连接，释放资源 |

Pending queue（默认 1024）的作用：会话创建完成但客户端尚未开 GET SSE 前，服务端先把反向消息暂存，避免丢。客户端连上 GET 后会一次性下发。**超过 `WithPendingQueueSize` 配置的条数未消费**会关闭该 Session 并返回错误，而不是仅丢弃单条消息；业务方如果预期会有大量反向消息，请把 `WithPendingQueueSize` 调大。

### 5.4 代理节点：ACPProxy

`proxy.ACPProxy` 的定位：**只看字节，不看协议**。

用途：把外部 Client 的 WebSocket 流量转发到下游（通常是用户实现的 AgentServer RPC 服务）。常见场景：网关层、鉴权拦截、多租户路由、灰度。

#### 5.4.1 部署约束

> **`server.ACPServer` 与 `proxy.ACPProxy` 默认都使用 `/acp`。**如果挂在同一 Hertz 路由器上而不改 endpoint，会在路由注册阶段冲突；如果确实要共存，请显式改成不同路径。

#### 5.4.2 基本用法

```go
import (
    hertzserver "github.com/cloudwego/hertz/pkg/app/server"
    acpproxy    "github.com/eino-contrib/acp/proxy"
    acpstream   "github.com/eino-contrib/acp/stream"
)

func main() {
    factory := &MyStreamerFactory{...} // 实现 acpstream.StreamerFactory

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

#### 5.4.3 Streamer 接口

Proxy 把每条 Client WS 连接对接给一个 Streamer。Streamer 是 **双向字节管道**，由用户按自己的 RPC 框架实现（gRPC、Kitex、TTHeader、Thrift Streaming、WebSocket 到 AgentServer……）：

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

契约要点（务必遵守，否则行为不可预期）：

- **边界**：一次 `WritePayload` 对应另一端一次 `ReadPayload`，用户自己负责帧划分。
- **并发**：`WritePayload` 和 `ReadPayload` 可以来自两条 goroutine 并发调用。`Close` 也可以和它们并发。
- **Close 幂等**；触发后所有 in-flight read/write 必须尽快解除阻塞并返回错误。
- **不吞错**：网络错、认证错、peer close，都要**原样**返回。
- **不要自己加超时**：ctx 只约束当前调用；长连接生命周期完全依赖 `Close`。
- **clean close 返回 `io.EOF`**：Client 侧可用 `errors.Is(err, io.EOF)` 识别。

#### 5.4.4 HeaderForwarder

Proxy 自己不解析 ACP 协议，但需要转发鉴权 / 租户 / traceId 等 HTTP header 到下游：

```go
acpproxy.WithHeaderForwarder(acpproxy.ForwardHeaders("Authorization", "X-Tenant-Id", "X-Request-Id"))
```

或自定义：

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

注意：
- 回调运行在 Hertz handler 同 goroutine，**不要做耗时操作**。
- 返回的 map 之后归 Proxy 所有，回调方不要再修改。

#### 5.4.5 Keepalive & 连接健康

Proxy 实现了 WS 层心跳：

- 每 `WithWebSocketPingInterval`（默认 30s）发一次 Ping；
- `WithWebSocketPongTimeout`（默认 75s）没收到任何帧（data 或 pong）就关连接；
- **Pong 收到会刷新读 deadline**——正常连接永远不会超时。

> 设成 0 表示禁用 ping/pong 超时，半开连接会**永久**占用一个并发槽位，**不推荐**。

#### 5.4.6 背压与上限

| 维度 | 参数 | 默认 | 说明 |
| --- | --- | --- | --- |
| 最大并发连接 | `WithMaxConcurrentConnections(n)` | 10000 | 超限返回 503 |
| 握手超时 | `WithHandshakeTimeout(d)` | 15 s | WS 握手阶段的截止时间 |
| WS 写超时 | `WithWebSocketWriteTimeout(d)` | 30 s | 单次写 deadline |
| 单条消息上限 | `WithMaxMessageSize(n)` | 10 MB | 超限关连接 |

Proxy 的关键不变量：**一条 Client WS ↔ 一个 Streamer**，独立的 up/down 两条 pump goroutine，跨连接互不影响。

#### 5.4.7 北向仅 WS，不支持 HTTP

Proxy 刻意**不支持** Streamable HTTP 作为北向入口：Streamable HTTP 由多个独立 HTTP 请求（POST / GET / DELETE）组成，需要按 `Acp-Connection-Id` header 做 sticky 路由到同一后端；Proxy 在不解析协议的前提下无法保证这种亲和性，与**只搬字节、不看协议**的定位冲突。非 WS 请求会直接返回 `400 Bad Request`：

```
proxy endpoint only supports WebSocket
```

如果你需要既支持 HTTP 又要有代理能力，让下游直接对接 ACPServer；Proxy 只负责 WS 这一条路。

## 6. 其他

### 6.1 扩展方法（Custom Request / Notification）

ACP 官方支持 `_` 前缀的自定义方法（[Extensibility](https://agentclientprotocol.com/protocol/extensibility#custom-requests)）。SDK 在此基础上暴露两套接口，Agent / Client 任一方都可以选择实现：

```go
// 自定义 Request（有响应）
type ExtMethodHandler interface {
    HandleExtMethod(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// 自定义 Notification（无响应）
type ExtNotificationHandler interface {
    HandleExtNotification(ctx context.Context, method string, params json.RawMessage) error
}
```

#### 6.1.1 发送扩展消息

```go
// Client → Agent
raw, err := clientConn.CallExtRequest(ctx, "_myvendor.getStats", map[string]any{
    "sessionId": sid,
    "scope":     "last-24h",
})
// raw 是 json.RawMessage，业务方自行 Unmarshal

_ = clientConn.CallExtNotification(ctx, "_myvendor.heartbeat", map[string]any{
    "ts": time.Now().Unix(),
})

// Agent → Client（完全对称）
_ = agentConn.CallExtNotification(ctx, "_myvendor.toast", map[string]any{
    "sessionId": sid,
    "message":   "任务完成",
})
```

SDK 只做一件事：**校验 method 必须以 `_` 开头**；不以 `_` 开头直接报错。

#### 6.1.2 接收扩展消息

Agent 和 Client 只要实现上面两个接口，SDK 自动把非内置方法派发过来：

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

#### 6.1.3 Streamable HTTP 下的 sessionId 约定

Streamable HTTP 是**多连接共享的复用模式**：同一个 TCP 连接可能承载多个 Session 或并发请求流。所以如果你的扩展消息需要被路由到特定 Session，**一定要在 params 顶层带 `sessionId` 字段**：

```json
{
  "sessionId": "sess-123",
  "data": {}
}
```

SDK 提供了辅助类型：

```go
type CustomExtRequest struct {
    Meta      map[string]any  `json:"_meta,omitempty"`
    SessionID SessionID       `json:"sessionId"`
    Data      json.RawMessage `json:"data"`
}

type CustomExtNotification = CustomExtRequest
```

不遵守这个约定的话，HTTP 模式下消息可能被路由到错误的会话。

WebSocket / stdio 是单连接单 session 的点对点模式，不需要强制带 sessionId（但带上也无害）。

### 6.2 错误处理

#### 6.2.1 统一的错误类型

SDK 从 handler 返回值一路到线协议都用同一个 `RPCError`：

```go
type RPCError struct {
    Code    int             // JSON-RPC 错误码
    Message string
    Data    json.RawMessage // 可选附加信息
}
```

常用构造器：

| 构造器 | Code | 用途 |
| --- | --- | --- |
| `acp.ErrMethodNotFound(m)` | -32601 | 未实现的方法 |
| `acp.ErrInvalidParams(msg)` | -32602 | 参数校验失败 |
| `acp.ErrInternalError(msg, data)` | -32603 | 内部错误；data 可以是 error、struct 或任何可序列化类型 |
| `acp.ErrServerBusy(msg)` | -32001 | 服务繁忙 |
| `acp.ErrRequestCanceled(msg)` | -32800 | 请求被取消（ACP 自定义码） |
| `acp.NewRPCError(code, msg, data)` | 自定义 | 完全自定义错误 |

`NewRPCError` 对 `data` 做容错：
- `json.RawMessage` / `[]byte` + valid JSON → 直接透传；
- 非 valid JSON → 重新编码成 JSON 字符串，保证线上 payload 合法；
- 其他类型 → `json.Marshal`，失败则日志告警并丢 `data`。

#### 6.2.2 错误透传原则

SDK 遵守「**不吞错误**」：

- Handler 返回的 `error` 如果是 `*RPCError`，线协议用它本身；否则会被包成 `ErrInternalError`，**但保留原始 error 字符串**以供定位。
- 传输层异常（解析失败、write 超时、EOF、SSE 断流等）都会通过 `Err()` / `Done()` 透传到业务方。
- 通知（Notification）没有响应通道，失败会走 `WithNotificationErrorHandler`（若注册）或日志。

#### 6.2.3 Sentinel 错误（便于 `errors.Is`）

```go
transport.ErrTransportClosed  // 传输已关闭
transport.ErrConnNotStarted   // 连接未启动
transport.ErrConnClosed       // 连接已关闭
transport.ErrNoSessionID      // 无法路由（通常是 HTTP 下扩展消息缺 sessionId）
transport.ErrPendingCancelled // 反向调用被取消（pending tracker 被关闭）
transport.ErrSenderClosed     // sender 关闭时还有反向请求在等待
transport.ErrUnknownSession   // 路由到的 session 不存在或已失效
```

### 6.3 日志

```go
// 默认日志是标准库 + 合理的前缀，可以覆盖
acp.SetLogger(myLogger) // myLogger 实现 acp.Logger 接口（Printf 风格）

l := acp.GetLogger() // 获取当前 logger，永远非 nil
```

`acp.Logger` 要求提供 `Debug / Info / Warn / Error` 以及 `CtxDebug / CtxInfo / CtxWarn / CtxError` 这几组 Printf 风格方法（参考 `internal/log.Logger`）。

- **默认 logger 的 Debug 日志会打印全量 JSON-RPC 消息**。生产环境请把日志级别设到 Info 及以上。
- **Access 日志**：传输层在 Debug 打开时会记录消息收发，标注方向 `send` / `recv` 和 transport 名；适合做流量回放、排错。如果你安装了自定义 logger，需要它实现 `DebugEnabler` 并显式开启 Debug，才能保留完整 access payload 日志。

### 6.4 目录结构速览

```
acp/
├── types_gen.go / agent_gen.go / client_gen.go   // 协议生成
├── base.go                                        // BaseAgent / BaseClient
├── extension.go                                   // 扩展协议辅助
├── errors.go                                      // RPCError
├── logger.go                                      // SetLogger / GetLogger
├── conn/                                          // JSON-RPC 双向封装
├── transport/
│   ├── stdio/                                     // newline-delimited JSON
│   ├── http/client/                               // Streamable HTTP 客户端
│   └── ws/                                        // WebSocket 客户端
├── server/                                        // Hertz 服务端 (HTTP + WS)
├── proxy/                                         // 透明 WS 代理
├── stream/                                        // Proxy ↔ AgentServer 的 Streamer 抽象
├── examples/                                      // agent / client / proxy 三个可运行示例
└── cmd/generate/                                  // Schema 驱动代码生成
```

## 7. 常见问题

- **「请求超时但 Agent 其实已经处理完了」**：检查服务端 `WithRequestTimeout`（默认 HTTP 5min）和客户端 ctx deadline；HTTP 长任务把 server 超时调大即可。
- **「session/update 丢失」**：多半是 HTTP GET SSE 未建立就发了通知；SDK 会先走 pendingQueue（默认 1024）。如果堆满，不是简单丢一条消息，而是会关闭该 Session 并返回错误。调大 `WithPendingQueueSize` 或确保先 `NewSession` 再推送。
- **「WebSocket 建连就断」**：99% 是 Hertz 没设 `NoHijackConnPool = true`。
- **「Close 后 goroutine 泄漏」**：确保调用了 `conn.Close()`；stdio 额外要确保底层 reader/writer 被关（`cmd.Wait()` 回收子进程管道）。
- **「扩展消息路由错到别的 session」**：HTTP 下务必在 params 里带 `sessionId`。
