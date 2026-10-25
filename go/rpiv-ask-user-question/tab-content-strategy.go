package rpivaskuserquestion

// Ports view/tab-content-strategy.ts (v2.15.0): the per-tab content providers
// and the two hint builders.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Tab copy.
const (
	notesHeader       = "Notes:"
	globalNotesHeader = "Global note:"
	reviewGlobalHint  = KeyPlaceholder + " to add a note"
	reviewNoteLabel   = "Note"
	savedNoteLabel    = "Note for this answer:"
)

// Component is a renderable row producer. The strategies return these instead of
// live pi-tui components; the dialog concatenates their rendered lines.
type Component interface {
	Render(width int) []string
}

// textRow is a padded one-line text row (pi-tui Text with paddingX=1,
// paddingY=0).
type textRow struct {
	Text        string
	PaddingLeft int
}

// Render mirrors Text.render with no vertical padding.
func (t textRow) Render(width int) []string {
	pad := repeatSpace(t.PaddingLeft)
	avail := maxInt(0, width-t.PaddingLeft)
	return []string{pad + truncateToWidth(t.Text, avail, "", false)}
}

// spacerRow renders n blank rows (pi-tui Spacer).
type spacerRow struct{ Lines int }

// Render renders the blank rows.
func (s spacerRow) Render(int) []string {
	rows := make([]string, maxInt(0, s.Lines))
	return rows
}

// editorRows renders the headless editor's current lines. It is the Component
// adapter the strategies use for the notes editor.
type editorRows struct{ Editor *TextEditor }

// Render renders the editor's lines, clipped to width.
func (e editorRows) Render(width int) []string {
	lines := e.Editor.GetLines()
	if len(lines) == 0 {
		return []string{""}
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, truncateToWidth(line, width, "", false))
	}
	return out
}

// TabContentStrategy is the per-tab content provider. Per-tick state threads
// through the method arguments; the chrome wrapper enforces height equality
// across tabs via BodyHeight + FooterRowCount.
type TabContentStrategy interface {
	// FooterRowCount is the total RENDERED footer row count. It MUST equal what
	// FooterRows emits; it drives the residual math.
	FooterRowCount() int
	// HeadingRows are the variable rows above the body.
	HeadingRows(state QuestionnaireState) []Component
	// BodyComponent is the component placed at the body slot.
	BodyComponent(state QuestionnaireState) TabBody
	// BodyHeight is the natural rendered height of BodyComponent(state).
	BodyHeight(width int, state QuestionnaireState) int
	// MidRows are the optional rows between the body's trailing spacer and the
	// bottom border.
	MidRows(state QuestionnaireState) []Component
	// FooterRows are the rows below the bottom border; the rendered count MUST
	// equal FooterRowCount.
	FooterRows(state QuestionnaireState) []Component
	// FocusedItemRowRange is the focused item's row range within the body, or nil.
	FocusedItemRowRange(width int, state QuestionnaireState) *[2]int
}

// TabBody is the body-slot contract: a renderable that can also report the
// focused row range and its natural heights.
type TabBody interface {
	Component
	FocusedItemRowRange(width int) [2]int
}

// QuestionTabStrategyConfig is QuestionTabStrategy's construction-time config.
type QuestionTabStrategyConfig struct {
	Theme                sdk.UITheme
	Questions            []QuestionData
	GetPreviewPane       func() *PreviewPane
	TabsByIndex          []TabComponents
	NotesInput           *TextEditor
	IsMulti              bool
	GetCurrentBodyHeight func(width int) int
	CollapseKey          string
	NotesKey             string
}

// QuestionTabStrategy renders one question tab.
type QuestionTabStrategy struct {
	config QuestionTabStrategyConfig
}

// NewQuestionTabStrategy constructs the strategy.
func NewQuestionTabStrategy(config QuestionTabStrategyConfig) *QuestionTabStrategy {
	return &QuestionTabStrategy{config: config}
}

// FooterRowCount mirrors `readonly footerRowCount = 2`: Spacer(1) plus
// OneLineClippedText.
func (s *QuestionTabStrategy) FooterRowCount() int { return 2 }

