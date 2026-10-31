//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock 内核常量定义（对应 linux/landlock.h UAPI，支持 ABI v1 至 v5+）。
const (
	landlockCreateRulesetVersion = 1 << 0

	landlockRulePathBeneath = 1

	// 文件系统访问权限位（ABI v1）
	landlockAccessFSExecute    uint64 = 1 << 0
	landlockAccessFSWriteFile  uint64 = 1 << 1
	landlockAccessFSReadFile   uint64 = 1 << 2
	landlockAccessFSReadDir    uint64 = 1 << 3
	landlockAccessFSRemoveDir  uint64 = 1 << 4
	landlockAccessFSRemoveFile uint64 = 1 << 5
	landlockAccessFSMakeChar   uint64 = 1 << 6
	landlockAccessFSMakeDir    uint64 = 1 << 7
	landlockAccessFSMakeReg    uint64 = 1 << 8
	landlockAccessFSMakeSock   uint64 = 1 << 9
	landlockAccessFSMakeFifo   uint64 = 1 << 10
	landlockAccessFSMakeBlock  uint64 = 1 << 11
	landlockAccessFSMakeSym    uint64 = 1 << 12

	// ABI v2 新增：跨目录链接或重命名
	landlockAccessFSRefer uint64 = 1 << 13
	// ABI v3 新增：文件截断（truncate / O_TRUNC / creat）
	landlockAccessFSTruncate uint64 = 1 << 14
	// ABI v5 新增：字符/块设备 ioctl 控制
	landlockAccessFSIoctlDev uint64 = 1 << 15

	// 网络访问权限位（ABI v4 新增）
	landlockAccessNetBindTCP    uint64 = 1 << 0
	landlockAccessNetConnectTCP uint64 = 1 << 1

	// landlock_restrict_self 扩展标志（更高版本内核支持的内联全线程同步标志）
	landlockRestrictSelfTSync      uintptr = 1 << 3
	landlockRestrictSelfNoNewPrivs uintptr = 1 << 4
)

const accessFSABIV1Mask uint64 = landlockAccessFSExecute |
	landlockAccessFSWriteFile |
	landlockAccessFSReadFile |
	landlockAccessFSReadDir |
	landlockAccessFSRemoveDir |
	landlockAccessFSRemoveFile |
	landlockAccessFSMakeChar |
	landlockAccessFSMakeDir |
	landlockAccessFSMakeReg |
	landlockAccessFSMakeSock |
	landlockAccessFSMakeFifo |
	landlockAccessFSMakeBlock |
	landlockAccessFSMakeSym

// landlockRulesetAttr 对应内核 struct landlock_ruleset_attr（ABI v1..v5 布局）。
type landlockRulesetAttr struct {
	HandledAccessFS  uint64
	HandledAccessNet uint64
}

// landlockPathBeneathAttr 对应内核 struct landlock_path_beneath_attr。
// 内核读取前 12 字节（8 字节 AllowedAccess + 4 字节 ParentFD）。
type landlockPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

// defaultELFInterpreters 列出常见的 Linux ELF 动态链接器路径。
// 当 Policy.Exec 非空时，内核在 execve 动态链接二进制期间会以 FMODE_EXEC 打开解释器，
// 因此需要授予动态链接器文件级执行与读取权限。
var defaultELFInterpreters = []string{
	"/lib64/ld-linux-x86-64.so.2",
	"/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
	"/usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
	"/usr/lib64/ld-linux-x86-64.so.2",
	"/usr/lib/ld-linux-x86-64.so.2",
	"/lib/ld-linux.so.2",
	"/lib/ld-linux-aarch64.so.1",
	"/lib/ld-musl-x86_64.so.1",
}

// defaultSharedLibPaths 列出动态链接二进制启动所需的系统共享库目录与缓存文件。
var defaultSharedLibPaths = []string{
	"/etc/ld.so.cache",
	"/etc/ld.so.conf",
	"/etc/ld.so.conf.d",
	"/lib",
	"/lib64",
	"/usr/lib",
	"/usr/lib64",
	"/usr/local/lib",
}

// defaultNullDevices 列出常用于重定向（如 2>/dev/null）的无状态安全字符设备。
var defaultNullDevices = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/full",
}

// defaultRandomDevices 列出运行时常用的只读随机数设备。
var defaultRandomDevices = []string{
	"/dev/urandom",
	"/dev/random",
}

func handledFSForABI(abi int) uint64 {
	mask := accessFSABIV1Mask
	if abi >= 2 {
		mask |= landlockAccessFSRefer
	}
	if abi >= 3 {
		mask |= landlockAccessFSTruncate
	}
	if abi >= 5 {
		mask |= landlockAccessFSIoctlDev
	}
	return mask
}

