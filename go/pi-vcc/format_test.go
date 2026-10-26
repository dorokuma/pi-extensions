package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/core/format.ts: capItems, wrapLongLines, capBrief and
// formatSummary's section assembly.

func TestCapItems(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	tests := []struct {
		name string
		want string
	}{
		{"keeps the newest with an earlier marker", "(+2 earlier) c, d, e"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CapItems(items, 3, ", "); got != test.want {
				t.Fatalf("CapItems = %q, want %q", got, test.want)
			}
		})
	}
	if got := CapItems(items[:2], 3, ", "); got != "a, b" {
		t.Fatalf("CapItems under the limit = %q", got)
	}
	if got := CapItems(items, 5, ", "); got != "a, b, c, d, e" {
		t.Fatalf("CapItems at the limit = %q", got)
	}
	// The overflow count is the number of dropped items, and the separator is the
	// caller's (tracked commands join with " | ").
	if got := CapItems(items, 4, " | "); got != "(+1 earlier) b | c | d | e" {
		t.Fatalf("CapItems(items, 4, \" | \") = %q", got)
	}
}

func TestStripCapMarker(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"a, b (+2 more)", "a, b"},
		{"(+2 earlier) a, b", "a, b"},
		{"  (+3 earlier) x", "x"},
		{"plain", "plain"},
		{"a (+0 more)", "a"},
	}
	for _, test := range tests {
		if got := StripCapMarker(test.in); got != test.want {
			t.Fatalf("StripCapMarker(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestWrapLongLines_ShortLinesPassThrough(t *testing.T) {
	text := "short line\nanother"
	if got := WrapLongLinesDefault(text); got != text {
		t.Fatalf("WrapLongLinesDefault changed short text: %q", got)
	}
}

func TestWrapLongLines_Wraps(t *testing.T) {
	line := strings.Repeat("word ", 40)
	wrapped := WrapLongLines(line, 40)
	lines := jsSplitLines(wrapped)
	if len(lines) < 2 {
		t.Fatalf("expected a wrap, got %d line(s)", len(lines))
	}
	for i, part := range lines {
		if jsLen(part) > 40 && i > 0 {
			t.Fatalf("line %d is %d units, over the 40-unit width: %q", i, jsLen(part), part)
		}
	}
	if joined := strings.Join(strings.Fields(wrapped), " "); joined != jsTrimSpace(line) {
		t.Fatalf("wrapping lost or added words:\njoined: %q\nwant:   %q", joined, jsTrimSpace(line))
	}
}

func TestWrapLongLines_IndentedContinuation(t *testing.T) {
	line := "- " + strings.Repeat("x", 130)
	wrapped := WrapLongLines(line, 60)
	lines := jsSplitLines(wrapped)
	if len(lines) < 2 {
		t.Fatalf("expected a wrap, got %d line(s)", len(lines))
	}
	if !strings.HasPrefix(lines[1], "  ") {
		t.Fatalf("continuation line has no indent: %q", lines[1])
	}
}

func TestCapBrief(t *testing.T) {
	lines := make([]string, 0, 130)
	lines = append(lines, "- request")
	for i := 0; i < 130; i++ {
		lines = append(lines, "line "+jsNumberString(float64(i)))
	}
	got := CapBrief(strings.Join(lines, "\n"))
	if !strings.HasPrefix(got, "...(") {
		t.Fatalf("capBrief did not add the omission marker: %q", jsSlice(got, 0, 40))
	}
	if jsLen(strings.Join(jsSplitLines(got), "\n")) >= jsLen(strings.Join(lines, "\n")) {
		t.Fatal("capBrief did not shorten the text")
	}
	if got := CapBrief("a\nb"); got != "a\nb" {
		t.Fatalf("capBrief changed short text: %q", got)
	}
}

// TestCapBrief_CutsAtASectionHeader pins the `firstHeader > 0` rule: when the
// kept window opens mid-section, the cut moves forward to the first header so a
// section is never presented without its title.
func TestCapBrief_CutsAtASectionHeader(t *testing.T) {
	lines := make([]string, 0, 200)
	lines = append(lines, "[User Messages]", "- old request")
	for i := 0; i < 100; i++ {
		lines = append(lines, "filler "+jsNumberString(float64(i)))
	}
	lines = append(lines, "[Commits]", "- abc: fix")
	for i := 0; i < 30; i++ {
		lines = append(lines, "tail "+jsNumberString(float64(i)))
	}
	got := CapBrief(strings.Join(lines, "\n"))
	body := got
	if idx := strings.Index(got, "\n\n"); idx >= 0 {
		body = got[idx+2:]
	}
	if !strings.HasPrefix(body, "[Commits]") {
		t.Fatalf("capBrief should cut at the first header in the kept window, got %q", jsSlice(body, 0, 40))
	}
}

func TestFormatSummary_SectionsAndOrder(t *testing.T) {
	data := SectionData{
		EarlyRequests:   []string{"fix auth"},
		FilesAndChanges: []string{"Modified: a.go"},
		Commits:         []string{"abc1234: fix"},
		TrackedCommands: []string{"git: status"},
		UserMessages:    []string{"then run the tests"},
		BriefTranscript: "[user]\nhello",
	}
	got := FormatSummary(data, FormatSummaryOptions{})
	wantOrder := []string{
		"[Early Session Requests]", "[Files And Changes]", "[Commits]", "[Tracked Commands]",
		"[User Messages]",
	}
	lastIdx := -1
	for _, header := range wantOrder {
		idx := strings.Index(got, header)
		if idx < 0 {
			t.Fatalf("summary is missing %s:\n%s", header, got)
		}
		if idx < lastIdx {
			t.Fatalf("%s is out of order:\n%s", header, got)
		}
		lastIdx = idx
	}
	// The 0.10.2 sections are gone; an old [Session Goal] line in the brief
	// transcript is transcript text, not a section.
	for _, gone := range []string{"[Session Goal]", "[Outstanding Context]", "[User Preferences]"} {
		if strings.Contains(got, gone) {
			t.Fatalf("the summary still renders %s:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "\n\n---\n\n") {
		t.Fatalf("summary has no separator before the brief:\n%s", got)
	}
	// RECALL_NOTE is compile()'s job, not formatSummary's.
	if strings.Contains(got, "vcc_recall") {
		t.Fatalf("formatSummary appended RECALL_NOTE:\n%s", got)
	}
}

func TestFormatSummary_EmptySectionsAreOmitted(t *testing.T) {
	got := FormatSummary(SectionData{EarlyRequests: []string{"x"}}, FormatSummaryOptions{})
	if strings.Contains(got, "[Commits]") {
		t.Fatalf("an empty section was rendered:\n%s", got)
	}
	if !strings.HasPrefix(got, "[Early Session Requests]\n- x") {
		t.Fatalf("unexpected summary: %q", got)
	}
}

func TestFormatSummary_EmptyIsEmpty(t *testing.T) {
	if got := FormatSummary(SectionData{}, FormatSummaryOptions{}); got != "" {
		t.Fatalf("empty data rendered %q", got)
	}
}

func TestFormatSummary_CapBriefTranscriptOption(t *testing.T) {
	lines := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		lines = append(lines, "line "+jsNumberString(float64(i)))
	}
	data := SectionData{EarlyRequests: []string{"goal"}, BriefTranscript: strings.Join(lines, "\n")}
	capped := FormatSummary(data, FormatSummaryOptions{})
	uncapped := FormatSummary(data, FormatSummaryOptions{CapBriefTranscript: boolPtr(false)})
	if jsLen(capped) >= jsLen(uncapped) {
		t.Fatalf("capBriefTranscript=false did not keep more text (%d vs %d)", jsLen(uncapped), jsLen(capped))
	}
	if !strings.Contains(capped, "earlier lines omitted") {
		t.Fatal("the capped rendering has no omission marker")
	}
	if strings.Contains(uncapped, "earlier lines omitted") {
		t.Fatal("the uncapped rendering has an omission marker")
	}
}

func boolPtr(value bool) *bool { return &value }

// TestJsFixed1 pins the token-count formatting used by the stats strings.
func TestJsFixed1(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{1.2, "1.2"},
		{1, "1.0"},
		{12.34, "12.3"},
		{0.05, "0.1"},
	}
	for _, test := range tests {
		if got := jsFixed1(test.in); got != test.want {
			t.Fatalf("jsFixed1(%v) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestJsNumberString(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{12, "12"},
		{12.5, "12.5"},
		{-3, "-3"},
	}
	for _, test := range tests {
		if got := jsNumberString(test.in); got != test.want {
			t.Fatalf("jsNumberString(%v) = %q, want %q", test.in, got, test.want)
		}
	}
}
