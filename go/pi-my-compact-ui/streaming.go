package compactui

import (
	"strings"
)

// This file ports src/thinking.ts (L1112-1165) and the streaming-tool half of
// src/streaming-tools.ts (L522-754).

// ── thinking (L1112-1165) ────────────────────────────────────────────────────

// updateActiveThinkingTokens is updateActiveThinkingTokens (L1113-1126): the
// visible text of the active run, and its token count from the host usage or
// the 4-chars-per-token estimate.
func (r *runtimeState) updateActiveThinkingTokens(message *assistantMessage) {
	r.mu.Lock()
	entry := r.activeThinking
	if entry == nil {
		r.mu.Unlock()
		return
	}
	texts := make([]string, 0, len(r.activeThinkingBlocks))
	for _, block := range r.activeThinkingBlockOrder {
		if strings.TrimSpace(block.text) != "" {
			texts = append(texts, block.text)
		}
	}
	entry.text = strings.Join(texts, "\n\n")
	reported, hasReported := messageReasoningTokens(message)
	if hasReported && reported > 0 {
		entry.tokens = reported
		entry.tokensExact = true
		r.lastReportedReasoning = reported
		r.mu.Unlock()
		return
	}
	entry.tokens = estimateTextTokens(entry.text)
	entry.tokensExact = false
	r.mu.Unlock()
}

// messageReasoningTokens is `Number(message?.usage?.reasoning)` plus its
// finiteness check.
func messageReasoningTokens(message *assistantMessage) (int, bool) {
	if message == nil || message.usage == nil || message.usage.reasoning == nil {
		return 0, false
	}
	return *message.usage.reasoning, true
}

// createThinkingEntry is createThinkingEntry (L1127-1143).
func (r *runtimeState) createThinkingEntry() *thinkingEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createThinkingEntryLocked()
}

func (r *runtimeState) createThinkingEntryLocked() *thinkingEntry {
	entry := &thinkingEntry{
		kind:   "thinking",
		id:     r.thinkingEntrySeq,
		active: true,
	}
	r.thinkingEntrySeq++
	r.activeThinking = entry
	r.activeThinkingBlocks = map[int]string{}
	r.activeThinkingBlockOrder = nil
	r.unattachedThinking = append(r.unattachedThinking, entry)
	return entry
}

// finalizeActiveThinking is finalizeActiveThinking (L1144-1157).
func (r *runtimeState) finalizeActiveThinking() {
	r.mu.Lock()
	owner := r.finalizeActiveThinkingLocked()
	r.mu.Unlock()
	// Clearing `active` changes the run's row (spinner -> check, token label) and
	// can drop it out of the visible set entirely, so the card painting the tree
	// has to repaint.
	if owner != nil {
		r.invalidateGroupCards(owner)
	}
}

// finalizeActiveThinkingLocked is finalizeActiveThinking for a caller that
// already holds mu. It never reaches the host; the owner group's cached render
// is dropped under the same lock (invalidateLocked only drops the cached
// render) and the owner is returned so the caller can route the host repaint
// through invalidateGroupCards after unlocking.
func (r *runtimeState) finalizeActiveThinkingLocked() *toolGroupComponent {
	entry := r.activeThinking
	if entry == nil {
		return nil
	}
	entry.active = false
	if r.activeThinkingIndex != nil {
		r.sealedThinkingIndexes[*r.activeThinkingIndex] = true
	}
	r.lastSealedThinkingEntry = entry
	r.activeThinking = nil
	r.activeThinkingIndex = nil
	r.activeThinkingBlocks = map[int]string{}
	r.activeThinkingBlockOrder = nil
	owner := entry.owner
	if owner != nil {
		owner.invalidateLocked()
	}
	return owner
}

