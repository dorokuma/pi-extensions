package herdragentstate

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func createTempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "herdr.sock")
}

// TestTransport_RealUnixSocket_Protocol verifies communication over a real unix socket.
// Asserts method, params, seq increment, session ref priority, and newline separation.
func TestTransport_RealUnixSocket_Protocol(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	type rawReceived struct {
		raw    string
		req    Request
		params map[string]any
	}

	var mu sync.Mutex
	var received []rawReceived

	stopServer := make(chan struct{})
	serverDone := make(chan struct{})

	go func() {
		defer close(serverDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-stopServer:
					return
				default:
					return
				}
			}

			// Read line
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err == nil {
				var req Request
				_ = json.Unmarshal([]byte(line), &req)
				paramBytes, _ := json.Marshal(req.Params)
				var params map[string]any
				_ = json.Unmarshal(paramBytes, &params)

				mu.Lock()
				received = append(received, rawReceived{
					raw:    line,
					req:    req,
					params: params,
				})
				mu.Unlock()

				// Reply to acknowledge receipt (triggers client finish(true))
				_, _ = conn.Write([]byte("{\"result\":null}\n"))
			}
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-test-1", nil, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond})

	// Test 1: Send report_agent_session with both path and id -> path must take precedence
	absPath := "/root/workspace/test/session.jsonl"
	sessionID := "sess-id-12345"
	startSource := "startup"
	client.SendReportAgentSession(SessionRef{Path: &absPath, ID: &sessionID}, &startSource)

	// Test 2: Send report_agent with only session ID
	msg := "waiting for user input"
	client.SendReportAgent(SessionRef{ID: &sessionID}, "blocked", &msg, NextReportSeq())

	// Test 3: Send report_agent with working state and no message
	client.SendReportAgent(SessionRef{Path: &absPath}, "working", nil, NextReportSeq())

	// Wait for requests to arrive using condition polling
	_ = pollCondition(1*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 3
	})
	close(stopServer)
	_ = listener.Close()
	<-serverDone

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("expected 3 received requests, got %d", len(received))
	}

	// Message 1 assertion: pane.report_agent_session
	m1 := received[0]
	if m1.req.Method != "pane.report_agent_session" {
		t.Errorf("m1 expected method pane.report_agent_session, got %s", m1.req.Method)
	}
	if !hasSuffix(m1.raw, "\n") {
		t.Errorf("m1 expected newline terminated")
	}
	if m1.params["pane_id"] != "pane-test-1" || m1.params["source"] != "herdr:pi" || m1.params["agent"] != "pi" {
		t.Errorf("m1 unexpected standard fields: %+v", m1.params)
	}
	if m1.params["agent_session_path"] != absPath {
		t.Errorf("m1 expected agent_session_path=%s, got %v", absPath, m1.params["agent_session_path"])
	}
	if _, ok := m1.params["agent_session_id"]; ok {
		t.Errorf("m1 agent_session_id should be omitted when agent_session_path is present")
	}
	if m1.params["session_start_source"] != "startup" {
		t.Errorf("m1 expected session_start_source=startup, got %v", m1.params["session_start_source"])
	}

	// Message 2 assertion: pane.report_agent with id
	m2 := received[1]
	if m2.req.Method != "pane.report_agent" {
		t.Errorf("m2 expected method pane.report_agent, got %s", m2.req.Method)
	}
	if m2.params["agent_session_id"] != sessionID {
		t.Errorf("m2 expected agent_session_id=%s, got %v", sessionID, m2.params["agent_session_id"])
	}
	if _, ok := m2.params["agent_session_path"]; ok {
		t.Errorf("m2 agent_session_path should be omitted")
	}
	if m2.params["state"] != "blocked" || m2.params["message"] != msg {
		t.Errorf("m2 unexpected state/message: %+v", m2.params)
	}

	// Message 3 assertion: pane.report_agent with working state
	m3 := received[2]
	if m3.params["state"] != "working" {
		t.Errorf("m3 expected state working, got %v", m3.params["state"])
	}
	if _, ok := m3.params["message"]; ok {
		t.Errorf("m3 expected message omitted, got %v", m3.params["message"])
	}

	// Seq monotonicity assertion
	seq1 := uint64(m1.params["seq"].(float64))
	seq2 := uint64(m2.params["seq"].(float64))
	seq3 := uint64(m3.params["seq"].(float64))
	if seq2 <= seq1 || seq3 <= seq2 {
		t.Errorf("expected monotonic seq increase: %d < %d < %d", seq1, seq2, seq3)
	}
}

