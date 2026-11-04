package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	jevgate "github.com/dorokuma/pi-extensions/go/jev-gate"
)

// TestVersionFlag verifies --version prints the injected/dev version and
// returns before entering the MCP stdio read loop.
func TestVersionFlag(t *testing.T) {
	if version == "" {
		t.Fatal("version must be non-empty (default \"dev\" or ldflags-injected)")
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	os.Stdout = w
	defer func() { os.Stdout = old }()

	oldArgs := os.Args
	os.Args = []string{"jev-mcp", "--version"}
	defer func() { os.Args = oldArgs }()

	main()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != version {
		t.Fatalf("--version output = %q, want %q", got, version)
	}
}

// stubIdentity swaps the resolver for one call chain and restores it after
// the test: tools/list and tools/call gate on it per request.
func stubIdentity(t *testing.T, isSubagent bool, reason string) {
	t.Helper()
	old := resolveAgentIdentity
	resolveAgentIdentity = func() (bool, string) { return isSubagent, reason }
	t.Cleanup(func() { resolveAgentIdentity = old })
}

// dispatchOne parses the single JSON-RPC response a message produces.
func dispatchOne(t *testing.T, message map[string]any) map[string]any {
	t.Helper()
	buffer := &strings.Builder{}
	writer := &responseWriter{out: buffer, current: "line"}
	handleRPC(writer, message)
	line := strings.TrimSpace(buffer.String())
	if line == "" {
		t.Fatal("no response written")
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("response %q is not JSON: %v", line, err)
	}
	return reply
}

// TestIdentityResolverIsSharedCore pins the wiring fix (SF-4): the request
// gate must call the shared jevgate S1–S4 resolver, not an inline variant.
func TestIdentityResolverIsSharedCore(t *testing.T) {
	t.Run("ctxmode_role_is_denied", func(t *testing.T) {
		t.Setenv("CTXMODE_ROLE", "worker")
		isSubagent, reason := jevgate.ResolveMCPAgentIdentity()
		if !isSubagent || !strings.Contains(reason, "CTXMODE_ROLE") {
			t.Fatalf("shared resolver must deny CTXMODE_ROLE: %v %q", isSubagent, reason)
		}
	})
	t.Run("non_p1_pane_is_denied_before_socket", func(t *testing.T) {
		t.Setenv("CTXMODE_ROLE", "")
		t.Setenv("PI_SUBAGENT", "")
		t.Setenv("PI_ROLE", "")
		t.Setenv("HERDR_PANE_ID", "w66:p8")
		isSubagent, reason := jevgate.ResolveMCPAgentIdentity()
		if !isSubagent || !strings.Contains(reason, `no ":p1" suffix`) {
			t.Fatalf("shared resolver must require :p1: %v %q", isSubagent, reason)
		}
	})
	t.Run("missing_pane_is_denied", func(t *testing.T) {
		t.Setenv("CTXMODE_ROLE", "")
		t.Setenv("PI_SUBAGENT", "")
		t.Setenv("PI_ROLE", "")
		t.Setenv("HERDR_PANE_ID", "")
		isSubagent, reason := jevgate.ResolveMCPAgentIdentity()
		if !isSubagent || !strings.HasPrefix(reason, "S1: HERDR_PANE_ID") {
			t.Fatalf("shared resolver must require HERDR_PANE_ID: %v %q", isSubagent, reason)
		}
	})
}

// TestMCPIdentityEnvChains covers the S3/S4 ancestry rules through the
// exported env-injected resolver (the full matrix lives in jevgate's
// identity_test.go; here we pin the ones the .mjs previously could not
// check at all).
func TestMCPIdentityEnvChains(t *testing.T) {
	fixture := func(chain []jevgate.AncestorProcess, shellPID int) jevgate.IdentityEnv {
		ppids := make(map[int]int, len(chain))
		cmdlines := make(map[int]string, len(chain))
		for _, entry := range chain {
			ppids[entry.PID] = entry.PPID
			cmdlines[entry.PID] = entry.Cmdline
		}
		return jevgate.IdentityEnv{
			Getenv:      func(string) string { return "" },
			PID:         chain[0].PID,
			SocketPath:  "/tmp/fake.sock",
			Timeout:     50 * time.Millisecond,
			ReadStat:    func(pid int) (int, bool) { ppid, ok := ppids[pid]; return ppid, ok },
			ReadCmdline: func(pid int) string { return cmdlines[pid] },
			Environ:     func() []string { return nil },
			QuerySocket: func(string, string, time.Duration) (int, bool) { return shellPID, shellPID > 0 },
			ArgvCache:   &jevgate.ArgvCache{},
		}
	}
	envWithPane := func(env jevgate.IdentityEnv) jevgate.IdentityEnv {
		env.Getenv = func(name string) string {
			if name == "HERDR_PANE_ID" {
				return "w1:p1"
			}
			return ""
		}
		return env
	}

	t.Run("agy_main_agent_allowed", func(t *testing.T) {
		chain := []jevgate.AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		isSubagent, reason := jevgate.ResolveMCPAgentIdentityWithEnv(envWithPane(fixture(chain, 300)))
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("forged_pane_id_denied_without_ancestor_match", func(t *testing.T) {
		chain := []jevgate.AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		// The socket answers a shell_pid that is NOT in this ancestry: the
		// forged pane id cannot satisfy S3.
		isSubagent, reason := jevgate.ResolveMCPAgentIdentityWithEnv(envWithPane(fixture(chain, 4242)))
		if !isSubagent || !strings.Contains(reason, "is not in this process ancestry") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("subagent_ancestor_denied", func(t *testing.T) {
		chain := []jevgate.AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy --agent worker"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		isSubagent, reason := jevgate.ResolveMCPAgentIdentityWithEnv(envWithPane(fixture(chain, 300)))
		if !isSubagent || !strings.Contains(reason, "S4:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
}

// TestToolsListGate covers the per-request identity gate on tools/list.
func TestToolsListGate(t *testing.T) {
	t.Run("main_agent_gets_jev_check", func(t *testing.T) {
		stubIdentity(t, false, "S1-S4 hold")
		reply := dispatchOne(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		result, _ := reply["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools = %v", result["tools"])
		}
		tool, _ := tools[0].(map[string]any)
		if tool["name"] != "jev_check" {
			t.Fatalf("tool = %v", tool)
		}
	})
	t.Run("subagent_gets_empty_list", func(t *testing.T) {
		stubIdentity(t, true, "S1: HERDR_PANE_ID unset or empty")
		reply := dispatchOne(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
		result, _ := reply["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		if len(tools) != 0 {
			t.Fatalf("subagent tools = %v", result["tools"])
		}
	})
}

// TestToolsCallGate covers the denial shape on tools/call.
func TestToolsCallGate(t *testing.T) {
	stubIdentity(t, true, "S4: nested second host runtime ancestor")
	reply := dispatchOne(t, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "jev_check", "arguments": map[string]any{"task": "gate_scan", "payload": "x"}},
	})
	result, _ := reply["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("denial must be an error result: %v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("denial has no content: %v", result)
	}
	text, _ := content[0].(map[string]any)
	deny, _ := text["text"].(string)
	if !strings.Contains(deny, "仅限主代理") || !strings.Contains(deny, "nested second host runtime") {
		t.Fatalf("deny text = %q", deny)
	}
}
