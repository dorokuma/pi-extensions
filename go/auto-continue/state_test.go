package autocontinue

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type notifyCall struct {
	message string
	level   string
}

type statusCall struct {
	key  string
	text string
}

// fakeHost implements Host without any sdk.Context.
type fakeHost struct {
	notifyCalls []notifyCall
	statusCalls []statusCall
	clearCalls  int

	idle       bool
	idleErr    error
	pending    bool
	pendingErr error
	editorText string
	editorErr  error

	sent    []string
	sendErr error

	terms   []func(string) TerminalInput
	termErr error
	unsubs  int
}

func newHost() *fakeHost { return &fakeHost{idle: true} }

func (h *fakeHost) Notify(message, level string) {
	h.notifyCalls = append(h.notifyCalls, notifyCall{message, level})
}

func (h *fakeHost) SetStatus(key, text string) {
	h.statusCalls = append(h.statusCalls, statusCall{key, text})
}

func (h *fakeHost) ClearStatus(string) { h.clearCalls++ }

func (h *fakeHost) IsIdle() (bool, error) { return h.idle, h.idleErr }

func (h *fakeHost) HasPendingMessages() (bool, error) { return h.pending, h.pendingErr }

func (h *fakeHost) GetEditorText() (string, error) { return h.editorText, h.editorErr }

func (h *fakeHost) SendUserMessage(content string) error {
	h.sent = append(h.sent, content)
	return h.sendErr
}

func (h *fakeHost) OnTerminalInput(handler func(string) TerminalInput) (func(), error) {
	if h.termErr != nil {
		return nil, h.termErr
	}
	h.terms = append(h.terms, handler)
	return func() { h.unsubs++ }, nil
}

func (h *fakeHost) lastNotify(t *testing.T) notifyCall {
	t.Helper()
	if len(h.notifyCalls) == 0 {
		t.Fatal("no notification was sent")
	}
	return h.notifyCalls[len(h.notifyCalls)-1]
}

func (h *fakeHost) notifyMessages() []string {
	out := make([]string, 0, len(h.notifyCalls))
	for _, call := range h.notifyCalls {
		out = append(out, call.message)
	}
	return out
}

type fakeTimer struct {
	fn       func()
	duration time.Duration
	stopped  bool
	fired    bool
}

func (t *fakeTimer) Stop() { t.stopped = true }

type fakeScheduler struct {
	timers []*fakeTimer
}

func (s *fakeScheduler) AfterFunc(d time.Duration, fn func()) Timer {
	timer := &fakeTimer{fn: fn, duration: d}
	s.timers = append(s.timers, timer)
	return timer
}

// pending returns the most recent armed (not stopped, not fired) timer with the
// given duration, or nil.
func (s *fakeScheduler) pending(d time.Duration) *fakeTimer {
	for i := len(s.timers) - 1; i >= 0; i-- {
		timer := s.timers[i]
		if timer.duration == d && !timer.stopped && !timer.fired {
			return timer
		}
	}
	return nil
}

func (s *fakeScheduler) count(d time.Duration) int {
	total := 0
	for _, timer := range s.timers {
		if timer.duration == d {
			total++
		}
	}
	return total
}

func (s *fakeScheduler) fire(timer *fakeTimer) {
	timer.fired = true
	timer.fn()
}

type fakeClock struct{ ms float64 }

func (c *fakeClock) now() time.Time { return time.UnixMilli(int64(c.ms)) }

func (c *fakeClock) advance(ms float64) { c.ms += ms }

const continueDelay = time.Duration(DefaultDelayMs) * time.Millisecond
const watchdogDelay = time.Duration(CompactionBusyMaxMs) * time.Millisecond

type harness struct {
	state *State
	host  *fakeHost
	sched *fakeScheduler
	clock *fakeClock
	path  string
}

// newHarness builds a State over a t.TempDir() preference file, a fake
// scheduler and a fake clock: no test touches ~/.pi, ~/.pig or sleeps.
func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	clock := &fakeClock{ms: 1_000_000}
	sched := &fakeScheduler{}
	path := t.TempDir() + "/auto-continue.json"
	state := NewState(Options{
		SettingsPath: path,
		Scheduler:    sched,
		Now:          clock.now,
		LookupEnv:    lookupFrom(env),
	})
	return &harness{state: state, host: newHost(), sched: sched, clock: clock, path: path}
}

func (h *harness) errorTurn(errText string) {
	h.state.HandleAgentEnd([]map[string]any{
		{"role": "user", "content": "do the thing"},
		{"role": "assistant", "stopReason": "error", "errorMessage": errText},
	})
}

// ── agent_settled: scheduling ────────────────────────────────────────────────

func TestAgentSettledSchedulesContinueOnTransientError(t *testing.T) {
	h := newHarness(t, nil)
	h.errorTurn("HTTP 502 bad gateway")

	h.state.HandleAgentSettled(h.host)

	timer := h.sched.pending(continueDelay)
	if timer == nil {
		t.Fatal("no continue timer was armed")
	}
	if len(h.host.statusCalls) != 1 || h.host.statusCalls[0] != (statusCall{StatusKey, "auto-continue 1/20"}) {
		t.Fatalf("status calls = %+v, want the 1/20 status", h.host.statusCalls)
	}
	want := "[auto-continue] http-502，2500ms 后自动继续（1/20）"
	if got := h.host.lastNotify(t); got != (notifyCall{want, "info"}) {
		t.Fatalf("notify = %+v, want %+v", got, notifyCall{want, "info"})
	}
	if len(h.host.sent) != 0 {
		t.Fatalf("continue was sent before the delay: %v", h.host.sent)
	}

	h.sched.fire(timer)

	if len(h.host.sent) != 1 || h.host.sent[0] != ContinuePrompt {
		t.Fatalf("sent = %v, want [%q]", h.host.sent, ContinuePrompt)
	}
	h.state.mu.Lock()
	streak, selfSending, timerSet := h.state.streak, h.state.selfSending, h.state.timer != nil
	h.state.mu.Unlock()
	if streak != 1 {
		t.Fatalf("streak = %d, want 1", streak)
	}
	if !selfSending {
		t.Fatal("selfSending was not set before sendUserMessage")
	}
	if timerSet {
		t.Fatal("timer field was not cleared when it fired")
	}
	if h.host.clearCalls == 0 {
		t.Fatal("status was not cleared after the send")
	}
}

