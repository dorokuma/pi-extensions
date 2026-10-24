package ctxorchestrate

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the ctx_search_digest half of the extension: rg output parsing, the
// cross-pattern dedupe/aggregation, the argument forwarding, the degradation
// paths and the model-facing text limits. The nested ctx_fs calls are always a
// fake runner: no rg runs and no external binary is needed.

// fakeRunner stands in for ctx.ExecuteTool("ctx_fs", ...).
type fakeRunner struct {
	mu       sync.Mutex
	calls    []map[string]any
	inFlight int
	peak     int
	handle   func(index int, args map[string]any) nestedOutcome
}

func (f *fakeRunner) run(args map[string]any) nestedOutcome {
	f.mu.Lock()
	index := len(f.calls)
	f.calls = append(f.calls, args)
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if f.handle == nil {
		return nestedOutcome{ok: true}
	}
	return f.handle(index, args)
}

func (f *fakeRunner) callsSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.calls...)
}

// rgByPattern answers each pattern with its queued outputs in order, so a retry
// gets the second entry.
func rgByPattern(responses map[string][]nestedOutcome) *fakeRunner {
	var mu sync.Mutex
	seen := map[string]int{}
	return &fakeRunner{handle: func(_ int, args map[string]any) nestedOutcome {
		pattern, _ := args["pattern"].(string)
		mu.Lock()
		defer mu.Unlock()
		queue := responses[pattern]
		index := seen[pattern]
		seen[pattern] = index + 1
		if index >= len(queue) {
			return nestedOutcome{ok: true}
		}
		return queue[index]
	}}
}

// renderRg writes a ctxmode-shaped rg output for the given files.
func renderRg(files []RgFile, reported int, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "engine=rg matches=%d files=%d limit=50 truncated=%t git=none indexed=yes\n", reported, len(files), truncated)
	fmt.Fprintf(&b, "%d matches in %d files (full set indexed, > first-screen limit 50). Retrieve details: ctx_fs action=rg\n", reported, len(files))
	b.WriteString("Files (M=modified, A=added, ??=untracked; then by match count):\n")
	for _, file := range files {
		fmt.Fprintf(&b, "%s %d matches (27KB - use offset to read relevant section)\n", file.Path, len(file.Hits))
		for _, hit := range file.Hits {
			fmt.Fprintf(&b, "  %d: %s\n", hit.Line, hit.Excerpt)
		}
	}
	return b.String()
}

func textOutcome(text string) nestedOutcome { return nestedOutcome{ok: true, text: text} }

func digestOf(t *testing.T, result any) SearchDigestOutput {
	t.Helper()
	out, ok := result.(SearchDigestOutput)
	if !ok {
		t.Fatalf("structuredContent is %T, want SearchDigestOutput", result)
	}
	return out
}

// ── parseRgOutput ───────────────────────────────────────────────────────────

func TestParseRgOutputGroupedShape(t *testing.T) {
	input := strings.Join([]string{
		"engine=rg matches=7 files=2 limit=3 truncated=false git=none indexed=yes",
		"→ Read .pi/agent/SYSTEM.md (27KB - use offset to read relevant section)",
		"7 matches in 2 files (full set indexed, > first-screen limit 3). Retrieve details: ctx_fs action=rg",
		"Files (M=modified, A=added, ??=untracked; then by match count):",
		".pi/agent/SYSTEM.md 4 matches (27KB - use offset to read relevant section)",
		"  55: **codemode 灰度规则**",
		"  77: ctx_search_digest 用法",
		"  ... (+2 more)",
		"go/pigsdk/extension.go 3 matches (12KB - use offset to read relevant section)",
		"  26: type Schema = map[string]any",
		"  43: type EventFunc func(ctx Context, data map[string]any) (any, error)",
	}, "\n")

	parse := ParseRgOutput(input, hitsDefault)

	if parse.ReportedMatches != 7 {
		t.Fatalf("reportedMatches = %d, want 7", parse.ReportedMatches)
	}
	if parse.Truncated {
		t.Fatalf("truncated = true, want false")
	}
	if len(parse.Files) != 2 {
		t.Fatalf("files = %d, want 2 (%+v)", len(parse.Files), parse.Files)
	}
	if parse.Files[0].Path != ".pi/agent/SYSTEM.md" || parse.Files[1].Path != "go/pigsdk/extension.go" {
		t.Fatalf("paths = %q, %q", parse.Files[0].Path, parse.Files[1].Path)
	}
	wantFirst := []RgHit{{Line: 55, Excerpt: "**codemode 灰度规则**"}, {Line: 77, Excerpt: "ctx_search_digest 用法"}}
	if !reflect.DeepEqual(parse.Files[0].Hits, wantFirst) {
		t.Fatalf("first file hits = %+v, want %+v", parse.Files[0].Hits, wantFirst)
	}
	if len(parse.Files[1].Hits) != 2 || parse.Files[1].Hits[0].Line != 26 {
		t.Fatalf("second file hits = %+v", parse.Files[1].Hits)
	}
}

