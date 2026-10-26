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
	text := "[Early Session Requests]\n- fix auth\n\n[Commits]\n- abc: x\n\n---\n\nbrief"
	got := sectionOf(text, "Early Session Requests")
	if got != "[Early Session Requests]\n- fix auth" {
		t.Fatalf("sectionOf(Early Session Requests) = %q", got)
	}
	if got := sectionOf(text, "Commits"); got != "[Commits]\n- abc: x" {
		t.Fatalf("sectionOf(Commits) = %q", got)
	}
	if got := sectionOf(text, "Missing"); got != "" {
		t.Fatalf("sectionOf(missing) = %q", got)
	}
}

// TestSectionOf_HeaderAreaOnly pins 0.11.1 (#60): a section is a "[Title]" line
// standing alone in the header area, so a transcript line that merely starts
// with the name is not a section.
func TestSectionOf_HeaderAreaOnly(t *testing.T) {
	text := "[Early Session Requests]\n- fix auth\n\n---\n\n[user]\nsee [Tracked Commands] for the log\n[assistant]\n" +
		"[Tracked Commands] is not a section here"
	if got := sectionOf(text, "Tracked Commands"); got != "" {
		t.Fatalf("a transcript mention was read as a section: %q", got)
	}
	// A wrapped item keeps its continuation lines: the section runs to the next
	// blank line.
	wrapped := "[Early Session Requests]\n- a very long request\n  continues here\n\n[Commits]\n- abc: x"
	if got := sectionOf(wrapped, "Early Session Requests"); got != "[Early Session Requests]\n- a very long request\n  continues here" {
		t.Fatalf("the section was cut at the continuation line: %q", got)
	}
	// Without a leading header line the whole text is the brief, so no section
	// is found at all.
	if got := sectionOf("[user]\njust a transcript", "Early Session Requests"); got != "" {
		t.Fatalf("a brief-only text produced a section: %q", got)
	}
}

func TestBriefOf(t *testing.T) {
	text := "[Early Session Requests]\n- x" + patchSep + "[user]\nhi"
	if got := briefOf(text); got != "[user]\nhi" {
		t.Fatalf("briefOf = %q", got)
	}
	// Text that does not open with a header line is a brief in its entirety:
	// TS's splitSummary returns the trimmed text with no head.
	if got := briefOf("no separator here"); got != "no separator here" {
		t.Fatalf("briefOf without a separator = %q", got)
	}
}

// TestSplitSummary_NoLeadingHeader pins the shape rule: text that does not open
// with a header line is the brief, and header sections with no separator have
// no brief.
func TestSplitSummary_NoLeadingHeader(t *testing.T) {
	head, brief := splitSummary("[user]\nhi\n\n[assistant]\nthere")
	if head != "" || brief != "[user]\nhi\n\n[assistant]\nthere" {
		t.Fatalf("splitSummary = (%q, %q), want (\"\", the whole text as brief)", head, brief)
	}
	head, brief = splitSummary("[Early Session Requests]\n- x\n\n[Commits]\n- y")
	if head != "[Early Session Requests]\n- x\n\n[Commits]\n- y" || brief != "" {
		t.Fatalf("splitSummary = (%q, %q), want (the text, \"\")", head, brief)
	}
}

func TestMergeHeaderSection_EarlyRequestsDedupAndCap(t *testing.T) {
	prev := "[Early Session Requests]\n- a\n- b"
	fresh := "[Early Session Requests]\n- b\n- c"
	got := mergeHeaderSection(earlyHeader, prev, fresh)
	// Union without duplicate b, then the 8-line cap applied from the tail.
	want := "[Early Session Requests]\n- a\n- b\n- c"
	if got != want {
		t.Fatalf("mergeHeaderSection = %q, want %q", got, want)
	}

	// Nine distinct requests cap to the last eight. Both sides must be non-empty:
	// with an empty prev, mergeHeaderSection returns fresh verbatim (the TS
	// `if (!prev) return fresh`), so the cap is applied on a real merge.
	many := make([]string, 0, 9)
	for i := 0; i < 9; i++ {
		many = append(many, "- g"+jsNumberString(float64(i)))
	}
	capped := mergeHeaderSection(earlyHeader, "[Early Session Requests]\n- seed", strings.Join(many, "\n"))
	if lines := jsSplitLines(capped); len(lines) != 9 { // header + 8
		t.Fatalf("capped Early Session Requests has %d lines, want 9:\n%s", len(lines), capped)
	}
	if !strings.Contains(capped, "- g8") || strings.Contains(capped, "- g0\n") || strings.HasSuffix(capped, "- g0") {
		t.Fatalf("the cap kept the wrong end:\n%s", capped)
	}
	// An unmerged pass-through keeps everything, per the TS short-circuit.
	if passthrough := mergeHeaderSection(earlyHeader, "", strings.Join(many, "\n")); jsSplitLines(passthrough)[0] != "- g0" {
		t.Fatalf("the pass-through path truncated the input:\n%s", passthrough)
	}
}

