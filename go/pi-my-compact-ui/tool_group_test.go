package compactui

import (
	"strings"
	"testing"
)

// This file covers src/tool-group.ts (L808-1071): the group model queries and
// the four row renderers plus renderCollapsed / render.

// newGroupRuntime builds a runtime with a recording theme pinned at clock 0.
func newGroupRuntime(t *testing.T) (*runtimeState, *recordingTheme) {
	t.Helper()
	r := testRuntime(t)
	th := &recordingTheme{}
	r.withTheme(th)
	return r, th
}

// Test visibleEntries (TS L875-887): tools and notices always, thinking runs
// only while active or non-empty.
func TestVisibleEntries(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)

	active := &thinkingEntry{kind: "thinking", active: true, text: "", owner: group}
	empty := &thinkingEntry{kind: "thinking", active: false, text: "   "}
	withText := &thinkingEntry{kind: "thinking", active: false, text: "hello"}
	tool := newTestTool("t1", "read", nil)
	notice := &noticeEntry{kind: "notice", text: "cache miss: 1 tokens re-billed"}

	group.entries = []groupEntry{
		{kind: "thinking", think: active},
		{kind: "thinking", think: empty},
		{kind: "thinking", think: withText},
		{kind: "tool", tool: tool},
		{kind: "notice", notice: notice},
	}

	visible := group.visibleEntries()
	if len(visible) != 4 {
		t.Fatalf("visible entries = %d, want 4 (empty inactive thinking is hidden)", len(visible))
	}
	if visible[0].think != active || visible[1].think != withText || visible[2].tool != tool || visible[3].notice != notice {
		t.Error("visible entries lost their stream order")
	}
	if !group.hasVisibleEntries() {
		t.Error("hasVisibleEntries")
	}
	if !group.hasThinking() {
		t.Error("hasThinking")
	}
	if !group.hasTools() {
		t.Error("hasTools")
	}
	if !group.hasActiveThinking() {
		t.Error("hasActiveThinking")
	}

	// A group whose only entries are empty inactive thinking runs has nothing
	// to show.
	onlyEmpty := r.newToolGroup(0)
	onlyEmpty.entries = []groupEntry{{kind: "thinking", think: empty}}
	if onlyEmpty.hasVisibleEntries() {
		t.Error("a group of empty thinking should not be visible")
	}
	if onlyEmpty.hasThinking() || onlyEmpty.hasTools() || onlyEmpty.hasActiveThinking() {
		t.Error("empty group claims entries it does not show")
	}
}

// Test hasPending (TS L891-893).
func TestHasPending(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	pending := newTestTool("p", "read", nil).pending()
	group.entries = []groupEntry{{kind: "tool", tool: pending}}
	if !group.hasPending() {
		t.Error("a started result-less tool is pending")
	}
	// A result-less tool stays pending even when a result object exists while
	// isPartial is set: toolStatus checks isPartial before the result.
	finished := newTestTool("p2", "read", nil).pending()
	finished.result = doneResult("ok")
	if !group.hasPending() {
		t.Error("isPartial keeps the tool pending")
	}
	finished.isPartial = false
	group.entries = []groupEntry{{kind: "tool", tool: finished}}
	group.children = []*toolComponent{finished}
	if group.hasPending() {
		t.Error("a finished tool is not pending")
	}
}

// Test addTool (TS L838-853): the tool is absorbed, restarted, and owned.
func TestAddTool(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	tool := newTestTool("call-1", "read", map[string]any{"path": "/tmp/x"})

	group.addTool(tool)

	if len(group.children) != 1 || group.children[0] != tool {
		t.Fatalf("children = %d", len(group.children))
	}
	if len(group.entries) != 1 || group.entries[0].tool != tool {
		t.Fatalf("entries = %+v", group.entries)
	}
	if tool.owner != group {
		t.Error("tool owner not set")
	}
	if tool.groupedAt == nil {
		t.Error("_groupedAt not stamped")
	}
	// The tool's own render is hidden once it belongs to a group.
	if lines := tool.Render(80); lines != nil {
		t.Errorf("grouped tool renders %v, want no lines", lines)
	}
	// A result-less tool records its start.
	r.mu.Lock()
	_, started := r.toolStarts["call-1"]
	r.mu.Unlock()
	if !started {
		t.Error("tool start not remembered")
	}
}

