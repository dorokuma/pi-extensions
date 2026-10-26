package pivcc

import (
	"strings"
	"testing"
)

// Tests for the core text primitives ported from src/core/content.ts,
// sanitize.ts, tool-args.ts and skill-collapse.ts.

// ── Clip ────────────────────────────────────────────────────────────────────

func TestClip(t *testing.T) {
	tests := []struct {
		name string
		text string
		max  int
		want string
	}{
		{"short text is returned whole", "hello world", 200, "hello world"},
		{"exact length is returned whole", "abcde", 5, "abcde"},
		{"cuts at a word boundary past 60%", "the quick brown fox jumps", 16, "the quick brown"},
		{"no boundary past 60% cuts at max", "abcdefghijklmnop", 8, "abcdefgh"},
		{"space at the very start is not a boundary", " abcdefghijklmnop", 4, " abc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Clip(test.text, test.max); got != test.want {
				t.Fatalf("Clip(%q, %d) = %q, want %q", test.text, test.max, got, test.want)
			}
		})
	}
}

// TestClip_DoesNotSplitSurrogatePair pins content.ts's surrogate guard: a cut
// that would land between the halves of an astral character steps back one unit.
func TestClip_DoesNotSplitSurrogatePair(t *testing.T) {
	// U+1F600 is two UTF-16 units; with max=2 the cut lands after the first half.
	text := "ab\U0001F600cd"
	got := Clip(text, 3)
	// Units: a b 1F 20(lo) c d -> max 3 would split the pair, so end moves to 2.
	if got != "ab" {
		t.Fatalf("Clip(%q, 3) = %q, want %q (no split surrogate)", text, got, "ab")
	}
}

func TestClip_CountsUTF16Units(t *testing.T) {
	text := strings.Repeat("\U0001F600", 4) // 8 UTF-16 units
	if got := Clip(text, 8); got != text {
		t.Fatalf("Clip(%q, 8) should keep all 4 astral chars, got %q", text, got)
	}
	if got := Clip(text, 4); jsLen(got) != 4 {
		t.Fatalf("Clip(%q, 4) has %d units, want 4 (got %q)", text, jsLen(got), got)
	}
}

// ── ClipSentence ────────────────────────────────────────────────────────────

func TestClipSentence(t *testing.T) {
	tests := []struct {
		name string
		text string
		max  int
		want string
	}{
		{"short text whole", "Done.", 200, "Done."},
		{"cuts at the last sentence end past half", "First bit. Second bit. Third part runs long here.", 30, "First bit. Second bit."},
		{"falls back to clip with no terminator", "no punctuation at all in this long sentence right here", 20, "no punctuation at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClipSentence(test.text, test.max); got != test.want {
				t.Fatalf("ClipSentence(%q, %d) = %q, want %q", test.text, test.max, got, test.want)
			}
		})
	}
}

// ── NonEmptyLines / FirstLine / TextOf ─────────────────────────────────────

func TestNonEmptyLines(t *testing.T) {
	got := NonEmptyLines("  a  \n\n b\t\n   \nc")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("NonEmptyLines = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NonEmptyLines = %#v, want %#v", got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := FirstLine("first line\nsecond line", 200); got != "first line" {
		t.Fatalf("FirstLine = %q", got)
	}
	if got := FirstLine("abcdefghij rest\nmore", 8); got != "abcdefgh" {
		t.Fatalf("FirstLine clipped = %q", got)
	}
	if got := FirstLine("", 100); got != "" {
		t.Fatalf("FirstLine(empty) = %q", got)
	}
}

func TestTextOf(t *testing.T) {
	tests := []struct {
		name    string
		content any
		want    string
	}{
		{"string content", "hello", "hello"},
		{"text parts joined by newline", []any{
			map[string]any{"type": "text", "text": "one"},
			map[string]any{"type": "thinking", "thinking": "ignored"},
			map[string]any{"type": "text", "text": "two"},
		}, "one\ntwo"},
		{"nil content", nil, ""},
		{"non-text parts only", []any{map[string]any{"type": "image", "mimeType": "image/png"}}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := TextOf(test.content); got != test.want {
				t.Fatalf("TextOf = %q, want %q", got, test.want)
			}
		})
	}
}

