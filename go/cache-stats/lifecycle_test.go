package cachestats

import (
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// sdkContextUsage is the SDK's context-usage record under a short local name.
type sdkContextUsage = sdk.ContextUsage

// Ports the lifecycle and stats sections of test/cache-stats.mjs: the agent_end
// accounting, the per-turn reports, the dual-scope stats output and the reset
// watermark.

// fireTurnEnd is the turn_end handler with an explicit message.
func fireTurnEnd(state *instanceState, host agentHost, message any) {
	applyTurnEndEvent(state, host, map[string]any{"message": message})
}

// fireAgentEnd is the agent_end handler with an explicit message list.
func fireAgentEnd(state *instanceState, host agentHost, messages []any, verbose bool) {
	applyAgentEnd(state, host, map[string]any{"messages": messages}, verbose)
}

// assistantMessage builds an assistant message with a usage payload.
func assistantMessage(usage map[string]any) map[string]any {
	return map[string]any{"role": "assistant", "usage": usage}
}

func TestAgentEndCountsUsageOnce(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	message := assistantMessage(usageMap("input", 150, "cacheRead", 350, "cacheWrite", 50))
	fireTurnEnd(state, host, message)
	fireAgentEnd(state, host, []any{message}, false)

	stats := joinedStats(state, nil)
	requireContains(t, stats, "Cumulative: input=150  output=0  cacheRead=350  cacheWrite=50")
	requireContains(t, stats, "read=350 / denom=550")
	requireContains(t, stats, "Turns: 1")
	requireContains(t, stats, "Aggregate hit: 64%  (current run; read=350 / denom=550)")

	requireInt(t, "entries", len(host.entries), 1)
	requireInt(t, "append custom type", len(host.entries), 1)
	if host.entries[0].customType != "cache-stats-turn" {
		t.Fatalf("custom entry type = %q", host.entries[0].customType)
	}
	requireFloat(t, "entry.input", host.entries[0].data["input"].(float64), 150)
	requireFloat(t, "entry.cacheRead", host.entries[0].data["cacheRead"].(float64), 350)
	requireFloat(t, "entry.cacheWrite", host.entries[0].data["cacheWrite"].(float64), 50)
	requireFloat(t, "entry.denom", host.entries[0].data["denom"].(float64), 550)
	requireInt(t, "entry.hitPct", host.entries[0].data["hitPct"].(int), 64)
	requireInt(t, "entry.turn", host.entries[0].data["turn"].(int), 1)
}

func TestConsecutiveTurnEndAccumulatesOnce(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	first := assistantMessage(usageMap("input", 100, "cacheRead", 200, "cacheWrite", 0))
	second := assistantMessage(usageMap("input", 50, "cacheRead", 150, "cacheWrite", 50))
	fireTurnEnd(state, host, first)
	fireTurnEnd(state, host, second)

	stats := joinedStats(state, nil)
	requireContains(t, stats, "Cumulative: input=150  output=0  cacheRead=350  cacheWrite=50")
	requireContains(t, stats, "read=350 / denom=550")

	// agent_end with the same messages must not double count (liveRun is
	// non-empty, so the fallback never runs).
	fireAgentEnd(state, host, []any{first, second}, false)
	stats = joinedStats(state, nil)
	requireContains(t, stats, "Cumulative: input=150  output=0  cacheRead=350  cacheWrite=50")
	requireContains(t, stats, "Turns: 1")
	requireInt(t, "entries", len(host.entries), 1)
}

func TestAgentEndClearsLiveRunAcrossRuns(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}

	// Run 1: one turn.
	state.liveRun = emptyLiveRun()
	fireTurnEnd(state, host, assistantMessage(usageMap("input", 100, "cacheRead", 400, "cacheWrite", 0)))
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 100, "cacheRead", 400, "cacheWrite", 0))}, false)

	// Run 2: aborted, no turn_end.
	state.liveRun = emptyLiveRun()
	fireAgentEnd(state, host, []any{}, false)

	stats := joinedStats(state, nil)
	requireContains(t, stats, "Turns: 2")
	requireContains(t, stats, "Cumulative: input=100  output=0  cacheRead=400  cacheWrite=0")

	// Run 3: one more turn.
	state.liveRun = emptyLiveRun()
	fireTurnEnd(state, host, assistantMessage(usageMap("input", 50, "cacheRead", 100, "cacheWrite", 0)))
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 50, "cacheRead", 100, "cacheWrite", 0))}, false)

	stats = joinedStats(state, nil)
	requireContains(t, stats, "Turns: 3")
	requireContains(t, stats, "Cumulative: input=150  output=0  cacheRead=500  cacheWrite=0")
	// Run 2 appended nothing: it had no cache activity.
	requireInt(t, "entries", len(host.entries), 2)
}

