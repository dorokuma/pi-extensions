# Changelog — go/exclude-tools-guard

## [Unreleased]

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
