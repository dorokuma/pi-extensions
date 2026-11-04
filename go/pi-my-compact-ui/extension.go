package compactui

import (
	"os"
	"sort"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	tui "github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports src/extension.ts (L2192-2452): the factory, its two commands
// and its eleven event subscriptions, plus the widget renderer that replaces
// the TS `ctx.ui.setWidget("compact-anim", ...)` capture.
//
// Concurrency contract (task hard rule): every handler below snapshots what it
// needs under runtimeState.mu, releases the lock, and only then reaches the
// host. The host-facing closures are all panic-guarded inside sdkUI / safeCall,
// so a zero sdk.Context{} cannot take the runtime down.

// Extension constructs the compact-ui extension for PiG. It is the factory a
// generated PiG runner calls.
func Extension() *sdk.Extension {
	return NewExtension()
}

// NewExtension builds the extension and its runtime state. The tests use it to
// drive the wiring without a live host connection.
//
// It is the factory a generated PiG runner calls, and it is where the port
// reaches the host surfaces PLAN.md §4 names: setFooter / onTerminalInput /
// registerToolRenderer become SetWidgetRenderer + OnTerminalInput (wired at
// session_start) and ext.ToolRenderer (wired here).
func NewExtension() *sdk.Extension {
	r := createRuntimeState()
	r.configPath = configPathFor(os.Getenv, os.UserHomeDir)
	r.config = loadConfig(r.configPath, readConfigFile)
	r.maxGroupEntries = r.config.MaxGroupEntries
	r.spinnerStartMs = wallClock()

	e := sdk.New("pi-my-compact-ui")

	// L2210-2212: the three install steps of the factory.
	r.installGrouping()
	r.installNativeThinkingSuppression()
	r.installCompactionSummaryRendering(nil)

	// L256-334: the native thinking suppression. The TS patch removes the
	// thinking blocks from the host's own render; the PiG form is the registered
	// Markdown transformer, which the host applies to the "assistant-thinking"
	// Markdown as well and which renders no lines for an empty result
	// (transformMarkdown, assistant.go).
	e.MarkdownTransformer(transformMarkdownFunc)

	// PLAN.md §4: registerToolRenderer. The resolver hands every tool card the
	// compact one-line format the group uses, so a PiG subprocess session draws
	// the same tree/row UI the TS prototype patch produces in-process.
	r.installToolRenderer(e)

	// L2213-2219: /compact-inspect.
	e.Command("compact-inspect", "Select a tool call or thinking entry to inspect details (Shortcut: Ctrl+I)", func(ctx sdk.Context, _ string) error {
		defer func() { _ = recover() }()
		r.mu.Lock()
		r.activeUIContext = sdkUI{ctx: ctx}
		r.mu.Unlock()
		r.openInspectSelectorModal()
		return nil
	})

	// L2220-2273: session_start.
	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		r.handleSessionStart(ctx)
		return nil, nil
	})

	// L2274-2278: session_shutdown.
	e.OnEvent(sdk.EventSessionShutdown, func(_ sdk.Context, _ map[string]any) (any, error) {
		r.handleSessionShutdown()
		return nil, nil
	})

	// L2279-2298: the work-phase events.
	e.OnEvent(sdk.EventAgentStart, func(_ sdk.Context, _ map[string]any) (any, error) {
		r.setAgentWorkPhase(strPtr(phaseWorking))
		return nil, nil
	})
	e.OnEvent(sdk.EventTurnStart, func(_ sdk.Context, _ map[string]any) (any, error) {
		r.setAgentWorkPhase(strPtr(phaseWaitingResponse))
		return nil, nil
	})
	e.OnEvent(sdk.EventTurnEnd, func(_ sdk.Context, _ map[string]any) (any, error) {
		r.setAgentWorkPhase(strPtr(phaseWorking))
		return nil, nil
	})

	// L2299-2325: tool_execution_start / tool_execution_end.
	e.OnEvent(sdk.EventToolExecutionStart, func(_ sdk.Context, data map[string]any) (any, error) {
		r.handleToolExecutionStart(data)
		return nil, nil
	})
	e.OnEvent(sdk.EventToolExecutionEnd, func(_ sdk.Context, data map[string]any) (any, error) {
		r.handleToolExecutionEnd(data)
		return nil, nil
	})

	// L2358-2415: message_start.
	e.OnEvent(sdk.EventMessageStart, func(_ sdk.Context, data map[string]any) (any, error) {
		r.handleMessageStart(data)
		return nil, nil
	})

	// L2416-2425: message_end.
	e.OnEvent(sdk.EventMessageEnd, func(_ sdk.Context, data map[string]any) (any, error) {
		r.handleMessageEnd(data)
		return nil, nil
	})

	// L2426-2490: message_update.
	e.OnEvent(sdk.EventMessageUpdate, func(_ sdk.Context, data map[string]any) (any, error) {
		r.handleMessageUpdate(data)
		return nil, nil
	})

	// L2491-2530: agent_end.
	e.OnEvent(sdk.EventAgentEnd, func(_ sdk.Context, _ map[string]any) (any, error) {
		r.handleAgentEnd()
		return nil, nil
	})

	// L2288-2297: /compact-ui-config.
	r.registerConfigCommand(e)

	return e
}

// strPtr is a small helper for the `*string` phase values.
func strPtr(value string) *string { return &value }

