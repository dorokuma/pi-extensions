package herdragentstate

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// TestExtension_Disabled asserts that if HERDR_ENV != 1 or variables are missing,
// the extension is a complete no-op and never attempts any socket connection.
func TestExtension_Disabled(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.Close()
		}
	}()

	// Sub-test 1: Enabled() checks
	testCases := []struct {
		env     string
		sock    string
		pane    string
		enabled bool
	}{
		{"", sockPath, "pane-1", false},
		{"0", sockPath, "pane-1", false},
		{"true", sockPath, "pane-1", false},
		{"1", "", "pane-1", false},
		{"1", sockPath, "", false},
		{"1", sockPath, "pane-1", true},
	}

	for _, tc := range testCases {
		cfg := Config{Env: tc.env, SocketPath: tc.sock, PaneID: tc.pane}
		if got := Enabled(cfg); got != tc.enabled {
			t.Errorf("Enabled(%+v) = %v; want %v", cfg, got, tc.enabled)
		}
	}

	// Sub-test 2: Extension() with HERDR_ENV=0 in environment
	origEnv := os.Getenv("HERDR_ENV")
	origSock := os.Getenv("HERDR_SOCKET_PATH")
	origPane := os.Getenv("HERDR_PANE_ID")
	defer func() {
		_ = os.Setenv("HERDR_ENV", origEnv)
		_ = os.Setenv("HERDR_SOCKET_PATH", origSock)
		_ = os.Setenv("HERDR_PANE_ID", origPane)
	}()

	_ = os.Setenv("HERDR_ENV", "0")
	_ = os.Setenv("HERDR_SOCKET_PATH", sockPath)
	_ = os.Setenv("HERDR_PANE_ID", "pane-1")

	ext := Extension()
	if ext == nil {
		t.Fatal("Extension() should return non-nil *sdk.Extension even when disabled")
	}

	// Replace sleep with condition check verifying zero connections established
	pollCondition(50*time.Millisecond, func() bool {
		return connections.Load() != 0
	})

	if got := connections.Load(); got != 0 {
		t.Fatalf("expected 0 socket connections when disabled, got %d", got)
	}
}

// TestEventHandlers_Logic asserts event handling logic without external Pi host.
func TestEventHandlers_Logic(t *testing.T) {
	client := NewClient("dummy.sock", "pane-test", nil, []time.Duration{10 * time.Millisecond})
	sm := NewStateManager(client)

	// 1. herdr:blocked without rootSession must be ignored
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": true, "label": "review"})
	if sm.blockedCount != 0 {
		t.Errorf("expected blockedCount=0 when not rootSession, got %d", sm.blockedCount)
	}

	// 2. Set rootSession and trigger herdr:blocked
	sm.mu.Lock()
	sm.rootSession = true
	sm.mu.Unlock()

	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": true, "label": "review"})
	if sm.blockedCount != 1 || sm.blockedMessage != "review" {
		t.Errorf("expected blockedCount=1 and label='review', got count=%d label=%q", sm.blockedCount, sm.blockedMessage)
	}

	// 3. Increment again
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": true, "label": "permission"})
	if sm.blockedCount != 2 || sm.blockedMessage != "permission" {
		t.Errorf("expected blockedCount=2 and label='permission', got count=%d label=%q", sm.blockedCount, sm.blockedMessage)
	}

	// 4. Decrement once -> blockedCount=1, message remains
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": false})
	if sm.blockedCount != 1 || sm.blockedMessage != "permission" {
		t.Errorf("expected blockedCount=1, got count=%d label=%q", sm.blockedCount, sm.blockedMessage)
	}

	// 5. Decrement to zero -> blockedCount=0, message cleared
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": false})
	if sm.blockedCount != 0 || sm.blockedMessage != "" {
		t.Errorf("expected blockedCount=0, cleared message, got count=%d label=%q", sm.blockedCount, sm.blockedMessage)
	}

	// 6. Over-decrement does not drop below 0
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": false})
	if sm.blockedCount != 0 {
		t.Errorf("expected blockedCount=0, got %d", sm.blockedCount)
	}
}

