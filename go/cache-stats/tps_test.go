package cachestats

import (
	"math"
	"testing"
)

// Ports the TPS sections of test/cache-stats.mjs: the computeTurnTps oracle
// cases, the streaming decode windows, the four sanity guards and the
// consumed-once semantics of the timing stamps.

// fakeClock substitutes nowMillis so every stamp is explicit.
type fakeClock struct{ now float64 }

func (c *fakeClock) install(t *testing.T) {
	t.Helper()
	previous := nowMillis
	nowMillis = func() float64 { return c.now }
	t.Cleanup(func() { nowMillis = previous })
}

func TestComputeTurnTpsNonStreamingFallback(t *testing.T) {
	// A missing request stamp is unknown.
	requireOptionalString(t, "no t0", computeTurnTps((*float64)(nil), tpsMessage(90, nil)), nil)

	// The end-to-end window uses turnEndedAt when doneAt is absent.
	stamps := &turnTimingStamps{requestStartedAt: floatPtr(1000), turnEndedAt: floatPtr(3000)}
	requireOptionalString(t, "end-to-end", computeTurnTps(stamps, tpsMessage(90, nil)), strPtr("45"))

	// A message that is not an assistant with usage is unknown.
	requireOptionalString(t, "user", computeTurnTps(stamps, map[string]any{"role": "user"}), nil)
	requireOptionalString(t, "no usage", computeTurnTps(stamps, map[string]any{"role": "assistant"}), nil)
	requireOptionalString(t, "zero output", computeTurnTps(stamps, tpsMessage(0, nil)), nil)
}

func TestComputeTurnTpsOracleCases(t *testing.T) {
	// (a) reasoning == output with a text stream: n = 0 -> null.
	equalReasoning := &turnTimingStamps{
		firstThinkingAt: floatPtr(1000),
		firstTextAt:     floatPtr(2000),
		lastDeltaAt:     floatPtr(3000),
		doneAt:          floatPtr(3000),
	}
	requireOptionalString(t, "oracle a", computeTurnTps(equalReasoning, tpsMessage(100, 100)), nil)

	// (b) interleaved text before thinking: n = 150 over w = 2000 -> 75.
	interleaved := &turnTimingStamps{
		firstTextAt:     floatPtr(2000),
		firstThinkingAt: floatPtr(3000),
		lastDeltaAt:     floatPtr(3000),
		doneAt:          floatPtr(4000),
	}
	requireOptionalString(t, "oracle b", computeTurnTps(interleaved, tpsMessage(200, 50)), strPtr("75"))

	// (c) pure thinking with reasoning > output: not visible -> null.
	overReasoning := &turnTimingStamps{
		firstThinkingAt: floatPtr(2000),
		lastDeltaAt:     floatPtr(3000),
		doneAt:          floatPtr(3000),
	}
	requireOptionalString(t, "oracle c", computeTurnTps(overReasoning, tpsMessage(50, 80)), nil)

	// (d) the 20x guard uses (firstText - firstAny), not (firstText -
	// firstThinking): firstAny == firstText == 1000 passes (166.7).
	denominator := &turnTimingStamps{
		firstThinkingAt: floatPtr(1500),
		firstTextAt:     floatPtr(1000),
		lastDeltaAt:     floatPtr(1500),
		doneAt:          floatPtr(1600),
	}
	requireOptionalString(t, "oracle d", computeTurnTps(denominator, tpsMessage(101, 1)), strPtr("166.7"))
}

// decodeOfRef is the reference copied from the magpie binary's decodeOf, as the
// TS differential test carries it.
func decodeOfRef(ttft, firstReasoning, firstText, flow, ms, out, think float64) *string {
	visible := think > 0 && !(firstText > 0) && firstReasoning >= ttft && flow > 0 && think <= out
	n := out
	if think > 0 && !visible {
		n = out - think
	}
	from := ttft
	if visible {
		from = firstReasoning
	} else if think > 0 {
		from = firstText
	}
	w := ms - from
	if !(n > 0 && ttft > 0 && from > 0 && w >= 100 && n*1000 <= 10000*w) {
		return nil
	}
	if flow > 0 && flow*4 < w {
		return nil
	}
	if think > 0 && firstText > 0 && n*(firstText-ttft) > 20*think*w {
		return nil
	}
	return formatTps(n, w)
}

