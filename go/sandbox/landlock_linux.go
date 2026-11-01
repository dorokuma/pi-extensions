//go:build linux

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock 内核常量定义 (对应 linux/landlock.h UAPI，支持 ABI v1 至 v5+)。
const (
	landlockCreateRulesetVersion = 1 << 0

	landlockRulePathBeneath = 1

	// 文件系统访问权限 (ABI v1)
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
	// ABI v3 新增：文件截断 (truncate / O_TRUNC / creat)
	landlockAccessFSTruncate uint64 = 1 << 14
	// ABI v5 新增：字符/块设备 ioctl 控制
	landlockAccessFSIoctlDev uint64 = 1 << 15

	// 网络访问权限 (ABI v4 新增)
	landlockAccessNetBindTCP    uint64 = 1 << 0
	landlockAccessNetConnectTCP uint64 = 1 << 1

	// landlock_restrict_self 扩展标志（更高版本内核支持的内联全线程同步标志）
	landlockRestrictSelfTSync      uintptr = 1 << 3
	landlockRestrictSelfNoNewPrivs uintptr = 1 << 4
)

// Seccomp UAPI 常量定义 (对应 linux/seccomp.h 与 linux/filter.h)。
const (
	seccompSetModeFilter uintptr = 1

	seccompFilterFlagTSync       uintptr = 1 << 0
	seccompFilterFlagNewListener uintptr = 1 << 3
	seccompFilterFlagTSyncESRCH  uintptr = 1 << 4

	seccompRetUserNotif uint32 = 0x7fc00000
	seccompRetAllow     uint32 = 0x7fff0000

	seccompUserNotifFlagContinue uint32 = 1 << 0

	seccompIoctlNotifRecv    uintptr = 0xc0502100
	seccompIoctlNotifSend    uintptr = 0xc0182101
	seccompIoctlNotifIDValid uintptr = 0x40082102

	bpfLD   uint16 = 0x00
	bpfW    uint16 = 0x00
	bpfABS  uint16 = 0x20
	bpfJMP  uint16 = 0x05
	bpfJEQ  uint16 = 0x10
	bpfJSET uint16 = 0x40
	bpfK    uint16 = 0x00
	bpfRET  uint16 = 0x06
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

// landlockRulesetAttr 对应内核 struct landlock_ruleset_attr (ABI v1..v5 布局)。
type landlockRulesetAttr struct {
	HandledAccessFS  uint64
	HandledAccessNet uint64
}

// landlockPathBeneathAttr 对应内核 struct landlock_path_beneath_attr。
// 内核读取前 12 字节 (8 字节 AllowedAccess + 4 字节 ParentFD)。
type landlockPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

// seccompData 对应内核 struct seccomp_data (64 字节)。
type seccompData struct {
	NR                 int32
	Arch               uint32
	InstructionPointer uint64
	Args               [6]uint64
}

// seccompNotif 对应内核 struct seccomp_notif (80 字节)。
type seccompNotif struct {
	ID    uint64
	Pid   uint32
	Flags uint32
	Data  seccompData
}

// seccompNotifResp 对应内核 struct seccomp_notif_resp (24 字节)。
type seccompNotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

// defaultELFInterpreters 列出常见的 Linux ELF 动态链接器路径。
// 当 Policy.Exec 非空时，内核在 execve 动态链接二进制期间会以 FMODE_EXEC 打开 PT_INTERP 解释器，
// 因此需在 Landlock 中授予其文件级执行与读取权限，同时通过 seccomp PROT_EXEC 守卫拦截直接以命令行调用 ld-linux 的行为。
var defaultELFInterpreters = []string{
	"/lib64/ld-linux-x86-64.so.2",
	"/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
	"/usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
	"/usr/lib64/ld-linux-x86-64.so.2",
	"/usr/lib/ld-linux-x86-64.so.2",
	"/lib/ld-linux.so.2",
	"/lib/ld-linux-aarch64.so.1",
	"/lib/ld-musl-x86-64.so.1",
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

// defaultNullDevices 列出常用于重定向 (如 2>/dev/null) 的无状态安全字符设备。
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

func init() {
	if os.Getenv(envInternalModeKey) == "helper" {
		runHelperProcess()
		os.Exit(0)
	}
	if goExecDir := os.Getenv(envGoExecDirKey); goExecDir != "" && len(os.Args) > 0 {
		switch filepath.Base(os.Args[0]) {
		case "go":
			os.Exit(runGoWrapperClient(goExecDir))
		case "pi-test-runner":
			os.Exit(runTestRunnerClient(goExecDir))
		}
	}
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

// accessFileMask 返回文件（非目录）在对应 ABI 下合法授予的权限掩码。
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

	rulesetFD, err := createAndPopulateRuleset(p, abi, hasFS, enforceNet)
	if err != nil {
		return err
	}
	defer unix.Close(rulesetFD)

	goExecDir := lookupEnvValue(p.Env, envGoExecDirKey)
	realGoBin := lookupEnvValue(p.Env, envRealGoBinKey)

	// 1. 当配置了 Exec 白名单时，在收紧当前进程 Landlock 规则集之前先启动受控辅助进程并安装 seccomp PROT_EXEC 守卫。
	// 非 BestEffort 模式下，若辅助进程或 seccomp 安装失败，直接返回错误、不继续执行；
	// BestEffort 模式下输出告警并降级继续施加 Landlock 规则。
	if len(p.Exec) > 0 {
		if err := setupHelperAndSeccomp(p, goExecDir, realGoBin); err != nil {
			if !p.BestEffort {
				return fmt.Errorf("%w: %v", ErrSeccomp, err)
			}
			p.warnf("sandbox: seccomp PROT_EXEC guard unavailable (%v); degrading without ld-linux mmap restriction", err)
		}
	}

	// 2. 将当前进程（含全部线程）永久锁入目标 Landlock 规则集（此时当前进程对 goExecDir 仅具只读+执行权）。
	if err := enforceRuleset(rulesetFD); err != nil {
		return err
	}

	return applyEnv(p.Env)
}

func setupHelperAndSeccomp(p Policy, goExecDir, realGoBin string) error {
	ctrlFD, lifeWFD, err := startHelperProcess(p, goExecDir, realGoBin)
	if err != nil {
		return fmt.Errorf("start helper process: %w", err)
	}
	defer func() {
		if ctrlFD >= 0 {
			_ = unix.Close(ctrlFD)
		}
	}()

	notifyFD, secErr := installSeccompExecGuard()
	if secErr != nil {
		// 若非 BestEffort（或未启用 Go 构建代理），立即关闭存活管道让辅助进程退出
		if (!p.BestEffort || goExecDir == "") && lifeWFD >= 0 {
			_ = unix.Close(lifeWFD)
		}
		return fmt.Errorf("install seccomp filter: %w", secErr)
	}

	if err := sendNotifyFDToHelper(ctrlFD, notifyFD); err != nil {
		_ = unix.Close(notifyFD)
		if (!p.BestEffort || goExecDir == "") && lifeWFD >= 0 {
			_ = unix.Close(lifeWFD)
		}
		return fmt.Errorf("transfer seccomp notify fd to helper: %w", err)
	}
	_ = unix.Close(notifyFD)
	return nil
}

func createAndPopulateRuleset(p Policy, abi int, hasFS, enforceNet bool) (int, error) {
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
		return -1, fmt.Errorf("sandbox: landlock_create_ruleset (abi=%d): %w", abi, errno)
	}
	rulesetFD := int(r1)

	if hasFS {
		entries, err := buildPathRuleEntries(p, abi)
		if err != nil {
			_ = unix.Close(rulesetFD)
			return -1, err
		}
		for _, entry := range entries {
			if err := addPathRule(rulesetFD, entry.path, entry.access, abi, entry.optional, p); err != nil {
				_ = unix.Close(rulesetFD)
				return -1, err
			}
		}
	}
	return rulesetFD, nil
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

	// 当存在 Exec 白名单时，自动放行系统 ELF 动态链接器（供内核 open_exec 作为 PT_INTERP 加载）
	// 与基础共享库目录（只读），并通过 seccomp 守卫拦截用户态直接调用 ld-linux 的行为。
	if len(p.Exec) > 0 {
		for _, interp := range defaultELFInterpreters {
			_ = addEntry(interp, landlockAccessFSExecute|landlockAccessFSReadFile, true)
		}
		for _, libPath := range defaultSharedLibPaths {
			_ = addEntry(libPath, roMask, true)
		}
		_ = addEntry("/proc", roMask, true)
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
	// 在纯 Go 构建 (CGO_ENABLED=0) 下可原子覆盖整个 Go 进程的所有线程。
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

	// 2. 当进程使用了 cgo (例如运行 `go test -race` 时，race detector 会开启 iscgo) 时，
	// syscall.AllThreadsSyscall 会返回 ENOTSUP。
	if errno != syscall.ENOTSUP && errno != unix.EOPNOTSUPP {
		return fmt.Errorf("sandbox: prctl(PR_SET_NO_NEW_PRIVS) (all-threads): %w", errno)
	}

	// 将当前 goroutine 锁定在当前 OS 线程上 (成功时不解锁)，确保当前线程及后续 fork/exec 的子进程严格受限。
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

// installSeccompExecGuard 安装针对 SYS_MMAP / SYS_MPROTECT (带 PROT_EXEC) 的 seccomp-unotify 过滤器。
// 当任何受控进程或子进程尝试执行可执行内存映射时：
// 1. 若该进程的 /proc/<pid>/exe 指向动态链接器（如直接调用 `/lib64/ld-linux-x86-64.so.2 /root/.local/bin/agy`），立即拒绝并返回 EACCES；
// 2. 若映射的目标文件属于 ForbiddenExecNames 或位于不可执行的 ReadWrite 目录内（如从可写临时目录 LD_PRELOAD 注入），立即拒绝并返回 EACCES。
func installSeccompExecGuard() (int, error) {
	// seccomp(SECCOMP_SET_MODE_FILTER) 要求调用线程（及 TSYNC 同步的所有同组线程）已设置 PR_SET_NO_NEW_PRIVS。
	lockedThread := false
	_, _, nnpErrno := syscall.AllThreadsSyscall6(
		unix.SYS_PRCTL,
		unix.PR_SET_NO_NEW_PRIVS,
		1, 0, 0, 0, 0,
	)
	if nnpErrno != 0 {
		if nnpErrno != syscall.ENOTSUP && nnpErrno != unix.EOPNOTSUPP {
			return -1, fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", nnpErrno)
		}
		runtime.LockOSThread()
		lockedThread = true
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			runtime.UnlockOSThread()
			return -1, fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
		}
	}

	filter := []unix.SockFilter{
		// 0: 加载 seccomp_data.nr (偏移 0)
		{Code: bpfLD | bpfW | bpfABS, K: 0},
		// 1: 若 nr == SYS_MMAP 跳至指令 3 (检查 prot)
		{Code: bpfJMP | bpfJEQ | bpfK, Jt: 1, Jf: 0, K: uint32(unix.SYS_MMAP)},
		// 2: 若 nr == SYS_MPROTECT 继续至指令 3，否则跳至指令 6 (ALLOW)
		{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 3, K: uint32(unix.SYS_MPROTECT)},
		// 3: 加载 seccomp_data.args[2] 低 32 位 (prot 参数，偏移 32)
		{Code: bpfLD | bpfW | bpfABS, K: 32},
		// 4: 若 prot & PROT_EXEC != 0 则转交 USER_NOTIF (指令 5)，否则跳至 ALLOW (指令 6)
		{Code: bpfJMP | bpfJSET | bpfK, Jt: 0, Jf: 1, K: uint32(unix.PROT_EXEC)},
		// 5: 转交用户态监听器判定
		{Code: bpfRET | bpfK, K: seccompRetUserNotif},
		// 6: 放行
		{Code: bpfRET | bpfK, K: seccompRetAllow},
	}
	prog := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}

	flags := seccompFilterFlagNewListener | seccompFilterFlagTSync | seccompFilterFlagTSyncESRCH
	r1, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		seccompSetModeFilter,
		flags,
		uintptr(unsafe.Pointer(&prog)),
	)
	if errno != 0 {
		// 降级尝试不带 TSYNC_ESRCH 的当前线程监听器注册（如 cgo / -race 场景）
		r1, _, errno = unix.Syscall(
			unix.SYS_SECCOMP,
			seccompSetModeFilter,
			seccompFilterFlagNewListener,
			uintptr(unsafe.Pointer(&prog)),
		)
	}
	runtime.KeepAlive(&filter)
	runtime.KeepAlive(&prog)
	if errno != 0 {
		if lockedThread {
			runtime.UnlockOSThread()
		}
		return -1, errno
	}
	return int(r1), nil
}

