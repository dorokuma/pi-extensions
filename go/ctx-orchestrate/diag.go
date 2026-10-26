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
//	PIG_HOME             when set, the default directory is $PIG_HOME/agent/logs
//
// The default without PIG_HOME is the TS location, <home>/.pi/agent/logs, so a
// plain run keeps parity. A PiG run started with PIG_HOME set (any redirected
// or isolated home) must not write into the production ~/.pi, so its own home
// takes over the default.

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
	dir := resolveDiagDir(os.Getenv("CTX_ORCH_DIAG_DIR"), os.Getenv("PIG_HOME"), home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	diagState.path = filepath.Join(dir, "ctx-orchestrate.log")
	return diagState.path, nil
}

// resolveDiagDir picks the diagnostics directory: an explicit CTX_ORCH_DIAG_DIR
// wins; otherwise a non-empty PIG_HOME keeps the log inside the isolated PiG home
// ($PIG_HOME/agent/logs); otherwise the TS default <home>/.pi/agent/logs, with
// /tmp standing in for a home that cannot be resolved.
func resolveDiagDir(diagDir, pigHome, home string) string {
	if dir := strings.TrimSpace(diagDir); dir != "" {
		return dir
	}
	if root := strings.TrimSpace(pigHome); root != "" {
		return filepath.Join(root, "agent", "logs")
	}
	if home = strings.TrimSpace(home); home == "" {
		home = "/tmp"
	}
	return filepath.Join(home, ".pi", "agent", "logs")
}
