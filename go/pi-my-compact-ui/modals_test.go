package compactui

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// This file covers src/modals.ts (L1563-2080) and src/settings.ts
// (L2084-2191).

// modalRuntime builds a runtime with a fake TUI captured.
func modalRuntime(t *testing.T) (*runtimeState, *fakeTUI) {
	t.Helper()
	r := testRuntime(t)
	tui := newFakeTUI(24)
	r.mu.Lock()
	r.capturedTui = tui
	r.mu.Unlock()
	return r, tui
}

// selectorRuntimeWithCustomUI builds a selector runtime with no TUI handle but a
// live uiContext, which is the ctx.ui.Custom branch: the host exposes no
// addOverlay, so the two modals must share one Custom root.
func selectorRuntimeWithCustomUI(t *testing.T) (*runtimeState, *fakeUI) {
	t.Helper()
	r, _ := selectorRuntime(t)
	ui := newFakeUI()
	r.mu.Lock()
	r.capturedTui = nil
	r.activeUIContext = ui
	r.mu.Unlock()
	return r, ui
}

// ── DetailModalComponent ───────────────────────────────────────────────────

// Test the detail modal renders a tool entry (TS L1651-1800).
func TestDetailModalToolEntry(t *testing.T) {
	r, tui := modalRuntime(t)
	tool := newTestTool("t", "bash", map[string]any{"command": "go test ./..."}).succeeded("all good")
	modal := r.newDetailModal(groupEntry{kind: "tool", tool: tool})

	lines := modal.Render(80)
	if len(lines) < 2 {
		t.Fatalf("modal lines = %d", len(lines))
	}
	if !strings.Contains(lines[0], "bash") {
		t.Errorf("title = %q", lines[0])
	}
	if !strings.Contains(lines[0], "[Esc to close]") {
		t.Errorf("title lacks the close hint: %q", lines[0])
	}
	joined := joinLines(lines)
	for _, want := range []string{"$ go test ./...", "── Output ──", "all good"} {
		if !strings.Contains(joined, want) {
			t.Errorf("modal is missing %q:\n%s", want, joined)
		}
	}
	for _, line := range lines {
		if tuiLen(line) > 80 {
			t.Errorf("line %q is %d columns, want <= 80", line, tuiLen(line))
		}
	}
	_ = tui
	modal.dispose()
}

// Test the tool branch fallbacks: path, generic args and no output.
func TestDetailModalToolFallbacks(t *testing.T) {
	r, _ := modalRuntime(t)

	withPath := r.newDetailModal(groupEntry{kind: "tool", tool: newTestTool("a", "read", map[string]any{"path": "/tmp/x"}).succeeded("")})
	joined := joinLines(withPath.Render(80))
	if !strings.Contains(joined, "Path:") || !strings.Contains(joined, "/tmp/x") {
		t.Errorf("path branch = %s", joined)
	}
	if !strings.Contains(joined, "(no output)") {
		t.Errorf("a finished tool without output should say so: %s", joined)
	}
	withPath.dispose()

	generic := r.newDetailModal(groupEntry{kind: "tool", tool: newTestTool("b", "mystery", map[string]any{"alpha": 1.0}).succeeded("done")})
	joined = joinLines(generic.Render(80))
	if !strings.Contains(joined, "alpha") {
		t.Errorf("generic args should be pretty-printed: %s", joined)
	}
	generic.dispose()

	// A pending tool says it is waiting for output.
	pending := newTestTool("c", "read", nil).pending()
	waiting := r.newDetailModal(groupEntry{kind: "tool", tool: pending})
	if !strings.Contains(joinLines(waiting.Render(80)), "(running, waiting for output...)") {
		t.Errorf("pending tool = %s", joinLines(waiting.Render(80)))
	}
	waiting.dispose()
}

