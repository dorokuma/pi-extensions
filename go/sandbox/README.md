# `go/sandbox` — 基于 Linux Landlock 的无特权进程沙盒

`go/sandbox` 提供零 `cgo`、仅依赖 `golang.org/x/sys/unix` 的 Linux Landlock 文件系统与网络沙盒封装，以及命令行工具 `cmd/sbx`，用于约束子代理（如只读代码审查员 `ReadOnlyReviewer`）的文件读写与程序执行边界。

## 环境要求

- **操作系统**：启用了 Landlock LSM 的 Linux 内核（`Linux >= 5.13`，自适应支持 ABI v1 至 v5+）。
- **构建方式**：纯 Go（`CGO_ENABLED=0`），不依赖 `libcap` 或任何 C 库。
- **权限模型**：基于 `prctl(PR_SET_NO_NEW_PRIVS, 1)`、`landlock_restrict_self` 与 `seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_NEW_LISTENER)`，无需 `root` 或 `CAP_SYS_ADMIN` 权限。

## 核心接口与预设

- `sandbox.Supported() (abi int, ok bool)`：探测当前运行内核是否支持 Landlock 并返回最高 ABI 版本。
- `sandbox.Apply(p Policy) error`：将沙盒策略不可逆地施加到当前进程（及后续派生的所有子进程），并注入 `Policy.Env` 环境变量。当 `Policy.Exec` 非空时，默认强制安装 `seccomp` `PROT_EXEC` 守卫；若非 `BestEffort` 模式下 `seccomp` 守卫安装失败，直接返回错误（`ErrSeccomp`）、不执行目标命令；仅在 `BestEffort` 模式下输出告警并降级。
- `sandbox.ReadOnlyReviewer(workdir string) Policy`：构造"只读审查员"预设策略：
  - 根文件系统 `/` 只读；
  - 仅允许 `<workdir>/reports` 与会话级私有临时目录 `/tmp/pi-reviewer-*`（含 `TMPDIR`、`GOTMPDIR`、`GOCACHE`）可写；
  - **严格执行 `W^X`（可写与可执行完全分离）**：所有 `ReadWrite` 目录（`<workdir>/reports`、`TMPDIR`、`GOTMPDIR`、`GOCACHE`）均**不**授予 `LANDLOCK_ACCESS_FS_EXECUTE` 权限；
  - 建立独立的只读可执行目录 `/tmp/pi-reviewer-exec-*`，通过受控的 `go` 包装器与前置构建代理支持 `go build`、`go vet`、`go test`；
  - 严格限制可执行二进制白名单，显式拦截嵌套代理与调度器（`pig`、`pi`、`agy`、`grok`、`dispatch-agent`）以及直接调用 ELF 动态链接器（`ld-linux*.so*`）。
- `sandbox.ReviewerAgent(workdir string) Policy`：在 `ReadOnlyReviewer` 之上允许**顶层**启动 `agy`/`pig`/`pi`，并对 pig 与 agy MCP 生态做三层安全修复：
  - **ctxmode/MCP 数据隔离**：点名放行 `ctxmode`/`codegraph-go`/`jev-mcp`/`jev-mcp-go`/`agy-recall`（含软链目标）；未显式指定 `CTXMODE_DB` 时注入 `<privateTmpDir>/ctxmode/context_mode.db`（0700）；宿主兜底仅精确放行 `~/.local/share/ctxmode` 与 `agy-recall` state，绝不放宽到 `~/.local/share` 或 `gemini-web`。
  - **pig AgentDir 私有隔离**：宿主 `/root/.pig/agent` 保持 100% 只读；进沙盒前把白名单配置文件 0600 拷入 `<privateTmpDir>/pig-agent`（`extensions` 软链指向只读宿主目录），并按需注入 `PIG_CODING_AGENT_DIR`。
  - **Cell 缓存 W^X**：宿主态预刷新 `<PIG_HOME>/cache` 下有效缓存单元 `usage.json`（沙盒内 `TouchUsage` 命中 <1h 纯只读短路）；仅 `.locks` 目录与 `cache/toolchains` 版本记录目录可写，仅已构建 `<digest>/runner` 与 `cache/ext` 可执行产物可执行，`ReadWrite`/`Exec` 无任何父子包含。

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

