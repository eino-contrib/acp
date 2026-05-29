# WebSocket Ping/Pong 心跳方案

## 背景

本方案实施前，ACP SDK 的 WebSocket 心跳行为不统一：

- **SDK Client Transport**：无 ping/pong，无 read deadline
- **SDK Server**（internal WS transport）：无 ping/pong，无 read deadline
- **Proxy**：Server 主动 Ping + PongHandler，per-connection 起 pingPump goroutine

Proxy 北向连接的 Client 也是我们的 SDK Client，因此 Proxy 自己发 Ping 没有必要——应该统一让 Client 负责心跳。

### 问题

1. **半开连接泄漏**：对端崩溃或网络断开 → read 永远阻塞，goroutine 和内存泄漏
2. **NAT / LB 空闲超时**：中间设备通常 30s~5min 空闲超时，无心跳帧会被踢掉
3. **行为不一致**：Proxy 是 Server 发 Ping，SDK 直连没有心跳，两套逻辑

## 设计决策

### 统一为 Client 主动 Ping

所有场景（SDK 直连、Proxy 北向）统一为 **Client 发 Ping，Server/Proxy 安装 PingHandler 刷新 read deadline**。

| 角色 | 职责 |
|------|------|
| **Client** | 起 pingPump，周期发 Ping（默认 30s）；设 PongHandler 刷新自身 read deadline |
| **Server / Proxy** | 设 read deadline（默认 0 不启用，生产推荐 75s）；安装 PingHandler 刷新 deadline 并回 Pong；读到 ACP text message 也刷新 deadline（Proxy 额外接受 BinaryMessage） |

**关键约束**：hertz-contrib/websocket 中 Ping/Pong 是 control frame，由库内部处理，不会作为 ReadMessage() 的返回值。因此 Server/Proxy **必须**通过 SetPingHandler 主动刷新 read deadline，不能仅依赖 ReadMessage() 返回时刷新。

### 关键库行为依赖

本方案依赖 `hertz-contrib/websocket`（当前版本 v0.2.0，基于 gorilla/websocket 端口）的以下行为，实现前需验证：

| 假设 | 依据 | 验证方式 |
|------|------|----------|
| Control frame (Ping/Pong) 不从 ReadMessage() 返回，由库内部 handler 消费 | gorilla/websocket 文档 + 源码 `conn.go` advanceFrame() | 基于 hertz-contrib/websocket v0.2.0 源码审查确认；Proxy 已有真实 control frame 集成测试（TestProxyPingBeforeFirstFrame / TestProxyPingAfterFirstFrame）验证库 dispatch 行为；Client/Server 直连路径可补充同类测试 |
| WriteControl 与 WriteMessage 共享底层 `mu` 写锁，并发调用安全（库内部串行化） | 源码 `conn.go` WriteControl() 实现 | 源码审查确认；race test 验证调用方使用非零 deadline 无竞争 |
| WriteControl deadline=0 近似无界等待写锁 | 源码 `conn.go` WriteControl()：deadline 为零时使用 `d = 1000 * time.Hour`，通过 channel select + timer 等待写锁，效果近似无界 | 源码审查确认；当前测试仅验证调用方统一使用非零 deadline |

> 若 hertz-contrib/websocket 升级后上述行为变化，需重新评估方案。建议在 go.mod 中 pin 到已验证版本。

**理由**：

1. Server / Proxy 不需要 per-connection pingPump goroutine，连接数多时开销更小
2. NAT 保活是客户端方向的需求，Client 发帧才能保住 NAT 表项
3. Client 自行感知连接死亡（Pong 超时），可自行决策重连
4. Proxy 北向也是我们的 SDK Client，统一行为、减少维护成本

## 详细设计

### Client 端

新增心跳 Options：
- `WithPingInterval(d)` — Ping 发送间隔，默认 30s，0 禁用（见配置约束章节）
- `WithReadTimeout(d)` — read deadline，默认 75s，0 禁用（不推荐）

行为：
1. 连接成功后安装心跳——设初始 read deadline + PongHandler
2. 读循环每次读到 ACP text message 也刷新 read deadline（活跃连接不会被误杀）
3. 独立 pingPump goroutine 周期发 Ping 帧（通过 WriteControl 写入，与 WriteMessage 共享底层连接写锁，但 WriteControl 支持独立 deadline，不会被长数据帧写入无限阻塞）
4. Ping 写失败收敛：写失败 → 设 terminal error → 关底层 ws → 读循环退出 → 上层感知

