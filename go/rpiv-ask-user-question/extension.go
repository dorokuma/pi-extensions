package rpivaskuserquestion

// extension.go: the PiG wiring for the ported extension.
//
// [Extension] builds the sdk.Extension and registers the ask_user_question tool
// plus the before_agent_start reconciler — the two calls the TS default export
// makes. PiG loads a factory package by calling this function, then running the
// returned extension over the host socket.
//
// Where the TS version reaches for live pi-tui objects (ui.custom with a
// component factory, an OverlayHandle, a raw onTerminalInput listener), the port
// uses PiG's serializable equivalents: the session implements
// sdk.RemoteComponent, the handle arrives through RemoteOverlayOptions.OnHandle,
// and the collapse key is watched with Context.OnTerminalInput.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// ToolName is the canonical tool name, shared with the reconciler.
const ToolName = "ask_user_question"

// logName is the extension name reported to the host.
const logName = "rpiv-ask-user-question"

// Error messages shown to the model, verbatim from ask-user-question.ts.
const (
	errorNoUIDescription = "Error: UI not available (running in non-interactive mode)"

	errorNoCustomUI = "Error: this client cannot render the questionnaire (custom UI is unavailable, e.g. RPC/ACP hosts such as Zed or Paseo). " +
		"The user never saw the questions — do NOT treat this as a decline. Ask the questions as plain chat text instead, without using this tool."
)

// Bel is the standard terminal bell byte: one portable attention signal for the
// coming wait, written to stdout only when a TTY owns it (keeping the byte out of
// piped RPC transports).
const Bel = "\x07"

// DefaultPromptSnippet and DefaultPromptGuidelines are the system-prompt copy,
// verbatim from the TS defaults.
const DefaultPromptSnippet = "Ask the user up to 4 structured questions (2-4 options each) when requirements are ambiguous"

// DefaultPromptGuidelines is the guidelines list.
var DefaultPromptGuidelines = []string{
	"Use ask_user_question whenever the user's request is underspecified and you cannot proceed without concrete decisions — you can ask up to 4 questions per invocation.",
	`Each question MUST have 2-4 options. Every option requires a concise label (1-5 words) and a description explaining what the choice means or its trade-offs. The user can additionally type a custom answer via the automatically appended "Type something." row on every question, or press Esc to abandon the questionnaire. Do NOT author "Other" or "Type something." labels yourself — reserved labels are rejected at runtime.`,
	`Set multiSelect: true when multiple answers are valid. Provide an options[].preview markdown string when an option benefits from richer side-by-side context (mockups, code snippets, diagrams, configs) — single-select only. The "Type something." row is appended to every question; in preview mode it expands to the full pane width while typing so the custom answer is not cramped into the narrow options column. If you recommend a specific option, make that the first option and append "(Recommended)" to its label.`,
	"Do not stack multiple ask_user_question calls back-to-back — group all clarifying questions into one invocation.",
}

// DefaultToolDescription is the tool description, verbatim.
const DefaultToolDescription = `Ask the user one or more structured questions during execution. Use when you need to:
1. Gather user preferences or requirements
2. Clarify ambiguous instructions
3. Get decisions on implementation choices as you work
4. Offer choices to the user about what direction to take

Usage notes:
- Users can type a custom answer via the automatically appended "Type something." row on every question or press Esc to abandon the questionnaire. Do NOT author "Other" or "Type something." labels yourself — reserved labels are rejected at runtime.
- Use multiSelect: true when multiple answers are valid. The "Type something." row is available on every question, including when options carry a ` + "`preview`" + `; in preview mode it expands to the full pane width while typing so the custom answer is not cramped into the narrow options column.
- If you recommend a specific option, make that the first option in the list and add "(Recommended)" at the end of the label.

Preview feature:
Use the optional ` + "`preview`" + ` field on options when presenting concrete artifacts that users need to visually compare:
- ASCII mockups of UI layouts or components
- Code snippets showing different implementations
- Diagram variations
- Configuration examples

Preview content is rendered as markdown in a monospace box. Multi-line text with newlines is supported. When any option has a preview, the UI switches to a side-by-side layout with a vertical option list on the left and preview on the right. Do not use previews for simple preference questions where labels and descriptions suffice. Note: previews are only supported for single-select questions (not multiSelect).`

