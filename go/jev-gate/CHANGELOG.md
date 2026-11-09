# Changelog — go/jev-gate

## [Unreleased]

### Added

- add a herdr-core (`hc`) positive-proof branch to the S1–S4 identity core
  (`identity.go`, shared by the extension and `cmd/jev-mcp`): when
  `HERDR_PANE_ID` is empty and `HC_TERMINAL_ID` matches `^t[0-9a-z]+$`, S2
  queries the hard-coded `/run/user/0/herdr-core/default.sock` with JSON-RPC
  2.0 `terminal.get`; the reply must echo `id` and `address`, must be
  `"live"` when `state` is present, must carry an explicit `parent` key that
  is JSON `null` (top-level terminal — the `hc` analogue of herdr's `:p1`
  main pane; `hc run --bg` subagent terminals always carry a parent
  address), and must return `pid > 0` as `shell_pid` for the unchanged S3
  ancestry and S4 checks. `HC_TERMINAL_SOCKET` is deliberately never read.
- the same `hc` shape lands in `go/exclude-tools-guard` (which shares this
  identity core).

## [0.1.1] - 2026-11-03

### Added

- add `--version` flag and injectable `main.version` variable for SemVer builds
  (`go build -ldflags "-X main.version=X.Y.Z"`; source default is `dev`).
  The MCP `initialize` `serverInfo.version` now reports the same value instead
  of the hardcoded `1.0.0`.

## [0.1.0] - 2026-11-03

首次正式发布。Go port of the jev-gate Pi extension (judgement-evidence verifier gate)
plus the `cmd/jev-mcp` MCP server binary.

### Fixed

- fix: set User-Agent `jev/1.0` on requests to the magpie gateway, so magpie usage
  stats display the caller as `jev` instead of Go's default `Go-http-client/1.1`.
  The UA is applied in both `Call` and `classifyWith` paths.
