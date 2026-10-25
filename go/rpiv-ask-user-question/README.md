# rpiv-ask-user-question — Go 移植（PiG 扩展）

`@juicesharp/rpiv-ask-user-question` **v2.15.0** 的完整 Go 移植，运行在 PiG 的
Go extension SDK（`github.com/MichaelKinsy/PiG/extensions/sdk`）之上。

原版是一个 Pi 扩展，注册 `ask_user_question` 工具：模型用它向用户提出结构化
选择题（1–4 题、每题 2–4 个选项），自动追加 `Type something.` 自定义输入行，
支持多选、`preview` 并排预览、笔记、问答回执与外部事件答题。

本目录是该扩展的**唯一权威源**：`go/rpiv-ask-user-question/`，独立嵌套 Go
module，通过 `go/go.work` 在本地解析 SDK，PiG 运行时由 staged SDK 注入
（**`go.mod` 中刻意不写 `replace`**）。

---

## 1. 加载形态

```go
// extension.go
func Extension() *sdk.Extension
```

PiG 以 **factory** 形式加载（不是 `package main`）。实测校验结果见 §7。

| 项 | 值 |
|---|---|
| form | `factory` |
| language | `go` |
| package | `github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question` |
| factory symbol | `Extension` |
| runtime | `subprocess` |
| 注册工具 | `ask_user_question` |
| 注册事件 handler | `before_agent_start`（工具动态调和） |

---

## 2. 文件映射（TS → Go）

原版 46 个 `.ts` 文件 + `locales/` 下 9 个 `.json`。全部功能逐文件对照移植；
Go 侧按原版目录结构平铺为单一 `package rpivaskuserquestion`（Go 不允许同包名
跨目录），文件头注释标注对应的 TS 源文件。

### 配置层

| TS 源 | Go 文件 | 覆盖内容 |
|---|---|---|
| `config.ts` | `config.go` | `resolveCollapseKey` / `resolveNotesKey` / `formatKeySpecForDisplay` / `formatNotesKeyForDisplay` / `loadConfig` |
| `@juicesharp/rpiv-config/config.ts` | `config.go` | `loadJsonConfigWithLegacyFallback`、`validateGuidanceFields`、`configPath` XDG 解析、`readEnvVar`、`expandTilde` |
| `state/i18n-bridge.ts` | `i18n.go` | `t(key, fallback)`、`displayLabel(kind)`、9 语言 embed 与回退 |

配置查找顺序（隔离优先，且仅在设置了隔离变量时启用）：

1. `$PIG_CODING_AGENT_DIR/extensions/rpiv-ask-user-question/config.json`
2. `$PIG_HOME/.config/rpiv-ask-user-question/config.json`
3. `$PIG_HOME/config/rpiv-ask-user-question/config.json`
4. `$XDG_CONFIG_HOME/rpiv-ask-user-question/config.json`（原版行为）
5. `~/.config/rpiv-ask-user-question/config.json`（原版 legacy 回退）

原版语义完整保留：**XDG 文件存在但损坏时报错并返回 `{}`，绝不静默回退 legacy**。

两点必须说清的口径：

- **legacy 路径就是 `~/.config/<name>/config.json`**，不是
  `~/.pi/agent/extensions/<name>/config.json`。`@juicesharp/rpiv-config` 的
  `legacyConfigPath` 原文是 `join(defaultConfigDir(), name, file)`，而
  `defaultConfigDir()` 即 `join(homedir(), ".config")`；该包明确把 Pi 原生路径
  （`~/.pi/…`）划成正交的另一层，不在此统一。原版的完整查找序只有 4→5 两级。
- 1–3 是 **PiG 侧有意加入的额外超集**：仅当 `PIG_CODING_AGENT_DIR` / `PIG_HOME`
  非空时才成为候选，两者都没设时顺序退化为原版的 4→5。损坏文件同样**不回退**。

`LoadConfig()` 的调用时机与原版一致：`guidance` 只在**工具注册时**读一次
（`registerAskUserQuestionTool`），`collapseKey` / `notesKey` 在**每次执行走 TUI 路径时**
实时读（`runTUIPath`），改配置文件后下一次问答即生效，无需重载。

### 工具契约层

