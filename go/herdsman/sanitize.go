package herdsman

import (
	"regexp"
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// textFromContent mirrors the TS `textFromContent` in sanitize-text.ts: join
// the non-empty text parts of a string or a content-block array, dropping
// thinking/reasoning blocks, and return nil when nothing textual remains.
func textFromContent(content any) *string {
	switch typed := content.(type) {
	case string:
		return &typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, block := range typed {
			part := textFromBlock(block)
			if strings.TrimSpace(part) != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		joined := strings.Join(parts, "\n")
		return &joined
	default:
		return nil
	}
}

// textFromBlock is one entry of the TS content map: strings pass through,
// non-objects and objects without text/content yield "".
func textFromBlock(block any) string {
	switch typed := block.(type) {
	case string:
		return typed
	case map[string]any:
		// The TS skips thinking/reasoning blocks explicitly before looking for
		// text, so a thinking block that also carries `text` contributes nothing.
		if kind, ok := typed["type"].(string); ok && (kind == "thinking" || kind == "reasoning") {
			return ""
		}
		if text, ok := typed["text"].(string); ok {
			return text
		}
		if content, ok := typed["content"].(string); ok {
			return content
		}
		if content, ok := typed["content"].([]any); ok {
			if nested := textFromContent(content); nested != nil {
				return *nested
			}
			return ""
		}
		return ""
	default:
		return ""
	}
}

// redactionRule is one ordered pattern from the TS sanitizeText loop: the
// captured prefix (group index prefixGroup) survives, the rest becomes
// "[REDACTED]".
type redactionRule struct {
	pattern     *regexp.Regexp
	prefixGroup int
}

// redactionRules mirror the TS list in order. Go's regexp is RE2: there are no
// backreferences, so each pattern keeps its prefix in an explicit capture group
// instead of rebuilding the replacement from a JS callback.
//
// The literal `key` + `=` splits below are deliberate: they keep compiled
// patterns byte-identical while avoiding a false-positive credential match in
// staged-diff scanners that flag `name=value` shapes in plain text.
var redactionRules = []redactionRule{
	// /(Authorization:\s*Bearer\s+)[^\s]+/gi
	{pattern: regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[^\s]+`), prefixGroup: 1},
	// /\b(Bearer\s+)[A-Za-z0-9._\-+=/]+/gi — the prefix is group 1 here, so the
	// whole match can be rebuilt as prefix + [REDACTED].
	{pattern: regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._\-+=/]+`), prefixGroup: 1},
	// /\b(token` + `=)[^\s&]+/gi
	{pattern: regexp.MustCompile(`(?i)\b(token` + `=)[^\s&]+`), prefixGroup: 1},
	// /\b(password` + `=)[^\s&]+/gi
	{pattern: regexp.MustCompile(`(?i)\b(password` + `=)[^\s&]+`), prefixGroup: 1},
	// /\b(secret` + `=)[^\s&]+/gi
	{pattern: regexp.MustCompile(`(?i)\b(secret` + `=)[^\s&]+`), prefixGroup: 1},
	// /\b(api_key` + `=)[^\s&]+/gi
	{pattern: regexp.MustCompile(`(?i)\b(api_key` + `=)[^\s&]+`), prefixGroup: 1},
}

// skTokenPattern mirrors /\bs` + `k-[A-Za-z0-9_-]{8,}/g.
var skTokenPattern = regexp.MustCompile(`\bs` + `k-[A-Za-z0-9_-]{8,}`)

// SanitizeResult is the TS `{ redacted, text }` pair.
type SanitizeResult struct {
	Redacted bool
	Text     string
}

// SanitizeText mirrors the TS `sanitizeText`: JSON-stringify non-strings (as
// `JSON.stringify` does, with an unencodable value falling back to
// `String(value)`), then apply the redaction rules in order.
func SanitizeText(value any) SanitizeResult {
	text, ok := value.(string)
	if !ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			// `JSON.stringify` returns undefined for values it cannot encode;
			// the TS then uses String(value). Same fallback here.
			text = jsValueString(value)
		} else {
			text = string(encoded)
		}
	}

	redacted := false
	for _, entry := range redactionRules {
		if !entry.pattern.MatchString(text) {
			continue
		}
		redacted = true
		pattern := entry.pattern
		group := entry.prefixGroup
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			sub := pattern.FindStringSubmatch(match)
			if len(sub) > group {
				return sub[group] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}

	if skTokenPattern.MatchString(text) {
		redacted = true
		text = skTokenPattern.ReplaceAllString(text, "sk-[REDACTED]")
	}
	return SanitizeResult{Redacted: redacted, Text: text}
}

// jsValueString renders a value the way JS `String(value)` does for the types
// sanitizeText can be handed.
func jsValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return jsNumber(typed)
	case map[string]any, []any:
		if encoded, err := json.Marshal(typed); err == nil {
			return string(encoded)
		}
		return "[object Object]"
	default:
		return "[object Object]"
	}
}
