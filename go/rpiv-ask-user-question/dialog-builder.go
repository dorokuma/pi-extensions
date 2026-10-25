package rpivaskuserquestion

// Ports view/dialog-builder.ts (v2.15.0): the dialog chrome and the 3-region
// scroll partition.

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// MinScrollRowsWithPinnedHeading is how many rows the scroll window must keep
// for the body before the heading stays pinned above it.
const MinScrollRowsWithPinnedHeading = 3

// DialogProps is one tick's dialog projection.
type DialogProps struct {
	State             QuestionnaireState
	ActivePreviewPane *PreviewPane
}

// DialogConfig is DialogView's construction-time config (frozen afterwards).
type DialogConfig struct {
	Theme        sdk.UITheme
	Questions    []QuestionData
	TabBar       *TabBar
	NotesInput   *TextEditor
	IsMulti      bool
	TabsByIndex  []TabComponents
	SubmitPicker *SubmitPicker
	// GetBodyHeight is the worst-case body height across all tabs/options.
	GetBodyHeight func(width int) int
	// GetCurrentBodyHeight is the active tab/option's body height.
	GetCurrentBodyHeight func(width int) int
	// GetTerminalRows is the row budget, read at render time.
	GetTerminalRows func() int
	CollapseKey     string
	NotesKey        string
}

// DialogView is the dialog chrome.
type DialogView struct {
	liveProps     DialogProps
	config        DialogConfig
	questionTab   *QuestionTabStrategy
	submitTab     *SubmitTabStrategy
	maxFooterRows int
}

// NewDialogView constructs the dialog.
func NewDialogView(config DialogConfig, initialProps DialogProps) *DialogView {
	// The strategy is built before the view exists, so the pane getter closes over
	// the pointer and reads it once the mount is live.
	var view *DialogView
	questionTab := NewQuestionTabStrategy(QuestionTabStrategyConfig{
		Theme:     config.Theme,
		Questions: config.Questions,
		// Mirrors dialog-builder.ts: the body pane is the one the adapter projected
		// for THIS tick (`liveProps.activePreviewPane`), never the pane the dialog was
		// constructed with — a pinned pane renders tab 0's options, preview and notes
		// affordance on every other tab. The fallback reads the SAME live props, so a
		// caller that only moved `liveProps.state.currentTab` (without projecting a
		// pane) still gets that tab's pane, not the construction-time one.
		GetPreviewPane: func() *PreviewPane {
			if view == nil {
				return fallbackPreviewPane(config, initialProps)
			}
			if view.liveProps.ActivePreviewPane != nil {
				return view.liveProps.ActivePreviewPane
			}
			return fallbackPreviewPane(config, view.liveProps)
		},
		TabsByIndex:          config.TabsByIndex,
		NotesInput:           config.NotesInput,
		IsMulti:              config.IsMulti,
		GetCurrentBodyHeight: config.GetCurrentBodyHeight,
		CollapseKey:          config.CollapseKey,
		NotesKey:             config.NotesKey,
	})
	view = &DialogView{
		liveProps:     initialProps,
		config:        config,
		questionTab:   questionTab,
		maxFooterRows: questionTab.FooterRowCount(),
	}
	if config.IsMulti {
		view.submitTab = NewSubmitTabStrategy(SubmitTabStrategyConfig{
			Theme:        config.Theme,
			Questions:    config.Questions,
			SubmitPicker: config.SubmitPicker,
			NotesInput:   config.NotesInput,
			NotesKey:     config.NotesKey,
		})
		if rows := view.submitTab.FooterRowCount(); rows > view.maxFooterRows {
			view.maxFooterRows = rows
		}
	}
	return view
}

// fallbackPreviewPane is the pane getter's safe fallback while no tick has projected
// one yet (construction, and tests that build a strategy without props): the tab the
// initial state points at, clamped into range.
func fallbackPreviewPane(config DialogConfig, initialProps DialogProps) *PreviewPane {
	if len(config.TabsByIndex) == 0 {
		return nil
	}
	index := SelectActivePreviewPaneIndex(initialProps.State.CurrentTab, len(config.Questions))
	if index < 0 || index >= len(config.TabsByIndex) {
		return config.TabsByIndex[0].Preview
	}
	return config.TabsByIndex[index].Preview
}

// SetProps replaces the live projection.
func (d *DialogView) SetProps(props DialogProps) { d.liveProps = props }

