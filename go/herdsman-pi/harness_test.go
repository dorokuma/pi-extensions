package herdsmanpi

// Test harness: the Go equivalent of the TS suite's createFakeClient /
// createFakePi / fakeCtx / vi.useFakeTimers. Everything runs in-process and on
// a manual clock; no test ever opens a socket against the production daemon,
// and no test reads or writes anything under the real ~/.herdsman or
// ~/.pi/agent (see TestMain for the log-home redirect).

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// ── manual clock ────────────────────────────────────────────────────────────

type manualTimerEntry struct {
	owners *manualTimers
	id     int64
	at     time.Time
	// interval of 0 means one-shot; SetInterval needs it.
	interval time.Duration
	callback func()
	repeat   bool
	canceled bool
}

// manualTimers is the Go stand-in for vi.useFakeTimers: callbacks fire only
// from Advance, in deadline order (id order breaks ties), and callbacks may
// queue further timers inside the advanced window.
type manualTimers struct {
	mu      sync.Mutex
	now     time.Time
	nextID  int64
	entries map[int64]*manualTimerEntry
}

func newManualTimers() *manualTimers {
	return &manualTimers{
		now:     time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		entries: map[int64]*manualTimerEntry{},
	}
}

func (m *manualTimers) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// UnixMilli keeps the extension's nowMS() on the same fake clock.
func (m *manualTimers) UnixMilli() int64 { return m.Now().UnixMilli() }

func (m *manualTimers) SetTimeout(delay time.Duration, callback func()) Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	entry := &manualTimerEntry{owners: m, id: m.nextID, at: m.now.Add(delay), callback: callback}
	m.entries[entry.id] = entry
	return entry
}

func (m *manualTimers) SetInterval(interval time.Duration, callback func()) Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	entry := &manualTimerEntry{
		owners: m, id: m.nextID, at: m.now.Add(interval), interval: interval, callback: callback, repeat: true,
	}
	m.entries[entry.id] = entry
	return entry
}

func (t *manualTimerEntry) Stop() bool {
	if t == nil {
		return false
	}
	owner := t.owners
	if owner == nil {
		return false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.entries[t.id] == nil {
		return false
	}
	delete(owner.entries, t.id)
	return true
}

// Advance runs every callback due within the next window.
func (m *manualTimers) Advance(window time.Duration) {
	target := m.Now().Add(window)
	for {
		m.mu.Lock()
		var next *manualTimerEntry
		for _, entry := range m.entries {
			if entry.at.After(target) {
				continue
			}
			if next == nil || entry.at.Before(next.at) || (entry.at.Equal(next.at) && entry.id < next.id) {
				next = entry
			}
		}
		if next == nil {
			m.now = target
			m.mu.Unlock()
			return
		}
		m.now = next.at
		if next.repeat {
			next.at = next.at.Add(next.interval)
		} else {
			delete(m.entries, next.id)
		}
		callback := next.callback
		m.mu.Unlock()
		callback()
	}
}

// pendingTimerCount lets tests assert the queue is empty.
func (m *manualTimers) pendingTimerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// ── fake daemon client ──────────────────────────────────────────────────────

type clientCall struct {
	method string
	params any
}

// fakeClient mirrors the TS createFakeClient: recorded calls, a pluggable
// response function, and manual connect/stream triggers.
type fakeClient struct {
	mu       sync.Mutex
	calls    []clientCall
	closed   bool
	closeFn  func()
	response func(method string, params any) any
	failure  func(method string, params any) error

	onConnected    func() error
	onDisconnected func(error)
	onStream       func(DaemonStreamMessage)

	// dialInline makes ResetForSession run the onConnected callback inline, the
	// way the real client's synchronous dial does.
	dialInline bool

	// currentEvents backs the default connectionResponse (the TS
	// activeFakeClient.currentEvents).
	currentEvents map[int]AgentEventWireRecord
}

func newFakeClient() *fakeClient {
	return &fakeClient{currentEvents: map[int]AgentEventWireRecord{}}
}

func (c *fakeClient) Close() {
	c.mu.Lock()
	c.closed = true
	closeFn := c.closeFn
	c.mu.Unlock()
	if closeFn != nil {
		closeFn()
	}
}

func (c *fakeClient) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeClient) ResetForSession() {
	c.mu.Lock()
	dialInline := c.dialInline
	handler := c.onConnected
	c.mu.Unlock()
	// dialInline reproduces the real client's synchronous dial: the connect
	// callback runs on the caller's goroutine, inside ResetForSession.
	if dialInline && handler != nil {
		_ = handler()
	}
}

