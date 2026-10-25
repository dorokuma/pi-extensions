package rpivaskuserquestion

// Ports state/key-router.ts (v2.15.0): the pure keystroke dispatcher.
//
// The cascade order is part of the contract:
//
//	toggle_collapsed (top intercept, from every inner state)
//	→ collapsed lockout
//	→ notes editor
//	→ input mode
//	→ Submit tab
//	→ tab switch
//	→ notes key
//	→ up/down navigation
//	→ multi/single-select tab
//
// One port note: the TS router calls pi-tui's `matchesKey(data, spec)`, whose
// parser lowercases and understands the KeyId grammar. Here that is
// [MatchesKey], implemented over the same grammar so the router stays pure and
// the unit tests can drive raw terminal chunks.

// Keybinding semantic names (pi-tui keybinding ids).
const (
	keybindUp             = "tui.select.up"
	keybindDown           = "tui.select.down"
	keybindConfirm        = "tui.select.confirm"
	keybindSubmit         = "tui.input.submit"
	keybindCancel         = "tui.select.cancel"
	keybindNewLine        = "tui.input.newLine"
	keybindEditorUp       = "tui.editor.cursorUp"
	keybindEditorDown     = "tui.editor.cursorDown"
	keybindClear          = "tui.editor.deleteToLineStart"
	keybindExternalEditor = "app.editor.external"
)

// notesActivateKey is the fallback notes key when the runtime field is not a
// string (a stale outer module); the default is `n`.
const notesActivateKey = "n"

// QuestionnaireAction is the discriminated union of every reducer action.
type QuestionnaireAction struct {
	Kind string

	// nav
	NextIndex  int
	InputValue string
	// input_edit / input_replace
	Value string
	// tab_switch
	NextTab int
	// confirm
	Answer         *QuestionAnswer
	AutoAdvanceTab *int
	// toggle
	Index int
	// multi_confirm
	Selected []string
	// submit_nav
	SubmitNextIndex int
	// notes_forward
	Data string
}

// Action kinds, matching the TS union members.
const (
	ActionNav             = "nav"
	ActionInputClear      = "input_clear"
	ActionInputEdit       = "input_edit"
	ActionInputReplace    = "input_replace"
	ActionTabSwitch       = "tab_switch"
	ActionConfirm         = "confirm"
	ActionToggle          = "toggle"
	ActionMultiConfirm    = "multi_confirm"
	ActionCancel          = "cancel"
	ActionNotesEnter      = "notes_enter"
	ActionNotesExit       = "notes_exit"
	ActionSubmit          = "submit"
	ActionSubmitNav       = "submit_nav"
	ActionNotesForward    = "notes_forward"
	ActionToggleCollapsed = "toggle_collapsed"
	ActionIgnore          = "ignore"
)

// WrapTab mirrors wrapTab: a positive modulo over total, 0 for an empty list.
func WrapTab(index, total int) int {
	if total <= 0 {
		return 0
	}
	mod := index % total
	if mod < 0 {
		mod += total
	}
	return mod
}

// AllAnswered reports whether every question has an answer.
func AllAnswered(state QuestionnaireState, runtime QuestionnaireRuntime) bool {
	if len(runtime.Questions) == 0 {
		return false
	}
	for i := range runtime.Questions {
		if _, ok := state.Answers[i]; !ok {
			return false
		}
	}
	return true
}

// totalTabs mirrors totalTabs: one tab for a single question, one per question
// plus the Submit tab in multi-question mode.
func totalTabs(runtime QuestionnaireRuntime) int {
	if runtime.IsMulti {
		return len(runtime.Questions) + 1
	}
	return 1
}

// computeAutoAdvanceTab mirrors computeAutoAdvanceTab: advance to the next
// question, or to the Submit tab after the last one.
func computeAutoAdvanceTab(state QuestionnaireState, runtime QuestionnaireRuntime) *int {
	if !runtime.IsMulti {
		return nil
	}
	next := len(runtime.Questions)
	if state.CurrentTab < len(runtime.Questions)-1 {
		next = state.CurrentTab + 1
	}
	return &next
}

