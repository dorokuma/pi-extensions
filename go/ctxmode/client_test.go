package ctxmode

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ctxmode.test.ts:239.
func TestDisposeStreams(t *testing.T) {
	c := newClient()
	p := newFakeProc()
	p.autoExit = true
	c.disposeProc(p)
	if !reflect.DeepEqual(p.signals, []string{"terminated"}) {
		t.Fatal(p.signals)
	}
	if !p.in.closed || !p.out.closed || !p.err.closed {
		t.Fatal("stdio not closed")
	}
}

// ctxmode.test.ts:259.
func TestDisposeEscalates(t *testing.T) {
	t.Setenv("CTXMODE_DISPOSE_WAIT_MS", "50")
	c := newClient()
	p := newFakeProc()
	c.disposeProc(p)
	if !reflect.DeepEqual(p.signals, []string{"terminated", "killed"}) {
		t.Fatal(p.signals)
	}
}

// ctxmode.test.ts:271: Go readers are tied to process identity, not removable callbacks.
func TestDisposeOldStderrIsolated(t *testing.T) {
	c := newClient()
	p := newFakeProc()
	p.autoExit = true
	c.proc = p
	c.shutDownProc()
	c.mu.Lock()
	if c.proc == p {
		c.handleStderrChunkLocked("pollute-me\n")
	}
	c.mu.Unlock()
	if len(c.stderrBuffer) != 0 || !p.err.closed {
		t.Fatal("old stderr leaked")
	}
}

// ctxmode.test.ts:413.
func TestStartInitializeFailureReaped(t *testing.T) {
	_, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_FAIL_INIT", "1")
	c := withClient(t)
	requireError(t, c.Start(), "initialize refused")
	c.mu.Lock()
	proc, initialized := c.proc, c.initialized
	c.mu.Unlock()
	if proc != nil || initialized {
		t.Fatal("failed handshake retained process")
	}
	pid := 0
	for _, s := range strings.Fields(fileText(t, pids)) {
		pid = atoi(t, s)
	}
	if pid <= 0 {
		t.Fatal("missing pid")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child not reaped: %v", err)
	}
}

// ctxmode.test.ts:458. The wire request is written once and cleanup rejects it;
// a stopped client makes the reconnect attempt fail without replay/spawn.
func TestDisconnectDoesNotReplay(t *testing.T) {
	for _, boom := range []string{"ctxmode disconnected", "ctxmode not running"} {
		c, p := recordingClient()
		done := callAsync(c, context.Background(), "ctx_run", map[string]any{"action": "execute", "command": "echo hi"})
		waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
		c.mu.Lock()
		c.stopped = true
		entry := c.pending[1]
		delete(c.pending, 1)
		entry.done <- reply{err: errors.New(boom)}
		c.initialized = false
		c.proc = nil
		c.mu.Unlock()
		requireError(t, (<-done).err, boom)
		if len(p.in.snapshot()) != 1 {
			t.Fatal("tools/call replayed")
		}
	}
}

// ctxmode.test.ts:562.
func TestEscCancellation(t *testing.T) {
	c, p := recordingClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := callAsync(c, ctx, "ctx_run", map[string]any{"action": "execute", "argv": []string{"sleep", "300"}})
	waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
	if pendingCount(c) != 1 {
		t.Fatal("request not pending")
	}
	cancel()
	requireError(t, (<-done).err, "cancelled (user pressed esc)")
	writes := p.in.snapshot()
	if len(writes) != 2 {
		t.Fatal(writes)
	}
	req, notify := decodeRequest(t, writes[0]), decodeRequest(t, writes[1])
	if req["method"] != "tools/call" || notify["method"] != "notifications/cancelled" || notify["params"].(map[string]any)["requestId"] != req["id"] {
		t.Fatal(writes)
	}
	requireContains(t, notify["params"].(map[string]any)["reason"].(string), "esc")
	if pendingCount(c) != 0 {
		t.Fatal("pending survived Esc")
	}
}

