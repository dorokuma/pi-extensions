package compactui

import (
	"strings"
	"sync"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file covers src/thinking.ts (L1112-1165), src/streaming-tools.ts
// (L522-754) and src/grouping.ts (L1166-1511).

// ── thinking.ts ────────────────────────────────────────────────────────────

// Test createThinkingEntry / finalizeActiveThinking (TS L1127-1157).
// Test an indexed thinking event after an un-indexed run starts a new run.
// TS L2369 is JS `runtime.activeThinkingIndex !== contentIndex` on a nullable
// field, so the comparison also holds when no index was recorded yet -- a
// literal Go translation dereferences a nil *int (必修 6).
func TestIndexedThinkingDeltaAfterUnindexedRun(t *testing.T) {
	r, ui, _ := startedRuntime(t)
	_ = ui
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	content := []any{map[string]any{"type": "thinking", "thinking": "indexed run"}}

	// An un-indexed run: no contentIndex at all.
	r.handleMessageUpdate(map[string]any{
		"message":               map[string]any{"role": "assistant", "content": content},
		"assistantMessageEvent": map[string]any{"type": "thinking_delta"},
	})
	r.mu.Lock()
	first := r.activeThinking
	r.mu.Unlock()
	if first == nil {
		t.Fatal("the un-indexed run never started")
	}

	// An indexed event must not fold into it: it starts and finalizes a new run.
	r.handleMessageUpdate(map[string]any{
		"message":               map[string]any{"role": "assistant", "content": content},
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": float64(0)},
	})
	r.mu.Lock()
	second := r.activeThinking
	index := r.activeThinkingIndex
	r.mu.Unlock()
	if second == first {
		t.Error("the indexed event folded into the un-indexed run")
	}
	if second == nil || index == nil || *index != 0 {
		t.Fatalf("no new run at index 0 (run=%v idx=%v)", second != nil, index)
	}
	if first.active {
		t.Error("the superseded run stayed active")
	}
	if second.text != "indexed run" {
		t.Errorf("new run text = %q, want the block's own thinking text", second.text)
	}
}

// Test message_start finalizes an unfinished thinking run *before* the turn
// reset clears activeThinking (TS L2312-2316, 必修 5). Finalizing after the
// reset leaves the previous run's `active` flag stuck at true, so it renders as
// "thinking..." forever and the group never reports its work as done.
func TestMessageStartFinalizesThinkingBeforeTurnReset(t *testing.T) {
	r, ui, _ := startedRuntime(t)
	_ = ui
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{}},
		"assistantMessageEvent": map[string]any{
			"type": "thinking_start", "contentIndex": float64(0),
		},
	})
	r.handleMessageUpdate(map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "thinking", "thinking": "still working on it"}},
		},
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": float64(0)},
	})

	r.mu.Lock()
	entry := r.activeThinking
	group := r.lastActiveGroup
	r.mu.Unlock()
	if entry == nil || group == nil {
		t.Fatal("the thinking run never started")
	}
	if !entry.active {
		t.Fatal("the run should be active before message_start")
	}

	// A new user turn arrives while the run is still open.
	r.handleMessageStart(map[string]any{
		"message": map[string]any{"role": "user", "content": "next question"},
	})

	r.mu.Lock()
	stillActive := entry.active
	r.mu.Unlock()
	if stillActive {
		t.Error("the leftover run stayed active after message_start")
	}
	if joined := joinLines(group.Render(80)); strings.Contains(joined, "thinking...") {
		t.Errorf("sealed group render = %q, want no live thinking spinner", joined)
	}
}

func TestCreateAndFinalizeThinking(t *testing.T) {
	r, _ := newGroupRuntime(t)

	entry := r.createThinkingEntry()
	if !entry.active || entry.text != "" || entry.tokens != 0 || entry.owner != nil {
		t.Errorf("new entry = %+v", entry)
	}
	r.mu.Lock()
	active := r.activeThinking
	unattached := len(r.unattachedThinking)
	r.mu.Unlock()
	if active != entry || unattached != 1 {
		t.Fatalf("active=%v unattached=%d", active == entry, unattached)
	}

	// finalize seals it and remembers it as the last sealed run.
	r.mu.Lock()
	r.activeThinkingIndex = intPtrValue(2)
	r.mu.Unlock()
	r.finalizeActiveThinking()
	if entry.active {
		t.Error("finalize should clear active")
	}
	r.mu.Lock()
	last := r.lastSealedThinkingEntry
	sealed := r.sealedThinkingIndexes[2]
	active2 := r.activeThinking
	idx := r.activeThinkingIndex
	r.mu.Unlock()
	if last != entry || !sealed || active2 != nil || idx != nil {
		t.Errorf("after finalize: last=%v sealed=%v active=%v idx=%v", last == entry, sealed, active2 != nil, idx != nil)
	}

	// Finalizing with nothing active is a no-op.
	r.finalizeActiveThinking()
}

// Test updateActiveThinkingTokens (TS L1113-1126): the reported reasoning
// count wins over the 4-chars-per-token estimate.
func TestUpdateActiveThinkingTokens(t *testing.T) {
	r, _ := newGroupRuntime(t)

	// No active run: nothing happens.
	r.updateActiveThinkingTokens(&assistantMessage{})

	entry := r.createThinkingEntry()
	r.setThinkingBlock(0, "first block")
	r.setThinkingBlock(1, "  ")
	r.setThinkingBlock(2, "second block")

	r.updateActiveThinkingTokens(&assistantMessage{})
	if entry.text != "first block\n\nsecond block" {
		t.Errorf("joined text = %q", entry.text)
	}
	if entry.tokens != estimateTextTokens(entry.text) || entry.tokensExact {
		t.Errorf("estimate = %d exact=%v", entry.tokens, entry.tokensExact)
	}

	// An indexed block is replaced in place and keeps its position.
	r.setThinkingBlock(0, "replaced first")
	r.updateActiveThinkingTokens(&assistantMessage{})
	if entry.text != "replaced first\n\nsecond block" {
		t.Errorf("after replacement = %q", entry.text)
	}

	// A reported reasoning count is exact.
	reported := 4321
	r.updateActiveThinkingTokens(&assistantMessage{usage: &usage{reasoning: &reported}})
	if entry.tokens != 4321 || !entry.tokensExact {
		t.Errorf("reported = %d exact=%v", entry.tokens, entry.tokensExact)
	}
	r.mu.Lock()
	lastReported := r.lastReportedReasoning
	r.mu.Unlock()
	if lastReported != 4321 {
		t.Errorf("lastReportedReasoning = %d", lastReported)
	}

	// A zero report falls back to the estimate.
	zero := 0
	r.updateActiveThinkingTokens(&assistantMessage{usage: &usage{reasoning: &zero}})
	if entry.tokensExact {
		t.Error("a zero report should not be exact")
	}
}

// Test refreshSealedThinkingTokens (TS L1158-1165).
func TestRefreshSealedThinkingTokens(t *testing.T) {
	r, th := newGroupRuntime(t)
	group := r.newToolGroup(0)
	r.withTheme(th)

	entry := &thinkingEntry{kind: "thinking", text: "reasoning", tokens: 3, owner: group}
	r.mu.Lock()
	r.lastSealedThinkingEntry = entry
	r.mu.Unlock()

	reported := 900
	r.refreshSealedThinkingTokens(&reported)
	if entry.tokens != 900 || !entry.tokensExact {
		t.Errorf("refreshed = %d exact=%v", entry.tokens, entry.tokensExact)
	}

	// An exact run is left alone.
	entry.tokens = 1
	r.refreshSealedThinkingTokens(&reported)
	if entry.tokens != 1 {
		t.Errorf("an exact run should not be overwritten: %d", entry.tokens)
	}

	// A run without text, or without a positive report, is left alone.
	empty := &thinkingEntry{kind: "thinking", text: "  "}
	r.mu.Lock()
	r.lastSealedThinkingEntry = empty
	r.mu.Unlock()
	r.refreshSealedThinkingTokens(&reported)
	if empty.tokensExact {
		t.Error("an empty run should not take the report")
	}

	r.mu.Lock()
	r.lastSealedThinkingEntry = nil
	r.mu.Unlock()
	r.refreshSealedThinkingTokens(nil)
}

