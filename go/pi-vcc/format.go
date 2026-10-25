package pivcc

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Ports src/core/format.ts (with the pi-vcc-0.10.0-recall-note patch applied):
// section rendering, capping, wrapping and the summary's RECALL_NOTE tail.

// section is format.ts section(): a titled block of "- item" lines, or "" when
// there is nothing to report.
func section(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, "- "+item)
	}
	return "[" + title + "]\n" + strings.Join(lines, "\n")
}

// CapItems is format.ts capItems(): join up to limit items, naming the overflow.
// keep "head" keeps the oldest and ends with "(+N more)"; "tail" keeps the
// newest and starts with "(+N earlier)".
func CapItems(items []string, limit int, joinWith string, keep string) string {
	if len(items) <= limit {
		return strings.Join(items, joinWith)
	}
	over := len(items) - limit
	if keep == "tail" {
		return "(+" + jsNumberString(float64(over)) + " earlier) " + strings.Join(items[len(items)-limit:], joinWith)
	}
	return strings.Join(items[:limit], joinWith) + " (+" + jsNumberString(float64(over)) + " more)"
}

// capMarkerHeadRe and capMarkerTailRe are the two halves of
// format.ts stripCapMarker.
var (
	capMarkerHeadRe = mustCompile(`^` + jsSpace + `*\(\+\d+ earlier\)` + jsSpace + `*`)
	capMarkerTailRe = mustCompile(jsSpace + `*\(\+\d+ (?:more|earlier)\)` + jsSpace + `*$`)
)

// StripCapMarker is format.ts stripCapMarker(): the inverse of CapItems, used
// when a merge re-reads a capped line.
func StripCapMarker(text string) string {
	out := capMarkerHeadRe.ReplaceAllString(text, "")
	return capMarkerTailRe.ReplaceAllString(out, "")
}

// BriefMaxLines is format.ts BRIEF_MAX_LINES.
const BriefMaxLines = 120

// tuiSafeLineChars is format.ts TUI_SAFE_LINE_CHARS.
const tuiSafeLineChars = 120

// indentRe is the leading-indent capture of format.ts wrapLine:
// /^\s*(?:[-*]\s+|\d+\.\s+)?/.
var indentRe = mustCompile(`^` + jsSpace + `*(?:[-*]` + jsSpace + `+|\d+\.` + jsSpace + `+)?`)

// wrapLine is format.ts wrapLine(): wrap one line to maxChars, indenting
// continuations under the original indent.
func wrapLine(line string, maxChars int) []string {
	if jsLen(line) <= maxChars {
		return []string{line}
	}

	indent := indentRe.FindString(line)
	continuationIndent := ""
	if indent != "" {
		width := jsLen(indent)
		if width > 8 {
			width = 8
		}
		continuationIndent = jsRepeat(" ", width)
	}
	wrapped := make([]string, 0, 2)
	remaining := line
	prefix := ""

	for jsLen(prefix)+jsLen(remaining) > maxChars {
		available := maxChars - jsLen(prefix)
		if available < 20 {
			available = 20
		}
		splitAt := jsLastIndexByteSpaceBefore(remaining, available)
		if float64(splitAt) < float64(available)*0.5 {
			splitAt = available
		}

		wrapped = append(wrapped, prefix+jsTrimRightSpace(jsSlice(remaining, 0, splitAt)))
		remaining = jsTrimLeftSpace(jsSlice(remaining, splitAt, jsLen(remaining)))
		prefix = continuationIndent
	}

	if remaining != "" {
		wrapped = append(wrapped, prefix+remaining)
	}
	return wrapped
}

// WrapLongLines is format.ts wrapLongLines().
func WrapLongLines(text string, maxChars int) string {
	lines := jsSplitLines(text)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, wrapLine(line, maxChars)...)
	}
	return strings.Join(out, "\n")
}

