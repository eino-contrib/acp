# Object Union（oneOf / anyOf）生成方案

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

`SetSessionConfigOptionRequest` 在 unstable schema 新增 boolean 与 value id 两个分支，但生成器只产出 boolean 分支。两个根因：

- **变体识别漏判**：value id 分支无 discriminator const，分支顶层也非 `$ref` / `allOf-ref`（ref 在分支内部 `properties.value.allOf`，指向 scalar alias `SessionConfigValueId`）。现有识别只认“有 const”或“顶层是 ref”，于是跳过该分支。
- **parent 共享字段无处承载**：该 union parent 层带 `sessionId` / `configId` 及 parent `required`，而对标的 `SessionUpdate` parent 层无 `properties` / `required`。共享字段承载是本方案**新增能力**，SessionUpdate 在这点上不是先例。

第二个根因是通用缺陷，所有带 parent 共享字段的 object union 都丢 parent 字段、漏校验 parent required。受影响产物：

| Union | parent properties | parent required | 当前缺陷 |
|---|---|---|---|
| `SetSessionConfigOptionRequest` | `_meta` / `sessionId` / `configId` | `sessionId` / `configId` | value id 分支整体丢失；boolean 分支丢 parent 字段 |
| `CreateElicitationRequest` | `_meta` / `message` | `message` | variant 丢 `_meta` / `message` |
| `CreateElicitationResponse` | `_meta` | 无 | variant 丢 `_meta`，optional parent 字段无法 round-trip |
| `ElicitationFormMode` | `requestedSchema` | `requestedSchema` | variant 丢 `requestedSchema` |
| `ElicitationUrlMode` | `elicitationId` / `url` | `elicitationId` / `url` | union 未生成完整 parent 字段 |
| `SessionConfigOption` | `_meta` / `id` / `name` / `category` / `description` | `id` / `name` | variant 丢全部 parent 字段 |

目标（只改生成器，不手写产物）：对象类 `oneOf` / `anyOf` 的变体形态统一以 `SessionUpdate` 的现有生成方式为基准：union wrapper 持有每分支一个指针字段，每个分支都有独立 variant wrapper；ref / allOf-ref 分支由 variant wrapper 匿名嵌入被引用 payload，inline / ref-in-property 分支由 variant wrapper 直接表达字段。识别覆盖“分支无 const、ref 在分支内部字段”形态；支持具默认语义的 discriminator 分支（如 `value_id`）；上表所有 union 每个 variant 补齐 parent 字段。primitive union、primitive array union、open enum 保持现有公开形态。

本方案的代码输出变化范围仅限上表 6 个带 parent shared fields 的 object union。`SessionUpdate`、`AuthMethod` 等无 parent shared fields 的既有 object union 已经是上述 SessionUpdate-style wrapper 形态，不纳入本次行为改造，继续作为形态回归对象：不新增 exact-one 校验、不新增 union-level `Validate()`、不改构造函数签名、不改 `types_gen.go` 输出形态。`AuthMethod` 仍只用测试确认未知 discriminator 不会因为 `SetSessionConfigOptionRequest` 的 allowlist fallback 而被放宽。

不做：生成器目录重构、新运行时依赖、request / response / notification 后缀策略调整。

> 当前 schema 不存在“array 变体 + parent 共享字段”组合：带 array 变体的 union（`SessionConfigSelectOptions`、`ElicitationContentValue`）均无 parent 共享字段，反之亦然。生成器遇到该组合直接 fail-fast，避免生成不完整类型。

## 生成策略

### 分支识别与命名

