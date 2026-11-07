package codegraphgo

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A real subprocess implements only the MCP protocol; it never opens an index,
// calls a provider, or reads production configuration. Tests exercise the actual
// pipes/readers/timers/Wait rather than mocking the transport into a green result.
// CG_TEST_PIDFILE lets liveness probes assert the child was really reaped.
func TestMCPProcess(t *testing.T) {
	if os.Getenv("CG_TEST_HELPER") != "1" {
		return
	}
	if pidFile := os.Getenv("CG_TEST_PIDFILE"); pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
	}
	mode := os.Getenv("CG_TEST_MODE")
	calls := 0
	stdoutOpen := true
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(90)
		}
		if req.Method == "notifications/initialized" {
			continue
		}
		result := map[string]any{}
		message := ""
		switch req.Method {
		case "initialize":
			if req.Params["protocolVersion"] != "2024-11-05" || req.Params["clientInfo"].(map[string]any)["name"] != "pi-codegraph-go" {
				os.Exit(91)
			}
			if mode == "exit-init" {
				fmt.Fprintln(os.Stderr, `level=INFO warming index`)
				fmt.Fprintln(os.Stderr, `open /decided-root/.codegraph/index.lock: permission denied EACCES`)
				fmt.Fprint(os.Stderr, `direct fallback blocked TOKEN=abc123partial-no-newline`)
				os.Exit(3)
			}
			if mode == "exit-init-clean" {
				os.Exit(0)
			}
			if mode == "exit-flood" {
				fmt.Fprintln(os.Stderr, "level=INFO banner")
				// 20 KB without a newline: the carry fragment must stay
				// bounded instead of accumulating until the process exits.
				fmt.Fprint(os.Stderr, strings.Repeat("F", 20*1024))
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "FATAL open /decided-root/.codegraph/index.lock: permission denied EACCES")
				os.Exit(4)
			}
			if mode == "hang-init" {
				continue
			}
			if mode == "init-error" {
				message = "handshake rejected"
			}
			result["instructions"] = "server fixture instructions"
		case "tools/list":
			if mode == "exit-list" {
				fmt.Fprintln(os.Stderr, `crash after handshake API_KEY: zzz`)
				os.Exit(9)
			}
			result["tools"] = []any{map[string]any{"name": "codegraph", "description": "fixture", "inputSchema": map[string]any{"type": "object"}}}
		case "tools/call":
			calls++
			if mode == "alive-close-stdout" && !stdoutOpen {
				continue // transport already closed; stay silent and alive
			}
			if req.Params["arguments"].(map[string]any)["action"] == "__kill__" {
				fmt.Fprintf(os.Stderr, "level=INFO killed by fixture\nlevel=ERROR final carry")
				os.Exit(7)
			}
			if req.Params["arguments"].(map[string]any)["action"] == "__flood__" {
				// ~120 KB of stderr while the call is in flight: the DEBUG
				// mirror must not let an unread parent stderr stall the
				// reader or the request (R5).
				for i := 0; i < 2000; i++ {
					fmt.Fprintf(os.Stderr, "flood line %d padding padding padding padding padding\n", i)
				}
			}
			if mode == "exit-run" {
				fmt.Fprintln(os.Stderr, `level=INFO note`)
				fmt.Fprintln(os.Stderr, `fatal: auth password='p@ss w0rd' Authorization: Bearer regtokXYZ url https://u:p%40ss@example.invalid/x`)
				fmt.Fprintln(os.Stderr, "shutdown reason: index corrupt")
				os.Exit(7)
			}
			if mode == "disconnect" {
				for i := 0; i < 25; i++ {
					fmt.Fprintf(os.Stderr, "level=INFO line %d\n", i)
				}
				fmt.Fprint(os.Stderr, "level=ERROR final carry")
				os.Exit(7)
			}
			if mode == "hang-call" {
				continue
			}
			if mode == "timeout-once" && calls == 1 {
				continue
			}
			if mode == "error" {
				message = "invalid argument"
			}
			if mode == "retry-error" {
				message = "disconnected fixture"
			}
			args := req.Params["arguments"].(map[string]any)
			if req.Params["name"] != "codegraph" {
				os.Exit(92)
			}
			text := fmt.Sprintf("## Result\nmax=%v\n```go\ncode()\n```\nf.go:2:source", args["max_results"])
			if args["action"] == "empty" {
				text = ""
			}
			result["content"] = []any{map[string]any{"type": "text", "text": text}}
		default:
			message = "unknown method"
		}
		fmt.Fprintln(os.Stdout, "non-json startup noise")
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if message != "" {
			delete(resp, "result")
			resp["error"] = map[string]any{"code": -1, "message": message}
		}
		if encoder.Encode(resp) != nil {
			os.Exit(93)
		}
		if mode == "exit-after-list" && req.Method == "tools/list" {
			// Respond first, THEN die: the tools/list result is delivered to
			// the reader before the monitor reaps — the identity-race shape
			// that must surface THIS process's exit report (R6).
			fmt.Fprintln(os.Stderr, `crash after handshake API_KEY: zzz`)
			os.Exit(9)
		}
		if mode == "stall-stdin" && req.Method == "tools/list" {
			// Stop reading stdin right after the handshake but stay alive:
			// the next request write must hit its deadline instead of
			// parking forever on a full pipe (R3).
			for {
				time.Sleep(time.Hour)
			}
		}
		if mode == "alive-close-stdout" && req.Method == "tools/list" {
			// Close stdout AFTER the handshake: the client's reader sees EOF
			// while the child stays alive (R4).
			os.Stdout.Close()
			stdoutOpen = false
		}
		if mode == "alive-close-stdin" && req.Method == "tools/list" {
			// Close stdin AFTER the handshake: the next client write hits a
			// dead pipe while the child stays alive (R4).
			os.Stdin.Close()
		}
	}
	// stdin ended (EOF or closed). alive-close-* modes keep the child
	// alive — orphaned but running — so the client has to retire it (R4).
	for mode == "alive-close-stdout" || mode == "alive-close-stdin" {
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// helperClient builds a client whose subprocess is this test binary in the
// given mode sequence. It returns the spawn counter and the PID file path
// (rewritten on each spawn) for liveness assertions.
func helperClient(t *testing.T, modes ...string) (*CodeGraphClient, *atomic.Int32, string) {
	t.Helper()
	isolated(t)
	t.Setenv("CODEGRAPH_GO_START_TIMEOUT_MS", "3000")
	t.Setenv("CODEGRAPH_GO_REQUEST_TIMEOUT_MS", "3000")
	c := newCodeGraphClient("/decided-root", "fixture note")
	counter := &atomic.Int32{}
	pidFile := filepath.Join(t.TempDir(), "pid")
	c.command = func(_ string, args ...string) *exec.Cmd {
		i := int(counter.Add(1)) - 1
		mode := "normal"
		if len(modes) > 0 {
			mode = modes[min(i, len(modes)-1)]
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestMCPProcess$", "--")
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "CG_TEST_HELPER=1", "CG_TEST_MODE="+mode, "CG_TEST_PIDFILE="+pidFile)
		return cmd
	}
	t.Cleanup(func() {
		c.mu.Lock()
		p := c.proc
		c.mu.Unlock()
		c.stop()
		if p != nil {
			select {
			case <-p.done:
			case <-time.After(4 * time.Second):
				t.Error("subprocess did not stop")
			}
		}
	})
	return c, counter, pidFile
}

// waitPIDGone polls kill(pid, 0) until the helper PID recorded in pidFile is
// gone or the deadline hits. Returns false while the process still exists
// (an unreaped zombie still counts as alive — kill(0) reports it).
func waitPIDGone(t *testing.T, pidFile string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
					return true
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// readPID returns the last recorded helper PID, or 0 when not yet written.
func readPID(pidFile string) int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func TestClientHandshakeAndConcurrentStart(t *testing.T) {
	c, count, _ := helperClient(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Go(func() { errs <- c.start() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 1 || len(c.getTools()) != 1 || c.getServerInstructions() != "server fixture instructions" {
		t.Fatal("handshake/single spawn")
	}
	results := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			text, err := c.callTool("codegraph", map[string]any{"action": "search", "max": 999})
			if err != nil {
				results <- err.Error()
			} else {
				results <- text
			}
		})
	}
	wg.Wait()
	close(results)
	for got := range results {
		if !strings.Contains(got, "[ 1 行代码已省略 ]") || !strings.HasSuffix(got, "f.go:2") {
			t.Fatal(got)
		}
	}
	text, err := c.callTool("codegraph", map[string]any{"action": "node"})
	if err != nil || !strings.Contains(text, "f.go:2:source") {
		t.Fatal(text, err)
	}
	text, err = c.callTool("codegraph", map[string]any{"action": "empty"})
	if err != nil || text != "no result" {
		t.Fatal(text, err)
	}
	c.mu.Lock()
	p := c.proc
	c.mu.Unlock()
	c.stop()
	<-p.done
	if _, err = c.sendRequest("tools/list", nil, time.Second); err == nil || err.Error() != "codegraph-go not running" {
		t.Fatal(err)
	}
	c.sendNotification("noop", nil)
}
func TestClientDisconnectReconnectOnce(t *testing.T) {
	c, count, _ := helperClient(t, "disconnect", "normal")
	got, err := c.callTool("codegraph", map[string]any{"action": "search"})
	if err != nil || !strings.Contains(got, "max=70") || count.Load() != 2 {
		t.Fatal(got, err, count.Load())
	}
	b, err := os.ReadFile(filepath.Join(homeDir(), ".pig", "agent", "logs", "codegraph-go.log"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(string(b), "process exited abnormally code=7") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		b, _ = os.ReadFile(filepath.Join(homeDir(), ".pig", "agent", "logs", "codegraph-go.log"))
	}
	if !strings.Contains(string(b), "process exited abnormally code=7") || !strings.Contains(string(b), "level=ERROR final carry") || strings.Contains(string(b), "level=INFO line") {
		t.Fatal(string(b))
	}
}
func TestClientTimeoutAndErrorBoundaries(t *testing.T) {
	// hang-init pins the TS callTool shape: `if (!this.initialized) await
	// this.start()` sits outside the try/catch, so a failing start returns its
	// own error and must not spawn a second process through the retry branch.
	for _, tc := range []struct {
		mode    string
		want    string
		spawns  int32
		success bool
	}{
		{"timeout-once", "", 1, true}, {"hang-call", "timed out", 1, false}, {"retry-error", "disconnected fixture", 1, false}, {"error", "invalid argument", 1, false}, {"init-error", "handshake rejected", 1, false}, {"hang-init", "initialize timed out", 1, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c, count, _ := helperClient(t, tc.mode)
			t.Setenv("CODEGRAPH_GO_REQUEST_TIMEOUT_MS", "30")
			if tc.mode == "hang-init" {
				t.Setenv("CODEGRAPH_GO_START_TIMEOUT_MS", "30")
			}
			got, err := c.callTool("codegraph", map[string]any{"action": "search"})
			if tc.success {
				if err != nil || !strings.Contains(got, "Result") {
					t.Fatal(got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
			if count.Load() != tc.spawns {
				t.Fatal(count.Load())
			}
			c.mu.Lock()
			pending := len(c.pending)
			c.mu.Unlock()
			if pending != 0 {
				t.Fatal("pending requests leaked")
			}
		})
	}
}

// P1-2: TS keeps `if (!this.initialized) await this.start()` outside the
// try/catch, so a first start() that fails returns that error without a second
// spawn; only a failed tools/call that matches not running|disconnected|timed
// out starts once more. The next call is then free to start fresh.
func TestCallToolFailedStartDoesNotRetry(t *testing.T) {
	c, count, _ := helperClient(t, "hang-init", "normal")
	t.Setenv("CODEGRAPH_GO_START_TIMEOUT_MS", "30")
	if _, err := c.callTool("codegraph", map[string]any{"action": "search"}); err == nil || !strings.Contains(err.Error(), "initialize timed out") {
		t.Fatal(err)
	}
	// A retry would spawn the "normal" fixture and answer with a result instead.
	if count.Load() != 1 {
		t.Fatalf("failed first start must not retry: spawns=%d", count.Load())
	}
	t.Setenv("CODEGRAPH_GO_START_TIMEOUT_MS", "3000")
	if got, err := c.callTool("codegraph", map[string]any{"action": "search"}); err != nil || !strings.Contains(got, "max=70") || count.Load() != 2 {
		t.Fatal(got, err, count.Load())
	}
}
func TestClientMissingBinary(t *testing.T) {
	isolated(t)
	c := newCodeGraphClient("/decided", "cwd")
	t.Setenv("CODEGRAPH_GO_BIN", filepath.Join(t.TempDir(), "absent"))
	if err := c.start(); err == nil {
		t.Fatal("missing binary accepted")
	}
	c.stop()
	if c.proc != nil || c.initialized {
		t.Fatal("cleanup")
	}
	if _, err := startClientAt("/decided", "cwd"); err == nil {
		t.Fatal("startClientAt error")
	}
}
func TestStderrRingAndDiagnostics(t *testing.T) {
	h := isolated(t)
	c := newCodeGraphClient(h, "cwd")
	p := &clientProcess{}
	for i := 0; i < 25; i++ {
		c.consumeStderr(p, fmt.Sprintf("level=INFO line%d\n", i))
	}
	c.consumeStderr(p, "level=DEBUG partial")
	c.consumeStderr(p, " tail")
	c.mu.Lock()
	c.flushStderrCarryLocked(p)
	if len(p.stderr) != 20 || p.stderr[0] != "level=INFO line6" {
		t.Fatal(p.stderr)
	}
	c.dumpStderrBufferLocked(p, true)
	c.mu.Unlock()
	log := filepath.Join(h, ".pig", "agent", "logs", "codegraph-go.log")
	deadline := time.Now().Add(2 * time.Second)
	var b []byte
	for {
		b, _ = os.ReadFile(log)
		if strings.Contains(string(b), "(5 lines)") || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(b), "(5 lines)") || !strings.Contains(string(b), "partial tail") || strings.Contains(string(b), "line6") {
		t.Fatal(string(b))
	}
	for _, line := range []string{"level=INFO hi", `{"level": "debug"}`} {
		if isInterestingStderrLine(line) {
			t.Fatal(line)
		}
	}
	if !isInterestingStderrLine("warning") {
		t.Fatal("warning")
	}
	c.consumeStderr(p, "level=DEBUG ignored\n")
	c.mu.Lock()
	c.dumpStderrBufferLocked(p, false)
	c.clearStderrBufferLocked(p)
	c.mu.Unlock()
	t.Setenv("CODEGRAPH_GO_DEBUG", "1")
	diagLog("debug test")
	c.consumeStderr(p, "debug chunk\n")
	// A failed mkdir is deliberately TUI-safe and does not panic.
	t.Setenv("CODEGRAPH_GO_DEBUG", "")
	writeFixture(t, filepath.Join(h, "blocked"), "file")
	t.Setenv("HOME", filepath.Join(h, "blocked"))
	diagLog("unwritable directory")
	// P2-4: os.homedir() || "/tmp" — an unresolvable HOME falls back to
	// /tmp/.pig/agent/logs/codegraph-go.log. The fixture cleans up only what it
	// created itself.
	t.Setenv("HOME", "")
	fallback := filepath.Join("/tmp", ".pig", "agent", "logs", "codegraph-go.log")
	_, statErr := os.Stat(fallback)
	diagLog("home fallback")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ = os.ReadFile(fallback)
		if strings.Contains(string(b), "home fallback") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(b), "home fallback") {
		t.Fatalf("homedir fallback: %q", string(b))
	}
	if statErr != nil {
		t.Cleanup(func() {
			os.Remove(fallback)
			dir := filepath.Dir(fallback)
			for range 3 { // logs, agent, .pig — only empty dirs are removed
				os.Remove(dir)
				dir = filepath.Dir(dir)
			}
		})
	}
}

// The sandbox-refusal shape from production: the daemon exits while the
// client waits on initialize. The tool error must carry phase, exit code,
// workdir and a bounded, sanitized stderr tail instead of a bare
// "disconnected" or a start-timeout. The process identity is captured via
// the monitor hook BEFORE start returns (R7): asserting c.proc afterwards
// would silently skip when it is already nil.
func TestClientExitDiagOnStartup(t *testing.T) {
	c, _, _ := helperClient(t, "exit-init")
	var procsMu sync.Mutex
	var procs []*clientProcess
	c.testAfterMonitorWait = func() {
		c.mu.Lock()
		p := c.proc
		c.mu.Unlock()
		procsMu.Lock()
		procs = append(procs, p)
		procsMu.Unlock()
	}
	err := c.start()
	if err == nil {
		t.Fatal("start accepted dead process")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.phase != "initialize" || ee.code != "3" || ee.workdir != "/decided-root" {
		t.Fatalf("diag fields: %#v", err)
	}
	text := err.Error()
	for _, want := range []string{"phase=initialize", "exit=3", "workdir=/decided-root", "permission denied EACCES", "direct fallback blocked", "TOKEN=REDACTED"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "ueb099oytvcdy") || strings.Contains(text, "level=INFO warming index") {
		t.Fatal("secret leaked or INFO noise kept: " + text)
	}
	// stderr already hit EOF: a second start may not hang on its goroutines.
	if err = c.start(); err == nil || !strings.Contains(err.Error(), "exit=3") {
		t.Fatal(err)
	}
	// Every process the monitor saw must be fully reaped — none may linger
	// past its own done channel (R4/R7).
	procsMu.Lock()
	defer procsMu.Unlock()
	if len(procs) == 0 {
		t.Fatal("monitor never observed a process")
	}
	for _, p := range procs {
		if p == nil {
			continue
		}
		select {
		case <-p.done:
		case <-time.After(4 * time.Second):
			t.Fatal("Wait goroutine leaked after exit")
		}
	}
}
func TestClientExitDiagPhases(t *testing.T) {
	for _, tc := range []struct {
		mode, phase, code, want string
	}{
		{"exit-init-clean", "initialize", "0", ""},
		{"exit-list", "tools/list", "9", "API_KEY: REDACTED"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c, _, _ := helperClient(t, tc.mode)
			err := c.start()
			if err == nil {
				t.Fatal("start accepted dead process")
			}
			text := err.Error()
			for _, w := range []string{"phase=" + tc.phase, "exit=" + tc.code, "workdir=/decided-root", tc.want} {
				if !strings.Contains(text, w) {
					t.Fatalf("missing %q in %q", w, text)
				}
			}
			if strings.Contains(text, "zzz") {
				t.Fatal("secret leaked: " + text)
			}
		})
	}
}
func TestClientExitDiagRunningPhase(t *testing.T) {
	c, _, _ := helperClient(t)
	if err := c.start(); err != nil {
		t.Fatal(err)
	}
	_, err := c.sendRequest("tools/call", map[string]any{"name": "codegraph", "arguments": map[string]any{"action": "__kill__"}}, 3000*time.Millisecond)
	if err == nil {
		t.Fatal("dead daemon answered")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.phase != "running" || ee.code != "7" {
		t.Fatalf("diag fields: %#v", err)
	}
	if !strings.Contains(err.Error(), "level=ERROR final carry") {
		t.Fatal("stderr tail missing: " + err.Error())
	}
	// callTool treats the diagnostic as reconnectable and restarts once.
	if got, err := c.callTool("codegraph", map[string]any{"action": "search"}); err != nil || !strings.Contains(got, "max=70") {
		t.Fatal(got, err)
	}
}
func TestClientMissingBinaryDiag(t *testing.T) {
	isolated(t)
	c := newCodeGraphClient("/decided", "cwd")
	absent := filepath.Join(t.TempDir(), "absent")
	t.Setenv("CODEGRAPH_GO_BIN", absent)
	err := c.start()
	if err == nil || !strings.Contains(err.Error(), "phase=spawn") || !strings.Contains(err.Error(), "workdir=/decided") || !strings.Contains(err.Error(), absent) {
		t.Fatal(err)
	}
}

// A closed stdin must surface the write error/diagnostic at once instead of
// parking the request on a pending channel nobody will deliver to. Retired
// process identity keeps the second cleanup a no-op.
func TestClientWriteFailureReturnsPromptly(t *testing.T) {
	c := newCodeGraphClient("/decided", "cwd")
	r, w := io.Pipe()
	r.Close()
	w.Close()
	p := &clientProcess{cmd: exec.Command("true"), stdin: w, phase: "running", done: make(chan struct{})}
	c.mu.Lock()
	c.proc = p
	c.initialized = true
	c.mu.Unlock()
	_, err := c.sendRequest("tools/call", map[string]any{"name": "codegraph"}, 3000*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.cleanupLocked(p, errors.New("again")) // retired already; must not panic
	pending := len(c.pending)
	proc := c.proc
	c.mu.Unlock()
	if proc != nil || pending != 0 || c.initialized {
		t.Fatal("cleanup")
	}
}

// Diagnostics are for the tool result text and the log file only: protocol
// stdout must stay byte-clean while a startup failure is reported.
func TestClientDiagDoesNotPolluteStdout(t *testing.T) {
	c, _, _ := helperClient(t, "exit-init")
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	_, err := c.callTool("codegraph", map[string]any{"action": "search"})
	os.Stdout = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "exit=3") {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("stdout polluted: %q", string(out))
	}
}

// The stderr tail embedded in errors is bounded and credential-shaped
// assignments are masked. Selection mirrors the log dump.
func TestStderrDiagSummaryBoundedAndSanitized(t *testing.T) {
	c := newCodeGraphClient("/decided", "cwd")
	p := &clientProcess{}
	for i := 0; i < 25; i++ {
		c.consumeStderr(p, fmt.Sprintf("info %d\n", i))
	}
	c.consumeStderr(p, "TOKEN=abc123 fail\n"+strings.Repeat("x", 300)+"\n"+`password: "hunter2" tail`)
	c.mu.Lock()
	summary := c.stderrDiagSummaryLocked(p)
	c.mu.Unlock()
	if len(summary) > diagSummaryBytes {
		t.Fatalf("unbounded summary %d > %d bytes", len(summary), diagSummaryBytes)
	}
	if strings.Contains(summary, "abc123") || strings.Contains(summary, "hunter2") {
		t.Fatal(summary)
	}
	if !strings.Contains(summary, "TOKEN=REDACTED") || !strings.Contains(summary, "…") {
		t.Fatal(summary)
	}
	if got := sanitizeDiagText(`{"api_key":"sk-1","n":1}`); strings.Contains(got, "sk-1") {
		t.Fatal(got)
	}
	// A quoted value with spaces is masked whole; an opening quote that
	// never closes masks the rest of the line instead of leaking words —
	// for both quote kinds (R1).
	if got := sanitizeDiagText(`password = "s e c r e t" tail`); strings.Contains(got, "s e c r e t") || !strings.Contains(got, "tail") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText(`TOKEN="open quote leaks`); strings.Contains(got, "leaks") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText(`TOKEN='single open leaks`); strings.Contains(got, "leaks") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText(`TOKEN='s e c r e t' tail`); strings.Contains(got, "s e c r e t") || !strings.Contains(got, "tail") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText("Authorization: Bearer tok123"); strings.Contains(got, "tok123") || !strings.Contains(got, "Bearer REDACTED") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText("https://u:p%40ss@h/x"); strings.Contains(got, "p%40ss") {
		t.Fatal(got)
	}
	// Zero-width splicing cannot reassemble an unmasked key: controls are
	// normalized before credential shapes are matched (R1).
	if got := sanitizeDiagText("TOK\u200bEN=s3cr3t"); strings.Contains(got, "s3cr3t") || strings.Contains(got, "TOK\u200bEN") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText("TO\u200b+\u200bKEN=x"); !strings.Contains(got, "TO+KEN") {
		t.Fatal(got)
	}
	if got := sanitizeDiagText("ok\x1b[2J\x07end"); got != "ok[2Jend" {
		t.Fatal(got)
	}
	if got := truncateRunes("日本語テスト文字列", 4); got != "日本語テ…" {
		t.Fatal(got)
	}
	if got := truncateBytesTail(strings.Repeat("x", 100), 10); got != "…xxxxxxx" || len(got) != 10 {
		t.Fatal(got, len(got))
	}
}

// Cross-chunk safety (R1): a secret split across reads must be masked as a
// whole — never as one sanitized half plus one raw half — and a newline-free
// flood must hit its bound BEFORE a newline or process exit (R7): the raw
// carry itself is checked mid-stream, not only the final text.
func TestConsumeStderrCrossChunkBound(t *testing.T) {
	c := newCodeGraphClient("/decided", "cwd")
	p := &clientProcess{}
	// Double-quoted value with a space, split mid-value across two chunks.
	c.consumeStderr(p, `x TOKEN="ab`)
	c.consumeStderr(p, `cd" tail`+"\n")
	c.mu.Lock()
	c.flushStderrCarryLocked(p)
	joined := strings.Join(p.stderr, "\n")
	c.mu.Unlock()
	if strings.Contains(joined, "abcd") || !strings.Contains(joined, "TOKEN=REDACTED") {
		t.Fatal(joined)
	}
	// Unterminated fragment grows past the bound: the carry state must be
	// bounded at THIS point — before any newline or exit (R7).
	p2 := &clientProcess{}
	flood := strings.Repeat("F", 30000)
	for i := 0; i < len(flood); i += 4096 {
		end := min(i+4096, len(flood))
		c.consumeStderr(p2, flood[i:end])
		c.mu.Lock()
		if len(p2.carry) > diagCarryBytes {
			t.Fatalf("unbounded carry mid-flood: %d bytes", len(p2.carry))
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	truncFlag := p2.carryTrunc
	ringLen := len(p2.stderr)
	c.mu.Unlock()
	if !truncFlag || ringLen == 0 || ringLen > 2 {
		t.Fatalf("flood not committed once and discarded: carryTrunc=%v ring=%d", truncFlag, ringLen)
	}
	// Text before the newline belongs to the discarded line; the record
	// after it starts clean and the bounded head can never reassemble.
	c.consumeStderr(p2, "\nTRAILER ok\n")
	c.mu.Lock()
	joined = strings.Join(p2.stderr, "\n")
	c.mu.Unlock()
	if strings.Contains(joined, strings.Repeat("F", 300)) {
		t.Fatalf("unbounded flood kept: %d bytes", len(joined))
	}
	if !strings.Contains(joined, "TRAILER ok") {
		t.Fatal(joined)
	}
	// Single-quoted secret split across chunks, quote never closed: the
	// whole tail stays masked, nothing leaks word by word.
	p3 := &clientProcess{}
	c.consumeStderr(p3, "TOKEN='one t")
	c.consumeStderr(p3, "wo\n")
	c.mu.Lock()
	c.flushStderrCarryLocked(p)
	joined = strings.Join(p3.stderr, "\n")
	c.mu.Unlock()
	if strings.Contains(joined, "one two") || !strings.Contains(joined, "TOKEN=REDACTED") {
		t.Fatal(joined)
	}
}

// Startup with a newline-free stderr flood: the error text stays bounded and
// keeps the fatal tail instead of the flood (R1).
func TestClientExitDiagFloodBounded(t *testing.T) {
	c, _, _ := helperClient(t, "exit-flood")
	err := c.start()
	if err == nil {
		t.Fatal("start accepted dead process")
	}
	text := err.Error()
	for _, want := range []string{"phase=initialize", "exit=4", "EACCES"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, strings.Repeat("F", 600)) {
		t.Fatalf("unbounded flood kept: %d bytes", len(text))
	}
	if len(text) > 2048 {
		t.Fatalf("diagnostic exceeded bound: %d bytes", len(text))
	}
}

// R3: the request deadline covers queueing AND the actual write. A child
// that stops reading stdin makes a >pipe-capacity write hit its deadline,
// retire ITS process, and leave no wedged writer behind — without touching
// a successor (the callTool one-shot retry rule is unchanged).
func TestClientWriteDeadlineCoversBlockedPipe(t *testing.T) {
	c, count, pidFile := helperClient(t, "stall-stdin", "normal")
	if err := c.start(); err != nil {
		t.Fatal(err)
	}
	pid := readPID(pidFile)
	if pid <= 0 {
		t.Fatal("no helper PID recorded")
	}
	// Capture the process identity BEFORE the request retires it (R7):
	// asserting on c.proc afterwards would silently skip once it is nil.
	c.mu.Lock()
	p1 := c.proc
	c.mu.Unlock()
	if p1 == nil {
		t.Fatal("no process after start")
	}
	// ~256 KiB request — far above the pipe buffer — while the child never
	// reads stdin again.
	big := map[string]any{"name": "codegraph", "arguments": map[string]any{"action": "search", "pad": strings.Repeat("x", 256*1024)}}
	t.Setenv("CODEGRAPH_GO_REQUEST_TIMEOUT_MS", "300")
	started := time.Now()
	_, err := c.sendRequest("tools/call", big, 300*time.Millisecond)
	if err == nil {
		t.Fatal("wedged daemon answered")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("sendRequest blocked %v past its deadline", elapsed)
	}
	if !waitPIDGone(t, pidFile, 4*time.Second) {
		t.Fatal("write deadline leaked a live child process")
	}
	c.mu.Lock()
	pending := len(c.pending)
	proc := c.proc
	c.mu.Unlock()
	if pending != 0 || proc != nil {
		t.Fatal("pending/proc leaked after write deadline")
	}
	// No leftover writer: the retired process's quit signal is closed and
	// its own done settled (reap responsibility never dropped).
	select {
	case <-p1.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("writer queue still open after retirement")
	}
	select {
	case <-p1.done:
	case <-time.After(4 * time.Second):
		t.Fatal("retired process was never reaped")
	}
	// callTool's single reconnect rule still applies and spawns a NEW
	// process (normal mode) — the old monitor must not clear it.
	got, err := c.callTool("codegraph", map[string]any{"action": "search"})
	if err != nil || !strings.Contains(got, "max=70") {
		t.Fatal(got, err)
	}
	if count.Load() != 2 {
		t.Fatalf("successor spawn missing: %d", count.Load())
	}
	if newPID := readPID(pidFile); newPID == pid {
		t.Fatalf("stale PID file after respawn: %d", newPID)
	}
}

// R4: a child that closes stdout (or dies holding stderr) while staying
// alive is retired in bounded time — pending requests end, the PID is
// reaped, and the old monitor never clears a successor's identity.
func TestClientOrphanedProcessRetired(t *testing.T) {
	c, count, pidFile := helperClient(t, "alive-close-stdout", "normal")
	if err := c.start(); err != nil {
		t.Fatal(err)
	}
	pid := readPID(pidFile)
	// The helper closed stdout right after tools/list. The reader hits EOF;
	// the orphan-retire path must reap the still-alive child.
	if !waitPIDGone(t, pidFile, 5*time.Second) {
		t.Fatal("orphaned child left running after stdout EOF")
	}
	c.mu.Lock()
	proc := c.proc
	pending := len(c.pending)
	c.mu.Unlock()
	if proc != nil || pending != 0 {
		t.Fatal("orphan retirement left proc/pending behind")
	}
	// A follow-up request surfaces a real diagnostic, and a fresh start
	// must not be disturbed by the retired monitor (R4/R6).
	got, err := c.callTool("codegraph", map[string]any{"action": "search"})
	if err != nil || !strings.Contains(got, "max=70") || count.Load() != 2 {
		t.Fatal(got, err, count.Load())
	}
	if newPID := readPID(pidFile); newPID == 0 || newPID == pid {
		t.Fatalf("respawn PID probe: old=%d new=%d", pid, newPID)
	}
}

// R4/R2: a child that closes its stdin while staying alive fails the next
// write — the caller gets the diagnostic promptly and the child is reaped.
func TestClientOrphanedStdinWriteReaped(t *testing.T) {
	c, _, pidFile := helperClient(t, "alive-close-stdin")
	if err := c.start(); err != nil {
		t.Fatal(err)
	}
	_, err := c.sendRequest("tools/call", map[string]any{"name": "codegraph", "arguments": map[string]any{"action": "search"}}, 3000*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatal(err)
	}
	if !waitPIDGone(t, pidFile, 5*time.Second) {
		t.Fatal("child left running after stdin write failure")
	}
	c.mu.Lock()
	proc := c.proc
	pending := len(c.pending)
	c.mu.Unlock()
	if proc != nil || pending != 0 {
		t.Fatal("write failure left proc/pending behind")
	}
}

// R5: DEBUG mirroring must not let an unread parent stderr stall the
// stderr reader, the request path, or stop(). The sink queue is bounded and
// drops under backpressure; losslessness is not claimed.
func TestClientDiagSinkBackpressureBounded(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The sink's DEBUG destination is swapped for an unread pipe — the real
	// os.Stderr is never redirected or closed.
	diagSetOut(w)
	defer diagSetOut(os.Stderr)
	c, _, _ := helperClient(t, "normal")
	t.Setenv("CODEGRAPH_GO_DEBUG", "1")
	if err := c.start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	got, err := c.callTool("codegraph", map[string]any{"action": "__flood__"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Result") {
		t.Fatal(got)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("call stalled behind unread diagnostic sink: %v", elapsed)
	}
	stopped := make(chan struct{})
	go func() { c.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop stalled behind unread diagnostic sink")
	}
	// Close the pipe end first so a blocked sink write returns, then point
	// the sink at the void and drain the queue before the pipe goes away.
	w.Close()
	diagSetOut(io.Discard)
	diagFlush()
	_, _ = io.Copy(io.Discard, r)
	r.Close()
}

// R6: the tools/list response is delivered before the monitor retires the
// dying child — a deterministic barrier orders reader → monitor → doStart
// without sleeps. doStart must surface THAT process's real exit report
// (phase/exit/workdir/stderr), never a bare "disconnected" and never a
// successor's error.
func TestClientIdentityRaceExitDiag(t *testing.T) {
	c, _, _ := helperClient(t, "exit-after-list")
	// Deterministic barriers, no sleeps: the monitor is held AFTER Wait()
	// returned so the reader can deliver tools/list first; the orphan
	// retiree is held so the monitor (not the reader-EOF path) performs
	// the retirement; the identity check then deterministically loses the
	// race and must report THIS process's real exit.
	gateMonitor := make(chan struct{})
	gateOrphan := make(chan struct{})
	c.testAfterMonitorWait = func() { <-gateMonitor }
	c.testOrphanHook = func() { <-gateOrphan }
	c.testBeforeIdentityCheck = func() {
		close(gateMonitor) // let the monitor retire p first
		c.mu.Lock()
		p := c.proc
		c.mu.Unlock()
		if p != nil {
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
			}
		}
		close(gateOrphan)
	}
	t.Cleanup(func() {
		select {
		case <-gateMonitor:
		default:
			close(gateMonitor)
		}
		select {
		case <-gateOrphan:
		default:
			close(gateOrphan)
		}
	})
	err := c.start()
	if err == nil {
		t.Fatal("start accepted a process that died at tools/list")
	}
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("identity race lost the exit report: %#v", err)
	}
	if ee.phase != "tools/list" || ee.code != "9" || ee.workdir != "/decided-root" {
		t.Fatalf("wrong process identity: %#v", err)
	}
	if !strings.Contains(err.Error(), "API_KEY: REDACTED") || strings.Contains(err.Error(), "zzz") {
		t.Fatal("stderr tail wrong or leaked: " + err.Error())
	}
}
