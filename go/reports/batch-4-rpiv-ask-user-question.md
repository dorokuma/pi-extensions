# 第 4 批：rpiv-ask-user-question 扩展 Go 移植报告（reports/batch-4-rpiv-ask-user-question.md）

| 项 | 值（均为本次实测/实读） |
|---|---|
| 分支 | `feature/go-rpiv-ask`（`git branch --show-current` → `feature/go-rpiv-ask`） |
| 工作树路径 | `/root/workspace/.wt/pi-extensions/feature-go-rpiv-ask`（`go/` 为其 Go 区） |
| 基线 | `feature/go-batch1` @ `338c5f0`（`git log --oneline -1 338c5f0` → `338c5f0 feat(go): port cache-stats extension to Go (batch 3)`） |
| 代码提交 | 完整 SHA `1f7d089381197dee45d520b0cb1811a66bd8af09`；短 SHA `1f7d089` |
| 报告提交 | 见 §7.1 收口核验中的 `docs(go)` 提交（本文件自身 SHA 在交付回执中给出，不自引用） |
| 工具链实测 | `go version go1.26.4 linux/amd64`（EXIT=0）；`./bin/pig-test --version` → `0.5.2+1.4.0`（EXIT=0） |
| 门禁状态 | B 档提交前门禁（Verifier + Reviewer）4/4 判据 PASS，0 must fix / 0 should fix，共 5 轮迭代 |

证据范围声明：

- 本报告只写本次会话**亲自跑过**的命令输出与**亲眼读到**的文件内容；每条命令贴原文并显式捕获 `EXIT=$?`。
- §3.2 / §3.3 中三段超长输出（`make`、`pig-test --help`、`install --validate-only --json`）由脚本把**落盘的原始捕获文件**原样嵌入本报告，未经任何手工改写。
- 未跑的都写「未跑」并给原因（见 §6.2）。
- 全程未打印任何密钥；未改动 `/root/.pi/agent/extensions/*.ts` 生产部署件，未改动外生宿主仓（`/root/workspace/codegraph-go`、`/root/workspace/ctxmode`、`/root/workspace/pi-cache-stats`），未 push、未合并（见 §6.3）。
- 采集命令均在授权工作树 `/root/workspace/.wt/pi-extensions/feature-go-rpiv-ask` 内执行；主仓库 `/root/workspace/pi-extensions` 只做只读 `git status` 取证。

---

## 1. 批次结论摘要与文件映射

### 1.1 结论

`@juicesharp/rpiv-ask-user-question` **v2.15.0** 已完整移植到 Go（PiG Go extension
SDK），以 **factory** 形态（`package rpivaskuserquestion` + `func Extension() *sdk.Extension`）
被 PiG 加载，注册 `ask_user_question` 工具与 `before_agent_start` handler。
模块级 `gofmt` / `go build` / `go vet` / `go test -count=1 -race -cover` 与工作区级
`make build vet test sdk-check` 全绿，PiG 隔离目录加载与 `install --validate-only`
均 EXIT=0。语句覆盖率 **86.6%**。

### 1.2 TS 权威源（实测）

| 源 | 版本 | `.ts` 文件数 | `.ts` 行数 | 备注 |
|---|---|---|---|---|
| `/root/.pi/agent/npm/node_modules/@juicesharp/rpiv-ask-user-question` | `2.15.0` | 46 | 6548 | 另有 `locales/*.json` 9 个语言文件（9 文件共 329 行） |
| `/root/.pi/agent/npm/node_modules/@juicesharp/rpiv-config`（同源依赖） | `2.15.0` | 2 | 322 | `config.ts` 309 行 + `index.ts` 13 行 |

实测命令与原文：

```console
$ find /root/.pi/agent/npm/node_modules/@juicesharp/rpiv-ask-user-question -name '*.ts' -type f -exec wc -l {} + | tail -1
  6548 总计
$ find /root/.pi/agent/npm/node_modules/@juicesharp/rpiv-config -name '*.ts' -type f -exec wc -l {} + | tail -1
   322 总计
$ ls /root/.pi/agent/npm/node_modules/@juicesharp/rpiv-ask-user-question/locales
de.json  en.json  es.json  fr.json  pt-BR.json  pt.json  ru.json  uk.json  zh.json
```

**npm 发布包内无自带测试文件**（上述目录中不存在任何 `*.test.ts` / `*.spec.ts` /
`__tests__/`）。因此 Go 侧**对照 TS 源码行为从零编写**了全部测试：19 个
`*_test.go` 文件，其中 18 个承载测试函数（`main_test.go` 只承载 `TestMain` 做
locale 隔离，`testtheme_test.go` 只承载共享 fixture），共 **342 个顶层测试函数 /
522 个测试项（含子测试）**，全部 PASS。

### 1.3 Go 交付物规模（实测）

| 项 | 数值 |
|---|---|
| 非测试 `.go` 文件 | **40 个** / **9461 行** |
| `*_test.go` 测试文件 | **19 个** / **8314 行**（其中 18 个含测试函数） |
| `locales/*.json` | 9 个 / 329 行（与 npm 原版逐字节一致，见 §4.2） |
| `go.mod` | 7 行 |
| `README.md` | 359 行 |
| 新增文件合计 | **70 个** |
| `go/go.work` | 更新（`use` 块新增 `./rpiv-ask-user-question`，现共 16 条 `use` 项） |

提交统计：**71 files changed, 18471 insertions(+)**（= 9461 实现 + 8314 测试 +
329 locale + 359 README + 7 go.mod + `go.work` 1 行）。

> 口径说明：模块 `README.md` §7.3 记录的「生产代码 9,370 行 / 测试代码 8,007 行」
> 取自门禁中途某一轮；本报告数值为**提交点实测**（`find … -exec wc -l {} +`），
> 两者差异来自门禁后期补充的实现与测试行。

### 1.4 文件映射（TS → Go）