- 归类总则：object union 中只要存在任一带 discriminator const 的分支（含显式 `discriminator` 或由共享 const 字段推导），整个 union 按 discriminator union 处理，无 const 的分支即默认分支候选；`SetSessionConfigOptionRequest` 虽是 `anyOf` 且 boolean 分支才带 `type` const，仍按此归类为 discriminator union。所有分支均无 const 的 object `anyOf` 才按非 discriminator `anyOf` 走结构匹配。
- variant 类型按 SessionUpdate-style wrapper 生成：inline object → 独立 variant wrapper；ref / allOf-ref object → 独立 variant wrapper 匿名嵌入被引用 payload；ref / allOf-ref object + parent 共享字段 → 同一个 variant wrapper 同时承载 parent 字段并匿名嵌入 payload；ref 在内部字段 → 独立 variant wrapper，按字段 schema 表达 payload；仅含 discriminator const、无其他 payload 的空分支 → 按 inline object 处理，生成仅承载 parent 共享字段的 variant wrapper（如 `CreateElicitationResponse` 的 decline / cancel）。无 parent shared fields 的既有 ref / allOf-ref union 已符合该形态，必须保持字节级不变。
- 识别必须覆盖“分支无 const、顶层非 ref、ref 在内部字段”的分支，不得跳过。
- 命名：discriminator union variant 名优先取 const，无 const 的默认分支取 `title`；非 discriminator `anyOf` 保持现有 ref / allOf-ref 推导的成员 / accessor / constructor 命名，仅 inline 分支或无法从 ref 稳定命名时取 `title`。`title` → Go 名复用现有 title-case 规范化（`value_id` → `ValueID`），allowlist 默认 variant 名须与该规范化结果一致。
- `title` 仅用于命名；`title` / `description` 等自然语言不参与默认分支或任何生成判定。
- 需用 `title` 命名的无 const 分支若 `title` 缺失 / 为空 / 规范化后为空 / 与其他分支冲突，生成期报错（含 union 名、分支序号、原因），不静默跳过。

### 默认分支与 unknown fallback

- 默认分支仅存在于 discriminator union，用于 “discriminator 缺失时” 的 fallback。判定信号：union 有显式或推导 discriminator，且内部唯一一个分支无 const。无法唯一确定时生成期报错（含 union 名、候选分支序号、原因），不隐式任选、不降级为顺序默认。
- 非 discriminator `anyOf` 无默认分支语义，按结构匹配反序列化，不参与“多默认分支”判断。
- “discriminator 未知时” fallback 是独立能力，不由默认分支自动推导。启用信号为生成器内部 allowlist（key=definition 名，value=校验后默认 variant 名），当前唯一条目 `SetSessionConfigOptionRequest -> ValueID`（源自分支 `title: "value_id"`）。allowlist 命中但默认分支不存在 / 不唯一 / 命名不匹配时生成期报错，避免悄悄放宽其他 union 的语义。

### 共享字段

parent 有共享字段时，每个 variant 都要能独立表达完整请求 / 响应参数。parent shared fields 指 parent `properties` 全量字段（不限 required）；optional 字段（如 `_meta`、`category`、`description`）也必须 round-trip。

- inline object：parent properties 全量合并进 variant struct；parent required 只影响校验。
- ref object：外层 wrapper 承载 parent properties，payload 用被引用类型。最终 wire JSON 必须是 parent fields、payload fields、discriminator const（如有）扁平合并后的单层对象；不能依赖 `encoding/json` 对匿名嵌入 struct 的默认行为作为最终序列化语义。
- ref 在内部字段：独立 variant 类型，按分支 properties 表达字段并合并 parent properties。
- parent 字段与 payload 字段 JSON 名 / Go 名冲突时，仅 schema 语义完全一致才允许复用，否则报错（含 definition、variant index、冲突字段、原因）。语义一致的判定口径：解析单层 `$ref` / `allOf` ref 后，比较字段 schema 的 wire/validation 相关内容（`type`、`const`、`enum`、`oneOf` / `anyOf` / `allOf`、`items`、`properties`、`required`、`additionalProperties`、`default`）及该字段在所属对象中的 required 状态；忽略 `title` / `description` / `x-*` 等说明性元数据。required 状态不同也视为冲突。
- array 变体（含 array-of-ref）+ parent 共享字段组合直接 fail-fast（见背景）。
- 无法安全承载 parent shared fields 时报错（含 definition、variant index、字段 / 原因），不依赖测试偶然暴露。