// TestTransport_RetryAndTimeout asserts that when a server does not reply or drops connection,
// two distinct attempts are executed corresponding to the 500ms + 1500ms retries.
func TestTransport_RetryAndTimeout(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var attempts atomic.Int32
	var attemptDurations []time.Duration
	var mu sync.Mutex

	stopServer := make(chan struct{})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 256)
				_, _ = c.Read(buf)
				time.Sleep(150 * time.Millisecond)
			}(conn)
		}
	}()

	// Use fast injected timeouts to avoid waiting real 2000ms: 30ms and 60ms
	fastTimeouts := []time.Duration{30 * time.Millisecond, 60 * time.Millisecond}

	// Wrap dialer to record elapsed time of each attempt
	dialer := func(network, address string, timeout time.Duration) (net.Conn, error) {
		c, err := net.DialTimeout(network, address, timeout)
		mu.Lock()
		attemptDurations = append(attemptDurations, timeout)
		mu.Unlock()
		return c, err
	}

	client := NewClient(sockPath, "pane-retry-test", dialer, fastTimeouts)

	start := time.Now()
	client.SendRequest(Request{
		ID:     "test-req",
		Method: "test.method",
		Params: nil,
	})
	totalElapsed := time.Since(start)

	close(stopServer)
	_ = listener.Close()

	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected exactly 2 delivery attempts upon unresponsiveness, got %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attemptDurations) != 2 {
		t.Fatalf("expected 2 dial calls recorded, got %d", len(attemptDurations))
	}
	if attemptDurations[0] > 30*time.Millisecond || attemptDurations[0] < 25*time.Millisecond ||
		attemptDurations[1] > 60*time.Millisecond || attemptDurations[1] < 55*time.Millisecond {
		t.Errorf("expected timeouts bounded within remaining budget for [30ms, 60ms], got %v", attemptDurations)
	}
	if totalElapsed < 80*time.Millisecond {
		t.Errorf("total elapsed time %v unexpectedly faster than sum of timeouts", totalElapsed)
	}
}

// TestTransport_ImmediateClose_Retries asserts that if the server accepts and immediately closes (EOF),
// the client retries the 2nd attempt.
func TestTransport_ImmediateClose_Retries(t *testing.T) {
	sockPath := createTempSocket(t)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var attempts atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			_ = conn.Close() // immediate drop
		}
	}()

	client := NewClient(sockPath, "pane-drop-test", nil, []time.Duration{20 * time.Millisecond, 40 * time.Millisecond})
	client.SendRequest(Request{ID: "drop-req", Method: "pane.report_agent"})

	_ = listener.Close()

	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 attempts on immediate drop, got %d", got)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func pollCondition(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestTransport_NonJSONGarbage verifies that receiving non-JSON garbage bytes
// from server still satisfies delivery success, pinning TS data event behavior (C2).
func TestTransport_NonJSONGarbage(t *testing.T) {
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
			_, _ = reader.ReadString('\n')
			// Respond with non-JSON garbage bytes
			_, _ = conn.Write([]byte("NON_JSON_GARBAGE_PAYLOAD_ABC123\n"))
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-garbage-test", nil, []time.Duration{100 * time.Millisecond})
	ok := client.SendRequestAttempt(Request{ID: "garbage-req", Method: "pane.report_agent"}, 100*time.Millisecond)
	if !ok {
		t.Fatal("expected SendRequestAttempt to return true when receiving non-JSON bytes, matching TS socket.on('data')")
	}
}