pingPump 生命周期：
- `PingInterval=0` 时不启动，Close() 跳过等待不 hang
- 通过 done channel 通知退出
- 退出时释放 ticker 资源

- Close() 行为：
  - 关闭 done channel → best-effort 通过 WriteControl 发送 close frame（5s deadline）→ 关闭底层 ws（unblock 阻塞的 ReadMessage）→ 等待读循环和 pingPump 退出 → 释放资源

### Server 端

新增 Options：
- `WithReadTimeout(d)` — WS 连接 read deadline，默认 0（不启用），生产推荐 75s，0 禁用
- `WithInitializeTimeout(d)` — 首条消息必须是合法的 initialize request 并在此时间内到达，否则断连；默认 15s，0 禁用

行为：
1. WS 升级成功后设 initialize deadline，此阶段 PingHandler 不刷新 deadline
2. 收到合法 initialize request 后切换到正常 read deadline，此后 PingHandler 正常刷新 deadline
3. PingHandler 始终回 Pong（echo Ping payload，符合 RFC 6455 §5.5.3）
4. 读循环中每次读到 ACP text message 也刷新 read deadline
5. Read deadline 超时 → 关连接、释放资源

Initialize Timeout 设计：限制未完成初始化的 WebSocket 连接占用资源的最长时长。当前实现中 Agent / AgentConnection 会在 WebSocket 升级前创建（newWSConn 内部已调用 AgentFactory 并启动 AgentConnection，随后才执行 Upgrade），因此该 timeout 不能避免资源创建，只能限制资源占用时间。初始化等待阶段收到 Ping 仍回 Pong（协议要求），但不刷新 initialize deadline。

### Control Frame Write Deadline

**所有角色**的所有 control frame（Ping、Pong、Close）必须使用 `WriteControl` 而非 `WriteMessage`，且必须设非零 deadline，统一为 5s。

| 场景 | deadline |
|------|----------|
| Client pingPump 发 Ping | 5s |
| Server/Proxy PingHandler 回 Pong | 5s |
| Close frame (best-effort) | 5s |

**迁移要求**：当前 Client Close（`WriteMessage(CloseMessage)`）和 Proxy Close（`writeWSMessage(CloseMessage)`）都必须迁移到 `WriteControl(CloseMessage, ..., deadline=now+5s)`。

**与应用层写串行化的关系**：`WriteControl` 不经过应用层 `writePermit`（Client）或 `wsWriteMu`（Proxy），直接使用库内部写锁 + 独立 deadline。原因：Close 和 Pong 是对异常/协议事件的响应，必须在较短时间内完成，不能排队等待大数据帧写完。应用层串行化仅约束普通 data frame。注意：WriteControl 的 deadline（5s）仍然需要等待库内部写锁（与 WriteMessage 共享），因此如果一个 data frame 写入持续超过 5s（如网络异常导致 TCP 写阻塞），WriteControl 仍可能超时。

**原因**：hertz-contrib/websocket 的 WriteControl 在 deadline 为零时使用 1000h timer + channel select 等待写锁，效果近似无界等待。如果某个数据帧写入长时间阻塞，零 deadline 的 WriteControl 会导致 pingPump 不退出、Close() hang。选择 5s 是在"容忍正常写锁竞争"和"检测异常连接"之间的平衡。

### WriteControl 并发模型

WriteControl（Ping/Pong/Close）与 WriteMessage（数据帧）**共享底层连接写锁**。并发调用是安全的（库内部串行化），但 WriteControl 会等待正在进行的 WriteMessage 完成。

因此：
- WriteControl 必须设置合理 deadline（5s），既能容忍正常 data frame 写入时的锁竞争，又能在连接真正不可用时及时失败
- 普通数据帧写入仍应保持单写者模式或应用层串行化
- WriteControl 不是"独立锁"也不是"不竞争"，而是"共享锁 + 独立 deadline"

### Server public 配置链路

internal/wsserver 是 internal 包，外部用户无法直接配置。Public option 从 server 包向下穿透到 internal/wsserver。

最小实现链路：

1. **Public option 定义**（server 包）：
   - `server.WithWebSocketReadTimeout(d)` — 穿透到 wsserver 的 read deadline
   - `server.WithWebSocketInitializeTimeout(d)` — 穿透到 wsserver 的 initialize deadline

