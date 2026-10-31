package herdsman

// End-to-end regression net for the herdsman extension's notification path:
//
//	session_start → presence register → daemon stream (agent.event) → pending
//	projection → wake scheduling → hidden wake injection (SendMessage
//	customType=herdsman-wake-context, display=false, triggerTurn) → wake
//	consumption evidence → agent_settled → agent.notifications.ack.
//
// The suite exists so a change that silently breaks "the daemon's agent
// updates wake this Pi up" is caught by `go test`, not by an operator noticing
// that agents finished and nobody was told. Two layers are covered:
//
//   - In-memory: the fixture's fakeClient emits stream messages while the
//     manual timers drive the wake ladder deterministically.
//   - Over a real socket: a real ReconnectingDaemonClient talks to a fake
//     daemon (extensionintegration_test.go's fakeDaemon harness), so the
//     notification decoder, the stream dispatcher and the RPC paths run on
//     the production client instead of a stub.
//
// Everything is hermetic: Unix sockets live under t.TempDir(), the wake
// timers are real but the negative cases order on observable barriers instead
// of sleeps, the turn-completion probe is injected so nothing polls a real
// session file, and no test touches a real daemon, ~/.herdsman, webhooks or
// the network.

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// daemonHarness is the socket-side counterpart of wakeFixture's serverPage:
// the fake daemon serves it, and the test owns when a pushed event becomes
// listable. A register/get response always reports an empty page until the
// test enqueues rows — the production daemon only lists an event once the
// agent actually finished, so an event that is listable from the register
// response on would inject even if the agent.event push path were dead (the
// pre-fixture bypass this type exists to close).
type daemonHarness struct {
	page *serverPageState
	// pendingEnqueue is run inside the daemon's `agent.orchestrator.get`
	// handler, before the response is written: the deterministic "the push
	// path ran" point at which the daemon may list the event.
	pendingEnqueue func()
	// pendingEnqueueOnce keeps a second wake pass from re-firing the hook.
	pendingEnqueueOnce sync.Once
	ackCount           atomic.Int64
}

// ackedEventIDs records the eventId of every `agent.notifications.ack` the
// daemon saw, in arrival order.
func (h *daemonHarness) ackedEventIDs() []int {
	h.page.mu.Lock()
	defer h.page.mu.Unlock()
	return append([]int{}, h.page.ackedIDs...)
}

// enqueue lists an event on the daemon's pending page. The positive chain
// fires it from inside the daemon's `agent.orchestrator.get` handler: that
// request is issued by `startWake` on the wake-timer goroutine, which the
// agent.event dispatch already scheduled, so the enqueue is ordered strictly
// after the push path ran — and lands before the response the wake applies.
func (h *daemonHarness) enqueue(event AgentEventWireRecord) {
	h.page.put(event)
}

// runPendingEnqueue fires the once-only enqueue the positive chain installs.
func (h *daemonHarness) runPendingEnqueue() {
	if h.pendingEnqueue == nil {
		return
	}
	h.pendingEnqueueOnce.Do(h.pendingEnqueue)
}

// newRealSocketFixture is newRealClientFixture with the production clock: the
// E2E chain must fire the extension's wake timers (WAKE_SETTLE_MS, the busy
// ladder) for real, where the socket suites only need the client transport.
// The fixture cleans up after itself: shutdown stops the wake timer, the
// keepalive ticker, the client read loop and the dispatcher, and the daemon's
// Close drops the accepted connections.
func newRealSocketFixture(t *testing.T) (*fixture, *fakeDaemon, *daemonHarness) {
	t.Helper()
	daemon := newFakeDaemon(t)
	harness := &daemonHarness{page: &serverPageState{}}
	servePendingDaemon(daemon, harness)
	var fixture *fixture
	fixture = newFixture(t, ExtensionOptions{
		Env:    herdrEnvFor(daemon.socket),
		Timers: realTimers{},
		// The session file is a fixture path, so the probe is only injected to
		// keep the turn-completion signal deterministic: no test ever polls a
		// real file for text that was never written.
		ConfirmSessionWrite: func(expectedText, path string) TurnCompletionCheck {
			return TurnCompletionCheck{Confirmed: true, Reason: ReasonAlreadyWritten}
		},
		ClientFactory: func() HerdsmanDaemonClient {
			client := &recordingClient{
				HerdsmanDaemonClient: NewReconnectingDaemonClient(daemon.socket, nil),
				fixture:              fixture,
			}
			return client
		},
	}, true, nil)
	t.Cleanup(fixture.core.HandleSessionShutdown)
	return fixture, daemon, harness
}

