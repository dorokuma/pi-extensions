# Changelog

本文件记录 `go/pigsdk`（`github.com/MichaelKinsy/PiG/extensions/sdk` 的本仓镜像）的版本变更，遵循 [Keep a Changelog](https://keepachangelog.com/) 风格与本仓 `VERSIONING.md`。

## [0.1.0] - 2026-11-09

### Added

- 首次发布：与 staged PiG SDK 同步的完整 SDK 源码，以及配套回归测试模块 `go/pigsdk-regress`。

### Fixed

- 补充扩展上下文的 nil-result 防御性守卫。
- 修复 MCP 断开连接时的资源清理逻辑。

### Known Risks

- 上述两项修复为本仓库相对上游 staged SDK 的本地补丁，后续重新同步上游 SDK 时需复核其适用性。
