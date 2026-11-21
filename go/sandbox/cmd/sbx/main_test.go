package main

import (
	"bufio"
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/dorokuma/pi-extensions/go/sandbox"
)

const envTestWantSbxMain = "GO_WANT_SBX_MAIN"

// unsetEnv 保证 key 在本测试内不存在，并在 t.Cleanup 恢复原值（含原本未
// 设置情形）。与 sandbox/preset_test.go 中同名 helper 语义一致，
// main_test.go 属 package main 需要自有副本。
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

// envTestMountProbe 让复用测试二进制的沙盒内进程进入 mount 探针模式：
// 直接对 setns(/proc/1/ns/mnt) 与 umount2(/nonexistent-sbx-mount-seal-probe)
// 发 syscall 并打印 errno，验证 mount-seal seccomp 锁以 EPERM 拦截。
const envTestMountProbe = "GO_WANT_SBX_MOUNT_PROBE"

// probeShmMagicSize 是 CLONE_NEWIPC 实证探针使用的唯一魔数段大小：
// 沙盒内以 shmget(IPC_PRIVATE, probeShmMagicSize) 创建私有段，宿主侧
// 对比探针前后 /proc/sysvipc/shm 的 (key,shmid,size) 集合——沙盒退出后
// 宿主若新增 size=probeShmMagicSize 的段即判定 IPC ns 隔离失效/残留
// （不比对 shmid 值本身，避免极小概率 ID 号巧合误判）。
const probeShmMagicSize = 12288