2. **ACPServer 新增字段**：
   - `wsReadTimeout time.Duration`
   - `wsInitializeTimeout time.Duration`

3. **穿透方式**：`newWSConn()` 调用 `wsserver.New(wsserver.WithReadTimeout(...), wsserver.WithInitializeTimeout(...))`，将 ACPServer 持有的配置值传入

4. **默认值归属**：默认值只在 `server` 层维护（WithWebSocketReadTimeout 默认 0，WithWebSocketInitializeTimeout 默认 15s）。`internal/wsserver` 层不维护默认值，只消费传入的最终配置；传入 0 表示禁用

### Proxy 改动

**Deprecated**（非 breaking，保留到下一个 major 版本）：
- `WithWebSocketPingInterval` — 标记 deprecated，内部不再使用，保留编译兼容
- `WithWebSocketPongTimeout` — 标记 deprecated，内部映射到 `WithWebSocketReadTimeout`

**新增**：
- `WithWebSocketReadTimeout` option（替代原 PongTimeout 语义）
- `WithWebSocketFirstFrameTimeout` option（默认 15s，0 禁用；首条 data frame 必须在 streamer 创建后此时间内到达，非 WS upgrade 后立即计时）

**行为**：
- 安装 PingHandler（回 Pong echo payload）+ 设初始 first-frame deadline
- 首帧前：PingHandler 只回 Pong，不刷新 read deadline（防止只发 Ping 不发业务帧持续占用连接和 downstream streamer）
- 首帧后：切换到正常 read deadline，此后 PingHandler 正常刷新
- 数据帧刷新 read deadline 的逻辑保留

保护边界说明：downstream streamer 在 first-frame timeout 安装之前已创建。first-frame timeout 的保护含义是限制 downstream streamer 被空占的最大时长（默认 15s），而非阻止 streamer 被创建。更强保护需改架构，后续优化。

## 配置约束

| 组合 | 行为 | 是否推荐 |
|------|------|----------|
| Client `PingInterval=30s` + Server/Proxy `ReadTimeout=75s` | 正常保活，2.5 倍容忍 | **推荐（生产显式配置）** |
| Client `PingInterval=0` + Server/Proxy `ReadTimeout=75s` | Client 不发 Ping，若业务也无上行数据帧，Server/Proxy 将在 75s 后主动断连 | **不推荐** |
| Client `PingInterval=0` + Server/Proxy `ReadTimeout=0` | 双方均无超时，半开连接永久泄漏 | **仅用于调试** |
| Client `PingInterval=N` + Server/Proxy `ReadTimeout=0` | Client 保持 Ping 但 Server 不设 deadline，Server 侧半开连接泄漏 | **不推荐** |

### 本地可校验规则

以下规则在**同一进程**内可校验，违反时打 warn 日志，不强制修正：

| 角色 | 规则 | 原因 |
|------|------|------|
| Client | `PongTimeout >= 2 × PingInterval`（推荐 2.5×） | 容忍 1 次 Ping 丢帧 + 网络抖动 |
| Client | `PingInterval=0 && PongTimeout>0` → warn | 无下行 data frame 时 Client 会超时 |
| Client | `PingInterval>0 && PongTimeout=0` → 允许 | Client 发 Ping 保活但不做超时检测 |

### 部署约束（跨进程，无法自动校验）

以下是 Client 与 Server/Proxy 之间的配比关系，只能通过文档、示例、集成测试和 metrics 观察保证：

- `Server/Proxy ReadTimeout >= 2 × Client PingInterval`——否则健康连接可能被误杀
- Client PongTimeout 和 Server/Proxy ReadTimeout 建议对齐（生产推荐均配置为 75s）
- Server/Proxy `ReadTimeout>0` 时，必须确保上游 Client 会发送 Ping 或周期性数据帧（Server/Proxy 无法在本进程内自动校验上游 Client 的 PingInterval 配置）

文档和 GoDoc 中需注明推荐组合，但 Option 层**不做跨端校验**。

### 边界输入处理规则

| 输入 | 行为 |
|------|------|
| `d < 0` | 视为无效输入，忽略（不修改默认值），打 warn 日志 |
| `d == 0` | 禁用对应机制（不设 deadline / 不启动 pingPump） |
| `0 < d < 1s` | 允许但打 warn（极小值通常只在测试中使用，生产不建议低于 1s） |
| `PingInterval=0` | 应视为高级选项，文档和 godoc 中需标注风险 |

