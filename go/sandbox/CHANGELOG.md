# Changelog

本文件记录 `github.com/dorokuma/pi-extensions/go/sandbox` 的版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循仓库根目录 [VERSIONING.md](../../VERSIONING.md)。

## [Unreleased]

### Breaking Changes

- **`ReadOnlyReviewer` 移除 `<workdir>/reports` 自动创建与写授权**：`reviewer`/
  `reviewer-agent` 预设不再在目标 Git 工作树内创建 `reports/` 子目录，`<workdir>`
  与其 `reports/` 子目录不再获得任何写授权——报告统一收敛到
  `/tmp/herdr-role-reports`（仅 `ReviewerAgentWithEnv`/`--preset reviewer-agent`
  白名单）或各角色私有 `TMPDIR`。依赖 `<workdir>/reports` 旧写面的调用方须改经
  `DISPATCH_REPORT_PATH`/`--report-to` 或角色私有 `TMPDIR` 落盘（oracle SF-2）。
- **`ReadOnlyReviewer` `Policy.Exec` 由整个 `<goExecDir>` 收缩为
  `<goExecDir>/bin`（oracle9 MF-1）**：原整目录放行等于授予审查员执行
  `<goExecDir>/work/` 下任何 ELF 的权利（含经 broker/objcopy 写穿进去的
  `ForbiddenExecNames`/非白名单二进制）；收缩后审查员仅可执行
  `bin/go`、`bin/pi-test-runner`、`bin/hc` 受限客户端。`.test` 产物
  （`<goExecDir>/work/run-*/.../*.test`）不再对审查员主进程可执行：
  `pi-test-runner` 位于 `bin/` 内，启动前先叠加审查员规则集再 exec 目标——
  helper 进程（broker/`go test` 拉起方）在自身 Landlock 域内保留
  `<goExecDir>/work` 执行授权，`pi-test-runner` 经 broker 注入的
  `PI_SANDBOX_GO_TEST_WORK` 获得该产物根放行（仅本进程、非审查员全局）。
  调用方若在沙盒内直接 `exec` `<goExecDir>/work/*` 将收到 `EACCES`。
- **阶段 2 安全收口（W10 非 broker 收紧项，4 处缺陷修补）**：
  - `validateTrustedPigDir` 末级本体严格化：`Perm()&0o022 != 0` 无条件拒
    （含 `01777` sticky——`/tmp`/`var/tmp` 不得再被当作可信 `PIG_HOME`/
    AgentDir）；sticky 豁免仅留给 `filepath.Dir(dir)` 及之上的祖先级
    临时根。祖先链上的 `/` 直属 `uid==0` 顶层软链不再跳回 `/` 提前结束，
    改为 `os.Readlink` 单跳跟随（限 8 跳防环、悬空/越出根拒绝），继续
    逐级校验真实目标及其全部祖先——顶层软链指向非粘滞 `0777`、异属主
    目录或目标链内含非顶层嵌套软链的穿透路径全部被拒（兼容 macOS
    `/tmp -> private/tmp`、`/var -> private/var` 布局）。新增包内同口径
    seam `validateTrustedPigDirWithRoot(dir, label, sysRoot)`，生产固定
    `sysRoot="/"`，零外部入口。
  - `resolveAgentDirSelection` 默认层 `<pigHome>/agent` 闭合校验：新增
    `sourceTrusted` 字段，默认层与「候选被拒后回退默认目录」两条路径
    都对默认目录执行 `validateTrustedPigDir`；`MaybeWarmGoExtensionCells`
    与 `ReviewerAgentWithEnv` 在最终来源目录不可信（软链/`0777`/异属主）
    时一律回退空私有 `pig-agent`，绝不读取不可信 `<pigHome>/agent`。
  - `preparePrivatePigAgentDirFromAgent` 软链穿透封堵：`extensions` 目录
    与 `pigAgentConfigFiles` 白名单配置文件均由 `os.Stat` 改 `os.Lstat`，
    软链本体一律不作来源（私有 `pig-agent` 不再被越界拷入外部目标）。
  - `sandboxEnvStripKeys` 补剔 `PI_SANDBOX_INTERNAL_MODE`/
    `PI_SANDBOX_HELPER_CFG`/`PI_SANDBOX_BROKER_CFG` 三枚内部指针键；
    `buildPolicy` 对 `reviewer` 与 `reviewer-agent` 两个预设统一捕获并
    回钉 `PI_SANDBOX_GO_EXEC_DIR`；`cleanupPresetTempDirsFromPolicy`
    的清理身份改为「预设创建时经 `PI_SANDBOX_PRESET_TMP_DIRS` 登记的
    私有临时目录清单」——`ReadOnlyReviewer` 创建 `privateTmpDir`/
    `goExecDir` 后把二者绝对路径写入该内部 env 键，`buildPolicy`
    返回前捕获并从最终 env 剔除；清理只删登记集合（逐项保留
    `isPresetTempDir` 形态二次确认），不再对 `Policy.Exec` 做
    「条目本体或父目录前缀命中即删」的推断式清理（调用方经
    `--exec /tmp/pi-reviewer-exec-manual/bin` 传入的形似路径不再
    被误判为本预设创建而递归误删其父目录）。无登记清单时（单测/
    外部调用方手工构造 policy）才回退旧的 env 字段推断
    （TMPDIR/GOTMPDIR/PI_SANDBOX_GO_EXEC_DIR）+ ReadWrite 前缀命中，
    `Policy.Exec` 一律不参与清理推断。
  - **行为变化预告（分阶段归属，阶段 3 拆除 broker 前同步）**：
    `Policy.Exec` 收缩为 `<goExecDir>/bin` 是**本阶段（阶段 2）已生效**
    的收紧——自此沙盒内 `go test`/`go run` 的 `.test`/构建产物不再对
    审查员主进程直接可执行（直接 `exec` `<goExecDir>/work/*` 或在
    `GOTMPDIR` 下执行产物收到 `EACCES`）；「产物执行改经
    `pi-test-runner`（位于 `bin/` 内）在自身 Landlock 域内经
    `PI_SANDBOX_GO_TEST_WORK` 追加放行后完成」是**阶段 3 才完整落地**
    的收口——阶段 2 仍保留 broker/helper 旧路径，仅其 `Exec` 授权范围
    已随 `<goExecDir>/bin` 收缩同步收紧。

### Added

- **reviewer-agent fd3（claude-pty debugW）CLOEXEC 收口 + ExtraFiles 单路转发**：
  `runMain` 入口对 agent 预设继承的 fd3 置 `FD_CLOEXEC`（`sealInheritedFD3`），
  杜绝其泄漏进 `MaybeWarmGoExtensionCells` 预热子进程、`codegraph-go
  -warm-daemon` 常驻 daemon、`reportRelay` 的 `hc send` 及 `sandbox.Apply`
  派生的 helper/broker（无 CLOEXEC 的 debugW 写端被常驻 daemon 继承会永久
  劫持 `claude-pty` `drainDebug` 的 EOF）。`newChildCmd` 与 `runAsSandboxInit`
  经 `ExtraFiles` 显式单路转发给最终目标进程，父进程 `Start()` 后立即关闭
  自身 fd3 副本；无 mountns 的 `syscall.Exec` 分支在 `Exec` 前恢复
  `FD_CLOEXEC=0` 透传。`childExecSpec` 新增 `FD3Forward` 标记跨两跳传递；
  非 agent 预设不改变 fd3 语义。
- **helper 握手与限制施加 fail-closed + BestEffort 透传 + EINTR 重试**：
  `landlock_linux.go` 引入 `errSeccompFilterInstalled` 标记 seccomp
  PROT_EXEC 过滤器装入后的下游失败——post-install 失败不可撤销，
  BestEffort 也不得降级（否则 PROT_EXEC 映射永久阻塞在无人服务的 notify
  应答上）。helper 就绪握手 ACK 需 `n==1 && byte=='1'`，读错/EOF/非法字节
  均视为拒绝启动并 kill+wait 回收；broker 监听器未建立、`createAndPopulate
  Ruleset`/`enforceRuleset`/`installSeccompExecGuard` 失败均 fail-closed
  退出（`helperFailf`），绝不以未受限状态 ACK 继续服务；`runTestRunnerClient`
  配置缺失/损坏/必需路径不存在即返回 126 拒绝执行目标，绝不裸 exec。
  `helperConfig` 新增 `BestEffort` 透传父策略降级口径（缺省 false 会让
  helper 对 BestEffort 策略内容许的瞬态缺失路径 fail-closed，语义不一致）；
  `helperPolicy`/`reviewerPolicy` 均携带。握手链 `unix.Read`/`Sendmsg`/
  `Recvmsg` 包 `EINTR` 重试（SIGURG 异步抢占打断阻塞读不再误判为握手失败）。
