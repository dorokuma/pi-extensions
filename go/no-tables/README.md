# no-tables (Go 移植版)

本扩展是原 TS 版 `no-tables.ts`（Pi 扩展分发中枢权威源）向 PiG Go 扩展体系的移植实现。Rule A–G 与表格转换在 `\n` / `\r\n` / `\r` 三种行终止符下均已与 TS **逐字节一致**（对齐做法与 5140 例 `node` 对拍证据见「行终止符对齐」）；仅 U+2028 / U+2029 仍为已知且仅文档化的分歧。

## 背景与设计初衷

1. **自动转换表格**：将 assistant 输出中的任何 Markdown 表格自动转换为易读的 bullet 列表项目（`- **首列** — 列名: 值, ...`）。
2. **终端兼容性转换**：将终端渲染器不支持或易显示异常的 Markdown 格式重写：
   - Rule A: 将 H3-H6 标题转换为粗体文本行（`### 标题` → `**标题**`）；
   - Rule B: 将图片语法转换为文本链接（`![alt](url)` → `[alt](url)`，空 alt 填补为 `[image](url)`）；
   - Rule C & D: 将脚注定义与引用语法展平（`[^1]` → `[1]`，`[^1]: text` → `**[1]** text`）；
   - Rule E: 将 Markdown 链接展开为可点击的纯文本与裸 URL（`[label](url)` → `label url`）。
3. **修复紧贴代码块围栏 (Rule F)**：修复模型常将代码块开头围栏紧贴在正文行末导致的渲染错误（如 `**标题**```），将其换行放置到独立行；将缩进代码块围栏归零到第 0 列。
4. **修复粗体右侧闭合粘连 (Rule G)**：修复模型输出形如 `**注意：**后文` 时因标点与正文粘连导致 CommonMark 规范判定失败而显示为字面量星号的问题，在符合条件时自动在闭合 `**` 后补入空格。
5. **静默零配置**：完全在后台透明运行，无需任何用户交互与额外配置。

## TS 行号与 Go 实现对照表

原 TS 权威源：`extensions/no-tables/src/no-tables.ts`（317 行，md5: `a1f215b9843f38943cf86df38b4205f5`；Go 实现 `extension.go` 745 行、`extension_test.go` 720 行）