// Extension constructs the ask_user_question extension for PiG.
func Extension() *sdk.Extension {
	e := sdk.New(logName)

	answers := NewAnswerChannel(func(channel string, payload any) error {
		return e.Events().Emit(channel, payload)
	})
	if _, err := e.Events().On(AskUserAnswerEvent, func(_ sdk.Context, payload any) error {
		answers.Handle(payload)
		return nil
	}); err != nil {
		// A bus that cannot be reached is a load-time failure the host reports; the
		// tool still registers so the extension is not silently dead.
		_, _ = fmt.Fprint(os.Stderr, logName+": events.on failed: "+err.Error()+"\n")
	}

	// Guidance is read once at load, exactly as registerAskUserQuestionTool does.
	guidance := ValidateGuidanceFields(LoadConfig().Guidance)
	description := DefaultToolDescription
	if guidance != nil && guidance.Description != "" {
		description = guidance.Description
	}
	snippet := DefaultPromptSnippet
	if guidance != nil && guidance.PromptSnippet != "" {
		snippet = guidance.PromptSnippet
	}
	guidelines := DefaultPromptGuidelines
	if guidance != nil && len(guidance.PromptGuidelines) > 0 {
		guidelines = guidance.PromptGuidelines
	}

	e.RegisterTool(sdk.ToolDefinition{
		Name:             ToolName,
		Label:            "Ask User Question",
		Description:      description,
		PromptSnippet:    snippet,
		PromptGuidelines: guidelines,
		Parameters:       QuestionParamsSchema(),
		// Interactive: always a top-level call, never from a script, so a
		// "codemode only" policy keeps declaring it to the model (#310).
		Exposure:         sdk.ToolExposureModelOnly,
		PrepareArguments: PrepareQuestionArguments,
		RenderCall:       renderToolCall,
		RenderResult:     renderToolResult,
		Execute:          executeTool(answers),
	})

	// Reconcile the tool's presence in the active set before each turn.
	e.OnEvent(sdk.EventBeforeAgentStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		ReconcileAskUserQuestionTool(ctx)
		return nil, nil
	})
	return e
}

// ValidateGuidanceFields is the exported guidance validator (config.go holds the
// implementation; this name mirrors the TS export used at registration).
func ValidateGuidanceFields(fields *GuidanceFields) *GuidanceFields {
	if fields == nil {
		return nil
	}
	result := &GuidanceFields{}
	if fields.PromptSnippet != "" {
		result.PromptSnippet = fields.PromptSnippet
	}
	if len(fields.PromptGuidelines) > 0 {
		allNonEmpty := true
		for _, guideline := range fields.PromptGuidelines {
			if guideline == "" {
				allNonEmpty = false
				break
			}
		}
		if allNonEmpty {
			result.PromptGuidelines = append([]string{}, fields.PromptGuidelines...)
		}
	}
	if fields.Description != "" {
		result.Description = fields.Description
	}
	if result.empty() {
		return nil
	}
	return result
}

// ReconcileAskUserQuestionTool mirrors reconcileAskUserQuestionTool: strip or
// restore the tool to match ctx.HasUI. Idempotent — when the tool is already in
// the right state the active set is left untouched.
//
// RPC hosts are deliberately NOT stripped: the tool renders there via the select
// / input dialog walker, so HasUI is the honest signal.
func ReconcileAskUserQuestionTool(ctx sdk.Context) {
	active, err := ctx.GetActiveTools()
	if err != nil {
		return
	}
	hasTool := containsString(active, ToolName)
	hasUI := ctx.HasUI()
	if !hasUI && hasTool {
		filtered := make([]string, 0, len(active))
		for _, name := range active {
			if name != ToolName {
				filtered = append(filtered, name)
			}
		}
		ctx.SetActiveTools(filtered)
		return
	}
	if hasUI && !hasTool {
		next := append(append([]string{}, active...), ToolName)
		ctx.SetActiveTools(next)
	}
}