// refreshSealedThinkingTokens is refreshSealedThinkingTokens (L1158-1165): the
// late exact reasoning count replaces a sealed run's estimate.
func (r *runtimeState) refreshSealedThinkingTokens(reported *int) {
	r.mu.Lock()
	if reported != nil && *reported > 0 {
		r.lastReportedReasoning = *reported
	}
	entry := r.lastSealedThinkingEntry
	if entry == nil || entry.tokensExact {
		r.mu.Unlock()
		return
	}
	if strings.TrimSpace(entry.text) == "" || r.lastReportedReasoning <= 0 {
		r.mu.Unlock()
		return
	}
	entry.tokens = r.lastReportedReasoning
	entry.tokensExact = true
	owner := entry.owner
	r.mu.Unlock()
	// The token label on the thinking row changed; the card has to repaint it.
	r.invalidateGroupCards(owner)
}

// requestRender asks the captured TUI for a frame. It is called only outside
// mu, never under it.
func requestRender(tui tuiHandle) {
	if tui == nil {
		return
	}
	tui.requestRender()
}

// ── streaming tools (L522-754) ───────────────────────────────────────────────

// toolElapsed is toolElapsed (L523-556): the elapsed label of a tool call,
// from the recorded duration, the group end or the live start. It takes mu
// itself; toolElapsedLocked is the form a caller already holding mu uses.
func toolElapsed(r *runtimeState, tool *toolComponent) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.toolElapsedLocked(tool)
}

// toolElapsedLocked is toolElapsed for a caller that already holds mu. It must
// not call nowMs (which takes the lock) or any other locking helper.
func (r *runtimeState) toolElapsedLocked(tool *toolComponent) string {
	if tool == nil {
		return formatToolDuration(0)
	}
	if known, ok := r.toolDurations[tool.toolCallID]; ok {
		tool.elapsedDur = &known
		return formatToolDuration(known)
	}
	if tool.groupEndAt != nil {
		start, present := r.toolStarts[tool.toolCallID]
		if !present {
			start = *tool.groupEndAt
		}
		dur := float64(*tool.groupEndAt-start) / 1e3
		if dur < 0 {
			dur = 0
		}
		tool.elapsedDur = &dur
		return formatToolDuration(dur)
	}
	if tool.elapsedDur != nil {
		// A host-reported duration (the tool renderer's DurationMs, or a duration
		// an earlier render resolved) is the best evidence available even while
		// the call is still running.
		return formatToolDuration(*tool.elapsedDur)
	}
	if tool.result != nil && !tool.isPartial {
		resultTs := tool.result.timestamp
		start, present := r.toolStarts[tool.toolCallID]
		if present && resultTs != 0 && resultTs >= start {
			dur := float64(resultTs-start) / 1e3
			tool.elapsedDur = &dur
			return formatToolDuration(dur)
		}
		zero := 0.0
		tool.elapsedDur = &zero
		return "0ms"
	}
	group := tool.owner
	if tool.interrupted || (group != nil && group.sealed) || r.interruptedToolCallID[tool.toolCallID] {
		zero := 0.0
		tool.elapsedDur = &zero
		return "0ms"
	}
	start, present := r.toolStarts[tool.toolCallID]
	if present {
		dur := float64(r.nowMsLocked()-start) / 1e3
		if dur < 0 {
			dur = 0
		}
		return formatToolDuration(dur)
	}
	return "0ms"
}

// loadSessionToolTimings is loadSessionToolTimings (L564-597): replay a
// resumed session's tool calls and results into the timing maps, marking a
// call without a result as interrupted.
func (r *runtimeState) loadSessionToolTimings(entries []map[string]any) {
	if entries == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sessionCalls := map[string]bool{}
	sessionResults := map[string]bool{}
	for _, entry := range entries {
		if asString(entry["type"]) != "message" {
			continue
		}
		message, ok := asObject(entry["message"])
		if !ok {
			continue
		}
		switch asString(message["role"]) {
		case "assistant":
			content, isList := asList(message["content"])
			if !isList {
				continue
			}
			startTime := entryTime(message, entry)
			for _, item := range content {
				block, isObject := asObject(item)
				if !isObject || asString(block["type"]) != "toolCall" {
					continue
				}
				id := asString(block["id"])
				if id == "" {
					continue
				}
				sessionCalls[id] = true
				if startTime > 0 {
					r.toolStarts[id] = startTime
				}
			}
		case "toolResult":
			id := asString(message["toolCallId"])
			if id == "" {
				continue
			}
			sessionResults[id] = true
			endTime := entryTime(message, entry)
			start, present := r.toolStarts[id]
			if present && endTime >= start {
				r.toolDurations[id] = float64(endTime-start) / 1e3
			}
		}
	}
	for id := range sessionCalls {
		if sessionResults[id] {
			continue
		}
		if _, present := r.toolDurations[id]; !present {
			r.toolDurations[id] = 0
		}
		r.interruptedToolCallID[id] = true
	}
}

