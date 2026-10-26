package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/core/report.ts: every helper the report builds from, plus
// buildCompactReport() as a whole.

// reportFixture is a small session whose shape exercises every counter: a user
// goal, an assistant tool call naming a file, the tool's result and a final
// assistant line.
func reportFixture() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "Ship the recall-note patch and check the OAuth token refresh bug"},
		{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "I will check the auth flow."},
			map[string]any{"type": "toolCall", "name": "edit", "id": "call-1", "arguments": map[string]any{
				"path": "/root/workspace/pi-extensions/go/pi-vcc/format.go", "content": "package pivcc",
			}},
		}},
		{"role": "toolResult", "toolName": "edit", "toolCallId": "call-1", "content": "wrote 1 line"},
		{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "The token refresh still fails on expiry, see issue #28."},
		}},
	}
}

func TestReport_CountRolesAndBlocks(t *testing.T) {
	messages := reportFixture()
	roles := countRoles(messages)
	if roles.User != 1 || roles.Assistant != 2 || roles.ToolResult != 1 {
		t.Fatalf("roleCounts = %#v", roles)
	}
	blocks := countBlocks(messages)
	// The two assistant messages normalize to user + tool_call + tool_result +
	// assistant blocks.
	if blocks.User != 1 || blocks.Assistant != 2 || blocks.ToolCalls != 1 || blocks.ToolResults != 1 {
		t.Fatalf("blockCounts = %#v", blocks)
	}
}

func TestReport_CountRolesIgnoresUnknownRoles(t *testing.T) {
	counts := countRoles([]map[string]any{
		{"role": "system", "content": "x"},
		{"role": "custom", "content": "y"},
		{"role": "bashExecution", "command": "ls"},
	})
	if counts.User != 0 || counts.Assistant != 0 || counts.ToolResult != 0 {
		t.Fatalf("counts = %#v, want no known roles counted", counts)
	}
}

func TestReport_InputCharsUsesFullRendering(t *testing.T) {
	messages := reportFixture()
	want := 0
	for i, msg := range messages {
		want += jsLen(RenderMessage(msg, i, true).Summary)
	}
	if got := inputCharsOf(messages); got != want {
		t.Fatalf("inputCharsOf = %d, want %d", got, want)
	}
	if want == 0 {
		t.Fatal("the fixture must produce characters")
	}
}

func TestReport_TopFilesOrderAndCap(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "edit",
			"arguments": map[string]any{"path": "/a.go"}}}},
		// file_path is probed only when path is absent.
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "read",
			"arguments": map[string]any{"file_path": "/b.go"}}}},
		// A second call naming the same file does not repeat it.
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "edit",
			"arguments": map[string]any{"path": "/a.go"}}}},
		// A tool call with no file argument is skipped.
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "read",
			"arguments": map[string]any{"limit": 5}}}},
	}
	files := topFilesOf(messages)
	if len(files) != 2 || files[0] != "/a.go" || files[1] != "/b.go" {
		t.Fatalf("topFilesOf = %#v, want [/a.go /b.go]", files)
	}

	// Twelve distinct files: the cap keeps the first ten.
	many := make([]map[string]any, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "toolCall", "name": "edit",
				"arguments": map[string]any{"path": "/f" + jsNumberString(float64(i)) + ".go"}},
		}})
	}
	capped := topFilesOf(many)
	if len(capped) != reportTopFilesLimit || capped[0] != "/f0.go" || capped[9] != "/f9.go" {
		t.Fatalf("capped top files = %#v, want the first %d", capped, reportTopFilesLimit)
	}
}

