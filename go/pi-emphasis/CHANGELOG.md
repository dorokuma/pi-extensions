# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/pi-emphasis` 的版本变更，格式遵循
[Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录
[VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

### Fixed

- 标题行内 `[[Word]]` 现渲染为胶囊：此前 `Decorate` 的 heading 分支只给标题词加图标，
  `[[…]]` 原样透传；现按正文同一 `protectedSpans` 契约在非受保护片段中调用 `pill()`，
  并在胶囊后仍有后续字符时追加 `\x1b[1m` + `mdHeading` 前景色（新增 `Palette.Heading`，
  经 `theme.GetFgAnsi("mdHeading")` 解析、查不到留空），恢复宿主 headingInline 的粗体与
  颜色；`themeFingerprint` 固定 fg token 列表同步加入 `mdHeading`，主题切换可正确重建。
  行内代码/可解析链接内的 `[[…]]` 保持原样，行尾胶囊不冗余追加恢复序列。

## [0.1.0] - 2026-11-19

首次发布。Go port of [johnlindquist/pi-emphasis](https://github.com/johnlindquist/pi-emphasis)
v0.2.0（commit `f884b736c489944ee778ab951c7f78580f137b02`）：助手回复的
display-only 强调渲染——`[[Word]]` 彩色图标胶囊、标题/行首标签图标、粗体/
箭头/对勾/树线主题着色，以及 TUI 模式下的 `reply_style` 系统提示词注入。

### Added

- 完整移植上游 `index.ts` 全部导出语义：`PillTone`、`Colorize`、`PillStyle`、
  `Palette`、`ThemeLike`、`Vocabulary`、`ContextWords`、`ToneOf`、
  `CreatePalette`、`Decorate`、`ReplyGuidance`、`ColorBold` 与扩展工厂
  `Extension()`（`Version = "0.1.0"`，`package emphasis`，无 `package main`）。
- 7 类胶囊词表（error/warning/success/danger/mode/owner/neutral）+ also 接受词
  + `ContextWords`；`SYNONYMS` 同义词归一显示；已知词可带 `12/12`、`: 3 tests`
  细节；PUA 自有图标优先。
- `before_agent_start` 注入 `reply_style` 指导段（仅 TUI，约 1.5K tokens），
  `PI_EMPHASIS_GUIDANCE=0` 可关闭。
- 上游 `test/emphasis.test.ts` 全部断言的 Go 迁移 + `demo/deploy-reply.md` /
  `demo/every-pill.md` 端到端渲染回归 + `-race` 并发与生命周期单测；含
  `TestExtensionWireLifecycleViaRunWithConn`，以 `net.Pipe()` 驱动真实
  `Extension().RunWithConn` 完成 register→ready→theme_change→
  session_start→before_agent_start→markdown_transform→shutdown 协议闭环。

### Changed（针对 `pig 0.7.0+1.5.0` 的兼容增强）

- 词表保序 + 确定性 `ReplyGuidance()`：`Vocabulary`/`LABEL_ICONS`/
  `ACTION_ICONS` 改有序切片（`SYNONYMS` 同样经有序 `synonymEntries` 切片构建，
  `known` 词表构造不迭代无序 map），词表按上游 `Object.keys` 插入序排列，
  `ReplyGuidance()` 输出与上游逐字一致（Go map 迭代序随机会漂移词表序）。
- CommonMark §4.5 围栏状态机：`parseFenceOpen` 取捕获组纯围栏串，反引号围栏
  info string 含 `` ` `` 时判定为单行内联代码跨段（对齐宿主
  `parseMarkdownFenceOpen`，覆盖宿主 Mermaid 转换器行首 ` ```...``` ` 输出），
  不再误开多行围栏；`isFenceClose` 逐字对齐宿主 `isMarkdownFenceClose`
  （`tui/markdown.go:1055-1062`）：0-3 空格缩进、开启字符 run ≥ 开启长度、
  其后允许任意 `~`/`` ` `` 混合串、尾部仅认 ASCII 空格（` ```~~~ ` 判闭栏、
  尾部 tab 不闭栏），修复缩进闭合失锁、`  ~~~` 伪闭合反引号块与 ` ```ts`
  信息串伪闭合。
- `protectedSpans` 链接保护改线性扫描：行内链接/图片整体长度由
  `matchInlineLink` 给出——宿主 `tui/markdown_marked.go` `markedInlineLink`
  正则 + `link()` + `outputLink()`（:163、:711-790）的严格等价移植，标签按
  宿主 5 分支交替精确扫描——分支 1 `\[(?:\\[\s\S]|[^\[\]\\])*\]` 整段消费嵌套
  `[...]`（内部反引号为普通字符）、分支 2 `\\[\s\S]`、分支 3
  `` `+(?!`)[^`]*?`+(?!`) `` 递归续扫、分支 4 `` ``+(?=\]) `` 仅 run≥2 且紧邻
  `]`、分支 5 `[^\[\]\\`]`——未闭合单反引号或嵌套括号未配对整体返回 0（不再
  误保护伪链接），嵌套 `` [`v1]` `` 内反引号不再跨出吞同行代码跨段；标签内
  未闭合反引号串经独立前瞻下标扫描，找不到等长闭合串时开启串按字面字符留在
  `[label]` 内继续找 `]`，对齐宿主 `` ``+(?=\]) `` 接受 `[**Spec** ``](url)`
  的行为；链接尾部 `\s*` 换 `isJSWhitespace`（含 `\r` 等 JS 空白）并按宿主
  正则回溯——整体失配时 `\s*` 自右让出空白、每个切分点按 destination 三候选
  （尖括号→裸 run→空断言）重试，让出的非 ASCII JS 空白可进裸 destination、
  让出的 ASCII 空白可作 title 分隔符，同一裸 run 内各起点共享 run 右端
  及其尾部判定、run 内最右 `)` 回退摊还复扫，`\s*` 跳过与 title 分隔符
  扫描按起点位置共享结束位置记忆（相邻 run 的右端落在同一空白后缀内时不
  再各自重扫公共后缀）保持线性；裸
  destination 贪婪吞 `)` 后按宿主 `findClosingBracket` 平衡截断/校验，非图片
  标签内经 `markedUnescapeBrackets` 反转义后仍含非图片行内链接时按
  `outputLink` 返回 0 让内层真链接先受保护——反转义物化为标签边界内的
  独立文本视图、以独立 `linkCache` 扫描，内层候选不越出标签 `]` 吃进外层
  destination、`\[`/`\]` 反转义后的新裸括号能被识别为内层链接起点；
  图片截断路径 `linkLen` 按 rune 覆盖完整标签（`labelStart` 恒指 `[`，
  `labelFrom` 统一 `+1`，多字节 label 不再错计）；（标签内 `![...]` 图片
  token 整体
  消费、不触发外层让渡，图片 label 内的内层链接归属图片作用域）；标签探针
  同步宿主 `nextInlineToken` 消费优先级——`$`/`\\(`/`\\[` 起始的 LaTeX token
  （`tokenizeInlineLatex`，含 pending 未闭合形态）与 `<` 起始的 HTML tag
  （`markedInlineTag` 六分支，注释/闭合/开标签/PI/声明/CDATA）先整体跳过，
  其内部形似链接的字节不计为内层链接；尖括号 destination 的 `\\.` 按宿主
  `translateMarkedPattern` 排除 `\n`/`\r`/U+2028/U+2029 四种行终止符；支持 `\`
  转义、`<...>` 尖括号 URL、含平衡圆括号 URL 及 `"..."`/`'...'`/`(...)`
  标题（标题内容严格排除各自未转义闭符，闭符后无法接上 `\s*\)` 时整条
  失配；内容内的反斜杠按宿主 `\\<闭符>?` 可选闭符语义先贪婪吞噬、整体
  失配时回溯为单反斜杠内容、把闭符让给标题收尾，该回溯经失败位置记忆化
  保持线性）；裸 destination 经 JSTrim 后以 `<` 开头时仅走宿主尖括号
  校验（与 `findClosingBracket` 平衡截断互斥），其余 destination 才做
  平衡截断；单行超过 `8192` 字节时按 display-only 安全护栏原样透传不做
  强调装饰（围栏开闭跟踪仍照常维护），8192 字节以内配合上述按行
  `linkCache` 位置记忆化将单行最坏耗时控制在毫秒级；全程零 `regexp2`
  依赖、纯标准库有界回退，合法复杂行内链接
  整体不再被注入 ANSI 破坏宿主 `markedInlineLink`。
- 纯 ASCII 大小写折叠：新增 `asciiLower`（`len` 字节等长），`splitLabel`/
  `withLabelIcon`/`actionLabel` 查表改用它，修复 `[[OK]]` 在 U+212A（3→1
  字节折叠）等输入下按折叠后长度切原串导致的 UTF-8 拦腰截断非法字节与尾部
  重影；`actionLabel` 回调 `icon == ""` 时原样返回，不再在 `**` 后插裸空格
  破坏粗体定界。
- `GetBgAnsi` 返回 `("\x1b[49m", nil)`/空/错误统一回退反显胶囊（`text`→
  `accent`），覆盖 `theme: "system"` 无面板背景场景。
- faint `dim`（`\x1b[2m` 结尾）不经 `Fg` 的合并 `\x1b[22;39m`，自拼拆分
  `\x1b[22m\x1b[39m` 并在 `inBold` 时 `reopen()`（`\x1b[1m` + `open`），
  不再打断外层粗体。
- `decorateProse` 单遍从左到右扫描（`proseScanRe`：粗体标记、`[[pill]]`、
  `✓✔`/`✗✘`/`→` 与 `[├└]──`）：同一 prose 段内 `**✓ Passed**`、`**a → b**`
  的符号在 `inBold` 已置位时着色，闭合码后正确补 `open` 恢复粗体前景色；
  faint `dim` 的树线连接符在 `inBold` 时补 `\x1b[1m` + `open` 同时恢复粗体
  字重（此前 marks/Tree 先于 bold 扫描替换，`inBold` 尚为空，粗体后半段丢
  失颜色/字重）。
- `PI_EMPHASIS_COLOR` 先 `GetFgAnsi` 校验再 `Fg`（防 panic），无效回退
  `"mdLink"`，仍不可用则本次转换原样返回。
- 状态 `sync.RWMutex` 化 + 主题指纹（`Name + ColorMode + 关键 token ANSI`）
  缓存重建 + `OnThemeChange` 置脏 + `""` 输入 O(1)；单一 `gen` 拆分为正交的
  `sessionGen` 与 `paletteGen`：`sessionGen` 仅在 `setThemeFunc`/`clearTheme`
  写锁内递增，`setUnsubTheme(sessionGen, unsub)` 按它判定会话过期——有效绑定
  期间的 `invalidate()`（主题推送由 SDK feed worker 与 `session_start` 并发
  投递）不再把当前订阅误判陈旧退订；`paletteGen` 在 `setThemeFunc`/
  `clearTheme`/`invalidate` 均递增，`paletteFor` 锁外构建后仅当
  `s.paletteGen == snapPaletteGen && s.themeFunc != nil` 才写回 `s.built`，
  并发主题切换不再回写陈旧调色板或残留订阅。
- `transform` 晚快照窗口根治：`paletteGen` 快照提前到与 `f := s.themeFunc`
  同一读锁内，`paletteFor(theme, snapPaletteGen)` 不再在自身读锁内重读
  `s.paletteGen`——在 `theme := f()`（旧主题）与 `paletteFor` 之间被
  `setThemeFunc`/`invalidate` 插入换绑并让另一 goroutine 缓存新调色板后，旧
  主题构建不再携带新代数覆写 `s.built`；新增 `onAfterThemeRead` 测试钩子
  （生产恒 nil）确定性复现该窗口。
- `session_start` 重复触发（startup/`/new`/resume）先解绑旧 `OnThemeChange`
  订阅再存新订阅，避免同一子进程累积回调；非 TUI 的 `session_start` 调用
  `clearTheme()` 清空 `themeFunc`/缓存调色板并解绑订阅，TUI→非 TUI 切换后
  不再残留旧主题或误注入 `reply_style`。
- `themeFingerprint` 拼入 `PI_EMPHASIS_COLOR` 与 `PI_EMPHASIS_ROUND_CAPS`，且
  自定义 `PI_EMPHASIS_COLOR` token 拼入其 `GetFgAnsi` 实际转义序列：同名主题
  仅自定义 token 前景色变化不再指纹碰撞，环境变量切换时缓存调色板确定性失效
  重建。
