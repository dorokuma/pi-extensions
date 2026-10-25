package pivcc

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ── JS RegExp semantics on RE2 ──────────────────────────────────────────────
//
// Go's regexp (RE2) differs from a JavaScript RegExp in four ways that reach
// this port's observable output:
//
//  1. `\b` is a word boundary over ASCII word characters. JS's `\b` (without
//     the /u flag) uses the same ASCII definition, so both agree — but Go's
//     `\w` inside a character class is also ASCII-only, which matches.
//  2. `.` never matches a line terminator in JS (with or without /s). Go's `.`
//     does not match "\n" but does match "\r", U+2028 and U+2029. Patterns
//     here that use `.` next to user text therefore spell the JS terminator
//     class out as [^\n\r\x{2028}\x{2029}].
//  3. `\s` in JS covers the Unicode space separators plus U+FEFF; Go's `\s`
//     is ASCII-only. Every pattern below spells JS's set out as a class.
//  4. JS `i` folds case with Unicode simple case folding; Go's `(?i)` does the
//     same for the ASCII and Latin-1 ranges this extension matches.
//  5. RE2 has no `\uXXXX` escape, so the JS patterns' Unicode escapes are
//     written as `\x{...}` here.
//
// The replacements are named so the source stays close to the TS original.

const (
	// jsDot is JS's `.` (no /s flag): anything but a line terminator.
	jsDot = `[^\n\r\x{2028}\x{2029}]`
	// jsSpace is JS's `\s`: Unicode whitespace plus the BOM.
	jsSpace = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	// jsSpaceChars is jsSpace's body, for pasting into another character class.
	jsSpaceChars = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`
	// jsNotSpace is JS's `\S`.
	jsNotSpace = `[^\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	// jsWord is JS's `\w`.
	jsWord = `[A-Za-z0-9_]`
)

// mustCompile panics on a bad pattern. Every pattern in this package is a
// literal, so a failure is a programming error caught by the tests.
func mustCompile(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

// compileIgnoreCase compiles a pattern with JS's `i` flag.
func compileIgnoreCase(pattern string) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + pattern)
}

// jsTrimSpace trims the code points JS's `String.prototype.trim` removes:
// WhiteSpace and LineTerminator, which is Go's unicode.IsSpace plus "\uFEFF".
func jsTrimSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF'
	})
}

// jsTrimLeftSpace trims the same set from the left.
func jsTrimLeftSpace(s string) string {
	return strings.TrimLeftFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF'
	})
}

// jsTrimRightSpace trims the same set from the right.
func jsTrimRightSpace(s string) string {
	return strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF'
	})
}

// jsIsSpaceRune reports whether r is JS `\s`.
func jsIsSpaceRune(r rune) bool {
	if r == '\uFEFF' {
		return true
	}
	return unicode.IsSpace(r)
}

// jsIsLineTerminator reports whether r ends a JS line for `.` and `$`.
func jsIsLineTerminator(r rune) bool {
	switch r {
	case '\n', '\r', '\u2028', '\u2029':
		return true
	}
	return false
}

// isASCIIWord reports membership in JS `\w` for `\b`.
func isASCIIWord(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// jsWordBoundary reports whether position i (a byte offset that sits on a rune
// boundary) is a JS `\b` boundary: exactly one side is an ASCII word char.
func jsWordBoundary(s string, i int) bool {
	before, beforeOK := lastRuneBefore(s, i)
	after, afterOK := firstRuneAt(s, i)
	beforeWord := beforeOK && isASCIIWord(before)
	afterWord := afterOK && isASCIIWord(after)
	return beforeWord != afterWord
}

func lastRuneBefore(s string, i int) (rune, bool) {
	if i <= 0 || i > len(s) {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r, true
}

func firstRuneAt(s string, i int) (rune, bool) {
	if i < 0 || i >= len(s) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r, true
}

// jsSplitWhitespace is `s.split(/\s+/)`: it splits on runs of JS whitespace and
// keeps empty fields at both ends, exactly as the JS call does.
func jsSplitWhitespace(s string) []string {
	if s == "" {
		return []string{""}
	}
	var out []string
	start := 0
	inSep := false
	for i, r := range s {
		if jsIsSpaceRune(r) {
			if !inSep {
				out = append(out, s[start:i])
				inSep = true
			}
			continue
		}
		if inSep {
			start = i
			inSep = false
		}
	}
	if inSep {
		out = append(out, s[start:])
	} else {
		out = append(out, s[start:])
	}
	return out
}

// jsSplitLines is `s.split("\n")`. Go's strings.Split agrees, so this exists
// only to keep call sites self-documenting.
func jsSplitLines(s string) []string {
	return strings.Split(s, "\n")
}

// jsSplitRegexp splits on a regexp, keeping leading, trailing and empty fields
// the way `String.prototype.split` does.
func jsSplitRegexp(re *regexp.Regexp, s string) []string {
	matches := re.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return []string{s}
	}
	out := make([]string, 0, len(matches)+1)
	last := 0
	for _, m := range matches {
		out = append(out, s[last:m[0]])
		last = m[1]
	}
	out = append(out, s[last:])
	return out
}

// jsReplaceRegexp is `s.replace(re, replacement)` for a global regexp where the
// replacement is computed from the match: it rewrites every non-overlapping
// match left to right.
func jsReplaceRegexp(s string, re *regexp.Regexp, replacement func(string, []string) string) string {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		groups := make([]string, len(m)/2)
		for g := 0; g < len(m)/2; g++ {
			if m[2*g] >= 0 {
				groups[g] = s[m[2*g]:m[2*g+1]]
			}
		}
		b.WriteString(replacement(s[m[0]:m[1]], groups))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// jsReplaceRegexpFirst is `s.replace(re, replacement)` for a non-global regexp:
// only the first match is rewritten.
func jsReplaceRegexpFirst(s string, re *regexp.Regexp, replacement func(string, []string) string) string {
	m := re.FindStringSubmatchIndex(s)
	if m == nil {
		return s
	}
	groups := make([]string, len(m)/2)
	for g := 0; g < len(m)/2; g++ {
		if m[2*g] >= 0 {
			groups[g] = s[m[2*g]:m[2*g+1]]
		}
	}
	var b strings.Builder
	b.WriteString(s[:m[0]])
	b.WriteString(replacement(s[m[0]:m[1]], groups))
	b.WriteString(s[m[1]:])
	return b.String()
}
