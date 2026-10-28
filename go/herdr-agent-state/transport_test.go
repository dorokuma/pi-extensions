package herdragentstate

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// herdrSuccessReply mirrors the real Herdr success_response envelope
// ({"id":...,"result":...}): the request id is echoed and a result object is
// carried. The pre-fix fake servers replied with a bare {"result":null}, which
// no longer satisfies the id-matching validation.
func herdrSuccessReply(rawLine string) []byte {
	var head struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(rawLine), &head)
	idBytes, _ := json.Marshal(head.ID)
	out := append([]byte(`{"id":`), idBytes...)
	out = append(out, []byte(`,"result":{}}`+"\n")...)
	return out
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
				_, _ = conn.Write(herdrSuccessReply(line))
			}
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-test-1", nil, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond})

	// Test 1: Send report_agent_session with both path and id -> path must take precedence
	absPath := "/root/workspace/test/session.jsonl"
	sessionID := "sess-id-12345"
	startSource := "startup"
	client.SendReportAgentSession(SessionRef{Path: &absPath, ID: &sessionID}, &startSource, nil)

	// Test 2: Send report_agent with only session ID
	msg := "waiting for user input"
	client.SendReportAgent(SessionRef{ID: &sessionID}, "blocked", &msg, NextReportSeq(), nil)

	// Test 3: Send report_agent with working state and no message
	client.SendReportAgent(SessionRef{Path: &absPath}, "working", nil, NextReportSeq(), nil)

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
	if m1.params["pane_id"] != "pane-test-1" || m1.params["source"] != SourcePi || m1.params["agent"] != "pig" {
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

// TestTransport_JSONErrorResponseIsNotSuccess is the pre-registered negative control
// for the JSON error-response bug (brief2 B step 2: "解析匹配响应id、success/error并校验而非仅bytes").
// A herdr reply carrying an "error" envelope must NOT be counted as a successful report:
// the old implementation accepted any received bytes, so this case FAILS before the fix.
func TestTransport_JSONErrorResponseIsNotSuccess(t *testing.T) {
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
			// Herdr ErrorResponse envelope: {"id":..., "error":{"code":...,"message":...}}
			_, _ = conn.Write([]byte("{\"id\":\"garbage-req\",\"error\":{\"code\":\"pane_not_found\",\"message\":\"no such pane\"}}\n"))
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-error-test", nil, []time.Duration{100 * time.Millisecond})
	if ok := client.SendRequestAttempt(Request{ID: "garbage-req", Method: "pane.report_agent"}, 100*time.Millisecond); ok {
		t.Fatal("expected SendRequestAttempt to return false when herdr answers with a JSON error envelope, matching the official error_response schema")
	}
}

// TestSource_NotReservedByHerdr is the regression for the reserved-source bug.
// Herdr ships a built-in "pi" integration whose source is "herdr:pi"; a report
// carrying that source is answered {"result":{"type":"ok"}} yet silently
// discarded, so the pane keeps agent_status "unknown" (observed live on
// w3N:pE). This case pins the identity we send instead: agent stays "pig",
// matching the pig runtime, and the source is stable, unique, and never in
// Herdr's own "herdr:" namespace.
func TestSource_NotReservedByHerdr(t *testing.T) {
	if AgentPi != "pig" {
		t.Fatalf("expected agent %q, got %q", "pig", AgentPi)
	}
	if SourcePi == "herdr:pi" {
		t.Fatal("source must not be \"herdr:pi\": Herdr's own built-in pi integration owns it and silently drops such reports")
	}
	if strings.HasPrefix(SourcePi, "herdr:") {
		t.Fatalf("source must not start with \"herdr:\" (Herdr reserves that prefix for its own integrations), got %q", SourcePi)
	}
	if SourcePi == "" {
		t.Fatal("source must be a non-empty stable identifier")
	}

	// Every report must carry the same source.
	client := NewClient("dummy.sock", "pane-source-test", nil, []time.Duration{10 * time.Millisecond})
	sessPath := "/root/workspace/test/session.jsonl"
	if req, ok := client.BuildReportAgentSessionRequest(SessionRef{Path: &sessPath}, nil, nil); !ok {
		t.Fatal("expected a session report request")
	} else if req.Params.(ReportAgentSessionParams).Source != SourcePi {
		t.Fatalf("session report source = %q, want %q", req.Params.(ReportAgentSessionParams).Source, SourcePi)
	}
	rel := client.BuildReleaseAgentRequest()
	if rel.Params.(ReleaseAgentParams).Source != SourcePi {
		t.Fatalf("release source = %q, want %q", rel.Params.(ReleaseAgentParams).Source, SourcePi)
	}
}

// TestTransport_IDMismatchIsNotSuccess covers the id-matching half of the response
// validation: a success envelope whose id does not match the request is rejected.
func TestTransport_IDMismatchIsNotSuccess(t *testing.T) {
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
			// Well-formed success envelope, but the id belongs to another request.
			_, _ = conn.Write([]byte("{\"id\":\"some-other-request\",\"result\":{}}\n"))
			_ = conn.Close()
		}
	}()

	client := NewClient(sockPath, "pane-id-mismatch-test", nil, []time.Duration{100 * time.Millisecond})
	if ok := client.SendRequestAttempt(Request{ID: "mismatch-req", Method: "pane.report_agent"}, 100*time.Millisecond); ok {
		t.Fatal("expected SendRequestAttempt to return false when the response id does not match the request id")
	}
}

