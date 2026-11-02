# Changelog — go/jev-gate

## [Unreleased]

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
