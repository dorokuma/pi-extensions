package compactui

import (
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports src/grouping.ts (L1166-1511): the model that folds streamed
// tool calls and thinking runs into toolGroupComponents, anchors a group before
// the assistant text it precedes, absorbs the host's `cache miss` notices, and
// seals a group at the end of a turn.
//
// The TS version reaches the host's component tree by patching
// Container.prototype.addChild/removeChild/clear (installGrouping, L1400-1505).
// The port keeps the same order of operations on the ported tree: the patched
// addChild becomes runtimeState.addChildTo, which the extension calls from its
// host-event handlers.

// previousGroupable is previousGroupable (L1167-1178): the nearest preceding
// child that can join a group, skipping spacers, assistant messages and sealed
// groups.
func previousGroupable(children []component, start int) (component, int, bool) {
	for i := start; i >= 0; i-- {
		child := children[i]
		if isSpacer(child) || isAssistantMessage(child) {
			continue
		}
		if group, ok := child.(*toolGroupComponent); ok {
			if group.sealed {
				continue
			}
			return child, i, true
		}
		if isToolExecution(child) {
			return child, i, true
		}
	}
	return nil, 0, false
}

// assistantHasVisibleText is assistantHasVisibleText (L1179-1184).
func assistantHasVisibleText(component *assistantMessageComponent) bool {
	if component == nil || component.lastMessage == nil {
		return false
	}
	for _, item := range component.lastMessage.content {
		if item.typ == "text" && strings.TrimSpace(item.text) != "" {
			return true
		}
	}
	return false
}

// absorbThinkingBeforeText is absorbThinkingBeforeText (L1185-1201): the
// thinking runs streamed before the first visible text join the sealing group.
func (r *runtimeState) absorbThinkingBeforeText(group *toolGroupComponent, message *assistantMessage) {
	if message == nil {
		return
	}
	for _, item := range message.content {
		if item.typ == "text" && strings.TrimSpace(item.text) != "" {
			break
		}
		if item.typ != "thinking" {
			continue
		}
		text := strings.TrimSpace(item.thinking)
		if text == "" || r.thinkingAlreadyShown(text) {
			continue
		}
		r.mu.Lock()
		entry := &thinkingEntry{
			kind:        "thinking",
			id:          r.thinkingEntrySeq,
			text:        text,
			tokens:      estimateTextTokens(text),
			tokensExact: false,
			active:      false,
			owner:       group,
		}
		r.thinkingEntrySeq++
		group.entries = append(group.entries, groupEntry{kind: "thinking", think: entry})
		r.mu.Unlock()
	}
}

// sealOpenGroupAtAssistantText is sealOpenGroupAtAssistantText (L1202-1218).
func (r *runtimeState) sealOpenGroupAtAssistantText(assistant *assistantMessageComponent) {
	open := r.findOpenGroup()
	if open == nil {
		return
	}
	r.absorbThinkingBeforeText(open, assistant.lastMessage)
	r.mu.Lock()
	open.sealed = true
	r.mu.Unlock()
	r.finalizeActiveThinking()
	contentContainer := assistant.contentContainer
	if contentContainer != nil && hasVisibleTextMarkdown(contentContainer) {
		r.mu.Lock()
		previous := r.lastStreamingComp
		r.lastStreamingComp = assistant
		r.mu.Unlock()
		r.anchorGroupBeforeCurrentText(open, 0)
		r.mu.Lock()
		if previous != nil {
			r.lastStreamingComp = previous
		} else {
			r.lastStreamingComp = assistant
		}
		r.mu.Unlock()
	}
	open.invalidate()
}

// hasVisibleTextMarkdown reports whether the container holds a text Markdown.
func hasVisibleTextMarkdown(c *container) bool {
	for _, child := range c.snapshot() {
		if isVisibleTextMarkdown(child) {
			return true
		}
	}
	return false
}

// ensureThinkingGroup is ensureThinkingGroup (L1219-1245): park the
// unattached thinking runs in a group after the streaming message.
func (r *runtimeState) ensureThinkingGroup() {
	r.mu.Lock()
	pending := len(r.unattachedThinking)
	r.mu.Unlock()
	if pending == 0 {
		return
	}
	group := r.findOpenGroup()
	if group != nil {
		r.mu.Lock()
		chat := r.lastChatContainer
		streaming := r.lastStreamingComp
		r.mu.Unlock()
		if chat != nil && streaming != nil {
			r.placeGroupAfter(chat, streaming, group)
		}
	}
	if group == nil {
		// nowMs takes the lock itself, so read the clock before mu is taken.
		now := r.nowMs()
		r.mu.Lock()
		if r.lastChatContainer == nil || r.lastStreamingComp == nil {
			r.mu.Unlock()
			return
		}
		parent := r.lastChatContainer
		idx := parent.indexOf(r.lastStreamingComp)
		group = r.newToolGroupLocked(now)
		r.detachGroupLocked(group, nil)
		insertAt := parent.len()
		if idx >= 0 {
			insertAt = idx + 1
		}
		parent.insertAt(insertAt, group)
		group.mount = parent
		r.addGroupLocked(group)
		r.lastActiveGroup = group
		r.mu.Unlock()
	}
	r.mu.Lock()
	for len(r.unattachedThinking) > 0 {
		entry := r.unattachedThinking[0]
		r.unattachedThinking = r.unattachedThinking[1:]
		group.addThinkingLocked(entry)
	}
	tui := r.capturedTui
	r.mu.Unlock()
	group.invalidate()
	requestRender(tui)
}

// flushPendingTextSeal is flushPendingTextSeal (L1246-1270): once real text has
// streamed, the open group is sealed and anchored before that text.
func (r *runtimeState) flushPendingTextSeal() {
	r.mu.Lock()
	pending := r.pendingTextSeal
	hasActive := r.activeThinking != nil
	unattached := len(r.unattachedThinking)
	active := r.lastActiveGroup
	ordinal := r.pendingTextOrdinal
	r.mu.Unlock()
	if !pending {
		return
	}
	if hasActive || unattached > 0 {
		r.ensureThinkingGroup()
	}
	r.mu.Lock()
	active = r.lastActiveGroup
	sealed := active != nil && active.sealed
	var ordinalCopy *int
	if r.pendingTextOrdinal != nil {
		value := *r.pendingTextOrdinal
		ordinalCopy = &value
	}
	r.mu.Unlock()
	if active != nil && !sealed {
		if active.hasVisibleEntries() {
			if ordinalCopy != nil {
				r.anchorGroupBeforeCurrentText(active, *ordinalCopy)
			}
		} else {
			r.mu.Lock()
			if r.lastChatContainer != nil {
				r.removeGroupFromContainer(r.lastChatContainer, active)
			}
			r.deleteGroupLocked(active)
			r.mu.Unlock()
		}
		r.mu.Lock()
		active.sealed = true
		r.pendingTextSeal = false
		r.pendingTextOrdinal = nil
		r.mu.Unlock()
		r.finalizeActiveThinking()
		active.invalidate()
		return
	}
	r.mu.Lock()
	r.pendingTextSeal = false
	r.pendingTextOrdinal = nil
	r.mu.Unlock()
	_ = ordinal
}

// precedingAssistantThinking is getPrecedingAssistantThinking (L1271-1291).
func (r *runtimeState) precedingAssistantThinking(children []component, beforeIndex int) []string {
	for i := beforeIndex; i >= 0; i-- {
		child := children[i]
		if isSpacer(child) {
			continue
		}
		assistant, ok := child.(*assistantMessageComponent)
		if !ok {
			break
		}
		if assistant.lastMessage == nil {
			return nil
		}
		texts := make([]string, 0, len(assistant.lastMessage.content))
		for _, item := range assistant.lastMessage.content {
			if item.typ != "thinking" {
				continue
			}
			text := strings.TrimSpace(item.thinking)
			if text != "" && !r.thinkingAlreadyShown(text) {
				texts = append(texts, text)
			}
		}
		return texts
	}
	return nil
}

// attachPrecedingThinkingToGroup is attachPrecedingThinkingToGroup (L1292-1306).
func (r *runtimeState) attachPrecedingThinkingToGroup(group *toolGroupComponent, children []component, beforeIndex int) {
	texts := r.precedingAssistantThinking(children, beforeIndex)
	if len(texts) == 0 {
		return
	}
	for _, text := range texts {
		if r.thinkingAlreadyShown(text) {
			continue
		}
		r.mu.Lock()
		entry := &thinkingEntry{
			kind:   "thinking",
			id:     r.thinkingEntrySeq,
			text:   text,
			tokens: estimateTextTokens(text),
			owner:  group,
		}
		r.thinkingEntrySeq++
		group.entries = append(group.entries, groupEntry{kind: "thinking", think: entry})
		r.mu.Unlock()
	}
}

// joinOpenGroup is joinOpenGroup (L1307-1327).
func (r *runtimeState) joinOpenGroup(parent *container, index int, comp component) bool {
	r.mu.Lock()
	chat := r.lastChatContainer
	streaming := r.lastStreamingComp
	r.mu.Unlock()
	if chat == nil || parent != chat {
		return false
	}
	open := r.findOpenGroup()
	if open == nil {
		return false
	}
	// The container's child list is guarded by the container's own mutex, so
	// every splice and append below goes through it; the helpers that take
	// runtimeState.mu themselves are called after it is released.
	parent.spliceRange(index, 1)
	if streaming != nil && parent.contains(streaming) {
		r.placeGroupAfter(parent, streaming, open)
	} else if !parent.contains(open) {
		parent.addChild(open)
		open.mount = parent
	}
	children := parent.snapshot()
	r.attachPrecedingThinkingToGroup(open, children, parent.indexOf(open)-1)
	open.addTool(comp.(*toolComponent))
	r.mu.Lock()
	open.mount = parent
	r.lastActiveGroup = open
	r.mu.Unlock()
	open.invalidate()
	return true
}

// maybeGroup is maybeGroup (L1328-1372): fold a tool execution into the open
// group, the preceding group, a group around the preceding tool, or a fresh
// group.
func (r *runtimeState) maybeGroup(parent *container, comp component) {
	tool, isTool := comp.(*toolComponent)
	if !isTool || tool == nil {
		return
	}
	if parent == nil || isToolGroup(parent) {
		return
	}
	index := parent.indexOf(comp)
	if index < 0 {
		return
	}
	if r.joinOpenGroup(parent, index, comp) {
		return
	}
	prior, priorIndex, hasPrior := previousGroupable(parent.snapshot(), index-1)
	if hasPrior {
		if priorGroup, ok := prior.(*toolGroupComponent); ok && !priorGroup.sealed {
			r.attachPrecedingThinkingToGroup(priorGroup, parent.snapshot(), index-1)
			parent.spliceRange(index, 1)
			priorGroup.addTool(tool)
			r.mu.Lock()
			r.lastActiveGroup = priorGroup
			r.mu.Unlock()
			return
		}
	}
	if hasPrior {
		if priorTool, ok := prior.(*toolComponent); ok {
			group := r.newToolGroup(r.nowMs())
			r.attachPrecedingThinkingToGroup(group, parent.snapshot(), priorIndex-1)
			group.addTool(priorTool)
			r.attachPrecedingThinkingToGroup(group, parent.snapshot(), index-1)
			group.addTool(tool)
			r.mu.Lock()
			parent.spliceInsert(priorIndex, 1, group)
			parent.spliceRange(index, 1)
			group.mount = parent
			r.addGroupLocked(group)
			r.lastActiveGroup = group
			r.mu.Unlock()
			return
		}
	}
	group := r.newToolGroup(r.nowMs())
	r.attachPrecedingThinkingToGroup(group, parent.snapshot(), index-1)
	group.addTool(tool)
	r.mu.Lock()
	parent.spliceInsert(index, 1, group)
	group.mount = parent
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()
}

// extractCacheMissNotice is extractCacheMissNotice (L1373-1399): the host's
// notice text, when it is a `cache miss ... tokens re-billed` line.
func extractCacheMissNotice(component any) (string, bool) {
	text := ""
	switch value := component.(type) {
	case *textComponent:
		text = value.text
	case *markdownComponent:
		text = value.text
	case interface{ build() string }:
		text = value.build()
	case interface{ Text() string }:
		text = value.Text()
	}
	if text == "" {
		return "", false
	}
	clean := strings.TrimSpace(tui.StripTerminalSequences(text))
	if isCacheMissNotice(clean) {
		return clean, true
	}
	return "", false
}

// isCacheMissNotice is the regex of L1394:
// `^cache miss(?: after [^:]+)?:.*tokens re-billed` case-insensitively.
func isCacheMissNotice(text string) bool {
	lower := strings.ToLower(text)
	if !strings.HasPrefix(lower, "cache miss") {
		return false
	}
	rest := lower[len("cache miss"):]
	rest = strings.TrimPrefix(rest, " after ")
	if rest != lower[len("cache miss"):] {
		// `after <something without a colon>` then a colon.
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			return false
		}
		rest = rest[colon:]
	} else if !strings.HasPrefix(rest, ":") {
		return false
	}
	return strings.Contains(rest, "tokens re-billed")
}