// ctxmode.test.ts:601.
func TestClientTimeoutCancels(t *testing.T) {
	t.Setenv("CTXMODE_REQUEST_TIMEOUT_MS", "20")
	c, p := recordingClient()
	_, err := c.CallTool(context.Background(), "ctx_git", map[string]any{"action": "status"})
	requireError(t, err, "timed out after 20ms")
	writes := p.in.snapshot()
	if len(writes) != 2 {
		t.Fatal(writes)
	}
	notify := decodeRequest(t, writes[1])
	if notify["method"] != "notifications/cancelled" || notify["params"].(map[string]any)["requestId"] != float64(1) {
		t.Fatal(notify)
	}
	requireContains(t, notify["params"].(map[string]any)["reason"].(string), "client gave up after 20ms")
	if pendingCount(c) != 0 {
		t.Fatal("pending survived timeout")
	}
}

// ctxmode.test.ts:619.
func TestNormalResponseNoCancel(t *testing.T) {
	c, p := recordingClient()
	done := callAsync(c, context.Background(), "ctx_git", map[string]any{"action": "status"})
	waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
	respond(c, 1, "CLEAN")
	r := <-done
	if r.err != nil || r.result.Content[0].Text != "CLEAN" || len(p.in.snapshot()) != 1 {
		t.Fatal(r, p.in.snapshot())
	}
}

// ctxmode.test.ts:633.
func TestAlreadyCancelledNoWrites(t *testing.T) {
	c, p := recordingClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CallTool(ctx, "ctx_run", map[string]any{"action": "execute"})
	requireError(t, err, cancelledMessage)
	if len(p.in.snapshot()) != 0 || pendingCount(c) != 0 {
		t.Fatal("pre-cancel wrote request")
	}
}

// ctxmode.test.ts:648.
func TestLateAbortNoNotify(t *testing.T) {
	c, p := recordingClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := callAsync(c, ctx, "ctx_git", map[string]any{"action": "status"})
	waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
	respond(c, 1, "CLEAN")
	if r := <-done; r.err != nil || r.result.Content[0].Text != "CLEAN" {
		t.Fatal(r)
	}
	cancel()
	if len(p.in.snapshot()) != 1 {
		t.Fatal("late cancel notification")
	}
}

// ctxmode.test.ts:849.
func TestStopIsTerminal(t *testing.T) {
	_, pids := setupHelper(t)
	c := newClient()
	c.Stop()
	requireError(t, c.Start(), "was stopped")
	if countPids(t, pids) != 0 {
		t.Fatal("stopped client spawned")
	}
}

// ctxmode.test.ts:928.
func TestCrashAutoRestart(t *testing.T) {
	_, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_CRASH", "1")
	c := withClient(t)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if countPids(t, pids) != 1 {
		t.Fatal("initial spawn")
	}
	waitFor(t, func() bool { return countPids(t, pids) >= 2 })
	c.Stop()
	n := countPids(t, pids)
	time.Sleep(600 * time.Millisecond)
	if countPids(t, pids) != n {
		t.Fatal("restart after stop")
	}
}

// Additional handshake failure branch and in-handshake abort boundary.
func TestHandshakeListFailureAndAbort(t *testing.T) {
	recv, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_FAIL_LIST", "1")
	c := withClient(t)
	requireError(t, c.Start(), "list refused")
	for _, s := range strings.Fields(fileText(t, pids)) {
		if err := syscall.Kill(atoi(t, s), 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatal("list failure process survived", err)
		}
	}
	t.Setenv("CTXMODE_TEST_FAIL_LIST", "")
	t.Setenv("CTXMODE_TEST_HANG_INIT", "1")
	t.Setenv("CTXMODE_START_TIMEOUT_MS", "500")
	c = withClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := callAsync(c, ctx, "ctx_run", map[string]any{"action": "execute"})
	waitFor(t, func() bool { return strings.Count(fileText(t, recv), `"method":"initialize"`) >= 2 })
	cancel()
	// Handshake is not user-cancelled in TS. Its own timeout still sends cancel and reaps.
	requireError(t, (<-done).err, "initialize timed out after 500ms")
	if strings.Contains(fileText(t, recv), "tools/call") {
		t.Fatal("aborted during handshake emitted tool request")
	}
}

