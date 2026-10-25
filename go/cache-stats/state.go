package cachestats

import (
	"crypto/rand"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file ports the per-instance state of cache-stats.ts (TS L320-420 and
// L536-700): the run snapshot, the live accumulator, the turn reports, the
// footer capture and its live-hit cache, the prefix memory, and the TPS
// scratch stamps, together with the transitions that touch them.

// snapshot is Snapshot: the current run's cumulative counters.
type snapshot struct {
	totalCacheRead  float64
	totalCacheWrite float64
	totalInput      float64
	totalOutput     float64
	totalHitDenom   float64
	turns           int
}

// liveRun is LiveRun: the accumulator reset at each agent_start.
type liveRun struct {
	input      float64
	output     float64
	cacheRead  float64
	cacheWrite float64
	hitDenom   float64
}

// turnReport is TurnReport: one bounded per-turn record.
type turnReport struct {
	turn       int
	input      float64
	output     float64
	cacheRead  float64
	cacheWrite float64
	denom      float64
	hitPct     *int
}

// footerContext is FooterContext: the last context-usage snapshot.
type footerContext struct {
	tokens        *int
	contextWindow int
	percent       *float64
}

// footerSources are the live sources the footer re-reads on every render, as
// the TS footerSources holds the session manager and a bound getContextUsage.
type footerSources struct {
	entries         func() ([]any, bool)
	getContextUsage func() (*sdk.ContextUsage, bool)
}

// footerHitCache is the dirty-check cache of the footer's live hit rates.
type footerHitCache struct {
	key       string
	aggregate *int
	latest    *int
}

// instanceState is InstanceState: all per-extension-instance state. TS:
// InstanceState (L396-428).
//
// mu guards every field below. The SDK renders a footer from a background
// goroutine (surface.go:100-123 pushes each render off the message loop), so a
// render reading the footer fields can overlap a lifecycle or command handler
// writing them. Two rules keep that safe:
//
//  1. Hold mu while touching fields, never across a call into the SDK. In
//     particular SetFooterRenderer must be called with mu released: it renders
//     synchronously through surfaceRenderer.push, which takes the renderer mutex
//     and calls back into renderFooter, and replaceSurface waits on that same
//     mutex for the previous renderer. Calling it under mu would deadlock
//     against our own renderer.
//  2. renderFooter takes mu itself, so the renderer closure installed by
//     installFooter never inherits a locked state.
//
// Pure read/render helpers take the lock (snapshotLocked and friends); the
// state transitions in this file do too. Only *Locked helpers may be called
// with mu already held.
type instanceState struct {
	mu sync.Mutex

	runtimeEnabled bool
	snapshot       snapshot
	liveRun        liveRun
	turnReports    []turnReport
	unknown400     map[string]bool

	// footerRefresh requests a footer re-render; force matches the TS
	// requestRender(force). The SDK has no force flag, so the port re-pushes
	// the renderer and ignores force.
	footerRefresh   func(force bool)
	footerModelName string
	footerThinking  *string
	footerContext   *footerContext
	footerSources   *footerSources

	// setFooterRendererFn is the host edge installFooter / uninstallFooter / the
	// footerRefresh closure all call, so the whole production path can be
	// driven by the tests. nil means sdk.Context.SetFooterRenderer (see
	// footerRendererHook, which is what every call site uses); a test injects
	// a stub whose SetFooterRenderer renders synchronously, exactly as the
	// SDK does.
	setFooterRendererFn func(ctx sdk.Context, render func(width int) []string) error

	footerResetEpoch    int
	footerResetBaseline *int
	footerHitCache      *footerHitCache

	// TPS estimation stamps: request start, streaming decode stamps and the
	// formatted tokens/s of the last completed turn.
	providerRequestStartedAt *int64
	streamFirstThinkingAt    *int64
	streamFirstTextAt        *int64
	streamLastDeltaAt        *int64
	streamDoneAt             *int64
	lastTurnTps              *string

	footerInstalled bool

	prefixDiagEnabled bool
	prefixSalt        [16]byte
	prefixByScope     *lruMap[storedPrefix]
	lastPrefixDiag    *prefixView
}

func emptySnapshot() snapshot { return snapshot{} }

func emptyLiveRun() liveRun { return liveRun{} }

// randomSalt is a 16-byte fingerprint salt. TS: randomSalt (L435-442).
func randomSalt() [16]byte {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to a zero salt, which
		// only weakens the display-only fingerprint.
		return salt
	}
	return salt
}

// createInstanceState builds the initial state. TS: createInstanceState
// (L444-467).
func createInstanceState(prefixDiagEnabled bool) *instanceState {
	return &instanceState{
		runtimeEnabled:    true,
		snapshot:          emptySnapshot(),
		liveRun:           emptyLiveRun(),
		turnReports:       []turnReport{},
		unknown400:        map[string]bool{},
		prefixDiagEnabled: prefixDiagEnabled,
		prefixSalt:        randomSalt(),
		prefixByScope:     newLRUMap[storedPrefix](prefixScopeLimit),
	}
}

// clearPrefixMemory drops the per-scope snapshots. TS: clearPrefixMemory
// (L469-472).
func clearPrefixMemory(state *instanceState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	clearPrefixMemoryLocked(state)
}

func clearPrefixMemoryLocked(state *instanceState) {
	state.prefixByScope.clear()
	state.lastPrefixDiag = nil
}

// clearTurnTiming drops the TPS scratch stamps. TS: clearTurnTiming
// (L474-480).
func clearTurnTiming(state *instanceState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	clearTurnTimingLocked(state)
}

func clearTurnTimingLocked(state *instanceState) {
	state.providerRequestStartedAt = nil
	state.streamFirstThinkingAt = nil
	state.streamFirstTextAt = nil
	state.streamLastDeltaAt = nil
	state.streamDoneAt = nil
}

// resetRunState resets the current run, drops the captured footer sources, and
// invalidates the footer live-hit cache. TS: resetRunState (L482-508).
func resetRunState(state *instanceState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	resetRunStateLocked(state)
}

func resetRunStateLocked(state *instanceState) {
	state.snapshot = emptySnapshot()
	state.liveRun = emptyLiveRun()
	state.turnReports = []turnReport{}
	state.unknown400 = map[string]bool{}
	clearPrefixMemoryLocked(state)
	state.footerSources = nil
	state.footerResetEpoch++
	state.footerResetBaseline = nil
	state.footerHitCache = nil
	clearTurnTimingLocked(state)
	state.lastTurnTps = nil
}

// footerReadInput is one ctx capture for readFooterCtx: the extension fills it
// from the SDK context and the tests fill it directly.
type footerReadInput struct {
	modelName          string
	thinking           *string
	hasSessionManager  bool
	entries            func() ([]any, bool)
	hasGetContextUsage bool
	getContextUsage    func() (*sdk.ContextUsage, bool)
	contextUsage       *sdk.ContextUsage
}

// readFooterCtx re-captures only what this ctx provides and never inherits a
// reference from a previous session. TS: readFooterCtx (L535-560).
func readFooterCtx(state *instanceState, input footerReadInput) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.footerModelName = input.modelName
	state.footerThinking = input.thinking
	if input.hasSessionManager || input.hasGetContextUsage {
		sources := &footerSources{}
		if input.hasSessionManager {
			sources.entries = input.entries
		}
		if input.hasGetContextUsage {
			sources.getContextUsage = input.getContextUsage
		}
		state.footerSources = sources
	} else {
		state.footerSources = nil
	}
	if input.contextUsage != nil {
		state.footerContext = &footerContext{
			tokens:        input.contextUsage.Tokens,
			contextWindow: input.contextUsage.ContextWindow,
			percent:       input.contextUsage.Percent,
		}
	} else {
		state.footerContext = nil
	}
}

