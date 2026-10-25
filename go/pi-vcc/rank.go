package pivcc

import (
	"sort"
	"strings"
)

// Ports src/core/rank.ts: score and select the blocks that make the brief.

// BriefRankingOptions is rank.ts BriefRankingOptions.
type BriefRankingOptions struct {
	// MaxBlocks caps the normalized blocks used to build the brief transcript.
	MaxBlocks int
	// PreserveRecentBlocks always keeps this many latest blocks.
	PreserveRecentBlocks int
	// FileOps is hook-provided file activity used as structural signal.
	FileOps *FileOps
	// MaxBriefChars is the primary size budget (floor of the size-relative
	// budget when the ceiling and slope are also set).
	MaxBriefChars int
	// MaxBriefCharsCeiling is the size-relative budget's hard ceiling.
	MaxBriefCharsCeiling int
	// BriefCharsPerBlock is the per-block slope of the size-relative budget.
	BriefCharsPerBlock int
}

const (
	defaultMaxBlocks     = 80
	defaultRecentBlocks  = 16
	minSegmentClosingLen = 120
	trivialBashPenalty   = 16
)

// RankedBlock is rank.ts RankedBlock.
type RankedBlock struct {
	Block   NormalizedBlock
	Index   int
	Score   int
	Reasons []string
}

var (
	editToolRe = compileIgnoreCase(`^(edit|write|multiedit|quick_edit|target_edit|apply_patch)$`)
	readToolRe = compileIgnoreCase(`^(read|glob|grep|ls|find|semantic_query|semantic_grep|semantic_show)$`)

	// testCommandRe is rank.ts TEST_COMMAND_RE.
	testCommandRe = compileIgnoreCase(`\b(?:bun|npm|pnpm|yarn|node|pytest|cargo|go|mvn|gradle)\b[^\n]*(?:test|spec|check|lint|build|tsc)`)

	// ghPrPollRe is rank.ts GH_PR_POLL_RE.
	ghPrPollRe = compileIgnoreCase(`(?:^|` + jsSpace + `)gh` + jsSpace + `+pr` + jsSpace + `+(?:view|checks)` +
		jsSpace + `+(\d+)\b`)

	// workflowCommandRe is rank.ts WORKFLOW_COMMAND_RE.
	workflowCommandRe = compileIgnoreCase(`(?:^|` + jsSpace + `)(?:gh` + jsSpace + `+(?:pr|issue)` + jsSpace +
		`+[a-z-]+|git` + jsSpace + `+(?:commit|push|merge|rebase|revert|cherry-pick|tag|reset|checkout|branch)\b)`)

	// trivialBashLineRe is rank.ts TRIVIAL_BASH_LINE_RE.
	trivialBashLineRe = mustCompile(`^(?:set` + jsSpace + `+[-+]|cd(?:` + jsSpace + `+\S+)?$|export` + jsSpace +
		`+\w+=|(?:source|\.)` + jsSpace + `+\S+|pwd$|true$|:$|#|ls(?:` + jsSpace + `|$)|echo\b|clear$|sleep\b)`)

	// bashPathRe is rank.ts pathFromBlock's
	// /(?:^|\s)([\w./-]+\.[\w-]+)(?:\s|$)/.
	bashPathRe = mustCompile(`(?:^|` + jsSpace + `)([\w./-]+\.[\w-]+)(?:` + jsSpace + `|$)`)
)

// isTrivialOnlyBash is rank.ts isTrivialOnlyBash().
func isTrivialOnlyBash(raw string) bool {
	lines := jsSplitLines(raw)
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		kept = append(kept, lines[i])
		if closeIdx := HeredocCloseIndex(lines, i); closeIdx != -1 {
			i = closeIdx
		}
	}
	meaningful := 0
	for _, line := range kept {
		trimmed := jsTrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trivialBashLineRe.MatchString(trimmed) {
			continue
		}
		meaningful++
	}
	return meaningful == 0
}

// asPathSet is rank.ts asPathSet().
func asPathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path != "" {
			set[path] = true
		}
	}
	return set
}

// bashCommandFromBlock is rank.ts bashCommandFromBlock().
func bashCommandFromBlock(block NormalizedBlock) (string, bool) {
	if block.Kind == KindBash {
		return block.Command, true
	}
	if block.Kind == KindToolCall && compileIgnoreCase(`^bash$`).MatchString(block.Name) {
		if command, ok := block.Args["command"].(string); ok {
			return command, true
		}
	}
	return "", false
}

// pathFromBlock is rank.ts pathFromBlock().
func pathFromBlock(block NormalizedBlock) (string, bool) {
	if block.Kind == KindToolCall {
		return extractPath(block.Args)
	}
	if block.Kind == KindBash {
		if match := bashPathRe.FindStringSubmatch(block.Command); match != nil {
			return match[1], true
		}
	}
	return "", false
}

func addScore(ranked *RankedBlock, points int, reason string) {
	ranked.Score += points
	ranked.Reasons = append(ranked.Reasons, reason)
}

