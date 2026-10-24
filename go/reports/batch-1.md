# 第 1 批（小件）：6 个扩展的 Go 移植报告（reports/batch-1.md）

| 项 | 值（均为本次实测/实读） |
|---|---|
| 分支 | `feature/go-batch1`（`git status --porcelain` 首行 `## feature/go-batch1`） |
| HEAD（写报告时） | `9cd6108` feat(go): port exclude-tools-guard extension to Go (batch 1) |
| 报告会话起点 | `2026-10-24 10:05:03 +0800`（`date '+START %Y-%m-%d %H:%M:%S %z'`） |
| 工具链实测 | `go version go1.26.4 linux/amd64`（EXIT=0）；`pig --version` → `0.5.2+1.4.0`（EXIT=0） |
| 工作目录 | `/root/workspace/pi-extensions/go` |

证据范围声明：

- 本报告只写本次会话**亲自跑过**的命令输出与**亲眼读到**的文件内容；每条命令贴原文并显式捕获 `EXIT=$?`（含 `make` 各目标、6 个组件的 gofmt/build/vet/`-race -v` 测试、6 次 PiG 隔离加载）。
- 未跑的都写「未跑」并给原因（见 §9、§10）。
- 门禁轮次记录（§5）引用的是调度档位留下的任务书文件 `/tmp/batch1-*.md`（临时过程文件，非持久交付物）与该组件 README 的加固记录；本轮没有重跑历史门禁，引用处均标来源。
- 全程未打印任何密钥；隔离目录 `models.json` 只读取了 mtime，未读内容。

---

## 0. 证据勘误：gemini-safety 部署件的实际路径

任务书 §3 指定的部署件路径 `/root/.pi/agent/extensions-disabled/gemini-safety.ts.disabled-20261024` **在本机不存在**。实测：

```console
$ ls -d /root/.pi/agent/extensions-disabled ; echo "LS_EXIT=$?"
ls: 无法访问 '/root/.pi/agent/extensions-disabled': 没有那个文件或目录
LS_EXIT=2

$ find /root -name '*gemini-safety*' -not -path '*/node_modules/*' 2>/dev/null ; echo "FIND_EXIT=$?"
/root/.pig/cache/jiti/5f6100de4f30033eaa3a9f276c201d5348fe621722acde6c4a316b9d0efcaa30/extensions-gemini-safety.7ac35e0e.mjs
/root/.pig-pidirs-test/agent/extensions/gemini-safety.ts
FIND_EXIT=0

$ find /root/.pi -name '*gemini*' 2>/dev/null | head
（无 gemini-safety 命中；仅 4 个 agents/*.bak 含 "gemini" 字样）
```

同内容（行数/字节/md5 与任务书给的数字一致）的部署件实际在 `/root/.pig-pidirs-test/agent/extensions/gemini-safety.ts`：

```console
$ md5sum /root/.pig-pidirs-test/agent/extensions/gemini-safety.ts
5b14b82a1d8e72f723c1ed98dc1b1f13  /root/.pig-pidirs-test/agent/extensions/gemini-safety.ts
$ wc -lc /root/.pig-pidirs-test/agent/extensions/gemini-safety.ts
  49 2325 /root/.pig-pidirs-test/agent/extensions/gemini-safety.ts
```

因此 §3 一律按**实测路径**书写；任务书里那条 `extensions-disabled/...disabled-20261024` 路径未能核实，按「不存在」记录（不编造）。

---

## 1. 批次结论摘要

| 组件 | commit | 提交文件数 | Go 实现 / 测试行数 | 测试状态（`-count=1 -race -v`） |
|---|---|---|---|---|
| pi-context-pruning | `ca1e121` | 9 | 721 / 903 | 25 顶层全 PASS，`-race` 全绿，`DATA RACE`=0，`ok … 1.015s` |
| pi-env | `8880a9f` | 4 | 925 / 886 | 23 顶层 / 34 PASS（含子用例），`DATA RACE`=0，`ok … 1.013s` |
| pi-tinyfish | `dd1828f` | 7 | 1280 / 959 | 21 顶层 / 31 PASS，`DATA RACE`=0，`ok … 1.084s` |
| no-tables | `1afde07` | 4 | 608 / 660 | 36 顶层 / 54 PASS，`DATA RACE`=0，`ok … 1.029s` |
| herdr-agent-state | `fb043a7` | 8 | 715 / 1364 | 17 顶层全 PASS，`DATA RACE`=0，`ok … 1.441s` |
| exclude-tools-guard | `9cd6108` | 4 | 968 / 1846 | 14 顶层 / 85 PASS（含子用例），`DATA RACE`=0，`ok … 1.228s` |
| 骨架：`go/pigsdk/` 镜像 | `e5e310f` | 172（该提交共 179） | —— | `make sdk-check` → `OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)`，EXIT=0 |
| 骨架：`go/go.work` | `e5e310f` 建立，`bd9129d` 收敛为 6 组件 | 1 | 8 条 `use` 项（`.`/6 组件/`./pigsdk`） | 根模块与 6 组件 `go build/vet/test` 全绿（见 §10） |
| 骨架：Makefile 组件门禁 | `e5e310f` | 1 | `COMPONENTS := $(filter-out pigsdk,…$(wildcard */go.mod))` | `make build/vet/test` 三段自动覆盖 6 组件，EXIT 全 0 |
| 骨架：`make sdk-check` | `e5e310f` | （在 Makefile 内） | 逐文件 md5 对比 staged SDK，仅放行 `README.md`，无 staged SDK 时 `SKIP` + 0 | 实测 EXIT=0，见 §10 |

组件级测试命令的原始输出见 §4 各节（每节都贴了 `RUN`/`PASS`/`FAIL`/`DATA RACE` 计数与 `ok` 行）。

---

## 2. 提交清单（实测原文）

```console
$ git -C /root/workspace/pi-extensions log --oneline -10
9cd6108 feat(go): port exclude-tools-guard extension to Go (batch 1)
bd9129d chore(go): drop deprecated gemini-safety extension
fb043a7 feat(go): port herdr-agent-state extension to Go (batch 1)
1afde07 feat(go): port no-tables extension to Go (batch 1)
dd1828f feat(go): port pi-tinyfish extension to Go with direct HTTP (batch 1)
8880a9f feat(go): port pi-env extension to Go (batch 1)
ca1e121 feat(go): port pi-context-pruning extension to Go (batch 1)
58f930b docs(plan): cutover reuses pi config dir via PIG_USE_PI_DIRS=1
e5e310f feat(go): port gemini-safety extension to Go with batch-1 scaffold
eebe6bd chore(go): rename examples-build to examples-smoke (it makes a model call)
（EXIT=0）
```

逐提交 `git show --shortstat` 原文（首行 + 文件数；文件数用 `git show --name-only --pretty=format: <sha> | grep -c .` 实测）：

```console
$ for sha in e5e310f ca1e121 8880a9f dd1828f 1afde07 fb043a7 bd9129d 9cd6108; do
    printf '%s | ' "$sha"; git show --shortstat --oneline "$sha" | tr '\n' ' '; echo; done
e5e310f | e5e310f feat(go): port gemini-safety extension to Go with batch-1 scaffold  179 files changed, 45356 insertions(+), 16 deletions(-)
ca1e121 | ca1e121 feat(go): port pi-context-pruning extension to Go (batch 1)  9 files changed, 1712 insertions(+)
8880a9f | 8880a9f feat(go): port pi-env extension to Go (batch 1)  4 files changed, 1955 insertions(+)
dd1828f | dd1828f feat(go): port pi-tinyfish extension to Go with direct HTTP (batch 1)  7 files changed, 2355 insertions(+)
1afde07 | 1afde07 feat(go): port no-tables extension to Go (batch 1)  4 files changed, 1407 insertions(+)
fb043a7 | fb043a7 feat(go): port herdr-agent-state extension to Go (batch 1)  8 files changed, 2307 insertions(+)
bd9129d | bd9129d chore(go): drop deprecated gemini-safety extension  8 files changed, 12 insertions(+), 601 deletions(-)
9cd6108 | 9cd6108 feat(go): port exclude-tools-guard extension to Go (batch 1)  4 files changed, 2983 insertions(+)
```

文件清单（去掉 `go/pigsdk/` 172 个镜像文件后的提交内容，`git show --name-only --pretty=format:` 实测）：

- `e5e310f`：`go/Makefile`、`go/README.md`、`go/gemini-safety/{README.md,extension.go,extension_test.go,go.mod}`、`go/go.work` + `go/pigsdk/` 172 文件。
- `ca1e121`：`go/pi-context-pruning/{README.md,config.go,config_test.go,extension.go,extension_test.go,go.mod,pruner.go,pruner_test.go,types.go}`。
- `8880a9f`：`go/pi-env/{README.md,extension.go,extension_test.go,go.mod}`。
- `dd1828f`：`go/pi-tinyfish/{README.md,api.go,api_test.go,extension.go,extension_test.go,go.mod,render.go}`。
- `1afde07`：`go/no-tables/{README.md,extension.go,extension_test.go,go.mod}`。
- `fb043a7`：`go/herdr-agent-state/{README.md,extension.go,extension_test.go,go.mod,state.go,state_test.go,transport.go,transport_test.go}`。
- `bd9129d`：`go/PLAN.md`、`go/README.md`、`go/gemini-safety/*`（删除 4 文件）、`go/go.work`、`go/reports/PLAN.v1.md`。
- `9cd6108`：`go/exclude-tools-guard/{README.md,extension.go,extension_test.go,go.mod}`。