// HandleInput is empty — input is routed by the session.
func (d *DialogView) HandleInput(string) {}

// Invalidate is a no-op: invalidation is driven by the props adapter.
func (d *DialogView) Invalidate() {}

// renderStrategy picks the strategy for the current tab.
func (d *DialogView) renderStrategy(state QuestionnaireState) TabContentStrategy {
	onSubmit := d.config.IsMulti && state.CurrentTab == len(d.config.Questions)
	if onSubmit && d.submitTab != nil {
		return d.submitTab
	}
	return d.questionTab
}

// Render mirrors render(): the 3-region partition with scroll-to-focus.
func (d *DialogView) Render(width int) []string {
	state := d.liveProps.State
	strategy := d.renderStrategy(state)

	headingRows := strategy.HeadingRows(state)
	headingHeight := renderedHeight(headingRows, width)

	// Fixed region sizes: top border (1) + tab bar (2 when multi) + spacer (1);
	// bottom border (1) + footer rows.
	topFixed := 1 + 1
	if d.config.IsMulti && d.config.TabBar != nil {
		topFixed += 2
	}
	bottomFixed := 1 + strategy.FooterRowCount()
	termRows := d.config.GetTerminalRows()

	d.allotBodyRows(strategy, termRows-topFixed-headingHeight-bottomFixed-d.rowsAroundBody(strategy, width))

	natural := d.buildContainer(strategy, headingRows, width)

	// Residual spacer: equalize total height across tabs (only when no overflow).
	restingMidRows := 0
	if !state.NotesVisible {
		restingMidRows = maxInt(0, d.rowsAroundBody(strategy, width)-1)
	}
	spacerRows := d.config.GetBodyHeight(width) + d.maxFooterRows -
		strategy.BodyHeight(width, state) - strategy.FooterRowCount() - restingMidRows
	if spacerRows < 0 {
		spacerRows = 0
	}

	if len(natural) <= termRows {
		return renderFitsTerminal(natural, minInt(spacerRows, termRows-len(natural)))
	}

	// OVERFLOW — 3-region partition with scroll-to-focus. The heading joins the
	// fixed top whenever the body keeps a usable window, so the question being
	// answered never scrolls out of view.
	pinHeading := termRows-topFixed-headingHeight-bottomFixed >= MinScrollRowsWithPinnedHeading
	top := topFixed
	if pinHeading {
		top += headingHeight
	}
	middleRows := len(natural) - top - bottomFixed
	availableMiddle := maxInt(0, termRows-top-bottomFixed)
	if availableMiddle == 0 {
		return renderChromeOnly(natural, top, bottomFixed, termRows)
	}

	// Never slice past the middle region into the bottom-fixed rows, which are
	// appended below — that would draw the bottom border and hint twice (#260).
	windowRows := minInt(availableMiddle, middleRows)
	rowsBeforeBody := 0
	if !pinHeading {
		rowsBeforeBody = headingHeight
	}
	var focusedRange *[2]int
	if state.NotesVisible {
		focusedRange = d.notesFocusRange(natural[top:top+middleRows], strategy, width, windowRows)
	} else if rows := strategy.FocusedItemRowRange(width, state); rows != nil {
		offset := [2]int{rows[0] + rowsBeforeBody, rows[1] + rowsBeforeBody}
		focusedRange = &offset
	}
	scrollStart := ComputeScrollStart(focusedRange, 0, windowRows, middleRows)
	end := top + scrollStart + windowRows
	if end > len(natural) {
		end = len(natural)
	}
	scrollableMiddle := make([]string, 0, windowRows)
	scrollableMiddle = append(scrollableMiddle, natural[top+scrollStart:end]...)
	var focusedInWindow *[2]int
	if focusedRange != nil {
		focusedInWindow = &[2]int{focusedRange[0] - scrollStart, focusedRange[1] - scrollStart}
	}
	DecorateOverflow(scrollableMiddle, scrollStart > 0, scrollStart+windowRows < middleRows,
		d.config.Theme, focusedInWindow)

	result := make([]string, 0, top+len(scrollableMiddle)+bottomFixed)
	result = append(result, natural[:top]...)
	result = append(result, scrollableMiddle...)
	result = append(result, natural[len(natural)-bottomFixed:]...)
	if len(result) > termRows {
		return result[:termRows]
	}
	return result
}