func TestComputeTurnTpsMatchesMagpieDecodeOf(t *testing.T) {
	type sample struct {
		name            string
		firstThinkingAt *float64
		firstTextAt     *float64
		lastDeltaAt     *float64
		doneAt          *float64
		out             float64
		think           float64
	}
	samples := []sample{
		{"S1", floatPtr(6402), floatPtr(8240), floatPtr(18065), floatPtr(17575), 989, 141},
		{"S2", floatPtr(5504), floatPtr(11989), floatPtr(12694), floatPtr(12694), 467, 370},
		{"S3", nil, floatPtr(6418), floatPtr(18118), floatPtr(18118), 834, 0},
		{"pure-thinking-visible", floatPtr(2000), nil, floatPtr(3000), floatPtr(3000), 80, 80},
		{"oracle-a-equal-reasoning-out", floatPtr(1000), floatPtr(2000), floatPtr(3000), floatPtr(3000), 100, 100},
		{"oracle-b-text-before-thinking", floatPtr(3000), floatPtr(2000), floatPtr(3000), floatPtr(4000), 200, 50},
		{"oracle-c-pure-thinking-exceeds-out", floatPtr(2000), nil, floatPtr(3000), floatPtr(3000), 50, 80},
		{"guard-20x-breach", floatPtr(1000), floatPtr(2000), floatPtr(2100), floatPtr(2100), 510, 10},
		{"oracle-d-interleaved-denom", floatPtr(1500), floatPtr(1000), floatPtr(1500), floatPtr(1600), 101, 1},
	}
	for _, testCase := range samples {
		var ttft float64
		switch {
		case testCase.firstThinkingAt != nil && testCase.firstTextAt != nil:
			ttft = *testCase.firstThinkingAt
			if *testCase.firstTextAt < ttft {
				ttft = *testCase.firstTextAt
			}
		case testCase.firstThinkingAt != nil:
			ttft = *testCase.firstThinkingAt
		case testCase.firstTextAt != nil:
			ttft = *testCase.firstTextAt
		}
		ms := 0.0
		if testCase.doneAt != nil {
			ms = *testCase.doneAt
		} else if testCase.lastDeltaAt != nil {
			ms = *testCase.lastDeltaAt
		}
		flowEnd := ms
		if testCase.lastDeltaAt != nil {
			flowEnd = *testCase.lastDeltaAt
		}
		flow := flowEnd - ttft
		firstReasoning := 0.0
		if testCase.firstThinkingAt != nil {
			firstReasoning = *testCase.firstThinkingAt
		}
		firstText := 0.0
		if testCase.firstTextAt != nil {
			firstText = *testCase.firstTextAt
		}
		want := decodeOfRef(ttft, firstReasoning, firstText, flow, ms, testCase.out, testCase.think)
		stamps := &turnTimingStamps{
			firstThinkingAt: testCase.firstThinkingAt,
			firstTextAt:     testCase.firstTextAt,
			lastDeltaAt:     testCase.lastDeltaAt,
			doneAt:          testCase.doneAt,
		}
		got := computeTurnTps(stamps, tpsMessage(testCase.out, testCase.think))
		requireOptionalString(t, testCase.name, got, want)
	}
}

func TestTurnTpsSlotLifecycle(t *testing.T) {
	clock := &fakeClock{}
	clock.install(t)
	state := createInstanceState(true)
	host := &fakeHost{}

	// No completed round yet.
	requireOptionalString(t, "initial", state.lastTurnTps, nil)

	// turn_end without a recorded request stamp -> n/a.
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
	requireOptionalString(t, "no t0", state.lastTurnTps, nil)

	// A request stamp plus assistant output gives the estimate.
	clock.now = 2_000_000
	applyProviderRequestTiming(state)
	clock.now = 2_002_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
	requireOptionalString(t, "45 t/s", state.lastTurnTps, strPtr("45"))

	// The last request's stamp wins in a tool loop.
	clock.now = 3_000_000
	applyProviderRequestTiming(state)
	clock.now = 3_001_000
	applyProviderRequestTiming(state)
	clock.now = 3_003_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(120, nil)})
	requireOptionalString(t, "last t0", state.lastTurnTps, strPtr("60"))

	// Zero output tokens -> n/a.
	clock.now = 4_000_000
	applyProviderRequestTiming(state)
	clock.now = 4_001_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(0, nil)})
	requireOptionalString(t, "zero output", state.lastTurnTps, nil)

	// An assistant without usage clears the estimate.
	clock.now = 5_000_000
	applyProviderRequestTiming(state)
	clock.now = 5_001_000
	applyTurnEndEvent(state, host, map[string]any{"message": map[string]any{"role": "assistant"}})
	requireOptionalString(t, "no usage", state.lastTurnTps, nil)

	// resetRunState clears the estimate.
	clock.now = 6_000_000
	applyProviderRequestTiming(state)
	clock.now = 6_002_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
	requireOptionalString(t, "before reset", state.lastTurnTps, strPtr("45"))
	resetRunState(state)
	requireOptionalString(t, "after reset", state.lastTurnTps, nil)
}