// awaitStreamHandled returns once the handler of the pushed stream message at
// the given index (0-based, in delivery order) has returned. The real client
// pushes stream messages onto its own streamQueue and the streamLoop goroutine
// invokes the handler serially; RPC responses bypass that queue entirely and
// are resolved straight on the read loop. So an answered `agent.orchestrator.get`
// proves NOTHING about whether a pushed message's handler already ran — the
// observable "processing done" signal the negative cases need is the
// recordingClient's handled ledger, which is appended strictly after the
// handler returns (i.e. after any wake timer it scheduled was armed).
func awaitStreamHandled(t *testing.T, f *fixture, index int) {
	t.Helper()
	recorder, ok := f.recorder()
	if !ok {
		t.Fatal("the fixture has no recording client")
	}
	awaitCondition(t, fmt.Sprintf("stream message %d handled", index), func() bool {
		return recorder.handledCount() > index
	})
}

// wakeTimerArmed reports whether the core currently has a wake timer planted.
func wakeTimerArmed(f *fixture) bool {
	f.core.mu.Lock()
	defer f.core.mu.Unlock()
	return f.core.wakeTimer != nil
}

// settleWindow returns after every short-horizon wake timer (settle at 0ms,
// the busy ladder's first rungs) has had its chance to fire. It is NOT a
// processing barrier — nothing about an RPC answer orders it after a pushed
// message's handler — it only gives a *wrongly surviving* timer room to run
// after the handler-return signal already proved processing is done.
// Positive assertions always wait on an observable signal first.
func settleWindow() { time.Sleep(250 * time.Millisecond) }

// ── in-memory end-to-end ────────────────────────────────────────────────────

