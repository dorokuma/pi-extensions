package herdsmanpi

// Regression suite for the reviewer's must-fix list. Every case is written to
// fail against the implementation as it was reviewed: either the asserted
// behaviour is wrong, or the old code parks (the lock/RPC cases therefore carry
// a short watchdog instead of waiting for Go's 10-minute deadlock detector).
//
// Nothing here touches a production socket: the daemon-client cases bind a fake
// daemon on a socket under t.TempDir() (newFakeDaemon), the extension cases run
// on a client that only blocks until the test releases it, and all of them run
// on the manual clock of the harness.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// watchdog is how long a regression case waits for something that used to hang
// forever. It has to stay far below the runtime deadlock detector so that
// `go test -run TestRegression_` on the old implementation fails fast.
const watchdog = time.Second

// ── blocking daemon client ──────────────────────────────────────────────────

// blockingClient is a daemon client whose requests stop at the door until the
// test releases them. That is what makes the lock discipline observable: while
// a request is in flight, a test can ask whether the core still holds e.mu.
type blockingClient struct {
	mu              sync.Mutex
	issuedMethods   []string
	release         chan struct{}
	armed           bool
	failMethod      map[string]error
	onEnter         func(method string)
	response        func(method string, params any) any
	onConnected     func() error
	onDisconnected  func(error)
	onStreamMessage func(DaemonStreamMessage)
}

func newBlockingClient(response func(method string, params any) any) *blockingClient {
	return &blockingClient{release: make(chan struct{}), response: response}
}

// arm makes the next requests stop at the door until releaseAll.
func (c *blockingClient) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	c.release = make(chan struct{})
}

// ownerResponse is the `agent.orchestrator.*` reply of a workspace this Pi owns.
func ownerResponse() map[string]any {
	return connectionResponse(nil, &connectionOptions{})
}

// nonOwnerResponse is the same reply with another terminal holding the role.
func nonOwnerResponse() map[string]any {
	other := "term_agent"
	return connectionResponse(nil, &connectionOptions{OwnerTerminal: &other, HasOwner: true})
}

// failOn makes `method` answer with err once its turn comes (nil clears it),
// so a test can drive the failure branches of a request it released.
func (c *blockingClient) failOn(method string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failMethod == nil {
		c.failMethod = map[string]error{}
	}
	if err == nil {
		delete(c.failMethod, method)
		return
	}
	c.failMethod[method] = err
}

func (c *blockingClient) Request(method string, params any) (any, error) {
	c.mu.Lock()
	c.issuedMethods = append(c.issuedMethods, method)
	armed := c.armed
	release := c.release
	enter := c.onEnter
	response := c.response
	c.mu.Unlock()
	if enter != nil {
		enter(method)
	}
	if armed {
		<-release
	}
	// The failure is read after the wait at the door: a test arms a request,
	// changes the world while it stands there, and only then decides how the
	// request answers.
	c.mu.Lock()
	failure, hasFailure := c.failMethod[method]
	c.mu.Unlock()
	if hasFailure {
		return nil, failure
	}
	if response != nil {
		return response(method, params), nil
	}
	return map[string]any{}, nil
}

func (c *blockingClient) Close() {}

// ResetForSession fires the connect callback inline, the way the real client's
// synchronous dial does.
func (c *blockingClient) ResetForSession() {
	c.mu.Lock()
	handler := c.onConnected
	c.mu.Unlock()
	if handler != nil {
		_ = handler()
	}
}

