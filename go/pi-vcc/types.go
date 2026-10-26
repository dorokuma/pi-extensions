package pivcc

// NormalizedBlocks are the uniform units every extractor reads, ported from
// src/types.ts and src/core/normalize.ts.

// BlockKind names the NormalizedBlock variant.
type BlockKind string

const (
	// KindUser is a user message's text.
	KindUser BlockKind = "user"
	// KindAssistant is assistant text.
	KindAssistant BlockKind = "assistant"
	// KindToolCall is one assistant tool call.
	KindToolCall BlockKind = "tool_call"
	// KindToolResult is a tool result message.
	KindToolResult BlockKind = "tool_result"
	// KindBash is a bashExecution message (pi's shell turns).
	KindBash BlockKind = "bash"
)

// NormalizedBlock is one unit of the normalized transcript. Only the fields a
// variant uses are set; the rest keep their zero value (TS's absent
// properties).
type NormalizedBlock struct {
	Kind BlockKind

	// Text is user/assistant text, or a tool result's text.
	Text string
	// Injected is types.ts `injected`: the text came from an extension
	// custom_message rather than from the user. It renders in the brief but is
	// not read into [Early Session Requests] or [User Messages].
	Injected bool
	// Name is a tool call's name or a tool result's tool name.
	Name string
	// Args is a tool call's argument object.
	Args map[string]any
	// Command and Output are a bash block's command and captured output.
	Command string
	Output  string
	// ExitCode is a bash block's exit status; HasExit is TS's
	// `exitCode != null` (the field is `number | undefined`).
	ExitCode int
	HasExit  bool

	// SourceIndex is the session-global `#N` index; HasSource is TS's
	// `sourceIndex != null`.
	SourceIndex int
	HasSource   bool
}

func userBlock(text string, index int, hasIndex bool) NormalizedBlock {
	return NormalizedBlock{Kind: KindUser, Text: text, SourceIndex: index, HasSource: hasIndex}
}

// userBlockInjected is a user block whose text an extension injected.
func userBlockInjected(text string, index int, hasIndex bool, injected bool) NormalizedBlock {
	block := userBlock(text, index, hasIndex)
	block.Injected = injected
	return block
}

// sourceRef renders ` (#N)` for a block with an index and "" otherwise
// (brief.ts's `b.sourceIndex != null ? ` (#${b.sourceIndex})` : ""`).
func sourceRef(b NormalizedBlock) string {
	if !b.HasSource {
		return ""
	}
	return " (#" + jsNumberString(float64(b.SourceIndex)) + ")"
}

// FileOps is the hook-provided file activity (src/types.ts FileOps).
type FileOps struct {
	ReadFiles     []string
	ModifiedFiles []string
	CreatedFiles  []string
}

// CompactionReason is src/types.ts CompactionReason.
type CompactionReason string

const (
	// ReasonManual is a `/compact` or `/pi-vcc` request.
	ReasonManual CompactionReason = "manual"
	// ReasonThreshold is the automatic context-threshold compaction.
	ReasonThreshold CompactionReason = "threshold"
	// ReasonOverflow is overflow recovery.
	ReasonOverflow CompactionReason = "overflow"
)

// SectionData is src/sections.ts SectionData: everything formatSummary renders.
type SectionData struct {
	// EarlyRequests is the session's first user messages, pinned across
	// compactions (extract/user-messages.ts).
	EarlyRequests   []string
	FilesAndChanges []string
	Commits         []string
	// UserMessages is the newest user messages after the early requests.
	UserMessages []string
	// TrackedCommands is populated only when settings.trackCommands is
	// non-empty (see settings.go).
	TrackedCommands []string
	BriefTranscript string
}

// PiVccCompactionDetails is src/details.ts PiVccCompactionDetails, the
// `details` payload stored on a compaction entry. Field order matches the TS
// object literal so the persisted JSON reads the same.
type PiVccCompactionDetails struct {
	Compactor           string           `json:"compactor"`
	Version             int              `json:"version"`
	Sections            []string         `json:"sections"`
	SourceMessageCount  int              `json:"sourceMessageCount"`
	PreviousSummaryUsed bool             `json:"previousSummaryUsed"`
	Reason              CompactionReason `json:"reason,omitempty"`
	WillRetry           bool             `json:"willRetry,omitempty"`
}