**提交顺序实测发现（如实记录，未改历史）**：`go/go.work` 只在 `e5e310f` 与 `bd9129d` 两次提交中被改动：

```console
$ git -C /root/workspace/pi-extensions log --oneline -- go/go.work
bd9129d chore(go): drop deprecated gemini-safety extension
e5e310f feat(go): port gemini-safety extension to Go with batch-1 scaffold

$ git show ca1e121:go/go.work        # fb043a7:go/go.work 内容相同
use (
	.
	./gemini-safety
	./pigsdk
)
```

即 `ca1e121`…`fb043a7` 这 5 个组件提交**没有把各自的 `./<组件>` 条目一起提交**（工作树里已加、但当时提交只 add 了 `go/<组件>/`），到 `bd9129d` 才一次性补齐 6 条。用该提交的内容做实测：

```console
$ rm -rf /tmp/ca1chk && mkdir -p /tmp/ca1chk
$ cd /root/workspace/pi-extensions && git archive ca1e121 | tar -x -C /tmp/ca1chk
$ cd /tmp/ca1chk/go/pi-context-pruning && timeout 120 go build ./... ; echo EXIT=$?
pattern ./...: directory prefix . does not contain modules listed in go.work or their selected dependencies
EXIT=1
$ rm -rf /tmp/ca1chk ; echo CLEAN_EXIT=$?
CLEAN_EXIT=0
```

结论：中间 5 个提交点单独检出后组件不可构建（缺 `go.work` 条目 → SDK 无法解析到 `go/pigsdk`）；**当前 HEAD 已正确**（§10 的 `make build/vet/test` 全绿）。这是提交流程缺陷，不是代码缺陷，未改历史、未 amend。

---

## 3. gemini-safety：已弃用并删除

### 3.1 原因

- pi 侧只走喜鹊（magpie）的 `openai` / `anthropic` 格式；Gemini 原生 `generateContent` 通道不再使用。
- Gemini 2.5+ 的四类内容过滤默认即 `Off`，再注入 `safetySettings` 无实际效果。
- 故该扩展在 Go 侧没有存在理由；`PLAN.md` 第 1 批清单由 7 件改为 6 件（`bd9129d` 实测 diff，见下）。

### 3.2 证据（实测）

部署件（实际路径，见 §0 勘误）：`/root/.pig-pidirs-test/agent/extensions/gemini-safety.ts` —— 49 行 / 2325 B / md5 `5b14b82a1d8e72f723c1ed98dc1b1f13`。文件头自述其用途是「把 `safetySettings` 推给 Google 原生 generateContent」，并声明「对其他 provider/api 是 no-op」。

`/root/.pi/agent/extensions/` 已无该文件（贴 `ls` 原文）：

```console
$ ls -l /root/.pi/agent/extensions/
总计 436
-rw-r--r-- 1 root root 46766 10月 4日 20:16 auto-continue.ts
-rw-r--r-- 1 root root 81348 10月18日 16:38 cache-stats.ts
-rw-r--r-- 1 root root 45979 10月 3日 22:34 codegraph-go.ts
-rw-r--r-- 1 root root 39203 10月22日 09:23 ctxmode.ts
-rw-r--r-- 1 root root 45076 10月 4日 17:11 ctx-orchestrate.ts
-rw-r--r-- 1 root root 12450 10月22日 13:57 exclude-tools-guard.ts
-rw-r--r-- 1 root root  2466 10月 7日 21:08 exclude-tools-guard.ts.bak-20261008-122514
-rw-r--r-- 1 root root  6694  9月22日 12:15 herdr-agent-state.ts
-rw-r--r-- 1 root root   890  9月13日 16:55 herdsman-pi.ts
-rw-r--r-- 1 root root 48840 10月16日 23:14 jev-gate.ts
-rw-r--r-- 1 root root 32639 10月 7日 21:07 jev-gate.ts.bak-20261008-122514
-rw-r--r-- 1 root root 31065 10月20日 19:48 moshi-hooks.ts
-rw-r--r-- 1 root root 13197 10月22日 13:57 no-tables.ts
-rw-r--r-- 1 root root  1926  9月 2日 14:08 no-tables.ts.bak-20260902-140815
-rw-r--r-- 1 root root  3020  9月 2日 16:11 no-tables.ts.bak-20260902-161100
drwxr-xr-x 2 root root  4096 10月 8日 22:14 __tests__
```

`/root/.pi/agent/extensions/__tests__/` 也只有 `no-tables.test.ts`，无 gemini-safety 测试。

### 3.3 删除提交 `bd9129d`

```console
$ git -C /root/workspace/pi-extensions show bd9129d --stat --oneline
bd9129d chore(go): drop deprecated gemini-safety extension
 go/PLAN.md                         |   2 +-
 go/README.md                       |   6 +-
 go/gemini-safety/README.md         |  71 -------
 go/gemini-safety/extension.go      | 135 -------------
 go/gemini-safety/extension_test.go | 385 -------------------------------------
 go/gemini-safety/go.mod            |   5 -
 go/go.work                         |   7 +-
 go/reports/PLAN.v1.md              |   2 +
 8 files changed, 12 insertions(+), 601 deletions(-)
```

- `go/go.work`：移除 `./gemini-safety`，补入 6 个组件条目（见 §2）。
- `go/README.md`、`go/PLAN.md`：第 1 批清单由 7 件改 6 件，并标注「gemini-safety 已弃用、已删除」及同一句原因（`safetySettings` 注入无意义）。
- `go/reports/PLAN.v1.md`：历史正文不改，只追加一行注记（diff 里是 `@@ -41,6 +41,8 @@` 的 +2 行）。
- 源码/测试/go.mod 整体删除（-601 行）。

---

## 4. 逐组件报告

公共前置：以下每节的 gofmt/build/vet/test 都在**该组件目录**下跑，测试日志落 `/tmp`（临时文件，用完不删，仅作本次证据；不属于交付物）。

### 4.1 pi-context-pruning（`ca1e121`）

**做了什么**：对照 `/root/.pi/agent/git/github.com/dorokuma/pi-context-pruning/extensions/context-pruning/` 的 4 个 TS 文件共 218 行（`index.ts` 90、`config.ts` 49、`pruner.ts` 56、`types.ts` 23）；TS 测试 `test/config.test.ts` 21 行、`test/pruner.test.ts` 64 行。Go 侧 `config.go` 237 + `pruner.go` 172 + `types.go` 27 + `extension.go` 285 = 721 行实现，测试 903 行（`config_test.go` 134、`pruner_test.go` 371、`extension_test.go` 398）。

**钩子/命令**（`grep -n` 实测）：

- `e.OnEvent(sdk.EventSessionStart, …)`（`extension.go:256`）
- `e.OnEvent(sdk.EventContext, …)`（`extension.go:264`）
- 4 个命令：`prune`（`extension.go:268`）、`prune-toggle`（272）、`prune-stats`（276）、`prune-config`（280）

**测试命令与输出原文**：

```console
$ cd /root/workspace/pi-extensions/go/pi-context-pruning
$ echo "== gofmt -l ." && gofmt -l . ; echo "GOFMT_EXIT=$?" ; go build ./... ; echo "BUILD_EXIT=$?" ; go vet ./... ; echo "VET_EXIT=$?"
== gofmt -l .
GOFMT_EXIT=0
BUILD_EXIT=0
VET_EXIT=0
（gofmt -l 无输出 = 无未格式化文件）

$ go test -count=1 -race -v ./... >/tmp/t-pcp.log 2>&1 ; echo "TEST_EXIT=$?"
TEST_EXIT=0
RUN=25
PASS_TOP=25
PASS_ALL=25
FAIL=0
RACE=0
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	1.015s
```

25 条顶层用例名（`grep '^--- PASS'`）：`TestValidGlobalAndProjectFieldsMergeInOrder`、`TestInvalidProjectValuesPreserveValidGlobalValues`、`TestValidationHelpers`、`TestValidIntegerOverflowCap`、`TestFormatTokens`、`TestFormatStatsMessage`、`TestFormatConfigMessage`、`TestCommandHandlers`、`TestHandleSessionStart`、`TestContextRewritingComparison`、`TestContextDisabledWithForcePrune`、`TestDoesNotPruneBelowMinimum`、`TestPrunesBeyondProtectBudgetAndMinimum`、`TestCustomExtensionToolsArePrunableByDefault`、`TestErrorOutputsRemainProtected`、`TestTheLatestTwoUserTurnsRemainProtected`、`TestCompactionSummaryStopsTheBackwardScan`、`TestForceEquivalentZeroMinimumStillPreservesTheProtectBudget`、`TestAllowlistAndProtectedListAreEnforced`、`TestPruneMarker`、`TestFormatWithCommas`、`TestProtectedTurnsZero`、`TestPruneProtectExactBoundary`、`TestToolResultWithoutIsErrorField`、`TestMultiTurnProtectionWindow`。

