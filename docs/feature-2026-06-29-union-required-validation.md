# 判别式 union 的 required 校验补全

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

同期的「required 嵌套字段递归校验修复」(`feature-2026-06-29-recursive-required-validation.md`) 让普通 struct 的 `Validate()` 对 required 的**值类型 union** 字段产出递归：取字段地址做运行时接口断言 `any(&v.Field).(interface{ Validate() error })`，命中则递归调用。该断言对未实现 `Validate()` 的目标是安全 no-op。

问题在于：判别式 union（`oneOf` / `anyOf` + discriminator，且不携带 parent shared fields）走的是 `generateDiscriminatedUnion` 的 `variantInfos` 生成路径，该路径只产出 `MarshalJSON` / `UnmarshalJSON` / accessor / constructor，**从不产出 `Validate()`**。于是上面那条递归断言对这类 union 永远 `ok == false`，整段递归退化为静默 no-op。

结果是 required 的值类型 union 字段缺失时能直接穿过 `Validate()`：

- `SessionNotification.Update` 是 required、值类型 `SessionUpdate`（判别式 union）。`SessionNotification.Validate()` 只检查 `SessionID`，再对 `&v.Update` 做接口断言；但 `SessionUpdate` 无 `Validate()`，断言失败，于是 `SessionNotification{SessionID:"s1"}`（`Update` 为零值、无任何 variant）能通过校验。
- `RequestPermissionResponse.Outcome` 是 required、值类型 `RequestPermissionOutcome`（判别式 union）。`RequestPermissionResponse{}`（`Outcome` 零值）同样能通过校验。

这不仅是构造态问题：当入站 JSON 完全缺失 `update` 字段时，`encoding/json` 根本不会触发 `SessionUpdate.UnmarshalJSON`，字段停在零值，随后 `Validate()` 也不拦截，缺失因此穿透到 dispatch 边界之后的 handler。

值得注意的是，带 parent shared fields 的 object union 路径 (`generateObjectUnionWithParent` → `emitUnionValidate`) **已经**会产出「恰好一个 variant + 递归选中 variant」的 `Validate()`。本次修复把同一语义对齐到 `variantInfos` 路径的判别式 union，消除这一不一致。

目标（只改生成器，不手写产物）：判别式 union 及其每个 variant wrapper 产出 `Validate()`，使 required 值类型 union 字段的缺失能在协议层被拦截，同时让已选 variant 的 payload 校验贯穿下去。

## 生成策略

- **union 层 `Validate()`**：统计已置位的 variant 指针数 `set`；`set != 1` 时返回 `"<Union>: exactly one variant must be set, got N"`（覆盖「一个都没设」的零值缺失，以及「设了多个」的非法状态）；随后命中唯一非 nil variant，递归调用其 `Validate()`。这与 object union 路径的 `emitUnionValidate` 写法一致。
- **variant wrapper 层 `Validate()`**：
  - variant 自身的 inline required 字段（去掉 discriminator 后）复用 `buildValidateChecks` 产出本层 presence 校验，与普通 struct 同一套逻辑（string 非空 / pointer / slice 非 nil / 值类型嵌套递归）。当前 schema 中判别式 union 的 variant 均无 inline required 字段，这一分支不产出实际校验，但保持与普通 struct 行为一致的防御性实现。
  - variant 内嵌引用 payload 类型（如 `SessionUpdateToolCall` 内嵌 `ToolCall`）时，对内嵌字段取地址做运行时接口断言递归，命中则调用其 `Validate()`；payload 无 `Validate()` 时为安全 no-op，沿用既有写法。
- 生成器层把 `Validate()` body 的 per-field check 发射逻辑从 `generateValidateFunc` 抽出为 `writeValidateChecks`（接收者固定命名为 `v`），供普通 struct 与 union variant wrapper 共用，避免重复实现校验分支。
- 一旦判别式 union 产出 `Validate()`，普通 struct 路径里早已发射、此前为 no-op 的 `any(&v.Field).(...)` 递归断言即开始生效——holder 侧的生成代码无需改动。

## 影响范围

- 仅判别式 union（`variantInfos` 路径）及其 variant wrapper 的生成输出受影响：新增 union 层与 variant 层 `Validate()`。`MarshalJSON` / `UnmarshalJSON` / accessor / constructor 形态逐字不变。
- object union（parent shared fields）路径本就产出 `Validate()`，不受影响。
- 行为变化（**潜在破坏性**）：此前能通过 `Validate()` 的「缺失 required union 字段」「union 设置了多个 variant」「已选 variant 的 payload 缺失 required 子字段」现在会被拒。持有 required 值类型 union 字段的请求 / 通知 params 在 dispatch 边界即被拦截；response 类型的 `Validate()` 同步加深，但仍只在调用方手动调用时生效。
- 线协议层与序列化行为不变：`MarshalJSON` / `UnmarshalJSON` 取值与字段名未改，已落库 / 在线报文的解析不受影响；仅显式 `Validate()` 调用的判定结果变严格。
- 当前 schema 中判别式 union 的 variant 不存在经 required 字段构成的引用环，递归不会自我循环；该口径与 object union 既有递归一致。

## 测试要点

- 生成器层（`cmd/generate/gen_union_test.go`）：从 unstable schema 生成后，断言 `SessionUpdate` / `AuthMethod` 产出 `func (s *SessionUpdate) Validate()` / `func (a *AuthMethod) Validate()` 且含 `"exactly one variant must be set"`；variant wrapper（如 `SessionUpdateToolCall` / `AuthMethodEnvVarVariant`）各自产出 `Validate()`；同时确认 constructor 签名（如 `NewSessionUpdateToolCall(v ToolCall) SessionUpdate`）保持不变。
- 运行时层（`validate_recursive_test.go`）：
  - `SessionNotification{SessionID:"s1"}`（`Update` 零值）被拒，错误含 `Update:` 与 `exactly one variant must be set` 上下文。
  - 入站 JSON `{"sessionId":"s1"}`（完全缺 `update`）反序列化后 `Validate()` 仍被拒，验证缺失字段不触发 `UnmarshalJSON` 的穿透路径已被堵住。
  - `RequestPermissionResponse{}`（`Outcome` 零值）被拒。
  - 设置了合法 variant 的 `SessionNotification`（如 `NewSessionUpdatePlanRemoved(PlanRemoved{ID:"p1"})`）通过校验。
  - 设置了 variant 但其 payload 缺失 required 子字段（如 `NewSessionUpdateToolCall(ToolCall{Title:"t"})` 缺 `toolCallId`）被拒，错误含 `toolCallId is required`，验证 union 递归进入已选 variant 的 payload 校验。
