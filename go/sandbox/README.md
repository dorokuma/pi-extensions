# `go/sandbox` — 基于 Linux Landlock 的无特权进程沙盒

`go/sandbox` 提供零 `cgo`、仅依赖 `golang.org/x/sys/unix` 的 Linux Landlock 文件系统与网络沙盒封装，以及命令行工具 `cmd/sbx`，用于约束子代理（如只读代码审查员 `ReadOnlyReviewer`）的文件读写与程序执行边界。

## 环境要求

- **操作系统**：启用了 Landlock LSM 的 Linux 内核（`Linux >= 5.13`，自适应支持 ABI v1 至 v5+）。
- **构建方式**：纯 Go（`CGO_ENABLED=0`），不依赖 `libcap` 或任何 C 库。
- **权限模型**：库级 `sandbox.Apply` 基于 `prctl(PR_SET_NO_NEW_PRIVS, 1)`、
  `landlock_restrict_self` 与 `seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_NEW_LISTENER)`，无需 `root` 或 `CAP_SYS_ADMIN` 权限。
  命令行 `sbx` 除 Landlock 外默认对受限子进程启用 `CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC`
  联合命名空间隔离（socket 白名单视图 + 独立 `/proc` + 独立 IPC
  命名空间（隔离 SysV shm/sem/msg 与按名解析的 mq_open 系统调用）+ PID 1 轻量 init +
  Linux capabilities 裁剪至 `0x22` + mount-seal seccomp 锁），该路径需要
  `CAP_SYS_ADMIN`；无此能力时严格模式
  拒绝启动（父进程派生子进程失败退 `126`），`--best-effort` 下降级为不
  unshare 并跳过覆盖（与旧版共享宿主命名空间的行为一致，仅 Landlock
  边界生效）。

## 核心接口与预设

- `sandbox.Supported() (abi int, ok bool)`：探测当前运行内核是否支持 Landlock 并返回最高 ABI 版本。
- `sandbox.Apply(p Policy) error`：将沙盒策略不可逆地施加到当前进程（及后续派生的所有子进程），并注入 `Policy.Env` 环境变量。当 `Policy.Exec` 非空时，默认强制安装 `seccomp` `PROT_EXEC` 守卫；若非 `BestEffort` 模式下 `seccomp` 守卫安装失败，直接返回错误（`ErrSeccomp`）、不执行目标命令；仅在 `BestEffort` 模式下输出告警并降级。
- `sandbox.ReadOnlyReviewer(workdir string) Policy`：构造"只读审查员"预设策略：
  - 根文件系统 `/` 只读；
  - 仅允许 `<workdir>/reports` 与会话级私有临时目录 `/tmp/pi-reviewer-*`（含 `TMPDIR`、`GOTMPDIR`、`GOCACHE`）可写；
  - **严格执行 `W^X`（可写与可执行完全分离）**：所有 `ReadWrite` 目录（`<workdir>/reports`、`TMPDIR`、`GOTMPDIR`、`GOCACHE`）均**不**授予 `LANDLOCK_ACCESS_FS_EXECUTE` 权限；
  - 建立独立的只读可执行目录 `/tmp/pi-reviewer-exec-*`，通过受控的 `go` 包装器与前置构建代理支持 `go build`、`go vet`、`go test`；
  - 严格限制可执行二进制白名单，显式拦截嵌套代理与调度器（`pig`、`pi`、`agy`、`grok`、`dispatch-agent`、`herdr`，含 ELF 本体名 `dispatch-agent-go`、`herdr-core`、`herdr-mobile-bridge`，并闭合「禁名+`-`/`.`」前缀派生形态如 `dispatch-agent-go.bak-*`）以及直接调用 ELF 动态链接器（`ld-linux*.so*`）。
