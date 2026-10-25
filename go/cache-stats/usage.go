package cachestats

import (
	"math"
	"strconv"
	"strings"
)

// This file ports the pure cache-hit-rate helpers of cache-stats.ts
// (TS L40-330): normalizeUsage, aggregateHit, scanLiveHitRates,
// computeLiveHitRates, cacheHitDenom, cacheHitPct and extractAndNormalizeUsage.

// usage is the normalized usage payload of one provider round.
type usage struct {
	input       float64
	output      float64
	cacheRead   float64
	cacheWrite  float64
	reasoning   float64
	totalTokens float64
	total       float64
}

// rawUsageKeys is the ordered list of keys normalizeUsage consults, in the
// order the TS `??` chain reads them.
var (
	rawCacheWriteKeys = []string{"cacheWrite", "cache_creation_input_tokens", "cache_write_input_tokens", "prompt_cache_miss_tokens"}
	rawInputKeys      = []string{"input", "prompt_tokens", "input_tokens", "promptTokens"}
	rawOutputKeys     = []string{"output", "completion_tokens", "output_tokens", "completionTokens"}
	rawReasoningKeys  = []string{"reasoning", "reasoning_tokens", "reasoningTokens"}
	rawTotalKeys      = []string{"totalTokens", "total_tokens", "total"}
)

// The cacheRead `??` chain interleaves three flat keys with the nested
// prompt_tokens_details.cached_tokens, so normalizeUsage reads it explicitly:
// cacheRead, then prompt_tokens_details.cached_tokens, then cached_tokens,
// cache_read_input_tokens and prompt_cache_hit_tokens.
var rawCacheReadTailKeys = []string{"cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"}

// normalizeUsage normalizes the diverse provider usage payloads (Pi native,
// OpenAI prompt_tokens/completion_tokens/total_tokens, Anthropic cache
// details) to one structure. TS: normalizeUsage (L62-137).
func normalizeUsage(u any) usage {
	if u == nil || !isObjectValue(u) {
		return usage{}
	}
	// u.prompt_tokens_details?.cached_tokens and the other nested detail fields
	// sit between the flat cache keys in the TS `??` chain.
	cacheRead := pickNumber(u, "cacheRead")
	if !cacheRead.ok {
		cacheRead = nestedNumber(u, "prompt_tokens_details", "cached_tokens")
	}
	if !cacheRead.ok {
		cacheRead = pickNumber(u, rawCacheReadTailKeys...)
	}
	cacheWrite := pickNumber(u, rawCacheWriteKeys...)
	input := pickNumber(u, rawInputKeys...)
	output := pickNumber(u, rawOutputKeys...)

	reasoning := pickNumber(u, rawReasoningKeys...)
	if !reasoning.ok {
		reasoning = nestedNumber(u, "completion_tokens_details", "reasoning_tokens")
	}
	if !reasoning.ok {
		reasoning = nestedNumber(u, "output_tokens_details", "reasoning_tokens")
	}

	// TS: `Number(u.totalTokens ?? u.total_tokens ?? u.total ?? (input +
	// output + cacheRead + cacheWrite)) || (input + output + cacheRead +
	// cacheWrite)`. The `||` fallback fires both when no key matched and when
	// the coerced value is falsy: 0, NaN and -0 all fall back to the sum. The
	// sum is built from the already `Number(v) || 0`-normalized four terms, so a
	// NaN term contributes 0 rather than poisoning the total.
	//
	// A negative hit is truthy in JS and is returned as is, so the only reasons
	// to fall back are "no key produced a number" (raw == nil) and "the number
	// is zero" (which covers NaN, as orZero collapses it).
	inTok := input.zero()
	outTok := output.zero()
	crTok := cacheRead.zero()
	cwTok := cacheWrite.zero()
	sum := inTok + outTok + crTok + cwTok
	totalTokens := sum
	if raw := pickNumber(u, rawTotalKeys...).raw(); raw != nil {
		if coerced := orZero(jsNumber(raw)); coerced != 0 {
			totalTokens = coerced
		}
	}
	return usage{
		input:       inTok,
		output:      outTok,
		cacheRead:   crTok,
		cacheWrite:  cwTok,
		reasoning:   clampNonNegValue(reasoning.raw()),
		totalTokens: totalTokens,
		total:       totalTokens,
	}
}