// TestTransport_SingleAttemptHardTimeoutBudget asserts that a single attempt's total duration
// does not exceed timeout when the socket is unresponsive (S1 verification).
func TestTransport_SingleAttemptHardTimeoutBudget(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	// Server accepts and holds connection without sending any response or closing
	serverDone := make(chan struct{})
	defer close(serverDone)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				<-serverDone
				_ = c.Close()
			}(conn)
		}
	}()

	client := NewClient(sockPath, "pane-budget-test", nil, []time.Duration{50 * time.Millisecond})

	targetTimeout := 50 * time.Millisecond
	start := time.Now()
	ok := client.SendRequestAttempt(Request{ID: "budget-req", Method: "pane.report_agent"}, targetTimeout)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("expected SendRequestAttempt to return false on unresponsive socket")
	}

	t.Logf("S1 timing proof: targetTimeout=%v, measured elapsed=%v (%d ms)", targetTimeout, elapsed, elapsed.Milliseconds())

	// Hard budget: must be around targetTimeout (e.g. <= targetTimeout + 20ms scheduler tolerance),
	// strictly bounded well below 2x timeout (100ms).
	if elapsed > targetTimeout+25*time.Millisecond {
		t.Fatalf("attempt duration %v exceeded budget (target %v + 25ms tolerance)", elapsed, targetTimeout)
	}
}

type mockSessionProvider struct {
	mode        string
	isIdle      bool
	idleErr     error
	sessionFile *string
	fileErr     error
	sessionID   string
	idErr       error
}

func (m *mockSessionProvider) Mode() string {
	return m.mode
}

func (m *mockSessionProvider) IsIdle() (bool, error) {
	return m.isIdle, m.idleErr
}

func (m *mockSessionProvider) GetSessionFile() (*string, error) {
	return m.sessionFile, m.fileErr
}

func (m *mockSessionProvider) GetSessionID() (string, error) {
	return m.sessionID, m.idErr
}

type msgRecord struct {
	raw    string
	method string
	seq    uint64
}

// TestAgentStart_SessionSeqOrdering verifies that agent_start allocates report_agent_session's seq
// before report_agent's seq, asserting sessionMsg.seq < agentMsg.seq (S3).
func TestAgentStart_SessionSeqOrdering(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var mu sync.Mutex
	var received []msgRecord

	stopServer := make(chan struct{})
	defer close(stopServer)

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
				var seq uint64
				if pBytes, err := json.Marshal(req.Params); err == nil {
					var pMap map[string]any
					if err := json.Unmarshal(pBytes, &pMap); err == nil {
						if sVal, ok := pMap["seq"].(float64); ok {
							seq = uint64(sVal)
						}
					}
				}
				mu.Lock()
				received = append(received, msgRecord{raw: line, method: req.Method, seq: seq})
				mu.Unlock()

				_, _ = conn.Write([]byte("{\"result\":null}\n"))
			}
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-seq-test", nil, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond})
	sm := NewStateManager(client)

	sessionPath := "/root/workspace/test/session.jsonl"
	mockProvider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      true,
		sessionFile: &sessionPath,
	}

	// 1. Session start establishes rootSession
	sm.handleSessionStartCore(mockProvider, nil)

	// Wait for session_start messages to finish
	okInit := pollCondition(1*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 2
	})
	if !okInit {
		mu.Lock()
		t.Fatalf("expected 2 messages from session_start, got %d: %+v", len(received), received)
		mu.Unlock()
	}

	mu.Lock()
	var initSessionMsg, initAgentMsg *msgRecord
	for i := range received {
		if received[i].method == "pane.report_agent_session" && initSessionMsg == nil {
			initSessionMsg = &received[i]
		}
		if received[i].method == "pane.report_agent" && initAgentMsg == nil {
			initAgentMsg = &received[i]
		}
	}
	if initSessionMsg == nil || initAgentMsg == nil {
		t.Fatalf("missing session_start messages: sessionMsg=%v, agentMsg=%v", initSessionMsg, initAgentMsg)
	}
	if initSessionMsg.seq >= initAgentMsg.seq {
		t.Fatalf("expected session_start session.seq (%d) < agent.seq (%d)", initSessionMsg.seq, initAgentMsg.seq)
	}

	// Reset received list for agent_start observation
	received = nil
	mu.Unlock()

	// 2. Trigger agent_start
	sm.handleAgentStartCore(mockProvider)

	// Wait for 2 messages from agent_start (pane.report_agent_session and pane.report_agent)
	ok := pollCondition(1*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 2
	})
	if !ok {
		mu.Lock()
		t.Fatalf("expected 2 messages from agent_start, got %d: %+v", len(received), received)
		mu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()

	var sessionMsg, agentMsg *msgRecord
	for i := range received {
		if received[i].method == "pane.report_agent_session" && sessionMsg == nil {
			sessionMsg = &received[i]
		}
		if received[i].method == "pane.report_agent" && agentMsg == nil {
			agentMsg = &received[i]
		}
	}

	if sessionMsg == nil || agentMsg == nil {
		t.Fatalf("missing required messages: sessionMsg=%v, agentMsg=%v", sessionMsg, agentMsg)
	}

	t.Logf("S3 message 1 raw: %s", sessionMsg.raw)
	t.Logf("S3 message 2 raw: %s", agentMsg.raw)
	t.Logf("S3 seq assertion: session_seq=%d, agent_seq=%d", sessionMsg.seq, agentMsg.seq)

	if sessionMsg.seq >= agentMsg.seq {
		t.Fatalf("expected report_agent_session.seq (%d) < report_agent.seq (%d)", sessionMsg.seq, agentMsg.seq)
	}
}

