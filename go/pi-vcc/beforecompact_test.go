package pivcc

import (
	"os"
	"strings"
	"testing"
)

// Tests for src/hooks/before-compact.ts: version gating, instruction parsing,
// live-window collection, the own-cut decision, the tail budget rescue and the
// smart-keep boost.

// message builds a branch entry of type "message".
func messageEntry(id, role, text string) map[string]any {
	return map[string]any{
		"id":   id,
		"type": "message",
		"message": map[string]any{
			"role":    role,
			"content": text,
		},
	}
}

// compactionEntry builds a compaction branch entry.
func compactionEntry(id, firstKeptID string) map[string]any {
	return map[string]any{"id": id, "type": "compaction", "firstKeptEntryId": firstKeptID}
}

// transcript builds a simple user/assistant alternating transcript.
func transcript(turns int) []map[string]any {
	entries := make([]map[string]any, 0, turns*2)
	for i := 0; i < turns; i++ {
		n := jsNumberString(float64(i))
		entries = append(entries, messageEntry("u"+n, "user", "please do task "+n))
		entries = append(entries, messageEntry("a"+n, "assistant", "done with task "+n))
	}
	return entries
}

// ── version gating ──────────────────────────────────────────────────────────

func TestParseVersionCore(t *testing.T) {
	tests := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"1.2.3", [3]int{1, 2, 3}, true},
		{"v1.2.3", [3]int{1, 2, 3}, true},
		{"1.2.3-beta.1", [3]int{1, 2, 3}, true},
		{"1.2.3+build5", [3]int{1, 2, 3}, true},
		{"1.2", [3]int{}, false},
		{"", [3]int{}, false},
		{"garbage", [3]int{}, false},
		{"1.2.x", [3]int{}, false},
		{"1.x.3", [3]int{}, false},
	}
	for _, test := range tests {
		got, ok := parseVersionCore(test.in)
		if ok != test.ok || got != test.want {
			t.Fatalf("parseVersionCore(%q) = (%v, %v), want (%v, %v)", test.in, got, ok, test.want, test.ok)
		}
	}
}

func TestShouldScheduleAutoContinue(t *testing.T) {
	older := jsNumberString(float64(PiSelfResumeVersion[0])) + "." +
		jsNumberString(float64(PiSelfResumeVersion[1])) + ".0"
	newer := jsNumberString(float64(PiSelfResumeVersion[0])) + "." +
		jsNumberString(float64(PiSelfResumeVersion[1]+1)) + ".0"
	exact := jsNumberString(float64(PiSelfResumeVersion[0])) + "." +
		jsNumberString(float64(PiSelfResumeVersion[1])) + "." +
		jsNumberString(float64(PiSelfResumeVersion[2]))

	// Both keys must turn.
	if !ShouldScheduleAutoContinue(true, older) {
		t.Fatalf("an older pi with the setting on should continue (threshold %v, got %q)",
			PiSelfResumeVersion, older)
	}
	if ShouldScheduleAutoContinue(false, older) {
		t.Fatal("the setting being off must suppress the continue")
	}
	if ShouldScheduleAutoContinue(true, newer) {
		t.Fatal("a newer pi must not schedule the fallback continue")
	}
	// An unreadable version fails safe.
	if ShouldScheduleAutoContinue(true, "unknown") {
		t.Fatal("an unparseable version must fail safe to no continue")
	}
	// The exact threshold version does not need the fallback.
	if len(PiSelfResumeVersion) == 3 && ShouldScheduleAutoContinue(true, exact) {
		t.Fatal("the threshold version itself must not schedule a continue")
	}
}