- **reviewer-agent 私有 PIG_HOME + 宿主产物软链投影 + 启动前 fail-closed**：
  `ReviewerAgentWithEnv` 不再把 `Policy.Env["PIG_HOME"]` 指向宿主 `<PIG_HOME>`，
  改为指向 `<privateTmpDir>/pig-home`（0700，进 `ReadWrite` 不进 `Exec`）。
  私有 `bin`/`docs`/`state` 以软链指向宿主同名目录（pig 读 SDK/文档/本体
  均落宿主只读域，SDK 物理路径与宿主 prepare 完全一致）；`cache/cells/<lang>`
  与 `cache/ext` 为真实可写目录承载 `.build-*` 构建 scratch 与 `.locks`
  （目录本身不进 `Exec`，冷路径自建产物不可执行，W^X 不破）；宿主已构建的
  有效缓存单元经同名软链投影进私有 `cells/<lang>/<digest>` 与 `ext/<entry>`，
  沙盒内 pig 命中后经软链 exec 宿主真实 runner（已逐文件枚举进 `Policy.Exec`），
  修复原「宿主 cache 只读致 `.build-*` scratch 创建 EACCES」缺陷。新增
  `ReviewerAgentCellTargetError`/`ReviewerAgentCellTargets`：`buildPolicy` 在
  启动前对「扩展集（`sourceAgentGoExtensionDirs`）× 宿主有效产物集
  （`cellCacheEntryDirs`+`validCellEntryDir`）× 投影/Exec 命中」做完整性
  校验——有 Go 扩展但宿主产物为 0 或存在未成功投影/Exec 未命中项时启动前
  报错拒绝并指名缺失项，不进入「build 成功但 exec 失败」的不完全兼容态。
- **独立 PID 命名空间 + 专属 procfs（闭环 `/proc/<pid>/root` 魔链绕过与进程控制面）**：
  `CLONE_NEWNS` 派生升级为 `CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC`（Go
  `Cloneflags`，受限子进程直接以新 PID ns 的 PID 1 启动，并同时获得
  独立 IPC 命名空间——隔离 SysV `shm`/`sem`/`msg` 与按命名空间解析的
  POSIX `mq_open`/`mq_unlink` 系统调用：沙盒内读不到宿主共享内存段、
  按名打开宿主 POSIX 队列返回 `ENOENT`，沙盒内创建的 SysV IPC 对象随
  命名空间销毁不留宿主持久残留；继承的宿主 `/dev/mqueue` 挂载未额外
  覆盖，Landlock 阻断其写/建，但若宿主预存可读队列节点仍可经路径
  `O_RDONLY` 打开并 `mq_timedreceive` 取走消息）；覆盖 `/run` 之前先按硬校验
  （`os.Getpid()==1`，非独立 PID ns 拒绝继续）挂载 `MS_NOSUID|MS_NODEV|
  MS_NOEXEC` 的 procfs。此后沙盒内 `/proc` 只反映本沙盒进程：宿主全部
  PID 不可见，`/proc/<pid>/root|cwd|fd` 魔链只解析到沙盒自身已覆盖
  `/run` 的挂载命名空间（穿透宿主 socket 面需 `ptrace_may_access` +
  宿主视图，二者同时消失）；`ptrace`/`process_vm_*`/`pidfd_getfd` 对
  宿主进程的控制面因宿主 PID 不可达而天然失效。此前共享宿主 PID ns
  时 `/proc/1/root/run/docker.sock` 一行即可绕过全部挂载覆盖，等价
  沙盒外任意命令执行。
- **受控 `go` 构建代理参数/路径收口（oracle9 MF-1）**：`validateGoWrapperArgs`
  （客户端与 broker 双端）`-o` 校验改为解析首个已存在祖先目录的
  `EvalSymlinks` 真实路径后再判是否落入 `<goExecDir>`——沙盒内
  `ln -s $PI_SANDBOX_GO_EXEC_DIR/work $TMPDIR/sym` + `go build -o $TMPDIR/sym/pwn`
  的祖先软链写穿被拒；缺省 `-o` 且 `cwd`（`req.Dir`）解析后位于
  `<goExecDir>`（含 work/）的构建被拒；`runGoWrapperClient` 客户端预校验
  改为传真实 `cwd` 而非空串（空串会让 cwd 校验静默失效）；
  `-ldflags`/`-gcflags`/`-asmflags`/`-gccgoflags` 纳入禁用标志（可在 broker
  Landlock 域内拉起外部链接器/编译外挂，经 `-extld` 脚本借白名单 `objcopy`
  拷入禁用二进制）。新增 `resolvePathThroughAncestorSymlinks`：末级不存在时
  逐级向上找首个已存在祖先，经 `EvalSymlinks` 解析真实目标后拼回剩余段。
- **`validateTrustedPigDir` 祖先链逐级校验 + `appendCellCachePolicy` 仅入
  解析后真实 `artifactPath`（oracle9 SF-1）**：PIG_HOME/AgentDir 的末级
  `Lstat` 自动跟随中间软链，仅查末级会让 `pig-link/host` 或 `world-0777/host`
  穿透；末级不存在（ENOENT）时也必须校验整条已存在祖先链（否则 `MkdirAll`
  跟随祖先软链在外部目标建 `.locks`）。新增 `trustedPigDirModeOwnerOK`：
  每级目录须 Uid==0||euid 且非 0o022 可写，仅豁免 `/` 直属 uid==0 的系统顶层
  软链（macOS `/var`）与 uid==0 且带 `os.ModeSticky` 的 01777 临时根
  （`/tmp`、`/var/tmp`）；`artifactPath` 入 `Policy.Exec` 前先 `EvalSymlinks`
  并要求落在 `resolvedCacheRoot` 内（不写未解析软链别名——Apply 前改指别名
  可把任意/0777 二进制带入沙盒执行）。
- **轻量 PID 1 init**：`MountNS` 路径下 `sbx` 不再以 `syscall.Exec` 顶替
  PID 1（PID 1 忽略默认未捕获信号且无人回收孤儿僵尸），改由
  `runAsSandboxInit` 担任 init：派生目标命令（记录其实际 PID 并向它转发
  信号——`sandbox.Apply` 会先派生 helper/broker 辅助进程，业务进程 PID
  不固定为 2），外层 `sbx` 转发 `SIGINT`/`SIGTERM`/`SIGHUP`/`SIGQUIT`，
  容器内 init 另监听 `SIGUSR1`/`SIGUSR2`/`SIGWINCH`/`SIGCHLD`；每轮先
  `Wait4(-1, WNOHANG)` 排空已退出子孙（孤儿收尸），目标子进程退出
  即返回其退出码/`128+信号`；`MountNS=false`（`--best-effort` 降级）
  仍走原 `syscall.Exec`。
- **mount-seal 扩展至 45/47 系统调用 + x32 ABI 封堵**：在 12 个挂载族之上
  新增 `chroot`（魔链越狱第二跳）、`ptrace`、`process_vm_readv`、
  `process_vm_writev`、`pidfd_getfd`、`kcmp`（进程控制族），以及
  capability 门控的宿主全局副作用/句柄/密钥环族 27 个（`reboot`/
  `kexec_load`/`kexec_file_load`/`init_module`/`finit_module`/
  `delete_module`/`bpf`/`perf_event_open`/`fanotify_init`/`syslog`/
  `sethostname`/`setdomainname`/`settimeofday`/`clock_settime`/
  `adjtimex`/`clock_adjtime`/`swapon`/`swapoff`/`quotactl`/
  `quotactl_fd`/`acct`/
  `open_by_handle_at`/`name_to_handle_at`/`add_key`/`request_key`/
  `keyctl`/`pidfd_send_signal`，amd64 另有 `ioperm`/`iopl`），均返回
  `EPERM`；amd64 过滤器在 `arch` 校验与 `nr` 比较链之间新增
  `JGE __X32_SYSCALL_BIT(0x40000000)`，封堵与原生 amd64 共享
  `AUDIT_ARCH` 的 x32 调用号绕过。`pidfd_open` 有意不封——Go 标准库
  管理沙盒内自身子进程需要，而宿主 PID 在新 PID ns 内本就 `ESRCH`。
- **Linux capabilities 根治裁剪（`drop-capabilities`）**：全部覆盖挂载与
  继承 cwd 重绑完成后、seal 之前，循环 `PR_CAPBSET_DROP` 收缩 bounding
  set（`CAP_SETPCAP` 留到循环最后一步——内核要求调用时仍持该能力），
  `PR_CAP_AMBIENT_CLEAR_ALL` 清空 ambient，`capset` 把
  Effective/Permitted 置为 `0x22`（仅 `CAP_DAC_OVERRIDE|CAP_KILL`）
  Inheritable 置 0；helper/broker、目标命令与 `go test` 子孙经
  fork/exec 继承同一已裁剪能力集，`reboot`/载模块/改时钟/`bpf` 等
  capability 门控宿主副作用在能力校验层整体失效。优先
  `syscall.AllThreadsSyscall` 全线程生效，`-race`/cgo 下 `ENOTSUP`
  回退 `runChildFromEnv` 入口 `runtime.LockOSThread()` 锁定的线程。
