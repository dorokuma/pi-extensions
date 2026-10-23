# no-tables (Go 移植版)

本扩展是原 TS 版 `no-tables.ts`（Pi 扩展分发中枢权威源）向 PiG Go 扩展体系的移植实现。在纯 LF 输入下，Rule A–G 与表格转换与 TS 逐字节一致；含 CR(`\r`)/U+2028/U+2029 的输入存在已知分歧。其中 CRLF 对齐已列入下一批待修项，不作为永久接受的分歧。

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

原 TS 权威源：`extensions/no-tables/src/no-tables.ts`（317 行，md5: `a1f215b9843f38943cf86df38b4205f5`；Go 实现 `extension.go` 608 行、`extension_test.go` 660 行）

| 规则/功能 | TS 原版实现定位 | Go 移植版实现定位 | 行为一致性与实现说明 |
|---|---|---|---|
| **扩展注册入口** | L9-30: `export default function (pi)` 监听 `message_end` | `extension.go`: `Extension()` & `HandleMessageEnd()` | 监听 `sdk.EventMessageEnd`。提取 `data["message"]`，非 assistant 角色直接忽略返回 `(nil, nil)`。 |
| **消息体遍历与改写** | L16-29: 遍历 `parts`，改写 text 类型 block，检测改动后返回新 message | `extension.go`: `HandleMessageEnd()` | 遍历 `[]any` 中的 text blocks（浅拷贝 block 保持原字段），有改动时浅拷贝构造新 message map 返回 `map[string]any{"message": newMsg}`；无修改返回 `(nil, nil)`。防御性支持 `string` 形式 content。 |
| **管道串联** | L19: `convertUnsupportedMarkdown(convertTables(part.text))` | `extension.go`: `ConvertUnsupportedMarkdown(ConvertTables(partText))` | 保持严格一致的调用顺序：先执行表格转换，再执行不支持语法与围栏转换。 |
| **表格转换** | L284-310: `convertTables(md)` | `extension.go`: `ConvertTables(md string) string` | 正则匹配表格头、分隔行与数据行，首列作为 label，其余列作为 details，生成 bullet 列表。CRLF 输入不做任何额外宽容，与 TS 一致，CRLF 表格保持原样不处理。 |
| **单元格切分** | L312-318: `splitCells(line)` | `extension.go`: `splitCells(line string) []string` | 去除行首尾 `\|` 字符并按 `\|` 切分。 |
| **不支持语法总控** | L32-58: `convertUnsupportedMarkdown(md)` | `extension.go`: `ConvertUnsupportedMarkdown(md string) string` | 先跑 F1/F2 围栏归一化，按代码块切分，偶数段（非代码块正文）应用规则 A-E/G，最后再跑一次围栏归一化保证二次安全。 |
| **代码块围栏修复 (Rule F)** | L60-131: `normalizeMisplacedFences(md)` | `extension.go`: `NormalizeMisplacedFences(md string) string` | 状态机扫描每行，准确识别代码块内外状态；处理粘连在正文末尾的代码块围栏（F1）与缩进围栏（F2），保持代码块内部内容字节级不变。 |
| **围栏行去缩进** | L133-135: `dedentFenceLine(line)` | `extension.go`: `dedentFenceLine(line string) string` | 去除前导空格与制表符（当后续紧随 3+ 反引号或波浪号时）。 |
| **行内代码块切分** | L139-150: `splitByInlineCode(text)` | `extension.go`: `splitByInlineCode(text string) []string` | 确定性双指针扫描器，准确切分单行内由 N 个反引号闭合的行内代码跨度。 |
| **代码区间记录** | L152-163: `joinWithCodeRanges(segments)` | `extension.go`: `joinWithCodeRanges(segments []string) (string, []CodeSpanRange)` | 拼接片段并计算奇数段代码跨度在全文中的 `[Start, End)` 字节偏移。 |
| **正文规则应用** | L165-191: `applyMarkdownRules(text)` | `extension.go`: `applyMarkdownRules(text string) string` | 先执行行锚定规则 Rule A 与 Rule D，按行内代码切分后对正文应用 Rule C/B/E/G，再重组跨片段执行 Rule E 与 Rule G。 |
| **行内规则串联** | L193-210: `applyInlineMarkdownRules(text)` | `extension.go`: `applyInlineMarkdownRules(text string) string` | 串联 Rule C (脚注引用)、Rule B (图片)、Rule E (链接) 与 Rule G (粗体闭合)。 |
| **H3-H6 标题转粗体 (Rule A)** | L167: `text.replace(/^(#{3,6})\s+(.+?)\s*#*\s*$/gm, "**$2**")` | `extension.go`: `reHeading.ReplaceAllString(text, "**$2**")` | 匹配多行模式下的 H3-H6 标题行并替换为粗体文本行。 |
| **脚注定义展平 (Rule D)** | L170: `text.replace(/^\[\^([^\]]+)\]:\s*(.+)$/gm, "**[$1]** $2")` | `extension.go`: `reFootDef.ReplaceAllString(text, "**[$1]** $2")` | 将脚注定义行转换为粗体序号开头的正文行。 |
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
3. **换行符处理（CRLF 与行终止符）**：
   - 在规范的 LF 输入下，Rule A–G 与表格转换与 TS 逐字节一致。
   - 在含 CR（`\r`）、U+2028（行分隔符）、U+2029（段分隔符）的输入上，由于 Go `.`/`(?m)$` 与 JS `.`/`m$` 存在语义差异（Go `.` 匹配 `[^\n]`，JS `.` 匹配 `[^\n\r\u2028\u2029]`），两者存在已知分歧，详见下节实测对比。对于标准的纯 CRLF 规范表格行（`\r\n` 分隔行）与纯 CRLF 粘连围栏，两者均保持原样不予转换。
