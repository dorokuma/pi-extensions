package herdsman

// Integration coverage for the extension against the real daemon client over a
// real Unix socket. The unit suites drive a fake client; these cases exist
// because the two deadlocks that hung the extension under the real host only
// appear when the client actually dials (the fake client never runs the
// extension's own callbacks on the caller's goroutine).

import (
	"net"
	"sync"
	"testing"
	"time"
)

// herdrEnvFor builds the environment closure for a complete Herdr identity that
// points at the given socket.
func herdrEnvFor(socketPath string) func(key string) (string, bool) {
	values := map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_SOCKET_PATH":  socketPath,
		"HERDR_PANE_ID":      "wB:p-test",
		"HERDR_WORKSPACE_ID": "wB",
	}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// daemonStateResponse is the `agent.orchestrator.*` reply the fake daemon
// sends: an owned scope, no pending events, watermark 0.
func daemonStateResponse() map[string]any {
	return map[string]any{
		"events": []any{},
		"presence": map[string]any{
			"connectedAt":      1,
			"herdrSessionName": "default",
			"paneId":           "wB:p-test",
			"subscriberId":     "pi-session",
			"terminalId":       "term_pi",
			"workspaceId":      "wB",
		},
		"state": map[string]any{
			"ackedEventId":     0,
			"herdrSessionName": "default",
			"owner":            map[string]any{"paneId": "wB:p-test", "terminalId": "term_pi"},
			"updatedAt":        "2026-07-10T00:00:00.000Z",
			"workspaceId":      "wB",
		},
	}
}

// serveDaemonState answers every orchestrator method with a healthy state.
func serveDaemonState(daemon *fakeDaemon) {
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": daemonStateResponse()})
	})
}

// recordingClient delegates to the real client and remembers the methods it
// was asked for, plus the ones that came back answered. A request that is
// logged but never answered is exactly the read-loop deadlock, so the tests
// wait on the answered list.
type recordingClient struct {
	HerdsmanDaemonClient
	fixture *fixture

	mu       sync.Mutex
	answered []string
}

func (c *recordingClient) Request(method string, params any) (any, error) {
	c.fixture.client.record(method, params)
	result, err := c.HerdsmanDaemonClient.Request(method, params)
	c.mu.Lock()
	c.answered = append(c.answered, method)
	c.mu.Unlock()
	return result, err
}

func (c *recordingClient) answeredCount(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, entry := range c.answered {
		if entry == method {
			count++
		}
	}
	return count
}

// awaitAnswered waits until a request for the method came back answered.
func awaitAnswered(t *testing.T, f *fixture, method string) {
	t.Helper()
	recorder, ok := f.recorder()
	if !ok {
		t.Fatal("the fixture has no recording client")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if recorder.answeredCount(method) > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s was never answered (issued: %v)", method, f.client.Methods())
}

// newRealClientFixture wires the extension to a real client talking to a fake
// daemon on a real socket.
func newRealClientFixture(t *testing.T) (*fixture, *fakeDaemon) {
	t.Helper()
	daemon := newFakeDaemon(t)
	serveDaemonState(daemon)
	var fixture *fixture
	fixture = newFixture(t, ExtensionOptions{
		Env: herdrEnvFor(daemon.socket),
		ClientFactory: func() HerdsmanDaemonClient {
			return &recordingClient{
				HerdsmanDaemonClient: NewReconnectingDaemonClient(daemon.socket, nil),
				fixture:              fixture,
			}
		},
	}, true, nil)
	return fixture, daemon
}

// recorder digs the recording client out of the core's client field.
func (f *fixture) recorder() (*recordingClient, bool) {
	f.core.mu.Lock()
	client := f.core.client
	f.core.mu.Unlock()
	recorder, ok := client.(*recordingClient)
	return recorder, ok
}

// awaitMethod waits until the fixture's client log shows the method. The real
// client dials and answers on its own goroutines, so a call issued by a handler
// is only visible a little later.
func awaitMethod(t *testing.T, f *fixture, method string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.client.CallsFor(method)) > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the client never issued %s (issued: %v)", method, f.client.Methods())
}

// countMethod is the current number of logged calls for a method.
func countMethod(f *fixture, method string) int {
	return len(f.client.CallsFor(method))
}

// runWithWatchdog runs fn and fails the test instead of hanging when it
// deadlocks.
func runWithWatchdog(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s deadlocked", what)
	}
}

