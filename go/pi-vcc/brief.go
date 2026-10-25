package pivcc

import (
	"strings"
	"unicode"
)

// Ports src/core/brief.ts: the compressed transcript that follows the sections,
// plus the bash-command compression and heredoc detection that rank.go shares.

const (
	truncateUser                     = 256
	segmentClosingAssistantHeadWords = 120
	segmentClosingAssistantTailWords = 120
	assistantHeadWords               = 80
	assistantTailWords               = 120
)

// selfTalkPrefixRe is brief.ts SELF_TALK_PREFIX_RE:
// /^\s*(?:hmm|wait|actually|oh|okay|ok|well|so)[,.!\s-]+/i.
//
// jsSpace is itself a character class, so it cannot be pasted inside the
// trailing `[,.!\s-]+` class: the class is written out explicitly instead.
var selfTalkPrefixRe = compileIgnoreCase(`^` + jsSpace +
	`*(?:hmm|wait|actually|oh|okay|ok|well|so)[,.!` + jsSpaceChars + `-]+`)

// isNoiseUser is brief.ts isNoiseUser().
func isNoiseUser(text string) bool {
	return jsTrimSpace(text) == ""
}

// stopWords is brief.ts STOP_WORDS.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
	"will": true, "would": true, "could": true,
	"should": true, "may": true, "might": true, "shall": true, "can": true, "need": true, "must": true,
	"to": true, "of": true, "in": true, "for": true, "on": true, "with": true, "at": true, "by": true,
	"from": true, "as": true,
	"into": true, "through": true, "during": true, "before": true, "after": true, "above": true,
	"below":   true,
	"between": true, "under": true, "over": true,
	"and": true, "but": true, "or": true, "nor": true, "not": true, "so": true, "yet": true,
	"both": true, "either": true,
	"neither": true, "each": true, "every": true, "all": true, "any": true, "few": true, "more": true,
	"most":  true,
	"other": true, "some": true, "such": true, "no": true,
	"that": true, "this": true, "these": true, "those": true, "it": true, "its": true,
	"i": true, "me": true, "my": true, "we": true, "our": true, "you": true, "your": true,
	"he": true, "him": true, "his": true,
	"she": true, "her": true, "they": true, "them": true, "their": true, "who": true, "which": true,
	"what": true,
	"if":   true, "then": true, "than": true, "when": true, "where": true, "how": true, "just": true,
	"also": true,
}

// horizontalSpaceRe is `/[^\S\n]+/g`: whitespace other than a newline.
var horizontalSpaceRe = mustCompile(`[^\S\n]+`)

// blankRunRe is `/\n{3,}/g`.
var blankRunRe = mustCompile(`\n{3,}`)

// normalizeForTokenBudget is brief.ts normalizeForTokenBudget().
func normalizeForTokenBudget(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = horizontalSpaceRe.ReplaceAllString(text, " ")
	text = blankRunRe.ReplaceAllString(text, "\n\n")
	return jsTrimSpace(text)
}

// wordSegment is one segment of brief.ts's Intl.Segmenter({granularity:"word"})
// walk: the segment text and its code-unit start index.
type wordSegment struct {
	text  string
	start int // UTF-16 code-unit index
}

// segmentWords walks text the way Intl.Segmenter's word granularity does for
// the purposes of brief.ts: it yields maximal runs of letters/digits as words
// and every other code point (with its adjacent whitespace) as a non-word
// segment. Both consumers only ask whether a segment is a word and where it
// starts and ends, so this grouping is equivalent for them.
func segmentWords(text string) []wordSegment {
	segments := make([]wordSegment, 0, 16)
	unit := 0
	runes := []rune(text)
	for i := 0; i < len(runes); {
		r := runes[i]
		if isWordLikeLetterOrNumber(r) {
			start := unit
			var b strings.Builder
			for i < len(runes) && isWordLikeLetterOrNumber(runes[i]) {
				b.WriteRune(runes[i])
				unit += jsRuneWidth(runes[i])
				i++
			}
			segments = append(segments, wordSegment{text: b.String(), start: start})
			continue
		}
		start := unit
		var b strings.Builder
		for i < len(runes) && !isWordLikeLetterOrNumber(runes[i]) {
			b.WriteRune(runes[i])
			unit += jsRuneWidth(runes[i])
			i++
		}
		segments = append(segments, wordSegment{text: b.String(), start: start})
	}
	return segments
}

