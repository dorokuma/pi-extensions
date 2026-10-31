package herdsman

import (
	"unicode/utf8"
)

// lastRune returns the final rune of value and its encoded width. An invalid
// tail byte decodes as utf8.RuneError of width 1, mirroring how a JS string can
// carry a lone surrogate.
func lastRune(value string) (rune, int) {
	if value == "" {
		return utf8.RuneError, 0
	}
	return utf8.DecodeLastRuneInString(value)
}