// executeTool returns the tool's Execute handler.
func executeTool(answers AnswerChannel) sdk.ToolFunc {
	return func(ctx sdk.Context, params map[string]any) (any, error) {
		// Line-terminator normalization runs once here, ahead of validation, so
		// every downstream consumer sees the same clean text (#192).
		typed := normalizeQuestionParams(decodeQuestionParams(params))
		if !ctx.HasUI() {
			return rejectWithoutUI(), nil
		}
		validation := validateQuestionnaire(typed)
		if !validation.OK {
			return buildErrorResult(validation.Message, validation.Error), nil
		}

		// RPC hosts cannot render ui.custom, but the dialog sub-protocol works:
		// route them to the sequential walker up front.
		viaRPC := ctx.Mode() == "rpc"
		toolCallID := ctx.ToolCallID()
		open := answers.Open(toolCallID, typed.Questions, hostFor(viaRPC))
		defer answers.Close(open)

		emitPrompt(ctx, typed, toolCallID)
		if viaRPC {
			return runRPCPath(ctx, typed, toolCallID), nil
		}
		return runTUIPath(ctx, typed, open), nil
	}
}

func hostFor(viaRPC bool) AnswerHost {
	if viaRPC {
		return AnswerHostRPC
	}
	return AnswerHostTUI
}

// rejectWithoutUI is the non-interactive backstop (the reconciler normally strips
// the tool first).
func rejectWithoutUI() ToolResult {
	return BuildToolResult(errorNoUIDescription, QuestionnaireResult{
		Answers:   []QuestionAnswer{},
		Cancelled: true,
		Error:     &[]QuestionnaireError{ErrorNoUI}[0],
	})
}

// buildErrorResult is a validation/usage failure envelope.
func buildErrorResult(message string, code QuestionnaireError) ToolResult {
	errCode := code
	return BuildToolResult(message, QuestionnaireResult{
		Answers:   []QuestionAnswer{},
		Cancelled: true,
		Error:     &errCode,
	})
}

// emitPrompt publishes ASK_USER_PROMPT_EVENT for external listeners.
func emitPrompt(ctx sdk.Context, typed QuestionParams, toolCallID string) {
	ctx.Events().Emit(AskUserPromptEvent, PromptEventPayload{
		Questions:  toPromptQuestions(typed),
		ToolCallID: strPtr(toolCallID),
	})
}

// emitBlocked publishes ASK_USER_BLOCKED_EVENT.
func emitBlocked(ctx sdk.Context, active bool, toolCallID string) {
	ctx.Events().Emit(AskUserBlockedEvent, BlockedEventPayload{
		Active:     active,
		ToolCallID: strPtr(toolCallID),
	})
}

// emitTerminalAttention writes one bell byte when a TTY owns the coming wait.
func emitTerminalAttention() {
	if stat, err := os.Stdout.Stat(); err == nil && stat.Mode()&os.ModeCharDevice != 0 {
		_, _ = os.Stdout.WriteString(Bel)
	}
}

// runRPCPath walks the questionnaire through the host's native dialogs, bracketed
// by the blocked-event pair.
func runRPCPath(ctx sdk.Context, typed QuestionParams, toolCallID string) ToolResult {
	emitBlocked(ctx, true, toolCallID)
	defer emitBlocked(ctx, false, toolCallID)
	emitTerminalAttention()
	result := RunRpcQuestionnaire(context.Background(), sdkDialogUI{ctx}, typed)
	return BuildQuestionnaireResponse(&result, typed)
}

// sdkDialogUI adapts sdk.Context to the walker's DialogUI contract.
type sdkDialogUI struct{ ctx sdk.Context }

// Select shows the host's selector.
func (u sdkDialogUI) Select(_ context.Context, title string, options []string) (string, bool, error) {
	value, ok, err := u.ctx.Select(title, options)
	if err != nil || !ok {
		return "", ok, err
	}
	if text, isString := value.(string); isString {
		return text, true, nil
	}
	// A non-string reply (some RPC clients answer with a number) reads as its JSON
	// text, which the walker's number/label rules then interpret.
	return fmt.Sprint(value), true, nil
}

// Input shows the host's text input.
func (u sdkDialogUI) Input(_ context.Context, title, placeholder string) (string, bool, error) {
	value, ok, err := u.ctx.Input(title, placeholder)
	if err != nil || !ok {
		return "", ok, err
	}
	if text, isString := value.(string); isString {
		return text, true, nil
	}
	return fmt.Sprint(value), true, nil
}

