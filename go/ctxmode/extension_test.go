package ctxmode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ctxmode.test.ts:668: cancellation propagates as a Go error, never success text.
func TestExtensionAbortRejected(t *testing.T) {
	recv, _ := setupHelper(t)
	h := newSDKHost(t)
	h.event("session_start")
	defer h.event("session_shutdown")
	if h.tool("ctx_run") == nil {
		t.Fatal("ctx_run not registered")
	}
	// Pre-aborted tool context exercises the same handler the SDK registers.
	c := newClient()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	state := &extensionState{client: c}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := state.run(ctx, "ctx_run", map[string]any{"action": "execute", "argv": []string{"sleep", "300"}})
	requireError(t, err, cancelledMessage)
	if result != nil {
		t.Fatal("cancel returned text")
	}
	methods := fileText(t, recv)
	requireContains(t, methods, "initialize")
	requireContains(t, methods, "tools/list")
	if strings.Contains(methods, "tools/call") || strings.Contains(methods, "notifications/cancelled") {
		t.Fatal("pre-abort sent request", methods)
	}
}

// ctxmode.test.ts:722.
func TestExtensionStatsRegistered(t *testing.T) {
	recv, _ := setupHelper(t)
	h := newSDKHost(t)
	h.event("session_start")
	defer h.event("session_shutdown")
	if h.tool("ctx_stats") == nil || !contains(newClient().GetTools(), "ctx_stats") {
		t.Fatal("ctx_stats absent")
	}
	_, call := h.request("tool_call", "ctx_stats", map[string]any{"json": true})
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), `"name":"ctx_stats"`) })
	h.event("session_shutdown")
	requireContains(t, responseText(h.await(call)), "ctxmode disconnected")
}

// ctxmode.test.ts:772.
func TestExtensionFSResolveRelated(t *testing.T) {
	recv, _ := setupHelper(t)
	h := newSDKHost(t)
	h.event("session_start")
	defer h.event("session_shutdown")
	def := h.tool("ctx_fs")
	if def == nil {
		t.Fatal("ctx_fs missing")
	}
	action := def["parameters"].(map[string]any)["properties"].(map[string]any)["action"].(map[string]any)
	if !reflect.DeepEqual(action["enum"], []any{"ls", "glob", "stat", "rg", "resolve", "related"}) || action["description"] != "ls|glob|stat|rg|resolve|related" {
		t.Fatal(action)
	}
	requireContains(t, def["description"].(string), "resolve")
	requireContains(t, def["description"].(string), "related")
	args := []map[string]any{{"action": "resolve", "path": "@router.go", "limit": float64(5)}, {"action": "related", "path": "router.go", "limit": float64(3)}}
	_, resolve := h.request("tool_call", "ctx_fs", args[0])
	_, related := h.request("tool_call", "ctx_fs", args[1])
	waitFor(t, func() bool {
		return strings.Contains(fileText(t, recv), `"action":"resolve"`) && strings.Contains(fileText(t, recv), `"action":"related"`)
	})
	seen := map[string]map[string]any{}
	for _, line := range strings.Split(fileText(t, recv), "\n") {
		if line == "" {
			continue
		}
		req := decodeRequest(t, line)
		if req["method"] == "tools/call" {
			p := req["params"].(map[string]any)
			if p["name"] != "ctx_fs" {
				t.Fatal(p)
			}
			a := p["arguments"].(map[string]any)
			seen[a["action"].(string)] = a
		}
	}
	for _, a := range args {
		if !reflect.DeepEqual(seen[a["action"].(string)], a) {
			t.Fatal(seen, a)
		}
	}
	h.event("session_shutdown")
	requireContains(t, responseText(h.await(resolve)), "ctxmode disconnected")
	requireContains(t, responseText(h.await(related)), "ctxmode disconnected")
}