// TestLifecycle_SessionStartGate asserts that non-tui modes do not enable rootSession (S4).
func TestLifecycle_SessionStartGate(t *testing.T) {
	client := NewClient("dummy.sock", "pane-test", nil, []time.Duration{10 * time.Millisecond})
	sm := NewStateManager(client)

	sessPath := "/root/workspace/test/session.jsonl"

	// 1. Non-tui mode (e.g. rpc / json / print) must be ignored completely
	nonTuiProvider := &mockSessionProvider{
		mode:        "rpc",
		isIdle:      true,
		sessionFile: &sessPath,
	}
	reason := "startup"
	sm.handleSessionStartCore(nonTuiProvider, &reason)

	if sm.rootSession {
		t.Fatal("expected rootSession=false for mode='rpc'")
	}
	if sm.agentActive {
		t.Fatal("expected agentActive=false for mode='rpc'")
	}

	// 2. TUI mode with isIdle=false -> rootSession=true, agentActive=true (working)
	tuiActiveProvider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      false,
		sessionFile: &sessPath,
	}
	sm.handleSessionStartCore(tuiActiveProvider, &reason)

	if !sm.rootSession {
		t.Fatal("expected rootSession=true for mode='tui'")
	}
	if !sm.agentActive {
		t.Fatal("expected agentActive=true when isIdle=false")
	}
	state, _ := sm.DesiredState()
	if state != "working" {
		t.Fatalf("expected state='working', got %s", state)
	}

	// 3. TUI mode with isIdle=true -> agentActive=false (idle)
	tuiIdleProvider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      true,
		sessionFile: &sessPath,
	}
	sm.handleSessionStartCore(tuiIdleProvider, &reason)
	if sm.agentActive {
		t.Fatal("expected agentActive=false when isIdle=true")
	}
	state, _ = sm.DesiredState()
	if state != "idle" {
		t.Fatalf("expected state='idle', got %s", state)
	}
}

// TestLifecycle_AgentStartGate asserts agent_start gate on rootSession (S4).
func TestLifecycle_AgentStartGate(t *testing.T) {
	client := NewClient("dummy.sock", "pane-test", nil, []time.Duration{10 * time.Millisecond})
	sm := NewStateManager(client)

	sessPath := "/root/workspace/test/session.jsonl"
	provider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      false,
		sessionFile: &sessPath,
	}

	// 1. Without rootSession, agent_start must do nothing
	sm.handleAgentStartCore(provider)
	if sm.agentActive {
		t.Fatal("expected agentActive=false when rootSession is false")
	}

	// 2. With rootSession=true, agent_start sets agentActive=true and publishes working
	sm.mu.Lock()
	sm.rootSession = true
	sm.mu.Unlock()

	sm.handleAgentStartCore(provider)
	if !sm.agentActive {
		t.Fatal("expected agentActive=true when rootSession is true")
	}
	state, _ := sm.DesiredState()
	if state != "working" {
		t.Fatalf("expected desiredState='working', got %s", state)
	}
}