// TestMergeHeaderSection_CommitsDedupHashedAndHashless pins the 0.11.1 commit
// merge: one line per commit, a hashless line replaced in place by the hashed
// line of the same commit.
func TestMergeHeaderSection_CommitsDedupHashedAndHashless(t *testing.T) {
	prev := "[Commits]\n- abc1234: fix auth\n- drop the dead code"
	fresh := "[Commits]\n- fix auth\n- add the docs"
	got := mergeHeaderSection("Commits", prev, fresh)
	if strings.Count(got, "fix auth") != 1 {
		t.Fatalf("the same commit was kept twice:\n%s", got)
	}
	if !strings.Contains(got, "- abc1234: fix auth") {
		t.Fatalf("the hashed line lost its place:\n%s", got)
	}
	if !strings.Contains(got, "- add the docs") {
		t.Fatalf("a fresh commit was lost:\n%s", got)
	}
	// The hashed line stays where the first copy was.
	if strings.Index(got, "abc1234") > strings.Index(got, "add the docs") {
		t.Fatalf("the hashed line moved to the tail:\n%s", got)
	}
}

// TestMergeHeaderSection_UserMessagesKeepsNewest pins the User Messages merge:
// the newest 15 lines, oldest first, a repeat moving to the end.
func TestMergeHeaderSection_UserMessagesKeepsNewest(t *testing.T) {
	prevLines := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		prevLines = append(prevLines, "- old "+jsNumberString(float64(i)))
	}
	prev := "[User Messages]\n" + strings.Join(prevLines, "\n")
	fresh := "[User Messages]\n- old 0\n- brand new"
	got := mergeHeaderSection("User Messages", prev, fresh)
	lines := jsSplitLines(got)
	if len(lines) != 16 { // header + 15
		t.Fatalf("User Messages has %d lines, want 16:\n%s", len(lines), got)
	}
	if !strings.HasSuffix(got, "- brand new") {
		t.Fatalf("the newest message is not last:\n%s", got)
	}
	if strings.Count(got, "old 0") != 1 {
		t.Fatalf("the repeated line was duplicated:\n%s", got)
	}
	// The repeated line moved to the end, so the oldest dropped out.
	if strings.Contains(got, "old 1\n") || strings.HasSuffix(got, "old 1") {
		t.Fatalf("the oldest message survived the cap:\n%s", got)
	}
}

// TestDedupCommitLines pins dedupCommitLines(): first-seen order, the hashed
// line kept where the hashless one stood.
func TestDedupCommitLines(t *testing.T) {
	got := dedupCommitLines([]string{
		"- abc1234: one",
		"- two",
		"- abc1234: one",
		"- two",
		"- def4567: three",
	})
	want := []string{"- abc1234: one", "- two", "- def4567: three"}
	if len(got) != len(want) {
		t.Fatalf("dedupCommitLines = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupCommitLines = %#v, want %#v", got, want)
		}
	}
	// A hashless line seen first is upgraded in place, not moved to the tail.
	got = dedupCommitLines([]string{"- one", "- other", "- abc1234: one"})
	want = []string{"- abc1234: one", "- other"}
	if len(got) != len(want) {
		t.Fatalf("dedupCommitLines = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupCommitLines = %#v, want %#v", got, want)
		}
	}
}

// TestPinnedEarlyRequests_FromEarlySection pins the first summary's requests
// being kept as they are.
func TestPinnedEarlyRequests_FromEarlySection(t *testing.T) {
	prev := "[Early Session Requests]\n- first request\n- second request\n- third request\n- fourth request"
	got := pinnedEarlyRequests(prev)
	want := []string{"- first request", "- second request", "- third request"}
	if len(got) != len(want) {
		t.Fatalf("pinnedEarlyRequests = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pinnedEarlyRequests = %#v, want %#v", got, want)
		}
	}
}

// TestPinnedEarlyRequests_OldSessionGoalDegrades pins the 0.11.0 compatibility
// path: a previous summary written by 0.10.2 has no [Early Session Requests],
// so its first real [Session Goal] line is pinned instead.
func TestPinnedEarlyRequests_OldSessionGoalDegrades(t *testing.T) {
	prev := "[Session Goal]\n- [Scope change]\n- port the extractor to Go\n- keep the patch"
	got := pinnedEarlyRequests(prev)
	if len(got) != 1 || got[0] != "- port the extractor to Go" {
		t.Fatalf("pinnedEarlyRequests = %#v, want the first real goal line", got)
	}
	// A summary with neither section pins nothing.
	if got := pinnedEarlyRequests("[Commits]\n- abc1234: x"); len(got) != 0 {
		t.Fatalf("pinnedEarlyRequests = %#v, want nothing", got)
	}
}

