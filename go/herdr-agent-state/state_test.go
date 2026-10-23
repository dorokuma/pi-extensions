package herdragentstate

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStateMachine_Transitions tests the exact state progression:
// idle -> working -> blocked -> unblock -> idle
// It also verifies deduplication of identical states and that force=true overrides deduplication.
func TestStateMachine_Transitions(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	type stateReport struct {
		State   string
		Message *string
	}

	var mu sync.Mutex
	var reports []stateReport

	stopServer := make(chan struct{})

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
					reports = append(reports, stateReport{
						State:   p.State,
						Message: p.Message,
					})
					mu.Unlock()
				}
				_, _ = conn.Write([]byte("{\"result\":null}\n"))
			}
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-sm-test", nil, []time.Duration{50 * time.Millisecond, 100 * time.Millisecond})
	sm := NewStateManager(client)

	drainWait := make(chan struct{}, 10)
	sm.SetDrainNotifyChan(drainWait)

	waitForDrain := func() {
		select {
		case <-drainWait:
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for queue drain")
		}
	}

	// 1. Initial PublishState(force=true) -> idle
	sm.PublishState(true)
	waitForDrain()

	// 2. Duplicate PublishState(force=false) -> identical idle, should NOT send
	sm.PublishState(false)
	// Brief wait to ensure no message was queued or sent
	time.Sleep(30 * time.Millisecond)

	// 3. agentActive = true -> working
	sm.mu.Lock()
	sm.agentActive = true
	sm.mu.Unlock()
	sm.PublishState(false)
	waitForDrain()

	// 4. Duplicate working state -> should NOT send
	sm.PublishState(false)
	time.Sleep(30 * time.Millisecond)

	// 5. blockedCount > 0 with label -> blocked(label)
	sm.mu.Lock()
	sm.blockedCount = 1
	sm.blockedMessage = "human in the loop"
	sm.mu.Unlock()
	sm.PublishState(false)
	waitForDrain()

	// 6. blockedCount decremented back to 0 -> working (since agentActive is still true)
	sm.mu.Lock()
	sm.blockedCount = 0
	sm.blockedMessage = ""
	sm.mu.Unlock()
	sm.PublishState(false)
	waitForDrain()

	// 7. agentActive = false -> idle
	sm.mu.Lock()
	sm.agentActive = false
	sm.mu.Unlock()
	sm.PublishState(false)
	waitForDrain()

	// 8. force=true on same idle state -> MUST send
	sm.PublishState(true)
	waitForDrain()

	close(stopServer)
	_ = listener.Close()

	mu.Lock()
	defer mu.Unlock()

	expectedStates := []string{
		"idle",
		"working",
		"blocked",
		"working",
		"idle",
		"idle", // from force=true
	}

	if len(reports) != len(expectedStates) {
		t.Fatalf("expected %d reports, got %d: %+v", len(expectedStates), len(reports), reports)
	}

	for i, exp := range expectedStates {
		if reports[i].State != exp {
			t.Errorf("step %d: expected state %s, got %s", i, exp, reports[i].State)
		}
		if exp == "blocked" {
			if reports[i].Message == nil || *reports[i].Message != "human in the loop" {
				t.Errorf("step %d: expected blocked message 'human in the loop', got %v", i, reports[i].Message)
			}
		} else {
			if reports[i].Message != nil {
				t.Errorf("step %d: expected nil message, got %v", i, *reports[i].Message)
			}
		}
	}
}