| 规则/功能 | TS 原版实现定位 | Go 移植版实现定位 | 行为一致性与实现说明 |
|---|---|---|---|
| **扩展注册入口** | L9-30: `export default function (pi)` 监听 `message_end` | `extension.go`: `Extension()` & `HandleMessageEnd()` | 监听 `sdk.EventMessageEnd`。提取 `data["message"]`，非 assistant 角色直接忽略返回 `(nil, nil)`。 |
| **消息体遍历与改写** | L16-29: 遍历 `parts`，改写 text 类型 block，检测改动后返回新 message | `extension.go`: `HandleMessageEnd()` | 遍历 `[]any` 中的 text blocks（浅拷贝 block 保持原字段），有改动时浅拷贝构造新 message map 返回 `map[string]any{"message": newMsg}`；无修改返回 `(nil, nil)`。防御性支持 `string` 形式 content。 |
| **管道串联** | L19: `convertUnsupportedMarkdown(convertTables(part.text))` | `extension.go`: `ConvertUnsupportedMarkdown(ConvertTables(partText))` | 保持严格一致的调用顺序：先执行表格转换，再执行不支持语法与围栏转换。 |
| **表格转换** | L284-310: `convertTables(md)` | `extension.go`: `ConvertTables(md string) string` | 正则匹配表格头、分隔行与数据行，首列作为 label，其余列作为 details，生成 bullet 列表。`^` 按 JS `/m` 在裸 CR 后也能起行，`.` 限定为 `[^\n\r]`：CRLF 表格与含裸 CR 单元格的表格都与 TS 一样保持原样不转 bullet。 |
| **单元格切分** | L312-318: `splitCells(line)` | `extension.go`: `splitCells(line string) []string` | 去除行首尾 `\|` 字符并按 `\|` 切分。 |
| **不支持语法总控** | L32-58: `convertUnsupportedMarkdown(md)` | `extension.go`: `ConvertUnsupportedMarkdown(md string) string` | 先跑 F1/F2 围栏归一化，按代码块切分，偶数段（非代码块正文）应用规则 A-E/G，最后再跑一次围栏归一化保证二次安全。 |
| **代码块围栏修复 (Rule F)** | L60-131: `normalizeMisplacedFences(md)` | `extension.go`: `NormalizeMisplacedFences(md string) string` | 状态机扫描每行（`split("\n")`，与 TS 相同），准确识别代码块内外状态；处理粘连在正文末尾的代码块围栏（F1）与缩进围栏（F2），保持代码块内部内容字节级不变。`reOpenFence` 的 `(.*)$` 改成 `([^\n\r]*)$`：TS 该正则无 `m` 标志，行尾带 CR 时 TS 不认它是围栏，Go 也必须不认，否则状态机会去缩进 TS 保留的行。 |
| **围栏行去缩进** | L133-135: `dedentFenceLine(line)` | `extension.go`: `dedentFenceLine(line string) string` | 去除前导空格与制表符（当后续紧随 3+ 反引号或波浪号时）。只做前缀检查、不看行尾，因此行尾 CR 不影响其行为（这正是 CRLF 样例 1–4 的机制）。 |
| **行内代码块切分** | L139-150: `splitByInlineCode(text)` | `extension.go`: `splitByInlineCode(text string) []string` | 确定性双指针扫描器，准确切分单行内由 N 个反引号闭合的行内代码跨度。 |
| **代码区间记录** | L152-163: `joinWithCodeRanges(segments)` | `extension.go`: `joinWithCodeRanges(segments []string) (string, []CodeSpanRange)` | 拼接片段并计算奇数段代码跨度在全文中的 `[Start, End)` 字节偏移。 |
| **正文规则应用** | L165-191: `applyMarkdownRules(text)` | `extension.go`: `applyMarkdownRules(text string) string` | 先执行行锚定规则 Rule A 与 Rule D，按行内代码切分后对正文应用 Rule C/B/E/G，再重组跨片段执行 Rule E 与 Rule G。 |
| **行内规则串联** | L193-210: `applyInlineMarkdownRules(text)` | `extension.go`: `applyInlineMarkdownRules(text string) string` | 串联 Rule C (脚注引用)、Rule B (图片)、Rule E (链接) 与 Rule G (粗体闭合)。 |
| **H3-H6 标题转粗体 (Rule A)** | L167: `text.replace(/^(#{3,6})\s+(.+?)\s*#*\s*$/gm, "**$2**")` | `extension.go`: `replaceHeadingLines(text)` | 匹配多行模式下的 H3-H6 标题行并替换为粗体文本行。`^`/`$`/`.` 按 JS `/m` 语义对齐（`jsLineStart` = `(?:^|(\r))`、`jsLineEnd` = `(\r\n|\r|\n|\z)`、`.` = `[^\n\r]`），因此 `"### a\rb"` 与 TS 一样输出 `"**a**\rb"`。 |
| **脚注定义展平 (Rule D)** | L170: `text.replace(/^\[\^([^\]]+)\]:\s*(.+)$/gm, "**[$1]** $2")` | `extension.go`: `replaceFootnoteDefinitions(text)` | 将脚注定义行转换为粗体序号开头的正文行。与 Rule A 共用 JS `/m` 锚点语义（`^` 在裸 CR 后断行、`$` 在 CR 前断行、`.` 不跨 CR）。 |
| **脚注引用展平 (Rule C)** | L195: `text.replace(/\[\^([^\]]+)\]/g, "[$1]")` | `extension.go`: `reFootRef.ReplaceAllString(text, "[$1]")` | 将 `[^id]` 引用简化为 `[id]`。 |
| **图片转链接 (Rule B)** | L198-201: `!+\[(.*?)\]\((.*?)\)` | `extension.go`: `reImg.ReplaceAllStringFunc(...)` | 将 Markdown 图片语法转换为文本链接，空 alt 兜底使用 `image`。 |
| **链接展开 (Rule E)** | L212-236: `applyLinkRule(text, codeRanges)` | `extension.go`: `applyLinkRule(text string, codeRanges []CodeSpanRange) string` | 展开 `[label](url)`，对 label 与 url 相同或 label 为空进行简化；跨越行内代码界定符时保留原样。 |
| **粗体闭合修复 (Rule G)** | L238-282: `applyBoldClosingRule(text, codeRanges)` | `extension.go`: `applyBoldClosingRule(text string, codeRanges []CodeSpanRange) string` | 覆盖 30 种标点符号集合；仅在闭合 `**` 紧随非空白、非星号、非标点字符时自动在闭合星号后补入空格；内部代码块跨越保护。 |