func TestAgentEndFallbackAccumulatesToSnapshot(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	// Only agent_end fires: the live accumulator is empty and the messages
	// carry the usage.
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 120, "cacheRead", 360, "cacheWrite", 0))}, false)

	requireInt(t, "entries", len(host.entries), 1)
	requireFloat(t, "entry.input", host.entries[0].data["input"].(float64), 120)
	requireFloat(t, "entry.denom", host.entries[0].data["denom"].(float64), 480)
	requireInt(t, "entry.hitPct", host.entries[0].data["hitPct"].(int), 75)

	stats := joinedStats(state, nil)
	requireContains(t, stats, "Turns: 1")
	requireContains(t, stats, "Cumulative: input=120  output=0  cacheRead=360  cacheWrite=0")
	requireContains(t, stats, "Aggregate hit: 75%  (current run; read=360 / denom=480)")
}

func TestAgentEndFullCacheHitSkipsFallback(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	fireTurnEnd(state, host, assistantMessage(usageMap("input", 0, "output", 50, "cacheRead", 500, "cacheWrite", 0)))
	// agent_end carries unrelated history; the live run must win.
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 999, "output", 999, "cacheRead", 999, "cacheWrite", 0))}, false)

	requireInt(t, "entries", len(host.entries), 1)
	requireFloat(t, "entry.input", host.entries[0].data["input"].(float64), 0)
	requireFloat(t, "entry.cacheRead", host.entries[0].data["cacheRead"].(float64), 500)
	requireInt(t, "entry.hitPct", host.entries[0].data["hitPct"].(int), 100)
	requireFloat(t, "entry.denom", host.entries[0].data["denom"].(float64), 500)
}

func TestOpenAIUsageInAgentEnd(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	message := map[string]any{"role": "assistant", "usage": usageMap(
		"prompt_tokens", 1200, "completion_tokens", 300, "total_tokens", 1500,
	)}
	fireTurnEnd(state, host, message)
	fireAgentEnd(state, host, []any{message}, false)

	// The message usage object was normalized in place.
	usage := message["usage"].(map[string]any)
	requireFloat(t, "input", usage["input"].(float64), 1200)
	requireFloat(t, "output", usage["output"].(float64), 300)
	requireFloat(t, "cacheRead", usage["cacheRead"].(float64), 0)
	requireFloat(t, "totalTokens", usage["totalTokens"].(float64), 1500)

	stats := joinedStats(state, nil)
	requireContains(t, stats, "input=1200")
	requireContains(t, stats, "output=300")
	requireContains(t, stats, "cacheRead=0")
	requireContains(t, stats, "cacheWrite=0")
	// No cache interaction: no fabricated 0%.
	requireContains(t, stats, "Aggregate hit: n/a")
}

func TestOpenAICachedTokensInAgentEnd(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	message := map[string]any{"role": "assistant", "usage": usageMap(
		"prompt_tokens", 200, "completion_tokens", 80, "total_tokens", 1080,
		"prompt_tokens_details", map[string]any{"cached_tokens": 800},
	)}
	fireTurnEnd(state, host, message)
	fireAgentEnd(state, host, []any{message}, false)

	usage := message["usage"].(map[string]any)
	requireFloat(t, "input", usage["input"].(float64), 200)
	requireFloat(t, "output", usage["output"].(float64), 80)
	requireFloat(t, "cacheRead", usage["cacheRead"].(float64), 800)
	requireFloat(t, "cacheWrite", usage["cacheWrite"].(float64), 0)

	stats := joinedStats(state, nil)
	requireContains(t, stats, "input=200")
	requireContains(t, stats, "output=80")
	requireContains(t, stats, "cacheRead=800")
	requireContains(t, stats, "Aggregate hit: 80%")
}

func TestTurnReportsAreBounded(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	for i := 0; i < 55; i++ {
		message := assistantMessage(usageMap("input", 1, "cacheRead", 0, "cacheWrite", 0))
		fireTurnEnd(state, host, message)
		fireAgentEnd(state, host, []any{message}, false)
	}
	stats := joinedStats(state, nil)
	requireContains(t, stats, "Turns: 55")
	requireContains(t, stats, "input=55")
	turnLines := 0
	for _, line := range strings.Split(stats, "\n") {
		if strings.HasPrefix(line, "  T") {
			turnLines++
		}
	}
	if turnLines > turnReportLimit {
		t.Fatalf("per-turn lines = %d, want <= %d", turnLines, turnReportLimit)
	}
}

func TestStatsDualScope(t *testing.T) {
	sessionEntries := anyList(
		assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 50, "cacheRead", 950, "cacheWrite", 0)),
	)
	state := createInstanceState(true)
	host := &fakeHost{}
	message := assistantMessage(usageMap("input", 0, "cacheRead", 500, "cacheWrite", 0))
	fireTurnEnd(state, host, message)
	fireAgentEnd(state, host, []any{message}, false)

	stats := joinedStats(state, sessionEntries)
	requireContains(t, stats, "Aggregate hit: 100%  (current run; read=500 / denom=500)")
	requireContains(t, stats, "Session aggregate: 88%  (all session, footer scope; read=1750 / denom=2000)")

	// The session aggregate is available even when the footer was never
	// installed.
	state2 := createInstanceState(true)
	stats2 := joinedStats(state2, sessionEntries)
	requireContains(t, stats2, "Session aggregate: 88%")
}