// accessFileMask 返回单文件（非目录）在对应 ABI 下可合法授予的权限掩码。
// 若对非目录 fd 传入目录专有权限位（如 READ_DIR / MAKE_REG），内核 landlock_add_rule 会返回 EINVAL。
func accessFileMask(abi int) uint64 {
	mask := landlockAccessFSExecute | landlockAccessFSWriteFile | landlockAccessFSReadFile
	if abi >= 3 {
		mask |= landlockAccessFSTruncate
	}
	if abi >= 5 {
		mask |= landlockAccessFSIoctlDev
	}
	return mask
}

func accessReadOnly() uint64 {
	return landlockAccessFSReadFile | landlockAccessFSReadDir
}

func accessReadWrite(abi int) uint64 {
	return handledFSForABI(abi) &^ landlockAccessFSExecute
}

func accessExec() uint64 {
	return landlockAccessFSExecute | landlockAccessFSReadFile | landlockAccessFSReadDir
}

func supported() (int, bool) {
	r1, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0,
		0,
		landlockCreateRulesetVersion,
	)
	if errno != 0 {
		return 0, false
	}
	abi := int(r1)
	if abi <= 0 {
		return 0, false
	}
	return abi, true
}

type pathRuleEntry struct {
	path     string
	access   uint64
	optional bool
}

func applyPolicy(p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}

	abi, ok := supported()
	if !ok {
		if p.BestEffort {
			p.warnf("sandbox: Landlock is not supported by kernel; degrading to no-op")
			return applyEnv(p.Env)
		}
		return ErrUnsupported
	}

	enforceNet := p.DenyNet
	if enforceNet && abi < 4 {
		if p.BestEffort {
			p.warnf("sandbox: kernel Landlock ABI %d < 4 does not support DenyNet; enforcing filesystem policy only", abi)
			enforceNet = false
		} else {
			return fmt.Errorf("%w (current ABI: %d)", ErrNetUnsupported, abi)
		}
	}

	hasFS := len(p.ReadOnly) > 0 || len(p.ReadWrite) > 0 || len(p.Exec) > 0
	if !hasFS && !enforceNet {
		return applyEnv(p.Env)
	}

	attr := landlockRulesetAttr{}
	if hasFS {
		attr.HandledAccessFS = handledFSForABI(abi)
	}
	if enforceNet {
		attr.HandledAccessNet = landlockAccessNetBindTCP | landlockAccessNetConnectTCP
	}

	attrSize := unsafe.Sizeof(attr)
	if abi < 4 {
		attrSize = unsafe.Sizeof(attr.HandledAccessFS)
	}

	r1, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		attrSize,
		0,
	)
	runtime.KeepAlive(&attr)
	if errno != 0 {
		return fmt.Errorf("sandbox: landlock_create_ruleset (abi=%d): %w", abi, errno)
	}
	rulesetFD := int(r1)
	defer unix.Close(rulesetFD)

	if hasFS {
		entries, err := buildPathRuleEntries(p, abi)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := addPathRule(rulesetFD, entry.path, entry.access, abi, entry.optional, p); err != nil {
				return err
			}
		}
	}

	if err := enforceRuleset(rulesetFD); err != nil {
		return err
	}
	return applyEnv(p.Env)
}

func buildPathRuleEntries(p Policy, abi int) ([]pathRuleEntry, error) {
	indexByPath := make(map[string]int)
	var entries []pathRuleEntry

	addEntry := func(raw string, access uint64, optional bool) error {
		norm, err := NormalizePath(raw)
		if err != nil {
			return err
		}
		candidates := []string{norm}
		if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
			if resolvedAbs, err := NormalizePath(resolved); err == nil && resolvedAbs != norm {
				candidates = append(candidates, resolvedAbs)
			}
		}
		for _, c := range candidates {
			if idx, exists := indexByPath[c]; exists {
				entries[idx].access |= access
				if !optional {
					entries[idx].optional = false
				}
			} else {
				indexByPath[c] = len(entries)
				entries = append(entries, pathRuleEntry{
					path:     c,
					access:   access,
					optional: optional,
				})
			}
		}
		return nil
	}

	roMask := accessReadOnly()
	for _, path := range p.ReadOnly {
		if err := addEntry(path, roMask, false); err != nil {
			return nil, err
		}
	}

	rwMask := accessReadWrite(abi)
	for _, path := range p.ReadWrite {
		if err := addEntry(path, rwMask, false); err != nil {
			return nil, err
		}
	}

	execMask := accessExec()
	for _, path := range p.Exec {
		if err := addEntry(path, execMask, false); err != nil {
			return nil, err
		}
	}

	// 当存在 Exec 白名单时，自动放行系统 ELF 动态链接器（执行+读取）与基础共享库目录（只读），
	// 以及 /dev/null 等安全字符设备，确保白名单内的动态链接工具与子进程可正常装载运行。
	if len(p.Exec) > 0 {
		for _, interp := range defaultELFInterpreters {
			_ = addEntry(interp, landlockAccessFSExecute|landlockAccessFSReadFile, true)
		}
		for _, libPath := range defaultSharedLibPaths {
			_ = addEntry(libPath, roMask, true)
		}
	}

	if len(p.ReadOnly) > 0 || len(p.ReadWrite) > 0 || len(p.Exec) > 0 {
		for _, dev := range defaultNullDevices {
			_ = addEntry(dev, rwMask, true)
		}
		for _, dev := range defaultRandomDevices {
			_ = addEntry(dev, roMask, true)
		}
	}

	return entries, nil
}

