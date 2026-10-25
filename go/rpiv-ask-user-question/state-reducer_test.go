package rpivaskuserquestion

import (
	"reflect"
	"testing"
)

// ── fixtures ───────────────────────────────────────────────────────────────

// singleParams is a one-question single-select questionnaire.
func singleParams() QuestionParams {
	return QuestionParams{Questions: []QuestionData{twoOptions("Auth", "Which auth?")}}
}

// multiParams is a two-question questionnaire (the second multi-select).
func multiParams() QuestionParams {
	second := multiQuestion("Which features?", "A", "B", "C")
	second.Header = "Features"
	return QuestionParams{Questions: []QuestionData{
		twoOptions("Auth", "Which auth?"),
		second,
	}}
}

// applyContextFor builds the reducer context for params.
func applyContextFor(params QuestionParams) ApplyContext {
	return ApplyContext{Questions: params.Questions, ItemsByTab: BuildItemsByTab(params)}
}

// stateFor builds the initial state.
func stateFor() QuestionnaireState { return NewQuestionnaireState() }

// effectsOf flattens the effect kinds.
func effectsOf(result ApplyResult) []EffectKind {
	kinds := make([]EffectKind, 0, len(result.Effects))
	for _, effect := range result.Effects {
		kinds = append(kinds, effect.Kind)
	}
	return kinds
}

// doneResult returns the done effect's result, failing when absent.
func doneResult(t *testing.T, result ApplyResult) QuestionnaireResult {
	t.Helper()
	for _, effect := range result.Effects {
		if effect.Kind == EffectDone {
			if effect.Result == nil {
				t.Fatal("done effect without a result")
			}
			return *effect.Result
		}
	}
	t.Fatalf("no done effect; effects = %v", effectsOf(result))
	return QuestionnaireResult{}
}

// ── row intent ─────────────────────────────────────────────────────────────

func TestSentinelsToAppend(t *testing.T) {
	single := twoOptions("H", "Q?")
	if got := SentinelsToAppend(single); !reflect.DeepEqual(got, []SentinelKind{SentinelOther}) {
		t.Fatalf("single-select sentinels = %v", got)
	}
	multi := multiQuestion("Q?", "A", "B")
	if got := SentinelsToAppend(multi); !reflect.DeepEqual(got, []SentinelKind{SentinelOther, SentinelNext}) {
		t.Fatalf("multi-select sentinels = %v", got)
	}
}

func TestBuildItemsForQuestion(t *testing.T) {
	single := BuildItemsForQuestion(twoOptions("H", "Q?"))
	if len(single) != 3 {
		t.Fatalf("single items = %d", len(single))
	}
	if single[0].Kind != RowKindOption || single[0].Label != "A" || single[0].Description != "desc A" {
		t.Fatalf("option row = %#v", single[0])
	}
	if single[2].Kind != RowKindOther || single[2].Label != SentinelOtherLabel {
		t.Fatalf("other row = %#v", single[2])
	}

	multi := BuildItemsForQuestion(multiQuestion("Q?", "A", "B"))
	if len(multi) != 4 {
		t.Fatalf("multi items = %d", len(multi))
	}
	if multi[2].Kind != RowKindOther || multi[3].Kind != RowKindNext || multi[3].Label != SentinelNextLabel {
		t.Fatalf("multi sentinel rows = %#v / %#v", multi[2], multi[3])
	}
}

func TestRowIntentMeta(t *testing.T) {
	other := RowIntentMetaFor(RowKindOther)
	if !other.ActivatesInputMode || !other.Numbered || !other.Reserved || other.BlocksMultiToggle {
		t.Fatalf("other meta = %#v", other)
	}
	next := RowIntentMetaFor(RowKindNext)
	if next.ActivatesInputMode || next.Numbered || !next.BlocksMultiToggle || !next.AutoSubmitsInMulti {
		t.Fatalf("next meta = %#v", next)
	}
	option := RowIntentMetaFor(RowKindOption)
	if option.Label != "" || option.Reserved || !option.Numbered || option.ActivatesInputMode {
		t.Fatalf("option meta = %#v", option)
	}
}