func TestParseRgOutputBareFileLineShape(t *testing.T) {
	input := strings.Join([]string{
		"a.go:12:func main() {",
		"b.go:3:x := 1",
		"  7: indented line belongs to b.go",
	}, "\n")

	parse := ParseRgOutput(input, hitsDefault)

	if len(parse.Files) != 2 {
		t.Fatalf("files = %+v", parse.Files)
	}
	if parse.Files[0].Path != "a.go" || len(parse.Files[0].Hits) != 1 || parse.Files[0].Hits[0].Line != 12 {
		t.Fatalf("a.go hits = %+v", parse.Files[0])
	}
	if parse.Files[1].Path != "b.go" {
		t.Fatalf("b.go path = %q", parse.Files[1].Path)
	}
	want := []RgHit{{Line: 3, Excerpt: "x := 1"}, {Line: 7, Excerpt: "indented line belongs to b.go"}}
	if !reflect.DeepEqual(parse.Files[1].Hits, want) {
		t.Fatalf("b.go hits = %+v, want %+v", parse.Files[1].Hits, want)
	}
	if parse.ReportedMatches != 0 {
		t.Fatalf("reportedMatches = %d, want 0 (no engine header)", parse.ReportedMatches)
	}
}

func TestParseRgOutputTruncationSignals(t *testing.T) {
	flag := "engine=rg matches=9 files=1 limit=1 truncated=true git=none\nsrc.go 9 matches\n  1: x\n"
	if parse := ParseRgOutput(flag, hitsDefault); !parse.Truncated {
		t.Fatalf("truncated=true in the head must set Truncated")
	}
	capture := "engine=rg matches=9 files=1 limit=1 truncated=false git=none\ncapture truncated at 500 hits\nsrc.go 9 matches\n  1: x\n"
	if parse := ParseRgOutput(capture, hitsDefault); !parse.Truncated {
		t.Fatalf("`capture truncated` in the body must set Truncated")
	}
	if parse := ParseRgOutput("", hitsDefault); len(parse.Files) != 0 || parse.Truncated {
		t.Fatalf("empty input = %+v, want no files", parse)
	}
}

func TestParseRgOutputMaxPerFile(t *testing.T) {
	input := "src.go 3 matches\n  1: a\n  2: b\n  3: c\n"
	parse := ParseRgOutput(input, 2)
	if len(parse.Files) != 1 || len(parse.Files[0].Hits) != 2 {
		t.Fatalf("hits = %+v, want 2 kept", parse.Files)
	}
	if parse.Files[0].Hits[1].Line != 2 {
		t.Fatalf("kept = %+v", parse.Files[0].Hits)
	}
}

func TestParseRgOutputExcerptIsFlattenedAndCapped(t *testing.T) {
	long := strings.Repeat("字", ExcerptCap+50)
	input := "src.go 1 matches\n  9: a\t\tb   c\n"
	parse := ParseRgOutput(input, hitsDefault)
	if got := parse.Files[0].Hits[0].Excerpt; got != "a b c" {
		t.Fatalf("excerpt = %q, want whitespace collapsed", got)
	}
	longParse := ParseRgOutput("src.go 1 matches\n  9: "+long+"\n", hitsDefault)
	excerpt := longParse.Files[0].Hits[0].Excerpt
	if utf16Len(excerpt) != ExcerptCap {
		t.Fatalf("excerpt units = %d, want %d", utf16Len(excerpt), ExcerptCap)
	}
	if !strings.HasSuffix(excerpt, "…") {
		t.Fatalf("excerpt must end with the ellipsis")
	}
}

// ── aggregation ─────────────────────────────────────────────────────────────