// Test thinkingAlreadyShown (TS L197-211).
func TestThinkingAlreadyShown(t *testing.T) {
	r, _ := newGroupRuntime(t)

	if !r.thinkingAlreadyShown("   ") {
		t.Error("blank text is already shown")
	}
	if r.thinkingAlreadyShown("fresh") {
		t.Error("fresh text is not shown")
	}

	entry := r.createThinkingEntry()
	entry.text = "live run  "
	if !r.thinkingAlreadyShown("live run") {
		t.Error("the active run should match")
	}
	r.finalizeActiveThinking()

	// An unattached run matches too.
	parked := &thinkingEntry{kind: "thinking", text: " parked "}
	r.mu.Lock()
	r.unattachedThinking = append(r.unattachedThinking, parked)
	r.mu.Unlock()
	if !r.thinkingAlreadyShown("parked") {
		t.Error("an unattached run should match")
	}

	// A grouped run matches.
	group := r.newToolGroup(0)
	group.entries = append(group.entries, groupEntry{kind: "thinking", think: &thinkingEntry{kind: "thinking", text: "grouped"}})
	r.mu.Lock()
	r.addGroupLocked(group)
	r.mu.Unlock()
	if !r.thinkingAlreadyShown("grouped") {
		t.Error("a grouped run should match")
	}
}

// ── streaming-tools.ts ─────────────────────────────────────────────────────

// Test toolElapsed (TS L523-556).
func TestToolElapsed(t *testing.T) {
	r, _ := newGroupRuntime(t)

	// A known duration wins.
	r.mu.Lock()
	r.toolDurations["known"] = 1.5
	r.mu.Unlock()
	tool := newTestTool("known", "read", nil)
	if got := toolElapsed(r, tool); got != "1.50s" {
		t.Errorf("known duration = %q", got)
	}
	if tool.elapsedDur == nil || *tool.elapsedDur != 1.5 {
		t.Errorf("_elapsedDuration = %v", tool.elapsedDur)
	}

	// A group end stamp measures from the recorded start.
	r.setClock(func() int64 { return 10_000 })
	r.mu.Lock()
	r.toolStarts["ended"] = 8_000
	r.mu.Unlock()
	ended := newTestTool("ended", "read", nil)
	end := int64(9_500)
	ended.groupEndAt = &end
	if got := toolElapsed(r, ended); got != "1.50s" {
		t.Errorf("group end = %q", got)
	}

	// Without a start, the group end is the start.
	orphan := newTestTool("orphan", "read", nil)
	orphan.groupEndAt = &end
	if got := toolElapsed(r, orphan); got != "0ms" {
		t.Errorf("orphan = %q", got)
	}

	// A finished, non-partial tool with a recorded duration reuses it.
	stored := newTestTool("stored", "read", nil)
	dur := 2.25
	stored.elapsedDur = &dur
	if got := toolElapsed(r, stored); got != "2.25s" {
		t.Errorf("stored duration = %q", got)
	}

	// A result timestamp measures against the recorded start.
	resulting := newTestTool("resulting", "read", nil)
	resulting.result = &toolResult{timestamp: 4_000}
	r.mu.Lock()
	r.toolStarts["resulting"] = 1_000
	r.mu.Unlock()
	if got := toolElapsed(r, resulting); got != "3.00s" {
		t.Errorf("result timestamp = %q", got)
	}

	// A result without a usable timestamp reports zero.
	noStart := newTestTool("no-start", "read", nil)
	noStart.result = &toolResult{timestamp: 4_000}
	if got := toolElapsed(r, noStart); got != "0ms" {
		t.Errorf("result without start = %q", got)
	}

	// An interrupted or sealed tool reports zero.
	interrupted := newTestTool("interrupted", "read", nil)
	interrupted.interrupted = true
	if got := toolElapsed(r, interrupted); got != "0ms" {
		t.Errorf("interrupted = %q", got)
	}
	r.mu.Lock()
	r.interruptedToolCallID["flagged"] = true
	r.mu.Unlock()
	flagged := newTestTool("flagged", "read", nil)
	if got := toolElapsed(r, flagged); got != "0ms" {
		t.Errorf("flagged id = %q", got)
	}
	sealedGroup := r.newToolGroup(0)
	sealedGroup.sealed = true
	inSealed := newTestTool("in-sealed", "read", nil)
	inSealed.owner = sealedGroup
	if got := toolElapsed(r, inSealed); got != "0ms" {
		t.Errorf("sealed group = %q", got)
	}

	// A live tool measures against the clock.
	r.setClock(func() int64 { return 20_000 })
	r.mu.Lock()
	r.toolStarts["live"] = 18_000
	r.mu.Unlock()
	live := newTestTool("live", "read", nil)
	if got := toolElapsed(r, live); got != "2.00s" {
		t.Errorf("live = %q", got)
	}

	// No start and no result reports zero, and a nil tool is guarded.
	unknown := newTestTool("unknown", "read", nil)
	if got := toolElapsed(r, unknown); got != "0ms" {
		t.Errorf("unknown = %q", got)
	}
	if got := toolElapsed(r, nil); got != "0ms" {
		t.Errorf("nil tool = %q", got)
	}
}

// Test rememberToolStart (TS L560-563).
func TestRememberToolStart(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.setClock(func() int64 { return 777 })

	r.rememberToolStart("", 777)
	r.mu.Lock()
	_, present := r.toolStarts[""]
	r.mu.Unlock()
	if present {
		t.Error("an empty id must not be recorded")
	}

	r.rememberToolStart("a", 777)
	r.mu.Lock()
	start := r.toolStarts["a"]
	r.mu.Unlock()
	if start != 777 {
		t.Errorf("start = %d", start)
	}
	// A second call does not overwrite the first.
	r.rememberToolStart("a", 999)
	r.mu.Lock()
	start = r.toolStarts["a"]
	r.mu.Unlock()
	if start != 777 {
		t.Errorf("start was overwritten: %d", start)
	}
	// A tool with a known duration is not restarted.
	r.mu.Lock()
	r.toolDurations["b"] = 1
	r.mu.Unlock()
	r.rememberToolStart("b", 5)
	r.mu.Lock()
	_, started := r.toolStarts["b"]
	r.mu.Unlock()
	if started {
		t.Error("a timed tool should not gain a start")
	}
}

// Test loadSessionToolTimings (TS L564-597).
func TestLoadSessionToolTimings(t *testing.T) {
	r, _ := newGroupRuntime(t)
	entries := []map[string]any{
		{
			"type": "message",
			"message": map[string]any{
				"role":      "assistant",
				"timestamp": 1_000.0,
				"content": []any{
					map[string]any{"type": "toolCall", "id": "done"},
					map[string]any{"type": "toolCall", "id": "cut"},
					map[string]any{"type": "text", "text": "hi"},
				},
			},
		},
		{
			"type": "message",
			"message": map[string]any{
				"role":       "toolResult",
				"toolCallId": "done",
				"timestamp":  3_000.0,
			},
		},
		{
			// An entry timestamp supplies the start when the message has none.
			"type":      "message",
			"timestamp": "1970-01-01T00:00:05Z",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "toolCall", "id": "entry-ts"}},
			},
		},
		{"type": "custom", "message": map[string]any{"role": "user"}},
		{"type": "message"},
		{
			"type": "message",
			"message": map[string]any{
				"role":    "assistant",
				"content": "not a list",
			},
		},
	}
	r.loadSessionToolTimings(entries)

	r.mu.Lock()
	doneDuration, doneTimed := r.toolDurations["done"]
	cutDuration, cutTimed := r.toolDurations["cut"]
	cutInterrupted := r.interruptedToolCallID["cut"]
	_, doneInterrupted := r.interruptedToolCallID["done"]
	entryStart, entryRecorded := r.toolStarts["entry-ts"]
	r.mu.Unlock()

	if !doneTimed || doneDuration != 2.0 {
		t.Errorf("finished call duration = %v (timed=%v)", doneDuration, doneTimed)
	}
	if doneInterrupted {
		t.Error("a call with a result is not interrupted")
	}
	if !cutTimed || cutDuration != 0 {
		t.Errorf("unfinished call should be timed 0: %v (timed=%v)", cutDuration, cutTimed)
	}
	if !cutInterrupted {
		t.Error("a call without a result is interrupted")
	}
	if !entryRecorded || entryStart != 5_000 {
		t.Errorf("entry timestamp start = %d (recorded=%v)", entryStart, entryRecorded)
	}

	// Nil entries are a no-op.
	r.loadSessionToolTimings(nil)
}

