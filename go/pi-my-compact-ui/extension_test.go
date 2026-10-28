package compactui

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file covers src/extension.ts (L2192-2452): the factory, the eleven
// event handlers, session_start / session_shutdown, and the terminal-input
// routing. Every handler is driven through the runtime method it wires, with a
// fake uiContext standing in for the host.

// startedRuntime builds a runtime whose session_start has run against a fake UI.
func startedRuntime(t testing.TB) (*runtimeState, *fakeUI, *fakeTUI) {
	t.Helper()
	r := testRuntime(t)
	ui := newFakeUI()
	tui := newFakeTUI(24)
	r.mu.Lock()
	r.activeUIContext = ui
	r.capturedTui = tui
	r.acceptingEvents = true
	r.mu.Unlock()
	return r, ui, tui
}

// Test Extension / NewExtension register the documented surface
// (TS L2210-2219, L2288-2297).
// Test the factory builds distinct, correctly named extensions and that a
// second build drives the same wiring without panicking (task 应修 11.2). The
// SDK exposes no accessor for the registered resolver/command/event counts, so
// the assertions below are the observable surface: name, distinctness and a
// panic-free second build. The bodies those registrations call are covered by
// TestToolRendererCompactRow and the event-handler tests.
func TestExtensionRegistration(t *testing.T) {
	e := Extension()
	if e == nil {
		t.Fatal("Extension() returned nil")
	}
	if e.Name() != "pi-my-compact-ui" {
		t.Errorf("extension name = %q", e.Name())
	}

	// The factory is deterministic enough to build twice without panicking, and
	// both instances own separate runtime state.
	second := NewExtension()
	if second == nil {
		t.Fatal("NewExtension() returned nil")
	}
	if e == second {
		t.Error("each factory call must build a distinct extension")
	}
	if second.Name() != e.Name() {
		t.Errorf("second extension name = %q, want %q", second.Name(), e.Name())
	}
}

// Test the tool renderer the factory registers draws the same compact row the
// group draws. Result text stays folded unless Expanded is set.
func TestToolRendererCompactRow(t *testing.T) {
	r, _ := modalRuntime(t)
	group := r.newToolGroup(0)
	tool := newTestTool("call-1", "read", map[string]any{"path": "/etc/hosts"})
	group.addTool(tool)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	width := 80
	duration := int64(1500)
	call := r.toolRenderLines("read", map[string]any{"path": "/etc/hosts"}, sdk.ToolRenderContext{
		ToolCallID:       "call-1",
		ArgsComplete:     true,
		ExecutionStarted: true,
		IsPartial:        true,
		DurationMs:       &duration,
	}, nil, false, width, false)
	if len(call) != 2 {
		t.Fatalf("call lines = %d, want the tree (header + row)", len(call))
	}
	joined := joinLines(call)
	if !strings.Contains(joined, "read") || !strings.Contains(joined, "/etc/hosts") {
		t.Errorf("call rows = %q, want the tool name and its summary", joined)
	}
	// The tree path times the call from the group's own start (the pinned test
	// clock), the same source the multi-entry tree uses; the host DurationMs
	// only fed the old bare-row path.
	if !strings.Contains(call[1], "(0ms)") {
		t.Errorf("call row = %q, want the elapsed label", call[1])
	}
	if !strings.Contains(call[0], "tool calling...") {
		t.Errorf("call header = %q, want the tree header", call[0])
	}
	if !strings.Contains(call[1], "└  ") {
		t.Errorf("call row = %q, want the tree rail", call[1])
	}
	for _, line := range call {
		if tuiLen(line) > width {
			t.Errorf("call line width = %d, want <= %d", tuiLen(line), width)
		}
	}

	output := "127.0.0.1 localhost\n"
	folded := r.toolRenderLines("read", map[string]any{"path": "/etc/hosts"}, sdk.ToolRenderContext{
		ToolCallID:   "call-1",
		ArgsComplete: true,
	}, &output, false, width, true)
	if len(folded) != 0 {
		t.Fatalf("folded result lines = %v, want empty (Call owns the row)", folded)
	}

	expandedBody := r.toolRenderLines("read", map[string]any{"path": "/etc/hosts"}, sdk.ToolRenderContext{
		ToolCallID:   "call-1",
		ArgsComplete: true,
		Expanded:     true,
	}, &output, true, width, true)
	if len(expandedBody) != 0 {
		t.Fatalf("expanded result lines = %v, want none: a grouped tool renders nothing of its own, so Ctrl+O adds no output body (TS L1100-1110 hideGroupedToolRender + L822-823 setExpanded no-op); the detail view is Ctrl+I", expandedBody)
	}
	hostCard := append(append([]string{}, call...), expandedBody...)
	if len(hostCard) != 2 || !strings.Contains(hostCard[1], "read") {
		t.Errorf("host Call+Result card = %q, want the tree alone (header + row, no output body)", hostCard)
	}

	// A zero width must not panic or produce a negative-width wrap. The empty
	// render context carries no call id, so the card is the ungrouped one and
	// keeps the plain single row.
	if got := r.toolRenderLines("read", nil, sdk.ToolRenderContext{}, nil, false, 0, false); len(got) != 1 {
		t.Errorf("zero-width call lines = %d, want 1", len(got))
	}
}

// TestToolRendererCtxRunSummary covers the one-line summary for ctx_run args.
func TestToolRendererCtxRunSummary(t *testing.T) {
	r, _ := modalRuntime(t)
	args := map[string]any{"action": "execute", "command": "herdr pane read w3N:p1 --lines 40"}
	lines := r.toolRenderLines("ctx_run", args, sdk.ToolRenderContext{
		ToolCallID:   "ctx-1",
		ArgsComplete: true,
	}, nil, false, 100, false)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	joined := joinLines(lines)
	if !strings.Contains(joined, "ctx_run") {
		t.Errorf("row = %q, want tool name", joined)
	}
	if !strings.Contains(joined, "herdr pane read") {
		t.Errorf("row = %q, want command summary", joined)
	}
	if strings.Contains(joined, "ctx_run ...") || strings.HasSuffix(strings.TrimSpace(joined), "...") {
		// Allow truncation ellipsis on the right, but not a bare "..." summary.
		summary := toolSummary("ctx_run", args)
		if summary.content == "..." {
			t.Errorf("toolSummary content = %q, want the command", summary.content)
		}
	}
}

// TestToolRendererHidesGroupedNonDisplayTool covers hideGroupedToolRender on
// the host ToolRenderer path: only the last grouped tool paints.
func TestToolRendererHidesGroupedNonDisplayTool(t *testing.T) {
	r, _ := modalRuntime(t)
	group := r.newToolGroup(0)
	first := newTestTool("call-a", "ctx_run", map[string]any{"command": "echo a"}).succeeded("a-out")
	second := newTestTool("call-b", "ctx_run", map[string]any{"command": "echo b"}).succeeded("b-out")
	group.addTool(first)
	group.addTool(second)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	hidden := r.toolRenderLines("ctx_run", first.args, sdk.ToolRenderContext{
		ToolCallID:   "call-a",
		ArgsComplete: true,
	}, strPtr("a-out"), false, 80, false)
	if len(hidden) != 0 {
		t.Fatalf("first grouped tool Call lines = %v, want empty (hidden)", hidden)
	}
	if got := r.toolRenderLines("ctx_run", first.args, sdk.ToolRenderContext{
		ToolCallID:   "call-a",
		ArgsComplete: true,
	}, strPtr("a-out"), false, 80, true); len(got) != 0 {
		t.Fatalf("first grouped tool Result lines = %v, want empty (hidden)", got)
	}

	shown := r.toolRenderLines("ctx_run", second.args, sdk.ToolRenderContext{
		ToolCallID:   "call-b",
		ArgsComplete: true,
	}, strPtr("b-out"), false, 80, false)
	if len(shown) == 0 {
		t.Fatal("display tool Call rendered no lines")
	}
	if got := r.toolRenderLines("ctx_run", second.args, sdk.ToolRenderContext{
		ToolCallID:   "call-b",
		ArgsComplete: true,
	}, strPtr("b-out"), false, 80, true); len(got) != 0 {
		t.Fatalf("display tool Result lines = %v, want empty (Call owns the tree)", got)
	}
	joined := joinLines(shown)
	if !strings.Contains(joined, "ctx_run") {
		t.Errorf("group render = %q, want tool rows", joined)
	}
	if !strings.Contains(joined, "echo a") || !strings.Contains(joined, "echo b") {
		t.Errorf("group render = %q, want both tool summaries", joined)
	}
	if strings.Contains(joined, "a-out") || strings.Contains(joined, "b-out") {
		t.Errorf("group render = %q, want folded results", joined)
	}
}