func TestSearchDigestDedupesHitsAcrossPatterns(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"foo": {textOutcome(renderRg([]RgFile{{Path: "src/a.go", Hits: []RgHit{
			{Line: 10, Excerpt: "foo one"},
			{Line: 20, Excerpt: "foo two"},
		}}}, 2, false))},
		"bar": {textOutcome(renderRg([]RgFile{{Path: "src/a.go", Hits: []RgHit{
			{Line: 20, Excerpt: "bar two is longer"},
			{Line: 30, Excerpt: "bar three"},
		}}}, 2, false))},
	})

	result := executeSearchDigest(map[string]any{"patterns": []any{"foo", "bar"}}, runner.run)
	if result.IsError {
		t.Fatalf("IsError = true, want false (%s)", result.Content)
	}
	out := digestOf(t, result.StructuredContent)
	if out.Summary != "patterns=2 files=1 hits=3 (rg reported 4)" {
		t.Fatalf("summary = %q", out.Summary)
	}
	if len(out.Files) != 1 || len(out.Files[0].Hits) != 3 {
		t.Fatalf("files = %+v", out.Files)
	}
	hits := out.Files[0].Hits
	if hits[0].Line != 10 || !reflect.DeepEqual(hits[0].Patterns, []string{"foo"}) {
		t.Fatalf("hit 0 = %+v", hits[0])
	}
	if hits[1].Line != 20 || !reflect.DeepEqual(hits[1].Patterns, []string{"foo", "bar"}) {
		t.Fatalf("hit 1 = %+v, want the shared line with both patterns", hits[1])
	}
	if hits[1].Excerpt != "bar two is longer" {
		t.Fatalf("hit 1 excerpt = %q, want the longer excerpt", hits[1].Excerpt)
	}
	if hits[2].Line != 30 || !reflect.DeepEqual(hits[2].Patterns, []string{"bar"}) {
		t.Fatalf("hit 2 = %+v", hits[2])
	}
	if !strings.Contains(result.Content, "src/a.go (3)") {
		t.Fatalf("text = %q", result.Content)
	}
	if !strings.Contains(result.Content, "  20: bar two is longer  [foo, bar]") {
		t.Fatalf("text = %q", result.Content)
	}
}

