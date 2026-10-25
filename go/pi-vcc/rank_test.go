package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/core/rank.ts: block scoring, adjacency boosts, dedup keys and
// the size-relative selection.

func TestScoreBlock_BasicWeights(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindUser, Text: "please fix it"},
		{Kind: KindAssistant, Text: "working"},
		{Kind: KindToolResult, Name: "bash", Text: "output"},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if ranked[0].Score < 18 {
		t.Fatalf("a user block should score at least 18, got %d (%v)", ranked[0].Score, ranked[0].Reasons)
	}
	if !containsStr(ranked[0].Reasons, "user-turn") {
		t.Fatalf("user reasons = %v", ranked[0].Reasons)
	}
	if !containsStr(ranked[1].Reasons, "assistant-context") {
		t.Fatalf("assistant reasons = %v", ranked[1].Reasons)
	}
	if !containsStr(ranked[2].Reasons, "tool-result-low-value") {
		t.Fatalf("tool result reasons = %v", ranked[2].Reasons)
	}
}

func TestScoreBlock_EditToolOutranksReadTool(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/a.go"}},
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/a.go"}},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if ranked[0].Score <= ranked[1].Score {
		t.Fatalf("edit (%d) should outrank read (%d)", ranked[0].Score, ranked[1].Score)
	}
	if !containsStr(ranked[0].Reasons, "edit-tool") || !containsStr(ranked[1].Reasons, "read-tool") {
		t.Fatalf("reasons = %v / %v", ranked[0].Reasons, ranked[1].Reasons)
	}
}

func TestScoreBlock_TestCommandBoosted(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "bun test tests/auth.test.ts"}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "ls"}},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "test-command") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
	if ranked[0].Score <= ranked[1].Score {
		t.Fatalf("a test command (%d) should outrank a trivial one (%d)", ranked[0].Score, ranked[1].Score)
	}
}

func TestScoreBlock_WorkflowCommandBoosted(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "git commit -m x"}},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "workflow-command") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
}

func TestScoreBlock_NonzeroExitBoosted(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindBash, Command: "ls", ExitCode: 2, HasExit: true},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "nonzero-exit") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
	// A failed command is NOT also penalized as scaffolding.
	if containsStr(ranked[0].Reasons, "trivial-bash") {
		t.Fatalf("a failed scaffolding command was penalized: %v", ranked[0].Reasons)
	}
}

func TestScoreBlock_TrivialBashPenalized(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindBash, Command: "cd /tmp", ExitCode: 0, HasExit: true},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "trivial-bash") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
}

func TestScoreBlock_HookFileSignals(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/a.go"}},
	}
	withOps := RankBriefBlocks(blocks, BriefRankingOptions{
		FileOps: &FileOps{ModifiedFiles: []string{"/a.go"}},
	})
	without := RankBriefBlocks(blocks, BriefRankingOptions{})
	if withOps[0].Score <= without[0].Score {
		t.Fatalf("a hook-modified file should add score (%d vs %d)", withOps[0].Score, without[0].Score)
	}
	if !containsStr(withOps[0].Reasons, "hook-modified-file") {
		t.Fatalf("reasons = %v", withOps[0].Reasons)
	}
}

func TestScoreBlock_LongToolResultPenalized(t *testing.T) {
	long := strings.Repeat("x", 1500)
	blocks := []NormalizedBlock{{Kind: KindToolResult, Name: "bash", Text: long}}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "long-tool-result") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
}

// TestBoostAdjacency pins the neighbourhood boosts around an edit.
func TestBoostAdjacency(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindUser, Text: "do the thing"},
		{Kind: KindAssistant, Text: "on it"},
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/a.go"}},
		{Kind: KindAssistant, Text: "done"},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "near-important-event") {
		t.Fatalf("the user turn before an edit was not boosted: %v", ranked[0].Reasons)
	}
	if !containsStr(ranked[3].Reasons, "after-important-event") {
		t.Fatalf("the assistant turn after an edit was not boosted: %v", ranked[3].Reasons)
	}
}