## 与 TS 原版的差异与 SDK 契约说明

1. **宿主生命周期事件与返回值采用**：
   - 原版 TS 扩展通过 `pi.on("message_end", async (event) => ...)` 监听并返回 `{ message: { ...event.message, content: newParts } }`。
   - Go 版通过 `e.OnEvent(sdk.EventMessageEnd, HandleMessageEnd)` 注册，并返回 `map[string]any{"message": newMsg}`。
   - **PiG 依据**：经查阅 PiG host 源码 `/root/workspace/ref/PiG/coding/extension/host/inproc/runner.go:2155-2197`（`EmitMessageEnd` 函数）及 `workspace/ref/PiG/test/extension-conformance/event_result_reference_test.go:190-197`，PiG host 在收到 `message_end` 的返回结果时，检查 `typed.Message != nil` 且角色一致后，原子替换当前的消息内容并返回。因此 Go 端的 map 返回值能被 PiG 完全正确接收并生效。
2. **正则引擎与解析器设计**：
   - 保留的 RE2 正则：`reTable`（表格匹配）、`reHeading`（标题匹配）、`reFootDef`（脚注定义）、`reFootRef`（脚注引用）、`reImg`（图片匹配）、`reLink`（链接匹配）、`reGluedTail`（粘连围栏行尾匹配）、`reOpenFence`（开启围栏）、`reCloseFence`（闭合围栏）、`reFencedBlock`（代码块分段）。
   - Go 标准库 `regexp` 基于 RE2 引擎，由有限自动机（DFA/NFA）保证线性时间复杂度，RE2 本身无回溯（这是引擎属性，不是本移植相对 TS 的改进）。
   - 由于 Go RE2 不支持零宽断言（lookaround 如 `(?<!...)` 与 `(?=...)`）及反向引用（`\1`），对于 TS 中依赖上述特性的部分，Go 版采用确定性双指针扫描器（`splitByInlineCode`）与线性状态机（`NormalizeMisplacedFences` 围栏状态、`applyBoldClosingRule` 标点判定）进行实现。
3. **换行符处理（CRLF / CR 行终止符）**：
   - Rule A–G 与表格转换在 `\n`、`\r\n`、`\r` 三种行终止符下均与 TS 逐字节一致（已用 `node` 直跑 TS 权威源对拍，见下节）。
   - 关键差异在于 JS `/m` 的 `^`/`$` 在裸 CR 处也断行、JS `.` 不跨 CR，而 Go 的 `(?m)^`/`(?m)$` 只认 `\n`、`.` 只排除 `\n`。Go 端用 `jsLineStart`/`jsLineEnd`/`jsDot` 三个构件对齐，详见下节。
   - 仍需注意的语义细节：TS 的行锚定规则末尾的 `\s*` 会吃掉行终止符本身（`"### a\r\n"` → `"**a**"`），Go 端因字符类 `jsWhitespaceClass` 与 TS `\s` 等价而自然一致。
   - 仅剩 U+2028（行分隔符）/U+2029（段分隔符）为已知且仅文档化的分歧。
4. **JavaScript `\s` 字符集对齐**：
   - Go RE2 的 `\s` 仅涵盖 ASCII 空白（`[\t\n\f\r ]`），而 JavaScript 正则中的 `\s` 包含 25 个空白字符（除 ASCII 外还含 `\u00a0`、`\u1680`、`\u2000-\u200a`、`\u2028`、`\u2029`、`\u202f`、`\u205f`、`\u3000`、`\ufeff` 与 `\v`）。
   - Go 实现定义了与 JS 等价的 `jsWhitespaceClass` 字符集常量及 `isJSWhitespace` 判定函数，使 H3-H6 标题转粗体（如 `###\u3000标题`、`### a\u3000`）及脚注定义（如 `[^1]:\u3000text`）与 TS 输出完全一致。

## 行终止符对齐（`\n` / `\r\n` / `\r`）

