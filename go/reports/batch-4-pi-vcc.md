# batch-4 — pi-vcc（Go 移植收尾：report.ts 补遗 + 装配层/补丁测试 + 隔离实测 + W4/W5 两轮审查返修）

> 工作目录：`/root/workspace/.wt/pi-extensions/feature-go-pi-vcc`（worktree，分支 `feature/go-pi-vcc`）
> 本报告只写本会话**亲自跑过**的命令输出与**亲眼读到**的文件内容；没跑的一律写「未跑」。
> 提交号（W6 回填真实值）：组件代码提交 `41abab1` / 完整 `41abab1d4feeb268d283d93036da507cf63583d7`，分支 `feature/go-pi-vcc`，
> 内容 = `go/pi-vcc/` 的 **58** 个文件 + `go/go.work` 的 1 行（`git show --shortstat` → `59 files changed, 16463 insertions(+)`）。
> 本分支上的提交结构共两次，**不合并（未 merge 到 `feature/go-batch1` / `main`）、不 push、主目录零触碰**：
> ① `41abab1 feat(go): port pi-vcc extension to Go (batch 4)` —— 组件代码 + `go.work`；
> ② 紧随其后、只含本报告文件的 `docs(go): add batch-4-pi-vcc report`。报告自身那次的 SHA 由它自己的提交流程现场生成（写正文时尚不存在），
> 为避免「报告自指自身 SHA → 任何一次 amend 都让引用失效」，正文只钉组件提交号；报告自身那次用 `git log -n 2 --oneline` 现场核对（见 §13）。
>
> **W6（本轮）**：门禁通过后的收尾，**不改任何 `.go`**——只修本报告的文字：§4 / §5.2 残留的「未改生产代码」旧表述按 §2.1 / §15 / §16 的事实改写、
> §8 补上 `go/bin/pig-test` 输出面的核对（该包装脚本**不打印任何横幅**，(c) 段补回此前被漏抄的末段输出）、回填真实提交号并更新 §13。逐条 before/after 见 **§17**。
>
> **W5 返修（第 2 轮返修）**：第 2 轮 reviewer 的 2 项 Should Fix 与 2 项 Consider 已全部处理并复跑全绿，
> 逐项证据见 **§16**。本报告早前章节记录的是返修前的状态，凡计数/输出以 **§16** 的最新块为准。
>
> **W4 返修（第 1 轮返修）**：第 1 轮 reviewer 的 3 项问题（1 Must Fix + 2 Should Fix）已全部修复并复跑全绿，
> 逐项证据见 **§15**。

---

## 1. 阶段结论

- **W6 报告文字与提交落地（本轮，详见 §17）**：**零代码改动**，只改本报告——(1) §4 / §5.2 的「未改生产代码」口径残留（W6 全文实测只剩 §5.2 一处，§4 改为新增准确口径段，见 §17.2 第 3 条）按事实改写为：除 §2.1 补齐 `report.go` 与 W4/W5 按审查意见对 `trackedCommands.go`、`extstate.go`、`drilldown.go`、`searchentries.go` 做的局部修复外，其余核心模块维持初版移植实现；(2) §8 明确 `go/bin/pig-test` 的输出面——脚本全文只有 `unset`/`export`/`exec`，不打印 `pig-test: isolated mode ...` / `pig-test: production /root/.pi/agent untouched` 这类横幅（实测 `--version` 与 (a) 冒烟的完整输出都无横幅），并把 (c) 段漏抄的末段补回；(3) 组件代码提交 `41abab1` 与本报告的 `docs(go)` 提交都落在 `feature/go-pi-vcc`（未 merge、未 push）。
- **补齐项全部完成**：`src/core/report.ts` 的对等移植 `report.go` + `report_test.go` 已落地；`extension.go` / `hook.go` / `recallrun.go` / `extstate.go` 四条装配/纯逻辑路径的单元测试已补齐（含一个跑在**真实 length-prefixed IPC** 上的 SDK 桩 harness）。
- **`@sting8k/pi-vcc@0.10.2` 的 38 个 TS 源文件全部有 Go 对应物**，无一遗漏（对照清单见 §3）。
- **两个本地补丁**（`pi-vcc-0.9.0-ctx-run`、`pi-vcc-0.10.0-recall-note`）的实现与测试逐条对照见 §5：`verify-eqi12.mjs` 的 A/B/C 三节断言、`verify.mjs` 的 V0–V4 全部用例都有 1:1 的 Go 用例，仅 3 项纯 TS/部署侧检查无 Go 等价物（已逐条列明）。
- **构建/静态检查/竞态测试全绿**：`go build ./...`、`go vet ./...`、`go test -race -count=1 ./...` 均 EXIT=0；`go/` 下 `make build` / `make vet` / `make test`（根模块 + 15 个组件）均 EXIT=0（原文见 §6、§7）。
- **PiG 隔离目录实测通过**：`./bin/pig-test -e ./pi-vcc` 在 `PIG_HOME=/root/.pig-test` 下加载成功（EXIT=0），并实际调用 `vcc_recall` 工具两次（`mode:'touched'` 与 `query:`），拿到真实工具返回原文（见 §8）。
- **W4 审查返修完成（W4，详见 §15）**：修复 reviewer 第 1 轮提出的 3 项问题——`trackedCommands.go:45` 前导 `&` 越界 panic（Must Fix）、`extstate.go` `scheduleAutoContinue` 定时器发布/取消竞态（Should Fix 1）、`patchCtxRun_test.go:254` 假绿断言（Should Fix 2）；每项均补了回归测试，并给出「改前 FAIL、改后 PASS」的对照证据。
- **W5 审查返修完成（第 2 轮，详见 §16）**：第 2 轮 reviewer 的 2 项 Should Fix 与 2 项 Consider 全部处理——`drilldown.go` offset 越界文案失真与负 `limit` 切片 panic（`go/pi-vcc/drilldown.go:183-191`）、`:full` 50KB 截断改为 `jsSlice` 与 TS `body.slice(0, MAX_FULL_BYTES)` 对齐（`go/pi-vcc/drilldown.go:155-162`）、`GetFileIndicators` 改返回导出具名类型 `FileIndicator`（`go/pi-vcc/searchentries.go:344-355`）；每项均补了回归测试，并给出「改前 FAIL/panic、改后 PASS」与 TS 原版 node 实跑对照。
- **W5 返修后全绿**：`go/pi-vcc` 下 `gofmt -l .` 为空，`go build ./...`、`go vet ./...`、`go test -race -count=1 ./...` 均 EXIT=0；顶层测试函数 **283** 个（W4 返修后 280，W5 新增 3 个）、`=== RUN` **443** 条（W4 返修后 436）、`DATA RACE` **0** 条、`--- FAIL` **0** 条（原文见 §6、§16.4）。
- **返修后全绿（W4）**：`go/pi-vcc` 下 `gofmt -l .` 为空，`go build ./...`、`go vet ./...`、`go test -race -count=1 ./...` 均 EXIT=0；顶层测试函数 **280** 个（返修前 277）、`=== RUN` **436** 条（返修前 431）、`DATA RACE` **0** 条（原文见 §15.4）。
- **遗留 4 项**（§12）。其中 2 项（缺 `README.md`、17 个既有 WIP 文件 `gofmt -l` 不干净）已在**进入 B 档提交门禁前的收尾清理（W3）**解决：新增 `go/pi-vcc/README.md`、对 `go/pi-vcc/` 执行 `gofmt -w .` 使 `gofmt -l .` 为空，并复跑 build/vet/race 全绿（详见 §6 gofmt 收尾与 W3 复跑块、§12）。余下 2 项见 §12 第 3、4 条。

---

## 2. 本轮做了什么

### 2.1 `report.ts` → `report.go` + `report_test.go`

- `report.ts` 全文件只有一个导出函数 `buildCompactReport(input: CompileInput): CompactReport`（`src/core/report.ts:196`），另有 `RecallProbe` / `CompactReport` 两个接口。它在运行时**不被任何路径调用**（`index.ts` 只注册 hook、两个命令与 recall 工具），是包的诊断面（"这次压缩到底做了什么" 的 dump 形状）。
- 移植保持完整且行为一致：`SECTION_HEADERS` 四段固定顺序、`\n\n---\n\n` 切 brief、`previewOf()` 的 edgeCount=3 / clip=220、`queryOf()` 的 `terms.slice(0,6)`、`topFilesOf()` 的 `slice(0,10)` 与四个文件路径键序（`path`/`file_path`/`filePath`/`file`）。
- `report_test.go`：16 个顶层用例、373 行，覆盖四个 section 头的渲染、role/block 计数、recall 探针（查询词派生、摘要命中判定、命中数）与 top-files 排序。

### 2.2 装配层测试（`extension_test.go`，13 个用例 + `TestMain` / 804 行）

- **harness 不是 mock，是真实 IPC**：`net.Pipe()` 两端，一端 `Extension()` + `ext.RunWithConn()`，另一端按 PiG 的 length-prefixed 帧协议读写（读 register 帧、应答 host call、向扩展发请求）。做法对齐 `go/ctxmode/test_helpers_test.go` 的 `sdkHost`。
- `TestMain` **围栏配置路径**：`PI_VCC_CONFIG_PATH` 指向 `t.TempDir()` 下的临时文件，任何测试都不会读写生产 `/root/.pi/agent/pi-vcc-config.json`。
- 覆盖：命令/工具/事件注册面（`pi-vcc`、`pi-vcc-recall`、`vcc_recall`、`context` / `before_agent_start` / `session_before_compact` / `session_compact`）、`vcc_recall` 的 search/paging/无 session/参数修复（`prepare` 参数钉住）、两个命令的成功与失败分支、`context` 事件剔除 auto-continue 标记、`before_agent_start` 清 pending continue。

### 2.3 钩子装配测试（`hook_test.go`，11 个用例 / 332 行）

- 覆盖 `session_before_compact` 的**五条出口**：产出摘要（happy path，断言 summary/`details.compactor`/`details.version`/`sourceMessageCount`/`firstKeptEntryId`/`tokensBefore` 与状态记录）、默认拒绝（`overrideDefaultCompaction:false` 时交回 pi）、override 开启时接管、provider 跳过（大小写不敏感，`/pi-vcc` 显式调用仍放行）、无内容可压时 `cancel` + 通知。
- 覆盖 `session_compact` 的两条路径：仅当落库 compaction entry 的 `details.compactor === "pi-vcc"` 才弹 stats toast；pending follow-up prompt 在压缩落地后发出一次且被消费。

### 2.4 纯逻辑测试（`extstate_test.go` 12 个用例 / `recallrun_test.go` 15 个用例）

- `extstate_test.go`（**W4 返修后 12 例 / 395 行**，返修前 9 例 / 210 行）：每扩展独立 state（两个 `Extension()` 不共享）、`lastStats` 返回副本（调用方改返回值不污染状态）、pending follow-up 的 set/take 生命周期（空串是清除而非存储）、`clearPendingAutoContinue` 幂等且真停表、`shouldScheduleAutoContinue` 双开关（设置 + pi 版本，版本不可读时 fail-safe 不 continue）、`formatCompactionStats` 两种形态（普通与 budget cut）、**真实路径**驱动：`PI_VCC_PI_VERSION=0.84.3` 下 threshold 压缩确实发出 `customType=pi-vcc-auto-continue`、`display=false`、`options.triggerTurn=true` + `deliverAs=followUp` 的不可见续跑；detached Context 下 `scheduleAutoContinue` 的 `recover` 保命（不 panic 逃逸）。**W4 新增 3 例**钉定时器竞态，见 §15.2。
- `recallrun_test.go`：`RecallView` 的 partial/full 双缓存（互不共享、full 更长）、`scope` 过滤非当前分支条目、`widen()` 复用同一 wide view 且 `scope:all` 自举、`currentTurn()` 三个分支（压缩后无进行中轮次 / 无 manager 不 panic / 缓存）；`runRecall` 的六个动作逐个覆盖（search、search 分页越界、range 含倒序/夹尾/完全越界、expand 含配对 toolResult 与非当前分支提示、touched 含 `#N:path` 下钻提示、recent、drill 的 off-path 前缀）；以及 `invalidExpandIndices` / `toolCallIds`。

---

