# Changelog — go/exclude-tools-guard

## [Unreleased]

## [0.2.0] - 2026-11-22

### Added

- Claude Code / `claude-pty` runtime recognition in `IsAgentRuntimeCmdline`
  (`1ec41d7`): `reAgentRuntime` now also matches `(?:^|/)claude/versions/` and
  the argv0 basenames `claude`, `claude-code` and `claude-pty` count as agent
  runtimes, so a Claude-family host between host and terminal shell is caught
  by S4 and fused-form S3 instead of being silently trusted.

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
