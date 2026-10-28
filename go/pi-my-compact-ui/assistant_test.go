package compactui

import (
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file covers src/assistant-patches.ts (L193-521) and
// src/compaction.ts (L1512-1562).

// ── assistant patches ──────────────────────────────────────────────────────

// Test applyAssistantThinkingPatch strips thinking blocks (TS L268-333).
func TestApplyAssistantThinkingPatch(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	container := assistant.contentContainer

	message := &assistantMessage{role: "assistant", content: []contentItem{
		thinkingItem("reasoning"),
		textItem("answer"),
	}}
	out := r.applyAssistantThinkingPatch(container, message, false)
	if len(out) != 1 || out[0].typ != "text" {
		t.Fatalf("filtered content = %+v", out)
	}

	// A message without thinking is passed through untouched.
	plain := &assistantMessage{content: []contentItem{textItem("only text")}}
	if out := r.applyAssistantThinkingPatch(container, plain, false); len(out) != 1 {
		t.Errorf("plain content = %+v", out)
	}

	// A nil message yields nothing.
	if out := r.applyAssistantThinkingPatch(container, nil, false); out != nil {
		t.Errorf("nil message = %+v", out)
	}
}

// Test applyAssistantThinkingPatch only stages history for a dead message
// (TS L286-288).
func TestApplyAssistantThinkingPatchLiveVsHistorical(t *testing.T) {
	// A live (streaming) message does not stage anchors.
	liveRuntime, _ := newGroupRuntime(t)
	liveAssistant := liveRuntime.newTestAssistant()
	liveMessage := &assistantMessage{content: []contentItem{
		thinkingItem("live thought"),
		textItem("streaming answer"),
	}}
	liveRuntime.applyAssistantThinkingPatch(liveAssistant.contentContainer, liveMessage, true)
	liveRuntime.mu.Lock()
	liveState := liveRuntime.assistantContentStates[liveAssistant.contentContainer]
	liveRuntime.mu.Unlock()
	if len(liveState.anchors) != 0 {
		t.Errorf("a streaming message staged %d anchors", len(liveState.anchors))
	}

	// A finished message stages them.
	deadRuntime, _ := newGroupRuntime(t)
	deadAssistant := deadRuntime.newTestAssistant()
	deadRuntime.applyAssistantThinkingPatch(deadAssistant.contentContainer, liveMessage, false)
	deadRuntime.mu.Lock()
	deadState := deadRuntime.assistantContentStates[deadAssistant.contentContainer]
	deadRuntime.mu.Unlock()
	if len(deadState.anchors) != 1 {
		t.Errorf("a finished message staged %d anchors, want 1", len(deadState.anchors))
	}
}

// Test applyAssistantThinkingPatch mounts the anchored group of a
// thinking-only message (TS L319-331).
func TestApplyAssistantThinkingPatchMountsThinkingOnly(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	container := assistant.contentContainer

	message := &assistantMessage{content: []contentItem{thinkingItem("only thought")}}
	out := r.applyAssistantThinkingPatch(container, message, false)
	if len(out) != 0 {
		t.Fatalf("a thinking-only message should render nothing: %+v", out)
	}
	// The staged group is mounted with a spacer on each side.
	if len(container.children) != 3 {
		t.Fatalf("children = %d, want 3 (spacer, group, spacer): %+v", len(container.children), container.children)
	}
	if !isSpacer(container.children[0]) || !isToolGroup(container.children[1]) || !isSpacer(container.children[2]) {
		t.Errorf("children shape = %+v", container.children)
	}
	r.mu.Lock()
	mount := container.children[1].(*toolGroupComponent).mount
	r.mu.Unlock()
	if mount != container {
		t.Error("the group mount was not recorded")
	}

	// A second pass does not add another copy.
	r.applyAssistantThinkingPatch(container, message, false)
	if len(container.children) != 3 {
		t.Errorf("second pass added children: %d", len(container.children))
	}
}

// Test ensureAssistantThinkingPatched normalises the flags (TS L335-344).
func TestEnsureAssistantThinkingPatched(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := &assistantMessageComponent{tui: r, hideThinking: false, hiddenThinking: "Thinking..."}
	already := r.ensureAssistantThinkingPatched(assistant)
	if !already {
		t.Error("ensureAssistantThinkingPatched reports the patch is installed")
	}
	if !assistant.hideThinking || assistant.hiddenThinking != "" {
		t.Errorf("flags = hide=%v label=%q", assistant.hideThinking, assistant.hiddenThinking)
	}
	if r.ensureAssistantThinkingPatched(nil) {
		t.Error("a nil component reports nothing patched")
	}
}

// Test installCompactionSummaryRendering and the header render
// (TS L1518-1562). The plain theme is production's: it does not inflate the
// measured width.
func TestCompactionHeaderRendering(t *testing.T) {
	r := testRuntime(t)

	// The wide form fits and keeps the exact token count with separators.
	header := r.installCompactionSummaryRendering(12_345.0)
	r.mu.Lock()
	installed := r.compactionStyleInstalled
	r.mu.Unlock()
	if !installed {
		t.Error("the compaction style was not recorded as installed")
	}
	lines := header.Render(80)
	if len(lines) != 1 {
		t.Fatalf("wide header lines = %v", lines)
	}
	for _, want := range []string{"›‹", "Context compacted", "12,345 tokens → summary"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("wide header = %q, missing %q", lines[0], want)
		}
	}

	// A narrow width falls back to the compact form.
	narrow := header.Render(30)
	if len(narrow) != 1 {
		t.Fatalf("narrow header lines = %v", narrow)
	}
	for _, want := range []string{"Compacted", "12.3K tok"} {
		if !strings.Contains(narrow[0], want) {
			t.Errorf("narrow header = %q, missing %q", narrow[0], want)
		}
	}
	if tuiLen(narrow[0]) > 30 {
		t.Errorf("narrow header is %d columns, want <= 30", tuiLen(narrow[0]))
	}
}

