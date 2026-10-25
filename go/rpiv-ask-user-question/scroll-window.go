package rpivaskuserquestion

// Ports view/scroll-window.ts and view/stateful-view.ts (v2.15.0): the shared
// scroll-window row maths and the prop-driven component contract.

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Overflow arrows.
const (
	overflowUp   = "↑"
	overflowDown = "↓"
	overflowBoth = "↕"
)

// ComputeScrollStart mirrors computeScrollStart: window start centred on the
// focused option, top-anchored when there is no interactive focus.
func ComputeScrollStart(bodyRange *[2]int, rowsBeforeBody, availableMiddle, middleRows int) int {
	if bodyRange == nil {
		return 0
	}
	focusedRowInMiddle := rowsBeforeBody + bodyRange[0]
	focusedHeight := bodyRange[1] - bodyRange[0]
	idealStart := focusedRowInMiddle - maxInt(0, availableMiddle-focusedHeight)/2
	return maxInt(0, minInt(idealStart, middleRows-availableMiddle))
}

// DecorateOverflow mirrors decorateOverflow: mark the window edges with overflow
// arrows, never drawing over a protected row.
func DecorateOverflow(scrollableMiddle []string, hasUp, hasDown bool, theme sdk.UITheme, protectedRows *[2]int) {
	drawable := func(row int) bool {
		return protectedRows == nil || row < protectedRows[0] || row >= protectedRows[1]
	}
	paint := func(text string) string { return theme.Fg("dim", text) }
	if hasUp && hasDown && len(scrollableMiddle) == 1 {
		// Single-row middle: the combined ↕ avoids the collision where ↓ would
		// overwrite ↑.
		if drawable(0) {
			scrollableMiddle[0] = paint(overflowBoth)
		}
		return
	}
	if hasUp && len(scrollableMiddle) > 0 && drawable(0) {
		scrollableMiddle[0] = paint(overflowUp)
	}
	last := len(scrollableMiddle) - 1
	if hasDown && last > 0 && drawable(last) {
		scrollableMiddle[last] = paint(overflowDown)
	}
}

// WindowLinesResult is windowLines' result.
type WindowLinesResult struct {
	Lines []string
	Focus *[2]int
}

// WindowLines mirrors windowLines: window lines to at most rows rows, centred on
// the focus range, with overflow arrows that never cover the focused rows.
func WindowLines(lines []string, rows int, focus *[2]int, theme sdk.UITheme) WindowLinesResult {
	if len(lines) <= rows {
		out := make([]string, len(lines))
		copy(out, lines)
		return WindowLinesResult{Lines: out, Focus: focus}
	}
	start := ComputeScrollStart(focus, 0, rows, len(lines))
	end := minInt(start+rows, len(lines))
	visible := make([]string, 0, end-start)
	visible = append(visible, lines[start:end]...)
	var inWindow *[2]int
	if focus != nil {
		inWindow = &[2]int{focus[0] - start, focus[1] - start}
	}
	DecorateOverflow(visible, start > 0, start+rows < len(lines), theme, inWindow)
	return WindowLinesResult{Lines: visible, Focus: inWindow}
}

// StatefulView is the generic prop-driven component contract: every renderable
// owns its own props type, computed from canonical state by a selector and
// pushed with SetProps.
type StatefulView[P any] interface {
	Render(width int) []string
	SetProps(props P)
	Invalidate()
	HandleInput(data string)
}

// OneLineClippedText is a single-row, width-clipped chrome cell. Clipping (not
// wrapping) keeps the footer row count invariant.
type OneLineClippedText struct {
	Text        string
	PaddingLeft int
}

// Render mirrors OneLineClippedText.render.
func (c OneLineClippedText) Render(width int) []string {
	pad := ""
	if c.PaddingLeft > 0 {
		pad = repeatSpace(c.PaddingLeft)
	}
	avail := maxInt(0, width-c.PaddingLeft)
	return []string{pad + truncateToWidth(c.Text, avail, "…", false)}
}

// repeatSpace is strings.Repeat(" ", n).
func repeatSpace(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = ' '
	}
	return string(buf)
}
