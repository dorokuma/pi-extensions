package rpivaskuserquestion

// Headless multiline text editor. The npm original uses pi-tui's `Editor`
// component directly (a live TUI object); PiG has no in-process pi-tui editor,
// so the port carries an equivalent text buffer with the editing semantics the
// questionnaire relies on:
//
//	Left/Right/Up/Down          cursor movement (across logical lines)
//	Home/End, Ctrl+A/Ctrl+E     line start / line end
//	Ctrl+Left/Right             word jumps
//	Backspace/Delete            character deletion, joining lines at edges
//	Ctrl+W / Ctrl+U / Ctrl+K    word kill / line-start kill / line-end kill
//	Enter                       newline (when submit is disabled)
//	Shift+Enter / Alt+Enter     newline
//	paste (bracketed)           inserted verbatim, paste markers stripped
//
// The questionnaire's own key router owns confirm/submit semantics, so the
// editors it mounts run with `DisableSubmit = true`: a submit binding reaching
// the editor must not run submitValue() and silently destroy the draft (#156).

import (
	"strings"
	"unicode"
)

// Cursor is a line/column position (UTF-16 offsets in the TS original; byte
// offsets here are only used as an index into the line's string).
type Cursor struct {
	Line int
	Col  int
}

// TextEditor is a headless multiline editor buffer.
type TextEditor struct {
	lines   []string
	cursor  Cursor
	focused bool
	// DisableSubmit mirrors pi-tui's `disableSubmit`: when false, an Enter key
	// would "submit" (clear the buffer). The questionnaire never wants that.
	DisableSubmit bool
	// MaxLines, when > 0, caps the buffer height (0 = unlimited).
	MaxLines int
	// OnChange is notified after any buffer mutation.
	OnChange func()
}

// NewTextEditor constructs an empty editor.
func NewTextEditor() *TextEditor {
	return &TextEditor{lines: []string{""}, DisableSubmit: true}
}

// Invalidate is a no-op: the buffer holds no derived cache. It exists so the
// notes editor cell satisfies the adapter's Invalidatable contract.
func (e *TextEditor) Invalidate() {}

// SetFocused records whether the editor owns keyboard focus.
func (e *TextEditor) SetFocused(focused bool) { e.focused = focused }

// Focused reports the focus state.
func (e *TextEditor) Focused() bool { return e.focused }

// GetText returns the buffer's text: logical lines joined with "\n". Bracketed
// paste markers are never stored, so `GetExpandedText` and `GetText` agree.
func (e *TextEditor) GetText() string { return strings.Join(e.lines, "\n") }

// GetExpandedText mirrors the TS `getExpandedText?.() ?? getText()` idiom. This
// buffer stores expanded text, so it is the same value.
func (e *TextEditor) GetExpandedText() string { return e.GetText() }

// GetLines returns a copy of the buffer's logical lines.
func (e *TextEditor) GetLines() []string {
	out := make([]string, len(e.lines))
	copy(out, e.lines)
	return out
}

// GetCursor returns the current cursor position.
func (e *TextEditor) GetCursor() Cursor { return e.cursor }

// SetText replaces the buffer and puts the cursor at the end. Used to restore
// drafts via the `set_input_buffer` / `set_notes_value` effects.
func (e *TextEditor) SetText(text string) {
	e.lines = splitLines(text)
	if len(e.lines) == 0 {
		e.lines = []string{""}
	}
	last := len(e.lines) - 1
	e.cursor = Cursor{Line: last, Col: len(e.lines[last])}
	e.changed()
}

// Insert inserts text at the cursor, honouring embedded newlines (paste).
func (e *TextEditor) Insert(text string) {
	if text == "" {
		return
	}
	line := e.lines[e.cursor.Line]
	head, tail := line[:e.cursor.Col], line[e.cursor.Col:]
	parts := splitLines(text)
	if len(parts) == 1 {
		e.lines[e.cursor.Line] = head + parts[0] + tail
		e.cursor.Col += len(parts[0])
		e.changed()
		return
	}
	merged := make([]string, 0, len(e.lines)+len(parts)-1)
	merged = append(merged, e.lines[:e.cursor.Line]...)
	merged = append(merged, head+parts[0])
	merged = append(merged, parts[1:len(parts)-1]...)
	lastPart := parts[len(parts)-1]
	merged = append(merged, lastPart+tail)
	merged = append(merged, e.lines[e.cursor.Line+1:]...)
	e.lines = merged
	e.cursor = Cursor{Line: e.cursor.Line + len(parts) - 1, Col: len(lastPart)}
	e.changed()
}

