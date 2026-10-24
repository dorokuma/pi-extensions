package moshihooks

// Local Unix-domain-socket integration tests: a throwaway daemon that speaks the
// same one-message-per-connection protocol as moshi-hook's
// internal/socket/server.go.

import (
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// connection is one daemon-side connection as it was observed.
type connection struct {
	payload  string
	cleanEOF bool
	elapsed  time.Duration
}

// testDaemon accepts one-message connections and records each of them.
type testDaemon struct {
	path string

	mu          sync.Mutex
	connections []connection
}

// startDaemon listens on a throwaway Unix socket. ackDelay delays the ack a
// daemon writes after reading the envelope to EOF.
func startDaemon(t *testing.T, ackDelay time.Duration) *testDaemon {
	t.Helper()
	path := filepath.Join(t.TempDir(), "moshi-hook.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	daemon := &testDaemon{path: path}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go daemon.serve(conn, ackDelay)
		}
	}()
	return daemon
}

func (d *testDaemon) serve(conn net.Conn, ackDelay time.Duration) {
	defer conn.Close()
	started := time.Now()
	payload, err := io.ReadAll(conn)
	elapsed := time.Since(started)

	d.mu.Lock()
	d.connections = append(d.connections, connection{payload: string(payload), cleanEOF: err == nil, elapsed: elapsed})
	d.mu.Unlock()

	if ackDelay > 0 {
		time.Sleep(ackDelay)
	}
	_, _ = conn.Write([]byte("ok\n"))
}

// snapshot returns the connections recorded so far.
func (d *testDaemon) snapshot() []connection {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]connection, len(d.connections))
	copy(out, d.connections)
	return out
}

// waitFor waits until at least count connections were recorded.
func (d *testDaemon) waitFor(t *testing.T, count int) []connection {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if connections := d.snapshot(); len(connections) >= count {
			return connections
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("recorded %d connections, want %d", len(d.snapshot()), count)
	return nil
}

func TestDeliveryWritesOneJSONLineAndHalfCloses(t *testing.T) {
	daemon := startDaemon(t, 0)
	envelope := baseEnvelope(&HookPayload{SessionID: "s-1", Cwd: "/work/moshi"}, "s-1", "SessionStart")

	deliverEnvelope(envelope, daemon.path, SendTimeout)

	connections := daemon.waitFor(t, 1)
	observed := connections[0]
	if !observed.cleanEOF {
		t.Fatal("the daemon did not see a clean half-close")
	}
	if observed.elapsed > SendTimeout {
		t.Fatalf("the daemon waited %v for EOF, want well under %v", observed.elapsed, SendTimeout)
	}
	if !strings.HasSuffix(observed.payload, "\n") {
		t.Fatalf("payload does not end with a newline: %q", observed.payload)
	}
	if lines := strings.Count(observed.payload, "\n"); lines != 1 {
		t.Fatalf("payload carries %d newlines, want exactly one line: %q", lines, observed.payload)
	}
	// Field order is the TS insertion order, so the base fields lead the line.
	if !strings.HasPrefix(observed.payload, `{"type":"session.update","source":"pi","sessionId":"s-1","eventName":"SessionStart"`) {
		t.Fatalf("payload starts with %q", observed.payload[:minInt(len(observed.payload), 120)])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(observed.payload, "\n")), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%q)", err, observed.payload)
	}
	if decoded["sessionId"] != "s-1" || decoded["projectName"] != "moshi" {
		t.Fatalf("decoded envelope = %v", decoded)
	}
	if _, ok := decoded["muxEnv"].(map[string]any); !ok {
		t.Fatalf("muxEnv = %v, want an object", decoded["muxEnv"])
	}
}

func TestDeliveryToASlowDaemonReleasesAfterTheTimeout(t *testing.T) {
	daemon := startDaemon(t, 600*time.Millisecond)

	started := time.Now()
	deliverEnvelope(&Envelope{Type: "session.update", SessionID: "s-1"}, daemon.path, SendTimeout)
	elapsed := time.Since(started)

	if elapsed < 200*time.Millisecond {
		t.Fatalf("delivery returned after %v, want to wait for the ack until the deadline", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("delivery returned after %v, want to be released by the 250ms deadline", elapsed)
	}
}

func TestDeliveryWithoutADaemonIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.sock")

	started := time.Now()
	deliverEnvelope(&Envelope{Type: "session.update"}, path, SendTimeout)
	elapsed := time.Since(started)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("a refused connection took %v", elapsed)
	}
}

func TestSenderDeliversOverTheSocketInOrder(t *testing.T) {
	daemon := startDaemon(t, 0)
	t.Setenv("MOSHI_SOCKET_PATH", daemon.path)
	t.Cleanup(ResetSenderForTest)
	ResetSenderForTest()

	sender := NewSender()
	for _, sessionID := range []string{"1", "2", "3", "4"} {
		sender.Send(baseEnvelope(&HookPayload{SessionID: sessionID}, sessionID, "SessionStart"))
	}
	sender.Flush(3 * time.Second)

	connections := daemon.waitFor(t, 4)
	if len(connections) != 4 {
		t.Fatalf("daemon recorded %d connections, want 4", len(connections))
	}
	for index, observed := range connections {
		want := `"sessionId":"` + string(rune('1'+index)) + `"`
		if !strings.Contains(observed.payload, want) {
			t.Fatalf("connection %d = %q, want %s", index, observed.payload, want)
		}
		if lines := strings.Count(observed.payload, "\n"); lines != 1 {
			t.Fatalf("connection %d carries %d lines, want 1", index, lines)
		}
	}
}

func TestFlushSendsDrainsTheSocketQueue(t *testing.T) {
	daemon := startDaemon(t, 0)
	t.Setenv("MOSHI_SOCKET_PATH", daemon.path)
	t.Cleanup(ResetSenderForTest)
	ResetSenderForTest()

	for _, eventName := range []string{"SessionStart", "UserPromptSubmit", "AgentEnd"} {
		SendEnvelope(baseEnvelope(&HookPayload{SessionID: "s-1"}, "s-1", eventName))
	}
	FlushSends(ShutdownFlushTimeout)

	connections := daemon.snapshot()
	if len(connections) != 3 {
		t.Fatalf("daemon recorded %d sessions, want 3 before FlushSends returned", len(connections))
	}
	for index, eventName := range []string{"SessionStart", "UserPromptSubmit", "AgentEnd"} {
		if !strings.Contains(connections[index].payload, `"eventName":"`+eventName+`"`) {
			t.Fatalf("connection %d = %q, want %s", index, connections[index].payload, eventName)
		}
	}
}

func TestFlushSendsWithoutASenderIsANoop(t *testing.T) {
	ResetSenderForTest()
	started := time.Now()
	FlushSends(ShutdownFlushTimeout)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("a sender-less FlushSends took %v", elapsed)
	}
}

func TestExtensionRegistersTheLifecycleHooks(t *testing.T) {
	extension := Extension()
	if extension == nil || extension.Name() != "moshi-hooks" {
		t.Fatalf("Extension() = %v", extension)
	}
}

// minInt keeps the payload slice bound readable in failure messages.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
