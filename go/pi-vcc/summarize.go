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

// earlyHeader is summarize.ts EARLY: the section the first summary pins and
// every later compaction keeps as it is.
const earlyHeader = "Early Session Requests"

// headerNames is summarize.ts HEADER_NAMES, in rendering order.
var headerNames = []string{
	earlyHeader, "Files And Changes", "Commits", "Tracked Commands", "User Messages",
}

// summarySeparator is summarize.ts SEPARATOR.
const summarySeparator = "\n\n---\n\n"

// sectionHeaderLineRe is summarize.ts HEADER_LINE_RE: a "[Title Case]" line
// standing alone. JS's `$` (no /m flag) is end of input, and Go's `$` (no (?m)
// flag) is end of text, so the two agree when the regex is applied to one line.
var sectionHeaderLineRe = mustCompile(`^\[[A-Z][^\]\n]*\]$`)

// splitSummary is summarize.ts splitSummary(): the header area and the brief
// transcript. Without a leading header line the whole text is the brief; header
// sections only (no separator) have no brief.
func splitSummary(text string) (head, brief string) {
	firstLine := text
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		firstLine = text[:idx]
	}
	if !sectionHeaderLineRe.MatchString(firstLine) {
		return "", jsTrimSpace(text)
	}
	idx := strings.Index(text, summarySeparator)
	if idx < 0 {
		return text, ""
	}
	return text[:idx], jsTrimSpace(text[idx+len(summarySeparator):])
}