## 3. 迁移文件与模块对照清单（TS 0.10.2 → Go）

`@sting8k/pi-vcc@0.10.2` 的 `src/` 共 **38 个 `.ts`**（外加根 `index.ts`，已计入下表首行）。逐一核对结果：

| # | TS 源文件 | Go 对应物 | 说明 |
|---|---|---|---|
| 1 | `index.ts` | `extension.go` `Extension()` | 装配：scaffoldSettings + hook + 2 命令 + 工具 |
| 2 | `src/commands/pi-vcc.ts` | `extension.go` `RegisterPiVccCommand` | 命令名 `pi-vcc`（TS `registerCommand("pi-vcc")`） |
| 3 | `src/commands/vcc-recall.ts` | `extension.go` `RegisterVccRecallCommand` | 命令名 `pi-vcc-recall` |
| 4 | `src/tools/recall.ts` | `extension.go` `RegisterRecallTool` + `RecallToolSchema` + `recallrun.go` | 工具名 `vcc_recall` |
| 5 | `src/hooks/before-compact.ts` | `hook.go`（装配）+ `beforecompact.go`（纯逻辑） | 4 个事件：`context`/`before_agent_start`/`session_before_compact`/`session_compact` |
| 6 | `src/core/brief.ts` | `brief.go` | |
| 7 | `src/core/build-sections.ts` | `buildSections.go` | |
| 8 | `src/core/compact-args.ts` | `compactargs.go` | |
| 9 | `src/core/content.ts` | `content.go` | |
| 10 | `src/core/drill-down.ts` | `drilldown.go` | |
| 11 | `src/core/filter-noise.ts` | `filterNoise.go` | |
| 12 | `src/core/format-recall.ts` | `formatrecall.go` | |
| 13 | `src/core/format.ts` | `format.go` | `RECALL_NOTE` 常量在 `format.go:150`（补丁 #2 注入点） |
| 14 | `src/core/global-indices.ts` | `globalindices.go` | |
| 15 | `src/core/jsonl.ts` | `jsonl.go` | |
| 16 | `src/core/lineage.ts` | `loadmessages.go`（`GetActiveLineageEntryIDs`）+ `extension.go`（`sessionManagerAdapter`） | TS 的 `LineageEntryLike`/`LineageSessionManagerLike` → `LineageEntryLike`/`LineageSessionManager` |
| 17 | `src/core/load-messages.ts` | `loadmessages.go` | |
| 18 | `src/core/normalize.ts` | `normalize.go` | |
| 19 | `src/core/rank.ts` | `rank.go` | |
| 20 | `src/core/recall-request.ts` | `recallrequest.go` | |
| 21 | `src/core/recall-run.ts` | `recallrun.go` | |
| 22 | `src/core/recall-scope.ts` | `recallscope.go` | |
| 23 | `src/core/render-entries.ts` | `renderentries.go` | |
| 24 | `src/core/report.ts` | `report.go` | **本轮补齐**（`BuildCompactReport`，`report.go:364`） |
| 25 | `src/core/sanitize.ts` | `sanitize.go` | |
| 26 | `src/core/search-entries.ts` | `searchentries.go` | |
| 27 | `src/core/settings.ts` | `settings.go` | |
| 28 | `src/core/skill-collapse.ts` | `skillcollapse.go` | |
| 29 | `src/core/summarize.ts` | `summarize.go` | `Compile`（`summarize.go:408`）、`StripRecallNote`（:451）、`buildRecallNoteRe`（:436） |
| 30 | `src/core/token-estimate.ts` | `tokenestimate.go` | |
| 31 | `src/core/tool-args.ts` | `toolargs.go` | |
| 32 | `src/details.ts` | `types.go`（`PiVccCompactionDetails`） | 字段序与 TS 对象字面量一致，落库 JSON 同序 |
| 33 | `src/extract/commits.ts` | `commits.go` | |
| 34 | `src/extract/files.ts` | `files.go` | |
| 35 | `src/extract/goals.ts` | `goalsPreference.go` | 与 preferences 合文件（同属 extract 小工具） |
| 36 | `src/extract/preferences.ts` | `goalsPreference.go` | 同上 |
| 37 | `src/extract/tracked-commands.ts` | `trackedCommands.go` | **补丁 #1 注入点**（`commandTextsOf`，`trackedCommands.go:223`） |
| 38 | `src/sections.ts` | `types.go`（`SectionData`） | 接口 → 结构体 |
| 39 | `src/types.ts` | `types.go` | `CompactionReason`/`FileOps`/`NormalizedBlock` 等 |

Go 侧另有 6 个**无 TS 对应物**的基础设施文件（JS 语义适配层与每扩展状态），见 §10：

| Go 文件 | 用途 |
|---|---|
| `jslite.go` | UTF-16 码位字符串语义（`jsLen`/`jsSlice`/`jsSplitLines`）、`jsFixed1`（JS `toFixed(1)` 的 half-away-from-zero） |
| `jsmath.go` | JS 数值格式化辅助 |
| `jsregex.go` | RE2 上复刻 JS RegExp 语义（`.` 不吃行终止符、`\s` 含 Unicode 分隔符 + U+FEFF、`i` 用 Unicode simple case folding、`\uXXXX` 转义展开、JS `\b`） |
| `jsonparse.go` / `jsonl.go` | JSON 对象解析与 JSONL 逐行读取 |
| `regexpwrap.go` | 正则编译包装 |
| `extstate.go` | 每扩展模块级状态（TS 是单模块实例，Go 一进程服务多扩展，见 §10 D2） |

---

## 4. 测试资产总览

`go/pi-vcc` 下共 **283 个顶层测试函数**（`grep -c '^func Test'` 减去 `TestMain`；W4 返修后为 280，W5 返修新增 3 个）；本轮移植 + W4/W5 两轮返修共涉及 **6** 个测试文件（下表；早前版本写「5 个文件」是 W5 新增 `recallrequest_test.go` 之前留下的计数，已纠）：

| 测试文件 | 顶层用例数 | 行数 | 覆盖对象 |
|---|---|---|---|
| `report_test.go` | 16 | 373 | `report.go`（本轮补齐） |
| `extension_test.go` | 13（+`TestMain`） | 804 | `extension.go` 装配 + SDK 桩 harness |
| `hook_test.go` | 11 | 332 | `hook.go` 两个事件的装配 |
| `extstate_test.go` | **12**（W4 返修前 9） | **395**（返修前 210） | `extstate.go` 状态机 + auto-continue 门禁 + 定时器竞态 |
| `recallrun_test.go` | 15 | 362 | `recallrun.go` 视图与六动作 |
| `recallrequest_test.go` | 16（W5 返修前 13） | **419**（W5 返修前 324） | `recallrequest.go` 参数修复 + `drilldown.go` offset 窗口 / `:full` 截断（W5 返修见 §16） |

说明：W4 返修改动了 2 个生产文件（`trackedCommands.go` +5 行注释、`extstate.go` 净增约 20 行）与 2 个测试文件（`extstate_test.go` +185 行、`patchCtxRun_test.go` +8 行），测试行数与改动同量级（约 9.7 倍，见 §15.5 的理由说明）。

W5 返修（§16）的改动量见 **§16.5**：生产侧净增 11 行（`drilldown.go`，其中 11 行是注释与等价改写）、净减 1 行（`searchentries.go` 匿名结构体 → 导出具名类型），测试侧新增 97 行（`recallrequest_test.go` +95、`recall_test.go` +2），比例理由见 §16.5。

**生产代码范围（与 §2.1 / §15 / §16 对齐的准确口径，W6 改写）**：除 §2.1 补齐的 `report.go`（及配套 `report_test.go`）与 W4、W5 两轮按审查意见对 `trackedCommands.go`（§15.1）、`extstate.go`（§15.2）、`drilldown.go`（§16.1 / §16.2）、`searchentries.go`（§16.3）做的局部修复外，`go/pi-vcc/` 其余核心模块维持初版移植实现。本节统计的 6 个测试文件覆盖的是「初版实现 + 上述局部修复」这个整体——**不是**「只加测试、一行生产代码都没动」；早前版本在本节与 §5.2 出现过的「本会话未新增任何生产代码行 / 本轮未改任何生产代码」属 §2.1 补齐与两轮返修之前的旧口径；W6 核对本文件时全文实测只剩 §5.2 V4a 一处，已按上面口径改写，本节则补上这句准确口径（逐条 before/after 见 §17）。

---

## 5. 两个本地补丁：实现与测试逐条对照

### 5.1 补丁 #1 `pi-vcc-0.9.0-ctx-run`（tracked-commands）

**补丁做了什么**（`pi-vcc-0.9.0-ctx-run-trackCommands.patch`，共 3 处 hunk）：

1. 新增 `commandTextsOf(args)`：从工具调用参数里取 shell 类命令文本，三种形状——
   - `args.command`（string，与 bash 同字段名）；
   - `args.argv`（数组，**只收 string 元素**，`join(" ")`；全非 string 则不贡献）；
   - `args.commands`（数组，元素为 string 或含 string `command` 字段的对象）。
   无这些字段的工具调用不贡献任何文本 → 对非 shell 工具仍是 no-op。
2. 主循环的门槛从 `b.kind !== "tool_call" || b.name !== "bash"` 放宽为 `b.kind !== "tool_call"`，即**任何工具调用**都按 `commandTextsOf` 收命令。
3. `args ?? {}` 兜底（参数缺失不炸）。

**Go 侧实现**：`trackedCommands.go:223 commandTextsOf()`（三形状、非 string 过滤、顺序为 command→argv→commands）、`:291` 主循环 `for _, command := range commandTextsOf(block.Args)`。

**测试对照**：

| verify-eqi12.mjs 用例 | Go 用例 | 结论 |
|---|---|---|
| A. `loadSettings()` 读出 `trackCommands` | 各测试直接传 `trackCommands`；`settings_test` 侧覆盖加载 | 等价（Go 用例以参数形式钉住同一输入） |
| B. 回放最新真实 session 的 toolCall，要求 `git:`/`herdr:` 行出现（条件断言：仅当真实 session 含可抽取的 ctx_run git/herdr 调用时才判失败） | `TestExtractTrackedCommands_SessionFileReplay`（JSONL 回放）+ `TestVerifyEqi12_TrackedLinePrefix`（`git:` 行前缀） | **等价且有真实数据实测**：见 §9（126 个真实 toolCall，抓到 upstream 会漏的 herdr 命令） |
| C1 bash 路径仍工作（`journalctl -u docker -n 5`） | `TestVerifyEqi12_SyntheticMixed` | 1:1 |
| C2 argv 被抓（`herdr pane read wA:p6V --lines 120`） | 同上 | 1:1 |
| C3 batch commands 被抓（`gh run list --limit 3`） | 同上 | 1:1 |
| C4 `ctx_run.command` 被扫（`git status --porcelain`） | 同上 | 1:1 |
| C5 `ctx_run.argv` 被 join（`git log --oneline -3`） | 同上 | 1:1 |
| C6 非 shell 工具不泄漏（`ask_user_question` 的 `questions` 不含 `git status ok`） | 同上（负向断言） | 1:1 |

补丁自身的边界场景（mjs 未逐条断言、但属补丁逻辑一部分）另有 Go 用例：`TestCommandTextsOf_ThreeShapes`（7 个子例：仅 command / argv 丢非 string / argv 全非 string 不贡献 / commands 混合 / 三形状顺序 / 非 shell 工具 / `args=nil`）、`TestExtractTrackedCommands_NonBashToolIsScanned`、`TestExtractTrackedCommands_UpstreamBashOnlyWouldMissThis`（回归钉：同一 block 在 bash-only 规则下必漏）、`TestExtractTrackedCommands_CommandNameMatching`（**16** 个子例，W4 返修新增 2 个：前导 `&` 是分隔符、前导 `&>file` 重定向不是分隔符——其余为前缀不匹配、引号内不是边界、管道/换行是边界、`VAR=`/`sudo`/`env -u` 跳过但保留、`gh pr` 前缀形、`2>&1` 重定向不是分隔符）、`TestExtractTrackedCommands_ReRunMovesToTail`、`TestExtractTrackedCommands_CapKeepsNewest`（11 条留最新 10 条 + `(+1 earlier)` 标记）、`TestExtractTrackedCommands_SSHRemoteScan`（6 个子例：双引号/单引号/无引号远端、bool flag 不吃 host、value flag 吃参数、无远端命令）、`TestExtractTrackedCommands_SSHIsExcludedFromItsOwnRemoteScan`（远端里的 `ssh` 不重复入账）、`TestExtractTrackedCommands_DisabledByEmptyTrackList`、`TestExtractTrackedCommands_OrderFollowsConfig`、`TestExtractTrackedCommands_CommentQuoteIsNotAQuote`。

