# 生成器 import 收集顺序修复

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

生成器把所有类型写入一个 `strings.Builder`，最终 `go/format` 格式化后输出。生成的代码是否需要 `encoding/json` / `fmt`，由 `g.needJSON` / `g.needFmt` 两个标志位决定，并据此写出 import 块。

此前 import 块由一段**前置预扫描**决定：在写 import 前遍历 `defs`，只对 `TypeDiscriminatedUnion`（置 `needJSON`+`needFmt`）和 `structNeedsJSON` 命中的 struct（置 `needJSON`）置位，然后立刻写出 import 块，**之后**才生成各类型主体。

问题在于：大量 import 需求是在主体生成阶段才被置位的，发生在 import 块写出**之后**，预扫描覆盖不到。包括：

- `Validate()` 生成（用 `fmt.Errorf`，置 `needFmt`）；
- 带 schema `default` / `x-deserialize-*` 的自定义 `UnmarshalJSON`（用 `json` / `fmt`）；
- ext payload 类型别名（`type X = json.RawMessage`，置 `needJSON`）；
- primitive / simple union 等其它生成形态。

`go/format` 只做语法层格式化，**不会**增删 import。因此一旦出现「主体引用了 `json` / `fmt`、但预扫描没置位」的组合，产物会缺失 import 而无法编译（`undefined: fmt` / `imported and not used`）。

当前 bundled schema 因为含 discriminated union，预扫描把 `needJSON`+`needFmt` 都置真，恰好掩盖了这个缺陷——产物始终正确。但这是潜伏缺陷：一个「只有普通 struct + required 字段 / default，无任何 union、无 `json.RawMessage` 字段」的 schema 就会触发缺 import。

目标（只改生成器，不改既有产物）：import 块改为依据「主体生成期间实际记录的 import 需求」计算，而非独立的前置预扫描，使 import 与 emitter 不再脱钩。

## 生成策略

- 调整生成顺序：先把全部类型主体写入 `g.buf`，各 emitter 在产出代码时自行记录其 import 需求（`needJSON` / `needFmt` / `needHasKey`）；最后再组装 header + import 块并 prepend 到主体之前。
- 各 emitter 自声明 import 需求：原先依赖预扫描的 discriminated union 路径改为在 `generateDiscriminatedUnion` 入口自行置位 `needJSON`+`needFmt`（该路径必然产出 `json.Marshal/Unmarshal` 与 `fmt.Errorf`），与旧预扫描对每个 `TypeDiscriminatedUnion` 的置位逐字等价。
- 移除已无调用方的 `structNeedsJSON` 与其依赖的 `resolveGoType`：struct 字段的 `json.RawMessage` import 需求已由 `resolveFieldType` 在字段生成时即时置位 `needJSON`，预扫描式判定不再需要。
- 行为保持：对当前 schema，新顺序产出的 `types_gen.go` 与既有产物逐字节一致；本次仅修复潜伏的 import 计算缺陷，不改变任何生成内容。

## 影响范围

- 仅影响生成器内部 import 计算时机；对当前 schema 的产物零变化（逐字节一致）。
- 对未来 / 外部 schema：当某类型仅经由 `Validate()`、自定义 `UnmarshalJSON`、ext payload 或 primitive/simple union 触发 `json` / `fmt` 使用，且无 union 提前置位时，import 块不再缺失，产物可正常编译。

## 测试要点

- 生成器层：构造最小 schema（单个普通 struct，含一个 required string 触发 `Validate()`→`fmt`、一个带 `default` 字段触发自定义 `UnmarshalJSON`→`json`+`fmt`，无 union、无 `json.RawMessage` 字段），生成后用 `go/parser` 解析 AST，断言凡引用 `fmt.` / `json.` 选择器的产物均带对应 import。该用例在旧预扫描下会因 import 块为空而失败，在新顺序下通过。
- 回归保证：对 bundled schema 重新生成，确认 `types_gen.go` 等产物与提交版本逐字节一致。
