package compactui

import (
	"strconv"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// groupEntry is one row of a group: a tool, a thinking run, a notice or a
// provisional (still streaming) tool call, in the order it streamed in
// (L822-826). Exactly one of the four pointers is set, which mirrors the TS
// discriminated union.
type groupEntry struct {
	kind      string // "tool" | "thinking" | "notice"
	tool      *toolComponent
	think     *thinkingEntry
	notice    *noticeEntry
	preparing *preparingTool
}

// toolGroupComponent is PT's ToolGroupComponent (L808-1071): the tree-shaped
// block that folds thinking runs and tool calls into one compact subtree.
type toolGroupComponent struct {
	tui        *runtimeState
	toolCallID string
	toolName   string
	// anchored marks a group nested at a visible-text boundary (L815).
	anchored bool
	// sealed marks a group closed by real text output: no further entries (L817).
	sealed  bool
	entries []groupEntry
	// children are the tool components, as Container.children holds them.
	children []*toolComponent
	// mount records the container the group was last mounted in (GROUP_MOUNT).
	mount *container
	// rendered caches the last rendered lines (Container caches too).
	rendered []string
	// renderedWidth / renderedSpinIndex key the cache so an animation tick that
	// only advances the spinner frame rebuilds when the glyph changes, and a
	// host Working-tick re-render with the same frame is O(1).
	renderedWidth     int
	renderedSpinIndex int
	renderedAnchored  bool
}

// newToolGroup builds a group. TS names it
// `compact-group-${Date.now()}-${Math.random().toString(36).slice(2,7)}`; the
// port keeps the same shape with a monotonic sequence for the random suffix.
func (r *runtimeState) newToolGroup(nowMs int64) *toolGroupComponent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.newToolGroupLocked(nowMs)
}

// newToolGroupLocked is newToolGroup with the caller's lock held.
func (r *runtimeState) newToolGroupLocked(nowMs int64) *toolGroupComponent {
	r.groupSeq++
	return &toolGroupComponent{
		tui:        r,
		toolCallID: "compact-group-" + strconv.FormatInt(nowMs, 10) + "-" + base36(r.groupSeq),
		toolName:   "group",
	}
}

// invalidateGroupCards is the one channel through which a change in a group's
// visible state reaches the host, and the reason the port has no scattered
// hand-written invalidate collections left.
//
// A host ToolExecutionComponent keeps the frame it last painted until the card
// is dirty or its Invalidate hook fires (tui/tool_execution.go: Render reuses
// definitionCall until Invalidate). A card that is neither the group's display
// tool nor animating is never dirty, so sealing a group, marking a call
// interrupted, changing a token count or adding/removing an entry is invisible
// on screen without this call -- the failure brief2 R4 A is about: the tree
// header stayed at "waiting for response..." for a whole turn.
//
// It takes mu itself, because the callbacks siblingCardInvalidatesExceptLocked
// returns are host calls and must not run under mu. A caller that already holds
// mu therefore collects the groups it changed and calls this after unlocking
// (see handleAgentEnd / sealHistoricalGroupsFrom / handleMessageStart).
func (r *runtimeState) invalidateGroupCards(group *toolGroupComponent) {
	r.invalidateGroupCardsExcept(group, "")
}

// invalidateGroupCardsExcept is invalidateGroupCards for a caller that wants one
// card left alone: keepID names a card whose own render path repaints it in the
// same frame anyway (a newly added entry, a call taking the display rights), so
// invalidating it would only queue a redundant host repaint.
func (r *runtimeState) invalidateGroupCardsExcept(group *toolGroupComponent, keepID string) {
	if group == nil {
		return
	}
	r.mu.Lock()
	stale := group.siblingCardInvalidatesExceptLocked(keepID)
	tui := r.capturedTui
	r.mu.Unlock()
	group.invalidate()
	for _, invalidate := range stale {
		invalidate()
	}
	// No card hook is registered for this group (a thinking-only group that no
	// host card paints yet, or a test without a host): fall back to a frame
	// request so the state change still reaches the screen.
	if len(stale) == 0 && keepID == "" {
		requestRender(tui)
	}
}

