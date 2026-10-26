---
title: batch-4b — pi-vcc Go 侧同步至上游 0.11.1（原生保留两个本地补丁）
status: 已完成并提交（B 档门禁 verifier + reviewer 终审全绿）
worktree: /root/workspace/.wt/pi-extensions/feature-go-pi-vcc-sync-0.11.1
branch: feature/go-pi-vcc-sync-0.11.1
base_commit: ec98b86 docs(go): add batch-4-pi-vcc report
upstream: npm @sting8k/pi-vcc@0.11.1
author: worker (pi)
date: 2026-10-25
---

# batch-4b — pi-vcc Go 侧同步至上游 `@sting8k/pi-vcc@0.11.1`

## 0. 提交号

**代码提交 `0a73d7c`**（完整 SHA：`0a73d7c1c4ffad9e0bf410a359b9d8fbfab4bbbe`；短 SHA：`0a73d7c`），
提交信息：`feat(go): sync pi-vcc to upstream 0.11.1 and preserve local patches`。

B 档提交门禁通过并获批准后，代码提交已在分支 `feature/go-pi-vcc-sync-0.11.1` 上完成；
**未执行 `git merge` / `git push`**，未部署，未写
`/root/.pi/agent/extensions/*.ts`、未改生产 `~/.pi/agent` 配置（隔离实测经
`go/bin/pig-test` 包装脚本强制 `PIG_HOME=/root/.pig-test`）。

## 1. 阶段结论（先读）

1. **范围**：把 `go/pi-vcc`（原对齐 `@sting8k/pi-vcc@0.10.2` + 两个本地补丁）同步到上游
   **0.11.1**，逐条落地 `vcc-sync-report.md` §5.2 的 **表 A–N**，并**原生保留**两个本地补丁
   （`pi-vcc-0.9.0-ctx-run`、`pi-vcc-0.10.0-recall-note`）。
2. **结果**：表 A–N 全部落地（§3），补丁 #1/#2 保活核验通过（§4），`vcc-sync-report.md`
   §5.4 的 6 条抽样行为全部有 Go 测试钉住并通过（§5）。
3. **本地验证全绿**：`gofmt -l .` 无输出、`go build ./...` / `go vet ./...` 无输出且 EXIT=0、
   `go test -race -count=1 ./...` → `ok … 4.063s`（**0 FAIL / 0 SKIP**）、
   补丁专项 `-v -run 'Patch|RecallNote|CommandTextsOf|CtxRun|Verify'` EXIT=0（20 顶层 PASS + 7 子测试）、
   `go/` 下 `make build && make vet && make test` → `EXIT_ALL=0`（15 组件全 ok）。
   实测计数（首轮审查补强后的最终值）：**顶层测试 305 / `=== RUN` 485（含 180 子测试）**，
   逐项口径见 §6.5。
4. **PiG 隔离实测**：`./bin/pig-test -e ./pi-vcc -p "回复 OK"` → `OK`，EXIT=0；
   `./bin/pig-test -e ./pi-vcc -p "请调用 vcc_recall 工具，参数 mode:'touched'…"` →
   `No file operations found in session history.`，EXIT=0（`vcc_recall` 被真实调用并返回真实结果）。
5. **文档**：`go/pi-vcc/README.md` 标题改为「与 TS 0.11.1 的行为差异」，并把文内对报告的引用
   同步补上本文件（§2 列 diff）。
6. **未做/遗留**：未跑 TS 侧（node 端补丁 verify、生产 pi 压缩冒烟）——那是 TS 升级窗格的事；
   新增的 `drillable:false` 过滤只在单测层验证（`recallrun.go` 的 `Drillable` 守卫），未在隔离 PiG 里跑
   真实 recall 端到端；提交已完成（代码提交 `0a73d7c`，B 档门禁通过），合并/部署未做（无授权）。详见 §8。

## 2. 最新修订（本轮改动）

| # | 文件 | 修订内容 |
|---|---|---|
| R1 | `go/pi-vcc/README.md` | 第 79 行标题 `## 与 TS 0.10.2 的行为差异` → `## 与 TS 0.11.1 的行为差异`；来源段/差异段/验证段对报告的引用同步加入 `go/reports/batch-4b-sync-0.11.1.md`（4 处） |
| R2 | `go/reports/batch-4b-sync-0.11.1.md` | 本报告首次落盘（表 A–N 对照、补丁保活核验、§5.4 行为对照、命令输出原文、差异与遗留、`git status` 原文） |
| R3 | （无代码改动） | 本轮**未新增/修改任何 `.go` 代码**；所有 Go 改动均来自上一轮的同步实现（§3 表 A–N 即其逐项对照） |
| R4 | `go/pi-vcc/extract_test.go`、`go/pi-vcc/recallrun_test.go`、`go/pi-vcc/summarize.go` | **首轮审查补强轮**（本轮）：2 项 Should Fix 测试补强 + 1 处注释对齐。① `TestExtractCommits_HashlessAndHashedAreOneCommit` 插入中间提交 `chore: middle`（`def5678`）与尾部重复无 hash 提交，断言 `got` 长度 2 且 `got[0]` 原地升级为 `abc1234`、`got[1]` 为 `def5678`；② 新增 `TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit`（端到端：多路径 `edit` 只列文件不出尾注；同会话叠加 `b.go` 单文件 `write` 后尾注精确指向 `query:'#1:b.go'`）；③ `summarize.go` 合并分支注释对齐上游 `summarize.ts:133`。详见 §5 表第 4/5 行与 §6.2b |
| R5 | `go/reports/batch-4b-sync-0.11.1.md` | 本报告按补强后实测计数更新 §1/§6.1/§6.2/§6.5/§5 表与 §9 状态快照；删除 §6.5 末尾自相矛盾的「与任务书预期不符」段与 §8 第 4 条「计数口径差异待复核」（后续条目顺延） |

R4 中 ③ 的 diff（注释对齐，无行为改动）：

```diff
-	// Every other section (Early Session Requests, Commits): one line per entry,
-	// capped from the tail.
+	// Commits (and any non-pinned fallback section): one line per commit,
+	// capped from the tail.
```

R4 中 ① 的 fixture 顺序（`ExtractCommits` 的 hash 捕获窗口是其后的 2 个 block，故 `-q` 那条自带一个
无 hash 的空结果，`chore: middle` 的结果才不会被首条无 hash 条目抢走）：

