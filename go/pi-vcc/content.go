package pivcc

import (
	"regexp"
	"strings"
)

// Ports src/core/content.ts: clipping, text extraction and the shared
// content-bearing heuristic.

// Clip truncates text to at most max UTF-16 code units, preferring a word
// boundary, and never splits a surrogate pair (content.ts clip()).
func Clip(text string, max int) string {
	if jsLen(text) <= max {
		return text
	}
	cut := jsLastIndexByteSpaceBefore(text, max)
	end := max
	if float64(cut) > float64(max)*0.6 {
		end = cut
	}
	// Avoid splitting a surrogate pair: the code unit before `end` is a high
	// surrogate, so step back one unit.
	if end > 0 && end < jsLen(text) {
		if code, ok := jsCharCodeAt(text, end-1); ok && code >= 0xD800 && code <= 0xDBFF {
			end--
		}
	}
	return jsSlice(text, 0, end)
}

// sentenceEndRe finds `[.!?]` at the end of a line or followed by whitespace,
// matching `/[.!?](?:\s|$)/g`.
var sentenceEndRe = mustCompile(`[.!?](?:` + jsSpace + `|$)`)

// ClipSentence truncates at the last sentence boundary at or before max,
// falling back to Clip (content.ts clipSentence()).
func ClipSentence(text string, max int) string {
	if jsLen(text) <= max {
		return text
	}
	window := jsSlice(text, 0, max)
	matches := sentenceEndRe.FindAllStringIndex(window, -1)
	if len(matches) > 0 {
		last := matches[len(matches)-1]
		end := jsLen(window[:last[0]]) + 1 // include the punctuation
		if float64(end) >= float64(max)*0.5 {
			return jsSlice(text, 0, end)
		}
	}
	return Clip(text, max)
}

// NonEmptyLines returns the trimmed, non-empty lines of text
// (content.ts nonEmptyLines()).
func NonEmptyLines(text string) []string {
	out := make([]string, 0, 8)
	for _, line := range jsSplitLines(text) {
		if trimmed := jsTrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// FirstLine is the first line of text, clipped (content.ts firstLine()).
func FirstLine(text string, max int) string {
	lines := jsSplitLines(text)
	first := ""
	if len(lines) > 0 {
		first = lines[0]
	}
	return Clip(first, max)
}

// TextParts is content.ts textParts(): the text of every text part, and the
// content itself when it is a bare string.
func TextParts(content any) []string {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))
		for _, part := range value {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := block["type"].(string)
			if kind != "text" {
				continue
			}
			text, _ := block["text"].(string)
			out = append(out, text)
		}
		return out
	}
	return nil
}

// TextOf is content.ts textOf(): every text part joined with "\n".
func TextOf(content any) string {
	return strings.Join(TextParts(content), "\n")
}

// PathKeys is content.ts PATH_KEYS: the tool-argument keys that name a file.
var PathKeys = []string{"path", "file_path", "filePath", "file"}