- **继承 cwd 重绑（`rebind-cwd`，闭环相对路径拨号旁路）**：tmpfs 覆盖只
  改变其后的绝对路径解析，内核已持有的 `fs->pwd` 仍引用覆盖前的底层
  目录项——调用方从 `/run/user/0` 启动 sbx 时，沙盒内相对路径
  `herdr-core/default.sock` 可直拨覆盖前的宿主 socket。全部覆盖完成后
  `os.Chdir` 回原 cwd 并 `os.Getwd()` 复核，强制从新挂载命名空间根
  重解析 cwd；原 cwd 在新视图不存在（如 `/run/user/0/herdr-core` 未
  回绑）时 `ENOENT`，默认 fail-closed 拒绝启动，不擅自切往
  `--workdir`。
- **PID-ns 硬校验前置到一切宿主可见变异之前**：`isolateRuntimeSockets`
  首步即判 `os.Getpid()==1`，非独立 PID ns 时严格模式直接拒绝（绝不
  先私有化宿主 `/` 再报错），`--best-effort` 下告警点名
  `/proc/<hostPid>/root` 魔链仍可达并跳过全部覆盖挂载（仅降权+装锁）；
  `private-propagation` 失败与 `mount-proc` 失败的 best-effort 告警同样
  点名魔链残余面。
- **回报中继在途连接硬上限**：`serve()` 对同时在途连接设上限
  `reportMaxActiveConns=32`，超限连接立即以 100ms 写超时回
  `reject + relay busy` 并丢弃——杜绝沙盒内廉价空拨号耗尽外层父进程
  FD/内存并把 `consecErrs` 顶满杀死中继。
- **回报中继 Accept 永久错误自旋保护**：`serve()` 维护连续非关闭
  `Accept` 错误计数，达到 `reportAcceptMaxConsecErrs`（100 次，约 1s
  退避）主动退出循环，防止监听器 fd 进入永久错误态时空转；成功
  `Accept` 清零。
- **回报文本剔除 Unicode `Cf` 格式控制符**：`sanitizeReportText` 在剔除
  除 `\n`/`\t` 外控制字符之上，同步剔除 `\u202e` 双向覆盖符、\u200b
  零宽字符与 BOM 等 `Cf` 类别字符，防父终端渲染侧文本欺骗。

### Fixed

- **reviewer-agent 私有 AgentDir 身份不得从用户可控 TMPDIR 重建（N1）**：
  `buildPolicy` 原先在 `mergeEnv` 后用合并后的 `TMPDIR` 重新
  `filepath.Join(tmpdir, "pig-agent")` 推导回钉目标——`--env TMPDIR=`
  与 `--env PIG_CODING_AGENT_DIR=` 同时覆盖时，重建出的路径可指向宿主
  不可信目录（如 `--env TMPDIR=/root/.pig` 重建得 `/root/.pig/pig-agent`），
  重新消费宿主配置。修复为引入结构化 merge 前事实 `reviewerAgentDirFact`
  （`captureReviewerAgentDirFact` 在 `ReviewerAgentWithEnv` 产出、`cfgEnv`
  merge 前捕获：私有目录本体 / 受信自定义 / 有效目录三项），
  `normalizeEffectiveAgentDirs` 只消费该事实，绝不从合并后 env 重建私有
  目录身份。
- **宿主 `os.Environ()` 中 `PIG_USE_PI_DIRS`/`PI_CODING_AGENT_DIR` 残留
  透传进最终 exec 环境（N2）**：`stripSandboxEnv` 原只剔除
  `GPG_AGENT_INFO`/`SSH_AUTH_SOCK`/`DBUS_SESSION_BUS_ADDRESS`/`XDG_RUNTIME_DIR`，
  宿主残留的 `PIG_USE_PI_DIRS=1` + `PI_CODING_AGENT_DIR=<路径>` 会穿透进
  受限子进程——pig 包装器后置 `export PIG_USE_PI_DIRS=1` 时激活宿主 PI
  目录、打穿私有 AgentDir 隔离。修复：`stripHostEnv` 在 `stripSandboxEnv`
  之上仅对宿主 `os.Environ()` 追加剔除这两个键（`sandboxHostStripExtraKeys`），
  作用于 `runSandboxedChild` 受限子进程环境与 `runChildFromEnv` 最终
  `execEnv`；`--env` 显式声明的合法 PI 自定义入口不受剥离影响。
- **受信自定义 AgentDir 规范化拼写误丢 + 明确 PI 入口被遮蔽（N7）**：
  `trustedCustom` 判定原先用「解析后规范化路径 == cfgEnv 原始字符串」比较——
  通过 `validateTrustedPigDir` 校验的相对路径、冗余分隔（`a/./b`）、点段
  （`a/sub/..`）写法会因拼写不同被误丢、回钉私有目录；且仅
  `PIG_CODING_AGENT_DIR` 入口受信，`PIG_USE_PI_DIRS=1` + `PI_CODING_AGENT_DIR`
  的明确 PI 自定义入口被 PIG 预值遮蔽。修复：`customFromCfgEnv` 仅认
  `cfgEnv` 显式声明（PIG 与 `PIG_USE_PI_DIRS=1`+PI 双入口同权），与
  `srcTrusted` 合并成 `trustedCustom`；受信自定义经 `absCleanPath` 规范化
  后保留为有效目录，预热、配置复制与最终 exec 用同一解析结果。
- **宿主 `PIG_USE_PI_DIRS=1` 开关不得激活 `cfgEnv` 的 `PI_CODING_AGENT_DIR`
  为受信自定义（AgentDir 跨源选源显式化收敛）**：原实现把「显式自定义」
  判定分散在 `resolveSourcePigAgentDir`（复制/预热源，`lookup` 允许宿主
  兜底开关）、`customFromCfgEnv`（trustedCustom 有效目录，仅 cfgEnv）与
  `injectPigCodingAgentDir`（注入保持，仅 cfgEnv）三处——trustedCustom 与
  inject 虽已仅认 cfgEnv 开关，但复制/预热源的 `lookup` 允许宿主
  `PIG_USE_PI_DIRS=1` 兜底激活 `cfgEnv` 的 PI 值为自定义源，三处口径
  分裂易再冒认。收敛为统一 `resolveAgentDirSelection` 三层模型：显式层
  （`agentDirOriginCfgEnv`/`trustedCustom`）要求开关与 PI 键同在 `cfgEnv`
  内显式声明（`piDirsInCfg && strings.TrimSpace(piDirsV)=="1"`）；开关仅
  来自宿主环境时，cfgEnv 的 PI 值因 PI 键已在 cfgEnv 出现而不满足
  HostEnv 层「键未在 cfgEnv 出现」前提（`!piInCfg` 不成立），显式层
  与宿主回退层均不命中，直接回落第 3 层 `agentDirOriginDefault`
  （完全忽略未配对显式开关的 cfgEnv PI 值，既不置 `trustedCustom` 也
  不作复制/预热源），预热 `warm.go` 被拒分支口径与显式层一致——宿主
  残留开关无法借显式 PI 值冒认受信自定义。
- **GOROOT 别名漏洞（`resolveGoToolchainPaths`）**：旧实现对
  `os.Getenv("GOROOT")` 校验 `<GOROOT>/bin/go` 后把原始 `envGR` 加入
  `goRoots`，再直接放行 `filepath.Join(root, "pkg", "tool")`——伪造
  GOROOT 的 `bin/go` 只要是指向受信 go 的软链，伪造的
  `<GOROOT>/pkg/tool` 仍会整目录混入 `Policy.Exec`。修复：绝不把原始
  `envGR` 加入 `goRoots`，只从 `trustedRealGoCandidate` 返回的已解析
  真实 go 推导规范化可信根 `filepath.Dir(filepath.Dir(trusted))`；新增
  `trustedToolDir` 对 `pkg/tool` 做非软链真实目录 + 固定可信前缀 +
  `perm&0o022==0` + 属主（0 或当前 euid）四重校验，软链/外部/可写
  `pkg/tool` 一律不进 `Policy.Exec`。
