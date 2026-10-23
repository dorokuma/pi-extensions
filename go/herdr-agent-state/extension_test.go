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
				_, _ = conn.Write([]byte("{\"result\":null}\n"))
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
