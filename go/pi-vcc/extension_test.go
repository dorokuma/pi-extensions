package pivcc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// The assembly layer: everything index.ts, commands/*.ts and tools/recall.ts
// wire together, driven over PiG's real length-prefixed IPC against a fake host
// (the same approach as go/ctxmode/test_helpers_test.go's sdkHost).
//
// TestMain also fences the settings path: pi-vcc scaffolds its config file at
// load time, and no test may read or write the production
// /root/.pi/agent/pi-vcc-config.json.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pi-vcc-go-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("PI_VCC_CONFIG_PATH", filepath.Join(dir, "pi-vcc-config.json")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// ── the fake host ───────────────────────────────────────────────────────────

// pivccHost is a fake PiG host: it reads the extension's register frame,
// answers host calls, and lets a test fire requests (tools, commands, events).
type pivccHost struct {
	t    *testing.T
	conn net.Conn

	writeMu sync.Mutex

	mu            sync.Mutex
	next          int
	responses     map[string]chan map[string]any
	handlers      map[string]int
	commands      map[string]map[string]any
	tools         map[string]map[string]any
	calls         []hostCall
	notifications []string

	// sessionFile is what getSessionFile answers; "" answers "no session".
	sessionFile string
	// sessionEntries are what watchSessionLog answers (the session log).
	sessionEntries []json.RawMessage
	// compactResult, when set, is what a `compact` call answers, which fires
	// the /pi-vcc command's OnComplete callback.
	compactResult map[string]any
	// compactError, when set, fails the `compact` call instead.
	compactError string

	done       chan error
	readerDone chan struct{}

	// ext is the extension the harness built, so a test can reach its state.
	ext *sdk.Extension
}

type hostCall struct {
	Method string
	Args   map[string]any
}