// entryTime is `Number(msg.timestamp) || new Date(entry.timestamp).getTime()`.
func entryTime(message map[string]any, entry map[string]any) int64 {
	if raw, present := message["timestamp"]; present {
		if number, ok := toNumber(raw); ok && number != 0 {
			return int64(number)
		}
	}
	return parseTimestampMillis(asString(entry["timestamp"]))
}

// findGroupedTool is findGroupedTool (L599-610): the grouped tool with this
// call id, or at this content index. Deviation (bug fix, brief2): the id wins
// once the stream carries one -- the original (TS bundle L598-608) falls through
// to `tool._contentIndex === contentIndex` even when an id was given, so a
// rewritten block in a reused slot folds onto whatever call already sits there.
func (r *runtimeState) findGroupedTool(toolCallID string, contentIndex int) *toolComponent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.findGroupedToolLocked(toolCallID, contentIndex)
}

func (r *runtimeState) findGroupedToolLocked(toolCallID string, contentIndex int) *toolComponent {
	for _, group := range r.groupsListLocked() {
		for _, entry := range group.entries {
			if entry.kind != "tool" || entry.tool == nil {
				continue
			}
			tool := entry.tool
			if toolCallID != "" && tool.toolCallID == toolCallID {
				return tool
			}
			// The content-index fallback only applies while the group is still
			// streaming. A sealed group is history: folding a brand-new call
			// into it (same index, next turn) updates the old entry instead of
			// creating one, so the new call has no group entry at all and its
			// host card paints a bare row outside the compact tree.
			if group.sealed {
				continue
			}
			if tool.hasIndex && tool.contentIndex == contentIndex {
				return tool
			}
		}
	}
	return nil
}

// invalidateGroupedTool is invalidateGroupedTool (L611-620).
func (r *runtimeState) invalidateGroupedTool(tool *toolComponent) {
	r.mu.Lock()
	group := (*toolGroupComponent)(nil)
	if tool != nil {
		group = tool.owner
	}
	if group == nil {
		group = r.lastActiveGroup
	}
	if group != nil && !group.sealed {
		r.lastActiveGroup = group
	}
	r.mu.Unlock()
	r.invalidateGroupCards(group)
}

// findOpenGroup is findOpenGroup (L621-629).
func (r *runtimeState) findOpenGroup() *toolGroupComponent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.findOpenGroupLocked()
}

func (r *runtimeState) findOpenGroupLocked() *toolGroupComponent {
	if r.lastActiveGroup != nil && !r.lastActiveGroup.sealed && r.groups[r.lastActiveGroup] {
		return r.lastActiveGroup
	}
	for _, group := range r.groupsListLocked() {
		if !group.sealed {
			return group
		}
	}
	return nil
}

// placeGroupAfter is placeGroupAfter (L630-641): move the group right after
// `after` in the chat container.
func (r *runtimeState) placeGroupAfter(parent *container, after component, group *toolGroupComponent) {
	if parent == nil {
		return
	}
	r.mu.Lock()
	r.detachGroupLocked(group, parent)
	r.mu.Unlock()
	if existing := parent.indexOf(group); existing >= 0 {
		parent.spliceRange(existing, 1)
	}
	afterIndex := -1
	if after != nil {
		afterIndex = parent.indexOf(after)
	}
	insertAt := parent.len()
	if afterIndex >= 0 {
		insertAt = afterIndex + 1
	}
	// mount and lastActiveGroup are read by renders and by the anchor bookkeeping,
	// so they move under mu together with the child list.
	r.mu.Lock()
	parent.insertAt(insertAt, group)
	group.mount = parent
	r.lastActiveGroup = group
	r.mu.Unlock()
}