### 兼容不发 Ping 的 Client

本方案以 SDK Client 发送协议层 Ping 为前提，但必须兼容不发 Ping 的 Client（旧版 SDK、浏览器 WebSocket、第三方实现）。

**兼容策略**：

1. **Server/Proxy ReadTimeout 默认为 0**（不启用）——initialize / first-frame 阶段完成后，不会因读空闲断开连接。注意：`InitializeTimeout`（Server 默认 15s）和 `FirstFrameTimeout`（Proxy 默认 15s）仍独立生效；如需完全禁用初始阶段超时，需显式配置 `WithWebSocketInitializeTimeout(0)` / `WithWebSocketFirstFrameTimeout(0)`。
2. **ReadTimeout 仅在显式配置后生效**——生产环境需确认所有上游 Client 已升级至新 SDK（会主动 Ping）后，再启用 ReadTimeout。
3. **数据帧也刷新 read deadline**——即使 Client 不发 Ping，只要在 ReadTimeout 周期内有上行数据帧（如 JSON-RPC 请求），连接也不会被断开。这意味着高频交互的 Client 即使不发 Ping 也能存活。
4. **PingHandler 始终安装**——无论 ReadTimeout 是否启用，Server/Proxy 都安装 PingHandler 并正确回 Pong。如果旧 Client 恰好发了 Ping（比如底层库自带），Server 也能正确响应。

**总结**：不发 Ping 的 Client 在 ReadTimeout=0 时完全不受影响；在 ReadTimeout>0 时，只要有周期性上行数据帧也能存活；只有既不发 Ping 也无上行数据帧的空闲连接才会被超时清理。

## 版本发布与兼容策略

### 兼容矩阵

| Client | Server/Proxy | 行为 | 是否安全 |
|--------|-------------|------|----------|
| 新 SDK（发 Ping） | 新版（ReadTimeout=75s） | 正常保活 | 安全 |
| 旧 SDK（不发 Ping） | 新版（ReadTimeout=75s） | 75s 后断连 | **不安全** |
| 浏览器/第三方（不发 Ping） | 新版（ReadTimeout=75s） | 75s 后断连 | **不安全** |
| 新 SDK（发 Ping） | 旧版（无 ReadTimeout） | Client 侧 Ping 正常发出，Server 不刷新，不影响 | 安全 |

### 本次发布策略

- Client 默认启用 Ping（PingInterval=30s, PongTimeout=75s）
- Server/Proxy 新增 `ReadTimeout` 能力，**默认值为 0**（不启用），保持旧行为不 break 旧 Client
- 生产环境**推荐显式配置** `ReadTimeout=75s`，确认所有 Client 已升级后再启用
- 旧 `WithWebSocketPingInterval` 和 `WithWebSocketPongTimeout` 标记 deprecated 但保留编译兼容；`WithWebSocketPongTimeout` 内部映射到 `WithWebSocketReadTimeout`

> **未来可选演进**：当确认线上 Client 均已升级后，可将 Server/Proxy `ReadTimeout` 默认值切换为 75s（breaking change，需 major 版本）

### Deprecated Option 迁移表

| 旧 Option | 旧行为 | 新行为（升级后） | 用户需要做什么 |
|-----------|--------|-----------------|----------------|
| `WithWebSocketPingInterval(d)` | Proxy per-connection 起 pingPump，每 `d` 主动发 Ping | **内部不再使用**，Proxy 不再主动发 Ping。编译通过但行为消失 | 升级 SDK Client 使其主动 Ping；若有非 SDK Client，确保其发协议层 Ping 或业务层有上行数据帧，否则不要启用 Server/Proxy ReadTimeout |
| `WithWebSocketPongTimeout(d)` | Proxy 在 `d` 内未收到 Pong 则断连 | **内部映射到 `WithWebSocketReadTimeout(d)`**，但注意该 read timeout 只在首个 data frame 到达后生效；首帧前由 `WithWebSocketFirstFrameTimeout` 控制，若 first-frame timeout 为 0 则首帧前无 read deadline。**前提是 Client 主动发送 Ping 或周期性 data frame**；若旧部署依赖 Proxy 主动 Ping 保活 idle 连接，升级后 idle Client 将因无上行帧而被断连 | 确保 Client 已升级为主动 Ping（或有周期性上行数据帧）。建议迁移到 `WithWebSocketReadTimeout` 以避免未来 deprecation 移除后编译失败 |