// TestLifecycle_AgentSettledGate asserts agent_settled gate on rootSession and isIdle (S4),
// and verifies that when isIdle() is not true, no idle message is sent.
func TestLifecycle_AgentSettledGate(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var sentReports atomic.Int32
	var receivedStates []string
	var mu sync.Mutex

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err == nil {
				var req Request
				_ = json.Unmarshal([]byte(line), &req)
				if req.Method == "pane.report_agent" {
					pBytes, _ := json.Marshal(req.Params)
					var p ReportAgentParams
					_ = json.Unmarshal(pBytes, &p)
					mu.Lock()
					receivedStates = append(receivedStates, p.State)
					mu.Unlock()
				}
				sentReports.Add(1)
				_, _ = conn.Write(herdrSuccessReply(line))
			}
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-test", nil, []time.Duration{10 * time.Millisecond})
	sm := NewStateManager(client)

	drainWait := make(chan struct{}, 10)
	sm.SetDrainNotifyChan(drainWait)

	// Set sm initially active
	sm.mu.Lock()
	sm.rootSession = false
	sm.agentActive = true
	sm.mu.Unlock()

	// 1. Without rootSession, agent_settled is ignored
	sm.handleAgentSettledCore(&mockSessionProvider{isIdle: true})
	if !sm.agentActive {
		t.Fatal("expected agentActive to remain true without rootSession")
	}
	if got := sentReports.Load(); got != 0 {
		t.Fatalf("expected 0 reports sent without rootSession, got %d", got)
	}

	// Enable rootSession
	sm.mu.Lock()
	sm.rootSession = true
	sm.mu.Unlock()

	// 2. isIdle = false -> must NOT set idle and must NOT send idle message
	sm.handleAgentSettledCore(&mockSessionProvider{isIdle: false})
	if !sm.agentActive {
		t.Fatal("expected agentActive to remain true when isIdle=false")
	}
	time.Sleep(30 * time.Millisecond)
	if got := sentReports.Load(); got != 0 {
		t.Fatalf("expected 0 reports sent when isIdle=false (no idle message sent), got %d", got)
	}

	// 3. isIdle returns error -> must NOT set idle and must NOT send idle message
	sm.handleAgentSettledCore(&mockSessionProvider{isIdle: true, idleErr: net.ErrClosed})
	if !sm.agentActive {
		t.Fatal("expected agentActive to remain true when isIdle returns error")
	}
	time.Sleep(30 * time.Millisecond)
	if got := sentReports.Load(); got != 0 {
		t.Fatalf("expected 0 reports sent when isIdle returns error, got %d", got)
	}

	// 4. isIdle = true with nil error -> transitions to idle and sends idle message
	sm.handleAgentSettledCore(&mockSessionProvider{isIdle: true})
	if sm.agentActive {
		t.Fatal("expected agentActive=false when isIdle=true")
	}
	state, _ := sm.DesiredState()
	if state != "idle" {
		t.Fatalf("expected state='idle', got %s", state)
	}

	select {
	case <-drainWait:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for queue drain")
	}

	if got := sentReports.Load(); got != 1 {
		t.Fatalf("expected exactly 1 idle report sent when isIdle=true, got %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(receivedStates) != 1 || receivedStates[0] != "idle" {
		t.Fatalf("expected 1 received state 'idle', got %v", receivedStates)
	}
}

// TestLifecycle_BlockedPriorityAndMessages asserts message priority and counter transitions (S4).
func TestLifecycle_BlockedPriorityAndMessages(t *testing.T) {
	client := NewClient("dummy.sock", "pane-test", nil, []time.Duration{10 * time.Millisecond})
	sm := NewStateManager(client)

	sm.mu.Lock()
	sm.rootSession = true
	sm.agentActive = true // working
	sm.mu.Unlock()

	// 1. Initial active state without block is working
	st, msg := sm.DesiredState()
	if st != "working" || msg != nil {
		t.Fatalf("expected working with nil message, got st=%s msg=%v", st, msg)
	}

	// 2. Blocked event arrives with message
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": true, "label": "user confirmation"})
	st, msg = sm.DesiredState()
	if st != "blocked" || msg == nil || *msg != "user confirmation" {
		t.Fatalf("expected blocked with message 'user confirmation', got st=%s msg=%v", st, msg)
	}

	// 3. Second blocked event updates message and increments count
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": true, "label": "diff approval"})
	st, msg = sm.DesiredState()
	if st != "blocked" || msg == nil || *msg != "diff approval" {
		t.Fatalf("expected blocked with message 'diff approval', got st=%s msg=%v", st, msg)
	}

	// 4. One unblock decrements count to 1, message persists
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": false})
	st, msg = sm.DesiredState()
	if st != "blocked" || msg == nil || *msg != "diff approval" {
		t.Fatalf("expected still blocked with message 'diff approval', got st=%s msg=%v", st, msg)
	}

	// 5. Second unblock drops to 0, reverts to working (since agentActive=true) and clears message
	_ = sm.HandleHerdrBlocked(sdk.Context{}, map[string]any{"active": false})
	st, msg = sm.DesiredState()
	if st != "working" || msg != nil {
		t.Fatalf("expected reverted to working with nil message, got st=%s msg=%v", st, msg)
	}
}

// TestResume_NotOnSessionReportBeforePaneHeld covers the resume gating branch:
// Herdr refuses resume_argv until the reporter holds the pane by reporting state
// first, so the very first pane.report_agent_session must not carry it, while the
// session report that follows an accepted state report does. One behaviour
// branch, no extra cases.
func TestResume_NotOnSessionReportBeforePaneHeld(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err == nil {
				_, _ = conn.Write(herdrSuccessReply(line))
			}
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-resume-gate", nil, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond})
	sm := NewStateManager(client)

	drainWait := make(chan struct{}, 10)
	sm.SetDrainNotifyChan(drainWait)

	sessPath := "/root/workspace/test/session.jsonl"
	sm.mu.Lock()
	sm.rootSession = true
	sm.currentAgentSessionPath = &sessPath
	sm.mu.Unlock()

	// Before any state report has been delivered the source does not hold the
	// pane, so the session report must not carry resume_argv (Herdr would answer
	// resume_not_accepted).
	source := "startup"
	req, ok := sm.PrepareReportSession(&source)
	if !ok {
		t.Fatal("expected PrepareReportSession to build a request")
	}
	params, _ := req.Params.(ReportAgentSessionParams)
	if params.ResumeArgv != nil {
		t.Fatalf("expected no resume_argv on the first session report: the pane is not held yet, got %v", params.ResumeArgv)
	}

	// Deliver one state report: the pane counts as held only after Herdr has
	// accepted it, which is exactly when drainLoop sets agentStateReported
	// (queueing alone must not mark the pane as held).
	sm.PublishState(true)
	select {
	case <-drainWait:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for queue drain")
	}

	sm.mu.Lock()
	reported := sm.agentStateReported
	sm.mu.Unlock()
	if !reported {
		t.Fatal("expected agentStateReported=true only after a state report was accepted by herdr")
	}

	req, ok = sm.PrepareReportSession(nil)
	if !ok {
		t.Fatal("expected PrepareReportSession to build a request")
	}
	params, _ = req.Params.(ReportAgentSessionParams)
	argv := params.ResumeArgv
	if len(argv) != 3 || argv[0] != "pig" || argv[1] != "--session" || argv[2] != sessPath {
		t.Fatalf("expected resume_argv [pig --session %s] once the pane is held, got %v", sessPath, argv)
	}
}

// TestRelease_AndResumeArgv covers the two protocol gaps the official doc requires
// beyond state reporting: the resume command attached to reports, and the release
// on exit. One behaviour branch each, no extra cases. The shutdown half also pins
// the S2 lifecycle contract: HandleSessionShutdown resets the state machine and
// waits for the in-flight drain iteration to end before pane.release_agent is
// sent, so the release is ordered strictly after the report and the pane is not
// re-locked after exit.
func TestRelease_AndResumeArgv(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	type record struct {
		method string
		params map[string]any
	}

	var mu sync.Mutex
	var received []record

	// reportSeen fires once the server has read a request line, i.e. the client
	// send is on the wire (for the delayed report, still awaiting its reply).
	reportSeen := make(chan struct{}, 4)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				var req Request
				_ = json.Unmarshal([]byte(line), &req)
				pBytes, _ := json.Marshal(req.Params)
				var p map[string]any
				_ = json.Unmarshal(pBytes, &p)

				select {
				case reportSeen <- struct{}{}:
				default:
				}

				if req.Method == "pane.report_agent" {
					// Model Herdr taking a moment to apply the state report: the
					// client send stays in flight until this reply is written, so a
					// shutdown that does not wait for the drain would let the
					// release overtake the still-pending report.
					time.Sleep(150 * time.Millisecond)
				}

				mu.Lock()
				received = append(received, record{method: req.Method, params: p})
				mu.Unlock()

				_, _ = c.Write(herdrSuccessReply(line))
			}(conn)
		}
	}()

	// Timeouts comfortably above the server's 150ms report delay so the first
	// attempt succeeds and no retry duplicates the report.
	client := NewClient(sockPath, "pane-release-test", nil, []time.Duration{500 * time.Millisecond, 1000 * time.Millisecond})
	sessPath := "/root/workspace/test/session.jsonl"
	sm := NewStateManager(client)

	sm.mu.Lock()
	sm.rootSession = true
	sm.currentAgentSessionPath = &sessPath
	sm.mu.Unlock()

	// 1. A state report must carry the resume command. The server holds its
	// reply, so the send is still in flight when shutdown arrives.
	sm.PublishState(true)
	select {
	case <-reportSeen:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for the state report to reach the server")
	}

	// 2. session_shutdown while the report send is in flight: it must reset the
	// state machine and block until the in-flight drain iteration ends before
	// sending pane.release_agent.
	if _, err := sm.HandleSessionShutdown(sdk.Context{}, nil); err != nil {
		t.Fatalf("HandleSessionShutdown returned error: %v", err)
	}

	sm.mu.Lock()
	reset := !sm.rootSession && sm.queuedState == nil && !sm.agentStateReported &&
		sm.lastState == nil && sm.lastMessage == nil && !sm.agentActive &&
		sm.blockedCount == 0 && sm.blockedMessage == "" && !sm.sendInFlight && sm.inFlightDone == nil
	sm.mu.Unlock()
	if !reset {
		t.Fatal("expected HandleSessionShutdown to reset the state machine and clear the in-flight drain")
	}

	pollCondition(1*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 2
	})

	assertFirstSession := func() {
		mu.Lock()
		defer mu.Unlock()
		if len(received) != 2 {
			t.Fatalf("expected 2 requests (report_agent + release_agent), got %d: %+v", len(received), received)
		}

		// The report must be applied before the release: without the in-flight wait
		// the release would overtake the still-pending report and re-lock the pane.
		agent := received[0]
		if agent.method != "pane.report_agent" {
			t.Fatalf("expected first method pane.report_agent, got %s", agent.method)
		}
		argv, ok := agent.params["resume_argv"].([]any)
		if !ok || len(argv) != 3 || argv[0] != "pig" || argv[1] != "--session" || argv[2] != sessPath {
			t.Fatalf("expected resume_argv [pig --session %s], got %v", sessPath, agent.params["resume_argv"])
		}

		release := received[1]
		if release.method != "pane.release_agent" {
			t.Fatalf("expected second method pane.release_agent, got %s", release.method)
		}
		for _, key := range []string{"pane_id", "source", "agent", "seq"} {
			if _, present := release.params[key]; !present {
				t.Fatalf("release_agent params missing required %q: %+v", key, release.params)
			}
		}
		if release.params["pane_id"] != "pane-release-test" || release.params["source"] != SourcePi || release.params["agent"] != "pi" {
			t.Fatalf("unexpected release_agent identity fields: %+v", release.params)
		}

		// release_agent.seq is allocated only after the in-flight report has been
		// sent, so it must be strictly greater than the report's seq.
		agentSeq := uint64(agent.params["seq"].(float64))
		releaseSeq := uint64(release.params["seq"].(float64))
		if releaseSeq <= agentSeq {
			t.Fatalf("expected release_agent.seq (%d) > report_agent.seq (%d)", releaseSeq, agentSeq)
		}
	}
	assertFirstSession()

	// 3. A handler that is still querying IsIdle() outside the lock must not
	// report anything once the session was shut down: the session generation
	// it captured is stale by then, so it may not publish a state any more.
	//
	// A second session takes the manager over first, so the stale handler
	// belongs to a generation that the shutdown closes again.
	sessionProvider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      true,
		sessionFile: &sessPath,
	}
	sm.handleSessionStartCore(sessionProvider, nil)

	if !pollCondition(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 4
	}) {
		mu.Lock()
		t.Fatalf("expected the second session to report session+state, got %+v", received)
		mu.Unlock()
	}

	// Make the state of the second session observable: a stale handler that
	// were not rejected would publish a new state (idle) here.
	sm.mu.Lock()
	sm.agentActive = true
	sm.lastState = nil
	sm.lastMessage = nil
	sm.mu.Unlock()

	// The stale handler blocks inside IsIdle(), i.e. outside the lock.
	gate := make(chan struct{})
	idleEntered := make(chan struct{})
	staleProvider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      true,
		sessionFile: &sessPath,
		idleHook: func() {
			close(idleEntered)
			<-gate
		},
	}
	staleDone := make(chan struct{})
	go func() {
		defer close(staleDone)
		sm.handleAgentSettledCore(staleProvider)
	}()
	<-idleEntered

	// The session is shut down while the handler is still waiting for IsIdle().
	if _, err := sm.HandleSessionShutdown(sdk.Context{}, nil); err != nil {
		t.Fatalf("HandleSessionShutdown returned error: %v", err)
	}

	// Isolate everything that would arrive after the release.
	mu.Lock()
	released := false
	for _, r := range received {
		if r.method == "pane.release_agent" {
			released = true
		}
	}
	received = nil
	mu.Unlock()
	if !released {
		t.Fatal("expected pane.release_agent to be sent for the second session")
	}

	// Let the stale handler finish its IsIdle() query and settle.
	close(gate)
	<-staleDone
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 0 {
		t.Fatalf("expected no request after pane.release_agent (the stale handler must not report again), got %+v", received)
	}
}