// appendChangedGroup records a group whose visible state changed, once, so a
// caller that has to release mu before repainting collects one entry per group
// no matter how many of its entries it touched (the group list is tiny, so the
// scan is cheaper than a map and keeps the order stable).
func appendChangedGroup(list []*toolGroupComponent, group *toolGroupComponent) []*toolGroupComponent {
	if group == nil {
		return list
	}
	for _, existing := range list {
		if existing == group {
			return list
		}
	}
	return append(list, group)
}

// addTool is addTool (L838-853): absorb a provisional entry, hide the tool's
// own render, and append it in stream order.
//
// It takes mu itself: every caller outside this file's locked helpers calls
// addTool without a lock, and nowMs would deadlock under one.
func (g *toolGroupComponent) addTool(tool *toolComponent) {
	now := g.tui.nowMs()
	keepID := ""
	if tool != nil {
		keepID = tool.toolCallID
	}
	g.tui.mu.Lock()
	g.addToolAt(tool, now)
	g.tui.mu.Unlock()
	// The new entry takes the display rights, so its own card repaints from the
	// dirty state; only the siblings that yielded those rights need a nudge.
	g.tui.invalidateGroupCardsExcept(g, keepID)
}

// siblingCardInvalidatesExceptLocked returns host Invalidate callbacks for
// every grouped tool card except keepID. The caller holds mu and must invoke
// the callbacks only after unlocking (host notify must not run under mu).
// Entries are walked, not just children: a call that is still streaming has
// its toolComponent only in the group entry (nothing has mounted it in
// children yet), and its host card already exists and paints.
func (g *toolGroupComponent) siblingCardInvalidatesExceptLocked(keepID string) []func() {
	if g == nil || g.tui == nil {
		return nil
	}
	seen := make(map[string]bool, len(g.children)+len(g.entries))
	out := make([]func(), 0, len(g.children)+len(g.entries))
	collect := func(id string) {
		if id == "" || id == keepID || seen[id] {
			return
		}
		seen[id] = true
		if fn := g.tui.toolCardInvalidate[id]; fn != nil {
			out = append(out, fn)
		}
	}
	for _, entry := range g.entries {
		if entry.kind == "tool" && entry.tool != nil {
			collect(entry.tool.toolCallID)
		}
	}
	for _, child := range g.children {
		if child != nil {
			collect(child.toolCallID)
		}
	}
	return out
}

// displayCardIDLocked is the tool call id of the host card that paints this
// group (displayToolLocked): the newest visible tool entry that has a host card
// -- a streaming entry lives only in entries, nothing has mounted it in children
// yet -- with the last mounted card as the fallback. The caller holds mu.
func (g *toolGroupComponent) displayCardIDLocked() string {
	if display := g.displayToolLocked(); display != nil {
		return display.toolCallID
	}
	return ""
}

// addToolAt is addTool for a caller that already holds mu and has read the
// clock.
func (g *toolGroupComponent) addToolAt(tool *toolComponent, now int64) {
	g.tui.absorbPreparingToolLocked(tool)
	hideGroupedToolRender(tool)
	if g.tui.interruptedToolCallID[tool.toolCallID] {
		tool.interrupted = true
	}
	if tool.result == nil && !g.tui.hasDurationLocked(tool.toolCallID) {
		g.tui.rememberToolStartLocked(tool.toolCallID, now)
	}
	g.children = append(g.children, tool)
	if tool.groupedAt == nil {
		tool.groupedAt = &now
	}
	tool.owner = g
	g.entries = append(g.entries, groupEntry{kind: "tool", tool: tool})
	g.invalidateLocked()
}

