package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const integrationEnvKey = "PI_SANDBOX_INTEGRATION_CASE"

func TestNormalizePath(t *testing.T) {
	t.Parallel()

	if _, err := NormalizePath(""); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NormalizePath empty: got %v, want ErrInvalidPolicy", err)
	}
	if _, err := NormalizePath("/tmp/bad\x00path"); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NormalizePath NUL: got %v, want ErrInvalidPolicy", err)
	}

	got, err := NormalizePath("/usr/local/../bin/./git")
	if err != nil {
		t.Fatalf("NormalizePath valid path: %v", err)
	}
	if got != "/usr/bin/git" {
		t.Fatalf("NormalizePath clean: got %q, want %q", got, "/usr/bin/git")
	}

	rel, err := NormalizePath("./testdata/sub/..")
	if err != nil {
		t.Fatalf("NormalizePath relative: %v", err)
	}
	if !filepath.IsAbs(rel) {
		t.Fatalf("expected absolute path, got %q", rel)
	}

	list, err := NormalizePaths([]string{"/tmp/a", "/tmp/b/../a", "/tmp/b"})
	if err != nil {
		t.Fatalf("NormalizePaths: %v", err)
	}
	if len(list) != 2 || list[0] != "/tmp/a" || list[1] != "/tmp/b" {
		t.Fatalf("NormalizePaths dedup: got %v, want [/tmp/a /tmp/b]", list)
	}
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()

	var empty Policy
	if err := empty.Validate(); !errors.Is(err, ErrEmptyPolicy) {
		t.Fatalf("empty policy: got %v, want ErrEmptyPolicy", err)
	}

	denyNetOnly := Policy{DenyNet: true}
	if err := denyNetOnly.Validate(); err != nil {
		t.Fatalf("DenyNet-only policy should be valid, got %v", err)
	}

	badRO := Policy{ReadOnly: []string{""}}
	if err := badRO.Validate(); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("bad ReadOnly policy: got %v, want ErrInvalidPolicy", err)
	}

	badEnvNoEq := Policy{ReadOnly: []string{"/"}, Env: []string{"INVALID_ENV"}}
	if err := badEnvNoEq.Validate(); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("bad Env policy (missing '='): got %v, want ErrInvalidPolicy", err)
	}

	badEnvNUL := Policy{ReadOnly: []string{"/"}, Env: []string{"TMPDIR=/tmp\x00bad"}}
	if err := badEnvNUL.Validate(); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("bad Env policy (NUL byte): got %v, want ErrInvalidPolicy", err)
	}

	for _, forbidden := range ForbiddenExecNames {
		p := Policy{
			ReadOnly: []string{"/"},
			Exec:     []string{filepath.Join("/usr/local/bin", forbidden)},
		}
		if err := p.Validate(); !errors.Is(err, ErrForbiddenExec) {
			t.Fatalf("expected ErrForbiddenExec for %q, got %v", forbidden, err)
		}
	}
}

