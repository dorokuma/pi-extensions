package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/core/summarize.ts: section extraction, the three merge shapes,
// the brief roll-forward and compile()'s RECALL_NOTE discipline.

// longBrief renders n numbered lines, for cap/merge assertions.
func longBrief(prefix string, n int) string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, prefix+" "+jsNumberString(float64(i)))
	}
	return strings.Join(lines, "\n")
}

func TestSectionOf(t *testing.T) {
	text := "[Session Goal]\n- fix auth\n\n[Commits]\n- abc: x\n\n---\n\nbrief"
	got := sectionOf(text, "Session Goal")
	if got != "[Session Goal]\n- fix auth" {
		t.Fatalf("sectionOf(Session Goal) = %q", got)
	}
	if got := sectionOf(text, "Commits"); got != "[Commits]\n- abc: x" {
		t.Fatalf("sectionOf(Commits) = %q", got)
	}
	if got := sectionOf(text, "Missing"); got != "" {
		t.Fatalf("sectionOf(missing) = %q", got)
	}
}

func TestBriefOf(t *testing.T) {
	text := "[Session Goal]\n- x" + patchSep + "[user]\nhi"
	if got := briefOf(text); got != "[user]\nhi" {
		t.Fatalf("briefOf = %q", got)
	}
	if got := briefOf("no separator here"); got != "" {
		t.Fatalf("briefOf without a separator = %q", got)
	}
}

func TestMergeHeaderSection_GoalDedupAndCap(t *testing.T) {
	prev := "[Session Goal]\n- a\n- b"
	fresh := "[Session Goal]\n- b\n- c"
	got := mergeHeaderSection("Session Goal", prev, fresh)
	// Union without duplicate b, then the 8-line cap applied from the tail.
	want := "[Session Goal]\n- a\n- b\n- c"
	if got != want {
		t.Fatalf("mergeHeaderSection = %q, want %q", got, want)
	}

	// Eleven distinct goals cap to the last eight. Both sides must be non-empty:
	// with an empty prev, mergeHeaderSection returns fresh verbatim (the TS
	// `if (!prev) return fresh`), so the cap is applied on a real merge.
	many := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		many = append(many, "- g"+jsNumberString(float64(i)))
	}
	capped := mergeHeaderSection("Session Goal", "[Session Goal]\n- seed", strings.Join(many, "\n"))
	if lines := jsSplitLines(capped); len(lines) != 9 { // header + 8
		t.Fatalf("capped Session Goal has %d lines, want 9:\n%s", len(lines), capped)
	}
	if !strings.Contains(capped, "- g10") || strings.Contains(capped, "- g0\n") || strings.HasSuffix(capped, "- g0") {
		t.Fatalf("the cap kept the wrong end:\n%s", capped)
	}
	// An unmerged pass-through keeps everything, per the TS short-circuit.
	if passthrough := mergeHeaderSection("Session Goal", "", strings.Join(many, "\n")); jsSplitLines(passthrough)[0] != "- g0" {
		t.Fatalf("the pass-through path truncated the input:\n%s", passthrough)
	}
}

func TestMergeHeaderSection_OutstandingContextIsFreshOnly(t *testing.T) {
	prev := "[Outstanding Context]\n- old problem"
	fresh := "[Outstanding Context]\n- new problem"
	if got := mergeHeaderSection("Outstanding Context", prev, fresh); got != fresh {
		t.Fatalf("Outstanding Context should be fresh only, got %q", got)
	}
	if got := mergeHeaderSection("Outstanding Context", prev, ""); got != "" {
		t.Fatalf("Outstanding Context with no fresh text should be empty, got %q", got)
	}
}

func TestMergeHeaderSection_DropsSkillLines(t *testing.T) {
	prev := "[User Preferences]\n- prefer tabs\n- <skill name=\"x\">"
	fresh := "[User Preferences]\n- answer in Chinese"
	got := mergeHeaderSection("User Preferences", prev, fresh)
	if strings.Contains(got, "<skill") {
		t.Fatalf("a skill line survived the merge:\n%s", got)
	}
	if !strings.Contains(got, "prefer tabs") || !strings.Contains(got, "answer in Chinese") {
		t.Fatalf("merge lost a preference:\n%s", got)
	}
}

func TestMergeFileLines_CategoryUnionAndDedup(t *testing.T) {
	prev := "[Files And Changes]\n- Modified: a.go\n- Created: b.go"
	fresh := "[Files And Changes]\n- Modified: a.go, c.go\n- Read: d.go"
	got := mergeFileLines(prev, fresh)
	for _, want := range []string{"a.go", "c.go", "b.go", "d.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("merge lost %q:\n%s", want, got)
		}
	}
	// A file that is Modified must not also appear as Created.
	created := ""
	for _, line := range jsSplitLines(got) {
		if strings.HasPrefix(line, "- Created: ") {
			created = line
		}
	}
	if strings.Contains(created, "b.go") && strings.Contains(got, "- Modified: b.go") {
		t.Fatalf("b.go is both Modified and Created:\n%s", got)
	}
}