// Test the thinking and notice branches (TS L1658-1680).
func TestDetailModalThinkingAndNotice(t *testing.T) {
	r, _ := modalRuntime(t)

	think := &thinkingEntry{kind: "thinking", text: "line one\nline two", tokens: 1500}
	modal := r.newDetailModal(groupEntry{kind: "thinking", think: think})
	joined := joinLines(modal.Render(80))
	for _, want := range []string{"Thinking", "(1.5k)", "line one", "line two"} {
		if !strings.Contains(joined, want) {
			t.Errorf("thinking modal missing %q:\n%s", want, joined)
		}
	}
	modal.dispose()

	empty := r.newDetailModal(groupEntry{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "  "}})
	if !strings.Contains(joinLines(empty.Render(80)), "(thinking...)") {
		t.Errorf("empty thinking modal = %s", joinLines(empty.Render(80)))
	}
	empty.dispose()

	notice := r.newDetailModal(groupEntry{kind: "notice", notice: &noticeEntry{kind: "notice", text: "cache miss: 99 tokens re-billed"}})
	noticeLines := joinLines(notice.Render(80))
	if !strings.Contains(noticeLines, "cache miss") || !strings.Contains(noticeLines, "99 tokens re-billed") {
		t.Errorf("notice modal = %s", noticeLines)
	}
	notice.dispose()
}

// Test the key handling of the detail modal (TS L1620-1650).
func TestDetailModalKeys(t *testing.T) {
	r, _ := modalRuntime(t)
	tool := newTestTool("t", "bash", map[string]any{"command": "x"})
	// A long output makes the modal scrollable (more lines than the inner height).
	long := ""
	for i := 0; i < 60; i++ {
		long += "output line " + itoa(i) + "\n"
	}
	tool.succeeded(long)
	modal := r.newDetailModal(groupEntry{kind: "tool", tool: tool})
	_ = modal.Render(80)
	// Rendering with followingEnd pinned scrollTop at the bottom.
	atBottom := modal.scrollTop
	if atBottom == 0 {
		t.Fatal("a long output should have scrolled to the end")
	}

	// k / Up scroll up one line and release the follow-end pin. A navigation key
	// is consumed but does not close the overlay, so handleInput reports false.
	if modal.handleInput("k") {
		t.Error("a navigation key must not close the modal")
	}
	if modal.scrollTop != atBottom-1 {
		t.Errorf("k scrollTop = %d, want %d", modal.scrollTop, atBottom-1)
	}
	if modal.followingEnd {
		t.Error("scrolling up should release the follow-end pin")
	}
	modal.handleInput("j")
	if modal.scrollTop != atBottom {
		t.Errorf("j scrollTop = %d, want %d", modal.scrollTop, atBottom)
	}

	// PageUp / End / Home.
	modal.handleInput("\x1b[5~")
	if modal.scrollTop != maxInt(0, atBottom-10) {
		t.Errorf("PageUp scrollTop = %d, want %d", modal.scrollTop, maxInt(0, atBottom-10))
	}
	if modal.handleInput("\x1b[F") {
		t.Error("End must not close the modal")
	}
	if modal.scrollTop != atBottom {
		t.Errorf("End scrollTop = %d, want %d", modal.scrollTop, atBottom)
	}
	modal.handleInput("\x1b[H")
	if modal.scrollTop != 0 {
		t.Errorf("Home scrollTop = %d, want 0", modal.scrollTop)
	}

	// An unknown key neither scrolls nor closes.
	if modal.handleInput("z") {
		t.Error("an unknown key should not close the modal")
	}

	// Esc closes the modal through the runtime.
	r.mu.Lock()
	r.activeModalComponent = modal
	r.mu.Unlock()
	modal.handleInput("\x1b")
	r.mu.Lock()
	open := r.activeModalComponent
	r.mu.Unlock()
	if open != nil {
		t.Error("Esc did not close the modal")
	}
}

// Test openDetailModal / closeDetailModal through a fake TUI
// (TS L1801-1855).
func TestDetailModalLifecycle(t *testing.T) {
	r, tui := modalRuntime(t)
	r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("t", "read", nil)})

	r.mu.Lock()
	comp := r.activeModalComponent
	handle := r.activeModalHandle
	r.mu.Unlock()
	if comp == nil || handle == nil {
		t.Fatalf("modal not opened (comp=%v handle=%v)", comp != nil, handle != nil)
	}
	if tui.renderCount() == 0 {
		t.Error("opening a modal should request a render")
	}
	tui.mu.Lock()
	if len(tui.overlays) != 1 {
		t.Errorf("overlays = %d", len(tui.overlays))
	}
	if tui.overlays[0].widthPct != 85 || tui.overlays[0].maxHeight != 80 {
		t.Errorf("overlay geometry = %dx%% / %d%%", tui.overlays[0].widthPct, tui.overlays[0].maxHeight)
	}
	tui.mu.Unlock()

	r.closeDetailModal()
	r.mu.Lock()
	comp = r.activeModalComponent
	handle = r.activeModalHandle
	r.mu.Unlock()
	if comp != nil || handle != nil {
		t.Error("closeDetailModal left state behind")
	}

	// A second close is safe.
	r.closeDetailModal()
}

