package pivcc

import (
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Tests for extstate.go: the per-extension state before-compact.ts keeps at
// module scope, and the invisible continue it schedules.

// TestExtState_IsKeyedPerExtension pins that two extensions never share state:
// the TS original has one module instance, the Go port serves one subprocess
// per extension, so the map is the equivalent boundary.
func TestExtState_IsKeyedPerExtension(t *testing.T) {
	first, second := Extension(), Extension()
	defer func() {
		extensionStatesMu.Lock()
		delete(extensionStates, first)
		delete(extensionStates, second)
		extensionStatesMu.Unlock()
	}()
	if extStateOf(first) == extStateOf(second) {
		t.Fatal("two extensions share one state")
	}
	// extStateOf is stateFor: the same extension always maps to the same state.
	if extStateOf(first) != stateFor(first) {
		t.Fatal("extStateOf(first) != stateFor(first)")
	}
}

// TestExtState_LastStatsIsCopied pins that the recorded stats are a snapshot:
// a caller mutating what it read back cannot corrupt the state.
func TestExtState_LastStatsIsCopied(t *testing.T) {
	state := &extensionState{}
	if _, ok := state.lastStatsRead(); ok {
		t.Fatal("a fresh state reports stats")
	}
	state.setLastStats(CompactionStats{Summarized: 2, KeptUserTurns: 1, TotalUserTurns: 3})
	read, ok := state.lastStatsRead()
	if !ok || read.Summarized != 2 {
		t.Fatalf("stats = %#v (ok=%v)", read, ok)
	}
	read.Summarized = 99
	read.KeptUserTurns = 99
	again, _ := state.lastStatsRead()
	if again.Summarized != 2 || again.KeptUserTurns != 1 {
		t.Fatalf("the state was mutated through the returned copy: %#v", again)
	}
}

// TestExtState_PendingFollowUpLifecycle pins the set/take pair: a take consumes
// the prompt, and an empty prompt clears rather than stores.
func TestExtState_PendingFollowUpLifecycle(t *testing.T) {
	state := &extensionState{}
	if prompt, ok := state.takePendingFollowUp(); ok || prompt != "" {
		t.Fatalf("take on a fresh state = (%q, %v)", prompt, ok)
	}
	state.setPendingFollowUp("now run the tests")
	if prompt, ok := state.takePendingFollowUp(); !ok || prompt != "now run the tests" {
		t.Fatalf("take = (%q, %v)", prompt, ok)
	}
	if _, ok := state.takePendingFollowUp(); ok {
		t.Fatal("a second take still reported a prompt")
	}
	state.setPendingFollowUp("again")
	state.setPendingFollowUp("")
	if _, ok := state.takePendingFollowUp(); ok {
		t.Fatal("an empty prompt was stored")
	}
}

// TestExtState_ClearPendingAutoContinueIsSafe pins that clearing with nothing
// pending is a no-op, and that a stopped timer is stopped.
func TestExtState_ClearPendingAutoContinueIsSafe(t *testing.T) {
	state := &extensionState{}
	state.clearPendingAutoContinue()
	fired := make(chan struct{}, 1)
	state.mu.Lock()
	state.pendingAutoContinue = time.AfterFunc(50*time.Millisecond, func() { fired <- struct{}{} })
	timer := state.pendingAutoContinue
	state.mu.Unlock()
	state.clearPendingAutoContinue()
	state.clearPendingAutoContinue() // idempotent
	if timer == nil {
		t.Fatal("no timer was installed")
	}
	select {
	case <-fired:
		t.Fatal("the cleared timer still fired")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestExtState_ScheduleAutoContinueSendsMarker drives the real path: a threshold
// compaction on a pi old enough to need the fallback schedules the invisible
// continue, and the marker message reaches the host.
func TestExtState_ScheduleAutoContinueSendsMarker(t *testing.T) {
	t.Setenv("PI_VCC_PI_VERSION", "0.84.3")
	h := newHost(t)
	h.setState(true)

	// Let pi-vcc compact once so the stats the toast needs exist.
	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"tokensBefore": float64(4000),
	}))
	state := extStateOf(h.ext)
	state.setLastCompactWasPiVcc(false)

	h.event(sdk.EventSessionCompact, map[string]any{
		"fromExtension": true,
		"reason":        ReasonThreshold,
		"compactionEntry": map[string]any{
			"details": map[string]any{"compactor": CompactionDetailsCompactor},
		},
	})
	// The continue is sent on the next tick, as setTimeout(..., 0) is.
	h.waitForCalls("sendMessage", 1)
	sends := h.callsOf("sendMessage")
	message, _ := sends[len(sends)-1].Args["message"].(map[string]any)
	if message["customType"] != AutoContinueCustomType {
		t.Fatalf("customType = %v, want the auto-continue marker", message["customType"])
	}
	if message["display"] != false {
		t.Fatalf("message = %#v, want an invisible continue", message)
	}
	options, _ := sends[len(sends)-1].Args["options"].(map[string]any)
	if options["triggerTurn"] != true || options["deliverAs"] != "followUp" {
		t.Fatalf("options = %#v, want a triggering follow-up", options)
	}
}

