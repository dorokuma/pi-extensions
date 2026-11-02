# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/sandbox` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.2.2] - 2026-11-01

PATCH：修复 reviewer-agent 下 pig 扩展加载因锁目录 / 构建缓存 / harness 运行时目录写权限不足而失败的问题。

### Fixed

- `ReviewerAgent` 窄放行 pig 实测必需的写路径（strace `mkdirat`/`openat` EACCES）：
  - `/root/.pig/agent`：`settings.json.lock` / `auth.json.lock` / `models-store.json.lock` 的 proper-lockfile 父目录（mkdir/rmdir；仅放锁路径无法取得 MAKE_DIR，且预建锁目录会导致 already held）
  - `/root/.pig/cache/cells/go`：`.locks/*` 与 ephemeral `.build-*` scratch
  - `/root/.pig/cache/toolchains`：toolchain `*.json.lock`
  - `/run/user/0/pig`：扩展 harness socket 目录（`mkdir h*`；不存在时 `MkdirAll`）
- `/root/.pig/cache/cells/go` 同时进入 `Exec`：packed extension `runner` 需可执行；借助既有 `computeNonExecRWDirs` 机制（RW∩Exec 路径不进 seccomp W^X 拒绝集）。**不**放行整个 `/root` 或 `/root/.pig`。

### Known Risks

- `/root/.pig/cache/cells/go` 同时可写可执行，是为 pig 扩展 runner 保留的局部 W^X 例外；仅限该缓存树，不扩大到 `/root/.pig` 整树。

### Tests

- CLI / 预设测试断言上述 pig 路径在 ReadWrite；`cells/go` 在 Exec；整树 `/root/.pig` 与 `/root` 不在 ReadWrite；ReadOnlyReviewer 不获得 pig 锁放宽。

## [0.3.0] - 2026-11-01

MINOR：向后兼容地补齐 reviewer-agent 预设对 pig 与 agy MCP 生态的三层安全修复（
ctxmode/MCP 数据隔离、pig AgentDir 私有隔离、Cell 缓存 W^X 预热）。

### Added

- `ReviewerAgent` 在 `Policy.Exec` 中按名字逐个点名放行 MCP 服务二进制
  `ctxmode` / `codegraph-go` / `jev-mcp` / `jev-mcp-go` / `agy-recall`，经
  `ResolveMCPServerPaths()` 同时覆盖 PATH、`standardBinDirs`、`agentLookupDirs` 与
  `agyHookLookupDirs()`（homeDir() 与 /root 两种 HOME），并把软链解析目标一并放行。
  依旧不整目录放行 `~/.local/bin`（`herdr`、`dispatch-agent`、`dispatch-agent-go`、
  `grok` 不进白名单）。
- ctxmode 私有数据库：`CTXMODE_DB` 未显式指定时，`Policy.Env` 注入
  `CTXMODE_DB=<privateTmpDir>/ctxmode/context_mode.db`（宿主态预建 0700；
  `go/ctxmode` 的 `roleEnviron` 不经滤地透传环境，沙盒内 ctxmode 直接读取该值）。
  宿主兜底仅精确放行 `~/.local/share/ctxmode` 与
  `~/.gemini/config/plugins/agy-recall/state`（覆盖 `homeDir()`、`/root`、
  `/root/.agy-pro-home`），绝不放宽到 `~/.local/share` 或触碰
  `~/.local/share/gemini-web/chrome-profile`。
- pig AgentDir 私有隔离：进入 Landlock 前在 `<privateTmpDir>/pig-agent`（0700）下
  以 0600 拷入白名单配置文件（`settings.json`、`models.json`、`models-store.json`、
  `auth.json`、`keybindings.json`、`trusted-projects.json`，不拷 `sessions/`、`.lock`、
  日志等），并把 `extensions` 软链指向只读的宿主 `/root/.pig/agent/extensions`。
  `PIG_CODING_AGENT_DIR` 未被调用方显式指定时注入私有目录；显式指定时保留。
  宿主 `/root/.pig/agent` 保持 100% 只读，绝不进 `Policy.ReadWrite`。
