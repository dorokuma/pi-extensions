package compactui

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file ports src/assistant-patches.ts (L193-521): the historical-thinking
// anchors of a resumed session, the native-thinking suppression patch, the
// turn divider and the phantom-padding cleanup.
//
// The TS installs its behaviour by patching AssistantMessageComponent.prototype
// (updateContent / setHideThinkingBlock / setHiddenThinkingLabel). A subprocess
// extension cannot patch the host's prototypes, so the port applies the same
// transformation to the message map it receives on each host event and records
// the historical anchors itself; installNativeThinkingSuppression here marks the
// patch as installed and normalises the flags a component carries
// (hideThinkingBlock = true, hiddenThinkingLabel = ""), which is the visible
// effect the TS patch has on rendering.

// thinkingAlreadyShown is thinkingAlreadyShown (L197-211): the same run text
// already reached a group or the active entry.
func (r *runtimeState) thinkingAlreadyShown(text string) bool {
	norm := strings.TrimSpace(text)
	if norm == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeThinking != nil && strings.TrimSpace(r.activeThinking.text) == norm {
		return true
	}
	for _, entry := range r.unattachedThinking {
		if strings.TrimSpace(entry.text) == norm {
			return true
		}
	}
	for _, group := range r.groupsListLocked() {
		for _, entry := range group.entries {
			if entry.kind == "thinking" && entry.think != nil && strings.TrimSpace(entry.think.text) == norm {
				return true
			}
		}
	}
	return false
}

