package herdsman

// Port of test/unit/herdsman-pi-wake.test.ts (projection, formatting and
// dead-letter accounting). The TS `event()` builder is `wakeEvent` here; the
// `publishedOutcomeKind` fixture (a verbatim snapshot of the released 0.15.0
// outcomeKind) is ported next to it so the backward-compatibility assertions
// keep running against the published behaviour instead of a hand-written
// imitation.

import (
	"testing"
)

// ── fixtures ────────────────────────────────────────────────────────────────

func wakeEvent(id int, kind string, payload map[string]any, options *eventOptions) AgentEventWireRecord {
	if options == nil {
		options = &eventOptions{}
	}
	text := "assistant result " + formatInt(int64(id))
	if options.text != nil {
		text = *options.text
	}
	history := &CompactAgentHistory{
		LastAssistantMessage: &CompactText{Text: &text},
	}
	mergedPayload := map[string]any{"agent": "claude"}
	for key, value := range payload {
		mergedPayload[key] = value
	}
	event := AgentEventWireRecord{
		CompactHistory: history,
		ID:             id,
		Payload:        mergedPayload,
		Type:           kind,
	}
	panel := "wB:p2"
	if options.paneID != nil {
		event.PaneID = options.paneID
	} else {
		event.PaneID = &panel
	}
	if options.terminal != nil {
		event.TerminalID = options.terminal
	} else if options.hasTerminal {
		// An explicit nil (the TS `terminalId: null`) means "no terminal".
		event.TerminalID = nil
	} else {
		event.TerminalID = stringPtr("term_agent")
	}
	if options.hasPaneGeneration {
		event.PaneGeneration = options.paneGeneration
	}
	return event
}

// publishedOutcomeKind is the verbatim `outcomeKind` of the published
// @dorokuma/herdsman-pi@0.15.0 (test/fixtures/herdsman-pi-0.15.0-outcome-kind.ts,
// package/src/wake.ts sha256 3d63716723cd05920ef080de481fbcab894e93801d8fb0d1a0a036d7ee4059a3).
// It keeps the backward-compatibility assertions honest: a released client
// does not know `stalled` and still wakes on `failed`.
func publishedOutcomeKind(event AgentEventWireRecord) string {
	if event.TerminalID == nil || *event.TerminalID == "" {
		return ""
	}
	switch event.Type {
	case "agent.done":
		return "completed"
	case "agent.blocked":
		return "blocked"
	case "agent.failed":
		payload := asRecord(event.Payload)
		reason := stringValue(payload["reason"])
		if reason != nil && (*reason == PlanWaitingHistory || *reason == "degraded") {
			if fallback, ok := payload["fallbackOutcome"].(bool); ok && fallback {
				return "failed"
			}
			return ""
		}
		return "failed"
	case "agent.discarded":
		payload := asRecord(event.Payload)
		agent := stringValue(payload["agent"])
		if agent == nil || *agent == "agy" {
			reason := stringValue(payload["reason"])
			if reason != nil && (*reason == PlanWaitingHistory || *reason == "degraded") {
				if fallback, ok := payload["fallbackOutcome"].(bool); ok && fallback {
					return "failed"
				}
				return ""
			}
		}
		return "failed"
	}
	payload := asRecord(event.Payload)
	if event.Type == "agent.idle" {
		if from, ok := payload["from"].(string); ok && from == "working" {
			return "completed"
		}
	}
	return ""
}

// ── projection ──────────────────────────────────────────────────────────────

func TestWakeSelectsDoneAndKeepsRawEventsAscending(t *testing.T) {
	events := []AgentEventWireRecord{
		wakeEvent(1, "agent.status.changed", map[string]any{"from": "idle", "to": "working"}, nil),
		wakeEvent(2, "agent.status.changed", map[string]any{"from": "working", "to": "done"}, nil),
		wakeEvent(3, "agent.done", map[string]any{"from": "working", "to": "done"}, nil),
		wakeEvent(4, "agent.status.changed", map[string]any{"from": "done", "to": "idle"}, nil),
		wakeEvent(5, "agent.idle", map[string]any{"from": "done", "to": "idle"}, nil),
	}
	projection := ProjectAgentOutcomes(events, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(projection.Outcomes))
	}
	if projection.Outcomes[0].EventID != 3 || projection.Outcomes[0].Kind != "completed" ||
		projection.Outcomes[0].TerminalID != "term_agent" {
		t.Fatalf("unexpected outcome: %+v", projection.Outcomes[0])
	}
	if idsOf(projection.RawEvents) != "1,2,3,4,5" {
		t.Fatalf("raw events: %s", idsOf(projection.RawEvents))
	}
	if len(projection.SuppressedEventIDs) != 0 {
		t.Fatalf("unexpected suppressions: %v", projection.SuppressedEventIDs)
	}
}

func idsOf(events []AgentEventWireRecord) string {
	ids := make([]int, len(events))
	for index, event := range events {
		ids[index] = event.ID
	}
	return joinInts(ids)
}

func outcomeIDsOf(outcomes []AgentOutcome) string {
	ids := make([]int, len(outcomes))
	for index, outcome := range outcomes {
		ids[index] = outcome.EventID
	}
	return joinInts(ids)
}

