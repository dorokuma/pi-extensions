# `go/sandbox` — 基于 Linux Landlock LSM 的无特权进程沙盒

`github.com/dorokuma/pi-extensions/go/sandbox` 为 `pi-extensions`（特别是现役主代理 `pig` 与 `go/herdsman` 派发的只读审查员、体检员子代理）提供**内核态（Linux Landlock LSM）**的文件系统只读约束、可执行文件白名单（防止子代理嵌套拉起 `pig`/`pi`/`agy`/`grok`/`dispatch-agent`）以及 TCP 网络隔离兜底。

本模块采用纯 Go 实现（零 `cgo`，仅依赖 `golang.org/x/sys/unix`），不依赖 `root` 权限或 `CAP_SYS_ADMIN`。

> **注意**：若仓库根目录存在 `go.work` 且尚未将 `./go/sandbox` 加入 `go.work`，在 `go/sandbox` 目录下执行构建、检查或测试时请带上 `GOWORK=off`（例如 `GOWORK=off go vet ./...`、`GOWORK=off go test -race ./...`）。

## 1. 功能特性

- **无特权内核强制**：通过 `PR_SET_NO_NEW_PRIVS` + `landlock_restrict_self` 在内核 VFS 与 Socket 层直接拒绝越权访问，一旦 `Apply` 生效即不可撤销，且自动继承给后续 `fork`/`exec` 的全部子孙进程。
- **W^X 文件系统隔离与按需编译执行**：`ReadWrite` 目录默认仅授予读写、创建、删除、截断权限，不授予执行权限（`LANDLOCK_ACCESS_FS_EXECUTE`）；在 `ReadOnlyReviewer` 预设下，仅对隔离子目录 `/tmp/pi-reviewer-*`（及其中的 `GOTMPDIR`）额外开放执行权限以支持 `go test` 运行编译出的测试二进制，而 `<workdir>/reports` 仍严格保持不可执行。
- **Go 工具链开箱即用**：`ReadOnlyReviewer` 自动在私有临时目录 `/tmp/pi-reviewer-*` 下创建 `go-tmp` 与 `go-cache`，并通过 `Policy.Env` 注入 `TMPDIR`、`GOTMPDIR`、`GOCACHE`（`Apply` 与 `sbx` 均自动应用），无需将全局 `/tmp` 或 `$HOME/.cache` 放成可写即可正常执行 `go build`、`go vet`、`go test`。
- **嵌套代理拦截**：全盘 `/` 只读，仅开放 `<workdir>/reports` 与 `/tmp/pi-reviewer-*` 私有临时目录可写，通过文件级 `Exec` 白名单仅允许 `bash`/`sh`/`git`/`go`/`rg`/`cat` 等基础工具，在策略校验层与内核 `execve` 层双重拦截 `pig`、`pi`、`agy`、`grok`、`dispatch-agent`。
- **平滑降级（`BestEffort`）**：支持在不支持 Landlock 或 ABI 版本较低的内核上按需降级并向 `Stderr`（或自定义 `WarnWriter`）告警。

## 2. 快速上手

### 2.1 库 API 调用

```go
package main

import (
	"fmt"
	"os"

	"[github.com/dorokuma/pi-extensions/go/sandbox](https://github.com/dorokuma/pi-extensions/go/sandbox)"
)

func main() {
	abi, ok := sandbox.Supported()
	if !ok {
		fmt.Fprintln(os.Stderr, "当前内核未启用 Landlock")
		os.Exit(1)
	}
	fmt.Printf("Landlock ABI v%d 已就绪\n", abi)

	// 使用只读审查员预设（自动创建 <workdir>/reports 与 /tmp/pi-reviewer-* 可写目录，
	// 并在 Apply 时自动设置 TMPDIR / GOTMPDIR / GOCACHE 环境变量）
	policy := sandbox.ReadOnlyReviewer("/path/to/repo")
	policy.DenyNet = true // ABI >= 4 时禁止所有 TCP bind/connect

	if err := sandbox.Apply(policy); err != nil {
		fmt.Fprintf(os.Stderr, "应用沙盒失败: %v\n", err)
		os.Exit(1)
	}

	// 此后当前进程及所有子进程均处于只读沙盒内，且可直接运行 go vet / go test
}
```

