package rpivaskuserquestion

// Ports tool/external-answer.ts (v2.15.0): turn an inbound
// `rpiv:ask-user:answer` payload (still untrusted) into the QuestionnaireResult
// the dialog would have produced.
//
// Strict by design — the contract can be relaxed later without breaking
// emitters, never tightened. Rejection reasons and their order are
// byte-identical to the TS original because emitters log them.

import (
	"strings"
)

// ExternalAnswerOutcome is the discriminated result of resolveExternalAnswer.
type ExternalAnswerOutcome struct {
	OK     bool
	Result *QuestionnaireResult
	Code   AskUserAnswerRejectCode
	Reason string
}

func rejectAnswer(code AskUserAnswerRejectCode, reason string) ExternalAnswerOutcome {
	return ExternalAnswerOutcome{Code: code, Reason: reason}
}

// noteOf mirrors noteOf: a note as the TUI stores it — trimmed, and absent when
// blank.
func noteOf(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// isRecord mirrors the TS `isRecord` guard: a non-nil, non-array object.
func isRecord(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, false
	}
	return object, true
}

// ResolveExternalAnswer mirrors resolveExternalAnswer.
//
// Answers are shaped as the reducer shapes them — preview attached to a picked
// single-select option, multi-select labels in authored order with custom text
// in `answer`, notes trimmed — so the envelope and `details` are the same
// whether the user typed or a program answered.
//
// `invalid_payload` is a wrong shape or type; `invalid_answer` a well-formed
// answer that does not fit these questions.
func ResolveExternalAnswer(questions []QuestionData, payload map[string]any) ExternalAnswerOutcome {
	cancelRaw, hasCancel := payload["cancel"]
	globalNoteRaw, hasGlobalNote := payload["globalNote"]
	answersRaw, hasAnswers := payload["answers"]

	if hasCancel {
		if _, ok := cancelRaw.(bool); !ok {
			return rejectAnswer(RejectInvalidPayload, "cancel must be a boolean")
		}
	}
	var globalNoteText *string
	if hasGlobalNote {
		text, ok := globalNoteRaw.(string)
		if !ok {
			return rejectAnswer(RejectInvalidPayload, "globalNote must be a string")
		}
		globalNoteText = &text
	}
	note := noteOf(globalNoteText)

	cancelled, _ := cancelRaw.(bool)
	if cancelled {
		if hasAnswers {
			list, isList := answersRaw.([]any)
			if !isList || len(list) != 0 {
				return rejectAnswer(RejectInvalidPayload, "cancel takes no answers")
			}
		}
		result := &QuestionnaireResult{Answers: []QuestionAnswer{}, Cancelled: true, GlobalNote: note}
		return ExternalAnswerOutcome{OK: true, Result: result}
	}

	list, isList := answersRaw.([]any)
	if !hasAnswers || !isList {
		return rejectAnswer(RejectInvalidPayload, "answers must be an array")
	}

	byQuestion := make(map[int]map[string]any, len(list))
	for _, entryRaw := range list {
		entry, ok := isRecord(entryRaw)
		if !ok {
			return rejectAnswer(RejectInvalidPayload, "each answer must be an object")
		}
		indexValue, present := entry["questionIndex"]
		index, isInt := jsonInt(indexValue)
		if !present || !isInt {
			return rejectAnswer(RejectInvalidPayload, "questionIndex must be an integer")
		}
		if index < 0 || index >= len(questions) {
			return rejectAnswer(RejectInvalidAnswer,
				"questionIndex "+itoa(index)+" out of range (0.."+itoa(len(questions)-1)+")")
		}
		if _, duplicate := byQuestion[index]; duplicate {
			return rejectAnswer(RejectInvalidAnswer, "question "+itoa(index)+" answered twice")
		}
		byQuestion[index] = entry
	}

	resolved := make([]QuestionAnswer, 0, len(questions))
	for i := range questions {
		entry, found := byQuestion[i]
		if !found {
			return rejectAnswer(RejectInvalidAnswer,
				`no answer for question `+itoa(i)+` ("`+questions[i].Header+`")`)
		}
		outcome := resolveEntry(questions[i], i, entry)
		if !outcome.OK {
			return ExternalAnswerOutcome{Code: outcome.Code, Reason: outcome.Reason}
		}
		resolved = append(resolved, *outcome.Answer)
	}
	result := &QuestionnaireResult{Answers: resolved, Cancelled: false, GlobalNote: note}
	return ExternalAnswerOutcome{OK: true, Result: result}
}