func (c *blockingClient) OnConnected() func() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onConnected
}
func (c *blockingClient) SetOnConnected(handler func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onConnected = handler
}
func (c *blockingClient) OnDisconnected() func(error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onDisconnected
}
func (c *blockingClient) SetOnDisconnected(handler func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDisconnected = handler
}
func (c *blockingClient) OnStreamMessage() func(DaemonStreamMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onStreamMessage
}
func (c *blockingClient) SetOnStreamMessage(handler func(DaemonStreamMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onStreamMessage = handler
}

func (c *blockingClient) setOnEnter(enter func(method string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEnter = enter
}

func (c *blockingClient) issued(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, entry := range c.issuedMethods {
		if entry == method {
			count++
		}
	}
	return count
}

// releaseAll lets every in flight request answer and disarms the client, so the
// rest of the scenario runs without standing at the door.
func (c *blockingClient) releaseAll() {
	c.mu.Lock()
	c.armed = false
	release := c.release
	c.mu.Unlock()
	select {
	case <-release:
	default:
		close(release)
	}
}

func newBlockingFixture(t *testing.T, client *blockingClient) *fixture {
	t.Helper()
	return newFixture(t, ExtensionOptions{
		ClientFactory: func() HerdsmanDaemonClient { return client },
	}, true, &herdsmanEnv{})
}

// startBlocking brings the core up against a blocking client. The registration
// is released immediately, so the core ends up connected with the scope the
// response describes.
func startBlocking(t *testing.T, f *fixture, client *blockingClient) {
	t.Helper()
	client.arm()
	go func() { f.core.HandleSessionStart(f.ctx, nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client.issued("agent.orchestrator.register") > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	client.releaseAll()
	awaitRegistration(t, f.core)
}

// assertCoreLockFree fails when the core mutex stays held: every daemon request
// must run with e.mu released, so anything waiting for it has to be granted.
func assertCoreLockFree(t *testing.T, core *HerdsmanExtension, what string) {
	t.Helper()
	granted := make(chan struct{})
	go func() {
		core.mu.Lock()
		core.mu.Unlock()
		close(granted)
	}()
	select {
	case <-granted:
	case <-time.After(watchdog):
		t.Fatalf("%s holds the extension lock across the daemon request", what)
	}
}

// holdAndRelease runs an entry point that must issue `method`, proves the core
// lock is free while that request is in flight, then releases the request and
// waits for the entry point to finish (so a green run also proves the request
// still ran, not that it was skipped).
func holdAndRelease(
	t *testing.T,
	core *HerdsmanExtension,
	client *blockingClient,
	method string,
	what string,
	call func(),
) {
	t.Helper()
	entered := make(chan string, 8)
	client.setOnEnter(func(issued string) {
		select {
		case entered <- issued:
		default:
		}
	})
	client.arm()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		call()
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		client.releaseAll()
		t.Fatalf("%s never issued %s", what, method)
	}
	assertCoreLockFree(t, core, what)
	client.releaseAll()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return after its request was released", what)
	}
}

func coreConnected(core *HerdsmanExtension) bool {
	core.mu.Lock()
	defer core.mu.Unlock()
	return core.connected
}

// waitForIssued waits until the client has issued `want` requests of method. It
// is how a case parks a request at the door without racing the goroutine that
// issues it: the count is appended under the client's lock before the request
// waits for its release.
func waitForIssued(t *testing.T, client *blockingClient, method string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && client.issued(method) < want {
		time.Sleep(2 * time.Millisecond)
	}
	if got := client.issued(method); got < want {
		t.Fatalf("%s was issued %d time(s), want at least %d", method, got, want)
	}
}

// ── Fix 1: no blocking request under the core lock ──────────────────────────

func TestRegression_CoreNeverHoldsItsLockDuringADaemonRequest(t *testing.T) {
	// Registration: the presence register is the longest request the extension
	// issues, and its error branch used to return with the mutex still held.
	t.Run("presence registration", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any { return ownerResponse() })
		f := newBlockingFixture(t, client)
		holdAndRelease(t, f.core, client, "agent.orchestrator.register", "registerPresence", func() {
			f.core.HandleSessionStart(f.ctx, nil)
		})
		awaitRegistration(t, f.core)
		if !coreConnected(f.core) {
			t.Fatal("the registration response was not applied")
		}
	})

	// Same call, failing: the register request answers with a real error, so the
	// failure branch of finishRegistration actually runs. While the request is in
	// flight the lock has to stay free, and the failed registration has to leave
	// registrationInFlight cleared, so a later connect can register again.
	//
	// The failure stays armed until the request is released: clearing it before
	// the release would let blockingClient read it after the wait at the door had
	// already passed, and the request would answer successfully instead — the
	// green this case used to report while the failure branch never ran.
	t.Run("failed registration releases the lock", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any { return ownerResponse() })
		f := newBlockingFixture(t, client)
		client.arm()
		client.failOn("agent.orchestrator.register", errors.New("register failed"))
		go func() { f.core.HandleSessionStart(f.ctx, nil) }()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && client.issued("agent.orchestrator.register") == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if client.issued("agent.orchestrator.register") == 0 {
			client.releaseAll()
			t.Fatal("registerPresence never issued the registration")
		}
		assertCoreLockFree(t, f.core, "failed registerPresence")
		client.releaseAll()
		awaitRegistration(t, f.core)
		assertCoreLockFree(t, f.core, "the finished failed registration")
		if coreConnected(f.core) {
			t.Fatal("expected coreConnected to be false after failed registration")
		}
		f.core.mu.Lock()
		inFlight := f.core.registrationInFlight
		f.core.mu.Unlock()
		if inFlight != nil {
			t.Fatal("registrationInFlight stayed set after a failed registration")
		}
		// A later connect must be able to run a fresh registration.
		client.failOn("agent.orchestrator.register", nil)
		client.ResetForSession()
		awaitRegistration(t, f.core)
		if !coreConnected(f.core) {
			t.Fatal("the extension did not re-register after a failed registration")
		}
		if got := client.issued("agent.orchestrator.register"); got != 2 {
			t.Fatalf("the re-registration never reached the daemon: %d register call(s)", got)
		}
	})

	t.Run("status command", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any { return ownerResponse() })
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)
		holdAndRelease(t, f.core, client, "agent.orchestrator.get", "handleCommand(status)", func() {
			f.core.handleCommand("status", f.ctx)
		})
		note := f.ctx.LastNotification()
		if note == nil || !strings.Contains(note.message, "Herdsman is watching") {
			t.Fatalf("the status reply was not reported: %+v", note)
		}
	})

	t.Run("role command", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any { return ownerResponse() })
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)
		holdAndRelease(t, f.core, client, "agent.orchestrator.set", "handleCommand(off)", func() {
			f.core.handleCommand("off", f.ctx)
		})
		if client.issued("agent.orchestrator.set") != 1 {
			t.Fatalf("the role command did not reach the daemon: %d", client.issued("agent.orchestrator.set"))
		}
	})

	// A role change arriving on the notification path used to re-read the state
	// while the notification handler held the lock.
	t.Run("role gain refresh", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any {
			if method == "agent.orchestrator.get" {
				return connectionResponse(nil, &connectionOptions{
					Events: []AgentEventWireRecord{
						wakeEvent(61, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")}),
					},
					HasEvents: true,
				})
			}
			return nonOwnerResponse()
		})
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)
		if len(f.core.pendingSnapshot()) != 0 {
			t.Fatalf("the non-owner queued updates: %+v", f.core.pendingSnapshot())
		}
		other := stringPtr("term_agent")
		mine := stringPtr("term_pi")
		holdAndRelease(t, f.core, client, "agent.orchestrator.get", "refreshAfterRoleGain", func() {
			// The entry point the real client's dispatcher calls on a pushed role
			// change; the fake client of this fixture is not the client the core
			// got, so the message goes in directly.
			f.core.handleStreamMessage(roleChangeStream(other, mine, "wB:p1"))
		})
		// The role-change path only fires for the live stream handler.
		if client.issued("agent.orchestrator.get") == 0 {
			t.Fatal("gaining the role did not re-read the daemon state")
		}
		if len(f.core.pendingSnapshot()) != 1 {
			t.Fatalf("the refreshed pending page was not applied: %+v", f.core.pendingSnapshot())
		}
	})

	t.Run("wake pending read", func(t *testing.T) {
		event := wakeEvent(62, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")})
		client := newBlockingClient(func(method string, params any) any {
			return connectionResponse(nil, &connectionOptions{Events: []AgentEventWireRecord{event}, HasEvents: true})
		})
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)
		holdAndRelease(t, f.core, client, "agent.orchestrator.get", "startWake", func() {
			f.timers.Advance(10 * time.Millisecond)
		})
		if len(f.pi.WakeInjections()) != 1 {
			t.Fatalf("the wake did not inject after its read completed: %d", len(f.pi.WakeInjections()))
		}
	})

	t.Run("settlement acknowledgement", func(t *testing.T) {
		event := wakeEvent(63, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")})
		client := newBlockingClient(func(method string, params any) any {
			return connectionResponse(nil, &connectionOptions{Events: []AgentEventWireRecord{event}, HasEvents: true})
		})
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)
		f.timers.Advance(10 * time.Millisecond)
		if len(f.pi.WakeInjections()) != 1 {
			t.Fatalf("the wake did not inject: %d", len(f.pi.WakeInjections()))
		}
		f.core.HandleAgentStart()
		f.core.HandleMessageEnd(wakeConsumptionEvidence([]int{63}))
		holdAndRelease(t, f.core, client, "agent.notifications.ack", "HandleAgentSettled", func() {
			f.core.HandleAgentSettled(f.ctx)
		})
		if client.issued("agent.notifications.ack") != 1 {
			t.Fatalf("the settled batch was not acknowledged: %d", client.issued("agent.notifications.ack"))
		}
		if pending := f.core.pendingSnapshot(); len(pending) != 0 {
			t.Fatalf("the acknowledged event stayed pending: %+v", pending)
		}
	})

	// A suppressed upstream error is acknowledged silently from a wake timer.
	t.Run("silent upstream error acknowledgement", func(t *testing.T) {
		text := "API Error: 429 rate_limit_error"
		event := wakeEvent(64, "agent.done", map[string]any{}, &eventOptions{
			terminal: stringPtr("term_agent"),
			text:     &text,
		})
		client := newBlockingClient(func(method string, params any) any {
			return connectionResponse(nil, &connectionOptions{Events: []AgentEventWireRecord{event}, HasEvents: true})
		})
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)
		if len(f.pi.WakeInjections()) != 0 {
			t.Fatal("an upstream error was injected instead of suppressed")
		}
		holdAndRelease(t, f.core, client, "agent.notifications.ack", "scheduleSilentUpstreamErrorAck", func() {
			f.timers.Advance(10 * time.Millisecond)
		})
		if client.issued("agent.notifications.ack") != 1 {
			t.Fatalf("the suppressed error was not acknowledged: %d", client.issued("agent.notifications.ack"))
		}
	})
}

