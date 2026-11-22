# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/cache-stats` 的版本变更，格式遵循
[Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录
[VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.2.0] - 2026-11-22

### Breaking Changes

- **彻底移除 herdsman/shepherd 页脚状态段及相关符号**（相对 `0.1.0` 的破坏性
  变更，`0.y` 破坏性变更按 MINOR 升位规则披露）。
  - 旧行为：渲染前读取 `ctx.FooterData().ExtensionStatuses()`，命中
    `"herdsman"` 或 `"shepherd"` 键时经 `sheepMeat`（剥前导 `◆/◇/●/▲/■/▸`
    图标与 Shepherd/Herdsman 标签）与 `formatHerdsmanStatus`（计数形态渲染为
    `"<n> Up"`，运行无结果渲染 `"On"`，未加载返回 nil）在页脚最前渲染
    `● <status>` 段。
  - 新行为：页脚固定三段——双命中率菱形 `◆ <agg> ◇ <latest>`、上下文占用
    `▲ <pct>/<window>`、模型 `■ <name> · <thinking>`；`footerRender` 不再接收
    statuses 参数（签名 `footerRender(state, theme, statuses, width)` →
    `footerRender(state, theme, width)`），`sheepMeat` /
    `formatHerdsmanStatus` / `sheepPrefix` / `herdsmanCount` / `herdsmanBare` /
    `footerIconSheep`（`●`）/`safeExtensionStatuses` 一并删除，宿主输入
    `footerUIInputs` 收窄为只读主题的 `footerUITheme`。
  - 迁移方式：无 API/命令面变化，`/cache-stats` 子命令、命中率统计、前缀
    诊断行为不变；若外部脚本按 `●` 段解析页脚，改按 `◆◇` / `▲` / `■` 三段
    定位，段序起点由 `●` 变为 `◆`。

### Removed

- 测试侧 `TestFormatHerdsmanStatus` / `TestFormatHerdsmanStatusRaw`，
  `TestFooterSegmentOrderAndHerdsman` 更名 `TestFooterSegmentOrderAndModel`
  并删除 sheep 断言；`TestStripProvider` 承接 `stripProvider` 断言。

## [0.1.0] - 2026-11-08

首次正式发布。Go port of the cache-stats Pi extension（提示缓存观测扩展）：
双命中率统计、可选 TUI 页脚、按模型的 400 告警、`/cache-stats` 命令与只读
前缀诊断。

### Breaking Changes

- **彻底移除 TPS（tokens/s）显示及专用计算链路**（相对移植基线的破坏性变更，
  `0.y` 破坏性变更按 MINOR 升位规则披露）。
  - 旧行为：`before_provider_request` 记录请求起点，`message_update` /
    `message_end` 累积流式解码时间戳，`turn_end` 经 `computeTurnTps` 一次性
    消费全部时间戳并估算输出 tokens/s，页脚渲染 `▸ <n> t/s` 段（不可估时
    `▸ n/a`）。
  - 新行为：整条链路删除——页脚仅剩 herdsman 状态、双命中率菱形、上下文
    占用与模型段；`message_update` / `message_end` 事件订阅、计时状态字段
    （`providerRequestStartedAt` / `streamFirstThinkingAt` /
    `streamFirstTextAt` / `streamLastDeltaAt` / `streamDoneAt` /
    `lastTurnTps`）与 `clearTurnTiming` / `applyProviderRequestTiming` /
    `applyMessageUpdate` / `applyMessageEnd` / `applyTurnEnd` /
    `computeTurnTps` / `formatTps` / `turnTimingStamps` / `toStamp` /
    `millisToStamp` / `nowMillis` 一并移除；`tps.go` 与 `tps_test.go` 删除，
    保留的共用页脚格式化助手（`formatWindowSize` / `sheepMeat` /
    `formatHerdsmanStatus` / `stripProvider` / `contextSegment` /
    `truncateFooter` / `jsNumberString` / `uiTheme`）迁至 `format.go`。
  - 迁移方式：无 API/命令面变化，`/cache-stats` 子命令、命中率统计、usage
    汇总与前缀诊断行为不变；若外部脚本解析页脚 ▸ 段，改按 herdsman / ◆◇ /
    ▲ / ■ 段定位。

### Removed

- 事件订阅 `message_update`、`message_end`（订阅数 12 → 10），两者此前只服务
  TPS 时间戳采集。
- 测试侧 `tps_test.go` 全部 TPS 用例、`TestFormatTps`，以及 footer /
  concurrency / harness 测试中的 TPS 构造与断言；`jsNumberString` 的
  Number::toString 对拍测试迁入 `format_test.go`。

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/cache-stats/v0.2.0...HEAD
[0.2.0]: https://github.com/dorokuma/pi-extensions/compare/go/cache-stats/v0.1.0...go/cache-stats/v0.2.0