**关键提醒**：`WithWebSocketPingInterval` 升级后 Proxy **不再有主动保活能力**。如果部署环境中 NAT/LB 依赖 Proxy→Client 方向的帧来维持映射，必须确保 Client 已升级为主动 Ping，否则 NAT 表项可能因空闲超时被清除。

### 灰度启用 ReadTimeout 指引

Server/Proxy `ReadTimeout` 默认为 0（不启用），启用需要确认所有上游 Client 均能在 ReadTimeout 周期内发送帧。以下是推荐的灰度流程：

1. **观测阶段**（不启用 ReadTimeout）：部署新版后，通过业务自建 metrics 或抓包确认新 SDK Client 的 Ping 正常发送。SDK 当前不内置 Ping/Pong 成功日志或 metrics；`pong write failed` WARN 是**异常信号**（表示 Server/Proxy 回 Pong 失败），不能作为健康确认依据。如需量化，建议接入自定义 metrics（如 `acp_ws_ping_received_total`）
2. **识别旧 Client**：通过 access log / 连接元信息（如 User-Agent、SDK version header）统计仍在使用旧 SDK 或非 SDK Client 的连接占比
3. **按环境分批启用**：staging → 灰度（低流量 endpoint / tenant）→ 全量。每批通过日志 `read timeout` WARN 或业务自建 metrics（如 `acp_ws_timeout_total{type=read_timeout}`）观察是否有非预期增长
4. **混合 Client 环境**：若某个 endpoint 同时服务新旧 Client，该 endpoint **不启用 ReadTimeout**，直到旧 Client 占比降为 0
5. **退出条件**：连续观察期（建议 ≥ 3 天）内 read_timeout close 均为预期场景（如 Client 正常断连漏发 close frame），无非预期断连

### 回滚方案

- Server/Proxy 侧：将 `ReadTimeout` 设回 0 即可恢复旧行为（不 deadline），无需代码回滚
- Client 侧：将 `PingInterval` 设为 0 可禁用 Ping，不影响 Server

## 数据流

```
SDK Client                          Server / Proxy
    │                                     │
    │  ── WebSocket Upgrade ────────────▶ │
    │                                     │  设 initialize/first-frame deadline (15s)
    │                                     │  安装 PingHandler（回 Pong echo appData，不刷新 deadline）
    │                                     │
    │  Ping (if configured) ────────────▶ │  PingHandler: 回 Pong(appData)，但 NOT 刷新 deadline
    │◀─────────── Pong(appData) ─────────  │
    │                                     │
    │  initialize request ──────────────▶ │  validate → 切换到正常 read deadline (75s)
    │                                     │  此后 PingHandler 正常刷新 deadline
    │                                     │
    │  pingPump: Ping (every 30s) ──────▶ │  PingHandler: read deadline 刷新 + WriteControl(Pong, 5s deadline)
    │◀─────────── Pong(appData) ─────────  │
    │  PongHandler: read deadline 刷新    │
    │                                     │
    │  数据帧 ──────────────────────────▶ │  ReadMessage 返回 → read deadline 刷新
    │◀─────────── 数据帧 ────────────────  │  Client readLoop → read deadline 刷新
    │                                     │
    │  Client 崩溃 → 不再发任何帧         │
    │                                     │  75s 超时 → 关连接
    │                                     │
    │  Server 崩溃 → 不回 Pong            │
    │  75s 超时 → 感知死亡                │
    │                                     │
    │  只发 Ping 不发 initialize/data     │
    │                                     │  15s initialize/first-frame timeout → 关连接
    │                                     │
    │  Ping 写失败 (WriteControl 5s超时)  │
    │  → set termErr → close ws           │
    │  → readLoop 退出 → 上层感知         │
    │                                     │
    │  Close() 调用                       │
    │  → close(done)                      │
    │  → pingPump 退出 (select done)      │
    │  → best-effort close frame (5s)     │
    │  → wsConn.Close() (unblock read)    │
    │  → readLoop 退出 (read error)       │
    │  → 等待 readDone (+pingDone if set) │
    │  → 释放资源                         │
```

