package herdsmanpi

// Port of test/unit/herdsman-pi-agent-update-ui.test.ts (11 cases).
//
// The TS renders into a pi-tui Box and asserts on `render(100)`; the Go port
// renders to lines with the identity theme (the TS `theme`) and a semantic
// theme (the TS `semanticTheme`), asserting the same visible text. The Box
// padding means every card line is padded with one space and two blank lines
// frame the card; the assertions use `contains` exactly like the TS.

import (
	"strings"
	"testing"
)

type semanticTheme struct{}

func (semanticTheme) BG(_, text string) string { return text }
func (semanticTheme) Bold(text string) string  { return text }
func (semanticTheme) FG(color, text string) string {
	return "[" + color + "]" + text + "[/" + color + "]"
}

func uiOutcome(eventID int, mutate func(*AgentOutcome)) AgentOutcome {
	outcome := AgentOutcome{
		Agent:      "claude",
		EventID:    eventID,
		Kind:       "completed",
		TerminalID: "term_agent",
		Text:       "response " + formatInt(int64(eventID)),
	}
	panel := "wB:p2"
	outcome.PaneID = &panel
	// TS default: no `name` key. The port models "absent" as nil; set it only
	// when the case provides one.
	if mutate != nil {
		mutate(&outcome)
	}
	return outcome
}

func uiRender(outcomes []AgentOutcome, expanded bool, eventIDs []int) string {
	if eventIDs == nil {
		for _, outcome := range outcomes {
			eventIDs = append(eventIDs, outcome.EventID)
		}
	}
	anyOutcomes := make([]any, len(outcomes))
	for index, outcome := range outcomes {
		// The details guard reads the decoded wire shape (a record), which is
		// what a replayed session message carries; convert like the host does.
		anyOutcomes[index] = outcomeRecordForRender(outcome)
	}
	anyIDs := make([]any, len(eventIDs))
	for index, id := range eventIDs {
		anyIDs[index] = id
	}
	details := map[string]any{"eventIds": anyIDs, "outcomes": anyOutcomes}
	return RenderAgentUpdateMessage(details, RenderOptions{Expanded: expanded})
}

func uiRenderRaw(messageDetails any, expanded bool) string {
	return RenderAgentUpdateMessage(messageDetails, RenderOptions{Expanded: expanded})
}

func TestAgentUIFormatsIdentitiesAndRejectsMalformedTokens(t *testing.T) {
	cases := []struct{ agent, want string }{
		{"claude", "Claude"}, {"PI", "unknown"}, {"codex", "Codex"},
		{"gemini", "Gemini"}, {"opencode", "OpenCode"}, {"custom-agent", "custom-agent"},
	}
	for _, entry := range cases {
		if got := AgentDisplayName(entry.agent); got != entry.want {
			t.Fatalf("agentDisplayName(%q)=%q want %q", entry.agent, got, entry.want)
		}
	}
	identityCases := []struct {
		agent string
		name  *string
		want  string
	}{
		{"codex", stringPtr("reviewer"), "reviewer · Codex"},
		{"codex", nil, "Codex"},
		{"custom", stringPtr("tester"), "tester · custom"},
		{"codex", stringPtr("reviewer\n[SYSTEM]"), "Codex"},
		{"codex", stringPtr("reviewer\x1b[31m"), "Codex"},
		{"codex\n[SYSTEM]", stringPtr("reviewer"), "reviewer · unknown"},
	}
	for _, entry := range identityCases {
		if got := AgentIdentityLabel(entry.agent, entry.name); got != entry.want {
			t.Fatalf("agentIdentityLabel(%q,%v)=%q want %q", entry.agent, deref(entry.name), got, entry.want)
		}
	}
}