// numberResult is the outcome of one `Number(v) || 0` step of the TS chain.
type numberResult struct {
	value  float64
	ok     bool // a key was present and non-nullish
	finite bool // the coerced number is finite
}

// raw is the coerced number when one was produced, else 0.
func (r numberResult) raw() any {
	if !r.ok {
		return nil
	}
	return r.value
}

// zero is `Number(v) || 0`: 0 when nothing was found, and 0 for NaN as well.
func (r numberResult) zero() float64 {
	if !r.ok {
		return 0
	}
	return orZero(r.value)
}

// pickNumber returns the first present non-nullish value among keys, coerced
// with Number(). ok reports that a key was found; finite reports that the
// coercion produced a finite number (a non-numeric string yields NaN, as
// Number("abc") does).
func pickNumber(obj any, keys ...string) numberResult {
	for _, key := range keys {
		value, present := objectLookup(obj, key)
		if !present || value == nil {
			continue
		}
		n := jsNumber(value)
		return numberResult{value: n, ok: true, finite: !math.IsNaN(n)}
	}
	return numberResult{}
}

// nestedNumber reads obj[outer][inner], the TS optional-chained detail fields.
func nestedNumber(obj any, outer, inner string) numberResult {
	details, present := objectLookup(obj, outer)
	if !present || details == nil || !isObjectValue(details) {
		return numberResult{}
	}
	value, present := objectLookup(details, inner)
	if !present || value == nil {
		return numberResult{}
	}
	n := jsNumber(value)
	return numberResult{value: n, ok: true, finite: !math.IsNaN(n)}
}

// orZero is JS `n || 0`: NaN and -0 collapse to 0.
func orZero(n float64) float64 {
	if math.IsNaN(n) || n == 0 {
		return 0
	}
	return n
}

// clampNonNegValue is clampNonNeg (TS L58-61): a finite and strictly positive
// number, else 0.
func clampNonNegValue(v any) float64 {
	n := jsNumber(v)
	if !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
		return n
	}
	return 0
}

// aggregateHit computes the aggregate percentage cache hit rate (0-100,
// rounded to an integer). It returns nil when totalDenom <= 0. TS: aggregateHit
// (L142-145).
func aggregateHit(totalRead, totalDenom float64) *int {
	if totalDenom <= 0 {
		return nil
	}
	value := int(math.Round(totalRead / totalDenom * 100))
	return &value
}

// cacheHitDenom computes input + cacheRead + cacheWrite, the denominator the
// hit rate divides by. TS: cacheHitDenom (L228-250); the Go form takes a raw
// usage value (the map form) or explicit numbers.
func cacheHitDenom(usageOrInput any, cacheRead, cacheWrite *float64) float64 {
	if _, isNumber := usageOrInput.(float64); isNumber {
		input := jsNumber(usageOrInput)
		read, write := 0.0, 0.0
		if cacheRead != nil {
			read = jsNumber(*cacheRead)
		}
		if cacheWrite != nil {
			write = jsNumber(*cacheWrite)
		}
		return orZero(input) + orZero(read) + orZero(write)
	}
	norm := normalizeUsage(usageOrInput)
	return norm.input + norm.cacheRead + norm.cacheWrite
}

// cacheHitDenomNumbers is the numeric-argument overload: cacheHitDenom(input,
// cacheRead, cacheWrite).
func cacheHitDenomNumbers(input, cacheRead, cacheWrite float64) float64 {
	return orZero(jsNumber(input)) + orZero(jsNumber(cacheRead)) + orZero(jsNumber(cacheWrite))
}

// cacheHitPct computes the percentage cache hit rate, or nil when the
// denominator is <= 0. TS: cacheHitPct (L252-266).
func cacheHitPct(usageOrInput any) *int {
	norm := normalizeUsage(usageOrInput)
	return aggregateHit(norm.cacheRead, cacheHitDenom(usageOrInput, nil, nil))
}

