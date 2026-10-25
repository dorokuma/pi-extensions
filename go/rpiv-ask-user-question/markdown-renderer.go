package rpivaskuserquestion

// A minimal markdown renderer for preview content. The npm original hands the
// preview text to pi-tui's `Markdown` component, which colors headings, lists
// and fenced code through the active MarkdownTheme. PiG exposes markdown as a
// host-rendered kit value, so the port renders the parts the preview box
// actually relies on — headings, bullets, fenced code bodies, inline code,
// rules and blank lines — through the same width-aware primitives the rest of
// the view layer uses. Styling stays optional: a nil theme renders plain text,
// which is what the headless tests assert against.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// markdownRenderer renders preview markdown at a width.
type markdownRenderer struct {
	theme sdk.UITheme
	// styled is false when the host has no palette (headless tests), which keeps
	// assertions readable and matches the "unknown tokens do not panic" path.
	styled bool
}

// newMarkdownRenderer builds a renderer for the given theme.
func newMarkdownRenderer(theme sdk.UITheme) markdownRenderer {
	return markdownRenderer{theme: theme, styled: theme.Name != ""}
}

// render lays the text out to width.
func (r markdownRenderer) render(text string, width int) []string {
	lines := splitLines(text)
	out := make([]string, 0, len(lines))
	inFence := false
	for _, raw := range lines {
		trimmed := strings.TrimRight(raw, " \t")
		// Fenced code: keep the fence markers (the caller strips them) and pass
		// the body through verbatim — code is not re-wrapped.
		if strings.HasPrefix(strings.TrimSpace(trimmed), "```") {
			inFence = !inFence
			out = append(out, trimmed)
			continue
		}
		if inFence {
			out = append(out, r.styleCode(trimmed))
			continue
		}
		out = append(out, r.renderLine(trimmed, width)...)
	}
	return out
}

// renderLine renders one non-code line.
func (r markdownRenderer) renderLine(line string, width int) []string {
	if strings.TrimSpace(line) == "" {
		return []string{""}
	}
	// Horizontal rule.
	if isRule(line) {
		return []string{r.paint("dim", strings.Repeat("─", minInt(width, 40)))}
	}
	// ATX heading: `#`..`######` then the text.
	if level, text, ok := heading(line); ok {
		styled := text
		if level <= 2 {
			styled = r.paint("accent", text)
		}
		if r.styled {
			styled = r.theme.Bold(styled)
		}
		return wrapTextWithAnsi(styled, width)
	}
	// Bullet list item: keep the marker, hang the continuation.
	if marker, text, ok := bullet(line); ok {
		prefix := marker + " "
		indent := strings.Repeat(" ", visibleWidth(prefix))
		wrapped := wrapTextWithAnsi(text, maxInt(1, width-visibleWidth(prefix)))
		lines := make([]string, 0, len(wrapped))
		for i, segment := range wrapped {
			if i == 0 {
				lines = append(lines, prefix+segment)
				continue
			}
			lines = append(lines, indent+segment)
		}
		return lines
	}
	return wrapTextWithAnsi(line, width)
}

// styleCode paints a fenced-code body line.
func (r markdownRenderer) styleCode(line string) string {
	if !r.styled {
		return line
	}
	return r.theme.Fg("muted", line)
}

// paint applies a foreground token when the theme has a palette.
func (r markdownRenderer) paint(token, text string) string {
	if !r.styled {
		return text
	}
	return r.theme.Fg(token, text)
}

// heading recognises an ATX heading, returning its level and text.
func heading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := line[level:]
	if rest == "" {
		return 0, "", false
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	return level, strings.TrimSpace(rest), true
}

// bullet recognises a list item, returning its marker and text.
func bullet(line string) (string, string, bool) {
	for _, marker := range []string{"-", "*", "+"} {
		if strings.HasPrefix(line, marker+" ") {
			return marker, strings.TrimSpace(line[len(marker)+1:]), true
		}
	}
	// Ordered items keep their number as the marker.
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits+1 < len(line) && line[digits] == '.' && line[digits+1] == ' ' {
		return line[:digits+1], strings.TrimSpace(line[digits+2:]), true
	}
	return "", "", false
}

// isRule recognises a horizontal rule (`---`, `***`, `___` with 3+ chars).
func isRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return false
	}
	marker := trimmed[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != marker {
			return false
		}
	}
	return true
}