// TestBoostSegmentClosingAssistants pins the 120-char segment-closing rule.
func TestBoostSegmentClosingAssistants(t *testing.T) {
	longText := strings.Repeat("this is a substantive conclusion. ", 6)
	blocks := []NormalizedBlock{
		{Kind: KindAssistant, Text: longText},
		{Kind: KindUser, Text: "next"},
	}
	ranked := RankBriefBlocks(blocks, BriefRankingOptions{})
	if !containsStr(ranked[0].Reasons, "segment-closing-assistant") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}

	// A short assistant block is not a segment close.
	short := RankBriefBlocks([]NormalizedBlock{
		{Kind: KindAssistant, Text: "ok"},
		{Kind: KindUser, Text: "next"},
	}, BriefRankingOptions{})
	if containsStr(short[0].Reasons, "segment-closing-assistant") {
		t.Fatalf("a short block was treated as a segment close: %v", short[0].Reasons)
	}
}

func TestDedupKey_GhPrPoll(t *testing.T) {
	key, ok := dedupKey(NormalizedBlock{
		Kind: KindToolCall, Name: "bash",
		Args: map[string]any{"command": "gh pr view 123"},
	})
	if !ok || key != "gh-pr-poll:123" {
		t.Fatalf("dedupKey = (%q, %v)", key, ok)
	}
	// `gh pr checks 123` collapses with `gh pr view 123`.
	key2, _ := dedupKey(NormalizedBlock{
		Kind: KindToolCall, Name: "bash",
		Args: map[string]any{"command": "gh pr checks 123"},
	})
	if key != key2 {
		t.Fatalf("gh pr view and gh pr checks should share a key: %q vs %q", key, key2)
	}
}

func TestDedupKey_NormalizesWhitespace(t *testing.T) {
	a, _ := dedupKey(NormalizedBlock{Kind: KindBash, Command: "git   status"})
	b, _ := dedupKey(NormalizedBlock{Kind: KindBash, Command: "git status"})
	if a != b {
		t.Fatalf("whitespace-normalized keys differ: %q vs %q", a, b)
	}
}

// TestSelectRankedBriefBlocks_FastPath pins the no-op path: everything fits and
// there is no budget, so the input comes back unchanged.
func TestSelectRankedBriefBlocks_FastPath(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindUser, Text: "a"},
		{Kind: KindAssistant, Text: "b"},
	}
	got := SelectRankedBriefBlocks(blocks, BriefRankingOptions{})
	if len(got) != 2 {
		t.Fatalf("fast path returned %d blocks, want 2", len(got))
	}
}

// TestSelectRankedBriefBlocks_RespectsMaxBlocks pins the block cap and that the
// most recent blocks survive.
func TestSelectRankedBriefBlocks_RespectsMaxBlocks(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 40)
	for i := 0; i < 40; i++ {
		blocks = append(blocks, NormalizedBlock{
			Kind: KindAssistant, Text: "turn " + jsNumberString(float64(i)),
			SourceIndex: i, HasSource: true,
		})
	}
	got := SelectRankedBriefBlocks(blocks, BriefRankingOptions{MaxBlocks: 10, PreserveRecentBlocks: 4})
	if len(got) > 10 {
		t.Fatalf("selected %d blocks, over the 10-block cap", len(got))
	}
	if len(got) < 4 {
		t.Fatalf("selected %d blocks, under the preserve-recent floor", len(got))
	}
	// Order is by original index, not by score.
	for i := 1; i < len(got); i++ {
		if got[i-1].SourceIndex >= got[i].SourceIndex {
			t.Fatalf("output is not in transcript order at %d", i)
		}
	}
}

