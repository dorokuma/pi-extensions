package pivcc

// Ports src/core/report.ts: the compaction report buildCompactReport() builds.
//
// Nothing in the extension calls it at runtime — index.ts registers the hook,
// the two commands and the recall tool only. report.ts is the package's
// diagnostic surface (the shape a "what did this compaction do" dump takes), so
// the port keeps it complete and behaviour-identical, covered by report_test.go.

import (
	"strconv"
	"strings"
)

// reportSectionHeaders is report.ts SECTION_HEADERS: the four headers a
// pi-vcc summary always renders in this order.
var reportSectionHeaders = []string{"Session Goal", "Files And Changes", "Commits", "Outstanding Context"}

// reportBriefSeparator is the `\n\n---\n\n` report.ts splits on to find the
// brief transcript. summarize.go carries the same constant as
// summarySeparator; the port keeps a local name so the two stay independent.
const reportBriefSeparator = "\n\n---\n\n"

// reportEdgeCount is previewOf()'s default edgeCount: how many entries each end
// of the preview keeps.
const reportEdgeCount = 3

// reportProbeLimit is queryOf()'s `terms.slice(0, 6)`.
const reportProbeLimit = 6

// reportTopFilesLimit is topFilesOf()'s `slice(0, 10)`.
const reportTopFilesLimit = 10

// reportPreviewClip is previewOf()'s `clip(entry.summary, 220)`.
const reportPreviewClip = 220

// reportFilePathKeys is the argument key order both topFilesOf() and probesOf()
// probe for a tool call's file.
var reportFilePathKeys = []string{"path", "file_path", "filePath", "file"}

// RoleCounts is report.ts RoleCounts: the raw message roles, counted from
// `msg.role`.
type RoleCounts struct {
	User       int
	Assistant  int
	ToolResult int
}

// BlockCounts is report.ts BlockCounts: the normalized blocks, counted by kind.
type BlockCounts struct {
	User        int
	Assistant   int
	ToolCalls   int
	ToolResults int
}

// RecallProbe is report.ts RecallProbe: one "can the model still find this?"
// check — a line of the session, the query derived from it, whether the summary
// already mentions it, and how many entries a recall of that query returns.
type RecallProbe struct {
	Label            string
	SourceText       string
	Query            string
	SummaryMentioned bool
	RecallHits       int
}

// CompactReportBefore is report.ts CompactReport["before"]: what the session
// looked like before the compaction.
type CompactReportBefore struct {
	MessageCount    int
	RoleCounts      RoleCounts
	BlockCounts     BlockCounts
	InputChars      int
	EstimatedTokens int
	TopFiles        []string
	Preview         string
}

// CompactReportAfter is report.ts CompactReport["after"]: what the compaction
// produced.
type CompactReportAfter struct {
	SummaryLength        int
	EstimatedTokens      int
	SectionCount         int
	SummaryPreview       string
	GoalsCount           int
	BlockersCount        int
	BriefTranscriptLines int
}

// CompactReportCompression is report.ts CompactReport["compression"].
type CompactReportCompression struct {
	CharsBefore    int
	CharsAfter     int
	Ratio          float64
	MessagesBefore int
}

// CompactReportRecall is report.ts CompactReport["recall"].
type CompactReportRecall struct {
	Probes []RecallProbe
}

// CompactReport is report.ts CompactReport.
type CompactReport struct {
	Summary     string
	Before      CompactReportBefore
	After       CompactReportAfter
	Compression CompactReportCompression
	Recall      CompactReportRecall
}

// countRoles is report.ts countRoles().
func countRoles(messages []map[string]any) RoleCounts {
	counts := RoleCounts{}
	for _, msg := range messages {
		switch messageRole(msg) {
		case "user":
			counts.User++
		case "assistant":
			counts.Assistant++
		case "toolResult":
			counts.ToolResult++
		}
	}
	return counts
}

// countBlocks is report.ts countBlocks().
func countBlocks(messages []map[string]any) BlockCounts {
	counts := BlockCounts{}
	for _, block := range Normalize(messages, nil) {
		switch block.Kind {
		case KindUser:
			counts.User++
		case KindAssistant:
			counts.Assistant++
		case KindToolCall:
			counts.ToolCalls++
		case KindToolResult:
			counts.ToolResults++
		}
	}
	return counts
}

