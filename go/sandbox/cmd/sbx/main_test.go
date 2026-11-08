package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const envTestWantSbxMain = "GO_WANT_SBX_MAIN"

// envTestMountProbe 让复用测试二进制的沙盒内进程进入 mount 探针模式：
// 直接对 setns(/proc/1/ns/mnt) 与 umount2(/nonexistent-sbx-mount-seal-probe)
// 发 syscall 并打印 errno，验证 mount-seal seccomp 锁以 EPERM 拦截。
const envTestMountProbe = "GO_WANT_SBX_MOUNT_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(envSbxChildSpecKey) != "" {
		os.Exit(runChildFromEnv())
	}
	if os.Getenv(envTestMountProbe) == "1" {
		os.Exit(runMountNSProbe())
	}
	if os.Getenv(envTestWantSbxMain) == "1" {
		_ = os.Unsetenv(envTestWantSbxMain)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMountNSProbe 在受限沙盒内执行：对 setns、umount2 及越狱/进程控制族
// 直接发 syscall，打印 errno 供父进程断言（seccomp 锁装好后必须均为
// EPERM=1；umount2/chroot 目标用不存在路径，未装锁时返回 ENOENT=2，详见
// 行内注释）。并断言独立 PID 命名空间视图：打印自身 PID、探测宿主父进程
// PID 在沙盒 /proc 下的可见性、以及 /proc/1/root 魔链是否穿透到宿主
// /run。使用 golang.org/x/sys/unix 的架构相关系统调用号常量，amd64 与
// arm64 的过滤清单都能命中（过滤器按当前 GOARCH 生成）；amd64 另打 x32
// 位探针。此外对 moshi-hook.sock 做一次 unix dial：只明确接受
// connected 与 ECONNREFUSED（socket 可达但无监听端），EROFS（回绑被
// 只读 remount 覆盖）/ENOENT（未回绑）与 EACCES/超时等其它错误一律报
// 具体值并在断言侧判失败。
func runMountNSProbe() int {
	// setns：打开 /proc/self/ns/mnt 的 fd 发起 setns（新 PID ns 内
	// /proc/self 即本沙盒进程自身；过滤器按系统调用号拦截，EPERM 与目标
	// 无关；无过滤器时本调用返回 0/EINVAL）。
	setnsErrno := -1
	if f, err := os.Open("/proc/self/ns/mnt"); err == nil {
		defer f.Close()
		_, _, e := syscall.Syscall(unix.SYS_SETNS, f.Fd(), 0, 0)
		setnsErrno = int(e)
	} else {
		setnsErrno = -2
	}
	// moshi-hook.sock 连通性（先 dial 再发其它探针，保证拨号结果反映的是
	// 白名单视图原状而非被探针改动后的状态）：仅明确接受 connected 与
	// ECONNREFUSED；EROFS/ENOENT 说明回绑丢失或被只读化，EACCES/超时等
	// 其它错误同样是失败的证据，全部如实打印供断言判红。
	if _, err := os.Lstat("/run/user/0/moshi-hook.sock"); err == nil {
		conn, derr := net.DialTimeout("unix", "/run/user/0/moshi-hook.sock", 2*time.Second)
		switch {
		case derr == nil:
			_ = conn.Close()
			fmt.Println("moshi_dial=connected")
		case errors.Is(derr, syscall.ECONNREFUSED):
			fmt.Printf("moshi_dial=refused(%v)\n", derr)
		default:
			fmt.Printf("moshi_dial=other(%v)\n", derr)
		}
	} else {
		fmt.Println("moshi_dial=absent")
	}
	// umount2：对保证不存在的固定路径发卸载请求探针。seccomp-BPF 过滤器
	// 在系统调用入口按 syscall 号直接拦截，早于内核路径查找，装锁时必然
	// 返回 EPERM(1)；未装锁时内核路径查找返回 ENOENT(2)。故意选不存在
	// 路径是为了防误伤：即使开发者或子代理在宿主机 root 环境误裸跑本探针，
	// 也绝不会卸载任何真实挂载点（旧实现对 /run 发 MNT_DETACH，宿主机
	// 裸跑会把宿主 /run 延迟卸载）。
	umountErrno := -1
	if p, err := syscall.BytePtrFromString("/nonexistent-sbx-mount-seal-probe"); err == nil {
		_, _, e := syscall.Syscall(unix.SYS_UMOUNT2, uintptr(unsafe.Pointer(p)), 0, 0)
		umountErrno = int(e)
	}
	fmt.Printf("setns_errno=%d\numount_errno=%d\n", setnsErrno, umountErrno)

	// 独立 PID 命名空间视图：本进程在沙盒新 PID ns 内；宿主父进程 PID
	// （经 HOST_PARENT_PID 传入）在沙盒 /proc 下必须 ENOENT。
	fmt.Printf("self_pid=%d\n", os.Getpid())
	if hp := strings.TrimSpace(os.Getenv("HOST_PARENT_PID")); hp != "" {
		if _, err := os.Stat("/proc/" + hp); os.IsNotExist(err) {
			fmt.Println("host_pid=hidden")
		} else {
			fmt.Printf("host_pid=visible(%v)\n", err)
		}
	}
	// /proc/1/root 魔链：新 PID ns + 独立 procfs 下，/proc/1 是本沙盒 init，
	// 其 root 指向本沙盒已覆盖 /run 的挂载命名空间——经它访问宿主
	// /run 全权 socket 路径必须 ENOENT（宿主存在性由父进程前置核验）。
	for _, p := range strings.Split(strings.TrimSpace(os.Getenv("PROC1_ROOT_PROBE_PATHS")), " ") {
		if p == "" {
			continue
		}
		key := "proc1root_" + strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(strings.TrimPrefix(p, "/"))
		if _, err := os.Stat(filepath.Join("/proc/1/root", p)); os.IsNotExist(err) {
			fmt.Println(key + "=hidden")
		} else {
			fmt.Printf("%s=visible(%v)\n", key, err)
		}
	}

	// 越狱/进程控制族 seccomp 探针（装锁时全部 EPERM=1）：
	// chroot 对不存在路径未装锁返回 ENOENT(2)，同理不碰任何真实目录；
	// ptrace 全零参数未装锁返回 0/EINVAL/ESRCH；pidfd_getfd(-1) 未装锁
	// 内核先校验 fd 返回 EBADF(9)。
	chrootErrno := -1
	if p, err := syscall.BytePtrFromString("/nonexistent-sbx-probe"); err == nil {
		_, _, e := syscall.Syscall(unix.SYS_CHROOT, uintptr(unsafe.Pointer(p)), 0, 0)
		chrootErrno = int(e)
	}
	_, _, ptraceE := syscall.Syscall6(unix.SYS_PTRACE, 0, 0, 0, 0, 0, 0)
	_, _, pidfdGetfdE := syscall.Syscall(unix.SYS_PIDFD_GETFD, ^uintptr(0), 0, 0) // fd=-1
	fmt.Printf("chroot_errno=%d\nptrace_errno=%d\npidfd_getfd_errno=%d\n",
		chrootErrno, int(ptraceE), int(pidfdGetfdE))
	// x32 ABI 探针（仅 amd64）：调用号带 __X32_SYSCALL_BIT(0x40000000)，
	// 未封堵时内核按 x32 号表解释返回非 EPERM（如 getpid 成功或 ENOSYS），
	// 装锁后 JGE 高位规则直接返回 EPERM(1)。
	if runtime.GOARCH == "amd64" {
		_, _, x32E := syscall.Syscall(0x40000000|unix.SYS_GETPID, 0, 0, 0)
		fmt.Printf("x32_getpid_errno=%d\n", int(x32E))
	}
	return 0
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

	t.Run("session_cap_via_injectable_clock", func(t *testing.T) {
		var mu sync.Mutex
		now := time.Now()
		r := &reportRelay{parent: "tTEST", tokens: reportBurstTokens, lastRef: now,
			hcPath: writeFakeHc(t, "#!/bin/sh\nexit 0\n"),
			now:    func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
		advance := func(d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		}
		// 真实走完 1..20 条成功：tokens 耗尽时推进一个 refill 周期补充。
		for i := 1; i <= reportSessionMax; i++ {
			if status, _ := r.process(reportRequest{To: "tTEST", Text: "x"}); status != reportStatusOK {
				t.Fatalf("send %d status = %d, want ok", i, status)
			}
			if i < reportSessionMax {
				advance(reportRefillEvery)
			}
		}
		// 第 21 条：会话上限拒绝。
		if status, reason := r.process(reportRequest{To: "tTEST", Text: "x"}); status != reportStatusRejected || !strings.Contains(reason, "rate") {
			t.Fatalf("session cap 21st: status=%d reason=%q, want reject", status, reason)
		}
	})

	t.Run("token_refill_preserves_remainder", func(t *testing.T) {
		var mu sync.Mutex
		t0 := time.Now()
		now := t0
		r := &reportRelay{parent: "tTEST", tokens: reportBurstTokens, lastRef: t0,
			hcPath: writeFakeHc(t, "#!/bin/sh\nexit 0\n"),
			now:    func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
		advance := func(d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		}
		// 耗尽突发桶：连取 reportBurstTokens 枚全部成功，紧接一次必然失败，
		// 此刻 tokens==0、lastRef==t0，后续 refill 必走未满桶 else 分支
		// （余数保留路径），不再是满桶重置路径。
		for i := 0; i < reportBurstTokens; i++ {
			if !r.takeToken() {
				t.Fatalf("takeToken %d rejected while draining burst bucket", i)
			}
		}
		if r.takeToken() {
			t.Fatal("takeToken succeeded with empty burst bucket")
		}
		// 推进 45s（1.5 个周期）：elapsed=45s → add=1，0+1 < burstTokens 命中
		// else 分支，lastRef 只推进已消费的整周期 → lastRef = t0+30s；
		// 消费后 tokens 回 0，同一时刻再取必失败。
		advance(45 * time.Second)
		if !r.takeToken() {
			t.Fatal("refill after 45s rejected: expected add=1 via else branch")
		}
		if r.takeToken() {
			t.Fatal("second takeToken at same instant succeeded: tokens should be 0")
		}
		// 再推进 15s（累计 t0+60s，距 lastRef 恰满 30s）：add=1 → tokens=1，
		// 取出成功。若实现丢弃 15s 余数把 lastRef 置成 now（t0+45s），此刻
		// 仅过 15s，必然拒绝——本条断言正是余数保留的红灯探针。
		advance(15 * time.Second)
		if !r.takeToken() {
			t.Fatal("remainder-preserved refill rejected: lastRef lost 15s remainder")
		}
	})

	t.Run("frame_size_boundary_64kib", func(t *testing.T) {
		// 协议边界是字面常量 65532（64*1024-4，含 4B 头的整包不超 64KiB）；
		// 测试帧长度一律用硬编码边界值构造，不得引用生产常量生成——否则实现
		// 回退为旧值时测试边界随之漂移，锁定不住回归。先独立断言生产常量。
		const frameBodyMax = 65532
		if reportMaxFrame != frameBodyMax {
			t.Fatalf("reportMaxFrame = %d, want protocol boundary %d", reportMaxFrame, frameBodyMax)
		}
		newPipe := func() (*reportRelay, net.Conn) {
			srv, cli := net.Pipe()
			r := &reportRelay{parent: "tTEST", tokens: reportBurstTokens, lastRef: time.Now(),
				hcPath: writeFakeHc(t, "#!/bin/sh\nexit 0\n")}
			go r.handleConn(srv)
			return r, cli
		}
		// sendFrame 发出 n 字节帧体并读取完整回包（1 字节状态 + 原因文本）。
		sendFrame := func(cli net.Conn, payload []byte) (byte, string) {
			t.Helper()
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
			go func() {
				_, _ = cli.Write(hdr[:])
				_, _ = cli.Write(payload)
			}()
			_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
			resp, err := io.ReadAll(cli)
			if err != nil {
				t.Fatalf("read reply: %v", err)
			}
			if len(resp) == 0 {
				t.Fatal("empty reply frame")
			}
			return resp[0], string(resp[1:])
		}
		// 恰好等于帧体上限（4B 头 + 64KiB-4 体 = 64KiB 整包）：构造合法
		// JSON 请求体填充至精确 65532 字节，必须走完校验投递返回
		// reportStatusOK——不能停在超帧拒绝或 bad request。
		exactBody := []byte(fmt.Sprintf(`{"to":"tTEST","text":"%s"}`,
			strings.Repeat("a", frameBodyMax-len(`{"to":"tTEST","text":""}`))))
		if len(exactBody) != frameBodyMax {
			t.Fatalf("exact-limit payload = %d bytes, want %d", len(exactBody), frameBodyMax)
		}
		_, cli := newPipe()
		if st, reason := sendFrame(cli, exactBody); st != reportStatusOK {
			t.Fatalf("exact-limit frame status = %d reason=%q, want ok", st, reason)
		}
		_ = cli.Close()
		// 上限 +1（65533）→ 超帧拒绝，且原因必须点名 frame too large（不是
		// bad request/short frame 等其它拒绝理由）。
		_, cli = newPipe()
		if st, reason := sendFrame(cli, make([]byte, frameBodyMax+1)); st != reportStatusRejected || !strings.Contains(reason, "frame too large") {
			t.Fatalf("over-limit frame status = %d reason=%q, want reject+frame too large", st, reason)
		}
		_ = cli.Close()
	})

	t.Run("concurrent_dial_close_race", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			sockDir := t.TempDir()
			ln, err := net.Listen("unix", filepath.Join(sockDir, "report.sock"))
			if err != nil {
				t.Skipf("unix listen: %v", err)
			}
			rctx, rcancel := context.WithCancel(context.Background())
			r := &reportRelay{
				parent:    "tTEST",
				ln:        ln,
				ctx:       rctx,
				cancel:    rcancel,
				serveDone: make(chan struct{}),
				conns:     make(map[net.Conn]struct{}),
				tokens:    reportSessionMax,
				lastRef:   time.Now(),
				hcPath:    writeFakeHc(t, "#!/bin/sh\nexit 0\n"),
			}
			go r.serve()
			var wg sync.WaitGroup
			for d := 0; d < 8; d++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if c, err := net.Dial("unix", ln.Addr().String()); err == nil {
						_ = c.SetDeadline(time.Now().Add(2 * time.Second))
						_ = c.Close()
					}
				}()
			}
			// 与在途拨号并发关闭：旧实现 Add/Wait 竞态在 -race 下报警。
			r.close()
			wg.Wait()
		}
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
	// 父进程预置非空哨兵，验证外部指针类变量在沙盒内被全部剥离；
	// --env 同口径校验（HC_TERMINAL_SOCKET 走 --env 显式注入）。
	t.Setenv("HC_SCOPE", "leak")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/leak.sock")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/leak")
	t.Setenv("GPG_AGENT_INFO", "leak")

	moshiPresent := false
	if st, err := os.Stat("/run/user/0/moshi-hook.sock"); err == nil && st.Mode()&os.ModeSocket != 0 {
		moshiPresent = true
	}
	// 宿主 /etc/resolv.conf 存在且可读时，沙盒内同名路径必须可读
	// （systemd-resolved stub 场景靠 /run 白名单回绑还原；普通文件场景
	// /etc 不在覆盖范围内）。宿主不可读时跳过该断言防假绿。
	resolvConfReadable := false
	if f, err := os.Open("/etc/resolv.conf"); err == nil {
		_ = f.Close()
		resolvConfReadable = true
	} else {
		t.Logf("host /etc/resolv.conf unreadable (skip precondition): %v", err)
	}
	// 宿主前置核验：/run 全权 socket 面在宿主上确实存在，沙盒内断言
	// 才有隐藏语义（非假绿）；宿主不存在时记录跳过该断言。
	type runSockCheck struct{ path, key string }
	runSocks := []runSockCheck{
		{"/run/systemd/private", "run_systemd_private"},
		{"/run/docker.sock", "run_docker_sock"},
		{"/run/containerd/containerd.sock", "run_containerd_sock"},
		{"/run/dbus/system_bus_socket", "run_dbus_sock"},
		{"/run/user/0/herdr-core/default.sock", "run_herdr_sock"},
	}
	var runSockScriptLines []string
	var runSockAsserts []runSockCheck
	for _, sc := range runSocks {
		// 存在为 socket 或普通文件都算“宿主上可达对象存在”。
		if st, err := os.Stat(sc.path); err == nil && (st.Mode()&os.ModeSocket != 0 || st.Mode().IsRegular()) {
			runSockScriptLines = append(runSockScriptLines,
				"[ -e "+sc.path+" ] && echo "+sc.key+"=visible || echo "+sc.key+"=hidden")
			runSockAsserts = append(runSockAsserts, sc)
		} else {
			t.Logf("host %s absent (skip precondition): socket-hiding assertion not applicable", sc.path)
		}
	}

	probeBin, _ := os.Executable()
	// 宿主 /run 全权 socket 路径集同时用于两类断言：沙盒内直连路径 ENOENT
	// （runSockAsserts），以及经 /proc/1/root 魔链访问 ENOENT
	// （PROC1_ROOT_PROBE_PATHS 传给探针按路径逐个 stat）。
	var proc1ProbePaths []string
	for _, sc := range runSockAsserts {
		proc1ProbePaths = append(proc1ProbePaths, sc.path)
	}
	script := `
printf 'sh_pid=%s\n' "$$"
printf 'init_comm=%s\n' "$(cat /proc/1/comm 2>/dev/null)"
hc send tTEST hello
printf 'send_hi_rc=%d\n' "$?"
hc run true
printf 'hc_run_rc=%d\n' "$?"
hc send tOTHER x
printf 'send_other_rc=%d\n' "$?"
"$FAKE_HC_ABS" ls
printf 'abs_hc_rc=%d\n' "$?"
"$PROBE_BIN" 2>/dev/null
[ -S /run/user/0/systemd/private ] && echo systemd_sock=visible || echo systemd_sock=hidden
[ -S /run/user/0/bus ] && echo bus_sock=visible || echo bus_sock=hidden
[ -S /run/user/0/herdr-core/default.sock ] && echo hc_sock=visible || echo hc_sock=hidden
[ -S /run/user/0/moshi-hook.sock ] && echo moshi_sock=visible || echo moshi_sock=absent
[ -r /etc/resolv.conf ] && echo resolv_conf=readable || echo resolv_conf=unreadable
[ -n "$HC_TERMINAL_SOCKET" ] && echo hc_term_sock=set || echo hc_term_sock=empty
[ -n "$HC_SCOPE" ] && echo hc_scope=set || echo hc_scope=empty
[ -n "$SSH_AUTH_SOCK" ] && echo ssh_sock=set || echo ssh_sock=empty
[ -n "$DBUS_SESSION_BUS_ADDRESS" ] && echo dbus_addr=set || echo dbus_addr=empty
[ -n "$GPG_AGENT_INFO" ] && echo gpg_info=set || echo gpg_info=empty
echo "xdg_rt=$XDG_RUNTIME_DIR"
[ -n "$HC_TERMINAL_ID" ] && echo hc_term_id=kept || echo hc_term_id=empty
echo "goexec_dir=$PI_SANDBOX_GO_EXEC_DIR"
echo "gotmpdir=$GOTMPDIR"
`
	script += strings.Join(runSockScriptLines, "\n") + "\n"
	out, code := captureRunMain(t, []string{
		"--preset", "reviewer-agent", "--workdir", workdir,
		"--report-to", "tTEST",
		"--env", "HC_TERMINAL_SOCKET=/run/user/0/herdr-core/default.sock",
		"--env", "FAKE_HC_ABS=" + fake,
		"--env", "PROBE_BIN=" + probeBin,
		"--env", "GO_WANT_SBX_MOUNT_PROBE=1",
		"--env", "HOST_PARENT_PID=" + strconv.Itoa(os.Getpid()),
		"--env", "PROC1_ROOT_PROBE_PATHS=" + strings.Join(proc1ProbePaths, " "),
		"--env", "PI_SANDBOX_GO_EXEC_DIR=/tmp/hijack", // 必须被回钉回 pi-reviewer-exec-*
		"--exec", filepath.Dir(probeBin),
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
	for _, sc := range runSockAsserts {
		assertOut(sc.key, "hidden")
	}
	if moshiPresent {
		assertOut("moshi_sock", "visible")
		// 白名单回绑仅明确接受两类结果：connected 或 ECONNREFUSED（socket
		// 可达但无监听端）。EROFS（回绑被只读 remount 覆盖）、ENOENT
		// （未回绑）与 EACCES/超时等其它错误一律判失败——拨号被权限阻断
		// 不得假绿。
		if !strings.Contains(out, "moshi_dial=connected") && !strings.Contains(out, "moshi_dial=refused(") {
			t.Fatalf("moshi_dial verdict missing or unexpected (want connected or refused); output:\n%s", out)
		}
	}
	if resolvConfReadable {
		assertOut("resolv_conf", "readable")
	}
	assertOut("hc_term_sock", "empty")
	assertOut("hc_scope", "empty")
	assertOut("ssh_sock", "empty")
	assertOut("dbus_addr", "empty")
	assertOut("gpg_info", "empty")
	assertOut("hc_term_id", "kept")

	// mount-seal seccomp 锁：amd64 与 arm64 下探针的 setns/umount2 与
	// 越狱/进程控制族（chroot/ptrace/pidfd_getfd）必须全部返回 EPERM
	// （探针与过滤器同用 x/sys/unix 的架构相关调用号）。
	switch runtime.GOARCH {
	case "amd64", "arm64":
		assertOut("setns_errno", "1")
		assertOut("umount_errno", "1")
		assertOut("chroot_errno", "1")
		assertOut("ptrace_errno", "1")
		assertOut("pidfd_getfd_errno", "1")
		if runtime.GOARCH == "amd64" {
			assertOut("x32_getpid_errno", "1") // x32 ABI 位必须被 JGE 规则封堵
		}
	default:
		t.Logf("GOARCH=%s: mount-seal filter not installed, probe not asserted", runtime.GOARCH)
	}

	// 独立 PID 命名空间：PID 1 必须是 sbx init 本体（测试二进制自身；
	// Apply 的 helper/broker 会合法占用 2..N，故不钉死受测命令为 PID 2，
	// 改以「PID 1 身份 + 小号 PID + 宿主 PID ENOENT」三重证据锁定新 ns）。
	wantInitComm := filepath.Base(probeBin)
	if len(wantInitComm) > 15 { // comm 上限 TASK_COMM_LEN-1=15
		wantInitComm = wantInitComm[:15]
	}
	assertOut("init_comm", wantInitComm)
	assertOut("host_pid", "hidden")
	for _, key := range []string{"sh_pid", "self_pid"} {
		matched := false
		for _, line := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(line, key+"="); ok {
				matched = true
				n, err := strconv.Atoi(v)
				if err != nil || n <= 0 || n >= 500 {
					t.Fatalf("%s = %q: not a small sandbox pidns pid (host pid leak?); output:\n%s", key, v, out)
				}
			}
		}
		if !matched {
			t.Fatalf("missing %s line; output:\n%s", key, out)
		}
	}
	for _, sc := range runSockAsserts {
		key := "proc1root_" + strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(strings.TrimPrefix(sc.path, "/"))
		assertOut(key, "hidden")
	}

	// PI_SANDBOX_GO_EXEC_DIR：--env 劫持值被回钉回预设注入的
	// pi-reviewer-exec-*，且 GOTMPDIR 与其同根（gotmp 子目录）。
	sawGoExec := false
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "goexec_dir="); ok {
			sawGoExec = true
			if !strings.HasPrefix(filepath.Base(v), "pi-reviewer-exec-") {
				t.Fatalf("PI_SANDBOX_GO_EXEC_DIR = %q, want /tmp/pi-reviewer-exec-* (re-pin failed)", v)
			}
			if v == "/tmp/hijack" {
				t.Fatalf("PI_SANDBOX_GO_EXEC_DIR hijack survived: %q", v)
			}
		}
	}
	if !sawGoExec {
		t.Fatalf("missing goexec_dir line; output:\n%s", out)
	}

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
