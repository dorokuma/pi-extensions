package rpivaskuserquestion

// Ports state/questionnaire-session.ts and state/build-questionnaire.ts
// (v2.15.0): the session runtime.
//
// The session owns the canonical state cell, the two headless editor cells, the
// notes-draft mirror and the effect runner. State transitions go through the pure
// Reduce; UI fan-out goes through the props adapter.
//
// Against PiG it is also an sdk.RemoteComponent: the SDK renders it locally and
// sends only terminal lines to the host, routing input back only while the
// overlay owns focus. The TS session returned a component object to
// ctx.ui.custom; here the session IS that component.

import (
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// QuestionnaireSessionConfig is the session's construction-time config.
type QuestionnaireSessionConfig struct {
	Theme      sdk.UITheme
	Params     QuestionParams
	ItemsByTab [][]WrappingSelectItem
	Done       func(QuestionnaireResult)
	// Keybindings resolves the semantic key names.
	Keybindings Keybindings
	// EditInput opens the configured external editor. It returns ok=false on a
	// reported launch failure, keeping the draft.
	EditInput func(value string) (string, bool)
	// CollapseKey is the resolved collapse/expand spec ("off" disables it).
	CollapseKey string
	// NotesKey is the resolved notes key.
	NotesKey string
	// CanReopenWhileHidden is true iff execute registered the raw terminal-input
	// listener — the only path that can reach a hidden overlay. Without it the
	// visible one-line collapsed row is the fallback rendering, since hiding the
	// overlay would otherwise be irreversible.
	CanReopenWhileHidden bool
	// RowBudget maps terminal rows to rows the dialog may render.
	RowBudget RowBudget
	// TerminalSize reports the current terminal columns/rows.
	TerminalSize func() (columns, rows int)
	// RequestRender asks the host to repaint. It is invoked by RequestRender with
	// s.mu released, so it must NOT call back into RequestRender: that closure would
	// re-enter itself without end (the mount therefore wires its repaint request only
	// into the external-editor flow, and lets the SDK's invalidate callback repaint).
	RequestRender func()
	// SetOverlayHidden drives OverlayHandle.SetHidden while collapsing.
	SetOverlayHidden func(hidden bool)
}

// QuestionnaireSession is the dialog runtime and the PiG remote component.
type QuestionnaireSession struct {
	mu sync.Mutex

	state      QuestionnaireState
	questions  []QuestionData
	isMulti    bool
	itemsByTab [][]WrappingSelectItem

	notesInput  *TextEditor
	inlineInput *TextEditor
	viewAdapter *QuestionnairePropsAdapter
	dialog      *DialogView
	tabViews    []TabComponents

	theme                sdk.UITheme
	keybindings          Keybindings
	editInput            func(value string) (string, bool)
	collapseKey          string
	notesKey             string
	canReopenWhileHidden bool
	rowBudget            RowBudget
	terminalSize         func() (columns, rows int)
	requestRender        func()
	setOverlayHidden     func(hidden bool)

	// applyContext is the reducer's session-lifetime context.
	applyContext ApplyContext

	inputEditorOpen bool
	// done is the settle path; it fires at most once.
	done       func(QuestionnaireResult)
	finished   bool
	pending    *QuestionnaireResult
	invalidate func()
}

// NewQuestionnaireSession assembles the session: tabs, dialog, adapter, and the
// initial projection.
func NewQuestionnaireSession(config QuestionnaireSessionConfig) *QuestionnaireSession {
	questions := config.Params.Questions
	isMulti := len(questions) > 1
	itemsByTab := config.ItemsByTab

	session := &QuestionnaireSession{
		state:                NewQuestionnaireState(),
		questions:            questions,
		isMulti:              isMulti,
		itemsByTab:           itemsByTab,
		theme:                config.Theme,
		keybindings:          config.Keybindings,
		editInput:            config.EditInput,
		collapseKey:          config.CollapseKey,
		notesKey:             config.NotesKey,
		canReopenWhileHidden: config.CanReopenWhileHidden,
		rowBudget:            config.RowBudget,
		terminalSize:         config.TerminalSize,
		requestRender:        config.RequestRender,
		setOverlayHidden:     config.SetOverlayHidden,
		done:                 config.Done,
	}
	if session.rowBudget == nil {
		session.rowBudget = FullRowBudget
	}
	if session.terminalSize == nil {
		session.terminalSize = func() (int, int) { return 80, 24 }
	}
	session.applyContext = ApplyContext{Questions: questions, ItemsByTab: itemsByTab}

	session.notesInput = NewTextEditor()
	session.inlineInput = NewTextEditor()

	columns, _ := session.terminalSize()
	_ = columns
	theme := config.Theme
	selectTheme := sdkTheme(theme)

	tabs := make([]TabComponents, 0, len(questions))
	for i, question := range questions {
		items := WrappingSelectItemsFor(itemsByTab, i)
		optionList := NewOptionListView(items, selectTheme)
		previewBlock := NewPreviewBlockRenderer(question, theme, session.notesKey)
		pane := NewPreviewPane(PreviewPaneConfig{
			Question:         question,
			Theme:            theme,
			GetTerminalWidth: func() int { cols, _ := session.terminalSize(); return cols },
			OptionListView:   optionList,
			PreviewBlock:     previewBlock,
		})
		tab := TabComponents{OptionList: optionList, Preview: pane}
		if question.MultiSelect {
			tab.MultiSelect = NewMultiSelectView(theme, question)
			tab.BodyHeights = MultiSelectBodyHeights(tab.MultiSelect)
		} else {
			tab.BodyHeights = PreviewBodyHeights(pane)
		}
		tabs = append(tabs, tab)
	}
	session.tabViews = tabs
	session.injectGlobalLeftWidth(tabs)

	var submitPicker *SubmitPicker
	var tabBar *TabBar
	if isMulti {
		submitPicker = NewSubmitPicker(theme)
		tabBar = NewTabBar(theme)
	}

	dialog := NewDialogView(DialogConfig{
		Theme:         theme,
		Questions:     questions,
		TabBar:        tabBar,
		NotesInput:    session.notesInput,
		IsMulti:       isMulti,
		TabsByIndex:   tabs,
		SubmitPicker:  submitPicker,
		GetBodyHeight: globalBodyHeight(tabs),
		GetCurrentBodyHeight: currentBodyHeight(tabs, func() int {
			// Read without locking: the dialog is a private field of the session and
			// this getter only runs from inside its Render, which the session performs
			// while it already holds s.mu — re-locking here would self-deadlock the
			// paint.
			return session.state.CurrentTab
		}),
		GetTerminalRows: func() int {
			_, rows := session.terminalSize()
			return session.rowBudget(rows)
		},
		CollapseKey: session.collapseKey,
		NotesKey:    session.notesKey,
	}, DialogProps{State: session.state, ActivePreviewPane: activePane(tabs, session.state, len(questions))})
	session.dialog = dialog

	globalBindings := []BoundGlobalBinding{globalBinding(dialog, SelectDialogProps)}
	if submitPicker != nil {
		globalBindings = append(globalBindings, globalSubmitPickerBinding(submitPicker))
	}
	if tabBar != nil {
		globalBindings = append(globalBindings, globalTabBarBinding(tabBar))
	}

	session.viewAdapter = NewQuestionnairePropsAdapter(QuestionnairePropsAdapterConfig{
		// Never the session's own repaint callback: Apply is called while `s.mu` is
		// held (commit, handleIgnoreInline), so a callback reaching back into any
		// public entry of the session — RequestRender above all, since that is what
		// the mount's requestRepaint does — would lock the mutex this very goroutine
		// already owns and self-deadlock. Repaint is requested by the callers of
		// Apply, once the lock is released.
		RequestRender:       nil,
		Questions:           questions,
		ItemsByTab:          itemsByTab,
		TabsByIndex:         tabs,
		InlineInput:         session.inlineInput,
		GlobalBindings:      globalBindings,
		PerTabBindings:      []BoundPerTabBinding{OptionListBinding(), PreviewPaneBinding(), MultiSelectBinding()},
		ExtraInvalidatables: []Invalidatable{session.notesInput},
	})

	// Initial paint is delegated to the adapter, as the TS session does, and the
	// repaint request follows it outside the projection.
	session.viewAdapter.Apply(session.state)
	session.RequestRender()
	return session
}

// SetInvalidate implements sdk.RemoteComponentInvalidator.
func (s *QuestionnaireSession) SetInvalidate(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidate = fn
}

// RequestRender asks the host to repaint. Before the SDK installs the callback
// (or after the overlay closes) it is a no-op.
func (s *QuestionnaireSession) RequestRender() {
	// Both callbacks run with the lock released: each one is host code that may
	// re-enter this session synchronously (the SDK renders from its invalidate
	// callback, and a host may hand its repaint straight back here), so holding
	// s.mu across either of them is a same-goroutine deadlock on the re-entry.
	s.mu.Lock()
	invalidate := s.invalidate
	requestRender := s.requestRender
	s.mu.Unlock()
	if requestRender != nil {
		requestRender()
	}
	if invalidate != nil {
		invalidate()
	}
}

// Dispose implements sdk.RemoteComponentDisposer.
func (s *QuestionnaireSession) Dispose() {}

// Render implements sdk.RemoteComponent: the collapsed one-line hint, or the
// full dialog. The whole paint runs under s.mu: Pi renders from its own loop
// while keystrokes land on the input goroutine, and the dialog plus its tab
// components are session-owned state that commit re-projects through
// viewAdapter.Apply (SetProps) under this very lock. Painting outside it let a
// render read the dialog mid-projection.
func (s *QuestionnaireSession) Render(width int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Collapsed {
		// One dim row, exactly like the TS buildCollapsedRender: inline it sits in the
		// editor slot, and for the overlay a single row shrinks the bottom-anchored
		// overlay from full height to one line so the transcript stays readable.
		return []string{s.theme.Fg("dim", " "+collapsedHintLine(s.collapseKey)+" ")}
	}
	// The dialog reads its props from the adapter's last Apply, so refresh them
	// before every paint. The snapshot is cloned inline rather than through
	// snapshotState, which would re-lock the mutex this goroutine already owns.
	snapshot := s.state.clone()
	s.dialog.SetProps(DialogProps{State: snapshot, ActivePreviewPane: activePane(s.tabViews, snapshot, len(s.questions))})
	return s.dialog.Render(width)
}

// HandleInput implements sdk.RemoteComponent.
func (s *QuestionnaireSession) HandleInput(data string) (sdk.RemoteComponentResult, error) {
	s.mu.Lock()
	if s.inputEditorOpen {
		s.mu.Unlock()
		return sdk.RemoteComponentResult{}, nil
	}
	state := s.state
	runtime := s.runtimeLocked(data)
	s.mu.Unlock()

	action := RouteKey(data, state, runtime)
	if action.Kind == ActionIgnore {
		s.handleIgnoreInline(data, state)
		return sdk.RemoteComponentResult{}, nil
	}
	return s.commit(action), nil
}

// commit applies an action through the reducer, runs its effects, mirrors the
// notes draft and re-projects.
func (s *QuestionnaireSession) commit(action QuestionnaireAction) sdk.RemoteComponentResult {
	s.mu.Lock()
	result := Reduce(s.state, action, s.applyContext)
	s.state = result.State
	effects := result.Effects
	s.mu.Unlock()

	for _, effect := range effects {
		s.runEffect(effect)
	}

	s.mu.Lock()
	s.mirrorNotesDraftLocked()
	s.viewAdapter.Apply(s.state)
	pending := s.pending
	s.pending = nil
	finished := s.finished
	s.mu.Unlock()

	// Ask for the repaint only now that the projection is complete and the lock is
	// gone: the repaint path is the host's, and it may re-enter the session.
	s.RequestRender()

	if pending != nil {
		// Deliver the settled result to the session's owner exactly once (the
		// reducer's `done` effect), and report it to the SDK so the overlay closes.
		// `done` is host code, so it runs AFTER `s.mu` is released: calling it under
		// the lock would let a re-entrant callback from the owner deadlock on the
		// very lock this goroutine is holding.
		value := *pending
		shouldCallDone := false
		if !finished {
			s.mu.Lock()
			if !s.finished {
				s.finished = true
				shouldCallDone = true
			}
			s.mu.Unlock()
		}
		if shouldCallDone && s.done != nil {
			s.done(value)
		}
		return sdk.RemoteComponentResult{Done: true, Value: value}
	}
	return sdk.RemoteComponentResult{}
}

// runEffect executes one declarative effect.
func (s *QuestionnaireSession) runEffect(effect Effect) {
	switch effect.Kind {
	case EffectSetInputBuffer, EffectClearInputBuffer, EffectSetNotesValue, EffectSetNotesFocused, EffectForwardNotesKeystroke:
		// These effects mutate session-owned components — the very editors Render
		// paints, and does so while holding s.mu — so they are mutated under that
		// same lock. No host code runs here: the headless editor has no OnChange
		// wired in this session. The host-facing effects below (the external editor,
		// hiding the overlay) keep running with the lock released, because either of
		// them may re-enter the session synchronously.
		s.mu.Lock()
		switch effect.Kind {
		case EffectSetInputBuffer:
			s.inlineInput.SetText(effect.Value)
		case EffectClearInputBuffer:
			s.inlineInput.SetText("")
		case EffectSetNotesValue:
			s.notesInput.SetText(effect.Value)
		case EffectSetNotesFocused:
			s.notesInput.SetFocused(effect.Focused)
		case EffectForwardNotesKeystroke:
			s.notesInput.HandleInput(effect.Data)
		}
		s.mu.Unlock()
	case EffectOpenInputEditor:
		s.openInputEditor(effect.Value)
	case EffectSetOverlayHidden:
		// Suppressed entirely without a raw terminal listener: hiding would then
		// be irreversible, so the visible one-line collapsed row is the fallback.
		if s.canReopenWhileHidden && s.setOverlayHidden != nil {
			s.setOverlayHidden(effect.Hidden)
		}
	case EffectDone:
		if effect.Result != nil {
			s.mu.Lock()
			s.pending = effect.Result
			s.mu.Unlock()
		}
	}
}

// openInputEditor opens the external editor; on success it commits the
// replacement buffer, on reported failure it retains the draft.
func (s *QuestionnaireSession) openInputEditor(value string) {
	s.mu.Lock()
	if s.inputEditorOpen || s.editInput == nil {
		s.mu.Unlock()
		return
	}
	s.inputEditorOpen = true
	s.mu.Unlock()

	edited, ok := s.editInput(value)

	s.mu.Lock()
	s.inputEditorOpen = false
	s.mu.Unlock()
	if ok {
		s.commit(QuestionnaireAction{Kind: ActionInputReplace, Value: edited})
	}
}

// handleIgnoreInline mirrors handleIgnoreInline: the per-keystroke fast path that
// delegates text editing to the headless editor.
func (s *QuestionnaireSession) handleIgnoreInline(data string, state QuestionnaireState) {
	if !state.InputMode {
		return
	}
	s.mu.Lock()
	s.inlineInput.HandleInput(data)
	s.viewAdapter.Apply(s.state)
	s.mu.Unlock()
	// Same rule as commit: project under the lock, repaint outside it.
	s.RequestRender()
}

// mirrorNotesDraftLocked mirrors mirrorNotesDraft: keep the canonical draft in
// step with the editor.
func (s *QuestionnaireSession) mirrorNotesDraftLocked() {
	draft := s.notesInput.GetExpandedText()
	if s.state.NotesDraft != draft {
		s.state.NotesDraft = draft
	}
}

// runtimeLocked builds the per-tick dispatcher context. data is the chunk being
// routed, needed only for the movement predicates.
func (s *QuestionnaireSession) runtimeLocked(_ string) QuestionnaireRuntime {
	cursor := s.inlineInput.GetCursor()
	lines := s.inlineInput.GetLines()
	var current *WrappingSelectItem
	items := s.itemsForTab(s.state.CurrentTab)
	if s.state.OptionIndex >= 0 && s.state.OptionIndex < len(items) {
		item := items[s.state.OptionIndex]
		current = &item
	}
	return QuestionnaireRuntime{
		Keybindings:      s.keybindings,
		InputBuffer:      s.inlineInput.GetExpandedText(),
		CanMoveInputUp:   cursor.Line > 0,
		CanMoveInputDown: cursor.Line < len(lines)-1,
		Questions:        s.questions,
		IsMulti:          s.isMulti,
		CurrentItem:      current,
		Items:            items,
		CollapseKey:      s.collapseKey,
		NotesKey:         s.notesKey,
	}
}

// itemsForTab returns the tab's row list, nil-safe.
func (s *QuestionnaireSession) itemsForTab(tab int) []WrappingSelectItem {
	return WrappingSelectItemsFor(s.itemsByTab, tab)
}

// snapshotState copies the current state for a render pass.
func (s *QuestionnaireSession) snapshotState() QuestionnaireState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.clone()
}

