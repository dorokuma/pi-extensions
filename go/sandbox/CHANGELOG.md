# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/sandbox` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

拟发布为 **v0.2.0**（MINOR：向后兼容的新预设与 CLI 能力）。

### Added

- 新预设 `--preset reviewer-agent --workdir DIR`：在 ReadOnlyReviewer 基础上允许**顶层**启动 `agy` / `pig` / `pi`（经 `Policy.EntryAgents` 进入 Exec 白名单），并补充 `mkdir` / `cp` / `touch`。
- 额外可写目录：`/tmp/herdr-role-sessions`（pig `--session-dir`）；agy 为 `~/.gemini/antigravity-cli` 下数据子目录与根级状态文件（**不含** `bin/`，真实 ELF `bin/agy` 仅 Exec）。
- `Policy.EntryAgents`、`ResolveEntryAgentPaths`、`IsEntryAgentPath`：Validate / `sbx` 入口校验 / seccomp `PROT_EXEC` 守卫识别合法顶层代理，避免把正在运行的 agy/pig 误判为一律拒绝 mmap。

### Known Risks

- **嵌套再起 agy/pig 无法在 OS 层做到「只放行第一次 exec」**：Landlock 按路径授予 `EXECUTE`，顶层放行后子进程通常也能再 `execve` 同一路径；当前无 execve 计数或一次性 ticket。`dispatch-agent` / `grok` 仍不在白名单。兜底依赖只读角色无 `bash` / `run_command` 以及策略层禁止再派发。
- 原 `--preset reviewer` / `ReadOnlyReviewer` 行为不变；`reviewer-agent` 默认仍 `DenyNet: false`（Magpie 需要网络）。

## [0.1.0] - 2026-11-01

`go/sandbox` 的首个正式发布版本。覆盖只读审查员预设下的 W^X 加固、go broker、seccomp `PROT_EXEC` 守卫，以及 `sbx` CLI 兼容与严格 seccomp 失败语义。

### Added

- Linux Landlock 文件系统 / 网络沙盒封装（零 cgo，依赖 `golang.org/x/sys/unix`）。
- 命令行入口 `cmd/sbx`：支持 `--ro` / `--rw` / `--exec` / `--env` / `--deny-net` / `--best-effort` / `--check`。
- 预设 `--preset reviewer --workdir DIR`，以及兼容别名 `--reviewer WORKDIR`。
- ReadOnlyReviewer：可写与可执行临时目录分离（W^X）；经 helper go broker 代理 `go` 工具链；seccomp 拒绝针对 ld-linux / 禁止项 / 可写路径的 `PROT_EXEC` mmap。
- 导出 `ErrSeccomp`；非 BestEffort 下 seccomp 安装失败时 `Apply` 失败且不执行目标命令。
- `sbx --version`：构建时通过 `-ldflags "-X main.version=..."` 注入版本号（未注入时为 `dev`）。

### Fixed

- 恢复并重写 `cmd/sbx` 的 `parseArgs`（`FlagSet`）与 CLI 测试，与 `--reviewer` 别名行为对齐。
- 严格 seccomp：安装失败不再静默降级（仅 `BestEffort` 告警并降级）。
- README 披露只读路径上已有 `.so` 经 `LD_PRELOAD` 注入的残余风险。

### Security

- 阻断审查员预设下「拷贝到可写目录再执行」与「ld-linux 直载」绕过。
- 可写目录内构建出的二进制不可执行（W^X）；`PI_SANDBOX_GO_EXEC_DIR` 对沙盒内进程不可写。

### Known Risks

- 宿主只读路径上**已存在**的 `.so` 仍可能经 `LD_PRELOAD` 注入（构造器会执行）；需配合更严的环境或挂载策略。
- `BestEffort` 下 seccomp 失败会去掉 `PROT_EXEC` 守卫，不得对其抱安全预期。
- helper / go broker 提权面、Unix socket、`/proc/<pid>/fd` TOCTOU、脚本解释器白名单、默认 `DenyNet: false` 等既有边界仍然存在；详见模块 `README.md`。

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.1.0...HEAD
[0.1.0]: https://github.com/dorokuma/pi-extensions/releases/tag/go/sandbox/v0.1.0
