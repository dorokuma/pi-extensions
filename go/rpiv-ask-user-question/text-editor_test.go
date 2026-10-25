package rpivaskuserquestion

import (
	"strings"
	"testing"
)

func newEditor(text string) *TextEditor {
	editor := NewTextEditor()
	editor.SetText(text)
	return editor
}

func TestTextEditorDefaults(t *testing.T) {
	editor := NewTextEditor()
	if got := editor.GetText(); got != "" {
		t.Fatalf("initial text = %q", got)
	}
	if !editor.DisableSubmit {
		t.Fatal("DisableSubmit must default to true (#156)")
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 0}) {
		t.Fatalf("initial cursor = %#v", got)
	}
}

func TestTextEditorSetTextPutsCursorAtEnd(t *testing.T) {
	editor := newEditor("line1\nline2\nabc")
	if got := editor.GetText(); got != "line1\nline2\nabc" {
		t.Fatalf("text = %q", got)
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 2, Col: 3}) {
		t.Fatalf("cursor = %#v", got)
	}
	lines := editor.GetLines()
	if len(lines) != 3 {
		t.Fatalf("lines = %#v", lines)
	}
	// GetLines returns a copy.
	lines[0] = "mutated"
	if editor.GetLines()[0] != "line1" {
		t.Fatal("GetLines leaked its backing array")
	}
}

func TestTextEditorInsertAtCursor(t *testing.T) {
	editor := newEditor("ac")
	editor.cursor = Cursor{Line: 0, Col: 1}
	editor.Insert("b")
	if got := editor.GetText(); got != "abc" {
		t.Fatalf("text = %q", got)
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("cursor = %#v", got)
	}
}

func TestTextEditorInsertMultiline(t *testing.T) {
	editor := newEditor("headTail")
	editor.cursor = Cursor{Line: 0, Col: 4}
	editor.Insert("1\n2\n3")
	if got := editor.GetText(); got != "head1\n2\n3Tail" {
		t.Fatalf("text = %q", got)
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 2, Col: 1}) {
		t.Fatalf("cursor = %#v", got)
	}
}

func TestTextEditorCursorMovement(t *testing.T) {
	editor := newEditor("ab\ncd")
	editor.cursor = Cursor{Line: 0, Col: 1}

	editor.HandleInput("\x1b[C") // Right
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("right = %#v", got)
	}
	editor.HandleInput("\x1b[C") // Right at end of line wraps
	if got := editor.GetCursor(); got != (Cursor{Line: 1, Col: 0}) {
		t.Fatalf("right wrap = %#v", got)
	}
	editor.HandleInput("\x1b[D") // Left at line start wraps back
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("left wrap = %#v", got)
	}
	editor.HandleInput("\x1b[B") // Down
	if got := editor.GetCursor(); got != (Cursor{Line: 1, Col: 2}) {
		t.Fatalf("down = %#v", got)
	}
	editor.HandleInput("\x1b[A") // Up
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("up = %#v", got)
	}
	editor.HandleInput("\x1b[H") // Home
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 0}) {
		t.Fatalf("home = %#v", got)
	}
	editor.HandleInput("\x1b[F") // End
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("end = %#v", got)
	}
	editor.HandleInput("\x01") // Ctrl+A
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 0}) {
		t.Fatalf("ctrl+a = %#v", got)
	}
	editor.HandleInput("\x05") // Ctrl+E
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("ctrl+e = %#v", got)
	}
}

// Vertical movement clamps the column to the target line's length.
func TestTextEditorCursorClampsColumn(t *testing.T) {
	editor := newEditor("abcdef\nx")
	editor.cursor = Cursor{Line: 0, Col: 6}
	editor.HandleInput("\x1b[B")
	if got := editor.GetCursor(); got != (Cursor{Line: 1, Col: 1}) {
		t.Fatalf("cursor = %#v", got)
	}
	editor.HandleInput("\x1b[A")
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 1}) {
		t.Fatalf("cursor = %#v", got)
	}
}