// ── nav ───────────────────────────────────────────────────────────────────

func TestReduceNavMovesTheCursor(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()

	result := Reduce(state, QuestionnaireAction{Kind: ActionNav, NextIndex: 1, InputValue: ""}, ctx)
	if result.State.OptionIndex != 1 {
		t.Fatalf("optionIndex = %d", result.State.OptionIndex)
	}
	if result.State.InputMode {
		t.Fatal("inputMode set on an option row")
	}
}

// Focusing the Type-something. row enters input mode and seeds the buffer from
// the stored draft.
func TestReduceNavEntersInputModeWithDraft(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.CustomDraftsByTab = map[int]string{0: "draft"}

	result := Reduce(state, QuestionnaireAction{Kind: ActionNav, NextIndex: 2, InputValue: ""}, ctx)
	if !result.State.InputMode {
		t.Fatal("inputMode not set")
	}
	if len(result.Effects) != 1 || result.Effects[0].Kind != EffectSetInputBuffer || result.Effects[0].Value != "draft" {
		t.Fatalf("effects = %#v", result.Effects)
	}
}

// Leaving input mode records the in-flight draft.
func TestReduceNavLeavingInputModeRecordsDraft(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.InputMode = true
	state.OptionIndex = 2

	result := Reduce(state, QuestionnaireAction{Kind: ActionNav, NextIndex: 0, InputValue: "typed so far"}, ctx)
	if result.State.InputMode {
		t.Fatal("inputMode still set")
	}
	if result.State.CustomDraftsByTab[0] != "typed so far" {
		t.Fatalf("drafts = %v", result.State.CustomDraftsByTab)
	}
}

// Leaving the custom row on a multi-select question persists the answer.
func TestReduceNavPersistsMultiAnswerOnLeave(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.CurrentTab = 1
	state.InputMode = true
	state.OptionIndex = 3 // the custom row (3 options + 1)
	state.MultiSelectChecked = map[int]struct{}{0: {}}

	result := Reduce(state, QuestionnaireAction{Kind: ActionNav, NextIndex: 0, InputValue: "free text"}, ctx)
	answer, ok := result.State.Answers[1]
	if !ok {
		t.Fatalf("answer not persisted: %v", result.State.Answers)
	}
	if answer.Kind != AnswerKindMulti || answer.Answer == nil || *answer.Answer != "free text" {
		t.Fatalf("answer = %#v", answer)
	}
	if len(answer.Selected) != 1 || answer.Selected[0] != "A" {
		t.Fatalf("selected = %v", answer.Selected)
	}
}

// ── input effects ─────────────────────────────────────────────────────────

func TestReduceInputClear(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.InputMode = true
	state.CustomDraftsByTab = map[int]string{0: "text"}

	result := Reduce(state, QuestionnaireAction{Kind: ActionInputClear}, ctx)
	if result.State.CustomDraftsByTab[0] != "" {
		t.Fatalf("drafts = %v", result.State.CustomDraftsByTab)
	}
	if !reflect.DeepEqual(effectsOf(result), []EffectKind{EffectClearInputBuffer}) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
}

func TestReduceInputEdit(t *testing.T) {
	ctx := applyContextFor(singleParams())
	result := Reduce(stateFor(), QuestionnaireAction{Kind: ActionInputEdit, Value: "draft"}, ctx)
	if !reflect.DeepEqual(effectsOf(result), []EffectKind{EffectOpenInputEditor}) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
	if result.Effects[0].Value != "draft" {
		t.Fatalf("effect value = %q", result.Effects[0].Value)
	}
}

func TestReduceInputReplace(t *testing.T) {
	ctx := applyContextFor(singleParams())
	result := Reduce(stateFor(), QuestionnaireAction{Kind: ActionInputReplace, Value: "edited"}, ctx)
	if result.State.CustomDraftsByTab[0] != "edited" {
		t.Fatalf("drafts = %v", result.State.CustomDraftsByTab)
	}
	if result.Effects[0].Kind != EffectSetInputBuffer || result.Effects[0].Value != "edited" {
		t.Fatalf("effects = %#v", result.Effects)
	}
}