// addThinking is addThinking (L854-859): idempotent, a run belongs to exactly
// one group. It takes mu itself.
func (g *toolGroupComponent) addThinking(entry *thinkingEntry) {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	g.addThinkingLocked(entry)
}

// addThinkingLocked is addThinking for a caller that already holds mu.
func (g *toolGroupComponent) addThinkingLocked(entry *thinkingEntry) {
	if entry.owner != nil {
		return
	}
	entry.owner = g
	g.entries = append(g.entries, groupEntry{kind: "thinking", think: entry})
	g.invalidateLocked()
}

// addNotice is addNotice (L860-865). It takes mu itself.
func (g *toolGroupComponent) addNotice(entry *noticeEntry) {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	g.addNoticeLocked(entry)
}

// addNoticeLocked is addNotice for a caller that already holds mu.
func (g *toolGroupComponent) addNoticeLocked(entry *noticeEntry) {
	if entry.owner != nil && entry.owner != g {
		return
	}
	entry.owner = g
	g.entries = append(g.entries, groupEntry{kind: "notice", notice: entry})
	g.invalidateLocked()
}

// hasChildLocked reports whether the group already lists tool in children. The
// caller holds mu.
func (g *toolGroupComponent) hasChildLocked(tool *toolComponent) bool {
	for _, child := range g.children {
		if child == tool {
			return true
		}
	}
	return false
}

// removeTool is removeTool (L866-874). It takes mu itself and routes the
// resulting repaint through invalidateGroupCards: dropping an entry changes
// what the tree shows, and a card that keeps its last frame would still list
// the removed row.
func (g *toolGroupComponent) removeTool(tool *toolComponent) {
	g.tui.mu.Lock()
	g.removeToolLocked(tool)
	g.tui.mu.Unlock()
	g.tui.invalidateGroupCards(g)
}

// removeToolLocked is removeTool for a caller that already holds mu. It only
// drops the entries: the caller has to route the repaint through
// invalidateGroupCards after releasing mu (removeTool, removeChildFrom and
// absorbGroupedPreparingLocked all do).
func (g *toolGroupComponent) removeToolLocked(tool *toolComponent) {
	for i, child := range g.children {
		if child == tool {
			g.children = append(g.children[:i], g.children[i+1:]...)
			break
		}
	}
	for i, entry := range g.entries {
		if entry.kind == "tool" && entry.tool == tool {
			g.entries = append(g.entries[:i], g.entries[i+1:]...)
			break
		}
	}
	if tool != nil && tool.owner == g {
		tool.owner = nil
	}
}

// visibleEntriesLocked is getVisibleEntries (L875-887) for a caller that holds
// mu: tools and notices always, thinking runs only while active or non-empty.
func (g *toolGroupComponent) visibleEntriesLocked() []groupEntry {
	out := make([]groupEntry, 0, len(g.entries))
	for _, entry := range g.entries {
		switch entry.kind {
		case "tool", "notice":
			out = append(out, entry)
		case "thinking":
			if entry.think.active || strings.TrimSpace(entry.think.text) != "" {
				out = append(out, entry)
			}
		}
	}
	return out
}

// visibleEntries is visibleEntriesLocked for a caller outside mu.
func (g *toolGroupComponent) visibleEntries() []groupEntry {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.visibleEntriesLocked()
}

// hasVisibleEntries is hasVisibleEntries (L888-890).
func (g *toolGroupComponent) hasVisibleEntries() bool {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.hasVisibleEntriesLocked()
}

// hasVisibleEntriesLocked is hasVisibleEntries for a caller that holds mu.
func (g *toolGroupComponent) hasVisibleEntriesLocked() bool {
	return len(g.visibleEntriesLocked()) > 0
}

// hasPending is hasPending (L891-893).
func (g *toolGroupComponent) hasPending() bool { return g.hasPendingLocked() }

