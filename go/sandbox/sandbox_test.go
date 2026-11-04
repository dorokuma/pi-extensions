package sandbox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

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

	// DefaultReviewerTools 含 ssh：ReadOnlyReviewer（--preset reviewer）的 Exec
	// 必须含宿主 ssh（含软链解析目标），使沙盒内可执行远端只读探查；
	// ForbiddenExecNames 绝不入 Exec。
	roExecSet := make(map[string]struct{}, len(policy.Exec)*2)
	for _, ep := range policy.Exec {
		roExecSet[ep] = struct{}{}
		if r, err := filepath.EvalSymlinks(ep); err == nil && r != "" {
			roExecSet[r] = struct{}{}
		}
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatalf("host ssh not found in PATH: %v", err)
	}
	if _, ok := roExecSet[sshBin]; !ok {
		t.Fatalf("ssh %s not in ReadOnlyReviewer Exec", sshBin)
	}
	if resolvedSSH, err := filepath.EvalSymlinks(sshBin); err == nil && resolvedSSH != "" {
		if _, ok := roExecSet[resolvedSSH]; !ok {
			t.Fatalf("resolved ssh %s not in ReadOnlyReviewer Exec", resolvedSSH)
		}
	}
	for _, ep := range policy.Exec {
		if sandbox.IsForbiddenExec(ep) {
			t.Fatalf("ReadOnlyReviewer Exec unexpectedly contains forbidden exec %s", ep)
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

// unsetEnv 保证 key 在本测试内不存在，并在 t.Cleanup 恢复原值（含原本未设置情形）。
// 代替裸 os.Unsetenv，避免污染环境且不还原。
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	orig, existed := os.LookupEnv(key)
	if existed {
		_ = os.Unsetenv(key)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestReviewerAgentPreset(t *testing.T) {
	// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))

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
	foundAgyProHome := false
	foundTTY := false
	foundBinRW := false
	for _, rw := range policy.ReadWrite {
		if rw == "/tmp/herdr-role-sessions" {
			foundSession = true
		}
		if strings.Contains(rw, "/root/.agy-pro-home/.gemini/antigravity-cli/") {
			foundAgyProHome = true
		}
		if rw == "/dev/tty" {
			foundTTY = true
		}
		if rw == "/root/.gemini/antigravity-cli/bin" || rw == "/root/.agy-pro-home/.gemini/antigravity-cli/bin" ||
			strings.HasPrefix(rw, "/root/.gemini/antigravity-cli/bin/") ||
			strings.HasPrefix(rw, "/root/.agy-pro-home/.gemini/antigravity-cli/bin/") {
			foundBinRW = true
		}
	}
	if !foundSession {
		t.Fatal("expected /tmp/herdr-role-sessions in ReadWrite")
	}
	if !foundAgyProHome {
		t.Fatalf("expected /root/.agy-pro-home/.gemini/antigravity-cli in ReadWrite, got %v", policy.ReadWrite)
	}
	if !foundTTY {
		t.Fatal("expected /dev/tty in ReviewerAgent ReadWrite")
	}
	if foundBinRW {
		t.Fatal("antigravity-cli/bin must not be in ReadWrite (W^X)")
	}

	// 四层私有隔离取代宿主锁目录放宽：宿主敏感目录不得出现在 ReadWrite，
	// cells/go 目录本身不得同时处于 RW∩Exec（0.2.2 的 W^X 例外已移除；
	// .locks 子目录可写、已构建 runner 产物可执行的分离策略由
	// TestReviewerAgentCellCachePolicy 专门断言）。
	for _, rw := range policy.ReadWrite {
		if rw == "/root/.pig" || rw == "/root" {
			t.Fatalf("must not grant whole /root/.pig or /root in ReadWrite: %q", rw)
		}
		if rw == "/root/.pig/agent" || strings.HasPrefix(rw, "/root/.pig/agent/") {
			t.Fatalf("host /root/.pig/agent must stay read-only, got ReadWrite %q", rw)
		}
		if rw == "/run/user/0" || strings.HasPrefix(rw, "/run/user/0/") {
			t.Fatalf("host /run/user/0 must not be writable, got ReadWrite %q", rw)
		}
	}
	rwSetCheck := make(map[string]struct{}, len(policy.ReadWrite))
	for _, rw := range policy.ReadWrite {
		rwSetCheck[rw] = struct{}{}
	}
	for _, ep := range policy.Exec {
		if ep == "/root/.pig/cache/cells/go" {
			if _, ok := rwSetCheck[ep]; ok {
				t.Fatalf("cells/go must not be simultaneously ReadWrite+Exec (W^X): %q", ep)
			}
			t.Fatalf("cells/go dir itself must not be in Exec (per-artifact Exec only): %q", ep)
		}
		if ep == "/root/.pig/agent" || ep == "/run/user/0/pig" {
			t.Fatalf("host sensitive dir must not be in Exec: %q", ep)
		}
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

func TestReviewerAgentKeepsRootNarrow(t *testing.T) {
	// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })

	for _, rw := range policy.ReadWrite {
		if rw == "/root" || rw == "/" || rw == "/root/.pig" {
			t.Fatalf("ReadWrite must not widen to %q", rw)
		}
	}
	// ReadOnlyReviewer 不得获得 /dev/tty、agy-pro-home 或 pig 锁写权限
	ro := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(ro) })
	for _, rw := range ro.ReadWrite {
		if rw == "/dev/tty" {
			t.Fatal("ReadOnlyReviewer must not grant /dev/tty")
		}
		if strings.Contains(rw, ".agy-pro-home") {
			t.Fatalf("ReadOnlyReviewer must not grant agy-pro-home RW: %s", rw)
		}
		if rw == "/root/.pig/agent" || strings.Contains(rw, "/root/.pig/cache/") {
			t.Fatalf("ReadOnlyReviewer must not grant pig lock RW: %s", rw)
		}
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

// TestReviewerAgentAgyHooks 回归：ReviewerAgent 必须以最小权限放行 agy 生命周期钩子
// （herdsman/herdsman-go 本体 + agy-rules-hook/moshi-hook/agy-recover-400/agy-recall，
// 含符号链接目标），且不放宽 ReadOnlyReviewer、不把 /root/.herdsman 加入 ReadWrite、
// 保持 W^X（Exec 与 ReadWrite 无父子目录交叠）。
func TestReviewerAgentAgyHooks(t *testing.T) {
	// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })

	if err := policy.Validate(); err != nil {
		t.Fatalf("ReviewerAgent Validate failed: %v", err)
	}

	// Exec 集合：规范化 + 符号链接解析两份都进 set，模拟 Landlock 按路径授予时对软链与目标的放行。
	execSet := make(map[string]struct{}, len(policy.Exec)*2)
	for _, ep := range policy.Exec {
		execSet[ep] = struct{}{}
		if r, err := filepath.EvalSymlinks(ep); err == nil && r != "" {
			execSet[r] = struct{}{}
		}
	}

	// 宿主机真实存在的钩子二进制必须能解析进 Exec；不存在的直接 fail（防 silently skip）。
	hookBins := []string{
		"/root/.local/bin/herdsman",
		"/root/.local/bin/agy-rules-hook",
		"/root/.local/bin/moshi-hook",
		"/root/.local/bin/agy-recover-400",
		"/root/.local/bin/agy-recall",
	}
	for _, hb := range hookBins {
		if _, err := os.Stat(hb); err != nil {
			t.Fatalf("host hook binary %s missing: %v", hb, err)
		}
		if _, ok := execSet[hb]; !ok {
			t.Fatalf("hook %s not in ReviewerAgent Exec", hb)
		}
	}
	// herdsman 包装脚本 exec 的 ELF 本体也必须在 Exec 中。
	herdsmanGo := "/root/.local/lib/herdsman-go/herdsman"
	if _, err := os.Stat(herdsmanGo); err != nil {
		t.Fatalf("herdsman-go ELF %s missing: %v", herdsmanGo, err)
	}
	if _, ok := execSet[herdsmanGo]; !ok {
		t.Fatalf("herdsman-go ELF %s not in ReviewerAgent Exec", herdsmanGo)
	}

	// 不得把 /root/.local/bin 整个目录放进 Exec（会连带放行 herdr/dispatch-agent）。
	for _, ep := range policy.Exec {
		if ep == "/root/.local/bin" || ep == "/root/.local/bin/" {
			t.Fatal("ReviewerAgent must not Exec-allow whole /root/.local/bin")
		}
		base := filepath.Base(ep)
		if base == "herdr" || base == "dispatch-agent" || base == "grok" {
			t.Fatalf("ReviewerAgent Exec unexpectedly contains %s", ep)
		}
	}

	// /root/.herdsman 保持只读，绝不进 ReadWrite（W^X + 最小权限）。
	for _, rw := range policy.ReadWrite {
		if strings.Contains(rw, "/.herdsman") {
			t.Fatalf("ReviewerAgent must not grant ReadWrite to %s", rw)
		}
	}

	// W^X：任何 ReadWrite 路径不得与 Exec 路径相同或互为父子目录。
	for _, rw := range policy.ReadWrite {
		normRW, err := sandbox.NormalizePath(rw)
		if err != nil {
			t.Fatalf("NormalizePath rw %q: %v", rw, err)
		}
		for ep := range execSet {
			normEP, err := sandbox.NormalizePath(ep)
			if err != nil {
				continue
			}
			if normRW == normEP ||
				strings.HasPrefix(normEP, normRW+string(filepath.Separator)) ||
				strings.HasPrefix(normRW, normEP+string(filepath.Separator)) {
				t.Fatalf("W^X violation: ReadWrite %q overlaps Exec %q", normRW, normEP)
			}
		}
	}

	// ReadOnlyReviewer 行为不变：不新增钩子、无 agy-pro-home/herdsman Exec。
	ro := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(ro) })
	roSet := make(map[string]struct{}, len(ro.Exec))
	for _, ep := range ro.Exec {
		roSet[ep] = struct{}{}
	}
	for _, hb := range hookBins {
		if _, ok := roSet[hb]; ok {
			t.Fatalf("ReadOnlyReviewer Exec unexpectedly contains hook %s", hb)
		}
	}
	if _, ok := roSet[herdsmanGo]; ok {
		t.Fatal("ReadOnlyReviewer Exec unexpectedly contains herdsman-go ELF")
	}
}

// TestReviewerAgentMCPServerExec 安全细则一回归：ReviewerAgent 必须放行精确的
// MCP 服务二进制（ctxmode / codegraph-go / jev-mcp / jev-mcp-go / agy-recall，含软链目标），
// 仍不整目录放行 ~/.local/bin，且 dispatch-agent / dispatch-agent-go / herdr / grok 不进 Exec。
func TestReviewerAgentMCPServerExec(t *testing.T) {
	// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
	if err := policy.Validate(); err != nil {
		t.Fatalf("ReviewerAgent Validate failed: %v", err)
	}

	execSet := make(map[string]struct{}, len(policy.Exec)*2)
	for _, ep := range policy.Exec {
		execSet[ep] = struct{}{}
		if r, err := filepath.EvalSymlinks(ep); err == nil && r != "" {
			execSet[r] = struct{}{}
		}
	}
	for _, name := range []string{
		"ctxmode", "codegraph-go", "jev-mcp", "jev-mcp-go", "agy-recall",
	} {
		hostPath := filepath.Join("/root/.local/bin", name)
		if _, err := os.Stat(hostPath); err != nil {
			t.Fatalf("host MCP binary %s missing: %v", hostPath, err)
		}
		if _, ok := execSet[hostPath]; !ok {
			t.Fatalf("MCP binary %s not in ReviewerAgent Exec", hostPath)
		}
	}

	// ReviewerAgent 继承 DefaultReviewerTools：宿主 ssh 必须解析进 Exec（含软链目标），
	// 使沙盒内可执行远端只读探查。（EntryAgents agy/pig/pi 按预设设计仍属顶层放行。）
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatalf("host ssh not found in PATH: %v", err)
	}
	if _, ok := execSet[sshBin]; !ok {
		t.Fatalf("ssh %s not in ReviewerAgent Exec", sshBin)
	}
	if resolvedSSH, err := filepath.EvalSymlinks(sshBin); err == nil && resolvedSSH != "" {
		if _, ok := execSet[resolvedSSH]; !ok {
			t.Fatalf("resolved ssh %s not in ReviewerAgent Exec", resolvedSSH)
		}
	}
	for _, ep := range policy.Exec {
		if ep == "/root/.local/bin" || ep == "/root/.local/bin/" {
			t.Fatal("ReviewerAgent must not Exec-allow whole /root/.local/bin")
		}
		if st, err := os.Stat(ep); err == nil && st.IsDir() && (ep == "/root/.local" || ep == "/root") {
			t.Fatalf("ReviewerAgent must not Exec-allow dir %s", ep)
		}
		switch filepath.Base(ep) {
		case "herdr", "dispatch-agent", "dispatch-agent-go", "grok":
			t.Fatalf("ReviewerAgent Exec unexpectedly contains %s", ep)
		}
	}
}

// TestReviewerAgentCtxmodeIsolation 安全细则一回归：CTXMODE_DB 未显式指定时注入沙盒私有
// 数据库路径；显式指定时保留调用方值。宿主兜底只允许 ~/.local/share/ctxmode 与
// agy-recall state；~/.local/share 与 ~/.local/share/gemini-web 绝不出现在 ReadWrite。
func TestReviewerAgentCtxmodeIsolation(t *testing.T) {
	// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	t.Run("default private db", func(t *testing.T) {
		unsetEnv(t, "CTXMODE_DB")
		policy := sandbox.ReviewerAgent(workDir)
		t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
		envMap := parseEnvMap(policy.Env)
		db := envMap["CTXMODE_DB"]
		if db == "" {
			t.Fatalf("expected injected CTXMODE_DB, env=%v", policy.Env)
		}
		if !strings.HasPrefix(db, "/tmp/pi-reviewer-") || !strings.HasSuffix(db, "/ctxmode/context_mode.db") {
			t.Fatalf("unexpected CTXMODE_DB %q", db)
		}
		dir := filepath.Dir(db)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("ctxmode private dir %s missing: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("ctxmode private dir mode = %o, want 0700", got)
		}
	})

	t.Run("explicit CTXMODE_DB preserved", func(t *testing.T) {
		t.Setenv("CTXMODE_DB", "/custom/ctx.db")
		policy := sandbox.ReviewerAgent(workDir)
		t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
		if got := parseEnvMap(policy.Env)["CTXMODE_DB"]; got != "" {
			t.Fatalf("CTXMODE_DB must not be overridden, got %q", got)
		}
	})

	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
	for _, rw := range policy.ReadWrite {
		if rw == "/root/.local/share" || strings.HasPrefix(rw, "/root/.local/share/gemini-web") ||
			strings.Contains(rw, "chrome-profile") {
			t.Fatalf("ReadWrite must not widen to %q", rw)
		}
	}
	foundCtxmodeShare := false
	foundRecallState := false
	for _, rw := range policy.ReadWrite {
		if rw == "/root/.local/share/ctxmode" {
			foundCtxmodeShare = true
		}
		if strings.HasSuffix(rw, "/plugins/agy-recall/state") {
			foundRecallState = true
		}
	}
	if !foundCtxmodeShare {
		t.Fatalf("expected /root/.local/share/ctxmode in ReadWrite, got %v", policy.ReadWrite)
	}
	if !foundRecallState {
		t.Fatalf("expected agy-recall state dir in ReadWrite, got %v", policy.ReadWrite)
	}

	// 对照：ReadOnlyReviewer 不得注入 CTXMODE_DB、不得获得这些放宽。
	ro := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(ro) })
	if got := parseEnvMap(ro.Env)["CTXMODE_DB"]; got != "" {
		t.Fatalf("ReadOnlyReviewer must not inject CTXMODE_DB, got %q", got)
	}
	for _, rw := range ro.ReadWrite {
		if strings.Contains(rw, ".local/share") || strings.Contains(rw, "agy-recall") {
			t.Fatalf("ReadOnlyReviewer must not grant %s", rw)
		}
	}
}