- **GOROOT 中间软链别名彻底闭合（oracle8 MF-1）**：上项修复后仍有三条
  残余逃逸面——① `runtime.GOROOT()`（Go 标准库优先返回进程启动时
  `GOROOT` 环境变量原始值）未经任何校验直接进入 `goRoots`，伪造
  `GOROOT=<tmp>/fake-goroot` 绕过 `envGR` 受信判定；② `goRoots` 遍历时
  `<root>/bin/go` 未通过 `trustedRealGoCandidate` 并不跳过该 `root`，
  未认证根的 `pkg/tool` 仍被收录；③ `trustedToolDir` 仅 `Lstat` 末级
  `tool`——伪造根 `pkg -> /usr/local/go/pkg` 中间软链下
  `<fake>/pkg/tool` Lstat 为真实目录、`EvalSymlinks` 解析在可信前缀内，
  未解析的原始 `toolDir` 被写入 `Policy.Exec`（别名拼写获执行授权）。
  修复：`runtime.GOROOT()` 入口移除（绝不信任原始环境变量 GOROOT）；
  `bin/go` 未通过校验直接 `continue`，且 `pkg/tool` 只基于已解析真实
  go 推导的规范真实根 `filepath.Dir(filepath.Dir(trusted))` 收录；
  `trustedToolDir` 改为返回 `(string, bool)`（拆出包内可注入前缀的
  `trustedToolDirWithPrefixes`，生产固定 `trustedGoToolDirPrefixes` 零
  外部开启入口），调用点只把解析后的规范真实路径写入 `Policy.Exec`。
- **`ReviewerAgentWithEnv` 统一 `PIG_HOME` 受信校验 + `.locks` 先建后验
  修复（oracle8 SF-1，方案 1A）**：`warm.go` 预热已有
  `validatePigHomeDir`（拒软链/`0777`/异属主），但策略构造侧未执行同
  一校验——不可信 `PIG_HOME` 下 `<pigHome>/agent` 仍被作配置/扩展源、
  `usage.json` 仍被改写、其缓存产物仍枚举进 `Policy.Exec`；且
  `appendCellCachePolicy` 在 `resolvePolicyPathWithin` 拒软链前先
  `os.MkdirAll(..., ".locks")`，会跟随软链在外部目录凭空创建
  `.locks`。修复：`resolvePigHomeDir` 后立即 `validatePigHomeDir`；
  不可信时默认层 `AgentDir` 源回退为空私有 `pig-agent`（`cfgEnv` 独立
  可信自定义 `AgentDir` 不受牵连仍生效），跳过 `refreshCellUsageMetadata`/
  `appendCellCachePolicy` 与宿主 `bin/docs/state` 投影（经
  `prepareEmptyPrivatePigHome` 退回空私有 `pig-home`），并向
  `reviewerAgentCellTargets` 记录错误项（`hasGoExt` 强制为 true）使
  `ReviewerAgentCellTargetError()` 非 nil——真实 `pig` 入口启动前
  fail-closed，非 `pig` 命令以空私有根放行。`appendCellCachePolicy`
  在 `os.MkdirAll` 前冻结 `cache/ext`、`cache/cells` 与各 `cells/<lang>`
  的 `isRealDirNoSymlink` 判定，任一级软链即不为其创建 `.locks`；
  `artifactPath` 入 `Policy.Exec` 前追加 `Lstat` 普通文件 +
  `Perm()&0o022==0` + 属主合规校验，`0777` 全局可写二进制不再混入。
- **fd3 身份误认导致 `runtime: epollwait on fd 3 failed with 9`（plain
  模式 netpoll fatal）**：plain 模式（未注入调试管道）下 Go runtime 自身把
  fd 3 分配给 epoll（`anon_inode:[eventpoll]`，带 `FD_CLOEXEC`），旧
  `fd3Active()` 仅凭 `F_GETFD` 成功就把 fd3 当作 claude-pty 继承的
  debugW 管道，`acquireFd3File`/`closeFD3` 关闭了 runtime 的 epoll fd。
  修复：`fd3Active()` 收紧为「`flags&FD_CLOEXEC==0`（ExtraFiles 传入无
  CLOEXEC）且 `unix.Fstat` 为管道 `S_IFIFO` 或普通文件 `S_IFREG`」，
  `anon_inode`/socket/目录/带 CLOEXEC 的 runtime 内部描述符一律不接管；
  plain 与 pipe 两模式均不再误关 runtime epoll。
- **`TestReviewerAgentPigAgentDirIsolation` 私有 TMPDIR 路径假设修正**：
  生产 `ReadOnlyReviewer` 固定于 `/tmp` 创建 `pi-reviewer-*` 私有
  `TMPDIR`，旧断言 `HasPrefix(got, os.TempDir())` 在自定义 `TMPDIR`
  环境恒红；改以预设实际产出的 `policy.Env["TMPDIR"]` 拼接 `pig-agent`
  校验，不迁移生产私有根。
- **cell 目标集枚举口径分裂：`cells/go` 语言根软链逃逸（N4）**：
  `countValidExecutableGoArtifacts`（total）直接 `ReadDir(cells/go)` 会
  跟随符号链接，而投影侧 `preparePrivatePigHome` 与 Exec 完整性校验拒绝
  跟随语言根软链——宿主 `cache/cells/go` 为指向外部目录的软链时
  `total>0` 但投影为空、`missing` 为空，完整性门禁零投影假放行。修复：
  抽出共用谓词 `cellEntryHiddenOrSymlink`（软链/非目录/隐藏条目一律排除，
  `.tombstone-*` 除外），total、投影与 Exec 校验三方统一走
  `cellCacheEntryDirs` 同一枚举；`countValidExecutableGoArtifacts` 对
  `cells/go`/`ext` 根先 `isSymlinkPath` 拒绝跟随软链，并新增
  「宿主可执行 Go 产物 × 私有投影」一致性复核——任一有效 Go 产物未被
  软链投影进私有 cache 即记入 missing fail-closed。
- **预热子进程前置 go 目录可被调用方 PATH 投毒（N5）**：
  `runPigWarmSubprocess` 为对齐 cell 摘要把 `resolveRealGoBinary` 按宿主
  `PATH`/`PI_SANDBOX_REAL_GO` 选中的目录前置进预热 `PATH`——该解析不校验
  可信前缀，工作树/任意目录内放「假 go」并前置宿主 `PATH` 即让该不可信
  父目录被前置，预热子进程在 Landlock 生效前以宿主权限执行它
  （`controlledWarmPath` 刻意不读调用方 `PATH` 的既有防线被打穿）。修复：
  新增 `resolveSandboxRealGoBinary` 受信解析——先同口径解析，再经
  `trustedRealGoCandidate` 校验真实路径（`EvalSymlinks` 后）必须落在
  `trustedGoToolchainPrefixes`（版本化/系统 Go 安装根与 `standardBinDirs`
  标准 bin）之下；不可信命中直接放弃 PATH 对齐、回退固定可信目录顺序
  取首个可信 go（兜底与沙盒侧 `resolveRealGoBinary` 固定目录回退一致）。
- **reviewer-agent cell 校验三处缺陷（oracle FM-2）**：① cell 校验失败时
  `buildPolicy` 丢弃已创建 `policy` 返回空结构，早于
  `cleanupPresetTempDirsFromPolicy` defer——`/tmp/pi-reviewer-*`（含
  `0600` `pig-agent/auth.json` 凭据副本）与 `pi-reviewer-exec-*` 永久
  泄漏；修复为 fail-closed 前先清理。② fail-closed 对所有
  `reviewer-agent` 命令无条件执行——`claude`/`agy`/`sh` 等非 pig 命令
  `MaybeWarmGoExtensionCells` 不预热，冷缓存下被误拒启动；修复为仅对
  真实 pig 入口（`IsPigEntryCommand`，与预热同口径）执行校验。③
  `total` 用 `validCellEntryDir` 统计漏掉 `language=="go"` 与可执行位
  门禁——宿主 `cache/ext/node-runtime-*`（`language:"node"`）即让
  `total>0` 假通过；修复为改用 `countValidExecutableGoArtifacts`
  （语言门 + `mode&0o111`）。
- **`PIG_CODING_AGENT_DIR`/`PI_CODING_AGENT_DIR` 注入三缺陷（oracle FM-4）**：
  ① `injectPigCodingAgentDir` 未接 `cfgEnv`，`--env PIG_CODING_AGENT_DIR=`
  空覆盖屏蔽宿主变量后函数仍见宿主 `os.Getenv` 非空提前返回，空串写进
  最终 env；② 宿主残留恶意/旧 `PI_CODING_AGENT_DIR` 会在 pig 包装器后置
  `export PIG_USE_PI_DIRS=1` 时被激活生效；③ 仅 `PIG_USE_PI_DIRS=1` 时
  才对称注入 `PI_CODING_AGENT_DIR`。修复：函数接收 `cfgEnv`，「显式自定义」
  仅认 `cfgEnv`（`--env` 显式声明）；默认可信 `AgentDir` 下双变量一律经
  `overrideEnvKey` 回钉私有 `pig-agent`（空覆盖不再产生空路径）；宿主
  残留变量一律覆写。
- **reviewer-agent 在 `<workdir>/reports` 创建目录污染目标工作树
  （oracle SF-2）**：`ReadOnlyReviewer` 原在 `os.MkdirAll(<workdir>/reports)`
  并将其放入 `ReadWrite[0]`——目标 Git 工作树未忽略 `reports/` 时每次
  启动 sbx 都弄脏 `git status`，且使只读角色在源仓内拥有可写子目录。
  报告根已统一收敛 `/tmp/herdr-role-reports`，故移除该创建与写权限；
  `runTestRunnerClient` 取私有 `TMPDIR` 不再硬编码 `reviewerRW[1]`（
  ReadWrite 列表移除 reports 后按位置取会错位），改从 env `TMPDIR` 判定。
