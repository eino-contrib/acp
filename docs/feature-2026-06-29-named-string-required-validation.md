# 命名 string 类型 required 字段校验修复

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

生成器为普通 struct 类型产出 `Validate()`，在请求 / 响应 / 通知边界经 dispatch 的 `validatable` 接口被调用，用于在进入业务 handler 前拦截缺失 required 字段的非法报文。

此前 `Validate()` 只对 Go 类型恰好是字面 `string` 的 required 字段生成非空校验。但 schema 中大量 required 字段以 `$ref` / 单元素 `allOf` 指向命名 string 别名（如 `SessionId` / `TerminalId` / `AuthMethodId` / `ToolCallId` / `MCPConnectionId`），生成后字段类型是 `SessionID` / `TerminalID` 等命名类型而非字面 `string`，于是这些 required 字段被整体跳过校验。

结果是缺失这些 required 字段的请求能通过 decode 与 `Validate()`，以零值进入业务 handler，协议层的 required 校验在这些字段上完全失效。受影响面覆盖几乎所有会话相关请求与通知的 `sessionId`，以及 `terminalId` / `toolCallId` / `connectionId` / `methodId` / `optionId` / `modeId` 等典型标识字段，合计 60 余个类型上的数十个字段。

目标（只改生成器，不手写产物）：required 字段的非空校验不再以生成后的 Go 类型字符串为判据，而是解析字段 schema 的底层类型；底层最终落到 free-form string 的 required 字段，无论是否经由命名别名，都生成非空校验。

## 生成策略

判定 required 字段是否为 string-like 时，按以下规则解析底层类型：

- 跟随 `$ref` 与单元素 `allOf` ref 到目标定义；
- 跟随去掉 `null` 后仅剩单一变体的 `oneOf` / `anyOf`；
- 解析链路带环时安全终止；
- 底层为 free-form string（含 `["string","null"]` 可空形态）则判定为 string-like。

带 `enum` 或 `const` 约束的 schema 明确不视为 string-like：这类字段是取值受限的成员校验，不适用单纯的非空校验，沿用生成器既有“不校验枚举取值”的口径，避免把空字符串合法的枚举误判为缺失。

校验语义与既有字面 string 字段一致：required string 只校验 presence（空字符串即视为缺失并报 `<jsonField> is required`），不引入 `minLength` 等额外约束，不改变指针 / slice / map 等其它类型的既有校验形态。

## 影响范围

- 仅普通 struct 类型的 `Validate()` 输出受影响；带 parent shared fields 的 object union 仍走各自 `UnmarshalJSON` presence 校验与 union-level `Validate()` 路径，本次不改其形态与语义。
- 命名 string 别名 required 字段的请求 / 响应 / 通知在 dispatch 边界恢复拦截能力。
- 非 string-like 的 required 字段（枚举、整型、对象、数组等）校验形态不变。

## 测试要点

- 生成器层：从 unstable schema 生成后，断言 `AuthMethodAgent.id`、`AuthenticateRequest.methodId`、`TerminalOutputRequest.sessionId` / `terminalId`、`CreateTerminalResponse.terminalId`、`SessionNotification.sessionId`、`ToolCall.toolCallId`、`ConnectMCPResponse.connectionId` 等字段均产出非空校验。
- string-like 判定：字面 string、`$ref` / `allOf` ref 指向 string 别名、单变体 `oneOf`、可空 string 判为 string-like；枚举别名、const、整型别名、`nil` 判为非 string-like。
- 运行时层：缺失上述命名 string required 字段时 `Validate()` 报对应 `<jsonField> is required`；字段置位后校验通过，确认是 presence 校验而非过度拒绝。