// buildSingleSelectAnswer mirrors buildSingleSelectAnswer. A blank draft is not
// an answer (#301): Enter stays on the row instead of recording an empty custom
// answer. The sentinel rows produce no answer.
func buildSingleSelectAnswer(state QuestionnaireState, runtime QuestionnaireRuntime) *QuestionAnswer {
	if state.CurrentTab < 0 || state.CurrentTab >= len(runtime.Questions) {
		return nil
	}
	question := runtime.Questions[state.CurrentTab]

	if state.InputMode {
		if len(trimSpace(runtime.InputBuffer)) == 0 {
			return nil
		}
		return &QuestionAnswer{
			QuestionIndex: state.CurrentTab,
			Question:      question.Question,
			Kind:          AnswerKindCustom,
			Answer:        strPtr(runtime.InputBuffer),
		}
	}
	item := runtime.CurrentItem
	if item == nil {
		return nil
	}
	if item.Kind == RowKindOther || item.Kind == RowKindNext {
		return nil
	}
	return &QuestionAnswer{
		QuestionIndex: state.CurrentTab,
		Question:      question.Question,
		Kind:          AnswerKindOption,
		Answer:        strPtr(item.Label),
	}
}

// buildMultiSelected mirrors buildMultiSelected: the checked labels in authored
// order.
func buildMultiSelected(state QuestionnaireState, runtime QuestionnaireRuntime) []string {
	if state.CurrentTab < 0 || state.CurrentTab >= len(runtime.Questions) {
		return nil
	}
	question := runtime.Questions[state.CurrentTab]
	out := make([]string, 0, len(question.Options))
	for i := range question.Options {
		if _, checked := state.MultiSelectChecked[i]; checked {
			out = append(out, question.Options[i].Label)
		}
	}
	return out
}

// buildMultiConfirmAction mirrors buildMultiConfirmAction.
func buildMultiConfirmAction(state QuestionnaireState, runtime QuestionnaireRuntime) QuestionnaireAction {
	action := QuestionnaireAction{
		Kind:           ActionMultiConfirm,
		Selected:       buildMultiSelected(state, runtime),
		AutoAdvanceTab: computeAutoAdvanceTab(state, runtime),
	}
	if len(trimSpace(runtime.InputBuffer)) > 0 {
		action.Value = runtime.InputBuffer
	}
	return action
}

// tabSwitchAction mirrors tabSwitchAction: Tab/Right forward, Shift+Tab/Left
// back, wrapping over the tab count; nil for a single-question questionnaire.
func tabSwitchAction(data string, state QuestionnaireState, runtime QuestionnaireRuntime) *QuestionnaireAction {
	if !runtime.IsMulti {
		return nil
	}
	total := totalTabs(runtime)
	if MatchesKey(data, "tab") || MatchesKey(data, "right") {
		return &QuestionnaireAction{Kind: ActionTabSwitch, NextTab: WrapTab(state.CurrentTab+1, total)}
	}
	if MatchesKey(data, "shift+tab") || MatchesKey(data, "left") {
		return &QuestionnaireAction{Kind: ActionTabSwitch, NextTab: WrapTab(state.CurrentTab-1, total)}
	}
	return nil
}

// nextNavOnDown mirrors nextNavOnDown: DOWN at the last item wraps to the first.
func nextNavOnDown(state QuestionnaireState, runtime QuestionnaireRuntime) QuestionnaireAction {
	return QuestionnaireAction{
		Kind:       ActionNav,
		NextIndex:  WrapTab(state.OptionIndex+1, maxInt(1, len(runtime.Items))),
		InputValue: runtime.InputBuffer,
	}
}