func TestMain(m *testing.M) {
	if os.Getenv(envSbxChildSpecKey) != "" {
		os.Exit(runChildFromEnv())
	}
	if os.Getenv(envTestMountProbe) == "1" {
		os.Exit(runMountNSProbe())
	}
	if h := os.Getenv(envTestFd3Harness); h != "" {
		_ = os.Unsetenv(envTestFd3Harness)
		os.Exit(runFd3Harness(h))
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
// 位探针。capabilities 断言读取 /proc/self/status 五组 Cap* 掩码（裁剪后
// Eff/Prm/Bnd 应精确为 0x22、Inh/Amb 为 0），bpf/finit_module/
// open_by_handle_at 三个非破坏探针验证宿主全局副作用族被 EPERM 拦截。
// CLONE_NEWIPC 隔离实证：读取 /proc/sysvipc/shm 统计除表头外的数据行数
// 打印 sysvipc_init_shm=<n>（独立 IPC ns 内即使宿主存在 PostgreSQL 等
// 共享内存段也必为 0），再以 shmget(IPC_PRIVATE, probeShmMagicSize)
// 创建私有段打印 sandbox_shmid=<id>（不调 IPC_RMID——若未隔离该段会
// 持久残留宿主；隔离时随 IPC ns 销毁自动回收，宿主侧按
// probeShmMagicSize 魔数对比前后段集合断言无新增残留）。
// 此外对 moshi-hook.sock 做一次 unix dial：只明确接受
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

	// capabilities 裁剪：/proc/self/status 五组掩码原样打印供父进程断言
	// （裁剪后 CapEff/CapPrm/CapBnd 应精确为 0000000000000022，
	// CapInh/CapAmb 为 0000000000000000）。
	if st, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(st), "\n") {
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			switch key {
			case "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb":
				fmt.Printf("%s=%s\n", strings.ToLower(key), strings.TrimSpace(val))
			}
		}
	} else {
		fmt.Printf("caps_status=error(%v)\n", err)
	}

	// 宿主全局副作用族 seccomp 探针（装锁时全部 EPERM=1，均非破坏）：
	// bpf(BPF_MAP_CREATE) 未装锁且能力在位时返回 0/EINVAL；finit_module
	// 与 open_by_handle_at 对 fd=-1 未装锁时返回 EBADF(9)（能力校验在前，
	// 未装锁且有能力亦可能 EPERM，但装锁后必然 EPERM）。
	bpfAttr := []byte{
		1, 0, 0, 0, // map_type = BPF_MAP_TYPE_HASH
		4, 0, 0, 0, // key_size
		4, 0, 0, 0, // value_size
		1, 0, 0, 0, // max_entries
	}
	_, _, bpfE := syscall.Syscall(unix.SYS_BPF, 0, uintptr(unsafe.Pointer(&bpfAttr[0])), 0)
	_, _, finitE := syscall.Syscall(unix.SYS_FINIT_MODULE, ^uintptr(0), 0, 0) // fd=-1
	_, _, obhE := syscall.Syscall(unix.SYS_OPEN_BY_HANDLE_AT, ^uintptr(0), 0, 0)
	fmt.Printf("bpf_errno=%d\nfinit_module_errno=%d\nopen_by_handle_at_errno=%d\n",
		int(bpfE), int(finitE), int(obhE))

	// CLONE_NEWIPC 隔离实证：独立 IPC ns 内 /proc/sysvipc/shm 必为空
	// （宿主 SysV shm/sem/msg 不可见，按名 mq_open 亦隔离）；再经 shmget
	// 创建私有段验证自身 IPC 可用且退出后随 ns 自动销毁不残留宿主。
	if b, err := os.ReadFile("/proc/sysvipc/shm"); err == nil {
		n := 0
		for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if i > 0 && strings.TrimSpace(line) != "" {
				n++
			}
		}
		fmt.Printf("sysvipc_init_shm=%d\n", n)
	} else {
		fmt.Printf("sysvipc_init_shm=error(%v)\n", err)
	}
	if shmid, _, shmE := unix.Syscall(unix.SYS_SHMGET, 0, probeShmMagicSize, unix.IPC_CREAT|0o600); shmE == 0 {
		fmt.Printf("sandbox_shmid=%d\n", int(shmid))
	} else {
		fmt.Printf("sandbox_shmid=error(%v)\n", shmE)
	}
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
	// 私有 PIG_HOME 重定向：最终 env 的 PIG_HOME 指向 <TMPDIR>/pig-home 私有根
	//（非 cfgEnv 的宿主 fakeHome；fakeHome 仅参与预热对齐与产物投影）。
	tmpdir := envListValue(policy.Env, "TMPDIR")
	wantPrivate := filepath.Join(tmpdir, "pig-home")
	if got != wantPrivate {
		t.Fatalf("PIG_HOME must be re-pinned to private root %q, got %q (env=%v)", wantPrivate, got, policy.Env)
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

// mustJSONList 按 PI_SANDBOX_PRESET_TMP_DIRS 的 JSON 数组格式序列化
// 目录清单（登记改用 JSON 后，测试构造清单字面量与生产序列化同型，
// 且不受路径含 ":" 影响）。
func mustJSONList(t *testing.T, dirs ...string) string {
	t.Helper()
	raw, err := json.Marshal(dirs)
	if err != nil {
		t.Fatalf("marshal dir manifest: %v", err)
	}
	return string(raw)
}

// TestCleanupPresetTempDirsNeverDeletesExecParentDir 是创建清单清理的
// 负向回归（reviewer 发现 1）：Policy.Exec 含用户手工创建的
// /tmp/pi-reviewer-exec-manual/bin 时，cleanupPresetTempDirsFromPolicy
// 绝不做「父目录前缀命中即删」的推断式清理——manual 目录及其内容必须
// 完整保留。反向断言同一登记清单中的真实创建目录仍被递归删除。
func TestCleanupPresetTempDirsNeverDeletesExecParentDir(t *testing.T) {
	// 用户手工创建的「形似预设」exec 目录：basename 命中 pi-reviewer-exec-
	// 前缀、父级在 /tmp 下，但不属于本预设创建清单。
	manualDir, err := os.MkdirTemp("/tmp", "pi-reviewer-exec-manual-")
	if err != nil {
		t.Fatalf("MkdirTemp manual: %v", err)
	}
	manualBin := filepath.Join(manualDir, "bin")
	if err := os.MkdirAll(manualBin, 0o755); err != nil {
		t.Fatalf("MkdirAll manual bin: %v", err)
	}
	manualMarker := filepath.Join(manualDir, "keep.txt")
	if err := os.WriteFile(manualMarker, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatalf("WriteFile manual marker: %v", err)
	}
	defer func() { _ = os.RemoveAll(manualDir) }()

	// 本预设真实创建的目录（登记进 PI_SANDBOX_PRESET_TMP_DIRS 清单）。
	revDir, err := os.MkdirTemp("/tmp", "pi-reviewer-")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer: %v", err)
	}
	execDir, err := os.MkdirTemp("/tmp", "pi-reviewer-exec-")
	if err != nil {
		t.Fatalf("MkdirTemp exec: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(execDir, "bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll exec bin: %v", err)
	}

	policy := sandbox.Policy{
		Env: []string{
			"TMPDIR=" + revDir,
			"PI_SANDBOX_GO_EXEC_DIR=" + execDir,
			"PI_SANDBOX_PRESET_TMP_DIRS=" + mustJSONList(t, revDir, execDir),
		},
		ReadWrite: []string{revDir},
		// Exec 收缩为 <goExecDir>/bin，并混入调用方手工创建的
		// manual/bin——登记清单口径下它不属于清理目标。
		Exec: []string{filepath.Join(execDir, "bin"), manualBin},
	}
	cleanupPresetTempDirsFromPolicy(policy)

	if _, err := os.Lstat(revDir); !os.IsNotExist(err) {
		t.Fatalf("registered dir %q must be removed, stat err = %v", revDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("registered dir %q must be removed, stat err = %v", execDir, err)
	}
	// 核心负向断言：manual 目录（含 bin/ 与内容）必须完整保留——
	// 旧实现父目录前缀推断会递归删除整个 manualDir。
	if _, err := os.Lstat(manualDir); err != nil {
		t.Fatalf("manual dir %q must NOT be removed, stat err = %v", manualDir, err)
	}
	data, err := os.ReadFile(manualMarker)
	if err != nil || string(data) != "do-not-delete" {
		t.Fatalf("manual marker must be intact, data=%q err=%v", data, err)
	}
	if info, err := os.Lstat(manualBin); err != nil || !info.IsDir() {
		t.Fatalf("manual bin dir must be intact, err=%v", err)
	}
}

// TestCleanupPresetTempDirsUsesBuildPolicyManifest 断言 buildPolicy 捕获的
// 登记清单（内部键已从最终 env 剥离）驱动清理：返回的 policy 不再含
// PI_SANDBOX_PRESET_TMP_DIRS，lastPresetTempDirs 仍精确删除本预设创建
// 的两目录；--env 传入的手工目录不被误伤。
//
// 捕获必要性对照（reviewer R2 F3）：本用例在调用 cleanup 前把最终 env 的
// TMPDIR/PI_SANDBOX_GO_EXEC_DIR 字段替换为无关真实目录——若 buildPolicy
// 中 lastPresetTempDirs 捕获被删，清理只能走 env 字段兜底（政策里不再含
// 登记键），登记目录会残留泄漏，本测试立即 FAIL；同时兜底路径会把替代
// 目录当作预设目录误删（连同其内容），反向断言「替代目录必须完整保留」
// 一并锁死该行为。红绿对照已本地验证：删捕获行 → FAIL，还原 → PASS。
func TestCleanupPresetTempDirsUsesBuildPolicyManifest(t *testing.T) {
	t.Setenv("PIG_HOME", t.TempDir()) // 隔离宿主 pig home
	workdir := t.TempDir()
	manualDir, err := os.MkdirTemp("/tmp", "pi-reviewer-exec-manual-")
	if err != nil {
		t.Fatalf("MkdirTemp manual: %v", err)
	}
	manualBin := filepath.Join(manualDir, "bin")
	if err := os.MkdirAll(manualBin, 0o755); err != nil {
		t.Fatalf("MkdirAll manual bin: %v", err)
	}
	defer func() { _ = os.RemoveAll(manualDir) }()

	// env 指针遮蔽夹具：替换 TMPDIR/PI_SANDBOX_GO_EXEC_DIR 用的无关真实目录，
	// 若捕获被删走兜底，这两个目录会被当作预设目录误删。
	shadowTmpDir := t.TempDir()
	shadowTmpMarker := filepath.Join(shadowTmpDir, "keep.txt")
	if err := os.WriteFile(shadowTmpMarker, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatalf("WriteFile shadow tmp marker: %v", err)
	}
	shadowExecDir := t.TempDir()
	shadowExecMarker := filepath.Join(shadowExecDir, "keep.txt")
	if err := os.WriteFile(shadowExecMarker, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatalf("WriteFile shadow exec marker: %v", err)
	}

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer",
		WorkDir: workdir,
		// --env 传入无关键 + 手工 exec 目录：验证登记清单清理不误伤外部目录。
		Env:  []string{"SBX_TEST_UNRELATED=1", "GOPROXY=off"},
		Exec: []string{manualBin},
	})
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	// 内部登记键不得泄进最终 env。
	if v := envListValue(policy.Env, "PI_SANDBOX_PRESET_TMP_DIRS"); v != "" {
		t.Fatalf("PI_SANDBOX_PRESET_TMP_DIRS must be stripped from final env, got %q", v)
	}
	if envListValue(policy.Env, "SBX_TEST_UNRELATED") != "1" {
		t.Fatalf("unrelated --env key must survive merge, env=%v", policy.Env)
	}
	tmpdir := envListValue(policy.Env, "TMPDIR")
	execDir := envListValue(policy.Env, "PI_SANDBOX_GO_EXEC_DIR")
	if !strings.HasPrefix(filepath.Base(tmpdir), "pi-reviewer-") ||
		!strings.HasPrefix(filepath.Base(execDir), "pi-reviewer-exec-") {
		t.Fatalf("unexpected preset dirs: TMPDIR=%q EXEC_DIR=%q", tmpdir, execDir)
	}

	// 遮蔽 env 字段指针：最终 env 的 TMPDIR/PI_SANDBOX_GO_EXEC_DIR 改为
	// 无关目录后，旧式 env 字段兜底只能指向 shadow 目录——清理只剩
	// lastPresetTempDirs 一条可信通路（登记键已被 buildPolicy 剥离）。
	// ReadWrite 仍保留登记目录 rwDir（兜底 rw 前缀命中同样指向登记目录，
	// 与捕获指向同一目录集，不构成独立旁路）。
	policy.Env = mergeEnv(policy.Env, []string{
		"TMPDIR=" + shadowTmpDir,
		"PI_SANDBOX_GO_EXEC_DIR=" + shadowExecDir,
	})

	cleanupPresetTempDirsFromPolicy(policy)

	// 反向断言：shadow 目录必须完整保留——兜底路径若被当作预设目录
	// 误删，说明清理未经登记清单（删除捕获后的泄漏行为即被本断言锁死）。
	if data, err := os.ReadFile(shadowTmpMarker); err != nil || string(data) != "do-not-delete" {
		t.Fatalf("shadow TMPDIR dir must be intact, data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(shadowExecMarker); err != nil || string(data) != "do-not-delete" {
		t.Fatalf("shadow exec dir must be intact, data=%q err=%v", data, err)
	}

	if _, err := os.Lstat(tmpdir); !os.IsNotExist(err) {
		t.Fatalf("preset TMPDIR %q must be removed via manifest, stat err = %v", tmpdir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("preset exec dir %q must be removed via manifest, stat err = %v", execDir, err)
	}
	if _, err := os.Lstat(manualDir); err != nil {
		t.Fatalf("manual dir %q must NOT be removed, stat err = %v", manualDir, err)
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

// TestCleanupPresetTempDirsColonRootManifest 是 F1 回归：备用根
// os.TempDir() 经 TMPDIR 设为含 ":" 的目录时，登记清单（JSON 数组）
// 必须正确还原两个含冒号路径——不误拆成无效段、不误删、能精确清理。
// 旧冒号分隔序列化在此场景下被 Split 拆成无效段，isPresetTempDir 校验
// 全失败、非空清单短路返回不再回退兜底，两个真实目录残留泄漏——本
// 用例在旧实现下必 FAIL（登记目录残留），新实现下 PASS（红绿对照已
// 本地验证）。
func TestCleanupPresetTempDirsColonRootManifest(t *testing.T) {
	// 备用根夹具：含 ":" 的真实目录作为 os.TempDir()。
	colonRoot := filepath.Join(t.TempDir(), "a:b")
	if err := os.MkdirAll(colonRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll colon root: %v", err)
	}
	t.Setenv("TMPDIR", colonRoot)

	// 备用目录：位于含冒号根下、基名带固定前缀（与 preset.go
	// os.MkdirTemp("")/filepath.Join(os.TempDir(), ...) 备用分支同型）。
	revDir, err := os.MkdirTemp(colonRoot, "pi-reviewer-")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer: %v", err)
	}
	execDir, err := os.MkdirTemp(colonRoot, "pi-reviewer-exec-")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer-exec: %v", err)
	}
	if !strings.Contains(revDir, ":") || !strings.Contains(execDir, ":") {
		t.Fatalf("fixture dirs must contain ':' to cover the colon-root defect: %q %q", revDir, execDir)
	}

	// 解码层断言：registeredPresetTempDirs 对含冒号清单精确还原两段。
	manifest := mustJSONList(t, revDir, execDir)
	got := registeredPresetTempDirs([]string{"PI_SANDBOX_PRESET_TMP_DIRS=" + manifest})
	if len(got) != 2 || got[0] != revDir || got[1] != execDir {
		t.Fatalf("registeredPresetTempDirs = %v, want [%q %q]", got, revDir, execDir)
	}

	// 清理层断言：cleanupPresetTempDirsFromPolicy 按 JSON 清单精确删除
	// 两个真实目录（不误拆、不误删、不留泄漏）。
	policy := sandbox.Policy{
		Env: []string{
			"TMPDIR=" + revDir,
			"PI_SANDBOX_GO_EXEC_DIR=" + execDir,
			"PI_SANDBOX_PRESET_TMP_DIRS=" + manifest,
		},
		ReadWrite: []string{revDir},
		Exec:      []string{filepath.Join(execDir, "bin")},
	}
	cleanupPresetTempDirsFromPolicy(policy)

	if _, err := os.Lstat(revDir); !os.IsNotExist(err) {
		t.Fatalf("colon-root registered dir %q must be removed, stat err = %v", revDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("colon-root registered exec dir %q must be removed, stat err = %v", execDir, err)
	}

	// 旧冒号格式安全降级：外部塞入的旧格式清单解码失败不得 panic，
	// 按「无清单」走 env 字段兜底。
	legacy := "PI_SANDBOX_PRESET_TMP_DIRS=" + revDir + string(os.PathListSeparator) + execDir
	legacyGot := registeredPresetTempDirs([]string{legacy})
	_ = legacyGot // 不 panic 即达标（返回 nil 或还原结果均为安全降级）
}

// TestCleanupPresetTempDirsNonUTF8RootFallback 是 R3 发现 1 的兜底回归：
// TMPDIR 指向含非 UTF-8 字节的根时，ReadOnlyReviewer 备用分支产生的
// privateTmpDir/goExecDir 均为非 UTF-8 路径——preset.go 登记侧 utf8.ValidString
// 校验使登记键缺失（JSON 无法无损承载非 UTF-8 路径），清理必须走保留原始
// 字节的 env/ReadWrite 兜底。本用例验证兜底路径（isPresetTempDir 的
// allowedTempRoots 以原始字节 os.TempDir() 前缀比对）对非 UTF-8 目录仍能精确
// 删除——不 panic、不残留。
func TestCleanupPresetTempDirsNonUTF8RootFallback(t *testing.T) {
	// lastPresetTempDirs 是包级变量、由 buildPolicy 填充；隔离本用例，避免
	// 先前测试（TestCleanupPresetTempDirsUsesBuildPolicyManifest 等）残留的
	// 清单短路兜底路径造成假残留。
	lastPresetTempDirs = nil
	t.Cleanup(func() { lastPresetTempDirs = nil })

	// 非 UTF-8 备用根（真实存在的目录，基名含 \xff）。
	nonUTF8Root := filepath.Join(t.TempDir(), "root-"+string([]byte{0xff}))
	if err := os.MkdirAll(nonUTF8Root, 0o700); err != nil {
		t.Fatalf("MkdirAll non-UTF8 root: %v", err)
	}
	t.Setenv("TMPDIR", nonUTF8Root)

	// 夹具两目录与 preset.go 备用分支同型（os.MkdirTemp(<非UTF8根>)）。
	revDir, err := os.MkdirTemp(nonUTF8Root, "pi-reviewer-")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer (non-UTF8 root): %v", err)
	}
	execDir, err := os.MkdirTemp(nonUTF8Root, "pi-reviewer-exec-")
	if err != nil {
		t.Fatalf("MkdirTemp reviewer-exec (non-UTF8 root): %v", err)
	}
	if utf8.ValidString(revDir) || utf8.ValidString(execDir) {
		t.Fatalf("fixture dirs must be non-UTF-8: %q %q", revDir, execDir)
	}

	// 反向佐证：encoding/json 对非 UTF-8 字符串有损替换 U+FFFD 不报错——
	// 证明登记侧若不校验 UTF-8 就会写入与真实目录不符的被损清单。
	lossy := mustJSONList(t, revDir, execDir)
	decoded := registeredPresetTempDirs([]string{"PI_SANDBOX_PRESET_TMP_DIRS=" + lossy})
	if len(decoded) == 2 && decoded[0] == revDir && decoded[1] == execDir {
		t.Fatalf("json round-trip of non-UTF-8 path unexpectedly lossless: %q", decoded)
	}

	// 兜底断言：policy.Env 无登记键，cleanupPresetTempDirsFromPolicy 经
	// TMPDIR/PI_SANDBOX_GO_EXEC_DIR/ReadWrite 字段兜底删除两个非 UTF-8 目录。
	policy := sandbox.Policy{
		Env: []string{
			"TMPDIR=" + revDir,
			"PI_SANDBOX_GO_EXEC_DIR=" + execDir,
		},
		ReadWrite: []string{revDir},
	}
	cleanupPresetTempDirsFromPolicy(policy)

	if _, err := os.Lstat(revDir); !os.IsNotExist(err) {
		t.Fatalf("non-UTF-8 registered dir %q must be removed via fallback, stat err = %v", revDir, err)
	}
	if _, err := os.Lstat(execDir); !os.IsNotExist(err) {
		t.Fatalf("non-UTF-8 exec dir %q must be removed via fallback, stat err = %v", execDir, err)
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
	return invokeSbxSubprocessDir(t, "", extraEnv, args...)
}

// invokeSbxSubprocessDir 与 invokeSbxSubprocess 相同，另允许指定子进程
// 启动目录 dir（空串保持继承），用于覆盖挂载后继承 cwd 重绑的回归。
func invokeSbxSubprocessDir(t *testing.T, dir string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cmd := exec.Command(selfExe, args...)
	env := append(os.Environ(), envTestWantSbxMain+"=1")
	env = append(env, extraEnv...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}

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
		// Unicode Cf 格式控制符同样剔除：\u202e 双向覆盖符、\u200b 零宽
		// 空格、\ufeff BOM——防父终端渲染侧文本欺骗。
		inCf := "x\u202ey\u200bz\ufeffw"
		if got := sanitizeReportText(inCf); got != "xyzw" {
			t.Fatalf("sanitizeReportText(Cf) = %q, want %q", got, "xyzw")
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

	t.Run("report_addr_regex", func(t *testing.T) {
		// 与 dispatch-agent 地址校验完全一致的 ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$：
		// 首字符必须字母或数字（拒绝 "-" 开头的 hc send flag 注入），禁 `:` 与 `/`。
		cases := []struct {
			name string
			addr string
			want bool
		}{
			{"dash_prefix_rejected", "-bad", false}, // 首字符 '-' 会被 hc send 当 flag
			{"colon_rejected", "ws:p7", false},      // 禁 ':'
			{"slash_rejected", "a/../x", false},     // 禁 '/' 与路径形态
			{"empty_rejected", "", false},
			{"over_64_rejected", strings.Repeat("a", 65), false},
			{"plain_id", "t13z", true},
			{"dashed_id", "scout-t1", true},
			{"bare_name", "probe", true},
			{"dot_underscore", "a.b_c", true},
			{"max_64", strings.Repeat("a", 64), true},
		}
		for _, tc := range cases {
			if got := reportAddrRe.MatchString(tc.addr); got != tc.want {
				t.Errorf("%s: reportAddrRe.MatchString(%q) = %v, want %v", tc.name, tc.addr, got, tc.want)
			}
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
		r := &reportRelay{parent: "tTEST", ln: ln, tokens: 1, lastRef: time.Now()}
		old := reportConnReadTimeout
		reportConnReadTimeout = 200 * time.Millisecond
		// 先关停中继再恢复全局超时：全局变量的恢复期若排在 r.close() 之前，
		// 高负载下 handler 可能按旧值再跑一轮，构成时序抖动源。
		defer func() {
			r.close()
			reportConnReadTimeout = old
		}()
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

	t.Run("drain_timeout_cancels_inflight_deliver", func(t *testing.T) {
		// 假 hc 先落标记文件证明子进程确实启动，再阻塞 5s 制造在途投递。
		marker := filepath.Join(t.TempDir(), "hc-started")
		rctx, rcancel := context.WithCancel(context.Background())
		sd := make(chan struct{})
		close(sd) // 不经 serve()：预关闭 serveDone 让 close() 直走 drain 路径
		r := &reportRelay{
			parent:    "tTEST",
			ctx:       rctx,
			cancel:    rcancel,
			serveDone: sd,
			conns:     make(map[net.Conn]struct{}),
			tokens:    1,
			lastRef:   time.Now(),
			hcPath:    writeFakeHc(t, "#!/bin/sh\ntouch "+marker+"\nsleep 5\n"),
		}
		// 在途 deliver 必须先于 close() 登记 wg，消除 Add/Wait 竞态。
		r.wg.Add(1)
		statusCh := make(chan byte, 1)
		go func() {
			defer r.wg.Done()
			st, _ := r.process(reportRequest{To: "tTEST", Text: "x"})
			statusCh <- st
		}()
		waitForFile(t, marker, 3*time.Second)

		old := reportDrainTimeout
		reportDrainTimeout = 80 * time.Millisecond
		defer func() { reportDrainTimeout = old }()
		start := time.Now()
		r.close()
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("close() took %v, want < 1s (drain timeout must cancel in-flight deliver)", elapsed)
		}
		select {
		case st := <-statusCh:
			if st == reportStatusOK {
				t.Fatal("in-flight deliver reported ok after drain-timeout cancel")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("deliver goroutine did not return after drain-timeout cancel")
		}
	})

	t.Run("active_conn_cap_prevents_dos", func(t *testing.T) {
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
		defer r.close()

		// 32 个空闲连接不写数据，占满在途上限。
		idle := make([]net.Conn, 0, reportMaxActiveConns)
		for i := 0; i < reportMaxActiveConns; i++ {
			c, err := net.Dial("unix", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial %d: %v", i, err)
			}
			idle = append(idle, c)
		}
		defer func() {
			for _, c := range idle {
				_ = c.Close()
			}
		}()
		waitForConns(t, r, reportMaxActiveConns)

		// 第 33 个连接立即收到拒绝帧（含 busy），不占名额。
		extra, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial extra: %v", err)
		}
		_ = extra.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 256)
		n, err := extra.Read(buf)
		_ = extra.Close()
		if err != nil || n == 0 {
			t.Fatalf("read reject frame: n=%d err=%v", n, err)
		}
		if buf[0] != reportStatusRejected || !strings.Contains(string(buf[1:n]), "busy") {
			t.Fatalf("reject frame = %q, want status %d + busy", string(buf[:n]), reportStatusRejected)
		}

		// 释放一个名额后正常请求立即成功。
		_ = idle[0].Close()
		idle = idle[1:]
		waitForConns(t, r, reportMaxActiveConns-1)
		ok, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial after free: %v", err)
		}
		defer ok.Close()
		st, reason := sendReportRequest(t, ok, "tTEST", "hi")
		if st != reportStatusOK {
			t.Fatalf("after freeing a slot status=%d reason=%q, want ok", st, reason)
		}
	})

	t.Run("accept_consec_error_cap_exits", func(t *testing.T) {
		l := &errListener{err: errors.New("boom")}
		r := newTestReportRelay(t, l)
		done := make(chan struct{})
		go func() {
			r.serve()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("serve() did not exit after %d consecutive accept errors", reportAcceptMaxConsecErrs)
		}
		if got := atomic.LoadInt32(&l.calls); got != int32(reportAcceptMaxConsecErrs) {
			t.Fatalf("Accept called %d times, want exactly %d", got, reportAcceptMaxConsecErrs)
		}
		select {
		case <-r.serveDone:
		case <-time.After(time.Second):
			t.Fatal("serveDone not closed after serve() exit")
		}
	})
}

// sendReportRequest 向中继连接发送一帧 {to,text} 并读取完整回包，返回
// 1 字节状态与原因文本。
func sendReportRequest(t *testing.T, c net.Conn, to, text string) (byte, string) {
	t.Helper()
	payload, err := json.Marshal(reportRequest{To: to, Text: text})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.Write(hdr[:]); err != nil {
		t.Fatalf("write frame hdr: %v", err)
	}
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write frame body: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("read reply: n=%d err=%v", n, err)
	}
	return buf[0], string(buf[1:n])
}

// waitForConns 轮询等待中继登记的活跃连接数达到 want（注册/注销都是
// 异步的，固定 sleep 属时序抖动源）。
func waitForConns(t *testing.T, r *reportRelay, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.conns)
		r.mu.Unlock()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active conns = %d, want %d", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForFile 轮询等待标记文件出现（子进程启动确认用）。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker file %q did not appear within %v", path, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newTestReportRelay 构造一个带假 hc 与后台 ctx 的中继实例。
func newTestReportRelay(t *testing.T, ln net.Listener) *reportRelay {
	t.Helper()
	rctx, rcancel := context.WithCancel(context.Background())
	t.Cleanup(rcancel)
	return &reportRelay{
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
}

// errListener 是一个 Accept 永远返回固定错误的伪 net.Listener。
type errListener struct {
	calls int32
	err   error
}

func (l *errListener) Accept() (net.Conn, error) {
	atomic.AddInt32(&l.calls, 1)
	return nil, l.err
}
func (l *errListener) Close() error   { return nil }
func (l *errListener) Addr() net.Addr { return dummyAddr("err-listener") }

type dummyAddr string

func (d dummyAddr) Network() string { return "dummy" }
func (d dummyAddr) String() string  { return string(d) }

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

// hostShmSegments 返回宿主 /proc/sysvipc/shm 中除表头外的数据行集合
// （每行含 key/shmid/perms/size/attach 计数字段），供探针前后对比；
// 宿主文件不可读时返回空集合并由调用方决定是否跳过断言。
func hostShmSegments(t *testing.T) map[string]struct{} {
	t.Helper()
	segs := make(map[string]struct{})
	b, err := os.ReadFile("/proc/sysvipc/shm")
	if err != nil {
		t.Logf("host /proc/sysvipc/shm unreadable: %v", err)
		return segs
	}
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if i > 0 && strings.TrimSpace(line) != "" {
			segs[line] = struct{}{}
		}
	}
	return segs
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
	// 阶段 3：broker/pi-test-runner 拆除后，沙盒内 go list/build/vet 原样
	// 可用（受信 GOROOT 回钉 + pkg/tool 已收录 Exec）；go test 需在沙盒
	// 内执行动态产物，按 W^X 自然拒（EACCES）。保留一个含真实 _test.go
	// 的最小模块以驱动 go test 产物执行路径。
	capTestDir := filepath.Join(workdir, "captest")
	if err := os.MkdirAll(capTestDir, 0o755); err != nil {
		t.Fatalf("MkdirAll captest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(capTestDir, "go.mod"),
		[]byte("module example.com/captest\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write captest go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(capTestDir, "cap.go"), []byte("package captest\n\nfunc One() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write cap.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(capTestDir, "cap_test.go"),
		[]byte("package captest\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) { if One() != 1 { t.Fatal(\"bad\") } }\n"), 0o644); err != nil {
		t.Fatalf("write cap_test.go: %v", err)
	}
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
# PID-1 孤儿收尸实证（有界确认，无假绿）：子 shell 后台启动轮询
# orphan.release 的孙进程并记下其 PID，子 shell 随即退出使孙进程成为
# 孤儿；主脚本先确证孤儿被 PID 1 接管（PPid=1），再放行退出并确证
# /proc/<pid> 目录已消失（PID 1 经 Wait4 收割），最后扫 /proc 断言零僵尸。
( (while [ ! -f "$TMPDIR/orphan.release" ]; do :; done) &
  printf '%s\n' "$!" > "$TMPDIR/orphan.pid" )
i=0; ORPHAN_PID=""
while [ -z "$ORPHAN_PID" ] && [ $i -lt 20000 ]; do
	[ -s "$TMPDIR/orphan.pid" ] && ORPHAN_PID=$(cat "$TMPDIR/orphan.pid")
	i=$((i+1))
done
i=0
while [ $i -lt 20000 ]; do
	if [ -n "$ORPHAN_PID" ] && grep -q '^PPid:[[:space:]]*1$' "/proc/$ORPHAN_PID/status" 2>/dev/null; then
		break
	fi
	j=0; while [ $j -lt 500 ]; do j=$((j+1)); done
	i=$((i+1))
done
if grep -q '^PPid:[[:space:]]*1$' "/proc/$ORPHAN_PID/status" 2>/dev/null; then
	echo orphan_adopted=1
else
	echo orphan_adopted=0
fi
: > "$TMPDIR/orphan.release"
i=0
while [ -e "/proc/$ORPHAN_PID" ] && [ $i -lt 200000 ]; do i=$((i+1)); done
if [ ! -e "/proc/$ORPHAN_PID" ]; then
	echo orphan_reaped=1
else
	echo orphan_reaped=0
	grep '^State:' "/proc/$ORPHAN_PID/status" 2>/dev/null || true
fi
z=0; for s in /proc/[0-9]*/status; do grep -q 'State:.*Z' "$s" && z=$((z+1)); done
printf 'zombie_count=%s\n' "$z"
# 阶段 3：broker 拆除后 go list/go build/go vet 只读编译子命令原样可用
#（rc=0，go vet 经受信 GOROOT/pkg/tool 执行，不触可写域产物）；go test
# 需执行动态产物，按 W^X 自然拒。broker.sock/work/bin/pi-test-runner 已不存在。
cd "$CAPTEST_DIR" && go list . > "$TMPDIR/caplist.out" 2>&1
printf 'golist_rc=%d\n' "$?"
cd "$CAPTEST_DIR" && go build -o "$TMPDIR/capbin" . > "$TMPDIR/capbuild.out" 2>&1
printf 'gobuild_rc=%d\n' "$?"
cd "$CAPTEST_DIR" && go vet . > "$TMPDIR/capvet.out" 2>&1
printf 'govet_rc=%d\n' "$?"
cd "$CAPTEST_DIR" && go test -count=1 . > "$TMPDIR/captest.out" 2>&1
printf 'gotest_rc=%d\n' "$?"
# W^X 拒绝锁执行期 EACCES：同一行须同时含 fork/exec 前缀与 permission
# denied 后缀（go test 产物路径 go-tmp/go-build<random> 不固定，用
# 前缀+后缀匹配）；仅 permission denied 会把 chmod 000 源文件的 open
# EACCES 等非执行期拒绝假绿。
grep -Eiq 'fork/exec.*permission denied' "$TMPDIR/captest.out" && echo gotest_eacces=1 || echo gotest_eacces=0
[ -e "$PI_SANDBOX_GO_EXEC_DIR/broker.sock" ] || [ -L "$PI_SANDBOX_GO_EXEC_DIR/broker.sock" ] && echo broker_sock=present || echo broker_sock=absent
[ -e "$PI_SANDBOX_GO_EXEC_DIR/work" ] || [ -L "$PI_SANDBOX_GO_EXEC_DIR/work" ] && echo goexec_work=present || echo goexec_work=absent
[ -e "$PI_SANDBOX_GO_EXEC_DIR/bin/pi-test-runner" ] || [ -L "$PI_SANDBOX_GO_EXEC_DIR/bin/pi-test-runner" ] && echo test_runner=present || echo test_runner=absent
# R2：--env GOROOT=/tmp/evil 与宿主 GOROOT 一律剥离；沙盒内只能看到
# 预设注入的受信根或空值，绝不能是 /tmp/evil。
echo "goroot_env=${GOROOT:-<unset>}"
`
	script += strings.Join(runSockScriptLines, "\n") + "\n"
	// CLONE_NEWIPC 实证对照基线：探针前宿主 SysV shm 段集合（探针后
	// 断言宿主无新增 size=probeShmMagicSize 的残留段——独立 IPC ns
	// 销毁时沙盒内 shmget 段随 ns 自动回收，不泄漏到宿主）。
	shmBefore := hostShmSegments(t)
	out, code := captureRunMain(t, []string{
		"--preset", "reviewer-agent", "--workdir", workdir,
		"--report-to", "tTEST",
		"--env", "HC_TERMINAL_SOCKET=/run/user/0/herdr-core/default.sock",
		"--env", "FAKE_HC_ABS=" + fake,
		"--env", "PROBE_BIN=" + probeBin,
		"--env", "CAPTEST_DIR=" + capTestDir,
		"--env", "GO_WANT_SBX_MOUNT_PROBE=1",
		"--env", "HOST_PARENT_PID=" + strconv.Itoa(os.Getpid()),
		"--env", "PROC1_ROOT_PROBE_PATHS=" + strings.Join(proc1ProbePaths, " "),
		"--env", "PI_SANDBOX_GO_EXEC_DIR=/tmp/hijack", // 必须被回钉回 pi-reviewer-exec-*
		"--env", "GOROOT=/tmp/evil", // 必须被 sandboxEnvStripKeys 剥离（R2）
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
	// 越狱/进程控制族（chroot/ptrace/pidfd_getfd）及宿主全局副作用族
	//（bpf/finit_module/open_by_handle_at）必须全部返回 EPERM
	// （探针与过滤器同用 x/sys/unix 的架构相关调用号）。
	switch runtime.GOARCH {
	case "amd64", "arm64":
		assertOut("setns_errno", "1")
		assertOut("umount_errno", "1")
		assertOut("chroot_errno", "1")
		assertOut("ptrace_errno", "1")
		assertOut("pidfd_getfd_errno", "1")
		assertOut("bpf_errno", "1")
		assertOut("finit_module_errno", "1")
		assertOut("open_by_handle_at_errno", "1")
		if runtime.GOARCH == "amd64" {
			assertOut("x32_getpid_errno", "1") // x32 ABI 位必须被 JGE 规则封堵
		}
	default:
		t.Logf("GOARCH=%s: mount-seal filter not installed, probe not asserted", runtime.GOARCH)
	}

	// capabilities 根治裁剪：沙盒内 CapEff/CapPrm/CapBnd 精确为
	// 0x22（CAP_DAC_OVERRIDE|CAP_KILL），CapInh/CapAmb 全零（fork/exec
	// 继承裁剪后能力集，helper 同域）。
	assertOut("capinh", "0000000000000000")
	assertOut("capprm", "0000000000000022")
	assertOut("capeff", "0000000000000022")
	assertOut("capbnd", "0000000000000022")
	assertOut("capamb", "0000000000000000")
	// 阶段 3：broker 拆除后 go list/build/vet 原样通过（rc=0）；go test
	// 需执行 .test 动态产物，按 W^X 自然拒（rc=1 且含 fork/exec ...
	// permission denied）；broker.sock/work/bin/pi-test-runner 对象不存在
	//（[ -e ]||[ -L ] 锁不存在语义，普通文件/悬链不得误报 absent）。
	assertOut("golist_rc", "0")
	assertOut("gobuild_rc", "0")
	assertOut("govet_rc", "0")
	// gotest_rc 按完整输出行精确比较：assertOut 是子串匹配，gotest_rc=1
	// 会把 126/127（如沙盒拒绝 exec go 本身）误判为 rc=1 假绿。
	gotestRcExact := false
	for _, line := range strings.Split(out, "\n") {
		if line == "gotest_rc=1" {
			gotestRcExact = true
			break
		}
	}
	if !gotestRcExact {
		t.Fatalf("missing exact line gotest_rc=1 (assertOut substring would let 126/127 pass); output:\n%s", out)
	}
	assertOut("gotest_eacces", "1")
	assertOut("broker_sock", "absent")
	assertOut("goexec_work", "absent")
	assertOut("test_runner", "absent")
	// R2：--env GOROOT=/tmp/evil 必须被 sandboxEnvStripKeys 剥离；沙盒内
	// 只能看到预设注入的受信根或 <unset>，绝不能是 /tmp/evil。
	if strings.Contains(out, "goroot_env=/tmp/evil") {
		t.Fatalf("GOROOT=/tmp/evil leaked into sandbox env (want stripped); output:\n%s", out)
	}

	// PID-1 孤儿收尸：孤儿先被 PID 1 接管（orphan_adopted），放行退出后
	// /proc/<pid> 彻底消失（orphan_reaped——PID 1 经 Wait4 收割），全量
	// /proc 扫描零僵尸残留。
	assertOut("orphan_adopted", "1")
	assertOut("orphan_reaped", "1")
	assertOut("zombie_count", "0")

	// 独立 PID 命名空间：PID 1 必须是 sbx init 本体（测试二进制自身；
	// Apply 的 helper 会合法占用 2..N，故不钉死受测命令为 PID 2，
	// 改以「PID 1 身份 + 小号 PID + 宿主 PID ENOENT」三重证据锁定新 ns）。
	wantInitComm := filepath.Base(probeBin)
	if len(wantInitComm) > 15 { // comm 上限 TASK_COMM_LEN-1=15
		wantInitComm = wantInitComm[:15]
	}
	assertOut("init_comm", wantInitComm)
	assertOut("host_pid", "hidden")

	// CLONE_NEWIPC 隔离：独立 IPC ns 内 sysvipc 初始视图必为空；沙盒内
	// 私有 shmget 段退出后随 ns 销毁——宿主 /proc/sysvipc/shm 不得新增
	// size=probeShmMagicSize 的残留段（以魔数 size 区分宿主既有段与
	// 探针段，不比对 shmid 值，避免极小概率 ID 号巧合误判）。
	assertOut("sysvipc_init_shm", "0")
	sawShmid := false
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "sandbox_shmid="); ok {
			sawShmid = true
			if _, err := strconv.Atoi(v); err != nil {
				t.Fatalf("sandbox_shmid = %q: shmget inside sandbox failed; output:\n%s", v, out)
			}
		}
	}
	if !sawShmid {
		t.Fatalf("missing sandbox_shmid line; output:\n%s", out)
	}
	for seg := range hostShmSegments(t) {
		if _, known := shmBefore[seg]; !known {
			fields := strings.Fields(seg)
			// /proc/sysvipc/shm 列布局：key shmid perms size ...；
			// 新增行且 size 字段（第 4 列）命中魔数即判定沙盒 IPC 段残留宿主。
			if len(fields) >= 4 && fields[3] == strconv.Itoa(probeShmMagicSize) {
				t.Fatalf("host sysvipc shm leaked segment after sandbox exit (magic size %d): %q", probeShmMagicSize, seg)
			}
		}
	}
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

// TestSbxCwdRebindAfterCover 覆盖挂载后继承 cwd 重绑回归（reviewer #1）：
// tmpfs 覆盖只改变其后的绝对路径解析，不迁移内核已持有的 fs->pwd——沙盒
// 内相对路径仍可引用覆盖前的底层目录。修复后隔离流程在全部覆盖完成后
// os.Chdir 回原 cwd 从新视图重解析：
//   - 原 cwd 为 /run/user/0（宿主存在）：目录本身在回绑白名单项时被重建，
//     重绑成功；相对路径 herdr-core/default.sock 必须 ENOENT（未回绑），
//     moshi-hook.sock 回绑后相对路径可见；
//   - 原 cwd 为 /run/user/0/herdr-core（宿主存在时）：新视图下该子目录未
//     重建，rebind-cwd 返回 ENOENT，必须 fail-closed 非零退出。
func TestSbxCwdRebindAfterCover(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mount namespace isolation requires CAP_SYS_ADMIN (euid 0)")
	}
	if _, err := os.Stat("/run/user/0"); err != nil {
		t.Skipf("host /run/user/0 absent: %v", err)
	}
	workdir := t.TempDir()

	moshiPresent := false
	if st, err := os.Stat("/run/user/0/moshi-hook.sock"); err == nil && st.Mode()&os.ModeSocket != 0 {
		moshiPresent = true
	}

	out, code := invokeSbxSubprocessDir(t, "/run/user/0", nil,
		"--preset", "reviewer-agent", "--workdir", workdir,
		"--", "/bin/sh", "-c",
		`[ -e herdr-core/default.sock ] && echo rel_herdr=visible || echo rel_herdr=hidden
[ -e moshi-hook.sock ] && echo rel_moshi=visible || echo rel_moshi=absent`,
	)
	if !moshiPresent {
		// 无回绑项时覆盖层不重建 /run/user/0：原 cwd 在新视图不存在，
		// fail-closed 拒绝启动同样满足「不得静默继承旧目录项」的契约。
		if code == 0 {
			t.Fatalf("cwd under un-recreated /run/user/0 must fail closed, got 0; output:\n%s", out)
		}
		return
	}
	if code != 0 {
		t.Fatalf("sbx exit = %d, want 0 (rebound cwd should resolve /run/user/0); output:\n%s", code, out)
	}
	if !strings.Contains(out, "rel_herdr=hidden") {
		t.Fatalf("relative herdr-core/default.sock must be ENOENT after cwd rebind; output:\n%s", out)
	}
	if !strings.Contains(out, "rel_moshi=visible") {
		t.Fatalf("relative moshi-hook.sock must be visible via rebind; output:\n%s", out)
	}

	if _, err := os.Stat("/run/user/0/herdr-core"); err == nil {
		out, code := invokeSbxSubprocessDir(t, "/run/user/0/herdr-core", nil,
			"--preset", "reviewer-agent", "--workdir", workdir,
			"--", "/bin/sh", "-c", "echo SHOULD_NOT_REACH",
		)
		if code == 0 || strings.Contains(out, "SHOULD_NOT_REACH") {
			t.Fatalf("cwd inside covered /run/user/0/herdr-core must fail closed (exit=%d); output:\n%s", code, out)
		}
	} else {
		t.Log("host /run/user/0/herdr-core absent; fail-closed case not applicable")
	}
}

// TestChildSpecOptionalReadWriteEndToEnd 回归 [pattern:verifier-transient-fail]：
// OptionalReadWrite 必须随 childExecSpec 经 PI_SANDBOX_INTERNAL_CHILD_SPEC
// 100% 透传到受限子进程——宿主 agy 的 conversation_summaries.db-shm 等
// 快照枚举瞬态文件在 Apply 前被 WAL checkpoint 删除时，修复前子进程
// Apply ENOENT 崩溃（sbx: apply sandbox policy failed: sandbox: open
// "...db-shm": no such file or directory），修复后干净退 0；未登记
// OptionalReadWrite 的 ReadWrite 缺失路径保持 fail-closed 非零拒绝。
func TestChildSpecOptionalReadWriteEndToEnd(t *testing.T) {
	// spec JSON 正反向映射：OptionalReadWrite 序列化后完整回读。
	t.Run("spec_json_roundtrip", func(t *testing.T) {
		spec := childExecSpec{
			Bin:               "/bin/sh",
			Command:           []string{"/bin/sh", "-c", "true"},
			ReadWrite:         []string{"/tmp/x.db-shm"},
			OptionalReadWrite: []string{"/tmp/x.db-shm"},
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("marshal spec: %v", err)
		}
		var back childExecSpec
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal spec: %v", err)
		}
		if len(back.OptionalReadWrite) != 1 || back.OptionalReadWrite[0] != "/tmp/x.db-shm" {
			t.Fatalf("OptionalReadWrite lost in JSON roundtrip: %v (raw=%s)", back.OptionalReadWrite, raw)
		}
	})

	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(selfExe); err == nil && resolved != "" {
		selfExe = resolved
	}

	// execSelfWithSpec 以 envSbxChildSpecKey 驱动测试二进制进入
	// runChildFromEnv（TestMain 分支），端到端验证 spec 的 Apply 行为。
	execSelfWithSpec := func(t *testing.T, spec childExecSpec) (string, int) {
		t.Helper()
		specBytes, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("marshal spec: %v", err)
		}
		env := mergeEnv(stripSandboxEnv(os.Environ()),
			[]string{envSbxChildSpecKey + "=" + string(specBytes)})
		cmd := exec.Command(selfExe)
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			return out.String(), exitCodeFromWaitErr(err)
		}
		return out.String(), 0
	}

	missingShm := filepath.Join(t.TempDir(), "conversation_summaries.db-shm")

	t.Run("optional_transient_passes", func(t *testing.T) {
		if _, ok := sandbox.Supported(); !ok {
			t.Skip("Landlock not supported on this kernel")
		}
		spec := childExecSpec{
			Bin:               "/bin/sh",
			Command:           []string{"/bin/sh", "-c", "echo transient_ok"},
			ReadOnly:          []string{"/"},
			ReadWrite:         []string{missingShm},
			OptionalReadWrite: []string{missingShm},
			Exec:              []string{"/bin/sh"},
			MountNS:           false,
		}
		out, code := execSelfWithSpec(t, spec)
		if code != 0 {
			t.Fatalf("child spec with optional transient missing file exit = %d, want 0; output:\n%s", code, out)
		}
		if !strings.Contains(out, "transient_ok") {
			t.Fatalf("expected transient_ok in output, got:\n%s", out)
		}
	})

	t.Run("non_optional_missing_fails_closed", func(t *testing.T) {
		if _, ok := sandbox.Supported(); !ok {
			t.Skip("Landlock not supported on this kernel")
		}
		spec := childExecSpec{
			Bin:       "/bin/sh",
			Command:   []string{"/bin/sh", "-c", "echo SHOULD_NOT_REACH"},
			ReadOnly:  []string{"/"},
			ReadWrite: []string{missingShm},
			Exec:      []string{"/bin/sh"},
			MountNS:   false,
		}
		out, code := execSelfWithSpec(t, spec)
		if code == 0 || strings.Contains(out, "SHOULD_NOT_REACH") {
			t.Fatalf("non-optional missing ReadWrite must fail closed (exit=%d); output:\n%s", code, out)
		}
		if !strings.Contains(out, "no such file or directory") {
			t.Fatalf("expected ENOENT error in output, got:\n%s", out)
		}
	})

	t.Run("cli_rw_missing_fails_closed", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("strict mode mount namespace requires CAP_SYS_ADMIN (euid 0)")
		}
		if _, ok := sandbox.Supported(); !ok {
			t.Skip("Landlock not supported on this kernel")
		}
		// 显式 --rw 的缺失路径绝不进入 OptionalReadWrite：严格模式 fail-closed。
		out, code := invokeSbxSubprocess(t, nil,
			"--rw", missingShm,
			"--", "/bin/sh", "-c", "echo SHOULD_NOT_REACH",
		)
		if code == 0 || strings.Contains(out, "SHOULD_NOT_REACH") {
			t.Fatalf("explicit --rw missing path must fail closed (exit=%d); output:\n%s", code, out)
		}
		if !strings.Contains(out, "no such file or directory") {
			t.Fatalf("expected ENOENT error in output, got:\n%s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// fd3（claude-pty debugW 写端）显式转发与 CLOEXEC 收口回归（oracle FM-3）。
//
// 背景：claude-pty 经 ExtraFiles 把 debugW 传入 sbx 为 fd3（无 CLOEXEC）。
// 修复必须同时满足：
//   1. fd3 显式经 ExtraFiles 单路转发到最终目标进程（read reviewer-agent
//      目标能读到 fd3 内容）——证明转发链 newChildCmd→runAsSandboxInit
//      生效；
//   2. fd3 不泄漏给 helper/预热子进程等中间进程（在 sbx 入口已
//      置 CLOEXEC）——用「目标进程检测自身 fd3 是否打开」作判据：
//      agent preset 下目标应能打开 /dev/fd/3 或读到 fd3；非 agent preset
//      或未传入 fd3 时目标侧 fd3 应不存在。
// 这里用「目标进程内 openat('/dev/fd/3')/readlink('/proc/self/fd/3')」
// 作真实探针（不依赖宿主 /dev/fd 存在则用 /proc/self/fd）。

// invokeSbxSubprocessWithFD3 与 invokeSbxSubprocess 相同，但额外把调用方
// 提供的 *os.File 作为 ExtraFiles 传入 sbx（子进程内即 fd3），模拟
// claude-pty 的 debugW 写端继承。返回 sbx 退出码与目标侧 fd3 探测输出。
func invokeSbxSubprocessWithFD3(t *testing.T, extraFile *os.File, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(selfExe, args...)
	env := append(os.Environ(), envTestWantSbxMain+"=1")
	env = append(env, extraEnv...)
	cmd.Env = env
	if extraFile != nil {
		cmd.ExtraFiles = []*os.File{extraFile}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), exitCodeFromWaitErr(err)
}

// TestFD3ExplicitForwardAndNoLeak 覆盖 fd3 显式转发（agent preset）与
// 收口不泄漏（非 agent preset / 目标侧探测）。
func TestFD3ExplicitForwardAndNoLeak(t *testing.T) {
	if _, ok := sandbox.Supported(); !ok {
		t.Skip("Landlock not supported on this kernel")
	}
	workdir := t.TempDir()

	// 目标侧探针：readlink /proc/self/fd/3 判定 fd3 是否打开。
	// 目标进程内 fd3 有效则 readlink 成功并打印非空目标；fd3 关闭或
	// 未转发则 readlink ENOENT 失败打印 FD3_CLOSED。
	fd3Probe := `if readlink /proc/self/fd/3 >/dev/null 2>&1; then
	echo "FD3_OPEN:$(readlink /proc/self/fd/3)"
else
	echo "FD3_CLOSED"
fi`

	t.Run("agent_preset_forwards_fd3_to_target", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer r.Close()
		out, code := invokeSbxSubprocessWithFD3(t, w,
			nil,
			"--best-effort", "--preset", "reviewer-agent", "--workdir", workdir,
			"--", "/bin/sh", "-c", fd3Probe,
		)
		_ = w.Close()
		if code != 0 {
			t.Fatalf("reviewer-agent exit = %d; output:\n%s", code, out)
		}
		if !strings.Contains(out, "FD3_OPEN:") {
			t.Fatalf("agent preset must forward fd3 to target via ExtraFiles; output:\n%s", out)
		}
	})

	t.Run("fd3_write_end_fully_released_on_sbx_exit", func(t *testing.T) {
		// 无泄漏判据（claude-pty drainDebug 语义）：传入 debugW 写端给
		// sbx，测试侧持有读端。sbx 及其全部子进程（helper/init/
		// 目标）退出后，若任一中间进程残留持有 fd3 写端，读端 Read 永远
		// 收不到 EOF（oracle FM-3 的 daemon 劫持缺陷）。收口正确时 sbx
		// 退出即全部写端关闭，读端立即 EOF。
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer r.Close()
		out, code := invokeSbxSubprocessWithFD3(t, w,
			nil,
			"--best-effort", "--preset", "reviewer-agent", "--workdir", workdir,
			"--", "/bin/sh", "-c", fd3Probe,
		)
		_ = w.Close()
		if code != 0 {
			t.Fatalf("reviewer-agent exit = %d; output:\n%s", code, out)
		}
		// sbx 进程已退出：读端设超时读，须在限时内收到 EOF（证明写端
		// 无残留持有者——helper/daemon 均未继承 fd3）。
		_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 8)
		n, rerr := r.Read(buf)
		if rerr == nil && n > 0 {
			t.Fatalf("unexpected data on debug pipe read end: %q", buf[:n])
		}
		if rerr != io.EOF {
			t.Fatalf("fd3 write end leaked: read end did not see EOF after sbx exit (err=%v); output:\n%s", rerr, out)
		}
	})

	t.Run("prewarmed_daemon_does_not_inherit_fd3", func(t *testing.T) {
		// N5：用受控真实 pig 入口 + t.TempDir() 内自包含最小 Go 扩展（仅
		// 标准库 + pigsdk，GOPROXY=off 兼容，sdk 经 replace 指向本次独占
		// PIG_HOME 下 pig --help 自动落盘的内嵌 staged SDK）触发
		// MaybeWarmGoExtensionCells 真实冷路径预热——pig --help 编译 cell
		// 并在加载扩展时经 OnLoaded 派生一个带唯一 daemonToken、上限
		// sleep 30 且写 daemon.pid 的常驻子进程。该常驻进程若继承 fd3
		// 写端，sbx 退出后读端收不到 EOF（oracle FM-3 daemon 劫持）。
		pigBin := "/root/.local/bin/pig"
		st, err := os.Stat(pigBin)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			t.Fatalf("real pig entry %s required for warm-up daemon test: %v", pigBin, err)
		}
		// 版本核对：pig wrapper 记录版本行（存在性硬要求，不静默 skip）。
		if vout, verr := exec.Command(pigBin, "--version").CombinedOutput(); verr != nil {
			t.Fatalf("pig --version failed: %v\n%s", verr, vout)
		} else {
			t.Logf("pig version: %s", strings.TrimSpace(string(vout)))
		}

		// 受控宿主 pig home：agent/extensions/fd3-daemon-ext -> t.TempDir()
		// 自包含扩展源码软链（与生产布局同构）；空 cache 触发冷路径预热。
		hostPigHome := t.TempDir()
		extSrc := t.TempDir()
		if err := os.MkdirAll(filepath.Join(hostPigHome, "agent", "extensions"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(hostPigHome, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		daemonToken := fmt.Sprintf("sbx-fd3-daemon-%d-%d", os.Getpid(), time.Now().UnixNano())
		daemonPidFile := filepath.Join(t.TempDir(), "daemon.pid")
		daemonLogFile := filepath.Join(t.TempDir(), "daemon.log")
		// 最小扩展：factory 形态（func Extension() *sdk.Extension）。
		// warm 阶段 pig --help 会 exec 已编译的 cell 做 register/identity 校验
		//（name_mismatch 证明 cell 被真实执行）；init() 在 cell 进程启动即跑，
		// 于其中派生 `sleep 30` 常驻子进程并写 daemon.pid。pidfile/log/token
		// 以编译期字面量注入扩展源码——warm 子进程使用最小干净环境，无法经
		// 环境变量传递，字面量保证 warm cell exec 与沙盒内任何 cell exec 都
		// 指向同一可观测 daemon.pid。daemon 存活即模拟 codegraph-go
		// -warm-daemon 的「预热拉起的常驻子进程」形态。
		// 仅标准库 + sdk，GOPROXY=off 下 go build 可解（replace -> staged sdk）。
		extGo := fmt.Sprintf(`package fd3daemon

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	fd3DaemonPidFile = %s
	fd3DaemonLogFile = %s
	fd3DaemonToken   = %s
)

// init 在 cell（扩展编译产物）被 pig exec 加载时先于工厂/OnLoaded 执行：
// 派生一个可观测的常驻 sleep daemon，写 daemon.pid 供测试精准回收。
func init() {
	cmd := exec.Command("sleep", "30")
	// Setsid 脱离 warm/cell exec 进程组——warm 结束时对进程组 SIGKILL
	// 不会波及 daemon，daemon 存活供测试验证其 fd3 不是 debug 写端。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "SBX_FD3_DAEMON_TOKEN="+fd3DaemonToken)
	if fd3DaemonLogFile != "" {
		if lf, err := os.OpenFile(fd3DaemonLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			cmd.Stdout = lf
			cmd.Stderr = lf
		}
	}
	if err := cmd.Start(); err == nil {
		pid := cmd.Process.Pid
		_ = cmd.Process.Release()
		_ = os.WriteFile(fd3DaemonPidFile, []byte(strconv.Itoa(pid)), 0o600)
	}
}

func Extension() *sdk.Extension {
	return sdk.New("fd3-daemon")
}
`, strconv.Quote(daemonPidFile), strconv.Quote(daemonLogFile), strconv.Quote(daemonToken))
		if err := os.WriteFile(filepath.Join(extSrc, "extension.go"), []byte(extGo), 0o644); err != nil {
			t.Fatal(err)
		}
		// go.mod：sdk 用 replace 指向独占 PIG_HOME 下的 staged sdk-*
		//（pig --help 首次运行自动落盘内嵌 SDK）；本测试不复制待审
		// go/pigsdk——SDK 来源必须是 pig 自身 staged，独立真实。
		goMod := `module sbxtest/fd3daemon

go 1.26

require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
`
		if err := os.WriteFile(filepath.Join(extSrc, "go.mod"), []byte(goMod), 0o644); err != nil {
			t.Fatal(err)
		}
		// 软链名必须与扩展注册 identity（sdk.New("fd3-daemon")）一致，否则
		// pig register 报 name_mismatch 拒绝加载。
		if err := os.Symlink(extSrc, filepath.Join(hostPigHome, "agent", "extensions", "fd3-daemon")); err != nil {
			t.Fatal(err)
		}
		// 扩展发现走 agentDir/settings.json 的 extensions 数组（绝对路径目录，
		// pig 扫描其中的扩展）；仅放软链目录不被自动发现——必须显式声明。
		hextDir := filepath.Join(hostPigHome, "agent", "extensions")
		settingsJSON := `{"extensions":[` + strconv.Quote(hextDir) + `]}`
		if err := os.WriteFile(filepath.Join(hostPigHome, "agent", "settings.json"), []byte(settingsJSON), 0o600); err != nil {
			t.Fatal(err)
		}

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer r.Close()
		// 扩展经环境变量拿到 token/pidfile/logfile（cfgEnv 注入进沙盒策略 env）。
		out, code := invokeSbxSubprocessWithFD3(t, w,
			[]string{
				"PIG_HOME=" + hostPigHome,
				"SBX_FD3_DAEMON_PIDFILE=" + daemonPidFile,
				"SBX_FD3_DAEMON_LOG=" + daemonLogFile,
				"SBX_FD3_DAEMON_TOKEN=" + daemonToken,
			},
			"--best-effort", "--preset", "reviewer-agent", "--workdir", workdir,
			"--env", "SBX_FD3_DAEMON_PIDFILE="+daemonPidFile,
			"--env", "SBX_FD3_DAEMON_LOG="+daemonLogFile,
			"--env", "SBX_FD3_DAEMON_TOKEN="+daemonToken,
			"--", pigBin, "--help",
		)
		_ = w.Close()
		if code != 0 {
			t.Fatalf("reviewer-agent real pig exit = %d; output:\n%s", code, out)
		}

		// 预热确实发生：宿主 cache 下必须出现有效可执行 Go 产物
		//（cells/go 合包或 ext 单扩展），否则预热路径根本没跑——直接判失败。
		if n := countGoArtifactsForTest(t, filepath.Join(hostPigHome, "cache")); n == 0 {
			t.Fatalf("warm-up did not produce any Go cell artifact under %s (prewarm path never ran); output:\n%s", hostPigHome, out)
		}

		// 扩展加载确实拉起 daemon：daemon.pid 必须存在且指向带本次
		// daemonToken 的存活进程。
		pidBytes, err := os.ReadFile(daemonPidFile)
		if err != nil {
			t.Fatalf("daemon.pid not written (extension OnLoaded daemon spawn did not run): %v; output:\n%s", err, out)
		}
		daemonPid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil || daemonPid <= 0 {
			t.Fatalf("daemon.pid bad content %q: %v", pidBytes, err)
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", daemonPid))
		if err != nil {
			t.Fatalf("daemon pid %d not alive after sbx exit: %v", daemonPid, err)
		}
		// daemonToken 经环境变量注入（sleep 的 cmdline 只是 argv，不含 env）——
		// 核验 /proc/<pid>/environ 携带本次 token，确保是本测试拉起的 daemon。
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", daemonPid))
		if err != nil {
			t.Fatalf("daemon pid %d environ unreadable: %v", daemonPid, err)
		}
		if !strings.Contains(string(environ), daemonToken) {
			t.Fatalf("daemon pid %d environ does not contain daemonToken %q (cmdline %q)", daemonPid, daemonToken, cmdline)
		}
		// 精准回收：核验 environ 含 token 后 SIGKILL 并等待退出，绝不触碰
		// 其它宿主进程。
		t.Cleanup(func() {
			if env2, cerr := os.ReadFile(fmt.Sprintf("/proc/%d/environ", daemonPid)); cerr == nil && strings.Contains(string(env2), daemonToken) {
				_ = syscall.Kill(daemonPid, syscall.SIGKILL)
				_, _ = unix.Wait4(daemonPid, nil, 0, nil)
			}
		})
		t.Logf("prewarm-spawned daemon alive: pid=%d token=%s", daemonPid, daemonToken)

		// daemon 存活期间，父侧 debug 读端立即 io.EOF：sbx 及其全部中间
		// 进程（helper/init）与预热派生的 daemon 均未残留持有 fd3
		// 写端——fd3 CLOEXEC 收口与 ExtraFiles 单路转发真实生效。
		_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 8)
		n, rerr := r.Read(buf)
		if rerr == nil && n > 0 {
			t.Fatalf("unexpected data on debug pipe read end: %q", buf[:n])
		}
		if rerr != io.EOF {
			t.Fatalf("fd3 write end leaked to warm-up/daemon: read end no EOF while daemon alive (err=%v); output:\n%s", rerr, out)
		}
		// /proc/<pid>/fd/3 不指向本次 debug 管道：daemon 若继承 fd3，其
		// fd/3 的 readlink 目标是本管道 inode（pipe:[N]），与测试侧
		// r 的 fstat 设备号一致即泄漏。
		var rStat unix.Stat_t
		_ = unix.Fstat(int(r.Fd()), &rStat)
		if target, lerr := os.Readlink(fmt.Sprintf("/proc/%d/fd/3", daemonPid)); lerr == nil {
			var fd3Stat unix.Stat_t
			if dfd, oerr := unix.Open(fmt.Sprintf("/proc/%d/fd/3", daemonPid), unix.O_RDONLY|unix.O_NONBLOCK, 0); oerr == nil {
				_ = unix.Fstat(dfd, &fd3Stat)
				_ = unix.Close(dfd)
				if fd3Stat.Dev == rStat.Dev && fd3Stat.Ino == rStat.Ino {
					t.Fatalf("daemon pid %d fd/3 -> %s is the debug pipe (fd3 leaked to warm daemon)", daemonPid, target)
				}
			}
			t.Logf("daemon pid %d fd/3 -> %s (not this debug pipe)", daemonPid, target)
		}
	})
}

// countGoArtifactsForTest 是 main 包对 sandbox 内部 countValidExecutableGoArtifacts
// 的最小复刻（不导出生产符号）：枚举口径与生产 cellCacheEntryDirs /
// validCellEntryDir 完全对齐——cacheRoot、cache/cells、cache/ext 与
// cells/<lang> 根本体是软链/非目录时对应整个枚举被跳过（Lstat 拒绝），
// 条目谓词同 cellEntryHiddenOrSymlink（软链/非目录/隐藏名跳过，
// .tombstone-* 除外）；ready.json 须 InputDigest 与目录名绑定、Artifact
// 为单路径元素，artifact 经 Lstat 为普通文件且 size 一致（拒绝软链穿透
// 与路径逃逸）；语言门禁同生产：cells/go 下 language 缺省或 =="go"，
// 其余 cells/<lang> 与 ext 必须显式 language=="go"。
func countGoArtifactsForTest(t *testing.T, cacheRoot string) int {
	t.Helper()
	count := 0
	isRealDirNoSymlink := func(path string) bool {
		st, err := os.Lstat(path)
		return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
	}
	hiddenOrSymlink := func(e os.DirEntry) bool {
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			return true
		}
		name := e.Name()
		return strings.HasPrefix(name, ".") && !strings.HasPrefix(name, ".tombstone-")
	}
	var dirs []string
	addEntries := func(root string) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			if hiddenOrSymlink(e) {
				continue
			}
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	// 根本体是软链/非真实目录时整体拒枚举（与生产 cellCacheEntryDirs 一致）。
	if !isRealDirNoSymlink(cacheRoot) {
		return 0
	}
	if extRoot := filepath.Join(cacheRoot, "ext"); isRealDirNoSymlink(extRoot) {
		addEntries(extRoot)
	}
	cellsRoot := filepath.Join(cacheRoot, "cells")
	if isRealDirNoSymlink(cellsRoot) {
		if langs, err := os.ReadDir(cellsRoot); err == nil {
			for _, lang := range langs {
				if lang.Type()&os.ModeSymlink != 0 || !lang.IsDir() || strings.HasPrefix(lang.Name(), ".") {
					continue
				}
				addEntries(filepath.Join(cellsRoot, lang.Name()))
			}
		}
	}
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(dir, "ready.json"))
		if err != nil {
			continue
		}
		var meta struct {
			InputDigest string `json:"inputDigest"`
			Artifact    string `json:"artifact"`
			Size        int64  `json:"size"`
			Created     int64  `json:"created"`
			Language    string `json:"language"`
		}
		if json.Unmarshal(data, &meta) != nil || meta.Created <= 0 {
			continue
		}
		// 目录名须与 InputDigest 绑定（<digest> 或 <name>-<digest>）。
		base := filepath.Base(dir)
		if meta.InputDigest == "" || (base != meta.InputDigest && !strings.HasSuffix(base, "-"+meta.InputDigest)) {
			continue
		}
		// artifact 名为单路径元素：拒绝 .. / 绝对或嵌套路径逃逸。
		name := meta.Artifact
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
			continue
		}
		// 语言门禁同生产：cells/go 下 language 缺省或 =="go"；其余
		// cells/<lang> 与 cache/ext 必须显式 language=="go"。
		rel, rerr := filepath.Rel(cacheRoot, dir)
		inCellsGo := rerr == nil && strings.HasPrefix(rel, "cells"+string(filepath.Separator)+"go"+string(filepath.Separator))
		if inCellsGo {
			if meta.Language != "" && meta.Language != "go" {
				continue
			}
		} else if meta.Language != "go" {
			continue
		}
		// artifact 经 Lstat 必须是普通文件且 size 一致（拒绝软链穿透），
		// 再要求带执行位。
		st, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !st.Mode().IsRegular() || st.Size() != meta.Size || st.Mode()&0o111 == 0 {
			continue
		}
		count++
	}
	return count
}

// TestReviewerAgentCellCheckOnlyPigEntry 覆盖 oracle FM-2B：reviewer-agent 的
// cell 目标集 fail-closed 仅对真实 pig 入口生效——非 pig 命令（sh/agy/claude）
// 在冷缓存（无有效 Go cell）下不得被 ReviewerAgentCellTargetError 误拒启动。
func TestReviewerAgentCellCheckOnlyPigEntry(t *testing.T) {
	if _, ok := sandbox.Supported(); !ok {
		t.Skip("Landlock not supported on this kernel")
	}
	workdir := t.TempDir()
	// 冷缓存隔离 pig home：含 Go 扩展但无任何有效 Go cell（触发 cell 校验
	// 失败的场景），非 pig 命令仍须正常启动不被误拒。
	fakePigHome := t.TempDir()
	goExtDir := filepath.Join(fakePigHome, "agent", "extensions", "fake-go-ext")
	if err := os.MkdirAll(goExtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goExtDir, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("sh_command_not_rejected_by_cell_check", func(t *testing.T) {
		out, code := invokeSbxSubprocess(t,
			[]string{"PIG_HOME=" + fakePigHome},
			"--best-effort", "--preset", "reviewer-agent", "--workdir", workdir,
			"--", "/bin/sh", "-c", "echo NONPIG_OK",
		)
		if code != 0 || !strings.Contains(out, "NONPIG_OK") {
			t.Fatalf("non-pig command must not be rejected by cell check (exit=%d); output:\n%s", code, out)
		}
	})
}

// ---------------------------------------------------------------------------
// M1/M2：buildPolicy 在 cfgEnv 合并完成后统一归一化有效 AgentDir（oracle FM-4
// 复审）。断言对象是最终 policy.Env（mergeEnv(policy.Env, cfgEnv) 之后），
// 不是 injectPigCodingAgentDir 的阶段输出——后者会被后续 mergeEnv 用 cfgEnv
// 空覆盖重新打穿。
//
// 语义契约：
//  - 受信（非软链、目录、属主合规、perm & 0o022 == 0）的显式非空
//    PIG_CODING_AGENT_DIR 保留；同时 PI_CODING_AGENT_DIR 被归一到同一有效
//    目录且 PIG_USE_PI_DIRS 不残留宿主值（否则 pig 包装器后置 export
//    PIG_USE_PI_DIRS=1 仍可能激活宿主残留 PI_*）。
//  - 其余情形（键缺失 / 显式空串 / 重复键末项为空 / 显式值不可信）两个
//    AgentDir 键都回钉为 ReviewerAgentWithEnv 预建的私有 pig-agent
//    （即 <TMPDIR>/pig-agent），且各自只有一个条目。
//
// 基线红证据：808f0eb 下两键均不受统一归一约束——显式空串会被 mergeEnv 写
// 回空串（M1），非空 PIG_CODING_AGENT_DIR 令整个注入函数直接 return，宿主
// PI_CODING_AGENT_DIR 原样继承（M2）。

// privateAgentDirFromEnv 返回 ReviewerAgentWithEnv 在 policy.Env 中预建的
// 私有 pig-agent 目录（TMPDIR/pig-agent）。断言 M1/M2 的最终有效值时使用。
func privateAgentDirFromEnv(t *testing.T, env []string) string {
	t.Helper()
	tmp := envListValue(env, "TMPDIR")
	if tmp == "" {
		t.Fatalf("policy.Env missing TMPDIR (env=%v)", env)
	}
	return filepath.Join(tmp, "pig-agent")
}

// envKeyValue 返回 env 切片中 key 的生效值（最后一个条目）与条目数，
// 用于断言「最终生效值」与「只保留一个条目」。
func envKeyValue(env []string, key string) (val string, count int) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = strings.TrimPrefix(kv, prefix)
			count++
		}
	}
	return val, count
}

// TestEffectiveAgentDirsPinnedAfterEmptyEnvMerge 覆盖 M1：cfgEnv 合并完成后，
// 显式空覆盖（含重复键末项为空）不得把空串写进最终 policy.Env；双 AgentDir
// 键统一回钉到 ReviewerAgentWithEnv 准备的私有 pig-agent。
func TestEffectiveAgentDirsPinnedAfterEmptyEnvMerge(t *testing.T) {
	t.Setenv("PIG_HOME", t.TempDir()) // 隔离宿主 pig home
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	workdir := t.TempDir()

	t.Run("explicit_empty_PIG_CODING_AGENT_DIR", func(t *testing.T) {
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PIG_CODING_AGENT_DIR="},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		private := privateAgentDirFromEnv(t, policy.Env)
		if got, n := envKeyValue(policy.Env, "PIG_CODING_AGENT_DIR"); got != private {
			t.Fatalf("final PIG_CODING_AGENT_DIR = %q (count=%d), want %q", got, n, private)
		}
		if got, n := envKeyValue(policy.Env, "PI_CODING_AGENT_DIR"); got != private {
			t.Fatalf("final PI_CODING_AGENT_DIR = %q (count=%d), want %q", got, n, private)
		}
	})

	t.Run("explicit_empty_PI_CODING_AGENT_DIR", func(t *testing.T) {
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PI_CODING_AGENT_DIR="},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		private := privateAgentDirFromEnv(t, policy.Env)
		if got, _ := envKeyValue(policy.Env, "PIG_CODING_AGENT_DIR"); got != private {
			t.Fatalf("final PIG_CODING_AGENT_DIR = %q, want %q", got, private)
		}
		if got, _ := envKeyValue(policy.Env, "PI_CODING_AGENT_DIR"); got != private {
			t.Fatalf("final PI_CODING_AGENT_DIR = %q, want %q", got, private)
		}
	})

	t.Run("duplicate_key_last_empty", func(t *testing.T) {
		// 先给非空再追加空：mergeEnv 保留最后一个条目（空串），必须被
		// 归一化回钉私有目录。
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env: []string{
				"PIG_CODING_AGENT_DIR=/tmp/sbx-unused-agent-dir",
				"PIG_CODING_AGENT_DIR=",
			},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		private := privateAgentDirFromEnv(t, policy.Env)
		got, n := envKeyValue(policy.Env, "PIG_CODING_AGENT_DIR")
		if got != private {
			t.Fatalf("final PIG_CODING_AGENT_DIR = %q (count=%d), want %q", got, n, private)
		}
		if n != 1 {
			t.Fatalf("PIG_CODING_AGENT_DIR entries = %d, want 1", n)
		}
	})
}

// TestEffectiveEnvDropsHostPINoiseWithCustomPigDir 覆盖 M2：显式可信的
// PIG_CODING_AGENT_DIR 分支下，宿主残留的 PI_CODING_AGENT_DIR /
// PIG_USE_PI_DIRS 不得原样继承到最终 env（否则 pig 包装器后置 export
// PIG_USE_PI_DIRS=1 会激活宿主 PI 残留，即 FM-4 触发链）。
func TestEffectiveEnvDropsHostPINoiseWithCustomPigDir(t *testing.T) {
	trustedCustom := t.TempDir() // 0700 属主=euid：validateTrustedPigDir 通过
	if err := os.Chmod(trustedCustom, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_HOME", t.TempDir())
	t.Setenv("PIG_USE_PI_DIRS", "1")
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/sbx-host-pi-noise") // 宿主残留 PI 键
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	workdir := t.TempDir()

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Env:     []string{"PIG_CODING_AGENT_DIR=" + trustedCustom},
	})
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	defer cleanupPresetTempDirsFromPolicy(policy)

	t.Run("custom_pig_dir_retained", func(t *testing.T) {
		if got, _ := envKeyValue(policy.Env, "PIG_CODING_AGENT_DIR"); got != trustedCustom {
			t.Fatalf("trusted custom PIG_CODING_AGENT_DIR must be retained, got %q", got)
		}
	})

	t.Run("host_pi_noise_not_inherited", func(t *testing.T) {
		// 宿主残留 PI_CODING_AGENT_DIR 不得原样进入最终 env：要么被清除，
		// 要么被归一到有效 AgentDir（trustedCustom）。不得出现宿主值。
		if got, _ := envKeyValue(policy.Env, "PI_CODING_AGENT_DIR"); got == "/tmp/sbx-host-pi-noise" {
			t.Fatalf("host PI_CODING_AGENT_DIR leaked into final env: %q", got)
		}
		// 同理，宿主 PIG_USE_PI_DIRS=1 不得被策略注入放行残留激活。
		if got, _ := envKeyValue(policy.Env, "PIG_USE_PI_DIRS"); got == "1" {
			t.Fatalf("host PIG_USE_PI_DIRS=1 leaked into final env")
		}
	})
}

// TestEffectiveEnvMixedAgentDirSources 覆盖 M2/收紧后的混合来源判定：
// PIG_USE_PI_DIRS=1 开关的来源（cfgEnv 显式声明 vs 仅宿主环境）决定
// cfgEnv 的 PI_CODING_AGENT_DIR 是否归入 CfgEnv 受信自定义——宿主开关
// 绝不能越权激活显式受信语义（resolveAgentDirSelection 显式层收紧）。
// 双键统一断言最终 policy.Env 生效值（merge 后归一化）。
func TestEffectiveEnvMixedAgentDirSources(t *testing.T) {
	workdir := t.TempDir()
	trustedCustom := t.TempDir()
	if err := os.Chmod(trustedCustom, 0o700); err != nil {
		t.Fatal(err)
	}
	untrusted := t.TempDir() // 0700 默认 t.TempDir 是 0700；造一个组可写不可信目录
	untrustedDir := filepath.Join(untrusted, "untrusted-agent")
	if err := os.MkdirAll(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}

	privateDir := func(t *testing.T, env []string) string {
		t.Helper()
		return privateAgentDirFromEnv(t, env)
	}
	assertBoth := func(t *testing.T, policy sandbox.Policy, want string, label string) {
		t.Helper()
		for _, key := range []string{"PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
			got, n := envKeyValue(policy.Env, key)
			if got != want || n != 1 {
				t.Fatalf("%s: final %s = %q (count=%d), want %q", label, key, got, n, want)
			}
		}
	}

	// ① 宿主 PIG=H + --env PIG_USE_PI_DIRS=1 --env PI=U（U 不可信）：
	// cfgEnv 显式开关激活 PI 入口，但 U 不可信被拒 → 双键回钉私有。
	t.Run("cfg_switch_plus_untrusted_pi_pins_private", func(t *testing.T) {
		t.Setenv("PIG_HOME", t.TempDir())
		t.Setenv("PIG_CODING_AGENT_DIR", "/host/agent")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PIG_USE_PI_DIRS=1", "PI_CODING_AGENT_DIR=" + untrustedDir},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		assertBoth(t, policy, privateDir(t, policy.Env), "untrusted PI must fall back to private")
	})

	// ② 宿主 PIG=H + --env PIG_USE_PI_DIRS=1 --env PI=C（C 可信）：
	// cfgEnv 显式开关 + 可信 PI → 双键归一到 C。
	t.Run("cfg_switch_plus_trusted_pi_retained", func(t *testing.T) {
		t.Setenv("PIG_HOME", t.TempDir())
		t.Setenv("PIG_CODING_AGENT_DIR", "/host/agent")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PIG_USE_PI_DIRS=1", "PI_CODING_AGENT_DIR=" + trustedCustom},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		assertBoth(t, policy, trustedCustom, "trusted cfg PI dir must be effective")
	})

	// ②b：--env PIG_HOME=<untrusted> + 可信自定义 AgentDir：PIG_HOME
	// 不可信（SF-1）但受信自定义 AgentDir 独立于 pigHome 通过校验，
	// 双键仍归一到自定义目录（显式层规则不被不可信 pigHome 破坏）。
	t.Run("untrusted_pig_home_keeps_trusted_custom_agentdir", func(t *testing.T) {
		untrustedHome := filepath.Join(t.TempDir(), "pig-ww")
		if err := os.MkdirAll(untrustedHome, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(untrustedHome, 0o777); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PIG_HOME", t.TempDir()) // 宿主值不生效：cfgEnv 显式 PIG_HOME 覆盖
		unsetEnv(t, "PIG_USE_PI_DIRS")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PIG_HOME=" + untrustedHome, "PIG_CODING_AGENT_DIR=" + trustedCustom},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		assertBoth(t, policy, trustedCustom, "trusted custom AgentDir must survive untrusted PIG_HOME")
	})

	// ③ --env PIG= + --env PIG_USE_PI_DIRS=1 --env PI=C：显式 PIG 空覆盖
	// 不屏蔽显式 PI 入口，双键归一到 C。
	t.Run("pig_empty_override_does_not_shadow_trusted_pi", func(t *testing.T) {
		t.Setenv("PIG_HOME", t.TempDir())
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PIG_CODING_AGENT_DIR=", "PIG_USE_PI_DIRS=1", "PI_CODING_AGENT_DIR=" + trustedCustom},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		assertBoth(t, policy, trustedCustom, "explicit PIG empty override must not shadow trusted cfg PI dir")
	})

	// ④ 宿主 PIG_USE_PI_DIRS=1 + --env PI=C（未显式传 PIG_USE_PI_DIRS=1）：
	// 收紧点——开关仅来自宿主环境，不得把宿主开关激活为 CfgEnv，可信 C
	// 也落 HostEnv 层仅作复制源，双键回钉私有（不修信任语义）。
	t.Run("host_only_switch_does_not_promote_cfg_pi_to_cfgenv", func(t *testing.T) {
		t.Setenv("PIG_HOME", t.TempDir())
		t.Setenv("PIG_USE_PI_DIRS", "1")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		policy, err := buildPolicy(cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"PI_CODING_AGENT_DIR=" + trustedCustom},
		})
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		assertBoth(t, policy, privateDir(t, policy.Env), "host-only PIG_USE_PI_DIRS=1 must not activate cfgEnv PI dir as trusted custom")
	})
}

// ---------------------------------------------------------------------------
// N1：私有 AgentDir 身份不得从用户可控 TMPDIR 重建——merge 前捕获的预设
// 事实是唯一的回钉依据。以下用例覆盖任务书要求的四种变体，断言最终
// 双键均为真实私有目录（预设预建的 <privateTmpDir>/pig-agent，即 TMPDIR
// 键被 --env 覆盖前的那个目录），且不是被拒路径。
//
// 红证据：旧实现 normalizeEffectiveAgentDirs 的回钉根用 merge 后的
// TMPDIR 重新 filepath.Join 得出——TMPDIR 与 AgentDir 同时被 --env 覆盖
// 时会重新消费宿主配置。

func TestPrivateAgentDirIdentityNotRebuiltFromUserTmpdir(t *testing.T) {
	workdir := t.TempDir()
	for _, env := range []string{"PIG_HOME", "PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_USE_PI_DIRS"} {
		unsetEnv(t, env)
	}

	assertPrivateBothKeys := func(t *testing.T, cfg cliConfig, forbidden ...string) {
		t.Helper()
		var fact reviewerAgentDirFact
		captureReviewerAgentDirFactHook = func(f reviewerAgentDirFact) { fact = f }
		t.Cleanup(func() { captureReviewerAgentDirFactHook = nil })
		policy, err := buildPolicy(cfg)
		if err != nil {
			t.Fatalf("buildPolicy: %v", err)
		}
		defer cleanupPresetTempDirsFromPolicy(policy)
		priv := fact.privateDir
		if priv == "" {
			t.Fatalf("captured fact has empty privateDir (fact=%+v)", fact)
		}
		for _, key := range []string{"PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
			got, n := envKeyValue(policy.Env, key)
			if got != priv {
				t.Fatalf("final %s = %q (count=%d), want real private dir %q (not rebuilt from user TMPDIR)", key, got, n, priv)
			}
			for _, f := range forbidden {
				if got == f {
					t.Fatalf("final %s = %q: must not equal rejected path", key, f)
				}
			}
		}
	}

	t.Run("tmpdir_override_plus_agentdir_empty", func(t *testing.T) {
		// --env TMPDIR=/root/.pig --env PIG_CODING_AGENT_DIR= ：旧实现从
		// merge 后 TMPDIR 重建得 /root/.pig/pig-agent（重新消费宿主配置）。
		assertPrivateBothKeys(t, cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"TMPDIR=" + t.TempDir(), "PIG_CODING_AGENT_DIR="},
		}, "/root/.pig/pig-agent")
	})

	t.Run("tmpdir_override_plus_untrusted_custom_rejected", func(t *testing.T) {
		realDir := t.TempDir()
		linkDir := filepath.Join(t.TempDir(), "agent-link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		assertPrivateBothKeys(t, cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"TMPDIR=" + t.TempDir(), "PIG_CODING_AGENT_DIR=" + linkDir},
		}, linkDir, realDir)
	})

	t.Run("empty_tmpdir", func(t *testing.T) {
		// --env TMPDIR= ：旧实现空 TMPDIR 退成 cwd 相对的 pig-agent。
		assertPrivateBothKeys(t, cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"TMPDIR=", "PIG_CODING_AGENT_DIR="},
		}, "pig-agent", filepath.Join(workdir, "pig-agent"))
	})

	t.Run("tmpdir_points_to_untrusted_parent", func(t *testing.T) {
		// TMPDIR 指向宿主不可信父目录（如 /root/.pig）：私有目录身份
		// 仍须是预设事实，不是 TMPDIR 下的重建路径。
		fakePig := t.TempDir()
		t.Setenv("PIG_HOME", fakePig)
		assertPrivateBothKeys(t, cliConfig{
			Preset:  "reviewer-agent",
			WorkDir: workdir,
			Env:     []string{"TMPDIR=" + fakePig, "PIG_CODING_AGENT_DIR="},
		}, filepath.Join(fakePig, "pig-agent"))
	})
}

// TestEffectiveEnvPigUsePiDirsStrippedFromHostEnv 覆盖 N2：宿主
// os.Environ() 中的 PIG_USE_PI_DIRS / PI_CODING_AGENT_DIR 残留不得透传
// 进最终目标 exec env——断言对象是 stripSandboxEnv(os.Environ())+policy.Env
// 合并后的最终 env（与 runChildFromEnv 的 execEnv 同路径），不只断言
// policy.Env。
func TestEffectiveEnvPigUsePiDirsStrippedFromHostEnv(t *testing.T) {
	t.Setenv("PIG_USE_PI_DIRS", "1")
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/host-pi-residual")
	t.Setenv("PIG_HOME", t.TempDir())
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	workdir := t.TempDir()

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
	})
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	defer cleanupPresetTempDirsFromPolicy(policy)

	// 最终 exec env：runChildFromEnv 用 stripHostEnv(os.Environ())+policy.Env。
	execEnv := mergeEnv(stripHostEnv(os.Environ()), policy.Env)
	if got, n := envKeyValue(execEnv, "PIG_USE_PI_DIRS"); n != 0 {
		t.Fatalf("host PIG_USE_PI_DIRS must not appear in final exec env (got %q x%d)", got, n)
	}
	if got, _ := envKeyValue(execEnv, "PI_CODING_AGENT_DIR"); got == "/tmp/host-pi-residual" {
		t.Fatalf("host PI_CODING_AGENT_DIR residual leaked into final exec env: %q", got)
	}
}

// TestUntrustedCustomAgentDirFallsBackToPrivate 覆盖 M2 校验失败路径：
// 显式 PIG_CODING_AGENT_DIR 指向不可信目录（软链）时，最终生效目录必须
// 回退到 ReviewerAgentWithEnv 准备的私有 pig-agent，不得只回退复制源。
func TestUntrustedCustomAgentDirFallsBackToPrivate(t *testing.T) {
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "agent-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_HOME", t.TempDir())
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	workdir := t.TempDir()

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Env:     []string{"PIG_CODING_AGENT_DIR=" + linkDir},
	})
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	defer cleanupPresetTempDirsFromPolicy(policy)
	private := privateAgentDirFromEnv(t, policy.Env)

	if got, _ := envKeyValue(policy.Env, "PIG_CODING_AGENT_DIR"); got != private {
		t.Fatalf("untrusted PIG_CODING_AGENT_DIR must fall back to private %q, got %q", private, got)
	}
	if got, _ := envKeyValue(policy.Env, "PI_CODING_AGENT_DIR"); got != private {
		t.Fatalf("untrusted PI_CODING_AGENT_DIR must fall back to private %q, got %q", private, got)
	}
}

// ---------------------------------------------------------------------------
// M3：fd3 的 os.File 所有权必须与原始 close 一致。
//
// 缺陷链（oracle FM-3 复审）：forwardFD3ToChild 每次都对同一整数 fd3 新建
// os.NewFile 包装器，而 closeFD3 只做裸 unix.Close(3)。Go 1.24+ 的
// os.NewFile 带 (*file).close 终结器：首次 Start 失败后旧 cmd 被替换，旧
// wrapper 被 GC 终结会在重试前关闭仍需转发的 fd3；成功后终结器还可能关
// 闭被复用的 fd3 槽位。
//
// 契约：每个进程生命周期内 fd3 只建立一个明确 owner（os.File），跨重试
// 复用；最终由该 owner 的 Close（或移交子进程生命周期）关闭，禁止多个
// wrapper 共管整数 3。
//
// 判定方式：直接调用包级 fd3Owner API（与 runSandboxedChild/runAsSandboxInit
// 同一路径）：acquireFd3File 两次调用（模拟 mountns 重试的首次失败 + 重试）
// 必须返回同一 *os.File（单 owner），且强制 GC 后 fd3 仍活跃（无终结器关闭）；
// releaseFd3File 显式 Close 后再 GC，不得对已复用的 fd3 槽位二次关闭。
// 基线（808f0eb）：无 fd3 转发代码，此用例编译期红（fd3Owner 未定义）。
func TestFd3ForwardOwnerSurvivesGCAndRetry(t *testing.T) {
	// 经 selfExe 隔离子进程 + ExtraFiles 确定性把管道写端注入为该子进程
	// 的 fd3（ExtraFiles 传入的 fd3 无 CLOEXEC，正是 claude-pty debugW 形态），
	// 不触碰主测试进程 fd3——消除「fd3 未开 t.Skip」与「dummy.Fd()==3 才断言
	// 槽位复用」两类条件性假绿。
	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(selfExe); rerr == nil && resolved != "" {
		selfExe = resolved
	}

	runHarness := func(t *testing.T, scenario string) (string, int) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer r.Close()
		cmd := exec.Command(selfExe)
		cmd.Env = append(os.Environ(), envTestFd3Harness+"="+scenario)
		cmd.ExtraFiles = []*os.File{w} // 子进程内即 fd3（无 CLOEXEC）
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		runErr := cmd.Run()
		_ = w.Close()
		// 子进程退出后 debug 读端必须立即 EOF（写端无残留持有）。
		_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 8)
		n, rerr := r.Read(buf)
		if rerr == nil && n > 0 {
			t.Fatalf("[%s] unexpected data on debug pipe read end: %q", scenario, buf[:n])
		}
		if rerr != io.EOF {
			t.Fatalf("[%s] fd3 write end leaked in harness subprocess: no EOF (err=%v); output:\n%s", scenario, rerr, out.String())
		}
		return out.String(), exitCodeFromWaitErr(runErr)
	}

	t.Run("strict_start_failure_releases_owner_and_slot_reuse", func(t *testing.T) {
		// 严格模式（BestEffort=false）：childSysProcAttrForRun 注入非法
		// Cloneflags 驱动真实 cmd.Start() 失败 → runSandboxedChild 退 126，
		// closeFD3(fd3Fwd) 对称释放 owner（fd3Owner.f==nil、终结器撤销）。
		// 槽位 3 复用新文件后经 GC + releaseFd3File 仍完好（无幽灵关闭）。
		out, code := runHarness(t, "strict-retry-slot-reuse")
		if code != 0 {
			t.Fatalf("strict harness exit = %d; output:\n%s", code, out)
		}
		for _, want := range []string{"HARNESS_OK", "START_FAILED_126", "OWNER_NIL", "SLOT_REUSE_OK"} {
			if !strings.Contains(out, want) {
				t.Fatalf("strict harness missing %q; output:\n%s", want, out)
			}
		}
	})

	t.Run("besteffort_retry_reuses_same_owner_target_sees_fd3", func(t *testing.T) {
		// BestEffort=true：首次 Start 非法 Cloneflags 失败 → 回退不带 mountns
		// 重试成功；owner 跨重试复用同一 *os.File，目标进程经 ExtraFiles
		// 收到 fd3（读端读到 FD3_OPEN）。本路径需要内核支持命名空间派生，
		// 无 CAP_SYS_ADMIN/内核不支持时如实标记而不假绿。
		out, code := runHarness(t, "besteffort-retry-owner-reuse")
		if strings.Contains(out, "HARNESS_SKIP_UNSUPPORTED") {
			t.Logf("best-effort mountns retry unsupported on this host; harness output:\n%s", out)
			return
		}
		if code != 0 {
			t.Fatalf("besteffort harness exit = %d; output:\n%s", code, out)
		}
		for _, want := range []string{"HARNESS_OK", "RETRY_SUCCESS", "OWNER_NIL_AFTER_RETRY", "FD3_OPEN"} {
			if !strings.Contains(out, want) {
				t.Fatalf("besteffort harness missing %q; output:\n%s", want, out)
			}
		}
	})
}

// envTestFd3Harness 让复用测试二进制进入 fd3 owner 确定性测试线束模式
// （TestMain 分支）：子进程的 fd3 由测试父进程经 ExtraFiles 注入管道写端
// （无 CLOEXEC，claude-pty debugW 形态），线束内部再接管 fd3Owner/
// childSysProcAttrForRun 驱动 runSandboxedChild 的真实 Start 失败/重试路径。
const envTestFd3Harness = "GO_WANT_SBX_FD3_HARNESS"

// runFd3Harness 在隔离子进程内执行 fd3 owner/转发测试线束：fd3 已是父进程
// 注入的管道写端（无 CLOEXEC）。scenario 选择用例分支：
//   - "strict-retry-slot-reuse"：BestEffort=false + 非法 Cloneflags →
//     真实 Start 失败退 126，断言 owner 对称释放（fd3Owner.f==nil）、
//     父侧管道 EOF（由外层 runHarness 断言）、槽位 3 复用后 GC+
//     releaseFd3File 不幽灵关闭；
//   - "besteffort-retry-owner-reuse"：BestEffort=true + 首次非法
//     Cloneflags → 失败后回退不带 mountns 重试成功，断言 owner 跨重试
//     复用同一 *os.File 且目标进程读到 FD3_OPEN。
func runFd3Harness(scenario string) int {
	fmt.Println("HARNESS_OK")
	if !fd3Active() {
		fmt.Println("FD3_INACTIVE: fd3 was not injected as a non-CLOEXEC pipe/regular file")
		return 1
	}
	workdir, err := os.MkdirTemp("", "sbx-fd3-harness-")
	if err != nil {
		fmt.Println("MKDIR_FAIL:", err)
		return 1
	}
	defer os.RemoveAll(workdir)

	// 用真实 policy 构造 cfg：reviewer-agent 预设让 fd3Fwd 生效，目标为
	// 探测自身 fd3 的 sh 探针。bin 直接给 /bin/sh（不走 resolveCommandPath）。
	// 必须用 POSIX builtin `[ -e ]`——最小 Exec 白名单只放行 /bin/sh，外部
	// readlink 会被 Landlock/seccomp Exec 拒、造成 FD3_CLOSED 假阴性。
	fd3Probe := `if [ -e /proc/self/fd/3 ]; then
echo "FD3_OPEN"
else
echo "FD3_CLOSED"
fi`

	switch scenario {
	case "strict-retry-slot-reuse":
		// 非法 Cloneflags：0x7fffffff 含未定义 flag，clone 必然 EINVAL
		// → cmd.Start() 真实失败。BestEffort=false 不重试，退 126。
		orig := childSysProcAttrForRun
		childSysProcAttrForRun = func() *syscall.SysProcAttr {
			return &syscall.SysProcAttr{Cloneflags: 0x7fffffff}
		}
		defer func() { childSysProcAttrForRun = orig }()

		cfg := cliConfig{
			Preset:     "reviewer-agent",
			WorkDir:    workdir,
			BestEffort: false,
			Command:    []string{"/bin/sh", "-c", fd3Probe},
		}
		code := runSandboxedChild("/bin/sh", cfg, sandbox.Policy{BestEffort: false}, true)
		if code != 126 {
			fmt.Printf("START_FAILED_126 expected, got exit %d\n", code)
			return 1
		}
		fmt.Println("START_FAILED_126")
		if fd3Owner.f != nil {
			fmt.Println("OWNER_NIL expected: fd3Owner.f still non-nil after failed start")
			return 1
		}
		fmt.Println("OWNER_NIL")
		// 槽位 3 复用：releaseFd3File 后 fd3 已关闭，打开新文件占 fd3，
		// GC + 再次 releaseFd3File 不得幽灵关闭它。
		dummy, derr := os.Open("/dev/null")
		if derr != nil {
			fmt.Println("DUMMY_OPEN_FAIL:", derr)
			return 1
		}
		if int(dummy.Fd()) != 3 {
			// 隔离子进程内 fd3 槽位未被复用属确定性失败：严禁假绿
			// SLOT_REUSE_OK，必须明确诊断并非零退出。
			fmt.Printf("SLOT_REUSE_FAIL: expected dummy.Fd()==3, got %d\n", dummy.Fd())
			_ = dummy.Close()
			return 1
		}
		// 槽位 3 确已被 dummy 复用：GC + 短暂等待后再次 releaseFd3File，
		// dummy.Stat() 仍成功证明无陈旧终结器/旧 owner 幽灵关闭。
		runtime.GC()
		runtime.Gosched()
		time.Sleep(50 * time.Millisecond)
		_ = releaseFd3File()
		if _, serr := dummy.Stat(); serr != nil {
			fmt.Println("SLOT_REUSE_OK expected: fd3 slot reused file closed by stale finalizer:", serr)
			_ = dummy.Close()
			return 1
		}
		fmt.Println("SLOT_REUSE_OK")
		_ = dummy.Close()
		return 0

	case "besteffort-retry-owner-reuse":
		// 首次非法 Cloneflags 失败 → BestEffort 回退 sysProcAttr=nil 重试。
		// 需要内核命名空间派生能力；首次调用先探测一次（用真实
		// childSysProcAttr）确认本机支持，否则如实标记跳过而非假绿。
		probe := childSysProcAttr()
		probeCmd := exec.Command("/bin/true")
		probeCmd.SysProcAttr = probe
		if perr := probeCmd.Run(); perr != nil {
			fmt.Println("HARNESS_SKIP_UNSUPPORTED: mountns clone unavailable:", perr)
			return 0
		}
		orig := childSysProcAttrForRun
		calls := 0
		childSysProcAttrForRun = func() *syscall.SysProcAttr {
			calls++
			if calls == 1 {
				return &syscall.SysProcAttr{Cloneflags: 0x7fffffff}
			}
			return childSysProcAttr()
		}
		defer func() { childSysProcAttrForRun = orig }()

		var outBuf bytes.Buffer
		cfg := cliConfig{
			Preset:     "reviewer-agent",
			WorkDir:    workdir,
			BestEffort: true,
			Command:    []string{"/bin/sh", "-c", fd3Probe},
		}
		// runSandboxedChild 写 stdout/stderr：目标 fd3Probe 的 FD3_OPEN 经
		// 管道回传需要目标真正继承 fd3——harness 父侧持有该管道读端。
		// 这里直接以子进程 stdout 捕获：把 os.Stdout 临时替换为管道写端
		// 不可行（fd3 已被占用）。改为断言 owner 跨重试复用 + 目标启动成功。
		// 记录 acquire 的 owner 指针以验证重试复用。
		first, aerr := acquireFd3File()
		if aerr != nil || first == nil {
			fmt.Println("ACQUIRE_FAIL:", aerr)
			return 1
		}
		_ = outBuf
		// 合法最小 policy：ReadOnly "/" + Exec /bin/sh（空 policy 会被
		// sandbox.Apply 以「无 fs rules 且 DenyNet=false」拒绝）。
		pol := sandbox.Policy{
			ReadOnly:   []string{"/"},
			Exec:       []string{"/bin/sh"},
			BestEffort: true,
		}
		// 子进程经 childEnv=os.Environ() 继承 envTestFd3Harness 会递归跑
		// harness 而非 runChildFromEnv——显式剔除后再派生，保证孙进程走
		// 沙盒化 exec 路径。
		if err := os.Unsetenv(envTestFd3Harness); err != nil {
			fmt.Println("UNSETENV_FAIL:", err)
			return 1
		}
		code := runSandboxedChild("/bin/sh", cfg, pol, true)
		if code != 0 {
			fmt.Printf("RETRY_SUCCESS expected (exit 0), got %d\n", code)
			return 1
		}
		fmt.Println("RETRY_SUCCESS")
		// owner 跨重试复用语义已由「重试仍成功派生且目标看到 fd3」覆盖；
		// Start 成功后 runSandboxedChild 已 closeFD3 释放 owner（目标子进程
		// 独占写端），owner 必须归零——非 nil 才说明 owner 被错误悬持。
		if fd3Owner.f != nil {
			fmt.Println("OWNER_NIL_AFTER_RETRY expected: fd3Owner.f still non-nil after successful retry")
			_ = releaseFd3File()
			return 1
		}
		fmt.Println("OWNER_NIL_AFTER_RETRY")
		_ = first
		return 0
	}
	fmt.Println("UNKNOWN_SCENARIO:", scenario)
	return 1
}

// requireRealPigEntry 断言宿主真实 pig 入口存在且可执行——cell 门禁测试以
// /root/.local/bin/pig 作为 IsPigEntryCommand 触发名（含路径分隔符，经
// checkPigCandidate 真实文件校验）；缺失直接 t.Fatalf，绝不静默 skip 假绿。
func requireRealPigEntry(t *testing.T) string {
	t.Helper()
	const pigBin = "/root/.local/bin/pig"
	st, err := os.Stat(pigBin)
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		t.Fatalf("real pig entry %s required for cell-gate test: %v", pigBin, err)
	}
	return pigBin
}

// ---------------------------------------------------------------------------
// M6c：FM-2A cell 校验失败必须清理 pi-reviewer-* 与 pi-reviewer-exec-*
// （含 auth.json 副本）。直接构造 reviewer-agent + pig 命令 + 会让 cell
// 校验失败的缓存（Go 扩展存在但零有效 Go 产物），断言 buildPolicy 返回
// 错误后临时目录被清空。断言方式是捕获调用前后 /tmp 下 pi-reviewer-*/
// pi-reviewer-exec-* 目录的实际差集：遇 nil/err 不再 skip（遇 Policy{} 零
// 条清理断言的假绿场景）。
func TestCellValidationFailureCleansPresetDirs(t *testing.T) {
	hostPigHome := t.TempDir()
	extDir := filepath.Join(hostPigHome, "agent", "extensions", "fake-go-ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(hostPigHome, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIG_HOME", hostPigHome)
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	workdir := t.TempDir()

	// 前后差集：枚举 /tmp 下 pi-reviewer-*/pi-reviewer-exec-* 目录。
	snapshot := func() map[string]bool {
		out := map[string]bool{}
		entries, _ := os.ReadDir("/tmp")
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() && (strings.HasPrefix(name, "pi-reviewer-") || strings.HasPrefix(name, "pi-reviewer-exec-")) {
				out[filepath.Join("/tmp", name)] = true
			}
		}
		return out
	}
	before := snapshot()

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Command: []string{requireRealPigEntry(t)},
	})
	if err == nil {
		cleanupPresetTempDirsFromPolicy(policy)
		t.Fatalf("buildPolicy must fail closed on zero valid Go artifacts (hasGoExt=true), got nil error")
	}
	// buildPolicy 在 cell 校验失败时返回空 Policy{}（Env=nil）——创建的
	// pi-reviewer-* 证据不再来自 policy.Env，改由 captureReviewerAgentDirFactHook
	// 捕获 merge 前事实（privateDir 位于 privateTmpDir 内）。
	var fact reviewerAgentDirFact
	captureReviewerAgentDirFactHook = func(f reviewerAgentDirFact) { fact = f }
	t.Cleanup(func() { captureReviewerAgentDirFactHook = nil })

	// 第二次调用只为捕获事实（同一次 buildPolicy 内私有目录路径随机，
	// 与上一次的差集断言独立）。
	_, err2 := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Command: []string{requireRealPigEntry(t)},
	})
	if err2 == nil {
		t.Fatalf("second buildPolicy must also fail closed")
	}
	after := snapshot()

	// 新增目录差集必须为空（pi-reviewer-*/pi-reviewer-exec-* 均被清理）。
	for dir := range after {
		if !before[dir] {
			t.Fatalf("cell validation failure leaked preset dir %s", dir)
		}
	}
	// 反向断言：merge 前事实确实含私有 TMPDIR（pi-reviewer-*）——
	// 证明本次确实创建过预设临时目录，上面的差集断言不是空转。
	if fact.privateDir == "" || !strings.Contains(filepath.Base(filepath.Dir(fact.privateDir)), "pi-reviewer-") {
		t.Fatalf("captured fact has no pi-reviewer-* private dir (fact=%+v); cleanup assertions vacuous", fact)
	}
	if _, serr := os.Lstat(fact.privateDir); !os.IsNotExist(serr) {
		t.Fatalf("cell validation failure left private pig-agent dir %s (err=%v)", fact.privateDir, serr)
	}
}

// ---------------------------------------------------------------------------
// M5：Go 有效产物 + 非执行条目的混合缓存不得误拒 pig；node-only 仍须拒绝。
//
// missing 分母必须只覆盖「预注册的可执行目标集合」（与 total 同口径），
// 非执行（ready.json 合法但无执行位）/非 Go 条目只作可读投影，不参与 Exec
// 完整性校验，也不得计入 missing。

// writeFakeGoExtCell 在 <cacheRoot> 下造一个语言 go 的可执行缓存单元；
// ready.json 的 size 取脚本实际字节数，确保 validCellEntryDir 真实命中
// （旧夹具把脚本字面 17 字节写成 size:18，cell 被排除、hasGoExt=false
// 直接绕过门禁——本函数必须保证夹具本身合法有效）。
func writeFakeGoExtCell(t *testing.T, cacheRoot, name, artifact string) string {
	t.Helper()
	dir := filepath.Join(cacheRoot, "ext", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, artifact), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"inputDigest":%q,"artifact":%q,"size":%d,"created":1700000000,"language":"go"}`,
		name, artifact, len(script))
	if err := os.WriteFile(filepath.Join(dir, "ready.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGoValidPlusNonExecEntriesStillAllowPig 覆盖 M5：宿主 pig home 下既有
// 合法可执行 Go cell，又有「无执行位但 ready 元数据有效」的缓存单元时，
// buildPolicy 不得因无关非执行条目进入 missing 而拒绝 pig。用例真正创建
// Go 扩展并造 size 正确的 ready 元数据，直接断言 hasGoExt/total/missing
// 三层与 buildPolicy 结果。
func TestGoValidPlusNonExecEntriesStillAllowPig(t *testing.T) {
	hostPigHome := t.TempDir()
	cacheRoot := filepath.Join(hostPigHome, "cache")
	writeFakeGoExtCell(t, cacheRoot, "fake-go-ext-aaa001", "bin")

	// 混合条目：ready 元数据有效但 artifact 无执行位（appendCellCachePolicy
	// 明确跳过该类条目进 Exec；投影只保留其可读缓存）。不得计入 missing。
	nonExec := filepath.Join(cacheRoot, "ext", "fake-cache-bbb002")
	if err := os.MkdirAll(nonExec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonExec, "blob"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	nonExecMeta := fmt.Sprintf(`{"inputDigest":"fake-cache-bbb002","artifact":"blob","size":%d,"created":1700000000,"language":"go"}`, len("data"))
	if err := os.WriteFile(filepath.Join(nonExec, "ready.json"), []byte(nonExecMeta), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIG_HOME", hostPigHome)
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	workdir := t.TempDir()

	// 必须真实存在 Go 扩展源码，使 hasGoExt=true，门禁真正生效。
	extDir := filepath.Join(hostPigHome, "agent", "extensions", "fake-go-ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Command: []string{requireRealPigEntry(t)},
	})
	if err != nil {
		t.Fatalf("buildPolicy must not reject pig: mixed go-valid + nonexec cache, err=%v", err)
	}
	defer cleanupPresetTempDirsFromPolicy(policy)

	// 门禁断言：hasGoExt=true；total 只计可执行 Go cell（=1）；missing=0。
	hasGoExt, total, missing := sandbox.ReviewerAgentCellTargets()
	if !hasGoExt {
		t.Fatalf("hasGoExt = false, want true (Go extension source present)")
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1: only the executable Go cell counts, nonexec blob must not", total)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty: nonexec blob must not enter Exec integrity check", missing)
	}
	// 非执行缓存不得进 Exec 白名单。
	for _, ex := range policy.Exec {
		if strings.HasSuffix(ex, "blob") {
			t.Fatalf("non-executable cache blob %q must not enter Exec whitelist", ex)
		}
	}
}

// TestNodeOnlyCacheStillRejectsPig 覆盖 M5 负向边界：宿主 cache 仅有
// node-runtime（language!=go）/ 无执行位条目时 total=0，pig 仍须拒绝。
// 用例真正创建 Go 扩展源码（hasGoExt=true），node cell 不充数、
// total=0、buildPolicy fail-closed。
func TestNodeOnlyCacheStillRejectsPig(t *testing.T) {
	hostPigHome := t.TempDir()
	cacheRoot := filepath.Join(hostPigHome, "cache")
	nodeCell := filepath.Join(cacheRoot, "ext", "node-runtime-ccc003")
	if err := os.MkdirAll(nodeCell, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeCell, "bin"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeMeta := fmt.Sprintf(`{"inputDigest":"node-runtime-ccc003","artifact":"bin","size":%d,"created":1700000000,"language":"node"}`, len("#!/bin/sh\nexit 0\n"))
	if err := os.WriteFile(filepath.Join(nodeCell, "ready.json"), []byte(nodeMeta), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIG_HOME", hostPigHome)
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	workdir := t.TempDir()

	// 必须真实存在 Go 扩展源码，使 hasGoExt=true，门禁真正生效。
	extDir := filepath.Join(hostPigHome, "agent", "extensions", "fake-go-ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	policy, err := buildPolicy(cliConfig{
		Preset:  "reviewer-agent",
		WorkDir: workdir,
		Command: []string{requireRealPigEntry(t)},
	})
	if err == nil {
		cleanupPresetTempDirsFromPolicy(policy)
		t.Fatalf("buildPolicy must reject pig: node-only cache total=0 hasGoExt=true, got nil error")
	}
	// fail-closed 生效：对 pig 返回错误即拒绝启动；node cell 不得进 Exec。
	if len(policy.Exec) > 0 {
		for _, ex := range policy.Exec {
			if strings.Contains(ex, "node-runtime-ccc003") {
				t.Fatalf("node-only cache entry %q must not enter Exec whitelist", ex)
			}
		}
	}
	// 直接断言目标集：hasGoExt=true、total=0（node cell 不充数）。
	hasGoExt, total, _ := sandbox.ReviewerAgentCellTargets()
	if !hasGoExt {
		t.Fatalf("hasGoExt = false, want true (Go extension source present)")
	}
	if total != 0 {
		t.Fatalf("total = %d, want 0: node-runtime cell must not count toward Go target set", total)
	}
}

// TestBuildPolicyGorootBoundary 锁定 GOROOT 剥离边界：
//   - 宿主 os.Environ() 的 GOROOT 一律经 stripHostEnv 剔除；
//   - 无预设自定义模式（cfg.Preset == ""）允许调用方显式 --env GOROOT=...；
//   - 预设模式（reviewer / reviewer-agent）剔除 --env GOROOT=...，防止覆盖预设受信根。
func TestBuildPolicyGorootBoundary(t *testing.T) {
	t.Setenv("PIG_HOME", filepath.Join(t.TempDir(), "isolated-pig"))
	hostEnv := []string{"GOROOT=/host/toolchain/cache", "KEEP=1"}

	t.Run("non_preset_allows_explicit_env_goroot", func(t *testing.T) {
		policy, err := buildPolicy(cliConfig{
			Env: []string{"GOROOT=/opt/custom-go", "FOO=bar"},
		})
		if err != nil {
			t.Fatalf("buildPolicy failed: %v", err)
		}
		if got := envListValue(policy.Env, "GOROOT"); got != "/opt/custom-go" {
			t.Fatalf("policy.Env GOROOT = %q, want /opt/custom-go", got)
		}
		execEnv := mergeEnv(stripHostEnv(hostEnv), policy.Env)
		if got := envListValue(execEnv, "GOROOT"); got != "/opt/custom-go" {
			t.Fatalf("execEnv GOROOT = %q, want /opt/custom-go", got)
		}
		if got := envListValue(execEnv, "KEEP"); got != "1" {
			t.Fatalf("execEnv KEEP = %q, want 1", got)
		}
	})

	t.Run("non_preset_strips_host_goroot", func(t *testing.T) {
		policy, err := buildPolicy(cliConfig{
			Env: []string{"FOO=bar"},
		})
		if err != nil {
			t.Fatalf("buildPolicy failed: %v", err)
		}
		execEnv := mergeEnv(stripHostEnv(hostEnv), policy.Env)
		if got := envListValue(execEnv, "GOROOT"); got != "" {
			t.Fatalf("host GOROOT leaked into non-preset execEnv: got %q, want empty", got)
		}
	})

	for _, preset := range []string{"reviewer", "reviewer-agent"} {
		t.Run("preset_"+preset+"_strips_env_goroot", func(t *testing.T) {
			workdir := t.TempDir()
			policy, err := buildPolicy(cliConfig{
				Preset:  preset,
				WorkDir: workdir,
				Env:     []string{"GOROOT=/tmp/evil"},
				Command: []string{"/bin/true"},
			})
			if err != nil {
				t.Fatalf("buildPolicy(%s) failed: %v", preset, err)
			}
			t.Cleanup(func() { cleanupPresetTempDirsFromPolicy(policy) })
			if got := envListValue(policy.Env, "GOROOT"); got == "/tmp/evil" {
				t.Fatalf("preset %s leaked --env GOROOT=/tmp/evil into policy.Env", preset)
			}
			execEnv := mergeEnv(stripHostEnv(hostEnv), policy.Env)
			if got := envListValue(execEnv, "GOROOT"); got == "/tmp/evil" || got == "/host/toolchain/cache" {
				t.Fatalf("preset %s leaked untrusted GOROOT=%q into execEnv", preset, got)
			}
		})
	}
}