// runTuiPath mounts the questionnaire dialog: inline in the editor slot, or as an
// overlay (see presentation.go). Each mount races the inbound answer channel, so
// an answer accepted before any dialog took it ends the call.
func runTUIPath(ctx sdk.Context, typed QuestionParams, open *OpenQuestionnaire) ToolResult {
	itemsByTab := BuildItemsByTab(typed)
	var presentation Presentation
	dialogUI := sdkDialogUI{ctx}

	// Bracketed from before the mount: every prompt event is followed by exactly
	// one `{active:false}` for its tool call, however the call ends.
	emitBlocked(ctx, true, open.ToolCallID)
	defer func() {
		if presentation.Release != nil {
			presentation.Release()
		}
		emitBlocked(ctx, false, open.ToolCallID)
	}()

	cfg := LoadConfig()
	collapseKey := ResolveCollapseKey(cfg)
	notesKey := ResolveNotesKey(cfg)

	mount := func(p Presentation) (*QuestionnaireResult, bool) {
		return mountQuestionnaire(ctx, typed, itemsByTab, collapseKey, notesKey, p, open)
	}

	// An inbound answer accepted before any dialog mounted ends the call at once.
	if settled := open.Settled(); settled != nil {
		return BuildQuestionnaireResponse(settled, typed)
	}

	presentation = ResolvePresentation()
	emitTerminalAttention()
	result, answeredEarly := mount(presentation)
	if result == YieldToOverlay {
		// Another overlay was on screen: stack on top of it instead.
		if presentation.Release != nil {
			presentation.Release()
			presentation.Release = nil
		}
		presentation = StackedOverlayPresentation
		result, answeredEarly = mount(presentation)
	}

	if result == nil {
		// The host cannot render the dialog. The RPC walker takes over when the host
		// has the select/input primitives; otherwise the model is told the user never
		// saw the questions (the no_custom_ui backstop), never a decline.
		open.SetHost(AnswerHostRPC)
		if answeredEarly {
			return BuildQuestionnaireResponse(lateAnswer(open), typed)
		}
		if !HasDialogUI(dialogUI) {
			return buildErrorResult(errorNoCustomUI, ErrorNoCustomUI)
		}
		rpcResult := RunRpcQuestionnaire(context.Background(), dialogUI, typed)
		return BuildQuestionnaireResponse(&rpcResult, typed)
	}
	return BuildQuestionnaireResponse(result, typed)
}

// lateAnswer reads an answer the channel accepted with no dialog mounted.
func lateAnswer(open *OpenQuestionnaire) *QuestionnaireResult {
	if settled := open.Settled(); settled != nil {
		return settled
	}
	select {
	case answered := <-open.AnsweredEarly():
		return answered
	default:
		return nil
	}
}

