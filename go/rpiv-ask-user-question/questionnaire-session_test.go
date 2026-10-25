package rpivaskuserquestion

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── session fixtures ──────────────────────────────────────────────────────

// sessionConfig builds a session over params with a deterministic terminal.
func sessionConfig(params QuestionParams) QuestionnaireSessionConfig {
	return QuestionnaireSessionConfig{
		Theme:        themeForTests(),
		Params:       params,
		ItemsByTab:   BuildItemsByTab(params),
		Keybindings:  defaultBindings(),
		CollapseKey:  DefaultCollapseKey,
		NotesKey:     DefaultNotesKey,
		RowBudget:    FullRowBudget,
		TerminalSize: func() (int, int) { return 120, 40 },
	}
}

// newSession builds a session and records the result it settles with.
func newSession(t *testing.T, params QuestionParams) (*QuestionnaireSession, *[]QuestionnaireResult) {
	t.Helper()
	config := sessionConfig(params)
	results := &[]QuestionnaireResult{}
	config.Done = func(result QuestionnaireResult) { *results = append(*results, result) }
	return NewQuestionnaireSession(config), results
}

func TestSessionInitialRender(t *testing.T) {
	session, _ := newSession(t, singleParams())
	lines := session.Render(80)
	if len(lines) == 0 {
		t.Fatal("no rows rendered")
	}
	text := renderStrings(lines)
	if !strings.Contains(text, "Which auth?") {
		t.Fatalf("question missing:\n%s", text)
	}
	if !strings.Contains(text, "A") || !strings.Contains(text, "B") {
		t.Fatalf("options missing:\n%s", text)
	}
	// The footer names the resolved keys. The line is clipped at the terminal
	// width, so assert on the parts that survive rather than the collapse tail.
	if !strings.Contains(text, "Enter to select") {
		t.Fatalf("footer missing:\n%s", text)
	}
	if !strings.Contains(text, "n to add notes") {
		t.Fatalf("notes hint missing:\n%s", text)
	}
}

// Confirm on an option settles the session with an option answer.
func TestSessionConfirmSingleSelect(t *testing.T) {
	session, results := newSession(t, singleParams())
	component, err := session.HandleInput("\r")
	if err != nil {
		t.Fatalf("HandleInput: %v", err)
	}
	if !component.Done {
		t.Fatal("session did not finish on confirm")
	}
	if len(*results) != 1 {
		t.Fatalf("settlements = %d", len(*results))
	}
	answer := (*results)[0].Answers[0]
	if answer.Kind != AnswerKindOption || *answer.Answer != "A" {
		t.Fatalf("answer = %#v", answer)
	}
}

// Esc settles with a cancelled result.
func TestSessionCancel(t *testing.T) {
	session, results := newSession(t, singleParams())
	component, _ := session.HandleInput("\x1b")
	if !component.Done {
		t.Fatal("session did not finish on cancel")
	}
	if !(*results)[0].Cancelled {
		t.Fatalf("result = %#v", (*results)[0])
	}
}

// Typing on the custom row collects a draft, and Enter commits it.
func TestSessionCustomAnswer(t *testing.T) {
	session, results := newSession(t, singleParams())

	// Down twice focuses the Type-something. row.
	session.HandleInput("\x1b[B")
	session.HandleInput("\x1b[B")
	for _, r := range "custom text" {
		session.HandleInput(string(r))
	}
	// The draft is visible in the inline editor.
	if text := renderStrings(session.Render(80)); !strings.Contains(text, "custom text") {
		t.Fatalf("draft not visible:\n%s", text)
	}

	component, _ := session.HandleInput("\r")
	if !component.Done {
		t.Fatal("session did not finish")
	}
	answer := (*results)[0].Answers[0]
	if answer.Kind != AnswerKindCustom || *answer.Answer != "custom text" {
		t.Fatalf("answer = %#v", answer)
	}
}

// Navigation down wraps through the option rows and back to the first.
func TestSessionNavigationWraps(t *testing.T) {
	session, _ := newSession(t, singleParams())

	// A, B, Type something. → three downs returns to A.
	for i := 0; i < 3; i++ {
		session.HandleInput("\x1b[B")
	}
	text := renderStrings(session.Render(80))
	if !strings.Contains(text, "❯ 1. A") {
		t.Fatalf("wrap did not return to the first row:\n%s", text)
	}
}

// Digit hotkeys answer directly.
func TestSessionDigitHotkey(t *testing.T) {
	session, results := newSession(t, singleParams())
	component, _ := session.HandleInput("2")
	if !component.Done {
		t.Fatal("digit did not confirm")
	}
	if answer := (*results)[0].Answers[0]; *answer.Answer != "B" {
		t.Fatalf("answer = %#v", answer)
	}
}

