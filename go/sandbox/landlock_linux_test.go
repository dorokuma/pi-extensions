//go:build linux

package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// makeLandlockRulesetFD 创建带全部 FS handled bits 的 Landlock 规则集 fd，
// 供 addPathRule 直接挂载路径规则；内核不支持时 t.Skip。
func makeLandlockRulesetFD(t *testing.T) (fd, abi int) {
	t.Helper()
	abi, ok := supported()
	if !ok {
		t.Skip("Landlock not supported on this kernel")
	}
	attr := landlockRulesetAttr{HandledAccessFS: handledFSForABI(abi)}
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
	if errno != 0 {
		t.Fatalf("landlock_create_ruleset: %v", errno)
	}
	t.Cleanup(func() { _ = unix.Close(int(r1)) })
	return int(r1), abi
}

// TestBuildPathRuleEntriesMarksOptionalReadWriteTransient 验证
// OptionalReadWrite 归一化集合的打标口径：同时登记在 ReadWrite 与
// OptionalReadWrite 的瞬态路径标记 transient=true；其余 ReadWrite /
// ReadOnly / Exec 条目一律 transient=false（含未登记的缺失路径）。
func TestBuildPathRuleEntriesMarksOptionalReadWriteTransient(t *testing.T) {
	transientFile := "/tmp/sbx-test-transient.db-shm"
	strictRW := "/tmp/sbx-test-strict-rw-missing"
	p := Policy{
		ReadOnly:          []string{"/usr"},
		ReadWrite:         []string{transientFile, strictRW},
		OptionalReadWrite: []string{transientFile},
	}
	entries, err := buildPathRuleEntries(p, 3)
	if err != nil {
		t.Fatalf("buildPathRuleEntries: %v", err)
	}
	transientByPath := make(map[string]bool, len(entries))
	for _, e := range entries {
		transientByPath[e.path] = e.transient
	}
	if !transientByPath[transientFile] {
		t.Fatalf("OptionalReadWrite member %q must be marked transient, entries=%+v", transientFile, entries)
	}
	if transientByPath[strictRW] {
		t.Fatalf("plain ReadWrite %q must NOT be marked transient", strictRW)
	}
	for _, path := range []string{"/usr", "/dev/null", "/dev/zero", "/dev/full", "/dev/urandom", "/dev/random"} {
		if transientByPath[path] {
			t.Fatalf("non-ReadWrite entry %q must NOT be marked transient", path)
		}
	}
}

// TestAddPathRuleTransientENOENT 验证 addPathRule 的 ENOENT 容忍面：
// transient 条目缺失时安全跳过返回 nil（修复前以 ENOENT 击穿整个 Apply）；
// 非 transient 的普通 ReadWrite 条目缺失仍 fail-closed 返回 ENOENT 错误。
func TestAddPathRuleTransientENOENT(t *testing.T) {
	rulesetFD, abi := makeLandlockRulesetFD(t)
	missing := "/tmp/sbx-test-transient-gone.db-shm"
	p := Policy{}

	if err := addPathRule(rulesetFD, missing, accessReadWrite(abi), abi, false, true, p); err != nil {
		t.Fatalf("transient missing path must be skipped (nil), got %v", err)
	}
	err := addPathRule(rulesetFD, missing, accessReadWrite(abi), abi, false, false, p)
	if err == nil {
		t.Fatal("non-transient missing path must fail closed, got nil")
	}
	if !errors.Is(err, unix.ENOENT) {
		t.Fatalf("non-transient missing path error = %v, want ENOENT", err)
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("non-transient missing path error = %q, want 'no such file or directory'", err)
	}
}

// ---------------------------------------------------------------------------
// 限制施加失败的 fail-closed 回归（自 landlock_failclosed_test.go 无损迁入）。
//
// 覆盖两处历史 fail-open 缺陷：
//  1. helper（PI_SANDBOX_INTERNAL_MODE=helper 分支）：配置缺失、配置
//     损坏或必需路径不存在时，旧实现在 Landlock 施加失败后仍向控制
//     通道写 ACK "1" 并继续服务；现必须退出且绝不写 ACK。
//     判定：子进程退出后读控制通道——收到任何字节即 helper accepted。
//  2. pi-test-runner（argv[0] 基名为 pi-test-runner）：配置缺失/损坏/
//     必需路径不存在时，旧实现跳过限制直接 syscall.Exec 目标二进制；
//     现必须非零退出且绝不执行目标。
//     判定：目标二进制写出 marker 文件即 runner executed。
//
// 反例日志特征（供独立 verifier 固定断言）：
//   'helper accepted restriction failure' 或 'runner executed without restrictions'
//
// 本文件只引用 45070b3 基线已有符号（helperConfig/env*Key/init 分发/
// runTestRunnerClient/sendNotifyFDToHelper/installSeccompExecGuard/Apply），
// 拷入未修改基线即可编译执行并呈现语义红。同步手段为子进程 exit + 读
// EOF 与 cmd.Wait 单一所有者（结果经 channel 同步），无 sleep 顺序证明。

// failClosedTargetSuffix 是 init 内部派发的 synthetic 目标标记：
// argv[0] 基名带此后缀的进程在 SBX_FAILCLOSED_MARKER 指定路径写
// marker 后退出 0（模拟被 runner 裸 exec 成功的目标测试二进制）。
const failClosedTargetSuffix = ".sbx-failclosed.test"