// Test noteStreamingToolCall creates, updates and hands over a provisional
// entry (TS L720-753).
func TestNoteStreamingToolCall(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	// A toolcall_start creates a provisional entry inside a fresh group.
	content := []contentBlock{{typ: "text", block: streamBlock{text: "hello"}}}
	r.noteStreamingToolCall(content, streamEvent{
		typ:          "toolcall_start",
		contentIndex: intPtrValue(1),
		toolCall:     &streamBlock{id: "call-1", name: "read", typ: "toolCall", partialJSON: "/tmp/x"},
	})

	r.mu.Lock()
	preparing := r.preparingByIndex[1]
	group := r.lastActiveGroup
	groupCount := len(r.groups)
	r.mu.Unlock()
	if preparing == nil {
		t.Fatal("no provisional entry was created")
	}
	if preparing.toolCallID != "call-1" || preparing.toolName != "read" {
		t.Errorf("provisional entry = %+v", preparing)
	}
	if preparing.args["path"] != "/tmp/x" {
		t.Errorf("provisional args = %v", preparing.args)
	}
	if !preparing.receivingArgs {
		t.Error("a non-terminal toolcall event is still receiving arguments")
	}
	if group == nil || groupCount != 1 {
		t.Fatalf("no group was opened: %v", group)
	}
	if len(group.entries) != 1 || group.entries[0].preparing != preparing {
		t.Errorf("group entries = %+v", group.entries)
	}

	// A toolcall_end updates the same entry.
	r.noteStreamingToolCall(content, streamEvent{
		typ:          "toolcall_end",
		contentIndex: intPtrValue(1),
		toolCall:     &streamBlock{id: "call-1", name: "read", typ: "toolCall", arguments: map[string]any{"path": "/tmp/y"}},
	})
	r.mu.Lock()
	updated := r.preparingByIndex[1]
	r.mu.Unlock()
	if updated.args["path"] != "/tmp/y" {
		t.Errorf("arguments were not updated: %v", updated.args)
	}
	if updated.receivingArgs {
		t.Error("toolcall_end finishes the argument stream")
	}

	// An event that locates no call is ignored.
	r.noteStreamingToolCall(content, streamEvent{typ: "toolcall_delta"})
}

// Test noteStreamingToolCall keeps a call's identity when the wire reuses a
// content index (brief2). The model can rewrite a block in place, so the
// stream carries a brand-new call in a slot an already-executed call owned.
// The call id, once present, must identify the call on its own: folding by
// the shared index overwrites the finished call's arguments and leaves the
// new call without a group entry, so its host card paints a bare row outside
// the compact tree for the whole streaming window.
func TestNoteStreamingToolCallIDWinsOverReusedIndex(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	// A finished call that streamed at content index 0.
	first := newTestTool("call-a", "read", map[string]any{"path": "/a"})
	first.contentIndex = 0
	first.hasIndex = true
	first.succeeded("a-out")
	group := r.newToolGroup(0)
	group.addTool(first)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	// The block at index 0 now carries a different call.
	content := []contentBlock{{typ: "toolCall", block: streamBlock{id: "call-b", name: "ctx_kb", typ: "toolCall", arguments: map[string]any{"query": "q"}}}}
	r.noteStreamingToolCall(content, streamEvent{typ: "toolcall_end", contentIndex: intPtrValue(0)})

	r.mu.Lock()
	preparing := r.preparingByIndex[0]
	firstArgs := first.args["path"]
	groupEntries := len(group.entries)
	r.mu.Unlock()
	if preparing == nil {
		t.Fatal("no provisional entry was created for the new call")
	}
	if preparing.toolCallID != "call-b" {
		t.Errorf("provisional entry id = %q, want call-b", preparing.toolCallID)
	}
	if firstArgs != "/a" {
		t.Errorf("the finished call's arguments were overwritten: %v", firstArgs)
	}
	if groupEntries != 2 {
		t.Errorf("group entries = %d, want the finished call plus the new one", groupEntries)
	}
}

// Test noteStreamingToolCall updates an already-grouped tool (TS L532-541).
func TestNoteStreamingToolCallGrouped(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	tool := newTestTool("grouped", "read", nil)
	group := r.newToolGroup(0)
	group.addTool(tool)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	// The content block is what streams; the ported path reads its arguments
	// (locateStreamingToolCall returns content[index] when the index points at
	// a toolCall block, as L148-151 does).
	content := []contentBlock{{typ: "toolCall", block: streamBlock{id: "grouped", typ: "toolCall", partialJSON: "next"}}}
	r.noteStreamingToolCall(content, streamEvent{
		typ:          "toolcall_delta",
		contentIndex: intPtrValue(0),
		toolCall:     &streamBlock{id: "grouped", typ: "toolCall", partialJSON: "next"},
	})

	if tool.contentIndex != 0 || !tool.hasIndex {
		t.Errorf("index = %d (set=%v)", tool.contentIndex, tool.hasIndex)
	}
	if tool.args["query"] != "next" {
		t.Errorf("args = %v", tool.args)
	}
	r.mu.Lock()
	_, noPreparing := r.preparingByIndex[0]
	r.mu.Unlock()
	if noPreparing {
		t.Error("an existing grouped tool must not gain a provisional entry")
	}
}

// Test attachPreparingTool / detachPreparingTool / absorbPreparingTool
// (TS L669-715).
func TestPreparingToolLifecycle(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	preparing := &preparingTool{toolCallID: "pending-1", toolName: "read", args: map[string]any{"path": "/p"}}
	r.attachPreparingTool(preparing)
	r.mu.Lock()
	owner := preparing.owner
	r.mu.Unlock()
	if owner == nil {
		t.Fatal("attachPreparingTool did not open a group")
	}

	// Attaching again is a no-op while the entry has an owner.
	before := len(owner.entries)
	r.attachPreparingTool(preparing)
	if len(owner.entries) != before {
		t.Error("attachPreparingTool added a duplicate entry")
	}

	// The real tool absorbs the provisional entry: the id, index and args move
	// over and the provisional entry leaves the group.
	r.mu.Lock()
	r.toolStarts["pending-1"] = 500
	r.preparingByIndex[0] = preparing
	r.mu.Unlock()

	real := newTestTool("real-1", "read", nil)
	r.absorbPreparingTool(real)
	if real.contentIndex != 0 || !real.hasIndex {
		t.Errorf("index not absorbed: %d", real.contentIndex)
	}
	if real.args["path"] != "/p" {
		t.Errorf("args not absorbed: %v", real.args)
	}
	r.mu.Lock()
	absorbedStart := r.toolStarts["real-1"]
	preparingOwner := preparing.owner
	_, still := r.preparingByIndex[0]
	r.mu.Unlock()
	if absorbedStart != 500 {
		t.Errorf("start not transferred: %d", absorbedStart)
	}
	if preparingOwner != nil || still {
		t.Errorf("provisional entry survived: owner=%v still=%v", preparingOwner != nil, still)
	}
	entries := 0
	for _, entry := range owner.entries {
		if entry.preparing == preparing {
			entries++
		}
	}
	if entries != 0 {
		t.Error("detach did not remove the provisional entry")
	}

	// absorbPreparingTool with nothing to absorb is a no-op, and nil is safe.
	r.absorbPreparingTool(newTestTool("no-match", "read", nil))
	r.absorbPreparingTool(nil)
}