// ── tab switch ────────────────────────────────────────────────────────────

func TestReduceTabSwitchResetsAndSeeds(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.OptionIndex = 2
	state.InputMode = true
	state.NotesVisible = true
	state.SubmitChoiceIndex = 1
	state.NotesByTab = map[int]string{1: "note for tab 1"}
	state.CustomDraftsByTab = map[int]string{1: "draft for tab 1"}
	state.Answers = map[int]QuestionAnswer{
		1: {QuestionIndex: 1, Question: "Which features?", Kind: AnswerKindMulti, Selected: []string{"B"}},
	}

	result := Reduce(state, QuestionnaireAction{Kind: ActionTabSwitch, NextTab: 1}, ctx)
	next := result.State
	if next.CurrentTab != 1 {
		t.Fatalf("currentTab = %d", next.CurrentTab)
	}
	if next.InputMode || next.NotesVisible || next.SubmitChoiceIndex != 0 {
		t.Fatalf("state not reset: %#v", next)
	}
	// A multi-select answer focuses the Next row (3 options + the custom row = index
	// 4) so a stray Enter re-confirms instead of replacing the answer.
	if next.OptionIndex != 4 {
		t.Fatalf("optionIndex = %d, want the Next row", next.OptionIndex)
	}
	if next.NotesDraft != "note for tab 1" {
		t.Fatalf("notesDraft = %q", next.NotesDraft)
	}
	// The saved B answer seeds the checkbox set.
	if _, checked := next.MultiSelectChecked[1]; !checked {
		t.Fatalf("checked = %v", next.MultiSelectChecked)
	}
	wantEffects := []EffectKind{EffectSetNotesFocused, EffectSetNotesValue, EffectSetInputBuffer}
	if !reflect.DeepEqual(effectsOf(result), wantEffects) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
	if result.Effects[1].Value != "note for tab 1" || result.Effects[2].Value != "draft for tab 1" {
		t.Fatalf("effects = %#v", result.Effects)
	}
}

// A prior option answer focuses that option on return.
func TestReduceTabSwitchFocusesPriorOption(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.Answers = map[int]QuestionAnswer{
		0: {QuestionIndex: 0, Question: "Which auth?", Kind: AnswerKindOption, Answer: strPtr("B")},
	}
	result := Reduce(state, QuestionnaireAction{Kind: ActionTabSwitch, NextTab: 0}, ctx)
	if result.State.OptionIndex != 1 {
		t.Fatalf("optionIndex = %d, want 1", result.State.OptionIndex)
	}
}

// A custom answer starts at row 0 (focusing the custom row would enter input
// mode).
func TestReduceTabSwitchCustomAnswerStartsAtZero(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.Answers = map[int]QuestionAnswer{
		0: {QuestionIndex: 0, Kind: AnswerKindCustom, Answer: strPtr("custom")},
	}
	result := Reduce(state, QuestionnaireAction{Kind: ActionTabSwitch, NextTab: 0}, ctx)
	if result.State.OptionIndex != 0 {
		t.Fatalf("optionIndex = %d", result.State.OptionIndex)
	}
}

// ── confirm ───────────────────────────────────────────────────────────────

func TestReduceConfirmOptionWithPreviewAndNotes(t *testing.T) {
	params := singleParams()
	params.Questions[0].Options[0].Preview = strPtr("```go\nx\n```")
	ctx := applyContextFor(params)

	state := stateFor()
	state.NotesByTab = map[int]string{0: "a note"}

	result := Reduce(state, QuestionnaireAction{
		Kind:   ActionConfirm,
		Answer: &QuestionAnswer{QuestionIndex: 0, Question: "Which auth?", Kind: AnswerKindOption, Answer: strPtr("A")},
	}, ctx)

	if !reflect.DeepEqual(effectsOf(result), []EffectKind{EffectDone}) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
	done := doneResult(t, result)
	if done.Cancelled || len(done.Answers) != 1 {
		t.Fatalf("done = %#v", done)
	}
	answer := done.Answers[0]
	if answer.Preview == nil || *answer.Preview != "```go\nx\n```" {
		t.Fatalf("preview = %v", answer.Preview)
	}
	if answer.Notes == nil || *answer.Notes != "a note" {
		t.Fatalf("notes = %v", answer.Notes)
	}
}