# 只读审查员代理（允许顶层 agy/pig，扩展会话 RW；嵌套再起同名代理 OS 层仍可能成功，见 CHANGELOG）
go run ./cmd/sbx --preset reviewer-agent --workdir /path/to/repo -- /root/.local/bin/agy --help
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
     - **阻止映射禁用二进制与可写目录中的文件**：若 `SYS_MMAP(PROT_EXEC)` 的目标 `fd` 指向 `ForbiddenExecNames`（`pig`、`pi`、`agy`、`grok`、`dispatch-agent`）或位于不可执行的 `ReadWrite` 目录（`TMPDIR`、`GOTMPDIR`、`GOCACHE`、`<workdir>/reports`）内，同样直接返回 `EACCES` 拒绝映射。这也封堵了将自编译 `.so` 写入可写临时目录后再通过 `LD_PRELOAD` 或 `dlopen` 加载的绕过手法。

## 残余风险与已知边界

1. **只读路径上已有的 `.so` 仍可经 `LD_PRELOAD` 注入**：当前 `seccomp` `PROT_EXEC` 守卫拦截的是「直接将 `ld-linux` 作为主程序启动」、「映射 `ForbiddenExecNames` 二进制」以及「从不可执行的 `ReadWrite` 目录（`TMPDIR`、`GOTMPDIR`、`GOCACHE`、`<workdir>/reports`）映射文件」。由于正常动态链接加载系统共享库同样通过 `mmap(..., PROT_EXEC, ...)` 完成，对于根文件系统 `/` 等只读路径上**原本已存在**的 `.so` 文件，若子代理对直接启动的动态链接程序设置 `LD_PRELOAD` 指向这些只读路径上已有的 `.so`（注：受控 `go` 构建代理内部已主动剥离 `LD_PRELOAD`、`LD_AUDIT` 与 `LD_LIBRARY_PATH`），当前沙盒不会拦截该只读 `.so` 的映射。
2. **白名单内的脚本解释器（`bash` / `sh` / `awk` / `sed`）**：`ReadOnlyReviewer` 白名单中包含 `bash`、`sh` 等工具，审查员仍可执行仓库或临时目录中的 Shell 脚本逻辑；但脚本中派生的任何外部二进制仍受 Landlock `Exec` 白名单与 `seccomp` `PROT_EXEC` 守卫双重约束，无法借此拉起 `agy`、`pi`、`pig`、`grok`、`dispatch-agent` 或其他非白名单 ELF。
3. **被审查仓库自身的测试代码与构建期代码**：运行 `go test` 时，被审查仓库中的 `*_test.go` 代码会在沙盒内执行。由于 `pi-test-runner` 在启动测试二进制前已叠加了审查员的 Landlock 规则集且继承了 `seccomp` `PROT_EXEC` 守卫，测试代码同样无法篡改仓库源码、无法向可执行目录写入文件、也无法执行禁用二进制。
4. **`/proc/<pid>/fd/<fd>` 路径检查的极窄 TOCTOU 窗口**：`seccomp_unotify` 通过读取 `/proc/<pid>/fd/<fd>` 校验被映射文件的路径。由于审查员对所有可执行目录均无写权限，且所有可写目录（`TMPDIR`、`GOTMPDIR`、`GOCACHE`、`reports`）均被整体禁止 `PROT_EXEC` 映射，审查员无法在可执行目录内通过硬链接或重命名制造竞争条件。
5. **默认未开启网络隔离**：`ReadOnlyReviewer` 默认 `DenyNet: false`；若需彻底阻断 TCP 网络连接，可将 `Policy.DenyNet` 设为 `true`（要求内核 Landlock ABI >= 4）或配合 `sbx --deny-net` 使用。
