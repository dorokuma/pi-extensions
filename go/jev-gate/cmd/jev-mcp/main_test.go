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
// gate inside handleRPC must call the shared jevgate S1–S4 resolver, not an
// inline variant. The subtests dispatch real tools/list requests through
// handleRPC (not a stubbed resolveAgentIdentity) and assert the gate's
// observable behaviour, with every host-environment input explicitly
// overridden so nothing leaks in from the test process's own environment.
func TestIdentityResolverIsSharedCore(t *testing.T) {
	// clearIdentityInputs removes every environment input the identity gate
	// could read from this process so a test asserts its own setup only.
	clearIdentityInputs := func(t *testing.T) {
		t.Helper()
		for _, name := range os.Environ() {
			key, _, _ := strings.Cut(name, "=")
			switch {
			case strings.HasPrefix(key, "PIG_EXT_"), key == "CTXMODE_ROLE",
				key == "PI_SUBAGENT", key == "PI_ROLE", key == "HERDR_PANE_ID",
				key == "PIG_HARNESS_ARGV_FILE":
				t.Setenv(key, "")
			}
		}
	}
	toolsListTools := func(t *testing.T, messageID int) []any {
		t.Helper()
		stubSchemaDirs(t)
		reply := dispatchOne(t, map[string]any{"jsonrpc": "2.0", "id": messageID, "method": "tools/list"})
		result, _ := reply["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		return tools
	}
	t.Run("ctxmode_role_is_denied", func(t *testing.T) {
		clearIdentityInputs(t)
		t.Setenv("CTXMODE_ROLE", "worker")
		if tools := toolsListTools(t, 41); len(tools) != 0 {
			t.Fatalf("CTXMODE_ROLE request must be denied by the real gate, tools=%v", tools)
		}
		isSubagent, reason := jevgate.ResolveMCPAgentIdentity()
		if !isSubagent || !strings.Contains(reason, "CTXMODE_ROLE") {
			t.Fatalf("shared resolver must deny CTXMODE_ROLE: %v %q", isSubagent, reason)
		}
	})
	t.Run("non_p1_pane_is_denied_before_socket", func(t *testing.T) {
		clearIdentityInputs(t)
		t.Setenv("HERDR_PANE_ID", "w66:p8")
		if tools := toolsListTools(t, 42); len(tools) != 0 {
			t.Fatalf("non-p1 pane request must be denied before the socket, tools=%v", tools)
		}
		isSubagent, reason := jevgate.ResolveMCPAgentIdentity()
		if !isSubagent || !strings.Contains(reason, `no ":p1" suffix`) {
			t.Fatalf("shared resolver must require :p1: %v %q", isSubagent, reason)
		}
	})
	t.Run("missing_pane_is_denied", func(t *testing.T) {
		clearIdentityInputs(t)
		if tools := toolsListTools(t, 43); len(tools) != 0 {
			t.Fatalf("missing-pane request must be denied, tools=%v", tools)
		}
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

// TestHelperProcessMain is not a test: it is the real-entrypoint half of
// TestLazySchemaNotWrittenWithoutToolsList, re-executed as a real OS process
// so the no-write contract is observed through main() itself (stdin frames →
// readLoop → EOF), not through handleRPC alone. Before calling main() it
// points jevSchemaDirs at the temp dir the parent injected via
// JEV_MAIN_HELPER_SCHEMA_DIR (same family as stubSchemaDirs): the helper can
// never touch — or even read — the two production mcp dirs, whatever the
// code under test decides to write.
func TestHelperProcessMain(t *testing.T) {
	if os.Getenv("JEV_MAIN_HELPER") != "1" {
		return
	}
	marker := os.Getenv("JEV_MAIN_HELPER_MARKER")
	schemaDir := os.Getenv("JEV_MAIN_HELPER_SCHEMA_DIR")
	if marker == "" || schemaDir == "" {
		os.Exit(2)
	}
	jevSchemaDirs = []string{schemaDir}
	// Redirect stdout so the ping reply cannot pollute the test's own output;
	// record the schema-dir state both before and after main() ran. A ReadDir
	// error is not an empty dir: it is recorded and fails the helper so the
	// parent never mistakes an unreadable dir for "nothing written".
	dumpState := func(stage string) {
		target := marker + "." + stage
		var lines []string
		for _, dir := range jevSchemaDirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				_ = os.WriteFile(target+".error", []byte(err.Error()), 0o600)
				os.Exit(4)
			}
			for _, entry := range entries {
				lines = append(lines, filepath.Join(dir, entry.Name()))
			}
		}
		_ = os.WriteFile(target, []byte(strings.Join(lines, "\n")), 0o600)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		os.Exit(3)
	}
	os.Stdout = devnull
	dumpState("before")
	main()
	dumpState("after")
	os.Exit(0)
}

// TestLazySchemaNotWrittenWithoutToolsList drives the real startup path: a
// jev-mcp process that answers a ping and reaches EOF (the way agy would
// spawn and probe it) must leave the schema dirs untouched — no startup
// write. The helper is the test binary itself re-executed as main(); the
// parent asserts the before/after state is empty.
func TestLazySchemaNotWrittenWithoutToolsList(t *testing.T) {
	dir := t.TempDir()
	schemaDir := t.TempDir()
	marker := filepath.Join(dir, "state")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessMain$")
	cmd.Env = append(os.Environ(),
		"JEV_MAIN_HELPER=1",
		"JEV_MAIN_HELPER_MARKER="+marker,
		// The helper redirects jevSchemaDirs at this temp dir before calling
		// main(): even if the code under test regains a startup write, it can
		// only write here — never the production mcp dirs.
		"JEV_MAIN_HELPER_SCHEMA_DIR="+schemaDir,
		// Production-looking identity inputs: the gate denies everything, so
		// even a tools/list inside this process would be the denied branch.
		"CTXMODE_ROLE=verifier",
	)
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper main() failed: %v\n%s", err, output)
	}
	for _, stage := range []string{"before", "after"} {
		data, err := os.ReadFile(marker + "." + stage)
		if err != nil {
			t.Fatalf("helper did not record the %s state: %v", stage, err)
		}
		if names := strings.TrimSpace(string(data)); names != "" {
			t.Fatalf("schema dirs must stay untouched on the real startup path, saw %q (%s)", names, stage)
		}
	}
	// The production constants stay what they are (main() cannot have drifted
	// into asserting a stub); the helper alone redirected them.
	if len(jevSchemaDirs) != 2 || !strings.HasSuffix(jevSchemaDirs[0], "antigravity-cli/mcp/jev") {
		t.Fatalf("jevSchemaDirs = %v, want the production mcp/jev dirs", jevSchemaDirs)
	}
	// The injected dir itself stays empty too: the real startup path wrote
	// nothing even at its (redirected) destination.
	if entries, err := os.ReadDir(schemaDir); err != nil || len(entries) != 0 {
		t.Fatalf("injected schema dir must stay empty on the real startup path: entries=%v err=%v", entries, err)
	}
}

// TestInFlightDroppedBeforeResponseVisible pins Q4: a tools/call request id
// is removed from the in-flight table before the response bytes reach the
// client, so "client sees the response ⇒ id is reusable" always holds. The
// controlled writer pauses mid-Write — at that instant the response is
// already visible — and asserts the id is gone. The deferred cleanup stays
// idempotent: it only deletes the entry while it still points at this call,
// so it cannot remove a newer registration that reused the freed id.
func TestInFlightDroppedBeforeResponseVisible(t *testing.T) {
	stubIdentity(t, false, "S1-S4 hold")
	stubSchemaDirs(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"clef","answers":{"kind":{"choice":"normal_delivery","probabilities":{"normal_delivery":0.95},"confidence":0.9},"states_conclusion":{"type":"noul","noul":0.1}}}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("MAGPIE_BASE_URL", server.URL)
	t.Setenv("MAGPIE_API_KEY", "test-key")

	visible := make(chan struct{})
	releaseWrite := make(chan struct{})
	spy := &spyBlockingWriter{onWrite: func() {
		select {
		case <-visible:
		default:
			close(visible)
		}
		<-releaseWrite
	}}
	writer := &responseWriter{out: spy}
	message := map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "jev_check", "arguments": map[string]any{"task": "return_triage", "payload": "x"}},
	}
	handleRPC(writer, message, "line")
	select {
	case <-visible:
	case <-time.After(15 * time.Second):
		t.Fatal("response never became visible within 15s")
	}
	key := requestKey(message["id"])
	inFlightMu.Lock()
	_, stillRegistered := inFlight[key]
	inFlightMu.Unlock()
	if stillRegistered {
		close(releaseWrite)
		t.Fatal("response is visible to the client but id 9 is still registered in-flight")
	}
	close(releaseWrite)
	inFlightWg.Wait()
	// A fresh registration under the same id succeeds and the stale deferred
	// drop cannot remove it (it only deletes entries that still point at the
	// old call object).
	stale := &inFlightCall{cancel: func() {}}
	if !registerInFlight(key, stale) {
		t.Fatal("id 9 must be reusable after the first call completed")
	}
	dropInFlight(key, &inFlightCall{cancel: func() {}}) // wrong call pointer: no-op
	inFlightMu.Lock()
	_, kept := inFlight[key]
	inFlightMu.Unlock()
	if !kept {
		t.Fatal("a stale drop must not delete a newer registration under the same id")
	}
	dropInFlight(key, stale)
}