// HeadingRows mirrors headingRows.
func (s *QuestionTabStrategy) HeadingRows(state QuestionnaireState) []Component {
	out := make([]Component, 0, 4)
	var question *QuestionData
	if state.CurrentTab >= 0 && state.CurrentTab < len(s.config.Questions) {
		question = &s.config.Questions[state.CurrentTab]
	}
	// In multi-question mode the tab bar already shows the header.
	if !s.config.IsMulti && question != nil && question.Header != "" {
		out = append(out, textRow{
			Text:        s.config.Theme.Bg("selectedBg", " "+question.Header+" "),
			PaddingLeft: 1,
		})
		out = append(out, spacerRow{Lines: 1})
	}
	if question != nil {
		out = append(out, textRow{Text: s.config.Theme.Bold(question.Question), PaddingLeft: 1})
		out = append(out, spacerRow{Lines: 1})
	}
	return out
}

// BodyComponent mirrors bodyComponent.
func (s *QuestionTabStrategy) BodyComponent(state QuestionnaireState) TabBody {
	var question *QuestionData
	if state.CurrentTab >= 0 && state.CurrentTab < len(s.config.Questions) {
		question = &s.config.Questions[state.CurrentTab]
	}
	var multiSelect *MultiSelectView
	if state.CurrentTab >= 0 && state.CurrentTab < len(s.config.TabsByIndex) {
		multiSelect = s.config.TabsByIndex[state.CurrentTab].MultiSelect
	}
	if question != nil && question.MultiSelect && multiSelect != nil {
		return multiSelect
	}
	return s.config.GetPreviewPane()
}

// BodyHeight mirrors bodyHeight.
func (s *QuestionTabStrategy) BodyHeight(width int, _ QuestionnaireState) int {
	return s.config.GetCurrentBodyHeight(width)
}

// MidRows mirrors midRows: the open notes editor, or one clipped saved-note row.
func (s *QuestionTabStrategy) MidRows(state QuestionnaireState) []Component {
	if state.NotesVisible {
		return []Component{
			textRow{Text: s.config.Theme.Fg("muted", T("notes.header", notesHeader)), PaddingLeft: 1},
			editorRows{Editor: s.config.NotesInput},
			spacerRow{Lines: 1},
		}
	}
	note := SelectSavedNote(state.NotesByTab, state.Answers, state.CurrentTab)
	if note == nil {
		return nil
	}
	firstLine := ""
	rest := 0
	parts := splitLines(*note)
	if len(parts) > 0 {
		firstLine = parts[0]
		rest = len(parts) - 1
	}
	text := T("notes.saved_label", savedNoteLabel) + " " + firstLine
	if rest > 0 {
		text += " …"
	}
	return []Component{OneLineClippedText{Text: s.config.Theme.Fg("dim", text), PaddingLeft: 1}}
}

// FooterRows mirrors footerRows.
func (s *QuestionTabStrategy) FooterRows(state QuestionnaireState) []Component {
	var question *QuestionData
	if state.CurrentTab >= 0 && state.CurrentTab < len(s.config.Questions) {
		question = &s.config.Questions[state.CurrentTab]
	}
	return []Component{
		spacerRow{Lines: 1},
		OneLineClippedText{
			Text: s.config.Theme.Fg("dim",
				BuildHintText(question, s.config.IsMulti, state, s.config.CollapseKey, s.config.NotesKey)),
			PaddingLeft: 1,
		},
	}
}

// FocusedItemRowRange mirrors focusedItemRowRange.
func (s *QuestionTabStrategy) FocusedItemRowRange(width int, state QuestionnaireState) *[2]int {
	var question *QuestionData
	if state.CurrentTab >= 0 && state.CurrentTab < len(s.config.Questions) {
		question = &s.config.Questions[state.CurrentTab]
	}
	var multiSelect *MultiSelectView
	if state.CurrentTab >= 0 && state.CurrentTab < len(s.config.TabsByIndex) {
		multiSelect = s.config.TabsByIndex[state.CurrentTab].MultiSelect
	}
	if question != nil && question.MultiSelect && multiSelect != nil {
		rows := multiSelect.FocusedItemRowRange(width)
		return &rows
	}
	rows := s.config.GetPreviewPane().FocusedItemRowRange(width)
	return &rows
}