```go
{Kind: KindToolCall,   Name: "bash", Args: map[string]any{"command": `git commit -q -m "fix: quiet"`}},
{Kind: KindToolResult, Name: "bash", Text: ""},
{Kind: KindToolCall,   Name: "bash", Args: map[string]any{"command": `git commit -m "chore: middle"`}},
{Kind: KindToolResult, Name: "bash", Text: "[main def5678] chore: middle"},
{Kind: KindToolCall,   Name: "bash", Args: map[string]any{"command": `git commit -m "fix: quiet"`}},
{Kind: KindToolResult, Name: "bash", Text: "[main abc1234] fix: quiet"},
{Kind: KindToolCall,   Name: "bash", Args: map[string]any{"command": `git commit -q -m "fix: quiet"`}},
```

R1 的 diff（相对 HEAD `ec98b86`，仅列本轮新增的 4 处引用与标题改动；同文件另有上一轮已写入的
0.11.1 内容）：

```diff
-  （完整对照清单见 `go/reports/batch-4-pi-vcc.md` §3）。0.11.1 相对 0.10.2 的上游变更已同步，
-  摘要小节构成随之改变，见下文「摘要小节」。
+  （完整对照清单见 `go/reports/batch-4-pi-vcc.md` §3）。0.11.1 相对 0.10.2 的上游变更已同步，
+  摘要小节构成随之改变，见下文「摘要小节」；本轮同步的逐项落地对照（表 A–N）与实测证据见
+  `go/reports/batch-4b-sync-0.11.1.md`。
@@
-## 与 TS 0.10.2 的行为差异
+## 与 TS 0.11.1 的行为差异
@@
-`go/reports/batch-4-pi-vcc.md` §10。
+`go/reports/batch-4-pi-vcc.md` §10；同步到 0.11.1 后的差异复核与遗留项见
+`go/reports/batch-4b-sync-0.11.1.md`。
@@
-未发现"Go 侧行为与 TS 不一致"的实质偏差；如发现，按 `go/reports/batch-4-pi-vcc.md` §5 的用例逐条对照定位。
+未发现"Go 侧行为与 TS 不一致"的实质偏差；如发现，按 `go/reports/batch-4-pi-vcc.md` §5 的用例逐条对照定位，
+0.11.1 同步后的对照结果记在 `go/reports/batch-4b-sync-0.11.1.md`。
@@
-工具在隔离会话中被实际调用并返回真实结果（见 batch-4 报告 §8）。
+工具在隔离会话中被实际调用并返回真实结果（见 batch-4 报告 §8；0.11.1 同步后的复测输出原文
+与退出码见 `go/reports/batch-4b-sync-0.11.1.md`）。
```

## 3. 表 A–N 逐项落地对照

来源：`/root/workspace/vcc-sync-report.md` §5.2（上游改动 → Go 落点）。「测试」列中的
**新增**＝本轮新加的函数，**更新**＝同名测试函数体被改写（逐函数 `git show HEAD:<file>` 与
工作树比对得出），**删除**＝旧版随之移除。所有列出的测试在 §6 的 `-race` 全量跑中 PASS。