### 2.2 命令行包装工具 `sbx`

`go/sandbox/cmd/sbx` 可用于把只读子代理命令包一层启动，先执行 `sandbox.Apply`（含 `Policy.Env` 注入）再调用 `syscall.Exec` 原位替换为目标命令：

```bash
# 编译 sbx（若本模块不在 go.work 中，需指定 GOWORK=off）
GOWORK=off go build -o /tmp/sbx ./cmd/sbx

# 方式一：显式指定只读、可写、可执行白名单与禁网
/tmp/sbx \
  --ro / \
  --rw /tmp/work /path/to/repo/reports \
  --exec /bin/sh /usr/bin/git /usr/bin/rg /usr/bin/cat \
  --deny-net \
  -- /usr/bin/git status

# 方式二：使用只读审查员预设运行 go vet / go test
GOWORK=off /tmp/sbx --reviewer . -- /usr/bin/bash -c "GOWORK=off go vet ./... && GOWORK=off go test ./..."
```

## 3. 内核与 Landlock ABI 要求对照表

| Landlock ABI | 最低 Linux 内核版本 | 本包启用能力 | 说明 |
| :--- | :--- | :--- | :--- |
| **ABI v1** | Linux 5.13+ | 基础文件读写、目录遍历、文件/目录创建与删除、`execve` 执行控制 | `Supported()` 返回 `abi >= 1, ok = true` |
| **ABI v2** | Linux 5.19+ | 额外管控跨目录重命名与硬链接（`LANDLOCK_ACCESS_FS_REFER`） | 自动纳入 `HandledAccessFS` 与 `ReadWrite` |
| **ABI v3** | Linux 6.2+ | 额外管控文件截断（`LANDLOCK_ACCESS_FS_TRUNCATE`，含 `O_TRUNC` / `truncate`） | 自动纳入 `HandledAccessFS` 与 `ReadWrite` |
| **ABI v4** | Linux 6.7+ | 管控经典 TCP 端口 `bind(2)` 与 `connect(2)` | `Policy.DenyNet = true` 要求 **ABI >= 4** |
| **ABI v5+** | Linux 6.10+（含 eqi12 的 6.12.100） | 额外管控字符/块设备 `ioctl(2)`（`LANDLOCK_ACCESS_FS_IOCTL_DEV`） | 自动纳入 `HandledAccessFS` 与 `ReadWrite` |

- 当内核不支持 Landlock 时：
  - 默认（`BestEffort: false`）：`Apply` 返回 `sandbox.ErrUnsupported`。
  - `BestEffort: true`：向 `WarnWriter`（默认 `os.Stderr`）输出告警并降级为空操作返回 `nil`。
- 当设置了 `DenyNet: true` 但内核 `ABI < 4` 时：
  - 默认（`BestEffort: false`）：`Apply` 返回 `sandbox.ErrNetUnsupported`。
  - `BestEffort: true`：输出告警，仅强制生效文件系统规则，跳过 TCP 限制。

## 4. 已知限制与安全边界

1. **已打开的文件描述符（Inherited File Descriptors）不受限**
   Landlock 在 `open(2)` / `execve(2)` / `bind(2)` / `connect(2)` 等系统调用创建或绑定新句柄时做路径/端口鉴权。**在调用 `Apply` 之前已经打开的文件描述符（fd）不受后续 Landlock 规则限制**。若父进程持有敏感可写文件的打开句柄，应在派生子进程前设置 `O_CLOEXEC` 或显式关闭。
