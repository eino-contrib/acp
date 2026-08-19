# Hertz / Gin 双 HTTP 框架支持重构方案

> 状态（2026-08-19）：已实现，并完成六轮 P0/P1 review 及逐项证伪复核。最终确认 0 个 P0、7 个 P1；7 个 P1 均已有修复前失败测试、真实 TCP 或安全契约证据，并已修复。第 1、4 节记录的是重构前基线；当前公共 API、运行约束和验证结果以第 10–19 节及 README 为准。

## 0. 六次复核后的 P0/P1 结论

本节是本方案的最终问题清单。只有能在实现基线上稳定复现、具备协议或 SDK 契约依据、且证伪复核后仍达到 P0/P1 的问题才列入；仅有设计差异、Draft/UNSTABLE 能力缺口、终止阶段的有界可靠性加固或测试自定义期待，不作为本期独立 P0/P1。

| ID | 优先级 | 确认问题 | 修复前验证 | 修复结果 |
|---|---|---|---|---|
| P1-1 | P1 | SSE writer 写失败后，已经被 `Send` / `SendLive` 成功放入旧 outbox、但尚未交给 writer 的尾部消息会静默丢失 | `TestSessionWriterFailurePreservesAcceptedOutboxTail` 在修复前稳定失败：replacement listener 收不到 accepted tail | writer failure 原子撤下 generation，等待已捕获该 outbox 的 sender 收敛，并把未写出的尾部按 FIFO 回灌 pending |
| P1-2 | P1 | writer 阻塞超过 handoff 等待上限、session 随后被移出 connection map 时，writer 脱离 shutdown registry，导致 `Shutdown` 虚报排空 | `TestConnectionCloseWaitsForBlockedSessionWriter`、`TestConnectionCloseTracksWriterAfterSessionOverflowRemoval` 在修复前均能观察到 writer 仍阻塞而 connection close 已返回 | writer 生命周期提升为 connection 级 tracker；shutdown 先封闭 writer admission，再等待所有 generation 实际退出 |
| P1-3 | P1 | Gin/Gorilla Server 在 core 主动关闭时可能直接断开 TCP，客户端收到 1006 而不是正常 1000 | 在临时副本恢复修复前 close 路径后，`TestServerCloseSendsNormalCloseAcrossFrameworks` 以真实 Hertz/Gin TCP listener 重复运行，Gin 稳定得到 1006 | Server core 在取消 connection 和关闭 socket 前同步尝试发送 1000 close frame；双框架 wire contract 固定最终 close code |
| P1-4 | P1 | README 主接入示例显式使用无条件 `CheckOrigin: true`，覆盖底层库的安全同源默认策略 | `TestWebSocketOriginPolicyDefaultsAndExplicitOverride` 证明恶意 Origin 在默认配置下返回 403、在原示例配置下升级为 101 | 删除全放行示例，保留安全默认并明确要求自定义 origin allowlist |
| P1-5 | P1 | `make run-http` / `run-ws` / `run-proxy` 按端口枚举 PID 后无条件 `kill -9`，会强杀非本项目进程 | 只读/无破坏验证证明 `lsof -t -i <port>` 会选中测试启动的无关 listener；`TestMakeRunTargetsOnlyCleanUpOwnedProcesses` 固定脚本边界 | target 仅记录自己启动的 `$!`，通过 `trap` 终止并等待该 child，不再扫描或强杀端口占用者 |
| P1-6 | P1 | `session/close` 成功后，Streamable HTTP 服务端仍保留 transport session 与 GET SSE，正常重复关闭会线性累积长连接/handler；服务端正确收敛后，开启 SSE reconnect 的客户端又会无限重连已关闭 session | 修复前真实 Gin 链路连续执行 12 次 `NewSession → 自动 GET SSE → CloseSession`，活跃 GET handler 从 1 线性增长到 12，重复 5 轮稳定；只恢复服务端 close 清理后 20 轮均归零；旧客户端启用 reconnect 后 10 轮均持续 GET 已关闭 session | 服务端仅在 `session/close` 成功响应后移除 transport session 并驱逐 SSE；客户端先标记 terminal intent、保留当前流供 close handler 使用，断线时抑制重连；wire RPC error 或明确未发送时撤销 intent，超时/断线等结果不确定错误完成 terminal；`session/delete` 不触发 transport 清理。为避免该 P1 修复引入终止阶段尾帧回归，session close 以有界 outbox drain fence 写完正常可写的 accepted tail，成功 close 的客户端读取旧 GET 到 EOF |
| P1-7 | P1 | Streamable HTTP 下 `session/resume` 成功后，服务端未在当前 logical HTTP connection 建立对应 transport session，SDK 客户端也未自动启动 GET SSE listener；RPC 表面成功，但 GET 返回 404，Agent 的反向 request/notification 无法路由 | `TestHandleProtocolPostRegistersResumedSession` 与 `TestSessionResumeStartsClientListener` 在修复前稳定失败；`TestSessionResumeEstablishesSSEAcrossFrameworks` 以 Hertz/Gin 真实 TCP 固定 active GET 与反向 `session/update` | 服务端在 resume handler 执行前按 request `sessionId` 建立 provisional transport session，使 handler 回调可路由；dispatch/response 失败时仅回滚本次新建 entry；生成的 `ClientConnection.ResumeSession` 成功后自动启动该 ID 的 GET SSE listener |

六次复核后，以下真实差异降为 P2，本期按范围要求不修复，也不作为发布阻断项：Draft/UNSTABLE `session/fork` 成功后尚未自动建立 target transport session/listener、HTTP body read 尚未纳入 core `Shutdown` admission、Hertz 未显式开启客户端断连感知造成的最长一个 keepalive 周期滞留、close-control 写锁竞争/custom adapter 违约加固、重复 `Connection` / `Upgrade` / `Sec-WebSocket-Extensions` field-line 的兼容性差异、Hertz 私有 `RequestContext.Set` key 与标准 context 的桥接策略，以及 Proxy 示例带 host 的 `-agent-listen` 地址构造。`session/close` accepted-tail drain 属于 P1-6 修复的回归约束，不再作为独立 P1 计数。

## 1. 背景

当前 SDK 的 HTTP 模式和 Proxy 北向入口都以 Hertz 为唯一服务端框架：

- ACPServer 在同一端点承载 Streamable HTTP（POST、GET SSE、DELETE）和 WebSocket；
- ACPProxy 在北向承载 WebSocket，并把消息透明转发给下游 Streamer；
- Streamable HTTP 的协议处理已经通过内部请求上下文做了初步抽象，但目前只有 Hertz 适配；
- WebSocket 的连接、升级器、关闭错误和帧常量仍直接依赖 Hertz WebSocket 实现；
- Proxy 的请求头转发回调直接接收 Hertz 请求上下文，业务配置也因此绑定 Hertz。

如果直接为 Gin 复制一套 handler、SSE writer 和 Proxy pump，两套实现会在连接生命周期、超时、错误码和心跳策略上逐步分叉。本次重构应先收敛框架边界，再增加 Gin 入口，保证协议与连接状态机始终只有一份。

## 2. 目标