func (c *fakeClient) Request(method string, params any) (any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, clientCall{method: method, params: params})
	failure := c.failure
	response := c.response
	c.mu.Unlock()
	if failure != nil {
		if err := failure(method, params); err != nil {
			return nil, err
		}
	}
	if response != nil {
		return response(method, params), nil
	}
	return map[string]any{}, nil
}

func (c *fakeClient) OnConnected() func() error       { return c.onConnected }
func (c *fakeClient) SetOnConnected(h func() error)   { c.onConnected = h }
func (c *fakeClient) OnDisconnected() func(error)     { return c.onDisconnected }
func (c *fakeClient) SetOnDisconnected(h func(error)) { c.onDisconnected = h }
func (c *fakeClient) OnStreamMessage() func(DaemonStreamMessage) {
	return c.onStream
}
func (c *fakeClient) SetOnStreamMessage(h func(DaemonStreamMessage)) { c.onStream = h }

// ConnectNow fires the onConnected handler (the TS `await client.connect()`);
// tests call awaitRegistration afterwards.
func (c *fakeClient) ConnectNow() {
	c.mu.Lock()
	handler := c.onConnected
	c.mu.Unlock()
	if handler != nil {
		_ = handler()
	}
}

func (c *fakeClient) DisconnectNow(err error) {
	c.mu.Lock()
	handler := c.onDisconnected
	c.mu.Unlock()
	if handler != nil {
		handler(err)
	}
}

func (c *fakeClient) EmitStream(message DaemonStreamMessage) {
	c.mu.Lock()
	handler := c.onStream
	c.mu.Unlock()
	if handler != nil {
		handler(message)
	}
}

func (c *fakeClient) Calls() []clientCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clientCall{}, c.calls...)
}

// record appends a call the way Request does; a wrapper around another client
// uses it to keep the fixture's call log complete.
func (c *fakeClient) record(method string, params any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, clientCall{method: method, params: params})
}

func (c *fakeClient) CallsFor(method string) []clientCall {
	var out []clientCall
	for _, call := range c.Calls() {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

// AckedIDs mirrors the TS `ackedIds()` helper.
func (c *fakeClient) AckedIDs() []int {
	var ids []int
	for _, call := range c.CallsFor("agent.notifications.ack") {
		if params, ok := call.params.(map[string]any); ok {
			if id, ok := jsNumberAsInt(params["eventId"]); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (c *fakeClient) Methods() []string {
	calls := c.Calls()
	methods := make([]string, len(calls))
	for index, call := range calls {
		methods[index] = call.method
	}
	return methods
}

// ── fake pi / context ───────────────────────────────────────────────────────

type sentMessage struct {
	message CustomMessage
	options SendMessageOptions
}

// fakePi implements PiAPI and records what the extension sends.
type fakePi struct {
	mu        sync.Mutex
	failureOn bool
	// HiddenMessages receives display=false messages (the TS hiddenMessages).
	HiddenMessages []sentMessage
	CustomMessages []sentMessage
	Entries        []clientCall
}

func (p *fakePi) AppendEntry(customType string, data any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Entries = append(p.Entries, clientCall{method: customType, params: data})
	return nil
}

func (p *fakePi) SendMessage(message CustomMessage, options SendMessageOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failureOn {
		return errors.New("pi.sendMessage failed")
	}
	record := sentMessage{message: message, options: options}
	if !message.Display {
		p.HiddenMessages = append(p.HiddenMessages, record)
	} else {
		p.CustomMessages = append(p.CustomMessages, record)
	}
	return nil
}

// SetSendFailure makes SendMessage fail while set (the TS throwing
// sendMessageImplementation).
func (p *fakePi) SetSendFailure(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failureOn = fail
}

func (p *fakePi) SetSessionName(name string) error { return nil }

// WakeInjections mirrors the TS `wakeInjections(pi)`.
func (p *fakePi) WakeInjections() []sentMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sentMessage
	for _, entry := range p.HiddenMessages {
		if entry.message.CustomType == wakeContextCustomType {
			out = append(out, entry)
		}
	}
	return out
}

// fakeContext implements Context.
type fakeContext struct {
	mu            sync.Mutex
	Statuses      map[string]*string
	Notifications []notification
	idle          *bool // nil: the host cannot answer
	Aborts        int
	sessionFile   string
	sessionID     string
	// hasUI mirrors ctx.hasUI: nil is "the host never reported one" (unknown),
	// a pointer is the reported value. The SDK always knows it; the tests keep
	// the TS "absent means unknown" surface.
	hasUI *bool
	// mode mirrors ctx.mode: "" is "the host never named one". The SDK pins an
	// unreported mode to "print", so tests that model an interactive run pass
	// it explicitly.
	mode string
}

type notification struct {
	message string
	level   NotifyLevel
}

func newFakeContext(idle *bool) *fakeContext {
	if idle == nil {
		on := true
		idle = &on
	}
	return &fakeContext{
		Statuses:    map[string]*string{},
		idle:        idle,
		sessionFile: "/tmp/pi-session.jsonl",
		sessionID:   "pi-session",
	}
}

func (c *fakeContext) Abort() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Aborts++
}

func (c *fakeContext) IsIdle() (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle == nil {
		return false, false
	}
	return *c.idle, true
}

func (c *fakeContext) SetIdle(idle bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idle = &idle
}

func (c *fakeContext) SessionFile() string { return c.sessionFile }
func (c *fakeContext) SessionID() string   { return c.sessionID }
func (c *fakeContext) HasUI() (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasUI == nil {
		return false, false
	}
	return *c.hasUI, true
}
func (c *fakeContext) Mode() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == "" {
		return "", false
	}
	return c.mode, true
}
func (c *fakeContext) UI() ContextUI { return fakeUI{owner: c} }

type fakeUI struct{ owner *fakeContext }

func (u fakeUI) Notify(message string, level NotifyLevel) {
	u.owner.mu.Lock()
	defer u.owner.mu.Unlock()
	u.owner.Notifications = append(u.owner.Notifications, notification{message, level})
}

func (u fakeUI) SetStatus(key string, value *string) {
	u.owner.mu.Lock()
	defer u.owner.mu.Unlock()
	u.owner.Statuses[key] = value
}

func (c *fakeContext) Status(key string) *string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Statuses[key]
}

func (c *fakeContext) LastNotification() *notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Notifications) == 0 {
		return nil
	}
	return &c.Notifications[len(c.Notifications)-1]
}

