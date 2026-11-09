# Changelog — go/moshi-hooks

## [Unreleased]

## [0.1.0] - 2026-11-10

首次正式发布。Go port of the `moshi-hooks.ts` Pi extension: normalizes Pi
session lifecycle events and writes them to the moshi-hook daemon's Unix
socket (one JSON line per connection); daemon absence is swallowed silently.

### Added

- herdr-core (`hc`) terminal context resolution (`terminal.go`): when classic
  `HERDR_*` env is absent but `HC_TERMINAL_ID` is set, the terminal id fills
  `herdrPane`, the session name is derived from `HC_TERMINAL_SOCKET`
  (`<dir>/<session>.sock`), and `terminalKind` reports `"herdr"`.
- `MuxEnvKeys` now includes `HC_SESSION` / `HC_TERMINAL_ID` /
  `HC_TERMINAL_SOCKET` and the rex driver keys (`REX_BLOCK` / `REX_SERVER` /
  `REX_SESSION`), in sync with the deployed TS mux registry.
