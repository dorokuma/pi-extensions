package herdsmanpi

// Upstream model error classifier for Herdsman Pi wake filtering.
//
// Ports upstream-error.ts byte-for-byte in behaviour. The TS module is
// dependency-free by design; this port keeps that property. It only normalises
// text and applies pattern rules and never reads files, env vars or the network.
//
// Regex translation notes (JS -> Go RE2):
//   - RE2 has no lookahead/lookbehind, so the TS `(?![...])` negative
//     lookaheads on the Chinese tokens are re-expressed as an explicit
//     "matched, but the next rune is a continuation particle" check in
//     startStrongToken. The set of rejected particles is kept character for
//     character.
//   - JS `\s` includes NBSP and the Unicode space separators; RE2's `\s` is
//     ASCII-only. The patterns that can meet non-ASCII whitespace spell the
//     class out as jsSpaceClass below.

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxErrorShapedChars mirrors MAX_ERROR_SHAPED_CHARS.
const MaxErrorShapedChars = 400

// WakeFilterConfig mirrors the TS `WakeFilterConfig`.
type WakeFilterConfig struct {
	Enabled       bool
	ExtraPatterns []string
}

// DefaultWakeFilterConfig mirrors DEFAULT_WAKE_FILTER_CONFIG.
var DefaultWakeFilterConfig = WakeFilterConfig{Enabled: true, ExtraPatterns: []string{}}

// UpstreamErrorMatch mirrors `{ matched: false } | { matched: true; pattern }`.
type UpstreamErrorMatch struct {
	Matched bool
	Pattern string
}

// jsSpaceClass is JS `\s` as a RE2 class: ASCII whitespace plus the Unicode
// space separators RE2 recognises.
const jsSpaceClass = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

// envelopePattern mirrors ENVELOPE_PATTERN.
var envelopePattern = regexp.MustCompile(`(?i)^(?:api` + jsSpaceClass + `+error|error` + jsSpaceClass + `*[:：]|connection` + jsSpaceClass + `+error` + jsSpaceClass + `*[:：]|the` + jsSpaceClass + `+model` + jsSpaceClass + `+is` + jsSpaceClass + `+(?:currently` + jsSpaceClass + `+)?overloaded|econnreset|etimedout|enotfound|eai_again|socket` + jsSpaceClass + `+hang` + jsSpaceClass + `+up|fetch` + jsSpaceClass + `+failed|und_err_|request` + jsSpaceClass + `+timed` + jsSpaceClass + `+out|timeout` + jsSpaceClass + `+of` + jsSpaceClass + `+\d+` + jsSpaceClass + `*ms` + jsSpaceClass + `+exceeded|rate_limit_error|overloaded_error|resource_exhausted|insufficient_quota|you` + jsSpaceClass + `+exceeded` + jsSpaceClass + `+your` + jsSpaceClass + `+current` + jsSpaceClass + `+quota|quota` + jsSpaceClass + `+exceeded|(?:429|503|529)` + jsSpaceClass + `+(?:too` + jsSpaceClass + `+many` + jsSpaceClass + `+requests|service` + jsSpaceClass + `+unavailable|overloaded|bad` + jsSpaceClass + `+gateway|gateway` + jsSpaceClass + `+timeout))`)

// codeStrongTokenPattern mirrors CODE_STRONG_TOKEN_PATTERN.
var codeStrongTokenPattern = regexp.MustCompile(`(?i)\b(?:rate_limit_error|overloaded_error|resource_exhausted|insufficient_quota|ECONNRESET|ECONNREFUSED|ETIMEDOUT|ENOTFOUND|EAI_AGAIN|und_err_)\w*\b`)