// TestToolRendererGroupTreeAppearsOnce concatenates every card's Call+Result
// output for a multi-tool group and checks the group tree is painted once.
// After the display tool switches, the previous display card returns empty.
func TestToolRendererGroupTreeAppearsOnce(t *testing.T) {
	r, _ := modalRuntime(t)
	group := r.newToolGroup(0)
	first := newTestTool("call-a", "ctx_run", map[string]any{"command": "echo a"}).succeeded("a-out")
	second := newTestTool("call-b", "ctx_run", map[string]any{"command": "echo b"}).succeeded("b-out")
	third := newTestTool("call-c", "ctx_run", map[string]any{"command": "echo c"}).succeeded("c-out")
	group.addTool(first)
	group.addTool(second)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	// While second is the display tool, first is hidden and second paints.
	if got := r.toolRenderLines("ctx_run", first.args, sdk.ToolRenderContext{ToolCallID: "call-a", ArgsComplete: true}, nil, false, 80, false); len(got) != 0 {
		t.Fatalf("old display Call after switch = %v, want empty", got)
	}
	mid := r.toolRenderLines("ctx_run", second.args, sdk.ToolRenderContext{ToolCallID: "call-b", ArgsComplete: true}, nil, false, 80, false)
	if len(mid) == 0 {
		t.Fatal("second Call should paint the tree")
	}

	group.addTool(third)

	// After third takes display rights, second must clear (both phases).
	if got := r.toolRenderLines("ctx_run", second.args, sdk.ToolRenderContext{ToolCallID: "call-b", ArgsComplete: true}, strPtr("b-out"), false, 80, false); len(got) != 0 {
		t.Fatalf("previous display Call after switch = %v, want empty", got)
	}
	if got := r.toolRenderLines("ctx_run", second.args, sdk.ToolRenderContext{ToolCallID: "call-b", ArgsComplete: true}, strPtr("b-out"), false, 80, true); len(got) != 0 {
		t.Fatalf("previous display Result after switch = %v, want empty", got)
	}

	var host []string
	for _, id := range []string{"call-a", "call-b", "call-c"} {
		var tool *toolComponent
		switch id {
		case "call-a":
			tool = first
		case "call-b":
			tool = second
		default:
			tool = third
		}
		host = append(host, r.toolRenderLines("ctx_run", tool.args, sdk.ToolRenderContext{ToolCallID: id, ArgsComplete: true}, strPtr(id+"-out"), false, 80, false)...)
		host = append(host, r.toolRenderLines("ctx_run", tool.args, sdk.ToolRenderContext{ToolCallID: id, ArgsComplete: true}, strPtr(id+"-out"), false, 80, true)...)
	}
	joined := joinLines(host)
	if strings.Count(joined, "echo a") != 1 || strings.Count(joined, "echo b") != 1 || strings.Count(joined, "echo c") != 1 {
		t.Fatalf("concatenated host cards = %q, want each tool summary once", joined)
	}
	// Header / tree only from the display Call: one leading blank from group.Render.
	headers := 0
	for _, line := range host {
		plain := tuiStrip(line)
		if strings.Contains(plain, "tool") && (strings.Contains(plain, "calling") || strings.Contains(plain, "done") || strings.HasPrefix(strings.TrimSpace(plain), "✓") || strings.HasPrefix(strings.TrimSpace(plain), "·")) {
			// group header lines mention state, not "ctx_run"
			if !strings.Contains(plain, "ctx_run") {
				headers++
			}
		}
	}
	if headers != 1 {
		t.Fatalf("group header count = %d in %q, want 1", headers, joined)
	}
}

// Test setAgentWorkPhase (TS L2192-2196).
func TestSetAgentWorkPhase(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setAgentWorkPhase(strPtr(phaseWorking))
	r.mu.Lock()
	phase := r.agentWorkPhase
	r.mu.Unlock()
	if phase == nil || *phase != phaseWorking {
		t.Fatalf("phase = %v", phase)
	}
	if tui.renderCount() == 0 {
		t.Error("setting the phase should request a render")
	}
	r.setAgentWorkPhase(nil)
	r.mu.Lock()
	phase = r.agentWorkPhase
	r.mu.Unlock()
	if phase != nil {
		t.Errorf("phase after clear = %v", *phase)
	}
}

// Test handleToolExecutionStart / handleToolExecutionEnd (TS L2299-2325).
func TestToolExecutionHandlers(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })

	r.handleToolExecutionStart(map[string]any{"toolCallId": "call-1"})
	r.mu.Lock()
	start, started := r.toolStarts["call-1"]
	phase := r.agentWorkPhase
	r.mu.Unlock()
	if !started || start != 1_000 {
		t.Errorf("start = %d (recorded=%v)", start, started)
	}
	if phase == nil || *phase != phaseWorking {
		t.Errorf("phase = %v", phase)
	}

	// The end handler records the duration and stamps the group end.
	group := r.newToolGroup(0)
	tool := newTestTool("call-1", "read", nil)
	group.addTool(tool)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	r.setClock(func() int64 { return 3_500 })
	r.handleToolExecutionEnd(map[string]any{"toolCallId": "call-1"})
	r.mu.Lock()
	duration, timed := r.toolDurations["call-1"]
	r.mu.Unlock()
	if !timed || duration != 2.5 {
		t.Errorf("duration = %v (timed=%v)", duration, timed)
	}
	if tool.groupEndAt == nil || *tool.groupEndAt != 3_500 {
		t.Errorf("groupEndAt = %v", tool.groupEndAt)
	}
	if tui.renderCount() == 0 {
		t.Error("the tool end should request a render")
	}

	// A second end for the same call does not overwrite the duration.
	r.setClock(func() int64 { return 9_000 })
	r.handleToolExecutionEnd(map[string]any{"toolCallId": "call-1"})
	r.mu.Lock()
	duration = r.toolDurations["call-1"]
	r.mu.Unlock()
	if duration != 2.5 {
		t.Errorf("duration was overwritten: %v", duration)
	}
}

// Test handleMessageStart role user resets the turn (TS L2358-2400). The same
// user turn seals the open group, finalizes the leftover thinking run, drops the
// empty groups and converts a provisional call with a real id in place, so the
// four steps the handler performs after `now` are asserted together.
func TestMessageStartUser(t *testing.T) {
	r, _, _ := startedRuntime(t)
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.pendingTextSeal = true
	r.handledTextIndexes[0] = true
	r.mu.Unlock()

	// A provisional call whose stream already carried a real id has a host card,
	// so the new user turn converts it into an interrupted group entry instead of
	// detaching it (the same policy handleAgentEnd applies).
	streaming := newTestTool("call-a", "ctx_run", map[string]any{"command": "sleep 5"}).pending()
	provisional := &preparingTool{
		toolCallID: "call-a", toolName: "ctx_run", args: streaming.args,
		owner: group, comp: streaming, hasIndex: true,
	}
	r.mu.Lock()
	group.entries = append(group.entries, groupEntry{kind: "tool", tool: streaming, preparing: provisional})
	r.preparingByIndex[0] = provisional
	r.mu.Unlock()

	_ = r.createThinkingEntry()
	r.setClock(func() int64 { return 42_000 })
	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "user"}})

	if !group.sealed {
		t.Error("the open group should be sealed by a user message")
	}
	r.mu.Lock()
	active := r.activeThinking
	pending := r.pendingTextSeal
	handled := len(r.handledTextIndexes)
	turnStart := r.turnStartMs
	streamingComp := r.lastStreamingComp
	preparing := len(r.preparingByIndex)
	linked := group.entries[len(group.entries)-1].preparing != nil
	inChildren := group.hasChildLocked(streaming)
	interrupted := streaming.interrupted && r.interruptedToolCallID["call-a"]
	r.mu.Unlock()
	if active != nil || pending || handled != 0 || preparing != 0 {
		t.Errorf("turn state not reset: active=%v pending=%v handled=%d preparing=%d", active != nil, pending, handled, preparing)
	}
	if turnStart != 42_000 {
		t.Errorf("turnStartMs = %d", turnStart)
	}
	if streamingComp != nil {
		t.Error("lastStreamingComp should be cleared")
	}
	if linked {
		t.Error("the provisional entry kept its preparing link, want the call converted in place")
	}
	if !inChildren || !interrupted {
		t.Errorf("converted call: inChildren=%v interrupted=%v, want the interrupted group entry the agent_end walk sees", inChildren, interrupted)
	}
}

// Test handleMessageStart drops empty groups (TS L2376-2382).
func TestMessageStartDropsEmptyGroups(t *testing.T) {
	r, _, _ := startedRuntime(t)
	chat := newContainer(r)
	empty := r.newToolGroup(0)
	chat.children = []component{empty}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.addGroupLocked(empty)
	r.mu.Unlock()

	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "user"}})
	r.mu.Lock()
	_, present := r.groups[empty]
	r.mu.Unlock()
	if present {
		t.Error("an empty group should be dropped at a new user turn")
	}
	if chat.contains(empty) {
		t.Error("the empty group should leave the chat container")
	}
}

// Test handleMessageStart role assistant (TS L2402-2414).
func TestMessageStartAssistant(t *testing.T) {
	r, _, _ := startedRuntime(t)
	_ = r.createThinkingEntry()
	r.mu.Lock()
	r.handledTextIndexes[3] = true
	r.mu.Unlock()

	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "assistant"}})
	r.mu.Lock()
	active := r.activeThinking
	handled := len(r.handledTextIndexes)
	phase := r.agentWorkPhase
	r.mu.Unlock()
	if active != nil {
		t.Error("the active thinking run should be finalized")
	}
	if handled != 0 {
		t.Error("handledTextIndexes should be cleared")
	}
	if phase == nil || *phase != phaseWaitingResponse {
		t.Errorf("phase = %v", phase)
	}

	// An unknown role does nothing.
	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "system"}})
	r.handleMessageStart(map[string]any{})
}