func computeNonExecRWDirs(p Policy) []string {
	execSet := make(map[string]struct{}, len(p.Exec))
	for _, ep := range p.Exec {
		if norm, err := NormalizePath(ep); err == nil {
			execSet[norm] = struct{}{}
		}
	}
	var out []string
	for _, rw := range p.ReadWrite {
		if norm, err := NormalizePath(rw); err == nil {
			if _, ok := execSet[norm]; !ok {
				out = append(out, norm)
			}
		}
	}
	return out
}

func serveSeccompNotifications(notifyFD int, nonExecRWDirs []string) {
	defer unix.Close(notifyFD)
	var req seccompNotif
	var resp seccompNotifResp

	for {
		req = seccompNotif{}
		_, _, errno := unix.Syscall(
			unix.SYS_IOCTL,
			uintptr(notifyFD),
			seccompIoctlNotifRecv,
			uintptr(unsafe.Pointer(&req)),
		)
		if errno != 0 {
			if errno == unix.EINTR || errno == unix.ENOENT {
				continue
			}
			return
		}

		deny := shouldDenyExecMmap(&req, nonExecRWDirs)
		// 验证通知 ID 仍然有效，防止极端情况下的 PID 复用
		_, _, validErrno := unix.Syscall(
			unix.SYS_IOCTL,
			uintptr(notifyFD),
			seccompIoctlNotifIDValid,
			uintptr(unsafe.Pointer(&req.ID)),
		)
		if validErrno != 0 {
			continue
		}

		resp = seccompNotifResp{
			ID: req.ID,
		}
		if deny {
			resp.Error = -int32(unix.EACCES)
		} else {
			resp.Flags = seccompUserNotifFlagContinue
		}
		_, _, _ = unix.Syscall(
			unix.SYS_IOCTL,
			uintptr(notifyFD),
			seccompIoctlNotifSend,
			uintptr(unsafe.Pointer(&resp)),
		)
	}
}