// sectionOf is summarize.ts sectionOf(): one named section's text, header
// included, trimmed. A section is the "[Title]" line standing alone in the
// header area plus the lines up to the next blank line, so a transcript line
// that merely mentions the name is never mistaken for the section.
func sectionOf(text, header string) string {
	// summarize.ts: `splitSummary(text).head.split("\n")`.
	head, _ := splitSummary(text)
	lines := jsSplitLines(head)
	start := -1
	for i, line := range lines {
		if line == "["+header+"]" {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := start + 1
	for end < len(lines) && jsTrimSpace(lines[end]) != "" {
		end++
	}
	return jsTrimSpace(strings.Join(lines[start:end], "\n"))
}

// briefOf is summarize.ts briefOf(): everything after the first separator.
func briefOf(text string) string {
	_, brief := splitSummary(text)
	return brief
}

// isCleanLine is summarize.ts isClean(): an item line of a section.
func isCleanLine(line string) bool {
	return strings.HasPrefix(line, "- ") && !strings.Contains(line, "<skill") && !strings.Contains(line, "</skill")
}

// unwrapSection is summarize.ts unwrapSection(): both summaries went through
// wrapLongLines (the fresh one in formatSummary), so rejoin each item's indented
// continuation lines before a merge reads only "- " lines.
func unwrapSection(text, header string) string {
	return rejoinWrappedSection(sectionOf(text, header))
}

// goalMarkerRe is pinnedEarlyRequests()'s `/^- \[[^\]]*\]$/`: a
// "- [skill: x]" / "- [Scope change]" marker line rather than a real request.
var goalMarkerRe = mustCompile(`^- \[[^\]]*\]$`)

// pinnedEarlyRequests is summarize.ts pinnedEarlyRequests(): the previous
// summary's early requests, kept as they are: the first summary sets them. An
// older version's [Session Goal] gives its first real line (not a
// "- [skill: x]" / "- [Scope change]" marker).
func pinnedEarlyRequests(prev string) []string {
	early := make([]string, 0, EarlyRequestsKept)
	for _, line := range jsSplitLines(unwrapSection(prev, earlyHeader)) {
		if isCleanLine(line) {
			early = append(early, line)
		}
	}
	if len(early) > 0 {
		if len(early) > EarlyRequestsKept {
			early = early[:EarlyRequestsKept]
		}
		return early
	}
	for _, line := range jsSplitLines(unwrapSection(prev, "Session Goal")) {
		if isCleanLine(line) && !goalMarkerRe.MatchString(line) {
			return []string{line}
		}
	}
	return nil
}

// commitsCap is summarize.ts COMMITS_CAP.
const commitsCap = 8

// commitHashLineRe is summarize.ts COMMIT_HASH_RE: a commit line that carries
// its hash.
var commitHashLineRe = mustCompile(`^- [0-9a-f]{7,12}: `)

// bareCommit is summarize.ts bareCommit(): the line with its hash stripped, so a
// hashed and a hashless line of one commit compare equal.
func bareCommit(line string) string {
	return commitHashLineRe.ReplaceAllString(line, "- ")
}

// dedupCommitLines is summarize.ts dedupCommitLines(): one line per commit,
// first-seen order. A hashless line is the same commit as a hashed line with its
// message (one side did not capture the hash): keep the hashed one, in the
// earlier line's place.
func dedupCommitLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		duplicate := false
		for _, existing := range out {
			if existing == line {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		bare := bareCommit(line)
		if commitHashLineRe.MatchString(line) {
			hashless := -1
			for i, existing := range out {
				if existing == bare {
					hashless = i
					break
				}
			}
			if hashless >= 0 {
				out[hashless] = line
				continue
			}
			out = append(out, line)
			continue
		}
		already := false
		for _, existing := range out {
			if bareCommit(existing) == bare {
				already = true
				break
			}
		}
		if !already {
			out = append(out, line)
		}
	}
	return out
}

// cleanItemLines is the `text.split("\n").filter(isClean)` both merge paths use.
func cleanItemLines(text string) []string {
	out := make([]string, 0, 8)
	for _, line := range jsSplitLines(text) {
		if isCleanLine(line) {
			out = append(out, line)
		}
	}
	return out
}

// mergeHeaderSection is summarize.ts mergeHeaderSection().
func mergeHeaderSection(header, prev, fresh string) string {
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

	prevLines := cleanItemLines(prev)
	freshLines := cleanItemLines(fresh)

	if header == "User Messages" {
		// summarize.ts: keepNewestUserMessages([...prevLines, ...freshLines])
		merged := make([]string, 0, len(prevLines)+len(freshLines))
		merged = append(merged, prevLines...)
		merged = append(merged, freshLines...)
		return "[" + header + "]\n" + strings.Join(KeepNewestUserMessages(merged), "\n")
	}

	// Commits (and any non-pinned fallback section): one line per commit,
	// capped from the tail.
	capped := dedupCommitLines(append(append(make([]string, 0, len(prevLines)+len(freshLines)), prevLines...), freshLines...))
	if len(capped) > commitsCap {
		capped = capped[len(capped)-commitsCap:]
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
func formatCategorizedLines(header string, merged map[string]*orderedSet, categories []string, joinWith string, itemLimit int) string {
	lines := make([]string, 0, len(categories))
	for _, category := range categories {
		if merged[category].Len() > 0 {
			lines = append(lines, "- "+category+": "+CapItems(merged[category].Items(), itemLimit, joinWith))
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
	// Newest-last with re-touched files moved to the end, so the capped list
	// follows the files being worked on instead of freezing on the first ones.
	merged := mergeCategorizedLines(fileCategories, prev, fresh, ",", true)
	// Dedup: if already in Modified, drop from Created (file existed before).
	for _, path := range merged["Modified"].Items() {
		merged["Created"].Delete(path)
	}
	return formatCategorizedLines("Files And Changes", merged, fileCategories, ", ", FilesPerCategory)
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
	return formatCategorizedLines("Tracked Commands", merged, names, CommandSeparator, CommandsPerName)
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

// rejoinWrappedSection is summarize.ts's `replace(/\n[ \t]+(?=\S)/g, " ")`:
// the stored summary went through wrapLongLines, so an item's indented
// continuation lines are joined back into one line.
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
	// Merge header sections.
	early := pinnedEarlyRequests(prev)
	headers := make([]string, 0, len(headerNames))
	for _, header := range headerNames {
		merged := ""
		if header == earlyHeader && len(early) > 0 {
			merged = "[" + earlyHeader + "]\n" + strings.Join(early, "\n")
		} else {
			merged = mergeHeaderSection(header, unwrapSection(prev, header), unwrapSection(fresh, header))
		}
		if merged != "" {
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
	// Strip any legacy RECALL_NOTE baked into prev summary (pre-fix format)
	// so merge doesn't re-stack it inside the brief. TS's `prev` is
	// `string | undefined`: the flag below is its definedness, which decides
	// hasEarlyRequests, while a non-empty string decides the merge.
	prevDefined := input.HasPreviousSummary && input.PreviousSummary != ""
	prev := ""
	if prevDefined {
		prev = StripRecallNote(input.PreviousSummary)
	}
	capFresh := true
	if options.capFreshBrief != nil {
		capFresh = *options.capFreshBrief
	}
	data := BuildSections(BuildSectionsInput{
		Blocks:           blocks,
		BriefBlocks:      briefBlocks,
		FileOps:          input.FileOps,
		TrackCommands:    input.TrackCommands,
		HasEarlyRequests: prevDefined && len(pinnedEarlyRequests(prev)) > 0,
	})
	fresh := FormatSummary(data, FormatSummaryOptions{CapBriefTranscript: &capFresh})
	merged := fresh
	if prev != "" {
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