// statusContextHead and statusContextTail are STATUS_CONTEXT_PATTERN split at
// the `(?=...)` lookahead, which RE2 cannot express. statusContextMatch
// requires the head at the start of the text and the tail immediately after
// it, which is what the TS lookahead asserts. The `\b` after the status code
// needs no explicit form: every tail alternative rejects a following digit
// (end of text, punctuation, or whitespace), so a head match that stopped
// inside a longer digit run can never satisfy the tail — the same verdict the
// TS `\b` reaches.
var statusContextHead = regexp.MustCompile(`(?i)^(?:error|err|failed)\b` + jsSpaceClass + `*(?:(?:status|code|with)` + jsSpaceClass + `*)*[:=]?` + jsSpaceClass + `*(?:429|503|529)`)

var statusContextTail = regexp.MustCompile(`(?i)^$|^` + jsSpaceClass + `*[:：,，。.！!?？;；)]|^` + jsSpaceClass + `+(?:rate|overload|limit|exceed|quota|unavailable|busy|slow|too` + jsSpaceClass + `+many|retry|please|try)\b`)

// statusContextMatch mirrors STATUS_CONTEXT_PATTERN.test(text).
func statusContextMatch(text string) bool {
	head := statusContextHead.FindString(text)
	if head == "" {
		return false
	}
	return statusContextTail.MatchString(text[len(head):])
}

// substantiveHeadingPattern mirrors SUBSTANTIVE_HEADING_PATTERN.
var substantiveHeadingPattern = regexp.MustCompile(`(?m)^#{1,6}` + jsSpaceClass)

// delimitedExtraPattern mirrors DELIMITED_EXTRA_PATTERN.
var delimitedExtraPattern = regexp.MustCompile(`^/(.+)/([gimsuy]*)$`)