// Test opening a detail modal closes the previous one (TS L1802).
func TestDetailModalReplacesPrevious(t *testing.T) {
	r, _ := modalRuntime(t)
	r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("a", "read", nil)})
	r.mu.Lock()
	first := r.activeModalComponent
	r.mu.Unlock()

	r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("b", "write", nil)})
	r.mu.Lock()
	second := r.activeModalComponent
	r.mu.Unlock()
	if second == nil || second == first {
		t.Error("the new modal should have replaced the old one")
	}
	if !first.disposed {
		t.Error("the replaced modal should be disposed")
	}
}

// ── InspectSelectorModal ───────────────────────────────────────────────────

// selectorRuntime builds a runtime with groups holding inspectable entries.
func selectorRuntime(t *testing.T) (*runtimeState, *fakeTUI) {
	t.Helper()
	r, tui := modalRuntime(t)
	for i := 0; i < 3; i++ {
		group := r.newToolGroup(0)
		group.entries = []groupEntry{
			{kind: "tool", tool: newTestTool("t"+itoa(i), "read", map[string]any{"path": "/p" + itoa(i)}).succeeded("ok")},
			{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "thought " + itoa(i), tokens: 100 * (i + 1)}},
		}
		r.mu.Lock()
		r.addGroupLocked(group)
		r.mu.Unlock()
	}
	return r, tui
}

// Test allInspectableEntries gathers every visible entry (TS L1804-1813).
func TestAllInspectableEntries(t *testing.T) {
	r, _ := selectorRuntime(t)
	if got := len(r.allInspectableEntries()); got != 6 {
		t.Errorf("entries = %d, want 6", got)
	}
	empty := testRuntime(t)
	if got := len(empty.allInspectableEntries()); got != 0 {
		t.Errorf("empty runtime entries = %d", got)
	}
}

// Test the selector renders and tracks its selection (TS L1824-2060).
func TestInspectSelectorRender(t *testing.T) {
	r, _ := selectorRuntime(t)
	modal := r.newInspectSelectorModal()

	lines := modal.Render(80)
	joined := joinLines(lines)
	for _, want := range []string{"Inspect", "↑/↓ select · Enter view · Esc exit", "/6]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("selector missing %q:\n%s", want, joined)
		}
	}
	// The initial selection trails the newest entry, so the counter reads 6/6.
	if !strings.Contains(joined, "[6/6]") {
		t.Errorf("selector counter = %s", joined)
	}
	for _, line := range lines {
		if tuiLen(line) > 80 {
			t.Errorf("line %q is %d columns, want <= 80", line, tuiLen(line))
		}
	}
	if modal.selectedIdx != len(modal.entries)-1 {
		t.Errorf("initial selection = %d, want the last entry %d", modal.selectedIdx, len(modal.entries)-1)
	}
}

// Test the empty selector (TS L1950-1959).
func TestInspectSelectorEmpty(t *testing.T) {
	r, _ := modalRuntime(t)
	modal := r.newInspectSelectorModal()
	joined := joinLines(modal.Render(60))
	if !strings.Contains(joined, "(no tool calls or thinking entries)") {
		t.Errorf("empty selector = %s", joined)
	}
	if modal.selectedIdx != 0 || modal.scrollTop != 0 {
		t.Errorf("empty selection = %d / scroll %d", modal.selectedIdx, modal.scrollTop)
	}
}