## 默认参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| Client `PingInterval` | 30s | 0 禁用（见配置约束）；若目标环境 NAT idle timeout 恰好为 30s，应调低至 20~25s |
| Client `PongTimeout` | 75s | ≈ 2.5 × PingInterval，容忍 1-2 次丢帧；PingInterval=0 时也独立生效 |
| Server/Proxy `ReadTimeout` | 0（默认不启用，生产推荐显式配置 75s） | 启用后与 Client PongTimeout 对齐 |
| Server `InitializeTimeout` | 15s | 首条 data frame 必须通过 initialize 校验，防止未初始化连接占用资源 |
| Proxy `FirstFrameTimeout` | 15s | 首条 data frame 必须在此时间内到达（Proxy 不校验内容，只要求有 data frame） |
| Control frame write deadline | 5s | 所有 WriteControl（Ping/Pong/Close）统一；需高于正常 data frame 写入延迟以避免写锁竞争误杀 |

## 影响范围

| 模块 | 改动概述 |
|------|----------|
| Client Transport (ws) | 新增心跳 option、pingPump、PongHandler、read deadline 刷新、Ping 写失败收敛、Close 生命周期 |
| Server (internal wsserver) | 新增 read deadline、PingHandler、initialize timeout、读循环 deadline 刷新 |
| Server (public) | 新增 public option 穿透到 internal wsserver |
| Proxy | 删除 pingPump；旧 option deprecated 并映射；新增 PingHandler + read deadline + first-frame timeout |
| README / README.zh-CN | 更新 Proxy 示例和 Keepalive 章节，迁移到新 option |

## 不做的事

1. **不做客户端自动重连**：本次只做心跳保活与死亡检测，重连后续单独设计
2. **不做应用层心跳（JSON-RPC ping）**：WebSocket 协议层 Ping/Pong 帧即可

## 错误分类与 Close Code

| 场景 | 触发端 | local error | 是否发 close frame | close code | metric reason | 日志字段 |
|------|--------|-------------|-------------------|------------|---------------|----------|
| Client Pong timeout (read deadline exceeded) | Client | `i/o timeout` | 否（超时后直接关底层连接） | 无（不发 close frame） | `pong_timeout` | local_conn_id, timeout |
| Client Ping write failed | Client | `WriteControl` 返回的底层 error | 否（写失败意味着连接已不可用） | 无 | `ping_write_failed` | local_conn_id, err |
| Server/Proxy read timeout | Server/Proxy | `i/o timeout` | 是，best-effort | 1001 (Going Away) | `read_timeout` | conn_id, timeout, role |
| Server initialize timeout | Server | `i/o timeout` | 是，best-effort | 4000 (Initialize Timeout，自定义) | `initialize_timeout` | conn_id, timeout |
| Proxy first-frame timeout | Proxy | `i/o timeout` | 是，best-effort | 4001 (First Frame Timeout，自定义) | `first_frame_timeout` | conn_id, timeout |
| PingHandler WriteControl(Pong) failed | Server/Proxy | `WriteControl` 返回的底层 error | 否 | 无 | `pong_write_failed` | conn_id, err |
| Client 主动 Close() | Client | 无 | 是，best-effort | 1000 (Normal Closure) | `normal_close` | local_conn_id |
| Server 主动 Close() | Server | 无 | 是，best-effort | 1000 (Normal Closure) | `normal_close` | conn_id |
| Proxy 主动 Close() / shutdown | Proxy | 无 | 是，best-effort | 1001 (Going Away) | `normal_close` | conn_id |

**自定义 close code 范围**：4000-4999 为应用自定义区间（RFC 6455 §7.4.2），本方案使用 4000-4001。

**best-effort close frame**：所有 close frame 通过 `WriteControl(CloseMessage, payload, deadline=now+5s)` 发送，失败不阻塞关闭流程。

## 成功指标

| 指标 | 验收标准 |
|------|----------|
| 半开连接检测 | Server/Proxy 启用 ReadTimeout 后，在 Client 崩溃后 ≤ ReadTimeout 内发现并释放连接 |
| Client 死亡感知 | Server 不回 Pong 时，Client 在 ≤ PongTimeout（默认 75s）内返回 terminal error |
| Proxy goroutine 开销 | Proxy 不再为每条北向连接启动 pingPump goroutine |
| goroutine 无泄漏 | Close() 后 readLoop 和 pingPump 均已退出，无残留 goroutine |
| race 安全 | `go test -race ./...` 通过 |
| 相关单测通过 | 下方测试清单全部 PASS |
| 新旧版本兼容 | 旧 Client + 新 Server（ReadTimeout=0）不断连；新 Client + 旧 Server 正常工作 |