func addPathRule(rulesetFD int, path string, access uint64, abi int, optional bool, p Policy) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if optional && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EACCES)) {
			return nil
		}
		if p.BestEffort && errors.Is(err, unix.ENOENT) {
			p.warnf("sandbox: skipping non-existent path %q: %v", path, err)
			return nil
		}
		return fmt.Errorf("sandbox: open %q: %w", path, err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("sandbox: fstat %q: %w", path, err)
	}

	if (st.Mode & unix.S_IFMT) != unix.S_IFDIR {
		access &= accessFileMask(abi)
	} else {
		access &= handledFSForABI(abi)
	}
	if access == 0 {
		return nil
	}

	ruleAttr := landlockPathBeneathAttr{
		AllowedAccess: access,
		ParentFD:      int32(fd),
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		landlockRulePathBeneath,
		uintptr(unsafe.Pointer(&ruleAttr)),
		0,
		0,
		0,
	)
	runtime.KeepAlive(&ruleAttr)
	if errno != 0 {
		return fmt.Errorf("sandbox: landlock_add_rule %q: %w", path, errno)
	}
	return nil
}

func enforceRuleset(rulesetFD int) error {
	// 1. 优先通过 Go 标准库 syscall.AllThreadsSyscall6 / AllThreadsSyscall 在所有 OS 线程同步生效。
	// 在纯 Go 构建（CGO_ENABLED=0）下可原子覆盖整个 Go 进程的所有线程。
	_, _, errno := syscall.AllThreadsSyscall6(
		unix.SYS_PRCTL,
		unix.PR_SET_NO_NEW_PRIVS,
		1, 0, 0, 0, 0,
	)
	if errno == 0 {
		_, _, errno = syscall.AllThreadsSyscall(
			unix.SYS_LANDLOCK_RESTRICT_SELF,
			uintptr(rulesetFD),
			0,
			0,
		)
		if errno != 0 {
			return fmt.Errorf("sandbox: landlock_restrict_self (all-threads): %w", errno)
		}
		return nil
	}

	// 2. 当进程启用了 cgo（例如运行 `go test -race` 时，race detector 会开启 iscgo）时，
	// syscall.AllThreadsSyscall 会返回 ENOTSUP。
	if errno != syscall.ENOTSUP && errno != unix.EOPNOTSUPP {
		return fmt.Errorf("sandbox: prctl(PR_SET_NO_NEW_PRIVS) (all-threads): %w", errno)
	}

	// 将当前 goroutine 锁定在当前 OS 线程上（成功时不解锁），确保当前线程及后续 fork/exec 的子进程严格受限。
	runtime.LockOSThread()

	// 若运行在支持 TSYNC | NO_NEW_PRIVS 的高版本内核上，尝试内核态全线程同步。
	_, _, tsyncErrno := unix.Syscall(
		unix.SYS_LANDLOCK_RESTRICT_SELF,
		uintptr(rulesetFD),
		landlockRestrictSelfTSync|landlockRestrictSelfNoNewPrivs,
		0,
	)
	if tsyncErrno == 0 {
		return nil
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("sandbox: prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	_, _, restrictErrno := unix.Syscall(
		unix.SYS_LANDLOCK_RESTRICT_SELF,
		uintptr(rulesetFD),
		0,
		0,
	)
	if restrictErrno != 0 {
		runtime.UnlockOSThread()
		return fmt.Errorf("sandbox: landlock_restrict_self: %w", restrictErrno)
	}
	return nil
}
