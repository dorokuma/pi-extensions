package ctxmode

import (
	"context"
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
