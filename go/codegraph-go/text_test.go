package codegraphgo

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestDefaults(t *testing.T) {
	for _, kind := range []string{"SEARCH", "FILES", "SYMBOL", "EXPLORE"} {
		t.Setenv("CODEGRAPH_GO_"+kind+"_MAX", "")
		t.Setenv("CODEGRAPH_GO_"+kind+"_HARD", "")
	}
	cases := []struct{ in, want map[string]any }{
		{map[string]any{"action": "search"}, map[string]any{"action": "search", "max_results": 70}},
		{map[string]any{"action": "search", "max": 1000}, map[string]any{"action": "search", "max": 120}},
		{map[string]any{"action": "search", "max_results": 0, "max": 1000}, map[string]any{"action": "search", "max_results": 70, "max": 1000}},
		{map[string]any{"action": "files", "max": "42"}, map[string]any{"action": "files", "max": 100}},
		{map[string]any{"action": "files", "max": 300}, map[string]any{"action": "files", "max": 150}},
		{map[string]any{"action": "callers"}, map[string]any{"action": "callers", "max_results": 40}},
		{map[string]any{"action": "callees", "max_results": 900}, map[string]any{"action": "callees", "max_results": 80}},
		{map[string]any{"action": "impact", "max": -1}, map[string]any{"action": "impact", "max": 40}},
		{map[string]any{"action": "explore", "max": 0}, map[string]any{"action": "explore", "skipCode": true}},
		{map[string]any{"action": "explore", "max": 999, "skipCode": false}, map[string]any{"action": "explore", "max": 60, "skipCode": false}},
		{map[string]any{"action": "explore", "max": nil, "skipCode": nil}, map[string]any{"action": "explore", "skipCode": nil}},
		{map[string]any{"action": "node", "offset": 0, "limit": 9000}, map[string]any{"action": "node", "offset": 1, "limit": 2000, "includeCode": false}},
		{map[string]any{"action": "node", "offset": 1000001, "limit": nil, "includeCode": true}, map[string]any{"action": "node", "offset": 1000000, "limit": nil, "includeCode": true}},
		{map[string]any{"action": "affected"}, map[string]any{"action": "affected", "depth": 5}},
		{map[string]any{"action": "affected", "depth": 50}, map[string]any{"action": "affected", "depth": 10}},
		{map[string]any{"action": "status", "extra": "x"}, map[string]any{"action": "status", "extra": "x"}},
		// The SDK can hand JSON numbers over as json.Number; they are numbers for
		// TS typeof/Number.isFinite, so they clamp and count as zero exactly like
		// float64 (explore max=0 still means "server decides").
		{map[string]any{"action": "files", "max": json.Number("300")}, map[string]any{"action": "files", "max": 150}},
		{map[string]any{"action": "node", "offset": json.Number("0")}, map[string]any{"action": "node", "offset": 1, "includeCode": false}},
		{map[string]any{"action": "explore", "max": json.Number("0")}, map[string]any{"action": "explore", "skipCode": true}},
	}
	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			before := map[string]any{}
			for k, v := range tc.in {
				before[k] = v
			}
			if got := withToolDefaults(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
			if !reflect.DeepEqual(tc.in, before) {
				t.Fatal("mutated input")
			}
		})
	}
	// TS typeof/Number.isFinite: strings, bools and nil are not numbers, a JSON
	// number literal is, a finite fraction floors and a below-1 or non-finite
	// value keeps def. Clamping happens in float space before the int
	// conversion, so 1e30 saturates at hard instead of wrapping negative.
	for _, tc := range []struct {
		in   any
		want int
	}{
		{math.NaN(), 7}, {math.Inf(1), 7}, {math.Inf(-1), 7}, {"5", 7}, {false, 7}, {nil, 7}, {-1, 7}, {-1e30, 7},
		{1e30, 12}, {math.MaxFloat64, 12}, {4.9, 4}, {float32(2.5), 2}, {int64(9), 9},
		{json.Number("4.9"), 4}, {json.Number("42"), 12}, {json.Number("1e30"), 12}, {json.Number("0"), 7},
		{json.Number("abc"), 7}, {json.Number(""), 7},
	} {
		if got := clampInt(tc.in, 7, 12); got != tc.want {
			t.Errorf("clampInt(%#v) = %d want %d", tc.in, got, tc.want)
		}
	}
	t.Setenv("CODEGRAPH_GO_SEARCH_MAX", "8")
	t.Setenv("CODEGRAPH_GO_SEARCH_HARD", "9")
	if withToolDefaults(map[string]any{"action": "search"})["max_results"] != 8 {
		t.Fatal("dynamic default")
	}
}
func TestKeepCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"codegraph", map[string]any{"skipCode": false}, true}, {"codegraph", map[string]any{"includeCode": true}, true}, {"codegraph", map[string]any{"action": "node"}, true}, {"codegraph", map[string]any{"action": "read"}, true}, {"READ", nil, true}, {"", map[string]any{"name": "Node"}, true}, {"codegraph", map[string]any{"name": "node"}, false}, {"codegraph", map[string]any{"skipCode": nil, "includeCode": "true"}, false}, {"", map[string]any{"name": nil}, false},
	} {
		if got := wantsKeepCode(tc.name, tc.args); got != tc.want {
			t.Errorf("%+v = %v", tc, got)
		}
	}
}
func TestCompression(t *testing.T) {
	t.Setenv("CODEGRAPH_GO_OUTPUT_CHARS", "")
	t.Setenv("CODEGRAPH_GO_OUTPUT_LINES", "")
	if compressToolText("") != "" || compressToolText("a  \r\n\n\n\n\nb\t") != "a\n\n\nb" {
		t.Fatal("normalization")
	}
	lines := []string{}
	for i := 0; i < 300; i++ {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	got := compressToolText(strings.Join(lines, "\n"))
	if !strings.Contains(got, "省略 41 行") || !strings.HasPrefix(got, "line0\n") || !strings.HasSuffix(got, "line299") {
		t.Fatal(got)
	}
	t.Setenv("CODEGRAPH_GO_OUTPUT_LINES", "0")
	got = compressToolText(strings.Join(lines, "\n"))
	if !strings.Contains(got, "省略 81 行") {
		t.Fatal(got)
	}
	t.Setenv("CODEGRAPH_GO_OUTPUT_CHARS", "5")
	got = compressToolText("abcd😀尾")
	if !strings.HasPrefix(got, "abcd\n...") || !utf8.ValidString(got) {
		t.Fatal(got)
	}
	t.Setenv("CODEGRAPH_GO_OUTPUT_CHARS", "10")
	got = compressToolText("12345678\nabcdefghij")
	if !strings.HasPrefix(got, "12345678\n...") {
		t.Fatal(got)
	}
	t.Setenv("CODEGRAPH_GO_OUTPUT_CHARS", "0")
	got = compressToolText(strings.Repeat("中", 14001))
	if !strings.Contains(got, "上限 14000 字") || len(utf16.Encode([]rune(strings.Split(got, "\n...")[0]))) != 14000 {
		t.Fatal("fallback char budget")
	}
	// Very small line caps retain the TS minimum head/tail, including overlap.
	t.Setenv("CODEGRAPH_GO_OUTPUT_LINES", "1")
	t.Setenv("CODEGRAPH_GO_OUTPUT_CHARS", "18000")
	if !strings.Contains(compressToolText("a\nb"), "省略 -53 行") {
		t.Fatal("slice overlap")
	}
}
func TestCleanText(t *testing.T) {
	text := "## Title\n**bold** *italic* `inline`\n```go\n x()\n\n y()\n```\na.go:12:match\n> quote  "
	got := formatCleanText(text, false)
	want := "Title\nbold italic inline\n[ 2 行代码已省略 ]\na.go:12\nquote"
	if got != want {
		t.Fatalf("%q", got)
	}
	keep := formatCleanText(text, true)
	if strings.Contains(keep, "代码已省略") || !strings.Contains(keep, "a.go:12:match") || !strings.Contains(keep, "x()") {
		t.Fatal(keep)
	}
	// TS still applies inline-backtick stripping when keepCode=true; this can
	// remove the original fence delimiters. Preserve that order, not idealize it.
	if formatCleanText("```\n\n```", false) != "[ 1 行代码已省略 ]" || formatCleanText("", false) != "" {
		t.Fatal("empty")
	}
	// ^\s*> consumes the blank lines before a quote after blank-line
	// compression; the leading BOM blocks the earlier heading match.
	if got := formatCleanText("\ufeff#\u00a0标题\n\n\n\n> 引用\ufeff", false); got != "#\u00a0标题\n引用" {
		t.Fatalf("JS trim/order: %q", got)
	}
	// Fenced line counting uses JS trim too: a BOM-only line is empty, while a
	// ZWSP-only line is not JS whitespace and still counts as a code line.
	if got := formatCleanText("```go\n\ufeff\n\u200b\ncode()\n```", false); got != "[ 2 行代码已省略 ]" {
		t.Fatalf("js trim in fences: %q", got)
	}
	// The final trim is the same set: BOM and NBSP are whitespace, ZWSP is not.
	if got := formatCleanText("\ufeff \u00a0text\u200b\u00a0 \ufeff", false); got != "text\u200b" {
		t.Fatalf("js trim final: %q", got)
	}
}
func TestEnvironmentNumbers(t *testing.T) {
	for _, s := range []string{"bad", "NaN", "Inf", "1e99"} {
		t.Setenv("N", s)
		if envNumber("N", 9) != 9 {
			t.Fatal(s)
		}
	}
	t.Setenv("N", "4.9")
	if envNumber("N", 9) != 4 {
		t.Fatal("floor")
	}
}