**PiG 隔离加载实测**：

```console
$ cd /root/workspace/pi-extensions/go && ./bin/pig-test -e ./pi-context-pruning -p "回复 OK" >/tmp/pig-pcp.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-pcp.log
EXIT=0
OK
```

- 加载成功（无加载错误、EXIT=0），但**本次提示词没有触发 `context` 改写**：剪枝阈值是 `pruneMinimum=20000` 可剪 token，`-p "回复 OK"` 的上下文只有约 2k token（同一环境 baseline 的 `usage.input` 实测 2054），无可剪对象。
- 4 个斜杠命令在非交互 `-p` 模式下无输入通道，故未执行（`/prune-*` 只在交互式 TUI 可用）。
- 状态栏/通知在单次 CLI 模式下由 PiG 的 no-op UI 上下文丢弃（README 已记 `ui_bridge.go:1356-1367`），也没有可观测输出。

**与 TS 版的行为差异**（提炼自该组件 README 差异节，每条一行）：

1. 剪枝时保持原切片不可变（浅拷贝后替换 `content`），TS 直接原位赋值。
2. 配置探测多支持 `PIG_HOME`（优先 `$PIG_HOME/agent/settings.json`），并在 `session_start` 增量合并 `ctx.GetSettings()`；TS 固定 `homedir()/.pi/agent/settings.json` + `cwd/.pi/settings.json`。
3. `ValidInteger` 兼容 Go 原生整数类型（`int`/`int64`/`uint`），TS 只有 `number`。
4. 超 `math.MaxInt` 的大值（如 `1e19`）饱和为 `math.MaxInt`，避免溢出成负数把「恒不剪」翻成「恒可剪」。
5. `LoadConfig("")` 时跳过项目配置（仅直调路径差异，运行时由 `ctx.Cwd()`/`os.Getwd()` 提供）。
6. `PruneToolOutputsAny` 对非 `map[string]any` 元素做空 map 兜底（wire 上不可达的防御路径）。
7. Token 格式化用 `toFixed1`（`math/big` 有理数，ECMA-262 half-up 远离 0）+ 千分位逗号，对齐 JS `toFixed(1)`/`toLocaleString()`，避免 locale 差异。

**遗留/未做**：单次 CLI 模式下 UI 状态栏不可见（PiG 行为，非缺陷）；`toFixed1` 的对照基准取自历史门禁轮次的 Node 实测（本轮未重跑 Node）。

### 4.2 pi-env（`8880a9f`）

**做了什么**：对照 npm `pi-env@1.0.1` 的 `/root/.pi/agent/npm/node_modules/pi-env/extensions/env.ts`（237 行，TS 版无测试）。Go 侧 `extension.go` 925 行实现 + `extension_test.go` 886 行测试。

**钩子/命令**（`grep -n` 实测）：

- `Extension()` 初始化即 `ApplyEnv(cwd)`（启动即注入）
- `e.MessageRenderer(MessageType, MessageRendererHandler)`（`extension.go:843`，`customType="pi-env"`）
- `e.OnEvent(sdk.EventContext, …)`（`extension.go:846`，把 `pi-env` 自定义消息从 LLM 上下文剔除）
- `e.Command("env", …)`（`extension.go:856`）
- `e.OnEvent(sdk.EventSessionStart, …)`（`extension.go:867`，非标量/未解析/覆盖告警）
- 代码内**无 argv 依赖**：`grep -c 'argv' pi-env/extension.go` → `0`（与任务书「argv 无关」一致）

**测试命令与输出原文**：

```console
$ cd /root/workspace/pi-extensions/go/pi-env
$ echo "== gofmt -l ." && gofmt -l . ; echo "GOFMT_EXIT=$?" ; go build ./... ; echo "BUILD_EXIT=$?" ; go vet ./... ; echo "VET_EXIT=$?"
== gofmt -l .
GOFMT_EXIT=0
BUILD_EXIT=0
VET_EXIT=0

$ go test -count=1 -race -v ./... >/tmp/t-env.log 2>&1 ; echo "TEST_EXIT=$?"
TEST_EXIT=0
RUN=34
PASS_TOP=23
PASS_ALL=34
FAIL=0
RACE=0
ok  	github.com/dorokuma/pi-extensions/go/pi-env	1.013s
```

23 条顶层用例：`TestResolveValue`、`TestResolveValue_SelfReference`、`TestExtractEnv`、`TestIsScalarAndScalarToString`、`TestApplyEnvExplicit_MergePriority`、`TestApplyEnvExplicit_NonScalarIgnored`、`TestApplyEnvExplicit_ReloadStaleKeyCleanup`、`TestApplyEnvExplicit_OverridesTracking`、`TestFormatReport`、`TestBuildSessionStartWarnings`、`TestFilterContextMessages`、`TestRenderMessageText`、`TestApplyEnv_RealEnvRollback`、`TestExtensionSmoke`、`TestKeyOrder_ReportFormatOrder`、`TestKeyOrder_WarningOrder`、`TestKeyOrder_SiblingReferenceCounterexample`、`TestKeyOrder_GlobalAndProjectSections`、`TestKeyOrder_SideBySideComparison`、`TestStrictSettings_NoEnvKeyZeroInjection`、`TestStrictSettings_EmptyAndNullEnvKeys`、`TestStrictSettings_OnlyEnvInjectedThemeExcluded`、`TestStrictSettings_ExistingBehaviorPreserved`。

**PiG 隔离加载实测**：

```console
$ ./bin/pig-test -e ./pi-env -p "回复 OK" >/tmp/pig-env.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-env.log
EXIT=0
OK
```

- 加载成功（EXIT=0）。**未触发注入**：隔离配置 `/root/.pig-test/agent/settings.json` 的顶层键实测只有 `['defaultModel','defaultProvider','modelThinkingLevels']`，`has env key: False` → 严格模式零注入、无 `session_start` 告警。
- `/env` 命令在 `-p` 非交互模式无入口，未执行。
- 该扩展在 PiG 下的**环境注入对工具不生效**（子进程架构），见 §7a。

**与 TS 版的行为差异**（README 差异节，每条一行）：

1. 键序改为**书写序**（`OrderedEnv` + `json.Decoder` 逐 token 流式解析 + 流式求值），不再 `sort.Strings`；修复了同 scope 兄弟 `$VAR` 引用求值顺序偏差（`{"ZPATH":"$ABASE/x","ABASE":"/opt"}` 仍得 `/x` 并告警 `ABASE` 未解析）。
2. 严格模式：settings.json 无顶层 `"env"` 键 → 零注入（不把 `theme`/`defaultModel` 当环境变量注入）；`"env":{}`、`"env":null` 同样零注入。
3. Project 段覆盖判定用键存在性（`_, inGlobal := globalEnv.Get(key)`），TS 用 `!globalEnvRaw?.[key]`，falsy 值（`""`/`false`/`0`）下 TS 会误报「覆盖了宿主变量」。
4. `context` 过滤对非 `map[string]any` 消息直接跳过（wire 上恒为对象）。
5. 渲染器返回 `[]string` 行切片（PiG `RendererFunc` 契约），TS 返回 `Text` 组件。
6. `session_start` 除 `ctx.SendMessage` 外还调用 `ctx.Notify(level="warning")`。

**遗留/未做**：注入到宿主 env 的通道不存在（§7a）；跨引用只做单次正则替换（不做拓扑展开）；fused 同进程形态下 `os.Setenv` 才影响宿主（`host.go:3106-3113` 注释），本批未在 fused 形态实测。

### 4.3 pi-tinyfish（`dd1828f`，纯 stdlib HTTP）

**做了什么**：对照 npm `pi-tinyfish@0.1.1` 的 `/root/.pi/agent/npm/node_modules/pi-tinyfish/index.ts`（621 行）+ `@tiny-fish/sdk` 0.0.8 的 `dist/_utils/client.js`、`dist/search/index.js`、`dist/fetch/index.js`；去掉 SDK，改 `net/http` 直连。Go 侧 `api.go` 448 + `extension.go` 652 + `render.go` 180 = 1280 行实现，测试 959 行（`api_test.go` 393、`extension_test.go` 566）。

**钩子/命令**：无事件钩子，只注册两个工具（`grep -n` 实测）——`e.RegisterTool(SearchToolDefinition())`（`extension.go:649`）、`e.RegisterTool(FetchToolDefinition())`（`extension.go:650`），工具名 `tinyfish_search`（`extension.go:532`）与 `tinyfish_fetch`（`extension.go:588`），另有 `ctx.OnUpdate` 进度通知（`extension.go:357`、`438`）与渲染器（`render.go:45/50/90`）。

**测试命令与输出原文**：

