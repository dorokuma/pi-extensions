package rpivaskuserquestion

// Ports tool/types.ts (v2.15.0): the parameter schema, the answer/result
// shapes, and the error-code vocabulary. Field names and JSON tags mirror the
// TypeScript object shapes exactly so the wire form (tool results, event
// payloads, session entries) stays byte-compatible with the npm original.

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Question-count and option-count bounds (tool/types.ts:11-14).
const (
	MinQuestions = 1
	MaxQuestions = 4
	MinOptions   = 2
	MaxOptions   = 4
)

// MaxHeaderLength and MaxLabelLength are the schema's hard string caps.
const (
	MaxHeaderLength = 16
	MaxLabelLength  = 60
)

// Sentinel labels for the two runtime rows, sourced from rowIntentMeta.
const (
	SentinelOtherLabel = "Type something."
	SentinelNextLabel  = "Next"
)

// ReservedLabels are the author-facing labels rejected by validation. Order is
// pinned by the TS source: "Other" (CC parity, no runtime kind), then the
// runtime sentinels.
var ReservedLabels = []string{"Other", SentinelOtherLabel, SentinelNextLabel}

// reservedLabelSet is the membership form used by validation.
var reservedLabelSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(ReservedLabels))
	for _, label := range ReservedLabels {
		set[label] = struct{}{}
	}
	return set
}()

// OptionData mirrors OptionSchema's static type. Preview is a pointer so an
// omitted preview stays distinguishable from an empty one (the TS code tests
// `typeof preview === "string" && preview.length > 0`, and `hasPreview` derives
// from that).
type OptionData struct {
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Preview     *string `json:"preview,omitempty"`
}

// previewText returns the option's preview text, or "" when absent.
func (o OptionData) previewText() string {
	if o.Preview == nil {
		return ""
	}
	return *o.Preview
}

// hasPreview mirrors the `hasPreview` derivation used by the prompt event and
// the transcript renderer.
func (o OptionData) hasPreview() bool {
	return o.previewText() != ""
}

// QuestionData mirrors QuestionSchema's static type. MultiSelect is a plain
// bool: the schema default is false, so an absent flag and an explicit false
// are the same value after decoding.
type QuestionData struct {
	Question    string       `json:"question"`
	Header      string       `json:"header"`
	Options     []OptionData `json:"options"`
	MultiSelect bool         `json:"multiSelect"`
}

// QuestionParams mirrors QuestionParamsSchema.
type QuestionParams struct {
	Questions []QuestionData `json:"questions"`
}

// AnswerKind is the answer-intent discriminator (tool/types.ts:113-129).
type AnswerKind string

// The three answer kinds.
const (
	AnswerKindOption AnswerKind = "option"
	AnswerKindCustom AnswerKind = "custom"
	AnswerKindMulti  AnswerKind = "multi"
)

// QuestionAnswer mirrors QuestionAnswer. Selected/Notes/Preview are pointers so
// an absent field is distinguishable from an empty one, matching the TS
// conditional-spread contract (`!("notes" in answer)` still holds).
type QuestionAnswer struct {
	QuestionIndex int        `json:"questionIndex"`
	Question      string     `json:"question"`
	Kind          AnswerKind `json:"kind"`
	Answer        *string    `json:"answer"`
	Selected      []string   `json:"selected,omitempty"`
	Notes         *string    `json:"notes,omitempty"`
	Preview       *string    `json:"preview,omitempty"`
}

// QuestionnaireError is the closed error-code vocabulary.
type QuestionnaireError string

// Every error code the TS union declares.
const (
	ErrorNoUI               QuestionnaireError = "no_ui"
	ErrorNoCustomUI         QuestionnaireError = "no_custom_ui"
	ErrorNoQuestions        QuestionnaireError = "no_questions"
	ErrorEmptyOptions       QuestionnaireError = "empty_options"
	ErrorTooManyQuestions   QuestionnaireError = "too_many_questions"
	ErrorDuplicateQuestion  QuestionnaireError = "duplicate_question"
	ErrorDuplicateOptionLbl QuestionnaireError = "duplicate_option_label"
	ErrorReservedLabel      QuestionnaireError = "reserved_label"
	ErrorSessionLoadFailed  QuestionnaireError = "session_load_failed"
	ErrorStaleModuleCache   QuestionnaireError = "stale_module_cache"
)

