package pivcc

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Tests for src/core/recall-request.ts and src/core/drill-down.ts: argument
// repair, action precedence and `#N:path` parsing.

// ── drill-down ──────────────────────────────────────────────────────────────

func TestParseDrillDown(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantNil  bool
		wantIdx  int
		wantPath string
		wantFull bool
		offset   int
		hasOff   bool
		limit    int
		hasLimit bool
	}{
		{name: "plain path", query: "#12:src/core/summarize.ts", wantIdx: 12, wantPath: "src/core/summarize.ts"},
		{name: "full suffix", query: "#3:a.go:full", wantIdx: 3, wantPath: "a.go", wantFull: true},
		{name: "offset", query: "#7:a.go:40", wantIdx: 7, wantPath: "a.go", offset: 40, hasOff: true},
		{name: "offset and limit", query: "#7:a.go:40:20", wantIdx: 7, wantPath: "a.go",
			offset: 40, hasOff: true, limit: 20, hasLimit: true},
		{name: "not a drill-down", query: "auth bug", wantNil: true},
		{name: "missing index", query: "a.go", wantNil: true},
		{name: "empty path", query: "#1:", wantNil: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseDrillDown(test.query)
			if test.wantNil {
				if got != nil {
					t.Fatalf("ParseDrillDown(%q) = %#v, want nil", test.query, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseDrillDown(%q) = nil", test.query)
			}
			if got.Index != test.wantIdx || got.PathPattern != test.wantPath || got.Full != test.wantFull {
				t.Fatalf("ParseDrillDown(%q) = %#v", test.query, got)
			}
			if got.Offset != test.offset || got.HasOffset != test.hasOff ||
				got.Limit != test.limit || got.HasLimit != test.hasLimit {
				t.Fatalf("ParseDrillDown(%q) offset/limit = %#v", test.query, got)
			}
		})
	}
}

// TestParseDrillDown_NumericPathSegmentStaysInPath pins the lazy-group recovery:
// `#1:dir/2` is a path, not an offset of `dir/`.
func TestParseDrillDown_NumericPathSegmentStaysInPath(t *testing.T) {
	got := ParseDrillDown("#1:docs/v1/2")
	if got == nil {
		t.Fatal("ParseDrillDown returned nil")
	}
	if got.PathPattern != "docs/v1/2" {
		t.Fatalf("PathPattern = %q, want the whole path", got.PathPattern)
	}
	if got.HasOffset {
		t.Fatalf("a path segment was read as an offset: %#v", got)
	}
}

// ── formatToolCallContent: window bounds and the :full cap ──────────────────

// drillCall builds a content-bearing write call for the drill-down formatter.
func drillCall(body string) contentBearingCall {
	return contentBearingCall{Name: "write", Path: "notes.md", Content: body, HasContent: true}
}

// TestFormatToolCallContent_OffsetBeyondFileLength pins the diagnostic for an
// offset past the end of the file. drill-down.ts floors `startLine =
// Math.max(0, offset)` at 0 but never clamps it up to totalLines, so the message
// must echo the caller's offset (99) rather than the file length (5).
func TestFormatToolCallContent_OffsetBeyondFileLength(t *testing.T) {
	got := formatToolCallContent(drillCall("1\n2\n3\n4\n5"), 7, false, 99, true, 0, false)
	want := "Offset 99 is beyond file length 5. Use #7:notes.md for the first 30 lines."
	if got != want {
		t.Fatalf("beyond-end window = %q, want %q", got, want)
	}
}

// TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow pins that a negative
// limit cannot panic: `endLine = startLine + limit` lands before startLine, so
// the window stays empty and the caller gets the beyond-end diagnostic instead
// of a slice out of range.
//
// The parsed drill-down cannot produce a negative limit at all — the `:N[:M]`
// suffix only accepts \d+ — so this guards direct callers of the exported
// entry-file expansion. (TS gets the same no-panic outcome from Array.slice
// tolerating the reversed bounds.)
func TestFormatToolCallContent_NegativeLimitYieldsEmptyWindow(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		totalLines int
	}{
		{"short file", "1\n2\n3\n4\n5", 5},
		{"long file", strings.Repeat("x\n", 99) + "x", 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatToolCallContent(drillCall(test.body), 7, false, 0, true, -5, true)
			want := "Offset 0 is beyond file length " + strconv.Itoa(test.totalLines) +
				". Use #7:notes.md for the first 30 lines."
			if got != want {
				t.Fatalf("negative-limit window = %q, want %q", got, want)
			}
		})
	}
}

// TestFormatToolCallContent_FullCapSlicesCodeUnits pins the :full cap: the 50KB
// gate is a UTF-8 byte comparison (`Buffer.byteLength(body, "utf8") >
// MAX_FULL_BYTES`), the cut is `body.slice(0, MAX_FULL_BYTES)` in UTF-16 code
// units, and the reported shortfall stays a byte difference. Cutting Go bytes
// instead would leave half of a multi-byte character at the cut — invalid UTF-8
// — and split a surrogate pair.
func TestFormatToolCallContent_FullCapSlicesCodeUnits(t *testing.T) {
	head := strings.Repeat("a", drillMaxFullBytes-1)
	tests := []struct {
		name        string
		body        string
		wantVisible string
	}{
		{
			name:        "three byte rune at the boundary",
			body:        head + "语" + "tail",
			wantVisible: head + "语",
		},
		{
			name: "surrogate pair at the boundary",
			body: head + "🙂" + "tail",
			// The cut falls between the pair's two code units. A lone surrogate
			// cannot live in a Go string, so jsFromUnits emits U+FFFD — the very
			// bytes Node writes when it encodes that lone surrogate.
			wantVisible: head + "\uFFFD",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatToolCallContent(drillCall(test.body), 7, true, 0, false, 0, false)
			want := "File: notes.md\nTool: write\n\n" + test.wantVisible + "\n\n... (" +
				strconv.Itoa(len(test.body)-drillMaxFullBytes) +
				" more bytes — file exceeds 50KB display limit. Use #7:notes.md:30 for next page.)"
			if got != want {
				t.Fatalf(":full output mismatch\n got: %q\nwant: %q", got, want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf(":full cut produced invalid UTF-8: %x", got[len(got)-40:])
			}
		})
	}
}

// ── toIndex / indexList ─────────────────────────────────────────────────────

func TestToIndex(t *testing.T) {
	tests := []struct {
		in   any
		want int
		ok   bool
	}{
		{float64(12), 12, true},
		{12, 12, true},
		{"12", 12, true},
		{"#12", 12, true},
		{" 12 ", 12, true},
		{float64(12.5), 0, false},
		{"twelve", 0, false},
		{nil, 0, false},
	}
	for _, test := range tests {
		got, ok := toIndex(test.in)
		if got != test.want || ok != test.ok {
			t.Fatalf("toIndex(%v) = (%d, %v), want (%d, %v)", test.in, got, ok, test.want, test.ok)
		}
	}
}

func TestIndexList(t *testing.T) {
	tests := []struct {
		in   string
		want []int
		ok   bool
	}{
		{"12", []int{12}, true},
		{"#12, #15", []int{12, 15}, true},
		{"12 15", []int{12, 15}, true},
		{"[12, 15]", []int{12, 15}, true},
		{"12, fifteen", nil, false},
		{"", nil, false},
	}
	for _, test := range tests {
		got, ok := indexList(test.in)
		if ok != test.ok {
			t.Fatalf("indexList(%q) ok = %v, want %v", test.in, ok, test.ok)
		}
		if !ok {
			continue
		}
		if len(got) != len(test.want) {
			t.Fatalf("indexList(%q) = %v, want %v", test.in, got, test.want)
		}
		for i := range test.want {
			if got[i] != test.want[i] {
				t.Fatalf("indexList(%q) = %v, want %v", test.in, got, test.want)
			}
		}
	}
}

