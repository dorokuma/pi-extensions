package herdsmanpi

// Port of test/unit/herdsman-pi-extension.test.ts — part 1: acknowledgement
// failure classification, self-contained loading, socket default, hidden
// update formatting, the Herdr environment gate and presence registration.
//
// The TS drives the extension through a fake `pi` object and a fake client; the
// Go port uses the same shapes (fakePi/fakeClient/fakeContext in harness_test.go)
// and the manual clock instead of vi.useFakeTimers.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type codedError struct {
	code    string
	message string
}

func (e *codedError) Error() string   { return e.message }
func (e *codedError) AckCode() string { return e.code }

func TestClassifyAckFailureByMessage(t *testing.T) {
	cases := []struct {
		message string
		want    AckFailureClass
	}{
		{"invalidated", AckTerminal},
		{"no longer pending", AckTerminal},
		{"Only the current orchestrator can acknowledge notifications", AckTerminal},
		{"Only the next pending orchestrator event can be acknowledged", AckResync},
		{"an unknown daemon failure", AckTransient},
	}
	for _, entry := range cases {
		if got := ClassifyAckFailure(errors.New(entry.message)); got != entry.want {
			t.Fatalf("classify(%q)=%s want %s", entry.message, got, entry.want)
		}
	}
}

func TestClassifyAckFailureByStructuredCode(t *testing.T) {
	cases := []struct {
		code string
		want AckFailureClass
	}{
		{"ORCHESTRATOR_NOT_OWNER", AckTerminal},
		{"ORCHESTRATOR_EVENT_INVALIDATED", AckTerminal},
		{"ORCHESTRATOR_EVENT_FAILED", AckTerminal},
		{"ORCHESTRATOR_EVENT_ALREADY_ACKED", AckTerminal},
		{"ORCHESTRATOR_EVENT_NOT_IN_SCOPE", AckTerminal},
		{"ORCHESTRATOR_EVENT_OUT_OF_ORDER", AckResync},
		{"ORCHESTRATOR_OWNER_REPLACED", AckTerminal},
		{"ORCHESTRATOR_EVENT_NOT_FOUND", AckTerminal},
		{"ORCHESTRATOR_BUSY", AckTransient},
		{"ORCHESTRATOR_CONNECTION_LOST", AckTransient},
		{"ORCHESTRATOR_RECONCILING", AckTransient},
		{"ORCHESTRATOR_ACK_TIMEOUT", AckTransient},
	}
	for _, entry := range cases {
		err := &codedError{code: entry.code, message: "legacy"}
		if got := ClassifyAckFailure(err); got != entry.want {
			t.Fatalf("classify(%s)=%s want %s", entry.code, got, entry.want)
		}
	}
}

func TestClassifyAckFailureCodeBeatsMessage(t *testing.T) {
	// `invalidated` in the message, but the structured code says transient.
	if got := ClassifyAckFailure(&codedError{code: "temporary_failure", message: "invalidated"}); got != AckTransient {
		t.Fatalf("got %s", got)
	}
	// A structured terminal code wins over a benign message.
	if got := ClassifyAckFailure(&codedError{code: "event_invalidated", message: "temporary"}); got != AckTerminal {
		t.Fatalf("got %s", got)
	}
}

func TestExtensionLoadsAndRejectsOversizedFrames(t *testing.T) {
	// The real client over a real socket: a frame larger than 1 MiB must break
	// the connection on the client side BEFORE any handler sees it, so an
	// oversized push can never reach the extension's stream handler.
	daemon := newFakeDaemon(t)
	serveDaemonState(daemon)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)

	var handled atomic.Int64
	var disconnects atomic.Int64
	client.SetOnStreamMessage(func(message DaemonStreamMessage) { handled.Add(1) })
	client.SetOnDisconnected(func(err error) { disconnects.Add(1) })

	awaitCondition(t, "the first connection", func() bool { return daemon.Connections() == 1 })
	// One frame over the limit, pushed straight at the client.
	daemon.Write(mustConn(t, daemon), strings.Repeat("x", DefaultMaxFrameBytes+1024))

	// The decoder refuses it, the connection goes away and the client retries.
	awaitCondition(t, "the disconnect after an oversized frame", func() bool {
		return disconnects.Load() > 0 && daemon.Connections() >= 2
	})
	if got := handled.Load(); got != 0 {
		t.Fatalf("the stream handler ran %d time(s) for a rejected frame", got)
	}

	// The shared decoder is the component that refused it, with the documented
	// error -- the same one the client runs.
	decoder, err := NewJsonLineDecoder(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, pushErr := decoder.Push(strings.Repeat("x", DefaultMaxFrameBytes+1) + "\n"); !errors.Is(pushErr, ErrFrameTooLarge) {
		t.Fatalf("expected the frame-too-large error, got %v", pushErr)
	}

	// And the client keeps working after the rejection.
	awaitCondition(t, "a request on the reconnected socket", func() bool {
		_, requestErr := client.Request("agent.list", map[string]any{})
		return requestErr == nil
	})
}