func TestSearchDigestFileOrderByHitCountThenPath(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"p": {textOutcome(renderRg([]RgFile{
			{Path: "b.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}},
			{Path: "a.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}},
			{Path: "c.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}, {Line: 2, Excerpt: "y"}}},
		}, 4, false))},
	})

	out := digestOf(t, executeSearchDigest(map[string]any{"patterns": []any{"p"}}, runner.run).StructuredContent)

	paths := []string{out.Files[0].Path, out.Files[1].Path, out.Files[2].Path}
	if !reflect.DeepEqual(paths, []string{"c.go", "a.go", "b.go"}) {
		t.Fatalf("paths = %v, want hit-count desc then path asc", paths)
	}
}

func TestSearchDigestEmptyPatterns(t *testing.T) {
	runner := rgByPattern(nil)
	result := executeSearchDigest(map[string]any{"patterns": []any{"", "   "}}, runner.run)

	if !result.IsError {
		t.Fatalf("IsError = false, want true")
	}
	if result.Content != "ctx_search_digest: no patterns given" {
		t.Fatalf("content = %q", result.Content)
	}
	out := digestOf(t, result.StructuredContent)
	if out.Patterns == nil || len(out.Patterns) != 0 || out.Files == nil || len(out.Files) != 0 {
		t.Fatalf("empty result must carry empty (not nil) patterns/files: %+v", out)
	}
	if !reflect.DeepEqual(out.Errors, []SearchDigestError{{Pattern: "", Error: "patterns is empty"}}) {
		t.Fatalf("errors = %+v", out.Errors)
	}
	if len(runner.callsSnapshot()) != 0 {
		t.Fatalf("no nested call may run without patterns")
	}
}

func TestSearchDigestForwardsArguments(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"alpha": {textOutcome(renderRg([]RgFile{{Path: "a.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}}}, 1, false))},
		"beta":  {textOutcome(renderRg([]RgFile{{Path: "a.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}}}, 1, false))},
	})

	executeSearchDigest(map[string]any{
		"patterns":       []any{"alpha", "beta"},
		"path":           " src ",
		"glob":           " *.go ",
		"max_hits":       float64(999),
		"include_hidden": true,
		"ignore_case":    true,
		"literal":        true,
		"context":        float64(9),
	}, runner.run)

	calls := runner.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("nested calls = %d, want 2", len(calls))
	}
	// The calls run concurrently, so index them by pattern rather than arrival order.
	byPattern := map[string]map[string]any{}
	for _, call := range calls {
		pattern, _ := call["pattern"].(string)
		byPattern[pattern] = call
	}
	want := map[string]any{
		"action": "rg", "path": "src", "glob": "*.go", "limit": hitsHard,
		"include_hidden": true, "ignore_case": true, "literal": true, "context": maxContextLines,
	}
	for _, pattern := range []string{"alpha", "beta"} {
		expected := map[string]any{"pattern": pattern}
		for key, value := range want {
			expected[key] = value
		}
		if !reflect.DeepEqual(byPattern[pattern], expected) {
			t.Fatalf("call for %q = %+v, want %+v", pattern, byPattern[pattern], expected)
		}
	}
}

func TestSearchDigestMaxHitsClamping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   int
	}{
		{"absent", map[string]any{"patterns": []any{"p"}}, hitsDefault},
		{"zero", map[string]any{"patterns": []any{"p"}, "max_hits": float64(0)}, hitsDefault},
		{"negative", map[string]any{"patterns": []any{"p"}, "max_hits": float64(-3)}, hitsDefault},
		{"hard cap", map[string]any{"patterns": []any{"p"}, "max_hits": float64(501)}, hitsHard},
		{"in range", map[string]any{"patterns": []any{"p"}, "max_hits": float64(7)}, 7},
		{"not a number", map[string]any{"patterns": []any{"p"}, "max_hits": "12"}, hitsDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := rgByPattern(map[string][]nestedOutcome{
				"p": {textOutcome(renderRg([]RgFile{{Path: "a.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}}}, 1, false))},
			})
			executeSearchDigest(tc.params, runner.run)
			calls := runner.callsSnapshot()
			if len(calls) != 1 {
				t.Fatalf("calls = %d", len(calls))
			}
			if got := calls[0]["limit"]; got != tc.want {
				t.Fatalf("limit = %v, want %d", got, tc.want)
			}
		})
	}
}

func TestSearchDigestHiddenRetryOnZeroHits(t *testing.T) {
	empty := renderRg(nil, 0, false)
	hit := renderRg([]RgFile{{Path: ".hidden/secret.txt", Hits: []RgHit{{Line: 4, Excerpt: "needle"}}}}, 1, false)
	runner := rgByPattern(map[string][]nestedOutcome{
		"needle": {textOutcome(empty), textOutcome(hit)},
	})

	out := digestOf(t, executeSearchDigest(map[string]any{
		"patterns": []any{"needle"},
		"path":     "../",
	}, runner.run).StructuredContent)

	calls := runner.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want a retry", len(calls))
	}
	if _, ok := calls[0]["include_hidden"]; ok {
		t.Fatalf("first call must not force include_hidden: %+v", calls[0])
	}
	if calls[1]["include_hidden"] != true || calls[1]["glob"] != "**" {
		t.Fatalf("retry args = %+v, want include_hidden + ** glob", calls[1])
	}
	if len(out.Files) != 1 || out.Files[0].Path != ".hidden/secret.txt" {
		t.Fatalf("retry result not used: %+v", out.Files)
	}
}

func TestSearchDigestNoRetryWithoutRoot(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"needle": {textOutcome(renderRg(nil, 0, false))},
	})
	executeSearchDigest(map[string]any{"patterns": []any{"needle"}}, runner.run)
	if calls := runner.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("calls = %d, want no retry without path", len(calls))
	}
}

func TestSearchDigestPartialFailureStaysData(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"ok":   {textOutcome(renderRg([]RgFile{{Path: "a.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}}}, 1, false))},
		"boom": {{ok: false, isError: true, err: "rg exploded"}},
	})

	result := executeSearchDigest(map[string]any{"patterns": []any{"ok", "boom"}}, runner.run)
	if result.IsError {
		t.Fatalf("partial failure must not be an error result")
	}
	out := digestOf(t, result.StructuredContent)
	if !reflect.DeepEqual(out.Errors, []SearchDigestError{{Pattern: "boom", Error: "rg exploded"}}) {
		t.Fatalf("errors = %+v", out.Errors)
	}
	if !strings.Contains(out.Summary, "; 1 pattern(s) degraded: boom") {
		t.Fatalf("summary = %q", out.Summary)
	}
}

func TestSearchDigestTotalFailureIsError(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"a": {{ok: false, isError: true, err: "host down"}},
		"b": {{ok: false, isError: true}},
	})
	result := executeSearchDigest(map[string]any{"patterns": []any{"a", "b"}}, runner.run)
	if !result.IsError {
		t.Fatalf("IsError = false, want true when every pattern failed")
	}
	out := digestOf(t, result.StructuredContent)
	if len(out.Errors) != 2 {
		t.Fatalf("errors = %+v", out.Errors)
	}
	if out.Errors[1].Error != "rg failed" {
		t.Fatalf("empty error must fall back to %q, got %q", "rg failed", out.Errors[1].Error)
	}
}

func TestSearchDigestTruncatedSummary(t *testing.T) {
	runner := rgByPattern(map[string][]nestedOutcome{
		"p": {textOutcome(renderRg([]RgFile{{Path: "a.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}}}, 1, true))},
	})
	out := digestOf(t, executeSearchDigest(map[string]any{"patterns": []any{"p"}}, runner.run).StructuredContent)
	if !strings.Contains(out.Summary, "; rg output truncated — narrow path/glob or page with offset") {
		t.Fatalf("summary = %q", out.Summary)
	}
}

func TestSearchDigestTextLimits(t *testing.T) {
	files := make([]RgFile, 0, 45)
	for i := 0; i < 45; i++ {
		hits := make([]RgHit, 0, 7)
		for j := 0; j < 7; j++ {
			hits = append(hits, RgHit{Line: j + 1, Excerpt: fmt.Sprintf("hit %d", j)})
		}
		files = append(files, RgFile{Path: fmt.Sprintf("f%02d.go", i), Hits: hits})
	}
	runner := rgByPattern(map[string][]nestedOutcome{
		"p": {textOutcome(renderRg(files, 315, false))},
	})

	result := executeSearchDigest(map[string]any{"patterns": []any{"p"}}, runner.run)

	if !strings.Contains(result.Content, "... (+5 more files)") {
		t.Fatalf("text = %q", result.Content)
	}
	if !strings.Contains(result.Content, "  ... (+2 more hits)") {
		t.Fatalf("text = %q", result.Content)
	}
	if out := digestOf(t, result.StructuredContent); len(out.Files) != 45 {
		t.Fatalf("structured files = %d, want all 45 (text is capped, data is not)", len(out.Files))
	}
}

func TestSearchDigestBoundsConcurrency(t *testing.T) {
	patterns := make([]any, 0, 9)
	for i := 0; i < 9; i++ {
		patterns = append(patterns, fmt.Sprintf("p%d", i))
	}
	runner := &fakeRunner{handle: func(_ int, _ map[string]any) nestedOutcome {
		time.Sleep(5 * time.Millisecond)
		return textOutcome(renderRg([]RgFile{{Path: "same.go", Hits: []RgHit{{Line: 1, Excerpt: "x"}}}}, 1, false))
	}}

	result := executeSearchDigest(map[string]any{"patterns": patterns}, runner.run)

	if len(runner.callsSnapshot()) != 9 {
		t.Fatalf("calls = %d, want 9", len(runner.callsSnapshot()))
	}
	if runner.peak > concurrency {
		t.Fatalf("peak in flight = %d, want <= %d", runner.peak, concurrency)
	}
	if runner.peak < 2 {
		t.Fatalf("peak in flight = %d, want the calls to run concurrently", runner.peak)
	}
	// The shared file:line is one hit carrying every pattern, in input order.
	out := digestOf(t, result.StructuredContent)
	if len(out.Files) != 1 || len(out.Files[0].Hits) != 1 {
		t.Fatalf("aggregate = %+v", out.Files)
	}
	want := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"}
	if !reflect.DeepEqual(out.Files[0].Hits[0].Patterns, want) {
		t.Fatalf("patterns = %v, want %v", out.Files[0].Hits[0].Patterns, want)
	}
	if out.Summary != "patterns=9 files=1 hits=1 (rg reported 9)" {
		t.Fatalf("summary = %q", out.Summary)
	}
}