func shouldDenyExecMmap(req *seccompNotif, nonExecRWDirs []string) bool {
	pid := int(req.Pid)
	if pid <= 0 {
		return true
	}

	// 1. 检查发起可执行映射的进程主程序是否为 ld-linux 等动态链接器或禁止执行的二进制。
	// 当通过内核 PT_INTERP 正常启动白名单程序时，/proc/<pid>/exe 指向该白名单程序本身；
	// 当用户直接执行 `/lib64/ld-linux-x86-64.so.2 <target>` 时，/proc/<pid>/exe 指向 ld-linux，在此直接拒绝。
	if exeTarget, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil && exeTarget != "" {
		exeTarget = strings.TrimSuffix(exeTarget, " (deleted)")
		if IsELFInterpreter(exeTarget) || IsForbiddenExec(exeTarget) {
			return true
		}
	}

	// 2. 若为 SYS_MMAP 且映射的是具体文件描述符，检查目标文件是否属于禁止执行的二进制或位于不可执行的可写目录中。
	if req.Data.NR == int32(unix.SYS_MMAP) {
		fd := int32(req.Data.Args[4])
		if fd >= 0 {
			if fdTarget, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, fd)); err == nil && fdTarget != "" {
				fdTarget = strings.TrimSuffix(fdTarget, " (deleted)")
				if IsForbiddenExec(fdTarget) {
					return true
				}
				for _, rwDir := range nonExecRWDirs {
					if fdTarget == rwDir || strings.HasPrefix(fdTarget, rwDir+string(filepath.Separator)) {
						return true
					}
				}
			}
		}
	}
	return false
}

