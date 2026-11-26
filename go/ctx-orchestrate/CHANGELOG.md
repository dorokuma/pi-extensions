# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/ctx-orchestrate` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.0] - 2026-11-24

首次正式发布。`extensions/ctx-orchestrate` 的 Go 移植版本：提供 `ctx_search_digest` 组合工具（在 `codemode` 命名空间下多路并发检索、跨模式去重并按文件聚合）、`codemode` 回传治理钩子，以及 Go 版独有的 `codemode` 沙盒嵌套调用守卫。

### Added

- **嵌套调用守卫（Go 版独有）**：对嵌套工具调用执行正面只读白名单与分层调用预算校验，顶层调用直接放行。
  - 白名单覆盖 6 个工具：`ctx_fs`、`ctx_git`、`ctx_kb`、`codegraph` 的只读动作子集，以及无动作路由的 `ctx_stats` 和 `ctx_search_digest`；白名单校验先于预算扣除，被拒调用不消耗额度。
  - 大小写重复 `action` 键、`ctx_stats` 非对象输入或携带非空 `action` 字段，以及嵌套调用内部异常均 fail-closed 阻断。
- **`models.*` 窄护栏**：当顶层 `codemode` 调用代码匹配 `models.classify` / `generateImages` / `getModelsOfType` / `getAvailableOfType` / `getModelOfType` 时阻断执行；可通过 `CTX_ORCH_ALLOW_SANDBOX_MODELS=1` 放开。
- **实例独占分层调用预算**：一级嵌套调用默认上限 16（`CTX_ORCH_MAX_NESTED_CALLS`，范围 `[1, 64]`），同一根调用下的嵌套调用总数默认上限 64（`CTX_ORCH_MAX_TOTAL_NESTED_CALLS`，范围 `[16, 256]`，含 `ctx_search_digest` 扇出）；环境变量每次扣费实时读取，活跃根上限 128 槽位并按 FIFO 逐出，被逐出根的后续调用 fail-closed 拒绝。
- **顶层 `ctx_search_digest` 预算跟踪**：顶层 `ctx_search_digest` 调用的 `ctx_fs` 扇出仅计入总嵌套额度，不占用一级额度；工具完成、中止或回合结束时自动回收预算槽位（不受 `CTX_ORCH_GOVERN` 开关影响）。
- **`[codemode-guard:v1]` 互锁能力标记**：`ctx_search_digest` 描述尾部携带该标记，供 `go/exclude-tools-guard` v0.3.0+ 校验守卫在位后才保留 `codemode`。
- **`codemode` 回传治理钩子**：对顶层 `codemode` 返回文本做长度截断（默认 3000 UTF-16 code units，由 `CTX_ORCH_TEXT_CAP` 配置、下限 200；`CTX_ORCH_GOVERN=0` 可关闭），截断后追加引导提示并透传原始结构字段与 `isError` / `usage`；嵌套调用结果不截断。
