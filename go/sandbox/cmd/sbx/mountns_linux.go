//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// 沙盒 socket 白名单视图与进程面隔离（仅 Linux，需 CLONE_NEWNS|CLONE_NEWPID）：
// sbx 外层父进程以 Cloneflags=CLONE_NEWNS|CLONE_NEWPID 派生受限子进程，
// 子进程直接作为新 PID 命名空间的 PID 1 启动，在进入 Landlock 之前
// （runChildFromEnv → isolateRuntimeSockets → Apply）把宿主暴露的 IPC
// socket 面与进程面收成白名单视图：
//   - 挂载新 PID 命名空间专属的独立 procfs（MS_NOSUID|MS_NODEV|MS_NOEXEC），
//     沙盒内 /proc 只反映本沙盒的进程：宿主全部 PID 不可见，
//     /proc/<pid>/root|cwd|fd 魔链只指向沙盒自身的挂载命名空间（其中 /run
//     已被覆盖），不再穿透到宿主视图；挂载前硬校验 os.Getpid()==1，绝不在
//     共享 PID 命名空间内覆盖 procfs；
//   - 整个 /run 被空 tmpfs 覆盖，仅按原相对路径回绑白名单项
//     （/run/user/0/moshi-hook.sock——agy 钩子遥测；/etc/resolv.conf 经
//     EvalSymlinks 落在 /run/ 下的真实文件，保证 DNS stub 解析不断），
//     覆盖层最后 remount 为只读；
//   - /tmp/tmux-0 存在时被空 tmpfs 覆盖（tmux send-keys 注入面）；
//   - 覆盖路径不存在时跳过；任一步骤失败默认拒绝启动（fail-closed），
//     --best-effort 下只告警降级；
//   - 全部挂载与清理完成后安装 PR_SET_NO_NEW_PRIVS + seccomp-BPF
//     过滤器（SECCOMP_FILTER_FLAG_TSYNC 同步本进程全部线程，不含
//     TSYNC 时只锁调用线程，其余线程可经被拦 syscall 逃逸）：
//     mount/umount2/setns/unshare/pivot_root/open_tree/move_mount/
//     fsopen/fsconfig/fsmount/fspick/mount_setattr（挂载族）与
//     chroot/ptrace/process_vm_readv/process_vm_writev/pidfd_getfd/
//     kcmp（越狱第二跳与进程控制族）共 18 个系统调用一律返回 EPERM，
//     amd64 下另以 JGE 0x40000000 封堵 x32 ABI 位绕过；
//     pidfd_open 有意不封——Go 标准库管理沙盒内自身子进程会用它，
//     而在独立 PID ns 内宿主 PID 本就 ESRCH 不可达。
//
// PID 1 由 sbx 自身担任轻量 init：runAsSandboxInit 派生目标命令为 PID 2，
// 回收孤儿僵尸并向 PID 2 转发信号（业务进程若为 PID 1 会忽略默认未捕获
// 信号且无人收尸）。
//
// 已知边界（按设计保持可达）：/root/.herdsman/herdsman.sock（herdsman 扩展
// 必需）、/root/.config/herdr/herdr.sock（老 herdr agent 状态上报）、
// goExecDir 内 broker.sock 与 report.sock。

const (
	runDir           = "/run"
	tmuxSockDir      = "/tmp/tmux-0"
	moshiHookSockRel = "user/0/moshi-hook.sock"
	resolvConfPath   = "/etc/resolv.conf"
	sandboxStagePref = "pi-sbx-stage-"
)

// mount-seal seccomp 过滤器使用的 UAPI 常量（linux/seccomp.h / audit.h /
// filter.h），与 go/sandbox/landlock_linux.go 中的定义同源但独立（跨包）。
const (
	mountSealSetModeFilter uintptr = 1 // SECCOMP_SET_MODE_FILTER
	mountSealFlagTSync     uintptr = 1 // SECCOMP_FILTER_FLAG_TSYNC：同步全部线程的 NNP 与过滤器

	mountSealRetErrno       uint32 = 0x00050000 // SECCOMP_RET_ERRNO
	mountSealRetAllow       uint32 = 0x7fff0000 // SECCOMP_RET_ALLOW
	mountSealRetKillProcess uint32 = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	mountSealAuditArchX8664 uint32 = 0xc000003e // AUDIT_ARCH_X86_64
	mountSealAuditArchARM64 uint32 = 0xc00000b7 // AUDIT_ARCH_AARCH64
	mountSealX32SyscallBit  uint32 = 0x40000000 // __X32_SYSCALL_BIT：amd64 x32 ABI 调用号高位
	mountSealDataNrOffset   uint32 = 0          // seccomp_data.nr
	mountSealDataArchOffset uint32 = 4          // seccomp_data.arch
	mountSealBPFJmpMax             = 255        // BPF 单跳上限

	bpfLD  uint16 = 0x00 // BPF_LD
	bpfW   uint16 = 0x00 // BPF_W
	bpfABS uint16 = 0x20 // BPF_ABS
	bpfJMP uint16 = 0x05 // BPF_JMP
	bpfJEQ uint16 = 0x10 // BPF_JEQ
	bpfJGE uint16 = 0x30 // BPF_JGE
	bpfK   uint16 = 0x00 // BPF_K
	bpfRET uint16 = 0x06 // BPF_RET
)