4. **JavaScript `\s` 字符集对齐**：
   - Go RE2 的 `\s` 仅涵盖 ASCII 空白（`[\t\n\f\r ]`），而 JavaScript 正则中的 `\s` 包含 25 个空白字符（除 ASCII 外还含 `\u00a0`、`\u1680`、`\u2000-\u200a`、`\u2028`、`\u2029`、`\u202f`、`\u205f`、`\u3000`、`\ufeff` 与 `\v`）。
   - Go 实现定义了与 JS 等价的 `jsWhitespaceClass` 字符集常量及 `isJSWhitespace` 判定函数，使 H3-H6 标题转粗体（如 `###\u3000标题`、`### a\u3000`）及脚注定义（如 `[^1]:\u3000text`）与 TS 输出完全一致。

## 已知未复刻的行终止符分歧

在 LF 输入下，Rule A–G 与表格转换与 TS 逐字节一致；含 CR（`\r`）/U+2028/U+2029 的输入存在已知分歧。现按处置方案拆分为两个小节：

### 下一批待修：CRLF / CR 行终止符对齐

在 LF 输入下，Rule A–G 与表格转换与 TS 逐字节一致。Windows 换行的文件/工具输出在实际中常见，不能只文档化，CRLF（`\r\n`，以及裸 CR `\r`）已定调为下一批必须修的项。

修法方向是让受影响的正则与切行逻辑对齐 CRLF/CR：名单收敛为 `reTable`、`reOpenFence`、`reHeading`、`reFootDef`（加上「按 JS 行终止符切行」的通用逻辑；`reImg` 与 `reLink` 已经用 `[^\r\n]` 对齐了 CRLF/CR，只剩 U+2028/U+2029 分歧，属「仅文档化」栏），目标字符类写 `[\n\r]`（注明 U+2028/U+2029 暂不纳入），并提示「简单把 `.` 换成 `[^\n\r]` 不够，`(?m)$` 也要在 CR 断行，否则分歧会增多（实测从 263 例涨到 371 例）」。

编译期与运行期可复现的对比例子如下（凡例子中含 `\r` 的，均待修）：

1. `"```\r\n    ```\r\n"`（待修）：
   - TS 输出：`"```\r\n```\r\n"`（TS 把缩进闭合行去缩进）
   - Go 输出：`"```\r\n    ```\r\n"`（Go 原样）
2. `"```bash\r\n    ```\r\n"`（待修）：
   - TS 输出：`"```bash\r\n```\r\n"`（TS 把缩进闭合行去缩进）
   - Go 输出：`"```bash\r\n    ```\r\n"`（Go 原样）
