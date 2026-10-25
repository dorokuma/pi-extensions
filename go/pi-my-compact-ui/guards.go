package compactui

import (
	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports src/helpers.ts' JS-compatibility helpers, the guard
// predicates of src/guards.ts (L1072-1111) and the key parsing of
// src/helpers.ts isCtrlI. The "instanceof" checks of the TS guards become
// concrete-type checks over the ported tree, because the ported components are
// plain structs rather than prototype subclasses of pi-tui classes.

// The key predicates delegate to the PiG SDK's pi-tui port
// (go/pigsdk/tui/keys.go), so the extension sees exactly the bytes the host
// sees: a hand-rolled CSI-u parser here once read `;3` (an Alt modifier) as a
// key release and rejected the modifier-only `Ctrl+Alt+I` the Kitty protocol
// really sends, which made Ctrl+I unreachable on some terminals.

// isKeyRelease is pi-tui's isKeyRelease: the terminal's key-release report.
// Kitty marks it with the `:3` event-type field, so a bare modifier such as
// `\x1b[105;7u` (Ctrl+Alt+I, no event type) is a press, not a release.
func isKeyRelease(data string) bool { return tui.IsKeyRelease(data) }

// keyIDs maps the pi-tui Key names the ported code compares against onto the
// SDK's canonical key identifiers. matchesKey and parseKey answer from the
// same table, so isCtrlI and the modal key handling share one spelling.
var keyIDs = map[string]tui.KeyID{
	"escape":    tui.Key.Escape,
	"enter":     tui.Key.Enter,
	"space":     tui.Key.Space,
	"tab":       tui.Key.Tab,
	"backspace": tui.Key.Backspace,
	"up":        tui.Key.Up,
	"down":      tui.Key.Down,
	"right":     tui.Key.Right,
	"left":      tui.Key.Left,
	"home":      tui.Key.Home,
	"end":       tui.Key.End,
	"pageup":    tui.Key.PageUp,
	"pagedown":  tui.Key.PageDown,
	"delete":    tui.Key.Delete,
	"ctrl+i":    tui.Key.Ctrl("i"),
}

// parseKey normalises one input chunk to a pi-tui key identifier, or "" when
// the chunk is not a single named key.
func parseKey(data string) string {
	if id, ok := tui.ParseKey(data); ok {
		return id
	}
	return ""
}

// matchesKey reports whether data is the named key.
func matchesKey(data, name string) bool {
	id, known := keyIDs[name]
	if !known {
		return false
	}
	return tui.MatchesKey(data, id)
}

// isContainer, isAssistantMessage, isToolExecution, isSpacer, isMarkdown,
// isText and isToolGroup are the guards of src/guards.ts (L1072-1111).
func isContainer(value component) bool {
	_, ok := value.(*container)
	return ok
}

func isAssistantMessage(value component) bool {
	_, ok := value.(*assistantMessageComponent)
	return ok
}

func isToolExecution(value component) bool {
	_, ok := value.(*toolComponent)
	return ok
}

func isSpacer(value component) bool {
	_, ok := value.(*spacer)
	return ok
}

func isMarkdown(value component) bool {
	_, ok := value.(*markdownComponent)
	return ok
}

func isText(value component) bool {
	_, ok := value.(*textComponent)
	return ok
}

func isToolGroup(value component) bool {
	_, ok := value.(*toolGroupComponent)
	return ok
}

// isVisibleTextMarkdown is isVisibleTextMarkdown (L408-410): a Markdown block
// that is not the default-styled thinking placeholder.
func isVisibleTextMarkdown(value component) bool {
	markdown, ok := value.(*markdownComponent)
	return ok && !markdown.defaultTextStyle
}

// hideGroupedToolRender is hideGroupedToolRender (L1084-1094): the TS patches
// ToolExecutionComponent.prototype.render so a grouped tool renders nothing.
// The port folds that into toolComponent.Render (component.go), so this
// function only records the patch state for the README mapping.
func hideGroupedToolRender(tool *toolComponent) {
	if tool == nil {
		return
	}
	// Nothing to install: Render already returns no lines while owner != nil.
}