- Cell 缓存预热与最小权限：宿主态在进入 Landlock 前扫描 `<PIG_HOME>/cache`
  （`PIG_HOME` 缺省取 `~/.pig`）下含有效 `ready.json` 的缓存单元，以原子写把
  `usage.json` 的 `lastUsed` 刷新为当前 Unix 秒（与 PiG `runtimecell.TouchUsage`
  同语义，沙盒内命中 <1h 纯只读短路，不写 `<digest>/` 临时文件）。
  仅 `cells/<lang>/.locks`、`ext/.locks`（宿主态预建 0755）与 `cache/toolchains`
  版本记录目录（含 `*.json.lock`）进 `ReadWrite`；仅已构建缓存单元的 artifact
  （`cells/<lang>/<digest>/runner`、`cache/ext/<entry>/<artifact>` 带执行位普通文件）
  进 `Exec`。

### Security

- 沙盒内 pig 不再拥有对宿主 AgentDir 的任何写权限；`usage.json` 刷新把
  运行期 `TouchUsage` 变为纯只读短路，`<digest>/` 单元目录保持只读。
- 新单测断言 `~/.local/share` 与 `~/.local/share/gemini-web` 绝不出现在
  `Policy.ReadWrite`，并断言 `Policy.ReadWrite` 与 `Policy.Exec` 之间不存在
  任何相等或父子包含关系（W^X）。

### Known Risks

- 既有边界不变：`/proc/<pid>/fd` TOCTOU、只读路径已存在 `.so` 经 `LD_PRELOAD` 注入、
  顶层放行后嵌套再起同名代理、`DenyNet: false` 等，详见 README 与 0.1.0 条目。
- 调用方显式设置 `PIG_CODING_AGENT_DIR` / `CTXMODE_DB` 时，对应目录的写权限
  由调用方自负责；预设只做最小兜底。

## [0.2.1] - 2026-11-01

PATCH：修复 reviewer-agent 下 agy 派活路径无法写状态 / 打开 TTY 的问题。

### Fixed

- `ReviewerAgent` 同时放行 agy 硬编码 HOME 下的状态根 `/root/.agy-pro-home/.gemini/antigravity-cli`（数据子目录 + 根级状态文件，**仍不含** `bin/`）。原先只按 `os.UserHomeDir()`/`$HOME` 开放 `~/.gemini/antigravity-cli`，与 agy 实际写入路径不一致，导致派活命令下 log / oauth token 刷新出现 `permission denied`，并表现为未登录。
- `ReviewerAgent` 额外放行 `/dev/tty`：`dispatch-agent-go reviewer --executor agy` 使用 `-i`（`--prompt-interactive`），bubbletea 需要控制终端；仅本预设放行，不改全局 `defaultNullDevices`。
- 根级 `bin` 符号链接（agy-pro-home 下常见）按名字显式跳过，避免被当成普通文件加入 ReadWrite 而破坏 W^X。

### Tests

- CLI / 预设测试断言 agy-pro-home 数据路径与 `/dev/tty` 在 ReadWrite 中；断言 `bin/` 不在 ReadWrite；断言 ReadOnlyReviewer 不获得上述放宽。

## [0.2.0] - 2026-11-01

MINOR：向后兼容的新预设与 CLI 能力。

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

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.3.0...HEAD
[0.3.0]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.2.2...go/sandbox/v0.3.0
[0.2.2]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.2.1...go/sandbox/v0.2.2
[0.2.1]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.2.0...go/sandbox/v0.2.1
[0.2.0]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.1.0...go/sandbox/v0.2.0
[0.1.0]: https://github.com/dorokuma/pi-extensions/releases/tag/go/sandbox/v0.1.0