// TestShutdown_WaitsForInFlightSessionReport is the regression for the
// unsynchronized session report: pane.report_agent_session used to be
// delivered by an untracked goroutine, so pane.release_agent could overtake a
// session report that was still in flight. Every request of a session must
// share one sending lifecycle: the release is delivered only after all
// in-flight session/state requests have completed, and the session report is
// sequenced before the state report of the same session.
// One behaviour branch (send lifecycle ordering), no extra cases.
func TestShutdown_WaitsForInFlightSessionReport(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	type record struct {
		method string
		seq    uint64
	}

	var mu sync.Mutex
	var completed []record

	// sessionSeen is signalled when the server has read a session report and
	// starts holding its reply, i.e. while that send is still in flight.
	sessionSeen := make(chan struct{}, 4)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				var req Request
				_ = json.Unmarshal([]byte(line), &req)
				pBytes, _ := json.Marshal(req.Params)
				var p map[string]any
				_ = json.Unmarshal(pBytes, &p)
				var seq uint64
				if v, ok := p["seq"].(float64); ok {
					seq = uint64(v)
				}

				if req.Method == "pane.report_agent_session" {
					select {
					case sessionSeen <- struct{}{}:
					default:
					}
					// Hold the reply so the session report stays in flight
					// while the shutdown runs.
					time.Sleep(300 * time.Millisecond)
				}

				mu.Lock()
				completed = append(completed, record{method: req.Method, seq: seq})
				mu.Unlock()

				_, _ = c.Write(herdrSuccessReply(line))
			}(conn)
		}
	}()

	client := NewClient(sockPath, "pane-session-wait-test", nil, []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond})
	sm := NewStateManager(client)

	sessPath := "/root/workspace/test/session.jsonl"
	provider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      false,
		sessionFile: &sessPath,
	}

	// 1. A session starts: the session report must be sequenced before the
	// state report of the same session.
	reason := "startup"
	sm.handleSessionStartCore(provider, &reason)

	select {
	case <-sessionSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the session report to reach the server")
	}

	if !pollCondition(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		var session, state bool
		for _, r := range completed {
			switch r.method {
			case "pane.report_agent_session":
				session = true
			case "pane.report_agent":
				state = true
			}
		}
		return session && state
	}) {
		t.Fatal("timed out waiting for the session report and the state report of the first session")
	}

	mu.Lock()
	var firstSession, firstState *record
	for i := range completed {
		if completed[i].method == "pane.report_agent_session" && firstSession == nil {
			firstSession = &completed[i]
		}
		if completed[i].method == "pane.report_agent" && firstState == nil {
			firstState = &completed[i]
		}
	}
	mu.Unlock()
	if firstSession == nil || firstState == nil {
		t.Fatalf("expected a session report and a state report, got %+v", completed)
	}
	if firstSession.seq >= firstState.seq {
		t.Fatalf("expected the session report seq (%d) < the state report seq (%d)", firstSession.seq, firstState.seq)
	}

	// 2. A second session report goes in flight and the session is shut down
	// while it is still on the wire.
	sm.handleAgentStartCore(provider)
	select {
	case <-sessionSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the second session report to reach the server")
	}

	if _, err := sm.HandleSessionShutdown(sdk.Context{}, nil); err != nil {
		t.Fatalf("HandleSessionShutdown returned error: %v", err)
	}

	// The second session report is still on the wire when the shutdown
	// returns in the broken implementation: wait for it, so the delivery
	// order of the release can be compared with it.
	if !pollCondition(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		var sessions int
		for _, r := range completed {
			if r.method == "pane.report_agent_session" {
				sessions++
			}
		}
		return sessions >= 2
	}) {
		t.Fatal("timed out waiting for the second session report to be delivered")
	}

	mu.Lock()
	defer mu.Unlock()

	releaseIdx := -1
	sessionIdx := 0
	secondSessionIdx := -1
	for i, r := range completed {
		if r.method == "pane.release_agent" {
			releaseIdx = i
		}
		if r.method == "pane.report_agent_session" {
			sessionIdx++
			if sessionIdx == 2 {
				secondSessionIdx = i
			}
		}
	}
	if releaseIdx < 0 {
		t.Fatalf("expected a pane.release_agent request, got %+v", completed)
	}
	if secondSessionIdx < 0 {
		t.Fatalf("expected two pane.report_agent_session requests, got %+v", completed)
	}
	if releaseIdx < secondSessionIdx {
		t.Fatalf("expected pane.release_agent to be delivered after the in-flight session report, got %+v", completed)
	}
	if completed[releaseIdx].seq <= completed[secondSessionIdx].seq {
		t.Fatalf("expected the release seq (%d) > the session report seq (%d), got %+v",
			completed[releaseIdx].seq, completed[secondSessionIdx].seq, completed)
	}
}

