package rpivaskuserquestion

// Ports tool/format-answer.ts (v2.15.0): the scalar answer formatter shared by
// the envelope and the transcript.

// NoInputPlaceholder is the uniform placeholder for empty / null answer text.
const NoInputPlaceholder = "(no input)"

// FormatAnswerVariant mirrors the TS type. `variant` is unused across all
// branches (the chat branch that once distinguished envelope from summary is
// gone); it is retained for signature stability.
type FormatAnswerVariant string

// The two variants.
const (
	FormatVariantSummary  FormatAnswerVariant = "summary"
	FormatVariantEnvelope FormatAnswerVariant = "envelope"
)

// FormatAnswerScalar mirrors formatAnswerScalar:
//
//	multi  → selected labels, then the custom text when non-empty, joined ", ";
//	         the placeholder when nothing is selected.
//	custom → the answer text, or the placeholder when empty.
//	option → the answer label, or the placeholder when null.
func FormatAnswerScalar(answer QuestionAnswer, _ FormatAnswerVariant) string {
	switch answer.Kind {
	case AnswerKindMulti:
		values := make([]string, 0, len(answer.Selected)+1)
		values = append(values, answer.Selected...)
		if answer.Answer != nil && len(*answer.Answer) > 0 {
			values = append(values, *answer.Answer)
		}
		if len(values) > 0 {
			return joinLabels(values)
		}
		return NoInputPlaceholder
	case AnswerKindCustom:
		if answer.Answer != nil && len(*answer.Answer) > 0 {
			return *answer.Answer
		}
		return NoInputPlaceholder
	case AnswerKindOption:
		if answer.Answer != nil {
			return *answer.Answer
		}
		return NoInputPlaceholder
	}
	return NoInputPlaceholder
}
