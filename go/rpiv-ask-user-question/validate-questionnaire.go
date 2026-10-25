package rpivaskuserquestion

// Ports tool/validate-questionnaire.ts (v2.15.0): the pure runtime validator.
// Covers every guard except `no_ui` (which depends on ctx.hasUI and stays at
// the call site). Message wording is byte-identical to the TS constants, and so is
// the guard set: reserved-label and duplicate-label matching are exact,
// case-sensitive comparisons, and a multiSelect question that carries option
// previews is accepted.

// Validation failure messages, verbatim from validate-questionnaire.ts.
const (
	ErrorMsgNoQuestions       = "Error: At least one question is required"
	ErrorMsgTooManyQuestions  = "Error: At most " + "4" + " questions are allowed per invocation"
	ErrorMsgDuplicateQuestion = "Error: Question text must be unique within an invocation"
	ErrorMsgTooFewOptions     = "Error: Each question requires at least " + "2" + " options"
	ErrorMsgReservedLabel     = "Error: Option label is reserved (Other, Type something., Next)"
	ErrorMsgDuplicateLabel    = "Error: Option labels must be unique within a question"
)

// ValidationResult is the discriminated result of validateQuestionnaire.
type ValidationResult struct {
	OK      bool
	Error   QuestionnaireError
	Message string
}

// validateQuestionnaire mirrors validateQuestionnaire. `reserved_label` MUST
// short-circuit before `duplicate_option_label`, and question duplication is
// checked across all questions before any per-question option guard runs.
func validateQuestionnaire(params QuestionParams) ValidationResult {
	if len(params.Questions) == 0 {
		return ValidationResult{Error: ErrorNoQuestions, Message: ErrorMsgNoQuestions}
	}
	if len(params.Questions) > MaxQuestions {
		return ValidationResult{Error: ErrorTooManyQuestions, Message: ErrorMsgTooManyQuestions}
	}

	seenQuestions := make(map[string]struct{}, len(params.Questions))
	for _, question := range params.Questions {
		if _, duplicate := seenQuestions[question.Question]; duplicate {
			return ValidationResult{Error: ErrorDuplicateQuestion, Message: ErrorMsgDuplicateQuestion}
		}
		seenQuestions[question.Question] = struct{}{}
	}

	for _, question := range params.Questions {
		if len(question.Options) < MinOptions {
			return ValidationResult{Error: ErrorEmptyOptions, Message: ErrorMsgTooFewOptions}
		}
		seenLabels := make(map[string]struct{}, len(question.Options))
		for _, option := range question.Options {
			if _, reserved := reservedLabelSet[option.Label]; reserved {
				return ValidationResult{Error: ErrorReservedLabel, Message: ErrorMsgReservedLabel}
			}
			if _, duplicate := seenLabels[option.Label]; duplicate {
				return ValidationResult{Error: ErrorDuplicateOptionLbl, Message: ErrorMsgDuplicateLabel}
			}
			seenLabels[option.Label] = struct{}{}
		}
	}

	return ValidationResult{OK: true}
}

// A multiSelect question may carry option previews: neither validateQuestionnaire
// nor execute rejects that pairing — the schema ships `preview` on every option.
// It is ignored downstream instead: PreviewPane renders the plain option list for a
// multiSelect question, and the reducer attaches a preview only to a single-select
// option answer. So there is no runtime guard to add here.
