package cachestats

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports the pure formatting helpers of cache-stats.ts: formatTps,
// formatWindowSize, formatHerdsmanStatus (with sheepMeat and stripProvider),
// contextSegment, truncateFooter, and the turn-speed estimator computeTurnTps
// (TS L432-520 and L700-810).

// nowMillis is Date.now(). Tests substitute it to drive the streaming
// estimator deterministically; the default reads the wall clock.
var nowMillis = func() float64 { return float64(time.Now().UnixMilli()) }

// themeStyler is the slice of Pi's Theme the footer uses: fg(token, text)
// colors text with the named token. The SDK's sdk.UITheme satisfies it through
// [uiTheme]; tests substitute a recording styler.
type themeStyler interface {
	Fg(token, text string) string
}

// uiTheme adapts sdk.UITheme to themeStyler.
type uiTheme struct {
	theme interface {
		Fg(token, text string) string
	}
}

func (u uiTheme) Fg(token, text string) string { return u.theme.Fg(token, text) }

// formatWindowSize formats a context window size to a compact
// decimal-abbreviated string. No binary divisibility shortcuts are used. TS:
// formatWindowSize (L471-495).
func formatWindowSize(size float64) string {
	if math.IsNaN(size) || math.IsInf(size, 0) || size <= 0 {
		return jsNumberString(size)
	}
	if size < 1_000 {
		return jsNumberString(size)
	}
	if size < 1_000_000 {
		k := math.Round(size / 1_000)
		if k >= 1_000 {
			return "1M"
		}
		return strconv.Itoa(int(k)) + "K"
	}
	m := size / 1_000_000
	if m >= 10 {
		return strconv.Itoa(int(math.Round(m))) + "M"
	}
	// Round at integer-tenth granularity so binary float drift cannot drop a
	// value a full tenth; a trailing ".0" vanishes on its own.
	return jsNumberString(math.Round(size/100_000)/10) + "M"
}

// formatTps formats an estimated generation speed (output tokens per second)
// into a compact footer string, or nil when the estimate is impossible or
// meaningless. TS: formatTps (L497-520).
func formatTps(outputTokens, elapsedMs float64) *string {
	if math.IsNaN(outputTokens) || math.IsInf(outputTokens, 0) || outputTokens <= 0 {
		return nil
	}
	if math.IsNaN(elapsedMs) || math.IsInf(elapsedMs, 0) || elapsedMs <= 0 {
		return nil
	}
	tps := outputTokens / (elapsedMs / 1000)
	if math.IsNaN(tps) || math.IsInf(tps, 0) || tps <= 0 {
		return nil
	}
	rounded := math.Round(tps*10) / 10
	if rounded == math.Trunc(rounded) && rounded != 0 {
		value := strconv.Itoa(int(rounded))
		return &value
	}
	value := strconv.FormatFloat(rounded, 'f', 1, 64)
	return &value
}

var (
	// sheepPrefix strips the leading footer marker of the shepherd/herdsman
	// status.
	sheepPrefix = regexp.MustCompile(`^\s*[◆◇●▲■▸]?\s*(?:(?:Shepherd|Herdsman)(?:\s*[·•|｜]\s*)?)`)
	// herdsmanCount matches the returned-results forms.
	herdsmanCount = regexp.MustCompile(`(?i)(\d+)\s*(?:agent\s*)?(?:updates?|completed|done|UP)\b`)
	// herdsmanBare matches a status that is only a count.
	herdsmanBare = regexp.MustCompile(`^(\d+)$`)
)

// sheepMeat strips the leading footer marker from the shepherd/herdsman
// extension status. TS: sheepMeat (L436-438).
func sheepMeat(s string) string {
	return strings.TrimSpace(sheepPrefix.ReplaceAllString(s, ""))
}

// formatHerdsmanStatus formats the shepherd/herdsman status for the footer:
// "[count] Up" when agents returned results, "On" when running without
// results, and nil when not loaded or not running. TS: formatHerdsmanStatus
// (L446-458).
func formatHerdsmanStatus(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	meat := sheepMeat(*s)
	if match := herdsmanCount.FindStringSubmatch(meat); match != nil {
		value := match[1] + " Up"
		return &value
	}
	if match := herdsmanBare.FindStringSubmatch(meat); match != nil {
		value := match[1] + " Up"
		return &value
	}
	value := "On"
	return &value
}

