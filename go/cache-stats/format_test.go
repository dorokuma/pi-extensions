package cachestats

import (
	"math"
	"testing"
)

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