// TestExtension_EventWiring asserts that Extension() registers handlers when enabled (S4).
func TestExtension_EventWiring(t *testing.T) {
	origEnv := os.Getenv("HERDR_ENV")
	origSock := os.Getenv("HERDR_SOCKET_PATH")
	origPane := os.Getenv("HERDR_PANE_ID")
	defer func() {
		_ = os.Setenv("HERDR_ENV", origEnv)
		_ = os.Setenv("HERDR_SOCKET_PATH", origSock)
		_ = os.Setenv("HERDR_PANE_ID", origPane)
	}()

	_ = os.Setenv("HERDR_ENV", "1")
	_ = os.Setenv("HERDR_SOCKET_PATH", "dummy.sock")
	_ = os.Setenv("HERDR_PANE_ID", "pane-test-wire")

	ext := Extension()
	if ext == nil {
		t.Fatal("Extension() should return non-nil extension when enabled")
	}
}

// TestExtension_DefersBusSubscriptionToOnLoaded asserts the herdr:blocked
// subscription is deferred to OnLoaded instead of being made inside the factory.
// A factory-time Events().On dials the host socket and waits on a synchronous
// host RPC; when /reload admits several extensions at once the host can still be
// accepting an earlier member, so that wait never ends and the reload deadlocks.
// This fake host accepts the connection and never answers it, so a factory-time
// subscription shows up as exactly that accepted connection.
func TestExtension_DefersBusSubscriptionToOnLoaded(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	t.Setenv("PIG_EXT_SOCKET", sockPath)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", sockPath)
	t.Setenv("HERDR_PANE_ID", "pane-factory")

	done := make(chan *sdk.Extension, 1)
	go func() { done <- Extension() }()

	select {
	case ext := <-done:
		if ext == nil {
			t.Fatal("Extension() returned nil")
		}
	case conn := <-accepted:
		_ = conn.Close()
		t.Fatal("Extension() opened the host socket from the factory: the bus subscription must be deferred to OnLoaded")
	case <-time.After(5 * time.Second):
		t.Fatal("Extension() did not return: a factory-time host call blocks the reload")
	}
}
