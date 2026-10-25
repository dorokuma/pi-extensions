package rpivaskuserquestion

// Ports view/components/multi-select-view.ts (v2.15.0): the multi-select option
// list — pointer + checkbox + label rows, the Type-something. row, and the Next
// row.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Multi-select row glyphs.
const (
	msChecked     = "[✔]"
	msUnchecked   = "[ ]"
	msBoxLabelGap = " "
	// Continuation indent: CC parity — descriptions indent to col 2 (past the
	// pointer slot), not to the full prefix column.
	msContinuationIndent = "  "
)

// MultiSubmitLabel is the Next row's label on the last question tab.
const MultiSubmitLabel = "Submit"

// MultiSelectOtherRowProps projects the Type-something. row.
type MultiSelectOtherRowProps struct {
	// Active marks the focused row (OptionIndex == len(options)).
	Active bool
	// Checked: non-empty custom text counts as a selected multi-value.
	Checked bool
	// InputMode is state.InputMode — keystrokes append to the buffer.
	InputMode bool
	// InputBuffer is the live inline-input text.
	InputBuffer string
	// InputCursorOffset is the inline-input cursor.
	InputCursorOffset *int
}

// MultiSelectViewProps is one tick's multi-select projection.
type MultiSelectViewProps struct {
	Rows       []MultiSelectRowProps
	Other      MultiSelectOtherRowProps
	NextActive bool
	NextLabel  string
}

// MultiSelectRowProps is one option row's state.
type MultiSelectRowProps struct {
	Checked bool
	Active  bool
}

// multiSelectLayout is the width-keyed derived layout (render, height and
// focused-row measurement all come from it).
type multiSelectLayout struct {
	Lines        []string
	FocusedRange [2]int
}

// MultiSelectView renders a multi-select question.
type MultiSelectView struct {
	theme    sdk.UITheme
	question QuestionData
	props    MultiSelectViewProps
	cached   *multiSelectLayout
	cachedW  int
}

// NewMultiSelectView constructs the view.
func NewMultiSelectView(theme sdk.UITheme, question QuestionData) *MultiSelectView {
	return &MultiSelectView{
		theme:    theme,
		question: question,
		props: MultiSelectViewProps{
			Other:     MultiSelectOtherRowProps{},
			NextLabel: DisplayLabel(SentinelNext),
		},
		cachedW: -1,
	}
}

// SetProps replaces the projection and drops the derived cache.
func (v *MultiSelectView) SetProps(props MultiSelectViewProps) {
	v.props = props
	v.cached = nil
}

// HandleInput is empty — input is routed at the dialog level.
func (v *MultiSelectView) HandleInput(string) {}

// Invalidate drops the derived cache.
func (v *MultiSelectView) Invalidate() { v.cached = nil }

// Render mirrors render().
func (v *MultiSelectView) Render(width int) []string { return v.layout(width).Lines }

// FocusedItemRowRange mirrors focusedItemRowRange.
func (v *MultiSelectView) FocusedItemRowRange(width int) [2]int { return v.layout(width).FocusedRange }

// NaturalHeight mirrors naturalHeight.
func (v *MultiSelectView) NaturalHeight(width int) int { return len(v.layout(width).Lines) }

// layout mirrors layout(): one width-keyed computation for render, height and
// focus.
func (v *MultiSelectView) layout(width int) multiSelectLayout {
	if v.cached != nil && v.cachedW == width {
		return *v.cached
	}
	build := multiSelectLayout{FocusedRange: [2]int{0, 0}}
	contentWidth := maxInt(1, width-v.prefixVisibleWidth())
	numberWidth := len(itoa(maxInt(1, len(v.question.Options)+1)))

	v.appendOptionRows(&build, width, contentWidth, numberWidth)

	otherStart := len(build.Lines)
	build.Lines = append(build.Lines, v.renderOtherRow(contentWidth, numberWidth)...)
	if v.props.Other.Active {
		build.FocusedRange = [2]int{otherStart, len(build.Lines)}
	}

	v.appendNextRow(&build, width)

	v.cached = &build
	v.cachedW = width
	return build
}

