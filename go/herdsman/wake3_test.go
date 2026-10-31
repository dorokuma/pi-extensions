package herdsman

// Third part of the wake-suite port: suppression, dead-letter accounting and
// the pane-generation completion evidence.

import (
	"strings"
	"testing"
)

func TestWakeSuppressesUpstreamErrorInsteadOfProjecting(t *testing.T) {
	text := "API Error: 429 rate_limit_error"
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(43, "agent.done", map[string]any{}, &eventOptions{text: &text}),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 0 {
		t.Fatalf("unexpected outcomes: %+v", projection.Outcomes)
	}
	if len(projection.SuppressedEventIDs) != 1 || projection.SuppressedEventIDs[0] != 43 {
		t.Fatalf("unexpected suppressions: %v", projection.SuppressedEventIDs)
	}
	if idsOf(projection.RawEvents) != "43" {
		t.Fatalf("raw events: %s", idsOf(projection.RawEvents))
	}
}

func TestWakeSuppressesFailedOutcomeWithUpstreamReason(t *testing.T) {
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(44, "agent.failed", map[string]any{
			"from": "working", "name": "reviewer", "reason": "request timed out", "to": "done",
		}, nil),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 0 || len(projection.SuppressedEventIDs) != 1 || projection.SuppressedEventIDs[0] != 44 {
		t.Fatalf("unexpected projection: %+v", projection)
	}
}

func TestWakeKeepsNormalResult(t *testing.T) {
	text := "implemented the retry queue"
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(45, "agent.done", map[string]any{}, &eventOptions{text: &text}),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 || len(projection.SuppressedEventIDs) != 0 {
		t.Fatalf("unexpected projection: %+v", projection)
	}
}

func TestWakeSuppressesEmptyCompletedRound(t *testing.T) {
	empty := ""
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(421, "agent.done", map[string]any{}, &eventOptions{text: &empty}),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 0 || len(projection.SuppressedEventIDs) != 1 || projection.SuppressedEventIDs[0] != 421 {
		t.Fatalf("unexpected projection: %+v", projection)
	}
	if idsOf(projection.RawEvents) != "421" {
		t.Fatalf("raw events: %s", idsOf(projection.RawEvents))
	}

	blank := "   "
	boomReason := "boom"
	mixed := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(422, "agent.done", map[string]any{}, &eventOptions{text: &blank}),
		wakeEvent(423, "agent.failed", map[string]any{"from": "working", "reason": boomReason, "to": "done"},
			&eventOptions{text: &empty}),
		wakeEvent(424, "agent.done", map[string]any{}, &eventOptions{text: stringPtr("kept")}),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(mixed.SuppressedEventIDs) != 1 || mixed.SuppressedEventIDs[0] != 422 {
		t.Fatalf("unexpected suppressions: %v", mixed.SuppressedEventIDs)
	}
	if outcomeIDsOf(mixed.Outcomes) != "423,424" {
		t.Fatalf("unexpected outcomes: %s", outcomeIDsOf(mixed.Outcomes))
	}
	if mixed.Outcomes[0].Kind != "failed" || mixed.Outcomes[1].Kind != "completed" {
		t.Fatalf("unexpected kinds: %+v", mixed.Outcomes)
	}
}

func TestWakeKeepsLongReportMentioning429(t *testing.T) {
	report := strings.Repeat("Investigated the flaky run and reproduced a 429 once. ", 20) + "End of report."
	if len(report) <= 400 {
		t.Fatalf("report too short: %d", len(report))
	}
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(46, "agent.done", map[string]any{}, &eventOptions{text: &report}),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 || len(projection.SuppressedEventIDs) != 0 {
		t.Fatalf("unexpected projection: %+v", projection)
	}
}

func TestWakeKeeps429OutcomeWhenFilterDisabled(t *testing.T) {
	text := "API Error: 429 rate_limit_error"
	disabled := WakeFilterConfig{Enabled: false, ExtraPatterns: []string{}}
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(47, "agent.done", map[string]any{}, &eventOptions{text: &text}),
	}, disabled, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 || len(projection.SuppressedEventIDs) != 0 {
		t.Fatalf("unexpected projection: %+v", projection)
	}
}

func TestWakeSuppressesHarmlessCustomPatternMatch(t *testing.T) {
	text := "waiting for checkpoint"
	custom := WakeFilterConfig{Enabled: true, ExtraPatterns: []string{"checkpoint"}}
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(48, "agent.done", map[string]any{}, &eventOptions{text: &text}),
	}, custom, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 0 || len(projection.SuppressedEventIDs) != 1 || projection.SuppressedEventIDs[0] != 48 {
		t.Fatalf("unexpected projection: %+v", projection)
	}
}

