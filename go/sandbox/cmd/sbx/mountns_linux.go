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

// 沙盒 socket 白名单视图（仅 Linux，需 CLONE_NEWNS）：
// sbx 外层父进程以 Unshareflags=CLONE_NEWNS 派生受限子进程，子进程在进入
// Landlock 之前（runChildFromEnv → isolateRuntimeSockets → Apply）把宿主
// 暴露的 IPC socket 面收成白名单视图：
//   - 整个 /run 被空 tmpfs 覆盖，仅按原相对路径回绑白名单项
//     （/run/user/0/moshi-hook.sock——agy 钩子遥测；/etc/resolv.conf 经
//     EvalSymlinks 落在 /run/ 下的真实文件，保证 DNS stub 解析不断），
//     覆盖层最后 remount 为只读；
//   - /tmp/tmux-0 存在时被空 tmpfs 覆盖（tmux send-keys 注入面）；
//   - 覆盖路径不存在时跳过；任一步骤失败默认拒绝启动（fail-closed），
//     --best-effort 下只告警降级；
//   - 全部挂载与清理完成后安装 PR_SET_NO_NEW_PRIVS + seccomp-BPF
//     过滤器：mount/umount2/setns/unshare/pivot_root/open_tree/move_mount/
//     fsopen/fsconfig/fsmount/fspick/mount_setattr 一律返回 EPERM，
//     使沙盒内 root（init userns 的 CAP_SYS_ADMIN）无法经 setns 跳回宿主
//     挂载命名空间或 umount2 撤销白名单视图。
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

	mountSealRetErrno       uint32 = 0x00050000 // SECCOMP_RET_ERRNO
	mountSealRetAllow       uint32 = 0x7fff0000 // SECCOMP_RET_ALLOW
	mountSealRetKillProcess uint32 = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	mountSealAuditArchX8664 uint32 = 0xc000003e // AUDIT_ARCH_X86_64
	mountSealAuditArchARM64 uint32 = 0xc00000b7 // AUDIT_ARCH_AARCH64
	mountSealDataNrOffset   uint32 = 0          // seccomp_data.nr
	mountSealDataArchOffset uint32 = 4          // seccomp_data.arch
	mountSealBPFJmpMax             = 255        // BPF 单跳上限

	bpfLD  uint16 = 0x00 // BPF_LD
	bpfW   uint16 = 0x00 // BPF_W
	bpfABS uint16 = 0x20 // BPF_ABS
	bpfJMP uint16 = 0x05 // BPF_JMP
	bpfJEQ uint16 = 0x10 // BPF_JEQ
	bpfK   uint16 = 0x00 // BPF_K
	bpfRET uint16 = 0x06 // BPF_RET
)

// childSysProcAttr 返回派生受限子进程使用的 SysProcAttr：CLONE_NEWNS 创建
// 独立挂载命名空间。Go 运行时在 unshare 后自动把 / 置为 rprivate
// （src/syscall/exec_linux.go: unshare 不处理 --shared 挂载的传播标记，
// Go 会补 mount(NULL, "/", NULL, MS_REC|MS_PRIVATE, NULL)），子进程内的
// 覆盖挂载不会传播回宿主挂载命名空间。
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
}

// isolateRuntimeSockets 在受限子进程内（进入 Landlock 之前）施加白名单视图
// 并在全部挂载与清理完成后安装 mount-seal seccomp 锁。
// 仅当父进程确实以 CLONE_NEWNS 派生（childExecSpec.MountNS=true）时才被调用；
// 任一覆盖/回绑/封锁步骤失败默认返回错误拒绝启动，bestEffort 时仅告警并继续
// （含挂载未全部成功时仍按同策略安装 seccomp 锁）。
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

	// Go 运行时在 CLONE_NEWNS 后已自动执行一次；此处显式再置一次，语义独立于
	// runtime 实现细节，确保后续覆盖挂载不传播回宿主挂载命名空间。
	if err := step("private-propagation", func() error {
		return unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	}); err != nil {
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
	rel  string // 相对 /run 的路径（也是覆盖层内重建挂载点的路径）
	host string // 覆盖前宿主视角的绝对路径
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
		items = append(items, runPreserveItem{rel: moshiHookSockRel, host: moshiHost})
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
// （文件回绑补只读 remount），最后把覆盖层 remount 为只读（MS_REMOUNT 只
// 作用于覆盖层自身，不影响其上的回绑挂载）。覆盖路径不存在时跳过。
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
		// 文件回绑补只读 remount：bind 挂载自身的只读化不影响经其打开的
		// socket connect()/文件读，只挡写路径。
		if err := unix.Mount("", dst, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", it.rel, err)
		}
	}
	if err := unix.Mount("tmpfs", runDir, "tmpfs",
		unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("remount %s read-only: %w", runDir, err)
	}
	return nil
}