// childSysProcAttr 返回派生受限子进程使用的 SysProcAttr：
// Cloneflags=CLONE_NEWNS|CLONE_NEWPID 在 clone 时同时创建独立挂载命名空间
// 与独立 PID 命名空间，子进程直接以新 PID ns 的 PID 1 启动（Go 1.26 实测
// cmd.Start() 子进程 os.Getpid()==1；不能用 Unshareflags 携带
// CLONE_NEWPID——unshare(2) 只把新 PID ns 记给未来的子进程，调用进程本身
// 仍留在宿主 PID ns，挂载的 procfs 依旧反映宿主进程面）。
// Go 标准库只对 Unshareflags 路径自动补一次
// mount(NULL, "/", NULL, MS_REC|MS_PRIVATE, NULL)（src/syscall/exec_linux.go，
// 修正 unshare 不处理 --shared 挂载的传播标记）；Cloneflags 派生路径没有该
// 补挂，/ 的私有化由 isolateRuntimeSockets 首步 private-propagation 硬前置
// 执行（失败即熔断全部覆盖挂载），保证子进程内的覆盖挂载不传播回宿主
// 挂载命名空间。
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID}
}

// isolateRuntimeSockets 在受限子进程内（进入 Landlock 之前）施加白名单视图
// 并在全部挂载与清理完成后安装 mount-seal seccomp 锁。
// 仅当父进程确实以 CLONE_NEWNS 派生（childExecSpec.MountNS=true）时才被调用；
// 任一覆盖/回绑/封锁步骤失败默认返回错误拒绝启动，bestEffort 时仅告警并继续
// （含挂载未全部成功时仍按同策略安装 seccomp 锁）。/ 的 rprivate 化是全部
// 覆盖挂载的硬前置：失败时 bestEffort 下也跳过全部覆盖挂载（仅仍尝试安装
// seccomp 锁），绝不在未私有化的 / 上执行 mount。
func isolateRuntimeSockets(bestEffort bool) error {
	step := func(name string, fn func() error) error {
		if err := fn(); err != nil {
			if bestEffort {
				fmt.Fprintf(os.Stderr, "sbx: WARN mount step %s failed, socket hiding degraded: %v\n", name, err)
				return nil
			}
			return fmt.Errorf("mount step %s failed: %w", name, err)
		}
		return nil
	}

	// / 的 rprivate 化是全部覆盖挂载的硬前置：Go 标准库只对 Unshareflags
	// 路径自动补 MS_REC|MS_PRIVATE，Cloneflags 派生路径不会执行，必须在此
	// 显式完成。若失败且 / 仍为 shared 传播，任何后续覆盖挂载都会泄漏回
	// 宿主挂载命名空间，因此 bestEffort 下也只安装 mount-seal 锁（非挂载
	// 操作）后直接返回，绝不在未私有化的 / 上执行 mount。
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		if bestEffort {
			fmt.Fprintf(os.Stderr, "sbx: WARN mount step private-propagation failed, skipping all cover mounts: %v\n", err)
			return step("seal-mount-namespace", sealMountNamespace)
		}
		return fmt.Errorf("mount step private-propagation failed: %w", err)
	}
	// 独立 procfs 必须在 private-propagation 之后、seal 之前挂载：此后沙盒内
	// /proc 只反映新 PID 命名空间，宿主 PID 与其 /proc/<pid>/root|cwd|fd
	// 魔链彻底不可见。
	if err := step("mount-proc", mountIsolatedProc); err != nil {
		return err
	}
	if err := step("cover-tmux-sockets", coverTmuxSockets); err != nil {
		return err
	}
	if err := step("cover-runtime-dir", coverRuntimeDir); err != nil {
		return err
	}
	// 挂载命名空间篡改封锁：必须在全部覆盖/回绑/暂存清理完成后安装，
	// 因为 seccomp 锁生效后自身也不能再执行 mount/umount2。
	return step("seal-mount-namespace", sealMountNamespace)
}