func TestAgentSettledSkipsWhenDisabled(t *testing.T) {
	h := newHarness(t, map[string]string{"PI_AUTO_CONTINUE": "0"})
	h.errorTurn("HTTP 502")

	h.state.HandleAgentSettled(h.host)

	if got := h.sched.count(continueDelay); got != 0 {
		t.Fatalf("armed %d timers while disabled", got)
	}
	if len(h.host.notifyCalls) != 0 {
		t.Fatalf("notified while disabled: %v", h.host.notifyMessages())
	}
}

func TestAgentSettledBlockers(t *testing.T) {
	t.Run("not idle", func(t *testing.T) {
		h := newHarness(t, nil)
		h.host.idle = false
		h.errorTurn("HTTP 502")
		h.state.HandleAgentSettled(h.host)
		if got := h.sched.count(continueDelay); got != 0 {
			t.Fatalf("armed %d timers while busy", got)
		}
	})

	t.Run("idle check fails", func(t *testing.T) {
		h := newHarness(t, nil)
		h.host.idleErr = errors.New("transport")
		h.errorTurn("HTTP 502")
		h.state.HandleAgentSettled(h.host)
		if got := h.sched.count(continueDelay); got != 0 {
			t.Fatalf("armed %d timers on an isIdle error", got)
		}
	})

	t.Run("pending messages", func(t *testing.T) {
		h := newHarness(t, nil)
		h.host.pending = true
		h.errorTurn("HTTP 502")
		h.state.HandleAgentSettled(h.host)
		if got := h.sched.count(continueDelay); got != 0 {
			t.Fatalf("armed %d timers with queued messages", got)
		}
	})

	t.Run("editor draft", func(t *testing.T) {
		h := newHarness(t, nil)
		h.host.editorText = "  半截草稿 "
		h.errorTurn("HTTP 502")
		h.state.HandleAgentSettled(h.host)
		if got := h.sched.count(continueDelay); got != 0 {
			t.Fatalf("armed %d timers with a draft", got)
		}
		want := "[auto-continue] 检测到输入框有草稿，不自动继续"
		if got := h.host.lastNotify(t); got.message != want {
			t.Fatalf("notify = %q, want %q", got.message, want)
		}
	})

	t.Run("editor text unavailable", func(t *testing.T) {
		h := newHarness(t, nil)
		h.host.editorErr = errors.New("no editor")
		h.errorTurn("HTTP 502")
		h.state.HandleAgentSettled(h.host)
		if got := h.sched.count(continueDelay); got != 1 {
			t.Fatalf("armed %d timers, want 1: an unreadable editor is not a draft", got)
		}
	})
}

func TestTimerGenerationDedup(t *testing.T) {
	h := newHarness(t, nil)
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)
	first := h.sched.pending(continueDelay)
	if first == nil {
		t.Fatal("no timer armed")
	}

	// A new run cancels the pending timer (cancelPending bumps the generation).
	h.state.HandleAgentStart()
	if !first.stopped {
		t.Fatal("pending timer survived agent_start")
	}
	h.sched.fire(first)
	if len(h.host.sent) != 0 {
		t.Fatalf("a stale timer still sent: %v", h.host.sent)
	}
}

func TestMaxAttemptsCap(t *testing.T) {
	h := newHarness(t, map[string]string{"PI_AUTO_CONTINUE_MAX": "2"})
	h.errorTurn("HTTP 502")

	for round := 1; round <= 2; round++ {
		h.state.HandleAgentSettled(h.host)
		timer := h.sched.pending(continueDelay)
		if timer == nil {
			t.Fatalf("round %d did not arm a timer", round)
		}
		h.sched.fire(timer)
	}

	h.state.mu.Lock()
	streak := h.state.streak
	h.state.mu.Unlock()
	if streak != 2 {
		t.Fatalf("streak = %d, want 2", streak)
	}

	before := h.sched.count(continueDelay)
	h.state.HandleAgentSettled(h.host)
	if got := h.sched.count(continueDelay); got != before {
		t.Fatalf("cap did not stop scheduling: %d timers, want %d", got, before)
	}
	want := "[auto-continue] 连续 2 次自动继续已达上限（瞬时错误 / 早衰 stop 都计数），停手。可 /auto-continue 看状态，或自己发「继续」。"
	if got := h.host.lastNotify(t); got.message != want || got.level != "warning" {
		t.Fatalf("notify = %+v, want %q warning", got, want)
	}
}

// ── esc lock (R1-R4) ─────────────────────────────────────────────────────────

func TestAbortedTurnLatchesUserCancelled(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleAgentEnd([]map[string]any{
		{"role": "assistant", "stopReason": "aborted"},
	})

	h.state.HandleAgentSettled(h.host)

	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	streak := h.state.streak
	h.state.mu.Unlock()
	if !cancelled {
		t.Fatal("aborted turn did not latch userCancelled")
	}
	if streak != 0 {
		t.Fatalf("streak = %d, want 0", streak)
	}
	want := "[auto-continue] 已按 esc 中止：本轮不再自动继续，手动发消息或下一轮正常结束后恢复"
	if got := h.host.lastNotify(t); got.message != want {
		t.Fatalf("notify = %q, want %q", got.message, want)
	}

	// A later transient error must not schedule anything while the lock is held.
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)
	if got := h.sched.count(continueDelay); got != 0 {
		t.Fatalf("armed %d timers while esc-locked", got)
	}
}