// Test the selector's key handling (TS L1856-1931).
func TestInspectSelectorKeys(t *testing.T) {
	r, tui := selectorRuntime(t)
	modal := r.newInspectSelectorModal()
	_ = modal.Render(80)

	before := modal.selectedIdx
	modal.handleInput("k")
	if modal.selectedIdx != before-1 {
		t.Errorf("k: %d -> %d", before, modal.selectedIdx)
	}
	modal.handleInput("j")
	if modal.selectedIdx != before {
		t.Errorf("j did not restore the selection: %d", modal.selectedIdx)
	}
	modal.handleInput("\x1b[H")
	if modal.selectedIdx != 0 {
		t.Errorf("Home selection = %d", modal.selectedIdx)
	}
	modal.handleInput("\x1b[F")
	if modal.selectedIdx != len(modal.entries)-1 {
		t.Errorf("End selection = %d", modal.selectedIdx)
	}
	modal.handleInput("\x1b[5~")
	if modal.selectedIdx != maxInt(0, len(modal.entries)-1-modal.visibleHgt) {
		t.Errorf("PageUp selection = %d", modal.selectedIdx)
	}
	modal.handleInput("\x1b[6~")
	if modal.selectedIdx != len(modal.entries)-1 {
		t.Errorf("PageDown selection = %d", modal.selectedIdx)
	}
	if tui.renderCount() == 0 {
		t.Error("navigation should request a render")
	}

	// An unknown key is not consumed.
	if modal.handleInput("z") {
		t.Error("an unknown key should not be consumed")
	}

	// Enter opens the detail modal for the selection.
	modal.handleInput("\r")
	r.mu.Lock()
	detail := r.activeModalComponent
	r.mu.Unlock()
	if detail == nil {
		t.Error("Enter did not open the detail modal")
	}
	r.closeDetailModal()

	// Esc closes the selector.
	r.mu.Lock()
	r.activeSelectorComp = modal
	r.mu.Unlock()
	modal.handleInput("\x1b")
	r.mu.Lock()
	open := r.activeSelectorComp
	r.mu.Unlock()
	if open != nil {
		t.Error("Esc did not close the selector")
	}
}

// Test refreshEntries keeps or trails the selection (TS L1824-1841).
func TestInspectSelectorRefresh(t *testing.T) {
	r, _ := selectorRuntime(t)
	modal := r.newInspectSelectorModal()
	// A trailing selection follows a new entry.
	modal.selectedIdx = len(modal.entries) - 1
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("new", "read", nil).succeeded("ok")}}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.mu.Unlock()
	modal.refreshEntries(false)
	if modal.selectedIdx != len(modal.entries)-1 {
		t.Errorf("a trailing selection = %d, want %d", modal.selectedIdx, len(modal.entries)-1)
	}

	// A mid-list selection stays where it is.
	modal.selectedIdx = 1
	modal.refreshEntries(false)
	if modal.selectedIdx != 1 {
		t.Errorf("a mid-list selection moved to %d", modal.selectedIdx)
	}

	// An emptied list resets.
	r.mu.Lock()
	r.groups = map[*toolGroupComponent]bool{}
	r.mu.Unlock()
	modal.refreshEntries(false)
	if modal.selectedIdx != 0 || modal.scrollTop != 0 || len(modal.entries) != 0 {
		t.Errorf("empty refresh = idx %d scroll %d entries %d", modal.selectedIdx, modal.scrollTop, len(modal.entries))
	}
}

// Test the selector lifecycle through the fake TUI (TS L2071-2122).
func TestInspectSelectorLifecycle(t *testing.T) {
	r, tui := selectorRuntime(t)
	r.openInspectSelectorModal()
	r.mu.Lock()
	comp := r.activeSelectorComp
	handle := r.activeSelectorHandle
	r.mu.Unlock()
	if comp == nil || handle == nil {
		t.Fatalf("selector not opened (comp=%v handle=%v)", comp != nil, handle != nil)
	}
	tui.mu.Lock()
	if len(tui.overlays) != 1 || tui.overlays[0].maxHeight != 70 {
		t.Errorf("overlay = %+v", tui.overlays)
	}
	tui.mu.Unlock()

	r.closeInspectSelectorModal()
	r.mu.Lock()
	comp = r.activeSelectorComp
	handle = r.activeSelectorHandle
	r.mu.Unlock()
	if comp != nil || handle != nil {
		t.Error("closeInspectSelectorModal left state behind")
	}
	// A second close is safe.
	r.closeInspectSelectorModal()
}

// Test opening the selector closes an open detail modal (TS L2072-2075).
func TestInspectSelectorClosesDetail(t *testing.T) {
	r, _ := selectorRuntime(t)
	r.openDetailModal(groupEntry{kind: "tool", tool: newTestTool("a", "read", nil)})
	r.openInspectSelectorModal()
	r.mu.Lock()
	detail := r.activeModalComponent
	r.mu.Unlock()
	if detail != nil {
		t.Error("opening the selector should close the detail modal")
	}
}

