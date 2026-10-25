package rpivaskuserquestion

// Ports view/components/preview/preview-block-renderer.ts (v2.15.0): the bordered
// markdown preview block for one question.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Affordance templates. `{key}` is replaced by the configured notes key so
// per-locale word order is preserved.
const (
	NotesAffordanceTemplate     = "Notes: press {key} to add notes"
	NotesEditAffordanceTemplate = "Note saved: press {key} to edit"

	keyPlaceholder = "{key}"
)

// NotesAffordance is which affordance the block shows under the preview.
type NotesAffordance string

// The three affordance states.
const (
	AffordanceAdd  NotesAffordance = "add"
	AffordanceEdit NotesAffordance = "edit"
	AffordanceNone NotesAffordance = "none"
)

// AffordanceText builds one affordance line with the configured notes key.
func AffordanceText(affordance NotesAffordance, notesKey string) string {
	template := NotesAffordanceTemplate
	if affordance == AffordanceEdit {
		template = NotesEditAffordanceTemplate
	}
	text := T("preview.notes_affordance", template)
	if affordance == AffordanceEdit {
		text = T("preview.notes_edit_affordance", NotesEditAffordanceTemplate)
	}
	return strings.ReplaceAll(text, keyPlaceholder, FormatNotesKeyForDisplay(notesKey))
}

// contentBudgetFor mirrors contentBudgetFor: the layout's preview cap minus the
// border and affordance overhead.
func contentBudgetFor(mode PreviewLayoutMode) int {
	capRows := MaxPreviewHeightStacked
	if mode == LayoutSideBySide {
		capRows = MaxPreviewHeightSideBySide
	}
	return maxInt(1, capRows-BorderVerticalOverhead-NotesAffordanceOverhead)
}

// blockFit is fitBlock's result.
type blockFit struct {
	ContentRows int
	Affordance  bool
}

// fitBlock mirrors fitBlock: cap the content by the layout budget and keep the
// affordance; under a tighter allowance drop the affordance first, then shrink
// the content so the box still closes with its "N lines hidden" count.
func fitBlock(rawRows int, mode PreviewLayoutMode, maxRows *int) (blockFit, bool) {
	contentRows := minInt(rawRows, contentBudgetFor(mode))
	if maxRows == nil || BorderVerticalOverhead+contentRows+NotesAffordanceOverhead <= *maxRows {
		return blockFit{ContentRows: contentRows, Affordance: true}, true
	}
	fitted := minInt(contentRows, *maxRows-BorderVerticalOverhead)
	if fitted >= 1 {
		return blockFit{ContentRows: fitted}, true
	}
	return blockFit{}, false
}

// innerWidthFor mirrors innerWidthFor: the padding-aware content width.
func innerWidthFor(width int) int {
	return maxInt(1, width-BorderHorizontalOverhead-2*BorderInnerPaddingHorizontal)
}

// PreviewBlockRenderer renders the bordered markdown preview block for a single
// question. It is NOT a component — a pure render-and-measure helper consumed by
// PreviewPane.
type PreviewBlockRenderer struct {
	theme    sdk.UITheme
	cache    *MarkdownContentCache
	notesKey string
}

// NewPreviewBlockRenderer constructs the renderer.
func NewPreviewBlockRenderer(question QuestionData, theme sdk.UITheme, notesKey string) *PreviewBlockRenderer {
	return &PreviewBlockRenderer{
		theme:    theme,
		notesKey: notesKey,
		cache:    NewMarkdownContentCache(question, theme),
	}
}

// HasAnyPreview mirrors hasAnyPreview.
func (r *PreviewBlockRenderer) HasAnyPreview() bool { return r.cache.HasAnyPreview() }

// Has mirrors has(optionIndex).
func (r *PreviewBlockRenderer) Has(optionIndex int) bool { return r.cache.Has(optionIndex) }

// Invalidate drops the markdown cache.
func (r *PreviewBlockRenderer) Invalidate() { r.cache.Invalidate() }

// BlockHeight mirrors blockHeight: always equal to RenderBlock's length for the
// same arguments.
func (r *PreviewBlockRenderer) BlockHeight(width, optionIndex int, mode PreviewLayoutMode, maxRows *int) int {
	rawRows := len(r.cache.BodyFor(optionIndex, innerWidthFor(width)))
	fit, ok := fitBlock(rawRows, mode, maxRows)
	if !ok {
		return 0
	}
	height := BorderVerticalOverhead + fit.ContentRows
	if fit.Affordance {
		height += NotesAffordanceOverhead
	}
	return height
}

// RenderBlock mirrors renderBlock: the bordered box, then (when it fits) a blank
// separator and the notes-affordance row.
func (r *PreviewBlockRenderer) RenderBlock(
	width, optionIndex int,
	mode PreviewLayoutMode,
	affordance NotesAffordance,
	maxRows *int,
) []string {
	maxInnerWidth := innerWidthFor(width)
	raw := r.cache.BodyFor(optionIndex, maxInnerWidth)
	fit, ok := fitBlock(len(raw), mode, maxRows)
	if !ok {
		return nil
	}
	hidden := len(raw) - fit.ContentRows
	contentLines := raw
	if hidden > 0 {
		contentLines = raw[:fit.ContentRows]
	}

	dimensions := ComputeBoxDimensions(contentLines, maxInnerWidth)
	colorFn := func(text string) string { return r.theme.Fg("accent", text) }
	boxed := RenderBorderedBox(contentLines, dimensions.BoxWidth, colorFn, hidden)
	if !fit.Affordance {
		return boxed
	}

	text := ""
	if affordance != AffordanceNone && r.cache.Has(optionIndex) {
		text = AffordanceText(affordance, r.notesKey)
	}
	if text == "" {
		return append(boxed, "", "")
	}
	return append(boxed, "", r.theme.Fg("muted", text))
}
