package pivcc

import (
	"regexp"
	"strings"
)

// Ports src/core/summarize.ts: build the fresh summary, merge it with the
// previous one, and append RECALL_NOTE once.

// CompileInput is summarize.ts CompileInput.
type CompileInput struct {
	Messages []map[string]any
	// PreviousSummary is the summary the last compaction stored, if any.
	PreviousSummary    string
	HasPreviousSummary bool
	FileOps            *FileOps
	// SourceIndices supplies the session-global `#N` index per message
	// position; a negative entry stands for TS's `undefined`. Nil keeps the
	// legacy positional behaviour.
	SourceIndices []int
	// TrackCommands threads PiVccSettings.TrackCommands into the summary.
	TrackCommands []string
}

// RankedCompileInput is summarize.ts RankedCompileInput.
type RankedCompileInput struct {
	CompileInput
	Ranking *BriefRankingOptions
}

// headerNames is summarize.ts HEADER_NAMES, in rendering order.
var headerNames = []string{
	"Session Goal", "Files And Changes", "Commits", "Tracked Commands", "Outstanding Context", "User Preferences",
}

// summarySeparator is summarize.ts SEPARATOR.
const summarySeparator = "\n\n---\n\n"

// summarySeparatorRe is summarize.ts's `/\n\n---\n\n/` match used by sectionOf.
var summarySeparatorIndex = "\n\n---\n\n"

// sectionOf is summarize.ts sectionOf(): one named section's text, header
// included, trimmed.
func sectionOf(text, header string) string {
	tag := "[" + header + "]"
	start := strings.Index(text, tag)
	if start < 0 {
		return ""
	}
	after := text[start:]
	nextSection := make([]int, 0, len(headerNames))
	for _, name := range headerNames {
		if name == header {
			continue
		}
		if idx := strings.Index(after, "["+name+"]"); idx > 0 {
			nextSection = append(nextSection, idx)
		}
	}
	candidates := nextSection
	if sep := strings.Index(after, summarySeparatorIndex); sep > 0 {
		candidates = append(candidates, sep)
	}
	sortInts(candidates)
	if len(candidates) > 0 && candidates[0] != 0 {
		return jsTrimSpace(after[:candidates[0]])
	}
	return jsTrimSpace(after)
}

// briefOf is summarize.ts briefOf(): everything after the first separator.
func briefOf(text string) string {
	idx := strings.Index(text, summarySeparatorIndex)
	if idx < 0 {
		return ""
	}
	return jsTrimSpace(text[idx+len(summarySeparatorIndex):])
}

// mergeHeaderSection is summarize.ts mergeHeaderSection().
func mergeHeaderSection(header, prev, fresh string) string {
	// Outstanding Context is volatile — always use fresh only.
	if header == "Outstanding Context" {
		return fresh
	}
	if prev == "" {
		return fresh
	}
	if fresh == "" {
		return prev
	}
	if header == "Files And Changes" {
		return mergeFileLines(prev, fresh)
	}
	if header == "Tracked Commands" {
		return mergeTrackedCommandLines(prev, fresh)
	}

	isClean := func(line string) bool {
		return strings.HasPrefix(line, "- ") && !strings.Contains(line, "<skill") && !strings.Contains(line, "</skill")
	}
	combined := make([]string, 0, 16)
	seen := make(map[string]bool, 16)
	for _, text := range []string{prev, fresh} {
		for _, line := range jsSplitLines(text) {
			if !isClean(line) || seen[line] {
				continue
			}
			seen[line] = true
			combined = append(combined, line)
		}
	}
	capLines := 15
	if header == "Session Goal" || header == "Commits" {
		capLines = 8
	}
	capped := combined
	if len(capped) > capLines {
		capped = capped[len(capped)-capLines:]
	}
	if len(capped) == 0 {
		return ""
	}
	return "[" + header + "]\n" + strings.Join(capped, "\n")
}

// mergeCategorizedLines is summarize.ts mergeCategorizedLines(): union each
// category's items across compactions. touchOnDup re-inserts an existing entry
// at the tail, which with tail-capping makes the section "N most-recently-USED".
func mergeCategorizedLines(categories []string, prev, fresh, splitOn string, touchOnDup bool) map[string]*orderedSet {
	merged := make(map[string]*orderedSet, len(categories))
	for _, category := range categories {
		merged[category] = newOrderedSet()
	}
	for _, text := range []string{prev, fresh} {
		for _, line := range jsSplitLines(text) {
			for _, category := range categories {
				prefix := "- " + category + ": "
				if !strings.HasPrefix(line, prefix) {
					continue
				}
				rest := StripCapMarker(line[len(prefix):])
				for _, piece := range strings.Split(rest, splitOn) {
					trimmed := jsTrimSpace(piece)
					if trimmed == "" {
						continue
					}
					if touchOnDup && merged[category].Has(trimmed) {
						merged[category].Delete(trimmed)
					}
					merged[category].Add(trimmed)
				}
			}
		}
	}
	return merged
}

