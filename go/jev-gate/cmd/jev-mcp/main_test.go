package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// lockedBuffer is a writer the async tools/call worker can share with the
// test goroutine safely.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// dispatchOne parses the single JSON-RPC response a message produces. Replies
// can arrive asynchronously (tools/call runs on its own goroutine), so it
// waits until the writer produced a line.
func dispatchOne(t *testing.T, message map[string]any) map[string]any {
	t.Helper()
	buffer := &lockedBuffer{}
	writer := &responseWriter{out: buffer}
	handleRPC(writer, message, "line")
	deadline := time.Now().Add(10 * time.Second)
	for strings.TrimSpace(buffer.String()) == "" {
		if time.Now().After(deadline) {
			t.Fatal("no response written within 10s")
		}
		time.Sleep(time.Millisecond)
	}
	line := strings.TrimSpace(buffer.String())
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

// stubSchemaDirs redirects the lazy schema write targets to a test temp dir
// so a tools/list allow path can never touch the production schema dirs
// (reviewer must fix: the gate test used to write the real ~/.gemini dirs).
func stubSchemaDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := jevSchemaDirs
	jevSchemaDirs = []string{dir}
	t.Cleanup(func() { jevSchemaDirs = old })
	return dir
}

// TestToolsListGate covers the per-request identity gate on tools/list.
func TestToolsListGate(t *testing.T) {
	t.Run("main_agent_gets_jev_check", func(t *testing.T) {
		stubIdentity(t, false, "S1-S4 hold")
		schemaDir := stubSchemaDirs(t)
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
		// The allowed tools/list is the one path that writes the lazy schema
		// files — and it writes only into the injected dir.
		for _, name := range []string{"jev_check.json", "instructions.md"} {
			if _, err := os.Stat(filepath.Join(schemaDir, name)); err != nil {
				t.Fatalf("expected %s written under the injected schema dir: %v", name, err)
			}
		}
	})
	t.Run("subagent_gets_empty_list", func(t *testing.T) {
		stubIdentity(t, true, "S1: HERDR_PANE_ID unset or empty")
		schemaDir := stubSchemaDirs(t)
		reply := dispatchOne(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
		result, _ := reply["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		if len(tools) != 0 {
			t.Fatalf("subagent tools = %v", result["tools"])
		}
		// A denied tools/list must not write the schema files either.
		if entries, err := os.ReadDir(schemaDir); err != nil || len(entries) != 0 {
			t.Fatalf("denied tools/list must not write schema files: entries=%v err=%v", entries, err)
		}
	})
}

// TestLazySchemaNotWrittenWithoutToolsList pins the lazy schema write: a
// server that answers anything but an allowed tools/list leaves the schema
// dirs untouched (the startup write is gone — a denied subagent's binary
// must not write the global schema).
func TestLazySchemaNotWrittenWithoutToolsList(t *testing.T) {
	t.Run("ping_writes_nothing", func(t *testing.T) {
		stubIdentity(t, false, "S1-S4 hold")
		schemaDir := stubSchemaDirs(t)
		reply := dispatchOne(t, map[string]any{"jsonrpc": "2.0", "id": 11, "method": "ping"})
		if _, ok := reply["result"]; !ok {
			t.Fatalf("ping reply = %v", reply)
		}
		if entries, err := os.ReadDir(schemaDir); err != nil || len(entries) != 0 {
			t.Fatalf("schema dir must stay untouched without tools/list: entries=%v err=%v", entries, err)
		}
	})
	t.Run("denied_tools_call_writes_nothing", func(t *testing.T) {
		stubIdentity(t, true, "S4: nested second host runtime ancestor")
		schemaDir := stubSchemaDirs(t)
		reply := dispatchOne(t, map[string]any{
			"jsonrpc": "2.0", "id": 12, "method": "tools/call",
			"params": map[string]any{"name": "jev_check", "arguments": map[string]any{"task": "gate_scan", "payload": "x"}},
		})
		result, _ := reply["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("denial must be an error result: %v", result)
		}
		if entries, err := os.ReadDir(schemaDir); err != nil || len(entries) != 0 {
			t.Fatalf("denied tools/call must not write schema files: entries=%v err=%v", entries, err)
		}
	})
}

// TestDuplicateInFlightRequestID pins F-3: two concurrent tools/call sharing
// one id — the second is refused with a duplicate error result and the first
// still completes normally (registration, cancellation and cleanup stay
// one-to-one).
func TestDuplicateInFlightRequestID(t *testing.T) {
	stubIdentity(t, false, "S1-S4 hold")
	stubSchemaDirs(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-release
		writer.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"clef","answers":{"kind":{"choice":"normal_delivery","probabilities":{"normal_delivery":0.95,"provider_error":0.02},"confidence":0.9},"states_conclusion":{"type":"noul","noul":0.1}},"usage":{"input_tokens":120,"output_tokens":8}}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("MAGPIE_BASE_URL", server.URL)
	t.Setenv("MAGPIE_API_KEY", "test-key")

	buffer := &lockedBuffer{}
	writer := &responseWriter{out: buffer}
	message := map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "jev_check", "arguments": map[string]any{"task": "return_triage", "payload": "x"}},
	}
	// Both calls are dispatched while the HTTP round-trip is still blocked,
	// so id 7 is genuinely in flight when the second register is attempted.
	handleRPC(writer, message, "line")
	handleRPC(writer, message, "line")
	close(release)
	deadline := time.Now().Add(30 * time.Second)
	var duplicateReply, normalReply map[string]any
	for duplicateReply == nil || normalReply == nil {
		if time.Now().After(deadline) {
			t.Fatalf("expected one duplicate error and one normal result, got: %s", buffer.String())
		}
		for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
			if line == "" {
				continue
			}
			var reply map[string]any
			if err := json.Unmarshal([]byte(line), &reply); err != nil {
				t.Fatalf("reply %q is not JSON: %v", line, err)
			}
			result, _ := reply["result"].(map[string]any)
			text := resultText(result)
			switch {
			case strings.Contains(text, "duplicate in-flight request id"):
				duplicateReply = reply
			case strings.Contains(text, "verdict:"):
				normalReply = reply
			}
		}
		time.Sleep(time.Millisecond)
	}
	if normalReply == nil {
		t.Fatal("the first id=7 call must complete normally")
	}
}