// Test handleMessageEnd (TS L2416-2425).
func TestMessageEnd(t *testing.T) {
	r, _, tui := startedRuntime(t)
	entry := r.createThinkingEntry()

	reported := 4_321
	r.handleMessageEnd(map[string]any{"message": map[string]any{
		"role":  "assistant",
		"usage": map[string]any{"reasoning": 4_321.0},
	}})

	if entry.active {
		t.Error("the run should be finalized")
	}
	if entry.tokens != reported || !entry.tokensExact {
		t.Errorf("tokens = %d exact=%v, want the reported count", entry.tokens, entry.tokensExact)
	}
	r.mu.Lock()
	phase := r.agentWorkPhase
	r.mu.Unlock()
	if phase == nil || *phase != phaseWorking {
		t.Errorf("phase = %v", phase)
	}
	if tui.renderCount() == 0 {
		t.Error("refreshing the sealed run should request a render")
	}

	// A non-assistant message is ignored.
	r.handleMessageEnd(map[string]any{"message": map[string]any{"role": "user"}})
	r.handleMessageEnd(map[string]any{})
}

// Test handleThinkingDelta through message_update (TS L2430-2450).
func TestMessageUpdateThinking(t *testing.T) {
	r, _, tui := startedRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	_ = tui

	// thinking_start opens a run at index 0.
	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "thinking", "thinking": "step one"}},
		},
		"assistantMessageEvent": map[string]any{"type": "thinking_start", "contentIndex": 0.0},
	})
	r.mu.Lock()
	entry := r.activeThinking
	idx := r.activeThinkingIndex
	r.mu.Unlock()
	if entry == nil || idx == nil || *idx != 0 {
		t.Fatalf("run not started (entry=%v idx=%v)", entry != nil, idx)
	}
	if entry.text != "step one" {
		t.Errorf("run text = %q", entry.text)
	}

	// thinking_end finalizes it and records the sealed index.
	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "thinking", "thinking": "step one"}},
		},
		"assistantMessageEvent": map[string]any{"type": "thinking_end", "contentIndex": 0.0},
	})
	r.mu.Lock()
	active := r.activeThinking
	sealed := r.sealedThinkingIndexes[0]
	last := r.lastSealedThinkingEntry
	phase := r.agentWorkPhase
	r.mu.Unlock()
	if active != nil {
		t.Error("thinking_end should finalize the run")
	}
	if !sealed || last != entry {
		t.Errorf("sealed = %v last = %v", sealed, last == entry)
	}
	if phase == nil || *phase != phaseWaitingOutput {
		t.Errorf("phase after thinking_end = %v", phase)
	}

	// A delta for a sealed index is ignored.
	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "thinking", "thinking": "late"}},
		},
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": 0.0},
	})
	r.mu.Lock()
	active = r.activeThinking
	r.mu.Unlock()
	if active != nil {
		t.Error("a sealed index must not reopen a run")
	}
}

// Test a second thinking block starts a new run (TS L2437-2444).
func TestMessageUpdateThinkingSecondBlock(t *testing.T) {
	r, _, _ := startedRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	content := []any{
		map[string]any{"type": "thinking", "thinking": "first"},
		map[string]any{"type": "thinking", "thinking": "second"},
	}
	r.handleMessageUpdate(map[string]any{
		"message":               map[string]any{"role": "assistant", "content": content},
		"assistantMessageEvent": map[string]any{"type": "thinking_start", "contentIndex": 0.0},
	})
	first := func() *thinkingEntry {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.activeThinking
	}()

	r.handleMessageUpdate(map[string]any{
		"message":               map[string]any{"role": "assistant", "content": content},
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": 1.0},
	})
	r.mu.Lock()
	second := r.activeThinking
	idx := r.activeThinkingIndex
	r.mu.Unlock()
	if second == nil || second == first {
		t.Error("a new content index should start a new run")
	}
	if idx == nil || *idx != 1 {
		t.Errorf("activeThinkingIndex = %v", idx)
	}
	// finalizeActiveThinking runs first, as L2371 does: the previous run is
	// closed before the new one starts.
	if first.active {
		t.Error("starting a run at a new index should finalize the previous one")
	}
}

// Test handleTextDelta seals the group at the text (TS L2451-2467).
func TestMessageUpdateText(t *testing.T) {
	r, _, _ := startedRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	assistant.contentContainer.children = []component{&markdownComponent{text: "answer"}}
	chat.children = []component{assistant}
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "answer"}},
		},
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0.0},
	})
	r.mu.Lock()
	handled := r.handledTextIndexes[0]
	r.mu.Unlock()
	if !handled {
		t.Error("the text index should be recorded as handled")
	}
	if !group.sealed {
		t.Error("streaming real text should seal the open group")
	}

	// A repeated delta for the same index is a no-op.
	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "answer"}},
		},
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0.0},
	})
}

// Test handleMessageUpdate done/error (TS L2477-2484).
func TestMessageUpdateDoneAndError(t *testing.T) {
	for _, streamType := range []string{"done", "error"} {
		r, _, _ := startedRuntime(t)
		_ = r.createThinkingEntry()
		reported := 77
		r.handleMessageUpdate(map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{},
				"usage":   map[string]any{"reasoning": float64(reported)},
			},
			"assistantMessageEvent": map[string]any{"type": streamType},
		})
		r.mu.Lock()
		active := r.activeThinking
		r.mu.Unlock()
		if active != nil {
			t.Errorf("%s should finalize the run", streamType)
		}
	}

	// A non-assistant message and a message-less event are ignored.
	r, _, _ := startedRuntime(t)
	r.handleMessageUpdate(map[string]any{"message": map[string]any{"role": "user"}})
	r.handleMessageUpdate(map[string]any{})
}

// Test handleAgentEnd clears the turn (TS L2491-2530).
func TestAgentEnd(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 10_000 })
	r.mu.Lock()
	r.turnStartMs = 5_000
	r.mu.Unlock()

	group := r.newToolGroup(0)
	unfinished := newTestTool("unfinished", "read", nil).pending()
	group.addTool(unfinished)
	empty := r.newToolGroup(0)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.addGroupLocked(empty)
	r.lastActiveGroup = group
	r.mu.Unlock()

	r.handleAgentEnd()

	if !group.sealed {
		t.Error("the open group should be sealed at agent end")
	}
	if unfinished.groupEndAt == nil {
		t.Error("an unfinished tool should record its group end")
	}
	r.mu.Lock()
	interrupted := r.interruptedToolCallID["unfinished"]
	turnStart := r.turnStartMs
	_, emptyPresent := r.groups[empty]
	preparing := len(r.preparingByIndex)
	r.mu.Unlock()
	if !interrupted || !unfinished.interrupted {
		t.Error("an unfinished tool should be marked interrupted")
	}
	if emptyPresent {
		t.Error("an empty group should be dropped")
	}
	if preparing != 0 {
		t.Errorf("preparingByIndex = %d", preparing)
	}
	_ = turnStart
	if tui.renderCount() == 0 {
		t.Error("agent end should request a render")
	}
}

// Test sealHistoricalGroups (TS L2263-2271).
func TestSealHistoricalGroups(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 8_000 })
	group := r.newToolGroup(0)
	tool := newTestTool("history", "read", nil).pending()
	group.addTool(tool)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	done := make(chan struct{})
	close(done) // hand-built timer: no goroutine was started for it
	r.animTimer = &animTimer{stop: make(chan struct{}), done: done}
	r.mu.Unlock()

	r.sealHistoricalGroups()

	if !group.sealed {
		t.Error("a historical group should be sealed")
	}
	if tool.groupEndAt == nil || *tool.groupEndAt != 8_000 {
		t.Errorf("groupEndAt = %v", tool.groupEndAt)
	}
	r.mu.Lock()
	duration, timed := r.toolDurations["history"]
	interrupted := r.interruptedToolCallID["history"]
	active := r.lastActiveGroup
	timer := r.animTimer
	r.mu.Unlock()
	if !timed || duration != 0 {
		t.Errorf("duration = %v (timed=%v)", duration, timed)
	}
	if !interrupted || !tool.interrupted {
		t.Error("an unfinished historical tool is interrupted")
	}
	if active != nil {
		t.Error("lastActiveGroup should be cleared")
	}
	if timer != nil {
		t.Error("the animation timer should be stopped")
	}
}

// Test handleSessionStart wires the UI (TS L2220-2273).
func TestSessionStart(t *testing.T) {
	r := testRuntime(t)
	ui := newFakeUI()
	r.mu.Lock()
	r.activeUIContext = ui
	r.mu.Unlock()

	r.handleSessionStart(sdkZeroContext())

	// lastInputHandler takes ui.mu itself, so it runs outside the copy block.
	handler := ui.lastInputHandler()
	ui.mu.Lock()
	labels := append([]string(nil), ui.hiddenLabels...)
	widgets := append([]string(nil), ui.widgetKeys...)
	ui.mu.Unlock()

	if len(labels) != 1 || labels[0] != "" {
		t.Errorf("setHiddenThinkingLabel calls = %v, want one empty label", labels)
	}
	if handler == nil {
		t.Error("onTerminalInput was not registered")
	}
	if len(widgets) != 1 || widgets[0] != "compact-anim" {
		t.Errorf("widget keys = %v", widgets)
	}
	r.mu.Lock()
	accepting := r.acceptingEvents
	theme := r.currentTheme
	r.mu.Unlock()
	if !accepting {
		t.Error("the runtime should accept events after session_start")
	}
	_ = theme
}

