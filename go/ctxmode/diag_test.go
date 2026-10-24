package ctxmode

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func itoa(n int) string { return strconv.Itoa(n) }

// ctxmode.test.ts:110.
func TestDiagDegradedRecovery(t *testing.T) {
	isolatedLog(t)
	good := os.Getenv("CTXMODE_DIAG_DIR")
	blocker := filepath.Join(good, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	diagnostics.Lock()
	diagnostics.degraded = false
	diagnostics.path = ""
	diagnostics.Unlock()
	t.Setenv("CTXMODE_DIAG_DIR", filepath.Join(blocker, "sub"))
	DiagLog("probe1")
	DiagLog("probe2")
	os.Stderr = old
	_ = w.Close()
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "诊断日志降级") != 1 {
		t.Fatalf("warnings %q", data)
	}
	requireContains(t, string(data), "日志目录不可用")
	t.Setenv("CTXMODE_DIAG_DIR", good)
	DiagLog("degrade-recovery")
	requireContains(t, fileText(t, logPath()), "degrade-recovery")
}

// ctxmode.test.ts:128.
func TestStderrRing(t *testing.T) {
	c := newClient()
	for i := 1; i <= 25; i++ {
		c.handleStderrChunkLocked("ring-line-" + itoa(i) + "\n")
	}
	if len(c.stderrBuffer) != 20 || c.stderrBuffer[0] != "ring-line-6" || c.stderrBuffer[19] != "ring-line-25" {
		t.Fatal(c.stderrBuffer)
	}
}

// ctxmode.test.ts:137.
func TestStderrCarry(t *testing.T) {
	c := newClient()
	c.handleStderrChunkLocked("carry-a\ncarry-b\ncarry-")
	if !reflect.DeepEqual(c.stderrBuffer, []string{"carry-a", "carry-b"}) || c.stderrCarry != "carry-" {
		t.Fatal(c.stderrBuffer, c.stderrCarry)
	}
	c.handleStderrChunkLocked("c-tail\n")
	if c.stderrBuffer[2] != "carry-c-tail" || c.stderrCarry != "" {
		t.Fatal(c.stderrBuffer)
	}
	c.handleStderrChunkLocked("dangling-no-newline")
	c.flushStderrCarryLocked()
	if c.stderrBuffer[3] != "dangling-no-newline" || c.stderrCarry != "" {
		t.Fatal(c.stderrBuffer)
	}
}

// ctxmode.test.ts:152.
func TestDumpStderrFallback(t *testing.T) {
	isolatedLog(t)
	c := newClient()
	for i := 1; i <= 6; i++ {
		c.handleStderrChunkLocked("fallback level=INFO n=" + itoa(i) + "\n")
	}
	c.dumpStderrBufferLocked(true)
	log := fileText(t, logPath())
	for _, s := range []string{"last stderr before exit (5 lines)", "n=2", "n=6"} {
		requireContains(t, log, s)
	}
	if strings.Contains(log, "n=1") || len(c.stderrBuffer) != 0 {
		t.Fatal(log)
	}
	c.handleStderrChunkLocked("fallback2 level=INFO x=1\n")
	c.dumpStderrBufferLocked(false)
	if fileText(t, logPath()) != log {
		t.Fatal("non-force emitted logs")
	}
}

// ctxmode.test.ts:172.
func TestExitZero(t *testing.T) {
	isolatedLog(t)
	c := newClient()
	c.stopped = true
	c.handleStderrChunkLocked("exit0-visible\n")
	c.handleProcExitLocked("0", "")
	log := fileText(t, logPath())
	requireContains(t, log, "unexpected exit code=0")
	if strings.Contains(log, "abnormally") || strings.Contains(log, "exit0-visible") {
		t.Fatal(log)
	}
}

// ctxmode.test.ts:185.
func TestExitSignal(t *testing.T) {
	isolatedLog(t)
	c := newClient()
	c.stopped = true
	c.handleStderrChunkLocked("killed-line-1\nkilled-line-2\n")
	c.handleProcExitLocked("null", "SIGKILL")
	log := fileText(t, logPath())
	for _, s := range []string{"abnormally code=null signal=SIGKILL", "last stderr before exit (2 lines)", "killed-line-1", "killed-line-2"} {
		requireContains(t, log, s)
	}
}

// ctxmode.test.ts:196.
func TestExitNonzero(t *testing.T) {
	isolatedLog(t)
	c := newClient()
	c.stopped = true
	c.handleStderrChunkLocked("crash-code-5\n")
	c.handleProcExitLocked("5", "")
	log := fileText(t, logPath())
	requireContains(t, log, "abnormally code=5")
	requireContains(t, log, "crash-code-5")
}

// ctxmode.test.ts:283.
func TestDiagRotation(t *testing.T) {
	isolatedLog(t)
	t.Setenv("CTXMODE_DIAG_MAX_BYTES", "100")
	for i := 1; i <= 4; i++ {
		DiagLog("ROT#" + itoa(i) + " " + strings.Repeat("x", 150))
	}
	for _, tc := range []struct{ suffix, want, absent string }{{"", "ROT#4", "ROT#3"}, {".1", "ROT#3", "ROT#4"}, {".2", "ROT#2", "ROT#1"}} {
		log := fileText(t, logPath()+tc.suffix)
		requireContains(t, log, tc.want)
		if strings.Contains(log, tc.absent) {
			t.Fatal(log)
		}
	}
	if _, err := os.Stat(logPath() + ".3"); !os.IsNotExist(err) {
		t.Fatalf("extra history: %v", err)
	}
}

// P2-1: CTXMODE_DIAG_MAX_BYTES="0" is a number, so every append rotates first.
func TestDiagMaxBytesZeroRotatesEveryWrite(t *testing.T) {
	isolatedLog(t)
	t.Setenv("CTXMODE_DIAG_MAX_BYTES", "0")
	DiagLog("ROT-ZERO-1")
	DiagLog("ROT-ZERO-2")
	log := fileText(t, logPath())
	requireContains(t, log, "ROT-ZERO-2")
	if strings.Contains(log, "ROT-ZERO-1") {
		t.Fatal(log)
	}
	requireContains(t, fileText(t, logPath()+".1"), "ROT-ZERO-1")
}

func TestDiagOptInsAndFiltering(t *testing.T) {
	isolatedLog(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	t.Setenv("CTXMODE_DEBUG", "1")
	DiagLog("debug only")
	t.Setenv("CTXMODE_DEBUG", "")
	t.Setenv("CTXMODE_DIAG_STDERR", "1")
	DiagLog("both")
	os.Stderr = old
	_ = w.Close()
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, string(data), "debug only")
	requireContains(t, string(data), "both")
	log := fileText(t, logPath())
	requireContains(t, log, "both")
	if strings.Contains(log, "debug only") {
		t.Fatal(log)
	}
	for _, line := range []string{"level=INFO x", "level=debug y", `{"level" : "DEBUG"}`, `{"level":"INFO"}`} {
		if isInterestingStderrLine(line) {
			t.Fatalf("not filtered: %s", line)
		}
	}
	if !isInterestingStderrLine("level=ERROR failure") {
		t.Fatal("ERROR filtered")
	}
}
