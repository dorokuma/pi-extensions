# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/cache-stats` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.2.0] - 2026-11-22

### Breaking Changes

- **移除 `herdsman`/`shepherd` 页脚状态段**：页脚精简为固定三段——双命中率菱形（`◆ <agg> ◇ <latest>`）、上下文占用（`▲ <pct>/<window>`）与模型信息（`■ <name> · <thinking>`），不再在最前渲染 `● <status>` 状态段。
  - **迁移方式**：`/cache-stats` 子命令、命中率统计与前缀诊断行为不变；若外部脚本按 `●` 段解析页脚，请改按 `◆◇` / `▲` / `■` 三段定位（段序起点由 `●` 变为 `◆`）。

## [0.1.0] - 2026-11-08

首次正式发布。Go port of the `cache-stats` Pi extension（提示缓存观测扩展）：双命中率统计、可选 TUI 页脚、按模型的 400 告警、`/cache-stats` 命令与只读前缀诊断。

### Breaking Changes

- **移除 TPS（tokens/s）显示及计算链路**（相对移植基线）：页脚不再渲染 `▸ <n> t/s`（或 `▸ n/a`）段，仅保留 herdsman 状态、双命中率菱形、上下文占用与模型段。
  - **迁移方式**：`/cache-stats` 子命令、命中率统计、usage 汇总与前缀诊断行为不变；若外部脚本按 `▸` 段解析页脚，请改按 herdsman / `◆◇` / `▲` / `■` 段定位。

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/cache-stats/v0.2.0...HEAD
[0.2.0]: https://github.com/dorokuma/pi-extensions/compare/go/cache-stats/v0.1.0...go/cache-stats/v0.2.0