// ToggleCollapsedExternal mirrors toggleCollapsedExternal: the public toggle used
// by the raw terminal-input listener, routed through commit so the transition
// stays in the reducer and the overlay hide happens via the effect like every
// other side effect.
func (s *QuestionnaireSession) ToggleCollapsedExternal() {
	s.mu.Lock()
	blocked := s.inputEditorOpen
	s.mu.Unlock()
	if blocked {
		return
	}
	s.commit(QuestionnaireAction{Kind: ActionToggleCollapsed})
}

// collapsedHintLine mirrors buildCollapsedRender's hint line. With collapseKey
// "off" the router never toggles collapsed, but ToggleCollapsedExternal is a
// public ungated entry — fall back to the cancel-only line rather than rendering
// a literal "Off to expand".
func collapsedHintLine(collapseKey string) string {
	if collapseKey == CollapseKeyOff {
		return T("hint.cancel", HintPartCancel)
	}
	return stringsReplace(
		T("hint.expand_line", CollapsedHintTemplate()),
		KeyPlaceholder, FormatKeySpecForDisplay(collapseKey))
}

// injectGlobalLeftWidth mirrors injectGlobalLeftWidth: every pane gets the same
// cross-tab left-width getter, so the options column is stable on tab switch.
func (s *QuestionnaireSession) injectGlobalLeftWidth(tabs []TabComponents) {
	questions := s.questions
	itemsByTab := s.itemsByTab
	getter := func(paneWidth int) int {
		return CrossTabLeftWidthWithDonation(questions, itemsByTab, questions, paneWidth)
	}
	for i := range tabs {
		if tabs[i].Preview != nil {
			tabs[i].Preview.SetGlobalLeftWidth(getter)
		}
	}
}