// ── normalizeToolParams ─────────────────────────────────────────────────────

func TestNormalizeToolParams_RepairsKnownMistakes(t *testing.T) {
	params := NormalizeToolParams(map[string]any{
		"expand": "12, #15",
		"range":  "3-9",
		"page":   "2",
		"mode":   " ALL ",
	})
	expand, ok := params["expand"].([]any)
	if !ok || len(expand) != 2 {
		t.Fatalf("expand = %#v", params["expand"])
	}
	rng, ok := params["range"].([]any)
	if !ok || len(rng) != 2 || rng[0] != float64(3) || rng[1] != float64(9) {
		t.Fatalf("range = %#v", params["range"])
	}
	if params["page"] != float64(2) {
		t.Fatalf("page = %#v", params["page"])
	}
	// mode:'all' means scope, so it is renamed.
	if params["scope"] != "all" {
		t.Fatalf("mode:all was not converted to scope: %#v", params)
	}
	if _, present := params["mode"]; present {
		t.Fatalf("mode was left behind: %#v", params)
	}
}

func TestNormalizeToolParams_DropsUnrepairable(t *testing.T) {
	params := NormalizeToolParams(map[string]any{
		"query":    "auth",
		"scope":    "everywhere",
		"page":     "first",
		"bogusKey": "value",
	})
	dropped, ok := params[DroppedKey].([]any)
	if !ok || len(dropped) != 3 {
		t.Fatalf("dropped = %#v (params %#v)", params[DroppedKey], params)
	}
	for _, key := range []string{"scope", "page", "bogusKey"} {
		if _, present := params[key]; present {
			t.Fatalf("%q survived normalization: %#v", key, params)
		}
	}
	if params["query"] != "auth" {
		t.Fatalf("a valid param was lost: %#v", params)
	}
}

func TestNormalizeToolParams_RangeOrderAndSingleIndex(t *testing.T) {
	params := NormalizeToolParams(map[string]any{"range": "9-3"})
	rng := params["range"].([]any)
	if rng[0] != float64(3) || rng[1] != float64(9) {
		t.Fatalf("a reversed range was not normalized: %#v", rng)
	}
	// A single index reads just that entry: [n, n].
	params = NormalizeToolParams(map[string]any{"range": 7})
	rng = params["range"].([]any)
	if len(rng) != 2 || rng[0] != float64(7) || rng[1] != float64(7) {
		t.Fatalf("a single index was not widened: %#v", rng)
	}
}

func TestNormalizeToolParams_NonObject(t *testing.T) {
	if got := NormalizeToolParams(nil); len(got) != 0 {
		t.Fatalf("NormalizeToolParams(nil) = %#v", got)
	}
	if got := NormalizeToolParams("string"); len(got) != 0 {
		t.Fatalf("NormalizeToolParams(string) = %#v", got)
	}
}

// ── parseToolRequest precedence ─────────────────────────────────────────────

func TestParseToolRequest_Precedence(t *testing.T) {
	// A drill-down query wins over everything else.
	request := ParseToolRequest(map[string]any{"query": "#12:a.go", "page": float64(2)})
	if request.Action.Kind != ActionDrill || request.Action.Drill == nil || request.Action.Drill.Index != 12 {
		t.Fatalf("drill did not win: %#v", request.Action)
	}
	if !containsStr(request.Ignored, "page") {
		t.Fatalf("the unused page param was not reported: %#v", request.Ignored)
	}

	// mode:'touched' beats a plain query.
	request = ParseToolRequest(map[string]any{"query": "auth", "mode": "touched"})
	if request.Action.Kind != ActionTouched {
		t.Fatalf("touched did not win: %#v", request.Action)
	}
	if !containsStr(request.Ignored, "query") {
		t.Fatalf("the ignored query was not reported: %#v", request.Ignored)
	}

	// expand beats range and a query.
	request = ParseToolRequest(map[string]any{
		"expand": []any{float64(1)},
		"range":  []any{float64(2), float64(3)},
		"query":  "auth",
	})
	if request.Action.Kind != ActionExpand || len(request.Action.Indices) != 1 {
		t.Fatalf("expand did not win: %#v", request.Action)
	}

	// range beats a query.
	request = ParseToolRequest(map[string]any{
		"range": []any{float64(2), float64(3)},
		"query": "auth",
	})
	if request.Action.Kind != ActionRange || len(request.Action.Range) != 2 {
		t.Fatalf("range did not win: %#v", request.Action)
	}

	// A query beats the recent default.
	request = ParseToolRequest(map[string]any{"query": "auth"})
	if request.Action.Kind != ActionSearch || request.Action.Query != "auth" {
		t.Fatalf("search did not win: %#v", request.Action)
	}

	// Nothing set: the most recent entries.
	request = ParseToolRequest(map[string]any{})
	if request.Action.Kind != ActionRecent {
		t.Fatalf("recent was not the default: %#v", request.Action)
	}
}

