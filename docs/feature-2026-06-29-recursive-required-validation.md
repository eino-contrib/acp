# required 嵌套字段递归校验修复

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

生成器为普通 struct 类型产出 `Validate()`。入站请求与通知的 params 在进入业务 handler 前，经 dispatch 的 `validatable` 接口被调用，用于拦截非法报文。本次只扩展 `Validate()` 覆盖的校验深度，不改变各边界是否调用它的既有行为。

此前普通 struct 的 `Validate()` 只对 required 字段做「本层」校验：required string 非空、required pointer / slice 非 nil。它不会对 required 的**值类型 struct** 字段递归调用其 `Validate()`，也不会遍历 required **slice 的元素**做校验。

结果是嵌套的 required 缺失能穿过协议层。典型例子：

- `RequestPermissionRequest.toolCall` 是 required，类型为值类型 `ToolCallUpdate`，而 `ToolCallUpdate.toolCallId` 是 required。但 `RequestPermissionRequest.Validate()` 只检查 `options` 与 `sessionId`，于是 `{"sessionId":"s1","options":[],"toolCall":{}}` 能通过外层校验进入 handler，尽管 `toolCall.toolCallId` 缺失。
- `Plan.entries` 是 required slice，元素 `PlanEntry.content` 是 required。但 `Plan.Validate()` 只检查 `entries != nil`，不会校验每个 `PlanEntry`，于是含空 `PlanEntry{}` 的数组能通过外层校验。

值得注意的是，带 parent shared fields 的 object union 路径**已经**会递归（通过运行时接口断言调用嵌套 payload 的 `Validate()`），普通 struct 路径却没有。本次修复把同一递归语义对齐到普通 struct，消除这一不一致。

目标（只改生成器，不手写产物）：普通 struct 的 `Validate()` 在完成本层 presence 校验后，对可能携带 `Validate()` 的 required 嵌套目标递归校验，使协议层 required 语义贯穿到嵌套结构与数组元素。

## 生成策略

- required **值类型 struct / 单变体 union** 字段：对字段取地址做运行时接口断言 `any(&v.Field).(interface{ Validate() error })`，命中则递归调用。值类型 struct 永远非 nil，因此不做 presence 预检，其 required 性由递归本身传递性地强制。
- required **pointer** 字段：保留既有非 nil presence 校验；非 nil 后，若指针目标可校验则递归。当前 schema 中 required pointer 仅为可空基础类型（非可校验），因此这一分支当前不产出递归调用，输出不变。
- required **slice** 字段：保留既有非 nil presence 校验；随后 `for i := range` 遍历，对元素取地址 `&v.Field[i]` 做同样的接口断言递归。元素地址形式可命中指针接收者的 `Validate()`。
- 递归仅在字段 schema 解析到**命名复合类型**（struct / union）时才产出，借由 `schemaValidatable` 判定：跟随 `$ref`、单元素 `allOf` ref、去 `null` 后单变体 `oneOf` / `anyOf`、ref 别名，解析链路带环时安全终止。基础类型、string、枚举、map、free-form object、以及生成为 `json.RawMessage` 的多变体 union 均不视为可校验，不产出递归调用，避免对非复合字段产生死代码。
- 递归一律走运行时接口断言而非直接方法调用：并非所有类型都产出 `Validate()`（仅有校验项或请求 / 响应类型才有），断言形式对未实现 `Validate()` 的目标是安全 no-op，沿用 object union 路径既有写法。
- 嵌套错误带字段 / 索引上下文回传：值字段包成 `<Field>: <err>`，slice 元素包成 `<Field>[<i>]: <err>`，便于定位深层非法字段。

## 影响范围

- 仅普通 struct 类型的 `Validate()` 输出受影响，且为纯增量：既有本层 presence 校验逐字保留，仅在其后追加对 required 嵌套目标的递归。
- 命名 string required 校验（见同期 named-string 修复）与各类型本层 presence 形态不变。
- object union 路径本就递归，不受影响。
- 嵌套 required 缺失的入站请求 / 通知 params 现可在 dispatch 边界被拦截；response 类型的 `Validate()` 同步加深，但仍只在调用方手动调用时生效。
- 当前 schema 中不存在「经由 required 字段构成的类型引用环」，因此递归不会自我循环；该口径与 object union 既有递归一致。

## 测试要点

- 生成器层：从 unstable schema 生成后，断言 `RequestPermissionRequest.Validate()` 含对 `&v.ToolCall` 的接口断言递归、并对 `v.Options` 元素逐个递归；`Plan.Validate()` 对 `v.Entries` 元素逐个递归；`PlanEntry.Validate()` 这类仅含 required string / 枚举的类型不产出任何嵌套接口断言，仅保留非空校验（确认递归被正确门控、不产生死代码）。
- 运行时层：
  - `RequestPermissionRequest{ToolCall: ToolCallUpdate{}}`（options 非 nil）因嵌套 `toolCallId` 缺失而被拒，错误含 `ToolCall:` 上下文；`ToolCall.ToolCallID` 置位后通过。
  - `Plan{Entries: []PlanEntry{{...有效...}, {}}}` 因第二个元素 `content` 缺失被拒，错误含 `Entries[1]` 索引上下文；全部元素有效时通过。
  - `RequestPermissionRequest.Options` 中含 `name` / `optionId` 缺失的元素时被拒，错误含 `Options[1]` 索引上下文（证明 slice 递归在 presence 通过后仍执行）。
  - 仅含 required string、无可校验嵌套的请求（如 `WriteTextFileRequest`）完整置位后仍正常通过，确认递归对无 `Validate()` 的嵌套类型不 panic。