有 parent shared fields 的 variant 统一生成自定义 `MarshalJSON`：先 marshal payload 为 `map[string]json.RawMessage`，再写入 parent shared fields，最后写入 discriminator const；冲突字段在生成期已按上述规则处理，不在运行期靠覆盖顺序消解。payload 本身是 union 且实现 `MarshalJSON` 时也按同样的 map merge 处理，确保 `CreateElicitationRequest` 这类 “parent wrapper + nested union payload” 不丢 `_meta` / `message` / `mode` / payload 字段。`UnmarshalJSON` 同样基于原始 JSON key set 做分支识别与 required presence，不依赖嵌入 struct 的默认解码副作用。

例：`SetSessionConfigOptionRequest` boolean 与 value id 分支均含 `_meta` / `sessionId` / `configId`；`CreateElicitationRequest` 各 variant 含 `_meta` / `message`；`CreateElicitationResponse` 各 variant 含 `_meta`；`SessionConfigOption` 各 variant 含 `_meta` / `id` / `name` / `category` / `description`。

### 序列化 / 反序列化

- `MarshalJSON`：对本次受影响 union，先校验恰好一个 variant 被设置，未设置或多设置均报错；有 const 的分支写出区分字段。`MarshalJSON` 与 `Validate()` 对“多 variant 被设置”必须一致失败，不静默取第一个非空分支。`SessionUpdate` 等无 parent shared fields 的既有 object union 保持原生成形态，不在本次补 exact-one。
- discriminator union 反序列化优先级：① 命中 const 的分支优先；② discriminator 缺失只尝试唯一默认分支；③ discriminator 未知默认报错，仅 allowlist 启用的 union 在默认分支 decode 且 required / presence 校验通过后 fallback；④ 多默认分支生成期报错。
- 非 discriminator `anyOf` 反序列化：用 required 字段 / 数组元素 required 等结构信号判断候选；唯一匹配则选中；无匹配报 `data does not match any variant`；多匹配报 ambiguous union 错误，不顺序静默选择。当前 schema 中的非 discriminator object `anyOf`（`ElicitationFormMode` / `ElicitationUrlMode`）各分支 required 字段互不相交（`ElicitationSessionScope` required `sessionId`、`ElicitationRequestScope` required `requestId`），可由 required presence 唯一区分；ambiguous 多匹配是兜底安全行为，当前 schema 无天然触发样本。
- 负向边界须失败不得落错分支：缺失 / 未知类型字段但 payload 不符默认分支、缺失默认分支 required、缺失 parent required、非 discriminator `anyOf` 多匹配。

### required 校验入口

当前 union 不生成 `Validate()`，dispatch 经可选 `validatable` 接口调用、未实现即静默跳过，导致 parent required 在请求边界完全不校验。本次采用双入口，并区分 wire presence 与手动构造：

- `UnmarshalJSON`：分支识别后做 required presence 校验（选中分支 required + parent required），以原始 JSON 字段是否存在为准，不看 Go 零值，避免 required boolean / number / string 缺失被误判为合法零值。required 且 schema 不允许 `null` 的字段，JSON key 存在但值为 `null` 也必须失败；本次受影响 union 若未来出现 required nullable 字段，生成期先 fail-fast，避免手动构造路径无法区分“缺失”和“显式 null”。
- union-level `Validate()`：覆盖手动构造对象后的校验，接入 dispatch 的 `validatable` 链路；至少校验“恰好一个 variant 被设置”及选中 variant 的 parent + 分支 required。以 pointer 是否为 nil 为准。选中 variant 内部的 ref / allOf-ref payload、嵌套 union payload 或其他实现 `Validate()` 的字段也必须递归调用 `Validate()`，确保 parent required 与 payload required 都在 dispatch 边界生效。
- 为区分“未设置”与合法零值，variant 中 required 且零值可能合法的 scalar 字段用 pointer presence 形态：`boolean` → `*bool`，`string` → `*string`，`integer` / `number` → `*int64` / `*float64`，scalar alias / allOf-ref scalar alias → `*AliasType`。
- required string 只校验 presence 不校验非空：除非 schema 声明 `minLength` 等约束，否则显式空字符串合法。
- discriminator const 字段由分支识别与 `MarshalJSON` 写出，不要求调用方设置，也不作为手动构造的 required data 字段。