// Test a streaming tool call renders and opens in both modals before the real
// tool_execution_start arrives, without dereferencing a nil entry.tool (task
// 必修 3). The provisional entry is a real *toolComponent, so the group render,
// the selector and the detail modal all read the same shape.
func TestPreparingToolRendersAndOpensWithoutPanic(t *testing.T) {
	r, ui, _ := startedRuntime(t)
	_ = ui
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	toolBlock := map[string]any{
		"type":      "toolCall",
		"id":        "call-1",
		"name":      "read",
		"arguments": map[string]any{"path": "/p"},
	}
	secondBlock := map[string]any{
		"type":      "toolCall",
		"id":        "call-2",
		"name":      "bash",
		"arguments": map[string]any{"command": "ls"},
	}
	message := map[string]any{
		"role":    "assistant",
		"content": []any{toolBlock, secondBlock},
	}
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_start", "contentIndex": float64(0)},
	})
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_start", "contentIndex": float64(1)},
	})
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_delta", "contentIndex": float64(0)},
	})
	r.handleMessageUpdate(map[string]any{
		"message":               message,
		"assistantMessageEvent": map[string]any{"type": "toolcall_delta", "contentIndex": float64(1)},
	})

	// The provisional entry is mounted and the group can render it.
	r.mu.Lock()
	group := r.lastActiveGroup
	r.mu.Unlock()
	if group == nil {
		t.Fatal("the streamed tool call did not open a group")
	}
	if !group.hasPending() {
		t.Error("the streamed tool call should still be pending")
	}
	lines := group.Render(80)
	joined := joinLines(lines)
	if !strings.Contains(joined, "tool call receiving...") {
		t.Errorf("group render = %q, want the receiving hint", joined)
	}
	if !strings.Contains(joined, "read") || !strings.Contains(joined, "bash") {
		t.Errorf("group render = %q, want both tool names", joined)
	}

	// Both modals render and open without panicking while the call is still
	// provisional: this is where a nil entry.tool used to crash.
	selector := r.newInspectSelectorModal()
	// The provisional call is still live, so the selector armed its poll loop;
	// release it instead of leaking the goroutine past the test.
	defer selector.dispose()
	if len(selector.entries) == 0 {
		t.Fatal("the selector has no entries for the streamed tool call")
	}
	_ = selector.Render(80)
	selector.handleInput("\r")
	r.mu.Lock()
	detail := r.activeModalComponent
	r.mu.Unlock()
	if detail == nil {
		t.Fatal("Enter did not open the detail modal")
	}
	detailLines := detail.Render(80)
	detailJoined := joinLines(detailLines)
	if !strings.Contains(detailJoined, "running, waiting for output...") {
		t.Errorf("detail render = %q, want the pending hint", detailJoined)
	}
	r.closeDetailModal()

	// The real execution takes over the provisional entry smoothly.
	r.handleToolExecutionStart(map[string]any{
		"toolCallId": "call-1",
		"toolName":   "read",
		"args":       map[string]any{"path": "/p"},
	})
	r.mu.Lock()
	_, callOnePreparing := r.preparingByIndex[0]
	r.mu.Unlock()
	if callOnePreparing {
		t.Error("the provisional entry for call-1 survived the real tool execution")
	}
	r.handleToolExecutionEnd(map[string]any{
		"toolCallId": "call-1",
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "127.0.0.1 localhost"}},
		},
	})
	r.mu.Lock()
	group = r.lastActiveGroup
	entries := append([]groupEntry(nil), group.entries...)
	r.mu.Unlock()
	for _, entry := range entries {
		if entry.kind != "tool" || entry.tool == nil || entry.tool.toolCallID != "call-1" {
			continue
		}
		if toolStatus(entry.tool, group) != "success" {
			t.Errorf("call-1 status = %q, want success after tool_execution_end", toolStatus(entry.tool, group))
		}
		if entry.preparing != nil {
			t.Error("call-1 kept its provisional link after the real execution")
		}
	}
	finalJoined := joinLines(group.Render(80))
	if !strings.Contains(finalJoined, "read") {
		t.Errorf("final group render = %q, want the finished tool row", finalJoined)
	}
	if strings.Contains(finalJoined, "tool calling...") {
		t.Errorf("final group render = %q, want no waiting hint", finalJoined)
	}
}

// Test takePreparingTool by id and by unique name (TS L686-703).
func TestTakePreparingTool(t *testing.T) {
	r, _ := newGroupRuntime(t)

	first := &preparingTool{toolCallID: "id-1", toolName: "read"}
	second := &preparingTool{toolCallID: "id-2", toolName: "write"}
	third := &preparingTool{toolCallID: "id-3", toolName: "grep"}
	lonely := &preparingTool{toolCallID: "id-4", toolName: "bash"}
	r.mu.Lock()
	r.preparingByIndex[0] = first
	r.preparingByIndex[1] = second
	r.preparingByIndex[2] = third
	r.preparingByIndex[3] = lonely
	r.mu.Unlock()

	// By id.
	if got := r.takePreparingTool("id-1", ""); got != first {
		t.Errorf("take by id = %v", got)
	}
	// By a unique name.
	if got := r.takePreparingTool("", "bash"); got != lonely {
		t.Errorf("take by name = %v", got)
	}
	// A name shared by no entry.
	if got := r.takePreparingTool("", "missing"); got != nil {
		t.Errorf("take missing = %v", got)
	}
	// A name shared by two entries is ambiguous, so nothing is taken.
	twin := &preparingTool{toolCallID: "id-5", toolName: "read"}
	sibling := &preparingTool{toolCallID: "id-6", toolName: "read"}
	r.mu.Lock()
	r.preparingByIndex[4] = twin
	r.preparingByIndex[5] = sibling
	r.mu.Unlock()
	if got := r.takePreparingTool("", "read"); got != nil {
		t.Errorf("ambiguous name should take nothing, got %v", got)
	}
}

// Test clearPreparingTools (TS L716-719).
func TestClearPreparingTools(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	chat.children = []component{assistant}

	one := &preparingTool{toolCallID: "a", toolName: "read"}
	two := &preparingTool{toolCallID: "b", toolName: "write"}
	r.mu.Lock()
	r.preparingByIndex[0] = one
	r.preparingByIndex[1] = two
	r.mu.Unlock()
	r.attachPreparingTool(one)
	r.attachPreparingTool(two)

	r.clearPreparingTools()
	r.mu.Lock()
	remaining := len(r.preparingByIndex)
	r.mu.Unlock()
	if remaining != 0 {
		t.Errorf("preparingByIndex = %d entries, want 0", remaining)
	}
	if one.owner != nil || two.owner != nil {
		t.Error("cleared entries kept an owner")
	}
}

// ── grouping.ts ────────────────────────────────────────────────────────────

// Test previousGroupable (TS L1167-1178).
func TestPreviousGroupable(t *testing.T) {
	r, _ := newGroupRuntime(t)
	sealed := r.newToolGroup(0)
	sealed.sealed = true
	open := r.newToolGroup(0)
	tool := newTestTool("t", "read", nil)
	assistant := r.newTestAssistant()

	children := []component{
		tool,
		newSpacer(1),
		assistant,
		sealed,
		open,
	}
	// Starting at the sealed group, the walk skips it, the assistant message
	// and the spacer, and stops at the tool execution (index 0). Children after
	// `start` are not visited.
	found, index, ok := previousGroupable(children, 3)
	if !ok || found != tool || index != 0 {
		t.Errorf("found %T at %d (ok=%v), want the tool at 0", found, index, ok)
	}

	// From the open group the walk returns it directly.
	found, index, ok = previousGroupable(children, 4)
	if !ok || found != open || index != 4 {
		t.Errorf("open: found %T at %d (ok=%v), want the open group at 4", found, index, ok)
	}

	// A sealed group is never returned, even as the only candidate.
	if _, _, ok := previousGroupable([]component{sealed}, 0); ok {
		t.Error("a sealed group is not groupable")
	}

	// A tool execution is groupable too.
	found, index, ok = previousGroupable(children, 0)
	if !ok || found != tool || index != 0 {
		t.Errorf("tool: found %T at %d", found, index)
	}

	// Nothing groupable before the first child.
	if _, _, ok := previousGroupable(children[:1], -1); ok {
		t.Error("an empty prefix has nothing groupable")
	}
	// A leading spacer / assistant message yields nothing.
	if _, _, ok := previousGroupable([]component{newSpacer(1), assistant}, 1); ok {
		t.Error("spacers and assistant messages are not groupable")
	}
}

