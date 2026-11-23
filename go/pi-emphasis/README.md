# pi-emphasis（Go 移植版）

可扫读的回复。本扩展把助手回复里的 `[[Done]]`、`[[Failed]]`、`[[Needs you]]`
渲染成带图标的彩色胶囊，给标题与行首粗体标签按词配上图标，并为粗体、箭头
`→`、对勾 `✓`/`✗` 与树线 `├──`/`└──` 着色。**只改绘制**：落盘消息、复制文本
与 HTML 导出保留原始 Markdown，不调用模型、零成本运行。

- **上游出处**：[johnlindquist/pi-emphasis](https://github.com/johnlindquist/pi-emphasis)
  v0.2.0，锁定 commit `f884b736c489944ee778ab951c7f78580f137b02`。
- **移植目标**：本机 `pig 0.7.0+1.5.0`（PiG Go 扩展运行时，staged SDK
  `sdk-51955af53b263ab7`）。
- **模块**：`github.com/dorokuma/pi-extensions/go/pi-emphasis`，包名 `emphasis`
  （`package emphasis`，无 `package main`、无 `init()`；`init` 级初始化仅用于
  构建只读查找表 `toneWords`/`known`）。
- **版本**：`const Version = "0.1.1"`，遵循 `../../VERSIONING.md`。

## 功能概览

| 输入 | 绘制效果 |
|---|---|
| `**anchor term**` | 保持粗体并染主题色（默认 `mdLink` 链接蓝） |
| `[[Word]]` | 按词着色的圆角胶囊，并带该词的图标（≤32 字符） |
| `## Heading` | 标题词命中图标表时前置图标，行内 `[[Word]]` 渲染为胶囊；胶囊后仍有文字时恢复标题粗体与 `mdHeading` 前景色（标题本身着色仍由宿主负责） |
| `**Label:**`（行首/列表项/树线后） | 行首粗体标签按词配图标 |
| `**Check:**` 等 9 个动作引导词 | 行内任意位置、带冒号时配图标 |
| `→` | accent 色 |
| `✓` / `✔`、`✗` / `✘` | success / error 色 |
| `├──` / `└──` | dim 色（faint dim 用拆分闭合码，见下文兼容设计） |

受保护不改写：围栏代码块、**任意等长 N 反引号行内代码**、Markdown 链接/图片
`[label](url)`、转义的 `\*`。标题行（`##`）内不再整体豁免：`[[Word]]` 按正文同一
`protectedSpans` 契约渲染为胶囊（代码跨段与可解析链接内仍原样保留），仅在胶囊后
仍有后续字符时追加 `\x1b[1m` + `mdHeading` 前景恢复宿主 headingInline 的粗体与颜色。

## 胶囊词表（7 类）

| 色调 | 含义 | core（指导语要求模型使用） | also（同等着色接受） |
|---|---|---|---|
| error 红 | 出错了 | Failed, Blocked, Broken, Unsafe | fail, failing, error, rejected, down |
| warning 黄 | 看这里 | Needs you, Decision, Unverified, Partial, Skipped, In progress, Pending, At risk | needs review, waiting, review, todo, stale, draft |
| success 绿 | 已落定 | Done, Passed, Observed, Fixed, Live, Recommended, Keep | pass, ready, merged, safe, verified, shipped, ok, approved, complete, completed, finished, healthy |
| danger 实红 | 有后果 | Costs money, Destructive, Irreversible | — |
| mode 青 | 工作类型 | Brainstorm, Investigate, Preview, Implement, Qualify | — |
| owner 蓝 | 归属 | Owned, Shared, Foreign | — |
| neutral 软蓝 | 上下文/其它 | Inferred, New, Changed, Optional（`ContextWords`）及任意短标签 | — |

**同义词归一**（`SYNONYMS`）：complete/completed/ok/finished → `Done`，pass →
`Passed`，fail/failing/error → `Failed`，waiting → `Pending`，needs review →
`Review`。已知词可带细节：`[[Passed 12/12]]`、`[[Failed: 3 tests]]`。标签自带
PUA 图标（`\ue000-\uf8ff` / `\u{f0000}-\u{ffffd}`）时优先使用自带图标。

**标题/行首标签图标**（`LABEL_ICONS`，32 词）：finding, terminal, folder, file,
branch, review, ideas, cause, fix, risk, limits, next, rollback, undo, options,
result, why, how, cost, rule, example, checked, pills, colors, icons, scoreboard,
verdict, effect, evidence, plan, status, decision。

**动作引导词图标**（`ACTION_ICONS`，9 词，须带冒号）：check, do, next, unblock,
left, run, guard, undo, see。

## 环境变量（4 个）

| 变量 | 作用 | 默认 |
|---|---|---|
| `PI_EMPHASIS_COLOR` | 粗体着色的主题 token | `mdLink` |
| `PI_EMPHASIS_ROUND_CAPS` | `=0` 用方形芯片（无 Nerd Font 圆角时） | 圆角 |
| `PI_EMPHASIS_PILL_ICONS` | `=0` 关闭胶囊/标题/引导词全部图标 | 开 |
| `PI_EMPHASIS_GUIDANCE` | `=0` 不注入 `reply_style` 系统提示词段 | 注入（仅 TUI） |

## 针对 `pig 0.7.0+1.5.0` 的 5 项兼容设计

1. **词表保序 + 确定性 `ReplyGuidance()`**：`Vocabulary`、`LABEL_ICONS`、
   `ACTION_ICONS` 全部用有序切片定义（Go map 迭代序随机），词表按上游
   `Object.keys` 插入序排列；`KNOWN` 用 `sort.SliceStable` 按长度降序，
   `ReplyGuidance()` 输出与上游 TS 逐字一致。TUI 模式 `session_start`
   激活后，`before_agent_start` 向 `*sdk.SystemPromptSections`
   `.Set("reply_style", ReplyGuidance())`；非 TUI 或 `PI_EMPHASIS_GUIDANCE=0`
   时 `.Delete("reply_style")`，返回 `(nil, nil)`。
2. **保护围栏代码块、内联代码与 Markdown 链接**：宿主内置 Mermaid 转换器
   先运行并把含 ANSI 的图表包进多反引号代码跨段；`markedInlineLink` 在
   `[label](url)` 内被注入裸 `\x1b[` 会因未配对 `[` 解析破损。块级按
   CommonMark §4.5 围栏状态机解析：`parseFenceOpen`/`isFenceClose` 校验开闭
   围栏同字符、闭栏长度 ≥ 开栏且行尾仅空白，反引号围栏 info string 含 `` ` ``
   时判为单行内联代码跨段而非多行围栏。行内由 `protectedSpans` 左到右切分
   两类受保护片段——① CommonMark 任意等长 N 反引号代码跨段（与 `go/no-tables`
   `splitByInlineCode` 相同的精确等长闭合扫描）；② 行内链接/图片整体长度由
   `matchInlineLink` 给出——宿主 `tui/markdown_marked.go` `markedInlineLink`
   5 分支标签扫描 + `link()`/`outputLink()` 的严格等价移植：裸 destination
   按宿主 `findClosingBracket` 平衡截断、非图片标签内嵌套真链接按
   `outputLink` 让渡内层（标签经 `markedUnescapeBrackets` 反转义后在自身
   边界内重新分词探测），零第三方正则依赖、纯标准库有界回退。片段内部原样
   保留不注入 ANSI，`inBold != ""` 时在片段后补 `open` 恢复粗体前景色。
   单行超过 `8192` 字节时按 display-only 安全护栏原样透传不做强调装饰
   （围栏开闭跟踪仍照常维护）；8192 字节以内配合按行 `linkCache` 位置
   记忆化将单行最坏耗时控制在毫秒级。
3. **`theme: "system"` 空背景回退 + `dim` 不打断外层粗体**：`GetBgAnsi` 返回
   `("\x1b[49m", nil)`、空串或错误时一律回退「前景 + `\x1b[7m` 反显」，
   `labelFg == "text"` 升级为 `"accent"`；`UITheme.Fg("dim", …)` 会输出
   `\x1b[22;39m` 合并码打断外层 `**bold**` 且不被宿主 `ansiSpan` 重开，故
   faint dim（`\x1b[2m` 结尾）经 `GetFgAnsi` 自拼、收尾写拆开的
   `\x1b[22m\x1b[39m`，并在 `inBold != ""` 时用 `reopen()`
   （`\x1b[1m` + `open`）重开粗体与颜色。
4. **防未知颜色 panic + 未就绪调色板保护**：`UITheme.Fg` 对非法 token 直接
   panic，适配器对 `PI_EMPHASIS_COLOR` 一律先 `GetFgAnsi` 校验；失败回退
   `"mdLink"`，仍为空（宿主未下发调色板）则本次转换原样返回 `markdown`。
5. **主题热切换与并发安全**：`AsyncMarkdownTransform` 与事件回调可并发，
   扩展实例状态用 `sync.RWMutex` 保护，并以正交双代数 `sessionGen`/
   `paletteGen` 并发状态机区分会话换绑与调色板失效：`sessionGen` 仅在
   `setThemeFunc`/`clearTheme` 内递增，`setUnsubTheme` 按它判定订阅过期，
   `invalidate()` 不会把当前 `OnThemeChange` 订阅误退订；`paletteGen` 在
   `setThemeFunc`/`clearTheme`/`invalidate` 均递增，`paletteFor` 锁外构建后
   仅当代数未变才写回缓存。`OnSessionStart`（`ctx.Mode()=="tui"` 时激活并
   `ctx.OnThemeChange` 置脏缓存；非 TUI 会话调 `clearTheme()` 解绑订阅、清空
   主题与缓存调色板）与 `MarkdownTransformer`（比对 `themeFingerprint`：
   `Name + ColorMode + 关键 token ANSI`，并同时覆盖 `PI_EMPHASIS_COLOR`——
   含自定义 token 经 `GetFgAnsi` 解析后的前景色 ANSI——与
   `PI_EMPHASIS_ROUND_CAPS`）安全重建缓存 `Palette`；输入 `""` O(1) 直返。RE2 适配：`OWN_ICON` 写作
   `\x{e000}-\x{f8ff}` 与 `\x{f0000}-\x{ffffd}`；胶囊 `pad`（`\u00a0`）保持
   包裹在 `\x1b[1m` 与 `\x1b[22;27m\x1b[39m\x1b[49m` 内侧防行尾裁剪。

## 构建与测试

```sh
# 组件级
cd go/pi-emphasis
gofmt -l .            # 无输出
go vet ./...
go test -race -v ./...

# 仓级门禁（go/ 目录）
cd go && make build && make vet && make sdk-check
```

## 部署

本组件为纯 Go 扩展，直接把含 `go.mod` 的模块目录软链到扩展加载目录即可
（不走 `federated/registry.json` 的 `.ts` 清单）：

```sh
ln -sfn /root/workspace/pi-extensions/go/pi-emphasis \
  ~/.pig/agent/extensions/pi-emphasis
```

即 `$PIG_HOME/agent/extensions/pi-emphasis` 指向本模块目录；`/reload` 或新开
会话生效。PiG 构建期以 `GOWORK=off` 把
`require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0` 重定向到 staged
SDK，因此组件 `go.mod` 严禁写本地 `replace`。

## 许可

MIT：上游 `Copyright (c) 2026 John Lindquist` + Go 移植
`Copyright (c) 2026 dorokuma`，见 `LICENSE`。