| TS 源 | Go 文件 |
|---|---|
| `tool/types.ts` | `types.go`（schema、`QuestionParams`、`QuestionAnswer`、`QuestionnaireResult`、错误码） |
| `tool/normalize-params.ts` | `normalize-params.go`（`\r\n`→`\n`、游离 `\r`→删除、double-encoded JSON 修复 #244） |
| `tool/validate-questionnaire.ts` | `validate-questionnaire.go`（题数/选项数、重复题、保留标签、重复标签；守卫集与原版相同——保留/重复标签是**大小写敏感的精确比较**，`multiSelect` 带 `preview` **不拦截**） |
| `tool/format-answer.ts` | `format-answer.go`（`NO_INPUT_PLACEHOLDER = "(no input)"`） |
| `tool/response-envelope.ts` | `response-envelope.go`（cancelled / 单题 / 多题 / unanswered / globalNote） |
| `tool/external-answer.ts` | `external-answer.go`（严格校验、错误码与 reason 逐字对齐；`questionIndex` / `optionIndexes` 必须是**真整数**——小数、`NaN`、`±Inf` 一律按 `invalid_payload` 拒绝） |

### 事件与答题通道

| TS 源 | Go 文件 |
|---|---|
| `events.ts` | `events.go`（4 个 `rpiv:ask-user:*` 频道常量与 payload 类型） |
| `answer-channel.ts` | `answer-channel.go`（`missing_tool_call_id` / `unknown_tool_call` / `rpc_host_unsupported` / `already_settled`、first-answer-wins） |

### 状态机层

| TS 源 | Go 文件 |
|---|---|
| `state/row-intent.ts` | `row-intent.go`（`ROW_INTENT_META`、`sentinelsToAppend`） |
| `state/state.ts` | `state.go`（`QuestionnaireState` / `QuestionnaireRuntime`） |
| `state/state-reducer.ts` | `state-reducer.go`（16 个 action 的纯 reducer + `Effect` 闭集） |
| `state/key-router.ts` | `key-router.go`（完整分层级联） |
| `state/keys.ts`（pi-tui） | `keys.go`（委托 SDK 的 `MatchesKey` / `IsKeyRelease` / `IsKeyRepeat`） |
| `state/selectors/*.ts`（4 个） | `selectors.go` + `selectors-projections.go` |
| `state/build-questionnaire.ts` | `questionnaire-session.go`（装配部分） |
| `state/questionnaire-session.ts` | `questionnaire-session.go` |
| `state/external-editor.ts` | `external-editor.go` |

按键路由级联顺序与原版一致：

```
collapseKey（顶层拦截，任意内部态可用）
→ collapsed 锁定（仅 Esc 放行）
→ notes 模式
→ inputMode 模式
→ Submit tab
→ tab 切换（Tab/→、Shift+Tab/←）
→ notesKey
→ ↑/↓ 导航
→ multi-select / single-select 分支
```

### 视图层