// hasPendingLocked is hasPending for a caller that holds mu.
func (g *toolGroupComponent) hasPendingLocked() bool {
	for _, entry := range g.visibleEntriesLocked() {
		if entry.kind == "tool" && toolStatus(entry.tool, g) == "pending" {
			return true
		}
	}
	return false
}

// hasActiveThinking is hasActiveThinking (L894-896).
func (g *toolGroupComponent) hasActiveThinking() bool {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.hasActiveThinkingLocked()
}

// hasActiveThinkingLocked is hasActiveThinking for a caller that holds mu.
func (g *toolGroupComponent) hasActiveThinkingLocked() bool {
	for _, entry := range g.visibleEntriesLocked() {
		if entry.kind == "thinking" && entry.think.active {
			return true
		}
	}
	return false
}

// hasThinking is hasThinking (L897-899).
func (g *toolGroupComponent) hasThinking() bool {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.hasThinkingLocked()
}

// hasThinkingLocked is hasThinking for a caller that holds mu.
func (g *toolGroupComponent) hasThinkingLocked() bool {
	for _, entry := range g.visibleEntriesLocked() {
		if entry.kind == "thinking" {
			return true
		}
	}
	return false
}

// hasTools is hasTools (L900-902).
func (g *toolGroupComponent) hasTools() bool {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.hasToolsLocked()
}

// hasToolsLocked is hasTools for a caller that holds mu.
func (g *toolGroupComponent) hasToolsLocked() bool {
	for _, entry := range g.visibleEntriesLocked() {
		if entry.kind == "tool" {
			return true
		}
	}
	return false
}

// workPhaseLabel is workPhaseLabel (L903-916): only the latest open group
// reflects work between streamed entries.
func (g *toolGroupComponent) workPhaseLabel() (string, bool) {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.workPhaseLabelLocked()
}

// workPhaseLabelLocked is workPhaseLabel for a caller that holds mu.
func (g *toolGroupComponent) workPhaseLabelLocked() (string, bool) {
	phase := g.tui.agentWorkPhaseLocked()
	if g.sealed || g.tui.lastActiveGroupLocked() != g || g.hasPendingLocked() || g.hasActiveThinkingLocked() {
		return "", false
	}
	if phase == nil {
		return "", false
	}
	switch *phase {
	case phaseWaitingResponse:
		return "waiting for response...", true
	case phaseWaitingOutput:
		return "waiting for next output...", true
	case phaseWorking:
		return "working...", true
	}
	return "", false
}

// needsAnimation is needsAnimation (L917-920).
func (g *toolGroupComponent) needsAnimation() bool {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.needsAnimationLocked()
}

// needsAnimationLocked is needsAnimation for a caller that holds mu.
func (g *toolGroupComponent) needsAnimationLocked() bool {
	if g.sealed {
		return false
	}
	if !g.hasVisibleEntriesLocked() {
		return false
	}
	if g.hasPendingLocked() || g.hasActiveThinkingLocked() {
		return true
	}
	if _, ok := g.workPhaseLabelLocked(); ok {
		return true
	}
	return !g.hasToolsLocked() && g.tui.agentWorkPhaseLocked() != nil
}

// invalidate is invalidate (L836-837): the TS drops the cached lines; the port
// clears the cached render. rendered is shared with the renderer, so it is
// written under mu; invalidateLocked is the form a caller already holding mu
// uses.
func (g *toolGroupComponent) invalidate() {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	g.invalidateLocked()
}

// invalidateLocked is invalidate for a caller that holds mu.
func (g *toolGroupComponent) invalidateLocked() {
	g.rendered = nil
	g.renderedWidth = 0
	g.renderedSpinIndex = -1
}

// Render implements component.
func (g *toolGroupComponent) Render(width int) []string { return g.renderWidth(width) }

// Invalidate implements component.
func (g *toolGroupComponent) Invalidate() { g.invalidate() }