func TestWakeClassifiesBlockedAndWorkingToIdleFallback(t *testing.T) {
	blocked := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(6, "agent.blocked", map[string]any{"to": "blocked"}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(blocked.Outcomes) != 1 || blocked.Outcomes[0].Kind != "blocked" || blocked.Outcomes[0].EventID != 6 {
		t.Fatalf("unexpected blocked outcome: %+v", blocked.Outcomes)
	}
	idle := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(7, "agent.idle", map[string]any{"from": "working", "to": "idle"}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(idle.Outcomes) != 1 || idle.Outcomes[0].Kind != "completed" || idle.Outcomes[0].EventID != 7 {
		t.Fatalf("unexpected idle outcome: %+v", idle.Outcomes)
	}
}

func TestWakeFiltersFailedWithWaitingHistory(t *testing.T) {
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(19, "agent.failed", map[string]any{
			"agent": "claude", "from": "working", "name": "reviewer",
			"paneId": "wB:p2", "reason": PlanWaitingHistory, "to": "done",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 0 {
		t.Fatalf("expected no outcomes, got %+v", projection.Outcomes)
	}
	if len(projection.RawEvents) != 1 {
		t.Fatalf("expected the raw event to survive, got %d", len(projection.RawEvents))
	}
}

func TestWakeProjectsFailedAndFormatsReason(t *testing.T) {
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(20, "agent.failed", map[string]any{
			"agent": "claude", "from": "working", "name": "reviewer",
			"paneId": "wB:p2", "reason": "PROCESS_CRASH", "to": "done",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(projection.Outcomes))
	}
	outcome := projection.Outcomes[0]
	if outcome.Kind != "failed" || textOrEmpty(outcome.Reason) != "PROCESS_CRASH" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	formatted := FormatAgentOutcomeUpdates(projection.Outcomes)
	mustContain(t, formatted, "[HERDSMAN WAKE POLICY]")
	mustContain(t, formatted, "- failed reviewer · Claude wB:p2")
	mustContain(t, formatted, "reason: PROCESS_CRASH")
	mustContain(t, formatted, "last assistant (pre-round): assistant result 20")
}

func TestWakeCarriesFailedAssistantBodyOrOmitsIt(t *testing.T) {
	withBody := AgentOutcome{
		Agent: "claude", EventID: 25, Kind: "failed", Name: stringPtr("worker"),
		PaneID: stringPtr("wB:p2"), Reason: stringPtr("degraded"),
		TerminalID: "term_agent", Text: "Partial output",
	}
	formatted := FormatAgentOutcomeUpdates([]AgentOutcome{withBody})
	mustContain(t, formatted, "- failed worker · Claude wB:p2")
	mustContain(t, formatted, "reason: degraded")
	mustContain(t, formatted, "last assistant (pre-round): Partial output")

	withoutBody := withBody
	withoutBody.Text = ""
	emptyFormatted := FormatAgentOutcomeUpdates([]AgentOutcome{withoutBody})
	mustContain(t, emptyFormatted, "reason: degraded")
	// 无正文时的输出与改前逐字一致：只保留 reason 行。
	mustNotContain(t, emptyFormatted, "last assistant")
}

func TestWakeProjectsDiscardedAsFailedEvenWithWaitingHistory(t *testing.T) {
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(21, "agent.discarded", map[string]any{
			"agent": "claude", "from": "working", "name": "reviewer",
			"paneId": "wB:p2", "reason": PlanWaitingHistory, "to": "done",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(projection.Outcomes))
	}
	outcome := projection.Outcomes[0]
	if outcome.Kind != "failed" || textOrEmpty(outcome.Reason) != PlanWaitingHistory {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if len(projection.RawEvents) != 1 {
		t.Fatalf("expected the raw event, got %d", len(projection.RawEvents))
	}
	formatted := FormatAgentOutcomeUpdates(projection.Outcomes)
	mustContain(t, formatted, "- failed reviewer · Claude wB:p2")
	mustContain(t, formatted, "reason: "+PlanWaitingHistory)
}

func TestWakeProjectsDiscardedWithCustomReason(t *testing.T) {
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(22, "agent.discarded", map[string]any{
			"agent": "claude", "from": "working", "name": "worker",
			"paneId": "wB:p3", "reason": "TIMEOUT_DISCARD", "to": "idle",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 || projection.Outcomes[0].Kind != "failed" ||
		textOrEmpty(projection.Outcomes[0].Reason) != "TIMEOUT_DISCARD" {
		t.Fatalf("unexpected outcomes: %+v", projection.Outcomes)
	}
}

func TestWakeProjectsAgyWaitingHistorySilentlyButWakesOnRealFailure(t *testing.T) {
	intermediate := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(23, "agent.discarded", map[string]any{
			"agent": "agy", "from": "working", "name": "worker",
			"paneId": "wB:p4", "reason": PlanWaitingHistory, "to": "done",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(intermediate.Outcomes) != 0 || len(intermediate.RawEvents) != 1 {
		t.Fatalf("unexpected intermediate projection: %+v", intermediate.Outcomes)
	}
	genuine := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(24, "agent.discarded", map[string]any{
			"agent": "agy", "from": "working", "name": "worker",
			"paneId": "wB:p4", "reason": "COMMAND_EXITED_WITH_ERROR", "to": "failed",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(genuine.Outcomes) != 1 || genuine.Outcomes[0].Kind != "failed" {
		t.Fatalf("unexpected genuine projection: %+v", genuine.Outcomes)
	}
}
