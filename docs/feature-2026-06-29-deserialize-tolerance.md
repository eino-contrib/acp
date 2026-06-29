# 反序列化容错扩展（x-deserialize-*）支持

## 背景与目标
> 生成的文件基于 schema.unstable.json 和 meta.unstable.json

v1 unstable schema 在大量字段上标注了两个反序列化容错扩展：

- `x-deserialize-default-on-error`：当某字段的 wire 取值类型不符时，不让整条报文解码失败，而是让该字段回退到默认值（有 schema `default` 用 schema 默认值，否则用 Go 零值）。
- `x-deserialize-skip-invalid-items`：对数组字段，逐个元素解码，丢弃解码失败的元素，保留可解码的元素，而不是让整个数组解码失败。

此前生成器没有建模这两个扩展，生成结果对这些字段一律走标准严格 `json.Unmarshal`。结果是 Go SDK 在面对类型不符的可选字段（如 `mcp/message` 的 `params`、几乎所有类型的 `_meta`、以及标注了 skip-invalid 的数组）时，比参考实现更严格：对方发来不符合形态的非关键字段时，本应被容忍并回退，却会直接整条解码失败。

目标（只改生成器，不手写产物）：让生成的反序列化逻辑遵循 schema 标注的容错语义，使本 SDK 在这些字段上与参考实现的解码宽容度对齐，同时不改变合法报文的解码结果。

## 生成策略

- 容错只作用于带显式扩展标注的字段，未标注字段保持既有严格语义。
- 合法报文走单次严格解码的快路径，不引入额外开销，解码结果与此前完全一致。
- 仅当严格解码失败时，才进入容错恢复：对标注 `default-on-error` 的字段，若其取值存在、非 null 且无法解码，则将其从报文中剔除，使其回退到默认；对标注 `skip-invalid-items` 的数组字段，只保留可解码的元素；随后再做一次严格解码。
- `default-on-error` 与 schema `default` 可作用于同一字段：剔除坏值后，既有的默认值应用逻辑会把该字段补成 schema 默认值（而非 Go 零值）。两类逻辑合并进同一个反序列化入口，不产生重复方法。
- 顶层非对象报文、语法非法的 JSON 仍然失败，不被容错吞掉；显式 `null` 视为合法输入而非需要恢复的错误。
- required 字段在当前 schema 中不带 `default-on-error`，因此容错不会把 required 字段悄悄丢成缺失。

## 影响范围

- 仅影响带 `x-deserialize-*` 标注字段的结构体反序列化；object union、enum、primitive 等其它生成形态不变。
- 合法报文解码行为不变；变化只发生在「字段取值类型不符」这一非合规输入路径上。
- 既有的 schema `default` 应用行为保持不变，并与容错回退正确叠加。

## 测试要点

- 生成器层：标注 `default-on-error` 的 object/map 字段产出按字段剔除的容错调用；标注 `skip-invalid-items` 的数组字段产出按元素保留的容错调用；容错辅助逻辑各只生成一份；同时带 `default` 与容错标注的类型只生成一个反序列化入口，且兼具默认值应用与容错。
- 运行时层：
  - `MessageMCPRequest.params` 取值为非对象时解码不失败、`params` 回退为 nil，兄弟字段正常解码；取值为合法对象时原样保留。
  - 任意类型的 `_meta` 取值类型不符时被容忍并回退为 nil。
  - 标注 skip-invalid 的数组（如 `Plan.entries`）含坏元素时只丢弃坏元素、保留好元素；取值非数组时整体回退；全部合法时不变。
  - 同时带 schema `default` 与 `default-on-error` 的字段（如 `AuthEnvVar.secret`，默认 true）取值类型不符时回退到 schema 默认值而非 Go 零值；缺失时仍套用默认；显式合法值优先。
  - 顶层非对象报文与语法非法 JSON 仍然返回错误。
