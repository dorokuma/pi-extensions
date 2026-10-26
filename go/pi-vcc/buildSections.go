package pivcc

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
	// HasEarlyRequests is build-sections.ts hasEarlyRequests: the previous
	// summary already holds the early requests, so this compaction's first
	// messages are ordinary user messages.
	HasEarlyRequests bool
}

// formatFileActivity is build-sections.ts formatFileActivity(): the files
// section's lines, capped at FILES_PER_CATEGORY per category (the newest kept).
func formatFileActivity(blocks []NormalizedBlock, fileOps *FileOps) []string {
	activity := ExtractFiles(blocks, fileOps)
	// Dedup: if already Modified, drop from Created.
	for _, path := range activity.Modified.Items() {
		activity.Created.Delete(path)
	}
	lines := make([]string, 0, 3)
	capSet := func(set *orderedSet) string {
		return CapItems(set.Items(), FilesPerCategory, ", ")
	}
	if activity.Modified.Len() > 0 {
		lines = append(lines, "Modified: "+capSet(activity.Modified))
	}
	if activity.Created.Len() > 0 {
		lines = append(lines, "Created: "+capSet(activity.Created))
	}
	if activity.Read.Len() > 0 {
		lines = append(lines, "Read: "+capSet(activity.Read))
	}
	return lines
}

// BuildSections is build-sections.ts buildSections().
func BuildSections(input BuildSectionsInput) SectionData {
	briefBlocks := input.Blocks
	if input.BriefBlocks != nil {
		briefBlocks = input.BriefBlocks
	}
	// The early requests are the session's first user messages, picked by
	// position; they are not repeated in [User Messages].
	messages := ExtractUserMessages(input.Blocks)
	early := messages
	if input.HasEarlyRequests {
		early = nil
	} else if len(early) > EarlyRequestsKept {
		early = early[:EarlyRequestsKept]
	}
	return SectionData{
		EarlyRequests:   early,
		FilesAndChanges: formatFileActivity(input.Blocks, input.FileOps),
		Commits:         FormatCommits(ExtractCommits(input.Blocks), 8),
		UserMessages:    KeepNewestUserMessages(messages[len(early):]),
		TrackedCommands: FormatTrackedCommands(ExtractTrackedCommands(input.Blocks, input.TrackCommands)),
		BriefTranscript: StringifyBrief(BuildBriefSections(briefBlocks)),
	}
}