// ── Sanitize ────────────────────────────────────────────────────────────────

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"CRLF becomes LF", "a\r\nb", "a\nb"},
		{"lone CR becomes LF", "a\rb", "a\nb"},
		{"ANSI colour codes are stripped", "\x1b[31mred\x1b[0m text", "red text"},
		{"ANSI with parameters is stripped", "\x1b[1;32mok\x1b[0m", "ok"},
		{"control characters are stripped", "a\x00b\x07c", "abc"},
		{"tab and newline survive", "a\tb\nc", "a\tb\nc"},
		{"a lone ESC is a control character and goes too", "a\x1bb", "ab"},
		{"the whole C0 range except tab/newline is stripped", "\x01\x02\t\n\x1f", "\t\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Sanitize(test.in); got != test.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// ── tool-args ───────────────────────────────────────────────────────────────

func TestExtractPath(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
		ok   bool
	}{
		{"path key", map[string]any{"path": "/a/b"}, "/a/b", true},
		{"file_path key", map[string]any{"file_path": "/a/b"}, "/a/b", true},
		{"filePath key", map[string]any{"filePath": "/a/b"}, "/a/b", true},
		{"file key", map[string]any{"file": "/a/b"}, "/a/b", true},
		{"key order puts path first", map[string]any{"path": "first", "file": "second"}, "first", true},
		{"non-string path ignored", map[string]any{"path": 42}, "", false},
		{"no path key", map[string]any{"command": "ls"}, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := extractPath(test.args)
			if got != test.want || ok != test.ok {
				t.Fatalf("extractPath = (%q, %v), want (%q, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestSummarizeToolArgs(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"path wins", map[string]any{"path": "/a", "command": "ls"}, "path=/a"},
		{"command next", map[string]any{"command": "ls"}, "command=ls"},
		{"query next", map[string]any{"query": "auth"}, "query=auth"},
		{"argument names last", map[string]any{"a": 1, "b": 2}, "a, b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := summarizeToolArgs(test.args); got != test.want {
				t.Fatalf("summarizeToolArgs = %q, want %q", got, test.want)
			}
		})
	}
}

// TestExtractPaths pins tool-args.ts extractPaths(): a single path arg, else the
// string entries of a `paths` array (a multi-file edit tool).
func TestExtractPaths(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"single path arg", map[string]any{"path": "/a/b"}, []string{"/a/b"}},
		{"path arg wins over paths", map[string]any{"path": "/a", "paths": []any{"/b", "/c"}}, []string{"/a"}},
		{"paths array", map[string]any{"paths": []any{"/a", "/b", "/c"}}, []string{"/a", "/b", "/c"}},
		{"non-string entries dropped", map[string]any{"paths": []any{"/a", 42, "", "/d"}}, []string{"/a", "/d"}},
		// TS's `if (one)` is truthiness: an empty path arg falls through.
		{"empty path arg falls through", map[string]any{"path": "", "paths": []any{"/a"}}, []string{"/a"}},
		{"no path at all", map[string]any{"command": "ls"}, nil},
		{"paths not an array", map[string]any{"paths": "/a"}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ExtractPaths(test.args)
			if len(got) != len(test.want) {
				t.Fatalf("ExtractPaths = %#v, want %#v", got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Fatalf("ExtractPaths = %#v, want %#v", got, test.want)
				}
			}
		})
	}
}

// TestSummarizeToolArgs_MultiPath pins the ` +N` tail a multi-path call renders.
func TestSummarizeToolArgs_MultiPath(t *testing.T) {
	if got := summarizeToolArgs(map[string]any{"paths": []any{"/a", "/b", "/c"}}); got != "path=/a +2" {
		t.Fatalf("summarizeToolArgs = %q, want \"path=/a +2\"", got)
	}
}

// TestToolOneLiner_MultiPath pins brief.ts toolOneLiner()'s ` +N` tail.
func TestToolOneLiner_MultiPath(t *testing.T) {
	if got := toolOneLiner("edit", map[string]any{"paths": []any{"/a", "/b"}}); got != `* edit "/a" +1` {
		t.Fatalf("toolOneLiner = %q, want %q", got, `* edit "/a" +1`)
	}
	if got := toolOneLiner("edit", map[string]any{"path": "/a"}); got != `* edit "/a"` {
		t.Fatalf("toolOneLiner = %q, want %q", got, `* edit "/a"`)
	}
}

// ── IsContentBearing / ExtractToolCallText ─────────────────────────────────

