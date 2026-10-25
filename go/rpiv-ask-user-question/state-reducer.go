package rpivaskuserquestion

// Ports state/state-reducer.ts (v2.15.0): the pure reducer.
//
// reduce(state, action, ctx) → (state, Effect[]). Handlers are pure, named and
// individually testable; the dispatch table has an entry per action kind, so a
// new action fails to compile here until a handler is registered.

import "strings"

// EffectKind names one declarative side-effect.
type EffectKind string

// The closed effect set.
const (
	EffectSetInputBuffer        EffectKind = "set_input_buffer"
	EffectClearInputBuffer      EffectKind = "clear_input_buffer"
	EffectOpenInputEditor       EffectKind = "open_input_editor"
	EffectSetNotesValue         EffectKind = "set_notes_value"
	EffectSetNotesFocused       EffectKind = "set_notes_focused"
	EffectForwardNotesKeystroke EffectKind = "forward_notes_keystroke"
	EffectSetOverlayHidden      EffectKind = "set_overlay_hidden"
	EffectDone                  EffectKind = "done"
)

// Effect is one declarative side-effect emitted by Reduce.
type Effect struct {
	Kind    EffectKind
	Value   string
	Focused bool
	Hidden  bool
	Data    string
	Result  *QuestionnaireResult
}

// ApplyContext is the session-lifetime context the reducer needs.
type ApplyContext struct {
	Questions  []QuestionData
	ItemsByTab [][]WrappingSelectItem
}

// ApplyResult is the reducer's return: the new state and the effects to run.
type ApplyResult struct {
	State   QuestionnaireState
	Effects []Effect
}

// orderedAnswers mirrors orderedAnswers: the answers in question order,
// skipping unanswered questions.
func orderedAnswers(state QuestionnaireState, questions []QuestionData) []QuestionAnswer {
	out := make([]QuestionAnswer, 0, len(state.Answers))
	for i := range questions {
		if answer, ok := state.Answers[i]; ok {
			out = append(out, answer)
		}
	}
	return out
}

// syncMultiSelectFromAnswers mirrors syncMultiSelectFromAnswers: seed the
// checkbox set from a previously saved multi-select answer.
func syncMultiSelectFromAnswers(
	answers map[int]QuestionAnswer,
	questions []QuestionData,
	tab int,
) map[int]struct{} {
	checked := map[int]struct{}{}
	if tab < 0 || tab >= len(questions) {
		return checked
	}
	question := questions[tab]
	if !question.MultiSelect {
		return checked
	}
	saved, ok := answers[tab]
	if !ok {
		return checked
	}
	for i := range question.Options {
		for _, label := range saved.Selected {
			if label == question.Options[i].Label {
				checked[i] = struct{}{}
				break
			}
		}
	}
	return checked
}

// selectedLabels mirrors selectedLabels: the checked labels in authored order.
func selectedLabels(state QuestionnaireState, question QuestionData) []string {
	selected := make([]string, 0, len(question.Options))
	for i := range question.Options {
		if _, checked := state.MultiSelectChecked[i]; checked {
			selected = append(selected, question.Options[i].Label)
		}
	}
	return selected
}

// persistMultiSelectAnswer mirrors persistMultiSelectAnswer: write the current
// checkbox set (plus any custom draft) into the answers map.
func persistMultiSelectAnswer(state QuestionnaireState, ctx ApplyContext) map[int]QuestionAnswer {
	tab := state.CurrentTab
	if tab < 0 || tab >= len(ctx.Questions) {
		return state.Answers
	}
	question := ctx.Questions[tab]
	if !question.MultiSelect {
		return state.Answers
	}
	selected := selectedLabels(state, question)
	custom := customDraftValueFor(state, tab)
	hasCustom := len(trimSpace(custom)) > 0
	out := copyAnswers(state.Answers)
	if len(selected) == 0 && !hasCustom {
		delete(out, tab)
		return out
	}
	answer := QuestionAnswer{
		QuestionIndex: tab,
		Question:      question.Question,
		Kind:          AnswerKindMulti,
		Selected:      selected,
	}
	if hasCustom {
		answer.Answer = strPtr(custom)
	}
	if pendingNotes, ok := state.NotesByTab[tab]; ok && len(pendingNotes) > 0 {
		answer.Notes = strPtr(pendingNotes)
	}
	out[tab] = answer
	return out
}