func TestTurnEndAbortedLatchesImmediately(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleTurnEnd(h.host, map[string]any{"role": "assistant", "stopReason": "aborted"})
	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if !cancelled {
		t.Fatal("turn_end with aborted did not latch userCancelled")
	}
}

func TestNormalStopReleasesEscLock(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleTurnEnd(h.host, map[string]any{"role": "assistant", "stopReason": "aborted"})

	h.state.HandleAgentEnd([]map[string]any{
		{"role": "assistant", "stopReason": "stop", "content": textBlocksOf("任务完成。")},
	})
	h.state.HandleAgentSettled(h.host)

	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if cancelled {
		t.Fatal("a normal completion did not release the esc lock")
	}
	want := "[auto-continue] 上一轮已正常结束，自动继续已恢复"
	if got := h.host.lastNotify(t); got.message != want {
		t.Fatalf("notify = %q, want %q", got.message, want)
	}
}

func TestPrematureStopDoesNotReleaseEscLock(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleTurnEnd(h.host, map[string]any{"role": "assistant", "stopReason": "aborted"})

	// Evidence for gate A and B: a tool ran a second ago, the human wrote long ago.
	h.state.HandleAgentStart()
	h.state.HandleToolExecutionStart()
	h.state.mu.Lock()
	h.state.lastHumanUserTs = h.clock.ms - UserProgressStallMs - 10_000
	h.state.mu.Unlock()

	h.state.HandleAgentEnd([]map[string]any{thinkingOnly()})
	h.state.HandleAgentSettled(h.host)

	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if !cancelled {
		t.Fatal("a premature stop released the esc lock")
	}
	for _, message := range h.host.notifyMessages() {
		if strings.Contains(message, "自动继续已恢复") {
			t.Fatalf("unlock notice was sent: %q", message)
		}
	}
	if got := h.sched.count(continueDelay); got != 0 {
		t.Fatalf("armed %d timers while esc-locked on a premature stop", got)
	}
}

func TestPrematureStopContinues(t *testing.T) {
	h := newHarness(t, nil)
	// The human wrote 100s ago; the run started 100s ago; nothing else happened.
	h.state.HandleInput("interactive", "帮我干完这件事")
	h.clock.advance(100_000)
	h.state.HandleAgentStart()
	h.clock.advance(100_000)
	h.state.HandleAgentEnd([]map[string]any{thinkingOnly()})

	h.state.HandleAgentSettled(h.host)

	timer := h.sched.pending(continueDelay)
	if timer == nil {
		t.Fatalf("premature stop did not schedule a continue; notifications: %v", h.host.notifyMessages())
	}
	want := "[auto-continue] stop:premature-thinking-only（疑似早衰 stop：只有 thinking 没有正文），2500ms 后自动继续（1/20）"
	if got := h.host.lastNotify(t); got.message != want {
		t.Fatalf("notify = %q, want %q", got.message, want)
	}

	h.sched.fire(timer)
	if len(h.host.sent) != 1 || h.host.sent[0] != ContinuePrompt {
		t.Fatalf("sent = %v, want the continue prompt", h.host.sent)
	}
}

// ── compaction (R3) ──────────────────────────────────────────────────────────

func TestCompactionBlocksAndWatchdogUnlocks(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.state.HandleSessionBeforeCompact(h.host)

	h.state.mu.Lock()
	compacting := h.state.compacting
	h.state.mu.Unlock()
	if !compacting {
		t.Fatal("session_before_compact did not set the busy flag")
	}
	watchdog := h.sched.pending(watchdogDelay)
	if watchdog == nil {
		t.Fatal("no compaction watchdog was armed")
	}

	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)
	if got := h.sched.count(continueDelay); got != 0 {
		t.Fatalf("armed %d continue timers during compaction", got)
	}

	h.sched.fire(watchdog)
	h.state.mu.Lock()
	compacting = h.state.compacting
	h.state.mu.Unlock()
	if compacting {
		t.Fatal("the watchdog fired without unlocking the busy flag")
	}
	want := "[auto-continue] 压缩 busy 标志超过 300000ms 未清理，已自行解锁"
	if got := h.host.lastNotify(t); got.message != want || got.level != "warning" {
		t.Fatalf("notify = %+v, want %q warning", got, want)
	}
}

func TestCompactionEndStopsWatchdog(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.state.HandleSessionBeforeCompact(h.host)
	watchdog := h.sched.pending(watchdogDelay)
	if watchdog == nil {
		t.Fatal("no watchdog armed")
	}

	h.state.HandleSessionCompact()
	if !watchdog.stopped {
		t.Fatal("session_compact did not stop the watchdog")
	}
	notifies := len(h.host.notifyCalls)
	h.sched.fire(watchdog)
	if len(h.host.notifyCalls) != notifies {
		t.Fatalf("a cleared watchdog still notified: %v", h.host.notifyMessages())
	}
}

func TestCompactionStartCancelsTimerAndIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)
	timer := h.sched.pending(continueDelay)

	h.state.HandleSessionBeforeCompact(h.host)
	if !timer.stopped {
		t.Fatal("compaction start did not cancel the pending continue")
	}
	h.state.HandleSessionBeforeCompact(h.host)
	if got := h.sched.count(watchdogDelay); got != 1 {
		t.Fatalf("watchdogs armed = %d, want 1 (second start is a no-op)", got)
	}
}

func TestAgentStartClearsCompaction(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionBeforeCompact(h.host)
	h.state.HandleAgentStart()
	h.state.mu.Lock()
	compacting := h.state.compacting
	h.state.mu.Unlock()
	if compacting {
		t.Fatal("agent_start did not unlock the compaction flag")
	}
}