- HTTP 模式同时支持 Hertz 和 Gin，包括 Streamable HTTP 与 WebSocket。
- Proxy 北向 WebSocket 同时支持 Hertz 和 Gin。
- ACP 协议处理、连接表、session、SSE 队列、WebSocket 心跳和 Proxy 双向转发保持单一实现。
- 框架选择只影响路由接入、HTTP 请求响应适配和 WebSocket upgrade，不影响 Agent、Client、Streamer 等业务实现。
- Hertz 与 Gin 对外行为一致：状态码、响应头、SSE 格式、WebSocket close code、超时和关闭语义一致。
- 公共 API 明确表达使用的框架，不再让无框架标识的 Handler 或 Mount 隐含代表 Hertz。
- 框架相关类型只出现在适配层，协议核心和转发核心不依赖 Hertz 或 Gin 类型。

## 3. 非目标

- 不改变 ACP 协议、JSON-RPC envelope、session 路由和 cancel 语义。
- 不给 Proxy 增加 Streamable HTTP 北向能力；Proxy 仍只接受 WebSocket。
- 不修改 HTTP Client 和 WebSocket Client 的实现选型。
- 不把 WebSocket 连接放入 Streamable HTTP 使用的 connection table。
- 不抽象成可动态加载任意 Web 框架的插件系统；本期明确支持 Hertz 和 Gin。
- 不改变下游 Streamer 协议或 Proxy 示例的南向连接方式。

## 4. 现状判断

### 4.1 已具备的复用基础

Streamable HTTP 的 POST、GET SSE、DELETE 处理已经基于统一的内部请求上下文，协议、connection、session 和 pending request 逻辑没有直接依赖 Hertz。增加 Gin 时，这一部分不需要重写，只需要补齐标准 HTTP 请求响应与 SSE 适配。

服务端 WebSocket transport 已经依赖一个较窄的连接能力集合，主要包括消息读写、control frame、deadline、read limit、Ping handler 和关闭。这一边界可以提升为 Hertz 与 Gorilla 共用的内部契约。

Proxy 的并发限制、下游建连、上下行 pump、超时、心跳和关闭策略本身与 HTTP 框架无关。主要耦合集中在请求入口、header 提取、WebSocket upgrade 和具体连接类型。

### 4.2 必须拆除的耦合

- ACPServer 和 ACPProxy 的路由挂载入口直接使用 Hertz router 类型；
- handler 直接使用 Hertz 请求上下文；
- WebSocket core 直接使用 Hertz WebSocket 连接与错误类型；
- Server 和 Proxy 的 upgrader 配置属于 Hertz 专属类型；
- Proxy header forwarder 暴露 Hertz 请求上下文；
- 文档和示例默认把 Hertz 约束描述成协议本身的约束。

## 5. 核心设计原则

### 5.1 核心与框架适配分离

Hertz 和 Gin 只负责把原生请求转换成统一请求模型，并完成各自的 WebSocket upgrade。转换之后，所有请求都进入同一套 HTTP 协议处理或 WebSocket / Proxy 连接核心。

### 5.2 不复制状态机

以下逻辑不得按框架分叉：

- initialize 与 connection ID 管理；
- POST 直接派发及请求超时；
- GET SSE 绑定、替换、积压队列与 keepalive；
- DELETE 关闭逻辑与 idle reaper；
- WebSocket 首帧校验、读写协程、心跳和关闭；
- Proxy 并发限制、下游建连、上下行 pump、大小限制和 close code 分类。

### 5.3 框架差异在边界归一化

Hertz WebSocket 与 Gorilla WebSocket 的方法形态接近，但连接类型和错误类型不兼容。适配层负责把两套实现归一成内部连接契约，核心不得同时判断两套库的错误类型。

### 5.4 显式 API 优于隐式默认

项目仍处于功能开发期，本次允许清理带有 Hertz 默认含义的公共入口，不保留仅用于兼容旧命名的冗余包装。调用方应显式选择 Hertz 或 Gin 接入。

## 6. 目标架构

~~~text
                         ┌──────────────────────────────┐
                         │       ACPServer / ACPProxy   │
                         │  framework-neutral runtime   │
                         └──────────────┬───────────────┘
                                        │
                    ┌───────────────────┴───────────────────┐
                    │                                       │
        ┌───────────▼───────────┐               ┌───────────▼───────────┐
        │ Streamable HTTP core  │               │ WebSocket / Proxy core│
        │ POST / GET / DELETE   │               │ lifecycle + pumps     │
        └───────────▲───────────┘               └───────────▲───────────┘
                    │                                       │
          unified HTTP context                    unified WS connection
                    │                                       │
          ┌─────────┴─────────┐                   ┌─────────┴─────────┐
          │                   │                   │                   │
   ┌──────▼──────┐     ┌──────▼──────┐     ┌──────▼──────┐     ┌──────▼──────┐
   │Hertz adapter│     │net/http     │     │ Hertz WS    │     │ Gorilla WS  │
   │request / SSE│     │adapter (Gin)│     │ adapter     │     │ adapter     │
   └─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
~~~

整体分为四层：

1. 运行时核心：管理 ACPServer / ACPProxy 的配置、连接和关闭。
2. 协议与转发核心：处理 Streamable HTTP、WebSocket transport 和 Proxy pump。
3. 统一边界契约：统一 HTTP 请求响应能力以及已升级的 WebSocket 连接能力。
4. 框架适配层：分别处理 Hertz 与 net/http（Gin）的路由、SSE 输出和 WebSocket upgrade。

## 7. 框架接入策略

### 7.1 Hertz

Hertz 继续使用原生 handler、请求上下文、SSE writer 和 Hertz WebSocket upgrader。现有协议语义保持不变，重构后它只是统一核心的一种适配实现。

承载 Streamable HTTP 的 Hertz 宿主首选启用 `hertzserver.WithStreamBody(true)`。Hertz 默认会在 handler 前缓冲 request body，并应用宿主级上限（当前依赖版本默认为 4 MiB），而 ACP 的 `server.WithMaxHTTPMessageSize` 默认是 10 MiB；若不调整宿主，4–10 MiB 的合法请求可能在到达 adapter 前被 Hertz 拒绝，chunked 或未知长度 body 也无法由 ACP adapter 在读取过程中按 `max+1` 有界检查。流式 body 让超过 Hertz 预读阈值的请求以 stream 进入 adapter，由 SDK 统一执行配置上限与 413 语义。不能启用流式 body 时，可把 `hertzserver.WithMaxRequestBodySize(...)` 设为不小于 SDK 上限，避免宿主提前拒绝，但此方案可能先缓冲完整 body。这两项都是 Hertz server 级配置；其他路由需要不同策略时，应为 ACP 路由使用独立宿主。

### 7.2 Gin

Gin 基于标准 net/http，因此推荐把 Gin 支持建立在标准 HTTP handler 之上：

- Streamable HTTP 使用标准 request / response writer 适配；
- WebSocket 使用 Gorilla WebSocket upgrader；
- Gin 公开适配层只保留很薄的 handler 包装与路由注册能力；
- 标准 HTTP 请求响应适配属于内部复用能力，本期不额外承诺第三套通用 net/http 公共 API。

该方案避免协议核心直接依赖 Gin，也避免为 Gin 单独实现一套 SSE 和 WebSocket 状态机。