func TestTextEditorDeleteBackward(t *testing.T) {
	editor := newEditor("abc")
	editor.cursor = Cursor{Line: 0, Col: 3}

	editor.HandleInput("\x7f")
	if got := editor.GetText(); got != "ab" {
		t.Fatalf("text = %q", got)
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("cursor = %#v", got)
	}

	// At the line start, backspace joins with the previous line.
	editor = newEditor("ab\ncd")
	editor.cursor = Cursor{Line: 1, Col: 0}
	editor.HandleInput("\x7f")
	if got := editor.GetText(); got != "abcd" {
		t.Fatalf("joined text = %q", got)
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 2}) {
		t.Fatalf("joined cursor = %#v", got)
	}

	// At the very start nothing happens.
	editor = newEditor("ab")
	editor.cursor = Cursor{}
	editor.HandleInput("\x7f")
	if got := editor.GetText(); got != "ab" {
		t.Fatalf("text = %q", got)
	}
}

func TestTextEditorDeleteForward(t *testing.T) {
	editor := newEditor("abc")
	editor.cursor = Cursor{Line: 0, Col: 1}
	editor.HandleInput("\x1b[3~")
	if got := editor.GetText(); got != "ac" {
		t.Fatalf("text = %q", got)
	}

	// At the line end, delete joins the next line.
	editor = newEditor("ab\ncd")
	editor.cursor = Cursor{Line: 0, Col: 2}
	editor.HandleInput("\x1b[3~")
	if got := editor.GetText(); got != "abcd" {
		t.Fatalf("joined text = %q", got)
	}

	// At the very end nothing happens.
	editor = newEditor("ab")
	editor.cursor = Cursor{Line: 0, Col: 2}
	editor.HandleInput("\x1b[3~")
	if got := editor.GetText(); got != "ab" {
		t.Fatalf("text = %q", got)
	}
}

func TestTextEditorWordKillAndLineKill(t *testing.T) {
	editor := newEditor("hello world")
	editor.cursor = Cursor{Line: 0, Col: 11}
	editor.HandleInput("\x17") // Ctrl+W
	if got := editor.GetText(); got != "hello " {
		t.Fatalf("ctrl+w text = %q", got)
	}

	editor = newEditor("hello world")
	editor.cursor = Cursor{Line: 0, Col: 6}
	editor.HandleInput("\x15") // Ctrl+U kills to line start
	if got := editor.GetText(); got != "world" {
		t.Fatalf("ctrl+u text = %q", got)
	}
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 0}) {
		t.Fatalf("ctrl+u cursor = %#v", got)
	}

	editor = newEditor("hello world")
	editor.cursor = Cursor{Line: 0, Col: 5}
	editor.HandleInput("\x0b") // Ctrl+K kills to line end
	if got := editor.GetText(); got != "hello" {
		t.Fatalf("ctrl+k text = %q", got)
	}
}

func TestTextEditorWordJumps(t *testing.T) {
	editor := newEditor("hello big world")
	editor.cursor = Cursor{Line: 0, Col: 15}
	editor.HandleInput("\x1b[1;5D") // Ctrl+Left
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 10}) {
		t.Fatalf("ctrl+left = %#v", got)
	}
	editor.HandleInput("\x1b[1;5C") // Ctrl+Right
	if got := editor.GetCursor(); got != (Cursor{Line: 0, Col: 15}) {
		t.Fatalf("ctrl+right = %#v", got)
	}
}

func TestTextEditorNewlineKeys(t *testing.T) {
	for _, chunk := range []string{"\r", "\n", "\x1b\r", "\x1b[13;2u"} {
		editor := newEditor("ab")
		editor.cursor = Cursor{Line: 0, Col: 1}
		if !editor.HandleInput(chunk) {
			t.Fatalf("chunk %q not consumed", chunk)
		}
		if got := editor.GetText(); got != "a\nb" {
			t.Fatalf("chunk %q text = %q", chunk, got)
		}
		if got := editor.GetCursor(); got != (Cursor{Line: 1, Col: 0}) {
			t.Fatalf("chunk %q cursor = %#v", chunk, got)
		}
	}
}