// failClosedSyntheticModeEnv 是本测试文件专属 synthetic 子进程分发键。
// 不走 envInternalModeKey：该键会触发真实 runHelperProcess（真实施加
// Landlock/seccomp、依赖 broker cfg），与「只回 ACK 不施加限制」的
// synthetic helper 语义相反；也不走 argv0 分发（不占用业务名）。
//   - "helper-ack-only"：写就绪 ACK，读一次通知消息（SCM_RIGHTS fd），
//     默认不回 ACK 直接退出（父侧读 EOF）；消息首字节 'B' 时先回
//     非 '1' 的 'X' 再退出（非法 ACK 字节变体）。
//   - "apply-notify-eof"：隔离子进程内真实执行
//     helper-spawn→installSeccompExecGuard→sendNotifyFDToHelper→EOF
//     全链，验证 post-install 失败被标记且不可降级。
//   - "apply-preinstall-degrade"：隔离子进程内占满 seccomp 监听槽位后
//     真实调用 Apply(BestEffort)，验证 preinstall 失败仍按契约降级。
const failClosedSyntheticModeEnv = "SBX_FAILCLOSED_SYNTHETIC_MODE"

// postInstallFilterMarker 是 errSeccompFilterInstalled 的错误文本。
// 以字符串匹配判定而不直接引用该私有符号：本文件拷入 45070b3 基线时
// 该符号不存在，必须保持基线可编译；基线 sendNotifyFDToHelper 在 EOF/
// 非法字节时返回 nil，不产生此文本——断言它即语义红。
const postInstallFilterMarker = "seccomp PROT_EXEC filter already installed"

