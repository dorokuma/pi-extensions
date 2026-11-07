package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
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

// TestBuildPolicyRepinsPigHome 验证 reviewer-agent 预设下 mergeEnv 之后
// PIG_HOME 被回钉为 ReviewerAgentWithEnv 解析出的非空值：cfgEnv 中的
// PIG_HOME 键（含前面的覆盖项）不得把其它值/空串写进最终 Policy.Env，
// 沙盒内 pig 与预热/策略构造引用同一个缓存根。
func TestBuildPolicyRepinsPigHome(t *testing.T) {
	fakeHome := t.TempDir()
	otherHome := t.TempDir()
	t.Setenv("PIG_HOME", otherHome) // 宿主值必须被 cfgEnv 覆盖，不得进入最终 env
	workdir := t.TempDir()

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Env:     []string{"PIG_HOME=" + otherHome, "PIG_HOME=" + fakeHome},
	})
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	defer cleanupPresetTempDirsFromPolicy(policy)

	got := envListValue(policy.Env, "PIG_HOME")
	if got != fakeHome {
		t.Fatalf("PIG_HOME must be re-pinned to resolved %q, got %q (env=%v)", fakeHome, got, policy.Env)
	}
	// 同名键只保留一个条目。
	count := 0
	for _, kv := range policy.Env {
		if strings.HasPrefix(kv, "PIG_HOME=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one PIG_HOME entry in Policy.Env, got %d (%v)", count, policy.Env)
	}
}

// TestBuildPolicyRepinsClaudeConfigDir 验证 reviewer-agent 预设下 mergeEnv 之后
// CLAUDE_CONFIG_DIR 被回钉为 ReviewerAgentWithEnv 注入的 <privateTmpDir>/claude：
// --env CLAUDE_CONFIG_DIR=<其他路径> 覆盖不得把 CC 写面指回宿主或与 ReadWrite 脱配，
// 且同名键只保留一个条目（与 PIG_HOME 回钉同型）。
func TestBuildPolicyRepinsClaudeConfigDir(t *testing.T) {
	t.Setenv("PIG_HOME", t.TempDir()) // 隔离宿主 pig home，防预热/策略构造读真实缓存
	overrideDir := t.TempDir()
	workdir := t.TempDir()

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Env:     []string{"CLAUDE_CONFIG_DIR=" + overrideDir, "CLAUDE_CONFIG_DIR=" + overrideDir},
	})
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	defer cleanupPresetTempDirsFromPolicy(policy)

	got := envListValue(policy.Env, "CLAUDE_CONFIG_DIR")
	if got == overrideDir || got == "" {
		t.Fatalf("CLAUDE_CONFIG_DIR must be re-pinned to <privateTmpDir>/claude, got %q (env=%v)", got, policy.Env)
	}
	want := filepath.Join(envListValue(policy.Env, "TMPDIR"), "claude")
	if got != want {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, want)
	}
	// 同名键只保留一个条目。
	count := 0
	for _, kv := range policy.Env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one CLAUDE_CONFIG_DIR entry in Policy.Env, got %d (%v)", count, policy.Env)
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