// mountIsolatedProc 在新 PID 命名空间内挂载专属 procfs（本进程即该 ns 的
// PID 1）。硬校验 os.Getpid()==1：CLONE_NEWPID 经 Cloneflags 生效时子进程
// 直接以 PID 1 启动；若派生路径漂移导致进程仍留在共享 PID 命名空间，
// 挂载 procfs 只会把宿主进程视图（含 /proc/<hostPid>/root 魔链）原样带进
// 沙盒，必须拒绝继续而非掩耳盗铃。procfs 内容恒反映调用进程的 PID ns，
// 挂载后 /proc/1 即本进程自身、/proc/1/root 指向本沙盒已覆盖 /run 的
// 挂载命名空间视图。
func mountIsolatedProc() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("refusing to mount /proc outside isolated PID namespace (pid=%d)", os.Getpid())
	}
	return unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
}

// coverTmuxSockets 以空只读 tmpfs 覆盖 /tmp/tmux-0（存在时），切断沙盒内
// 进程向任意宿主终端注入按键的 tmux socket 通道。
func coverTmuxSockets() error {
	st, err := os.Stat(tmuxSockDir)
	if err != nil || !st.IsDir() {
		return nil // 覆盖路径不存在时跳过
	}
	return unix.Mount("tmpfs", tmuxSockDir, "tmpfs",
		unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0555")
}

// runPreserveItem 描述覆盖 /run 前需要保留并按原相对路径回绑的白名单项。
type runPreserveItem struct {
	rel      string // 相对 /run 的路径（也是覆盖层内重建挂载点的路径）
	host     string // 覆盖前宿主视角的绝对路径
	isSocket bool   // 回绑目标为 unix socket：connect() 需挂载点可写，不得补只读 remount
}

// collectRunPreserveItems 收集 /run 覆盖层的白名单回绑项：
//   - /run/user/0/moshi-hook.sock 存在且为 socket（agy 钩子遥测必需）；
//   - /etc/resolv.conf 经 EvalSymlinks 落在 /run/ 下且为普通文件（systemd-
//     resolved stub 场景，不回绑会让沙盒内 DNS 解析直接断链）。
func collectRunPreserveItems() []runPreserveItem {
	var items []runPreserveItem
	seen := make(map[string]struct{})

	moshiHost := filepath.Join(runDir, filepath.FromSlash(moshiHookSockRel))
	if s, err := os.Stat(moshiHost); err == nil && s.Mode()&os.ModeSocket != 0 {
		items = append(items, runPreserveItem{rel: moshiHookSockRel, host: moshiHost, isSocket: true})
		seen[moshiHookSockRel] = struct{}{}
	}
	if resolved, err := filepath.EvalSymlinks(resolvConfPath); err == nil && strings.HasPrefix(resolved, runDir+"/") {
		if s, err := os.Stat(resolved); err == nil && s.Mode().IsRegular() {
			rel := strings.TrimPrefix(resolved, runDir+"/")
			if _, dup := seen[rel]; !dup {
				items = append(items, runPreserveItem{rel: rel, host: resolved})
				seen[rel] = struct{}{}
			}
		}
	}
	return items
}

// coverRuntimeDir 以 tmpfs 覆盖整个 /run，隐藏 /run/systemd/private、
// /run/docker.sock、/run/containerd/containerd.sock、/run/dbus/system_bus_socket、
// /run/user/0/herdr-core/* 等全部宿主全权 socket 面；随后只把白名单项
// （moshi-hook.sock、/run 下的 resolv.conf 目标）经 /tmp 暂存回绑还原路径
// （文件回绑补只读 remount；socket 回绑跳过只读化——connect() 要求挂载点
// 可写，MS_RDONLY remount 会让 unix connect 返回 EROFS），最后把覆盖层
// remount 为只读（MS_REMOUNT 只作用于覆盖层自身，不影响其上的回绑挂载）。
// 覆盖路径不存在时跳过。
func coverRuntimeDir() error {
	st, err := os.Stat(runDir)
	if err != nil || !st.IsDir() {
		return nil
	}

	items := collectRunPreserveItems()
	if len(items) == 0 {
		// 无必需回绑项：直接以只读空 tmpfs 覆盖。
		return unix.Mount("tmpfs", runDir, "tmpfs",
			unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755")
	}

	// 白名单项需要保持可达：覆盖前先逐个 bind 到 /tmp 下私有暂存点（bind
	// 挂载在覆盖后仍引用原文件/socket），再以可写 tmpfs 覆盖、重建挂载点
	// 目录与空文件并回绑。
	stage, err := os.MkdirTemp("/tmp", sandboxStagePref)
	if err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}
	defer func() {
		for i := range items {
			_ = unix.Unmount(filepath.Join(stage, strconv.Itoa(i)), unix.MNT_DETACH)
		}
		_ = os.RemoveAll(stage)
	}()

	for i, it := range items {
		sp := filepath.Join(stage, strconv.Itoa(i))
		if f, err := os.OpenFile(sp, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
			return fmt.Errorf("create stage mountpoint %s: %w", it.rel, err)
		} else {
			_ = f.Close()
		}
		if err := unix.Mount(it.host, sp, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("stage %s: %w", it.rel, err)
		}
	}

	if err := unix.Mount("tmpfs", runDir, "tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("cover %s: %w", runDir, err)
	}
	for i, it := range items {
		dst := filepath.Join(runDir, filepath.FromSlash(it.rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create %s mountpoint dir: %w", it.rel, err)
		}
		if f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
			return fmt.Errorf("create %s mountpoint: %w", it.rel, err)
		} else {
			_ = f.Close()
		}
		sp := filepath.Join(stage, strconv.Itoa(i))
		if err := unix.Mount(sp, dst, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("rebind %s: %w", it.rel, err)
		}
		// 仅普通文件回绑补只读 remount：bind 挂载自身的只读化不影响经其打开的
		// 文件读，只挡写路径；socket 回绑保持可写，否则 unix connect() 被
		// EROFS 拦截（moshi-hook.sock 必须在沙盒内可连接）。
		if !it.isSocket {
			if err := unix.Mount("", dst, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("remount %s read-only: %w", it.rel, err)
			}
		}
	}
	if err := unix.Mount("tmpfs", runDir, "tmpfs",
		unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("remount %s read-only: %w", runDir, err)
	}
	return nil
}

// mountSealBlockedSyscalls 返回当前架构下被封堵的系统调用号集合：
// 12 个挂载命名空间相关系统调用 + 6 个越狱/进程控制调用（chroot 是
// /proc/<pid>/root 魔链的越狱第二跳；ptrace/process_vm_*/pidfd_getfd/kcmp
// 是共享 PID ns 残余面下的进程控制族，独立 PID ns 已使宿主 PID 不可达，
// 此处纵深封锁本 ns 内滥用）。非 amd64/arm64 返回 nil（该架构不安装过滤器）。
// pidfd_open 刻意不在名单内：Go 标准库用它管理本 ns 内子进程，而宿主 PID
// 在新 PID ns 下本就 ESRCH 不可达。
func mountSealBlockedSyscalls() []uintptr {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return []uintptr{
			unix.SYS_MOUNT,
			unix.SYS_UMOUNT2,
			unix.SYS_SETNS,
			unix.SYS_UNSHARE,
			unix.SYS_PIVOT_ROOT,
			unix.SYS_OPEN_TREE,
			unix.SYS_MOVE_MOUNT,
			unix.SYS_FSOPEN,
			unix.SYS_FSCONFIG,
			unix.SYS_FSMOUNT,
			unix.SYS_FSPICK,
			unix.SYS_MOUNT_SETATTR,
			unix.SYS_CHROOT,
			unix.SYS_PTRACE,
			unix.SYS_PROCESS_VM_READV,
			unix.SYS_PROCESS_VM_WRITEV,
			unix.SYS_PIDFD_GETFD,
			unix.SYS_KCMP,
		}
	default:
		return nil
	}
}

