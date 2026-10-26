package pivcc

import (
	"strings"
	"unicode"
)

// Ports src/extract/user-messages.ts: what the user actually wrote, verbatim and
// clipped, for the [Early Session Requests] and [User Messages] sections.

const (
	// userMessageChars is user-messages.ts USER_MESSAGE_CHARS: each user
	// message becomes one item, clipped to this many characters.
	userMessageChars = 300
	// EarlyRequestsKept is user-messages.ts EARLY_REQUESTS_KEPT: how many of
	// the first messages [Early Session Requests] keeps.
	EarlyRequestsKept = 3
	// userMessagesKept is user-messages.ts USER_MESSAGES_KEPT: how many of the
	// newest messages [User Messages] keeps.
	userMessagesKept = 15
)

// userMessageWordCount is user-messages.ts wordCount(): the number of
// `new Intl.Segmenter(undefined, { granularity: "word" })` segments that contain
// a letter or a digit. TS leaves the locale to the runtime; on this host that
// resolves to zh-CN, so ICU breaks CJK with its Chinese dictionary.
//
// Go has no ICU and no dictionary data, so the segments are counted from the
// character classes UAX#29 would assign:
//
//   - a run of letters/digits is one word, joined across the separators that
//     join words in UAX#29 (an apostrophe between two alphanumerics, "." or ","
//     between two digits);
//   - each contiguous run of Han ideographs counts ceil(n/2) words, the average
//     of ICU's dictionary output (a two-character Chinese word is one segment,
//     and an odd character falls out as its own);
//   - Hiragana and Katakana count one word per character, which is what ICU
//     does for them once the dictionary has no entry.
//
// The measured calibration table lives in extract_test.go
// (TestUserMessageWordCount_IntlSegmenterCalibration). The count only decides
// whether a message has at least two words, so a ±1 on a long message never
// changes an outcome; the known divergences are the short Chinese replies ICU
// reads as one word and this approximation also reads as one, and the
// two-character replies ICU splits into two.
func userMessageWordCount(text string) int {
	count := 0
	runes := []rune(text)
	for i := 0; i < len(runes); {
		if !isWordLikeLetterOrNumber(runes[i]) {
			i++
			continue
		}
		if isSyllabicScript(runes[i]) {
			// Hiragana and Katakana are one word each: ICU's dictionary has no
			// entry for them, so it falls back to one segment per character.
			i++
			count++
			continue
		}
		if isIdeographicScript(runes[i]) {
			start := i
			for i < len(runes) && isWordLikeLetterOrNumber(runes[i]) && isIdeographicScript(runes[i]) {
				i++
			}
			// ceil(n/2): ICU's Chinese dictionary averages two characters per
			// word, and a lone character is a word of its own.
			count += (i - start + 1) / 2
			continue
		}
		for {
			i++
			if i >= len(runes) {
				break
			}
			if isWordLikeLetterOrNumber(runes[i]) && !isIdeographicScript(runes[i]) {
				continue
			}
			if joinsWordRunes(runes[i-1], runes[i], nextRune(runes, i)) {
				continue
			}
			break
		}
		count++
	}
	return count
}

// nextRune is the rune after i, or 0 past the end.
func nextRune(runes []rune, i int) rune {
	if i+1 >= len(runes) {
		return 0
	}
	return runes[i+1]
}

// joinsWordRunes reports whether the separator sep keeps a word going in
// UAX#29: an apostrophe (MidLetter/Single_Quote) or a "." / "," (MidNum) with the
// right neighbours.
func joinsWordRunes(before, sep, after rune) bool {
	if after == 0 {
		return false
	}
	switch sep {
	case '\'', '’':
		return isWordLikeLetterOrNumber(before) && isWordLikeLetterOrNumber(after)
	case '.', ',', '，', '．':
		return unicode.IsDigit(before) && unicode.IsDigit(after)
	}
	return false
}

// isIdeographicScript reports whether r is a Han ideograph, which ICU's Chinese
// dictionary breaks into words of about two characters.
func isIdeographicScript(r rune) bool {
	return unicode.In(r, unicode.Han)
}

// isSyllabicScript reports whether r is Hiragana or Katakana, which ICU counts
// one segment per character.
func isSyllabicScript(r rune) bool {
	return unicode.In(r, unicode.Hiragana, unicode.Katakana)
}

// userLine is user-messages.ts userLine(): the user's own words in a message, on
// one line. An expanded skill alone is not the user's words: a skill with text
// keeps the text behind a `[skill: name]` tag. Text that opens with a tag
// (`<peer_message …>`, `<local-command-stdout>`) was put there by a tool, and a
// one-word reply ("ok", "好的") says little: both are dropped.
func userLine(text string) string {
	rest, skills := replaceSkillBlocks(text, func(string) string { return " " })
	rest = collapseWhitespace(rest)
	if strings.HasPrefix(rest, "<") || userMessageWordCount(rest) < 2 {
		return ""
	}
	if len(skills) > 0 {
		rest = "[skill: " + strings.Join(skills, ", ") + "] " + rest
	}
	return Clip(rest, userMessageChars)
}

// KeepNewestUserMessages is user-messages.ts keepNewestUserMessages(): the newest
// userMessagesKept lines, oldest first. A repeated line counts as new: it moves
// to the end.
func KeepNewestUserMessages(lines []string) []string {
	seq := make([]string, 0, len(lines))
	for _, line := range lines {
		for i, existing := range seq {
			if existing == line {
				seq = append(seq[:i:i], seq[i+1:]...)
				break
			}
		}
		seq = append(seq, line)
	}
	if len(seq) > userMessagesKept {
		seq = seq[len(seq)-userMessagesKept:]
	}
	return seq
}

// ExtractUserMessages is user-messages.ts extractUserMessages(): what the user
// wrote, oldest first, verbatim and clipped. Messages the user did not write
// (`injected`) are left out. No wording is interpreted, so this works the same
// in every language.
func ExtractUserMessages(blocks []NormalizedBlock) []string {
	lines := make([]string, 0, 8)
	for _, block := range blocks {
		if block.Kind != KindUser || block.Injected {
			continue
		}
		if line := userLine(block.Text); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
