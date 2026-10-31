package herdsman

import (
	"regexp"
	"strings"
)

// stripVTControlCharacters mirrors Node's `util.stripVTControlCharacters`,
// which the TS sources use in three places (wake excerpts, the context text
// cleaner and the UI display cleaner).
//
// Node's implementation walks the string once, consuming a full escape sequence
// at every ESC, so a sequence is removed whole and a bare ESC disappears on its
// own. A regexp with an alternation is close but not identical (it also matches
// nothing for a truncated sequence), so the walk is reproduced literally.
func stripVTControlCharacters(value string) string {
	if !strings.Contains(value, "\x1b") {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	runes := []rune(value)
	for index := 0; index < len(runes); index++ {
		char := runes[index]
		if char != 0x1b {
			out.WriteRune(char)
			continue
		}
		// ESC: try to consume one sequence; a lone ESC consumes just itself.
		index = skipEscapeSequence(runes, index)
	}
	return out.String()
}

// skipEscapeSequence returns the index of the last rune belonging to the
// escape sequence that starts at runes[start] (an ESC).
//
// The grammar follows Node's ansi-regex subset that util.stripVTControlCharacters
// implements: `ESC [ params intermediates final` (CSI), `ESC ] ... BEL|ST`
// (OSC) and `ESC single-byte` for everything else.
func skipEscapeSequence(runes []rune, start int) int {
	index := start + 1
	if index >= len(runes) {
		return start
	}
	switch runes[index] {
	case '[':
		// CSI: parameter bytes 0x30-0x3F, intermediate bytes 0x20-0x2F, then a
		// final byte 0x40-0x7E.
		index++
		for index < len(runes) && runes[index] >= 0x30 && runes[index] <= 0x3F {
			index++
		}
		for index < len(runes) && runes[index] >= 0x20 && runes[index] <= 0x2F {
			index++
		}
		if index < len(runes) && runes[index] >= 0x40 && runes[index] <= 0x7E {
			return index
		}
		return start
	case ']':
		// OSC: terminated by BEL or by ST (ESC \).
		index++
		for index < len(runes) {
			if runes[index] == 0x07 {
				return index
			}
			if runes[index] == 0x1b && index+1 < len(runes) && runes[index+1] == '\\' {
				return index + 1
			}
			index++
		}
		return start
	default:
		if runes[index] >= 0x40 && runes[index] <= 0x5F {
			// Two-byte sequence (e.g. ESC ( ) , ESC = , ESC >).
			return index
		}
		return start
	}
}

// multilineTrailingSpacePattern is the /gm trailing-space rule shared by the
// excerpt and context cleaners.
var multilineTrailingSpacePattern = regexp.MustCompile(`(?m)[^\S\n]+$`)
