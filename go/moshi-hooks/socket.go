package moshihooks

// Socket path resolution and the ordered, one-message-per-connection wire
// protocol, ported from moshi-hooks.ts (L21-106, L795-836).
//
// One message per connection (see moshi-hook's internal/socket/server.go):
// dial, write one envelope as a JSON line, half-close, drain the daemon ack (so
// the daemon does not log broken pipes), close. Every failure is silent —
// hooks must never interrupt the user's turn when Moshi is absent.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

const (
	// SendTimeout caps one envelope's dial + write + drain
	// (TS: sendTimeoutMs, L52).
	SendTimeout = 250 * time.Millisecond

	// ShutdownFlushTimeout is how long session_shutdown waits for the queue to
	// drain (TS: flushSends(1000), L832).
	ShutdownFlushTimeout = 1000 * time.Millisecond

	// windowsPipePath is the named pipe the daemon listens on for Windows
	// (TS: resolveSocketPath, L31).
	windowsPipePath = `\\.\pipe\moshi-hook`
)

// ResolveSocketPath mirrors internal/config.SocketPath: the MOSHI_SOCKET_PATH
// override wins over the per-platform default so tests and dev daemons can
// point us at a throwaway socket (TS: resolveSocketPath, L24).
//
// On Linux the daemon listens under XDG_RUNTIME_DIR, but hook subprocesses can
// run without that variable (e.g. inside a herdr pane). When it is empty we
// probe /run/user/<uid>/moshi-hook.sock — the systemd-user default — before
// falling back to /tmp, so the envelope does not get silently dropped.
func ResolveSocketPath() string {
	if override := os.Getenv("MOSHI_SOCKET_PATH"); override != "" {
		return override
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, "Library", "Application Support", "Moshi", "moshi-hook.sock")
	case "windows":
		return windowsPipePath
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "moshi-hook.sock")
	}
	if path := runUserSocketPath(); path != "" {
		return path
	}
	return "/tmp/moshi-hook.sock"
}

// runUserSocketDir is the parent of the per-uid runtime directories probed
// when XDG_RUNTIME_DIR is empty. It is a variable so tests can point the
// probe at a temp directory instead of the real /run/user.
var runUserSocketDir = "/run/user"

// runUserSocketPath returns /run/user/<uid>/moshi-hook.sock when that file
// exists, else "".
func runUserSocketPath() string {
	path := filepath.Join(runUserSocketDir, strconv.Itoa(os.Getuid()), "moshi-hook.sock")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// deliverEnvelope performs one send attempt (TS: deliverEnvelope, L62).
//
// It never reports failure: a missing daemon, a refused connection, a write
// error and a daemon that does not ack within timeout are all the same silent
// no-op, because the hook must not interrupt the turn.
//
// Windows named pipes are not dialable through package net without a
// non-standard dependency, so on Windows the send is a documented no-op; the
// path resolver above still returns the pipe path. Linux and macOS — the
// targets of this port — use the Unix socket.
func deliverEnvelope(envelope *Envelope, sockPath string, timeout time.Duration) {
	if runtime.GOOS == "windows" || sockPath == "" {
		return
	}
	if envelope == nil {
		return
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// JSON.stringify does not escape <, > or &; keep the bytes identical.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return
	}

	// One deadline covers the whole attempt — dial, write and drain — so a stuck
	// daemon delays this attempt by at most timeout, exactly like the TS timer
	// that starts before createConnection resolves.
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", sockPath, timeout)
	if err != nil {
		return
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return
	}
	if _, err := conn.Write(buffer.Bytes()); err != nil {
		return
	}
	// Half-close: the daemon reads the single line to EOF, then acks.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}
	// Drain the ack until EOF or the deadline, then let Close tear the
	// connection down. A daemon that is slow to ack cannot hold the queue: the
	// deadline ends this attempt and the next envelope goes out.
	_, _ = io.Copy(io.Discard, conn)
}

// deliveryFunc sends one envelope to sockPath within timeout.
type deliveryFunc func(envelope *Envelope, sockPath string, timeout time.Duration)

// Sender delivers envelopes one at a time, in the order they were queued.
//
// The daemon handles connections concurrently, so a resolution sent on its own
// connection could otherwise land before the prompt it resolves. It writes
// session state first and acks after its other work, so a connection still
// unacked after SendTimeout is let go: a stuck daemon cannot hold back the rest
// (TS: sendQueue + inFlightSends, L51-53).
type Sender struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    []*Envelope
	pending  int
	sockPath func() string
	deliver  deliveryFunc
	timeout  time.Duration
}

// NewSender starts the ordered delivery worker the running extension uses.
func NewSender() *Sender {
	return newSender(ResolveSocketPath, deliverEnvelope, SendTimeout)
}

// newSender wires the endpoint, the delivery and the per-envelope cap. Tests
// inject a recording delivery so a run never needs a socket.
func newSender(sockPath func() string, deliver deliveryFunc, timeout time.Duration) *Sender {
	sender := &Sender{sockPath: sockPath, deliver: deliver, timeout: timeout}
	sender.cond = sync.NewCond(&sender.mu)
	go sender.run()
	return sender
}

// run delivers queued envelopes serially until the process exits.
func (s *Sender) run() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 {
			s.cond.Wait()
		}
		envelope := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()

		s.deliver(envelope, s.sockPath(), s.timeout)

		s.mu.Lock()
		s.pending--
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// Send queues one envelope and returns immediately
// (TS: sendEnvelope, L55).
func (s *Sender) Send(envelope *Envelope) {
	if envelope == nil {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, envelope)
	s.pending++
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Flush waits until every queued and in-flight envelope has been delivered (or
// has failed), capped at timeout (TS: flushSends, L97).
//
// session_shutdown awaits this: without it the process exits before the socket
// even connects and the session never ends in Moshi.
func (s *Sender) Flush(timeout time.Duration) {
	if timeout < 0 {
		timeout = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()
	for s.pending > 0 && time.Now().Before(deadline) {
		s.cond.Wait()
	}
}

// Pending reports how many envelopes are queued or in flight. Tests and the
// load drill use it; the TS keeps the same count in inFlightSends.
func (s *Sender) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

var (
	globalMu     sync.Mutex
	globalSender *Sender
)

// sender returns the process-wide sender, starting it on first use.
func sender() *Sender {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalSender == nil {
		globalSender = NewSender()
	}
	return globalSender
}

// SendEnvelope queues one envelope for ordered delivery
// (TS: sendEnvelope, L55).
func SendEnvelope(envelope *Envelope) { sender().Send(envelope) }

// FlushSends waits for the process-wide sender to drain
// (TS: flushSends, L97).
func FlushSends(timeout time.Duration) {
	globalMu.Lock()
	current := globalSender
	globalMu.Unlock()
	if current == nil {
		return
	}
	current.Flush(timeout)
}

// SetSenderForTest swaps the process-wide sender. Tests only.
func SetSenderForTest(replacement *Sender) {
	globalMu.Lock()
	globalSender = replacement
	globalMu.Unlock()
}

// ResetSenderForTest drops the process-wide sender so the next SendEnvelope
// starts a fresh queue against the real socket. Tests only.
func ResetSenderForTest() {
	globalMu.Lock()
	globalSender = nil
	globalMu.Unlock()
}
