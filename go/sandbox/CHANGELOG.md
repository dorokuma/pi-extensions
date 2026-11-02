# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/sandbox` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

## [0.3.0] - 2026-11-02

MINOR：以四层私有隔离取代 `0.2.2` 的
宿主目录写放行与 `cells/go` 目录级 RW∩Exec W^X 例外，向后兼容地补齐
reviewer-agent 预设对 pig 与 agy MCP 生态的安全修复（ctxmode/MCP 数据隔离、
pig AgentDir 私有隔离、Cell 缓存 W^X 预热、XDG_RUNTIME_DIR 私有隔离），并补齐
初审确认的 7 项修复（SBX-M1..M3、SBX-S1..S4）。

### Added

- 受控发布脚本 `scripts/deploy.sh`（0755）：`sbx` 二进制唯一受控部署入口。发起前先做三项零文件系统副作用校验（拒绝 `*/.wt/*` 与顶层 `*/.wt` 调用方物理 cwd；`SBX_DEPLOY_TARGET` 非空、不以 `/` 结尾、不指向已存在目录含指向目录的软链；相对路径仅在内存中拼接到调用方 cwd），随后剥掉全部 `GIT_*` 本地环境变量（`git rev-parse --local-env-vars`）并强校验模块物理路径归属 `<toplevel>/go/sandbox`（封堵经 `GIT_DIR`/`GIT_WORK_TREE` 伪造主仓查询从而从 worktree 编译的绕过）+ 三道硬门禁（禁止 worktree（top-level 不以 `/.wt` 结尾或含 `/.wt/`）/非 `main`/dirty 工作树发布，`git status --porcelain --untracked-files=normal` 独立赋值保留非零退出阻断）+ `go vet -p 2`/`go test -p 2 -race` 预检（所有 `go` 调用显式 `-p 2`，不依赖可被外部覆盖的 `GOFLAGS`）；**仅门禁与预检全部通过后才首次触碰文件系统**：`mkdir -p` 目标父目录、`pwd -P` 规范化物理路径并复核非目录，然后 `-ldflags "-s -w -X main.version=<baseVer>+<shortSHA>"` 版本戳注入（`<baseVer>` 权威源是 `go/sandbox/CHANGELOG.md` 首个已发布 `## [X.Y.Z]` SemVer 章节号——`cmd/sbx/main.go` 的 `var version = "dev"` 仅是未注入兜底且经 `grep` 强校验存在，`[Unreleased]`/缺失/非 SemVer 一律拒绝，绝不产出 `sbx dev*`）+ 替换前冒烟（`--version` 精确相等 + SemVer+SHA 正则双断言/`--check`（Linux 下默认硬阻断，仅 `SBX_DEPLOY_ALLOW_NO_LANDLOCK=1` 降级 WARN）/reviewer-agent+codegraph-go，workdir 与 `reports/` 全落私有 `SMOKE_PIG_HOME`，沙盒私有目录经回传路径 + 严格前缀校验清理，零残留）+ `chattr +i/-i` 前置探测（先于触碰任何备份与线上目标）+ 备份（`<target>.bak-<时间戳>-<pid>-<旧版本标识>`，`0444`，mtime 取当前备份时刻，旧版本标识 `awk` 提取无尾随换行不产 `_`）+ `chmod 0555` + 紧贴 `mv -f` 前才 `chattr -i`（最小化无锁窗口，`lsattr` 复核确认解锁才继续）+ `mv -f` 与新锁成功后才裁剪 `.bak*` 至最新 3 个（纯 bash 数组排序新备份固定排首位、不用 `ls -1t`，mv/加锁失败绝不提前删历史备份）+ **`chattr +i` 内核级防误写锁**（统一 `EXIT` `trap` 兜底清理与恢复，恢复失败显式 `ERROR`；`SBX_DEPLOY_SKIP_IMMUTABLE=1`/`SBX_DEPLOY_FAST_VERIFY=1`/`SBX_DEPLOY_ALLOW_NO_LANDLOCK=1` 仅供单测/特殊测试环境，生产必须关闭）。任一冒烟失败立即删除临时二进制、绝不触碰线上目标。配套 `cmd/sbx/deploy_test.go` 全程在 `t.TempDir()` 临时 Git 仓测试十六道分支（worktree/非 main/dirty/外部 `GIT_DIR` 伪装的拒绝 + `.wt` 调用方 cwd 拒绝含顶层边界与拒绝后零目录创建 + TARGET 形状拒绝（以 `/` 结尾/已存在目录/指向目录软链）+ CHANGELOG 仅 `[Unreleased]` 拒绝 + `cmd/sbx/main.go` 缺 `var version` 拒绝 + 干净 main 部署断言 `--version` 精确 `sbx <CHANGELOG首个SemVer>+<SHA>`/`0555`/`.bak*` 裁剪 + 旧 target mtime 早于既有备份的裁剪回归且备份名无尾随 `_` + 含空格路径首次安装 + 外部目录相对 `SBX_DEPLOY_TARGET` 规范化安装 + Linux `--check` 失败阻断 + `chattr` 前置探测失败阻断 + `lsattr` 仍报 i 时解锁校验阻断并断言 EXIT trap 回锁 + `mv` 失败时回锁且全部 `.bak*` 保留），测试运行器统一剥离继承的 `GIT_*` 变量。
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
  日志等），并在私有 `settings.json` 中经 `injectHostExtensionsSetting` 显式声明
  宿主扩展目录绝对路径（**不再创建 `extensions` 软链**——软链会让自动发现的
  `cfg.Source` 变成随机私有路径而脱靶 packed-go 打包缓存）。
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

### Changed（相对 `0.2.2` 行为）