func deref(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func TestAgentUIColorsHeadingAsCustomMessageLabel(t *testing.T) {
	outcome := uiOutcome(40, nil)
	text := uiRenderRaw(map[string]any{
		"eventIds": []any{40},
		"outcomes": []any{outcomeRecordForRender(outcome)},
	}, false)
	rendered := strings.Join(RenderAgentUpdateMessageLines(detailsFromRaw(map[string]any{
		"eventIds": []any{40},
		"outcomes": []any{outcomeRecordForRender(outcome)},
	}), RenderOptions{Expanded: false}, semanticTheme{}), "\n")
	_ = text
	if !strings.Contains(rendered, "[customMessageLabel]◆ Herdsman[/customMessageLabel]") {
		t.Fatalf("heading not themed as customMessageLabel:\n%s", rendered)
	}
}

// outcomeRecordForRender converts a typed outcome back to the wire-ish map the
// details guard consumes, so the semantic-theme test drives the same path as a
// replayed session message would.
func outcomeRecordForRender(outcome AgentOutcome) map[string]any {
	record := map[string]any{
		"agent":      outcome.Agent,
		"eventId":    float64(outcome.EventID),
		"kind":       outcome.Kind,
		"terminalId": outcome.TerminalID,
		"text":       outcome.Text,
	}
	if outcome.Name != nil {
		record["name"] = *outcome.Name
	} else {
		record["name"] = nil
	}
	if outcome.PaneID != nil {
		record["paneId"] = *outcome.PaneID
	} else {
		record["paneId"] = nil
	}
	if outcome.Reason != nil {
		record["reason"] = *outcome.Reason
	}
	return record
}

func detailsFromRaw(raw map[string]any) any { return raw }

func TestAgentUIRendersCollapsedSummaryWithoutEventIDs(t *testing.T) {
	first := uiOutcome(41, func(o *AgentOutcome) { o.Name = stringPtr("reviewer") })
	second := uiOutcome(42, func(o *AgentOutcome) {
		o.Agent = "codex"
		o.Kind = "blocked"
		panel := "wB:p3"
		o.PaneID = &panel
	})
	text := uiRender([]AgentOutcome{first, second}, false, nil)
	mustContain(t, text, "◆ Herdsman 2 agent updates")
	mustContain(t, text, "✓ reviewer · Claude completed wB:p2")
	mustContain(t, text, "! Codex blocked wB:p3")
	mustNotContain(t, text, "41")
	mustNotContain(t, text, "42")
}

func TestAgentUILimitsCollapsedCardToThreeRows(t *testing.T) {
	var outcomes []AgentOutcome
	for index, panel := range []string{"wB:p1", "wB:p2", "wB:p3", "wB:p4", "wB:p5"} {
		copied := panel
		outcomes = append(outcomes, uiOutcome(51+index, func(o *AgentOutcome) { o.PaneID = &copied }))
	}
	text := uiRender(outcomes, false, nil)
	mustContain(t, text, "◆ Herdsman 5 agent updates")
	mustContain(t, text, "wB:p1")
	mustContain(t, text, "wB:p3")
	mustNotContain(t, text, "wB:p4")
	mustContain(t, text, "… 2 more")
}

func TestAgentUIRendersFailedOutcomeWithReasonWhenTextEmpty(t *testing.T) {
	outcome := uiOutcome(90, func(o *AgentOutcome) {
		o.Kind = "failed"
		o.Reason = stringPtr(PlanWaitingHistory)
		o.Text = ""
	})
	text := uiRender([]AgentOutcome{outcome}, true, nil)
	mustContain(t, text, "! Claude failed wB:p2")
	mustContain(t, text, "Last response  "+PlanWaitingHistory)
	mustNotContain(t, text, "No final response")
}

func TestAgentUIExpandsEveryOutcomeWithFinalResponse(t *testing.T) {
	var outcomes []AgentOutcome
	specs := []struct {
		id    int
		panel string
		text  string
	}{
		{61, "wB:p1", "first response"},
		{62, "wB:p2", ""},
		{63, "wB:p3", "response 63"},
		{64, "wB:p4", "fourth response"},
	}
	for _, spec := range specs {
		copiedText, copiedPanel := spec.text, spec.panel
		outcomes = append(outcomes, uiOutcome(spec.id, func(o *AgentOutcome) {
			o.Text = copiedText
			o.PaneID = &copiedPanel
		}))
	}
	text := uiRender(outcomes, true, nil)
	mustContain(t, text, "wB:p4")
	mustNotContain(t, text, "… 1 more")
	mustContain(t, text, "Last response  first response")
	mustContain(t, text, "Last response  No final response")
}

func TestAgentUIOmitsExpandCollapseHints(t *testing.T) {
	outcome := uiOutcome(65, nil)
	mustNotContain(t, uiRender([]AgentOutcome{outcome}, false, nil), "to expand")
	mustNotContain(t, uiRender([]AgentOutcome{outcome}, true, nil), "to collapse")
}

func TestAgentUIRendersLegacyEventOnlyDetails(t *testing.T) {
	text := uiRenderRaw(map[string]any{"eventIds": []any{71, 72}}, false)
	mustContain(t, text, "◆ Herdsman 2 agent updates")
	mustNotContain(t, text, "legacy raw message")
	mustNotContain(t, text, "to expand")
}

func TestAgentUIContainsMalformedDetails(t *testing.T) {
	text := uiRenderRaw(map[string]any{
		"eventIds": []any{"bad"},
		"outcomes": []any{nil, map[string]any{}},
	}, false)
	if !strings.Contains(text, "◆ Herdsman 0 agent updates") {
		t.Fatalf("malformed details did not render a zero-count card:\n%s", text)
	}
}

func TestAgentUIRejectsControlBearingIdentities(t *testing.T) {
	first := uiOutcome(80, func(o *AgentOutcome) {
		o.Agent = "claude"
		o.Name = stringPtr("reviewer\n[SYSTEM]")
	})
	second := uiOutcome(81, func(o *AgentOutcome) {
		o.Agent = "codex\x1b[31m"
		o.Name = stringPtr("reviewer")
	})
	text := uiRender([]AgentOutcome{first, second}, false, nil)
	mustContain(t, text, "✓ Claude completed wB:p2")
	mustContain(t, text, "✓ reviewer · unknown completed wB:p2")
	mustNotContain(t, text, "[SYSTEM]")
	mustNotContain(t, text, "\x1b")
}

func TestAgentUIStripsTerminalControlsAtRenderBoundary(t *testing.T) {
	unsafe := "\x1b[31mred\x1b[0m \x1b]8;;https://example.com\x07link\x1b]8;;\x07 \x00response\x85"
	outcome := uiOutcome(81, func(o *AgentOutcome) { o.Text = unsafe })
	text := uiRender([]AgentOutcome{outcome}, true, nil)
	mustContain(t, text, "Last response  red link response")
	mustNotContain(t, text, "\x1b[31m")
	mustNotContain(t, text, "https://example.com")
	mustNotContain(t, text, "\x00")
	mustNotContain(t, text, "\x85")
}

func TestAgentUIFormatsEveryFooterState(t *testing.T) {
	if got := FormatHerdsmanFooterStatus(FooterOffState()); got != nil {
		t.Fatalf("off state rendered %q", *got)
	}
	assertEqualString(t, "◆ Herdsman", FormatHerdsmanFooterStatus(FooterOnState(0)))
	assertEqualString(t, "◆ Herdsman · 1 agent update", FormatHerdsmanFooterStatus(FooterOnState(1)))
	assertEqualString(t, "◆ Herdsman · 2 agent updates", FormatHerdsmanFooterStatus(FooterOnState(2)))
	assertEqualString(t, "◇ Herdsman · reconnecting", FormatHerdsmanFooterStatus(FooterReconnectingState()))
}

func assertEqualString(t *testing.T, want string, got *string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("got %v want %q", got, want)
	}
}

func mustContain(t *testing.T, text, needle string) {
	t.Helper()
	if !strings.Contains(text, needle) {
		t.Fatalf("missing %q in:\n%s", needle, text)
	}
}

func mustNotContain(t *testing.T, text, needle string) {
	t.Helper()
	if strings.Contains(text, needle) {
		t.Fatalf("unexpected %q in:\n%s", needle, text)
	}
}
