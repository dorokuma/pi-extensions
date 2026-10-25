package rpivaskuserquestion

import (
	"testing"
)

// fakeKeybindings maps semantic keybinding names to the raw chunks that trigger
// them, so router tests drive the cascade without a live keybindings manager.
type fakeKeybindings map[string][]string

// Matches reports whether data triggers the named keybinding.
func (k fakeKeybindings) Matches(data, name string) bool {
	for _, chunk := range k[name] {
		if chunk == data {
			return true
		}
	}
	return false
}

// defaultBindings uses pi's defaults: confirm/submit/cancel on their usual keys.
func defaultBindings() fakeKeybindings {
	return fakeKeybindings{
		"tui.select.up":                {"\x1b[A"},
		"tui.select.down":              {"\x1b[B"},
		"tui.select.confirm":           {"\r"},
		"tui.input.submit":             {"\r"},
		"tui.select.cancel":            {"\x1b"},
		"tui.input.newLine":            {"\x1b\r"},
		"tui.editor.cursorUp":          {"\x1b[1;5A"},
		"tui.editor.cursorDown":        {"\x1b[1;5B"},
		"tui.editor.deleteToLineStart": {"\x15"},
		"app.editor.external":          {"\x1b[103;5u"},
	}
}

// runtimeFor builds a router runtime for params at the given tab.
func runtimeFor(params QuestionParams, tab int, optionIndex int) QuestionnaireRuntime {
	itemsByTab := BuildItemsByTab(params)
	var items []WrappingSelectItem
	if tab >= 0 && tab < len(itemsByTab) {
		items = itemsByTab[tab]
	}
	var current *WrappingSelectItem
	if optionIndex >= 0 && optionIndex < len(items) {
		item := items[optionIndex]
		current = &item
	}
	return QuestionnaireRuntime{
		Keybindings: defaultBindings(),
		Questions:   params.Questions,
		IsMulti:     len(params.Questions) > 1,
		CurrentItem: current,
		Items:       items,
		CollapseKey: DefaultCollapseKey,
		NotesKey:    DefaultNotesKey,
	}
}

func TestWrapTab(t *testing.T) {
	cases := []struct{ index, total, want int }{
		{0, 3, 0}, {2, 3, 2}, {3, 3, 0}, {4, 3, 1}, {-1, 3, 2}, {-3, 3, 0}, {-4, 3, 2}, {0, 0, 0}, {5, 1, 0},
	}
	for _, tc := range cases {
		if got := WrapTab(tc.index, tc.total); got != tc.want {
			t.Fatalf("WrapTab(%d,%d) = %d, want %d", tc.index, tc.total, got, tc.want)
		}
	}
}

func TestAllAnswered(t *testing.T) {
	params := multiParams()
	runtime := runtimeFor(params, 0, 0)

	state := stateFor()
	if AllAnswered(state, runtime) {
		t.Fatal("empty state reported all-answered")
	}
	state.Answers[0] = QuestionAnswer{QuestionIndex: 0}
	if AllAnswered(state, runtime) {
		t.Fatal("partial state reported all-answered")
	}
	state.Answers[1] = QuestionAnswer{QuestionIndex: 1}
	if !AllAnswered(state, runtime) {
		t.Fatal("fully answered state reported incomplete")
	}

	// No questions at all is never "all answered".
	if AllAnswered(stateFor(), QuestionnaireRuntime{}) {
		t.Fatal("empty questionnaire reported all-answered")
	}
}

// ── collapse intercept ────────────────────────────────────────────────────

// The collapse key is intercepted at the top and works from every inner state.
func TestRouteKeyCollapseFromEveryInnerState(t *testing.T) {
	params := multiParams()
	states := map[string]QuestionnaireState{
		"resting":      stateFor(),
		"notes":        {NotesVisible: true},
		"input mode":   {InputMode: true},
		"collapsed":    {Collapsed: true},
		"submit tab":   {CurrentTab: 2},
		"multi-select": {CurrentTab: 1},
	}
	for name, state := range states {
		runtime := runtimeFor(params, state.CurrentTab, 0)
		action := RouteKey("\x1d", state, runtime) // Ctrl+]
		if action.Kind != ActionToggleCollapsed {
			t.Fatalf("%s: action = %q, want toggle_collapsed", name, action.Kind)
		}
	}
}

