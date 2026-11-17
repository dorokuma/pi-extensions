---
title: batch-4c — pi-vcc Go 侧同步至上游 0.11.2（三项召回护栏 + PiG 适配台账）
status: 已完成并提交（本分支 docs/pi-vcc-sync-0.11.2；代码提交 2eb4e7e 在 main 上）
worktree: /root/workspace/.wt/pi-extensions/docs-pi-vcc-sync-0.11.2
branch: docs/pi-vcc-sync-0.11.2
base_commit: aa3cad9 docs: standardize repository release policy
upstream: npm @sting8k/pi-vcc@0.11.2（gitHead 2da84918，上游 tag v0.11.2）
author: worker (pi)
date: 2026-11-17
---

# batch-4c — pi-vcc Go 侧同步至上游 `@sting8k/pi-vcc@0.11.2`

## 0. 范围与提交号

本轮只做两件事：**把停在 0.11.1 的文档台账纠正到 0.11.2**（`2eb4e7e` 早已把运行时代码同步完毕），
以及**补齐两组与上游 `tests/recall-output-cap.test.ts` 对等的 Go 测试**。生产代码未改。

代码侧同步提交（均在 `main` 上，0.11.1 基线 `0a73d7c` 之后）：

| 提交 | 日期 | 说明 |
|---|---|---|
| `0258c34` | 2026-10-27 | `fix(pi-vcc): apply context_edit and honour PIG_EXT_HOST_INFO agentDir`（本地适配） |
| `264743b` | 2026-10-27 | `fix(pi-vcc): isolate PIG_EXT_HOST_INFO in settings-path tests`（测试隔离） |
| `aafbbc0` | 2026-10-28 | `test(pi-vcc): clear coding-agent-dir overrides in settings-path test`（测试隔离） |
| `9807ddf` | 2026-10-31 | `fix: resolve agent dir to ~/.pig/agent, never ~/.pi`（本地适配） |
| **`2eb4e7e`** | 2026-10-31 | **`fix(pi-vcc): adopt upstream 0.11.2 context guards`（0.11.2 三项护栏移植）** |
| `b287a63` | 2026-10-31 | `fix(pi-vcc): restore panic recover in globalIndexFor`（本地加固） |

本轮（文档/测试，本分支）：

| 提交 | 说明 |
|---|---|
| `9e86507` | `docs(pi-vcc): bump compare baseline to 0.11.2 and fix stale ~/.pi/agent wording`（`go/pi-vcc/README.md`） |
| `7c708af` | `test(pi-vcc): add 0.11.2 upstream-parity cases for recall cap and BM25`（`recall_test.go`、`recallrun_test.go`） |
| 本提交 | `docs(reports): add batch-4c pi-vcc sync-to-0.11.2 ledger`（本文件） |

## 1. 阶段结论（先读）

1. **上游范围**：`@sting8k/pi-vcc@0.11.2`（2026-10-29，上游 `v0.11.1...v0.11.2` 即 PR `#64` / 修复 issue `#63`）
   只含三项护栏，无新增配置项/命令/参数/摘要小节：① 单条召回结果硬顶 `50 KB`（`capOutput`）；
   ② 搜索摘要单行裁到 `300` 字符窗口（`clipLine`）；③ 搜索与 BM25 语料跳过 `1000+` 字符混合
   Base64 串（`BASE64_RUN` / `looksEncoded` / `searchableText`）。另同步 `vcc_recall` 工具与
   `expand` 参数文案（去掉 `untruncated`，加 `One result is capped at 50 KB.`）。
2. **Go 侧状态**：`2eb4e7e` 已将三项护栏与文案 1:1 移植完成（§2 对照），文档台账滞后是本轮 P1
   修正对象；测试对等缺口是本轮 P2 补齐对象（§3）。