// The notes editor opens with `n`, the keystrokes are forwarded, and Enter
// commits the trimmed note.
func TestSessionNotesFlow(t *testing.T) {
	session, results := newSession(t, singleParams())

	session.HandleInput("n")
	text := renderStrings(session.Render(80))
	if !strings.Contains(text, "Notes:") {
		t.Fatalf("notes header missing:\n%s", text)
	}
	for _, r := range "a note" {
		session.HandleInput(string(r))
	}
	// Enter closes the editor (and does not submit the questionnaire).
	for _, r := range "" {
		_ = r
	}
	component, _ := session.HandleInput("\r")
	if component.Done {
		t.Fatal("closing the notes editor finished the questionnaire")
	}
	// The saved note renders under the options.
	text = renderStrings(session.Render(80))
	if !strings.Contains(text, "Note for this answer: a note") {
		t.Fatalf("saved note missing:\n%s", text)
	}

	// Confirming now carries the note onto the answer.
	component, _ = session.HandleInput("\r")
	if !component.Done {
		t.Fatal("confirm did not finish")
	}
	answer := (*results)[0].Answers[0]
	if answer.Notes == nil || *answer.Notes != "a note" {
		t.Fatalf("answer = %#v", answer)
	}
}

// The global note lives on the Submit tab and lifts onto the result.
func TestSessionGlobalNote(t *testing.T) {
	session, results := newSession(t, multiParams())

	// Answer the first question (Enter on option A advances to question 2).
	session.HandleInput("\r")
	// Question 2 is multi-select: check A, walk to the Next row, commit there.
	session.HandleInput(" ")
	// Rows: A(0) B(1) C(2) Type-something.(3) Next(4).
	for i := 0; i < 4; i++ {
		session.HandleInput("\x1b[B")
	}
	session.HandleInput("\r") // confirm on Next → advances to the Submit tab

	text := renderStrings(session.Render(80))
	if !strings.Contains(text, "Review your answers") {
		t.Fatalf("review heading missing:\n%s", text)
	}

	// The Submit tab's `n` opens the global-note editor.
	session.HandleInput("n")
	for _, r := range "global" {
		session.HandleInput(string(r))
	}
	session.HandleInput("\r") // close the notes editor

	text = renderStrings(session.Render(80))
	if !strings.Contains(text, "Note") || !strings.Contains(text, "global") {
		t.Fatalf("global note entry missing:\n%s", text)
	}

	component, _ := session.HandleInput("\r") // submit (row 0 is focused)
	if !component.Done {
		t.Fatal("submit did not finish")
	}
	result := (*results)[0]
	if result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
	if result.GlobalNote == nil || *result.GlobalNote != "global" {
		t.Fatalf("globalNote = %v", result.GlobalNote)
	}
}

// Collapsing swaps the dialog for a single hint row, and collapsing again
// restores it.
func TestSessionCollapse(t *testing.T) {
	session, _ := newSession(t, singleParams())

	session.HandleInput("\x1d") // Ctrl+]
	collapsed := session.Render(80)
	if len(collapsed) != 1 {
		t.Fatalf("collapsed rows = %d", len(collapsed))
	}
	text := renderStrings(collapsed)
	if !strings.Contains(text, "to expand") || !strings.Contains(text, "Esc to cancel") {
		t.Fatalf("collapsed hint = %q", text)
	}

	session.HandleInput("\x1d")
	if rows := session.Render(80); len(rows) < 3 {
		t.Fatalf("expanded rows = %d", len(rows))
	}
}

// The collapsed row is dimmed, like the TS buildCollapsedRender's theme.fg("dim", …).
// A plain row sits in the editor slot at full brightness, indistinguishable from a
// live dialog row.
func TestSessionCollapsedRowIsDimmed(t *testing.T) {
	config := sessionConfig(singleParams())
	config.Theme = paletteTheme(t, map[string]string{"dim": "\x1b[2m"})
	session := NewQuestionnaireSession(config)
	session.ToggleCollapsedExternal()

	lines := session.Render(80)
	if len(lines) != 1 {
		t.Fatalf("collapsed rows = %d", len(lines))
	}
	// A faint token closes with SGR 22;39 (the SDK's faint handling).
	want := "\x1b[2m " + collapsedHintLine(DefaultCollapseKey) + " \x1b[22;39m"
	if lines[0] != want {
		t.Fatalf("collapsed row = %q, want %q", lines[0], want)
	}
}

// With the collapse key off, the collapsed line names the cancel key instead.
func TestSessionCollapsedLineWithCollapseOff(t *testing.T) {
	config := sessionConfig(singleParams())
	config.CollapseKey = CollapseKeyOff
	session := NewQuestionnaireSession(config)
	session.ToggleCollapsedExternal()
	text := renderStrings(session.Render(80))
	if !strings.Contains(text, "Esc to cancel") || strings.Contains(text, "expand") {
		t.Fatalf("collapsed line = %q", text)
	}
}