// "off" disables the collapse shortcut entirely.
func TestRouteKeyCollapseOffDisables(t *testing.T) {
	runtime := runtimeFor(singleParams(), 0, 0)
	runtime.CollapseKey = CollapseKeyOff
	if action := RouteKey("\x1d", stateFor(), runtime); action.Kind == ActionToggleCollapsed {
		t.Fatal("collapse key acted while off")
	}
}

// An empty collapseKey (a stale outer module) disables the shortcut rather than
// panicking inside the key matcher.
func TestRouteKeyCollapseEmptyKeyIsDisabled(t *testing.T) {
	runtime := runtimeFor(singleParams(), 0, 0)
	runtime.CollapseKey = ""
	if action := RouteKey("\x1d", stateFor(), runtime); action.Kind == ActionToggleCollapsed {
		t.Fatal("empty collapse key acted")
	}
}

// ── collapsed lockout ─────────────────────────────────────────────────────

// While collapsed, every keystroke is swallowed except cancel.
func TestRouteKeyCollapsedLockout(t *testing.T) {
	params := singleParams()
	state := stateFor()
	state.Collapsed = true
	runtime := runtimeFor(params, 0, 0)

	for _, chunk := range []string{"\x1b[A", "\x1b[B", "\r", "1", "n", " ", "\t", "a"} {
		action := RouteKey(chunk, state, runtime)
		if action.Kind != ActionIgnore {
			t.Fatalf("chunk %q → %q, want ignore", chunk, action.Kind)
		}
	}
	if action := RouteKey("\x1b", state, runtime); action.Kind != ActionCancel {
		t.Fatalf("esc → %q, want cancel", action.Kind)
	}
}

// The collapse key still toggles while collapsed (the intercept runs first).
func TestRouteKeyCollapseTogglesBackWhileCollapsed(t *testing.T) {
	state := stateFor()
	state.Collapsed = true
	if action := RouteKey("\x1d", state, runtimeFor(singleParams(), 0, 0)); action.Kind != ActionToggleCollapsed {
		t.Fatalf("action = %q", action.Kind)
	}
}

// ── notes mode ───────────────────────────────────────────────────────────

func TestRouteKeyNotesMode(t *testing.T) {
	params := singleParams()
	state := stateFor()
	state.NotesVisible = true
	runtime := runtimeFor(params, 0, 0)

	cases := map[string]string{
		"\x1b":   ActionNotesExit,    // cancel exits the editor
		"\x1b\r": ActionNotesForward, // newline is forwarded
		"\r":     ActionNotesExit,    // confirm exits the editor
		"a":      ActionNotesForward, // text is forwarded
		"\x1b[A": ActionNotesForward, // arrows are forwarded too
	}
	for chunk, want := range cases {
		action := RouteKey(chunk, state, runtime)
		if action.Kind != want {
			t.Fatalf("chunk %q → %q, want %q", chunk, action.Kind, want)
		}
	}

	// A forwarded keystroke carries the raw chunk.
	action := RouteKey("typed", state, runtime)
	if action.Data != "typed" {
		t.Fatalf("forwarded data = %q", action.Data)
	}
}

// ── input mode ───────────────────────────────────────────────────────────