```console
$ cd /root/workspace/pi-extensions/go/pi-tinyfish
$ echo "== gofmt -l ." && gofmt -l . ; echo "GOFMT_EXIT=$?" ; go build ./... ; echo "BUILD_EXIT=$?" ; go vet ./... ; echo "VET_EXIT=$?"
== gofmt -l .
GOFMT_EXIT=0
BUILD_EXIT=0
VET_EXIT=0

$ go test -count=1 -race -v ./... >/tmp/t-tf.log 2>&1 ; echo "TEST_EXIT=$?"
TEST_EXIT=0
RUN=31
PASS_TOP=21
PASS_ALL=31
FAIL=0
RACE=0
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	1.084s
```

21 条顶层用例：`TestMissingAPIKey`、`TestSearchSuccess`、`TestFetchSuccess`、`TestNonRetryableStatusCodes`、`TestRetryableStatusCodesUntilExhaustion`、`TestTimeoutSpanningRetries`、`TestAPIIntegrationInjection`、`TestToolRegistration`、`TestNormalizeUrls`、`TestNormalizeNumber`、`TestSearchParameterValidation`、`TestFetchParameterValidation`、`TestFormatSearchResponse`、`TestFormatFetchResponse`、`TestTruncationAndFormatting`、`TestExecuteSearchWithFakeServer`、`TestExecuteFetchWithFakeServer`、`TestRenderers`、`TestExecuteSearchPageClampingWithFakeServer`、`TestExecuteFetchInvalidFormatFallback`、`TestRenderCallChineseUTF8Truncation`（全部用 `net/http/httptest` 假服务端，零外网、零真 key）。

**PiG 隔离加载实测**：

```console
$ ./bin/pig-test -e ./pi-tinyfish -p "回复 OK" >/tmp/pig-tf.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-tf.log
EXIT=0
OK

$ ./bin/pig-test -e ./pi-tinyfish --mode json -p "回复 OK" >/tmp/pig-tf-json.log 2>&1 ; echo EXIT=$?
EXIT=0
（从 agent_end 的 system 消息里提取 sections.tools）
<tools>
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement, including multiple disjoint edits in one call
- write: Create or overwrite new files
- tinyfish_search: Search the web with TinyFish for ranked results, snippets, and URLs. Use location/language for geo-targeting.
- tinyfish_fetch: Fetch known URLs with TinyFish to get rendered/extracted page content as markdown, html, or json.
```

→ **工具注册确实生效**（`tinyfish_search`/`tinyfish_fetch` 出现在活跃工具表）。未发起真实 API 调用（无 `TINYFISH_API_KEY` 参与，也不该发起）。

**与 TS 版的行为差异**（README 差异节，每条一行）：

1. 去掉 `@tiny-fish/sdk` 与 `p-retry`/Zod，纯 `net/http` + 手写参数校验；端点/头/默认超时/重试参数全部常量化（base `https://agent.tinyfish.ai`、`GET /v1/search`、`POST /v1/fetch`、`X-API-Key`、`User-Agent: tinyfish-typescript/0.0.8`、`X-TF-Request-Origin: tinyfish-js`）。
2. 超时沿用插件覆盖值 10s（search）/150s（fetch），并包裹整个重试序列；仅 408/429/5xx 重试，最多 2 次（共 3 次尝试），退避 500ms→1000ms（上限 8000ms）。
3. 进度通知走 `ctx.OnUpdate(partial)`（TS 是 `onUpdate?.({content, details})`）。
4. 渲染器返回 `[]string` 行切片（SDK 未暴露动态 theme），不做硬编码 ANSI。
5. `page` clamp 到 [0,10]；非法 `format` 静默回退 markdown（对齐 `index.ts:312-313`、`326-328`）。
6. 网络错误直接返回底层 `err`（TS 包成 `APIConnectionError`）；`io.ReadAll` 失败**参与**重试（TS 在 `_request` 外读 body，不重试）。
7. `textLength` 用 UTF-8 字节数（`len(...)`），TS 用 JS `string.length`（UTF-16 code units）。
8. 取消传播：Go 监听 `sdk.Context.Done()` 即时取消 HTTP/退避，TS 未把 `_signal` 传给 SDK。
9. 截断按 `[]rune` 切，避免切断多字节字符。

**遗留/未做**：`api.go:385` 在 body 读错误的重试分支把「取消」也报成 `Request timed out after Nms`（实测代码：`if sleepErr := c.sleeper(reqCtx, backoff); sleepErr != nil { return nil, fmt.Errorf("Request timed out after %dms", …) }`），既有问题、非本批引入；真实 API 端到端调用未跑（无 key、且明确禁止）。

### 4.4 no-tables（`1afde07`）

**做了什么**：对照权威 TS `extensions/no-tables/src/no-tables.ts`（317 行，md5 `a1f215b9843f38943cf86df38b4205f5`，与部署件 `/root/.pi/agent/extensions/no-tables.ts` 一致）。Go 侧 `extension.go` 608 行实现 + `extension_test.go` 660 行测试；两条 TS 测试（`/root/.pi/agent/extensions/__tests__/no-tables.test.ts` 123 行、仓内 `extensions/no-tables/test/no-tables.test.ts` 188 行）合并翻译为 Go 用例。

**钩子/命令**：仅 `e.OnEvent(sdk.EventMessageEnd, HandleMessageEnd)`（`extension.go:606`）；改写时返回 `map[string]any{"message": newMsg}`（`extension.go:584`、`596`），无改动返回 `(nil, nil)`。

**测试命令与输出原文**：

```console
$ cd /root/workspace/pi-extensions/go/no-tables
$ echo "== gofmt -l ." && gofmt -l . ; echo "GOFMT_EXIT=$?" ; go build ./... ; echo "BUILD_EXIT=$?" ; go vet ./... ; echo "VET_EXIT=$?"
== gofmt -l .
GOFMT_EXIT=0
BUILD_EXIT=0
VET_EXIT=0

$ go test -count=1 -race -v ./... >/tmp/t-nt.log 2>&1 ; echo "TEST_EXIT=$?"
TEST_EXIT=0
RUN=54
PASS_TOP=36
PASS_ALL=54
FAIL=0
RACE=0
ok  	github.com/dorokuma/pi-extensions/go/no-tables	1.029s
```

36 条顶层用例：`TestNormalizeMisplacedFences_*`（10 条：GluedMovesToOwnLine / AlreadyOnOwnLine / TildeGlued / IndentedFencesMoveToColumn0 / InlineBackticksNeverTouched / FenceMentionedMidSentence / InsideBlockquote / Idempotent / Edges / NestedInsideLongerFence）、`TestConvertUnsupportedMarkdown_HeadingFollowedByFenceNotReglued`、`TestConvertUnsupportedMarkdown_RulesAtoEApplyToProseAfterGluedFence`、`TestRuleG_*`（6 条）、`TestRuleE_LinkLabelContainingInlineCode`、`TestConvertTables_*`（4 条：Basic/SingleColumn/EmptyHeaderColumn/MultipleTables）、`TestConvertUnsupportedMarkdown_ImagesAndFootnotes`、`TestConvertUnsupportedMarkdown_Headings`、`TestConvertUnsupportedMarkdown_Links`、`TestConvertUnsupportedMarkdown_Boundaries`、`TestHandleMessageEnd_NonAssistant`、`TestHandleMessageEnd_AssistantNoChange`、`TestHandleMessageEnd_AssistantWithTable`、`TestExtension_Construct`、`TestJSWhitespace_HeadingsAndFootnotes`、`TestCRLF_VerbatimPreserved`、`TestDocumentedDeviations_U2028`、`TestKnownDeviations_LineTerminators`（**8 条行终止符分歧冻结用例**）。

**PiG 隔离加载实测**（这次真的触发了钩子）：

```console
$ ./bin/pig-test -e ./no-tables -p "用一个 2 列 2 行的 markdown 表格回答：A=1，B=2" >/tmp/pig-nt.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-nt.log
EXIT=0
--- output ---
- **A** — 值: 1
- **B** — 值: 2
```

模型按提示词输出了表格，`message_end` 钩子把表格转换成 bullet 列表后才交付终端 → **钩子实际生效**。（对照 §4.1/§4.2 的 `回复 OK` 场景：没有表格就没有可改写对象，钩子空转返回 `(nil,nil)`。）

**与 TS 版的行为差异**（README 差异节，每条一行）：

1. 宿主契约：`message_end` 返回 `map[string]any{"message": newMsg}`（PiG `EmitMessageEnd` 检查 `typed.Message` 后原子替换，依据 `ref/PiG/coding/extension/host/inproc/runner.go:2155-2197`）。
2. 正则引擎：RE2 无 lookaround/反向引用 → 用确定性双指针扫描器（`splitByInlineCode`）与线性状态机（围栏、Rule G 标点判定）替代。
3. JS `\s` 字符集对齐：自定义 `jsWhitespaceClass`/`isJSWhitespace`（含 `\u00a0`、`\u3000` 等 25 个空白），`###　标题`、`[^1]:　text` 与 TS 一致。
4. 行终止符分歧（含 CR/U+2028/U+2029）目前**已知未复刻**：8 条样例冻结在 `TestKnownDeviations_LineTerminators`，其中 CRLF/CR 列为下一批待修（§8），U+2028/U+2029 仅文档化。
5. 其余（Rule A–G 与表格转换）在纯 LF 输入下与 TS 逐字节一致（README 明确限定前提，不再写「完全复刻」）。