func TestWakeDoesNotConsumeSuppressedIDsIntoSeen(t *testing.T) {
	projector := NewAgentOutcomeProjector(DefaultWakeFilterConfig)
	suppressedText := "API Error: 429 rate_limit_error"
	suppressedEvent := wakeEvent(49, "agent.done", map[string]any{}, &eventOptions{text: &suppressedText})
	normalEvent := wakeEvent(50, "agent.done", map[string]any{}, nil)

	first := projector.Project([]AgentEventWireRecord{suppressedEvent})
	if len(first.Outcomes) != 0 || idsOf(first.RawEvents) != "49" ||
		len(first.SuppressedEventIDs) != 1 || first.SuppressedEventIDs[0] != 49 {
		t.Fatalf("unexpected first projection: %+v", first)
	}
	second := projector.Project([]AgentEventWireRecord{suppressedEvent, normalEvent})
	if outcomeIDsOf(second.Outcomes) != "50" || idsOf(second.RawEvents) != "49,50" ||
		len(second.SuppressedEventIDs) != 1 || second.SuppressedEventIDs[0] != 49 {
		t.Fatalf("unexpected second projection: %+v", second)
	}
}

func TestWakeSuppressesFallbackForAlreadyCompletedPane(t *testing.T) {
	presentedCompletion := wakeEvent(60, "agent.done", map[string]any{"from": "working", "to": "done"},
		&eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1"), hasTerminal: true, terminal: nil})
	fallbackFailure := wakeEvent(61, "agent.failed", map[string]any{
		"fallbackOutcome": true, "from": "working", "name": "worker",
		"reason": "degraded", "to": "failed",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")})

	projection := ProjectAgentOutcomes([]AgentEventWireRecord{presentedCompletion, fallbackFailure},
		DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 0 {
		t.Fatalf("fallback not suppressed: %+v", projection.Outcomes)
	}
	if idsOf(projection.RawEvents) != "60,61" {
		t.Fatalf("raw events: %s", idsOf(projection.RawEvents))
	}

	// Control 1: agent.failed + fallbackOutcome stays failed (never stalled).
	alone := ProjectAgentOutcomes([]AgentEventWireRecord{fallbackFailure}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{})
	if len(alone.Outcomes) != 1 || alone.Outcomes[0].Kind != "failed" || textOrEmpty(alone.Outcomes[0].Reason) != "degraded" {
		t.Fatalf("unexpected control-1 outcome: %+v", alone.Outcomes)
	}

	// Control 1b: only agent.discarded + fallbackOutcome projects as stalled.
	fallbackDiscarded := wakeEvent(65, "agent.discarded", map[string]any{
		"agent": "agy", "fallbackOutcome": true, "from": "working",
		"name": "worker", "reason": "degraded", "to": "discarded",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")})
	if ids := ProjectAgentOutcomes([]AgentEventWireRecord{fallbackDiscarded}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{}); len(ids.Outcomes) != 1 || ids.Outcomes[0].Kind != "stalled" {
		t.Fatalf("unexpected control-1b outcome: %+v", ids.Outcomes)
	}

	// Control 1c: a late completion still wakes after a stalled one.
	lateText := "Finished the late task"
	subsequentCompletion := wakeEvent(66, "agent.done", map[string]any{"from": "working", "to": "done"},
		&eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1"), text: &lateText})
	wakeProjector := NewAgentOutcomeProjector(DefaultWakeFilterConfig)
	first := wakeProjector.Project([]AgentEventWireRecord{fallbackDiscarded})
	if len(first.Outcomes) != 1 || first.Outcomes[0].Kind != "stalled" || first.Outcomes[0].EventID != 65 {
		t.Fatalf("unexpected first stalled projection: %+v", first.Outcomes)
	}
	second := wakeProjector.Project([]AgentEventWireRecord{subsequentCompletion})
	if len(second.Outcomes) != 1 || second.Outcomes[0].Kind != "completed" || second.Outcomes[0].Text != "Finished the late task" {
		t.Fatalf("unexpected late completion: %+v", second.Outcomes)
	}
	third := wakeProjector.Project([]AgentEventWireRecord{subsequentCompletion})
	if len(third.Outcomes) != 0 {
		t.Fatalf("duplicate completion re-presented: %+v", third.Outcomes)
	}

	// Backward compatibility: the *published* 0.15.0 outcomeKind sees `failed`
	// where this client says `stalled`, so an old orchestrator still wakes.
	if got := publishedOutcomeKind(fallbackFailure); got != "failed" {
		t.Fatalf("published outcomeKind(fallbackFailure)=%s", got)
	}
	if got := publishedOutcomeKind(fallbackDiscarded); got != "failed" {
		t.Fatalf("published outcomeKind(fallbackDiscarded)=%s", got)
	}

	// Control 2: a completion on another pane must not suppress the fallback.
	otherPane := "wB:p9"
	crossPane := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(62, "agent.done", map[string]any{"from": "working", "to": "done"}, &eventOptions{paneID: &otherPane}),
		fallbackFailure,
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if outcomeIDsOf(crossPane.Outcomes) != "61,62" {
		t.Fatalf("unexpected control-2 outcomes: %s", outcomeIDsOf(crossPane.Outcomes))
	}

	// Control 3 (production shape): the completion itself wakes, fallback stays
	// suppressed, so there is no double wake.
	production := ProjectAgentOutcomes([]AgentEventWireRecord{
		wakeEvent(63, "agent.done", map[string]any{"from": "working", "to": "done"},
			&eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")}),
		wakeEvent(64, "agent.failed", map[string]any{
			"fallbackOutcome": true, "from": "working", "reason": "degraded", "to": "failed",
		}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")}),
	}, DefaultWakeFilterConfig, map[CompletedPaneKey]int{})
	if outcomeIDsOf(production.Outcomes) != "63" {
		t.Fatalf("unexpected control-3 outcomes: %s", outcomeIDsOf(production.Outcomes))
	}
}

func TestWakeProjectsStallSettledAgyDiscardAsStalled(t *testing.T) {
	agyStallDiscard := wakeEvent(90, "agent.discarded", map[string]any{
		"agent": "agy", "fallbackOutcome": true, "from": "working",
		"name": "worker", "reason": PlanStalled, "to": "discarded",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")})
	stalled := ProjectAgentOutcomes([]AgentEventWireRecord{agyStallDiscard}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{})
	if len(stalled.Outcomes) != 1 || stalled.Outcomes[0].Kind != "stalled" ||
		textOrEmpty(stalled.Outcomes[0].Reason) != PlanStalled {
		t.Fatalf("unexpected stall outcome: %+v", stalled.Outcomes)
	}
	mustContain(t, FormatAgentOutcomeUpdates(stalled.Outcomes), "- stalled ")

	// An agentless (orphan/stitched) row behaves the same.
	agentless := wakeEvent(91, "agent.discarded", map[string]any{
		"fallbackOutcome": true, "from": "working", "reason": PlanStalled, "to": "discarded",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")})
	agentless.Payload = map[string]any{
		"fallbackOutcome": true, "from": "working", "reason": PlanStalled, "to": "discarded",
	}
	if ids := ProjectAgentOutcomes([]AgentEventWireRecord{agentless}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{}); len(ids.Outcomes) != 1 || ids.Outcomes[0].Kind != "stalled" {
		t.Fatalf("unexpected agentless outcome: %+v", ids.Outcomes)
	}

	// A pi discard keeps its old semantics: `agent === "pi"` is not in the
	// agy-or-empty branch, so it stays failed.
	piStallDiscard := wakeEvent(92, "agent.discarded", map[string]any{
		"agent": "pi", "fallbackOutcome": true, "from": "working",
		"reason": PlanStalled, "to": "discarded",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")})
	if ids := ProjectAgentOutcomes([]AgentEventWireRecord{piStallDiscard}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{}); len(ids.Outcomes) != 1 || ids.Outcomes[0].Kind != "failed" {
		t.Fatalf("unexpected pi outcome: %+v", ids.Outcomes)
	}

	// The published 0.15.0 client does not know `stalled` and still wakes.
	if got := publishedOutcomeKind(agyStallDiscard); got != "failed" {
		t.Fatalf("published outcomeKind(agyStallDiscard)=%s", got)
	}
	if got := publishedOutcomeKind(piStallDiscard); got != "failed" {
		t.Fatalf("published outcomeKind(piStallDiscard)=%s", got)
	}

	// A late completion after the stall still wakes.
	projector := NewAgentOutcomeProjector(DefaultWakeFilterConfig)
	if ids := projector.Project([]AgentEventWireRecord{agyStallDiscard}); len(ids.Outcomes) != 1 || ids.Outcomes[0].Kind != "stalled" {
		t.Fatalf("unexpected stall projection: %+v", ids.Outcomes)
	}
	lateText := "late jittered turn result"
	lateCompletion := wakeEvent(93, "agent.idle", map[string]any{"from": "working", "to": "idle"},
		&eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1"), text: &lateText})
	if ids := projector.Project([]AgentEventWireRecord{lateCompletion}); len(ids.Outcomes) != 1 || ids.Outcomes[0].Kind != "completed" {
		t.Fatalf("unexpected late completion: %+v", ids.Outcomes)
	}
}

func TestWakeGenerationlessCompletionSuppressesNothing(t *testing.T) {
	projector := NewAgentOutcomeProjector(DefaultWakeFilterConfig)
	generationless := wakeEvent(80, "agent.done", map[string]any{"from": "working", "to": "done"},
		&eventOptions{hasPaneGeneration: true, paneGeneration: nil, hasTerminal: true, terminal: nil})
	samePaneFallback := wakeEvent(81, "agent.failed", map[string]any{
		"fallbackOutcome": true, "from": "working", "reason": "degraded", "to": "failed",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-2")})
	omittedGenerationFallback := wakeEvent(82, "agent.failed", map[string]any{
		"fallbackOutcome": true, "from": "working", "reason": "degraded", "to": "failed",
	}, &eventOptions{paneID: stringPtr("wB:p3")})

	projection := projector.Project([]AgentEventWireRecord{
		generationless, samePaneFallback, omittedGenerationFallback,
	})
	if outcomeIDsOf(projection.Outcomes) != "81,82" {
		t.Fatalf("unexpected outcomes: %s", outcomeIDsOf(projection.Outcomes))
	}
}

func TestWakeSuppressesFallbackOnlyForMatchingGeneration(t *testing.T) {
	projector := NewAgentOutcomeProjector(DefaultWakeFilterConfig)
	first := projector.Project([]AgentEventWireRecord{
		wakeEvent(83, "agent.done", map[string]any{"from": "working", "to": "done"},
			&eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1"), hasTerminal: true, terminal: nil}),
	})
	if len(first.Outcomes) != 0 {
		t.Fatalf("terminal-less completion projected: %+v", first.Outcomes)
	}
	sameGeneration := wakeEvent(84, "agent.failed", map[string]any{
		"fallbackOutcome": true, "from": "working", "reason": "degraded", "to": "failed",
	}, &eventOptions{hasPaneGeneration: true, paneGeneration: stringPtr("gen-1")})
	nextGeneration := wakeEvent(85, "agent.failed", map[string]any{
		"fallbackOutcome": true, "from": "working", "reason": "degraded", "to": "failed",
	}, &eventOptions{paneID: stringPtr("wB:p3"), hasPaneGeneration: true, paneGeneration: stringPtr("gen-2")})
	second := projector.Project([]AgentEventWireRecord{sameGeneration, nextGeneration})
	if outcomeIDsOf(second.Outcomes) != "85" {
		t.Fatalf("unexpected outcomes: %s", outcomeIDsOf(second.Outcomes))
	}
}

func TestWakeDeadLetterKeepsContentAndDedups(t *testing.T) {
	stranded := wakeEvent(90, "agent.done", map[string]any{"from": "working", "to": "done"}, nil)
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{stranded}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 || projection.Outcomes[0].Kind != "completed" || projection.Outcomes[0].EventID != 90 {
		t.Fatalf("unexpected projection: %+v", projection.Outcomes)
	}
	formatted := FormatAgentOutcomeUpdates(projection.Outcomes)
	mustContain(t, formatted, "event: 90")
	mustContain(t, formatted, "last assistant: assistant result 90")

	projector := NewAgentOutcomeProjector(DefaultWakeFilterConfig)
	if outcomeIDsOf(projector.Project([]AgentEventWireRecord{stranded}).Outcomes) != "90" {
		t.Fatal("the deduplicating projector lost the event")
	}
}

func TestWakeWithoutConsumptionEvidenceIsNotPresented(t *testing.T) {
	pending := wakeEvent(91, "agent.done", map[string]any{"from": "working", "to": "done"}, nil)
	projection := ProjectAgentOutcomes([]AgentEventWireRecord{pending}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{})
	if len(projection.Outcomes) != 1 || projection.Outcomes[0].Kind != "completed" || projection.Outcomes[0].EventID != 91 {
		t.Fatalf("unexpected projection: %+v", projection.Outcomes)
	}
	if idsOf(projection.RawEvents) != "91" || len(projection.SuppressedEventIDs) != 0 {
		t.Fatalf("unexpected evidence: raw=%s suppressed=%v", idsOf(projection.RawEvents), projection.SuppressedEventIDs)
	}
	// A second projection in the same session still projects it.
	if len(ProjectAgentOutcomes([]AgentEventWireRecord{pending}, DefaultWakeFilterConfig,
		map[CompletedPaneKey]int{}).Outcomes) != 1 {
		t.Fatal("an unconfirmed id was treated as seen")
	}
}