// TestReviewerAgentPigAgentDirIsolation 安全细则二回归：宿主 /root/.pig/agent 保持 100% 只读；
// 私有 <privateTmpDir>/pig-agent 只含白名单配置文件（0600），settings.json 注入宿主
// 扩展目录绝对路径（不再创建 extensions 软链，保证 packed-go cell.Key 与宿主一致命中缓存）；
// PIG_CODING_AGENT_DIR 未显式指定时注入私有目录，显式指定时保留。
func TestReviewerAgentPigAgentDirIsolation(t *testing.T) {
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	t.Run("default injection", func(t *testing.T) {
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		// SBX-S3：完整隔离 fakePigHome，绝不触碰宿主真实 /root/.pig/agent。
		fakePigHome := filepath.Join(t.TempDir(), "fakepig")
		hostAgent := filepath.Join(fakePigHome, "agent")
		hostExt := filepath.Join(hostAgent, "extensions")
		if err := os.MkdirAll(hostExt, 0o755); err != nil {
			t.Fatalf("mkdir fake extensions: %v", err)
		}
		if err := os.WriteFile(filepath.Join(hostExt, "demo.ts"), []byte("// demo\n"), 0o644); err != nil {
			t.Fatalf("write fake extension file: %v", err)
		}
		srcSettings := `{"theme":"dark","customKey":42}`
		for name, content := range map[string]string{
			"settings.json":    srcSettings,
			"models.json":      "{}",
			"auth.json":        "{}",
			"keybindings.json": "{}",
		} {
			if err := os.WriteFile(filepath.Join(hostAgent, name), []byte(content), 0o600); err != nil {
				t.Fatalf("write fake %s: %v", name, err)
			}
		}
		if err := os.MkdirAll(filepath.Join(hostAgent, "sessions"), 0o755); err != nil {
			t.Fatalf("mkdir fake sessions: %v", err)
		}
		if err := os.WriteFile(filepath.Join(hostAgent, "sessions", "dummy.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write fake sessions/dummy.jsonl: %v", err)
		}
		t.Setenv("PIG_HOME", fakePigHome)

		policy := sandbox.ReviewerAgent(workDir)
		t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
		envMap := parseEnvMap(policy.Env)
		dst := envMap["PIG_CODING_AGENT_DIR"]
		if dst == "" {
			t.Fatalf("expected injected PIG_CODING_AGENT_DIR, env=%v", policy.Env)
		}
		if !strings.HasPrefix(dst, "/tmp/pi-reviewer-") || !strings.HasSuffix(dst, "/pig-agent") {
			t.Fatalf("unexpected PIG_CODING_AGENT_DIR %q", dst)
		}
		info, err := os.Stat(dst)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("private pig-agent dir %s bad state: %v mode=%v", dst, err, info.Mode())
		}
		// 4 个白名单配置文件 0600 拷入；sessions/.lock 等不拷。
		for _, name := range []string{"settings.json", "models.json", "auth.json", "keybindings.json"} {
			dstFile := filepath.Join(dst, name)
			fi, err := os.Stat(dstFile)
			if err != nil {
				t.Fatalf("expected copied %s: %v", dstFile, err)
			}
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Fatalf("%s mode = %o, want 0600", dstFile, got)
			}
		}
		if _, err := os.Stat(filepath.Join(dst, "sessions")); !os.IsNotExist(err) {
			t.Fatal("private pig-agent must not contain sessions")
		}
		// 不再创建 extensions 软链：软链会让自动发现的 cfg.Source 指向随机私有
		// 路径，packed-go cellHash 随之变化，永久脱靶宿主打包缓存。
		if _, err := os.Lstat(filepath.Join(dst, "extensions")); !os.IsNotExist(err) {
			t.Fatalf("private pig-agent must not contain extensions entry (symlink or dir), err=%v", err)
		}
		// 私有 settings.json 必须显式声明宿主扩展目录绝对路径，并保留原有键值。
		data, err := os.ReadFile(filepath.Join(dst, "settings.json"))
		if err != nil {
			t.Fatalf("expected private settings.json: %v", err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("private settings.json not a JSON object: %v", err)
		}
		rawExt, ok := obj["extensions"]
		if !ok {
			t.Fatal("private settings.json missing injected extensions entry")
		}
		var extEntries []string
		if err := json.Unmarshal(rawExt, &extEntries); err != nil || len(extEntries) == 0 {
			t.Fatalf("extensions entry must be a non-empty string array: %s err=%v", rawExt, err)
		}
		found := false
		for _, e := range extEntries {
			if e == hostExt {
				found = true
			}
		}
		if !found {
			t.Fatalf("extensions entry %v does not contain host extensions dir %s", extEntries, hostExt)
		}
		// 宿主原有键值逐项保留（theme/customKey 不因注入丢失或改写）。
		var srcObj map[string]json.RawMessage
		srcData, err := os.ReadFile(filepath.Join(hostAgent, "settings.json"))
		if err != nil {
			t.Fatalf("read fake host settings.json: %v", err)
		}
		if err := json.Unmarshal(srcData, &srcObj); err != nil {
			t.Fatalf("fake host settings.json not a JSON object: %v", err)
		}
		for key, srcVal := range srcObj {
			dstVal, ok := obj[key]
			if !ok {
				t.Fatalf("private settings.json lost host key %q", key)
			}
			var srcNorm, dstNorm any
			if json.Unmarshal(srcVal, &srcNorm) == nil && json.Unmarshal(dstVal, &dstNorm) == nil {
				if !reflect.DeepEqual(srcNorm, dstNorm) {
					t.Fatalf("private settings.json key %q changed: %s -> %s", key, srcVal, dstVal)
				}
			}
		}
	})

	t.Run("explicit PIG_CODING_AGENT_DIR preserved", func(t *testing.T) {
		// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
		t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))
		t.Setenv("PIG_CODING_AGENT_DIR", "/custom/agent")
		policy := sandbox.ReviewerAgent(workDir)
		t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
		if got := parseEnvMap(policy.Env)["PIG_CODING_AGENT_DIR"]; got != "" {
			t.Fatalf("PIG_CODING_AGENT_DIR must not be overridden, got %q", got)
		}
	})

	t.Run("custom host extensions preserved", func(t *testing.T) {
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		fakeHome := filepath.Join(tmp, "fakepig")
		agentDir := filepath.Join(fakeHome, "agent")
		if err := os.MkdirAll(filepath.Join(agentDir, "extensions"), 0o755); err != nil {
			t.Fatalf("mkdir fake extensions: %v", err)
		}
		src := `{"theme":"dark","extensions":["/custom/ext-a","./rel-ext"]}`
		if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(src), 0o600); err != nil {
			t.Fatalf("write fake settings.json: %v", err)
		}
		t.Setenv("PIG_HOME", fakeHome)
		policy := sandbox.ReviewerAgent(workDir)
		t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
		dst := parseEnvMap(policy.Env)["PIG_CODING_AGENT_DIR"]
		if dst == "" {
			t.Fatal("expected injected PIG_CODING_AGENT_DIR")
		}
		if _, err := os.Lstat(filepath.Join(dst, "extensions")); !os.IsNotExist(err) {
			t.Fatalf("private pig-agent must not contain extensions entry, err=%v", err)
		}
		data, err := os.ReadFile(filepath.Join(dst, "settings.json"))
		if err != nil {
			t.Fatalf("expected private settings.json: %v", err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("private settings.json not a JSON object: %v", err)
		}
		var extEntries []string
		if err := json.Unmarshal(obj["extensions"], &extEntries); err != nil {
			t.Fatalf("extensions entry must stay a string array: %v", err)
		}
		if !reflect.DeepEqual(extEntries, []string{"/custom/ext-a", filepath.Join(agentDir, "rel-ext"), filepath.Join(agentDir, "extensions")}) {
			t.Fatalf("relative extension must resolve against host agent dir and host ext dir must be appended, got %v", extEntries)
		}
		var theme string
		if err := json.Unmarshal(obj["theme"], &theme); err != nil || theme != "dark" {
			t.Fatalf("host theme must be preserved, got %s err=%v", obj["theme"], err)
		}
	})

	// 宿主 AgentDir 绝不进 ReadWrite（含 PIG_HOME 解析形式）。
	// SBX-S3：此处仅断言策略路径构成，隔离 PIG_HOME 为通用空目录即可，
	// 不再依赖宿主 /root/.pig/agent 的真实布局。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
	for _, rw := range policy.ReadWrite {
		if rw == "/root/.pig" || rw == "/root/.pig/agent" || strings.HasPrefix(rw, "/root/.pig/agent/") {
			t.Fatalf("host pig agent dir must stay read-only, ReadWrite=%q", rw)
		}
		if strings.Contains(rw, "pig-agent") && !strings.HasPrefix(rw, "/tmp/pi-reviewer-") {
			t.Fatalf("unexpected pig-agent ReadWrite %q", rw)
		}
	}
}