- `sandbox.ReviewerAgent(workdir string) Policy`：在 `ReadOnlyReviewer` 之上允许**顶层**启动 `agy`/`pig`/`pi`/`claude`（`EntryAgents`；软链解析目标须过身份校验——basename 命中白名单名 `agy`/`pig`/`pi`/`claude`/`agentapi`/`webm_encoder`，或含 `/<查找名>/` 路径段且位于已知安装根前缀之下；basename 命中禁名及其 `-`/`.` 前缀派生名（如 `dispatch-agent-go.bak-*`）一律拒，整个候选（含软链 norm）不收录并打 `WARN` 跳过，防 PATH 前置毒软链混入），并对 pig 与 agy MCP 生态做四层私有隔离：
  - **ctxmode/MCP 数据隔离**：点名放行 `ctxmode`/`codegraph-go`/`jev-mcp`/`jev-mcp-go`/`agy-recall`（含软链目标）；未显式指定 `CTXMODE_DB` 时注入 `<privateTmpDir>/ctxmode/context_mode.db`（0700）；宿主兜底仅精确放行 `~/.local/share/ctxmode` 与 `agy-recall` state，绝不放宽到 `~/.local/share` 或 `gemini-web`。
  - **pig AgentDir 私有隔离**：宿主 `/root/.pig/agent` 保持 100% 只读；进沙盒前把白名单配置文件 0600 拷入 `<privateTmpDir>/pig-agent`，并在私有 `settings.json` 中显式声明宿主扩展目录绝对路径（**不再用 `extensions` 软链**——软链会让自动发现的 `cfg.Source` 变成随机私有路径而脱靶 packed-go 打包缓存；已有 `extensions` 数组时相对本地路径条目相对宿主 `<pigHome>/agent` 解析并在未显式排除时追加宿主扩展目录），并按需注入 `PIG_CODING_AGENT_DIR`（`PIG_USE_PI_DIRS=1` 时对称注入 `PI_CODING_AGENT_DIR`）。
  - **Cell 缓存 W^X**：宿主态预刷新 `<PIG_HOME>/cache` 下有效缓存单元 `usage.json`（沙盒内 `TouchUsage` 命中 <1h 纯只读短路）；仅 `.locks` 目录与已有记录的 `cache/toolchains` 目录可写，仅已构建 `<digest>/runner` 与 `cache/ext` 可执行产物可执行，`ReadWrite`/`Exec` 无任何父子包含；目标路径经 `Lstat` 拒绝符号链接 + `EvalSymlinks` 校验严格位于预期根内且不与 `Exec` 重合。
  - **XDG_RUNTIME_DIR 私有隔离**：宿主 `/run/user/0`（systemd user bus、keyring 等敏感套接字）绝不进 `ReadWrite`；在 `<privateTmpDir>` 下预建 0700 短名 `rt` 并覆写注入 `XDG_RUNTIME_DIR=<privateTmpDir>/rt`。
  - **claude 私有配置目录**：CC 会话必写配置目录；注入 `CLAUDE_CONFIG_DIR=<privateTmpDir>/claude`（0700，每会话私有目录，加入 `ReadWrite`）后 CC 配置根整体切到私有目录，写面随之收敛；目录随 `privateTmpDir` 清理、无跨会话状态——CC 会话状态本就不需跨会话持久，固定共享目录会让沙盒内 claude 写的 settings/hooks/mcpServers 被下一次会话原样继承（跨会话投毒通道），与 pig-agent/ctxmode/XDG_RUNTIME_DIR 全落 `privateTmpDir` 的每会话隔离口径一致；宿主 `~/.claude` 与 `~/.claude.json` 仅随通用只读根顺带可读、CC 不消费其配置（宿主配置内容属不可信面，不放给沙盒内写）。
  - **Go 扩展按需宿主预热**：`sbx` 在 `buildPolicy` 之前调用 `MaybeWarmGoExtensionCells`——当 preset 为 `reviewer-agent`、目标命令经固定可信目录（`<trustedHome>/.local/bin`、`<trustedHome>/.pig/bin`、`/root/.local/bin`、`/root/.pig/bin`、`standardBinDirs`；真实文件必须落在可信前缀内、普通文件、`perm & 0o022 == 0`、属主 root 或当前 euid，绝不读调用方 `PATH`）解析为 `pig`、且 `<PIG_HOME>/agent/extensions` 下存在 Go 扩展时，在 `<PIG_HOME>/cache/.locks/sbx-warm.lock` 排他锁内以最小干净环境（私有 cwd、私有 `pig-agent` AgentDir、受控 `PATH`）执行 `pig --help` 完成 cell 编译。指纹（pig + `bin/pi`/`official/pig` 真实 ELF + 扩展/SDK 源清单）与 `cache/.locks/sbx-warm.sha256` 墓碑一致且存在有效产物（`cache/cells/go` 合包或 `cache/ext` 单扩展）时零子进程短路；子进程 stderr 命中 `Failed to load extension` / `extension build failed` 即使退 0 也拒绝写墓碑；预热前后指纹漂移有界重试 2 轮；预热期信号中断按进程组 SIGKILL 回收、`/tmp/sbx-warm-*` 零残留并以 `128+信号` 直接退出。

### `sbx --preset` 失败清理

`--preset reviewer` / `reviewer-agent` 在构造策略时会创建 `/tmp/pi-reviewer-*` 与 `/tmp/pi-reviewer-exec-*` 私有临时目录。若在进入 `syscall.Exec` 前的参数校验、命令解析或 `sandbox.Apply` 阶段提前退出，`sbx` 会主动清理这两个目录，不泄漏到 `/tmp`。

## 构建与测试

```bash
cd go/sandbox
go build ./...
go vet ./...
go test -v -race ./...
```

使用 `sbx` 检测当前机器的 Landlock 支持状态：

```bash
go run ./cmd/sbx --check
```

以 `ReadOnlyReviewer` 预设运行命令（支持 `--reviewer WORKDIR` 与 `--preset reviewer --workdir DIR` 两种等价写法）：

```bash
go run ./cmd/sbx --reviewer /path/to/repo -- go test ./...
# 或等价写法：
go run ./cmd/sbx --preset reviewer --workdir /path/to/repo -- go test ./...

# 只读审查员代理（允许顶层 agy/pig/pi/claude，扩展会话 RW；嵌套再起同名代理 OS 层仍可能成功，见 CHANGELOG）
go run ./cmd/sbx --preset reviewer-agent --workdir /path/to/repo -- /root/.local/bin/agy --help

# 只读角色回报中继：沙盒内 `hc send <父终端> <文本>` 经外层中继发送（仅 reviewer-agent）
go run ./cmd/sbx --preset reviewer-agent --workdir /path/to/repo --report-to t28 -- /root/.local/bin/agy
```

### `--report-to`：只读角色回报中继