func TestBuildExecWhitelistAndReviewerPreset(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	allowedTool := filepath.Join(tmpDir, "my-safe-tool")
	if err := os.WriteFile(allowedTool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write allowedTool: %v", err)
	}

	forbiddenPaths := make([]string, 0, len(ForbiddenExecNames))
	for _, name := range ForbiddenExecNames {
		fp := filepath.Join(tmpDir, name)
		if err := os.WriteFile(fp, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write forbidden %s: %v", name, err)
		}
		forbiddenPaths = append(forbiddenPaths, fp)
	}

	// 构造指向禁止执行器的符号链接，验证符号链接穿透拦截
	symlinkToPig := filepath.Join(tmpDir, "harmless-alias")
	if err := os.Symlink(filepath.Join(tmpDir, "pig"), symlinkToPig); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	inputs := append([]string{allowedTool, symlinkToPig}, forbiddenPaths...)
	whitelist := BuildExecWhitelist(inputs)

	for _, w := range whitelist {
		base := filepath.Base(w)
		if IsForbiddenExec(w) || base == "harmless-alias" {
			t.Fatalf("forbidden tool leaked into whitelist: %s", w)
		}
	}
	foundAllowed := false
	for _, w := range whitelist {
		if w == allowedTool {
			foundAllowed = true
		}
	}
	if !foundAllowed {
		t.Fatalf("expected %s in whitelist, got %v", allowedTool, whitelist)
	}

	workdir := t.TempDir()
	reviewerPolicy := ReadOnlyReviewer(workdir)
	for _, rw := range reviewerPolicy.ReadWrite {
		if strings.HasPrefix(rw, "/tmp/") {
			t.Cleanup(func() { _ = os.RemoveAll(rw) })
		}
	}
	if err := reviewerPolicy.Validate(); err != nil {
		t.Fatalf("ReadOnlyReviewer validate: %v", err)
	}
	if len(reviewerPolicy.ReadOnly) != 1 || reviewerPolicy.ReadOnly[0] != "/" {
		t.Fatalf("ReadOnlyReviewer ReadOnly: got %v, want [/]", reviewerPolicy.ReadOnly)
	}
	if len(reviewerPolicy.ReadWrite) != 2 {
		t.Fatalf("ReadOnlyReviewer ReadWrite count: got %d (%v), want 2", len(reviewerPolicy.ReadWrite), reviewerPolicy.ReadWrite)
	}
	wantReportDir := filepath.Join(workdir, ReviewerReportSubdir)
	if reviewerPolicy.ReadWrite[0] != wantReportDir {
		t.Fatalf("ReadOnlyReviewer ReadWrite[0]: got %q, want %q", reviewerPolicy.ReadWrite[0], wantReportDir)
	}
	privateTmp := reviewerPolicy.ReadWrite[1]
	wantEnv := map[string]string{
		"TMPDIR":   privateTmp,
		"GOTMPDIR": filepath.Join(privateTmp, "go-tmp"),
		"GOCACHE":  filepath.Join(privateTmp, "go-cache"),
	}
	for _, kv := range reviewerPolicy.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("invalid env entry %q", kv)
		}
		if expected, exists := wantEnv[k]; exists && v == expected {
			delete(wantEnv, k)
		}
	}
	if len(wantEnv) != 0 {
		t.Fatalf("missing expected env vars from ReadOnlyReviewer: %v (got %v)", wantEnv, reviewerPolicy.Env)
	}
	for _, ep := range reviewerPolicy.Exec {
		if IsForbiddenExec(ep) {
			t.Fatalf("ReadOnlyReviewer Exec contains forbidden binary: %s", ep)
		}
	}
}

func TestBestEffortMissingPathWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := Policy{
		ReadOnly:   []string{"/"},
		BestEffort: true,
		WarnWriter: &buf,
	}
	p.warnf("test warning %d", 42)
	if !strings.Contains(buf.String(), "test warning 42") {
		t.Fatalf("unexpected warn output: %q", buf.String())
	}
}