// ctxmode.test.ts:871.
func TestExtensionShutdownNoRespawn(t *testing.T) {
	recv, pids := setupHelper(t)
	h := newSDKHost(t)
	h.event("session_start")
	defer h.event("session_shutdown")
	_, call := h.request("tool_call", "ctx_run", map[string]any{"action": "run_task", "kind": "go_test", "timeout_ms": 600000})
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), "tools/call") })
	if countPids(t, pids) != 1 {
		t.Fatal("initial spawn count")
	}
	h.event("session_shutdown")
	result := h.await(call)
	if result["error"] != nil {
		t.Fatal("disconnect should return text", result)
	}
	requireContains(t, responseText(result), "ctxmode disconnected")
	if res, _ := result["result"].(map[string]any); res["is_error"] != true {
		t.Fatalf("disconnect result missing is_error=true: %v", result)
	}
	time.Sleep(500 * time.Millisecond)
	if countPids(t, pids) != 1 {
		t.Fatal("shutdown spawned second child")
	}
}
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// P2-3: `NewClient` gets filepath.Abs(workdir) while the notify text keeps the raw
// workdir string, on both the session_start and the ctxmode-start path.
func TestWorkdirNotifyKeepsRawValue(t *testing.T) {
	setupHelper(t)
	t.Setenv("CTXMODE_WORKDIR", "relative-workdir")
	h := newSDKHost(t)
	h.event("session_start")
	state := &extensionState{}
	if _, err := state.start("relative-workdir", false, func() {}); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	got := state.client.workdir
	state.mu.Unlock()
	want := filepath.Join(wd, "relative-workdir")
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("client workdir %q, want %q", got, want)
	}
	state.stop()
	// ctxmode-start runs the same start closure after the session client is gone.
	h.event("session_shutdown")
	_, ch := h.request("command", "ctxmode-start", "")
	h.await(ch)
	h.mu.Lock()
	notes := strings.Join(h.notifications, "\n")
	h.mu.Unlock()
	if n := strings.Count(notes, "(workdir: relative-workdir)"); n != 2 {
		t.Fatalf("raw workdir notifications = %d in %q", n, notes)
	}
	if strings.Contains(notes, want) {
		t.Fatalf("notify leaked the resolved workdir: %q", notes)
	}
	h.event("session_shutdown")
}

// Additional real SDK cancellation and all lifecycle/command branches.
func TestSDKCancellationAndCommands(t *testing.T) {
	recv, pids := setupHelper(t)
	h := newSDKHost(t)
	_, ch := h.request("command", "ctxmode-stop", "")
	h.await(ch)
	_, ch = h.request("command", "ctxmode-start", "")
	h.await(ch)
	_, ch = h.request("command", "ctxmode-start", "")
	h.await(ch)
	if countPids(t, pids) != 1 {
		t.Fatal("start while running spawned")
	}
	id, call := h.request("tool_call", "ctx_git", map[string]any{"action": "status"})
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), "tools/call") })
	h.send(map[string]any{"type": "cancel", "id": id})
	r := h.await(call)
	if r["error"] == nil {
		t.Fatal("SDK abort not rejected", r)
	}
	requireContains(t, r["error"].(map[string]any)["message"].(string), cancelledMessage)
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), "notifications/cancelled") })
	_, prompt := h.request("event", "before_agent_start", map[string]any{
		"systemPrompt":        "BASE",
		"systemPromptOptions": map[string]any{"sections": map[string]any{}},
	})
	promptResponse := h.await(prompt)
	if promptResponse["error"] != nil {
		t.Fatalf("before_agent_start failed: %+v", promptResponse)
	}
	requireContains(t, promptResponse["result"].(map[string]any)["_pigPromptResult"].(map[string]any)["systemPrompt"].(string), "BASE\n\n## Ctxmode server instructions\n\nstub")
	_, ch = h.request("command", "ctxmode-stop", "")
	h.await(ch)
	_, call = h.request("tool_call", "ctx_git", map[string]any{"action": "status"})
	requireContains(t, responseText(h.await(call)), "ctxmode 未运行。用 /ctxmode-start 启动。")
	h.mu.Lock()
	notes := strings.Join(h.notifications, "\n")
	h.mu.Unlock()
	for _, s := range []string{"ctxmode 未运行", "ctxmode 已启动", "ctxmode 已在运行", "ctxmode 已停止"} {
		requireContains(t, notes, s)
	}
}
func TestExtensionStartupFailureLevels(t *testing.T) {
	setupHelper(t)
	t.Setenv("CTXMODE_TEST_FAIL_INIT", "1")
	h := newSDKHost(t)
	h.event("session_start")
	_, ch := h.request("command", "ctxmode-start", "")
	h.await(ch)
	h.mu.Lock()
	notes := strings.Join(h.notifications, "\n")
	h.mu.Unlock()
	for _, s := range []string{"ctxmode 启动失败: initialize refused", "warning", "error"} {
		requireContains(t, notes, s)
	}
	h.mu.Lock()
	n := len(h.tools)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("failed startup registered tools")
	}
	h.event("session_shutdown")
}