// TestExtState_NoContinueOnNewPi pins the version gate: a host that reports a
// modern pi gets no invisible continue.
func TestExtState_NoContinueOnNewPi(t *testing.T) {
	t.Setenv("PI_VCC_PI_VERSION", "1.0.0")
	h := newHost(t)
	h.setState(true)

	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"tokensBefore": float64(4000),
	}))
	extStateOf(h.ext).setLastCompactWasPiVcc(false)
	h.event(sdk.EventSessionCompact, map[string]any{
		"fromExtension": true,
		"reason":        ReasonThreshold,
		"compactionEntry": map[string]any{
			"details": map[string]any{"compactor": CompactionDetailsCompactor},
		},
	})
	h.waitForCalls("ui.notify", 1)
	for _, call := range h.callsOf("sendMessage") {
		message, _ := call.Args["message"].(map[string]any)
		if message["customType"] == AutoContinueCustomType {
			t.Fatal("a modern pi got an invisible continue")
		}
	}
}

// TestExtState_ShouldScheduleAutoContinue pins both keys of the gate.
func TestExtState_ShouldScheduleAutoContinue(t *testing.T) {
	if !ShouldScheduleAutoContinue(true, "0.84.3") {
		t.Fatal("an older pi with the setting on should get the continue")
	}
	if ShouldScheduleAutoContinue(true, "0.84.4") {
		t.Fatal("the self-resume version should not")
	}
	if ShouldScheduleAutoContinue(true, "1.0.0") {
		t.Fatal("a newer pi should not")
	}
	if ShouldScheduleAutoContinue(false, "0.84.3") {
		t.Fatal("the setting off should not schedule")
	}
	if ShouldScheduleAutoContinue(true, "not-a-version") {
		t.Fatal("an unreadable version should fail safe")
	}
}

// TestExtState_ScheduleAutoContinueSurvivesDetachedContext pins the recover in
// scheduleAutoContinue: the timer's callback must never take the extension
// down, even when the Context it was handed has no connection.
func TestExtState_ScheduleAutoContinueSurvivesDetachedContext(t *testing.T) {
	state := &extensionState{}
	state.scheduleAutoContinue(nil, sdk.Context{})
	// The callback runs on the next tick; give it time to panic and recover.
	time.Sleep(100 * time.Millisecond)
	state.clearPendingAutoContinue()
}

// ── the timer race the reviewer flagged ─────────────────────────────────────

// widenAutoContinueDelay stretches the auto-continue tick so a test can land a
// clear before it fires. With the real zero delay the callback and the clear
// race for s.mu and the outcome is a photo finish, so the assertion would be
// flaky; the widened delay makes "cleared before the tick" observable. It
// returns the delay so a test can wait past it.
func widenAutoContinueDelay(t *testing.T) time.Duration {
	t.Helper()
	const delay = 250 * time.Millisecond
	previous := autoContinueDelay
	autoContinueDelay = delay
	t.Cleanup(func() { autoContinueDelay = previous })
	return delay
}

// thresholdCompactEvent is the session_compact payload that arms the invisible
// continue: a pi-vcc threshold compaction.
func thresholdCompactEvent() map[string]any {
	return map[string]any{
		"fromExtension": true,
		"reason":        ReasonThreshold,
		"compactionEntry": map[string]any{
			"details": map[string]any{"compactor": CompactionDetailsCompactor},
		},
	}
}

// autoContinueSends counts the invisible-continue markers the host recorded.
func autoContinueSends(h *pivccHost) int {
	sends := 0
	for _, call := range h.callsOf("sendMessage") {
		message, _ := call.Args["message"].(map[string]any)
		if message["customType"] == AutoContinueCustomType {
			sends++
		}
	}
	return sends
}

// waitForAutoContinueSends waits until the host recorded want markers.
func waitForAutoContinueSends(t *testing.T, h *pivccHost, want int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if autoContinueSends(h) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d auto-continue markers before the deadline, want %d", autoContinueSends(h), want)
}

// TestExtState_ClearBeforeTickSuppressesAutoContinue pins the first half of the
// race: a clear that lands between the arm and the tick must suppress the
// invisible continue. Before the fix the callback ignored whether the timer was
// still the live one, so a cleared timer still sent the marker.
func TestExtState_ClearBeforeTickSuppressesAutoContinue(t *testing.T) {
	delay := widenAutoContinueDelay(t)
	t.Setenv("PI_VCC_PI_VERSION", "0.84.3")
	h := newHost(t)
	h.setState(true)

	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"tokensBefore": float64(4000),
	}))
	extStateOf(h.ext).setLastCompactWasPiVcc(false)
	h.event(sdk.EventSessionCompact, thresholdCompactEvent())

	// The tick is still pending (the delay is widened): cancel it before it can
	// fire, as a new agent turn would.
	extStateOf(h.ext).clearPendingAutoContinue()
	time.Sleep(delay + 100*time.Millisecond)

	if sends := autoContinueSends(h); sends != 0 {
		t.Fatalf("a cleared auto-continue still fired %d time(s): %#v", sends, h.callsOf("sendMessage"))
	}
	state := extStateOf(h.ext)
	state.mu.Lock()
	pending := state.pendingAutoContinue
	state.mu.Unlock()
	if pending != nil {
		t.Fatal("the clear left a timer behind")
	}
}

