# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/ctx-orchestrate` 的版本变更，格式遵循
[Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录
[VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.0] - 2026-11-24

首次正式发布。`extensions/ctx-orchestrate` 的 Go 移植：`ctx_search_digest` 组合工具
（codemode 命名空间下多路 `ctx_fs` rg 并发检索、跨模式去重、按文件聚合）+
codemode 回传治理钩子，外加 Go 版独有的 codemode 沙盒嵌套调用守卫
（TS 版无对应物）。

### Added

- **`EventToolCall` 嵌套守卫（`guard.go`，Go 版独有）**：对
  `parentToolCallId != ""` 的嵌套调用执行正面只读白名单 + 分层调用预算，
  顶层调用一律放行：
  - 白名单覆盖 6 个工具：`ctx_fs` / `ctx_git` / `ctx_kb` / `codegraph` 的
    只读 `action` 子集、无路由动作的 `ctx_stats` 与 `ctx_search_digest`；
    白名单先于预算评估，被拒调用不消耗额度；
  - 大小写重复 `action` 键（`"Action"` / `"ACTION"` 等经 Go `encoding/json`
    大小写折叠可能绕过已批准 action）一律 fail-closed 阻断；`ctx_stats`
    的 input 非对象或携带任何非空 `action` 字段同样 fail-closed；
  - 嵌套调用 panic 亦 fail-closed（与 `tool_result` 钩子的 fail-open 相反）。
- **`models.*` 窄护栏**：顶层 codemode 调用的 `input.code` 匹配五个
  `models.<method>` 名称（classify / generateImages / getModelsOfType /
  getAvailableOfType / getModelOfType）即阻断（允许点号两侧空白，不要求
  紧跟 `(`，亦覆盖直接方法引用）——这些 QuickJS 全局绕过
  `tool_call` 事件无法逐次拦截，该护栏是窄反滥用栏而非对抗边界；
  `CTX_ORCH_ALLOW_SANDBOX_MODELS=1` 可放开。
- **实例独占分层调用预算（`CallBudgetTracker`）**：一级嵌套调用上限
  默认 16（`CTX_ORCH_MAX_NESTED_CALLS`，clamp [1, 64]），同一根嵌套调用
  总数上限默认 64（`CTX_ORCH_MAX_TOTAL_NESTED_CALLS`，clamp [16, 256]，
  含 `ctx_search_digest` 二级 `ctx_fs` 扇出）；env 在每次扣费时实时读取，
  packed-cell `/reload` 或第二代会话不共享计数；活根限 128 槽位、满后严格
  FIFO 逐出，被逐根的后续调用按未知父调用 fail-closed；`<rootID>/<数字段>`
  二级跳只计总数不计一级额度。
- **顶层 `ctx_search_digest` `totalOnly` 根**：顶层 `ctx_search_digest` 调用
  登记为 totalOnly 预算根——其自身 `ctx_fs` 扇出以一级嵌套调用到达该
  `toolCallId` 下，与 codemode → `ctx_search_digest` 二级跳同配额形态，
  只计总数不动一级额度。
- **槽位回收双通道**：`EventToolResult` 与 `EventToolExecutionEnd` 均对
  已结束的顶层 `codemode` / `ctx_search_digest` 释放预算槽位（幂等；
  `Release` 不受 `CTX_ORCH_GOVERN` 门控，防止中止/中断调用泄漏槽位）；
  `EventAgentEnd` 清空回合遗留根。
- **`[codemode-guard:v1]` 互锁能力标记**：`ctx_search_digest` 的
  `Description` 尾部携带该标记，供 `go/exclude-tools-guard` v0.3.0 判定
  嵌套守卫在位后才保留 `codemode`。
- **codemode 回传治理钩子**：`tool_result` 事件对顶层 codemode 回传做
  物理截断——默认 3000 UTF-16 code units 硬顶（`CTX_ORCH_TEXT_CAP`，
  下限 200，非法值回退默认；`CTX_ORCH_GOVERN=0` 关闭），截断后追加新引导
  文案标记并显式透传 `content` / `details` / `structuredContent` /
  `isError` / `usage`；嵌套调用结果不治理。