| 项 | 上游改动 | 本次改动文件 | 核心符号 | Go 测试（新增/更新/删除） |
|---|---|---|---|---|
| **A** | SectionData 换肤（`sections.ts`/`types.ts`） | `types.go`、`buildSections.go`、`format.go`、`report.go`、`summarize.go` | `SectionData.EarlyRequests` / `.UserMessages` 取代 `SessionGoal` / `UserPreferences` / `OutstandingContext` | 更新：`format_test.go` `TestFormatSummary_SectionsAndOrder`、`TestFormatSummary_EmptySectionsAreOmitted`、`TestFormatSummary_CapBriefTranscriptOption`；`extract_test.go` `TestBuildSections`、`TestBuildSections_BriefBlocksOverride` |
| **B** | Early + User Messages（新 `user-messages.ts`，删 `goals.ts`/`preferences.ts`） | **新增** `userMessages.go`；**删除** `goalsPreference.go` | 新增：`ExtractUserMessages`、`KeepNewestUserMessages`、`userLine`、`userMessageWordCount`、`EARLY_REQUESTS_KEPT`；删除（HEAD 版原有）：`ExtractGoals`、`ExtractPreferences`、`DedupPreferencesAgainstGoals`、`isSubstantiveGoal`、`stripLeadingBullet`、`truncateAtTemplate`、`head`、`clipAll` | 新增：`TestExtractUserMessages_FirstMessagesAreTheEarlyRequests`、`_ChineseMessagesAreKept`、`_InjectedIsLeftOut`、`_DropsToolOpenedText`、`_SkillTextKeepsTheTag`、`TestKeepNewestUserMessages`、`TestUserLine_ClipsAt300`、`TestUserMessageWordCount_IntlSegmenterCalibration`、`TestUserMessageWordCount_ShortRepliesAreDocumented`；删除：`TestExtractGoals_*`（5）、`TestExtractPreferences`、`TestExtractPreferences_OnePerBlock`、`TestDedupPreferencesAgainstGoals`、`TestExtractOutstandingContext`、`TestExtractOutstandingContext_CapsAtFive` |
| **C** | `build-sections.ts` | `buildSections.go` | `HasEarlyRequests`（前 3 条之后才开始收 User Messages）；`formatFileActivity` 改走 `CapItems`；删除 `extractOutstandingContext` | 新增：`TestBuildSections_HasEarlyRequestsSkipsTheFirstMessages`、`TestBuildSections_EarlyRequestsCapAtThree`、`TestBuildSections_InjectedStaysOutOfBothSections`；更新：`TestFormatFileActivity_CapsCategoryAtTen`、`TestBuildSections` |
| **D** | `format.ts`（章节头 + `capItems` 三参） | `format.go` | `CapItems(items, limit, joinWith)`（去掉 `keep head/tail`，**恒保最新** + 前导 `(+N earlier)`）；`formatSummary` 头改为 `Early Session Requests` / `User Messages`；**`RecallNote` 保持补丁加长句不变** | 更新：`TestCapItems`（含 `(+N earlier)` 与 `(+1 earlier) b \| c \| d \| e` 分隔符断言）、`TestCapBrief`、`TestCapBrief_CutsAtASectionHeader`、`TestFormatSummary_*`（3 个）；`patchRecallNote_test.go` `renderSample`、`TestVerify_RecallNote_V4c`、`TestVerify_RecallNote_V4d`（仅 fixture 章节名改写，**尾注断言未改**） |
| **E** | `summarize.ts` merge | `summarize.go` | 新增/改写：`splitSummary`、`sectionHeaderLineRe`、`unwrapSection`、`pinnedEarlyRequests`、`bareCommit`、`dedupCommitLines`、`cleanItemLines`、`isCleanLine`；**保留** `buildRecallNoteRe` / `StripRecallNote`（补丁 #2） | 新增：`TestSectionOf_HeaderAreaOnly`、`TestSplitSummary_NoLeadingHeader`、`TestMergeHeaderSection_EarlyRequestsDedupAndCap`、`TestMergeHeaderSection_CommitsDedupHashedAndHashless`、`TestMergeHeaderSection_UserMessagesKeepsNewest`、`TestDedupCommitLines`、`TestPinnedEarlyRequests_FromEarlySection`、`TestPinnedEarlyRequests_OldSessionGoalDegrades`、`TestUnwrapSection`、`TestMergePrevious_PinsTheEarlyRequests`、`TestCompile_HasEarlyRequestsSkipsTheFreshOnes`、`TestCompile_FirstCompactionCollectsEarlyRequests`；更新：`TestSectionOf`、`TestBriefOf`、`TestMergePrevious_RollsTheBriefForward`、`TestMergePrevious_PreserveFreshBriefCapsTheOldPart`、`TestCompile_StripsPreviousNoteBeforeMerging`、`TestRejoinWrappedSection`；删除：`TestMergeHeaderSection_GoalDedupAndCap`、`TestMergeHeaderSection_OutstandingContextIsFreshOnly`、`TestMergeHeaderSection_DropsSkillLines` |
| **F** | `injected` 传播（`normalize.ts`/`filter-noise.ts`/`types.ts`） | `types.go`、`normalize.go`、`filterNoise.go` | `NormalizedBlock.Injected`、`userBlockInjected`；`FilterNoise` 在重建 user 块时保留 `Injected` | 新增：`TestExtractUserMessages_InjectedSurvivesNormalize`；配套：`TestExtractUserMessages_InjectedIsLeftOut`、`TestBuildSections_InjectedStaysOutOfBothSections` |
| **G** | hook 标记 custom / 保留 `!cmd`（`before-compact.ts`） | `beforecompact.go`、`hook.go` | 新增 `summarizerInputMessage`；`handleSessionBeforeCompact` 用它包装转换后消息（`bashExecution` 不被降级成普通 user） | 新增：`TestSummarizerInputMessage`；更新：`TestPreviewContent`、`TestHook_SessionBeforeCompactProducesSummary` |
| **H** | `extractPaths`（`tool-args.ts`） | `toolargs.go` | `ExtractPaths`（`path` / `file_path` / `paths[]`）、`morePathsSuffix`；`SummarizeToolArgs` 走多路径 | 新增：`TestExtractPaths`、`TestSummarizeToolArgs_MultiPath`；更新：`TestSummarizeToolArgs` |
| **I** | files / brief / render / search / recall-run | `files.go`、`brief.go`、`renderentries.go`、`searchentries.go`、`recallrun.go` | `ExtractFiles` 对每个 `ExtractPaths` 结果归类（Read/Modified/Created）；`toolOneLiner` 多路径 + `+N`；`extractFilesFromContent` 用 `ExtractPaths`；recall touched 写入判定加 `Entry.Drillable` 守卫（`drillable:false` 不进 touched 写入点） | 新增：`TestExtractFiles_MultiPath`、`TestToolOneLiner_MultiPath`、**`TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit`（首轮审查补强轮新增，`Drillable` 守卫的端到端用例）** |
| **J** | `commits.ts` 增强 | `commits.go` | `ExtractCommits` 重写：`commitSegments`、`commitGroup`、`subjectOf`、`subjectFromStdinHeredoc`、`hashFromResults`、`hashForSubject`、`commitHasHash`（组合 flag `-am`/`-qam`、`-F -` heredoc、一条 bash 多 commit、有/无 hash 去重） | 新增：`TestExtractCommits_CombinedFlag`、`TestExtractCommits_StdinHeredoc`、`TestExtractCommits_MultipleInOneCommand`、`TestExtractCommits_ProseDoesNotSplit`、`TestExtractCommits_HashlessAndHashedAreOneCommit`（**补强轮改写**：加中间提交 + 尾部重复无 hash） |
| **K** | `skill-collapse.ts` 精简 | `skillcollapse.go` | 新增 `skillBlockMatches`、`replaceSkillBlocks`、`collapseWhitespace`、`CollapseSkillText`；删除 `CollapseSkillLines` | 更新：`content_test.go` `TestCollapseSkillText`；删除：`TestCollapseSkillLines` |
| **L** | `report.ts` 诊断面 | `report.go` | `probesOf` 探针 `goal`→`request`、去掉 `problem`；`CompactReport` 计数 `GoalsCount`/`BlockersCount` → `EarlyRequestsCount`；`briefLineCountOf` 改走 `briefOf`；`sectionCountOf` 用 `sectionOf` | 更新：`TestReport_SectionCountOf`、`TestReport_BriefLineCountOf`、`TestReport_ProbesOf` |
| **M** | `tracked-commands.ts` 仅 `capItems` 签名 | `trackedCommands.go` | `FormatTrackedCommands` 调用改三参 `CapItems(rendered, CommandsPerName, CommandSeparator)`；**`commandTextsOf` 与非 bash 扫描一字不动（补丁 #1 保活）** | 未改动：`patchCtxRun_test.go`（含 `TestCommandTextsOf_ThreeShapes`、`TestExtractTrackedCommands_NonBashToolIsScanned`、`TestExtractTrackedCommands_UpstreamBashOnlyWouldMissThis`、`TestExtractTrackedCommands_CapKeepsNewest` 等，全部 PASS） |
| **N** | 测试与文档 | `README.md`、本报告、各 `*_test.go` 改名/新增 | 对齐声明改为「`0.11.1` + 两本地补丁」；新增「摘要小节（0.11.1）」与 D3 小节 | 见上各行；测试总数见 §6.5 |