// HandleInput applies one raw terminal chunk to the buffer and reports whether
// it consumed the chunk. Text-editing keys only; the router owns semantics.
func (e *TextEditor) HandleInput(data string) bool {
	if data == "" {
		return false
	}
	// Bracketed paste: unwrap and insert verbatim.
	if strings.HasPrefix(data, "\x1b[200~") {
		inner := strings.TrimSuffix(strings.TrimPrefix(data, "\x1b[200~"), "\x1b[201~")
		inner = strings.ReplaceAll(inner, "\x1b[200~", "")
		inner = strings.ReplaceAll(inner, "\x1b[201~", "")
		inner = strings.ReplaceAll(inner, "\r\n", "\n")
		inner = strings.ReplaceAll(inner, "\r", "\n")
		e.Insert(inner)
		return true
	}
	// A stray paste-end marker carries no text of its own: strip it and keep any
	// text around it.
	if strings.Contains(data, "\x1b[201~") {
		data = strings.ReplaceAll(data, "\x1b[201~", "")
		data = strings.ReplaceAll(data, "\x1b[200~", "")
		if data == "" {
			return true
		}
	}

	switch data {
	case "\x1b[D", "\x1bOD":
		e.moveLeft()
		return true
	case "\x1b[C", "\x1bOC":
		e.moveRight()
		return true
	case "\x1b[A", "\x1bOA":
		e.moveUp()
		return true
	case "\x1b[B", "\x1bOB":
		e.moveDown()
		return true
	case "\x1b[H", "\x1b[1~", "\x1bOH", "\x01": // Home, Ctrl+A
		e.cursor.Col = 0
		return true
	case "\x1b[F", "\x1b[4~", "\x1bOF", "\x05": // End, Ctrl+E
		e.cursor.Col = len(e.lines[e.cursor.Line])
		return true
	case "\x1b[3~": // Delete
		e.deleteForward()
		return true
	case "\x7f", "\b": // Backspace
		e.deleteBackward()
		return true
	case "\x17": // Ctrl+W — kill the word before the cursor
		e.killWordBefore()
		return true
	case "\x15": // Ctrl+U — kill to line start
		e.lines[e.cursor.Line] = e.lines[e.cursor.Line][e.cursor.Col:]
		e.cursor.Col = 0
		e.changed()
		return true
	case "\x0b": // Ctrl+K — kill to line end
		e.lines[e.cursor.Line] = e.lines[e.cursor.Line][:e.cursor.Col]
		e.changed()
		return true
	case "\x1b[1;5D", "\x1bb": // Ctrl+Left, Alt+b — word left
		e.cursor.Col = wordBoundaryBefore(e.lines[e.cursor.Line], e.cursor.Col)
		return true
	case "\x1b[1;5C", "\x1bf": // Ctrl+Right, Alt+f — word right
		e.cursor.Col = wordBoundaryAfter(e.lines[e.cursor.Line], e.cursor.Col)
		return true
	case "\x1b\r", "\x1b\n", "\x1b[13;2u": // Alt+Enter, Shift+Enter
		e.insertNewline()
		return true
	case "\r", "\n":
		// With submit disabled the editor treats Enter as a newline; otherwise the
		// questionnaire's router would have consumed it first.
		e.insertNewline()
		return true
	}

	// A lone ESC (not part of a sequence) is not text.
	if data == "\x1b" {
		return false
	}
	// Printable run (may be several runes for a fast paste without brackets).
	if isPrintable(data) {
		e.Insert(data)
		return true
	}
	return false
}

// insertNewline splits the current line at the cursor.
func (e *TextEditor) insertNewline() {
	line := e.lines[e.cursor.Line]
	head, tail := line[:e.cursor.Col], line[e.cursor.Col:]
	e.lines[e.cursor.Line] = head
	merged := make([]string, 0, len(e.lines)+1)
	merged = append(merged, e.lines[:e.cursor.Line+1]...)
	merged = append(merged, tail)
	merged = append(merged, e.lines[e.cursor.Line+1:]...)
	e.lines = merged
	e.cursor = Cursor{Line: e.cursor.Line + 1, Col: 0}
	e.changed()
}

// moveLeft moves the cursor one column left, wrapping to the previous line end.
func (e *TextEditor) moveLeft() {
	if e.cursor.Col > 0 {
		e.cursor.Col--
		return
	}
	if e.cursor.Line > 0 {
		e.cursor.Line--
		e.cursor.Col = len(e.lines[e.cursor.Line])
	}
}

// moveRight moves the cursor one column right, wrapping to the next line start.
func (e *TextEditor) moveRight() {
	if e.cursor.Col < len(e.lines[e.cursor.Line]) {
		e.cursor.Col++
		return
	}
	if e.cursor.Line < len(e.lines)-1 {
		e.cursor.Line++
		e.cursor.Col = 0
	}
}