func TestIsContentBearing(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want bool
	}{
		{"content with a path", map[string]any{"path": "/a", "content": "text"}, true},
		{"empty content is not bearing", map[string]any{"path": "/a", "content": ""}, false},
		{"edits array of objects", map[string]any{"path": "/a", "edits": []any{map[string]any{"oldText": "a"}}}, true},
		{"edits array with a non-object", map[string]any{"path": "/a", "edits": []any{"x"}}, false},
		{"empty edits is not bearing", map[string]any{"path": "/a", "edits": []any{}}, false},
		{"oldText without edits", map[string]any{"path": "/a", "oldText": "x"}, true},
		{"newText without edits", map[string]any{"path": "/a", "newText": "x"}, true},
		{"oldText WITH edits is not bearing", map[string]any{"path": "/a", "edits": []any{}, "oldText": "x"}, false},
		{"no path", map[string]any{"content": "text"}, false},
		{"nil args", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsContentBearing(test.args); got != test.want {
				t.Fatalf("IsContentBearing(%v) = %v, want %v", test.args, got, test.want)
			}
		})
	}
}

func TestExtractToolCallText(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"content", map[string]any{"content": "body"}, "body\n"},
		{"edits old and new", map[string]any{"edits": []any{map[string]any{"oldText": "o", "newText": "n"}}}, "o\nn\n"},
		{"oldText without edits", map[string]any{"oldText": "o"}, "o\n"},
		{"newText without edits", map[string]any{"newText": "n"}, "n\n"},
		{"oldText with edits is skipped", map[string]any{"edits": []any{}, "oldText": "o"}, ""},
		{"nothing", map[string]any{"path": "/a"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ExtractToolCallText(test.args); got != test.want {
				t.Fatalf("ExtractToolCallText = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExtractToolCallArgsText(t *testing.T) {
	args := map[string]any{
		"command":  "git status",
		"path":     "/a/b",
		"edits":    []any{map[string]any{"oldText": "old", "newText": "new"}},
		"commands": []any{"one", map[string]any{"command": "two"}},
		"count":    float64(3),
	}
	got := ExtractToolCallArgsText(args)
	for _, want := range []string{"git status", "/a/b", "old", "new", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ExtractToolCallArgsText is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "3") {
		t.Fatalf("a non-string scalar leaked in:\n%s", got)
	}
	if got := ExtractToolCallArgsText(nil); got != "" {
		t.Fatalf("ExtractToolCallArgsText(nil) = %q", got)
	}
}

// ── Snippet ─────────────────────────────────────────────────────────────────

func TestSnippet(t *testing.T) {
	text := strings.Repeat("x", 100) + "Needle" + strings.Repeat("y", 100)
	snippet, ok := Snippet(text, "needle", 10)
	if !ok {
		t.Fatal("Snippet did not find a case-insensitive match")
	}
	if !strings.Contains(snippet, "Needle") {
		t.Fatalf("Snippet lost the match: %q", snippet)
	}
	if !strings.HasPrefix(snippet, "...") || !strings.HasSuffix(snippet, "...") {
		t.Fatalf("Snippet should be elided on both sides: %q", snippet)
	}
	if _, ok := Snippet(text, "absent", 10); ok {
		t.Fatal("Snippet found an absent term")
	}
	// No elision at the very start/end.
	snippet, ok = Snippet("abc", "abc", 10)
	if !ok || snippet != "abc" {
		t.Fatalf("Snippet = (%q, %v), want (\"abc\", true)", snippet, ok)
	}
}

// ── skill collapse ──────────────────────────────────────────────────────────

func TestCollapseSkillText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a closed block becomes its name",
			in:   "before <skill name=\"pdf\" other=\"x\">\nbody\n</skill> after",
			want: "before [skill: pdf] after",
		},
		{
			name: "an unterminated block runs to the end",
			in:   "keep <skill name=\"pdf\">body that never closes",
			want: "keep [skill: pdf]",
		},
		{
			name: "two blocks",
			in:   "<skill name=\"a\">1</skill> and <skill name=\"b\">2</skill>",
			want: "[skill: a] and [skill: b]",
		},
		{
			name: "no block is unchanged",
			in:   "plain text",
			want: "plain text",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CollapseSkillText(test.in); got != test.want {
				t.Fatalf("CollapseSkillText(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}
