package ctxmode

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var diagnostics struct {
	sync.Mutex
	path     string
	dir      string
	degraded bool
}

func reportDiagDegraded(reason string) {
	if diagnostics.degraded {
		return
	}
	diagnostics.degraded = true
	fmt.Fprintf(os.Stderr, "[ctxmode] 诊断日志降级: %s; 本次会话不再重复提示\n", reason)
}

// DiagLog is TUI-safe unless DEBUG or DIAG_STDERR is explicitly enabled.
func DiagLog(msg string) {
	diagnostics.Lock()
	defer diagnostics.Unlock()
	line := "[ctxmode] " + msg
	if os.Getenv("CTXMODE_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	if os.Getenv("CTXMODE_DIAG_STDERR") == "1" {
		fmt.Fprintln(os.Stderr, line)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/tmp"
	}
	dir := envString("CTXMODE_DIAG_DIR", filepath.Join(home, ".pi", "agent", "logs"))
	if diagnostics.dir != dir {
		diagnostics.path = ""
		diagnostics.dir = dir
	}
	if diagnostics.path == "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			reportDiagDegraded("日志目录不可用: " + err.Error())
			return
		}
		diagnostics.path = filepath.Join(dir, "ctxmode.log")
	}
	p := diagnostics.path
	if info, err := os.Stat(p); err == nil && info.Size() > int64(envNumber("CTXMODE_DIAG_MAX_BYTES", 5*1024*1024)) {
		_ = os.Remove(p + ".2")
		_ = os.Rename(p+".1", p+".2")
		_ = os.Rename(p, p+".1")
	}
	file, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		_, err = fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), line)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		diagnostics.path = ""
		reportDiagDegraded("写入失败: " + err.Error())
	}
}

var boringStderr = regexp.MustCompile(`(?i)\blevel=(INFO|DEBUG)\b|"level"\s*:\s*"(INFO|DEBUG)"`)

func isInterestingStderrLine(line string) bool { return !boringStderr.MatchString(line) }
