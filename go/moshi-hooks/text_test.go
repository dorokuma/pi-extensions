package moshihooks

import (
	"regexp"
	"strings"
	"testing"
)

func TestStripMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"code block dropped", "before\n```go\nfmt.Println(1)\n```\nafter", "before after"},
		{"inline code keeps text", "run `go test` now", "run go test now"},
		{"link keeps its text", "see [the docs](https://example.com/x)", "see the docs"},
		{"bold and italic", "**bold** and *italic* and __under__", "bold and italic and under"},
		{"headings and bullets", "## Title\n- one\n- two\n1. three", "Title one two three"},
		{"blockquote", "> quoted", "quoted"},
		{"whitespace collapses", "a  \n\t b", "a b"},
		{"empty", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stripMarkdown(test.in); got != test.want {
				t.Fatalf("stripMarkdown(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short enough", "abc", 3, "abc"},
		{"clipped", "abcd", 3, "ab…"},
		{"zero", "abc", 0, "…"},
		{"runes not bytes", "中文测试", 3, "中文…"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := truncateText(test.in, test.n); got != test.want {
				t.Fatalf("truncateText(%q, %d) = %q, want %q", test.in, test.n, got, test.want)
			}
		})
	}
}

func TestPlainTextClip(t *testing.T) {
	if got, want := plainTextClip("**hello** world", 200), "hello world"; got != want {
		t.Fatalf("plainTextClip = %q, want %q", got, want)
	}
	if got, want := plainTextClip("abcdef", 3), "ab…"; got != want {
		t.Fatalf("plainTextClip clipped = %q, want %q", got, want)
	}
}

func TestDisplayUserPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "  fix the bug  ", "fix the bug"},
		{"empty", "   ", ""},
		{"user query wrapper", "<user_query>\n 只回 OK \n</user_query>", "只回 OK"},
		{"user query is case insensitive", "<USER_QUERY>ok</USER_QUERY>", "ok"},
		{"skill block removed", "ask\n<skill_information>x</skill_information>", "ask"},
		{"image files removed", "look\n<image_files>a.png</image_files>", "look"},
		{"system reminder removed", "go\n<system-reminder>noise</system-reminder>", "go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := displayUserPrompt(test.in); got != test.want {
				t.Fatalf("displayUserPrompt(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestFormatUserPromptDetail(t *testing.T) {
	if got, want := formatUserPromptDetail("**hi**"), "You: hi"; got != want {
		t.Fatalf("formatUserPromptDetail = %q, want %q", got, want)
	}
	if got := formatUserPromptDetail("   "); got != "" {
		t.Fatalf("formatUserPromptDetail(blank) = %q, want empty", got)
	}
	long := strings.Repeat("a", 300)
	got := formatUserPromptDetail(long)
	if want := "You: " + strings.Repeat("a", 199) + "…"; got != want {
		t.Fatalf("formatUserPromptDetail(long) = %q, want %q", got, want)
	}
}

func TestPlanSummary(t *testing.T) {
	plan := "# Plan\n\n> **Ship it** now\nsecond line"
	if got, want := planSummary(plan), "Plan"; got != want {
		t.Fatalf("planSummary = %q, want %q", got, want)
	}
	if got := planSummary("###\n\n- \n"); got != "" {
		t.Fatalf("planSummary(noise only) = %q, want empty", got)
	}
}

func TestFirstString(t *testing.T) {
	if got, want := firstString("", 0, "second", "third"), "second"; got != want {
		t.Fatalf("firstString = %q, want %q", got, want)
	}
	if got := firstString(nil, 42, ""); got != "" {
		t.Fatalf("firstString(no strings) = %q, want empty", got)
	}
}

func TestTextFromMessage(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"string content", map[string]any{"content": "plain"}, "plain"},
		{"chunks", map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "one"},
			"two",
			map[string]any{"type": "image"},
		}}, "one two"},
		{"nested message", map[string]any{"message": map[string]any{"content": "inner"}}, "inner"},
		{"nested entry", map[string]any{"entry": map[string]any{"content": "entry"}}, "entry"},
		{"not an object", "text", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := textFromMessage(test.value); got != test.want {
				t.Fatalf("textFromMessage = %q, want %q", got, test.want)
			}
		})
	}
}

func TestModelLabel(t *testing.T) {
	if got, want := modelLabel(map[string]any{"displayName": "Smart Ass", "name": "n", "id": "i"}), "Smart Ass"; got != want {
		t.Fatalf("modelLabel(displayName) = %q, want %q", got, want)
	}
	if got, want := modelLabel(map[string]any{"display_name": "Snake", "id": "i"}), "Snake"; got != want {
		t.Fatalf("modelLabel(display_name) = %q, want %q", got, want)
	}
	if got, want := modelLabel(map[string]any{"modelId": "only-id"}), "only-id"; got != want {
		t.Fatalf("modelLabel(modelId) = %q, want %q", got, want)
	}
	if got := modelLabel(nil); got != "" {
		t.Fatalf("modelLabel(nil) = %q, want empty", got)
	}
}

func TestNewSessionID(t *testing.T) {
	lowercaseHex := regexp.MustCompile(`^[0-9a-f]{32}$`)
	first := newSessionID()
	if !lowercaseHex.MatchString(first) {
		t.Fatalf("newSessionID = %q, want 32 lowercase hex characters", first)
	}
	if second := newSessionID(); second == first {
		t.Fatalf("newSessionID repeated %q", first)
	}
}

func TestJSTrimMatchesJavaScriptSpace(t *testing.T) {
	if got, want := jsTrim("\u00a0 hi \ufeff"), "hi"; got != want {
		t.Fatalf("jsTrim = %q, want %q", got, want)
	}
}