对照结论：**14 项（A–N）全部落地**。表 A–N 之外无额外改动；`go/` 内其它 16 个组件与本轮无关
（`make test` 仅作回归证据）。

## 4. 两个本地补丁保活核验（必做项）

### 4.1 补丁 #1 `pi-vcc-0.9.0-ctx-run`（`trackedCommands.go`）

| 保活点 | 证据 |
|---|---|
| `commandTextsOf(args)` 存在且三形状齐全 | `trackedCommands.go:225` `func commandTextsOf(args map[string]any) []string`；`args.command`(string)、`args.argv`(数组，非 string 丢弃后 `strings.Join(tokens, " ")`，空则不贡献)、`args.commands`(元素为 string 或含 string `command` 的对象) |
| 非 bash 扫描（主循环不再限定 `b.name == "bash"`） | `trackedCommands.go:288-293`：只判 `block.Kind != KindToolCall`，随后对任意 tool_call 调 `commandTextsOf(block.Args)`；无 shell 字段的工具天然 no-op |
| 前导 `&` 保护（不索引 `command[-1]`） | `trackedCommands.go:45-49`：`case c == '&' && (byteAt(command, i-1) == '>' \|\| byteAt(command, i-1) == '<' \|\| byteAt(command, i+1) == '>')` → `continue`；注释明写 `byteAt` 守卫左邻，使前导 `&` 读作 0（等价 TS `cmd[i-1] === undefined`）而非 `-1` panic。测试用 `{"leading ampersand is a separator", "& git status", …}` 与 `{"leading ampersand redirection is not a separator", "&>out git status", …}` 双向钉住（`patchCtxRun_test.go:200-207`） |
| 测试未被削弱 | `patchCtxRun_test.go` 在本轮 **完全未改动**（`git status` 无该文件），14 个测试函数（含 `TestVerifyEqi12_SyntheticMixed`、`TestVerifyEqi12_TrackedLinePrefix`、`TestExtractTrackedCommands_SessionFileReplay`）全 PASS |

### 4.2 补丁 #2 `pi-vcc-0.10.0-recall-note`（`format.go` `RecallNote`）

`format.go:146-154` 注释明写「RECALL_NOTE **with the patch applied**」，常量为（与原句一字不改 + 追加句）：

```go
const RecallNote = "Use `vcc_recall` to search for prior work, decisions, and context from before this summary. " +
	"Do not redo work already completed." +
	" After any compaction, first call vcc_recall to pull this task's original instructions " +
	"(the first user message) back before continuing; never judge whether it is needed."
```

- `summarize.go` 的 `buildRecallNoteRe()` 仍**由该常量动态生成**正则、`StripRecallNote()` 仍据此剥离旧尾注，
  故尾注不随压缩叠加；本轮同步 0.11.1 的 merge（A–N 的 E 项）后这两者**未被换成静态正则**。
- `patchRecallNote_test.go` 本轮仅改 fixture 的章节名（`renderSample`、`TestVerify_RecallNote_V4c`、`TestVerify_RecallNote_V4d` 的 `SectionData` 字段），
  **尾注断言（`TestRecallNoteExactValue` 的逐字节值、V1a…V4d 的头尾对照、叠加剥离）一字未改**。

### 4.3 补丁专项测试输出（原文，`-race -count=1 -v`）

命令与输出见 §6.2；结果 **EXIT=0**，`RUN 27`（顶层 20 + 子测试 7），`--- FAIL` 0 条。其中与本两补丁
直接相关的顶层用例：`TestExtractTrackedCommands_CtxRunPatchShapes`、`TestVerifyEqi12_SyntheticMixed`、
`TestVerifyEqi12_TrackedLinePrefix`、`TestCommandTextsOf_ThreeShapes`（7 子测试）、`TestRecallNoteExactValue`、
`TestVerify_RecallNote_V1a…V3c`、`TestVerify_RecallNote_V4c`、`TestVerify_RecallNote_V4d`、
`TestStripRecallNote_IdempotentAcrossWrapping`、`TestStripRecallNote_RemovesStackedCopies`、
`TestCompile_AppendsRecallNoteOnce`、`TestBuildRecallNoteRe_MatchesTheWrappedNote` —— 全 PASS。

## 5. `vcc-sync-report.md` §5.4 行为对照清单（6 条抽样）与 Go 测试

| # | 抽样行为（§5.4 原文） | 对应 Go 测试函数 | 结果 |
|---|---|---|---|
| 1 | 多轮压缩后 Files 列表跟随最近文件（非冻在最早 10 个） | `TestCapItems`（`(+2 earlier) c, d, e` / `(+1 earlier) b \| c \| d \| e`，恒保最新）、`TestFormatFileActivity_CapsCategoryAtTen`、`TestExtractTrackedCommands_CapKeepsNewest`、`TestFormatSummary_SectionsAndOrder` | PASS |
| 2 | 中文用户消息进入 `[User Messages]` / `[Early Session Requests]` | `TestExtractUserMessages_ChineseMessagesAreKept`、`TestExtractUserMessages_FirstMessagesAreTheEarlyRequests`、`TestBuildSections_EarlyRequestsCapAtThree`、`TestMergeHeaderSection_EarlyRequestsDedupAndCap`、`TestMergeHeaderSection_UserMessagesKeepsNewest`、`TestCompile_FirstCompactionCollectsEarlyRequests` | PASS |
| 3 | `custom_message` 不进 Early/User Messages | `TestExtractUserMessages_InjectedIsLeftOut`、`TestExtractUserMessages_InjectedSurvivesNormalize`、`TestBuildSections_InjectedStaysOutOfBothSections` | PASS |
| 4 | 一条 bash 里两次 `git commit` 都进 `[Commits]` | `TestExtractCommits_MultipleInOneCommand`、`TestExtractCommits_CombinedFlag`、`TestExtractCommits_StdinHeredoc`、`TestExtractCommits_HashlessAndHashedAreOneCommit`（**本轮补强**：插入 `chore: middle` + 尾部重复无 hash，钉住「后来捕获的 hash 原地升级首现条目、不移尾、不被重复无 hash 降级」）、`TestMergeHeaderSection_CommitsDedupHashedAndHashless`、`TestDedupCommitLines` | PASS |
| 5 | `paths: [...]` 编辑工具文件进 Modified / recall touched | `TestExtractPaths`、`TestExtractFiles_MultiPath`、`TestSummarizeToolArgs_MultiPath`、`TestToolOneLiner_MultiPath`、**`TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit`（本轮新增，端到端钉住 `drillable:false` 多路径编辑只列文件、不出 `#N:path` 尾注）** | PASS |
| 6 | 补丁 #1/#2 行为与升级前一致 | §4.1/§4.2 的两套补丁测试（`patchCtxRun_test.go` 未改动 + `patchRecallNote_test.go` 断言未改），另有 `TestCompile_AppendsRecallNoteOnce`、`TestStripRecallNote_RemovesStackedCopies` | PASS |