// ── Fix 2: a skipped wake injection reports the id as presented ─────────────

func TestRegression_SkippedWakeInjectionReportsPresented(t *testing.T) {
	core := NewExtension(&fakePi{}, ExtensionOptions{
		Timers:     newManualTimers(),
		Env:        envFunc(nil),
		WakeFilter: &DefaultWakeFilterConfig,
	})
	// The helper is reached exactly for the ids that were already presented, are
	// still awaiting consumption, or are suppressed; the TS returns true there
	// and alreadyPresented forwards that verdict.
	if !core.noteSkippedWakeInjection(1, "presented") {
		t.Fatal("noteSkippedWakeInjection reported a presented id as injectable")
	}
	core.mu.Lock()
	core.wakeAwaitingConsumption[2] = true
	core.presentedEventIDs[3] = true
	core.wakeSuppressedEventIDs[4] = true
	awaiting := core.alreadyPresented(2)
	presented := core.alreadyPresented(3)
	suppressed := core.alreadyPresented(4)
	unknown := core.alreadyPresented(5)
	core.mu.Unlock()
	if !awaiting || !presented || !suppressed {
		t.Fatalf("alreadyPresented: awaiting=%t presented=%t suppressed=%t", awaiting, presented, suppressed)
	}
	if unknown {
		t.Fatal("an unseen id was reported as presented")
	}
}

// ── a minimal Pi host on the SDK wire ───────────────────────────────────────

// maxTestFrameBytes is sdk.MaxFrameSize, repeated here because the test host
// reads frames on its own.
const maxTestFrameBytes = 128 * 1024 * 1024

// sdkTestHost is the smallest host that can drive the package entry point. It
// speaks the wire of go/pigsdk/protocol.go over net.Pipe -- a 4-byte big-endian
// length prefix in front of every JSON frame -- finishes the register/ready
// handshake, records the method of every `call` the extension makes, and
// answers each one (`idle: true`, so an `isIdle` probe has an answer to carry
// back), and counts the `response` of every `request` it sends.
//
// Both SDK-adapter regressions below are driven through it instead of through
// the adapter's own symbols, so the same case runs unchanged against an
// implementation that names those symbols differently.
type sdkTestHost struct {
	t        *testing.T
	conn     net.Conn
	handlers map[string]int
	done     chan error

	writeMu sync.Mutex
	next    int

	mu       sync.Mutex
	calls    []string
	answered int
}

// startSDKTestHost serves ext over one end of an in-memory pipe and handshakes
// with it the way Pi's loader does.
func startSDKTestHost(t *testing.T, ext *sdk.Extension) *sdkTestHost {
	t.Helper()
	hostConn, extConn := net.Pipe()
	host := &sdkTestHost{t: t, conn: hostConn, handlers: map[string]int{}, done: make(chan error, 1)}
	go func() { host.done <- ext.RunWithConn(extConn) }()

	first, err := host.read()
	if err != nil {
		t.Fatalf("the extension never registered: %v", err)
	}
	if first["type"] != "register" {
		t.Fatalf("the first frame was %v, want register", first["type"])
	}
	register, _ := first["register"].(map[string]any)
	subscriptions, _ := register["handlers"].([]any)
	for _, item := range subscriptions {
		entry, _ := item.(map[string]any)
		event, _ := entry["event"].(string)
		id, _ := entry["handler_id"].(float64)
		if event != "" {
			host.handlers[event] = int(id)
		}
	}
	_ = host.send(map[string]any{"type": "ready", "ready": map[string]any{
		"cwd": t.TempDir(), "mode": "interactive", "width": 80, "height": 24,
	}})
	go host.serve()
	t.Cleanup(host.stop)
	return host
}