### 已完成：CRLF / CR 与 JS 正则语义对齐

Windows 换行的文件/工具输出在实际中常见，所以第 1 批遗留的 CRLF/CR 分歧已为本批修完，不留作永久分歧。修法不是简单把 `.` 换成 `[^\n\r]`（那样只治一半，`(?m)$` 仍不在 CR 断行），而是把 JS `/m` 的三类语义全部搬到 Go：

- `jsLineStart = (?:^|(\r))`：JS 的 `^` 在裸 CR 后也断行。RE2 没有 lookbehind，所以裸 CR 被**消费进捕获组 1**，替换时原样写回（净效果等同零宽锚点）。`\r\n` 中 `\r` 与 `\n` 之间的位置不需要单独分支：后面紧跟 `\n`，而任何规则都不可能以 `\n` 开头。
- `jsLineEnd = (\r\n|\r|\n|\z)`：JS 的 `$` 在任意行终止符前也断行。同样消费后写回；`replaceWithJSLines` 把「写出内容」与「重扫位置」都落在终止符**起点**，复现 JS 零宽 `$` 之后游标仍在终止符处的位置（否则 `"### a\r\r#### b"` 这类相邻 CR 会漏掉后一个标题）。
- `jsDot = [^\n\r]`：JS 的 `.` 不跨 CR、LF。

落地范围：

- `reTable`、`reHeading`、`reFootDef` 改用上述三个构件；Rule A / Rule D 的替换由 `replaceHeadingLines` / `replaceFootnoteDefinitions` 按捕获组重组（`replaceWithJSLines` 是两者共用的扫描循环）。
- `reOpenFence` 的 `(.*)$` 改为 `([^\n\r]*)$`：TS 这条正则没有 `m` 标志，行尾带 CR 时 TS 根本不认为它是围栏，Go 也必须不认，否则围栏状态机会去缩进 TS 保留的行。
- `reCloseFence`、`reGluedTail`、`reFencedBlock`、`dedentFenceLine`、`splitByInlineCode`、`reFootRef`、`reImg`、`reLink` 逐条比对后无需改动（`reImg`/`reLink` 早已用 `[^\r\n]`；其余不涉及 `^`/`$`/`.` 或本就不用 `/m`）。结论写在 `extension.go` 对应正则的注释里。

原「待修」清单 6 条样例现已与 TS 逐字节一致（下表右列即 TS 与 Go 的共同输出，由 `node` 直跑 TS 权威源得到）：

| # | 输入 | TS 输出 = Go 输出 |
|---|---|---|
| 1 | `"```\r\n    ```\r\n"` | `"```\r\n```\r\n"`（F2 去缩进） |
| 2 | `"```bash\r\n    ```\r\n"` | `"```bash\r\n```\r\n"` |
| 3 | `"```\r\ncode\r\n    ```\r\n"` | `"```\r\ncode\r\n```\r\n"` |
| 4 | `"````markdown\r\n    ```\r\n    code\r\n    ```\r\n````\r\n"` | `"````markdown\r\n```\r\n    code\r\n```\r\n````\r\n"` |
| 5 | `"### a\rb"` | `"**a**\rb"`（`$` 在 CR 前断行，只转 `a`） |
| 7 | `"| a\rb | c |\n| - | - |\n| 1 | 2 |\n"` | 原样（`.` 不跨 CR，表格不成立） |

一个容易误判的点：样例 1–4 的真正机制是 **F2 去缩进**（`dedentFenceLine` 只做前缀检查），不是闭合围栏状态机。CRLF 行尾让 TS 的 `open`/`close` 两个正则都匹配失败，状态机因此根本不介入；Go 修正 `reOpenFence` 后与之一致。

### 对拍证据（node 直跑 TS 权威源）

对拍方法：把 TS 权威源原样复制到临时目录，仅在文件尾追加一行 `export { convertTables }`（`convertTables` 原本未导出，不改任何逻辑），用 `node` 直接 import（Node ≥ 22.18 原生类型剥离）；对每个输入同时对比 `convertTables`、`normalizeMisplacedFences`、`convertUnsupportedMarkdown` 与完整管道四个字段。

