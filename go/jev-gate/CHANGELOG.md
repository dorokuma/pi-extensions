# Changelog — go/jev-gate

## [Unreleased]

## [0.4.0] - 2026-11-25

### Breaking Changes

- **Removed classic herdr identity support** (`herdr.sock` / `HERDR_PANE_ID`): herdr-core (`hc` via `HC_TERMINAL_ID` + `terminal.get` on `/run/user/0/herdr-core/default.sock`) is now the sole terminal identity verification path for both the extension and `cmd/jev-mcp`, aligned with `go/exclude-tools-guard` v0.2.0+ and the TypeScript `jev-gate` / `exclude-tools-guard` extensions.
  - `HC_TERMINAL_ID` must match `^t[0-9a-z]+$` and the `terminal.get` record must be live (when `state` is present), top-level (`parent` is JSON `null`), and carry a positive `pid`; any failure resolves fail-closed to subagent.
  - **Migration**: Sessions relying on classic `HERDR_*` pane identity now resolve as subagent; run inside a herdr-core (`hc`) terminal or accept the subagent tool set.

## [0.3.0] - 2026-11-24

### Added

- Recognize `claude`, `claude-code`, `claude-pty`, `dispatch-agent-go`, and `/claude/versions/` path segments in process ancestry checks so a Claude-family runtime appearing between the process and the pane shell fails closed in both extension and `cmd/jev-mcp` modes (also synced to `exclude-tools-guard` Go and TS).

### Fixed

- `cmd/jev-mcp`: Tolerate `tools/call` `params.arguments` delivered as a JSON-encoded string (up to 3 nested string layers) from tool bridges instead of silently falling back to `{}`; non-object or invalid JSON values now return an explicit tool error.

## [0.2.0] - 2026-11-10

### Added

- Support for herdr-core (`hc`) terminal identity verification (shared by the extension, `cmd/jev-mcp`, and `go/exclude-tools-guard`): when `HERDR_PANE_ID` is empty and `HC_TERMINAL_ID` matches `^t[0-9a-z]+$`, queries `/run/user/0/herdr-core/default.sock` with JSON-RPC 2.0 `terminal.get` and verifies matching `id`/`address`, `"live"` state (when present), explicit `parent: null` (top-level terminal), and `pid > 0` before running process ancestry checks (`HC_TERMINAL_SOCKET` is ignored).

## [0.1.1] - 2026-11-03

### Added

- Added `--version` flag and injectable build version (`-ldflags "-X main.version=X.Y.Z"`, defaulting to `dev`); MCP `initialize` `serverInfo.version` now reports the build version instead of hardcoded `1.0.0`.

## [0.1.0] - 2026-11-03

Initial release. Go port of the `jev-gate` Pi extension (judgement-evidence verifier gate) plus the `cmd/jev-mcp` MCP server binary.

### Fixed

- Set `User-Agent: jev/1.0` on requests to the magpie gateway so usage stats attribute traffic to `jev` instead of Go's default `Go-http-client/1.1`.