// A custom answer on a multi-select question is promoted to a multi answer.
func TestReduceConfirmPromotesCustomToMulti(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.CurrentTab = 1
	state.MultiSelectChecked = map[int]struct{}{0: {}, 2: {}}

	result := Reduce(state, QuestionnaireAction{
		Kind:           ActionConfirm,
		Answer:         &QuestionAnswer{QuestionIndex: 1, Question: "Which features?", Kind: AnswerKindCustom, Answer: strPtr("free")},
		AutoAdvanceTab: intPtr(2),
	}, ctx)

	// autoAdvanceTab set → a tab switch, not done.
	if result.State.CurrentTab != 2 {
		t.Fatalf("currentTab = %d", result.State.CurrentTab)
	}
	answer := result.State.Answers[1]
	if answer.Kind != AnswerKindMulti {
		t.Fatalf("kind = %q", answer.Kind)
	}
	if len(answer.Selected) != 2 || answer.Selected[0] != "A" || answer.Selected[1] != "C" {
		t.Fatalf("selected = %v", answer.Selected)
	}
}

// Confirming a custom answer clears its draft; confirming a plain option keeps
// an unrelated draft.
func TestReduceConfirmClearsCustomDraft(t *testing.T) {
	ctx := applyContextFor(singleParams())

	custom := stateFor()
	custom.CustomDraftsByTab = map[int]string{0: "typed"}
	result := Reduce(custom, QuestionnaireAction{
		Kind:   ActionConfirm,
		Answer: &QuestionAnswer{QuestionIndex: 0, Kind: AnswerKindCustom, Answer: strPtr("typed")},
	}, ctx)
	if _, present := result.State.CustomDraftsByTab[0]; present {
		t.Fatalf("draft kept: %v", result.State.CustomDraftsByTab)
	}

	option := stateFor()
	option.CustomDraftsByTab = map[int]string{0: "typed"}
	result = Reduce(option, QuestionnaireAction{
		Kind:   ActionConfirm,
		Answer: &QuestionAnswer{QuestionIndex: 0, Kind: AnswerKindOption, Answer: strPtr("A")},
	}, ctx)
	if option.CustomDraftsByTab[0] != "typed" {
		t.Fatalf("option confirm dropped an unrelated draft")
	}
}

// ── toggle / multi confirm ────────────────────────────────────────────────

func TestReduceTogglePersistsMultiAnswer(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.CurrentTab = 1

	result := Reduce(state, QuestionnaireAction{Kind: ActionToggle, Index: 1}, ctx)
	answer, ok := result.State.Answers[1]
	if !ok {
		t.Fatalf("answer not persisted: %v", result.State.Answers)
	}
	if len(answer.Selected) != 1 || answer.Selected[0] != "B" {
		t.Fatalf("selected = %v", answer.Selected)
	}
	if answer.Answer != nil {
		t.Fatalf("custom text = %v", answer.Answer)
	}

	// Toggling it off with nothing else checked deletes the answer.
	removed := Reduce(result.State, QuestionnaireAction{Kind: ActionToggle, Index: 1}, ctx)
	if _, present := removed.State.Answers[1]; present {
		t.Fatalf("answer kept after untoggle: %v", removed.State.Answers)
	}
}

func TestReduceToggleKeepsCustomOnlyAnswer(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.CurrentTab = 1
	state.CustomDraftsByTab = map[int]string{1: "custom only"}

	result := Reduce(state, QuestionnaireAction{Kind: ActionToggle, Index: 1}, ctx)
	answer := result.State.Answers[1]
	if answer.Answer == nil || *answer.Answer != "custom only" {
		t.Fatalf("answer = %#v", answer)
	}

	// Untoggling with a custom draft present keeps the answer.
	removed := Reduce(result.State, QuestionnaireAction{Kind: ActionToggle, Index: 1}, ctx)
	if _, present := removed.State.Answers[1]; !present {
		t.Fatal("answer with custom text removed")
	}
}

