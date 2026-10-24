package ctxorchestrate

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// This file ports the small string helpers of ctx-orchestrate.ts (clampUtf16Index,
// oneLine, clampInt) whose semantics are JavaScript-specific: lengths and cuts
// are counted in UTF-16 code units, and JavaScript's \s is wider than Go's.

const (
	// ExcerptCap is the per-excerpt character cap: a digest excerpt must stay
	// short, the model fetches full content itself. TS: EXCERPT_CAP.
	ExcerptCap = 200
	// errLineCap is the cap of a degraded-pattern error message. TS: oneLine(..., 300).
	errLineCap = 300
)

// jsWSInner is the body of a character class matching JavaScript's \s
// (ECMAScript WhiteSpace + LineTerminator). Go's \s is only [\t\n\f\r ], so the
// class is spelled out; it is the same one no-tables documents.
const jsWSInner = `\t\n\f\r\v \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsWSClass is a single JavaScript \s character.
const jsWSClass = `[` + jsWSInner + `]`

var (
	// reWSRun is JavaScript's /\s+/.
	reWSRun = regexp.MustCompile(jsWSClass + `+`)
	// reTrailingWS is JavaScript's /\s+$/ (a line's trailing whitespace, incl. \r).
	reTrailingWS = regexp.MustCompile(jsWSClass + `+$`)
)

// utf16Len returns the length of s in UTF-16 code units, which is what
// JavaScript's String.prototype.length counts: a rune outside the BMP (an
// astral rune, e.g. an emoji) counts as two. The whole text-cap governance is
// specified in these units, so every comparison in this package uses it.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// truncateUTF16 returns the longest prefix of s that is at most units UTF-16
// code units long, and never splits a surrogate pair. It is the cut that
// clampUtf16Index(s, units, towardStart=true) + slice(0, cut) takes in the TS
// version: for units <= 0 the result is empty, for units >= utf16Len(s) whole s.
func truncateUTF16(s string, units int) string {
	if units <= 0 {
		return ""
	}
	if utf16Len(s) <= units {
		return s
	}
	used := 0
	for i, r := range s {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if used+width > units {
			return s[:i]
		}
		used += width
	}
	return s
}

// oneLine flattens every whitespace run to one space, trims, and truncates to
// cap UTF-16 code units with an ellipsis. TS: oneLine(text, cap = EXCERPT_CAP).
func oneLine(text string, cap int) string {
	flat := strings.TrimSpace(reWSRun.ReplaceAllString(text, " "))
	if utf16Len(flat) <= cap {
		return flat
	}
	return truncateUTF16(flat, cap-1) + "…"
}

// numberValue reads a JSON number as JavaScript's `typeof value === "number"`
// would see it. Pig decodes tool arguments and event payloads with
// encoding/json, so integral JSON numbers arrive as float64; int and
// json.Number are accepted too so unit tests and other callers can pass them.
func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case float32:
		f := float64(v)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case jsonNumber:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

// jsonNumber is the interface of encoding/json.Number (and of the SDK's own
// json.Number), matched structurally so this package needs no extra import.
type jsonNumber interface{ Float64() (float64, error) }

// clampInt clamps an integer argument into [1, hard]; a missing, non-numeric or
// below-1 value falls back to def. TS: clampInt(value, def, hard).
func clampInt(value any, def, hard int) int {
	n := def
	if f, ok := numberValue(value); ok {
		n = int(math.Floor(f))
	}
	if n < 1 {
		n = def
	}
	if n > hard {
		n = hard
	}
	return n
}

// positiveInt reports floor(value) when value is a positive finite number.
func positiveInt(value any) (int, bool) {
	f, ok := numberValue(value)
	if !ok || f <= 0 {
		return 0, false
	}
	return int(math.Floor(f)), true
}

// optStr trims a string argument and drops it when blank. TS: optStr.
func optStr(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return s
}

// toJSString is JavaScript's String(value) for the JSON values a tool argument
// can hold, so a non-string pattern becomes its JSON text rather than "<nil>".
func toJSString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}
