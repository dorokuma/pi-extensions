package pivcc

import "strings"

// Ports src/core/build-sections.ts: blocks -> SectionData.

// BuildSectionsInput is build-sections.ts BuildSectionsInput.
type BuildSectionsInput struct {
	Blocks []NormalizedBlock
	// BriefBlocks overrides the blocks the brief transcript is built from.
	BriefBlocks []NormalizedBlock
	// FileOps is the hook-provided file activity, authoritative for files
	// touched before this compaction.
	FileOps *FileOps
	// TrackCommands names the commands to record in [Tracked Commands].
	TrackCommands []string
}

// blockerRe is build-sections.ts BLOCKER_RE.
var blockerRe = compileIgnoreCase(`\b(fail(ed|s|ure|ing)?|broken|cannot|can't|won't work|does not work|doesn't work|still (broken|failing|wrong)|blocked|blocker|not (fixed|resolved|working)|crash(es|ed|ing)?)\b`)

// continuationBulletRe is build-sections.ts's `/^\s*[-*+>]\s/`.
var continuationBulletRe = mustCompile(`^` + jsSpace + `*[-*+>]` + jsSpace)

// continuationParenRe is build-sections.ts's `/^\s*\(/`.
var continuationParenRe = mustCompile(`^` + jsSpace + `*\(`)

// sentenceStartRe is build-sections.ts's `/^\s*["'` + "`" + `*_]?[A-Z` + "`" + `]/`.
var sentenceStartRe = mustCompile(`^` + jsSpace + `*["'` + "`" + `*_]?[A-Z` + "`" + `]`)

// extractOutstandingContext is build-sections.ts extractOutstandingContext():
// unresolved errors and pending questions from the last 20 blocks, capped at 5.
func extractOutstandingContext(blocks []NormalizedBlock) []string {
	items := make([]string, 0, 5)
	start := 0
	if len(blocks) > 20 {
		start = len(blocks) - 20
	}
	for _, block := range blocks[start:] {
		if block.Kind != KindAssistant && block.Kind != KindUser {
			continue
		}
		for _, line := range NonEmptyLines(block.Text) {
			if !blockerRe.MatchString(line) {
				continue
			}
			if jsLen(line) < 15 {
				continue
			}
			// Skip continuation fragments.
			if continuationBulletRe.MatchString(line) {
				continue
			}
			if continuationParenRe.MatchString(line) {
				continue
			}
			// Require a sentence-like start.
			if !sentenceStartRe.MatchString(line) {
				continue
			}
			clipped := ClipSentence(line, 150)
			if block.Kind == KindUser {
				clipped = "[user] " + clipped
			}
			duplicate := false
			for _, existing := range items {
				if existing == clipped {
					duplicate = true
					break
				}
			}
			if !duplicate {
				items = append(items, clipped)
			}
			break
		}
	}
	if len(items) > 5 {
		items = items[:5]
	}
	return items
}

// formatFileActivity is build-sections.ts formatFileActivity(): the files
// section's lines, capped at 10 per category.
func formatFileActivity(blocks []NormalizedBlock, fileOps *FileOps) []string {
	activity := ExtractFiles(blocks, fileOps)
	// Dedup: if already Modified, drop from Created.
	for _, path := range activity.Modified.Items() {
		activity.Created.Delete(path)
	}
	lines := make([]string, 0, 3)
	capSet := func(set *orderedSet, limit int) string {
		items := set.Items()
		if len(items) <= limit {
			return strings.Join(items, ", ")
		}
		return strings.Join(items[:limit], ", ") + " (+" + jsNumberString(float64(len(items)-limit)) + " more)"
	}
	if activity.Modified.Len() > 0 {
		lines = append(lines, "Modified: "+capSet(activity.Modified, 10))
	}
	if activity.Created.Len() > 0 {
		lines = append(lines, "Created: "+capSet(activity.Created, 10))
	}
	if activity.Read.Len() > 0 {
		lines = append(lines, "Read: "+capSet(activity.Read, 10))
	}
	return lines
}

// BuildSections is build-sections.ts buildSections().
func BuildSections(input BuildSectionsInput) SectionData {
	briefBlocks := input.Blocks
	if input.BriefBlocks != nil {
		briefBlocks = input.BriefBlocks
	}
	sessionGoal := ExtractGoals(input.Blocks)
	userPreferences := DedupPreferencesAgainstGoals(ExtractPreferences(input.Blocks), sessionGoal)
	return SectionData{
		SessionGoal:        sessionGoal,
		OutstandingContext: extractOutstandingContext(input.Blocks),
		FilesAndChanges:    formatFileActivity(input.Blocks, input.FileOps),
		Commits:            FormatCommits(ExtractCommits(input.Blocks), 8),
		UserPreferences:    userPreferences,
		TrackedCommands:    FormatTrackedCommands(ExtractTrackedCommands(input.Blocks, input.TrackCommands)),
		BriefTranscript:    StringifyBrief(BuildBriefSections(briefBlocks)),
	}
}
