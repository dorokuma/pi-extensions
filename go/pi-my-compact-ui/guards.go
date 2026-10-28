package compactui

import (
	"strings"

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
// The port folds that into toolComponent.Render (component.go) for the
// in-process tree, and into toolRenderLines (extension.go) for the PiG host
// ToolRenderer path: non-display grouped cards return no lines so the host
// hides them (renderShell "self" + empty content).
func hideGroupedToolRender(tool *toolComponent) {
	if tool == nil {
		return
	}
	// Nothing to install on the prototype: host cards are hidden by
	// toolRenderLines when the tool has an owner and is not the group's
	// display tool. toolComponent.Render already returns nil while owner != nil.
}

// preparingToolCallIDPrefix marks a provisional tool call whose stream has not
// carried a real call id yet (TS L730 `preparing:${index}`). The host mounts a
// tool card in message_update only once the streamed block carries an id
// (interactive_events.go: MessageUpdateEvent with pta.id != ""), so a call with
// this placeholder has no card to paint with.
const preparingToolCallIDPrefix = "preparing:"

// hostCardForCallID reports whether the host has (or already mounted) a tool
// card for this call id.
func hostCardForCallID(id string) bool {
	return id != "" && !strings.HasPrefix(id, preparingToolCallIDPrefix)
}

// hasHostToolCard is hostCardForCallID for a tool component.
func hasHostToolCard(tool *toolComponent) bool {
	if tool == nil {
		return false
	}
	return hostCardForCallID(tool.toolCallID)
}

// displayToolLocked returns the tool component whose host card paints this
// group, or nil when no card can paint it. The caller holds mu.
//
// The newest visible *tool* entry owns the display rights, so newly added calls
// take the tree over as they stream in. A call that is still streaming lives
// only in the group entry (its component is not mounted in children yet), so the
// entry -- not the last mounted child -- decides who paints. The scan is a
// reverse walk, not a read at the last index: a thinking run that keeps
// streaming after the call, or an absorbed `cache miss` notice, is the newest
// visible entry while the tool that paints the tree is the one before it. Such
// an entry draws no card of its own, so stopping there left the group with no
// display card at all -- the whole tree disappeared -- or, with an older call
// mounted, took the tree back to that stale card.
func (g *toolGroupComponent) displayToolLocked() *toolComponent {
	if g == nil {
		return nil
	}
	visible := g.visibleEntriesLocked()
	for i := len(visible) - 1; i >= 0; i-- {
		entry := visible[i]
		if entry.kind != "tool" || entry.tool == nil {
			continue
		}
		// A stream that has not carried an id yet has no host card: letting the
		// placeholder take the display rights invalidated the card that painted
		// the tree while nothing could paint it in return.
		if !hasHostToolCard(entry.tool) {
			continue
		}
		return entry.tool
	}
	for i := len(g.children) - 1; i >= 0; i-- {
		child := g.children[i]
		if !hasHostToolCard(child) {
			continue
		}
		return child
	}
	return nil
}

// isGroupHostDisplayToolLocked reports whether this tool is the one host card
// that should paint the whole group (displayToolLocked). Only one card can
// answer true, so the tree is never painted twice.
func isGroupHostDisplayToolLocked(group *toolGroupComponent, tool *toolComponent) bool {
	if group == nil || tool == nil {
		return false
	}
	return group.displayToolLocked() == tool
}