// notesValueFor mirrors notesValueFor: the in-flight side-band store is
// authoritative before an option is confirmed; answer.notes is its mirror.
func notesValueFor(state QuestionnaireState, tab int) string {
	if note, ok := state.NotesByTab[tab]; ok {
		return note
	}
	if answer, ok := state.Answers[tab]; ok && answer.Notes != nil {
		return *answer.Notes
	}
	return ""
}

// customDraftValueFor mirrors customDraftValueFor.
func customDraftValueFor(state QuestionnaireState, tab int) string {
	if draft, ok := state.CustomDraftsByTab[tab]; ok {
		return draft
	}
	answer, ok := state.Answers[tab]
	if !ok {
		return ""
	}
	if answer.Kind != AnswerKindCustom && answer.Kind != AnswerKindMulti {
		return ""
	}
	if answer.Answer != nil {
		return *answer.Answer
	}
	return ""
}

// setCustomDraft mirrors setCustomDraft.
func setCustomDraft(state QuestionnaireState, tab int, value string) map[int]string {
	drafts := copyStringMap(state.CustomDraftsByTab)
	drafts[tab] = value
	return drafts
}

// withoutCustomDraft mirrors withoutCustomDraft.
func withoutCustomDraft(state QuestionnaireState, tab int) map[int]string {
	if _, ok := state.CustomDraftsByTab[tab]; !ok {
		return state.CustomDraftsByTab
	}
	drafts := copyStringMap(state.CustomDraftsByTab)
	delete(drafts, tab)
	return drafts
}

// answeredRowIndex mirrors answeredRowIndex: the row focused when entering a
// tab — the chosen option, or Next for a multi-select answer, so a stray Enter
// re-confirms instead of replacing the answer with row 0.
func answeredRowIndex(answer *QuestionAnswer, items []WrappingSelectItem) int {
	index := -1
	if answer != nil {
		switch answer.Kind {
		case AnswerKindOption:
			if answer.Answer != nil {
				for i, item := range items {
					if item.Kind == RowKindOption && item.Label == *answer.Answer {
						index = i
						break
					}
				}
			}
		case AnswerKindMulti:
			for i, item := range items {
				if item.Kind == RowKindNext {
					index = i
					break
				}
			}
		}
	}
	if index < 0 {
		return 0
	}
	return index
}

// switchTabResult mirrors switchTabResult.
func switchTabResult(state QuestionnaireState, nextTab int, ctx ApplyContext) ApplyResult {
	notesValue := notesValueFor(state, nextTab)
	var savedAnswer *QuestionAnswer
	if answer, ok := state.Answers[nextTab]; ok {
		saved := answer
		savedAnswer = &saved
	}
	var items []WrappingSelectItem
	if nextTab >= 0 && nextTab < len(ctx.ItemsByTab) {
		items = ctx.ItemsByTab[nextTab]
	}
	transitioned := state.clone()
	transitioned.CurrentTab = nextTab
	transitioned.OptionIndex = answeredRowIndex(savedAnswer, items)
	transitioned.InputMode = false
	transitioned.NotesVisible = false
	transitioned.SubmitChoiceIndex = 0
	transitioned.MultiSelectChecked = syncMultiSelectFromAnswers(state.Answers, ctx.Questions, nextTab)
	transitioned.NotesDraft = notesValue
	return ApplyResult{
		State: transitioned,
		Effects: []Effect{
			{Kind: EffectSetNotesFocused, Focused: false},
			{Kind: EffectSetNotesValue, Value: notesValue},
			{Kind: EffectSetInputBuffer, Value: customDraftValueFor(state, nextTab)},
		},
	}
}

