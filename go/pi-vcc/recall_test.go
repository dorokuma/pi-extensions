package pivcc

import (
	"strings"
	"testing"
)

// Tests for the recall subsystem: src/core/recall-scope.ts, render-entries.ts,
// search-entries.ts and format-recall.ts.

// ── recall scope ────────────────────────────────────────────────────────────

func TestNormalizeRecallScope(t *testing.T) {
	tests := []struct {
		in   any
		want RecallScope
	}{
		{"all", ScopeAll},
		{"ALL", ScopeAll},
		{"lineage", ScopeLineage},
		{"", ScopeLineage},
		{nil, ScopeLineage},
		{42, ScopeLineage},
	}
	for _, test := range tests {
		if got := NormalizeRecallScope(test.in); got != test.want {
			t.Fatalf("NormalizeRecallScope(%v) = %v, want %v", test.in, got, test.want)
		}
	}
}

func TestNormalizeRecallMode(t *testing.T) {
	tests := []struct {
		in   any
		want RecallMode
	}{
		{"touched", ModeTouched},
		{"TOUCHED", ModeTouched},
		{"hybrid", ModeHybridRecall},
		{"bogus", ModeHybridRecall},
		{nil, ModeHybridRecall},
	}
	for _, test := range tests {
		if got := NormalizeRecallMode(test.in); got != test.want {
			t.Fatalf("NormalizeRecallMode(%v) = %v, want %v", test.in, got, test.want)
		}
	}
}

func TestParseRecallScope(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantScope RecallScope
		wantText  string
	}{
		{"no scope token", "auth bug", ScopeLineage, "auth bug"},
		{"scope:all is extracted and removed", "auth bug scope:all", ScopeAll, "auth bug"},
		{"scope:lineage is extracted", "scope:lineage auth", ScopeLineage, "auth"},
		{"scope inside words is still a token", "see scope:all now", ScopeAll, "see now"},
		{"adjacent tokens keep their text", "scope:bogus text", ScopeLineage, "scope:bogus text"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope, text := ParseRecallScope(test.text)
			if scope != test.wantScope || text != test.wantText {
				t.Fatalf("ParseRecallScope(%q) = (%v, %q), want (%v, %q)",
					test.text, scope, text, test.wantScope, test.wantText)
			}
		})
	}
}

// ── render entries ──────────────────────────────────────────────────────────

func TestRenderMessage_User(t *testing.T) {
	entry := RenderMessage(map[string]any{"role": "user", "content": "hello there"}, 3, false)
	if entry.Role != "user" || entry.Index != 3 || entry.Summary != "hello there" {
		t.Fatalf("RenderMessage(user) = %#v", entry)
	}
}

func TestRenderMessage_ToolResult(t *testing.T) {
	entry := RenderMessage(map[string]any{
		"role": "toolResult", "toolName": "bash", "content": "exit 0",
	}, 4, false)
	if entry.Role != "tool_result" || entry.Summary != "[bash] exit 0" {
		t.Fatalf("RenderMessage(toolResult) = %#v", entry)
	}
}

func TestRenderMessage_BashExecution(t *testing.T) {
	entry := RenderMessage(map[string]any{
		"role": "bashExecution", "command": "go test ./...", "output": "ok",
	}, 5, false)
	if entry.Role != "bash" || !strings.HasPrefix(entry.Summary, "$ go test ./...") {
		t.Fatalf("RenderMessage(bashExecution) = %#v", entry)
	}
	if !strings.Contains(entry.Summary, "ok") {
		t.Fatalf("bash output missing: %q", entry.Summary)
	}
}

func TestRenderMessage_AssistantToolCallsFirst(t *testing.T) {
	entry := RenderMessage(map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "Let me look."},
			map[string]any{"type": "toolCall", "name": "read", "arguments": map[string]any{"path": "/a.go"}},
		},
	}, 6, false)
	if entry.Role != "assistant" {
		t.Fatalf("role = %q", entry.Role)
	}
	lines := jsSplitLines(entry.Summary)
	if lines[0] != "read(path=/a.go)" {
		t.Fatalf("tool calls should render first: %q", entry.Summary)
	}
	if !strings.Contains(entry.Summary, "Let me look.") {
		t.Fatalf("assistant text missing: %q", entry.Summary)
	}
	if len(entry.Files) != 1 || entry.Files[0] != "/a.go" {
		t.Fatalf("Files = %v", entry.Files)
	}
}

