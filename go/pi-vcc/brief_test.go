package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/core/brief.ts: heredoc detection, bash compression, brief-line
// grouping/collapsing/capping and tool one-liners.

// ── heredoc detection ───────────────────────────────────────────────────────

func TestHeredocCloseIndex(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		i     int
		want  int
	}{
		{"terminator found", []string{"cat > f <<EOF", "body", "EOF"}, 0, 2},
		{"dash heredoc", []string{"python3 - <<-PY", "body", "PY"}, 0, 2},
		{"quoted terminator", []string{"cat <<'EOF'", "x", "EOF"}, 0, 2},
		{"no terminator downstream", []string{"echo 8 << 20"}, 0, -1},
		{"numeric shift is not a heredoc", []string{"x=$((8 << 10))", "y"}, 0, -1},
		{"no opener", []string{"git status"}, 0, -1},
		{"terminator must match exactly", []string{"cat <<EOF", " EOFTWO", "EOF"}, 0, 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := HeredocCloseIndex(test.lines, test.i); got != test.want {
				t.Fatalf("HeredocCloseIndex = %d, want %d", got, test.want)
			}
		})
	}
}

// TestHeredocCloseIndex_ShiftOperatorNotHeredoc pins the lookbehind guard: `Rd<<8`
// and `x << 10` are shift operations, not heredocs.
func TestHeredocCloseIndex_ShiftOperatorNotHeredoc(t *testing.T) {
	for _, line := range []string{"a=Rd<<8", "n=$((x << 10))", "f(<<)"} {
		if got := HeredocCloseIndex([]string{line, "later"}, 0); got != -1 {
			t.Fatalf("HeredocCloseIndex(%q) = %d, want -1", line, got)
		}
	}
}

// ── bash compression ────────────────────────────────────────────────────────

func TestCompressBash(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single command unchanged", "git status", "git status"},
		{"pipe tail stripped", "git status --porcelain | head -5", "git status --porcelain"},
		{"nested pipe tail stripped", "git log | grep x | wc -l", "git log | grep x"},
		{"cd prefix stripped", "cd /root/x && git diff --stat", "git diff --stat"},
		{"set -euo pipefail dropped", "set -euo pipefail\ngit commit -m x", "git commit -m x"},
		{"export dropped", "export FOO=1\nmake test", "make test"},
		{"comments dropped", "# a comment\ngo test ./...", "go test ./..."},
		{"only trivial lines falls back to the first", "set -e", "set -e"},
		{"multiple commands joined", "git add .\ngit commit -m x\ngit push", "git add .; git commit -m x; git push"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CompressBash(test.in); got != test.want {
				t.Fatalf("CompressBash(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// TestCompressBash_HeredocBodies pins the heredoc rules: a file-writer heredoc
// keeps its opener only, every other heredoc surfaces a one-line body preview.
func TestCompressBash_HeredocBodies(t *testing.T) {
	fileWriter := "cat > notes.md <<EOF\nlots of prose\nmore prose\nEOF"
	if got := CompressBash(fileWriter); strings.Contains(got, "lots of prose") {
		t.Fatalf("a file-writer heredoc kept its body: %q", got)
	}

	interpreter := "python3 - <<PY\nimport os\nprint(1)\nPY"
	got := CompressBash(interpreter)
	if !strings.Contains(got, "print(1)") {
		t.Fatalf("an interpreter heredoc lost its preview: %q", got)
	}
	if strings.Contains(got, "import os") {
		t.Fatalf("a boilerplate body line became the preview: %q", got)
	}

	sshHeredoc := "ssh host <<CMD\ndocker ps\nCMD"
	if got := CompressBash(sshHeredoc); !strings.Contains(got, "docker ps") {
		t.Fatalf("an ssh heredoc lost its preview: %q", got)
	}
	// The terminator must not survive as a stray line.
	if got := CompressBash(sshHeredoc); strings.Contains(got, "; CMD") || strings.HasSuffix(got, "CMD") {
		t.Fatalf("the heredoc terminator leaked into the command: %q", got)
	}
}

func TestCompressBash_CapsLongCommands(t *testing.T) {
	long := "echo " + strings.Repeat("x", 400)
	got := CompressBash(long)
	if jsLen(got) > bashCap {
		t.Fatalf("CompressBash produced %d units, over the %d cap", jsLen(got), bashCap)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("a capped command should end in an ellipsis: %q", jsSlice(got, maxInt(0, jsLen(got)-10), jsLen(got)))
	}
}

// ── brief sections ──────────────────────────────────────────────────────────

func TestBuildBriefSections_UserAndAssistant(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindUser, Text: " please fix the auth bug ", SourceIndex: 0, HasSource: true},
		{Kind: KindAssistant, Text: "On it.", SourceIndex: 1, HasSource: true},
	}
	sections := BuildBriefSections(blocks)
	if len(sections) != 2 {
		t.Fatalf("sections = %#v, want 2", sections)
	}
	if sections[0].Header != "[user]" || sections[0].Lines[0] != "please fix the auth bug (#0)" {
		t.Fatalf("user section = %#v", sections[0])
	}
	if sections[1].Header != "[assistant]" || sections[1].Lines[0] != "On it. (#1)" {
		t.Fatalf("assistant section = %#v", sections[1])
	}
}

// TestBuildBriefSections_NoRefWithoutIndex pins the fail-closed rule: a block
// with no source index renders no `(#N)`.
func TestBuildBriefSections_NoRefWithoutIndex(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{{Kind: KindUser, Text: "hello"}})
	if jsSplitLines(sections[0].Lines[0])[0] != "hello" {
		t.Fatalf("a ref-less block rendered a ref: %#v", sections[0])
	}
}

func TestBuildBriefSections_BashLine(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{
		{Kind: KindBash, Command: "git status --porcelain | head -5", SourceIndex: 2, HasSource: true},
	})
	if got := sections[0].Lines[0]; got != "$ git status --porcelain (#2)" {
		t.Fatalf("bash line = %q", got)
	}
	if sections[0].Header != "[user]" {
		t.Fatalf("a bash block should render under [user], got %q", sections[0].Header)
	}
}