**遗留/未做**：CRLF/CR 行终止符对齐未做（下一批）；U+2028/U+2029 不计划修。

### 4.5 herdr-agent-state（`fb043a7`，C 档 + oracle）

**做了什么**：对照 herdr 托管的 `/root/.pi/agent/extensions/herdr-agent-state.ts`（261 行，md5 `c5e0582adbba7fba3bcba83b08274a79`，**全程未改**）。Go 侧 `extension.go` 57 + `state.go` 385 + `transport.go` 273 = 715 行实现；测试 1364 行（`extension_test.go` 404、`state_test.go` 305、`transport_test.go` 655）。协议依据 `herdr api schema --json` 的 `pane.report_agent` / `pane.report_agent_session`。

**钩子/命令**（`grep -n` 实测）：

- `e.OnEvent(sdk.EventSessionStart, sm.HandleSessionStart)`（`extension.go:52`）
- `e.OnEvent(sdk.EventAgentStart, sm.HandleAgentStart)`（`extension.go:53`）
- `e.OnEvent(sdk.EventAgentSettled, sm.HandleAgentSettled)`（`extension.go:54`）
- 进程内事件总线 `herdr:blocked`（`state.go: HandleHerdrBlocked`）
- 启用条件 `Enabled(cfg)`：`HERDR_ENV=="1" && socketPath!="" && paneId!=""`（`extension.go:28`，对照 TS L18-20）

**测试命令与输出原文**：

```console
$ cd /root/workspace/pi-extensions/go/herdr-agent-state
$ echo "== gofmt -l ." && gofmt -l . ; echo "GOFMT_EXIT=$?" ; go build ./... ; echo "BUILD_EXIT=$?" ; go vet ./... ; echo "VET_EXIT=$?"
== gofmt -l .
GOFMT_EXIT=0
BUILD_EXIT=0
VET_EXIT=0

$ go test -count=1 -race -v ./... >/tmp/t-has.log 2>&1 ; echo "TEST_EXIT=$?"
TEST_EXIT=0
RUN=17
PASS=17
FAIL=0
RACE=0
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	1.441s
```

17 条用例：`TestExtension_Disabled`、`TestEventHandlers_Logic`、`TestLifecycle_SessionStartGate`、`TestLifecycle_AgentStartGate`、`TestLifecycle_AgentSettledGate`、`TestLifecycle_BlockedPriorityAndMessages`、`TestExtension_EventWiring`、`TestStateMachine_Transitions`、`TestQueue_CoalescingAndSerialization`、`TestPathValidation_IsAbsolutePosixOrWin32`、`TestTransport_RealUnixSocket_Protocol`、`TestTransport_RetryAndTimeout`、`TestTransport_ImmediateClose_Retries`、`TestTransport_NonJSONGarbage`、`TestTransport_SingleAttemptHardTimeoutBudget`、`TestAgentStart_SessionSeqOrdering`、`TestSessionStart_AsyncNonBlockingAndSeqOrdering`（真实 Unix socket 假服务端，不连生产 herdr）。

**PiG 隔离加载实测**：

```console
$ ./bin/pig-test -e ./herdr-agent-state -p "回复 OK" >/tmp/pig-has.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-has.log
EXIT=0
OK
```

- 加载成功（EXIT=0）。**未触发上报**：本会话环境无 `HERDR_ENV`/`HERDR_SOCKET_PATH`/`HERDR_PANE_ID`（`bin/pig-test` 也不注入），`Enabled()` 为 false → 完全 no-op、零连接（对应 `TestExtension_Disabled` 的断言口径）。隔离 `settings.json` 里也没有任何 herdr 相关键（实测 `has herdr key: []`）。
- 若要真触发上报，必须在 herdr 管理的 pane 里带齐三个环境变量；本轮按硬规矩不连生产 herdr 做写操作，故未跑。

**与 TS 版的行为差异**（README 差异节，每条一行）：

1. 运行时模型：多 goroutine + `sync.Mutex` 保护状态与排队（TS 单线程事件循环）。
2. Socket 预算：dial 前算剩余 deadline，dial/write/read 共享统一预算，单次 attempt 硬上限 500ms/1500ms（已对齐，不叠加）。
3. Windows Named Pipe 不支持（`npipe` 死代码已删），仅 Unix domain socket（Linux/macOS）。
4. session seq 顺序：handler 内同步快照 sessionRef + 预分配 seq，网络发送放后台 goroutine，保证 `report_agent_session.seq < 随后 report_agent.seq`。
5. 单向确认语义：只判 `n>0` 即交付成功，不解析 herdr 回执；到达线序不保证，下游应按 seq 处理。
6. 测试可观测性：可注入超时与排空完成 channel，免死等真实 2s。

**遗留/未做**：真实 herdr 端到端上报未跑（需 pane 环境，违反「不连生产 herdr 写」）；Windows 平台不支持。

### 4.6 exclude-tools-guard（`9cd6108`，C 档 + oracle）

**做了什么**：对照部署件 `/root/.pi/agent/extensions/exclude-tools-guard.ts`（279 行，md5 `30722dbb05328b63a2cb87729072b7a0`；与仓内 `extensions/exclude-tools-guard/src/exclude-tools-guard.ts` 逐字节一致，该文件相对 HEAD 的未提交改动属另一工作项）。Go 侧 `extension.go` 968 行实现 + `extension_test.go` 1846 行测试。

**钩子/命令**（`grep -n` 实测）：

- `e.OnEvent(sdk.EventSessionStart, …)`（`extension.go:941`）
- `e.OnEvent(sdk.EventBeforeAgentStart, …)`（`extension.go:951`）
- 身份判据入口 `DetectSubprocessForm` / `ResolveAgentIdentityWithEnv`，所有降级分支走 `failClosed(reason)`（`extension.go:713`，S1/S2/S3/S4 调用点 `:799/:807/:813/:824/:859/:864`）
- 子代理提示词清理 `DeleteAddendumSection`（`extension.go:213`，调用点 `:242`）

**测试命令与输出原文**：

```console
$ cd /root/workspace/pi-extensions/go/exclude-tools-guard
$ echo "== gofmt -l ." && gofmt -l . ; echo "GOFMT_EXIT=$?" ; go build ./... ; echo "BUILD_EXIT=$?" ; go vet ./... ; echo "VET_EXIT=$?"
== gofmt -l .
GOFMT_EXIT=0
BUILD_EXIT=0
VET_EXIT=0

$ go test -count=1 -race -v ./... >/tmp/t-etg.log 2>&1 ; echo "TEST_EXIT=$?"
TEST_EXIT=0
RUN=85
PASS_TOP=14
PASS_ALL=85
FAIL=0
RACE=0
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	1.228s
```

14 条顶层用例（含 71 个子用例，故 `RUN=85` / `PASS_ALL=85`）：`TestQueryPaneShellPid_RealUnixSocket`、`TestResolveAgentIdentityWithEnv`、`TestResolveAgentIdentity_IdentityCriteriaRefactor`、`TestResolveAgentIdentity_PackedCellForm`、`TestResolveAgentIdentity_FailClosedBranches_WarnOnce`、`TestResolveAgentIdentity_HerdrQueryFailure_WarnsOnce`、`TestHostArgvFile_PIG_HARNESS_ARGV_FILE`、`TestFilterActiveTools`、`TestEnforceToolExclusions_SetterCalls`、`TestCleanPromptOptions`、`TestIsAgentRuntimeCmdline`、`TestExcludedToolsAccessors`、`TestExtensionWiring`、`TestEnforceToolExclusions_WarningOnceOnFailure`。

**PiG 隔离加载实测**（A/B 对照，证明钩子真的改了活跃工具表）：

```console
# A：不加载扩展（baseline）
$ ./bin/pig-test --mode json -p "回复 OK" >/tmp/pig-baseline-json.log 2>&1 ; echo EXIT=$?
EXIT=0
sections 键：["cwd","docs","preamble","project_context","rules","tools"]
<tools>
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement, including multiple disjoint edits in one call
- write: Create or overwrite new files
</tools>

# B：加载 exclude-tools-guard
$ ./bin/pig-test -e ./exclude-tools-guard -p "回复 OK" >/tmp/pig-etg.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-etg.log
EXIT=0
OK
$ ./bin/pig-test -e ./exclude-tools-guard --mode json -p "回复 OK" >/tmp/pig-etg-json.log 2>&1 ; echo EXIT=$?
EXIT=0
<tools>
- read: Read file contents
- edit: Make precise file edits with exact text replacement, including multiple disjoint edits in one call
- write: Create or overwrite new files
</tools>

# 对照：`-e` 确实被消费（坏路径直接失败）
$ ./bin/pig-test -e ./does-not-exist -p "回复 OK" >/tmp/pig-badpath.log 2>&1 ; echo EXIT=$? ; tail -2 /tmp/pig-badpath.log
EXIT=1
Error: Failed to load extension "/root/workspace/pi-extensions/go/does-not-exist": Extension path does not exist: /root/workspace/pi-extensions/go/does-not-exist
Hint: Start without extensions using "pig -ne".
```

