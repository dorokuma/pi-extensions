package pivcc

import (
	"regexp"
	"strings"
	"testing"
)

// Tests for local patch #2 (pi-vcc-0.10.0-recall-note). Every check in the
// patch's verify.mjs is reproduced here: V0 (module surface), V1a-d (constant
// and its strip regex), V2/V2b (the note appears exactly once across merges and
// a pre-patch summary keeps its stale copy), V3a-c (the tail adds no
// '['-anchored line) and V4a-d (nothing but the constant changed).

// patchSep is verify.mjs's SEP.
const patchSep = "\n\n---\n\n"

// patchAdded is verify.mjs's ADDED: the sentence the patch appends.
const patchAdded = "After any compaction, first call vcc_recall to pull this task's original instructions " +
	"(the first user message) back before continuing; never judge whether it is needed."

// upstreamRecallNote is the UNPATCHED 0.10.2 RECALL_NOTE, taken verbatim from
// the archived original at
// /root/.pi/agent/patches/pi-vcc-0.10.0-recall-note/format.ts.orig-0.10.2.ts.
// The patch keeps it as a prefix of the new constant.
const upstreamRecallNote = "Use `vcc_recall` to search for prior work, decisions, and context from before this summary. " +
	"Do not redo work already completed."

// flat is verify.mjs's `flat`: collapse all whitespace.
var flatRe = mustCompile(jsSpace + `+`)

func flat(s string) string {
	return jsTrimSpace(flatRe.ReplaceAllString(s, " "))
}

// occur is verify.mjs's `occur`: how many non-overlapping copies hay holds.
func occur(hay, needle string) int {
	if needle == "" {
		return 0
	}
	return strings.Count(hay, needle)
}

// buildNoteRe is verify.mjs's buildNoteRe: RECALL_NOTE_RE rebuilt from a note.
func buildNoteRe(note string) *regexp.Regexp {
	return buildRecallNoteRe(note)
}

// ── the exact constant ──────────────────────────────────────────────────────

// TestRecallNoteExactValue pins the constant byte-for-byte. The task book
// requires this exact string; a change here is a behaviour change.
func TestRecallNoteExactValue(t *testing.T) {
	want := "Use `vcc_recall` to search for prior work, decisions, and context from before this summary. " +
		"Do not redo work already completed." +
		" After any compaction, first call vcc_recall to pull this task's original instructions " +
		"(the first user message) back before continuing; never judge whether it is needed."
	if RecallNote != want {
		t.Fatalf("RecallNote = %q\nwant %q", RecallNote, want)
	}
}

// ── V1: note content + regex ────────────────────────────────────────────────

// TestVerify_RecallNote_V1a pins verify.mjs V1a: the new note contains the
// appended sentence.
func TestVerify_RecallNote_V1a(t *testing.T) {
	if !strings.Contains(RecallNote, patchAdded) {
		t.Fatalf("RecallNote does not contain the appended sentence: %q", RecallNote)
	}
}

// TestVerify_RecallNote_V1b pins V1b: the old sentence is preserved verbatim as
// a prefix.
func TestVerify_RecallNote_V1b(t *testing.T) {
	if !strings.HasPrefix(RecallNote, upstreamRecallNote) {
		t.Fatalf("RecallNote does not start with the upstream sentence: %q", RecallNote)
	}
	if jsTrimSpace(upstreamRecallNote) == "" {
		t.Fatal("the upstream sentence must not be empty")
	}
}

// TestVerify_RecallNote_V1c pins V1c: RECALL_NOTE_RE, rebuilt from the new
// constant, matches the wrapped note in a rendered summary.
func TestVerify_RecallNote_V1c(t *testing.T) {
	product := WrapLongLinesDefault("[Session Goal]\n- x" + patchSep + RecallNote)
	if !buildNoteRe(RecallNote).MatchString(product) {
		t.Fatalf("the rebuilt RECALL_NOTE_RE did not match the wrapped note:\n%s", product)
	}
}

// TestVerify_RecallNote_V1d pins V1d: stripping it removes the appended sentence
// from the rendered text.
func TestVerify_RecallNote_V1d(t *testing.T) {
	product := WrapLongLinesDefault("[Session Goal]\n- x" + patchSep + RecallNote)
	stripped := jsTrimRightSpace(buildNoteRe(RecallNote).ReplaceAllString(product, "\n\n"))
	if strings.Contains(stripped, patchAdded) {
		t.Fatalf("the appended sentence survived the strip: %q", jsSlice(stripped, maxInt(0, jsLen(stripped)-60), jsLen(stripped)))
	}
	if strings.Contains(stripped, "vcc_recall") {
		t.Fatalf("the note's text survived the strip: %q", jsSlice(stripped, maxInt(0, jsLen(stripped)-60), jsLen(stripped)))
	}
}

// ── V2: the accumulator ─────────────────────────────────────────────────────