func TestDefaultSocketPathUsesHerdsmanHome(t *testing.T) {
	t.Setenv("HERDSMAN_HOME", "/tmp/herdsman-home")
	if got := DefaultSocketPath(); got != "/tmp/herdsman-home/herdsman.sock" {
		t.Fatalf("got %q", got)
	}
	// A trailing slash is trimmed, exactly like the TS `replace(/\/$/, "")`.
	t.Setenv("HERDSMAN_HOME", "/tmp/herdsman-home/")
	if got := DefaultSocketPath(); got != "/tmp/herdsman-home/herdsman.sock" {
		t.Fatalf("trailing slash: got %q", got)
	}
}

func TestFormatHiddenAgentUpdatesSanitizesAndKeepsStructure(t *testing.T) {
	rawText := "secret " + "sk-" + "\u200bant-api03-" + "sample-key-12345678\u202e-injection"
	first := wakeEvent(1, "agent.done", map[string]any{"agent": "codex", "name": "worker"},
		&eventOptions{paneID: stringPtr("wB:p1"), text: &rawText})
	second := wakeEvent(2, "agent.blocked", map[string]any{"agent": "claude"},
		&eventOptions{paneID: stringPtr("wB:p2")})
	second.CompactHistory = &CompactAgentHistory{}

	updates := FormatHiddenAgentUpdates([]AgentEventWireRecord{first, second})
	lines := strings.Split(updates, "\n")
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines, got %d:\n%s", len(lines), updates)
	}
	if lines[0] != "[HERDSMAN AGENT UPDATES]" {
		t.Fatalf("line 0: %q", lines[0])
	}
	if lines[1] != "- agent.done worker · Codex wB:p1" {
		t.Fatalf("line 1: %q", lines[1])
	}
	if lines[2] != "  last assistant: secret sk-[REDACTED]" {
		t.Fatalf("line 2: %q", lines[2])
	}
	if lines[3] != "  event: 1" {
		t.Fatalf("line 3: %q", lines[3])
	}
	if lines[4] != "- agent.blocked Claude wB:p2" {
		t.Fatalf("line 4: %q", lines[4])
	}
	if lines[5] != "  last assistant: " {
		t.Fatalf("line 5: %q", lines[5])
	}
	if lines[6] != "  event: 2" {
		t.Fatalf("line 6: %q", lines[6])
	}
	for _, leaked := range []string{"sk-" + "\u200bant-api03", "\u202e", "sample-key"} {
		if strings.Contains(updates, leaked) {
			t.Fatalf("leaked %q", leaked)
		}
	}
}

func TestExtensionDoesNotConnectOutsideCompleteHerdrEnvironment(t *testing.T) {
	created := 0
	f := newFixture(t, ExtensionOptions{
		ClientFactory: func() HerdsmanDaemonClient {
			created++
			return newFakeClient()
		},
	}, true, nil) // no Herdr identity
	f.core.HandleSessionStart(f.ctx)
	if created != 0 {
		t.Fatalf("client created outside a Herdr environment (%d)", created)
	}
	if status := f.ctx.Status("herdsman"); status != nil {
		t.Fatalf("unexpected status %q", *status)
	}
	f.core.handleCommand("", f.ctx)
	last := f.ctx.LastNotification()
	if last == nil || last.message != HerdrRequiredMsg || last.level != NotifyError {
		t.Fatalf("unexpected notification: %+v", last)
	}

	// A Herdr environment without a pane id is incomplete too.
	incomplete := 0
	baseEnv := envFunc(&herdsmanEnv{paneID: "wB:p1", workspaceID: "wB"})
	partialEnv := func(key string) (string, bool) {
		if key == "HERDR_PANE_ID" {
			// The TS deletes only HERDR_PANE_ID and keeps the rest of the Herdr
			// environment, so the launch identity is still incomplete.
			return "", false
		}
		return baseEnv(key)
	}
	partial := newFixture(t, ExtensionOptions{
		ClientFactory: func() HerdsmanDaemonClient {
			incomplete++
			return newFakeClient()
		},
		Env: partialEnv,
	}, true, nil)
	partial.core.HandleSessionStart(partial.ctx)
	if incomplete != 0 {
		t.Fatalf("client created without a pane id (%d)", incomplete)
	}
}