// TestTransport_NonJSONGarbage pins the corrected delivery contract: a reply that is
// not a parseable Herdr envelope is NOT a successful report. The TS port counted any
// received bytes as delivered (socket.on("data")), which is exactly the "仅 bytes"
// behaviour the official contract forbids; this case was updated with the fix.
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
	if ok := client.SendRequestAttempt(Request{ID: "garbage-req", Method: "pane.report_agent"}, 100*time.Millisecond); ok {
		t.Fatal("expected SendRequestAttempt to return false for non-JSON bytes: delivery must be validated against the Herdr response envelope, not inferred from received bytes")
	}
}

// TestTransport_FragmentedResponseFrame is the regression for the response
// reader: Herdr's reply may reach the client in more than one TCP segment, so
// the response frame must be read completely (up to the newline) before it is
// validated. A frame that never completes must not be counted as a successful
// delivery. One behaviour branch (frame completeness), no extra cases.
func TestTransport_FragmentedResponseFrame(t *testing.T) {
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
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				reply := herdrSuccessReply(line)
				// Deliver the reply in two segments with a gap in between, so
				// the first read only sees half of the frame.
				half := len(reply) / 2
				_, _ = c.Write(reply[:half])
				time.Sleep(200 * time.Millisecond)
				_, _ = c.Write(reply[half:])
				time.Sleep(50 * time.Millisecond)
			}(conn)
		}
	}()

	client := NewClient(sockPath, "pane-fragment-test", nil, []time.Duration{500 * time.Millisecond})
	req := Request{ID: "fragment-req", Method: "pane.report_agent"}
	if !client.SendRequestAttempt(req, 500*time.Millisecond) {
		t.Fatal("expected a complete response frame that arrives in two segments to be accepted")
	}

	// A frame that never completes must not be counted as a delivery.
	truncPath := createTempSocket(t)
	truncListener, err := net.Listen("unix", truncPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer truncListener.Close()

	go func() {
		for {
			conn, err := truncListener.Accept()
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
				reply := herdrSuccessReply(line)
				// Only half of the frame is delivered and the connection is
				// held open: the frame never completes.
				_, _ = c.Write(reply[:len(reply)/2])
				time.Sleep(500 * time.Millisecond)
			}(conn)
		}
	}()

	truncClient := NewClient(truncPath, "pane-truncated-test", nil, []time.Duration{200 * time.Millisecond})
	if truncClient.SendRequestAttempt(req, 200*time.Millisecond) {
		t.Fatal("expected a truncated response frame not to be counted as a successful delivery")
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
	// idleHook, when set, is called by IsIdle before it answers. It lets a
	// test hold a handler inside the lock-free IsIdle() query and drive a
	// concurrent session shutdown in that window.
	idleHook func()
}

func (m *mockSessionProvider) Mode() string {
	return m.mode
}

func (m *mockSessionProvider) IsIdle() (bool, error) {
	if m.idleHook != nil {
		m.idleHook()
	}
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

				_, _ = conn.Write(herdrSuccessReply(line))
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

					_, _ = c.Write(herdrSuccessReply(line))
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
