package cachestats

import (
	"math"
	"testing"
)

// Ports test/hit-rate.mjs sections 1-9 and the cache-hit portions of
// test/cache-stats.mjs.

func TestHitRateDenominatorAndPercent(t *testing.T) {
	// 1. Real Pi shape: input 200 (net), cacheRead 800 -> denom 1000, 80%.
	usage1 := usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)
	requireFloat(t, "denom", cacheHitDenom(usage1, nil, nil), 1000)
	requireOptionalInt(t, "pct", cacheHitPct(usage1), intPtr(80))
	requireFloat(t, "denom(numbers)", cacheHitDenomNumbers(200, 800, 0), 1000)
	requireOptionalInt(t, "pct(numbers)", cacheHitPctNumbers(200, 800, 0), intPtr(80))

	// 2. With cacheWrite: denom 1050 -> 76%.
	usage2 := usageMap("input", 200, "cacheRead", 800, "cacheWrite", 50)
	requireFloat(t, "denom2", cacheHitDenom(usage2, nil, nil), 1050)
	requireOptionalInt(t, "pct2", cacheHitPct(usage2), intPtr(76))
	requireFloat(t, "denom2(numbers)", cacheHitDenomNumbers(200, 800, 50), 1050)
	requireOptionalInt(t, "pct2(numbers)", cacheHitPctNumbers(200, 800, 50), intPtr(76))

	// 3. cacheRead = 0 with a positive denominator -> 0%.
	requireFloat(t, "denom3", cacheHitDenom(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0), nil, nil), 1000)
	requireOptionalInt(t, "pct3", cacheHitPct(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)), intPtr(0))
	requireFloat(t, "denom3b", cacheHitDenom(usageMap("input", 500, "cacheRead", 0, "cacheWrite", 50), nil, nil), 550)
	requireOptionalInt(t, "pct3b", cacheHitPct(usageMap("input", 500, "cacheRead", 0, "cacheWrite", 50)), intPtr(0))

	// 4. All zeros / empty usage / denom <= 0 -> nil, never NaN.
	requireFloat(t, "denom4", cacheHitDenom(usageMap("input", 0, "cacheRead", 0, "cacheWrite", 0), nil, nil), 0)
	requireOptionalInt(t, "pct4", cacheHitPct(usageMap("input", 0, "cacheRead", 0, "cacheWrite", 0)), nil)
	requireFloat(t, "denom4b", cacheHitDenom(usageMap(), nil, nil), 0)
	requireOptionalInt(t, "pct4b", cacheHitPct(usageMap()), nil)
	requireFloat(t, "denom4c", cacheHitDenomNumbers(0, 0, 0), 0)
	requireOptionalInt(t, "pct4c", cacheHitPctNumbers(0, 0, 0), nil)

	// 5. The old formula bug cases: Pi's input is net, so the denominator grows.
	requireFloat(t, "denom5a", cacheHitDenom(usageMap("input", 1000, "cacheRead", 800, "cacheWrite", 0), nil, nil), 1800)
	requireOptionalInt(t, "pct5a", cacheHitPct(usageMap("input", 1000, "cacheRead", 800, "cacheWrite", 0)), intPtr(44))
	requireFloat(t, "denom5b", cacheHitDenom(usageMap("input", 1000, "cacheRead", 9000, "cacheWrite", 0), nil, nil), 10000)
	requireOptionalInt(t, "pct5b", cacheHitPct(usageMap("input", 1000, "cacheRead", 9000, "cacheWrite", 0)), intPtr(90))
}

func TestAggregateHitOverTurns(t *testing.T) {
	turn1 := usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)
	turn2 := usageMap("input", 200, "cacheRead", 800, "cacheWrite", 50)
	denom1 := cacheHitDenom(turn1, nil, nil)
	denom2 := cacheHitDenom(turn2, nil, nil)
	requireFloat(t, "denom1", denom1, 1000)
	requireOptionalInt(t, "pct1", cacheHitPct(turn1), intPtr(80))
	requireFloat(t, "denom2", denom2, 1050)
	requireOptionalInt(t, "pct2", cacheHitPct(turn2), intPtr(76))

	totalRead := 800.0 + 800.0
	totalDenom := denom1 + denom2
	requireFloat(t, "totalRead", totalRead, 1600)
	requireFloat(t, "totalDenom", totalDenom, 2050)
	requireOptionalInt(t, "agg", aggregateHit(totalRead, totalDenom), intPtr(78))
	requireOptionalInt(t, "agg-negative-denom", aggregateHit(10, 0), nil)
}