## 8. Streamable HTTP 设计

### 8.1 统一请求响应能力

内部 HTTP 上下文继续作为协议层唯一依赖，覆盖以下能力：

- 获取请求 context、method、header 和 body；
- 设置响应 header 与 status；
- 写入文本错误；
- 输出并 flush SSE message event；
- 输出 SSE keepalive；
- 感知客户端断开和请求取消；
- 结束当前 SSE writer。

现有 Hertz 适配保留其 chunked writer 语义。标准 HTTP 适配使用 request、response writer、flusher 和请求 context。Gin 只把自身请求交给这个标准适配，不进入协议层。

请求体大小限制必须在读取过程中生效，不能只依赖 Content-Length 或在完整读入内存后再检查。对于 chunked、缺少 Content-Length 或伪造长度的请求，两种框架都必须在达到上限后停止读取并返回相同的 413 响应。

### 8.2 SSE 行为统一

两种框架必须输出完全一致的 SSE 语义：

- 响应类型为 text/event-stream；
- 每条 JSON-RPC 消息使用 message event；
- 每个 data 行遵循 SSE 换行规则；
- 建立 GET listener 后立即输出一次 keepalive；
- 后续按统一间隔输出 keepalive；
- 每次 event 或 keepalive 后及时 flush；
- 写失败、请求取消、session 关闭、connection 关闭或 listener 被替换时退出；
- listener 替换与 session / connection 关闭并发时，旧流的驱逐信号只能关闭一次，不能因重复关闭导致进程 panic；
- `Send` / `SendLive` 返回成功后，消息所有权已经转移给 session；listener 替换或请求结束触发 `UnbindStream` 时，必须先停止旧 writer、等待已经取得旧 outbox 的 sender 收敛，再把尚未写出的消息按原顺序移回 pending queue，不能因为驱逐旧 generation 而静默丢失；Unbind 的 detach / 等待 / 回灌必须与下一次 Bind 的 pending drain / writer 发布共用同一串行 handoff，禁止新 generation 先发布、旧消息后回灌；
- terminal `session/close` 也必须保留 close handler 已成功发送的消息：关闭先封住新发送，通过 writer FIFO drain fence 等待已接受消息完成写出，再驱逐 GET；客户端收到成功 close response 后不能抢先关闭 GET body，而应继续读取到服务端 EOF，避免跨连接响应乱序丢失尾帧；
- SSE writer 因写失败退出后，该 generation 必须立即失去 live 状态；后续反向 request 要快速返回无活跃流错误，普通 notification 则可重新进入有界 pending queue，等待新的 GET listener。

SSE 的 session 绑定、旧流驱逐、pending queue 和慢消费者保护继续由现有协议核心负责，框架适配层只负责可靠写出。

请求协商需要区分两条契约：

- POST 为兼容已有调用方，继续把缺失的 `Accept` 视为可接受；显式 `Accept` 的最终协商结果必须同时允许 `application/json` 与 `text/event-stream`。媒体范围按具体度决定优先级，匹配范围的 `q=0` 表示不可接受。这是仓库既有 `ValidatePostHeaders` 契约的修正，不作为 Active RFD 的新增要求表述。
- GET 按 Active RFD 要求必须显式接受 `text/event-stream`；缺失、只接受其他媒体类型、最终 SSE 质量为 `q=0` 或 qvalue 非法时，在 connection/session lookup 与任何 SSE 输出之前返回 406。

### 8.3 Context 传递

- 单次 POST、GET、DELETE 使用框架请求 context，保留超时和客户端断开语义。
- initialize 创建长连接状态时，保留请求 context 中的认证、租户和 trace 值，但解除与单次请求取消的绑定，再由 ACPServer 根 context 控制最终关闭。
- Gin middleware 需要传给 AgentFactory 的数据应写入标准 request context；只存在于 Gin 私有上下文中的值不会被核心隐式读取。
- 适配层不吞掉 AgentFactory、业务 handler 或 StreamerFactory 返回的内部错误。

Gin 的上下文传播契约明确限定为标准 request context 和 request header。认证、租户、trace 等 middleware 必须在进入 ACP handler 前完成，并把所需值写入 request context。Gin 私有键值不做自动复制，因为其生命周期和类型约束不适合被隐式带入长连接。middleware 已经 abort 的请求不得继续调用 ACP handler；ACP handler 开始 upgrade 后，后续 middleware 不得再修改响应。

## 9. WebSocket 统一设计

### 9.1 库选型

- Hertz 继续使用 hertz-contrib/websocket。
- Gin 使用 gorilla/websocket。

两者均能满足消息读写、deadline、read limit、Ping handler 和 control frame 能力，但不能直接互相转换。

### 9.2 内部连接契约

新增统一 WebSocket 连接契约，覆盖：

- 读取和写入 message；
- 写入 control frame；
- 设置 read limit；
- 设置读写 deadline；
- 安装 Ping handler；
- 关闭连接。

服务端 WebSocket transport 与 Proxy connection 都依赖这一契约，不再依赖任何具体 WebSocket 库。

### 9.3 错误与常量归一化

内部 WebSocket 层统一维护协议所需的 frame type、close code、关闭消息格式以及下列错误语义：

- peer close 及其 close code；
- message 超过 read limit；
- 读超时与写超时；
- control frame 写锁竞争；
- 普通网络关闭。

Hertz 和 Gorilla adapter 在所有读写边界把各自的 native error 转为内部错误，尤其要覆盖 ReadMessage 返回的 close / read-limit 错误，以及 WriteControl 返回的写锁竞争和真实 socket 写失败。核心只判断内部错误，确保同一种场景在两种框架下生成同一个 close code，同时不把短暂写锁竞争误判成连接失效。

### 9.4 连接生命周期

统一后的服务端 WebSocket 流程如下：

1. 框架 adapter 判断请求是否为合法的 GET upgrade。
2. 生成 connection ID，并放入 handshake response header。
3. 对应框架完成 upgrade。
4. upgrade 成功后创建本连接专属的 Agent、AgentConnection 和 WebSocket transport。
5. 使用脱离请求取消、但保留请求值的 connection context 运行长连接。
6. 首条业务帧必须是 initialize request；之后进入正常读写与心跳阶段。
7. 客户端关闭、超时、读写失败或 ACPServer 关闭时，统一收敛到一次关闭路径。

Agent 实例放在 upgrade 成功后创建，避免无效握手触发 AgentFactory 或残留后台读循环。AgentFactory 以及 `ConnectionAwareAgent.SetClientConnection` 都属于不可信的用户 bootstrap hook：任一 panic 都必须被转换成 connection-local setup error，不能逃逸到框架或 hijack goroutine。若 upgrade 后创建或注入连接失败，则以固定、非敏感的服务端错误 close frame 结束 WebSocket；HTTP 初始化路径也必须释放尚未登记的 sender、AgentConnection、context 和 admission。

框架 upgrader 的 `CheckOrigin`、错误响应回调等同样属于调用方代码，但其 panic 发生在 adapter 已取得 admission 之后。Adapter 不吞掉也不改写该 panic；它必须先完成本次 admission、清除 provisional handshake 状态，再以原始 panic value 继续向上传播，确保外层 Recovery middleware 接管时不会留下 Server registry 条目或 Proxy 并发配额。