// appendOptionRows mirrors appendOptionRows.
func (v *MultiSelectView) appendOptionRows(build *multiSelectLayout, width, contentWidth, numberWidth int) {
	for i := range v.question.Options {
		option := v.question.Options[i]
		if i >= len(v.props.Rows) {
			continue
		}
		row := v.props.Rows[i]
		start := len(build.Lines)
		pointer := inactivePointer
		if row.Active {
			pointer = v.theme.Fg("accent", activePointer)
		}
		box := v.theme.Fg("muted", msUnchecked)
		if row.Checked {
			box = v.theme.Fg("accent", msChecked)
		}
		label := truncateToWidth(option.Label, contentWidth, "…", false)
		styledLabel := label
		if row.Active {
			styledLabel = v.theme.Fg("accent", v.theme.Bold(label))
		}
		number := padStart(itoa(i+1), numberWidth)
		build.Lines = append(build.Lines, truncateToWidth(
			pointer+number+numberSeparator+box+msBoxLabelGap+styledLabel, width, "", false))
		if option.Description != "" {
			for _, segment := range wrapTextWithAnsi(option.Description, contentWidth) {
				build.Lines = append(build.Lines, msContinuationIndent+v.theme.Fg("muted", segment))
			}
		}
		if row.Active {
			build.FocusedRange = [2]int{start, len(build.Lines)}
		}
	}
}

// appendNextRow mirrors appendNextRow.
func (v *MultiSelectView) appendNextRow(build *multiSelectLayout, width int) {
	nextStart := len(build.Lines)
	pointer := inactivePointer
	label := v.props.NextLabel
	if v.props.NextActive {
		pointer = v.theme.Fg("accent", activePointer)
		label = v.theme.Fg("accent", v.theme.Bold(label))
	}
	build.Lines = append(build.Lines, truncateToWidth(pointer+label, width, "", false))
	if v.props.NextActive {
		build.FocusedRange = [2]int{nextStart, len(build.Lines)}
	}
}

// renderOtherRow mirrors renderOtherRow.
func (v *MultiSelectView) renderOtherRow(contentWidth, numberWidth int) []string {
	other := v.props.Other
	pointer := inactivePointer
	if other.Active {
		pointer = v.theme.Fg("accent", activePointer)
	}
	box := v.theme.Fg("muted", msUnchecked)
	if other.Checked {
		box = v.theme.Fg("accent", msChecked)
	}
	number := padStart(itoa(len(v.question.Options)+1), numberWidth)
	rowPrefix := pointer + number + numberSeparator + box + msBoxLabelGap
	continuationPrefix := strings.Repeat(" ", visibleWidth(rowPrefix))
	selectedText := func(text string) string { return v.theme.Fg("accent", v.theme.Bold(text)) }

	if other.Active && other.InputMode {
		return RenderInlineInputRow(RenderInlineInputOptions{
			Buffer:             other.InputBuffer,
			CursorOffset:       other.InputCursorOffset,
			RowPrefix:          rowPrefix,
			ContinuationPrefix: continuationPrefix,
			ContentWidth:       contentWidth,
			SelectedText:       selectedText,
		})
	}

	text := other.InputBuffer
	if text == "" {
		text = DisplayLabel(SentinelOther)
	}
	segments := wrapTextWithAnsi(text, contentWidth)
	lines := make([]string, 0, len(segments))
	for i, segment := range segments {
		prefix := continuationPrefix
		if i == 0 {
			prefix = rowPrefix
		}
		line := prefix + segment
		if other.Active {
			line = selectedText(line)
		}
		lines = append(lines, line)
	}
	return lines
}

// prefixVisibleWidth mirrors prefixVisibleWidth: the state-independent canonical
// prefix for option rows (pointer + digits + separator + box + gap).
func (v *MultiSelectView) prefixVisibleWidth() int {
	numberWidth := len(itoa(maxInt(1, len(v.question.Options)+1)))
	return visibleWidth(inactivePointer) + numberWidth +
		visibleWidth(numberSeparator+msUnchecked+msBoxLabelGap)
}