// TestE2EDispatchToWakeToAck drives the whole chain on the in-memory fixture:
// a registration that owns the workspace, an agent.event pushed by the daemon,
// the wake injection that must follow it, the turn that consumes it and the
// acknowledgement that closes the loop.
func TestE2EDispatchToWakeToAck(t *testing.T) {
	f := wakeFixture(t, nil, true)

	// Dispatch side: the registration carried this Pi's pane/subscriber/terminal
	// identity, and it is the identity the wake path below checks events
	// against (an event must come from a *different* terminal).
	registers := f.client.CallsFor("agent.orchestrator.register")
	if len(registers) != 1 {
		t.Fatalf("expected exactly one register call, got %d", len(registers))
	}
	params, ok := registers[0].params.(map[string]any)
	if !ok {
		t.Fatalf("register params are %#v", registers[0].params)
	}
	if got := params["paneId"]; got != "wB:p1" {
		t.Fatalf("register paneId=%v", got)
	}
	if got := params["subscriberId"]; got != "pi-session" {
		t.Fatalf("register subscriberId=%v", got)
	}
	if got := params["workspaceId"]; got != "wB" {
		t.Fatalf("register workspaceId=%v", got)
	}
	f.core.mu.Lock()
	isOrchestrator := f.core.isOrchestrator
	f.core.mu.Unlock()
	if !isOrchestrator {
		t.Fatal("the extension did not take the orchestrator role at register")
	}

	// The worker session finished: the daemon pushes the event over the
	// notification channel. From a foreign terminal it is a pending outcome
	// and schedules the wake; from our own terminal it must be ignored.
	worker := wakeEvent(1, "agent.done",
		map[string]any{"agent": "claude", "name": "worker"},
		&eventOptions{paneID: stringPtr("wB:p-worker"), terminal: stringPtr("term_agent")})
	streamEvent(f, worker)
	self := wakeEvent(2, "agent.done", map[string]any{}, &eventOptions{
		paneID:   stringPtr("wB:p-self"),
		terminal: stringPtr("term_pi"),
	})
	// Emitted directly, not through streamEvent: an own-terminal event must not
	// join the fake daemon's pending page either, because a real daemon only
	// ever lists other terminals' rows on it.
	f.client.EmitStream(DaemonStreamMessage{Method: "agent.event", Event: ptrEvent(self)})
	if pending := f.core.pendingSnapshot(); len(pending) != 1 || pending[0].ID != 1 {
		t.Fatalf("the stream event did not join the pending projection: %+v", pending)
	}
	// The event is journaled into the session too, for the wake renderer.
	if got := len(f.pi.Entries); got != 1 {
		t.Fatalf("agent_event entries: %d", got)
	} else if f.pi.Entries[0].method != agentEventEntryType {
		t.Fatalf("entry customType: %s", f.pi.Entries[0].method)
	}

	// Wake side: WAKE_SETTLE_MS is 0 on the injected clock, so one advance runs
	// the settle timer and the injectable pass.
	f.timers.Advance(time.Second)
	injections := f.wakeInjections()
	if len(injections) != 1 {
		t.Fatalf("expected exactly one wake injection, got %d", len(injections))
	}
	injection := injections[0]
	if injection.options.DeliverAs == nil || *injection.options.DeliverAs != "followUp" ||
		injection.options.TriggerTurn == nil || !*injection.options.TriggerTurn {
		t.Fatalf("the wake was not delivered as a turn-triggering follow-up: %+v", injection.options)
	}
	mustContain(t, injection.message.Content, agentUpdatesHeading)
	mustContain(t, injection.message.Content, "event: 1")
	if _, presented := detailsOf(t, injection); joinInts(presented) != "1" {
		t.Fatalf("presented event ids: %v", presented)
	}

	// The wake turn runs and the injected copy reaches the transcript. The
	// consumption evidence is replayed from the injection's own details rather
	// than hardcoded, so the test asserts on what was actually sent.
	consumedIDs, presentedIDs := detailsOf(t, injection)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(wakeConsumptionEvidence(presentedIDs, consumedIDs...))
	f.core.HandleMessageEnd(assistantEnd("stop", nil))
	if acks := f.client.AckedIDs(); len(acks) != 0 {
		t.Fatalf("acked before the turn settled: %v", acks)
	}
	f.core.HandleAgentSettled(f.ctx)
	f.waitForCompletionSignal()

	// The loop closes: the daemon got the ack for the consumed event and the
	// turn-completion signal carries the same scope identity.
	if got := joinInts(f.client.AckedIDs()); got != "1" {
		t.Fatalf("acks after settlement: %s", got)
	}
	completed := f.client.CallsFor("agent.turn.completed")
	if len(completed) != 1 {
		t.Fatalf("turn completion signals: %d", len(completed))
	}
	completedParams, _ := completed[0].params.(map[string]any)
	if got := completedParams["paneId"]; got != "wB:p1" {
		t.Fatalf("turn.completed paneId=%v", got)
	}
	if got := completedParams["terminalId"]; got != "term_pi" {
		t.Fatalf("turn.completed terminalId=%v", got)
	}
	if got := completedParams["workspaceId"]; got != "wB" {
		t.Fatalf("turn.completed workspaceId=%v", got)
	}
	if pending := f.core.pendingSnapshot(); len(pending) != 0 {
		t.Fatalf("pending projection not drained: %+v", pending)
	}
}

// ── real socket end-to-end ──────────────────────────────────────────────────

