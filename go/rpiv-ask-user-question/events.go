package rpivaskuserquestion

// Ports events.ts (v2.15.0): the public `rpiv:*` event contract.
//
// STABILITY POLICY — channel names are immutable; payload changes are
// append-only (listeners must tolerate unknown fields); breaking changes need a
// new channel with dual-emit. Payloads stay JSON-safe: primitives, arrays and
// plain objects only.

// Event channel names.
const (
	AskUserPromptEvent       = "rpiv:ask-user:prompt"
	AskUserBlockedEvent      = "rpiv:ask-user:blocked"
	AskUserAnswerEvent       = "rpiv:ask-user:answer"
	AskUserAnswerResultEvent = "rpiv:ask-user:answer-result"
)

// AskUserAnswerRejectCode explains why an answer was refused. Branch on this,
// not on `reason`:
//   - not_found:        no open questionnaire has that toolCallId.
//   - invalid_payload:  a field is missing or has the wrong type.
//   - invalid_answer:   well-formed, but does not fit the questions.
//   - unsupported_host: the questionnaire runs in an RPC host's own dialogs.
type AskUserAnswerRejectCode string

// The reject codes.
const (
	RejectNotFound        AskUserAnswerRejectCode = "not_found"
	RejectInvalidPayload  AskUserAnswerRejectCode = "invalid_payload"
	RejectInvalidAnswer   AskUserAnswerRejectCode = "invalid_answer"
	RejectUnsupportedHost AskUserAnswerRejectCode = "unsupported_host"
)

// PromptQuestion is one question in the prompt event payload.
type PromptQuestion struct {
	Question    string         `json:"question"`
	Header      string         `json:"header"`
	MultiSelect bool           `json:"multiSelect"`
	Options     []PromptOption `json:"options"`
}

// PromptOption is one option in the prompt event payload. The preview content
// itself is not shipped; only the `hasPreview` bit is.
type PromptOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	HasPreview  bool   `json:"hasPreview"`
}

// PromptEventPayload is ASK_USER_PROMPT_EVENT's payload.
type PromptEventPayload struct {
	Questions  []PromptQuestion `json:"questions"`
	ToolCallID *string          `json:"toolCallId,omitempty"`
}

// BlockedEventPayload is ASK_USER_BLOCKED_EVENT's payload: true while input is
// awaited, false when the wait ends (answer, cancel, or error).
type BlockedEventPayload struct {
	Active     bool    `json:"active"`
	ToolCallID *string `json:"toolCallId,omitempty"`
}

// AnswerEntry is one question's answer inside an inbound answer event.
type AnswerEntry struct {
	QuestionIndex int     `json:"questionIndex"`
	OptionIndexes []int   `json:"optionIndexes,omitempty"`
	Text          *string `json:"text,omitempty"`
	Notes         *string `json:"notes,omitempty"`
}

// AnswerEventPayload is ASK_USER_ANSWER_EVENT's payload (inbound).
type AnswerEventPayload struct {
	ToolCallID string        `json:"toolCallId"`
	RequestID  *string       `json:"requestId,omitempty"`
	Answers    []AnswerEntry `json:"answers,omitempty"`
	GlobalNote *string       `json:"globalNote,omitempty"`
	Cancel     *bool         `json:"cancel,omitempty"`
}

// AnswerResultEventPayload is ASK_USER_ANSWER_RESULT_EVENT's payload (outbound
// verdict, one per attempt).
type AnswerResultEventPayload struct {
	OK         bool                     `json:"ok"`
	Code       *AskUserAnswerRejectCode `json:"code,omitempty"`
	Reason     *string                  `json:"reason,omitempty"`
	RequestID  *string                  `json:"requestId,omitempty"`
	ToolCallID *string                  `json:"toolCallId,omitempty"`
}