2. **部分文件系统元数据系统调用不受 Landlock 管控**
   根据 Linux 内核文档，`chdir(2)`、`stat(2)`、`flock(2)`、`chmod(2)`、`chown(2)`、`setxattr(2)`、`utime(2)`、`fcntl(2)`、`access(2)` 目前不在 Landlock 文件路径权限位拦截范围内。
3. **`ioctl(2)` 的限制范围**
   - 在 ABI v1–v4 上，Landlock 不限制 `ioctl(2)`。
   - 在 ABI v5+（如 Linux 6.12）上，`LANDLOCK_ACCESS_FS_IOCTL_DEV` 仅限制对新打开的**字符设备或块设备**调用驱动实现的 `ioctl`，对普通文件或 `FIOCLEX`/`FIONBIO` 等通用描述符命令仍放行。
4. **网络隔离（`DenyNet`）仅覆盖经典 TCP（`IPPROTO_TCP`）**
   - `DenyNet` 在 ABI v4–v9 上仅拦截 TCP 的 `bind(2)` 与 `connect(2)`，**不拦截 UDP、ICMP、Raw Socket 或 Unix Domain Socket**。
   - 受 Linux 内核 Landlock Erratum 1 影响，内核 TCP 规则仅匹配经典 `IPPROTO_TCP` 套接字，不拦截 Multipath TCP（`IPPROTO_MPTCP`）。Go 1.24+ 的 `net.Listen` 默认启用 MPTCP（`net.Dial` 仍默认使用经典 TCP），如需在进程内一并封堵监听或 UDP/原始套接字，建议结合 network namespace 或 seccomp-bpf。
5. **动态链接器（`ld-linux`）、`GOTMPDIR` 可执行权限与解释器边界**
   - 当 `Policy.Exec` 非空时，为使动态链接的白名单工具（如 `git`、`rg`、`bash`）通过内核 `open_exec` 装载解释器，本包会自动对系统 ELF 动态链接器（如 `/lib64/ld-linux-x86-64.so.2`）授予文件级 `EXECUTE | READ_FILE` 权限，并对 `/lib`、`/usr/lib` 等系统共享库目录授予只读权限。
   - 在 `ReadOnlyReviewer` 预设下，为支持 `go test` 执行临时编译出的 `*.test` 二进制，私有临时目录 `/tmp/pi-reviewer-*` 同时具备读写与执行权限（`<workdir>/reports` 仍无执行权限）。Landlock 在内核层直接阻断对 `/root/.pig/bin/pig` 等非白名单路径的 `execve`，但若子代理拥有 `bash`/`sh` 或 `go` 编译器权限，理论上可将可读二进制拷贝至私有临时目录或通过 `/lib64/ld-linux-x86-64.so.2` 映射加载。若需进一步防拷贝绕过，建议将受禁二进制所在私有目录（如 `/root/.pig`）从只读挂载视图隔离，并与 `ctxmode` 命令参数拦截配合构成纵深防御。
6. **Ruleset 叠加层数上限（16 层）**
   Linux 内核限制单个线程最多叠加 16 层 Landlock ruleset，超出时 `landlock_restrict_self` 返回 `E2BIG`。
7. **cgo / `go test -race` 下的多线程同步机制**
   在纯 Go 构建（`CGO_ENABLED=0`）下，`Apply` 通过 `syscall.AllThreadsSyscall` 将 Landlock 规则同步应用到进程内所有 OS 线程；当启用 `cgo`（例如执行 `go test -race`）时，Go 运行时的 `AllThreadsSyscall` 会返回 `ENOTSUP`，此时本包自动退避为 `runtime.LockOSThread()` 锁定当前调用协程所在的 OS 线程并应用规则（若内核支持 ABI v8+ `LANDLOCK_RESTRICT_SELF_TSYNC` 则优先使用内核全线程同步）。因此建议在子代理包装器（如 `cmd/sbx`）入口早期或紧邻 `exec` 前调用 `Apply`。