func TestTurnTpsConsumedExactlyOnce(t *testing.T) {
	clock := &fakeClock{}
	clock.install(t)
	state := createInstanceState(true)
	host := &fakeHost{}

	// Turn 1: a fresh request stamp is consumed by turn_end.
	clock.now = 10_000_000
	applyProviderRequestTiming(state)
	clock.now = 10_001_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
	requireOptionalString(t, "turn 1", state.lastTurnTps, strPtr("90"))

	// Turn 2: no new request, a far-advanced clock. A repeated turn_end must
	// not re-measure the consumed stamp.
	clock.now = 10_060_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
	requireOptionalString(t, "turn 2", state.lastTurnTps, nil)

	// Turn 3: a fresh request re-records the stamp.
	clock.now = 11_000_000
	applyProviderRequestTiming(state)
	clock.now = 11_000_500
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(100, nil)})
	requireOptionalString(t, "turn 3", state.lastTurnTps, strPtr("200"))
}

func TestTurnTpsConsumedStreamingStamps(t *testing.T) {
	clock := &fakeClock{}
	clock.install(t)
	state := createInstanceState(true)
	host := &fakeHost{}

	clock.now = 12_000_000
	applyProviderRequestTiming(state)
	clock.now = 12_000_200
	applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "t"}, int64(clock.now))
	clock.now = 12_000_500
	applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "a"}, int64(clock.now))
	clock.now = 12_001_500
	applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "b"}, int64(clock.now))
	clock.now = 12_001_500
	applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
	clock.now = 12_001_500
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(100, 20)})
	requireOptionalString(t, "streaming", state.lastTurnTps, strPtr("80"))

	// A repeated turn_end with no new stamps renders n/a.
	clock.now = 12_060_000
	applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(100, 20)})
	requireOptionalString(t, "repeated", state.lastTurnTps, nil)
}

func TestTurnTpsStreamingWindows(t *testing.T) {
	t.Run("ttft excluded", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 20_000_000
		applyProviderRequestTiming(state)
		clock.now = 20_005_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": ""}, int64(clock.now))
		clock.now = 20_006_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "Hello"}, int64(clock.now))
		clock.now = 20_007_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": " world"}, int64(clock.now))
		applyMessageUpdate(state, map[string]any{"type": "done"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
		requireOptionalString(t, "ttft", state.lastTurnTps, strPtr("90"))

		// toolcall_delta folds into the text decode window.
		clock.now = 20_010_000
		applyProviderRequestTiming(state)
		clock.now = 20_013_000
		applyMessageUpdate(state, map[string]any{"type": "toolcall_delta", "delta": "{\"a\":"}, int64(clock.now))
		clock.now = 20_014_000
		applyMessageUpdate(state, map[string]any{"type": "toolcall_delta", "delta": "1}"}, int64(clock.now))
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(90, nil)})
		requireOptionalString(t, "toolcall", state.lastTurnTps, strPtr("90"))
	})

	t.Run("pure thinking stream", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 30_000_000
		applyProviderRequestTiming(state)
		clock.now = 30_002_000
		applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "step 1"}, int64(clock.now))
		clock.now = 30_003_000
		applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "step 2"}, int64(clock.now))
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(80, 80)})
		requireOptionalString(t, "thinking", state.lastTurnTps, strPtr("80"))
	})

	t.Run("split reasoning", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 40_000_000
		applyProviderRequestTiming(state)
		clock.now = 40_001_000
		applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "think"}, int64(clock.now))
		clock.now = 40_002_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "answer"}, int64(clock.now))
		clock.now = 40_003_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": " done"}, int64(clock.now))
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(150, 50)})
		requireOptionalString(t, "split", state.lastTurnTps, strPtr("100"))

		// Missing reasoning tokens falls back to the full window (75).
		clock.now = 41_000_000
		applyProviderRequestTiming(state)
		clock.now = 41_001_000
		applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "think"}, int64(clock.now))
		clock.now = 41_002_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "answer"}, int64(clock.now))
		clock.now = 41_003_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": " done"}, int64(clock.now))
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(150, nil)})
		requireOptionalString(t, "fallback", state.lastTurnTps, strPtr("75"))
	})

	t.Run("message_end locks doneAt", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 50_000_000
		applyProviderRequestTiming(state)
		clock.now = 50_001_000
		applyMessageUpdate(state, map[string]any{"type": "toolcall_delta", "delta": "{\"cmd\":"}, int64(clock.now))
		clock.now = 50_002_000
		applyMessageUpdate(state, map[string]any{"type": "toolcall_delta", "delta": "\"run\"}"}, int64(clock.now))
		// message_end fires when the stream finishes, before the tools run.
		clock.now = 50_002_000
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		clock.now = 50_002_100
		state.liveRun = emptyLiveRun()
		clock.now = 50_011_000
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(100, nil)})
		requireOptionalString(t, "tool latency excluded", state.lastTurnTps, strPtr("100"))
	})
}