// openToolGroup is openToolGroup (L642-668): the open group, or a new one
// right after the streaming assistant message.
func (r *runtimeState) openToolGroup() *toolGroupComponent {
	if open := r.findOpenGroup(); open != nil {
		r.mu.Lock()
		chat := r.lastChatContainer
		streaming := r.lastStreamingComp
		r.mu.Unlock()
		if chat != nil && streaming != nil {
			r.placeGroupAfter(chat, streaming, open)
		}
		return open
	}
	// nowMs takes the lock itself, so read the clock before mu is taken.
	now := r.nowMs()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openToolGroupLocked(now)
}

// openToolGroupLocked is openToolGroup with the caller's lock held. The caller
// has read the clock already (nowMs takes the lock itself).
func (r *runtimeState) openToolGroupLocked(now int64) *toolGroupComponent {
	chat := r.lastChatContainer
	streaming := r.lastStreamingComp
	if chat == nil || streaming == nil {
		return nil
	}
	children := chat.snapshot()
	idx := chat.indexOf(streaming)
	group := r.newToolGroupLocked(now)
	r.detachGroupLocked(group, nil)
	insertAt := len(children)
	if idx >= 0 {
		insertAt = idx + 1
	}
	chat.insertAt(insertAt, group)
	group.mount = chat
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	return group
}

// attachPreparingTool is attachPreparingTool (L669-677).
func (r *runtimeState) attachPreparingTool(tool *preparingTool) {
	r.mu.Lock()
	owned := tool.owner != nil
	r.mu.Unlock()
	if owned {
		return
	}
	group := r.openToolGroup()
	if group == nil {
		return
	}
	r.mu.Lock()
	tool.owner = group
	// Parked thinking runs belong to the same turn as the call that is about to
	// stream in, so fold them in first: the group then opens with its thinking
	// entry already present and the very first frame paints the tree, instead of
	// a lone tool row that only grows a tree once the thinking is attached.
	for len(r.unattachedThinking) > 0 {
		entry := r.unattachedThinking[0]
		r.unattachedThinking = r.unattachedThinking[1:]
		group.addThinkingLocked(entry)
	}
	// The entry carries a real toolComponent, so every renderer and modal reads
	// the same fields it reads for an executed tool call (TS hangs the preparing
	// object itself on the entry).
	if tool.comp == nil {
		receiving := tool.receivingArgs
		tool.comp = &toolComponent{
			toolCallID:    tool.toolCallID,
			toolName:      tool.toolName,
			args:          tool.args,
			isPartial:     true,
			preparing:     true,
			receivingArgs: &receiving,
			contentIndex:  tool.contentIndex,
			hasIndex:      tool.hasIndex,
			tui:           r,
		}
	}
	group.entries = append(group.entries, groupEntry{kind: "tool", tool: tool.comp, preparing: tool})
	r.lastActiveGroup = group
	r.mu.Unlock()
	// The group just grew, so the card that painted until now keeps a frame of
	// the smaller group unless it is invalidated: the host re-renders a card
	// only when it is dirty or its Invalidate hook fires, and a finished or
	// non-shell card is neither. Without this the previous display tool's card
	// still shows its old single-entry row (or a stale tree) next to the new
	// tree for up to one animation tick. The entry itself is left alone: its own
	// card repaints from the dirty state.
	r.invalidateGroupCardsExcept(group, tool.comp.toolCallID)
}

// detachPreparingTool is detachPreparingTool (L678-685). It takes mu itself and
// routes the resulting repaint through invalidateGroupCards: the tree loses a
// row.
func (r *runtimeState) detachPreparingTool(tool *preparingTool) {
	r.mu.Lock()
	group := (*toolGroupComponent)(nil)
	if tool != nil {
		group = tool.owner
	}
	r.detachPreparingToolLocked(tool)
	r.mu.Unlock()
	r.invalidateGroupCards(group)
}