// absorbCacheMissNotice is absorbCacheMissNotice (L1396-1414).
func (r *runtimeState) absorbCacheMissNotice(parent *container, text string) bool {
	r.mu.Lock()
	if r.lastChatContainer == nil && parent != nil {
		r.lastChatContainer = parent
	}
	r.mu.Unlock()
	group := r.findOpenGroup()
	if group == nil {
		group = r.openToolGroup()
	}
	if group == nil {
		r.mu.Lock()
		if r.lastActiveGroup != nil && r.groups[r.lastActiveGroup] {
			group = r.lastActiveGroup
		}
		r.mu.Unlock()
	}
	if group == nil {
		return false
	}
	r.mu.Lock()
	entry := &noticeEntry{
		kind:       "notice",
		id:         r.noticeEntrySeq,
		noticeType: "cache_miss",
		text:       text,
	}
	r.noticeEntrySeq++
	group.addNoticeLocked(entry)
	r.lastActiveGroup = group
	tui := r.capturedTui
	r.mu.Unlock()
	requestRender(tui)
	return true
}

// ── the patched Container.addChild / removeChild / clear (L1428-1503) ───────

// addChildTo is the patched Container.prototype.addChild of installGrouping
// (L1442-1500): it folds cache-miss notices into the group, patches a new
// assistant message, groups tool calls and restores text anchors.
func (r *runtimeState) addChildTo(parent *container, comp component) {
	r.mu.Lock()
	chat := r.lastChatContainer
	r.mu.Unlock()
	isChat := chat == nil || parent == chat
	if isChat {
		if text, ok := extractCacheMissNotice(comp); ok {
			if last := lastChild(parent); last != nil && isSpacer(last) {
				parent.removeChild(last)
			}
			if r.absorbCacheMissNotice(parent, text) {
				return
			}
		}
	}
	if comp == nil {
		return
	}
	if assistant, ok := comp.(*assistantMessageComponent); ok {
		r.ensureAssistantThinkingPatched(assistant)
		streaming := assistant.isStreaming
		r.mu.Lock()
		if streaming {
			assistant.live = true
		}
		r.lastChatContainer = parent
		r.lastStreamingComp = assistant
		assistant.streamingComp = streaming
		r.mu.Unlock()
		lastMsg := assistant.lastMessage
		isError := false
		hasTools := false
		if lastMsg != nil {
			isError = lastMsg.stopReason == "error" || lastMsg.stopReason == "aborted" || lastMsg.errorMessage != ""
			for _, item := range lastMsg.content {
				if item.typ == "toolCall" {
					hasTools = true
					break
				}
			}
		}
		if assistantHasVisibleText(assistant) {
			r.sealOpenGroupAtAssistantText(assistant)
		} else if isError && !hasTools {
			if open := r.findOpenGroup(); open != nil {
				r.mu.Lock()
				open.sealed = true
				r.mu.Unlock()
				r.finalizeActiveThinking()
				open.invalidate()
			}
		} else {
			if open := r.findOpenGroup(); open != nil {
				r.placeGroupAfter(parent, assistant, open)
			}
		}
		r.mu.Lock()
		unattached := len(r.unattachedThinking)
		r.mu.Unlock()
		if unattached > 0 {
			r.ensureThinkingGroup()
		}
		r.flushPendingTextSeal()
	}
	r.maybeGroup(parent, comp)
	r.stripAssistantPhantomPadding(parent, comp)
	r.restoreAssistantAnchor(parent, comp)
}