// Test the SDK adapter separates "the key was consumed" from "the overlay
// closed" (task 必修 2). RemoteComponentResult{Done: true} ends the Custom
// component, so a navigation key must report Done == false or the first arrow
// press would dismiss the modal in production.
func TestCustomAdapterInputNeverClosesOnNavigation(t *testing.T) {
	r, _ := selectorRuntime(t)
	tool := newTestTool("t", "read", map[string]any{"path": "/p"})
	long := ""
	for i := 0; i < 60; i++ {
		long += "output line " + itoa(i) + "\n"
	}
	tool.succeeded(long)

	// Detail modal: navigation scrolls, Esc/q close.
	detail := r.newDetailModal(groupEntry{kind: "tool", tool: tool})
	_ = detail.Render(80)
	adapter := &sdkComponentAdapter{built: detail}
	atBottom := detail.scrollTop
	if atBottom == 0 {
		t.Fatal("a long output should have scrolled to the end")
	}
	for _, key := range []string{"\x1b[A", "\x1b[B", "k", "j", "\x1b[5~", "\x1b[6~", "\x1b[H", "\x1b[F", "z"} {
		result, err := adapter.HandleInput(key)
		if err != nil {
			t.Fatalf("HandleInput(%q): %v", key, err)
		}
		if result.Done {
			t.Errorf("HandleInput(%q) closed the overlay; navigation must report Done == false", key)
		}
	}
	if detail.scrollTop != atBottom {
		t.Errorf("navigation did not round-trip the scroll: %d, want %d", detail.scrollTop, atBottom)
	}
	r.mu.Lock()
	r.activeModalComponent = detail
	r.mu.Unlock()
	for _, key := range []string{"\x1b", "q", "Q"} {
		result, err := adapter.HandleInput(key)
		if err != nil {
			t.Fatalf("HandleInput(%q): %v", key, err)
		}
		if !result.Done {
			t.Errorf("HandleInput(%q) did not close the overlay; a close key must report Done == true", key)
		}
		r.openDetailModal(groupEntry{kind: "tool", tool: tool})
		r.mu.Lock()
		r.activeModalComponent = detail
		r.mu.Unlock()
	}

	// Selector modal: navigation moves the selection, Esc/q close.
	selector := r.newInspectSelectorModal()
	_ = selector.Render(80)
	selectorAdapter := &sdkComponentAdapter{built: selector}
	before := selector.selectedIdx
	for _, key := range []string{"\x1b[A", "\x1b[B", "k", "j", "\x1b[5~", "\x1b[6~", "\x1b[H", "\x1b[F", "z"} {
		result, err := selectorAdapter.HandleInput(key)
		if err != nil {
			t.Fatalf("HandleInput(%q): %v", key, err)
		}
		if result.Done {
			t.Errorf("selector HandleInput(%q) closed the overlay; navigation must report Done == false", key)
		}
	}
	if selector.selectedIdx != before {
		t.Errorf("selector selection drifted: %d, want %d", selector.selectedIdx, before)
	}
	r.mu.Lock()
	r.activeSelectorComp = selector
	r.mu.Unlock()
	result, err := selectorAdapter.HandleInput("\x1b")
	if err != nil {
		t.Fatalf("HandleInput(Esc): %v", err)
	}
	if !result.Done {
		t.Error("Esc did not close the selector overlay")
	}
}