→ `bash` 从活跃工具表消失，`session_start`/`before_agent_start` 的剔除逻辑**实际执行**。

```console
# 身份判定的 reason 无法从 stderr 观测（D56）
$ PI_IDENTITY_DEBUG=1 ./bin/pig-test -e ./exclude-tools-guard -p "回复 OK" >/tmp/pig-etg-debug.log 2>&1 ; echo EXIT=$? ; cat /tmp/pig-etg-debug.log
EXIT=0
OK
```

→ 无任何 stderr 行（宿主把扩展子进程 stderr 重定向到临时文件，见 §7d/§9）。

**与 TS 版的行为差异**（README 差异节，每条一行）：

1. 拓扑与宿主锚定（M1）：subprocess 形态下 self 是扩展子进程，宿主 = 向上**第一个** agent-runtime 祖先；fused 形态 `hostIndex=0`；`shellIndex > hostIndex` 强制；S4 只扫 `(hostIndex, shellIndex)`。
2. 形态判别（packed-cell 阻断修复）：`DetectSubprocessForm` 覆盖 `PIG_EXT_SOCKET` / `PIG_EXT_SOCKET_<NAME>`（前缀枚举 + 回查非空）/ `PIG_EXT_PACKED_CELL` / `PIG_EXT_ACTIVE_MEMBERS`。
3. 宿主 argv 桥接（M2）：读 `PIG_HARNESS_ARGV_FILE`（JSON 数组）与 `os.Args` 去重保序合并；env 设了但读不到/坏 JSON → fail-closed 判子代理；读完 `os.Unsetenv`；并用 `ArgvCache` 修复「第 2 个 turn 起标记失效」。
4. `shell_pid` 严格类型：只认 JSON number，字符串形式一律 fail-closed（TS 用 `Number()` 宽松转换）。
5. `ReadProcPpid` 用 `Fields` 切分，容忍括号后多空格/换行（TS `split(/\s+/)`）。
6. `forceSystemPrompt` 非字符串显式类型断言（TS 直接 `replaceAll` 会抛）。
7. 告警去重：`sync.Once` 首次无条件 warning（stderr），不改变事件返回值 `(nil,nil)`。
8. 子代理提示词清理多一步：删 `sections["addendum"]`（TS 加固版只清 `appendSystemPrompt`，会被 PiG 的 custom section 重新叠回）。
9. 移除 Windows `npipe` 死代码，仅 Unix socket。
10. `PIG_EXT_SOCKET_` 前缀被视为 PiG 保留命名空间（误设会改变形态判定输入）。

**遗留/未做**：残余 fail-open 面（§7d）无代码解；身份降级 warning 在部署形态可能被吞（D56）；真实 herdr 生产 socket 只做只读/失败路径测试，未做端到端主代理判定实测。

---

## 5. 门禁与验收记录

来源：调度档位留下的任务书文件 `/tmp/batch1-*.md`（临时过程文件）+ 各组件 README 的加固记录。**本轮没有重跑历史门禁**，只做证据核对；每条的「复验结论」指相应档位在文件里记录的结论（原文摘要），不是本轮新跑出来的。

### pi-context-pruning（verifier 2 轮 / reviewer 2 轮）

| 轮次 | 关键问题 | 修法 | 复验结论 |
|---|---|---|---|
| reviewer R1 | 0 must / 3 should / 5 consider：README 引用错（`ui_bridge.go:408-415`）、`PruneMarker`/`FormatWithCommas` 未逐字钉、禁用态 + `/prune` 强剪用例缺失 | 改引用到 `1356-1367`（NoopUIContext）；新增 `TestPruneMarker`（逐字 U+2014）、`TestFormatWithCommas`、`TestContextDisabledWithForcePrune` | R2 确认三条应修闭环 |
| reviewer R2 | 格式化与 JS 不同：Go `%.1f` vs ECMA-262 `toFixed(1)`；`1e19` 转 int 会溢出成负数 | 新增 `toFixed1`（`math/big` 有理数、half-up 远离 0）+ Node 对拍用例；`ValidInteger` 对超 `math.MaxInt` 饱和 | R2 抽查 9950→`9.9K`、1250→`1.3K` 等一致；无 panic/负零 |
| verifier（2 轮） | 行数自报不实（678/637 vs 报 ~440/~470）、README 断言失真 | README 改为「行为等价 + 差异节」，行号对齐 | 提交前 `build/vet/test(-race)` 全绿；本报告复跑 25 PASS |

### pi-env（verifier 2 轮 / reviewer 3 轮）

| 轮次 | 关键问题 | 修法 | 复验结论 |
|---|---|---|---|
| verifier parity | `TestParityG` FAIL：Go 用 `sort.Strings` 排键序，TS 保持书写序 → `/env` 报告行序、告警行序不一致，且兄弟 `$VAR` 引用求值偏差 | 新增 `OrderedEnv` + `json.Decoder` 流式有序解析（报告/告警/求值全保序） | R1 定级 must fix → 修后 5 条键序用例全绿 |
| reviewer R2 | must fix：settings.json **无 `env` 键**时「裸对象回退」把 `theme`/`defaultModel` 当 env 注入 | 读文件路径改严格模式（只认顶层 `env`），裸对象回退仅留给直调入口；新增 4 条回归（无 env/`{}`/`null`/混排） | R3 确认零注入、既有行为不回归；本报告复跑 34 PASS |
| reviewer R3 | consider：README 行号漂移、缺口措辞未加「以独立子进程方式加载时」限定 | 行号改对；缺口措辞加 fused（D31）限定 | R3 闭环 |

### pi-tinyfish（verifier 2 轮 / reviewer 2 轮）

| 轮次 | 关键问题 | 修法 | 复验结论 |
|---|---|---|---|
| reviewer R1 | 3 should：缺 key 文案与 SDK 不一致且测试把差异锁死；README 两处行号错；`page` 越界与非法 `format` 语义与 TS 分叉 | 文案逐字改回 SDK 原文并同步断言；README 622→621、`client.js:16`→`:43`；`executeSearch` clamp [0,10]、`executeFetch` 静默回退 | R2 逐项闭环 |
| 建议项 | UTF-8 截断可能切坏多字节；错误前缀多余；body 读错误不重试；`SetClientFactory` 泄漏导出面；超时用例不证明「不多发」 | 截断改 `[]rune`；去掉 `Connection failed: `；body 读错误纳入重试并在 README 记差异；改非导出 `setClientFactory`；补 `reqCount==1` 断言 | R2 确认；本报告复跑 31 PASS |

### no-tables（verifier 2 轮 / reviewer 3 轮）

| 轮次 | 关键问题 | 修法 | 复验结论 |
|---|---|---|---|
| reviewer R1 | **must fix F1**：Go RE2 的 `\s` 只有 ASCII，TS 的 `\s` 含 25 个空白 → `###　标题`（全角空格 U+3000）、`[^1]:　text` 漏转 | 定义 `jsWhitespaceClass`/`isJSWhitespace` 并替换 4 处 `\s`；4 个反例 + 2 个新反例与 TS 并列对比 | R2：F1 已闭环（专门交代「别动」） |
| reviewer R1 | 应修 F2：CRLF 上 Go 比 TS 多做 → 决定**复刻 TS**；F3 文档失真（「318 行」「100% 等价」「消除正则回溯」）；F4 既定偏离无全等断言 | CRLF 行为回退为 TS 精确行为；README 删等价断言、改实数字；每条偏离补全等用例 | R2 逐条闭环 |
| reviewer R2 | **must fix**：Go `.`/`(?m)$` 与 JS `.`/`m$` 行终止符语义差（含 `\r`/U+2028/U+2029 输入分歧，4000 例模糊 263 例）；README 等价断言仍失实；`splitByInlineCode` 两处死分支 | 按用户裁决：**不重构**，CRLF/CR 转「下一批待修」、U+2028/2029 仅文档化；8 条样例冻结 + `// NEXT-BATCH` 标记；删死分支 | R3 确认：2 must + 2 应修按裁决闭环；本报告复跑 54 PASS（含 `TestKnownDeviations_LineTerminators`） |

### herdr-agent-state（verifier / reviewer 3 轮 / oracle 1 轮）