`--report-to ID`（如 `t28`，派发方取外层 `HC_TERMINAL_ID` 传入）在 `reviewer-agent` 预设下启用回报通道：sbx 外层父进程（Landlock 规则集外）在 `<goExecDir>/report.sock`（0600）起中继，沙盒内 `hc` 是指向 sbx 本体的受限客户端软链，只接受 `hc send <addr> <text>` 精确两操作数（`hc --help`/`--version` 打受限说明退 0，其它形态退 126，中继不可用时退 75）。中继帧为 4 字节大端长度前缀 + JSON `{"to","text"}`（上限 64KiB），同时在途连接硬上限 32（超限连接立即回 `reject + relay busy` 丢弃，杜绝沙盒内廉价空拨号耗尽外层父进程 FD），校验 `to` 必须等于父地址，文本经 UTF-8 规范化、剔除除 `\n`/`\t` 外控制字符与全部 Unicode `Cf` 格式控制符（`\u202e` 双向覆盖符、零宽字符、BOM 等，防父终端渲染侧文本欺骗）、8000 字节字符边界截断，再受令牌桶（容量 3、每 30 秒补 1、每会话 20 条）限额；执行 `hc send` 时 argv 固定、不经 shell、超时 5 秒，文本以 `-` 开头时前置空格。真 `/root/.local/bin/hc` 不进任何 Exec 白名单。

### 沙盒 socket 白名单视图、进程面隔离与环境剥离（Linux，`CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC`）

受限子进程在 `clone` 时同时进入独立挂载命名空间、独立 PID 命名空间与
独立 IPC 命名空间（后者隔离 SysV `shm`/`sem`/`msg` 与按命名空间解析的 POSIX
`mq_open`/`mq_unlink` 系统调用：沙盒内读不到宿主共享内存段且无法按名打开宿主
POSIX 消息队列，沙盒内创建的 SysV IPC 对象随命名空间销毁不留宿主持久残留；
注：继承的宿主 /dev/mqueue 挂载视图未额外覆盖，见残余 #12；直接作为新 PID ns 的
PID 1 启动），在 `sandbox.Apply` 之前完成全部隔离步骤：

1. **PID-ns 前置硬校验**（先于一切宿主可见变异）：`os.Getpid()!=1` 说明进程仍留在共享 PID 命名空间，严格模式直接拒绝启动（绝不先改宿主 `/` 传播属性再报错）；`--best-effort` 下告警（点名 `/proc/<hostPid>/root` 魔链仍可达）并跳过全部覆盖挂载，仅执行降权与封锁；
2. **`/` 置 `MS_REC|MS_PRIVATE` 私有传播**，确保后续覆盖挂载不回流宿主（失败同样熔断全部覆盖挂载）；
3. **挂载新 PID ns 专属 procfs**（`MS_NOSUID|MS_NODEV|MS_NOEXEC`）——此后沙盒内 `/proc` 只反映本沙盒进程，宿主全部 PID 不可见，`/proc/<pid>/root|cwd|fd` 魔链只指向沙盒自身已覆盖 `/run` 的挂载命名空间，不再穿透宿主视图；`--best-effort` 下挂载失败时告警会显式点名 `/proc/<hostPid>/root magic-link remains reachable`；
4. **`/tmp/tmux-0` 存在时被空只读 tmpfs 覆盖**（tmux send-keys 注入面）；
5. **整个 `/run` 被 tmpfs 覆盖**，仅按原相对路径回绑白名单项：`/run/user/0/moshi-hook.sock`（agy 钩子遥测，socket 回绑保持可写，`connect()` 要求挂载点可写）与 `/run` 下的 `/etc/resolv.conf` 解析目标（普通文件，补只读 remount 保 DNS stub）；覆盖层最终整体 remount 为只读；
6. **继承 cwd 重绑**（`rebind-cwd`）：覆盖挂载只改变其后的绝对路径解析，内核已持有的 `fs->pwd` 仍引用覆盖前的底层目录项；隔离流程 `os.Chdir` 回原 cwd 并 `os.Getwd()` 复核，强制内核从新挂载命名空间根重解析 cwd。原 cwd 在新视图不存在（如位于被覆盖且未回绑的 `/run/user/0/herdr-core`）时返回 `ENOENT`，默认 fail-closed 拒绝启动；不会擅自切往 `--workdir`，正常目录的继承 cwd 契约不变；
7. **Linux capabilities 根治裁剪**（`drop-capabilities`）：先循环 `PR_CAPBSET_DROP` 收缩 bounding set（`CAP_SETPCAP` 留到最后一步——内核要求调用时仍持该能力），再 `PR_CAP_AMBIENT_CLEAR_ALL` 清空 ambient，最后 `capset` 把 `Effective`/`Permitted` 置为 `0x22`（仅 `CAP_DAC_OVERRIDE|CAP_KILL`：前者供 uid=0 清理只读权限临时目录且受 Landlock 约束，后者供独立 PID ns 内信号转发），`Inheritable` 置 0。`CAP_SYS_ADMIN`/`CAP_SYS_BOOT`/`CAP_SYS_MODULE`/`CAP_BPF`/`CAP_PERFMON`/`CAP_NET_ADMIN`/`CAP_SYS_TIME`/`CAP_SYS_RAWIO`/`CAP_SYS_PTRACE`/`CAP_SYS_CHROOT`/`CAP_DAC_READ_SEARCH`/`CAP_LINUX_IMMUTABLE` 等其余能力一律剥离，helper/broker 与目标命令等全部子孙经 fork/exec 继承同一已裁剪能力集；
8. **mount-seal seccomp-BPF 锁**（`PR_SET_NO_NEW_PRIVS` + `SECCOMP_FILTER_FLAG_TSYNC` 同步全部线程）：12 个挂载族（`mount`/`umount2`/`setns`/`unshare`/`pivot_root`/`open_tree`/`move_mount`/`fsopen`/`fsconfig`/`fsmount`/`fspick`/`mount_setattr`）+ 6 个越狱/进程控制族（`chroot`/`ptrace`/`process_vm_readv`/`process_vm_writev`/`pidfd_getfd`/`kcmp`）+ 27 个 capability 门控的宿主全局副作用/句柄/密钥环族（`reboot`/`kexec_load`/`kexec_file_load`/`init_module`/`finit_module`/`delete_module`/`bpf`/`perf_event_open`/`fanotify_init`/`syslog`/`sethostname`/`setdomainname`/`settimeofday`/`clock_settime`/`adjtimex`/`clock_adjtime`/`swapon`/`swapoff`/`quotactl`/`quotactl_fd`/`acct`/`open_by_handle_at`/`name_to_handle_at`/`add_key`/`request_key`/`keyctl`/`pidfd_send_signal`；amd64 另有 `ioperm`/`iopl`）共 45 个（amd64 47 个）系统调用一律 `EPERM`；amd64 下另以 `JGE __X32_SYSCALL_BIT` 封堵 x32 ABI 位绕过；`pidfd_open` 有意放行（Go 标准库管理沙盒内子进程需要，宿主 PID 在新 PID ns 内本就 `ESRCH` 不可达）。异构 ABI `KILL_PROCESS`；仅 `linux/amd64` 与 `linux/arm64` 安装，其它架构跳过。