func TestTurnTpsSanityGuards(t *testing.T) {
	t.Run("guard 1: window below 100ms", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 60_000_000
		applyProviderRequestTiming(state)
		clock.now = 60_000_500
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "a"}, int64(clock.now))
		clock.now = 60_000_550
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(50, nil)})
		requireOptionalString(t, "guard 1", state.lastTurnTps, nil)
	})

	t.Run("guard 2: over 10,000 t/s", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 61_000_000
		applyProviderRequestTiming(state)
		clock.now = 61_000_500
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "a"}, int64(clock.now))
		clock.now = 61_000_600
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(1500, nil)})
		requireOptionalString(t, "guard 2", state.lastTurnTps, nil)
	})

	t.Run("guard 3: flow below a quarter of the window", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 62_000_000
		applyProviderRequestTiming(state)
		clock.now = 62_001_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "a"}, int64(clock.now))
		clock.now = 62_001_200
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "b"}, int64(clock.now))
		clock.now = 62_002_000
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(50, nil)})
		requireOptionalString(t, "guard 3", state.lastTurnTps, nil)
	})

	t.Run("guard 4: 20x firstText guard", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 63_000_000
		applyProviderRequestTiming(state)
		clock.now = 63_001_000
		applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "t"}, int64(clock.now))
		clock.now = 63_002_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "a"}, int64(clock.now))
		clock.now = 63_002_100
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "b"}, int64(clock.now))
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(510, 10)})
		requireOptionalString(t, "guard 4", state.lastTurnTps, nil)
	})

	t.Run("retry clears partial stamps", func(t *testing.T) {
		clock := &fakeClock{}
		clock.install(t)
		state := createInstanceState(true)
		host := &fakeHost{}
		clock.now = 64_000_000
		applyProviderRequestTiming(state)
		clock.now = 64_000_500
		applyMessageUpdate(state, map[string]any{"type": "thinking_delta", "delta": "aborted"}, int64(clock.now))
		clock.now = 64_002_000
		// A retry clears the half-recorded thinking stamp.
		applyProviderRequestTiming(state)
		clock.now = 64_003_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "ok"}, int64(clock.now))
		clock.now = 64_004_000
		applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "!"}, int64(clock.now))
		applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(clock.now))
		applyTurnEndEvent(state, host, map[string]any{"message": tpsMessage(80, nil)})
		requireOptionalString(t, "retry", state.lastTurnTps, strPtr("80"))
	})
}