3. `"```\r\ncode\r\n    ```\r\n"`（待修）：
   - TS 输出：`"```\r\ncode\r\n```\r\n"`（TS 把缩进闭合行去缩进）
   - Go 输出：`"```\r\ncode\r\n    ```\r\n"`（Go 原样）
4. `"````markdown\r\n    ```\r\n    code\r\n    ```\r\n````\r\n"`（待修）：
   - TS 输出：`"````markdown\r\n```\r\n    code\r\n```\r\n````\r\n"`（TS 把内层缩进围栏去缩进）
   - Go 输出：`"````markdown\r\n    ```\r\n    code\r\n    ```\r\n````\r\n"`（Go 原样）
5. `"### a\rb"`（待修）：
   - TS 输出：`"**a**\rb"`（TS 的 `reHeading` 将 `\r` 视作行尾，仅将 `a` 转换为粗体）
   - Go 输出：`"**a\rb**"`（Go 的 `(?m)$` 仅匹配 `\n`，整行 `a\rb` 被转换为粗体）
7. `"| a\rb | c |\n| - | - |\n| 1 | 2 |\n"`（待修）：
   - TS 输出：`"| a\rb | c |\n| - | - |\n| 1 | 2 |\n"`（TS 原样：JS 正则 `.` 不匹配 `\r`，表格未被转换）
   - Go 输出：`"- **1** — c: 2\n"`（Go 会转成 bullet：Go 正则 `.` 匹配 `\r`，表格被转换）

### 仅文档化（不计划修）：U+2028 / U+2029

在 LF 输入下，Rule A–G 与表格转换与 TS 逐字节一致。由于 Unicode 行分隔符 U+2028 与段分隔符 U+2029 在实际中罕见，相关分歧仅文档化、不计划修。

编译期与运行期可复现的对比例子如下（罕见、暂不修）：

6. `"### a\u2029b"`（罕见、暂不修）：
   - TS 输出：`"**a**\u2029b"`（TS 的 `reHeading` 将 U+2029 视作行尾，仅将 `a` 转换为粗体）
   - Go 输出：`"**a\u2029b**"`（Go 的 `(?m)$` 不视 U+2029 为行尾，整行 `a\u2029b` 被转换为粗体）
8. `"[a\u2028b](https://x.com)"` 与 `"![a\u2028b](https://x.com/a.png)"`（罕见、暂不修）：
   - TS 输出：`"[a\u2028b](https://x.com)"` 与 `"![a\u2028b](https://x.com/a.png)"`（TS 原样：JS 正则 `.` 不匹配 U+2028，无法匹配为有效图片/链接，保持原样）
   - Go 输出：`"a\u2028b https://x.com"` 与 `"a\u2028b https://x.com/a.png"`（Go 会展开：Go 正则 `.` 匹配 U+2028，图片转为链接后被展开为纯文本与裸 URL）

## 如何运行本地测试

在 `/root/workspace/pi-extensions/go/no-tables` 目录下：

```bash
# 静态分析
go vet ./...

# 编译检查
go build ./...

# 运行对照单元测试
go test -count=1 -v ./...
```

测试集涵盖：
1. 部署版（`/root/.pi/agent/extensions/__tests__/no-tables.test.ts`）与仓内版（`extensions/no-tables/test/no-tables.test.ts`）全部 20 条原版测试用例（F1/F2 围栏修复、幂等性、行内反引号、标点保护、Rule G 粗体修复、Rule E 链接展开）；
2. Markdown 表格转换基础、单列、空表头、多表格用例；
3. Rule A-E 不支持语法转换用例；
4. 边界用例（空文本、纯代码块、已是纯文本、CRLF 换行等）；
5. `HandleMessageEnd` 拦截机制、非 assistant 过滤、无改动 no-op、只读不可变性验证；
6. Unicode 空白符（JS `\s`）对齐用例（全角空格、不间断空格等）；
7. 已知未复刻的行终止符分歧冻结用例（8 条典型分歧样例的全等断言，显式固化 Go 行为以便回归时清晰可见）。

## 如何在隔离 PiG 环境中加载

使用 `go/` 目录下的 `./bin/pig-test` 隔离测试脚本进行加载验证：

```bash
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./no-tables -p "回复 OK"
```
