package rpivaskuserquestion

// Ports view/components/preview/preview-pane.ts (v2.15.0): the thin layout
// composer that puts the option list and the preview block side by side (or
// stacked).

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// minBoxRows is the smallest preview box worth showing: top border, one content
// line, bottom border.
const minBoxRows = 3

// PreviewPaneProps is one tick's preview-pane projection.
type PreviewPaneProps struct {
	NotesVisible bool
	// HasNote: the question carries a saved note, so the affordance offers edit.
	HasNote       bool
	SelectedIndex int
	Focused       bool
	// InputMode: the other row is focused and accepting input, so the pane
	// renders the option list at full width instead of the cramped left column.
	InputMode bool
}

// PreviewPaneConfig is the construction-time config.
type PreviewPaneConfig struct {
	Question         QuestionData
	Theme            sdk.UITheme
	GetTerminalWidth func() int
	OptionListView   *OptionListView
	PreviewBlock     *PreviewBlockRenderer
}

// PreviewPane lays out the options column and the preview block.
type PreviewPane struct {
	question         QuestionData
	getTerminalWidth func() int
	optionListView   *OptionListView
	previewBlock     *PreviewBlockRenderer
	theme            sdk.UITheme
	props            PreviewPaneProps
	// rowAllowance is set by the dialog before each render; nil is unconstrained.
	rowAllowance *int
	// globalLeftWidth is injected once by the questionnaire builder. The sentinel
	// fails loudly rather than silently falling back to a magic constant.
	globalLeftWidth func(paneWidth int) int
}

// NewPreviewPane constructs the pane.
func NewPreviewPane(config PreviewPaneConfig) *PreviewPane {
	return &PreviewPane{
		question:         config.Question,
		getTerminalWidth: config.GetTerminalWidth,
		optionListView:   config.OptionListView,
		previewBlock:     config.PreviewBlock,
		theme:            config.Theme,
		globalLeftWidth: func(int) int {
			panic("PreviewPane.SetGlobalLeftWidth must be called before Render")
		},
	}
}

// SetGlobalLeftWidth injects the cross-tab left-width getter.
func (p *PreviewPane) SetGlobalLeftWidth(getter func(paneWidth int) int) {
	p.globalLeftWidth = getter
}

// SetProps replaces the projection.
func (p *PreviewPane) SetProps(props PreviewPaneProps) { p.props = props }

// SetRowAllowance records the rows the dialog can give this body this frame.
func (p *PreviewPane) SetRowAllowance(rows *int) { p.rowAllowance = rows }

// HandleInput is empty — the pane is layout only.
func (p *PreviewPane) HandleInput(string) {}

// Invalidate drops both children's caches.
func (p *PreviewPane) Invalidate() {
	p.previewBlock.Invalidate()
	p.optionListView.Invalidate()
}

// notesAffordance mirrors notesAffordance.
func (p *PreviewPane) notesAffordance() NotesAffordance {
	if !p.props.Focused || p.props.NotesVisible {
		return AffordanceNone
	}
	if p.props.HasNote {
		return AffordanceEdit
	}
	return AffordanceAdd
}

// Render mirrors render().
func (p *PreviewPane) Render(width int) []string {
	if p.question.MultiSelect {
		return p.optionListView.Render(width)
	}
	// Hide the preview pane entirely when no option carries a preview.
	if !p.previewBlock.HasAnyPreview() {
		return p.optionListView.Render(width)
	}
	// inputMode: the preview is irrelevant, so render the option list at full
	// pane width instead of the cramped left column.
	if p.props.InputMode {
		return p.optionListView.Render(width)
	}
	return p.layout(width).lines
}

// paneLayout is one layout pass's output.
type paneLayout struct {
	lines []string
	focus *[2]int
}

// layout mirrors layout().
func (p *PreviewPane) layout(width int) paneLayout {
	mode := DecideLayout(p.getTerminalWidth(), width)
	if mode == LayoutSideBySide {
		return p.layoutSideBySide(width, mode)
	}
	return p.layoutStacked(width, mode)
}

