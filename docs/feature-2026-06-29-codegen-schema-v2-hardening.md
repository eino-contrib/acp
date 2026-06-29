# Codegen 适配 Schema v2 的三处缺陷修复

## 背景

ACP SDK 的类型、接口、handler 注册表、方法元数据均由 `cmd/generate` 从
`cmd/generate/schema/` 下的 schema / meta 快照确定性生成。本次将快照升级到 ACP schema
**v2** 后，复查发现三处由 v2 新形态触发的生成缺陷。三者都会让运行时与 schema 语义不一致，
且现有测试无法察觉，因此一并修复，并补齐能真正卡住这些缺陷的回归测试。

## 缺陷一：`mcp/message` 的 request 形态未生成

### 问题

`mcp/message` 是 v2 中唯一一个**同时**具备 request 形态与 notification 形态的 wire method：
它的 request 类型同时出现在 agent 与 client 两侧的 request 集合中，并有对应的 response 类型；
它的 notification 类型则用于不带 JSON-RPC `id` 的场景。

此前生成器对“一个 wire method”只产出一个方法：一旦发现存在 notification 形态，就把整个方法
判定为 notification，丢弃 request / response。结果是带 `id` 的 `mcp/message` 请求进入分发后
找不到 handler，直接返回 method-not-found；扩展 handler 也兜不住（扩展方法仅限以 `_` 开头的
名字）。更隐蔽的是，handler 覆盖测试是基于同一份“notification-only”的方法元数据推导期望集合，
因此对该缺陷始终“通过”，无法报警。

### 处理

允许“一个 wire method 同时拥有 request 与 notification 两种形态”，分别生成两个方法、两类
handler，并在方法元数据中用“是否支持 request / 是否支持 notification”两个能力位表达，而不再用
单一的“是否 notification”布尔。运行时按 JSON-RPC `id` 是否存在来路由：有 `id` 走 request
handler，无 `id` 走 notification handler，两者注册在各自的表中、对应同一个 wire method。

命名上，request 保留规范名（与其它 request 方法风格一致），notification 形态加
`Notification` 后缀以示区分。该重命名只影响这一个 unstable 方法。

覆盖测试改为按“支持 request”和“支持 notification”两个能力位分别核对请求表与通知表，于是
`mcp/message` 会被同时要求出现在两张表中——测试从此能够真正发现“缺少 request handler”。

## 缺陷二：required 的字符串别名字段漏校验

### 问题

生成的 `Validate()` 对 required 的裸字符串字段会做非空校验，但当字段类型是**字符串别名**
（如会话 ID、MCP 连接 ID、鉴权方式 ID 等众多标识符类型）时，旧规则只认字面意义上的字符串类型，
于是这些 required 别名字段被整体跳过。缺失这些字段的请求会被解码成零值别名并继续进入 handler，
与“裸字符串 required 字段会被拦下”的既有行为不一致。该缺口波及面相当广，涵盖各类标识符字段；
本次 v2 又新增了若干受影响的协议入口（鉴权登录、MCP 连接 / 断开 / 消息等）。

### 处理

校验生成在判断字段是否需要非空检查时，除了裸字符串，还识别“指向基础字符串别名的引用”，对其
同样生成非空校验。规则对既有的同类字段（各种会话 / 工具调用 / 计划标识符）一并补齐，行为与裸
字符串字段对齐。

## 缺陷三：联合体回退解析会接受非法对象

### 问题

由结构化联合体（如命令输入规格）在解码时，若判别键缺失会进入“逐个变体试解析”的回退路径。
旧逻辑只要某个变体能被 JSON 解码成功就接受它，而 Go 的解码对“缺失 required 字段”并不报错，
因此空对象 `{}` 或任意无关对象会被错误地接受成第一个能解析的变体。这使得本应“从 any 收紧为
结构化联合体以保住 schema 语义”的目标并未真正达成。此外，联合体内联合成的变体包装类型此前没有
生成 `Validate()`，其 required 判别字段无人把关。

### 处理

回退试解析在某个变体解码成功后，若该变体带有校验能力，则先运行校验，仅在通过时才接受，否则
继续尝试下一个变体。同时为内联合成的变体包装类型补生成 `Validate()`，使其 required 判别字段
得到校验。对于没有 required 字段的变体（如响应结果 / 错误这类联合体），行为保持不变，仍可正常
round-trip。最终空对象与不符合任何变体的对象会被明确拒绝，而合法载荷照常解析。

## 验证

- `make gen` 确定性重生成，校验报告显示接口方法 0 缺失、0 签名不符。
- `go build ./...`、`go vet ./...` 全绿。
- `go test ./...`（含 `-race`）全绿，新增回归测试覆盖：`mcp/message` 请求经分发到达 handler
  并返回响应、notification 形态仍按通知分发；required 别名字段缺失时校验失败；结构化联合体拒绝
  空对象 / 非法对象、合法变体照常解析。