- **arm64 测试夹具（`fix-sbx-portable-baseline` 移植 + TMPDIR 隔离）**：
  `sandbox_test.go` 系统只读目录枚举改为必需目录存在性校验 + 可选库目录
  （`/lib64`/`/usr/lib64`，x86-64 专属）按实际存在追加——arm64 rootfs 无
  `/lib64`，原硬编码致 `addPathRule` 对非 optional 路径 ENOENT fail-closed；
  `TestReviewerAgentAgyHooks` 改 `t.TempDir` 夹具 + `HERDSMAN_GO_BIN`/`PATH`
  注入，不再依赖已退役的 `/root/.local/bin/herdsman`；
  `TestWarmGoExtensionCellsSignalInterrupt` 用 `t.Setenv("TMPDIR",
  t.TempDir())` 隔离 `os.TempDir()`，消除宿主 `/tmp` 历史 `sbx-warm-*`
  诊断文件污染零残留 glob 断言。
- **reviewer-agent 预热 cell 与沙盒内实际构建 toolchain 错配，导致 pig
  扩展在只读 `cells/go` 下重建 `.build-*` scratch 撞 `permission denied`**：
  `goPackedCellHash` 把 Go 工具链的 `commandVersion`/`goroot`/`compiler`
  折进 cell 摘要——预热 `pig --help` 与沙盒内 pig 必须解析到**同一个 go**
  才会命中同一个 `cells/go/<digest>/runner`。沙盒内 `go` 经 `goExecDir`
  包装器由 `resolveRealGoBinary`（宿主 `PATH` 顺序）选出真实 go；预热子进程
  原先只用 `controlledWarmPath()` 的固定目录顺序（`/usr/local/go/bin` 在
  `/usr/bin` 之前），在宿主 `PATH` 先命中 `/usr/bin/go` 的机器上解析出
  不同 go → cell 摘要必然错配 → 沙盒内 pig 在 `cells/go` 下
  `os.MkdirTemp(".build-*")` 重建被拒（`cells/go` 本身不在 ReadWrite，
  只放行 `.locks` 与已构建 artifact），15 个 Go 扩展全部加载失败、进程
  `exit 1`。修复：`runPigWarmSubprocess` 在与沙盒同口径 `resolveRealGoBinary("")`
  解析出真实 go 后，把其所在目录提前到受控 `PATH` 之首，保证预热/沙盒两侧
  `exec.LookPath("go")` 解析一致、cell 摘要命中宿主已构建 cell。
  **未放宽整个 `cells/go`、未把 `cells/go` 加入 ReadWrite 或 Exec，
  W^X 断言（`cells/go` 不得同时 ReadWrite+Exec）不变。**
- **`[pattern:verifier-transient-fail]` 结案（agy 瞬态状态文件 TOCTOU 击穿
  `sbx`）**：`appendAgyStateReadWrite` 经 `os.ReadDir` 快照枚举
  antigravity-cli 状态根下的根级文件（`settings.json`/`history.jsonl`/
  `*.db` 及其 SQLite 瞬态伴侣 `*.db-shm`/`*.db-wal`/`*.db-journal`、临时
  原子写文件）并追加进 `Policy.ReadWrite`；宿主 agy 在枚举与受限子进程
  `sandbox.Apply` 的毫秒级窗口内经 WAL checkpoint 删除 `.db-shm` 时，
  `unix.Open(path, O_PATH)` 返回 ENOENT 直接击穿整个沙盒启动
  （`sbx: apply sandbox policy failed: sandbox: open "...db-shm": no such
  file or directory`，即 2026-11-08 ROW48 悬案）。修复：`Policy` 新增
  `OptionalReadWrite` 字段（`Validate` 同样经 `NormalizePaths` 规范化校验），
  `appendAgyStateReadWrite` 枚举出的根级文件同步登记（授权仍完全由
  `ReadWrite` 决定，`OptionalReadWrite` 仅标记「缺失可容忍」）；该字段
  随 `childExecSpec.orw` 与 `helperConfig.orw` 跨
  `PI_SANDBOX_INTERNAL_CHILD_SPEC` 与 broker/helper/`pi-test-runner`
  全链路 100% 透传。`buildPathRuleEntries` 将 OptionalReadWrite 归一化为
  集合打标 `transient`，`addPathRule` 仅对 `transient` 条目容忍
  `ENOENT`（含悬空软链）安全跳过；未登记的普通 `ReadWrite` 缺失路径
  （显式 `--rw`、`<workdir>/reports`、私有 `TMPDIR`、agyStateDataSubdirs
  数据子目录等）保持严格 fail-closed ENOENT 报错不变。
- **`[0.4.0]` 文档勘误**：父进程派生受限子进程失败（`unshare` 不被支持/
  权限不足等）严格模式实际退 `126`（`--best-effort` 下降级），子进程内
  挂载/封锁步骤失败才退 `1`；原文「严格模式拒绝启动（退出码 1）」描述
  的是后者，已更正为两类失败路径区分。
- **init 文档口径修正**：README/CHANGELOG 中「派生目标命令为 PID 2」
  更正为「记录的目标子进程 PID」——`sandbox.Apply` 先派生
  helper/broker，业务进程 PID 不固定为 2；同时区分 CLI 外层 `sbx`
  父进程仅监听并转发 `SIGINT`/`SIGTERM`/`SIGHUP`/`SIGQUIT` 4 种信号，
  与容器内 PID-1 init 监听的 8 种信号（`SIGCHLD` 仅用于唤醒 `Wait4`
  收尸不转发，其余 7 种转发给目标子进程），不再混述为同一端到端
  能力。
- **drain 超时取消在途投递失效**：`deliver` 的 `cmd.Stdout/Stderr =
  io.Discard` 非 `*os.File`，`exec` 会建管道并起 copy goroutine；`hc`
  派生继承管道的孤儿子孙时，`cmd.Wait` 被拖到子孙退出才返回——
  `close()` drain 超时 `cancel()` 杀死 `hc` 本体后 `Wait` 仍挂至其
  孤儿子孙自然退出。改为 stdout/stderr 置 nil（走 `/dev/null`，不建
  管道），drain 超时取消路径恢复即时生效。

### Changed

- **集成回归断言加固**：帧边界测试改以协议字面常量 `65532`/`65533`
  构造（不再引用生产常量生成边界，回退实现即红）；moshi 拨号只明确
  接受 `connected`/`ECONNREFUSED`，`EROFS`/`ENOENT`/`EACCES`/超时等其余
  错误一律判失败；沙盒内新增独立 PID ns 断言（PID 1 为 sbx init 本体、
  宿主父 PID `ENOENT`、`/proc/1/root` 经宿主 `/run` 全权 socket 路径
  `ENOENT`）与扩展 seccomp 探针（`chroot`/`ptrace`/`pidfd_getfd`/amd64
  `x32` 位全部 `EPERM`）。

- **禁名后缀闭合：`.` 与 `-` 双分隔符前缀派生**：`isForbiddenBaseName`
  的派生判定从仅 `禁名+"-"` 扩为 `禁名+"-"` 与 `禁名+"."` 两种前缀
  （`basename == 禁名` 或以其前缀开头即命中），此前
  `dispatch-agent-go.bak-*` 这类「禁名 + `.` + 任意后缀」形态不被
  `IsForbiddenExec`（含 `EvalSymlinks` 解析后判定）、`Policy.Validate`、
  Landlock Exec 白名单、seccomp `shouldDenyExecMmap` 及软链目标身份校验
  层任何一层拦阻——软链指到 `.bak` 备份二进制的命令名即可绕穿透。
  身份校验层 `entryAgentTargetRejectReason` 改直接复用
  `isForbiddenBaseName`，两层语义保持一致；`allowedTargets` 白名单优先于
  禁名档，合法入口（agy/pig/pi/claude 版本化安装等）不受影响。
- **入口代理身份校验「not in allowed roots」档补测试**：新增
  `TestResolveEntryAgentPathsRejectsNotInAllowedRoots`，覆盖
  `claude → /tmp/<rand>/claude/evil`（含 `/claude/` 段但不在任何安装根
  前缀下）反例，断言 WARN 命中 `not in allowed roots, skipped` 且
  软链 norm 与目标 rn 均不入 `EntryAgents`，与既有
  `missing /<name>/ path segment` 档互补。

### Changed

