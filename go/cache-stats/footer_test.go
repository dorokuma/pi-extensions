package cachestats

import (
	"strings"
	"testing"
)

// Ports the footer sections of test/cache-stats.mjs: the live dual hit-rate
// segment, the gate and reset watermark, the dirty-check cache, the context
// occupancy colors and the segment order.

// upperTheme renders each style as an upper-case marker, matching the TS
// colorTheme.
type upperTheme struct{}

func (upperTheme) Fg(token, text string) string {
	return "[" + strings.ToUpper(token) + ":" + text + "]"
}

// boolUsageSource returns a fixed context usage.
func boolUsageSource(usage *sdkContextUsage) func() (*sdkContextUsage, bool) {
	return func() (*sdkContextUsage, bool) { return usage, usage != nil }
}

// footerStateWithEntries builds a state whose footer reads entries and usage.
func footerStateWithEntries(modelName string, entries func() ([]any, bool), usage func() (*sdkContextUsage, bool)) *instanceState {
	state := createInstanceState(true)
	state.footerModelName = modelName
	state.footerSources = &footerSources{entries: entries, getContextUsage: usage}
	if usage != nil {
		if value, ok := usage(); ok {
			state.footerContext = &footerContext{tokens: value.Tokens, contextWindow: value.ContextWindow, percent: value.Percent}
		}
	}
	return state
}

func TestFooterSnapshotFallbackSegments(t *testing.T) {
	// The mock context has no getEntries, so both diamonds come from the
	// snapshot.
	state := footerStateWithEntries("GPT-4o (OpenAI)", nil, boolUsageSource(sdkUsage(64000, 128000, 50)))
	recordTurnUsage(state, map[string]any{"role": "assistant", "usage": usageMap(
		"prompt_tokens", 1000, "completion_tokens", 200, "total_tokens", 1200)})
	line := footerRender(state, plainTheme{}, map[string]string{}, 120)
	requireContains(t, line, "◆ n/a")
	requireContains(t, line, "▲ 50%/128K")
	requireContains(t, line, "■ GPT-4o")
	requireContains(t, line, "▸ n/a")

	recordTurnUsage(state, map[string]any{"role": "assistant", "usage": usageMap(
		"prompt_tokens", 200, "completion_tokens", 100, "total_tokens", 1100,
		"prompt_tokens_details", map[string]any{"cached_tokens": 800})})
	line = footerRender(state, plainTheme{}, map[string]string{}, 120)
	requireContains(t, line, "◆ 40%")
	requireContains(t, line, "▲ 50%/128K")
}