func TestParseToolRequest_ScopeFromParams(t *testing.T) {
	request := ParseToolRequest(map[string]any{"scope": "all", "query": "auth"})
	if request.Scope != ScopeAll {
		t.Fatalf("Scope = %v", request.Scope)
	}
	// An absent scope defaults to lineage.
	if got := ParseToolRequest(map[string]any{"query": "auth"}); got.Scope != ScopeLineage {
		t.Fatalf("default Scope = %v", got.Scope)
	}
}

func TestParseToolRequest_PageFloor(t *testing.T) {
	// A sub-1 page is clamped for the actions that use it.
	request := ParseToolRequest(map[string]any{"query": "auth", "page": float64(0)})
	if request.Action.Page != 1 {
		t.Fatalf("Page = %d, want 1", request.Action.Page)
	}
	request = ParseToolRequest(map[string]any{"query": "auth", "page": float64(4)})
	if request.Action.Page != 4 {
		t.Fatalf("Page = %d, want 4", request.Action.Page)
	}
}

func TestParseToolRequest_DroppedPropagates(t *testing.T) {
	params := NormalizeToolParams(map[string]any{"query": "auth", "scope": "everywhere"})
	request := ParseToolRequest(params)
	if len(request.Dropped) == 0 {
		t.Fatalf("the dropped note did not reach the request: %#v", request)
	}
}

// ── parseCommandRequest ─────────────────────────────────────────────────────

func TestParseCommandRequest(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantKind RecallActionKind
		wantQ    string
		scope    RecallScope
		page     int
	}{
		{"empty is recent", "", ActionRecent, "", ScopeLineage, 1},
		{"query", "auth bug", ActionSearch, "auth bug", ScopeLineage, 1},
		{"query with scope", "auth bug scope:all", ActionSearch, "auth bug", ScopeAll, 1},
		{"query with page", "auth page:3", ActionSearch, "auth", ScopeLineage, 3},
		{"scope and page together", "auth scope:all page:2", ActionSearch, "auth", ScopeAll, 2},
		// parseCommandRequest never drills down: a drill-down shaped argument is
		// just a search query on this path (drilling is a tool-call concern,
		// handled by ParseToolRequest).
		{"drill-down shape stays a search", "#4:a.go", ActionSearch, "#4:a.go", ScopeLineage, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseCommandRequest(test.args)
			if got.Action.Kind != test.wantKind {
				t.Fatalf("kind = %v, want %v (%#v)", got.Action.Kind, test.wantKind, got.Action)
			}
			if test.wantQ != "" && got.Action.Query != test.wantQ {
				t.Fatalf("query = %q, want %q", got.Action.Query, test.wantQ)
			}
			if got.Scope != test.scope {
				t.Fatalf("scope = %v, want %v", got.Scope, test.scope)
			}
			if got.Action.Kind == ActionSearch && got.Action.Page != test.page {
				t.Fatalf("page = %d, want %d", got.Action.Page, test.page)
			}
		})
	}
}