隔离完成后 `sbx` 本体不直接 `exec` 业务命令，而是担任**轻量 PID 1 init**：派生目标命令并向它转发信号（其实际 PID 不固定为 2——`sandbox.Apply` 会先派生 helper/broker 辅助进程；信号经 `kill(目标子进程 PID, sig)` 送达）、以 `Wait4(-1, WNOHANG)` 循环回收孤儿僵尸进程（业务进程若为 PID 1 会忽略默认未捕获信号且无人收尸）；目标子进程退出即返回其退出码/`128+信号`，PID 1 退出后内核自动清理命名空间内残余进程。**信号转发分两层口径**：CLI 外层 `sbx` 父进程仅监听并转发 `SIGINT`/`SIGTERM`/`SIGHUP`/`SIGQUIT` 共 4 种；容器内 PID-1 init 监听 `SIGCHLD`/`SIGINT`/`SIGTERM`/`SIGHUP`/`SIGQUIT`/`SIGUSR1`/`SIGUSR2`/`SIGWINCH` 共 8 种——其中 `SIGCHLD` 仅用于唤醒 `Wait4` 收尸不转发，其余 7 种转发给目标子进程。

同时从环境剔除全部 `HC_` 前缀变量（保留 `HC_TERMINAL_ID`）、`GPG_AGENT_INFO`、`SSH_AUTH_SOCK`、`DBUS_SESSION_BUS_ADDRESS` 与宿主 `XDG_RUNTIME_DIR`（`--env` 显式传入同样剔除；预设注入的私有 `XDG_RUNTIME_DIR` 不受影响）。**退出码口径**：父进程派生子进程失败（`unshare` 不被支持/权限不足等）严格模式退 `126`，`--best-effort` 下告警回退不 unshare（跳过全部覆盖与挂载锁，绝不动宿主命名空间）；子进程内任一挂载/封锁步骤失败默认退 `1` 拒绝启动，`--best-effort` 下仅告警继续。已知边界（按设计保持可达）：`/root/.herdsman/herdsman.sock`（herdsman 扩展必需）、`/root/.config/herdr/herdr.sock`（老 herdr 状态上报）、goExecDir 内 `broker.sock`/`report.sock`。

## 受控发布 `scripts/deploy.sh`

`scripts/deploy.sh`（0755）是 `sbx` 二进制的唯一受控发布入口，禁止手改部署副本。它首先（在任何 `cd`/子 shell 与任何文件系统副作用之前）完成三项零副作用校验：拒绝从 `*/.wt/*` 或顶层 `*/.wt` 调用方物理 cwd 发起发布；校验 `SBX_DEPLOY_TARGET` 非空、不以 `/` 结尾、不指向已存在目录（含指向目录的软链）；相对目标路径仅在内存中拼接到调用方 cwd。随后只在满足以下**三道硬门禁**时才继续：

1. **禁止 worktree 发布**：`git rev-parse --absolute-git-dir` 与 `--git-common-dir` 的 `realpath` 必须相等（主仓 `.git` 非 worktree），且 top-level 不以 `/.wt` 结尾或含 `/.wt/`；
2. **禁止非 `main` 分支发布**：`git symbolic-ref --short HEAD` 必须为 `main`；
3. **禁止 dirty 工作树发布**：`git status --porcelain --untracked-files=normal` 独立赋值（保留非零退出码）后必须为空。