func TestReport_Preview(t *testing.T) {
	if got := previewOf(nil, reportEdgeCount); got != "(empty)" {
		t.Fatalf("previewOf(nil) = %q, want \"(empty)\"", got)
	}
	// Six entries: rendered whole, with no ellipsis.
	short := make([]map[string]any, 0, 6)
	for i := 0; i < 6; i++ {
		short = append(short, map[string]any{"role": "user", "content": "message " + jsNumberString(float64(i))})
	}
	preview := previewOf(short, reportEdgeCount)
	if strings.Contains(preview, "...") {
		t.Fatalf("a short preview must not be elided:\n%s", preview)
	}
	if !strings.Contains(preview, "#0 [user] message 0") || !strings.Contains(preview, "#5 [user] message 5") {
		t.Fatalf("a short preview lost its edges:\n%s", preview)
	}

	// Nine entries: the first three, an ellipsis, then the last three.
	long := make([]map[string]any, 0, 9)
	for i := 0; i < 9; i++ {
		long = append(long, map[string]any{"role": "user", "content": "entry " + jsNumberString(float64(i))})
	}
	preview = previewOf(long, reportEdgeCount)
	lines := jsSplitLines(preview)
	if len(lines) != reportEdgeCount*2+1 {
		t.Fatalf("elided preview has %d lines, want %d:\n%s", len(lines), reportEdgeCount*2+1, preview)
	}
	if lines[reportEdgeCount] != "..." {
		t.Fatalf("the middle line is %q, want \"...\"", lines[reportEdgeCount])
	}
	if !strings.HasPrefix(lines[0], "#0 [user] entry 0") {
		t.Fatalf("first line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "#8 [user] entry 8") {
		t.Fatalf("last line = %q", lines[len(lines)-1])
	}
}

func TestReport_PreviewClipsLongSummaries(t *testing.T) {
	long := strings.Repeat("x", 500)
	line := previewLine(RenderedEntry{Index: 0, Role: "user", Summary: long})
	// "#0 [user] " is 10 units, so the clipped summary must fit in 220.
	if jsLen(line) > 10+reportPreviewClip {
		t.Fatalf("preview line is %d units, over the 220-char clip", jsLen(line))
	}
	if !strings.HasPrefix(line, "#0 [user] ") {
		t.Fatalf("line lost its index and role: %q", line)
	}
}

func TestReport_SectionCountOf(t *testing.T) {
	summary := "[Early Session Requests]\n- x\n[Files And Changes]\nModified: a.go\n[User Messages]\n- y"
	if got := sectionCountOf(summary); got != 3 {
		t.Fatalf("sectionCountOf = %d, want 3", got)
	}
	// A header that is only mentioned in the text does not count: the test is
	// for the `[Header]` anchor.
	if got := sectionCountOf("Early Session Requests and Commits"); got != 0 {
		t.Fatalf("sectionCountOf = %d for an anchorless summary, want 0", got)
	}
	if got := sectionCountOf(""); got != 0 {
		t.Fatalf("sectionCountOf(\"\") = %d, want 0", got)
	}
}

func TestReport_BriefLineCountOf(t *testing.T) {
	if got := briefLineCountOf("[Early Session Requests]\n- x"); got != 0 {
		t.Fatalf("briefLineCountOf = %d for a summary with no separator, want 0", got)
	}
	summary := "[Early Session Requests]\n- x" + reportBriefSeparator + "user: one\nassistant: two\nuser: three"
	if got := briefLineCountOf(summary); got != 3 {
		t.Fatalf("briefLineCountOf = %d, want 3", got)
	}
	// An empty brief transcript counts as no brief at all: TS's `brief ? ... : 0`.
	if got := briefLineCountOf("[Early Session Requests]\n- x" + reportBriefSeparator); got != 0 {
		t.Fatalf("briefLineCountOf = %d for an empty brief, want 0", got)
	}
}

func TestReport_QueryTermsOfAndQueryOf(t *testing.T) {
	terms := queryTermsOf("fix the OAuth token refresh bug in auth.ts (v2)")
	want := []string{"fix", "the", "OAuth", "token", "refresh", "bug", "auth.ts"}
	if len(terms) != len(want) {
		t.Fatalf("queryTermsOf = %#v, want %#v", terms, want)
	}
	for i := range want {
		if terms[i] != want[i] {
			t.Fatalf("queryTermsOf[%d] = %q, want %q (all: %#v)", i, terms[i], want[i], terms)
		}
	}
	// Two-letter words are below the {3,} minimum.
	if got := queryTermsOf("go to"); len(got) != 0 {
		t.Fatalf("queryTermsOf dropped the minimum: %#v", got)
	}
	// Only the first six terms survive.
	if got := queryTermsOf("alpha beta gamma delta epsilon zeta eta theta iota"); len(got) != 9 {
		t.Fatalf("queryTermsOf = %d terms, want 9", len(got))
	}
	if got := queryOf("alpha beta gamma delta epsilon zeta eta theta iota"); got != "alpha beta gamma delta epsilon zeta" {
		t.Fatalf("queryOf = %q, want the first six terms", got)
	}
	if got := queryOf("  "); got != "" {
		t.Fatalf("queryOf(blank) = %q, want \"\"", got)
	}
}

func TestReport_MatchesQuery(t *testing.T) {
	if !matchesQuery("The OAuth Token Refresh bug", "token refresh") {
		t.Fatal("a case-insensitive match failed")
	}
	if matchesQuery("The OAuth Token Refresh bug", "token signing") {
		t.Fatal("a missing term matched")
	}
	// Every term must appear, not just one.
	if matchesQuery("alpha beta", "alpha gamma") {
		t.Fatal("a partial match matched")
	}
	// An empty query matches everything, as JS's every() on an empty array.
	if !matchesQuery("anything", "") {
		t.Fatal("an empty query should match")
	}
	if !matchesQuery("anything", "   ") {
		t.Fatal("a whitespace query should match")
	}
}

func TestReport_CompressionRatio(t *testing.T) {
	if got := compressionRatio(100, 0); got != 0 {
		t.Fatalf("ratio with an empty summary = %v, want 0", got)
	}
	if got := compressionRatio(1000, 250); got != 4 {
		t.Fatalf("ratio = %v, want 4", got)
	}
	// toFixed(2) rounds, then Number() drops the trailing zero.
	if got := compressionRatio(10, 3); got != 3.33 {
		t.Fatalf("ratio = %v, want 3.33", got)
	}
	if got := compressionRatio(5, 4); got != 1.25 {
		t.Fatalf("ratio = %v, want 1.25 (no trailing zero)", got)
	}
}

func TestReport_FirstToolCallFile(t *testing.T) {
	blocks := Normalize(reportFixture(), nil)
	if got := firstToolCallFile(blocks); got != "/root/workspace/pi-extensions/go/pi-vcc/format.go" {
		t.Fatalf("firstToolCallFile = %q", got)
	}
	// A tool call naming an empty string is skipped in favour of the next one.
	skipped := Normalize([]map[string]any{
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "edit",
			"arguments": map[string]any{"path": ""}}}},
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "name": "edit",
			"arguments": map[string]any{"path": "/next.go"}}}},
	}, nil)
	if got := firstToolCallFile(skipped); got != "/next.go" {
		t.Fatalf("firstToolCallFile = %q, want /next.go", got)
	}
	// No tool call at all: no file.
	if got := firstToolCallFile(Normalize([]map[string]any{{"role": "user", "content": "hi"}}, nil)); got != "" {
		t.Fatalf("firstToolCallFile = %q, want \"\"", got)
	}
}