// Test the compaction token clamp (TS L1549-1551): a non-positive or
// non-numeric count renders as zero.
func TestCompactionTokenClamp(t *testing.T) {
	r, _ := newGroupRuntime(t)
	cases := []struct {
		in   any
		want int
	}{
		{nil, 0},
		{0.0, 0},
		{-5.0, 0},
		{"nope", 0},
		{1234.0, 1234},
	}
	for _, tc := range cases {
		header := r.installCompactionSummaryRendering(tc.in)
		if header.tokensBefore != tc.want {
			t.Errorf("tokensBefore(%v) = %d, want %d", tc.in, header.tokensBefore, tc.want)
		}
	}
}

// Test compactionSummaryRender applies GROUP_PADDING_X (TS L1546-1554).
func TestCompactionSummaryRender(t *testing.T) {
	r := testRuntime(t)
	lines := r.compactionSummaryRender(999.0, 60)
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, " ") {
			t.Errorf("line %q lacks the padding", line)
		}
		if tuiLen(line) > 60 {
			t.Errorf("line %q is %d columns, want <= 60", line, tuiLen(line))
		}
	}
	if !strings.Contains(lines[0], "999 tokens") {
		t.Errorf("summary = %q", lines[0])
	}
}

// Test sdkTheme degrades to unstyled text for an unknown colour token. The host
// palette panics on an unknown colour when it holds a palette; the adapter
// swallows it so a theme refresh cannot take down a render. bold/inverse carry
// no colour token, so the zero palette still styles them.
func TestSDKThemeUnknownTokenFallsBack(t *testing.T) {
	th := sdkTheme{}
	if got := th.fg("muted", "plain"); got != "plain" {
		t.Errorf("zero palette fg = %q, want the text unchanged", got)
	}
	if got := th.bg("accent", "x"); got != "x" {
		t.Errorf("zero palette bg = %q", got)
	}
	// Modifiers are colour-free: the SDK emits their ANSI around the text.
	if got := th.bold("bold"); !strings.Contains(got, "bold") {
		t.Errorf("zero palette bold = %q, want the text wrapped", got)
	}
	if got := th.inverse("x"); !strings.Contains(got, "x") {
		t.Errorf("zero palette inverse = %q", got)
	}
}

// Test fgOf / boldOf leave text alone without a theme.
func TestThemeHelpersWithoutTheme(t *testing.T) {
	if got := fgOf(nil, "dim", "text"); got != "text" {
		t.Errorf("fgOf(nil) = %q", got)
	}
	if got := boldOf(nil, "text"); got != "text" {
		t.Errorf("boldOf(nil) = %q", got)
	}
	th := &recordingTheme{}
	if got := fgOf(th, "dim", "text"); got != "[dim:text]" {
		t.Errorf("fgOf = %q", got)
	}
	if got := boldOf(th, "text"); got != "«text»" {
		t.Errorf("boldOf = %q", got)
	}
}

