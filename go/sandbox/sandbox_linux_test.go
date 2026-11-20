//go:build linux

package sandbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/dorokuma/pi-extensions/go/sandbox"
)

// occupySeccompListenerSlot 预先占用当前进程的唯一 SECCOMP_FILTER_FLAG_NEW_LISTENER
// 槽位（安装一个 allow-all 过滤器并挂新 listener），使后续 sandbox.Apply 安装
// seccomp 守卫时必定触发内核 EBUSY 错误。仅 Linux 可用。
func occupySeccompListenerSlot() (int, error) {
	const (
		seccompSetModeFilter         uintptr = 1
		seccompFilterFlagTSync       uintptr = 1 << 0
		seccompFilterFlagNewListener uintptr = 1 << 3
		seccompFilterFlagTSyncESRCH  uintptr = 1 << 4
		seccompRetAllow              uint32  = 0x7fff0000
		bpfRET                       uint16  = 0x06
		bpfK                         uint16  = 0x00
	)

	runtime.LockOSThread()
	_, _, errno := syscall.AllThreadsSyscall6(
		unix.SYS_PRCTL,
		unix.PR_SET_NO_NEW_PRIVS,
		1, 0, 0, 0, 0,
	)
	if errno != 0 {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return -1, err
		}
	}

	filter := []unix.SockFilter{
		{Code: bpfRET | bpfK, K: seccompRetAllow},
	}
	prog := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}

	flags := seccompFilterFlagNewListener | seccompFilterFlagTSync | seccompFilterFlagTSyncESRCH
	r1, _, sysErrno := unix.Syscall(
		unix.SYS_SECCOMP,
		seccompSetModeFilter,
		flags,
		uintptr(unsafe.Pointer(&prog)),
	)
	if sysErrno != 0 {
		r1, _, sysErrno = unix.Syscall(
			unix.SYS_SECCOMP,
			seccompSetModeFilter,
			seccompFilterFlagNewListener,
			uintptr(unsafe.Pointer(&prog)),
		)
	}
	runtime.KeepAlive(&filter)
	runtime.KeepAlive(&prog)
	if sysErrno != 0 {
		return -1, sysErrno
	}
	return int(r1), nil
}

// subprocNonUTF8Preset 是 TestReadOnlyReviewerNonUTF8RootSkipsManifest 的
// 隔离子进程线束：在独立 mount namespace 里把 /tmp 重挂为只读 tmpfs，强制
// ReadOnlyReviewer 的 os.MkdirTemp("/tmp") 主路径失败、落到 os.TempDir()
// 备用分支；再把 TMPDIR 指向含非 UTF-8 字节的根，使备用分支产生的
// privateTmpDir/goExecDir 均为非 UTF-8 路径。打印最终 policy.Env 供父进程
// 断言 PI_SANDBOX_PRESET_TMP_DIRS 登记键缺失（不登记）与两目录确为非 UTF-8。
// 打印 SKIP= 前缀时父进程 t.Skip（unshare/mount 需 CAP_SYS_ADMIN）。
func subprocNonUTF8Preset() {
	// mount namespace 是线程局部状态（CLONE_NEWNS 只影响调用线程）：必须在
	// Unshare 前把 goroutine 钉在当前 OS 线程上，否则 Unshare 后被调度到
	// 其它线程会让后续 mount 落回宿主 mount ns（改宿主传播属性、覆盖宿主
	// 挂载点）。锁后 Unshare→rprivate→tmpfs→MkdirTemp→清理全程同一线程。
	// 子进程最终以 os.Exit 退出，线程随进程销毁，无需配对 UnlockOSThread。
	runtime.LockOSThread()
	os.Exit(runNonUTF8Preset())
}

// runNonUTF8Preset 承载 subprocNonUTF8Preset 的实际逻辑，返回进程退出码。
// 独立成内层函数是为了让 defer 清理生效：os.Exit 跳过所有 defer，故把
// 含 defer 的逻辑收进本函数、外层统一 os.Exit；t0 建立后注册 RemoveAll，
// SKIP/FATAL/成功三条返回路径都会回收 nonutf8-base-* 与其下 root-\xff
// 非 UTF-8 根，不留夹具残留。
func runNonUTF8Preset() int {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		fmt.Printf("SKIP=unshare:%v\n", err)
		return 0
	}
	// rprivate 是隔离硬前置（与 cmd/sbx/mountns_linux.go 同口径）：失败时
	// / 仍为 shared 传播，后续 tmpfs 重挂会泄漏回宿主 mount ns，必须立即
	// 中止，绝不在未私有化的 / 上继续 mount。
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		fmt.Printf("FATAL=rprivate:%v\n", err)
		return 1
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_RDONLY, ""); err != nil {
		fmt.Printf("SKIP=mount:%v\n", err)
		return 0
	}
	// 非 UTF-8 备用根必须仍在可写区（/tmp 已 ro）：按序尝试常见可写根。
	var t0 string
	for _, base := range []string{"/dev/shm", "/run", "/"} {
		d, err := os.MkdirTemp(base, "nonutf8-base-")
		if err == nil {
			t0 = d
			break
		}
	}
	if t0 == "" {
		fmt.Println("SKIP=no-writable-base")
		return 0
	}
	// t0 建立后注册 defer 清理：其后的所有 return（FATAL/成功）都会回收
	// t0（root-\xff 非 UTF-8 根是 t0 子目录，一并删除）。
	defer os.RemoveAll(t0)
	nonUTF8Root := filepath.Join(t0, "root-"+string([]byte{0xff}))
	if err := os.MkdirAll(nonUTF8Root, 0o700); err != nil {
		fmt.Printf("FATAL=root:%v\n", err)
		return 1
	}
	if err := os.Setenv("TMPDIR", nonUTF8Root); err != nil {
		fmt.Printf("FATAL=setenv:%v\n", err)
		return 1
	}

	policy := sandbox.ReadOnlyReviewer(".")
	for _, kv := range policy.Env {
		fmt.Println("ENV=" + kv)
	}
	fmt.Printf("VALID_TMP=%v\n", utf8.ValidString(envOf(policy.Env, "TMPDIR")))
	fmt.Printf("VALID_EXEC=%v\n", utf8.ValidString(envOf(policy.Env, "PI_SANDBOX_GO_EXEC_DIR")))
	// 兜底清理：两目录都在 TMPDIR 下，父进程不负责清理。
	_ = os.RemoveAll(envOf(policy.Env, "TMPDIR"))
	_ = os.RemoveAll(envOf(policy.Env, "PI_SANDBOX_GO_EXEC_DIR"))
	return 0
}

// envOf 是子进程线束内的 env list 查找辅助（不走导出 API）。
func envOf(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if len(kv) > len(prefix) && kv[:len(prefix)] == prefix {
			return kv[len(prefix):]
		}
	}
	return ""
}