// Test addTool marks a known-interrupted call (TS L844-846).
func TestAddToolInterrupted(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.mu.Lock()
	r.interruptedToolCallID["dead"] = true
	r.mu.Unlock()
	group := r.newToolGroup(0)
	tool := newTestTool("dead", "bash", nil)
	group.addTool(tool)
	if !tool.interrupted {
		t.Error("the interrupted id should mark the tool")
	}
	if got := toolStatus(tool, group); got != "error" {
		t.Errorf("status = %q, want error", got)
	}
}

// Test addThinking is idempotent (TS L854-859).
func TestAddThinkingIdempotent(t *testing.T) {
	r, _ := newGroupRuntime(t)
	first := r.newToolGroup(0)
	second := r.newToolGroup(0)
	entry := &thinkingEntry{kind: "thinking", text: "reasoning"}

	first.addThinking(entry)
	second.addThinking(entry)

	if len(first.entries) != 1 {
		t.Fatalf("first group entries = %d, want 1", len(first.entries))
	}
	if len(second.entries) != 0 {
		t.Errorf("second group took a run it does not own: %d entries", len(second.entries))
	}
	if entry.owner != first {
		t.Error("owner changed by the second add")
	}
}

// Test addNotice (TS L860-865): a notice already owned elsewhere is not
// stolen, and adding invalidates the group.
func TestAddNotice(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	entry := &noticeEntry{kind: "notice", text: "cache miss: 1 tokens re-billed"}

	group.rendered = []string{"stale"}
	group.addNotice(entry)
	if len(group.entries) != 1 || entry.owner != group {
		t.Fatalf("notice not added: %+v", group.entries)
	}
	if group.rendered != nil {
		t.Error("addNotice should invalidate the cached render")
	}

	other := r.newToolGroup(0)
	other.addNotice(entry)
	if len(other.entries) != 0 {
		t.Error("a notice owned by another group should be ignored")
	}

	// A run owned by this same group is appended again: the guard only rejects
	// an owner that is a *different* group (TS L860-862).
	unowned := &noticeEntry{kind: "notice", text: "second"}
	group.addNotice(unowned)
	group.addNotice(unowned)
	if len(group.entries) != 3 {
		t.Errorf("entries = %d, want 3 (re-adding to the same group is not filtered)", len(group.entries))
	}
}

// Test removeTool (TS L866-874).
func TestRemoveTool(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	tool := newTestTool("x", "read", nil)
	group.addTool(tool)
	group.removeTool(tool)
	if len(group.children) != 0 || len(group.entries) != 0 {
		t.Errorf("removeTool left children=%d entries=%d", len(group.children), len(group.entries))
	}
	if tool.owner != nil {
		t.Error("owner not cleared")
	}
	// Removing an unknown tool is a no-op.
	group.removeTool(nil)
	group.removeTool(newTestTool("y", "read", nil))
}