// newHost builds the extension over a net.Pipe and drives it as a fake host.
func newHost(t *testing.T) *pivccHost {
	t.Helper()
	hostEnd, extEnd := net.Pipe()
	h := &pivccHost{
		t:          t,
		conn:       hostEnd,
		responses:  map[string]chan map[string]any{},
		handlers:   map[string]int{},
		commands:   map[string]map[string]any{},
		tools:      map[string]map[string]any{},
		done:       make(chan error, 1),
		readerDone: make(chan struct{}),
	}
	ext := Extension()
	h.ext = ext
	go func() { h.done <- ext.RunWithConn(extEnd) }()

	register := h.readFrame()
	payload, ok := register["register"].(map[string]any)
	if !ok {
		t.Fatalf("first frame is not a register frame: %#v", register)
	}
	for _, raw := range payload["handlers"].([]any) {
		entry := raw.(map[string]any)
		h.handlers[entry["event"].(string)] = int(entry["handler_id"].(float64))
	}
	for _, raw := range payload["commands"].([]any) {
		entry := raw.(map[string]any)
		h.commands[entry["name"].(string)] = entry
	}
	for _, raw := range payload["tools"].([]any) {
		entry := raw.(map[string]any)
		h.tools[entry["name"].(string)] = entry
	}
	h.send(map[string]any{"type": "ready", "ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})
	go h.readLoop()

	t.Cleanup(func() {
		h.send(map[string]any{"type": "shutdown"})
		select {
		case err := <-h.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("SDK shutdown deadline")
		}
		_ = h.conn.Close()
		<-h.readerDone
	})
	return h
}

func (h *pivccHost) readLoop() {
	defer close(h.readerDone)
	for {
		msg, err := readPivccFrame(h.conn)
		if err != nil {
			return
		}
		switch msg["type"] {
		case "call":
			h.answerCall(msg)
		case "response":
			h.mu.Lock()
			ch := h.responses[msg["id"].(string)]
			h.mu.Unlock()
			if ch != nil {
				ch <- msg["response"].(map[string]any)
			}
		}
	}
}

// answerCall records one host call and answers it the way a real host would.
func (h *pivccHost) answerCall(msg map[string]any) {
	call := msg["call"].(map[string]any)
	method, _ := call["method"].(string)
	args, _ := call["args"].(map[string]any)
	h.mu.Lock()
	h.calls = append(h.calls, hostCall{Method: method, Args: args})
	sessionFile, entries := h.sessionFile, h.sessionEntries
	compactResult, compactError := h.compactResult, h.compactError
	h.mu.Unlock()

	if method == "ui.notify" {
		h.mu.Lock()
		h.notifications = append(h.notifications, fmt.Sprint(args))
		h.mu.Unlock()
	}

	var result any
	switch method {
	case "getSessionFile":
		if sessionFile != "" {
			result = map[string]any{"sessionFile": sessionFile}
		}
	case "watchSessionLog":
		// The host pages the log by cursor: everything from the cursor onward,
		// and nothing once the cursor reached the end (which is what ends the
		// SDK's "complete" poll).
		cursor := 0
		if value, ok := args["cursor"].(float64); ok {
			cursor = int(value)
		}
		page := []json.RawMessage{}
		if cursor < len(entries) {
			page = entries[cursor:]
		}
		leafID := ""
		if len(entries) > 0 {
			var last struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(entries[len(entries)-1], &last)
			leafID = last.ID
		}
		result = map[string]any{
			"entries":    page,
			"entryCount": len(entries),
			"hasMore":    false,
			"leafId":     leafID,
		}
	case "compact":
		if compactError != "" {
			h.send(map[string]any{"type": "call_result", "id": msg["id"], "call_result": map[string]any{"error": map[string]any{"message": compactError}}})
			return
		}
		result = compactResult
	default:
		result = nil
	}
	h.send(map[string]any{"type": "call_result", "id": msg["id"], "call_result": map[string]any{"result": result}})
}

func (h *pivccHost) send(msg map[string]any) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		h.t.Error(err)
		return
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := h.conn.Write(append(header[:], data...)); err != nil {
		h.t.Error(err)
	}
}

func (h *pivccHost) readFrame() map[string]any {
	h.t.Helper()
	msg, err := readPivccFrame(h.conn)
	if err != nil {
		h.t.Fatal(err)
	}
	return msg
}

// request sends one request and returns the channel its response arrives on.
func (h *pivccHost) request(method, name string, args any) (string, <-chan map[string]any) {
	h.mu.Lock()
	h.next++
	id := strconv.Itoa(h.next)
	ch := make(chan map[string]any, 1)
	h.responses[id] = ch
	h.mu.Unlock()
	req := map[string]any{"method": method, "args": args}
	if method == "event" {
		req["event"] = name
		req["handler_id"] = h.handlers[name]
	} else {
		req["tool"] = name
	}
	h.send(map[string]any{"type": "request", "id": id, "request": req})
	return id, ch
}

func (h *pivccHost) await(ch <-chan map[string]any) map[string]any {
	h.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		h.t.Fatal("SDK response deadline")
		return nil
	}
}

// event fires one registered event handler with data.
func (h *pivccHost) event(name string, data map[string]any) map[string]any {
	_, ch := h.request("event", name, data)
	return h.await(ch)
}

// toolCall fires the named tool with params.
func (h *pivccHost) toolCall(name string, params map[string]any) map[string]any {
	_, ch := h.request("tool_call", name, params)
	return h.await(ch)
}

// command fires the named command with its argument text.
func (h *pivccHost) command(name, args string) map[string]any {
	_, ch := h.request("command", name, args)
	return h.await(ch)
}

// callsOf returns every recorded call of one method.
func (h *pivccHost) callsOf(method string) []hostCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hostCall, 0, 4)
	for _, call := range h.calls {
		if call.Method == method {
			out = append(out, call)
		}
	}
	return out
}

func (h *pivccHost) notes() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.notifications, "\n")
}

func (h *pivccHost) waitForCalls(method string, count int) {
	h.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.callsOf(method)) >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("only %d %s calls before the deadline, want %d", len(h.callsOf(method)), method, count)
}

// setSession points the host's session answers at a session file and the log
// it holds, so a recall call has something to search.
func (h *pivccHost) setSession(sessionFile string, entries []json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionFile = sessionFile
	h.sessionEntries = entries
}

// setState pushes a state_update: the UI flag, the session file and the active
// model, exactly as a host reports them.
func (h *pivccHost) setState(hasUI bool) {
	h.mu.Lock()
	sessionFile := h.sessionFile
	h.mu.Unlock()
	h.send(map[string]any{"type": "notify", "notify": map[string]any{"method": "state_update", "args": map[string]any{
		"state": map[string]any{
			"hasUI": hasUI,
			"session": map[string]any{
				"sessionId":       "test-session",
				"sessionFile":     sessionFile,
				"entriesAppended": []json.RawMessage{},
				"entryCount":      0,
				"leafId":          "",
			},
		},
	}}})
}