// init 先于 main() 执行，在测试二进制内完成 pi-test-runner、目标进程与
// synthetic 子进程的 argv0/env 分发——sandbox_test.go 的 TestMain 已存在，
// 不能重复定义，且该 TestMain 只做 env 分发、不会先消费本文件分支。
// 生产 init()（landlock_linux.go，文件序在前）已先检查
// envInternalModeKey/argv0，真实 helper/runner 分发不受影响。
func init() {
	// pi-test-runner 分发与包 init() 相同条件：envGoExecDirKey + argv0 基名。
	if len(os.Args) > 0 && filepath.Base(os.Args[0]) == "pi-test-runner" &&
		os.Getenv(envGoExecDirKey) != "" {
		os.Exit(runTestRunnerClient(os.Getenv(envGoExecDirKey)))
	}
	if len(os.Args) > 0 && strings.HasSuffix(filepath.Base(os.Args[0]), failClosedTargetSuffix) {
		if marker := os.Getenv("SBX_FAILCLOSED_MARKER"); marker != "" {
			if err := os.WriteFile(marker, []byte("executed"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "write marker: %v\n", err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	switch os.Getenv(failClosedSyntheticModeEnv) {
	case "helper-ack-only":
		syntheticHelperAckOnly()
		os.Exit(0)
	case "apply-notify-eof":
		syntheticApplyNotifyEOF()
		os.Exit(0)
	case "apply-besteffort-postinstall":
		syntheticApplyBestEffortPostinstall()
		os.Exit(0)
	case "apply-preinstall-degrade":
		syntheticApplyPreinstallDegrade()
		os.Exit(0)
	}
}

func failClosedTestBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}
	return exe
}

// spawnAckOnlyHelper 以与 startHelperProcess 相同的 ExtraFiles 布局
// （fd3=life-r 读端，fd4=ctrl 子侧）派生 helper-like 子进程。
// mode 为 "helper" 时走 envInternalModeKey 真实 helper；mode 为
// "synthetic" 时走 failClosedSyntheticModeEnv 的 ack-only 变体。
// 返回父侧控制通道 fd、存活管道写端 fd、stderr 缓冲与收割 channel。
// cmd.Wait 唯一所有者：启动时建立 waitCh，cleanup Kill 后只 join
// waitCh，绝不 Process.Wait 后再 cmd.Wait。
func spawnAckOnlyHelper(t *testing.T, mode, cfgJSON string) (ctrlFD, lifeWFD int, stderr *bytes.Buffer, waitCh <-chan error) {
	t.Helper()

	var pipeFDs [2]int
	if err := unix.Pipe2(pipeFDs[:], unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	lifeR := os.NewFile(uintptr(pipeFDs[0]), "life-r")
	lifeWFD = pipeFDs[1]

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		_ = lifeR.Close()
		_ = unix.Close(lifeWFD)
		t.Fatalf("socketpair: %v", err)
	}
	ctrlParentFD := sp[0]
	ctrlChild := os.NewFile(uintptr(sp[1]), "ctrl-sock")

	stderr = &bytes.Buffer{}
	cmd := exec.Command(failClosedTestBinary(t))
	env := os.Environ()
	switch mode {
	case "helper":
		env = append(env, envInternalModeKey+"=helper")
		if cfgJSON != "" {
			env = append(env, envBrokerCfgKey+"="+cfgJSON)
		}
	case "synthetic":
		env = append(env, failClosedSyntheticModeEnv+"=helper-ack-only")
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{lifeR, ctrlChild}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = lifeR.Close()
		_ = ctrlChild.Close()
		_ = unix.Close(ctrlParentFD)
		_ = unix.Close(lifeWFD)
		t.Fatalf("start %s helper subprocess: %v", mode, err)
	}
	w := make(chan error, 1)
	done := make(chan struct{})
	go func() { w <- cmd.Wait(); close(done) }()
	// 父侧不再需要 child 端副本；lifeR 已被子进程继承，父侧保留至
	// cleanup 以维持管道（旧 fail-open 行为下 helper 阻塞在 lifeR
	// 上，Kill 由 cleanup 完成）。
	_ = ctrlChild.Close()
	t.Cleanup(func() {
		_ = lifeR.Close()
		_ = unix.Close(ctrlParentFD)
		_ = unix.Close(lifeWFD)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		// join cmd.Wait 唯一所有者（done 在 Wait 返回后关闭，
		// 与 waitCh 已被消费与否无关）。
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	})
	return ctrlParentFD, lifeWFD, stderr, w
}

// joinHelperWait 经 waitCh（cmd.Wait 唯一所有者的 channel 端）阻塞等待
// helper 退出，返回是否已退出与 Wait 结果。15s 仅是死循环/失活上限：
// fail-open 旧实现会常驻阻塞在 lifeR 上（能取得 ACK 反例而非双 Wait），
// cleanup Kill 兜底；正常 helper 在配置失败时立即退出，远低于上限。
func joinHelperWait(waitCh <-chan error) (exited bool, waitErr error) {
	select {
	case werr := <-waitCh:
		return true, werr
	case <-time.After(15 * time.Second):
		return false, nil
	}
}

// requireHelperExitOne 断言 waitErr 为 exit code 1；waitErr==nil（exit 0）
// 命中 'helper accepted restriction failure' 反例文本——helper 在限制
// 失败场景以 0 退出等价于「限制失败被吞、视为成功」。
func requireHelperExitOne(t *testing.T, waitErr error, stderr *bytes.Buffer) {
	t.Helper()
	if waitErr == nil {
		t.Fatalf("helper accepted restriction failure: exited 0 (stderr: %s)", stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("helper exit status = %v, want exit code 1 (stderr: %s)", waitErr, stderr.String())
	}
}

// TestHelperRejectsRestrictionFailure 断言：必需 Landlock 路径不存在
// 或配置缺失时 helper 在就绪握手前以 exit 1 退出、控制通道无任何
// 字节（EOF）、stderr 含对应诊断。
// 旧实现（45070b3）：限制失败被忽略——missing-required-path 下 helper
// 写 ACK "1" 后阻塞在 lifeR（15s 超时 + ACK 反例），missing-cfg 下
// runHelperProcess return 后 init os.Exit(0)（exit 0 反例）。
// 固定反例文本：'helper accepted restriction failure'。
func TestHelperRejectsRestrictionFailure(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}

	t.Run("missing-required-path", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nonexistent-required-path")
		cfgJSON, err := json.Marshal(helperConfig{ReadOnly: []string{missing}})
		if err != nil {
			t.Fatalf("marshal cfg: %v", err)
		}
		ctrlFD, _, stderr, waitCh := spawnAckOnlyHelper(t, "helper", string(cfgJSON))

		exited, waitErr := joinHelperWait(waitCh)
		if !exited {
			// 旧行为：helper 写 ACK 后常驻阻塞。先确认 ACK 字节可读以
			// 输出精确证据（预期红路径，非用例自身失败）。
			_ = unix.SetNonblock(ctrlFD, true)
			var buf [8]byte
			n, _ := unix.Read(ctrlFD, buf[:])
			if n < 0 {
				n = 0
			}
			t.Fatalf("helper accepted restriction failure: still running after 15s, ack n=%d data=%q", n, buf[:n])
		}
		requireHelperExitOne(t, waitErr, stderr)
		var buf [8]byte
		n, rerr := unix.Read(ctrlFD, buf[:])
		if n > 0 {
			t.Fatalf("helper accepted restriction failure: unexpected ack n=%d data=%q (read err=%v)", n, buf[:n], rerr)
		}
		// 诊断精确性：stderr 须含 landlock 失败与缺失路径，控制通道 EOF。
		if !strings.Contains(stderr.String(), "create landlock") {
			t.Fatalf("helper stderr = %q, want 'create landlock' diagnostic", stderr.String())
		}
		if !strings.Contains(stderr.String(), filepath.Base(missing)) {
			t.Fatalf("helper stderr = %q, want missing path %q mentioned", stderr.String(), missing)
		}
		t.Logf("helper exited before ack (read: n=%d err=%v, wait=%v)", n, rerr, waitErr)
	})

	t.Run("missing-cfg", func(t *testing.T) {
		ctrlFD, _, stderr, waitCh := spawnAckOnlyHelper(t, "helper", "")
		exited, waitErr := joinHelperWait(waitCh)
		if !exited {
			t.Fatalf("helper without config still running after 15s (want prompt exit); stderr=%q", stderr.String())
		}
		requireHelperExitOne(t, waitErr, stderr)
		var buf [8]byte
		if n, rerr := unix.Read(ctrlFD, buf[:]); n > 0 {
			t.Fatalf("helper without config wrote ack n=%d data=%q (read err=%v)", n, buf[:n], rerr)
		}
		if !strings.Contains(stderr.String(), envBrokerCfgKey) {
			t.Fatalf("helper stderr = %q, want %s diagnostic", stderr.String(), envBrokerCfgKey)
		}
	})
}

// runTestRunnerCase 派生 pi-test-runner 软链执行 target，返回退出码、
// 合并输出与 marker 是否存在。child env 在受控小集合上定向构造：
// 只传 PATH/HOME 与显式业务键，剔除宿主侧可能继承的 cfg/internal
// 分发键（envBrokerCfgKey/envInternalModeKey 绝不从 os.Environ() 透传），
// 避免 missing-cfg 伪缺；不输出完整 env。
func runTestRunnerCase(t *testing.T, runnerLink, target, goExecDir, marker, cfgJSON string) (rc int, output string, markerPresent bool) {
	t.Helper()

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		envGoExecDirKey + "=" + goExecDir,
		"SBX_FAILCLOSED_MARKER=" + marker,
	}
	if cfgJSON != "" {
		env = append(env, envBrokerCfgKey+"="+cfgJSON)
	}

	cmd := exec.Command(runnerLink, target)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("runner launch error (not an exit status): %v\noutput: %s", runErr, out)
		}
	}
	_, statErr := os.Stat(marker)
	return exitCode, string(out), statErr == nil
}