func TestBuildBriefSections_SkipsEmptyToolName(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{
		{Kind: KindToolCall, Name: "", Args: map[string]any{}},
		{Kind: KindToolCall, Name: "   ", Args: map[string]any{}},
	})
	if len(sections) != 0 {
		t.Fatalf("malformed tool calls rendered: %#v", sections)
	}
}

func TestBuildBriefSections_ToolResultsOmitted(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{
		{Kind: KindToolResult, Name: "bash", Text: "huge output\nmore"},
	})
	if len(sections) != 0 {
		t.Fatalf("tool results should be omitted from the brief: %#v", sections)
	}
}

func TestBuildBriefSections_StripsSelfTalk(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{
		{Kind: KindAssistant, Text: "Hmm, actually, the bug is in the token refresh."},
	})
	line := sections[0].Lines[0]
	if strings.HasPrefix(line, "Hmm") || strings.HasPrefix(line, "actually") {
		t.Fatalf("self-talk prefix survived: %q", line)
	}
	if !strings.Contains(line, "the bug is in the token refresh.") {
		t.Fatalf("self-talk stripping removed content: %q", line)
	}
}

// TestBuildBriefSections_CollapsesIdenticalToolLines pins the `(#a, #b) xN`
// collapse of consecutive identical tool lines.
func TestBuildBriefSections_CollapsesIdenticalToolLines(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}, SourceIndex: 12, HasSource: true},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}, SourceIndex: 16, HasSource: true},
	})
	lines := sections[0].Lines
	if len(lines) != 1 {
		t.Fatalf("expected one collapsed line, got %#v", lines)
	}
	if lines[0] != `* bash "bun test" (#12, #16) x2` {
		t.Fatalf("collapsed line = %q", lines[0])
	}

	// A third identical call extends the group.
	sections = BuildBriefSections([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}, SourceIndex: 12, HasSource: true},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}, SourceIndex: 16, HasSource: true},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}, SourceIndex: 20, HasSource: true},
	})
	if got := sections[0].Lines[0]; got != `* bash "bun test" (#12, #16, #20) x3` {
		t.Fatalf("three-call collapse = %q", got)
	}
}

// TestBuildBriefSections_ReflessLineNeverJoinsACollapseGroup pins the documented
// rule: merging a ref-less line would emit a malformed "(#12, #) x2".
func TestBuildBriefSections_ReflessLineNeverJoinsACollapseGroup(t *testing.T) {
	sections := BuildBriefSections([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}, SourceIndex: 12, HasSource: true},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test"}},
	})
	lines := sections[0].Lines
	if len(lines) != 2 {
		t.Fatalf("a ref-less line joined the group: %#v", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, ", #)") || strings.Contains(line, "x2") {
			t.Fatalf("malformed collapse output: %q", line)
		}
	}
}