// TestExtensionSessionStartSurvivesSynchronousDialCallback is the
// regression guard for the deadlock that hung the extension under the real
// host: the Go daemon client dials synchronously inside ResetForSession, so
// the onConnected callback runs on the goroutine that called
// HandleSessionStart. That callback takes e.mu, so the dial must not be issued
// while the mutex is held (Go mutexes are not reentrant). The fake client with
// dialInline reproduces the real ordering exactly.
func TestExtensionSessionStartSurvivesSynchronousDialCallback(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{paneID: "wB:p1", workspaceID: "wB"})
	f.client.mu.Lock()
	f.client.dialInline = true
	f.client.mu.Unlock()

	// A deadlock would block HandleSessionStart forever, so the call runs on its
	// own goroutine and the test fails on a timeout instead of hanging.
	done := make(chan struct{})
	go func() {
		f.core.HandleSessionStart(f.ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleSessionStart deadlocked on the synchronous connect callback")
	}

	// The inline callback must still have driven the registration (it is
	// dispatched on its own goroutine, like every other registration).
	awaitRegistration(t, f.core)
	if got := len(f.client.CallsFor("agent.orchestrator.register")); got != 1 {
		t.Fatalf("expected one registration from the inline connect callback, got %d", got)
	}
}

func TestExtensionRegistersPresenceAndAdoptsDaemonLocation(t *testing.T) {
	f := newFixture(t, ExtensionOptions{}, true, &herdsmanEnv{paneID: "wB:p1", workspaceID: "wB"})
	registerResponse := connectionResponse(f.client, &connectionOptions{PaneID: "wC:p3", WorkspaceID: "wC"})
	f.client.response = func(method string, params any) any {
		switch method {
		case "agent.orchestrator.register", "agent.orchestrator.get":
			return registerResponse
		default:
			return map[string]any{"accepted": true}
		}
	}
	f.start()

	registerCalls := f.client.CallsFor("agent.orchestrator.register")
	if len(registerCalls) != 1 {
		t.Fatalf("expected one registration, got %d", len(registerCalls))
	}
	params, ok := registerCalls[0].params.(map[string]any)
	if !ok {
		t.Fatalf("registration params: %#v", registerCalls[0].params)
	}
	if params["herdrSocketPath"] != "/tmp/herdr.sock" || params["paneId"] != "wB:p1" ||
		params["subscriberId"] != "pi-session" || params["subscriberKind"] != "pi" || params["workspaceId"] != "wB" {
		t.Fatalf("unexpected registration params: %#v", params)
	}
	sessionRef, ok := params["sessionRef"].(map[string]any)
	if !ok {
		t.Fatalf("sessionRef: %#v", params["sessionRef"])
	}
	if sessionRef["agent"] != "pi" || sessionRef["kind"] != "path" ||
		sessionRef["source"] != "herdr:pi" || sessionRef["value"] != "/tmp/pi-session.jsonl" {
		t.Fatalf("unexpected sessionRef: %#v", sessionRef)
	}
	status := f.ctx.Status("herdsman")
	if status == nil || *status != "◆ Herdsman" {
		t.Fatalf("unexpected status: %v", status)
	}

	// A final assistant message ends the turn and signals completion. With no
	// session file present the write cannot be confirmed, yet the signal still
	// goes out with confirmed=false.
	callsBefore := len(f.client.Calls())
	f.core.HandleMessageEnd(assistantEnd("stop", nil))
	f.waitForCompletionSignal()
	turnCalls := f.client.CallsFor("agent.turn.completed")
	if len(turnCalls) != 1 {
		t.Fatalf("expected one turn-completion call, got %d (calls before=%d)", len(turnCalls), callsBefore)
	}
	turnParams, ok := turnCalls[0].params.(map[string]any)
	if !ok {
		t.Fatalf("turn params: %#v", turnCalls[0].params)
	}
	if turnParams["confirmed"] != false || turnParams["expectedText"] != "completed" ||
		turnParams["herdrSessionName"] != "default" || turnParams["paneId"] != "wC:p3" ||
		turnParams["terminalId"] != "term_pi" || turnParams["workspaceId"] != "wC" {
		t.Fatalf("unexpected turn completion params: %#v", turnParams)
	}

	// A reconnect re-registers with the daemon-adopted location.
	f.client.ConnectNow()
	awaitRegistration(t, f.core)
	registers := f.client.CallsFor("agent.orchestrator.register")
	last, ok := registers[len(registers)-1].params.(map[string]any)
	if !ok {
		t.Fatalf("last registration params: %#v", registers[len(registers)-1].params)
	}
	if last["paneId"] != "wC:p3" || last["workspaceId"] != "wC" {
		t.Fatalf("reconnect did not adopt the daemon location: %#v", last)
	}
}

