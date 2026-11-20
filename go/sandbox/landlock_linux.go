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
	if len(os.Args) > 0 && filepath.Base(os.Args[0]) == "hc" {
		// hc 受限客户端：ReviewerAgent 在 <goExecDir>/bin/hc 建软链指向 sbx 本体。
		// 分发先于 envGoExecDirKey 判空，保证 env 缺失时客户端以 75 报告而非落到 runMain。
		os.Exit(runReportClient(os.Getenv(envGoExecDirKey)))
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
	path   string
	access uint64
	// optional 为软可选系统路径（ELF 解释器/共享库目录//proc//dev/*）：
	// 打开失败时容忍 ENOENT/ENOTDIR/EACCES。
	optional bool
	// transient 仅对 Policy.OptionalReadWrite 登记的瞬态状态文件生效：
	// 打开返回 ENOENT（含悬空软链/快照枚举后被宿主删除）时安全跳过；
	// 不授予任何额外权限，也绝不放宽普通 ReadWrite 条目的 fail-closed 语义。
	transient bool
}

// errSeccompFilterInstalled 标记 seccomp PROT_EXEC 过滤器已装入当前进程
// 之后发生的下游失败（notify FD 转发给 helper 失败）。过滤器一经安装即
// 不可撤销：即使 BestEffort 也不得降级继续——否则后续 PROT_EXEC 映射会
// 永远阻塞在等待无人服务的 notify 应答上。该标记只用于内部分型，不构成
// 新的公开协议；Apply 返回的错误仍包装 ErrSeccomp。
var errSeccompFilterInstalled = errors.New("seccomp PROT_EXEC filter already installed (unrecoverable)")

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
			// 过滤器已装入本进程（post-install）的失败不可撤销：任何模式
			// （含 BestEffort）都必须上抛，拒绝继续；安装前失败仍遵循
			// BestEffort 降级契约。
			if !p.BestEffort || errors.Is(err, errSeccompFilterInstalled) {
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

// startHelperForSetup 是 setupHelperAndSeccomp 派生辅助进程用的函数点
// （默认直指 startHelperProcess）。提取为包内未导出变量仅供同包单测注入
// 受控 helper（如就绪 ACK 后 notify 握手 EOF/非法字节），驱动真实
// Apply→setup→post-install 失败路径；无任何环境变量/CLI/Policy 字段入口，
// 生产恒为 startHelperProcess。
var startHelperForSetup = startHelperProcess

func setupHelperAndSeccomp(p Policy, goExecDir, realGoBin string) error {
	ctrlFD, lifeWFD, err := startHelperForSetup(p, goExecDir, realGoBin)
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
		// fail-closed：关闭存活管道终止 helper，避免留下无 seccomp
		// 守卫的 broker/辅助进程继续服务。
		if lifeWFD >= 0 {
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
			if err := addPathRule(rulesetFD, entry.path, entry.access, abi, entry.optional, entry.transient, p); err != nil {
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

	// optionalRWSet 归一化 Policy.OptionalReadWrite 为规范化路径集合：
	// 仅对其中登记的瞬态路径在 Apply 打开时容忍 ENOENT。NormalizePaths
	// 失败（非法路径本应在 Validate 被拦截）时保守置空，不扩大容忍面。
	optionalRWSet := make(map[string]struct{}, len(p.OptionalReadWrite))
	if normORW, err := NormalizePaths(p.OptionalReadWrite); err == nil {
		for _, op := range normORW {
			optionalRWSet[op] = struct{}{}
		}
	}

	addEntry := func(raw string, access uint64, optional bool, transient bool) error {
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
				if !transient {
					entries[idx].transient = false
				}
			} else {
				indexByPath[c] = len(entries)
				entries = append(entries, pathRuleEntry{
					path:      c,
					access:    access,
					optional:  optional,
					transient: transient,
				})
			}
		}
		return nil
	}

	roMask := accessReadOnly()
	for _, path := range p.ReadOnly {
		if err := addEntry(path, roMask, false, false); err != nil {
			return nil, err
		}
	}

	rwMask := accessReadWrite(abi)
	for _, path := range p.ReadWrite {
		// 快照枚举的瞬态状态文件（OptionalReadWrite 成员）在 Apply 时允许
		// ENOENT 跳过；普通 ReadWrite 条目保持 fail-closed ENOENT 报错。
		normRW, _ := NormalizePath(path)
		_, transient := optionalRWSet[normRW]
		if err := addEntry(path, rwMask, false, transient); err != nil {
			return nil, err
		}
	}

	execMask := accessExec()
	for _, path := range p.Exec {
		if err := addEntry(path, execMask, false, false); err != nil {
			return nil, err
		}
	}

	// 当存在 Exec 白名单时，自动放行系统 ELF 动态链接器（供内核 open_exec 作为 PT_INTERP 加载）
	// 与基础共享库目录（只读），并通过 seccomp 守卫拦截用户态直接调用 ld-linux 的行为。
	if len(p.Exec) > 0 {
		for _, interp := range defaultELFInterpreters {
			_ = addEntry(interp, landlockAccessFSExecute|landlockAccessFSReadFile, true, false)
		}
		for _, libPath := range defaultSharedLibPaths {
			_ = addEntry(libPath, roMask, true, false)
		}
		_ = addEntry("/proc", roMask, true, false)
	}

	if len(p.ReadOnly) > 0 || len(p.ReadWrite) > 0 || len(p.Exec) > 0 {
		for _, dev := range defaultNullDevices {
			_ = addEntry(dev, rwMask, true, false)
		}
		for _, dev := range defaultRandomDevices {
			_ = addEntry(dev, roMask, true, false)
		}
	}

	return entries, nil
}

func addPathRule(rulesetFD int, path string, access uint64, abi int, optional, transient bool, p Policy) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if transient && errors.Is(err, unix.ENOENT) {
			// OptionalReadWrite 登记的瞬态状态文件（如 agy .db-shm）：
			// 枚举快照后已被宿主删除（含悬空软链）时安全跳过，不击穿 Apply。
			return nil
		}
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

func serveSeccompNotifications(notifyFD int, nonExecRWDirs []string, entryAgents []string) {
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

		deny := shouldDenyExecMmap(&req, nonExecRWDirs, entryAgents)
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

func shouldDenyExecMmap(req *seccompNotif, nonExecRWDirs []string, entryAgents []string) bool {
	pid := int(req.Pid)
	if pid <= 0 {
		return true
	}

	// 1. 检查发起可执行映射的进程主程序是否为 ld-linux 等动态链接器或禁止执行的二进制。
	// 当通过内核 PT_INTERP 正常启动白名单程序时，/proc/<pid>/exe 指向该白名单程序本身；
	// 当用户直接执行 `/lib64/ld-linux-x86-64.so.2 <target>` 时，/proc/<pid>/exe 指向 ld-linux，在此直接拒绝。
	// ReviewerAgent 顶层放行的 EntryAgents（agy/pig/pi/claude）作为 exe 时不得一律拒绝 PROT_EXEC，
	// 否则代理进程自身的合法内存映射会被误杀。
	if exeTarget, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil && exeTarget != "" {
		exeTarget = strings.TrimSuffix(exeTarget, " (deleted)")
		if IsELFInterpreter(exeTarget) {
			return true
		}
		if IsForbiddenExec(exeTarget) && !IsEntryAgentPath(entryAgents, exeTarget) {
			return true
		}
	}

	// 2. 若为 SYS_MMAP 且映射的是具体文件描述符，检查目标文件是否属于禁止执行的二进制或位于不可执行的可写目录中。
	if req.Data.NR == int32(unix.SYS_MMAP) {
		fd := int32(req.Data.Args[4])
		if fd >= 0 {
			if fdTarget, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, fd)); err == nil && fdTarget != "" {
				fdTarget = strings.TrimSuffix(fdTarget, " (deleted)")
				// 与 exe 检查同口径：EntryAgents 成员（agy/pig/pi/claude 及其
				// 过身份校验的软链目标）自身被 PROT_EXEC 映射属合法运行期行为，
				// 不得与毒软链目标一同被拒。
				if IsForbiddenExec(fdTarget) && !IsEntryAgentPath(entryAgents, fdTarget) {
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
	ReadOnly          []string `json:"ro"`
	ReadWrite         []string `json:"rw"`
	OptionalReadWrite []string `json:"orw"`
	Exec              []string `json:"exec"`
	DenyNet           bool     `json:"deny_net"`
	GoExecDir         string   `json:"go_exec_dir"`
	RealGoBin         string   `json:"real_go_bin"`
	NonExecRWDirs     []string `json:"non_exec_rw"`
	EntryAgents       []string `json:"entry_agents"`
	HasBroker         bool     `json:"has_broker"`
	// BestEffort 透传父策略降级口径：缺省 false 会让 helper 对 BestEffort
	// 策略内的瞬态缺失路径（OptionalReadWrite 之外的 ENOENT）fail-closed，
	// 与父进程已接受的降级语义不一致（oracle FM-5）。
	BestEffort bool `json:"best_effort"`
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
		if !hasBroker {
			// 请求了 Go 构建代理但监听器未能建立：fail-closed，
			// 不得悄悄以无 broker 形态继续启动。
			_ = lifeR.Close()
			_ = unix.Close(lifeWFD)
			_ = unix.Close(ctrlParentFD)
			_ = ctrlChildFile.Close()
			if brokerSockFile != nil {
				_ = brokerSockFile.Close()
			}
			_ = os.Remove(sockPath)
			return -1, -1, fmt.Errorf("create go broker listener at %q failed", sockPath)
		}
	}

	cfg := helperConfig{
		ReadOnly:          p.ReadOnly,
		ReadWrite:         p.ReadWrite,
		OptionalReadWrite: p.OptionalReadWrite,
		Exec:              p.Exec,
		DenyNet:           p.DenyNet,
		GoExecDir:         goExecDir,
		RealGoBin:         realGoBin,
		NonExecRWDirs:     computeNonExecRWDirs(p),
		EntryAgents:       append([]string(nil), p.EntryAgents...),
		HasBroker:         hasBroker,
		BestEffort:        p.BestEffort,
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

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var ack [1]byte
	n, err := readRetryEINTR(ctrlParentFD, ack[:])
	if err != nil || n != 1 || ack[0] != '1' {
		// helper 就绪握手失败：读错误、EOF（n=0）或非 '1' 字节均视为
		// 拒绝启动。杀死子进程并 wait 收割，完整回收控制与存活管道。
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = unix.Close(ctrlParentFD)
		_ = unix.Close(lifeWFD)
		_ = <-waitCh
		if err != nil {
			return -1, -1, fmt.Errorf("read helper readiness ack: %w", err)
		}
		return -1, -1, fmt.Errorf("helper rejected startup (ack: n=%d byte=%q)", n, ack[0])
	}
	return ctrlParentFD, lifeWFD, nil
}

// readRetryEINTR 包装阻塞式 unix.Read：Go 运行时的异步抢占信号 SIGURG
// 会打断无 SA_RESTART 的 socket/pipe 读，EINTR 属可重试中断而非失败，
// 未包裹时会被误判为 helper 握手 IO 失败（oracle FM-5）。
func readRetryEINTR(fd int, b []byte) (int, error) {
	for {
		n, err := unix.Read(fd, b)
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

func sendNotifyFDToHelper(ctrlFD int, notifyFD int) error {
	oob := unix.UnixRights(notifyFD)
	var err error
	for {
		err = unix.Sendmsg(ctrlFD, []byte("N"), oob, nil, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("send seccomp notify fd: %w: %w", errSeccompFilterInstalled, err)
	}
	var ack [1]byte
	n, err := readRetryEINTR(ctrlFD, ack[:])
	if err != nil {
		return fmt.Errorf("read helper notify-fd ack: %w: %w", errSeccompFilterInstalled, err)
	}
	if n != 1 || ack[0] != '1' {
		return fmt.Errorf("helper rejected seccomp notify fd (ack: n=%d byte=%q): %w", n, ack[0], errSeccompFilterInstalled)
	}
	return nil
}

// helperWriteAll 向控制通道写完整 len(b) 字节：unix.Write 遇 EINTR（Go
// 运行时的异步抢占信号 SIGURG 会打断无 SA_RESTART 的 socket 写）重试，
// 短写续写直至写完；真正失败（含 EPIPE/ECONNRESET——父进程已撤）返回
// 错误。就绪 ACK 与 notify-fd ACK 共用本路径：返回值被丢弃时，EINTR 中断
// 未写出会让父进程阻塞读 ACK 而 helper 在 Recvmsg 等 notifyFD——双方互等；
// 第二 ACK 写中断时 defer 关通道让父进程把正常启动误判失败（oracle M4）。
// helperWriteEINTRObservedForTest 是包内未导出的 EINTR 观测回调：仅在
// helperWriteAll 真实命中 unix.EINTR 分支时调用一次（默认 nil 零开销）。
// 供同包单测以原子计数证明真实内核 EINTR 被命中并重试，杜绝「未命中前
// 不排管道」的无界等待假绿；无任何环境变量/CLI/config 开启入口。
var helperWriteEINTRObservedForTest func()

func helperWriteAll(fd int, b []byte) error {
	for len(b) > 0 {
		n, err := unix.Write(fd, b)
		if errors.Is(err, unix.EINTR) {
			if helperWriteEINTRObservedForTest != nil {
				helperWriteEINTRObservedForTest()
			}
			continue
		}
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// helperFailf 报告辅助进程启动失败并退出（退出码 1，区别于就绪 ACK）。
// 关闭控制通道使父进程读 ACK 收到 EOF/非 '1'，再关闭存活管道退出。
func helperFailf(ctrlFD int, lifeR *os.File, brokerLn net.Listener, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sandbox helper: "+format+"\n", args...)
	if ctrlFD >= 0 {
		_ = unix.Close(ctrlFD)
		ctrlFD = -1
	}
	if lifeR != nil {
		_ = lifeR.Close()
	}
	if brokerLn != nil {
		_ = brokerLn.Close()
	}
	os.Exit(1)
}

func runHelperProcess() {
	// 控制通道与存活管道从 ExtraFiles 继承；缺失即无法向父进程报到，直接退出。
	lifeR := os.NewFile(3, "life-r")
	ctrlFD := 4
	if lifeR == nil {
		fmt.Fprintln(os.Stderr, "sandbox helper: missing inherited lifetime pipe")
		os.Exit(1)
	}
	unix.CloseOnExec(3)
	unix.CloseOnExec(ctrlFD)

	fail := func(format string, args ...any) {
		helperFailf(ctrlFD, lifeR, nil, format, args...)
	}

	rawCfg := os.Getenv(envBrokerCfgKey)
	if rawCfg == "" {
		fail("missing %s configuration", envBrokerCfgKey)
	}
	var cfg helperConfig
	if err := json.Unmarshal([]byte(rawCfg), &cfg); err != nil {
		fail("invalid %s configuration: %v", envBrokerCfgKey, err)
	}

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
		if brokerLn == nil {
			fail("re-attach broker listener failed")
		}
	}

	// 1. 辅助进程自身施加 Landlock 约束，仅额外允许向 goExecDir/work 与 goExecDir/cache 写入编译产物。
	// 任一步失败即 fail-closed：helper 不得在未受限状态下 ACK 并对外提供服务。
	// MF-1：审查员 Policy.Exec 已收缩为 <goExecDir>/bin（不含 work/）；helper
	// 仍须能执行 broker 拉起的 go test 产物（<goExecDir>/work/run-*/.../*.test）
	// ——该授权仅存于 helper Landlock 域，审查员进程不享有。
	abi, ok := supported()
	if !ok {
		fail("landlock not supported")
	}
	helperRW := append([]string{}, cfg.ReadWrite...)
	helperExec := append([]string{}, cfg.Exec...)
	if cfg.GoExecDir != "" {
		helperRW = append(helperRW,
			filepath.Join(cfg.GoExecDir, "work"),
			filepath.Join(cfg.GoExecDir, "cache"),
		)
		helperExec = append(helperExec, filepath.Join(cfg.GoExecDir, "work"))
	}
	helperPolicy := Policy{
		ReadOnly:          cfg.ReadOnly,
		ReadWrite:         helperRW,
		OptionalReadWrite: cfg.OptionalReadWrite,
		Exec:              helperExec,
		DenyNet:           cfg.DenyNet,
		BestEffort:        cfg.BestEffort,
	}
	rulesetFD, err := createAndPopulateRuleset(helperPolicy, abi, true, cfg.DenyNet && abi >= 4)
	if err != nil {
		fail("create landlock ruleset: %v", err)
	}
	if err := enforceRuleset(rulesetFD); err != nil {
		_ = unix.Close(rulesetFD)
		fail("enforce landlock ruleset: %v", err)
	}
	_ = unix.Close(rulesetFD)

	// 2. 辅助进程为自身派生的子进程（如 go test）安装 seccomp PROT_EXEC 守卫。
	selfNotifyFD, err := installSeccompExecGuard()
	if err != nil || selfNotifyFD < 0 {
		fail("install seccomp exec guard: %v", err)
	}
	unix.CloseOnExec(selfNotifyFD)
	go serveSeccompNotifications(selfNotifyFD, cfg.NonExecRWDirs, cfg.EntryAgents)

	if brokerLn != nil {
		go serveGoBroker(brokerLn, cfg)
	}

	// 通知父进程辅助进程已就绪：EINTR 重试并校验完整 1 字节写出；写
	// 失败（父进程已撤/通道关闭）直接退出，不再进入 Recvmsg 等待——
	// 父进程读不到 ACK 不会推进到 notifyFD 转发，helper 必须 fail-closed。
	if err := helperWriteAll(ctrlFD, []byte("1")); err != nil {
		_ = unix.Close(ctrlFD)
		ctrlFD = -1
		fail("write readiness ack: %v", err)
	}

	// 接收父进程传递过来的 seccomp notifyFD；仅当完整收到 notify FD
	// 并启动 serving goroutine 后才向父进程回 ACK，任一步骤失败则
	// 不写入字节——父进程将 EOF/非 '1' 视为握手失败并终止本进程。
	go func() {
		defer unix.Close(ctrlFD)
		buf := make([]byte, 16)
		oob := make([]byte, unix.CmsgSpace(4))
		var oobn int
		var err error
		for {
			_, oobn, _, _, err = unix.Recvmsg(ctrlFD, buf, oob, 0)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil || oobn <= 0 {
			return
		}
		scms, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return
		}
		received := false
		for _, scm := range scms {
			fds, err := unix.ParseUnixRights(&scm)
			if err == nil {
				for _, fd := range fds {
					unix.CloseOnExec(fd)
					go serveSeccompNotifications(fd, cfg.NonExecRWDirs, cfg.EntryAgents)
					received = true
				}
			}
		}
		if !received {
			return
		}
		// notify-fd ACK 同口径：EINTR 重试、校验完整写出；写失败时
		// defer unix.Close(ctrlFD) 触发对端 EOF 唤醒父进程（把正常启动
		// 判失败比静默丢失 ACK 更符合 fail-closed）。
		_ = helperWriteAll(ctrlFD, []byte("1"))
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

	// 纵深防御：cfg.RealGoBin 必须过受信校验（真实常规可执行文件、非组/
	// 全局可写、属主合规、固定前缀、父目录合规）；不通过或为空时回退
	// resolveSandboxRealGoBinary——与预热/策略同一受信实体，绝不执行
	// 未校验的任意路径（oracle MF-2 broker 侧污染面）。
	realGo := ""
	if cfg.RealGoBin != "" {
		if trusted, ok := trustedRealGoCandidate(cfg.RealGoBin); ok {
			realGo = trusted
		}
	}
	if realGo == "" {
		realGo = resolveSandboxRealGoBinary(filepath.Join(cfg.GoExecDir, "bin"))
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

	sanitizedEnv := sanitizeGoBrokerEnv(req.Env, cfg, invWorkDir, subcmd)

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

// resolvePathThroughAncestorSymlinks 把 path 中首个已存在的祖先目录经
// filepath.EvalSymlinks 解析为真实路径，再拼回剩余相对段与末级名——
// 末级不存在、中间级不存在或含多级软链时均能对齐最终真实落点。
// 不解软链的 NormalizePath 无法发现 "$TMPDIR/sym -> <goExecDir>/work" 这类
// 祖先软链别名（oracle9 MF-1）；本函数把校验锚定到真实文件系统。
// 解析失败（路径完全不存在至根、软链环等）返回空串，调用方按 fail-closed
// 拒绝处理。
func resolvePathThroughAncestorSymlinks(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != "" {
		return filepath.Clean(resolved)
	}
	// 逐级向上找首个已存在的祖先目录（Lstat 不跟随软链判定存在性，
	// EvalSymlinks 在祖先上解析其真实目标）。
	ancestor := filepath.Dir(abs)
	missing := []string{filepath.Base(abs)}
	for {
		if ancestor == "/" || ancestor == "." {
			return ""
		}
		if _, lerr := os.Lstat(ancestor); lerr == nil {
			resolved, rerr := filepath.EvalSymlinks(ancestor)
			if rerr != nil || resolved == "" {
				return ""
			}
			parts := []string{filepath.Clean(resolved)}
			for i := len(missing) - 1; i >= 0; i-- {
				parts = append(parts, missing[i])
			}
			return filepath.Join(parts...)
		} else if !os.IsNotExist(lerr) {
			return ""
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
}

func validateGoWrapperArgs(args []string, cwd, goExecDir string) (string, error) {
	if len(args) == 0 {
		return "help", nil
	}

	cleanExecDir, err := NormalizePath(goExecDir)
	if err != nil {
		cleanExecDir = filepath.Clean(goExecDir)
	}
	// 受保护根一律取解析后的真实路径：$TMPDIR/sym -> <goExecDir>/work 这类
	// 祖先软链别名必须落回真实树比较（oracle9 MF-1）。
	realExecDir := cleanExecDir
	if resolved := resolvePathThroughAncestorSymlinks(cleanExecDir); resolved != "" {
		realExecDir = resolved
	}
	inProtected := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		return candidate == realExecDir || strings.HasPrefix(candidate, realExecDir+string(filepath.Separator))
	}

	subcmd := ""
	explicitOutput := false
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
		// MF-1：链接器/编译器外挂标志可在 broker Landlock 域内执行外部
		// 命令（如 -ldflags=-linkmode=external -extld=<wrapper>），白名单
		// 工具（objcopy/ld/gcc）由此可把任意 ELF 拷入 <goExecDir>/work；
		// go build/vet/test 合法流程均不需要这些标志，一律封禁。
		case "ldflags", "gcflags", "asmflags", "gccgoflags":
			return "", fmt.Errorf("flag -%s is forbidden in ReadOnlyReviewer sandbox", flagName)
		case "c":
			if subcmd == "test" {
				return "", errors.New("flag -c is forbidden in ReadOnlyReviewer go test")
			}
		case "o":
			explicitOutput = true
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
			resolvedOut := resolvePathThroughAncestorSymlinks(outPath)
			if inProtected(resolvedOut) {
				return "", fmt.Errorf("output path %q inside protected exec dir is forbidden", outPath)
			}
			continue
		}

		if strings.HasPrefix(arg, "-o") && len(arg) > 2 && !hasEq {
			explicitOutput = true
			outPath := arg[2:]
			if !filepath.IsAbs(outPath) && cwd != "" {
				outPath = filepath.Join(cwd, outPath)
			}
			resolvedOut := resolvePathThroughAncestorSymlinks(outPath)
			if inProtected(resolvedOut) {
				return "", fmt.Errorf("output path %q inside protected exec dir is forbidden", outPath)
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

	// MF-1：缺省 -o 时 go build/go test 会把产物写进 cwd（req.Dir）；cwd 位于
	// <goExecDir>（其下 work 为 broker 可写域）时等同经 broker 向受保护
	// 执行目录写二进制，必须拒绝。校验锚定 EvalSymlinks 后真实路径，
	// 覆盖 cwd 经软链指向 goExecDir 的别名形态。
	if !explicitOutput && cwd != "" {
		resolvedCwd := resolvePathThroughAncestorSymlinks(cwd)
		if inProtected(resolvedCwd) {
			return "", fmt.Errorf("working directory %q inside protected exec dir is forbidden without explicit -o", cwd)
		}
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

func sanitizeGoBrokerEnv(baseEnv []string, cfg helperConfig, invWorkDir string, subcmd string) []string {
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
	// MF-1：仅 go test 链路把 .test 产物执行根暴露给 pi-test-runner——
	// runner 在叠加审查员规则集前需要经 cfg.Exec 放行 <goExecDir>/work；
	// 非 test 子命令不注入，审查员侧 Policy.Env 同样不含该键。
	if subcmd == "test" {
		out = append(out, envGoTestWorkDirKey+"="+filepath.Join(cfg.GoExecDir, "work"))
	}
	return out
}

func runGoWrapperClient(goExecDir string) int {
	// MF-1：先取真实 cwd 再做客户端预校验——空串会让缺省 -o 时的 cwd 校验
	// 与 -o 相对路径拼接受阻（broker 侧还会以 req.Dir 复验，双端同口径）。
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	subcmd, err := validateGoWrapperArgs(os.Args[1:], cwd, goExecDir)
	if err != nil || subcmd == "" {
		fmt.Fprintf(os.Stderr, "go: sandbox rejected command: %v\n", err)
		return 2
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
	// fail-closed：配置缺失/损坏或任一限制步骤失败即拒绝执行目标二进制，绝不裸 exec。
	rawCfg := os.Getenv(envBrokerCfgKey)
	if rawCfg == "" {
		fmt.Fprintf(os.Stderr, "pi-test-runner: missing %s configuration\n", envBrokerCfgKey)
		return 126
	}
	var cfg helperConfig
	if err := json.Unmarshal([]byte(rawCfg), &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "pi-test-runner: invalid %s configuration: %v\n", envBrokerCfgKey, err)
		return 126
	}
	abi, ok := supported()
	if !ok {
		fmt.Fprintln(os.Stderr, "pi-test-runner: landlock not supported")
		return 126
	}
	// MF-1：审查员 Policy.Exec 已收缩为 <goExecDir>/bin；.test 产物位于
	// helper 可写域 <goExecDir>/work 下，审查员侧不再持有该执行授权。
	// pi-test-runner 在叠加审查员规则集时，仅经 broker 注入的
	// envGoTestWorkDirKey 把该产物根追加进 Exec（仅本进程，非审查员全局）。
	runnerExec := append([]string{}, cfg.Exec...)
	if testWorkDir := os.Getenv(envGoTestWorkDirKey); testWorkDir != "" {
		runnerExec = append(runnerExec, testWorkDir)
	}
	rulesetPolicy := Policy{
		ReadOnly:          cfg.ReadOnly,
		ReadWrite:         cfg.ReadWrite,
		OptionalReadWrite: cfg.OptionalReadWrite,
		Exec:              runnerExec,
		DenyNet:           cfg.DenyNet,
		BestEffort:        cfg.BestEffort,
	}
	rulesetFD, err := createAndPopulateRuleset(rulesetPolicy, abi, true, cfg.DenyNet && abi >= 4)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-test-runner: create landlock ruleset failed: %v\n", err)
		return 126
	}
	if err := enforceRuleset(rulesetFD); err != nil {
		_ = unix.Close(rulesetFD)
		fmt.Fprintf(os.Stderr, "pi-test-runner: enforce landlock ruleset failed: %v\n", err)
		return 126
	}
	_ = unix.Close(rulesetFD)

	// 将 GOTMPDIR 与 GOCACHE 恢复为审查员私有临时目录下的路径：私有目录
	// 从环境 TMPDIR 判定（ReviewerAgentWithEnv/ReadOnlyReviewer 已注入
	// TMPDIR=<privateTmpDir>），不再硬编码用 reviewerRW[1]——ReadWrite
	// 列表已移除 <workdir>/reports（oracle SF-2），按位置取会错位。
	env := os.Environ()
	if privateTmp := lookupEnvValue(env, "TMPDIR"); privateTmp != "" {
		env = overrideEnvKey(env, "GOTMPDIR", filepath.Join(privateTmp, "go-tmp"))
		env = overrideEnvKey(env, "GOCACHE", filepath.Join(privateTmp, "go-cache"))
	}
	env = removeEnvKey(env, envBrokerCfgKey)
	// envGoTestWorkDirKey 仅本进程消费，不透传给被执行的 .test 二进制。
	env = removeEnvKey(env, envGoTestWorkDirKey)

	if err := syscall.Exec(targetBin, os.Args[1:], env); err != nil {
		fmt.Fprintf(os.Stderr, "pi-test-runner: exec %s failed: %v\n", targetBin, err)
		return 126
	}
	return 0
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