证据口径：以上全部来自 §6.1 的 `go test -race -count=1 ./...`（0 FAIL）、§6.2 的补丁专项 `-v` 跑与
§6.2b 的补强用例 `-v` 跑；**未**在 TS/node 侧复跑（见 §8 遗留项）。

## 6. 本地验证：命令与输出原文

环境：`go version go1.26.4 linux/amd64`（`/root/workspace/…/go/pi-vcc` 实跑）；
隔离实测脚本 `go/bin/pig-test` 内写死 `PIG_HOME=/root/.pig-test` 并 `unset PIG_CODING_AGENT_DIR /
PIG_CODING_AGENT_SESSION_DIR / PIG_USE_PI_DIRS`。

### 6.1 `go/pi-vcc`：gofmt / build / vet / 全量 race 测试

```console
$ gofmt -l .
（无输出）
exit 0

$ go build ./...
（无输出）
exit 0

$ go vet ./...
（无输出）
exit 0

$ go test -race -count=1 ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	4.063s
exit 0
```

（`gofmt -l .` 输出为空＝无文件需格式化；`go test` 输出仅 `ok` 一行，无 `DATA RACE`、无 `FAIL`。
耗时 4.063s 为首轮审查补强（新增 1 个顶层用例）后的重跑值。）

### 6.2 补丁专项 `-race -count=1 -v`（完整输出，56 行）

```console
$ go test -race -count=1 -v -run 'Patch|RecallNote|CommandTextsOf|CtxRun|Verify' ./...
=== RUN   TestExtractTrackedCommands_CtxRunPatchShapes
--- PASS: TestExtractTrackedCommands_CtxRunPatchShapes (0.00s)
=== RUN   TestVerifyEqi12_SyntheticMixed
--- PASS: TestVerifyEqi12_SyntheticMixed (0.00s)
=== RUN   TestVerifyEqi12_TrackedLinePrefix
--- PASS: TestVerifyEqi12_TrackedLinePrefix (0.00s)
=== RUN   TestCommandTextsOf_ThreeShapes
=== RUN   TestCommandTextsOf_ThreeShapes/command_only
=== RUN   TestCommandTextsOf_ThreeShapes/argv_only,_non-strings_dropped
=== RUN   TestCommandTextsOf_ThreeShapes/argv_with_no_string_elements_contributes_nothing
=== RUN   TestCommandTextsOf_ThreeShapes/commands_with_strings_and_objects
=== RUN   TestCommandTextsOf_ThreeShapes/all_three_shapes_are_collected_in_order
=== RUN   TestCommandTextsOf_ThreeShapes/a_non-shell_tool_contributes_nothing
=== RUN   TestCommandTextsOf_ThreeShapes/no_arguments_at_all
--- PASS: TestCommandTextsOf_ThreeShapes (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/command_only (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/argv_only,_non-strings_dropped (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/argv_with_no_string_elements_contributes_nothing (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/commands_with_strings_and_objects (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/all_three_shapes_are_collected_in_order (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/a_non-shell_tool_contributes_nothing (0.00s)
    --- PASS: TestCommandTextsOf_ThreeShapes/no_arguments_at_all (0.00s)
=== RUN   TestRecallNoteExactValue
--- PASS: TestRecallNoteExactValue (0.00s)
=== RUN   TestVerify_RecallNote_V1a
--- PASS: TestVerify_RecallNote_V1a (0.00s)
=== RUN   TestVerify_RecallNote_V1b
--- PASS: TestVerify_RecallNote_V1b (0.00s)
=== RUN   TestVerify_RecallNote_V1c
--- PASS: TestVerify_RecallNote_V1c (0.00s)
=== RUN   TestVerify_RecallNote_V1d
--- PASS: TestVerify_RecallNote_V1d (0.00s)
=== RUN   TestVerify_RecallNote_V2
--- PASS: TestVerify_RecallNote_V2 (0.01s)
=== RUN   TestVerify_RecallNote_V2b
--- PASS: TestVerify_RecallNote_V2b (0.00s)
=== RUN   TestVerify_RecallNote_V3a
--- PASS: TestVerify_RecallNote_V3a (0.00s)
=== RUN   TestVerify_RecallNote_V3b
--- PASS: TestVerify_RecallNote_V3b (0.00s)
=== RUN   TestVerify_RecallNote_V3c
--- PASS: TestVerify_RecallNote_V3c (0.00s)
=== RUN   TestVerify_RecallNote_V4c
--- PASS: TestVerify_RecallNote_V4c (0.00s)
=== RUN   TestVerify_RecallNote_V4d
--- PASS: TestVerify_RecallNote_V4d (0.00s)
=== RUN   TestStripRecallNote_IdempotentAcrossWrapping
--- PASS: TestStripRecallNote_IdempotentAcrossWrapping (0.00s)
=== RUN   TestStripRecallNote_RemovesStackedCopies
--- PASS: TestStripRecallNote_RemovesStackedCopies (0.00s)
=== RUN   TestCompile_AppendsRecallNoteOnce
--- PASS: TestCompile_AppendsRecallNoteOnce (0.00s)
=== RUN   TestBuildRecallNoteRe_MatchesTheWrappedNote
--- PASS: TestBuildRecallNoteRe_MatchesTheWrappedNote (0.00s)
PASS
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	1.060s
exit 0
```

### 6.2b 首轮审查补强用例实跑（`-race -count=1 -v`）