// A bracketed paste is inserted verbatim with its markers stripped and its line
// endings normalized.
func TestTextEditorBracketedPaste(t *testing.T) {
	editor := newEditor("")
	editor.HandleInput("\x1b[200~line1\r\nline2\rline3\x1b[201~")
	if got := editor.GetText(); got != "line1\nline2\nline3" {
		t.Fatalf("text = %q", got)
	}
}

// A stray paste-end marker is not text.
func TestTextEditorStrayPasteMarkers(t *testing.T) {
	editor := newEditor("")
	editor.HandleInput("\x1b[201~tail")
	if got := editor.GetText(); got != "tail" {
		t.Fatalf("text = %q", got)
	}
}

func TestTextEditorPlainText(t *testing.T) {
	editor := newEditor("")
	if !editor.HandleInput("abc") {
		t.Fatal("plain text not consumed")
	}
	if got := editor.GetText(); got != "abc" {
		t.Fatalf("text = %q", got)
	}
	// A lone ESC is not text.
	if editor.HandleInput("\x1b") {
		t.Fatal("lone ESC consumed as text")
	}
}

// Multibyte runes are deleted as whole code points.
func TestTextEditorMultibyteDeletion(t *testing.T) {
	editor := newEditor("aé")
	editor.cursor = Cursor{Line: 0, Col: len("aé")}
	editor.HandleInput("\x7f")
	if got := editor.GetText(); got != "a" {
		t.Fatalf("text = %q", got)
	}
}

func TestTextEditorOnChangeFires(t *testing.T) {
	editor := NewTextEditor()
	count := 0
	editor.OnChange = func() { count++ }
	editor.SetText("x")
	editor.Insert("y")
	editor.HandleInput("\x1b[C")
	if count != 2 {
		t.Fatalf("OnChange fired %d times, want 2 (movement is not a change)", count)
	}
}

func TestTextEditorMaxLines(t *testing.T) {
	editor := NewTextEditor()
	editor.MaxLines = 2
	editor.SetText("a")
	editor.HandleInput("\r")
	editor.HandleInput("b")
	editor.HandleInput("\r")
	editor.HandleInput("c")
	if got := len(editor.GetLines()); got > 2 {
		t.Fatalf("lines = %d, want at most 2", got)
	}
}

func TestTextEditorFocused(t *testing.T) {
	editor := NewTextEditor()
	if editor.Focused() {
		t.Fatal("focused by default")
	}
	editor.SetFocused(true)
	if !editor.Focused() {
		t.Fatal("SetFocused(true) not reflected")
	}
	editor.Invalidate()
}

// GetExpandedText agrees with GetText (no paste-marker map to expand).
func TestTextEditorGetExpandedText(t *testing.T) {
	editor := newEditor("a\nb")
	if editor.GetExpandedText() != editor.GetText() {
		t.Fatalf("expanded = %q, text = %q", editor.GetExpandedText(), editor.GetText())
	}
}

// ── input cursor offset ───────────────────────────────────────────────────

func TestInputCursorOffset(t *testing.T) {
	editor := newEditor("ab\ncd")
	editor.cursor = Cursor{Line: 1, Col: 1}
	offset := inputCursorOffset(editor)
	if offset == nil || *offset != 4 { // "ab\n" = 3, +1
		t.Fatalf("offset = %v", offset)
	}
	if inputCursorOffset(nil) != nil {
		t.Fatal("nil editor gave an offset")
	}
}

// ── inline input row rendering ────────────────────────────────────────────

