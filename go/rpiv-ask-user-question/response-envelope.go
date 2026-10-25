package rpivaskuserquestion

// Ports tool/response-envelope.ts (v2.15.0): the LLM-facing envelope builder.

import "strings"

// Envelope copy, verbatim from the TS constants.
const (
	DeclineMessage = "User declined to answer questions"
	EnvelopePrefix = "User has answered your questions:"
	EnvelopeSuffix = "You can now continue with the user's answers in mind."
)

// ToolContent is one content block of a tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResult is the tool's return shape: one text block plus the structured
// details the renderer and replay consumers read.
type ToolResult struct {
	Content []ToolContent       `json:"content"`
	Details QuestionnaireResult `json:"details"`
}

// BuildToolResult mirrors buildToolResult.
func BuildToolResult(text string, details QuestionnaireResult) ToolResult {
	return ToolResult{
		Content: []ToolContent{{Type: "text", Text: text}},
		Details: details,
	}
}

// BuildQuestionnaireResponse mirrors buildQuestionnaireResponse: map a
// QuestionnaireResult to the LLM-facing envelope.
//
// Cancelled and "no segments" both fall to DeclineMessage so the model sees one
// canonical "didn't answer" signal. The global-note segment is pushed before the
// zero-segments check, so a note-bearing submit with zero answers still yields
// the answered envelope. A partial submit names the questions left blank in an
// `unanswered:` segment (and details.unanswered) so the model can tell
// "not decided yet" from "settled" (#291).
func BuildQuestionnaireResponse(result *QuestionnaireResult, params QuestionParams) ToolResult {
	if result == nil || result.Cancelled {
		details := QuestionnaireResult{Cancelled: true}
		if result != nil {
			details.Answers = result.Answers
			if result.GlobalNote != nil && len(*result.GlobalNote) > 0 {
				details.GlobalNote = result.GlobalNote
			}
		}
		if details.Answers == nil {
			details.Answers = []QuestionAnswer{}
		}
		return BuildToolResult(DeclineMessage, details)
	}

	segments := make([]string, 0, len(params.Questions)+2)
	unanswered := make([]int, 0)
	for i := range params.Questions {
		answer, found := findAnswer(result.Answers, i)
		if found {
			segments = append(segments, BuildAnswerSegment(answer))
			continue
		}
		unanswered = append(unanswered, i)
	}

	hasGlobalNote := result.GlobalNote != nil && len(*result.GlobalNote) > 0
	if len(segments) == 0 && !hasGlobalNote {
		return BuildToolResult(DeclineMessage, QuestionnaireResult{
			Answers:   result.Answers,
			Cancelled: true,
		})
	}
	if len(unanswered) > 0 {
		segments = append(segments, BuildUnansweredSegment(unanswered, params))
	}
	if hasGlobalNote {
		segments = append(segments, "global note: "+*result.GlobalNote+".")
	}

	details := *result
	if len(unanswered) > 0 {
		details.Unanswered = unanswered
	}
	text := EnvelopePrefix + " " + strings.Join(segments, " ") + " " + EnvelopeSuffix
	return BuildToolResult(text, details)
}

// findAnswer returns the answer for questionIndex, first match wins (the TS
// `answers.find(x => x.questionIndex === i)`).
func findAnswer(answers []QuestionAnswer, questionIndex int) (QuestionAnswer, bool) {
	for _, answer := range answers {
		if answer.QuestionIndex == questionIndex {
			return answer, true
		}
	}
	return QuestionAnswer{}, false
}

// BuildUnansweredSegment mirrors buildUnansweredSegment:
// `unanswered: "<question>", "<question>".`
func BuildUnansweredSegment(indexes []int, params QuestionParams) string {
	quoted := make([]string, 0, len(indexes))
	for _, index := range indexes {
		text := ""
		if index >= 0 && index < len(params.Questions) {
			text = params.Questions[index].Question
		}
		quoted = append(quoted, `"`+text+`"`)
	}
	return "unanswered: " + strings.Join(quoted, ", ") + "."
}

// BuildAnswerSegment mirrors buildAnswerSegment: the `"Q"="A"` shape plus the
// optional `selected preview:` / `user notes:` suffixes.
func BuildAnswerSegment(answer QuestionAnswer) string {
	parts := []string{`"` + answer.Question + `"="` + FormatAnswerScalar(answer, FormatVariantEnvelope) + `"`}
	if answer.Preview != nil && len(*answer.Preview) > 0 {
		parts = append(parts, "selected preview: "+*answer.Preview)
	}
	if answer.Notes != nil && len(*answer.Notes) > 0 {
		parts = append(parts, "user notes: "+*answer.Notes)
	}
	return strings.Join(parts, ". ") + "."
}