// ctx_report (7th tool): registered model-only with annotations and a
// closed single-content schema, as the server registers it.
func TestExtensionReportRegistered(t *testing.T) {
	setupHelper(t)
	h := newSDKHost(t)
	h.event("session_start")
	defer h.event("session_shutdown")
	h.mu.Lock()
	toolCount := len(h.tools)
	toolMap := make(map[string]map[string]any, toolCount)
	for name, def := range h.tools {
		toolMap[name] = def
	}
	h.mu.Unlock()
	// Snapshot h.tools before any Fatalf: sdkHost cleanup needs h.mu to deliver
	// the pending shutdown response, so a Fatalf while holding the lock would
	// deadlock the test instead of reporting a clean assertion failure.
	if toolCount != 7 {
		t.Fatalf("registered tools = %v", toolMap)
	}
	if !contains(newClient().GetTools(), "ctx_report") {
		t.Fatal("ctx_report absent from toolNames")
	}
	def := h.tool("ctx_report")
	if def == nil {
		t.Fatal("ctx_report missing")
	}
	if def["exposure"] != "model-only" {
		t.Fatalf("exposure = %v", def["exposure"])
	}
	ann, _ := def["annotations"].(map[string]any)
	if ann["readOnlyHint"] != false || ann["destructiveHint"] != false || ann["openWorldHint"] != false {
		t.Fatalf("annotations = %v", ann)
	}
	schema := def["parameters"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v", schema["additionalProperties"])
	}
	if !reflect.DeepEqual(schema["required"], []any{"content"}) {
		t.Fatalf("required = %v", schema["required"])
	}
	content := schema["properties"].(map[string]any)["content"].(map[string]any)
	if content["type"] != "string" {
		t.Fatalf("content schema = %v", content)
	}
	if h.tool("ctx_run")["exposure"] != nil {
		t.Fatal("unrelated tool leaked an exposure")
	}
}

// marshalResultJSON returns the wire JSON the PiG subprocess host decodes; it
// must use the snake_case `is_error` key the host's ToolResult recognises.
func marshalResultJSON(t *testing.T, result any) string {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	return string(data)
}

// decodeWireResult unmarshals the marshalled tool result into the wire shape
// the PiG subprocess host decodes; it asserts on line-level JSON only, never
// on production-internal types, so the test file still compiles against a
// baseline without those symbols.
func decodeWireResult(t *testing.T, result any) wireResult {
	t.Helper()
	var w wireResult
	if err := json.Unmarshal([]byte(marshalResultJSON(t, result)), &w); err != nil {
		t.Fatalf("unmarshal wire result: %v", err)
	}
	return w
}

