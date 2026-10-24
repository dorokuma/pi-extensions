package ctxorchestrate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Tests for the JavaScript-semantics helpers the rest of the package relies on.

func TestUTF16Len(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"汉字", 2},
		{"😀", 2}, // astral rune: two UTF-16 code units, four UTF-8 bytes
		{"😀a", 3},
		{"café", 4},
	} {
		if got := utf16Len(tc.in); got != tc.want {
			t.Fatalf("utf16Len(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTruncateUTF16(t *testing.T) {
	for _, tc := range []struct {
		in    string
		units int
		want  string
	}{
		{"abcdef", 3, "abc"},
		{"abcdef", 99, "abcdef"},
		{"abcdef", 0, ""},
		{"abcdef", -1, ""},
		{"😀😀", 3, "😀"}, // a 3-unit budget cannot hold two astral runes
		{"😀😀", 2, "😀"},
		{"😀😀", 1, ""},   // never half a surrogate pair
		{"a😀b", 2, "a"}, // one unit left, the astral rune does not fit
	} {
		got := truncateUTF16(tc.in, tc.units)
		if got != tc.want {
			t.Fatalf("truncateUTF16(%q, %d) = %q, want %q", tc.in, tc.units, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncateUTF16(%q, %d) split a rune: %q", tc.in, tc.units, got)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("  a\t\tb \n c  ", ExcerptCap); got != "a b c" {
		t.Fatalf("whitespace collapse = %q", got)
	}
	if got := oneLine("short", ExcerptCap); got != "short" {
		t.Fatalf("short text = %q", got)
	}
	long := strings.Repeat("字", ExcerptCap+10)
	got := oneLine(long, ExcerptCap)
	if utf16Len(got) != ExcerptCap {
		t.Fatalf("capped length = %d, want %d", utf16Len(got), ExcerptCap)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("capped text must end with the ellipsis: %q", got)
	}
	// Astral runes count twice, and the cut never splits one: a 200-unit cap gives
	// 99 emoji (198 units) plus the ellipsis, i.e. 199 units — the same cut the TS
	// clampUtf16Index(flat, cap-1, towardStart=true) takes.
	emoji := oneLine(strings.Repeat("😀", 200), ExcerptCap)
	if utf16Len(emoji) != ExcerptCap-1 {
		t.Fatalf("emoji capped length = %d, want %d", utf16Len(emoji), ExcerptCap-1)
	}
	if !utf8.ValidString(emoji) {
		t.Fatal("oneLine split a rune")
	}
}

func TestClampInt(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int
	}{
		{float64(7.9), 7},
		{float64(0), hitsDefault},
		{float64(-3), hitsDefault},
		{float64(9999), hitsHard},
		{9, 9},
		{int64(12), 12},
		{"42", hitsDefault}, // a string is not a number
		{nil, hitsDefault},
	} {
		if got := clampInt(tc.value, hitsDefault, hitsHard); got != tc.want {
			t.Fatalf("clampInt(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestOptStr(t *testing.T) {
	if got := optStr("  src/  "); got != "src/" {
		t.Fatalf("optStr = %q", got)
	}
	for _, value := range []any{"", "   ", nil, float64(3)} {
		if got := optStr(value); got != "" {
			t.Fatalf("optStr(%v) = %q, want empty", value, got)
		}
	}
}

func TestPositiveInt(t *testing.T) {
	if got, ok := positiveInt(float64(3.9)); !ok || got != 3 {
		t.Fatalf("positiveInt(3.9) = %d, %t", got, ok)
	}
	for _, value := range []any{float64(0), float64(-1), nil, "3"} {
		if _, ok := positiveInt(value); ok {
			t.Fatalf("positiveInt(%v) must not be ok", value)
		}
	}
}

func TestToJSString(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{"abc", "abc"},
		{float64(12), "12"},
		{float64(1.5), "1.5"},
		{true, "true"},
		{nil, "null"},
	} {
		if got := toJSString(tc.value); got != tc.want {
			t.Fatalf("toJSString(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestEnvFlag(t *testing.T) {
	t.Setenv("CTX_ORCH_TEST_FLAG", "")
	if !envFlag("CTX_ORCH_TEST_FLAG", true) {
		t.Fatal("unset must yield the fallback")
	}
	if envFlag("CTX_ORCH_TEST_FLAG", false) {
		t.Fatal("unset must yield the fallback")
	}
	t.Setenv("CTX_ORCH_TEST_FLAG", " YES ")
	if !envFlag("CTX_ORCH_TEST_FLAG", false) {
		t.Fatal("YES with spaces must be true")
	}
	t.Setenv("CTX_ORCH_TEST_FLAG", "off")
	if envFlag("CTX_ORCH_TEST_FLAG", true) {
		t.Fatal("off must be false")
	}
}