- **废弃并移除** `0.2.2` 的 `pigLockReadWriteDirs` / `appendPigLockReadWrite`：不再向
  `/root/.pig/agent`、`/root/.pig/cache/cells/go`、`/root/.pig/cache/toolchains`、
  `/run/user/0/pig` 授予宿主写权限；`cells/go` 目录本身不再进入 `Exec`（
  0.2.2 的目录级 RW∩Exec W^X 例外取消，改为仅已构建 artifact 文件 Exec、
  仅 `.locks` 子目录与已有记录的 `toolchains` 目录可写）。
- **四层私有隔离**（完整保留本分支方案）：
  - `ResolveAgyHookPaths()` / `ResolveMCPServerPaths()` 按名放行 `ctxmode`、
    `codegraph-go`、`jev-mcp`、`jev-mcp-go`、`agy-recall` 及软链目标；
  - 私有 `CTXMODE_DB`（`<privateTmpDir>/ctxmode/context_mode.db`）+ 精确宿主兜底
    （`appendCtxmodeStateReadWrite`）；
  - 私有 `PIG_CODING_AGENT_DIR`（宿主 `/root/.pig/agent` 100% 只读，`settings.json`
    经 `injectHostExtensionsSetting` 声明宿主扩展目录）；
  - 宿主态预刷新缓存单元 `usage.json` + `appendCellCachePolicy`（严守 W^X）；
  - 私有短名 `XDG_RUNTIME_DIR=<privateTmpDir>/rt`（宿主 `/run/user/0` 100% 只读）。

### Fixed（初审 7 项）

- **SBX-M1**（跨平台编译）：`lookupEnvValue` / `overrideEnvKey` 自 `landlock_linux.go`
  移入无构建标签的 `sandbox.go`，`GOOS=darwin` 编译恢复通过；测试侧把仅 Linux 可用的
  `occupySeccompListenerSlot` 拆分至 `sandbox_linux_test.go` + `sandbox_other_test.go` 存根。
- **SBX-M2**（防软链逃逸与 W^X 绕过）：新增 `resolvePolicyPathWithin`，
  `appendCtxmodeStateReadWrite` 与 `appendCellCachePolicy` 对目标路径 `Lstat` 拒绝
  符号链接、`EvalSymlinks` 校验严格位于预期根之内且不与 `Policy.Exec` 已有路径重合。
- **SBX-M3**（`settings.json` 已有 `extensions` 数组）：`injectHostExtensionsSetting`
  支持把相对本地路径条目相对宿主 `<pigHome>/agent` 解析为绝对路径；未含等价
  `hostExtDir` 且未显式排除（`-`/`!` 前缀或 `-extensions`/`!extensions`）时追加，
  避免 `PIG_CODING_AGENT_DIR` 改指私有目录后丢失宿主默认扩展；`null`/非数组原样保留。
- **SBX-S1**（`PIG_USE_PI_DIRS=1` 对称注入）：`injectPigCodingAgentDir` 在
  `PIG_USE_PI_DIRS=1` 且 `PI_CODING_AGENT_DIR` 未设置时对称追加私有目录。
- **SBX-S2**（CLI 预 `Exec` 失败清理）：`sbx` 在 `--preset` 已生成私有 `TMPDIR` /
  `PI_SANDBOX_GO_EXEC_DIR` 后、进入 `syscall.Exec` 前的参数校验/二进制查找/沙盒初始化
  错误分支，主动 `os.RemoveAll` 清理，避免泄漏 `/tmp/pi-reviewer-*`。
- **SBX-S3**（测试隔离）：测试内裸 `os.Unsetenv` 全部改为 `t.Setenv` / `unsetEnv`（
  带 `t.Cleanup` 还原）；新增用例默认绑定 `t.TempDir()` 构造的隔离 `PIG_HOME` / `HOME`，
  不改写宿主真实 `/root/.pig/cache` 或 `~/.local/share/gemini-web/chrome-profile`。
- **SBX-S4**（文档对齐）：README、`preset.go` 注释（`usageTouchMinInterval`、
  `injectHostExtensionsSetting`）与本文件更新，清理过时 `extensions` 软链表述。

### Security

- 沙盒内 pig 不再拥有对宿主 AgentDir 的任何写权限；`usage.json` 刷新把
  运行期 `TouchUsage` 变为纯只读短路，`<digest>/` 单元目录保持只读。
- 新单测断言 `~/.local/share` 与 `~/.local/share/gemini-web` 绝不出现在
  `Policy.ReadWrite`，并断言 `Policy.ReadWrite` 与 `Policy.Exec` 之间不存在
  任何相等或父子包含关系（W^X）。

### Known Risks（继承并补充）

- `usage.json` 预刷新后，`<55m` 会话内 pig `TouchUsage` 命中 `<1h` 只读短路；
  `>55m` 超长会话属 PiG 侧只读容错范围外（可能回退为写临时文件并被 Landlock 拒绝）。
- 冷启动新增语言 `cells/<lang>` 需宿主先预热构建，沙盒内不创建新语言缓存树。
- `cache/toolchains` 在存在 `*.json`/`*.json.lock` 记录时为目录级可写（锁文件同目录要求）。
- `/tmp/herdr-role-sessions` 为同会话共享写根。
- Landlock 路径级 `Exec` 无一次性 `execve` 计数器：顶层放行后嵌套再起同名代理 OS 层仍可能成功。
- 既有边界不变：`/proc/<pid>/fd` TOCTOU、只读路径已存在 `.so` 经 `LD_PRELOAD` 注入、
  顶层放行后嵌套再起同名代理、`DenyNet: false` 等，详见 README 与 0.1.0 条目。
- 调用方显式设置 `PIG_CODING_AGENT_DIR` / `CTXMODE_DB` 时，对应目录的写权限
  由调用方自负责；预设只做最小兜底。

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