// IsContentBearing is content.ts isContentBearing(): a call that has a path
// argument and at least one large string/array content field. Generic
// heuristic, not dependent on tool names.
func IsContentBearing(args map[string]any) bool {
	if args == nil {
		return false
	}
	hasPath := false
	for _, key := range PathKeys {
		if _, ok := args[key].(string); ok {
			hasPath = true
			break
		}
	}
	if !hasPath {
		return false
	}
	if content, ok := args["content"].(string); ok && jsLen(content) > 0 {
		return true
	}
	if edits, ok := args["edits"].([]any); ok && len(edits) > 0 {
		all := true
		for _, edit := range edits {
			if _, isObject := edit.(map[string]any); !isObject {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	_, editsPresent := args["edits"]
	if !editsPresent {
		if oldText, ok := args["oldText"].(string); ok && jsLen(oldText) > 0 {
			return true
		}
		if newText, ok := args["newText"].(string); ok && jsLen(newText) > 0 {
			return true
		}
	}
	return false
}

// ExtractToolCallText is content.ts extractToolCallText(): the textual content
// of a call's arguments (content, edits, oldText/newText), used for counting
// touched-file lines.
func ExtractToolCallText(args map[string]any) string {
	var b strings.Builder
	if content, ok := args["content"].(string); ok {
		b.WriteString(content)
		b.WriteString("\n")
	}
	edits, hasEditsArray := args["edits"].([]any)
	if hasEditsArray {
		for _, edit := range edits {
			block, isObject := edit.(map[string]any)
			if !isObject {
				continue
			}
			if oldText, ok := block["oldText"].(string); ok {
				b.WriteString(oldText)
				b.WriteString("\n")
			}
			if newText, ok := block["newText"].(string); ok {
				b.WriteString(newText)
				b.WriteString("\n")
			}
		}
	}
	if oldText, ok := args["oldText"].(string); ok && !hasEditsArray {
		b.WriteString(oldText)
		b.WriteString("\n")
	}
	if newText, ok := args["newText"].(string); ok && !hasEditsArray {
		b.WriteString(newText)
		b.WriteString("\n")
	}
	return b.String()
}

// ExtractToolCallArgsText is content.ts extractToolCallArgsText(): every scalar
// string argument, plus strings one level into array-of-object fields.
func ExtractToolCallArgsText(args map[string]any) string {
	if args == nil {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, key := range sortedKeys(args) {
		value := args[key]
		switch value := value.(type) {
		case string:
			parts = append(parts, value)
		case []any:
			for _, item := range value {
				switch item := item.(type) {
				case string:
					parts = append(parts, item)
				case map[string]any:
					for _, innerKey := range sortedKeys(item) {
						if inner, ok := item[innerKey].(string); ok {
							parts = append(parts, inner)
						}
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// Snippet is content.ts snippet(): ~radius chars around the first
// case-insensitive match of term, or "" plus false when there is none.
func Snippet(text, term string, radius int) (string, bool) {
	idx := indexFold(text, term)
	if idx < 0 {
		return "", false
	}
	units := jsUnits(text)
	termUnits := jsUnits(term)
	start := idx - radius
	if start < 0 {
		start = 0
	}
	end := idx + len(termUnits) + radius
	if end > len(units) {
		end = len(units)
	}
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(units) {
		suffix = "..."
	}
	return prefix + jsFromUnits(units[start:end]) + suffix, true
}

// indexFold is `text.toLowerCase().indexOf(term.toLowerCase())` in code units.
func indexFold(text, term string) int {
	lowerText := jsToLower(text)
	lowerTerm := jsToLower(term)
	idx := strings.Index(lowerText, lowerTerm)
	if idx < 0 {
		return -1
	}
	// Convert the byte offset into a code-unit offset.
	return jsLen(lowerText[:idx])
}

// jsToLower is `String.prototype.toLowerCase`, whose length can differ from
// the input's; callers only use it for finding a substring of the lowered
// text, so the mapping is applied whole.
func jsToLower(s string) string {
	return strings.ToLower(s)
}

// regexpEscape is the `s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")` idiom used by
// several modules.
func regexpEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '.', '*', '+', '?', '^', '$', '{', '}', '(', ')', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// regexpEscapeRe is the compiled form of the same escape set.
var regexpEscapeRe = mustCompile(`[.*+?^${}()|\[\]\\]`)

// escapeRegexp is search-entries.ts escapeRegex().
func escapeRegexp(s string) string {
	return regexpEscape(s)
}

// sortedKeys returns a map's keys in sorted order. The TS original reads
// `Object.keys`, whose order is insertion order for string keys; this port
// sorts instead, because a Go map has no insertion order. It is noted as a
// deviation in README.md and only reaches output where the TS order is itself
// unspecified-looking (tool argument names).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(values []string) {
	if len(values) < 2 {
		return
	}
	quickSortStrings(values, 0, len(values)-1)
}

func quickSortStrings(values []string, lo, hi int) {
	for lo < hi {
		if hi-lo < 12 {
			for i := lo + 1; i <= hi; i++ {
				for j := i; j > lo && values[j] < values[j-1]; j-- {
					values[j], values[j-1] = values[j-1], values[j]
				}
			}
			return
		}
		mid := lo + (hi-lo)/2
		if values[mid] < values[lo] {
			values[mid], values[lo] = values[lo], values[mid]
		}
		if values[hi] < values[lo] {
			values[hi], values[lo] = values[lo], values[hi]
		}
		if values[hi] < values[mid] {
			values[hi], values[mid] = values[mid], values[hi]
		}
		pivot := values[mid]
		i, j := lo, hi
		for i <= j {
			for values[i] < pivot {
				i++
			}
			for values[j] > pivot {
				j--
			}
			if i <= j {
				values[i], values[j] = values[j], values[i]
				i++
				j--
			}
		}
		if j-lo < hi-i {
			quickSortStrings(values, lo, j)
			lo = i
		} else {
			quickSortStrings(values, i, hi)
			hi = j
		}
	}
}

var _ = regexpEscapeRe
var _ = regexp.MustCompile