// WrapLongLinesDefault is wrapLongLines(text) at the TUI width.
func WrapLongLinesDefault(text string) string {
	return WrapLongLines(text, tuiSafeLineChars)
}

// headerLineRe is format.ts's `/^\[.+\]/` (JS `.` excludes line terminators).
var headerLineRe = mustCompile(`^\[` + jsDot + `+\]`)

// CapBrief is format.ts capBrief(): keep the last BRIEF_MAX_LINES lines, cut at
// the first section header so a section is never split.
func CapBrief(text string) string {
	lines := jsSplitLines(text)
	if len(lines) <= BriefMaxLines {
		return text
	}
	omitted := len(lines) - BriefMaxLines
	kept := lines[len(lines)-BriefMaxLines:]
	firstHeader := -1
	for i, line := range kept {
		if headerLineRe.MatchString(line) {
			firstHeader = i
			break
		}
	}
	clean := kept
	if firstHeader > 0 {
		clean = kept[firstHeader:]
	}
	return "...(" + jsNumberString(float64(omitted)) + " earlier lines omitted)\n\n" + strings.Join(clean, "\n")
}

// RecallNote is format.ts RECALL_NOTE **with the pi-vcc-0.10.0-recall-note
// patch applied**: the upstream sentence followed, unchanged, by the sentence
// that tells the model to pull this task's original instructions back after any
// compaction. The constant is byte-identical to the patched TS value; the test
// TestRecallNoteExactValue pins it.
const RecallNote = "Use `vcc_recall` to search for prior work, decisions, and context from before this summary. " +
	"Do not redo work already completed." +
	" After any compaction, first call vcc_recall to pull this task's original instructions " +
	"(the first user message) back before continuing; never judge whether it is needed."

// FormatSummaryOptions is format.ts FormatSummaryOptions.
type FormatSummaryOptions struct {
	// CapBriefTranscript defaults to true.
	CapBriefTranscript *bool
}

// FormatSummary is format.ts formatSummary(). RECALL_NOTE is intentionally NOT
// appended here: compile() appends it once, after the merge with the previous
// summary, so the note never compounds inside the brief transcript.
func FormatSummary(data SectionData, options FormatSummaryOptions) string {
	capBriefTranscript := true
	if options.CapBriefTranscript != nil {
		capBriefTranscript = *options.CapBriefTranscript
	}
	headerParts := make([]string, 0, 6)
	for _, part := range []string{
		section("Session Goal", data.SessionGoal),
		section("Files And Changes", data.FilesAndChanges),
		section("Commits", data.Commits),
		section("Tracked Commands", data.TrackedCommands),
		section("Outstanding Context", data.OutstandingContext),
		section("User Preferences", data.UserPreferences),
	} {
		if part != "" {
			headerParts = append(headerParts, part)
		}
	}

	parts := make([]string, 0, 2)
	if len(headerParts) > 0 {
		parts = append(parts, strings.Join(headerParts, "\n\n"))
	}
	if data.BriefTranscript != "" {
		if capBriefTranscript {
			parts = append(parts, CapBrief(data.BriefTranscript))
		} else {
			parts = append(parts, data.BriefTranscript)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return WrapLongLinesDefault(strings.Join(parts, "\n\n---\n\n"))
}

// ── ordered JSON helpers for settings scaffolding ───────────────────────────

// quoteJSONString is `JSON.stringify(s)` for a string.
func quoteJSONString(s string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// indentJSON renders a JSON value 2-space indented at the given depth, the way
// `JSON.stringify(value, null, 2)` nests inside an already-open brace.
func indentJSON(value any, depth int) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "null"
	}
	body := strings.TrimSuffix(buf.String(), "\n")
	// Re-indent continuation lines under the enclosing brace.
	lines := strings.Split(body, "\n")
	if len(lines) > 1 {
		pad := strings.Repeat("  ", depth)
		for i := 1; i < len(lines); i++ {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