// setModel pushes the active model, so a handler that reads the provider sees
// it.
func (h *pivccHost) setModel(provider, model string) {
	h.send(map[string]any{"type": "notify", "notify": map[string]any{"method": "state_update", "args": map[string]any{
		"state": map[string]any{
			"model": map[string]any{"id": model, "provider": map[string]any{"id": provider}},
		},
	}}})
}

// setSettings rewrites the isolated settings file the extension loads.
func (h *pivccHost) setSettings(values map[string]any) {
	h.t.Helper()
	merged := map[string]any{
		"overrideDefaultCompaction":     true,
		"smartKeepTail":                 true,
		"continueAfterThresholdCompact": true,
		"debug":                         false,
		"skipForProviders":              []any{},
		"skipCustomTypes":               []any{},
		"trackCommands":                 []any{},
	}
	for key, value := range values {
		merged[key] = value
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(settingsPathForTest(), append(data, '\n'), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// settingsPathForTest is the fenced settings path TestMain installed.
func settingsPathForTest() string { return os.Getenv("PI_VCC_CONFIG_PATH") }

// readFileForTest reads a file a test wrote.
func readFileForTest(path string) ([]byte, error) { return os.ReadFile(path) }

// responseText is a tool/command response's text content. sdk.ToolResult
// marshals its content as a plain string; a host that answers with blocks gets
// the array shape.
func responseText(r map[string]any) string {
	result, _ := r["result"].(map[string]any)
	if result == nil {
		return fmt.Sprint(r)
	}
	switch content := result["content"].(type) {
	case string:
		return content
	case []any:
		if len(content) == 0 {
			return fmt.Sprint(result)
		}
		first, _ := content[0].(map[string]any)
		text, _ := first["text"].(string)
		return text
	}
	return fmt.Sprint(result)
}

func readPivccFrame(r io.Reader) (map[string]any, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// ── registration: what index.ts wires up ────────────────────────────────────

// index.ts registers exactly two commands and one tool; Extension() must do
// the same, with the TS descriptions.
func TestExtension_RegistersCommandsToolAndEvents(t *testing.T) {
	h := newHost(t)
	if len(h.commands) != 2 {
		t.Fatalf("commands = %v, want pi-vcc and pi-vcc-recall", keysOf(h.commands))
	}
	piVcc := h.commands["pi-vcc"]
	if piVcc == nil || piVcc["description"] != PiVccCommandDescription {
		t.Fatalf("/pi-vcc = %#v", piVcc)
	}
	if piVcc["description"] != "Compact conversation with pi-vcc structured summary" {
		t.Fatalf("/pi-vcc description = %q", piVcc["description"])
	}
	recall := h.commands["pi-vcc-recall"]
	if recall == nil || recall["description"] != VccRecallCommandDescription {
		t.Fatalf("/pi-vcc-recall = %#v", recall)
	}

	tool := h.tools[RecallToolName]
	if tool == nil {
		t.Fatalf("tools = %v, want %s", keysOf(h.tools), RecallToolName)
	}
	if tool["name"] != RecallToolName || tool["label"] != "VCC Recall" {
		t.Fatalf("tool identity = %#v", tool)
	}
	if tool["description"] != RecallToolDescription {
		t.Fatal("the tool description drifted from tools/recall.ts")
	}
	if tool["prompt_snippet"] != RecallToolPromptSnippet {
		t.Fatalf("prompt_snippet = %q", tool["prompt_snippet"])
	}
	if tool["prepares_arguments"] != true {
		t.Fatal("the tool must declare prepareArguments")
	}
	// The schema tools/recall.ts declares: query/range/expand/page/scope/mode.
	params, _ := tool["parameters"].(map[string]any)
	properties, _ := params["properties"].(map[string]any)
	for _, name := range []string{"query", "range", "expand", "page", "scope", "mode"} {
		if properties[name] == nil {
			t.Fatalf("schema is missing %q: %#v", name, properties)
		}
	}
	if params["type"] != "object" {
		t.Fatalf("schema type = %v", params["type"])
	}

	// before-compact.ts registers four handlers; index.ts adds no more.
	for _, name := range []string{
		sdk.EventContext, sdk.EventBeforeAgentStart, sdk.EventSessionBeforeCompact, sdk.EventSessionCompact,
	} {
		if h.handlers[name] == 0 {
			t.Fatalf("no handler for %q (handlers: %v)", name, h.handlers)
		}
	}
	if len(h.handlers) != 4 {
		t.Fatalf("handlers = %v, want exactly the four before-compact ones", h.handlers)
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// Extension() must be safe to call twice: PiG builds one extension per load,
// but the state map is keyed per extension, so a second build is independent.
func TestExtension_TwiceBuildsIndependentState(t *testing.T) {
	first := Extension()
	second := Extension()
	if first == second {
		t.Fatal("two builds returned the same extension")
	}
	if extStateOf(first) == extStateOf(second) {
		t.Fatal("two builds share one state")
	}
	extStateOf(first).setLastCompactWasPiVcc(true)
	if extStateOf(second).getLastCompactWasPiVcc() {
		t.Fatal("the second build inherited the first's state")
	}
}

// ── vcc_recall over the wire ───────────────────────────────────────────────

// recallSession is a two-turn session on one branch, the shape pi writes: the
// first turn is history a recall can reach, the second is the turn in
// progress, which the tool leaves out of its search.
func recallSession(t *testing.T) (string, []json.RawMessage) {
	t.Helper()
	lines := []string{
		`{"type":"message","id":"e1","parentId":"","message":{"role":"user","content":"fix the OAuth token refresh bug in auth.ts"}}`,
		`{"type":"message","id":"e2","parentId":"e1","message":{"role":"assistant","content":[{"type":"text","text":"I will look at the auth flow and the token refresh path."}]}}`,
		`{"type":"message","id":"e3","parentId":"e2","message":{"role":"toolResult","toolName":"read","toolCallId":"call-1","content":"auth.ts: token refresh fails on expiry"}}`,
		`{"type":"message","id":"e4","parentId":"e3","message":{"role":"user","content":"now write the regression test for it"}}`,
		`{"type":"message","id":"e5","parentId":"e4","message":{"role":"assistant","content":[{"type":"text","text":"Adding the test now."}]}}`,
	}
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	entries := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		entries = append(entries, json.RawMessage(line))
	}
	return sessionFile, entries
}

// TestExtension_RecallToolSearchesTheSession is the full tool path: the host
// asks vcc_recall to search, the tool reads the session file, and the answer
// carries the hit with its #N index and the paging hint.
func TestExtension_RecallToolSearchesTheSession(t *testing.T) {
	sessionFile, entries := recallSession(t)
	h := newHost(t)
	h.setSession(sessionFile, entries)

	result := h.toolCall(RecallToolName, map[string]any{"query": "token refresh"})
	if result["error"] != nil {
		t.Fatalf("vcc_recall failed: %#v", result["error"])
	}
	text := responseText(result)
	if !strings.Contains(text, "token refresh") {
		t.Fatalf("the answer lost the query:\n%s", text)
	}
	if !strings.Contains(text, "#0") && !strings.Contains(text, "#1") {
		t.Fatalf("the answer lost the hit index:\n%s", text)
	}
	// The tool's paging hint points at page:N, not the command's
	// /pi-vcc-recall spelling.
	if strings.Contains(text, "/pi-vcc-recall") {
		t.Fatalf("the tool used the command's paging hint:\n%s", text)
	}
	// The host saw the session file it answered with.
	if got := h.callsOf("getSessionFile"); len(got) != 1 {
		t.Fatalf("getSessionFile calls = %d, want 1", len(got))
	}
}

// TestExtension_RecallToolPaging exercises the second page of a search that
// has more hits than SearchPageSize. The session is one long first turn, so
// every entry is searchable history.
func TestExtension_RecallToolPaging(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	var lines []string
	for i := 0; i < 8; i++ {
		parent := ""
		if i > 0 {
			parent = `,"parentId":"e` + strconv.Itoa(i-1) + `"`
		}
		lines = append(lines, fmt.Sprintf(
			`{"type":"message","id":"e%d"%s,"message":{"role":"user","content":"needle entry %s"}}`,
			i, parent, jsNumberString(float64(i))))
	}
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	entries := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		entries = append(entries, json.RawMessage(line))
	}
	h := newHost(t)
	h.setSession(sessionFile, entries)

	first := responseText(h.toolCall(RecallToolName, map[string]any{"query": "needle"}))
	if !strings.Contains(first, "Page 1/2") {
		t.Fatalf("page 1 header missing:\n%s", first)
	}
	if !strings.Contains(first, "Use page:2 for more results") {
		t.Fatalf("the next-page hint is missing:\n%s", first)
	}
	second := responseText(h.toolCall(RecallToolName, map[string]any{"query": "needle", "page": float64(2)}))
	if !strings.Contains(second, "Page 2/2") {
		t.Fatalf("page 2 header missing:\n%s", second)
	}
	// The last page carries no next-page hint.
	if strings.Contains(second, "Use page:") {
		t.Fatalf("the last page advertises another page:\n%s", second)
	}
}

// TestExtension_RecallToolWithoutSessionFile covers the guard tools/recall.ts
// has before anything else: no session file, no recall.
func TestExtension_RecallToolWithoutSessionFile(t *testing.T) {
	h := newHost(t)
	result := h.toolCall(RecallToolName, map[string]any{"query": "anything"})
	if result["error"] != nil {
		t.Fatalf("the tool should answer, not fail: %#v", result["error"])
	}
	if text := responseText(result); text != "No session file available." {
		t.Fatalf("text = %q", text)
	}
}

// TestExtension_RecallToolPrepareArgumentsPinsRepair covers the host's
// tool_prepare_arguments path: NormalizeToolParams repairs the model's
// arguments before pi validates the schema.
func TestExtension_RecallToolPrepareArgumentsPinsRepair(t *testing.T) {
	h := newHost(t)
	_, ch := h.request("tool_prepare_arguments", RecallToolName, map[string]any{"expand": []any{float64(3)}})
	response := h.await(ch)
	if response["error"] != nil {
		t.Fatalf("prepare_arguments failed: %#v", response["error"])
	}
	result, _ := response["result"].(map[string]any)
	if result["expand"] == nil {
		t.Fatalf("prepared arguments = %#v, want the normalised params", result)
	}
}

// ── the two commands ───────────────────────────────────────────────────────

// TestExtension_PiVccRecallCommandSendsMessage is commands/vcc-recall.ts: the
// command answers by injecting a message that triggers a turn.
func TestExtension_PiVccRecallCommandSendsMessage(t *testing.T) {
	sessionFile, entries := recallSession(t)
	h := newHost(t)
	h.setSession(sessionFile, entries)

	result := h.command("pi-vcc-recall", "token refresh")
	if result["error"] != nil {
		t.Fatalf("/pi-vcc-recall failed: %#v", result["error"])
	}
	sends := h.callsOf("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sends))
	}
	message, _ := sends[0].Args["message"].(map[string]any)
	if message["customType"] != "vcc-recall" {
		t.Fatalf("customType = %v", message["customType"])
	}
	content, _ := message["content"].(string)
	if !strings.Contains(content, "token refresh") {
		t.Fatalf("content lost the query:\n%s", content)
	}
	if message["display"] != true {
		t.Fatalf("message = %#v, want display", message)
	}
	options, _ := sends[0].Args["options"].(map[string]any)
	if options["triggerTurn"] != true {
		t.Fatalf("options = %#v, want triggerTurn", options)
	}
}

// TestExtension_PiVccRecallCommandWithoutSessionFile keeps the command's guard.
func TestExtension_PiVccRecallCommandWithoutSessionFile(t *testing.T) {
	h := newHost(t)
	result := h.command("pi-vcc-recall", "anything")
	if result["error"] != nil {
		t.Fatalf("the command should answer, not fail: %#v", result["error"])
	}
	if len(h.callsOf("sendMessage")) != 0 {
		t.Fatal("a command without a session file sent a message")
	}
	h.waitForCalls("ui.notify", 1)
	if !strings.Contains(h.notes(), "No session file available.") {
		t.Fatalf("notifications = %q", h.notes())
	}
}

// TestExtension_PiVccCommandStartsCompaction is commands/pi-vcc.ts: the command
// asks the host to compact with pi-vcc's custom instructions and no follow-up.
func TestExtension_PiVccCommandStartsCompaction(t *testing.T) {
	h := newHost(t)
	h.mu.Lock()
	h.compactResult = map[string]any{"summary": "[Session Goal]\n- x"}
	h.mu.Unlock()

	result := h.command("pi-vcc", "")
	if result["error"] != nil {
		t.Fatalf("/pi-vcc failed: %#v", result["error"])
	}
	h.waitForCalls("compact", 1)
	compacts := h.callsOf("compact")
	if len(compacts) != 1 {
		t.Fatalf("compact calls = %d, want 1", len(compacts))
	}
	if compacts[0].Args["awaitCompletion"] != true {
		t.Fatalf("compact args = %#v, want awaitCompletion", compacts[0].Args)
	}
	// OnComplete notifies once the UI has settled.
	h.waitForCalls("ui.notify", 1)
	if !strings.Contains(h.notes(), "Compacted with pi-vcc") {
		t.Fatalf("notifications = %q", h.notes())
	}
}

// TestExtension_PiVccCommandReportsFailure covers the OnError branch: a failed
// compaction is reported as an error notification, not a crash.
func TestExtension_PiVccCommandReportsFailure(t *testing.T) {
	h := newHost(t)
	h.mu.Lock()
	h.compactError = "boom"
	h.mu.Unlock()

	result := h.command("pi-vcc", "")
	if result["error"] != nil {
		t.Fatalf("/pi-vcc failed: %#v", result["error"])
	}
	h.waitForCalls("ui.notify", 1)
	if !strings.Contains(h.notes(), "Compaction failed: boom") {
		t.Fatalf("notifications = %q", h.notes())
	}
}

// TestExtension_PiVccCommandMapsNothingToCompact covers the two host messages
// that mean "there was nothing to do".
func TestExtension_PiVccCommandMapsNothingToCompact(t *testing.T) {
	for _, message := range []string{"Compaction cancelled", "Already compacted", "Nothing to compact yet: too short"} {
		t.Run(message, func(t *testing.T) {
			h := newHost(t)
			h.mu.Lock()
			h.compactError = message
			h.mu.Unlock()
			if result := h.command("pi-vcc", ""); result["error"] != nil {
				t.Fatalf("/pi-vcc failed: %#v", result["error"])
			}
			h.waitForCalls("ui.notify", 1)
			if !strings.Contains(h.notes(), "Nothing to compact") {
				t.Fatalf("notifications = %q", h.notes())
			}
		})
	}
}

// ── the context event ──────────────────────────────────────────────────────

// TestExtension_ContextStripsAutoContinueMarker is the context handler: the
// invisible continue pi-vcc sends itself never reaches the model, and a payload
// without the marker is left untouched.
func TestExtension_ContextStripsAutoContinueMarker(t *testing.T) {
	h := newHost(t)
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "keep me"},
		map[string]any{"role": "custom", "customType": AutoContinueCustomType, "content": []any{}},
		map[string]any{"role": "custom", "customType": "other", "content": "also keep"},
	}}
	response := h.event(sdk.EventContext, payload)
	if response["error"] != nil {
		t.Fatalf("context event failed: %#v", response["error"])
	}
	result, _ := response["result"].(map[string]any)
	messages, _ := result["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want the marker dropped", messages)
	}
	// A payload without the marker returns nothing, so the host keeps its own.
	h.event(sdk.EventContext, map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "nothing to strip"},
	}})
}

// TestExtension_BeforeAgentStartClearsPendingContinue wires the second handler:
// a scheduled continue is cancelled once the agent starts.
//
// The end state is what the test pins: both a cleared timer and one that
// already fired leave pendingAutoContinue nil, so the assertion holds either
// way (extstate_test.go pins the clear itself).
func TestExtension_BeforeAgentStartClearsPendingContinue(t *testing.T) {
	h := newHost(t)
	state := extStateOf(h.ext)
	state.scheduleAutoContinue(h.ext, sdk.Context{})
	response := h.event(sdk.EventBeforeAgentStart, map[string]any{"systemPromptOptions": map[string]any{}})
	if response["error"] != nil {
		t.Fatalf("before_agent_start failed: %#v", response["error"])
	}
	state.mu.Lock()
	timer := state.pendingAutoContinue
	state.mu.Unlock()
	if timer != nil {
		t.Fatal("the pending continue survived the agent start")
	}
}
