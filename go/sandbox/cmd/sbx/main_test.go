package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const envTestWantSbxMain = "GO_WANT_SBX_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(envSbxChildSpecKey) != "" {
		os.Exit(runChildFromEnv())
	}
	if os.Getenv(envTestWantSbxMain) == "1" {
		_ = os.Unsetenv(envTestWantSbxMain)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestParseArgsBasicAndReviewerAlias(t *testing.T) {
	wd := t.TempDir()

	cfg, err := parseArgs([]string{"--reviewer", wd, "--", "ls", "-la"})
	if err != nil {
		t.Fatalf("parseArgs(--reviewer) unexpected error: %v", err)
	}
	if cfg.Preset != "reviewer" {
		t.Fatalf("Preset = %q, want %q", cfg.Preset, "reviewer")
	}
	if cfg.WorkDir != wd {
		t.Fatalf("WorkDir = %q, want %q", cfg.WorkDir, wd)
	}
	if len(cfg.Command) != 2 || cfg.Command[0] != "ls" || cfg.Command[1] != "-la" {
		t.Fatalf("Command = %v, want [ls -la]", cfg.Command)
	}

	if _, err := parseArgs([]string{"--reviewer", wd, "--preset", "reviewer-agent", "--", "true"}); err == nil {
		t.Fatal("expected conflict error between --reviewer and --preset reviewer-agent")
	}

	if _, err := parseArgs([]string{"--reviewer", wd, "--workdir", "/tmp/other", "--", "true"}); err == nil {
		t.Fatal("expected conflict error between --reviewer and mismatched --workdir")
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{"A=1", "B=2", "C=3"}
	overrides := []string{"B=20", "D=4"}
	got := mergeEnv(base, overrides)
	want := []string{"A=1", "B=20", "C=3", "D=4"}
	if len(got) != len(want) {
		t.Fatalf("mergeEnv len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCleanupPresetTempDirsFallbackPath(t *testing.T) {
	customTmpParent := t.TempDir()
	t.Setenv("TMPDIR", customTmpParent)

	revDir, err := os.MkdirTemp("", "pi-reviewer-*")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer: %v", err)
	}
	execDir, err := os.MkdirTemp("", "pi-reviewer-exec-*")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer-exec: %v", err)
	}

	roSub := filepath.Join(revDir, "readonly-sub")
	if err := os.MkdirAll(roSub, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(roSub, "item.txt"), []byte("x"), 0o400); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_ = os.Chmod(roSub, 0o500)

	cleanupPresetTempDirsFromEnv([]string{
		"TMPDIR=" + revDir,
		"PI_SANDBOX_GO_EXEC_DIR=" + execDir,
	})

	if _, err := os.Lstat(revDir); !os.IsNotExist(err) {
		t.Fatalf("expected %q to be removed, stat err = %v", revDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("expected %q to be removed, stat err = %v", execDir, err)
	}
}

// TestSbxPresetNormalExitCleansTempDirs 验证正常退出（退出码 0）后，
// /tmp/pi-reviewer-* 与 /tmp/pi-reviewer-exec-* 目录在运行期存在、退出后被彻底删除。
func TestSbxPresetNormalExitCleansTempDirs(t *testing.T) {
	workdir := t.TempDir()
	script := `
if [ ! -d "$TMPDIR" ] || [ ! -d "$PI_SANDBOX_GO_EXEC_DIR" ]; then
	exit 91
fi
printf "%s\n%s\n" "$TMPDIR" "$PI_SANDBOX_GO_EXEC_DIR"
`
	out, code := invokeSbxSubprocess(t, nil,
		"--best-effort", "--preset", "reviewer", "--workdir", workdir,
		"--", "/bin/sh", "-c", script,
	)
	if code != 0 {
		t.Fatalf("sbx exit code = %d, want 0; output:\n%s", code, out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected output lines: %q", out)
	}
	tmpDir := strings.TrimSpace(lines[0])
	execDir := strings.TrimSpace(lines[1])

	if !strings.Contains(filepath.Base(tmpDir), "pi-reviewer-") {
		t.Fatalf("unexpected TMPDIR %q", tmpDir)
	}
	if !strings.Contains(filepath.Base(execDir), "pi-reviewer-exec-") {
		t.Fatalf("unexpected PI_SANDBOX_GO_EXEC_DIR %q", execDir)
	}

	if _, err := os.Lstat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("TMPDIR %q still exists after normal exit (err=%v)", tmpDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("PI_SANDBOX_GO_EXEC_DIR %q still exists after normal exit (err=%v)", execDir, err)
	}
}

// TestSbxPresetNonZeroExitCodePreservedAndCleaned 验证子进程以非 0 退出码退出时，
// sbx 原样返回该退出码，且临时目录依然被删除。
func TestSbxPresetNonZeroExitCodePreservedAndCleaned(t *testing.T) {
	workdir := t.TempDir()
	script := `
printf "%s\n%s\n" "$TMPDIR" "$PI_SANDBOX_GO_EXEC_DIR"
exit 42
`
	out, code := invokeSbxSubprocess(t, nil,
		"--best-effort", "--preset", "reviewer", "--workdir", workdir,
		"--", "/bin/sh", "-c", script,
	)
	if code != 42 {
		t.Fatalf("sbx exit code = %d, want 42; output:\n%s", code, out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected output lines: %q", out)
	}
	tmpDir := strings.TrimSpace(lines[0])
	execDir := strings.TrimSpace(lines[1])

	if _, err := os.Lstat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("TMPDIR %q leaked after non-zero exit (err=%v)", tmpDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("PI_SANDBOX_GO_EXEC_DIR %q leaked after non-zero exit (err=%v)", execDir, err)
	}
}

// TestSbxPresetSignalForwardingAndCleanup 验证 sbx 父进程收到 SIGTERM / SIGINT 时，
// 会将信号转发给受限子进程，且在子进程终止后仍会清理全部预设临时目录。
func TestSbxPresetSignalForwardingAndCleanup(t *testing.T) {
	signals := []struct {
		name     string
		sig      syscall.Signal
		wantCode int
	}{
		{name: "SIGTERM", sig: syscall.SIGTERM, wantCode: 128 + int(syscall.SIGTERM)},
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 128 + int(syscall.SIGINT)},
	}

	for _, tc := range signals {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			selfExe, err := os.Executable()
			if err != nil {
				t.Fatalf("os.Executable: %v", err)
			}

			// 使用 /bin/sh 内置 trap + read 阻塞在标准输入管道上，不依赖外部 sleep 二进制（sleep 不在 DefaultReviewerTools 白名单内）。
			script := `
trap 'exit 143' TERM
trap 'exit 130' INT
printf "%s\n%s\nREADY\n" "$TMPDIR" "$PI_SANDBOX_GO_EXEC_DIR"
read _
`
			cmd := exec.Command(selfExe,
				"--best-effort", "--preset", "reviewer", "--workdir", workdir,
				"--", "/bin/sh", "-c", script,
			)
			cmd.Env = append(os.Environ(), envTestWantSbxMain+"=1")

			stdinR, stdinW, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe stdin: %v", err)
			}
			defer stdinW.Close()
			defer stdinR.Close()
			cmd.Stdin = stdinR

			stdoutPipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("StdoutPipe: %v", err)
			}
			var stderrBuf bytes.Buffer
			cmd.Stderr = &stderrBuf

			if err := cmd.Start(); err != nil {
				t.Fatalf("cmd.Start: %v", err)
			}

			reader := bufio.NewReader(stdoutPipe)
			readLineWithTimeout := func() string {
				type res struct {
					line string
					err  error
				}
				ch := make(chan res, 1)
				go func() {
					s, err := reader.ReadString('\n')
					ch <- res{line: strings.TrimSpace(s), err: err}
				}()
				select {
				case r := <-ch:
					if r.err != nil {
						t.Fatalf("read child line failed: %v (stderr: %s)", r.err, stderrBuf.String())
					}
					return r.line
				case <-time.After(10 * time.Second):
					_ = cmd.Process.Kill()
					t.Fatalf("timed out waiting for child output (stderr: %s)", stderrBuf.String())
					return ""
				}
			}

			tmpDir := readLineWithTimeout()
			execDir := readLineWithTimeout()
			ready := readLineWithTimeout()
			if ready != "READY" {
				_ = cmd.Process.Kill()
				t.Fatalf("expected READY, got %q", ready)
			}

			// 发送信号前确认两个临时目录确实存在于磁盘上。
			if _, err := os.Stat(tmpDir); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("expected TMPDIR %q to exist while running: %v", tmpDir, err)
			}
			if _, err := os.Stat(execDir); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("expected PI_SANDBOX_GO_EXEC_DIR %q to exist while running: %v", execDir, err)
			}

			// 向 sbx 父进程发送信号。
			if err := cmd.Process.Signal(tc.sig); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("send signal %v failed: %v", tc.sig, err)
			}

			waitErr := cmd.Wait()
			gotCode := exitCodeFromWaitErr(waitErr)
			if gotCode != tc.wantCode {
				t.Fatalf("after %s, exit code = %d, want %d (stderr: %s)", tc.name, gotCode, tc.wantCode, stderrBuf.String())
			}

			if _, err := os.Lstat(tmpDir); !os.IsNotExist(err) {
				t.Fatalf("TMPDIR %q leaked after %s (err=%v)", tmpDir, tc.name, err)
			}
			if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
				t.Fatalf("PI_SANDBOX_GO_EXEC_DIR %q leaked after %s (err=%v)", execDir, tc.name, err)
			}
		})
	}
}