## 日志规范

心跳与超时相关事件通过 WARN 级别日志输出，包含可 grep 的关键信息（key=value 风格），便于线上排障：

| 事件 | 级别 | 关键信息 |
|------|------|----------|
| Client Ping write failed | WARN | local_conn_id, err |
| Client Pong timeout (read deadline exceeded) | WARN | local_conn_id, timeout |
| Server/Proxy read timeout → close | WARN | conn_id, timeout |
| Server initialize timeout → close | WARN | conn_id, timeout |
| Proxy first-frame timeout → close | WARN | conn_id, timeout |
| PingHandler WriteControl(Pong) failed | WARN | conn_id, err |
| 配置不变量违反 | WARN | option, value, constraint |

### conn_id 设计

- **Server / Proxy**：连接建立时生成 UUID 作为 conn_id（已有实现），所有日志事件使用该 ID
- **Client**：连接建立时本地生成 UUID 作为 local_conn_id，用于 Client 侧日志关联
- 跨端关联：当前不做。如果未来需要跨端日志关联，可通过协商（如 initialize 响应携带 server conn_id）解决

## 测试覆盖

测试约束：单测统一使用 10ms~100ms 级别 timeout 配置，避免真实 15s/75s 等待。

主要覆盖场景：
- Client 周期 Ping / Pong 超时 / Ping 写失败收敛 / Close() 生命周期
- Server initialize timeout / read timeout (close code 1001) / PingHandler echo
- Proxy first-frame timeout / read timeout / PingHandler
- deprecated option 编译兼容与等价映射
- 配置边界（0 值禁用、负数忽略）
- `go test -race` 并发安全

建议后续补充：
- 极小值（<1s）warn 日志断言
- 新旧版本兼容矩阵（旧 Client + 新 Server、新 Client + 旧 Server）
- 真实 control frame 集成测试（验证 Ping/Pong 的库 dispatch 行为，而非仅手动调用 handler）
- server 包 option 穿透测试（验证 WithWebSocketReadTimeout / WithWebSocketInitializeTimeout 通过 newWSConn 传给 internal wsserver）

## 关键风险项

| 风险 | 说明 | 缓解 |
|------|------|------|
| Close() / pingPump 生命周期 | Close() 必须先关 ws 再等 goroutine，顺序不对会 hang 或 panic | TestCloseDoesNotHang / TestCloseDoesNotHangPingIntervalZero / TestCloseMustCloseWSToUnblockReadMessage 覆盖；race test 必须通过 |
| WriteControl 与 WriteMessage 共享写锁 | WriteControl 和 WriteMessage 竞争同一个连接内部锁，WriteControl 必须设合理 deadline 以容忍正常写锁竞争 | 统一 5s deadline（高于正常 data frame 网络延迟，低于 data frame write timeout 30s）；TestWriteControlPingUsesControlWriteDeadline / TestPingHandlerWriteControlUsesControlWriteDeadline 覆盖 |
| PingInterval 默认值与 NAT 边界 | 默认 30s 恰好等于部分 NAT/LB 的最短 idle timeout | 文档在配置约束中注明应小于 NAT idle timeout；若目标环境 idle=30s，用户需调低至 20~25s |
| read deadline 配置误杀 | ReadTimeout 过短 + 网络抖动可能误断健康连接 | 默认 2.5× 容忍；配置章节要求 `>= 2×PingInterval`，违反时打 warn |
| read deadline 覆盖边界 | read deadline 只检测 socket read 空闲/半开，不覆盖读出消息后的业务转发阻塞（如 Proxy streamer.WritePayload 或 Client/Server inbox 满） | Proxy upPump 对 WritePayload 增加 per-call timeout（30s）；Client/Server inbox 投递有 done channel 兜底 |
| Proxy first-frame timeout 保护有限 | downstream streamer 在 timeout 前已创建，无法阻止短时资源占用 | 当前只承诺限制占用时长（默认 15s）；更强保护需改架构，后续优化 |
| 旧 Client 被断连 | 旧 SDK Client 不发 Ping，新 Server 若启用 ReadTimeout 会断连 | Server/Proxy 默认 ReadTimeout=0（不启用）；生产环境确认 Client 全部升级后再显式配置 75s |

