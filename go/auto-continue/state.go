package autocontinue

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// TerminalInput is the port's own verdict type for a raw terminal chunk, so the
// state machine stays testable without an sdk.Context. The extension adapter
// maps it onto sdk.TerminalInputResult (extension.go).
type TerminalInput struct {
	// Consume stops the chunk from reaching later handlers and the editor.
	Consume bool
	// Data, when non-nil, replaces the chunk.
	Data *string
}

// Host is the slice of the extension context the state machine needs. The
// extension adapter implements it over sdk.Context; tests implement it with a
// fake.
type Host interface {
	Notify(message, level string)
	SetStatus(key, text string)
	ClearStatus(key string)
	IsIdle() (bool, error)
	HasPendingMessages() (bool, error)
	GetEditorText() (string, error)
	SendUserMessage(content string) error
	OnTerminalInput(handler func(data string) TerminalInput) (func(), error)
}

// Timer is the subset of *time.Timer the state machine uses.
type Timer interface {
	Stop()
}

// Scheduler arms the state machine's timers. The default is *time.AfterFunc;
// tests inject a deterministic fake so no test sleeps.
type Scheduler interface {
	AfterFunc(d time.Duration, fn func()) Timer
}

type realScheduler struct{}

// realTimer adapts *time.Timer (whose Stop returns bool) to [Timer].
type realTimer struct{ timer *time.Timer }

func (t realTimer) Stop() { t.timer.Stop() }

func (realScheduler) AfterFunc(d time.Duration, fn func()) Timer {
	return realTimer{timer: time.AfterFunc(d, fn)}
}