// detachPreparingToolLocked is detachPreparingTool with the caller's lock held.
func (r *runtimeState) detachPreparingToolLocked(tool *preparingTool) {
	group := tool.owner
	if group != nil {
		for i, entry := range group.entries {
			if entry.kind == "tool" && entry.preparing == tool {
				group.entries = append(group.entries[:i], group.entries[i+1:]...)
				break
			}
		}
	}
	tool.owner = nil
}

// interruptPreparingToolLocked converts a provisional entry whose stream already
// carried a real call id into the interrupted group entry the tree keeps when a
// turn is cut short (Esc or a stream error) before tool_execution_start. The
// caller holds mu.
//
// The host has a card for such a call (it mounts one as soon as the streamed
// block has an id) and keeps it across the abort, so the entry must stay in the
// group: detaching it left the card without a group -- a bare row outside the
// tree -- while the sibling that had yielded the display rights stayed blank.
// The original keeps the very component in the group too (addTool moved it into
// children, and the agent_end walk marks it `_interrupted`), which is exactly
// the state finished here: the entry loses its provisional link, the component
// becomes a normal group entry owned by the group and listed in children so that
// walk and the historical seal see it, its group end is stamped, and it is
// marked interrupted (interruptedToolCallID + interrupted, which toolStatus
// reports as "error" and the row renders as a cross).
func (r *runtimeState) interruptPreparingToolLocked(tool *preparingTool, now int64) {
	if tool == nil {
		return
	}
	group := tool.owner
	comp := tool.comp
	if group == nil || comp == nil {
		return
	}
	for i, entry := range group.entries {
		if entry.kind == "tool" && entry.preparing == tool {
			group.entries[i].preparing = nil
			break
		}
	}
	comp.preparing = false
	comp.owner = group
	if comp.groupEndAt == nil && !r.hasDurationLocked(comp.toolCallID) {
		at := now
		comp.groupEndAt = &at
	}
	r.interruptedToolCallID[comp.toolCallID] = true
	comp.interrupted = true
	if !group.hasChildLocked(comp) {
		group.children = append(group.children, comp)
	}
	group.invalidateLocked()
	tool.owner = nil
}

// takePreparingTool is takePreparingTool (L686-703): the provisional entry a
// real tool call replaces, by id or by a unique name match.
func (r *runtimeState) takePreparingTool(toolCallID, toolName string) *preparingTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.takePreparingToolLocked(toolCallID, toolName)
}

// takePreparingToolLocked is takePreparingTool with the caller's lock held.
func (r *runtimeState) takePreparingToolLocked(toolCallID, toolName string) *preparingTool {
	if toolCallID != "" {
		for index, candidate := range r.preparingByIndex {
			if candidate.toolCallID == toolCallID {
				delete(r.preparingByIndex, index)
				return candidate
			}
		}
	}
	matches := make([]int, 0, 1)
	for index, candidate := range r.preparingByIndex {
		if candidate.toolName == toolName {
			matches = append(matches, index)
		}
	}
	if len(matches) == 1 {
		candidate := r.preparingByIndex[matches[0]]
		delete(r.preparingByIndex, matches[0])
		return candidate
	}
	return nil
}

// absorbPreparingTool is absorbPreparingTool (L704-715).
func (r *runtimeState) absorbPreparingTool(tool *toolComponent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.absorbPreparingToolLocked(tool)
}

// absorbPreparingToolLocked is absorbPreparingTool with the caller's lock held.
func (r *runtimeState) absorbPreparingToolLocked(tool *toolComponent) {
	if tool == nil {
		return
	}
	preparing := r.takePreparingToolLocked(tool.toolCallID, tool.toolName)
	if preparing == nil {
		return
	}
	id := tool.toolCallID
	if start, present := r.toolStarts[preparing.toolCallID]; present && id != "" {
		if _, exists := r.toolStarts[id]; !exists {
			r.toolStarts[id] = start
		}
	}
	tool.contentIndex = preparing.contentIndex
	tool.hasIndex = true
	receiving := preparing.receivingArgs
	tool.receivingArgs = &receiving
	if len(tool.args) == 0 && preparing.args != nil {
		tool.args = preparing.args
	}
	r.detachPreparingToolLocked(preparing)
}