func TestFooterLiveDualHitRates(t *testing.T) {
	entries := []any{}
	theme := &recordingTheme{}
	state := footerStateWithEntries("GPT-4o (openai)", func() ([]any, bool) { return entries, true }, boolUsageSource(sdkUsage(40000, 128000, 31.25)))

	line := footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[DIM:◆] [DIM:n/a] [DIM:◇] [DIM:n/a]")
	requireContains(t, line, "[TXT:31%/128K]")

	entries = append(entries, assistantEntry(usageMap("input", 200, "cacheRead", 800, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:80%]")
	requireContains(t, line, "[WARN:80%]")

	entries = append(entries, roleEntry("toolResult", usageMap("input", 100, "cacheRead", 100, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:75%]")
	requireContains(t, line, "[WARN:80%]")

	entries = append(entries, assistantEntry(usageMap("input", 50, "cacheRead", 950, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:84%]")
	requireContains(t, line, "[WARN:95%]")

	// A failing source falls back to the snapshot, which was never written.
	entries = append(entries, assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)))
	state.footerSources.entries = func() ([]any, bool) { return nil, false }
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[DIM:n/a]")
}

func TestFooterGateAndWatermark(t *testing.T) {
	entries := []any{}
	getEntriesCalls := 0
	theme := &recordingTheme{}
	state := footerStateWithEntries("no-cache-model", func() ([]any, bool) {
		getEntriesCalls++
		return entries, true
	}, boolUsageSource(sdkUsage(2000, 10000, 20)))

	// 1. Empty entries: no stale 0%.
	line := footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[DIM:n/a]")
	requireInt(t, "calls 1", getEntriesCalls, 1)

	// 2. Cache round A -> 50% both.
	entries = append(entries, assistantEntry(usageMap("input", 500, "cacheRead", 500, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:50%]")
	requireInt(t, "calls 2", getEntriesCalls, 2)

	// 3. Same entry count reuses the cached result (no rescan).
	entries[0].(map[string]any)["message"].(map[string]any)["usage"].(map[string]any)["cacheRead"] = 999
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:50%]")
	requireInt(t, "calls 3", getEntriesCalls, 3)
	entries[0].(map[string]any)["message"].(map[string]any)["usage"].(map[string]any)["cacheRead"] = 500

	// 4. A measured cache-free round shows ◇ 0%, ◆ keeps its stricter rule.
	entries = append(entries, assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:25%]")
	requireContains(t, line, "[WARN:0%]")
	requireInt(t, "calls 4", getEntriesCalls, 4)

	// 5. Cache round C -> ◆ 47% ◇ 90%; only ◇ wears the warning color.
	entries = append(entries, assistantEntry(usageMap("input", 100, "cacheRead", 900, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:47%]")
	requireContains(t, line, "[WARN:90%]")
	if count := strings.Count(line, "[WARN:"); count != 1 {
		t.Fatalf("warning markers = %d, want 1", count)
	}
	requireInt(t, "calls 5", getEntriesCalls, 5)

	// 6. A reset watermark moves the window past every current entry.
	baseline := len(entries)
	state.footerResetBaseline = &baseline
	state.footerResetEpoch++
	state.footerHitCache = nil
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[DIM:n/a]")

	// 7. Only the entry appended after the reset counts: 50%.
	entries = append(entries, assistantEntry(usageMap("input", 100, "cacheRead", 100, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:50%]")
	requireContains(t, line, "[WARN:50%]")

	// 8. A second reset whose window never shows a cache interaction keeps both
	// diamonds at n/a, with no warning 0%.
	baseline = len(entries)
	state.footerResetBaseline = &baseline
	state.footerResetEpoch++
	state.footerHitCache = nil
	entries = append(entries, assistantEntry(usageMap("input", 1000, "cacheRead", 0, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[DIM:◆] [DIM:n/a] [DIM:◇] [DIM:n/a]")
	requireNotContains(t, line, "[WARN:0%]")

	// 9. Cache lands again inside the gated window: the gate reopens.
	entries = append(entries, assistantEntry(usageMap("input", 100, "cacheRead", 100, "cacheWrite", 0)))
	line = footerRender(state, theme, map[string]string{}, 120)
	requireContains(t, line, "[TXT:8%]")
	requireContains(t, line, "[WARN:50%]")
}

func TestFooterContextColors(t *testing.T) {
	current := sdkUsage(5000, 10000, 50)
	state := footerStateWithEntries("ctx-color-model", nil, func() (*sdkContextUsage, bool) { return current, true })

	line := footerRender(state, upperTheme{}, map[string]string{}, 200)
	requireContains(t, line, "[TEXT:50%/10K]")

	current = sdkUsage(8000, 10000, 80)
	line = footerRender(state, upperTheme{}, map[string]string{}, 200)
	requireContains(t, line, "[WARNING:80%/10K]")

	current = sdkUsage(9500, 10000, 95)
	line = footerRender(state, upperTheme{}, map[string]string{}, 200)
	requireContains(t, line, "[ERROR:95%/10K]")
	requireContains(t, line, "[DIM:n/a]")
	requireContains(t, line, "[DIM:▸] [DIM:n/a]")
	requireContains(t, line, "[TEXT:ctx-color-model")
	requireNotContains(t, line, "[ERROR:n/a]")
	requireNotContains(t, line, "[WARNING:n/a]")
}

func TestFooterSegmentOrderAndHerdsman(t *testing.T) {
	state := footerStateWithEntries("tps-model", nil, boolUsageSource(sdkUsage(1000, 10000, 10)))
	statuses := map[string]string{"herdsman": "◆ Herdsman · 2 agent updates"}
	line := footerRender(state, plainTheme{}, statuses, 200)
	requireContains(t, line, "● 2 Up")
	requireContains(t, line, "▲ 10%/10K")
	requireContains(t, line, "▸ n/a")
	requireContains(t, line, "■ tps-model")

	contextIndex := strings.Index(line, "▲")
	tpsIndex := strings.Index(line, "▸")
	modelIndex := strings.Index(line, "■")
	if !(contextIndex >= 0 && tpsIndex > contextIndex && modelIndex > tpsIndex) {
		t.Fatalf("segment order is wrong: %q", line)
	}

	// A running herdsman without results shows "On"; only a herdsman that is not
	// loaded at all renders no sheep segment.
	line = footerRender(state, plainTheme{}, map[string]string{"herdsman": "◆ Herdsman"}, 200)
	requireContains(t, line, "● On")
	line = footerRender(state, plainTheme{}, map[string]string{}, 200)
	if strings.Contains(line, "●") {
		t.Fatalf("an absent herdsman renders no sheep segment: %q", line)
	}
	requireContains(t, line, "◆ n/a")

	// The thinking level merges into the model segment.
	state.footerThinking = strPtr("high")
	line = footerRender(state, plainTheme{}, map[string]string{}, 200)
	requireContains(t, line, "■ tps-model · high")

	// The last turn's estimate renders as tokens/s.
	state.lastTurnTps = strPtr("45")
	line = footerRender(state, plainTheme{}, map[string]string{}, 200)
	requireContains(t, line, "▸ 45 t/s")
}

func TestFooterRefreshForceFlags(t *testing.T) {
	state := createInstanceState(true)
	host := &fakeHost{}

	// turn_end refreshes unforced.
	applyTurnEndEvent(state, host, map[string]any{"message": assistantMessage(usageMap("input", 10, "cacheRead", 20, "cacheWrite", 0))})
	// agent_end refreshes forced.
	applyAgentEnd(state, host, map[string]any{"messages": []any{}}, false)

	requireInt(t, "renders", len(host.renders), 2)
	if host.renders[0] {
		t.Fatal("turn_end must request an unforced render")
	}
	if !host.renders[1] {
		t.Fatal("agent_end must request a forced render")
	}
}

func TestFooterTruncationNeverWraps(t *testing.T) {
	state := footerStateWithEntries("GPT-4o (OpenAI) with a very long model label", nil, boolUsageSource(sdkUsage(163072, 128000, 42)))
	line := footerRender(state, plainTheme{}, map[string]string{"herdsman": "◆ Herdsman · 3 agent updates"}, 40)
	if strings.Contains(line, "\n") {
		t.Fatal("the footer must never contain a newline")
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("a narrow footer must be truncated with an ellipsis: %q", line)
	}
}