// rowsAroundBody mirrors rowsAroundBody: the body's trailing spacer plus any mid
// rows (e.g. the notes editor).
func (d *DialogView) rowsAroundBody(strategy TabContentStrategy, width int) int {
	return 1 + renderedHeight(strategy.MidRows(d.liveProps.State), width)
}

// notesFocusRange mirrors notesFocusRange.
func (d *DialogView) notesFocusRange(
	middle []string,
	strategy TabContentStrategy,
	width, windowRows int,
) *[2]int {
	notesRows := maxInt(0, d.rowsAroundBody(strategy, width)-1)
	notesStart := len(middle) - notesRows
	if notesRows <= windowRows {
		return &[2]int{notesStart, len(middle)}
	}
	cursorRow := -1
	for i := notesStart; i < len(middle); i++ {
		if i >= 0 && containsSubstring(middle[i], cursorMarker) {
			cursorRow = i
			break
		}
	}
	if cursorRow == -1 {
		return &[2]int{notesStart, len(middle)}
	}
	return &[2]int{cursorRow, cursorRow + 1}
}

// allotBodyRows mirrors allotBodyRows: hand the active pane the rows its body may
// use this frame; clear every other tab's allowance.
func (d *DialogView) allotBodyRows(strategy TabContentStrategy, bodyRows int) {
	state := d.liveProps.State
	onSubmit := d.config.IsMulti && state.CurrentTab == len(d.config.Questions)
	var active *PreviewPane
	if !onSubmit && bodyRows >= MinScrollRowsWithPinnedHeading {
		active = d.liveProps.ActivePreviewPane
	}
	for i := range d.config.TabsByIndex {
		pane := d.config.TabsByIndex[i].Preview
		if pane == nil {
			continue
		}
		if pane == active {
			rows := bodyRows
			pane.SetRowAllowance(&rows)
			continue
		}
		pane.SetRowAllowance(nil)
	}
}

// buildContainer mirrors buildContainerFromStrategy: border, tab bar, spacer,
// heading, body, spacer, mid rows, border, footer.
func (d *DialogView) buildContainer(
	strategy TabContentStrategy,
	headingRows []Component,
	width int,
) []string {
	state := d.liveProps.State
	border := func() string { return d.config.Theme.Fg("accent", repeatWidth(width)) }

	out := make([]string, 0, 16)
	out = append(out, border())
	if d.config.IsMulti && d.config.TabBar != nil {
		out = append(out, d.config.TabBar.Render(width)...)
	}
	out = append(out, "")
	out = append(out, renderedLines(headingRows, width)...)
	out = append(out, strategy.BodyComponent(state).Render(width)...)
	out = append(out, "")
	out = append(out, renderedLines(strategy.MidRows(state), width)...)
	out = append(out, border())
	out = append(out, renderedLines(strategy.FooterRows(state), width)...)
	return out
}

// repeatWidth is the horizontal rule the DynamicBorder draws.
func repeatWidth(width int) string {
	if width <= 0 {
		return ""
	}
	rule := make([]rune, width)
	for i := range rule {
		rule[i] = '─'
	}
	return string(rule)
}

// renderedHeight sums the rendered rows of a component list.
func renderedHeight(components []Component, width int) int {
	total := 0
	for _, component := range components {
		total += len(component.Render(width))
	}
	return total
}

// renderedLines concatenates the rendered rows of a component list.
func renderedLines(components []Component, width int) []string {
	out := make([]string, 0, len(components))
	for _, component := range components {
		out = append(out, component.Render(width)...)
	}
	return out
}

// renderFitsTerminal mirrors renderFitsTerminal.
func renderFitsTerminal(natural []string, spacerRows int) []string {
	if spacerRows <= 0 {
		return natural
	}
	out := make([]string, 0, len(natural)+spacerRows)
	out = append(out, natural...)
	for i := 0; i < spacerRows; i++ {
		out = append(out, "")
	}
	return out
}

// renderChromeOnly mirrors renderChromeOnly.
func renderChromeOnly(natural []string, topFixed, bottomFixed, termRows int) []string {
	out := make([]string, 0, topFixed+bottomFixed)
	if topFixed <= len(natural) {
		out = append(out, natural[:topFixed]...)
	}
	start := len(natural) - bottomFixed
	if start >= topFixed && start <= len(natural) {
		out = append(out, natural[start:]...)
	}
	if len(out) > termRows {
		return out[:termRows]
	}
	return out
}

// containsSubstring is strings.Contains without the import at the call site.
func containsSubstring(text, substr string) bool { return indexOf(text, substr) >= 0 }