// Test assistantHasVisibleText (TS L1179-1184).
func TestAssistantHasVisibleText(t *testing.T) {
	r, _ := newGroupRuntime(t)
	if assistantHasVisibleText(nil) {
		t.Error("nil assistant")
	}
	noMessage := &assistantMessageComponent{tui: r}
	if assistantHasVisibleText(noMessage) {
		t.Error("no message")
	}
	blank := r.newTestAssistant(textItem("   "))
	if assistantHasVisibleText(blank) {
		t.Error("blank text is not visible")
	}
	real := r.newTestAssistant(thinkingItem("thinking"), textItem("answer"))
	if !assistantHasVisibleText(real) {
		t.Error("a text block is visible")
	}
}

// Test stageHistoricalThinking / ensureHistoricalThinkingAnchors
// (TS L212-259).
func TestStageHistoricalThinking(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	state := &assistantContentState{anchors: map[int]*toolGroupComponent{}}
	message := &assistantMessage{role: "assistant", usage: &usage{reasoning: intPtrValue(4_000)}}

	r.stageHistoricalThinking(state, 0, []string{"a", "  ", "a", "b"}, message)
	group := state.anchors[0]
	if group == nil {
		t.Fatal("no group was staged")
	}
	if !group.anchored || !group.sealed {
		t.Error("a historical group is anchored and sealed")
	}
	if len(group.entries) != 2 {
		t.Fatalf("entries = %d, want 2 (blanks dropped, duplicates deduplicated)", len(group.entries))
	}
	for _, entry := range group.entries {
		if entry.think.tokens != 4_000 || !entry.think.tokensExact {
			t.Errorf("tokens = %d exact=%v, want the reported 4000", entry.think.tokens, entry.think.tokensExact)
		}
		if entry.think.active {
			t.Error("a historical run is not active")
		}
	}

	// A second staging for the same ordinal is ignored.
	r.stageHistoricalThinking(state, 0, []string{"different"}, message)
	if state.anchors[0] != group {
		t.Error("the ordinal was staged twice")
	}

	// Without a reported count the estimate is used.
	r.stageHistoricalThinking(state, 1, []string{"estimate me"}, &assistantMessage{})
	if state.anchors[1].entries[0].think.tokensExact {
		t.Error("without a report the count is an estimate")
	}

	// Text already shown contributes nothing.
	shown := "already shown"
	_ = r.createThinkingEntry()
	r.mu.Lock()
	r.unattachedThinking[0].text = shown
	r.mu.Unlock()
	r.stageHistoricalThinking(state, 2, []string{shown}, message)
	if state.anchors[2] != nil {
		t.Error("an already-shown run should not be staged")
	}
	_ = assistant
}

// Test ensureHistoricalThinkingAnchors (TS L239-259).
func TestEnsureHistoricalThinkingAnchors(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	container := assistant.contentContainer

	message := &assistantMessage{role: "assistant", content: []contentItem{
		thinkingItem("first thought"),
		thinkingItem("second thought"),
		textItem("answer one"),
		thinkingItem("third thought"),
		textItem("  "),
		textItem("answer two"),
	}}
	r.ensureHistoricalThinkingAnchors(container, message)

	r.mu.Lock()
	state := r.assistantContentStates[container]
	r.mu.Unlock()
	if state == nil {
		t.Fatal("no content state")
	}
	if len(state.anchors[0].entries) != 2 {
		t.Errorf("ordinal 0 has %d runs, want 2", len(state.anchors[0].entries))
	}
	if len(state.anchors[1].entries) != 1 {
		t.Errorf("ordinal 1 has %d runs, want 1", len(state.anchors[1].entries))
	}
	if _, present := state.anchors[2]; present {
		t.Error("a blank text block is not an ordinal")
	}

	// A tool-call-only message does NOT stage its leading thinking: the TS
	// condition needs `!hasToolCalls` (L254).
	r2, _ := newGroupRuntime(t)
	assistant2 := r2.newTestAssistant()
	r2.ensureHistoricalThinkingAnchors(assistant2.contentContainer, &assistantMessage{
		content: []contentItem{thinkingItem("only thought"), toolCallItem()},
	})
	r2.mu.Lock()
	state2 := r2.assistantContentStates[assistant2.contentContainer]
	r2.mu.Unlock()
	if len(state2.anchors) != 0 {
		t.Errorf("a tool-call message must not stage leading thinking: %d anchors", len(state2.anchors))
	}

	// A message with tool calls and no text does not stage at ordinal 0 twice.
	r3, _ := newGroupRuntime(t)
	assistant3 := r3.newTestAssistant()
	r3.ensureHistoricalThinkingAnchors(assistant3.contentContainer, &assistantMessage{
		content: []contentItem{toolCallItem()},
	})
	r3.mu.Lock()
	state3 := r3.assistantContentStates[assistant3.contentContainer]
	r3.mu.Unlock()
	if state3 != nil {
		if _, present := state3.anchors[0]; present {
			t.Error("a tool-call-only message stages nothing")
		}
	}

	// Nil and empty content are no-ops.
	r.ensureHistoricalThinkingAnchors(nil, message)
	r.ensureHistoricalThinkingAnchors(container, nil)
	r.ensureHistoricalThinkingAnchors(container, &assistantMessage{})
}

// Test maybeGroup folds consecutive tool executions (TS L1328-1372).
func TestMaybeGroup(t *testing.T) {
	r, _ := newGroupRuntime(t)
	parent := newContainer(r)
	first := newTestTool("a", "read", nil)
	parent.children = []component{first}

	r.maybeGroup(parent, first)
	if len(parent.children) != 1 {
		t.Fatalf("children = %d", len(parent.children))
	}
	group, ok := parent.children[0].(*toolGroupComponent)
	if !ok {
		t.Fatalf("first tool was not wrapped in a group: %T", parent.children[0])
	}
	if len(group.entries) != 1 || group.entries[0].tool != first {
		t.Errorf("group entries = %+v", group.entries)
	}
	r.mu.Lock()
	active := r.lastActiveGroup
	inGroups := r.groups[group]
	r.mu.Unlock()
	if active != group || !inGroups {
		t.Error("the new group is not the active one")
	}

	// A second tool joins the preceding open group.
	second := newTestTool("b", "write", nil)
	parent.children = append(parent.children, second)
	r.maybeGroup(parent, second)
	if len(parent.children) != 1 {
		t.Fatalf("second tool was not folded: %d children", len(parent.children))
	}
	if len(group.entries) != 2 || group.entries[1].tool != second {
		t.Errorf("group entries after join = %+v", group.entries)
	}
}

// Test maybeGroup skips the open-group path for a non-chat container and
// wraps a lone tool (TS L1345-1372).
func TestMaybeGroupFallbacks(t *testing.T) {
	r, _ := newGroupRuntime(t)

	// A component outside the parent is ignored.
	parent := newContainer(r)
	r.maybeGroup(parent, newTestTool("y", "read", nil))
	if len(parent.children) != 0 {
		t.Error("a component not in the parent must be ignored")
	}

	// A non-tool component is ignored.
	parent.addChild(&textComponent{text: "hi"})
	r.maybeGroup(parent, &textComponent{text: "hi"})

	// A lone tool inside an assistant content container is still wrapped,
	// because the container is not the chat container.
	assistant := r.newTestAssistant()
	content := assistant.contentContainer
	tool := newTestTool("z", "read", nil)
	content.addChild(tool)
	r.maybeGroup(content, tool)
	if len(content.children) != 1 {
		t.Fatalf("content children = %d", len(content.children))
	}
	if _, ok := content.children[0].(*toolGroupComponent); !ok {
		t.Errorf("the tool was not grouped in the content container: %T", content.children[0])
	}
}

