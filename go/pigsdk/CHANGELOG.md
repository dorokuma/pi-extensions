# Changelog

本文件记录 `go/pigsdk`（`github.com/MichaelKinsy/PiG/extensions/sdk` 的本仓镜像）的
发布变更，遵循 [Keep a Changelog](https://keepachangelog.com/) 风格与本仓 `VERSIONING.md`。
本目录是 PiG staged SDK 的逐字节镜像（`make sdk-check` 校验），仅 `PIGSDK_LOCAL_PATCHES`
声明的防御性守卫为本仓有意偏差；本文件属发布元数据，已豁免镜像比对。

## [0.1.0] - 2026-11-09

### Added

- 首次发布：与 staged PiG SDK `51955af53b263ab7` 逐字节同步的完整 SDK 源码。
- `go/pigsdk-regress`：本仓新增的回归测试模块，覆盖 SDK 行为（随发布一并入库）。

### Fixed

- `context.go`：nil-result 防御性守卫（H10；本地声明补丁，`make sdk-check` 显示 `PATCH OK`）。
- `mcp_extension_options.go`：MCP disconnect 清理（本地声明补丁，`make sdk-check` 显示 `PATCH OK`）。

### Known Risks

- 上述两条补丁是本仓相对 staged SDK 的有意偏差，记录在 `go/Makefile` 的
  `PIGSDK_LOCAL_PATCHES`；上游 SDK 重新同步时须复核补丁是否仍然适用。
