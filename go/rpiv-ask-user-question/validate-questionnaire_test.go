package rpivaskuserquestion

import (
	"testing"
)

// helper builds a question with n options whose labels are "A", "B", …
func question(header, text string, labels ...string) QuestionData {
	options := make([]OptionData, 0, len(labels))
	for _, label := range labels {
		options = append(options, OptionData{Label: label, Description: "desc " + label})
	}
	return QuestionData{Question: text, Header: header, Options: options}
}

func twoOptions(header, text string) QuestionData {
	return question(header, text, "A", "B")
}

func TestValidateQuestionnaireAcceptsWellFormedParams(t *testing.T) {
	result := validateQuestionnaire(QuestionParams{Questions: []QuestionData{twoOptions("H", "Q?")}})
	if !result.OK {
		t.Fatalf("expected ok, got %+v", result)
	}
}

func TestValidateQuestionnaireRejectsViolationsInOrder(t *testing.T) {
	cases := []struct {
		name    string
		params  QuestionParams
		code    QuestionnaireError
		message string
	}{
		{
			name:    "no questions",
			params:  QuestionParams{},
			code:    ErrorNoQuestions,
			message: ErrorMsgNoQuestions,
		},
		{
			name: "too many questions",
			params: QuestionParams{Questions: []QuestionData{
				twoOptions("A", "Q1?"), twoOptions("B", "Q2?"), twoOptions("C", "Q3?"),
				twoOptions("D", "Q4?"), twoOptions("E", "Q5?"),
			}},
			code:    ErrorTooManyQuestions,
			message: ErrorMsgTooManyQuestions,
		},
		{
			name: "duplicate question text",
			params: QuestionParams{Questions: []QuestionData{
				twoOptions("A", "same?"), twoOptions("B", "same?"),
			}},
			code:    ErrorDuplicateQuestion,
			message: ErrorMsgDuplicateQuestion,
		},
		{
			name: "too few options",
			params: QuestionParams{Questions: []QuestionData{
				question("H", "Q?", "only"),
			}},
			code:    ErrorEmptyOptions,
			message: ErrorMsgTooFewOptions,
		},
		{
			name: "reserved label Other",
			params: QuestionParams{Questions: []QuestionData{
				question("H", "Q?", "Other", "B"),
			}},
			code:    ErrorReservedLabel,
			message: ErrorMsgReservedLabel,
		},
		{
			name: "reserved Type something",
			params: QuestionParams{Questions: []QuestionData{
				question("H", "Q?", "Type something.", "B"),
			}},
			code:    ErrorReservedLabel,
			message: ErrorMsgReservedLabel,
		},
		{
			name: "reserved Next",
			params: QuestionParams{Questions: []QuestionData{
				question("H", "Q?", "A", "Next"),
			}},
			code:    ErrorReservedLabel,
			message: ErrorMsgReservedLabel,
		},
		{
			name: "duplicate option label",
			params: QuestionParams{Questions: []QuestionData{
				question("H", "Q?", "A", "A"),
			}},
			code:    ErrorDuplicateOptionLbl,
			message: ErrorMsgDuplicateLabel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := validateQuestionnaire(tc.params)
			if result.OK {
				t.Fatal("expected failure, got ok")
			}
			if result.Error != tc.code {
				t.Fatalf("code = %q, want %q", result.Error, tc.code)
			}
			if result.Message != tc.message {
				t.Fatalf("message = %q, want %q", result.Message, tc.message)
			}
		})
	}
}

// reserved_label MUST short-circuit before duplicate_option_label.
func TestValidateQuestionnaireReservedBeatsDuplicate(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{
		question("H", "Q?", "Next", "Next"),
	}}
	result := validateQuestionnaire(params)
	if result.OK || result.Error != ErrorReservedLabel {
		t.Fatalf("expected reserved_label, got %+v", result)
	}
}