// ── stats formatting ────────────────────────────────────────────────────────

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{1200, "1.2k"},
		{12345, "12.3k"},
	}
	for _, test := range tests {
		if got := formatTokens(test.in); got != test.want {
			t.Fatalf("formatTokens(%d) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestFormatCompactionStats(t *testing.T) {
	normal := FormatCompactionStats(CompactionStats{
		KeptUserTurns: 2, TotalUserTurns: 7, KeptTokensEst: 1500, Summarized: 12,
	})
	for _, want := range []string{"kept 2/7 turns", "1.5k tok", "summarized 12"} {
		if !strings.Contains(normal, want) {
			t.Fatalf("stats lost %q: %q", want, normal)
		}
	}

	smart := FormatCompactionStats(CompactionStats{
		KeptUserTurns: 3, TotalUserTurns: 7, KeptTokensEst: 900, Summarized: 4, SmartKeepAdjusted: true,
	})
	if !strings.Contains(smart, "smart-keep") {
		t.Fatalf("the smart-keep note is missing: %q", smart)
	}

	oversized := FormatCompactionStats(CompactionStats{
		BudgetCut: string(BudgetCutOversizedTail), KeptTokensEst: 2000, Summarized: 9,
	})
	if !strings.Contains(oversized, "mid-turn cut") || !strings.Contains(oversized, "oversized tail") {
		t.Fatalf("the oversized-tail note is wrong: %q", oversized)
	}

	noAnchor := FormatCompactionStats(CompactionStats{
		BudgetCut: string(BudgetCutNoAnchor), KeptTokensEst: 2000, Summarized: 9,
	})
	if !strings.Contains(noAnchor, "no user anchor") {
		t.Fatalf("the no-anchor note is wrong: %q", noAnchor)
	}
}

// ── event context / instructions ────────────────────────────────────────────

func TestReadCompactionEventContext(t *testing.T) {
	reason, retry := ReadCompactionEventContext(map[string]any{"reason": "threshold", "willRetry": true})
	if reason != ReasonThreshold || !retry {
		t.Fatalf("ReadCompactionEventContext = (%v, %v)", reason, retry)
	}
	// An unknown reason is empty, not a bogus value.
	reason, _ = ReadCompactionEventContext(map[string]any{"reason": "mystery"})
	if reason != "" {
		t.Fatalf("unknown reason = %q, want empty", reason)
	}
	reason, retry = ReadCompactionEventContext(map[string]any{})
	if reason != "" || retry {
		t.Fatalf("empty event = (%v, %v)", reason, retry)
	}
}

func TestParseCompactionInstructions(t *testing.T) {
	// The bare marker: pi-vcc's own compaction with the default keep.
	bare := ParseCompactionInstructions(PiVccCompactInstruction)
	if !bare.IsPiVcc || bare.KeepUserTurns != 1 || bare.KeepUserTurnsExplicit {
		t.Fatalf("bare marker = %+v", bare)
	}

	// Marker plus keep:N.
	withKeep := ParseCompactionInstructions(PiVccCompactInstruction + " keep:4")
	if !withKeep.IsPiVcc || withKeep.KeepUserTurns != 4 || !withKeep.KeepUserTurnsExplicit {
		t.Fatalf("marker with keep = %+v", withKeep)
	}

	// Marker with a keep-less follow-up prompt still counts as pi-vcc.
	withPrompt := ParseCompactionInstructions(PiVccCompactInstruction + " summarise the auth work")
	if !withPrompt.IsPiVcc || withPrompt.KeepUserTurns != 1 {
		t.Fatalf("marker with prompt = %+v", withPrompt)
	}

	// A plain instruction is not pi-vcc, but its keep:N still applies.
	plain := ParseCompactionInstructions("keep:2 tidy the summary")
	if plain.IsPiVcc {
		t.Fatalf("a plain instruction was read as pi-vcc: %+v", plain)
	}
	if plain.KeepUserTurns != 2 || !plain.KeepUserTurnsExplicit {
		t.Fatalf("plain keep = %+v", plain)
	}
	if !plain.HasFollowUpPrompt || plain.FollowUpPrompt != "tidy the summary" {
		t.Fatalf("plain prompt = %+v", plain)
	}

	// Nothing at all: defaults, no prompt.
	empty := ParseCompactionInstructions("")
	if empty.IsPiVcc || empty.KeepUserTurns != 1 || empty.HasFollowUpPrompt {
		t.Fatalf("empty instructions = %+v", empty)
	}
}

func TestNormalizeKeepUserTurns(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, 0}, {3, 3}, {-1, 0}, {-99, 0},
	}
	for _, test := range tests {
		if got := NormalizeKeepUserTurns(test.in); got != test.want {
			t.Fatalf("NormalizeKeepUserTurns(%d) = %d, want %d", test.in, got, test.want)
		}
	}
}

// ── collectLiveMessages ─────────────────────────────────────────────────────