// Test joinOpenGroup (TS L1307-1327) relocates the group after the streaming
// assistant message.
func TestJoinOpenGroup(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	open := r.newToolGroup(0)
	tool := newTestTool("new", "read", nil)
	chat.children = []component{assistant, open, tool}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.addGroupLocked(open)
	r.lastActiveGroup = open
	r.mu.Unlock()

	if !r.joinOpenGroup(chat, 2, tool) {
		t.Fatal("joinOpenGroup returned false")
	}
	if chat.contains(tool) {
		t.Error("the tool should have left the chat container")
	}
	if len(open.entries) != 1 || open.entries[0].tool != tool {
		t.Errorf("group entries = %+v", open.entries)
	}
	// The group sits right after the streaming message.
	if chat.children[1] != open {
		t.Errorf("group is not after the streaming message: %T", chat.children[1])
	}

	// A different parent never joins.
	other := newContainer(r)
	if r.joinOpenGroup(other, 0, newTestTool("q", "read", nil)) {
		t.Error("a foreign parent must not join")
	}

	// Without an open group there is nothing to join.
	r.mu.Lock()
	r.lastActiveGroup = nil
	r.lastChatContainer = nil
	r.mu.Unlock()
	if r.joinOpenGroup(chat, 0, newTestTool("w", "read", nil)) {
		t.Error("no open group should mean no join")
	}
}

// Test openToolGroup (TS L642-668).
func TestOpenToolGroup(t *testing.T) {
	r, _ := newGroupRuntime(t)
	// Without a chat container and streaming message there is no group.
	if got := r.openToolGroup(); got != nil {
		t.Errorf("openToolGroup without a container = %v", got)
	}

	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	group := r.openToolGroup()
	if group == nil {
		t.Fatal("openToolGroup created no group")
	}
	if chat.children[1] != group {
		t.Errorf("group is not after the streaming message: %T", chat.children[1])
	}

	// A second call returns the same open group and re-places it.
	again := r.openToolGroup()
	if again != group {
		t.Error("openToolGroup did not reuse the open group")
	}
}

// Test sealOpenGroupAtAssistantText (TS L1202-1218).
func TestSealOpenGroupAtAssistantText(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	open := r.newToolGroup(0)
	chat.children = []component{open}
	r.mu.Lock()
	r.addGroupLocked(open)
	r.lastActiveGroup = open
	r.lastChatContainer = chat
	r.mu.Unlock()

	// The assistant message has a visible text block and its content container
	// holds a visible Markdown, so the group anchors before it.
	assistant := r.newTestAssistant(thinkingItem("pre-text thought"), textItem("the answer"))
	assistant.contentContainer.children = []component{&markdownComponent{text: "the answer"}}

	_ = r.createThinkingEntry()
	r.sealOpenGroupAtAssistantText(assistant)

	if !open.sealed {
		t.Error("the open group should be sealed")
	}
	if len(open.entries) != 1 || open.entries[0].kind != "thinking" {
		t.Errorf("the pre-text thinking was not absorbed: %+v", open.entries)
	}
	r.mu.Lock()
	active := r.activeThinking
	r.mu.Unlock()
	if active != nil {
		t.Error("the active run should be finalized")
	}
}

// Test flushPendingTextSeal (TS L1246-1270).
func TestFlushPendingTextSeal(t *testing.T) {
	r, _ := newGroupRuntime(t)

	// Nothing pending: no-op.
	r.flushPendingTextSeal()

	// A pending seal with an empty open group drops the group.
	chat := newContainer(r)
	chatWithStreaming := newContainer(r)
	assistant := r.newTestAssistant()
	chatWithStreaming.children = []component{assistant}
	empty := r.newToolGroup(0)
	chat.children = []component{empty}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.addGroupLocked(empty)
	r.lastActiveGroup = empty
	r.pendingTextSeal = true
	r.pendingTextOrdinal = intPtrValue(0)
	r.mu.Unlock()

	r.flushPendingTextSeal()
	r.mu.Lock()
	pending := r.pendingTextSeal
	ordinal := r.pendingTextOrdinal
	inGroups := r.groups[empty]
	r.mu.Unlock()
	if pending || ordinal != nil {
		t.Error("the pending seal was not cleared")
	}
	if inGroups {
		t.Error("an empty group should have been dropped")
	}
	if chat.contains(empty) {
		t.Error("the empty group should have left the chat container")
	}
	if !empty.sealed {
		t.Error("the group should be sealed even after being dropped")
	}

	// A pending seal with a non-empty open group seals and anchors it.
	r2, _ := newGroupRuntime(t)
	chat2 := newContainer(r2)
	assistant2 := r2.newTestAssistant()
	assistant2.contentContainer.children = []component{
		&markdownComponent{text: "first"},
		&markdownComponent{text: "second"},
	}
	chat2.children = []component{assistant2}
	group := r2.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
	r2.mu.Lock()
	r2.lastChatContainer = chat2
	r2.lastStreamingComp = assistant2
	r2.addGroupLocked(group)
	r2.lastActiveGroup = group
	r2.pendingTextSeal = true
	r2.pendingTextOrdinal = intPtrValue(1)
	r2.mu.Unlock()
	r2.flushPendingTextSeal()
	if !group.sealed {
		t.Error("a non-empty group should be sealed")
	}
	r2.mu.Lock()
	_, anchored := r2.groupAnchors[group]
	r2.mu.Unlock()
	if !anchored {
		t.Error("the group should have been anchored before the pending text ordinal")
	}
}