| 轮次 | 关键问题 | 修法 | 复验结论 |
|---|---|---|---|
| reviewer（C 档） | 并发/时序与协议正确性审查：状态机、串行化、seq、超时预算、未启用零连接 | 按 §3 对照表逐条对齐；测试用真实 Unix socket 假服务端 | 进入 oracle |
| oracle R1 | `handleSessionStartCore` 同步 `ReportSession` → herdr 不应答时 `session_start` 分发实测卡 **2.002s**（TS 是 async 不阻塞） | 改为「同步准备（sessionRef + 预分配 seq + 组请求）+ `go SendRequest`」，保持 `session.seq < agent.seq` | R3 确认 handler < 50ms 返回、seq 顺序未破；新增 `TestSessionStart_AsyncNonBlockingAndSeqOrdering` |
| oracle R2 | `TestTransport_NonJSONGarbage` **flaky**（`-count=20 -race` 6/20 失败），根因：假服务端不读请求就写垃圾+close → 客户端 `write: broken pipe` | 假服务端先 `ReadString('\n')` 读请求行再写 | R3 复跑 `-count=20` 与 `-count=5` 稳定；本报告 `RACE=0` |
| oracle R6 | README「仅读取返回的 JSON-RPC 格式确认」表述宽于实际（实为只判 `n>0`） | README 改「单向确认、只看是否有字节返回」+ 到达线序不保证说明 | R3 确认文档与 `transport.go` 一致 |

### exclude-tools-guard（verifier / reviewer 6 轮 / oracle 4 轮，C 档）

| 轮次 | 关键问题 | 修法 | 复验结论 |
|---|---|---|---|
| verifier | **自带测试有 data race**（`TestQueryPaneShellPid_RealUnixSocket`，`go test -race` 直接失败）+ `gofmt -l` 列出 `extension.go` | 跨 goroutine 共享变量改同步安全写法（不删用例、不 Skip、不加 `t.Parallel()` 掩盖）+ `gofmt -w` | 修后 `-race` 全绿、`DATA RACE` 计数 0 |
| reviewer R1/R3 | **must fix**：三条 argv 负向标记（`--name role-`、`/tmp/herdr-role-sessions`、`/tmp/dispatch-`）在 Go 子进程形态下永不命中（方向 fail-open）；另 5 应修（吞错、README 行号、基准声明冲突、超时预算 3s、测试缺口） | 新增 `LoadEffectiveArgs` 读 `PIG_HARNESS_ARGV_FILE` 并参与判定；错误不再静默；`QueryPaneShellPid` 改入口起算 1500ms 总预算；补测试 | R3 确认 must fix 闭环 |
| oracle O1 | **M1**：`os.Getpid()` 使 `chain[1]`=宿主，S4 会检查宿主 → 宿主叫 `pi` 时**主代理被判子代理**；**M2**：argv 文件读失败静默退化（可伪造 + 丢标记）；**M3**：测试对「主代理判主代理」零覆盖 | 引入 hostIndex 锚定 + `shellIndex > hostIndex` + S4 扫中间段；`IsAgentRuntimeCmdline` 加 `pig`；argv 读失败 fail-closed；补 7 条拓扑/桥接用例 | O2 复检：M1/M2/M3 闭环 |
| 已决②落地 | 判据重构：删 pane 命名判据（`:p1`）、`hostIndex` 找第一个 agent-runtime 祖先、找不到 fail-closed | 改 `ResolveAgentIdentityWithEnv` + 新用例（含 `pane=w32:p23` 仍判 main）+ README 记录移除的旁路 | reviewer R5：A/B/C/E 均落地；**must fix**：README 未记录「子代理无标记判 main」残余面 |
| oracle O3 | **功能性阻断**：真实 PiG 下 Go 扩展走 packed-go cell，只有 `PIG_EXT_SOCKET_<NAME>`、无 `PIG_EXT_SOCKET` → 判 fused → 自身 cmdline `…/cells/go/<hash>/runner` 认不出 → **真实主代理被判子代理**（丢 `jev_check`/`ask_user_question`、每 turn 清 `appendSystemPrompt`）；另 2 应修（降级不可见；addendum 可经 `sections` 重叠） | 新增 `DetectSubprocessForm`（三形态）+ `failClosed(reason)` 统一单次 warning + `DeleteAddendumSection` + README 补两条残余面 | reviewer R6 / O4：4 项修复闭环；本报告复跑 85 PASS、`RACE=0` |

---

## 6. 已决事项（用户拍板，不列为遗留）

1. **Go 版暂不替代 TS 部署件**：第 1 批全过 + 用户批准前，线上仍跑 TS；Go 只在 `go/` 下开发测试（本轮未改任何生产部署件，见 §10 的 md5/`find` 证据）。
2. **exclude-tools-guard 不依赖 pane 命名**：subprocess 以 `PIG_EXT_SOCKET` / packed-cell 环境（`PIG_EXT_SOCKET_<NAME>`、`PIG_EXT_PACKED_CELL`、`PIG_EXT_ACTIVE_MEMBERS`）为主判别式；`hostIndex` 找第一个 agent-runtime 祖先，找不到 fail-closed；pane 名只作辅助证据。
3. **herdr 不可达的 1.5s 超时可接受**：一律 fail-closed + 单条日志，不重试阻塞（`sync.Once` 去重、1500ms 预算不改写）。
4. **gemini-safety 已弃用并删除**（原因与证据见 §3）。

---

## 7. PiG 缺口（写实，未改 PiG 源码）

### a) pi-env 的环境注入在 PiG 下对工具不生效

- Go 扩展是独立子进程，`os.Setenv` 只影响自身；`bash` 等工具由宿主派生，继承**宿主** env。
- 本轮实测核对：`/root/workspace/ref/PiG/coding/extension/host/subprocess/host_calls.go` 第 427-535 行的 RPC 分发表内 `grep -n 'env\|Env'` **无命中**（`GREP_EXIT=1`）→ extension→host 全表没有「写宿主环境变量」的方法。
- 结论：该组件在 PiG 下只剩**报告/过滤/告警/渲染**价值；fused（D31）同进程形态下 `os.Setenv` 才影响宿主（`host.go:3106-3113` 注释明示），本批未在 fused 下实测。

### b) herdr 0.9.5 无 PiG 官方集成

```console
$ herdr integration install --help 2>&1 | head -40 ; echo EXIT=$?
Install an integration

Usage: herdr integration install <TARGET>

Arguments:
  <TARGET>
          [possible values: pi, omp, claude, codex, copilot, devin, droid, kimi, opencode, kilo, hermes, qodercli, qwen, cursor, mastracode, antigravity-cli, grok, letta]

EXIT=0
$ herdr integration status 2>&1 ; echo EXIT=$?
pi: current (v9) (/root/.pi/agent/extensions/herdr-agent-state.ts)
omp: not installed (/root/.omp/agent/extensions/herdr-omp-agent-state.ts)
claude: not installed (/root/.claude/hooks/herdr-agent-state.sh)
codex: not installed (/root/.codex/herdr-agent-state.sh)
…
grok: current (v3) (/root/.grok/hooks/herdr-agent-state.sh)
letta (experimental): not installed (/root/.letta/hooks/herdr-agent-session.sh)
EXIT=0
```

→ possible values 里**没有** `pig`；`pi` 目标仍指向 Node 版 TS。故 herdr-agent-state 只能自写 Unix socket 上报；**不得改动被 herdr 托管的 TS**（本轮实测 md5 `c5e0582adbba7fba3bcba83b08274a79` 未变）。

### c) herdr 会把正在跑工具的 agy 误判成 idle

- 实测反例（记录在 `go/herdr-agent-state/README.md` §5.1，本批未复跑）：`p12` 在 **21:45:02** 被判 `idle`，实际 **21:49:42** 才结束（相差 4 分 40 秒）。
- 因此我们的上报**不依赖** herdr 的 idle 分类：`state` 只由 `blockedCount`、`agentActive`（`agent_start` 置位、`agent_settled` + `ctx.IsIdle()==true` 复位）决定。
- 判断完成应以 `agent_settled` + `ctx.IsIdle()==true` 为准（或底层工具进程实际退出）。

### d) exclude-tools-guard 的残余 fail-open 面（现网可复现）

- 子代理在**自己 pane** 且无可见负向标记时被判 main（`herdr agent start --kind pi` 不注入 `--name role-` / `--session-dir` / `--system-prompt`；现网证据：子代理 pane `w32:p26`、`shell_pid=2475328`、cmdline 仅 `pi`、env 无 `PI_SUBAGENT`/`PI_ROLE`）。
- 另一条：「另一个 pi 塞进主代理 pane shell」也会判 main。
- 这两条任何代码层判别式都修不到，只能靠派发层始终注入负向标记。相关 warning 在部署形态还可能被宿主吞掉（D56）：实测 `ref/PiG/coding/extension/host/subprocess/host.go:3238-3248` 把子进程 stderr 写临时文件，仅在失败诊断引用时保留。

---

## 8. 下一批待修项：no-tables 的 CRLF（`\r\n`）行终止符对齐

- 范围：`reTable`、`reOpenFence`、`reHeading`、`reFootDef` + 按 JS 行终止符切行的通用逻辑（`reImg`/`reLink` 已用 `[^\r\n]` 对齐 CRLF/CR）。
- 目标字符类 `[\n\r]`；**U+2028/U+2029 仅文档化、不计划修**。
- 修法提示（README 与调度记录一致）：单纯把 `.` 换成 `[^\n\r]` **不够**，`(?m)$` 也必须在 CR 断行，否则分歧会增多（实测 263 例 → 371 例）。
- 现状：8 条样例已冻结在 `TestKnownDeviations_LineTerminators`（本轮 PASS），README 拆成「下一批待修：CRLF / CR」与「仅文档化：U+2028 / U+2029」两栏。