// The set_overlay_hidden effect only fires when the host can reopen the overlay.
func TestSessionOverlayHideGatedByListener(t *testing.T) {
	var hiddenCalls []bool
	config := sessionConfig(singleParams())
	config.CanReopenWhileHidden = false
	config.SetOverlayHidden = func(hidden bool) { hiddenCalls = append(hiddenCalls, hidden) }
	session := NewQuestionnaireSession(config)
	session.ToggleCollapsedExternal()
	if len(hiddenCalls) != 0 {
		t.Fatalf("SetHidden called without a raw listener: %v", hiddenCalls)
	}

	hiddenCalls = nil
	config = sessionConfig(singleParams())
	config.CanReopenWhileHidden = true
	config.SetOverlayHidden = func(hidden bool) { hiddenCalls = append(hiddenCalls, hidden) }
	session = NewQuestionnaireSession(config)
	session.ToggleCollapsedExternal()
	if len(hiddenCalls) != 1 || !hiddenCalls[0] {
		t.Fatalf("SetHidden calls = %v", hiddenCalls)
	}
}

// Repaint callbacks must never run while the session holds s.mu. commit and
// handleIgnoreInline used to project through viewAdapter.Apply with the lock held,
// and Apply ends by invoking the session's repaint callback — which re-enters a
// locking entry point of the very same session on the same goroutine (the mount
// passed RequestRender: requestRepaint, and requestRepaint calls back into
// QuestionnaireSession.RequestRender; and an SDK that renders synchronously inside
// its invalidate callback, like remoteOverlay.render, reaches Render). Re-locking a
// mutex the current goroutine already owns is a self-deadlock, so both paths now
// project under the lock and ask for the repaint after releasing it.
func TestSessionRepaintCallbacksRunOutsideTheSessionLock(t *testing.T) {
	var mu sync.Mutex
	var mounted *QuestionnaireSession
	var repaints int32
	// repaint re-enters the session the way a host repaint does: Render locks s.mu.
	// The session is resolved through a mutex-guarded pointer, exactly as the mount
	// resolves the one it has not published yet.
	repaint := func() {
		mu.Lock()
		current := mounted
		mu.Unlock()
		if current == nil {
			return
		}
		atomic.AddInt32(&repaints, 1)
		if len(current.Render(80)) == 0 {
			t.Error("repaint painted no rows")
		}
	}

	config := sessionConfig(singleParams())
	// The mount's repaint seam, and the callback the SDK installs while the overlay
	// is active — here both paint synchronously, the way the SDK's render loop does.
	config.RequestRender = repaint
	session := NewQuestionnaireSession(config)
	mu.Lock()
	mounted = session
	mu.Unlock()
	session.SetInvalidate(repaint)

	done := make(chan struct{})
	go func() {
		defer close(done)
		session.HandleInput("\x1b[B") // nav → commit
		session.HandleInput("\x1b[B") // the Type-something. row → commit
		for _, r := range "abc" {     // ActionIgnore → handleIgnoreInline
			session.HandleInput(string(r))
		}
		session.ToggleCollapsedExternal() // collapse → commit
		session.ToggleCollapsedExternal() // expand → commit
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: a repaint callback re-entered the session while it held s.mu")
	}
	if atomic.LoadInt32(&repaints) == 0 {
		t.Fatal("the input sequence never reached a repaint")
	}
	if text := renderStrings(session.Render(80)); !strings.Contains(text, "abc") {
		t.Fatalf("inline draft lost:\n%s", text)
	}
}

// The external-editor effect commits the edited buffer on success and keeps the
// draft on failure.
func TestSessionExternalEditorEffect(t *testing.T) {
	t.Run("success replaces the buffer", func(t *testing.T) {
		config := sessionConfig(singleParams())
		config.EditInput = func(value string) (string, bool) { return "edited text", true }
		results := &[]QuestionnaireResult{}
		config.Done = func(result QuestionnaireResult) { *results = append(*results, result) }
		session := NewQuestionnaireSession(config)

		session.HandleInput("\x1b[B")
		session.HandleInput("\x1b[B")
		session.HandleInput("\x1b[103;5u") // app.editor.external
		if text := renderStrings(session.Render(80)); !strings.Contains(text, "edited text") {
			t.Fatalf("edited text not committed:\n%s", text)
		}
	})

	t.Run("failure keeps the draft", func(t *testing.T) {
		config := sessionConfig(singleParams())
		config.EditInput = func(value string) (string, bool) { return "", false }
		session := NewQuestionnaireSession(config)

		session.HandleInput("\x1b[B")
		session.HandleInput("\x1b[B")
		session.HandleInput("k")
		session.HandleInput("\x1b[103;5u")
		if text := renderStrings(session.Render(80)); !strings.Contains(text, "k") {
			t.Fatalf("draft lost:\n%s", text)
		}
	})
}

// Input is ignored while the external editor owns the terminal.
func TestSessionInputIgnoredWhileEditorOpen(t *testing.T) {
	release := make(chan struct{})
	config := sessionConfig(singleParams())
	config.EditInput = func(value string) (string, bool) {
		<-release
		return "", false
	}
	session := NewQuestionnaireSession(config)
	session.HandleInput("\x1b[B")
	session.HandleInput("\x1b[B")
	go session.HandleInput("\x1b[103;5u")
	// A second keystroke while the editor is open is dropped.
	component, _ := session.HandleInput("\r")
	if component.Done {
		t.Fatal("input processed while the editor was open")
	}
	close(release)
}