// refreshFooter asks the installed footer renderer to render again. TS:
// refreshFooter (L562-571).
//
// The callback is copied under the lock and invoked after it is released:
// footerRefresh re-pushes the renderer, and re-pushing reaches the SDK, which
// renders synchronously and waits on the outgoing renderer's mutex.
func refreshFooter(state *instanceState, force bool) {
	state.mu.Lock()
	refresh := state.footerRefresh
	state.mu.Unlock()
	if refresh == nil {
		return
	}
	refresh(force)
}

// recordTurnUsage accumulates one assistant message's usage into the live run
// and the snapshot, and normalizes the source object in place. TS:
// recordTurnUsage (L573-611).
func recordTurnUsage(state *instanceState, rawMessage any) {
	message, ok := asObject(rawMessage)
	if !ok {
		return
	}
	role, _ := message.lookup("role")
	rawUsage, hasUsage := message.lookup("usage")
	if role != "assistant" || !hasUsage || rawUsage == nil {
		return
	}
	// normalizeUsage and writeBackUsage touch only the caller's payload, never
	// the instance state, so they run before the lock is taken.
	norm := normalizeUsage(rawUsage)
	writeBackUsage(rawUsage, norm)
	denom := cacheHitDenomOf(norm)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.liveRun.input += norm.input
	state.liveRun.output += norm.output
	state.liveRun.cacheWrite += norm.cacheWrite
	if denom > 0 {
		state.liveRun.cacheRead += norm.cacheRead
		state.liveRun.hitDenom += denom
	}
	state.snapshot.totalInput += norm.input
	state.snapshot.totalOutput += norm.output
	state.snapshot.totalCacheWrite += norm.cacheWrite
	if denom > 0 {
		state.snapshot.totalCacheRead += norm.cacheRead
		state.snapshot.totalHitDenom += denom
	}
}