// iconFor is iconFor (L921-925).
func (g *toolGroupComponent) iconFor(tool *toolComponent, frame string) string {
	switch toolStatus(tool, g) {
	case "pending":
		return frame
	case "error":
		return "✗"
	default:
		return "✓"
	}
}

// colorFor is colorFor (L926-928).
func (g *toolGroupComponent) colorFor(status string) string {
	switch status {
	case "pending":
		return "accent"
	case "error":
		return "error"
	default:
		return "success"
	}
}

// toolRow is toolRow (L930-953): bold accent tool name, dim payload, elapsed
// right-aligned to width. The whole read runs under mu, because the tool's
// status, arguments and elapsed time are shared with the event handlers;
// toolRowLocked is the form a caller already holding mu uses.
func (g *toolGroupComponent) toolRow(rail string, tool *toolComponent, frame string, width int, isDim bool) string {
	g.tui.mu.Lock()
	defer g.tui.mu.Unlock()
	return g.toolRowLocked(rail, tool, frame, width, isDim)
}

// toolRowLocked is toolRow for a caller that already holds mu.
func (g *toolGroupComponent) toolRowLocked(rail string, tool *toolComponent, frame string, width int, isDim bool) string {
	th := g.tui.themeLocked()
	status := toolStatus(tool, g)
	summary := toolSummary(tool.toolName, tool.args)
	prefix := ""
	if rail != "" {
		prefix = fgOf(th, "dim", rail)
	}
	var leftHeader string
	if isDim {
		leftHeader = prefix + fgOf(th, "dim", g.iconFor(tool, frame)) + " " + fgOf(th, "dim", summary.name)
	} else {
		leftHeader = prefix + fgOf(th, g.colorFor(status), g.iconFor(tool, frame)) + " " + fgOf(th, "toolTitle", boldOf(th, summary.name))
	}
	rightColor := "muted"
	if isDim {
		rightColor = "dim"
	}
	right := fgOf(th, rightColor, "("+g.tui.toolElapsedLocked(tool)+")")
	rightLen := tui.VisibleWidth(right)
	headerLen := tui.VisibleWidth(leftHeader)
	if summary.content != "" {
		maxContentLen := width - headerLen - 1 - rightLen - 1
		if maxContentLen > 3 {
			truncated := tui.TruncateToWidth(summary.content, maxContentLen, fgOf(th, "dim", "..."), false)
			left := leftHeader + " " + fgOf(th, "dim", truncated)
			gap := maxInt(1, width-tui.VisibleWidth(left)-rightLen)
			return left + strings.Repeat(" ", gap) + right
		}
	}
	gap := maxInt(1, width-headerLen-rightLen)
	return leftHeader + strings.Repeat(" ", gap) + right
}

// noticeRow is noticeRow (L954-967): the absorbed `cache miss` notice row.
func (g *toolGroupComponent) noticeRow(rail string, entry *noticeEntry, width int, isDim bool) string {
	th := g.tui.themeLocked()
	prefix := ""
	if rail != "" {
		prefix = fgOf(th, "dim", rail)
	}
	iconColor := "warning"
	titleColor := "toolTitle"
	if isDim {
		iconColor = "dim"
		titleColor = "dim"
	}
	icon := fgOf(th, iconColor, "!")
	title := fgOf(th, titleColor, "cache miss")
	content := noticeContent(entry.text)
	line := prefix + icon + " " + title
	if content != "" {
		line += " " + fgOf(th, "dim", content)
	}
	return tui.TruncateToWidth(line, width, fgOf(th, "dim", "..."), false)
}

// noticeContent is the notice body cleanup noticeRow and the modals share
// (L963, L1723, L2005).
func noticeContent(text string) string {
	out := stripCacheMissPrefix(text)
	out = strings.ReplaceAll(out, ":", "")
	return strings.TrimSpace(out)
}

