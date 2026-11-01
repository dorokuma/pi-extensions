package sandbox_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/dorokuma/pi-extensions/go/sandbox"
)

const testSubprocEnv = "PI_SANDBOX_TEST_SUBPROC"

func TestMain(m *testing.M) {
	switch os.Getenv(testSubprocEnv) {
	case "ro_vs_rw":
		subprocROvsRW()
		return
	case "exec_whitelist":
		subprocExecWhitelist()
		return
	case "reviewer_preset":
		subprocReviewerPreset()
		return
	case "reviewer_go_toolchain":
		subprocReviewerGoToolchain()
		return
	case "seccomp_failure":
		subprocSeccompFailure()
		return
	case "reviewer_agent_entry":
		subprocReviewerAgentEntry()
		return
	}
	os.Exit(m.Run())
}

func TestNormalizePathsAndValidate(t *testing.T) {
	if _, err := sandbox.NormalizePath(""); !errors.Is(err, sandbox.ErrInvalidPolicy) {
		t.Fatalf("expected ErrInvalidPolicy for empty path, got %v", err)
	}
	if _, err := sandbox.NormalizePath("bad\x00path"); !errors.Is(err, sandbox.ErrInvalidPolicy) {
		t.Fatalf("expected ErrInvalidPolicy for NUL path, got %v", err)
	}

	paths, err := sandbox.NormalizePaths([]string{"/tmp/a/../b", "/tmp/b"})
	if err != nil {
		t.Fatalf("NormalizePaths failed: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/tmp/b" {
		t.Fatalf("unexpected normalized paths: %v", paths)
	}

	var empty sandbox.Policy
	if err := empty.Validate(); !errors.Is(err, sandbox.ErrEmptyPolicy) {
		t.Fatalf("expected ErrEmptyPolicy, got %v", err)
	}

	valid := sandbox.Policy{
		ReadOnly: []string{"/usr"},
		Env:      []string{"FOO=bar"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid policy, got %v", err)
	}

	badEnv := sandbox.Policy{
		ReadOnly: []string{"/usr"},
		Env:      []string{"INVALID_ENV_ENTRY"},
	}
	if err := badEnv.Validate(); !errors.Is(err, sandbox.ErrInvalidPolicy) {
		t.Fatalf("expected ErrInvalidPolicy for invalid env entry, got %v", err)
	}

	for _, forbidden := range sandbox.ForbiddenExecNames {
		p := sandbox.Policy{
			ReadOnly: []string{"/"},
			Exec:     []string{"/usr/local/bin/" + forbidden},
		}
		if err := p.Validate(); !errors.Is(err, sandbox.ErrForbiddenExec) {
			t.Fatalf("expected ErrForbiddenExec for %q, got %v", forbidden, err)
		}
	}

	for _, interp := range []string{
		"/lib64/ld-linux-x86-64.so.2",
		"/usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
		"/lib/ld-linux-aarch64.so.1",
		"/lib/ld-musl-x86-64.so.1",
		"/lib/ld.so",
	} {
		p := sandbox.Policy{
			ReadOnly: []string{"/"},
			Exec:     []string{interp},
		}
		if err := p.Validate(); !errors.Is(err, sandbox.ErrForbiddenExec) {
			t.Fatalf("expected ErrForbiddenExec for ELF interpreter %q, got %v", interp, err)
		}
	}
}

func TestBuildExecWhitelistAndReadOnlyReviewerPolicy(t *testing.T) {
	tmp := t.TempDir()

	// 在临时 PATH 目录中放置允许的工具与禁止的二进制/动态链接器（含符号链接）
	allowedTool := filepath.Join(tmp, "allowed-tool")
	if err := os.WriteFile(allowedTool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write allowedTool: %v", err)
	}
	for _, name := range append(append([]string{}, sandbox.ForbiddenExecNames...), "ld-linux-x86-64.so.2") {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	symlinkToAgy := filepath.Join(tmp, "sneaky-link")
	if err := os.Symlink(filepath.Join(tmp, "agy"), symlinkToAgy); err != nil {
		t.Fatalf("symlink sneaky-link: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	whitelist := sandbox.BuildExecWhitelist([]string{
		"allowed-tool",
		"pig",
		"pi",
		"agy",
		"grok",
		"dispatch-agent",
		"ld-linux-x86-64.so.2",
		symlinkToAgy,
	})

	foundAllowed := false
	for _, ep := range whitelist {
		if ep == allowedTool {
			foundAllowed = true
		}
		if sandbox.IsForbiddenExec(ep) {
			t.Fatalf("whitelist unexpectedly contained forbidden binary: %s", ep)
		}
		if sandbox.IsELFInterpreter(ep) {
			t.Fatalf("whitelist unexpectedly contained ELF interpreter: %s", ep)
		}
	}
	if !foundAllowed {
		t.Fatalf("expected %s in whitelist, got %v", allowedTool, whitelist)
	}

	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	policy := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() {
		cleanupReviewerTempDirs(policy)
	})

	if err := policy.Validate(); err != nil {
		t.Fatalf("ReadOnlyReviewer Validate failed: %v", err)
	}

	// 验证 <workdir>/reports 已创建且在 ReadWrite 列表中
	reportDir := filepath.Join(workDir, sandbox.ReviewerReportSubdir)
	if st, err := os.Stat(reportDir); err != nil || !st.IsDir() {
		t.Fatalf("expected report dir %s to exist, err=%v", reportDir, err)
	}

	// 验证 W^X：ReadOnlyReviewer 下没有任何 ReadWrite 目录同时出现在 Exec 白名单中（也不存在父子目录交叠）
	for _, rw := range policy.ReadWrite {
		normRW, err := sandbox.NormalizePath(rw)
		if err != nil {
			t.Fatalf("NormalizePath rw %q: %v", rw, err)
		}
		for _, ep := range policy.Exec {
			normEP, err := sandbox.NormalizePath(ep)
			if err != nil {
				t.Fatalf("NormalizePath exec %q: %v", ep, err)
			}
			if normRW == normEP ||
				strings.HasPrefix(normEP, normRW+string(filepath.Separator)) ||
				strings.HasPrefix(normRW, normEP+string(filepath.Separator)) {
				t.Fatalf("W^X violation in ReadOnlyReviewer: ReadWrite path %q overlaps with Exec path %q", normRW, normEP)
			}
		}
	}

	// 验证 Env 中包含 TMPDIR、GOTMPDIR、GOCACHE，且均位于不可执行的私有临时目录下
	envMap := parseEnvMap(policy.Env)
	for _, key := range []string{"TMPDIR", "GOTMPDIR", "GOCACHE"} {
		val := envMap[key]
		if val == "" {
			t.Fatalf("expected %s in policy.Env, got %v", key, policy.Env)
		}
		if st, err := os.Stat(val); err != nil || !st.IsDir() {
			t.Fatalf("expected %s directory %q to exist, err=%v", key, val, err)
		}
	}
}

func TestBestEffortDegradation(t *testing.T) {
	if _, ok := sandbox.Supported(); ok {
		// 当内核支持 Landlock 时，测试不存在的路径在 BestEffort 模式下仅输出告警而不报错
		var warnBuf bytes.Buffer
		p := sandbox.Policy{
			ReadOnly:   []string{"/", "/non-existent-path-for-pi-sandbox-test"},
			BestEffort: true,
			WarnWriter: &warnBuf,
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate failed: %v", err)
		}
		return
	}

	var warnBuf bytes.Buffer
	p := sandbox.Policy{
		ReadOnly:   []string{"/"},
		Env:        []string{"PI_SANDBOX_BEST_EFFORT_TEST=1"},
		BestEffort: true,
		WarnWriter: &warnBuf,
	}
	if err := sandbox.Apply(p); err != nil {
		t.Fatalf("expected BestEffort Apply to succeed when Landlock unsupported, got %v", err)
	}
	if os.Getenv("PI_SANDBOX_BEST_EFFORT_TEST") != "1" {
		t.Fatalf("expected Policy.Env to be applied in BestEffort mode")
	}
}

func requireLandlock(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("skipping integration test: Landlock requires Linux (current GOOS=%s)", runtime.GOOS)
	}
	abi, ok := sandbox.Supported()
	if !ok {
		t.Skip("skipping integration test: Landlock is not supported or enabled on this kernel")
	}
	t.Logf("Landlock supported on host kernel (ABI v%d)", abi)
}

func TestLandlockReadOnlyVsReadWrite(t *testing.T) {
	requireLandlock(t)

	tmp := t.TempDir()
	roDir := filepath.Join(tmp, "ro")
	rwDir := filepath.Join(tmp, "rw")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	if err := os.MkdirAll(rwDir, 0o755); err != nil {
		t.Fatalf("mkdir rw: %v", err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testSubprocEnv+"=ro_vs_rw",
		"TEST_RO_DIR="+roDir,
		"TEST_RW_DIR="+rwDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ro_vs_rw subprocess failed: %v\noutput:\n%s", err, string(out))
	}
}

func subprocROvsRW() {
	roDir := os.Getenv("TEST_RO_DIR")
	rwDir := os.Getenv("TEST_RW_DIR")

	p := sandbox.Policy{
		ReadOnly:  []string{"/usr", "/lib", "/lib64", "/bin", "/etc", "/proc", roDir},
		ReadWrite: []string{rwDir},
	}
	if err := sandbox.Apply(p); err != nil {
		fmt.Fprintf(os.Stderr, "Apply failed: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.ReadFile(filepath.Join(roDir, "hello.txt")); err != nil {
		fmt.Fprintf(os.Stderr, "expected reading roDir/hello.txt to succeed, got: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(filepath.Join(roDir, "forbidden.txt"), []byte("x"), 0o644); err == nil {
		fmt.Fprintln(os.Stderr, "expected writing to roDir to fail, but it succeeded")
		os.Exit(1)
	}

	if err := os.WriteFile(filepath.Join(rwDir, "allowed.txt"), []byte("ok"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "expected writing to rwDir to succeed, got: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func TestLandlockExecWhitelistAndLdLinuxBlock(t *testing.T) {
	requireLandlock(t)

	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("skipping: 'true' binary not found in PATH: %v", err)
	}
	trueBin, err = filepath.EvalSymlinks(trueBin)
	if err != nil {
		t.Fatalf("EvalSymlinks(true): %v", err)
	}
	trueData, err := os.ReadFile(trueBin)
	if err != nil {
		t.Fatalf("read %s: %v", trueBin, err)
	}

	tmp := t.TempDir()
	allowedBin := filepath.Join(tmp, "allowed-true")
	deniedBin := filepath.Join(tmp, "denied-true")
	fakeAgy := filepath.Join(tmp, "agy")
	fakePig := filepath.Join(tmp, "pig")

	for _, dst := range []string{allowedBin, deniedBin, fakeAgy, fakePig} {
		if err := os.WriteFile(dst, trueData, 0o755); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testSubprocEnv+"=exec_whitelist",
		"TEST_BIN_DIR="+tmp,
		"TEST_ALLOWED_BIN="+allowedBin,
		"TEST_DENIED_BIN="+deniedBin,
		"TEST_FAKE_AGY="+fakeAgy,
		"TEST_FAKE_PIG="+fakePig,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exec_whitelist subprocess failed: %v\noutput:\n%s", err, string(out))
	}
}

func subprocExecWhitelist() {
	binDir := os.Getenv("TEST_BIN_DIR")
	allowedBin := os.Getenv("TEST_ALLOWED_BIN")
	deniedBin := os.Getenv("TEST_DENIED_BIN")
	fakeAgy := os.Getenv("TEST_FAKE_AGY")
	fakePig := os.Getenv("TEST_FAKE_PIG")

	p := sandbox.Policy{
		ReadOnly: []string{"/usr", "/lib", "/lib64", "/bin", "/etc", "/proc", binDir},
		Exec:     []string{allowedBin},
	}
	if err := sandbox.Apply(p); err != nil {
		fmt.Fprintf(os.Stderr, "Apply failed: %v\n", err)
		os.Exit(1)
	}

	// 1. 白名单内的二进制必须能正常执行
	if err := exec.Command(allowedBin).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "expected whitelisted binary %s to succeed, got: %v\n", allowedBin, err)
		os.Exit(1)
	}

	// 2. 不在白名单内的二进制直接执行必须被拒绝
	for _, blocked := range []string{deniedBin, fakeAgy, fakePig} {
		if err := exec.Command(blocked).Run(); err == nil {
			fmt.Fprintf(os.Stderr, "expected non-whitelisted binary %s to fail, but it succeeded\n", blocked)
			os.Exit(1)
		}
	}

	// 3. 通过 ld-linux 直接加载不在白名单内的 ELF（如 agy / denied-true）必须被拒绝
	if ldLinux := findHostELFInterpreter(); ldLinux != "" {
		for _, target := range []string{deniedBin, fakeAgy, fakePig} {
			if err := exec.Command(ldLinux, target).Run(); err == nil {
				fmt.Fprintf(os.Stderr, "SECURITY BREACH: %s %s succeeded under Exec whitelist!\n", ldLinux, target)
				os.Exit(1)
			}
		}
	}

	os.Exit(0)
}

func TestLandlockSeccompGuardFailureHandling(t *testing.T) {
	requireLandlock(t)

	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("skipping: 'true' binary not found in PATH: %v", err)
	}
	trueBin, err = filepath.EvalSymlinks(trueBin)
	if err != nil {
		t.Fatalf("EvalSymlinks(true): %v", err)
	}

	tmp := t.TempDir()
	roDir := filepath.Join(tmp, "ro")
	rwDir := filepath.Join(tmp, "rw")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	if err := os.MkdirAll(rwDir, 0o755); err != nil {
		t.Fatalf("mkdir rw: %v", err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testSubprocEnv+"=seccomp_failure",
		"TEST_RO_DIR="+roDir,
		"TEST_RW_DIR="+rwDir,
		"TEST_ALLOWED_BIN="+trueBin,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seccomp_failure subprocess failed: %v\noutput:\n%s", err, string(out))
	}
}

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

func subprocSeccompFailure() {
	roDir := os.Getenv("TEST_RO_DIR")
	rwDir := os.Getenv("TEST_RW_DIR")
	allowedBin := os.Getenv("TEST_ALLOWED_BIN")

	// 预先占用当前线程/进程的唯一 SECCOMP_FILTER_FLAG_NEW_LISTENER 槽位，
	// 使后续 sandbox.Apply 安装 seccomp 守卫时必定触发内核 EBUSY 错误。
	dummyListenerFD, err := occupySeccompListenerSlot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "occupySeccompListenerSlot failed: %v\n", err)
		os.Exit(1)
	}
	defer unix.Close(dummyListenerFD)

	// 1. 非 BestEffort 模式下，seccomp 安装失败必须直接返回错误（包装 ErrSeccomp），且不应用环境变量
	strictPolicy := sandbox.Policy{
		ReadOnly:   []string{"/usr", "/lib", "/lib64", "/bin", "/etc", "/proc", roDir},
		ReadWrite:  []string{rwDir},
		Exec:       []string{allowedBin},
		Env:        []string{"PI_SECCOMP_STRICT_SHOULD_NOT_APPLY=1"},
		BestEffort: false,
	}
	if err := sandbox.Apply(strictPolicy); err == nil {
		fmt.Fprintln(os.Stderr, "expected sandbox.Apply to fail when seccomp installation fails in non-BestEffort mode, got nil")
		os.Exit(1)
	} else if !errors.Is(err, sandbox.ErrSeccomp) {
		fmt.Fprintf(os.Stderr, "expected error wrapping ErrSeccomp, got: %v\n", err)
		os.Exit(1)
	}
	if os.Getenv("PI_SECCOMP_STRICT_SHOULD_NOT_APPLY") != "" {
		fmt.Fprintln(os.Stderr, "expected strictPolicy.Env not to be applied when Apply returns error")
		os.Exit(1)
	}

	// 2. BestEffort 模式下，seccomp 安装失败应输出告警并降级继续执行 Landlock 限制
	var warnBuf bytes.Buffer
	bestEffortPolicy := strictPolicy.WithBestEffort(true)
	bestEffortPolicy.WarnWriter = &warnBuf
	bestEffortPolicy.Env = []string{"PI_SECCOMP_BEST_EFFORT_APPLIED=1"}

	if err := sandbox.Apply(bestEffortPolicy); err != nil {
		fmt.Fprintf(os.Stderr, "expected BestEffort sandbox.Apply to degrade and succeed on seccomp failure, got: %v\n", err)
		os.Exit(1)
	}
	if !strings.Contains(warnBuf.String(), "seccomp") {
		fmt.Fprintf(os.Stderr, "expected BestEffort warning to mention seccomp, got: %q\n", warnBuf.String())
	}
	if os.Getenv("PI_SECCOMP_BEST_EFFORT_APPLIED") != "1" {
		fmt.Fprintln(os.Stderr, "expected BestEffort Env to be applied")
		os.Exit(1)
	}
	// 验证 Landlock 文件系统只读规则已生效
	if err := os.WriteFile(filepath.Join(roDir, "blocked.txt"), []byte("x"), 0o644); err == nil {
		fmt.Fprintln(os.Stderr, "expected write to roDir to be blocked after BestEffort degradation")
		os.Exit(1)
	}

	os.Exit(0)
}

func TestLandlockReviewerPresetSecurity(t *testing.T) {
	requireLandlock(t)

	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("skipping: 'true' binary not found in PATH: %v", err)
	}
	trueBin, err = filepath.EvalSymlinks(trueBin)
	if err != nil {
		t.Fatalf("EvalSymlinks(true): %v", err)
	}
	elfBytes, err := os.ReadFile(trueBin)
	if err != nil {
		t.Fatalf("read %s: %v", trueBin, err)
	}

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	// 构造模拟的 /root/.local/bin/{agy,pi,pig,grok,dispatch-agent} 与任意可读 ELF
	localBinDir := filepath.Join(tmp, "local-bin")
	if err := os.MkdirAll(localBinDir, 0o755); err != nil {
		t.Fatalf("mkdir local-bin: %v", err)
	}
	fakeAgy := filepath.Join(localBinDir, "agy")
	fakePi := filepath.Join(localBinDir, "pi")
	fakePig := filepath.Join(localBinDir, "pig")
	fakeDispatch := filepath.Join(localBinDir, "dispatch-agent")
	arbitraryELF := filepath.Join(localBinDir, "unlisted-elf")
	for _, dst := range []string{fakeAgy, fakePi, fakePig, fakeDispatch, arbitraryELF} {
		if err := os.WriteFile(dst, elfBytes, 0o755); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testSubprocEnv+"=reviewer_preset",
		"TEST_WORKDIR="+workDir,
		"TEST_FAKE_AGY="+fakeAgy,
		"TEST_FAKE_PI="+fakePi,
		"TEST_FAKE_PIG="+fakePig,
		"TEST_FAKE_DISPATCH="+fakeDispatch,
		"TEST_ARBITRARY_ELF="+arbitraryELF,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reviewer_preset subprocess failed: %v\noutput:\n%s", err, string(out))
	}
}

func subprocReviewerPreset() {
	workDir := os.Getenv("TEST_WORKDIR")
	fakeAgy := os.Getenv("TEST_FAKE_AGY")
	fakePi := os.Getenv("TEST_FAKE_PI")
	fakePig := os.Getenv("TEST_FAKE_PIG")
	fakeDispatch := os.Getenv("TEST_FAKE_DISPATCH")
	arbitraryELF := os.Getenv("TEST_ARBITRARY_ELF")

	policy := sandbox.ReadOnlyReviewer(workDir)
	if err := sandbox.Apply(policy); err != nil {
		fmt.Fprintf(os.Stderr, "Apply(ReadOnlyReviewer) failed: %v\n", err)
		os.Exit(1)
	}

	// 1. 代码仓库只读，只有 <workdir>/reports 可写
	if _, err := os.ReadFile(filepath.Join(workDir, "README.md")); err != nil {
		fmt.Fprintf(os.Stderr, "reading workDir/README.md failed: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(workDir, "hacked.txt"), []byte("no"), 0o644); err == nil {
		fmt.Fprintln(os.Stderr, "expected write to workDir root to fail, but it succeeded")
		os.Exit(1)
	}
	reportPath := filepath.Join(workDir, sandbox.ReviewerReportSubdir, "audit.md")
	if err := os.WriteFile(reportPath, []byte("ok\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "expected write to reports dir to succeed, got: %v\n", err)
		os.Exit(1)
	}

	// 2. 把 agy 或任意可读 ELF 拷进所有可写目录（TMPDIR、GOTMPDIR、GOCACHE、reports）后再直接执行，必须被拒绝
	elfData, err := os.ReadFile(fakeAgy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "expected fakeAgy (%s) to be readable, got: %v\n", fakeAgy, err)
		os.Exit(1)
	}
	writableDirs := []string{
		os.Getenv("TMPDIR"),
		os.Getenv("GOTMPDIR"),
		os.Getenv("GOCACHE"),
		filepath.Join(workDir, sandbox.ReviewerReportSubdir),
	}
	var copiedPaths []string
	for _, dir := range writableDirs {
		if dir == "" {
			fmt.Fprintln(os.Stderr, "missing expected writable temp env dir")
			os.Exit(1)
		}
		for _, baseName := range []string{"agy", "copied-elf"} {
			dst := filepath.Join(dir, baseName)
			if err := os.WriteFile(dst, elfData, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "failed to copy ELF into writable dir %s: %v\n", dst, err)
				os.Exit(1)
			}
			copiedPaths = append(copiedPaths, dst)
			if err := exec.Command(dst).Run(); err == nil {
				fmt.Fprintf(os.Stderr, "SECURITY BREACH: copied ELF %s in writable dir %s executed!\n", dst, dir)
				os.Exit(1)
			}
		}
	}

	// 3. 尝试向任何可执行目录（含受控的 goExecDir）写入或拷贝 ELF，必须被拒绝
	for _, ep := range policy.Exec {
		if st, err := os.Stat(ep); err == nil && st.IsDir() {
			probe := filepath.Join(ep, "injected-elf")
			if err := os.WriteFile(probe, elfData, 0o755); err == nil {
				fmt.Fprintf(os.Stderr, "SECURITY BREACH: able to write %s into executable dir %s!\n", probe, ep)
				os.Exit(1)
			}
		}
	}

	// 4. 用 ld-linux 直接加载不在白名单的 ELF（含原路径的 agy/pi/pig/dispatch-agent/unlisted-elf 及拷入临时目录的副本），必须被拒绝
	if ldLinux := findHostELFInterpreter(); ldLinux != "" {
		targets := append([]string{fakeAgy, fakePi, fakePig, fakeDispatch, arbitraryELF}, copiedPaths...)
		for _, target := range targets {
			if err := exec.Command(ldLinux, target).Run(); err == nil {
				fmt.Fprintf(os.Stderr, "SECURITY BREACH: ld-linux (%s) executed non-whitelisted ELF %s!\n", ldLinux, target)
				os.Exit(1)
			}
		}
	}

	os.Exit(0)
}

func TestLandlockReviewerGoToolchain(t *testing.T) {
	requireLandlock(t)

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("skipping integration test: 'go' toolchain not found in PATH: %v", err)
	}

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "gomod")
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "demo"), 0o755); err != nil {
		t.Fatalf("mkdir gomod: %v", err)
	}

	goMod := "module example.com/reviewcheck\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	libGo := `package reviewcheck