// TestReportRelayValidation 表驱动覆盖中继校验链路：非父地址拒绝、控制字符
// 剔除、UTF-8 边界截断、限流拒绝/恢复、慢连接读超时。
func TestReportRelayValidation(t *testing.T) {
	t.Run("sanitize_strips_control_chars", func(t *testing.T) {
		in := "a\x1b[31m\r\nb\x07\tend"
		got := sanitizeReportText(in)
		want := "a[31m\nb\tend"
		if got != want {
			t.Fatalf("sanitizeReportText = %q, want %q", got, want)
		}
	})

	t.Run("sanitize_truncates_at_rune_boundary", func(t *testing.T) {
		pad := strings.Repeat("a", reportMaxTextBytes-1)
		got := sanitizeReportText(pad + "尾tail")
		// 「尾」占字节 7999-8001 跨 8000 边界，回退到字符边界后整字剔除。
		want := pad + reportTruncSuffix
		if got != want {
			t.Fatalf("truncated = %q (len %d), want len %d", got[len(got)-40:], len(got), len(want))
		}
	})

	t.Run("non_parent_address_rejected", func(t *testing.T) {
		r := &reportRelay{parent: "tTEST", tokens: 1, lastRef: time.Now()}
		status, _ := r.process(reportRequest{To: "tOTHER", Text: "hi"})
		if status != reportStatusRejected {
			t.Fatalf("status = %d, want reject(%d)", status, reportStatusRejected)
		}
	})

	t.Run("rate_limit_burst_and_session_cap", func(t *testing.T) {
		var calls int32
		r := &reportRelay{parent: "tTEST", tokens: reportBurstTokens, lastRef: time.Now()}
		fake := writeFakeHc(t, "#!/bin/sh\nexit 0\n")
		r.hcPath = fake
		for i := 0; i < reportBurstTokens; i++ {
			status, _ := r.process(reportRequest{To: "tTEST", Text: "x"})
			if status != reportStatusOK {
				t.Fatalf("send %d status = %d, want ok", i, status)
			}
			calls++
		}
		if status, reason := r.process(reportRequest{To: "tTEST", Text: "x"}); status != reportStatusRejected || !strings.Contains(reason, "rate") {
			t.Fatalf("burst exhausted: status=%d reason=%q", status, reason)
		}
		// 时间前进一格 refill 周期后恢复。
		r.mu.Lock()
		r.lastRef = time.Now().Add(-reportRefillEvery)
		r.mu.Unlock()
		if status, _ := r.process(reportRequest{To: "tTEST", Text: "x"}); status != reportStatusOK {
			t.Fatalf("after refill status = %d, want ok", status)
		}
		// 会话上限：耗尽 sent 计数后拒绝。
		r.mu.Lock()
		r.sent = reportSessionMax
		r.tokens = reportBurstTokens
		r.mu.Unlock()
		if status, _ := r.process(reportRequest{To: "tTEST", Text: "x"}); status != reportStatusRejected {
			t.Fatalf("session cap: status = %d, want reject", status)
		}
		_ = calls
	})

	t.Run("delivery_failure_maps_status2", func(t *testing.T) {
		r := &reportRelay{parent: "tTEST", tokens: 1, lastRef: time.Now()}
		r.hcPath = writeFakeHc(t, "#!/bin/sh\nexit 3\n")
		status, reason := r.process(reportRequest{To: "tTEST", Text: "x"})
		if status != reportStatusFailed || !strings.Contains(reason, "delivery") {
			t.Fatalf("status=%d reason=%q, want failed", status, reason)
		}
	})

	t.Run("dash_prefixed_text_gets_leading_space", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hc.log")
		fake := writeFakeHc(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+logFile+"\n")
		r := &reportRelay{parent: "tTEST", tokens: 1, lastRef: time.Now(), hcPath: fake}
		if status, _ := r.process(reportRequest{To: "tTEST", Text: "-dash"}); status != reportStatusOK {
			t.Fatalf("status = %d, want ok", status)
		}
		data, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("read hc log: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 3 || lines[0] != "send" || lines[1] != "tTEST" || lines[2] != " -dash" {
			t.Fatalf("hc argv lines = %v, want [send tTEST ' -dash']", lines)
		}
	})

	t.Run("slow_connection_times_out", func(t *testing.T) {
		sockDir := t.TempDir()
		ln, err := net.Listen("unix", filepath.Join(sockDir, "report.sock"))
		if err != nil {
			t.Skipf("unix listen: %v", err)
		}
		defer ln.Close()
		r := &reportRelay{parent: "tTEST", ln: ln, tokens: 1, lastRef: time.Now()}
		old := reportConnReadTimeout
		reportConnReadTimeout = 200 * time.Millisecond
		defer func() { reportConnReadTimeout = old }()
		go r.serve()
		conn, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		// 永不写字节：服务端读超时应主动关连接并回拒绝帧。
		deadline := time.After(3 * time.Second)
		reply := make(chan []byte, 1)
		go func() {
			buf := make([]byte, 256)
			n, _ := conn.Read(buf)
			reply <- buf[:n]
		}()
		select {
		case b := <-reply:
			if len(b) == 0 || b[0] != reportStatusRejected {
				t.Fatalf("reply = %v, want reject frame", b)
			}
		case <-deadline:
			t.Fatal("server did not close slow connection within read timeout")
		}
	})
}

func writeFakeHc(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-hc")
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake hc: %v", err)
	}
	return p
}

// captureRunMain 在本进程内调用 runMain 并捕获其 stdout/stderr（受限子进程
// 经 os.Executable() 复用测试二进制，TestMain 的 envSbxChildSpecKey 分支进入
// runChildFromEnv）。
func captureRunMain(t *testing.T, args []string) (string, int) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	outCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(rOut); outCh <- b }()
	errCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(rErr); errCh <- b }()

	os.Stdout, os.Stderr = wOut, wErr
	code := runMain(args)
	os.Stdout, os.Stderr = origOut, origErr
	_ = wOut.Close()
	_ = wErr.Close()
	out := append(<-outCh, <-errCh...)
	return string(out), code
}