// readConfigFile reads a config file.
func readConfigFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// ── production event wiring ─────────────────────────────────────────────────
//
// The TS extension drives the host's own component tree through prototype
// patches, so an event only has to update `runtime`. A PiG subprocess owns no
// host tree, so the same events have to build the ported tree the grouping
// model reads: a chat container, the live assistant message, and a real
// ToolExecutionComponent per tool call. These helpers are that bridge.

// ensureChatContainerLocked creates the default main container the first time
// the runtime needs one. The caller holds mu.
func (r *runtimeState) ensureChatContainerLocked() *container {
	if r.lastChatContainer == nil {
		r.lastChatContainer = newContainer(r)
	}
	return r.lastChatContainer
}

// ensureLiveAssistantLocked mounts the live streaming assistant message in the
// chat container when the host has not supplied one. The caller holds mu.
func (r *runtimeState) ensureLiveAssistantLocked(message *assistantMessage) *assistantMessageComponent {
	r.ensureChatContainerLocked()
	if r.lastStreamingComp == nil {
		content := newContainer(r)
		comp := &assistantMessageComponent{
			contentContainer: content,
			lastMessage:      message,
			isStreaming:      true,
			live:             true,
			streamingComp:    true,
			tui:              r,
		}
		r.assistantContentCont[content] = true
		r.getAssistantContentStateLocked(content)
		r.lastChatContainer.addChild(comp)
		r.lastStreamingComp = comp
	}
	return r.lastStreamingComp
}

// ensureToolComponent mounts the real tool component of a tool_execution_start
// event: the grouped one when the call is already known, otherwise a fresh
// ToolExecutionComponent folded into the open group by the patched addChild
// path (which absorbs a streaming `preparing` entry on the way in).
func (r *runtimeState) ensureToolComponent(id, name string, args map[string]any) *toolComponent {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	r.ensureLiveAssistantLocked(nil)
	existing := r.findGroupedToolByIDLocked(id)
	tui := r.capturedTui
	r.mu.Unlock()
	if existing != nil {
		r.mu.Lock()
		if name != "" {
			existing.toolName = name
		}
		if len(args) > 0 {
			existing.args = args
		}
		existing.executionStarted = true
		existing.isPartial = true
		changed := r.absorbGroupedPreparingLocked(existing)
		r.mu.Unlock()
		for _, group := range changed {
			r.invalidateGroupCardsExcept(group, existing.toolCallID)
		}
		r.invalidateGroupedTool(existing)
		requestRender(tui)
		return existing
	}
	tool := &toolComponent{
		toolCallID:       id,
		toolName:         name,
		args:             args,
		executionStarted: true,
		isPartial:        true,
		tui:              r,
	}
	r.mu.Lock()
	chat := r.lastChatContainer
	chat.addChild(tool)
	r.mu.Unlock()
	r.addChildTo(chat, tool)
	r.mu.Lock()
	tui = r.capturedTui
	r.mu.Unlock()
	requestRender(tui)
	return tool
}

// findGroupedToolByIDLocked is findGroupedTool for a caller that holds mu and
// has only the call id (no content index to match on).
func (r *runtimeState) findGroupedToolByIDLocked(toolCallID string) *toolComponent {
	if toolCallID == "" {
		return nil
	}
	for _, group := range r.groupsListLocked() {
		for _, entry := range group.entries {
			if entry.kind != "tool" || entry.tool == nil {
				continue
			}
			if entry.tool.toolCallID == toolCallID {
				return entry.tool
			}
		}
	}
	return nil
}

// groupOwnerForToolLocked is the group a host card's tool belongs to. The
// caller holds mu. A mounted tool carries its owner directly. A call that is
// still streaming has its toolComponent only in the group entry (nothing has
// mounted it in children yet), so the entry's preparing record names the group;
// the TS tree hangs the preparing object itself on the entry for the same
// reason. Without this the streaming frames fall through to the bare
// single-row branch and the card paints outside the compact tree.
func (r *runtimeState) groupOwnerForToolLocked(tool *toolComponent) *toolGroupComponent {
	if tool == nil {
		return nil
	}
	if tool.owner != nil {
		return tool.owner
	}
	for _, group := range r.groupsListLocked() {
		for _, entry := range group.entries {
			if entry.kind != "tool" || entry.tool != tool || entry.preparing == nil {
				continue
			}
			return entry.preparing.owner
		}
	}
	return nil
}

// applyToolResult records a finished result on the matching grouped tool, so
// the group render and both modals see the arguments, the output, the status
// and the elapsed time of a real execution.
func (r *runtimeState) applyToolResult(id string, result *toolResult) {
	if id == "" {
		return
	}
	r.mu.Lock()
	tool := r.findGroupedToolByIDLocked(id)
	if tool == nil {
		r.mu.Unlock()
		return
	}
	tool.result = result
	tool.isPartial = false
	if result != nil && result.isError {
		tool.interrupted = false
	}
	group := tool.owner
	if group == nil {
		group = r.lastActiveGroup
	}
	r.mu.Unlock()
	// A group only re-renders through a dirty card or its Invalidate hook, and the
	// spinner of a finished call has already stopped: the completed tool may not
	// be the display tool, so every card of the group is invalidated and the one
	// that paints the tree repaints from the shared state.
	r.invalidateGroupCards(group)
}