// Test workPhaseLabel (TS L903-916): only the latest open group shows the
// phase, and never while it has pending work.
func TestWorkPhaseLabel(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	// No phase at all.
	if _, ok := group.workPhaseLabel(); ok {
		t.Error("no phase should yield no label")
	}

	for phase, want := range map[string]string{
		phaseWaitingResponse: "waiting for response...",
		phaseWaitingOutput:   "waiting for next output...",
		phaseWorking:         "working...",
	} {
		r.mu.Lock()
		r.agentWorkPhase = strPtr(phase)
		r.mu.Unlock()
		got, ok := group.workPhaseLabel()
		if !ok || got != want {
			t.Errorf("phase %q = (%q, %v), want %q", phase, got, ok, want)
		}
	}

	// An unknown phase is not labelled.
	r.mu.Lock()
	r.agentWorkPhase = strPtr("idle")
	r.mu.Unlock()
	if _, ok := group.workPhaseLabel(); ok {
		t.Error("unknown phase should yield no label")
	}

	// A sealed group never shows it.
	r.mu.Lock()
	r.agentWorkPhase = strPtr(phaseWorking)
	group.sealed = true
	r.mu.Unlock()
	if _, ok := group.workPhaseLabel(); ok {
		t.Error("sealed group should not show the phase")
	}
	group.sealed = false

	// Only the last active group shows it.
	if _, ok := group.workPhaseLabel(); !ok {
		t.Fatal("the last active group should show the phase")
	}
	r.mu.Lock()
	other := r.newToolGroupLocked(0)
	r.lastActiveGroup = other
	r.mu.Unlock()
	if _, ok := group.workPhaseLabel(); ok {
		t.Error("a non-active group should not show the phase")
	}

	// Pending work suppresses the phase.
	r.mu.Lock()
	r.lastActiveGroup = group
	r.agentWorkPhase = strPtr(phaseWorking)
	group.entries = append(group.entries, groupEntry{kind: "tool", tool: newTestTool("p", "read", nil).pending()})
	r.mu.Unlock()
	if _, ok := group.workPhaseLabel(); ok {
		t.Error("a group with pending tools should not show the phase")
	}
}

// Test needsAnimation (TS L917-920).
func TestNeedsAnimation(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	if group.needsAnimation() {
		t.Error("empty group should not animate")
	}

	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("p", "read", nil).pending()}}
	if !group.needsAnimation() {
		t.Error("a pending tool animates")
	}

	group.entries = []groupEntry{{kind: "thinking", think: &thinkingEntry{kind: "thinking", active: true}}}
	if !group.needsAnimation() {
		t.Error("an active thinking run animates")
	}

	// A finished tool in the last active group keeps animating through the
	// work phase label.
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.agentWorkPhase = strPtr(phaseWorking)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("done", "read", nil).succeeded("ok")}}
	r.mu.Unlock()
	if !group.needsAnimation() {
		t.Error("the work phase label keeps the group animating")
	}

	// Without tools, a non-nil phase animates too.
	other := r.newToolGroup(0)
	r.mu.Lock()
	r.lastActiveGroup = other
	r.mu.Unlock()
	thinkingOnly := r.newToolGroup(0)
	thinkingOnly.entries = []groupEntry{{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "done"}}}
	if !thinkingOnly.needsAnimation() {
		t.Error("a thinking-only open group animates while a phase is set")
	}

	// A sealed group never animates.
	r.mu.Lock()
	r.agentWorkPhase = nil
	r.mu.Unlock()
	thinkingOnly.sealed = true
	if thinkingOnly.needsAnimation() {
		t.Error("sealed groups do not animate")
	}
}

// Test iconFor / colorFor (TS L921-928).
func TestIconAndColorFor(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	cases := []struct {
		tool  *toolComponent
		icon  string
		color string
	}{
		{newTestTool("p", "read", nil).pending(), "F", "accent"},
		{newTestTool("e", "read", nil).failed(), "✗", "error"},
		{newTestTool("s", "read", nil).succeeded("ok"), "✓", "success"},
	}
	for _, tc := range cases {
		if got := group.iconFor(tc.tool, "F"); got != tc.icon {
			t.Errorf("iconFor(%q) = %q, want %q", tc.tool.toolCallID, got, tc.icon)
		}
		if got := group.colorFor(toolStatus(tc.tool, nil)); got != tc.color {
			t.Errorf("colorFor = %q, want %q", got, tc.color)
		}
	}
}