// mountQuestionnaire performs one ctx.Custom mount under `presentation`. The
// overlay additionally captures its handle (so collapse can setHidden) and
// registers a raw terminal listener for the collapse key, which stays reachable
// while the overlay is hidden.
func mountQuestionnaire(
	ctx sdk.Context,
	typed QuestionParams,
	itemsByTab [][]WrappingSelectItem,
	collapseKey, notesKey string,
	presentation Presentation,
	open *OpenQuestionnaire,
) (*QuestionnaireResult, bool) {
	var (
		mu      sync.Mutex
		session *QuestionnaireSession
		handled *sdk.OverlayHandle
	)

	// An answer accepted before this mount (a host may mount late): close at once
	// with that answer.
	if settled := open.Settled(); settled != nil {
		return settled, true
	}

	// One hint per mount, the first time the dialog actually went hidden: the row
	// it leaves behind is a single dim line, easy to mistake for the dialog having
	// closed. setOverlayHidden below is the single authority that raises it — every
	// hide, including the one the raw collapse key drives, runs through the reducer's
	// set_overlay_hidden effect — and the one shot survives the later expand/collapse
	// cycles of the same mount.
	var hideAnnounced bool
	announceHideOnce := func() {
		mu.Lock()
		if hideAnnounced {
			mu.Unlock()
			return
		}
		hideAnnounced = true
		mu.Unlock()
		ctx.Notify(hiddenNotice(collapseKey), "info")
	}

	setOverlayHidden := func(hidden bool) {
		mu.Lock()
		handle := handled
		mu.Unlock()
		if handle == nil {
			return
		}
		if err := handle.SetHidden(hidden); err != nil {
			return
		}
		// Only the transition that really left the overlay hidden announces: a
		// rejected or still-visible setHidden must not consume the one shot.
		if hidden && handle.IsHidden() {
			announceHideOnce()
		}
	}

	// The SDK installs the host's repaint callback through SetInvalidate while the
	// overlay is active; every repaint the dialog itself changes goes through the
	// session, which owns the callback, so the mount never has to hold a mutable copy
	// of it. This closure is the hand-over used from outside the session — the
	// external editor's hand-back below — and it must stay outside the session's own
	// repaint config: a session that called back into RequestRender would recurse.
	requestRepaint := func() {
		mu.Lock()
		current := session
		mu.Unlock()
		if current != nil {
			current.RequestRender()
		}
	}

	// The raw collapse listener is registered BEFORE the session is built, exactly as
	// the TS mountQuestionnaire does: hiding the overlay is only reversible through
	// this listener, so the session may emit setOverlayHidden only when the listener
	// was actually handed back. Without it collapse falls back to the visible
	// one-line row instead of hiding the dialog beyond every input path.
	removeCollapseKeyListener := registerCollapseKeyListener(
		ctx,
		collapseKey,
		presentation.HideOnCollapse,
		func() *QuestionnaireSession {
			mu.Lock()
			defer mu.Unlock()
			return session
		},
		func() *sdk.OverlayHandle {
			mu.Lock()
			defer mu.Unlock()
			return handled
		},
	)
	canReopenWhileHidden := removeCollapseKeyListener != nil
	if removeCollapseKeyListener != nil {
		defer removeCollapseKeyListener()
	}

	questionnaire := NewQuestionnaireSession(QuestionnaireSessionConfig{
		Theme:                ctx.UITheme(),
		Params:               typed,
		ItemsByTab:           itemsByTab,
		Done:                 func(result QuestionnaireResult) { open.Settle(&result) },
		Keybindings:          DefaultKeybindings,
		CollapseKey:          collapseKey,
		NotesKey:             notesKey,
		CanReopenWhileHidden: canReopenWhileHidden,
		RowBudget:            presentation.RowBudget,
		TerminalSize: func() (int, int) {
			return ctx.Width(), ctx.Height()
		},
		SetOverlayHidden: setOverlayHidden,
		EditInput: func(value string) (string, bool) {
			return editWithHostEditor(ctx, value, requestRepaint)
		},
	})
	// Published under the same mutex the getters read through: a keystroke can land
	// on the SDK's input loop while this goroutine is still mounting.
	mu.Lock()
	session = questionnaire
	mu.Unlock()

	// Hand over the mounted dialog's closer: an answer that arrives from outside
	// while the dialog is open closes it at once, so the tool returns the external
	// answer instead of waiting for a keystroke. The overlay closes through its
	// handle — the only cross-process close path PiG offers.
	open.Attach(func(*QuestionnaireResult) {
		mu.Lock()
		handle := handled
		mu.Unlock()
		if handle != nil {
			_ = handle.Hide()
		}
	})
	defer open.Detach()

	options := sdk.RemoteOverlayOptions{}
	if presentation.Overlay {
		percent := func(text string) *sdk.OverlaySize {
			return &sdk.OverlaySize{Value: parsePercent(text), Percent: true}
		}
		options.Overlay = true
		options.OverlayOptions = &sdk.OverlayOptions{
			Anchor:    "bottom-center",
			Width:     percent("100%"),
			MaxHeight: percent("100%"),
		}
		options.OnHandle = func(handle *sdk.OverlayHandle) {
			mu.Lock()
			handled = handle
			mu.Unlock()
		}
	}
	// The inline branch leaves options.Overlay at its zero value: RemoteOverlayOptions
	// marshals `overlay` with omitempty, so the host gets no overlay key and mounts the
	// component in the editor slot — PiG's reading of the TS `{ overlay: false }`, and
	// the branch where no handle exists to hide (sdk.Context.runRemoteComponent only
	// publishes a handle when the wire carries overlay: true).

	value, err := ctx.Custom(questionnaire, options)
	if err != nil {
		// A failed custom call is "the host cannot render", never a decline.
		return nil, false
	}
	result := resultFromCustom(value)
	// First answer wins: an inbound answer accepted while the user was typing took
	// the questionnaire, so the dialog's own result is refused (mirrors the TS
	// Promise.race over the mount and open.answeredEarly).
	if settled := open.Settled(); settled != nil {
		return settled, false
	}
	if result == nil {
		if answered := lateAnswer(open); answered != nil {
			return answered, true
		}
		return nil, false
	}
	return result, false
}