// layoutStacked mirrors layoutStacked: options, a blank gap, then the preview
// block — fitted to the allowance when the natural height exceeds it.
func (p *PreviewPane) layoutStacked(width int, mode PreviewLayoutMode) paneLayout {
	affordance := p.notesAffordance()
	options := p.optionListView.Render(width)
	focus := p.optionListView.FocusedItemRowRange(width)
	gap := make([]string, StackedGapRows)
	fullBlock := p.previewBlock.BlockHeight(width, p.props.SelectedIndex, mode, nil)
	if p.rowAllowance == nil || len(options)+StackedGapRows+fullBlock <= *p.rowAllowance {
		lines := make([]string, 0, len(options)+len(gap)+fullBlock)
		lines = append(lines, options...)
		lines = append(lines, gap...)
		lines = append(lines, p.previewBlock.RenderBlock(width, p.props.SelectedIndex, mode, affordance, nil)...)
		return paneLayout{lines: lines, focus: &focus}
	}
	allowance := *p.rowAllowance
	// Prefer keeping a row either side of the focused option for the overflow
	// arrows; give those up only when they are what keeps the preview box from
	// fitting.
	focusRows := 1
	if focus != ([2]int{}) {
		focusRows = maxInt(1, focus[1]-focus[0])
	}
	previewRowsWith := func(optionsFloor int) int {
		return minInt(fullBlock, allowance-StackedGapRows-minInt(len(options), optionsFloor))
	}
	roomy := previewRowsWith(focusRows + 2)
	previewRows := roomy
	if roomy < minBoxRows {
		previewRows = previewRowsWith(focusRows)
	}
	block := p.previewBlock.RenderBlock(width, p.props.SelectedIndex, mode, affordance, &previewRows)
	blockOverhead := 0
	if len(block) > 0 {
		blockOverhead = StackedGapRows + len(block)
	}
	optionRows := allowance - blockOverhead
	windowed := WindowLines(options, maxInt(1, optionRows), &focus, p.theme)
	if len(block) == 0 {
		return paneLayout{lines: windowed.Lines, focus: windowed.Focus}
	}
	lines := make([]string, 0, len(windowed.Lines)+len(gap)+len(block))
	lines = append(lines, windowed.Lines...)
	lines = append(lines, gap...)
	lines = append(lines, block...)
	return paneLayout{lines: lines, focus: windowed.Focus}
}

// layoutSideBySide mirrors layoutSideBySide.
func (p *PreviewPane) layoutSideBySide(width int, mode PreviewLayoutMode) paneLayout {
	adaptiveLeft := p.globalLeftWidth(width)
	columns := ColumnWidthsFor(width, adaptiveLeft)

	leftRendered := p.optionListView.Render(columns.LeftWidth)
	leftFocus := p.optionListView.FocusedItemRowRange(columns.LeftWidth)
	var left WindowLinesResult
	if p.rowAllowance == nil {
		left = WindowLinesResult{Lines: leftRendered, Focus: &leftFocus}
	} else {
		left = WindowLines(leftRendered, *p.rowAllowance, &leftFocus, p.theme)
	}
	rightLines := p.renderPaddedPreviewLines(columns.RightWidth, mode, p.rowAllowance)

	rows := maxInt(len(left.Lines), len(rightLines))
	gapStr := repeatSpace(columns.Gap)
	out := make([]string, 0, rows)
	for i := 0; i < rows; i++ {
		leftRaw := ""
		if i < len(left.Lines) {
			leftRaw = left.Lines[i]
		}
		rightRaw := ""
		if i < len(rightLines) {
			rightRaw = rightLines[i]
		}
		leftClamped := truncateToWidth(leftRaw, columns.LeftWidth, "", false)
		leftPad := repeatSpace(maxInt(0, columns.LeftWidth-visibleWidth(leftClamped)))
		out = append(out, truncateToWidth(leftClamped+leftPad+gapStr+rightRaw, width, "", false))
	}
	return paneLayout{lines: out, focus: left.Focus}
}