func TestCollectLiveMessages_NoCompaction(t *testing.T) {
	entries := transcript(2)
	live := CollectLiveMessages(entries)
	if len(live) != 4 {
		t.Fatalf("live entries = %d, want 4", len(live))
	}
	if live[0].EntryID != "u0" || live[3].EntryID != "a1" {
		t.Fatalf("live window = %#v", live)
	}
}

func TestCollectLiveMessages_FromFirstKept(t *testing.T) {
	entries := []map[string]any{
		messageEntry("u0", "user", "old"),
		messageEntry("a0", "assistant", "old reply"),
		compactionEntry("c1", "u1"),
		messageEntry("u1", "user", "kept"),
		messageEntry("a1", "assistant", "kept reply"),
	}
	live := CollectLiveMessages(entries)
	if len(live) != 2 {
		t.Fatalf("live entries = %d, want 2: %#v", len(live), live)
	}
	if live[0].EntryID != "u1" {
		t.Fatalf("the live window should start at the kept id: %#v", live)
	}
}

func TestCollectLiveMessages_OrphanRecovery(t *testing.T) {
	// firstKeptEntryId names an entry that does not exist: fall back to
	// everything after the compaction.
	entries := []map[string]any{
		messageEntry("u0", "user", "old"),
		compactionEntry("c1", "missing-id"),
		messageEntry("u1", "user", "after"),
	}
	live := CollectLiveMessages(entries)
	if len(live) != 1 || live[0].EntryID != "u1" {
		t.Fatalf("orphan recovery produced %#v", live)
	}
}

func TestCollectLiveMessages_ContextEditDefers(t *testing.T) {
	// A context-edited history needs pi's projection, which the SDK does not
	// expose here: the collector reports nothing and the hook errors out.
	entries := []map[string]any{
		{"id": "e1", "type": "context_edit"},
		messageEntry("u0", "user", "text"),
	}
	if live := CollectLiveMessages(entries); len(live) != 0 {
		t.Fatalf("a context_edit history should yield no live entries: %#v", live)
	}
}

func TestCollectLiveMessages_SkipsCompactionEntries(t *testing.T) {
	entries := []map[string]any{
		compactionEntry("c1", ""),
		compactionEntry("c2", ""),
		messageEntry("u1", "user", "live"),
	}
	live := CollectLiveMessages(entries)
	if len(live) != 1 || live[0].EntryID != "u1" {
		t.Fatalf("compaction entries leaked into the window: %#v", live)
	}
}

// ── buildOwnCut ─────────────────────────────────────────────────────────────

func TestBuildOwnCut_CancelCases(t *testing.T) {
	// Nothing live at all.
	if got := BuildOwnCut(nil, 1, nil); got.OK || got.Reason != CancelNoLiveMessages {
		t.Fatalf("empty transcript = %#v", got)
	}
	// Two entries is too few to be worth a compaction.
	if got := BuildOwnCut(transcript(1), 1, nil); got.OK || got.Reason != CancelTooFewLiveMessages {
		t.Fatalf("two entries = %#v", got)
	}
}

func TestBuildOwnCut_KeepZeroCompactsAll(t *testing.T) {
	got := BuildOwnCut(transcript(3), 0, nil)
	if !got.OK || !got.CompactAll {
		t.Fatalf("keep:0 should compact all: %#v", got)
	}
	if got.KeepFallbackToCompactAll {
		t.Fatalf("keep:0 must not be flagged as a fallback: %#v", got)
	}
	if got.TotalUserTurns != 3 || len(got.Messages) != 6 {
		t.Fatalf("keep:0 window = %#v", got)
	}
	if got.FirstKeptEntryID != "" {
		t.Fatalf("compact-all leaves no first kept id: %q", got.FirstKeptEntryID)
	}
}