WebSocket 连接仍不进入 Streamable HTTP 的 connection table。ACPServer 保留独立的活跃 WebSocket 生命周期集合，仅用于整体关闭，不参与 HTTP connection ID 查询和 session 路由。

upgrade 与 ACPServer.Close 之间必须有统一的 admission 边界：进入 closing 后拒绝新握手；已经完成握手但尚未登记的连接必须观察到 root context 已关闭并立即退出；连接登记与全量关闭不能留下“Close 已扫描完成、连接随后才加入集合”的窗口。Hertz 的 hijack callback 与 Gorilla 的同步 upgrade 执行时序不同，但都必须满足这一约束。

### 9.5 握手一致性

Server 共用一套 WebSocket attempt 判定和握手前校验，避免畸形请求在一个框架进入 WebSocket、在另一个框架误入 GET SSE：

- 同时识别 Upgrade / Connection token 和 Sec-WebSocket-Key、Sec-WebSocket-Version 等握手信号；疑似 WebSocket 但字段不完整的请求按握手失败处理，不回退到 SSE。
- 有效握手统一要求 GET、Connection 中包含 upgrade token、Upgrade 中包含 websocket token、版本为 13 且 key 合法。
- Origin 默认采用同源策略；允许的 origin、subprotocol 和 compression 由各框架 adapter 配置，但默认值与协商结果必须一致。
- 默认不启用压缩、不声明 subprotocol。启用后，两种 adapter 使用同一业务配置和相同优先级。
- Acp-Connection-Id 只在成功握手响应中返回，失败握手不产生可复用 connection ID。
- 重复 header、大小写和逗号分隔 token 按 HTTP 语义解析，不能用简单字符串相等代替。
- 对等的握手失败必须返回相同状态码和稳定的对外错误类别；底层 upgrader 的详细错误只进入内部日志。

## 10. ACPServer 对外接入

### 10.1 官方 adapter API 模型

ACPServer 根对象只拥有 AgentFactory、协议配置、连接状态和关闭生命周期，不再持有 router、endpoint 或任一框架 upgrader。官方适配层提供两个独立包：

| 适配层 | 输入 | 输出 | 拥有的配置 |
|---|---|---|---|
| Server Hertz adapter | ACPServer core | Hertz 原生 handler | Hertz upgrader、origin、buffer、compression、subprotocol |
| Server Gin adapter | ACPServer core | Gin 原生 handler | Gorilla upgrader、origin、buffer、compression、subprotocol |

adapter handler 是引用 core 的轻量、不可变对象，不拥有连接表，也不提供独立 Close。所有连接最终登记到 core，统一由 ACPServer 关闭。adapter 构造完成后不允许并发修改 upgrader 配置。

路由路径完全由宿主 router 注册。SDK 继续保留默认 /acp 路径常量供示例和调用方复用，但 handler 本身不知道路径，也不提供 Mount。内部标准 net/http 适配不作为与 Hertz / Gin 并列的第三个官方 handler 包。core 同时导出 `HTTPContext`、WebSocket admission/connection 等 adapter-facing SPI，供第三方框架自定义接入；这类 adapter 必须自行复现官方 adapter 的握手尝试识别、RFC 6455 校验、Origin 策略、panic cleanup 与 lifecycle admission，SDK 不承诺其行为自动与 Hertz/Gin 对齐。

### 10.2 配置归属

| 配置类别 | 归属 |
|---|---|
| 请求超时、connection idle、pending queue、消息大小、inflight 上限 | ACPServer core |
| WebSocket initialize/read timeout、通知错误处理 | ACPServer core |
| Origin 校验、buffer、compression、handshake 细节 | 对应框架 handler adapter |
| 路由路径和 middleware | 宿主 Hertz / Gin router |

框架专属 upgrader 不再作为 ACPServer core option，避免 core 同时保存两套互斥类型。

## 11. ACPProxy 对外接入

ACPProxy 采用与 ACPServer 相同的公共 API 模型：根对象只拥有 StreamerFactory、运行时配置、并发配额和连接生命周期；Hertz adapter 与 Gin adapter 分别返回原生 handler，并拥有各自 upgrader 配置。adapter 不拥有活跃连接，也不提供独立 Close。endpoint 由宿主 router 注册，默认路径常量仅用于约定和示例。

### 11.1 统一握手协调

Proxy 增加内部统一握手请求模型，提供 method、header、response header、错误响应、request context 和 upgrade callback 等最小能力。

Proxy 复用 Server 的 WebSocket attempt 分类、握手校验和默认 Origin 策略，只有握手成功后的业务生命周期不同，不能再维护一套较宽松的 Upgrade 字符串判断。

Hertz 与 Gin adapter 只负责包装各自请求并执行具体 upgrade。以下逻辑统一留在 Proxy core：

- 只接受 WebSocket；
- 只允许 GET upgrade；
- Proxy 关闭后拒绝新连接；
- 获取与释放并发连接配额；
- 生成 connection ID；
- 提取转发 metadata；
- 调用 StreamerFactory；
- 跟踪活跃连接；
- 运行上下行 pump 并完成关闭。

并发配额必须通过一次性释放机制管理：upgrade 失败或同步 upgrader callback panic 时立即释放，upgrade 成功后在连接退出时释放，任何路径都不能重复释放或泄漏。Callback panic 的清理只维护 adapter 自身资源不变量，清理完成后仍向上传播原始 panic。

### 11.2 Proxy 连接状态机

Proxy 将一条北向连接明确划分为以下阶段，并从 admission 开始纳入统一 registry，而不是等 Streamer 创建成功后才登记：

1. admitted：已通过关闭状态和并发配额检查；
2. upgrading：正在完成框架 WebSocket upgrade；
3. creating downstream：已升级，正在调用 StreamerFactory；
4. active：Streamer 已创建，双向 pump 正在运行；
5. closing / closed：正在或已经收敛资源。

Proxy shutdown 原子停止 admission，取消 upgrading 之后所有可取消操作，并等待 registry 排空或调用方的 shutdown deadline 到期。StreamerFactory 必须观察传入 context；如果实现忽略取消，SDK 无法强制终止其内部阻塞，context-bounded shutdown 应返回 deadline 错误，而不能声称已经完整关闭。

任何已经 upgrade 且已经获得非 nil Streamer 的清理分支，都必须先尝试发送北向 close frame，并立即关闭北向 WebSocket，再调用下游 `Streamer.Close`；该约束覆盖 active pump 退出、factory 返回 Streamer 同时返回 error、factory 成功返回时 Proxy 已进入 closing，以及 timeout / cancel 后迟到的 Streamer。`Streamer.Close` 是调用方实现，可能阻塞或 panic；它不得阻止客户端 socket 及时断开。下游 Close 尚未返回时 admission 仍保留在 registry 中，因此 `Close` 保持非阻塞，而 `Shutdown(ctx)` 必须继续等待真实收敛并在 deadline 到期时返回错误。