// QuestionnaireResult mirrors QuestionnaireResult. Unanswered stays nil for a
// complete or declined result so those stay byte-identical to the TS output.
type QuestionnaireResult struct {
	Answers    []QuestionAnswer    `json:"answers"`
	Cancelled  bool                `json:"cancelled"`
	GlobalNote *string             `json:"globalNote,omitempty"`
	Unanswered []int               `json:"unanswered,omitempty"`
	Error      *QuestionnaireError `json:"error,omitempty"`
}

// IsQuestionnaireResult mirrors isQuestionnaireResult: a value is a result when
// it carries an answers array and a boolean cancelled flag.
func IsQuestionnaireResult(value any) (*QuestionnaireResult, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, false
	}
	rawAnswers, ok := object["answers"].([]any)
	if !ok {
		return nil, false
	}
	cancelled, ok := object["cancelled"].(bool)
	if !ok {
		return nil, false
	}
	result := questionnaireResultFromMap(object)
	result.Answers = answersFromList(rawAnswers)
	result.Cancelled = cancelled
	return result, true
}

// questionnaireResultFromMap reads the optional result fields from a decoded
// object, leaving absent fields nil.
func questionnaireResultFromMap(object map[string]any) *QuestionnaireResult {
	result := &QuestionnaireResult{}
	if note, ok := object["globalNote"].(string); ok {
		result.GlobalNote = &note
	}
	if raw, ok := object["unanswered"].([]any); ok {
		for _, item := range raw {
			if index, ok := jsonInt(item); ok {
				result.Unanswered = append(result.Unanswered, index)
			}
		}
	}
	if code, ok := object["error"].(string); ok {
		errCode := QuestionnaireError(code)
		result.Error = &errCode
	}
	return result
}

// answersFromList decodes a decoded-JSON answers array into QuestionAnswer
// values. Unknown fields are ignored, mirroring the TS structural read.
func answersFromList(raw []any) []QuestionAnswer {
	answers := make([]QuestionAnswer, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok || object == nil {
			continue
		}
		answers = append(answers, questionAnswerFromMap(object))
	}
	return answers
}

// questionAnswerFromMap reads one answer object.
func questionAnswerFromMap(object map[string]any) QuestionAnswer {
	answer := QuestionAnswer{}
	if index, ok := jsonInt(object["questionIndex"]); ok {
		answer.QuestionIndex = index
	}
	if question, ok := object["question"].(string); ok {
		answer.Question = question
	}
	if kind, ok := object["kind"].(string); ok {
		answer.Kind = AnswerKind(kind)
	}
	if text, ok := object["answer"].(string); ok {
		answer.Answer = &text
	}
	if raw, ok := object["selected"].([]any); ok {
		for _, item := range raw {
			if label, ok := item.(string); ok {
				answer.Selected = append(answer.Selected, label)
			}
		}
	}
	if notes, ok := object["notes"].(string); ok {
		answer.Notes = &notes
	}
	if preview, ok := object["preview"].(string); ok {
		answer.Preview = &preview
	}
	return answer
}

// maxSafeInt is the same bound as an integer: JS's Number.isSafeInteger limit.
// It backs the in-process `int` branch of [jsonInt], so a caller that hands a
// QuestionAnswer over programmatically (no JSON in between) is refused exactly
// where a peer sending the same number as JSON float64 would be.
const maxSafeInt = int64(1<<53 - 1)

// maxSafeIntFloat is the largest magnitude a JSON number can carry and still be
// an exactly representable integer — JS's Number.isSafeInteger bound. On 64-bit
// Go, float64(math.MaxInt) rounds UP to 2^63, so a naive `n > math.MaxInt` guard
// lets 2^63 through and `int(n)` then wraps to math.MinInt: an out-of-range index
// instead of a rejection.
const maxSafeIntFloat = float64(1<<53 - 1)