// P1-1: a handshake *timeout* must reap the child before Start returns, exactly
// like initialize/tools-list refusal. No waitFor here: the pid has to be gone the
// moment Start() returns, otherwise an async reaper would still be running.
func TestStartTimeoutReapsBeforeReturn(t *testing.T) {
	_, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_HANG_INIT", "1")
	t.Setenv("CTXMODE_START_TIMEOUT_MS", "300")
	c := withClient(t)
	requireError(t, c.Start(), "initialize timed out after 300ms")
	c.mu.Lock()
	proc, initialized := c.proc, c.initialized
	c.mu.Unlock()
	if proc != nil || initialized {
		t.Fatal("failed handshake retained process")
	}
	if countPids(t, pids) != 1 {
		t.Fatalf("spawn count %d", countPids(t, pids))
	}
	for _, s := range strings.Fields(fileText(t, pids)) {
		if err := syscall.Kill(atoi(t, s), 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("child survived Start's handshake timeout: %v", err)
		}
	}
}

// P1-2: Esc must not be hidden behind an in-flight stdin write. The fake stdin
// blocks inside Write, so a synchronous write under the state lock would keep the
// caller (and the pending bookkeeping) stuck until the pipe drains.
func TestCallToolCancelUnblocksBlockedWrite(t *testing.T) {
	c, p := recordingClient()
	p.in.entered = make(chan struct{})
	p.in.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := callAsync(c, ctx, "ctx_run", map[string]any{"action": "execute", "argv": []string{"sleep", "300"}})
	select {
	case <-p.in.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request write never started")
	}
	cancel()
	select {
	case r := <-done:
		requireError(t, r.err, cancelledMessage)
	case <-time.After(2 * time.Second):
		t.Fatal("cancel blocked behind a blocked stdin write")
	}
	if pendingCount(c) != 0 {
		t.Fatal("pending survived Esc")
	}
	close(p.in.block)
	// The deferred notification still follows the request line once the pipe drains.
	waitFor(t, func() bool { return len(p.in.snapshot()) == 2 })
	writes := p.in.snapshot()
	req, notify := decodeRequest(t, writes[0]), decodeRequest(t, writes[1])
	if req["method"] != "tools/call" || notify["method"] != "notifications/cancelled" || notify["params"].(map[string]any)["requestId"] != req["id"] {
		t.Fatal(writes)
	}
	requireContains(t, notify["params"].(map[string]any)["reason"].(string), "esc")
}

// P1-2: the disconnect-reconnect handshake is a 15s-scale window; Esc during it
// must end the call instead of blocking on Start().
func TestReconnectHandshakeHonoursCancel(t *testing.T) {
	recv, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_HANG_INIT", "1")
	t.Setenv("CTXMODE_START_TIMEOUT_MS", "2000")
	c, p := recordingClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := callAsync(c, ctx, "ctx_run", map[string]any{"action": "execute"})
	waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
	c.mu.Lock()
	entry := c.pending[1]
	delete(c.pending, 1)
	c.initialized = false
	c.proc = nil
	c.mu.Unlock()
	entry.done <- reply{err: errors.New("ctxmode disconnected")}
	// The reconnect handshake is now in flight (the helper hangs on initialize).
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), "initialize") })
	start := time.Now()
	cancel()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("cancelled call waited %s for the reconnect handshake", elapsed)
		}
		requireError(t, r.err, cancelledMessage)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled call blocked on the reconnect handshake")
	}
	// The in-flight start still owns the child: it reaps it on its own timeout.
	waitFor(t, func() bool {
		for _, s := range strings.Fields(fileText(t, pids)) {
			if err := syscall.Kill(atoi(t, s), 0); !errors.Is(err, syscall.ESRCH) {
				return false
			}
		}
		return true
	})
}

// P2-1: CTXMODE_DISPOSE_WAIT_MS="0" is a number (TS "0" || DEFAULT is "0"), so
// the wait window is 0 and SIGKILL escalation happens at once.
func TestDisposeWaitZeroEscalates(t *testing.T) {
	t.Setenv("CTXMODE_DISPOSE_WAIT_MS", "0")
	c := newClient()
	p := newFakeProc()
	c.disposeProc(p)
	if !reflect.DeepEqual(p.signals, []string{"terminated", "killed"}) {
		t.Fatal(p.signals)
	}
}

