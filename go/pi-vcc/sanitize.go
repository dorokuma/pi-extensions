package pivcc

import "strings"

// Ports src/core/sanitize.ts.
//
// ANSI_RE is /\x1b\[[0-9;]*[A-Za-z]/g and CTRL_RE is
// /[\x00-\x08\x0b\x0c\x0e-\x1f]/g in JS; both are ASCII-only classes, so the
// byte-level rewrite below matches exactly.

const (
	ansiPrefix = "\x1b["
)

// Sanitize normalizes line endings to "\n" and strips ANSI escape sequences and
// control characters (sanitize.ts sanitize()).
func Sanitize(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = stripANSI(text)
	return stripControl(text)
}

// stripANSI removes every ESC "[" [0-9;]* [A-Za-z] run.
func stripANSI(text string) string {
	if !strings.Contains(text, ansiPrefix) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '[' {
			j := i + 2
			for j < len(text) && (text[j] == ';' || (text[j] >= '0' && text[j] <= '9')) {
				j++
			}
			if j < len(text) && ((text[j] >= 'A' && text[j] <= 'Z') || (text[j] >= 'a' && text[j] <= 'z')) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// stripControl removes the control characters CTRL_RE lists.
func stripControl(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= 0x00 && c <= 0x08) || c == 0x0b || c == 0x0c || (c >= 0x0e && c <= 0x1f) {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