// doneFor mirrors doneFor: the global note lifts from the Submit-tab
// pseudo-index (`len(questions)` in NotesByTab), attached on submit and cancel
// alike.
func doneFor(state QuestionnaireState, ctx ApplyContext, cancelled bool) ApplyResult {
	result := QuestionnaireResult{
		Answers:   orderedAnswers(state, ctx.Questions),
		Cancelled: cancelled,
	}
	if globalNote, ok := state.NotesByTab[len(ctx.Questions)]; ok && len(globalNote) > 0 {
		result.GlobalNote = strPtr(globalNote)
	}
	return ApplyResult{State: state, Effects: []Effect{{Kind: EffectDone, Result: &result}}}
}

// handlerFunc is one action handler.
type handlerFunc func(state QuestionnaireState, action QuestionnaireAction, ctx ApplyContext) ApplyResult

// navHandler mirrors navHandler: move the row cursor, entering or leaving input
// mode, and persist the custom draft on the way out of input mode.
func navHandler(state QuestionnaireState, action QuestionnaireAction, ctx ApplyContext) ApplyResult {
	var items []WrappingSelectItem
	if state.CurrentTab >= 0 && state.CurrentTab < len(ctx.ItemsByTab) {
		items = ctx.ItemsByTab[state.CurrentTab]
	}
	inputMode := false
	if action.NextIndex >= 0 && action.NextIndex < len(items) {
		inputMode = RowIntentMetaFor(items[action.NextIndex].Kind).ActivatesInputMode
	}
	customDraftsByTab := state.CustomDraftsByTab
	if state.InputMode {
		customDraftsByTab = setCustomDraft(state, state.CurrentTab, action.InputValue)
	}
	next := state.clone()
	next.OptionIndex = action.NextIndex
	next.InputMode = inputMode
	next.CustomDraftsByTab = customDraftsByTab
	if state.InputMode && !inputMode && state.CurrentTab >= 0 && state.CurrentTab < len(ctx.Questions) &&
		ctx.Questions[state.CurrentTab].MultiSelect {
		// Mirror the reducer order: the answer is persisted against the state as
		// it stands after the draft was recorded.
		next.Answers = persistMultiSelectAnswer(next, ctx)
	}
	if !inputMode {
		return ApplyResult{State: next}
	}
	return ApplyResult{
		State:   next,
		Effects: []Effect{{Kind: EffectSetInputBuffer, Value: customDraftValueFor(next, state.CurrentTab)}},
	}
}

// inputClearHandler mirrors inputClearHandler.
func inputClearHandler(state QuestionnaireState, _ QuestionnaireAction, _ ApplyContext) ApplyResult {
	next := state.clone()
	next.CustomDraftsByTab = setCustomDraft(state, state.CurrentTab, "")
	return ApplyResult{State: next, Effects: []Effect{{Kind: EffectClearInputBuffer}}}
}

// inputEditHandler mirrors inputEditHandler.
func inputEditHandler(state QuestionnaireState, action QuestionnaireAction, _ ApplyContext) ApplyResult {
	return ApplyResult{State: state, Effects: []Effect{{Kind: EffectOpenInputEditor, Value: action.Value}}}
}

// inputReplaceHandler mirrors inputReplaceHandler.
func inputReplaceHandler(state QuestionnaireState, action QuestionnaireAction, _ ApplyContext) ApplyResult {
	next := state.clone()
	next.CustomDraftsByTab = setCustomDraft(state, state.CurrentTab, action.Value)
	return ApplyResult{State: next, Effects: []Effect{{Kind: EffectSetInputBuffer, Value: action.Value}}}
}