func TestRouteKeyInputMode(t *testing.T) {
	params := singleParams()
	runtime := runtimeFor(params, 0, 2) // the Type-something. row
	state := stateFor()
	state.InputMode = true
	state.OptionIndex = 2

	t.Run("newline wins over confirm", func(t *testing.T) {
		// A configuration that folds Enter into newline must not confirm.
		kb := defaultBindings()
		kb["tui.input.newLine"] = []string{"\r"}
		runtime := runtime
		runtime.Keybindings = kb
		if action := RouteKey("\r", state, runtime); action.Kind != ActionIgnore {
			t.Fatalf("action = %q, want ignore", action.Kind)
		}
	})

	t.Run("confirm builds a custom answer", func(t *testing.T) {
		runtimeWithBuffer := runtime
		runtimeWithBuffer.InputBuffer = "typed"
		action := RouteKey("\r", state, runtimeWithBuffer)
		if action.Kind != ActionConfirm {
			t.Fatalf("action = %q", action.Kind)
		}
		if action.Answer == nil || action.Answer.Kind != AnswerKindCustom || *action.Answer.Answer != "typed" {
			t.Fatalf("answer = %#v", action.Answer)
		}
	})

	t.Run("confirm with a blank draft stays on the row", func(t *testing.T) {
		runtimeWithBuffer := runtime
		runtimeWithBuffer.InputBuffer = "   "
		if action := RouteKey("\r", state, runtimeWithBuffer); action.Kind != ActionIgnore {
			t.Fatalf("action = %q, want ignore", action.Kind)
		}
	})

	t.Run("ctrl+u clears", func(t *testing.T) {
		if action := RouteKey("\x15", state, runtime); action.Kind != ActionInputClear {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("external editor", func(t *testing.T) {
		runtimeWithBuffer := runtime
		runtimeWithBuffer.InputBuffer = "draft"
		action := RouteKey("\x1b[103;5u", state, runtimeWithBuffer)
		if action.Kind != ActionInputEdit || action.Value != "draft" {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("esc cancels", func(t *testing.T) {
		if action := RouteKey("\x1b", state, runtime); action.Kind != ActionCancel {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("editor cursor keys are swallowed only when movable", func(t *testing.T) {
		short := runtime
		short.Keybindings = fakeKeybindings{
			"tui.select.up":                {"\x1b[A"},
			"tui.select.down":              {"\x1b[B"},
			"tui.select.confirm":           {"\r"},
			"tui.input.submit":             {"\r"},
			"tui.select.cancel":            {"\x1b"},
			"tui.input.newLine":            {"\x1b\r"},
			"tui.editor.cursorUp":          {"\x1b[1;5A"},
			"tui.editor.cursorDown":        {"\x1b[1;5B"},
			"tui.editor.deleteToLineStart": {"\x15"},
			"app.editor.external":          {"\x1b[103;5u"},
		}
		// A multi-line draft at the first line: cursorUp is swallowed, cursorDown
		// falls through to option navigation.
		movable := short
		movable.CanMoveInputUp = true
		movable.CanMoveInputDown = false
		if action := RouteKey("\x1b[1;5A", state, movable); action.Kind != ActionIgnore {
			t.Fatalf("up → %q", action.Kind)
		}
		// cursorDown is bound to \x1b[1;5B; with canMoveInputDown false the branch
		// is skipped and the plain down binding then navigates.
		if action := RouteKey("\x1b[1;5B", state, movable); action.Kind != ActionIgnore {
			t.Fatalf("cursorDown at last line → %q, want ignore", action.Kind)
		}
		// A genuine movable cursorDown is swallowed.
		bothMovable := short
		bothMovable.CanMoveInputUp = true
		bothMovable.CanMoveInputDown = true
		if action := RouteKey("\x1b[1;5B", state, bothMovable); action.Kind != ActionIgnore {
			t.Fatalf("movable cursorDown → %q, want ignore", action.Kind)
		}
		// The plain down key still navigates while in input mode.
		if action := RouteKey("\x1b[B", state, bothMovable); action.Kind != ActionNav {
			t.Fatalf("plain down → %q, want nav", action.Kind)
		}
	})

	t.Run("up/down navigate rows", func(t *testing.T) {
		if action := RouteKey("\x1b[A", state, runtime); action.Kind != ActionNav {
			t.Fatalf("up → %q", action.Kind)
		}
		if action := RouteKey("\x1b[B", state, runtime); action.Kind != ActionNav {
			t.Fatalf("down → %q", action.Kind)
		}
	})

	t.Run("other keys are ignored", func(t *testing.T) {
		if action := RouteKey("x", state, runtime); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})
}

// Enter in input mode on a multi-select question commits the multi answer.
func TestRouteKeyInputModeMultiConfirm(t *testing.T) {
	params := multiParams()
	runtime := runtimeFor(params, 1, 3) // custom row of the 3-option question
	runtime.InputBuffer = "custom text"
	state := stateFor()
	state.CurrentTab = 1
	state.InputMode = true
	state.OptionIndex = 3
	state.MultiSelectChecked = map[int]struct{}{0: {}}

	action := RouteKey("\r", state, runtime)
	if action.Kind != ActionMultiConfirm {
		t.Fatalf("action = %q", action.Kind)
	}
	if len(action.Selected) != 1 || action.Selected[0] != "A" {
		t.Fatalf("selected = %v", action.Selected)
	}
	if action.Value != "custom text" {
		t.Fatalf("value = %q", action.Value)
	}
	if action.AutoAdvanceTab == nil || *action.AutoAdvanceTab != 2 {
		t.Fatalf("autoAdvanceTab = %v", action.AutoAdvanceTab)
	}
}

// ── submit tab ───────────────────────────────────────────────────────────

func TestRouteKeySubmitTab(t *testing.T) {
	params := multiParams()
	runtime := runtimeFor(params, 2, 0) // the Submit tab
	state := stateFor()
	state.CurrentTab = 2

	cases := []struct {
		name  string
		chunk string
		state *QuestionnaireState
		want  string
		extra func(QuestionnaireAction) bool
	}{
		{name: "esc cancels", chunk: "\x1b", want: ActionCancel},
		{name: "enter on row 0 submits", chunk: "\r", want: ActionSubmit},
		{name: "enter on row 1 cancels", chunk: "\r", state: statePtr(withSubmitChoice(1)), want: ActionCancel},
		{name: "down moves to cancel", chunk: "\x1b[B", want: ActionSubmitNav, extra: func(a QuestionnaireAction) bool { return a.SubmitNextIndex == 1 }},
		{name: "up wraps to cancel", chunk: "\x1b[A", want: ActionSubmitNav, extra: func(a QuestionnaireAction) bool { return a.SubmitNextIndex == 1 }},
		{name: "down from cancel wraps to submit", chunk: "\x1b[B", state: statePtr(withSubmitChoice(1)), want: ActionSubmitNav, extra: func(a QuestionnaireAction) bool { return a.SubmitNextIndex == 0 }},
		{name: "tab switches", chunk: "\t", want: ActionTabSwitch},
		{name: "left switches back", chunk: "\x1b[D", want: ActionTabSwitch},
		{name: "n opens the global note", chunk: "n", want: ActionNotesEnter},
		{name: "other keys ignored", chunk: "x", want: ActionIgnore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := state
			if tc.state != nil {
				start = *tc.state
			}
			action := RouteKey(tc.chunk, start, runtime)
			if action.Kind != tc.want {
				t.Fatalf("action = %q, want %q", action.Kind, tc.want)
			}
			if tc.extra != nil && !tc.extra(action) {
				t.Fatalf("action payload = %#v", action)
			}
		})
	}
}

// withSubmitChoice is a one-field state helper on the Submit tab.
func withSubmitChoice(index int) QuestionnaireState {
	state := stateFor()
	state.CurrentTab = 2
	state.SubmitChoiceIndex = index
	return state
}

// statePtr returns a pointer to a state value.
func statePtr(state QuestionnaireState) *QuestionnaireState { return &state }

// ── tab switching ────────────────────────────────────────────────────────

func TestRouteKeyTabSwitch(t *testing.T) {
	params := multiParams()
	runtime := runtimeFor(params, 0, 0)

	for _, chunk := range []string{"\t", "\x1b[C"} {
		action := RouteKey(chunk, stateFor(), runtime)
		if action.Kind != ActionTabSwitch || action.NextTab != 1 {
			t.Fatalf("chunk %q → %#v", chunk, action)
		}
	}
	for _, chunk := range []string{"\x1b[Z", "\x1b[D"} {
		action := RouteKey(chunk, stateFor(), runtime)
		if action.Kind != ActionTabSwitch || action.NextTab != 2 {
			t.Fatalf("chunk %q → %#v (should wrap to the submit tab)", chunk, action)
		}
	}
}

// A single-question questionnaire has no tabs to switch.
func TestRouteKeyNoTabSwitchForSingleQuestion(t *testing.T) {
	runtime := runtimeFor(singleParams(), 0, 0)
	if action := RouteKey("\t", stateFor(), runtime); action.Kind == ActionTabSwitch {
		t.Fatal("single-question questionnaire switched tabs")
	}
}

// ── notes key ───────────────────────────────────────────────────────────

func TestRouteKeyNotesKey(t *testing.T) {
	params := singleParams()
	runtime := runtimeFor(params, 0, 0)
	if action := RouteKey("n", stateFor(), runtime); action.Kind != ActionNotesEnter {
		t.Fatalf("n → %q", action.Kind)
	}

	// A configured single-character keys opens the editor too.
	custom := runtime
	custom.NotesKey = "т"
	if action := RouteKey("т", stateFor(), custom); action.Kind != ActionNotesEnter {
		t.Fatalf("т → %q", action.Kind)
	}

	// A configured combo spec matches through the key matcher.
	combo := runtime
	combo.NotesKey = "ctrl+n"
	if action := RouteKey("\x0e", stateFor(), combo); action.Kind != ActionNotesEnter {
		t.Fatalf("ctrl+n → %q", action.Kind)
	}

	// An empty notes key falls back to the default `n`.
	stale := runtime
	stale.NotesKey = ""
	if action := RouteKey("n", stateFor(), stale); action.Kind != ActionNotesEnter {
		t.Fatalf("empty notes key → %q", action.Kind)
	}
}

// The notes gate sits above the multi-select toggle block: `n` neither toggles
// nor is blocked by the Next row's toggle suppression.
func TestRouteKeyNotesKeyAboveMultiSelect(t *testing.T) {
	params := multiParams()
	runtime := runtimeFor(params, 1, 4) // the Next row, which blocks toggle
	if action := RouteKey("n", stateFor(), runtime); action.Kind != ActionNotesEnter {
		t.Fatalf("n on the Next row → %q", action.Kind)
	}
}

// ── navigation ───────────────────────────────────────────────────────────

func TestRouteKeyUpDownNavigation(t *testing.T) {
	params := singleParams() // A, B, Type something.
	runtime := runtimeFor(params, 0, 0)

	down := RouteKey("\x1b[B", stateFor(), runtime)
	if down.Kind != ActionNav || down.NextIndex != 1 {
		t.Fatalf("down → %#v", down)
	}
	state := stateFor()
	state.OptionIndex = 1
	up := RouteKey("\x1b[A", state, runtime)
	if up.Kind != ActionNav || up.NextIndex != 0 {
		t.Fatalf("up → %#v", up)
	}
	// Wrapping at both ends.
	wrapped := RouteKey("\x1b[B", stateWithIndex(2), runtime)
	if wrapped.NextIndex != 0 {
		t.Fatalf("down at last row → %d", wrapped.NextIndex)
	}
	back := RouteKey("\x1b[A", stateFor(), runtime)
	if back.NextIndex != 2 {
		t.Fatalf("up at first row → %d", back.NextIndex)
	}
}

func stateWithIndex(index int) QuestionnaireState {
	state := stateFor()
	state.OptionIndex = index
	return state
}

// ── single-select tab ───────────────────────────────────────────────────

func TestRouteKeySingleSelectTab(t *testing.T) {
	params := singleParams()
	runtime := runtimeFor(params, 0, 0)

	t.Run("enter confirms the focused option", func(t *testing.T) {
		action := RouteKey("\r", stateFor(), runtime)
		if action.Kind != ActionConfirm || action.Answer == nil || *action.Answer.Answer != "A" {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("esc cancels", func(t *testing.T) {
		if action := RouteKey("\x1b", stateFor(), runtime); action.Kind != ActionCancel {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("digit answers its row", func(t *testing.T) {
		action := RouteKey("2", stateFor(), runtime)
		if action.Kind != ActionConfirm || action.Answer == nil || *action.Answer.Answer != "B" {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("the custom row's digit focuses it instead", func(t *testing.T) {
		action := RouteKey("3", stateFor(), runtime)
		if action.Kind != ActionNav || action.NextIndex != 2 {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("an out-of-range digit is ignored", func(t *testing.T) {
		if action := RouteKey("9", stateFor(), runtime); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("a pasted multi-digit string matches no digit", func(t *testing.T) {
		if action := RouteKey("12", stateFor(), runtime); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("enter on the custom row is ignored", func(t *testing.T) {
		atCustom := runtimeFor(singleParams(), 0, 2)
		if action := RouteKey("\r", stateWithIndex(2), atCustom); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("unknown keys are ignored", func(t *testing.T) {
		if action := RouteKey("x", stateFor(), runtime); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})
}

// ── multi-select tab ─────────────────────────────────────────────────────

func TestRouteKeyMultiSelectTab(t *testing.T) {
	params := multiParams()
	runtime := runtimeFor(params, 1, 0) // first option of the 3-option question
	state := stateFor()
	state.CurrentTab = 1

	t.Run("space toggles the focused row", func(t *testing.T) {
		action := RouteKey(" ", state, runtime)
		if action.Kind != ActionToggle || action.Index != 0 {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("enter toggles a regular row", func(t *testing.T) {
		action := RouteKey("\r", state, runtime)
		if action.Kind != ActionToggle || action.Index != 0 {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("space on the custom row is suppressed", func(t *testing.T) {
		atCustom := runtimeFor(params, 1, 3)
		if action := RouteKey(" ", state, atCustom); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("enter on the custom row is suppressed", func(t *testing.T) {
		atCustom := runtimeFor(params, 1, 3)
		if action := RouteKey("\r", state, atCustom); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("space on the Next row is suppressed", func(t *testing.T) {
		atNext := runtimeFor(params, 1, 4)
		if action := RouteKey(" ", state, atNext); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("enter on the Next row commits", func(t *testing.T) {
		atNext := runtimeFor(params, 1, 4)
		atNext.InputBuffer = "extra"
		action := RouteKey("\r", state, atNext)
		if action.Kind != ActionMultiConfirm {
			t.Fatalf("action = %q", action.Kind)
		}
		if action.Value != "extra" {
			t.Fatalf("value = %q", action.Value)
		}
	})

	t.Run("esc cancels", func(t *testing.T) {
		if action := RouteKey("\x1b", state, runtime); action.Kind != ActionCancel {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("a digit toggles its row", func(t *testing.T) {
		action := RouteKey("2", state, runtime)
		if action.Kind != ActionToggle || action.Index != 1 {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("the custom row's digit focuses it", func(t *testing.T) {
		action := RouteKey("4", state, runtime)
		if action.Kind != ActionNav || action.NextIndex != 3 {
			t.Fatalf("action = %#v", action)
		}
	})

	t.Run("the Next row has no digit", func(t *testing.T) {
		if action := RouteKey("5", state, runtime); action.Kind != ActionIgnore {
			t.Fatalf("action = %q", action.Kind)
		}
	})

	t.Run("up/down still navigate", func(t *testing.T) {
		if action := RouteKey("\x1b[B", state, runtime); action.Kind != ActionNav {
			t.Fatalf("action = %q", action.Kind)
		}
	})
}

// ── empty / defensive ────────────────────────────────────────────────────

// A runtime with no items still routes without panicking.
func TestRouteKeyEmptyItems(t *testing.T) {
	runtime := QuestionnaireRuntime{
		Keybindings: defaultBindings(),
		Questions:   []QuestionData{twoOptions("H", "Q?")},
		CollapseKey: DefaultCollapseKey,
		NotesKey:    DefaultNotesKey,
	}
	if action := RouteKey("\x1b[B", stateFor(), runtime); action.Kind != ActionNav {
		t.Fatalf("action = %q", action.Kind)
	}
	if action := RouteKey("1", stateFor(), runtime); action.Kind != ActionIgnore {
		t.Fatalf("action = %q", action.Kind)
	}
}

// A current tab outside the question range is ignored rather than panicking.
func TestRouteKeyOutOfRangeTab(t *testing.T) {
	params := singleParams()
	runtime := runtimeFor(params, 0, 0)
	state := stateFor()
	state.CurrentTab = 5
	if action := RouteKey("\r", state, runtime); action.Kind != ActionIgnore {
		t.Fatalf("action = %q", action.Kind)
	}
}