func TestNormalizeUsageOpenAIStyles(t *testing.T) {
	// 8a. plain OpenAI payload: no cache.
	basic := normalizeUsage(usageMap("prompt_tokens", 1500, "completion_tokens", 300, "total_tokens", 1800))
	requireFloat(t, "basic.input", basic.input, 1500)
	requireFloat(t, "basic.output", basic.output, 300)
	requireFloat(t, "basic.totalTokens", basic.totalTokens, 1800)
	requireFloat(t, "basic.total", basic.total, 1800)
	requireFloat(t, "basic.cacheRead", basic.cacheRead, 0)
	requireFloat(t, "basic.cacheWrite", basic.cacheWrite, 0)
	requireFloat(t, "basic.denom", cacheHitDenom(usageMap("prompt_tokens", 1500, "completion_tokens", 300, "total_tokens", 1800), nil, nil), 1500)
	requireOptionalInt(t, "basic.pct", cacheHitPct(usageMap("prompt_tokens", 1500, "completion_tokens", 300, "total_tokens", 1800)), intPtr(0))

	// 8b. nested prompt_tokens_details.cached_tokens.
	cached := usageMap("prompt_tokens", 200, "completion_tokens", 100, "total_tokens", 1100,
		"prompt_tokens_details", map[string]any{"cached_tokens": 800})
	normCached := normalizeUsage(cached)
	requireFloat(t, "cached.input", normCached.input, 200)
	requireFloat(t, "cached.output", normCached.output, 100)
	requireFloat(t, "cached.cacheRead", normCached.cacheRead, 800)
	requireFloat(t, "cached.cacheWrite", normCached.cacheWrite, 0)
	requireFloat(t, "cached.totalTokens", normCached.totalTokens, 1100)
	requireFloat(t, "cached.denom", cacheHitDenom(cached, nil, nil), 1000)
	requireOptionalInt(t, "cached.pct", cacheHitPct(cached), intPtr(80))

	// 8c. flat cached_tokens plus cache_creation_input_tokens.
	mixed := usageMap("prompt_tokens", 200, "completion_tokens", 50, "total_tokens", 1100,
		"cached_tokens", 800, "cache_creation_input_tokens", 50)
	normMixed := normalizeUsage(mixed)
	requireFloat(t, "mixed.input", normMixed.input, 200)
	requireFloat(t, "mixed.output", normMixed.output, 50)
	requireFloat(t, "mixed.cacheRead", normMixed.cacheRead, 800)
	requireFloat(t, "mixed.cacheWrite", normMixed.cacheWrite, 50)
	requireFloat(t, "mixed.denom", cacheHitDenom(mixed, nil, nil), 1050)
	requireOptionalInt(t, "mixed.pct", cacheHitPct(mixed), intPtr(76))

	// 8d. Pi native fields win their slots.
	native := normalizeUsage(usageMap("input", 200, "output", 100, "cacheRead", 800, "cacheWrite", 50, "totalTokens", 1150))
	requireFloat(t, "native.input", native.input, 200)
	requireFloat(t, "native.output", native.output, 100)
	requireFloat(t, "native.cacheRead", native.cacheRead, 800)
	requireFloat(t, "native.cacheWrite", native.cacheWrite, 50)
	requireFloat(t, "native.totalTokens", native.totalTokens, 1150)

	// 8e. empty / nil.
	empty := normalizeUsage(usageMap())
	requireFloat(t, "empty.input", empty.input, 0)
	requireFloat(t, "empty.output", empty.output, 0)
	requireFloat(t, "empty.cacheRead", empty.cacheRead, 0)
	requireFloat(t, "empty.cacheWrite", empty.cacheWrite, 0)
	requireFloat(t, "empty.totalTokens", empty.totalTokens, 0)
	nullNorm := normalizeUsage(nil)
	requireFloat(t, "null.input", nullNorm.input, 0)
	requireFloat(t, "null.totalTokens", nullNorm.totalTokens, 0)
}