3. **本地验证全绿**（`go/` 经 `go.work`）：`go vet ./pi-vcc/...` EXIT=0；
   `go test ./pi-vcc/...` → `ok github.com/dorokuma/pi-extensions/go/pi-vcc`（含 3 个新增用例）EXIT=0；
   `gofmt -l pi-vcc` 无输出。仓库无 `make check`/`release-check` 目标（`go/Makefile` 只有
   `build/test/vet/sdk-check/…`），故按任务书以 `go vet` + `go test` 为准。
4. **未做**：未跑 `make build && make vet && make test`（15 组件全量，超出本任务范围且与
   本轮文档/测试改动无关——任务书显式只要求 `go vet ./pi-vcc/... && go test ./pi-vcc/...`）；
   未跑 TS 侧 / 隔离 PiG 实测（任务书未要求）；未合并、未推送（无授权）。

## 2. 0.11.2 三项护栏的 Go 落点（`2eb4e7e`）

按上游 `src/core/recall-run.ts`、`src/core/search-entries.ts`、`src/tools/recall.ts` 逐项核对
本仓 `go/pi-vcc/` 源文件与符号（行号为当前 HEAD 实测）：

| 上游 | Go 落点 | 说明 |
|---|---|---|
| `RECALL_OUTPUT_CHARS = 50*1024` | `recallrun.go:22` `RecallOutputChars = 50 * 1024` | 常量同值；注释注明按 UTF-16 码位计 |
| `capOutput(text)` | `recallrun.go:179` `capOutput`；`recallrun.go:173` `RunRecall` 调 `capOutput(IgnoredNote + runAction)` | 切点落在高代理（`[\uD800-\uDBFF]`）上前移 1 单位；头部 `Warning: output clipped at <end> of <len> chars …`，尾注 `\n...[clipped at 50 KB]` |
| `BASE64_RUN = /[A-Za-z0-9+/=]{1000,}/g` | `searchentries.go:226` `base64RunRe` | 同阈值同字符集 |
| `looksEncoded(run)` | `searchentries.go:232` `looksEncoded` | `>=5%` 数字、`>=15%` 大写、`>=15%` 小写三阈值同上游 |
| `searchableText(msg, entry)` | `searchentries.go:255` `searchableText` | 命中编码段替换为 `"…"`（U+2026），只作用于 regex 匹配（`:567`）、BM25 建档（`:594`）与 `lineSnippet` 语料（`:623`）；存储与 `expand` 原文不动 |
| `SNIPPET_LINE_CHARS = 300` | `searchentries.go:322` `snippetLineChars = 300` | 同值 |
| `clipLine(line, at)` | `searchentries.go:327` `clipLine`；`searchentries.go:352` `lineSnippet` 逐行调用（`:385`） | 命中行以首次命中为中心取 300 窗口、上下文行从行首截 300、超长侧加 `"…"`，与上游一致 |
| `recall.ts` 描述/`expand` 文案 | `extension.go:47`（`full text of those entries`）、`extension.go:57`（`One result is capped at 50 KB.`）、`extension.go:61`（`full text: expand:[N]`） | `untruncated` 字样移除、50 KB 提示加入，与上游 `src/tools/recall.ts:29,39,57` 对齐 |

## 3. 本地适配提交（PiG 差异，均在 `2eb4e7e` 之外保留）

