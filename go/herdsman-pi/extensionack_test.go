package herdsmanpi

// Port of test/unit/herdsman-pi-extension.test.ts — part 2: acknowledgement
// ordering and gating, footer refresh, role loss, non-owner behaviour and the
// `/herdsman` command.

import (
	"testing"
	"time"
)

// wakeClient configures the fake client the way the TS `createWakeClient` does:
// register/get answer with a connection state, acks are accepted.
func configureWakeClient(f *fixture) {
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get", "agent.orchestrator.set":
			return connectionResponse(f.client, &connectionOptions{})
		case "agent.list":
			return map[string]any{"agents": []any{}}
		default:
			return map[string]any{"acknowledged": true}
		}
	}
}

func TestAcknowledgesOwnerUpdatesInIDOrderAfterFinalResponse(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
	first := wakeEvent(42, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-a"), hasTerminal: true, terminal: stringPtr("term_agent")})
	second := wakeEvent(41, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-b"), hasTerminal: true, terminal: stringPtr("term_agent")})
	registerEvents := []AgentEventWireRecord{first, second}
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register":
			return connectionResponse(f.client, &connectionOptions{Events: registerEvents, HasEvents: true})
		case "agent.orchestrator.get":
			return connectionResponse(f.client, &connectionOptions{Events: registerEvents, HasEvents: true})
		case "agent.list":
			return map[string]any{"agents": []any{}}
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	// One further event arrives over the stream (43). The daemon's pending page
	// is re-read while the wake is prepared, so 43 is in the batch even though
	// it was not part of the initial register response — exactly the TS case.
	streamed := wakeEvent(43, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-c"), terminal: stringPtr("term_agent")})
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register":
			return connectionResponse(f.client, &connectionOptions{Events: registerEvents, HasEvents: true})
		case "agent.orchestrator.get":
			return connectionResponse(f.client, &connectionOptions{
				Events:    []AgentEventWireRecord{first, second, streamed},
				HasEvents: true,
			})
		case "agent.list":
			return map[string]any{"agents": []any{}}
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.start()

	// Three more events arrive over the stream: one from another terminal (45
	// carries no terminal at all and is dropped), then 43, then one from our own
	// terminal (44), which is ignored because a terminal never acknowledges its
	// own updates.
	f.client.EmitStream(DaemonStreamMessage{Method: "agent.event", Event: ptrEvent(streamed)})
	f.client.EmitStream(DaemonStreamMessage{Method: "agent.event", Event: ptrEvent(wakeEvent(44, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-d"), terminal: stringPtr("term_pi")}))})
	f.client.EmitStream(DaemonStreamMessage{Method: "agent.event", Event: ptrEvent(wakeEvent(45, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-e"), hasTerminal: true, terminal: nil}))})

	status := f.ctx.Status("herdsman")
	if status == nil || *status != "◆ Herdsman · 3 agent updates" {
		t.Fatalf("unexpected status: %v", deref(status))
	}
	if got := FormatHiddenAgentUpdates([]AgentEventWireRecord{wakeEvent(1, "agent.done", map[string]any{}, nil)}); !contains(got, "[HERDSMAN AGENT UPDATES]") {
		t.Fatalf("hidden updates lost the heading: %q", got)
	}

	f.timers.Advance(500 * time.Millisecond)
	f.core.HandleAgentStart()
	// The injected wake content reaching the transcript is what authorises the
	// acknowledgement; the turn outcome alone never does.
	f.core.HandleMessageEnd(wakeConsumptionEvidence([]int{41, 42, 43}))
	if kept := f.core.HandleContext([]any{}); len(kept) != 0 {
		t.Fatalf("context kept %d messages", len(kept))
	}
	if acks := f.client.AckedIDs(); len(acks) != 0 {
		t.Fatalf("acked before the turn settled: %v", acks)
	}

	f.core.HandleMessageEnd(assistantEnd("stop", []any{
		map[string]any{"type": "thinking", "text": "hidden reasoning"},
	}))
	if acks := f.client.AckedIDs(); len(acks) != 0 {
		t.Fatalf("acked on message_end: %v", acks)
	}
	f.core.HandleAgentSettled(f.ctx)

	if got := joinInts(f.client.AckedIDs()); got != "41,42,43" {
		t.Fatalf("ack order: %s", got)
	}
	status = f.ctx.Status("herdsman")
	if status == nil || *status != "◆ Herdsman" {
		t.Fatalf("footer not refreshed: %v", deref(status))
	}
}

// ptrEvent returns a pointer to a copy (the stream decoder hands pointers).
func ptrEvent(event AgentEventWireRecord) *AgentEventWireRecord { return &event }

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}

func TestRetainsOnlyUnacknowledgedEventsAfterPartialFailure(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
	events := []AgentEventWireRecord{
		wakeEvent(51, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}),
		wakeEvent(52, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}),
	}
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get":
			return connectionResponse(f.client, &connectionOptions{Events: events, HasEvents: true})
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.client.failure = func(method string, params any) error {
		if method == "agent.notifications.ack" {
			if record, ok := params.(map[string]any); ok {
				if jsNumberOrZero(record["eventId"]) == 52 {
					return &codedError{code: "ORCHESTRATOR_BUSY", message: "busy"}
				}
			}
		}
		return nil
	}
	f.start()
	f.timers.Advance(500 * time.Millisecond)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(wakeConsumptionEvidence([]int{51, 52}))
	f.core.HandleAgentSettled(f.ctx)

	// The first event was accepted; the second one stays pending with a
	// backoff after the transient failure.
	pending := f.core.pendingSnapshot()
	if len(pending) == 0 {
		t.Fatal("the failed event was dropped from the pending projection")
	}
	found := false
	for _, event := range pending {
		if event.ID == 52 {
			found = true
			if event.Attempts == nil || *event.Attempts != 1 {
				t.Fatalf("attempt counter: %+v", event.Attempts)
			}
		}
	}
	if !found {
		t.Fatalf("event 52 left the pending projection: %+v", pending)
	}
	last := f.ctx.LastNotification()
	if last == nil || last.level != NotifyWarning {
		t.Fatalf("expected a warning notification, got %+v", last)
	}
}

func jsNumberOrZero(value any) int {
	number, _ := jsNumberAsInt(value)
	return number
}

func TestRefreshesFooterAfterEachSuccessfulAck(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
	events := []AgentEventWireRecord{
		wakeEvent(55, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}),
		wakeEvent(56, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}),
	}
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get":
			return connectionResponse(f.client, &connectionOptions{Events: events, HasEvents: true})
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.start()
	f.timers.Advance(500 * time.Millisecond)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(wakeConsumptionEvidence([]int{55, 56}))
	f.core.HandleAgentSettled(f.ctx)
	if got := joinInts(f.client.AckedIDs()); got != "55,56" {
		t.Fatalf("ack order: %s", got)
	}
	status := f.ctx.Status("herdsman")
	if status == nil || *status != "◆ Herdsman" {
		t.Fatalf("footer not refreshed: %v", deref(status))
	}
}

func TestRetainsAllEventsAfterFullAckFailure(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
	events := []AgentEventWireRecord{
		wakeEvent(57, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}),
	}
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get":
			return connectionResponse(f.client, &connectionOptions{Events: events, HasEvents: true})
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.client.failure = func(method string, params any) error {
		if method == "agent.notifications.ack" {
			return &codedError{code: "ORCHESTRATOR_CONNECTION_LOST", message: "lost"}
		}
		return nil
	}
	f.start()
	f.timers.Advance(500 * time.Millisecond)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(wakeConsumptionEvidence([]int{57}))
	f.core.HandleAgentSettled(f.ctx)

	if pending := f.core.pendingSnapshot(); len(pending) != 1 || pending[0].ID != 57 {
		t.Fatalf("event was not retained: %+v", pending)
	}
	last := f.ctx.LastNotification()
	if last == nil || last.level != NotifyWarning {
		t.Fatalf("expected a warning notification, got %+v", last)
	}
}

// TestInvalidatesDeliveredBatchOnRoleLossWithoutAbortingNormalTurn pins both
// sides of the TS `loseRole` abort rule over a real, in-flight wake batch: a
// herdsman-triggered batch that produced no substantive work is interrupted,
// a batch whose turn already did work is left alone. The assertion only means
// anything once a batch is actually in flight -- with no batch the rule is
// skipped and both directions pass empty.
func TestInvalidatesDeliveredBatchOnRoleLossWithoutAbortingNormalTurn(t *testing.T) {
	for _, entry := range []struct {
		name        string
		substantive bool
		wantAborts  int
	}{
		{name: "an idle wake is aborted", substantive: false, wantAborts: 1},
		{name: "a substantive turn keeps running", substantive: true, wantAborts: 0},
	} {
		t.Run(entry.name, func(t *testing.T) {
			f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
			event := wakeEvent(61, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")})
			f.client.response = func(method string, params any) any {
				switch method {
				case "agent.orchestrator.register", "agent.orchestrator.get":
					return connectionResponse(f.client, &connectionOptions{
						Events:    []AgentEventWireRecord{event},
						HasEvents: true,
					})
				default:
					return map[string]any{"acknowledged": true}
				}
			}
			f.start()
			// The wake injection is what makes the batch real.
			f.timers.Advance(500 * time.Millisecond)
			f.core.mu.Lock()
			batch := f.core.deliveredBatch
			f.core.mu.Unlock()
			if batch == nil {
				t.Fatal("no in-flight batch was delivered, so the abort rule was not exercised")
			}
			if !batch.HerdsmanTriggered {
				t.Fatal("the batch was not herdsman-triggered, so loseRole must not abort it")
			}
			f.core.HandleAgentStart()
			if entry.substantive {
				// Substantive work happened, so a role loss must not abort the turn.
				f.core.HandleSubstantiveToolUse()
			}
			// The daemon pushes the role to another pane.
			f.client.EmitStream(roleChangeStream(stringPtr("term_pi"), stringPtr("term_other"), "wB:p1"))
			if f.ctx.Aborts != entry.wantAborts {
				t.Fatalf("aborts=%d want %d", f.ctx.Aborts, entry.wantAborts)
			}
			f.core.mu.Lock()
			stillInFlight := f.core.deliveredBatch
			f.core.mu.Unlock()
			if stillInFlight != nil {
				t.Fatal("the invalidated batch still gates the next wake")
			}
			if status := f.ctx.Status("herdsman"); status != nil {
				t.Fatalf("footer still shown: %q", *status)
			}
		})
	}
}

func TestKeepsContextAndUpdatesDisabledForNonOwner(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
	other := "term_other"
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get":
			return connectionResponse(f.client, &connectionOptions{
				OwnerTerminal: &other, HasOwner: true,
			})
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.start()
	context := contextSnapshot("ready")
	f.client.EmitStream(DaemonStreamMessage{
		Method:       "agent.context.changed",
		HerdrSession: "default",
		WorkspaceID:  "wB",
		Context:      context,
	})
	f.client.EmitStream(DaemonStreamMessage{Method: "agent.event", Event: ptrEvent(
		wakeEvent(58, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}))})
	if f.core.latestContext != nil {
		t.Fatalf("non-owner adopted a context: %+v", f.core.latestContext)
	}
	if len(f.core.pendingSnapshot()) != 0 {
		t.Fatalf("non-owner queued updates: %+v", f.core.pendingSnapshot())
	}
	if f.core.isOrchestrator {
		t.Fatal("non-owner claims orchestrator role")
	}
}

func TestImplementsLocalCommandParsingAndStatusMessages(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{})
	configureWakeClient(f)
	f.start()

	f.core.handleCommand("bogus", f.ctx)
	if last := f.ctx.LastNotification(); last == nil || last.message != CommandUsage || last.level != NotifyWarning {
		t.Fatalf("unexpected usage notification: %+v", last)
	}

	f.core.handleCommand("status", f.ctx)
	last := f.ctx.LastNotification()
	if last == nil || last.message != "Herdsman is watching agent updates · default/wB · wB:p1" || last.level != NotifyInfo {
		t.Fatalf("unexpected status notification: %+v", last)
	}
	if calls := f.client.CallsFor("agent.orchestrator.get"); len(calls) == 0 {
		t.Fatal("status did not query the daemon")
	}

	f.core.handleCommand("on", f.ctx)
	last = f.ctx.LastNotification()
	if last == nil || last.message != "Herdsman is watching agent updates · default/wB · wB:p1" {
		t.Fatalf("unexpected on notification: %+v", last)
	}
	onCalls := f.client.CallsFor("agent.orchestrator.set")
	if len(onCalls) != 1 {
		t.Fatalf("expected one set call, got %d", len(onCalls))
	}
	if params, ok := onCalls[0].params.(map[string]any); !ok || params["enabled"] != true {
		t.Fatalf("unexpected set params: %#v", onCalls[0].params)
	}

	// The completions mirror getArgumentCompletions.
	if items := CompleteCommand("o"); len(items) != 2 {
		t.Fatalf("expected on/off completions, got %+v", items)
	}
	if items := CompleteCommand("z"); len(items) != 0 {
		t.Fatalf("expected no completions, got %+v", items)
	}
}

// pendingSnapshot copies the pending projection for assertions.
func (e *HerdsmanExtension) pendingSnapshot() []AgentEventWireRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AgentEventWireRecord{}, e.pendingEvents...)
}

// contextSnapshot builds an AgentWorkspaceContextSnapshot (TS helper).
func contextSnapshot(lastAssistantText string) *AgentWorkspaceContextSnapshot {
	return &AgentWorkspaceContextSnapshot{
		Agents: []AgentContextListItem{
			{
				Agent:       stringPtr("claude"),
				AgentStatus: stringPtr("idle"),
				History: &CompactAgentHistory{
					LastAssistantMessage: &CompactText{Text: &lastAssistantText},
				},
				PaneID:     stringPtr("wB:p-agent"),
				TerminalID: stringPtr("term_agent"),
			},
		},
		HerdrSessionName: "default",
		UpdatedAt:        "2026-07-16T00:00:00.000Z",
		WorkspaceID:      "wB",
	}
}
