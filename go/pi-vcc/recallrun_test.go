package pivcc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tests for recall-run.ts's RecallView and runRecall(): the view a recall call
// sees (scope, caching, current turn) and every action it can run.

// fakeManager is a LineageSessionManager over a fixed branch and log.
type fakeManager struct {
	branch []LineageEntryLike
	all    []LineageEntryLike
}

func (m fakeManager) GetBranch() ([]LineageEntryLike, error)  { return m.branch, nil }
func (m fakeManager) GetEntries() ([]LineageEntryLike, error) { return m.all, nil }

// recallFixtureFile writes a two-turn session and returns its path plus the
// entries a lineage branch would hold.
func recallFixtureFile(t *testing.T) (string, []LineageEntryLike) {
	t.Helper()
	lines := []string{
		`{"type":"message","id":"e1","parentId":"","message":{"role":"user","content":"fix the OAuth token refresh bug in auth.ts"}}`,
		`{"type":"message","id":"e2","parentId":"e1","message":{"role":"assistant","content":[{"type":"text","text":"I will look at the auth flow and the token refresh path."}]}}`,
		`{"type":"message","id":"e3","parentId":"e2","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"edit","arguments":{"path":"/root/workspace/pi-vcc/format.go","content":"package pivcc"}}]}}`,
		`{"type":"message","id":"e4","parentId":"e3","message":{"role":"toolResult","toolName":"edit","toolCallId":"call-1","content":"wrote 1 line"}}`,
		`{"type":"message","id":"e5","parentId":"e4","message":{"role":"user","content":"now add the regression test for the refresh bug"}}`,
		`{"type":"message","id":"e6","parentId":"e5","message":{"role":"assistant","content":[{"type":"text","text":"Adding it now."}]}}`,
	}
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	entries := make([]LineageEntryLike, 0, len(lines))
	for i, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("decode fixture line %d: %v", i, err)
		}
		entries = append(entries, LineageEntryLike(decoded))
	}
	return sessionFile, entries
}

// strconvQuote quotes text for embedding in a JSON line.
func strconvQuote(text string) string { return strconv.Quote(text) }

// writeSessionFile writes the given session JSONL lines to a temp file and
// returns its path plus the entries a lineage branch would hold.
func writeSessionFile(t *testing.T, lines ...string) (string, []LineageEntryLike) {
	t.Helper()
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	entries := make([]LineageEntryLike, 0, len(lines))
	for i, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("decode fixture line %d: %v", i, err)
		}
		entries = append(entries, LineageEntryLike(decoded))
	}
	return sessionFile, entries
}

// ── the view ────────────────────────────────────────────────────────────────

