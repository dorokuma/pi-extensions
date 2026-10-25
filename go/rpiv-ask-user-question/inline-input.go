package rpivaskuserquestion

// Ports view/components/inline-input.ts (v2.15.0): the cursor-marked inline
// editor row shared by the single-select and multi-select views.

import (
	"strings"

	sdktui "github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// cursorMarker is pi-tui's zero-width cursor marker, used by the dialog to find
// the row carrying the cursor.
const cursorMarker = "\x1b_pi:c\x07"

// RenderInlineInputOptions is one inline-input row request.
type RenderInlineInputOptions struct {
	// Buffer is the live inline-input text.
	Buffer string
	// CursorOffset is the cursor position; nil or out of range falls back to
	// end-of-buffer.
	CursorOffset *int
	// RowPrefix prefixes the first emitted line (e.g. `❯ 4. `).
	RowPrefix string
	// ContinuationPrefix prefixes wrapped continuation lines.
	ContinuationPrefix string
	// ContentWidth is the visible columns available for the buffer.
	ContentWidth int
	// SelectedText styles each line.
	SelectedText func(string) string
}

// resolveCursorOffset mirrors resolveCursorOffset: fall back to end-of-buffer
// for nil or an out-of-range offset.
func resolveCursorOffset(buffer string, requested *int) int {
	if requested != nil && *requested >= 0 && *requested <= len(buffer) {
		return *requested
	}
	return len(buffer)
}

// buildCursorRaw mirrors buildCursorRaw: `before | CURSOR_MARKER | reverse-video
// cell | after`. The cell under the cursor is the single grapheme at the offset
// (or NBSP at end-of-buffer / on a literal space, both wrap-safe); zero
// characters shift, the column under the cursor inverts.
func buildCursorRaw(buffer string, offset int) string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(buffer) {
		offset = len(buffer)
	}
	before := buffer[:offset]
	rest := buffer[offset:]

	// One grapheme at the cursor: a rune plus any following combining marks, or
	// a whole ZWJ emoji sequence.
	at, width := firstGrapheme(rest)
	cursorAtLineEnd := at == "\n"
	cell := at
	if cell == "" || cell == " " || cursorAtLineEnd {
		cell = "\xa0"
	}
	consumed := width
	if cursorAtLineEnd {
		consumed = 0
	}
	after := rest[consumed:]
	return before + cursorMarker + "\x1b[7m" + cell + "\x1b[27m" + after
}

// firstGrapheme returns the leading grapheme cluster and its byte length. It
// treats a base rune plus following combining marks / variation selectors as
// one cluster, and a ZWJ sequence as one, which is what the TS Intl.Segmenter
// path guarantees at the SGR 7/27 boundary.
func firstGrapheme(text string) (string, int) {
	if text == "" {
		return "", 0
	}
	runes := []rune(text)
	end := 1
	for end < len(runes) && isCombining(runes[end]) {
		end++
	}
	// Extend across ZWJ-joined sequences: <base> ZWJ <base> …
	for end < len(runes) && runes[end] == '\u200d' {
		end++ // consume the ZWJ
		if end < len(runes) {
			end++ // consume the joined base
		}
		for end < len(runes) && isCombining(runes[end]) {
			end++
		}
	}
	cluster := string(runes[:end])
	return cluster, len(cluster)
}

// isCombining reports whether r is a combining mark or a variation selector.
func isCombining(r rune) bool {
	switch {
	case r >= 0x0300 && r <= 0x036f: // combining diacritical marks
		return true
	case r >= 0x1ab0 && r <= 0x1aff: // combining diacritical marks extended
		return true
	case r >= 0x1dc0 && r <= 0x1dff: // combining diacritical marks supplement
		return true
	case r >= 0x20d0 && r <= 0x20ff: // combining marks for symbols
		return true
	case r >= 0xfe00 && r <= 0xfe0f: // variation selectors
		return true
	case r >= 0xfe20 && r <= 0xfe2f: // combining half marks
		return true
	}
	return false
}

// RenderInlineInputRow mirrors renderInlineInputRow: render the buffer across
// logical and visually wrapped lines, cursor visualized by reverse video.
func RenderInlineInputRow(opts RenderInlineInputOptions) []string {
	raw := buildCursorRaw(opts.Buffer, resolveCursorOffset(opts.Buffer, opts.CursorOffset))
	style := opts.SelectedText
	if style == nil {
		style = func(text string) string { return text }
	}
	segments := wrapTextWithAnsi(raw, opts.ContentWidth)
	lines := make([]string, 0, len(segments))
	for i, segment := range segments {
		prefix := opts.ContinuationPrefix
		if i == 0 {
			prefix = opts.RowPrefix
		}
		lines = append(lines, style(prefix+segment))
	}
	return lines
}

// wrapTextWithAnsi is sdk/tui's ANSI-aware wrapper, named locally so the view
// files do not each import the tui package.
func wrapTextWithAnsi(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	return sdktui.WrapTextWithAnsi(text, width)
}

// truncateToWidth is sdk/tui's ANSI-aware truncation. `pad` mirrors the TS
// fourth argument (pad to the full width with spaces).
func truncateToWidth(text string, maxWidth int, ellipsis string, pad bool) string {
	return sdktui.TruncateToWidth(text, maxWidth, ellipsis, pad)
}

// visibleWidth is sdk/tui's ANSI-aware width.
func visibleWidth(text string) int { return sdktui.VisibleWidth(text) }

// fillWidth pads text with spaces to width (the TS `+" ".repeat(n)` idiom).
func fillWidth(text string, width int) string {
	missing := width - visibleWidth(text)
	if missing <= 0 {
		return text
	}
	return text + strings.Repeat(" ", missing)
}