func TestReport_ProbesOf(t *testing.T) {
	messages := reportFixture()
	summary := Compile(CompileInput{Messages: messages})
	probes := probesOf(messages, summary)
	labels := make([]string, 0, len(probes))
	for _, probe := range probes {
		labels = append(labels, probe.Label)
		if probe.SourceText == "" {
			t.Fatalf("probe %q has an empty source text", probe.Label)
		}
		if probe.Query == "" {
			t.Fatalf("probe %q derived no query", probe.Label)
		}
	}
	if len(labels) == 0 {
		t.Fatal("no probes were produced")
	}
	if labels[0] != "request" {
		t.Fatalf("probe order = %v, want the request first", labels)
	}
	// The goal line survives the summary, so the goal probe is mentioned.
	for _, probe := range probes {
		if probe.Label == "request" && !probe.SummaryMentioned {
			t.Fatalf("the request probe is not mentioned by the summary:\n%s", summary)
		}
	}
}

func TestReport_ProbesOfDropsEmptySources(t *testing.T) {
	// A session with no goal, no file and no problem yields no probes at all.
	messages := []map[string]any{
		{"role": "user", "content": "ok"},
		{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}},
	}
	if probes := probesOf(messages, Compile(CompileInput{Messages: messages})); len(probes) != 0 {
		t.Fatalf("probes = %#v, want none", probes)
	}
}

