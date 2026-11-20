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
	"sync"
	"sync/atomic"
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
	case "helper-ack-only", "helper-ack-bad-byte":
		syntheticHelperAckOnly()
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
	case "synthetic-bad-byte":
		env = append(env, failClosedSyntheticModeEnv+"=helper-ack-bad-byte")
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

// ---------------------------------------------------------------------------
// MF-1 回归：受控 go 构建代理 validateGoWrapperArgs 的参数/路径收口。
// 纯函数断言，不依赖真实 go 工具链——直接驱动生产校验函数覆盖：
//   - -o 经 $TMPDIR 软链指向 <goExecDir>/work 时必须拒（首个存在祖先软链解析）；
//   - 缺省 -o + cwd=<goExecDir>/work（含软链别名）时必须拒；
//   - -ldflags/-gcflags/-asmflags/-gccgoflags 外挂标志必须拒；
//   - 合法 build/vet/test 参数仍放行（防全拒假绿）。
func TestValidateGoWrapperArgsWriteThrough(t *testing.T) {
	goExecDir := t.TempDir()
	workDir := filepath.Join(goExecDir, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	outside := t.TempDir()

	// 软链：$outside/sym -> <goExecDir>/work，-o <sym>/pwn 必须被拒。
	sym := filepath.Join(outside, "sym")
	if err := os.Symlink(workDir, sym); err != nil {
		t.Fatalf("symlink sym->work: %v", err)
	}

	reject := func(name string, args []string, cwd string) {
		t.Helper()
		if _, err := validateGoWrapperArgs(args, cwd, goExecDir); err == nil {
			t.Fatalf("%s: validateGoWrapperArgs(%v, cwd=%q) must be rejected, got nil", name, args, cwd)
		}
	}
	accept := func(name string, args []string, cwd string) {
		t.Helper()
		if _, err := validateGoWrapperArgs(args, cwd, goExecDir); err != nil {
			t.Fatalf("%s: validateGoWrapperArgs(%v, cwd=%q) = %v, want nil", name, args, cwd, err)
		}
	}

	// 路径 A：软链 -o 写穿
	reject("symlink -o", []string{"build", "-o", filepath.Join(sym, "pwn"), "./x"}, outside)
	reject("symlink -o joined flag", []string{"build", "-o" + filepath.Join(sym, "pwn"), "./x"}, outside)
	// 直接 -o 到 goExecDir 内部（含嵌套 work/）
	reject("direct -o into goExecDir", []string{"build", "-o", filepath.Join(goExecDir, "work", "pwn"), "./x"}, outside)
	reject("direct -o into goExecDir root", []string{"build", "-o", filepath.Join(goExecDir, "pwn"), "./x"}, outside)

	// 路径 B：缺省 -o + cwd 落在 goExecDir（含 work/ 与软链别名）
	reject("no -o cwd=goExecDir/work", []string{"build", "./x"}, workDir)
	reject("no -o cwd=goExecDir", []string{"build", "./x"}, goExecDir)
	// cwd 经软链指向 work/（sym 本身是 work 的软链）
	reject("no -o cwd=symlink to work", []string{"build", "./x"}, sym)

	// 路径 C：链接器/编译器外挂标志
	for _, flagArgs := range [][]string{
		{"build", "-ldflags=-linkmode=external -extld=/bin/sh", "./x"},
		{"build", "-ldflags", "-s -w", "./x"},
		{"build", "-gcflags=-N", "./x"},
		{"build", "-asmflags=-trimpath", "./x"},
		{"build", "-gccgoflags=-foo", "./x"},
		{"test", "-ldflags=all=-linkmode=external", "./..."},
	} {
		reject("external flag "+flagArgs[1], flagArgs, outside)
	}

	// 合法流程放行（cwd 在 goExecDir 之外、无外挂标志、无软链 -o）
	accept("plain build", []string{"build", "./x"}, outside)
	accept("plain vet", []string{"vet", "./x"}, outside)
	accept("plain test", []string{"test", "./x"}, outside)
	accept("explicit -o to outside", []string{"build", "-o", filepath.Join(outside, "bin"), "./x"}, outside)
	// -o 目标不存在（新文件）但父目录在 goExecDir 外：允许（写面归审查员
	// 私有 TMPDIR 域，与 W^X 无关）。
	accept("explicit -o new file outside", []string{"build", "-o", filepath.Join(outside, "new-sub", "bin"), "./x"}, outside)
}

// TestValidateGoWrapperArgsClientCwd 断言：runGoWrapperClient 现在必须以真实
// cwd 而非空串调 validateGoWrapperArgs——空串会让缺省 -o 的 cwd 校验完全失效。
// 经客户端路径预校验（客户端 cwd=<goExecDir>/work 时即使未走 broker 也必须拒）。
func TestValidateGoWrapperArgsClientCwd(t *testing.T) {
	goExecDir := t.TempDir()
	workDir := filepath.Join(goExecDir, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	// 模拟客户端在 <goExecDir>/work 内发起 build：客户端侧（cwd 非空）与
	// broker 侧（req.Dir 非空）都应拒绝——cwd 为空串的旧实现客户端会漏过。
	if _, err := validateGoWrapperArgs([]string{"build", "./x"}, workDir, goExecDir); err == nil {
		t.Fatal("client-side validation with real cwd inside goExecDir/work must reject")
	}
	// cwd 空串时缺省 -o 校验被跳过（仅防 runGoWrapperClient 回退传空串的
	// 静默退化回归——本断言锁定"空 cwd 放行、真实 cwd 拒绝"的差异）。
	if _, err := validateGoWrapperArgs([]string{"build", "./x"}, "", goExecDir); err != nil {
		t.Fatalf("empty cwd must skip cwd check (fail-open only via client regression), got %v", err)
	}
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
// 消费存活管道 + 就绪 ACK，读一次生产 sendNotifyFDToHelper 发出的真实 'N'
// + SCM_RIGHTS 消息（关闭收到的临时 fd 防泄漏）；
// failClosedSyntheticModeEnv=="helper-ack-bad-byte" 时先回非 '1' 的 'X'，
// 然后阻塞读 lifeR 保持 ctrlFD 连接不断开（直至父侧 close(lifeWFD)），杜绝
// EPIPE/ECONNRESET 抢跑——父侧必须真实读到 'X' 而非任意发送错误。
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
	_, oobn, _, _, err := unix.Recvmsg(ctrlFD, buf, oob, 0)
	if err != nil {
		os.Exit(3)
	}
	// 关闭生产经 SCM_RIGHTS 传入的 notify fd 副本（本 synthetic 不消费 fd 本体）。
	if oobn > 0 {
		if scms, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
			for _, scm := range scms {
				if fds, ferr := unix.ParseUnixRights(&scm); ferr == nil {
					for _, fd := range fds {
						_ = unix.Close(fd)
					}
				}
			}
		}
	}
	if os.Getenv(failClosedSyntheticModeEnv) == "helper-ack-bad-byte" {
		// 回非法 ACK 字节 'X' 并保持连接：阻塞读 lifeR（父侧在断言完成
		// 后 close(lifeWFD) 唤醒本进程退出），确保父侧读到的是真实 'X'
		// 而非发送端已断开造成的 EPIPE/ECONNRESET。
		_, _ = unix.Write(ctrlFD, []byte("X"))
		var drain [8]byte
		_, _ = unix.Read(3, drain[:]) // 阻塞至父侧 close(lifeWFD) 触发 EOF
		_ = unix.Close(ctrlFD)
		_ = lifeR.Close()
		os.Exit(0)
	}
	_ = unix.Close(ctrlFD)
	_ = lifeR.Close()
	os.Exit(0)
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

	// 注：非法字节 'X' 分支已由 TestInvalidHelperAckRejectedByProductionPath
	// 经生产 sendNotifyFDToHelper 直接消费覆盖（helper-ack-bad-byte 变体，
	// 断言精确 byte='X' 诊断），此处的自写自读 'B'/'X' 伪子例已删除
	//（reviewer5 发现 2：synthetic 只能证明错误产生，不能证明生产消费）。
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
	t.Run("blocked_write_completes_full_byte", func(t *testing.T) {
		// 受控障碍（与真实内核 EINTR 分开标注）：填满管道写缓冲让 write
		// 阻塞，helperWriteAll 必须在 drain 释放空间后重试并完整写出
		// 1 字节——证明「写阻塞后正确重试/续写、最终校验写满 len(b)」的
		// 生产逻辑，不以注入的 EINTR 冒充内核事件。
		var pipeFDs [2]int
		if err := unix.Pipe2(pipeFDs[:], unix.O_NONBLOCK); err != nil {
			t.Fatalf("pipe2: %v", err)
		}
		rFD, wFD := pipeFDs[0], pipeFDs[1]
		defer unix.Close(rFD)
		defer unix.Close(wFD)

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
		if _, err := unix.FcntlInt(uintptr(wFD), unix.F_SETFL, 0); err != nil {
			t.Fatalf("clear O_NONBLOCK: %v", err)
		}

		// 排空协程：恰好读走 filled 字节，释放空间让阻塞 write 完成。
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

		if err := helperWriteAll(wFD, []byte("1")); err != nil {
			t.Fatalf("helperWriteAll on blocked-then-drained pipe = %v, want nil", err)
		}
		<-drainDone

		if _, err := unix.FcntlInt(uintptr(rFD), unix.F_SETFL, unix.O_NONBLOCK); err != nil {
			t.Fatalf("set rFD nonblock: %v", err)
		}
		var ack [1]byte
		n, rerr := unix.Read(rFD, ack[:])
		if rerr != nil || n != 1 || ack[0] != '1' {
			t.Fatalf("ACK byte not fully written: n=%d byte=%q err=%v", n, ack[0], rerr)
		}
	})

	t.Run("real_eintr_observed_or_unverified", func(t *testing.T) {
		// 真实内核 EINTR 观测：填满管道后向锁定线程投递 Tgkill(SIGURG)
		// 驱动 helperWriteEINTRObservedForTest 回调计数。本机（linux/arm64 +
		// Go runtime，SIGURG 走 SA_RESTART 异步抢占路径）已实证不产生
		// EINTR——此时如实 t.Logf UNVERIFIED 而非假绿或无界等待；可观测
		// 平台则断言 eintr_count>=1 证明重试路径真实命中。
		var pipeFDs [2]int
		if err := unix.Pipe2(pipeFDs[:], unix.O_NONBLOCK); err != nil {
			t.Fatalf("pipe2: %v", err)
		}
		rFD, wFD := pipeFDs[0], pipeFDs[1]
		defer unix.Close(rFD)
		defer unix.Close(wFD)

		filler := make([]byte, 4096)
		for {
			n, err := unix.Write(wFD, filler)
			if n <= 0 && err != nil {
				break
			}
		}
		if _, err := unix.FcntlInt(uintptr(wFD), unix.F_SETFL, 0); err != nil {
			t.Fatalf("clear O_NONBLOCK: %v", err)
		}

		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pid, tid := unix.Getpid(), unix.Gettid()

		var eintrCount int32
		firstEINTR := make(chan struct{})
		var once sync.Once
		helperWriteEINTRObservedForTest = func() {
			atomic.AddInt32(&eintrCount, 1)
			once.Do(func() { close(firstEINTR) })
		}
		t.Cleanup(func() { helperWriteEINTRObservedForTest = nil })

		stopSig := make(chan struct{})
		go func() {
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopSig:
					return
				case <-ticker.C:
					_ = unix.Tgkill(pid, tid, unix.SIGURG)
				}
			}
		}()
		defer close(stopSig)

		drainDone := make(chan struct{})
		startDrain := func() {
			go func() {
				defer close(drainDone)
				buf := make([]byte, 1<<20)
				_, _ = unix.Read(rFD, buf)
			}()
		}

		go func() {
			select {
			case <-firstEINTR:
				startDrain()
			case <-time.After(3 * time.Second):
				// 有界等待超时：无论是否观测到 EINTR 都排空防 write
				// 永久阻塞；观测结果由 eintrCount 如实报告。
				startDrain()
			}
		}()
		werr := helperWriteAll(wFD, []byte("1"))
		if werr != nil {
			t.Fatalf("helperWriteAll = %v, want nil", werr)
		}
		if atomic.LoadInt32(&eintrCount) < 1 {
			t.Logf("UNVERIFIED: real kernel EINTR not observed on this platform (eintr_count=0); retry-path coverage falls back to controlled blocked-write subtest")
		} else {
			t.Logf("real kernel EINTR observed: eintr_count=%d", atomic.LoadInt32(&eintrCount))
		}
		<-drainDone
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

	ctrlFD, lifeWFD, _, waitCh := spawnAckOnlyHelper(t, "synthetic-bad-byte", "")
	var ready [1]byte
	n, err := unix.Read(ctrlFD, ready[:])
	if err != nil || n != 1 || ready[0] != '1' {
		t.Fatalf("readiness ack: n=%d byte=%q err=%v", n, ready[0], err)
	}

	// notifyFD 占位（synthetic helper 收到后经 SCM_RIGHTS 关闭，不消费）。
	nsp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("notify socketpair: %v", err)
	}
	defer unix.Close(nsp[0])
	defer unix.Close(nsp[1])

	// 生产路径唯一入口：sendNotifyFDToHelper 发送真实 'N' + SCM_RIGHTS，
	// synthetic helper（helper-ack-bad-byte）收到 'N' 后回非法字节 'X' 并
	// 保持连接不断开——断言精确非法字节拒绝诊断（含 byte='X'），而非任意
	// EPIPE/ECONNRESET/EOF。删除生产 `ack[0] != '1'` 拒绝分支本断言即红。
	err = sendNotifyFDToHelper(ctrlFD, nsp[0])
	if err == nil {
		t.Fatalf("production sendNotifyFDToHelper must reject invalid ack 'X', got nil")
	}
	if !strings.Contains(err.Error(), "byte='X'") {
		t.Fatalf("sendNotifyFDToHelper error = %q, want invalid-byte diagnostic containing byte='X'", err)
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
	markerDir := filepath.Dir(marker)

	// 把生产 setupHelperAndSeccomp 的 helper 启动点替换为受控 helper：
	// 该 helper 真实建立 socketpair/存活管道并就绪 ACK '1'，但 notify 握手
	// 阶段读 'N' 后不回 ACK 直接退出（父侧生产 sendNotifyFDToHelper 收到
	// EOF → errSeccompFilterInstalled）。这仍是真实握手协议 + 真实
	// installSeccompExecGuard 装 filter + 真实生产 sendNotifyFDToHelper 路径，
	// 仅 helper 的 notify 应答被确定性控制。
	startHelperForSetup = func(p Policy, goExecDir, realGoBin string) (int, int, error) {
		ctrlFD, lifeWFD, waitCh := spawnSyntheticHelperChild()
		_ = waitCh // 存活至 Apply 失败路径关闭 lifeWFD 触发 helper 退出
		var ack [1]byte
		n, err := unix.Read(ctrlFD, ack[:])
		if err != nil || n != 1 || ack[0] != '1' {
			_ = unix.Close(ctrlFD)
			_ = unix.Close(lifeWFD)
			return -1, -1, fmt.Errorf("controlled helper readiness ack: n=%d byte=%q err=%v", n, ack[0], err)
		}
		return ctrlFD, lifeWFD, nil
	}

	var warnBuf bytes.Buffer
	p := Policy{
		ReadOnly:   []string{"/"},
		ReadWrite:  []string{markerDir},
		Exec:       []string{"/bin/true"},
		Env:        []string{"SBX_POSTINSTALL_ENV=1"},
		BestEffort: true,
		WarnWriter: &warnBuf,
	}
	applyErr := Apply(p)
	result := "unexpected"
	switch {
	case applyErr == nil:
		// 变异/缺陷形态：post-install 失败被 BestEffort 降级吞掉继续，
		// 且 Env 被应用——本用例预期红路径（不命中即绿）。
		result = "degraded-nil"
		if os.Getenv("SBX_POSTINSTALL_ENV") == "1" {
			result = "degraded-nil-env-applied"
		}
	case errors.Is(applyErr, ErrSeccomp):
		// 正确分型：post-install 失败上抛 ErrSeccomp；同时断言未降级且
		// Env 未应用。
		if strings.Contains(warnBuf.String(), "degrading without ld-linux") {
			result = "seccomp-but-degraded"
		} else if os.Getenv("SBX_POSTINSTALL_ENV") != "" {
			result = "seccomp-but-env-applied"
		} else {
			result = "post-install-err"
		}
	default:
		result = "non-seccomp-err:" + applyErr.Error()
	}
	if marker != "" {
		_ = os.WriteFile(marker, []byte(result), 0o600)
	}
	os.Exit(0)
}

// TestBestEffortPostInstallMustNotDegrade 覆盖 M6b：真实 Apply(BestEffort=true)
// 消费已装入 filter 后的 notify 握手失败——post-install 失败必须返回
// ErrSeccomp、不降级、不应用 Env，绝不因 BestEffort 放行继续。
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
		t.Fatalf("post-install Apply failure must return ErrSeccomp without degradation, marker=%q\noutput: %s", content, out)
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