// moveUp moves the cursor up a line, clamping the column.
func (e *TextEditor) moveUp() {
	if e.cursor.Line == 0 {
		return
	}
	e.cursor.Line--
	e.cursor.Col = minInt(e.cursor.Col, len(e.lines[e.cursor.Line]))
}

// moveDown moves the cursor down a line, clamping the column.
func (e *TextEditor) moveDown() {
	if e.cursor.Line >= len(e.lines)-1 {
		return
	}
	e.cursor.Line++
	e.cursor.Col = minInt(e.cursor.Col, len(e.lines[e.cursor.Line]))
}

// deleteBackward removes the character before the cursor (joining lines at the
// line start).
func (e *TextEditor) deleteBackward() {
	if e.cursor.Col > 0 {
		line := e.lines[e.cursor.Line]
		start := prevRuneStart(line, e.cursor.Col)
		e.lines[e.cursor.Line] = line[:start] + line[e.cursor.Col:]
		e.cursor.Col = start
		e.changed()
		return
	}
	if e.cursor.Line == 0 {
		return
	}
	prev := e.lines[e.cursor.Line-1]
	cur := e.lines[e.cursor.Line]
	merged := make([]string, 0, len(e.lines)-1)
	merged = append(merged, e.lines[:e.cursor.Line-1]...)
	merged = append(merged, prev+cur)
	merged = append(merged, e.lines[e.cursor.Line+1:]...)
	e.lines = merged
	e.cursor = Cursor{Line: e.cursor.Line - 1, Col: len(prev)}
	e.changed()
}

// deleteForward removes the character after the cursor (joining lines at the
// line end).
func (e *TextEditor) deleteForward() {
	line := e.lines[e.cursor.Line]
	if e.cursor.Col < len(line) {
		end := nextRuneEnd(line, e.cursor.Col)
		e.lines[e.cursor.Line] = line[:e.cursor.Col] + line[end:]
		e.changed()
		return
	}
	if e.cursor.Line >= len(e.lines)-1 {
		return
	}
	cur := e.lines[e.cursor.Line]
	next := e.lines[e.cursor.Line+1]
	merged := make([]string, 0, len(e.lines)-1)
	merged = append(merged, e.lines[:e.cursor.Line]...)
	merged = append(merged, cur+next)
	merged = append(merged, e.lines[e.cursor.Line+2:]...)
	e.lines = merged
	e.changed()
}

// killWordBefore removes the word before the cursor.
func (e *TextEditor) killWordBefore() {
	line := e.lines[e.cursor.Line]
	start := wordBoundaryBefore(line, e.cursor.Col)
	e.lines[e.cursor.Line] = line[:start] + line[e.cursor.Col:]
	e.cursor.Col = start
	e.changed()
}

// changed notifies the owner.
func (e *TextEditor) changed() {
	if e.MaxLines > 0 && len(e.lines) > e.MaxLines {
		e.lines = e.lines[:e.MaxLines]
		if e.cursor.Line >= len(e.lines) {
			e.cursor.Line = len(e.lines) - 1
			e.cursor.Col = len(e.lines[e.cursor.Line])
		}
	}
	if e.OnChange != nil {
		e.OnChange()
	}
}

// prevRuneStart returns the byte index of the rune before col.
func prevRuneStart(line string, col int) int {
	if col <= 0 {
		return 0
	}
	start := col - 1
	for start > 0 && line[start]&0xC0 == 0x80 { // continuation byte
		start--
	}
	return start
}

// nextRuneEnd returns the byte index just past the rune at col.
func nextRuneEnd(line string, col int) int {
	if col >= len(line) {
		return len(line)
	}
	end := col + 1
	for end < len(line) && line[end]&0xC0 == 0x80 {
		end++
	}
	return end
}

// wordBoundaryBefore returns the index where the word before col starts.
func wordBoundaryBefore(line string, col int) int {
	index := col
	for index > 0 && isSpaceByte(line[index-1]) {
		index--
	}
	for index > 0 && !isSpaceByte(line[index-1]) {
		index--
	}
	return index
}

// wordBoundaryAfter returns the index just past the word at col.
func wordBoundaryAfter(line string, col int) int {
	index := col
	for index < len(line) && isSpaceByte(line[index]) {
		index++
	}
	for index < len(line) && !isSpaceByte(line[index]) {
		index++
	}
	return index
}

// isSpaceByte reports whether b is ASCII whitespace.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// isPrintable reports whether the chunk is printable text (no control bytes).
func isPrintable(text string) bool {
	for _, r := range text {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
