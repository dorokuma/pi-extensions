// Package notables implements the no-tables extension for PiG in Go.
//
// Background & Motivation:
// 1. Automatically converts any markdown table in assistant output to bullet lists.
// 2. Converts markdown formats unsupported by terminal renderers (h3-h6 headings, images, footnotes).
// 3. Converts markdown links to plain text and bare URLs.
// 4. Normalizes misplaced and glued code block fences so text following fences is not swallowed.
// 5. Repairs right-flanking bold closers preceded by punctuation and followed by text.
// Silent, zero-config, no user interaction needed.
//
// Alignment & Differences with TypeScript Implementation:
// 1. Hook Registration:
//   - TS: pi.on("message_end", async (event) => ...)
//   - Go: e.OnEvent(sdk.EventMessageEnd, HandleMessageEnd)
//
// 2. Event Payload Structure:
//   - TS: event.message carries role and content parts array.
//   - Go: data["message"] map carries role and content parts slice.
//
// 3. Transformation Pipeline:
//   - TS: convertUnsupportedMarkdown(convertTables(part.text))
//   - Go: ConvertUnsupportedMarkdown(ConvertTables(partText))
//   - Execution order and rules (Rule A to Rule G) mirror TS semantics with documented edge differences.
//
// 4. Return Value:
//   - TS: returns { message: { ...event.message, content: newParts } } when changed, or undefined for no-op.
//   - Go: returns map[string]any{"message": newMsg} when changed, or (nil, nil) for no-op.
package notables

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// CodeSpanRange represents a [Start, End) byte range for inline code spans.
type CodeSpanRange struct {
	Start int
	End   int
}