// resultText extracts the text of a tools/call result's first content entry.
func resultText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	entry, _ := content[0].(map[string]any)
	text, _ := entry["text"].(string)
	return text
}

// TestRequestKeyTypeTags pins the typed in-flight keys of F-3: JSON-RPC ids
// 1 and "1" are distinct ids and must never collide on one map entry.
func TestRequestKeyTypeTags(t *testing.T) {
	if requestKey(float64(1)) == requestKey("1") {
		t.Fatal("numeric id 1 and string id \"1\" must map to distinct keys")
	}
	if got := requestKey("1"); !strings.HasPrefix(got, "s:") || !strings.HasSuffix(got, "1") {
		t.Fatalf("string key = %q", got)
	}
	callA := &inFlightCall{cancel: func() {}}
	if !registerInFlight(requestKey(float64(1)), callA) {
		t.Fatal("first registration must succeed")
	}
	defer dropInFlight(requestKey(float64(1)))
	if !registerInFlight(requestKey("1"), &inFlightCall{cancel: func() {}}) {
		t.Fatal("a string id equal in text to a numeric id must register independently")
	}
	defer dropInFlight(requestKey("1"))
	if registerInFlight(requestKey(float64(1)), &inFlightCall{cancel: func() {}}) {
		t.Fatal("a true duplicate in-flight id must be refused")
	}
}

// TestDrainFrameBound pins the complete-line branch of the frame bound: a
// line frame whose body fits maxFrameBytes is dispatched, one byte more is
// refused before dispatch with -32700 Frame too large.
func TestDrainFrameBound(t *testing.T) {
	// A ping request padded to exactly maxFrameBytes: accepted and answered
	// in line framing.
	fixedPrefix := `{"jsonrpc":"2.0","id":"big","method":"ping","params":{"pad":"`
	fixedSuffix := `"}}`
	padding := strings.Repeat("x", maxFrameBytes-len(fixedPrefix)-len(fixedSuffix))
	frame := []byte(fixedPrefix + padding + fixedSuffix)
	if len(frame) != maxFrameBytes {
		t.Fatalf("fixture frame = %d bytes, want exactly %d", len(frame), maxFrameBytes)
	}
	buffer := &lockedBuffer{}
	writer := &responseWriter{out: buffer}
	rest := drain(append(frame, '\n'), writer)
	if len(rest) != 0 {
		t.Fatalf("accepted frame must be fully consumed, rest=%d bytes", len(rest))
	}
	if out := buffer.String(); !strings.Contains(out, `"big"`) || strings.Contains(out, "Frame too large") {
		t.Fatalf("a maxFrameBytes body + newline must be accepted, got %q", out)
	}

	// One byte more than maxFrameBytes before the newline: refused before
	// dispatch and the buffered tail is dropped.
	buffer2 := &lockedBuffer{}
	writer2 := &responseWriter{out: buffer2}
	oversized := bytes.Repeat([]byte("x"), maxFrameBytes+1)
	rest = drain(append(oversized, '\n'), writer2)
	if len(rest) != 0 {
		t.Fatalf("oversized frame must drop the buffered tail, rest=%d bytes", len(rest))
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buffer2.String())), &reply); err != nil {
		t.Fatalf("rejection reply is not JSON: %v (%q)", err, buffer2.String())
	}
	rpcErr, _ := reply["error"].(map[string]any)
	if code, _ := rpcErr["code"].(float64); code != -32700 || rpcErr["message"] != "Frame too large" {
		t.Fatalf("oversized frame must be refused with -32700 Frame too large, got %v", reply)
	}
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