func TestMergeTrackedCommandLines_TailCapAndTouch(t *testing.T) {
	// Entries are the bare command text; the "name: " prefix belongs to the
	// rendered line, not the entry (extract/tracked-commands.ts formatTrackedCommands).
	prevEntries := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		prevEntries = append(prevEntries, "cmd"+jsNumberString(float64(i)))
	}
	prev := "[Tracked Commands]\n- git: " + strings.Join(prevEntries, CommandSeparator)
	// Re-running cmd0 makes it the most recently used.
	fresh := "[Tracked Commands]\n- git: cmd0"
	got := mergeTrackedCommandLines(prev, fresh)
	body := got
	if idx := strings.Index(got, "\n"); idx >= 0 {
		body = got[idx+1:]
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "cmd0") {
		t.Fatalf("the re-run entry was not moved to the tail:\n%s", got)
	}
	if strings.Count(body, "cmd0") != 1 {
		t.Fatalf("the re-run entry was duplicated:\n%s", got)
	}
	if strings.Contains(got, "earlier)") {
		// Only six entries, so no cap marker is expected here.
		t.Fatalf("unexpected cap marker with six entries:\n%s", got)
	}
}

func TestMergeTrackedCommandLines_DiscoverNames(t *testing.T) {
	// A name that appears only in the previous summary must survive.
	prev := "[Tracked Commands]\n- kubectl: get pods"
	fresh := "[Tracked Commands]\n- docker: ps"
	got := mergeTrackedCommandLines(prev, fresh)
	if !strings.Contains(got, "- kubectl: get pods") || !strings.Contains(got, "- docker: ps") {
		t.Fatalf("a category was orphaned:\n%s", got)
	}
}

func TestMergePrevious_RollsTheBriefForward(t *testing.T) {
	prev := "[Session Goal]\n- a" + patchSep + "[user]\nold"
	fresh := "[Session Goal]\n- b" + patchSep + "[user]\nnew"
	got := mergePrevious(prev, fresh, mergePreviousOptions{})
	if !strings.Contains(got, "[user]\nold") || !strings.Contains(got, "[user]\nnew") {
		t.Fatalf("the brief did not roll forward:\n%s", got)
	}
	if !strings.Contains(got, "- a") || !strings.Contains(got, "- b") {
		t.Fatalf("the goal did not merge:\n%s", got)
	}
}

func TestMergePrevious_PreserveFreshBriefCapsTheOldPart(t *testing.T) {
	prev := "[Session Goal]\n- a" + patchSep + longBrief("oldline", 200)
	fresh := "[Session Goal]\n- b" + patchSep + longBrief("newline", 10)
	got := mergePrevious(prev, fresh, mergePreviousOptions{preserveFreshBrief: true})
	lines := jsSplitLines(got)
	// BRIEF_MAX_LINES is a budget for the brief, and mergeBriefTranscriptWithFreshBudget
	// spends it as `capBriefToLineBudget(prev, MAX - freshLines) + "\n\n" + fresh`.
	// capBriefToLineBudget only shortens to its budget when the kept window holds
	// a section header to cut at; otherwise it keeps the whole window. So the
	// merged brief is at most (MAX lines of prev) + 1 blank + (fresh lines), and
	// the sections above add their own lines. This pins the TS arithmetic.
	briefLines := 0
	if idx := strings.LastIndex(got, summarySeparator); idx >= 0 {
		briefLines = len(jsSplitLines(got[idx+len(summarySeparator):]))
	}
	// capBriefToLineBudget keeps MAX-freshLines lines of the previous brief; the
	// merge then adds one blank line and the fresh brief. Allow two lines of
	// slack for the leading omission marker and the blank line after it.
	if want := BriefMaxLines + 1 + 2; briefLines > want {
		t.Fatalf("the preserved brief is %d lines, want at most %d (budget %d + a blank line + slack for the omission marker)",
			briefLines, want, BriefMaxLines)
	}
	if len(lines) > BriefMaxLines+16 {
		t.Fatalf("preserved merge produced %d lines, well over the brief budget", len(lines))
	}
	if !strings.Contains(got, "newline 9") {
		t.Fatal("the preserved merge dropped the fresh brief")
	}
	if !strings.Contains(got, "earlier lines omitted") {
		t.Fatal("the preserved merge did not cap the older part")
	}
}

