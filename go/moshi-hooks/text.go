package moshihooks

// Text helpers, ported one-to-one from moshi-hooks.ts (L215-368, L761-767).
//
// The TypeScript source strings come from an agent transcript, so every
// transformation below mirrors a JavaScript regular expression or string
// operation exactly — including JavaScript's `\s` character set, which is wider
// than Go's `\s` (ASCII only). `jsSpaceClass` is the ECMA-262 WhiteSpace +
// LineTerminator set and is substituted everywhere the TS used `\s`.

import (
	crand "crypto/rand"
	"encoding/hex"
	randv2 "math/rand/v2"
	"regexp"
	"strings"
)

// jsSpaceClass is the body of a Go character class matching exactly the
// characters JavaScript's `\s` matches (ECMA-262 WhiteSpace + LineTerminator).
const jsSpaceClass = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsSpacePattern is jsSpaceClass as a bracket expression.
const jsSpacePattern = "[" + jsSpaceClass + "]"

// isJSSpace reports whether r is one of the characters JavaScript's `\s` matches.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680',
		'\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// jsTrim mirrors String.prototype.trim, which trims exactly `\s`.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

var (
	// TS mdCodeBlock = /```[a-zA-Z0-9_-]*\n[\s\S]*?```/g (L314).
	mdCodeBlock = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\n.*?```")
	// TS mdInlineCode = /`([^`]*)`/g (L315).
	mdInlineCode = regexp.MustCompile("`([^`]*)`")
	// TS mdLink = /\[([^\]]+)\]\([^)]+\)/g (L316).
	mdLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	// TS mdBoldStar = /\*\*([^*]+)\*\*/g (L317).
	mdBoldStar = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	// TS mdBoldUnder = /__([^_]+)__/g (L318).
	mdBoldUnder = regexp.MustCompile(`__([^_]+)__`)
	// TS mdItalicStar = /(^|[^*])\*([^*\n]+)\*/g (L319).
	mdItalicStar = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
	// TS mdItalicUnder = /(^|[^_])_([^_\n]+)_/g (L320).
	mdItalicUnder = regexp.MustCompile(`(^|[^_])_([^_\n]+)_`)
	// TS mdLineNoise = /^\s*(#{1,6}\s+|[>\-*+]\s+|\d+\.\s+)/gm (L321).
	mdLineNoise = regexp.MustCompile(`(?m)^` + jsSpacePattern + `*(#{1,6}` + jsSpacePattern + `+|[>\-*+]` + jsSpacePattern + `+|\d+\.` + jsSpacePattern + `+)`)
	// TS mdWhitespace = /\s+/g (L322).
	mdWhitespace = regexp.MustCompile(jsSpacePattern + `+`)

	// TS userQueryRe = /<user_query>\s*([\s\S]*?)\s*<\/user_query>/i (L351).
	userQueryRe = regexp.MustCompile(`(?is)<user_query>` + jsSpacePattern + `*(.*?)` + jsSpacePattern + `*</user_query>`)
	// TS skillInfoRe = /<skill_information>[\s\S]*?<\/skill_information>/gi (L352).
	skillInfoRe = regexp.MustCompile(`(?is)<skill_information>.*?</skill_information>`)
	// TS imageFilesRe = /<image_files>[\s\S]*?<\/image_files>/gi (L353).
	imageFilesRe = regexp.MustCompile(`(?is)<image_files>.*?</image_files>`)
	// TS systemRemRe = /<system-reminder>[\s\S]*?<\/system-reminder>/gi (L354).
	systemRemRe = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)

	// TS planSummary leading-noise strip: /^[#>*\-\s]+/ (L762).
	planLeadingNoise = regexp.MustCompile(`^[#>*\-` + jsSpaceClass + `]+`)

	// TS `.replace(/[\/\\]+$/, "")` in projectNameFromCwd (L206).
	trailingSeparators = regexp.MustCompile(`[/\\]+$`)
)

// firstString returns the first non-empty string among the values
// (TS: firstString, L215).
func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

// textFromContent flattens a message content value (TS: textFromContent, L222).
func textFromContent(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	chunks, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		var text string
		switch typed := chunk.(type) {
		case string:
			text = typed
		case map[string]any:
			text, _ = typed["text"].(string)
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

// nestedObject reads a nested object member, treating a non-object as absent.
func nestedObject(value map[string]any, key string) map[string]any {
	nested, _ := value[key].(map[string]any)
	return nested
}

// textFromMessage flattens a message object (TS: textFromMessage, L238).
func textFromMessage(value any) string {
	message, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return firstString(
		textFromContent(message["content"]),
		textFromContent(nestedObject(message, "message")["content"]),
		textFromContent(nestedObject(message, "entry")["content"]),
	)
}

// modelLabel picks a display label from a model object
// (TS: modelLabel, L248).
func modelLabel(model any) string {
	object, ok := model.(map[string]any)
	if !ok {
		return ""
	}
	return firstString(object["displayName"], object["display_name"], object["name"], object["id"], object["modelId"])
}

// stripMarkdown mirrors stripMarkdown (L324): code blocks dropped wholesale,
// inline pairs keep their text, everything collapses to one line.
func stripMarkdown(s string) string {
	if s == "" {
		return ""
	}
	s = mdCodeBlock.ReplaceAllString(s, "")
	s = mdInlineCode.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdBoldStar.ReplaceAllString(s, "$1")
	s = mdBoldUnder.ReplaceAllString(s, "$1")
	s = mdItalicStar.ReplaceAllString(s, "$1$2")
	s = mdItalicUnder.ReplaceAllString(s, "$1$2")
	s = mdLineNoise.ReplaceAllString(s, "")
	s = mdWhitespace.ReplaceAllString(s, " ")
	return jsTrim(s)
}

// truncateText keeps at most n runes, ending an over-long string with an
// ellipsis (TS: truncateText, L338).
func truncateText(s string, n int) string {
	chars := []rune(s)
	if len(chars) <= n {
		return s
	}
	keep := n - 1
	if keep < 0 {
		keep = 0
	}
	return string(chars[:keep]) + "…"
}

// plainTextClip is stripMarkdown followed by truncateText
// (TS: plainTextClip, L344).
func plainTextClip(s string, n int) string { return truncateText(stripMarkdown(s), n) }

// displayUserPrompt drops the wrappers an agent puts around the typed ask
// (TS: displayUserPrompt, L356).
func displayUserPrompt(s string) string {
	s = jsTrim(s)
	if s == "" {
		return ""
	}
	if match := userQueryRe.FindStringSubmatch(s); match != nil {
		return jsTrim(match[1])
	}
	s = skillInfoRe.ReplaceAllString(s, "")
	s = imageFilesRe.ReplaceAllString(s, "")
	s = systemRemRe.ReplaceAllString(s, "")
	return jsTrim(s)
}

// formatUserPromptDetail is the "You: …" subtitle
// (TS: formatUserPromptDetail, L365).
func formatUserPromptDetail(prompt string) string {
	s := plainTextClip(displayUserPrompt(prompt), 200)
	if s == "" {
		return ""
	}
	return "You: " + s
}

// planSummary is the plan's first prose line, for the inbox row
// (TS: planSummary, L761).
func planSummary(plan string) string {
	for _, line := range strings.Split(plan, "\n") {
		text := jsTrim(planLeadingNoise.ReplaceAllString(line, ""))
		if text != "" {
			return text
		}
	}
	return ""
}

// newSessionID returns 16 random bytes as hex (TS: newSessionID, L300).
func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := crand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(randv2.Uint32())
		}
	}
	return hex.EncodeToString(buf)
}

// stringPtr is the "assigned in the TS, so present in the JSON" marker for the
// per-event envelope fields.
func stringPtr(value string) *string { return &value }

// intPtr marks an optional number field as present.
func intPtr(value int) *int { return &value }