func containsStr(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// scoreBlock is rank.ts scoreBlock().
func scoreBlock(block NormalizedBlock, index, total int, modifiedFiles, readFiles map[string]bool) RankedBlock {
	ranked := RankedBlock{Block: block, Index: index, Reasons: make([]string, 0, 4)}
	recency := 0
	if total > 1 {
		recency = int(jsRound(float64(index) / float64(total-1) * 12))
	}
	addScore(&ranked, recency, "recency")

	switch block.Kind {
	case KindUser:
		addScore(&ranked, 18, "user-turn")
	case KindAssistant:
		addScore(&ranked, 10, "assistant-context")
	case KindToolResult:
		addScore(&ranked, 1, "tool-result-low-value")
	}

	if block.Kind == KindToolCall {
		command, hasCommand := bashCommandFromBlock(block)
		switch {
		case editToolRe.MatchString(block.Name):
			addScore(&ranked, 34, "edit-tool")
		case hasCommand && testCommandRe.MatchString(command):
			addScore(&ranked, 26, "test-command")
		case readToolRe.MatchString(block.Name):
			addScore(&ranked, 6, "read-tool")
		default:
			addScore(&ranked, 12, "tool-call")
		}
		if hasCommand && workflowCommandRe.MatchString(command) {
			addScore(&ranked, 14, "workflow-command")
		}
		if hasCommand && isTrivialOnlyBash(command) {
			addScore(&ranked, -trivialBashPenalty, "trivial-bash")
		}
	}

	if block.Kind == KindBash {
		addScore(&ranked, 8, "bash")
		if block.HasExit && block.ExitCode != 0 {
			addScore(&ranked, 24, "nonzero-exit")
		}
		if testCommandRe.MatchString(block.Command) {
			addScore(&ranked, 22, "test-command")
		}
		if workflowCommandRe.MatchString(block.Command) {
			addScore(&ranked, 14, "workflow-command")
		}
		// Penalize scaffolding-only commands, but not ones that failed.
		if isTrivialOnlyBash(block.Command) && !(block.HasExit && block.ExitCode != 0) {
			addScore(&ranked, -trivialBashPenalty, "trivial-bash")
		}
	}

	if path, ok := pathFromBlock(block); ok {
		if modifiedFiles[path] {
			addScore(&ranked, 18, "hook-modified-file")
		}
		if readFiles[path] {
			addScore(&ranked, 6, "hook-read-file")
		}
	}

	if block.Kind == KindToolResult && jsLen(block.Text) > 1000 {
		addScore(&ranked, -8, "long-tool-result")
	}
	return ranked
}

// jsRound is `Math.round`: half away from zero, which for the non-negative
// values here is plain rounding half up.
func jsRound(value float64) float64 {
	if value < 0 {
		return -jsRound(-value)
	}
	floor := float64(int64(value))
	if value-floor >= 0.5 {
		return floor + 1
	}
	return floor
}

// boostAdjacency is rank.ts boostAdjacency().
func boostAdjacency(ranked []RankedBlock) {
	important := make([]int, 0, len(ranked))
	for _, r := range ranked {
		if r.Score >= 34 || containsStr(r.Reasons, "edit-tool") || containsStr(r.Reasons, "test-command") ||
			containsStr(r.Reasons, "nonzero-exit") {
			important = append(important, r.Index)
		}
	}
	for _, idx := range important {
		for i := idx - 1; i >= 0 && i >= idx-8; i-- {
			if ranked[i].Block.Kind == KindUser {
				addScore(&ranked[i], 10, "near-important-event")
				break
			}
		}
		for i := idx - 1; i >= 0 && i >= idx-4; i-- {
			if ranked[i].Block.Kind == KindAssistant {
				addScore(&ranked[i], 7, "near-important-event")
				break
			}
		}
		for i := idx + 1; i <= len(ranked)-1 && i <= idx+4; i++ {
			if ranked[i].Block.Kind == KindAssistant || ranked[i].Block.Kind == KindBash {
				addScore(&ranked[i], 5, "after-important-event")
				break
			}
		}
	}
}

// nextNonToolResult is rank.ts nextNonToolResult().
func nextNonToolResult(ranked []RankedBlock, index int) (NormalizedBlock, bool) {
	for i := index + 1; i < len(ranked); i++ {
		if ranked[i].Block.Kind != KindToolResult {
			return ranked[i].Block, true
		}
	}
	return NormalizedBlock{}, false
}

// boostSegmentClosingAssistants is rank.ts boostSegmentClosingAssistants().
func boostSegmentClosingAssistants(ranked []RankedBlock) {
	for i := 0; i < len(ranked); i++ {
		current := ranked[i]
		if current.Block.Kind != KindAssistant {
			continue
		}
		if jsLen(jsTrimSpace(current.Block.Text)) < minSegmentClosingLen {
			continue
		}
		next, ok := nextNonToolResult(ranked, i)
		if !ok || next.Kind == KindUser {
			addScore(&ranked[i], 14, "segment-closing-assistant")
		}
	}
}

// dedupKey is rank.ts dedupKey().
func dedupKey(block NormalizedBlock) (string, bool) {
	command, hasCommand := bashCommandFromBlock(block)
	if hasCommand {
		if match := ghPrPollRe.FindStringSubmatch(command); match != nil {
			return "gh-pr-poll:" + match[1], true
		}
	}
	if hasCommand {
		normalized := jsTrimSpace(multiSpaceRe.ReplaceAllString(command, " "))
		if normalized == "" {
			return "", false
		}
		return "bash:" + normalized, true
	}
	if block.Kind == KindToolCall {
		if path, ok := pathFromBlock(block); ok {
			return "tool:" + strings.ToLower(block.Name) + ":" + path, true
		}
	}
	return "", false
}

// multiSpaceRe is rank.ts's `/\s+/g`.
var multiSpaceRe = mustCompile(jsSpace + `+`)

// RankBriefBlocks is rank.ts rankBriefBlocks().
func RankBriefBlocks(blocks []NormalizedBlock, options BriefRankingOptions) []RankedBlock {
	modifiedFiles := asPathSet(nil)
	readFiles := asPathSet(nil)
	if options.FileOps != nil {
		modifiedFiles = asPathSet(options.FileOps.ModifiedFiles)
		readFiles = asPathSet(options.FileOps.ReadFiles)
	}
	ranked := make([]RankedBlock, 0, len(blocks))
	for index, block := range blocks {
		ranked = append(ranked, scoreBlock(block, index, len(blocks), modifiedFiles, readFiles))
	}
	boostAdjacency(ranked)
	boostSegmentClosingAssistants(ranked)
	return ranked
}

// SelectRankedBriefBlocks is rank.ts selectRankedBriefBlocks().
func SelectRankedBriefBlocks(blocks []NormalizedBlock, options BriefRankingOptions) []NormalizedBlock {
	maxBlocks := options.MaxBlocks
	if maxBlocks == 0 {
		maxBlocks = defaultMaxBlocks
	}
	// Size-relative budget: clamp(slope * blockCount, floor, ceiling).
	maxBriefChars := options.MaxBriefChars
	hasBudget := options.MaxBriefChars != 0
	if options.MaxBriefChars != 0 && options.MaxBriefCharsCeiling != 0 && options.BriefCharsPerBlock != 0 {
		scaled := options.BriefCharsPerBlock * len(blocks)
		maxBriefChars = int(jsRound(mathMin(
			float64(options.MaxBriefCharsCeiling),
			mathMax(float64(options.MaxBriefChars), float64(scaled)),
		)))
		hasBudget = true
	}
	// Fast path: nothing to trim by count and no char budget to enforce.
	if len(blocks) <= maxBlocks && !hasBudget {
		return blocks
	}

	preserveRecent := options.PreserveRecentBlocks
	if preserveRecent == 0 {
		preserveRecent = defaultRecentBlocks
	}
	if preserveRecent > maxBlocks {
		preserveRecent = maxBlocks
	}
	ranked := RankBriefBlocks(blocks, options)
	selected := make(map[int]bool, maxBlocks)
	seenKeys := make(map[string]bool, maxBlocks)

	var costs []int
	if hasBudget {
		costs = make([]int, len(blocks))
		for i, block := range blocks {
			if block.Kind == KindToolResult {
				costs[i] = 0
				continue
			}
			costs[i] = jsLen(CompileBrief([]NormalizedBlock{block})) + 1
		}
	}
	usedChars := 0

	// Keep the latest blocks to preserve local continuity, newest first.
	start := len(blocks) - preserveRecent
	if start < 0 {
		start = 0
	}
	for i := len(blocks) - 1; i >= start; i-- {
		if blocks[i].Kind == KindToolResult {
			continue
		}
		if selected[i] {
			continue
		}
		if hasBudget && usedChars+costs[i] > maxBriefChars {
			continue
		}
		selected[i] = true
		if hasBudget {
			usedChars += costs[i]
		}
		if key, ok := dedupKey(blocks[i]); ok {
			seenKeys[key] = true
		}
	}

	ordered := make([]RankedBlock, len(ranked))
	copy(ordered, ranked)
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].Score != ordered[b].Score {
			return ordered[a].Score > ordered[b].Score
		}
		return ordered[b].Index < ordered[a].Index
	})
	for _, item := range ordered {
		if len(selected) >= maxBlocks {
			break
		}
		if selected[item.Index] {
			continue
		}
		if item.Block.Kind == KindToolResult {
			continue
		}
		if key, ok := dedupKey(item.Block); ok && seenKeys[key] {
			continue
		}
		if hasBudget {
			// Skip (not break) so smaller high-value blocks can still fit.
			if usedChars+costs[item.Index] > maxBriefChars {
				continue
			}
			usedChars += costs[item.Index]
		}
		selected[item.Index] = true
		if key, ok := dedupKey(item.Block); ok {
			seenKeys[key] = true
		}
	}

	indices := make([]int, 0, len(selected))
	for index := range selected {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	out := make([]NormalizedBlock, 0, len(indices))
	for _, index := range indices {
		out = append(out, blocks[index])
	}
	return out
}

func mathMin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
