package rpivaskuserquestion

// Ports view/components/option-list-view.ts, view/components/tab-bar.ts and
// view/components/submit-picker.ts (v2.15.0): the three props-driven chrome
// components.

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// MaxVisibleOptions is the cap on visible option rows in the select window.
const MaxVisibleOptions = 10

// OptionListViewProps is one tick's option-list projection.
type OptionListViewProps struct {
	SelectedIndex     int
	Focused           bool
	InputBuffer       string
	InputCursorOffset *int
	// Confirmed is the optional previously-confirmed marker.
	Confirmed *ConfirmedIndicator
}

// OptionListView is the sole owner of the option list's interactive state: it
// wraps a single WrappingSelect and setProps is its only mutator.
type OptionListView struct {
	selectView *WrappingSelect
}

// NewOptionListView constructs the list.
func NewOptionListView(items []WrappingSelectItem, theme WrappingSelectTheme) *OptionListView {
	total := len(items)
	return &OptionListView{
		selectView: NewWrappingSelect(items, minInt(len(items), MaxVisibleOptions), theme, WrappingSelectOptions{
			NumberStartOffset:      0,
			TotalItemsForNumbering: &total,
		}),
	}
}

// SetProps applies one projection.
func (v *OptionListView) SetProps(props OptionListViewProps) {
	v.selectView.SetSelectedIndex(props.SelectedIndex)
	v.selectView.SetFocused(props.Focused)
	if props.Confirmed != nil {
		index := props.Confirmed.Index
		v.selectView.SetConfirmedIndex(&index, props.Confirmed.LabelOverride)
	} else {
		v.selectView.SetConfirmedIndex(nil, nil)
	}
	v.selectView.SetInputBuffer(props.InputBuffer)
	v.selectView.SetInputCursorOffset(props.InputCursorOffset)
}

// HandleInput is empty — input is routed at the container level.
func (v *OptionListView) HandleInput(string) {}

// Invalidate forwards to the wrapped select.
func (v *OptionListView) Invalidate() { v.selectView.Invalidate() }

// Render mirrors render().
func (v *OptionListView) Render(width int) []string { return v.selectView.Render(width) }

// FocusedItemRowRange mirrors focusedItemRowRange.
func (v *OptionListView) FocusedItemRowRange(width int) [2]int {
	return v.selectView.FocusedItemRowRange(width)
}

// TabBarProps is one tick's tab-bar projection. Every render-time derivation is
// hoisted here so Render is pure styling.
type TabBarProps struct {
	Tabs   []TabBarTab
	Submit TabBarSubmit
}

// TabBarTab is one question tab.
type TabBarTab struct {
	Label    string
	Answered bool
	Active   bool
}

// TabBarSubmit is the Submit tab's state.
type TabBarSubmit struct {
	Active      bool
	AllAnswered bool
}

// TabBar renders the multi-question tab strip.
type TabBar struct {
	theme sdk.UITheme
	props TabBarProps
}

// NewTabBar constructs the bar.
func NewTabBar(theme sdk.UITheme) *TabBar {
	return &TabBar{theme: theme}
}

// SetProps replaces the projection.
func (b *TabBar) SetProps(props TabBarProps) { b.props = props }

// HandleInput is empty (chrome only).
func (b *TabBar) HandleInput(string) {}

// Invalidate is a no-op (no cache).
func (b *TabBar) Invalidate() {}

// Render mirrors render(): [tabLine, ""].
func (b *TabBar) Render(width int) []string {
	pieces := " ← "
	for _, tab := range b.props.Tabs {
		box := "□"
		if tab.Answered {
			box = "■"
		}
		rawSeg := " " + box + " " + tab.Label + " "
		styled := b.theme.Fg("muted", rawSeg)
		if tab.Answered {
			styled = b.theme.Fg("success", rawSeg)
		}
		if tab.Active {
			styled = b.theme.Bg("selectedBg", b.theme.Fg("text", rawSeg))
		}
		pieces += styled + " "
	}
	submitText := " ✓ Submit "
	submitStyled := b.theme.Fg("dim", submitText)
	if b.props.Submit.AllAnswered {
		submitStyled = b.theme.Fg("success", submitText)
	}
	if b.props.Submit.Active {
		submitStyled = b.theme.Bg("selectedBg", b.theme.Fg("text", submitText))
	}
	pieces += submitStyled + " →"
	return []string{truncateToWidth(pieces, width, "", false), ""}
}

// Submit-picker copy.
const (
	SubmitLabel = "Submit answers"
	CancelLabel = "Cancel"
)

// SubmitPickerProps is the fixed 2-row picker's projection.
type SubmitPickerProps struct {
	Rows []SubmitPickerRow
}

// SubmitPickerRow is one picker row's active flag.
type SubmitPickerRow struct {
	Active bool
}

// SubmitPicker renders the Submit tab's 2-row picker.
type SubmitPicker struct {
	theme sdk.UITheme
	props SubmitPickerProps
}

// NewSubmitPicker constructs the picker.
func NewSubmitPicker(theme sdk.UITheme) *SubmitPicker {
	return &SubmitPicker{
		theme: theme,
		props: SubmitPickerProps{Rows: []SubmitPickerRow{{}, {}}},
	}
}

// SetProps replaces the projection.
func (p *SubmitPicker) SetProps(props SubmitPickerProps) { p.props = props }

// HandleInput is empty (chrome only).
func (p *SubmitPicker) HandleInput(string) {}

// Invalidate is a no-op (no cache).
func (p *SubmitPicker) Invalidate() {}

// NaturalHeight mirrors naturalHeight: constant 2, state-independent.
func (p *SubmitPicker) NaturalHeight(int) int { return 2 }

// Render mirrors render(): always two rows, so the chrome row math holds.
func (p *SubmitPicker) Render(width int) []string {
	lines := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		text := T("submit.label", SubmitLabel)
		if i == 1 {
			text = T("submit.cancel", CancelLabel)
		}
		active := i < len(p.props.Rows) && p.props.Rows[i].Active
		pointer := inactivePointer
		if active {
			pointer = activePointer
		}
		label := p.theme.Fg("text", text)
		if active {
			label = p.theme.Fg("accent", p.theme.Bold(text))
		}
		lines = append(lines, truncateToWidth(pointer+itoa(i+1)+numberSeparator+label, width, "", false))
	}
	return lines
}