func TestBuildOwnCut_KeepsTheRequestedTurns(t *testing.T) {
	entries := transcript(4)
	got := BuildOwnCut(entries, 2, nil)
	if !got.OK || got.CompactAll {
		t.Fatalf("keep:2 = %#v", got)
	}
	if got.KeptUserTurns != 2 || got.TotalUserTurns != 4 {
		t.Fatalf("kept %d/%d turns, want 2/4", got.KeptUserTurns, got.TotalUserTurns)
	}
	// The cut lands on the third user turn (index 4 of 8 entries).
	if got.FirstKeptEntryID != "u2" {
		t.Fatalf("FirstKeptEntryID = %q, want u2", got.FirstKeptEntryID)
	}
	// The summarized window is everything before the cut.
	if len(got.Messages) != 4 || len(got.SelectedIDs) != 4 {
		t.Fatalf("summarized window = %d messages, %d ids", len(got.Messages), len(got.SelectedIDs))
	}
	if got.RequestedKeepUserTurns != 2 {
		t.Fatalf("RequestedKeepUserTurns = %d", got.RequestedKeepUserTurns)
	}
}

func TestBuildOwnCut_KeepAllFallsBackToCompactAll(t *testing.T) {
	// Asking to keep every turn leaves nothing to summarize.
	got := BuildOwnCut(transcript(3), 3, nil)
	if !got.OK || !got.CompactAll || !got.KeepFallbackToCompactAll {
		t.Fatalf("keep:all = %#v", got)
	}
}

func TestBuildOwnCut_SkipsCustomTypes(t *testing.T) {
	entries := []map[string]any{
		messageEntry("u0", "user", "task"),
		{"id": "c1", "type": "custom_message", "customType": "pi-vcc-note", "content": "noise"},
		messageEntry("a0", "assistant", "reply"),
		messageEntry("u1", "user", "next"),
		messageEntry("a1", "assistant", "reply2"),
	}
	// Skipping the custom type does not remove the renderable messages.
	got := BuildOwnCut(entries, 1, []string{"pi-vcc-note"})
	if !got.OK {
		t.Fatalf("BuildOwnCut = %#v", got)
	}
}

// ── budget cut ──────────────────────────────────────────────────────────────

func TestFindBudgetCutIndex(t *testing.T) {
	// Four messages of ~100 chars each; a 200-token budget at 4 chars/token
	// needs ~800 chars, so the walk from the tail crosses at index 0.
	entries := transcript(2)
	live := CollectLiveMessages(entries)
	idx := FindBudgetCutIndex(live, 10, 4)
	if idx < 0 {
		t.Fatalf("FindBudgetCutIndex = %d, want a cut inside the window", idx)
	}
	if idx >= len(live) {
		t.Fatalf("cut index %d is past the window", idx)
	}
	// A huge budget keeps everything: no cut.
	if got := FindBudgetCutIndex(live, 1_000_000, 4); got != -1 {
		t.Fatalf("an oversized budget produced cut %d, want -1", got)
	}
}

func TestFindBudgetCutIndex_NeverStartsOnToolResult(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": strings.Repeat("x", 100)},
		{"role": "toolResult", "content": strings.Repeat("x", 100)},
		{"role": "toolResult", "content": strings.Repeat("x", 100)},
		{"role": "assistant", "content": strings.Repeat("x", 100)},
	}
	live := make([]EntryWithMessage, 0, len(messages))
	for i, message := range messages {
		live = append(live, EntryWithMessage{EntryID: "e" + jsNumberString(float64(i)), Message: message})
	}
	got := FindBudgetCutIndex(live, 1, 4)
	if got < 0 || messageRole(live[got].Message) == "toolResult" {
		t.Fatalf("the cut landed on a toolResult: %d", got)
	}
}

func TestApplyTailBudget_NoChangeWhenUnderBudget(t *testing.T) {
	entries := transcript(4)
	cut := BuildOwnCut(entries, 2, nil)
	got := ApplyTailBudget(entries, cut, ApplyTailBudgetOptions{MaxTokens: 1_000_000, CharsPerToken: 4})
	if got.BudgetCut != "" {
		t.Fatalf("a small tail was re-cut: %#v", got)
	}
	if got.FirstKeptEntryID != cut.FirstKeptEntryID {
		t.Fatalf("the cut changed: %q -> %q", cut.FirstKeptEntryID, got.FirstKeptEntryID)
	}
}