## 生产部署指引

本节为使用方在生产环境部署时的参考，SDK 本身不强制实现。

### 建议观测的 Metrics

> **注意**：以下指标为建议业务方自行接入的外部 metrics，SDK 本身不内置 metrics 上报。SDK 提供结构化 WARN 日志作为对应事件信号。

| Metric | 说明 |
|--------|------|
| `acp_ws_active_connections{role}` | 当前活跃连接数 |
| `acp_ws_close_total{role,reason,code}` | 连接关闭计数（按原因和 close code 分） |
| `acp_ws_timeout_total{role,type}` | 超时关闭计数（type: read_timeout / initialize_timeout / first_frame_timeout） |
| `acp_ws_ping_write_failed_total{role}` | Ping 写失败计数 |
| `acp_ws_pong_write_failed_total{role}` | Pong 写失败计数 |

### 建议告警

- read timeout close rate 突增（可能旧 Client 未升级或网络异常）
- initialize/first-frame timeout 突增（可能有恶意连接或 Client 逻辑异常）
- active connections 异常下降（可能误配 ReadTimeout）

### 容量估算公式

- Ping/Pong QPS = 活跃连接数 / PingInterval（如 10w 连接 / 30s ≈ 3333 Ping/s + 3333 Pong/s）
- 单条 Ping/Pong 帧：2~6 bytes control frame header + payload（通常为空或几 bytes）
- Client 侧新增 1 goroutine（pingPump）+ 1 ticker per connection
- Proxy 侧移除原有 pingPump goroutine，净减少 1 goroutine per proxied connection；Server 侧无变化（仅安装 PingHandler/read deadline，无新增 goroutine）

## Known Design Constraints

### Control Frame Deadline vs Data Write Timeout

All control frames (Ping, Pong, Close) use `WriteControl` with a 5s deadline. Data frame writes (`WriteMessage`) default to 30s timeout. Both share the websocket library's internal write lock.

**Implication**: If a data frame write holds the internal write lock for more than 5s (e.g. large payload + slow network), a concurrent `WriteControl` call will timeout. This causes:
- Client: pingPump treats it as `ping_write_failed` and closes the connection
- Server/Proxy: Pong write fails, logged as `pong_write_failed`

This is a **deliberate trade-off**: 5s is chosen to balance between tolerating normal write-lock contention and detecting truly broken connections. In practice, data frame writes rarely exceed 5s on healthy networks. If this becomes an issue in specific deployments (e.g. very large payloads over high-latency links), operators should:
1. Reduce max message size to keep individual writes short
2. Or accept that heartbeat may declare the connection dead during extreme write latency

### Read Loop Backpressure and Heartbeat Interaction

The read loop dispatches data frames synchronously:
- Client/Server: blocking send to `inbox` channel
- Proxy: blocking call to `streamer.WritePayload`

During this blocking period, the read loop cannot call `ReadMessage()`, so:
- Incoming Pong frames are not processed (PongHandler does not fire)
- Incoming Ping frames are not processed (PingHandler does not fire, no Pong reply)

**Impact by role**:

- **Proxy**: `WritePayload` has a per-call context timeout (default 30s). If downstream is slow, the write times out, the read loop resumes, and the next `ReadMessage()` hits the expired read deadline — connection closes. Read deadline effectively acts as a backpressure circuit breaker for Proxy.
- **Client/Server**: The read loop blocks on a Go channel send (`inbox <- msg`). If `inbox` is full and the consumer is stalled, the goroutine is stuck in the channel send — it never returns to `ReadMessage()`. The socket read deadline only fires during an active `Read()` syscall, so it **cannot** interrupt a blocked channel send. The connection will only close when `done` channel is closed or the consumer resumes and the next `ReadMessage()` sees the expired deadline.

**Consequence**: For Client/Server, read deadline is NOT a backpressure circuit breaker when the consumer is permanently stalled. It only triggers if the consumer eventually unblocks and the read loop re-enters `ReadMessage()`.

**Recommendation for operators**: Ensure the application layer continuously drains messages from `ReadMessage()` / the inbox channel. If processing a single message may take a long time, offload it to a worker pool rather than blocking the read loop consumer. Do not rely on read deadline alone to protect against permanently stalled consumers on Client/Server — implement application-level consumption timeouts or monitoring if needed.