func TestNormalizeUsageReasoningAndTotals(t *testing.T) {
	// reasoning_tokens_details precedence and the clamp.
	norm := normalizeUsage(usageMap(
		"input", 10, "output", 100,
		"completion_tokens_details", map[string]any{"reasoning_tokens": 40},
	))
	requireFloat(t, "reasoning", norm.reasoning, 40)

	// An explicit reasoning value wins over the nested detail.
	norm2 := normalizeUsage(usageMap("reasoning", 7, "completion_tokens_details", map[string]any{"reasoning_tokens": 40}))
	requireFloat(t, "reasoning2", norm2.reasoning, 7)

	// Non-finite / non-positive reasoning clamps to 0.
	requireFloat(t, "reasoning-neg", normalizeUsage(usageMap("reasoning", -5)).reasoning, 0)
	requireFloat(t, "reasoning-abc", normalizeUsage(usageMap("reasoning", "abc")).reasoning, 0)

	// totalTokens falls back to input+output+cacheRead+cacheWrite.
	total := normalizeUsage(usageMap("input", 100, "output", 50, "cacheRead", 10, "cacheWrite", 5))
	requireFloat(t, "total-fallback", total.totalTokens, 165)

	// A non-numeric total falls back as well (Number("abc") || sum).
	totalABC := normalizeUsage(usageMap("input", 100, "totalTokens", "abc"))
	requireFloat(t, "total-abc", totalABC.totalTokens, 100)
}

func TestExtractAndNormalizeUsage(t *testing.T) {
	// A single message.
	single := extractAndNormalizeUsage(map[string]any{
		"role":  "assistant",
		"usage": usageMap("input", 120, "cacheRead", 360, "cacheWrite", 0),
	})
	requireFloat(t, "single.input", single.input, 120)
	requireFloat(t, "single.cacheRead", single.cacheRead, 360)

	// An array of messages; user and toolResult messages are ignored, and a
	// bare usage payload counts.
	summed := extractAndNormalizeUsage([]any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "usage": usageMap("input", 100, "cacheRead", 400, "cacheWrite", 0)},
		map[string]any{"role": "toolResult"},
		map[string]any{"role": "assistant", "usage": usageMap("input", 50, "cacheRead", 100, "cacheWrite", 0)},
	})
	requireFloat(t, "summed.input", summed.input, 150)
	requireFloat(t, "summed.cacheRead", summed.cacheRead, 500)

	// A bare usage payload (no role, no surrounding usage key).
	bare := extractAndNormalizeUsage(usageMap("input", 10, "cacheRead", 20))
	requireFloat(t, "bare.input", bare.input, 10)
	requireFloat(t, "bare.cacheRead", bare.cacheRead, 20)

	// nil aggregate is all zeros.
	none := extractAndNormalizeUsage(nil)
	requireFloat(t, "none.input", none.input, 0)

	// The source usage object is normalized in place.
	source := usageMap("prompt_tokens", 200, "completion_tokens", 80, "total_tokens", 1080,
		"prompt_tokens_details", map[string]any{"cached_tokens": 800})
	_ = extractAndNormalizeUsage(map[string]any{"role": "assistant", "usage": source})
	requireFloat(t, "writeback.input", source["input"].(float64), 200)
	requireFloat(t, "writeback.output", source["output"].(float64), 80)
	requireFloat(t, "writeback.cacheRead", source["cacheRead"].(float64), 800)
	requireFloat(t, "writeback.cacheWrite", source["cacheWrite"].(float64), 0)
}

