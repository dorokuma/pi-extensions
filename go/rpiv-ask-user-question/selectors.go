package rpivaskuserquestion

// Ports state/selectors/{contract,derivations,focus,projections}.ts (v2.15.0):
// the pure state → props derivations the view layer consumes.

// ActiveView is the discriminated focus union. Priority: notes > submit >
// options — the same cascade RouteKey uses.
type ActiveView string

// The three focus owners.
const (
	ActiveViewNotes   ActiveView = "notes"
	ActiveViewOptions ActiveView = "options"
	ActiveViewSubmit  ActiveView = "submit"
)

// SelectActiveView mirrors selectActiveView.
func SelectActiveView(state QuestionnaireState, totalQuestions int) ActiveView {
	if state.NotesVisible {
		return ActiveViewNotes
	}
	if state.CurrentTab == totalQuestions {
		return ActiveViewSubmit
	}
	return ActiveViewOptions
}

// ConfirmedIndicator mirrors the selectConfirmedIndicator result.
type ConfirmedIndicator struct {
	Index         int
	LabelOverride *string
}

// SelectConfirmedIndicator mirrors selectConfirmedIndicator: which row in the
// active tab should be marked as previously confirmed. Multi-select handles its
// own boxes, so it never reports an indicator.
func SelectConfirmedIndicator(
	questions []QuestionData,
	currentTab int,
	answers map[int]QuestionAnswer,
	items []WrappingSelectItem,
) *ConfirmedIndicator {
	if currentTab < 0 || currentTab >= len(questions) {
		return nil
	}
	if questions[currentTab].MultiSelect {
		return nil
	}
	prior, ok := answers[currentTab]
	if !ok {
		return nil
	}
	if prior.Kind == AnswerKindCustom {
		otherIndex := -1
		for i, item := range items {
			if item.Kind == RowKindOther {
				otherIndex = i
				break
			}
		}
		if otherIndex < 0 {
			return nil
		}
		override := ""
		if prior.Answer != nil {
			override = *prior.Answer
		}
		return &ConfirmedIndicator{Index: otherIndex, LabelOverride: &override}
	}
	if prior.Kind != AnswerKindOption || prior.Answer == nil {
		return nil
	}
	for i, item := range items {
		if item.Kind == RowKindOption && item.Label == *prior.Answer {
			return &ConfirmedIndicator{Index: i}
		}
	}
	return nil
}

// SelectActivePreviewPaneIndex mirrors selectActivePreviewPaneIndex: the Submit
// tab reuses the last question's pane purely for layout.
func SelectActivePreviewPaneIndex(currentTab, totalQuestions int) int {
	if totalQuestions <= 0 {
		return 0
	}
	if currentTab < totalQuestions-1 {
		return maxInt(0, currentTab)
	}
	return totalQuestions - 1
}

// SelectSavedNote mirrors selectSavedNote: the committed note for a tab, or
// nil when it has none. The side-band is authoritative, with answer.notes as
// the mirror; a present empty string means the note was cleared.
func SelectSavedNote(notesByTab map[int]string, answers map[int]QuestionAnswer, tab int) *string {
	var note string
	if value, ok := notesByTab[tab]; ok {
		note = value
	} else if answer, ok := answers[tab]; ok && answer.Notes != nil {
		note = *answer.Notes
	}
	if len(note) == 0 {
		return nil
	}
	return &note
}