// applyMessageUpdate records one streaming stamp. TS: the message_update
// handler (L1899-1922).
func applyMessageUpdate(state *instanceState, assistantMessageEvent any, now int64) {
	event, ok := asObject(assistantMessageEvent)
	if !ok {
		return
	}
	typeValue, _ := event.lookup("type")
	eventType, isString := typeValue.(string)
	if !isString {
		return
	}
	delta, _ := event.lookup("delta")
	deltaText, isDeltaString := delta.(string)
	state.mu.Lock()
	defer state.mu.Unlock()
	switch eventType {
	case "thinking_delta":
		if isDeltaString && deltaText != "" {
			if state.streamFirstThinkingAt == nil {
				stamp := now
				state.streamFirstThinkingAt = &stamp
			}
			stamp := now
			state.streamLastDeltaAt = &stamp
		}
	case "text_delta", "toolcall_delta":
		if isDeltaString && deltaText != "" {
			if state.streamFirstTextAt == nil {
				stamp := now
				state.streamFirstTextAt = &stamp
			}
			stamp := now
			state.streamLastDeltaAt = &stamp
		}
	case "done":
		stamp := now
		state.streamDoneAt = &stamp
	}
}

// applyMessageEnd locks doneAt once the assistant message finished streaming,
// so tool latency after it is excluded from the estimate. TS: the message_end
// handler (L1924-1934).
func applyMessageEnd(state *instanceState, message any, now int64) {
	obj, ok := asObject(message)
	if !ok {
		return
	}
	role, _ := obj.lookup("role")
	if role != "assistant" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.streamDoneAt == nil && (state.streamFirstThinkingAt != nil || state.streamFirstTextAt != nil) {
		stamp := now
		state.streamDoneAt = &stamp
	}
}

// applyTurnEnd consumes this turn's timing stamps exactly once and stores the
// estimated tokens/s. TS: the turn_end handler (L1936-1955).
func applyTurnEnd(state *instanceState, message any, now int64) {
	state.mu.Lock()
	defer state.mu.Unlock()
	timing := &turnTimingStamps{
		requestStartedAt: millisToStamp(state.providerRequestStartedAt),
		firstThinkingAt:  millisToStamp(state.streamFirstThinkingAt),
		firstTextAt:      millisToStamp(state.streamFirstTextAt),
		lastDeltaAt:      millisToStamp(state.streamLastDeltaAt),
		doneAt:           millisToStamp(state.streamDoneAt),
	}
	endStamp := float64(now)
	timing.turnEndedAt = &endStamp
	clearTurnTimingLocked(state)
	// computeTurnTps reads only its own arguments (the stamps and the message),
	// never the instance state, so it may run under the lock.
	state.lastTurnTps = computeTurnTps(timing, message)
}

// millisToStamp widens a millisecond stamp to the float64 the estimator uses.
func millisToStamp(value *int64) *float64 {
	if value == nil {
		return nil
	}
	converted := float64(*value)
	return &converted
}

// observePrefixDiagnostics records one read-only prefix observation. When the
// comparison scope is unavailable it drops every known scope and reports
// skipped. TS: observePrefixDiagnostics (L1690-1710).
//
// The payload walk (summarizePayload) is the expensive part and touches only
// the payload and the immutable salt, so it runs before the lock is taken; the
// LRU read/compare/write then happens under the lock as one critical section so
// two concurrent observations cannot interleave between the compare and the
// store (which would let both report stable/baseline against a stale entry).
func observePrefixDiagnostics(state *instanceState, payload any, identity scopeIdentity) {
	state.mu.Lock()
	salt := state.prefixSalt
	state.mu.Unlock()

	key, ok := scopeKey(salt, identity)
	if !ok {
		state.mu.Lock()
		state.prefixByScope.clear()
		state.lastPrefixDiag = &prefixView{
			status:   statusSkipped,
			shape:    shapeUnknown,
			sections: skippedCoreSections(),
		}
		state.mu.Unlock()
		return
	}
	budget := &prefixBudget{}
	summary := summarizePayload(salt, payload, budget)

	state.mu.Lock()
	defer state.mu.Unlock()
	prev, hasPrev := state.prefixByScope.get(key)
	var prevPtr *storedPrefix
	if hasPrev {
		prevPtr = &prev
	}
	status := decidePrefixStatus(prevPtr, summary.stored)
	state.prefixByScope.set(key, summary.stored)
	state.lastPrefixDiag = &prefixView{
		status:   status,
		shape:    summary.shape,
		sections: summary.sections,
		extras:   summary.extras,
	}
}

// skipPrefixDiagnostics records a skipped observation without touching the
// known scopes, as the before_provider_request catch does. TS: the handler's
// catch branch (L1806-1819).
func skipPrefixDiagnostics(state *instanceState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.prefixByScope.clear()
	state.lastPrefixDiag = &prefixView{
		status:   statusSkipped,
		shape:    shapeUnknown,
		sections: skippedCoreSections(),
	}
}