// FocusedItemRowRange mirrors focusedItemRowRange.
func (p *PreviewPane) FocusedItemRowRange(width int) [2]int {
	if p.question.MultiSelect {
		return p.optionListView.FocusedItemRowRange(width)
	}
	if !p.previewBlock.HasAnyPreview() {
		return p.optionListView.FocusedItemRowRange(width)
	}
	if p.props.InputMode {
		return p.optionListView.FocusedItemRowRange(width)
	}
	if focus := p.layout(width).focus; focus != nil {
		return *focus
	}
	return [2]int{0, 1}
}

// NaturalHeight mirrors naturalHeight.
func (p *PreviewPane) NaturalHeight(width int) int {
	if p.question.MultiSelect || !p.previewBlock.HasAnyPreview() || p.props.InputMode {
		return len(p.optionListView.Render(width))
	}
	if p.rowAllowance != nil {
		return len(p.layout(width).lines)
	}
	mode := DecideLayout(p.getTerminalWidth(), width)
	widths := BodyWidthsFor(width, mode, p.globalLeftWidth(width))
	optionsHeight := len(p.optionListView.Render(widths.OptionsWidth))
	previewHeight := p.previewBlock.BlockHeight(widths.PreviewWidth, p.props.SelectedIndex, mode, nil)
	if mode == LayoutSideBySide {
		return maxInt(optionsHeight, previewHeight)
	}
	return optionsHeight + StackedGapRows + previewHeight
}

// MaxNaturalHeight mirrors maxNaturalHeight.
func (p *PreviewPane) MaxNaturalHeight(width int) int {
	if p.question.MultiSelect || !p.previewBlock.HasAnyPreview() || p.props.InputMode {
		return len(p.optionListView.Render(width))
	}
	mode := DecideLayout(p.getTerminalWidth(), width)
	widths := BodyWidthsFor(width, mode, p.globalLeftWidth(width))
	optionsHeight := len(p.optionListView.Render(widths.OptionsWidth))
	maxPreviewBlock := 0
	for i := range p.question.Options {
		if h := p.previewBlock.BlockHeight(widths.PreviewWidth, i, mode, nil); h > maxPreviewBlock {
			maxPreviewBlock = h
		}
	}
	var max int
	if mode == LayoutSideBySide {
		max = maxInt(optionsHeight, maxPreviewBlock)
	} else {
		max = optionsHeight + StackedGapRows + maxPreviewBlock
	}
	// A fitted body never exceeds the allowance, whichever option is focused.
	if p.rowAllowance == nil {
		return max
	}
	return minInt(max, *p.rowAllowance)
}

// renderPaddedPreviewLines mirrors renderPaddedPreviewLines.
func (p *PreviewPane) renderPaddedPreviewLines(
	colWidth int,
	mode PreviewLayoutMode,
	maxRows *int,
) []string {
	inner := maxInt(1, colWidth-PreviewPaddingLeft)
	contentLines := p.previewBlock.RenderBlock(inner, p.props.SelectedIndex, mode, p.notesAffordance(), maxRows)
	first := ""
	if len(contentLines) > 0 {
		first = contentLines[0]
	}
	boxWidth := maxInt(1, visibleWidth(first))
	boxAlignedPad := maxInt(PreviewPaddingLeft, colWidth-boxWidth)
	out := make([]string, 0, len(contentLines))
	for _, line := range contentLines {
		if line == "" {
			out = append(out, "")
			continue
		}
		// A line wider than the box (a long-locale affordance) slides left to stay
		// fully visible; truncation engages only when the column runs out.
		pad := maxInt(PreviewPaddingLeft, minInt(boxAlignedPad, colWidth-visibleWidth(line)))
		out = append(out, strings.Repeat(" ", pad)+truncateToWidth(line, colWidth-pad, "", false))
	}
	return out
}