type helperConfig struct {
	ReadOnly      []string `json:"ro"`
	ReadWrite     []string `json:"rw"`
	Exec          []string `json:"exec"`
	DenyNet       bool     `json:"deny_net"`
	GoExecDir     string   `json:"go_exec_dir"`
	RealGoBin     string   `json:"real_go_bin"`
	NonExecRWDirs []string `json:"non_exec_rw"`
	HasBroker     bool     `json:"has_broker"`
}

func startHelperProcess(p Policy, goExecDir, realGoBin string) (int, int, error) {
	selfExe, err := os.Executable()
	if err != nil || selfExe == "" {
		return -1, -1, fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(selfExe); err == nil && resolved != "" {
		selfExe = resolved
	}

	var pipeFDs [2]int
	if err := unix.Pipe2(pipeFDs[:], unix.O_CLOEXEC); err != nil {
		return -1, -1, err
	}
	lifeR := os.NewFile(uintptr(pipeFDs[0]), "life-r")
	lifeWFD := pipeFDs[1]

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		_ = lifeR.Close()
		_ = unix.Close(lifeWFD)
		return -1, -1, err
	}
	ctrlParentFD := sp[0]
	ctrlChildFile := os.NewFile(uintptr(sp[1]), "ctrl-sock")

	extraFiles := []*os.File{lifeR, ctrlChildFile}

	hasBroker := false
	var brokerSockFile *os.File
	if goExecDir != "" {
		sockPath := filepath.Join(goExecDir, "broker.sock")
		_ = os.Remove(sockPath)
		unixLn, err := net.Listen("unix", sockPath)
		if err == nil {
			_ = os.Chmod(sockPath, 0o600)
			if uln, ok := unixLn.(*net.UnixListener); ok {
				uln.SetUnlinkOnClose(false)
				if f, err := uln.File(); err == nil {
					brokerSockFile = f
					extraFiles = append(extraFiles, brokerSockFile)
					hasBroker = true
				}
			}
			_ = unixLn.Close()
		}
	}

	cfg := helperConfig{
		ReadOnly:      p.ReadOnly,
		ReadWrite:     p.ReadWrite,
		Exec:          p.Exec,
		DenyNet:       p.DenyNet,
		GoExecDir:     goExecDir,
		RealGoBin:     realGoBin,
		NonExecRWDirs: computeNonExecRWDirs(p),
		HasBroker:     hasBroker,
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		_ = lifeR.Close()
		_ = unix.Close(lifeWFD)
		_ = unix.Close(ctrlParentFD)
		_ = ctrlChildFile.Close()
		if brokerSockFile != nil {
			_ = brokerSockFile.Close()
		}
		return -1, -1, err
	}

	cmd := exec.Command(selfExe)
	cmd.Env = append(os.Environ(),
		envInternalModeKey+"=helper",
		envBrokerCfgKey+"="+string(cfgBytes),
	)
	cmd.ExtraFiles = extraFiles
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		_ = lifeR.Close()
		_ = unix.Close(lifeWFD)
		_ = unix.Close(ctrlParentFD)
		_ = ctrlChildFile.Close()
		if brokerSockFile != nil {
			_ = brokerSockFile.Close()
		}
		return -1, -1, err
	}

	// 子进程启动后再清除父进程所持 lifeWFD 的 FD_CLOEXEC，
	// 既避免辅助进程自身继承 lifeWFD，又保证当前进程后续调用 syscall.Exec (如 sbx) 时保持存活管道连接，
	// 且使用原始 fd 避免被 Go GC finalizer 提前关闭。
	_, _ = unix.FcntlInt(uintptr(lifeWFD), unix.F_SETFD, 0)

	_ = lifeR.Close()
	_ = ctrlChildFile.Close()
	if brokerSockFile != nil {
		_ = brokerSockFile.Close()
	}

	go func() {
		_ = cmd.Wait()
	}()

	var ack [1]byte
	if _, err := unix.Read(ctrlParentFD, ack[:]); err != nil {
		_ = unix.Close(ctrlParentFD)
		_ = unix.Close(lifeWFD)
		return -1, -1, err
	}
	return ctrlParentFD, lifeWFD, nil
}