// syncPreparingToolLocked copies the streamed state onto the toolComponent the
// group entry renders, so a renderer never sees stale arguments or a stale
// receiving flag. The caller holds mu.
func (r *runtimeState) syncPreparingToolLocked(tool *preparingTool) {
	if tool == nil || tool.comp == nil {
		return
	}
	tool.comp.toolName = tool.toolName
	tool.comp.toolCallID = tool.toolCallID
	tool.comp.args = tool.args
	receiving := tool.receivingArgs
	tool.comp.receivingArgs = &receiving
	tool.comp.contentIndex = tool.contentIndex
	tool.comp.hasIndex = tool.hasIndex
}

// clearPreparingTools is clearPreparingTools (L716-719).
func (r *runtimeState) clearPreparingTools() {
	r.mu.Lock()
	pending := make([]*preparingTool, 0, len(r.preparingByIndex))
	groups := make([]*toolGroupComponent, 0, len(r.preparingByIndex))
	for _, tool := range r.preparingByIndex {
		pending = append(pending, tool)
		if tool.owner != nil {
			groups = append(groups, tool.owner)
		}
	}
	r.preparingByIndex = map[int]*preparingTool{}
	for _, tool := range pending {
		r.detachPreparingToolLocked(tool)
	}
	r.mu.Unlock()
	for _, group := range groups {
		r.invalidateGroupCards(group)
	}
}

// absorbGroupedPreparingLocked retires the provisional entry that already
// rendered as comp. When the real tool call *is* that component the entry stays
// in the group and only its provisional link is cleared -- detaching it would
// drop the component the group is about to draw. The transition to the executed
// state has to finish here: the component leaves `preparing` (so later stream
// events update it instead of reopening a provisional entry), takes the group as
// its owner and joins the group's `children`, which is the list agent_end and
// the historical seal walk to mark an unfinished call interrupted. The caller
// holds mu; the groups whose visible state changed are returned so the caller
// can route their repaint through invalidateGroupCards after unlocking.
func (r *runtimeState) absorbGroupedPreparingLocked(comp *toolComponent) []*toolGroupComponent {
	if comp == nil {
		return nil
	}
	for _, group := range r.groupsListLocked() {
		for index, entry := range group.entries {
			if entry.kind != "tool" || entry.tool != comp || entry.preparing == nil {
				continue
			}
			retired := entry.preparing
			retired.owner = nil
			retired.comp = nil
			for preparingIndex, candidate := range r.preparingByIndex {
				if candidate == retired {
					delete(r.preparingByIndex, preparingIndex)
				}
			}
			group.entries[index].preparing = nil
			// Same entry, same component: finish the executing transition in place.
			changed := []*toolGroupComponent{group}
			if previous := comp.owner; previous != nil && previous != group {
				// The component is still listed by the group that owned the
				// provisional entry; moving it must not leave an orphan behind, and
				// that group lost a row, so it has to repaint too.
				previous.removeToolLocked(comp)
				changed = append(changed, previous)
			}
			comp.preparing = false
			comp.owner = group
			if !group.hasChildLocked(comp) {
				group.children = append(group.children, comp)
			}
			// Host cards for non-display siblings must re-render empty (ShellSelf
			// + [] lines). Without Invalidate they keep Default Spacer(1) or a
			// stale tree and stack into the large blank gap in gap.png.
			group.invalidateLocked()
			return changed
		}
	}
	return nil
}

