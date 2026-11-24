# Changelog — go/exclude-tools-guard

## [Unreleased]

## [0.3.0] - 2026-11-24

### Added

- **codemode 互锁守卫（`EnforceToolExclusions`）**：过滤后的激活工具集仍含
  `codemode` 时，每次评估调用一次 `GetAllTools` 探测嵌套调用守卫，通过
  三项校验才保留 `codemode`，任一不满足即剥离：
  - 注册了名为 `ctx_search_digest` 的工具；
  - 其 `SourceInfo.Path`（去空白、大小写不敏感）不以 `.ts` 结尾——TS 移植版
    无嵌套调用守卫，不算凭据；
  - 其 `Description` 携带能力标记 `[codemode-guard:v1]`——无标记的旧版
    Go 构建或同名无关工具无互锁契约。
- **探测失败 fail-closed**：`GetAllTools` 报错或守卫缺失时，经
  `reportCodemodeGuardMissing` 打点并把 `codemode` 从过滤集剥离；剥离路径
  仍照常执行 `SetActiveTools`——守卫探测失败绝不跳过基础排除集。

## [0.2.0] - 2026-11-22

### Added

- Claude Code / `claude-pty` runtime recognition in `IsAgentRuntimeCmdline`
  (`1ec41d7`): `reAgentRuntime` now also matches `(?:^|/)claude/versions/` and
  the argv0 basenames `claude`, `claude-code` and `claude-pty` count as agent
  runtimes. In the subprocess form S3 can anchor a Claude host as `hostIndex`,
  and a Claude runtime sitting between host and terminal shell is caught by
  S4; in the fused form S3 recognizes a Claude-family self as a valid agent
  runtime host instead of rejecting it as an unknown runtime.

### Breaking Changes

- **Removed the classic herdr identity branch** (`herdr.sock` /
  `HERDR_PANE_ID`); the hc (`HC_TERMINAL_ID` + `terminal.get`) shape is now the
  only S1/S2 path.
  - Removed: `HerdrSocketFile` constant, `QueryPaneShellPid`, the
    `IdentityEnv.SocketPath` / `IdentityEnv.QuerySocket` injection fields and
    the `HERDR_PANE_ID` lookup branch in `ResolveAgentIdentityWithEnv`.
  - New behavior: S1 requires `HC_TERMINAL_ID` to match `^t[0-9a-z]+$` and the
    `terminal.get` record to be live, top-level (parent is JSON null) and
    carry a positive pid; every failure resolves fail-closed to subagent.
  - Migration: sessions relying on classic `HERDR_*` pane identity now
    resolve as subagent; run inside a herdr-core (`hc`) terminal or accept
    the subagent tool set.

## [0.1.0] - 2026-11-10

首次正式发布。Go port of the exclude-tools-guard Pi extension: strips
dispatch-layer tools from the visible tool set per role.

### Added

- herdr-core (`hc`) positive-proof branch in the shared S1–S4 identity core
  (same shape as `go/jev-gate` v0.2.0): when `HERDR_PANE_ID` is empty and
  `HC_TERMINAL_ID` matches `^t[0-9a-z]+$`, S2 queries the hard-coded
  `/run/user/0/herdr-core/default.sock` with JSON-RPC 2.0 `terminal.get`; the
  reply must echo `id`/`address`, be `"live"` when `state` is present, carry an
  explicit `parent` key that is JSON `null`, and return `pid > 0` as
  `shell_pid` for the unchanged S3/S4 checks.

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/exclude-tools-guard/v0.3.0...HEAD
[0.3.0]: https://github.com/dorokuma/pi-extensions/compare/go/exclude-tools-guard/v0.2.0...go/exclude-tools-guard/v0.3.0
[0.2.0]: https://github.com/dorokuma/pi-extensions/compare/go/exclude-tools-guard/v0.1.0...go/exclude-tools-guard/v0.2.0
