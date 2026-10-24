package autocontinue

import (
	"encoding/json"
	"regexp"
	"strings"
)

// jsSpaceBody is JavaScript's \s as the body of a character class
// ([\t\n\v\f\r \u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]).
// TS uses \s inside several regexes and String.prototype.trim; Go's \s is only
// [\t\n\f\r ], so every JS \s is spelled out with this body.
const jsSpaceBody = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsSpaceClass is the same set as a standalone Go character class.
const jsSpaceClass = `[` + jsSpaceBody + `]`

// jsDotBody is JavaScript's "." (any character but \n, \r, \u2028, \u2029),
// as the body of a character class. Go's "." already excludes \n but not the
// other line terminators.
const jsDotBody = `^\n\r\x{2028}\x{2029}`

// jsDotClass is JavaScript's "." in Go syntax.
const jsDotClass = `[` + jsDotBody + `]`

var (
	// reTruncatedEnd is TS auto-continue.ts:377 `/[：，、；:;,]$/u`.
	reTruncatedEnd = regexp.MustCompile(`[：，、；:;,]$`)

	// reMarkdownHeading is TS auto-continue.ts:384 `^ {0,3}#{1,6}(?:\s|$)`.
	reMarkdownHeading = regexp.MustCompile(`^ {0,3}#{1,6}(` + jsSpaceClass + `|$)`)

	// reMarkdownEmptyListItem is TS auto-continue.ts:385
	// `^ {0,3}(?:[-*+]|\d+[.)])\s*$`.
	reMarkdownEmptyListItem = regexp.MustCompile(`^ {0,3}([-*+]|[0-9]+[.)])` + jsSpaceClass + `*$`)

	// reEllipsisEnd is TS auto-continue.ts:387 `/(?:\.\.\.|…)$/u`.
	reEllipsisEnd = regexp.MustCompile(`(\.\.\.|…)$`)
)

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim mirrors JavaScript's String.prototype.trim.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// jsTrimRight mirrors TS auto-continue.ts:375 `text.replace(/\s+$/u, "")`.
func jsTrimRight(s string) string {
	return strings.TrimRightFunc(s, isJSSpace)
}

// asBlocks mirrors TS asBlocks (auto-continue.ts:333): a non-array content
// stands for "no blocks"; array members that are not objects behave as
// missing fields (JS `b?.type` is undefined for them).
func asBlocks(content any) []any {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	return blocks
}

// blockText mirrors TS blockText (auto-continue.ts:344): a string content is
// the text itself, otherwise every well-formed text block is concatenated.
func blockText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	var out strings.Builder
	for _, raw := range asBlocks(content) {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := block["type"].(string); blockType != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			out.WriteString(text)
		}
	}
	return out.String()
}

// hasThinkingBlock mirrors TS hasThinkingBlock (auto-continue.ts:352).
func hasThinkingBlock(content any) bool {
	for _, raw := range asBlocks(content) {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := block["type"].(string); blockType == "thinking" {
			return true
		}
	}
	return false
}

// hasToolCallBlock mirrors TS hasToolCallBlock (auto-continue.ts:356).
func hasToolCallBlock(content any) bool {
	for _, raw := range asBlocks(content) {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := block["type"].(string); blockType == "toolCall" {
			return true
		}
	}
	return false
}

// userMessageText mirrors TS userMessageText (auto-continue.ts:361).
func userMessageText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	return blockText(content)
}

// LooksTruncatedText mirrors TS looksTruncatedText (auto-continue.ts:374): does
// this assistant text stop mid-sentence? Only strong signals count, so a normal
// ending (period, question mark, closing bracket, table row) never matches.
func LooksTruncatedText(text string) bool {
	trimmed := jsTrimRight(text)
	if trimmed == "" {
		return false
	}
	// ① ends on a clause-opening punctuation mark
	if reTruncatedEnd.MatchString(trimmed) {
		return true
	}
	// ② an unclosed ``` fence (odd number of fences)
	if strings.Count(trimmed, "```")%2 == 1 {
		return true
	}
	lines := strings.Split(trimmed, "\n")
	lastLine := ""
	if len(lines) > 0 {
		lastLine = lines[len(lines)-1]
	}
	// ③ a markdown heading or an empty list item is the last line
	if reMarkdownHeading.MatchString(lastLine) {
		return true
	}
	if reMarkdownEmptyListItem.MatchString(lastLine) {
		return true
	}
	// ④ ends on an ellipsis
	return reEllipsisEnd.MatchString(trimmed)
}

// jsNumberOf mirrors `typeof v === "number"` plus the numeric coercions the
// message scans rely on: JSON numbers decode to float64, but a message built in
// Go may carry any integer type.
func jsNumberOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// positiveTs mirrors TS positiveTs (auto-continue.ts:423): only a finite,
// strictly positive timestamp is evidence.
func positiveTs(ts *float64) *float64 {
	if ts == nil {
		return nil
	}
	value := *ts
	if mathIsNaN(value) || mathIsInf(value) || value <= 0 {
		return nil
	}
	return ts
}

// lastToolUseTsFromMessages mirrors TS lastToolUseTsFromMessages
// (auto-continue.ts:392): the newest timestamp among tool results and
// assistant messages that carry a toolCall block.
func lastToolUseTsFromMessages(messages []map[string]any) *float64 {
	var newest *float64
	for _, message := range messages {
		ts, ok := jsNumberOf(message["timestamp"])
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role != "toolResult" && !hasToolCallBlock(message["content"]) {
			continue
		}
		if newest == nil || ts > *newest {
			value := ts
			newest = &value
		}
	}
	return newest
}

// lastUserTsFromMessages mirrors TS lastUserTsFromMessages
// (auto-continue.ts:410): the newest timestamp among user messages that are not
// this extension's own "继续" prompt.
func lastUserTsFromMessages(messages []map[string]any) *float64 {
	var newest *float64
	for _, message := range messages {
		role, _ := message["role"].(string)
		if role != "user" {
			continue
		}
		ts, ok := jsNumberOf(message["timestamp"])
		if !ok {
			continue
		}
		if jsTrim(userMessageText(message["content"])) == ContinuePrompt {
			continue
		}
		if newest == nil || ts > *newest {
			value := ts
			newest = &value
		}
	}
	return newest
}