// Options configures a State. Every field is optional; nil picks the production
// behaviour, which is also what the extension factory uses.
type Options struct {
	// SettingsPath overrides where the preference file lives. Tests pass a
	// t.TempDir() path; empty resolves [SettingsPath].
	SettingsPath string
	// Scheduler arms the continue timer and the compaction watchdog.
	Scheduler Scheduler
	// Now is the clock used for the premature-stop timestamps.
	Now func() time.Time
	// LookupEnv reads PI_AUTO_CONTINUE, PI_AUTO_CONTINUE_MAX and
	// PI_AUTO_CONTINUE_DELAY_MS. Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// continueState is the dispatch phase of the current generation's continue
// attempt. The esc path and the timer callback both read it under mu, so
// exactly one of "esc cancels this round" and "the callback dispatches" can
// win, and neither can report an outcome it did not achieve.
//
// TS's single thread makes the equivalent transition implicit: the closure's
// `timer` variable is set for the whole delay, and setTimeout's callback either
// runs before the esc handler or after it. Go's handlers run concurrently, so
// the phase makes the same ordering explicit:
//
//	continueIdle      — no attempt is armed for the current generation.
//	continueArmed     — the generation was bumped; the timer is being armed.
//	continueChecking  — the callback runs its host pre-checks; still cancellable.
//	continueCommitted— the callback took the dispatch claim: the send happens,
//	                    and a later esc must not claim it cancelled the round.
//
// A callback whose token is no longer the current generation must not touch any
// of this: an already-fired-but-parked stale callback would otherwise clear the
// new generation's timer (F1) and leave esc unable to cancel it.
type continueState uint8

const (
	continueIdle continueState = iota
	continueArmed
	continueChecking
	continueCommitted
)

// State is the port of the closure state in TS's default export
// (auto-continue.ts:632-1114): the enable flags, the streak, the pending timer,
// the esc lock, and the premature-stop timestamps.
//
// The Go SDK runs handlers concurrently (docs/extension-api-parity.md: "The Go,
// Rust and Python SDKs run handlers concurrently"), so every field is guarded by
// mu. Host calls are made after releasing mu, so a blocking UI call cannot
// deadlock the timer callback.
type State struct {
	mu sync.Mutex

	// configuration (auto-continue.ts:634-637)
	enabled      bool
	maxAttempts  int
	delayMs      int
	settingsPath string
	sched        Scheduler
	now          func() time.Time
	lookupEnv    func(string) (string, bool)

	// run state (auto-continue.ts:643-687)
	streak              int
	lastHTTPStatus      *float64
	lastMessages        []map[string]any
	timer               Timer
	generation          int64
	continuePhase       continueState
	compacting          bool
	compactWatchdog     Timer
	offTerminalInput    func()
	terminalHost        Host
	userCancelled       bool
	lastToolUseTs       float64
	lastHumanUserTs     float64
	runStartTs          float64
	selfSending         bool
	promptFromExtension bool
}

// NewState mirrors TS's module-level initialisation (auto-continue.ts:633-637):
// load the persisted preferences, apply the environment overrides, and keep the
// result until a command changes it.
func NewState(opts Options) *State {
	state := &State{
		settingsPath: opts.SettingsPath,
		sched:        opts.Scheduler,
		now:          opts.Now,
		lookupEnv:    opts.LookupEnv,
	}
	if state.sched == nil {
		state.sched = realScheduler{}
	}
	if state.now == nil {
		state.now = time.Now
	}
	if state.lookupEnv == nil {
		state.lookupEnv = os.LookupEnv
	}
	if state.settingsPath == "" {
		state.settingsPath = SettingsPath()
	}

	persisted := loadPersist(state.settingsPath)
	state.enabled = persisted.Enabled
	if override := envEnabledOverride(state.lookupEnv); override != nil {
		state.enabled = *override
	}
	state.maxAttempts = envInt(state.lookupEnv, "PI_AUTO_CONTINUE_MAX", persisted.MaxAttempts, MinAttempts, MaxAttemptsLimit)
	state.delayMs = envInt(state.lookupEnv, "PI_AUTO_CONTINUE_DELAY_MS", persisted.DelayMs, 0, MaxDelayMs)
	return state
}

// nowMs is Date.now().
func (s *State) nowMs() float64 {
	return float64(s.now().UnixMilli())
}

// clearStatus mirrors TS clearStatus (auto-continue.ts:697-703): a missing host
// or a failed ui call is ignored.
func clearStatus(host Host) {
	if host == nil {
		return
	}
	host.ClearStatus(StatusKey)
}

// editorHasDraft mirrors TS editorHasDraft (auto-continue.ts:623-630): only a
// readable string counts, and any failure means "no draft".
func editorHasDraft(host Host) bool {
	if host == nil {
		return false
	}
	text, err := host.GetEditorText()
	if err != nil {
		return false
	}
	return jsTrim(text) != ""
}

// lastAssistant mirrors TS lastAssistant (auto-continue.ts:615-621).
func lastAssistant(messages []map[string]any) map[string]any {
	for i := len(messages) - 1; i >= 0; i-- {
		if roleOf(messages[i]) == "assistant" {
			return messages[i]
		}
	}
	return nil
}

// cancelPending mirrors TS cancelPending (auto-continue.ts:689-695): the
// generation bump makes an already-fired-but-unscheduled callback a no-op.
func (s *State) cancelPending() {
	s.mu.Lock()
	s.cancelPendingLocked()
	s.mu.Unlock()
}

func (s *State) cancelPendingLocked() {
	s.generation++
	s.continuePhase = continueIdle
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// judgeLocked mirrors TS judgePrematureStop (auto-continue.ts:712-718). Callers
// hold mu.
func (s *State) judgeLocked(message map[string]any, messages []map[string]any) Verdict {
	now := s.nowMs()
	return IsPrematureStop(message, PrematureStopFacts{
		Messages:          messages,
		LastToolUseTs:     &s.lastToolUseTs,
		LastUserMessageTs: &s.lastHumanUserTs,
		RunStartTs:        &s.runStartTs,
		Now:               &now,
	})
}

// markUserAbort mirrors TS markUserAbort (auto-continue.ts:728-748): the user
// aborted (esc) was observed, so cancel the pending timer, reset the streak and
// latch userCancelled. lastMessages is dropped because the aborted turn may have
// produced no assistant message at all, and keeping the previous error would let
// the next agent_settled continue it again.
func (s *State) markUserAbort(host Host) {
	s.mu.Lock()
	s.cancelPendingLocked()
	s.streak = 0
	s.lastHTTPStatus = nil
	s.lastMessages = nil
	first := !s.userCancelled
	s.userCancelled = true
	s.mu.Unlock()

	clearStatus(host)
	if !first {
		return
	}
	if host == nil {
		return
	}
	host.Notify(fmt.Sprintf("[%s] 已按 esc 中止：本轮不再自动继续，手动发消息或下一轮正常结束后恢复", LogName), "info")
}

// clearCompactWatchdogLocked mirrors TS clearCompactWatchdog
// (auto-continue.ts:751-756). Callers hold mu.
func (s *State) clearCompactWatchdogLocked() {
	if s.compactWatchdog != nil {
		s.compactWatchdog.Stop()
		s.compactWatchdog = nil
	}
}

// markCompactionStart mirrors TS markCompactionStart (auto-continue.ts:763-782).
// ctx.isIdle() cannot see compaction, so the local flag is the only guard against
// sending "继续" mid-compaction.
func (s *State) markCompactionStart(host Host) {
	s.mu.Lock()
	if s.compacting {
		s.mu.Unlock()
		return
	}
	s.compacting = true
	s.cancelPendingLocked()
	s.clearCompactWatchdogLocked()
	s.compactWatchdog = s.sched.AfterFunc(time.Duration(CompactionBusyMaxMs)*time.Millisecond, func() {
		s.compactionWatchdogFired(host)
	})
	s.mu.Unlock()
}

func (s *State) compactionWatchdogFired(host Host) {
	s.mu.Lock()
	s.compactWatchdog = nil
	if !s.compacting {
		s.mu.Unlock()
		return
	}
	s.compacting = false
	s.mu.Unlock()
	if host == nil {
		return
	}
	host.Notify(fmt.Sprintf("[%s] 压缩 busy 标志超过 %dms 未清理，已自行解锁", LogName, CompactionBusyMaxMs), "warning")
}

// markCompactionEnd mirrors TS markCompactionEnd (auto-continue.ts:784-787).
func (s *State) markCompactionEnd() {
	s.mu.Lock()
	s.markCompactionEndLocked()
	s.mu.Unlock()
}

func (s *State) markCompactionEndLocked() {
	s.compacting = false
	s.clearCompactWatchdogLocked()
}

// HandleTerminalInput mirrors TS handleTerminalInput (auto-continue.ts:804-839).
// A bare ESC is never consumed: pi's own ESC semantics (abort retry / abort
// compaction / abort run) must keep working, this only cleans up our pending
// state.
func (s *State) HandleTerminalInput(data string) TerminalInput {
	if data != BareEsc {
		return TerminalInput{}
	}
	s.mu.Lock()
	host := s.terminalHost
	if host == nil {
		s.mu.Unlock()
		return TerminalInput{}
	}
	// R2: a pending continue while pi is idle — cancel this round only, do NOT
	// latch userCancelled (the user still wants later errors to continue).
	//
	// The observation and the invalidation are one critical section. The timer
	// callback takes its dispatch claim under the same mutex, so esc and the send
	// cannot both win, and the callback keeps its cancellable identity while it
	// waits inside a host pre-check (F2). A callback already past
	// continueCommitted is not pending here: its send goes out, and this esc must
	// not report a cancellation it did not achieve.
	if s.continuePhase == continueArmed || s.continuePhase == continueChecking {
		s.cancelPendingLocked()
		s.mu.Unlock()
		clearStatus(host)
		host.Notify(fmt.Sprintf("[%s] 计时中按了 esc：已取消本轮自动继续", LogName), "info")
		return TerminalInput{}
	}
	s.mu.Unlock()

	// TS wraps isIdle() in try/catch and treats a failure as idle.
	idle := true
	if value, err := host.IsIdle(); err == nil {
		idle = value
	}
	if idle {
		return TerminalInput{}
	}
	// A run is in flight (streaming / retry backoff / tool execution): esc means
	// the user wants to stop.
	s.markUserAbort(host)
	return TerminalInput{}
}

// registerTerminalInput mirrors TS registerTerminalInput
// (auto-continue.ts:841-857): every session_start re-registers, because a
// session replacement clears extension UI listeners.
func (s *State) registerTerminalInput(host Host) {
	s.mu.Lock()
	s.terminalHost = host
	off := s.offTerminalInput
	s.offTerminalInput = nil
	s.mu.Unlock()

	if off != nil {
		off()
	}
	if host == nil {
		return
	}
	unsubscribe, err := host.OnTerminalInput(s.HandleTerminalInput)
	if err != nil || unsubscribe == nil {
		// No raw-input capability: the main flow is unaffected.
		return
	}
	s.mu.Lock()
	s.offTerminalInput = unsubscribe
	s.mu.Unlock()
}

// unregisterTerminalInput mirrors TS unregisterTerminalInput
// (auto-continue.ts:859-868).
func (s *State) unregisterTerminalInput() {
	s.mu.Lock()
	s.terminalHost = nil
	off := s.offTerminalInput
	s.offTerminalInput = nil
	s.mu.Unlock()

	if off != nil {
		off()
	}
}

// HandleSessionStart mirrors TS's session_start handler
// (auto-continue.ts:871-887).
func (s *State) HandleSessionStart(host Host) {
	s.mu.Lock()
	s.cancelPendingLocked()
	s.streak = 0
	s.lastHTTPStatus = nil
	s.lastMessages = nil
	s.userCancelled = false
	s.selfSending = false
	s.promptFromExtension = false
	// The premature-stop timestamps are per session: a new / fork / switch /
	// reload must not inherit the previous session's activity.
	s.lastToolUseTs = 0
	s.lastHumanUserTs = 0
	s.runStartTs = 0
	s.markCompactionEndLocked()
	s.mu.Unlock()

	s.registerTerminalInput(host)
	clearStatus(host)
}

// HandleSessionShutdown mirrors TS's session_shutdown handler
// (auto-continue.ts:889-900).
func (s *State) HandleSessionShutdown(host Host) {
	s.mu.Lock()
	s.cancelPendingLocked()
	s.userCancelled = false
	s.selfSending = false
	s.promptFromExtension = false
	s.runStartTs = 0
	s.markCompactionEndLocked()
	s.mu.Unlock()

	s.unregisterTerminalInput()
	clearStatus(host)
}

// HandleSessionBeforeCompact mirrors TS (auto-continue.ts:902-904).
func (s *State) HandleSessionBeforeCompact(host Host) {
	s.markCompactionStart(host)
}

// HandleSessionCompact mirrors TS (auto-continue.ts:906-908).
func (s *State) HandleSessionCompact() {
	s.markCompactionEnd()
}

// HandleAgentStart mirrors TS's agent_start handler (auto-continue.ts:910-923).
func (s *State) HandleAgentStart() {
	s.mu.Lock()
	s.cancelPendingLocked()
	s.lastHTTPStatus = nil
	// The run's start: gate A's run-duration evidence counts from here.
	s.runStartTs = s.nowMs()
	// Only this run's assistant messages count. An aborted run may produce none,
	// and a stale previous error would be continued again.
	s.lastMessages = nil
	// A new run means compaction is over (fallback unlock).
	s.markCompactionEndLocked()
	s.selfSending = false
	s.mu.Unlock()
}

// HandleAfterProviderResponse mirrors TS (auto-continue.ts:925-929).
func (s *State) HandleAfterProviderResponse(status any) {
	value, ok := jsNumberOf(status)
	if !ok || value <= 0 {
		return
	}
	s.mu.Lock()
	s.lastHTTPStatus = &value
	s.mu.Unlock()
}

// HandleToolExecutionStart mirrors TS (auto-continue.ts:933-935).
func (s *State) HandleToolExecutionStart() {
	s.mu.Lock()
	s.lastToolUseTs = s.nowMs()
	s.mu.Unlock()
}

// HandleToolExecutionEnd mirrors TS (auto-continue.ts:937-939).
func (s *State) HandleToolExecutionEnd() {
	s.mu.Lock()
	s.lastToolUseTs = s.nowMs()
	s.mu.Unlock()
}

// HandleAgentEnd mirrors TS (auto-continue.ts:941-943).
func (s *State) HandleAgentEnd(messages []map[string]any) {
	s.mu.Lock()
	if messages == nil {
		s.lastMessages = nil
	} else {
		s.lastMessages = messages
	}
	s.mu.Unlock()
}

// HandleInput mirrors TS's input handler (auto-continue.ts:951-969): it tells
// the extension's own sendUserMessage apart from a message the user really sent.
func (s *State) HandleInput(source, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source == "extension" {
		s.promptFromExtension = true
		if jsTrim(text) == ContinuePrompt {
			s.selfSending = true
		}
		// The extension's own prompt is not a manual user message: it neither
		// clears the esc lock nor refreshes lastHumanUserTs, otherwise the
		// second shot of a continue chain would fail gate B.
		return
	}
	s.promptFromExtension = false
	s.selfSending = false
	// interactive / rpc: a real manual message releases the esc lock and marks
	// gate B's starting point.
	s.lastHumanUserTs = s.nowMs()
	s.userCancelled = false
}

// HandleBeforeAgentStart mirrors TS (auto-continue.ts:971-977).
func (s *State) HandleBeforeAgentStart(prompt string, hasPrompt bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	selfContinue := s.selfSending || s.promptFromExtension || (hasPrompt && jsTrim(prompt) == ContinuePrompt)
	s.selfSending = false
	if selfContinue {
		return
	}
	s.userCancelled = false
}

// HandleTurnEnd mirrors TS's turn_end handler (auto-continue.ts:979-996).
func (s *State) HandleTurnEnd(host Host, message map[string]any) {
	if message == nil || roleOf(message) != "assistant" || stopReasonOf(message) == "" {
		return
	}
	switch stopReasonOf(message) {
	case "aborted":
		s.markUserAbort(host)
		return
	case "error":
		return
	}
	// A suspected premature stop keeps the streak: the turn is taken into the
	// continue chain by agent_settled, and the streak must accumulate across
	// turns or maxAttempts would never apply.
	s.mu.Lock()
	premature := s.judgeLocked(message, nil)
	if !premature.OK {
		s.streak = 0
	}
	s.mu.Unlock()
}

// HandleAgentSettled mirrors TS's agent_settled handler
// (auto-continue.ts:998-1113).
func (s *State) HandleAgentSettled(host Host) {
	s.mu.Lock()
	enabled := s.enabled
	compacting := s.compacting
	s.mu.Unlock()
	if !enabled {
		return
	}
	// R3: no scheduling during compaction (isIdle() cannot tell), and the timer
	// callback checks the same flag.
	if compacting {
		clearStatus(host)
		return
	}
	if host == nil {
		return
	}

	s.mu.Lock()
	assistant := lastAssistant(s.lastMessages)
	messages := s.lastMessages
	unlocked := false
	// R4: a completed turn (stopReason === "stop") releases the esc lock, so the
	// lock spans "from esc to the next normal completion or manual input".
	// A suspected premature stop is NOT a normal completion: releasing there
	// would let the premature branch send "继续" after an esc.
	if s.userCancelled && stopReasonOf(assistant) == "stop" {
		if !s.judgeLocked(assistant, messages).OK {
			s.userCancelled = false
			unlocked = true
		}
	}
	cancelled := s.userCancelled
	s.mu.Unlock()

	if unlocked && host != nil {
		host.Notify(fmt.Sprintf("[%s] 上一轮已正常结束，自动继续已恢复", LogName), "info")
	}
	// esc already locked this round out: schedule nothing, whatever stale error
	// lastMessages still holds.
	if cancelled {
		clearStatus(host)
		return
	}

	if stopReasonOf(assistant) == "aborted" {
		// The turn was aborted: latch the permanent cancel so the old error does
		// not trigger another round.
		s.markUserAbort(host)
		return
	}

	s.mu.Lock()
	httpStatus := s.lastHTTPStatus
	s.mu.Unlock()

	decision := IsContinueWorthyError(assistant, httpStatus)
	if !decision.OK {
		// No transient error: check the premature stop, which reuses the whole
		// continue chain (streak/maxAttempts, idle/draft/compaction/esc checks).
		s.mu.Lock()
		premature := s.judgeLocked(assistant, s.lastMessages)
		s.mu.Unlock()
		if premature.OK {
			decision = premature
		}
	}
	if !decision.OK {
		if reason := stopReasonOf(assistant); reason != "" && reason != "error" {
			s.mu.Lock()
			s.streak = 0
			s.mu.Unlock()
		}
		return
	}

	s.mu.Lock()
	streak := s.streak
	maxAttempts := s.maxAttempts
	delayMs := s.delayMs
	s.mu.Unlock()

	if streak >= maxAttempts {
		if host != nil {
			host.Notify(fmt.Sprintf("[%s] 连续 %d 次自动继续已达上限（瞬时错误 / 早衰 stop 都计数），停手。可 /auto-continue 看状态，或自己发「继续」。", LogName, streak), "warning")
		}
		clearStatus(host)
		return
	}

	if idle, err := host.IsIdle(); err != nil || !idle {
		return
	}
	if pending, err := host.HasPendingMessages(); err != nil || pending {
		return
	}
	if editorHasDraft(host) {
		if host != nil {
			host.Notify(fmt.Sprintf("[%s] 检测到输入框有草稿，不自动继续", LogName), "info")
		}
		return
	}

	s.mu.Lock()
	s.generation++
	token := s.generation
	attempt := s.streak + 1
	// Armed before the timer object exists: esc reads this one phase, so its
	// cancellation covers the window in which the status/notify below runs and
	// the timer is armed afterwards (the arm re-checks the token).
	s.continuePhase = continueArmed
	s.mu.Unlock()

	if host != nil {
		host.SetStatus(StatusKey, fmt.Sprintf("%s %d/%d", LogName, attempt, maxAttempts))
	}
	// The premature stop's reason code must reach the log (review and debugging
	// both use it); the human hint is only a supplement.
	reason := decision.Reason
	message := fmt.Sprintf("[%s] %s", LogName, reason)
	if hint, ok := PrematureHints[reason]; ok {
		message += "（" + hint + "）"
	}
	message += fmt.Sprintf("，%dms 后自动继续（%d/%d）", delayMs, attempt, maxAttempts)
	if host != nil {
		host.Notify(message, "info")
	}

	s.mu.Lock()
	if token == s.generation && s.continuePhase == continueArmed {
		s.timer = s.sched.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
			s.fireContinue(host, token, attempt)
		})
	}
	s.mu.Unlock()
}