func sendNotifyFDToHelper(ctrlFD int, notifyFD int) error {
	oob := unix.UnixRights(notifyFD)
	if err := unix.Sendmsg(ctrlFD, []byte("N"), oob, nil, 0); err != nil {
		return err
	}
	var ack [1]byte
	_, err := unix.Read(ctrlFD, ack[:])
	return err
}

func runHelperProcess() {
	rawCfg := os.Getenv(envBrokerCfgKey)
	if rawCfg == "" {
		return
	}
	var cfg helperConfig
	if err := json.Unmarshal([]byte(rawCfg), &cfg); err != nil {
		return
	}

	lifeR := os.NewFile(3, "life-r")
	ctrlFD := 4
	unix.CloseOnExec(3)
	unix.CloseOnExec(ctrlFD)

	var brokerLn net.Listener
	if cfg.HasBroker {
		sockFD := 5
		unix.CloseOnExec(sockFD)
		f := os.NewFile(uintptr(sockFD), "broker-sock")
		if f != nil {
			if ln, err := net.FileListener(f); err == nil {
				brokerLn = ln
			}
			_ = f.Close()
		}
	}

	// 1. 辅助进程自身施加 Landlock 约束，仅额外允许向 goExecDir/work 与 goExecDir/cache 写入编译产物。
	if abi, ok := supported(); ok {
		helperRW := append([]string{}, cfg.ReadWrite...)
		if cfg.GoExecDir != "" {
			helperRW = append(helperRW,
				filepath.Join(cfg.GoExecDir, "work"),
				filepath.Join(cfg.GoExecDir, "cache"),
			)
		}
		helperPolicy := Policy{
			ReadOnly:  cfg.ReadOnly,
			ReadWrite: helperRW,
			Exec:      cfg.Exec,
			DenyNet:   cfg.DenyNet,
		}
		if rulesetFD, err := createAndPopulateRuleset(helperPolicy, abi, true, cfg.DenyNet && abi >= 4); err == nil {
			_ = enforceRuleset(rulesetFD)
			_ = unix.Close(rulesetFD)
		}
	}

	// 2. 辅助进程为自身派生的子进程（如 go test）安装 seccomp PROT_EXEC 守卫。
	if selfNotifyFD, err := installSeccompExecGuard(); err == nil && selfNotifyFD >= 0 {
		unix.CloseOnExec(selfNotifyFD)
		go serveSeccompNotifications(selfNotifyFD, cfg.NonExecRWDirs)
	}

	if brokerLn != nil {
		go serveGoBroker(brokerLn, cfg)
	}

	// 通知父进程辅助进程已就绪
	_, _ = unix.Write(ctrlFD, []byte("1"))

	// 接收父进程传递过来的 seccomp notifyFD
	go func() {
		defer unix.Close(ctrlFD)
		buf := make([]byte, 16)
		oob := make([]byte, unix.CmsgSpace(4))
		_, oobn, _, _, err := unix.Recvmsg(ctrlFD, buf, oob, 0)
		if err != nil || oobn <= 0 {
			return
		}
		scms, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return
		}
		for _, scm := range scms {
			fds, err := unix.ParseUnixRights(&scm)
			if err == nil {
				for _, fd := range fds {
					unix.CloseOnExec(fd)
					go serveSeccompNotifications(fd, cfg.NonExecRWDirs)
				}
			}
		}
		_, _ = unix.Write(ctrlFD, []byte("1"))
	}()

	if lifeR != nil {
		_, _ = io.Copy(io.Discard, lifeR)
		_ = lifeR.Close()
	}
	if brokerLn != nil {
		_ = brokerLn.Close()
	}
	if cfg.GoExecDir != "" {
		_ = os.RemoveAll(filepath.Join(cfg.GoExecDir, "work"))
		_ = os.RemoveAll(filepath.Join(cfg.GoExecDir, "cache"))
	}
}