// TestExtensionSessionStartWithRealClientConnectsAndRegisters drives
// session_start against a real socket. The client dials synchronously inside
// ResetForSession, so the onConnected callback runs on the goroutine that
// called HandleSessionStart -- holding e.mu across that call self-deadlocks.
func TestExtensionSessionStartWithRealClientConnectsAndRegisters(t *testing.T) {
	f, _ := newRealClientFixture(t)
	runWithWatchdog(t, "HandleSessionStart", func() {
		f.core.HandleSessionStart(f.ctx, nil)
	})
	awaitMethod(t, f, "agent.orchestrator.register")
	awaitAnswered(t, f, "agent.orchestrator.register")

	registers := f.client.CallsFor("agent.orchestrator.register")
	if len(registers) != 1 {
		t.Fatalf("expected one registration, got %d", len(registers))
	}
	f.core.mu.Lock()
	connected := f.core.connected
	f.core.mu.Unlock()
	if !connected {
		t.Fatal("the extension did not record the connection")
	}
}

// TestExtensionAgentStartAfterRealConnect keeps the event path alive after the
// connection is up: agent_start must not wait behind the session-start work.
func TestExtensionAgentStartAfterRealConnect(t *testing.T) {
	f, _ := newRealClientFixture(t)
	runWithWatchdog(t, "HandleSessionStart", func() {
		f.core.HandleSessionStart(f.ctx, nil)
	})
	awaitMethod(t, f, "agent.orchestrator.register")
	runWithWatchdog(t, "HandleAgentStart", func() {
		f.core.HandleAgentStart()
	})
	runWithWatchdog(t, "HandleAgentSettled", func() {
		f.ctx.SetIdle(true)
		f.core.HandleAgentSettled(f.ctx)
	})
}

// TestExtensionHandlesOrchestratorStreamWithRealClient covers the notification
// path: the daemon pushes an orchestrator change that hands us the role, and
// the handler that runs on it asks the daemon for fresh state. That request
// must not wait behind the read loop that has to deliver its response.
func TestExtensionHandlesOrchestratorStreamWithRealClient(t *testing.T) {
	f, daemon := newRealClientFixture(t)

	// Another pane owns the role to begin with.
	owner := map[string]any{"paneId": "wB:p-other", "terminalId": "term_agent"}
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		state := daemonStateResponse()
		state["state"].(map[string]any)["owner"] = owner
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": state})
	})

	runWithWatchdog(t, "HandleSessionStart", func() {
		f.core.HandleSessionStart(f.ctx, nil)
	})
	awaitMethod(t, f, "agent.orchestrator.register")
	// The registration has to be applied before the role change is pushed: e.mu
	// no longer spans the register round trip, so a notification that lands
	// mid-registration still has no scope and is dropped -- exactly what the TS
	// does while `state.currentScope` is undefined.
	awaitRegistration(t, f.core)
	awaitAnswered(t, f, "agent.orchestrator.register")

	getsBefore := countMethod(f, "agent.orchestrator.get")
	_ = getsBefore
	daemon.WriteJSON(mustConn(t, daemon), map[string]any{
		"method": "agent.orchestrator.changed",
		"params": map[string]any{
			"change": map[string]any{
				"current": map[string]any{
					"herdrSessionName": "default",
					"owner":            map[string]any{"paneId": "wB:p-test", "terminalId": "term_pi"},
					"workspaceId":      "wB",
				},
				"previous": map[string]any{
					"herdrSessionName": "default",
					"owner":            owner,
					"workspaceId":      "wB",
				},
			},
			"herdrSessionName": "default",
			"workspaceId":      "wB",
		},
	})

	// Gaining the role refreshes the state. The request has to come back
	// answered: a notification that blocked the read loop would leave it
	// issued but unanswered until its timeout.
	awaitAnswered(t, f, "agent.orchestrator.get")
}

// mustConn returns the single connection the fake daemon accepted.
func mustConn(t *testing.T, daemon *fakeDaemon) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.Connections() >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if daemon.Connections() == 0 {
		t.Fatal("the fake daemon never accepted a connection")
	}
	// The daemon's serve loop owns its connections; the test only needs one to
	// push a notification, and the last accepted one is the live client.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.live) == 0 {
		t.Fatal("no live connection to push on")
	}
	return daemon.live[len(daemon.live)-1]
}
