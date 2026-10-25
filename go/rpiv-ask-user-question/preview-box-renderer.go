package rpivaskuserquestion

// Ports view/components/preview/preview-box-renderer.ts (v2.15.0): the rounded
// preview box, the fence-marker stripper, and the box-dimension maths.

import (
	"regexp"
	"strings"
)

// ANSI_SGR_RE / ANSI_OSC8_RE mirror the TS regexes.
var (
	ansiSGRRe   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	ansiOSC8Re  = regexp.MustCompile(`\x1b\]8;[^\x07\x1b]*(?:\x07|\x1b\\)`)
	fenceMarkRe = regexp.MustCompile("^`{3}")
)

// Box geometry constants.
const (
	// BorderVerticalOverhead is the top + bottom border rows.
	BorderVerticalOverhead = 2
	// BorderHorizontalOverhead is the left + right vertical bar columns.
	BorderHorizontalOverhead = 2
	// BorderInnerPaddingHorizontal is the 1-col padding between bar and content.
	BorderInnerPaddingHorizontal = 1
	// BoxMinContentWidth is the floor for the preview box's inner content width.
	BoxMinContentWidth = 40
)

// StripFenceMarkers mirrors stripFenceMarkers: drop fenced-code-block marker
// lines from rendered markdown, leaving only the code body. Inline code is
// unaffected.
func StripFenceMarkers(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		clean := ansiOSC8Re.ReplaceAllString(ansiSGRRe.ReplaceAllString(line, ""), "")
		if fenceMarkRe.MatchString(clean) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// RenderBorderedBox mirrors renderBorderedBox: wrap lines in a 4-sided border
// with 1 col of inner padding. When hidden > 0, the bottom dash run is replaced
// with a "N lines hidden" indicator (the corners stay).
func RenderBorderedBox(lines []string, width int, colorFn func(string) string, hidden int) []string {
	if colorFn == nil {
		colorFn = func(s string) string { return s }
	}
	dashSpan := maxInt(1, width-BorderHorizontalOverhead)
	contentInner := maxInt(1, dashSpan-2*BorderInnerPaddingHorizontal)
	pad := strings.Repeat(" ", BorderInnerPaddingHorizontal)

	out := make([]string, 0, len(lines)+2)
	out = append(out, colorFn("╭"+strings.Repeat("─", dashSpan)+"╮"))
	for _, line := range lines {
		padded := truncateToWidth(line, contentInner, "", true)
		out = append(out, colorFn("│")+pad+padded+pad+colorFn("│"))
	}
	if hidden > 0 {
		indicator := " ✂ ── " + itoa(hidden) + " lines hidden ── "
		space := dashSpan - visibleWidth(indicator)
		leftFill := strings.Repeat("─", maxInt(0, space/2))
		rightFill := strings.Repeat("─", maxInt(0, dashSpan-len(leftFill)-len(indicator)))
		out = append(out, colorFn("╰"+leftFill+indicator+rightFill+"╯"))
		return out
	}
	out = append(out, colorFn("╰"+strings.Repeat("─", dashSpan)+"╯"))
	return out
}

// BoxDimensions is computeBoxDimensions' result.
type BoxDimensions struct {
	InnerWidth int
	BoxWidth   int
}

// ComputeBoxDimensions mirrors computeBoxDimensions:
//
//	contentWidth = max(minWidth, widestRenderedLine)
//	boxWidth     = min(contentWidth + 4, effectiveMaxWidth)
//
// Trailing whitespace is stripped before measuring because the markdown
// renderer pads every line to the width.
func ComputeBoxDimensions(contentLines []string, maxInnerWidth int) BoxDimensions {
	widest := minInt(BoxMinContentWidth, maxInnerWidth)
	for _, line := range contentLines {
		trimmed := strings.TrimRight(line, " \t")
		if w := visibleWidth(trimmed); w > widest {
			widest = w
		}
	}
	innerWidth := minInt(widest, maxInnerWidth)
	return BoxDimensions{
		InnerWidth: innerWidth,
		BoxWidth:   innerWidth + BorderHorizontalOverhead + 2*BorderInnerPaddingHorizontal,
	}
}