// servePendingDaemon answers the daemon's RPC surface the way a live herdsman
// daemon does: register/set/get return a connection state whose owner is this
// terminal and whose pending page is the harness's — empty until the test
// enqueues onto it. Acks are validated against the event the harness last
// listed (the daemon only ever sees an ack for an id it served to this scope)
// before they are counted, atomically, so the test goroutine can read the
// counter while the serve goroutine writes it.
func servePendingDaemon(daemon *fakeDaemon, harness *daemonHarness) {
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		switch message.Method {
		case "agent.orchestrator.register", "agent.orchestrator.get", "agent.orchestrator.set":
			if message.Method == "agent.orchestrator.get" {
				// The wake pass's get can only be issued by startWake, which the
				// agent.event handler already scheduled — so enqueueing the pushed
				// event here keeps the page honest (empty until the push path ran)
				// without any client-side hook.
				harness.runPendingEnqueue()
			}
			result := daemonStateResponse()
			result["events"] = harness.page.snapshot()
			daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": result})
		case "agent.notifications.ack":
			eventID, _ := jsNumberAsInt(message.Params["eventId"])
			if ackErr := harness.page.validateAck(eventID); ackErr != nil {
				daemon.WriteJSON(conn, map[string]any{"id": message.ID, "error": map[string]any{
					"message": ackErr.Error(),
				}})
				return
			}
			harness.ackCount.Add(1)
			daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{
				"acknowledged": true,
				"state": map[string]any{
					"ackedEventId":     eventID,
					"herdrSessionName": "default",
					"owner":            map[string]any{"paneId": "wB:p-test", "terminalId": "term_pi"},
					"updatedAt":        "2026-07-10T00:00:00.000Z",
					"workspaceId":      "wB",
				},
			}})
		default:
			daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
		}
	})
}

