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

// 沙盒 socket 白名单视图、进程面与 SysV IPC 隔离（仅 Linux，需
// CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC）：sbx 外层父进程以
// Cloneflags=CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC 派生受限子进程，
// 子进程直接作为新 PID 命名空间的 PID 1 启动，并同时获得独立 IPC
// 命名空间（隔离 SysV shm/sem/msg 与按名 mq_open/mq_unlink——沙盒
// 内读不到宿主共享内存段且按名打开宿主 POSIX 队列返回 ENOENT，沙盒
// 内创建的 IPC 对象随命名空间销毁不留宿主持久残留；未覆盖继承的
// /dev/mqueue 挂载视图，若宿主预存可读队列节点仍可经路径 O_RDONLY
// 打开并 mq_timedreceive 取走消息，写/建由 Landlock 阻断）；在进入 Landlock 之前
// （runChildFromEnv → isolateRuntimeSockets → Apply）把宿主暴露的 IPC
// socket 面与进程面收成白名单视图：
//   - 动手前先硬校验 os.Getpid()==1：进程未在独立 PID 命名空间内时绝不
//     先改宿主 / 传播属性再报错（非 bestEffort 直接拒绝启动；bestEffort
//     下跳过全部覆盖挂载，仅执行 drop-capabilities 与 seal-mount-namespace）；
//   - 挂载新 PID 命名空间专属的独立 procfs（MS_NOSUID|MS_NODEV|MS_NOEXEC），
//     沙盒内 /proc 只反映本沙盒的进程：宿主全部 PID 不可见，
//     /proc/<pid>/root|cwd|fd 魔链只指向沙盒自身的挂载命名空间（其中 /run
//     已被覆盖），不再穿透到宿主视图；
//   - 整个 /run 被空 tmpfs 覆盖，仅按原相对路径回绑白名单项
//     （/run/user/0/moshi-hook.sock——agy 钩子遥测；/etc/resolv.conf 经
//     EvalSymlinks 落在 /run/ 下的真实文件，保证 DNS stub 解析不断），
//     覆盖层最后 remount 为只读；
//   - /tmp/tmux-0 存在时被空 tmpfs 覆盖（tmux send-keys 注入面）；
//   - 覆盖完成后回绑继承 cwd：os.Chdir 回原 cwd 并 os.Getwd() 复核，强制
//     内核从新挂载命名空间根重解析 fs->pwd，解除旧目录项对被覆盖前
//     底层 /run 或 /tmp/tmux-0 的引用；原 cwd 在新视图下不存在（如
//     /run/user/0 未被回绑的子目录）时 fail-closed 拒绝启动；
//   - Linux capabilities 根治裁剪：先 PR_CAPBSET_DROP 收缩 bounding set
//     （CAP_SETPCAP 留到最后一步 drop——内核要求调用时仍持该能力），
//     再 PR_CAP_AMBIENT_CLEAR_ALL 清空 ambient，最后 capset 把
//     Effective/Permitted 置为 0x22（仅 CAP_DAC_OVERRIDE|CAP_KILL：
//     前者供 uid=0 清理只读权限临时目录且受 Landlock 严格约束，后者供
//     独立 PID ns 内信号转发），Inheritable 置 0。此后沙盒内 root 不再
//     持有 CAP_SYS_ADMIN/SYS_BOOT/SYS_MODULE/BPF/PERFMON/NET_ADMIN/
//     SYS_TIME/SYS_RAWIO/SYS_PTRACE/SYS_CHROOT/DAC_READ_SEARCH 等任何
//     高权能力，capability 门控的宿主全局副作用面整体消失；
//   - 全部挂载与清理完成后安装 PR_SET_NO_NEW_PRIVS + seccomp-BPF
//     过滤器（SECCOMP_FILTER_FLAG_TSYNC 同步本进程全部线程，不含
//     TSYNC 时只锁调用线程，其余线程可经被拦 syscall 逃逸）：
//     mount/umount2/setns/unshare/pivot_root/open_tree/move_mount/
//     fsopen/fsconfig/fsmount/fspick/mount_setattr（挂载族）、
//     chroot/ptrace/process_vm_readv/process_vm_writev/pidfd_getfd/
//     kcmp（越狱第二跳与进程控制族）与 reboot/kexec*/init|finit|
//     delete_module/bpf/perf_event_open/fanotify_init/syslog/
//     sethostname/setdomainname/settimeofday/clock_settime/adjtimex/
//     clock_adjtime/swapon/swapoff/quotactl/quotactl_fd/acct/
//     open_by_handle_at/
//     name_to_handle_at/add_key/request_key/keyctl/pidfd_send_signal
//     （capability 门控的宿主全局副作用/句柄/密钥环族，纵深封锁）
//     共 45 个系统调用一律返回 EPERM（amd64 另有 ioperm/iopl 共 47 个）；
//     amd64 下另以 JGE 0x40000000 封堵 x32 ABI 位绕过；
//     pidfd_open 有意不封——Go 标准库管理沙盒内自身子进程会用它，
//     而在独立 PID ns 内宿主 PID 本就 ESRCH 不可达。
//
// PID 1 由 sbx 自身担任轻量 init：runAsSandboxInit 派生目标命令（其实际
// PID 不固定——sandbox.Apply 会先派生 helper/broker 辅助进程），回收孤儿
// 僵尸并向目标命令转发信号（业务进程若为 PID 1 会忽略默认未捕获信号且
// 无人收尸）。
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

	// amd64 专属系统调用号：x/sys 只为 amd64/386/ppc/mips 定义
	// SYS_IOPERM/SYS_IOPL，arm64 无这两个调用（也无此号）——直接内联
	// amd64 调用号避免引用架构专属常量导致 GOARCH=arm64 交叉编译失败。
	mountSealSysIopermAmd64 uintptr = 173 // SYS_IOPERM (amd64)
	mountSealSysIoplAmd64   uintptr = 172 // SYS_IOPL (amd64)

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
// Cloneflags=CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC 在 clone 时同时创建
// 独立挂载命名空间、独立 PID 命名空间与独立 IPC 命名空间——后者切断
// 沙盒读写宿主 SysV 共享内存/信号量/消息队列及按名
// mq_open/mq_unlink 的通道（继承的 /dev/mqueue 挂载视图未覆盖，见文件头
// 注释），
// 沙盒内新建的 IPC 对象随命名空间销毁、不残留宿主。子进程直接以新
// PID ns 的 PID 1 启动（Go 1.26 实测
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
	return &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC}
}