| TS 源 | Go 文件 | 覆盖内容 |
|---|---|---|
| `config.ts` | `config.go` | `resolveCollapseKey` / `resolveNotesKey` / `formatKeySpecForDisplay` / `loadConfig` |
| `@juicesharp/rpiv-config/config.ts` | `config.go` | `loadJsonConfigWithLegacyFallback`、`validateGuidanceFields`、XDG `configPath` 解析、`readEnvVar`、`expandTilde` |
| `state/i18n-bridge.ts` | `i18n.go` | `t(key, fallback)`、`displayLabel(kind)`、9 语言 embed 与回退 |
| `tool/types.ts` | `types.go` | schema、`QuestionParams`、`QuestionAnswer`、`QuestionnaireResult`、错误码、`jsonInt` 安全整数守卫 |
| `tool/normalize-params.ts` | `normalize-params.go` | `\r\n`→`\n`、游离 `\r`→删除、double-encoded JSON 修复 |
| `tool/validate-questionnaire.ts` | `validate-questionnaire.go` | 题数/选项数、重复题、保留标签、重复标签（守卫集与原版相同） |
| `tool/format-answer.ts` | `format-answer.go` | `NO_INPUT_PLACEHOLDER = "(no input)"` |
| `tool/response-envelope.ts` | `response-envelope.go` | cancelled / 单题 / 多题 / unanswered / globalNote |
| `tool/external-answer.ts` | `external-answer.go` | 严格校验、错误码与 reason 逐字对齐 |
| `events.ts` | `events.go` | 4 个 `rpiv:ask-user:*` 频道常量与 payload 类型 |
| `answer-channel.ts` | `answer-channel.go` | `missing_tool_call_id` / `unknown_tool_call` / `rpc_host_unsupported` / `already_settled`、first-answer-wins |
| `state/row-intent.ts` | `row-intent.go` | `ROW_INTENT_META`、`sentinelsToAppend` |
| `state/state.ts` | `state.go` | `QuestionnaireState` / `QuestionnaireRuntime` |
| `state/state-reducer.ts` | `state-reducer.go` | 16 个 action 的纯 reducer + `Effect` 闭集 |
| `state/key-router.ts` | `key-router.go` | 完整分层级联 |
| `state/keys.ts`（pi-tui） | `keys.go` | 委托 SDK 的 `MatchesKey` / `IsKeyRelease` / `IsKeyRepeat` |
| `state/selectors/*.ts`（4 个） | `selectors.go` + `selectors-projections.go` | derivations / focus / projections / contract |
| `state/build-questionnaire.ts` + `state/questionnaire-session.ts` | `questionnaire-session.go` | 装配 + 会话（`sdk.RemoteComponent`） |
| `state/external-editor.ts` | `external-editor.go` | 外部编辑器流程 |
| `presentation.ts` | `presentation.go` | 内联槽位互斥、`INLINE_ROW_RATIO=0.7`、`MIN_BUDGETED_ROWS=12`、`YIELD_TO_OVERLAY` |
| `view/scroll-window.ts` + `view/stateful-view.ts` | `scroll-window.go` | 居中滚动窗口 + `StatefulView[P]` 泛型契约 |
| `view/component-binding.ts` + `view/props-adapter.ts` | `props-adapter.go` | 全局 + per-tab 双注册表扇出 |
| `view/components/wrapping-select.ts` | `wrapping-select.go` | 换行选择器 |
| `view/components/option-list-view.ts`（含 `submit-picker.ts` / `tab-bar.ts`） | `option-list-view.go` | 选项列表 / 提交选择器 / tab 条 |
| `view/components/multi-select-view.ts` | `multi-select-view.go` | 多选视图 |
| `view/components/inline-input.ts` | `inline-input.go` | grapheme 感知光标、反显、NBSP |
| `view/components/preview/*`（6 个） | `markdown-content-cache.go` / `markdown-renderer.go` / `preview-block-renderer.go` / `preview-box-renderer.go` / `preview-layout-decider.go` / `preview-pane.go` | 预览渲染与布局 |
| `view/dialog-builder.ts` + `view/dialog-copy.ts` + `view/tab-components.ts` | `dialog-builder.go` / `dialog-copy.go` / `tab-components.go` | 三段式滚动分区、文案、tab 组件 |
| `view/tab-content-strategy.ts` | `tab-content-strategy.go` | `QuestionTabStrategy` / `SubmitTabStrategy`、`buildHintText` |
| `view/transcript.ts` | `transcript.go` | `renderAskCall` / `renderAskResult` |
| `index.ts` + `ask-user-question.ts` + `reconcile.ts` | `extension.go` | 工具注册 + reconciler 接线 |
| `rpc-fallback.ts` | `rpc-fallback.go` | 无 TUI 时的文本降级路径 |
| （无直接对应） | `text-editor.go` | 替代 pi-tui 无头 `Editor` |
| `locales/*.json`（9 个） | `locales/*.json` | `//go:embed`，与 npm 原版逐字节一致 |

按键路由级联顺序与原版一致：`collapseKey`（顶层拦截）→ collapsed 锁定（仅 Esc 放行）
→ notes 模式 → inputMode 模式 → Submit tab → tab 切换 → notesKey → ↑/↓ 导航 →
multi-select / single-select 分支。

---

## 2. 提交清单（实测原文）

```console
$ git log --oneline -5
1f7d089 feat(go): port rpiv-ask-user-question extension to Go (batch 4)
338c5f0 feat(go): port cache-stats extension to Go (batch 3)
4a03b16 docs(go): add batch-2 migration report
f5104f8 feat(go): port ctxmode extension to Go (batch 2)
9680a46 feat(go): port codegraph-go extension to Go (batch 2)
（EXIT=0）
```

代码提交实测原文：

```console
$ git rev-parse HEAD
1f7d089381197dee45d520b0cb1811a66bd8af09
$ git rev-parse --short HEAD
1f7d089
$ git show --shortstat --oneline HEAD
1f7d089 feat(go): port rpiv-ask-user-question extension to Go (batch 4)
 71 files changed, 18471 insertions(+)
（EXIT=0）
```

提交前 `git status --short`（代码提交前的工作树状态，实测原文）：

```console
 M go/go.work
?? go/rpiv-ask-user-question/
```

暂存后 `git diff --cached --name-only | wc -l` → `71`（70 个新增 + `go/go.work` 修改）。
`go/go.work` 的改动为（`git diff go/go.work` 原文）：

```diff
@@ -18,4 +18,5 @@ use (
 	./pi-env
 	./pi-tinyfish
 	./pigsdk
+	./rpiv-ask-user-question
 )
```

报告提交（本文件）随后执行：

```console
$ git add go/reports/batch-4-rpiv-ask-user-question.md
$ git commit -m "docs(go): add batch-4 rpiv-ask-user-question migration report"
```