- **短禁名不再参与「禁名+`-`」前缀派生**：前缀派生增加
  `forbiddenNameDeriveMinLen`（=5）门槛——`pi`/`agy`/`pig`/`grok` 是大量
  合法工件名前缀（`pi-reviewer-*` 受控执行目录、`pig-agent` 私有配置目录、
  `agy-recall`/`agy-rules-hook`/`agy-pro-home` 等），短名无门槛派生会把
  预设自建的合法 `Exec` 路径误判禁名、击穿全部预设。短禁名仍按精确匹配
  判定；长度 ≥ 5 的禁名（`dispatch-agent`、`dispatch-agent-go`、`herdr`、
  `herdr-core`、`herdr-mobile-bridge`）继续 `-` 派生并新增 `.` 派生。
- **`sbx --report-to` 父终端地址白名单收紧**：`reportAddrRe` 收紧为
  `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`——首字符必须是 ASCII 字母或数字
  （剔除前导 `-`/`.`/`_`），总长 1–64，字符集不含 `:`/`/`；封堵
  `--report-to=-flag` 这类向 `hc send` 注入选项参数的地址，并拒绝旧式
  `ws:p7` 与 `a/../x` 路径形态。非法地址保持既有降级语义：stderr 打印
  告警并仅关闭回报中继（客户端侧以退出码 75 报告），沙盒照常运行；
  校验口径与 `dispatch-agent` 的地址校验完全一致。
- **`ResolveEntryAgentPaths` 隔离 HOME 单测去宿主依赖夹具化**：
  `sandbox_test.go` 的 `TestResolveEntryAgentPathsIsolatedHomeKeepsHostRoots`
  与 `TestResolveEntryAgentPathsIsolatedHomeKeepsAgyStateBin` 改用
  `t.TempDir()` 合成宿主安装根夹具（claude 版本化安装根与 agyStateBin
  三成员），经测试专用 seam `sandbox.ResolveEntryAgentPathsForTest`
  注入 `hostHome`（与生产 `resolveEntryAgentPaths(trustedHomeDir())`
  同一代码路径，生产 HOME 信任边界不变），消除干净容器/CI 未安装
  `claude` 或 `agy` 时的静默 `t.Skip`。

## [0.4.0] - 2026-11-07

MINOR：只读角色回报中继（`--report-to`）+ 沙盒 socket 白名单视图 +
环境剥离。Landlock ABI 6 不管 pathname unix socket 的 `connect()`，
此前沙盒内任意代码可直连 `/run/user/0/herdr-core/*`（hc 控制面）、
`systemd/private`/`bus`（驱动 root systemd，等价沙盒外任意命令执行）、
`/tmp/tmux-0/*`（终端按键注入）、gnupg/openssh_agent（凭据代理）。

### Breaking Changes

- **CLI 默认启用 `CLONE_NEWNS|CLONE_NEWPID` 联合命名空间隔离**：此前受限
  子进程与父进程共享宿主挂载与 PID 命名空间；现在子进程在独立 mount ns
  与 PID ns 内执行（覆盖挂载不传播回宿主，宿主进程在沙盒 `/proc` 下
  不可见）。无 `CAP_SYS_ADMIN` 可用的环境（如非特权用户且内核未放行
  unprivileged user-ns）下 `unshare` 会失败：**父进程派生子进程失败**
  严格模式退 `126`，`--best-effort` 下告警降级回旧行为（不 unshare、
  跳过全部 socket 覆盖、不安装挂载锁）；**子进程内挂载/封锁步骤失败**
  退 `1` 拒绝启动，`--best-effort` 下仅告警继续。
- **socket 白名单视图从 `/run/user/0` 扩为整个 `/run`**：此前仅覆盖
  `/run/user/0`；现在整个 `/run` 被空 tmpfs 覆盖，仅按原相对路径回绑
  `/run/user/0/moshi-hook.sock` 与 `/run` 下的 `/etc/resolv.conf`
  解析目标（普通文件）。沙盒内原可达的 `/run` 下其它对象——
  `/run/systemd/private`、`/run/docker.sock`、
  `/run/containerd/containerd.sock`、`/run/dbus/system_bus_socket`、
  `/run/user/0/herdr-core/*` 等——全部不可见，依赖它们的沙盒内代码
  将收到 `ENOENT`/`EACCES`。迁移方式：确有需求的对象只能经由新增
  白名单回绑项放行（需改码并过审查）。
- **沙盒内挂载族与进程控制族系统调用一律 `EPERM`**：全部覆盖/回绑与
  暂存清理完成后、`sandbox.Apply` 之前安装 `PR_SET_NO_NEW_PRIVS` +
  seccomp-BPF 过滤器（支持 `linux/amd64` 与 `linux/arm64`，异构 ABI
  直接 `KILL_PROCESS`，amd64 另封堵 x32 `__X32_SYSCALL_BIT`），
  `mount`/`umount2`/`setns`/`unshare`/`pivot_root`/`open_tree`/
  `move_mount`/`fsopen`/`fsconfig`/`fsmount`/`fspick`/`mount_setattr`
  与 `chroot`/`ptrace`/`process_vm_readv`/`process_vm_writev`/
  `pidfd_getfd`/`kcmp` 共 18 个系统调用返回 `EPERM`；过滤器与 NNP 跨
  `execve` 继承。此前沙盒内 root 可 `setns` 跳回宿主挂载命名空间、
  `umount2` 撤销覆盖层、`chroot` 组合魔链越狱或 `ptrace`/`pidfd_getfd`
  操纵宿主进程，这些逃逸路径均被堵死；依赖上述系统调用的沙盒内代码
  将失败。`--best-effort` 下锁安装失败仅告警降级。

### Added

- **`--report-to ID`**：父终端地址（如 `t28`，由派发方取外层
  `HC_TERMINAL_ID` 传入）。仅 `--preset reviewer-agent` 且地址合法且可信
  hc 存在时，sbx 外层父进程（在 Landlock 规则集外）在
  `<goExecDir>/report.sock`（0600）起回报中继，先于受限子进程就绪：
  4 字节大端长度前缀 + JSON `{"to","text"}`（整包上限 64KiB），每连接 5s
  读 deadline，校验顺序为 `to` 必须等于父地址 → UTF-8 规范化并剔除除
  `\n`/`\t` 外 C0/C1 控制字符 → 超 8000 字节按字符边界截断并追加
  `…[truncated by sbx]` → 令牌桶（容量 3，每 30 秒补 1，每会话 20 条）→
  `exec.CommandContext` 固定 argv `hc send <parent> <text>`（不经 shell，
  超时 5s，文本以 `-` 开头时前置空格——hc 的 clap 参数解析不接受 `-`
  开头的 TEXT 操作数）。回包 1 字节状态（0 成功 / 1 拒绝 / 2 投递失败）
  + 简短原因；退出时先关监听再在途等待最多 6 秒。hc 仅在固定可信位置
  解析（`/root/.local/bin/hc` 与 trustedHome 同路径，绝不走 PATH）；
  外层 `HC_TERMINAL_ID` 存在且等于 `--report-to` 时拒绝启用（防发给自己）。
- **沙盒内 `hc` 受限客户端**：`ReviewerAgentWithEnv` 在
  `<goExecDir>/bin/hc` 建指向 sbx 本体的软链（仅此预设），`init` 分发按
  argv[0]==`hc` 进入 `runReportClient`：仅放行 `hc send <addr> <text>`
  精确两操作数经 report.sock 提交；`hc --help`/`hc --version` 打印受限
  说明退 0；其它形态退 126；中继不可用或 `PI_SANDBOX_GO_EXEC_DIR` 为空
  时 stderr 一行退 75。真 `/root/.local/bin/hc` 不进任何 Exec 白名单。
- **socket 白名单视图（Linux，`CLONE_NEWNS|CLONE_NEWPID`）**：受限子
  进程同时进入独立挂载与 PID 命名空间（Go 标准库只对 `Unshareflags`
  路径自动置 `/` rprivate，`Cloneflags` 路径不会自动执行，由
  `isolateRuntimeSockets` 首步 private-propagation 硬前置执行
  `MS_REC|MS_PRIVATE`，失败即熔断全部覆盖挂载；子进程即新 PID ns
  的 PID 1），在 `sandbox.Apply` 之前：
  先硬校验 `os.Getpid()==1` 后挂载专属 procfs（宿主 PID 与
  `/proc/<pid>/root` 魔链彻底不可见），随后整个 `/run` 被 tmpfs 覆盖，
  只按原相对路径回绑白名单项——`moshi-hook.sock`（agy 钩子遥测，
  socket 回绑保持可写，`connect()` 要求挂载点可写）与 `/run` 下的
  `/etc/resolv.conf` 目标文件（DNS stub，补只读 remount），覆盖层整体
  remount 为只读；`/tmp/tmux-0` 存在时被空只读 tmpfs 覆盖。覆盖路径
  不存在跳过；任一挂载步骤失败默认拒绝启动（fail-closed），
  `--best-effort` 下仅告警。unshare 失败在 `--best-effort` 下回退不
  unshare（此时跳过全部覆盖，绝不动宿主命名空间）。全部挂载与清理
  完成后安装 mount-seal seccomp 锁（见 Breaking Changes）。