var providerSuffix = regexp.MustCompile(` \([^)]*\)$`)

// stripProvider strips a trailing " (provider)" from a model display name and
// leaves inner parentheses intact. TS: stripProvider (L521-523).
func stripProvider(name string) string {
	return providerSuffix.ReplaceAllString(name, "")
}

// contextSegment colors the context segment by occupancy, mirroring Pi's
// official footer: percent > 90 error, > 70 warning, otherwise default text.
// TS: contextSegment (L529-533).
func contextSegment(theme themeStyler, percent float64, display string) string {
	if percent > 90 {
		return theme.Fg("error", display)
	}
	if percent > 70 {
		return theme.Fg("warning", display)
	}
	return theme.Fg("text", display)
}

// truncateFooter truncates a single-line footer text to width so it never
// wraps, keeping the beginning of the line and appending an ellipsis. TS:
// truncateFooter (L861-868).
func truncateFooter(line string, width int, theme themeStyler) string {
	maxWidth := width
	if maxWidth < 0 {
		maxWidth = 0
	}
	ellipsis := "…"
	if theme != nil {
		ellipsis = theme.Fg("dim", "…")
	}
	return tui.TruncateToWidth(line, maxWidth, ellipsis, false)
}

// turnTimingStamps is the TS TurnTimingStamps record. A nil field is the TS
// undefined / null.
type turnTimingStamps struct {
	requestStartedAt *float64
	firstAnyAt       *float64
	firstThinkingAt  *float64
	firstTextAt      *float64
	lastDeltaAt      *float64
	doneAt           *float64
	turnEndedAt      *float64
}

// toStamp coerces a raw value to a finite timestamp or nil. TS: toStamp
// (L722-724).
func toStamp(v any) *float64 {
	switch typed := v.(type) {
	case nil:
		return nil
	case *float64:
		if typed == nil || math.IsNaN(*typed) || math.IsInf(*typed, 0) {
			return nil
		}
		value := *typed
		return &value
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil
		}
		value := typed
		return &value
	}
	return nil
}