func TestApplyTailBudget_RescuesOversizedTail(t *testing.T) {
	// Long turns swell the tail past the budget.
	entries := make([]map[string]any, 0, 16)
	for i := 0; i < 8; i++ {
		n := jsNumberString(float64(i))
		entries = append(entries, messageEntry("u"+n, "user", strings.Repeat("q", 800)))
		entries = append(entries, messageEntry("a"+n, "assistant", strings.Repeat("r", 800)))
	}
	cut := BuildOwnCut(entries, 3, nil)
	if !cut.OK || cut.CompactAll {
		t.Fatalf("setup produced %#v", cut)
	}
	got := ApplyTailBudget(entries, cut, ApplyTailBudgetOptions{MaxTokens: 200, CharsPerToken: 4})
	if got.BudgetCut != BudgetCutOversizedTail {
		t.Fatalf("BudgetCut = %q, want the oversized-tail rescue: %#v", got.BudgetCut, got)
	}
	// The rescue moves the cut later (keeps less of the tail).
	if len(got.Messages) <= len(cut.Messages) {
		t.Fatalf("the rescue did not shrink the kept tail: %d -> %d", len(cut.Messages), len(got.Messages))
	}
}

func TestApplyTailBudget_NoAnchorRescue(t *testing.T) {
	// keep = total turns forces the compact-all fallback; with a token budget the
	// cut becomes a mid-turn budget cut instead.
	entries := make([]map[string]any, 0, 16)
	for i := 0; i < 8; i++ {
		n := jsNumberString(float64(i))
		entries = append(entries, messageEntry("u"+n, "user", strings.Repeat("q", 800)))
		entries = append(entries, messageEntry("a"+n, "assistant", strings.Repeat("r", 800)))
	}
	cut := BuildOwnCut(entries, 8, nil)
	if !cut.OK || !cut.CompactAll || !cut.KeepFallbackToCompactAll {
		t.Fatalf("setup produced %#v", cut)
	}
	got := ApplyTailBudget(entries, cut, ApplyTailBudgetOptions{MaxTokens: 400, CharsPerToken: 4})
	if got.BudgetCut != BudgetCutNoAnchor {
		t.Fatalf("BudgetCut = %q, want the no-anchor rescue: %#v", got.BudgetCut, got)
	}
	if got.CompactAll {
		t.Fatalf("the no-anchor rescue should keep part of the tail: %#v", got)
	}
}

func TestApplyTailBudget_ExplicitCompactAllUnchanged(t *testing.T) {
	entries := transcript(6)
	cut := BuildOwnCut(entries, 0, nil)
	got := ApplyTailBudget(entries, cut, ApplyTailBudgetOptions{MaxTokens: 1, CharsPerToken: 4})
	// keep:0 is the user's explicit choice: no rescue.
	if got.BudgetCut != "" {
		t.Fatalf("an explicit keep:0 was rescued: %#v", got)
	}
}

func TestApplyTailBudget_CancelledCutPassesThrough(t *testing.T) {
	cut := OwnCutResult{Reason: CancelNoLiveMessages}
	got := ApplyTailBudget(transcript(3), cut, ApplyTailBudgetOptions{})
	if got.OK || got.Reason != CancelNoLiveMessages {
		t.Fatalf("a cancelled cut was touched: %#v", got)
	}
}

// ── smart keep ──────────────────────────────────────────────────────────────

func TestResolveSmartKeepUserTurns_ExplicitKeepIsHonoured(t *testing.T) {
	got := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          transcript(6),
		RequestedKeepUserTurns: 2,
		Explicit:               true,
		SmartKeepTail:          true,
	})
	if got.KeepUserTurns != 2 || got.SmartAdjusted {
		t.Fatalf("an explicit keep was adjusted: %#v", got)
	}
}

func TestResolveSmartKeepUserTurns_SettingOff(t *testing.T) {
	got := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          transcript(6),
		RequestedKeepUserTurns: 1,
		SmartKeepTail:          false,
	})
	if got.KeepUserTurns != 1 || got.SmartAdjusted {
		t.Fatalf("smart-keep off still adjusted: %#v", got)
	}
}

func TestResolveSmartKeepUserTurns_DefaultKeepOne(t *testing.T) {
	// A negative request means "unspecified": the default is 1.
	got := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          transcript(3),
		RequestedKeepUserTurns: -1,
		SmartKeepTail:          false,
	})
	if got.KeepUserTurns != 1 {
		t.Fatalf("KeepUserTurns = %d, want the default 1", got.KeepUserTurns)
	}
}