// contextFilterCore is a bare extension instance: the context filter reads
// nothing but the message list, so no host, daemon or timers are needed.
func contextFilterCore() *HerdsmanExtension {
	return NewExtension(&fakePi{}, ExtensionOptions{
		Timers:     newManualTimers(),
		Env:        envFunc(nil),
		WakeFilter: &DefaultWakeFilterConfig,
	})
}

// TestContextFilterKeepsToolResultsEchoingTheMarker is the regression guard for
// the retro filter: the context hook dropped every message whose content merely
// *contained* the marker, so a tool result that read this repository's
// markers.go (or grepped for the constant, or quoted a wake report back) was
// swallowed and the model was handed a turn with a hole in it. A tool result is
// never herdsman's own injection, so it survives the filter verbatim.
func TestContextFilterKeepsToolResultsEchoingTheMarker(t *testing.T) {
	core := contextFilterCore()

	// A `read` of markers.go: the file quotes the constant's compiled value as
	// ordinary content, mid-text, inside a content block.
	readResult := map[string]any{
		"role":       "toolResult",
		"toolName":   "read",
		"toolCallId": "call_1",
		"content": []any{map[string]any{
			"type": "text",
			"text": "const agentContextMarker = \"" + agentContextMarker + "\"\n",
		}},
	}
	// A grep hit for the constant, echoed as a plain string.
	grepResult := map[string]any{
		"role":       "toolResult",
		"toolName":   "bash",
		"toolCallId": "call_2",
		"content":    "markers.go:13:const agentContextMarker = \"" + agentContextMarker + "\"",
	}
	// A tool result that *starts* with the marker block, the way a herdsman
	// message quoted back through a tool would: still a tool result, so still
	// evidence the model must see.
	wakeEcho := map[string]any{
		"role":       "toolResult",
		"toolCallId": "call_3",
		"content":    "\n  " + agentContextMarker + "\n  policy block\n",
	}

	kept := core.HandleContext([]any{readResult, grepResult, wakeEcho})
	if len(kept) != 3 {
		t.Fatalf("kept %d of 3 tool results: %#v", len(kept), kept)
	}
	for _, raw := range kept {
		message, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("kept a non-message: %#v", raw)
		}
		if role, _ := message["role"].(string); role != "toolResult" {
			t.Fatalf("kept role %q", role)
		}
	}
}

// TestContextFilterStillDropsHerdsmansOwnMessages pins the other half of the
// contract: herdsman's own entries are still removed from the incoming list,
// both by customType and by the marker prefix, while a message that only
// mentions the marker mid-text is not herdsman's and is kept.
func TestContextFilterStillDropsHerdsmansOwnMessages(t *testing.T) {
	core := contextFilterCore()

	injected := []any{
		map[string]any{"customType": herdsmanAgentContextCustomType, "role": "user", "content": agentContextMarker + "\n  policy block\n"},
		map[string]any{"customType": herdsmanWakeQueuedCustomType, "role": "user", "content": "queued wake replay"},
		map[string]any{"role": "user", "content": agentContextMarker + "\n  policy block\n"},
		// Leading whitespace is trimmed before the prefix test.
		map[string]any{"role": "user", "content": "  \n" + agentContextMarker + "\n  policy block\n"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": agentContextMarker + "\n  policy block\n"}}},
	}
	if kept := core.HandleContext(injected); len(kept) != 0 {
		t.Fatalf("kept %d herdsman messages: %#v", len(kept), kept)
	}

	// A mention in the middle of an assistant message is not herdsman's block.
	mention := map[string]any{"role": "assistant", "content": "the marker " + agentContextMarker + " is split in markers.go"}
	if kept := core.HandleContext([]any{mention}); len(kept) != 1 {
		t.Fatalf("kept %d messages, want the assistant mention: %#v", len(kept), kept)
	}
}

var _ = time.Second