// Test the shared ctx.ui.Custom root: Enter switches the root's view to the
// detail instead of stacking a second Custom session, and the detail's Esc/q/Q
// falls back to the selector rather than ending the root (task 必修 2).
func TestCustomRootSwitchesViewsInsteadOfStacking(t *testing.T) {
	r, ui := selectorRuntimeWithCustomUI(t)
	r.openInspectSelectorModal()
	ui.waitCustomCalls(t, 1)
	if ui.customCalls != 1 {
		t.Fatalf("custom calls = %d, want 1 (a single root)", ui.customCalls)
	}
	r.mu.Lock()
	root := r.activeCustomRoot
	r.mu.Unlock()
	if root == nil {
		t.Fatal("the ui.Custom path did not open a shared root")
	}

	// Enter on the selector switches the view; no second Custom session. The
	// keystroke is driven through the root exactly as the host delivers it, so
	// the selector's Enter branch (not a direct show()) is what opens the detail.
	selector := root.selector
	if selector == nil {
		t.Fatal("the root carries no selector view")
	}
	if selector.selectedIdx < 0 || selector.selectedIdx >= len(selector.entries) {
		t.Fatalf("selector selection = %d of %d entries", selector.selectedIdx, len(selector.entries))
	}
	wantEntry := selector.entries[selector.selectedIdx]
	done, _ := root.HandleInput("\r")
	if done {
		t.Error("Enter on the selector must not end the root")
	}
	if ui.customCalls != 1 {
		t.Errorf("custom calls = %d, want 1 (Enter must not stack a second session)", ui.customCalls)
	}
	detail, ok := root.current().(*detailModalComponent)
	if !ok {
		t.Fatalf("root view = %T, want the detail modal after Enter", root.current())
	}
	if detail.entry.kind != wantEntry.kind {
		t.Errorf("detail entry kind = %q, want the selected %q", detail.entry.kind, wantEntry.kind)
	}
	if wantEntry.kind == "tool" && detail.entry.tool != wantEntry.tool {
		t.Error("Enter opened a detail for a different entry than the selection")
	}

	// The detail's Esc falls back to the selector instead of ending the root.
	done, _ = root.HandleInput("\x1b")
	if done {
		t.Error("the detail's Esc must fall back to the selector, not end the root")
	}
	if view := root.current(); view != remoteComponent(root.selector) {
		t.Errorf("root view = %T, want the selector", view)
	}
	if ui.customCalls != 1 {
		t.Errorf("custom calls = %d, want 1 after the detail closed", ui.customCalls)
	}

	// The selector's own Esc ends the root.
	done, _ = root.HandleInput("\x1b")
	if !done {
		t.Error("the selector's Esc must end the root")
	}
	if ui.customCalls != 1 {
		t.Errorf("custom calls = %d, want 1 for the whole overlay session", ui.customCalls)
	}
}

// ── round-2 review fixes ────────────────────────────────────────────────────

// Test the detail modal's poll loop reads the live entry under the runtime lock
// (task 必修 3). isEntryActive used to read think.active / tool.owner /
// toolStatus(tool) while holding only the modal lock, which is a data race with
// the event handlers that finalize the run and apply a tool result. The race
// detector is the assertion: a real live entry's detail modal is opened (which
// arms the poll loop) and repainted while events write the same fields.
func TestDetailModalPollingIsRaceFree(t *testing.T) {
	r, _, tui := startedRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	group := r.newToolGroup(0)
	think := &thinkingEntry{kind: "thinking", text: "streaming", tokens: 40, active: true}
	tool := newTestTool("live", "read", map[string]any{"path": "/p"}).pending()
	r.mu.Lock()
	group.entries = []groupEntry{
		{kind: "thinking", think: think},
		{kind: "tool", tool: tool},
	}
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	// A real, still-live entry: opening its detail modal arms the poll loop.
	r.openDetailModal(groupEntry{kind: "thinking", think: think})
	r.mu.Lock()
	modal := r.activeModalComponent
	r.mu.Unlock()
	if modal == nil {
		t.Fatal("the detail modal did not open")
	}
	modal.mu.Lock()
	polling := modal.polling
	modal.mu.Unlock()
	if !polling {
		t.Fatal("a live thinking entry did not start the poll loop")
	}
	// The live tool entry gets the same treatment, so the tool branch of
	// isEntryActive is exercised too.
	toolModal := r.newDetailModal(groupEntry{kind: "tool", tool: tool})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// The poll path: the armed loop plus the repaint a host frame triggers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = modal.isEntryActive()
			_ = modal.Render(80)
			_ = toolModal.isEntryActive()
			_ = toolModal.Render(80)
			// A freshly started run is the live entry the next poll would re-check.
			probe := &detailModalComponent{tui: r, entry: groupEntry{kind: "thinking", think: r.activeThinkingEntry()}}
			_ = probe.isEntryActive()
		}
	}()
	// Event producers: the writes the host's event goroutines perform.
	wg.Add(1)
	go func() {
		defer wg.Done()
		toolBlock := map[string]any{"type": "toolCall", "id": "live", "name": "read", "arguments": map[string]any{"path": "/p"}}
		message := map[string]any{"role": "assistant", "content": []any{toolBlock}}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.createThinkingEntry()
			r.finalizeActiveThinking()
			r.handleToolExecutionStart(map[string]any{"toolCallId": "live", "toolName": "read", "args": map[string]any{"path": "/p"}})
			r.handleMessageUpdate(map[string]any{
				"message":               message,
				"assistantMessageEvent": map[string]any{"type": "toolcall_delta", "contentIndex": float64(1)},
			})
			r.handleToolExecutionEnd(map[string]any{
				"toolCallId": "live",
				"result":     map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}},
			})
		}
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	r.closeDetailModal()
	toolModal.dispose()
	_ = tui
}