// TestReviewerAgentCellCachePolicy 安全细则三回归（SBX-M2/SBX-S3）：在
// t.TempDir() 下自建完整隔离 fakePigHome（PIG_HOME 指向它，零触碰宿主
// /root/.pig/cache），构造过期与新鲜 usage.json 各一，验证宿主态仅刷新过期
// usage.json（fresh <5m 只读短路不重写）；仅 .locks 目录与 toolchains 锁文件
// 目录进 ReadWrite，已构建 runner/产物进 Exec，且 ReadWrite 与 Exec 之间不存在
// 任何相等或父子包含关系（W^X）。
func TestReviewerAgentCellCachePolicy(t *testing.T) {
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	// 完整隔离 fakePigHome：cells/go/<digest>/runner + ext/demo-<digest2>/ext.js
	// 两个有效缓存单元（0o755 非空普通文件 + 匹配文件大小的 ready.json）与
	// toolchains/go.json 记录。
	fakePigHome := filepath.Join(tmp, "fakepig")
	cacheRoot := filepath.Join(fakePigHome, "cache")
	goCellDir := filepath.Join(cacheRoot, "cells", "go", "018957e3099bb082ccfa2203f7c3e681e56cbc86f8c748d8193125177940f18c")
	extCellDir := filepath.Join(cacheRoot, "ext", "demo-56cbc86f8c748d8193125177940f18c018957e3099bb082ccfa2203f7c3e681")
	toolchainsDir := filepath.Join(cacheRoot, "toolchains")
	for _, d := range []string{goCellDir, extCellDir, toolchainsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	writeCell := func(dir, artifact string, lastUsed int64) {
		t.Helper()
		artifactPath := filepath.Join(dir, artifact)
		data := []byte("#!/bin/sh\nexit 0\n")
		if err := os.WriteFile(artifactPath, data, 0o755); err != nil {
			t.Fatalf("write artifact %s: %v", artifactPath, err)
		}
		ready := map[string]any{
			"inputDigest": strings.TrimPrefix(filepath.Base(dir), "demo-"),
			"artifact":    artifact,
			"size":        int64(len(data)),
			"created":     time.Now().Unix(),
		}
		readyData, err := json.Marshal(ready)
		if err != nil {
			t.Fatalf("marshal ready.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ready.json"), readyData, 0o644); err != nil {
			t.Fatalf("write ready.json: %v", err)
		}
		usageData, err := json.Marshal(struct {
			LastUsed int64 `json:"lastUsed"`
		}{LastUsed: lastUsed})
		if err != nil {
			t.Fatalf("marshal usage.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "usage.json"), usageData, 0o644); err != nil {
			t.Fatalf("write usage.json: %v", err)
		}
	}
	// 过期单元：lastUsed=1，宿主态必须原子刷新为当前时间。
	writeCell(goCellDir, "runner", 1)
	// 新鲜单元：lastUsed=now-1m，命中 <5m 只读短路，绝不重写。
	freshLastUsed := time.Now().Add(-1 * time.Minute).Unix()
	writeCell(extCellDir, "ext.js", freshLastUsed)

	if err := os.WriteFile(filepath.Join(toolchainsDir, "go.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write toolchains/go.json: %v", err)
	}

	staleBefore := time.Now().Unix()
	t.Setenv("PIG_HOME", fakePigHome)
	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })
	if err := policy.Validate(); err != nil {
		t.Fatalf("ReviewerAgent Validate failed: %v", err)
	}

	readLastUsed := func(dir string) int64 {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "usage.json"))
		if err != nil {
			t.Fatalf("usage.json missing in %s: %v", dir, err)
		}
		var usage struct {
			LastUsed int64 `json:"lastUsed"`
		}
		if err := json.Unmarshal(data, &usage); err != nil {
			t.Fatalf("usage.json malformed in %s: %v", dir, err)
		}
		return usage.LastUsed
	}

	// 过期 usage.json 已刷新（>= 调用前基线），新鲜 usage.json 保持原值未被重写。
	if got := readLastUsed(goCellDir); got < staleBefore {
		t.Fatalf("stale usage.json not refreshed: lastUsed=%d < %d", got, staleBefore)
	}
	if got := readLastUsed(extCellDir); got != freshLastUsed {
		t.Fatalf("fresh usage.json must not be rewritten (<5m 短路): lastUsed=%d, want %d", got, freshLastUsed)
	}

	// 锁目录与 toolchains 锁文件目录在 ReadWrite，缓存单元目录不在 ReadWrite。
	rwSet := make(map[string]struct{}, len(policy.ReadWrite))
	for _, rw := range policy.ReadWrite {
		rwSet[rw] = struct{}{}
	}
	for _, dir := range []string{
		filepath.Join(cacheRoot, "cells", "go", ".locks"),
		filepath.Join(cacheRoot, "ext", ".locks"),
		toolchainsDir,
	} {
		if _, ok := rwSet[dir]; !ok {
			t.Fatalf("expected %s in ReadWrite, got %v", dir, policy.ReadWrite)
		}
	}
	for _, dir := range []string{
		filepath.Join(cacheRoot, "cells"),
		filepath.Join(cacheRoot, "cells", "go"),
		filepath.Join(cacheRoot, "ext"),
		goCellDir,
		extCellDir,
	} {
		if _, ok := rwSet[dir]; ok {
			t.Fatalf("%s must not be ReadWrite", dir)
		}
	}

	// 已构建 runner 与 ext.js 产物在 Exec。
	execSet := make(map[string]struct{}, len(policy.Exec)*2)
	for _, ep := range policy.Exec {
		execSet[ep] = struct{}{}
		if r, err := filepath.EvalSymlinks(ep); err == nil && r != "" {
			execSet[r] = struct{}{}
		}
	}
	for _, want := range []string{
		filepath.Join(goCellDir, "runner"),
		filepath.Join(extCellDir, "ext.js"),
	} {
		if _, ok := execSet[want]; !ok {
			t.Fatalf("expected cached artifact %s in Exec, got %v", want, policy.Exec)
		}
	}

	// W^X：ReadWrite 与 Exec 之间不存在任何相等或父子包含关系。
	for _, rw := range policy.ReadWrite {
		normRW, err := sandbox.NormalizePath(rw)
		if err != nil {
			t.Fatalf("NormalizePath rw %q: %v", rw, err)
		}
		for ep := range execSet {
			normEP, err := sandbox.NormalizePath(ep)
			if err != nil {
				continue
			}
			if normRW == normEP ||
				strings.HasPrefix(normEP, normRW+string(filepath.Separator)) ||
				strings.HasPrefix(normRW, normEP+string(filepath.Separator)) {
				t.Fatalf("W^X violation: ReadWrite %q overlaps Exec %q", normRW, normEP)
			}
		}
	}

	// 对照：ReadOnlyReviewer 不得放行 .locks、缓存产物或任何 fakePigHome 下路径。
	ro := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(ro) })
	for _, rw := range ro.ReadWrite {
		if strings.Contains(rw, fakePigHome) || strings.Contains(rw, "/.pig/cache") {
			t.Fatalf("ReadOnlyReviewer must not grant %s", rw)
		}
	}
	for _, ep := range ro.Exec {
		if strings.Contains(ep, fakePigHome) || strings.Contains(ep, "/.pig/cache") {
			t.Fatalf("ReadOnlyReviewer must not Exec %s", ep)
		}
	}
}