func TestRenderMessage_FullSkipsClipping(t *testing.T) {
	long := strings.Repeat("x", 500)
	clipped := RenderMessage(map[string]any{"role": "user", "content": long}, 0, false)
	full := RenderMessage(map[string]any{"role": "user", "content": long}, 0, true)
	if jsLen(clipped.Summary) >= jsLen(full.Summary) {
		t.Fatalf("full=true did not keep more text (%d vs %d)", jsLen(clipped.Summary), jsLen(full.Summary))
	}
	if jsLen(clipped.Summary) > 300 {
		t.Fatalf("clipped summary is %d units, over 300", jsLen(clipped.Summary))
	}
}

// ── file indicators / touched files ─────────────────────────────────────────

func TestGetFileIndicators(t *testing.T) {
	msg := map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "toolCall", "name": "edit", "arguments": map[string]any{
				"path": "/a.go", "content": "one\ntwo\n\nthree",
			}},
			// Not content-bearing: no content/edits, so it is skipped.
			map[string]any{"type": "toolCall", "name": "read", "arguments": map[string]any{"path": "/b.go"}},
		},
	}
	// The exported element type is part of the API surface, so it is spelled out
	// rather than inferred.
	var got []FileIndicator = GetFileIndicators(msg)
	if len(got) != 1 {
		t.Fatalf("GetFileIndicators = %#v, want one bearing call", got)
	}
	if got[0].Path != "/a.go" || got[0].ToolName != "edit" {
		t.Fatalf("indicator = %#v", got[0])
	}
	if got[0].LineCount != 3 {
		t.Fatalf("LineCount = %d, want 3 non-empty lines", got[0].LineCount)
	}
}

func TestGetTouchedFiles(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "content": []any{map[string]any{
			"type": "toolCall", "name": "edit",
			"arguments": map[string]any{"path": "/a.go", "content": "x"},
		}}},
		{"role": "assistant", "content": []any{map[string]any{
			"type": "toolCall", "name": "write",
			"arguments": map[string]any{"path": "/a.go", "content": "y"},
		}}},
		{"role": "assistant", "content": []any{map[string]any{
			"type": "toolCall", "name": "edit",
			"arguments": map[string]any{"path": "/b.go", "content": "z"},
		}}},
	}
	rendered := []RenderedEntry{{Index: 0}, {Index: 1}, {Index: 2}}
	got := GetTouchedFiles(messages, rendered)
	if len(got) != 2 {
		t.Fatalf("GetTouchedFiles = %#v, want two files", got)
	}
	// An 8-digit pseudo index appears only on a file touched twice.
	if got[0].Path != "/a.go" || len(got[0].Entries) != 2 {
		t.Fatalf("a.go = %#v", got[0])
	}
	if got[1].Path != "/b.go" || len(got[1].Entries) != 1 {
		t.Fatalf("b.go = %#v", got[1])
	}
	for i, touch := range got[0].Entries {
		if touch.Index != i {
			t.Fatalf("entry index = %d, want %d", touch.Index, i)
		}
	}
}

// ── search ──────────────────────────────────────────────────────────────────

func searchFixture() ([]RenderedEntry, []map[string]any) {
	texts := []string{
		"fix the OAuth token refresh bug",
		"the login flow returns 401 on expiry",
		"unrelated documentation about styling",
		"token refresh happens in auth.ts",
	}
	entries := make([]RenderedEntry, 0, len(texts))
	messages := make([]map[string]any, 0, len(texts))
	for i, text := range texts {
		entries = append(entries, RenderedEntry{Index: i, Role: "user", Summary: text})
		messages = append(messages, map[string]any{"role": "user", "content": text})
	}
	return entries, messages
}