// stageHistoricalThinking is stageHistoricalThinking (L212-238): a sealed,
// anchored group holding the thinking runs that precede one text block of a
// historical assistant message.
func (r *runtimeState) stageHistoricalThinking(state *assistantContentState, ordinal int, texts []string, message *assistantMessage) {
	if state == nil {
		return
	}
	r.mu.Lock()
	if _, present := state.anchors[ordinal]; present {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	seen := map[string]bool{}
	fresh := make([]string, 0, len(texts))
	for _, text := range texts {
		if strings.TrimSpace(text) == "" || seen[text] {
			continue
		}
		seen[text] = true
		if r.thinkingAlreadyShown(text) {
			continue
		}
		fresh = append(fresh, text)
	}
	if len(fresh) == 0 {
		return
	}
	group := r.newToolGroup(r.nowMs())
	group.anchored = true
	group.sealed = true
	reasoning, hasReasoning := messageReasoningTokens(message)
	tokensExact := hasReasoning && reasoning > 0
	r.mu.Lock()
	for _, text := range fresh {
		tokens := estimateTextTokens(text)
		if tokensExact {
			tokens = reasoning
		}
		entry := &thinkingEntry{
			kind:        "thinking",
			id:          r.thinkingEntrySeq,
			text:        text,
			tokens:      tokens,
			tokensExact: tokensExact,
			owner:       group,
		}
		r.thinkingEntrySeq++
		group.entries = append(group.entries, groupEntry{kind: "thinking", think: entry})
	}
	r.mu.Unlock()
	if !group.hasVisibleEntries() {
		return
	}
	r.mu.Lock()
	state.anchors[ordinal] = group
	r.addGroupLocked(group)
	r.mu.Unlock()
}

// ensureHistoricalThinkingAnchors is ensureHistoricalThinkingAnchors
// (L239-259): stage the thinking of a historical (not streaming) message at
// each visible text boundary.
func (r *runtimeState) ensureHistoricalThinkingAnchors(contentContainer *container, message *assistantMessage) {
	if contentContainer == nil || message == nil || len(message.content) == 0 {
		return
	}
	r.mu.Lock()
	state := r.getAssistantContentStateLocked(contentContainer)
	r.mu.Unlock()
	current := make([]string, 0, len(message.content))
	textOrdinal := 0
	hasToolCalls := false
	for _, item := range message.content {
		switch item.typ {
		case "thinking":
			text := strings.TrimSpace(item.thinking)
			if text != "" {
				current = append(current, text)
			}
		case "text":
			if strings.TrimSpace(item.text) != "" {
				r.stageHistoricalThinking(state, textOrdinal, current, message)
				current = nil
				textOrdinal++
			}
		case "toolCall":
			hasToolCalls = true
		}
	}
	if !hasToolCalls && textOrdinal == 0 && len(current) > 0 {
		r.stageHistoricalThinking(state, 0, current, message)
	}
}

// installNativeThinkingSuppression is installNativeThinkingSuppression
// (L260-334). The TS patches AssistantMessageComponent.prototype so the native
// render never sees a thinking block; a subprocess extension cannot patch a host
// prototype, so the same visible effect is produced where the host does expose a
// seam: transformMarkdown drops the "assistant-thinking" Markdown, and this
// function keeps the flag/state half (the component's thinking flags, plus the
// record that the suppression is installed). The content rewrite the TS performs
// on its own tree copy still happens in applyAssistantThinkingPatch.
func (r *runtimeState) installNativeThinkingSuppression() {
	r.mu.Lock()
	r.thinkingSuppressionInstalled = true
	r.mu.Unlock()
}

// markdownMessageTypeAssistantThinking is Pi's MarkdownTransformContext
// messageType for the reasoning trace of an assistant turn. The host sets it on
// the thinking Markdown component of an assistant message
// (tui/assistant_message_block.go:109 SetThinkingMarkdownTransform) and on the
// async transform pair (internal/codingagent/interactive_chat.go:72), and
// re-exports it as markdowntransform.MarkdownMessageAssistantThinking
// (coding/extension/markdowntransform/markdowntransform.go).
const markdownMessageTypeAssistantThinking = "assistant-thinking"

// transformMarkdown is the display-only Markdown transformer the factory
// registers. It is the PiG form of the TS installNativeThinkingSuppression
// (bundle L256-334, which filters thinking blocks out of the native
// AssistantMessageComponent content), now that the host seam makes it possible:
//
//   - the host runs every registered transformer over the assistant markdown
//     *and* over the assistant-thinking markdown (interactive_chat.go:193-197
//     assistantMarkdownTransform is installed for both message types);
//   - tui/markdown.go:297-318 renders the transformed text, and when its
//     widthx.JSTrim is empty it returns []string{} -- zero lines, not one blank
//     line. That is why returning "" suppresses the native thinking body
//     whatever hideThinkingBlock is set to (R3 concluded this was impossible; R4
//     B disproved it against the host code and a live frame).
//
// Everything else is returned unchanged, so ordinary assistant/user Markdown
// rendering is exactly the host's (including the Mermaid transformer that runs
// in the same chain).
//
// The extension's own tree rows never pass through here: thinkingRow draws a
// plain one-line string through the ToolRenderer (toolRenderLines), so the
// in-tree thinking row of a group and the host's native thinking block are two
// independent paths -- see TestMarkdownTransformerLeavesTreeThinkingRow.
func (r *runtimeState) transformMarkdown(markdown string, context sdk.MarkdownTransformContext) string {
	if context.MessageType == markdownMessageTypeAssistantThinking {
		return ""
	}
	return markdown
}

// transformMarkdownFunc pins transformMarkdown to the shape
// sdk.Extension.MarkdownTransformer accepts, so the factory registration below
// cannot drift from the method (the SDK keeps its transformer field private and
// offers no getter, so this is the compile-time half of that proof).
var transformMarkdownFunc sdk.MarkdownTransformerFunc = (*runtimeState)(nil).transformMarkdown

// ensureAssistantThinkingPatched is ensureAssistantThinkingPatched (L335-344):
// it marks the component patched and reports whether it already was.
func (r *runtimeState) ensureAssistantThinkingPatched(assistant *assistantMessageComponent) bool {
	if assistant == nil {
		return false
	}
	r.installNativeThinkingSuppression()
	r.mu.Lock()
	r.thinkingSuppressionInstalled = true
	r.mu.Unlock()
	assistant.hideThinking = true
	assistant.hiddenThinking = ""
	return true
}

// applyAssistantThinkingPatch is the body of the patched updateContent
// (L268-333) for one host message: it stages the historical anchors of a dead
// message and returns the content the host should render (thinking blocks
// removed). The port never mutates the host's message.
func (r *runtimeState) applyAssistantThinkingPatch(contentContainer *container, message *assistantMessage, isStreaming bool) []contentItem {
	if message == nil {
		return nil
	}
	r.mu.Lock()
	live := isStreaming
	r.mu.Unlock()
	if live {
		// The patched updateContent sets LIVE_ASSISTANT_KEY for a streaming
		// message; the live flag only gates the historical staging below.
	}
	hasThinking := false
	hasToolCalls := false
	hasText := false
	for _, item := range message.content {
		switch item.typ {
		case "thinking":
			hasThinking = true
		case "toolCall":
			hasToolCalls = true
		case "text":
			if strings.TrimSpace(item.text) != "" {
				hasText = true
			}
		}
	}
	if !live && contentContainer != nil {
		r.ensureHistoricalThinkingAnchors(contentContainer, message)
	}
	if !hasThinking {
		return message.content
	}
	filtered := make([]contentItem, 0, len(message.content))
	for _, item := range message.content {
		if item.typ == "thinking" {
			continue
		}
		filtered = append(filtered, item)
	}
	if !live && !hasToolCalls && !hasText && contentContainer != nil {
		r.mu.Lock()
		state := r.assistantContentStates[contentContainer]
		var group *toolGroupComponent
		if state != nil {
			group = state.anchors[0]
		}
		r.mu.Unlock()
		if group != nil && group.hasVisibleEntries() && !contentContainer.contains(group) {
			r.mu.Lock()
			r.detachGroupLocked(group, contentContainer)
			r.mu.Unlock()
			contentContainer.spliceInsert(contentContainer.len(), 0, newSpacer(1), group, newSpacer(1))
			group.mount = contentContainer
		}
	}
	return filtered
}

// turnDividerRender is TurnDividerComponent.render (L367-381).
func (d *turnDividerComponent) renderWidth(width int) []string {
	var th theme
	if d.tui != nil {
		d.tui.mu.Lock()
		th = d.tui.themeLocked()
		d.tui.mu.Unlock()
	}
	middle := "worked for " + d.timeLabel
	avail := maxInt(6, width-tuiLen(middle)-2)
	left := avail / 2
	right := avail - left
	dash := func(n int) string {
		if n <= 0 {
			return ""
		}
		return strings.Repeat("─", n)
	}
	line := fgOf(th, "dim", dash(left)) + " " + fgOf(th, "muted", middle) + " " + fgOf(th, "dim", dash(right))
	return []string{truncateWidth(line, maxInt(1, width)), ""}
}

// Render implements component.
func (d *turnDividerComponent) Render(width int) []string { return d.renderWidth(width) }