func TestResolveSmartKeepUserTurns_BoostsASmallTail(t *testing.T) {
	// Six short turns: the keep:1 tail is tiny, so smart-keep grows it.
	got := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          transcript(6),
		RequestedKeepUserTurns: 1,
		SmartKeepTail:          true,
		CharsPerToken:          4,
	})
	if got.KeepUserTurns < 1 {
		t.Fatalf("KeepUserTurns = %d", got.KeepUserTurns)
	}
	if !got.SmartAdjusted && got.KeepUserTurns == 1 {
		t.Fatalf("a tiny tail was not boosted: %#v", got)
	}
	if got.FromKeep != 1 {
		t.Fatalf("FromKeep = %d, want the requested 1", got.FromKeep)
	}
}

func TestResolveSmartKeepUserTurns_StopsAtTheMaxTokens(t *testing.T) {
	// With a very small max, no boost fits, so the base keep stands.
	got := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          transcript(4),
		RequestedKeepUserTurns: 1,
		SmartKeepTail:          true,
		MinTokens:              1,
		MaxTokens:              1,
		CharsPerToken:          4,
	})
	if got.KeepUserTurns != 1 || got.SmartAdjusted {
		t.Fatalf("no boost should fit: %#v", got)
	}
}

func TestResolveSmartKeepUserTurns_NoBoostWhenBaseIsLarge(t *testing.T) {
	// The boost only engages when the keep:1 tail is under MinSmartTailTokens
	// (5000). Each turn here carries ~20000 chars ≈ 5000 tokens, so the base
	// tail is already large and nothing is boosted.
	entries := make([]map[string]any, 0, 20)
	for i := 0; i < 10; i++ {
		n := jsNumberString(float64(i))
		entries = append(entries, messageEntry("u"+n, "user", strings.Repeat("q", 20000)))
		entries = append(entries, messageEntry("a"+n, "assistant", strings.Repeat("r", 20000)))
	}
	got := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          entries,
		RequestedKeepUserTurns: 1,
		SmartKeepTail:          true,
		CharsPerToken:          4,
	})
	if got.KeepUserTurns != 1 || got.SmartAdjusted {
		t.Fatalf("a large base tail was boosted: %#v", got)
	}
}

// ── reason messages / debug ─────────────────────────────────────────────────

func TestReasonMessage(t *testing.T) {
	if got := ReasonMessage(CancelNoLiveMessages); !strings.Contains(got, "Nothing to compact") {
		t.Fatalf("ReasonMessage(no live) = %q", got)
	}
	if got := ReasonMessage(CancelTooFewLiveMessages); !strings.Contains(got, "Too few messages") {
		t.Fatalf("ReasonMessage(too few) = %q", got)
	}
}

func TestWriteDebugSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/debug.json"

	// Disabled: nothing is written.
	WriteDebugSnapshot(path, false, map[string]any{"a": 1})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a disabled snapshot wrote a file")
	}

	// Enabled: the JSON lands on disk.
	WriteDebugSnapshot(path, true, map[string]any{"a": 1})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("enabled snapshot not written: %v", err)
	}
	if !strings.Contains(string(data), `"a": 1`) {
		t.Fatalf("snapshot content = %s", data)
	}

	// An unwritable path must not panic.
	WriteDebugSnapshot(dir+"/missing-dir/debug.json", true, map[string]any{"a": 1})
}

func TestPreviewContent(t *testing.T) {
	short := previewContent("hello")
	if short != "hello" {
		t.Fatalf("previewContent(string) = %q", short)
	}
	long := previewContent(strings.Repeat("x", 400))
	if jsLen(long) != 300 {
		t.Fatalf("previewContent clipped to %d units, want 300", jsLen(long))
	}
	parts := previewContent([]any{
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "toolCall", "name": "bash"},
		map[string]any{"type": "thinking", "thinking": "hmm"},
		map[string]any{"type": "image", "mimeType": "image/png"},
		map[string]any{"type": "mystery"},
	})
	for _, want := range []string{"hi", "[toolCall:bash]", "[thinking]", "[image:image/png]", "[mystery]"} {
		if !strings.Contains(parts, want) {
			t.Fatalf("previewContent lost %q: %q", want, parts)
		}
	}
	if got := previewContent(42); got != "" {
		t.Fatalf("previewContent(non-content) = %q", got)
	}
}