// SubmitTabStrategyConfig is SubmitTabStrategy's construction-time config.
type SubmitTabStrategyConfig struct {
	Theme        sdk.UITheme
	Questions    []QuestionData
	SubmitPicker *SubmitPicker
	NotesInput   *TextEditor
	NotesKey     string
}

// SubmitTabStrategy renders the Submit tab.
type SubmitTabStrategy struct {
	config SubmitTabStrategyConfig
}

// NewSubmitTabStrategy constructs the strategy.
func NewSubmitTabStrategy(config SubmitTabStrategyConfig) *SubmitTabStrategy {
	return &SubmitTabStrategy{config: config}
}

// FooterRowCount mirrors `readonly footerRowCount = 5`.
func (s *SubmitTabStrategy) FooterRowCount() int { return 5 }

// HeadingRows mirrors headingRows.
func (s *SubmitTabStrategy) HeadingRows(_ QuestionnaireState) []Component {
	return []Component{
		textRow{
			Text:        s.config.Theme.Bold(s.config.Theme.Fg("accent", T("review.heading", ReviewHeading))),
			PaddingLeft: 1,
		},
		spacerRow{Lines: 1},
	}
}

// BodyComponent mirrors bodyComponent: the answer summary plus the committed
// global note.
func (s *SubmitTabStrategy) BodyComponent(state QuestionnaireState) TabBody {
	container := &stackBody{}
	for i := range s.config.Questions {
		question := s.config.Questions[i]
		answer, ok := state.Answers[i]
		if !ok {
			continue
		}
		label := "Q" + itoa(i+1)
		if question.Header != "" {
			label = question.Header
		}
		container.Add(textRow{Text: s.config.Theme.Fg("muted", " ● "+label), PaddingLeft: 1})
		container.Add(textRow{
			Text:        "   " + s.config.Theme.Fg("muted", "→") + " " + s.config.Theme.Fg("text", FormatAnswerScalar(answer, FormatVariantSummary)),
			PaddingLeft: 1,
		})
		if answer.Notes != nil && len(*answer.Notes) > 0 {
			container.Add(textRow{
				Text:        s.config.Theme.Fg("dim", "     "+T("notes.saved_label", savedNoteLabel)+" "+*answer.Notes),
				PaddingLeft: 1,
			})
		}
	}
	// The committed global note as a review entry — hidden while the editor is
	// open, since the editor (seeded with this text) is the live surface then.
	if globalNote, ok := state.NotesByTab[len(s.config.Questions)]; ok && len(globalNote) > 0 && !state.NotesVisible {
		container.Add(textRow{
			Text:        s.config.Theme.Fg("muted", " ● "+T("review.note_label", reviewNoteLabel)),
			PaddingLeft: 1,
		})
		container.Add(textRow{
			Text:        "   " + s.config.Theme.Fg("muted", "→") + " " + s.config.Theme.Fg("text", globalNote),
			PaddingLeft: 1,
		})
	}
	return container
}

// BodyHeight mirrors bodyHeight.
func (s *SubmitTabStrategy) BodyHeight(width int, state QuestionnaireState) int {
	return len(s.BodyComponent(state).Render(width))
}

// MidRows mirrors midRows: the global-note editor while it is open.
func (s *SubmitTabStrategy) MidRows(state QuestionnaireState) []Component {
	if !state.NotesVisible {
		return nil
	}
	return []Component{
		textRow{Text: s.config.Theme.Fg("muted", T("notes.global_header", globalNotesHeader)), PaddingLeft: 1},
		editorRows{Editor: s.config.NotesInput},
		spacerRow{Lines: 1},
	}
}

