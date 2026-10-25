package rpivaskuserquestion

// Ports view/components/wrapping-select.ts (v2.15.0): the numbered option list
// with the inline-input row and the confirmed-row mark.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	sdktui "github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// WrappingSelectItem is the row-intent discriminated union. `Kind` is the
// single discriminator.
//
//   - option: a regular author-defined option row.
//   - other:  the inline free-text input row appended to every question.
//   - next:   the explicit commit-and-advance row appended to multi-select
//     questions; renders without a number or checkbox.
type WrappingSelectItem struct {
	Kind        RowKind
	Label       string
	Description string
}

// WrappingSelectTheme is the per-select styling hooks. A nil hook renders the
// text unstyled.
type WrappingSelectTheme struct {
	SelectedText func(string) string
	Description  func(string) string
	ScrollInfo   func(string) string
}

// sdkTheme adapts the host theme to a WrappingSelectTheme, mirroring the
// makeSelectTheme the TUI builder uses.
func sdkTheme(theme sdk.UITheme) WrappingSelectTheme {
	return WrappingSelectTheme{
		SelectedText: func(text string) string { return theme.Fg("accent", theme.Bold(text)) },
		Description:  func(text string) string { return theme.Fg("muted", text) },
		ScrollInfo:   func(text string) string { return theme.Fg("dim", text) },
	}
}

// plainTheme renders everything unstyled — the default for headless tests.
var plainTheme = WrappingSelectTheme{
	SelectedText: func(text string) string { return text },
	Description:  func(text string) string { return text },
	ScrollInfo:   func(text string) string { return text },
}

// Row-prefix glyphs, verbatim from the TS component.
const (
	activePointer   = "❯ "
	inactivePointer = "  "
	numberSeparator = ". "
	confirmedMark   = " ✔"
	minContentWidth = 1
)

// WrappingSelectOptions are the numbering controls.
type WrappingSelectOptions struct {
	// NumberStartOffset starts numbering at this offset + 1.
	NumberStartOffset int
	// TotalItemsForNumbering overrides the total used to pad the number column.
	TotalItemsForNumbering *int
}

// WrappingSelect renders a numbered, wrapping option list.
type WrappingSelect struct {
	items                  []WrappingSelectItem
	maxVisible             int
	theme                  WrappingSelectTheme
	numberStartOffset      int
	totalItemsForNumbering int

	selectedIndex          int
	focused                bool
	inputBuffer            string
	inputCursorOffset      *int
	confirmedIndex         *int
	confirmedLabelOverride *string
}

// NewWrappingSelect constructs the list.
func NewWrappingSelect(
	items []WrappingSelectItem,
	maxVisible int,
	theme WrappingSelectTheme,
	options WrappingSelectOptions,
) *WrappingSelect {
	total := len(items)
	if options.TotalItemsForNumbering != nil {
		total = *options.TotalItemsForNumbering
	}
	return &WrappingSelect{
		items:                  items,
		maxVisible:             maxInt(1, maxVisible),
		theme:                  theme,
		numberStartOffset:      options.NumberStartOffset,
		totalItemsForNumbering: total,
		focused:                true,
	}
}

// SetNumbering realigns the number column without rebuilding the component.
func (w *WrappingSelect) SetNumbering(numberStartOffset, totalItemsForNumbering int) {
	w.numberStartOffset = numberStartOffset
	w.totalItemsForNumbering = maxInt(1, totalItemsForNumbering)
}

// SetSelectedIndex clamps and stores the cursor row.
func (w *WrappingSelect) SetSelectedIndex(index int) {
	w.selectedIndex = clampIndex(index, len(w.items))
}

// SetFocused records whether the list owns focus.
func (w *WrappingSelect) SetFocused(focused bool) { w.focused = focused }

// SetConfirmedIndex marks a previously-confirmed row; LabelOverride replaces the
// row's static label at render time (the `other` sentinel shows the prior
// custom text instead of "Type something.").
func (w *WrappingSelect) SetConfirmedIndex(index *int, labelOverride *string) {
	if index == nil {
		w.confirmedIndex = nil
		w.confirmedLabelOverride = nil
		return
	}
	clamped := clampIndex(*index, len(w.items))
	w.confirmedIndex = &clamped
	w.confirmedLabelOverride = labelOverride
}

// SetInputBuffer stores the live inline-input text.
func (w *WrappingSelect) SetInputBuffer(text string) { w.inputBuffer = text }

// SetInputCursorOffset sets the inline-input cursor; nil falls back to
// end-of-buffer.
func (w *WrappingSelect) SetInputCursorOffset(offset *int) { w.inputCursorOffset = offset }

// HandleInput is intentionally empty — input is routed at the container level.
func (w *WrappingSelect) HandleInput(string) {}

// Invalidate is a no-op: the component holds no derived cache.
func (w *WrappingSelect) Invalidate() {}

// Render mirrors render().
func (w *WrappingSelect) Render(width int) []string {
	if len(w.items) == 0 {
		return nil
	}
	start, end := w.visibleWindow()
	numberWidth := len(itoa(maxInt(1, w.totalItemsForNumbering)))
	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		isActive := i == w.selectedIndex && w.focused
		lines = append(lines, w.renderItem(w.items[i], i, isActive, width, numberWidth)...)
	}
	if start > 0 || end < len(w.items) {
		lines = append(lines, w.theme.ScrollInfo("  ("+itoa(w.selectedIndex+1)+"/"+itoa(len(w.items))+")"))
	}
	return lines
}