// replaySessionHistory rebuilds the ported tree of a resumed session: one
// assistant message component per assistant message, its tool calls folded
// into tool groups in stream order, and every tool result applied. session_start
// seals the outcome (sealHistoricalGroups), so the restored history is
// read-only and Ctrl+I / /compact-inspect can inspect it.
func (r *runtimeState) replaySessionHistory(entries []map[string]any) {
	if len(entries) == 0 {
		return
	}
	r.mu.Lock()
	r.ensureChatContainerLocked()
	r.mu.Unlock()
	for _, entry := range entries {
		if asString(entry["type"]) != "message" {
			continue
		}
		raw, ok := asObject(entry["message"])
		if !ok {
			continue
		}
		switch asString(raw["role"]) {
		case "assistant":
			r.replayAssistantMessage(raw)
		case "toolResult":
			r.replayToolResult(raw)
		}
	}
}

// replayAssistantMessage restores one historical assistant message.
func (r *runtimeState) replayAssistantMessage(raw map[string]any) {
	message, ok := decodeAssistantMessage(raw)
	if !ok {
		return
	}
	content, _ := asList(raw["content"])
	blocks := decodeContentBlocks(content)
	r.mu.Lock()
	chat := r.ensureChatContainerLocked()
	contentContainer := newContainer(r)
	assistant := &assistantMessageComponent{
		contentContainer: contentContainer,
		lastMessage:      message,
		tui:              r,
	}
	r.assistantContentCont[contentContainer] = true
	r.getAssistantContentStateLocked(contentContainer)
	chat.addChild(assistant)
	r.lastStreamingComp = assistant
	r.mu.Unlock()
	// A dead message stages its thinking at each visible text boundary, exactly
	// as the patched updateContent does (applyAssistantThinkingPatch).
	r.ensureHistoricalThinkingAnchors(contentContainer, message)
	for _, block := range blocks {
		if block.typ != "toolCall" || block.block.id == "" {
			continue
		}
		tool := &toolComponent{
			toolCallID:   block.block.id,
			toolName:     block.block.name,
			args:         block.block.arguments,
			argsComplete: true,
			tui:          r,
		}
		r.mu.Lock()
		chat.addChild(tool)
		r.mu.Unlock()
		r.addChildTo(chat, tool)
	}
}

// replayToolResult restores one historical tool result.
func (r *runtimeState) replayToolResult(raw map[string]any) {
	id := asString(raw["toolCallId"])
	if id == "" {
		return
	}
	result := &toolResult{timestamp: entryTime(raw, map[string]any{})}
	if items, isList := asList(raw["content"]); isList {
		result.content = decodeResultContent(items)
	}
	if asBool(raw["isError"]) {
		result.isError = true
	}
	r.applyToolResult(id, result)
}

// decodeResultContent decodes the content blocks of a tool result.
func decodeResultContent(list []any) []contentItem {
	out := make([]contentItem, 0, len(list))
	for _, entry := range list {
		block, ok := asObject(entry)
		if !ok {
			continue
		}
		if asString(block["type"]) == "text" {
			out = append(out, contentItem{typ: "text", text: asString(block["text"])})
		}
	}
	return out
}