// isolateRuntimeSockets 在受限子进程内（进入 Landlock 之前）施加白名单视图、
// 裁剪 Linux capabilities 并在全部挂载与清理完成后安装 mount-seal seccomp 锁。
// 仅当父进程确实以 CLONE_NEWNS 派生（childExecSpec.MountNS=true）时才被调用；
// 任一覆盖/回绑/封锁步骤失败默认返回错误拒绝启动，bestEffort 时仅告警并继续。
// 两个熔断前置按固定顺序执行：
//   - PID-ns 硬校验位于一切宿主可见变异之前：os.Getpid()!=1 时绝不先改
//     宿主 / 传播属性再报错（非 bestEffort 直接返回错误；bestEffort 下跳过
//     全部覆盖挂载，仅执行 drop-capabilities 与 seal-mount-namespace）；
//   - / 的 rprivate 化是全部覆盖挂载的硬前置：失败时 bestEffort 下也跳过
//     全部覆盖挂载（仅执行 drop-capabilities 与 seal-mount-namespace），
//     绝不在未私有化的 / 上执行 mount。
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
	// skipAllCoverMounts 在非独立 PID ns 或私有传播失败的降级路径下跳过全部
	// 覆盖挂载，仅执行降权与封锁两类非挂载操作（二者在共享挂载命名空间内
	// 同样有效）。
	skipAllCoverMounts := func() error {
		if err := step("drop-capabilities", dropSandboxCapabilities); err != nil {
			return err
		}
		return step("seal-mount-namespace", sealMountNamespace)
	}

	// PID-ns 前置硬校验必须先于 unix.Mount("", "/", "", MS_REC|MS_PRIVATE, "")：
	// 进程仍留在共享 PID 命名空间（伪造 PI_SANDBOX_INTERNAL_CHILD_SPEC 或派生
	// 路径漂移）时，先私有化宿主 / 再报错属宿主可见的状态变异；且 procfs 恒
	// 反映调用进程的 PID ns，共享 ns 下挂载 procfs 会把宿主进程视图（含
	// /proc/<hostPid>/root 魔链）原样带进沙盒。bestEffort 下同样跳过全部
	// 覆盖挂载，仅降权 + 装锁。
	if os.Getpid() != 1 {
		if bestEffort {
			fmt.Fprintf(os.Stderr, "sbx: WARN not running as PID 1 in isolated PID namespace (pid=%d), skipping all cover mounts; /proc/<hostPid>/root magic-link remains reachable\n", os.Getpid())
			return skipAllCoverMounts()
		}
		return fmt.Errorf("refusing to isolate: not running as PID 1 in isolated PID namespace (pid=%d)", os.Getpid())
	}

	// 继承 cwd 必须在覆盖挂载完成前捕获：覆盖后内核 fs->pwd 仍引用旧目录
	// 项，但此处的目标是从新视图重解析同一个原 cwd 字符串，不是解析旧
	// 目录项自身。
	origCwd, cwdErr := os.Getwd()

	// / 的 rprivate 化是全部覆盖挂载的硬前置：Go 标准库只对 Unshareflags
	// 路径自动补 MS_REC|MS_PRIVATE，Cloneflags 派生路径不会执行，必须在此
	// 显式完成。若失败且 / 仍为 shared 传播，任何后续覆盖挂载都会泄漏回
	// 宿主挂载命名空间，因此 bestEffort 下也只降权 + 安装 mount-seal 锁
	//（非挂载操作）后直接返回，绝不在未私有化的 / 上执行 mount。
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		if bestEffort {
			fmt.Fprintf(os.Stderr, "sbx: WARN mount step private-propagation failed, skipping all cover mounts: %v\n", err)
			return skipAllCoverMounts()
		}
		return fmt.Errorf("mount step private-propagation failed: %w", err)
	}
	// 独立 procfs 必须在 private-propagation 之后、seal 之前挂载：此后沙盒内
	// /proc 只反映新 PID 命名空间，宿主 PID 与其 /proc/<pid>/root|cwd|fd
	// 魔链彻底不可见。bestEffort 下失败必须点名残余面：宿主 procfs 仍在，
	// /proc/<hostPid>/root 魔链可绕过全部覆盖层。
	if err := mountIsolatedProc(); err != nil {
		if bestEffort {
			fmt.Fprintf(os.Stderr, "sbx: WARN mount step mount-proc failed, socket hiding degraded: %v; /proc/<hostPid>/root magic-link remains reachable\n", err)
		} else {
			return fmt.Errorf("mount step mount-proc failed: %w", err)
		}
	}
	if err := step("cover-tmux-sockets", coverTmuxSockets); err != nil {
		return err
	}
	if err := step("cover-runtime-dir", coverRuntimeDir); err != nil {
		return err
	}
	// 全部覆盖完成后从新挂载命名空间视图重解析并建立继承 cwd：内核持有的
	// fs->pwd 仍引用覆盖前的底层目录项（tmpfs 覆盖只改变其后的绝对路径解析，
	// 不迁移已有 cwd），不重建则沙盒内相对路径 herdr-core/default.sock 可
	// 直拨覆盖前的宿主 socket。原 cwd 在新视图下不存在（如位于被覆盖且未
	// 回绑的 /run/user/0/herdr-core）时 fail-closed 拒绝启动，绝不擅自切往
	// --workdir 破坏正常目录的继承 cwd 契约。
	if err := step("rebind-cwd", func() error { return rebindInheritedCwd(origCwd, cwdErr) }); err != nil {
		return err
	}
	// capabilities 裁剪先于 seal：capset 置位后 PR_CAPBSET_DROP 会因
	// CAP_SETPCAP 已随 Effective 清空而 EPERM，故 bounding 收缩必须在
	// capset 之前完成（dropSandboxCapabilities 内部保证顺序）。
	if err := step("drop-capabilities", dropSandboxCapabilities); err != nil {
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

// rebindInheritedCwd 在覆盖挂载完成后从新挂载命名空间视图重解析继承
// cwd：内核 fs->pwd 仍指向覆盖前的底层目录项，os.Chdir(origCwd) 强制按
// 绝对路径从新视图重新查找（旧目录项引用随之释放），os.Getwd() 复核
// 新视图确实能解析该路径。原 cwd 在新视图不存在（如被覆盖且未回绑的
// /run/user/0/herdr-core）时返回错误，默认 fail-closed 拒绝启动。
func rebindInheritedCwd(origCwd string, cwdErr error) error {
	if cwdErr != nil {
		return fmt.Errorf("capture inherited cwd failed: %w", cwdErr)
	}
	if err := os.Chdir(origCwd); err != nil {
		return fmt.Errorf("rebind cwd %q under covered view failed: %w", origCwd, err)
	}
	if _, err := os.Getwd(); err != nil {
		return fmt.Errorf("verify rebound cwd %q failed: %w", origCwd, err)
	}
	return nil
}

// sandboxKeepCapMask 是沙盒内保留的最小双能力掩码（0x22）：
// CAP_DAC_OVERRIDE 供 uid=0 清理只读权限临时目录（受 Landlock 严格约束），
// CAP_KILL 供独立 PID ns 内向目标命令转发信号；其余能力一律剥离。
const sandboxKeepCapMask uint64 = (uint64(1) << unix.CAP_DAC_OVERRIDE) | (uint64(1) << unix.CAP_KILL)

// sandboxCapLastCap 读取 /proc/sys/kernel/cap_last_cap；读不到回退 63
// （内核当前最高能力号），保证超上界的 PR_CAPBSET_DROP EINVAL 被忽略后
// 不残留高号能力。
func sandboxCapLastCap() int {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 63
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n >= 0 && n <= 63 {
		return n
	}
	return 63
}

// allThreadsPrctl 优先经 syscall.AllThreadsSyscall6 对进程内全部 OS 线程
// 执行 prctl；cgo/-race 下 AllThreadsSyscall 返回 ENOTSUP/EOPNOTSUPP 时
// 回退当前已锁定 OS 线程（runChildFromEnv 入口已 LockOSThread，
// fork/exec 继承的是单线程 credentials，覆盖已足够）。
func allThreadsPrctl(option int, arg2, arg3, arg4, arg5 uintptr) error {
	_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, uintptr(option), arg2, arg3, arg4, arg5, 0)
	if errno == syscall.ENOTSUP || errno == unix.EOPNOTSUPP {
		return unix.Prctl(option, arg2, arg3, arg4, arg5)
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// allThreadsCapset 优先经 syscall.AllThreadsSyscall 对全部 OS 线程执行
// capset；ENOTSUP/EOPNOTSUPP 回退当前锁定线程的 unix.Capset。
func allThreadsCapset(hdr *unix.CapUserHeader, data *unix.CapUserData) error {
	_, _, errno := syscall.AllThreadsSyscall(
		unix.SYS_CAPSET,
		uintptr(unsafe.Pointer(hdr)),
		uintptr(unsafe.Pointer(data)),
		0,
	)
	runtime.KeepAlive(hdr)
	runtime.KeepAlive(data)
	if errno == syscall.ENOTSUP || errno == unix.EOPNOTSUPP {
		return unix.Capset(hdr, data)
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// dropSandboxCapabilities 根治裁剪 Linux capabilities：
//  1. 先循环 PR_CAPBSET_DROP 收缩 bounding set（跳过保留项；
//     CAP_SETPCAP 刻意留到循环最后一步再 drop——内核要求 PR_CAPBSET_DROP
//     调用时仍持 CAP_SETPCAP，提前 capset 清 Effective 或先行 drop 都会
//     让后续 drop 返回 EPERM）。超出 cap_last_cap 上界的 EINVAL 忽略；
//  2. PR_CAP_AMBIENT_CLEAR_ALL 清空 ambient（execve 提升通道一并关闭）；
//  3. capset(LINUX_CAPABILITY_VERSION_3) 把 Effective/Permitted 置为
//     sandboxKeepCapMask、Inheritable 置 0。
//
// 此后沙盒内 root 不再持有 CAP_SYS_ADMIN/SYS_BOOT/SYS_MODULE/CAP_BPF/
// CAP_PERFMON/CAP_NET_ADMIN/CAP_SYS_TIME/CAP_SYS_RAWIO/CAP_SYS_PTRACE/
// CAP_SYS_CHROOT/CAP_DAC_READ_SEARCH/CAP_LINUX_IMMUTABLE 等高权能力，
// capability 门控的宿主全局副作用（reboot/载模块/改时钟/改主机名等）
// 在能力校验层整体失效；helper/broker 与目标命令等全部子孙经 fork/exec
// 继承同一已裁剪能力集。
func dropSandboxCapabilities() error {
	lastCap := sandboxCapLastCap()
	for c := 0; c <= lastCap; c++ {
		if c == unix.CAP_DAC_OVERRIDE || c == unix.CAP_KILL || c == unix.CAP_SETPCAP {
			continue
		}
		if err := allThreadsPrctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
			if errno, ok := err.(syscall.Errno); ok && errno == syscall.EINVAL {
				continue // 该能力号超出本内核认知范围，无需裁剪
			}
			return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d): %w", c, err)
		}
	}
	// CAP_SETPCAP 留到循环最后一步：此后本进程再不能修改 bounding set，
	// 但后续步骤（ambient 清空、capset）均不需要该能力。
	if err := allThreadsPrctl(unix.PR_CAPBSET_DROP, unix.CAP_SETPCAP, 0, 0, 0); err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.EINVAL {
			// 内核不识 CAP_SETPCAP（不可能出现在支持能力的内核上），忽略。
		} else {
			return fmt.Errorf("prctl(PR_CAPBSET_DROP, CAP_SETPCAP): %w", err)
		}
	}
	if err := allThreadsPrctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_CAP_AMBIENT_CLEAR_ALL): %w", err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	data[0].Effective = uint32(sandboxKeepCapMask)
	data[0].Permitted = uint32(sandboxKeepCapMask)
	data[1].Effective = uint32(sandboxKeepCapMask >> 32)
	data[1].Permitted = uint32(sandboxKeepCapMask >> 32)
	if err := allThreadsCapset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset(0x%x): %w", sandboxKeepCapMask, err)
	}
	return nil
}

