package rpivaskuserquestion

// Ports view/tab-components.ts (v2.15.0): the per-tab component bundle and its
// body-height contract.

// TabBodyHeights is one tab's body height pair.
type TabBodyHeights struct {
	// Current is the height of the tab's current body content.
	Current int
	// Max is the worst-case height across every option of the tab.
	Max int
}

// TabComponents is one question tab's component bundle.
type TabComponents struct {
	OptionList  *OptionListView
	Preview     *PreviewPane
	MultiSelect *MultiSelectView
	BodyHeights func(width int) TabBodyHeights
}

// PreviewBodyHeights mirrors previewBodyHeights.
func PreviewBodyHeights(pane *PreviewPane) func(width int) TabBodyHeights {
	return func(width int) TabBodyHeights {
		current := pane.NaturalHeight(width)
		maximum := maxInt(current, pane.MaxNaturalHeight(width))
		return TabBodyHeights{Current: current, Max: maximum}
	}
}

// MultiSelectBodyHeights mirrors multiSelectBodyHeights.
func MultiSelectBodyHeights(view *MultiSelectView) func(width int) TabBodyHeights {
	return func(width int) TabBodyHeights {
		height := view.NaturalHeight(width)
		return TabBodyHeights{Current: height, Max: height}
	}
}