// lastChild returns the last child of a container, or nil.
func lastChild(c *container) component { return c.last() }

// removeChildFrom is the patched Container.prototype.removeChild (L1481-1489).
// removeChildFrom is the patched Container.prototype.removeChild (L1481-1489):
// a tool that belongs to a group mounted in this same container leaves the
// group instead of the container, and the group disappears once it is empty.
func (r *runtimeState) removeChildFrom(parent *container, comp component) {
	tool, isTool := comp.(*toolComponent)
	if isTool && tool != nil && tool.owner != nil && parent.contains(tool.owner) {
		group := tool.owner
		r.mu.Lock()
		group.removeToolLocked(tool)
		if len(group.children) == 0 && len(group.entries) == 0 {
			r.deleteGroupLocked(group)
		}
		r.mu.Unlock()
		return
	}
	r.releaseAssistantAnchors(comp)
	parent.removeChild(comp)
}

// clearPatched is the patched Container.prototype.clear (L1490-1502).
func (r *runtimeState) clearPatched(parent *container) {
	children := parent.snapshot()
	parent.clear()
	r.mu.Lock()
	isContent := r.assistantContentCont[parent]
	if isContent {
		r.getAssistantContentStateLocked(parent).nextTextOrdinal = 0
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	for _, child := range children {
		if group, ok := child.(*toolGroupComponent); ok {
			r.mu.Lock()
			for _, tool := range group.children {
				tool.owner = nil
			}
			r.deleteGroupLocked(group)
			r.mu.Unlock()
		}
		r.releaseAssistantAnchors(child)
	}
}

// installGrouping is installGrouping (L1415-1505). The TS patches the global
// Container prototype; the ported tree has no prototype to patch, so the
// runtime records that its addChildTo/removeChildFrom/clearPatched are the
// installed behaviour. It is idempotent and safe to call twice (session_start
// and the factory both call it).
func (r *runtimeState) installGrouping() {
	r.mu.Lock()
	r.groupingInstalled = true
	r.mu.Unlock()
}

// removeGroupFromContainer is removeGroupFromContainer (L1471-1479).
// The caller holds mu.
func (r *runtimeState) removeGroupFromContainer(c *container, group *toolGroupComponent) {
	if c == nil || group == nil {
		return
	}
	index := c.indexOf(group)
	if index >= 0 {
		c.spliceRange(index, 1)
	}
	if group.mount == c {
		group.mount = nil
	}
}

// detachGroup is detachGroup (L348-358). The caller holds mu.
func (r *runtimeState) detachGroupLocked(group *toolGroupComponent, keep *container) {
	if group == nil {
		return
	}
	if mounted := group.mount; mounted != nil && mounted != keep {
		r.removeGroupFromContainer(mounted, group)
	}
	if r.lastChatContainer != nil && r.lastChatContainer != keep {
		r.removeGroupFromContainer(r.lastChatContainer, group)
	}
	if anchor := r.groupAnchors[group]; anchor != nil && anchor.container != nil && anchor.container != keep {
		r.removeGroupFromContainer(anchor.container, group)
	}
}

// noteGroupMount is noteGroupMount (L1459-1461).
func noteGroupMount(c *container, group *toolGroupComponent) {
	group.mount = c
}

// placeAnchoredGroupBeforeText is placeAnchoredGroupBeforeText (L411-427):
// move the group (with one spacer on each side) in front of the target text.
func (r *runtimeState) placeAnchoredGroupBeforeText(c *container, target component, group *toolGroupComponent) {
	if c == nil || group == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detachGroupLocked(group, c)
	targetIndex := c.indexOf(target)
	if targetIndex < 0 {
		return
	}
	spacerStart := targetIndex
	for spacerStart > 0 && isSpacer(c.at(spacerStart-1)) {
		spacerStart--
	}
	if targetIndex > spacerStart {
		c.spliceRange(spacerStart, targetIndex-spacerStart)
	}
	group.anchored = true
	c.spliceInsert(spacerStart, 0, newSpacer(1), group, newSpacer(1))
	group.mount = c
}

// insertAnchoredGroup is insertAnchoredGroup (L428-434).
func (r *runtimeState) insertAnchoredGroup(c *container, ordinal int, group *toolGroupComponent) {
	if group == nil || !group.hasVisibleEntries() {
		return
	}
	markdowns := make([]component, 0, 8)
	for _, child := range c.snapshot() {
		if isVisibleTextMarkdown(child) {
			markdowns = append(markdowns, child)
		}
	}
	if ordinal < 0 || ordinal >= len(markdowns) {
		return
	}
	r.mu.Lock()
	r.removeGroupFromContainer(c, group)
	r.mu.Unlock()
	r.placeAnchoredGroupBeforeText(c, markdowns[ordinal], group)
}

// anchorGroupBeforeCurrentText is anchorGroupBeforeCurrentText (L435-473).
func (r *runtimeState) anchorGroupBeforeCurrentText(group *toolGroupComponent, ordinal int) {
	r.mu.Lock()
	streaming := r.lastStreamingComp
	chat := r.lastChatContainer
	r.mu.Unlock()
	if streaming == nil || chat == nil {
		return
	}
	contentContainer := streaming.contentContainer
	if contentContainer == nil {
		return
	}
	r.mu.Lock()
	r.removeGroupFromContainer(chat, group)
	if previousAnchor := r.groupAnchors[group]; previousAnchor != nil {
		if previousState := r.assistantContentStates[previousAnchor.container]; previousState != nil {
			if previousState.anchors[previousAnchor.ordinal] == group {
				delete(previousState.anchors, previousAnchor.ordinal)
			}
		}
		r.removeGroupFromContainer(previousAnchor.container, group)
	}
	hasVisible := group.hasVisibleEntriesLocked()
	if !hasVisible {
		r.deleteGroupLocked(group)
		r.mu.Unlock()
		return
	}
	state := r.getAssistantContentStateLocked(contentContainer)
	if replaced := state.anchors[ordinal]; replaced != nil && replaced != group {
		r.removeGroupFromContainer(contentContainer, replaced)
		replaced.anchored = false
		delete(r.groupAnchors, replaced)
		r.deleteGroupLocked(replaced)
	}
	state.anchors[ordinal] = group
	r.groupAnchors[group] = &groupAnchor{container: contentContainer, ordinal: ordinal}
	tui := r.capturedTui
	r.mu.Unlock()
	r.insertAnchoredGroup(contentContainer, ordinal, group)
	chat.Invalidate()
	requestRender(tui)
}

// restoreAssistantAnchor is restoreAssistantAnchor (L474-487).
func (r *runtimeState) restoreAssistantAnchor(parent *container, comp component) {
	if parent == nil {
		return
	}
	r.mu.Lock()
	isContent := r.assistantContentCont[parent]
	r.mu.Unlock()
	if !isContent || !isVisibleTextMarkdown(comp) {
		return
	}
	r.mu.Lock()
	state := r.getAssistantContentStateLocked(parent)
	ordinal := state.nextTextOrdinal
	state.nextTextOrdinal++
	group := state.anchors[ordinal]
	divider := state.finalDivider
	r.mu.Unlock()
	if group != nil && group.hasVisibleEntries() {
		r.mu.Lock()
		r.removeGroupFromContainer(parent, group)
		r.mu.Unlock()
		r.placeAnchoredGroupBeforeText(parent, comp, group)
	}
	if divider != nil && divider.ordinal == ordinal {
		placeTurnDividerBeforeText(parent, comp, divider.component)
	}
}

// releaseAssistantAnchors is releaseAssistantAnchors (L488-503).
func (r *runtimeState) releaseAssistantAnchors(comp component) {
	assistant, ok := comp.(*assistantMessageComponent)
	if !ok || assistant.contentContainer == nil {
		return
	}
	contentContainer := assistant.contentContainer
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.assistantContentStates[contentContainer]
	if state == nil {
		return
	}
	for _, group := range state.anchors {
		for _, tool := range group.children {
			if tool.owner == group {
				tool.owner = nil
			}
		}
		delete(r.groupAnchors, group)
		r.deleteGroupLocked(group)
	}
	state.anchors = map[int]*toolGroupComponent{}
	state.finalDivider = nil
}

// stripAssistantPhantomPadding is stripAssistantPhantomPadding (L504-517).
func (r *runtimeState) stripAssistantPhantomPadding(parent *container, comp component) {
	if parent == nil {
		return
	}
	if isAssistantMessage(comp) {
		if contentContainer, ok := comp.(*assistantMessageComponent); ok && contentContainer.contentContainer != nil {
			r.mu.Lock()
			r.assistantContentCont[contentContainer.contentContainer] = true
			r.getAssistantContentStateLocked(contentContainer.contentContainer)
			r.mu.Unlock()
			return
		}
	}
	r.mu.Lock()
	isContent := r.assistantContentCont[parent]
	r.mu.Unlock()
	if !isContent {
		return
	}
	if !r.isNativeThinkingWidget(comp) {
		return
	}
	index := parent.indexOf(comp)
	if index >= 0 {
		parent.spliceRange(index, 1)
	}
	if isSpacer(parent.at(index)) {
		parent.spliceRange(index, 1)
	}
	// The trailing phantom padding is a run of spacers: drop them while the list
	// still consists of nothing but spacers.
	for {
		children := parent.snapshot()
		if len(children) == 0 {
			break
		}
		allSpacers := true
		for _, child := range children {
			if !isSpacer(child) {
				allSpacers = false
				break
			}
		}
		if !allSpacers {
			break
		}
		parent.truncate(len(children) - 1)
	}
}

// isNativeThinkingWidget is isNativeThinkingWidget (L518-534): the placeholder
// the host renders for a hidden thinking block.
func (r *runtimeState) isNativeThinkingWidget(comp component) bool {
	if comp == nil || isToolGroup(comp) {
		return false
	}
	if markdown, ok := comp.(*markdownComponent); ok {
		return markdown.defaultTextStyle
	}
	text, ok := comp.(*textComponent)
	if !ok {
		return false
	}
	visible := strings.TrimSpace(tui.StripTerminalSequences(text.text))
	switch visible {
	case "", "Thinking...", "thinking", "thinking...":
		return true
	}
	return false
}

// getAssistantContentState is getAssistantContentState (L345-353).
func (r *runtimeState) getAssistantContentStateLocked(c *container) *assistantContentState {
	state := r.assistantContentStates[c]
	if state == nil {
		state = &assistantContentState{anchors: map[int]*toolGroupComponent{}}
		r.assistantContentStates[c] = state
	}
	return state
}

// removeComponentFromContainer is removeComponentFromContainer (L359-366).
func removeComponentFromContainer(c *container, comp component) {
	if c == nil || comp == nil {
		return
	}
	c.removeChild(comp)
}

// placeTurnDividerBeforeText is placeTurnDividerBeforeText (L386-391).
func placeTurnDividerBeforeText(c *container, target component, divider *turnDividerComponent) {
	if c == nil || divider == nil {
		return
	}
	targetIndex := c.indexOf(target)
	if targetIndex < 0 {
		return
	}
	if c.tui != nil {
		c.tui.mu.Lock()
		defer c.tui.mu.Unlock()
	}
	removeComponentFromContainer(c, divider)
	c.insertAt(targetIndex, divider)
}

// insertTurnDivider is insertTurnDivider (L392-418): the "worked for X" rule
// before the final assistant text of a turn.
func (r *runtimeState) insertTurnDivider(elapsedMs int64) {
	if elapsedMs < 1e3 {
		return
	}
	r.mu.Lock()
	comp := r.lastStreamingComp
	chat := r.lastChatContainer
	r.mu.Unlock()
	if comp == nil || !isAssistantMessage(comp) {
		if chat == nil {
			return
		}
		children := chat.snapshot()
		for i := len(children) - 1; i >= 0; i-- {
			if isAssistantMessage(children[i]) {
				comp, _ = children[i].(*assistantMessageComponent)
				break
			}
		}
	}
	if comp == nil || comp.contentContainer == nil {
		return
	}
	contentContainer := comp.contentContainer
	markdowns := make([]component, 0, 8)
	contentChildren := contentContainer.snapshot()
	for _, child := range contentChildren {
		if isVisibleTextMarkdown(child) {
			markdowns = append(markdowns, child)
		}
	}
	if len(markdowns) == 0 {
		return
	}
	final := markdowns[len(markdowns)-1]
	finalIndex := contentContainer.indexOf(final)
	hasPrior := false
	for i := 0; i < finalIndex && i < len(contentChildren); i++ {
		if group, ok := contentChildren[i].(*toolGroupComponent); ok && group.hasVisibleEntries() {
			hasPrior = true
			break
		}
	}
	if !hasPrior {
		return
	}
	r.mu.Lock()
	state := r.getAssistantContentStateLocked(contentContainer)
	if state.finalDivider != nil && contentContainer.contains(state.finalDivider.component) {
		r.mu.Unlock()
		return
	}
	divider := &turnDividerComponent{timeLabel: formatWorkedTime(elapsedMs), tui: r}
	state.finalDivider = &dividerPlacement{ordinal: len(markdowns) - 1, component: divider}
	tui := r.capturedTui
	r.mu.Unlock()
	placeTurnDividerBeforeText(contentContainer, final, divider)
	contentContainer.Invalidate()
	requestRender(tui)
}