Should Fix 1/2 的两个用例（改写的 `TestExtractCommits_HashlessAndHashedAreOneCommit` 与新增的
`TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit`）连同原 `TestRunRecall_Touched` 一起实跑：

```console
$ go test -race -count=1 -v -run 'TestExtractCommits_HashlessAndHashedAreOneCommit|TestRunRecall_Touched' ./...
=== RUN   TestExtractCommits_HashlessAndHashedAreOneCommit
--- PASS: TestExtractCommits_HashlessAndHashedAreOneCommit (0.00s)
=== RUN   TestRunRecall_Touched
--- PASS: TestRunRecall_Touched (0.00s)
=== RUN   TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit
--- PASS: TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit (0.00s)
PASS
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	1.023s
exit 0
```

补丁专项（§6.2 上方）计数**不受本轮影响**（新用例不匹配 `-run` 模式）：仍为 `RUN` 27
（顶层 20 + `TestCommandTextsOf_ThreeShapes` 的 7 子测试），顶层 PASS 20，FAIL 0，EXIT=0。

### 6.3 `go/`：`make build && make vet && make test`

先分开实跑（此处的耗时/输出为本轮真实未缓存结果）：

```console
$ make build
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
==> [component: pi-vcc] go vet ./...
==> [root module] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/internal/version	0.002s
==> [component: auto-continue] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/auto-continue	0.010s
==> [component: cache-stats] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/cache-stats	0.062s
==> [component: codegraph-go] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/codegraph-go	0.219s
==> [component: ctxmode] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctxmode	3.666s
==> [component: ctx-orchestrate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctx-orchestrate	0.025s
==> [component: exclude-tools-guard] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	0.209s
==> [component: herdr-agent-state] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	0.428s
==> [component: jev-gate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/jev-gate	2.100s
==> [component: moshi-hooks] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/moshi-hooks	0.348s
==> [component: no-tables] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/no-tables	0.005s
==> [component: pi-context-pruning] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	0.004s
==> [component: pi-env] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-env	0.004s
==> [component: pi-tinyfish] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	0.064s
==> [component: pi-vcc] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	2.808s
EXIT_TEST=0
```

随后**再次链式**执行任务书原文命令，退出码合并为 0（`go test` 命中缓存显示 `(cached)`）：

```console
$ make build && make vet && make test; echo "EXIT_ALL=$?"
…（build/vet 的 30 行组件标题省略，与上同）…
==> [component: pi-vcc] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-vcc	(cached)
EXIT_ALL=0
```

### 6.4 `go/bin/pig-test -e ./pi-vcc`（PiG 隔离目录加载实测）

```console
$ ./bin/pig-test -e ./pi-vcc -p "回复 OK"
OK
exit 0
```

```console
$ ./bin/pig-test -e ./pi-vcc -p "请调用 vcc_recall 工具，参数 mode:'touched'，把工具返回的原文贴出来"
工具返回原文如下：

```
No file operations found in session history.
```

即：本会话历史中没有任何文件操作记录（当前是全新会话，尚未读写过文件）。
exit 0
```

结论：扩展在隔离 PiG 环境中被加载；第二条命令中 `vcc_recall`（`mode:'touched'`）被**真实调用**
并返回工具的真实输出（隔离会话历史为空 → `No file operations found in session history.`）。
两次调用均 `EXIT=0`；隔离目录由脚本强制为 `/root/.pig-test`，未触碰生产
`/root/.pi/agent`。命令前的 `-e ./pi-vcc` 指向工作树内的 Go 扩展目录（未经 install.sh、未部署）。

（首轮审查补强轮未重跑 PiG 隔离实测：本轮只改 `go/pi-vcc` 的测试与一处注释，不影响扩展加载与
工具调用路径；`make build && make vet && make test` 已在补强后重跑，`EXIT_ALL=0`、15 组件全 ok，
其中 `pi-vcc` 为 `ok … 2.792s`。）

### 6.5 测试计数与口径

| 口径 | 数值 |
|---|---|
| `go test -race -count=1 -v ./...` 的 `=== RUN` 总数 | **485**（顶层 305 + 子测试 180） |
| 其中顶层 `--- PASS` | **305**，`--- FAIL` 0，`--- SKIP` 0 |
| 源码中 `^func Test*` 声明（含 `func TestMain`，非测试） | 306 → 实际测试函数 305 |
| 补丁专项 `-v`（§6.2） | `RUN` 27（顶层 20 + `TestCommandTextsOf_ThreeShapes` 的 7 子测试），顶层 PASS 20，FAIL 0 |
| 本轮补强带来的增量 | 顶层 +1 / `=== RUN` +1（新增 `TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit`；`TestExtractCommits_HashlessAndHashedAreOneCommit` 为同名改写，不增计数） |

口径说明：`=== RUN` 以 `=== RUN` 行计（含子测试），顶层以不含 `/` 的 `=== RUN` / `--- PASS` 行计；
`go test -race -count=1 ./...` 的汇总行仍为单行 `ok`（无 FAIL / 无 DATA RACE / 无 SKIP）。
所有数字均为 `go/pi-vcc` 目录下实跑所得，未做任何“凑数”调整。

## 7. 与 TS 0.11.1 的行为差异

按「可观测行为」口径；同步 0.11.1 后逐条复核（与 `go/pi-vcc/README.md` 同步）：

### D1 配置路径解析（`settings.go:79` `AgentDir()` / `SettingsPath()`）

TS 固定 `homedir()/.pi/agent/pi-vcc-config.json`；Go 侧按 `$PI_CODING_AGENT_DIR` →
`$PIG_CODING_AGENT_DIR` → `$PIG_HOME/agent` → `~/.pi/agent` 解析，另认 `$PI_VCC_CONFIG_PATH` 显式覆盖。
本轮同步**未改动**该逻辑；隔离实测正是依赖它（顺手证明生效：`pig-test` 用 `PIG_HOME=/root/.pig-test`
而生产配置未被读写）。测试围栏：`extension_test.go` `TestMain` 把 `PI_VCC_CONFIG_PATH` 指向临时文件。

### D2 每扩展独立状态（`extstate.go`）

