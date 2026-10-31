package compactui

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file is the adapter layer for the "native thinking row" host seam: every
// way this extension suppresses or hides the host's own thinking render lives
// here, so the upstream PiG 0.6.0 hook (issue #384: the extension takes over
// rendering the hidden-thinking row, collapsing it to zero lines) has exactly
// one file to land in.
//
// The hook's shape is not known yet -- it may be a bool flag, an RPC, or an SDK
// method -- so this file converges the *call sites*, not the interface. When
// the hook ships, the single place to change is applyNativeThinkingSuppression
// (call the new hook, or negotiate with it), plus possibly the keep/drop
// decision on transformMarkdown below. handleSessionStart and every other
// caller must not need to know which primitive is underneath.
//
// Two different seams are folded into this one file because they implement the
// same suppression semantics:
//   - applyNativeThinkingSuppression / setHiddenThinkingLabel work on the
//     hidden-state row the host draws for a hidden thinking block;
//   - transformMarkdown works on the "assistant-thinking" Markdown body.
// installNativeThinkingSuppression / ensureAssistantThinkingPatched are the
// local mirror of that semantics (component flags + the installed record); they
// belong to the same boundary even though they never touch the host.

// applyNativeThinkingSuppression makes the host's native thinking render
// invisible / zero-footprint for us. Its body today is the one primitive the
// host exposes -- setHiddenThinkingLabel("") blanks the hidden-state label so
// the hidden thinking row draws one empty stub line -- but the semantic is the
// suppression, not the label.
//
// When the upstream #384 hook lands in PiG 0.6.0, this method body is the only
// place that changes: call the new hook (or negotiate with it) here, and the
// caller handleSessionStart stays untouched.
func (r *runtimeState) applyNativeThinkingSuppression(ui uiContext) {
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
//     whatever hideThinkingBlock is set to (R3 concluded this was impossible; R4
//     B disproved it against the host code and a live frame).
//
// Everything else is returned unchanged, so ordinary assistant/user Markdown
// rendering is exactly the host's (including the Mermaid transformer that runs
// in the same chain).
//
// The upstream #384 hook takes over the *hidden-state row render*; this
// transformer suppresses the *body markdown*. The two are different seams, so
// whether the transformer stays once the hook lands is decided by the hook's
// semantics -- it is kept as-is for now.
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