func TestReduceMultiConfirm(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.CurrentTab = 1
	state.NotesByTab = map[int]string{1: "pending note"}

	result := Reduce(state, QuestionnaireAction{
		Kind:           ActionMultiConfirm,
		Selected:       []string{"A", "B"},
		Value:          "extra",
		AutoAdvanceTab: intPtr(2),
	}, ctx)

	answer := result.State.Answers[1]
	if answer.Kind != AnswerKindMulti || answer.Answer == nil || *answer.Answer != "extra" {
		t.Fatalf("answer = %#v", answer)
	}
	if answer.Notes == nil || *answer.Notes != "pending note" {
		t.Fatalf("notes = %v", answer.Notes)
	}
	if result.State.CurrentTab != 2 {
		t.Fatalf("currentTab = %d", result.State.CurrentTab)
	}
}

func TestReduceMultiConfirmFinishesOnLastQuestion(t *testing.T) {
	ctx := applyContextFor(singleMultiParams())
	result := Reduce(stateFor(), QuestionnaireAction{
		Kind:     ActionMultiConfirm,
		Selected: []string{"A"},
	}, ctx)
	done := doneResult(t, result)
	if done.Cancelled || len(done.Answers) != 1 {
		t.Fatalf("done = %#v", done)
	}
}

// singleMultiParams is a one-question multi-select questionnaire (no auto
// advance, so multi_confirm finishes).
func singleMultiParams() QuestionParams {
	return QuestionParams{Questions: []QuestionData{multiQuestion("Which?", "A", "B")}}
}

// ── notes ─────────────────────────────────────────────────────────────────

func TestReduceNotesEnter(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.NotesByTab = map[int]string{0: "existing"}

	result := Reduce(state, QuestionnaireAction{Kind: ActionNotesEnter}, ctx)
	if !result.State.NotesVisible || result.State.NotesDraft != "existing" {
		t.Fatalf("state = %#v", result.State)
	}
	if !reflect.DeepEqual(effectsOf(result), []EffectKind{EffectSetNotesValue, EffectSetNotesFocused}) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
	if result.Effects[0].Value != "existing" || !result.Effects[1].Focused {
		t.Fatalf("effects = %#v", result.Effects)
	}
}

func TestReduceNotesEnterSeedsFromAnswerMirror(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.Answers = map[int]QuestionAnswer{
		0: {QuestionIndex: 0, Kind: AnswerKindOption, Answer: strPtr("A"), Notes: strPtr("mirrored")},
	}
	result := Reduce(state, QuestionnaireAction{Kind: ActionNotesEnter}, ctx)
	if result.State.NotesDraft != "mirrored" {
		t.Fatalf("notesDraft = %q", result.State.NotesDraft)
	}
}

func TestReduceNotesExitCommitsTrimmed(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.NotesVisible = true
	state.NotesDraft = "  trimmed note \n"

	result := Reduce(state, QuestionnaireAction{Kind: ActionNotesExit}, ctx)
	if result.State.NotesVisible {
		t.Fatal("notesVisible still set")
	}
	if result.State.NotesByTab[0] != "trimmed note" {
		t.Fatalf("notesByTab = %v", result.State.NotesByTab)
	}
	if !reflect.DeepEqual(effectsOf(result), []EffectKind{EffectSetNotesFocused}) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
	if result.Effects[0].Focused {
		t.Fatal("notes focus not cleared")
	}
}

// A blank draft clears the note and strips the mirror off an existing answer.
func TestReduceNotesExitClears(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.NotesVisible = true
	state.NotesDraft = "   "
	state.NotesByTab = map[int]string{0: "old"}
	state.Answers = map[int]QuestionAnswer{
		0: {QuestionIndex: 0, Kind: AnswerKindOption, Answer: strPtr("A"), Notes: strPtr("old")},
	}

	result := Reduce(state, QuestionnaireAction{Kind: ActionNotesExit}, ctx)
	if _, present := result.State.NotesByTab[0]; present {
		t.Fatalf("notesByTab = %v", result.State.NotesByTab)
	}
	if result.State.Answers[0].Notes != nil {
		t.Fatalf("mirror kept: %v", result.State.Answers[0].Notes)
	}
}

