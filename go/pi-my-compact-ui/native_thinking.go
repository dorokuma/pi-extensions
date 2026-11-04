package compactui

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file is the adapter layer for the "native thinking row" host seam: every
// way this extension suppresses or hides the host's own thinking render lives
// here. Upstream PiG 0.6.0 (issue #384 / D214) exposes OwnThinkingRuns so a
// hidden or zero-row thinking run leaves no stub and no spacer rows; that hook
// is called from applyNativeThinkingSuppression via a soft interface probe so
// 0.5.3 staged SDKs without the method still compile. The older
// setHiddenThinkingLabel seam stays as a fallback for those hosts.
//
// Call-site contract: applyNativeThinkingSuppression is the sole caller of the
// ownThinkingRuns and setHiddenThinkingLabel primitives for this seam, and
// transformMarkdown is the sole registered assistant-thinking seam. New host
// primitives for this seam land on uiContext/sdkUI as a pair per the
// accessors.go contract. handleSessionStart stays agnostic.
//
// Two seams share this file because they implement the same suppression goal:
//   - applyNativeThinkingSuppression / ownThinkingRuns tell the host this
//     extension owns thinking runs (no stub, no spacers around empty/hidden runs);
//     setHiddenThinkingLabel("") is the 0.5.3 fallback (empty stub label) and is
//     a no-op for layout once 0.6.0 has owned the runs;
//   - transformMarkdown empties the "assistant-thinking" Markdown body so the
//     run draws zero rows (OwnThinkingRuns then drops the surrounding spacers).
// installNativeThinkingSuppression / ensureAssistantThinkingPatched are the
// local mirror of that semantics (component flags + the installed record); they
// belong to the same boundary even though they never touch the host.

// applyNativeThinkingSuppression declares ownership of thinking runs on the
// host (D214 OwnThinkingRuns), then clears the hidden-thinking label as the
// 0.5.3 fallback. Order matters: own first so a 0.6.0 host skips the stub
// entirely; the empty label then only matters on older hosts that still draw
// a hidden-thinking stub. The transformer (transformMarkdown) still empties
// the body; this method only flips the host ownership / label seams.
func (r *runtimeState) applyNativeThinkingSuppression(ui uiContext) {
	ui.ownThinkingRuns(true)
	ui.setHiddenThinkingLabel("")
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
//     whatever hideThinkingBlock is set to.
//
// Everything else is returned unchanged, so ordinary assistant/user Markdown
// rendering is exactly the host's (including the Mermaid transformer that runs
// in the same chain).
//
// OwnThinkingRuns (D214) removes the spacers around a zero-row or hidden run;
// this transformer still empties the body so the run draws no rows. Both are
// required for the compact-ui layout: empty body + owned spacers.
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