func TestIntegrationLandlock(t *testing.T) {
	abi, ok := Supported()
	if !ok {
		t.Skip("Landlock is not supported or enabled on this kernel; skipping integration tests")
	}
	t.Logf("Detected kernel Landlock ABI version: %d", abi)

	t.Run("ReadOnlyAndReadWrite", func(t *testing.T) {
		roDir := t.TempDir()
		rwDir := t.TempDir()
		existingFile := filepath.Join(roDir, "existing.txt")
		if err := os.WriteFile(existingFile, []byte("readonly-content"), 0o644); err != nil {
			t.Fatalf("prepare ro file: %v", err)
		}
		runSubprocessCase(t, "fs-ro-rw", roDir, rwDir)
	})

	t.Run("ExecWhitelist", func(t *testing.T) {
		binDir := t.TempDir()
		rwDir := t.TempDir()

		srcBin, err := exec.LookPath("true")
		if err != nil {
			srcBin = "/bin/true"
		}
		resolvedSrc, err := filepath.EvalSymlinks(srcBin)
		if err == nil {
			srcBin = resolvedSrc
		}

		allowedBin := filepath.Join(binDir, "allowed-tool")
		deniedBin := filepath.Join(binDir, "unlisted-tool")
		pigBin := filepath.Join(binDir, "pig")

		for _, dst := range []string{allowedBin, deniedBin, pigBin} {
			if err := copyExecutable(srcBin, dst); err != nil {
				t.Fatalf("copyExecutable to %s: %v", dst, err)
			}
		}

		runSubprocessCase(t, "exec-whitelist", binDir, rwDir, allowedBin, deniedBin, pigBin)
	})

	t.Run("DenyNet", func(t *testing.T) {
		if abi < 4 {
			t.Skipf("Landlock ABI %d < 4 does not support TCP network restrictions", abi)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen tcp: %v", err)
		}
		defer ln.Close()

		rwDir := t.TempDir()
		runSubprocessCase(t, "deny-net", rwDir, ln.Addr().String())
	})

	t.Run("ReadOnlyReviewerPreset", func(t *testing.T) {
		workdir := t.TempDir()
		readme := filepath.Join(workdir, "README.md")
		if err := os.WriteFile(readme, []byte("# repo"), 0o644); err != nil {
			t.Fatalf("write README: %v", err)
		}

		srcBin, err := exec.LookPath("true")
		if err != nil {
			srcBin = "/bin/true"
		}
		if resolved, err := filepath.EvalSymlinks(srcBin); err == nil {
			srcBin = resolved
		}
		fakePig := filepath.Join(workdir, "pig")
		if err := copyExecutable(srcBin, fakePig); err != nil {
			t.Fatalf("copy fake pig: %v", err)
		}

		runSubprocessCase(t, "preset-reviewer", workdir, fakePig)
	})

	t.Run("ReadOnlyReviewerGoToolchain", func(t *testing.T) {
		if _, err := exec.LookPath("go"); err != nil {
			t.Skip("go binary not found in PATH; skipping Go toolchain reviewer integration test")
		}

		modDir := t.TempDir()
		goModContent := "module example.com/reviewercheck\n\ngo 1.22\n"
		if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goModContent), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		srcContent := "package reviewercheck\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
		if err := os.WriteFile(filepath.Join(modDir, "calc.go"), []byte(srcContent), 0o644); err != nil {
			t.Fatalf("write calc.go: %v", err)
		}
		testContent := "package reviewercheck\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Fatalf(\"Add(2, 3) = %d, want 5\", got)\n\t}\n}\n"
		if err := os.WriteFile(filepath.Join(modDir, "calc_test.go"), []byte(testContent), 0o644); err != nil {
			t.Fatalf("write calc_test.go: %v", err)
		}

		runSubprocessCase(t, "preset-reviewer-go", modDir)
	})
}

func runSubprocessCase(t *testing.T, caseName string, args ...string) {
	t.Helper()

	cmdArgs := append([]string{"-test.run=^TestIntegrationSubprocessHelper$", "--", caseName}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), integrationEnvKey+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess case %q failed: %v\noutput:\n%s", caseName, err, string(out))
	}
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o755)
}

func TestIntegrationSubprocessHelper(t *testing.T) {
	if os.Getenv(integrationEnvKey) != "1" {
		return
	}

	var extra []string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			extra = os.Args[i+1:]
			break
		}
	}
	if len(extra) == 0 {
		t.Fatal("missing subprocess case name")
	}

	caseName := extra[0]
	params := extra[1:]

	switch caseName {
	case "fs-ro-rw":
		if len(params) != 2 {
			t.Fatalf("fs-ro-rw expects 2 params, got %v", params)
		}
		subprocTestFSRORW(t, params[0], params[1])
	case "exec-whitelist":
		if len(params) != 5 {
			t.Fatalf("exec-whitelist expects 5 params, got %v", params)
		}
		subprocTestExecWhitelist(t, params[0], params[1], params[2], params[3], params[4])
	case "deny-net":
		if len(params) != 2 {
			t.Fatalf("deny-net expects 2 params, got %v", params)
		}
		subprocTestDenyNet(t, params[0], params[1])
	case "preset-reviewer":
		if len(params) != 2 {
			t.Fatalf("preset-reviewer expects 2 params, got %v", params)
		}
		subprocTestPresetReviewer(t, params[0], params[1])
	case "preset-reviewer-go":
		if len(params) != 1 {
			t.Fatalf("preset-reviewer-go expects 1 param, got %v", params)
		}
		subprocTestPresetReviewerGo(t, params[0])
	default:
		t.Fatalf("unknown subprocess case %q", caseName)
	}
}