// fireContinue mirrors the setTimeout callback of TS's agent_settled handler
// (auto-continue.ts:1078-1113).
//
// Two concurrency rules keep the port honest where TS's single thread needed no
// rule:
//
//   - A callback whose token is no longer the current generation returns before
//     touching anything. It fired (or was already queued) before the generation
//     was replaced, so `s.timer` may hold the *new* generation's timer; clearing
//     it here would hide that timer from esc and let it send (F1).
//   - The attempt keeps the current generation's identity while it runs the host
//     pre-checks (continueChecking), and the dispatch is claimed under mu
//     (continueCommitted) in the same critical section esc reads. So an esc
//     during a pre-check cancels the round and nothing is sent, while an esc
//     after the claim cannot report a cancellation it did not achieve (F2).
func (s *State) fireContinue(host Host, token int64, attempt int) {
	s.mu.Lock()
	if token != s.generation {
		// Stale callback of a superseded generation: leave the current timer and
		// every other field of the current generation alone.
		s.mu.Unlock()
		return
	}
	// This generation's own timer fired; from here the phase carries its
	// cancellable identity across the host calls below.
	s.timer = nil
	s.continuePhase = continueChecking
	enabled := s.enabled
	cancelled := s.userCancelled
	compacting := s.compacting
	s.mu.Unlock()

	if !enabled {
		s.finishContinue(token)
		return
	}
	// The same checks as agent_settled: locked / compacting / not idle / queued
	// messages / draft all suppress this send.
	if cancelled || compacting {
		s.finishContinue(token)
		clearStatus(host)
		return
	}
	if host == nil {
		s.finishContinue(token)
		return
	}
	if idle, err := host.IsIdle(); err != nil || !idle {
		s.finishContinue(token)
		clearStatus(host)
		return
	}
	if pending, err := host.HasPendingMessages(); err != nil || pending {
		s.finishContinue(token)
		clearStatus(host)
		return
	}
	if editorHasDraft(host) {
		s.finishContinue(token)
		clearStatus(host)
		return
	}

	s.mu.Lock()
	if token != s.generation {
		// esc cancelled while the pre-checks ran: the cancellation wins, so no
		// message is sent and the streak does not move.
		s.mu.Unlock()
		return
	}
	// Dispatch linearization point. esc reads continuePhase in this same critical
	// section, so either it cancelled before this claim (token bumped: the send
	// above never happens) or the claim came first (esc sees continueCommitted
	// and reports no cancellation).
	s.continuePhase = continueCommitted
	s.streak = attempt
	// Mark that the next agent_start / before_agent_start is this extension's
	// own continue, so it is not mistaken for a manual message that releases
	// userCancelled.
	s.selfSending = true
	s.mu.Unlock()

	// ExtensionAPI.sendUserMessage is fire-and-forget in TS (a failure surfaces
	// as an extension error event, not a synchronous throw); the Go SDK returns
	// the host's error, which this port ignores for the same reason. The host
	// call stays outside mu: it may block or re-enter, and the claim above
	// already decided the outcome.
	_ = host.SendUserMessage(ContinuePrompt)
	clearStatus(host)

	s.mu.Lock()
	s.finishContinueLocked(token)
	s.mu.Unlock()
}

