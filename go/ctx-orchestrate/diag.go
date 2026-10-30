package ctxorchestrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Diagnostics: TUI-safe logging. The TS version never uses console.* by default
// (it would corrupt the TUI input line): lines go to a log file, and only
// CTX_ORCH_DEBUG=1 also writes them to stderr.
//
//	CTX_ORCH_DEBUG=1     also write to stderr
//	CTX_ORCH_DIAG_DIR    directory of the log file (wins over everything below)
//	agent dir            resolveAgentDir(): PIG_EXT_HOST_INFO.agentDir,
//	                     PI_/PIG_CODING_AGENT_DIR, $PIG_HOME/agent, ~/.pig/agent
//
// Without an override the log lands in <agent dir>/logs under ~/.pig — never
// ~/.pi, so no run can recreate the retired Pi home.

// diagState is the process-wide diagnostics sink: the log path is resolved once,
// and a failure to write degrades diagnostics instead of failing the extension.
var diagState struct {
	mu       sync.Mutex
	path     string
	degraded bool
}

// diagLog writes one diagnostics line. TS: diagLog.
func diagLog(msg string) {
	line := "[ctx-orchestrate] " + msg
	if envFlag("CTX_ORCH_DEBUG", false) {
		fmt.Fprintln(os.Stderr, line)
	}
	diagState.mu.Lock()
	defer diagState.mu.Unlock()
	path, err := diagFile()
	if err == nil {
		var file *os.File
		file, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, err = file.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + line + "\n")
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err == nil {
		return
	}
	// Degrade once: drop the resolved path and report the failure through the
	// same sink, which then stops trying (as the TS version does).
	diagState.path = ""
	if diagState.degraded {
		return
	}
	diagState.degraded = true
	diagState.mu.Unlock()
	diagLog("diagnostics degraded: " + err.Error())
	diagState.mu.Lock()
}

// diagFile resolves (and caches) the diagnostics log path. The caller holds diagState.mu.
func diagFile() (string, error) {
	if diagState.path != "" {
		return diagState.path, nil
	}
	home, _ := os.UserHomeDir()
	dir := resolveDiagDir(os.Getenv("CTX_ORCH_DIAG_DIR"), home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	diagState.path = filepath.Join(dir, "ctx-orchestrate.log")
	return diagState.path, nil
}

// resolveDiagDir picks the diagnostics directory: an explicit CTX_ORCH_DIAG_DIR
// wins; otherwise <agent dir>/logs where the agent dir resolves like every Go
// extension (PIG_EXT_HOST_INFO.agentDir, PI_/PIG_CODING_AGENT_DIR,
// $PIG_HOME/agent, ~/.pig/agent — never ~/.pi). The env reads are the process's
// own; the home argument is what ~/.pig and the /tmp stand-in resolve from, so
// tests control it.
func resolveDiagDir(diagDir, home string) string {
	if dir := strings.TrimSpace(diagDir); dir != "" {
		return dir
	}
	if dir := resolveAgentDirWithHome(home); dir != "" {
		return filepath.Join(dir, "logs")
	}
	if home = strings.TrimSpace(home); home == "" {
		home = "/tmp"
	}
	return filepath.Join(home, ".pig", "agent", "logs")
}