// countNote counts how many times RECALL_NOTE appears in text that has been
// wrapped: the note is longer than the wrap width, so it is counted on the
// whitespace-flattened text, as verify.mjs does.
func countNote(text string) int {
	return strings.Count(flat(text), flat(RecallNote))
}

func TestCompile_AppendsRecallNoteOnce(t *testing.T) {
	got := Compile(CompileInput{Messages: []map[string]any{
		{"role": "user", "content": "fix the auth bug"},
		{"role": "assistant", "content": "on it"},
	}})
	if countNote(got) != 1 {
		t.Fatalf("RECALL_NOTE appears %d times, want 1:\n%s", countNote(got), got)
	}
	if !strings.Contains(got, "[user]") {
		t.Fatalf("the compile output has no brief transcript:\n%s", got)
	}
}

func TestCompile_EmptyInputIsEmpty(t *testing.T) {
	if got := Compile(CompileInput{Messages: nil}); got != "" {
		t.Fatalf("Compile(nil) = %q, want empty", got)
	}
}

func TestCompile_StripsPreviousNoteBeforeMerging(t *testing.T) {
	previous := WrapLongLinesDefault("[Session Goal]\n- old" + patchSep + RecallNote)
	got := Compile(CompileInput{
		Messages:           []map[string]any{{"role": "user", "content": "next task"}},
		PreviousSummary:    previous,
		HasPreviousSummary: true,
	})
	if countNote(got) != 1 {
		t.Fatalf("RECALL_NOTE appears %d times after a merge, want 1:\n%s", countNote(got), got)
	}
	if !strings.Contains(got, "- old") {
		t.Fatal("the previous section content was lost by the strip")
	}
}

func TestCompileRanked_UsesTheRankedBrief(t *testing.T) {
	messages := make([]map[string]any, 0, 60)
	for i := 0; i < 60; i++ {
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": longBrief("para", 20)}},
		})
	}
	plain := Compile(CompileInput{Messages: messages})
	ranked := CompileRanked(RankedCompileInput{
		CompileInput: CompileInput{Messages: messages},
		Ranking: &BriefRankingOptions{
			MaxBriefChars:        400,
			MaxBriefCharsCeiling: 400,
			BriefCharsPerBlock:   1,
		},
	})
	if ranked == "" || plain == "" {
		t.Fatal("a compile produced no output")
	}
	if jsLen(ranked) >= jsLen(plain) {
		t.Fatalf("the ranked brief is not smaller (%d vs %d)", jsLen(ranked), jsLen(plain))
	}
	if countNote(ranked) != 1 {
		t.Fatal("the ranked compile dropped RECALL_NOTE")
	}
}

func TestCompileRanked_InheritsFileOps(t *testing.T) {
	// longestCommonDirPrefix needs at least two absolute paths before it trims
	// anything (files.ts), so the fixture supplies two under one root.
	got := CompileRanked(RankedCompileInput{
		CompileInput: CompileInput{
			Messages: []map[string]any{
				{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "edit", "arguments": map[string]any{"path": "/a/b.go"}}}},
			},
			FileOps: &FileOps{ModifiedFiles: []string{"/a/b.go", "/a/c.go"}},
		},
	})
	if !strings.Contains(got, "Modified: b.go, c.go") {
		t.Fatalf("the ranking did not inherit fileOps:\n%s", got)
	}
}

func TestRejoinWrappedSection(t *testing.T) {
	// A wrapped item's indented continuation joins back into one line.
	wrapped := "[Session Goal]\n- fix the auth bug\n  in the login flow"
	got := rejoinWrappedSection(wrapped)
	if strings.Contains(got, "\n  in") {
		t.Fatalf("the continuation line was not rejoined: %q", got)
	}
	if !strings.Contains(got, "fix the auth bug in the login flow") {
		t.Fatalf("rejoin lost text: %q", got)
	}
}

func TestBuildRecallNoteRe_MatchesTheWrappedNote(t *testing.T) {
	wrapped := WrapLongLinesDefault(RecallNote)
	if !buildRecallNoteRe(RecallNote).MatchString(patchSep + wrapped) {
		t.Fatalf("the strip regex did not match the wrapped note:\n%s", wrapped)
	}
}

func TestSectionHeaders(t *testing.T) {
	summary := "[Session Goal]\n- x\n\n[Commits]\n- y\n\n---\n\n[user]\nhi"
	got := SectionHeaders(summary)
	want := []string{"Session Goal", "Commits", "user"}
	if len(got) != len(want) {
		t.Fatalf("SectionHeaders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SectionHeaders = %v, want %v", got, want)
		}
	}
}