- **轻量 PID 1 init 收尸与信号转发**：`MountNS` 路径下 `sbx` 本体作为
  新 PID ns 的 PID 1 不直接 `exec` 业务命令，而是派生目标命令并担任
  init（记录目标子进程 PID、转发信号、`Wait4(-1, WNOHANG)` 收尸），
  避免业务进程作为 PID 1 忽略默认未捕获信号与残留孤儿僵尸。
- **回报中继生命周期屏障**：`serve()` 在 `mu` 临界区内判 `closing` 并
  同步 `conns` 登记 + `wg.Add`，消除 `Add`/`Wait` 竞态；非 `ErrClosed`
  的 Accept 错误（EMFILE/ENFILE 等）退避 10ms 继续循环而非永久退出；
  `close()` 先关监听、等 accept 循环退出，再以 6s drain 定时器收尾，
  超时则取消根 `ctx`（回收在途 `hc send` 子进程）并强关残余连接。
  `PI_SANDBOX_GO_EXEC_DIR` 与 `PIG_HOME`/`CLAUDE_CONFIG_DIR` 同口径
  回钉，`--env` 不能劫持 relay 监听路径。帧体上限 64KiB-4（整包含 4B
  头不超 64KiB），响应写 deadline 独立 5s，令牌桶 refill 保留余数。
- **环境剥离**：沙盒内剔除全部 `HC_` 前缀变量（保留 `HC_TERMINAL_ID`）、
  `GPG_AGENT_INFO`、`SSH_AUTH_SOCK`、`DBUS_SESSION_BUS_ADDRESS`、
  `XDG_RUNTIME_DIR`；作用于宿主继承环境（`runSandboxedChild` 的 cmd.Env
  与 `runChildFromEnv` 的 mergeEnv 基底）与 `--env` 传入值（`buildPolicy`
  同口径过滤）。预设自身注入的 `XDG_RUNTIME_DIR`（reviewer-agent 私有
  0700 目录）不属于外部指针，不受影响。

### Known Risks

- 已知边界（按设计保持可达）：`/root/.herdsman/herdsman.sock`（herdsman
  扩展必需）、`/root/.config/herdr/herdr.sock`（老 herdr 状态上报）、
  goExecDir 内 `broker.sock` 与 `report.sock`。
- mount-seal seccomp 锁仅在受限子进程的 `CLONE_NEWNS|CLONE_NEWPID`
  路径安装：`--best-effort` 下 unshare 失败回退不 unshare 时，既不施加
  socket 覆盖也不安装挂载锁（与挂载步骤同口径告警降级），此时沙盒内
  root 仍可执行挂载类与进程控制类系统调用——降级属可观察 WARN，不作
  静默承诺。
- mount-seal 过滤器支持 `linux/amd64` 与 `linux/arm64`；其它 Linux
  架构跳过安装（不拒绝启动），挂载逃逸面在该架构上保持旧行为。

## [0.3.3] - 2026-11-07

PATCH：加固 `reviewer-agent` 预设并并入 `claude`（CC）入口支持——软链目标
身份校验封堵 PATH 前置毒软链混入白名单、`ForbiddenExecNames` 补 ELF
本体名堵三层拦截穿透、`CLAUDE_CONFIG_DIR` 每会话私有写面并回钉防
`--env` 脱配、软链校验 WARN 按拒绝原因分档。

### Added

- **`claude` 加入 `ReviewerAgent` 入口白名单**：`ResolveEntryAgentPaths`
  的查找名集合增加 `claude`（PATH、`standardBinDirs`、
  `agentLookupDirs`），沙盒内可经 `sbx --preset reviewer-agent` 顶层
  启动 `claude`（CC）。
- **claude 私有配置写面**：`ReviewerAgentWithEnv` 预建 `<privateTmpDir>/claude`
  （0700，每会话私有），加入 `ReadWrite` 并经 `overrideEnvKey` 注入
  `CLAUDE_CONFIG_DIR`。CC 会话必写配置目录；注入后 CC 配置根整体切到
  私有目录，宿主 `~/.claude` 与 `~/.claude.json` 仅随通用只读根顺带
  可读、CC 不消费其配置（宿主配置内容属不可信面，不放给沙盒内写）。
  目录落在每会话随机的 `privateTmpDir` 下：随 `privateTmpDir` 清理、
  无跨会话状态——固定共享目录会让沙盒内 claude 写的
  settings/hooks/mcpServers 被下一次会话原样继承（跨会话投毒通道），
  与 pig-agent/ctxmode/XDG_RUNTIME_DIR 全落 `privateTmpDir` 的每会话
  隔离口径一致。

### Changed

- **软链目标身份校验 WARN 分档**：目标被拒时 stderr WARN 按拒绝原因
  分档——禁名命中打 `forbidden exec name, skipped`、缺 `/<查找名>/`
  路径段打 `missing /<name>/ path segment, skipped`、不在安装根前缀
  保留 `not in allowed roots, skipped`，不再把三种原因混在同一文案
  下误导排障。判定逻辑本身不变（`entryAgentTargetAllowed` 重构为
  返回原因串的 `entryAgentTargetRejectReason`）。

### Fixed

- **`ForbiddenExecNames` 补 ELF 本体名（宿主实测纠偏）**：新增
  `dispatch-agent-go`、`herdr`、`herdr-core`、`herdr-mobile-bridge`——
  顶层 `IsForbiddenExec` 与 seccomp `shouldDenyExecMmap` 都按 resolved
  basename 精确匹配判定，名单不含本体名会让本体二进制穿透三层拦截
  （`Policy.Validate`/`BuildExecWhitelist` 过滤、Landlock Exec 白名单、
  seccomp PROT_EXEC 守卫）；禁名+`-` 前缀派生拦阻只在软链目标身份
  校验层生效，故 herdr 三形态（`/root/.local/bin/herdr`、
  `herdr-core`、`herdr-mobile-bridge`，均为宿主实测静态 ELF）逐个
  精确列入。早期推断名 `herdr-go` 宿主不存在，不入名单。上述成员
  均不在 `ResolveEntryAgentPaths` 白名单名（agy/pig/pi/claude/
  agentapi/webm_encoder）内，不冲突。既有成员语义只增不删。

### Security

- **软链目标身份校验**：`EvalSymlinks` 解析出的目标并入 `EntryAgents`
  前校验身份——basename 须命中白名单名（`agy`/`pig`/`pi`/
  `claude`/`agentapi`/`webm_encoder`），或路径含 `/<查找名>/` 段且位于
  已知安装根前缀之下；basename 命中禁名（含 `-` 前缀派生名如
  `dispatch-agent-go`）一律拒。目标不通过时整个候选（含软链自身）
  丢弃并打 `WARN`，封堵 PATH 前置毒软链（如 `claude → dispatch-agent`）
  把禁用二进制混入白名单；`/tmp/claude/evil` 这类路径段巧合同样被拒。
  版本化安装（`claude → ~/.local/share/claude/versions/<ver>`，
  basename 为版本号但路径含 `/claude/` 段且在安装根内）正常放行。
- **`CLAUDE_CONFIG_DIR` 回钉**：`buildPolicy` 在
  `mergeEnv(policy.Env, cfg.Env)` 之后（与 PIG_HOME 回钉同位置）把最终
  env 中该键强制回写为 `ReviewerAgentWithEnv` 注入的
  `<privateTmpDir>/claude`（仅 reviewer-agent 预设），防
  `--env CLAUDE_CONFIG_DIR=<其他路径或空>` 把 CC 写面指回宿主或与
  `ReadWrite` 策略脱配。
- **已声明接受的嵌套面**：沙盒内 `claude` 可再 exec 白名单内
  `agy`/`pig`/`pi` 同路径（Landlock 路径级放行，无一次性 execve 计数）；
  `dispatch-agent`/`grok` 仍不在白名单，兜底依赖角色工具白名单。

## [0.3.2] - 2026-11-04

PATCH：加固 `reviewer-agent` 宿主态 Go 扩展 cell 预热（
`MaybeWarmGoExtensionCells`，`buildPolicy` 之前按需执行）的信任边界与
正确性：固定可信目录解析 pig 入口、`PIG_HOME` 与显式 AgentDir 校验并与
`--env` 完全对齐、单扩展/合包双缓存形态与 `cache/ext` 语言门、
真实 ELF 指纹追踪、全量常规文件与 `settings.json` 指纹、
stderr 失败标记拦截、预热前后指纹一致性有界重试、
flock 等待与子进程运行全链路信号中断进程组回收。

### Fixed

