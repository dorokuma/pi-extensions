package jevgate

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// identityFixture builds an IdentityEnv over a synthetic process chain. chain[0]
// is this process; QuerySocket returns shellPID (0 means "query failed"). Argv
// and Env may be injected per hop; when Argv is empty resolveIdentity derives
// it from Cmdline via splitCmdlineTokens.
func identityFixture(env map[string]string, chain []AncestorProcess, shellPID int) IdentityEnv {
	values := make(map[string]string, len(env))
	for key, value := range env {
		values[key] = value
	}
	ppids := make(map[int]int, len(chain))
	cmdlines := make(map[int]string, len(chain))
	argvs := make(map[int][]string, len(chain))
	envs := make(map[int]map[string]string, len(chain))
	for _, entry := range chain {
		ppids[entry.PID] = entry.PPID
		cmdlines[entry.PID] = entry.Cmdline
		if entry.Argv != nil {
			argvs[entry.PID] = entry.Argv
		}
		if entry.Env != nil {
			envs[entry.PID] = entry.Env
		}
	}
	environ := make([]string, 0, len(values))
	for key, value := range values {
		environ = append(environ, key+"="+value)
	}
	return IdentityEnv{
		Getenv:      func(name string) string { return values[name] },
		PID:         chain[0].PID,
		SocketPath:  "/tmp/identity-fixture.sock",
		Timeout:     50 * time.Millisecond,
		ReadStat:    func(pid int) (int, bool) { ppid, ok := ppids[pid]; return ppid, ok },
		ReadCmdline: func(pid int) string { return cmdlines[pid] },
		ReadArgv:    func(pid int) []string { return argvs[pid] },
		ReadEnviron: func(pid int) map[string]string { return envs[pid] },
		Environ:     func() []string { return environ },
		QuerySocket: func(string, string, time.Duration) (int, bool) { return shellPID, shellPID > 0 },
		ArgvCache:   &ArgvCache{},
	}
}

// TestResolveAgentIdentityNegativeMarkers covers the launch markers.
func TestResolveAgentIdentityNegativeMarkers(t *testing.T) {
	chain := []AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"ctxmode_role_env", map[string]string{"CTXMODE_ROLE": "worker"}, nil, "launch marker: CTXMODE_ROLE=worker"},
		{"subagent_env", map[string]string{"PI_SUBAGENT": "1"}, nil, "launch marker: PI_SUBAGENT=1"},
		{"role_env", map[string]string{"PI_ROLE": "worker"}, nil, "launch marker: PI_ROLE=worker"},
		{"name_role", nil, []string{"pi", "--name", "role-worker"}, "launch marker: argv has --name role-"},
		{"name_role_eq", nil, []string{"pi", "--name=role-worker"}, "launch marker: argv has --name role-"},
		{"agent_flag", nil, []string{"pi", "--agent", "worker"}, "launch marker: argv has --agent"},
		{"agent_flag_eq", nil, []string{"pi", "--agent=worker"}, "launch marker: argv has --agent"},
		{"role_sessions", nil, []string{"pi", "/tmp/herdr-role-sessions/x.json"}, "launch marker: argv has /tmp/herdr-role-sessions"},
		{"dispatch", nil, []string{"pi", "/tmp/dispatch-abc"}, "launch marker: argv has /tmp/dispatch-"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := identityFixture(testCase.env, chain, 0)
			environment.Args = testCase.args
			isSubagent, reason := resolveAgentIdentityWithEnv(environment)
			if !isSubagent || !strings.Contains(reason, testCase.want) {
				t.Fatalf("isSubagent=%v reason=%q, want %q", isSubagent, reason, testCase.want)
			}
		})
	}
}

// TestPromptArgsNotMarkers pins the prompt-argument stripping: a main agent
// launched with -p/-i/--prompt whose prompt text merely mentions dispatch
// markers is not a subagent.
func TestPromptArgsNotMarkers(t *testing.T) {
	chain := []AncestorProcess{
		{PID: 100, PPID: 200, Cmdline: "/usr/local/bin/pi -p \"work on /tmp/dispatch-task.md\""},
		{PID: 200, PPID: 1, Cmdline: "/bin/bash"},
	}
	for _, args := range [][]string{
		{"pi", "-p", "work on /tmp/dispatch-task.md"},
		{"pi", "--prompt", "work on /tmp/dispatch-task.md"},
		{"pi", "--prompt=work on /tmp/dispatch-task.md"},
		{"pi", "-i", "please review /tmp/herdr-role-sessions/s1"},
		{"pi", "-p=work on /tmp/dispatch-task.md"},
	} {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 200)
		environment.Args = args
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent {
			t.Fatalf("args=%v resolved as subagent (%s); prompt text is not a marker", args, reason)
		}
	}
}