type goBrokerRequest struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
}

func serveGoBroker(ln net.Listener, cfg helperConfig) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleGoBrokerConn(conn, cfg)
	}
}

func handleGoBrokerConn(conn net.Conn, cfg helperConfig) {
	defer conn.Close()
	uconn, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}

	payload := make([]byte, 64*1024)
	oob := make([]byte, unix.CmsgSpace(3*4))
	n, oobn, _, _, err := uconn.ReadMsgUnix(payload, oob)
	if err != nil || n <= 0 {
		return
	}

	var stdioFiles []*os.File
	defer func() {
		for _, f := range stdioFiles {
			if f != nil {
				_ = f.Close()
			}
		}
	}()

	if oobn > 0 {
		scms, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err == nil {
			for _, scm := range scms {
				fds, err := unix.ParseUnixRights(&scm)
				if err == nil {
					for _, fd := range fds {
						unix.CloseOnExec(fd)
						stdioFiles = append(stdioFiles, os.NewFile(uintptr(fd), "stdio"))
					}
				}
			}
		}
	}
	if len(stdioFiles) < 3 {
		writeBrokerExitCode(uconn, 2)
		return
	}

	var req goBrokerRequest
	if err := json.Unmarshal(payload[:n], &req); err != nil {
		writeBrokerExitCode(uconn, 2)
		return
	}

	subcmd, err := validateGoWrapperArgs(req.Args, req.Dir, cfg.GoExecDir)
	if err != nil {
		_, _ = fmt.Fprintf(stdioFiles[2], "go: sandbox rejected arguments: %v\n", err)
		writeBrokerExitCode(uconn, 2)
		return
	}

	realGo := cfg.RealGoBin
	if realGo == "" {
		realGo = resolveRealGoBinary(filepath.Join(cfg.GoExecDir, "bin"))
	}
	if realGo == "" {
		_, _ = fmt.Fprintln(stdioFiles[2], "go: real go toolchain binary not found")
		writeBrokerExitCode(uconn, 127)
		return
	}

	workRoot := filepath.Join(cfg.GoExecDir, "work")
	invWorkDir, err := os.MkdirTemp(workRoot, "run-*")
	if err != nil {
		invWorkDir = workRoot
	} else {
		defer os.RemoveAll(invWorkDir)
	}

	finalArgs := req.Args
	if subcmd == "test" {
		runnerPath := filepath.Join(cfg.GoExecDir, "bin", "pi-test-runner")
		finalArgs = injectGoTestExecFlag(req.Args, runnerPath)
	}

	sanitizedEnv := sanitizeGoBrokerEnv(req.Env, cfg, invWorkDir)

	cmd := exec.Command(realGo, finalArgs...)
	cmd.Dir = req.Dir
	cmd.Env = sanitizedEnv
	cmd.Stdin = stdioFiles[0]
	cmd.Stdout = stdioFiles[1]
	cmd.Stderr = stdioFiles[2]

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			_, _ = fmt.Fprintf(stdioFiles[2], "go: %v\n", err)
			exitCode = 1
		}
	}
	writeBrokerExitCode(uconn, exitCode)
}

