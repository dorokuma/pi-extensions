package pivcc

import (
	"sync"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// The mutable state the compaction hook keeps between events, ported from
// before-compact.ts's module-level variables:
//
//	let lastStats: CompactionStats | null = null;
//	let lastCompactWasPiVcc = false;
//	let pendingFollowUpPrompt: string | null = null;
//	let pendingAutoContinueTimer: ... = null;
//
// TS keeps these at module scope because a pi extension is loaded once. A Go
// subprocess extension is built once too, but tests construct more than one, so
// the state hangs off the Extension instead of the package.
type extensionState struct {
	mu                  sync.Mutex
	lastStats           *CompactionStats
	lastCompactWasPiVcc bool
	pendingFollowUp     string
	hasPendingFollowUp  bool
	pendingAutoContinue *time.Timer
	// autoContinueGen counts every arm and every clear of the auto-continue.
	// The timer callback compares it against the generation it was started
	// with, so a clear (or a newer schedule) that lands before the callback
	// runs invalidates it instead of letting a stale callback fire.
	autoContinueGen uint64
}

// extensionStates holds one state per built extension, keyed by the extension
// pointer PiG handed us.
var (
	extensionStatesMu sync.Mutex
	extensionStates   = map[*sdk.Extension]*extensionState{}
)

func stateFor(ext *sdk.Extension) *extensionState {
	extensionStatesMu.Lock()
	defer extensionStatesMu.Unlock()
	state, ok := extensionStates[ext]
	if !ok {
		state = &extensionState{}
		extensionStates[ext] = state
	}
	return state
}

// extStateOf is the state of an extension, for the command handlers that read
// what the hook recorded.
func extStateOf(ext *sdk.Extension) *extensionState {
	return stateFor(ext)
}

func (s *extensionState) setLastStats(stats CompactionStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := stats
	s.lastStats = &copied
}

func (s *extensionState) lastStatsRead() (CompactionStats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastStats == nil {
		return CompactionStats{}, false
	}
	return *s.lastStats, true
}

func (s *extensionState) setLastCompactWasPiVcc(value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCompactWasPiVcc = value
}

func (s *extensionState) getLastCompactWasPiVcc() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCompactWasPiVcc
}

func (s *extensionState) setPendingFollowUp(prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prompt == "" {
		s.pendingFollowUp = ""
		s.hasPendingFollowUp = false
		return
	}
	s.pendingFollowUp = prompt
	s.hasPendingFollowUp = true
}

func (s *extensionState) takePendingFollowUp() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prompt, ok := s.pendingFollowUp, s.hasPendingFollowUp
	s.pendingFollowUp = ""
	s.hasPendingFollowUp = false
	return prompt, ok
}

// autoContinueDelay is how long scheduleAutoContinue waits before the tick, as
// `setTimeout(..., 0)` does. It is a variable so a test can widen it and
// observe a clear that lands before the tick, which a zero delay would turn
// into a photo finish against the callback.
var autoContinueDelay time.Duration

// clearPendingAutoContinue is before-compact.ts clearPendingAutoContinue().
//
// The stop happens under the mutex together with the generation bump, so a
// callback that already read the old generation cannot fire afterwards.
func (s *extensionState) clearPendingAutoContinue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearPendingAutoContinueLocked()
}

// clearPendingAutoContinueLocked stops and forgets the pending timer. The
// caller must already hold s.mu.
func (s *extensionState) clearPendingAutoContinueLocked() {
	if s.pendingAutoContinue != nil {
		s.pendingAutoContinue.Stop()
		s.pendingAutoContinue = nil
	}
	s.autoContinueGen++
}

// scheduleAutoContinue is before-compact.ts scheduleAutoContinue(): fire the
// invisible continue on the next tick, as `setTimeout(..., 0)` does.
//
// The whole arm runs under s.mu and the callback re-checks the generation it
// captured, so the timer pointer can never be overwritten by a racing
// schedule and a cleared or superseded callback returns without calling
// TriggerInvisibleContinue.
func (s *extensionState) scheduleAutoContinue(ext *sdk.Extension, ctx sdk.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearPendingAutoContinueLocked()
	gen := s.autoContinueGen
	timer := time.AfterFunc(autoContinueDelay, func() {
		defer func() { _ = recover() }()
		s.mu.Lock()
		if s.autoContinueGen != gen {
			// Cleared or replaced while the tick was pending.
			s.mu.Unlock()
			return
		}
		s.pendingAutoContinue = nil
		s.autoContinueGen = gen + 1
		s.mu.Unlock()
		TriggerInvisibleContinue(ctx)
	})
	s.pendingAutoContinue = timer
}

// TriggerInvisibleContinue is before-compact.ts triggerInvisibleContinue():
// send a custom message marked with the auto-continue customType so pi's queue
// stays coherent; the context handler strips it from the LLM payload.
func TriggerInvisibleContinue(ctx sdk.Context) error {
	trigger := true
	return ctx.SendCustomMessage(sdk.CustomMessage{
		CustomType: AutoContinueCustomType,
		Content:    []any{},
		Display:    false,
	}, sdk.SendMessageOptions{TriggerTurn: &trigger, DeliverAs: "followUp"})
}

// notifyLater is before-compact.ts scheduleCompactionStatsNotify(): the toast
// lands 500ms later so the UI settles first.
func notifyLater(ctx sdk.Context, message, level string) {
	time.AfterFunc(500*time.Millisecond, func() {
		defer func() { _ = recover() }()
		ctx.Notify(message, level)
	})
}