// ── bare ESC (R1/R2) ─────────────────────────────────────────────────────────

func TestTerminalInputNeedsSessionStart(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.state.HandleTerminalInput(BareEsc); got != (TerminalInput{}) {
		t.Fatalf("verdict before session_start = %+v, want zero", got)
	}
}

func TestTerminalInputIgnoresSequences(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)

	h.host.idle = false
	for _, data := range []string{"\x1b[A", "\x1bOP", "a", ""} {
		if got := h.state.HandleTerminalInput(data); got != (TerminalInput{}) {
			t.Fatalf("HandleTerminalInput(%q) = %+v, want zero", data, got)
		}
	}
	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if cancelled {
		t.Fatal("a key sequence latched userCancelled")
	}
}

func TestTerminalInputDuringTimerCancelsRoundOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)
	timer := h.sched.pending(continueDelay)
	if timer == nil {
		t.Fatal("no timer armed")
	}

	// R2: pi is idle with a pending timer, so esc only cancels this round.
	h.host.idle = true
	h.state.HandleTerminalInput(BareEsc)

	if !timer.stopped {
		t.Fatal("esc during the delay did not cancel the timer")
	}
	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if cancelled {
		t.Fatal("esc during the delay must NOT latch userCancelled")
	}
	want := "[auto-continue] 计时中按了 esc：已取消本轮自动继续"
	if got := h.host.lastNotify(t); got.message != want {
		t.Fatalf("notify = %q, want %q", got.message, want)
	}
}

func TestTerminalInputDuringRunLatchesOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.host.idle = false

	h.state.HandleTerminalInput(BareEsc)
	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if !cancelled {
		t.Fatal("esc during a run did not latch userCancelled")
	}
	first := len(h.host.notifyCalls)
	h.state.HandleTerminalInput(BareEsc)
	if len(h.host.notifyCalls) != first {
		t.Fatalf("the second esc notified again: %v", h.host.notifyMessages())
	}
}

func TestTerminalInputTreatsIdleErrorAsIdle(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.host.idleErr = errors.New("transport")

	h.state.HandleTerminalInput(BareEsc)

	h.state.mu.Lock()
	cancelled := h.state.userCancelled
	h.state.mu.Unlock()
	if cancelled {
		t.Fatal("an isIdle error was not treated as idle")
	}
}

func TestSessionStartWithoutTerminalCapability(t *testing.T) {
	h := newHarness(t, nil)
	h.host.termErr = errors.New("rpc mode")
	h.state.HandleSessionStart(h.host) // must not panic, must still reset
	h.state.mu.Lock()
	off := h.state.offTerminalInput
	h.state.mu.Unlock()
	if off != nil {
		t.Fatal("an unsubscribe was stored despite the registration failure")
	}
}

// ── session lifecycle ────────────────────────────────────────────────────────

func TestSessionStartResetsAndRegisters(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleAgentEnd([]map[string]any{{"role": "assistant", "stopReason": "error", "errorMessage": "HTTP 502"}})
	h.state.HandleAgentSettled(h.host)
	timer := h.sched.pending(continueDelay)
	h.sched.fire(timer)
	h.state.mu.Lock()
	h.state.userCancelled = true
	h.state.selfSending = true
	h.state.promptFromExtension = true
	h.state.lastToolUseTs = 5
	h.state.lastHumanUserTs = 5
	h.state.runStartTs = 5
	h.state.mu.Unlock()

	h.state.HandleSessionStart(h.host)

	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.streak != 0 || h.state.lastHTTPStatus != nil || h.state.lastMessages != nil {
		t.Fatalf("run state not reset: %+v", h.state)
	}
	if h.state.userCancelled || h.state.selfSending || h.state.promptFromExtension {
		t.Fatal("flags not reset")
	}
	if h.state.lastToolUseTs != 0 || h.state.lastHumanUserTs != 0 || h.state.runStartTs != 0 {
		t.Fatal("premature-stop timestamps not reset")
	}
	if len(h.host.terms) != 1 {
		t.Fatalf("terminal input registered %d times, want 1", len(h.host.terms))
	}
}

func TestSessionShutdownKeepsStreak(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(h.host)
	h.sched.fire(h.sched.pending(continueDelay))

	h.state.HandleSessionShutdown(h.host)

	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.streak != 1 {
		t.Fatalf("streak = %d, want 1: TS session_shutdown only cancels the timer", h.state.streak)
	}
	if h.state.userCancelled || h.state.selfSending || h.state.runStartTs != 0 {
		t.Fatal("shutdown did not reset the flags it owns")
	}
	if h.state.terminalHost != nil || h.state.offTerminalInput != nil {
		t.Fatal("terminal input was not unregistered")
	}
	if h.host.unsubs != 1 {
		t.Fatalf("unsubscribe called %d times, want 1", h.host.unsubs)
	}
}

func TestInputSourceDistinguishesExtensionFromHuman(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.state.mu.Lock()
	h.state.userCancelled = true
	h.state.mu.Unlock()

	// The extension's own continue: no unlock, no gate B refresh.
	h.state.HandleInput("extension", ContinuePrompt)
	h.state.mu.Lock()
	promptFromExtension, selfSending := h.state.promptFromExtension, h.state.selfSending
	stillLocked, lastUser := h.state.userCancelled, h.state.lastHumanUserTs
	h.state.mu.Unlock()
	if !promptFromExtension || !selfSending {
		t.Fatal("extension input did not set promptFromExtension/selfSending")
	}
	if !stillLocked || lastUser != 0 {
		t.Fatal("extension input counted as a manual message")
	}

	h.state.HandleBeforeAgentStart(ContinuePrompt, true)
	h.state.mu.Lock()
	selfSending = h.state.selfSending
	stillLocked = h.state.userCancelled
	h.state.mu.Unlock()
	if selfSending {
		t.Fatal("before_agent_start did not consume selfSending")
	}
	if !stillLocked {
		t.Fatal("before_agent_start of a self-continue released the esc lock")
	}

	// A human message releases the lock and starts gate B.
	h.clock.advance(1234)
	h.state.HandleInput("interactive", "我来了")
	h.state.mu.Lock()
	stillLocked = h.state.userCancelled
	lastUser = h.state.lastHumanUserTs
	h.state.mu.Unlock()
	if stillLocked {
		t.Fatal("a manual message did not release the esc lock")
	}
	if lastUser != h.clock.ms {
		t.Fatalf("lastHumanUserTs = %v, want %v", lastUser, h.clock.ms)
	}
}