func subprocTestFSRORW(t *testing.T, roDir, rwDir string) {
	p := Policy{
		ReadOnly:  []string{"/", roDir},
		ReadWrite: []string{rwDir},
	}
	if err := Apply(p); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// 1. 读取只读目录中的已有文件应当成功
	existing := filepath.Join(roDir, "existing.txt")
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "readonly-content" {
		t.Fatalf("read existing file in roDir: err=%v, data=%q", err, string(data))
	}

	// 2. (a) 写只读目录必须失败并返回 EACCES
	blockedFile := filepath.Join(roDir, "write-attempt.txt")
	err = os.WriteFile(blockedFile, []byte("blocked"), 0o644)
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES writing to roDir, got: %v", err)
	}

	// 截断只读目录中的已有文件同样必须失败并返回 EACCES
	f, err := os.OpenFile(existing, os.O_WRONLY|os.O_TRUNC, 0o644)
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES truncating file in roDir, got: %v", err)
	}

	// 删除只读目录中的已有文件必须失败并返回 EACCES
	if err := os.Remove(existing); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES removing file in roDir, got: %v", err)
	}

	// 3. (b) 写可写目录必须成功（创建、写入、读取、截断、删除）
	allowedFile := filepath.Join(rwDir, "allowed.txt")
	if err := os.WriteFile(allowedFile, []byte("hello-sandbox"), 0o644); err != nil {
		t.Fatalf("write to rwDir failed: %v", err)
	}
	got, err := os.ReadFile(allowedFile)
	if err != nil || string(got) != "hello-sandbox" {
		t.Fatalf("read back from rwDir failed: err=%v, got=%q", err, string(got))
	}
	if err := os.WriteFile(allowedFile, []byte("short"), 0o644); err != nil {
		t.Fatalf("overwrite/truncate in rwDir failed: %v", err)
	}
	if err := os.Remove(allowedFile); err != nil {
		t.Fatalf("remove file in rwDir failed: %v", err)
	}
}

func subprocTestExecWhitelist(t *testing.T, binDir, rwDir, allowedBin, deniedBin, pigBin string) {
	p := Policy{
		ReadOnly:  []string{"/", binDir},
		ReadWrite: []string{rwDir},
		Exec:      []string{allowedBin},
	}
	if err := Apply(p); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// 1. 执行白名单内的二进制必须成功
	if err := exec.Command(allowedBin).Run(); err != nil {
		t.Fatalf("exec whitelisted binary %s failed: %v", allowedBin, err)
	}

	// 2. (c) 执行非白名单二进制必须失败并返回 EACCES
	if err := exec.Command(deniedBin).Run(); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES executing non-whitelisted binary %s, got: %v", deniedBin, err)
	}

	// 3. 执行被禁止的 pig 二进制必须失败并返回 EACCES
	if err := exec.Command(pigBin).Run(); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES executing pig binary %s, got: %v", pigBin, err)
	}

	// 4. 即便把白名单二进制拷贝到可写目录 rwDir 并赋予 +x，也不可执行（W^X 保护）
	copiedInRW := filepath.Join(rwDir, "copied-exec")
	if err := copyExecutable(allowedBin, copiedInRW); err != nil {
		t.Fatalf("copy binary to rwDir: %v", err)
	}
	if err := exec.Command(copiedInRW).Run(); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES executing binary from rwDir %s, got: %v", copiedInRW, err)
	}
}

