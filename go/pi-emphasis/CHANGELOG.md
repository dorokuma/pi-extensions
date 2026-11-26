# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/pi-emphasis` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.1.1] - 2026-11-24

### Fixed

- **标题行内 `[[Word]]` 胶囊渲染**：标题行中非行内代码/可解析链接的 `[[Word]]` 现按正文规则渲染为胶囊，并在胶囊后仍有字符时自动恢复标题粗体与前景色（主题切换时同步重建缓存）；行内代码与链接中的 `[[…]]` 保持原样。

## [0.1.0] - 2026-11-19

首次正式发布。Go port of [johnlindquist/pi-emphasis](https://github.com/johnlindquist/pi-emphasis) v0.2.0：助手回复的纯展示强调渲染——`[[Word]]` 彩色图标胶囊、标题/行首标签图标、粗体/箭头/对勾/树线主题着色，以及 TUI 模式下的 `reply_style` 系统提示词注入。

### Added

- 完整移植上游强调渲染能力：7 类胶囊词表、同义词归一、`ContextWords`、PUA 自有图标优先，以及 TUI 模式下的 `reply_style` 引导注入（可通过 `PI_EMPHASIS_GUIDANCE=0` 关闭）。

### Changed

- **针对 `pig 0.7.0+1.5.0` 的兼容与安全增强**：
  - 词表按上游顺序确定性输出，`ReplyGuidance()` 输出稳定；
  - 对齐 CommonMark 与宿主 Markdown 的围栏与行内链接/图片/LaTeX/HTML 标签保护逻辑，避免链接内部被注入 ANSI 转义破坏渲染，并增加单行 8192 字节护栏；
  - 统一从左到右扫描粗体、`[[pill]]`、状态符号与树线，保持粗体字重与前景色并在无面板背景主题下自动回退反显胶囊；
  - 修复大写/Unicode 折叠边界导致的截断问题及无图标动作回调的冗余空格问题；
  - 引入线程安全与指纹驱动的主题缓存失效机制，支持 `PI_EMPHASIS_COLOR`（无效 token 安全回退）与 `PI_EMPHASIS_ROUND_CAPS` 切换以及 TUI/非 TUI 会话切换清理。