// registerCollapseKeyListener mirrors registerCollapseKeyListener: the raw terminal
// listener that toggles collapse while the overlay is hidden — pi-tui routes no input
// to a hidden overlay's HandleInput, so the key has to be watched at the terminal
// level. It returns nil ("not registered") when the presentation never hides the
// dialog, when the key is off, or when the host refused the subscription; callers
// derive canReopenWhileHidden from that, which is what keeps collapse on the visible
// one-line row instead of hiding the dialog beyond every input path. The hide hint is
// NOT this listener's business: the toggle it drives runs through the reducer, whose
// set_overlay_hidden effect reaches setOverlayHidden — the single authority that
// announces the one-shot hint once the overlay really went hidden.
func registerCollapseKeyListener(
	ctx sdk.Context,
	collapseKey string,
	hideOnCollapse bool,
	session func() *QuestionnaireSession,
	handle func() *sdk.OverlayHandle,
) func() {
	if !hideOnCollapse || collapseKey == CollapseKeyOff {
		return nil
	}
	unsub, err := ctx.OnTerminalInput(func(data string) sdk.TerminalInputResult {
		mounted := handle()
		if mounted == nil {
			return sdk.TerminalInputResult{}
		}
		// Only act while the questionnaire is hidden (its HandleInput is
		// unreachable) or actually focused. When another overlay is on top
		// (e.g. /btw), leave the keystroke to that overlay instead of toggling
		// the questionnaire from underneath it.
		if !mounted.IsHidden() && !mounted.IsFocused() {
			return sdk.TerminalInputResult{}
		}
		if !MatchesKey(data, collapseKey) {
			return sdk.TerminalInputResult{}
		}
		// Kitty terminals report press, repeat and release separately: toggle only
		// on the initial press so a tap does not immediately reopen the overlay and
		// a held key does not toggle it repeatedly.
		if IsKeyRelease(data) || IsKeyRepeat(data) {
			return sdk.TerminalInputResult{Consume: true}
		}
		if current := session(); current != nil {
			current.ToggleCollapsedExternal()
		}
		return sdk.TerminalInputResult{Consume: true}
	})
	if err != nil || unsub == nil {
		return nil
	}
	return unsub
}

// hiddenNotice is the one-shot info line naming the key that brings a hidden
// questionnaire back.
func hiddenNotice(collapseKey string) string {
	return "ask_user_question hidden — press " + FormatKeySpecForDisplay(collapseKey) + " to reopen"
}

// parsePercent reads the numeric part of a "100%" size literal. A literal with
// any non-digit character in its numeric part is invalid and reads as 0, so a
// typo never silently becomes a smaller overlay.
func parsePercent(text string) float64 {
	trimmed := stringsTrimSuffix(text, "%")
	if trimmed == "" {
		return 0
	}
	value := 0.0
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + float64(r-'0')
	}
	return value
}

// stringsTrimSuffix is strings.TrimSuffix without the import here.
func stringsTrimSuffix(text, suffix string) string {
	if len(suffix) <= len(text) && text[len(text)-len(suffix):] == suffix {
		return text[:len(text)-len(suffix)]
	}
	return text
}

// resultFromCustom decodes the value the component returned across the SDK
// boundary back into a QuestionnaireResult.
func resultFromCustom(value any) *QuestionnaireResult {
	switch typed := value.(type) {
	case *QuestionnaireResult:
		return typed
	case QuestionnaireResult:
		copied := typed
		return &copied
	case map[string]any:
		result, ok := IsQuestionnaireResult(typed)
		if !ok {
			return nil
		}
		return result
	}
	return nil
}