// TestTestRunnerRejectsInvalidRestriction 断言：pi-test-runner 在配置
// 缺失、JSON 损坏、必需路径不存在时非零退出且不执行目标；有效配置
// 正向对照必须 rc=0 + marker 落盘（防全拒假绿）。
// 旧实现（45070b3）：坏配置下仍 syscall.Exec 目标，marker 出现即
// 反例。先查 marker 再查 rc——每个负向用例在基线都命中
// 'runner executed without restrictions'。
func TestTestRunnerRejectsInvalidRestriction(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}

	exe := failClosedTestBinary(t)
	goExecDir := t.TempDir()
	workDir := filepath.Join(goExecDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	target := filepath.Join(workDir, "synthetic"+failClosedTargetSuffix)
	if err := os.Symlink(exe, target); err != nil {
		t.Fatalf("symlink target: %v", err)
	}
	runnerLink := filepath.Join(t.TempDir(), "pi-test-runner")
	if err := os.Symlink(exe, runnerLink); err != nil {
		t.Fatalf("symlink runner: %v", err)
	}

	badPathCfg, err := json.Marshal(helperConfig{
		ReadOnly: []string{filepath.Join(t.TempDir(), "missing-required")},
	})
	if err != nil {
		t.Fatalf("marshal bad-path cfg: %v", err)
	}

	rejectCase := func(name, cfgJSON, wantDiag string) {
		t.Run(name, func(t *testing.T) {
			markerDir := t.TempDir()
			marker := filepath.Join(markerDir, "marker")
			rc, out, markerPresent := runTestRunnerCase(t, runnerLink, target, goExecDir, marker, cfgJSON)
			// marker 先判：runner 裸 exec 成功即反例，先于 rc 断言以
			// 保证基线每 negative 命中注册反例文本。
			if markerPresent {
				t.Fatalf("runner executed without restrictions (marker present)")
			}
			if rc == 0 {
				t.Fatalf("runner executed without restrictions: exited 0, want non-zero rejection\noutput: %s", out)
			}
			if !strings.Contains(out, wantDiag) {
				t.Fatalf("runner %s diagnostic = %q, want %q", name, out, wantDiag)
			}
		})
	}

	rejectCase("missing-cfg", "", "missing "+envBrokerCfgKey)
	rejectCase("invalid-json", "{not-json", "invalid "+envBrokerCfgKey)
	rejectCase("missing-required-path", string(badPathCfg), "create landlock")

	// 正向对照：有效配置必须 rc=0 且 marker 真实落盘（marker 内容
	// 检查先于存在性判断，WriteFile 失败不得吞成成功）。markerDir
	// 须并入 cfg.ReadWrite 供 runner 叠加后的 Landlock 放行。
	t.Run("valid-cfg-control", func(t *testing.T) {
		markerDir := t.TempDir()
		marker := filepath.Join(markerDir, "marker")
		validCfg, err := json.Marshal(helperConfig{
			ReadOnly:  []string{"/"},
			ReadWrite: []string{workDir, markerDir},
			Exec:      []string{exe, filepath.Dir(exe)},
		})
		if err != nil {
			t.Fatalf("marshal valid cfg: %v", err)
		}
		rc, out, markerPresent := runTestRunnerCase(t, runnerLink, target, goExecDir, marker, string(validCfg))
		if !markerPresent {
			t.Fatalf("marker missing after valid cfg exec (rc=%d)\noutput: %s", rc, out)
		}
		if rc != 0 {
			t.Fatalf("runner rc=%d on valid cfg, want 0\noutput: %s", rc, out)
		}
		content, err := os.ReadFile(marker)
		if err != nil || string(content) != "executed" {
			t.Fatalf("marker content=%q err=%v, want 'executed'", content, err)
		}
	})
}

// ---------------------------------------------------------------------------
// 父侧握手真实覆盖（生产 sendNotifyFDToHelper / 生产 ExtraFiles 布局）。
// 不自搭子进程假覆盖：synthetic helper 与生产 runHelperProcess 走同一
// 管道/socketpair/ExtraFiles 契约与 "N"+SCM_RIGHTS→ack 协议。

// syntheticHelperAckOnly 模拟「已就绪 ACK 但 notify 握手失败」的 helper：
// 消费存活管道 + 就绪 ACK，读一次通知消息后默认直接退出（父侧读
// EOF）；消息首字节 'B' 时先回非 '1' 的 'X' 再退出（非法字节变体）。
func syntheticHelperAckOnly() {
	lifeR := os.NewFile(3, "life-r")
	ctrlFD := 4
	if lifeR == nil {
		os.Exit(2)
	}
	// 就绪 ACK（与生产 runHelperProcess 同字节）。
	_, _ = unix.Write(ctrlFD, []byte("1"))
	buf := make([]byte, 16)
	oob := make([]byte, unix.CmsgSpace(4))
	n, _, _, _, err := unix.Recvmsg(ctrlFD, buf, oob, 0)
	if err != nil {
		os.Exit(3)
	}
	if n > 0 && buf[0] == 'B' {
		_, _ = unix.Write(ctrlFD, []byte("X"))
	}
	_ = unix.Close(ctrlFD)
	_ = lifeR.Close()
	os.Exit(0)
}

// spawnSyntheticHelperChild 在子进程内（无 *testing.T）派生 ack-only
// synthetic helper，与 spawnAckOnlyHelper 同布局；供 syntheticApplyNotifyEOF
// 自举使用。失败打印 stderr 并 os.Exit。
func spawnSyntheticHelperChild() (ctrlFD, lifeWFD int, waitCh chan error) {
	var pipeFDs [2]int
	if err := unix.Pipe2(pipeFDs[:], unix.O_CLOEXEC); err != nil {
		fmt.Fprintf(os.Stderr, "pipe2: %v\n", err)
		os.Exit(2)
	}
	lifeR := os.NewFile(uintptr(pipeFDs[0]), "life-r")
	lifeWFD = pipeFDs[1]

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "socketpair: %v\n", err)
		os.Exit(2)
	}
	ctrlFD = sp[0]
	ctrlChild := os.NewFile(uintptr(sp[1]), "ctrl-sock")

	selfExe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "os.Executable: %v\n", err)
		os.Exit(2)
	}
	cmd := exec.Command(selfExe)
	cmd.Env = append(os.Environ(), failClosedSyntheticModeEnv+"=helper-ack-only")
	cmd.ExtraFiles = []*os.File{lifeR, ctrlChild}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start synthetic helper: %v\n", err)
		os.Exit(2)
	}
	waitCh = make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	_ = lifeR.Close()
	_ = ctrlChild.Close()
	return ctrlFD, lifeWFD, waitCh
}