// TestBuildBriefSections_CapsToolCallsPerTurn pins the eight-call tail cap and
// the omission marker placed just before the first kept call.
func TestBuildBriefSections_CapsToolCallsPerTurn(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 12)
	for i := 0; i < 12; i++ {
		blocks = append(blocks, NormalizedBlock{
			Kind: KindToolCall, Name: "read",
			Args:        map[string]any{"path": "/f" + jsNumberString(float64(i)) + ".go"},
			SourceIndex: i, HasSource: true,
		})
	}
	lines := BuildBriefSections(blocks)[0].Lines
	tools := 0
	marker := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "* (") && strings.Contains(line, "earlier tool-call entries omitted)") {
			marker = line
			continue
		}
		if strings.HasPrefix(line, "* ") {
			tools++
		}
	}
	if tools != 8 {
		t.Fatalf("kept %d tool lines, want 8: %#v", tools, lines)
	}
	if marker != "* (4 earlier tool-call entries omitted)" {
		t.Fatalf("omission marker = %q", marker)
	}
	// The cap keeps the NEWEST calls.
	if !strings.Contains(strings.Join(lines, "\n"), "/f11.go") {
		t.Fatal("the cap dropped the newest call")
	}
	if strings.Contains(strings.Join(lines, "\n"), "/f0.go") {
		t.Fatal("the cap kept the oldest call")
	}
}

// ── tool one-liners ─────────────────────────────────────────────────────────

func TestToolOneLiner(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"Read uses file_path", "Read", map[string]any{"file_path": "/a.go"}, `* Read "/a.go"`},
		{"Glob uses pattern", "Glob", map[string]any{"pattern": "**/*.go"}, `* Glob "**/*.go"`},
		{"Grep uses pattern", "Grep", map[string]any{"pattern": "func main"}, `* Grep "func main"`},
		{"lowercase read uses file_path", "read", map[string]any{"file_path": "/a.go"}, `* read "/a.go"`},
		{"path fallback", "weird", map[string]any{"path": "/x/y"}, `* weird "/x/y"`},
		{"bash compresses its command", "bash", map[string]any{"command": "git status | head -3"}, `* bash "git status"`},
		{"bash falls back to description", "bash", map[string]any{"description": "list files"}, `* bash "list files"`},
		{"query is clipped to 60", "search", map[string]any{"query": strings.Repeat("q", 100)}, `* search "` + strings.Repeat("q", 60) + `"`},
		{"bare name last", "mystery", map[string]any{}, "* mystery"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := toolOneLiner(test.tool, test.args); got != test.want {
				t.Fatalf("toolOneLiner(%q, %v) = %q, want %q", test.tool, test.args, got, test.want)
			}
		})
	}
}

// ── stringify ───────────────────────────────────────────────────────────────

func TestStringifyBrief_SuppressesBlankBetweenToolSections(t *testing.T) {
	sections := []BriefLine{
		{Header: "[user]", Lines: []string{"hi"}},
		{Header: "[assistant]", Lines: []string{`* bash "a"`, `* bash "b"`}},
		{Header: "[assistant]", Lines: []string{`* bash "c"`}},
	}
	got := StringifyBrief(sections)
	if strings.Contains(got, "* bash \"b\"\n\n* bash \"c\"") {
		t.Fatalf("a blank line separated consecutive tool sections:\n%s", got)
	}
	if !strings.Contains(got, "hi\n\n[assistant]") {
		t.Fatalf("a blank line is missing before the assistant section:\n%s", got)
	}
}

// ── token budgeting ─────────────────────────────────────────────────────────

func TestTruncateTokens(t *testing.T) {
	// Stop words do not count toward the budget.
	text := "the of in to fix the auth bug in the login flow"
	got := truncateTokens(text, 3)
	if !strings.Contains(got, "fix") || !strings.Contains(got, "auth") || !strings.Contains(got, "bug") {
		t.Fatalf("truncateTokens cut too early: %q", got)
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("truncateTokens did not mark the truncation: %q", got)
	}
	// Under budget: unchanged apart from whitespace normalization.
	if got := truncateTokens("fix auth", 10); got != "fix auth" {
		t.Fatalf("truncateTokens changed short text: %q", got)
	}
}

func TestTruncateTokensHeadTail(t *testing.T) {
	words := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		words = append(words, "w"+jsNumberString(float64(i)))
	}
	text := strings.Join(words, " ")
	got := truncateTokensHeadTail(text, 5, 5)
	if !strings.Contains(got, "...(middle truncated)...") {
		t.Fatalf("no middle marker: %q", got)
	}
	if !strings.HasPrefix(got, "w0 w1 w2 w3 w4") {
		t.Fatalf("the head is wrong: %q", jsSlice(got, 0, 40))
	}
	if !strings.HasSuffix(got, "w59") {
		t.Fatalf("the tail is wrong: %q", got)
	}
	// Under budget: no marker.
	if got := truncateTokensHeadTail("alpha beta", 5, 5); strings.Contains(got, "truncated") {
		t.Fatalf("short text was marked truncated: %q", got)
	}
}

func TestNormalizeForTokenBudget(t *testing.T) {
	got := normalizeForTokenBudget("  a\r\n\r\n\r\nb\t\tc  ")
	if got != "a\n\nb c" {
		t.Fatalf("normalizeForTokenBudget = %q", got)
	}
}