// WrappingSelectItemsFor reads one tab's rows, nil-safe.
func WrappingSelectItemsFor(itemsByTab [][]WrappingSelectItem, tab int) []WrappingSelectItem {
	if tab < 0 || tab >= len(itemsByTab) {
		return nil
	}
	return itemsByTab[tab]
}

// globalBodyHeight mirrors buildHeightComputers' global getter: the worst-case
// body height across every tab.
func globalBodyHeight(tabs []TabComponents) func(width int) int {
	return func(width int) int {
		maximum := 0
		for i := range tabs {
			if tabs[i].BodyHeights == nil {
				continue
			}
			if h := tabs[i].BodyHeights(width).Max; h > maximum {
				maximum = h
			}
		}
		return maxInt(1, maximum)
	}
}

// currentBodyHeight mirrors the current getter: the active tab's body height.
func currentBodyHeight(tabs []TabComponents, currentTab func() int) func(width int) int {
	return func(width int) int {
		index := minInt(currentTab(), len(tabs)-1)
		if index < 0 || index >= len(tabs) || tabs[index].BodyHeights == nil {
			return 0
		}
		return maxInt(0, tabs[index].BodyHeights(width).Current)
	}
}

// activePane mirrors pickInitialActivePreview / the adapter's pane resolution.
func activePane(tabs []TabComponents, state QuestionnaireState, totalQuestions int) *PreviewPane {
	if len(tabs) == 0 {
		return nil
	}
	index := SelectActivePreviewPaneIndex(state.CurrentTab, totalQuestions)
	if index >= 0 && index < len(tabs) && tabs[index].Preview != nil {
		return tabs[index].Preview
	}
	return tabs[0].Preview
}