// A committed note mirrors onto an existing answer.
func TestReduceNotesExitMirrorsOntoAnswer(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.NotesVisible = true
	state.NotesDraft = "note"
	state.Answers = map[int]QuestionAnswer{
		0: {QuestionIndex: 0, Kind: AnswerKindOption, Answer: strPtr("A")},
	}
	result := Reduce(state, QuestionnaireAction{Kind: ActionNotesExit}, ctx)
	if result.State.Answers[0].Notes == nil || *result.State.Answers[0].Notes != "note" {
		t.Fatalf("mirror = %v", result.State.Answers[0].Notes)
	}
}

func TestReduceNotesForward(t *testing.T) {
	ctx := applyContextFor(singleParams())
	result := Reduce(stateFor(), QuestionnaireAction{Kind: ActionNotesForward, Data: "x"}, ctx)
	if !reflect.DeepEqual(effectsOf(result), []EffectKind{EffectForwardNotesKeystroke}) {
		t.Fatalf("effects = %v", effectsOf(result))
	}
	if result.Effects[0].Data != "x" {
		t.Fatalf("data = %q", result.Effects[0].Data)
	}
}

// ── done paths ────────────────────────────────────────────────────────────

func TestReduceCancel(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.Answers = map[int]QuestionAnswer{
		0: {QuestionIndex: 0, Kind: AnswerKindOption, Answer: strPtr("A")},
	}
	result := Reduce(state, QuestionnaireAction{Kind: ActionCancel}, ctx)
	done := doneResult(t, result)
	if !done.Cancelled {
		t.Fatal("not cancelled")
	}
	if len(done.Answers) != 1 {
		t.Fatalf("answers = %#v", done.Answers)
	}
}

func TestReduceSubmit(t *testing.T) {
	ctx := applyContextFor(singleParams())
	result := Reduce(stateFor(), QuestionnaireAction{Kind: ActionSubmit}, ctx)
	done := doneResult(t, result)
	if done.Cancelled {
		t.Fatal("submit marked cancelled")
	}
}

// The global note lifts from the len(questions) pseudo-index on both submit and
// cancel; a blank note is dropped entirely.
func TestReduceGlobalNoteLift(t *testing.T) {
	ctx := applyContextFor(multiParams())

	for _, kind := range []string{ActionSubmit, ActionCancel} {
		state := stateFor()
		state.NotesByTab = map[int]string{2: "global"}
		done := doneResult(t, Reduce(state, QuestionnaireAction{Kind: kind}, ctx))
		if done.GlobalNote == nil || *done.GlobalNote != "global" {
			t.Fatalf("%s globalNote = %v", kind, done.GlobalNote)
		}
	}

	blank := stateFor()
	blank.NotesByTab = map[int]string{2: ""}
	done := doneResult(t, Reduce(blank, QuestionnaireAction{Kind: ActionSubmit}, ctx))
	if done.GlobalNote != nil {
		t.Fatalf("blank global note kept: %v", done.GlobalNote)
	}
}

// A per-question note never lifts as the global note.
func TestReducePerQuestionNoteIsNotGlobal(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.NotesByTab = map[int]string{0: "per question"}
	done := doneResult(t, Reduce(state, QuestionnaireAction{Kind: ActionSubmit}, ctx))
	if done.GlobalNote != nil {
		t.Fatalf("globalNote = %v", done.GlobalNote)
	}
}

// Answers come back in question order regardless of insertion order.
func TestReduceAnswersAreOrdered(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.Answers = map[int]QuestionAnswer{
		1: {QuestionIndex: 1, Question: "Which features?", Kind: AnswerKindMulti, Selected: []string{"A"}},
		0: {QuestionIndex: 0, Question: "Which auth?", Kind: AnswerKindOption, Answer: strPtr("A")},
	}
	done := doneResult(t, Reduce(state, QuestionnaireAction{Kind: ActionSubmit}, ctx))
	if len(done.Answers) != 2 || done.Answers[0].QuestionIndex != 0 || done.Answers[1].QuestionIndex != 1 {
		t.Fatalf("answers = %#v", done.Answers)
	}
}

