package excludetoolsguard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// mockToolManager tracks invocations of SetActiveTools for testing setter semantics.
type mockToolManager struct {
	activeTools []string
	getErr      error
	setCalls    [][]string
}

func (m *mockToolManager) GetActiveTools() ([]string, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.activeTools, nil
}

func (m *mockToolManager) SetActiveTools(tools []string) {
	m.setCalls = append(m.setCalls, tools)
	m.activeTools = tools
}

// 1. Identity Resolution Tests with Real Unix Domain Socket Fake Server
func TestQueryPaneShellPid_RealUnixSocket(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "fake-herdr.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to create unix socket listener: %v", err)
	}

	type serverAction func(conn net.Conn)

	var (
		mu            sync.RWMutex
		currentAction serverAction
		wg            sync.WaitGroup
	)

	setAction := func(act serverAction) {
		mu.Lock()
		currentAction = act
		mu.Unlock()
	}
	getAction := func() serverAction {
		mu.RLock()
		defer mu.RUnlock()
		return currentAction
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				act := getAction()
				if act != nil {
					act(c)
				}
			}(conn)
		}
	}()

	defer func() {
		_ = listener.Close()
		wg.Wait()
	}()

	t.Run("MainAgent_Success", func(t *testing.T) {
		setAction(func(c net.Conn) {
			reader := bufio.NewReader(c)
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			var req struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.Unmarshal([]byte(line), &req)
			if req.ID == "pi-identity" && req.Method == "pane.process_info" && req.Params["pane_id"] == "herdr:tab1:p1" {
				resp := `{"id":"pi-identity","result":{"process_info":{"pane_id":"herdr:tab1:p1","shell_pid":4242}}}` + "\n"
				_, _ = c.Write([]byte(resp))
			}
		})

		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 500*time.Millisecond)
		if !ok || pid != 4242 {
			t.Errorf("expected (4242, true), got (%d, %v)", pid, ok)
		}
	})

	t.Run("Subagent_MismatchedPaneId", func(t *testing.T) {
		setAction(func(c net.Conn) {
			resp := `{"id":"pi-identity","result":{"process_info":{"pane_id":"herdr:tab1:other","shell_pid":4242}}}` + "\n"
			_, _ = c.Write([]byte(resp))
		})
		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 500*time.Millisecond)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) for mismatched pane_id, got (%d, %v)", pid, ok)
		}
	})

	t.Run("Subagent_HalfClosedConnection", func(t *testing.T) {
		setAction(func(c net.Conn) {
			// Close connection immediately without writing any bytes (half-closed / peer hangup)
			_ = c.Close()
		})
		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 500*time.Millisecond)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) for half-closed connection, got (%d, %v)", pid, ok)
		}
	})

	t.Run("Subagent_StringShellPid", func(t *testing.T) {
		setAction(func(c net.Conn) {
			// shell_pid as string should be strictly rejected (only JSON number accepted)
			resp := `{"id":"pi-identity","result":{"process_info":{"pane_id":"herdr:tab1:p1","shell_pid":"4242"}}}` + "\n"
			_, _ = c.Write([]byte(resp))
		})
		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 500*time.Millisecond)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) for string shell_pid, got (%d, %v)", pid, ok)
		}
	})

	t.Run("Subagent_InvalidShellPid", func(t *testing.T) {
		setAction(func(c net.Conn) {
			resp := `{"id":"pi-identity","result":{"process_info":{"pane_id":"herdr:tab1:p1","shell_pid":-5}}}` + "\n"
			_, _ = c.Write([]byte(resp))
		})
		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 500*time.Millisecond)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) for negative shell_pid, got (%d, %v)", pid, ok)
		}
	})

	t.Run("Subagent_GarbageJSON", func(t *testing.T) {
		setAction(func(c net.Conn) {
			_, _ = c.Write([]byte("{this is not valid json\n"))
		})
		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 500*time.Millisecond)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) for garbage json, got (%d, %v)", pid, ok)
		}
	})

	t.Run("Subagent_HangTimeout", func(t *testing.T) {
		setAction(func(c net.Conn) {
			// Sleep longer than query timeout (50ms)
			time.Sleep(200 * time.Millisecond)
		})
		start := time.Now()
		pid, ok := QueryPaneShellPid(sockPath, "herdr:tab1:p1", 50*time.Millisecond)
		elapsed := time.Since(start)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) on timeout, got (%d, %v)", pid, ok)
		}
		if elapsed > 150*time.Millisecond {
			t.Errorf("timeout did not terminate promptly: took %v", elapsed)
		}
	})

	t.Run("Subagent_SocketNotExist", func(t *testing.T) {
		nonExistentPath := filepath.Join(tmpDir, "does-not-exist.sock")
		pid, ok := QueryPaneShellPid(nonExistentPath, "herdr:tab1:p1", 100*time.Millisecond)
		if ok || pid != 0 {
			t.Errorf("expected (0, false) for non-existent socket, got (%d, %v)", pid, ok)
		}
	})
}

