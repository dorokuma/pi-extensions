package pivcc

import "strings"

// Ports src/core/skill-collapse.ts: collapse <skill> blocks so a repeated skill
// card renders once.

// skillTagRe is /^-?\s*<skill\s+name="([^"]+)"/ for a single line.
var skillTagRe = mustCompile(`^-?` + jsSpace + `*<skill` + jsSpace + `+name="([^"]+)"`)

// skillCloseRe is /^-?\s*<\/skill>/.
var skillCloseRe = mustCompile(`^-?` + jsSpace + `*</skill>`)

// skillBlockOpenRe is the `<skill name="X" ...>` opener of
// skill-collapse.ts's `<skill\s+name="([^"]+)"[^>]*>[\s\S]*?(?:<\/skill>|$)`.
// RE2 has no lazy quantifier or lookahead, so CollapseSkillText scans with this
// opener and cuts each body at the next closing tag itself.
var skillBlockOpenRe = mustCompile(`<skill` + jsSpace + `+name="([^"]+)"[^>]*>`)

// skillBlockClose is the closing tag the body scan looks for.
const skillBlockClose = "</skill>"

// CollapseSkillLines is skill-collapse.ts collapseSkillLines(): dedup by name
// and drop everything inside a block.
func CollapseSkillLines(lines []string) []string {
	result := make([]string, 0, len(lines))
	seen := make(map[string]bool)
	inside := false
	for _, line := range lines {
		if match := skillTagRe.FindStringSubmatch(line); match != nil {
			inside = true
			name := match[1]
			if !seen[name] {
				seen[name] = true
				result = append(result, "[skill: "+name+"]")
			}
			continue
		}
		if inside {
			if skillCloseRe.MatchString(line) {
				inside = false
			}
			continue
		}
		result = append(result, line)
	}
	return result
}

// CollapseSkillText is skill-collapse.ts collapseSkillText(): every
// <skill name="X">…</skill> (or unterminated tail) becomes `[skill: X]`.
func CollapseSkillText(text string) string {
	var b strings.Builder
	last := 0
	for {
		loc := skillBlockOpenRe.FindStringSubmatchIndex(text[last:])
		if loc == nil {
			break
		}
		start := last + loc[0]
		name := text[last+loc[2] : last+loc[3]]
		body := last + loc[1]
		// The body runs to the first closing tag, or to the end of input.
		end := len(text)
		if closeIdx := strings.Index(text[body:], skillBlockClose); closeIdx >= 0 {
			end = body + closeIdx + len(skillBlockClose)
		}
		b.WriteString(text[last:start])
		b.WriteString("[skill: ")
		b.WriteString(name)
		b.WriteString("]")
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}

var _ = strings.Join