// cacheHitPctNumbers is the numeric-argument overload: cacheHitPct(input,
// cacheRead, cacheWrite).
func cacheHitPctNumbers(input, cacheRead, cacheWrite float64) *int {
	denom := cacheHitDenomNumbers(input, cacheRead, cacheWrite)
	return aggregateHit(orZero(jsNumber(cacheRead)), denom)
}

// liveHitScan is the accumulated counters of one window scan.
type liveHitScan struct {
	totalCacheRead  float64
	totalCacheWrite float64
	totalDenom      float64
	latest          *int
}

// scanLiveHitRates scans session entries from start (inclusive) and
// accumulates the cache counters. An assistant message sets latest
// unconditionally; a toolResult / branch_summary / compaction round counts
// towards the aggregate only. TS: scanLiveHitRates (L166-224).
func scanLiveHitRates(entries []any, start int) liveHitScan {
	var scan liveHitScan
	for i := start; i < len(entries); i++ {
		entry, ok := entries[i].(map[string]any)
		if !ok {
			// TS also tolerates a non-object entry (continue).
			if _, isObject := asObject(entries[i]); !isObject {
				continue
			}
			continue
		}
		if entryType, _ := entry["type"].(string); entryType == "message" {
			message, ok := entry["message"].(map[string]any)
			if !ok {
				continue
			}
			role, _ := message["role"].(string)
			rawUsage := message["usage"]
			switch {
			case role == "assistant" && rawUsage != nil:
				norm := normalizeUsage(rawUsage)
				denom := cacheHitDenomOf(norm)
				scan.totalCacheRead += norm.cacheRead
				scan.totalCacheWrite += norm.cacheWrite
				scan.totalDenom += denom
				// A measured round always owns latest: with denom > 0 it reports
				// aggregateHit (0% when no cache was in play), and denom == 0
				// leaves it unknown.
				if denom > 0 {
					scan.latest = aggregateHit(norm.cacheRead, denom)
				} else {
					scan.latest = nil
				}
			case role == "assistant":
				// Assistant without usage: data missing -> latest is unknown.
				scan.latest = nil
			case role == "toolResult" && rawUsage != nil:
				norm := normalizeUsage(rawUsage)
				denom := cacheHitDenomOf(norm)
				scan.totalCacheRead += norm.cacheRead
				scan.totalCacheWrite += norm.cacheWrite
				scan.totalDenom += denom
			}
			continue
		}
		entryType, _ := entry["type"].(string)
		if (entryType == "branch_summary" || entryType == "compaction") && entry["usage"] != nil {
			norm := normalizeUsage(entry["usage"])
			denom := cacheHitDenomOf(norm)
			scan.totalCacheRead += norm.cacheRead
			scan.totalCacheWrite += norm.cacheWrite
			scan.totalDenom += denom
		}
	}
	return scan
}

// cacheHitDenomOf is cacheHitDenom over an already normalized usage.
func cacheHitDenomOf(norm usage) float64 {
	return norm.input + norm.cacheRead + norm.cacheWrite
}

// hitRates is {aggregate, latest}; a nil field is the TS null (n/a).
type hitRates struct {
	aggregate *int
	latest    *int
}

// computeLiveHitRates computes the live aggregate and latest cache hit rates
// from session entries. resetBaseline (the entry count at reset time) restricts
// the scan to entries appended after a /cache-stats reset; nil is the TS
// undefined. TS: computeLiveHitRates (L276-317).
func computeLiveHitRates(entries []any, resetBaseline *int) hitRates {
	if entries == nil || len(entries) == 0 {
		return hitRates{}
	}
	baseline := 0
	if resetBaseline != nil {
		baseline = *resetBaseline
		if baseline >= 0 && len(entries) < baseline {
			return hitRates{}
		}
	}
	start := 0
	if resetBaseline != nil && baseline > 0 {
		start = baseline
	}
	scan := scanLiveHitRates(entries, start)
	seenCacheInteraction := scan.totalCacheRead > 0 || scan.totalCacheWrite > 0
	var aggregate *int
	if scan.totalDenom > 0 && seenCacheInteraction {
		aggregate = aggregateHit(scan.totalCacheRead, scan.totalDenom)
	}
	var latest *int
	if seenCacheInteraction {
		latest = scan.latest
	}
	return hitRates{aggregate: aggregate, latest: latest}
}