func TestRecallView_LoadCaches(t *testing.T) {
	dir := t.TempDir()
	// A message longer than the partial clip, so the two renders differ.
	long := strings.Repeat("the token refresh bug reproduces on expiry ", 20)
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"type":"message","id":"e1","message":{"role":"user","content":`+
		strconvQuote(long)+"}}\n"), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	view := OpenRecallView(sessionFile, ScopeAll, nil)
	first := view.Load(false)
	if len(first.Rendered) != 1 {
		t.Fatalf("loaded %d entries, want 1", len(first.Rendered))
	}
	if view.Load(false) != first {
		t.Fatal("the partial load was not cached")
	}
	full := view.Load(true)
	if full != view.Load(true) {
		t.Fatal("the full load was not cached")
	}
	if full == first {
		t.Fatal("the full load shares the partial cache")
	}
	// The full rendering keeps more text than the clipped one.
	if jsLen(full.Rendered[0].Summary) <= jsLen(first.Rendered[0].Summary) {
		t.Fatal("the full render is not longer than the clipped one")
	}
	if !strings.Contains(first.Rendered[0].Summary, long[:100]) {
		t.Fatal("the clipped render lost the message start")
	}
}

func TestRecallView_ScopeFiltersOffPathEntries(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	// The branch holds only the first four entries: the last two are on an
	// edited branch the lineage scope must not see.
	view := OpenRecallView(sessionFile, ScopeLineage, fakeManager{branch: entries[:4]})
	if got := len(view.Load(false).Rendered); got != 4 {
		t.Fatalf("lineage scope loaded %d entries, want 4", got)
	}
	// scope:all sees the whole file.
	wide := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries[:4]})
	if got := len(wide.Load(false).Rendered); got != 6 {
		t.Fatalf("scope:all loaded %d entries, want 6", got)
	}
}

func TestRecallView_WidenReusesOneWideView(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeLineage, fakeManager{branch: entries[:4]})
	wide := view.Widen()
	if wide == view {
		t.Fatal("widen returned the same view")
	}
	if wide.Scope != ScopeAll {
		t.Fatalf("wide scope = %v", wide.Scope)
	}
	if view.Widen() != wide {
		t.Fatal("widen built a second wide view")
	}
	// A view that is already scope:all widens to itself.
	if wide.Widen() != wide {
		t.Fatal("scope:all widened to another view")
	}
}

func TestRecallView_CurrentTurn(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeLineage, fakeManager{branch: entries})
	turn := view.CurrentTurn()
	// The turn in progress starts at the last user message (#4).
	if !turn[4] || !turn[5] || turn[3] {
		t.Fatalf("current turn = %v, want {4,5}", turn)
	}
	again := view.CurrentTurn()
	if len(again) != len(turn) || !again[4] || !again[5] || again[3] {
		t.Fatal("the turn was recomputed")
	}

	// A session whose last entry is a compaction has no turn in progress.
	sessionFile, _ = recallFixtureFile(t)
	compacted := append([]LineageEntryLike(nil), entries...)
	compacted = append(compacted, LineageEntryLike{"id": "c1", "type": "compaction"})
	empty := OpenRecallView(sessionFile, ScopeLineage, fakeManager{branch: compacted}).CurrentTurn()
	if len(empty) != 0 {
		t.Fatalf("turn = %v, want none after a compaction", empty)
	}

	// No session manager at all: no turn, no panic.
	if got := OpenRecallView(sessionFile, ScopeLineage, nil).CurrentTurn(); len(got) != 0 {
		t.Fatalf("turn = %v, want none without a manager", got)
	}
}

// ── the actions ─────────────────────────────────────────────────────────────

// recallHints is the command's hint set, which has no AroundHit.
var recallHints = RecallPagingHints{
	OutOfRange: func(query string, scope RecallScope, totalPages int, truncated bool) string {
		return "out of range " + query
	},
	NextPage: func(query string, scope RecallScope, page int) string { return "next page" },
}

func TestRunRecall_IgnoredNoteNamesUnusedParams(t *testing.T) {
	request := RecallRequest{
		Action:  RecallAction{Kind: ActionRecent, Query: ""},
		Dropped: []string{"page"},
		Ignored: []string{"query"},
	}
	note := IgnoredNote(request)
	if !strings.Contains(note, "Ignored: page.") {
		t.Fatalf("note = %q, want the dropped params", note)
	}
	if !strings.Contains(note, "Ignored: query (page applies to query or range results).") {
		t.Fatalf("note = %q, want the ignored reason", note)
	}
	if IgnoredNote(RecallRequest{Action: RecallAction{Kind: ActionSearch}}) != "" {
		t.Fatal("a clean request carries a note")
	}
}

func TestRunRecall_Search(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionSearch, Query: "token refresh", Page: 1},
	}, view, recallHints)
	if !strings.Contains(out, "token refresh") {
		t.Fatalf("the search lost the query:\n%s", out)
	}
	// The first turn's user message is a hit.
	if !strings.Contains(out, "#0") {
		t.Fatalf("the search lost the hit index:\n%s", out)
	}
	if strings.Contains(out, "Ignored:") {
		t.Fatalf("a clean search carries a note:\n%s", out)
	}
}

func TestRunRecall_SearchPagingOutOfRange(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionSearch, Query: "token refresh", Page: 9},
	}, view, recallHints)
	if !strings.Contains(out, "Page 9 is outside the available range") {
		t.Fatalf("the out-of-range answer is missing:\n%s", out)
	}
	if !strings.Contains(out, "out of range token refresh") {
		t.Fatalf("the paging hint is missing:\n%s", out)
	}
}

func TestRunRecall_Range(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionRange, Range: []int{1, 3}, Page: 1},
	}, view, recallHints)
	if !strings.Contains(out, "Range #1..#3") {
		t.Fatalf("the range header is missing:\n%s", out)
	}
	for _, want := range []string{"#1", "#2", "#3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%s is missing from the range:\n%s", want, out)
		}
	}

	// A malformed range is reported, not applied.
	bad := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionRange, Range: []int{3, 1}, Page: 1},
	}, view, recallHints)
	if !strings.Contains(bad, "Invalid range") {
		t.Fatalf("an inverted range was accepted:\n%s", bad)
	}

	// A range past the end is clamped and says so.
	clamped := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionRange, Range: []int{4, 99}, Page: 1},
	}, view, recallHints)
	if !strings.Contains(clamped, "#5 is the last entry") {
		t.Fatalf("the clamped range does not say so:\n%s", clamped)
	}

	// A range outside history entirely reports the last entry.
	none := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionRange, Range: []int{40, 50}, Page: 1},
	}, view, recallHints)
	if !strings.Contains(none, "No messages #40..#50") {
		t.Fatalf("an empty range is not reported:\n%s", none)
	}
}

func TestRunRecall_Expand(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionExpand, Indices: []int{0, 2}},
	}, view, recallHints)
	if !strings.Contains(out, "Expanded #0, #2") {
		t.Fatalf("the expand header is missing:\n%s", out)
	}
	// The expanded tool call's result is paired with it, clipped to
	// PairedResultChars at most.
	if !strings.Contains(out, "#3") {
		t.Fatalf("the paired tool result is missing:\n%s", out)
	}
	// An index outside history is reported instead of expanded.
	bad := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionExpand, Indices: []int{0, 99}},
	}, view, recallHints)
	if !strings.Contains(bad, "Cannot expand indices outside session history: 99") {
		t.Fatalf("an invalid index was accepted:\n%s", bad)
	}
}

func TestRunRecall_ExpandOffPathNotesTheBranch(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	// The branch holds only the first four entries; #5 is off-path but
	// reachable through scope:all's widen.
	view := OpenRecallView(sessionFile, ScopeLineage, fakeManager{branch: entries[:4]})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionExpand, Indices: []int{5}},
	}, view, recallHints)
	if !strings.Contains(out, "#5 is not on the current conversation path") {
		t.Fatalf("the off-path note is missing:\n%s", out)
	}
}

func TestRunRecall_Touched(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionTouched, Page: 1},
	}, view, recallHints)
	if !strings.Contains(out, "format.go") {
		t.Fatalf("the touched file is missing:\n%s", out)
	}
	if !strings.Contains(out, "#2") {
		t.Fatalf("the entry index is missing:\n%s", out)
	}
	// A write gets a ready #N:path drill-down.
	if !strings.Contains(out, "query:'#2:format.go'") {
		t.Fatalf("the drill-down hint is missing:\n%s", out)
	}
}

// TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit pins the 0.11.1
// `drillable: false` guard of runTouched(): a multi-path edit names its files in
// args.paths but carries no per-file content, so those files are listed without
// a drill-down hint, and the hint points at the file that was really written.
func TestRunRecall_TouchedSkipsNonDrillableMultiPathEdit(t *testing.T) {
	multiPathEdit := `{"type":"message","id":"e1","parentId":"","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"edit","arguments":{"code":"rewrite both files","paths":["/root/p/a.go","/root/p/b.go"]}}]}}`
	// A multi-path edit alone: both files are listed, neither has content to
	// drill into, so no "File content at an entry" note is appended.
	sessionFile, entries := writeSessionFile(t, multiPathEdit)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionTouched, Page: 1},
	}, view, recallHints)
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Fatalf("the multi-path edit's files are missing:\n%s", out)
	}
	if !strings.Contains(out, "#0 (edit)") {
		t.Fatalf("the multi-path edit's entry is missing:\n%s", out)
	}
	if strings.Contains(out, "--- File content at an entry: query:") {
		t.Fatalf("a non-drillable edit got a drill-down hint:\n%s", out)
	}

	// The same multi-path edit plus a single-file write of b.go: the note skips
	// a.go (its only entry is the non-drillable edit) and points at b.go's
	// write entry.
	bWrite := `{"type":"message","id":"e2","parentId":"e1","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-2","name":"write","arguments":{"path":"/root/p/b.go","content":"package p\n"}}]}}`
	sessionFile, entries = writeSessionFile(t, multiPathEdit, bWrite)
	view = OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out = RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionTouched, Page: 1},
	}, view, recallHints)
	if !strings.Contains(out, "#1 (write)") {
		t.Fatalf("the write entry is missing:\n%s", out)
	}
	if !strings.Contains(out, "query:'#1:b.go'") {
		t.Fatalf("the drill-down hint does not point at the write:\n%s", out)
	}
	if strings.Contains(out, "a.go'") {
		t.Fatalf("the hint points at the non-drillable edit:\n%s", out)
	}
}