// TestUnwrapSection pins unwrapSection(): a wrapped item's continuation lines
// are rejoined before a merge reads the item.
func TestUnwrapSection(t *testing.T) {
	text := "[Early Session Requests]\n- a very long request\n  continued on the next line\n\n[Commits]\n- abc: x"
	got := unwrapSection(text, "Early Session Requests")
	if strings.Contains(got, "\n  ") {
		t.Fatalf("the continuation line was not rejoined: %q", got)
	}
	if !strings.Contains(got, "a very long request continued on the next line") {
		t.Fatalf("unwrapSection lost text: %q", got)
	}
}

// TestMergePrevious_PinsTheEarlyRequests pins the 0.11.0 rule: the first
// summary sets the early requests and every later compaction keeps them, so the
// fresh session's own first messages are not re-collected.
func TestMergePrevious_PinsTheEarlyRequests(t *testing.T) {
	prev := "[Early Session Requests]\n- the original task book" + patchSep + "[user]\nold"
	fresh := "[Early Session Requests]\n- a later message" + patchSep + "[user]\nnew"
	got := mergePrevious(prev, fresh, mergePreviousOptions{})
	if !strings.Contains(got, "[Early Session Requests]\n- the original task book") {
		t.Fatalf("the pinned early requests were lost:\n%s", got)
	}
	if strings.Contains(got, "a later message") {
		t.Fatalf("a fresh early request replaced the pinned ones:\n%s", got)
	}
	if !strings.Contains(got, "[user]\nold") || !strings.Contains(got, "[user]\nnew") {
		t.Fatalf("the brief did not roll forward:\n%s", got)
	}
}

// TestCompile_HasEarlyRequestsSkipsTheFreshOnes pins hasEarlyRequests: with a
// previous summary that already holds early requests, this compaction's first
// user messages stay ordinary [User Messages].
func TestCompile_HasEarlyRequestsSkipsTheFreshOnes(t *testing.T) {
	previous := WrapLongLinesDefault("[Early Session Requests]\n- the original task book" + patchSep + "[user]\nold")
	got := Compile(CompileInput{
		Messages: []map[string]any{
			{"role": "user", "content": "a follow-up question about the parser"},
			{"role": "assistant", "content": "looking at it"},
		},
		PreviousSummary:    previous,
		HasPreviousSummary: true,
	})
	if !strings.Contains(got, "[Early Session Requests]\n- the original task book") {
		t.Fatalf("the pinned early requests were lost:\n%s", got)
	}
	if !strings.Contains(got, "[User Messages]\n- a follow-up question about the parser") {
		t.Fatalf("the follow-up did not become a user message:\n%s", got)
	}
}

// TestCompile_FirstCompactionCollectsEarlyRequests pins the other side: with no
// previous summary the first three user messages are the early requests and are
// not repeated in [User Messages].
func TestCompile_FirstCompactionCollectsEarlyRequests(t *testing.T) {
	got := Compile(CompileInput{Messages: []map[string]any{
		{"role": "user", "content": "first request: port the extractor"},
		{"role": "assistant", "content": "on it"},
		{"role": "user", "content": "second request: keep the patch"},
		{"role": "user", "content": "third request: add the tests"},
		{"role": "user", "content": "fourth request: update the docs"},
	}})
	if !strings.Contains(got, "[Early Session Requests]\n- first request: port the extractor") {
		t.Fatalf("the early requests were not collected:\n%s", got)
	}
	if !strings.Contains(got, "- fourth request: update the docs") {
		t.Fatalf("the fourth request is missing from [User Messages]:\n%s", got)
	}
	// The header part is what the merge owns; the brief transcript keeps every
	// message verbatim, so only the part before the separator is counted.
	head, _ := splitSummary(WrapLongLinesDefault(got))
	if strings.Count(head, "first request: port the extractor") != 1 {
		t.Fatalf("an early request was repeated in the header:\n%s", head)
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
	prev := "[User Messages]\n- a" + patchSep + "[user]\nold"
	fresh := "[User Messages]\n- b" + patchSep + "[user]\nnew"
	got := mergePrevious(prev, fresh, mergePreviousOptions{})
	if !strings.Contains(got, "[user]\nold") || !strings.Contains(got, "[user]\nnew") {
		t.Fatalf("the brief did not roll forward:\n%s", got)
	}
	if !strings.Contains(got, "- a") || !strings.Contains(got, "- b") {
		t.Fatalf("the user messages did not merge:\n%s", got)
	}
}

func TestMergePrevious_PreserveFreshBriefCapsTheOldPart(t *testing.T) {
	prev := "[User Messages]\n- a" + patchSep + longBrief("oldline", 200)
	fresh := "[User Messages]\n- b" + patchSep + longBrief("newline", 10)
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
	previous := WrapLongLinesDefault("[Early Session Requests]\n- old" + patchSep + RecallNote)
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
	wrapped := "[Early Session Requests]\n- fix the auth bug\n  in the login flow"
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
