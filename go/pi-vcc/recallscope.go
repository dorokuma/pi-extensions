package pivcc

import "strings"

// Ports src/core/recall-scope.ts.

// RecallScope is recall-scope.ts RecallScope.
type RecallScope string

const (
	// ScopeLineage reads the active conversation path (the default).
	ScopeLineage RecallScope = "lineage"
	// ScopeAll also reads edited or retried branches.
	ScopeAll RecallScope = "all"
)

// RecallMode is recall-scope.ts RecallMode.
type RecallMode string

const (
	// ModeHybridRecall is the default search mode.
	ModeHybridRecall RecallMode = "hybrid"
	// ModeTouched lists files worked on with their entry indices.
	ModeTouched RecallMode = "touched"
)

// scopeRe is recall-scope.ts SCOPE_RE.
var scopeRe = compileIgnoreCase(`\bscope:(lineage|all)\b`)

// NormalizeRecallScope is recall-scope.ts normalizeRecallScope().
func NormalizeRecallScope(scope any) RecallScope {
	if text, ok := scope.(string); ok && strings.ToLower(text) == "all" {
		return ScopeAll
	}
	return ScopeLineage
}

// NormalizeRecallMode is recall-scope.ts normalizeRecallMode().
func NormalizeRecallMode(mode any) RecallMode {
	if text, ok := mode.(string); ok {
		switch strings.ToLower(text) {
		case "hybrid":
			return ModeHybridRecall
		case "touched":
			return ModeTouched
		}
	}
	return ModeHybridRecall
}

// ParseRecallScope is recall-scope.ts parseRecallScope(): pull `scope:all` out
// of a command's text.
func ParseRecallScope(text string) (RecallScope, string) {
	match := scopeRe.FindStringSubmatch(text)
	scope := ScopeLineage
	if match != nil {
		scope = NormalizeRecallScope(match[1])
	}
	cleaned := scopeRe.ReplaceAllString(text, "")
	cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
	return scope, jsTrimSpace(cleaned)
}