// thinkingRow is thinkingRow (L968-1005): one row per thinking run, spinner
// while streaming, token label right-aligned.
func (g *toolGroupComponent) thinkingRow(rail string, entry *thinkingEntry, width int, isDim bool) string {
	th := g.tui.themeLocked()
	var spin *spinnerFrame
	if entry.active {
		frame := thinkingSpinnerFrame(g.tui.nowMsLocked(), g.tui.spinnerStartMs)
		spin = &frame
	}
	icon := "✓"
	iconColor := "thinkingText"
	if spin != nil {
		icon = spin.frame
		iconColor = spin.color
	}
	if isDim {
		iconColor = "dim"
	}
	prefix := ""
	if rail != "" {
		prefix = fgOf(th, "dim", rail)
	}
	preview := stripThinkingMarkup(strings.TrimSpace(entry.text))
	if preview == "" {
		label := "thinking"
		if entry.active {
			label = "thinking..."
		}
		titleColor := "toolTitle"
		if isDim {
			titleColor = "dim"
		}
		return prefix + fgOf(th, iconColor, icon) + " " + fgOf(th, titleColor, label)
	}
	titleColor := "toolTitle"
	if isDim {
		titleColor = "dim"
	}
	leftHeader := prefix + fgOf(th, iconColor, icon) + " " + fgOf(th, titleColor, "thinking")
	tokenLabel := thinkingTokenLabel(entry)
	rightColor := "muted"
	if isDim {
		rightColor = "dim"
	}
	right := fgOf(th, rightColor, tokenLabel)
	rightLen := tui.VisibleWidth(right)
	headerLen := tui.VisibleWidth(leftHeader)
	maxPreviewLen := width - headerLen - 1 - rightLen - 1
	if maxPreviewLen > 3 {
		contentColor := "thinkingText"
		if isDim {
			contentColor = "dim"
		}
		ellipsis := fgOf(th, contentColor, "...")
		truncated := tui.TruncateToWidth(strings.Join(strings.Fields(preview), " "), maxPreviewLen, ellipsis, false)
		left := leftHeader + " " + fgOf(th, contentColor, truncated)
		gap := maxInt(1, width-tui.VisibleWidth(left)-rightLen)
		return left + strings.Repeat(" ", gap) + right
	}
	gap := maxInt(1, width-headerLen-rightLen)
	return leftHeader + strings.Repeat(" ", gap) + right
}