func Sum(a, b int) int {
	return a + b
}
`
	if err := os.WriteFile(filepath.Join(workDir, "sum.go"), []byte(libGo), 0o644); err != nil {
		t.Fatalf("write sum.go: %v", err)
	}

	libTestGo := `package reviewcheck

import "testing"

func TestSum(t *testing.T) {
	if got := Sum(20, 22); got != 42 {
		t.Fatalf("Sum(20, 22) = %d, want 42", got)
	}
}
`
	if err := os.WriteFile(filepath.Join(workDir, "sum_test.go"), []byte(libTestGo), 0o644); err != nil {
		t.Fatalf("write sum_test.go: %v", err)
	}

	mainGo := `package main

import (
	"fmt"
	"example.com/reviewcheck"
)

func main() {
	fmt.Println(reviewcheck.Sum(1, 2))
}
`
	if err := os.WriteFile(filepath.Join(workDir, "cmd", "demo", "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatalf("write cmd/demo/main.go: %v", err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testSubprocEnv+"=reviewer_go_toolchain",
		"TEST_WORKDIR="+workDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reviewer_go_toolchain subprocess failed: %v\noutput:\n%s", err, string(out))
	}
}

func subprocReviewerGoToolchain() {
	workDir := os.Getenv("TEST_WORKDIR")

	policy := sandbox.ReadOnlyReviewer(workDir)
	if err := sandbox.Apply(policy); err != nil {
		fmt.Fprintf(os.Stderr, "Apply(ReadOnlyReviewer) failed: %v\n", err)
		os.Exit(1)
	}

	if err := os.Chdir(workDir); err != nil {
		fmt.Fprintf(os.Stderr, "chdir %s failed: %v\n", workDir, err)
		os.Exit(1)
	}

	// 1. go build ./... 必须正常通过
	if out, err := exec.Command("go", "build", "./...").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build ./... failed: %v\noutput:\n%s\n", err, string(out))
		os.Exit(1)
	}

	// 2. go vet ./... 必须正常通过
	if out, err := exec.Command("go", "vet", "./...").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go vet ./... failed: %v\noutput:\n%s\n", err, string(out))
		os.Exit(1)
	}

	// 3. go test ./... 必须正常编译并执行测试二进制通过
	if out, err := exec.Command("go", "test", "-v", "./...").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go test -v ./... failed: %v\noutput:\n%s\n", err, string(out))
		os.Exit(1)
	}

	// 4. 若用 go build -o 将二进制输出到可写的 TMPDIR，编译本身允许，但执行该产物必须被拒绝
	builtInTmp := filepath.Join(os.Getenv("TMPDIR"), "demo-bin")
	if out, err := exec.Command("go", "build", "-o", builtInTmp, "./cmd/demo").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build -o %s ./cmd/demo failed: %v\noutput:\n%s\n", builtInTmp, err, string(out))
		os.Exit(1)
	}
	if err := exec.Command(builtInTmp).Run(); err == nil {
		fmt.Fprintf(os.Stderr, "SECURITY BREACH: executing go build output %s inside TMPDIR succeeded!\n", builtInTmp)
		os.Exit(1)
	}

	// 5. 试图通过 go run、go test -exec、go test -toolexec、go test -c 绕过受控执行的行为必须被拒绝
	for _, forbiddenArgs := range [][]string{
		{"run", "./cmd/demo"},
		{"test", "-exec=/bin/sh", "./..."},
		{"test", "-toolexec=/bin/sh", "./..."},
		{"test", "-c", "./..."},
	} {
		if out, err := exec.Command("go", forbiddenArgs...).CombinedOutput(); err == nil {
			fmt.Fprintf(os.Stderr, "SECURITY BREACH: go %v unexpectedly succeeded:\n%s\n", forbiddenArgs, string(out))
			os.Exit(1)
		}
	}

	os.Exit(0)
}

func findHostELFInterpreter() string {
	for _, cand := range []string{
		"/lib64/ld-linux-x86-64.so.2",
		"/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
		"/usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
		"/lib/ld-linux-aarch64.so.1",
		"/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1",
		"/usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1",
	} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

func parseEnvMap(envList []string) map[string]string {
	m := make(map[string]string, len(envList))
	for _, kv := range envList {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func cleanupReviewerTempDirs(p sandbox.Policy) {
	envMap := parseEnvMap(p.Env)
	if tmpDir := envMap["TMPDIR"]; strings.HasPrefix(tmpDir, "/tmp/pi-reviewer-") {
		_ = os.RemoveAll(tmpDir)
	}
	if execDir := envMap["PI_SANDBOX_GO_EXEC_DIR"]; strings.HasPrefix(execDir, "/tmp/pi-reviewer-exec-") {
		_ = os.RemoveAll(execDir)
	}
}

func TestReviewerAgentPreset(t *testing.T) {
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() {
		cleanupReviewerTempDirs(policy)
	})

	if err := policy.Validate(); err != nil {
		t.Fatalf("ReviewerAgent Validate failed: %v", err)
	}
	if len(policy.EntryAgents) == 0 {
		t.Fatal("expected EntryAgents to be populated when agy/pig exist on host")
	}

	// 原 reviewer 不含的辅助工具应在 Exec 中
	foundMkdir := false
	for _, ep := range policy.Exec {
		base := filepath.Base(ep)
		if base == "mkdir" || base == "cp" || base == "touch" {
			foundMkdir = true
			break
		}
	}
	if !foundMkdir {
		t.Fatalf("expected mkdir/cp/touch in ReviewerAgent Exec, got %v", policy.Exec)
	}

	foundSession := false
	for _, rw := range policy.ReadWrite {
		if rw == "/tmp/herdr-role-sessions" {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatal("expected /tmp/herdr-role-sessions in ReadWrite")
	}

	// 对照：原 ReadOnlyReviewer 仍拒绝把 ForbiddenExec 放进 Exec，且无 EntryAgents
	ro := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(ro) })
	if len(ro.EntryAgents) != 0 {
		t.Fatalf("ReadOnlyReviewer must keep EntryAgents empty, got %v", ro.EntryAgents)
	}
	for _, ep := range ro.Exec {
		if sandbox.IsForbiddenExec(ep) {
			t.Fatalf("ReadOnlyReviewer Exec unexpectedly contains forbidden %s", ep)
		}
	}
	if err := ro.Validate(); err != nil {
		t.Fatalf("ReadOnlyReviewer Validate failed: %v", err)
	}

	// EntryAgents 路径允许通过 Validate；dispatch-agent 仍拒绝
	bad := sandbox.Policy{
		ReadOnly:    []string{"/"},
		Exec:        []string{"/usr/local/bin/dispatch-agent"},
		EntryAgents: policy.EntryAgents,
	}
	if err := bad.Validate(); !errors.Is(err, sandbox.ErrForbiddenExec) {
		t.Fatalf("expected ErrForbiddenExec for dispatch-agent even with EntryAgents, got %v", err)
	}
}

func TestReviewerAgentEntryExecIntegration(t *testing.T) {
	requireLandlock(t)

	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("skipping: true not found: %v", err)
	}
	trueData, err := os.ReadFile(trueBin)
	if err != nil {
		t.Fatalf("read true: %v", err)
	}

	tmp := t.TempDir()
	fakeAgy := filepath.Join(tmp, "agy")
	if err := os.WriteFile(fakeAgy, trueData, 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	denied := filepath.Join(tmp, "dispatch-agent")
	if err := os.WriteFile(denied, trueData, 0o755); err != nil {
		t.Fatalf("write fake dispatch-agent: %v", err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testSubprocEnv+"=reviewer_agent_entry",
		"TEST_BIN_DIR="+tmp,
		"TEST_FAKE_AGY="+fakeAgy,
		"TEST_FAKE_DISPATCH="+denied,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reviewer_agent_entry subprocess failed: %v\noutput:\n%s", err, string(out))
	}
}

func subprocReviewerAgentEntry() {
	binDir := os.Getenv("TEST_BIN_DIR")
	fakeAgy := os.Getenv("TEST_FAKE_AGY")
	fakeDispatch := os.Getenv("TEST_FAKE_DISPATCH")

	p := sandbox.Policy{
		ReadOnly:    []string{"/usr", "/lib", "/lib64", "/bin", "/etc", "/proc", binDir},
		Exec:        []string{fakeAgy},
		EntryAgents: []string{fakeAgy},
	}
	if err := sandbox.Apply(p); err != nil {
		fmt.Fprintf(os.Stderr, "Apply failed: %v\n", err)
		os.Exit(1)
	}

	if err := exec.Command(fakeAgy).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "expected EntryAgent agy to run, got: %v\n", err)
		os.Exit(1)
	}
	if err := exec.Command(fakeDispatch).Run(); err == nil {
		fmt.Fprintln(os.Stderr, "expected dispatch-agent to be denied, but it succeeded")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestIsEntryAgentPath(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-agy")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(tmp, "agy")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	agents := []string{link}
	if !sandbox.IsEntryAgentPath(agents, link) {
		t.Fatal("expected link path to match EntryAgents")
	}
	if !sandbox.IsEntryAgentPath(agents, real) {
		t.Fatal("expected resolved real path to match EntryAgents")
	}
	if !sandbox.IsEntryAgentPath(agents, "agy") {
		t.Fatal("expected basename agy to match EntryAgents")
	}
	if sandbox.IsEntryAgentPath(agents, "dispatch-agent") {
		t.Fatal("dispatch-agent must not match")
	}
}