// awaitWakeInjection polls fakePi until the hidden wake message lands. The real
// client's stream dispatcher delivers notifications on its own goroutine, so
// the injection (and the `agent.orchestrator.get` refresh inside the wake pass)
// is observable a beat after the socket push.
func awaitWakeInjection(t *testing.T, f *fixture) sentMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if injections := f.pi.WakeInjections(); len(injections) > 0 {
			return injections[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no wake injection arrived (issued calls: %v)", f.client.Methods())
	return sentMessage{}
}

// TestE2ERealSocketEventToWakeInjection runs the dispatch→event→wake chain over
// a real Unix socket: the daemon pushes agent.event, the real client decodes
// and dispatches it, and the extension must end up writing the hidden wake
// context into Pi.
func TestE2ERealSocketEventToWakeInjection(t *testing.T) {
	f, daemon, harness := newRealSocketFixture(t)

	runWithWatchdog(t, "HandleSessionStart", func() {
		f.core.HandleSessionStart(f.ctx, nil)
	})
	awaitMethod(t, f, "agent.orchestrator.register")
	awaitRegistration(t, f.core)
	awaitAnswered(t, f, "agent.orchestrator.register")

	// Register carried the Herdr identity of this pane, and the page it saw
	// was empty: nothing may inject until the push path delivers the event.
	registers := f.client.CallsFor("agent.orchestrator.register")
	if len(registers) != 1 {
		t.Fatalf("register calls: %d", len(registers))
	}
	if params, ok := registers[0].params.(map[string]any); ok {
		if got := params["paneId"]; got != "wB:p-test" {
			t.Fatalf("register paneId=%v", got)
		}
		if got := params["subscriberKind"]; got != "pi" {
			t.Fatalf("register subscriberKind=%v", got)
		}
	} else {
		t.Fatalf("register params are %#v", registers[0].params)
	}
	if pending := f.core.pendingSnapshot(); len(pending) != 0 {
		t.Fatalf("the empty register page produced pending events: %+v", pending)
	}

	// The daemon reports an agent's completion on the notification channel.
	// The write goes from a separate goroutine: the serve handler runs the
	// wake's follow-up `agent.orchestrator.get` on this same conn, and a
	// synchronous push from the test goroutine would only be read after the
	// client answers — which is fine, but pushing from a goroutine keeps the
	// ordering identical to a daemon that notifies unprompted.
	workerEvent := wakeEvent(1, "agent.done",
		map[string]any{"agent": "claude", "name": "worker"},
		&eventOptions{
			paneID:   stringPtr("wB:p-worker"),
			terminal: stringPtr("term_agent"),
			text:     stringPtr("worker finished"),
		})
	// The event may only join the daemon's pending page once the wake pass
	// re-reads it (the daemon's get handler fires this hook): an injection
	// that never saw the agent.event push cannot happen.
	harness.pendingEnqueue = func() { harness.enqueue(workerEvent) }
	eventFrame := map[string]any{
		"method": "agent.event",
		"params": map[string]any{
			"herdrSessionName": "default",
			"workspaceId":      "wB",
			"event":            workerEvent,
		},
	}
	conn := mustConn(t, daemon)
	go daemon.WriteJSON(conn, eventFrame)

	// The wake pass first re-reads the pending page, then injects. That get is
	// what enqueues the event onto the daemon's page (see servePendingDaemon):
	// it can only run after the push handler scheduled the wake, so the
	// injection below is impossible without a working agent.event path.
	awaitAnswered(t, f, "agent.orchestrator.get")
	awaitCondition(t, "event joined pending", func() bool {
		return len(f.core.pendingSnapshot()) == 1
	})
	injection := awaitWakeInjection(t, f)
	if injection.message.Display {
		t.Fatal("the wake was sent as a visible message")
	}
	if injection.options.TriggerTurn == nil || !*injection.options.TriggerTurn {
		t.Fatalf("the wake does not trigger a turn: %+v", injection.options)
	}
	mustContain(t, injection.message.Content, agentUpdatesHeading)
	mustContain(t, injection.message.Content, "event: 1")

	// The wake turn consumes the copy and settles: the ack must leave over the
	// same socket, so the daemon-side counter is the end-to-end evidence. The
	// consumption evidence replays the injection's own details, and the settle
	// is awaited through the completion-signal barrier so no turn-signal
	// goroutine outlives the test.
	consumedIDs, presentedIDs := detailsOf(t, injection)
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(wakeConsumptionEvidence(presentedIDs, consumedIDs...))
	f.core.HandleMessageEnd(assistantEnd("stop", nil))
	f.core.HandleAgentSettled(f.ctx)
	f.waitForCompletionSignal()
	awaitAnswered(t, f, "agent.notifications.ack")
	if harness.ackCount.Load() == 0 {
		t.Fatal("the daemon never saw the acknowledgement")
	}
	if got := joinInts(harness.ackedEventIDs()); got != "1" {
		t.Fatalf("the daemon acked unexpected event ids: %s", got)
	}
}

// TestE2ERealSocketSelfEventDoesNotWake pins the guard in the same socket
// setup: an event that carries this terminal's id is our own work and must not
// produce a wake.
func TestE2ERealSocketSelfEventDoesNotWake(t *testing.T) {
	f, daemon, _ := newRealSocketFixture(t)

	runWithWatchdog(t, "HandleSessionStart", func() {
		f.core.HandleSessionStart(f.ctx, nil)
	})
	awaitMethod(t, f, "agent.orchestrator.register")
	awaitRegistration(t, f.core)
	awaitAnswered(t, f, "agent.orchestrator.register")

	daemon.WriteJSON(mustConn(t, daemon), map[string]any{
		"method": "agent.event",
		"params": map[string]any{
			"herdrSessionName": "default",
			"workspaceId":      "wB",
			"event": map[string]any{
				"id":          5,
				"type":        "agent.done",
				"terminalId":  "term_pi",
				"paneId":      "wB:p-test",
				"workspaceId": "wB",
				"payload":     map[string]any{"agent": "pig"},
			},
		},
	})

	// The handler-return signal is the barrier an RPC answer cannot be: it
	// flips only after the self event's handler ran to completion, so whatever
	// the handler was ever going to schedule is armed (or fired) by this point.
	awaitStreamHandled(t, f, 0)
	if got := len(f.pi.WakeInjections()); got != 0 {
		t.Fatalf("an own-terminal event produced a wake: %d injections", got)
	}
	if pending := f.core.pendingSnapshot(); len(pending) != 0 {
		t.Fatalf("an own-terminal event entered the pending projection: %+v", pending)
	}
	// And nothing may surface late either: every short-horizon wake timer the
	// handler could have planted is a settle (0ms) or a busy-ladder rung.
	settleWindow()
	if got := len(f.pi.WakeInjections()); got != 0 {
		t.Fatalf("an own-terminal event produced a late wake: %d injections", got)
	}
	if wakeTimerArmed(f) {
		t.Fatal("an own-terminal event left a wake timer armed")
	}
}

// TestE2ERealSocketRoleLossStopsWake is the safety half of the chain over the
// wire: a wake already armed for this scope must be torn down when the daemon
// hands the role to another pane, not merely fail to re-arm afterwards.
func TestE2ERealSocketRoleLossStopsWake(t *testing.T) {
	f, daemon, _ := newRealSocketFixture(t)

	runWithWatchdog(t, "HandleSessionStart", func() {
		f.core.HandleSessionStart(f.ctx, nil)
	})
	awaitMethod(t, f, "agent.orchestrator.register")
	awaitRegistration(t, f.core)
	awaitAnswered(t, f, "agent.orchestrator.register")

	// Arm a real wake first: the busy ladder gives the orchestrator a pending
	// poll while a turn runs, so the role loss below has an actual timer to
	// cancel — a role loss that only prevented future wakes would pass a test
	// that never planted one.
	f.core.HandleAgentStart()
	f.ctx.SetIdle(false)
	conn := mustConn(t, daemon)
	daemon.WriteJSON(conn, map[string]any{
		"method": "agent.event",
		"params": map[string]any{
			"herdrSessionName": "default",
			"workspaceId":      "wB",
			"event": map[string]any{
				"id":          6,
				"type":        "agent.done",
				"terminalId":  "term_agent",
				"paneId":      "wB:p-worker",
				"workspaceId": "wB",
				"payload":     map[string]any{"agent": "claude"},
			},
		},
	})
	// The push is fully processed once its handler returns; a scheduled wake
	// then shows up as an armed timer, the observable "a wake is scheduled"
	// signal.
	awaitStreamHandled(t, f, 0)
	awaitCondition(t, "the wake is scheduled", func() bool {
		return wakeTimerArmed(f)
	})

	// The daemon hands the role to another terminal.
	daemon.WriteJSON(conn, map[string]any{
		"method": "agent.orchestrator.changed",
		"params": map[string]any{
			"herdrSessionName": "default",
			"workspaceId":      "wB",
			"change": map[string]any{
				"reason": "claimed",
				"current": map[string]any{
					"herdrSessionName": "default",
					"owner":            map[string]any{"paneId": "wB:p-other", "terminalId": "term_agent"},
					"workspaceId":      "wB",
				},
				"previous": map[string]any{
					"herdrSessionName": "default",
					"owner":            map[string]any{"paneId": "wB:p-test", "terminalId": "term_pi"},
					"workspaceId":      "wB",
				},
			},
		},
	})
	// Role loss is delivered over the same stream queue, so the next handled
	// entry (index 1) also proves loseRole already ran — its handler arm or
	// tear-down is done before the ledger records the return.
	awaitStreamHandled(t, f, 1)
	awaitCondition(t, "role lost", func() bool {
		f.core.mu.Lock()
		defer f.core.mu.Unlock()
		return !f.core.isOrchestrator
	})
	if wakeTimerArmed(f) {
		t.Fatal("the armed wake survived the role loss")
	}
	if f.core.wakeDeferredUntilSettled {
		t.Fatal("the deferral flag survived the role loss")
	}
	if pending := f.core.pendingSnapshot(); len(pending) != 0 {
		t.Fatalf("the pending projection survived the role loss: %+v", pending)
	}

	// After the role loss, pushing an event must not wake this Pi: it is no
	// longer the orchestrator and dropped the scope with the role.
	daemon.WriteJSON(conn, map[string]any{
		"method": "agent.event",
		"params": map[string]any{
			"herdrSessionName": "default",
			"workspaceId":      "wB",
			"event": map[string]any{
				"id":          7,
				"type":        "agent.done",
				"terminalId":  "term_agent",
				"paneId":      "wB:p-worker",
				"workspaceId": "wB",
				"payload":     map[string]any{"agent": "claude"},
			},
		},
	})
	// Stream message index 2 is the post-role-loss push: waiting for its
	// handler's return proves the extension already dropped it, after which the
	// settle window only watches for a wrongly armed late fire.
	awaitStreamHandled(t, f, 2)
	settleWindow()
	if got := len(f.pi.WakeInjections()); got != 0 {
		t.Fatalf("a wake fired after the role was lost: %d injections", got)
	}
	if wakeTimerArmed(f) {
		t.Fatal("a wake timer is armed after the role was lost")
	}
}