// Test handleSessionShutdown (TS L2274-2278).
func TestSessionShutdown(t *testing.T) {
	r, ui, _ := startedRuntime(t)
	_ = ui
	r.openInspectSelectorModal()
	r.handleSessionShutdown()

	r.mu.Lock()
	phase := r.agentWorkPhase
	modal := r.activeSelectorComp
	accepting := r.acceptingEvents
	captured := r.capturedTui
	r.mu.Unlock()
	if phase != nil {
		t.Error("the work phase should be cleared")
	}
	if modal != nil {
		t.Error("the selector should be closed")
	}
	if accepting {
		t.Error("the runtime should stop accepting events")
	}
	if captured != nil {
		t.Error("the captured TUI should be released")
	}
}

// Test the terminal-input routing (TS L2228-2258).
func TestTerminalInput(t *testing.T) {
	r, _, tui := startedRuntime(t)
	_ = tui

	// Ctrl+I opens the selector and is consumed.
	verdict := r.handleTerminalInput("\x1b[105;5u")
	if !verdict.consume {
		t.Error("Ctrl+I should be consumed")
	}
	r.mu.Lock()
	selector := r.activeSelectorHandle
	r.mu.Unlock()
	if selector == nil {
		t.Fatal("Ctrl+I did not open the selector")
	}

	// With the selector open, Esc closes it and is consumed.
	verdict = r.handleTerminalInput("\x1b")
	if !verdict.consume {
		t.Error("Esc should be consumed while the selector is open")
	}
	r.mu.Lock()
	selector = r.activeSelectorHandle
	r.mu.Unlock()
	if selector != nil {
		t.Error("Esc did not close the selector")
	}

	// With the detail modal open, Esc closes it.
	r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("t", "read", nil)})
	verdict = r.handleTerminalInput("\x1b")
	if !verdict.consume {
		t.Error("Esc should be consumed while the detail modal is open")
	}
	r.mu.Lock()
	detail := r.activeModalHandle
	r.mu.Unlock()
	if detail != nil {
		t.Error("Esc did not close the detail modal")
	}

	// A plain key passes through both when nothing is open and while an overlay
	// is open.
	if verdict := r.handleTerminalInput("z"); verdict.consume {
		t.Error("a plain key should pass through")
	}
	r.openInspectSelectorModal()
	if verdict := r.handleTerminalInput("z"); verdict.consume {
		t.Error("a plain key should pass through an open overlay")
	}
	// A key release is consumed only while an overlay is open.
	if verdict := r.handleTerminalInput("\x1b[105;7:3u"); !verdict.consume {
		t.Error("a key release should be consumed while an overlay is open")
	}
	r.closeInspectSelectorModal()
	if verdict := r.handleTerminalInput("\x1b[105;7:3u"); verdict.consume {
		t.Error("a key release should pass through with no overlay")
	}

	// Ctrl+I toggles: it closes the modal too.
	r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("t", "read", nil)})
	if verdict := r.handleTerminalInput("\x1b[105;5u"); !verdict.consume {
		t.Error("Ctrl+I should be consumed while the detail modal is open")
	}
	r.mu.Lock()
	detail = r.activeModalHandle
	r.mu.Unlock()
	if detail != nil {
		t.Error("Ctrl+I did not close the detail modal")
	}
}

// Test renderWidget returns no lines (TS L2259-2262).
func TestRenderWidget(t *testing.T) {
	r, _, _ := startedRuntime(t)
	if lines := r.renderWidget(80); len(lines) != 0 {
		t.Errorf("widget lines = %v, want none", lines)
	}
}

// Test the event handler map covers every subscribed event.
func TestEventHandlerCoverage(t *testing.T) {
	events := []string{
		"session_start", "session_shutdown", "agent_start", "turn_start", "turn_end",
		"tool_execution_start", "tool_execution_end", "message_start", "message_end",
		"message_update", "agent_end",
	}
	for _, event := range events {
		if eventHandlerName(event) == "" {
			t.Errorf("event %q has no handler mapping", event)
		}
	}
}

// Test the zero sdk.Context safety rule: no host-facing helper panics on it.
func TestZeroContextSafety(t *testing.T) {
	r := testRuntime(t)
	zero := sdkZeroContext()

	// sdkThemeFrom degrades to the plain theme.
	if th := sdkThemeFrom(zero); th == nil {
		t.Error("sdkThemeFrom returned nil")
	}

	// notifyHost swallows the panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("a host helper panicked on a zero Context: %v", rec)
		}
	}()
	notifyHost(zero, "message", "info")
	safeCall(func() { _ = zero.HasUI() })
	if ui := r.uiContextFor(zero); ui != nil {
		t.Error("uiContextFor should return nil without a captured UI or host")
	}

	// The event handlers tolerate it.
	r.handleSessionStart(zero)
	r.handleSessionShutdown()
	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "user"}})
	r.handleMessageEnd(map[string]any{})
	r.handleMessageUpdate(map[string]any{})
	r.handleAgentEnd()
	r.handleToolExecutionStart(map[string]any{})
	r.handleToolExecutionEnd(map[string]any{})
	r.handleConfigCommand(zero)
}

// ── concurrency ────────────────────────────────────────────────────────────

// Test host calls never run under the runtime lock. The fake UI asserts that
// the lock is free when the host is called: a violation deadlocks or trips the
// race detector, and this test names it directly.
func TestHostCallsNeverUnderLock(t *testing.T) {
	// The animation ticker is not armed in this test, so an immediate TryLock is
	// an exact assertion: a goroutine that already holds mu can never acquire
	// it, and there is no background tick to confuse the result.
	run := func(withTUI bool) {
		r := testRuntime(t)
		lockedUI := &lockProbeUI{t: t, state: r}
		r.mu.Lock()
		r.activeUIContext = lockedUI
		if withTUI {
			r.capturedTui = &lockProbeTUI{t: t, state: r}
		} else {
			// No TUI handle: the overlays go through ui.Custom and the notices
			// through ui.Notify instead of addOverlay / requestRender.
			r.capturedTui = nil
		}
		r.mu.Unlock()

		r.handleSessionStart(sdkZeroContext())
		r.handleToolExecutionStart(map[string]any{"toolCallId": "a"})
		r.handleToolExecutionEnd(map[string]any{"toolCallId": "a"})
		r.handleMessageStart(map[string]any{"message": map[string]any{"role": "user"}})
		group := r.newToolGroup(0)
		group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
		r.mu.Lock()
		r.addGroupLocked(group)
		r.lastActiveGroup = group
		r.mu.Unlock()
		r.handleAgentEnd()
		r.openInspectSelectorModal()
		r.closeInspectSelectorModal()
		r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("d", "read", nil)})
		r.closeDetailModal()
		if ui, ok := any(lockedUI).(uiContext); ok {
			ui.notify("probe", "info")
		}
		r.stopAnimation()
	}
	run(true)
	run(false)
}

// lockProbeUI is a uiContext that fails the test if mu was held on entry.
type lockProbeUI struct {
	t     *testing.T
	state *runtimeState
	mu    sync.Mutex
	calls int
}

