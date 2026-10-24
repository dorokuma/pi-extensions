package moshihooks

// The wire shape written to the moshi-hook daemon socket.

// Envelope is one normalized lifecycle envelope (TS: the object literals of
// baseEnvelope, L498, plus the per-event fields assigned in handleEvent).
//
// Field order is the TS insertion order — the baseEnvelope fields first, then
// the per-event fields — and the JSON names are the TS property names verbatim.
// The per-event fields are pointers because the TS distinguishes "never
// assigned" from "assigned an empty string": `envelope.message =
// formatUserPromptDetail(...)` is always on the wire, while `envelope.actionId`
// appears only when there is one.
type Envelope struct {
	Type             string            `json:"type"`
	Source           string            `json:"source"`
	SessionID        string            `json:"sessionId"`
	EventName        string            `json:"eventName"`
	Cwd              string            `json:"cwd"`
	ProjectName      string            `json:"projectName"`
	TerminalKind     string            `json:"terminalKind"`
	MuxEnv           map[string]string `json:"muxEnv"`
	TmuxSession      string            `json:"tmuxSession"`
	TmuxWindow       string            `json:"tmuxWindow"`
	TmuxPane         string            `json:"tmuxPane"`
	TmuxSocket       string            `json:"tmuxSocket"`
	ZellijSession    string            `json:"zellijSession"`
	ZellijPane       string            `json:"zellijPane"`
	HerdrSession     string            `json:"herdrSession"`
	HerdrPane        string            `json:"herdrPane"`
	HerdrWorkspaceID string            `json:"herdrWorkspaceId"`
	HerdrWorkspace   string            `json:"herdrWorkspace"`
	HerdrTabID       string            `json:"herdrTabId"`
	HerdrTab         string            `json:"herdrTab"`
	ModelName        string            `json:"modelName"`
	TranscriptPath   string            `json:"transcriptPath"`
	RequestedAt      string            `json:"requestedAt"`

	// ContextRemaining is present only when the host reported positive usage
	// (TS: `if (typeof payload.context_remaining === "number" && > 0)`).
	ContextRemaining *int `json:"contextRemaining,omitempty"`

	// Per-event fields.
	ActionID          *string            `json:"actionId,omitempty"`
	Phase             *string            `json:"phase,omitempty"`
	Category          *string            `json:"category,omitempty"`
	ToolName          *string            `json:"toolName,omitempty"`
	Title             *string            `json:"title,omitempty"`
	Subtitle          *string            `json:"subtitle,omitempty"`
	Message           *string            `json:"message,omitempty"`
	InteractivePrompt *InteractivePrompt `json:"interactivePrompt,omitempty"`
}

// InteractivePrompt is the Chat View card an approval event carries
// (TS: the prompt objects of PlanReview / QuestionAsked / QuestionResolved).
type InteractivePrompt struct {
	Kind         string     `json:"kind"`
	Plan         string     `json:"plan,omitempty"`
	Questions    []Question `json:"questions,omitempty"`
	ToolUseID    string     `json:"toolUseId,omitempty"`
	PlanFilePath string     `json:"planFilePath,omitempty"`
}

// Question is one normalized ask question
// (TS: askQuestions, L700). Header, question, multiSelect and options are
// always on the wire, as in the TS object literal.
type Question struct {
	Header      string           `json:"header"`
	Question    string           `json:"question"`
	MultiSelect bool             `json:"multiSelect"`
	Options     []QuestionOption `json:"options"`
	HasPreview  bool             `json:"hasPreview,omitempty"`
	ID          string           `json:"id,omitempty"`
	// RecommendedIndex is present only for a valid index into Options.
	RecommendedIndex *int `json:"recommendedIndex,omitempty"`
}

// QuestionOption is one ask option; the label is what the daemon renders and
// the description is its subtitle (TS: askQuestions option push, L716).
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