// mountSealAuditArch 返回当前架构的 AUDIT_ARCH_* 值；不支持的架构返回 0。
func mountSealAuditArch() uint32 {
	switch runtime.GOARCH {
	case "amd64":
		return mountSealAuditArchX8664
	case "arm64":
		return mountSealAuditArchARM64
	default:
		return 0
	}
}

// sealMountNamespace 安装 PR_SET_NO_NEW_PRIVS 与挂载族/进程控制族系统调用的
// seccomp-BPF 拦截（返回 EPERM），封堵沙盒内 root 经 setns 跳回宿主挂载
// 命名空间、经 umount2 撤销 socket 白名单视图、经 chroot 利用魔链做越狱
// 第二跳、或经 ptrace/process_vm_*/pidfd_getfd/kcmp 操纵其它进程。
//
// 过滤器语义：seccomp_data.arch 与当前 AUDIT_ARCH 不符时直接
// SECCOMP_RET_KILL_PROCESS（异构 ABI 绝不放行任何系统调用）；amd64 下
// 另以 JGE __X32_SYSCALL_BIT 封堵 x32 ABI（x32 与原生 amd64 共享
// AUDIT_ARCH，调用号带 0x40000000 高位，不拦截会绕过按原生号布防的
// JEQ 链）；当前架构下 18 个被封堵系统调用返回 ERRNO(EPERM)，其余全部
// ALLOW——不影响后续 Landlock + seccomp user-notif 过滤器叠加安装与白名单
// 命令 execve（过滤器与 NO_NEW_PRIVS 均跨 execve 保留，由 exec 目标进程
// 继承）。
//
// 仅支持 linux/amd64 与 linux/arm64；其它架构返回 nil 跳过（与
// --best-effort 无关，属不支持平台的空操作，不新增回归）。
//
// 必须 LockOSThread：调用线程须保证随后 syscall.Exec 的就是它（execve
// 保留线程的 NNP 与过滤器栈）；seccomp 系统调用带
// SECCOMP_FILTER_FLAG_TSYNC，把 NO_NEW_PRIVS 与本过滤器同步安装到进程
// 内全部现存线程（子进程此刻只有 Go runtime 线程，无外来线程竞争），
// 杜绝未过滤线程在 execve 前经挂载族 syscall 逃逸的窗口。
// 锁定不解开是有意的：本函数返回后子进程只会走到 Apply 与 exec 或退出。
func sealMountNamespace() error {
	auditArch := mountSealAuditArch()
	blocked := mountSealBlockedSyscalls()
	if auditArch == 0 || len(blocked) == 0 {
		return nil
	}
	if len(blocked)+8 > mountSealBPFJmpMax {
		return fmt.Errorf("mount-seal filter too large: %d insns", len(blocked)+8)
	}

	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}

	// 指令布局（prefix=3，amd64 因 x32 指令为 4）：
	//   0: 加载 seccomp_data.arch
	//   1: 若非当前 AUDIT_ARCH → KILL_PROCESS（idxKill）
	//   2: 加载 seccomp_data.nr
	//   [3]（仅 amd64）: 若 nr >= __X32_SYSCALL_BIT → ERRNO(EPERM)（idxErrno），
	//                    封堵 x32 ABI 绕过（x32 共享 AUDIT_ARCH_X86_64，
	//                    调用号带 0x40000000 高位）
	//   prefix..prefix+N-1: 逐一 JEQ 被封堵系统调用号 → ERRNO(EPERM)；
	//              未命中继续下一条，全部未命中顺序落到 ALLOW（idxAllow）
	//   idxAllow = prefix+N:  RET ALLOW
	//   idxErrno = prefix+N+1: RET ERRNO|EPERM
	//   idxKill  = prefix+N+2: RET KILL_PROCESS
	n := len(blocked)
	prefix := 3
	x32Guard := runtime.GOARCH == "amd64"
	if x32Guard {
		prefix = 4
	}
	idxAllow := prefix + n
	idxErrno := idxAllow + 1
	idxKill := idxAllow + 2

	filter := make([]unix.SockFilter, 0, idxKill+1)
	filter = append(filter,
		unix.SockFilter{Code: bpfLD | bpfW | bpfABS, K: mountSealDataArchOffset},
		unix.SockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: uint8(idxKill - 2), K: auditArch},
		unix.SockFilter{Code: bpfLD | bpfW | bpfABS, K: mountSealDataNrOffset},
	)
	if x32Guard {
		filter = append(filter, unix.SockFilter{
			Code: bpfJMP | bpfJGE | bpfK,
			Jt:   uint8(idxErrno - 4),
			Jf:   0,
			K:    mountSealX32SyscallBit,
		})
	}
	for i, nr := range blocked {
		filter = append(filter, unix.SockFilter{
			Code: bpfJMP | bpfJEQ | bpfK,
			Jt:   uint8(idxErrno - (prefix + i) - 1),
			Jf:   0,
			K:    uint32(nr),
		})
	}
	filter = append(filter,
		unix.SockFilter{Code: bpfRET | bpfK, K: mountSealRetAllow},
		unix.SockFilter{Code: bpfRET | bpfK, K: mountSealRetErrno | uint32(unix.EPERM)},
		unix.SockFilter{Code: bpfRET | bpfK, K: mountSealRetKillProcess},
	)

	prog := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}
	_, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		mountSealSetModeFilter,
		mountSealFlagTSync,
		uintptr(unsafe.Pointer(&prog)),
	)
	runtime.KeepAlive(&filter)
	runtime.KeepAlive(&prog)
	if errno != 0 {
		runtime.UnlockOSThread()
		return fmt.Errorf("seccomp(SECCOMP_SET_MODE_FILTER): %w", errno)
	}
	return nil
}