// prevNavOnUp mirrors prevNavOnUp: UP at the first item wraps to the last.
func prevNavOnUp(state QuestionnaireState, runtime QuestionnaireRuntime) QuestionnaireAction {
	return QuestionnaireAction{
		Kind:       ActionNav,
		NextIndex:  WrapTab(state.OptionIndex-1, maxInt(1, len(runtime.Items))),
		InputValue: runtime.InputBuffer,
	}
}

// routeCollapsed mirrors routeCollapsed: while collapsed, swallow every
// keystroke except cancel so reading the transcript cannot mutate anything.
func routeCollapsed(kb Keybindings, data string) QuestionnaireAction {
	if kb.Matches(data, keybindCancel) {
		return QuestionnaireAction{Kind: ActionCancel}
	}
	return QuestionnaireAction{Kind: ActionIgnore}
}

// routeNotesMode mirrors routeNotesMode.
func routeNotesMode(kb Keybindings, data string) QuestionnaireAction {
	if kb.Matches(data, keybindCancel) {
		return QuestionnaireAction{Kind: ActionNotesExit}
	}
	if kb.Matches(data, keybindNewLine) {
		return QuestionnaireAction{Kind: ActionNotesForward, Data: data}
	}
	if isConfirm(kb, data) {
		return QuestionnaireAction{Kind: ActionNotesExit}
	}
	return QuestionnaireAction{Kind: ActionNotesForward, Data: data}
}