// Test toolRow / noticeRow / thinkingRow render the themed row shapes.
func TestRowRendering(t *testing.T) {
	r, th := newGroupRuntime(t)
	group := r.newToolGroup(0)

	tool := newTestTool("t", "bash", map[string]any{"command": "go test ./..."}).succeeded("ok")
	row := group.toolRow("│  ", tool, "⠋", 80, false)
	for _, want := range []string{"[dim:│  ]", "[success:✓]", "[toolTitle:«bash»]", "[dim:go test ./...]", "[muted:(0ms)]"} {
		if !strings.Contains(row, want) {
			t.Errorf("toolRow = %q, missing %q", row, want)
		}
	}
	if len(th.themeTokens()) == 0 {
		t.Error("no theme calls recorded")
	}

	// A dim row uses the dim colour for the icon and name.
	dimRow := group.toolRow("└  ", tool, "⠋", 80, true)
	if !strings.Contains(dimRow, "[dim:bash]") {
		t.Errorf("dim toolRow = %q, want a dim name", dimRow)
	}

	// A notice row strips the label and the colons.
	notice := &noticeEntry{kind: "notice", text: "cache miss: 1234 tokens re-billed"}
	noticeRow := group.noticeRow("│  ", notice, 80, false)
	if !strings.Contains(noticeRow, "[warning:!]") || !strings.Contains(noticeRow, "cache miss") {
		t.Errorf("noticeRow = %q", noticeRow)
	}
	if !strings.Contains(noticeRow, "[dim:1234 tokens re-billed]") {
		t.Errorf("noticeRow content = %q", noticeRow)
	}

	// A thinking row shows the preview and the token label.
	think := &thinkingEntry{kind: "thinking", text: "**considering** the `case`", tokens: 1234}
	thinkRow := group.thinkingRow("└  ", think, 200, false)
	for _, want := range []string{"[dim:└  ]", "[thinkingText:✓]", "[toolTitle:thinking]", "[muted:(1.2k)]", "considering the case"} {
		if !strings.Contains(thinkRow, want) {
			t.Errorf("thinkingRow = %q, missing %q", thinkRow, want)
		}
	}

	// An empty thinking run renders the label only.
	empty := &thinkingEntry{kind: "thinking", text: "", active: true}
	emptyRow := group.thinkingRow("", empty, 80, false)
	if !strings.Contains(emptyRow, "thinking...") {
		t.Errorf("empty thinkingRow = %q", emptyRow)
	}
}

// Test rowWidths: a row alone may overflow the requested width (the TS fallback
// keeps a single space between the header and the elapsed label, L934-935 and
// L998-999), but the rendered group always truncates to the content width
// (L1079).
func TestRowWidths(t *testing.T) {
	r := testRuntime(t)
	group := r.newToolGroup(0)
	tool := newTestTool("t", "bash", map[string]any{"command": "go test ./... --very-long-argument"}).succeeded("ok")

	// The row itself keeps the right-hand elapsed label and at least one space.
	wide := group.toolRow("│  ", tool, "⠋", 80, false)
	if !strings.HasSuffix(wide, "(0ms)") {
		t.Errorf("toolRow should keep the elapsed label: %q", wide)
	}
	if !strings.Contains(wide, "  (0ms)") {
		t.Errorf("toolRow should separate content from the label: %q", wide)
	}

	// The rendered group never exceeds the width it was asked for.
	group.entries = []groupEntry{
		{kind: "tool", tool: tool},
		{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "**considering** the `case`", tokens: 1234}},
	}
	for _, width := range []int{40, 20, 12, 5, 1} {
		for _, line := range group.Render(width) {
			if got := tuiLen(line); got > width {
				t.Errorf("Render(%d) produced a %d-column line: %q", width, got, line)
			}
		}
	}
}