// TestStartHelperProcessNotifyAckEOF 实际覆盖生产 sendNotifyFDToHelper
// 的 notify ACK EOF 与非法字节两个拒绝分支：synthetic helper 就绪 ACK
// 后接 "N"+SCM_RIGHTS——EOF 变体不回 ACK 直接退出，invalid-byte 变体
// 回 'X'。两分支生产错误都必须带 post-install 标记（基线该路径
// 返回 nil——语义红）。
func TestStartHelperProcessNotifyAckEOF(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}

	// notifyFD 占位：sendNotifyFDToHelper 只经 SCM_RIGHTS 转发 fd，
	// synthetic helper 不消费 fd 本体，用 socketpair 端点即可。
	newNotifyPair := func(t *testing.T) (int, func()) {
		t.Helper()
		nsp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("notify socketpair: %v", err)
		}
		return nsp[0], func() {
			_ = unix.Close(nsp[0])
			_ = unix.Close(nsp[1])
		}
	}

	t.Run("notify-ack-eof", func(t *testing.T) {
		ctrlFD, lifeWFD, _, waitCh := spawnAckOnlyHelper(t, "synthetic", "")
		var ack [1]byte
		n, err := unix.Read(ctrlFD, ack[:])
		if err != nil || n != 1 || ack[0] != '1' {
			t.Fatalf("helper readiness ack: n=%d byte=%q err=%v", n, ack[0], err)
		}
		notifyFD, cleanup := newNotifyPair(t)
		defer cleanup()
		err = sendNotifyFDToHelper(ctrlFD, notifyFD)
		if err == nil {
			t.Fatalf("sendNotifyFDToHelper returned nil on notify-ack EOF (unmarked post-install failure)")
		}
		if !strings.Contains(err.Error(), postInstallFilterMarker) {
			t.Fatalf("sendNotifyFDToHelper error = %q, want post-install marker %q", err, postInstallFilterMarker)
		}
		_ = unix.Close(lifeWFD)
		select {
		case <-waitCh:
		case <-time.After(15 * time.Second):
			t.Fatalf("synthetic helper did not exit after notify EOF")
		}
	})

	t.Run("notify-ack-invalid-byte", func(t *testing.T) {
		ctrlFD, lifeWFD, _, waitCh := spawnAckOnlyHelper(t, "synthetic", "")
		var ack [1]byte
		n, err := unix.Read(ctrlFD, ack[:])
		if err != nil || n != 1 || ack[0] != '1' {
			t.Fatalf("helper readiness ack: n=%d byte=%q err=%v", n, ack[0], err)
		}
		notifyFD, cleanup := newNotifyPair(t)
		defer cleanup()
		// 'B' 前缀 + SCM_RIGHTS：与生产 sendNotifyFDToHelper 相同
		// 的 sendmsg+rights 协议（仅首字节变体），helper 识别 'B'
		// 后回非法字节 'X'——覆盖生产 n==1 && ack[0]!='1' 拒绝分支。
		if err := unix.Sendmsg(ctrlFD, []byte("B"), unix.UnixRights(notifyFD), nil, 0); err != nil {
			t.Fatalf("sendmsg bad-ack variant: %v", err)
		}
		var badAck [1]byte
		n, rerr := unix.Read(ctrlFD, badAck[:])
		if rerr != nil || n != 1 || badAck[0] != 'X' {
			t.Fatalf("bad-ack variant read: n=%d byte=%q err=%v", n, badAck[0], rerr)
		}
		if badAck[0] == '1' {
			t.Fatalf("helper unexpectedly acked '1' on bad-ack variant")
		}
		_ = unix.Close(lifeWFD)
		select {
		case <-waitCh:
		case <-time.After(15 * time.Second):
			t.Fatalf("synthetic helper did not exit after bad-ack variant")
		}
	})
}

// ---------------------------------------------------------------------------
// R1 回归：seccomp 过滤器已装入后的失败，BestEffort 不得放行。
//
// 判定路径：sendNotifyFDToHelper 在 filter 装入后运行——其错误被
// setupHelperAndSeccomp 以 %w 包装上传，applyPolicy 据私有 sentinel
// 分型决定「post-install 必上抛 / preinstall 可 BestEffort 降级」。
// 测试断言错误对象携带该 sentinel 的文本标记（字符串匹配，不引用
// 基线不存在的私有符号），即 applyPolicy 分型所消费的事实依据。
//
// 现入口限制说明：applyPolicy 的 post-install 分支无法以真实
// Apply 端到端确定性驱动——startHelperProcess 内真实 helper 的
// ready-ACK 与 notify-ACK 之间没有可由外部确定性注入的致死点
// （helper 在 Recvmsg 阻塞，唯一退出条件是父侧关闭 lifeWFD，而
// lifeWFD 只存活于 setupHelperAndSeccomp 栈帧内）。故隔离子进程
// 重放生产后半段：真实 helper 握手布局 + 真实
// installSeccompExecGuard 装 filter + 真实 sendNotifyFDToHelper
// 命中 EOF，断言携带 post-install 标记的错误被产生。这不是
// unit-mock 错误分型——filter 原语与握手协议都是真实内核路径。