// renderSample is verify.mjs's `render`: the body plus a note, wrapped.
func renderSample(note string) string {
	data := SectionData{
		SessionGoal:        []string{"Patch pi-vcc so the summary tail always tells the model to recall the task book"},
		OutstandingContext: []string{"pi must be restarted for the patch to take effect"},
		FilesAndChanges:    []string{"src/core/format.ts: appended one sentence to RECALL_NOTE"},
		Commits:            []string{},
		UserPreferences:    []string{"answer in Chinese"},
		TrackedCommands:    []string{"git: status --porcelain"},
		BriefTranscript:    "user: append the sentence\nassistant: done",
	}
	body := FormatSummary(data, FormatSummaryOptions{})
	return WrapLongLinesDefault(body + patchSep + note)
}

// verifyMessages is verify.mjs's `msgs`.
func verifyMessages() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "please append the recall-note sentence to format.ts"},
		{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "on it"}}},
	}
}

// TestVerify_RecallNote_V2 pins V2: across three merges in a row, the appended
// sentence appears exactly once in the compiled output.
func TestVerify_RecallNote_V2(t *testing.T) {
	stored := renderSample(RecallNote)
	current := stored
	occurrences := make([]int, 0, 3)
	for i := 1; i <= 3; i++ {
		current = Compile(CompileInput{
			Messages:           verifyMessages(),
			PreviousSummary:    current,
			HasPreviousSummary: true,
		})
		occurrences = append(occurrences, occur(flat(current), patchAdded))
	}
	if len(occurrences) != 3 {
		t.Fatalf("ran %d merges, want 3", len(occurrences))
	}
	for round, count := range occurrences {
		if count != 1 {
			t.Fatalf("round %d: the appended sentence appears %d times, want exactly 1 (all rounds: %v)\noutput:\n%s",
				round+1, count, occurrences, current)
		}
	}
}

// TestVerify_RecallNote_V2b pins V2b: a pre-patch summary still carries its old
// note (the documented, accepted side effect), while the new sentence appears
// exactly once.
func TestVerify_RecallNote_V2b(t *testing.T) {
	legacy := Compile(CompileInput{
		Messages:           verifyMessages(),
		PreviousSummary:    renderSample(upstreamRecallNote),
		HasPreviousSummary: true,
	})
	legacyFlat := flat(legacy)
	// Every copy of the NEW note also contains the old sentence as a prefix, so
	// subtract those to count the genuinely stale (pre-patch) copies.
	stale := occur(legacyFlat, flat(upstreamRecallNote)) - occur(legacyFlat, patchAdded)
	if stale != 1 {
		t.Fatalf("stale copies kept = %d, want 1 (the documented flaw)", stale)
	}
	if got := occur(legacyFlat, patchAdded); got != 1 {
		t.Fatalf("the new sentence appears %d times, want 1", got)
	}
	// The stale copy ages out via the brief cap: stripping the new note leaves
	// only the old one, which the next merge keeps as ordinary brief text.
	if !strings.Contains(legacyFlat, flat(upstreamRecallNote)) {
		t.Fatal("the legacy note is not present at all")
	}
}

// ── V3: the tail adds no section ───────────────────────────────────────────

// anchorsOf is verify.mjs's anchorsOf: the '['-anchored names, in order.
func anchorsOf(text string) []string {
	out := make([]string, 0, 8)
	for _, line := range jsSplitLines(text) {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		match := mustCompile(`^\[([^\n\r\x{2028}\x{2029}]+?)\]`).FindStringSubmatch(line)
		if match != nil {
			out = append(out, match[1])
		}
	}
	return out
}

// TestVerify_RecallNote_V3a pins V3a: the note tail adds no '['-anchored line.
func TestVerify_RecallNote_V3a(t *testing.T) {
	product := Compile(CompileInput{Messages: verifyMessages()})
	withoutTail := ""
	if idx := strings.LastIndex(product, patchSep); idx >= 0 {
		withoutTail = product[:idx]
	}
	all := anchorsOf(product)
	body := anchorsOf(withoutTail)
	if len(all) != len(body) {
		t.Fatalf("anchors(product) = %v, anchors(product minus tail) = %v", all, body)
	}
	for i := range all {
		if all[i] != body[i] {
			t.Fatalf("anchors(product) = %v, anchors(product minus tail) = %v", all, body)
		}
	}
}

// TestVerify_RecallNote_V3b pins V3b: no line of the note tail starts with '['.
func TestVerify_RecallNote_V3b(t *testing.T) {
	product := Compile(CompileInput{Messages: verifyMessages()})
	idx := strings.LastIndex(product, patchSep)
	if idx < 0 {
		t.Fatal("the compiled summary has no separator")
	}
	tail := product[idx+len(patchSep):]
	for _, line := range jsSplitLines(tail) {
		if strings.HasPrefix(line, "[") {
			t.Fatalf("a tail line starts with '[': %q\ntail:\n%s", line, tail)
		}
	}
}