func subprocTestDenyNet(t *testing.T, rwDir, targetAddr string) {
	p := Policy{
		ReadOnly:  []string{"/"},
		ReadWrite: []string{rwDir},
		DenyNet:   true,
	}
	if err := Apply(p); err != nil {
		t.Fatalf("Apply DenyNet failed: %v", err)
	}

	// (d) DenyNet 时发起 TCP 连接必须失败并返回 EACCES
	conn, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES on TCP connect to %s, got: %v", targetAddr, err)
	}

	// 同时验证经典 TCP bind 也被拒绝并返回 EACCES（显式关闭 Go 1.24+ 默认的 MPTCP）
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if ln != nil {
		_ = ln.Close()
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES on classic TCP bind, got: %v", err)
	}
}

func subprocTestPresetReviewer(t *testing.T, workdir, fakePig string) {
	p := ReadOnlyReviewer(workdir)
	defer func() { _ = os.RemoveAll(p.ReadWrite[1]) }()

	if err := Apply(p); err != nil {
		t.Fatalf("Apply ReadOnlyReviewer failed: %v", err)
	}

	// 1. workdir 根目录下直接写文件必须被拒绝（EACCES）
	forbiddenWrite := filepath.Join(workdir, "tamper.txt")
	if err := os.WriteFile(forbiddenWrite, []byte("no"), 0o644); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES writing directly to workdir, got: %v", err)
	}

	// 2. workdir/reports 报告目录下写文件必须成功，但拷贝可执行文件到 reports 后不可执行（W^X）
	reportFile := filepath.Join(workdir, ReviewerReportSubdir, "audit-report.md")
	if err := os.WriteFile(reportFile, []byte(fmt.Sprintf("ok at %d", time.Now().UnixNano())), 0o644); err != nil {
		t.Fatalf("write to reviewer report dir failed: %v", err)
	}
	execInReport := filepath.Join(workdir, ReviewerReportSubdir, " sneaky-bin")
	if err := copyExecutable(fakePig, execInReport); err != nil {
		t.Fatalf("copy binary to report dir: %v", err)
	}
	if err := exec.Command(execInReport).Run(); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES executing binary inside reports dir, got: %v", err)
	}

	// 3. /tmp 私有子目录下写文件必须成功
	tmpNote := filepath.Join(p.ReadWrite[1], "scratch.txt")
	if err := os.WriteFile(tmpNote, []byte("scratch"), 0o600); err != nil {
		t.Fatalf("write to reviewer private /tmp dir failed: %v", err)
	}

	// 4. 启动 fakePig 必须被内核拦截（EACCES）
	if err := exec.Command(fakePig).Run(); !errors.Is(err, unix.EACCES) {
		t.Fatalf("expected EACCES running fake pig, got: %v", err)
	}
}

func subprocTestPresetReviewerGo(t *testing.T, modDir string) {
	p := ReadOnlyReviewer(modDir)
	defer func() { _ = os.RemoveAll(p.ReadWrite[1]) }()

	if err := Apply(p); err != nil {
		t.Fatalf("Apply ReadOnlyReviewer failed: %v", err)
	}

	// 验证 Apply 已将私有临时目录注入当前进程环境变量
	if got := os.Getenv("TMPDIR"); got != p.ReadWrite[1] {
		t.Fatalf("TMPDIR after Apply: got %q, want %q", got, p.ReadWrite[1])
	}

	// 1. 在 ReadOnlyReviewer 沙盒内运行 go vet ./... 必须成功
	vetCmd := exec.Command("go", "vet", "./...")
	vetCmd.Dir = modDir
	vetCmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := vetCmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet under ReadOnlyReviewer failed: %v\noutput:\n%s", err, string(out))
	}

	// 2. 在 ReadOnlyReviewer 沙盒内运行 go test ./...（编译并执行 GOTMPDIR 下的测试二进制）必须成功
	testCmd := exec.Command("go", "test", "-v", "./...")
	testCmd.Dir = modDir
	testCmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := testCmd.CombinedOutput(); err != nil {
		t.Fatalf("go test under ReadOnlyReviewer failed: %v\noutput:\n%s", err, string(out))
	}
}