// mountSealBlockedSyscalls 返回当前架构下被封堵的系统调用号集合：
// 12 个挂载命名空间相关系统调用 + 6 个越狱/进程控制调用（chroot 是
// /proc/<pid>/root 魔链的越狱第二跳；ptrace/process_vm_*/pidfd_getfd/kcmp
// 是共享 PID ns 残余面下的进程控制族，独立 PID ns 已使宿主 PID 不可达，
// 此处纵深封锁本 ns 内滥用）+ 27 个 capability 门控的宿主全局副作用/
// 句柄/密钥环调用（能力裁剪是根治，seccomp 按号拦截做无旁路的纵深：
// reboot/kexec_load/kexec_file_load 换核与重启宿主、init_module/
// finit_module/delete_module 内核代码执行、bpf/perf_event_open 全域
// 观测与注入、fanotify_init 宿主打标、syslog 读内核日志、sethostname/
// setdomainname 改宿主 UTS 主机名、settimeofday/clock_settime/adjtimex/
// clock_adjtime 篡改宿主时钟、swapon/swapoff/quotactl/quotactl_fd/acct
// 存储与计费
// 面、open_by_handle_at/name_to_handle_at 路径无关句柄打开（Landlock
// 无拦截点）、add_key/request_key/keyctl init-userns 密钥环、
// pidfd_send_signal 经走私 pidfd 信令宿主进程）。amd64 另有 ioperm/iopl
// （IO 端口直访，arm64 无此调用号）。非 amd64/arm64 返回 nil（该架构
// 不安装过滤器）。pidfd_open 刻意不在名单内：Go 标准库用它管理本 ns
// 内子进程，而宿主 PID 在新 PID ns 下本就 ESRCH 不可达。
func mountSealBlockedSyscalls() []uintptr {
	blocked := []uintptr{
		// 挂载命名空间族
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
		// 越狱第二跳与进程控制族
		unix.SYS_CHROOT,
		unix.SYS_PTRACE,
		unix.SYS_PROCESS_VM_READV,
		unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_PIDFD_GETFD,
		unix.SYS_KCMP,
		// capability 门控的宿主全局副作用/句柄/密钥环族（纵深）
		unix.SYS_REBOOT,
		unix.SYS_KEXEC_LOAD,
		unix.SYS_KEXEC_FILE_LOAD,
		unix.SYS_INIT_MODULE,
		unix.SYS_FINIT_MODULE,
		unix.SYS_DELETE_MODULE,
		unix.SYS_BPF,
		unix.SYS_PERF_EVENT_OPEN,
		unix.SYS_FANOTIFY_INIT,
		unix.SYS_SYSLOG,
		unix.SYS_SETHOSTNAME,
		unix.SYS_SETDOMAINNAME,
		unix.SYS_SETTIMEOFDAY,
		unix.SYS_CLOCK_SETTIME,
		unix.SYS_ADJTIMEX,
		unix.SYS_CLOCK_ADJTIME,
		unix.SYS_SWAPON,
		unix.SYS_SWAPOFF,
		unix.SYS_QUOTACTL,
		unix.SYS_QUOTACTL_FD,
		unix.SYS_ACCT,
		unix.SYS_OPEN_BY_HANDLE_AT,
		unix.SYS_NAME_TO_HANDLE_AT,
		unix.SYS_ADD_KEY,
		unix.SYS_REQUEST_KEY,
		unix.SYS_KEYCTL,
		unix.SYS_PIDFD_SEND_SIGNAL,
	}
	switch runtime.GOARCH {
	case "amd64":
		return append(blocked, mountSealSysIopermAmd64, mountSealSysIoplAmd64)
	case "arm64":
		return blocked
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

// sealMountNamespace 安装 PR_SET_NO_NEW_PRIVS 与挂载族/进程控制族/宿主
// 全局副作用族系统调用的 seccomp-BPF 拦截（返回 EPERM），封堵沙盒内
// root 经 setns 跳回宿主挂载命名空间、经 umount2 撤销 socket 白名单
// 视图、经 chroot 利用魔链做越狱第二跳、经 ptrace/process_vm_*/
// pidfd_getfd/kcmp 操纵其它进程、或经 reboot/载模块/改时钟/bpf 等
// capability 门控调用改写宿主全局状态。
//
// 过滤器语义：seccomp_data.arch 与当前 AUDIT_ARCH 不符时直接
// SECCOMP_RET_KILL_PROCESS（异构 ABI 绝不放行任何系统调用）；amd64 下
// 另以 JGE __X32_SYSCALL_BIT 封堵 x32 ABI（x32 与原生 amd64 共享
// AUDIT_ARCH，调用号带 0x40000000 高位，不拦截会绕过按原生号布防的
// JEQ 链）；当前架构下 mountSealBlockedSyscalls 名单内系统调用一律
// 返回 ERRNO(EPERM)，其余全部 ALLOW——不影响后续 Landlock + seccomp
// user-notif 过滤器叠加安装与白名单命令 execve（过滤器与 NO_NEW_PRIVS
// 均跨 execve 保留，由 exec 目标进程继承）。
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