// inputCharsOf is report.ts inputCharsOf(): the total length of every message's
// full (unclipped) rendering.
func inputCharsOf(messages []map[string]any) int {
	total := 0
	for i, msg := range messages {
		total += jsLen(RenderMessage(msg, i, true).Summary)
	}
	return total
}

// topFilesOf is report.ts topFilesOf(): the files named by tool calls, in first
// appearance order, capped at ten.
func topFilesOf(messages []map[string]any) []string {
	files := newOrderedSet()
	for _, block := range Normalize(messages, nil) {
		if block.Kind != KindToolCall {
			continue
		}
		for _, key := range reportFilePathKeys {
			if value, ok := block.Args[key].(string); ok {
				files.Add(value)
				break
			}
		}
	}
	items := files.Items()
	if len(items) > reportTopFilesLimit {
		items = items[:reportTopFilesLimit]
	}
	return items
}

// previewLine is one preview entry's `#N [role] clipped summary`.
func previewLine(entry RenderedEntry) string {
	return "#" + jsNumberString(float64(entry.Index)) + " [" + entry.Role + "] " + Clip(entry.Summary, reportPreviewClip)
}

// previewOf is report.ts previewOf(): the first and last few entries, with an
// ellipsis between them.
func previewOf(messages []map[string]any, edgeCount int) string {
	rendered := make([]RenderedEntry, 0, len(messages))
	for i, msg := range messages {
		rendered = append(rendered, RenderMessage(msg, i, false))
	}
	if len(rendered) == 0 {
		return "(empty)"
	}
	if len(rendered) <= edgeCount*2 {
		lines := make([]string, 0, len(rendered))
		for _, entry := range rendered {
			lines = append(lines, previewLine(entry))
		}
		return strings.Join(lines, "\n")
	}
	first := rendered[:edgeCount]
	last := rendered[len(rendered)-edgeCount:]
	lines := make([]string, 0, len(first)+len(last)+1)
	for _, entry := range first {
		lines = append(lines, previewLine(entry))
	}
	lines = append(lines, "...")
	for _, entry := range last {
		lines = append(lines, previewLine(entry))
	}
	return strings.Join(lines, "\n")
}

// sectionCountOf is report.ts sectionCountOf(): how many of the four headers
// the summary renders.
func sectionCountOf(summary string) int {
	count := 0
	for _, header := range reportSectionHeaders {
		if strings.Contains(summary, "["+header+"]") {
			count++
		}
	}
	return count
}

// briefLineCountOf is report.ts briefLineCountOf(): the brief transcript's line
// count, or 0 when the summary has no brief transcript at all.
func briefLineCountOf(summary string) int {
	idx := strings.Index(summary, reportBriefSeparator)
	if idx < 0 {
		return 0
	}
	return len(jsSplitLines(summary[idx+len(reportBriefSeparator):]))
}

// queryTermRe is report.ts `/[\p{L}\p{N}_./-]{3,}/gu`: the words a probe query
// is built from. The JS `u` flag changes nothing for this class, so the pattern
// carries over unchanged.
var queryTermRe = mustCompile(`[\p{L}\p{N}_./-]{3,}`)

// queryTermsOf is report.ts queryTermsOf().
func queryTermsOf(text string) []string {
	matches := queryTermRe.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		term := jsTrimSpace(match)
		if term == "" {
			continue
		}
		out = append(out, term)
	}
	return out
}

// queryOf is report.ts queryOf(): the first six terms, joined by spaces.
func queryOf(text string) string {
	terms := queryTermsOf(text)
	if len(terms) > reportProbeLimit {
		terms = terms[:reportProbeLimit]
	}
	return strings.Join(terms, " ")
}