func jsRuneWidth(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// truncateTokens is brief.ts truncateTokens(): keep the first `limit`
// non-stopword words and cut there with a "(truncated)" marker.
func truncateTokens(text string, limit int) string {
	flat := normalizeForTokenBudget(text)
	count := 0
	lastEnd := 0
	for _, seg := range segmentWords(flat) {
		if containsLetterOrNumber(seg.text) {
			if !stopWords[strings.ToLower(seg.text)] {
				count++
				if count > limit {
					return jsTrimRightSpace(jsSlice(flat, 0, lastEnd)) + "...(truncated)"
				}
			}
		}
		lastEnd = seg.start + jsLen(seg.text)
	}
	return flat
}

// significantWordSpans is brief.ts significantWordSpans(): the code-unit spans
// of the non-stopword words.
type wordSpan struct {
	start int
	end   int
}

func significantWordSpans(flat string) []wordSpan {
	words := make([]wordSpan, 0, 32)
	for _, seg := range segmentWords(flat) {
		if !containsLetterOrNumber(seg.text) {
			continue
		}
		if stopWords[strings.ToLower(seg.text)] {
			continue
		}
		words = append(words, wordSpan{start: seg.start, end: seg.start + jsLen(seg.text)})
	}
	return words
}

// truncateTokensHeadTail is brief.ts truncateTokensHeadTail().
func truncateTokensHeadTail(text string, headLimit, tailLimit int) string {
	flat := normalizeForTokenBudget(text)
	if headLimit <= 0 || tailLimit <= 0 {
		return flat
	}
	words := significantWordSpans(flat)
	if len(words) <= headLimit+tailLimit {
		return flat
	}
	head := jsTrimRightSpace(jsSlice(flat, 0, words[headLimit-1].end))
	tail := jsTrimLeftSpace(jsSlice(flat, words[len(words)-tailLimit].start, jsLen(flat)))
	return head + "\n...(middle truncated)...\n" + tail
}

// nextRenderableBlock is brief.ts nextRenderableBlock(): the next block that is
// not a tool result.
func nextRenderableBlock(blocks []NormalizedBlock, index int) (NormalizedBlock, bool) {
	for i := index + 1; i < len(blocks); i++ {
		if blocks[i].Kind != KindToolResult {
			return blocks[i], true
		}
	}
	return NormalizedBlock{}, false
}

// isSegmentClosingAssistant is brief.ts isSegmentClosingAssistant(): an
// assistant block followed by nothing or by a user turn.
func isSegmentClosingAssistant(blocks []NormalizedBlock, index int) bool {
	if index < 0 || index >= len(blocks) || blocks[index].Kind != KindAssistant {
		return false
	}
	next, ok := nextRenderableBlock(blocks, index)
	return !ok || next.Kind == KindUser
}

// ── bash command compression ────────────────────────────────────────────────

const (
	bashCap         = 240
	heredocBodyCap  = 80
	heredocEllipsis = "\u2026"
)

// pipeTailRe is brief.ts PIPE_TAIL_RE.
var pipeTailRe = mustCompile(jsSpace + `*\|\s*(?:head|tail|sort|wc|column|tr|cut|awk|uniq|python3|node|bun)(?:` +
	jsSpace + `[^|]*)?$`)

// pipeTailUnicodeRe is the same rule with JS `\s` in the leading position, used
// when the rule is re-applied to an already trimmed string.
var trivialLineRe = mustCompile(`^(?:set` + jsSpace + `+[-+]|cd` + jsSpace + `+\S+$|export` + jsSpace +
	`+\w+=|(?:source|\.)` + jsSpace + `+\S+|pwd$|true$|:$|#)`)

// heredocOpenRe is brief.ts HEREDOC_OPEN_RE:
// /(?<![\w)])<<-?\s*["']?([A-Za-z_]\w*)["']?/ — `<<` at a command boundary.
// RE2 has no lookbehind, so the "not preceded by a word char or `)`" condition
// is checked in heredocCloseIndex instead.
var heredocOpenRe = mustCompile(`<<-?` + jsSpace + `*["']?([A-Za-z_]\w*)["']?`)

// fileWriterHeredocRe is brief.ts FILEWRITER_HEREDOC_RE.
var fileWriterHeredocRe = mustCompile(`(?:^|[|&;]` + jsSpace + `*)(?:cat|tee|dd)\b`)

// bodyNoiseRe is brief.ts BODY_NOISE_RE.
var bodyNoiseRe = mustCompile(`^(?:import` + jsSpace + `|from` + jsSpace + `+\S+` + jsSpace + `+import|require\(|const` +
	jsSpace + `+\w+` + jsSpace + `*=` + jsSpace + `*require|#|//|"""|'''|"use strict"|use` + jsSpace + `+strict|<\?php)`)

// heredocMatchAt is the `<<` opener found at line, honoring the front guard that
// RE2 cannot express as a lookbehind. It returns the terminator and true.
func heredocMatchAt(line string) (string, bool) {
	locs := heredocOpenRe.FindAllStringSubmatchIndex(line, -1)
	for _, loc := range locs {
		before := loc[0] - 1
		if before >= 0 {
			r, _ := lastRuneBefore(line, loc[0])
			if isASCIIWord(r) || r == ')' {
				continue
			}
		}
		return line[loc[2]:loc[3]], true
	}
	return "", false
}

// HeredocCloseIndex is brief.ts heredocCloseIndex(): the index of the line that
// terminates the heredoc opened on lines[i], or -1 when no terminator appears
// downstream (a shift op, a quoted string or a truncated body).
func HeredocCloseIndex(lines []string, i int) int {
	if i < 0 || i >= len(lines) {
		return -1
	}
	term, ok := heredocMatchAt(lines[i])
	if !ok {
		return -1
	}
	for j := i + 1; j < len(lines); j++ {
		if jsTrimSpace(lines[j]) == term {
			return j
		}
	}
	return -1
}

// cdPrefixRe is brief.ts's `^cd\s+\S+\s*&&\s*`.
var cdPrefixRe = mustCompile(`^cd` + jsSpace + `+\S+` + jsSpace + `*&&` + jsSpace + `*`)

// stripCdPrefix is brief.ts stripCdPrefix().
func stripCdPrefix(line string) string {
	return jsTrimSpace(cdPrefixRe.ReplaceAllString(line, ""))
}

// stripPipeTail is brief.ts stripPipeTail(): up to three passes of PIPE_TAIL_RE.
func stripPipeTail(line string) string {
	current := line
	for i := 0; i < 3; i++ {
		next := pipeTailRe.ReplaceAllString(current, "")
		if next == current {
			break
		}
		current = next
	}
	return jsTrimSpace(current)
}

// CompressBash is brief.ts compressBash(): drop heredoc bodies and scaffolding
// lines, strip `cd … &&` prefixes and pipe tails, join with "; " and cap.
func CompressBash(raw string) string {
	rawLines := jsSplitLines(raw)
	withoutHeredocBodies := make([]string, 0, len(rawLines))
	for i := 0; i < len(rawLines); i++ {
		line := rawLines[i]
		withoutHeredocBodies = append(withoutHeredocBodies, line)
		closeIdx := HeredocCloseIndex(rawLines, i)
		if closeIdx == -1 {
			continue
		}
		// File-writer heredocs name their target -> keep the opener only.
		// Every other heredoc has a content-free opener -> take the first
		// meaningful body line as a preview.
		wantPreview := !fileWriterHeredocRe.MatchString(line)
		preview := ""
		for j := i + 1; wantPreview && preview == "" && j < closeIdx; j++ {
			t := jsTrimSpace(rawLines[j])
			if t != "" && !bodyNoiseRe.MatchString(t) {
				preview = t
			}
		}
		if preview != "" {
			clipped := preview
			if jsLen(preview) > heredocBodyCap {
				clipped = jsSlice(preview, 0, heredocBodyCap-1) + heredocEllipsis
			}
			withoutHeredocBodies[len(withoutHeredocBodies)-1] = jsTrimSpace(line) + " " + clipped
		}
		i = closeIdx
	}

	lines := make([]string, 0, len(withoutHeredocBodies))
	for _, line := range withoutHeredocBodies {
		if trimmed := jsTrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return jsTrimSpace(raw)
	}

	meaningful := make([]string, 0, len(lines))
	for _, line := range lines {
		if trivialLineRe.MatchString(line) {
			continue
		}
		cleaned := stripPipeTail(stripCdPrefix(line))
		if cleaned != "" {
			meaningful = append(meaningful, cleaned)
		}
	}
	chosen := meaningful
	if len(chosen) == 0 {
		first := stripPipeTail(stripCdPrefix(lines[0]))
		chosen = nil
		if first != "" {
			chosen = []string{first}
		}
	}

	command := strings.Join(chosen, "; ")
	if jsLen(command) > bashCap {
		return jsSlice(command, 0, bashCap-3) + "..."
	}
	return command
}

// ── tool summary ────────────────────────────────────────────────────────────

// toolSummaryFields is brief.ts TOOL_SUMMARY_FIELDS.
var toolSummaryFields = map[string]string{
	"Read": "file_path", "Edit": "file_path", "Write": "file_path",
	"read": "file_path", "edit": "file_path", "write": "file_path",
	"Glob": "pattern", "Grep": "pattern",
}

// toolOneLiner is brief.ts toolOneLiner().
func toolOneLiner(name string, args map[string]any) string {
	if field, ok := toolSummaryFields[name]; ok {
		if value, isString := args[field].(string); isString {
			return "* " + name + " \"" + value + "\""
		}
	}
	if path, ok := extractPath(args); ok {
		return "* " + name + " \"" + path + "\""
	}
	if name == "bash" || name == "Bash" {
		raw := ""
		if command, ok := args["command"].(string); ok {
			raw = command
		} else if description, ok := args["description"].(string); ok {
			raw = description
		}
		return "* " + name + " \"" + CompressBash(raw) + "\""
	}
	if query, ok := args["query"].(string); ok {
		return "* " + name + " \"" + Clip(query, 60) + "\""
	}
	return "* " + name
}

// BriefLine is brief.ts BriefLine.
type BriefLine struct {
	Header string
	Lines  []string
}

// BuildBriefSections is brief.ts buildBriefSections().
func BuildBriefSections(blocks []NormalizedBlock) []BriefLine {
	sections := make([]BriefLine, 0, 8)
	lastHeader := ""

	push := func(header, line string) {
		if header == lastHeader && len(sections) > 0 {
			sections[len(sections)-1].Lines = append(sections[len(sections)-1].Lines, line)
			return
		}
		sections = append(sections, BriefLine{Header: header, Lines: []string{line}})
		lastHeader = header
	}

	pushText := func(header, text, ref string) {
		lines := jsSplitLines(text)
		if ref != "" && len(lines) > 0 {
			lines[len(lines)-1] = lines[len(lines)-1] + ref
		}
		for _, line := range lines {
			push(header, line)
		}
	}

	for blockIndex := 0; blockIndex < len(blocks); blockIndex++ {
		b := blocks[blockIndex]
		switch b.Kind {
		case KindUser:
			if isNoiseUser(b.Text) {
				break
			}
			text := truncateTokens(CollapseSkillText(b.Text), truncateUser)
			if text != "" {
				pushText("[user]", text, sourceRef(b))
			}
			lastHeader = "[user]"
		case KindBash:
			command := CompressBash(b.Command)
			if command != "" {
				push("[user]", "$ "+command+sourceRef(b))
			}
			lastHeader = "[user]"
		case KindAssistant:
			raw := b.Text
			// Strip leading self-talk (up to 2x; assistants sometimes chain).
			for i := 0; i < 2; i++ {
				stripped := selfTalkPrefixRe.ReplaceAllString(raw, "")
				if stripped == raw {
					break
				}
				raw = stripped
			}
			var text string
			if isSegmentClosingAssistant(blocks, blockIndex) {
				text = truncateTokensHeadTail(raw, segmentClosingAssistantHeadWords, segmentClosingAssistantTailWords)
			} else {
				text = truncateTokensHeadTail(raw, assistantHeadWords, assistantTailWords)
			}
			if text != "" {
				pushText("[assistant]", text, sourceRef(b))
			}
		case KindToolCall:
			// Skip malformed tool calls from streaming providers.
			if jsTrimSpace(b.Name) == "" {
				break
			}
			summary := toolOneLiner(b.Name, b.Args) + sourceRef(b)
			push("[assistant]", summary)
		case KindToolResult:
			// Tool result bodies are intentionally omitted from compact briefs.
		}
	}

	// Collapse consecutive identical tool lines (same text, different #ref).
	collapseRe := mustCompile(`^(.*) \((#[\d, #]+)\) x(\d+)$`)
	singleRefRe := mustCompile(`\(#\d+\)$`)
	refTailRe := mustCompile(`\(#(\d+)\)$`)
	for si := range sections {
		sec := &sections[si]
		if sec.Header != "[assistant]" {
			continue
		}
		out := make([]string, 0, len(sec.Lines))
		for _, line := range sec.Lines {
			if !strings.HasPrefix(line, "* ") {
				out = append(out, line)
				continue
			}
			ref := ""
			if match := refTailRe.FindStringSubmatch(line); match != nil {
				ref = match[1]
			}
			// A ref-less line never joins a ref'd collapse group.
			if ref == "" {
				out = append(out, line)
				continue
			}
			base := jsTrimRightSpace(jsSlice(line, 0, jsLen(line)-(jsLen(ref)+3)))
			last := ""
			if len(out) > 0 {
				last = out[len(out)-1]
			}
			if m := collapseRe.FindStringSubmatch(last); m != nil && m[1] == base {
				count, _ := parseIntStrict(m[3])
				out[len(out)-1] = base + " (" + m[2] + ", #" + ref + ") x" + jsNumberString(float64(count+1))
			} else if singleRefRe.MatchString(last) && jsTrimRightSpace(jsReplaceRegexpFirst(last, mustCompile(jsSpace+`*\(#\d+\)$`), func(string, []string) string { return "" })) == base {
				prevRef := ""
				if m := refTailRe.FindStringSubmatch(last); m != nil {
					prevRef = m[1]
				}
				out[len(out)-1] = base + " (#" + prevRef + ", #" + ref + ") x2"
			} else {
				out = append(out, line)
			}
		}
		sec.Lines = out
	}

	// Cap tool calls per [assistant] turn — keep tail (latest actions tend to be
	// the deciding edits/writes).
	const toolCallsPerTurn = 8
	for si := range sections {
		sec := &sections[si]
		if sec.Header != "[assistant]" {
			continue
		}
		toolIdxs := make([]int, 0, len(sec.Lines))
		for i, line := range sec.Lines {
			if strings.HasPrefix(line, "* ") {
				toolIdxs = append(toolIdxs, i)
			}
		}
		if len(toolIdxs) <= toolCallsPerTurn {
			continue
		}
		dropCount := len(toolIdxs) - toolCallsPerTurn
		dropSet := make(map[int]bool, dropCount)
		for _, idx := range toolIdxs[:dropCount] {
			dropSet[idx] = true
		}
		firstKeptToolIdx := toolIdxs[dropCount]
		next := make([]string, 0, len(sec.Lines))
		inserted := false
		for i := 0; i < len(sec.Lines); i++ {
			if dropSet[i] {
				continue
			}
			if !inserted && i == firstKeptToolIdx {
				next = append(next, "* ("+jsNumberString(float64(dropCount))+" earlier tool-call entries omitted)")
				inserted = true
			}
			next = append(next, sec.Lines[i])
		}
		sec.Lines = next
	}

	return sections
}

// StringifyBrief is brief.ts stringifyBrief().
func StringifyBrief(sections []BriefLine) string {
	out := make([]string, 0, len(sections)*2)
	for i, sec := range sections {
		if i > 0 {
			prev := sections[i-1]
			prevIsTools := prev.Header == "[assistant]" && allToolLines(prev.Lines)
			curIsTools := sec.Header == "[assistant]" && allToolLines(sec.Lines)
			if !(prevIsTools && curIsTools) {
				out = append(out, "")
			}
		}
		out = append(out, sec.Header)
		out = append(out, sec.Lines...)
	}
	return strings.Join(out, "\n")
}

func allToolLines(lines []string) bool {
	for _, line := range lines {
		if !strings.HasPrefix(line, "* ") {
			return false
		}
	}
	return true
}

// CompileBrief is brief.ts compileBrief().
func CompileBrief(blocks []NormalizedBlock) string {
	return StringifyBrief(BuildBriefSections(blocks))
}

// parseIntStrict is JS `parseInt(s, 10)` for a decimal digit string.
func parseIntStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<40 {
			return 0, false
		}
	}
	return n, true
}

var _ = unicode.IsSpace