// jsonInt reads a JSON number as an int, accepting the float64 form
// encoding/json produces. The guard is TS's `typeof idx !== "number" ||
// !Number.isInteger(idx)` verbatim: NaN, ±Inf, anything with a fractional part
// and anything beyond the exact-integer range are not integers, and the check
// runs on the float so no out-of-range float→int conversion
// (implementation-defined in Go) can silently produce an index.
func jsonInt(value any) (int, bool) {
	switch n := value.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, false
		}
		if n > maxSafeIntFloat || n < -maxSafeIntFloat {
			return 0, false
		}
		return int(n), true
	case int:
		// Same safety-integer bound as the float64 branch above: math.MaxInt /
		// math.MinInt handed over in-process is as unusable an index as the JSON form
		// that already gets rejected here.
		if int64(n) > maxSafeInt || int64(n) < -maxSafeInt {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// itoa is strconv.Itoa, kept local so config.go needs no import block churn.
func itoa(n int) string { return strconv.Itoa(n) }

// utf8Len is the code-point length, the Go reading of `[...s].length`.
func utf8Len(s string) int { return utf8.RuneCountInString(s) }

// strPtr returns a pointer to s (the terse spread replacement).
func strPtr(s string) *string { return &s }

// QuestionParamsSchema is the JSON Schema of the tool's arguments, mirroring
// tool/types.ts's TypeBox tree field for field, including the descriptions the
// model sees (they are part of the tool contract) and the 1..4 / 2..4 bounds.
func QuestionParamsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type":        "array",
				"minItems":    MinQuestions,
				"maxItems":    MaxQuestions,
				"description": "Questions to ask the user (1-4 questions)",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{
							"type": "string",
							"description": "The complete question to ask the user. Should be clear, specific, and end with a question mark. " +
								"Example: \"Which library should we use for date formatting?\" If multiSelect is true, phrase it accordingly, " +
								"e.g. \"Which features do you want to enable?\"",
						},
						"header": map[string]any{
							"type":      "string",
							"maxLength": MaxHeaderLength,
							"description": "MAX " + itoa(MaxHeaderLength) + " CHARACTERS — hard limit, requests over the limit are rejected. " +
								"Very short chip/tag shown next to the question. Examples: \"Auth method\", \"Library\", \"Approach\".",
						},
						"options": map[string]any{
							"type":     "array",
							"minItems": MinOptions,
							"maxItems": MaxOptions,
							"description": "The available choices for this question. Must have 2-4 options. Each option should be a " +
								"distinct, mutually exclusive choice (unless multiSelect is enabled). The 'Type something.' row is " +
								"appended automatically — do NOT author it.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label": map[string]any{
										"type":      "string",
										"maxLength": MaxLabelLength,
										"description": "MAX " + itoa(MaxLabelLength) + " CHARACTERS — hard limit, requests over the limit are rejected. " +
											"The display text for this option that the user will see and select. Should be concise (1-5 words) " +
											"and clearly describe the choice.",
									},
									"description": map[string]any{
										"type": "string",
										"description": "Explanation of what this option means or what will happen if chosen. Useful for " +
											"providing context about trade-offs or implications.",
									},
									"preview": map[string]any{
										"type": "string",
										"description": "Optional preview content rendered when this option is focused. Use for mockups, code " +
											"snippets, or visual comparisons that help users compare options. See the tool description for " +
											"the expected content format.",
									},
								},
								"required":             []string{"label", "description"},
								"additionalProperties": false,
							},
						},
						"multiSelect": map[string]any{
							"type":    "boolean",
							"default": false,
							"description": "Set to true to allow the user to select multiple options instead of just one. Use when " +
								"choices are not mutually exclusive.",
						},
					},
					"required":             []string{"question", "header", "options"},
					"additionalProperties": false,
				},
			},
		},
		"required": []string{"questions"},
	}
}

// toPromptQuestions shapes params into the event payload's question list,
// mirroring emitAskUserPromptEvent's mapping (content is not shipped; only the
// `hasPreview` bit is).
func toPromptQuestions(params QuestionParams) []PromptQuestion {
	questions := make([]PromptQuestion, 0, len(params.Questions))
	for _, question := range params.Questions {
		options := make([]PromptOption, 0, len(question.Options))
		for _, option := range question.Options {
			options = append(options, PromptOption{
				Label:       option.Label,
				Description: option.Description,
				HasPreview:  option.hasPreview(),
			})
		}
		questions = append(questions, PromptQuestion{
			Question:    question.Question,
			Header:      question.Header,
			MultiSelect: question.MultiSelect,
			Options:     options,
		})
	}
	return questions
}

// labelsOf is a small helper for tests and reducers that need option labels.
func labelsOf(question QuestionData) []string {
	labels := make([]string, 0, len(question.Options))
	for _, option := range question.Options {
		labels = append(labels, option.Label)
	}
	return labels
}

// joinLabels mirrors Array.prototype.join(", ") for the envelope helpers.
func joinLabels(labels []string) string { return strings.Join(labels, ", ") }