// activeThinkingEntry returns the run the poll loop would re-check next. It
// takes mu, like every other reader of the live bookkeeping.
func (r *runtimeState) activeThinkingEntry() *thinkingEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeThinking
}

// ── settings ───────────────────────────────────────────────────────────────

// Test makeStepper's render and key handling (TS L2196-2234).
func TestMakeStepper(t *testing.T) {
	meta := configKey{id: configField, label: "Max", min: 1, max: 30, step: 1}
	var doneWith *string
	stepper := makeStepper("Max collapsed entries", 5, meta, nil, func(v *string) { doneWith = v })

	lines := stepper.Render(80)
	if len(lines) != 6 {
		t.Fatalf("stepper lines = %d, want 6", len(lines))
	}
	joined := joinLines(lines)
	if !strings.Contains(joined, "Max collapsed entries") || !strings.Contains(joined, "5") {
		t.Errorf("stepper = %s", joined)
	}
	if !strings.Contains(joined, "◀ ▶ / − +  adjust    Enter  save    Esc  cancel") {
		t.Errorf("stepper hint = %s", joined)
	}

	// A narrow width truncates the hint line (L2220).
	if narrow := stepper.Render(20); tuiLen(narrow[5]) > 20 {
		t.Errorf("narrow hint = %q (%d columns)", narrow[5], tuiLen(narrow[5]))
	}

	// Left/right adjust within the bounds.
	stepper.handleInput("\x1b[C")
	if stepper.value != 6 {
		t.Errorf("Right = %d, want 6", stepper.value)
	}
	stepper.handleInput("\x1b[D")
	if stepper.value != 5 {
		t.Errorf("Left = %d, want 5", stepper.value)
	}
	stepper.handleInput("+")
	if stepper.value != 6 {
		t.Errorf("+ = %d", stepper.value)
	}
	stepper.handleInput("-")
	if stepper.value != 5 {
		t.Errorf("- = %d", stepper.value)
	}

	// The bounds clamp.
	for i := 0; i < 40; i++ {
		stepper.handleInput("\x1b[C")
	}
	if stepper.value != meta.max {
		t.Errorf("upper clamp = %d, want %d", stepper.value, meta.max)
	}
	for i := 0; i < 40; i++ {
		stepper.handleInput("\x1b[D")
	}
	if stepper.value != meta.min {
		t.Errorf("lower clamp = %d, want %d", stepper.value, meta.min)
	}

	// Enter commits, Esc cancels.
	stepper.handleInput("\r")
	if doneWith == nil || *doneWith != "1" {
		t.Errorf("Enter committed %v", doneWith)
	}
	doneWith = nil
	stepper2 := makeStepper("Max", 5, meta, nil, func(v *string) { doneWith = v })
	stepper2.handleInput("\x1b")
	if doneWith != nil {
		t.Errorf("Esc committed %v", doneWith)
	}

	// A zero-width render is still 1 column.
	if lines := stepper.Render(0); len(lines) != 6 {
		t.Errorf("zero-width stepper lines = %d", len(lines))
	}

	// The cache is invalidated by input.
	stepper.invalidate()
	if stepper.hasCash {
		t.Error("invalidate left the cache in place")
	}
}