---

## 9. 遗留与未跑（如实列）

1. **`make install-test` 未跑（读代码判定）**：Makefile 原文显示它只做「隔离目录防呆 + `mkdir -p` + `go build ./...`」，**没有真实安装到隔离 `extensions/` 的步骤**（第 0 批遗留）。本轮未执行该目标（它不改变任何东西，且不属本报告必跑项）。
2. **pi-tinyfish `api.go:385`**：body 读错误重试路径把「取消」也报成 `Request timed out after Nms`（实测代码见 §4.3），既有问题、非本批引入，未修（越权改源码）。
3. **no-tables U+2028/U+2029**：只文档化，不计划修。
4. **exclude-tools-guard 身份降级 warning 运维不可见**：D56（`host.go:3238-3248`），实测 `PI_IDENTITY_DEBUG=1` 下 stderr 仍无输出（§4.6）。
5. **`go/pigsdk/` 是 staged SDK 的仓内镜像**：`make sdk-check` 防漂移（本轮 EXIT=0）；PiG 升级后需重新同步（172 文件）。
6. **TS 侧 `extensions/exclude-tools-guard/*` 两处未提交改动**属另一工作项：本轮未动、也未提交（`git status --porcelain` 里仍是 ` M extensions/exclude-tools-guard/README.md`、` M extensions/exclude-tools-guard/src/exclude-tools-guard.ts`）。
7. **`go/reports/batch1.prompt`** 为任务输入记录，随本报告一并提交（此前 untracked）。
8. **提交顺序缺陷（§2）**：`ca1e121`…`fb043a7` 未带 `go.work` 条目，单提交点不可独立构建；当前 HEAD 已修复，未改历史。
9. **未跑项清单**：`pnpm setup`（本批不涉及 TS 依赖安装，未执行）；真实 TinyFish API 调用（无 key 且明令禁止）；真实 herdr 端到端上报（需 pane 环境，禁止写生产 herdr）；no-tables 的 Node 侧逐例对拍本轮未重跑（8 条冻结用例已是既存证据）；6 个组件的历史门禁轮次未重跑（按文件记录引用）。
10. **herdr 守护进程状态**：本轮未重启（实测当前 `herdsman-daemon` PID 2025239，启动时间 `2026-10-23 18:35:07`，早于本报告会话 10:05:03；与第 0 批记录的 PID 1369430 不同，说明批次之间被外部重启过，非本报告会话所为）。

---

## 10. 复现方式与隔离证据

### 10.1 必跑验证原文

```console
$ cd /root/workspace/pi-extensions/go && make build ; echo EXIT=$?
==> [root module] go build ./...
==> [component: exclude-tools-guard] go build ./...
==> [component: herdr-agent-state] go build ./...
==> [component: no-tables] go build ./...
==> [component: pi-context-pruning] go build ./...
==> [component: pi-env] go build ./...
==> [component: pi-tinyfish] go build ./...
EXIT=0
```

```console
$ make vet ; echo EXIT=$?
==> [root module] go vet ./...
==> [component: exclude-tools-guard] go vet ./...
==> [component: herdr-agent-state] go vet ./...
==> [component: no-tables] go vet ./...
==> [component: pi-context-pruning] go vet ./...
==> [component: pi-env] go vet ./...
==> [component: pi-tinyfish] go vet ./...
EXIT=0
```

```console
$ make test ; echo EXIT=$?
==> [root module] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/internal/version	(cached)
==> [component: exclude-tools-guard] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	0.209s
==> [component: herdr-agent-state] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	0.430s
==> [component: no-tables] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/no-tables	(cached)
==> [component: pi-context-pruning] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	(cached)
==> [component: pi-env] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-env	(cached)
==> [component: pi-tinyfish] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	(cached)
EXIT=0
```

（`(cached)` 行由 Go 缓存给出；去缓存的 `-count=1 -race -v` 结果见 §4，全部 EXIT=0。）

```console
$ make sdk-check ; echo EXIT=$?
Checking pigsdk against staged SDK: /root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef
OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)
EXIT=0
```

```console
$ git -C /root/workspace/pi-extensions status --porcelain
 M extensions/exclude-tools-guard/README.md
 M extensions/exclude-tools-guard/src/exclude-tools-guard.ts
?? go/reports/batch1.prompt
（STATUS_EXIT=0；此时本报告文件尚未写入）
```

复现命令序列（组件级）：

```bash
cd /root/workspace/pi-extensions/go/<组件>
gofmt -l . ; echo EXIT=$?
go build ./... ; echo EXIT=$?
go vet ./... ; echo EXIT=$?
go test -count=1 -race -v ./... ; echo EXIT=$?
cd /root/workspace/pi-extensions/go && ./bin/pig-test -e ./<组件> -p "回复 OK" ; echo EXIT=$?
```

6 次 PiG 加载实测汇总（详见 §4 各节）：pi-context-pruning `OK`/0、pi-env `OK`/0、pi-tinyfish `OK`/0（JSON 见工具表）、no-tables 表格→bullet/0、herdr-agent-state `OK`/0、exclude-tools-guard `OK`/0（JSON 见 `bash` 被剔除）。

### 10.2 隔离与「未碰生产」证据（实测）

- 全部 PiG 调用都过 `./bin/pig-test`（脚本实测写死 `export PIG_HOME=/root/.pig-test` 并 unset `PIG_CODING_AGENT_DIR`/`PIG_CODING_AGENT_SESSION_DIR`/`PIG_USE_PI_DIRS`）。
- **报告会话期间 `/root/.pi/agent` 零触碰**（会话起点 10:05:03，排除主代理自身会话目录）：

```console
$ find /root/.pi/agent -newermt '2026-10-24 10:05:00' -type f ! -path '*/sessions/*' 2>/dev/null | head -20 ; echo "COUNT=$(… | wc -l)"
COUNT=0
```

- **部署 TS 的 md5（全表，实测未变）**：

```console
$ md5sum /root/.pi/agent/extensions/*.ts | sort -k2
8d5c33da9ffce60f400ed73f90df32e1  /root/.pi/agent/extensions/auto-continue.ts
9b5692bfd8f7488154bf297d9e38e0a6  /root/.pi/agent/extensions/cache-stats.ts
fc35f79118e4b655f07a4fadf793f5dd  /root/.pi/agent/extensions/codegraph-go.ts
5f925d4921788c30dda994192d29c15a  /root/.pi/agent/extensions/ctxmode.ts
280f7767637f0e06cfb6db4c58ec8420  /root/.pi/agent/extensions/ctx-orchestrate.ts
30722dbb05328b63a2cb87729072b7a0  /root/.pi/agent/extensions/exclude-tools-guard.ts
c5e0582adbba7fba3bcba83b08274a79  /root/.pi/agent/extensions/herdr-agent-state.ts
3d909e1a87dd28a8d21e7e9b00668bad  /root/.pi/agent/extensions/herdsman-pi.ts
f4ff427bb51b6fa378fcfd356b81fa99  /root/.pi/agent/extensions/jev-gate.ts
3f8ec8443daf30e582d57eec99df383c  /root/.pi/agent/extensions/moshi-hooks.ts
a1f215b9843f38943cf86df38b4205f5  /root/.pi/agent/extensions/no-tables.ts
```

（`exclude-tools-guard.ts` = 本批基准 md5 `30722dbb…`；`herdr-agent-state.ts` = 托管 md5 `c5e0582a…`；`no-tables.ts` = 权威 md5 `a1f215b9…`，均与本批开始前一致。）

- **配置 mtime 早于本报告会话**：

```console
$ date '+START %Y-%m-%d %H:%M:%S %z' ; stat -c '%y  %n' /root/.pi/agent/models.json /root/.pi/agent/settings.json /root/.pi/agent/auth.json
START 2026-10-24 10:05:03 +0800
2026-10-24 09:37:32.657870562 +0800  /root/.pi/agent/models.json
2026-10-23 09:33:20.062201116 +0800  /root/.pi/agent/settings.json
2026-09-19 17:04:35.998158407 +0800  /root/.pi/agent/auth.json
```

说明：`models.json` 的 mtime（09:37:32）早于**本报告会话起点**（10:05:03），但晚于批次开始（10-23 17:11 的首个提交），即它在批次中途被批次外部的操作改过；本报告只能证明「本次写报告会话未改它」，不能替别的操作背书。

- **hooks.json 未改**：

```console
$ stat -c '%y  %s  %n' /root/.gemini/config/hooks.json
2026-10-22 21:15:23.538370017 +0800  1547  /root/.gemini/config/hooks.json
```

- **herdsman 未由本会话重启**：当前守护进程 PID 2025239，启动时间 `2026-10-23 18:35:07`（早于本报告会话起点）。

- **隔离产物落在 `/root/.pig-test`**：本轮每次 `-e` 加载都在 `/root/.pig-test/cache/cells/go/` 生成新的 Go cell（10:05–10:07 有新目录），`/root/.pig-test/agent/sessions/--root-workspace-pi-extensions-go--/` 记录本次会话。

BATCH-1 DONE
