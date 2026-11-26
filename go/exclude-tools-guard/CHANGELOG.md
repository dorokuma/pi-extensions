# Changelog — go/exclude-tools-guard

## [Unreleased]

## [0.3.0] - 2026-11-24

### Added

- **codemode 互锁守卫**：当过滤后的激活工具集仍包含 `codemode` 时，每次评估会检查嵌套调用守卫是否就位，需同时满足以下三项条件才保留 `codemode`，否则将其剥离：
  - 已注册名为 `ctx_search_digest` 的工具；
  - 该工具并非 `.ts` 实现（TS 移植版无嵌套调用守卫）；
  - 其描述携带能力标记 `[codemode-guard:v1]`。
- **探测失败 fail-closed**：工具列表查询报错或守卫缺失时，记录告警并将 `codemode` 从激活集剥离，同时继续照常应用基础排除集。

## [0.2.0] - 2026-11-22

### Added

- Recognize Claude Code (`claude`, `claude-code`, `claude-pty`, and `/claude/versions/` paths) as agent runtimes in both subprocess and fused-process identity checks.
- Isolate the per-call argument cache when no explicit cache is provided so isolated checks never inherit markers from the process-wide cache.

### Breaking Changes

- **Removed classic herdr identity support** (`herdr.sock` / `HERDR_PANE_ID`): herdr-core (`hc` via `HC_TERMINAL_ID` + `terminal.get`) is now the sole terminal identity verification path.
  - `HC_TERMINAL_ID` must match `^t[0-9a-z]+$` and the `terminal.get` record must be live, top-level (`parent` is JSON `null`), and carry a positive `pid`; any failure resolves fail-closed to subagent.
  - **Migration**: Sessions relying on classic `HERDR_*` pane identity now resolve as subagent; run inside a herdr-core (`hc`) terminal or accept the subagent tool set.

## [0.1.0] - 2026-11-10

Initial release. Go port of the `exclude-tools-guard` Pi extension: strips dispatch-layer tools from the visible tool set per role.

### Added

- Support for herdr-core (`hc`) terminal identity verification: when `HERDR_PANE_ID` is empty and `HC_TERMINAL_ID` matches `^t[0-9a-z]+$`, queries `/run/user/0/herdr-core/default.sock` with JSON-RPC 2.0 `terminal.get` and verifies a live, top-level (`parent` is JSON `null`) terminal record with `pid > 0` before running process ancestry checks.

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/exclude-tools-guard/v0.3.0...HEAD
[0.3.0]: https://github.com/dorokuma/pi-extensions/compare/go/exclude-tools-guard/v0.2.0...go/exclude-tools-guard/v0.3.0
[0.2.0]: https://github.com/dorokuma/pi-extensions/compare/go/exclude-tools-guard/v0.1.0...go/exclude-tools-guard/v0.2.0