| 提交 | Go 落点 | 说明 |
|---|---|---|
| `0258c34`（context_edit） | `beforecompact.go:358` `contextEditSpec`、`:369` `collectContextEdits`、`:398` `applyContextEditToMessage`、`:427` `appendLiveMessage` | `CollectLiveMessages`（`:456` 起）不再一遇 `context_edit` 即返回 nil，改为应用 Pi/PiG 的 omit-or-replace 语义，pig worker 会话可压缩；`settings.go` `AgentDir()` 优先 `sdk.AgentDir()`（`PIG_EXT_HOST_INFO.agentDir`） |
| `9807ddf`（agentdir） | **新增** `agentdir.go:30` `resolveAgentDir`、`:52` `expandAgentDirHome`；`settings.go:91` `AgentDir` 委托之、`settings.go:84` `SettingsPath` 拼 `pi-vcc-config.json` | 解析链 `PIG_EXT_HOST_INFO.agentDir` → `$PI_CODING_AGENT_DIR` → `$PIG_CODING_AGENT_DIR` → `$PIG_HOME/agent` → `~/.pig/agent`，**绝不回退 `~/.pi`**；README 差异 D1 已据此处更正 |
| `b287a63`（recover） | `hook.go:374` `globalIndexFor` 内 `:379` `defer func() { recover() }`；`hook.go:311` 另有一处 `recover` | 会话树异常时 `#N` 索引回退为空表而不是让 recall panic |
| `264743b` / `aafbbc0` | `tokenestimate_test.go` | `settings` 路径测试的环境变量隔离（`PIG_EXT_HOST_INFO` 与 coding-agent-dir 覆盖项清理），属测试基建，无生产语义 |

## 4. 本轮 P1：README 同步（`go/pi-vcc/README.md`，文档-only）

逐处核对语境后改动如下（历史事实描述——「0.11.1 相对 0.10.2 的变更」「0.11.1 用『用户原话』取代
三段推断式小节」——保留原样，未误改）：

| 位置 | 改动 |
|---|---|
| 标题段（原第 3 行） | `@sting8k/pi-vcc@0.11.1` → `@sting8k/pi-vcc@0.11.2` |
| 「来源与加载约定」第 1 条 | 对照基线 `0.11.1` → `0.11.2`；报告引用补 `batch-4c-sync-0.11.2.md` |
| 「摘要小节」标题 | `（0.11.1）` → `（0.11.1 起，0.11.2 未变）`（0.11.2 未改小节构成，标注更准确） |
| 「与 TS 0.11.1 的行为差异」标题 | → `与 TS 0.11.2 的行为差异`；差异清单引用加 `batch-4c-sync-0.11.2.md` |
| 差异 D1 | 定位由 `settings.go:79` 更正为 `agentdir.go`（`resolveAgentDir`，`settings.go:91` `AgentDir` 委托）；末档回退 `~/.pi/agent` → `~/.pig/agent`，与 `9807ddf` 的 `agentdir.go` 实际行为一致 |
| 「其它偏差」末尾 / 「验证」末尾 | 报告引用补 `batch-4c-sync-0.11.2.md`；验证段补一句指向本报告 |

## 5. 本轮 P2：补齐与上游 `tests/recall-output-cap.test.ts` 对等的 Go 测试

上游测试文件取自 `https://raw.githubusercontent.com/sting8k/pi-vcc/v0.11.2/tests/recall-output-cap.test.ts`（实取成功）。Go 侧只加测试、不改生产代码：

| 上游用例 | Go 对应 | 断言要点 |
|---|---|---|
| `leaves the order of the other entries unchanged`（`describe: search ranking with a long unbroken entry`） | `recall_test.go` `TestSearchEntries_BlobDocLeavesPlainOrderUnchanged` | 4 篇普通文档的 `SearchEntries(entries, messages, "max")` 命中次序，与追加一篇内容为 `max ` + 2 MB Base64 run 的 `toolResult`（index 9）后的次序**逐位相等**（排除 index 9 自身后比较）；锁死「`searchableText` 把编码段降为 `…` 后 avgDL 变化不扰乱 BM25 相对排序」 |
| `keeps long joined identifiers searchable` | `recall_test.go` `TestSearchEntries_JoinedIdentifiersStaySearchable` | `capturedNormalCompletionRecordOrdinaryObjectCreateDataProperty` ×30 连写标识符（含双大小写+数字，`looksEncoded` 不得误判）仍被子串 `OrdinaryObjectCreate` 命中，返回唯一 hit index 0 |
| `clips an expand over the cap and says so first` / 600 KB 长行端到端 | `recallrun_test.go` `TestRunRecall_CapsA600KBSingleLine` | 构造 ~600 KB **非 Base64** 单行（`a1,`/`b2,` 逗号分隔符使其不满足 `base64RunRe` 且字符混合不满足 `looksEncoded`，故 `searchableText` 不滤），`needle_mark` 置于保留窗口内：① `ActionSearch` 仍能命中 `#0`；② `ActionExpand` 输出被 `Warning: output clipped at ` 前缀顶住、以 `\n...[clipped at 50 KB]` 收尾、`jsLen(out) <= RecallOutputChars+512`、命中 `#0 [user]` 与 `needle_mark` 在保留体内仍可见 |