| TS 源 | Go 文件 |
|---|---|
| `presentation.ts` | `presentation.go`（内联槽位互斥、`INLINE_ROW_RATIO=0.7`、`MIN_BUDGETED_ROWS=12`、`YIELD_TO_OVERLAY`） |
| `view/scroll-window.ts` | `scroll-window.go`（居中滚动窗口、`↑`/`↓`/`↕`、保护行不被覆盖） |
| `view/stateful-view.ts` | `scroll-window.go`（`StatefulView[P]` 泛型契约） |
| `view/component-binding.ts` | `props-adapter.go` |
| `view/components/wrapping-select.ts` | `wrapping-select.go` |
| `view/components/option-list-view.ts` | `option-list-view.go` |
| `view/components/multi-select-view.ts` | `multi-select-view.go` |
| `view/components/inline-input.ts` | `inline-input.go`（grapheme 感知光标、反显、NBSP） |
| `view/components/submit-picker.ts` | `option-list-view.go` |
| `view/components/tab-bar.ts` | `option-list-view.go` |
| `view/components/preview/markdown-content-cache.ts` | `markdown-content-cache.go` |
| `view/components/preview/markdown-renderer.ts`（pi-tui `Markdown`） | `markdown-renderer.go` |
| `view/components/preview/preview-block-renderer.ts` | `preview-block-renderer.go` |
| `view/components/preview/preview-box-renderer.ts` | `preview-box-renderer.go`（圆角边框、`✂ ── N lines hidden ──`） |
| `view/components/preview/preview-layout-decider.ts` | `preview-layout-decider.go`（side-by-side vs stacked、slack donation） |
| `view/components/preview/preview-pane.ts` | `preview-pane.go`（行配额自适应） |
| `view/dialog-builder.ts` | `dialog-builder.go`（三段式滚动分区、heading 固定、残余 spacer） |
| `view/dialog-copy.ts` | `dialog-copy.go` |
| `view/props-adapter.ts` | `props-adapter.go`（全局 + per-tab 双注册表扇出） |
| `view/tab-components.ts` | `tab-components.go` |
| `view/tab-content-strategy.ts` | `tab-content-strategy.go`（`QuestionTabStrategy` / `SubmitTabStrategy`、`buildHintText`） |
| `view/transcript.ts` | `transcript.go`（`renderAskCall` / `renderAskResult`） |
| （无直接对应） | `text-editor.go`（替代 pi-tui 无头 `Editor`） |
| `locales/*.json`（9 个） | `locales/*.json`（`//go:embed`） |
| `index.ts` | `extension.go`（工具注册 + reconciler 接线） |
| `ask-user-question.ts` | `extension.go` |
| `reconcile.ts` | `extension.go`（`ReconcileAskUserQuestionTool`） |
| `rpc-fallback.ts` | `rpc-fallback.go` |

`locales/` 与 npm 原版**逐字节一致**（md5 已核对：`en.json` = `532140312b50b172d163d29a1eace3ac`）。

---

## 3. 与 SDK 的适配点

TypeScript 原版直接操作 pi-tui 的活对象；PiG 的 subprocess SDK 只能传可序列化
数据。五个关键适配（均已在代码注释中标注）：

1. **`ctx.ui.custom()` 组件** → `QuestionnaireSession` 实现
   `sdk.RemoteComponent`（`Render(width)` / `HandleInput(data)`），并通过
   `SetInvalidate` / `Dispose` 接入重绘与销毁。
2. **`OverlayHandle`** → `RemoteOverlayOptions.OnHandle`；折叠时
   `Handle.SetHidden`，外部答题到达时 `Handle.Hide` 关闭浮层。
3. **`ctx.ui.onTerminalInput`** → `Context.OnTerminalInput`，在浮层隐藏时监听
   `collapseKey` 重新展开；Kitty 协议 release/repeat 被过滤。注册时机与原版同序：
   **先注册监听、后建会话**，`canReopenWhileHidden` 只在监听器真的拿到（`err==nil &&
   unsub!=nil`）时为 `true`；注册不上就退回可见的单行折叠提示，绝不把浮层藏死。
   首次真的藏起来时补一条 `ask_user_question hidden — press <key> to reopen` 的
   info 提示（每次挂载最多一条）。
4. **`ctx.ui.notify`** → `Context.Notify`（外部编辑器失败、隐藏提示）。从外部编辑器
   交还终端后的重绘走会话的 `invalidate`，**不发通知**（原版是
   `tui.requestRender(true)`，不是 notify）。
5. **按键解析** → 委托 SDK 的 `tui.MatchesKey`（KeyId 语法与 pi-tui 同源），
   `view/*` 的 pi-tui `Markdown` → 本包 `markdown-renderer.go`。

`presentation.MustYieldInlineSlot` 在原版用 `tui.getTopmostVisibleOverlay()`；
PiG 无对应访问器，因此改为**可注入探针**（`MustYieldInlineSlot(probe)`），无
探针时不 yield——这样内联挂载永远不会错误地抢占浮层。

---

## 4. 关键行为契约（与原版逐字一致）

- 保留标签校验**始终锁定英文** `ROW_INTENT_META[kind].label`，即使用户切到中文
  也不会把「输入内容」当作保留标签。
- `NO_INPUT_PLACEHOLDER = "(no input)"`，`DECLINE_MESSAGE`、`ENVELOPE_PREFIX`、
  `ENVELOPE_SUFFIX`、所有校验错误文案逐字对齐。
- `(Recommended)` 后缀按 `/\s*\(Recommended\)$/` 语义剥离（吞噬前置空白，
  纯后缀剥成空串）。
