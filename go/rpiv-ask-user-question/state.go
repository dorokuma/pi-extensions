package rpivaskuserquestion

// Ports state/state.ts (v2.15.0): the canonical questionnaire state and the
// per-tick runtime context.

// QuestionnaireState is the canonical state for the dialog. Both the dispatcher
// (RouteKey) and the view layer read this same shape.
//
// The map/set fields are exported as plain Go maps for readability; the reducer
// always copies before mutating, so a state value can be shared freely between
// the reducer and the adapter (the TS ReadonlyMap/ReadonlySet contract).
type QuestionnaireState struct {
	CurrentTab   int
	OptionIndex  int
	InputMode    bool
	NotesVisible bool

	Answers map[int]QuestionAnswer
	// MultiSelectChecked is the checkbox set for the current multi-select tab.
	MultiSelectChecked map[int]struct{}
	// CustomDraftsByTab holds in-flight custom answers keyed by tab. A present
	// empty string overrides an older answer.
	CustomDraftsByTab map[int]string
	// NotesByTab is the pre-answer notes side-band, keyed by tab index. It is
	// decoupled from Answers so adding notes does NOT mark a question answered.
	// The Submit-tab global note lives at the `len(questions)` pseudo-index.
	NotesByTab map[int]string

	// SubmitChoiceIndex is the focused row in the Submit-tab picker (0 = Submit,
	// 1 = Cancel). Reset on tab switch.
	SubmitChoiceIndex int
	// NotesDraft mirrors the in-flight notes editor.
	NotesDraft string
	// Collapsed hides the questionnaire so the transcript behind it is readable.
	Collapsed bool
}

// NewQuestionnaireState mirrors the session's initialState().
func NewQuestionnaireState() QuestionnaireState {
	return QuestionnaireState{
		Answers:            map[int]QuestionAnswer{},
		MultiSelectChecked: map[int]struct{}{},
		CustomDraftsByTab:  map[int]string{},
		NotesByTab:         map[int]string{},
	}
}

// clone copies the mutable containers so a reducer step never mutates its input.
func (s QuestionnaireState) clone() QuestionnaireState {
	out := s
	out.Answers = copyAnswers(s.Answers)
	out.MultiSelectChecked = copySet(s.MultiSelectChecked)
	out.CustomDraftsByTab = copyStringMap(s.CustomDraftsByTab)
	out.NotesByTab = copyStringMap(s.NotesByTab)
	return out
}

func copyAnswers(in map[int]QuestionAnswer) map[int]QuestionAnswer {
	out := make(map[int]QuestionAnswer, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copySet(in map[int]struct{}) map[int]struct{} {
	out := make(map[int]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}

func copyStringMap(in map[int]string) map[int]string {
	out := make(map[int]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// QuestionnaireRuntime is the per-tick context the dispatcher needs alongside
// canonical state. Held separately because keybindings and the input buffer must
// never reach view setProps consumers.
type QuestionnaireRuntime struct {
	Keybindings      Keybindings
	InputBuffer      string
	CanMoveInputUp   bool
	CanMoveInputDown bool
	Questions        []QuestionData
	IsMulti          bool
	CurrentItem      *WrappingSelectItem
	Items            []WrappingSelectItem
	// CollapseKey is the resolved collapse/expand spec ("ctrl+]", "alt+o" or
	// "off"). "off" disables the shortcut.
	CollapseKey string
	// NotesKey is the resolved notes key ("n" by default).
	NotesKey string
}

// Keybindings is the subset of pi-tui's keybinding table the router consults.
// The Methods match semantic names like "tui.select.up".
type Keybindings interface {
	Matches(data string, name string) bool
}