// The multi-question session renders the tab bar and reuses the last pane on the
// Submit tab.
func TestSessionMultiQuestionChrome(t *testing.T) {
	session, _ := newSession(t, multiParams())
	text := renderStrings(session.Render(120))
	if !strings.Contains(text, "←") || !strings.Contains(text, "→") {
		t.Fatalf("tab bar missing:\n%s", text)
	}
	if !strings.Contains(text, "Auth") || !strings.Contains(text, "Features") {
		t.Fatalf("tab labels missing:\n%s", text)
	}
	if !strings.Contains(text, "Tab to switch questions") {
		t.Fatalf("multi hint missing:\n%s", text)
	}
}

// Multi-select: Space toggles and the Next row commits.
func TestSessionMultiSelectFlow(t *testing.T) {
	session, results := newSession(t, QuestionParams{Questions: []QuestionData{multiQuestion("Which?", "A", "B")}})

	session.HandleInput(" ")
	session.HandleInput("\x1b[B")
	session.HandleInput(" ")
	text := renderStrings(session.Render(80))
	if strings.Count(text, "[✔]") != 2 {
		t.Fatalf("checkboxes not toggled:\n%s", text)
	}

	// Reach the Next row (2 options + custom row + Next) and commit.
	for i := 0; i < 2; i++ {
		session.HandleInput("\x1b[B")
	}
	component, _ := session.HandleInput("\r")
	if !component.Done {
		t.Fatal("Next did not commit")
	}
	answer := (*results)[0].Answers[0]
	if len(answer.Selected) != 2 || answer.Selected[0] != "A" || answer.Selected[1] != "B" {
		t.Fatalf("selected = %v", answer.Selected)
	}
}

// A multi-select answer focuses the Next row when returning to the tab.
func TestSessionMultiSelectReentryFocus(t *testing.T) {
	session, _ := newSession(t, multiParams())
	// Answer question 1 (single-select) and advance to question 2.
	session.HandleInput("\r")
	// Question 2 is multi-select: check A, walk to the Next row, commit there.
	session.HandleInput(" ")
	for i := 0; i < 4; i++ {
		session.HandleInput("\x1b[B")
	}
	session.HandleInput("\r") // advances to the Submit tab

	// Switch back to the second question; its committed multi answer focuses Next.
	session.HandleInput("\x1b[Z")
	text := renderStrings(session.Render(80))
	// The last question's Next row reads "Submit" (nextLabelFor), and it is the
	// focused row after re-entry.
	if !strings.Contains(text, "❯ Submit") {
		t.Fatalf("Next row not focused on re-entry:\n%s", text)
	}
}

// Rendering never mutates canonical state and is safe to call repeatedly.
func TestSessionRenderIsIdempotent(t *testing.T) {
	session, _ := newSession(t, singleParams())
	first := renderStrings(session.Render(80))
	second := renderStrings(session.Render(80))
	if first != second {
		t.Fatal("repeated renders differ")
	}
}

// A tiny terminal still renders the chrome without panicking.
func TestSessionTinyTerminal(t *testing.T) {
	config := sessionConfig(singleParams())
	config.TerminalSize = func() (int, int) { return 20, 5 }
	session := NewQuestionnaireSession(config)
	rows := session.Render(20)
	if len(rows) == 0 {
		t.Fatal("no rows on a tiny terminal")
	}
	if len(rows) > 5 {
		t.Fatalf("rows = %d, above the 5-row terminal", len(rows))
	}
}

// The inline row budget caps the dialog on a tall terminal.
func TestSessionInlineBudgetCapsRows(t *testing.T) {
	labels := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		labels = append(labels, "opt"+itoa(i))
	}
	params := QuestionParams{Questions: []QuestionData{question("H", "Q?", labels...)}}
	config := sessionConfig(params)
	config.RowBudget = InlineRowBudget
	config.TerminalSize = func() (int, int) { return 120, 20 }
	session := NewQuestionnaireSession(config)
	rows := session.Render(120)
	if len(rows) > InlineRowBudget(20) {
		t.Fatalf("rows = %d, above the inline budget %d", len(rows), InlineRowBudget(20))
	}
}

// A preview-bearing question paints the side-by-side box.
func TestSessionPreviewLayout(t *testing.T) {
	preview := "# Title\n\nbody text"
	q := twoOptions("H", "Which?")
	q.Options[0].Preview = &preview
	params := QuestionParams{Questions: []QuestionData{q}}

	config := sessionConfig(params)
	config.TerminalSize = func() (int, int) { return 160, 40 }
	session := NewQuestionnaireSession(config)

	text := renderStrings(session.Render(140))
	if !strings.Contains(text, "╭") || !strings.Contains(text, "╰") {
		t.Fatalf("preview box missing:\n%s", text)
	}
	if !strings.Contains(text, "Notes: press n to add notes") {
		t.Fatalf("notes affordance missing:\n%s", text)
	}
}

// ── dialog copy / hints ──────────────────────────────────────────────────