TS 把 `lastStats` / `lastCompactWasPiVcc` / `pendingFollowUpPrompt` / auto-continue 定时器放模块作用域；
Go 收进 `extensionStates` 映射按 `*sdk.Extension` 取用。0.11.1 同步（`hook.go`/`beforecompact.go`）
继续走同一状态入口，未新增模块级可变状态。由 `TestExtState_IsKeyedPerExtension` 钉住。

### D3 用户消息词数（`userMessages.go` `userMessageWordCount`）与 JS `Intl.Segmenter` 的校准对照

TS 用 `new Intl.Segmenter(undefined, { granularity: "word" })`；Go 无 ICU/词典数据，按 UAX#29 字符类近似：
字母/数字连续段一词（跨撇号、数字间的 `.`/`,` 合并）、每个 Han 连续段 `ceil(n/2)` 词、平假名/片假名一字一词。

校准对照表（ICU 列 = **本轮在本机重跑 node 实测**：`node v26.7.0`，`Intl.Segmenter` 解析 locale = `zh-CN`，
按 `isWordLike` 计数；Go 列 = 本轮 Go 侧断言值，与测试 `TestUserMessageWordCount_IntlSegmenterCalibration`
的 `goWant` 列一致）：

| 输入 | ICU（node 实测） | Go | 一致 |
|---|---|---|---|
| `hello world` | 2 | 2 | ✅ |
| `don't` | 1 | 1 | ✅ |
| `e-mail` | 2 | 2 | ✅ |
| `3.14` | 1 | 1 | ✅ |
| `3-5` | 2 | 2 | ✅ |
| `한글 테스트` | 2 | 2 | ✅ |
| `中文消息测试` | 3 | 3 | ✅ |
| `好的` | 1 | 1 | ✅ |
| `没问题` | 2 | 2 | ✅ |
| `ひらがな` | 4 | 4 | ✅ |
| `中文a测试` | 3 | 3 | ✅ |
| `（）【】` | 0 | 0 | ✅ |
| `🎉 很好` | 1 | 1 | ✅ |
| `把 Go 版 pi-vcc 同步升级到上游 0.11.1 并且保留两个本地补丁` | **16** | **15** | ❌ 唯一长句分歧（ICU 词典比 `ceil(n/2)` 多读 1 词） |
| `走吧` / `是的` / `没事`（短回复，`TestUserMessageWordCount_ShortRepliesAreDocumented`） | 2 / 2 / 2 | 1 / 1 / 1 | ❌ 两字中文回复：ICU 词典读 2 词，Go 读 1 词 |

影响评估（与前一轮结论一致、本轮未变）：词数只用于「是否满两个词」的门槛判定，长句分歧不改变任何
消息的去留；短回复分歧会让两字中文回复在 Go 侧可能被丢弃而 TS 保留（**丢弃的是「走吧/是的/没事」这类
无语义承接词，不含任何实质指令**）；「实质指令永不丢」由 `TestExtractUserMessages_ChineseMessagesAreKept` 钉住。

### 其它偏差（0.10.2 期已记录，0.11.1 同步后仍然成立/未变）

- `sortedKeys`（`content.go`）：TS `Object.keys` 插入序 → Go 排序键（仅影响工具参数名这类“看似未指定”的输出）。
- JS 语义适配层（`jslite.go`/`jsregex.go`/`jsmath.go`）：UTF-16 码位取长切片、`toFixed(1)` 用 `math/big`
  half-away-from-zero、RegExp 在 RE2 上复刻 JS 语义（`.` 不吃行终止符、`\s` 含 Unicode 分隔符 + U+FEFF、
  `i` 用 Unicode simple case folding、`\uXXXX` 展开、JS `\b`）。0.11.1 的 commits 解析（组合 flag / heredoc / 多 commit）
  依赖这套 JS 语义复刻，测试见 A–N 表 J 行。
- `setTimeout(..., 0)` → `time.AfterFunc` + `recover`（auto-continue 不可见续跑，`extstate.go`）。
- JSON 数值到 `any` 得 `float64`：断言/格式化前统一 `jsNumberString` / `jsFixed1`，避免 `1` 渲染成 `1.0`。
- 0.11.1 新增的 `drillable: false`（无 per-file content 的编辑）在 recall touched 写入点由 `recallrun.go`
  的 `file.Entries[i].Drillable` 守卫挡住 —— 这是 0.11.1 新引入的行为，已在代码注释里与 `recall-run.ts` 对齐。

**未发现**「Go 侧与 TS 0.11.1 不一致」的实质偏差；如后续发现，按 `batch-4-pi-vcc.md` §5 的用例逐条对照。

## 8. 遗留项与未做事项（没跑的写没跑）

1. **提交状态**：已提交到 `feature/go-pi-vcc-sync-0.11.1`（代码提交 `0a73d7c`），按指令暂不合并进
   `feature/go-batch1` 或 `main`，未部署。
2. **TS（node）侧未跑**：`~/.pi/agent/patches/**/verify*.mjs`、生产 pi 的
   `pi update --extensions` → 重打补丁 → 压缩冒烟 → 重启，均属 TS 升级窗格，本 worktree **未执行**，
   本报告不对其作任何「应该可以」的推断。
3. **未做端到端 pi 压缩实测**：Go 侧只做了 `go test` 全绿 + 隔离 `pig-test` 加载/工具调用；
   「压缩一次后摘要尾注含补丁句」「一次 bash 两次 `git commit` 都进 `[Commits]`」是在
   Go 单测层（§5 表）钉住的，未在隔离 PiG 里跑真实压缩周期。
4. **`make examples-*` / `sdk-check` / `install-test` 未跑**：与本轮 pi-vcc 改动的关联为 0，
   故未执行（`sdk-check` 依赖 `/root/.pig-test/state/pigsdk/sdk-*` 暂存 SDK，本轮未校验 md5 一致性）。
5. **未动生产目录**：`/root/.pi/agent/extensions/*.ts`、`~/.pi/agent/patches/**`、`~/.pi/agent/npm/**`
   本轮零写入（隔离实测走 `PIG_HOME=/root/.pig-test`）。
6. **`go/pi-vcc/goalsPreference.go` 为删除**（内容搬入 `userMessages.go` 语义）；若 reviewer 需要
   旧实现对照，取 HEAD `ec98b86` 的该文件即可。

## 9. `git status --porcelain -b` 原文

