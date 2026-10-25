package rpivaskuserquestion

// Ports presentation.ts (v2.15.0): inline-vs-overlay presentation, the inline
// row budget, and the editor-slot claim.

// Inline row budget. A component in the editor slot only receives a width and
// the host clips the slot from the bottom when the dock outgrows the screen, so
// the inline dialog caps itself.
const (
	// InlineRowRatio is the share of terminal rows the inline dialog may use.
	InlineRowRatio = 0.7
	// InlineMinRows is the floor, so the chrome plus a few option rows always fit.
	InlineMinRows = 12
)

// RowBudget maps the terminal's row count to the rows the dialog may render.
type RowBudget func(terminalRows int) int

// FullRowBudget is the whole of terminal.rows: the overlay, and any host that
// already allocates the dialog's rows.
func FullRowBudget(terminalRows int) int { return terminalRows }

// InlineRowBudget mirrors inlineRowBudget: min(rows, max(InlineMinRows,
// floor(rows * InlineRowRatio))).
func InlineRowBudget(terminalRows int) int {
	return minInt(terminalRows, maxInt(InlineMinRows, int(float64(terminalRows)*InlineRowRatio)))
}

// YieldToOverlay is resolved by an inline mount that gives the editor slot up;
// execute remounts as an overlay on seeing it. It is a valid cancel result, so
// it is harmless should it ever escape.
var YieldToOverlay = &QuestionnaireResult{Answers: []QuestionAnswer{}, Cancelled: true}

// Presentation is a resolved mount mode.
type Presentation struct {
	// Overlay: true → focused overlay; false → the editor slot.
	Overlay bool
	// HideOnCollapse: overlay only — collapse may fully hide it. False when
	// stacked over another overlay.
	HideOnCollapse bool
	RowBudget      RowBudget
	// Release frees the inline slot; present only when this presentation claimed
	// it. Call once, on teardown.
	Release func()
}

// OverlayPresentation is the full-height overlay.
var OverlayPresentation = Presentation{
	Overlay:        true,
	HideOnCollapse: true,
	RowBudget:      FullRowBudget,
}

// StackedOverlayPresentation is the overlay a question stacks as when another
// overlay is already open. Collapse keeps it visible as the one-row hint: the
// host closes an overlay-mode custom by popping the TOPMOST overlay, so a hidden
// questionnaire above another overlay would be popped when that one closes,
// stranding it and hanging the agent.
var StackedOverlayPresentation = Presentation{
	Overlay:        true,
	HideOnCollapse: false,
	RowBudget:      FullRowBudget,
}

// inlineSlotHeld guards the single editor slot: a second inline mount would
// replace the first, whose promise would never settle.
var inlineSlotHeld = false

// claimInlineSlot mirrors claimInlineSlot.
func claimInlineSlot() func() {
	if inlineSlotHeld {
		return nil
	}
	inlineSlotHeld = true
	released := false
	return func() {
		if released {
			return
		}
		released = true
		inlineSlotHeld = false
	}
}

// ResolvePresentation mirrors resolvePresentation: inline in the editor slot, or
// the overlay when another questionnaire already holds the slot.
func ResolvePresentation() Presentation {
	release := claimInlineSlot()
	if release == nil {
		return OverlayPresentation
	}
	return Presentation{Overlay: false, RowBudget: InlineRowBudget, Release: release}
}

// MustYieldInlineSlot mirrors mustYieldInlineSlot: true when an inline mount
// would land underneath an overlay already on screen. Only a capturing overlay
// counts — a non-capturing one never takes keyboard focus, so the inline question
// stays focused and keeps the transcript scrollable.
//
// PiG's subprocess SDK has no `getTopmostVisibleOverlay`/`hasOverlay` accessor,
// so the port asks the injected probe (set by the session from the host's
// overlay state) instead. With no probe, an inline mount never yields.
func MustYieldInlineSlot(topmostOverlayVisible func() bool) bool {
	if topmostOverlayVisible == nil {
		return false
	}
	return topmostOverlayVisible()
}

// RowBudgetForHost mirrors rowBudgetForHost. The host-allocated-rows marker is a
// JS registry symbol with no PiG equivalent, so the host reports it through the
// probe.
func RowBudgetForHost(hostAllocatesRows bool, budget RowBudget) RowBudget {
	if hostAllocatesRows {
		return FullRowBudget
	}
	return budget
}
