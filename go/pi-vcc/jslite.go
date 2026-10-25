// Package pivcc is a Go port of @sting8k/pi-vcc 0.10.2 (an algorithmic
// conversation compactor for pi) plus the two local patches that ship with it
// on eqi12:
//
//   - pi-vcc-0.9.0-ctx-run: trackCommands reads command text from every
//     tool_call block (args.command, args.argv, args.commands), not only from
//     the bash tool (extract/tracked-commands.go).
//   - pi-vcc-0.10.0-recall-note: RECALL_NOTE carries the extra sentence that
//     tells the model to recall this task's original instructions after every
//     compaction (format.go).
//
// Behaviour parity notes for every deliberate deviation live in README.md; the
// port's own tests pin both TS behaviour and those deviations.
package pivcc

import (
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ── JavaScript string model ─────────────────────────────────────────────────
//
// The TS original counts `.length`, `slice`, and `lastIndexOf` in UTF-16 code
// units because every JS string is a sequence of them. Go strings are UTF-8
// byte sequences, so helper functions below reproduce the code-unit view
// exactly. They are only applied where a JS index or length reaches observable
// output (clipping, wrapping, caps); plain concatenation and comparison need no
// conversion.

// jsLen is JS's `String.prototype.length`: the number of UTF-16 code units.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// jsUnits returns the UTF-16 code units of s, the representation every index
// operation below works on.
func jsUnits(s string) []uint16 {
	units := make([]uint16, 0, len(s))
	for _, r := range s {
		if r >= 0x10000 {
			hi, lo := utf16.EncodeRune(r)
			units = append(units, uint16(hi), uint16(lo))
			continue
		}
		units = append(units, uint16(r))
	}
	return units
}

// jsFromUnits rebuilds a Go string from UTF-16 code units, as a JS string
// would hold them. A lone surrogate is kept as the Unicode replacement
// character, since Go strings cannot carry an unpaired surrogate.
func jsFromUnits(units []uint16) string {
	var b strings.Builder
	b.Grow(len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xD800 && u <= 0xDBFF && i+1 < len(units) {
			lo := units[i+1]
			if lo >= 0xDC00 && lo <= 0xDFFF {
				b.WriteRune(utf16.DecodeRune(rune(u), rune(lo)))
				i++
				continue
			}
		}
		if u >= 0xD800 && u <= 0xDFFF {
			b.WriteRune(utf8.RuneError)
			continue
		}
		b.WriteRune(rune(u))
	}
	return b.String()
}

// jsSlice is JS's `String.prototype.slice(start, end)` for non-negative
// bounds (the only form this port uses).
func jsSlice(s string, start, end int) string {
	units := jsUnits(s)
	if start < 0 {
		start = 0
	}
	if end > len(units) {
		end = len(units)
	}
	if start >= end {
		return ""
	}
	return jsFromUnits(units[start:end])
}

// jsLastIndexByteSpaceBefore is `s.lastIndexOf(" ", max)`: the code-unit index
// of the last ASCII space at or before index max, or -1.
func jsLastIndexByteSpaceBefore(s string, max int) int {
	units := jsUnits(s)
	if max > len(units) {
		max = len(units)
	}
	for i := max; i > 0; i-- {
		if units[i-1] == ' ' {
			return i - 1
		}
	}
	return -1
}

// jsCharCodeAt is `s.charCodeAt(i)`; ok is false past the end.
func jsCharCodeAt(s string, i int) (uint16, bool) {
	units := jsUnits(s)
	if i < 0 || i >= len(units) {
		return 0, false
	}
	return units[i], true
}

// jsRepeat is `" ".repeat(n)`.
func jsRepeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(s, n)
}

// ── JavaScript number formatting ────────────────────────────────────────────

// jsFixed1 is `n.toFixed(1)` for the finite, non-negative values this port
// formats (token counts). JS rounds half away from zero on the decimal digit
// that strconv also rounds, so FormatFloat reproduces it.
func jsFixed1(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "NaN"
	}
	return strconv.FormatFloat(n, 'f', 1, 64)
}

// jsNumberString is JS's `String(n)` for the integral values this port emits
// (counts and indices): there is no exponent form and no trailing ".0".
func jsNumberString(n float64) string {
	if n == math.Trunc(n) && math.Abs(n) < 1e21 {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// ── Unicode helpers ─────────────────────────────────────────────────────────

// isWordLikeLetterOrNumber reproduces the word test brief.ts uses to decide
// what counts against a word budget: `/[\\p{L}\\p{N}]/u` — any letter or
// decimal digit.
func isWordLikeLetterOrNumber(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// containsLetterOrNumber is the regex-level test above for a whole segment.
func containsLetterOrNumber(s string) bool {
	for _, r := range s {
		if isWordLikeLetterOrNumber(r) {
			return true
		}
	}
	return false
}