// Test the settings component applies and persists a change
// (TS L2249-2280).
func TestSettingsComponent(t *testing.T) {
	r, _ := modalRuntime(t)
	component := newSettingsComponent(r, 5, nil, r.configPath)

	// The list shows the current value.
	joined := joinLines(component.Render(60))
	if !strings.Contains(joined, "Max collapsed entries") || !strings.Contains(joined, "5") {
		t.Errorf("list = %s", joined)
	}

	// Enter opens the stepper for the selected item.
	component.HandleInput("\r")
	if component.stepper == nil {
		t.Fatal("Enter did not open the stepper")
	}
	// One more step, then confirm.
	component.HandleInput("\x1b[C")
	component.HandleInput("\r")
	if component.stepper != nil {
		t.Error("confirming should close the stepper")
	}
	if !component.changed {
		t.Error("the change was not recorded")
	}
	r.mu.Lock()
	applied := r.maxGroupEntries
	r.mu.Unlock()
	if applied != 6 {
		t.Errorf("maxGroupEntries = %d, want 6", applied)
	}
	// The config file was persisted.
	if got := loadConfig(r.configPath, readConfigFile); got.MaxGroupEntries != 6 {
		t.Errorf("persisted config = %+v, want 6", got)
	}

	// Esc closes the list and reports whether anything changed.
	done, value := component.HandleInput("\x1b")
	if !done {
		t.Error("Esc should close the list")
	}
	if changed, ok := value.(bool); !ok || !changed {
		t.Errorf("Esc result = %v", value)
	}
}

// Test the settings list navigation and cancel (TS L2249-2280).
func TestSettingsComponentNavigation(t *testing.T) {
	r, _ := modalRuntime(t)
	component := newSettingsComponent(r, 5, nil, r.configPath)

	// Up at the first item stays there; down at the last stays there.
	component.HandleInput("\x1b[A")
	if component.index != 0 {
		t.Errorf("Up index = %d", component.index)
	}
	component.HandleInput("\x1b[B")
	if component.index >= len(component.items) {
		t.Errorf("Down index = %d is out of range", component.index)
	}

	// Cancelling the stepper leaves the value alone.
	component.HandleInput("\r")
	component.HandleInput("\x1b")
	if component.stepper != nil {
		t.Error("Esc should close the stepper")
	}
	if component.changed {
		t.Error("a cancelled stepper should not mark the config changed")
	}
}

// Test the config command without a UI notifies the current value
// (TS L2239-2245).
func TestConfigCommandWithoutUI(t *testing.T) {
	r, _ := modalRuntime(t)
	r.maxGroupEntries = 7
	ui := newFakeUI()
	ui.ui = false
	r.mu.Lock()
	r.activeUIContext = ui
	r.mu.Unlock()

	r.handleConfigCommand(sdkZeroContext())
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if len(ui.notices) != 1 {
		t.Fatalf("notices = %d, want 1", len(ui.notices))
	}
	if ui.notices[0].message != "compact: maxGroupEntries=7" {
		t.Errorf("notice = %q", ui.notices[0].message)
	}
	if ui.notices[0].level != "info" {
		t.Errorf("level = %q", ui.notices[0].level)
	}
}

// Test the config command with a UI opens the component and notifies on
// save (TS L2247-2285).
func TestConfigCommandWithUI(t *testing.T) {
	r, _ := modalRuntime(t)
	ui := newFakeUI()
	ui.customFn = func(factory remoteComponentFactory, _ any) (any, error) {
		component := factory(nil)
		settings, ok := component.(*settingsComponent)
		if !ok {
			t.Fatalf("custom component = %T", component)
		}
		// Drive one change through the component.
		settings.HandleInput("\r")
		settings.HandleInput("\x1b[C")
		settings.HandleInput("\r")
		settings.HandleInput("\x1b")
		return settings.changed, nil
	}
	r.mu.Lock()
	r.activeUIContext = ui
	r.mu.Unlock()

	r.handleConfigCommand(sdkZeroContext())
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.customCalls != 1 {
		t.Errorf("custom calls = %d", ui.customCalls)
	}
	if len(ui.notices) != 1 || ui.notices[0].message != "compact-ui settings saved" {
		t.Errorf("notices = %+v", ui.notices)
	}
}

// Test the command handler is safe with a zero context and no UI context
// (the SDK's zero Context must not panic).
func TestConfigCommandZeroContextSafe(t *testing.T) {
	r := testRuntime(t)
	// No uiContext captured and a zero sdk.Context: the handler must not panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("handleConfigCommand panicked: %v", rec)
		}
	}()
	r.handleConfigCommand(sdkZeroContext())
}