随后默认执行 `go vet -p 2 ./...` 与 `go test -p 2 -race -count=1 ./...` 预检（所有 `go` 调用显式 `-p 2` 限流，不依赖可被外部覆盖的 `GOFLAGS`）；**仅在三道硬门禁、版本提取与预检全部通过后才首次触碰文件系统**——`mkdir -p` 目标父目录并把 `SBX_DEPLOY_TARGET` 经 `pwd -P` 规范化为物理绝对路径（再次确认非目录），在目标同目录编译 `<target_dir>/.sbx.new.$$` 并注入 `-ldflags "-s -w -X main.version=<baseVer>+<shortSHA>"`（`<baseVer>` 权威源是 `go/sandbox/CHANGELOG.md` 首个已发布 `## [X.Y.Z]` SemVer 章节号——`cmd/sbx/main.go` 的 `var version = "dev"` 仅是未注入兜底，`[Unreleased]`/缺失/非 SemVer 一律拒绝，绝不产出 `sbx dev*`）。对临时二进制执行 `--version`（精确相等断言 `sbx <baseVer>+<shortSHA>` + SemVer+SHA 正则双校验）、`--check`（**Linux 下默认硬阻断**：须零退出且输出含 `Landlock supported`，仅显式 `SBX_DEPLOY_ALLOW_NO_LANDLOCK=1` 可在无 Landlock 内核测试环境降级为 WARN）以及 reviewer-agent + codegraph-go 冒烟（workdir 与 `reports/` 全部落在私有 `SMOKE_PIG_HOME` 内，绝不写主仓；冒烟结束经沙盒回传精确路径 + 严格 `^/tmp/pi-reviewer-[A-Za-z0-9._-]+$` 前缀校验清理两个私有目录，零残留且绝不通配误删并发审查窗格的目录）；**任一失败立即删除临时二进制退出，绝不触碰线上目标**。冒烟通过后先 `chmod 0555` 临时二进制并对它做 `chattr +i/-i` 前置探测（探测失败即在触碰任何备份与线上目标之前退出），再备份旧目标为 `<target>.bak-<时间戳>-<pid>-<旧版本标识>`（`0444`，mtime 取当前备份时刻；旧版本标识取自旧二进制 `--version` 首行末列、`awk` 输出无尾随换行、清洗非法字符后截断 40），然后仅在紧贴 `mv -f` 原子替换前解除旧目标 `chattr -i`（最小化无锁窗口，`lsattr` 复核确认 i 标志确实清除才继续），`mv -f` 与新 `chattr +i` 全部成功后才裁剪仅保留最新 3 个 `.bak*`（纯 bash 数组 + `[[ -nt ]]` 排序，新备份固定排保留首位，不使用 `ls -1t`；mv 或加锁失败时绝不提前删历史备份），最后 `chattr +i` 加**内核级防误写锁**；统一 `EXIT` `trap` 兜底清理临时二进制/冒烟目录并在部署未成功时恢复 `+i`（恢复失败显式 `ERROR`）。

另：脚本在任何 `git` 查询前先剥掉全部 `GIT_*` 本地环境变量（`git rev-parse --local-env-vars`），并强校验模块物理路径必须归属 `<toplevel>/go/sandbox`，封堵经 `GIT_DIR`/`GIT_WORK_TREE` 伪造主仓查询从而从 worktree 编译的绕过路径。

环境变量：`SBX_DEPLOY_TARGET`（目标路径，必须指向文件路径——首次安装尚不存在，或升级覆盖已存在普通文件；空值、以 `/` 结尾、已存在目录（含指向目录的软链）一律拒绝；允许相对路径，在任何 `cd` 前于内存中拼接到调用方 cwd，门禁通过后经 `pwd -P` 规范化为物理绝对路径）、`SBX_DEPLOY_SKIP_IMMUTABLE=1`（仅无 `CAP_LINUX_IMMUTABLE` 的单测/临时目录，跳过 `chattr`）、`SBX_DEPLOY_FAST_VERIFY=1`（仅防止 `go test` 内调用 `deploy.sh` 递归套娃 `go test`，仍执行 `go vet -p 2` 并跳过 `go test` 与 reviewer-agent 冒烟）、`SBX_DEPLOY_ALLOW_NO_LANDLOCK=1`（仅无 Landlock 内核的特殊测试环境，让 Linux `--check` 降级为 WARN）。生产发布这些开关均必须为 `0`/未设置。

```bash
# 生产发布（主仓 main、干净工作树）：
bash go/sandbox/scripts/deploy.sh
```

## 安全机制与本轮封堵的绕过路径

### 1. 可写临时目录与可执行目录分离（`W^X`）+ 受控 `go` 工具链代理

在早期版本中，为了让 `go test` 编译并执行生成的 `*.test` 二进制，私有临时目录 `/tmp/pi-reviewer-*`（`TMPDIR`、`GOTMPDIR`、`GOCACHE`）同时拥有读写和执行权限。审查员子代理可以把任意可读的 ELF（如 `/root/.local/bin/agy`、`/root/.local/bin/pi`、`dispatch-agent`）拷贝进该目录后再执行，从而绕过执行白名单。