- 语料：4 组固定种子共 **5140** 个输入（README 8 条样例 + 片段随机拼接 + 行终止符密集的字符级随机 + 逐片段的 `\n`/`\r\n`/`\r` 矩阵 + 仓库既有测试语料的三种行终止符变体），共 **20560** 次字段对比。
- 结果：**0 处非预期分歧**；仅下方条目 6、8 的 U+2028/U+2029 输出不一致，与本节的「仅文档化」口径一致。
- 已固化回回归测试：`TestLineTerminatorParity_READMEItems`（原 6 条待修样例）、`TestLineTerminatorParity_RuleCoverage`（26 子用例，覆盖 Rule A–G 与表格）、`TestDocumentedDeviations_U2028U2029`（3 条）。测试里的期望值全部由上述 `node` 对拍生成，不是手写推导。

### 仅文档化（不计划修）：U+2028 / U+2029

由于 Unicode 行分隔符 U+2028 与段分隔符 U+2029 在实际中罕见，相关分歧仅文档化、不计划修（`jsLineStart`/`jsLineEnd`/`jsDot` 未纳入这两个字符）。

6. `"### a\u2029b"`（罕见、暂不修）：
   - TS 输出：`"**a**\u2029b"`（TS 的 `reHeading` 将 U+2029 视作行尾，仅将 `a` 转换为粗体）
   - Go 输出：`"**a\u2029b**"`（Go 的 `(?m)$` 不视 U+2029 为行尾，整行 `a\u2029b` 被转换为粗体）
8. `"[a\u2028b](https://x.com)"` 与 `"![a\u2028b](https://x.com/a.png)"`（罕见、暂不修）：
   - TS 输出：`"[a\u2028b](https://x.com)"` 与 `"![a\u2028b](https://x.com/a.png)"`（TS 原样：JS 正则 `.` 不匹配 U+2028，无法匹配为有效图片/链接，保持原样）
   - Go 输出：`"a\u2028b https://x.com"` 与 `"a\u2028b https://x.com/a.png"`（Go 会展开：Go 正则 `.` 匹配 U+2028，图片转为链接后被展开为纯文本与裸 URL）

## 如何运行本地测试

在 `/root/workspace/pi-extensions/go/no-tables` 目录下：

```bash
# 格式化（检查）
gofmt -l .

# 静态分析
go vet ./...

# 编译检查
go build ./...

# 运行对照单元测试（race 检测 + 逐用例输出）
go test -count=1 -race -v ./...
```

需要重做与 TS 的逐字节对拍时（可选，不属常规测试依赖）：把 `extensions/no-tables/src/no-tables.ts` 复制到临时目录并在文件尾追加 `export { convertTables }`，用 `node` 直接 import 后对同一批输入分别取 TS 与 Go 的 `convertTables`/`normalizeMisplacedFences`/`convertUnsupportedMarkdown`/完整管道四个输出逐字段比较。

测试集涵盖：
1. 部署版（`/root/.pi/agent/extensions/__tests__/no-tables.test.ts`）与仓内版（`extensions/no-tables/test/no-tables.test.ts`）全部 20 条原版测试用例（F1/F2 围栏修复、幂等性、行内反引号、标点保护、Rule G 粗体修复、Rule E 链接展开）；
2. Markdown 表格转换基础、单列、空表头、多表格用例；
3. Rule A-E 不支持语法转换用例；
4. 边界用例（空文本、纯代码块、已是纯文本、CRLF 换行等）；
5. `HandleMessageEnd` 拦截机制、非 assistant 过滤、无改动 no-op、只读不可变性验证；
6. Unicode 空白符（JS `\s`）对齐用例（全角空格、不间断空格等）；
7. CRLF / CR 与 TS 的对拍回归用例：`TestLineTerminatorParity_READMEItems`（原 6 条待修样例）+ `TestLineTerminatorParity_RuleCoverage`（26 子用例，覆盖 Rule A–G 与表格、混合行终止符、完整文档），期望值全部由 `node` 跑 TS 权威源生成；
8. 仅文档化的 U+2028 / U+2029 分歧冻结用例（3 条，显式固化 Go 行为以便回归时清晰可见）。

## 如何在隔离 PiG 环境中加载

使用 `go/` 目录下的 `./bin/pig-test` 隔离测试脚本进行加载验证：

```bash
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./no-tables -p "回复 OK"
```