// TestSelectRankedBriefBlocks_CharBudget pins the char budget: the selection
// stops before exceeding it and keeps the newest blocks.
func TestSelectRankedBriefBlocks_CharBudget(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 60)
	for i := 0; i < 60; i++ {
		blocks = append(blocks, NormalizedBlock{
			Kind: KindAssistant, Text: "paragraph " + strings.Repeat("x", 200),
			SourceIndex: i, HasSource: true,
		})
	}
	got := SelectRankedBriefBlocks(blocks, BriefRankingOptions{
		MaxBlocks:            80,
		MaxBriefChars:        600,
		MaxBriefCharsCeiling: 600,
		BriefCharsPerBlock:   1,
	})
	if len(got) == 0 {
		t.Fatal("the budget excluded everything")
	}
	total := 0
	for _, block := range got {
		total += jsLen(CompileBrief([]NormalizedBlock{block})) + 1
	}
	if total > 600 {
		t.Fatalf("the selection totals %d chars, over the 600 budget", total)
	}
	if len(got) >= 60 {
		t.Fatalf("the budget did not exclude anything (%d blocks)", len(got))
	}
}

// TestSelectRankedBriefBlocks_SizeRelativeBudget pins the clamp between floor and
// ceiling.
func TestSelectRankedBriefBlocks_SizeRelativeBudget(t *testing.T) {
	small := make([]NormalizedBlock, 0, 10)
	for i := 0; i < 10; i++ {
		small = append(small, NormalizedBlock{Kind: KindAssistant, Text: "x"})
	}
	large := make([]NormalizedBlock, 0, 500)
	for i := 0; i < 500; i++ {
		large = append(large, NormalizedBlock{Kind: KindAssistant, Text: "x"})
	}
	opts := BriefRankingOptions{
		MaxBlocks:            1000,
		MaxBriefChars:        100,
		MaxBriefCharsCeiling: 400,
		BriefCharsPerBlock:   1,
	}
	// A small transcript stays at the floor: clamp(1*10, 100, 400) = 100 chars,
	// which holds several blocks but not all ten, so the budget binds.
	smallSelection := SelectRankedBriefBlocks(small, opts)
	if len(smallSelection) >= 10 {
		t.Fatalf("the floor budget did not bind: selected all %d blocks", len(smallSelection))
	}
	if len(smallSelection) == 0 {
		t.Fatal("the floor budget excluded everything")
	}
	// A large transcript earns the ceiling, not more.
	got := SelectRankedBriefBlocks(large, opts)
	if len(got) >= 500 {
		t.Fatalf("the ceiling did not bound the large selection (%d blocks)", len(got))
	}
}

// TestSelectRankedBriefBlocks_DedupesRepeats pins the dedup key: a repeated
// identical command does not occupy two brief slots once the cap applies.
func TestSelectRankedBriefBlocks_DedupesRepeats(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindAssistant, Text: "a"},
		{Kind: KindBash, Command: "bun test", SourceIndex: 1, HasSource: true},
		{Kind: KindAssistant, Text: "b"},
		{Kind: KindBash, Command: "bun test", SourceIndex: 3, HasSource: true},
		{Kind: KindAssistant, Text: "c"},
	}
	got := SelectRankedBriefBlocks(blocks, BriefRankingOptions{MaxBlocks: 3, PreserveRecentBlocks: 1})
	seen := 0
	for _, block := range got {
		if block.Kind == KindBash && block.Command == "bun test" {
			seen++
		}
	}
	if seen > 1 {
		t.Fatalf("the same command was selected %d times: %#v", seen, got)
	}
}

func TestIsTrivialOnlyBash(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"ls", true},
		{"cd /tmp", true},
		{"echo hi", true},
		{"set -euo pipefail", true},
		{"# a comment", true},
		{"git status", false},
		{"set -e\ngit status", false},
		{"cat > f <<EOF\nbody\nEOF", false},
	}
	for _, test := range tests {
		if got := isTrivialOnlyBash(test.command); got != test.want {
			t.Fatalf("isTrivialOnlyBash(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}
