package autocontinue

import "time"

// Premature stop facts and judgement.
//
// TS reference: auto-continue.ts:302-500 (PrematureStopLike, PrematureStopFacts,
// isPrematureStop, PREMATURE_HINTS).

// PrematureStopFacts mirrors TS PrematureStopFacts (auto-continue.ts:317-329).
// Every field is injectable so the judgement never depends on a real clock or a
// real event stream. Nil pointers mean "no evidence", exactly like TS's
// undefined, and each timestamp is filtered through [positiveTs] first.
type PrematureStopFacts struct {
	// Messages is the run's messages (agent_end.messages): the fallback source
	// for toolUse activity and the last human user message.
	Messages []map[string]any
	// LastToolUseTs is the newest toolUse timestamp the extension tracked
	// itself; it wins over scanning Messages.
	LastToolUseTs *float64
	// LastUserMessageTs is the timestamp of the newest human user message (the
	// extension's own "继续" does not count).
	LastUserMessageTs *float64
	// RunStartTs is the run's start (agent_start), the parallel run-duration
	// evidence for gate A.
	RunStartTs *float64
	// Now is the injected current time; nil means "the caller did not inject
	// one" and the call site supplies it (TS falls back to Date.now()).
	Now *float64
}

// IsPrematureStop mirrors TS isPrematureStop (auto-continue.ts:446-497): the
// second continuation entry point, next to IsContinueWorthyError.
//
// Structure (either shape is suspicious):
//
//	a) no text block at all — thinking-only, or a wholly empty message;
//	b) text that stops mid-sentence (：/，/、/； ending, unclosed ``` fence, a
//	   markdown heading or empty list item with nothing after it).
//
// A stop carrying a toolCall is never touched (the model still wants to work).
//
// Two gates must both pass so a normal completion is not caught:
//
//	A) activity evidence (either is enough): toolUse activity within the last
//	   5 minutes, or a run that has lasted more than 90 seconds;
//	B) more than 90 seconds since the last human user message.
func IsPrematureStop(message map[string]any, facts PrematureStopFacts) Verdict {
	if message == nil || roleOf(message) != "assistant" {
		return verdict(false, "no-assistant")
	}
	if stopReasonOf(message) != "stop" {
		// error / length / toolUse / aborted / deferred are not this gate's
		// business: error goes through IsContinueWorthyError, the rest never
		// continue.
		return verdict(false, "stopReason:"+stopReasonOr(message))
	}
	if hasToolCallBlock(message["content"]) {
		return verdict(false, "stop:has-tool-call")
	}

	text := blockText(message["content"])
	var shape Verdict
	switch {
	case jsTrim(text) == "":
		if hasThinkingBlock(message["content"]) {
			shape = verdict(true, "stop:premature-thinking-only")
		} else {
			shape = verdict(true, "stop:premature-empty-content")
		}
	case LooksTruncatedText(text):
		shape = verdict(true, "stop:premature-truncated-text")
	default:
		shape = verdict(false, "stop:complete-text")
	}
	if !shape.OK {
		return shape
	}

	now := positiveTs(facts.Now)
	if now == nil {
		// TS: `positiveTs(facts.now) ?? Date.now()`.
		current := float64(time.Now().UnixMilli())
		now = &current
	}

	// Gate A: toolUse activity within the window, or a long run. Both widen the
	// pass set, but at least one piece of evidence is still required.
	toolUseTs := positiveTs(facts.LastToolUseTs)
	if toolUseTs == nil {
		toolUseTs = lastToolUseTsFromMessages(facts.Messages)
	}
	runStartTs := positiveTs(facts.RunStartTs)
	hasRecentToolUse := toolUseTs != nil && *now-*toolUseTs <= ToolUseWindowMs
	hasLongRun := runStartTs != nil && *now-*runStartTs > RunActiveMinMs
	if !hasRecentToolUse && !hasLongRun {
		return verdict(false, "premature:no-activity-evidence")
	}

	// Gate B: more than 90 seconds without progress since the last human user
	// message.
	userTs := positiveTs(facts.LastUserMessageTs)
	if userTs == nil {
		userTs = lastUserTsFromMessages(facts.Messages)
	}
	if userTs == nil || *now-*userTs <= UserProgressStallMs {
		return verdict(false, "premature:user-message-too-recent")
	}

	return shape
}

// PrematureHints mirrors TS PREMATURE_HINTS (auto-continue.ts:501-505): reason
// code to human hint. Only used in UI notifications; the reason code itself
// remains the decision's authority.
var PrematureHints = map[string]string{
	"stop:premature-thinking-only":  "疑似早衰 stop：只有 thinking 没有正文",
	"stop:premature-truncated-text": "疑似早衰 stop：正文中途断句",
	"stop:premature-empty-content":  "疑似早衰 stop：正文与 thinking 都是空的",
}