// ── native thinking suppression (brief2 R4 B) ───────────────────────────────

// TestMarkdownTransformerSuppressesNativeThinking is the regression for brief2
// R4 B. R3 concluded that the host's native thinking block could not be
// suppressed; the host code says otherwise: the transformers a session registers
// are installed for the assistant markdown *and* for the assistant-thinking
// markdown (internal/codingagent/interactive_chat.go:71-72, message types
// "assistant" / "assistant-thinking"), and tui/markdown.go:297-318 returns
// zero rendered lines when the transformed text's JSTrim is empty. A transformer
// that answers "" for the thinking type therefore removes the native thinking
// body exactly as the TS prototype patch does, whatever hideThinkingBlock says.
//
// The registration itself cannot be read back (sdk.Extension keeps its
// markdownTransform field private and exposes no getter), so the wiring is
// pinned at compile time by transformMarkdownFunc and this test covers the
// behaviour of the function that is handed to the host.
func TestMarkdownTransformerSuppressesNativeThinking(t *testing.T) {
	r := testRuntime(t)
	cases := []struct {
		name     string
		message  string
		markdown string
		want     string
	}{
		{
			name:     "native thinking body is dropped",
			message:  markdownMessageTypeAssistantThinking,
			markdown: "Let me read the file.",
			want:     "",
		},
		{
			name:     "native thinking keeps no fence either",
			message:  markdownMessageTypeAssistantThinking,
			markdown: "```mermaid\ngraph TD\n  A-->B\n```",
			want:     "",
		},
		{
			name:     "assistant text is returned unchanged",
			message:  "assistant",
			markdown: "# Title\n\nbody **bold** with `code`",
			want:     "# Title\n\nbody **bold** with `code`",
		},
		{
			name:     "user text is returned unchanged",
			message:  "user",
			markdown: "please read /tmp/a.txt",
			want:     "please read /tmp/a.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.transformMarkdown(tc.markdown, sdk.MarkdownTransformContext{
				MessageType:    tc.message,
				IsStreaming:    true,
				AvailableWidth: 80,
			})
			if got != tc.want {
				t.Errorf("transformMarkdown(%q, %q) = %q, want %q", tc.message, tc.markdown, got, tc.want)
			}
		})
	}
}

// TestMarkdownTransformerLeavesTreeThinkingRow is the "do the two paths interfere?"
// check brief2 R4 B asks for explicitly: suppressing the host's native thinking
// must not take the group's own thinking row with it.
//
// They are independent by construction, and this test pins both halves:
//   - the in-tree row is drawn by the extension itself (thinkingRow returns one
//     plain string, painted through the ToolRenderer by toolRenderLines), so it
//     never passes through a host Markdown component and never reaches
//     transformMarkdown;
//   - the same text handed to the host as "assistant-thinking" markdown renders
//     zero lines, which is what removes the native block.
func TestMarkdownTransformerLeavesTreeThinkingRow(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	group := r.newToolGroup(0)
	tool := newTestTool("call-a", "read", map[string]any{"path": "/a"}).succeeded("a-out")
	group.addTool(tool)
	const thought = "Let me read the file."
	r.mu.Lock()
	// owner stays nil: addThinkingLocked assigns the group and refuses an entry
	// that already belongs to one.
	group.addThinkingLocked(&thinkingEntry{
		kind:        "thinking",
		id:          1,
		text:        thought,
		tokens:      6,
		tokensExact: true,
	})
	group.sealed = true
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	tree := r.toolRenderLines("read", tool.args, sdk.ToolRenderContext{ToolCallID: "call-a"}, nil, false, 80, false)
	if !containsLine(tree, "thinking") || !containsLine(tree, thought) {
		t.Fatalf("card lines = %v, want the group's own thinking row inside the tree", tree)
	}
	if !strings.Contains(tree[0], "done") {
		t.Errorf("tree header = %q, want the sealed group's terminal state", tree[0])
	}

	// The very same text, on the host's native path, renders nothing.
	if got := r.transformMarkdown(thought, sdk.MarkdownTransformContext{
		MessageType:    markdownMessageTypeAssistantThinking,
		AvailableWidth: 80,
	}); got != "" {
		t.Errorf("native thinking transform = %q, want an empty result (zero rendered rows)", got)
	}
}