// editWithHostEditor opens the host's external editor for a custom answer,
// notifying on failure and keeping the draft (the TS editInput contract).
// requestRender is the session's repaint request, handed to the editor flow so the
// dialog is redrawn once the editor gives the terminal back.
func editWithHostEditor(ctx sdk.Context, value string, requestRender func()) (string, bool) {
	command := externalEditorCommand(ctx)
	if command == "" {
		ctx.Notify(T("editor.failed", "External editor failed")+": No external editor command is configured", "error")
		return "", false
	}
	edited, err := EditWithExternalEditor(hostTUI{ctx: ctx, requestRender: requestRender}, command, value, nil)
	if err != nil {
		ctx.Notify(T("editor.failed", "External editor failed")+": "+err.Error(), "error")
		return "", false
	}
	return edited, true
}

// externalEditorCommand resolves the editor command. Pi exposes the settings
// manager to Node only; the port reads $VISUAL then $EDITOR, the same precedence
// the settings value itself carries.
func externalEditorCommand(ctx sdk.Context) string {
	return ResolveEditorCommand(os.Getenv)()
}

// hostTUI adapts sdk.Context to the editor flow's TUI lifecycle contract. The
// subprocess SDK has no local TUI to stop: the host owns the terminal, and the
// editor's own output is what the user sees while the overlay is parked.
type hostTUI struct {
	ctx           sdk.Context
	requestRender func()
}

// Stop is a no-op: the host owns the terminal.
func (hostTUI) Stop() {}

// Start is a no-op.
func (hostTUI) Start() {}

// RequestRender asks the questionnaire itself to repaint, the way the TS flow's
// tui.requestRender(true) repaints the custom component after the editor exits. It
// never notifies: the editor hand-back is not a user-facing event, and an empty
// notice would flash an empty row in the host UI.
func (h hostTUI) RequestRender(bool) {
	if h.requestRender != nil {
		h.requestRender()
	}
}

// BuildItemsByTab builds each tab's row list: the author options plus the
// sentinels the row-intent table says to append.
func BuildItemsByTab(params QuestionParams) [][]WrappingSelectItem {
	out := make([][]WrappingSelectItem, 0, len(params.Questions))
	for _, question := range params.Questions {
		out = append(out, BuildItemsForQuestion(question))
	}
	return out
}

// BuildItemsForQuestion mirrors buildItemsForQuestion.
func BuildItemsForQuestion(question QuestionData) []WrappingSelectItem {
	items := make([]WrappingSelectItem, 0, len(question.Options)+2)
	for _, option := range question.Options {
		items = append(items, WrappingSelectItem{
			Kind:        RowKindOption,
			Label:       option.Label,
			Description: option.Description,
		})
	}
	for _, kind := range SentinelsToAppend(question) {
		items = append(items, WrappingSelectItem{Kind: RowKind(kind), Label: DisplayLabel(kind)})
	}
	return items
}

// renderToolCall mirrors renderCall.
func renderToolCall(ctx sdk.Context, args map[string]any, _ sdk.ToolRenderContext, width int) ([]string, error) {
	params := decodeQuestionParams(args)
	theme := ctx.UITheme()
	return renderAskCallWidth(&params, theme, width), nil
}

// renderToolResult mirrors renderResult, including the error and cancelled
// receipts.
func renderToolResult(
	ctx sdk.Context,
	result sdk.ToolRenderResult,
	options sdk.ToolRenderResultOptions,
	render sdk.ToolRenderContext,
	width int,
) ([]string, error) {
	var args *QuestionParams
	if render.Args != nil {
		decoded := decodeQuestionParams(render.Args)
		args = &decoded
	}
	view := ToolResultView{Details: result.Details}
	for _, block := range result.Content {
		text, _ := block["text"].(string)
		view.Content = append(view.Content, ToolContent{Type: "text", Text: text})
	}
	return RenderAskResultLines(view, args, ctx.UITheme(), options.Expanded, render.IsError, width), nil
}

// renderAskCallWidth renders the call line at the host's width.
func renderAskCallWidth(args *QuestionParams, theme sdk.UITheme, width int) []string {
	title := theme.Fg("toolTitle", theme.Bold("Ask"))
	headers := make([]string, 0, 4)
	if args != nil {
		for _, question := range args.Questions {
			if question.Header != "" {
				headers = append(headers, question.Header)
			}
		}
	}
	text := title
	if len(headers) > 0 {
		text = title + " " + theme.Fg("accent", strings.Join(headers, " · "))
	}
	if width <= 0 {
		return []string{text}
	}
	return []string{truncateToWidth(text, width, "…", false)}
}