// The server's isError flag and client-side call failures both surface as
// is_error=true on the wire; Esc stays a Go error, not a result.
type wireResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"is_error"`
}

func TestExtensionReportIsError(t *testing.T) {
	c, p := recordingClient()
	state := &extensionState{client: c}
	type callResult struct {
		result any
		err    error
	}
	run := func() <-chan callResult {
		ch := make(chan callResult, 1)
		go func() {
			res, err := state.run(context.Background(), "ctx_report", map[string]any{"content": "body"})
			ch <- callResult{res, err}
		}()
		return ch
	}
	// Server-side result.isError=true → toolResult.IsError with the text.
	rch := run()
	waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
	respondIsError(c, 1, "ctx_report: dispatch-report failed: exit status 1")
	r := <-rch
	wr := decodeWireResult(t, r.result)
	if r.err != nil || !wr.IsError || wr.Content[0].Text != "ctx_report: dispatch-report failed: exit status 1" {
		t.Fatalf("isError result = %+v err=%v", r.result, r.err)
	}
	wire := marshalResultJSON(t, r.result)
	if !strings.Contains(wire, `"is_error":true`) || strings.Contains(wire, "isError") {
		t.Fatalf("error wire JSON = %s", wire)
	}
	// A named contentBlock keeps the TS member order: "type" before "text".
	if ti, xi := strings.Index(wire, `"type":"text"`), strings.Index(wire, `"text":`); ti < 0 || ti > xi {
		t.Fatalf("content member order = %s", wire)
	}
	// A success response keeps is_error off the wire.
	rch = run()
	waitFor(t, func() bool { return len(p.in.snapshot()) == 2 })
	respond(c, 2, "dispatch-report: status=delivered path=/p bytes=4")
	r = <-rch
	wr = decodeWireResult(t, r.result)
	if r.err != nil || wr.IsError || wr.Content[0].Text != "dispatch-report: status=delivered path=/p bytes=4" {
		t.Fatalf("ok result = %+v err=%v", r.result, r.err)
	}
	if wire := marshalResultJSON(t, r.result); strings.Contains(wire, "is_error") || strings.Contains(wire, "isError") {
		t.Fatalf("success wire JSON = %s", wire)
	}
	// Low-cap boundary: a successful ctx_report receipt bypasses CompressToolText
	// verbatim, while a failed reply still goes through the compressor.
	t.Setenv("CTXMODE_OUTPUT_CHARS", "24")
	receipt := "dispatch-report: status=delivered path=/tmp/herdr-role-reports/default/r/report.md bytes=99"
	rch = run()
	waitFor(t, func() bool { return len(p.in.snapshot()) == 3 })
	respond(c, 3, receipt)
	r = <-rch
	wr = decodeWireResult(t, r.result)
	if r.err != nil || wr.IsError || wr.Content[0].Text != receipt {
		t.Fatalf("success receipt = %+v err=%v", r.result, r.err)
	}
	if strings.Contains(wr.Content[0].Text, "已截断") {
		t.Fatalf("success receipt compressed: %q", wr.Content[0].Text)
	}
	rch = run()
	waitFor(t, func() bool { return len(p.in.snapshot()) == 4 })
	respondIsError(c, 4, "ctx_report: dispatch-report failed: exit status 1\n"+strings.Repeat("E", 80))
	r = <-rch
	wr = decodeWireResult(t, r.result)
	if r.err != nil || !wr.IsError || !strings.Contains(wr.Content[0].Text, "已截断") {
		t.Fatalf("failed receipt = %+v err=%v", r.result, r.err)
	}
	// Client-side CallTool failure (client stopped) is an isError result, not a Go error.
	c2 := newClient()
	c2.Stop()
	state2 := &extensionState{client: c2}
	res2, err := state2.run(context.Background(), "ctx_report", map[string]any{"content": "x"})
	wr2 := decodeWireResult(t, res2)
	if err != nil || !wr2.IsError || !strings.Contains(wr2.Content[0].Text, "ctxmode error") {
		t.Fatalf("client-error result = %+v err=%v", res2, err)
	}
	// A missing client reports "未运行" as isError too.
	res3, err := (&extensionState{}).run(context.Background(), "ctx_report", nil)
	wr3 := decodeWireResult(t, res3)
	if err != nil || !wr3.IsError || !strings.Contains(wr3.Content[0].Text, "未运行") {
		t.Fatalf("no-client result = %+v err=%v", res3, err)
	}
	// A real tool_call over the SDK IPC returns the same isError shape: the
	// helper's default handler leaves tools/call unanswered, so the request is
	// still in flight when shutdown resolves it as "ctxmode disconnected".
	recv, _ := setupHelper(t)
	h := newSDKHost(t)
	h.event("session_start")
	_, call := h.request("tool_call", "ctx_report", map[string]any{"content": "ipc-body"})
	// The tools/call line must be on the wire before shutdown; the helper
	// never answers it (default case), so the reply can only be disconnect.
	waitFor(t, func() bool {
		return strings.Contains(fileText(t, recv), `"name":"ctx_report"`) && strings.Contains(fileText(t, recv), `"ipc-body"`)
	})
	h.event("session_shutdown")
	res, ok4 := h.await(call)["result"].(map[string]any)
	if !ok4 || res["is_error"] != true || res["content"].([]any)[0].(map[string]any)["type"] != "text" {
		t.Fatalf("IPC ctx_report result = %v", res)
	}
	if text := res["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "ctxmode disconnected") {
		t.Fatalf("IPC ctx_report text = %q", text)
	}
}
