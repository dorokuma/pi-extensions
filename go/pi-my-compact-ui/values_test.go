package compactui

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test shortenPath (TS L71-74).
func TestShortenPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory in this environment")
	}
	cases := []struct{ in, want string }{
		{home, "~"},
		{home + "/work", "~/work"},
		{home + "/a/b/c.ts", "~/a/b/c.ts"},
		{"/tmp/elsewhere", "/tmp/elsewhere"},
		{"", ""},
		{"relative/path", "relative/path"},
	}
	for _, tc := range cases {
		if got := shortenPath(tc.in); got != tc.want {
			t.Errorf("shortenPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Test oneLine (TS L75-82), including the JS UTF-16 truncation rules.
func TestOneLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"whitespace collapse", "  a \n\t b   c  ", -1, "a b c"},
		{"empty", "", -1, ""},
		{"zero max", "abcdef", 0, ""},
		{"max 1 literal dots", "abcdef", 1, "."},
		{"max 3 literal dots", "abcdef", 3, "..."},
		{"max 4 keeps one char", "abcdef", 4, "a..."},
		{"max 5", "abcdef", 5, "ab..."},
		{"exact length is unchanged", "abcd", 4, "abcd"},
		{"shorter than max", "ab", 10, "ab"},
		{"collapse then truncate", "a   b  c  d", 6, "a b..."},
	}
	for _, tc := range cases {
		if got := oneLine(tc.in, tc.max); got != tc.want {
			t.Errorf("%s: oneLine(%q, %d) = %q, want %q", tc.name, tc.in, tc.max, got, tc.want)
		}
	}
	// A non-BMP rune counts as two UTF-16 units, as JS .length does.
	if got := utf16Len("😀"); got != 2 {
		t.Fatalf("utf16Len(emoji) = %d, want 2", got)
	}
	if got := oneLine("😀😀😀😀", 6); got != "😀..." {
		t.Errorf("oneLine(emoji, 6) = %q, want %q", got, "😀...")
	}
	if got := utf16Slice("a😀b", 0, 2); got != "a" {
		t.Errorf("utf16Slice split a surrogate pair into %q, want %q", got, "a")
	}
	if got := utf16Slice("a😀b", 0, 3); got != "a😀" {
		t.Errorf("utf16Slice full pair = %q, want %q", got, "a😀")
	}
}

