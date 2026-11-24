# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/pi-my-compact-ui` 的版本变更，格式遵循
[Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录
[VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.0] - 2026-11-24

首次正式发布。`pi-my-compact-ui` 的 Go 移植：紧凑对话 UI 扩展，把一次
assistant 回合内连续流出的工具调用与思考过程折叠为可折叠工具组（树形
rail + 自旋帧 + 等待提示 + header 计数），历史思考重挂 assistant 文本锚点，
`cache miss` 注入折叠为通知行，并提供 `/compact-inspect`（Ctrl+I）与
`/compact-ui-config`。

### Added

- 完整移植 TS 权威源 `pi-my-compact-ui` 的工具组生命周期：顺序即语义的 `entries`、最近开放组吸收新条目、
  真实文本到达即 seal、历史思考锚点重挂与幻影 padding 剥离、cache miss 折叠、
  有界动画 tick 与 `session_shutdown` 停止、配置读取失败静默回退默认值。

### Fixed

- **嵌套调用抢占外层 `codemode` 卡片（工具树隐身）**：
  `handleToolExecutionStart` / `handleToolExecutionEnd` 对
  `parentToolCallId != ""` 的嵌套调用事件（codemode 沙盒内调用，id 形如
  `<parent>/<n>`）早退——宿主不为嵌套调用挂卡片，此前记录它会生成无卡片
  条目、抢走组内展示权并隐藏整个工具树。