func writeBrokerExitCode(conn net.Conn, code int) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(int32(code)))
	_, _ = conn.Write(buf[:])
}

func validateGoWrapperArgs(args []string, cwd, goExecDir string) (string, error) {
	if len(args) == 0 {
		return "help", nil
	}

	cleanExecDir, err := NormalizePath(goExecDir)
	if err != nil {
		cleanExecDir = filepath.Clean(goExecDir)
	}

	subcmd := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}

		// 拦截可挂载外部执行器或输出至受保护执行目录的危险标志
		trimmed := strings.TrimLeft(arg, "-")
		flagName, flagVal, hasEq := strings.Cut(trimmed, "=")
		switch flagName {
		case "exec", "toolexec", "overlay", "C":
			return "", fmt.Errorf("flag -%s is forbidden in ReadOnlyReviewer sandbox", flagName)
		case "c":
			if subcmd == "test" {
				return "", errors.New("flag -c is forbidden in ReadOnlyReviewer go test")
			}
		case "o":
			outPath := flagVal
			if !hasEq {
				if i+1 >= len(args) {
					return "", errors.New("flag -o missing output argument")
				}
				outPath = args[i+1]
				i++
			}
			if !filepath.IsAbs(outPath) && cwd != "" {
				outPath = filepath.Join(cwd, outPath)
			}
			if normOut, err := NormalizePath(outPath); err == nil {
				if normOut == cleanExecDir || strings.HasPrefix(normOut, cleanExecDir+string(filepath.Separator)) {
					return "", fmt.Errorf("output path %q inside protected exec dir is forbidden", outPath)
				}
			}
			continue
		}

		if strings.HasPrefix(arg, "-o") && len(arg) > 2 && !hasEq {
			outPath := arg[2:]
			if !filepath.IsAbs(outPath) && cwd != "" {
				outPath = filepath.Join(cwd, outPath)
			}
			if normOut, err := NormalizePath(outPath); err == nil {
				if normOut == cleanExecDir || strings.HasPrefix(normOut, cleanExecDir+string(filepath.Separator)) {
					return "", fmt.Errorf("output path %q inside protected exec dir is forbidden", outPath)
				}
			}
			continue
		}

		if subcmd == "" && !strings.HasPrefix(arg, "-") {
			subcmd = arg
		}
	}

	if subcmd == "" {
		return "help", nil
	}

	switch subcmd {
	case "build", "vet", "test", "list", "env", "version", "mod", "doc", "fmt", "bug", "help":
		return subcmd, nil
	default:
		return "", fmt.Errorf("subcommand %q is not permitted in ReadOnlyReviewer sandbox", subcmd)
	}
}

func injectGoTestExecFlag(args []string, runnerPath string) []string {
	out := make([]string, 0, len(args)+1)
	injected := false
	for _, arg := range args {
		out = append(out, arg)
		if !injected && arg == "test" {
			out = append(out, "-exec="+runnerPath)
			injected = true
		}
	}
	return out
}