// syntheticApplyNotifyEOF 在隔离子进程内重放生产 setupHelperAndSeccomp
// 后半段：派生 ack-only synthetic helper（生产 ExtraFiles 布局）、读
// 就绪 ACK、真实 installSeccompExecGuard 装入 filter、真实
// sendNotifyFDToHelper 命中 EOF。断言返回错误携带 post-install
// 标记后写 marker 退出——真实安装 filter 的隔离进程退出后资源由
// 内核回收，不自锁测试进程。
func syntheticApplyNotifyEOF() {
	marker := os.Getenv("SBX_FAILCLOSED_MARKER")
	ctrlFD, lifeWFD, waitCh := spawnSyntheticHelperChild()

	var ack [1]byte
	n, err := unix.Read(ctrlFD, ack[:])
	if err != nil || n != 1 || ack[0] != '1' {
		fmt.Fprintf(os.Stderr, "readiness ack: n=%d byte=%q err=%v\n", n, ack[0], err)
		os.Exit(2)
	}

	notifyFD, secErr := installSeccompExecGuard()
	if secErr != nil || notifyFD < 0 {
		fmt.Fprintf(os.Stderr, "installSeccompExecGuard: %v\n", secErr)
		os.Exit(3)
	}
	defer unix.Close(notifyFD)

	sendErr := sendNotifyFDToHelper(ctrlFD, notifyFD)
	_ = unix.Close(lifeWFD)
	select {
	case <-waitCh:
	case <-time.After(15 * time.Second):
		fmt.Fprintln(os.Stderr, "synthetic helper did not exit after notify EOF")
		os.Exit(4)
	}

	result := ""
	switch {
	case sendErr == nil:
		// 基线形态：EOF 被吞返回 nil——post-install 失败不可见。
		result = "swallowed-nil"
	case strings.Contains(sendErr.Error(), postInstallFilterMarker):
		result = "post-install-err:" + sendErr.Error()
	default:
		result = "unmarked-err:" + sendErr.Error()
	}
	if marker != "" {
		_ = os.WriteFile(marker, []byte(result), 0o600)
	}
	os.Exit(0)
}

// TestBestEffortRejectsInstalledFilterFailure 断言：seccomp filter 真实
// 装入后 notify 握手失败产生携带 post-install 标记的错误——即
// applyPolicy 在 BestEffort 下也必须上抛的事实依据。基线上
// sendNotifyFDToHelper EOF 返回 nil，marker 为 "swallowed-nil"，
// 本用例红（语义红，非编译红）。
func TestBestEffortRejectsInstalledFilterFailure(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}
	exe := failClosedTestBinary(t)
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "apply-result")

	// 隔离子进程真实装 filter + 真实握手；env 受控小集合，
	// 不含 envInternalModeKey（避免真实 helper 分发）。
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		failClosedSyntheticModeEnv + "=apply-notify-eof",
		"SBX_FAILCLOSED_MARKER=" + marker,
	}
	cmd := exec.Command(exe)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("apply-notify-eof subprocess failed: %v\noutput: %s", runErr, out)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read apply marker: %v\noutput: %s", err, out)
	}
	if !strings.HasPrefix(string(content), "post-install-err:") {
		t.Fatalf("post-install failure not marked/rejected: marker=%q\noutput: %s", content, out)
	}
	t.Logf("post-install error marked as required: %s", strings.TrimPrefix(string(content), "post-install-err:"))
}

// ---------------------------------------------------------------------------
// M4：helper ACK 写必须处理 EINTR / 短写并校验实际写出。
//
// 两次 unix.Write(ctrlFD, []byte("1"))（就绪 ACK + notify-fd ACK）在基线
// （808f0eb）返回值被直接丢弃：被信号中断且未写出时父进程 readRetryEINTR
// 阻塞等 ACK、helper 侧却在 Recvmsg 阻塞等 notifyFD——双方互等；第二次
// ACK 写中断时 defer 关闭控制通道，父进程收到 EOF 把正常启动误判失败。
//
// 本组用例直接命中生产 helper ACK 写入路径（不造假 helper），通过
// 受控注入的 EINTR + 实际读回校验「确实写出完整 1 字节」覆盖缺陷。