// read reads one length-prefixed frame.
func (h *sdkTestHost) read() (map[string]any, error) {
	var header [4]byte
	if _, err := io.ReadFull(h.conn, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxTestFrameBytes {
		return nil, fmt.Errorf("frame too large: %d bytes", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(h.conn, body); err != nil {
		return nil, err
	}
	var frame map[string]any
	if err := json.Unmarshal(body, &frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// send writes one length-prefixed frame.
func (h *sdkTestHost) send(frame map[string]any) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	packet := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(packet, uint32(len(body)))
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err = h.conn.Write(append(packet, body...))
	return err
}

// serve answers every frame the extension sends: a `call` gets the one result
// object both agent-control probes read, a `response` is counted.
func (h *sdkTestHost) serve() {
	for {
		frame, err := h.read()
		if err != nil {
			return
		}
		switch frame["type"] {
		case "call":
			call, _ := frame["call"].(map[string]any)
			method, _ := call["method"].(string)
			h.mu.Lock()
			h.calls = append(h.calls, method)
			h.mu.Unlock()
			_ = h.send(map[string]any{
				"type": "call_result",
				"id":   frame["id"],
				"call_result": map[string]any{"result": map[string]any{
					"idle": true, "sessionId": "regression-session",
				}},
			})
		case "response":
			h.mu.Lock()
			h.answered++
			h.mu.Unlock()
		}
	}
}

// event fires one registered event the way an emit does, and returns as soon as
// the request frame is on the wire; the answer is read by serve.
func (h *sdkTestHost) event(name string) {
	h.t.Helper()
	id, ok := h.handlers[name]
	if !ok {
		h.t.Fatalf("the extension registered no handler for %s", name)
	}
	h.request(map[string]any{"method": "event", "event": name, "handler_id": id, "args": map[string]any{}})
}

// command fires the registered `/herdsman` command. Its args travel as a JSON
// string, the way the host sends `pi.registerCommand` arguments.
func (h *sdkTestHost) command(args string) {
	h.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		h.t.Fatalf("encode the command arguments: %v", err)
	}
	h.request(map[string]any{"method": "command", "tool": "herdsman", "args": json.RawMessage(raw)})
}

// request sends one host request under a fresh id.
func (h *sdkTestHost) request(payload map[string]any) {
	h.t.Helper()
	h.mu.Lock()
	h.next++
	id := fmt.Sprintf("r%d", h.next)
	h.mu.Unlock()
	if err := h.send(map[string]any{"type": "request", "id": id, "request": payload}); err != nil {
		h.t.Fatalf("send the %v request: %v", payload["method"], err)
	}
}

// callsFor counts the host calls of one method.
func (h *sdkTestHost) callsFor(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, entry := range h.calls {
		if entry == method {
			count++
		}
	}
	return count
}

// methodList is every host call so far, for failure messages.
func (h *sdkTestHost) methodList() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, ",")
}

// waitsUntil polls predicate until it holds or the deadline passes.
func (h *sdkTestHost) waitsUntil(predicate func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return predicate()
}

// waitsForCall waits until the extension has made `want` host calls of method.
func (h *sdkTestHost) waitsForCall(method string, want int) bool {
	return h.waitsUntil(func() bool { return h.callsFor(method) >= want })
}

// stop shuts the serve loop down and reports how it ended.
func (h *sdkTestHost) stop() {
	_ = h.send(map[string]any{"type": "shutdown"})
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("the extension serve loop ended with an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = h.conn.Close()
		h.t.Errorf("the extension serve loop did not stop on shutdown")
	}
	_ = h.conn.Close()
}

// ── Fix 3: abort and idle reach the host from the adapter ───────────────────

func TestRegression_SDKContextForwardsAbortAndIsIdle(t *testing.T) {
	// The subprocess SDK does answer `abort` and `isIdle`; the adapter used to
	// swallow both (`Err()` for abort, a hardcoded "unknown" for idle), so
	// neither call ever reached Pi. The case runs end to end -- the package
	// entry point, a real `session_start` request, the Context the core ends up
	// holding and the host on the other end of the wire -- so it says nothing
	// about which symbol the adapter happens to name.
	exposed := make(chan *HerdsmanExtension, 1)
	host := startSDKTestHost(t, NewSDKExtension(ExtensionOptions{
		Env:            envFunc(nil),
		OnStateExposed: func(instance *HerdsmanExtension) { exposed <- instance },
	}))
	host.event(sdk.EventSessionStart)

	var instance *HerdsmanExtension
	select {
	case instance = <-exposed:
	case <-time.After(5 * time.Second):
		t.Fatal("the session_start request never created the extension core")
	}
	// The Context the core keeps is the one session_start delivered, and that
	// request has been answered by now; a background path (the role-lost wake,
	// a deferred wake's idle probe) still has to reach the host through it.
	if !host.waitsUntil(func() bool {
		instance.mu.Lock()
		defer instance.mu.Unlock()
		return instance.activeContext != nil
	}) {
		t.Fatal("session_start never handed the core its host context")
	}
	instance.mu.Lock()
	hosted := instance.activeContext
	instance.mu.Unlock()

	idle, known := hosted.IsIdle()
	if !known || !idle {
		t.Errorf("the core's Context reported no idle answer: idle=%t known=%t", idle, known)
	}
	if got := host.callsFor("isIdle"); got != 1 {
		t.Errorf("the idle probe reached the host %d time(s), want 1 (calls: %s)", got, host.methodList())
	}
	// An idle wake is aborted from a stream event, not from the handler whose
	// Context this is: the abort has to survive the request it was created in.
	hosted.Abort()
	if !host.waitsForCall("abort", 1) {
		t.Errorf("the abort never reached the host (calls: %s)", host.methodList())
	}
}

// ── Fix 4: the lazily created core is created once, under a mutex ───────────

func TestRegression_SDKCoreIsCreatedOnceUnderConcurrency(t *testing.T) {
	// The SDK answers host requests concurrently, so the creating paths (the
	// `session_start` emit and the `/herdsman` command) can reach the lazily
	// created core at the same time and, without the holder's mutex, both see
	// "not created yet": one of them gets a half-built pointer and the state
	// (delivery queue, dead letters) is built twice. Every request is sent
	// before any of them is answered, and the creation itself is held open from
	// the inside, so the second request arrives while the first one is still
	// missing. A core that serialises them hands back the one instance instead.
	var created int32
	host := startSDKTestHost(t, NewSDKExtension(ExtensionOptions{
		Env: envFunc(nil),
		OnStateExposed: func(*HerdsmanExtension) {
			atomic.AddInt32(&created, 1)
			time.Sleep(20 * time.Millisecond)
		},
	}))
	const racers = 4
	for index := 0; index < racers; index++ {
		if index%2 == 0 {
			host.event(sdk.EventSessionStart)
		} else {
			host.command("status")
		}
	}
	// Every request has to come back: a core that is created once but never
	// handed to the waiting callbacks would hang just as hard.
	answered := 0
	if !host.waitsUntil(func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		answered = host.answered
		return host.answered >= racers
	}) {
		t.Fatalf("only %d of the %d concurrent requests were answered", answered, racers)
	}
	if got := atomic.LoadInt32(&created); got != 1 {
		t.Fatalf("the core was created %d times through the entry point, exactly one is allowed", got)
	}
}

// ── Fix 5: onConnected may ask the daemon, and a late handler still fires ───