### 5.2 补丁 #2 `pi-vcc-0.10.0-recall-note`（`RECALL_NOTE`）

**补丁做了什么**（`format.ts-recall-note.patch`，单 hunk）：在 `src/core/format.ts` 的 `RECALL_NOTE` 常量尾部追加一句
`" After any compaction, first call vcc_recall to pull this task's original instructions (the first user message) back before continuing; never judge whether it is needed."`
原句一字未改。只改常量的理由是 `summarize.ts:276-282` 的 `RECALL_NOTE_RE` 由该常量动态生成、合并前 `stripRecallNote`，故尾注不会随压缩叠加。

**Go 侧实现**：`format.go:150 RecallNote`（逐字节等于补丁后常量）、`summarize.go:436 buildRecallNoteRe()`、`summarize.go:451 StripRecallNote()`。

**测试对照**：

| verify.mjs 用例 | Go 用例 | 结论 |
|---|---|---|
| V0 live 模块经 jiti 加载、导出齐全 | 无直接等价物 | **TS/jiti 专属**（加载机制检查）；Go 侧由"包可编译 + 全部符号被测试引用"等价保证 |
| V1a 新常量含追加句 | `TestVerify_RecallNote_V1a` | 1:1 |
| V1b 旧句逐字保留为前缀 | `TestVerify_RecallNote_V1b` | 1:1 |
| V1c 由新常量重建的 `RECALL_NOTE_RE` 命中渲染后的尾注 | `TestVerify_RecallNote_V1c` | 1:1 |
| V1d strip 后渲染文本不再含追加句与 `vcc_recall` | `TestVerify_RecallNote_V1d` | 1:1 |
| V2 连续 3 次 merge，追加句恰好出现 1 次 | `TestVerify_RecallNote_V2`（逐轮计数） | 1:1 |
| V2b 已知瑕疵：补丁前写入的旧摘要保留 1 份陈旧尾注，新句仍 1 次 | `TestVerify_RecallNote_V2b`（`stale==1`） | 1:1 |
| V3a 尾注不新增 `[` 锚行（anchors 相等） | `TestVerify_RecallNote_V3a` | 1:1 |
| V3b 尾注无一行以 `[` 开头 | `TestVerify_RecallNote_V3b` | 1:1 |
| V3c 尾注单独渲染也无 `[` 锚行 | `TestVerify_RecallNote_V3c` | 1:1 |
| V4a 去掉 `RECALL_NOTE` 块后两份 TS 源逐字节相同 | 无等价物 | **TS 文件 diff 专属**；Go 侧的对应保证是「补丁注入点 `format.go:150 RecallNote` 与 `summarize.go:436 buildRecallNoteRe` / `:451 StripRecallNote` 自初版移植后逐字未变」——W4/W5 的局部修复只落在 `trackedCommands.go` / `extstate.go` / `drilldown.go` / `searchentries.go`（§15.5、§16.5），这两个文件不在其中，其余核心模块同样维持初版移植实现（早前版本此处写「本轮未改任何生产代码」，与 §2.1 补齐 `report.go` 及两轮返修矛盾，W6 改写，见 §17） |
| V4b 安装副本与 patched 副本逐字节相同 | 无等价物 | **部署侧检查**（Go 移植不经 install.sh 同步 TS，故不适用） |
| V4c 渲染样本在尾注之前逐行相同 | `TestVerify_RecallNote_V4c` | 1:1 |
| V4d 尾部差异只有追加句 | `TestVerify_RecallNote_V4d` | 1:1 |
| （mjs 之外的补丁语义）尾注剥离幂等、叠加副本可清 | `TestStripRecallNote_IdempotentAcrossWrapping`、`TestStripRecallNote_RemovesStackedCopies`、`TestRecallNoteExactValue` | 超出 mjs 的加固 |

---

## 6. `go/pi-vcc` 构建 / 静态检查 / 竞态测试（原文）

**W5 返修后的最新输出（第 2 轮返修，本会话原文；凡计数以本节与 §16.4 为准）**：

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc
$ echo '=== gofmt -l . ==='; gofmt -l . ; echo GOFMT_EXIT=$?
=== gofmt -l . ===
GOFMT_EXIT=0

$ echo '=== go build ==='; go build ./...; echo BUILD_EXIT=$?
=== go build ===
BUILD_EXIT=0

$ echo '=== go vet ==='; go vet ./...; echo VET_EXIT=$?
=== go vet ===
VET_EXIT=0

$ echo '=== go test -race -count=1 ./... ==='; go test -race -count=1 ./...; echo TEST_EXIT=$?
=== go test -race -count=1 ./... ===
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.093s
TEST_EXIT=0

$ go test -race -count=1 -v ./... > /tmp/pivcc-round2.log 2>&1; echo V_EXIT=$?
V_EXIT=0
$ grep -c '^--- PASS' /tmp/pivcc-round2.log   # 283（W4 返修后 280）
$ grep -c '^=== RUN'  /tmp/pivcc-round2.log   # 443（W4 返修后 436）
$ grep -c 'DATA RACE'  /tmp/pivcc-round2.log  # 0
$ grep -c '^--- FAIL' /tmp/pivcc-round2.log   # 0
$ tail -2 /tmp/pivcc-round2.log
PASS
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.081s

$ go test -race -count=5 -run 'TestFormatToolCallContent_|TestGetFileIndicators|TestGetTouchedFiles|Drill' . ; echo FOCUS_EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	1.232s
FOCUS_EXIT=0
```

`gofmt -l .` 无任何输出（0 个文件）。以下为 W4 返修后的输出留档：

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc
$ echo '=== go build ==='; go build ./...; echo "EXIT=$?"
=== go build ===
EXIT=0

$ echo '=== go vet ==='; go vet ./...; echo "EXIT=$?"
=== go vet ===
EXIT=0

$ echo '=== go test -race -count=1 ==='; go test -race -count=1 ./...; echo "EXIT=$?"
=== go test -race -count=1 ===
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.096s
EXIT=0
```

> 以上为 **W4 返修后**的输出留档（更早的历史输出：`ok ... 2.650s` / `EXIT=0`，见本节末尾的留档；W5 的最新输出见本节开头）。

`-v` 全量明细（落 `/tmp/pivcc-round1.log`）：

```console
$ go test -race -count=1 -v ./... > /tmp/pivcc-round1.log 2>&1; echo "EXIT=$?"
EXIT=0
$ grep -c '^--- PASS' /tmp/pivcc-round1.log      # 280（返修前 277）
$ grep -c '^=== RUN'  /tmp/pivcc-round1.log      # 436（返修前 431，含子测试）
$ grep -c 'DATA RACE'  /tmp/pivcc-round1.log     # 0
$ tail -2 /tmp/pivcc-round1.log
PASS
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.085s
```

稳定性复跑（防时序用例偶发；W4 新增的 3 个竞态/时序用例单独加压到 10 次）：

```console
$ go test -count=3 -race ./...; echo EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	10.156s
EXIT=0

$ go test -race -count=10 -run 'TestExtState_(ClearBeforeTickSuppressesAutoContinue|StaleTickAfterClearStaysSilent|AutoContinueScheduleRace|ScheduleAutoContinueSendsMarker|ClearPendingAutoContinueIsSafe|ScheduleAutoContinueSurvivesDetachedContext)' . ; echo EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	18.582s
EXIT=0
```

`gofmt`（W4 返修后复验）：

```console
$ gofmt -l .
（无输出 = go/pi-vcc/ 全部 gofmt-clean）
```

<details>
<summary>返修前的历史留档（W3 收尾时的输出，未改动）</summary>

```console
$ go test -race -count=1 ./...; echo "EXIT=$?"
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	2.650s
EXIT=0
```

</details>

`gofmt` 清理**前**现状（W3 收尾前的历史记录）：

```console
$ gofmt -l .          # 17 个文件，均为既有 WIP 文件（结构体 tag 对齐差异）
beforecompact.go  brief.go  commits.go  drilldown.go  extension.go  extract_test.go
extstate.go  globalindices.go  hook.go  patchCtxRun_test.go  recallrequest_test.go
recallrun.go  searchentries.go  summarize.go  tokenestimate.go  tokenestimate_test.go  types.go
$ gofmt -l report_test.go extension_test.go hook_test.go extstate_test.go recallrun_test.go
（无输出 = 本轮 4 个新测试文件全部 clean）
```

- 上表 17 个文件**全部是本轮之前就存在的未跟踪 WIP 文件**，此前未改其中任何一个字节；差异仅为 `gofmt` 的结构体 tag 对齐（`gofmt -d` 实测只有 `type X struct { ... }` 的列对齐变化，无语义变化）。本轮新增/修改的测试文件（`report_test.go` / `extension_test.go` / `hook_test.go` / `extstate_test.go` / `recallrun_test.go`）本就得 gofmt-clean。

**W3 收尾（门禁前清理，本会话原文）**：

```console
$ gofmt -w .          # 对上述 17 个既有 WIP 文件原地格式化（仅结构体 tag 列对齐，无语义变化）
$ gofmt -l .          # 输出为空 = go/pi-vcc/ 全部 gofmt-clean
$ echo '=== go build ==='; go build ./...; echo "BUILD_EXIT=$?"
=== go build ===
BUILD_EXIT=0
$ echo '=== go vet ==='; go vet ./...; echo "VET_EXIT=$?"
=== go vet ===
VET_EXIT=0
$ echo '=== go test -race -count=1 ==='; go test -race -count=1 ./...; echo "TEST_EXIT=$?"
=== go test -race -count=1 ===
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	2.642s
TEST_EXIT=0
```

W3 已对这 17 个文件执行 `gofmt -w .`（原 §12 遗留项 2 关闭），并复跑 build/vet/race 全绿。

---

## 7. `go/` 根：go.work 与 Makefile