// noteStreamingToolCall is noteStreamingToolCall (L720-753): fold one streamed
// tool-call event into the group, creating or updating the provisional entry.
func (r *runtimeState) noteStreamingToolCall(content []contentBlock, streamEvent streamEvent) {
	index, block, found := locateStreamingToolCall(content, streamEvent)
	if !found {
		return
	}
	id := block.id
	name := block.name
	if name == "" {
		name = "tool"
	}
	args := streamingToolArgs(block)
	receivingArgs := streamEvent.typ != "toolcall_end"
	// nowMs takes the lock itself, so read the clock before entering it.
	now := r.nowMs()
	// The call id, once the stream carries one, identifies the call on its own.
	// Falling back to the content index when an id is present folds this call's
	// arguments onto a different, already-executed call that happens to share
	// the index (the model rewrites a block in place and the wire reuses the
	// slot): that corrupts the other entry and leaves this call with no group
	// entry at all, so its host card paints a bare row outside the compact tree
	// for the whole streaming window. The index only identifies a call whose
	// stream has not carried an id yet. This is a deliberate deviation from the
	// original: TS L599-610 checks the id first and then matches `_contentIndex`
	// unconditionally, id or not.
	var existing *toolComponent
	if id != "" {
		r.mu.Lock()
		existing = r.findGroupedToolByIDLocked(id)
		r.mu.Unlock()
	} else {
		existing = r.findGroupedTool("", index)
	}
	if existing != nil && !existing.preparing {
		r.mu.Lock()
		existing.contentIndex = index
		existing.hasIndex = true
		existing.args = args
		existing.receivingArgs = &receivingArgs
		callID := existing.toolCallID
		if callID == "" {
			callID = id
		}
		r.rememberToolStartLocked(callID, now)
		r.mu.Unlock()
		r.invalidateGroupedTool(existing)
		return
	}
	r.mu.Lock()
	preparing := r.preparingByIndex[index]
	created := false
	if preparing == nil {
		callID := id
		if callID == "" {
			callID = preparingToolCallIDPrefix + itoa(index)
		}
		preparing = &preparingTool{
			toolName:      name,
			toolCallID:    callID,
			args:          args,
			isPartial:     true,
			preparing:     true,
			receivingArgs: receivingArgs,
			contentIndex:  index,
			hasIndex:      true,
		}
		r.preparingByIndex[index] = preparing
		created = true
	} else {
		if name != "" && name != "tool" {
			preparing.toolName = name
		}
		if id != "" && preparing.toolCallID != id {
			previous := preparing.toolCallID
			preparing.toolCallID = id
			// The real id is what makes the host mount a card for this call, so
			// this is the frame where the call can take the tree over. The card
			// that painted until now has to repaint empty (it is no longer the
			// display tool) or both cards paint the same tree -- invalidatePreparingTool
			// below does that now that the entry has a card of its own.
			if start, present := r.toolStarts[previous]; present {
				if _, exists := r.toolStarts[id]; !exists {
					r.toolStarts[id] = start
				}
			}
		}
		preparing.args = args
		preparing.receivingArgs = receivingArgs
	}
	// The group entry renders tool.comp, so the streamed state has to reach it
	// before the entry is drawn again.
	r.syncPreparingToolLocked(preparing)
	callID := preparing.toolCallID
	r.mu.Unlock()
	_ = created
	// attachPreparingTool is idempotent (it returns when the entry already has
	// an owner), as the TS `if (!preparing.owner) attachPreparingTool(...)` is.
	r.attachPreparingTool(preparing)
	r.rememberToolStart(callID, now)
	r.invalidatePreparingTool(preparing)
}

// invalidatePreparingTool is invalidateGroupedTool for a provisional entry. It
// is the one place the streaming path repaints from, and it decides who paints:
//
//   - the entry already has a host card (the stream carried a real call id), so
//     it is the group's display card and the host repaints it from its dirty
//     state; only the siblings that yielded the display rights are invalidated.
//   - the entry has no card yet (its stream carries no id), so *another* card
//     paints the tree. That card never repaints on its own, so it has to be
//     invalidated on this very frame -- waiting for the 100ms animation tick
//     left the tree a frame behind the stream (brief2 R4 C3).
func (r *runtimeState) invalidatePreparingTool(tool *preparingTool) {
	r.mu.Lock()
	group := tool.owner
	if group == nil {
		group = r.lastActiveGroup
	}
	if group != nil && !group.sealed {
		r.lastActiveGroup = group
	}
	callID := tool.toolCallID
	r.mu.Unlock()
	keepID := ""
	if hostCardForCallID(callID) {
		keepID = callID
	}
	r.invalidateGroupCardsExcept(group, keepID)
}