// TestResolveAgentIdentityProofChain walks S1/S2/S3/S4.
func TestResolveAgentIdentityProofChain(t *testing.T) {
	subprocess := []AncestorProcess{
		{PID: 100, PPID: 200, Cmdline: "/opt/ext/jev-gate"},
		{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi --print"},
		{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
	}
	t.Run("S1_missing_pane", func(t *testing.T) {
		environment := identityFixture(map[string]string{"PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S1: HERDR_PANE_ID") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S1_pane_without_p1_suffix_rejected", func(t *testing.T) {
		// The ":p1" suffix is a necessary condition of S1: a non-p1 pane id is
		// rejected before the herdr socket is even queried.
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w32:p23", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, `is not a main pane (no ":p1" suffix)`) {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S1_forged_p1_suffix_still_needs_S2", func(t *testing.T) {
		// HERDR_PANE_ID=forged:p1 passes the suffix check but fails the herdr
		// socket query (shellPID=0 here): the pane id alone is never enough.
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "forged:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 0)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S2:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S2_query_failed", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 0)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S2:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S3_not_in_ancestry", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 999)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "is not in this process ancestry") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S3_no_runtime_ancestor", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 300, Cmdline: "/opt/ext/jev-gate"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "no agent-runtime ancestor") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S3_empty_cmdline_fails_closed", func(t *testing.T) {
		// An intermediate ancestor with an unreadable (empty) cmdline must not
		// slip past the S4 scan: empty argv fails closed.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/opt/ext/jev-gate"},
			{PID: 200, PPID: 250, Cmdline: "/usr/local/bin/pi"},
			{PID: 250, PPID: 300, Cmdline: ""},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "empty or unreadable cmdline") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S4_intermediate_runtime", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/opt/ext/jev-gate"},
			{PID: 200, PPID: 250, Cmdline: "/usr/local/bin/pi"},
			{PID: 250, PPID: 300, Cmdline: "/usr/local/bin/ctxmode run"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S4: agent runtime ancestor pid=250") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S4_second_pi_below_host", func(t *testing.T) {
		// A second pi/pig runtime between hop 0 and the host is S4-rejected.
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "/opt/ext/jev-gate"},
			{PID: 150, PPID: 200, Cmdline: "/usr/local/bin/pig -p x"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S4: agent runtime ancestor pid=200") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S4_ancestor_env_marker", func(t *testing.T) {
		// CTXMODE_ROLE in an ancestor's own /proc environ is a negative marker.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/opt/ext/jev-gate"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi", Env: map[string]string{"CTXMODE_ROLE": "worker"}},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "S4: ancestor pid=200") || !strings.Contains(reason, "CTXMODE_ROLE") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S4_ancestor_cli_marker", func(t *testing.T) {
		// --name role-* on a non-host ancestor's argv is a negative marker.
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "/opt/ext/jev-gate"},
			{PID: 150, PPID: 200, Cmdline: "/bin/sh -c runner --name role-worker"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "S4: subagent CLI flag in ancestor pid=150") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("main_subprocess", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("packed_cell_member_is_subprocess_form", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET_JEV_GATE": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("fused_main", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/usr/local/bin/pig"},
			{PID: 200, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 200)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("fused_unrecognisable_self", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/usr/bin/mytool --serve"},
			{PID: 200, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 200)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "fused form but self pid=100") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
}

// TestResolveMCPAgentIdentity walks the MCP-mode S4 topology rules.
func TestResolveMCPAgentIdentity(t *testing.T) {
	t.Run("agy_main_agent_passes", func(t *testing.T) {
		// jev-mcp(100) -> agy(200) -> pane shell(300): one host layer, allowed.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/root/.local/bin/jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("node_wrapped_agy_passes", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "node /root/.gemini/antigravity-cli/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("pi_host_layer_passes", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("forged_pane_id_rejected_at_S2", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "forged:p1"}, chain, 0)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S2:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("shell_pid_not_in_ancestry_rejected_at_S3", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "forged:p1"}, chain, 999)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "is not in this process ancestry") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("parent_agent_flag_rejected_at_S4", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy --agent worker"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "S4: subagent CLI flag in ancestor pid=200") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("parent_ctxmode_role_rejected_at_S4", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy", Env: map[string]string{"CTXMODE_ROLE": "worker"}},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "CTXMODE_ROLE") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("nested_second_agy_rejected_at_S4", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "jev-mcp"},
			{PID: 150, PPID: 200, Cmdline: "/usr/local/bin/agy --agent worker"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "S4: subagent CLI flag") && !strings.Contains(reason, "nested second host") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("pi_calling_agy_is_two_layers", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "jev-mcp"},
			{PID: 150, PPID: 200, Cmdline: "/usr/local/bin/pi"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "nested second host runtime") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("forbidden_ctxmode_ancestor_rejected", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "jev-mcp"},
			{PID: 150, PPID: 200, Cmdline: "/usr/local/bin/ctxmode run"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "agent/tool runtime ancestor pid=150") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("unknown_intermediate_shell_rejected", func(t *testing.T) {
		// An extra `bash -c` hop between jev-mcp and agy is not the host and
		// not a transparent interpreter wrapper: fail closed.
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "jev-mcp"},
			{PID: 150, PPID: 200, Cmdline: "/bin/bash -c /root/.local/bin/jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "unexpected intermediate ancestor pid=150") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("bash_c_inline_agy_not_host", func(t *testing.T) {
		// `bash -c "agy"` runs an inline command string, not the agy script:
		// the interpreter flag table must stop the host scan at -c so the
		// hop is rejected as an unexpected intermediate, not counted as the
		// agy host layer.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/bin/bash -c agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "unexpected intermediate ancestor pid=200") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("bash_c_inline_pi_not_host", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/bin/bash -c pi"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "unexpected intermediate ancestor pid=200") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("empty_cmdline_ancestor_fails_closed", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 150, Cmdline: "jev-mcp"},
			{PID: 150, PPID: 300, Cmdline: ""},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "empty or unreadable cmdline") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("prompt_with_dispatch_path_not_marker", func(t *testing.T) {
		// agy -i "work on /tmp/dispatch-x.md" is a main-agent prompt, not a
		// launch marker.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy -i \"work on /tmp/dispatch-x.md\""},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("env_wrapper_penetrated", func(t *testing.T) {
		// env -u FOO /usr/local/bin/agy still resolves to the agy host.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "env -u FOO /usr/local/bin/agy"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("env_wrapped_ctxmode_rejected", func(t *testing.T) {
		// env FOO=1 /usr/local/bin/ctxmode run penetrates to ctxmode: forbidden.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "env FOO=1 /usr/local/bin/ctxmode run"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "agent/tool runtime ancestor pid=200") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("literal_argv_beats_joined_cmdline", func(t *testing.T) {
		// An injected Argv is used verbatim (token boundaries); a marker hidden
		// inside one token's quoting of the joined cmdline cannot smuggle a
		// split token that a space-joined scan would read as two tokens.
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "jev-mcp"},
			{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy --name role-x",
				Argv: []string{"/usr/local/bin/agy", "--name role-x"}},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
		isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
		if isSubagent {
			t.Fatalf("single-token '--name role-x' is not the marker: isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
}

// TestEnvArgv0Masking pins F-1: GNU env's -a/--argv0 consumes one argument
// token (the fake argv0), so `env -a mask-name <forbidden>` must still resolve
// to the wrapped executable. Treating -a as a boolean flag let the mask name
// pass off as the executable and smuggled ctxmode past the S4 scan.
func TestEnvArgv0Masking(t *testing.T) {
	chains := map[string]string{
		"env_dash_a":       "env -a mask-name /usr/local/bin/ctxmode",
		"env_argv0_long":   "env --argv0 mask-name /usr/local/bin/ctxmode",
		"env_argv0_inline": "env --argv0=mask-name /usr/local/bin/ctxmode",
		// GNU long-option abbreviations and short-option clusters resolve to
		// the same --argv0 argument consumption (verified on this host: all
		// three forms of `env … mask-name cmd` exec cmd).
		"env_argv_abbrev": "env --argv mask-name /usr/local/bin/ctxmode",
		"env_arg_abbrev":  "env --arg mask-name /usr/local/bin/ctxmode",
		"env_cluster_ia":  "env -ia mask-name /usr/local/bin/ctxmode",
		"env_cluster_iam": "env -iamask-name /usr/local/bin/ctxmode",
		"env_split_opts":  `env -S "-a mask-name /usr/local/bin/ctxmode"`,
		// GNU -S quoting: the quoted mask is ONE argv0 argument, so the
		// wrapped ctxmode is still what env execs (verified: env --debug
		// splits `-a 'mask ctxmode' …` into -a + the single mask token).
		"env_split_opts_quoted_mask":     `env -S "-a 'mask-name ctxmode extra' /usr/local/bin/ctxmode"`,
		"env_split_opts_dq_mask":         `env -S '-a "mask name" /usr/local/bin/ctxmode'`,
		"env_split_opts_adjacent_quotes": `env -S "-a 'ma''sk' /usr/local/bin/ctxmode"`,
	}
	t.Run("extension_mode", func(t *testing.T) {
		for name, wrapper := range chains {
			t.Run(name, func(t *testing.T) {
				// [ext] -> [env -a mask-name ctxmode] -> [pi host] -> [pane shell]:
				// the env-wrapped ctxmode hop between host and shell is S4.
				chain := []AncestorProcess{
					{PID: 100, PPID: 150, Cmdline: "/opt/ext/jev-gate"},
					{PID: 150, PPID: 200, Cmdline: wrapper},
					{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi"},
					{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
				}
				environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
				isSubagent, reason := resolveAgentIdentityWithEnv(environment)
				if !isSubagent || !strings.Contains(reason, "S4") {
					t.Fatalf("isSubagent=%v reason=%q, want S4 rejection", isSubagent, reason)
				}
			})
		}
	})
	t.Run("mcp_mode", func(t *testing.T) {
		for name, wrapper := range chains {
			t.Run(name, func(t *testing.T) {
				chain := []AncestorProcess{
					{PID: 100, PPID: 150, Cmdline: "jev-mcp"},
					{PID: 150, PPID: 200, Cmdline: wrapper},
					{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/agy"},
					{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
				}
				environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 300)
				isSubagent, reason := ResolveMCPAgentIdentityWithEnv(environment)
				if !isSubagent || !strings.Contains(reason, "S4") {
					t.Fatalf("isSubagent=%v reason=%q, want S4 rejection", isSubagent, reason)
				}
			})
		}
	})
}

// TestEnvCommandTokens covers the env hop's option parsing against the real
// GNU coreutils 9.7 option table (verified on this host): required-argument
// options and their abbreviations consume the next token, a missing trailing
// argument yields no executable (never a panic), ambiguous or unknown options
// yield no executable without touching the next token, and -S re-parses its
// split value through the option phase.
func TestEnvCommandTokens(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		want   string // executable token env would exec; "" = no executable
		forbid bool   // whether the residual resolves to a forbidden runtime
	}{
		// argv0 mask forms: every one consumes the mask, never the command.
		{"argv0", []string{"env", "--argv0", "mask", "cmd"}, "cmd", false},
		{"argv0_inline", []string{"env", "--argv0=mask", "cmd"}, "cmd", false},
		{"argv_abbrev", []string{"env", "--argv", "mask", "cmd"}, "cmd", false},
		{"arg_abbrev", []string{"env", "--arg", "mask", "cmd"}, "cmd", false},
		{"a_short", []string{"env", "-a", "mask", "cmd"}, "cmd", false},
		{"cluster_ia", []string{"env", "-ia", "mask", "cmd"}, "cmd", false},
		{"cluster_iam", []string{"env", "-iamask", "cmd"}, "cmd", false},
		{"argv0_consumes_flag_token", []string{"env", "--argv0", "-i", "cmd"}, "cmd", false},
		// Other required-argument options.
		{"unset", []string{"env", "-u", "FOO", "cmd"}, "cmd", false},
		{"unset_abbrev", []string{"env", "--un", "FOO", "cmd"}, "cmd", false},
		{"unset_inline", []string{"env", "--unset=FOO", "cmd"}, "cmd", false},
		{"chdir", []string{"env", "-C", "/tmp", "cmd"}, "cmd", false},
		{"cluster_u", []string{"env", "-uFOO", "cmd"}, "cmd", false},
		// Trailing missing arguments: env exits(125) — no executable, no panic.
		{"trailing_a", []string{"env", "-a"}, "", false},
		{"trailing_argv0", []string{"env", "--argv0"}, "", false},
		{"trailing_u", []string{"env", "-u"}, "", false},
		{"trailing_unset", []string{"env", "--unset"}, "", false},
		{"trailing_chdir", []string{"env", "-C"}, "", false},
		{"trailing_split", []string{"env", "-S"}, "", false},
		{"trailing_cluster_ia", []string{"env", "-ia"}, "", false},
		// Unknown / ambiguous / malformed options: env exits(125), the next
		// token is neither an argument nor the executable.
		{"unknown_long", []string{"env", "--zzz", "x", "cmd"}, "", false},
		{"ambiguous_d", []string{"env", "--d", "x", "cmd"}, "", false},
		{"ambiguous_ig", []string{"env", "--ig", "x", "cmd"}, "", false},
		{"unknown_short", []string{"env", "-Z", "x", "cmd"}, "", false},
		{"no_arg_option_with_value", []string{"env", "--debug=x", "cmd"}, "", false},
		{"terminal_help", []string{"env", "--help", "cmd"}, "", false},
		{"terminal_version_abbrev", []string{"env", "--vers", "cmd"}, "", false},
		{"null_with_command", []string{"env", "-0", "cmd"}, "", false},
		{"null_alone", []string{"env", "-0"}, "", false},
		// Optional-argument options take their value only via `=`.
		{"block_signal_optional", []string{"env", "--block-signal", "cmd"}, "cmd", false},
		{"block_signal_inline", []string{"env", "--block-signal=PIPE", "cmd"}, "cmd", false},
		{"block_signal_abbrev", []string{"env", "--b=PIPE", "cmd"}, "cmd", false},
		// Operand phase: no permutation, `-` implies -i, `--` ends options.
		{"assign_before_option", []string{"env", "FOO=1", "-a", "mask", "cmd"}, "-a", false},
		{"ddash_assign", []string{"env", "--", "FOO=1", "cmd"}, "cmd", false},
		{"ddash_command", []string{"env", "--", "cmd"}, "cmd", false},
		{"lone_dash", []string{"env", "-", "FOO=1", "cmd"}, "cmd", false},
		{"assigns_only", []string{"env", "FOO=1", "BAR=2"}, "", false},
		// -S re-splits and re-parses through the option phase.
		{"split_command", []string{"env", "-S", "cmd arg1"}, "cmd", false},
		{"split_with_options", []string{"env", "-S", "-a mask cmd"}, "cmd", false},
		{"split_inline", []string{"env", "-Scmd arg"}, "cmd", false},
		{"split_long_abbrev", []string{"env", "--spl=cmd arg"}, "cmd", false},
		{"split_rest_appended", []string{"env", "-S", "cmd", "rest"}, "cmd", false},
		// GNU -S quote/escape rules (coreutils 9.7 build_argv, verified with
		// env --debug): quotes group ONE argument and may abut; whitespace
		// inside quotes never splits; `\_` separates outside double quotes;
		// `\c` truncates; `#` starts a comment only at an argument boundary
		// (inside quotes it is literal); `$` inside single quotes is literal
		// while a bare unquoted `$x` is an env error; unclosed quotes and
		// invalid escapes are env errors — no executable.
		{"split_sq_mask_multiword", []string{"env", "-S", "-a 'mask ctxmode extra' /usr/bin/true"}, "/usr/bin/true", false},
		{"split_sq_mask", []string{"env", "-S", "-a 'mask' /usr/bin/true"}, "/usr/bin/true", false},
		{"split_dq_mask", []string{"env", "-S", `-a "m x" /usr/bin/true`}, "/usr/bin/true", false},
		{"split_adjacent_quotes", []string{"env", "-S", "-a 'ma''sk' /usr/bin/true"}, "/usr/bin/true", false},
		{"split_mixed_quotes", []string{"env", "-S", "-a 'm'\"x\"'y' /usr/bin/true"}, "/usr/bin/true", false},
		{"split_empty_quoted_cmd", []string{"env", "-S", "'' /usr/bin/true"}, "", false},
		{"split_quoted_space_cmd", []string{"env", "-S", "'x z' /usr/bin/true"}, "x z", false},
		{"split_escaped_space_sep", []string{"env", "-S", "a\\_b /usr/bin/true"}, "a", false},
		{"split_escaped_space_in_dq", []string{"env", "-S", "-a \"m\\_x\" /usr/bin/true"}, "/usr/bin/true", false},
		{"split_backslash_literal", []string{"env", "-S", "-a x\\\\y /usr/bin/true"}, "/usr/bin/true", false},
		{"split_escape_tab", []string{"env", "-S", "a\\tb /usr/bin/true"}, "a\tb", false},
		{"split_escaped_hash", []string{"env", "-S", "a\\#b /usr/bin/true"}, "a#b", false},
		// `#` at an argument boundary opens a comment that swallows the rest
		// of the -S string: the string contributes no pieces, so the
		// executable falls through to the next argv token `cmd`. A literal-#
		// implementation would exec `#x` instead of `cmd`.
		{"split_comment_word_boundary", []string{"env", "-S", "#x y", "cmd"}, "cmd", false},
		{"split_comment_mid_word_literal", []string{"env", "-S", "a#b c /usr/bin/true"}, "a#b", false},
		// `#` inside quotes is literal: the quoted text IS the executable.
		// An always-comment implementation would see empty args → no
		// executable; a boundary-only implementation yields `#x`.
		{"split_hash_inside_quotes", []string{"env", "-S", "'#x' trailing"}, "#x", false},
		{"split_backslash_c_truncates", []string{"env", "-S", "a\\cb /usr/bin/true"}, "a", false},
		{"split_sq_backslash_literal", []string{"env", "-S", "'a\\qb' /usr/bin/true"}, "a\\qb", false},
		{"split_sq_escaped_quote", []string{"env", "-S", "-a 'm\\'x' /usr/bin/true"}, "/usr/bin/true", false},
		{"split_dq_escaped_quote", []string{"env", "-S", `-a "m\"x" /usr/bin/true`}, "/usr/bin/true", false},
		{"split_var_opaque_arg", []string{"env", "-S", "-a ${MASK} /usr/bin/true"}, "/usr/bin/true", false},
		// `$` inside single quotes is literal text, not expansion: the quoted
		// token is the executable verbatim. Contrasts split_dollar_bad_form
		// (unquoted `$x` → env error → no executable).
		{"split_var_sq_literal", []string{"env", "-S", "'$x' trailing"}, "$x", false},
		{"split_dollar_bad_form", []string{"env", "-S", "a $x /usr/bin/true"}, "", false},
		// `${MASK` has no closing brace: GNU env errors out on the split
		// string itself (verified: env(9.7) exits 125, "只支持 ${变量名}
		// 扩展"), so the appended `cmd` operand never runs and the want is
		// "no executable". A wrong implementation that accepts `${MASK` as
		// literal text instead lets -a consume it as the argv0 mask and
		// would yield executable `cmd` — the trailing operand is what makes
		// the two outcomes diverge and this case fail instead of passing
		// vacuously.
		{"split_dollar_bad_brace", []string{"env", "-S", "-a ${MASK", "cmd"}, "", false},
		{"split_unclosed_sq", []string{"env", "-S", "-a 'unclosed /usr/bin/true"}, "", false},
		{"split_unclosed_dq", []string{"env", "-S", `-a "unclosed /usr/bin/true`}, "", false},
		{"split_invalid_escape", []string{"env", "-S", "-a a\\ b /usr/bin/true"}, "", false},
		{"split_invalid_escape_q", []string{"env", "-S", "a\\qb /usr/bin/true"}, "", false},
		{"split_trailing_backslash", []string{"env", "-S", "a\\"}, "", false},
		// `\$` outside quotes is an escaped literal dollar (the GNU "#,$,'
		// literal" escape class), so the escaped token is the executable.
		// An implementation that errors on `\$` or expands `$x` yields no
		// executable instead of `$x`.
		{"split_escaped_dollar_literal", []string{"env", "-S", "\\$x trailing"}, "$x", false},
		// Forbidden-runtime residuals through wrappers.
		{"argv0_mask_ctxmode", []string{"env", "--argv0", "mask", "/usr/local/bin/ctxmode"}, "/usr/local/bin/ctxmode", true},
		{"argv_abbrev_ctxmode", []string{"env", "--argv", "mask", "/usr/local/bin/ctxmode"}, "/usr/local/bin/ctxmode", true},
		{"arg_abbrev_ctxmode", []string{"env", "--arg", "mask", "/usr/local/bin/ctxmode"}, "/usr/local/bin/ctxmode", true},
		{"cluster_ia_ctxmode", []string{"env", "-ia", "mask", "/usr/local/bin/ctxmode"}, "/usr/local/bin/ctxmode", true},
		{"split_opts_ctxmode", []string{"env", "-S", "-a mask /usr/local/bin/ctxmode"}, "/usr/local/bin/ctxmode", true},
		{"assign_ctxmode", []string{"env", "FOO=1", "/usr/local/bin/ctxmode"}, "/usr/local/bin/ctxmode", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := executableTokenOf(testCase.args, 0); got != testCase.want {
				t.Fatalf("executableTokenOf(%v) = %q, want %q", testCase.args, got, testCase.want)
			}
			if got := checkExecutableTokens(testCase.args, 0); got != testCase.forbid {
				t.Fatalf("checkExecutableTokens(%v) = %v, want %v", testCase.args, got, testCase.forbid)
			}
		})
	}
}

// TestPromptArgStrippingFlagBoundary pins the prompt-consumption rule of
// PiG (ref/PiG/coding/cli/args.go:258-270): -p/--print swallows the next
// token only when it is not an @file and not flag-shaped (except
// "---"-led text). Marker flags right after -p stay visible to the scan;
// "---"-led prompt text is prompt, not a flag.
func TestPromptArgStrippingFlagBoundary(t *testing.T) {
	marked := [][]string{
		{"agy", "-p", "--name", "role-x"},
		{"agy", "-p", "--agent", "worker"},
		{"prog", "-p", "x", "--agent", "worker"},
		// An @file right after -p is PiG's file-inclusion form, not prompt
		// text: it stays visible, and a marker-looking token is a marker.
		{"pi", "-p", "@/tmp/dispatch-task.md"},
		{"pi", "--print", "@/tmp/dispatch-task.md"},
	}
	for _, argv := range marked {
		if !HasSubagentCliFlags(argv) {
			t.Fatalf("argv=%v must stay a launch marker: -p may not swallow a structural token", argv)
		}
	}
	// Prompt text that is legitimately consumed — plain text and "---"-led
	// text — must never be scanned for markers.
	clean := [][]string{
		{"pi", "-p", "contains --name role-x in prompt text"},
		{"pi", "-p", "--- review /tmp/dispatch-x.md"},
		{"pi", "--print", "text /tmp/dispatch-task.md"},
	}
	for _, argv := range clean {
		if HasSubagentCliFlags(argv) {
			t.Fatalf("argv=%v prompt text must be stripped, not scanned", argv)
		}
	}
}

// TestResolveAgentIdentityUnreadableAncestry pins the unreadable /proc branch.
func TestResolveAgentIdentityUnreadableAncestry(t *testing.T) {
	environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, []AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 300)
	environment.ReadStat = func(int) (int, bool) { return 0, false }
	isSubagent, reason := resolveAgentIdentityWithEnv(environment)
	if !isSubagent || !strings.Contains(reason, "ancestry for this process is unreadable") {
		t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
	}
}

// TestDetectSubprocessForm covers every transport variable.
func TestDetectSubprocessForm(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		want  bool
		proof string
	}{
		{"isolated", map[string]string{"PIG_EXT_SOCKET": "/tmp/s"}, true, "PIG_EXT_SOCKET"},
		{"packed_cell", map[string]string{"PIG_EXT_PACKED_CELL": "cell"}, true, "PIG_EXT_PACKED_CELL"},
		{"active_members", map[string]string{"PIG_EXT_ACTIVE_MEMBERS": "a,b"}, true, "PIG_EXT_ACTIVE_MEMBERS"},
		{"member_socket", map[string]string{"PIG_EXT_SOCKET_JEV_GATE": "/tmp/s"}, true, "PIG_EXT_SOCKET_JEV_GATE"},
		{"empty_member_socket_is_not_proof", map[string]string{"PIG_EXT_SOCKET_JEV_GATE": ""}, false, ""},
		{"fused", nil, false, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environ := []string{}
			for key, value := range testCase.env {
				environ = append(environ, key+"="+value)
			}
			getenv := func(name string) string { return testCase.env[name] }
			got, proof := detectSubprocessForm(getenv, func() []string { return environ })
			if got != testCase.want || proof != testCase.proof {
				t.Fatalf("detectSubprocessForm = %v,%q want %v,%q", got, proof, testCase.want, testCase.proof)
			}
		})
	}
}

// TestLoadEffectiveArgs covers the harness argv bridge.
func TestLoadEffectiveArgs(t *testing.T) {
	t.Run("unset_falls_back_to_os_args", func(t *testing.T) {
		args, err := loadEffectiveArgs(func(string) string { return "" }, []string{"pig"}, &ArgvCache{})
		if err != nil || strings.Join(args, " ") != "pig" {
			t.Fatalf("args=%v err=%v", args, err)
		}
	})
	t.Run("file_is_merged_cached_and_kept_readable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		getenv := func(name string) string { return values[name] }
		cache := &ArgvCache{}
		args, err := loadEffectiveArgs(getenv, []string{"pig"}, cache)
		if err != nil {
			t.Fatalf("loadEffectiveArgs: %v", err)
		}
		if strings.Join(args, " ") != "pig --name role-worker" {
			t.Fatalf("args = %v", args)
		}
		if values["PIG_HARNESS_ARGV_FILE"] != path {
			t.Fatal("PIG_HARNESS_ARGV_FILE must stay set: packed cell siblings read it too")
		}
		again, err := loadEffectiveArgs(getenv, []string{"pig"}, cache)
		if err != nil || strings.Join(again, " ") != "pig --name role-worker" {
			t.Fatalf("cached args=%v err=%v", again, err)
		}
	})
	t.Run("argv_file_is_only_a_negative_marker_source", func(t *testing.T) {
		// The file can push the verdict toward subagent; it can never elevate a
		// session: a marker-free argv still has to pass S1–S4 on its own.
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["pi"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		environment := identityFixture(map[string]string{"PIG_HARNESS_ARGV_FILE": path},
			[]AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 100)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S1: HERDR_PANE_ID") {
			t.Fatalf("clean argv cannot elevate to main: isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("cached_argv_is_authoritative", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		getenv := func(name string) string { return values[name] }
		cache := &ArgvCache{}
		if _, err := loadEffectiveArgs(getenv, []string{"pig"}, cache); err != nil {
			t.Fatalf("first load: %v", err)
		}
		// A caller that finds the variable cleared or re-pointed (re-exported,
		// or pointing at an unreadable path) must get the argv cached by the
		// first load: the load is one process-wide critical section, not a
		// per-caller file read.
		values["PIG_HARNESS_ARGV_FILE"] = filepath.Join(t.TempDir(), "absent.json")
		args, err := loadEffectiveArgs(getenv, []string{"pig"}, cache)
		if err != nil || strings.Join(args, " ") != "pig --name role-worker" {
			t.Fatalf("cached args=%v err=%v", args, err)
		}
	})
	t.Run("missing_file_fails_closed", func(t *testing.T) {
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": filepath.Join(t.TempDir(), "absent.json")}
		_, err := loadEffectiveArgs(func(name string) string { return values[name] }, nil, &ArgvCache{})
		if err == nil {
			t.Fatal("a missing argv file must fail closed")
		}
	})
	t.Run("invalid_json_fails_closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`{"not":"an array"}`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		_, err := loadEffectiveArgs(func(name string) string { return values[name] }, nil, &ArgvCache{})
		if err == nil {
			t.Fatal("invalid argv JSON must fail closed")
		}
	})
	t.Run("bridge_failure_resolves_as_subagent", func(t *testing.T) {
		environment := identityFixture(map[string]string{"PIG_HARNESS_ARGV_FILE": filepath.Join(t.TempDir(), "absent.json")},
			[]AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 100)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "argv bridge failed:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("bridge_marker_resolves_as_subagent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["--name","role-verifier"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		environment := identityFixture(map[string]string{"PIG_HARNESS_ARGV_FILE": path},
			[]AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 100)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "launch marker: argv has --name role-") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
}

// TestLoadEffectiveArgsConcurrent pins the process-wide critical section of the
// argv bridge: every concurrent caller must see the bridged argv, and the
// environment variable stays set for packed cell siblings (Run with -race).
func TestLoadEffectiveArgsConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argv.json")
	if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
		t.Fatalf("write argv file: %v", err)
	}
	const (
		goroutines = 8
		rounds     = 200
	)
	for round := 0; round < rounds; round++ {
		var envMu sync.Mutex
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		getenv := func(name string) string {
			envMu.Lock()
			defer envMu.Unlock()
			return values[name]
		}
		cache := &ArgvCache{}
		results := make([][]string, goroutines)
		errs := make([]error, goroutines)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(goroutines)
		for index := 0; index < goroutines; index++ {
			go func(index int) {
				defer done.Done()
				<-start
				results[index], errs[index] = loadEffectiveArgs(getenv, []string{"pig"}, cache)
			}(index)
		}
		close(start)
		done.Wait()
		for index, args := range results {
			if errs[index] != nil {
				t.Fatalf("round %d caller %d: %v", round, index, errs[index])
			}
			if joined := strings.Join(args, " "); joined != "pig --name role-worker" {
				t.Fatalf("round %d caller %d args = %q, want the bridged argv \"pig --name role-worker\"", round, index, joined)
			}
		}
		if values["PIG_HARNESS_ARGV_FILE"] != path {
			t.Fatalf("round %d: PIG_HARNESS_ARGV_FILE must stay readable for packed cell siblings", round)
		}
	}
}

// paneSocket starts a unix socket server that replies with reply for every
// connection and returns its path.
func paneSocket(t *testing.T, reply string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			buffer := make([]byte, 4096)
			_, _ = conn.Read(buffer)
			if reply != "" {
				_, _ = conn.Write([]byte(reply))
			}
			_ = conn.Close()
		}
	}()
	return path
}

// TestQueryPaneShellPid covers the socket protocol branches.
func TestQueryPaneShellPid(t *testing.T) {
	ok := `{"id":"pi-identity","result":{"process_info":{"pane_id":"w1:p1","shell_pid":4321}}}`
	cases := []struct {
		name    string
		reply   string
		paneID  string
		wantPID int
		wantOK  bool
	}{
		{"ok", ok, "w1:p1", 4321, true},
		{"pane_mismatch", ok, "w1:p2", 0, false},
		{"wrong_request_id", `{"id":"other","result":{"process_info":{"pane_id":"w1:p1","shell_pid":7}}}`, "w1:p1", 0, false},
		{"zero_pid", `{"id":"pi-identity","result":{"process_info":{"pane_id":"w1:p1","shell_pid":0}}}`, "w1:p1", 0, false},
		{"fractional_pid", `{"id":"pi-identity","result":{"process_info":{"pane_id":"w1:p1","shell_pid":4.5}}}`, "w1:p1", 0, false},
		{"bad_json", `not json`, "w1:p1", 0, false},
		{"empty_reply", "", "w1:p1", 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := paneSocket(t, testCase.reply)
			pid, ok := queryPaneShellPid(path, testCase.paneID, time.Second)
			if ok != testCase.wantOK || pid != testCase.wantPID {
				t.Fatalf("queryPaneShellPid = %d,%v want %d,%v", pid, ok, testCase.wantPID, testCase.wantOK)
			}
		})
	}

	t.Run("no_socket", func(t *testing.T) {
		if _, ok := queryPaneShellPid(filepath.Join(t.TempDir(), "absent.sock"), "w1:p1", 50*time.Millisecond); ok {
			t.Fatal("a missing socket must fail closed")
		}
	})
}

// TestQueryPaneShellPidTimeout pins the bounded query budget.
func TestQueryPaneShellPidTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		time.Sleep(2 * time.Second)
		_ = conn.Close()
	}()
	start := time.Now()
	if _, ok := queryPaneShellPid(path, "w1:p1", 40*time.Millisecond); ok {
		t.Fatal("a silent server must fail closed")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("query took %s, the budget must bound it", elapsed)
	}
}

// TestReadProcSelf exercises the real /proc readers on this process.
func TestReadProcSelf(t *testing.T) {
	pid := os.Getpid()
	ppid, ok := readProcPpid(pid)
	if !ok || ppid <= 0 {
		t.Fatalf("readProcPpid(%d) = %d,%v", pid, ppid, ok)
	}
	if argv := readProcArgv(pid); len(argv) == 0 {
		t.Fatal("readProcArgv returned nothing for this process")
	}
	if cmdline := readProcCmdline(pid); cmdline == "" {
		t.Fatal("readProcCmdline returned nothing for this process")
	}
	chain := readAncestorChain(pid, identityAncestorMaxHops, readProcPpid, readProcCmdline, readProcArgv, readProcEnviron)
	if len(chain) == 0 || chain[0].PID != pid {
		t.Fatalf("chain = %+v", chain)
	}
	if len(chain[0].Argv) == 0 {
		t.Fatal("ancestor argv must be populated for this process")
	}
	if _, ok := readProcPpid(-1); ok {
		t.Fatal("an invalid pid must not resolve a ppid")
	}
}