// finishContinue ends the attempt of token's generation.
func (s *State) finishContinue(token int64) {
	s.mu.Lock()
	s.finishContinueLocked(token)
	s.mu.Unlock()
}

// finishContinueLocked is finishContinue for callers that hold mu. A superseded
// callback must not reset the phase of the generation that replaced it.
func (s *State) finishContinueLocked(token int64) {
	if token == s.generation {
		s.continuePhase = continueIdle
	}
}

// persistNow mirrors TS persistNow (auto-continue.ts:639-641).
func (s *State) persistNow() {
	s.mu.Lock()
	state := Persist{Enabled: s.enabled, MaxAttempts: s.maxAttempts, DelayMs: s.delayMs}
	path := s.settingsPath
	s.mu.Unlock()
	savePersist(path, state)
}

// HandleCommand mirrors TS's /auto-continue command handler
// (auto-continue.ts:1115-1155).
func (s *State) HandleCommand(host Host, args string) error {
	cmd := strings.ToLower(jsTrim(args))

	switch cmd {
	case "off", "disable", "0":
		s.mu.Lock()
		s.enabled = false
		s.mu.Unlock()
		s.cancelPending()
		clearStatus(host)
		s.persistNow()
		if host != nil {
			host.Notify(fmt.Sprintf("[%s] 已关闭（下次新对话也会保持关闭）", LogName), "info")
		}
		return nil
	case "on", "enable", "1":
		s.mu.Lock()
		s.enabled = true
		s.streak = 0
		// /auto-continue on is the manual entry point that releases the esc lock.
		s.userCancelled = false
		s.selfSending = false
		s.mu.Unlock()
		s.persistNow()
		s.mu.Lock()
		maxAttempts := s.maxAttempts
		s.mu.Unlock()
		if host != nil {
			host.Notify(fmt.Sprintf("[%s] 已开启，最多连续 %d 次（新对话默认开）", LogName, maxAttempts), "info")
		}
		return nil
	}

	if strings.HasPrefix(cmd, "max ") {
		number, isNumber := jsNumber(strings.TrimSpace(cmd[4:]))
		if !isNumber || number < MinAttempts || number > MaxAttemptsLimit {
			if host != nil {
				host.Notify(fmt.Sprintf("[%s] max 需要 1–20", LogName), "warning")
			}
			return nil
		}
		s.mu.Lock()
		s.maxAttempts = int(number)
		s.mu.Unlock()
		s.persistNow()
		s.mu.Lock()
		maxAttempts := s.maxAttempts
		s.mu.Unlock()
		if host != nil {
			host.Notify(fmt.Sprintf("[%s] max=%d", LogName, maxAttempts), "info")
		}
		return nil
	}

	s.mu.Lock()
	enabled := s.enabled
	streak := s.streak
	maxAttempts := s.maxAttempts
	delayMs := s.delayMs
	compacting := s.compacting
	cancelled := s.userCancelled
	s.mu.Unlock()

	status := "off"
	if enabled {
		status = "on"
	}
	line := fmt.Sprintf("[%s] %s  streak=%d/%d  delay=%dms", LogName, status, streak, maxAttempts, delayMs)
	if compacting {
		line += "  compacting（压缩中，不调度）"
	}
	if cancelled {
		line += "  esc-cancelled（手动发消息 / 正常结束 / on 后恢复）"
	}
	if host != nil {
		host.Notify(line, "info")
	}
	return nil
}