StreamerFactory 的 panic、返回 nil Streamer、超时和普通错误都按创建失败处理：释放配额、移除 registry 条目、保留完整内部错误，并在已经 upgrade 的连接上发送 1011。普通 factory 错误沿用当前可诊断的安全截断 reason；panic 只对外返回通用错误，不泄漏 panic 内容。

### 11.3 Header 转发解耦

现有 HeaderForwarder 直接接收 Hertz 请求上下文，应替换为框架无关的 metadata extractor。它只依赖：

- 请求 context；
- 只读 header accessor。

常用的“按名称复制请求头”继续由 ForwardHeaders helper 提供，但其实现不再知道 Hertz 或 Gin。自定义 extractor 也不访问框架原生上下文，避免业务配置重新形成框架耦合。

提取出的 metadata 在进入 StreamerFactory 前由 Proxy 获得独占快照，防止框架回收请求对象或调用方后续修改造成并发问题。跨框架需要的认证、租户和 trace 数据应放入 handler 接收的标准 request context 或请求头；框架私有 key bag 不做隐式全量复制。

### 11.4 Proxy payload 与帧语义

Proxy 的“透明”定义为 JSON-RPC payload 字节透明，不是 WebSocket frame type 透明：

- 北向 text 和 binary data frame 都可被读取，其 payload 原样交给 Streamer；
- Streamer 接口只传递 payload，不携带 WebSocket frame type；
- 从 Streamer 返回北向客户端的 payload 统一写为 text frame，与当前行为一致；
- control frame 不进入 Streamer，Ping / Pong / Close 仍由 Proxy 的 WebSocket 生命周期处理。

本期不扩展 Streamer 接口保存 binary frame type。若未来需要真正的帧级透明转发，应单独演进 Streamer 契约，不能在不同框架 adapter 中产生不同策略。

### 11.5 Proxy 行为一致性

| 场景 | 预期结果 |
|---|---|
| 普通 HTTP 请求访问 Proxy endpoint | HTTP 400 |
| 非 GET 的 WebSocket upgrade | HTTP 400 |
| Proxy 正在关闭 | HTTP 503 |
| 超出并发连接上限 | HTTP 503 |
| 下游 Streamer 创建失败 | WebSocket 1011 |
| 首帧超时 | WebSocket 4001 |
| 稳态读超时 | WebSocket 1001 |
| 消息超过限制 | WebSocket 1009 |
| 正常关闭或下游 EOF | WebSocket 1000 |

Proxy 的上下行 payload、日志内容和 Streamer 错误不得因框架不同而被改写。

## 12. 框架运行约束

### 12.1 Hertz

- WebSocket 服务继续要求宿主开启 NoHijackConnPool，防止 Hertz 回收升级后的连接。
- Streamable HTTP 服务首选开启 `hertzserver.WithStreamBody(true)`；若关闭流式 body，则 Hertz host 的 `WithMaxRequestBodySize` 必须不低于 `server.WithMaxHTTPMessageSize`，否则宿主会先于 ACP adapter 截断请求。
- 继续使用 Hertz SSE writer 和 Hertz WebSocket upgrader。
- 原生请求 context 的值需要在长连接 context 中保留。

### 12.2 Gin

- Gin 运行于标准 net/http server，WebSocket 使用 Gorilla upgrader。
- ResponseWriter 必须支持 flush；WebSocket 所在 server 必须支持 hijack。标准 Go HTTP/1.x server 满足这两个条件。
- upgrade 后不得再由后续 middleware 写 HTTP body。
- 默认 Origin 策略与 Hertz 保持同等安全级别，不提供默认全放行行为。
- Gin adapter handler 只包装内部标准 HTTP 适配，不拥有独立协议逻辑。

### 12.3 负载均衡与反向代理

- Streamable HTTP 的同一 connection 会跨多个 POST、GET 和 DELETE 请求，但 connection table 位于单实例内存中。负载均衡必须按 Acp-Connection-Id、粘性 cookie 或一致性路由，把同一 connection 的请求送到同一后端实例。
- SSE 路径必须关闭代理响应缓冲，并保证代理 idle timeout 大于 SSE keepalive 周期，否则 event 会被延迟或长连接会被误断开。
- WebSocket 路径必须透传 Upgrade、Connection 和相关握手 header。Gin / Gorilla 本期只支持 HTTP/1.1 upgrade，不承诺 RFC 8441 的 HTTP/2 WebSocket。
- 标准 HTTP server 的全局 WriteTimeout 不能短于预期的 SSE 生命周期；如果同一 server 同时承载普通短请求，应按路由或独立 listener 规划超时策略。
- ACPServer 与 ACPProxy 默认端点相同。在同一个 router 中同时挂载时，宿主必须为两者选择不同路径；否则应保持“一进程一个北向角色”。

### 12.4 统一关闭顺序

框架 server 的 Shutdown 通常不会替 SDK 管理所有 hijacked WebSocket，因此关闭必须由 core 与框架共同完成。ACPServer 和 ACPProxy 都提供两层明确语义：Close 只负责原子进入 closing、发出取消并启动资源收敛，要求幂等且不等待阻塞的业务 handler、SSE writer 或 downstream 实现；context-bounded Shutdown 复用同一触发路径，并等待 SDK 已登记的 registry 工作排空，调用方 deadline 到期时返回明确错误。对 Streamable HTTP，已登记的 dispatch、GET SSE setup/listener 与所有 connection-owned session writer 清理实际退出后才算排空；HTTP body read 尚未进入 core admission，已在第 0 节降为 P2 并留待后续处理。adapter 本身不提供 Close 或 Shutdown。

1. 调用 ACPServer / ACPProxy `Close`，原子进入 closing、拒绝新连接并取消活跃工作；
2. 执行 Hertz 或 net/http server 的优雅关闭，让 pending handler 与 upgrade 得到真实结果；
3. 调用 core `Shutdown(ctx)`，等待 Streamable HTTP 状态、SSE listener、WebSocket、Proxy Streamer 与 admission registry 排空；
4. 超过总关闭期限后返回 deadline 错误，由宿主决定是否强制终止。

Close 必须幂等，并覆盖关闭与 upgrade、SSE listener 替换、客户端主动断开同时发生的竞态。

## 13. API 清理与迁移

本次按开发期重构处理，不保留旧 API 的 deprecated 包装层。建议迁移关系如下：

| 当前能力 | 重构后 |
|---|---|
| ACPServer 的无框架 Handler / Mount | 使用独立的 Server Hertz adapter 或 Server Gin adapter 创建原生 handler |
| Server 的 Hertz WebSocket upgrader option | 移入 Hertz handler adapter 配置 |
| ACPProxy 的无框架 Handler / Mount | 使用独立的 Proxy Hertz adapter 或 Proxy Gin adapter 创建原生 handler |
| Proxy 的 Hertz upgrader option | 移入 Hertz handler adapter 配置 |
| Proxy 的 Hertz HeaderForwarder | 替换为框架无关 metadata extractor |
| Endpoint option | 移除；路由路径由宿主 router 决定，默认路径仅保留为常量 |

所有协议和运行时配置保持在 ACPServer / ACPProxy core；所有框架配置归对应 handler adapter。调用方切换框架时只替换 handler 创建与路由注册，不需要重建业务层配置。

## 14. 代码组织影响