// FooterRows mirrors footerRows: prompt row, picker (or padding), hint row.
func (s *SubmitTabStrategy) FooterRows(state QuestionnaireState) []Component {
	missing := make([]string, 0, len(s.config.Questions))
	for i := range s.config.Questions {
		question := s.config.Questions[i]
		if _, ok := state.Answers[i]; ok {
			continue
		}
		label := "Q" + itoa(i+1)
		if question.Header != "" {
			label = question.Header
		}
		missing = append(missing, label)
	}
	promptText := s.config.Theme.Fg("muted", T("review.ready", ReadyPrompt))
	if len(missing) > 0 {
		promptText = s.config.Theme.Fg("warning",
			T("review.incomplete", IncompleteWarningPrefix)+" "+strings.Join(missing, ", "))
	}
	out := []Component{
		spacerRow{Lines: 1},
		OneLineClippedText{Text: promptText, PaddingLeft: 1},
	}
	if s.config.SubmitPicker != nil {
		out = append(out, pickerRows{picker: s.config.SubmitPicker})
	} else {
		// Padding when the picker is not wired keeps the rendered count at 5.
		out = append(out, spacerRow{Lines: 1}, spacerRow{Lines: 1})
	}
	out = append(out, OneLineClippedText{
		Text:        s.config.Theme.Fg("dim", BuildSubmitHintText(state, s.config.NotesKey)),
		PaddingLeft: 1,
	})
	return out
}

// FocusedItemRowRange mirrors focusedItemRowRange: the Submit tab has no
// interactive body focus.
func (s *SubmitTabStrategy) FocusedItemRowRange(int, QuestionnaireState) *[2]int { return nil }

// pickerRows adapts SubmitPicker to Component.
type pickerRows struct{ picker *SubmitPicker }

// Render renders the picker.
func (p pickerRows) Render(width int) []string { return p.picker.Render(width) }

// stackBody is the Container equivalent: an ordered list of component rows.
type stackBody struct {
	children []Component
}

// Add appends a child.
func (b *stackBody) Add(child Component) { b.children = append(b.children, child) }

// Render renders every child in order.
func (b *stackBody) Render(width int) []string {
	out := make([]string, 0, len(b.children))
	for _, child := range b.children {
		out = append(out, child.Render(width)...)
	}
	return out
}

// FocusedItemRowRange reports no interactive focus.
func (b *stackBody) FocusedItemRowRange(int) [2]int { return [2]int{0, 0} }

// BuildHintText mirrors buildHintText. Order:
//
//	Enter · ↑/↓ [· Space toggle] [· n notes] [· Tab switch] · Esc [· <key> collapse]
//	[· Shift+Enter newline] [· Ctrl+U clear]
func BuildHintText(
	question *QuestionData,
	isMulti bool,
	state QuestionnaireState,
	collapseKey, notesKey string,
) string {
	parts := []string{T("hint.enter", HintPartEnter), T("hint.navigate", HintPartNav)}
	if question != nil && question.MultiSelect {
		parts = append(parts, T("hint.toggle", HintPartToggle))
	}
	if question != nil && !state.NotesVisible && !state.InputMode {
		parts = append(parts, notesHint("hint.notes", HintPartNotesTemplate, notesKey))
	}
	if isMulti {
		parts = append(parts, T("hint.tab", HintPartTab))
	}
	parts = append(parts, T("hint.cancel", HintPartCancel))
	if collapseKey != CollapseKeyOff {
		parts = append(parts, stringsReplace(
			T("hint.collapse", HintPartCollapseTemplate),
			KeyPlaceholder, FormatKeySpecForDisplay(collapseKey)))
	}
	if state.NotesVisible || state.InputMode {
		parts = append(parts, T("hint.newline", HintPartNewLine))
	}
	if state.InputMode {
		parts = append(parts, T("hint.clear", HintPartClear))
	}
	return strings.Join(parts, " · ")
}

// BuildSubmitHintText mirrors buildSubmitHintText.
func BuildSubmitHintText(state QuestionnaireState, notesKey string) string {
	parts := []string{T("hint.enter", HintPartEnter), T("hint.navigate", HintPartNav)}
	if !state.NotesVisible {
		parts = append(parts, notesHint("review.global_hint", reviewGlobalHint, notesKey))
	}
	parts = append(parts, T("hint.cancel", HintPartCancel))
	if state.NotesVisible {
		parts = append(parts, T("hint.newline", HintPartNewLine))
	}
	return strings.Join(parts, " · ")
}

// notesHint mirrors notesHint: a notes hint with the configured key in place of
// `{key}`.
func notesHint(key, template, notesKey string) string {
	return stringsReplace(T(key, template), KeyPlaceholder, FormatNotesKeyForDisplay(notesKey))
}