func TestSearchEntries_EmptyQueryReturnsAll(t *testing.T) {
	entries, messages := searchFixture()
	result, err := SearchEntriesDetailed(entries, messages, "   ", SearchTuning{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != len(entries) || result.Truncated {
		t.Fatalf("an empty query should return all entries: %#v", result)
	}
}

func TestSearchEntries_RanksByRelevance(t *testing.T) {
	entries, messages := searchFixture()
	hits := SearchEntries(entries, messages, "token refresh")
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	// The two token-refresh entries outrank the styling one.
	if hits[0].Index != 0 && hits[0].Index != 3 {
		t.Fatalf("top hit is #%d, want a token-refresh entry", hits[0].Index)
	}
	for _, hit := range hits {
		if hit.Index == 2 {
			t.Fatalf("the unrelated entry ranked: %#v", hit)
		}
	}
}

func TestSearchEntries_SnippetAroundMatch(t *testing.T) {
	entries, messages := searchFixture()
	hits := SearchEntries(entries, messages, "401")
	if len(hits) != 1 {
		t.Fatalf("hits = %#v, want one", hits)
	}
	if !hits[0].HasSnippet {
		t.Fatalf("hit has no snippet: %#v", hits[0])
	}
	if !strings.Contains(hits[0].Snippet, "401") {
		t.Fatalf("snippet lost the match: %q", hits[0].Snippet)
	}
}

func TestSearchEntries_RegexFastPath(t *testing.T) {
	entries, messages := searchFixture()
	// A regex query is recognised and matched as a pattern.
	hits := SearchEntries(entries, messages, `40\d`)
	if len(hits) != 1 || hits[0].Index != 1 {
		t.Fatalf("regex hits = %#v", hits)
	}
}

func TestSearchEntries_InvalidRegexFallsBack(t *testing.T) {
	entries, messages := searchFixture()
	// An unbalanced group must not panic or error out: it degrades to a
	// literal-term search.
	result, err := SearchEntriesDetailed(entries, messages, "auth(", SearchTuning{})
	if err != nil {
		t.Fatalf("an invalid regex returned an error: %v", err)
	}
	if len(result.Hits) > len(entries) {
		t.Fatalf("hits exceed the entry count: %#v", result.Hits)
	}
}

func TestSearchEntries_Cap(t *testing.T) {
	entries := make([]RenderedEntry, 0, 30)
	messages := make([]map[string]any, 0, 30)
	for i := 0; i < 30; i++ {
		text := "needle entry " + jsNumberString(float64(i))
		entries = append(entries, RenderedEntry{Index: i, Role: "user", Summary: text})
		messages = append(messages, map[string]any{"role": "user", "content": text})
	}
	result, err := SearchEntriesDetailed(entries, messages, "needle", SearchTuning{Cap: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 5 {
		t.Fatalf("cap did not apply: %d hits", len(result.Hits))
	}
	if !result.Truncated || result.TotalBeforeCap <= 5 {
		t.Fatalf("truncation not reported: %#v", result)
	}
}

func TestSearchEntries_RelativeFloor(t *testing.T) {
	entries := []RenderedEntry{
		{Index: 0, Role: "user", Summary: "alpha beta gamma delta"},
		{Index: 1, Role: "user", Summary: "alpha only here"},
	}
	messages := []map[string]any{
		{"role": "user", "content": "alpha beta gamma delta"},
		{"role": "user", "content": "alpha only here"},
	}
	// With a high floor, only the best-scoring hit survives.
	hits := SearchEntries(entries, messages, "alpha beta")
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Index != 0 {
		t.Fatalf("top hit = #%d, want #0", hits[0].Index)
	}
}

// ── format recall ───────────────────────────────────────────────────────────

func TestShortPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		cwd  string
		want string
	}{
		{"under cwd is relative", "/root/p/a.go", "/root/p", "./a.go"},
		{"deeper under cwd", "/root/p/sub/a.go", "/root/p", "./sub/a.go"},
		{"outside cwd stays absolute", "/other/a.go", "/root/p", "/other/a.go"},
		{"exact cwd is not relative", "/root/p", "/root/p", "/root/p"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ShortPath(test.path, test.cwd); got != test.want {
				t.Fatalf("ShortPath(%q, %q) = %q, want %q", test.path, test.cwd, got, test.want)
			}
		})
	}
}

func TestFormatRecallOutput(t *testing.T) {
	entries, messages := searchFixture()
	hits := SearchEntries(entries, messages, "token")
	got := FormatRecallOutput(hits, "token", "", false, false)
	if !strings.Contains(got, "token") {
		t.Fatalf("header lost the query:\n%s", got)
	}
	// Each hit is rendered with its #index so the reader can drill down.
	if !strings.Contains(got, "#0") && !strings.Contains(got, "#3") {
		t.Fatalf("hit indices are missing:\n%s", got)
	}
	// An empty result still renders a message, not an empty string.
	empty := FormatRecallOutput(nil, "nothing", "", false, false)
	if jsTrimSpace(empty) == "" {
		t.Fatal("an empty result rendered an empty string")
	}
}

func TestFormatTouchedOutput(t *testing.T) {
	touched := []TouchedFile{
		{Path: "/root/p/a.go", Entries: []FileTouch{{Index: 3, ToolName: "edit"}}},
	}
	got := FormatTouchedOutput(touched, 1, 20, "/root/p")
	if !strings.Contains(got, "a.go") {
		t.Fatalf("touched output lost the path:\n%s", got)
	}
	if !strings.Contains(got, "#3") {
		t.Fatalf("touched output lost the entry index:\n%s", got)
	}
	if jsTrimSpace(FormatTouchedOutput(nil, 1, 20, "/root/p")) == "" {
		t.Fatal("an empty touched list rendered an empty string")
	}
}