// mountSealBlockedSyscalls 返回当前架构下被封堵的挂载命名空间相关系统调用
// 号集合。非 amd64/arm64 返回 nil（该架构不安装过滤器）。
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

// sealMountNamespace 安装 PR_SET_NO_NEW_PRIVS 与挂载族系统调用的
// seccomp-BPF 拦截（返回 EPERM），封堵沙盒内 root 经 setns 跳回宿主挂载
// 命名空间或经 umount2 撤销 socket 白名单视图。
//
// 过滤器语义：seccomp_data.arch 与当前 AUDIT_ARCH 不符时直接
// SECCOMP_RET_KILL_PROCESS（异构 ABI 绝不放行任何系统调用）；当前架构下
// 12 个挂载族系统调用返回 ERRNO(EPERM)，其余全部 ALLOW——不影响后续
// Landlock + seccomp user-notif 过滤器叠加安装与白名单命令 execve
// （过滤器与 NO_NEW_PRIVS 均跨 execve 保留，由 exec 目标进程继承）。
//
// 仅支持 linux/amd64 与 linux/arm64；其它架构返回 nil 跳过（与
// --best-effort 无关，属不支持平台的空操作，不新增回归）。
//
// 必须 LockOSThread：NNP 与过滤器都挂在调用线程上，且必须保证随后
// syscall.Exec 的就是这条线程（execve 保留线程的 NNP 与过滤器栈）。
// 锁定不解开是有意的：本函数返回后子进程只会走到 Apply 与 exec 或退出。
func sealMountNamespace() error {
	auditArch := mountSealAuditArch()
	blocked := mountSealBlockedSyscalls()
	if auditArch == 0 || len(blocked) == 0 {
		return nil
	}
	if len(blocked)+4 > mountSealBPFJmpMax {
		return fmt.Errorf("mount-seal filter too large: %d insns", len(blocked)+4)
	}

	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}

	// 指令布局：
	//   0: 加载 seccomp_data.arch
	//   1: 若非当前 AUDIT_ARCH → KILL_PROCESS（idxKill）
	//   2: 加载 seccomp_data.nr
	//   3..3+N-1: 逐一 JEQ 被封堵系统调用号 → ERRNO(EPERM)（idxErrno）；
	//              未命中继续下一条，全部未命中顺序落到 ALLOW（idxAllow）
	//   idxAllow = 3+N:  RET ALLOW
	//   idxErrno = 4+N:  RET ERRNO|EPERM
	//   idxKill  = 5+N:  RET KILL_PROCESS
	n := len(blocked)
	idxAllow := 3 + n
	idxErrno := idxAllow + 1
	idxKill := idxAllow + 2

	filter := make([]unix.SockFilter, 0, idxKill+1)
	filter = append(filter,
		unix.SockFilter{Code: bpfLD | bpfW | bpfABS, K: mountSealDataArchOffset},
		unix.SockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: uint8(idxKill - 2), K: auditArch},
		unix.SockFilter{Code: bpfLD | bpfW | bpfABS, K: mountSealDataNrOffset},
	)
	for i, nr := range blocked {
		filter = append(filter, unix.SockFilter{
			Code: bpfJMP | bpfJEQ | bpfK,
			Jt:   uint8(idxErrno - (3 + i) - 1),
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
		0,
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