本轮通过 **`W^X` 目录分离 + 受控 `go` 包装器代理** 彻底堵上该口子：

1. **所有可写目录一律不可执行**：`ReadOnlyReviewer` 下，`Policy.ReadWrite` 中的 `<workdir>/reports` 与 `/tmp/pi-reviewer-*`（含 `TMPDIR`、`GOTMPDIR`、`GOCACHE`）均不包含在 `Policy.Exec` 中。审查员把 `agy` 或任意 ELF 拷入这些目录后直接执行时，内核 Landlock 直接返回 `EACCES` 拒绝执行。
2. **可执行构建目录对审查员只读**：单独创建 `/tmp/pi-reviewer-exec-*`（`goExecDir`），在审查员进程的 Landlock 规则集中仅授予 `ReadOnly + Exec` 权限，审查员无法向其中写入、拷贝或重命名任何文件。
3. **前置受控构建辅助进程与 `pi-test-runner` 降权运行**：
   - 在 `sandbox.Apply` 对审查员主进程调用 `landlock_restrict_self` 之前，先派生一个同样受 Landlock 约束的内部辅助进程（该辅助进程仅比审查员多出对 `<goExecDir>/work` 与 `<goExecDir>/cache` 的写权限）。
   - `Policy.Env` 将 `<goExecDir>/bin` 置于 `PATH` 首位，其中的 `go` 包装器会对子命令和参数做严格白名单校验（允许 `build`、`vet`、`test`、`list`、`env`、`version`、`mod`、`doc`、`fmt`，拒绝 `run`、`generate`、`tool`、`install` 以及 `-exec`、`-toolexec`、`-overlay`、`go test -c` 或指向 `goExecDir` 的 `-o` 参数），再通过 Unix 域套接字请求辅助进程调用真实的 `go` 二进制。
   - 执行 `go test` 时，辅助进程自动注入 `-exec=<goExecDir>/bin/pi-test-runner`。`pi-test-runner` 在校验目标路径必须位于 `<goExecDir>/work/` 且以 `.test` 结尾后，**先在自身进程上叠加审查员的 Landlock 规则集（收回对 `<goExecDir>/work` 与 `<goExecDir>/cache` 的写权限）**，再 `execve` 运行编译出的测试二进制。这样既保证了 `go build`、`go vet`、`go test` 正常工作，又确保测试代码本身同样处于严格的只读审查沙盒内。

### 2. 拦截通过 `ld-linux` 直接加载非白名单 ELF

#### Landlock 在动态链接器上的内核机制边界

在 Linux 内核 `fs/binfmt_elf.c` 的 `load_elf_binary()` 中，当内核通过 `execve` 启动任何动态链接的 ELF 二进制（如 `git`、`bash`、`go`）时，内核会调用 `open_exec(elf_interpreter)` 打开 `PT_INTERP` 段指定的动态链接器（如 `/lib64/ld-linux-x86-64.so.2`）。`open_exec()` 会带上内部标志 `__FMODE_EXEC`，而 Landlock 的 `hook_file_open` 只要检测到 `__FMODE_EXEC` 就会强制检查 `LANDLOCK_ACCESS_FS_EXECUTE`。因此，若直接从 Landlock 规则集中移除 `ld-linux` 的 `LANDLOCK_ACCESS_FS_EXECUTE` 权限，所有动态链接程序都会在 `execve` 阶段因无法打开解释器而启动失败（`EACCES`）。

反之，当用户在命令行直接执行 `/lib64/ld-linux-x86-64.so.2 /root/.local/bin/agy` 时：
- 内核 `execve` 的主体是 `ld-linux`；
- `ld-linux` 在用户态以普通只读方式（`openat(..., O_RDONLY|O_CLOEXEC)`，不含 `__FMODE_EXEC`）打开 `/root/.local/bin/agy`，随后调用 `mmap(..., PROT_EXEC, ...)` 将其代码段映射为可执行内存并跳转执行；
- 由于 Landlock LSM（ABI v1–v5）仅挂载在 `bprm_creds_for_exec` 与 `file_open(__FMODE_EXEC)` 上，**并未挂载 `security_mmap_file` 钩子**，仅靠 Landlock 自身无法区分"`ld-linux` 被内核作为 `PT_INTERP` 加载"与"`ld-linux` 被直接执行并 `mmap(PROT_EXEC)` 任意可读 ELF"。

#### 本轮实现的多层拦截方案