// TestVerify_RecallNote_V3c pins V3c: the note rendered on its own has no
// '['-anchored line.
func TestVerify_RecallNote_V3c(t *testing.T) {
	noteOnly := make([]string, 0, 8)
	for _, line := range jsSplitLines(WrapLongLinesDefault(patchSep + RecallNote)) {
		trimmed := jsTrimSpace(line)
		if trimmed == "" || trimmed == "---" {
			continue
		}
		noteOnly = append(noteOnly, line)
	}
	if len(noteOnly) == 0 {
		t.Fatal("the note rendered to no lines")
	}
	for _, line := range noteOnly {
		if strings.HasPrefix(line, "[") {
			t.Fatalf("a note line starts with '[': %q", line)
		}
	}
}

// ── V4: everything but the tail is unchanged ───────────────────────────────

// TestVerify_RecallNote_V4c pins V4c: the rendered sample is line-identical up
// to the tail note, comparing the upstream note with the patched one.
func TestVerify_RecallNote_V4c(t *testing.T) {
	oldRendered := jsSplitLines(renderSample(upstreamRecallNote))
	newRendered := jsSplitLines(renderSample(RecallNote))
	// body lines + "", "---", "" — derived, not hardcoded.
	data := SectionData{
		SessionGoal:        []string{"Patch pi-vcc so the summary tail always tells the model to recall the task book"},
		OutstandingContext: []string{"pi must be restarted for the patch to take effect"},
		FilesAndChanges:    []string{"src/core/format.ts: appended one sentence to RECALL_NOTE"},
		Commits:            []string{},
		UserPreferences:    []string{"answer in Chinese"},
		TrackedCommands:    []string{"git: status --porcelain"},
		BriefTranscript:    "user: append the sentence\nassistant: done",
	}
	bodyLines := len(jsSplitLines(WrapLongLinesDefault(FormatSummary(data, FormatSummaryOptions{}))))
	head := bodyLines + 3
	if head > len(oldRendered) || head > len(newRendered) {
		t.Fatalf("head %d exceeds the rendered lengths %d/%d", head, len(oldRendered), len(newRendered))
	}
	oldHead := strings.Join(oldRendered[:head], "\n")
	newHead := strings.Join(newRendered[:head], "\n")
	if oldHead != newHead {
		t.Fatalf("the leading %d lines differ\nold:\n%s\nnew:\n%s", head, oldHead, newHead)
	}
}

// TestVerify_RecallNote_V4d pins V4d: the tail differs only by the appended
// sentence.
func TestVerify_RecallNote_V4d(t *testing.T) {
	data := SectionData{
		SessionGoal:        []string{"Patch pi-vcc so the summary tail always tells the model to recall the task book"},
		OutstandingContext: []string{"pi must be restarted for the patch to take effect"},
		FilesAndChanges:    []string{"src/core/format.ts: appended one sentence to RECALL_NOTE"},
		Commits:            []string{},
		UserPreferences:    []string{"answer in Chinese"},
		TrackedCommands:    []string{"git: status --porcelain"},
		BriefTranscript:    "user: append the sentence\nassistant: done",
	}
	bodyLines := len(jsSplitLines(WrapLongLinesDefault(FormatSummary(data, FormatSummaryOptions{}))))
	head := bodyLines + 3
	oldLines := jsSplitLines(renderSample(upstreamRecallNote))
	newLines := jsSplitLines(renderSample(RecallNote))
	oldTail := flat(strings.Join(oldLines[head:], " "))
	newTail := flat(strings.Join(newLines[head:], " "))
	if !strings.Contains(newTail, patchAdded) {
		t.Fatalf("the new tail lacks the appended sentence: %q", newTail)
	}
	if strings.Contains(oldTail, patchAdded) {
		t.Fatalf("the old tail already had the appended sentence: %q", oldTail)
	}
	if oldTail == "" {
		t.Fatal("the old tail is empty")
	}
}

// TestStripRecallNote_IdempotentAcrossWrapping pins the reason the patch only
// has to touch the constant: the strip regex is built from it, so a wrapped note
// is removed whole and a second strip is a no-op.
func TestStripRecallNote_IdempotentAcrossWrapping(t *testing.T) {
	summary := WrapLongLinesDefault("[Session Goal]\n- x" + patchSep + RecallNote)
	once := StripRecallNote(summary)
	if strings.Contains(once, "vcc_recall") {
		t.Fatalf("the note survived one strip:\n%s", once)
	}
	if twice := StripRecallNote(once); twice != once {
		t.Fatalf("stripping twice changed the text:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

// TestStripRecallNote_RemovesStackedCopies pins the cleanup of summaries the
// pre-fix format stacked the note into.
func TestStripRecallNote_RemovesStackedCopies(t *testing.T) {
	stacked := "[Session Goal]\n- x" + patchSep + RecallNote + patchSep + WrapLongLinesDefault(RecallNote)
	stripped := StripRecallNote(stacked)
	if strings.Contains(stripped, "vcc_recall") {
		t.Fatalf("a stacked note survived the strip:\n%s", stripped)
	}
	if !strings.Contains(stripped, "[Session Goal]") {
		t.Fatalf("the strip removed section content:\n%s", stripped)
	}
}