func sanitizeGoBrokerEnv(baseEnv []string, cfg helperConfig, invWorkDir string) []string {
	blockedKeys := map[string]bool{
		"GOFLAGS":          true,
		"CC":               true,
		"CXX":              true,
		"FC":               true,
		"GCCGO":            true,
		"AR":               true,
		"LD_PRELOAD":       true,
		"LD_AUDIT":         true,
		"LD_LIBRARY_PATH":  true,
		envInternalModeKey: true,
		envBrokerCfgKey:    true,
	}

	out := make([]string, 0, len(baseEnv)+4)
	for _, kv := range baseEnv {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || blockedKeys[k] {
			continue
		}
		if k == "GOTMPDIR" || k == "GOCACHE" {
			continue
		}
		out = append(out, kv)
	}

	cfgBytes, _ := json.Marshal(cfg)
	out = append(out,
		"GOTMPDIR="+invWorkDir,
		"GOCACHE="+filepath.Join(cfg.GoExecDir, "cache"),
		envGoExecDirKey+"="+cfg.GoExecDir,
		envBrokerCfgKey+"="+string(cfgBytes),
	)
	return out
}

func runGoWrapperClient(goExecDir string) int {
	subcmd, err := validateGoWrapperArgs(os.Args[1:], "", goExecDir)
	if err != nil || subcmd == "" {
		fmt.Fprintf(os.Stderr, "go: sandbox rejected command: %v\n", err)
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	sockPath := filepath.Join(goExecDir, "broker.sock")
	addr := &net.UnixAddr{Name: sockPath, Net: "unix"}
	uconn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "go: connect sandbox broker failed: %v\n", err)
		return 1
	}
	defer uconn.Close()

	req := goBrokerRequest{
		Args: os.Args[1:],
		Dir:  cwd,
		Env:  os.Environ(),
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return 1
	}

	oob := unix.UnixRights(int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd()))
	if _, _, err := uconn.WriteMsgUnix(payload, oob, nil); err != nil {
		fmt.Fprintf(os.Stderr, "go: send request to sandbox broker failed: %v\n", err)
		return 1
	}

	var respBuf [4]byte
	if _, err := io.ReadFull(uconn, respBuf[:]); err != nil {
		return 1
	}
	return int(int32(binary.LittleEndian.Uint32(respBuf[:])))
}

func runTestRunnerClient(goExecDir string) int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "pi-test-runner: missing test binary argument")
		return 2
	}
	targetBin, err := NormalizePath(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-test-runner: invalid binary path: %v\n", err)
		return 2
	}

	workRoot, err := NormalizePath(filepath.Join(goExecDir, "work"))
	if err != nil {
		workRoot = filepath.Join(filepath.Clean(goExecDir), "work")
	}
	if !strings.HasPrefix(targetBin, workRoot+string(filepath.Separator)) || !strings.HasSuffix(targetBin, ".test") {
		fmt.Fprintf(os.Stderr, "pi-test-runner: refusing to execute non-test binary %q\n", targetBin)
		return 126
	}

	// 在启动测试二进制前叠加审查员自身的 Landlock 规则集，收回对 goExecDir/work 与 goExecDir/cache 的写权限。
	var reviewerRW []string
	if rawCfg := os.Getenv(envBrokerCfgKey); rawCfg != "" {
		var cfg helperConfig
		if err := json.Unmarshal([]byte(rawCfg), &cfg); err == nil {
			if abi, ok := supported(); ok {
				reviewerPolicy := Policy{
					ReadOnly:  cfg.ReadOnly,
					ReadWrite: cfg.ReadWrite,
					Exec:      cfg.Exec,
					DenyNet:   cfg.DenyNet,
				}
				if rulesetFD, err := createAndPopulateRuleset(reviewerPolicy, abi, true, cfg.DenyNet && abi >= 4); err == nil {
					_ = enforceRuleset(rulesetFD)
					_ = unix.Close(rulesetFD)
				}
			}
			reviewerRW = cfg.ReadWrite
		}
	}

	// 将 GOTMPDIR 与 GOCACHE 恢复为审查员私有临时目录下的路径
	env := os.Environ()
	if len(reviewerRW) >= 2 {
		privateTmp := reviewerRW[1]
		env = overrideEnvKey(env, "TMPDIR", privateTmp)
		env = overrideEnvKey(env, "GOTMPDIR", filepath.Join(privateTmp, "go-tmp"))
		env = overrideEnvKey(env, "GOCACHE", filepath.Join(privateTmp, "go-cache"))
	}
	env = removeEnvKey(env, envBrokerCfgKey)

	if err := syscall.Exec(targetBin, os.Args[1:], env); err != nil {
		fmt.Fprintf(os.Stderr, "pi-test-runner: exec %s failed: %v\n", targetBin, err)
		return 126
	}
	return 0
}

func lookupEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func overrideEnvKey(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, prefix+val)
			replaced = true
		} else {
			out = append(out, kv)
		}
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}

func removeEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}