// ── submit nav / collapse / ignore ───────────────────────────────────────

func TestReduceSubmitNav(t *testing.T) {
	ctx := applyContextFor(multiParams())
	result := Reduce(stateFor(), QuestionnaireAction{Kind: ActionSubmitNav, SubmitNextIndex: 1}, ctx)
	if result.State.SubmitChoiceIndex != 1 {
		t.Fatalf("submitChoiceIndex = %d", result.State.SubmitChoiceIndex)
	}
	if len(result.Effects) != 0 {
		t.Fatalf("effects = %v", effectsOf(result))
	}
}

func TestReduceToggleCollapsed(t *testing.T) {
	ctx := applyContextFor(singleParams())

	first := Reduce(stateFor(), QuestionnaireAction{Kind: ActionToggleCollapsed}, ctx)
	if !first.State.Collapsed {
		t.Fatal("not collapsed")
	}
	if !reflect.DeepEqual(effectsOf(first), []EffectKind{EffectSetOverlayHidden}) {
		t.Fatalf("effects = %v", effectsOf(first))
	}
	if !first.Effects[0].Hidden {
		t.Fatal("effect should hide")
	}

	second := Reduce(first.State, QuestionnaireAction{Kind: ActionToggleCollapsed}, ctx)
	if second.State.Collapsed {
		t.Fatal("still collapsed")
	}
	if second.Effects[0].Hidden {
		t.Fatal("effect should show")
	}
}

func TestReduceIgnore(t *testing.T) {
	ctx := applyContextFor(singleParams())
	state := stateFor()
	state.OptionIndex = 1
	result := Reduce(state, QuestionnaireAction{Kind: ActionIgnore}, ctx)
	if result.State.OptionIndex != 1 || len(result.Effects) != 0 {
		t.Fatalf("ignore changed state: %#v", result)
	}
}

// An unknown action kind is treated as ignore rather than panicking.
func TestReduceUnknownKindIsIgnore(t *testing.T) {
	ctx := applyContextFor(singleParams())
	result := Reduce(stateFor(), QuestionnaireAction{Kind: "no_such_action"}, ctx)
	if len(result.Effects) != 0 {
		t.Fatalf("effects = %v", effectsOf(result))
	}
}

// The reducer never mutates its input state.
func TestReduceDoesNotMutateInput(t *testing.T) {
	ctx := applyContextFor(multiParams())
	state := stateFor()
	state.CurrentTab = 1
	state.MultiSelectChecked = map[int]struct{}{0: {}}
	before := state.clone()

	_ = Reduce(state, QuestionnaireAction{Kind: ActionToggle, Index: 1}, ctx)
	if _, mutated := state.MultiSelectChecked[1]; mutated {
		t.Fatal("input checkbox set mutated")
	}
	if len(state.MultiSelectChecked) != 1 {
		t.Fatalf("input checkbox set size = %d", len(state.MultiSelectChecked))
	}
	if !reflect.DeepEqual(state.Answers, before.Answers) {
		t.Fatalf("input answers mutated: %v", state.Answers)
	}
}

// Every declared action kind has a handler.
func TestEveryActionKindHasAHandler(t *testing.T) {
	kinds := []string{
		ActionNav, ActionInputClear, ActionInputEdit, ActionInputReplace, ActionTabSwitch,
		ActionConfirm, ActionToggle, ActionMultiConfirm, ActionCancel, ActionNotesEnter,
		ActionNotesExit, ActionSubmit, ActionSubmitNav, ActionNotesForward,
		ActionToggleCollapsed, ActionIgnore,
	}
	for _, kind := range kinds {
		if _, present := handlers[kind]; !present {
			t.Fatalf("no handler for %q", kind)
		}
	}
	if len(handlers) != len(kinds) {
		t.Fatalf("handler count = %d, want %d", len(handlers), len(kinds))
	}
}

// intPtr is the auto-advance pointer helper.
func intPtr(n int) *int { return &n }