- **信任边界（pig 入口解析）**：`resolvePigEntryBinary` 彻底移除
  `exec.LookPath`，不再使用调用方 `os.Getenv("PATH")` 查找 `pig`；仅在固定
  可信目录（`<trustedHome>/.local/bin`、`<trustedHome>/.pig/bin`、
  `/root/.local/bin`、`/root/.pig/bin` 及 `standardBinDirs`）中解析，
  `EvalSymlinks` 后的真实文件必须落在可信前缀（上述目录或
  `<trustedHome>/.local/opt/pig`、`/root/.local/opt/pig`）内、是普通文件、
  `perm & 0o022 == 0` 且属主为 root 或当前 euid，任一不满足即拒绝预热。
  宿主 `HOME` 经 `os/user.LookupId(euid)` 获取（euid==0 兜底 `/root`），
  不读 `os.Getenv("HOME")`。
- **`PIG_HOME` 与 `--env` 完全对齐**：`MaybeWarmGoExtensionCells` 与新增
  `ReviewerAgentWithEnv`（`ReviewerAgent` 兼容签名保留，`buildPolicy`
  传入 `cfg.Env`）共用 `resolvePigHomeDir`：取 `--env` 最后一个
  `PIG_HOME` 键命中即停——非空取绝对路径；显式空串（如
  `--env PIG_HOME=A --env PIG_HOME=`）等同空覆盖，直接回退默认
  `<trustedHome>/.pig`，不回扫被覆盖条目也不读宿主环境变量；仅
  `--env` 完全不含该键时才读宿主 `PIG_HOME`。解析结果经
  `overrideEnvKey` 固定注入 `Policy.Env`，且 `buildPolicy` 在
  `mergeEnv(policy.Env, cfg.Env)` 之后再次回钉，防止空覆盖把空串写进
  最终 env 使沙盒内 pig 按外部 `HOME` 另算缓存根打穿对齐。预热前校验
  `PIG_HOME`：`Lstat` 拒绝符号链接；已存在时必须为目录、属主为当前
  euid（或 root）且 `perm & 0o022 == 0`（拒绝 `0777` 公共可写目录）。
- **显式 AgentDir 对齐**：新增 `resolveSourcePigAgentDir`，按与
  `injectPigCodingAgentDir` 完全一致的优先级（`PIG_CODING_AGENT_DIR`
  先查 `--env` 最后一项、空覆盖屏蔽宿主，再查宿主；有效
  `PIG_USE_PI_DIRS=="1"` 时同规则查 `PI_CODING_AGENT_DIR`）解析实际生效的
  宿主 AgentDir（缺省 `<PIG_HOME>/agent`）。自定义目录在预热前做与
  `PIG_HOME` 同等的软链/目录/属主/权限校验（不通过仅告警跳过预热）；
  预热指纹枚举、私有 AgentDir 副本（`preparePrivatePigAgentDirFromAgent`）
  与策略构造统一以其为源。
- **私有 AgentDir 同构**：预热子进程经 `preparePrivatePigAgentDirFromAgent`
  生成 `<sbx-warm-*>/pig-agent` 私有副本并注入 `PIG_CODING_AGENT_DIR` /
  `PI_CODING_AGENT_DIR`，不再直接指向宿主 AgentDir，与沙盒内扩展
  发现视图 100% 同构。
- **双缓存形态、语言门与锁位置**：单扩展编译产物 `cache/ext/<name>-<digest>/`
  与多扩展合包 `cache/cells/go/<digest>/runner` 同时纳入有效产物计数
  （`countValidExecutableGoArtifacts`，不扫无关语言 `cells/python` 等）；
  `cache/ext` 要求 `ready.json` 显式 `language:"go"`（`cellReadyMeta`
  新增 `Language` 字段，宿主 `node-runtime-*` 等非 Go 产物不计入），
  `cells/go` 允许 `language` 缺省或 `"go"`；预热互斥锁与指纹墓碑移至
  `<pigHome>/cache/.locks/`（`pigHome`、`cache`、`cache/.locks` 逐级
  `Lstat` 拒绝软链，锁文件 `O_NOFOLLOW`，墓碑不在沙盒 `ReadWrite` 中，
  沙盒内保持只读）。
- **空 `state/pigsdk` 冷启动**：删除 `len(sdkDirs)==0` 提前返回；只要存在
  Go 扩展即进入预热，由真实 `pig --help` 自动落盘内嵌 `state/pigsdk/sdk-*`。
- **真实 ELF 指纹追踪**：指纹除 `pig` 包装脚本外同步纳入
  `<pigHome>/bin/pi`、`<trustedHome>/.pig/bin/pi`、`/root/.pig/bin/pi`、
  `<trustedHome>/.local/opt/pig/official/pig`、
  `/root/.local/opt/pig/official/pig`（存在则 `EvalSymlinks` + `Stat`）
  的真实路径、`Size` 与纳秒 `ModTime`——包装脚本不变仅切换软链目标的
  升级同样失效重热。
- **全量常规文件与 `settings.json` 指纹**：扩展/SDK 目录文件清单移除
  `go.mod`/`go.sum`/`*.go` 白名单，改为采集全部普通文件（继续跳过
  `.git`/`testdata`/隐藏目录与软链等非常规条目）——`//go:embed`
  静态资产（`tools.json`、`locales/*.json` 等）变更同样失效重热；
  `<srcAgentDir>/settings.json` 内容 `sha256` 拼入指纹，且按其
  `"extensions"` 字段枚举显式本地路径扩展（与
  `injectHostExtensionsSetting`/`resolveHostExtensionEntry` 同口径，
  `null` 显式为空、支持排除默认 `extensions` 目录），配置变更立即使
  墓碑失效。
- **拦截假零退出**：流式跨块扫描子进程 stderr（重叠窗口不受 4KB 环形缓冲
  截断影响），命中 `Failed to load extension` / `extension build failed`
  即使进程退 0 也判定预热失败、告警降级且绝不写入 `sbx-warm.sha256`。
  `stderrScanWriter` 加 `sync.Mutex` 保护并经 `Snapshot` 读取：
  `cmd.WaitDelay` 到期后后台管道拷贝协程仍在 `Write` 时不再产生数据竞争。
- **预热前后指纹一致性与有界重试**：`flock` 内最多 2 轮；每轮运行前枚举
  `preStamp`，成功且有效产物 >0 后重枚举 `postStamp`，仅一致才原子写墓碑；
  预热期间源码漂移（如冷启动落盘 SDK、源码 A->B 切换）触发第 1 轮重验，
  仍漂移则告警跳过写戳，绝不把未构建的新指纹标为热态。
- **信号中断进程组回收**：预热期间注册 `SIGINT`/`SIGTERM`/`SIGHUP`/
  `SIGQUIT`（函数退出 `signal.Stop`）；`flock` 等待改为 `LOCK_NB` 轮询
  + `sigCtx.Done()` select，等锁期间收到信号同样立即退栈；捕获信号时取消
  子进程 `ctx` 触发进程组 `SIGKILL`，正常退栈跑完 `defer`
  （`/tmp/sbx-warm-*` 零残留、释放锁），以 `128+信号` 返回 `runMain`
  直接退出、不再进入沙盒。

## [0.3.1] - 2026-11-03

PATCH：修复 `sbx` 经 `syscall.Exec` 替换自身后无法清理预设临时目录的泄漏——每次
`--preset reviewer` / `reviewer-agent` 成功路径都会残留 `/tmp/pi-reviewer-*`（含
`auth.json` 凭证副本）与 `/tmp/pi-reviewer-exec-*`。

### Fixed

- 父进程不再直接 `syscall.Exec` 目标命令；改为以内部环境变量
  `PI_SANDBOX_INTERNAL_CHILD_SPEC` 派生子进程，由子进程施加 Landlock 后再
  `Exec` 目标。父进程保持在沙盒外，转发 `SIGINT`/`SIGTERM`/`SIGHUP`/`SIGQUIT`，
  等待子进程退出后执行预设临时目录清理，再按子进程退出码退出（被信号杀则
  `128+信号`）。不 `Setpgid`，继承 stdio。
- 补强清理路径：综合 `Policy.Env` / `ReadWrite` / `Exec` 识别
  `pi-reviewer-*` 与 `pi-reviewer-exec-*`，对只读子目录自动补权限并短重试。
- 新增集成测试覆盖正常退出、非零退出码、信号转发与 `reviewer-agent`
  凭证副本清理。

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

[Unreleased]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.4.0...HEAD
[0.4.0]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.3.3...go/sandbox/v0.4.0
[0.3.3]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.3.2...go/sandbox/v0.3.3
[0.3.2]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.3.1...go/sandbox/v0.3.2
[0.3.1]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.3.0...go/sandbox/v0.3.1
[0.3.0]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.2.2...go/sandbox/v0.3.0
[0.2.2]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.2.1...go/sandbox/v0.2.2
[0.2.1]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.2.0...go/sandbox/v0.2.1
[0.2.0]: https://github.com/dorokuma/pi-extensions/compare/go/sandbox/v0.1.0...go/sandbox/v0.2.0
[0.1.0]: https://github.com/dorokuma/pi-extensions/releases/tag/go/sandbox/v0.1.0