func TestTurnTimingResetFields(t *testing.T) {
	state := createInstanceState(true)
	requireOptionalString(t, "init providerRequestStartedAt", millisToString(state.providerRequestStartedAt), nil)
	if state.streamFirstThinkingAt != nil || state.streamFirstTextAt != nil || state.streamLastDeltaAt != nil || state.streamDoneAt != nil {
		t.Fatal("createInstanceState must initialize every streaming stamp to nil")
	}
	state.providerRequestStartedAt = intPtr64(12345)
	state.streamFirstThinkingAt = intPtr64(12350)
	state.streamFirstTextAt = intPtr64(12360)
	state.streamLastDeltaAt = intPtr64(12370)
	state.streamDoneAt = intPtr64(12380)
	state.lastTurnTps = strPtr("45")
	resetRunState(state)
	if state.providerRequestStartedAt != nil || state.streamFirstThinkingAt != nil || state.streamFirstTextAt != nil || state.streamLastDeltaAt != nil || state.streamDoneAt != nil {
		t.Fatal("resetRunState must clear every TPS scratch stamp")
	}
	requireOptionalString(t, "lastTurnTps", state.lastTurnTps, nil)
}

func intPtr64(value int64) *int64 { return &value }

func millisToString(value *int64) *string {
	if value == nil {
		return nil
	}
	converted := jsNumberString(float64(*value))
	return &converted
}

// TestJSNumberStringMatchesNumberToString pins jsNumberString against
// ECMAScript Number::toString(10). Every "want" was produced by running
// String(v) in Node, so these are cross-language vectors rather than a
// restatement of the implementation.
//
// Two defects motivated this test:
//
//   - an int64(v) conversion for integral values below 1e21 overflowed for
//     v >= 2^63, collapsing 1e19, 1e20, -1e20 (and every other value in
//     [2^63, 1e21)) onto "-9223372036854775808";
//   - Go's %g/'g' exponent form padded the exponent to two digits (1e-07) where
//     JS prints 1e-7.
func TestJSNumberStringMatchesNumberToString(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		// The overflow band: [2^63, 1e21) must render its real digits.
		{1e19, "10000000000000000000"},
		{1e20, "100000000000000000000"},
		{-1e20, "-100000000000000000000"},
		{math.Pow(2, 63), "9223372036854776000"},
		{float64(math.MaxInt64), "9223372036854776000"},
		{1e18, "1000000000000000000"},
		{12345678901234567890, "12345678901234567000"},
		{5e20, "500000000000000000000"},
		{9.9e20, "990000000000000000000"},
		{-1e19, "-10000000000000000000"},
		{9.2e18, "9200000000000000000"},
		// Plain notation boundary at the top: JS switches at 1e21.
		{1e21, "1e+21"},
		{1.5e21, "1.5e+21"},
		{2e21, "2e+21"},
		{-1e21, "-1e+21"},
		{1e20 + 1, "100000000000000000000"},
		// Plain notation boundary at the bottom: JS switches at 1e-7.
		{1e-7, "1e-7"},
		{2.5e-7, "2.5e-7"},
		{-1e-7, "-1e-7"},
		{1e-6, "0.000001"},
		{1.234e-6, "0.000001234"},
		// Ordinary values the walker and the window formatter pass through.
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{-1, "-1"},
		{100, "100"},
		{0.1, "0.1"},
		{0.5, "0.5"},
		{-0.5, "-0.5"},
		{123.456, "123.456"},
		{1.5, "1.5"},
		// Non-finite values keep the JS spellings.
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
	}
	for _, testCase := range cases {
		if got := jsNumberString(testCase.value); got != testCase.want {
			t.Errorf("jsNumberString(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// TestJSNumberStringLargeIntegersStayDistinct is the aliasing regression: the
// int64 overflow made every float64 in [2^63, 1e21) format identically, which
// in turn made distinct tool arguments hash to one fingerprint.
func TestJSNumberStringLargeIntegersStayDistinct(t *testing.T) {
	seen := map[string]float64{}
	values := []float64{1e18, 1e19, 1e20, 5e20, 9.9e20, -1e19, -1e20, math.Pow(2, 63), 9.2e18}
	for _, value := range values {
		text := jsNumberString(value)
		if previous, dup := seen[text]; dup {
			t.Fatalf("jsNumberString collision: %v and %v both render as %q", previous, value, text)
		}
		seen[text] = value
	}
	// The value that used to be produced for all of them must not appear at all.
	if _, present := seen["-9223372036854775808"]; present {
		t.Fatal("jsNumberString still overflows large integers onto -9223372036854775808")
	}
}
