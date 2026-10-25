package pivcc

import (
	"regexp"
	"strings"
)

// Ports src/extract/goals.ts.

// scopeChangeRe is goals.ts SCOPE_CHANGE_RE.
var scopeChangeRe = compileIgnoreCase(`\b(instead|actually|change of plan|forget that|new task|switch to|now I want|pivot|let'?s do|stop .* and)\b`)

// taskRe is goals.ts TASK_RE.
var taskRe = compileIgnoreCase(`\b(fix|implement|add|create|build|refactor|debug|investigate|update|remove|delete|migrate|deploy|test|write|set up)\b`)

// noiseShortRe is goals.ts NOISE_SHORT_RE.
var noiseShortRe = compileIgnoreCase(`^(ok|yes|no|sure|yeah|yep|go|hi|hey|thx|thanks|ok\b.*|y|n|k)` + jsSpace + `*[.!?]*$`)

// nonGoalRe is goals.ts NON_GOAL_RE: pasted output, code, paths, tool dumps and
// command-template boilerplate.
var nonGoalRe = compileIgnoreCase(`^` + jsSpace + `*[\[│├└─╭╰]|` +
	"```" + `|^` + jsSpace + `*(=[A-Z]+\(|function |const |let |var |import |export |class )|^(https?:|file:|/[A-Za-z])|\\n|^` +
	jsSpace + `*For each\b|\bin full\b[^\n]*\b(comments|issue|issues|PRs?|linked)\b`)

// templateSignalRe is goals.ts TEMPLATE_SIGNAL_RE.
var templateSignalRe = compileIgnoreCase(`^` + jsSpace + `*(For each\b|Do NOT implement\b|Analyze and propose\b|If Task/context\b|Output:` +
	jsSpace + `*$)`)

// truncateAtTemplate is goals.ts truncateAtTemplate().
func truncateAtTemplate(lines []string) []string {
	for i, line := range lines {
		if templateSignalRe.MatchString(line) {
			return lines[:i]
		}
	}
	return lines
}

// leadingBulletRe is goals.ts's `/^\s*(?:[-*+]|\d+\.)\s+/`.
var leadingBulletRe = mustCompile(`^` + jsSpace + `*(?:[-*+]|\d+\.)` + jsSpace + `+`)

// stripLeadingBullet is goals.ts stripLeadingBullet().
func stripLeadingBullet(line string) string {
	return jsTrimSpace(leadingBulletRe.ReplaceAllString(line, ""))
}

// maxGoalChars is goals.ts MAX_GOAL_CHARS.
const maxGoalChars = 200

// isSubstantiveGoal is goals.ts isSubstantiveGoal().
func isSubstantiveGoal(text string) bool {
	trimmed := jsTrimSpace(text)
	if jsLen(trimmed) <= 5 || jsLen(trimmed) > maxGoalChars {
		return false
	}
	if noiseShortRe.MatchString(trimmed) {
		return false
	}
	if nonGoalRe.MatchString(trimmed) {
		return false
	}
	return true
}

// leadingChars is goals.ts LEADING_CHARS: scope-change / task intent is tested
// only on the leading portion of a user block.
const leadingChars = 200

// ExtractGoals is goals.ts extractGoals(): the first user block's bullets, plus
// the most recent scope change or task re-statement, capped at 8 lines.
func ExtractGoals(blocks []NormalizedBlock) []string {
	goals := make([]string, 0, 8)
	var latestScopeChange []string

	for _, block := range blocks {
		if block.Kind != KindUser {
			continue
		}
		rawLines := NonEmptyLines(block.Text)
		truncated := truncateAtTemplate(rawLines)
		filtered := make([]string, 0, len(truncated))
		for _, line := range truncated {
			if isSubstantiveGoal(line) {
				filtered = append(filtered, line)
			}
		}
		collapsed := CollapseSkillLines(filtered)
		lines := make([]string, 0, len(collapsed))
		for _, line := range collapsed {
			bulleted := stripLeadingBullet(line)
			if jsLen(bulleted) > 5 {
				lines = append(lines, bulleted)
			}
		}
		if len(lines) == 0 {
			continue
		}

		if len(goals) == 0 {
			goals = append(goals, head(lines, 6)...)
			continue
		}

		leading := jsSlice(block.Text, 0, leadingChars)
		if scopeChangeRe.MatchString(leading) {
			latestScopeChange = clipAll(head(lines, 3), maxGoalChars)
		} else if taskRe.MatchString(leading) && jsLen(lines[0]) > 15 {
			latestScopeChange = clipAll(head(lines, 2), maxGoalChars)
		}
	}

	// Only emit the [Scope change] marker when we actually captured bullets.
	if len(latestScopeChange) > 0 {
		goals = append(goals, "[Scope change]")
		goals = append(goals, latestScopeChange...)
	}
	return head(goals, 8)
}

func head(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

func clipAll(values []string, max int) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, Clip(value, max))
	}
	return out
}

// ── preferences ─────────────────────────────────────────────────────────────

// prefPatterns is extract/preferences.ts PREF_PATTERNS.
var prefPatterns = []*regexp.Regexp{
	compileIgnoreCase(`\bprefer(?:s|red|ring)?` + jsSpace + `+\w`),
	compileIgnoreCase(`\bdon'?t want\b`),
	compileIgnoreCase(`\balways (?:use|do|run|prefer|keep|make|format|write|add|set|put|prefix|start|include|append)\b`),
	compileIgnoreCase(`\bnever (?:use|do|run|push|commit|write|ignore|add|set|put|remove|delete|include|deploy)\b`),
	compileIgnoreCase(`\bplease (?:use|avoid|keep|make|don'?t|do not|format|write)\b`),
	compileIgnoreCase(`\b(?:style|format|language|naming)` + jsSpace + `*[:=]` + jsSpace + `*\S`),
}

// ExtractPreferences is preferences.ts extractPreferences(): at most one
// preference line per user block, deduped case-insensitively, capped at 10.
func ExtractPreferences(blocks []NormalizedBlock) []string {
	prefs := make([]string, 0, 10)
	seen := make(map[string]bool)
	for _, block := range blocks {
		if block.Kind != KindUser {
			continue
		}
		perBlock := 0
		for _, line := range NonEmptyLines(block.Text) {
			trimmed := jsTrimSpace(line)
			if trimmed == "" || jsLen(trimmed) < 5 || jsLen(trimmed) > 200 {
				continue
			}
			// Reject questions.
			if strings.HasSuffix(trimmed, "?") || strings.Contains(trimmed, "?...") {
				continue
			}
			matched := false
			for _, pattern := range prefPatterns {
				if pattern.MatchString(trimmed) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			clipped := Clip(trimmed, 200)
			key := strings.ToLower(clipped)
			if seen[key] {
				continue
			}
			seen[key] = true
			prefs = append(prefs, clipped)
			// Cap per user block so a pasted rule list is not many prefs.
			perBlock++
			if perBlock >= 1 {
				break
			}
		}
	}
	return head(prefs, 10)
}

// DedupPreferencesAgainstGoals is preferences.ts dedupPreferencesAgainstGoals().
func DedupPreferencesAgainstGoals(prefs, goals []string) []string {
	goalSet := make(map[string]bool, len(goals))
	for _, goal := range goals {
		goalSet[strings.ToLower(jsTrimSpace(goal))] = true
	}
	out := make([]string, 0, len(prefs))
	for _, pref := range prefs {
		if goalSet[strings.ToLower(jsTrimSpace(pref))] {
			continue
		}
		out = append(out, pref)
	}
	return out
}
