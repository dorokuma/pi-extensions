package moshihooks

// Test harness: a frozen clock, a clean environment, a recording sender and a
// per-test reset of the process-level caches the extension keeps.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// frozenNow is the timestamp every harness envelope carries; it pins both
// requestedAt and the second-resolution state machine.
var frozenNow = time.Date(2026, 10, 25, 12, 34, 56, 789000000, time.UTC)

// clearEnvKeys are every variable detection reads. A test starts from "unset"
// so an ambient tmux / herdr / zellij pane cannot change the expected envelope.
// The pig session-root variables are cleared too: pigSessionsRoot must resolve
// only from what the test itself sets.
var clearEnvKeys = append([]string{
	"MOSHI_SOCKET_PATH",
	"TMUX", "TMUX_PANE",
	"HERDR_ENV", "HERDR_SESSION", "HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID", "HERDR_SOCKET_PATH",
	"ZELLIJ", "ZELLIJ_SESSION_NAME", "ZELLIJ_PANE_ID",
	"PIG_EXT_HOST_INFO", "PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_HOME", "XDG_CONFIG_HOME",
}, MuxEnvKeys...)

// newRecordingSender returns a Sender that records envelopes instead of dialing
// the daemon, plus a reader for what it has recorded so far.
func newRecordingSender(t *testing.T) (*Sender, func() []*Envelope) {
	t.Helper()
	var mu sync.Mutex
	recorded := []*Envelope{}
	sender := newSender(func() string { return "" }, func(envelope *Envelope, _ string, _ time.Duration) {
		mu.Lock()
		recorded = append(recorded, envelope)
		mu.Unlock()
	}, SendTimeout)
	t.Cleanup(func() { sender.Flush(2 * time.Second) })
	return sender, func() []*Envelope {
		mu.Lock()
		defer mu.Unlock()
		out := make([]*Envelope, len(recorded))
		copy(out, recorded)
		return out
	}
}

// harness wires one test's clock, environment, caches and sender.
type harness struct {
	t        *testing.T
	sender   *Sender
	recorded func() []*Envelope
	clock    *time.Time
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	for _, key := range clearEnvKeys {
		t.Setenv(key, "")
	}
	// Isolate pigSessionsRoot from the host's ~/.pig/agent/sessions: the
	// highest-priority source points at a throwaway agent dir, so
	// transcript-resolution fallbacks can never touch real session files.
	t.Setenv("PIG_EXT_HOST_INFO", fmt.Sprintf(`{"agentDir":%q}`, t.TempDir()))
	for key, value := range env {
		t.Setenv(key, value)
	}
	clock := frozenNow
	now = func() time.Time { return clock }
	ResetTerminalContextForTest()
	ResetSessionsForTest()
	sender, recorded := newRecordingSender(t)
	SetSenderForTest(sender)
	t.Cleanup(func() {
		ResetSenderForTest()
		now = time.Now
		ResetTerminalContextForTest()
		ResetSessionsForTest()
	})
	return &harness{t: t, sender: sender, recorded: recorded, clock: &clock}
}

// advance moves the harness clock forward.
func (h *harness) advance(d time.Duration) { *h.clock = h.clock.Add(d) }

// envelopes drains the sender and returns everything recorded so far.
func (h *harness) envelopes() []*Envelope {
	h.t.Helper()
	h.sender.Flush(2 * time.Second)
	return h.recorded()
}

// deliver runs one payload through handleEvent and returns the envelopes it
// produced.
func (h *harness) deliver(payload *HookPayload) []*Envelope {
	h.t.Helper()
	before := len(h.recorded())
	handleEvent(payload, nil)
	all := h.envelopes()
	if len(all) < before {
		h.t.Fatalf("recorded envelopes shrank from %d to %d", before, len(all))
	}
	return all[before:]
}

// deliverOne is deliver with the "exactly one envelope" assertion.
func (h *harness) deliverOne(payload *HookPayload) *Envelope {
	h.t.Helper()
	envelopes := h.deliver(payload)
	if len(envelopes) != 1 {
		h.t.Fatalf("%s: got %d envelopes, want 1", payload.HookEventName, len(envelopes))
	}
	return envelopes[0]
}

// deliverNothing asserts the payload produces no envelope at all.
func (h *harness) deliverNothing(payload *HookPayload) {
	h.t.Helper()
	if envelopes := h.deliver(payload); len(envelopes) != 0 {
		h.t.Fatalf("%s: got %d envelopes, want 0 (first %+v)", payload.HookEventName, len(envelopes), envelopes[0])
	}
}

// payload builds a payload for one event name with the harness's fixed session.
func payloadFor(eventName string) *HookPayload {
	return &HookPayload{
		HookEventName: eventName,
		SessionID:     "session-1",
		Cwd:           "/work/moshi",
		Model:         "group/smartass",
	}
}

// writeTranscript writes a first-line session header and returns its path.
func writeTranscript(t *testing.T, header string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(file, []byte(header+"\n{\"type\":\"message\"}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return file
}