// ── extension fixture helpers (TS startExtension / withHerdrEnv) ───────────

type herdsmanEnv struct {
	paneID      string
	workspaceID string
	socketPath  string
	autoOn      string
	ctxmodeRole string
	hasEnv      bool
}

// envFunc builds the Env closure the extension reads, from an optional Herdr
// identity (the TS withHerdrEnv / restoreEnv pair).
func envFunc(identity *herdsmanEnv) func(key string) (string, bool) {
	values := map[string]string{}
	if identity != nil {
		values["HERDR_ENV"] = "1"
		values["HERDR_SOCKET_PATH"] = valueOr(identity.socketPath, "/tmp/herdr.sock")
		values["HERDR_PANE_ID"] = valueOr(identity.paneID, "wB:p1")
		values["HERDR_WORKSPACE_ID"] = valueOr(identity.workspaceID, "wB")
		if identity.autoOn != "" {
			values["HERDSMAN_AUTO_ON"] = identity.autoOn
		}
		if identity.ctxmodeRole != "" {
			values["CTXMODE_ROLE"] = identity.ctxmodeRole
		}
	}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func valueOr(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// ── wire-record builders (TS event()/connectionResponse()) ─────────────────

type eventOptions struct {
	compactHistory    *CompactAgentHistory
	hasHistory        bool
	paneGeneration    *string
	hasPaneGeneration bool
	paneID            *string
	payload           map[string]any
	status            *string
	typ               *string
	hasTerminal       bool
	terminal          *string
	workspace         *string
	// text overrides the assistant body (nil keeps `assistant result <id>`).
	text *string
}

func wireEvent(id int, terminal *string, options *eventOptions) AgentEventWireRecord {
	if options == nil {
		options = &eventOptions{}
	}
	event := AgentEventWireRecord{
		ID:         id,
		Payload:    map[string]any{"agent": "claude"},
		TerminalID: terminal,
	}
	event.CompactHistory = &CompactAgentHistory{
		LastAssistantMessage: &CompactText{Text: stringPtr("done")},
	}
	if options.hasHistory {
		event.CompactHistory = options.compactHistory
	}
	if options.paneID != nil {
		event.PaneID = options.paneID
	} else {
		panel := "wB:p-agent"
		event.PaneID = &panel
	}
	if options.payload != nil {
		merged := map[string]any{"agent": "claude"}
		for key, value := range options.payload {
			merged[key] = value
		}
		event.Payload = merged
	}
	if options.hasTerminal {
		event.TerminalID = options.terminal
	} else if terminal != nil {
		event.TerminalID = terminal
	}
	if options.hasPaneGeneration {
		event.PaneGeneration = options.paneGeneration
	}
	event.Status = options.status
	event.Type = "agent.done"
	if options.typ != nil {
		event.Type = *options.typ
	}
	if options.workspace != nil {
		event.WorkspaceID = options.workspace
	} else {
		event.WorkspaceID = stringPtr("wB")
	}
	return event
}

func textPtr(value string) *string { return &value }

// connectionOptions mirrors the TS connectionResponse() options bag.
type connectionOptions struct {
	AckedEventID  *int
	HasAcked      bool
	Changed       *bool
	Context       *AgentWorkspaceContextSnapshot
	HasContext    bool
	Events        []AgentEventWireRecord
	HasEvents     bool
	OwnerTerminal *string
	HasOwner      bool
	PaneID        string
	WorkspaceID   string
}

func connectionResponse(client *fakeClient, options *connectionOptions) map[string]any {
	if options == nil {
		options = &connectionOptions{}
	}
	paneID := valueOr(options.PaneID, "wB:p1")
	workspaceID := valueOr(options.WorkspaceID, "wB")
	ownerTerminal := "term_pi"
	hasOwner := true
	if options.HasOwner {
		ownerTerminal = textOrEmpty(options.OwnerTerminal)
		hasOwner = options.OwnerTerminal != nil
	}
	var events []AgentEventWireRecord
	switch {
	case options.HasEvents:
		events = options.Events
	case client != nil:
		client.mu.Lock()
		for _, event := range client.currentEvents {
			events = append(events, event)
		}
		client.mu.Unlock()
		sort.Slice(events, func(left, right int) bool { return events[left].ID < events[right].ID })
	}
	acked := 0
	if options.AckedEventID != nil {
		acked = *options.AckedEventID
		// The TS helper always spells `ackedEventId` inside `state`; when the
		// caller passes a value it is the same watermark in both places.
		options.HasAcked = true
	}
	response := map[string]any{
		"events": events,
		"presence": map[string]any{
			"connectedAt":      1,
			"herdrSessionName": "default",
			"paneId":           paneID,
			"subscriberId":     "pi-session",
			"terminalId":       "term_pi",
			"workspaceId":      workspaceID,
		},
		"state": map[string]any{
			"ackedEventId":     acked,
			"herdrSessionName": "default",
			"owner":            nil,
			"updatedAt":        "2026-07-10T00:00:00.000Z",
			"workspaceId":      workspaceID,
		},
	}
	if hasOwner {
		ownerPane := "wB:p-other"
		if ownerTerminal == "term_pi" {
			ownerPane = paneID
		}
		response["state"].(map[string]any)["owner"] = map[string]any{
			"paneId":     ownerPane,
			"terminalId": ownerTerminal,
		}
	}
	if options.HasContext {
		if options.Context == nil {
			response["context"] = nil
		} else {
			response["context"] = *options.Context
		}
	}
	if options.Changed != nil {
		response["changed"] = *options.Changed
	}
	if options.HasAcked {
		response["ackedEventId"] = acked
	}
	return response
}

// wakeConsumptionEvidence builds the `message_end` event a drained wake copy
// produces (TS wakeConsumptionEvidence).
func wakeConsumptionEvidence(presentedEventIDs []int, eventIDs ...int) map[string]any {
	ids := eventIDs
	if len(ids) == 0 {
		ids = presentedEventIDs
	}
	presented := make([]any, len(presentedEventIDs))
	for index, id := range presentedEventIDs {
		presented[index] = id
	}
	all := make([]any, len(ids))
	for index, id := range ids {
		all[index] = id
	}
	return map[string]any{
		"message": map[string]any{
			"content":    "[" + "HERDSMAN AGENT UPDATES] …",
			"customType": wakeContextCustomType,
			"details": map[string]any{
				"eventIds":          all,
				"presentedEventIds": presented,
			},
			"display": false,
			"role":    "custom",
		},
	}
}

// assistantEnd builds the `message_end` event of a final assistant message.
func assistantEnd(stopReason string, content any) map[string]any {
	if content == nil {
		content = []any{map[string]any{"text": "completed", "type": "text"}}
	}
	return map[string]any{
		"message": map[string]any{
			"content":    content,
			"role":       "assistant",
			"stopReason": stopReason,
			"turnId":     "turn-1",
		},
	}
}

// roleChangeStream builds an `agent.orchestrator.changed` stream message.
func roleChangeStream(previousTerminal *string, currentTerminal *string, currentPane string) DaemonStreamMessage {
	if currentPane == "" {
		currentPane = "wB:p1"
	}
	return DaemonStreamMessage{
		Method: "agent.orchestrator.changed",
		Change: &AgentOrchestratorChanged{
			Reason: "claimed",
			Current: AgentOrchestratorWireState{
				AckedEventID:     0,
				HerdrSessionName: "default",
				Owner:            ownerRecord(currentTerminal, currentPane),
				UpdatedAt:        "2026-07-10T00:00:01.000Z",
				WorkspaceID:      "wB",
			},
			Previous: AgentOrchestratorWireState{
				AckedEventID:     0,
				HerdrSessionName: "default",
				Owner:            ownerRecord(previousTerminal, "wB:p1"),
				UpdatedAt:        "2026-07-10T00:00:00.000Z",
				WorkspaceID:      "wB",
			},
		},
	}
}

func ownerRecord(terminal *string, pane string) *AgentOrchestratorOwner {
	if terminal == nil {
		return nil
	}
	return &AgentOrchestratorOwner{PaneID: pane, TerminalID: *terminal}
}

// awaitRegistration waits for the registration goroutine of the core to drain
// (the TS `await client.connect()` awaits the same promise chain).
func awaitRegistration(t *testing.T, core *HerdsmanExtension) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		core.mu.Lock()
		pending := core.registrationInFlight
		core.mu.Unlock()
		if pending == nil {
			return
		}
		select {
		case <-pending:
		case <-time.After(time.Until(deadline)):
			t.Fatal("registration did not settle within 2s")
			return
		}
	}
}

