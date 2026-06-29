# v1 unstable schema 同步：破坏性变更与迁移说明

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

本次同步上游 `agent-client-protocol` 的 v1 unstable schema 并重新生成代码。除了反序列化容错、import 排序、required 校验等纯生成器改进之外，schema 本身在方法命名与方法集合上发生了变化。由于根包 `acp.Agent` / `acp.Client` 接口、其方法的线名常量、以及 `conn` 出入站方法都是 schema 的生成产物，这些 schema 变化会直接体现为**公开接口的破坏性变更（breaking change）**。

本文目的：把这次破坏性变更显式记录下来，给下游一份「旧 → 新」对照，明确删除项与替代方案，避免下游升级时只看到编译报错而不知缘由。

## 兼容性定位

- 本模块当前处于 `v0.x` 阶段（最新发布 tag 为 `v0.0.3`）。按 Go module 语义，`v0.x` 允许在 minor 之间引入破坏性变更，无需 major 边界。
- 被改名 / 删除的方法绝大多数带 `Unstable` 前缀，其文档注释原文即声明 “This capability is not part of the spec yet, and **may be removed or changed at any point**”。本次变更属于该契约本身承诺的行为。
- 因此本次**不**为旧方法 / 旧常量保留 deprecated 别名或兼容层：这些符号全部来自 schema 生成，手写别名既不在 schema 内，也会在下一次重新生成时被覆盖，与生成流程冲突。需要兼容的下游应直接按下表迁移到新符号。

## 破坏性变更对照

### 1. `Unstable*` 方法转正（去掉 Unstable 前缀）

这些方法在新 schema 中已脱离 unstable 区，方法名、线名常量、`conn` 出入站方法同步改名：

| 旧符号 | 新符号 | 线名（不变） |
| --- | --- | --- |
| `Agent.UnstableLogout` | `Agent.Logout` | `logout` |
| `Agent.UnstableCloseSession` | `Agent.CloseSession` | `session/close` |
| `Agent.UnstableResumeSession` | `Agent.ResumeSession` | `session/resume` |

对应的线名常量也随之改名：`MethodAgentUnstableLogout` → `MethodAgentLogout`、`MethodAgentUnstableCloseSession` → `MethodAgentCloseSession`、`MethodAgentUnstableResumeSession` → `MethodAgentResumeSession`。线名字符串本身保持不变，跨实现的线协议兼容不受影响。

### 2. 单复数命名修正（provider 系列）

schema 将 provider 系列方法由复数改为单数：

| 旧符号 | 新符号 | 线名（不变） |
| --- | --- | --- |
| `Agent.UnstableDisableProviders` | `Agent.UnstableDisableProvider` | `providers/disable` |
| `Agent.UnstableSetProviders` | `Agent.UnstableSetProvider` | `providers/set` |

相关请求/响应类型同步改名：`DisableProvidersRequest/Response` → `DisableProviderRequest/Response`、`SetProvidersRequest/Response` → `SetProviderRequest/Response`；常量 `MethodAgentUnstableDisableProviders` → `MethodAgentUnstableDisableProvider`、`MethodAgentUnstableSetProviders` → `MethodAgentUnstableSetProvider`。`UnstableListProviders`（list 语义为复数）保持不变。

### 3. 删除的方法

| 删除的符号 | 说明 / 替代 |
| --- | --- |
| `Agent.UnstableSetSessionModel` | 上游 v1 schema 已移除 `session/set_model`。相关类型 `SetSessionModelRequest/Response` 与常量 `MethodAgentUnstableSetSessionModel` 一并删除，当前无直接替代方法。 |

### 4. 新增的方法（下游需实现接口时注意）

下列方法为本次新增。直接实现 `acp.Agent` / `acp.Client` 接口（而非内嵌 `acp.BaseAgent` / `acp.BaseClient`）的下游代码，需要补齐对应方法才能通过编译：

- `Agent.DeleteSession`（线名 `session/delete`）
- `Agent.UnstableMessageMCP` / `Agent.UnstableMCPMessage`（线名同为 `mcp/message`，分别对应请求与通知两种 JSON-RPC 形态）
- `Client.UnstableConnectMCP`（线名 `mcp/connect`）
- `Client.UnstableDisconnectMCP`（线名 `mcp/disconnect`）
- `Client.UnstableMessageMCP` / `Client.UnstableMCPMessage`（线名同为 `mcp/message`）

> 关于 `mcp/message` 的请求/通知共用线名：入站消息按 JSON-RPC 是否带 `id` 分流到「请求处理表」或「通知处理表」两张独立路由表，二者不冲突；`MessageMCP*` 走请求，`MCPMessage`（通知）走 notification。

## 迁移建议

- **优先内嵌 Base 实现**：内嵌 `acp.BaseAgent` / `acp.BaseClient` 的下游，新增接口方法会自动获得「method not supported」默认实现，不会因接口新增方法而编译失败；只需把直接调用 / 重写的旧方法名按上表替换即可。
- **直接实现接口者**：按「删除 / 改名 / 新增」三类逐项调整：改名项替换符号、删除项移除调用、新增项补齐方法实现。
- **常量引用**：凡引用 `MethodAgent*` / `MethodClient*` 旧常量名处，按上表替换为新常量名；线名字符串值未变，已落库/已在线的协议报文不受影响。

## 影响范围

- 影响根包 `acp` 的公开接口、方法线名常量、请求/响应类型名，以及 `conn` 包的出入站方法签名。
- 不影响线协议层的方法字符串取值（如 `logout`、`session/close` 等保持原样），跨语言/跨实现互通不受本次改名影响。
- 不引入 deprecated 别名或兼容层；迁移以「按对照表替换符号」为唯一路径。

## 发布建议

- 作为下一个 `v0.x` minor 发布，并在 release note 中引用本文档，提示该版本含破坏性的接口改名与方法增删。