// 2. Identity Resolution Fail-Closed Logic (Markers & S1-S4)
func TestResolveAgentIdentityWithEnv(t *testing.T) {
	baseEnv := func() IdentityEnv {
		return IdentityEnv{
			Getenv: func(k string) string {
				if k == "HERDR_PANE_ID" {
					return "herdr:tab1:p1"
				}
				return ""
			},
			Args:       []string{"pi"},
			PID:        100,
			SocketPath: "/tmp/fake.sock",
			Timeout:    time.Second,
			QuerySocket: func(sock, pane string, to time.Duration) (int, bool) {
				return 4242, true
			},
			ReadStat: func(pid int) (int, bool) {
				if pid == 100 {
					return 4242, true
				}
				if pid == 4242 {
					return 1, true
				}
				return 0, false
			},
			ReadCmdline: func(pid int) string {
				if pid == 100 {
					return "pi"
				}
				if pid == 4242 {
					return "/bin/bash"
				}
				return ""
			},
		}
	}

	t.Run("MainAgent_AllS1ToS4Hold", func(t *testing.T) {
		env := baseEnv()
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if isSub {
			t.Errorf("expected main agent (isSub=false), got true: %s", reason)
		}
		if !strings.Contains(reason, "S1-S4 hold: pane=herdr:tab1:p1 shell_pid=4242 hop=1") {
			t.Errorf("unexpected reason format: %s", reason)
		}
	})

	t.Run("NegativeMarkers", func(t *testing.T) {
		markers := []struct {
			name   string
			mutate func(e *IdentityEnv)
			marker string
		}{
			{
				name: "PI_SUBAGENT=1",
				mutate: func(e *IdentityEnv) {
					origGet := e.Getenv
					e.Getenv = func(k string) string {
						if k == "PI_SUBAGENT" {
							return "1"
						}
						return origGet(k)
					}
				},
				marker: "launch marker: PI_SUBAGENT=1",
			},
			{
				name: "PI_ROLE=worker",
				mutate: func(e *IdentityEnv) {
					origGet := e.Getenv
					e.Getenv = func(k string) string {
						if k == "PI_ROLE" {
							return "worker"
						}
						return origGet(k)
					}
				},
				marker: "launch marker: PI_ROLE is set",
			},
			{
				name: "argv --name role-",
				mutate: func(e *IdentityEnv) {
					e.Args = []string{"pi", "--name", "role-researcher"}
				},
				marker: "launch marker: argv has --name role-",
			},
			{
				name: "argv /tmp/herdr-role-sessions",
				mutate: func(e *IdentityEnv) {
					e.Args = []string{"pi", "/tmp/herdr-role-sessions/sess-1"}
				},
				marker: "launch marker: argv has /tmp/herdr-role-sessions",
			},
			{
				name: "argv /tmp/dispatch-",
				mutate: func(e *IdentityEnv) {
					e.Args = []string{"pi", "/tmp/dispatch-task.json"}
				},
				marker: "launch marker: argv has /tmp/dispatch-",
			},
		}

		for _, tc := range markers {
			t.Run(tc.name, func(t *testing.T) {
				env := baseEnv()
				tc.mutate(&env)
				isSub, reason := ResolveAgentIdentityWithEnv(env)
				if !isSub {
					t.Errorf("expected subagent for %s, got false", tc.name)
				}
				if !strings.Contains(reason, tc.marker) {
					t.Errorf("expected reason to contain %q, got %q", tc.marker, reason)
				}
			})
		}
	})

	// S1 只要求 pane id 非空（它是 herdr 查询的查找键）；pane 命名不构成判据。
	t.Run("S1_PaneIdUnset_FailsClosed", func(t *testing.T) {
		envEmpty := baseEnv()
		envEmpty.Getenv = func(k string) string { return "" }
		isSub, reason := ResolveAgentIdentityWithEnv(envEmpty)
		if !isSub || !strings.Contains(reason, "S1: HERDR_PANE_ID unset or empty") {
			t.Errorf("expected S1 unset error, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// pane 命名不再作判据：非 ":p1" 后缀的 pane id 本身绝不导致子代理判定，
	// 它只作为辅助证据出现在 reason 里。
	t.Run("S1_PaneNamingIsNotACriterion", func(t *testing.T) {
		envNotP1 := baseEnv()
		envNotP1.Getenv = func(k string) string {
			if k == "HERDR_PANE_ID" {
				return "herdr:tab1:p2"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(envNotP1)
		if isSub {
			t.Errorf("pane naming must not be an identity criterion, got subagent: %s", reason)
		}
		if !strings.Contains(reason, "pane=herdr:tab1:p2") {
			t.Errorf("expected pane id recorded as auxiliary evidence, got: %s", reason)
		}
	})

	t.Run("S2_SocketFailure", func(t *testing.T) {
		env := baseEnv()
		env.QuerySocket = func(sock, pane string, to time.Duration) (int, bool) {
			return 0, false
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "S2: herdr pane query for herdr:tab1:p1 failed") {
			t.Errorf("expected S2 failure, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	t.Run("S3_AncestryMissingShellPid", func(t *testing.T) {
		env := baseEnv()
		// ancestry chain does not contain 4242
		env.ReadStat = func(pid int) (int, bool) {
			if pid == 100 {
				return 9999, true
			}
			if pid == 9999 {
				return 1, true
			}
			return 0, false
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "S3: pane herdr:tab1:p1 shell_pid=4242 is not in this process ancestry") {
			t.Errorf("expected S3 ancestry missing shell_pid, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	t.Run("S3_AncestryChainEmpty", func(t *testing.T) {
		env := baseEnv()
		env.ReadStat = func(pid int) (int, bool) {
			return 0, false
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "S3: /proc ancestry for this process is unreadable") {
			t.Errorf("expected S3 ancestry unreadable, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// M1 正向实证 ①：真实的 subprocess 拓扑（扩展是 pi 宿主的子进程） -> 判定为主代理（main agent）
	t.Run("SubprocessTopology_PiHost_MainAgent", func(t *testing.T) {
		env := baseEnv()
		// 拓扑：100 (exclude-tools-guard subprocess) -> 200 (pi 宿主) -> 4242 (pane shell)
		env.PID = 100
		env.Getenv = func(k string) string {
			switch k {
			case "HERDR_PANE_ID":
				return "herdr:tab1:p1"
			case "PIG_EXT_SOCKET":
				return "/tmp/pig-ext-sock.sock"
			default:
				return ""
			}
		}
		env.ReadStat = func(pid int) (int, bool) {
			if pid == 100 {
				return 200, true
			}
			if pid == 200 {
				return 4242, true
			}
			if pid == 4242 {
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			if pid == 100 {
				return "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"
			}
			if pid == 200 {
				return "pi -p hello"
			}
			if pid == 4242 {
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if isSub {
			t.Errorf("expected main agent (isSub=false) for pi host subprocess topology, got isSub=true, reason=%s", reason)
		}
		if !strings.Contains(reason, "S1-S4 hold: pane=herdr:tab1:p1 shell_pid=4242 hop=2") {
			t.Errorf("unexpected reason format: %s", reason)
		}
	})

	// M1 正向实证：将来迁移到 pig 宿主的 subprocess 拓扑 -> 判定为主代理（main agent）
	t.Run("SubprocessTopology_PigHost_MainAgent", func(t *testing.T) {
		env := baseEnv()
		// 拓扑：100 (exclude-tools-guard subprocess) -> 200 (pig 宿主) -> 4242 (pane shell)
		env.PID = 100
		env.Getenv = func(k string) string {
			switch k {
			case "HERDR_PANE_ID":
				return "herdr:tab1:p1"
			case "PIG_EXT_SOCKET":
				return "/tmp/pig-ext-sock.sock"
			default:
				return ""
			}
		}
		env.ReadStat = func(pid int) (int, bool) {
			if pid == 100 {
				return 200, true
			}
			if pid == 200 {
				return 4242, true
			}
			if pid == 4242 {
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			if pid == 100 {
				return "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"
			}
			if pid == 200 {
				return "pig -p hello"
			}
			if pid == 4242 {
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if isSub {
			t.Errorf("expected main agent (isSub=false) for pig host subprocess topology, got isSub=true, reason=%s", reason)
		}
	})

	// M1 反向实证 ②：宿主与 pane shell 之间夹了 agent 运行时 -> 判定为子代理（subagent）
	t.Run("SubprocessTopology_AgentRuntimeBetweenHostAndShell_Subagent", func(t *testing.T) {
		env := baseEnv()
		// 拓扑：100 (exclude-tools-guard subprocess) -> 200 (pi 宿主) -> 300 (中间 agent 运行时) -> 4242 (pane shell)
		env.PID = 100
		env.Getenv = func(k string) string {
			switch k {
			case "HERDR_PANE_ID":
				return "herdr:tab1:p1"
			case "PIG_EXT_SOCKET":
				return "/tmp/pig-ext-sock.sock"
			default:
				return ""
			}
		}
		env.ReadStat = func(pid int) (int, bool) {
			if pid == 100 {
				return 200, true
			}
			if pid == 200 {
				return 300, true
			}
			if pid == 300 {
				return 4242, true
			}
			if pid == 4242 {
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			if pid == 100 {
				return "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"
			}
			if pid == 200 {
				return "pi -p hello"
			}
			if pid == 300 {
				return "pi-coding-agent subagent-worker"
			}
			if pid == 4242 {
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "S4: agent runtime ancestor pid=300") {
			t.Errorf("expected S4 agent runtime rejection, got isSub=%v, reason=%s", isSub, reason)
		}
	})
}

// 2a. 身份判据重构回归（oracle2 反例 A/B + 三条已决落地）
//   - pane 命名不构成判据；PIG_EXT_SOCKET 为主判别式；hostIndex = 第一个 agent-runtime 祖先；
//   - 找不到祖先 / fused 自身无法识别 / herdr 查询失败一律 fail-closed 判子代理。
func TestResolveAgentIdentity_IdentityCriteriaRefactor(t *testing.T) {
	writeArgvFile := func(name, content string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("failed to write argv file: %v", err)
		}
		return path
	}

	// subprocessEnv 构造 subprocess 形态（PIG_EXT_SOCKET 非空）的判定环境。
	subprocessEnv := func(paneID string) IdentityEnv {
		envMap := map[string]string{
			"HERDR_PANE_ID":  paneID,
			"PIG_EXT_SOCKET": "/tmp/pig-ext-sock.sock",
		}
		return IdentityEnv{
			Getenv: func(k string) string { return envMap[k] },
			Unsetenv: func(k string) error {
				delete(envMap, k)
				return nil
			},
			Args:       []string{"/root/workspace/pi-extensions/go/bin/exclude-tools-guard"},
			PID:        100,
			SocketPath: "/tmp/fake.sock",
			Timeout:    time.Second,
			QuerySocket: func(sock, pane string, to time.Duration) (int, bool) {
				return 4242, true
			},
			ArgvCache: &ArgvCache{},
		}
	}

	const extBin = "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"

	// 用例 1：子代理宿主。subprocess 拓扑下宿主自身 cmdline 带 --name role-reviewer
	// （经 PIG_HARNESS_ARGV_FILE 桥接，因为 subprocess 的 os.Args 只有二进制路径）-> 判 subagent。
	// 取代原来依赖 ":p1"  pane 命名的旧用例。
	t.Run("SubagentHost_ArgvMarker_Subagent", func(t *testing.T) {
		env := subprocessEnv("herdr:tab1:p1")
		argvPath := writeArgvFile("role-reviewer.json", `["pi","--name","role-reviewer","-p","review"]`)
		origGet := env.Getenv
		env.Getenv = func(k string) string {
			if k == "PIG_HARNESS_ARGV_FILE" {
				return argvPath
			}
			return origGet(k)
		}
		// 拓扑：100 (扩展子进程) -> 200 (role-reviewer 宿主) -> 4242 (pane shell)
		env.ReadStat = func(pid int) (int, bool) {
			switch pid {
			case 100:
				return 200, true
			case 200:
				return 4242, true
			case 4242:
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			switch pid {
			case 100:
				return extBin
			case 200:
				return "pi --name role-reviewer -p review"
			case 4242:
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if !isSub || !strings.Contains(reason, "launch marker: argv has --name role-") {
			t.Errorf("expected subagent via argv role marker, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// 用例 2：隔层 spawn（oracle2 反例 A 回归）。ext(100)->sh(200)->pi(300)->bash(4242)
	// hostIndex 取第一个 agent-runtime 祖先（pi@300），S4 扫描区间为空 -> 判 main。
	t.Run("GappedSpawn_IntermediateShell_MainAgent", func(t *testing.T) {
		env := subprocessEnv("herdr:tab1:p1")
		env.ReadStat = func(pid int) (int, bool) {
			switch pid {
			case 100:
				return 200, true
			case 200:
				return 300, true
			case 300:
				return 4242, true
			case 4242:
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			switch pid {
			case 100:
				return extBin
			case 200:
				return "/bin/sh -c pig-ext"
			case 300:
				return "pi -p hello"
			case 4242:
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if isSub {
			t.Errorf("expected main agent for gapped spawn topology, got subagent: %s", reason)
		}
		if !strings.Contains(reason, "S1-S4 hold: pane=herdr:tab1:p1 shell_pid=4242 hop=3") {
			t.Errorf("unexpected reason format: %s", reason)
		}
	})

	// 用例 3：fused 形态但 self cmdline 无法识别（oracle2 反例 B 回归）。
	// PIG_EXT_SOCKET 未设 + self 为 "node /x/index.js" -> fail-closed 判 subagent。
	// 旧代码的 "|| len(chain) >= 2" 旁路会误把父进程当宿主从而放行。
	t.Run("FusedUnrecognizedSelfCmdline_Subagent", func(t *testing.T) {
		env := subprocessEnv("herdr:tab1:p1")
		env.Getenv = func(k string) string {
			if k == "HERDR_PANE_ID" {
				return "herdr:tab1:p1"
			}
			return ""
		}
		env.ReadStat = func(pid int) (int, bool) {
			switch pid {
			case 100:
				return 4242, true
			case 4242:
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			switch pid {
			case 100:
				return "node /x/index.js"
			case 4242:
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if !isSub || !strings.Contains(reason, "fused form but self pid=100 cmdline") {
			t.Errorf("expected fail-closed subagent for unrecognized fused self, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// 用例 4：subprocess 拓扑但链上找不到 agent-runtime 祖先 -> fail-closed 判 subagent。
	t.Run("NoAgentRuntimeAncestor_Subagent", func(t *testing.T) {
		env := subprocessEnv("herdr:tab1:p1")
		env.ReadStat = func(pid int) (int, bool) {
			switch pid {
			case 100:
				return 200, true
			case 200:
				return 4242, true
			case 4242:
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			switch pid {
			case 100:
				return extBin
			case 200:
				return "/bin/sh -c pig-ext"
			case 4242:
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if !isSub || !strings.Contains(reason, "no agent-runtime ancestor") {
			t.Errorf("expected fail-closed subagent when no agent-runtime ancestor exists, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// 用例 5：同进程第二次调用仍判 subagent（argv 进程内缓存，unsetenv 之后不丢标记）。
	// 与 2b 的 fused 拓扑用例互补：这里走 subprocess 拓扑，证明宿主锚定不会把第 2 个 turn 放行。
	t.Run("ConsecutiveCalls_Subagent_ArgvCache", func(t *testing.T) {
		env := subprocessEnv("herdr:tab1:p1")
		argvPath := writeArgvFile("consecutive-subprocess.json", `["pi","--name","role-reviewer-consecutive","-p","review"]`)
		origGet := env.Getenv
		// 模拟进程级 unsetenv：读取后该变量在本进程内消失（但缓存仍保留解析结果）。
		argvPathLive := argvPath
		env.Getenv = func(k string) string {
			if k == "PIG_HARNESS_ARGV_FILE" {
				return argvPathLive
			}
			return origGet(k)
		}
		env.Unsetenv = func(k string) error {
			if k == "PIG_HARNESS_ARGV_FILE" {
				argvPathLive = ""
			}
			return nil
		}
		env.ReadStat = func(pid int) (int, bool) {
			switch pid {
			case 100:
				return 200, true
			case 200:
				return 4242, true
			case 4242:
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			switch pid {
			case 100:
				return extBin
			case 200:
				return "pi --name role-reviewer-consecutive -p review"
			case 4242:
				return "/bin/bash"
			}
			return ""
		}

		isSub1, reason1 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#1: isSubagent=%v reason=%s", isSub1, reason1)
		if !isSub1 || !strings.Contains(reason1, "launch marker: argv has --name role-") {
			t.Fatalf("call#1 expected subagent, got isSub=%v, reason=%s", isSub1, reason1)
		}
		if v := env.Getenv("PIG_HARNESS_ARGV_FILE"); v != "" {
			t.Fatalf("expected PIG_HARNESS_ARGV_FILE unset after call#1, got %q", v)
		}

		isSub2, reason2 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#2: isSubagent=%v reason=%s", isSub2, reason2)
		if !isSub2 || !strings.Contains(reason2, "launch marker: argv has --name role-") {
			t.Errorf("call#2 expected cached subagent, got isSub=%v, reason=%s", isSub2, reason2)
		}
	})

	// 用例 6：pane 名不再作判据。HERDR_PANE_ID=w32:p23（非 :p1）+ 其余条件都像主代理
	// （宿主是 pi、无负向标记、herdr 查询成功）-> 判 main。
	t.Run("PaneNamingNotCriterion_MainAgent", func(t *testing.T) {
		env := subprocessEnv("w32:p23")
		env.ReadStat = func(pid int) (int, bool) {
			switch pid {
			case 100:
				return 200, true
			case 200:
				return 4242, true
			case 4242:
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string {
			switch pid {
			case 100:
				return extBin
			case 200:
				return "pi -p hello"
			case 4242:
				return "/bin/bash"
			}
			return ""
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if isSub {
			t.Errorf("expected main agent (pane naming is not a criterion), got subagent: %s", reason)
		}
		if !strings.Contains(reason, "pane=w32:p23") {
			t.Errorf("expected pane id as auxiliary evidence, got: %s", reason)
		}
	})
}

// 2e. packed-go cell 形态（真实 PiG 默认形态）回归：
// Go 扩展在真实 PiG 下默认作为 packed cell 成员运行——进程环境里没有 PIG_EXT_SOCKET，
// 只有每个成员一条 PIG_EXT_SOCKET_<NAME>（runtimecell.SocketEnvName）加上
// PIG_EXT_PACKED_CELL / PIG_EXT_ACTIVE_MEMBERS。旧实现只看 PIG_EXT_SOCKET，于是把 packed
// runner 误判成 fused 形态、认不出自身 cmdline，把真实主代理 fail-closed 成子代理
// （jev_check / ask_user_question 被摘、每个 turn 清 appendSystemPrompt）。
func TestResolveAgentIdentity_PackedCellForm(t *testing.T) {
	// packed-go cell 进程的真实 cmdline：<cells>/go/<hash>/runner。
	const packedRunner = "/root/.pig-test/state/cells/go/1a2b3c4d/runner"
	// 本组件的 packed 成员键（runtimecell.SocketEnvName("exclude-tools-guard")）。
	const memberSocketKey = "PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD"

	// packedCellEnv 只放真实迁移键名，不做伪造：没有 PIG_EXT_SOCKET，有成员 sock 键 +
	// PIG_EXT_PACKED_CELL + PIG_EXT_ACTIVE_MEMBERS。
	packedCellEnv := func(envMap map[string]string) IdentityEnv {
		envMap["HERDR_PANE_ID"] = "herdr:tab1:p1"
		return IdentityEnv{
			Getenv:   func(k string) string { return envMap[k] },
			Unsetenv: func(k string) error { delete(envMap, k); return nil },
			Args:     []string{packedRunner},
			PID:      100,
			// Environ 按真实进程环境枚举键名（packed 成员键名不可预知，只能前缀扫描）。
			Environ: func() []string {
				entries := make([]string, 0, len(envMap))
				for k, v := range envMap {
					entries = append(entries, k+"="+v)
				}
				return entries
			},
			SocketPath: "/tmp/fake.sock",
			Timeout:    time.Second,
			QuerySocket: func(sock, pane string, to time.Duration) (int, bool) {
				return 4242, true
			},
			ArgvCache: &ArgvCache{},
		}
	}

	// topology 把 pids 依次绑成祖先链（pids[0]=self，最后一个是 pane shell）。
	setTopology := func(env *IdentityEnv, pids []int, cmdlines map[int]string) {
		env.ReadStat = func(pid int) (int, bool) {
			for i, p := range pids {
				if p != pid {
					continue
				}
				if i+1 < len(pids) {
					return pids[i+1], true
				}
				return 1, true
			}
			return 0, false
		}
		env.ReadCmdline = func(pid int) string { return cmdlines[pid] }
	}

	// 用例 1（阻断修复的主证据）：只注入真实成员键 PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD、
	// 不设 PIG_EXT_SOCKET；配「宿主是 pi、父进程是 pane shell」拓扑 -> 必须走 subprocess 分支并判 main。
	t.Run("PackedCell_RealMemberKey_MainAgent", func(t *testing.T) {
		env := packedCellEnv(map[string]string{
			memberSocketKey: "/run/x.sock",
		})
		setTopology(&env, []int{100, 200, 4242}, map[int]string{
			100:  packedRunner,
			200:  "pi -p hello",
			4242: "/bin/bash",
		})
		if form, proof := DetectSubprocessForm(env.Getenv, env.Environ); !form || proof != memberSocketKey {
			t.Fatalf("expected %s to prove the subprocess form, got form=%v proof=%q", memberSocketKey, form, proof)
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if isSub {
			t.Errorf("packed-go cell member must be recognised as subprocess form -> main, got subagent: %s", reason)
		}
		if !strings.Contains(reason, "S1-S4 hold: pane=herdr:tab1:p1 shell_pid=4242 hop=2") {
			t.Errorf("unexpected reason format: %s", reason)
		}
	})

	// 用例 2：完整的 packed-cell 形态（成员键 + PIG_EXT_PACKED_CELL + PIG_EXT_ACTIVE_MEMBERS），
	// 但宿主与 pane shell 之间夹了 agent runtime -> 必须仍判 subagent（形态识别放宽不削弱 S4）。
	t.Run("PackedCell_AgentRuntimeBetweenHostAndShell_Subagent", func(t *testing.T) {
		env := packedCellEnv(map[string]string{
			memberSocketKey:          "/run/x.sock",
			"PIG_EXT_PACKED_CELL":    "go-1a2b3c4d",
			"PIG_EXT_ACTIVE_MEMBERS": "exclude-tools-guard,no-tables",
		})
		form, proof := DetectSubprocessForm(env.Getenv, env.Environ)
		t.Logf("subprocessForm=%v proof=%s", form, proof)
		if !form {
			t.Fatalf("expected the packed cell env to prove the subprocess form, got proof=%q", proof)
		}
		setTopology(&env, []int{100, 200, 300, 4242}, map[int]string{
			100:  packedRunner,
			200:  "pi -p hello",
			300:  "pi-coding-agent subagent-worker",
			4242: "/bin/bash",
		})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if !isSub || !strings.Contains(reason, "S4: agent runtime ancestor pid=300") {
			t.Errorf("expected S4 rejection for nested agent runtime in packed form, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// 用例 3（对照，原行为不得回归）：isolated 形态只有 PIG_EXT_SOCKET
	// -> 仍走 subprocess 分支、仍判 main。
	t.Run("IsolatedForm_PIG_EXT_SOCKET_MainAgent", func(t *testing.T) {
		env := packedCellEnv(map[string]string{
			"PIG_EXT_SOCKET": "/run/isolated.sock",
		})
		setTopology(&env, []int{100, 200, 4242}, map[int]string{
			100:  "/root/workspace/pi-extensions/go/bin/exclude-tools-guard",
			200:  "pi -p hello",
			4242: "/bin/bash",
		})
		if form, proof := DetectSubprocessForm(env.Getenv, env.Environ); !form || proof != "PIG_EXT_SOCKET" {
			t.Fatalf("expected PIG_EXT_SOCKET to prove the subprocess form, got form=%v proof=%q", form, proof)
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if isSub {
			t.Errorf("isolated form must stay main, got subagent: %s", reason)
		}
	})

	// 用例 4：成员键出现在环境里但值为空（"PIG_EXT_SOCKET_X="）——不得当作 subprocess 证明。
	// 此时退化为 fused 形态，packed runner 的 cmdline 认不出 -> fail-closed 判 subagent。
	t.Run("MemberKeyListedButEmpty_NotProof_FusedForm", func(t *testing.T) {
		env := packedCellEnv(map[string]string{
			memberSocketKey: "",
		})
		setTopology(&env, []int{100, 4242}, map[int]string{
			100:  packedRunner,
			4242: "/bin/bash",
		})
		if form, proof := DetectSubprocessForm(env.Getenv, env.Environ); form || proof != "" {
			t.Fatalf("an empty member key must not prove the subprocess form, got form=%v proof=%q", form, proof)
		}
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		t.Logf("isSubagent=%v reason=%s", isSub, reason)
		if !isSub || !strings.Contains(reason, "fused form but self pid=100 cmdline") {
			t.Errorf("expected fail-closed subagent for empty member key, got isSub=%v, reason=%s", isSub, reason)
		}
	})
}

// 2f. 身份降级可见性（应修 2）：S1/S3/S4 的 fail-closed 分支与 S2 共用同一条去重 warning，
// reason 里带分支名；debug 开关只加细节，不改判定逻辑。
func TestResolveAgentIdentity_FailClosedBranches_WarnOnce(t *testing.T) {
	baseEnv := func() IdentityEnv {
		return IdentityEnv{
			Getenv: func(k string) string {
				switch k {
				case "HERDR_PANE_ID":
					return "herdr:tab1:p1"
				case "PIG_EXT_SOCKET":
					return "/tmp/pig-ext-sock.sock"
				}
				return ""
			},
			Args:       []string{"pi"},
			PID:        100,
			Environ:    func() []string { return nil },
			SocketPath: "/tmp/fake.sock",
			Timeout:    time.Second,
			QuerySocket: func(sock, pane string, to time.Duration) (int, bool) {
				return 4242, true
			},
			ReadStat: func(pid int) (int, bool) {
				switch pid {
				case 100:
					return 200, true
				case 200:
					return 4242, true
				case 4242:
					return 1, true
				}
				return 0, false
			},
			ReadCmdline: func(pid int) string {
				switch pid {
				case 100:
					return "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"
				case 200:
					return "pi -p hello"
				case 4242:
					return "/bin/bash"
				}
				return ""
			},
			ArgvCache: &ArgvCache{},
		}
	}

	captureStderr := func(run func()) string {
		orig := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		os.Stderr = w
		defer func() { os.Stderr = orig }()
		run()
		_ = w.Close()
		out, _ := io.ReadAll(r)
		_ = r.Close()
		return string(out)
	}

	cases := []struct {
		name       string
		mutate     func(*IdentityEnv)
		wantBranch string
	}{
		{
			name: "S1_PaneIdUnset",
			mutate: func(e *IdentityEnv) {
				e.Getenv = func(string) string { return "" }
			},
			wantBranch: "S1: HERDR_PANE_ID unset or empty",
		},
		{
			name: "S3_NoAgentRuntimeAncestor",
			mutate: func(e *IdentityEnv) {
				e.ReadCmdline = func(pid int) string {
					switch pid {
					case 100:
						return "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"
					case 200:
						return "/bin/sh -c pig-ext"
					case 4242:
						return "/bin/bash"
					}
					return ""
				}
			},
			wantBranch: "S3: no agent-runtime ancestor",
		},
		{
			name: "S4_AgentRuntimeBetweenHostAndShell",
			mutate: func(e *IdentityEnv) {
				e.ReadStat = func(pid int) (int, bool) {
					switch pid {
					case 100:
						return 200, true
					case 200:
						return 300, true
					case 300:
						return 4242, true
					case 4242:
						return 1, true
					}
					return 0, false
				}
				e.ReadCmdline = func(pid int) string {
					switch pid {
					case 100:
						return "/root/workspace/pi-extensions/go/bin/exclude-tools-guard"
					case 200:
						return "pi -p hello"
					case 300:
						return "pi-coding-agent subagent-worker"
					case 4242:
						return "/bin/bash"
					}
					return ""
				}
			},
			wantBranch: "S4: agent runtime ancestor",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetIdentityWarnOnce()
			env := baseEnv()
			tc.mutate(&env)
			var reasons []string
			var subagents []bool
			out := captureStderr(func() {
				for i := 0; i < 3; i++ {
					isSub, reason := ResolveAgentIdentityWithEnv(env)
					subagents = append(subagents, isSub)
					reasons = append(reasons, reason)
				}
			})
			for i, isSub := range subagents {
				if !isSub {
					t.Fatalf("call#%d expected fail-closed subagent, got main (reason: %s)", i+1, reasons[i])
				}
				if !strings.Contains(reasons[i], tc.wantBranch) {
					t.Fatalf("call#%d reason must name branch %q, got %q", i+1, tc.wantBranch, reasons[i])
				}
			}
			if count := strings.Count(out, "[subagent-identity] warning: identity not verified, fail-closed to subagent:"); count != 1 {
				t.Errorf("expected exactly 1 deduplicated warning line, got %d, stderr:\n%s", count, out)
			}
			if !strings.Contains(out, tc.wantBranch) {
				t.Errorf("expected the warning to name branch %q, stderr:\n%s", tc.wantBranch, out)
			}
		})
	}
}

// 2d. herdr 查询失败/超时：fail-closed 判子代理 + 只记一条日志（sync.Once 去重）。
func TestResolveAgentIdentity_HerdrQueryFailure_WarnsOnce(t *testing.T) {
	ResetIdentityWarnOnce()

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stderr = w

	env := IdentityEnv{
		Getenv: func(k string) string {
			if k == "HERDR_PANE_ID" {
				return "herdr:tab1:p1"
			}
			return ""
		},
		Args:       []string{"pi"},
		PID:        100,
		SocketPath: "/tmp/fake.sock",
		Timeout:    1500 * time.Millisecond,
		QuerySocket: func(sock, pane string, to time.Duration) (int, bool) {
			if to != 1500*time.Millisecond {
				t.Errorf("expected the 1500ms budget to be preserved, got %v", to)
			}
			return 0, false
		},
		ReadStat: func(pid int) (int, bool) {
			if pid == 100 {
				return 4242, true
			}
			return 0, false
		},
		ReadCmdline: func(pid int) string {
			if pid == 100 {
				return "pi"
			}
			return ""
		},
		ArgvCache: &ArgvCache{},
	}

	for i := 0; i < 3; i++ {
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "S2: herdr pane query for herdr:tab1:p1 failed") {
			t.Fatalf("call#%d expected fail-closed subagent, got isSub=%v, reason=%s", i+1, isSub, reason)
		}
	}

	_ = w.Close()
	os.Stderr = origStderr
	out, _ := io.ReadAll(r)
	_ = r.Close()

	outputStr := string(out)
	if !strings.Contains(outputStr, "[subagent-identity] warning: identity not verified, fail-closed to subagent: S2: herdr pane query for herdr:tab1:p1 failed") {
		t.Errorf("expected one warning line naming the S2 branch, got: %q", outputStr)
	}
	if count := strings.Count(outputStr, "warning: identity not verified"); count != 1 {
		t.Errorf("expected exactly 1 warning line, got %d, output:\n%s", count, outputStr)
	}
}

// 2b. PIG_HARNESS_ARGV_FILE Tests (Must Fix 1: argv bridge)
func TestHostArgvFile_PIG_HARNESS_ARGV_FILE(t *testing.T) {
	tmpDir := t.TempDir()

	writeArgvFile := func(filename string, content string) string {
		path := filepath.Join(tmpDir, filename)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("failed to write argv file: %v", err)
		}
		return path
	}

	makeEnvWithArgvFile := func(filePath string, baseArgs []string) IdentityEnv {
		envMap := map[string]string{
			"HERDR_PANE_ID": "herdr:tab1:p1",
		}
		if filePath != "" {
			envMap["PIG_HARNESS_ARGV_FILE"] = filePath
		}
		return IdentityEnv{
			Getenv: func(k string) string {
				return envMap[k]
			},
			Unsetenv: func(k string) error {
				delete(envMap, k)
				return nil
			},
			Args:       baseArgs,
			PID:        100,
			SocketPath: "/tmp/fake.sock",
			Timeout:    time.Second,
			QuerySocket: func(sock, pane string, to time.Duration) (int, bool) {
				return 4242, true
			},
			ReadStat: func(pid int) (int, bool) {
				if pid == 100 {
					return 4242, true
				}
				if pid == 4242 {
					return 1, true
				}
				return 0, false
			},
			ReadCmdline: func(pid int) string {
				if pid == 100 {
					return "pi"
				}
				if pid == 4242 {
					return "/bin/bash"
				}
				return ""
			},
			ArgvCache: &ArgvCache{},
		}
	}

	// ① file exists and contains --name role-reviewer-x -> subagent
	t.Run("FileExists_WithRoleReviewerFlag", func(t *testing.T) {
		path := writeArgvFile("role-reviewer.json", `["pi", "--name", "role-reviewer-x", "-p", "review"]`)
		env := makeEnvWithArgvFile(path, []string{"/root/bin/pi"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "launch marker: argv has --name role-") {
			t.Errorf("expected subagent with --name role- marker, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// ② file contains /tmp/herdr-role-sessions/... -> subagent
	t.Run("FileExists_WithHerdrRoleSessions", func(t *testing.T) {
		path := writeArgvFile("herdr-role.json", `["pi", "/tmp/herdr-role-sessions/sess-99", "--continue"]`)
		env := makeEnvWithArgvFile(path, []string{"/root/bin/pi"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "launch marker: argv has /tmp/herdr-role-sessions") {
			t.Errorf("expected subagent with /tmp/herdr-role-sessions, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// ③ file contains /tmp/dispatch-foo.md -> subagent
	t.Run("FileExists_WithDispatchTask", func(t *testing.T) {
		path := writeArgvFile("dispatch-task.json", `["pi", "-p", "run /tmp/dispatch-foo.md"]`)
		env := makeEnvWithArgvFile(path, []string{"/root/bin/pi"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "launch marker: argv has /tmp/dispatch-") {
			t.Errorf("expected subagent with /tmp/dispatch-, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// ④ missing file -> fails closed to subagent (M2)
	t.Run("MissingFile_FailClosed_Subagent", func(t *testing.T) {
		missingPath := filepath.Join(tmpDir, "non-existent-argv.json")
		env := makeEnvWithArgvFile(missingPath, []string{"/root/bin/pi"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "argv bridge failed") {
			t.Errorf("expected fail-closed subagent on missing file, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// ⑤ bad JSON -> fails closed to subagent (M2)
	t.Run("BadJSON_FailClosed_Subagent", func(t *testing.T) {
		badJSONPath := writeArgvFile("bad-argv.json", `{"this": "is not an array"}`)
		env := makeEnvWithArgvFile(badJSONPath, []string{"/root/bin/pi"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "argv bridge failed") {
			t.Errorf("expected fail-closed subagent on bad JSON, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// ⑥ env var completely unset -> safely falls back to os.Args without failing closed
	t.Run("UnsetEnv_FallsBackToArgs_MainAgent", func(t *testing.T) {
		env := makeEnvWithArgvFile("", []string{"/root/bin/pi"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if isSub {
			t.Errorf("expected main agent when PIG_HARNESS_ARGV_FILE is unset, got isSub=true, reason=%s", reason)
		}
	})

	// ⑦ only os.Args matches -> still effective
	t.Run("OnlyOsArgsMatches_StillEffective", func(t *testing.T) {
		cleanFilePath := writeArgvFile("clean-argv.json", `["pi", "-p", "hello"]`)
		env := makeEnvWithArgvFile(cleanFilePath, []string{"/root/bin/pi", "--name", "role-worker-1"})
		isSub, reason := ResolveAgentIdentityWithEnv(env)
		if !isSub || !strings.Contains(reason, "launch marker: argv has --name role-") {
			t.Errorf("expected subagent when only os.Args contains marker, got isSub=%v, reason=%s", isSub, reason)
		}
	})

	// ⑧ post-use cleanup -> PIG_HARNESS_ARGV_FILE is deleted from environment after reading
	t.Run("PostUseCleanup_UnsetsEnvVar", func(t *testing.T) {
		cleanFilePath := writeArgvFile("cleanup-argv.json", `["pi", "-p", "hello"]`)
		unsetCalled := false
		var unsetKey string
		mockUnsetenv := func(k string) error {
			unsetCalled = true
			unsetKey = k
			return nil
		}
		mockGetenv := func(k string) string {
			if k == "PIG_HARNESS_ARGV_FILE" {
				return cleanFilePath
			}
			return ""
		}
		args, err := LoadEffectiveArgs(mockGetenv, mockUnsetenv, []string{"base"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !unsetCalled || unsetKey != "PIG_HARNESS_ARGV_FILE" {
			t.Errorf("expected PIG_HARNESS_ARGV_FILE to be unset after reading, got unsetCalled=%v, key=%s", unsetCalled, unsetKey)
		}
		if len(args) != 4 || args[0] != "base" || args[1] != "pi" {
			t.Errorf("unexpected args: %v", args)
		}
	})

	// ⑨ consecutive calls in same process retain subagent identity across unsetenv
	t.Run("ArgvCache_ConsecutiveCalls_Subagent", func(t *testing.T) {
		path := writeArgvFile("consecutive-role.json", `["pi", "--name", "role-reviewer-consecutive", "-p", "review"]`)
		env := makeEnvWithArgvFile(path, []string{"/root/bin/pi"})

		// Call #1: initial call reads argv file, unsets env, and caches result
		isSub1, reason1 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#1: isSubagent=%v, reason=%s", isSub1, reason1)
		if !isSub1 || !strings.Contains(reason1, "launch marker: argv has --name role-") {
			t.Fatalf("call#1 expected subagent with role marker, got isSub=%v, reason=%s", isSub1, reason1)
		}

		// Verify PIG_HARNESS_ARGV_FILE was indeed unset by call #1
		if postUnsetVal := env.Getenv("PIG_HARNESS_ARGV_FILE"); postUnsetVal != "" {
			t.Fatalf("expected PIG_HARNESS_ARGV_FILE to be unset after call#1, got %q", postUnsetVal)
		}

		// Call #2: env var is now unset, must reuse cache and remain subagent
		isSub2, reason2 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#2: isSubagent=%v, reason=%s", isSub2, reason2)
		if !isSub2 || !strings.Contains(reason2, "launch marker: argv has --name role-") {
			t.Fatalf("call#2 expected subagent with cached role marker, got isSub=%v, reason=%s", isSub2, reason2)
		}
	})

	// ⑩ consecutive calls when env was never set legally fall back to os.Args without false positives
	t.Run("ArgvCache_ConsecutiveCalls_UnsetEnv_MainAgent", func(t *testing.T) {
		env := makeEnvWithArgvFile("", []string{"/root/bin/pi"})

		// Call #1: env unset, falls back to os.Args -> main agent
		isSub1, reason1 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#1 (unset): isSubagent=%v, reason=%s", isSub1, reason1)
		if isSub1 || !strings.Contains(reason1, "S1-S4 hold") {
			t.Fatalf("call#1 expected main agent (isSub=false), got isSub=%v, reason=%s", isSub1, reason1)
		}

		// Call #2: env still unset, cache not poisoned, still main agent
		isSub2, reason2 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#2 (unset): isSubagent=%v, reason=%s", isSub2, reason2)
		if isSub2 || !strings.Contains(reason2, "S1-S4 hold") {
			t.Fatalf("call#2 expected main agent (isSub=false), got isSub=%v, reason=%s", isSub2, reason2)
		}
	})

	// ⑪ a failed read must NOT be cached as "always subagent": after the failing call, a
	// good argv file is still honoured, and the failure is re-evaluated every call.
	t.Run("ArgvCache_FailureNotCached_ReEvaluated", func(t *testing.T) {
		env := makeEnvWithArgvFile("", []string{"/root/bin/pi"})

		// Call #1: env points at a missing file -> fail-closed subagent, failure not cached.
		env.Getenv = func(k string) string {
			if k == "PIG_HARNESS_ARGV_FILE" {
				return filepath.Join(tmpDir, "missing-on-purpose.json")
			}
			return ""
		}
		isSub1, reason1 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#1 (bad path): isSubagent=%v, reason=%s", isSub1, reason1)
		if !isSub1 || !strings.Contains(reason1, "argv bridge failed") {
			t.Fatalf("call#1 expected fail-closed subagent, got isSub=%v, reason=%s", isSub1, reason1)
		}

		// Call #2: env now points at a valid argv file carrying --name role- -> still subagent,
		// but via the launch marker (proves the previous failure was not frozen into the cache).
		goodPath := writeArgvFile("after-failure.json", `["pi", "--name", "role-after-failure", "-p", "x"]`)
		env.Getenv = func(k string) string {
			if k == "PIG_HARNESS_ARGV_FILE" {
				return goodPath
			}
			return ""
		}
		isSub2, reason2 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#2 (good path): isSubagent=%v, reason=%s", isSub2, reason2)
		if !isSub2 || !strings.Contains(reason2, "launch marker: argv has --name role-") {
			t.Fatalf("call#2 expected subagent via launch marker, got isSub=%v, reason=%s", isSub2, reason2)
		}

		// Call #3: env unset again -> cache must hold the successful result from call #2, so the
		// negative markers still apply after unsetenv.
		env.Getenv = func(k string) string { return "" }
		isSub3, reason3 := ResolveAgentIdentityWithEnv(env)
		t.Logf("call#3 (unset after success): isSubagent=%v, reason=%s", isSub3, reason3)
		if !isSub3 || !strings.Contains(reason3, "launch marker: argv has --name role-") {
			t.Fatalf("call#3 expected cached subagent, got isSub=%v, reason=%s", isSub3, reason3)
		}
	})
}

// 3. Tool Filtering Tests
func TestFilterActiveTools(t *testing.T) {
	input := []string{
		"read",
		"bash",
		"edit",
		"grep",
		"write",
		"find",
		"ls",
		"custom_tool",
		"jev_check",
		"ask_user_question",
	}

	t.Run("MainAgent_Excludes4Tools_PreservesOrder", func(t *testing.T) {
		filtered, changed := FilterActiveTools(input, false)
		if !changed {
			t.Errorf("expected changed=true")
		}
		expected := []string{"read", "edit", "write", "custom_tool", "jev_check", "ask_user_question"}
		if !reflect.DeepEqual(filtered, expected) {
			t.Errorf("unexpected filtered tools.\nExpected: %v\nGot:      %v", expected, filtered)
		}
	})

	t.Run("Subagent_Excludes6Tools_PreservesOrder", func(t *testing.T) {
		filtered, changed := FilterActiveTools(input, true)
		if !changed {
			t.Errorf("expected changed=true")
		}
		expected := []string{"read", "edit", "write", "custom_tool"}
		if !reflect.DeepEqual(filtered, expected) {
			t.Errorf("unexpected filtered tools for subagent.\nExpected: %v\nGot:      %v", expected, filtered)
		}
	})

	t.Run("NoChange_SetActiveToolsNotCalled", func(t *testing.T) {
		cleanList := []string{"read", "edit", "write", "custom_tool"}
		filteredMain, changedMain := FilterActiveTools(cleanList, false)
		if changedMain || !reflect.DeepEqual(filteredMain, cleanList) {
			t.Errorf("expected unchanged for main agent on clean list")
		}

		filteredSub, changedSub := FilterActiveTools(cleanList, true)
		if changedSub || !reflect.DeepEqual(filteredSub, cleanList) {
			t.Errorf("expected unchanged for subagent on clean list")
		}
	})

	t.Run("EmptyList", func(t *testing.T) {
		filtered, changed := FilterActiveTools([]string{}, false)
		if changed || len(filtered) != 0 {
			t.Errorf("expected empty unchanged result")
		}
	})
}

// 4. EnforceToolExclusions Setter Invocation Tests
func TestEnforceToolExclusions_SetterCalls(t *testing.T) {
	t.Run("InvokesSetterWhenChanged", func(t *testing.T) {
		mgr := &mockToolManager{
			activeTools: []string{"read", "bash", "edit", "grep"},
		}
		isSub, err := EnforceToolExclusions(mgr, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if isSub {
			t.Errorf("expected isSub=false")
		}
		if len(mgr.setCalls) != 1 {
			t.Fatalf("expected exactly 1 call to SetActiveTools, got %d", len(mgr.setCalls))
		}
		expected := []string{"read", "edit"}
		if !reflect.DeepEqual(mgr.setCalls[0], expected) {
			t.Errorf("expected set active tools %v, got %v", expected, mgr.setCalls[0])
		}
	})

	t.Run("NoChange_DoesNotInvokeSetter", func(t *testing.T) {
		mgr := &mockToolManager{
			activeTools: []string{"read", "edit", "write"},
		}
		_, err := EnforceToolExclusions(mgr, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mgr.setCalls) != 0 {
			t.Errorf("expected 0 calls to SetActiveTools when no change, got %d", len(mgr.setCalls))
		}
	})

	t.Run("PropagatesGetError_DoesNotPretendSuccess_LogsDiagnostic", func(t *testing.T) {
		origDebug := os.Getenv("PI_IDENTITY_DEBUG")
		os.Setenv("PI_IDENTITY_DEBUG", "1")
		defer os.Setenv("PI_IDENTITY_DEBUG", origDebug)

		oldStderr := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		os.Stderr = w

		expectedErr := errors.New("host disconnected")
		mgr := &mockToolManager{
			getErr: expectedErr,
		}
		_, callErr := EnforceToolExclusions(mgr, false)

		_ = w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		_ = r.Close()

		if callErr == nil || !errors.Is(callErr, expectedErr) {
			t.Errorf("expected error %v, got %v (must not silently succeed)", expectedErr, callErr)
		}
		if len(mgr.setCalls) != 0 {
			t.Errorf("expected 0 calls to SetActiveTools on error, got %d", len(mgr.setCalls))
		}
		stderrOut := buf.String()
		if !strings.Contains(stderrOut, "[subagent-identity] warning: GetActiveTools failed:") {
			t.Errorf("expected stderr diagnostic, got: %q", stderrOut)
		}
	})
}

// 5. System Prompt Sanitization Tests
func TestCleanPromptOptions(t *testing.T) {
	t.Run("StripsWithLeadingDoubleNewline", func(t *testing.T) {
		leaked := "DISPATCH SHARED RULES\nDO NOT TOUCH TS"
		forcePrompt := fmt.Sprintf("Base prompt header\n\n<addendum>\n%s\n</addendum>\nBase prompt footer", leaked)
		options := map[string]any{
			"appendSystemPrompt": leaked,
			"forceSystemPrompt":  forcePrompt,
		}

		modified := CleanPromptOptions(options)
		if !modified {
			t.Errorf("expected modified=true")
		}
		if options["appendSystemPrompt"] != "" {
			t.Errorf("expected appendSystemPrompt cleared, got %q", options["appendSystemPrompt"])
		}
		expectedForce := "Base prompt header\nBase prompt footer"
		if options["forceSystemPrompt"] != expectedForce {
			t.Errorf("unexpected forceSystemPrompt.\nExpected: %q\nGot:      %q", expectedForce, options["forceSystemPrompt"])
		}
	})

	t.Run("StripsWithoutLeadingDoubleNewline", func(t *testing.T) {
		leaked := "DISPATCH SHARED RULES"
		forcePrompt := fmt.Sprintf("<addendum>\n%s\n</addendum>\nBase prompt footer", leaked)
		options := map[string]any{
			"appendSystemPrompt": leaked,
			"forceSystemPrompt":  forcePrompt,
		}

		CleanPromptOptions(options)
		if options["appendSystemPrompt"] != "" {
			t.Errorf("expected appendSystemPrompt cleared")
		}
		expectedForce := "\nBase prompt footer"
		if options["forceSystemPrompt"] != expectedForce {
			t.Errorf("unexpected forceSystemPrompt.\nExpected: %q\nGot:      %q", expectedForce, options["forceSystemPrompt"])
		}
	})

	t.Run("MultipleAddendumBlocks_ReplaceAllSemantics", func(t *testing.T) {
		leaked := "SECRET RULES"
		block := fmt.Sprintf("<addendum>\n%s\n</addendum>", leaked)
		// Both blocks preceded by \n\n
		forcePrompt := fmt.Sprintf("Intro\n\n%s\nMiddle\n\n%s\nEnd", block, block)
		options := map[string]any{
			"appendSystemPrompt": leaked,
			"forceSystemPrompt":  forcePrompt,
		}

		CleanPromptOptions(options)
		expectedForce := "Intro\nMiddle\nEnd"
		if options["forceSystemPrompt"] != expectedForce {
			t.Errorf("unexpected forceSystemPrompt.\nExpected: %q\nGot:      %q", expectedForce, options["forceSystemPrompt"])
		}
	})

	t.Run("EmptyOrMissingAppendSystemPrompt", func(t *testing.T) {
		forcePrompt := "Untouched force prompt"
		options := map[string]any{
			"appendSystemPrompt": "",
			"forceSystemPrompt":  forcePrompt,
		}

		modified := CleanPromptOptions(options)
		if modified {
			t.Errorf("expected modified=false when appendSystemPrompt is empty")
		}
		if options["forceSystemPrompt"] != forcePrompt {
			t.Errorf("forceSystemPrompt should remain untouched")
		}
	})

	t.Run("NilOptionsMap_SafeNoPanic", func(t *testing.T) {
		modified := CleanPromptOptions(nil)
		if modified {
			t.Errorf("expected false on nil map")
		}
	})

	// 应修 3：addendum 也会以自定义 section 形式存在。RenderSystemPrompt 先用
	// appendSystemPrompt 构建 base sections，再用 sections 叠加（ApplyCustomSystemPromptSections
	// 对非空同名 section 是覆盖/追加），所以只清 appendSystemPrompt 会把 addendum 重新叠回来。
	t.Run("DeletesAddendumSection_KeepsOtherSections", func(t *testing.T) {
		sections := sdk.SystemPromptSections{
			{Name: "plan_mode", Value: "keep me"},
			{Name: "addendum", Value: "LEAKED DISPATCH RULES"},
		}
		options := map[string]any{
			"appendSystemPrompt": "",
			"sections":           &sections,
		}
		modified := CleanPromptOptions(options)
		if !modified {
			t.Errorf("expected modified=true when only the addendum section is removed")
		}
		got := *options["sections"].(*sdk.SystemPromptSections)
		want := sdk.SystemPromptSections{{Name: "plan_mode", Value: "keep me"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("unexpected sections.\nExpected: %#v\nGot:      %#v", want, got)
		}
	})

	t.Run("UnknownSectionsShape_LeftUntouched", func(t *testing.T) {
		sections := map[string]any{"addendum": "x"}
		options := map[string]any{"appendSystemPrompt": "", "sections": sections}
		if CleanPromptOptions(options) {
			t.Errorf("expected modified=false: no known section collection to edit")
		}
		if !reflect.DeepEqual(options["sections"], sections) {
			t.Errorf("unknown sections shape must not be mutated, got %#v", options["sections"])
		}
	})
}

// 6. Helpers & Ancillary Tests
func TestIsAgentRuntimeCmdline(t *testing.T) {
	cases := []struct {
		cmdline  string
		expected bool
	}{
		{"/usr/local/bin/pi -p hello", true},
		{"pi", true},
		{"/bin/ctxmode list", true},
		{"ctxmode", true},
		{"node /root/workspace/packages/pi-coding-agent/index.js", true},
		{"node /bundle/cli.js", true},
		{"/bin/bash", false},
		{"/usr/bin/zsh -i", false},
		{"python3 test.py", false},
		{"", false},
	}

	for _, tc := range cases {
		t.Run(tc.cmdline, func(t *testing.T) {
			actual := IsAgentRuntimeCmdline(tc.cmdline)
			if actual != tc.expected {
				t.Errorf("IsAgentRuntimeCmdline(%q) = %v, expected %v", tc.cmdline, actual, tc.expected)
			}
		})
	}
}

// 7. Excluded Tools Accessor Tests
func TestExcludedToolsAccessors(t *testing.T) {
	tools := ExcludedTools()
	for _, expected := range []string{"grep", "find", "ls", "bash"} {
		if !tools[expected] {
			t.Errorf("ExcludedTools missing %q: %v", expected, tools)
		}
	}
	// Verify copy semantics: mutating the returned map does not affect internal state
	tools["grep"] = false
	if !ExcludedTools()["grep"] {
		t.Errorf("ExcludedTools returned map is not a copy")
	}

	subTools := SubagentExcludedTools()
	for _, expected := range []string{"jev_check", "ask_user_question"} {
		if !subTools[expected] {
			t.Errorf("SubagentExcludedTools missing %q: %v", expected, subTools)
		}
	}
	subTools["jev_check"] = false
	if !SubagentExcludedTools()["jev_check"] {
		t.Errorf("SubagentExcludedTools returned map is not a copy")
	}
}

// 8. Extension Wiring Tests (Extension() registers session_start and before_agent_start)
func TestExtensionWiring(t *testing.T) {
	ext := Extension()
	if ext == nil {
		t.Fatalf("Extension() returned nil")
	}

	val := reflect.ValueOf(ext).Elem()
	handlersVal := val.FieldByName("handlers")
	if !handlersVal.IsValid() {
		t.Fatalf("handlers field not found on sdk.Extension")
	}

	var registeredEvents []string
	handlerMap := make(map[string]int)
	for i := 0; i < handlersVal.Len(); i++ {
		entry := handlersVal.Index(i)
		evName := entry.FieldByName("Event").String()
		hID := int(entry.FieldByName("HandlerID").Int())
		registeredEvents = append(registeredEvents, evName)
		handlerMap[evName] = hID
	}

	// Assert both events are registered
	expectedEvents := map[string]bool{
		sdk.EventSessionStart:     false,
		sdk.EventBeforeAgentStart: false,
	}
	for _, ev := range registeredEvents {
		if _, ok := expectedEvents[ev]; ok {
			expectedEvents[ev] = true
		}
	}
	for ev, found := range expectedEvents {
		if !found {
			t.Errorf("expected event %q to be registered, registered events: %v", ev, registeredEvents)
		}
	}

	// Assert before_agent_start mutates data["systemPromptOptions"] in-place for subagent sessions
	beforeAgentStartHandlerID, ok := handlerMap[sdk.EventBeforeAgentStart]
	if !ok {
		t.Fatalf("before_agent_start event not in handlerMap")
	}

	eventFuncsField := val.FieldByName("eventFuncs")
	eventFuncsPtr := (*map[int]sdk.EventFunc)(unsafe.Pointer(eventFuncsField.UnsafeAddr()))
	handler, ok := (*eventFuncsPtr)[beforeAgentStartHandlerID]
	if !ok || handler == nil {
		t.Fatalf("before_agent_start handler not found in eventFuncs")
	}

	// In test environment, S1-S4 fail closed as subagent
	leakedAppend := "LEAKED_DISPATCH_PROMPT"
	data := map[string]any{
		"systemPromptOptions": map[string]any{
			"appendSystemPrompt": leakedAppend,
			"forceSystemPrompt":  fmt.Sprintf("Base prompt\n\n<addendum>\n%s\n</addendum>", leakedAppend),
		},
	}

	// Prepare sdk.Context with ext pointer set (matching internal Context{ext: e})
	var ctx sdk.Context
	ctxVal := reflect.ValueOf(&ctx).Elem()
	*(*(*sdk.Extension))(unsafe.Pointer(ctxVal.FieldByName("ext").UnsafeAddr())) = ext

	// Invoke handler directly
	_, err := handler(ctx, data)
	if err != nil {
		t.Fatalf("handler returned unexpected error: %v", err)
	}

	options, ok := data["systemPromptOptions"].(map[string]any)
	if !ok {
		t.Fatalf("systemPromptOptions missing or not map[string]any")
	}
	if options["appendSystemPrompt"] != "" {
		t.Errorf("expected appendSystemPrompt to be cleared in-place, got %q", options["appendSystemPrompt"])
	}
	if options["forceSystemPrompt"] != "Base prompt" {
		t.Errorf("expected forceSystemPrompt addendum stripped in-place, got %q", options["forceSystemPrompt"])
	}
}

// 7. EnforceToolExclusions Error Reporting Test (SF-1: warn once on failure, no spam)
func TestEnforceToolExclusions_WarningOnceOnFailure(t *testing.T) {
	ResetEnforceWarnOnce()

	// Capture stderr
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stderr = w

	mockErr := errors.New("simulated tool failure")
	reportEnforceError("session_start", mockErr)
	reportEnforceError("before_agent_start", mockErr)
	reportEnforceError("before_agent_start", mockErr)

	_ = w.Close()
	os.Stderr = origStderr

	out, _ := io.ReadAll(r)
	_ = r.Close()

	outputStr := string(out)
	// Must contain warning for the first call
	if !strings.Contains(outputStr, "[subagent-identity] warning: failed to enforce tool exclusions on session_start: simulated tool failure") {
		t.Errorf("expected warning on first error, got: %q", outputStr)
	}
	// Must not contain subsequent warnings (deduplicated)
	count := strings.Count(outputStr, "warning: failed to enforce tool exclusions")
	if count != 1 {
		t.Errorf("expected exactly 1 warning line, got %d, output:\n%s", count, outputStr)
	}
}