// vtControlPattern mirrors VT_CONTROL_PATTERN: the CSI class the TS
// normaliser strips; stray ESC/BEL bytes (OSC introducers included) fall out
// through CONTROL_CHARS_PATTERN exactly like the TS, so the two-step cleaning
// stays faithful to the original.
var vtControlPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// controlCharsPattern mirrors CONTROL_CHARS_PATTERN: C0 controls except
// \t \n \r, the C1 range, and DEL.
var controlCharsPattern = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]`)

// whitespaceRunPattern mirrors /\s+/g for phrase normalisation.
var whitespaceRunPattern = regexp.MustCompile(string(jsSpaceClass) + `+`)

// startStrongTokenPattern is START_STRONG_TOKEN_PATTERN's *positive* branch:
// the alternatives without the trailing negative lookahead, which
// startStrongToken applies itself.
var startStrongTokenPattern = regexp.MustCompile(`(?i)^(?:(?:the` + jsSpaceClass + `+)?request` + jsSpaceClass + `+failed|rate[_ -]?limit` + jsSpaceClass + `+(?:reached|exceeded|hit|error|exhausted)|socket` + jsSpaceClass + `+hang` + jsSpaceClass + `+up|fetch` + jsSpaceClass + `+failed|request` + jsSpaceClass + `+timed` + jsSpaceClass + `+out|timeout` + jsSpaceClass + `+of` + jsSpaceClass + `+\d+` + jsSpaceClass + `*ms` + jsSpaceClass + `+exceeded|quota` + jsSpaceClass + `+exceeded|you` + jsSpaceClass + `+exceeded` + jsSpaceClass + `+your` + jsSpaceClass + `+current` + jsSpaceClass + `+quota|the` + jsSpaceClass + `+model` + jsSpaceClass + `+is` + jsSpaceClass + `+(?:currently` + jsSpaceClass + `+)?overloaded)`)

// chineseStrongTokens are the four start-anchored Chinese tokens, each with
// its TS negative lookahead over the continuation particles that turn an
// error report into an explanation.
var chineseStrongTokens = []struct {
	token string
	// reject lists the runes the TS lookahead rejects after the token.
	reject string
}{
	{"频率限制", "与和及已的以而还也但并了在是也"},
	{"请求过于频繁", "与和及已的以而还也但并了在是也"},
	{"模型过载", "与和及已的以而还也但并了在是也"},
	{"资源耗尽", "与和及已的以而还也但并了在是也"},
}

// builtInPattern is one row of BUILT_IN_PATTERNS.
type builtInPattern struct {
	label string
	// pattern holds the regexp rows; matcher the rows that need more than a
	// single RE2 (the status-code family, whose TS form uses a lookahead).
	// Exactly one of the two is set.
	pattern *regexp.Regexp
	matcher func(string) bool
}

// matches reports whether the row matches text.
func (entry builtInPattern) matches(text string) bool {
	if entry.pattern != nil {
		return entry.pattern.MatchString(text)
	}
	if entry.matcher != nil {
		return entry.matcher(text)
	}
	return false
}

// builtInPatterns mirror BUILT_IN_PATTERNS in order: the first matching row
// names the match, so the order is part of the contract.
var builtInPatterns = []builtInPattern{
	// T1 — explicit transport/provider error tokens.
	{"api error", regexp.MustCompile(`(?i)\bapi` + jsSpaceClass + `+error\b` + jsSpaceClass + `*[:：]?` + jsSpaceClass + `*[45]\d\d\b`), nil},
	{"error envelope", regexp.MustCompile(`(?i)\berror` + jsSpaceClass + `*[:：]` + jsSpaceClass + `*[45]\d\d\b`), nil},
	{"request failed", regexp.MustCompile(`(?i)\brequest` + jsSpaceClass + `+failed\b`), nil},
	{"rate_limit_error", regexp.MustCompile(`(?i)\brate_limit_error\b`), nil},
	{"overloaded_error", regexp.MustCompile(`(?i)\boverloaded_error\b`), nil},
	{"resource_exhausted", regexp.MustCompile(`(?i)\bresource_exhausted\b`), nil},
	{"insufficient_quota", regexp.MustCompile(`(?i)\binsufficient_quota\b`), nil},
	{"you exceeded your current quota", regexp.MustCompile(`(?i)\byou` + jsSpaceClass + `+exceeded` + jsSpaceClass + `+your` + jsSpaceClass + `+current` + jsSpaceClass + `+quota\b`), nil},
	{"quota exceeded", regexp.MustCompile(`(?i)\bquota` + jsSpaceClass + `+exceeded\b`), nil},
	{"the model is overloaded", regexp.MustCompile(`(?i)\bthe` + jsSpaceClass + `+model` + jsSpaceClass + `+is` + jsSpaceClass + `+(?:currently` + jsSpaceClass + `+)?overloaded\b`), nil},
	// T2 — rate limiting phrasing with an explicit action/failure context.
	{"rate limit hit", regexp.MustCompile(`(?i)\brate[_ -]?limit` + jsSpaceClass + `+(?:reached|exceeded|hit|error|exhausted)\b`), nil},
	// T3 — transport failures (no bare \btimeout\b / \berror\b matching).
	{"connection error", regexp.MustCompile(`(?i)\bconnection` + jsSpaceClass + `+error\b` + jsSpaceClass + `*[:：]`), nil},
	{"econnreset", regexp.MustCompile(`(?i)\bECONNRESET\b`), nil},
	{"econnrefused", regexp.MustCompile(`(?i)\bECONNREFUSED\b`), nil},
	{"etimedout", regexp.MustCompile(`(?i)\bETIMEDOUT\b`), nil},
	{"enotfound", regexp.MustCompile(`(?i)\bENOTFOUND\b`), nil},
	{"eai_again", regexp.MustCompile(`(?i)\bEAI_AGAIN\b`), nil},
	{"socket hang up", regexp.MustCompile(`(?i)\bsocket` + jsSpaceClass + `+hang` + jsSpaceClass + `+up\b`), nil},
	{"fetch failed", regexp.MustCompile(`(?i)\bfetch` + jsSpaceClass + `+failed\b`), nil},
	{"und_err_", regexp.MustCompile(`(?i)\bund_err_[\w]*`), nil},
	{"request timed out", regexp.MustCompile(`(?i)\brequest` + jsSpaceClass + `+timed` + jsSpaceClass + `+out\b`), nil},
	{"timeout of Nms exceeded", regexp.MustCompile(`(?i)\btimeout` + jsSpaceClass + `+of` + jsSpaceClass + `+\d+` + jsSpaceClass + `*ms` + jsSpaceClass + `+exceeded\b`), nil},
	// T4 — bare status codes only when they co-occur with an error context.
	{"status code", nil, statusContextMatch},
	// T5 — a bare HTTP status line that names the failure itself.
	{"bare status line", regexp.MustCompile(`(?i)^(?:429|503|529)` + jsSpaceClass + `+(?:too` + jsSpaceClass + `+many` + jsSpaceClass + `+requests|service` + jsSpaceClass + `+unavailable|overloaded|bad` + jsSpaceClass + `+gateway|gateway` + jsSpaceClass + `+timeout)`), nil},
	// The Chinese strong tokens are start-anchored and are matched by
	// startStrongToken rather than by a row here; the rows exist so the label
	// of a match keeps coming from the same ordered table.
	{"频率限制", nil, nil},
	{"请求过于频繁", nil, nil},
	{"模型过载", nil, nil},
	{"资源耗尽", nil, nil},
}

// normalizeText mirrors the TS `normalizeText`.
func normalizeText(value *string) string {
	raw := ""
	if value != nil {
		raw = *value
	}
	cleaned := vtControlPattern.ReplaceAllString(raw, "")
	cleaned = controlCharsPattern.ReplaceAllString(cleaned, "")
	cleaned = whitespaceRunPattern.ReplaceAllString(cleaned, " ")
	return strings.TrimSpace(cleaned)
}

// matchesExtraPattern mirrors the TS `matchesExtraPattern`. An invalid custom
// regular expression is skipped without affecting the other rules.
func matchesExtraPattern(text, candidate string) bool {
	raw := strings.TrimSpace(candidate)
	if raw == "" {
		return false
	}
	if delimited := delimitedExtraPattern.FindStringSubmatch(raw); delimited != nil {
		source := delimited[1]
		flags := delimited[2]
		expression, err := compileJSRegexp(source, flags)
		if err != nil {
			return false
		}
		return expression.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(raw))
}

// jsRegExpFlags are the six flags a JS `/pattern/flags` literal accepts. JS
// rejects anything else (and rejects a repeated flag) at construction time, so
// an operator who writes `/foo/x` gets a warning and a pattern that never
// matches instead of a pattern whose flags were silently dropped.
const jsRegExpFlags = "gimsuy"

// compileJSRegexp translates a JS regex source and flags string into a Go
// regexp, honouring `i`, `m` and `s`; `g` and `u` are no-ops for matching and
// `y` (sticky) becomes a text-start anchor: a sticky match runs with
// lastIndex=0, so it may only succeed at the very start of the text.
func compileJSRegexp(source, flags string) (*regexp.Regexp, error) {
	seen := map[rune]bool{}
	for _, flag := range flags {
		if !strings.ContainsRune(jsRegExpFlags, flag) || seen[flag] {
			return nil, fmt.Errorf("invalid regexp flags: %q", flags)
		}
		seen[flag] = true
	}
	prefix := ""
	if seen['i'] {
		prefix += "(?i)"
	}
	if seen['m'] {
		prefix += "(?m)"
	}
	if seen['s'] {
		prefix += "(?s)"
	}
	pattern := source
	if seen['y'] {
		// \A is RE2's "start of text" (unlike ^, it is not moved by (?m)), which
		// is exactly what a sticky match with lastIndex=0 asserts.
		pattern = `\A(?:` + source + `)`
	}
	return regexp.Compile(prefix + pattern)
}

// startStrongToken mirrors the START_STRONG_TOKEN_PATTERN test, including the
// Chinese negative lookaheads RE2 cannot express.
func startStrongToken(head string) bool {
	if startStrongTokenPattern.MatchString(head) {
		return true
	}
	for _, entry := range chineseStrongTokens {
		if !strings.HasPrefix(head, entry.token) {
			continue
		}
		rest := head[len(entry.token):]
		if rest == "" {
			return true
		}
		next := []rune(rest)[0]
		if !strings.ContainsRune(entry.reject, next) {
			return true
		}
	}
	return false
}

// head120 is the TS `normalized.slice(0, 120)`: the *first 120 UTF-16 code
// units*. Astral characters count twice there, so the cut is done on UTF-16
// units and never splits a surrogate pair.
func head120(normalized string) string {
	units := 0
	for index, char := range normalized {
		width := 1
		if char > 0xFFFF {
			width = 2
		}
		if units+width > 120 {
			return normalized[:index]
		}
		units += width
	}
	return normalized
}

// utf16Len is JS `value.length`: the number of UTF-16 code units, so an astral
// character counts twice. Every length threshold the TS takes against a JS
// string must use this instead of len(), which counts bytes.
func utf16Len(value string) int {
	units := 0
	for _, char := range value {
		if char > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}

// isErrorShaped mirrors the TS `isErrorShaped`. Long texts must start with a
// recognizable error envelope; short texts must also carry an envelope at the
// start or a strong error token.
func isErrorShaped(text string) bool {
	normalized := normalizeText(&text)
	if normalized == "" {
		return false
	}
	head := head120(normalized)
	if utf16Len(normalized) > MaxErrorShapedChars {
		return envelopePattern.MatchString(head)
	}
	return envelopePattern.MatchString(head) ||
		startStrongToken(head) ||
		codeStrongTokenPattern.MatchString(normalized) ||
		statusContextMatch(normalized)
}

// hasSubstantiveWork mirrors the TS `hasSubstantiveWork`.
func hasSubstantiveWork(normalized string) bool {
	if strings.Contains(normalized, "```") {
		return true
	}
	if substantiveHeadingPattern.MatchString(normalized) {
		return true
	}
	return utf16Len(normalized) > MaxErrorShapedChars && !envelopePattern.MatchString(head120(normalized))
}