// Test estimateTextTokens (TS L83-85).
func TestEstimateTextTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{strings.Repeat("x", 400), 100},
	}
	for _, tc := range cases {
		if got := estimateTextTokens(tc.in); got != tc.want {
			t.Errorf("estimateTextTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if got := estimateTextTokens("😀"); got != 1 {
		t.Errorf("estimateTextTokens(emoji) = %d, want 1 (2 UTF-16 units / 4 rounded up)", got)
	}
}

// Test formatTokenK (TS L86-92).
func TestFormatTokenK(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0.0K"},
		{-5, "0.0K"},
		{1, "<0.1K"},
		{99, "<0.1K"},
		{100, "0.1K"},
		{1234, "1.2K"},
		{12345, "12.3K"},
		{2550, "2.5K"},
		{99949, "99.9K"},
		{99950, "100.0K"},
		{100000, "100K"},
		{1234567, "1235K"},
	}
	for _, tc := range cases {
		if got := formatTokenK(tc.in); got != tc.want {
			t.Errorf("formatTokenK(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Test jsTenths / jsTenthsString reproduce JS `(n / 1000).toFixed(1)` at the
// binary-float boundary (task 必修 5). JS rounds the IEEE-754 double
// `n / 1000`, not the exact rational n/1000: 2550/1000 is the double just below
// 2.55, so JS prints "2.5" where exact integer arithmetic prints "2.6".
// 1250/1000 is exactly 1.25, an exact tie, which ECMA-262 toFixed rounds up to
// "1.3". Every expected value below was cross-checked against
// `node -e '(n/1000).toFixed(1)'`.
func TestJsTenthsBinaryFloatBoundary(t *testing.T) {
	cases := []struct {
		tokens int
		want   string
	}{
		{1000, "1.0"},
		{1050, "1.1"},
		{1150, "1.1"},
		{1250, "1.3"},
		{1350, "1.4"},
		{1450, "1.4"},
		{1550, "1.6"},
		{1650, "1.6"},
		{1750, "1.8"},
		{1850, "1.9"},
		{1950, "1.9"},
		{2050, "2.0"},
		{2150, "2.1"},
		{2250, "2.3"},
		{2350, "2.4"},
		{2450, "2.5"},
		{2550, "2.5"},
		{2650, "2.6"},
		{2750, "2.8"},
		{2850, "2.9"},
		{2950, "3.0"},
		{3050, "3.0"},
		{9950, "9.9"},
		{10050, "10.1"},
		{99950, "100.0"},
	}
	for _, tc := range cases {
		if got := jsTenthsString(jsTenths(tc.tokens)); got != tc.want {
			t.Errorf("jsTenths(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
	}
	// The label helpers carry the same tenths through.
	if got := formatTokenK(2550); got != "2.5K" {
		t.Errorf("formatTokenK(2550) = %q, want 2.5K", got)
	}
	if got := thinkingTokenLabel(&thinkingEntry{tokens: 2550}); got != "(2.5k)" {
		t.Errorf("thinkingTokenLabel(2550) = %q, want (2.5k)", got)
	}
}

// Test thinkingTokenLabel (TS L93-101): lower-case k and the two regimes.
func TestThinkingTokenLabel(t *testing.T) {
	cases := []struct {
		entry *thinkingEntry
		want  string
	}{
		{nil, "(0t)"},
		{&thinkingEntry{tokens: 0}, "(0t)"},
		{&thinkingEntry{tokens: -7}, "(0t)"},
		{&thinkingEntry{tokens: 1}, "(1t)"},
		{&thinkingEntry{tokens: 999}, "(999t)"},
		{&thinkingEntry{tokens: 1000}, "(1.0k)"},
		{&thinkingEntry{tokens: 1234}, "(1.2k)"},
		{&thinkingEntry{tokens: 99949}, "(99.9k)"},
		{&thinkingEntry{tokens: 99950}, "(100.0k)"},
		{&thinkingEntry{tokens: 100000}, "(100k)"},
		{&thinkingEntry{tokens: 123456}, "(123k)"},
	}
	for _, tc := range cases {
		if got := thinkingTokenLabel(tc.entry); got != tc.want {
			t.Errorf("thinkingTokenLabel(%v) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

// Test formatToolDuration (TS L173-185).
func TestFormatToolDuration(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0ms"},
		{-1, "0ms"},
		{0.0004, "0ms"},
		{0.001, "1ms"},
		{0.9994, "999ms"},
		{0.9999, "1.00s"},
		{1, "1.00s"},
		{9.999, "10.00s"},
		{10, "10.0s"},
		{12.34, "12.3s"},
		{1234.5, "1234.5s"},
	}
	for _, tc := range cases {
		if got := formatToolDuration(tc.in); got != tc.want {
			t.Errorf("formatToolDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A non-finite duration falls back to zero, as `Number.isFinite` guards it.
	if got := formatToolDuration(nan()); got != "0ms" {
		t.Errorf("formatToolDuration(NaN) = %q, want %q", got, "0ms")
	}
	if got := formatToolDuration(inf()); got != "0ms" {
		t.Errorf("formatToolDuration(Inf) = %q, want %q", got, "0ms")
	}
}

// Test formatWorkedTime (TS L164-172).
func TestFormatWorkedTime(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "1s"},
		{499, "1s"},
		{999, "1s"},
		{1000, "1s"},
		{1500, "2s"},
		{59_000, "59s"},
		{60_000, "1m"},
		{61_000, "1m 1s"},
		{80_000, "1m 20s"},
		{3_600_000, "1h"},
		{3_660_000, "1h 1m"},
		{3_600_000 + 600_000, "1h 10m"},
		{7_320_000, "2h 2m"},
	}
	for _, tc := range cases {
		if got := formatWorkedTime(tc.in); got != tc.want {
			t.Errorf("formatWorkedTime(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Test toolSummary (TS L102-133): every branch plus the generic fallback.
func TestToolSummary(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/home/nobody"
	}
	cases := []struct {
		name string
		tool string
		args map[string]any
		want toolSummaryValue
	}{
		{"bash", "bash", map[string]any{"command": "ls  -la"}, toolSummaryValue{"bash", "ls -la"}},
		{"powershell empty", "powershell", nil, toolSummaryValue{"powershell", "..."}},
		{"read", "read", map[string]any{"path": home + "/x.go"}, toolSummaryValue{"read", "~/x.go"}},
		{"write", "write", map[string]any{"path": "/tmp/a"}, toolSummaryValue{"write", "/tmp/a"}},
		{"edit", "edit", map[string]any{"path": "/tmp/b"}, toolSummaryValue{"edit", "/tmp/b"}},
		{"find", "find", map[string]any{"pattern": "*.go", "path": "/tmp"}, toolSummaryValue{"find", "*.go in /tmp"}},
		{"find defaults", "find", nil, toolSummaryValue{"find", " in ."}},
		{"grep", "grep", map[string]any{"pattern": "TODO", "path": home + "/w"}, toolSummaryValue{"grep", "TODO in ~/w"}},
		{"grep defaults", "grep", map[string]any{}, toolSummaryValue{"grep", " in ."}},
		{"ls", "ls", map[string]any{"path": "/var"}, toolSummaryValue{"ls", "/var"}},
		{"ls default", "ls", nil, toolSummaryValue{"ls", "."}},
		{"web_search", "web_search", map[string]any{"query": "go  race"}, toolSummaryValue{"web_search", "go race"}},
		{"subagent agent", "subagent", map[string]any{"agent": "worker", "task": "t"}, toolSummaryValue{"subagent", "worker"}},
		{"subagent task", "subagent", map[string]any{"task": "do it"}, toolSummaryValue{"subagent", "do it"}},
		{"subagent default", "subagent", nil, toolSummaryValue{"subagent", "..."}},
		{"codemode", "codemode", map[string]any{"code": "1 +  1"}, toolSummaryValue{"codemode", "1 + 1"}},
		{"generic path", "mystery", map[string]any{"path": "/x"}, toolSummaryValue{"mystery", "/x"}},
		{"generic query", "mystery", map[string]any{"query": "q"}, toolSummaryValue{"mystery", "q"}},
		{"generic name", "mystery", map[string]any{"name": "n"}, toolSummaryValue{"mystery", "n"}},
		{"generic description", "mystery", map[string]any{"description": "d"}, toolSummaryValue{"mystery", "d"}},
		{"generic url", "mystery", map[string]any{"url": "u"}, toolSummaryValue{"mystery", "u"}},
		{"generic command", "ctx_run", map[string]any{"action": "execute", "command": "git status"}, toolSummaryValue{"ctx_run", "git status"}},
		{"generic argv", "ctx_run", map[string]any{"action": "execute", "argv": []any{"herdr", "pane", "read"}}, toolSummaryValue{"ctx_run", "herdr pane read"}},
		{"generic commands", "ctx_run", map[string]any{"action": "batch", "commands": []any{map[string]any{"command": "gh run list"}}}, toolSummaryValue{"ctx_run", "gh run list"}},
		{"generic action only", "ctx_run", map[string]any{"action": "execute"}, toolSummaryValue{"ctx_run", "execute"}},
		{"generic nothing", "mystery", map[string]any{"other": 1}, toolSummaryValue{"mystery", "..."}},
	}
	for _, tc := range cases {
		if got := toolSummary(tc.tool, tc.args); got != tc.want {
			t.Errorf("%s: toolSummary = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// A non-string JSON value is stringified the way String(value) does.
	if got := toolSummary("mystery", map[string]any{"name": 42.0}); got.content != "42" {
		t.Errorf("numeric argument = %q, want %q", got.content, "42")
	}
}

// Test toolStatus (TS L134-141).
func TestToolStatus(t *testing.T) {
	r := testRuntime(t)
	group := r.newToolGroup(0)

	pending := newTestTool("p1", "read", nil)
	pending.executionStarted = true
	if got := toolStatus(pending, nil); got != "pending" {
		t.Errorf("started, no result: %q, want pending", got)
	}

	partial := newTestTool("p2", "read", nil)
	partial.isPartial = true
	if got := toolStatus(partial, nil); got != "pending" {
		t.Errorf("partial: %q, want pending", got)
	}

	ok := newTestTool("p3", "read", nil).succeeded("done")
	if got := toolStatus(ok, nil); got != "success" {
		t.Errorf("result: %q, want success", got)
	}

	bad := newTestTool("p4", "read", nil).failed()
	if got := toolStatus(bad, nil); got != "error" {
		t.Errorf("isError: %q, want error", got)
	}

	interrupted := newTestTool("p5", "read", nil)
	interrupted.interrupted = true
	if got := toolStatus(interrupted, nil); got != "error" {
		t.Errorf("interrupted: %q, want error", got)
	}

	// A sealed group turns a result-less tool into an error.
	sealed := newTestTool("p6", "read", nil)
	sealed.executionStarted = true
	group.sealed = true
	sealed.owner = group
	if got := toolStatus(sealed, nil); got != "error" {
		t.Errorf("sealed group, no result: %q, want error", got)
	}
	group.sealed = false

	// The owner argument wins when the tool has no owner of its own.
	lonely := newTestTool("p7", "read", nil)
	lonely.executionStarted = true
	group.sealed = true
	if got := toolStatus(lonely, group); got != "error" {
		t.Errorf("owner argument: %q, want error", got)
	}
	if got := toolStatus(nil, nil); got != "pending" {
		t.Errorf("nil tool: %q, want pending", got)
	}
}

// Test streamingToolArgs (TS L142-155).
func TestStreamingToolArgs(t *testing.T) {
	complete := streamingToolArgs(streamBlock{arguments: map[string]any{"command": "ls"}})
	if complete["command"] != "ls" {
		t.Errorf("complete arguments = %v", complete)
	}
	cases := []struct {
		name  string
		block streamBlock
		key   string
		want  string
	}{
		{"bash partial", streamBlock{name: "bash", partialJSON: "  ls -la "}, "command", "ls -la"},
		{"powershell partial", streamBlock{name: "powershell", partialJSON: "dir"}, "command", "dir"},
		{"read partial", streamBlock{name: "read", partialJSON: "/tmp/x"}, "path", "/tmp/x"},
		{"write partial", streamBlock{name: "write", partialJSON: "/tmp/x"}, "path", "/tmp/x"},
		{"edit partial", streamBlock{name: "edit", partialJSON: "/tmp/x"}, "path", "/tmp/x"},
		{"ls partial", streamBlock{name: "ls", partialJSON: "/tmp"}, "path", "/tmp"},
		{"codemode partial", streamBlock{name: "codemode", partialJSON: "1+1"}, "code", "1+1"},
		{"unknown partial", streamBlock{name: "mystery", partialJSON: "q"}, "query", "q"},
		{"partialArgs fallback", streamBlock{name: "read", partialArgs: "/tmp/y"}, "path", "/tmp/y"},
	}
	for _, tc := range cases {
		got := streamingToolArgs(tc.block)
		if got[tc.key] != tc.want {
			t.Errorf("%s: args[%q] = %v, want %q", tc.name, tc.key, got[tc.key], tc.want)
		}
	}
	if got := streamingToolArgs(streamBlock{name: "read"}); len(got) != 0 {
		t.Errorf("empty partial = %v, want empty object", got)
	}
}

// Test locateStreamingToolCall (TS L156-172).
func TestLocateStreamingToolCall(t *testing.T) {
	content := []contentBlock{
		{typ: "text"},
		{typ: "toolCall", block: streamBlock{id: "a", typ: "toolCall"}},
		{typ: "toolCall", block: streamBlock{id: "b", typ: "toolCall"}},
	}
	// The content index wins when it points at a tool call.
	index, block, ok := locateStreamingToolCall(content, streamEvent{contentIndex: intPtrValue(2)})
	if !ok || index != 2 || block.id != "b" {
		t.Errorf("by index = (%d, %q, %v)", index, block.id, ok)
	}
	// An index pointing at a non-tool-call block falls through to the event.
	index, block, ok = locateStreamingToolCall(content, streamEvent{
		contentIndex: intPtrValue(0),
		toolCall:     &streamBlock{id: "b", typ: "toolCall"},
	})
	if !ok || index != 2 || block.id != "b" {
		t.Errorf("by id = (%d, %q, %v)", index, block.id, ok)
	}
	// An unknown id appends at the reported index.
	index, _, ok = locateStreamingToolCall(content, streamEvent{
		contentIndex: intPtrValue(7),
		toolCall:     &streamBlock{id: "zzz", typ: "toolCall"},
	})
	if !ok || index != 7 {
		t.Errorf("unknown id index = %d, want 7", index)
	}
	// Without an index the block lands at len(content), as the TS does.
	index, _, ok = locateStreamingToolCall(content, streamEvent{toolCall: &streamBlock{id: "zzz", name: "read"}})
	if !ok || index != len(content) {
		t.Errorf("no index: index = %d, want %d", index, len(content))
	}
	// A block with neither a toolCall type nor a name is not located.
	if _, _, ok := locateStreamingToolCall(content, streamEvent{toolCall: &streamBlock{}}); ok {
		t.Error("empty toolCall block should not be located")
	}
	if _, _, ok := locateStreamingToolCall(content, streamEvent{}); ok {
		t.Error("empty event should not be located")
	}
}

// Test toolResultText (TS L173-175).
func TestToolResultText(t *testing.T) {
	tool := newTestTool("t", "bash", nil)
	tool.result = &toolResult{content: []contentItem{
		{typ: "text", text: "  line one"},
		{typ: "image"},
		{typ: "text", text: "line two  "},
	}}
	if got := toolResultText(tool); got != "line one\nline two" {
		t.Errorf("toolResultText = %q", got)
	}
	if got := toolResultText(nil); got != "" {
		t.Errorf("nil tool = %q, want empty", got)
	}
	empty := newTestTool("t2", "bash", nil)
	if got := toolResultText(empty); got != "" {
		t.Errorf("no result = %q, want empty", got)
	}
}

// Test isCtrlI (TS L186-192): the literal tab and key releases are excluded.
func TestIsCtrlI(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"literal tab is not ctrl+i", "\t", false},
		{"ctrl+i control byte", "\x09", false},
		{"other key", "a", false},
		{"escape", "\x1b", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := isCtrlI(tc.data); got != tc.want {
			t.Errorf("%s: isCtrlI(%q) = %v, want %v", tc.name, tc.data, got, tc.want)
		}
	}
	// Ctrl+I itself (0x09) is the same byte as tab in a terminal, and the TS
	// source excludes it explicitly; matchesKey agrees.
	if !matchesKey("\x09", "ctrl+i") {
		t.Error("matchesKey should recognise the 0x09 byte as ctrl+i")
	}
	// A Kitty key-release report carries the `:3` event-type field, so the
	// Alt modifier form `\x1b[1;3u` is a press (and must keep reaching Ctrl+I),
	// while `\x1b[105;7:3u` (Ctrl+Alt+I released) is a release.
	if !isKeyRelease("\x1b[105;7:3u") {
		t.Error("kitty release sequence should be a key release")
	}
	if isKeyRelease("\x1b[1;3u") {
		t.Error("the Alt modifier form is a press, not a release")
	}
	if isKeyRelease("a") {
		t.Error("plain text is not a key release")
	}
}

// Test configPathFor: the PIG_HOME isolation red line (task §2.3).
func TestConfigPathFor(t *testing.T) {
	home := func() (string, error) { return "/home/user", nil }
	cases := []struct {
		name string
		pig  string
		want string
	}{
		{"PIG_HOME set", "/root/.pig-test", "/root/.pig-test/agent/compact-ui.json"},
		{"PIG_HOME empty falls back to ~/.pi", "", "/home/user/.pi/agent/compact-ui.json"},
	}
	for _, tc := range cases {
		env := func(key string) string {
			if key == "PIG_HOME" {
				return tc.pig
			}
			return ""
		}
		if got := configPathFor(env, home); got != tc.want {
			t.Errorf("%s: configPathFor = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A missing home directory still yields the conventional path.
	if got := configPathFor(func(string) string { return "" }, func() (string, error) { return "", errors.New("no home") }); got != filepath.Join("", ".pi", "agent", "compact-ui.json") {
		t.Errorf("erroring home = %q", got)
	}
}

// Test loadConfig / saveConfig (TS L791-807).
func TestLoadConfigAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compact-ui.json")

	// A missing file yields the default.
	if got := loadConfig(path, os.ReadFile); got.MaxGroupEntries != defaultMaxGroupEntries {
		t.Errorf("missing file = %+v, want default", got)
	}
	// A real write round-trips.
	if err := writeFile(path, marshalConfigJSON(compactConfig{MaxGroupEntries: 9})); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if got := loadConfig(path, os.ReadFile); got.MaxGroupEntries != 9 {
		t.Errorf("round trip = %+v, want 9", got)
	}
	// saveConfig writes the 2-space indented form plus a trailing newline.
	other := filepath.Join(dir, "other.json")
	saveConfig(compactConfig{MaxGroupEntries: 12}, other, writeFile)
	raw, err := os.ReadFile(other)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	want := "{\n  \"maxGroupEntries\": 12\n}\n"
	if string(raw) != want {
		t.Errorf("saved config = %q, want %q", raw, want)
	}
}

// Test loadConfigDegradesSilently: every malformed input yields defaults.
func TestLoadConfigDegradesSilently(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"not json", "nope"},
		{"json array", "[]"},
		{"missing key", `{"other":1}`},
		{"null key", `{"maxGroupEntries":null}`},
		{"string value", `{"maxGroupEntries":"7"}`},
	}
	for _, tc := range cases {
		got := loadConfig("ignored", func(string) ([]byte, error) { return []byte(tc.raw), nil })
		if got.MaxGroupEntries != defaultMaxGroupEntries {
			t.Errorf("%s: %+v, want default", tc.name, got)
		}
	}
	// A read error and a nil reader both degrade.
	if got := loadConfig("ignored", func(string) ([]byte, error) { return nil, errors.New("boom") }); got.MaxGroupEntries != defaultMaxGroupEntries {
		t.Errorf("read error = %+v", got)
	}
	if got := loadConfig("ignored", nil); got.MaxGroupEntries != defaultMaxGroupEntries {
		t.Errorf("nil reader = %+v", got)
	}
	// A write error is swallowed.
	saveConfig(compactConfig{MaxGroupEntries: 3}, "ignored", func(string, []byte) error { return errors.New("nope") })
	saveConfig(compactConfig{MaxGroupEntries: 3}, "ignored", nil)
}

// Test thinkingSpinnerFrame / toolSpinnerFrame (TS L756-762, L1006).
func TestSpinnerFrames(t *testing.T) {
	cases := []struct {
		nowMs int64
		want  string
		color string
	}{
		{0, "·", "dim"},
		{139, "·", "dim"},
		{140, "•", "muted"},
		{280, "●", "accent"},
		{420, "•", "muted"},
		{560, "·", "dim"},
	}
	for _, tc := range cases {
		got := thinkingSpinnerFrame(tc.nowMs, 0)
		if got.frame != tc.want || got.color != tc.color {
			t.Errorf("thinkingSpinnerFrame(%d) = %+v, want %s/%s", tc.nowMs, got, tc.want, tc.color)
		}
	}
	// A clock before the start wraps instead of panicking on a negative index.
	if got := thinkingSpinnerFrame(-500, 0); got.frame == "" {
		t.Error("negative elapsed should still yield a frame")
	}
	if got := toolSpinnerFrame(0, 0); got != "⠋" {
		t.Errorf("toolSpinnerFrame(0) = %q, want ⠋", got)
	}
	if got := toolSpinnerFrame(100, 0); got != "⠙" {
		t.Errorf("toolSpinnerFrame(100) = %q, want ⠙", got)
	}
	if got := toolSpinnerFrame(1000, 0); got != "⠋" {
		t.Errorf("toolSpinnerFrame wraps = %q, want ⠋", got)
	}
	if len(spinnerFrames) != 10 {
		t.Errorf("SPINNER has %d frames, want 10", len(spinnerFrames))
	}
	if spinnerMs != 100 || thinkingSpinnerIntervalMs != 140 {
		t.Errorf("intervals = %d / %d", spinnerMs, thinkingSpinnerIntervalMs)
	}
	if groupPaddingX != 1 || groupPaddingRight != 2 {
		t.Errorf("paddings = %d / %d", groupPaddingX, groupPaddingRight)
	}
	if disableMouseTrackingSequence != "\x1b[?1006l\x1b[?1004l\x1b[?1002l\x1b[?1000l" {
		t.Errorf("mouse sequence = %q", disableMouseTrackingSequence)
	}
}

// Test groupThousands is toLocaleString (`12,345`).
func TestGroupThousands(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{12, "12"},
		{123, "123"},
		{1234, "1,234"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, tc := range cases {
		if got := groupThousands(tc.in); got != tc.want {
			t.Errorf("groupThousands(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Test guards (TS L1072-1111).
func TestComponentGuards(t *testing.T) {
	r := testRuntime(t)
	var c component = newContainer(r)
	if !isContainer(c) || isAssistantMessage(c) || isToolGroup(c) || isMarkdown(c) || isText(c) || isSpacer(c) || isToolExecution(c) {
		t.Error("container guards disagree")
	}
	if !isAssistantMessage(&assistantMessageComponent{}) {
		t.Error("assistant guard")
	}
	if !isToolGroup(r.newToolGroup(0)) {
		t.Error("group guard")
	}
	if !isToolExecution(newTestTool("a", "b", nil)) {
		t.Error("tool guard")
	}
	if !isSpacer(newSpacer(1)) {
		t.Error("spacer guard")
	}
	if !isMarkdown(&markdownComponent{}) {
		t.Error("markdown guard")
	}
	if !isText(&textComponent{}) {
		t.Error("text guard")
	}
	// isVisibleTextMarkdown excludes the default-styled placeholder.
	if isVisibleTextMarkdown(&markdownComponent{defaultTextStyle: true}) {
		t.Error("default-styled markdown is not visible text")
	}
	if !isVisibleTextMarkdown(&markdownComponent{text: "hi"}) {
		t.Error("plain markdown is visible text")
	}
	if isVisibleTextMarkdown(nil) {
		t.Error("nil is not visible text markdown")
	}
}

// Test isNativeThinkingWidget (TS L518-534).
func TestIsNativeThinkingWidget(t *testing.T) {
	r := testRuntime(t)
	cases := []struct {
		name string
		comp component
		want bool
	}{
		{"default-styled markdown", &markdownComponent{defaultTextStyle: true}, true},
		{"visible markdown", &markdownComponent{text: "x"}, false},
		{"empty text", &textComponent{text: ""}, true},
		{"whitespace text", &textComponent{text: "   "}, true},
		{"Thinking...", &textComponent{text: "Thinking..."}, true},
		{"thinking lower", &textComponent{text: "thinking"}, true},
		{"thinking ellipsis", &textComponent{text: "\x1b[2mthinking...\x1b[0m"}, true},
		{"real text", &textComponent{text: "here is the answer"}, false},
		{"tool group", r.newToolGroup(0), false},
		{"container", newContainer(r), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := r.isNativeThinkingWidget(tc.comp); got != tc.want {
			t.Errorf("%s: isNativeThinkingWidget = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Test extractCacheMissNotice (TS L1373-1399).
func TestExtractCacheMissNotice(t *testing.T) {
	cases := []struct {
		name string
		comp component
		want string
		ok   bool
	}{
		{
			"plain notice",
			&textComponent{text: "cache miss: 1234 tokens re-billed"},
			"cache miss: 1234 tokens re-billed",
			true,
		},
		{
			"with after clause",
			&textComponent{text: "cache miss after model switch: 999 tokens re-billed"},
			"cache miss after model switch: 999 tokens re-billed",
			true,
		},
		{
			"case insensitive",
			&markdownComponent{text: "CACHE MISS: 12 tokens re-billed"},
			"CACHE MISS: 12 tokens re-billed",
			true,
		},
		{
			"escape sequences stripped",
			&textComponent{text: "\x1b[33mcache miss: 5 tokens re-billed\x1b[0m"},
			"cache miss: 5 tokens re-billed",
			true,
		},
		{"unrelated text", &textComponent{text: "compacted"}, "", false},
		{"prefix only", &textComponent{text: "cache miss"}, "", false},
		{"no re-billed", &textComponent{text: "cache miss: 5 tokens"}, "", false},
		{"empty", &textComponent{text: ""}, "", false},
		{"container", nil, "", false},
	}
	for _, tc := range cases {
		got, ok := extractCacheMissNotice(tc.comp)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	// isCacheMissNotice accepts the `after <word>` form and rejects a colon-less
	// after clause.
	if !isCacheMissNotice("cache miss after switch: 1 tokens re-billed") {
		t.Error("after clause should match")
	}
	if isCacheMissNotice("cache miss after switch 1 tokens re-billed") {
		t.Error("missing colon should not match")
	}
	if isCacheMissNotice("cache misse: 1 tokens re-billed") {
		t.Error("wrong prefix should not match")
	}
}

// Test noticeContent: the shared cleanup of L963 / L1723 / L2005.
func TestNoticeContent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cache miss: 1234 tokens re-billed", "1234 tokens re-billed"},
		{"cache miss  : 5 tokens  re-billed:", "5 tokens  re-billed"},
		{"unrelated: x", "unrelated x"},
		{"cache miss", ""},
	}
	for _, tc := range cases {
		if got := noticeContent(tc.in); got != tc.want {
			t.Errorf("noticeContent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// nan and inf build the non-finite test values from math, which needs a
// runtime call: a literal 0/0 is a compile error in Go.
func nan() float64 { return math.NaN() }

func inf() float64 { return math.Inf(1) }

// intPtrValue is the address-taking helper the event tests use.
func intPtrValue(value int) *int { return &value }