// check fails the test when mu is held on entry. The animation tick goroutine
// also takes mu briefly, so a single TryLock cannot tell a nested call from a
// concurrent holder: retry for a moment before declaring a violation.
func lockHeldNow(state *runtimeState) bool {
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		if state.mu.TryLock() {
			state.mu.Unlock()
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
}

func (u *lockProbeUI) check(name string) {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	if lockHeldNow(u.state) {
		u.t.Fatalf("%s was called while the runtime lock was held", name)
	}
}

func (u *lockProbeUI) setHiddenThinkingLabel(string) { u.check("setHiddenThinkingLabel") }
func (u *lockProbeUI) onTerminalInput(func(string) terminalInputVerdict) func() {
	u.check("onTerminalInput")
	return func() {}
}
func (u *lockProbeUI) setWidgetRenderer(string, func(int) []string) { u.check("setWidgetRenderer") }
func (u *lockProbeUI) notify(string, string)                        { u.check("notify") }
func (u *lockProbeUI) hasUI() bool {
	u.check("hasUI")
	return true
}
func (u *lockProbeUI) custom(factory remoteComponentFactory, _ any) (any, error) {
	u.check("custom")
	return nil, nil
}

// lockProbeTUI is a tuiHandle with the same assertion.
type lockProbeTUI struct {
	t     *testing.T
	state *runtimeState
}

func (v *lockProbeTUI) check(name string) {
	if lockHeldNow(v.state) {
		v.t.Fatalf("%s was called while the runtime lock was held", name)
	}
}

func (v *lockProbeTUI) requestRender()    { v.check("requestRender") }
func (v *lockProbeTUI) terminalRows() int { v.check("terminalRows"); return 24 }
func (v *lockProbeTUI) addOverlay(remoteComponent, int, int) overlayCloser {
	v.check("addOverlay")
	return &noopHandle{}
}

// noopHandle is a do-nothing overlay handle.
type noopHandle struct{}

func (noopHandle) hide() {}

// TestToolCardInvalidateRunsOffLock covers the invalidation channel the UI and
// TUI probes above cannot see: toolCardInvalidate is a raw func map, and
// invalidateGroupCards must collect those hooks under mu and invoke them after
// unlocking, because each one is a host call that re-enters the host render
// path. The probe fails the test when mu is still held at the call, so moving a
// callback inside the lock in invalidateGroupCardsExcept turns this test red.
//
// The three paths that repaint a group are driven in turn: the seal at a new
// user message, the agent_end walk that marks an unfinished call interrupted
// (Esc or a stream error), and a tool result landing afterwards.
func TestToolCardInvalidateRunsOffLock(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })

	calls := 0
	probe := func() {
		if lockHeldNow(r) {
			t.Error("toolCardInvalidate ran while the runtime lock was held")
			return
		}
		calls++
	}

	open := r.newToolGroup(0)
	other := r.newToolGroup(0)
	running := newTestTool("call-a", "ctx_run", map[string]any{"command": "sleep 5"}).pending()
	finished := newTestTool("call-b", "read", map[string]any{"path": "/b"}).pending()
	r.mu.Lock()
	r.addGroupLocked(open)
	r.addGroupLocked(other)
	r.lastActiveGroup = open
	r.toolCardInvalidate = map[string]func(){"call-a": probe, "call-b": probe}
	r.mu.Unlock()
	open.addTool(running)
	other.addTool(finished)

	// (a) A new user message seals the open group and repaints its cards.
	before := calls
	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "user"}})
	if calls == before {
		t.Fatal("sealing at a user message did not run the card invalidate hook")
	}

	// (b) Esc/abort: agent_end marks the still-running calls interrupted and
	// repaints the groups it mutates, sealed or not.
	before = calls
	r.handleAgentEnd()
	if calls == before {
		t.Fatal("the agent_end interrupt walk did not run the card invalidate hook")
	}

	// (c) A tool result lands after the abort and repaints its group.
	before = calls
	r.applyToolResult("call-b", doneResult("b"))
	if calls == before {
		t.Fatal("applyToolResult did not run the card invalidate hook")
	}
}

// Test a full turn under concurrent readers: the race detector is the
// assertion. Every path below is one the host really drives concurrently
// (renders on the TUI thread, events on the request goroutines).
func TestConcurrentTurnRace(t *testing.T) {
	r, ui, tui := startedRuntime(t)
	_ = ui
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	const workers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Renderers.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.mu.Lock()
				groups := make([]*toolGroupComponent, 0, len(r.groups))
				for group := range r.groups {
					groups = append(groups, group)
				}
				r.mu.Unlock()
				for _, group := range groups {
					_ = group.Render(60)
				}
				// The two modals read the same live entries and tools the events
				// write, so they render from value snapshots (see snapshotEntry).
				selector := r.newInspectSelectorModal()
				_ = selector.Render(80)
				_ = (&detailModalComponent{tui: r, entry: groupEntry{kind: "tool", tool: newTestTool("r", "read", map[string]any{"path": "/p"})}}).Render(80)
				_ = chat.Render(80)
				_ = tui.renderCount()
			}
		}()
	}

	// Event producers.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				id := fmt.Sprintf("call-%d-%d", seed, j)
				r.handleToolExecutionStart(map[string]any{"toolCallId": id})
				group := r.newToolGroup(0)
				tool := newTestTool(id, "read", map[string]any{"path": "/p"})
				group.addTool(tool)
				r.mu.Lock()
				r.addGroupLocked(group)
				r.lastActiveGroup = group
				r.mu.Unlock()
				r.handleToolExecutionEnd(map[string]any{"toolCallId": id})
				// Tool state is shared with the renderers, so it is written under
				// mu, exactly as an event handler does.
				r.mu.Lock()
				tool.succeeded("ok")
				r.mu.Unlock()
				r.rememberToolStart(id, 0)
				_ = r.allInspectableEntries()
			}
		}(i)
	}

	// Message/thinking producers.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			content := []any{map[string]any{"type": "thinking", "thinking": "thought"}}
			for j := 0; j < 20; j++ {
				r.handleMessageUpdate(map[string]any{
					"message":               map[string]any{"role": "assistant", "content": content},
					"assistantMessageEvent": map[string]any{"type": "thinking_start", "contentIndex": float64(seed)},
				})
				r.handleMessageUpdate(map[string]any{
					"message":               map[string]any{"role": "assistant", "content": content},
					"assistantMessageEvent": map[string]any{"type": "thinking_end", "contentIndex": float64(seed)},
				})
			}
		}(i)
	}

	// Re-anchoring producers: anchorGroupBeforeCurrentText splices a group out
	// of one container and into another while the renderers read both.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				group := r.newToolGroup(0)
				group.addTool(newTestTool(fmt.Sprintf("anchor-%d-%d", seed, j), "read", nil))
				r.mu.Lock()
				r.addGroupLocked(group)
				r.mu.Unlock()
				r.anchorGroupBeforeCurrentText(group, j)
				r.mu.Lock()
				r.deleteGroupLocked(group)
				r.mu.Unlock()
			}
		}(i)
	}

	// Give the workers a moment to overlap, then stop the renderers.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// The producers finish on their own; stop the renderers once they have.
	time.Sleep(20 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("concurrent turn did not finish\n%s\n", string(buf[:n]))
	}

	// The animation timer may still be armed; stop it so the test leaves nothing
	// running.
	r.stopAnimation()
}

// Test the animation tick is single-flight and cancellable (TS L769-785).
func TestAnimationSingleFlightAndStop(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 0 })
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("p", "read", nil).pending()}}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.mu.Unlock()

	r.scheduleAnimation()
	r.mu.Lock()
	first := r.animTimer
	r.mu.Unlock()
	if first == nil {
		t.Fatal("no timer was armed")
	}
	// A second call does not replace the pending timer.
	r.scheduleAnimation()
	r.mu.Lock()
	second := r.animTimer
	r.mu.Unlock()
	if first != second {
		t.Error("scheduleAnimation armed a second timer")
	}

	// Stopping ends the tick chain: the delay elapses without a render.
	r.stopAnimation()
	r.mu.Lock()
	armed := r.animTimer
	r.mu.Unlock()
	if armed != nil {
		t.Error("stopAnimation left the timer armed")
	}
	before := tui.renderCount()
	time.Sleep(200 * time.Millisecond)
	if got := tui.renderCount(); got != before {
		t.Errorf("a stopped timer still rendered: %d -> %d", before, got)
	}
}

// blockingTUI blocks inside requestRender until it is released, so a test can
// hold an in-flight animation tick inside its host call.
type blockingTUI struct {
	mu      sync.Mutex
	renders int
	entered chan struct{}
	release chan struct{}
}

func newBlockingTUI() *blockingTUI {
	return &blockingTUI{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingTUI) requestRender() {
	b.mu.Lock()
	b.renders++
	b.mu.Unlock()
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
}

func (b *blockingTUI) terminalRows() int { return 24 }

func (b *blockingTUI) addOverlay(remoteComponent, int, int) overlayCloser { return &noopHandle{} }

func (b *blockingTUI) renderCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.renders
}

// Test stopAnimation waits for a tick that already fired (task 必修 4). The tick
// clears r.animTimer as soon as it fires and only then calls the host, so a stop
// that only looks at r.animTimer returns while the host call is still running.
// stopAnimation must wait for the in-flight tick's completion channel too, and
// it must do so for every concurrent caller: the asynchronous seal of
// sealHistoricalGroupsFrom can overlap handleSessionShutdown, and the loser of
// that race used to see an emptied r.animTicks and return while the tick was
// still blocked in the host.
func TestStopAnimationWaitsForInFlightTick(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 0 })
	blocking := newBlockingTUI()
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("p", "read", nil).pending()}}
	r.mu.Lock()
	r.capturedTui = blocking
	r.addGroupLocked(group)
	r.mu.Unlock()

	r.scheduleAnimation()
	// The tick fires after the spinner delay and blocks inside requestRender.
	select {
	case <-blocking.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the animation tick never reached the host call")
	}
	// By now the tick has cleared r.animTimer: exactly the state where the old
	// stopAnimation returned immediately.
	r.mu.Lock()
	armed := r.animTimer
	inflight := len(r.animTicks)
	r.mu.Unlock()
	if armed != nil {
		t.Fatal("the tick should have cleared the armed timer before calling the host")
	}
	if inflight != 1 {
		t.Fatalf("animTicks = %d, want the one in-flight tick registered", inflight)
	}

	stopped := make(chan struct{})
	go func() {
		r.stopAnimation()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stopAnimation returned while the tick's host call was still running")
	case <-time.After(150 * time.Millisecond):
	}

	// A second, concurrent stopAnimation -- the seal overlapping the shutdown --
	// must wait for that very same tick. It can only learn about the tick through
	// the global registry, so the first caller must not empty it.
	stoppedSecond := make(chan struct{})
	go func() {
		r.stopAnimation()
		close(stoppedSecond)
	}()
	select {
	case <-stoppedSecond:
		t.Fatal("a concurrent stopAnimation returned while the tick's host call was still running")
	case <-time.After(150 * time.Millisecond):
	}
	// Neither caller may have taken the in-flight channel out of the registry:
	// only the tick detaches it, and only once its host call has returned.
	r.mu.Lock()
	registered := len(r.animTicks)
	r.mu.Unlock()
	if registered != 1 {
		t.Fatalf("animTicks = %d while a stopAnimation waits, want the in-flight tick still registered", registered)
	}

	// Releasing the host call lets the tick finish, and only then do the stops
	// return -- both of them.
	close(blocking.release)
	for _, done := range []chan struct{}{stopped, stoppedSecond} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("stopAnimation did not return after the in-flight tick completed")
		}
	}

	// No further host call may follow the stop.
	before := blocking.renderCount()
	time.Sleep(250 * time.Millisecond)
	if got := blocking.renderCount(); got != before {
		t.Errorf("a host call followed stopAnimation: %d -> %d", before, got)
	}
	r.mu.Lock()
	left := len(r.animTicks)
	r.mu.Unlock()
	if left != 0 {
		t.Errorf("animTicks = %d after stopAnimation, want 0", left)
	}
}