// computeTurnTps estimates the tokens/s of the turn that just finished: the
// output tokens of the assistant message over the elapsed time since the
// request start stamp. timing accepts a *turnTimingStamps record or a bare
// *float64 request-start stamp (the TS number overload); nil means no stamp.
// TS: computeTurnTps (L726-808).
func computeTurnTps(timing any, rawMessage any) *string {
	message, ok := asObject(rawMessage)
	if !ok {
		return nil
	}
	role, _ := message.lookup("role")
	rawUsage, hasUsage := message.lookup("usage")
	if role != "assistant" || !hasUsage || rawUsage == nil {
		return nil
	}
	norm := normalizeUsage(rawUsage)
	if math.IsNaN(norm.output) || math.IsInf(norm.output, 0) || norm.output <= 0 {
		return nil
	}

	var requestStartedAt, explicitFirstAnyAt *float64
	var firstThinkingAt, firstTextAt, lastDeltaAt, doneAt, turnEndedAt *float64
	switch typed := timing.(type) {
	case *float64:
		requestStartedAt = toStamp(typed)
	case *turnTimingStamps:
		if typed != nil {
			requestStartedAt = toStamp(typed.requestStartedAt)
			explicitFirstAnyAt = toStamp(typed.firstAnyAt)
			firstThinkingAt = toStamp(typed.firstThinkingAt)
			firstTextAt = toStamp(typed.firstTextAt)
			lastDeltaAt = toStamp(typed.lastDeltaAt)
			doneAt = toStamp(typed.doneAt)
			turnEndedAt = toStamp(typed.turnEndedAt)
		}
	}

	// Non-streaming fallback: end-to-end [requestStartedAt, doneAt ??
	// turnEndedAt ?? now].
	if firstThinkingAt == nil && firstTextAt == nil && explicitFirstAnyAt == nil {
		if requestStartedAt == nil {
			return nil
		}
		end := firstNonNil(doneAt, turnEndedAt)
		if end == nil {
			now := nowMillis()
			end = &now
		}
		return formatTps(norm.output, *end-*requestStartedAt)
	}

	// Streaming decode window, aligned to magpie's decodeOf.
	var firstAnyAt *float64
	if explicitFirstAnyAt != nil {
		firstAnyAt = explicitFirstAnyAt
	} else if firstThinkingAt != nil && firstTextAt != nil {
		value := math.Min(*firstThinkingAt, *firstTextAt)
		firstAnyAt = &value
	} else if firstThinkingAt != nil {
		firstAnyAt = firstThinkingAt
	} else {
		firstAnyAt = firstTextAt
	}
	if firstAnyAt == nil {
		return nil
	}

	end := firstNonNil(doneAt, lastDeltaAt, turnEndedAt)
	if end == nil {
		now := nowMillis()
		end = &now
	}
	flowBase := end
	if lastDeltaAt != nil {
		flowBase = lastDeltaAt
	}
	flow := *flowBase - *firstAnyAt
	think := norm.reasoning
	out := norm.output
	visible := think > 0 &&
		!(firstTextAt != nil && *firstTextAt > 0) &&
		firstThinkingAt != nil &&
		*firstThinkingAt >= *firstAnyAt &&
		flow > 0 &&
		think <= out
	n := out
	if think > 0 && !visible {
		n = out - think
	}
	var from *float64
	switch {
	case visible:
		from = firstThinkingAt
	case think > 0:
		from = firstTextAt
	default:
		from = firstAnyAt
	}
	if from == nil {
		return nil
	}

	w := *end - *from
	if !(n > 0 && *firstAnyAt > 0 && *from > 0 && w >= 100 && n*1000 <= 10000*w) {
		return nil
	}
	if flow > 0 && flow*4 < w {
		return nil
	}
	if think > 0 && firstTextAt != nil && *firstTextAt > 0 && n*(*firstTextAt-*firstAnyAt) > 20*think*w {
		return nil
	}
	return formatTps(n, w)
}

// firstNonNil returns the first non-nil pointer of the list.
func firstNonNil(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// jsNumberString is ECMAScript Number::toString(10) for the values the walker
// and the window-size fallbacks pass it: a finite number renders as the
// shortest round-tripping decimal digits, plain notation in [1e-6, 1e21) and
// exponential notation outside it. TS: String(n).
//
// No step converts to int64: a float64 at or above 2^63 (but still below
// 1e21) would overflow a signed conversion into one shared value, aliasing
// every large integer onto one string (and so onto one tool fingerprint).
// strconv.FormatFloat keeps the shortest round-tripping digits for the whole
// range, which is the same set Number::toString prints.
func jsNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "Infinity"
	}
	if math.IsInf(value, -1) {
		return "-Infinity"
	}
	if value == 0 {
		// Number::toString renders both zeros as "0" (JS has no "-0" string).
		return "0"
	}
	abs := math.Abs(value)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	// Exponential notation: Go writes the exponent as "e-07"/"e+21", JS as
	// "e-7"/"e+21", so re-spell only the negative branch's zero padding.
	text := strconv.FormatFloat(value, 'e', -1, 64)
	return canonicalizeExponent(text)
}

// canonicalizeExponent rewrites the exponent of an 'e'-formatted number from
// Go's two-digit form to ECMAScript's unpadded one (e-07 -> e-7). The positive
// branch already carries the "+" ECMAScript prints, and both keep at least one
// digit.
func canonicalizeExponent(text string) string {
	marker := strings.LastIndexByte(text, 'e')
	if marker < 0 {
		return text
	}
	mantissa, exponent := text[:marker], text[marker+1:]
	sign := ""
	if exponent != "" && (exponent[0] == '+' || exponent[0] == '-') {
		sign, exponent = exponent[:1], exponent[1:]
	}
	trimmed := strings.TrimLeft(exponent, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	return mantissa + "e" + sign + trimmed
}