// stripThinkingMarkup removes the markdown emphasis markers from a preview
// (`text.replace(/[*_#`>]+/g, "")`).
func stripThinkingMarkup(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch r {
		case '*', '_', '#', '`', '>':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renderCollapsed is renderCollapsed (L1006-1071): header plus one line per
// entry in stream order, folding the oldest entries past the configured cap.
//
// PiG-path deviation from TS L993 ("when there is only 1 visible entry, do not
// show hierarchy"): on this path the group is painted through the host tool
// card, and the TS bare row is indistinguishable from a card that never
// joined the compact framework -- exactly the first-frame jump brief2 is
// about. A lone visible entry therefore keeps the tree form (header + rail),
// so every entry a group shows is inside the framework from its first frame.
// The caller holds mu.
func (g *toolGroupComponent) renderCollapsed(width int, visible []groupEntry) []string {
	th := g.tui.themeLocked()
	frame := toolSpinnerFrame(g.tui.nowMsLocked(), g.tui.spinnerStartMs)
	workPhase, hasWorkPhase := g.workPhaseLabelLocked()
	lines := make([]string, 0, len(visible)+1)
	pendingTools := make([]groupEntry, 0, len(visible))
	for _, entry := range visible {
		if entry.kind == "tool" && toolStatus(entry.tool, g) == "pending" {
			pendingTools = append(pendingTools, entry)
		}
	}
	hasPendingTool := len(pendingTools) > 0
	receivingToolArgs := hasPendingTool
	for _, entry := range pendingTools {
		if entry.tool.executionStarted || !toolReceivingArgs(entry.tool) {
			receivingToolArgs = false
			break
		}
	}
	isThinking := !g.sealed && anyEntry(visible, func(e groupEntry) bool { return e.kind == "thinking" && e.think.active })
	hasActiveThinkingTokens := anyEntry(visible, func(e groupEntry) bool {
		return e.kind == "thinking" && e.think.active && (e.think.tokens > 0 || strings.TrimSpace(e.think.text) != "")
	})
	hasTools := anyEntry(visible, func(e groupEntry) bool { return e.kind == "tool" })
	hasThinking := anyEntry(visible, func(e groupEntry) bool { return e.kind == "thinking" })
	openNoTools := !g.sealed && !hasTools
	working := hasPendingTool || isThinking || openNoTools || hasWorkPhase

	state := ""
	stateColor := ""
	switch {
	case hasPendingTool:
		if receivingToolArgs {
			state = "tool call receiving..."
		} else {
			state = "tool calling..."
		}
		stateColor = "accent"
	case hasWorkPhase:
		state = workPhase
		stateColor = "thinkingText"
	case !hasTools:
		if working {
			if hasActiveThinkingTokens {
				state = "thinking..."
			} else {
				state = "waiting for first token..."
			}
			stateColor = "thinkingText"
		} else {
			state = "thinking"
			stateColor = "thinkingText"
		}
	case isThinking:
		if hasActiveThinkingTokens {
			state = "thinking..."
		} else {
			state = "waiting for first token..."
		}
		stateColor = "thinkingText"
	default:
		if hasThinking {
			state = "done"
		} else {
			state = "tools done"
		}
		stateColor = "success"
	}

	leftIcon := "✓"
	leftColor := stateColor
	if working {
		spin := thinkingSpinnerFrame(g.tui.nowMsLocked(), g.tui.spinnerStartMs)
		leftIcon = spin.frame
		leftColor = spin.color
	}
	maxEntries := g.tui.maxGroupEntriesLocked()
	hiddenCount := 0
	if len(visible) > maxEntries {
		hiddenCount = len(visible) - maxEntries
	}
	hiddenSuffix := ""
	if hiddenCount > 0 {
		hiddenSuffix = " " + fgOf(th, "muted", "("+strconv.Itoa(hiddenCount)+" hidden)")
	}
	lines = append(lines, fgOf(th, leftColor, leftIcon)+" "+fgOf(th, stateColor, state)+hiddenSuffix)
	entriesToRender := visible
	if hiddenCount > 0 {
		entriesToRender = visible[hiddenCount:]
	}
	total := len(entriesToRender)
	for index := 0; index < total; index++ {
		entry := entriesToRender[index]
		rail := "│  "
		if index == total-1 {
			rail = "└  "
		}
		isDim := hiddenCount > 0 && index == 0
		switch entry.kind {
		case "thinking":
			lines = append(lines, g.thinkingRow(rail, entry.think, width, isDim))
		case "notice":
			lines = append(lines, g.noticeRow(rail, entry.notice, width, isDim))
		default:
			lines = append(lines, g.toolRowLocked(rail, entry.tool, frame, width, isDim))
		}
	}
	return lines
}

// toolReceivingArgs is `(e.tool._receivingArgs ?? e.tool.argsComplete === false)`
// (L1022-1024): an explicit flag wins over the argsComplete default.
func toolReceivingArgs(tool *toolComponent) bool {
	if tool.receivingArgs != nil {
		return *tool.receivingArgs
	}
	return !tool.argsComplete
}

// anyEntry reports whether any entry satisfies pred.
func anyEntry(entries []groupEntry, pred func(groupEntry) bool) bool {
	for _, entry := range entries {
		if pred(entry) {
			return true
		}
	}
	return false
}

// renderWidth is render(width) (L1072-1081): padding, truncation and the
// leading blank line a non-anchored group carries.
//
// The whole read runs under mu: a render happens on the TUI goroutine while an
// event handler may be appending an entry, so entries/children/anchored must be
// read under the lock. scheduleAnimation is called after mu is released, because
// it takes the lock itself and is a host-facing step.
//
// rendered caches the padded lines keyed by width and spinner frame index so a
// host Working-tick that re-enters with the same frame is cheap; invalidate
// drops the cache when content changes.
func (g *toolGroupComponent) renderWidth(width int) []string {
	g.tui.mu.Lock()
	visible := g.visibleEntriesLocked()
	if len(visible) == 0 {
		g.rendered = nil
		g.tui.mu.Unlock()
		return nil
	}
	wantAnimation := g.needsAnimationLocked()
	spinIndex := g.animationSpinIndexLocked(wantAnimation)
	anchored := g.anchored
	if g.rendered != nil && g.renderedWidth == width && g.renderedSpinIndex == spinIndex && g.renderedAnchored == anchored {
		out := g.rendered
		g.tui.mu.Unlock()
		if wantAnimation {
			g.tui.scheduleAnimation()
		}
		return out
	}
	paddingCount := minInt(groupPaddingX, maxInt(0, width-1))
	padding := strings.Repeat(" ", paddingCount)
	contentWidth := maxInt(1, width-paddingCount-groupPaddingRight)
	lines := g.renderCollapsed(contentWidth, visible)
	if len(lines) == 0 {
		g.rendered = nil
		g.tui.mu.Unlock()
		if wantAnimation {
			g.tui.scheduleAnimation()
		}
		return nil
	}
	rendered := make([]string, len(lines))
	for i, line := range lines {
		rendered[i] = padding + tui.TruncateToWidth(line, contentWidth, "...", false)
	}
	if !anchored {
		rendered = append([]string{""}, rendered...)
	}
	g.rendered = rendered
	g.renderedWidth = width
	g.renderedSpinIndex = spinIndex
	g.renderedAnchored = anchored
	g.tui.mu.Unlock()

	if wantAnimation {
		g.tui.scheduleAnimation()
	}
	return rendered
}

// animationSpinIndexLocked is the spinner frame index that keyed the render
// cache. Non-animating groups use -1 so a later animation start cannot reuse
// a still frame by accident.
func (g *toolGroupComponent) animationSpinIndexLocked(animating bool) int {
	if !animating {
		return -1
	}
	now := g.tui.nowMsLocked()
	start := g.tui.spinnerStartMs
	if g.hasPendingLocked() {
		index := int((now - start) / spinnerMs)
		index %= len(spinnerFrames)
		if index < 0 {
			index += len(spinnerFrames)
		}
		return index
	}
	index := int((now - start) / thinkingSpinnerIntervalMs)
	index %= len(thinkingSpinnerFrames)
	if index < 0 {
		index += len(thinkingSpinnerFrames)
	}
	return index
}

// maxInt / minInt are the small integer helpers the porters share.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// base36 renders seq in base 36 with a leading digit, the shape of
// Math.random().toString(36).slice(2, 7) (a five-character suffix).
func base36(seq int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if seq <= 0 {
		return "00000"
	}
	out := make([]byte, 0, 6)
	for seq > 0 {
		out = append([]byte{digits[seq%36]}, out...)
		seq /= 36
	}
	for len(out) < 5 {
		out = append([]byte{'0'}, out...)
	}
	if len(out) > 5 {
		out = out[len(out)-5:]
	}
	return string(out)
}

// sdkThemeFrom captures the host palette from a context, safely: a zero
// sdk.Context panics inside the SDK's host handle, so the palette read is
// guarded and degrades to the unstyled plain theme.
func sdkThemeFrom(ctx sdk.Context) theme {
	var captured sdk.UITheme
	ok := func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		captured = ctx.UITheme()
		return true
	}()
	if !ok {
		return plainTheme{}
	}
	return sdkTheme{theme: captured}
}
