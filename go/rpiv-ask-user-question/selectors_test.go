package rpivaskuserquestion

import (
	"testing"
)

// ── selectors ─────────────────────────────────────────────────────────────

func TestSelectActiveView(t *testing.T) {
	cases := []struct {
		name          string
		state         QuestionnaireState
		totalQuestion int
		want          ActiveView
	}{
		{"notes wins over submit", QuestionnaireState{NotesVisible: true, CurrentTab: 1}, 1, ActiveViewNotes},
		{"submit tab", QuestionnaireState{CurrentTab: 1}, 1, ActiveViewSubmit},
		{"options", QuestionnaireState{CurrentTab: 0}, 2, ActiveViewOptions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectActiveView(tc.state, tc.totalQuestion); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectActivePreviewPaneIndex(t *testing.T) {
	cases := []struct{ tab, total, want int }{
		{0, 3, 0}, {1, 3, 1}, {2, 3, 2}, {3, 3, 2}, {9, 3, 2}, {0, 0, 0}, {-1, 2, 0}, {5, 1, 0},
	}
	for _, tc := range cases {
		if got := SelectActivePreviewPaneIndex(tc.tab, tc.total); got != tc.want {
			t.Fatalf("SelectActivePreviewPaneIndex(%d,%d) = %d, want %d", tc.tab, tc.total, got, tc.want)
		}
	}
}

func TestSelectConfirmedIndicator(t *testing.T) {
	single := twoOptions("H", "Q?")
	items := BuildItemsForQuestion(single) // A, B, other

	t.Run("option answer marks its row", func(t *testing.T) {
		answers := map[int]QuestionAnswer{0: {Kind: AnswerKindOption, Answer: strPtr("B")}}
		got := SelectConfirmedIndicator([]QuestionData{single}, 0, answers, items)
		if got == nil || got.Index != 1 || got.LabelOverride != nil {
			t.Fatalf("indicator = %#v", got)
		}
	})

	t.Run("custom answer marks the other row with an override", func(t *testing.T) {
		answers := map[int]QuestionAnswer{0: {Kind: AnswerKindCustom, Answer: strPtr("typed")}}
		got := SelectConfirmedIndicator([]QuestionData{single}, 0, answers, items)
		if got == nil || got.Index != 2 || got.LabelOverride == nil || *got.LabelOverride != "typed" {
			t.Fatalf("indicator = %#v", got)
		}
	})

	t.Run("no answer", func(t *testing.T) {
		if got := SelectConfirmedIndicator([]QuestionData{single}, 0, map[int]QuestionAnswer{}, items); got != nil {
			t.Fatalf("indicator = %#v", got)
		}
	})

	t.Run("multi-select owns its own boxes", func(t *testing.T) {
		multi := multiQuestion("Q?", "A", "B")
		answers := map[int]QuestionAnswer{0: {Kind: AnswerKindMulti, Selected: []string{"A"}}}
		if got := SelectConfirmedIndicator([]QuestionData{multi}, 0, answers, BuildItemsForQuestion(multi)); got != nil {
			t.Fatalf("indicator = %#v", got)
		}
	})

	t.Run("unknown option label", func(t *testing.T) {
		answers := map[int]QuestionAnswer{0: {Kind: AnswerKindOption, Answer: strPtr("gone")}}
		if got := SelectConfirmedIndicator([]QuestionData{single}, 0, answers, items); got != nil {
			t.Fatalf("indicator = %#v", got)
		}
	})

	t.Run("custom answer with no other row", func(t *testing.T) {
		answers := map[int]QuestionAnswer{0: {Kind: AnswerKindCustom, Answer: strPtr("x")}}
		optionOnly := []WrappingSelectItem{{Kind: RowKindOption, Label: "A"}}
		if got := SelectConfirmedIndicator([]QuestionData{single}, 0, answers, optionOnly); got != nil {
			t.Fatalf("indicator = %#v", got)
		}
	})

	t.Run("out-of-range tab", func(t *testing.T) {
		if got := SelectConfirmedIndicator([]QuestionData{single}, 4, map[int]QuestionAnswer{}, items); got != nil {
			t.Fatalf("indicator = %#v", got)
		}
	})
}

func TestSelectSavedNote(t *testing.T) {
	t.Run("side-band wins", func(t *testing.T) {
		notes := map[int]string{0: "side-band"}
		answers := map[int]QuestionAnswer{0: {Notes: strPtr("mirror")}}
		got := SelectSavedNote(notes, answers, 0)
		if got == nil || *got != "side-band" {
			t.Fatalf("note = %v", got)
		}
	})

	t.Run("mirror used when the side-band has no entry", func(t *testing.T) {
		answers := map[int]QuestionAnswer{0: {Notes: strPtr("mirror")}}
		got := SelectSavedNote(map[int]string{}, answers, 0)
		if got == nil || *got != "mirror" {
			t.Fatalf("note = %v", got)
		}
	})

	t.Run("a present empty string means cleared", func(t *testing.T) {
		notes := map[int]string{0: ""}
		answers := map[int]QuestionAnswer{0: {Notes: strPtr("mirror")}}
		if got := SelectSavedNote(notes, answers, 0); got != nil {
			t.Fatalf("note = %v", got)
		}
	})

	t.Run("no note", func(t *testing.T) {
		if got := SelectSavedNote(map[int]string{}, map[int]QuestionAnswer{}, 0); got != nil {
			t.Fatalf("note = %v", got)
		}
	})
}

// ── projections ───────────────────────────────────────────────────────────

func bindingContextFor(params QuestionParams) BindingContext {
	return BindingContext{
		Questions:      params.Questions,
		ItemsByTab:     BuildItemsByTab(params),
		TotalQuestions: len(params.Questions),
		ActiveView:     ActiveViewOptions,
		InputBuffer:    "buffer",
	}
}

func perTabContext(ctx BindingContext, index int) PerTabBindingContext {
	return PerTabBindingContext{BindingContext: ctx, I: index, Tab: &TabComponents{}}
}

func TestSelectMultiSelectProps(t *testing.T) {
	params := multiParams()
	ctx := bindingContextFor(params)
	state := stateFor()
	state.CurrentTab = 1
	state.OptionIndex = 1
	state.MultiSelectChecked = map[int]struct{}{0: {}, 2: {}}

	props := SelectMultiSelectProps(state, perTabContext(ctx, 1))
	if len(props.Rows) != 3 {
		t.Fatalf("rows = %d", len(props.Rows))
	}
	if !props.Rows[0].Checked || props.Rows[1].Checked || !props.Rows[2].Checked {
		t.Fatalf("checked = %#v", props.Rows)
	}
	if props.Rows[1].Active != true {
		t.Fatalf("row 1 active = %v", props.Rows[1].Active)
	}
	if props.NextActive {
		t.Fatal("Next row active while on an option")
	}
	if props.NextLabel != MultiSubmitLabel {
		t.Fatalf("next label = %q (last question ⇒ Submit)", props.NextLabel)
	}
}

func TestSelectMultiSelectPropsOtherAndNext(t *testing.T) {
	params := multiParams()
	ctx := bindingContextFor(params)

	// Focus the custom row.
	state := stateFor()
	state.CurrentTab = 1
	state.OptionIndex = 3
	state.InputMode = true
	props := SelectMultiSelectProps(state, perTabContext(ctx, 1))
	if !props.Other.Active || !props.Other.InputMode {
		t.Fatalf("other = %#v", props.Other)
	}
	if !props.Other.Checked {
		t.Fatal("non-empty buffer should mark the row checked")
	}

	// Focus the Next row.
	state.OptionIndex = 4
	props = SelectMultiSelectProps(state, perTabContext(ctx, 1))
	if !props.NextActive {
		t.Fatal("Next row not active")
	}
	if props.Other.Active {
		t.Fatal("custom row active while on Next")
	}

	// A blank buffer is unchecked.
	ctx.InputBuffer = "   "
	props = SelectMultiSelectProps(state, perTabContext(ctx, 1))
	if props.Other.Checked {
		t.Fatal("blank buffer marked checked")
	}
}

func TestSelectMultiSelectPropsEmptyAndIntermediate(t *testing.T) {
	params := multiParams()
	ctx := bindingContextFor(params)

	// Out-of-range tab yields the empty projection.
	props := SelectMultiSelectProps(stateFor(), perTabContext(ctx, 9))
	if len(props.Rows) != 0 || props.NextLabel != SentinelNextLabel {
		t.Fatalf("empty props = %#v", props)
	}

	// The first question's Next row reads "Next" (not the last question).
	props = SelectMultiSelectProps(stateFor(), perTabContext(ctx, 0))
	if props.NextLabel != SentinelNextLabel {
		t.Fatalf("first-question next label = %q", props.NextLabel)
	}

	// A non-options focus hides every active flag.
	ctx.ActiveView = ActiveViewSubmit
	state := stateFor()
	state.CurrentTab = 1
	state.OptionIndex = 0
	props = SelectMultiSelectProps(state, perTabContext(ctx, 1))
	if props.Rows[0].Active || props.Other.Active || props.NextActive {
		t.Fatalf("focus leaked into props = %#v", props)
	}
}

func TestSelectOptionListProps(t *testing.T) {
	params := singleParams()
	ctx := bindingContextFor(params)
	state := stateFor()
	state.OptionIndex = 1
	state.Answers = map[int]QuestionAnswer{0: {Kind: AnswerKindOption, Answer: strPtr("A")}}

	props := SelectOptionListProps(state, perTabContext(ctx, 0))
	if props.SelectedIndex != 1 || !props.Focused {
		t.Fatalf("props = %#v", props)
	}
	if props.Confirmed == nil || props.Confirmed.Index != 0 {
		t.Fatalf("confirmed = %#v", props.Confirmed)
	}
	if props.InputBuffer != "buffer" {
		t.Fatalf("inputBuffer = %q", props.InputBuffer)
	}
}

func TestSelectSubmitPickerProps(t *testing.T) {
	params := multiParams()
	ctx := bindingContextFor(params)
	ctx.ActiveView = ActiveViewSubmit

	state := stateFor()
	state.SubmitChoiceIndex = 1
	props := SelectSubmitPickerProps(state, ctx)
	if props.Rows[0].Active || !props.Rows[1].Active {
		t.Fatalf("rows = %#v", props.Rows)
	}

	ctx.ActiveView = ActiveViewOptions
	props = SelectSubmitPickerProps(state, ctx)
	if props.Rows[0].Active || props.Rows[1].Active {
		t.Fatal("picker rows active without submit focus")
	}
}

func TestSelectPreviewPaneProps(t *testing.T) {
	params := singleParams()
	ctx := bindingContextFor(params)
	state := stateFor()
	state.OptionIndex = 1
	state.NotesByTab = map[int]string{0: "a note"}

	props := SelectPreviewPaneProps(state, perTabContext(ctx, 0))
	if !props.HasNote || props.SelectedIndex != 1 || !props.Focused || props.NotesVisible {
		t.Fatalf("props = %#v", props)
	}

	state.InputMode = true
	props = SelectPreviewPaneProps(state, perTabContext(ctx, 0))
	if !props.InputMode {
		t.Fatal("inputMode not projected")
	}

	// A different tab has no note of its own.
	props = SelectPreviewPaneProps(state, perTabContext(ctx, 1))
	if props.HasNote {
		t.Fatal("note leaked to another tab")
	}
}

func TestSelectTabBarProps(t *testing.T) {
	params := multiParams()
	ctx := bindingContextFor(params)
	state := stateFor()
	state.Answers = map[int]QuestionAnswer{0: {Kind: AnswerKindOption, Answer: strPtr("A")}}

	props := SelectTabBarProps(state, ctx)
	if len(props.Tabs) != 2 {
		t.Fatalf("tabs = %d", len(props.Tabs))
	}
	if props.Tabs[0].Label != "Auth" || !props.Tabs[0].Answered || !props.Tabs[0].Active {
		t.Fatalf("tab 0 = %#v", props.Tabs[0])
	}
	if props.Tabs[1].Answered {
		t.Fatalf("tab 1 = %#v", props.Tabs[1])
	}
	if props.Submit.Active || props.Submit.AllAnswered {
		t.Fatalf("submit = %#v", props.Submit)
	}

	// A headerless question falls back to Q<n>.
	noHeader := QuestionParams{Questions: []QuestionData{twoOptions("", "Q?")}}
	ctx = bindingContextFor(noHeader)
	props = SelectTabBarProps(stateFor(), ctx)
	if props.Tabs[0].Label != "Q1" {
		t.Fatalf("label = %q", props.Tabs[0].Label)
	}

	// All answers present marks the submit tab complete.
	state.Answers[1] = QuestionAnswer{Kind: AnswerKindMulti}
	state.CurrentTab = 2
	props = SelectTabBarProps(state, bindingContextFor(params))
	if !props.Submit.AllAnswered || !props.Submit.Active {
		t.Fatalf("submit = %#v", props.Submit)
	}
}

func TestSelectDialogProps(t *testing.T) {
	params := singleParams()
	ctx := bindingContextFor(params)
	state := stateFor()
	props := SelectDialogProps(state, ctx)
	if props.State.CurrentTab != 0 || props.ActivePreviewPane != nil {
		t.Fatalf("props = %#v", props)
	}
}

func TestIsActiveTab(t *testing.T) {
	params := multiParams()
	ctx := bindingContextFor(params)
	state := stateFor()
	state.CurrentTab = 1

	if IsActiveTab(state, perTabContext(ctx, 1)) != true {
		t.Fatal("current tab not active")
	}
	if IsActiveTab(state, perTabContext(ctx, 0)) != false {
		t.Fatal("inactive tab reported active")
	}
	// On the Submit tab the last question's pane is still the active one (it is
	// reused for layout).
	state.CurrentTab = 2
	if !IsActiveTab(state, perTabContext(ctx, 1)) {
		t.Fatal("submit tab should reuse the last pane")
	}
}