func TestBeforeAgentStartHumanReleasesLock(t *testing.T) {
	h := newHarness(t, nil)
	h.state.mu.Lock()
	h.state.userCancelled = true
	h.state.mu.Unlock()

	h.state.HandleBeforeAgentStart("干点活", true)

	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.userCancelled {
		t.Fatal("a human prompt did not release the esc lock")
	}
}

func TestBeforeAgentStartWithoutPrompt(t *testing.T) {
	h := newHarness(t, nil)
	h.state.mu.Lock()
	h.state.userCancelled = true
	h.state.mu.Unlock()

	// A custom-message run carries no prompt; TS's `event.prompt?.trim()` is
	// undefined there, so it is treated as a manual start.
	h.state.HandleBeforeAgentStart("", false)

	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.userCancelled {
		t.Fatal("a prompt-less run did not release the esc lock")
	}
}

// ── provider status / tool activity ──────────────────────────────────────────

func TestAfterProviderResponseFeedsStatus(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleAfterProviderResponse(503.0)
	h.state.HandleAgentEnd([]map[string]any{
		{"role": "assistant", "stopReason": "error", "errorMessage": ""},
	})

	h.state.HandleAgentSettled(h.host)

	want := "[auto-continue] http-503，2500ms 后自动继续（1/20）"
	if got := h.host.lastNotify(t); got.message != want {
		t.Fatalf("notify = %q, want %q", got.message, want)
	}
}

func TestAfterProviderResponseIgnoresBadStatuses(t *testing.T) {
	for _, status := range []any{0.0, -1.0, "503", nil, map[string]any{}} {
		h := newHarness(t, nil)
		h.state.HandleAfterProviderResponse(status)
		h.state.mu.Lock()
		recorded := h.state.lastHTTPStatus
		h.state.mu.Unlock()
		if recorded != nil {
			t.Fatalf("status %v was recorded as %v", status, *recorded)
		}
	}
}

func TestToolExecutionTimestamps(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleToolExecutionStart()
	h.state.mu.Lock()
	start := h.state.lastToolUseTs
	h.state.mu.Unlock()
	if start != h.clock.ms {
		t.Fatalf("lastToolUseTs = %v, want %v", start, h.clock.ms)
	}

	h.clock.advance(500)
	h.state.HandleToolExecutionEnd()
	h.state.mu.Lock()
	end := h.state.lastToolUseTs
	h.state.mu.Unlock()
	if end != h.clock.ms {
		t.Fatalf("lastToolUseTs after end = %v, want %v", end, h.clock.ms)
	}
}

func TestTurnEndStreakHandling(t *testing.T) {
	t.Run("normal completion resets streak", func(t *testing.T) {
		h := newHarness(t, nil)
		h.state.mu.Lock()
		h.state.streak = 3
		h.state.mu.Unlock()
		h.state.HandleTurnEnd(h.host, map[string]any{"role": "assistant", "stopReason": "stop", "content": textBlocksOf("做完了。")})
		h.state.mu.Lock()
		defer h.state.mu.Unlock()
		if h.state.streak != 0 {
			t.Fatalf("streak = %d, want 0", h.state.streak)
		}
	})

	t.Run("error leaves streak untouched", func(t *testing.T) {
		h := newHarness(t, nil)
		h.state.mu.Lock()
		h.state.streak = 3
		h.state.mu.Unlock()
		h.state.HandleTurnEnd(h.host, map[string]any{"role": "assistant", "stopReason": "error"})
		h.state.mu.Lock()
		defer h.state.mu.Unlock()
		if h.state.streak != 3 {
			t.Fatalf("streak = %d, want 3", h.state.streak)
		}
	})

	t.Run("suspected premature stop keeps streak", func(t *testing.T) {
		h := newHarness(t, nil)
		h.state.mu.Lock()
		h.state.streak = 3
		h.state.mu.Unlock()
		h.state.HandleToolExecutionStart()
		h.state.mu.Lock()
		h.state.lastHumanUserTs = h.clock.ms - UserProgressStallMs - 1000
		h.state.mu.Unlock()
		h.state.HandleTurnEnd(h.host, thinkingOnly())
		h.state.mu.Lock()
		defer h.state.mu.Unlock()
		if h.state.streak != 3 {
			t.Fatalf("streak = %d, want 3 (premature stop continues the chain)", h.state.streak)
		}
	})

	t.Run("missing stopReason is ignored", func(t *testing.T) {
		h := newHarness(t, nil)
		h.state.mu.Lock()
		h.state.streak = 3
		h.state.mu.Unlock()
		h.state.HandleTurnEnd(h.host, map[string]any{"role": "assistant"})
		h.state.mu.Lock()
		defer h.state.mu.Unlock()
		if h.state.streak != 3 {
			t.Fatalf("streak = %d, want 3", h.state.streak)
		}
	})
}

// ── the /auto-continue command ───────────────────────────────────────────────