// TestSbxReviewerAgentCredentialsNotLeaked 验证 --preset reviewer-agent 运行期间
// 拷入 <privateTmpDir>/pig-agent/auth.json 的凭证副本在子进程退出后被彻底清除，不残留于 /tmp。
func TestSbxReviewerAgentCredentialsNotLeaked(t *testing.T) {
	workdir := t.TempDir()
	fakePigHome := t.TempDir()
	hostAgentDir := filepath.Join(fakePigHome, "agent")
	hostExtDir := filepath.Join(hostAgentDir, "extensions")
	if err := os.MkdirAll(hostExtDir, 0o700); err != nil {
		t.Fatalf("MkdirAll hostExtDir: %v", err)
	}

	authFixture := `{"token_placeholder":"fixture-value-for-cleanup-test"}`
	if err := os.WriteFile(filepath.Join(hostAgentDir, "auth.json"), []byte(authFixture), 0o600); err != nil {
		t.Fatalf("WriteFile auth.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostAgentDir, "settings.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatalf("WriteFile settings.json: %v", err)
	}

	script := `
auth_file="$PIG_CODING_AGENT_DIR/auth.json"
if [ ! -f "$auth_file" ]; then
	echo "missing auth.json in PIG_CODING_AGENT_DIR" >&2
	exit 92
fi
printf "%s\n%s\n%s\n" "$TMPDIR" "$PI_SANDBOX_GO_EXEC_DIR" "$auth_file"
`
	out, code := invokeSbxSubprocess(t,
		[]string{"PIG_HOME=" + fakePigHome},
		"--best-effort", "--preset", "reviewer-agent", "--workdir", workdir,
		"--", "/bin/sh", "-c", script,
	)
	if code != 0 {
		t.Fatalf("reviewer-agent exit code = %d, want 0; output:\n%s", code, out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("unexpected output lines: %q", out)
	}
	tmpDir := strings.TrimSpace(lines[0])
	execDir := strings.TrimSpace(lines[1])
	copiedAuthPath := strings.TrimSpace(lines[2])

	if !strings.HasPrefix(copiedAuthPath, tmpDir+string(filepath.Separator)) {
		t.Fatalf("copiedAuthPath %q is not inside TMPDIR %q", copiedAuthPath, tmpDir)
	}

	if _, err := os.Lstat(copiedAuthPath); !os.IsNotExist(err) {
		t.Fatalf("credential copy %q leaked after reviewer-agent exit (err=%v)", copiedAuthPath, err)
	}
	if _, err := os.Lstat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("TMPDIR %q leaked after reviewer-agent exit (err=%v)", tmpDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("PI_SANDBOX_GO_EXEC_DIR %q leaked after reviewer-agent exit (err=%v)", execDir, err)
	}
}

func invokeSbxSubprocess(t *testing.T, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cmd := exec.Command(selfExe, args...)
	env := append(os.Environ(), envTestWantSbxMain+"=1")
	env = append(env, extraEnv...)
	cmd.Env = env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	return out.String(), exitCodeFromWaitErr(err)
}