// Test renderCollapsed single entry (TS L1006-1013): no header, no rail.
func TestRenderCollapsedSingleEntry(t *testing.T) {
	r, _ := newGroupRuntime(t)
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", map[string]any{"path": "/x"}).succeeded("ok")}}
	lines := group.renderCollapsed(60, group.visibleEntries())
	if len(lines) != 1 {
		t.Fatalf("single-entry render = %d lines, want 1: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "└") || strings.Contains(lines[0], "│") {
		t.Errorf("single entry should have no tree rail: %q", lines[0])
	}

	// The same rule applies to a single notice and a single thinking run.
	noticeGroup := r.newToolGroup(0)
	noticeGroup.entries = []groupEntry{{kind: "notice", notice: &noticeEntry{kind: "notice", text: "cache miss: 1 tokens re-billed"}}}
	if lines := noticeGroup.renderCollapsed(60, noticeGroup.visibleEntries()); len(lines) != 1 {
		t.Errorf("single notice = %v", lines)
	}
	thinkGroup := r.newToolGroup(0)
	thinkGroup.entries = []groupEntry{{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "why"}}}
	if lines := thinkGroup.renderCollapsed(60, thinkGroup.visibleEntries()); len(lines) != 1 {
		t.Errorf("single thinking = %v", lines)
	}

	// A work phase forces the header even with one entry.
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.agentWorkPhase = strPtr(phaseWorking)
	r.mu.Unlock()
	if lines := group.renderCollapsed(60, group.visibleEntries()); len(lines) < 2 {
		t.Errorf("a work phase should add the header: %v", lines)
	}
}

// Test renderCollapsed header state machine (TS L1015-1054).
func TestRenderCollapsedHeaderStates(t *testing.T) {
	pendingReceiving := func() *toolComponent {
		tool := newTestTool("a", "read", nil)
		tool.isPartial = true
		tool.argsComplete = false
		return tool
	}
	pendingStarted := func() *toolComponent { return newTestTool("b", "read", nil).pending() }
	done := func(id string) *toolComponent { return newTestTool(id, "read", nil).succeeded("ok") }

	cases := []struct {
		name    string
		entries []groupEntry
		want    string
	}{
		{
			name: "receiving tool args",
			entries: []groupEntry{
				{kind: "tool", tool: pendingReceiving()},
				{kind: "tool", tool: pendingReceiving()},
			},
			want: "tool call receiving...",
		},
		{
			name: "tool calling",
			entries: []groupEntry{
				{kind: "tool", tool: pendingStarted()},
				{kind: "tool", tool: pendingStarted()},
			},
			want: "tool calling...",
		},
		{
			name: "waiting for first token",
			entries: []groupEntry{
				{kind: "thinking", think: &thinkingEntry{kind: "thinking", active: true, text: ""}},
				{kind: "tool", tool: done("c")},
			},
			want: "waiting for first token...",
		},
		{
			name: "thinking while streaming tokens",
			entries: []groupEntry{
				{kind: "thinking", think: &thinkingEntry{kind: "thinking", active: true, text: "some reasoning"}},
				{kind: "tool", tool: done("d")},
			},
			want: "thinking...",
		},
		{
			name: "tools done",
			entries: []groupEntry{
				{kind: "tool", tool: done("e")},
				{kind: "tool", tool: done("f")},
			},
			want: "tools done",
		},
		{
			name: "done with thinking",
			entries: []groupEntry{
				{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "why"}},
				{kind: "tool", tool: done("g")},
			},
			want: "done",
		},
	}
	for _, tc := range cases {
		r, _ := newGroupRuntime(t)
		group := r.newToolGroup(0)
		group.entries = tc.entries
		lines := group.renderCollapsed(80, group.visibleEntries())
		// The header only exists for a multi-entry group (a single entry renders
		// the bare row, TS L1008-1013).
		if len(lines) < 2 {
			t.Fatalf("%s: expected a header plus entry rows, got %v", tc.name, lines)
		}
		if !strings.Contains(lines[0], tc.want) {
			t.Errorf("%s: header = %q, want %q", tc.name, lines[0], tc.want)
		}
	}
}

