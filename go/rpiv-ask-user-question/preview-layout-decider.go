package rpivaskuserquestion

// Ports view/components/preview/preview-layout-decider.ts (v2.15.0): the pure
// layout/width maths for the side-by-side preview.

import "strings"

// Layout constants, verbatim from the TS module.
const (
	// PreviewMinWidth is the min terminal/pane width for side-by-side.
	PreviewMinWidth = 100
	// PreviewColumnGap is the visual gap between the two columns.
	PreviewColumnGap = 2
	// PreviewPaddingLeft is the 1-col padding inside the preview column.
	PreviewPaddingLeft = 1
	// StackedGapRows are the empty rows between options and preview in stacked
	// mode.
	StackedGapRows = 1

	// MinLeft is the floor for the adaptive left column width.
	MinLeft = 30
	// MaxLeftRatio caps the left column as a fraction of pane width.
	MaxLeftRatio = 0.5
	// MinPreviewWidth is the floor for the preview column width.
	MinPreviewWidth = 45
	// ConfirmedOverhead is visibleWidth(" ✔") = 2, reserved on the longest-label
	// measurement so a confirmed row is never truncated.
	ConfirmedOverhead = 2
)

// PreviewLayoutMode is "side-by-side" or "stacked".
type PreviewLayoutMode string

// The two layout modes.
const (
	LayoutSideBySide PreviewLayoutMode = "side-by-side"
	LayoutStacked    PreviewLayoutMode = "stacked"
)

// DecideLayout mirrors decideLayout: side-by-side only when both the terminal
// and the pane are wide enough. Pure of inputs, so the decision is computed
// once per render.
func DecideLayout(terminalWidth, paneWidth int) PreviewLayoutMode {
	if terminalWidth >= PreviewMinWidth && paneWidth >= PreviewMinWidth {
		return LayoutSideBySide
	}
	return LayoutStacked
}

// AdaptiveLeftWidth mirrors adaptiveLeftWidth: measure the longest visible
// label, clamp to the floor/ratio ceiling, and never exceed the width that
// leaves the preview its floor.
func AdaptiveLeftWidth(items []WrappingSelectItem, totalForNumbering, paneWidth int) int {
	prefixW := len(itoa(maxInt(1, totalForNumbering))) + 4 // digits + "❯ " + ". "
	maxLabel := 0
	for _, item := range items {
		if w := visibleWidth(item.Label); w > maxLabel {
			maxLabel = w
		}
	}
	desired := maxLabel + prefixW + ConfirmedOverhead
	ratioCapped := minInt(desired, int(float64(paneWidth)*MaxLeftRatio))
	available := paneWidth - PreviewColumnGap - MinPreviewWidth
	return maxInt(MinLeft, minInt(ratioCapped, maxInt(1, available)))
}

// CrossTabMaxLeftWidth mirrors crossTabMaxLeftWidth: the widest adaptive left
// column across every tab, so the options column stays stable on tab switch.
func CrossTabMaxLeftWidth(tabs []QuestionData, itemsByTab [][]WrappingSelectItem, paneWidth int) int {
	maximum := MinLeft
	for i := range tabs {
		var items []WrappingSelectItem
		if i < len(itemsByTab) {
			items = itemsByTab[i]
		}
		if width := AdaptiveLeftWidth(items, len(items), paneWidth); width > maximum {
			maximum = width
		}
	}
	return maximum
}

// PreviewSourceWidth mirrors previewSourceWidth: the widest source line across
// all options' previews, or 0 when no option carries one.
func PreviewSourceWidth(question QuestionData) int {
	maximum := 0
	for _, option := range question.Options {
		text := option.previewText()
		if text == "" {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			if w := visibleWidth(line); w > maximum {
				maximum = w
			}
		}
	}
	return maximum
}

// CrossTabPreviewBudget mirrors crossTabPreviewBudget: each question's preview
// appetite plus box/padding overhead, floored at MinPreviewWidth.
func CrossTabPreviewBudget(questions []QuestionData, paneWidth int) int {
	maximum := MinPreviewWidth
	for _, question := range questions {
		rawWidth := PreviewSourceWidth(question)
		capped := minInt(rawWidth, paneWidth-PreviewColumnGap-MinLeft)
		budget := capped + BorderHorizontalOverhead + 2*BorderInnerPaddingHorizontal + PreviewPaddingLeft
		if budget > maximum {
			maximum = budget
		}
	}
	return maximum
}

// CrossTabLeftWidthWithDonation mirrors crossTabLeftWidthWithDonation: combine
// the label-driven width with the slack donated by narrow previews, capped by
// the tighter of the preview-width safety limit and MaxLeftRatio.
func CrossTabLeftWidthWithDonation(
	tabs []QuestionData,
	itemsByTab [][]WrappingSelectItem,
	questions []QuestionData,
	paneWidth int,
) int {
	labelDriven := CrossTabMaxLeftWidth(tabs, itemsByTab, paneWidth)
	previewBudget := CrossTabPreviewBudget(questions, paneWidth)
	slackDonation := paneWidth - PreviewColumnGap - previewBudget
	previewSafetyCeiling := paneWidth - PreviewColumnGap - MinPreviewWidth
	ratioCeiling := int(float64(paneWidth) * MaxLeftRatio)
	ceiling := minInt(previewSafetyCeiling, ratioCeiling)
	return minInt(maxInt(labelDriven, slackDonation), maxInt(1, ceiling))
}

// ColumnWidths is columnWidths' result.
type ColumnWidths struct {
	LeftWidth  int
	RightWidth int
	Gap        int
}

// ColumnWidths mirrors columnWidths: split the pane for side-by-side, keeping
// both columns at least 1 col.
func ColumnWidthsFor(paneWidth, adaptiveLeft int) ColumnWidths {
	gap := PreviewColumnGap
	leftWidth := minInt(adaptiveLeft, maxInt(1, paneWidth-gap-1))
	rightWidth := maxInt(1, paneWidth-leftWidth-gap)
	return ColumnWidths{LeftWidth: leftWidth, RightWidth: rightWidth, Gap: gap}
}

// BodyWidths is bodyWidths' result.
type BodyWidths struct {
	OptionsWidth int
	PreviewWidth int
}

// BodyWidthsFor mirrors bodyWidths: the widths actually passed to the option
// list and the preview block. Stacked uses the full pane for both.
func BodyWidthsFor(paneWidth int, mode PreviewLayoutMode, adaptiveLeft int) BodyWidths {
	if mode == LayoutStacked {
		return BodyWidths{OptionsWidth: paneWidth, PreviewWidth: paneWidth}
	}
	columns := ColumnWidthsFor(paneWidth, adaptiveLeft)
	return BodyWidths{
		OptionsWidth: columns.LeftWidth,
		PreviewWidth: maxInt(1, columns.RightWidth-PreviewPaddingLeft),
	}
}