func TestComputeLiveHitRates(t *testing.T) {
	// 9a. empty / invalid entries.
	requireOptionalInt(t, "empty.aggregate", computeLiveHitRates(nil, nil).aggregate, nil)
	requireOptionalInt(t, "empty.latest", computeLiveHitRates(nil, nil).latest, nil)
	requireOptionalInt(t, "empty2.aggregate", computeLiveHitRates([]any{}, nil).aggregate, nil)
	requireOptionalInt(t, "invalid.aggregate", computeLiveHitRates(anyList(
		map[string]any{}, typedEntry("other", nil)), nil).aggregate, nil)

	// 9b. one assistant message: aggregate and latest agree.
	single := computeLiveHitRates([]any{assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0))}, nil)
	requireOptionalInt(t, "single.aggregate", single.aggregate, intPtr(80))
	requireOptionalInt(t, "single.latest", single.latest, intPtr(80))

	// 9c. multi-turn: aggregate cumulative, latest newest assistant.
	multi := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 50, "cacheRead", 950, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "multi.aggregate", multi.aggregate, intPtr(88))
	requireOptionalInt(t, "multi.latest", multi.latest, intPtr(95))

	// 9d. toolResult contributes to the aggregate only.
	withTool := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)),
		roleEntry("toolResult", usageMap("input", 100, "cacheRead", 100, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "tool.aggregate", withTool.aggregate, intPtr(75))
	requireOptionalInt(t, "tool.latest", withTool.latest, intPtr(80))

	// 9e. compaction and branch_summary contribute to the aggregate only.
	withCompaction := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)),
		typedEntry("compaction", usageMap("input", 50, "cacheRead", 50, "cacheWrite", 0)),
		typedEntry("branch_summary", usageMap("input", 50, "cacheRead", 50, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "compaction.aggregate", withCompaction.aggregate, intPtr(83))
	requireOptionalInt(t, "compaction.latest", withCompaction.latest, intPtr(90))

	// 9f. compaction only: aggregate yes, latest nil.
	compactionOnly := computeLiveHitRates(anyList(
		typedEntry("compaction", usageMap("input", 100, "cacheRead", 300, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "compactionOnly.aggregate", compactionOnly.aggregate, intPtr(75))
	requireOptionalInt(t, "compactionOnly.latest", compactionOnly.latest, nil)

	// 9g. mixed sequence.
	mixed := computeLiveHitRates(anyList(
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "hello"}},
		assistantEntry(usageMap("input", 300, "cacheRead", 700, "cacheWrite", 0)),
		roleEntry("toolResult", usageMap("input", 100, "cacheRead", 0, "cacheWrite", 0)),
		typedEntry("compaction", usageMap("input", 50, "cacheRead", 50, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)),
		roleEntry("toolResult", usageMap("input", 50, "cacheRead", 50, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "mixed.aggregate", mixed.aggregate, intPtr(74))
	requireOptionalInt(t, "mixed.latest", mixed.latest, intPtr(90))

	// 9h. zero usage / no usage.
	zero := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 0, "cacheRead", 0, "cacheWrite", 0)),
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "test"}},
	), nil)
	requireOptionalInt(t, "zero.aggregate", zero.aggregate, nil)
	requireOptionalInt(t, "zero.latest", zero.latest, nil)

	// 9i. a cache-free window keeps both diamonds n/a.
	noCache := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "nocache.aggregate", noCache.aggregate, nil)
	requireOptionalInt(t, "nocache.latest", noCache.latest, nil)

	// 9j. latest residue: a measured cache-free round reports 0%, never stale.
	residue := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "residue.aggregate", residue.aggregate, intPtr(45))
	requireOptionalInt(t, "residue.latest", residue.latest, intPtr(0))

	// 9k. a zero-denominator round clears latest to nil.
	zeroDenom := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 0, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "zerodenom.aggregate", zeroDenom.aggregate, intPtr(90))
	requireOptionalInt(t, "zerodenom.latest", zeroDenom.latest, nil)

	// 9l. a cacheWrite-only round is a cache interaction: 0% both.
	writeOnly := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 0, "cacheWrite", 50)),
	), nil)
	requireOptionalInt(t, "writeonly.aggregate", writeOnly.aggregate, intPtr(0))
	requireOptionalInt(t, "writeonly.latest", writeOnly.latest, intPtr(0))

	// 9m. cacheWrite accumulates from the toolResult and compaction branches too.
	mixedWrite := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 0, "cacheWrite", 50)),
		roleEntry("toolResult", usageMap("input", 100, "cacheRead", 100, "cacheWrite", 20)),
		typedEntry("compaction", usageMap("input", 100, "cacheRead", 100, "cacheWrite", 30)),
	), nil)
	requireOptionalInt(t, "mixedwrite.aggregate", mixedWrite.aggregate, intPtr(33))
	requireOptionalInt(t, "mixedwrite.latest", mixedWrite.latest, intPtr(0))
}

func TestComputeLiveHitRatesBaseline(t *testing.T) {
	baselineEntries := anyList(
		assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 50, "cacheRead", 950, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
	)
	// 9n. baseline 0 -> full scope.
	full := computeLiveHitRates(baselineEntries, intPtr(0))
	requireOptionalInt(t, "full.aggregate", full.aggregate, intPtr(58))
	requireOptionalInt(t, "full.latest", full.latest, intPtr(0))

	// baseline 1 -> last two only.
	one := computeLiveHitRates(baselineEntries, intPtr(1))
	requireOptionalInt(t, "one.aggregate", one.aggregate, intPtr(48))
	requireOptionalInt(t, "one.latest", one.latest, intPtr(0))

	// baseline 2 -> the post-reset window never showed cache: both n/a.
	two := computeLiveHitRates(baselineEntries, intPtr(2))
	requireOptionalInt(t, "two.aggregate", two.aggregate, nil)
	requireOptionalInt(t, "two.latest", two.latest, nil)

	// baseline == length / beyond length: n/a, never a wrong window.
	three := computeLiveHitRates(baselineEntries, intPtr(3))
	requireOptionalInt(t, "three.aggregate", three.aggregate, nil)
	requireOptionalInt(t, "three.latest", three.latest, nil)
	four := computeLiveHitRates(baselineEntries, intPtr(4))
	requireOptionalInt(t, "four.aggregate", four.aggregate, nil)
	requireOptionalInt(t, "four.latest", four.latest, nil)
}