// confirmHandler mirrors confirmHandler: shape the answer (promote a custom
// answer to multi, attach a preview, merge the pending note), store it, and
// either advance the tab or finish.
func confirmHandler(state QuestionnaireState, action QuestionnaireAction, ctx ApplyContext) ApplyResult {
	if action.Answer == nil {
		return ApplyResult{State: state}
	}
	answer := *action.Answer
	var question *QuestionData
	if answer.QuestionIndex >= 0 && answer.QuestionIndex < len(ctx.Questions) {
		question = &ctx.Questions[answer.QuestionIndex]
	}
	if answer.Kind == AnswerKindCustom && question != nil && question.MultiSelect {
		answer.Kind = AnswerKindMulti
		answer.Selected = selectedLabels(state, *question)
	} else if answer.Kind == AnswerKindOption && answer.Answer != nil && question != nil {
		for _, option := range question.Options {
			if option.Label == *answer.Answer && option.hasPreview() {
				answer.Preview = strPtr(option.previewText())
				break
			}
		}
	}
	if pendingNotes, ok := state.NotesByTab[answer.QuestionIndex]; ok && len(pendingNotes) > 0 {
		answer.Notes = strPtr(pendingNotes)
	}

	answers := copyAnswers(state.Answers)
	answers[answer.QuestionIndex] = answer
	hasCustomAnswer := answer.Kind == AnswerKindCustom ||
		(answer.Kind == AnswerKindMulti && answer.Answer != nil)
	customDraftsByTab := state.CustomDraftsByTab
	if hasCustomAnswer {
		customDraftsByTab = withoutCustomDraft(state, answer.QuestionIndex)
	}
	next := state.clone()
	next.Answers = answers
	next.CustomDraftsByTab = customDraftsByTab
	if action.AutoAdvanceTab != nil {
		return switchTabResult(next, *action.AutoAdvanceTab, ctx)
	}
	return doneFor(next, ctx, false)
}

// toggleHandler mirrors toggleHandler.
func toggleHandler(state QuestionnaireState, action QuestionnaireAction, ctx ApplyContext) ApplyResult {
	checked := copySet(state.MultiSelectChecked)
	if _, has := checked[action.Index]; has {
		delete(checked, action.Index)
	} else {
		checked[action.Index] = struct{}{}
	}
	intermediate := state.clone()
	intermediate.MultiSelectChecked = checked
	intermediate.Answers = persistMultiSelectAnswer(intermediate, ctx)
	return ApplyResult{State: intermediate}
}

// multiConfirmHandler mirrors multiConfirmHandler.
func multiConfirmHandler(state QuestionnaireState, action QuestionnaireAction, ctx ApplyContext) ApplyResult {
	if state.CurrentTab < 0 || state.CurrentTab >= len(ctx.Questions) {
		return ApplyResult{State: state}
	}
	question := ctx.Questions[state.CurrentTab]
	answer := QuestionAnswer{
		QuestionIndex: state.CurrentTab,
		Question:      question.Question,
		Kind:          AnswerKindMulti,
		Selected:      action.Selected,
	}
	if action.Value != "" {
		answer.Answer = strPtr(action.Value)
	}
	if pendingNotes, ok := state.NotesByTab[state.CurrentTab]; ok && len(pendingNotes) > 0 {
		answer.Notes = strPtr(pendingNotes)
	}
	answers := copyAnswers(state.Answers)
	answers[state.CurrentTab] = answer
	synced := state.clone()
	synced.Answers = answers
	synced.CustomDraftsByTab = withoutCustomDraft(state, state.CurrentTab)
	synced.MultiSelectChecked = syncMultiSelectFromAnswers(answers, ctx.Questions, state.CurrentTab)
	if action.AutoAdvanceTab != nil {
		return switchTabResult(synced, *action.AutoAdvanceTab, ctx)
	}
	return doneFor(synced, ctx, false)
}

// notesEnterHandler mirrors notesEnterHandler.
func notesEnterHandler(state QuestionnaireState, _ QuestionnaireAction, _ ApplyContext) ApplyResult {
	value := notesValueFor(state, state.CurrentTab)
	next := state.clone()
	next.NotesVisible = true
	next.NotesDraft = value
	return ApplyResult{
		State: next,
		Effects: []Effect{
			{Kind: EffectSetNotesValue, Value: value},
			{Kind: EffectSetNotesFocused, Focused: true},
		},
	}
}