// Test insertTurnDivider (TS L392-418).
func TestInsertTurnDivider(t *testing.T) {
	r, _ := newGroupRuntime(t)

	// Below one second there is no divider.
	r.insertTurnDivider(500)

	assistant := r.newTestAssistant()
	content := assistant.contentContainer
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
	final := &markdownComponent{text: "final answer"}
	content.children = []component{group, final}
	r.mu.Lock()
	r.lastStreamingComp = assistant
	r.addGroupLocked(group)
	r.mu.Unlock()

	r.insertTurnDivider(65_000)
	divider, ok := content.children[1].(*turnDividerComponent)
	if !ok {
		t.Fatalf("no divider before the final text: %T", content.children[1])
	}
	if divider.timeLabel != "1m 5s" {
		t.Errorf("divider label = %q, want 1m 5s", divider.timeLabel)
	}
	// The divider renders at a narrow width too: the middle label is cut, and
	// the record still ends with the blank separator line (L372).
	narrow := divider.Render(12)
	if len(narrow) != 2 {
		t.Errorf("narrow divider lines = %d, want 2", len(narrow))
	}
	if narrow[1] != "" {
		t.Errorf("a divider ends with a blank line: %q", narrow[1])
	}
	if tuiLen(narrow[0]) > 12 {
		t.Errorf("narrow divider = %d columns, want <= 12: %q", tuiLen(narrow[0]), narrow[0])
	}

	// The wide form keeps the middle label intact.
	lines := divider.Render(80)
	if len(lines) != 2 || !strings.Contains(lines[0], "worked for 1m 5s") {
		t.Errorf("divider lines = %v", lines)
	}

	r.insertTurnDivider(66_000)
	count := 0
	for _, child := range content.children {
		if _, isDivider := child.(*turnDividerComponent); isDivider {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dividers = %d, want 1", count)
	}
}

// Test insertTurnDivider needs prior content (TS L407-409).
func TestInsertTurnDividerNeedsPriorContent(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	content := assistant.contentContainer
	content.children = []component{&markdownComponent{text: "only text"}}
	r.mu.Lock()
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	r.insertTurnDivider(5_000)
	for _, child := range content.children {
		if _, isDivider := child.(*turnDividerComponent); isDivider {
			t.Error("a turn without grouped tools gets no divider")
		}
	}

	// Without a streaming component the last assistant message is used.
	r2, _ := newGroupRuntime(t)
	chat := newContainer(r2)
	assistant2 := r2.newTestAssistant()
	content2 := assistant2.contentContainer
	group := r2.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
	content2.children = []component{group, &markdownComponent{text: "final"}}
	chat.children = []component{assistant2}
	r2.mu.Lock()
	r2.lastChatContainer = chat
	r2.addGroupLocked(group)
	r2.mu.Unlock()
	r2.insertTurnDivider(5_000)
	found := false
	for _, child := range content2.children {
		if _, isDivider := child.(*turnDividerComponent); isDivider {
			found = true
		}
	}
	if !found {
		t.Error("the fallback path should still insert a divider")
	}
}

// Test absorbCacheMissNotice (TS L1396-1414).
func TestAbsorbCacheMissNotice(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	// absorbCacheMissNotice can only fold a notice into a group; opening one
	// requires the chat container and streaming message the session start sets.
	r.mu.Lock()
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	// With no container captured, the parent becomes the chat container.
	if !r.absorbCacheMissNotice(chat, "cache miss: 1 tokens re-billed") {
		t.Fatal("the notice was not absorbed")
	}
	r.mu.Lock()
	captured := r.lastChatContainer
	group := r.lastActiveGroup
	r.mu.Unlock()
	if captured != chat {
		t.Error("the chat container was not captured")
	}
	if group == nil || len(group.entries) != 1 || group.entries[0].kind != "notice" {
		t.Fatalf("notice entry missing: %+v", group)
	}
	notice := group.entries[0].notice
	// addNotice claims ownership for the group it joins (TS L860-864).
	if notice.noticeType != "cache_miss" || notice.owner != group {
		t.Errorf("notice = %+v (owner should be the group)", notice)
	}

	// A second notice reuses the open group.
	r.absorbCacheMissNotice(chat, "cache miss: 2 tokens re-billed")
	if len(group.entries) != 2 {
		t.Errorf("entries = %d, want 2", len(group.entries))
	}
}

// Test addChildTo folds a cache-miss notice instead of appending it
// (TS L1442-1457).
func TestAddChildAbsorbsCacheMiss(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	chat.addChild(newSpacer(1))
	r.addChildTo(chat, &textComponent{text: "cache miss: 42 tokens re-billed"})

	// The trailing spacer was consumed and no plain text child was appended: the
	// chat holds only the streaming assistant message and the group the notice
	// landed in (openToolGroup appends it after the streaming message).
	assistantCount, groupCount, plainCount := 0, 0, 0
	for _, child := range chat.children {
		switch child.(type) {
		case *assistantMessageComponent:
			assistantCount++
		case *toolGroupComponent:
			groupCount++
		case *textComponent:
			plainCount++
		case *spacer:
			t.Error("the trailing spacer should have been consumed")
		}
	}
	if plainCount != 0 {
		t.Errorf("the notice text became a chat child: %d", plainCount)
	}
	if assistantCount != 1 || groupCount != 1 {
		t.Fatalf("chat children = %d assistant, %d groups, want 1 and 1: %+v", assistantCount, groupCount, chat.children)
	}
	r.mu.Lock()
	group := r.lastActiveGroup
	r.mu.Unlock()
	if group == nil || len(group.entries) != 1 || group.entries[0].kind != "notice" {
		t.Errorf("notice not absorbed into a group: %+v", group)
	}

	// Non-notice text is appended normally.
	before := len(chat.children)
	chat.addChild(&textComponent{text: "plain text"})
	if len(chat.children) != before+1 {
		t.Errorf("plain text was not appended: %d children", len(chat.children))
	}
}

// Test removeChildFrom removes a grouped tool from its group (TS L1481-1489).
func TestRemoveChildFromGroupedTool(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	group := r.newToolGroup(0)
	tool := newTestTool("t", "read", nil)
	chat.children = []component{group}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.mu.Unlock()
	group.addTool(tool)

	r.removeChildFrom(chat, tool)
	if len(group.entries) != 0 {
		t.Errorf("the tool was not removed from the group: %+v", group.entries)
	}
	// The group is dropped once it is empty.
	r.mu.Lock()
	_, present := r.groups[group]
	r.mu.Unlock()
	if present {
		t.Error("an emptied group should leave runtime.groups")
	}
	// A non-grouped component is removed from the container.
	text := &textComponent{text: "x"}
	chat.addChild(text)
	r.removeChildFrom(chat, text)
	if chat.contains(text) {
		t.Error("a plain child was not removed")
	}
}

// Test clearPatched resets the text ordinal and drops groups
// (TS L1490-1502).
func TestClearPatched(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	content := assistant.contentContainer
	r.mu.Lock()
	state := r.getAssistantContentStateLocked(content)
	state.nextTextOrdinal = 3
	r.mu.Unlock()
	r.clearPatched(content)
	r.mu.Lock()
	reset := r.getAssistantContentStateLocked(content).nextTextOrdinal
	r.mu.Unlock()
	if reset != 0 {
		t.Errorf("nextTextOrdinal = %d, want 0", reset)
	}

	// A chat container drops its groups and releases the tools.
	chat := newContainer(r)
	group := r.newToolGroup(0)
	tool := newTestTool("t", "read", nil)
	chat.children = []component{group}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.mu.Unlock()
	group.addTool(tool)
	r.clearPatched(chat)
	r.mu.Lock()
	_, present := r.groups[group]
	r.mu.Unlock()
	if present {
		t.Error("clear should drop groups")
	}
	if tool.owner != nil {
		t.Error("clear should release grouped tools")
	}
	if len(chat.children) != 0 {
		t.Errorf("children = %d", len(chat.children))
	}
}

// Test restoreAssistantAnchor re-places a staged group and divider
// (TS L474-487).
func TestRestoreAssistantAnchor(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	content := assistant.contentContainer
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("t", "read", nil).succeeded("ok")}}
	divider := &turnDividerComponent{timeLabel: "3s", tui: r}
	r.mu.Lock()
	state := r.getAssistantContentStateLocked(content)
	state.anchors[0] = group
	state.finalDivider = &dividerPlacement{ordinal: 0, component: divider}
	r.addGroupLocked(group)
	r.mu.Unlock()

	// The restore hook only re-places the staged group and divider: the caller
	// appends the text child itself, as the patched addChild does after
	// original.addChild. So append it here first.
	text := &markdownComponent{text: "answer"}
	content.addChild(text)
	r.restoreAssistantAnchor(content, text)
	if !content.contains(text) {
		t.Fatal("the text child should be in the container")
	}
	// The group and the divider now sit before the text.
	groupIndex := content.indexOf(group)
	textIndex := content.indexOf(text)
	dividerIndex := content.indexOf(divider)
	if groupIndex < 0 || groupIndex > textIndex {
		t.Errorf("group index = %d, text index = %d", groupIndex, textIndex)
	}
	if dividerIndex < 0 || dividerIndex > textIndex {
		t.Errorf("divider index = %d, text index = %d", dividerIndex, textIndex)
	}

	// A non-content container, or a non-Markdown child, is ignored.
	r.restoreAssistantAnchor(nil, text)
	r.restoreAssistantAnchor(content, &textComponent{text: "x"})
	other := newContainer(r)
	r.restoreAssistantAnchor(other, text)
}

// Test releaseAssistantAnchors (TS L488-503).
func TestReleaseAssistantAnchors(t *testing.T) {
	r, _ := newGroupRuntime(t)
	assistant := r.newTestAssistant()
	content := assistant.contentContainer
	group := r.newToolGroup(0)
	tool := newTestTool("t", "read", nil)
	group.addTool(tool)
	r.mu.Lock()
	state := r.getAssistantContentStateLocked(content)
	state.anchors[0] = group
	state.finalDivider = &dividerPlacement{ordinal: 0, component: &turnDividerComponent{tui: r}}
	r.addGroupLocked(group)
	r.groupAnchors[group] = &groupAnchor{container: content, ordinal: 0}
	r.mu.Unlock()

	r.releaseAssistantAnchors(assistant)
	if tool.owner != nil {
		t.Error("the tool kept its group")
	}
	r.mu.Lock()
	_, inGroups := r.groups[group]
	_, anchored := r.groupAnchors[group]
	state2 := r.assistantContentStates[content]
	r.mu.Unlock()
	if inGroups || anchored {
		t.Error("the group survived release")
	}
	if len(state2.anchors) != 0 || state2.finalDivider != nil {
		t.Error("the content state was not cleared")
	}

	// A non-assistant component is ignored.
	r.releaseAssistantAnchors(&textComponent{text: "x"})
	r.releaseAssistantAnchors(nil)
}