// TestSessionStart_AsyncNonBlockingAndSeqOrdering asserts that handleSessionStartCore
// returns immediately (< 50ms) even when the server accepts but does not answer,
// and verifies that the session request is delivered asynchronously with seq < agent seq (R1).
func TestSessionStart_AsyncNonBlockingAndSeqOrdering(t *testing.T) {
	sockPath := createTempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	var mu sync.Mutex
	var received []msgRecord
	unblockServer := make(chan struct{})
	defer func() {
		select {
		case <-unblockServer:
		default:
			close(unblockServer)
		}
	}()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Hold connection without reading/answering until released
				<-unblockServer

				reader := bufio.NewReader(c)
				line, err := reader.ReadString('\n')
				if err == nil {
					var req Request
					_ = json.Unmarshal([]byte(line), &req)
					var seq uint64
					if pBytes, err := json.Marshal(req.Params); err == nil {
						var pMap map[string]any
						if err := json.Unmarshal(pBytes, &pMap); err == nil {
							if sVal, ok := pMap["seq"].(float64); ok {
								seq = uint64(sVal)
							}
						}
					}
					mu.Lock()
					received = append(received, msgRecord{raw: line, method: req.Method, seq: seq})
					mu.Unlock()

					_, _ = c.Write([]byte("{\"result\":null}\n"))
				}
			}(conn)
		}
	}()

	// Injected timeouts: 150ms + 300ms = 450ms worst-case if synchronous
	client := NewClient(sockPath, "pane-session-start-test", nil, []time.Duration{150 * time.Millisecond, 300 * time.Millisecond})
	sm := NewStateManager(client)

	sessionPath := "/root/workspace/test/session.jsonl"
	mockProvider := &mockSessionProvider{
		mode:        "tui",
		isIdle:      true,
		sessionFile: &sessionPath,
	}

	reason := "startup"
	start := time.Now()
	sm.handleSessionStartCore(mockProvider, &reason)
	elapsed := time.Since(start)

	// Budget assertion: handler must return immediately (< 50ms budget), not wait for attempts
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("session_start handler blocked for %v (exceeded 50ms budget; expected async non-blocking send)", elapsed)
	}
	t.Logf("session_start handler returned in %v (< 50ms budget satisfied)", elapsed)

	// Unblock server so pending/queued requests can complete
	close(unblockServer)

	// Wait for both session report and initial agent report
	ok := pollCondition(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 2
	})
	if !ok {
		mu.Lock()
		t.Fatalf("expected 2 messages delivered asynchronously, got %d: %+v", len(received), received)
		mu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()

	var sessionMsg, agentMsg *msgRecord
	for i := range received {
		if received[i].method == "pane.report_agent_session" && sessionMsg == nil {
			sessionMsg = &received[i]
		}
		if received[i].method == "pane.report_agent" && agentMsg == nil {
			agentMsg = &received[i]
		}
	}

	if sessionMsg == nil || agentMsg == nil {
		t.Fatalf("missing required messages: sessionMsg=%v, agentMsg=%v", sessionMsg, agentMsg)
	}

	t.Logf("R1 session message: %s (seq=%d)", sessionMsg.raw, sessionMsg.seq)
	t.Logf("R1 agent message: %s (seq=%d)", agentMsg.raw, agentMsg.seq)

	if sessionMsg.seq >= agentMsg.seq {
		t.Fatalf("expected report_agent_session.seq (%d) < report_agent.seq (%d)", sessionMsg.seq, agentMsg.seq)
	}
}