// Test renderCollapsed hidden count and rails (TS L1055-1071).
func TestRenderCollapsedFolding(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.maxGroupEntries = 3
	group := r.newToolGroup(0)
	group.sealed = true
	for i := 0; i < 5; i++ {
		group.entries = append(group.entries, groupEntry{kind: "tool", tool: newTestTool("id"+itoa(i), "read", nil).succeeded("ok")})
	}
	lines := group.renderCollapsed(80, group.visibleEntries())
	// header + 3 rendered entries.
	if len(lines) != 4 {
		t.Fatalf("folded render = %d lines, want 4: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "(2 hidden)") {
		t.Errorf("header = %q, want a hidden count", lines[0])
	}
	// The row right after the header is the dim, │-railed first visible entry;
	// the last row carries the └ rail (TS L1055-1071).
	if !strings.Contains(lines[1], "[dim:✓]") || !strings.Contains(lines[1], "│  ") {
		t.Errorf("first visible entry should be dim and │-railed: %q", lines[1])
	}
	if !strings.Contains(lines[2], "│  ") {
		t.Errorf("middle entry should use the │ rail: %q", lines[2])
	}
	if !strings.Contains(lines[3], "└  ") {
		t.Errorf("last entry should use the └ rail: %q", lines[3])
	}
}

// Test render (TS L1072-1081): padding, truncation and the leading blank line.
// The plain theme is what production renders with, so these width assertions
// measure real terminal columns.
func TestGroupRenderWidth(t *testing.T) {
	r := testRuntime(t)
	group := r.newToolGroup(0)
	group.entries = []groupEntry{
		{kind: "tool", tool: newTestTool("a", "read", map[string]any{"path": "/x"}).succeeded("ok")},
		{kind: "tool", tool: newTestTool("b", "read", map[string]any{"path": "/y"}).succeeded("ok")},
	}

	lines := group.Render(40)
	if len(lines) == 0 || lines[0] != "" {
		t.Fatalf("a non-anchored group starts with a blank line: %v", lines)
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, " ") {
			t.Errorf("line %q lacks the GROUP_PADDING_X indent", line)
		}
		if got := tuiLen(line); got > 40 {
			t.Errorf("line %q is %d columns, want <= 40", line, got)
		}
	}

	// An anchored group drops the leading blank line.
	group.anchored = true
	anchored := group.Render(40)
	if len(anchored) != len(lines)-1 {
		t.Fatalf("anchored render = %d lines, want %d", len(anchored), len(lines)-1)
	}

	// An empty group renders nothing.
	empty := r.newToolGroup(0)
	if lines := empty.Render(40); lines != nil {
		t.Errorf("empty group renders %v", lines)
	}

	// Width 1 keeps the padding at zero and the content width at 1.
	if lines := group.Render(1); len(lines) == 0 {
		t.Error("width 1 should still render")
	}
}

// Test renderWidth schedules animation for a live group (TS L1075-1077).
func TestGroupRenderSchedulesAnimation(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.setClock(func() int64 { return 0 })
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("p", "read", nil).pending()}}
	_ = group.Render(40)
	r.mu.Lock()
	timer := r.animTimer
	r.mu.Unlock()
	if timer == nil {
		t.Fatal("a live group should schedule an animation tick")
	}
	r.stopAnimation()
}

// Test base36 group ids keep the TS shape (L812).
func TestGroupIDShape(t *testing.T) {
	r, _ := newGroupRuntime(t)
	first := r.newToolGroup(1234)
	second := r.newToolGroup(1234)
	if !strings.HasPrefix(first.toolCallID, "compact-group-1234-") {
		t.Errorf("group id = %q", first.toolCallID)
	}
	if len(strings.TrimPrefix(first.toolCallID, "compact-group-1234-")) != 5 {
		t.Errorf("group id suffix = %q, want 5 characters", first.toolCallID)
	}
	if first.toolCallID == second.toolCallID {
		t.Error("group ids must be unique")
	}
	if first.toolName != "group" {
		t.Errorf("group toolName = %q, want group", first.toolName)
	}
}
