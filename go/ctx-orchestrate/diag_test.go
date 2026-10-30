package ctxorchestrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetDiagPath clears the cached log path for one test and restores it
// afterwards, so the test can exercise path resolution without disturbing the
// throwaway directory TestMain points every other test at.
func resetDiagPath(t *testing.T) {
	t.Helper()
	diagState.mu.Lock()
	savedPath, savedDegraded := diagState.path, diagState.degraded
	diagState.path = ""
	diagState.mu.Unlock()
	t.Cleanup(func() {
		diagState.mu.Lock()
		diagState.path, diagState.degraded = savedPath, savedDegraded
		diagState.mu.Unlock()
	})
}

// TestResolveDiagDir pins the precedence: an explicit CTX_ORCH_DIAG_DIR wins,
// then the resolved agent dir (PIG_EXT_HOST_INFO/agent-envs/PIG_HOME/ ~/.pig),
// and /tmp stands in when no home resolves.
func TestResolveDiagDir(t *testing.T) {
	// Clear every override the resolver reads so assertions don't depend on the
	// environment the test happens to run in.
	t.Setenv("PIG_EXT_HOST_INFO", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PIG_CODING_AGENT_DIR", "")
	for _, tc := range []struct {
		name    string
		diagDir string
		pigHome string
		home    string
		want    string
	}{
		{"explicit dir wins over PIG_HOME", "/var/log/ctx", "/pig", "/home/u", "/var/log/ctx"},
		{"blank explicit dir falls through", "   ", "/pig", "/home/u", "/pig/agent/logs"},
		{"PIG_HOME is trimmed and cleaned", "", " /pig/ ", "/home/u", "/pig/agent/logs"},
		{"home falls back to ~/.pig", "", "", "/home/u", "/home/u/.pig/agent/logs"},
		{"unknown home falls back to /tmp", "", "", "", "/tmp/.pig/agent/logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PIG_HOME", tc.pigHome)
			if got := resolveDiagDir(tc.diagDir, tc.home); got != tc.want {
				t.Fatalf("resolveDiagDir(%q, %q) = %q, want %q", tc.diagDir, tc.home, got, tc.want)
			}
		})
	}
}

// TestDiagDefaultsToPigHome is the isolation regression: with CTX_ORCH_DIAG_DIR
// unset and PIG_HOME set, one diagnostics line must land in $PIG_HOME/agent/logs
// and nothing may be created under the real home. Before the agent-dir
// resolution unified on ~/.pig the run wrote into $HOME/.pi/agent/logs.
func TestDiagDefaultsToPigHome(t *testing.T) {
	pigHome := t.TempDir()
	home := t.TempDir()
	t.Setenv("CTX_ORCH_DIAG_DIR", "")
	t.Setenv("PIG_EXT_HOST_INFO", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PIG_CODING_AGENT_DIR", "")
	t.Setenv("PIG_HOME", pigHome)
	t.Setenv("HOME", home)
	resetDiagPath(t)

	path, err := diagFile()
	if err != nil {
		t.Fatalf("diagFile: %v", err)
	}
	want := filepath.Join(pigHome, "agent", "logs", "ctx-orchestrate.log")
	if path != want {
		t.Fatalf("diagFile = %q, want %q", path, want)
	}

	diagLog("isolation check")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read diagnostics log: %v", err)
	}
	if !strings.Contains(string(data), "[ctx-orchestrate] isolation check") {
		t.Fatalf("diagnostics line missing from %s: %q", path, data)
	}
	if _, err := os.Stat(filepath.Join(home, ".pi")); !os.IsNotExist(err) {
		t.Fatalf("the run must not create $HOME/.pi (stat err = %v)", err)
	}
}
