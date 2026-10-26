package pivcc

import "strings"

// Ports src/core/skill-collapse.ts: collapse <skill> blocks so a repeated skill
// card renders once.

// skillBlockOpenRe is the `<skill name="X" ...>` opener of
// skill-collapse.ts's `<skill\s+name="([^"]+)"[^>]*>[\s\S]*?(?:<\/skill>|$)`.
// RE2 has no lazy quantifier or lookahead, so replaceSkillBlocks scans with this
// opener and cuts each body at the next closing tag itself.
var skillBlockOpenRe = mustCompile(`<skill` + jsSpace + `+name="([^"]+)"[^>]*>`)

// skillBlockClose is the closing tag the body scan looks for.
const skillBlockClose = "</skill>"

// skillBlockMatch is one SKILL_BLOCK_RE match: the byte range it covers and the
// name its first group captured.
type skillBlockMatch struct {
	start int
	end   int
	name  string
}

// skillBlockMatches is every match of skill-collapse.ts SKILL_BLOCK_RE, left to
// right. The `$` in the pattern is end-of-input in JS (no /m flag), which is
// why a body with no closing tag runs to the end of the text.
func skillBlockMatches(text string) []skillBlockMatch {
	out := make([]skillBlockMatch, 0, 4)
	last := 0
	for {
		loc := skillBlockOpenRe.FindStringSubmatchIndex(text[last:])
		if loc == nil {
			return out
		}
		start := last + loc[0]
		name := text[last+loc[2] : last+loc[3]]
		body := last + loc[1]
		end := len(text)
		if closeIdx := strings.Index(text[body:], skillBlockClose); closeIdx >= 0 {
			end = body + closeIdx + len(skillBlockClose)
		}
		out = append(out, skillBlockMatch{start: start, end: end, name: name})
		last = end
	}
}

// replaceSkillBlocks is `text.replace(SKILL_BLOCK_RE, replacement)` for a global
// pattern whose replacement is computed from the match. It returns the rewritten
// text and the captured names in match order, the two things its two callers
// (collapseSkillText and user-messages.ts userLine) need.
func replaceSkillBlocks(text string, replacement func(name string) string) (string, []string) {
	matches := skillBlockMatches(text)
	if len(matches) == 0 {
		return text, nil
	}
	var b strings.Builder
	names := make([]string, 0, len(matches))
	last := 0
	for _, match := range matches {
		b.WriteString(text[last:match.start])
		b.WriteString(replacement(match.name))
		names = append(names, match.name)
		last = match.end
	}
	b.WriteString(text[last:])
	return b.String(), names
}

// CollapseSkillText is skill-collapse.ts collapseSkillText(): every
// <skill name="X">…</skill> (or unterminated tail) becomes `[skill: X]`.
func CollapseSkillText(text string) string {
	out, _ := replaceSkillBlocks(text, func(name string) string {
		return "[skill: " + name + "]"
	})
	return out
}

// whitespaceRunRe is user-messages.ts's `/\s+/g`.
var whitespaceRunRe = mustCompile(jsSpace + `+`)

// collapseWhitespace is `text.replace(/\s+/g, " ").trim()`.
func collapseWhitespace(text string) string {
	return jsTrimSpace(whitespaceRunRe.ReplaceAllString(text, " "))
}
