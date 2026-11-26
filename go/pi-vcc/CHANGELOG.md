# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/pi-vcc` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.0] - 2026-11-22

`go/pi-vcc` 首个正式发布版本：`@sting8k/pi-vcc`（0.11.2）的 PiG Go 移植版本，并原生合入 `ctx-run` 命令追踪与 `recall-note` 尾注两个本地补丁（模块版本独立编号）。

### Added

- **对外接口**：提供工具 `vcc_recall`、斜杠命令 `/pi-vcc` 与 `/pi-vcc-recall`，以及上下文压缩相关生命周期钩子（compaction entry `details.compactor` 为 `"pi-vcc"`、`details.version` 为 `2`）。
- **摘要结构**：生成 `[Early Session Requests]`（前 3 条用户消息）、`[Files And Changes]`（Modified/Created/Read 各最新 10 个）、`[Commits]`（最多 8 行去重）、`[Tracked Commands]` 与 `[User Messages]`（最新 15 条）；`injected` 消息仅进 brief。
- **召回护栏**：单条召回结果硬顶 50 KB（超限按 UTF-16 安全截断并附首尾提示）、搜索摘要单行裁 300 字符窗口、搜索与 BM25 语料跳过 1000+ 字符的混合 Base64 串（不影响存储与 `expand` 原文）。
- **本地补丁原生实现**：
  - 支持从 `ctx_run` 等任意工具调用中提取 shell 命令文本纳入跨压缩命令追踪；
  - 压缩摘要保留 `vcc_recall` 召回引导尾注并在再次合并前自动去重，避免尾注叠加。
- **PiG 平台适配**：支持 `context_edit` 语义以便 worker 会话压缩；按 `PIG_EXT_HOST_INFO.agentDir` → `$PI_CODING_AGENT_DIR` → `$PIG_CODING_AGENT_DIR` → `$PIG_HOME/agent` → `~/.pig/agent` 解析配置目录（不回退 `~/.pi`，支持 `$PI_VCC_CONFIG_PATH` 覆盖）；异常会话树容错；每扩展实例状态隔离。

### Known Risks

- 本模块仅供 PiG 作为 factory 扩展加载，不能作为独立 Go 库通过 `go get` 引入。
- 上游尚未合并的 PR（`#41`、`#61`、`#62`、`#65`、`#66`）未包含在本版本中。
- 中文/CJK 短句词数统计采用近似算法，极少数两字短句与 `Intl.Segmenter` 存在分歧；工具参数名按字典序输出。

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/pi-vcc/v0.1.0...HEAD
[0.1.0]: https://github.com/dorokuma/pi-extensions/releases/tag/go/pi-vcc/v0.1.0