- `multiSelect` 选择标签按**作者顺序**输出，不用 emitter 给的顺序。
- 笔记存储于 `notesByTab` 侧带，`questions.length` 为全局笔记伪索引；
  空白草稿会被删除，而不是存空串。
- 部分提交时未答题在 `unanswered:` 段落与 `details.unanswered` 中列出，
  全答完的结果保持字节不变（`unanswered` 为 `nil`）。
- `multiSelect: true` 且选项带 `preview` **不是运行时错误**：原版校验器与 `execute`
  都没有这条拦截，Go 侧同样只在下游自然忽略——`PreviewPane` 对多选题渲染纯选项列
  表（无预览框、无备注 affordance），reducer 只给单选选项答案挂 `preview`。
- 折叠态渲染是**一行 dim 着色**的提示（`theme.fg("dim", " <hint> ")`），不是裸文本。

---

## 5. 与 npm 原版的三处**有意差异**

1. **`locales/` 是部分字典**：原版除 `en.json`（30 键）外，其余 8 个语言各
   25 键左右，缺 `review.global_hint` / `review.note_label` /
   `notes.global_header` / `hint.collapse` / `hint.expand_line`
   （`zh.json` 有 27 键）。Go 侧**原样复制**，运行时靠 `T(key, fallback)`
   回退英文——与 npm 原版未装 `@juicesharp/rpiv-i18n` 时的行为一致。
2. **locale 来源**：原版由 rpiv-i18n SDK 提供；Go 侧内置
   `LocaleSource` 钩子读 `RPIV_I18N_LOCALE` / `PIG_LOCALE` / `LANG` /
   `LC_ALL` / `LC_MESSAGES`，并支持 POSIX 形式（`de_DE.UTF-8` → `de`）。
3. **`_meta.notes` 注释键**：原版 locale 文件带有翻译备注键，Go 侧加载时
   保留但不参与键集合校验。

---

## 6. 测试覆盖

**338 个测试函数，86.3% 语句覆盖**，`-race` 全绿（DATA RACE=0）。

| 测试文件 | 覆盖对象 |
|---|---|
| `normalize-params_test.go` | 行终止符、double-encoded JSON、schema 边界与描述文案 |
| `validate-questionnaire_test.go` | 全部校验分支与**判定顺序**（保留标签先于重复标签）、本地化不影响保留集、`multiSelect` 带 `preview` 通过校验（原版无此拦截） |
| `response-envelope_test.go` | 信封全部形态：cancelled、单题、多题、部分提交、全局笔记、字节不变性 |
| `external-answer_test.go` | 每个 reject code 与 reason 逐字断言、乱序去重、笔记 trim |
| `answer-channel_test.go` | 事件总线交互、每种拒绝码、**first-answer-wins**、并发抢答（16 goroutine）、并发 open/close |
| `config_test.go` | key spec 语法、notesKey 冲突、guidance 校验、XDG/legacy/隔离路径、损坏文件不回退 |
| `i18n_test.go` | 9 语言嵌入加载、部分字典校验、`{key}` 占位符保真、fallback、POSIX locale 归一化 |
| `state-reducer_test.go` | 16 个 action 全分支、全局笔记上提、答案顺序、不变量（不改输入） |
| `key-router_test.go` | 完整级联：折叠拦截/锁定、notes、inputMode、Submit tab、tab 切换、数字直选、多选翻转 |
| `text-editor_test.go` | 光标移动、删除、Ctrl+W/U/K、换行、bracketed paste、grapheme 聚类 |
| `preview_test.go` | 框渲染/隐藏提示符、布局阈值、slack donation、markdown、宽键缓存 |
| `components_test.go` | `WrappingSelect` 编号/滚动/确认标记、多选视图、TabBar、SubmitPicker、滚动窗口、presentation |
| `selectors_test.go` | 全部 selector 投影与边界 |
| `transcript_test.go` | 调用/结果渲染、`(Recommended)` 剥离、预览竖线、笔记、表格对齐 |
| `rpc-fallback_test.go` | 单选/多选/自定义/取消、数字与标签回复、预览折叠、reconcile |
| `questionnaire-session_test.go` | 端到端会话：确认/取消/自定义/笔记/全局笔记/折叠（含**折叠行 dim 着色**、隐藏门控）/外部编辑器/多选（含**忽略 preview**）/预览布局（含**预览面板跟随当前 Tab**） |
| `extension_test.go` | `Extension()` 构建、guidance 覆盖、外部编辑器、**编辑器交还后的重绘走 invalidate 而非 notify**、**折叠监听门控与隐藏提示文案**、key 匹配委托 |