func TestCommandOff(t *testing.T) {
	for _, arg := range []string{"off", "OFF", " disable ", "0"} {
		t.Run(arg, func(t *testing.T) {
			h := newHarness(t, nil)
			h.errorTurn("HTTP 502")
			h.state.HandleAgentSettled(h.host)
			timer := h.sched.pending(continueDelay)

			if err := h.state.HandleCommand(h.host, arg); err != nil {
				t.Fatalf("HandleCommand(%q) = %v", arg, err)
			}
			if !timer.stopped {
				t.Fatal("off did not cancel the pending timer")
			}
			h.state.mu.Lock()
			enabled := h.state.enabled
			h.state.mu.Unlock()
			if enabled {
				t.Fatal("off left the extension enabled")
			}
			if got := h.host.lastNotify(t); got.message != "[auto-continue] 已关闭（下次新对话也会保持关闭）" {
				t.Fatalf("notify = %q", got.message)
			}
			if stored := loadPersist(h.path); stored.Enabled {
				t.Fatalf("off was not persisted: %+v", stored)
			}
		})
	}
}

func TestCommandOn(t *testing.T) {
	h := newHarness(t, map[string]string{"PI_AUTO_CONTINUE": "0"})
	h.state.mu.Lock()
	h.state.streak = 4
	h.state.userCancelled = true
	h.state.mu.Unlock()

	if err := h.state.HandleCommand(h.host, "on"); err != nil {
		t.Fatal(err)
	}

	h.state.mu.Lock()
	enabled, streak, cancelled := h.state.enabled, h.state.streak, h.state.userCancelled
	h.state.mu.Unlock()
	if !enabled || streak != 0 || cancelled {
		t.Fatalf("on left enabled/streak/cancelled = %v/%d/%v", enabled, streak, cancelled)
	}
	if got := h.host.lastNotify(t); got.message != "[auto-continue] 已开启，最多连续 20 次（新对话默认开）" {
		t.Fatalf("notify = %q", got.message)
	}
	if stored := loadPersist(h.path); !stored.Enabled {
		t.Fatal("on was not persisted")
	}
}

func TestCommandMax(t *testing.T) {
	cases := []struct {
		args       string
		wantMax    int
		wantNotify string
		wantLevel  string
	}{
		{args: "max 5", wantMax: 5, wantNotify: "[auto-continue] max=5", wantLevel: "info"},
		{args: "  MAX   7 ", wantMax: 7, wantNotify: "[auto-continue] max=7", wantLevel: "info"},
		{args: "max 3.9", wantMax: 3, wantNotify: "[auto-continue] max=3", wantLevel: "info"},
		{args: "max 1", wantMax: 1, wantNotify: "[auto-continue] max=1", wantLevel: "info"},
		{args: "max 20", wantMax: 20, wantNotify: "[auto-continue] max=20", wantLevel: "info"},
		{args: "max 0", wantMax: 20, wantNotify: "[auto-continue] max 需要 1–20", wantLevel: "warning"},
		{args: "max 21", wantMax: 20, wantNotify: "[auto-continue] max 需要 1–20", wantLevel: "warning"},
		{args: "max abc", wantMax: 20, wantNotify: "[auto-continue] max 需要 1–20", wantLevel: "warning"},
		{args: "max inf", wantMax: 20, wantNotify: "[auto-continue] max 需要 1–20", wantLevel: "warning"},
	}
	for _, tc := range cases {
		t.Run(tc.args, func(t *testing.T) {
			h := newHarness(t, nil)
			if err := h.state.HandleCommand(h.host, tc.args); err != nil {
				t.Fatal(err)
			}
			h.state.mu.Lock()
			maxAttempts := h.state.maxAttempts
			h.state.mu.Unlock()
			if maxAttempts != tc.wantMax {
				t.Fatalf("maxAttempts = %d, want %d", maxAttempts, tc.wantMax)
			}
			if got := h.host.lastNotify(t); got.message != tc.wantNotify || got.level != tc.wantLevel {
				t.Fatalf("notify = %+v, want %q %s", got, tc.wantNotify, tc.wantLevel)
			}
			if stored := loadPersist(h.path); stored.MaxAttempts != tc.wantMax {
				t.Fatalf("persisted maxAttempts = %d, want %d", stored.MaxAttempts, tc.wantMax)
			}
		})
	}
}

func TestCommandStatus(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		h := newHarness(t, nil)
		if err := h.state.HandleCommand(h.host, ""); err != nil {
			t.Fatal(err)
		}
		want := "[auto-continue] on  streak=0/20  delay=2500ms"
		if got := h.host.lastNotify(t); got.message != want || got.level != "info" {
			t.Fatalf("notify = %+v, want %q info", got, want)
		}
	})

	t.Run("disabled with flags", func(t *testing.T) {
		h := newHarness(t, map[string]string{"PI_AUTO_CONTINUE": "0"})
		if err := h.state.HandleCommand(h.host, "max 5"); err != nil {
			t.Fatal(err)
		}
		h.state.HandleSessionBeforeCompact(h.host)
		h.state.mu.Lock()
		h.state.streak = 1
		h.state.mu.Unlock()
		h.state.HandleTerminalInput(BareEsc) // no terminal host bound: ignored
		h.state.mu.Lock()
		h.state.userCancelled = true
		h.state.mu.Unlock()

		if err := h.state.HandleCommand(h.host, "status"); err != nil {
			t.Fatal(err)
		}
		want := "[auto-continue] off  streak=1/5  delay=2500ms  compacting（压缩中，不调度）  esc-cancelled（手动发消息 / 正常结束 / on 后恢复）"
		if got := h.host.lastNotify(t); got.message != want {
			t.Fatalf("notify = %q, want %q", got.message, want)
		}
	})

	t.Run("non command text falls through to status", func(t *testing.T) {
		h := newHarness(t, nil)
		if err := h.state.HandleCommand(h.host, "maximum 5"); err != nil {
			t.Fatal(err)
		}
		if got := h.host.lastNotify(t); !strings.HasPrefix(got.message, "[auto-continue] on  streak=") {
			t.Fatalf("notify = %q, want the status line", got.message)
		}
	})
}