func TestRegression_OnConnectedMayRequestAndLateHandlerStillFires(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	// A client built without a callback: the caller installs it afterwards, the
	// way HandleSessionStart does.
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	awaitCondition(t, "connection", func() bool { return daemon.Connections() == 1 })

	fired := make(chan struct{}, 4)
	client.SetOnConnected(func() error {
		select {
		case fired <- struct{}{}:
		default:
		}
		return nil
	})
	// The connection predates the callback: without the missed-notification
	// bookkeeping the session would never learn it is connected.
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the connect callback installed after the first dial never fired")
	}

	// And a callback may issue a request: the read loop has to be running
	// already, or the request waits for the loop that only starts after the
	// callback returns.
	answered := make(chan error, 1)
	go func() {
		_, err := client.Request("agent.list", map[string]any{})
		answered <- err
	}()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the request failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a request issued outside the callback did not complete")
	}

	// A second connect cycle fires once per generation: re-installing the handler
	// for the connection that was already reported must not fire it again.
	reconnected := make(chan struct{}, 4)
	client.SetOnConnected(func() error {
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return nil
	})
	if _, err := client.Request("agent.ping", map[string]any{}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(reconnected) != 0 {
		t.Fatal("one connection was reported twice to the connect callback")
	}

	// And the request may be issued from inside the callback itself: connect()
	// has to start the read loop before it calls the handler, or a request made
	// from inside the callback waits for the very loop that only starts after
	// the callback returns -- which is a deadlock the 1s watchdog below turns
	// into a failure.
	inCallback := make(chan error, 1)
	client.SetOnConnected(func() error {
		_, err := client.Request("agent.list", map[string]any{})
		select {
		case inCallback <- err:
		default:
		}
		return nil
	})
	// Drop the server side of the live socket so the client reconnects: the
	// fresh connect cycle calls the handler from inside connect().
	daemon.DropLive()
	select {
	case err := <-inCallback:
		if err != nil {
			t.Fatalf("the request issued from inside onConnected failed: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("a request issued from inside onConnected never completed: the read loop was not running yet")
	}
	client.Close()
}

// ── Fix R3-3: a reset stops the pending reconnect; a late connect attempt
// is inert; a stale report is inert ────────────────────────────────────────

// TestRegression_ResetForSessionStopsTheScheduledReconnect covers the reconnect
// window from three sides.
//
// The first part is the original case: a backoff the previous session scheduled
// and never got to run must be stopped by the reset.
//
// The second part is the atomicity the reconnect timer callback used to lack.
// Its callback validated itself under the lock, released the lock and only then
// called connect(); in that window a ResetForSession could establish the new
// connection, and the late connect() then went ahead anyway — the old
// beginConnectLocked equivalent only refused a closed or incompatible client,
// so it bumped the generation, closed the socket ResetForSession had just
// connected and dialled a second one behind it. The validation and the
// reservation must happen in one critical section, so a late attempt that finds
// the client already connected (or already connecting) is inert.
func TestRegression_ResetForSessionStopsTheScheduledReconnect(t *testing.T) {
	dir := t.TempDir()
	client := &ReconnectingDaemonClient{
		socketPath:      dir + "/absent.sock",
		reconnectDelays: []int64{50},
		nextID:          1,
		pending:         map[string]*pendingRequest{},
		now:             time.Now,
		state:           stateIdle,
	}
	// A backoff the previous session scheduled and never got to run.
	previous := time.AfterFunc(5*time.Second, func() {})
	client.reconnectTimer = previous

	client.ResetForSession()

	if previous.Stop() {
		t.Fatal("ResetForSession left the previous session's reconnect armed: it will dial behind the new session")
	}
	client.mu.Lock()
	now := client.reconnectTimer
	client.mu.Unlock()
	if now == previous {
		t.Fatal("ResetForSession kept the old timer instead of scheduling its own attempt")
	}
	if now != nil {
		now.Stop()
	}

	// The window itself: the client is connected, and a connect() that arrives
	// late — a reconnect timer whose lock window the reset raced through, or the
	// constructor's queued first dial — must find it is not idle and do nothing.
	// The old connect() went ahead regardless: it bumped the generation, closed
	// the live socket and dialled a second one.
	daemon := newFakeDaemon(t)
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	live := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(live.Close)
	awaitCondition(t, "first connection", func() bool { return daemon.Connections() == 1 })
	live.mu.Lock()
	generationBefore := live.generation
	live.mu.Unlock()

	live.connect()

	if got := daemon.Connections(); got != 1 {
		t.Fatalf("a late connect() dialled a second socket behind the live connection: %d connection(s)", got)
	}
	live.mu.Lock()
	generationAfter := live.generation
	state := live.state
	live.mu.Unlock()
	if generationAfter != generationBefore || state != stateConnected {
		t.Fatalf("a late connect() replaced the live connection: generation %d -> %d state=%v",
			generationBefore, generationAfter, state)
	}
	if _, err := live.Request("agent.list", map[string]any{}); err != nil {
		t.Fatalf("the live connection stopped serving after the late connect(): %v", err)
	}
}

func TestRegression_StaleDisconnectKeepsTheLiveConnection(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	awaitCondition(t, "first connection", func() bool { return daemon.Connections() == 1 })
	client.mu.Lock()
	live := client.generation
	client.mu.Unlock()

	// A reader of an already superseded connection reports its failure late.
	client.handleDisconnect(ErrSocketClosed, live-1)
	if _, err := client.Request("agent.list", map[string]any{}); err != nil {
		t.Fatalf("the live connection stopped serving: %v", err)
	}
	client.mu.Lock()
	generation := client.generation
	state := client.state
	timer := client.reconnectTimer
	client.mu.Unlock()
	if generation != live || state != stateConnected {
		t.Fatalf("a stale disconnect reset the client: generation=%d state=%v", generation, state)
	}
	if timer != nil {
		t.Fatal("a stale disconnect scheduled a reconnect behind the live socket")
	}
}

// ── Fix 7: only the live projection holds the ack cursor open ───────────────

func TestRegression_ConfirmablePrefixIgnoresTheQueuedAttemptCount(t *testing.T) {
	core := NewExtension(&fakePi{}, ExtensionOptions{
		Timers:     newManualTimers(),
		Env:        envFunc(nil),
		WakeFilter: &DefaultWakeFilterConfig,
	})
	event := wakeEvent(9, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")})
	queued := event
	// The delivery queue keeps the snapshot of the first delivery: its attempts
	// counter is already 1. Reading it made a stranded queued event permanently
	// unconfirmable, so agent_settled could never recover it.
	queued.Attempts = intPtr(1)
	core.mu.Lock()
	core.unackedDelivered[event.ID] = queued
	core.wakeConsumptionObserved[event.ID] = true
	core.pendingEvents = []AgentEventWireRecord{event}
	prefix := core.confirmableDeliveryPrefix(false)
	core.mu.Unlock()
	if len(prefix.ConfirmablePrefix) != 1 || prefix.ConfirmablePrefix[0].ID != event.ID {
		t.Fatalf("a queued delivery whose live projection never failed was not confirmable: %+v", prefix)
	}

	// The live counter still blocks it: an event the daemon is still retrying
	// must not be swept past.
	failed := event
	failed.Attempts = intPtr(1)
	core.mu.Lock()
	core.pendingEvents = []AgentEventWireRecord{failed}
	blocked := core.confirmableDeliveryPrefix(false)
	core.mu.Unlock()
	if len(blocked.ConfirmablePrefix) != 0 {
		t.Fatalf("an event with a live failed attempt was acknowledged: %+v", blocked.ConfirmablePrefix)
	}
}

// ── Fix 8: JS flag validation and sticky semantics ─────────────────────────

func TestRegression_ExtraPatternFlagsAreValidatedAndStickyIsAnchored(t *testing.T) {
	// A JS `/pattern/flags` literal rejects unknown and repeated flags, so an
	// operator typo must warn and never match instead of quietly matching. (The
	// `/…/flags` splitter only ever hands over the six known characters, so what
	// reaches the compiler is a repeated flag; an unknown one stays in the
	// pattern body, exactly as in the TS.)
	if _, err := compileJSRegexp(`foo`, `q`); err == nil {
		t.Fatal("an unsupported flag compiled without an error")
	}
	if _, err := compileJSRegexp(`foo`, `iix`); err == nil {
		t.Fatal("an unsupported flag mixed with known ones compiled without an error")
	}
	if _, err := compileJSRegexp(`foo`, `ii`); err == nil {
		t.Fatal("a repeated flag compiled without an error")
	}
	if _, err := compileJSRegexp(`foo`, `gimsuy`); err != nil {
		t.Fatalf("the six JS flags were rejected: %v", err)
	}
	if extraPatternWarning([]string{"/foo/ii"}) == nil {
		t.Fatal("a repeated flag was not reported by extraPatternWarning")
	}
	if extraPatternWarning([]string{"/foo/i", "/bar/y"}) != nil {
		t.Fatal("valid patterns were reported as invalid")
	}
	if matchesExtraPattern("foobar", "/foo/ii") {
		t.Fatal("a pattern with a repeated flag matched")
	}

	// Sticky means "match at lastIndex", and every caller here uses 0, so the
	// match must be anchored at the start of the text.
	if !matchesExtraPattern("foobar", "/foo/y") {
		t.Fatal("a sticky pattern stopped matching at the start")
	}
	if matchesExtraPattern("barfoo", "/foo/y") {
		t.Fatal("a sticky pattern matched away from the start of the text")
	}
	if !matchesExtraPattern("FOObar", "/foo/iy") {
		t.Fatal("sticky dropped the other flags")
	}
	if matchesExtraPattern("barFOO", "/foo/iy") {
		t.Fatal("sticky stopped anchoring the match once another flag was set")
	}
	if !matchesExtraPattern("barfoo", "/foo/") {
		t.Fatal("a non-sticky pattern stopped matching anywhere")
	}
}

// ── Fix 9: the length thresholds count UTF-16 code units, not bytes ─────────

// jsLength is JS `value.length`: the number of UTF-16 code units in a string.
// The two fixtures below use it to prove their own shape, so that proof travels
// with the fixture instead of reaching for either implementation's own helper.
func jsLength(value string) int { return len(utf16.Encode([]rune(value))) }

func TestRegression_ErrorShapedThresholdCountsUTF16Units(t *testing.T) {
	// 6 + 150 runes = 156 UTF-16 units but 468 bytes: JS treats this text as a
	// short message (strong token check included), while a byte comparison calls
	// it long and refuses to classify it at all.
	text := "请求过于频繁" + strings.Repeat("网", 150)
	if jsLength(text) > MaxErrorShapedChars {
		t.Fatalf("the fixture is no longer short in UTF-16 units: %d", jsLength(text))
	}
	if len(text) <= MaxErrorShapedChars {
		t.Fatal("the fixture no longer proves the byte/unit difference")
	}
	match := MatchUpstreamModelError(&text, DefaultWakeFilterConfig)
	if !match.Matched {
		t.Fatalf("a short error-shaped message was not classified: %+v", match)
	}
	if match.Pattern != "请求过于频繁" {
		t.Fatalf("unexpected match label: %q", match.Pattern)
	}

	// And the reverse: text that is long in units must go the long way round,
	// where only an envelope at the head classifies it.
	long := "请求过于频繁" + strings.Repeat("网", 400)
	if jsLength(long) <= MaxErrorShapedChars {
		t.Fatal("the long fixture is not long in UTF-16 units")
	}
	if MatchUpstreamModelError(&long, DefaultWakeFilterConfig).Matched {
		t.Fatal("a long message without an error envelope was classified")
	}
}

// probeTail is a turn-signal probe over one fixed tail.
type probeTail struct {
	tail string
	size int64
}

func (p probeTail) ReadTail(path string, chars int) *string { return &p.tail }
func (p probeTail) Size(path string) *int64                 { return &p.size }
func (p probeTail) Sleep(ms int)                            {}

func TestRegression_TurnSignalCandidateThresholdCountsUTF16Units(t *testing.T) {
	// The candidate is the last 200 UTF-16 code units, mirroring JS
	// `expectedText.slice(-200)`: 150 emoji are 600 UTF-8 bytes and 300 units, so
	// the candidate is the last 100 of them and a session tail carrying those 100
	// proves the write. Measuring the cut in bytes or runes moves both edges.
	expected := strings.Repeat("🙂", 150)
	if jsLength(expected) <= turnSignalCandidateMax {
		t.Fatal("the fixture is no longer long in UTF-16 units")
	}
	if len(expected) <= turnSignalCandidateMax {
		t.Fatal("the fixture no longer proves the byte/unit difference")
	}
	timeout := 0
	full := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: expected,
		Path:         "session.jsonl",
		TimeoutMS:    &timeout,
		Probe:        probeTail{tail: "prefix " + strings.Repeat("🙂", 100), size: 128},
	})
	if !full.Confirmed {
		t.Fatalf("the last 200 UTF-16 units were not accepted as the candidate: %+v", full)
	}

	partial := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: expected,
		Path:         "session.jsonl",
		TimeoutMS:    &timeout,
		Probe:        probeTail{tail: "prefix " + strings.Repeat("🙂", 70), size: 128},
	})
	if partial.Confirmed {
		t.Fatal("a candidate shorter than the last 200 UTF-16 units confirmed the write")
	}
	if partial.Reason != ReasonTimeout {
		t.Fatalf("unexpected reason: %s", partial.Reason)
	}
}

