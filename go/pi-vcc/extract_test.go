package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/extract/* and src/core/build-sections.ts: the extractors that
// turn a normalized transcript into SectionData.

// ── goals ───────────────────────────────────────────────────────────────────

func TestExtractGoals_FirstUserBullets(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("- port sanitize.ts to Go\n- keep behavior identical", 0, true),
		userBlock("ok", 1, true),
	}
	got := ExtractGoals(blocks)
	if !containsStr(got, "port sanitize.ts to Go") || !containsStr(got, "keep behavior identical") {
		t.Fatalf("ExtractGoals = %v", got)
	}
}

func TestExtractGoals_FiltersNoise(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("ok", 0, true),
		userBlock("thanks!", 1, true),
		userBlock("[error] something failed", 2, true),
		userBlock("/root/some/path.txt", 3, true),
	}
	if got := ExtractGoals(blocks); len(got) != 0 {
		t.Fatalf("noise was extracted as goals: %v", got)
	}
}

func TestExtractGoals_ScopeChangeAppended(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("port the extractor to Go", 0, true),
		userBlock("Actually, instead rewrite the parser first", 1, true),
	}
	got := ExtractGoals(blocks)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "[Scope change]") {
		t.Fatalf("the scope change was not marked: %v", got)
	}
	if !strings.Contains(joined, "rewrite the parser first") {
		t.Fatalf("the scope-change bullet was lost: %v", got)
	}
	// The original goal is still there, ahead of the marker.
	if !strings.Contains(joined, "port the extractor to Go") {
		t.Fatalf("the original goal was dropped: %v", got)
	}
	if strings.Index(joined, "port the extractor") > strings.Index(joined, "[Scope change]") {
		t.Fatalf("the scope change was placed before the original goal: %v", got)
	}
}

func TestExtractGoals_CapsAtEightLines(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock(strings.Repeat("build the thing line\n", 12), 0, true),
	}
	if got := ExtractGoals(blocks); len(got) > 8 {
		t.Fatalf("ExtractGoals returned %d lines, over the cap of 8: %v", len(got), got)
	}
}

func TestExtractGoals_StopsAtTemplateBoilerplate(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("fix the login bug\nFor each file, list the exports.\nOutput:\n- noop", 0, true),
	}
	got := ExtractGoals(blocks)
	if len(got) != 1 || got[0] != "fix the login bug" {
		t.Fatalf("ExtractGoals = %v, want just the real goal", got)
	}
}

// ── preferences ─────────────────────────────────────────────────────────────

func TestExtractPreferences(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("always use tabs for indentation", 0, true),
		userBlock("don't want emojis in output", 1, true),
		userBlock("what is this?", 2, true), // question: rejected
		userBlock("just run it", 3, true),   // matches no pattern
	}
	got := ExtractPreferences(blocks)
	if !containsStr(got, "always use tabs for indentation") {
		t.Fatalf("ExtractPreferences = %v", got)
	}
	if !containsStr(got, "don't want emojis in output") {
		t.Fatalf("ExtractPreferences lost the don't-want pattern: %v", got)
	}
	for _, item := range got {
		if strings.Contains(item, "?") || item == "just run it" {
			t.Fatalf("a non-preference was extracted: %q", item)
		}
	}
}

func TestExtractPreferences_OnePerBlock(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("always use tabs\nnever use spaces\nprefer gofmt", 0, true),
	}
	if got := ExtractPreferences(blocks); len(got) != 1 {
		t.Fatalf("ExtractPreferences returned %d prefs for one block, want 1: %v", len(got), got)
	}
}

func TestDedupPreferencesAgainstGoals(t *testing.T) {
	prefs := []string{"always use tabs", "unique preference"}
	goals := []string{"Always use tabs"}
	got := DedupPreferencesAgainstGoals(prefs, goals)
	if len(got) != 1 || got[0] != "unique preference" {
		t.Fatalf("DedupPreferencesAgainstGoals = %v", got)
	}
}

// ── outstanding context ─────────────────────────────────────────────────────

func TestExtractOutstandingContext(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindAssistant, Text: "Some preamble.\nThe tests are still failing after the refactor."},
		userBlock("The build is broken on arm64", 0, true),
		{Kind: KindAssistant, Text: "- still broken\nThis line is fine."},
	}
	got := extractOutstandingContext(blocks)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "The tests are still failing after the refactor.") {
		t.Fatalf("an assistant blocker was lost: %v", got)
	}
	// A user blocker is labelled so the section reader knows the source.
	if !strings.Contains(joined, "[user] The build is broken on arm64") {
		t.Fatalf("a user blocker was not labelled: %v", got)
	}
	// The bullet continuation fragment is skipped.
	if strings.Contains(joined, "still broken") {
		t.Fatalf("a bullet fragment was treated as a blocker: %v", got)
	}
}

func TestExtractOutstandingContext_CapsAtFive(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 8)
	for i := 0; i < 8; i++ {
		blocks = append(blocks, NormalizedBlock{
			Kind: KindAssistant,
			Text: "Failure number " + jsNumberString(float64(i)) + " happened in the pipeline",
		})
	}
	if got := extractOutstandingContext(blocks); len(got) != 5 {
		t.Fatalf("extractOutstandingContext returned %d items, want 5", len(got))
	}
}

// ── files ───────────────────────────────────────────────────────────────────

func TestExtractFiles(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/root/p/a.go"}},
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/root/p/b.go"}},
		{Kind: KindToolCall, Name: "write", Args: map[string]any{"path": "/root/p/c.go"}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "ls"}},
		// Case-insensitive tool matching.
		{Kind: KindToolCall, Name: "Read", Args: map[string]any{"file_path": "/root/p/d.go"}},
	}
	activity := ExtractFiles(blocks, nil)
	if !activity.Read.Has("a.go") || !activity.Read.Has("d.go") {
		t.Fatalf("Read = %v", activity.Read.Items())
	}
	if !activity.Modified.Has("b.go") {
		t.Fatalf("Modified = %v", activity.Modified.Items())
	}
	// write is both a write and a create tool.
	if !activity.Created.Has("c.go") || !activity.Modified.Has("c.go") {
		t.Fatalf("Created = %v Modified = %v", activity.Created.Items(), activity.Modified.Items())
	}
}

func TestExtractFiles_MergesFileOps(t *testing.T) {
	activity := ExtractFiles(nil, &FileOps{
		ReadFiles:     []string{"/root/p/x.go"},
		ModifiedFiles: []string{"/root/p/y.go"},
		CreatedFiles:  []string{"/root/p/z.go"},
	})
	if !activity.Modified.Has("y.go") || !activity.Created.Has("z.go") || !activity.Read.Has("x.go") {
		t.Fatalf("hook FileOps were not merged: %#v", activity)
	}
}

func TestLongestCommonDirPrefix(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  string
	}{
		{"two under a deep root", []string{"/root/p/q/a.go", "/root/p/q/b.go"}, "/root/p/q/"},
		// split("/a/x.go") = ["", "a", "x.go"]; the loop reaches i=2, which
		// clears the `i < 2` guard, so /a/ is a valid trim prefix.
		{"root plus one segment", []string{"/a/x.go", "/a/y.go"}, "/a/"},
		{"no common prefix", []string{"/a/x.go", "/b/y.go"}, ""},
		{"one path only", []string{"/root/p/a.go"}, ""},
		{"relative paths ignored", []string{"a.go", "b.go"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := longestCommonDirPrefix(test.paths); got != test.want {
				t.Fatalf("longestCommonDirPrefix(%v) = %q, want %q", test.paths, got, test.want)
			}
		})
	}
}

func TestFormatFileActivity(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/root/p/a.go"}},
		{Kind: KindToolCall, Name: "write", Args: map[string]any{"path": "/root/p/b.go"}},
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/root/p/c.go"}},
	}
	lines := formatFileActivity(blocks, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Modified: a.go, b.go") {
		t.Fatalf("Modified line wrong:\n%s", joined)
	}
	// b.go was written, so it must not also appear under Created.
	if strings.Contains(joined, "Created:") {
		t.Fatalf("a modified file also appears as Created:\n%s", joined)
	}
	if !strings.Contains(joined, "Read: c.go") {
		t.Fatalf("Read line missing:\n%s", joined)
	}
}

func TestFormatFileActivity_CapsCategoryAtTen(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 12)
	for i := 0; i < 12; i++ {
		name := "f" + jsNumberString(float64(i)) + ".go"
		blocks = append(blocks, NormalizedBlock{
			Kind: KindToolCall, Name: "read",
			Args: map[string]any{"path": "/root/p/" + name},
		})
	}
	joined := strings.Join(formatFileActivity(blocks, nil), "\n")
	if !strings.Contains(joined, "(+2 more)") {
		t.Fatalf("the cap marker is missing:\n%s", joined)
	}
}

// ── commits ─────────────────────────────────────────────────────────────────

func TestExtractCommits_FromCommandMessage(t *testing.T) {
	// extractCommits reads tool_call bash blocks and pairs the hash from the
	// following tool_result, exactly as commits.ts does.
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "fix: handle expiry"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main abc1234] fix: handle expiry"},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "git status"}},
	}
	got := ExtractCommits(blocks)
	if len(got) != 1 {
		t.Fatalf("ExtractCommits = %#v, want one commit", got)
	}
	if got[0].Message != "fix: handle expiry" {
		t.Fatalf("commit message = %q", got[0].Message)
	}
	if !got[0].HasHash || got[0].Hash != "abc1234" {
		t.Fatalf("hash not paired from the tool_result: %#v", got[0])
	}
}

func TestExtractCommits_AmendWithQuotedMessage(t *testing.T) {
	// An amend with a fresh quoted message records the new sha through the same
	// COMMIT_MSG_RE path (the regex requires a quoted -m subject).
	got := ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit --amend -m "fix: reworded"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main cafe123] fix: reworded"},
	})
	if len(got) != 1 || got[0].Hash != "cafe123" || got[0].Message != "fix: reworded" {
		t.Fatalf("ExtractCommits(amend) = %#v", got)
	}
}

func TestFormatCommits(t *testing.T) {
	commits := []CommitInfo{
		{Hash: "aaaaaaa", HasHash: true, Message: "first"},
		{Message: "no hash"},
		{Hash: "bbbbbbb", HasHash: true, Message: "third"},
	}
	got := FormatCommits(commits, 8)
	want := []string{"aaaaaaa: first", "no hash", "bbbbbbb: third"}
	if len(got) != len(want) {
		t.Fatalf("FormatCommits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FormatCommits = %v, want %v", got, want)
		}
	}
	// The cap keeps the most recent.
	if got := FormatCommits(commits, 2); len(got) != 2 || got[0] != "no hash" {
		t.Fatalf("FormatCommits cap kept the wrong end: %v", got)
	}
}

// ── tracked commands ────────────────────────────────────────────────────────

func TestExtractTrackedCommands(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go build ./cmd/x && go vet ./..."}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "ls -la"}},
	}
	activity := ExtractTrackedCommands(blocks, []string{"go", "bun"})
	got := FormatTrackedCommands(activity)
	if len(got) != 1 || !strings.HasPrefix(got[0], "go: ") {
		t.Fatalf("FormatTrackedCommands = %v", got)
	}
	joined := got[0]
	for _, want := range []string{"go test ./...", "go build ./cmd/x", "go vet ./..."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("a tracked invocation was lost:\n%s", joined)
		}
	}
}

func TestExtractTrackedCommands_CtxRunPatchShapes(t *testing.T) {
	blocks := []NormalizedBlock{
		// argv array form.
		{Kind: KindToolCall, Name: "ctx_run", Args: map[string]any{
			"argv": []any{"go", "test", "./..."},
		}},
		// batch commands array form.
		{Kind: KindToolCall, Name: "ctx_run", Args: map[string]any{
			"commands": []any{"go vet ./...", map[string]any{"command": "go build ./..."}},
		}},
	}
	activity := ExtractTrackedCommands(blocks, []string{"go"})
	entries := activity.ByCommand["go"]
	joined := strings.Join(entries, " | ")
	for _, want := range []string{"go test ./...", "go vet ./...", "go build ./..."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("a patch command shape was not tracked:\n%s", joined)
		}
	}
}

// TestExtractTrackedCommands_ReRunMovesToTail lives in patchCtxRun_test.go.

func TestExtractTrackedCommands_EmptyConfig(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}},
	}
	activity := ExtractTrackedCommands(blocks, nil)
	if len(activity.Order) != 0 || len(FormatTrackedCommands(activity)) != 0 {
		t.Fatalf("an empty trackCommands config produced output: %#v", activity)
	}
}

// ── buildSections end to end ────────────────────────────────────────────────

func TestBuildSections(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("port the sanitizer to Go", 0, true),
		// A separate turn: as a bullet of the first block, "always use tabs"
		// would also be a goal and dedupPreferencesAgainstGoals would drop it
		// from the preferences (that dedup is pinned by TestDedupPreferencesAgainstGoals).
		userBlock("always use tabs", 1, true),
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/root/p/sanitize.go"}},
		// A second absolute path under the same root lets the common-prefix trim run.
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/root/p/main.go"}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "feat: sanitize"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main 1111111] feat: sanitize"},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}},
		{Kind: KindAssistant, Text: "Done. The suite passes."},
	}
	data := BuildSections(BuildSectionsInput{
		Blocks:        blocks,
		TrackCommands: []string{"go"},
	})
	if !containsStr(data.SessionGoal, "port the sanitizer to Go") {
		t.Fatalf("SessionGoal = %v", data.SessionGoal)
	}
	joinedFiles := strings.Join(data.FilesAndChanges, "\n")
	if !strings.Contains(joinedFiles, "Modified: sanitize.go") || !strings.Contains(joinedFiles, "Read: main.go") {
		t.Fatalf("FilesAndChanges = %v", data.FilesAndChanges)
	}
	if !containsStr(data.Commits, "1111111: feat: sanitize") {
		t.Fatalf("Commits = %v", data.Commits)
	}
	if !containsStr(data.UserPreferences, "always use tabs") {
		t.Fatalf("UserPreferences = %v", data.UserPreferences)
	}
	if len(data.TrackedCommands) == 0 || !strings.Contains(data.TrackedCommands[0], "go test ./...") {
		t.Fatalf("TrackedCommands = %v", data.TrackedCommands)
	}
	if !strings.Contains(data.BriefTranscript, "[user]") || !strings.Contains(data.BriefTranscript, "[assistant]") {
		t.Fatalf("BriefTranscript = %q", data.BriefTranscript)
	}
}

func TestBuildSections_BriefBlocksOverride(t *testing.T) {
	all := []NormalizedBlock{
		userBlock("the real task", 0, true),
		userBlock("<analysis>\npreamble noise\n</analysis>", 1, true),
	}
	data := BuildSections(BuildSectionsInput{
		Blocks:      all,
		BriefBlocks: all[:1],
	})
	if strings.Contains(data.BriefTranscript, "analysis") {
		t.Fatalf("BriefBlocks was ignored:\n%s", data.BriefTranscript)
	}
	if !strings.Contains(data.BriefTranscript, "the real task") {
		t.Fatalf("BriefBlocks content missing:\n%s", data.BriefTranscript)
	}
}
