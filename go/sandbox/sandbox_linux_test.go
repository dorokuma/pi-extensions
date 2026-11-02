//go:build linux

package sandbox_test

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
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