func TestCommandMaxPersistsAcrossRestart(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.state.HandleCommand(h.host, "max 7"); err != nil {
		t.Fatal(err)
	}

	restarted := NewState(Options{
		SettingsPath: h.path,
		Scheduler:    &fakeScheduler{},
		Now:          h.clock.now,
		LookupEnv:    lookupFrom(nil),
	})
	if restarted.maxAttempts != 7 {
		t.Fatalf("maxAttempts after restart = %d, want 7", restarted.maxAttempts)
	}
}

func TestCommandOffPersistsAcrossRestart(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.state.HandleCommand(h.host, "off"); err != nil {
		t.Fatal(err)
	}

	restarted := NewState(Options{
		SettingsPath: h.path,
		Scheduler:    &fakeScheduler{},
		Now:          h.clock.now,
		LookupEnv:    lookupFrom(nil),
	})
	if restarted.enabled {
		t.Fatal("restart re-enabled a disabled extension")
	}
}

// ── timer-callback / bare-ESC interleavings ──────────────────────────────────

// hostGate holds the first call of one host method until the test releases it,
// so a test can park the timer callback inside a chosen host pre-check. Later
// calls pass straight through: esc itself must never be gated.
type hostGate struct {
	entered chan struct{}
	release chan struct{}
	used    atomic.Bool
}

func newHostGate() *hostGate {
	return &hostGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *hostGate) hold() {
	if g == nil || !g.used.CompareAndSwap(false, true) {
		return
	}
	g.entered <- struct{}{}
	<-g.release
}

// gatedHost is a Host for the interleaving tests. Its recordings are guarded by
// its own mutex: the timer callback runs on a real goroutine while the test
// drives esc from another one.
type gatedHost struct {
	mu       sync.Mutex
	notifies []notifyCall
	clears   int
	sent     []string
	idle     bool
	terms    []func(string) TerminalInput

	idleGate    *hostGate
	pendingGate *hostGate
	editorGate  *hostGate
	sendGate    *hostGate
}

func newGatedHost() *gatedHost { return &gatedHost{idle: true} }

func (h *gatedHost) Notify(message, level string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifies = append(h.notifies, notifyCall{message, level})
}

func (h *gatedHost) SetStatus(string, string) {}

func (h *gatedHost) ClearStatus(string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clears++
}

func (h *gatedHost) IsIdle() (bool, error) {
	h.idleGate.hold()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.idle, nil
}

func (h *gatedHost) HasPendingMessages() (bool, error) {
	h.pendingGate.hold()
	return false, nil
}

func (h *gatedHost) GetEditorText() (string, error) {
	h.editorGate.hold()
	return "", nil
}

func (h *gatedHost) SendUserMessage(content string) error {
	h.sendGate.hold()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, content)
	return nil
}

func (h *gatedHost) OnTerminalInput(handler func(string) TerminalInput) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.terms = append(h.terms, handler)
	return func() {}, nil
}

func (h *gatedHost) setIdle(idle bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idle = idle
}

func (h *gatedHost) sendCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sent)
}

func (h *gatedHost) notifyMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.notifies))
	for _, call := range h.notifies {
		out = append(out, call.message)
	}
	return out
}

func (h *gatedHost) hasNotify(substr string) bool {
	for _, message := range h.notifyMessages() {
		if strings.Contains(message, substr) {
			return true
		}
	}
	return false
}

// armContinue registers the esc handler, feeds a transient-error turn and lets
// agent_settled arm the continue timer.
func armContinue(t *testing.T) (*harness, *gatedHost, *fakeTimer) {
	t.Helper()
	h := newHarness(t, nil)
	host := newGatedHost()
	h.state.HandleSessionStart(host)
	h.errorTurn("HTTP 502")
	h.state.HandleAgentSettled(host)
	timer := h.sched.pending(continueDelay)
	if timer == nil {
		t.Fatalf("no continue timer was armed; notifications: %v", host.notifyMessages())
	}
	return h, host, timer
}

// T1: an already-fired but parked callback of an old generation must not clear
// the new generation's timer (F1) — otherwise esc finds no timer, treats pi as
// idle and the new timer still sends.
func TestStaleTimerCallbackLeavesNewTimerAlone(t *testing.T) {
	h, host, first := armContinue(t)

	// The first timer has fired, but its callback has not reached the state
	// machine yet. Meanwhile the generation is replaced (agent_start,
	// session_start and compaction start all do exactly this) and the next
	// attempt arms T2.
	resume := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-resume
		first.fn()
	}()

	h.state.cancelPending()
	h.state.HandleAgentSettled(host)
	second := h.sched.pending(continueDelay)
	if second == nil {
		t.Fatal("the second attempt armed no timer")
	}
	h.state.mu.Lock()
	secondToken := h.state.generation
	h.state.mu.Unlock()

	close(resume)
	<-done

	h.state.mu.Lock()
	kept, streak, keptToken := h.state.timer, h.state.streak, h.state.generation
	h.state.mu.Unlock()
	if kept != second {
		t.Fatalf("the stale callback replaced the current timer (state.timer = %v)", kept)
	}
	if second.stopped {
		t.Fatal("the stale callback stopped the current timer")
	}
	if keptToken != secondToken {
		t.Fatalf("the stale callback moved the generation from %d to %d", secondToken, keptToken)
	}
	if streak != 0 {
		t.Fatalf("the stale callback moved the streak to %d", streak)
	}
	if got := host.sendCount(); got != 0 {
		t.Fatalf("the stale callback sent %d message(s)", got)
	}

	// esc must still see T2 as pending: it stops it and invalidates the
	// generation, and the queued callback then sends nothing.
	h.state.HandleTerminalInput(BareEsc)
	if !second.stopped {
		t.Fatal("esc did not stop the current timer")
	}
	h.state.mu.Lock()
	timerAfter, generationAfter := h.state.timer, h.state.generation
	h.state.mu.Unlock()
	if timerAfter != nil {
		t.Fatalf("esc left the timer reference set: %v", timerAfter)
	}
	if generationAfter == secondToken {
		t.Fatalf("esc did not invalidate the generation (%d)", generationAfter)
	}
	if !host.hasNotify("已取消本轮自动继续") {
		t.Fatalf("esc did not report the cancellation: %v", host.notifyMessages())
	}

	second.fn()
	if got := host.sendCount(); got != 0 {
		t.Fatalf("a cancelled continue still sent %d message(s)", got)
	}
	h.state.mu.Lock()
	streak = h.state.streak
	h.state.mu.Unlock()
	if streak != 0 {
		t.Fatalf("the cancelled callback moved the streak to %d", streak)
	}
}