// asBool reads a decoded JSON boolean.
func asBool(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

// ── tool renderer (PLAN.md §4 registerToolRenderer) ─────────────────────────

// installToolRenderer registers the compact tool-card renderer. It draws the
// same one-line row (and the same status, elapsed time and summary) that
// toolGroupComponent draws for a folded tool, so the host card and the
// in-memory group never disagree.
func (r *runtimeState) installToolRenderer(e *sdk.Extension) {
	e.ToolRenderer(func(toolName string, _ func() *sdk.ToolRenderers) *sdk.ToolRenderers {
		return &sdk.ToolRenderers{
			Shell: sdk.ToolRenderShellSelf,
			// The host ToolExecution card concatenates Call lines then Result
			// lines into one shell. Only Call paints the compact row / group
			// tree; Result adds expanded output body (or nothing when folded).
			Call: func(_ sdk.Context, args map[string]any, render sdk.ToolRenderContext, width int) ([]string, error) {
				return r.toolRenderLines(toolName, args, render, nil, false, width, false), nil
			},
			Result: func(_ sdk.Context, result sdk.ToolRenderResult, options sdk.ToolRenderResultOptions, render sdk.ToolRenderContext, width int) ([]string, error) {
				expanded := options.Expanded || render.Expanded
				var output *string
				if expanded {
					text := toolRenderResultText(result)
					output = &text
				}
				return r.toolRenderLines(toolName, render.Args, render, output, expanded, width, true), nil
			},
		}
	})
}

// toolRenderLines renders one host tool card phase. The host concatenates Call
// then Result into one ShellSelf card (see ToolExecutionComponent.updateDisplay),
// so Call owns the group tree and Result contributes nothing for a grouped call.
// Grouped tools follow the TS hideGroupedToolRender contract: only the group's
// display tool paints, and it paints only the tree; the others return no lines
// so the host hides the card. resultPhase selects the Result half.
func (r *runtimeState) toolRenderLines(name string, args map[string]any, render sdk.ToolRenderContext, output *string, expanded bool, width int, resultPhase bool) []string {
	if width < 1 {
		width = 1
	}
	r.mu.Lock()
	r.rememberToolCardInvalidateLocked(render)
	existing := r.findGroupedToolByIDLocked(render.ToolCallID)
	owner := r.groupOwnerForToolLocked(existing)
	if existing != nil && owner != nil {
		if !isGroupHostDisplayToolLocked(owner, existing) {
			r.mu.Unlock()
			// Empty slice (not nil): host ShellSelf renderSelfShell returns no
			// rows only when len(content)==0; a JSON null on the wire can be
			// confused with "absent frame" in some paths. [] is unambiguous.
			return []string{}
		}
		if resultPhase {
			// hideGroupedToolRender (TS L1100-1110) patches the tool's own render to
			// nothing while the call belongs to a group, and ToolGroupComponent's
			// setExpanded is a no-op (TS L822-823): the original never shows a
			// grouped tool's output body, in any expansion state, because the group
			// owns the whole card. Ctrl+O on a grouped card therefore only re-renders
			// the tree; the output body lives in the detail view (Ctrl+I).
			r.mu.Unlock()
			return []string{}
		}
		r.mu.Unlock()
		lines := owner.Render(width)
		// Host renderSelfShell prepends one blank when content is non-empty
		// (tool_execution_definition.go). A non-anchored group also starts
		// with its own leading blank for in-chat embedding (TS path). On the
		// PiG ToolRenderer path those stack into two blank rows above the
		// group header; drop the group's copy so ShellSelf owns the single
		// separator (report3: blanks <= 1).
		if len(lines) > 0 && lines[0] == "" {
			lines = lines[1:]
		}
		return lines
	}
	if resultPhase {
		// Single-row cards (a lone card the extension never grouped, or a grouped
		// card that is not its group's display tool but still reaches this branch):
		// Call already drew the row/tree; Result only adds body.
		if !expanded || output == nil || strings.TrimSpace(*output) == "" {
			r.mu.Unlock()
			return []string{}
		}
		inner := maxInt(1, width)
		var lines []string
		for _, raw := range strings.Split(strings.TrimRight(*output, "\n"), "\n") {
			wrapped := tui.WrapTextWithAnsi(raw, inner)
			if len(wrapped) == 0 {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, wrapped...)
		}
		r.mu.Unlock()
		return lines
	}
	group := r.lastActiveGroup
	if owner != nil {
		group = owner
	}
	if group == nil {
		group = &toolGroupComponent{tui: r}
	}
	tool := r.toolRenderToolLocked(name, args, render)
	frame := toolSpinnerFrame(r.nowMsLocked(), r.spinnerStartMs)
	lines := []string{group.toolRowLocked("", tool, frame, width, false)}
	r.mu.Unlock()
	return lines
}

// rememberToolCardInvalidateLocked stores the host Invalidate hook for a card.
// The caller holds mu.
func (r *runtimeState) rememberToolCardInvalidateLocked(render sdk.ToolRenderContext) {
	if render.ToolCallID == "" || render.Invalidate == nil {
		return
	}
	if r.toolCardInvalidate == nil {
		r.toolCardInvalidate = map[string]func(){}
	}
	r.toolCardInvalidate[render.ToolCallID] = render.Invalidate
}

// toolRenderToolLocked builds the tool component a ToolRenderContext
// describes. The caller holds mu.
func (r *runtimeState) toolRenderToolLocked(name string, args map[string]any, render sdk.ToolRenderContext) *toolComponent {
	tool := &toolComponent{
		toolCallID:       render.ToolCallID,
		toolName:         name,
		args:             args,
		executionStarted: render.ExecutionStarted,
		argsComplete:     render.ArgsComplete,
		isPartial:        render.IsPartial,
		interrupted:      render.IsError,
		tui:              r,
	}
	if render.DurationMs != nil {
		seconds := float64(*render.DurationMs) / 1e3
		tool.elapsedDur = &seconds
	}
	if existing := r.findGroupedToolByIDLocked(render.ToolCallID); existing != nil {
		// Share the grouped component's state so the card and the group agree.
		tool.owner = existing.owner
		tool.result = existing.result
		tool.interrupted = existing.interrupted || render.IsError
		tool.groupEndAt = existing.groupEndAt
	}
	return tool
}

// toolRenderResultText joins the text blocks of a tool result.
func toolRenderResultText(result sdk.ToolRenderResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		if asString(item["type"]) != "text" {
			continue
		}
		if text := asString(item["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// handleSessionStart is the session_start handler (L2220-2273).
func (r *runtimeState) handleSessionStart(ctx sdk.Context) {
	ui := r.uiContextFor(ctx)
	th := sdkThemeFrom(ctx)

	r.mu.Lock()
	r.agentWorkPhase = nil
	r.currentTheme = th
	r.activeUIContext = ui
	r.acceptingEvents = true
	r.ensureChatContainerLocked()
	r.mu.Unlock()

	if ui == nil {
		return
	}
	ui.setHiddenThinkingLabel("")
	ui.onTerminalInput(r.handleTerminalInput)
	ui.setWidgetRenderer("compact-anim", r.renderWidget)

	// L2260-2262: the resumed session's tool timings, plus the ported tree the
	// inspector reads: without it a restored session shows an empty tool list.
	safeCall(func() {
		entries, err := ctx.SessionManager().GetEntries()
		if err != nil {
			return
		}
		r.loadSessionToolTimings(entries)
		r.replaySessionHistory(entries)
	})

	// L2263-2273: the three installs repeat and the historical groups seal.
	r.installGrouping()
	r.installNativeThinkingSuppression()
	r.installCompactionSummaryRendering(nil)
	r.sealHistoricalGroupsAsync()
}

// handleSessionShutdown is session_shutdown (L2274-2278).
func (r *runtimeState) handleSessionShutdown() {
	r.setAgentWorkPhase(nil)
	r.closeDetailModal()
	r.closeInspectSelectorModal()
	r.stopAnimation()
	r.mu.Lock()
	r.acceptingEvents = false
	r.capturedTui = nil
	r.mu.Unlock()
}

// handleTerminalInput is the onTerminalInput callback of session_start
// (L2228-2258): Ctrl+I opens the inspector, Esc/q close the open overlay.
func (r *runtimeState) handleTerminalInput(data string) terminalInputVerdict {
	if isKeyRelease(data) {
		r.mu.Lock()
		open := r.activeModalHandle != nil || r.activeSelectorHandle != nil || r.activeCustomRoot != nil
		r.mu.Unlock()
		return terminalInputVerdict{consume: open}
	}
	r.mu.Lock()
	hasModal := r.activeModalHandle != nil
	hasSelector := r.activeSelectorHandle != nil || r.activeCustomRoot != nil
	r.mu.Unlock()
	if hasModal {
		if matchesKey(data, "escape") || data == "q" || data == "Q" || isCtrlI(data) {
			r.closeDetailModal()
			return terminalInputVerdict{consume: true}
		}
		return terminalInputVerdict{}
	}
	if hasSelector {
		if matchesKey(data, "escape") || data == "q" || data == "Q" || isCtrlI(data) {
			r.closeInspectSelectorModal()
			return terminalInputVerdict{consume: true}
		}
		return terminalInputVerdict{}
	}
	if isCtrlI(data) {
		r.openInspectSelectorModal()
		return terminalInputVerdict{consume: true}
	}
	return terminalInputVerdict{}
}

// renderWidget is the `setWidget("compact-anim", ...)` renderer (L2259). The
// TS widget renders no lines: it only captures the TUI handle. The port keeps
// that contract and records the width-independent empty frame.
func (r *runtimeState) renderWidget(int) []string { return []string{} }

// setAgentWorkPhase is setAgentWorkPhase (L2192-2196).
func (r *runtimeState) setAgentWorkPhase(phase *string) {
	r.mu.Lock()
	r.agentWorkPhase = phase
	group := r.lastActiveGroup
	tui := r.capturedTui
	r.mu.Unlock()
	if group != nil {
		group.invalidate()
	}
	requestRender(tui)
}

// handleToolExecutionStart is tool_execution_start (L2299-2307).
func (r *runtimeState) handleToolExecutionStart(data map[string]any) {
	id := asString(data["toolCallId"])
	// nowMs takes the lock itself, so read the clock before entering it.
	now := r.nowMs()
	r.mu.Lock()
	r.rememberToolStartLocked(id, now)
	r.agentWorkPhase = strPtr(phaseWorking)
	group := r.lastActiveGroup
	selector := r.activeSelectorComp
	tui := r.capturedTui
	r.mu.Unlock()
	// Mount the real tool component (absorbing a streaming `preparing` entry),
	// so the group, the selector and the detail modal all see the call.
	r.ensureToolComponent(id, asString(data["toolName"]), objectOrNil(data["args"]))
	if group != nil {
		group.invalidate()
	}
	if selector != nil {
		selector.refreshEntries(false)
	}
	requestRender(tui)
}

// handleToolExecutionEnd is tool_execution_end (L2308-2325).
func (r *runtimeState) handleToolExecutionEnd(data map[string]any) {
	id := asString(data["toolCallId"])
	// nowMs takes the lock itself, so read the clock before entering it.
	end := r.nowMs()
	r.mu.Lock()
	r.agentWorkPhase = strPtr(phaseWorking)
	if start, present := r.toolStarts[id]; present {
		if _, exists := r.toolDurations[id]; !exists {
			duration := float64(end-start) / 1e3
			if duration < 0 {
				duration = 0
			}
			r.toolDurations[id] = duration
		}
	}
	for _, group := range r.groupsListLocked() {
		for _, tool := range group.children {
			if tool.toolCallID == id {
				at := end
				tool.groupEndAt = &at
			}
		}
	}
	group := r.lastActiveGroup
	selector := r.activeSelectorComp
	tui := r.capturedTui
	r.mu.Unlock()
	// The finished result belongs on the component, not only in the timing map.
	r.applyToolResult(id, decodeToolResult(data))
	if group != nil {
		group.invalidate()
	}
	if selector != nil {
		selector.refreshEntries(false)
	}
	requestRender(tui)
}

// decodeToolResult reads the result a tool_execution_end event carries, or nil
// when the host reports none.
func decodeToolResult(data map[string]any) *toolResult {
	raw, ok := asObject(data["result"])
	if !ok {
		if asBool(data["isError"]) {
			return &toolResult{isError: true}
		}
		return nil
	}
	result := &toolResult{}
	if items, isList := asList(raw["content"]); isList {
		result.content = decodeResultContent(items)
	}
	result.isError = asBool(raw["isError"]) || asBool(data["isError"])
	if number, ok := toNumber(raw["timestamp"]); ok {
		result.timestamp = int64(number)
	}
	return result
}

// handleMessageStart is message_start (L2358-2415).
func (r *runtimeState) handleMessageStart(data map[string]any) {
	message, _ := asObject(data["message"])
	role := asString(message["role"])
	if role == "user" {
		// nowMs takes the lock itself, so read the clock before entering it.
		now := r.nowMs()
		r.mu.Lock()
		// Every group whose visible state changes in this branch is repainted once
		// after mu is released: the seal below, the finalized run's owner, and the
		// groups whose provisional entries are converted or detached.
		var changed []*toolGroupComponent
		if group := r.lastActiveGroup; group != nil && !group.sealed {
			group.sealed = true
			group.invalidateLocked()
			changed = appendChangedGroup(changed, group)
		}
		r.turnStartMs = now
		// TS L2312-2316 finalizes the previous run *before* the reset clears
		// activeThinking; doing it after would leave a leftover run's `active`
		// flag stuck at true forever.
		changed = appendChangedGroup(changed, r.finalizeActiveThinkingLocked())
		r.resetTurnLocked()
		r.lastStreamingComp = nil
		empties := make([]*toolGroupComponent, 0, len(r.groups))
		for _, group := range r.groupsListLocked() {
			if !group.hasVisibleEntriesLocked() {
				empties = append(empties, group)
			}
		}
		for _, group := range empties {
			r.removeGroupFromContainer(r.lastChatContainer, group)
			r.deleteGroupLocked(group)
		}
		// A provisional call whose stream already carried a real id has a host card on
		// screen: the host mounts one as soon as the streamed block has an id, and the
		// card keeps painting outside the compact tree once the entry is gone. Such a
		// call is converted into an interrupted group entry in place -- the same policy
		// handleAgentEnd applies, so the two turn-end paths cannot disagree (brief2 R4
		// C1). Only a call whose stream never carried an id, which has no card, is
		// detached. The index order is the stream order, as in handleAgentEnd.
		indices := make([]int, 0, len(r.preparingByIndex))
		for index := range r.preparingByIndex {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			tool := r.preparingByIndex[index]
			if tool == nil {
				continue
			}
			changed = appendChangedGroup(changed, tool.owner)
			if group := tool.owner; group != nil && hostCardForCallID(tool.toolCallID) {
				r.interruptPreparingToolLocked(tool, now)
				continue
			}
			r.detachPreparingToolLocked(tool)
		}
		r.preparingByIndex = map[int]*preparingTool{}
		r.mu.Unlock()
		for _, group := range changed {
			r.invalidateGroupCards(group)
		}
		return
	}
	if role == "assistant" {
		r.finalizeActiveThinking()
		r.mu.Lock()
		// assistantMessageStartLocked clears lastStreamingComp, so it has to run
		// *before* the live assistant is mounted: mounting first and resetting
		// afterwards would drop the freshly mounted component and the first
		// thinking stream of the turn would find no streaming mount point
		// (ensureThinkingGroup / openToolGroup bail on a nil one).
		r.assistantMessageStartLocked()
		// The production host mounts the live assistant message itself; without
		// one yet (a resumed session, or an event before the first addChild) the
		// port creates it so the turn has somewhere to put groups.
		if message, ok := decodeAssistantMessage(data["message"]); ok {
			r.ensureLiveAssistantLocked(message)
		}
		r.mu.Unlock()
		r.setAgentWorkPhase(strPtr(phaseWaitingResponse))
	}
}

// handleMessageEnd is message_end (L2416-2425).
func (r *runtimeState) handleMessageEnd(data map[string]any) {
	message, ok := decodeAssistantMessage(data["message"])
	if !ok || message.role != "assistant" {
		return
	}
	r.mu.Lock()
	active := r.activeThinking != nil
	r.mu.Unlock()
	if active {
		r.updateActiveThinkingTokens(message)
	}
	r.finalizeActiveThinking()
	r.refreshSealedThinkingTokens(message.usage.reasoningPtr())
	r.setAgentWorkPhase(strPtr(phaseWorking))
	r.mu.Lock()
	selector := r.activeSelectorComp
	r.mu.Unlock()
	if selector != nil {
		selector.refreshEntries(false)
	}
}

// reasoningPtr returns the message's reasoning token count.
func (u *usage) reasoningPtr() *int {
	if u == nil {
		return nil
	}
	return u.reasoning
}

// handleMessageUpdate is message_update (L2426-2490).
func (r *runtimeState) handleMessageUpdate(data map[string]any) {
	message, ok := decodeAssistantMessage(data["message"])
	if !ok || message.role != "assistant" {
		return
	}
	rawContent, _ := asList(data["message"].(map[string]any)["content"])
	content := decodeContentBlocks(rawContent)
	streamEvent := decodeStreamEvent(data["assistantMessageEvent"])
	switch {
	case hasPrefix(streamEvent.typ, "thinking_"):
		r.handleThinkingDelta(message, content, streamEvent)
	case hasPrefix(streamEvent.typ, "text_"):
		r.handleTextDelta(message, content, streamEvent)
	case hasPrefix(streamEvent.typ, "toolcall_"):
		r.mu.Lock()
		r.agentWorkPhase = strPtr(phaseWorking)
		r.mu.Unlock()
		r.mu.Lock()
		active := r.activeThinking != nil
		r.mu.Unlock()
		if active {
			r.updateActiveThinkingTokens(message)
		}
		r.noteStreamingToolCall(content, streamEvent)
	case streamEvent.typ == "done" || streamEvent.typ == "error":
		r.mu.Lock()
		r.agentWorkPhase = strPtr(phaseWorking)
		r.mu.Unlock()
		r.mu.Lock()
		active := r.activeThinking != nil
		r.mu.Unlock()
		if active {
			r.updateActiveThinkingTokens(message)
		}
		r.finalizeActiveThinking()
		r.refreshSealedThinkingTokens(message.usage.reasoningPtr())
	}
	r.mu.Lock()
	group := r.lastActiveGroup
	selector := r.activeSelectorComp
	tui := r.capturedTui
	r.mu.Unlock()
	if group != nil {
		group.invalidate()
	}
	if selector != nil {
		selector.refreshEntries(false)
	}
	requestRender(tui)
}

// handleThinkingDelta is the thinking_ branch of message_update (L2430-2450).
func (r *runtimeState) handleThinkingDelta(message *assistantMessage, content []contentBlock, event streamEvent) {
	hasIndex := event.contentIndex != nil
	if hasIndex {
		r.mu.Lock()
		sealed := r.sealedThinkingIndexes[*event.contentIndex]
		r.mu.Unlock()
		if sealed {
			return
		}
	}
	phase := phaseWorking
	if event.typ == "thinking_end" {
		phase = phaseWaitingOutput
	}
	r.mu.Lock()
	r.agentWorkPhase = strPtr(phase)
	// TS L2369: `streamType === "thinking_start" || !runtime.activeThinking ||
	// hasIndex && runtime.activeThinkingIndex !== contentIndex`. The last
	// disjunct is JS `!==` on a nullable index, so it also holds when no index
	// was recorded yet -- the first indexed event after an un-indexed run starts
	// a new run.
	startsRun := event.typ == "thinking_start" || r.activeThinking == nil ||
		(hasIndex && (r.activeThinkingIndex == nil || *r.activeThinkingIndex != *event.contentIndex))
	r.mu.Unlock()
	if startsRun {
		r.finalizeActiveThinking()
		r.mu.Lock()
		if hasIndex {
			index := *event.contentIndex
			r.activeThinkingIndex = &index
		} else {
			r.activeThinkingIndex = nil
		}
		r.createThinkingEntryLocked()
		r.mu.Unlock()
	}
	// TS L2376: the block's own thinking text wins; streamEvent.content is only
	// the fallback for a thinking_end whose block is not a thinking block.
	// Overwriting unconditionally would blank a run when the terminal event
	// carries no content.
	blockText := ""
	if hasIndex && *event.contentIndex >= 0 && *event.contentIndex < len(content) && content[*event.contentIndex].typ == "thinking" {
		blockText = content[*event.contentIndex].block.thinking
	} else if event.typ == "thinking_end" {
		blockText = event.content
	}
	if hasIndex {
		r.setThinkingBlock(*event.contentIndex, blockText)
	}
	r.updateActiveThinkingTokens(message)
	r.ensureThinkingGroup()
	if event.typ == "thinking_end" {
		r.finalizeActiveThinking()
	}
}

// handleTextDelta is the text_ branch of message_update (L2451-2467).
func (r *runtimeState) handleTextDelta(message *assistantMessage, content []contentBlock, event streamEvent) {
	r.mu.Lock()
	r.agentWorkPhase = strPtr(phaseWorking)
	r.mu.Unlock()
	hasIndex := event.contentIndex != nil
	text := ""
	if hasIndex && *event.contentIndex >= 0 && *event.contentIndex < len(content) {
		block := content[*event.contentIndex]
		if block.typ == "text" {
			text = block.block.text
		}
	}
	if text == "" || !hasIndex {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	r.mu.Lock()
	handled := r.handledTextIndexes[*event.contentIndex]
	active := r.activeThinking != nil
	r.mu.Unlock()
	if handled {
		return
	}
	if active {
		r.updateActiveThinkingTokens(message)
	}
	ordinal := 0
	for i := 0; i < len(content) && i <= *event.contentIndex; i++ {
		if content[i].typ == "text" && strings.TrimSpace(content[i].block.text) != "" {
			if i == *event.contentIndex {
				break
			}
			ordinal++
		}
	}
	r.mu.Lock()
	r.handledTextIndexes[*event.contentIndex] = true
	r.pendingTextSeal = true
	r.pendingTextOrdinal = &ordinal
	r.mu.Unlock()
	r.flushPendingTextSeal()
}

// handleAgentEnd is agent_end (L2491-2530).
func (r *runtimeState) handleAgentEnd() {
	// nowMs takes the lock itself, so read the clock before entering it.
	now := r.nowMs()
	r.mu.Lock()
	r.agentWorkPhase = nil
	// The index order is the stream order: map iteration must not decide which
	// provisional call is converted first (the sibling invalidations of all of
	// them are collected together below).
	indices := make([]int, 0, len(r.preparingByIndex))
	for index := range r.preparingByIndex {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	// A provisional call whose stream already carried a real id has a host card
	// on screen: the host mounts one as soon as the streamed block carries an id
	// and keeps it when the turn is aborted (Esc/error) without a
	// tool_execution_start. The original keeps that very component inside the
	// group (addTool moves it into group.children, and the agent_end walk marks
	// it _interrupted), so it renders as an interrupted row inside the tree.
	// Detaching it instead left the card with no group -- a bare row outside the
	// tree -- and the sibling card that had already yielded the display rights
	// stayed blank, so the whole group vanished. Only a call with no id at all,
	// which has no card, is still detached.
	var changed []*toolGroupComponent
	for _, index := range indices {
		tool := r.preparingByIndex[index]
		if tool == nil {
			continue
		}
		// Every card of a changed group is repainted once after mu is released.
		changed = appendChangedGroup(changed, tool.owner)
		if group := tool.owner; group != nil && hostCardForCallID(tool.toolCallID) {
			r.interruptPreparingToolLocked(tool, now)
			continue
		}
		r.detachPreparingToolLocked(tool)
	}
	r.preparingByIndex = map[int]*preparingTool{}
	if group := r.lastActiveGroup; group != nil && !group.sealed {
		group.sealed = true
		// Sealing flips the header to its final state and stops the spinner; the
		// card keeps its last frame until its Invalidate hook fires.
		group.invalidateLocked()
		changed = appendChangedGroup(changed, group)
	}
	// The walk below mutates already-sealed groups too: it stamps a missing group
	// end and marks an unfinished call interrupted, both of which change what the
	// tree shows, so a mutated group joins the repaint list even when it was
	// sealed long ago.
	for _, group := range r.groupsListLocked() {
		mutated := false
		for _, tool := range group.children {
			if tool.groupEndAt == nil && !r.hasDurationLocked(tool.toolCallID) {
				at := now
				tool.groupEndAt = &at
				mutated = true
			}
			if tool.result == nil && !r.hasDurationLocked(tool.toolCallID) {
				r.interruptedToolCallID[tool.toolCallID] = true
				tool.interrupted = true
				mutated = true
			}
		}
		if mutated {
			group.invalidateLocked()
			changed = appendChangedGroup(changed, group)
		}
	}
	// TS L2410-2428 order: finalize the active run *before* the empty-group
	// cleanup. Clearing `active` drops an empty run out of visibleEntries, so a
	// group holding nothing but an empty thinking run is empty by the time the
	// cleanup looks at it; checking first left exactly that group behind as a
	// zombie empty group on screen (brief2 R4 C2).
	changed = appendChangedGroup(changed, r.finalizeActiveThinkingLocked())
	empties := make([]*toolGroupComponent, 0, len(r.groups))
	for _, group := range r.groupsListLocked() {
		if !group.hasVisibleEntriesLocked() {
			empties = append(empties, group)
		}
	}
	for _, group := range empties {
		r.removeGroupFromContainer(r.lastChatContainer, group)
		r.deleteGroupLocked(group)
	}
	selector := r.activeSelectorComp
	tui := r.capturedTui
	elapsedMs := now - r.turnStartMs
	r.mu.Unlock()
	for _, group := range changed {
		r.invalidateGroupCards(group)
	}
	r.refreshSealedThinkingTokens(nil)
	if selector != nil {
		selector.refreshEntries(false)
	}
	requestRender(tui)
	r.insertTurnDivider(elapsedMs)
	r.mu.Lock()
	r.resetTurnLocked()
	r.mu.Unlock()
}

// sealHistoricalGroups is the sealHistoricalGroups closure of session_start
// (L2263-2271): seal the resumed session's groups and mark their unfinished
// tool calls interrupted. It seals exactly the groups in `historical` -- the
// set session_start captured -- so a turn that starts while the async seal is
// still pending is never sealed by it.
func (r *runtimeState) sealHistoricalGroups() {
	r.mu.Lock()
	historical := r.groupsListLocked()
	r.mu.Unlock()
	r.sealHistoricalGroupsFrom(historical)
}

// sealHistoricalGroupsFrom seals the given historical groups.
func (r *runtimeState) sealHistoricalGroupsFrom(historical []*toolGroupComponent) {
	// nowMs takes the lock itself, so read the clock before entering it.
	now := r.nowMs()
	r.mu.Lock()
	inHistorical := make(map[*toolGroupComponent]bool, len(historical))
	for _, group := range historical {
		inHistorical[group] = true
	}
	for _, group := range r.groupsListLocked() {
		if !inHistorical[group] {
			continue
		}
		group.sealed = true
		for _, tool := range group.children {
			if tool.groupEndAt == nil {
				at := now
				tool.groupEndAt = &at
			}
			if tool.result == nil && !r.hasDurationLocked(tool.toolCallID) {
				r.toolDurations[tool.toolCallID] = 0
				r.interruptedToolCallID[tool.toolCallID] = true
				tool.interrupted = true
			}
		}
	}
	if r.lastActiveGroup != nil && inHistorical[r.lastActiveGroup] {
		r.lastActiveGroup = nil
	}
	// A new turn may already have opened an unsealed group; stopping the
	// animation tick then would freeze its spinner.
	anyOpen := false
	for _, group := range r.groupsListLocked() {
		if !group.sealed {
			anyOpen = true
			break
		}
	}
	tui := r.capturedTui
	groups := r.groupsListLocked()
	r.mu.Unlock()
	// Sealing stamps an end on every unfinished call and marks it interrupted, so
	// each group's tree changed and its cards have to repaint -- including the
	// historical ones a `/resume` restored, whose cards otherwise kept the frame
	// they were first drawn with.
	for _, group := range groups {
		r.invalidateGroupCards(group)
	}
	if !anyOpen {
		r.stopAnimation()
	}
	requestRender(tui)
}

// sealHistoricalGroupsAsync is `queueMicrotask(sealHistoricalGroups)` followed
// by `setTimeout(sealHistoricalGroups, 0)` (L2272-2273). The port runs it once
// (the second call is idempotent) on its own goroutine so session_start
// returns promptly, as the TS microtask does. The group set is snapshotted
// here, at session_start, so the delayed seal cannot touch a group a new turn
// created in the meantime.
func (r *runtimeState) sealHistoricalGroupsAsync() {
	r.mu.Lock()
	historical := r.groupsListLocked()
	r.mu.Unlock()
	go r.sealHistoricalGroupsFrom(historical)
}

// hasPrefix is strings.HasPrefix under the name the ported switch reads well
// with.
func hasPrefix(text, prefix string) bool {
	return len(text) >= len(prefix) && text[:len(prefix)] == prefix
}