func TestReport_BuildCompactReport(t *testing.T) {
	messages := reportFixture()
	report := BuildCompactReport(CompileInput{Messages: messages})

	if report.Summary == "" {
		t.Fatal("the report has no summary")
	}
	if report.Before.MessageCount != len(messages) || report.Compression.MessagesBefore != len(messages) {
		t.Fatalf("message counts disagree: before=%d compression=%d", report.Before.MessageCount, report.Compression.MessagesBefore)
	}
	if report.Before.InputChars <= 0 || report.Before.InputChars != report.Compression.CharsBefore {
		t.Fatalf("input chars: before=%d compression=%d", report.Before.InputChars, report.Compression.CharsBefore)
	}
	if report.After.SummaryLength != jsLen(report.Summary) || report.After.SummaryLength != report.Compression.CharsAfter {
		t.Fatalf("summary length: after=%d compression=%d", report.After.SummaryLength, report.Compression.CharsAfter)
	}
	if report.After.SummaryPreview != report.Summary {
		t.Fatal("the summary preview is not the summary")
	}
	// The compactor's estimate is ceil(chars/4).
	if want := (report.Before.InputChars + 3) / 4; report.Before.EstimatedTokens != want {
		t.Fatalf("estimated tokens = %d, want %d", report.Before.EstimatedTokens, want)
	}
	if want := (report.After.SummaryLength + 3) / 4; report.After.EstimatedTokens != want {
		t.Fatalf("estimated tokens = %d, want %d", report.After.EstimatedTokens, want)
	}
	// The fixture names one file and carries at least one section.
	if len(report.Before.TopFiles) != 1 || report.Before.TopFiles[0] != "/root/workspace/pi-extensions/go/pi-vcc/format.go" {
		t.Fatalf("top files = %#v", report.Before.TopFiles)
	}
	if report.After.SectionCount < 1 {
		t.Fatalf("section count = %d, want at least one header", report.After.SectionCount)
	}
	// A compiled summary with a brief transcript reports its line count.
	if report.After.BriefTranscriptLines < 1 {
		t.Fatalf("brief transcript lines = %d, want at least one", report.After.BriefTranscriptLines)
	}
	// The ratio is charsBefore/charsAfter rounded to two decimals.
	if want := compressionRatio(report.Compression.CharsBefore, report.Compression.CharsAfter); report.Compression.Ratio != want {
		t.Fatalf("ratio = %v, want %v", report.Compression.Ratio, want)
	}
	if report.Compression.Ratio <= 0 {
		t.Fatalf("ratio = %v, want a positive compression factor", report.Compression.Ratio)
	}
	// The preview is non-empty for a non-empty session.
	if report.Before.Preview == "" || report.Before.Preview == "(empty)" {
		t.Fatalf("preview = %q", report.Before.Preview)
	}
}

func TestReport_BuildCompactReportEmptySession(t *testing.T) {
	report := BuildCompactReport(CompileInput{})
	if report.Before.MessageCount != 0 {
		t.Fatalf("message count = %d, want 0", report.Before.MessageCount)
	}
	if report.Before.Preview != "(empty)" {
		t.Fatalf("preview = %q, want \"(empty)\"", report.Before.Preview)
	}
	if len(report.Before.TopFiles) != 0 {
		t.Fatalf("top files = %#v, want none", report.Before.TopFiles)
	}
	if report.Compression.CharsBefore != 0 {
		t.Fatalf("chars before = %d, want 0", report.Compression.CharsBefore)
	}
	// An empty session still compiles to a summary (the note and the headers),
	// so the ratio is the summary's own length against zero input.
	if report.After.SummaryLength != jsLen(report.Summary) {
		t.Fatalf("summary length = %d, want %d", report.After.SummaryLength, jsLen(report.Summary))
	}
}