### 构造函数与访问器

- constructor / accessor 名称尽量保持现有公开命名；非 discriminator ref / allOf-ref union 保持 ref 类型名风格，避免因 `title` 无必要 rename。
- 无 parent shared fields 的既有 ref / allOf-ref variant 构造函数保持现有 payload-only 签名与产物形态，例如 `NewSessionUpdateToolCall(v ToolCall)`、`NewAuthMethodEnvVarVariant(v AuthMethodEnvVar)`。
- 有 parent shared fields 的 variant 构造函数改为接收完整 wrapper（如 `NewSessionConfigOptionSelect(v SessionConfigOptionSelect)`），由 wrapper 承载 parent 字段 + payload，并补写 discriminator const（如有）。这是受影响 union 的预期 public API 变化，避免生成缺 parent required 的无效对象。inline / ref-in-property variant 同样接收完整 wrapper 并补写 const。

## 产物形态变化

- 命名 / 构造函数名尽量稳定；有 parent shared fields 的 variant 构造函数签名从 payload-only 改为完整 wrapper。
- 补齐分支与共享字段会改变部分 variant 字段集合（如 `SetSessionConfigOptionRequestBoolean` 补 `_meta` / `sessionId` / `configId`）；required scalar 从值类型改为 pointer presence（如 boolean 分支 `value`：`bool` → `*bool`，区分 `false` 与缺失）。不引入运行时依赖。
- primitive union / primitive array union / open enum 仅回归保护，不改公开形态。

## 测试与验收

验收标准：

- `SetSessionConfigOptionRequest` 能表达 boolean 与 value id 两分支，各 variant 含完整 parent 共享字段。
- 上表全部 union 每个 variant 含完整 parent 字段；带 parent required 的 union 缺失 parent required 时校验失败；仅 optional parent 字段的 union 能 round-trip。
- 受影响的对象 / 引用 / ref-in-property 类 union 变体形态与 `SessionUpdate` 的 wrapper + 指针字段模式一致，保留非 discriminator ref union 既有命名风格；`SessionUpdate` 自身不改。
- 受影响 union 具备 `UnmarshalJSON` presence 校验、union-level `Validate()`、`MarshalJSON` exact-one 校验；缺失 parent / 分支 / payload required 时请求边界与 dispatch 校验均失败。
- `SessionUpdate`、primitive union、primitive array union、open enum 形态不回退；生成器测试与全仓库测试通过。

测试覆盖：

- boolean 与 value id 两分支均含完整 parent properties；`_meta` 在两分支 round-trip。
- 缺失类型字段的 value id payload 正确反序列化；未知类型字段 + `value` 为字符串落 value id 分支；`value` 非字符串（无论类型字段缺失或未知）均失败。
- 缺失 `value` / `sessionId` / `configId` 校验失败；boolean `value=false`（合法显式零值通过）与缺失 `value`（失败）可区分；required 且非 nullable 字段传入 `null` 失败。
- required string presence：缺失失败，显式空字符串在无额外约束下通过。
- 其余 union 补齐并校验 parent 字段：`CreateElicitationRequest` 校验 `message` 且 `_meta` round-trip；`CreateElicitationResponse` `_meta` round-trip；`ElicitationFormMode` 校验 `requestedSchema`；`ElicitationUrlMode` 校验 `elicitationId` / `url`；`SessionConfigOption` 校验 `id` / `name` 且 `_meta` / `category` / `description` round-trip；缺失任一 parent required 失败。
- 非 discriminator `anyOf` 唯一匹配：`ElicitationFormMode` / `ElicitationUrlMode` 各分支 required 字段互不相交（`sessionId` vs `requestId`），按 required presence 唯一选中正确分支；分支 required 缺失时报 `data does not match any variant`。
- 受影响 union 同时设置多个 variant 指针时 `Validate()` 与 `MarshalJSON` 均失败。
- 有默认分支但未启用 unknown fallback 的 union（如 `AuthMethod`），未知 discriminator 仍失败。
- parent wrapper + nested union payload 的序列化能同时保留 parent fields 与 payload fields，如 `CreateElicitationRequest` 的 `_meta` / `message` / `mode` / scope 字段 round-trip；payload required 缺失会经递归 `Validate()` 失败。
- 生成期错误路径有覆盖：无 const 分支缺 `title` / `title` 冲突、无法承载 parent shared fields、parent 与 payload 字段冲突（含 required 状态不同）、required nullable 字段进入受影响 union、allowlist 命中但默认分支不唯一 / 不存在。
- boolean 分支序列化输出正确类型字段；有 parent shared fields 的 variant 构造函数接收完整 wrapper 并补 const，无 parent shared fields 的现有构造函数形态不回退。