// matchesQuery is report.ts matchesQuery(): every query term must appear.
//
// TS splits on /\s+/ and drops the empty fields, so all that matters is the run
// of whitespace-separated words; strings.FieldsFunc gives exactly that.
// jsSplitWhitespace is not used here: it keeps a trailing separator's slice
// where JS leaves an empty string, which a filter(Boolean) would drop anyway.
func matchesQuery(text, query string) bool {
	haystack := strings.ToLower(text)
	for _, term := range strings.FieldsFunc(strings.ToLower(query), jsIsSpaceRune) {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

// firstToolCallFile is the file the first tool call names — probesOf()'s "file"
// probe source. The key order is the same as topFilesOf()'s, but the first hit
// wins the probe outright, so a tool call naming an empty string keeps looking.
func firstToolCallFile(blocks []NormalizedBlock) string {
	for _, block := range blocks {
		if block.Kind != KindToolCall {
			continue
		}
		for _, key := range reportFilePathKeys {
			if value, ok := block.Args[key].(string); ok {
				if value != "" {
					return value
				}
				break
			}
		}
	}
	return ""
}

// reportFirstOf is report.ts's `data.sessionGoal[0] ?? ""`.
func reportFirstOf(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

// probesOf is report.ts probesOf(): one probe per non-empty source line.
func probesOf(messages []map[string]any, summary string) []RecallProbe {
	blocks := Normalize(messages, nil)
	data := BuildSections(BuildSectionsInput{Blocks: blocks})
	sources := []struct {
		label string
		text  string
	}{
		{"goal", reportFirstOf(data.SessionGoal)},
		{"file", firstToolCallFile(blocks)},
		{"problem", reportFirstOf(data.OutstandingContext)},
	}
	rendered := make([]RenderedEntry, 0, len(messages))
	for i, msg := range messages {
		rendered = append(rendered, RenderMessage(msg, i, false))
	}
	out := make([]RecallProbe, 0, len(sources))
	for _, source := range sources {
		sourceText := jsTrimSpace(source.text)
		query := queryOf(sourceText)
		if query == "" {
			continue
		}
		out = append(out, RecallProbe{
			Label:            source.label,
			SourceText:       sourceText,
			Query:            query,
			SummaryMentioned: matchesQuery(summary, query),
			RecallHits:       len(SearchEntries(rendered, messages, query)),
		})
	}
	return out
}

// compressionRatio is report.ts's `summary.length === 0 ? 0 :
// Number((inputChars / summary.length).toFixed(2))`.
func compressionRatio(inputChars, summaryChars int) float64 {
	if summaryChars == 0 {
		return 0
	}
	return jsNumberFromFixed2(float64(inputChars) / float64(summaryChars))
}

// jsNumberFromFixed2 is `Number((n).toFixed(2))`: two decimal places, parsed
// back into a number, so 12.50 reads as 12.5 the way JS's Number() leaves it.
func jsNumberFromFixed2(n float64) float64 {
	parsed, err := strconv.ParseFloat(strconv.FormatFloat(n, 'f', 2, 64), 64)
	if err != nil {
		return 0
	}
	return parsed
}

// BuildCompactReport is report.ts buildCompactReport(): compile the summary and
// describe what the compaction did.
func BuildCompactReport(input CompileInput) CompactReport {
	summary := Compile(input)
	data := BuildSections(BuildSectionsInput{Blocks: Normalize(input.Messages, nil)})
	inputChars := inputCharsOf(input.Messages)
	summaryChars := jsLen(summary)
	return CompactReport{
		Summary: summary,
		Before: CompactReportBefore{
			MessageCount:    len(input.Messages),
			RoleCounts:      countRoles(input.Messages),
			BlockCounts:     countBlocks(input.Messages),
			InputChars:      inputChars,
			EstimatedTokens: EstimateTokensFromChars(float64(inputChars), 0),
			TopFiles:        topFilesOf(input.Messages),
			Preview:         previewOf(input.Messages, reportEdgeCount),
		},
		After: CompactReportAfter{
			SummaryLength:        summaryChars,
			EstimatedTokens:      EstimateTokensFromChars(float64(summaryChars), 0),
			SectionCount:         sectionCountOf(summary),
			SummaryPreview:       summary,
			GoalsCount:           len(data.SessionGoal),
			BlockersCount:        len(data.OutstandingContext),
			BriefTranscriptLines: briefLineCountOf(summary),
		},
		Compression: CompactReportCompression{
			CharsBefore:    inputChars,
			CharsAfter:     summaryChars,
			Ratio:          compressionRatio(inputChars, summaryChars),
			MessagesBefore: len(input.Messages),
		},
		Recall: CompactReportRecall{Probes: probesOf(input.Messages, summary)},
	}
}