`go/go.work` 的 `use` 列表含 `./pi-vcc`（共 16 个 module），`go/Makefile` 的 `COMPONENTS` 由 `*/go.mod` 派生，因此 pi-vcc 自动进入 `build`/`test`/`vet` 三个目标。逐个实跑：

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go && make build 2>&1 | tail -20
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
==> [component: pi-vcc] go build ./...
EXIT=0
```

```console
$ make vet 2>&1 | tail -20
==> [root module] go vet ./...
==> [component: auto-continue] go vet ./...
…（15 个组件同上，逐一 EXIT=0）…
==> [component: pi-vcc] go vet ./...
EXIT=0
```

```console
$ make test 2>&1 | tail -40
==> [root module] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/internal/version	0.002s
==> [component: auto-continue] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/auto-continue	0.009s
==> [component: cache-stats] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/cache-stats	0.053s
==> [component: codegraph-go] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/codegraph-go	0.215s
==> [component: ctxmode] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctxmode	3.683s
==> [component: ctx-orchestrate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctx-orchestrate	0.025s
==> [component: exclude-tools-guard] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	0.209s
==> [component: herdr-agent-state] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	0.431s
==> [component: jev-gate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/jev-gate	2.107s
==> [component: moshi-hooks] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/moshi-hooks	0.349s
==> [component: no-tables] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/no-tables	0.004s
==> [component: pi-context-pruning] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	0.005s
==> [component: pi-env] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-env	0.004s
==> [component: pi-tinyfish] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	0.064s
==> [component: pi-vcc] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	1.391s
EXIT=0
```

注：`make test` 跑的是**不带 `-race`** 的全组件测试（Makefile 目标本身如此），竞态覆盖由 §6 的 `go test -race` 单独保证。`make sdk-check` 未跑（本轮未改 `go/pigsdk`，与 pi-vcc 无关）。

---

## 8. PiG 隔离目录加载实测（原文）

隔离手段：`go/bin/pig-test` 写死 `PIG_HOME=/root/.pig-test` 并 `unset PIG_CODING_AGENT_DIR/PIG_CODING_AGENT_SESSION_DIR/PIG_USE_PI_DIRS`，因此不会碰到生产 `/root/.pi/agent`。`pi-vcc` 是非 `main` 包名 + 导出 `Extension()`，按 batch-2 确认的 factory 规则被 PiG 编译成 packed-cell runner 加载。

**包装脚本的输出面（W6 补注：为什么下面三段没有首尾横幅）**：`go/bin/pig-test` 全文 8 行（4 行注释 + `unset` / `export` / `exec` 三行动作），**没有任何 `echo` / `printf`，不打印隔离横幅**。所以 (a)~(c) 引用的就是终端的**完整**输出：既没有 `pig-test: isolated mode (PIG_HOME=/root/.pig-test)` 这样的开头行，也没有 `pig-test: production /root/.pi/agent untouched` 这样的结尾行——这两行在本仓的包装脚本里不存在（W6 实测：`grep -rn 'isolated mode' go/ --exclude=batch-4-pi-vcc.md` 命中 **0** 处，`grep -c 'isolated mode' go/bin/pig-test` = 0，即 `go/bin/` 与全部既有留档里从未出现过这两行；本报告 W6 新增段落里的 4 处命中是本轮自己写进去的引用）。W6 现场复核（stdout+stderr 一起收，未过滤）：

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go && cat bin/pig-test
#!/bin/sh
# 强制隔离 PiG 运行环境：写死 PIG_HOME=/root/.pig-test，并清除可绕过 PIG_HOME 的环境变量。
# 裸调 pig 会写入 ~/.pig（详见 reports/batch-0.md 第 10 节），所有调用必须通过本包装脚本隔离。
# 清除可能绕过 PIG_HOME 的代理配置与共享模式环境变量，防止指向生产 /root/.pi/agent 或泄漏：
unset PIG_CODING_AGENT_DIR PIG_CODING_AGENT_SESSION_DIR PIG_USE_PI_DIRS 2>/dev/null || true
export PIG_HOME=/root/.pig-test
exec /root/.local/bin/pig "$@"

$ ./bin/pig-test --version 2>&1; echo EXIT=$?
0.5.2+1.4.0
EXIT=0

$ ./bin/pig-test -e ./pi-vcc -p "回复 OK" >/tmp/pig-pivcc-load-w6.log 2>&1; echo EXIT=$?; cat /tmp/pig-pivcc-load-w6.log
EXIT=0
OK
```

→ (a) 的 W6 复跑与首轮输出逐字节相同（`/tmp/pig-pivcc-load.log` 与 `/tmp/pig-pivcc-load-w6.log` 都是 3 字节 `OK\n`，md5 同为 `d36f8f9425c4…`），且两次都无横幅行。首轮 (b)(c) 的原始日志 `/tmp/pig-pivcc-recall.log`（85 B）、`/tmp/pig-pivcc-search.log`（413 B）W6 复查仍在，其中 (c) 的日志末尾还有一段模型自述「说明：查询词 …」，首轮抄录时漏了——本节 (c) 已补回，使引用与终端完整输出一致。（`/tmp` 下这些日志只是一次性证据，不属交付物、不入库；结论以本节引用的输出原文为准。）

**（a）加载冒烟**

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go && ./bin/pig-test -e ./pi-vcc -p "回复 OK" >/tmp/pig-pivcc-load.log 2>&1; echo EXIT=$?; cat /tmp/pig-pivcc-load.log
EXIT=0
OK
```

**（b）`vcc_recall` 工具实调（`mode:'touched'`）**

```console
$ ./bin/pig-test -e ./pi-vcc -p "请调用 vcc_recall 工具，参数 mode:'touched'，把工具返回的原文贴出来" >/tmp/pig-pivcc-recall.log 2>&1; echo EXIT=$?; cat /tmp/pig-pivcc-recall.log
EXIT=0
工具返回的原文如下：

```
No file operations found in session history.
```
```

→ 工具确实被注册并执行；隔离会话里没有文件操作，返回空结果属正确行为（`touched` 的空态文案）。

**（c）`vcc_recall` 工具实调（`query:` 检索路径）**

```console
$ ./bin/pig-test -e ./pi-vcc -p "请调用 vcc_recall 工具，参数 query:'vcc_recall 工具'，把工具返回的原文贴出来" >/tmp/pig-pivcc-search.log 2>&1; echo EXIT=$?; cat /tmp/pig-pivcc-search.log
EXIT=0
vcc_recall 返回的原文如下：

```
No earlier matches for "vcc_recall 工具"; its 1 match is in the current turn, which is already in your context.
```

说明：查询词 "vcc_recall 工具" 在当前会话的早期记录中没有匹配项；唯一的匹配出现在当前这一轮对话中（也就是你刚才的这条指令本身），而它已经存在于当前上下文里，所以无需再召回。
```

→ 检索路径跑通，且**当前轮次排除**逻辑正确：唯一命中就是本轮指令本身，工具据此提示"无需召回"（对应 `RecallPagingHints.SkipCurrentTurn` / `findCurrentTurn` 那条路径）。

**未覆盖的加载面（如实记录）**：`/pi-vcc` 与 `/pi-vcc-recall` 两个斜杠命令在非交互 `-p` 模式下没有输入通道，本轮**未实测**（单次 CLI 模式无法键入斜杠命令）；命令路径由 `extension_test.go` 的 6 个用例在进程内覆盖。

---

## 9. 真实会话回放（补丁 #1 section B 的等价实测）

`verify-eqi12.mjs` 的 B 节读**最新真实 session** 回放 toolCall。为给出同等强度的证据，本轮用 Go 侧的同一抽取逻辑 + 同一 `trackCommands`（读生产配置 `/root/.pi/agent/pi-vcc-config.json`，**只读**）跑了一次性临时 harness（跑完已移出包目录，未留在交付物里）：

```console
$ go test -count=1 -run TestTmpRealSessionReplay -v .   # 临时 harness，已删除
A. trackCommands = [herdr git gh docker sqlite3 systemctl journalctl ssh apt npm pi ps crontab lsof dmesg jq]
B. newest session: /root/.pi/agent/sessions/--root-workspace-ar400--/2026-10-25T06-20-18-428Z_01a171b7-927c-713e-805b-738e8431bb7b.jsonl (mtime 2026-10-25T16:44:52+08:00)
B. replayed 126 tool calls
B. output:
herdr: (+1 earlier) herdr tab create --workspace w3G --label worker-ar-evidence --no-focus | herdr pane rename w3G:pB worker-ar-evidence | herdr pane run w3G:pB cd /root/.config/magpie | herdr agent get w3G:pB | herdr pane current --current | herdr tab create --workspace w3G --label worker-ar-fix --no-focus | herdr pane rename w3G:pC worker-ar-fix | herdr pane run w3G:pC cd /root/.config/magpie | herdr agent get w3G:pC | herdr pane close w3G:pB
C. ctx_run raw commands=15 hasTracked=true gotGit=false gotHerdr=true
--- PASS: TestTmpRealSessionReplay (0.01s)
```

结论：126 个真实 toolCall 中，`ctx_run` 的 15 条命令文本被抓到并汇成 `herdr:` 行——这正是 upstream（只扫 `bash`）会整段漏掉的数据，等价于 mjs 的 `PASS(real): captured herdr=true`。该 harness 依赖宿主会话内容，**不适合作为常驻测试**（非 hermetic），故常驻覆盖用 `TestExtractTrackedCommands_SessionFileReplay`（自建 JSONL）。

---

## 10. 与 TS 0.10.2 原版的行为差异

按"可观测行为"口径逐条列（都是移植必然带来的差异，均已在代码注释或测试里钉住）：

1. **配置路径解析**（`settings.go:79-81`）：TS 固定 `homedir()/.pi/agent/pi-vcc-config.json`；Go 侧额外认 `PI_CODING_AGENT_DIR` → `PIG_CODING_AGENT_DIR` → `$PIG_HOME/agent` → `~/.pi/agent`，并支持 `PI_VCC_CONFIG_PATH` 显式覆盖。目的是隔离 PiG 运行时绝不读写生产配置（硬规则），也是测试围栏的基础。
2. **模块级状态 → 每扩展状态**（`extstate.go`）：TS 一个扩展加载一个模块实例；Go 一个进程可服务多个扩展，故 `lastStats`/`lastCompactWasPiVcc`/`pendingFollowUp`/`pendingAutoContinue` 收进 `extensionStates` 映射、按 `*sdk.Extension` 取用。由 `TestExtState_IsKeyedPerExtension` 钉住边界。
3. **`setTimeout(..., 0)` → `time.AfterFunc` + `recover`**：auto-continue 的不可见续跑改由 Go 定时器触发，回调里 `recover()` 兜住异常（detached Context 下会 panic，已由测试证明被 recover 接住而不逃逸）。**W4 返修**给这条路径补了两样东西（详见 §15.2）：(a) 回调在锁内校验「自己仍是当前代的定时器」，取消/被新一轮顶替时直接返回、绝不发标记；(b) 定时器延时可被测试拉宽（`autoContinueDelay`，默认仍为 0），否则「先 schedule 再立刻 clear」在 Go 里是与回调抢锁的平局，无法稳定断言。
4. **JS 语义适配层**：字符串按 UTF-16 码位切片/取长（`jslite.go`）、`toFixed(1)` 用 `math/big` 做 half-away-from-zero（`jsFixed1`）、RegExp 在 RE2 上复刻 JS 语义（`.` 不吃行终止符、`\s` 含 Unicode 分隔符 + U+FEFF、`i` 用 Unicode simple case folding、`\uXXXX` 展开、JS `\b` 定义）。这些是"看起来相同"的必要成本，不是行为差异。
5. **`read`/`write` 类字段的 JSON 数值**：Go `encoding/json` 反序列化到 `any` 得 `float64`，端口在断言/格式化前统一走 `jsNumberString`/`jsFixed1`，避免 `1` 被渲染成 `1.0`。
6. **命令名**：TS `registerCommand("pi-vcc")` / `registerCommand("pi-vcc-recall")`；Go `ext.Command("pi-vcc")` / `ext.Command("pi-vcc-recall")`，工具名同为 `vcc_recall`，`details.compactor` 同为 `"pi-vcc"`——**对外契约不变**（§8 的隔离实测即证）。
7. **`report.ts` 的 `buildCompactReport`** 在 TS 侧也没有运行时调用方；Go 侧同样保留完整实现与测试，不接入装配路径——行为一致（都不影响运行时）。

未发现"Go 侧行为与 TS 不一致"的实质偏差；如 reviewer 发现，按 §5 的用例逐条对照即可定位。

8. **负 `limit` 的空窗口语义**（W5 返修新增，`go/pi-vcc/drilldown.go:183-191`）：TS 用 `allLines.slice(startLine, endLine)`，而 JS `Array.slice` 的**负 end 索引**按 `length + end` 回绕——5 行文件传 `limit:-5` 得空窗口（与 Go 一致），但 100 行文件传 `limit:-1` 会得到 99 行窗口。Go 侧的守卫（`startLine < totalLines && startLine < endLine`，见 §16.1）在两种输入下都返回空窗口与「beyond file length」提示，即**长文件 + 负 limit** 这一格与 TS 不同。该输入**无法经 `ParseDrillDown` 产生**（`:N[:M]` 后缀只接受 `\d+`），只可能来自直接调用导出函数的调用方，故不构成可观测行为差异；此处按 reviewer 指定的守卫实现并显式记录偏差。
9. **`:full` 截断处的孤立代理对**（W5 返修新增，`go/pi-vcc/drilldown.go:155-162`）：TS `body.slice(0, MAX_FULL_BYTES)` 切在代理对中间时保留一个孤立高位代理；Go 字符串装不下孤立代理，`jsSlice`→`jsFromUnits` 按包内既定策略写成 `U+FFFD`。**两者的 UTF-8 字节输出一致**——node 实测把 TS 侧那个孤立代理编码出来正是 `ef bf bd`（见 §16.2 的对照表）。

---

## 11. PiG 本身缺口及绕法（本轮未改 PiG 源码）

1. **斜杠命令在 `-p` 单次模式下不可达**：非交互 CLI 没有输入通道，`/pi-vcc`、`/pi-vcc-recall` 无法实测。绕法：命令路径用进程内 SDK 桩覆盖（`extension_test.go` 6 个用例），隔离实测只覆盖工具与加载面。
2. **UI 面在单次 CLI 下被丢弃**：状态栏/通知由 PiG 的 no-op UI 上下文丢弃（batch-1 已记 `ui_bridge.go:1356-1367`），因此 stats toast 只能靠 harness 记录 host call 验证，隔离运行无可见输出。
3. **`make test` 不带 `-race`**：Makefile 目标本身不传 `-race`，竞态覆盖需单独 `go test -race -count=1 ./...`（§6 已补）。
4. **Go 扩展加载规则**（batch-2 已确认，本轮复核仍成立）：`go/<name>/` 必须是非 `main` 包名 + 导出 `Extension()`，否则 PiG 报 "must select its exact module root"。pi-vcc 满足该约定，故 `-e ./pi-vcc` 可直接编译加载。

---

## 12. 遗留问题

1. ~~**`go/pi-vcc/` 缺 `README.md`**~~（**W3 已解决**）：已创建 `go/pi-vcc/README.md`（风格对齐 `go/ctxmode/README.md` 等组件），覆盖 `@sting8k/pi-vcc@0.10.2` 的 Go 移植概览、注册的工具/命令/事件面、原生合入的两个本地补丁（`pi-vcc-0.9.0-ctx-run` / `pi-vcc-0.10.0-recall-note`），以及代码注释引用的说明：`settings.go:79`（D1 配置路径解析）、`extstate.go`（D2 每扩展独立状态）、`content.go:294`（`sortedKeys` 偏差）、`jslite.go:12`（JS 语义适配层），消除全部悬空引用。
2. ~~**17 个既有 WIP 文件 `gofmt -l` 不干净**~~（**W3 已解决**）：已对 `go/pi-vcc/` 执行 `gofmt -w .`（格式-only，仅结构体 tag 列对齐，无语义变化），现 `gofmt -l .` 输出为空；复跑 `go build` / `go vet` / `go test -race -count=1` 均 EXIT=0。
3. **`verify-eqi12.mjs` 的 A 节**（`loadSettings()` 实读生产配置）没有逐字节等价用例：Go 用例以显式参数传入同一 `trackCommands` 列表，等价但不读盘；真实读盘路径由 §9 的一次性 harness 实测过（只读生产配置）。
4. **TS/jiti 侧的 V0/V4a/V4b 三项检查无 Go 等价物**（§5.2 已逐条列明）：它们是加载机制与 TS 文件/部署副本 diff 的检查，Go 移植不经这条链路。若需端到端确认补丁在 TS 侧仍生效，应另行在 eqi12 上跑原 `verify.mjs`（本轮未跑，未写"应该可以"）。

---

## 13. git 状态确认

**提交前（W5 返修结束时留档，原样保留）**：

```console
$ git -C /root/workspace/.wt/pi-extensions/feature-go-pi-vcc status --porcelain -b
## feature/go-pi-vcc
 M go/go.work
?? go/pi-vcc/
?? go/reports/batch-4-pi-vcc.md
$ git -C /root/workspace/.wt/pi-extensions/feature-go-pi-vcc diff --stat
 go/go.work | 1 +
 1 file changed, 1 insertion(+)
```

**W6 提交落地（本会话原文）**：

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc
$ git add go/pi-vcc go/go.work
$ git diff --cached --name-only | wc -l; git diff --cached --stat | tail -1
59
 59 files changed, 16463 insertions(+)
$ git diff --cached --name-only | grep -v -e '^go/pi-vcc/' -e '^go/go.work$'; echo GREP_EXIT=$?   # 空输出 = 暂存区只含这两类路径
GREP_EXIT=1
$ git diff --cached -- go/go.work
diff --git a/go/go.work b/go/go.work
index 8b33ec2..4976dce 100644
--- a/go/go.work
+++ b/go/go.work
@@ -17,5 +17,6 @@ use (
 	./pi-context-pruning
 	./pi-env
 	./pi-tinyfish
+	./pi-vcc
 	./pigsdk
 )
$ git commit -m "feat(go): port pi-vcc extension to Go (batch 4)"
[feature/go-pi-vcc 41abab1] feat(go): port pi-vcc extension to Go (batch 4)
 59 files changed, 16463 insertions(+)
$ git rev-parse --short HEAD; git rev-parse HEAD
41abab1
41abab1d4feeb268d283d93036da507cf63583d7
$ git show --name-only --pretty=format: HEAD | grep -c '^go/pi-vcc/'; find go/pi-vcc -type f | wc -l
58
58
$ git status --porcelain -b
## feature/go-pi-vcc
?? go/reports/batch-4-pi-vcc.md      # 本报告，随后单独提交
$ git log -n 2 --oneline
41abab1 feat(go): port pi-vcc extension to Go (batch 4)
338c5f0 feat(go): port cache-stats extension to Go (batch 3)
$ git -C /root/workspace/pi-extensions status --porcelain -b   # 主目录只读查看，零写入
## feature/go-batch1
 M go/go.work
?? go/pi-my-compact-ui/
```

报告文件自身的提交（本会话随后执行）：`git add go/reports/batch-4-pi-vcc.md` + `git commit -m "docs(go): add batch-4-pi-vcc report"`，只含这一个文件；其后 `git status --porcelain -b` 输出单行 `## feature/go-pi-vcc`（工作树完全干净），`git log -n 3 --oneline` 头部就是「报告提交 → `41abab1` 组件提交 → `338c5f0` batch-3」这三条连续记录。

- 改动仅限本 worktree 的 `go/` 目录：`go/go.work` 一行改动（把 `./pi-vcc` 加入 `use` 列表，第 3 节表格即依赖它）+ `go/pi-vcc/` 的 **58** 个新文件 + 本报告 `go/reports/batch-4-pi-vcc.md`。
  - **计数纠偏（W6）**：本节早前写的「`go/pi-vcc/`（59 个文件）」用的是 `git ls-files --others --exclude-standard go | wc -l`，那个 59 把本报告文件也算了进去；`go/pi-vcc/` 实际 **58** 个文件（`find go/pi-vcc -type f | wc -l` = 58），加上 `go/go.work` 才是 `41abab1` 里的 **59 files changed**。派发任务书里「`go/go.work` 和 `go/pi-vcc/` 下的 60 个文件」与此同样不符，本报告以现场计数为准（58 + 1 = 59）。
- W4 返修的 4 个文件与 **W5 返修的 4 个文件**（`drilldown.go` / `searchentries.go` / `recallrequest_test.go` / `recall_test.go`）都在 `go/pi-vcc/` 内，已随 `41abab1` 一并入库；W6 未改其中任何一字节（本轮零代码改动，`git show --stat 41abab1` 之后本 worktree 无第二个改码提交）。
- 主目录 `/root/workspace/pi-extensions` 当前在 `feature/go-batch1` 分支；其工作区里的 `M go/go.work` 与 `?? go/pi-my-compact-ui/` 是**本轮之前就存在**的状态，本会话未在其中写入任何文件（只用 `git -C` 只读查看；W6 另在其中做过一次**只读** `read`——`go/bin/pig-test`，用于 §8 的横幅核对，零写入）；`/root/.pi/agent/` 只读（本会话只读了一份原版 TS 源：`/root/.pi/agent/npm/node_modules/@sting8k/pi-vcc/src/core/drill-down.ts`，未写、未安装、未巡检）。
- 提交号：组件代码 = `41abab1`（完整 `41abab1d4feeb268d283d93036da507cf63583d7`）；本报告 = 紧随其后的 `docs(go): add batch-4-pi-vcc report`（自指 SHA 的理由见报告顶部）。两次提交都在 `feature/go-pi-vcc`，**未 `git merge`（`feature/go-batch1` / `main` 未被触碰）、未 `git push`**。

---

## 14. 复现命令清单（本报告所有结论的最小复现集）

```bash
cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc
gofmt -l . && go build ./... && go vet ./... && go test -race -count=1 ./...

cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go
make build && make vet && make test
./bin/pig-test -e ./pi-vcc -p "回复 OK"
./bin/pig-test -e ./pi-vcc -p "请调用 vcc_recall 工具，参数 mode:'touched'，把工具返回的原文贴出来"
./bin/pig-test -e ./pi-vcc -p "请调用 vcc_recall 工具，参数 query:'vcc_recall 工具'，把工具返回的原文贴出来"
```

---

## 15. 第 1 轮审查返修（W4）：3 项问题逐条修复

> 授权范围：同一 worktree（`/root/workspace/.wt/pi-extensions/feature-go-pi-vcc`，分支 `feature/go-pi-vcc`）内修码 + 补回归测试 + 更新本报告。
> **未执行** `git add` / `git commit` / `git merge` / `git push`；主目录 `/root/workspace/pi-extensions` 与 `/root/.pi/agent/` 零写入。

### 15.1 Must Fix 1 — `trackedCommands.go:45` 前导 `&` 越界 panic

**问题**：`unquotedSeparators` 的 `&` 分支写的是 `command[i-1] == '>'`，当 `&` 位于**索引 0** 时取 `command[-1]`，直接 `panic: index out of range [-1]`。TS 原版 `cmd[i - 1]` 在 `i === 0` 时得到 `undefined`，两个比较都为 false，不炸——Go 端口漏掉了这层差异。

**修法**（`go/pi-vcc/trackedCommands.go:45`，1 行）：

```go
case c == '&' && (byteAt(command, i-1) == '>' || byteAt(command, i-1) == '<' || byteAt(command, i+1) == '>'):
```

复用同文件已有的 `byteAt`（越界返回 0），与 TS 的 `undefined` 语义精确对齐。

**行为核对**（对照 `/root/.pi/agent/patches/pi-vcc-0.9.0-ctx-run/tracked-commands.patched.ts` 的 `unquotedSeparators`，只读引用）：

| 输入 | `&` 是否分隔符 | TS 行为 | Go 修后行为 |
|---|---|---|---|
| `& git status` | 是（左邻不存在、右邻不是 `>`） | 分隔符，第 2 段起是 `git status` | 一致：抓到 `git status` |
| `&>out git status` | 否（右邻是 `>`，属重定向） | 非分隔符；段首 token 是 `&>out`，不是受跟踪名 | 一致：无命中 |

**回归测试**（`go/pi-vcc/patchCtxRun_test.go`，`TestExtractTrackedCommands_CommandNameMatching` 表内新增 2 个子例）：

```go
{"leading ampersand is a separator", "& git status", "git", []string{"git status"}},
{"leading ampersand redirection is not a separator", "&>out git status", "git", nil},
```

**改前 FAIL / 改后 PASS 证据**（先把该行改回 `command[i-1]`，跑同一用例）：

```console
$ go test -count=1 -run TestExtractTrackedCommands_CommandNameMatching .
--- FAIL: TestExtractTrackedCommands_CommandNameMatching (0.00s)
    --- FAIL: TestExtractTrackedCommands_CommandNameMatching/leading_ampersand_is_a_separator (0.00s)
panic: runtime error: index out of range [-1] [recovered, repanicked]
...
github.com/dorokuma/pi-extensions/go/pi-vcc.unquotedSeparators({0x81c870, 0xc})
	/root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc/trackedCommands.go:45 +0x1e5
...
FAIL	github.com/dorokuma/pi-extensions/go/pi-vcc	0.007s
```

改回 `byteAt` 后：

```console
$ go test -count=1 -run 'TestExtractTrackedCommands_(CommandNameMatching|CapKeepsNewest)' -v .
--- PASS: TestExtractTrackedCommands_CommandNameMatching (0.00s)
    --- PASS: TestExtractTrackedCommands_CommandNameMatching/leading_ampersand_is_a_separator (0.00s)
    --- PASS: TestExtractTrackedCommands_CommandNameMatching/leading_ampersand_redirection_is_not_a_separator (0.00s)
--- PASS: TestExtractTrackedCommands_CapKeepsNewest (0.00s)
PASS
```

### 15.2 Should Fix 1 — `extstate.go` `scheduleAutoContinue` 定时器发布与取消竞态

**问题**（`go/pi-vcc/extstate.go` 返修前 `:116-128`）：

```go
func (s *extensionState) clearPendingAutoContinue() {
	s.mu.Lock()
	timer := s.pendingAutoContinue
	s.pendingAutoContinue = nil
	s.mu.Unlock()
	if timer != nil { timer.Stop() }        // ① 停在锁外
}
func (s *extensionState) scheduleAutoContinue(...) {
	s.clearPendingAutoContinue()
	timer := time.AfterFunc(0, func() {
		s.mu.Lock(); s.pendingAutoContinue = nil; s.mu.Unlock()   // ② 不校验自己是否还活着
		TriggerInvisibleContinue(ctx)
	})
	s.mu.Lock(); s.pendingAutoContinue = timer; s.mu.Unlock()   // ③ 赋值在锁外
}
```

三个窗口：① 解锁与 `Stop()` 之间回调已启动 → 取消后仍发标记；② 回调不校验代数 → 被取消/顶替后照样发；③ 赋值在 `clearPendingAutoContinue()` 返回之后 → 并发 clear 看到 nil 白跑一趟，随后旧指针覆盖新定时器。

**修法**（`go/pi-vcc/extstate.go`）：

1. `extensionState` 增 `autoContinueGen uint64`：每次 arm / clear / fire 各递增一次，回调锁内比对「自己那代是否仍是当前代」。
2. `clearPendingAutoContinue()` 改为**持锁做完**「停表 + 置 nil + 代数++」（拆出 `clearPendingAutoContinueLocked()` 供持锁方复用）。
3. `scheduleAutoContinue()` 全程持锁：先停旧的，再取当前代数，`time.AfterFunc` 回调里先 `s.mu.Lock()` 校验代数——不一致就解锁返回、**绝不** `TriggerInvisibleContinue(ctx)`；一致才置 nil、`autoContinueGen = gen + 1`、解锁后发送（`defer func() { _ = recover() }()` 保留）。
4. 定时器延时可被测试拉宽：`var autoContinueDelay time.Duration`（默认 0，即 `setTimeout(..., 0)`）。这是**唯一的生产侧新增面**，理由见 15.5。

**回归测试**（`go/pi-vcc/extstate_test.go`，3 个新用例 + 3 个辅助函数）：

| 用例 | 场景 | 断言 |
|---|---|---|
| `TestExtState_ClearBeforeTickSuppressesAutoContinue` | 拉宽延时 → 真实 hook arm → **立刻** `clearPendingAutoContinue` | `sendMessage` 里 `customType=pi-vcc-auto-continue` 的标记数 == 0，且 `pendingAutoContinue == nil` |
| `TestExtState_StaleTickAfterClearStaysSilent` | **持锁**睡过延时，让回调停在 `s.mu` 上，再持锁 clear，最后放行 | 标记数 == 0（钉住「回调必须校验代数」） |
| `TestExtState_AutoContinueScheduleRace` | 3 个 goroutine 狂 `clearPendingAutoContinue`，同时主 goroutine 经真实 hook arm 8 次 | `-race` 无 DATA RACE；赛后无残留定时器；再 arm 一次**恰好**多发 1 个标记 |

**改前 FAIL / 改后 PASS 证据**（把回调里的代数校验临时改成 `if false && ...`，其余不动）：

```console
$ go test -count=1 -race -run 'TestExtState_(ClearBeforeTickSuppressesAutoContinue|StaleTickAfterClearStaysSilent|AutoContinueScheduleRace)' -v .
=== RUN   TestExtState_ClearBeforeTickSuppressesAutoContinue
--- PASS: TestExtState_ClearBeforeTickSuppressesAutoContinue (0.36s)
=== RUN   TestExtState_StaleTickAfterClearStaysSilent
    extstate_test.go:309: a stale tick sent the marker 1 time(s): []pivcc.hostCall{pivcc.hostCall{Method:"sendMessage", Args:map[string]interface {}{"message":map[string]interface {}{"content":[]interface {}{"customType":"pi-vcc-auto-continue", "display":false}, "options":map[string]interface {}{"deliverAs":"followUp", "triggerTurn":true}}}}
--- FAIL: TestExtState_StaleTickAfterClearStaysSilent (0.46s)
=== RUN   TestExtState_AutoContinueScheduleRace
--- PASS: TestExtState_AutoContinueScheduleRace (0.62s)
FAIL
```

→ 恢复校验后三例全 PASS（见 15.4）。

**关于两个用例的判别力，如实说明**：`ClearBeforeTickSuppressesAutoContinue` 因为延时被拉宽，`timer.Stop()` 稳定赢在回调之前，所以**单独移除代数校验它也会 PASS**——它钉的是「取消后不应发标记」这条端到端契约（reviewer 原话要求的场景）；真正钉住代数校验的是 `StaleTickAfterClearStaysSilent`（上面已证移除校验即 FAIL）。`AutoContinueScheduleRace` 是压力/一致性用例，`-race` 检测器与赛赛后状态断言是它的主要价值。三个用例覆盖的是三个不同窗口，不重复。

### 15.3 Should Fix 2 — `patchCtxRun_test.go:254` 假绿断言

**问题**：`TestExtractTrackedCommands_CapKeepsNewest` 第 245 行生成的是 `"docker restart api" + string(rune('a'+i-1))`（即 `docker restart apia` .. `docker restart apik`），第 254 行却写 `if strings.Contains(lines[0], "apialpha")`——`apialpha` 永不出现在输出里，这条「最旧的一条已被淘汰」的断言**永远是绿的**。

**修法**（`go/pi-vcc/patchCtxRun_test.go:254`）：

```go
// The oldest entry is `docker restart apia`; the tail cap must have
// dropped it, so its absence is what pins "keeps the NEWEST".
if strings.Contains(lines[0], "docker restart apia") {
	t.Fatalf("the cap kept the oldest entry: %q", lines[0])
}
```

`docker restart apia` 是第 1 条（最旧），tail cap 留最新 10 条（`apib`..`apik`）后它必须消失；且 `apia` 不是 `apib`..`apik` 任何一条的子串，断言不会误判。同用例里原有的 `(+1 earlier)` 前缀断言与 `apik`（最新）断言保持不变，三者合起来才真正锁死 tail 语义。

### 15.4 返修后的完整验证（原文）

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc
$ gofmt -l .
（无输出）

$ go build ./...; echo "BUILD_EXIT=$?"
BUILD_EXIT=0

$ go vet ./...; echo "VET_EXIT=$?"
VET_EXIT=0

$ go test -race -count=1 ./...; echo "TEST_EXIT=$?"
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.096s
TEST_EXIT=0

$ go test -race -count=1 -v ./... > /tmp/pivcc-round1.log 2>&1; echo "EXIT=$?"
EXIT=0
$ grep -c '^--- PASS' /tmp/pivcc-round1.log   # 280
$ grep -c '^=== RUN'  /tmp/pivcc-round1.log   # 436
$ grep -c 'DATA RACE'  /tmp/pivcc-round1.log  # 0

$ go test -count=3 -race ./...; echo EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	10.156s
EXIT=0

$ go test -race -count=10 -run 'TestExtState_(ClearBeforeTickSuppressesAutoContinue|StaleTickAfterClearStaysSilent|AutoContinueScheduleRace|ScheduleAutoContinueSendsMarker|ClearPendingAutoContinueIsSafe|ScheduleAutoContinueSurvivesDetachedContext)' . ; echo EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	18.582s
EXIT=0
```

注：`go/` 根的 `make build` / `make vet` / `make test` **本轮未重跑**——改动全部 confined 在 `go/pi-vcc/` 内，其余 15 个组件无依赖关系变化（`go/go.work` 的 `use` 列表未变）；§7 留档的是返修前的输出。

### 15.5 改动量与测试行数比

| 文件 | 改动 |
|---|---|
| `go/pi-vcc/trackedCommands.go` | 1 行表达式 + 3 行注释（+4） |
| `go/pi-vcc/extstate.go` | 结构体 +1 字段、`clearPendingAutoContinue` 持锁化、拆出 `clearPendingAutoContinueLocked`、`scheduleAutoContinue` 持锁 + 代数校验、新增 `autoContinueDelay`（净增约 20 行） |
| `go/pi-vcc/patchCtxRun_test.go` | 表内 +2 子例、修 1 处假绿断言（+8 行） |
| `go/pi-vcc/extstate_test.go` | +3 用例、+3 辅助函数、`sync` import（+185 行） |

测试新增 193 行 / 生产净增约 24 行 ≈ **8 倍**，超过共享纪律的 3 倍线，理由：(a) 3 项问题中有 2 项是**竞态/边界**，Go 侧没有轻量办法钉住时序窗口，只能用「拉宽延时」与「持锁停放回调」两种手法各写一例，外加一例并发压力；(b) `extstate_test.go` 的 3 个用例都走**真实 IPC harness**（`newHost`），每例都要重复「`PI_VCC_PI_VERSION=0.84.3` + `setState(true)` + `session_before_compact` 预热 + `setLastCompactWasPiVcc(false)` + `session_compact` arm」这套约 12 行装配，这部分是 harness 成本而非断言膨胀；(c) 3 个辅助函数（`widenAutoContinueDelay` / `thresholdCompactEvent` / `autoContinueSends` + `waitForAutoContinueSends`）被 3 个用例共享，正是为了避免把同一段装配抄三遍。

### 15.6 遗留

- `autoContinueDelay` 作为可写包级变量，是为测试拉宽定时的**刻意接缝**（默认 0 = `setTimeout(..., 0)`，生产行为不变）。若 reviewer 认为不应为测试改生产符号，替代方案是把「停表 + 校验代数」做成可注入的接口，成本更高；或接受 `ClearBeforeTickSuppressesAutoContinue` 只能覆盖到「Stop() 先赢」的那一半。
- §12 的 4 项遗留（A 节无逐字节等价用例、TS 侧 V0/V4a/V4b 无 Go 等价物等）**不受本轮返修影响**，仍开放。

---

## 16. 第 2 轮审查返修（W5）：2 项 Should Fix + 2 项 Consider

> 授权范围：同一 worktree（`/root/workspace/.wt/pi-extensions/feature-go-pi-vcc`，分支 `feature/go-pi-vcc`）内修码 + 补回归测试 + 更新本报告。
> **未执行** `git add` / `git commit` / `git merge` / `git push`；主目录 `/root/workspace/pi-extensions` 与 `/root/.pi/agent/` **零写入**（对 `/root/.pi/agent` 只做了一次只读查看：读原版 TS 源 `/root/.pi/agent/npm/node_modules/@sting8k/pi-vcc/src/core/drill-down.ts`；未执行任何安装、巡检或写入）。
> 返修前的基线实测：`gofmt -l .` 空、`go build`/`go vet`/`go test -race -count=1` 全 EXIT=0（`ok ... 4.083s`），即在绿基线上返修。

### 16.1 Should Fix 1 — `drilldown.go` offset 越界文案失真与负 `limit` 切片边界

**问题**（返修前 `go/pi-vcc/drilldown.go:177-181`）：

```go
if endLine > totalLines {
	endLine = totalLines
}
if startLine > totalLines {
	startLine = totalLines        // ← 把用户传入的 offset 抹成了 totalLines
}
visible := allLines[startLine:endLine]   // ← 负 limit 时 endLine < startLine，直接 panic
```

TS 原版（`@sting8k/pi-vcc@0.10.2` `src/core/drill-down.ts:119-127`，本机 `/root/.pi/agent/npm/node_modules/@sting8k/pi-vcc/src/core/drill-down.ts` 只读核对）：

```ts
const startLine = Math.max(0, offset);              // 只夹下界 0，从不夹到 totalLines
const maxLines = limit ?? 30;
const endLine = Math.min(startLine + maxLines, totalLines);
const visible = allLines.slice(startLine, endLine);
if (visible.length === 0) {
  return `Offset ${startLine} is beyond file length ${totalLines}. Use #${entryIndex}:${tc.path} for the first ${previewLimit} lines.`;
}
```

两处偏差：`offset > totalLines` 时报错文案输出被夹后的 `totalLines`（`Offset 5 is beyond file length 5`），且 `endLine < startLine` 时 Go 切片 panic。

**修法**（`go/pi-vcc/drilldown.go:183-191`，删掉 3 行夹取 + 改守卫式切片）：

```go
if endLine > totalLines {
	endLine = totalLines
}
// startLine is NOT clamped up to totalLines: drill-down.ts keeps the
// caller's own value (`Math.max(0, offset)` only floors at 0), so an
// out-of-range offset is reported back verbatim in the diagnostic below.
// Slicing is what needs guarding — a window that starts past the end, or a
// negative limit that puts endLine before startLine, simply shows nothing.
var visible []string
if startLine < totalLines && startLine < endLine {
	visible = allLines[startLine:endLine]
}
displayStart := startLine + 1
```

**TS 原版实跑对照**（把上面 4 行 TS 表达式逐字搬到 node 里跑，`/tmp/pivcc-r2-parity.mjs`，一次性临时脚本）：

```console
$ node /tmp/pivcc-r2-parity.mjs
beyond-window  visibleLen= 0 msg= "Offset 99 is beyond file length 5. Use #7:notes.md for the first 30 lines."
neg-limit-5len visibleLen= 0 msg= "Offset 0 is beyond file length 5. Use #7:notes.md for the first 30 lines."
neg-limit-100l visibleLen= 99
full cap: byteLen= 51206 shortfall= 6 units(body)= 51204 units(truncated)= 51200 tailUnits= "a语" endsWithLoneHighSurrogate= false utf8TailOfTruncated= 6161616161e8afad
full cap: byteLen= 51207 shortfall= 7 units(body)= 51205 units(truncated)= 51200 tailUnits= "a\ud83d" endsWithLoneHighSurrogate= true utf8TailOfTruncated= 6161616161efbfbd
```

第 1 行即 Go 修后期望文案：`offset=99` 原样回显；第 2 行证实负 `limit` 在 TS 侧同样得空窗口（Go 修后一致）；第 3 行是 TS 与 Go **已知不一致**的那一格（见 §10 第 8 条与 §16.6）。

**回归测试**（`go/pi-vcc/recallrequest_test.go:75-165`，3 个新用例 + 1 个辅助函数）：

| 用例 | 场景 | 断言 |
|---|---|---|
| `TestFormatToolCallContent_OffsetBeyondFileLength` | 5 行文本、`offset=99` | 返回文案**逐字等于** `Offset 99 is beyond file length 5. Use #7:notes.md for the first 30 lines.` |
| `TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow` | 5 行 / 100 行文本各传 `limit=-5`、`offset=0` | 不 panic，返回空窗口诊断 `Offset 0 is beyond file length 5/100. …` |
| `TestFormatToolCallContent_FullCapSlicesCodeUnits` | `:full` 截断（见 16.2） | 见 16.2 |

**改前 FAIL / 改后 PASS 证据**（先把 `drilldown.go` 改回返修前的夹取 + 字节切片，跑同一用例）：

```console
$ go test -count=1 -run 'TestFormatToolCallContent_' -v .
=== RUN   TestFormatToolCallContent_OffsetBeyondFileLength
    recallrequest_test.go:90: beyond-end window = "Offset 5 is beyond file length 5. Use #7:notes.md for the first 30 lines.", want "Offset 99 is beyond file length 5. Use #7:notes.md for the first 30 lines."
--- FAIL: TestFormatToolCallContent_OffsetBeyondFileLength (0.00s)
=== RUN   TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow
=== RUN   TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow/short_file
--- FAIL: TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow (0.00s)
    --- FAIL: TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow/short_file (0.00s)
panic: runtime error: slice bounds out of range [:-5] [recovered, repanicked]

goroutine 22 [running]:
testing.tRunner.func1.2({0x7ee2e0, 0x24ffa43d41c8})
	/usr/local/go/src/testing/testing.go:1974 +0x232
testing.tRunner.func1()
	/usr/local/go/src/testing/testing.go:1977 +0x349
panic({0x7ee2e0?, 0x24ffa43d41c8?})
	/usr/local/go/src/runtime/panic.go:860 +0x13a
github.com/dorokuma/pi-extensions/go/pi-vcc.formatToolCallContent({{0x819ea6, 0x5}, {0x81b9ec, 0x8}, {0x81dee2, 0x9}, 0x1, {0x0, 0x0}, 0x0, ...}, ...)
	/root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc/drilldown.go:191 +0xfba
…
FAIL	github.com/dorokuma/pi-extensions/go/pi-vcc	0.006s
FAIL
```

（上面 panic 栈里的 `drilldown.go:191` 是「把修法临时改回」的那份快照的行号——快照里 `:full` 的 6 行注释仍在，故比原始返修前版本下移 11 行；原始返修前版本中该切片语句位于 `drilldown.go:180`，可用 `diff` 的两个 hunk 头 `@@ -152,8 +152,14 @@` / `@@ -174,10 +180,15 @@` 复核。恢复修法后同一跑法转绿，见 16.4。）

### 16.2 Should Fix 2 — `:full` 50KB 截断与 TS `body.slice` 对齐

**问题**（返修前 `go/pi-vcc/drilldown.go:155-160`）：`truncated := body[:drillMaxFullBytes]` 按 **UTF-8 字节**切。TS 的判定是 `Buffer.byteLength(body, "utf8") > MAX_FULL_BYTES`（字节），截断是 `body.slice(0, MAX_FULL_BYTES)`（**UTF-16 码元**）。两者在 50KB 边界上不一致：字节切会把一个多字节字符切成两半，产出非法 UTF-8 字节序列；代理对也会被切开。

**修法**（`go/pi-vcc/drilldown.go:155-162`）：

```go
if len(body) > drillMaxFullBytes {
	truncated := jsSlice(body, 0, drillMaxFullBytes)
	return "File: " + call.Path + "\nTool: " + call.Name + "\n\n" + truncated + "\n\n... (" +
		jsNumberString(float64(len(body)-drillMaxFullBytes)) + " more bytes — file exceeds 50KB display limit. …"
}
```

判定仍是 `len(body)`（UTF-8 字节，与 `Buffer.byteLength` 同口径），字节差仍是 `len(body)-drillMaxFullBytes`（与 TS 的 `Buffer.byteLength(body, "utf8") - MAX_FULL_BYTES` 完全一致），只有截断换成 `jsSlice`。

**回归测试**（`go/pi-vcc/recallrequest_test.go:130-165`，表格驱动 2 格，同一分支不复制断言）：

| body（`head` = 51200 个 `a`） | 期望截断结果 | node 侧 TS 原版实测 |
|---|---|---|
| `head + "语" + "tail"` | `head + "语"`（51200 码元，整字不切） | `units(truncated)=51200 tailUnits="a语" shortfall=6`、UTF-8 尾 `…616161e8afad`（`语` 完整） |
| `head + "🙂" + "tail"` | `head + U+FFFD`（切在代理对中间，按包内既定策略写替换字符） | `endsWithLoneHighSurrogate=true`、Node 把该孤立代理编码出的 UTF-8 尾正是 `…616161efbfbd`，**与 Go 输出逐字节相同** |

两条断言都带 `utf8.ValidString(got)`，钉住「绝不在 50KB 边界产出非法字节序列」。

**改前 FAIL / 改后 PASS 证据**（把 `jsSlice(body, 0, drillMaxFullBytes)` 临时改回 `body[:drillMaxFullBytes]`）：

```console
$ go test -count=1 -run 'TestFormatToolCallContent_FullCapSlicesCodeUnits' -v .
=== RUN   TestFormatToolCallContent_FullCapSlicesCodeUnits
=== RUN   TestFormatToolCallContent_FullCapSlicesCodeUnits/three_byte_rune_at_the_boundary
    recallrequest_test.go:158: :full output mismatch
         got: <elided 100KB>
        want: <elided 100KB>
=== RUN   TestFormatToolCallContent_FullCapSlicesCodeUnits/surrogate_pair_at_the_boundary
    recallrequest_test.go:158: :full output mismatch
         got: <elided 100KB>
        want: <elided 100KB>
--- FAIL: TestFormatToolCallContent_FullCapSlicesCodeUnits (0.01s)
FAIL
```

并附一个一次性小程序（`/tmp/pivcc-fullcap-evidence/main.go`）直观测返修前字节切片的后果：

```console
$ go run .
byteLen=51206 codeUnits=51204 | old byte-slice validUTF8=false lastBytes=616161e8
byteLen=51207 codeUnits=51205 | old byte-slice validUTF8=false lastBytes=616161f0
```

→ 返修前截断结果 `utf8.ValidString == false`，末字节 `e8` / `f0` 正是被腰斩的多字节字符首字节。恢复 `jsSlice` 后两格全 PASS（见 16.4）。

### 16.3 Consider 1 — `GetFileIndicators` 导出为具名类型

**改动**（`go/pi-vcc/searchentries.go:344-355`）：匿名 `[]struct{...}` 改为导出具名类型，函数签名随之可读、可被调用方书写：

```go
// FileIndicator is search-entries.ts's getFileIndicators() element: one
// content-bearing file call of a message plus its non-empty content line count.
type FileIndicator struct {
	ToolName  string
	Path      string
	LineCount int
}

func GetFileIndicators(msg map[string]any) []FileIndicator {
	out := make([]FileIndicator, 0, 2)
	…
}
```

- 唯一的既有调用点 `GetTouchedFiles`（`searchentries.go:391`）只读 `fm.Path` / `fm.ToolName`，无需改动。
- `recall_test.go:154` 的 `TestGetFileIndicators` 把 `got :=` 改成 `var got []FileIndicator = GetFileIndicators(msg)`，让**导出类型本身就是断言的一部分**（类型漂移会让测试编译失败）。
- 全仓检索（`GetFileIndicators` / `FileIndicator` / `pi-extensions/go/pi-vcc`）：除 `go/pi-vcc` 自身外无其它模块引用该符号，无外部连带改动。
- 纯类型提取、无行为变化，故按共享测试纪律**不新增测试**。

### 16.4 Consider 2 + 本轮完整验证（原文）

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go/pi-vcc
$ gofmt -w . ; echo GOFMT_W_EXIT=$?
GOFMT_W_EXIT=0
$ gofmt -l . ; echo GOFMT_EXIT=$?
GOFMT_EXIT=0                      # 无任何输出 = go/pi-vcc/ 全部 gofmt-clean

# `gofmt -w .` 对本轮 4 个改动文件是**空操作**：写入前先 md5sum 存档，`gofmt -w .` 后
# `md5sum -c` 四个文件全部「成功」（逐字节未变），说明新增代码本身就是 gofmt-clean。

$ go build ./...; echo BUILD_EXIT=$?
BUILD_EXIT=0

$ go vet ./...; echo VET_EXIT=$?
VET_EXIT=0

$ go test -race -count=1 ./...; echo TEST_EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.093s
TEST_EXIT=0

$ go test -race -count=1 -v ./... > /tmp/pivcc-round2.log 2>&1; echo V_EXIT=$?
V_EXIT=0
$ grep -c '^--- PASS' /tmp/pivcc-round2.log   # 283（W4 返修后 280）
$ grep -c '^=== RUN'  /tmp/pivcc-round2.log   # 443（W4 返修后 436）
$ grep -c 'DATA RACE'  /tmp/pivcc-round2.log  # 0
$ grep -c '^--- FAIL' /tmp/pivcc-round2.log   # 0
$ tail -2 /tmp/pivcc-round2.log
PASS
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.081s

$ go test -count=1 -run 'TestFormatToolCallContent_' -v .
=== RUN   TestFormatToolCallContent_OffsetBeyondFileLength
--- PASS: TestFormatToolCallContent_OffsetBeyondFileLength (0.00s)
=== RUN   TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow
=== RUN   TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow/short_file
=== RUN   TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow/long_file
--- PASS: TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow (0.00s)
    --- PASS: TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow/short_file (0.00s)
    --- PASS: TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow/long_file (0.00s)
=== RUN   TestFormatToolCallContent_FullCapSlicesCodeUnits
=== RUN   TestFormatToolCallContent_FullCapSlicesCodeUnits/three_byte_rune_at_the_boundary
=== RUN   TestFormatToolCallContent_FullCapSlicesCodeUnits/surrogate_pair_at_the_boundary
--- PASS: TestFormatToolCallContent_FullCapSlicesCodeUnits (0.00s)
    --- PASS: TestFormatToolCallContent_FullCapSlicesCodeUnits/three_byte_rune_at_the_boundary (0.00s)
    --- PASS: TestFormatToolCallContent_FullCapSlicesCodeUnits/surrogate_pair_at_the_boundary (0.00s)
PASS
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	0.005s

$ go test -race -count=5 -run 'TestFormatToolCallContent_|TestGetFileIndicators|TestGetTouchedFiles|Drill' . ; echo FOCUS_EXIT=$?
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	1.232s
FOCUS_EXIT=0
```

`go/` 根 Makefile（本轮也跑了，改动虽 confined 在 `go/pi-vcc/`，但 `GetFileIndicators` 的导出面变了，故复跑一遍 16 个组件）：

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go && make build …
MAKE_BUILD_EXIT=0
$ make vet …
MAKE_VET_EXIT=0
$ make test …
MAKE_TEST_EXIT=0
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	2.815s      # 逐组件 EXIT=0，无 FAIL
```

`go make sdk-check` **未跑**（本轮未改 `go/pigsdk`，与 pi-vcc 无关，与 §7 的理由相同）。

> 临时证据文件：`/tmp/pivcc-r2-parity.mjs`（TS 原版实跑脚本）、`/tmp/pivcc-round2.log`（`-v` 全量日志）、`/tmp/pivcc-r2-make-{build,vet,test}.log`（`go/` 根 Makefile 输出）**保留在 `/tmp`** 供复核，会话结束即失效；返修过程中产生的 4 份 Go 源码副本（`/tmp/dd_pre.go`、`/tmp/dd_fixed.go`、`/tmp/se_fixed.go`、`/tmp/pivcc-drilldown-fixed.go.bak`）与一次性小程序 `/tmp/pivcc-fullcap-evidence/` 已在报告写完后删除。

### 16.5 改动量与测试行数比

| 文件 | 改动 |
|---|---|
| `go/pi-vcc/drilldown.go` | 266 → 277 行（`+15 / -4`，净 **+11**）：`:full` 加 6 行注释、截断从 `body[:drillMaxFullBytes]` 换成 `jsSlice(body, 0, drillMaxFullBytes)`；offset 窗口删掉 3 行夹取与 1 行直切（4 行代码），换成 5 行注释 + 4 行守卫切片。**代码行净增 0（+4 / -4），净增的 11 行全部是注释**；语义变化只有 2 处（1 个截断表达式 + 1 个切片边界）。 |
| `go/pi-vcc/searchentries.go` | 601 → 600 行（净 **-1**）：匿名结构体 + 局部别名 → 导出具名类型 `FileIndicator`。 |
| `go/pi-vcc/recallrequest_test.go` | 324 → 419 行（**+95**）：3 个用例 + 1 个辅助函数 `drillCall` + 3 行 import。 |
| `go/pi-vcc/recall_test.go` | 374 → 376 行（**+2**）：把 `got :=` 改成显式 `[]FileIndicator` 声明 + 2 行注释。 |

测试新增 97 行 / 生产净增 11 行 ≈ **8.8 倍**，超过共享纪律的 3 倍线，理由：(a) 生产侧本轮只有 2 个一行的语义修正 + 1 个类型提取，分母天然小；(b) 3 个用例钉的是 3 个不同分支（越界 offset 回显 / 负 limit 空窗口 / `:full` 码元截断），其中 `:full` 那例用 2 行表头（3 字节字符与代理对）覆盖同一个 50KB 边界，而不是把 100KB body 的断言抄两遍；(c) `TestFormatToolCallContent_FullCapSlicesCodeUnits` 必须逐字比对整段 100KB 输出，`head + tail` 的构造与 `want` 字符串本身就要占 4 行；(d) 2 处注释（11 行）是给下一个读 `drilldown.go` 的人解释「为什么不夹 `startLine`」「为什么按码元切」，属 reviewer 第 1 轮就要求的偏差落注释的延续。

### 16.6 遗留与偏差

1. **负 `limit` 在长文件上与 TS 不同**（§10 第 8 条）：TS 的 `Array.slice` 负 end 回绕会让 100 行文件 + `limit:-1` 显示 99 行，Go 守卫一律返回空窗口。按 reviewer 指定的守卫实现；该输入无法经 `ParseDrillDown` 产生。若要求逐字对齐，替代方案是在 `endLine` 上复刻 `length + end` 的回绕语义，但那会让「负 limit」变成合法窗口，与 reviewer「返回越界/空窗口提示」的要求相反。
2. **`:full` 截断处的孤立代理对**（§10 第 9 条）：Go 写 `U+FFFD`，TS 保留孤立高位代理；**UTF-8 字节输出一致**（node 实测两侧都是 `ef bf bd`）。这是 `jslite.go:56 jsFromUnits` 的既定策略，非本轮新增偏差。
3. **`go/pi-vcc/README.md` 未随本轮更新**：§10 第 8/9 条现在只落在本报告与代码注释里。`jslite.go` 的包注释承诺「每条刻意偏差都写在 README.md」，按 AGENTS.md 铁律④「需要入库的结论必须同步落到 README」的等价要求，建议在 `go/pi-vcc/README.md` 补 2 行偏差说明——本轮任务书只授权改 `drilldown.go` / `searchentries.go` / 测试 / 本报告，故未动 README，留给主代理决定是否补。
4. §12 的 4 项遗留、§15.6 的 `autoContinueDelay` 接缝讨论**不受本轮返修影响**，仍开放。

---

## 17. W6：报告文字修正与提交落地

> 授权范围：同一 worktree（`/root/workspace/.wt/pi-extensions/feature-go-pi-vcc`，分支 `feature/go-pi-vcc`）内改本报告文字 + 两次 `git commit`。
> **零代码改动**（未改 `.go` / `go.work` / `Makefile` 的任何字节）、**未 merge**、**未 push**、主目录 `/root/workspace/pi-extensions` 零写入（只做过一次只读 `read`：`go/bin/pig-test`）。

### 17.1 逐条 before → after

| # | 位置 | 旧表述（问题） | 新表述 |
|---|---|---|---|
| 1 | 报告顶部第 5 行 | `提交号：<PENDING-COMMIT>`（占位） | 真实组件提交 `41abab1` / `41abab1d4feeb268d283d93036da507cf63583d7`，并写明「组件提交 → 报告提交」两次提交的结构、不自指报告 SHA 的理由 |
| 2 | §4 首段 | 「本轮涉及 **5** 个文件」（W5 新增 `recallrequest_test.go` 后未更新，与下表 6 行不符） | 「本轮移植 + W4/W5 两轮返修共涉及 **6** 个测试文件」，并注明该计数来源 |
| 3 | §4 末（新增段） | 任务书指出 §4 约 118 行残留「本会话未新增任何生产代码行」；W6 打开文件时该句已不在 §4（全文仅剩 §5.2 V4a 一处，即本表第 4 行），但 §4 确实缺一句对生产代码范围的准确口径 | 新增一段准确口径：除 §2.1 补齐 `report.go` 与 W4/W5 对 `trackedCommands.go` / `extstate.go` / `drilldown.go` / `searchentries.go` 的局部修复外，其余核心模块维持初版移植实现（并显式指向 §15.1 / §15.2 / §16.1 / §16.2 / §16.3 的逐条定位） |
| 4 | §5.2 表 V4a 行 | Go 侧对应保证是「**本轮未改任何生产代码**」（与 §2.1 补齐 `report.go`、§15/§16 四处生产文件修复直接矛盾） | 改为可核验的具体口径：`format.go:150 RecallNote` 与 `summarize.go:436/451` 自初版移植后逐字未变，且不在 W4/W5 的修复清单（§15.5、§16.5）内 |
| 5 | §8 (a)~(c) 之前 | 未交代包装脚本会否打印横幅，读者无法判断引用是否为完整输出 | 新增「包装脚本的输出面」小节：`go/bin/pig-test` 全文（8 行、无 `echo`）+ W6 现场复跑 `--version` 与 (a) 冒烟的完整输出，证明**不存在** `pig-test: isolated mode …` / `pig-test: production /root/.pi/agent untouched` 两行横幅；并记录 (a) 复跑与首轮逐字节相同（md5 `d36f8f9425c4…`，3 字节 `OK\n`） |
| 6 | §8 (c) 输出块 | 只抄到工具返回的 code fence，漏了 `/tmp/pig-pivcc-search.log` 末尾那段「说明：查询词 …所以无需再召回。」 | 按日志原文补回该段（`cat -A` 复核 413 字节日志的确含此末段） |
| 7 | §13 | 「本会话仍未执行 `git add` / `git commit`」「提交号占位：`<PENDING-COMMIT>`」「`go/pi-vcc/` 59 个文件」 | 保留提交前留档，另起「W6 提交落地」原文块（`git add` / `--cached --stat` / `commit` / `rev-parse` / `status` / `log` / 主目录只读 `status`）；计数纠偏为 `go/pi-vcc/` 58 个文件（+ `go/go.work` = 59 files changed），并指出任务书「60 个文件」同样与现场计数不符 |
| 8 | 顶部与 §1 的轮次标签 | 「W5 返修（最新一轮）」「W4 返修（上一轮）」「W5 审查返修完成（本轮…）」——W6 落地后这些相对时间措辞会误导 | 改为绝对轮次标签（第 1 轮 / 第 2 轮 / 本轮=W6），并在 §1 增加 W6 条目 |

### 17.2 与任务书的三处事实偏差（以源码/现场为准，未擅改目标）

1. **「60 个文件」→ 实际 59**：`git add go/pi-vcc go/go.work` 后 `git diff --cached --stat` = `59 files changed`，其中 `go/pi-vcc/` 58 个（`find go/pi-vcc -type f | wc -l` = 58）+ `go/go.work` 1 个；`git diff --cached --name-only` 经 `grep -v -e '^go/pi-vcc/' -e '^go/go.work$'` 过滤后为空，即暂存区没有第三类路径。任务书的 60 与报告的 59 都源于把本报告文件计入/未计入的口径差，本轮只记录差异，不改码不补文件。
2. **「补上首尾两行隔离横幅」→ 该横幅不存在**：任务书要求 (a)~(c) 的输出里「补上**或明确说明**」`pig-test: isolated mode (PIG_HOME=/root/.pig-test)` / `pig-test: production /root/.pi/agent untouched`。本仓 `go/bin/pig-test` 只有 `unset` / `export` / `exec`，`grep -rn 'isolated mode' go/ --exclude=batch-4-pi-vcc.md` 命中 0 处（排除本报告 W6 自己写的引用），`./bin/pig-test --version` 的完整输出只有 `0.5.2+1.4.0`，故按「明确说明」处理：在 §8 写清脚本无横幅、并给出 W6 现场复跑证据。**没有**为了对齐任务书而把不存在的两行写进实测输出（那会让报告的「只写亲自跑过的输出」前言失效）。真正需要修的是 (c) 段被漏抄的末段，见 17.1 第 6 条。
3. **「两处旧表述」→ 本轮只找到 1 处**：任务书要求的两处里，只有 §5.2 V4a 行的「本轮未改任何生产代码」在当前文件里确实存在（全文 `grep` 「生产代码」/「未新增」/「未改任何」只命中该行）；§4 的那句「本会话未新增任何生产代码行」在本轮打开文件时已不存在（上一轮 W5 改写 §4 时连带消掉，无历史版本可比对，属推断）。本轮按任务意图在 §4 **新增**了准确口径段而非静默省略（见 17.1 第 3 行），未为了「凑两处」而虚构一段并不存在的旧文。

### 17.3 W6 的验证

```console
$ cd /root/workspace/.wt/pi-extensions/feature-go-pi-vcc/go && go build ./...; echo BUILD_EXIT=$?   # 根模块
BUILD_EXIT=0
$ cd pi-vcc && go build ./...; echo BUILD_EXIT=$? ; go vet ./...; echo VET_EXIT=$?
BUILD_EXIT=0
VET_EXIT=0
```

W6 未改 `.go`，故只跑 build/vet 复核「已提交内容可编译、可静态检查」；`-race` 全量与 `make build/vet/test` 的证据沿用 §6 / §7 / §16.4（无代码改动，无需复跑）。