func TestStatsResetWording(t *testing.T) {
	sessionEntries := anyList(assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)))
	state := createInstanceState(true)
	stats := joinedStats(state, sessionEntries)
	requireContains(t, stats, "Session aggregate: 90%  (all session, footer scope; read=900 / denom=1000)")

	baseline := len(sessionEntries)
	state.footerResetBaseline = &baseline
	stats = joinedStats(state, sessionEntries)
	requireContains(t, stats, "Session aggregate: 90%  (all session, footer currently post-reset window; read=900 / denom=1000)")
}

func TestPerTurnZeroPercentAfterInteraction(t *testing.T) {
	sessionEntries := anyList(
		assistantEntry(usageMap("input", 500, "cacheRead", 500, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
	)
	state := createInstanceState(true)
	host := &fakeHost{}
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0))}, false)

	stats := joinedStats(state, sessionEntries)
	requireContains(t, stats, "Session aggregate: 25%")
	requireContains(t, stats, "T1: i=1000 r=0 w=0 0%")
}

func TestStatsTailAndUnknown400(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	for i := 0; i < 3; i++ {
		message := assistantMessage(usageMap("input", 10, "cacheRead", 90, "cacheWrite", 0))
		fireTurnEnd(state, host, message)
		fireAgentEnd(state, host, []any{message}, false)
	}
	stats := joinedStats(state, nil)
	requireContains(t, stats, "Tail (last 3) hit: 90%")

	state.unknown400["audit/offline"] = true
	stats = joinedStats(state, nil)
	requireContains(t, stats, "Unclassified 400 models (strategy unchanged): audit/offline")
}

func TestResetRunStateClearsFooterSources(t *testing.T) {
	state := createInstanceState(true)
	state.footerSources = &footerSources{
		entries:         func() ([]any, bool) { return []any{}, true },
		getContextUsage: func() (*sdkContextUsage, bool) { return nil, false },
	}
	resetRunState(state)
	if state.footerSources != nil {
		t.Fatal("resetRunState must clear footerSources (no stale cross-session reference)")
	}
	if state.footerResetEpoch != 1 {
		t.Fatalf("resetRunState must bump the footer epoch, got %d", state.footerResetEpoch)
	}
	if state.footerHitCache != nil || state.footerResetBaseline != nil {
		t.Fatal("resetRunState must invalidate the footer live-hit cache and baseline")
	}
}

func TestReadFooterCtxDoesNotInheritSources(t *testing.T) {
	state := createInstanceState(true)
	readFooterCtx(state, footerReadInput{
		modelName:          "old-model",
		hasSessionManager:  true,
		entries:            func() ([]any, bool) { return []any{}, true },
		hasGetContextUsage: true,
		getContextUsage:    func() (*sdkContextUsage, bool) { return nil, false },
		contextUsage:       sdkUsage(10, 100, 10),
	})
	if state.footerSources == nil {
		t.Fatal("footerSources captured when ctx provides sources")
	}
	if state.footerSources.getContextUsage == nil {
		t.Fatal("getContextUsage captured when ctx provides it")
	}
	oldUsage := state.footerSources.getContextUsage

	// The new ctx has a session manager but no getContextUsage: the old bound
	// function must not survive.
	readFooterCtx(state, footerReadInput{
		modelName:         "new-model",
		hasSessionManager: true,
		entries:           func() ([]any, bool) { return []any{}, true },
	})
	if state.footerSources == nil {
		t.Fatal("footerSources stays set when ctx still provides a session manager")
	}
	if state.footerSources.getContextUsage != nil {
		t.Fatal("a missing getContextUsage must be nil, never a stale reference")
	}
	if oldUsage == nil {
		t.Fatal("fixture sanity")
	}

	// A ctx with neither source nulls footerSources.
	readFooterCtx(state, footerReadInput{modelName: "sources-less"})
	if state.footerSources != nil {
		t.Fatal("footerSources should be nil when ctx provides neither source")
	}
}

// sdkUsage builds a context-usage fixture.
func sdkUsage(tokens, window int, percent float64) *sdkContextUsage {
	return &sdkContextUsage{Tokens: &tokens, ContextWindow: window, Percent: &percent}
}

func TestVerboseAppendsWithoutCache(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}
	// Without verbose, a cache-free turn appends nothing.
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 10, "cacheRead", 0, "cacheWrite", 0))}, false)
	requireInt(t, "quiet entries", len(host.entries), 0)

	// With verbose, every turn is appended.
	fireAgentEnd(state, host, []any{assistantMessage(usageMap("input", 10, "cacheRead", 0, "cacheWrite", 0))}, true)
	requireInt(t, "verbose entries", len(host.entries), 1)
	// A cache-free turn still has a measurable denominator, so its hit rate is
	// 0% rather than n/a.
	requireInt(t, "cache-free hitPct", host.entries[0].data["hitPct"].(int), 0)
}