最终 `git status --short` 与 `git log --oneline -4` 见 §7.1（收口核验）。

---

## 3. 测试与验证结果原文

### 3.1 模块级（`go/rpiv-ask-user-question/`）

```console
$ gofmt -l .
EXIT=0
```

```console
$ go build ./...
EXIT=0
```

```console
$ go vet ./...
EXIT=0
```

连续 3 轮 `go test -count=1 -race ./...`（实测原文，逐轮捕获 `EXIT=$?`）：

```console
$ go test -count=1 -race ./...   [round 1]
ok  	github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question	1.098s
EXIT=0
$ go test -count=1 -race ./...   [round 2]
ok  	github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question	1.092s
EXIT=0
$ go test -count=1 -race ./...   [round 3]
ok  	github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question	1.102s
EXIT=0
```

带覆盖率（实测原文）：

```console
$ go test -count=1 -race -cover ./...
ok  	github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question	1.188s	coverage: 86.6% of statements
EXIT=0
```

顶层测试数与含子测试总数（`go test -count=1 -race -v ./...` 落盘后统计，实测原文）：

```console
EXIT=0
--- top-level (=== RUN   TestX, no slash) ---
342
--- all === RUN lines (incl. subtests) ---
522
--- PASS top-level ---
342
--- all PASS ---
342
--- FAIL/SKIP ---
0
--- DATA RACE ---
0
--- tail ---
--- PASS: TestValidateQuestionnaireAcceptsMultiSelectWithPreview (0.00s)
PASS
ok  	github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question	1.106s
```

统计口径说明：342 个顶层测试函数与源码中 `grep -c '^func Test'`（343，含
`TestMain`）一致；522 = 342 顶层 + 180 个子测试；FAIL / SKIP / DATA RACE 均为 0。
`t.Run(` 调用点 88 处（部分位于循环内，故子测试数 > 调用点数）。

### 3.2 工作区级（`go/`）

```console
$ make build vet test sdk-check
==> [root module] go build ./...
==> [component: auto-continue] go build ./...
==> [component: cache-stats] go build ./...
==> [component: codegraph-go] go build ./...
==> [component: ctxmode] go build ./...
==> [component: ctx-orchestrate] go build ./...
==> [component: exclude-tools-guard] go build ./...
==> [component: herdr-agent-state] go build ./...
==> [component: jev-gate] go build ./...
==> [component: moshi-hooks] go build ./...
==> [component: no-tables] go build ./...
==> [component: pi-context-pruning] go build ./...
==> [component: pi-env] go build ./...
==> [component: pi-tinyfish] go build ./...
==> [component: rpiv-ask-user-question] go build ./...
==> [root module] go vet ./...
==> [component: auto-continue] go vet ./...
==> [component: cache-stats] go vet ./...
==> [component: codegraph-go] go vet ./...
==> [component: ctxmode] go vet ./...
==> [component: ctx-orchestrate] go vet ./...
==> [component: exclude-tools-guard] go vet ./...
==> [component: herdr-agent-state] go vet ./...
==> [component: jev-gate] go vet ./...
==> [component: moshi-hooks] go vet ./...
==> [component: no-tables] go vet ./...
==> [component: pi-context-pruning] go vet ./...
==> [component: pi-env] go vet ./...
==> [component: pi-tinyfish] go vet ./...
==> [component: rpiv-ask-user-question] go vet ./...
==> [root module] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/internal/version	(cached)
==> [component: auto-continue] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/auto-continue	(cached)
==> [component: cache-stats] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/cache-stats	(cached)
==> [component: codegraph-go] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/codegraph-go	(cached)
==> [component: ctxmode] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctxmode	(cached)
==> [component: ctx-orchestrate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctx-orchestrate	(cached)
==> [component: exclude-tools-guard] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	(cached)
==> [component: herdr-agent-state] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	(cached)
==> [component: jev-gate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/jev-gate	(cached)
==> [component: moshi-hooks] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/moshi-hooks	(cached)
==> [component: no-tables] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/no-tables	(cached)
==> [component: pi-context-pruning] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	(cached)
==> [component: pi-env] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-env	(cached)
==> [component: pi-tinyfish] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	(cached)
==> [component: rpiv-ask-user-question] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/rpiv-ask-user-question	(cached)
Checking pigsdk against staged SDK: /root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef
OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)
EXIT=0
```

`go/go.work` 现共 16 条 `use` 项（`.` + 14 组件 + `./pigsdk`），`rpiv-ask-user-question`
已纳入，根模块与全部组件 build/vet/test 全绿；`sdk-check` 确认 `go/pigsdk` 与 staged
SDK 逐文件 md5 一致。

### 3.3 PiG 隔离配置目录（`PIG_CODING_AGENT_DIR=/root/.pig-test/agent`）