// notesExitHandler mirrors notesExitHandler: commit the trimmed draft to the
// side-band (and mirror it onto an existing answer), or clear it.
func notesExitHandler(state QuestionnaireState, _ QuestionnaireAction, _ ApplyContext) ApplyResult {
	trimmed := trimSpace(state.NotesDraft)
	notes := copyStringMap(state.NotesByTab)
	answers := copyAnswers(state.Answers)
	if len(trimmed) == 0 {
		delete(notes, state.CurrentTab)
		if prev, ok := answers[state.CurrentTab]; ok && prev.Notes != nil {
			prev.Notes = nil
			answers[state.CurrentTab] = prev
		}
	} else {
		notes[state.CurrentTab] = trimmed
		if prev, ok := answers[state.CurrentTab]; ok {
			prev.Notes = strPtr(trimmed)
			answers[state.CurrentTab] = prev
		}
	}
	next := state.clone()
	next.NotesByTab = notes
	next.Answers = answers
	next.NotesVisible = false
	return ApplyResult{State: next, Effects: []Effect{{Kind: EffectSetNotesFocused, Focused: false}}}
}

// cancelHandler mirrors cancelHandler.
func cancelHandler(state QuestionnaireState, _ QuestionnaireAction, ctx ApplyContext) ApplyResult {
	return doneFor(state, ctx, true)
}

// submitHandler mirrors submitHandler.
func submitHandler(state QuestionnaireState, _ QuestionnaireAction, ctx ApplyContext) ApplyResult {
	return doneFor(state, ctx, false)
}

// submitNavHandler mirrors submitNavHandler.
func submitNavHandler(state QuestionnaireState, action QuestionnaireAction, _ ApplyContext) ApplyResult {
	next := state.clone()
	next.SubmitChoiceIndex = action.SubmitNextIndex
	return ApplyResult{State: next}
}

// notesForwardHandler mirrors notesForwardHandler.
func notesForwardHandler(state QuestionnaireState, action QuestionnaireAction, _ ApplyContext) ApplyResult {
	return ApplyResult{State: state, Effects: []Effect{{Kind: EffectForwardNotesKeystroke, Data: action.Data}}}
}

// toggleCollapsedHandler mirrors toggleCollapsedHandler.
func toggleCollapsedHandler(state QuestionnaireState, _ QuestionnaireAction, _ ApplyContext) ApplyResult {
	next := state.clone()
	next.Collapsed = !state.Collapsed
	return ApplyResult{
		State:   next,
		Effects: []Effect{{Kind: EffectSetOverlayHidden, Hidden: next.Collapsed}},
	}
}

// ignoreHandler mirrors ignoreHandler.
func ignoreHandler(state QuestionnaireState, _ QuestionnaireAction, _ ApplyContext) ApplyResult {
	return ApplyResult{State: state}
}

// handlers is the dispatch table: one entry per action kind.
var handlers = map[string]handlerFunc{
	ActionNav:          navHandler,
	ActionInputClear:   inputClearHandler,
	ActionInputEdit:    inputEditHandler,
	ActionInputReplace: inputReplaceHandler,
	ActionTabSwitch: func(s QuestionnaireState, a QuestionnaireAction, c ApplyContext) ApplyResult {
		return switchTabResult(s, a.NextTab, c)
	},
	ActionConfirm:         confirmHandler,
	ActionToggle:          toggleHandler,
	ActionMultiConfirm:    multiConfirmHandler,
	ActionCancel:          cancelHandler,
	ActionNotesEnter:      notesEnterHandler,
	ActionNotesExit:       notesExitHandler,
	ActionNotesForward:    notesForwardHandler,
	ActionSubmit:          submitHandler,
	ActionSubmitNav:       submitNavHandler,
	ActionToggleCollapsed: toggleCollapsedHandler,
	ActionIgnore:          ignoreHandler,
}

// Reduce mirrors reduce: dispatch one action to its pure handler. An unknown
// kind is treated as `ignore` (the TS table is exhaustive by construction; the
// Go map is complete for every declared kind, and this is the defensive floor).
func Reduce(state QuestionnaireState, action QuestionnaireAction, ctx ApplyContext) ApplyResult {
	handler, ok := handlers[action.Kind]
	if !ok {
		return ignoreHandler(state, action, ctx)
	}
	return handler(state, action, ctx)
}

// trimSpace is strings.TrimSpace, named once so every trimming point in the
// port reads the same way as the TS `.trim()` it mirrors.
func trimSpace(text string) string { return strings.TrimSpace(text) }
