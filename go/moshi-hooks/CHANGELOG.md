# Changelog — go/moshi-hooks

## [Unreleased]

### Changed

- Retired/archived: removed from the default deployed extension set and excluded from the default workspace build/test loop (the source stays in tree for reference).

### Removed

- Classic `HERDR_*` environment probing (`HERDR_ENV`, `HERDR_SESSION`, `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`, `HERDR_TAB_ID`); only herdr-core (`hc`) variables `HC_TERMINAL_ID` / `HC_TERMINAL_SOCKET` produce a `"herdr"` terminal context.

> **Note**: this module is retired; the entry above is recorded for completeness and no new version will be published.

## [0.1.0] - 2026-11-10

Initial release. Go port of the `moshi-hooks.ts` Pi extension: normalizes Pi session lifecycle events and writes them to the moshi-hook daemon's Unix socket (one JSON line per connection; daemon absence is swallowed silently).

### Added

- herdr-core (`hc`) terminal context resolution: when classic `HERDR_*` env vars are absent and `HC_TERMINAL_ID` is set, fills the terminal ID, derives the session name from `HC_TERMINAL_SOCKET`, and reports `"herdr"` as the terminal kind.
- Included `HC_SESSION`, `HC_TERMINAL_ID`, `HC_TERMINAL_SOCKET`, and rex driver keys (`REX_BLOCK`, `REX_SERVER`, `REX_SESSION`) in forwarded mux environment keys.