// entryOutcome is resolveEntry's per-question result.
type entryOutcome struct {
	OK     bool
	Answer *QuestionAnswer
	Code   AskUserAnswerRejectCode
	Reason string
}

// resolveEntry mirrors resolveEntry: validate one question's entry and shape the
// answer the reducer would have built.
func resolveEntry(question QuestionData, index int, entry map[string]any) entryOutcome {
	optionIndexesRaw, hasOptionIndexes := entry["optionIndexes"]
	textRaw, hasText := entry["text"]
	notesRaw, hasNotes := entry["notes"]

	if hasOptionIndexes {
		list, isList := optionIndexesRaw.([]any)
		if !isList {
			return entryReject(RejectInvalidPayload, index, "optionIndexes must be an array of integers")
		}
		for _, item := range list {
			if _, ok := jsonInt(item); !ok {
				return entryReject(RejectInvalidPayload, index, "optionIndexes must be an array of integers")
			}
		}
	}
	if hasText {
		if _, ok := textRaw.(string); !ok {
			return entryReject(RejectInvalidPayload, index, "text must be a string")
		}
	}
	if hasNotes {
		if _, ok := notesRaw.(string); !ok {
			return entryReject(RejectInvalidPayload, index, "notes must be a string")
		}
	}

	picked := make([]int, 0)
	if hasOptionIndexes {
		for _, item := range optionIndexesRaw.([]any) {
			value, _ := jsonInt(item)
			picked = append(picked, value)
		}
	}
	if out, found := firstOutOfRange(picked, len(question.Options)); found {
		return entryReject(RejectInvalidAnswer, index,
			"option index "+itoa(out)+" out of range (0.."+itoa(len(question.Options)-1)+")")
	}
	if hasRepeatedIndex(picked) {
		return entryReject(RejectInvalidAnswer, index, "option index repeated")
	}
	if hasText {
		if strings.TrimSpace(textRaw.(string)) == "" {
			return entryReject(RejectInvalidAnswer, index, "text is blank")
		}
	}
	if len(picked) == 0 && !hasText {
		return entryReject(RejectInvalidAnswer, index, "needs optionIndexes or text")
	}

	base := QuestionAnswer{QuestionIndex: index, Question: question.Question}
	var notes *string
	if hasNotes {
		notes = noteOf(strPtr(notesRaw.(string)))
	}

	if question.MultiSelect {
		selected := make([]string, 0, len(picked))
		for n, option := range question.Options {
			if containsIndex(picked, n) {
				selected = append(selected, option.Label)
			}
		}
		answer := base
		answer.Kind = AnswerKindMulti
		answer.Selected = selected
		if hasText {
			answer.Answer = strPtr(textRaw.(string))
		}
		answer.Notes = notes
		return entryOutcome{OK: true, Answer: &answer}
	}

	if len(picked) > 0 && hasText {
		return entryReject(RejectInvalidAnswer, index, "single-select takes optionIndexes or text, not both")
	}
	if len(picked) > 1 {
		return entryReject(RejectInvalidAnswer, index,
			"single-select takes one option, got "+itoa(len(picked)))
	}
	if hasText {
		answer := base
		answer.Kind = AnswerKindCustom
		answer.Answer = strPtr(textRaw.(string))
		answer.Notes = notes
		return entryOutcome{OK: true, Answer: &answer}
	}
	option := question.Options[picked[0]]
	answer := base
	answer.Kind = AnswerKindOption
	answer.Answer = strPtr(option.Label)
	if option.hasPreview() {
		answer.Preview = strPtr(option.previewText())
	}
	answer.Notes = notes
	return entryOutcome{OK: true, Answer: &answer}
}

func entryReject(code AskUserAnswerRejectCode, index int, reason string) entryOutcome {
	return entryOutcome{Code: code, Reason: "question " + itoa(index) + ": " + reason}
}

func firstOutOfRange(picked []int, count int) (int, bool) {
	for _, n := range picked {
		if n < 0 || n >= count {
			return n, true
		}
	}
	return 0, false
}

func hasRepeatedIndex(picked []int) bool {
	seen := make(map[int]struct{}, len(picked))
	for _, n := range picked {
		if _, duplicate := seen[n]; duplicate {
			return true
		}
		seen[n] = struct{}{}
	}
	return false
}

func containsIndex(picked []int, target int) bool {
	for _, n := range picked {
		if n == target {
			return true
		}
	}
	return false
}