// Test two runtimes are fully independent (no package-level state).
func TestRuntimesAreIndependent(t *testing.T) {
	first := testRuntime(t)
	second := testRuntime(t)
	first.maxGroupEntries = 2
	second.maxGroupEntries = 9
	first.mu.Lock()
	a := first.maxGroupEntriesLocked()
	first.mu.Unlock()
	second.mu.Lock()
	b := second.maxGroupEntriesLocked()
	second.mu.Unlock()
	if a != 2 || b != 9 {
		t.Errorf("configs leaked across runtimes: %d / %d", a, b)
	}
}

// Test the config path honours the PIG_HOME red line end to end.
func TestConfigPathHonoursPigHome(t *testing.T) {
	t.Setenv("PIG_HOME", t.TempDir())
	r := createRuntimeState()
	r.configPath = configPathFor(func(key string) string {
		if key == "PIG_HOME" {
			return "/tmp/fake-pig-home"
		}
		return ""
	}, func() (string, error) { return "/home/user", nil })
	if !strings.HasPrefix(r.configPath, "/tmp/fake-pig-home") {
		t.Errorf("config path = %q, want it under PIG_HOME", r.configPath)
	}
	if !strings.HasSuffix(r.configPath, "/agent/compact-ui.json") {
		t.Errorf("config path = %q", r.configPath)
	}
}

// ── round-2 review fixes ────────────────────────────────────────────────────

// Test message_start(assistant) keeps the live assistant it mounts (task 必修 1).
// The assistant branch must reset the previous message stream *before* mounting
// the new live assistant: resetting afterwards dropped the freshly mounted
// component (assistantMessageStartLocked clears lastStreamingComp), and the
// first thinking stream of the turn then found no mount point --
// ensureThinkingGroup bails on a nil one, so the run was never grouped.
func TestMessageStartAssistantKeepsLiveAssistant(t *testing.T) {
	r, _, tui := startedRuntime(t)

	// No pre-mounted assistant: the event itself has to create one, exactly as
	// the production host does for the first assistant message of a turn.
	r.handleMessageStart(map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{}},
	})

	r.mu.Lock()
	streaming := r.lastStreamingComp
	chat := r.lastChatContainer
	r.mu.Unlock()
	if streaming == nil {
		t.Fatal("message_start(assistant) left lastStreamingComp nil")
	}
	if chat == nil || !chat.contains(streaming) {
		t.Fatal("the live assistant is not mounted in the chat container")
	}

	// The first thinking stream of the turn must land in a group mounted right
	// after that live assistant.
	content := []any{map[string]any{"type": "thinking", "thinking": "step one"}}
	r.handleMessageUpdate(map[string]any{
		"message":               map[string]any{"role": "assistant", "content": content},
		"assistantMessageEvent": map[string]any{"type": "thinking_start", "contentIndex": float64(0)},
	})
	r.handleMessageUpdate(map[string]any{
		"message":               map[string]any{"role": "assistant", "content": content},
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": float64(0)},
	})

	r.mu.Lock()
	groups := r.groupsListLocked()
	unattached := len(r.unattachedThinking)
	active := r.activeThinking
	children := chat.snapshot()
	r.mu.Unlock()
	if unattached != 0 {
		t.Errorf("the run was never grouped: %d unattached entries left", unattached)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want the one the thinking stream opened", len(groups))
	}
	group := groups[0]
	if len(group.entries) != 1 || group.entries[0].kind != "thinking" {
		t.Fatalf("group entries = %+v, want the streamed thinking run", group.entries)
	}
	if group.entries[0].think != active {
		t.Error("the grouped thinking run is not the active one")
	}
	if group.mount != chat {
		t.Error("the thinking group is not mounted in the chat container")
	}
	if idx := chat.indexOf(group); idx != chat.indexOf(streaming)+1 {
		t.Errorf("thinking group at %d, want right after the live assistant at %d", idx, chat.indexOf(streaming))
	}
	if len(children) != 2 {
		t.Errorf("chat children = %d, want the live assistant plus the thinking group", len(children))
	}
	if tui.renderCount() == 0 {
		t.Error("grouping the run should request a render")
	}
}

// Test a provisional tool call is converted into the executing one in place
// (task 必修 2): same entry, same component, and the component joins the group's
// children -- the list agent_end walks to mark an interrupted call. The port
// used to only clear the provisional link, which left comp.preparing set (so
// later stream events reopened a provisional entry), comp.owner nil and the
// component absent from group.children (so the interrupt walk missed it).
func TestPreparingToolAbsorbedInPlace(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })

	// A real assistant turn: message_start mounts the live assistant.
	r.handleMessageStart(map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{}},
	})

	toolBlock := map[string]any{
		"type":      "toolCall",
		"id":        "call-1",
		"name":      "read",
		"arguments": map[string]any{"path": "/p"},
	}
	message := map[string]any{"role": "assistant", "content": []any{toolBlock}}
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_start", "contentIndex": float64(0)},
	})
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_end", "contentIndex": float64(0)},
	})

	r.mu.Lock()
	preparing := r.preparingByIndex[0]
	group := r.lastActiveGroup
	r.mu.Unlock()
	if preparing == nil || preparing.comp == nil {
		t.Fatal("the streamed tool call mounted no provisional component")
	}
	if group == nil {
		t.Fatal("the streamed tool call opened no group")
	}
	comp := preparing.comp
	if len(group.entries) != 1 || group.entries[0].preparing != preparing {
		t.Fatalf("group entries = %+v, want the single provisional entry", group.entries)
	}

	// tool_execution_start for the same id converts that very component.
	r.handleToolExecutionStart(map[string]any{
		"toolCallId": "call-1",
		"toolName":   "read",
		"args":       map[string]any{"path": "/p"},
	})

	r.mu.Lock()
	preparingLeft := len(r.preparingByIndex)
	compPreparing := comp.preparing
	compOwner := comp.owner
	executionStarted := comp.executionStarted
	compIndex, compHasIndex := comp.contentIndex, comp.hasIndex
	compArgs := comp.args["path"]
	childCount, entryCount, linkedEntries := 0, 0, 0
	for _, entry := range group.entries {
		if entry.kind == "tool" && entry.tool == comp {
			entryCount++
			if entry.preparing != nil {
				linkedEntries++
			}
		}
	}
	for _, child := range group.children {
		if child == comp {
			childCount++
		}
	}
	r.mu.Unlock()
	if preparingLeft != 0 {
		t.Errorf("preparingByIndex = %d, want the provisional entry retired", preparingLeft)
	}
	if compPreparing {
		t.Error("comp.preparing is still set: later stream events would reopen a provisional entry")
	}
	if compOwner != group {
		t.Errorf("comp.owner = %v, want the group", compOwner)
	}
	if childCount != 1 {
		t.Errorf("group.children holds the component %d times, want exactly 1", childCount)
	}
	if entryCount != 1 || linkedEntries != 0 {
		t.Errorf("group.entries holds %d entries (%d still linked to a provisional), want 1 and 0", entryCount, linkedEntries)
	}
	if !executionStarted {
		t.Error("the absorbed component is not marked as executing")
	}
	if !compHasIndex || compIndex != 0 {
		t.Errorf("content index = %d (set=%v), want the absorbed 0", compIndex, compHasIndex)
	}
	if compArgs != "/p" {
		t.Errorf("args = %v, want the absorbed arguments", comp.args)
	}

	// A further stream update now lands on the grouped tool: no second entry,
	// no new provisional entry.
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_delta", "contentIndex": float64(0)},
	})
	r.mu.Lock()
	entriesAfter := len(group.entries)
	preparingAfter := len(r.preparingByIndex)
	childrenAfter := len(group.children)
	r.mu.Unlock()
	if entriesAfter != 1 || childrenAfter != 1 {
		t.Errorf("after the stream update: entries=%d children=%d, want 1 and 1", entriesAfter, childrenAfter)
	}
	if preparingAfter != 0 {
		t.Errorf("a stream update reopened a provisional entry: %d", preparingAfter)
	}

	// agent_end interrupts the call: the walk over group.children is what has to
	// see this tool.
	r.setClock(func() int64 { return 4_000 })
	r.handleAgentEnd()
	r.mu.Lock()
	interrupted := r.interruptedToolCallID["call-1"]
	groupEndAt := comp.groupEndAt
	duration, timed := r.toolDurations["call-1"]
	r.mu.Unlock()
	if !interrupted || !comp.interrupted {
		t.Error("the absorbed tool was not marked interrupted by agent_end")
	}
	if groupEndAt == nil || *groupEndAt != 4_000 {
		t.Errorf("groupEndAt = %v, want 4000", groupEndAt)
	}
	if timed {
		t.Errorf("duration = %v, want the interrupt walk to leave the duration map alone", duration)
	}
	if got := toolElapsed(r, comp); got != "3.00s" {
		t.Errorf("elapsed = %q, want the 3.00s the stamped group end implies", got)
	}
	if got := toolStatus(comp, group); got != "error" {
		t.Errorf("status = %q, want error for an interrupted call", got)
	}
	if tui.renderCount() == 0 {
		t.Error("agent_end should request a render")
	}
}