const (
	// jsWhitespaceClass defines the character class for regexes mirroring JavaScript \s:
	// [\t\n\f\r\v \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]
	jsWhitespaceClass = `[\t\n\f\r\v \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

	// jsLineStart mirrors JavaScript's multiline ^ (pattern flag m) for LF and CR.
	//
	// JS ^ matches at the start of input and right after any line terminator
	// (\n, \r, \u2028, \u2029); Go's (?m)^ only breaks on \n. The bare CR is
	// covered here by consuming it in capture group 1; callers re-emit that group
	// verbatim, so the net byte stream is unchanged (the anchor stays zero-width).
	// A CR inside "\r\n" needs no alternative: the position between \r and \n is
	// followed by \n, which no rule can start with. U+2028/U+2029 stay out of
	// scope (documented deviation).
	jsLineStart = `(?:^|(\r))`

	// jsLineEnd mirrors JavaScript's multiline $ (pattern flag m) for LF and CR.
	//
	// JS $ matches before any line terminator and at the end of input; Go's (?m)$
	// only breaks on \n. RE2 has no lookahead either, so the only way to express
	// "$ before a line terminator" is to consume that terminator in capture group 4.
	// replaceWithJSLines compensates for the consumption: it never rewrites that
	// group and resumes scanning on the terminator itself, which is exactly where
	// JS's zero-width $ leaves the cursor (see the helper's comment). Because the
	// preceding \s* can itself swallow a terminator (JS \s contains \r and \n), the
	// resulting bytes still match TS -- e.g. "### a\r\n" -> "**a**" (the CRLF is
	// eaten by \s*), not "**a**\r\n". U+2028/U+2029 stay out of scope (documented
	// deviation).
	jsLineEnd = `(\r\n|\r|\n|\z)`

	// jsDot mirrors JavaScript's unescaped . in the same scope: JS . never matches
	// \n or \r, while Go's . matches every byte except \n.
	jsDot = `[^\n\r]`
)

var (
	// tableRe matches markdown tables: header row + separator + data rows.
	// Mirrors TS no-tables.ts:286 `/^(\|.+\|)\n(\|[-| :]+\|)\n((?:\|.+\|\n?)+)/gm`:
	// JS multiline ^ also breaks after a bare CR, and JS . never crosses \r, so a
	// CRLF table never matches here (the row separator demands a bare \n).
	reTable = regexp.MustCompile(`(?m)` + jsLineStart + `(\|` + jsDot + `+\|)\n(\|[-| :]+\|)\n((?:\|` + jsDot + `+\|\n?)+)`)

	// Glued fence lookbehind/tail matcher:
	// A line ending with a glued fence: non-delimiter character + optional spaces + fence (3+) + optional tag + optional spaces
	// End pattern [ \t]*$ matches TS no-tables.ts:110 lookahead (?:\r?\n|$); CRLF lines ending with \r do not match.
	reGluedTail = regexp.MustCompile(`([^\n` + "`" + `~ \t>])[ \t]*(` + "`" + `{3,}|~{3,})([ \t]*[A-Za-z0-9_+.\-]*[ \t]*)$`)

	// Fenced code block opener.
	// Mirrors TS no-tables.ts:124 `/^( {0,3})(`{3,}|~{3,})(.*)$/` (no m flag, JS .
	// never crosses \r, JS $ is end-of-input). A line still carrying a CRLF's \r
	// therefore does NOT open a block in TS -- and must not in Go either, or the
	// fence state machine dedents lines TS leaves alone.
	reOpenFence = regexp.MustCompile(`^( {0,3})(` + "`" + `{3,}|~{3,})(` + jsDot + `*)$`)

	// Fenced code block closer matching TS no-tables.ts:87 [ \t]*$
	reCloseFence = regexp.MustCompile(`^( {0,3})(` + "`" + `{3,}|~{3,})[ \t]*$`)

	// Fenced code blocks for splitting prose and code (matching TS no-tables.ts:46)
	reFencedBlock = regexp.MustCompile("(```[\\s\\S]*?```|~~~[\\s\\S]*?~~~)")

	// Rule A: h3-h6 headers to bold line.
	// Mirrors TS no-tables.ts:167 `/^(#{3,6})\s+(.+?)\s*#*\s*$/gm` -- JS multiline ^
	// also breaks after a bare CR, JS . never crosses \r, and JS $ also breaks
	// before a bare CR (Go's (?m)$ only breaks on \n). Only patching . to [^\n\r]
	// is not enough: without the CR-aware $, "### a\rb" would still be read as one
	// heading (Go) instead of a heading plus trailing text (TS).
	reHeading = regexp.MustCompile(`(?m)` + jsLineStart + `(#{3,6})` + jsWhitespaceClass + `+(` + jsDot + `+?)` + jsWhitespaceClass + `*#*` + jsWhitespaceClass + `*` + jsLineEnd)

	// Rule D: Footnote definition lines.
	// Mirrors TS no-tables.ts:170 `/^\[\^([^\]]+)\]:\s*(.+)$/gm` under the same
	// CR-aware ^/$/. mapping as Rule A.
	reFootDef = regexp.MustCompile(`(?m)` + jsLineStart + `\[\^([^\]]+)\]:` + jsWhitespaceClass + `*(` + jsDot + `+)` + jsLineEnd)

	// Rule C: Footnote references
	reFootRef = regexp.MustCompile(`\[\^([^\]]+)\]`)

	// Rule B: Images to links ([^\r\n]* matches JS . behavior avoiding \r in CRLF)
	reImg = regexp.MustCompile(`!+\[([^\r\n]*?)\]\(([^\r\n]*?)\)`)

	// Rule E: Links to text + url ([^\r\n]* matches JS . behavior avoiding \r in CRLF)
	reLink = regexp.MustCompile(`\[([^\r\n]*?)\]\(([^\r\n]*?)\)`)

	// Rule G: punctuation runes allowed at the end of a bold span
	boldClosingPunctSet = map[rune]struct{}{
		'：': {}, ':': {}, '，': {}, ',': {}, '。': {}, '．': {}, '.': {}, '、': {},
		'；': {}, ';': {}, '！': {}, '!': {}, '？': {}, '?': {}, '…': {}, '—': {},
		'～': {}, '~': {}, '）': {}, ')': {}, '】': {}, ']': {}, '》': {}, '〉': {},
		'」': {}, '』': {}, '”': {}, '’': {}, '"': {}, '\'': {},
	}
)

func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func isBoldClosingPunct(r rune) bool {
	_, ok := boldClosingPunctSet[r]
	return ok
}

func isBoldFollowingNonSpace(r rune) bool {
	if r == 0 {
		return false
	}
	if isJSWhitespace(r) || r == '*' {
		return false
	}
	_, isPunct := boldClosingPunctSet[r]
	return !isPunct
}

// ConvertTables converts markdown tables into bullet lists.
func ConvertTables(md string) string {
	matches := reTable.FindAllStringSubmatchIndex(md, -1)
	if len(matches) == 0 {
		return md
	}

	var sb strings.Builder
	lastIndex := 0
	for _, m := range matches {
		sb.WriteString(md[lastIndex:m[0]])
		sb.WriteString(jsCaptured(md, m, 1)) // consumed CR restored (see jsLineStart)
		sb.WriteString(convertTableMatch(md[m[4]:m[5]], md[m[8]:m[9]]))
		lastIndex = m[1]
	}
	sb.WriteString(md[lastIndex:])
	return sb.String()
}

// jsCaptured returns the text captured by group n (1-based), or "" when the
// group did not participate in the match.
func jsCaptured(text string, m []int, n int) string {
	start, end := m[2*n], m[2*n+1]
	if start < 0 || end < 0 {
		return ""
	}
	return text[start:end]
}

// convertTableMatch renders one matched table (header row + body block) as bullets.
func convertTableMatch(headerLine, bodyBlock string) string {
	headers := splitCells(headerLine)
	trimmedBody := strings.TrimSpace(bodyBlock)
	lines := strings.Split(trimmedBody, "\n")

	var bullets []string
	for _, line := range lines {
		row := splitCells(line)
		label := ""
		if len(row) > 0 {
			label = strings.TrimSpace(row[0])
		}
		var details []string
		for i := 1; i < len(headers) && i < len(row); i++ {
			h := strings.TrimSpace(headers[i])
			v := strings.TrimSpace(row[i])
			if v != "" {
				if h != "" {
					details = append(details, fmt.Sprintf("%s: %s", h, v))
				} else {
					details = append(details, v)
				}
			}
		}
		if len(details) > 0 {
			bullets = append(bullets, fmt.Sprintf("- **%s** — %s", label, strings.Join(details, ", ")))
		} else if label != "" {
			bullets = append(bullets, fmt.Sprintf("- %s", label))
		}
	}
	return strings.Join(bullets, "\n") + "\n"
}

func splitCells(line string) []string {
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	return strings.Split(line, "|")
}

// ConvertUnsupportedMarkdown converts markdown formats unsupported by pi terminal.
func ConvertUnsupportedMarkdown(md string) string {
	// Rule F runs BEFORE the split below.
	normalized := NormalizeMisplacedFences(md)

	// Split by fenced code blocks (```, ~~~, ````) so rules A-G only apply outside code blocks
	matches := reFencedBlock.FindAllStringIndex(normalized, -1)
	var segments []string
	lastIndex := 0

	for _, m := range matches {
		segments = append(segments, normalized[lastIndex:m[0]])
		segments = append(segments, normalized[m[0]:m[1]])
		lastIndex = m[1]
	}
	segments = append(segments, normalized[lastIndex:])

	for i := 0; i < len(segments); i += 2 {
		segments[i] = applyMarkdownRules(segments[i])
	}

	// Second F pass over the joined result.
	return NormalizeMisplacedFences(strings.Join(segments, ""))
}

func dedentFenceLine(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return trimmed
	}
	return line
}

// NormalizeMisplacedFences repairs fence runs (3+ backticks or 3+ tildes)
// that CommonMark does not recognize as fences because of where they sit.
func NormalizeMisplacedFences(md string) string {
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	var openFence string

	for _, line := range lines {
		if openFence != "" {
			// Inside a block: only a genuine closing fence may be dedented
			dedented := dedentFenceLine(line)
			closeMatch := reCloseFence.FindStringSubmatch(dedented)
			if len(closeMatch) > 2 && closeMatch[2][0] == openFence[0] && len(closeMatch[2]) >= len(openFence) {
				openFence = ""
				out = append(out, dedented)
			} else {
				out = append(out, line)
			}
			continue
		}

		// Outside a block: check for glued fence (F1)
		fenceLine := line
		gluedMatch := reGluedTail.FindStringSubmatchIndex(line)
		if len(gluedMatch) > 0 {
			prefix := line[:gluedMatch[3]]
			// Guard: skip if there is already a 3+ fence run earlier on the line
			if !strings.Contains(prefix, "```") && !strings.Contains(prefix, "~~~") {
				out = append(out, prefix)
				fenceStartIndex := gluedMatch[4]
				fenceLine = strings.TrimLeft(line[fenceStartIndex:], " \t")
			} else {
				fenceLine = dedentFenceLine(line)
			}
		} else {
			// F2: an indented fence run is moved to column 0
			fenceLine = dedentFenceLine(line)
		}
		out = append(out, fenceLine)

		// Track whether this line opened a block
		openMatch := reOpenFence.FindStringSubmatch(fenceLine)
		if len(openMatch) > 3 {
			fenceChars := openMatch[2]
			info := openMatch[3]
			if !(fenceChars[0] == '`' && strings.Contains(info, "`")) {
				openFence = fenceChars
			}
		}
	}

	return strings.Join(out, "\n")
}

// splitByInlineCode splits text by inline backtick code spans (`...`),
// returning [prose, code, prose, code, ...].
func splitByInlineCode(text string) []string {
	var segments []string
	lastIndex := 0
	i := 0
	n := len(text)

	for i < n {
		if text[i] == '`' {
			start := i
			for i < n && text[i] == '`' {
				i++
			}
			openerLen := i - start

			// Find matching closer with exactly openerLen backticks on the same line
			closerStart := -1
			j := i
			for j < n && text[j] != '\n' {
				if text[j] == '`' {
					cStart := j
					for j < n && text[j] == '`' {
						j++
					}
					if j-cStart == openerLen {
						closerStart = cStart
						break
					}
				} else {
					j++
				}
			}

			if closerStart != -1 {
				matchEnd := closerStart + openerLen
				segments = append(segments, text[lastIndex:start], text[start:matchEnd])
				lastIndex = matchEnd
				i = matchEnd
				continue
			}
			i = start + openerLen
			continue
		}
		i++
	}
	segments = append(segments, text[lastIndex:])
	return segments
}

func joinWithCodeRanges(segments []string) (string, []CodeSpanRange) {
	var sb strings.Builder
	var codeRanges []CodeSpanRange
	offset := 0
	for i, seg := range segments {
		segLen := len(seg)
		if i%2 == 1 {
			codeRanges = append(codeRanges, CodeSpanRange{Start: offset, End: offset + segLen})
		}
		sb.WriteString(seg)
		offset += segLen
	}
	return sb.String(), codeRanges
}

// replaceWithJSLines applies a JavaScript /gm-style replacement to text.
//
// re must start with jsLineStart (group 1) and end with jsLineEnd (group
// termGroup). JS anchors are zero-width, but RE2 has no lookbehind/lookahead, so
// the only way to say "$ before a line terminator" is to consume that terminator.
// This helper undoes that consumption on both sides of the match:
//
//   - the consumed terminator is not rewritten and stays in the output verbatim;
//   - scanning resumes on the terminator itself, which is exactly where JS's
//     zero-width $ leaves the cursor.
//
// Resuming matters: "###\ta\r\r#### b" makes the trailing \s* swallow one \r and
// $ sit before the second one. If the scan skipped past the second \r, the
// following heading (whose ^ is the position after that \r) would be missed and
// Go would disagree with TS.
func replaceWithJSLines(text string, re *regexp.Regexp, termGroup int, render func(group func(int) string) string) string {
	var sb strings.Builder
	written := 0
	searchFrom := 0

	for searchFrom <= len(text) {
		offset := searchFrom
		m := re.FindStringSubmatchIndex(text[offset:])
		if m == nil {
			break
		}
		for i := range m {
			if m[i] >= 0 {
				m[i] += offset
			}
		}

		termStart := m[2*termGroup]
		if termStart < 0 {
			termStart = m[1]
		}
		if termStart <= m[0] {
			break // no forward progress; cannot happen for the rules below
		}

		sb.WriteString(text[written:m[0]])
		sb.WriteString(jsCaptured(text, m, 1)) // bare CR the ^ anchor consumed
		sb.WriteString(render(func(n int) string { return jsCaptured(text, m, n) }))
		written = termStart
		searchFrom = termStart
	}

	sb.WriteString(text[written:])
	return sb.String()
}

// replaceHeadingLines applies Rule A with JavaScript /gm anchor semantics.
//
// TS: text.replace(/^(#{3,6})\s+(.+?)\s*#*\s*$/gm, "**$2**")
//
// The Go regexp carries jsLineStart (group 1, a consumed bare CR), the heading
// run (group 2), the heading text (group 3) and jsLineEnd (group 4, a consumed
// line terminator).
func replaceHeadingLines(text string) string {
	return replaceWithJSLines(text, reHeading, 4, func(group func(int) string) string {
		return "**" + group(3) + "**"
	})
}

// replaceFootnoteDefinitions applies Rule D with JavaScript /gm anchor semantics.
//
// TS: text.replace(/^\[\^([^\]]+)\]:\s*(.+)$/gm, "**[$1]** $2")
// Group layout matches replaceHeadingLines: 1 = consumed bare CR, 2 = footnote
// id, 3 = definition body, 4 = consumed line terminator.
func replaceFootnoteDefinitions(text string) string {
	return replaceWithJSLines(text, reFootDef, 4, func(group func(int) string) string {
		return "**[" + group(2) + "]** " + group(3)
	})
}

func applyMarkdownRules(text string) string {
	// Rule A: h3-h6 headers to bold line
	text = replaceHeadingLines(text)

	// Rule D: Footnote definition lines
	text = replaceFootnoteDefinitions(text)

	// Split by inline backtick code spans (`...`) after line-anchored rules (A, D)
	segments := splitByInlineCode(text)
	for i := 0; i < len(segments); i += 2 {
		segments[i] = applyInlineMarkdownRules(segments[i])
	}

	joined, codeRanges := joinWithCodeRanges(segments)
	if len(codeRanges) == 0 {
		return joined
	}

	afterLinks := applyLinkRule(joined, codeRanges)
	finalCodeRanges := codeRanges
	if afterLinks != joined {
		_, finalCodeRanges = joinWithCodeRanges(splitByInlineCode(afterLinks))
	}
	return applyBoldClosingRule(afterLinks, finalCodeRanges)
}

func applyInlineMarkdownRules(text string) string {
	// Rule C: Footnote references
	text = reFootRef.ReplaceAllString(text, "[$1]")

	// Rule B: Images to links
	text = reImg.ReplaceAllStringFunc(text, func(m string) string {
		sub := reImg.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		alt := sub[1]
		url := sub[2]
		label := alt
		if strings.TrimSpace(alt) == "" {
			label = "image"
		}
		return "[" + label + "](" + url + ")"
	})

	// Rule E: Links to text + url
	text = applyLinkRule(text, nil)

	// Rule G: repair right-flanking bold closers
	text = applyBoldClosingRule(text, nil)

	return text
}

func applyLinkRule(text string, codeRanges []CodeSpanRange) string {
	matches := reLink.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var sb strings.Builder
	lastIndex := 0

	for _, m := range matches {
		offset := m[0]
		matchEnd := m[1]
		labelStart := m[2]
		labelEnd := m[3]
		urlStart := m[4]
		urlEnd := m[5]

		label := text[labelStart:labelEnd]
		url := text[urlStart:urlEnd]
		match := text[offset:matchEnd]

		inOrTouchesCodeDelimiter := false
		for _, r := range codeRanges {
			if r.Start < matchEnd && r.End > offset &&
				!(r.Start >= labelStart && r.End <= labelEnd) &&
				!(r.Start >= urlStart && r.End <= urlEnd) {
				inOrTouchesCodeDelimiter = true
				break
			}
		}

		sb.WriteString(text[lastIndex:offset])
		lastIndex = matchEnd

		if inOrTouchesCodeDelimiter {
			sb.WriteString(match)
		} else {
			trimmedLabel := strings.TrimSpace(label)
			trimmedUrl := strings.TrimSpace(url)
			if trimmedLabel == trimmedUrl || trimmedLabel == "" {
				sb.WriteString(trimmedUrl)
			} else {
				sb.WriteString(label + " " + url)
			}
		}
	}

	sb.WriteString(text[lastIndex:])
	return sb.String()
}

func applyBoldClosingRule(text string, codeRanges []CodeSpanRange) string {
	var sb strings.Builder
	lastIndex := 0
	i := 0
	n := len(text)

	for i < n-1 {
		if text[i] == '*' && text[i+1] == '*' {
			if i > 0 && text[i-1] == '*' {
				i++
				continue
			}
			offset := i
			if i+2 >= n {
				break
			}
			rNext, _ := utf8.DecodeRuneInString(text[i+2:])
			if rNext == '*' || isJSWhitespace(rNext) {
				i += 2
				continue
			}

			j := i + 2
			matchedCloser := -1
			for j < n-1 && text[j] != '\n' {
				if text[j] == '*' {
					if text[j+1] == '*' {
						rPrev, _ := utf8.DecodeLastRuneInString(text[i+2 : j])
						if rPrev != '*' && !isJSWhitespace(rPrev) {
							if j+2 == n || text[j+2] != '*' {
								matchedCloser = j
								break
							}
						}
					}
					break
				}
				j++
			}

			if matchedCloser != -1 {
				matchEnd := matchedCloser + 2
				match := text[offset:matchEnd]
				inner := text[offset+2 : matchedCloser]

				inOrTouchesCodeDelimiter := false
				for _, r := range codeRanges {
					if r.Start < matchEnd && r.End > offset &&
						!(r.Start >= offset+2 && r.End <= matchEnd-2) {
						inOrTouchesCodeDelimiter = true
						break
					}
				}

				sb.WriteString(text[lastIndex:offset])
				lastIndex = matchEnd

				shouldAppendSpace := false
				if !inOrTouchesCodeDelimiter {
					var nextChar rune
					if matchEnd < n {
						r, _ := utf8.DecodeRuneInString(text[matchEnd:])
						nextChar = r
					}
					lastInnerRune, _ := utf8.DecodeLastRuneInString(inner)
					if isBoldClosingPunct(lastInnerRune) && isBoldFollowingNonSpace(nextChar) {
						shouldAppendSpace = true
					}
				}

				sb.WriteString(match)
				if shouldAppendSpace {
					sb.WriteByte(' ')
				}

				i = matchEnd
				continue
			}

			i += 2
			continue
		}
		i++
	}

	sb.WriteString(text[lastIndex:])
	return sb.String()
}

// HandleMessageEnd intercepts message_end lifecycle event for assistant messages
// and rewrites table markdown and unsupported syntax to plain text / terminal-compatible formats.
func HandleMessageEnd(_ sdk.Context, data map[string]any) (any, error) {
	if data == nil {
		return nil, nil
	}
	rawMsg, ok := data["message"]
	if !ok || rawMsg == nil {
		return nil, nil
	}
	msg, ok := rawMsg.(map[string]any)
	if !ok || msg == nil {
		return nil, nil
	}

	role, _ := msg["role"].(string)
	if role != "assistant" {
		return nil, nil
	}

	content := msg["content"]
	if content == nil {
		return nil, nil
	}

	changed := false

	switch parts := content.(type) {
	case []any:
		newParts := make([]any, len(parts))
		for i, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part == nil {
				newParts[i] = rawPart
				continue
			}
			partType, _ := part["type"].(string)
			if partType != "text" {
				newParts[i] = part
				continue
			}
			partText, ok := part["text"].(string)
			if !ok {
				newParts[i] = part
				continue
			}

			converted := ConvertUnsupportedMarkdown(ConvertTables(partText))
			if converted != partText {
				changed = true
				newPart := make(map[string]any, len(part))
				for k, v := range part {
					newPart[k] = v
				}
				newPart["text"] = converted
				newParts[i] = newPart
			} else {
				newParts[i] = part
			}
		}

		if !changed {
			return nil, nil
		}

		newMsg := make(map[string]any, len(msg))
		for k, v := range msg {
			newMsg[k] = v
		}
		newMsg["content"] = newParts
		return map[string]any{"message": newMsg}, nil

	case string:
		converted := ConvertUnsupportedMarkdown(ConvertTables(parts))
		if converted == parts {
			return nil, nil
		}
		newMsg := make(map[string]any, len(msg))
		for k, v := range msg {
			newMsg[k] = v
		}
		newMsg["content"] = converted
		return map[string]any{"message": newMsg}, nil

	default:
		return nil, nil
	}
}

// Extension constructs and returns the no-tables extension instance for PiG.
func Extension() *sdk.Extension {
	e := sdk.New("no-tables")
	e.OnEvent(sdk.EventMessageEnd, HandleMessageEnd)
	return e
}