// R3 P1-1: the ctx watcher must exit on its own once the call settles; a long
// run of normally-completing calls with a never-cancelled (but cancellable) ctx
// must not grow the goroutine count.
func TestCallToolNormalCompletionNoGoroutineLeak(t *testing.T) {
	c, p := recordingClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := runtime.NumGoroutine()
	for i := 1; i <= 25; i++ {
		done := callAsync(c, ctx, "ctx_git", map[string]any{"action": "status"})
		waitFor(t, func() bool { return len(p.in.snapshot()) == i })
		respond(c, i, "CLEAN")
		if r := <-done; r.err != nil {
			t.Fatal(r.err)
		}
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+2 })
}

// R3 P1-2: Esc during the reconnect handshake must dispose the in-flight child
// at once instead of leaving it to the 15s-scale handshake timeout.
func TestReconnectCancelDisposesHandshakeChild(t *testing.T) {
	recv, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_HANG_INIT", "1")
	t.Setenv("CTXMODE_START_TIMEOUT_MS", "15000")
	c, p := recordingClient()
	t.Cleanup(c.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := callAsync(c, ctx, "ctx_run", map[string]any{"action": "execute"})
	waitFor(t, func() bool { return len(p.in.snapshot()) == 1 })
	c.mu.Lock()
	entry := c.pending[1]
	delete(c.pending, 1)
	c.initialized = false
	c.proc = nil
	c.mu.Unlock()
	entry.done <- reply{err: errors.New("ctxmode disconnected")}
	// The reconnect handshake is now in flight (the helper hangs on initialize).
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), "initialize") })
	cancel()
	select {
	case r := <-done:
		requireError(t, r.err, cancelledMessage)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled reconnect did not return")
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if handshakeChildGone(t, pids) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("in-flight handshake child survived the cancel")
}

func handshakeChildGone(t *testing.T, pids string) bool {
	t.Helper()
	for _, s := range strings.Fields(fileText(t, pids)) {
		if err := syscall.Kill(atoi(t, s), 0); !errors.Is(err, syscall.ESRCH) {
			return false
		}
	}
	return true
}

// R3 P2: the two timeout knobs ignore non-positive values (a 0ms budget would
// abort the handshake or every call before the peer can answer); the output caps
// keep honouring "0" per TS Number semantics.
func TestTimeoutKnobsIgnoreNonPositive(t *testing.T) {
	_, pids := setupHelper(t)
	t.Setenv("CTXMODE_START_TIMEOUT_MS", "0")
	c := withClient(t)
	if err := c.Start(); err != nil {
		t.Fatalf("CTXMODE_START_TIMEOUT_MS=0 aborted the handshake: %v", err)
	}
	if countPids(t, pids) != 1 {
		t.Fatalf("spawn count %d", countPids(t, pids))
	}
	for _, v := range []string{"0", "-1"} {
		t.Setenv("CTXMODE_REQUEST_TIMEOUT_MS", v)
		if got := timeoutForTool("ctx_git", map[string]any{"action": "status"}); got != 120000 {
			t.Fatalf("request timeout for %q = %d, want 120000", v, got)
		}
	}
	t.Setenv("CTXMODE_OUTPUT_CHARS", "0")
	if got := envNumber("CTXMODE_OUTPUT_CHARS", 50000); got != 0 {
		t.Fatalf("char cap for \"0\" = %d, want 0", got)
	}
}

func TestStopDuringStart(t *testing.T) {
	recv, pids := setupHelper(t)
	t.Setenv("CTXMODE_TEST_HANG_INIT", "1")
	c := withClient(t)
	done := make(chan error, 1)
	go func() { done <- c.Start() }()
	waitFor(t, func() bool { return strings.Contains(fileText(t, recv), "initialize") })
	c.Stop()
	requireError(t, <-done, "disconnected")
	requireError(t, c.Start(), "was stopped")
	if countPids(t, pids) != 1 {
		t.Fatal("stop/start race spawned replacement")
	}
}
func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