// Test the same in-place conversion on the normal tool_execution_end path
// (task 必修 2): the component keeps its single group entry, takes the result
// and reports success instead of staying pending.
func TestPreparingToolAbsorbedThenCompleted(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	r.handleMessageStart(map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{}},
	})

	toolBlock := map[string]any{
		"type":      "toolCall",
		"id":        "call-9",
		"name":      "read",
		"arguments": map[string]any{"path": "/q"},
	}
	message := map[string]any{"role": "assistant", "content": []any{toolBlock}}
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_start", "contentIndex": float64(0)},
	})
	r.handleToolExecutionStart(map[string]any{
		"toolCallId": "call-9",
		"toolName":   "read",
		"args":       map[string]any{"path": "/q"},
	})
	r.mu.Lock()
	group := r.lastActiveGroup
	r.mu.Unlock()
	if group == nil {
		t.Fatal("no group was opened")
	}
	var comp *toolComponent
	for _, entry := range group.entries {
		if entry.kind == "tool" {
			comp = entry.tool
		}
	}
	if comp == nil {
		t.Fatal("the group has no tool entry")
	}

	r.setClock(func() int64 { return 3_500 })
	r.handleToolExecutionEnd(map[string]any{
		"toolCallId": "call-9",
		"result":     map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}},
	})

	r.mu.Lock()
	entries := len(group.entries)
	children := len(group.children)
	compPreparing := comp.preparing
	compOwner := comp.owner
	resultText := ""
	if comp.result != nil && len(comp.result.content) > 0 {
		resultText = comp.result.content[0].text
	}
	duration, timed := r.toolDurations["call-9"]
	r.mu.Unlock()
	if entries != 1 || children != 1 {
		t.Errorf("entries=%d children=%d, want 1 and 1", entries, children)
	}
	if compPreparing || compOwner != group {
		t.Errorf("preparing=%v owner=%v, want false and the group", compPreparing, compOwner)
	}
	if resultText != "done" {
		t.Errorf("result = %q, want the applied output", resultText)
	}
	if !timed || duration != 2.5 {
		t.Errorf("duration = %v (timed=%v), want 2.5s", duration, timed)
	}
	if got := toolStatus(comp, group); got != "success" {
		t.Errorf("status = %q, want success", got)
	}
	if tui.renderCount() == 0 {
		t.Error("the tool end should request a render")
	}
}

// TestDisplayToolSkipsTrailingNonToolEntry covers the display-rights rule of a
// group whose newest visible entry is not a tool (brief2 R3 M1). A thinking run
// that keeps streaming after the call, or an absorbed `cache miss` notice, used
// to take the tree away from the group's only tool: the streaming call lives in
// `entries` only (nothing mounted it in children yet), so reading the last
// visible entry alone found no tool, fell back to the empty child list and left
// every card of the group unpainted -- the whole tree vanished from the screen.
func TestDisplayToolSkipsTrailingNonToolEntry(t *testing.T) {
	cases := []struct {
		name    string
		trailer groupEntry
	}{
		{
			name: "thinking",
			trailer: groupEntry{kind: "thinking", think: &thinkingEntry{
				kind: "thinking", id: 1, active: true, text: "weighing options",
			}},
		},
		{
			name:    "notice",
			trailer: groupEntry{kind: "notice", notice: &noticeEntry{kind: "notice", text: "cache miss: 42 tokens re-billed"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := modalRuntime(t)
			group := r.newToolGroup(0)
			comp := newTestTool("call-a", "ctx_run", map[string]any{"command": "echo a"})
			comp.isPartial = true
			prep := &preparingTool{
				toolCallID: "call-a", toolName: "ctx_run", args: comp.args,
				owner: group, comp: comp, hasIndex: true,
			}
			r.mu.Lock()
			group.entries = []groupEntry{{kind: "tool", tool: comp, preparing: prep}, tc.trailer}
			r.addGroupLocked(group)
			r.lastActiveGroup = group
			displayOK := isGroupHostDisplayToolLocked(group, comp)
			displayID := group.displayCardIDLocked()
			r.mu.Unlock()

			if !displayOK {
				t.Errorf("isGroupHostDisplayToolLocked = false: the trailing %s entry holds the display rights while no other card paints the group", tc.name)
			}
			if displayID != "call-a" {
				t.Errorf("displayCardIDLocked = %q, want the streaming call's card call-a", displayID)
			}
			lines := r.toolRenderLines("ctx_run", comp.args, sdk.ToolRenderContext{
				ToolCallID: "call-a",
				IsPartial:  true,
			}, nil, false, 80, false)
			if len(lines) < 2 {
				t.Fatalf("card lines = %v, want the group tree (header + row)", lines)
			}
			if !containsLine(lines, "echo a") {
				t.Errorf("card lines = %v, want the tool row inside the tree", lines)
			}
		})
	}
}

// TestAgentEndKeepsInterruptedStreamingTool is the regression for a streaming
// call the host already mounted as a card and that is then cut short by Esc (or
// a stream error) before tool_execution_start (brief2 R3 M2). The host mounts a
// card as soon as the streamed block carries an id (interactive_events.go,
// MessageUpdateEvent -> appendToChat) and keeps it across the abort; the
// original keeps that component in the group (addTool moves it into
// group.children, agent_end's walk marks it `_interrupted`), so it renders as an
// interrupted row inside the tree. Detaching the provisional entry instead left
// the card with no group -- a bare row outside the tree -- and the sibling card
// that had already yielded the display rights stayed blank.
func TestAgentEndKeepsInterruptedStreamingTool(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	r.handleMessageStart(map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{}},
	})

	firstBlock := map[string]any{"type": "toolCall", "id": "call-a", "name": "read", "arguments": map[string]any{"path": "/a"}}
	secondBlock := map[string]any{"type": "toolCall", "id": "call-b", "name": "ctx_run", "arguments": map[string]any{"command": "sleep 5"}}
	message := map[string]any{"role": "assistant", "content": []any{firstBlock, secondBlock}}
	for _, index := range []float64{0, 1} {
		r.handleMessageUpdate(map[string]any{
			"message":               message,
			"assistantMessageEvent": map[string]any{"type": "toolcall_start", "contentIndex": index},
		})
	}

	invalidates := map[string]int{}
	r.mu.Lock()
	group := r.lastActiveGroup
	var firstComp, secondComp *toolComponent
	if group != nil {
		for _, entry := range group.entries {
			if entry.kind != "tool" || entry.tool == nil {
				continue
			}
			switch entry.tool.toolCallID {
			case "call-a":
				firstComp = entry.tool
			case "call-b":
				secondComp = entry.tool
			}
		}
	}
	r.toolCardInvalidate = map[string]func(){
		"call-a": func() { invalidates["call-a"]++ },
		"call-b": func() { invalidates["call-b"]++ },
	}
	r.mu.Unlock()
	if group == nil || firstComp == nil || secondComp == nil {
		t.Fatalf("streamed calls not mounted: group=%v first=%v second=%v", group != nil, firstComp != nil, secondComp != nil)
	}

	// Esc/abort: the host keeps both mounted cards and emits agent_end without
	// ever sending tool_execution_start.
	r.handleAgentEnd()

	r.mu.Lock()
	kept, linked := 0, 0
	for _, entry := range group.entries {
		if entry.kind != "tool" || entry.tool == nil {
			continue
		}
		kept++
		if entry.preparing != nil {
			linked++
		}
	}
	_, interruptedMap := r.interruptedToolCallID["call-b"]
	displaySecond := isGroupHostDisplayToolLocked(group, secondComp)
	displayFirst := isGroupHostDisplayToolLocked(group, firstComp)
	groupSealed := group.sealed
	r.mu.Unlock()

	if kept != 2 {
		t.Errorf("group entries after agent_end = %d, want both streamed calls kept inside the tree", kept)
	}
	if linked != 0 {
		t.Errorf("%d entries are still linked to a provisional record, want the call converted in place", linked)
	}
	if !groupSealed {
		t.Error("agent_end should seal the open group")
	}
	if !interruptedMap || !secondComp.interrupted {
		t.Error("the interrupted streamed call is not marked interrupted")
	}
	if !displaySecond || displayFirst {
		t.Errorf("display rights: second=%v first=%v, want the newest call to paint the tree", displaySecond, displayFirst)
	}
	if got := toolStatus(secondComp, group); got != "error" {
		t.Errorf("status = %q, want error for an interrupted call", got)
	}

	tree := r.toolRenderLines("ctx_run", secondComp.args, sdk.ToolRenderContext{
		ToolCallID: "call-b",
		IsError:    true,
	}, nil, false, 80, false)
	if len(tree) < 3 {
		t.Fatalf("interrupted card lines = %v, want the group tree (header + both rows)", tree)
	}
	if !containsLine(tree, "sleep 5") || !containsLine(tree, "read") {
		t.Errorf("interrupted card lines = %v, want both streamed calls inside the tree", tree)
	}
	if got := r.toolRenderLines("read", firstComp.args, sdk.ToolRenderContext{
		ToolCallID: "call-a",
		IsError:    true,
	}, nil, false, 80, false); len(got) != 0 {
		t.Errorf("sibling card lines = %v, want empty (the display card owns the tree)", got)
	}
	if invalidates["call-b"] == 0 {
		t.Error("the interrupted card was not invalidated: it keeps painting the pending frame")
	}
	if invalidates["call-a"] == 0 {
		t.Error("the sibling card was not invalidated: it keeps its previous frame")
	}
}