计划涉及以下逻辑区域，但实现时不改变它们的业务职责：

- internal/httpserver：保留协议核心，补充标准 HTTP 请求与 SSE 适配。
- internal/wsserver：改为依赖统一 WebSocket 连接契约。
- 新的内部 WebSocket 适配区域：维护通用连接契约、错误语义以及 Hertz / Gorilla 包装。
- server：拆分 framework-neutral runtime、Hertz adapter、Gin adapter 与内部标准 HTTP 适配。
- proxy：拆分 framework-neutral admission / pump、Hertz adapter 与 Gin adapter，并重做 metadata 提取边界。
- examples/agent：增加 HTTP framework 选择，默认仍可使用 Hertz。
- examples/proxy：增加 Proxy framework 选择；南向 Streamer 示例不变。
- README.md 与 README.zh-CN.md：分别给出 Hertz 与 Gin 的接入和运行约束。

## 15. 分阶段实施计划

### 阶段一：建立行为基线

- 固化现有 Hertz HTTP、SSE、WebSocket 和 Proxy 的关键行为测试。
- 建立可复用的 adapter contract suite，使同一组用例能分别运行于 Hertz 与 Gin。
- 记录当前状态码、响应头、SSE frame 和 close code，作为重构验收基线。

### 阶段二：抽取 WebSocket 连接边界

- 建立统一连接契约与内部错误语义。
- 先接入 Hertz adapter，确认全部现有测试无行为变化。
- 让 WebSocket server transport 和 Proxy pump 改为依赖统一连接契约。

### 阶段三：接入 Gin 的 Streamable HTTP

- 增加标准 HTTP 请求响应适配。
- 复用现有 POST、GET SSE、DELETE 协议 handler。
- 验证 initialize、session 创建、反向消息、listener 替换和连接删除的全链路。

### 阶段四：接入 Gin WebSocket Server

- 增加 Gorilla upgrade 和连接 adapter。
- 对齐首帧校验、心跳、读写 timeout、消息限制和 ACPServer shutdown。
- 调整 WebSocket Agent 创建时序，确保失败握手不创建 Agent。

### 阶段五：重构 Proxy 入口

- 抽取统一握手协调和框架无关 metadata extractor。
- 保持 Hertz 行为不变后接入 Gin adapter。
- 对两种框架运行同一套并发、超时、close code 和 heartbeat 测试。

### 阶段六：收敛 API 与文档

- 删除隐式 Hertz 公共入口和 framework-specific core option。
- 更新示例、Makefile、架构文档和中英文 README。
- 完成 vet、全量测试、race 测试及基础性能对比。

## 16. 测试矩阵

所有标记为“双框架”的用例必须以相同断言分别运行在 Hertz 和 Gin 上。

| 模块 | 核心用例 | 范围 |
|---|---|---|
| HTTP POST | header 校验、initialize、后续 request、notification、response、body 限制、handler timeout | 双框架 |
| HTTP GET SSE | `Accept`/406、未知 connection/session、首个 keepalive、pending FIFO、listener 替换与 Unbind 保留已接受 outbox 消息、Unbind 与下一次 Bind 并发 handoff、写失败后的 live-state 失效、session / connection 关闭 | 双框架 + core race |
| HTTP DELETE | 正常删除、重复删除、删除后的请求 | 双框架 |
| HTTP session close | `session/close` 成功后服务端移除 transport session、结束现有 GET SSE、拒绝同 session 新 GET；客户端停止对应 listener 且不触发重连；wire RPC error 与结果不确定错误分别处理 | 双框架真实 TCP + core + client |
| HTTP session routing | POST 的业务 session 真值由 Agent 判断，不能用 GET SSE transport map miss 抢先返回 HTTP 404；unknown transport session 只在 GET/reverse routing 上报错 | core contract |
| HTTP session resume | `session/resume` 成功后建立 request `sessionId` 对应的 transport session 与 GET SSE listener；失败时回滚本次新建的 provisional entry | 双框架真实 TCP + core + client |
| HTTP session delete | `session/delete` 不隐式关闭活跃 transport session；允许 Agent 采用“从 session/list 删除但会话继续活跃”的协议语义 | 双框架真实 TCP + core + client |
| Server WebSocket | upgrade、connection ID、非法首帧、initialize timeout、稳态 timeout、Ping/Pong、消息上限 | 双框架 |
| Server 生命周期 | upgrade 失败不创建 Agent、不泄漏连接；AgentFactory / ConnectionAwareAgent 注入 panic 隔离；upgrader callback panic 先清理 admission 再原样传播；ACPServer Close 非阻塞；Server Close 对等输出 1000；Shutdown 等待已登记 HTTP dispatch、GET setup/listener 与 SSE writer 收敛 | 双框架 + core |
| Proxy HTTP 边界 | 非 WS、非 GET、关闭中、并发超限 | 双框架 |
| Proxy 数据面 | text/binary 上行 payload、固定 text 下行、header metadata、Streamer panic/nil/超时/错误、大小限制 | 双框架 |
| Proxy 心跳 | 首帧前 Ping、首帧后 Ping、无 Ping 读超时 | 双框架 |
| Proxy 生命周期 | upgrade 失败和 upgrader callback panic 配额归还、panic 原值传播、client/downstream 同时关闭、Proxy Close、active / factory-error / closing-race 分支中下游 Close 阻塞时北向 socket 仍及时关闭且 Shutdown 如实超时 | 双框架 + core |
| Adapter 单测 | close error、read limit、timeout、control write 错误归一化 | Hertz + Gorilla |
| WebSocket 握手 | attempt 分类、GET / token / version / key、默认 Origin 策略与失败状态 | 双框架 |
| 并发安全 | SSE listener 替换、WS data/Pong/Close 竞争、Proxy shutdown | race |