func TestHintPartsAndTemplates(t *testing.T) {
	if HintPartNotes() != "n to add notes" {
		t.Fatalf("notes hint = %q", HintPartNotes())
	}
	if HintPartCollapse() != "Ctrl+] to collapse" {
		t.Fatalf("collapse hint = %q", HintPartCollapse())
	}
	if HintSingle() != "Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel" {
		t.Fatalf("single hint = %q", HintSingle())
	}
	if !strings.Contains(HintMulti(), "Tab to switch questions") {
		t.Fatalf("multi hint = %q", HintMulti())
	}
	if CollapsedHintTemplate() != "{key} to expand · Esc to cancel" {
		t.Fatalf("collapsed template = %q", CollapsedHintTemplate())
	}
}

func TestBuildHintText(t *testing.T) {
	state := stateFor()
	question := twoOptions("H", "Q?")

	base := BuildHintText(&question, false, state, DefaultCollapseKey, DefaultNotesKey)
	if base != "Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel · Ctrl+] to collapse" {
		t.Fatalf("hint = %q", base)
	}

	// Multi-select adds the toggle part before notes.
	multi := multiQuestion("Q?", "A", "B")
	hint := BuildHintText(&multi, false, state, DefaultCollapseKey, DefaultNotesKey)
	if !strings.Contains(hint, "Space to toggle") {
		t.Fatalf("toggle part missing: %q", hint)
	}

	// Multi-question adds the tab part.
	hint = BuildHintText(&question, true, state, DefaultCollapseKey, DefaultNotesKey)
	if !strings.Contains(hint, "Tab to switch questions") {
		t.Fatalf("tab part missing: %q", hint)
	}

	// "off" drops the collapse part entirely.
	hint = BuildHintText(&question, false, state, CollapseKeyOff, DefaultNotesKey)
	if strings.Contains(hint, "collapse") {
		t.Fatalf("collapse part present while off: %q", hint)
	}

	// The notes part drops while the editor or the inline input owns the keyboard.
	notesOpen := state
	notesOpen.NotesVisible = true
	hint = BuildHintText(&question, false, notesOpen, DefaultCollapseKey, DefaultNotesKey)
	if strings.Contains(hint, "add notes") {
		t.Fatalf("notes part present with the editor open: %q", hint)
	}
	if !strings.Contains(hint, "Shift+Enter for newline") {
		t.Fatalf("newline hint missing: %q", hint)
	}

	inputOpen := state
	inputOpen.InputMode = true
	hint = BuildHintText(&question, false, inputOpen, DefaultCollapseKey, DefaultNotesKey)
	if !strings.Contains(hint, "Ctrl+U to clear") {
		t.Fatalf("clear hint missing: %q", hint)
	}

	// A custom notes key is named.
	hint = BuildHintText(&question, false, state, DefaultCollapseKey, "ctrl+n")
	if !strings.Contains(hint, "Ctrl+N to add notes") {
		t.Fatalf("custom notes key not named: %q", hint)
	}

	// No question (defensive) still renders the core.
	if hint := BuildHintText(nil, false, state, DefaultCollapseKey, DefaultNotesKey); !strings.Contains(hint, "Enter to select") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestBuildSubmitHintText(t *testing.T) {
	state := stateFor()
	hint := BuildSubmitHintText(state, DefaultNotesKey)
	if hint != "Enter to select · ↑/↓ to navigate · n to add a note · Esc to cancel" {
		t.Fatalf("hint = %q", hint)
	}

	notesOpen := state
	notesOpen.NotesVisible = true
	hint = BuildSubmitHintText(notesOpen, DefaultNotesKey)
	if strings.Contains(hint, "add a note") {
		t.Fatalf("note part present with the editor open: %q", hint)
	}
	if !strings.Contains(hint, "Shift+Enter for newline") {
		t.Fatalf("newline hint missing: %q", hint)
	}
}

// The body pane follows the active tab: the preview box and its notes affordance
// belong to the question being answered, not to tab 0 (dialog-builder.ts hands the
// strategy `liveProps.activePreviewPane`, read fresh every tick).
func TestSessionPreviewPaneFollowsActiveTab(t *testing.T) {
	preview := "# Gamma preview\n\ngamma body"
	first := twoOptions("First", "First?")
	first.Options[0].Label = "Alpha"
	first.Options[1].Label = "Beta"
	second := question("Second", "Second?", "Gamma", "Delta")
	second.Options[0].Preview = &preview

	config := sessionConfig(QuestionParams{Questions: []QuestionData{first, second}})
	config.TerminalSize = func() (int, int) { return 160, 40 }
	session := NewQuestionnaireSession(config)

	session.HandleInput("\t") // Tab → the second question
	text := renderStrings(session.Render(140))
	if !strings.Contains(text, "Second?") || !strings.Contains(text, "Gamma") {
		t.Fatalf("active question not rendered:\n%s", text)
	}
	if strings.Contains(text, "Alpha") {
		t.Fatalf("tab 0's option list painted on tab 1:\n%s", text)
	}
	if !strings.Contains(text, "╭") || !strings.Contains(text, "Notes: press n to add notes") {
		t.Fatalf("active tab's preview box or notes affordance missing:\n%s", text)
	}
}

// DialogView is a public component, so it must also be right when a caller moves
// `state.currentTab` without projecting a pane for the tick (ActivePreviewPane nil)
// — the pane getter's fallback reads the SAME live props, so the body follows the tab
// the live state points at instead of the pane the dialog was constructed with.
func TestDialogPreviewPaneFollowsLiveTabWithoutProjectedPane(t *testing.T) {
	alphaPreview := "# Alpha preview\n\nalpha body"
	gammaPreview := "# Gamma preview\n\ngamma body"
	first := question("First", "First?", "Alpha", "Beta")
	first.Options[0].Preview = &alphaPreview
	second := question("Second", "Second?", "Gamma", "Delta")
	second.Options[0].Preview = &gammaPreview
	params := QuestionParams{Questions: []QuestionData{first, second}}
	itemsByTab := BuildItemsByTab(params)

	tabs := make([]TabComponents, 0, len(params.Questions))
	for i, data := range params.Questions {
		optionList := NewOptionListView(WrappingSelectItemsFor(itemsByTab, i), plainTheme)
		pane := NewPreviewPane(PreviewPaneConfig{
			Question:         data,
			Theme:            themeForTests(),
			GetTerminalWidth: func() int { return 160 },
			OptionListView:   optionList,
			PreviewBlock:     NewPreviewBlockRenderer(data, themeForTests(), DefaultNotesKey),
		})
		pane.SetGlobalLeftWidth(func(width int) int {
			return CrossTabLeftWidthWithDonation(params.Questions, itemsByTab, params.Questions, width)
		})
		tabs = append(tabs, TabComponents{OptionList: optionList, Preview: pane})
	}

	// The body-height getter reads the tab the dialog is on, like the session's.
	currentTab := 0
	dialog := NewDialogView(DialogConfig{
		Theme:                themeForTests(),
		Questions:            params.Questions,
		TabBar:               NewTabBar(themeForTests()),
		NotesInput:           NewTextEditor(),
		IsMulti:              true,
		TabsByIndex:          tabs,
		SubmitPicker:         NewSubmitPicker(themeForTests()),
		GetBodyHeight:        globalBodyHeight(tabs),
		GetCurrentBodyHeight: currentBodyHeight(tabs, func() int { return currentTab }),
		GetTerminalRows:      func() int { return 40 },
		CollapseKey:          DefaultCollapseKey,
		NotesKey:             DefaultNotesKey,
	}, DialogProps{State: QuestionnaireState{}, ActivePreviewPane: tabs[0].Preview})

	// Tab 0 with its pane projected: tab 0's preview paints, tab 1's does not.
	if text := renderStrings(dialog.Render(100)); !strings.Contains(text, "Alpha preview") ||
		strings.Contains(text, "Gamma preview") {
		t.Fatalf("tab 0 body:\n%s", text)
	}

	// Tab 1 with NO pane projected: the body is tab 1's pane, not the construction one.
	currentTab = 1
	dialog.SetProps(DialogProps{State: QuestionnaireState{CurrentTab: 1}, ActivePreviewPane: nil})
	text := renderStrings(dialog.Render(100))
	if !strings.Contains(text, "Gamma preview") || !strings.Contains(text, "gamma body") {
		t.Fatalf("tab 1 preview missing:\n%s", text)
	}
	if strings.Contains(text, "Alpha") || strings.Contains(text, "alpha body") {
		t.Fatalf("tab 0's pane painted on tab 1:\n%s", text)
	}
}

// A multi-select question that carries previews is not rejected: the preview pane is
// simply not mounted for it — the option list paints at full width, with no box and
// no notes affordance — and the committed answer carries no preview.
func TestSessionMultiSelectIgnoresPreview(t *testing.T) {
	preview := "# preview\n\nbody"
	multi := multiQuestion("Which features?", "A", "B")
	multi.Options[0].Preview = &preview

	session, results := newSession(t, QuestionParams{Questions: []QuestionData{multi}})
	text := renderStrings(session.Render(80))
	if strings.Contains(text, "╭") || strings.Contains(text, "Notes: press n to add notes") {
		t.Fatalf("preview UI painted for a multi-select question:\n%s", text)
	}

	session.HandleInput(" ") // check A
	session.HandleInput("\x1b[B")
	session.HandleInput("\x1b[B") // the Next row
	component, err := session.HandleInput("\r")
	if err != nil || !component.Done {
		t.Fatalf("Next did not commit: %+v %v", component, err)
	}
	answer := (*results)[0].Answers[0]
	if answer.Preview != nil {
		t.Fatalf("multi-select answer carries a preview: %q", *answer.Preview)
	}
	if len(answer.Selected) != 1 || answer.Selected[0] != "A" {
		t.Fatalf("selected = %v", answer.Selected)
	}
}

// ── tab content strategies ───────────────────────────────────────────────

// strategyDeps builds the pieces a QuestionTabStrategy needs.
func strategyDeps(params QuestionParams, isMulti bool) (*QuestionTabStrategy, []TabComponents, *TextEditor) {
	itemsByTab := BuildItemsByTab(params)
	tabs := make([]TabComponents, 0, len(params.Questions))
	for i, question := range params.Questions {
		items := WrappingSelectItemsFor(itemsByTab, i)
		optionList := NewOptionListView(items, plainTheme)
		pane := NewPreviewPane(PreviewPaneConfig{
			Question:         question,
			Theme:            themeForTests(),
			GetTerminalWidth: func() int { return 120 },
			OptionListView:   optionList,
			PreviewBlock:     NewPreviewBlockRenderer(question, themeForTests(), DefaultNotesKey),
		})
		pane.SetGlobalLeftWidth(func(width int) int {
			return CrossTabLeftWidthWithDonation(params.Questions, itemsByTab, params.Questions, width)
		})
		tab := TabComponents{OptionList: optionList, Preview: pane}
		if question.MultiSelect {
			tab.MultiSelect = NewMultiSelectView(themeForTests(), question)
		}
		tabs = append(tabs, tab)
	}
	notesInput := NewTextEditor()
	strategy := NewQuestionTabStrategy(QuestionTabStrategyConfig{
		Theme:     themeForTests(),
		Questions: params.Questions,
		GetPreviewPane: func() *PreviewPane {
			return tabs[0].Preview
		},
		TabsByIndex: tabs,
		NotesInput:  notesInput,
		IsMulti:     isMulti,
		GetCurrentBodyHeight: func(width int) int {
			return tabs[0].Preview.NaturalHeight(width)
		},
		CollapseKey: DefaultCollapseKey,
		NotesKey:    DefaultNotesKey,
	})
	return strategy, tabs, notesInput
}

func TestQuestionTabStrategyHeading(t *testing.T) {
	params := singleParams()
	strategy, _, _ := strategyDeps(params, false)
	state := stateFor()

	// Single-question: the header badge is rendered inline.
	rows := strategy.HeadingRows(state)
	text := renderStrings(renderedLines(rows, 80))
	if !strings.Contains(text, "Auth") || !strings.Contains(text, "Which auth?") {
		t.Fatalf("heading = %q", text)
	}

	// Multi-question: the tab bar already shows the header, so the badge drops.
	multiStrategy, _, _ := strategyDeps(multiParams(), true)
	text = renderStrings(renderedLines(multiStrategy.HeadingRows(state), 80))
	if strings.Contains(text, "Auth") {
		t.Fatalf("header badge rendered in multi mode: %q", text)
	}
	if !strings.Contains(text, "Which auth?") {
		t.Fatalf("question missing: %q", text)
	}
}

func TestQuestionTabStrategyBodyAndFooter(t *testing.T) {
	strategy, tabs, _ := strategyDeps(multiParams(), true)
	state := stateFor()

	// The single-select tab's body is the preview pane; the multi-select tab's is
	// the multi view.
	if body := strategy.BodyComponent(state); body != TabBody(tabs[0].Preview) {
		t.Fatalf("body = %T", body)
	}
	state.CurrentTab = 1
	if body := strategy.BodyComponent(state); body != TabBody(tabs[1].MultiSelect) {
		t.Fatalf("body = %T", body)
	}

	if got := strategy.FooterRowCount(); got != 2 {
		t.Fatalf("footer row count = %d", got)
	}
	if rows := strategy.FooterRows(state); renderedHeight(rows, 80) != 2 {
		t.Fatalf("footer rows = %d, want 2", renderedHeight(rows, 80))
	}
}

func TestQuestionTabStrategyMidRows(t *testing.T) {
	strategy, _, notesInput := strategyDeps(singleParams(), false)
	state := stateFor()

	// Closed editor with no saved note: no mid rows.
	if rows := strategy.MidRows(state); len(rows) != 0 {
		t.Fatalf("mid rows = %d", len(rows))
	}

	// A saved note renders one clipped row.
	state.NotesByTab = map[int]string{0: "first line\nsecond line"}
	rows := strategy.MidRows(state)
	if len(rows) != 1 {
		t.Fatalf("mid rows = %d", len(rows))
	}
	text := renderStrings(renderedLines(rows, 80))
	if !strings.Contains(text, "Note for this answer: first line …") {
		t.Fatalf("saved note = %q", text)
	}

	// The open editor mounts its header, the editor, and a spacer.
	state.NotesVisible = true
	notesInput.SetText("draft")
	rows = strategy.MidRows(state)
	if len(rows) != 3 {
		t.Fatalf("mid rows = %d", len(rows))
	}
	text = renderStrings(renderedLines(rows, 80))
	if !strings.Contains(text, "Notes:") || !strings.Contains(text, "draft") {
		t.Fatalf("editor rows = %q", text)
	}
}

func TestSubmitTabStrategy(t *testing.T) {
	params := multiParams()
	picker := NewSubmitPicker(themeForTests())
	notesInput := NewTextEditor()
	strategy := NewSubmitTabStrategy(SubmitTabStrategyConfig{
		Theme:        themeForTests(),
		Questions:    params.Questions,
		SubmitPicker: picker,
		NotesInput:   notesInput,
		NotesKey:     DefaultNotesKey,
	})

	if got := strategy.FooterRowCount(); got != 5 {
		t.Fatalf("footer row count = %d", got)
	}

	state := stateFor()
	state.CurrentTab = 2
	state.Answers[0] = QuestionAnswer{QuestionIndex: 0, Question: "Which auth?", Kind: AnswerKindOption, Answer: strPtr("A"), Notes: strPtr("note")}
	state.NotesByTab[2] = "global"

	body := renderedLines([]Component{strategy.BodyComponent(state)}, 100)
	text := renderStrings(body)
	if !strings.Contains(text, "A") {
		t.Fatalf("answer summary missing:\n%s", text)
	}
	if !strings.Contains(text, "Note for this answer: note") {
		t.Fatalf("per-answer note missing:\n%s", text)
	}
	if !strings.Contains(text, "Note") || !strings.Contains(text, "global") {
		t.Fatalf("global note missing:\n%s", text)
	}

	if rows := strategy.FooterRows(state); renderedHeight(rows, 80) != 5 {
		t.Fatalf("footer rows = %d, want 5", renderedHeight(rows, 80))
	}
	// The incomplete warning names the unanswered question.
	text = renderStrings(renderedLines(strategy.FooterRows(state), 100))
	if !strings.Contains(text, "Answer remaining questions before submitting:") || !strings.Contains(text, "Features") {
		t.Fatalf("warning = %q", text)
	}
	if !strings.Contains(text, "Exit to submit") && !strings.Contains(text, "Esc to cancel") {
		t.Fatalf("submit hint = %q", text)
	}

	// A fully answered questionnaire shows the ready prompt.
	state.Answers[1] = QuestionAnswer{QuestionIndex: 1, Question: "Which features?", Kind: AnswerKindMulti}
	text = renderStrings(renderedLines(strategy.FooterRows(state), 100))
	if !strings.Contains(text, "Ready to submit your answers?") {
		t.Fatalf("ready prompt = %q", text)
	}

	// The global-note editor mounts below the summary.
	state.NotesVisible = true
	notesInput.SetText("draft")
	rows := strategy.MidRows(state)
	if len(rows) != 3 {
		t.Fatalf("mid rows = %d", len(rows))
	}
	text = renderStrings(renderedLines(rows, 80))
	if !strings.Contains(text, "Global note:") || !strings.Contains(text, "draft") {
		t.Fatalf("editor rows = %q", text)
	}

	// Heading and focused range.
	if rows := strategy.HeadingRows(state); !strings.Contains(renderStrings(renderedLines(rows, 80)), "Review your answers") {
		t.Fatalf("heading = %#v", rows)
	}
	if strategy.FocusedItemRowRange(80, state) != nil {
		t.Fatal("submit tab reported an interactive focus")
	}
}

// A submit strategy without a picker still renders exactly 5 footer rows.
func TestSubmitTabStrategyWithoutPicker(t *testing.T) {
	strategy := NewSubmitTabStrategy(SubmitTabStrategyConfig{
		Theme:      themeForTests(),
		Questions:  multiParams().Questions,
		NotesInput: NewTextEditor(),
		NotesKey:   DefaultNotesKey,
	})
	if rows := strategy.FooterRows(stateFor()); renderedHeight(rows, 80) != 5 {
		t.Fatalf("footer rows = %d, want 5", renderedHeight(rows, 80))
	}
}

// The SDK paints from its own loop while terminal input arrives on another
// goroutine, and both touch the same session-owned dialog and tab components:
// Render reads them, HandleInput re-projects them through the adapter. Render
// therefore holds s.mu for the whole paint — the version that unlocked before
// SetProps/Render let a keystroke re-project the dialog underneath a running
// paint (DATA RACE under -race, and two interleaved props sets on one paint).
// Run with -race: this asserts both that no race is reported and that Render and
// HandleInput keep making progress — a re-entrant lock inside the paint (the
// body-height getter used to take s.mu) would hang instead.
func TestQuestionnaireSessionConcurrentRenderAndHandleInput(t *testing.T) {
	session := NewQuestionnaireSession(sessionConfig(multiParams()))

	const rounds = 40
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		// Painters: the render loop, hammering Render from several threads.
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; n < rounds*4; n++ {
					if len(session.Render(80)) == 0 {
						t.Error("Render painted no rows")
						return
					}
				}
			}()
		}
		// The input driver: one goroutine, the way keystrokes reach the session.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < rounds; n++ {
				session.HandleInput("\x1b[B") // option navigation
				for _, r := range "abc" {     // inline input characters
					session.HandleInput(string(r))
				}
				session.HandleInput("\t") // tab switch
				session.HandleInput("n")  // notes editor opens…
				session.HandleInput("note")
				session.HandleInput("\r") // …and the confirm key closes it
				session.ToggleCollapsedExternal()
				session.ToggleCollapsedExternal()
			}
		}()
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: Render and HandleInput could not make progress against each other")
	}
	if len(session.Render(80)) == 0 {
		t.Fatal("the session stopped painting after the concurrent run")
	}
}