1. **策略与入口层拒绝 `ld-linux`**：`Policy.Validate()`、`BuildExecWhitelist()` 与 `cmd/sbx` 均通过 `IsELFInterpreter()` 显式拒绝将 `ld-linux*.so*` / `ld-musl*.so*` / `ld.so*` 加入用户 `Policy.Exec` 或作为 `sbx` 目标命令直接启动。
2. **Seccomp `PROT_EXEC` 内存映射守卫（`SECCOMP_RET_USER_NOTIF`）**：
   - 当 `Policy.Exec` 非空时，`sandbox.Apply` 自动安装一段精简的 BPF 过滤器，仅拦截带 `PROT_EXEC` 标志的 `SYS_MMAP` 与 `SYS_MPROTECT` 系统调用（普通文件读写与不含 `PROT_EXEC` 的内存分配在内核态直接放行，零上下文切换开销）。若非 `BestEffort` 模式下 `seccomp` 过滤器或监听辅助进程安装失败，`sandbox.Apply` 直接返回错误（`ErrSeccomp`），拒绝继续执行目标命令。
   - 当触发 `PROT_EXEC` 映射时，监听器在用户态核查发起进程的 `/proc/<pid>/exe` 与目标文件描述符 `/proc/<pid>/fd/<fd>`：
     - **区分内核解释器加载与直接调用 `ld-linux`**：当白名单程序（如 `/usr/bin/git`）经由内核 `PT_INTERP` 正常启动时，`/proc/<pid>/exe` 指向 `/usr/bin/git`；而当审查员直接运行 `/lib64/ld-linux-x86-64.so.2 /root/.local/bin/agy`（或将其符号链接后运行）时，`/proc/<pid>/exe` 指向 `ld-linux-x86-64.so.2`。监听器一旦发现 `/proc/<pid>/exe` 为 ELF 动态链接器或禁用二进制，立即拒绝 `mmap`/`mprotect` 并返回 `EACCES`。
     - **阻止映射禁用二进制与可写目录中的文件**：若 `SYS_MMAP(PROT_EXEC)` 的目标 `fd` 指向 `ForbiddenExecNames`（`pig`、`pi`、`agy`、`grok`、`dispatch-agent`、`herdr`，含 ELF 本体名 `dispatch-agent-go`、`herdr-core`、`herdr-mobile-bridge`）或位于不可执行的 `ReadWrite` 目录（`TMPDIR`、`GOTMPDIR`、`GOCACHE`、`<workdir>/reports`）内，同样直接返回 `EACCES` 拒绝映射。这也封堵了将自编译 `.so` 写入可写临时目录后再通过 `LD_PRELOAD` 或 `dlopen` 加载的绕过手法。

## 残余风险与已知边界