// extractAndNormalizeUsage sums the usage of the assistant messages in raw
// (one message, or an array of messages or bare usage payloads). TS:
// extractAndNormalizeUsage (L812-856).
func extractAndNormalizeUsage(raw any) usage {
	var out usage
	if raw == nil {
		return out
	}
	items := []any{raw}
	if array, ok := raw.([]any); ok {
		items = array
	}
	for _, item := range items {
		obj, ok := asObject(item)
		if !ok {
			continue
		}
		var source any
		if _, hasRole := obj.lookup("role"); hasRole {
			role, _ := obj.lookup("role")
			if role != "assistant" {
				continue
			}
			rawUsage, hasUsage := obj.lookup("usage")
			if !hasUsage || rawUsage == nil {
				continue
			}
			source = rawUsage
		} else if rawUsage, hasUsage := obj.lookup("usage"); hasUsage && rawUsage != nil {
			source = rawUsage
		} else {
			source = item
		}
		norm := normalizeUsage(source)
		writeBackUsage(source, norm)
		out.input += norm.input
		out.output += norm.output
		out.cacheRead += norm.cacheRead
		out.cacheWrite += norm.cacheWrite
	}
	return out
}

// writeBackUsage fills the normalized fields the raw usage object did not
// carry, mirroring recordTurnUsage's write-back (TS L657-673). A nil or
// non-object source is left alone.
func writeBackUsage(source any, norm usage) {
	obj, ok := source.(map[string]any)
	if !ok {
		return
	}
	setIfAbsent(obj, "input", norm.input)
	setIfAbsent(obj, "output", norm.output)
	setIfAbsent(obj, "cacheRead", norm.cacheRead)
	setIfAbsent(obj, "cacheWrite", norm.cacheWrite)
	setIfAbsent(obj, "totalTokens", norm.totalTokens)
	setIfAbsent(obj, "total", norm.totalTokens)
}

// setIfAbsent writes key when the map has no value for it yet.
func setIfAbsent(obj map[string]any, key string, value float64) {
	if _, exists := obj[key]; !exists {
		obj[key] = value
	}
}

// isObjectValue reports whether value is a JSON object rather than an array or
// a scalar (typeof value === "object" && value !== null && !Array.isArray).
func isObjectValue(value any) bool {
	_, ok := asObject(value)
	return ok
}

// jsNumber is JavaScript's Number(v) over a decoded JSON value. Unsupported
// values yield NaN, as Number({}) does.
func jsNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return math.NaN()
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case int32:
		return float64(typed)
	case uint64:
		return float64(typed)
	case bool:
		if typed {
			return 1
		}
		return 0
	case string:
		return jsStringToNumber(typed)
	case []any:
		// Array.prototype.toString joins the elements with "," and Number()
		// coerces that string: [] -> 0, [5] -> 5, [1,2] -> NaN.
		if len(typed) == 0 {
			return 0
		}
		if len(typed) == 1 {
			return jsNumber(typed[0])
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

// jsStringToNumber is Number(string) for an already-decoded Go string.
func jsStringToNumber(text string) float64 {
	trimmed := strings.TrimSpace(text)
	switch trimmed {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	case "NaN":
		return math.NaN()
	}
	if len(trimmed) > 2 && (trimmed[0] == '0') && (trimmed[1] == 'x' || trimmed[1] == 'X') {
		if parsed, err := strconv.ParseUint(trimmed[2:], 16, 64); err == nil {
			return float64(parsed)
		}
		return math.NaN()
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return math.NaN()
	}
	return parsed
}
