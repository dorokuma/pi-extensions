# Changelog — go/jev-gate

## [Unreleased]

## [0.4.0] - 2026-11-25

### Breaking Changes

- **Removed the classic herdr identity branch** (`herdr.sock` /
  `HERDR_PANE_ID`); the hc (`HC_TERMINAL_ID` + `terminal.get`) shape is now
  the only S1/S2 positive-proof path — aligned with
  `go/exclude-tools-guard` v0.2.0+, shared by the extension and
  `cmd/jev-mcp` (`identity.go`).
  - Removed: `herdrSocketFile` constant, `queryPaneShellPid`, the
    `IdentityEnv.SocketPath` / `IdentityEnv.QuerySocket` injection fields and
    the `HERDR_PANE_ID` lookup branch in `resolveIdentity`.
  - New behavior: S1 requires `HC_TERMINAL_ID` to match `^t[0-9a-z]+$` and
    the `terminal.get` record to be live (when `state` is present), top-level
    (parent is JSON null) and carry a positive pid; every failure resolves
    fail-closed to subagent (`sync.Once` warning dedup unchanged).
  - The same hc-only migration landed in the TS authority sources:
    the byte-identical `SUBAGENT-IDENTITY` blocks of
    `extensions/jev-gate/src/jev-gate.ts` and
    `extensions/exclude-tools-guard/src/exclude-tools-guard.ts` now query
    `/run/user/0/herdr-core/default.sock` with JSON-RPC 2.0 `terminal.get`
    instead of the classic `pane.process_info` herdr query.
  - Migration: sessions relying on classic `HERDR_*` pane identity now
    resolve as subagent; run inside a herdr-core (`hc`) terminal or accept
    the subagent tool set.

## [0.3.0] - 2026-11-24

### Added

- `identity.go`: extend `forbiddenExecutableBases` with `claude` /
  `claude-code` / `claude-pty` / `dispatch-agent-go`, and
  `forbiddenPathPattern` with the `/claude/versions/` path segment, so a
  claude-family runtime appearing between the process and the pane shell
  fails closed in both extension and `cmd/jev-mcp` (MCP) modes.
- `exclude-tools-guard` (`go` + `extensions/*.ts`): add claude /
  claude-code / claude-pty basenames and the `/claude/versions/` segment to
  `IsAgentRuntimeCmdline` / `isAgentRuntimeCmdline`.

### Fixed

- `cmd/jev-mcp`: tolerate `tools/call` `params.arguments` delivered as a
  JSON-encoded string (possibly nested, e.g. `"{\"a\":1}"` or
  `"\"{\\\"a\\\":1}\""`) instead of the spec-required object — observed
  from lazy tool bridges whose `Arguments` param is declared `type:string`.
  New `unwrapToolArguments` unwraps up to `maxArgStringLayers` (3) string
  layers; the previous silent `{}` fallback let malformed calls run with
  fabricated empty input, and non-object/non-JSON values now return an
  explicit tool error instead.

## [0.2.0] - 2026-11-10

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