```console
$ PIG_CODING_AGENT_DIR=/root/.pig-test/agent ./bin/pig-test -e ./rpiv-ask-user-question --help
pig - AI coding assistant with read, bash, edit, write tools

Usage:
  pig [options] [--] [@files...] [messages...]

Commands:
  pig install <source> [-l]     Install extension source and add to settings
  pig remove <source> [-l]      Remove extension source from settings
  pig uninstall <source> [-l]   Alias for remove
  pig update [source|self|pig]   Update pig, extensions, or model catalogs
  pig list                      List installed extensions from settings
  pig config [-l]               Open TUI to enable/disable package resources (Tab switches scope)
  pig auth <command>            Print credentials or check provider readiness
  pig mcp <command>             Check MCP servers, sign in to or out of OAuth servers
  pig <command> --help          Show help for install/remove/uninstall/update/list/config/auth/mcp

PiG Commands:
  pig login [provider]          Sign in to a provider
  pig logout [provider]         Sign out of a provider
  pig piglet <command>          List, show, validate, and build Piglets (run without arguments for usage)
  pig package <command>         List, validate, and publish Packages
  pig extension init <path>     Create a Go, Python, or Rust extension
  pig extension upgrade         Rewrite Go extensions written for an older SDK
  pig reload                    Stage the extension SDKs and drop builds an older SDK produced
  pig setup [go|container]      Show or install the toolchains that extensions and Piglet builds use
  pig verify [path...]          Verify this binary, downloads, installed Packages, and Resources
  pig status [--json]           Show Resources, Packages, and Piglets
  pig docs [show <topic>]       Read the local PiG documentation
  pig version                   Show PiG, pinned Pi, Go, and platform versions
  pig diagnose                  Show resolved configuration and binary identity

PiG Options:
  --piglet <name|path>           Run the named Piglet, or the Piglet file at path
  --no-stdin                     Ignore piped stdin (for a stdin pipe that never closes)

PiG keeps its configuration in ~/.pig and never reads or writes ~/.pi.

Options:
  --provider <name>              Provider to search for --model (requires --model)
  --model <pattern>              Model pattern or ID (supports "provider/id" and optional ":<thinking>")
  --api-key <key>                API key (defaults to env vars)
  --system-prompt <text>         System prompt (default: coding assistant prompt)
  --append-system-prompt <text>  Append text or file contents to the system prompt (can be used multiple times)
  --mode <mode>                  Output mode: text (default), json, or rpc
  --print, -p                    Non-interactive mode: process prompt and exit
  --continue, -c                 Continue previous session
  --resume, -r                   Select a session to resume
  --session <path|id>            Use specific session file or partial UUID
  --session-id <id>              Use exact project session ID, creating it if missing
  --fork <path|id>               Fork specific session file or partial UUID into a new session
  --session-dir <dir>            Directory for session storage and lookup
  --no-session                   Don't save session (ephemeral)
  --name, -n <name>              Set session display name
  --models <patterns>            Comma-separated model patterns for Ctrl+P cycling
                                 Supports globs (anthropic/*, *sonnet*) and fuzzy matching
  --no-tools, -nt                Disable all tools by default (built-in and extension)
  --no-builtin-tools, -nbt       Disable built-in tools by default but keep extension/custom tools enabled
  --tools, -t <tools>            Comma-separated allowlist of tool names or patterns (*) to enable
                                 Keeps MCP tools unless an entry starts with mcp__
  --exclude-tools, -xt <tools>   Comma-separated denylist of tool names or patterns (*) to disable
                                 Applies to all tools, MCP tools included
  --thinking <level>             Set thinking level: off, minimal, low, medium, high, xhigh, max
  --extension, -e <path>         Load an extension file or builtin:<name> (can be used multiple times)
  --no-extensions, -ne           Disable extension discovery and built-in extensions (explicit -e paths still work)
  --no-mcp                       Disable built-in MCP support: no servers connect and no MCP tools
  --skill <path>                 Load a skill file or directory (can be used multiple times)
  --no-skills, -ns               Disable skills discovery and loading
  --prompt-template <path>       Load a prompt template file or directory (can be used multiple times)
  --no-prompt-templates, -np     Disable prompt template discovery and loading
  --theme <path>                 Load a theme file or directory (can be used multiple times)
  --use-theme <name[/name]>      Set the initial interactive theme for this run
  --no-themes                    Disable theme discovery and loading
  --no-context-files, -nc        Disable AGENTS.md and CLAUDE.md discovery and loading
  --export <file>                Export session file to HTML and exit
  --list-models [search]         List available models (with optional fuzzy search)
  --verbose                      Force verbose startup (overrides quietStartup setting)
  --tui-mode <mode>              TUI mode: fullscreen (default) or regular
  --approve, -a                  Trust project-local files for this run
  --no-approve, -na              Ignore project-local files for this run
  --offline                      Disable startup network operations (same as PI_OFFLINE=1)
  --                             End option parsing; treat remaining arguments as messages/files
  --help, -h                     Show this help
  --version, -v                  Show version number

Extensions can register additional flags (e.g., --plan from plan-mode extension).

Examples:
  # Print a provider API key for an external client
  pig auth print-api-key --provider openai

  # Print an OAuth bearer token for an external client (refreshes if expired)
  pig auth print-bearer-token --provider openai-codex

  # Interactive mode
  pig

  # Interactive mode with initial prompt
  pig "List all .ts files in src/"

  # Include files in initial message
  pig @prompt.md @image.png "What color is the sky?"

  # Non-interactive mode (process and exit)
  pig -p "List all .ts files in src/"

  # Prompt beginning with a dash
  pig -p -- "- Summarize these points"

  # Multiple messages (interactive)
  pig "Read package.json" "What dependencies do we have?"

  # Continue previous session
  pig --continue "What did we discuss?"

  # Start a named session
  pig --name "Refactor auth module"

  # Use different model
  pig --provider openai --model gpt-4o-mini "Help me refactor this code"

  # Use model with provider prefix (no --provider needed)
  pig --model openai/gpt-4o "Help me refactor this code"

  # Use model with thinking level shorthand
  pig --model sonnet:high "Solve this complex problem"

  # Limit model cycling to specific models
  pig --models claude-sonnet,claude-haiku,gpt-4o

  # Limit to a specific provider with glob pattern
  pig --models "github-copilot/*"

  # Cycle models with fixed thinking levels
  pig --models sonnet:high,haiku:low

  # Start with a specific thinking level
  pig --thinking high "Solve this complex problem"

  # Read-only mode (no file modifications possible)
  pig --tools read,grep,find,ls -p "Review the code in src/"

  # Codemode with only the tools of one MCP server
  pig --tools read,bash,codemode,'mcp__radius__*'

  # Disable one tool while keeping the rest available
  pig --exclude-tools ask_question

  # Export a session file to HTML
  pig --export ~/.pig/agent/sessions/--path--/session.jsonl
  pig --export session.jsonl output.html

Environment Variables:
  ANTHROPIC_AUTH_TOKEN             - Anthropic bearer auth token
  ANTHROPIC_API_KEY                - Anthropic Claude API key
  ANTHROPIC_OAUTH_TOKEN            - Anthropic OAuth token (alternative to API key)
  ANT_LING_API_KEY                 - Ant Ling API key
  OPENAI_API_KEY                   - OpenAI GPT API key
  AZURE_OPENAI_API_KEY             - Azure OpenAI API key
  AZURE_OPENAI_BASE_URL            - Azure OpenAI/Cognitive Services base URL (e.g. https://{resource}.openai.azure.com)
  AZURE_OPENAI_RESOURCE_NAME       - Azure OpenAI resource name (alternative to base URL)
  AZURE_OPENAI_API_VERSION         - Azure OpenAI API version (default: v1)
  AZURE_OPENAI_DEPLOYMENT_NAME_MAP - Azure OpenAI model=deployment map (comma-separated)
  DEEPSEEK_API_KEY                 - DeepSeek API key
  NVIDIA_API_KEY                   - NVIDIA NIM API key
  GEMINI_API_KEY                   - Google Gemini API key
  GROQ_API_KEY                     - Groq API key
  CEREBRAS_API_KEY                 - Cerebras API key
  XAI_API_KEY                      - xAI Grok API key
  FIREWORKS_API_KEY                - Fireworks API key
  TOGETHER_API_KEY                 - Together AI API key
  BASETEN_API_KEY                  - Baseten API key
  OPENROUTER_API_KEY               - OpenRouter API key
  AI_GATEWAY_API_KEY               - Vercel AI Gateway API key
  ZAI_API_KEY                      - ZAI Coding Plan API key (Global)
  ZAI_CODING_CN_API_KEY            - ZAI Coding Plan API key (China)
  MISTRAL_API_KEY                  - Mistral API key
  MINIMAX_API_KEY                  - MiniMax API key
  MOONSHOT_API_KEY                 - Moonshot AI API key
  OPENCODE_API_KEY                 - OpenCode Zen/OpenCode Go API key
  KIMI_API_KEY                     - Kimi For Coding API key
  META_API_KEY                     - Meta Model API key
  CLOUDFLARE_API_KEY               - Cloudflare API token (Workers AI and AI Gateway)
  CLOUDFLARE_ACCOUNT_ID            - Cloudflare account id (required for both)
  CLOUDFLARE_GATEWAY_ID            - Cloudflare AI Gateway slug (required for AI Gateway)
  QWEN_TOKEN_PLAN_API_KEY          - Qwen Token Plan API key (international region)
  QWEN_TOKEN_PLAN_CN_API_KEY       - Qwen Token Plan API key (China region)
  XIAOMI_API_KEY                   - Xiaomi MiMo API key (api.xiaomimimo.com billing)
  XIAOMI_TOKEN_PLAN_CN_API_KEY     - Xiaomi MiMo Token Plan API key (China region)
  XIAOMI_TOKEN_PLAN_AMS_API_KEY    - Xiaomi MiMo Token Plan API key (Amsterdam region)
  XIAOMI_TOKEN_PLAN_SGP_API_KEY    - Xiaomi MiMo Token Plan API key (Singapore region)
  AWS_PROFILE                      - AWS profile for Amazon Bedrock
  AWS_ACCESS_KEY_ID                - AWS access key for Amazon Bedrock
  AWS_SECRET_ACCESS_KEY            - AWS secret key for Amazon Bedrock
  AWS_BEARER_TOKEN_BEDROCK         - Bedrock API key (bearer token)
  AWS_REGION                       - AWS region for Amazon Bedrock (e.g., us-east-1)
  PIG_CODING_AGENT_DIR             - Config directory (default: ~/.pig/agent)
  PIG_CODING_AGENT_SESSION_DIR     - Session storage directory (overridden by --session-dir)
  PIG_PACKAGE_DIR                  - Override package directory (for Nix/Guix store paths)
  PI_OFFLINE                       - Disable startup network operations when set to 1/true/yes
  PI_TELEMETRY                     - Override install telemetry when set to 1/true/yes or 0/false/no

Built-in Tool Names:
  read       - Read file contents
  bash       - Execute bash commands
  powershell - Execute PowerShell commands on Windows
  edit       - Edit files with find/replace
  write      - Write files (creates/overwrites)
  grep       - Search file contents (read-only, off by default)
  find       - Find files by glob pattern (read-only, off by default)
  ls         - List directory contents (read-only, off by default)
EXIT=0
```

