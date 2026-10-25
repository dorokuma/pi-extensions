package rpivaskuserquestion

// Ports state/selectors/projections.ts (v2.15.0): the per- component
// projections the adapter pushes each tick. Every render-time derivation is
// hoisted here so the components' Render methods stay pure styling.

// IsActiveTab mirrors the `isActiveTab` selector: the tab whose pane the layout
// is currently showing.
func IsActiveTab(state QuestionnaireState, ctx PerTabBindingContext) bool {
	return ctx.I == SelectActivePreviewPaneIndex(state.CurrentTab, ctx.TotalQuestions)
}

// emptyMultiSelectProps mirrors emptyMultiSelectProps.
func emptyMultiSelectProps(ctx PerTabBindingContext) MultiSelectViewProps {
	return MultiSelectViewProps{
		Rows: nil,
		Other: MultiSelectOtherRowProps{
			Active:            false,
			Checked:           false,
			InputMode:         false,
			InputBuffer:       ctx.InputBuffer,
			InputCursorOffset: ctx.InputCursorOffset,
		},
		NextActive: false,
		NextLabel:  DisplayLabel(SentinelNext),
	}
}

// nextLabelFor mirrors nextLabelFor: the last question's Next row reads
// "Submit".
func nextLabelFor(ctx PerTabBindingContext) string {
	if ctx.I == len(ctx.Questions)-1 {
		return MultiSubmitLabel
	}
	return DisplayLabel(SentinelNext)
}

// SelectMultiSelectProps mirrors selectMultiSelectProps.
func SelectMultiSelectProps(state QuestionnaireState, ctx PerTabBindingContext) MultiSelectViewProps {
	if ctx.I < 0 || ctx.I >= len(ctx.Questions) {
		return emptyMultiSelectProps(ctx)
	}
	question := ctx.Questions[ctx.I]
	focused := ctx.ActiveView == ActiveViewOptions

	rows := make([]MultiSelectRowProps, 0, len(question.Options))
	for i := range question.Options {
		_, checked := state.MultiSelectChecked[i]
		rows = append(rows, MultiSelectRowProps{
			Checked: checked,
			Active:  focused && i == state.OptionIndex,
		})
	}
	otherActive := focused && state.OptionIndex == len(question.Options)
	nextActive := focused && state.OptionIndex == len(question.Options)+1
	return MultiSelectViewProps{
		Rows: rows,
		Other: MultiSelectOtherRowProps{
			Active:            otherActive,
			Checked:           len(trimSpace(ctx.InputBuffer)) > 0,
			InputMode:         state.InputMode,
			InputBuffer:       ctx.InputBuffer,
			InputCursorOffset: ctx.InputCursorOffset,
		},
		NextActive: nextActive,
		NextLabel:  nextLabelFor(ctx),
	}
}

// SelectOptionListProps mirrors selectOptionListProps.
func SelectOptionListProps(state QuestionnaireState, ctx PerTabBindingContext) OptionListViewProps {
	var items []WrappingSelectItem
	if ctx.I >= 0 && ctx.I < len(ctx.ItemsByTab) {
		items = ctx.ItemsByTab[ctx.I]
	}
	focused := ctx.ActiveView == ActiveViewOptions
	confirmed := SelectConfirmedIndicator(ctx.Questions, state.CurrentTab, state.Answers, items)
	return OptionListViewProps{
		SelectedIndex:     state.OptionIndex,
		Focused:           focused,
		InputBuffer:       ctx.InputBuffer,
		InputCursorOffset: ctx.InputCursorOffset,
		Confirmed:         confirmed,
	}
}

// SelectSubmitPickerProps mirrors selectSubmitPickerProps.
func SelectSubmitPickerProps(state QuestionnaireState, ctx BindingContext) SubmitPickerProps {
	focused := ctx.ActiveView == ActiveViewSubmit
	return SubmitPickerProps{Rows: []SubmitPickerRow{
		{Active: focused && state.SubmitChoiceIndex == 0},
		{Active: focused && state.SubmitChoiceIndex == 1},
	}}
}

// SelectPreviewPaneProps mirrors selectPreviewPaneProps.
func SelectPreviewPaneProps(state QuestionnaireState, ctx PerTabBindingContext) PreviewPaneProps {
	return PreviewPaneProps{
		NotesVisible:  state.NotesVisible,
		HasNote:       SelectSavedNote(state.NotesByTab, state.Answers, ctx.I) != nil,
		SelectedIndex: state.OptionIndex,
		Focused:       ctx.ActiveView == ActiveViewOptions,
		InputMode:     state.InputMode,
	}
}

// SelectTabBarProps mirrors selectTabBarProps.
func SelectTabBarProps(state QuestionnaireState, ctx BindingContext) TabBarProps {
	tabs := make([]TabBarTab, 0, len(ctx.Questions))
	for i, question := range ctx.Questions {
		label := "Q" + itoa(i+1)
		if question.Header != "" {
			label = question.Header
		}
		_, answered := state.Answers[i]
		tabs = append(tabs, TabBarTab{Label: label, Answered: answered, Active: i == state.CurrentTab})
	}
	return TabBarProps{
		Tabs: tabs,
		Submit: TabBarSubmit{
			Active:      state.CurrentTab == len(ctx.Questions),
			AllAnswered: len(state.Answers) == len(ctx.Questions) && len(ctx.Questions) > 0,
		},
	}
}

// SelectDialogProps mirrors selectDialogProps.
func SelectDialogProps(state QuestionnaireState, ctx BindingContext) DialogProps {
	return DialogProps{State: state, ActivePreviewPane: ctx.ActivePreviewPane}
}