// ── Fix R2-1: a stale ack failure must not pollute the new cycle ────────────

// TestRegression_StaleAckFailureDoesNotPolluteTheNewCycle drives the failure
// branch of the acknowledgement loop while the connection it was issued on is
// already gone: the ack stops at the door, the extension disconnects (same
// client object — a reconnect reuses it), and only then does the failure
// answer arrive, classified as resync. The old code treated that failure as
// current: it wrote the failure into the delivery queue it had just kept and
// answered it with a fresh `agent.orchestrator.get` resync on the same
// connection. The guard must reject the whole failure instead.
func TestRegression_StaleAckFailureDoesNotPolluteTheNewCycle(t *testing.T) {
	event := wakeEvent(65, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent")})
	client := newBlockingClient(func(method string, params any) any {
		// register and get both answer with the same pending page: the event is
		// queued from the registration on, which is what schedules the wake.
		return connectionResponse(nil, &connectionOptions{Events: []AgentEventWireRecord{event}, HasEvents: true})
	})
	f := newBlockingFixture(t, client)
	startBlocking(t, f, client)
	f.timers.Advance(10 * time.Millisecond)
	if len(f.pi.WakeInjections()) != 1 {
		t.Fatalf("the wake did not inject: %d", len(f.pi.WakeInjections()))
	}
	f.core.HandleAgentStart()
	f.core.HandleMessageEnd(wakeConsumptionEvidence([]int{65}))

	// The acknowledgement stops at the door; while it is in flight the
	// connection drops and the role is lost.
	client.arm()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		f.core.HandleAgentSettled(f.ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && client.issued("agent.notifications.ack") == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if client.issued("agent.notifications.ack") == 0 {
		client.releaseAll()
		t.Fatal("the settlement never issued the acknowledgement")
	}
	// The disconnect is recorded from its own goroutine with a watchdog: the
	// old implementation parked the core lock across the ack request itself, so
	// there the disconnect could not even be applied -- the timeout below is
	// that regression, not a hang of this case.
	disconnectDone := make(chan struct{})
	go func() {
		defer close(disconnectDone)
		f.core.markDisconnected(f.ctx)
	}()
	select {
	case <-disconnectDone:
	case <-time.After(watchdog):
		client.releaseAll()
		t.Fatal("the core lock stayed held across the acknowledgement request: the disconnect could not be recorded")
	}
	getsBefore := client.issued("agent.orchestrator.get")

	// Now the stale failure arrives: resync-classified, on the dead cycle.
	client.failOn("agent.notifications.ack", &rpcFailure{Code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER", Message: "out of order"})
	client.releaseAll()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the settlement did not return after the stale failure was released")
	}

	// The guard must have rejected the failure: no resync read, no failure
	// written into the delivery queue, no pending page re-added. (The wake path
	// issued its own `agent.orchestrator.get` earlier, so only the delta after
	// the disconnect counts here.)
	if got := client.issued("agent.orchestrator.get"); got != getsBefore {
		t.Fatalf("a stale ack failure triggered a resync after the disconnect: %d new get(s)", got-getsBefore)
	}
	f.core.mu.Lock()
	queued := f.core.unackedDelivered[event.ID]
	pending := append([]AgentEventWireRecord{}, f.core.pendingEvents...)
	f.core.mu.Unlock()
	if queued.LastFailureCode != nil {
		t.Fatalf("the stale failure was written into the delivery queue: %+v", queued)
	}
	if len(pending) != 0 {
		t.Fatalf("the stale failure re-added pending events: %+v", pending)
	}
}

// ── Fix R3-1: an early-exit registration must not stick the in-flight
// marker, and a stale one must not borrow the new cycle ────────────────────

// TestRegression_EarlyRegistrationExitDoesNotStickTheInFlightMarker covers the
// registration lifecycle from both ends.
//
// Sub-test A closes the extension before the registration goroutine can do
// anything, so finishRegistration takes its early-exit branch — the one path
// that used to return without releasing the in-flight marker and made every
// later connect skip registering for the rest of the session.
//
// Sub-test B parks a registration inside its daemon request and then replaces
// the connection under it. The old finishRegistration captured the connection
// epoch *after* the request, from inside its own lock section, so a
// registration that had been started for the dead cycle read the new epoch,
// passed its own check and applied its response to the cycle that had just
// replaced it. The registration must instead be dropped at the entry, with the
// marker it owns cleared and the next connect registering normally.
//
// Sub-test C proves the subscriber is part of the connection identity: a guard
// from a replaced subscriber is not "still live" even when the client object,
// the epochs and the scope all match.
func TestRegression_EarlyRegistrationExitDoesNotStickTheInFlightMarker(t *testing.T) {
	t.Run("closed before the goroutine runs", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any { return ownerResponse() })
		f := newBlockingFixture(t, client)
		// Bring the identity up without dialling: the fake client's
		// ResetForSession only fires a callback that is not installed yet.
		f.core.HandleSessionStart(f.ctx, nil)

		// Count the registrations the live cycle already issued, so the check
		// below cannot be satisfied by a client that started from zero.
		before := client.issued("agent.orchestrator.register")

		// The registration goroutine will find the extension closed before it can
		// issue anything: exactly the early-exit branch.
		f.core.mu.Lock()
		f.core.closed = true
		f.core.mu.Unlock()
		f.core.registerPresence(f.ctx)

		deadline := time.Now().Add(watchdog)
		for time.Now().Before(deadline) {
			f.core.mu.Lock()
			inFlight := f.core.registrationInFlight
			f.core.mu.Unlock()
			if inFlight == nil {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		f.core.mu.Lock()
		inFlight := f.core.registrationInFlight
		f.core.mu.Unlock()
		if inFlight != nil {
			t.Fatal("an early-exit registration left registrationInFlight set: later connects can never register")
		}

		// The early exit must happen before the daemon request, not after it: a
		// closed extension that issued agent.orchestrator.register (and then
		// applied the reply) reached the daemon for a cycle that was already
		// gone. The pre-fix implementation had no guard before the request.
		if got := client.issued("agent.orchestrator.register"); got != before {
			t.Fatalf("a closed extension issued agent.orchestrator.register: %d -> %d", before, got)
		}
		if coreConnected(f.core) {
			t.Fatal("a closed extension marked itself connected")
		}

		// The extension must still be able to register once it is open again.
		f.core.mu.Lock()
		f.core.closed = false
		f.core.mu.Unlock()
		client.ResetForSession()
		awaitRegistration(t, f.core)
		if !coreConnected(f.core) {
			t.Fatal("the extension did not register after the early exit was cleaned up")
		}
	})

	// The registration goroutine is parked at the entry lock of
	// finishRegistration — the test holds e.mu across the `go func()` that
	// registerPresence starts — while the cycle it was created for is replaced
	// underneath it. A registration that only gets the lock after that
	// replacement must be dropped: the old code validated nothing but the client
	// object there, so it went on and issued its request with the parameters of
	// the cycle that had already been replaced.
	t.Run("stale registration after a reconnect", func(t *testing.T) {
		var phase int32
		var paramMu sync.Mutex
		var registerSubscribers []string
		client := newBlockingClient(func(method string, params any) any {
			if method == "agent.orchestrator.register" {
				record, _ := params.(map[string]any)
				subscriber, _ := record["subscriberId"].(string)
				paramMu.Lock()
				registerSubscribers = append(registerSubscribers, subscriber)
				paramMu.Unlock()
				if atomic.LoadInt32(&phase) == 2 {
					// A registration that is issued for a replaced cycle must never
					// be applied: this reply would strip the orchestrator role.
					return nonOwnerResponse()
				}
			}
			return ownerResponse()
		})
		f := newBlockingFixture(t, client)
		atomic.StoreInt32(&phase, 1)
		startBlocking(t, f, client)
		if !coreConnected(f.core) {
			t.Fatal("the fixture never registered")
		}
		f.core.mu.Lock()
		liveSubscriber := ""
		if f.core.subscriberID != nil {
			liveSubscriber = *f.core.subscriberID
		}
		f.core.mu.Unlock()

		// Start a second registration while this test holds e.mu: its goroutine
		// cannot enter finishRegistration's lock section until the lock is
		// released, which is what makes the interleaving deterministic.
		atomic.StoreInt32(&phase, 2)
		f.core.mu.Lock()
		f.core.registerPresence(f.ctx)
		staleDone := f.core.registrationInFlight
		// The reconnect replaces the cycle while the goroutine is still waiting
		// for the lock: a fresh subscriber, and the in-flight marker cleared so
		// the next connect may register again.
		f.core.subscriberID = stringPtr("pi-session-replaced")
		f.core.registrationInFlight = nil
		f.core.mu.Unlock()
		if staleDone == nil {
			t.Fatal("the registration goroutine never set its in-flight marker")
		}

		select {
		case <-staleDone:
		case <-time.After(2 * time.Second):
			t.Fatal("the stale registration goroutine never finished")
		}
		if got := client.issued("agent.orchestrator.register"); got != 1 {
			paramMu.Lock()
			seen := strings.Join(registerSubscribers, ",")
			paramMu.Unlock()
			t.Fatalf("a registration started for a replaced cycle still reached the daemon: %d register call(s) (subscriberIds: %s)", got, seen)
		}
		f.core.mu.Lock()
		stillOrchestrator := f.core.isOrchestrator
		inFlight := f.core.registrationInFlight
		f.core.mu.Unlock()
		if !stillOrchestrator {
			t.Fatal("the stale registration response was applied to the live cycle")
		}
		if inFlight != nil {
			t.Fatal("the stale registration left registrationInFlight set: later connects can never register")
		}

		// And a later connect must still be able to run a fresh registration,
		// whose own marker survives to its completion.
		atomic.StoreInt32(&phase, 3)
		client.ResetForSession()
		awaitRegistration(t, f.core)
		if !coreConnected(f.core) {
			t.Fatal("the extension did not re-register after the stale registration was dropped")
		}
		if got := client.issued("agent.orchestrator.register"); got != 2 {
			t.Fatalf("the re-registration never reached the daemon: %d register call(s)", got)
		}
		_ = liveSubscriber
	})

	// The subscriber is part of the connection identity: a session replacement
	// swaps it while the client object and the scope survive, and a command
	// answered for the old subscriber describes a session this core no longer
	// is. The status command is the observable path: it captures its identity
	// before the request and only applies the answer while that identity still
	// holds.
	t.Run("a replaced subscriber is not the live connection", func(t *testing.T) {
		client := newBlockingClient(func(method string, params any) any { return ownerResponse() })
		f := newBlockingFixture(t, client)
		startBlocking(t, f, client)

		client.arm()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			f.core.handleCommand("status", f.ctx)
		}()
		waitForIssued(t, client, "agent.orchestrator.get", 1)

		// A new session replaces the subscriber while the request is in flight.
		// The old implementations park the core lock across that request, so the
		// swap cannot even be recorded there: the watchdog below is that
		// regression, not a hang of this case.
		swapped := make(chan struct{})
		go func() {
			defer close(swapped)
			f.core.mu.Lock()
			f.core.subscriberID = stringPtr("pi-session-replaced")
			f.core.mu.Unlock()
		}()
		select {
		case <-swapped:
		case <-time.After(watchdog):
			client.releaseAll()
			t.Fatal("the status command held the extension lock across its daemon request")
		}
		client.releaseAll()
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("the status command did not return after its request was released")
		}
		if note := f.ctx.LastNotification(); note != nil && strings.Contains(note.message, "Herdsman is watching") {
			t.Fatal("a command answered for a replaced subscriber was applied to the live cycle")
		}
	})
}

// ── Fix R3-2: a cross-cycle ack batch must stop at its own guard ─────────

// TestRegression_CrossCycleAckResyncStopsTheBatch drives a two-event
// acknowledgement batch whose first event fails with a resync-classified error
// while the connection it was issued on is replaced under the resync read.
//
// The old handleAckFailure returned true from its AckResync branch whatever
// happened to the guard while the `agent.orchestrator.get` was in flight, and
// acknowledgeOne re-read the live guard for the next event: so the second event
// of the batch was acknowledged under the identity of the *new* cycle. Both the
// batch guard and the resync read must stop the loop instead.
func TestRegression_CrossCycleAckResyncStopsTheBatch(t *testing.T) {
	event1 := wakeEvent(81, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent1")})
	event2 := wakeEvent(82, "agent.done", map[string]any{}, &eventOptions{terminal: stringPtr("term_agent2")})
	client := newBlockingClient(func(method string, params any) any {
		return connectionResponse(nil, &connectionOptions{Events: []AgentEventWireRecord{event1, event2}, HasEvents: true})
	})
	f := newBlockingFixture(t, client)
	startBlocking(t, f, client)

	// Two events sit in the delivery queue with consumption evidence for both,
	// which is what makes them the confirmable prefix of an agent_settled batch.
	f.core.mu.Lock()
	f.core.unackedDelivered[event1.ID] = event1
	f.core.unackedDelivered[event2.ID] = event2
	f.core.wakeConsumptionObserved[event1.ID] = true
	f.core.wakeConsumptionObserved[event2.ID] = true
	f.core.mu.Unlock()

	// The resync read parks at the door of its own method: everything else
	// (the ack of the first event, the reconnect's register) flows freely.
	getGate := make(chan struct{})
	resyncParked := make(chan struct{}, 1)
	client.setOnEnter(func(method string) {
		if method != "agent.orchestrator.get" {
			return
		}
		select {
		case resyncParked <- struct{}{}:
		default:
		}
		<-getGate
	})
	client.failOn("agent.notifications.ack", &rpcFailure{
		Code:    "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
		Message: "event id out of range / resync",
	})

	settled := make(chan struct{})
	go func() {
		defer close(settled)
		f.core.HandleAgentSettled(f.ctx)
	}()
	waitForIssued(t, client, "agent.notifications.ack", 1)
	select {
	case <-resyncParked:
	case <-time.After(2 * time.Second):
		close(getGate)
		t.Fatal("the acknowledgement failure never issued its resync read")
	}

	// The cycle is replaced while the resync read is parked: the connection drops
	// and the same client reconnects and takes the orchestrator role again, so
	// the live state is a perfectly valid new cycle. Both steps are watched: an
	// implementation that parks the core lock across a daemon request cannot
	// record them, and the case must report that instead of hanging.
	replaced := make(chan bool, 1)
	go func() {
		disconnectDone := make(chan struct{})
		go func() {
			defer close(disconnectDone)
			f.core.markDisconnected(f.ctx)
		}()
		select {
		case <-disconnectDone:
		case <-time.After(watchdog):
			replaced <- false
			return
		}
		reconnectDone := make(chan struct{})
		go func() {
			defer close(reconnectDone)
			client.ResetForSession()
		}()
		select {
		case <-reconnectDone:
		case <-time.After(watchdog):
			replaced <- false
			return
		}
		replaced <- true
	}()
	var reconnected bool
	select {
	case reconnected = <-replaced:
	case <-time.After(3 * time.Second):
	}
	// The resync read answers now, on a cycle the batch no longer belongs to.
	close(getGate)
	if !reconnected {
		// Let the parked request drain before the case reports, so the goroutine
		// that holds the core lock across it can release it.
		time.Sleep(50 * time.Millisecond)
		t.Fatal("the cycle could not be replaced while the resync read was parked: the core lock stayed held across the daemon request")
	}
	awaitRegistration(t, f.core)
	f.core.mu.Lock()
	liveAgain := f.core.connected && f.core.isOrchestrator
	f.core.mu.Unlock()
	if !liveAgain {
		t.Fatal("the reconnect did not restore a live orchestrator cycle")
	}

	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("the settlement did not return after the resync read was released")
	}
	if got := client.issued("agent.notifications.ack"); got != 1 {
		t.Fatalf("the batch kept acknowledging after its cycle was replaced: %d ack(s) for 2 event(s)", got)
	}
	f.core.mu.Lock()
	stillQueued := len(f.core.unackedDelivered)
	f.core.mu.Unlock()
	if stillQueued != 2 {
		t.Fatalf("the batch was settled on the new cycle: %d event(s) still queued, want 2", stillQueued)
	}
}

// ── Fix R2-3: a stale reconnect timer must not dial behind the live socket ───

// TestRegression_StaleReconnectTimerDoesNotDialAgain lets a scheduled reconnect
// backoff callback that has already STARTED be superseded by a newer attempt.
//
// The callback is parked at the client lock while the test holds it: the timer
// has expired (Stop() reports it already fired) but its goroutine has not run
// yet, so the only way it can act is through the lock the reset is about to
// take. The old callback cleared reconnectTimer unconditionally and called
// connect() again, so a callback that had already started dialled a second
// socket behind the live one.
func TestRegression_StaleReconnectTimerDoesNotDialAgain(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	// A 15ms backoff: the timer expires while the test still holds the client
	// lock below, which is what makes the callback "already started"
	// deterministic instead of a race the test happens to win.
	client := NewReconnectingDaemonClient(daemon.socket, []int64{15})
	t.Cleanup(client.Close)
	awaitCondition(t, "first connection", func() bool { return daemon.Connections() == 1 })

	// The live socket drops; handleDisconnect schedules the reconnect backoff.
	daemon.DropLive()
	awaitCondition(t, "the client went idle with a backoff armed", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.state == stateIdle && client.reconnectTimer != nil
	})

	// Hold the client lock until the 15ms timer has expired. Its callback
	// goroutine is now started and blocked on c.mu.Lock(): it validated nothing
	// yet, so whatever it does next is decided by the critical section.
	client.mu.Lock()
	timer := client.reconnectTimer
	time.Sleep(40 * time.Millisecond)
	fired := !timer.Stop()
	client.mu.Unlock()
	if !fired {
		t.Fatal("the reconnect timer had not fired while the client lock was held: the started-callback path was not exercised")
	}

	// A fresh attempt takes over. The parked callback wakes up inside the same
	// critical section (or right behind it) and must find its generation gone
	// and the timer no longer its own, so it exits without dialling.
	client.ResetForSession()
	awaitCondition(t, "second connection", func() bool { return daemon.Connections() == 2 })

	// The parked callback has had plenty of time to enter its critical section:
	// it must not have dialled a third socket behind the live one.
	time.Sleep(80 * time.Millisecond)
	if got := daemon.Connections(); got != 2 {
		t.Fatalf("a started reconnect callback dialled again: %d connection(s)", got)
	}
	if _, err := client.Request("agent.list", map[string]any{}); err != nil {
		t.Fatalf("the live connection did not survive the started reconnect callback: %v", err)
	}
}

// ── Fix 10 (see also the two rewritten cases in extension_test.go and
// extensionack_test.go): the oversized frame must never reach a handler ──────