func TestComputeLiveHitRatesLatestResidue(t *testing.T) {
	// 9o. a trailing assistant without usage clears latest, the aggregate stays.
	noUsageTrail := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)),
		roleEntry("toolResult", usageMap("input", 100, "cacheRead", 0, "cacheWrite", 0)),
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant"}},
	), nil)
	requireOptionalInt(t, "trail.aggregate", noUsageTrail.aggregate, intPtr(73))
	requireOptionalInt(t, "trail.latest", noUsageTrail.latest, nil)

	// 9p. a mid-sequence assistant without usage clears latest for good.
	noUsageMiddle := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)),
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant"}},
		roleEntry("toolResult", usageMap("input", 100, "cacheRead", 100, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "middle.aggregate", noUsageMiddle.aggregate, intPtr(75))
	requireOptionalInt(t, "middle.latest", noUsageMiddle.latest, nil)

	// 9q. 0% vs n/a side by side.
	zeroVsNa := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant"}},
	), nil)
	requireOptionalInt(t, "zeroVsNa.aggregate", zeroVsNa.aggregate, intPtr(45))
	requireOptionalInt(t, "zeroVsNa.latest", zeroVsNa.latest, nil)

	// 9r / 9s. a pure no-cache window keeps both n/a across every entry kind.
	measuredNoCacheLast := computeLiveHitRates(anyList(
		roleEntry("toolResult", usageMap("input", 0, "cacheRead", 0, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 0, "cacheRead", 0, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 300, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "measuredNoCache.aggregate", measuredNoCacheLast.aggregate, nil)
	requireOptionalInt(t, "measuredNoCache.latest", measuredNoCacheLast.latest, nil)

	pureNoCache := computeLiveHitRates(anyList(
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
		roleEntry("toolResult", usageMap("input", 50, "cacheRead", 0, "cacheWrite", 0)),
		typedEntry("compaction", usageMap("input", 10, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "pureNoCache.aggregate", pureNoCache.aggregate, nil)
	requireOptionalInt(t, "pureNoCache.latest", pureNoCache.latest, nil)

	// 9t / 9u. the gate counts every scanned entry kind.
	interactionThenMiss := computeLiveHitRates(anyList(
		roleEntry("toolResult", usageMap("input", 100, "cacheRead", 0, "cacheWrite", 50)),
		assistantEntry(usageMap("input", 900, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "interactionThenMiss.aggregate", interactionThenMiss.aggregate, intPtr(0))
	requireOptionalInt(t, "interactionThenMiss.latest", interactionThenMiss.latest, intPtr(0))

	summaryInteraction := computeLiveHitRates(anyList(
		typedEntry("branch_summary", usageMap("input", 100, "cacheRead", 0, "cacheWrite", 30)),
		assistantEntry(usageMap("input", 200, "cacheRead", 0, "cacheWrite", 0)),
	), nil)
	requireOptionalInt(t, "summary.aggregate", summaryInteraction.aggregate, intPtr(0))
	requireOptionalInt(t, "summary.latest", summaryInteraction.latest, intPtr(0))

	// 9v. an open gate with a no-usage newest round keeps ◇ n/a.
	compactionNoUsage := computeLiveHitRates(anyList(
		typedEntry("compaction", usageMap("input", 100, "cacheRead", 500, "cacheWrite", 0)),
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant"}},
	), nil)
	requireOptionalInt(t, "compactionNoUsage.aggregate", compactionNoUsage.aggregate, intPtr(83))
	requireOptionalInt(t, "compactionNoUsage.latest", compactionNoUsage.latest, nil)

	// 9w. seen then miss, and the baseline that drops the cache round.
	seenThenMiss := anyList(
		assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)),
	)
	seen := computeLiveHitRates(seenThenMiss, nil)
	requireOptionalInt(t, "seen.aggregate", seen.aggregate, intPtr(45))
	requireOptionalInt(t, "seen.latest", seen.latest, intPtr(0))
	baselined := computeLiveHitRates(seenThenMiss, intPtr(1))
	requireOptionalInt(t, "seen.baseline.aggregate", baselined.aggregate, nil)
	requireOptionalInt(t, "seen.baseline.latest", baselined.latest, nil)
}

func TestFormatTps(t *testing.T) {
	// Integers render as integers.
	requireOptionalString(t, "45", formatTps(45, 1000), strPtr("45"))
	requireOptionalString(t, "90/2000", formatTps(90, 2000), strPtr("45"))
	requireOptionalString(t, "10000", formatTps(10000, 1000), strPtr("10000"))

	// Non-integers keep one decimal.
	requireOptionalString(t, "12.5 exact", formatTps(25, 2000), strPtr("12.5"))
	requireOptionalString(t, "12.46", formatTps(1246, 100000), strPtr("12.5"))
	requireOptionalString(t, "12.44", formatTps(1244, 100000), strPtr("12.4"))
	requireOptionalString(t, "45.04", formatTps(4504, 100000), strPtr("45"))

	// Tiny speeds never use scientific notation.
	tiny := formatTps(1, 100000)
	requireOptionalString(t, "tiny", tiny, strPtr("0.0"))

	// Uncomputable -> nil, never "NaN"/"Infinity".
	requireOptionalString(t, "zero output", formatTps(0, 1000), nil)
	requireOptionalString(t, "negative output", formatTps(-5, 1000), nil)
	requireOptionalString(t, "NaN output", formatTps(math.NaN(), 1000), nil)
	requireOptionalString(t, "Inf output", formatTps(math.Inf(1), 1000), nil)
	requireOptionalString(t, "zero elapsed", formatTps(45, 0), nil)
	requireOptionalString(t, "negative elapsed", formatTps(45, -1), nil)
	requireOptionalString(t, "NaN elapsed", formatTps(45, math.NaN()), nil)
	requireOptionalString(t, "Inf elapsed", formatTps(45, math.Inf(1)), nil)
}

func TestFormatWindowSize(t *testing.T) {
	cases := map[float64]string{
		1048576: "1M", 2097152: "2.1M", 524288: "524K", 131072: "131K", 65536: "66K",
		1000000: "1M", 2000000: "2M", 500000: "500K", 128000: "128K", 1500000: "1.5M",
		1000: "1K", 163072: "163K", 10485760: "10M", 9999999: "10M",
		1000001: "1M", 999999: "1M", 999: "999", 1001: "1K", 0: "0", -1000: "-1000",
	}
	for input, want := range cases {
		if got := formatWindowSize(input); got != want {
			t.Fatalf("formatWindowSize(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestFormatHerdsmanStatus(t *testing.T) {
	up := map[string]string{
		"◆ Herdsman · 1 agent update":  "1 Up",
		"◆ Herdsman · 2 agent updates": "2 Up",
		"◆ Herdsman · 3 agent updates": "3 Up",
		"Shepherd · 2 agent updates":   "2 Up",
		"1 agent update":               "1 Up",
		"2 updates":                    "2 Up",
		"1 UP":                         "1 Up",
		"5":                            "5 Up",
	}
	for input, want := range up {
		value := input
		requireOptionalString(t, input, formatHerdsmanStatus(&value), strPtr(want))
	}
	for _, input := range []string{"◆ Herdsman", "Herdsman", "◇ Herdsman · reconnecting", "Shepherd"} {
		value := input
		requireOptionalString(t, input, formatHerdsmanStatus(&value), strPtr("On"))
	}
	for _, input := range []string{"", "   "} {
		value := input
		requireOptionalString(t, input, formatHerdsmanStatus(&value), nil)
	}
	requireOptionalString(t, "undefined", formatHerdsmanStatus(nil), nil)
}

func TestFormatHerdsmanStatusRaw(t *testing.T) {
	if got := sheepMeat("◆ Herdsman · 1 agent update"); got != "1 agent update" {
		t.Fatalf("sheepMeat = %q", got)
	}
	if got := stripProvider("GPT-4o (OpenAI)"); got != "GPT-4o" {
		t.Fatalf("stripProvider = %q", got)
	}
	if got := stripProvider("Claude (anthropic) 3.5"); got != "Claude (anthropic) 3.5" {
		t.Fatalf("stripProvider inner = %q", got)
	}
}
