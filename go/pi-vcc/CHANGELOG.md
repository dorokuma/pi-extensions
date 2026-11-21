# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/pi-vcc` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.0] - 2026-11-22

`go/pi-vcc` 的首个正式发布版本：`@sting8k/pi-vcc` 上游 `0.11.2`（npm，pi 的算法式对话压缩扩展）
的 PiG Go 移植，含 eqi12 上随附的两个本地补丁（`pi-vcc-0.9.0-ctx-run` 与
`pi-vcc-0.10.0-recall-note`）的原生合入。**本模块版本号按 VERSIONING.md 独立编号
（首个正式版即 `0.1.0`），与上游 npm 版本号无对应关系**——「对齐上游 0.11.2」只描述移植
基线，不表示版本号映射。

### Added

- **对外注册面**（与 TS `index.ts` 五步一致，`Extension()` 为唯一入口，factory 扩展无 `main`/`init`）：
  - 工具 `vcc_recall`（`extension.go` `RegisterRecallTool`）；
  - 斜杠命令 `/pi-vcc`（`RegisterPiVccCommand`）与 `/pi-vcc-recall`（`RegisterVccRecallCommand`）；
  - 事件钩子 `context`、`before_agent_start`、`session_before_compact`、`session_compact`；
  - 落库 compaction entry 的 `details.compactor` 恒为 `"pi-vcc"`、`details.version` 恒为 `2`。
- **摘要小节构成（0.11.1 起，0.11.2 未变）**：`[Early Session Requests]`（本会话前 3 条用户
  消息）、`[Files And Changes]`（Modified/Created/Read 各最新 10 个）、`[Commits]`（最多 8 行
  去重）、`[Tracked Commands]`（按 `trackCommands` 分类）、`[User Messages]`（最新 15 条用户
  原话）；`injected` 消息只进 brief，不进 `[Early Session Requests]` 与 `[User Messages]` 两节。
- **上游 0.11.2 三项召回护栏（`2eb4e7e`）**：单条召回结果硬顶 50 KB（`RecallOutputChars`，
  `capOutput` 高代理对安全切割 + `Warning: output clipped` 头注 + `[clipped at 50 KB]` 尾注）；
  搜索摘要单行裁 300 字符窗口（`clipLine`/`snippetLineChars`）；搜索与 BM25 语料跳过
  `1000+` 字符混合 Base64 串（`base64RunRe`/`looksEncoded`/`searchableText`，仅作用于匹配语料，
  存储与 `expand` 原文不动）；同步 `vcc_recall` 工具与 `expand` 参数文案。
- **两个本地补丁的原生实现**：
  - `pi-vcc-0.9.0-ctx-run`：tracked-commands 对任意 tool_call 块经 `commandTextsOf` 抽取 shell 类
    命令文本（`args.command` string、`args.argv` 数组、`args.commands` 混合数组），让 `ctx_run`
    等 bash 之外的执行通道进入跨压缩记忆；
  - `pi-vcc-0.10.0-recall-note`：`RecallNote` 逐字节等于补丁后常量（提示压缩后先调
    `vcc_recall` 拉回原始指令），`StripRecallNote` 在合并前剥离旧尾注使其不随压缩叠加。
- **PiG 适配（本地补丁之外的平台差异）**：
  - `context_edit` 支持（`0258c34`）：`CollectLiveMessages` 应用 Pi/PiG 的 omit-or-replace 语义，
    pig worker 会话可压缩；
  - agentDir 解析（`9807ddf`，`agentdir.go` `resolveAgentDir`）：链为
    `PIG_EXT_HOST_INFO.agentDir` → `$PI_CODING_AGENT_DIR` → `$PIG_CODING_AGENT_DIR` →
    `$PIG_HOME/agent` → `~/.pig/agent`，**绝不回退 `~/.pi`**；`SettingsPath` 另认
    `$PI_VCC_CONFIG_PATH` 显式覆盖；
  - `globalIndexFor` 会话树异常时 `recover` 回退空表（`b287a63`），`session_before_compact` 生成摘要 `#N` 索引时不再 panic；
  - 每扩展独立状态（`extstate.go` `extensionStates` 按 `*sdk.Extension` 取键）与 JS 语义适配层
    （UTF-16 码位长度/切片、RE2 复刻 JS RegExp 语义、`toFixed` 经 `strconv.FormatFloat`
    实现［IEEE-754 round-half-to-even］、`setTimeout(0)` → `time.AfterFunc` + `recover`）。
- **上游对等测试**：与上游 `tests/recall-output-cap.test.ts` 对等的 Go 用例（BM25 排序不受
  Base64 blob 扰乱、长连写标识符可检索、600 KB 单行 expand 截断端到端），连同既有护栏测试
  与 `context_edit`/`agentDir`/`recover` 的钉住测试。

### Known Risks

- **不能被外部模块直接 `go get` 消费**：`go.mod` 依赖
  `github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`，本地经 `go/go.work` 解析到 `go/pigsdk`，
  无对应上游可拉取版本；本模块仅供 PiG 作为 factory 扩展加载（无 `main`），不作为库发布。
- **未跟进的上游未合入 PR**：`#41`（postTokensEst）、`#61`、`#62`（compact_context）、`#65`、
  `#66`（summarizeBranches）均未移植；若上游后续合并，需另起同步批次。
- **用户消息词数为 UAX#29 近似**（差异 D3）：Go 无 ICU 词典，Han 连续段按 `ceil(n/2)` 计、
  已知仅三处二字中文短回复（`走吧`/`是的`/`没事`）与一处长句与 `Intl.Segmenter` 实测分歧；词数只决定「是否满两个词」，
  不影响实质消息去留。
- **`sortedKeys` 改排序键**：Go map 无插入序，工具参数名输出顺序与 TS `Object.keys` 不同，
  属已记录的有意偏差；完整差异清单见 `go/reports/batch-4-pi-vcc.md` §10 及模块 README。

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/pi-vcc/v0.1.0...HEAD
[0.1.0]: https://github.com/dorokuma/pi-extensions/releases/tag/go/pi-vcc/v0.1.0