// TestHelperProcessSchemaWrite is not a test: it is the writer half of
// TestWriteFileAtomicConcurrent, re-executed as a real OS process so the
// atomicity claim is proven across processes (rename(2)), not just across
// goroutines of one binary.
func TestHelperProcessSchemaWrite(t *testing.T) {
	if os.Getenv("JEV_SCHEMA_WRITE_HELPER") != "1" {
		return
	}
	target := os.Getenv("JEV_SCHEMA_WRITE_TARGET")
	writer := os.Getenv("JEV_SCHEMA_WRITE_WRITER")
	rounds, _ := strconv.Atoi(os.Getenv("JEV_SCHEMA_WRITE_ROUNDS"))
	if target == "" || rounds <= 0 {
		os.Exit(2)
	}
	for round := 0; round < rounds; round++ {
		content, _ := json.Marshal(map[string]any{
			"name":   "jev_check",
			"writer": writer,
			"round":  round,
		})
		if err := writeFileAtomic(target, content, 0o644); err != nil {
			os.Exit(3)
		}
	}
	os.Exit(0)
}

// TestWriteFileAtomicConcurrent hammers one schema file from several real
// child processes while parent-side readers race them. Every completed read
// must be a non-empty, fully-parseable JSON object, and no temp file may be
// left behind.
func TestWriteFileAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "jev_check.json")

	const (
		writers = 4
		rounds  = 15
		readers = 4
	)

	// Seed the file so the first reader already sees valid content.
	seed, _ := json.Marshal(map[string]any{"name": "jev_check", "round": -1})
	if err := writeFileAtomic(target, seed, 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	stop := make(chan struct{})
	var emptyReads, parseErrors, readCount atomic.Int64
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(target)
				if err != nil {
					continue
				}
				readCount.Add(1)
				if len(data) == 0 {
					emptyReads.Add(1)
					continue
				}
				var parsed map[string]any
				if err := json.Unmarshal(data, &parsed); err != nil {
					parseErrors.Add(1)
				}
			}
		}()
	}

	var procs sync.WaitGroup
	for i := 0; i < writers; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessSchemaWrite$")
		cmd.Env = append(os.Environ(),
			"JEV_SCHEMA_WRITE_HELPER=1",
			"JEV_SCHEMA_WRITE_TARGET="+target,
			fmt.Sprintf("JEV_SCHEMA_WRITE_WRITER=%d", i),
			fmt.Sprintf("JEV_SCHEMA_WRITE_ROUNDS=%d", rounds),
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start writer %d: %v", i, err)
		}
		procs.Add(1)
		go func(cmd *exec.Cmd, i int) {
			defer procs.Done()
			if err := cmd.Wait(); err != nil {
				t.Errorf("writer %d exited with error: %v", i, err)
			}
		}(cmd, i)
	}
	procs.Wait()
	close(stop)
	wg.Wait()

	if emptyReads.Load() != 0 {
		t.Fatalf("readers saw %d empty reads: a torn write exposed an empty file", emptyReads.Load())
	}
	if parseErrors.Load() != 0 {
		t.Fatalf("readers saw %d JSON parse errors: a torn write exposed a partial file", parseErrors.Load())
	}
	if readCount.Load() == 0 {
		t.Fatal("readers never observed the file")
	}
	final, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	var last map[string]any
	if err := json.Unmarshal(final, &last); err != nil {
		t.Fatalf("final content is not valid JSON: %v", err)
	}
	if last["name"] != "jev_check" {
		t.Fatalf("final content = %v", last)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
}