// Question duplication is checked across ALL questions before any per-question
// option guard runs.
func TestValidateQuestionnaireDuplicateQuestionPrecedesOptionGuards(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{
		{Question: "same?", Header: "A", Options: []OptionData{{Label: "one"}}},
		{Question: "same?", Header: "B", Options: []OptionData{{Label: "Other"}, {Label: "B"}}},
	}}
	result := validateQuestionnaire(params)
	if result.Error != ErrorDuplicateQuestion {
		t.Fatalf("expected duplicate_question, got %+v", result)
	}
}

func TestValidateQuestionnaireBounds(t *testing.T) {
	// Exactly MAX_OPTIONS is accepted; one more is not representable, so assert
	// the boundary at four.
	ok := validateQuestionnaire(QuestionParams{Questions: []QuestionData{
		question("H", "Q?", "A", "B", "C", "D"),
	}})
	if !ok.OK {
		t.Fatalf("four options rejected: %+v", ok)
	}
	// Exactly four questions is accepted.
	var questions []QuestionData
	for i, header := range []string{"A", "B", "C", "D"} {
		questions = append(questions, twoOptions(header, "Q"+itoa(i)+"?"))
	}
	if result := validateQuestionnaire(QuestionParams{Questions: questions}); !result.OK {
		t.Fatalf("four questions rejected: %+v", result)
	}
}

// Reserved matching is exact and English-locked: a localized label must not be
// rejected, and a case variant is not the reserved string.
func TestValidateQuestionnaireReservedLabelsAreExactAndEnglish(t *testing.T) {
	for _, label := range []string{"other", "NEXT", "Type Something.", "Next "} {
		params := QuestionParams{Questions: []QuestionData{question("H", "Q?", label, "B")}}
		if result := validateQuestionnaire(params); !result.OK {
			t.Fatalf("label %q wrongly rejected: %+v", label, result)
		}
	}
}

// A localized display label must never widen the reserved set.
func TestReservedLabelSetIgnoresLocalization(t *testing.T) {
	previous := LocaleSource
	LocaleSource = func() string { return "zh" }
	defer func() { LocaleSource = previous }()

	if got := DisplayLabel(SentinelOther); got != "输入内容" {
		t.Fatalf("localized label = %q", got)
	}
	// The reserved set still holds only the English strings.
	if _, present := reservedLabelSet["输入内容"]; present {
		t.Fatal("localized label leaked into the reserved set")
	}
	if _, present := reservedLabelSet[SentinelOtherLabel]; !present {
		t.Fatal("English sentinel missing from the reserved set")
	}
}

func TestReservedLabelsOrderIsPinned(t *testing.T) {
	want := []string{"Other", "Type something.", "Next"}
	if len(ReservedLabels) != len(want) {
		t.Fatalf("ReservedLabels = %v", ReservedLabels)
	}
	for i := range want {
		if ReservedLabels[i] != want[i] {
			t.Fatalf("ReservedLabels[%d] = %q, want %q", i, ReservedLabels[i], want[i])
		}
	}
	if ErrorMsgReservedLabel != "Error: Option label is reserved (Other, Type something., Next)" {
		t.Fatalf("reserved message = %q", ErrorMsgReservedLabel)
	}
}

// multiSelect + preview is NOT a runtime rejection: the schema ships `preview` on
// every option and validate-questionnaire.ts has no guard for the pairing, so a
// question that asks for both validates (the preview is ignored downstream).
func TestValidateQuestionnaireAcceptsMultiSelectWithPreview(t *testing.T) {
	preview := "```go\ncode\n```"
	multi := question("H", "Q?", "A", "B")
	multi.MultiSelect = true
	multi.Options[0].Preview = &preview
	// An empty preview string is not a preview either way; both forms pass.
	blank := ""
	multi.Options[1].Preview = &blank

	if result := validateQuestionnaire(QuestionParams{Questions: []QuestionData{multi}}); !result.OK {
		t.Fatalf("multiSelect + preview rejected: %+v", result)
	}

	// Single-select with previews validates the same way.
	single := question("H", "Q?", "A", "B")
	single.Options[1].Preview = &preview
	if result := validateQuestionnaire(QuestionParams{Questions: []QuestionData{single}}); !result.OK {
		t.Fatalf("single-select preview rejected: %+v", result)
	}
}