// spyBlockingWriter blocks inside Write so the test can observe the instant
// the response is client-visible but the Write call has not returned. For
// the schema-ordering test the onWrite hook instead inspects the schema dir
// from inside Write — the instant the response bytes become visible.
type spyBlockingWriter struct {
	written bytes.Buffer
	onWrite func()
}

func (s *spyBlockingWriter) Write(p []byte) (int, error) {
	n, err := s.written.Write(p)
	if s.onWrite != nil {
		s.onWrite()
	}
	return n, err
}

// TestSchemaFilesWrittenBeforeToolsListResponse pins Q5: an allowed
// tools/list finishes publishing the lazy schema before the response bytes
// become visible. The spy writer inspects the injected schema dir from
// inside Write — at that instant both files must already exist and parse.
func TestSchemaFilesWrittenBeforeToolsListResponse(t *testing.T) {
	stubIdentity(t, false, "S1-S4 hold")
	schemaDir := stubSchemaDirs(t)
	spy := &spyBlockingWriter{onWrite: func() {
		for _, name := range []string{"jev_check.json", "instructions.md"} {
			data, err := os.ReadFile(filepath.Join(schemaDir, name))
			if err != nil {
				t.Errorf("response visible but %s is missing: %v", name, err)
				continue
			}
			if name == "jev_check.json" {
				var schema map[string]any
				if err := json.Unmarshal(data, &schema); err != nil || schema["name"] != "jev_check" {
					t.Errorf("response visible but %s is not a legal schema doc: %v", name, err)
				}
			}
		}
	}}
	writer := &responseWriter{out: spy}
	handleRPC(writer, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list"}, "line")
	if spy.written.Len() == 0 {
		t.Fatal("tools/list produced no response")
	}
	// The denied branch still writes nothing and gets an empty list.
	stubIdentity(t, true, "S1: HERDR_PANE_ID unset or empty")
	deniedDir := stubSchemaDirs(t)
	spy.onWrite = func() {
		if entries, err := os.ReadDir(deniedDir); err == nil && len(entries) != 0 {
			t.Errorf("denied tools/list must not publish schema files, saw %v", entries)
		}
	}
	handleRPC(writer, map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/list"}, "line")
	if entries, err := os.ReadDir(deniedDir); err != nil || len(entries) != 0 {
		t.Fatalf("denied tools/list wrote schema files: entries=%v err=%v", entries, err)
	}
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
	defer dropInFlight(requestKey(float64(1)), callA)
	callB := &inFlightCall{cancel: func() {}}
	if !registerInFlight(requestKey("1"), callB) {
		t.Fatal("a string id equal in text to a numeric id must register independently")
	}
	defer dropInFlight(requestKey("1"), callB)
	if registerInFlight(requestKey(float64(1)), &inFlightCall{cancel: func() {}}) {
		t.Fatal("a true duplicate in-flight id must be refused")
	}
}

// TestDrainFrameBound pins the complete-line and newline-less branches of
// the frame bound (Q6): the raw JSON body may be at most maxFrameBytes and
// the LF or CRLF terminator never counts toward it.
func TestDrainFrameBound(t *testing.T) {
	// A ping request padded to exactly maxFrameBytes: accepted and answered
	// in line framing, both under an LF and under a CRLF terminator.
	fixedPrefix := `{"jsonrpc":"2.0","id":"big","method":"ping","params":{"pad":"`
	fixedSuffix := `"}}`
	padding := strings.Repeat("x", maxFrameBytes-len(fixedPrefix)-len(fixedSuffix))
	frame := []byte(fixedPrefix + padding + fixedSuffix)
	if len(frame) != maxFrameBytes {
		t.Fatalf("fixture frame = %d bytes, want exactly %d", len(frame), maxFrameBytes)
	}
	for _, terminator := range [][]byte{{'\n'}, {'\r', '\n'}} {
		buffer := &lockedBuffer{}
		writer := &responseWriter{out: buffer}
		rest := drain(append(append([]byte{}, frame...), terminator...), writer)
		if len(rest) != 0 {
			t.Fatalf("accepted frame (term %q) must be fully consumed, rest=%d bytes", terminator, len(rest))
		}
		if out := buffer.String(); !strings.Contains(out, `"big"`) || strings.Contains(out, "Frame too large") {
			t.Fatalf("a maxFrameBytes body + %q must be accepted, got %q", terminator, out)
		}
	}

	// One byte more than maxFrameBytes before the terminator: refused before
	// dispatch and the buffered tail is dropped.
	for _, input := range [][]byte{
		append(bytes.Repeat([]byte("x"), maxFrameBytes+1), '\n'),
		append(bytes.Repeat([]byte("x"), maxFrameBytes+2), '\r', '\n'),
	} {
		buffer := &lockedBuffer{}
		writer := &responseWriter{out: buffer}
		rest := drain(input, writer)
		if len(rest) != 0 {
			t.Fatalf("oversized frame must drop the buffered tail, rest=%d bytes", len(rest))
		}
		var reply map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(buffer.String())), &reply); err != nil {
			t.Fatalf("rejection reply is not JSON: %v (%q)", err, buffer.String())
		}
		rpcErr, _ := reply["error"].(map[string]any)
		if code, _ := rpcErr["code"].(float64); code != -32700 || rpcErr["message"] != "Frame too large" {
			t.Fatalf("oversized frame must be refused with -32700 Frame too large, got %v", reply)
		}
	}

	// A maxFrameBytes body + CR is exactly the boundary buffer: it waits for
	// the LF instead of rejecting (a legal CRLF half), and the following LF
	// completes the frame.
	buffer := &lockedBuffer{}
	writer := &responseWriter{out: buffer}
	rest := drain(append(append([]byte{}, frame...), '\r'), writer)
	if len(rest) != maxFrameBytes+1 {
		t.Fatalf("body+CR must wait for the LF, rest=%d bytes", len(rest))
	}
	if strings.Contains(buffer.String(), "Frame too large") {
		t.Fatalf("body+CR is not yet a rejection: %q", buffer.String())
	}
	rest = drain(append(rest, '\n'), writer)
	if len(rest) != 0 || !strings.Contains(buffer.String(), `"big"`) {
		t.Fatalf("the pending CR must complete a CRLF frame, rest=%d out=%q", len(rest), buffer.String())
	}

	// A maxFrameBytes+1 buffer whose last byte is NOT a CR is rejected
	// immediately — no waiting on a terminator that can only make it worse.
	buffer2 := &lockedBuffer{}
	writer2 := &responseWriter{out: buffer2}
	rest = drain(bytes.Repeat([]byte("x"), maxFrameBytes+1), writer2)
	if len(rest) != 0 || !strings.Contains(buffer2.String(), "Frame too large") {
		t.Fatalf("maxFrameBytes+1 non-CR buffer must be refused, rest=%d out=%q", len(rest), buffer2.String())
	}

	// An accepted CRLF frame followed by more bytes: the trailing bytes stay
	// buffered for the next frame.
	buffer3 := &lockedBuffer{}
	writer3 := &responseWriter{out: buffer3}
	rest = drain([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\r\n"+`{"jsonrpc":"2.0"`), writer3)
	if !strings.Contains(buffer3.String(), `"id":1`) {
		t.Fatalf("first CRLF frame must dispatch, out=%q", buffer3.String())
	}
	if string(rest) != `{"jsonrpc":"2.0"` {
		t.Fatalf("trailing partial frame must stay buffered, rest=%q", rest)
	}
}

// chunkedReader feeds a fixed script of chunks, then io.EOF — it stands in
// for a pipe where a frame straddles several reads and a CR arrives alone.
// Each Read returns at most len(p) bytes of the current chunk; the
// unconsumed remainder is delivered by the next Read.
type chunkedReader struct {
	chunks [][]byte
	index  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	for r.index < len(r.chunks) && len(r.chunks[r.index]) == 0 {
		r.index++
	}
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.chunks[r.index] = r.chunks[r.index][n:]
	return n, nil
}

// TestReadLoopFrameBounds pins the same contract through the real readLoop
// (the 64KiB read path): 16MiB bodies under LF/CRLF and a frame split across
// body / CR / LF chunks all answer with the right id; the oversized shapes
// are refused.
func TestReadLoopFrameBounds(t *testing.T) {
	fixedPrefix := `{"jsonrpc":"2.0","id":"big","method":"ping","params":{"pad":"`
	fixedSuffix := `"}}`
	padding := strings.Repeat("x", maxFrameBytes-len(fixedPrefix)-len(fixedSuffix))
	frame := []byte(fixedPrefix + padding + fixedSuffix)

	run := func(t *testing.T, chunks ...[]byte) string {
		t.Helper()
		buffer := &lockedBuffer{}
		readLoop(&chunkedReader{chunks: chunks}, &responseWriter{out: buffer})
		return buffer.String()
	}

	t.Run("body_plus_lf", func(t *testing.T) {
		out := run(t, append(append([]byte{}, frame...), '\n'))
		if !strings.Contains(out, `"big"`) || strings.Contains(out, "Frame too large") {
			t.Fatalf("16MiB body + LF must answer, got %q", out)
		}
	})
	t.Run("body_plus_crlf", func(t *testing.T) {
		out := run(t, append(append([]byte{}, frame...), '\r', '\n'))
		if !strings.Contains(out, `"big"`) || strings.Contains(out, "Frame too large") {
			t.Fatalf("16MiB body + CRLF must answer, got %q", out)
		}
	})
	t.Run("chunked_body_cr_lf", func(t *testing.T) {
		// The body, the CR and the LF arrive in separate reads: the pending-CR
		// boundary must hold the frame open instead of rejecting it.
		out := run(t, frame, []byte{'\r'}, []byte{'\n'})
		if !strings.Contains(out, `"big"`) || strings.Contains(out, "Frame too large") {
			t.Fatalf("chunked body/CR/LF must answer, got %q", out)
		}
	})
	t.Run("chunked_body_split_cr_lf_merged_then_more", func(t *testing.T) {
		// body+CR+LF+next-frame-start in one chunk, then the rest of a second
		// oversized frame: the first must still have answered correctly and
		// the second must be refused.
		out := run(t,
			append(append(append([]byte{}, frame...), '\r', '\n'), []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)...),
			[]byte{'\n'},
		)
		if !strings.Contains(out, `"big"`) || !strings.Contains(out, `"id":2`) {
			t.Fatalf("CRLF frame + following JSON must both be handled, got %q", out)
		}
	})
	t.Run("body_plus_one_lf_rejected", func(t *testing.T) {
		out := run(t, append(append(append([]byte{}, frame...), 'x'), '\n'))
		if strings.Contains(out, `"big"`) || !strings.Contains(out, "Frame too large") {
			t.Fatalf("16MiB+1 body + LF must be refused, got %q", out)
		}
	})
	t.Run("body_plus_one_eof_rejected", func(t *testing.T) {
		out := run(t, append(append([]byte{}, frame...), 'x'))
		if !strings.Contains(out, "Frame too large") {
			t.Fatalf("16MiB+1 unterminated body must be refused, got %q", out)
		}
	})
	t.Run("body_crlf_then_json_byte_rejected", func(t *testing.T) {
		// A full-size body + CR + a JSON byte: the buffer past maxFrameBytes+1
		// can never complete a frame, so it is refused without EOF.
		out := run(t, append(append(append([]byte{}, frame...), '\r'), '{'))
		if !strings.Contains(out, "Frame too large") {
			t.Fatalf("body+CR+byte overflow must be refused, got %q", out)
		}
	})
	t.Run("dangling_cr_at_eof_rejected", func(t *testing.T) {
		out := run(t, append(append([]byte{}, frame...), 'x', '\r'))
		if !strings.Contains(out, "Frame too large") {
			t.Fatalf("dangling CR past the bound must be refused at EOF, got %q", out)
		}
	})
	t.Run("pending_cr_hit_by_eof_rejected", func(t *testing.T) {
		// A full-size body + CR waiting for its LF: EOF turns the pending CR
		// into an over-bound tail — refused, never dispatched.
		out := run(t, frame, []byte{'\r'})
		if !strings.Contains(out, "Frame too large") {
			t.Fatalf("a pending CR cut off by EOF must be refused, got %q", out)
		}
	})
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
// goroutines of one binary. Each writer waits for the parent's readers-ready
// barrier before it writes anything, publishes complete stale-schema
// documents, drops a started marker after its first successful write, and
// drops a done marker carrying its PID when it finishes — the started marker
// opens the parent's admission window, the done markers bound each reader's
// measured phase, before every writeFileAtomic call the writer drops a
// write-begin marker (target + ".wb.<writer>.<round>") and right after it
// succeeds a write-end marker (target + ".we.<writer>.<round>") so a parent
// reader can prove a write started inside one read's lifetime bracket while
// write activity was still live (a completion had reached the filesystem by
// that bracket's end).
func TestHelperProcessSchemaWrite(t *testing.T) {
	if os.Getenv("JEV_SCHEMA_WRITE_HELPER") != "1" {
		return
	}
	target := os.Getenv("JEV_SCHEMA_WRITE_TARGET")
	writer := os.Getenv("JEV_SCHEMA_WRITE_WRITER")
	readyGate := os.Getenv("JEV_SCHEMA_WRITE_READERS_READY")
	rounds, _ := strconv.Atoi(os.Getenv("JEV_SCHEMA_WRITE_ROUNDS"))
	if target == "" || readyGate == "" || rounds <= 0 {
		os.Exit(2)
	}
	// Writer barrier: do not enter the write phase until the parent reports
	// every reader goroutine armed. Bounded so a wedged parent can never pin
	// the child (and the child's own Wait is bounded on the parent side).
	readyDeadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(readyGate); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			os.Exit(4)
		}
		time.Sleep(time.Millisecond)
	}
	for round := 0; round < rounds; round++ {
		content, _ := json.Marshal(map[string]any{
			"name":        "jev_check",
			"description": fmt.Sprintf("stale-w%s-r%d", writer, round),
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		})
		// The write-begin marker lands BEFORE the atomic write and the write-end
		// marker lands right after it: a parent reader sees a NEW .wb.* file
		// between the wbBefore and wbAfter globs bracketing one os.ReadFile only
		// while a write began inside that bracket, and sees at least one .we.*
		// file only once a write has completed — together they keep pure
		// pre/post-phase static reads out of OVERLAP_READS. Cheap: empty files
		// are enough.
		_ = os.WriteFile(fmt.Sprintf("%s.wb.%s.%d", target, writer, round), nil, 0o600)
		if err := writeFileAtomic(target, content, 0o644); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(fmt.Sprintf("%s.we.%s.%d", target, writer, round), nil, 0o600)
		if round == 0 {
			// The first successful write is done: the target file now exists
			// and will never disappear again (rename only replaces it).
			_ = os.WriteFile(target+".started."+writer, []byte("1"), 0o600)
		}
	}
	// The done marker carries this process's real PID: the parent asserts the
	// writers were 4+ distinct OS processes, not threads of one binary.
	if err := os.WriteFile(target+".done."+writer, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

// staleSchemaDocOK reports whether data is one complete stale-schema
// document the writers publish — a valid full JSON object with the schema
// shape, not merely non-empty bytes.
func staleSchemaDocOK(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	description, hasDescription := doc["description"].(string)
	parameters, hasParameters := doc["parameters"].(map[string]any)
	return doc["name"] == "jev_check" && hasDescription &&
		strings.HasPrefix(description, "stale-") && hasParameters && parameters["type"] == "object"
}

// TestWriteFileAtomicConcurrent hammers one schema file from several real
// child processes while parent-side readers race them inside the measured
// window. The window opens at the first writer's started marker (first
// successful write) as an admission ticket only; its end is not a watcher
// notification — each reader rechecks the done markers at every completed
// read and leaves the measured phase once done == writers. A read counts
// toward OVERLAP_READS only when a NEW write-begin marker (.wb.*) appears
// inside its marker bracket (wbBefore → wbAfter globs) AND at least one
// write-end marker (.we.*) is present at that bracket's end: proof a write
// began inside the read's lifetime while write activity was still live —
// never the done-lag approximation of static post-write reads, and no
// counted read can start strictly after the last write began (no new .wb.*
// can appear inside its bracket) or be counted before the first write
// completed (no .we.* exists yet). Writers hold on a readers-ready barrier before their
// first write, so every counted read intersects the live write phase; this
// is phase-level liveness, not per-call proof that one os.ReadFile raced
// one writeFileAtomic. Every completed counted read must be a complete,
// legal stale-schema document; read errors — including ENOENT — are
// counted, and no temp file may be left behind.
func TestWriteFileAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "jev_check.json")
	readyGate := filepath.Join(dir, "readers.ready")

	const (
		writers = 4
		// The marker-diff window (wbBefore glob → ReadFile → wbAfter/we globs)
		// is microseconds-narrow and each write marker is created in an
		// instant, so only a small fraction of the write phase's reads land on
		// a marker boundary. rounds is sized so the marker stream still
		// crosses ≥100 bracketed reads under -race.
		rounds = 800
		// Reader count scales the bracket samples per unit of write phase;
		// 12 targets ≥150 OVERLAP_READS/round (~1.5× the 100 threshold).
		readers = 12
	)

	// windowStart closes when the first writer reports its first successful
	// write; it is only the readers' admission ticket — a read's overlap is
	// proven per-iteration by the write-begin/-end markers it diffs itself,
	// never by watcher timing or done-lag. windowStart also closes on procsDone (a
	// writer that died early) so a failed writer can never pin the readers
	// or the test.
	windowStart := make(chan struct{})
	procsDone := make(chan struct{})
	var onceProcsDone sync.Once
	closeProcsDone := func() { onceProcsDone.Do(func() { close(procsDone) }) }

	var watchers, wg, procs sync.WaitGroup
	var startedCmds []*exec.Cmd
	// Unified cleanup for every early-return path (t.Fatalf included),
	// registered before any background activity starts. Order matters:
	//  1. close procsDone once — unblocks the watcher and every reader even
	//     when the writers were never started or died mid-run;
	//  2. join watcher and readers so no goroutine keeps touching the temp
	//     dir t.TempDir removes;
	//  3. reap children: kill any still-running writer (idempotent once it
	//     exited — the per-writer reaper already called Wait, so we must
	//     never Wait here ourselves), then bound-join the reapers.
	defer func() {
		closeProcsDone()
		watchers.Wait()
		wg.Wait()
		for _, cmd := range startedCmds {
			_ = cmd.Process.Kill()
		}
		// The reapers are internally bounded (90s + kill), so procs.Wait
		// terminates; child Wait has already been issued by each reaper.
		procs.Wait()
	}()

	watchers.Add(1)
	go func() {
		defer watchers.Done()
		deadline := time.Now().Add(60 * time.Second)
		for {
			for i := 0; i < writers; i++ {
				if _, err := os.Stat(fmt.Sprintf("%s.started.%d", target, i)); err == nil {
					close(windowStart)
					return
				}
			}
			select {
			case <-procsDone:
				close(windowStart)
				return
			default:
			}
			if time.Now().After(deadline) {
				close(windowStart) // open anyway: the assertions below decide
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var overlapReads, emptyReads, parseErrors, schemaInvalid, readErrors atomic.Int64
	var readersReady sync.WaitGroup
	readersReady.Add(readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readersReady.Done()
			<-windowStart
			for {
				// A writer that died without its done marker must never pin a
				// reader; the done==writers exit below is the normal way out.
				select {
				case <-procsDone:
					return
				default:
				}
				// Bracket the read with the write markers: only a NEW .wb.* file
				// appearing between wbBefore and wbAfter proves a write began inside
				// this read's lifetime bracket, and at least one .we.* file present
				// at the bracket's end proves write activity had already produced a
				// completion — the read therefore cannot be a pure pre-first-write
				// or post-last-write static read. Both conditions are what
				// OVERLAP_READS counts (phase-level liveness: a write began in the
				// bracket while write activity was live; not per-call proof that
				// this os.ReadFile raced one writeFileAtomic).
				wbBefore, _ := filepath.Glob(target + ".wb.*")
				data, err := os.ReadFile(target)
				wbAfter, _ := filepath.Glob(target + ".wb.*")
				weNow, _ := filepath.Glob(target + ".we.*")
				// Classify THIS read before checking the done markers: a read that
				// completed while the last writers finished must still land in its
				// bucket (error / overlap+content check / uncounted) instead of
				// escaping classification when done == writers happens to go up
				// between the snapshots above and the check below.
				if err != nil {
					// Any error — ENOENT included — is a visibility violation:
					// inside the write phase the file must always exist and be
					// whole. Errors are not overlap evidence; they classify as
					// READ_ERRORS regardless of the wb/we diff.
					readErrors.Add(1)
				} else if len(wbAfter) <= len(wbBefore) || len(weNow) == 0 {
					// No write began inside this read's bracket — or none had
					// completed by its end — so it is a static read of a stable
					// file, not overlap.
				} else {
					overlapReads.Add(1)
					switch {
					case len(data) == 0:
						emptyReads.Add(1)
					default:
						var doc map[string]any
						if err := json.Unmarshal(data, &doc); err != nil {
							parseErrors.Add(1)
						} else if !staleSchemaDocOK(data) {
							schemaInvalid.Add(1)
						}
					}
				}
				// The measured phase ends when every writer has dropped its done
				// marker; the check runs AFTER this read's classification so the
				// final in-flight reads are counted, never silently skipped.
				doneMarkers, _ := filepath.Glob(target + ".done.*")
				if len(doneMarkers) == writers {
					return
				}
			}
		}()
	}

	// Readers armed: release the writers' start barrier. The barrier file
	// being a regular file in the same temp dir keeps the protocol on the
	// filesystem the child processes can see.
	readersReady.Wait()
	if err := os.WriteFile(readyGate, []byte("1"), 0o600); err != nil {
		t.Fatalf("write readers-ready barrier: %v", err)
	}

	pids := make(map[int]int, writers)
	var pidsMu sync.Mutex
	for i := 0; i < writers; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessSchemaWrite$")
		cmd.Env = append(os.Environ(),
			"JEV_SCHEMA_WRITE_HELPER=1",
			"JEV_SCHEMA_WRITE_TARGET="+target,
			"JEV_SCHEMA_WRITE_READERS_READY="+readyGate,
			fmt.Sprintf("JEV_SCHEMA_WRITE_WRITER=%d", i),
			fmt.Sprintf("JEV_SCHEMA_WRITE_ROUNDS=%d", rounds),
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start writer %d: %v", i, err)
		}
		startedCmds = append(startedCmds, cmd)
		pidsMu.Lock()
		pids[cmd.Process.Pid] = i
		pidsMu.Unlock()
		procs.Add(1)
		go func(cmd *exec.Cmd, i int) {
			defer procs.Done()
			// Bounded reap: a wedged child is killed after the deadline so the
			// test can never hang; the kill turns into an error either way.
			waitDone := make(chan error, 1)
			go func() { waitDone <- cmd.Wait() }()
			select {
			case err := <-waitDone:
				if err != nil {
					t.Errorf("writer %d exited with error: %v", i, err)
				}
			case <-time.After(90 * time.Second):
				_ = cmd.Process.Kill()
				<-waitDone
				t.Errorf("writer %d exceeded the reap deadline and was killed", i)
			}
		}(cmd, i)
	}

	procs.Wait()
	// Same close the deferred cleanup performs on early exits — the Once
	// makes the two paths share one close, never a double-close panic.
	closeProcsDone()
	// The readers bound the measured phase themselves: each exits on its own
	// per-iteration done==writers check (or the procsDone failsafe). Join the
	// started-marker watcher and the readers.
	watchers.Wait()
	wg.Wait()

	if got := len(pids); got != writers {
		t.Fatalf("expected %d distinct writer PIDs, saw %d (%v)", writers, got, pids)
	}
	for i := 0; i < writers; i++ {
		// Every writer must have completed at least one real write (started
		// marker) and its full round budget (done marker + exit 0, asserted
		// by the reaper above).
		if _, err := os.Stat(fmt.Sprintf("%s.started.%d", target, i)); err != nil {
			t.Fatalf("writer %d never completed a write: %v", i, err)
		}
		data, err := os.ReadFile(fmt.Sprintf("%s.done.%d", target, i))
		if err != nil {
			t.Fatalf("writer %d left no done marker: %v", i, err)
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || pid <= 0 {
			t.Fatalf("writer %d done marker carries no real PID: %q", i, data)
		}
	}
	t.Logf("OVERLAP_READS=%d (reads whose bracket gained a write-begin marker while a write-end marker proved live write activity)", overlapReads.Load())
	if overlapReads.Load() < 100 {
		t.Fatalf("OVERLAP_READS=%d (<100): the window never exercised concurrent reads", overlapReads.Load())
	}
	if emptyReads.Load() != 0 || parseErrors.Load() != 0 || schemaInvalid.Load() != 0 || readErrors.Load() != 0 {
		t.Fatalf("EMPTY=%d PARSE_ERRORS=%d SCHEMA_INVALID=%d READ_ERRORS=%d (all must be 0)",
			emptyReads.Load(), parseErrors.Load(), schemaInvalid.Load(), readErrors.Load())
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