WebSocket、SSE 断连和 shutdown 用例必须使用真实 TCP listener，不能只依赖不支持 hijack 或真实 flush 的 response recorder；不涉及框架 wire 行为的 core 并发与阻塞不变量可以使用可控 fake connection 精确制造竞态。当前 `TestStreamableHTTPAdapterContract` 已在 Hertz 与 Gin 的真实 TCP listener 上建立有效 session，并在 GET 响应保持打开时读到首个 `:keep-alive` 行，固定了首个 SSE 输出的即时 flush；该 contract 还覆盖 POST initialize、DELETE 与未知长度超限 413。`TestStreamableHTTPAdapterDoesNotPreRejectSDKAllowedBody` 进一步证明两种宿主都允许 4 MiB+1、但仍小于 SDK 配置上限的请求到达 ACP parser，避免 Hertz 默认宿主上限抢先拒绝。`TestSessionCloseConvergesSSEAcrossFrameworks` 与 `TestSessionCloseEvictsExistingRawSSEAcrossFrameworks` 以真实 Hertz/Gin listener 固定 close 后 GET handler 收敛、客户端不重连、原流结束及同 session 新 GET 为 404；`TestSessionDeleteKeepsActiveSSEAcrossFrameworks` 固定 delete 不会错误终止仍活跃的 session。客户端单测分别覆盖成功 close、wire RPC error、请求未发送、结果不确定 timeout 与 delete 不改变 listener。`TestSessionCloseDuringListenerReplacementDoesNotPanic` 与 `TestConcurrentSessionListenerReplacementsDoNotPanic` 固定 listener 替换同 session 关闭或再次替换并发时不会重复关闭驱逐 channel；`TestListenerReplacementPreservesAcceptedOutboxMessages`、`TestListenerUnbindPreservesAcceptedOutboxMessages`、`TestConcurrentListenerUnbindAndReplacementPreservesAcceptedOutboxMessages` 与 `TestSessionWriterFailurePreservesAcceptedOutboxTail` 固定 replacement、Unbind 和 writer failure 均保留已经被 sender 接受但尚未写出的 outbox 尾部。`TestConnectionCloseWaitsForBlockedSessionWriter` 与 `TestConnectionCloseTracksWriterAfterSessionOverflowRemoval` 固定 connection shutdown 会继续跟踪已 detach 或已从 session map 移除的 writer。`TestServerCloseSendsNormalCloseAcrossFrameworks` 用真实 Hertz/Gin TCP listener 固定 core Close 的 1000 close code。`TestHTTPConnectionSetterPanicIsSetupErrorAndCleansUnregisteredConnection` 与 `TestWebSocketConnectionSetterPanicReturnsSetupErrorAndClosesSocket` 分别固定 HTTP 和已升级 WebSocket 中的 `ConnectionAwareAgent.SetClientConnection` panic 隔离。Proxy 的 `TestProxyFrameworkAdapterHeartbeatAndSizeContract` 用同一套 Hertz/Gin 真实 TCP contract 固定首帧超时 4001、首帧前 Ping 不续期、首帧后 Ping 保活、无 Ping 读超时 1001，以及北向 frame 与南向 payload 超限时的双向 1009。多实例粘性路由仍属于部署约束，由集成环境按本方案验证。

Lifecycle 回归还必须用可控阻塞点固定关闭边界：`TestShutdownWaitsForHTTPDispatchHandler` 与 `TestShutdownWaitsForHTTPGetInitialKeepAlive` 证明 `Shutdown` 不会在已登记业务 handler 或 GET setup 尚未退出时报告成功；`TestHTTPConnectionCloseDoesNotWaitForBlockedSSEWriter` 证明 connection Close / DELETE 不被阻塞 writer 同步拖住，而 connection 级 writer tracker 保证后续 `Shutdown` 仍等待后台 session writer 真正退出。四个 adapter 的 upgrader panic 用例分别覆盖 `CheckOrigin` 与自定义错误回调，断言 admission / 配额已释放且 recover 观察到原始 panic value。

本轮新增 `TestHandleProtocolPostDoesNotUseSSESessionMapAsBusinessAuthority`，固定 POST 仍由 Agent 判断业务 session；`TestSessionResumeEstablishesSSEAcrossFrameworks` 固定 resume 自动建立 listener 并可接收反向 update；`TestRemoveSessionDrainsAcceptedOutboxMessages` 用可控 writer fence 固定 terminal close 在正常可写时不丢弃已接受的尾部消息。`TestSessionCloseConvergesSSEAcrossFrameworks` 还覆盖成功 close 后客户端继续读取旧 GET 到 EOF，不因 POST response 与 GET 尾帧乱序而丢失 close handler notification。

## 17. 可观测性要求

- 现有全量 Debug payload 日志策略不变。
- adapter 的握手、升级失败与宿主访问日志保留框架身份；共享 ACP payload access log 继续使用统一 transport channel，避免把框架差异泄漏进协议核心。
- connection ID 在 HTTP response、WebSocket handshake 和后续连接日志中保持一致。
- upgrade 失败、AgentFactory 失败、StreamerFactory 失败和 adapter 写失败都保留原始内部错误日志。
- 对外 close reason 继续进行长度和 UTF-8 安全处理；内部日志保留完整错误。
- adapter 不把业务错误统一包装成模糊的框架错误。

## 18. 风险与控制

| 风险 | 控制措施 |
|---|---|
| 两种 SSE writer 的 flush / chunk 行为不同 | 共享 wire-level 测试，直接断言事件字节与断开行为 |
| SSE listener 替换与关闭并发导致重复驱逐或 writer 失败后留下假活跃流 | listener replacement 串行化并先撤下旧驱逐信号；writer 退出按 generation 原子失效 live 状态；覆盖 rebind / Close race 与写失败后的 SendLive |
| `session/close` 驱逐 SSE 时 adapter Close 与协议层重复 Flush 并发 | Hertz 的 `WriteSSEEvent` / `WriteSSEKeepAlive` 已在 writer 锁内 flush；writer 创建后 adapter 将协议层追加的 raw Flush 视为 no-op，保留公共 SPI 的 Flush 契约；真实 Hertz/Gin close/delete contract 通过 race detector |
| listener replacement / Unbind 遗弃旧 outbox，或 Unbind 回灌与下一次 Bind 交错，导致已返回成功的 notification / reverse request 静默丢失或滞留到更晚 generation | 每个 outbox generation 跟踪已取得它的 sender；Bind 与 Unbind 共用完整 handoff 串行边界；停止旧 writer 后等待 sender 收敛，将未消费消息按 FIFO 前插回 pending，再允许下一 generation drain / publish；覆盖 replacement、Unbind 和并发 Unbind/Bind |
| 标准 HTTP 流式 body 在限制前占满内存 | 在读取过程中执行硬上限，并覆盖 chunked / 无 Content-Length 请求 |
| Hertz 与 Gorilla 的 close error 类型不同 | 在 adapter 边界归一化，核心只认内部错误 |
| upgrade 后 request context 提前取消 | connection context 保留值但脱离请求取消，由 root context 管理 |
| Gin middleware 在 hijack 后继续写响应 | 明确 handler 生命周期约束，并增加 middleware 集成测试 |
| Proxy upgrade 失败泄漏并发配额 | 单一 admission 流程和一次性释放，覆盖失败与并发关闭测试 |
| upgrader 用户回调 panic 绕过 admission / 配额释放 | adapter 在同步 upgrade 调用边界执行 cleanup-on-unwind，清理后原样 re-panic；Server/Proxy × Hertz/Gin 覆盖 origin 与错误回调 |
| Close 与异步 upgrade 交错导致漏关连接 | closing admission 与连接登记采用同一生命周期门控，并增加竞态测试 |
| AgentFactory 或 ConnectionAwareAgent 注入 panic 逃逸并破坏连接收敛 | 统一 bootstrap panic boundary；HTTP 清理未登记资源，已升级 WebSocket 发送固定 1011 并关闭；测试禁止泄漏 panic 文本 |
| StreamerFactory 忽略 context 导致 shutdown 卡住 | 使用 context-bounded shutdown，超时返回错误，并明确 factory 的取消契约 |
| active 或 factory 清理分支先调用 `Streamer.Close`，阻塞后导致 Proxy 客户端 WebSocket 无法及时断开 | 所有已 upgrade 且已获得 Streamer 的分支统一遵循 close frame → northbound WS Close → Streamer Close；admission 在下游返回前保持 tracked，覆盖 active、factory 返回 Streamer + error 与 closing 竞态、Close 非阻塞、socket 及时关闭和 Shutdown deadline |
| HTTP connection Close 被阻塞 SSE writer 拖住，或已 detach / 已移除 session 的 writer 脱离 shutdown registry | connection Close 异步启动 session 清理并保留 lifecycle slot；connection 级 writer tracker 独立于 session map，shutdown 封闭新 writer admission 后等待所有 generation 实际退出 |
| HeaderForwarder 改造影响鉴权 metadata | 提供统一 ForwardHeaders helper，并做双框架 metadata 对照测试 |
| Gin 私有上下文值未传入 AgentFactory | 只承诺标准 request context，提供 middleware 迁移说明和集成测试 |
| 两框架对畸形握手判断不一致 | 共用 attempt 分类和握手 contract suite，失败请求不得回退到 SSE |
| session/close 后 HTTP SSE 仍被路由或重连 | 服务端在成功响应后移除 transport session；client wrapper 先标记 terminal intent 而不提前关流，断线时抑制重连；wire RPC error 或明确未发送时撤销 intent，结果不确定时完成 terminal；session/delete 不进入 close 状态机 |
| session/close 清理与已接受 SSE 尾部消息竞态 | 服务端以 outbox drain fence 排空 close handler 已成功发送的消息后再驱逐；客户端成功 close 后读取旧 GET 到 EOF，只有结果不确定错误才强制关闭；core fence 与双框架真实 TCP 重复测试固定行为 |
| 把 SSE transport session map 当作 Agent 业务 session 真值 | GET/reverse routing 使用 transport map；带合法 header 的 POST 继续 dispatch，由 Agent 返回 JSON-RPC 业务结果或错误，避免与 WS/stdio 语义分叉 |
| resume 成功但没有 transport session / listener | 服务端按 request ID 建立 provisional entry 并在失败时回滚；生成客户端 wrapper 自动启动 GET SSE；双框架真实 TCP 固定反向 update |
| 代理缓冲、超时或非粘性路由破坏 SSE | 文档给出部署约束，并增加真实代理与多实例集成测试 |
| 新增 Gin / Gorilla 增加依赖与构建时间 | Gin 只作为薄适配依赖，固定直接依赖版本并记录构建基线 |
| 两套 adapter 后续行为漂移 | contract suite 必须同时覆盖两种框架，新增行为不得只测试一个框架 |