## 实施计划

1. [已完成] 调整 object union 分类与变体识别，覆盖 `oneOf` / `anyOf`、“分支无 const、ref 在内部字段”形态，区分 discriminator union 与非 discriminator `anyOf`；同时收窄行为变更范围，只改上表 6 个 parent-shared union，`SessionUpdate` / `AuthMethod` 等无 parent shared fields 的既有 union 仅做回归保护。
2. [已完成] 调整 variant / 字段命名：discriminator 默认分支用 `title`；非 discriminator ref / allOf-ref union 保持 ref 类型名风格；补 `title` 缺失 / 为空 / 冲突的生成期报错。
3. [已完成] 合并 parent properties 到 inline / ref object / ref-in-property variant，覆盖全部带 parent 共享字段的 union，确保 required 与 optional parent 字段均可表达；补 parent/payload 字段冲突的可执行判定；对“array 变体 + parent 共享字段”和“required nullable 字段进入受影响 union”组合 fail-fast。
4. [已完成] 调整反序列化：discriminator union 覆盖缺失类型、allowlist unknown fallback、非默认分支；非 discriminator `anyOf` 覆盖唯一匹配 / 无匹配 / 多匹配 ambiguous；其他 union 未知 discriminator 仍报错；补 allowlist 命中但默认分支不唯一 / 不存在的生成期报错。
5. [已完成] 补齐 required 校验入口：`UnmarshalJSON` 做 required presence + 非 nullable 禁止 `null` 校验，union-level `Validate()` 接入 dispatch；required scalar / scalar alias 生成 pointer presence，覆盖分支 + parent required；required string 校验 presence 而非非空；递归调用选中 variant 内 ref / nested union payload 的 `Validate()`。
6. [已完成] 调整 `MarshalJSON` 与构造函数：受影响 union 的 `MarshalJSON` 对未设置或多设置均失败；有 parent shared fields 的 variant 通过 map merge 扁平输出 parent + payload + const，避免嵌入 payload 自定义 `MarshalJSON` 时丢字段；有 parent shared fields 的 variant 构造函数接收完整 wrapper 并补 const。
7. [已完成] 按「测试与验收」一节的覆盖清单补齐生成器测试（正向 fallback、负向边界、ambiguous anyOf、`_meta` round-trip、required presence、生成期错误路径、构造函数形态、形态回归等），以该清单为准，不在本处重复维护用例列表，避免两处漂移。
8. [已完成] 重新生成代码并运行测试，对照验收标准确认全部通过；对 `types_gen.go` 做生成前后 diff，确认 `SessionUpdate`、primitive union、primitive array union、open enum 等未受影响类型字节级不变，受影响 union 的变化均在预期范围内。

## 风险

- 命名、字段集合、required scalar pointer presence、部分构造函数签名变化可能影响调用方。
- fallback 放宽仅限 allowlist 启用的 union，其他 union 未知 discriminator 仍报错。
- 非 discriminator `anyOf` 多匹配报 ambiguous 错误而非顺序选择；该路径为兜底安全行为，当前 schema 内的非 discriminator object `anyOf` 各分支 required 互不相交，不会触发歧义。
