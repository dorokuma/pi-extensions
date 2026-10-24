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

// TestResolveDiagDir pins the precedence: an explicit CTX_ORCH_DIAG_DIR wins, a
// set PIG_HOME keeps the log inside the isolated PiG home, and only then does the
// TS default <home>/.pi/agent/logs apply (/tmp standing in for an unknown home).
func TestResolveDiagDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		diagDir string
		pigHome string
		home    string
		want    string
	}{
		{"explicit dir wins over PIG_HOME", "/var/log/ctx", "/pig", "/home/u", "/var/log/ctx"},
		{"blank explicit dir is unset", "   ", "/pig", "/home/u", "/pig/agent/logs"},
		{"PIG_HOME is trimmed and cleaned", "", " /pig/ ", "/home/u", "/pig/agent/logs"},
		{"home fallback without PIG_HOME", "", "", "/home/u", "/home/u/.pi/agent/logs"},
		{"unknown home falls back to /tmp", "", "", "", "/tmp/.pi/agent/logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDiagDir(tc.diagDir, tc.pigHome, tc.home); got != tc.want {
				t.Fatalf("resolveDiagDir(%q, %q, %q) = %q, want %q", tc.diagDir, tc.pigHome, tc.home, got, tc.want)
			}
		})
	}
}

// TestDiagDefaultsToPigHome is the isolation regression: with CTX_ORCH_DIAG_DIR
// unset and PIG_HOME set (which is what ./bin/pig-test pins), one diagnostics
// line must land in $PIG_HOME/agent/logs and nothing may be created under the
// real home. Before the fix the PIG_HOME branch did not exist, so the run wrote
// into $HOME/.pi/agent/logs.
func TestDiagDefaultsToPigHome(t *testing.T) {
	pigHome := t.TempDir()
	home := t.TempDir()
	t.Setenv("CTX_ORCH_DIAG_DIR", "")
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
