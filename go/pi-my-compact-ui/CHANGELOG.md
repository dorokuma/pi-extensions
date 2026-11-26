# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/pi-my-compact-ui` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.0] - 2026-11-24

首次正式发布。`pi-my-compact-ui` 的 Go 移植版本：紧凑对话 UI 扩展，将单次 assistant 回合内连续流出的工具调用与思考过程折叠为可折叠工具组（树形 rail、加载动画、等待提示与计数标题），支持历史思考重挂到 assistant 文本锚点、`cache miss` 通知折叠，以及 `/compact-inspect`（Ctrl+I）与 `/compact-ui-config` 命令。

### Added

- 完整移植工具组生命周期：按时间顺序组织条目、自动合并与封口、历史思考重挂与冗余空白剥离、`cache miss` 折叠、有界动画刷新与会话关闭停止，以及配置读取失败时静默回退默认值。

### Fixed

- 修复沙盒内嵌套工具调用（如 `codemode` 子调用）误生成无卡片条目并抢占外层工具组展示权、导致整个工具树隐藏的问题。
