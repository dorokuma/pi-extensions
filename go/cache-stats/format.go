package cachestats

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports the pure formatting helpers of cache-stats.ts:
// formatWindowSize, formatHerdsmanStatus (with sheepMeat and stripProvider),
// contextSegment, truncateFooter and the ECMAScript Number::toString(10)
// renderer jsNumberString (TS L432-533 and L861-868).

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