func TestRenderInlineInputRowPutsTheCursorInReverseVideo(t *testing.T) {
	offset := 1
	lines := RenderInlineInputRow(RenderInlineInputOptions{
		Buffer:             "ab",
		CursorOffset:       &offset,
		RowPrefix:          "❯ 1. ",
		ContinuationPrefix: "     ",
		ContentWidth:       20,
		SelectedText:       func(text string) string { return text },
	})
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\x1b[7m") || !strings.Contains(joined, "\x1b[27m") {
		t.Fatalf("no reverse-video cursor: %q", joined)
	}
	if !strings.Contains(joined, cursorMarker) {
		t.Fatalf("no cursor marker: %q", joined)
	}
	if !strings.HasPrefix(lines[0], "❯ 1. ") {
		t.Fatalf("first line = %q", lines[0])
	}
}

// A nil cursor offset falls back to end-of-buffer and draws an NBSP cell.
func TestRenderInlineInputRowCursorFallbacks(t *testing.T) {
	for name, offset := range map[string]*int{"nil": nil, "negative": intPtr(-1), "too large": intPtr(99)} {
		lines := RenderInlineInputRow(RenderInlineInputOptions{
			Buffer:       "ab",
			CursorOffset: offset,
			RowPrefix:    "> ",
			ContentWidth: 20,
			SelectedText: func(text string) string { return text },
		})
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "\xa0") {
			t.Fatalf("%s: no NBSP cursor cell: %q", name, joined)
		}
	}
}

// A literal space under the cursor is drawn as NBSP so it stays visible.
func TestRenderInlineInputRowSpaceCursor(t *testing.T) {
	offset := 1
	lines := RenderInlineInputRow(RenderInlineInputOptions{
		Buffer:       "a b",
		CursorOffset: &offset,
		RowPrefix:    "",
		ContentWidth: 20,
		SelectedText: func(text string) string { return text },
	})
	if !strings.Contains(strings.Join(lines, ""), "\xa0") {
		t.Fatalf("space not replaced by NBSP: %q", lines)
	}
}

// A cursor at a logical newline draws the cursor on an NBSP but keeps the
// newline, so the next logical line still renders.
func TestRenderInlineInputRowCursorAtNewline(t *testing.T) {
	offset := 2 // "ab\ncd", at the "\n"
	lines := RenderInlineInputRow(RenderInlineInputOptions{
		Buffer:       "ab\ncd",
		CursorOffset: &offset,
		RowPrefix:    "",
		ContentWidth: 20,
		SelectedText: func(text string) string { return text },
	})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\xa0") {
		t.Fatalf("no NBSP at the newline: %q", joined)
	}
	if !strings.Contains(joined, "cd") {
		t.Fatalf("text after the newline lost: %q", joined)
	}
}

// The row wraps across lines, prefixing each continuation.
func TestRenderInlineInputRowWraps(t *testing.T) {
	lines := RenderInlineInputRow(RenderInlineInputOptions{
		Buffer:             "aaaa bbbb cccc dddd",
		RowPrefix:          "> ",
		ContinuationPrefix: "  ",
		ContentWidth:       8,
		SelectedText:       func(text string) string { return text },
	})
	if len(lines) < 2 {
		t.Fatalf("expected wrapping, got %d line(s): %#v", len(lines), lines)
	}
	for i, line := range lines {
		if i > 0 && !strings.HasPrefix(line, "  ") {
			t.Fatalf("continuation line %d = %q", i, line)
		}
	}
}

func TestFirstGrapheme(t *testing.T) {
	cases := []struct {
		in    string
		at    string
		width int
	}{
		{"", "", 0},
		{"abc", "a", 1},
		{"é", "é", 2},
		// Base + combining acute accent is one cluster.
		{"e\u0301x", "e\u0301", 3},
		// ZWJ sequence: family emoji.
		{"👨\u200d👩x", "👨\u200d👩", len("👨\u200d👩")},
	}
	for _, tc := range cases {
		at, width := firstGrapheme(tc.in)
		if at != tc.at || width != tc.width {
			t.Fatalf("firstGrapheme(%q) = (%q,%d), want (%q,%d)", tc.in, at, width, tc.at, tc.width)
		}
	}
}

func TestIsCombining(t *testing.T) {
	if !isCombining(0x0301) {
		t.Fatal("combining acute not recognised")
	}
	if !isCombining(0xfe0f) {
		t.Fatal("variation selector not recognised")
	}
	if isCombining('a') {
		t.Fatal("ASCII letter treated as combining")
	}
}