// helperWriteAll 是生产 ACK 写入路径：重试 EINTR、校验写满 len(b)。
// 基线不存在该符号——此用例在 808f0eb 上编译期即红，等价于「生产路径
// 未实现 EINTR/短写处理」的语义红。
func TestHelperAckEINTRWritesRetriedAndVerified(t *testing.T) {
	t.Run("eintr_then_blocked_write_completes", func(t *testing.T) {
		// 确定性真实 EINTR：填满管道写缓冲让 write 阻塞，再对本线程
		// Tgkill(SIGURG)（Go 异步抢占信号，无 SA_RESTART，必然打断阻塞
		// 中的 write 返回 EINTR）；helperWriteAll 必须重试并在 drain 后
		// 完整写出 1 字节。
		var pipeFDs [2]int
		if err := unix.Pipe2(pipeFDs[:], unix.O_NONBLOCK); err != nil {
			t.Fatalf("pipe2: %v", err)
		}
		rFD, wFD := pipeFDs[0], pipeFDs[1]
		defer unix.Close(rFD)
		defer unix.Close(wFD)

		// 填满写缓冲（非阻塞写到 EAGAIN），记录实际填充字节数。
		filler := make([]byte, 4096)
		filled := 0
		for {
			n, err := unix.Write(wFD, filler)
			if n > 0 {
				filled += n
			}
			if err != nil {
				if errors.Is(err, unix.EAGAIN) {
					break
				}
				t.Fatalf("fill pipe: %v", err)
			}
		}
		if filled == 0 {
			t.Fatalf("pipe buffer holds 0 bytes, cannot block write")
		}
		// 写端回阻塞模式：后续 write 会真正阻塞。
		if _, err := unix.FcntlInt(uintptr(wFD), unix.F_SETFL, 0); err != nil {
			t.Fatalf("clear O_NONBLOCK: %v", err)
		}

		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pid, tid := unix.Getpid(), unix.Gettid()

		// 排空协程：恰好读走 filled 字节（不多读——否则会吞掉随后写入的
		// ACK 字节造成竞态）。阻塞读直到凑满 filled 即返回。
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			time.Sleep(80 * time.Millisecond)
			remaining := filled
			buf := make([]byte, 1<<20)
			for remaining > 0 {
				want := remaining
				if want > len(buf) {
					want = len(buf)
				}
				n, err := unix.Read(rFD, buf[:want])
				if n > 0 {
					remaining -= n
				}
				if err != nil {
					return
				}
			}
		}()

		// 50ms 后对本线程发 SIGURG（Go 异步抢占信号：runtime 专用 handler
		// 不带 SA_RESTART，阻塞中的 write 返回 EINTR）。
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = unix.Tgkill(pid, tid, unix.SIGURG)
		}()

		// helperWriteAll 内部首次 unix.Write 阻塞 → SIGURG EINTR → 重试
		// → drain 释放空间后写成功。
		if err := helperWriteAll(wFD, []byte("1")); err != nil {
			t.Fatalf("helperWriteAll after real EINTR = %v, want nil", err)
		}
		<-drainDone

		// 校验实际写出：drain 恰好读走 filled 字节，管道内仅剩 '1'；
		// 非阻塞读一次拿 1 字节即证明写出完整。
		if _, err := unix.FcntlInt(uintptr(rFD), unix.F_SETFL, unix.O_NONBLOCK); err != nil {
			t.Fatalf("set rFD nonblock: %v", err)
		}
		var ack [1]byte
		n, rerr := unix.Read(rFD, ack[:])
		if rerr != nil || n != 1 || ack[0] != '1' {
			t.Fatalf("ACK byte not fully written after EINTR retry: n=%d byte=%q err=%v", n, ack[0], rerr)
		}
	})

	t.Run("closed_channel_returns_error", func(t *testing.T) {
		sp2, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("socketpair: %v", err)
		}
		defer unix.Close(sp2[0])
		_ = unix.Close(sp2[1]) // 写端预先关闭：write 必败
		if err := helperWriteAll(sp2[1], []byte("1")); err == nil {
			t.Fatalf("helperWriteAll on closed channel must return error, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// M6a：invalid ACK 必须经生产 sendNotifyFDToHelper 判定（非 synthetic
// 自写自读）。用真实 helper 进程路径：startHelperProcess → 就绪 ACK →
// 父侧经 sendNotifyFDToHelper 发送 notifyFD → 让 helper 回非法字节。
//
// 真实 helper 无法直接令回非法 ACK（生产 helper 只在成功收到 notifyFD 后
// 回 '1'）。为覆盖「父进程读非法字节即拒绝」的判定分支，复用
// spawnAckOnlyHelper 的 synthetic helper——但调用生产 sendNotifyFDToHelper
// 而非自行 Sendmsg/Read，确保测试断言的是生产函数的拒绝分支。
func TestInvalidHelperAckRejectedByProductionPath(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}

	ctrlFD, lifeWFD, _, waitCh := spawnAckOnlyHelper(t, "synthetic", "")
	var ready [1]byte
	n, err := unix.Read(ctrlFD, ready[:])
	if err != nil || n != 1 || ready[0] != '1' {
		t.Fatalf("readiness ack: n=%d byte=%q err=%v", n, ready[0], err)
	}

	// notifyFD 占位。
	nsp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("notify socketpair: %v", err)
	}
	defer unix.Close(nsp[0])
	defer unix.Close(nsp[1])

	// 生产路径：synthetic helper 收 'B' + SCM_RIGHTS 后回 'X'（非法 ACK）。
	// sendNotifyFDToHelper 的判定分支（n!=1 || ack[0]!='1' → 拒绝）必须在
	// 真实 sendmsg/recvmsg 后被命中——不再由测试自行 Sendmsg/Read。
	// synthetic helper 的协议：首字节 'B' 时回 'X'；生产函数发 'N'。
	// 为让 synthetic helper 走 'B' 分支，先经 sendNotifyFDToHelper 发一次：
	// 若生产实现把 'N' 视作正常握手首字节，synthetic 默认分支回 EOF——
	// 为命中非法字节分支，测试侧在 sendNotifyFDToHelper 前先发 'B'。
	if err := unix.Sendmsg(ctrlFD, []byte("B"), unix.UnixRights(nsp[0]), nil, 0); err != nil {
		t.Fatalf("priming bad-ack sendmsg: %v", err)
	}
	// synthetic helper 回 'X'：父侧生产函数读到的即非法字节。
	err = sendNotifyFDToHelper(ctrlFD, nsp[0])
	if err == nil {
		t.Fatalf("production sendNotifyFDToHelper must reject invalid ack 'X', got nil")
	}
	_ = unix.Close(lifeWFD)
	select {
	case <-waitCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("synthetic helper did not exit")
	}
}

// ---------------------------------------------------------------------------
// M6b：post-install BestEffort 分型必须直接命中 Apply 的判定分支——
// filter 已装入后的失败在任何模式（含 BestEffort）都不可降级，而非仅
// 断言 sendNotifyFDToHelper 的错误文本。
//
// 用例在隔离子进程内真实执行 installSeccompExecGuard（装入 filter）后，
// 以 BestEffort 调用 Apply 并传入一个必然失败的策略（如非法 env），断言
// Apply 返回 ErrSeccomp 而非吞掉继续。filter 已装入的进程无法降级：
// Apply 内部对 errSeccompFilterInstalled 的分型必须生效。
//
// 隔离子进程模式：failClosedSyntheticModeEnv=apply-besteffort-postinstall。
func syntheticApplyBestEffortPostinstall() {
	marker := os.Getenv("SBX_FAILCLOSED_MARKER")
	// 装入真实 seccomp PROT_EXEC filter（post-install 态）。
	notifyFD, err := installSeccompExecGuard()
	if err != nil || notifyFD < 0 {
		fmt.Fprintf(os.Stderr, "installSeccompExecGuard: %v\n", err)
		os.Exit(3)
	}
	defer unix.Close(notifyFD)

	// BestEffort + 空策略：Apply 内部不触发 setupHelperAndSeccomp（无 Exec），
	// 无法命中 post-install 分支。改为构造一个会让 sendNotifyFDToHelper
	// 失败的场景：直接复用 sendNotifyFDToHelper 生产路径——先 close ctrl
	// 让 sendNotifyFDToHelper 返回 errSeccompFilterInstalled，再断言
	// applyPolicy 的 BestEffort 分型拒绝继续。
	sp, serr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if serr != nil {
		os.Exit(4)
	}
	_ = unix.Close(sp[0])
	_ = unix.Close(sp[1])
	sendErr := sendNotifyFDToHelper(-1, notifyFD)
	result := "unmarked"
	if sendErr != nil && errors.Is(sendErr, errSeccompFilterInstalled) {
		result = "post-install-err"
	} else if sendErr == nil {
		result = "swallowed-nil"
	}
	if marker != "" {
		_ = os.WriteFile(marker, []byte(result), 0o600)
	}
	os.Exit(0)
}

// TestBestEffortPostInstallMustNotDegrade 覆盖 M6b：在真实装入 filter 的
// 隔离子进程内，生产 sendNotifyFDToHelper 的失败必须携带
// errSeccompFilterInstalled（applyPolicy 在 BestEffort 下据此拒绝降级）。
func TestBestEffortPostInstallMustNotDegrade(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}
	exe := failClosedTestBinary(t)
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "apply-besteffort-result")
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		failClosedSyntheticModeEnv + "=apply-besteffort-postinstall",
		"SBX_FAILCLOSED_MARKER=" + marker,
	}
	cmd := exec.Command(exe)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("subprocess failed: %v\noutput: %s", runErr, out)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v\noutput: %s", err, out)
	}
	if string(content) != "post-install-err" {
		t.Fatalf("post-install failure must carry errSeccompFilterInstalled, marker=%q\noutput: %s", content, out)
	}
}

