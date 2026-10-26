package herdsmanpi

// Port of test/unit/herdsman-pi-extension.test.ts — part 3: wake injection,
// the busy deferral ladder, dead-letter write-off, the retry budget, silent
// upstream-error acknowledgement and the keepalive.
//
// The TS uses vi.useFakeTimers and `vi.advanceTimersByTimeAsync`; the Go port
// installs the manual clock (harness_test.go) and calls Advance, which runs each
// due callback synchronously.

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// wakeFixture builds a started extension whose client answers connection-state
// queries with the given register events, exactly like the TS `createWakeClient`.
//
// `agent.orchestrator.get` re-lists whatever is pending, the way the daemon's
// pending discovery does: a wake pass re-reads the queue before injecting, and a
// page that omitted a still-pending event would assert that the server settled
// it (see `syncPendingEventsFromServer`, which only reaps on the ack watermark).
func wakeFixture(t *testing.T, events []AgentEventWireRecord, idle bool) *fixture {
	t.Helper()
	f := newFixture(t, ExtensionOptions{}, idle, &herdsmanEnv{})
	// The "server page" the fake daemon re-lists. It is filled by `streamEvent`
	// and drained by a successful ack, which is what the real daemon does: a
	// pending row stays listed until the watermark covers it.
	serverPage := &serverPageState{}
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.set":
			return connectionResponse(f.client, &connectionOptions{Events: events, HasEvents: true})
		case "agent.orchestrator.get":
			// No lock: the extension holds its own mutex across this call.
			return connectionResponse(f.client, &connectionOptions{Events: serverPage.snapshot(), HasEvents: true})
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.serverPage = serverPage
	f.start()
	return f
}

func (f *fixture) wakeInjections() []sentMessage { return f.pi.WakeInjections() }

func streamEvent(f *fixture, event AgentEventWireRecord) {
	if f.serverPage != nil {
		f.serverPage.put(event)
	}
	f.client.EmitStream(DaemonStreamMessage{Method: "agent.event", Event: &event})
}

// serverPageState is the fake daemon's pending page: what `orchestrator.get`
// re-lists. Guarded by its own mutex so it is safe to read from inside the
// client's request callback (the extension holds its mutex there).
type serverPageState struct {
	mu     sync.Mutex
	events map[int]AgentEventWireRecord
}

func (p *serverPageState) put(event AgentEventWireRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.events == nil {
		p.events = map[int]AgentEventWireRecord{}
	}
	p.events[event.ID] = event
}

func (p *serverPageState) snapshot() []AgentEventWireRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := make([]AgentEventWireRecord, 0, len(p.events))
	for _, event := range p.events {
		list = append(list, event)
	}
	sort.Slice(list, func(left, right int) bool { return list[left].ID < list[right].ID })
	return list
}

func detailsOf(t *testing.T, message sentMessage) (eventIDs []int, presented []int) {
	t.Helper()
	details, ok := message.message.Details.(map[string]any)
	if !ok {
		t.Fatalf("wake details are %#v", message.message.Details)
	}
	toInts := func(raw any) []int {
		values, ok := raw.([]any)
		if !ok {
			return nil
		}
		out := make([]int, 0, len(values))
		for _, value := range values {
			if id, ok := jsNumberAsInt(value); ok {
				out = append(out, id)
			}
		}
		return out
	}
	return toInts(details["eventIds"]), toInts(details["presentedEventIds"])
}

func TestWakeIgnoresNonOutcomesAndSelfEvents(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(30, "agent.status.changed", map[string]any{"from": "working", "to": "done"}, nil))
	streamEvent(f, wakeEvent(31, "agent.idle", map[string]any{"from": "done", "to": "idle"}, nil))
	self := wakeEvent(32, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_pi")})
	streamEvent(f, self)
	noTerminal := wakeEvent(33, "agent.done", map[string]any{}, &eventOptions{hasTerminal: true})
	streamEvent(f, noTerminal)

	f.timers.Advance(time.Second)
	if got := len(f.wakeInjections()); got != 0 {
		t.Fatalf("injected %d wakes for non-outcomes", got)
	}
}