## 19. 验收标准

- 同一个 AgentFactory 可以分别挂载到 Hertz 和 Gin，并通过 HTTP + SSE 与 WebSocket 全链路测试。
- 同一个 StreamerFactory 可以分别挂载到 Hertz Proxy 和 Gin Proxy，并通过透明转发全链路测试。
- Server / Proxy core 不再引用 Hertz 或 Gin 的请求上下文、router、upgrader 和 WebSocket Conn 类型。
- 官方公共接入提供四个独立 adapter（Server/Proxy × Hertz/Gin）；core 与 adapter 的配置、状态和关闭归属无重叠。core 另公开 adapter-facing SPI 供自定义宿主接入，自定义 adapter 必须自行执行等价的握手、Origin 与 admission 校验，不属于官方四 adapter 的行为兼容承诺。
- Streamable HTTP 协议 handler、WebSocket transport 和 Proxy pump 均只有一份实现。
- Hertz 与 Gin 的状态码、协议 header、SSE event、timeout 和 WebSocket close code 对齐。
- HTTP connection table 仍只管理 Streamable HTTP，WebSocket 不进入该表。
- 两种框架的 server close / proxy close 都能及时结束活跃连接，且无 goroutine、连接配额或 SSE listener 泄漏；无论处于 active、factory-error、timeout-late-result 或 closing-race 分支，下游 `Streamer.Close` 阻塞时北向 WebSocket 都必须先关闭，`Shutdown` 则继续等待或按 deadline 返回。
- shutdown 与 upgrade / initialize / downstream creation 并发时不会漏登、漏关或重复释放；deadline 到期会返回明确错误。
- HTTP connection Close / DELETE 不同步等待阻塞 SSE writer；ACPServer Shutdown 在已登记 dispatch、GET setup/listener 或任一 connection-owned session writer 尚未退出时不会返回成功。
- Hertz / Gin 的 Server 与 Proxy upgrader callback panic 均先释放 admission / 配额，再向外传播完全相同的 panic value。
- SSE listener 替换与 session 关闭并发不会 panic；replacement / Unbind（包括 Unbind 与下一次 Bind 重叠执行）以及 writer 写失败均不丢失已经被 `Send` / `SendLive` 接受但尚未写出的 outbox 尾部，下一 active listener 必须按 FIFO 恰好投递一次；writer 写失败后不再保留可接受 `SendLive` 的假活跃 generation。
- `session/close` 成功后，服务端 HTTP session 不再可路由，活跃 GET SSE 被驱逐，客户端对应 listener 停止且不会进入重连；客户端在发起 close 前只标记 terminal intent、保留当前流供 close handler 反向调用，明确 wire RPC error 或请求未发送时撤销 intent，超时/断线等结果不确定错误完成 terminal。
- `session/close` 清理前必须把 close handler 已由 `Send` 接受的 SSE 消息按 FIFO 写出；成功 close 的客户端继续读取原 GET 到 EOF，不因 POST response 与 GET 尾帧乱序而丢消息。
- SSE transport session map 只管理 GET/reverse routing，不抢占 Agent 对 POST 业务 session 的判断；`session/resume` 成功后必须建立 request session ID 对应的 transport session 和 GET listener，并能完成反向消息。
- `session/delete` 不得自动移除 transport session 或停止 listener；Agent 采用“从 session/list 删除但活跃会话继续”的合法语义时，后续 Prompt 和反向 SessionUpdate 仍成功。
- AgentFactory 与 ConnectionAwareAgent 注入 panic 均被限制为单连接 setup failure；已升级 WebSocket 返回固定 1011，不向 peer 泄漏 panic 内容。
- chunked 和未知 Content-Length 的超大请求在读入上限处被拒绝，不产生无界内存增长。
- 双框架真实 TCP 测试证明 WebSocket 能 upgrade、Server core Close 对等输出 1000、`TestStreamableHTTPAdapterContract` 能在 Hertz/Gin 的有效 session GET 响应保持打开时读到首个 `:keep-alive`、Hertz 不会以默认 4 MiB 宿主上限抢先拒绝 SDK 允许的 body，并固定 Proxy 的 4001/1001 与双向 1009。该结论不扩展到第 0 节已降级的 P2 项，也不扩展到尚未获得双框架 wire-level 覆盖的 pending FIFO 或 listener 替换；文档明确反向代理缓冲与多实例粘性路由约束。
- go vet、全量测试和 race 测试全部通过。
- 中英文 README 与示例完整说明两种框架的启动方式、WebSocket 限制和配置归属。

## 20. 最终建议

本次重构应按“先抽边界、再加 Gin”的顺序推进，不应在现有 Hertz handler 旁直接复制 Gin handler。最关键的两个抽象点是统一 HTTP 请求响应上下文，以及统一 WebSocket 连接与错误语义。完成这两点后，ACPServer 与 ACPProxy 的核心逻辑可以自然复用，后续修复协议、心跳或限流问题也只需要修改一处。