// formatCategorizedLines is summarize.ts formatCategorizedLines().
func formatCategorizedLines(header string, merged map[string]*orderedSet, categories []string, joinWith string, itemLimit int, keep string) string {
	lines := make([]string, 0, len(categories))
	for _, category := range categories {
		if merged[category].Len() > 0 {
			lines = append(lines, "- "+category+": "+CapItems(merged[category].Items(), itemLimit, joinWith, keep))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "[" + header + "]\n" + strings.Join(lines, "\n")
}

// fileCategories is summarize.ts FILE_CATEGORIES.
var fileCategories = []string{"Modified", "Created", "Read"}

// mergeFileLines is summarize.ts mergeFileLines().
func mergeFileLines(prev, fresh string) string {
	merged := mergeCategorizedLines(fileCategories, prev, fresh, ",", false)
	// Dedup: if already in Modified, drop from Created.
	for _, path := range merged["Modified"].Items() {
		merged["Created"].Delete(path)
	}
	return formatCategorizedLines("Files And Changes", merged, fileCategories, ", ", 10, "head")
}

// categoryNameRe is summarize.ts's `/^- ([^:]+): /`.
var categoryNameRe = mustCompile(`^- ([^:]+): `)

// discoverCategoryNames is summarize.ts discoverCategoryNames().
func discoverCategoryNames(text string) []string {
	names := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, line := range jsSplitLines(text) {
		if match := categoryNameRe.FindStringSubmatch(line); match != nil {
			if seen[match[1]] {
				continue
			}
			seen[match[1]] = true
			names = append(names, match[1])
		}
	}
	return names
}

// mergeTrackedCommandLines is summarize.ts mergeTrackedCommandLines().
func mergeTrackedCommandLines(prev, fresh string) string {
	names := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, text := range []string{prev, fresh} {
		for _, name := range discoverCategoryNames(text) {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	merged := mergeCategorizedLines(names, prev, fresh, CommandSeparator, true)
	return formatCategorizedLines("Tracked Commands", merged, names, CommandSeparator, CommandsPerName, "tail")
}

// mergeBriefTranscript is summarize.ts mergeBriefTranscript().
func mergeBriefTranscript(prev, fresh string) string {
	if prev == "" {
		return fresh
	}
	if fresh == "" {
		return prev
	}
	return prev + "\n\n" + fresh
}

// briefLineCount is summarize.ts briefLineCount().
func briefLineCount(text string) int {
	if text == "" {
		return 0
	}
	return len(jsSplitLines(text))
}

// capBriefToLineBudget is summarize.ts capBriefToLineBudget().
func capBriefToLineBudget(text string, maxLines int) string {
	if text == "" || maxLines <= 0 {
		return ""
	}
	lines := jsSplitLines(text)
	if len(lines) <= maxLines {
		return text
	}
	kept := lines[len(lines)-maxLines:]
	firstHeader := -1
	for i, line := range kept {
		if headerLineRe.MatchString(line) {
			firstHeader = i
			break
		}
	}
	clean := kept
	if firstHeader > 0 {
		clean = kept[firstHeader:]
	}
	omitted := len(lines) - len(clean)
	return "...(" + jsNumberString(float64(omitted)) + " earlier lines omitted)\n\n" + strings.Join(clean, "\n")
}

// mergeBriefTranscriptWithFreshBudget is summarize.ts
// mergeBriefTranscriptWithFreshBudget().
func mergeBriefTranscriptWithFreshBudget(prev, fresh string) string {
	if prev == "" {
		return fresh
	}
	if fresh == "" {
		return CapBrief(prev)
	}
	freshLines := briefLineCount(fresh)
	remaining := BriefMaxLines - freshLines
	if remaining < 0 {
		remaining = 0
	}
	prevTail := capBriefToLineBudget(prev, remaining)
	if prevTail == "" {
		return fresh
	}
	return prevTail + "\n\n" + fresh
}

// mergePreviousOptions is summarize.ts's inline options.
type mergePreviousOptions struct {
	preserveFreshBrief bool
}

// continuationIndentRe is summarize.ts's `/\n[ \t]+(?=\S)/g`: a newline
// followed by spaces/tabs before a non-whitespace byte. RE2 has no lookahead, so
// the group is captured and re-emitted by rejoinWrappedSection instead.
var continuationIndentRe = mustCompile("\n[ \t]+(" + jsNotSpace + ")")

// rejoinWrappedSection is sectionOf(prev, header) followed by the
// `replace(/\n[ \t]+(?=\S)/g, " ")` rejoin: the stored summary went through
// wrapLongLines, so an item's indented continuation lines are joined back.
func rejoinWrappedSection(text string) string {
	var b strings.Builder
	i := 0
	for i < len(text) {
		if text[i] == '\n' {
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
			if j > i+1 && j < len(text) && !isLineTerminatorOrSpaceByte(text[j]) {
				b.WriteByte(' ')
				i = j
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func isLineTerminatorOrSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// mergePrevious is summarize.ts mergePrevious().
func mergePrevious(prev, fresh string, options mergePreviousOptions) string {
	headers := make([]string, 0, len(headerNames))
	for _, header := range headerNames {
		freshSec := sectionOf(fresh, header)
		prevSec := rejoinWrappedSection(sectionOf(prev, header))
		if merged := mergeHeaderSection(header, prevSec, freshSec); merged != "" {
			headers = append(headers, merged)
		}
	}

	prevBrief := briefOf(prev)
	freshBrief := briefOf(fresh)
	var mergedBrief string
	if options.preserveFreshBrief {
		mergedBrief = mergeBriefTranscriptWithFreshBudget(prevBrief, freshBrief)
	} else {
		mergedBrief = mergeBriefTranscript(prevBrief, freshBrief)
	}

	parts := make([]string, 0, 2)
	if len(headers) > 0 {
		parts = append(parts, strings.Join(headers, "\n\n"))
	}
	if mergedBrief != "" {
		if options.preserveFreshBrief {
			parts = append(parts, mergedBrief)
		} else {
			parts = append(parts, CapBrief(mergedBrief))
		}
	}
	return strings.Join(parts, summarySeparator)
}

// compileWithBriefBlocksOptions is summarize.ts's inline options.
type compileWithBriefBlocksOptions struct {
	briefBlocksFor            func(blocks []NormalizedBlock) []NormalizedBlock
	capFreshBrief             *bool
	preserveFreshBriefOnMerge bool
}

// compileWithBriefBlocks is summarize.ts compileWithBriefBlocks(): normalize,
// filter noise, build the sections, format, strip any stale RECALL_NOTE from
// the previous summary, merge, and append RECALL_NOTE once.
func compileWithBriefBlocks(input CompileInput, options compileWithBriefBlocksOptions) string {
	blocks := FilterNoise(Normalize(input.Messages, input.SourceIndices))
	var briefBlocks []NormalizedBlock
	if options.briefBlocksFor != nil {
		briefBlocks = options.briefBlocksFor(blocks)
	}
	data := BuildSections(BuildSectionsInput{
		Blocks:        blocks,
		BriefBlocks:   briefBlocks,
		FileOps:       input.FileOps,
		TrackCommands: input.TrackCommands,
	})
	capFresh := true
	if options.capFreshBrief != nil {
		capFresh = *options.capFreshBrief
	}
	fresh := FormatSummary(data, FormatSummaryOptions{CapBriefTranscript: &capFresh})
	prev := ""
	hasPrev := false
	if input.HasPreviousSummary && input.PreviousSummary != "" {
		prev = StripRecallNote(input.PreviousSummary)
		hasPrev = true
	}
	merged := fresh
	if hasPrev {
		merged = mergePrevious(prev, fresh, mergePreviousOptions{preserveFreshBrief: options.preserveFreshBriefOnMerge})
	}
	if merged == "" {
		return ""
	}
	return WrapLongLinesDefault(merged + summarySeparator + RecallNote)
}

// Compile is summarize.ts compile().
func Compile(input CompileInput) string {
	return compileWithBriefBlocks(input, compileWithBriefBlocksOptions{})
}

// CompileRanked is summarize.ts compileRanked(): the ranked, size-relative brief.
func CompileRanked(input RankedCompileInput) string {
	ranking := BriefRankingOptions{}
	if input.Ranking != nil {
		ranking = *input.Ranking
	}
	if ranking.FileOps == nil {
		ranking.FileOps = input.FileOps
	}
	capFresh := false
	return compileWithBriefBlocks(input.CompileInput, compileWithBriefBlocksOptions{
		briefBlocksFor: func(blocks []NormalizedBlock) []NormalizedBlock {
			return SelectRankedBriefBlocks(blocks, ranking)
		},
		capFreshBrief:             &capFresh,
		preserveFreshBriefOnMerge: true,
	})
}

// recallNoteRe builds summarize.ts RECALL_NOTE_RE **from the (patched)
// RECALL_NOTE constant**: the note's words separated by any whitespace, with
// the optional separator in front of it. Building it from the constant is what
// keeps stripRecallNote working with the extra sentence — the reason the
// recall-note patch only has to touch the constant.
func buildRecallNoteRe(note string) *regexp.Regexp {
	words := strings.Fields(note)
	escaped := make([]string, 0, len(words))
	for _, word := range words {
		escaped = append(escaped, regexpEscape(word))
	}
	pattern := `\n*(?:---\n+)?` + strings.Join(escaped, jsSpace+"+") + `\n*`
	return regexp.MustCompile(pattern)
}

// recallNoteRe is the module's compiled RECALL_NOTE_RE.
var recallNoteRe = buildRecallNoteRe(RecallNote)

// StripRecallNote is summarize.ts stripRecallNote(): remove every RECALL_NOTE
// from a previous summary before merging, so the note never stacks.
func StripRecallNote(text string) string {
	return jsTrimRightSpace(recallNoteRe.ReplaceAllString(text, "\n\n"))
}

// sortInts sorts an int slice ascending.
func sortInts(values []int) {
	if len(values) < 2 {
		return
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
