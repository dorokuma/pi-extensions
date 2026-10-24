package autocontinue

import (
	"math"
	"strconv"
	"strings"
)

// jsNumber mirrors JavaScript's Number(value) for the shapes that reach it from
// JSON settings and event payloads: numbers, bools, null, strings, and the
// single-element arrays JS coerces through their only element. Objects, symbols
// and multi-element arrays are NaN, as in JS.
//
// The ok result is false exactly when JS would produce NaN; a real NaN input is
// (NaN, true) so callers can keep JavaScript's "not finite" checks separate.
func jsNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		return jsStringToNumber(v)
	case []any:
		switch len(v) {
		case 0:
			return 0, true
		case 1:
			return jsNumber(v[0])
		}
		return math.NaN(), false
	case map[string]any:
		return math.NaN(), false
	}
	if number, ok := jsNumberOf(value); ok {
		return number, true
	}
	return math.NaN(), false
}

// jsStringToNumber mirrors Number(string): JS whitespace is trimmed, the empty
// string is 0, "Infinity" keeps its sign, 0x / 0b / 0o are integers, and the
// rest is a decimal float. Unlike strconv.ParseFloat it rejects Go's hex-float
// "p" exponent form, which JS reads as NaN.
func jsStringToNumber(raw string) (float64, bool) {
	s := jsTrim(raw)
	if s == "" {
		return 0, true
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	sign := 1.0
	digits := s
	if strings.HasPrefix(digits, "+") {
		digits = digits[1:]
	} else if strings.HasPrefix(digits, "-") {
		sign = -1
		digits = digits[1:]
	}
	if len(digits) > 2 && digits[0] == '0' {
		base := 0
		switch digits[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			n, err := strconv.ParseUint(digits[2:], base, 64)
			if err != nil {
				return math.NaN(), false
			}
			return sign * float64(n), true
		}
	}
	if strings.ContainsAny(s, "pP") {
		// Go's hexadecimal float notation has no JS equivalent.
		return math.NaN(), false
	}
	number, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN(), false
	}
	return number, true
}

// jsStringOf mirrors `String(value ?? "")` for the shapes errorMessage can
// take. Objects and arrays yield the empty string instead of JS's
// "[object Object]" / joined elements; error messages on the wire are strings.
func jsStringOf(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	if number, ok := jsNumberOf(value); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return ""
}

// mathIsNaN and mathIsInf keep the NaN/Infinity checks in messages.go readable.
func mathIsNaN(f float64) bool { return math.IsNaN(f) }

func mathIsInf(f float64) bool { return math.IsInf(f, 0) }

// formatJSNumber renders a number the way a JS template literal does, e.g. 500
// for 500 and "Infinity" for +Inf.
func formatJSNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