// Test stripAssistantPhantomPadding (TS L504-517).
func TestStripAssistantPhantomPadding(t *testing.T) {
	r, _ := newGroupRuntime(t)

	// A content container passed as a child is registered.
	assistant := r.newTestAssistant()
	content := newContainer(r)
	r.stripAssistantPhantomPadding(newContainer(r), assistant)
	r.mu.Lock()
	registered := r.assistantContentCont[assistant.contentContainer]
	r.mu.Unlock()
	if !registered {
		t.Error("the content container was not registered")
	}
	_ = content

	// A native placeholder and the spacer right after it are removed. The
	// trailing-spacer loop only fires when every remaining child is a spacer
	// (L512), so the visible answer keeps the container from being emptied.
	chat := newContainer(r)
	rich := r.newTestAssistant()
	richContent := rich.contentContainer
	placeholder := &textComponent{text: "Thinking..."}
	realText := &markdownComponent{text: "answer"}
	richContent.children = []component{realText, placeholder, newSpacer(1)}
	r.stripAssistantPhantomPadding(richContent, placeholder)
	if richContent.contains(placeholder) {
		t.Error("the placeholder was not removed")
	}
	if len(richContent.children) != 1 || richContent.children[0] != realText {
		t.Errorf("children = %+v, want only the visible text", richContent.children)
	}
	_ = chat

	// A non-native widget is left alone.
	plain := newContainer(r)
	r.mu.Lock()
	r.assistantContentCont[plain] = true
	r.mu.Unlock()
	kept := &markdownComponent{text: "visible"}
	plain.children = []component{kept}
	r.stripAssistantPhantomPadding(plain, kept)
	if !plain.contains(kept) {
		t.Error("visible text must not be stripped")
	}
}

// Test installGrouping and the patch flags are idempotent (TS L1415-1505).
func TestInstallGrouping(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.installGrouping()
	r.installGrouping()
	r.mu.Lock()
	installed := r.groupingInstalled
	r.mu.Unlock()
	if !installed {
		t.Error("installGrouping did not record itself")
	}

	r.installNativeThinkingSuppression()
	r.mu.Lock()
	suppressed := r.thinkingSuppressionInstalled
	r.mu.Unlock()
	if !suppressed {
		t.Error("installNativeThinkingSuppression did not record itself")
	}
}

// Test concurrency: many goroutines drive the grouping model at once. The race
// detector is the assertion.
func TestGroupingConcurrentReads(t *testing.T) {
	r, _ := newGroupRuntime(t)
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	chat.children = []component{assistant}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.mu.Unlock()

	group := r.newToolGroup(0)
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()
	for i := 0; i < 5; i++ {
		group.addTool(newTestTool("t"+itoa(i), "read", nil).succeeded("ok"))
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = group.Render(60)
				_ = group.needsAnimation()
				_, _ = group.workPhaseLabel()
				_ = r.allInspectableEntries()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				group.addTool(newTestTool("c"+itoa(seed)+"-"+itoa(j), "read", nil).succeeded("ok"))
				r.rememberToolStart("watch-"+itoa(seed), 0)
			}
		}(i)
	}
	wg.Wait()
}

// TestStreamingFirstFrameWithoutIDKeepsDisplay covers both id-less first-frame
// edge cases of brief2 R3 S1. (a) A provisional entry whose stream carries no
// call id yet has no host card (the host creates one only once the block has an
// id), so it must not take the display rights from the card that can paint the
// tree -- and once the id arrives it must take over and invalidate that card.
// (b) A new assistant message starts a new stream: the content index a block
// occupies no longer identifies a call of the previous message, so the first
// id-less frame must not match a completed call there and overwrite its
// arguments.
func TestStreamingFirstFrameWithoutIDKeepsDisplay(t *testing.T) {
	r, _ := newGroupRuntime(t)
	r.setClock(func() int64 { return 1_000 })
	chat := newContainer(r)
	assistant := r.newTestAssistant()
	group := r.newToolGroup(0)
	finished := newTestTool("call-a", "read", map[string]any{"path": "/a"})
	finished.contentIndex = 0
	finished.hasIndex = true
	finished.succeeded("a-out")
	group.addTool(finished)
	invalidates := map[string]int{}
	r.mu.Lock()
	r.lastChatContainer = chat
	r.lastStreamingComp = assistant
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.toolCardInvalidate = map[string]func(){
		"call-a": func() { invalidates["call-a"]++ },
	}
	r.mu.Unlock()
	chat.children = []component{assistant, group}

	// (a) The next call's first frame carries no id: the host has mounted no card
	// for it, so the finished call's card stays the one that paints the tree.
	content := []contentBlock{
		{typ: "text", block: streamBlock{text: "next"}},
		{typ: "toolCall", block: streamBlock{name: "ctx_run", typ: "toolCall", arguments: map[string]any{"command": "echo a"}}},
	}
	r.noteStreamingToolCall(content, streamEvent{typ: "toolcall_start", contentIndex: intPtrValue(1)})
	r.mu.Lock()
	preparing := r.preparingByIndex[1]
	displayFinished := isGroupHostDisplayToolLocked(group, finished)
	displayID := group.displayCardIDLocked()
	r.mu.Unlock()
	if preparing == nil {
		t.Fatal("no provisional entry was created for the id-less frame")
	}
	if preparing.toolCallID != "preparing:1" {
		t.Errorf("provisional id = %q, want the preparing:1 placeholder", preparing.toolCallID)
	}
	if !displayFinished {
		t.Error("the id-less provisional entry took the display rights: its host card does not exist yet, so the screen loses the whole tree")
	}
	if displayID != "call-a" {
		t.Errorf("displayCardIDLocked = %q, want the mounted card call-a", displayID)
	}
	tree := r.toolRenderLines("read", finished.args, sdk.ToolRenderContext{
		ToolCallID:   "call-a",
		ArgsComplete: true,
	}, nil, false, 80, false)
	if len(tree) < 3 {
		t.Fatalf("tree lines = %v, want the header, the finished call and the streaming row", tree)
	}

	// The real id arrives: the host has now mounted the card, so the call takes
	// the tree over and the card that painted until then is invalidated.
	invalidates["call-a"] = 0
	content[1].block.id = "call-new"
	r.noteStreamingToolCall(content, streamEvent{typ: "toolcall_delta", contentIndex: intPtrValue(1)})
	r.mu.Lock()
	displayNew := isGroupHostDisplayToolLocked(group, preparing.comp)
	displayOld := isGroupHostDisplayToolLocked(group, finished)
	r.mu.Unlock()
	if !displayNew || displayOld {
		t.Errorf("after the id arrived: new=%v old=%v, want the new call to paint the tree", displayNew, displayOld)
	}
	if invalidates["call-a"] == 0 {
		t.Error("the previous display card was not invalidated when the real id arrived: it keeps painting the tree next to the new one")
	}

	// (b) A new assistant message must not let its first id-less frame match the
	// previous message's completed call by content index.
	r.handleMessageStart(map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{}},
	})
	first := []contentBlock{{typ: "toolCall", block: streamBlock{name: "write", typ: "toolCall", arguments: map[string]any{"path": "/b"}}}}
	r.noteStreamingToolCall(first, streamEvent{typ: "toolcall_start", contentIndex: intPtrValue(0)})
	r.mu.Lock()
	finishedArgs := finished.args["path"]
	finishedIndex, finishedHasIndex := finished.contentIndex, finished.hasIndex
	r.mu.Unlock()
	if finishedArgs != "/a" {
		t.Errorf("the previous message's tool arguments were overwritten: %v", finished.args)
	}
	if finishedHasIndex {
		t.Errorf("the previous message's call still answers content index %d", finishedIndex)
	}
}