```console
$ git status --porcelain -b
## feature/go-pi-vcc-sync-0.11.1
 M go/pi-vcc/README.md
 M go/pi-vcc/beforecompact.go
 M go/pi-vcc/beforecompact_test.go
 M go/pi-vcc/brief.go
 M go/pi-vcc/buildSections.go
 M go/pi-vcc/commits.go
 M go/pi-vcc/content_test.go
 M go/pi-vcc/extract_test.go
 M go/pi-vcc/files.go
 M go/pi-vcc/filterNoise.go
 M go/pi-vcc/format.go
 M go/pi-vcc/format_test.go
 D go/pi-vcc/goalsPreference.go
 M go/pi-vcc/hook.go
 M go/pi-vcc/hook_test.go
 M go/pi-vcc/normalize.go
 M go/pi-vcc/patchRecallNote_test.go
 M go/pi-vcc/recallrun.go
 M go/pi-vcc/recallrun_test.go
 M go/pi-vcc/renderentries.go
 M go/pi-vcc/report.go
 M go/pi-vcc/report_test.go
 M go/pi-vcc/searchentries.go
 M go/pi-vcc/skillcollapse.go
 M go/pi-vcc/summarize.go
 M go/pi-vcc/summarize_test.go
 M go/pi-vcc/toolargs.go
 M go/pi-vcc/trackedCommands.go
 M go/pi-vcc/types.go
?? go/pi-vcc/userMessages.go
?? go/reports/batch-4b-sync-0.11.1.md
```

（上为**本报告再次落盘之后**的最终状态：相对 §2 R2 首次落盘时多出 `extract_test.go`、
`recallrun_test.go`、`summarize.go` 三个 `M`（首轮审查补强轮的 3 个代码项；第 4 项是本报告自身的
计数清理，已包含在最后的 `??` 行里）。**无任何已暂存内容、未 commit。**）

要点：所有改动均在 `go/pi-vcc/` 与 `go/reports/`（即本 worktree 的 `go/` 目录）内；工作分支
`feature/go-pi-vcc-sync-0.11.1`；**无任何已暂存内容、未 commit**（以上均为提交前快照时点；
代码提交 `0a73d7c` 完成后的状态见下 §9.1）；`go/` 下其它组件、
`install.sh`、`federated/registry.json`、`.wt` 登记文件均未改动。

### 9.1 代码提交与 B 档门禁记录（提交后回填）

代码提交 `0a73d7c`（完整 SHA `0a73d7c1c4ffad9e0bf410a359b9d8fbfab4bbbe`）：`git show --stat 0a73d7c` 原文：

```console
$ git show --stat 0a73d7c
commit 0a73d7c1c4ffad9e0bf410a359b9d8fbfab4bbbe
Author: dorokuma <dorokuma@users.noreply.github.com>
Date:   Mon Oct 26 10:22:50 2026 +0800

    feat(go): sync pi-vcc to upstream 0.11.1 and preserve local patches

 go/pi-vcc/README.md               |  45 +++-
 go/pi-vcc/beforecompact.go        |  22 ++
 go/pi-vcc/beforecompact_test.go   |  26 +++
 go/pi-vcc/brief.go                |   8 +-
 go/pi-vcc/buildSections.go        | 111 +++-------
 go/pi-vcc/commits.go              | 270 ++++++++++++++++-------
 go/pi-vcc/content_test.go         |  73 ++++---
 go/pi-vcc/extract_test.go         | 440 ++++++++++++++++++++++++++++----------
 go/pi-vcc/files.go                |  26 +--
 go/pi-vcc/filterNoise.go          |   5 +-
 go/pi-vcc/format.go               |  28 +--
 go/pi-vcc/format_test.go          |  49 +++--
 go/pi-vcc/goalsPreference.go      | 207 ------------------
 go/pi-vcc/hook.go                 |   2 +-
 go/pi-vcc/hook_test.go            |   4 +-
 go/pi-vcc/normalize.go            |   5 +-
 go/pi-vcc/patchRecallNote_test.go |  39 ++--
 go/pi-vcc/recallrun.go            |   4 +-
 go/pi-vcc/recallrun_test.go       |  63 ++++++
 go/pi-vcc/renderentries.go        |   8 +-
 go/pi-vcc/report.go               |  43 ++--
 go/pi-vcc/report_test.go          |  22 +-
 go/pi-vcc/searchentries.go        |  31 ++-
 go/pi-vcc/skillcollapse.go        |  95 ++++----
 go/pi-vcc/summarize.go            | 282 +++++++++++++++++-------
 go/pi-vcc/summarize_test.go       | 283 ++++++++++++++++++++----
 go/pi-vcc/toolargs.go             |  38 +++-
 go/pi-vcc/trackedCommands.go      |   2 +-
 go/pi-vcc/types.go                |  23 +-
 go/pi-vcc/userMessages.go         | 177 +++++++++++++++
 30 files changed, 1633 insertions(+), 798 deletions(-)
```

（代码提交完成后、本报告提交前，`git status --porcelain -b` 仅剩一条
`?? go/reports/batch-4b-sync-0.11.1.md`；报告提交后工作树完全干净。）

B 档提交门禁记录（终审全绿后批准提交）：

- 首轮 `verifier` `w3B:pH`：4/4 PASS
- 首轮 `reviewer` `w3B:pN`
- 次轮 `reviewer` `w3B:pQ`：终审闭环
- 次轮 `verifier` `w3B:pR`：V1～V4 4/4 PASS

## 10. 交付物

| 交付物 | 路径 |
|---|---|
| 同步后的 Go 实现（29 个已跟踪文件改动/删除 + 1 新增，共 30 个文件，表 A–N） | `go/pi-vcc/`（本 worktree） |
| 更新后的说明 | `go/pi-vcc/README.md`（标题 + 引用，§2 R1） |
| 首轮审查补强（2 项 Should Fix 测试 + 1 处注释对齐） | `go/pi-vcc/extract_test.go`、`go/pi-vcc/recallrun_test.go`、`go/pi-vcc/summarize.go`（§2 R4） |
| 本报告 | `go/reports/batch-4b-sync-0.11.1.md` |

以上为 worker 自测证据，不替代 reviewer / verifier 的独立核验；提交号已回填（代码提交 `0a73d7c`，见 §0/§9.1）。