// MatchUpstreamModelError mirrors the TS `matchUpstreamModelError`.
func MatchUpstreamModelError(text *string, config WakeFilterConfig) UpstreamErrorMatch {
	if !config.Enabled {
		return UpstreamErrorMatch{}
	}
	normalized := normalizeText(text)
	if normalized == "" {
		return UpstreamErrorMatch{}
	}

	// Custom patterns bypass the error-shaped latch: an operator who configures
	// one has explicitly opted into matching it.
	for _, candidate := range config.ExtraPatterns {
		if matchesExtraPattern(normalized, candidate) {
			return UpstreamErrorMatch{Matched: true, Pattern: "extra:" + strings.TrimSpace(candidate)}
		}
	}

	if !isErrorShaped(normalized) {
		return UpstreamErrorMatch{}
	}
	if hasSubstantiveWork(normalized) {
		return UpstreamErrorMatch{}
	}

	if startStrongToken(head120(normalized)) {
		// The envelope and strong-token families overlap, so the label still
		// comes from the ordered table: a row naming the same family wins.
		for _, entry := range builtInPatterns {
			if entry.matches(normalized) {
				return UpstreamErrorMatch{Matched: true, Pattern: entry.label}
			}
		}
		for _, entry := range chineseStrongTokens {
			if strings.HasPrefix(head120(normalized), entry.token) {
				return UpstreamErrorMatch{Matched: true, Pattern: entry.token}
			}
		}
	}

	for _, builtIn := range builtInPatterns {
		if builtIn.matches(normalized) {
			return UpstreamErrorMatch{Matched: true, Pattern: builtIn.label}
		}
	}
	return UpstreamErrorMatch{}
}

// IsUpstreamModelError mirrors the TS `isUpstreamModelError`.
func IsUpstreamModelError(text *string, config WakeFilterConfig) bool {
	return MatchUpstreamModelError(text, config).Matched
}