测试隔离：`main_test.go` 用 `TestMain` 固定 `LocaleSource` 为空，使整包在
**无 locale（纯英文 fallback）** 下运行——不受构建机 `LANG=zh_CN.UTF-8` 影响；
需要语言的用例用 `withLocale(t, "zh")` 显式切换。

---

## 7. 实测结果

### 7.1 本目录质量门

```
$ gofmt -l .
（无输出）

$ go build ./...
（EXIT=0）

$ go vet ./...
（EXIT=0）

$ go test -count=1 -race -v ./...
ok  github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question
（338 个顶层测试全 PASS，DATA RACE=0，FAIL=0）

$ go test -count=1 -cover .
ok  ...  coverage: 86.3% of statements
```

上一轮记录的 335 个 / 86.8% 已按本轮实测重写（新增 7 个测试、删除 3 个偏离原版的
测试，净 +4）。

### 7.2 PiG 实际编译与加载

隔离目录 `/root/.pig-test/agent`（通过 `go/bin/pig-test` 包装脚本，强制
`PIG_HOME=/root/.pig-test` 并清除可绕过变量）。

```
$ ./bin/pig-test -e ./rpiv-ask-user-question --help
...（正常帮助输出）...
EXIT=0
```

结构化校验（证明 factory 被真正编译、解析并注册）：

> `hash` 是本次运行的证据：它覆盖包目录内容（**含本 README**），所以改本文档也会
> 改变它——不要把它当自指校验和。

```
$ ./bin/pig-test install ./rpiv-ask-user-question --validate-only --json
{
  "valid": true,
  "name": "rpiv-ask-user-question",
  "source": "/root/workspace/.wt/pi-extensions/feature-go-rpiv-ask/go",
  "hash": "d53c4d110fcecfe87aba411689ee7cb5ab95b6c1895146162d555e331c09b898",
  "tools": ["ask_user_question"],
  "handlers": ["before_agent_start"],
  "toolDetails": [{
    "name": "ask_user_question",
    "label": "Ask User Question",
    "description": "Ask the user one or more structured questions during execution. ...",
    "promptSnippet": "Ask the user up to 4 structured questions (2-4 options each) when requirements are ambiguous",
    "promptGuidelines": [ ... 4 条 ... ]
  }],
  "registered": true,
  "definition": {
    "form": "factory",
    "language": "go",
    "root": "/root/workspace/.wt/pi-extensions/feature-go-rpiv-ask/go",
    "package": "github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question",
    "factory": "Extension",
    "packable": true,
    "runtime": "subprocess",
    "sdk": "github.com/MichaelKinsy/PiG/extensions/sdk",
    "isolation": "shared-ok"
  }
}
EXIT=0
```

### 7.3 规模

| 项 | 数值（本轮实测） |
|---|---|
| 生产代码 | 9,370 行（40 个 `.go`） |
| 测试代码 | 8,007 行（19 个 `_test.go`） |
| 嵌入 locale | 9 个 JSON / 329 行 |
| 合计 | 17,706 行（含测试与 locale） |

---

## 8. 本地命令

```bash
cd go/rpiv-ask-user-question
gofmt -l .
go build ./...
go vet ./...
go test -count=1 -race -v ./...
go test -count=1 -cover .

# 隔离加载验证（从 go/ 目录）
cd ..
./bin/pig-test -e ./rpiv-ask-user-question --help
./bin/pig-test install ./rpiv-ask-user-question --validate-only --json
```

---

## 9. 未移植项

无可删减：原版 46 个 TS 模块的功能均已落到 Go 侧。唯一未一一对应的是
pi-tui 的 `Editor` 组件——PiG 没有可跨 subprocess 的编辑器对象，因此写了一个
等价的无头多行缓冲（`text-editor.go`），实现了问卷实际依赖的全部编辑语义
（光标、词跳、行首尾、Backspace/Delete、Ctrl+W/U/K、换行、bracketed paste、
grapheme 感知）。