// TestStateChangeInvalidatesDisplayCard covers the three state changes that left
// the tree on screen showing its previous frame (brief2 R3 S2): a non-display
// tool finishing after the spinner stopped, an absorbed `cache miss` notice, and
// the agent_end seal. A host card re-renders only when it is dirty or its
// Invalidate hook fires (tui/tool_execution.go: Render reuses definitionCall
// until Invalidate), so each of them has to invalidate the group's cards.
func TestStateChangeInvalidatesDisplayCard(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	group := r.newToolGroup(0)
	older := newTestTool("call-a", "read", map[string]any{"path": "/a"}).pending()
	display := newTestTool("call-b", "ctx_run", map[string]any{"command": "echo b"}).pending()
	group.addTool(older)
	group.addTool(display)
	invalidates := map[string]int{}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.toolCardInvalidate = map[string]func(){
		"call-a": func() { invalidates["call-a"]++ },
		"call-b": func() { invalidates["call-b"]++ },
	}
	r.mu.Unlock()

	// 1) A non-display tool finishes: the display card still paints the old row.
	r.applyToolResult("call-a", doneResult("a-out"))
	if invalidates["call-b"] == 0 {
		t.Error("applyToolResult left the display card with its previous frame")
	}

	// 2) An absorbed cache-miss notice appends a visible entry.
	invalidates["call-a"], invalidates["call-b"] = 0, 0
	chat := newContainer(r)
	r.mu.Lock()
	r.lastChatContainer = chat
	r.mu.Unlock()
	if !r.absorbCacheMissNotice(chat, "cache miss: 1234 tokens re-billed") {
		t.Fatal("the cache-miss notice was not absorbed into the open group")
	}
	if invalidates["call-b"] == 0 {
		t.Error("absorbCacheMissNotice left the display card with its previous frame")
	}

	// 3) agent_end seals the group: the header flips to its final state and the
	// spinner stops, so the card has to repaint.
	invalidates["call-a"], invalidates["call-b"] = 0, 0
	r.handleAgentEnd()
	if invalidates["call-b"] == 0 {
		t.Error("agent_end left the display card with its previous frame")
	}
	tree := r.toolRenderLines("ctx_run", display.args, sdk.ToolRenderContext{ToolCallID: "call-b"}, nil, false, 80, false)
	if len(tree) == 0 {
		t.Fatal("the sealed group's display card rendered no lines")
	}
	if !strings.Contains(tree[0], "tools done") {
		t.Errorf("header = %q, want the sealed group's final state", tree[0])
	}
}

// hostCardModel is the host's ToolExecutionComponent contract for one card: it
// paints through the extension's ToolRenderer, and it keeps that frame until its
// Invalidate hook fires (tui/tool_execution.go: Render reuses definitionCall
// until Invalidate). A test that asserts on card.lines therefore asserts on what
// the user actually sees, not on what the extension would render today.
type hostCardModel struct {
	r           *runtimeState
	name        string
	args        map[string]any
	toolCallID  string
	width       int
	lines       []string
	invalidates int
}

func newHostCardModel(r *runtimeState, name, toolCallID string, args map[string]any, width int) *hostCardModel {
	card := &hostCardModel{r: r, name: name, args: args, toolCallID: toolCallID, width: width}
	card.paint()
	return card
}

func (c *hostCardModel) paint() {
	c.lines = c.r.toolRenderLines(c.name, c.args, sdk.ToolRenderContext{ToolCallID: c.toolCallID}, nil, false, c.width, false)
}

func (c *hostCardModel) invalidate() {
	c.invalidates++
	c.paint()
}

// TestTextSealRepaintsHostCardOnce is the regression for brief2 R4 A: after a
// normal turn whose tool calls all finished and whose final text had already
// streamed, the tree header stayed at "waiting for response..." for good.
//
// Three decisions compounded. flushPendingTextSeal set active.sealed and only
// dropped the extension's own render cache; needsAnimationLocked answers false
// for a sealed group, so the animation tick stopped repainting the card; and
// handleAgentEnd skips its seal branch for an already-sealed group. None of them
// told the host, so the ToolExecutionComponent kept the frame it painted before
// the seal -- and for a finished turn nothing repaints it again.
//
// The fix routes every group-state change through invalidateGroupCards, so this
// asserts both halves of the contract: the header reaches its terminal state and
// the display card is invalidated exactly once (no silent no-op, no repaint
// storm).
func TestTextSealRepaintsHostCardOnce(t *testing.T) {
	r, _, _ := startedRuntime(t)
	now := int64(1_000)
	r.setClock(func() int64 { return now })

	// Turn start, assistant message start, one tool call streams in with a real
	// id, executes and finishes: the ordinary "tool then answer" turn.
	r.handleMessageStart(map[string]any{"message": map[string]any{"role": "user", "content": []any{}}})
	assistant := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "toolCall", "id": "call-a", "name": "read", "arguments": map[string]any{"path": "/a"}},
		map[string]any{"type": "text", "text": "all done"},
	}}
	r.handleMessageStart(map[string]any{"message": assistant})
	r.handleMessageUpdate(map[string]any{
		"message":               assistant,
		"assistantMessageEvent": map[string]any{"type": "toolcall_start", "contentIndex": float64(0)},
	})
	r.handleToolExecutionStart(map[string]any{"toolCallId": "call-a", "toolName": "read", "args": map[string]any{"path": "/a"}})
	now = 1_200
	r.handleToolExecutionEnd(map[string]any{
		"toolCallId": "call-a",
		"result":     map[string]any{"content": []any{map[string]any{"type": "text", "text": "a-out"}}},
	})
	// The turn's tools are done and the model has been asked for its answer: this
	// is the frame the R3 attachments caught, and the one that used to stick.
	r.setAgentWorkPhase(strPtr(phaseWaitingResponse))

	card := newHostCardModel(r, "read", "call-a", map[string]any{"path": "/a"}, 80)
	r.mu.Lock()
	r.toolCardInvalidate = map[string]func(){"call-a": card.invalidate}
	group := r.lastActiveGroup
	r.mu.Unlock()
	if group == nil {
		t.Fatal("the streamed call did not open a group")
	}
	if !containsLine(card.lines, "waiting for response...") {
		t.Fatalf("pre-seal card frame = %v, want the open group's \"waiting for response...\" header", card.lines)
	}

	// The final text streams: flushPendingTextSeal seals the group and anchors it
	// before that text.
	now = 1_300
	r.handleMessageUpdate(map[string]any{
		"message":               assistant,
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": float64(1)},
	})
	if !group.sealed {
		t.Fatal("the text delta did not seal the open group")
	}

	// Then the turn ends.
	r.handleAgentEnd()

	if got := card.invalidates; got != 1 {
		t.Errorf("display card invalidations across seal + agent_end = %d, want exactly 1", got)
	}
	if len(card.lines) == 0 {
		t.Fatal("the sealed group's display card rendered no lines")
	}
	if containsLine(card.lines, "waiting for response...") {
		t.Errorf("card frame after the turn = %v, want the terminal header, not the pre-seal frame", card.lines)
	}
	if !strings.Contains(card.lines[0], "tools done") {
		t.Errorf("tree header = %q, want the sealed group's terminal state", card.lines[0])
	}
	// The frame is not just terminal, it is current: it must match a fresh render.
	fresh := &hostCardModel{r: r, name: "read", args: map[string]any{"path": "/a"}, toolCallID: "call-a", width: 80}
	fresh.paint()
	if joinLines(card.lines) != joinLines(fresh.lines) {
		t.Errorf("painted frame = %v, want the current render %v", card.lines, fresh.lines)
	}
}

func tuiStrip(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') {
				inEsc = false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