// TestReviewerAgentPrivateXDGRuntimeDir 安全细则四回归：宿主环境预设
// XDG_RUNTIME_DIR=/run/user/0 时，ReviewerAgent 产出的合成 Env 中
// XDG_RUNTIME_DIR 必须指向 <privateTmpDir>/rt（0700，目录已预建），
// 且宿主值 /run/user/0 不残留（sbx mergeEnv 按同名键覆盖，无同名双键）。
// 私有目录在 privateTmpDir 内，已由既有 ReadWrite 覆盖，无需额外放行；
// 宿主 /run/user/0（含 systemd user bus/keyring 套接字）绝不进 ReadWrite。
// 对照：ReadOnlyReviewer 契约不变，不注入 XDG_RUNTIME_DIR。
func TestReviewerAgentPrivateXDGRuntimeDir(t *testing.T) {
	// SBX-S3：隔离 PIG_HOME，ReviewerAgent 不得触碰宿主 /root/.pig/cache。
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))

	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	t.Setenv("XDG_RUNTIME_DIR", "/run/user/0")
	policy := sandbox.ReviewerAgent(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(policy) })

	envMap := parseEnvMap(policy.Env)
	privateTmp := envMap["TMPDIR"]
	if !strings.HasPrefix(privateTmp, "/tmp/pi-reviewer-") {
		t.Fatalf("unexpected TMPDIR %q", privateTmp)
	}
	rt := envMap["XDG_RUNTIME_DIR"]
	want := filepath.Join(privateTmp, "rt")
	if rt != want {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", rt, want)
	}

	// Env 中不得残留宿主值，也不得有同名双键。
	count := 0
	for _, kv := range policy.Env {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			count++
			if kv != "XDG_RUNTIME_DIR="+want {
				t.Fatalf("unexpected XDG_RUNTIME_DIR entry %q", kv)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one XDG_RUNTIME_DIR entry, got %d in %v", count, policy.Env)
	}

	// 私有运行目录已预建且权限严格 0700。
	info, err := os.Stat(rt)
	if err != nil {
		t.Fatalf("private runtime dir %s must exist: %v", rt, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("private runtime dir %s bad state: mode=%v", rt, info.Mode())
	}

	// 宿主 /run/user/0 不得出现在任何 ReadWrite（敏感套接字目录）。
	for _, rw := range policy.ReadWrite {
		if rw == "/run/user/0" || strings.HasPrefix(rw, "/run/user/0/") || strings.HasPrefix(rw, "/run/user/") {
			t.Fatalf("host XDG_RUNTIME_DIR must not be writable, ReadWrite=%q", rw)
		}
	}
	// 私有 rt 被 privateTmpDir 的可写规则覆盖，无需单独放行。
	foundParentRW := false
	for _, rw := range policy.ReadWrite {
		if rw == privateTmp {
			foundParentRW = true
		}
		if rw == rt || strings.HasPrefix(rw, rt+string(filepath.Separator)) {
			t.Fatalf("rt must stay under privateTmpDir coverage, unexpected ReadWrite %q", rw)
		}
	}
	if !foundParentRW {
		t.Fatalf("expected ReadWrite to contain privateTmpDir %q, got %v", privateTmp, policy.ReadWrite)
	}

	// 对照：ReadOnlyReviewer 契约不变——不注入 XDG_RUNTIME_DIR，宿主值由
	// os.Environ() 原样透传（该预设不放行代理，无 socket 写入需求）。
	ro := sandbox.ReadOnlyReviewer(workDir)
	t.Cleanup(func() { cleanupReviewerTempDirs(ro) })
	if got := parseEnvMap(ro.Env)["XDG_RUNTIME_DIR"]; got != "" {
		t.Fatalf("ReadOnlyReviewer must not inject XDG_RUNTIME_DIR, got %q", got)
	}
}