已有对等件（`2eb4e7e` 已带，未重复加）：`TestCapOutput_*`、`TestRunRecall_ExpandCapsAt50KB`、
`TestClipLine`、`TestLineSnippet_ClipsContextLinesAt300`、`TestLooksEncoded`、`TestSearchableText`、
`TestSearchEntries_Base64RunCannotDominateBM25`、`TestSearchEntries_Base64RunFilteredOnRegexPath`。

## 6. 验证

在本目录经 `go/go.work` 实跑（环境 `go1.26 linux/amd64`）：

```text
gofmt -l pi-vcc                 # 输出为空
go vet ./pi-vcc/...             # EXIT=0，无输出
go test ./pi-vcc/...            # ok github.com/dorokuma/pi-extensions/go/pi-vcc  EXIT=0
go test ./pi-vcc/... -run 'BlobDocLeaves|JoinedIdentifiers|CapsA600KB' -v
  --- PASS: TestSearchEntries_BlobDocLeavesPlainOrderUnchanged (0.32s)
  --- PASS: TestSearchEntries_JoinedIdentifiersStaySearchable (0.03s)
  --- PASS: TestRunRecall_CapsA600KBSingleLine (0.14s)
  PASS / ok … EXIT=0
```

仓库无 `make check` / `release-check` 目标；`go/Makefile` 目标仅 `build / test / vet / sdk-check /
install-test / examples-*`，本轮按任务书执行 `go vet` + `go test`（显式捕获退出码，见上）。
新增测试均为**先以源码事实核对符号与行为后编写**、首轮即 PASS；`2eb4e7e` 的生产护栏未触发任何
需要"先 FAIL 后 PASS"的 bug 回归路径（属补测试对等件，非修 bug）。

## 7. 边界声明

- 未改 `main`；改动只落在本 worktree 分支 `docs/pi-vcc-sync-0.11.2`。
- 未改任何生产 `.go` 代码；测试仅追加在已有文件（`recall_test.go` / `recallrun_test.go`），不另开新文件。
- 未部署、未触碰 `/root/.pi|/.pig/agent/extensions` 任何部署副本；未合并、未推送。
- 行号引用（`recallrun.go:22,173,179`、`searchentries.go:226,232,255,322,327,352,567,594,623`、
  `extension.go:47,57,61`、`beforecompact.go:358,369,398,427,456`、`agentdir.go:30,52`、
  `settings.go:84,91`、`hook.go:311,374,379`）均为本分支 HEAD 实测，未抄外部报告。

## 8. 遗留 / 未做

- 未跑 `go/` 顶层 `make build && make vet && make test`（15 组件全量）：本轮仅文档 + pi-vcc 测试，
  任务书显式验证范围为 `go vet ./pi-vcc/... && go test ./pi-vcc/...`；如需全量由主代理另派。
- 未跑 TS 侧与隔离 PiG 端到端实测（任务书未要求）。
- 上游未合入 PR（`#41 postTokensEst`、`#61`、`#62 compact_context`、`#65`、`#66 summarizeBranches`）
  均未移植，见上游调研报告 `/tmp/herdr-role-reports/default/role-researcher-7a55af40/report.md` §4/§7。