func TestRunRecall_Recent(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	view := OpenRecallView(sessionFile, ScopeAll, fakeManager{branch: entries})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionRecent, Query: ""},
	}, view, recallHints)
	if !strings.Contains(out, "#5") {
		t.Fatalf("the recent entries are missing:\n%s", out)
	}
	if !strings.Contains(out, "Scope: all") {
		t.Fatalf("the scope prefix is missing:\n%s", out)
	}
}

func TestRunRecall_DrillDownOffPathNote(t *testing.T) {
	sessionFile, entries := recallFixtureFile(t)
	// The branch holds the first four entries; #4 is off-path for the lineage
	// scope, so the answer carries the off-path note first.
	view := OpenRecallView(sessionFile, ScopeLineage, fakeManager{branch: entries[:4]})
	out := RunRecall(RecallRequest{
		Action: RecallAction{Kind: ActionDrill, Drill: &DrillTarget{Index: 4, PathPattern: "session.jsonl"}},
	}, view, recallHints)
	if !strings.HasPrefix(out, offPathNote) {
		t.Fatalf("the off-path note is missing:\n%s", out)
	}
}

// ── helpers the port shares with the harness ────────────────────────────────

func TestInvalidExpandIndices(t *testing.T) {
	available := map[int]bool{0: true, 3: true}
	got := InvalidExpandIndices([]int{0, 1, 3, 7}, available)
	if len(got) != 2 || got[0] != 1 || got[1] != 7 {
		t.Fatalf("invalid indices = %v, want [1 7]", got)
	}
	if len(InvalidExpandIndices(nil, available)) != 0 {
		t.Fatal("no requested indices produced invalid ones")
	}
}

func TestToolCallIDs(t *testing.T) {
	message := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "toolCall", "id": "call-1", "name": "edit"},
		map[string]any{"type": "toolCall", "id": "call-2", "name": "read"},
	}}
	ids := toolCallIDs(message)
	if len(ids) != 2 || ids[0] != "call-1" || ids[1] != "call-2" {
		t.Fatalf("toolCallIds = %v", ids)
	}
	// Only an assistant message carries calls.
	if len(toolCallIDs(map[string]any{"role": "user", "content": "hi"})) != 0 {
		t.Fatal("a user message reported tool call ids")
	}
	if len(toolCallIDs(map[string]any{"role": "assistant", "content": "plain text"})) != 0 {
		t.Fatal("a text-only assistant message reported tool call ids")
	}
}