1. **只读路径上已有的 `.so` 仍可经 `LD_PRELOAD` 注入**：当前 `seccomp` `PROT_EXEC` 守卫拦截的是「直接将 `ld-linux` 作为主程序启动」、「映射 `ForbiddenExecNames` 二进制」以及「从不可执行的 `ReadWrite` 目录（`TMPDIR`、`GOTMPDIR`、`GOCACHE`、`<workdir>/reports`）映射文件」。由于正常动态链接加载系统共享库同样通过 `mmap(..., PROT_EXEC, ...)` 完成，对于根文件系统 `/` 等只读路径上**原本已存在**的 `.so` 文件，若子代理对直接启动的动态链接程序设置 `LD_PRELOAD` 指向这些只读路径上已有的 `.so`（注：受控 `go` 构建代理内部已主动剥离 `LD_PRELOAD`、`LD_AUDIT` 与 `LD_LIBRARY_PATH`），当前沙盒不会拦截该只读 `.so` 的映射。
2. **白名单内的脚本解释器（`bash` / `sh` / `awk` / `sed`）**：`ReadOnlyReviewer` 白名单中包含 `bash`、`sh` 等工具，审查员仍可执行仓库或临时目录中的 Shell 脚本逻辑；但脚本中派生的任何外部二进制仍受 Landlock `Exec` 白名单与 `seccomp` `PROT_EXEC` 守卫双重约束，无法借此拉起 `agy`、`pi`、`pig`、`grok`、`dispatch-agent` 或其他非白名单 ELF。
3. **被审查仓库自身的测试代码与构建期代码**：运行 `go test` 时，被审查仓库中的 `*_test.go` 代码会在沙盒内执行。由于 `pi-test-runner` 在启动测试二进制前已叠加了审查员的 Landlock 规则集且继承了 `seccomp` `PROT_EXEC` 守卫，测试代码同样无法篡改仓库源码、无法向可执行目录写入文件、也无法执行禁用二进制。
4. **`/proc/<pid>/fd/<fd>` 路径检查的极窄 TOCTOU 窗口**：`seccomp_unotify` 通过读取 `/proc/<pid>/fd/<fd>` 校验被映射文件的路径。由于审查员对所有可执行目录均无写权限，且所有可写目录（`TMPDIR`、`GOTMPDIR`、`GOCACHE`、`reports`）均被整体禁止 `PROT_EXEC` 映射，审查员无法在可执行目录内通过硬链接或重命名制造竞争条件。
5. **默认未开启网络隔离**：`ReadOnlyReviewer` 默认 `DenyNet: false`；若需彻底阻断 TCP 网络连接，可将 `Policy.DenyNet` 设为 `true`（要求内核 Landlock ABI >= 4）或配合 `sbx --deny-net` 使用。
6. **`usage.json` 预刷新的时间窗口**：宿主态预刷新把 `lastUsed` 写入缓存单元；沙盒内 pig `TouchUsage` 的 `<1h` 只读短路只在「会话剩余时长 <55m」内稳定命中。`>55m` 的超长会话里 pig 可能重新尝试写 `usage.json` 临时文件并被 Landlock 拒绝——这属于 PiG 侧只读容错范围，不视为沙盒漏洞，但调用方应知晓长会话下 pig 可能报只读写入失败。
7. **冷启动新增语言 `cells/<lang>` 需宿主先预热构建**：`reviewer-agent` 预设的 Go 扩展 cell 已由 `sbx` 宿主态按需预热（见上文），无需手工预构建；其它语言（如 `cells/python`）首次构建产物仍需先在宿主跑一次构建生成 `ready.json`/可执行产物，后续沙盒会话才能复用。
8. **`cache/toolchains` 目录级可写**：toolchain `*.json.lock` 与记录文件同目录，无法只放锁文件路径；仅在目录下已存在记录时才放行，不创建空可写目录。
9. **`/tmp/herdr-role-sessions` 为同会话共享写根**：同一沙盒会话内多个角色会话共享该目录，不保证互相隔离。
10. **Landlock 路径级 `Exec` 无一次性 `execve` 计数器**：`EntryAgents`（agy/pig/pi/claude）顶层放行后，子进程在 OS 层仍可再 `execve` 同一路径；兜底依赖只读角色工具白名单（无 `bash`/`run_command`），`dispatch-agent`/`grok` 仍不在白名单。
11. **`CLAUDE_CONFIG_DIR` 落在 `privateTmpDir` 下随会话清理、无跨会话持久化**：CC 配置根为每会话私有的 `<privateTmpDir>/claude`（0700），随 `privateTmpDir` 清理、无跨会话状态；`buildPolicy` 在 `mergeEnv` 之后回钉该键，`--env CLAUDE_CONFIG_DIR=` 覆盖不会把写面指回宿主或与策略脱配。
12. **socket 白名单视图与降权边界的真实残余**：独立挂载/PID 命名空间 + 独立 procfs + capabilities 裁剪至 `0x22` + 扩充后宿主全局副作用族 `EPERM` 锁落地后，沙盒内 root 已无法经 `mount`/`umount2`/`setns`/`chroot`/`ptrace`/`pidfd_getfd` 恢复 socket 面或操纵宿主进程，也无 `CAP_SYS_ADMIN`/`CAP_SYS_BOOT`/`CAP_BPF` 等能力驱动 `reboot`/载模块/改时钟（残余 capability 门控调用另有 seccomp 按号拦截做纵深），`/proc/<pid>/root` 魔链只指向沙盒自身视图。但仍有按设计/架构保留的残余面：
    - **AF_UNIX 抽象命名空间（`@` 前缀）socket 不经过路径解析**，白名单视图天然不覆盖（如 `@/tmp/.X11-unix/X0` 形态的抽象地址）；
    - **`/tmp` 下除 `tmux-0` 外的其它宿主 socket 目录未覆盖**（如 `.X11-unix`、`.ICE-unix`；当前主机未运行 X 会话时无实际面）；
    - **非 `amd64`/`arm64` 架构不安装 mount-seal**（过滤器按架构生成，不支持平台跳过且不拒绝启动），该架构上覆盖层仍可被沙盒内 root 经 `umount2`/`setns` 撤销——socket 隐藏在该架构上是收敛非对抗性意外收口，不是特权行为边界；
    - **`--best-effort` 降级面**：PID-ns 硬校验未过或私有传播失败时跳过全部覆盖挂载（仅降权+装锁），`mount-proc` 失败时宿主 procfs 仍在——两种情形下 `/proc/<hostPid>/root` 魔链均可达，告警文案会显式点名该残余面；
    - **`--best-effort` 完全降级残余（O5′）**：派生时完全放弃命名空间（`MountNS=false`，unshare 失败回退）的降级路径下，除 `/proc/<hostPid>/root` 魔链外还存在更宽的残余面——未执行 `isolateRuntimeSockets` 意味着 capabilities 未裁剪至 `0x22`（宿主 root 仍持完整能力）、继承 `cwd` 未从新挂载视图重绑、mount-seal 锁未安装（走原 `syscall.Exec` 而非 init 路径），与挂载步骤失败的「降权+装锁」部分降级属不同口径；
    - **无特权只读宿主信息面（O3′）**：`AF_NETLINK`（如 `NETLINK_KOBJECT_UEVENT`/`NETLINK_ROUTE` 只读 dump）与 `/proc/sys`、`/sys` 的只读宿主信息在沙盒内仍可读——无特权只读面按设计保留，不属越权通道；
    - **继承的宿主 `/dev/mqueue` 挂载视图未覆盖**：`CLONE_NEWIPC` 隔离了按命名空间解析的 `mq_open`/`mq_unlink` 系统调用（按名打开宿主队列返回 `ENOENT`），但未额外重挂或 tmpfs 覆盖宿主继承的 `/dev/mqueue` 挂载（绑定于宿主 `init_ipc_ns`）；Landlock / 只读规则阻断了 `O_RDWR` 打开与新建写入（`EACCES`），但若宿主 `/dev/mqueue` 预存可读队列节点，沙盒内经路径 `O_RDONLY` 打开 fd 后仍可调用 `mq_timedreceive` 接收并取走宿主队列消息（当前主机常态无宿主 POSIX 队列）。
    - 按设计保持可达：`/root/.herdsman/herdsman.sock`、`/root/.config/herdr/herdr.sock`、goExecDir 内 `broker.sock`/`report.sock`。
13. **回报中继只限文本回父终端**：沙盒内 `hc` 客户端只接受 `hc send <父地址> <text>`；向非父地址、`hc run/exec/file/keys` 等其它形态一律拒绝。中继以沙盒外 root 执行真实 `hc send`，其信任面仅限于「校验后的文本 + 固定 argv」。