func TestWakeCoalescesMultipleOutcomesIntoOneMessage(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(34, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	streamEvent(f, wakeEvent(35, "agent.blocked", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-2")}))
	f.timers.Advance(time.Second)

	injections := f.wakeInjections()
	if len(injections) != 1 {
		t.Fatalf("expected one coalesced wake, got %d", len(injections))
	}
	message := injections[0]
	if message.message.CustomType != wakeContextCustomType || message.message.Display != false {
		t.Fatalf("unexpected message shape: %+v", message.message)
	}
	if message.options.DeliverAs == nil || *message.options.DeliverAs != "followUp" {
		t.Fatalf("unexpected deliverAs: %+v", message.options.DeliverAs)
	}
	if message.options.TriggerTurn == nil || !*message.options.TriggerTurn {
		t.Fatalf("unexpected triggerTurn: %+v", message.options.TriggerTurn)
	}
	eventIDs, presented := detailsOf(t, message)
	if joinInts(eventIDs) != "34,35" || joinInts(presented) != "34,35" {
		t.Fatalf("unexpected details: eventIds=%v presented=%v", eventIDs, presented)
	}
	// The content is the formatted update block, policy first.
	mustContain(t, message.message.Content, "[HERDSMAN WAKE POLICY]")
	mustContain(t, message.message.Content, agentUpdatesHeading)
	mustContain(t, message.message.Content, "event: 34")
	mustContain(t, message.message.Content, "event: 35")
}

func TestWakeRetainsPendingOutcomesWhenInjectionFails(t *testing.T) {
	f := wakeFixture(t, nil, true)
	f.pi.SetSendFailure(true)
	streamEvent(f, wakeEvent(36, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)

	if got := len(f.wakeInjections()); got != 0 {
		t.Fatalf("a failed injection was recorded (%d)", got)
	}
	if len(f.core.pendingSnapshot()) == 0 {
		t.Fatal("the pending outcome was dropped after a failed injection")
	}
	if len(f.core.wakeAwaitingConsumption) != 0 {
		t.Fatalf("ids entered the awaiting set on a failed send: %v", f.core.wakeAwaitingConsumption)
	}
	if f.core.deliveredBatch != nil {
		t.Fatalf("a batch was recorded for a failed send: %+v", f.core.deliveredBatch)
	}

	// Once the transport works again the next wake presents it.
	f.pi.SetSendFailure(false)
	f.core.ScheduleWake(f.ctx)
	f.timers.Advance(time.Second)
	if got := len(f.wakeInjections()); got != 1 {
		t.Fatalf("expected the retry to inject, got %d", got)
	}
}

func TestWakeDefersWhileOrchestratorIsBusy(t *testing.T) {
	f := wakeFixture(t, nil, false) // idle:false
	f.core.HandleAgentStart()
	streamEvent(f, wakeEvent(37, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(500 * time.Millisecond)
	if got := len(f.wakeInjections()); got != 0 {
		t.Fatalf("injected into a busy orchestrator (%d)", got)
	}
	if !f.core.wakeDeferredUntilSettled {
		t.Fatal("the wake was not marked deferred")
	}
	status := f.ctx.Status("herdsman-wake")
	if status == nil || *status != "Herdsman · waiting for the current turn to end" {
		t.Fatalf("unexpected waiting status: %v", deref(status))
	}

	// The settlement drops the pending poll and re-takes the wake decision; the
	// injectable pass itself is still armed on WAKE_SETTLE_MS==0, so the manual
	// clock has to step once before the message is sent.
	f.ctx.SetIdle(true)
	f.core.HandleAgentSettled(f.ctx)
	f.timers.Advance(0)
	if got := len(f.wakeInjections()); got != 1 {
		t.Fatalf("settlement did not inject (%d)", got)
	}
}

func TestWakeBusyLadderBacksOff(t *testing.T) {
	f := wakeFixture(t, nil, false)
	f.core.HandleAgentStart()
	streamEvent(f, wakeEvent(640, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)
	// 100 -> 200 -> 500 walked; a genuinely busy orchestrator gets nothing.
	if got := len(f.wakeInjections()); got != 0 {
		t.Fatalf("injected while busy (%d)", got)
	}

	// The turn ended but no settlement arrives: the next rung is 1000ms away,
	// so a flat 100ms spin would have released by now and the ladder has not.
	f.ctx.SetIdle(true)
	f.timers.Advance(700 * time.Millisecond)
	if got := len(f.wakeInjections()); got != 0 {
		t.Fatalf("the backed-off poll released early (%d)", got)
	}
	f.timers.Advance(400 * time.Millisecond)
	injections := f.wakeInjections()
	if len(injections) != 1 {
		t.Fatalf("the backed-off tick did not release the wake (%d)", len(injections))
	}
	if injections[0].options.DeliverAs == nil || *injections[0].options.DeliverAs != "followUp" {
		t.Fatalf("unexpected delivery: %+v", injections[0].options.DeliverAs)
	}
}

func TestWakeLadderConstants(t *testing.T) {
	want := []int64{100, 200, 500, 1_000, 2_000, 5_000}
	if len(WakeBusyBackoffMS) != len(want) {
		t.Fatalf("ladder=%v", WakeBusyBackoffMS)
	}
	for index, value := range want {
		if WakeBusyBackoffMS[index] != value {
			t.Fatalf("rung %d = %d want %d", index, WakeBusyBackoffMS[index], value)
		}
	}
	if WakeBusySpinMS != WakeBusyBackoffMS[0] {
		t.Fatal("the first rung must be WAKE_BUSY_SPIN_MS")
	}
	if WakeBusyBackoffMS[len(WakeBusyBackoffMS)-1] != WakeBusyBackoffCapMS {
		t.Fatal("the last rung must be the cap")
	}
	if MaxAckAttempts != 5 || AckBackoffCapMS != 30_000 || KeepaliveIntervalMS != 30_000 {
		t.Fatalf("constants drifted: %d %d %d", MaxAckAttempts, AckBackoffCapMS, KeepaliveIntervalMS)
	}
	if WakeDeadLetterRetryLimit != 2 {
		t.Fatalf("dead-letter limit=%d", WakeDeadLetterRetryLimit)
	}
	if ackBackoffMs(1) != 250 || ackBackoffMs(2) != 500 || ackBackoffMs(20) != AckBackoffCapMS {
		t.Fatalf("ackBackoffMs ladder drifted: %d %d %d", ackBackoffMs(1), ackBackoffMs(2), ackBackoffMs(20))
	}
}

func TestWakeDeadLettersUndeliveredContentOnSettle(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(38, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)
	if got := len(f.wakeInjections()); got != 1 {
		t.Fatalf("expected the wake injection, got %d", got)
	}
	if !f.core.wakeAwaitingConsumption[38] {
		t.Fatalf("id 38 does not await consumption: %v", f.core.wakeAwaitingConsumption)
	}

	// The turn ends without the content ever reaching the transcript.
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(assistantEnd("error", nil))
	f.core.HandleAgentSettled(f.ctx)

	if f.core.wakeAwaitingConsumption[38] {
		t.Fatal("the stranded id still awaits consumption after the write-off")
	}
	if !f.core.wakeRetryableEventIDs[38] {
		t.Fatal("the stranded id was not written off")
	}
	if _, present := f.core.wakeDeadLetterRetryAt[38]; !present {
		t.Fatal("no retry deadline was pinned")
	}
	// The write-off must never acknowledge.
	if acks := f.client.AckedIDs(); len(acks) != 0 {
		t.Fatalf("the write-off acknowledged ids: %v", acks)
	}
	last := f.ctx.LastNotification()
	if last == nil || last.level != NotifyWarning || !strings.Contains(last.message, "could not be delivered") {
		t.Fatalf("unexpected notification: %+v", last)
	}
}

func TestWakeKeepsUndeliveredIDForTheDaemon(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(39, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(assistantEnd("error", nil))
	f.core.HandleAgentSettled(f.ctx)

	// Nothing was acknowledged, so the daemon still holds the update.
	if acks := f.client.AckedIDs(); len(acks) != 0 {
		t.Fatalf("acked after a write-off: %v", acks)
	}
	// It stays in the delivery queue as the watermark barrier.
	if _, queued := f.core.unackedDelivered[39]; !queued {
		t.Fatal("the written-off id left the delivery queue")
	}
	barrier := f.core.unconfirmedWatermarkBarrier()
	if barrier == nil || barrier.EventID != 39 {
		t.Fatalf("unexpected barrier: %+v", barrier)
	}
	if barrier.Reason != "dead-letter-backoff" {
		t.Fatalf("unexpected barrier reason: %s", barrier.Reason)
	}
}

func TestWakeDoesNotReinjectInsideTheWriteOffCycle(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(40, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(assistantEnd("error", nil))
	f.core.HandleAgentSettled(f.ctx)
	// The settle scheduled a wake decision, but the dead-letter backoff (5s) has
	// not elapsed, so the same id may not drive another turn yet.
	f.timers.Advance(100 * time.Millisecond)
	if got := len(f.wakeInjections()); got != 1 {
		t.Fatalf("the write-off cycle re-injected (%d injections)", got)
	}
}

func TestWakeStopsRetryingOnceBudgetIsSpent(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(41, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)
	for attempt := 0; attempt < WakeDeadLetterRetryLimit+1; attempt++ {
		f.core.HandleAgentStart()
		f.core.HandleMessageEnd(assistantEnd("error", nil))
		f.core.HandleAgentSettled(f.ctx)
		f.timers.Advance(20 * time.Second)
	}
	injections := len(f.wakeInjections())
	if injections > 1+WakeDeadLetterRetryLimit {
		t.Fatalf("retried past the budget: %d injections", injections)
	}
	if f.core.wakeDeadLetterAttempts[41] > WakeDeadLetterRetryLimit {
		t.Fatalf("attempt counter overspent: %d", f.core.wakeDeadLetterAttempts[41])
	}
}

func TestWakeRepresentsDeadLetterWhenFreshContentMerges(t *testing.T) {
	f := wakeFixture(t, nil, true)
	streamEvent(f, wakeEvent(42, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-1")}))
	f.timers.Advance(time.Second)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(assistantEnd("error", nil))
	f.core.HandleAgentSettled(f.ctx)
	if !f.core.wakeRetryableEventIDs[42] {
		t.Fatal("id 42 was not written off")
	}

	// A fresh external event arrives: the dead-lettered id rides that wake for
	// free (no attempt is spent).
	streamEvent(f, wakeEvent(43, "agent.done", map[string]any{}, &eventOptions{paneID: stringPtr("wB:p-2")}))
	f.timers.Advance(time.Second)
	injections := f.wakeInjections()
	if len(injections) != 2 {
		t.Fatalf("expected a second wake, got %d", len(injections))
	}
	eventIDs, presented := detailsOf(t, injections[1])
	if !strings.Contains(joinInts(eventIDs), "42") {
		t.Fatalf("the dead-lettered id did not ride the fresh wake: %v", eventIDs)
	}
	if !strings.Contains(joinInts(presented), "43") {
		t.Fatalf("the fresh content was not presented: %v", presented)
	}
	if f.core.wakeDeadLetterAttempts[42] != 0 {
		t.Fatalf("riding a fresh wake spent a retry attempt: %d", f.core.wakeDeadLetterAttempts[42])
	}
}

func TestWakeSweepsDeadLetterBookkeepingOnConfirm(t *testing.T) {
	f := wakeFixture(t, nil, true)
	f.core.mu.Lock()
	f.core.wakeRetryableEventIDs[651] = true
	f.core.wakeDeadLetterAttempts[651] = 2
	f.core.wakeDeadLetterRetryAt[651] = f.core.nowMS() + 15_000
	f.core.wakeRetryableEventIDs[652] = true
	f.core.wakeDeadLetterAttempts[652] = 1
	f.core.wakeDeadLetterRetryAt[652] = f.core.nowMS() + 5_000
	f.core.mu.Unlock()
	if _, queued := f.core.unackedDelivered[651]; queued {
		t.Fatal("test setup: 651 must not be in the queue")
	}

	watermark := 651
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get", "agent.orchestrator.set":
			return connectionResponse(f.client, &connectionOptions{AckedEventID: &watermark, HasAcked: true})
		default:
			return map[string]any{"acknowledged": true}
		}
	}
	f.core.handleCommand("status", f.ctx)

	if f.core.wakeRetryableEventIDs[651] || f.core.wakeDeadLetterAttempts[651] != 0 {
		t.Fatalf("651 bookkeeping survived the watermark")
	}
	if _, present := f.core.wakeDeadLetterRetryAt[651]; present {
		t.Fatal("651 retry deadline survived the watermark")
	}
	if !f.core.wakeRetryableEventIDs[652] || f.core.wakeDeadLetterAttempts[652] != 1 {
		t.Fatalf("652 bookkeeping was swept early")
	}
	if _, present := f.core.wakeDeadLetterRetryAt[652]; !present {
		t.Fatal("652 retry deadline was swept early")
	}
}