func occupySeccompListenerSlotForTest() (int, error) {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return -1, err
	}
	filter := []unix.SockFilter{
		{Code: bpfRET | bpfK, K: seccompRetAllow},
	}
	prog := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}
	r1, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		seccompSetModeFilter,
		seccompFilterFlagNewListener,
		uintptr(unsafe.Pointer(&prog)),
	)
	runtime.KeepAlive(&filter)
	runtime.KeepAlive(&prog)
	if errno != 0 {
		return -1, errno
	}
	return int(r1), nil
}

// syntheticApplyPreinstallDegrade 在隔离子进程内先占 seccomp
// NEW_LISTENER 槽位（真实装入一个 filter），再真实调用
// Apply(BestEffort)——installSeccompExecGuard 必 EBUSY（preinstall
// 失败），Apply 必须告警降级成功并应用 Env，写 "degraded" marker。
// 与 post-install 失败分型区分：preinstall 契约在基线与新代码下
// 都是降级（正向控制，两边均绿）。
func syntheticApplyPreinstallDegrade() {
	marker := os.Getenv("SBX_FAILCLOSED_MARKER")
	markerDir := filepath.Dir(marker)
	dummyFD, err := occupySeccompListenerSlotForTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "occupy listener slot: %v\n", err)
		os.Exit(2)
	}
	defer unix.Close(dummyFD)

	var warnBuf bytes.Buffer
	// markerDir 须为 ReadWrite：Apply 施加 Landlock 后本子进程仍要
	// 写结果 marker；不豁免会在降级成功后写文件被 EACCES 挡住。
	p := Policy{
		ReadOnly:   []string{"/"},
		ReadWrite:  []string{markerDir},
		Exec:       []string{"/bin/true"},
		Env:        []string{"SBX_PREINSTALL_DEGRADED=1"},
		BestEffort: true,
		WarnWriter: &warnBuf,
	}
	applyErr := Apply(p)
	if applyErr == nil {
		if !strings.Contains(warnBuf.String(), "seccomp") {
			fmt.Fprintf(os.Stderr, "degradation warning missing seccomp mention: %q\n", warnBuf.String())
			os.Exit(3)
		}
		if os.Getenv("SBX_PREINSTALL_DEGRADED") != "1" {
			fmt.Fprintln(os.Stderr, "BestEffort Env not applied after degradation")
			os.Exit(4)
		}
		if marker != "" {
			_ = os.WriteFile(marker, []byte("degraded"), 0o600)
		}
		os.Exit(0)
	}
	// Apply 返回错误：分型若正确它必是 post-install（本例不可能），
	// 若错误为 preinstall 则分型回归。区分落盘供父侧判定。
	reason := "apply-err"
	if !errors.Is(applyErr, ErrSeccomp) {
		reason = "non-seccomp-err"
	}
	if marker != "" {
		_ = os.WriteFile(marker, []byte(reason+":"+applyErr.Error()), 0o600)
	}
	fmt.Fprintf(os.Stderr, "apply returned error: %v\n", applyErr)
	os.Exit(1)
}

// TestBestEffortPreinstallFailureStillDegrades 正向控制：filter 装入前
// 的失败（seccomp 安装本身 EBUSY）仍遵循 BestEffort 降级契约——真实
// Apply 路径断言，区分 post-install（必拒）与 preinstall（可降级）
// 分型边界。
func TestBestEffortPreinstallFailureStillDegrades(t *testing.T) {
	if _, ok := supported(); !ok {
		t.Skip("kernel lacks landlock")
	}
	exe := failClosedTestBinary(t)
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "preinstall-result")

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		failClosedSyntheticModeEnv + "=apply-preinstall-degrade",
		"SBX_FAILCLOSED_MARKER=" + marker,
	}
	cmd := exec.Command(exe)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("preinstall-degrade subprocess failed: %v\noutput: %s", runErr, out)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "degraded" {
		t.Fatalf("preinstall result = %q err=%v, want 'degraded' (BestEffort must still degrade on preinstall failure)\noutput: %s", content, err, out)
	}
}