// T2: esc while the continue is still cancellable must cancel it, whether its
// callback has not started or is parked inside one of the three host pre-checks
// (F2). The callback must not send, must not grow the streak and must not
// report anything but the accurate cancellation.
func TestEscCancelsCallbackInHostPrechecks(t *testing.T) {
	cases := []struct {
		name  string
		start bool
		gate  func(*gatedHost) *hostGate
	}{
		{name: "timer armed, callback not started"},
		{
			name:  "callback parked in IsIdle",
			start: true,
			gate:  func(h *gatedHost) *hostGate { h.idleGate = newHostGate(); return h.idleGate },
		},
		{
			name:  "callback parked in GetEditorText",
			start: true,
			gate:  func(h *gatedHost) *hostGate { h.editorGate = newHostGate(); return h.editorGate },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, host, timer := armContinue(t)
			h.state.mu.Lock()
			tokenBefore := h.state.generation
			h.state.mu.Unlock()
			var done chan struct{}
			var gate *hostGate
			if tc.start {
				gate = tc.gate(host)
				done = make(chan struct{})
				go func() {
					defer close(done)
					timer.fn()
				}()
				<-gate.entered
			}

			h.state.HandleTerminalInput(BareEsc)

			if tc.start {
				close(gate.release)
				<-done
			} else {
				// The queued callback of the cancelled timer forced to run.
				timer.fn()
			}

			if got := host.sendCount(); got != 0 {
				t.Fatalf("the cancelled continue sent %d message(s)", got)
			}
			h.state.mu.Lock()
			streak, timerAfter, tokenAfter := h.state.streak, h.state.timer, h.state.generation
			h.state.mu.Unlock()
			if streak != 0 {
				t.Fatalf("streak = %d, want 0", streak)
			}
			if timerAfter != nil {
				t.Fatalf("the timer reference is still set: %v", timerAfter)
			}
			if tokenAfter == tokenBefore {
				t.Fatalf("esc did not invalidate the generation (%d)", tokenAfter)
			}
			if !tc.start && !timer.stopped {
				// Only a timer that has not fired yet can be stopped; the started
				// shapes have already consumed theirs (fireContinue clears it).
				t.Fatal("esc did not stop the armed timer")
			}
			if !host.hasNotify("已取消本轮自动继续") {
				t.Fatalf("esc did not report the cancellation: %v", host.notifyMessages())
			}
		})
	}
}

// T3: esc's cancellation and the callback's dispatch claim are one linearization
// point. Both orders are arranged deterministically: only one of "cancelled" and
// "sent" may be observed, and an already dispatched continue must not be
// reported as cancelled.
func TestEscVsDispatchClaimLinearization(t *testing.T) {
	t.Run("cancel wins", func(t *testing.T) {
		h, host, timer := armContinue(t)
		host.pendingGate = newHostGate()
		done := make(chan struct{})
		go func() {
			defer close(done)
			timer.fn()
		}()
		<-host.pendingGate.entered // the callback is inside its pre-checks

		h.state.HandleTerminalInput(BareEsc)
		cancelled := host.hasNotify("已取消本轮自动继续")

		close(host.pendingGate.release)
		<-done

		sent := host.sendCount()
		if cancelled && sent != 0 {
			t.Fatalf("a cancellation notice and a new continue were both observed (sent=%d)", sent)
		}
		if !cancelled {
			t.Fatalf("esc missed the pending continue: %v", host.notifyMessages())
		}
		if sent != 0 {
			t.Fatalf("the cancelled continue sent %d message(s)", sent)
		}
		h.state.mu.Lock()
		streak := h.state.streak
		h.state.mu.Unlock()
		if streak != 0 {
			t.Fatalf("streak = %d, want 0", streak)
		}
	})

	t.Run("send committed first", func(t *testing.T) {
		h, host, timer := armContinue(t)
		host.sendGate = newHostGate()
		done := make(chan struct{})
		go func() {
			defer close(done)
			timer.fn()
		}()
		<-host.sendGate.entered // the claim is taken, the host call is in flight

		host.setIdle(false)
		h.state.HandleTerminalInput(BareEsc)

		close(host.sendGate.release)
		<-done

		if host.hasNotify("已取消本轮自动继续") {
			t.Fatalf("esc reported a cancellation it did not achieve: %v", host.notifyMessages())
		}
		if got := host.sendCount(); got != 1 {
			t.Fatalf("the dispatched continue sent %d message(s), want 1", got)
		}
		if !host.hasNotify("已按 esc 中止") {
			t.Fatalf("esc neither cancelled nor latched the abort: %v", host.notifyMessages())
		}
	})
}
