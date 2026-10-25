package rpivaskuserquestion

// Ports view/dialog-copy.ts (v2.15.0): the footer hint copy and the review /
// collapse strings. This file imports nothing from the view layer, so
// dialog-builder and tab-content-strategy never form a cycle.

// Hint parts, verbatim from the TS constants.
const (
	HintPartEnter   = "Enter to select"
	HintPartNav     = "↑/↓ to navigate"
	HintPartNewLine = "Shift+Enter for newline"
	HintPartClear   = "Ctrl+U to clear"
	HintPartToggle  = "Space to toggle"
	HintPartTab     = "Tab to switch questions"
	HintPartCancel  = "Esc to cancel"
)

// KeyPlaceholder is the interpolation marker the configurable-key hints carry.
const KeyPlaceholder = "{key}"

// Collapse/expand and notes hint templates.
const (
	HintPartCollapseTemplate = KeyPlaceholder + " to collapse"
	HintPartExpandTemplate   = KeyPlaceholder + " to expand"
	HintPartNotesTemplate    = KeyPlaceholder + " to add notes"
)

// Review-tab copy.
const (
	ReviewHeading           = "Review your answers"
	ReadyPrompt             = "Ready to submit your answers?"
	IncompleteWarningPrefix = "⚠ Answer remaining questions before submitting:"
)

// HintPartNotes is the default-key (`n`) rendering of the notes template.
func HintPartNotes() string {
	return stringsReplace(HintPartNotesTemplate, KeyPlaceholder, FormatNotesKeyForDisplay(DefaultNotesKey))
}

// HintPartCollapse is the default-key (`Ctrl+]`) rendering of the collapse
// template.
func HintPartCollapse() string {
	return stringsReplace(HintPartCollapseTemplate, KeyPlaceholder, FormatKeySpecForDisplay(DefaultCollapseKey))
}

// HintSingle and HintMulti are the resting core hints for single- and
// multi-question resting states.
func HintSingle() string {
	return joinHintParts(HintPartEnter, HintPartNav, HintPartNotes(), HintPartCancel)
}

// HintMulti is the multi-question resting hint.
func HintMulti() string {
	return joinHintParts(HintPartEnter, HintPartNav, HintPartNotes(), HintPartTab, HintPartCancel)
}

// CollapsedHintTemplate is the single-line footer shown while collapsed.
func CollapsedHintTemplate() string {
	return joinHintParts(HintPartExpandTemplate, HintPartCancel)
}

// joinHintParts is `parts.join(" · ")` for the hint arrays.
func joinHintParts(parts ...string) string {
	var out string
	for i, part := range parts {
		if i > 0 {
			out += " · "
		}
		out += part
	}
	return out
}

// stringsReplace is strings.ReplaceAll, named so the template modules read the
// same as the TS `.replace(...)` calls they mirror.
func stringsReplace(text, old, replacement string) string {
	if old == "" || text == "" {
		return text
	}
	var out string
	for {
		index := indexOf(text, old)
		if index < 0 {
			return out + text
		}
		out += text[:index] + replacement
		text = text[index+len(old):]
	}
}

// indexOf is strings.Index without the import at every call site.
func indexOf(text, substr string) int {
	if substr == "" {
		return 0
	}
	last := len(text) - len(substr)
	if last < 0 {
		return -1
	}
	for i := 0; i <= last; i++ {
		if text[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