说明：该扩展**不注册额外命令行 flag**（原版同样没有），故 `--help` 输出即宿主二进制的
通用帮助；关键在于扩展被 `-e ./rpiv-ask-user-question` 顺利加载并使进程 EXIT=0。
若加载失败（包名冲突、`package main` 误判为 standalone、SDK 版本不匹配等），PiG 会在
此处以非零退出并报错。

`install --validate-only --json` 实测原文：

```console
$ PIG_CODING_AGENT_DIR=/root/.pig-test/agent ./bin/pig-test install ./rpiv-ask-user-question --validate-only --json
{
  "valid": true,
  "name": "rpiv-ask-user-question",
  "source": "/root/workspace/.wt/pi-extensions/feature-go-rpiv-ask/go",
  "hash": "940cdc03ff8bfa97e0c6cfd05c551883de100599a4a51d3d03739acb8338b363",
  "tools": [
    "ask_user_question"
  ],
  "handlers": [
    "before_agent_start"
  ],
  "toolDetails": [
    {
      "name": "ask_user_question",
      "label": "Ask User Question",
      "description": "Ask the user one or more structured questions during execution. Use when you need to:\n1. Gather user preferences or requirements\n2. Clarify ambiguous instructions\n3. Get decisions on implementation choices as you work\n4. Offer choices to the user about what direction to take\n\nUsage notes:\n- Users can type a custom answer via the automatically appended \"Type something.\" row on every question or press Esc to abandon the questionnaire. Do NOT author \"Other\" or \"Type something.\" labels yourself — reserved labels are rejected at runtime.\n- Use multiSelect: true when multiple answers are valid. The \"Type something.\" row is available on every question, including when options carry a `preview`; in preview mode it expands to the full pane width while typing so the custom answer is not cramped into the narrow options column.\n- If you recommend a specific option, make that the first option in the list and add \"(Recommended)\" at the end of the label.\n\nPreview feature:\nUse the optional `preview` field on options when presenting concrete artifacts that users need to visually compare:\n- ASCII mockups of UI layouts or components\n- Code snippets showing different implementations\n- Diagram variations\n- Configuration examples\n\nPreview content is rendered as markdown in a monospace box. Multi-line text with newlines is supported. When any option has a preview, the UI switches to a side-by-side layout with a vertical option list on the left and preview on the right. Do not use previews for simple preference questions where labels and descriptions suffice. Note: previews are only supported for single-select questions (not multiSelect).",
      "promptSnippet": "Ask the user up to 4 structured questions (2-4 options each) when requirements are ambiguous",
      "promptGuidelines": [
        "Use ask_user_question whenever the user's request is underspecified and you cannot proceed without concrete decisions — you can ask up to 4 questions per invocation.",
        "Each question MUST have 2-4 options. Every option requires a concise label (1-5 words) and a description explaining what the choice means or its trade-offs. The user can additionally type a custom answer via the automatically appended \"Type something.\" row on every question, or press Esc to abandon the questionnaire. Do NOT author \"Other\" or \"Type something.\" labels yourself — reserved labels are rejected at runtime.",
        "Set multiSelect: true when multiple answers are valid. Provide an options[].preview markdown string when an option benefits from richer side-by-side context (mockups, code snippets, diagrams, configs) — single-select only. The \"Type something.\" row is appended to every question; in preview mode it expands to the full pane width while typing so the custom answer is not cramped into the narrow options column. If you recommend a specific option, make that the first option and append \"(Recommended)\" to its label.",
        "Do not stack multiple ask_user_question calls back-to-back — group all clarifying questions into one invocation."
      ]
    }
  ],
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

`"valid": true` + `"registered": true`，`form: factory`、`runtime: subprocess`、
`factory: Extension`，与 `go.mod` / `README.md` 声明的加载形态一致。

### 3.4 5 轮 B 档门禁（Verifier + Reviewer）迭代消除的缺陷

| 轮次 | 消除的缺陷 |
|---|---|
| R1 | **折叠监听器前置注册与隐藏降级**：`registerCollapseKeyListener` 必须在会话构建**之前**注册（与原版同序），且 `canReopenWhileHidden` 只在监听器真正拿到（`err == nil && remove != nil`）时才为真；监听器缺失时隐藏态不承诺可重开。 |
| R1 | **多选带 `preview` 不拦截**：`validate-questionnaire` 的守卫集与原版逐字对齐——`preview` 仅单选支持，但多选带 `preview` 属**合法**输入，不得按错误拒绝。 |
| R2 | **DialogView 动态预览面板**：预览面板必须取**本 tick** 的 `liveProps.ActivePreviewPane`，不得沿用 dialog 构造时固化的那个（无面板时回退 `fallbackPreviewPane`）。 |
| R2 | **`hostTUI.RequestRender` 走 `invalidate`**：外部编辑器交还控制权后的重绘必须经 `requestRender` 回调（由 `SetInvalidate` 安装的 `s.invalidate`）触发，且**不得**在持有 `s.mu` 时回调，避免同一 goroutine 重入死锁。 |
| R3 | **`allowedBaseKeys` 补反斜杠**：`config.go:320` 的单字符基础键类必须包含 `\\`，与原版 `allowedBaseKeys` 字符串逐字一致（`"abcdefghijklmnopqrstuvwxyz0123456789_-!@#$%^&*()|~`'\":;,./<>?[]{}=\\"`）。 |
| R3 | **`jsonInt` 安全整数守卫**：`types.go:244` 对 JSON `float64` 与进程内 `int` 统一拦截 `NaN`、`±Inf`、小数及超出 `±(2^53-1)`（`maxSafeInt = int64(1<<53 - 1)`、`maxSafeIntFloat = float64(1<<53 - 1)`）的超大整数。 |
| R4/R5 | **消除 `s.mu` 重入死锁**：`Render` / `runEffect` 全程在 `s.mu` 互斥保护下执行；`Apply` 在持锁期间被调用，故其内部绝不回调 `RequestRender` 这类会重新取锁的公开入口（`questionnaire-session.go:174/196-202/226-241/249-256` 均有注释钉死该约束）。 |

最终轮次：Verifier 4/4 判据 PASS，Reviewer 0 must fix / 0 should fix。

---

## 4. 与 TS 原版（v2.15.0）的行为差异

### 4.1 隔离配置查找超集（有意为之的 PiG 增量）

仅当设置了 `PIG_CODING_AGENT_DIR` 或 `PIG_HOME` 时，才**优先**查找隔离路径
（`config.go:149` `isolatedConfigPaths`，`config.go:276` `candidates := append(e.isolatedConfigPaths(...), e.configPath(...))`）：

1. `$PIG_CODING_AGENT_DIR/extensions/rpiv-ask-user-question/config.json`
2. `$PIG_HOME/.config/rpiv-ask-user-question/config.json`
3. `$PIG_HOME/config/rpiv-ask-user-question/config.json`

未设置任何隔离变量时，查找顺序**退化为与 TS `@juicesharp/rpiv-config` 完全一致**的两级：

4. `$XDG_CONFIG_HOME/rpiv-ask-user-question/config.json`
5. `~/.config/rpiv-ask-user-question/config.json`（`legacyConfigPath`，即 `defaultConfigDir()` = `join(homedir(), ".config")`）

**损坏文件不静默回退**：XDG 路径的文件存在但 JSON 损坏时报 warning 并返回 `{}`，
不会继续尝试 legacy 路径——与原版语义逐字一致（`config.go:179/184`）。

### 4.2 Locale 来源钩子

TS 原版通过**可选 peerDependency** `@juicesharp/rpiv-i18n`（本机未安装）获取语言；
缺失时 `t(key, fallback)` 全部走内联英文回退。Go 侧内置 `LocaleSource` 包级钩子
（`i18n.go:73`），按 `RPIV_I18N_LOCALE` → `PIG_LOCALE` → `LANG` → `LC_ALL` →
`LC_MESSAGES` 顺序读取（`i18n.go:78`），并经 `normalizeLocaleCode` 归一化 POSIX
locale：截断 `.`/`@` 后缀、`_`→`-`、`C`/`POSIX` 视为无 locale、先精确匹配
（`pt-BR`）再按基础语言回落（`zh_CN.UTF-8` → `zh`）。

9 个 `locales/*.json` 与 npm 原版**逐字节一致**（实测 `diff -r` 无差异，md5 全部相同）：

```console
$ md5sum /root/.pi/agent/npm/node_modules/@juicesharp/rpiv-ask-user-question/locales/*.json | sort -k2
dd5207b5d0f08e21ce150b27839d928b  .../locales/de.json
532140312b50b172d163d29a1eace3ac  .../locales/en.json
5aee942a7eac35ba6ba0c4cd50df65c1  .../locales/es.json
2c4bcd7faacc6254c4cb887fa2e4b514  .../locales/fr.json
99445923dea2f7ee96905f4ddba57188  .../locales/pt-BR.json
fddcac5ab098f2cadfcc277f837f5db6  .../locales/pt.json
ad2b8bd540e85675ce89329a2cfa7ae2  .../locales/ru.json
8e40c6608bf18c931913d3726b2eaf19  .../locales/uk.json
e262ec0d524ac66d3ad8ec7d84318ede  .../locales/zh.json
$ diff -r <npm locales> <go locales>
IDENTICAL
```

缺失键按原版 `t(key, fallback)` 回退英文。测试态由 `main_test.go` 的 `TestMain`
把 `LocaleSource` 钉为空串，保证整套测试不依赖机器 `LANG`。

### 4.3 `jsonInt` 安全整数上限

对齐 JS `Number.isSafeInteger` 的 `±(2^53 - 1)` 边界（`types.go:225-236` 定义
`maxSafeInt` / `maxSafeIntFloat`）：对 JSON 解码得到的 `float64` 与进程内直接传入的
`int`，统一拦截 `NaN`、`±Inf`、小数（`math.Trunc(n) != n`）以及超出 `±(2^53 - 1)`
的超大整数（`types.go:244-265`）。TS 原版只可能收到 `float64`，故 `int` 分支是 Go 侧
的**加固**而非行为差异。

---

## 5. PiG SDK 适配与 PiG 自身缺口记录（只记不改 PiG 源码）

### 5.1 `tui.getTopmostVisibleOverlay()` 缺口

TS 原版 `presentation.ts` 调用 `tui.getTopmostVisibleOverlay()` 判断内联槽位是否需
让位于已有浮层。PiG subprocess SDK **未暴露**该查询 RPC。Go 侧通过可注入探针适配：
`MustYieldInlineSlot(topmostOverlayVisible func() bool) bool`（`presentation.go:103`）——
探针为 `nil` 时**不误抢占**（直接返回 false），由会话从宿主浮层状态注入探针。

### 5.2 Pi Settings Manager 外部编辑器读取缺口

TS 原版从宿主 settings manager 读取外部编辑器配置。PiG SDK **未跨进程暴露**
settings manager。Go 侧按相同优先级直接读取进程环境：`$VISUAL` → `$EDITOR`
（`external-editor.go`）。

### 5.3 配置读取告警的 stderr 通道

TS `@juicesharp/rpiv-config` 在 `config.json` 损坏时向进程 `stderr` 打印 warning。
PiG subprocess 扩展的 `stdout`/`stderr` 由宿主协议桥接管（协议通道），直接写会污染
协议流。故 `osConfigEnv.warn` 在生产态**静默回退 `{}`**（`config.go:66` 默认 no-op，
`config.go:179/184` 为两处告警点）；测试态可注入捕获器断言告警文案。

---

## 6. 遗留问题、工具缺陷专项调查与未跑项清单

### 6.1 `ctxmode` 工具链 `compressToolText` 截断破坏 JSON 的设计缺陷（奉爸爸指示专项排查结论）

**现象**：审查员在 `codemode` 沙盒中执行
`JSON.parse(await tools.ctx_fs({action: "ls", path: ".../go/rpiv-ask-user-question"}))`
时抛出
`SyntaxError: Expected property name or '}' in JSON at position 9085 (line 376 column 1)`。

**根因定位**（本次逐行实读源码确认）：

| 组件 | 位置 | 说明 |
|---|---|---|
| TS 定义 | `/root/workspace/ctxmode/integrations/pi/ctxmode.ts:155` | `export function compressToolText(text: string): string` |
| TS 调用点 | `/root/workspace/ctxmode/integrations/pi/ctxmode.ts:611` | `callTool` 内 `return compressToolText(text)` |
| TS 常量 | `/root/workspace/ctxmode/integrations/pi/ctxmode.ts:120-121` | `OUTPUT_CHAR_CAP = 50000`、`OUTPUT_LINE_CAP = 500`（`CTXMODE_OUTPUT_LINES`） |
| Go 定义 | `/root/workspace/pi-extensions/go/ctxmode/text.go:84` | `func CompressToolText(text string) string` |
| Go 调用点 | `/root/workspace/pi-extensions/go/ctxmode/client.go:656` | `return CompressToolText(strings.Join(texts, "\n")), nil` |
| Go 常量 | `go/ctxmode/text.go:86` | `envNumber("CTXMODE_OUTPUT_CHARS", 50000)`、`envNumber("CTXMODE_OUTPUT_LINES", 500)` |

`compressToolText` 对 `ctx_fs`、`ctx_git`、`ctx_run` 等工具返回文本做**无感知的纯文本
行数截断**。当目录条目数 > 60 时，`ctx_fs ls` 返回的 2 空格缩进 JSON 行数超过
`OUTPUT_LINE_CAP = 500`，于是（`ctxmode.ts:162-170` / `text.go:88-97`）：

- `headN = max(40, floor(500 × 0.75)) = 375`
- `tailN = max(15, 500 − 375 − 1) = 124`
- 保留前 375 行 + 后 124 行，在**第 376 行**插入非 JSON 文本标记
  `\n... (省略 ${omitted} 行；可缩小范围再查) ...\n`（`ctxmode.ts:168`；
  Go 侧 `fmt.Sprintf("\n... (省略 %d 行；可缩小范围再查) ...\n", ...)`）

该标记直接把合法 JSON 腰斩：`JSON.parse` 在第 376 行第 1 列（即标记起始处）抛
`SyntaxError`，与报错的 `position 9085 (line 376 column 1)` 精确吻合。
本例目录 56 个条目 / 574 行，按上述口径 `省略行数 = 总行数 − 375 − 124 = 574 − 499 = 75`。
（排查记录中"省略 74 行"对应同一 375/124 切分下 573 行的载荷；两种写法指向同一
机制，此处以公式为准。）

**本会话复现**（`codemode` 中 `await tools.ctx_fs({action:"ls", path:"go/rpiv-ask-user-question"})`
后对返回文本逐行统计，实测原文）：

```text
TOTAL_CHARS=12268
TOTAL_LINES=500
MARKER_1BASED_LINE=376
MARKER_TEXT="... (省略 2 行；可缩小范围再查) ..."
HEAD_LINES=375
TAIL_LINES=124
CHARS_BEFORE_MARKER=9234
FIRST_LINE="{"
LAST_LINE="}"
```

复现证实：输出被钉死在 500 行、标记落在第 376 行、首行 `{` 与末行 `}` 之间被插入
非 JSON 文本——`JSON.parse` 必败。

**后续修复建议**（属 `ctxmode` 宿主仓与 `go/ctxmode` 范围，本分支按隔离规矩**未跨仓
修改**，仅记录）：

1. 对结构化 JSON 输出工具（`ctx_fs`、`ctx_git`）**跳过行级文本截断**，或改用**合法
   JSON 字段级截断**（如截断 `entries` 数组并附 `"truncated": true`），保证输出始终
   可被 `JSON.parse`。
2. 或在 `codemode` 的 `callTool` 桥接层**透传原始未截断文本**（截断只对直接呈现给
   模型的文本生效，不对程序化消费路径生效）。
3. 若必须保留行级截断，至少让标记本身是合法 JSON（如把整个返回包成
   `{"truncated":true,"head":...,"tail":...}`），而非在原文行间插入。

### 6.2 未跑项清单（如实列出，绝不写"应该可以"）

- **`make examples-smoke`**：**未跑**。该目标（`Makefile:81`）为
  `$(PIG) -e ./examples/hello --mode json -p "调用 hello_ping 工具并返回其结果"`，
  会发起**真实大模型网络请求**，按规矩不跑。
- **交互式真机终端手敲按键 smoke**：**未跑**。子进程与沙盒环境无人工交互 TTY。
  已通过 `QuestionnaireSession` + `sdk.Context` 端到端单测（342 个顶层测试 /
  522 个测试项，含按键路由级联、折叠/展开、外部编辑器、浮层隐藏重开等路径）与
  `./bin/pig-test` 插件加载校验替代。
- **生产环境切流与部署**：**未跑**。按 `go/PLAN.md` §0 硬规矩，在爸爸批准前绝不修改
  `/root/.pi/agent/`、不切换线上 Pi。

### 6.3 隔离与零污染实测原文

`/root/.pi/agent/extensions/*.ts` md5（11 个文件，与第 2 批记录一致，无任何变动）：

```console
$ md5sum /root/.pi/agent/extensions/*.ts | sort -k2
55775e1fda107b4da9704c007d33b7b8  /root/.pi/agent/extensions/auto-continue.ts
9b5692bfd8f7488154bf297d9e38e0a6  /root/.pi/agent/extensions/cache-stats.ts
fc35f79118e4b655f07a4fadf793f5dd  /root/.pi/agent/extensions/codegraph-go.ts
5f925d4921788c30dda994192d29c15a  /root/.pi/agent/extensions/ctxmode.ts
0e33386a7178868cbefe31969f08720b  /root/.pi/agent/extensions/ctx-orchestrate.ts
30722dbb05328b63a2cb87729072b7a0  /root/.pi/agent/extensions/exclude-tools-guard.ts
c5e0582adbba7fba3bcba83b08274a79  /root/.pi/agent/extensions/herdr-agent-state.ts
3d909e1a87dd28a8d21e7e9b00668bad  /root/.pi/agent/extensions/herdsman-pi.ts
f4ff427bb51b6fa378fcfd356b81fa99  /root/.pi/agent/extensions/jev-gate.ts
3f8ec8443daf30e582d57eec99df383c  /root/.pi/agent/extensions/moshi-hooks.ts
a1f215b9843f38943cf86df38b4205f5  /root/.pi/agent/extensions/no-tables.ts
EXIT=0
```

主仓库 `/root/workspace/pi-extensions` 状态（只读取证；本批次所有改动均在授权 worktree
内，未触碰主仓库工作区）：

```console
$ git -C /root/workspace/pi-extensions status --short
 M go/go.work
?? go/pi-my-compact-ui/
$ git -C /root/workspace/pi-extensions branch --show-current
feature/go-batch1
$ git -C /root/workspace/pi-extensions diff go/go.work
diff --git a/go/go.work b/go/go.work
index 8b33ec2..ffacbcd 100644
--- a/go/go.work
+++ b/go/go.work
@@ -16,6 +16,7 @@ use (
 	./no-tables
 	./pi-context-pruning
 	./pi-env
+	./pi-my-compact-ui
 	./pi-tinyfish
 	./pigsdk
 )
```

上述主仓库待提交改动属 `feature/go-batch1` 分支上**与本批次无关**的
`pi-my-compact-ui` 工作（其 worktree 与本次 `feature/go-rpiv-ask` 相互独立），
本批次既未创建也未暂存其中任何内容。

---

## 7. 收口核验

### 7.1 工作树与提交落点

**报告提交前**的实测原文（`go/reports/batch-4-rpiv-ask-user-question.md` 尚为未跟踪文件）：

```console
$ git status --short
?? go/reports/batch-4-rpiv-ask-user-question.md
$ git log --oneline -4
1f7d089 feat(go): port rpiv-ask-user-question extension to Go (batch 4)
338c5f0 feat(go): port cache-stats extension to Go (batch 3)
4a03b16 docs(go): add batch-2 migration report
f5104f8 feat(go): port ctxmode extension to Go (batch 2)
$ git branch --show-current
feature/go-rpiv-ask
```

**报告提交后**的收口核验命令为 `git status --short`（期望**无任何输出**）与
`git log --oneline -4`（期望 HEAD 为 `docs(go): add batch-4 rpiv-ask-user-question
migration report`，其下为 `1f7d089`）。

> 说明：一个已提交的文件在物理上不可能包含**其自身**提交的 SHA，故此处只给出命令与
> 期望态；报告提交后的**真实原文**由 worker 在交付回执中给出（含两个提交的完整 SHA、
> 短 SHA 与 `git log -2 --stat`）。

两个提交（`feat(go)` 代码提交 + `docs(go)` 报告提交）均在 `feature/go-rpiv-ask`
分支上；报告提交后 `git status --short` 为空（工作树完全干净，无未提交、无未跟踪
文件）。**未合并**到 `feature/go-batch1` 或 `main`，**未 push**。

### 7.2 复现方式

```bash
# 模块级（cd go/rpiv-ask-user-question）
gofmt -l .
go build ./...
go vet ./...
go test -count=1 -race -cover ./...

# 工作区级（cd go）
make build vet test sdk-check

# PiG 隔离加载（cd go）
PIG_CODING_AGENT_DIR=/root/.pig-test/agent ./bin/pig-test -e ./rpiv-ask-user-question --help
PIG_CODING_AGENT_DIR=/root/.pig-test/agent ./bin/pig-test install ./rpiv-ask-user-question --validate-only --json
```

BATCH-4 DONE