// TestSbxReportRelayIntegration 集成回归：reviewer-agent + --report-to 下
// 沙盒内 hc 受限客户端经 report.sock 中继回报，同时验证 socket 白名单视图与
// 环境剥离。假 hc 路径经 hcPathOverride 注入（等价于构建期 -ldflags -X，
// 生产不可经环境覆盖）。
func TestSbxReportRelayIntegration(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mount namespace isolation requires CAP_SYS_ADMIN (euid 0)")
	}
	workdir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "hc.log")
	fake := writeFakeHc(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+logFile+"\nexit 0\n")

	oldOverride := hcPathOverride
	hcPathOverride = fake
	defer func() { hcPathOverride = oldOverride }()
	// 外层 HC_TERMINAL_ID 与 --report-to 不同：不禁用中继，且应透传进沙盒。
	t.Setenv("HC_TERMINAL_ID", "t999")

	moshiPresent := false
	if st, err := os.Stat("/run/user/0/moshi-hook.sock"); err == nil && st.Mode()&os.ModeSocket != 0 {
		moshiPresent = true
	}

	script := `
hc send tTEST hello
printf 'send_hi_rc=%d\n' "$?"
hc run true
printf 'hc_run_rc=%d\n' "$?"
hc send tOTHER x
printf 'send_other_rc=%d\n' "$?"
"$FAKE_HC_ABS" ls
printf 'abs_hc_rc=%d\n' "$?"
[ -S /run/user/0/systemd/private ] && echo systemd_sock=visible || echo systemd_sock=hidden
[ -S /run/user/0/bus ] && echo bus_sock=visible || echo bus_sock=hidden
[ -S /run/user/0/herdr-core/default.sock ] && echo hc_sock=visible || echo hc_sock=hidden
[ -S /run/user/0/moshi-hook.sock ] && echo moshi_sock=visible || echo moshi_sock=absent
[ -n "$HC_TERMINAL_SOCKET" ] && echo hc_term_sock=set || echo hc_term_sock=empty
[ -n "$HC_SCOPE" ] && echo hc_scope=set || echo hc_scope=empty
[ -n "$SSH_AUTH_SOCK" ] && echo ssh_sock=set || echo ssh_sock=empty
[ -n "$DBUS_SESSION_BUS_ADDRESS" ] && echo dbus_addr=set || echo dbus_addr=empty
echo "xdg_rt=$XDG_RUNTIME_DIR"
[ -n "$HC_TERMINAL_ID" ] && echo hc_term_id=kept || echo hc_term_id=empty
`
	out, code := captureRunMain(t, []string{
		"--preset", "reviewer-agent", "--workdir", workdir,
		"--report-to", "tTEST",
		"--env", "HC_TERMINAL_SOCKET=/run/user/0/herdr-core/default.sock",
		"--env", "FAKE_HC_ABS=" + fake,
		"--", "/bin/sh", "-c", script,
	})
	if code != 0 {
		t.Fatalf("sbx exit = %d, want 0; output:\n%s", code, out)
	}

	assertOut := func(key, want string) {
		t.Helper()
		if !strings.Contains(out, key+"="+want) {
			t.Fatalf("missing %q; output:\n%s", key+"="+want, out)
		}
	}
	assertOut("send_hi_rc", "0")
	assertOut("hc_run_rc", "126")
	assertOut("send_other_rc", "1")
	assertOut("abs_hc_rc", "126")
	assertOut("systemd_sock", "hidden")
	assertOut("bus_sock", "hidden")
	assertOut("hc_sock", "hidden")
	if moshiPresent {
		assertOut("moshi_sock", "visible")
	}
	assertOut("hc_term_sock", "empty")
	assertOut("hc_scope", "empty")
	assertOut("ssh_sock", "empty")
	assertOut("dbus_addr", "empty")
	assertOut("hc_term_id", "kept")

	// XDG_RUNTIME_DIR 由预设回钉为私有目录（非宿主 /run/user/0）。
	sawXDG := false
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "xdg_rt="); ok {
			sawXDG = true
			if v == "" || v == "/run/user/0" {
				t.Fatalf("XDG_RUNTIME_DIR = %q, want private runtime dir", v)
			}
		}
	}
	if !sawXDG {
		t.Fatalf("missing xdg_rt line; output:\n%s", out)
	}

	// 假 hc 收到的 argv 必须是 send tTEST hello。
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake hc log missing (delivery never happened): %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"send", "tTEST", "hello"}
	if len(got) != len(want) {
		t.Fatalf("fake hc argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fake hc argv = %v, want %v", got, want)
		}
	}
}