// startFixture wires client+pi+context+core exactly like the TS
// `startExtension`, and returns them to the test.
type fixture struct {
	t       *testing.T
	core    *HerdsmanExtension
	client  *fakeClient
	pi      *fakePi
	ctx     *fakeContext
	timers  *manualTimers
	signals []<-chan struct{}
	signal  chan struct{}
	// serverPage is the fake daemon's pending page for the wake tests.
	serverPage *serverPageState
}

func newFixture(t *testing.T, options ExtensionOptions, idle bool, identity *herdsmanEnv) *fixture {
	t.Helper()
	client := newFakeClient()
	if options.ClientFactory == nil {
		options.ClientFactory = func() HerdsmanDaemonClient { return client }
	}
	timers := newManualTimers()
	options.Timers = timers
	if options.Env == nil {
		options.Env = envFunc(identity)
	}
	if options.WakeFilter == nil {
		filter := DefaultWakeFilterConfig
		options.WakeFilter = &filter
	}
	signals := make(chan struct{}, 1024)
	if options.OnTurnCompletionSignal == nil {
		options.OnTurnCompletionSignal = func(completion <-chan struct{}) {
			go func() {
				<-completion
				select {
				case signals <- struct{}{}:
				default:
				}
			}()
		}
	}
	pi := &fakePi{}
	fixture := &fixture{t: t, client: client, pi: pi, timers: timers, signal: signals}
	fixture.ctx = newFakeContext(&idle)
	fixture.core = NewExtension(pi, options)
	return fixture
}

// start performs session_start + connect + registration, like the TS helper.
func (f *fixture) start() {
	f.core.HandleSessionStart(f.ctx, nil)
	f.client.ConnectNow()
	awaitRegistration(f.t, f.core)
}

// waitForCompletionSignal waits for one turn-completion goroutine to finish
// (the TS awaits the completion promise), so the next client-calls assertion is
// deterministic.
func (f *fixture) waitForCompletionSignal() {
	f.t.Helper()
	select {
	case <-f.signal:
	case <-time.After(2 * time.Second):
		f.t.Fatal("turn completion signal did not settle within 2s")
	}
}

// drainCompletionSignals consumes every already-delivered signal.
func (f *fixture) drainCompletionSignals() {
	for {
		select {
		case <-f.signal:
		default:
			return
		}
	}
}