// FocusedItemRowRange mirrors focusedItemRowRange: the [startRow, endRow) range
// of the focused item within Render's output.
func (w *WrappingSelect) FocusedItemRowRange(width int) [2]int {
	if len(w.items) == 0 {
		return [2]int{0, 0}
	}
	start, end := w.visibleWindow()
	numberWidth := len(itoa(maxInt(1, w.totalItemsForNumbering)))
	row := 0
	for i := start; i < end; i++ {
		isActive := i == w.selectedIndex && w.focused
		rows := len(w.renderItem(w.items[i], i, isActive, width, numberWidth))
		if i == w.selectedIndex {
			return [2]int{row, row + rows}
		}
		row += rows
	}
	return [2]int{0, 1}
}

// visibleWindow mirrors computeVisibleWindow.
func (w *WrappingSelect) visibleWindow() (int, int) {
	half := w.maxVisible / 2
	start := maxInt(0, minInt(w.selectedIndex-half, len(w.items)-w.maxVisible))
	end := minInt(start+w.maxVisible, len(w.items))
	return start, end
}

// renderItem mirrors renderItem.
func (w *WrappingSelect) renderItem(
	item WrappingSelectItem,
	index int,
	isActive bool,
	width int,
	numberWidth int,
) []string {
	rowPrefix := w.buildRowPrefix(index, isActive, numberWidth)
	continuationPrefix := strings.Repeat(" ", sdktui.VisibleWidth(rowPrefix))
	contentWidth := maxInt(minContentWidth, width-sdktui.VisibleWidth(rowPrefix))

	if item.Kind == RowKindOther && isActive {
		return RenderInlineInputRow(RenderInlineInputOptions{
			Buffer:             w.inputBuffer,
			CursorOffset:       w.inputCursorOffset,
			RowPrefix:          rowPrefix,
			ContinuationPrefix: continuationPrefix,
			ContentWidth:       contentWidth,
			SelectedText:       w.theme.SelectedText,
		})
	}

	label, isConfirmed := w.deriveConfirmedState(item, index)
	applySelectedStyle := isActive || isConfirmed

	lines := w.renderLabelBlock(label, rowPrefix, continuationPrefix, contentWidth, applySelectedStyle)
	return append(lines, w.renderDescriptionBlock(item.Description, continuationPrefix, contentWidth)...)
}

// deriveConfirmedState mirrors deriveConfirmedState.
func (w *WrappingSelect) deriveConfirmedState(item WrappingSelectItem, index int) (string, bool) {
	customDraft := ""
	if item.Kind == RowKindOther {
		customDraft = w.inputBuffer
	}
	confirmedAt := w.confirmedIndex != nil && *w.confirmedIndex == index
	override := ""
	if w.confirmedLabelOverride != nil {
		override = *w.confirmedLabelOverride
	}
	customDraftDiffersFromConfirmed := item.Kind == RowKindOther &&
		customDraft != "" && confirmedAt && customDraft != override
	isConfirmed := confirmedAt && !customDraftDiffersFromConfirmed

	baseLabel := item.Label
	if customDraft != "" {
		baseLabel = customDraft
	}
	if !isConfirmed {
		return baseLabel, false
	}
	shown := baseLabel
	if w.confirmedLabelOverride != nil {
		shown = *w.confirmedLabelOverride
	}
	return shown + confirmedMark, true
}

// buildRowPrefix mirrors buildRowPrefix.
func (w *WrappingSelect) buildRowPrefix(index int, isActive bool, numberWidth int) string {
	pointer := inactivePointer
	if isActive {
		pointer = activePointer
	}
	displayNumber := w.numberStartOffset + index + 1
	return pointer + padStart(itoa(displayNumber), numberWidth) + numberSeparator
}

// renderLabelBlock mirrors renderLabelBlock.
func (w *WrappingSelect) renderLabelBlock(
	label, rowPrefix, continuationPrefix string,
	contentWidth int,
	applySelectedStyle bool,
) []string {
	wrapped := wrapTextWithAnsi(label, contentWidth)
	lines := make([]string, 0, len(wrapped))
	for i, segment := range wrapped {
		prefix := continuationPrefix
		if i == 0 {
			prefix = rowPrefix
		}
		line := prefix + segment
		if applySelectedStyle {
			line = w.theme.SelectedText(line)
		}
		lines = append(lines, line)
	}
	return lines
}

// renderDescriptionBlock mirrors renderDescriptionBlock.
func (w *WrappingSelect) renderDescriptionBlock(description, continuationPrefix string, contentWidth int) []string {
	if description == "" {
		return nil
	}
	wrapped := wrapTextWithAnsi(description, contentWidth)
	lines := make([]string, 0, len(wrapped))
	for _, segment := range wrapped {
		lines = append(lines, continuationPrefix+w.theme.Description(segment))
	}
	return lines
}

// clampIndex mirrors `Math.max(0, Math.min(index, length - 1))`.
func clampIndex(index, length int) int {
	if length <= 0 {
		return 0
	}
	return maxInt(0, minInt(index, length-1))
}

// minInt is the two-argument minimum.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// padStart mirrors String.prototype.padStart with spaces.
func padStart(text string, width int) string {
	if len(text) >= width {
		return text
	}
	return strings.Repeat(" ", width-len(text)) + text
}
