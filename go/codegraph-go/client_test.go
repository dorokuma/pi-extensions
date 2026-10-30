package codegraphgo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A real subprocess implements only the MCP protocol; it never opens an index,
// calls a provider, or reads production configuration. Tests exercise the actual
// pipes/readers/timers/Wait rather than mocking the transport into a green result.
func TestMCPProcess(t *testing.T) {
	if os.Getenv("CG_TEST_HELPER") != "1" {
		return
	}
	mode := os.Getenv("CG_TEST_MODE")
	calls := 0
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
			if mode == "hang-init" {
				continue
			}
			if mode == "init-error" {
				message = "handshake rejected"
			}
			result["instructions"] = "server fixture instructions"
		case "tools/list":
			result["tools"] = []any{map[string]any{"name": "codegraph", "description": "fixture", "inputSchema": map[string]any{"type": "object"}}}
		case "tools/call":
			calls++
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
	}
	os.Exit(0)
}
func helperClient(t *testing.T, modes ...string) (*CodeGraphClient, *atomic.Int32) {
	t.Helper()
	isolated(t)
	t.Setenv("CODEGRAPH_GO_START_TIMEOUT_MS", "3000")
	t.Setenv("CODEGRAPH_GO_REQUEST_TIMEOUT_MS", "3000")
	c := newCodeGraphClient("/decided-root", "fixture note")
	counter := &atomic.Int32{}
	c.command = func(_ string, args ...string) *exec.Cmd {
		i := int(counter.Add(1)) - 1
		mode := "normal"
		if len(modes) > 0 {
			mode = modes[min(i, len(modes)-1)]
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestMCPProcess$", "--")
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "CG_TEST_HELPER=1", "CG_TEST_MODE="+mode)
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
	return c, counter
}
func TestClientHandshakeAndConcurrentStart(t *testing.T) {
	c, count := helperClient(t)
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
	c, count := helperClient(t, "disconnect", "normal")
	got, err := c.callTool("codegraph", map[string]any{"action": "search"})
	if err != nil || !strings.Contains(got, "max=70") || count.Load() != 2 {
		t.Fatal(got, err, count.Load())
	}
	b, err := os.ReadFile(filepath.Join(homeDir(), ".pig", "agent", "logs", "codegraph-go.log"))
	if err != nil {
		t.Fatal(err)
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
			c, count := helperClient(t, tc.mode)
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
	c, count := helperClient(t, "hang-init", "normal")
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
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
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
	b, err = os.ReadFile(fallback)
	if err != nil || !strings.Contains(string(b), "home fallback") {
		t.Fatalf("homedir fallback: %v %q", err, string(b))
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