// TestExtState_StaleTickAfterClearStaysSilent pins the interleaving the test
// above cannot reach: the tick already fired and is parked on s.mu when the
// clear lands. The callback must re-check that it is still the live generation
// and stay silent; a callback that relied only on the timer being stopped (the
// shape before the fix) sends the marker anyway.
//
// Holding s.mu across the wait is what pins the interleaving: the callback
// parks on the mutex, the clear runs, and only then does the callback proceed.
// The widened delay guarantees the mutex is taken before the tick fires.
func TestExtState_StaleTickAfterClearStaysSilent(t *testing.T) {
	delay := widenAutoContinueDelay(t)
	t.Setenv("PI_VCC_PI_VERSION", "0.84.3")
	h := newHost(t)
	h.setState(true)
	state := extStateOf(h.ext)

	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"tokensBefore": float64(4000),
	}))
	state.setLastCompactWasPiVcc(false)
	h.event(sdk.EventSessionCompact, thresholdCompactEvent())

	// Take the mutex before the tick can fire, so the callback parks on it.
	state.mu.Lock()
	time.Sleep(delay + 100*time.Millisecond)
	state.clearPendingAutoContinueLocked()
	state.mu.Unlock()

	time.Sleep(100 * time.Millisecond)
	if sends := autoContinueSends(h); sends != 0 {
		t.Fatalf("a stale tick sent the marker %d time(s): %#v", sends, h.callsOf("sendMessage"))
	}
}

// TestExtState_AutoContinueScheduleRace pins the concurrency half: schedules
// and clears hammering the same state stay race-free (the -race detector is the
// assertion) and leave one coherent state — no timer outlives the race, and a
// fresh arm still delivers exactly one marker, so no stale callback can
// double-send.
func TestExtState_AutoContinueScheduleRace(t *testing.T) {
	delay := widenAutoContinueDelay(t)
	t.Setenv("PI_VCC_PI_VERSION", "0.84.3")
	h := newHost(t)
	h.setState(true)
	state := extStateOf(h.ext)

	// Prime the stats the toast path needs once, then arm through the real
	// hook so the timer captures a live Context, as the extension does.
	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"tokensBefore": float64(4000),
	}))
	state.setLastCompactWasPiVcc(false)

	const arms = 8
	stop := make(chan struct{})
	var wg sync.WaitGroup
	// The clearers hammer clearPendingAutoContinue on their own goroutines
	// while the arms run inside the extension's request pump, so the two
	// genuinely race on the same state.
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					state.clearPendingAutoContinue()
				}
			}
		}()
	}
	for i := 0; i < arms; i++ {
		h.event(sdk.EventSessionCompact, thresholdCompactEvent())
	}
	close(stop)
	wg.Wait()

	// Every armed timer has either fired or been stopped by now.
	time.Sleep(delay + 100*time.Millisecond)
	state.mu.Lock()
	pending := state.pendingAutoContinue
	state.mu.Unlock()
	if pending != nil {
		t.Fatal("a timer outlived the schedule/clear race")
	}

	// The state machine is still coherent: one more arm delivers exactly one
	// more marker.
	before := autoContinueSends(h)
	h.event(sdk.EventSessionCompact, thresholdCompactEvent())
	waitForAutoContinueSends(t, h, before+1)
	if sends := autoContinueSends(h); sends != before+1 {
		t.Fatalf("auto-continue sends = %d, want %d after one more arm", sends, before+1)
	}
}

// TestExtState_FormatCompactionStats pins the toast text the two paths share.
func TestExtState_FormatCompactionStats(t *testing.T) {
	stats := CompactionStats{Summarized: 2, KeptUserTurns: 1, TotalUserTurns: 3, KeptTokensEst: 32, SmartKeepAdjusted: true}
	text := FormatCompactionStats(stats)
	for _, want := range []string{"pi-vcc: kept", "1/3", "~32 tok", "summarized 2", "smart-keep"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q is missing %q", text, want)
		}
	}
	budget := FormatCompactionStats(CompactionStats{
		Summarized: 3, KeptTokensEst: 1500, BudgetCut: string(BudgetCutNoAnchor),
	})
	if !strings.Contains(budget, "no user anchor") {
		t.Fatalf("a budget cut toast lost its reason: %q", budget)
	}
	if !strings.Contains(budget, "1.5k") {
		t.Fatalf("the token formatter did not abbreviate: %q", budget)
	}
}