// routeInputMode mirrors routeInputMode.
func routeInputMode(
	kb Keybindings,
	data string,
	state QuestionnaireState,
	runtime QuestionnaireRuntime,
) QuestionnaireAction {
	// Newline takes precedence over confirmation if a user configuration binds
	// the same physical key to both semantic actions.
	if kb.Matches(data, keybindNewLine) {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	if isConfirm(kb, data) {
		if state.CurrentTab >= 0 && state.CurrentTab < len(runtime.Questions) &&
			runtime.Questions[state.CurrentTab].MultiSelect {
			return buildMultiConfirmAction(state, runtime)
		}
		answer := buildSingleSelectAnswer(state, runtime)
		if answer == nil {
			return QuestionnaireAction{Kind: ActionIgnore}
		}
		return QuestionnaireAction{
			Kind:           ActionConfirm,
			Answer:         answer,
			AutoAdvanceTab: computeAutoAdvanceTab(state, runtime),
		}
	}
	// Pi's Ctrl+U line-kill binding is an explicit whole-draft clear,
	// independent of the cursor position.
	if kb.Matches(data, keybindClear) {
		return QuestionnaireAction{Kind: ActionInputClear}
	}
	if kb.Matches(data, keybindExternalEditor) {
		return QuestionnaireAction{Kind: ActionInputEdit, Value: runtime.InputBuffer}
	}
	if kb.Matches(data, keybindCancel) {
		return QuestionnaireAction{Kind: ActionCancel}
	}
	if kb.Matches(data, keybindEditorUp) && runtime.CanMoveInputUp {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	if kb.Matches(data, keybindEditorDown) && runtime.CanMoveInputDown {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	if kb.Matches(data, keybindUp) {
		return prevNavOnUp(state, runtime)
	}
	if kb.Matches(data, keybindDown) {
		return nextNavOnDown(state, runtime)
	}
	return QuestionnaireAction{Kind: ActionIgnore}
}

// routeSubmitTab mirrors routeSubmitTab. Submit always submits and Cancel
// always cancels; the warning header is informational only.
func routeSubmitTab(
	kb Keybindings,
	data string,
	state QuestionnaireState,
	runtime QuestionnaireRuntime,
) QuestionnaireAction {
	if kb.Matches(data, keybindCancel) {
		return QuestionnaireAction{Kind: ActionCancel}
	}
	if tab := tabSwitchAction(data, state, runtime); tab != nil {
		return *tab
	}
	if kb.Matches(data, keybindUp) || kb.Matches(data, keybindDown) {
		delta := -1
		if kb.Matches(data, keybindDown) {
			delta = 1
		}
		next := WrapTab(state.SubmitChoiceIndex+delta, 2)
		return QuestionnaireAction{Kind: ActionSubmitNav, SubmitNextIndex: next}
	}
	if isConfirm(kb, data) {
		if state.SubmitChoiceIndex == 1 {
			return QuestionnaireAction{Kind: ActionCancel}
		}
		return QuestionnaireAction{Kind: ActionSubmit}
	}
	if matchesNotesKey(data, runtime.NotesKey) {
		return QuestionnaireAction{Kind: ActionNotesEnter}
	}
	return QuestionnaireAction{Kind: ActionIgnore}
}

// digitKeys are the 1-9 hotkeys, in row order.
var digitKeys = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}

// digitRow mirrors digitRow: the row a digit hotkey names. `1`-`9` follow the
// list's numbering, so the unnumbered Next row has no digit. One key per
// keystroke — a pasted "12" matches none.
func digitRow(data string, runtime QuestionnaireRuntime) (int, bool, bool) {
	index := -1
	for i, digit := range digitKeys {
		if MatchesKey(data, digit) {
			index = i
			break
		}
	}
	if index < 0 || index >= len(runtime.Items) {
		return 0, false, false
	}
	meta := RowIntentMetaFor(runtime.Items[index].Kind)
	if !meta.Numbered {
		return 0, false, false
	}
	return index, true, meta.ActivatesInputMode
}

// routeMultiSelectTab mirrors routeMultiSelectTab.
func routeMultiSelectTab(
	kb Keybindings,
	data string,
	state QuestionnaireState,
	runtime QuestionnaireRuntime,
) QuestionnaireAction {
	var focusedMeta RowIntentMeta
	if runtime.CurrentItem != nil {
		focusedMeta = RowIntentMetaFor(runtime.CurrentItem.Kind)
	}
	// Space toggles the focused row's checkbox, suppressed on rows whose META
	// declares BlocksMultiToggle (Next) or ActivatesInputMode (the inline input).
	if MatchesKey(data, "space") {
		if focusedMeta.BlocksMultiToggle || focusedMeta.ActivatesInputMode {
			return QuestionnaireAction{Kind: ActionIgnore}
		}
		return QuestionnaireAction{Kind: ActionToggle, Index: state.OptionIndex}
	}
	if isConfirm(kb, data) {
		if focusedMeta.ActivatesInputMode {
			return QuestionnaireAction{Kind: ActionIgnore}
		}
		// Enter on a regular row toggles (matching Space); committing is gated
		// behind a row whose META declares AutoSubmitsInMulti (the Next sentinel).
		if !focusedMeta.AutoSubmitsInMulti {
			return QuestionnaireAction{Kind: ActionToggle, Index: state.OptionIndex}
		}
		return buildMultiConfirmAction(state, runtime)
	}
	if kb.Matches(data, keybindCancel) {
		return QuestionnaireAction{Kind: ActionCancel}
	}
	// A digit toggles its row; the Type something. row's digit focuses it instead.
	index, found, activatesInputMode := digitRow(data, runtime)
	if !found {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	if activatesInputMode {
		return QuestionnaireAction{Kind: ActionNav, NextIndex: index, InputValue: runtime.InputBuffer}
	}
	return QuestionnaireAction{Kind: ActionToggle, Index: index}
}

// routeSingleSelectTab mirrors routeSingleSelectTab.
func routeSingleSelectTab(
	kb Keybindings,
	data string,
	state QuestionnaireState,
	runtime QuestionnaireRuntime,
) QuestionnaireAction {
	if isConfirm(kb, data) {
		answer := buildSingleSelectAnswer(state, runtime)
		if answer == nil {
			return QuestionnaireAction{Kind: ActionIgnore}
		}
		return QuestionnaireAction{
			Kind:           ActionConfirm,
			Answer:         answer,
			AutoAdvanceTab: computeAutoAdvanceTab(state, runtime),
		}
	}
	if kb.Matches(data, keybindCancel) {
		return QuestionnaireAction{Kind: ActionCancel}
	}
	// A digit answers with its row, as Enter on it would; the Type something.
	// row's digit focuses it instead.
	index, found, activatesInputMode := digitRow(data, runtime)
	if !found {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	if activatesInputMode {
		return QuestionnaireAction{Kind: ActionNav, NextIndex: index, InputValue: runtime.InputBuffer}
	}
	runtimeAtRow := runtime
	runtimeAtRow.CurrentItem = &runtime.Items[index]
	stateAtRow := state
	stateAtRow.OptionIndex = index
	answer := buildSingleSelectAnswer(stateAtRow, runtimeAtRow)
	if answer == nil {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	return QuestionnaireAction{
		Kind:           ActionConfirm,
		Answer:         answer,
		AutoAdvanceTab: computeAutoAdvanceTab(state, runtime),
	}
}

// RouteKey mirrors routeKey: dispatch one raw terminal chunk to a
// QuestionnaireAction.
func RouteKey(data string, state QuestionnaireState, runtime QuestionnaireRuntime) QuestionnaireAction {
	kb := runtime.Keybindings

	// The collapse/expand toggle is a UI-level affordance, intercepted at the top
	// so it works from every inner state without reaching any branch that would
	// consume the keystroke. A missing/non-string key counts as disabled, and the
	// "off" literal disables it explicitly.
	if runtime.CollapseKey != "" && runtime.CollapseKey != CollapseKeyOff &&
		MatchesKey(data, runtime.CollapseKey) {
		return QuestionnaireAction{Kind: ActionToggleCollapsed}
	}

	if state.Collapsed {
		return routeCollapsed(kb, data)
	}
	if state.NotesVisible {
		return routeNotesMode(kb, data)
	}
	if state.InputMode {
		return routeInputMode(kb, data, state, runtime)
	}
	if runtime.IsMulti && state.CurrentTab == len(runtime.Questions) {
		return routeSubmitTab(kb, data, state, runtime)
	}

	if tab := tabSwitchAction(data, state, runtime); tab != nil {
		return *tab
	}

	if state.CurrentTab < 0 || state.CurrentTab >= len(runtime.Questions) {
		return QuestionnaireAction{Kind: ActionIgnore}
	}
	question := runtime.Questions[state.CurrentTab]

	// Universal notes activation: the editor opens on every question tab.
	// The blocks above already swallow the key when it should NOT reach here.
	if matchesNotesKey(data, runtime.NotesKey) {
		return QuestionnaireAction{Kind: ActionNotesEnter}
	}

	if kb.Matches(data, keybindUp) {
		return prevNavOnUp(state, runtime)
	}
	if kb.Matches(data, keybindDown) {
		return nextNavOnDown(state, runtime)
	}

	if question.MultiSelect {
		return routeMultiSelectTab(kb, data, state, runtime)
	}
	return routeSingleSelectTab(kb, data, state, runtime)
}

// isConfirm mirrors isConfirm: confirm has two semantic sources — the
// select-list default and the user's submit key.
func isConfirm(kb Keybindings, data string) bool {
	return kb.Matches(data, keybindConfirm) || kb.Matches(data, keybindSubmit)
}

// matchesNotesKey mirrors matchesNotesKey: a single character matches as typed
// (so a non-Latin key works), anything else matches as a key spec.
func matchesNotesKey(data, notesKey string) bool {
	key := notesKey
	if key == "" {
		key = notesActivateKey
	}
	if data == key {
		return true
	}
	return MatchesKey(data, key)
}

// maxInt and trimSpace keep the router free of tiny helper imports.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