// TestQueue_CoalescingAndSerialization asserts that when 3 state transitions arrive in quick succession
// while one is in-flight, the intermediate state is coalesced/superseded, and only the latest state is sent,
// with strictly one socket send in-flight at any moment.
func TestQueue_CoalescingAndSerialization(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var activeConnections atomic.Int32
	var maxConcurrentConnections atomic.Int32
	var mu sync.Mutex
	var receivedStates []string

	serverProcessingDelay := 50 * time.Millisecond

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			cur := activeConnections.Add(1)
			for {
				max := maxConcurrentConnections.Load()
				if cur <= max || maxConcurrentConnections.CompareAndSwap(max, cur) {
					break
				}
			}

			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err == nil {
				var req Request
				_ = json.Unmarshal([]byte(line), &req)
				pBytes, _ := json.Marshal(req.Params)
				var p ReportAgentParams
				_ = json.Unmarshal(pBytes, &p)

				// Artificially simulate processing delay so rapid queue states overlap
				time.Sleep(serverProcessingDelay)

				mu.Lock()
				receivedStates = append(receivedStates, p.State)
				mu.Unlock()

				_, _ = conn.Write([]byte("{\"result\":null}\n"))
			}
			activeConnections.Add(-1)
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-coalesce-test", nil, []time.Duration{200 * time.Millisecond, 400 * time.Millisecond})
	sm := NewStateManager(client)

	drainWait := make(chan struct{}, 10)
	sm.SetDrainNotifyChan(drainWait)

	// Step 1: Trigger state 1 (working)
	sm.mu.Lock()
	sm.agentActive = true
	sm.mu.Unlock()
	sm.PublishState(true) // Starts transmission of state 1 ("working")

	// Wait for the first send to establish connection on server
	if !pollCondition(1*time.Second, func() bool { return activeConnections.Load() > 0 }) {
		t.Fatal("timed out waiting for first send to establish connection")
	}

	// Step 2 & 3: While state 1 is still in flight (taking 50ms), rapidly push 2 and 3:
	// Transition to blocked (state 2)
	sm.mu.Lock()
	sm.blockedCount = 1
	sm.blockedMessage = "blocked-middle"
	sm.mu.Unlock()
	sm.PublishState(false)

	// Very immediately transition to unblocked / idle (state 3)
	sm.mu.Lock()
	sm.blockedCount = 0
	sm.agentActive = false
	sm.mu.Unlock()
	sm.PublishState(false)

	// Wait for queue to drain completely
	select {
	case <-drainWait:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drain")
	}

	_ = listener.Close()

	if max := maxConcurrentConnections.Load(); max > 1 {
		t.Fatalf("expected strictly 1 concurrent socket connection, but observed peak of %d", max)
	}

	mu.Lock()
	defer mu.Unlock()

	// Due to coalescing (queuedState replacement in drainStateQueue),
	// State 1 ("working") was sent, followed immediately by the latest State 3 ("idle").
	// Intermediate State 2 ("blocked") was overwritten in queue and NOT sent.
	if len(receivedStates) != 2 {
		t.Fatalf("expected exactly 2 states sent (coalesced), got %d: %v", len(receivedStates), receivedStates)
	}
	if receivedStates[0] != "working" {
		t.Errorf("expected 1st sent state to be 'working', got %s", receivedStates[0])
	}
	if receivedStates[1] != "idle" {
		t.Errorf("expected 2nd sent state to be 'idle' (coalescing dropped 'blocked'), got %s", receivedStates[1])
	}
}

// TestPathValidation_IsAbsolutePosixOrWin32 tests path format checking.
func TestPathValidation_IsAbsolutePosixOrWin32(t *testing.T) {
	cases := []struct {
		path     string
		expected bool
	}{
		{"/root/.pi/session.jsonl", true},
		{"/var/log", true},
		{"/", true},
		{`C:\Users\test\session.jsonl`, true},
		{`C:/Users/test/session.jsonl`, true},
		{`d:\data`, true},
		{`\\server\share\file`, true},
		{`//server/share/file`, true},
		{"relative/path.jsonl", false},
		{"./local.jsonl", false},
		{"../parent.jsonl", false},
		{"session.jsonl", false},
		{"", false},
	}

	for _, c := range cases {
		got := IsAbsolutePosixOrWin32(c.path)
		if got != c.expected {
			t.Errorf("IsAbsolutePosixOrWin32(%q) = %v; want %v", c.path, got, c.expected)
		}
	}
}
