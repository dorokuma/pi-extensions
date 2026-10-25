package rpivaskuserquestion

import (
	"reflect"
	"strings"
	"testing"
)

// ── normalizeLineTerminators / normalizeQuestionParams ──────────────────────

func TestNormalizeLineTerminators(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"crlf becomes lf", "a\r\nb", "a\nb"},
		{"lone cr is deleted", "GEMBA\r_LOG\r_FILE", "GEMBA_LOG_FILE"},
		{"cr at end is deleted", "tail\r", "tail"},
		{"plain text untouched", "no terminators", "no terminators"},
		{"lf untouched", "a\nb", "a\nb"},
		{"mixed", "a\r\nb\rc\nd", "a\nbc\nd"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeLineTerminators(tc.in); got != tc.want {
				t.Fatalf("normalizeLineTerminators(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeQuestionParamsNormalizesEveryUserFacingField(t *testing.T) {
	preview := "line1\r\nline2\rline3"
	params := QuestionParams{Questions: []QuestionData{{
		Question: "Q\r\nwith CR",
		Header:   "H\rder",
		Options: []OptionData{
			{Label: "La\rbel", Description: "De\rs\rc", Preview: &preview},
			{Label: "Other\r", Description: "plain"},
		},
	}}}

	got := normalizeQuestionParams(params)

	if got.Questions[0].Question != "Q\nwith CR" {
		t.Fatalf("question = %q", got.Questions[0].Question)
	}
	if got.Questions[0].Header != "Hder" {
		t.Fatalf("header = %q", got.Questions[0].Header)
	}
	if got.Questions[0].Options[0].Label != "Label" {
		t.Fatalf("label = %q", got.Questions[0].Options[0].Label)
	}
	if got.Questions[0].Options[0].Description != "Desc" {
		t.Fatalf("description = %q", got.Questions[0].Options[0].Description)
	}
	if got.Questions[0].Options[0].Preview == nil || *got.Questions[0].Options[0].Preview != "line1\nline2line3" {
		t.Fatalf("preview = %v", got.Questions[0].Options[0].Preview)
	}
	// A normalized "\r" in a label must not resurrect the reserved sentinel.
	if got.Questions[0].Options[1].Label != "Other" {
		t.Fatalf("label = %q", got.Questions[0].Options[1].Label)
	}
	// An absent preview stays absent.
	if got.Questions[0].Options[1].Preview != nil {
		t.Fatalf("absent preview became %v", got.Questions[0].Options[1].Preview)
	}
}

func TestNormalizeQuestionParamsIsPure(t *testing.T) {
	original := "Q\r\nX"
	params := QuestionParams{Questions: []QuestionData{{
		Question: original,
		Options:  []OptionData{{Label: "A\r", Description: "d"}, {Label: "B", Description: "e"}},
	}}}
	_ = normalizeQuestionParams(params)
	if params.Questions[0].Question != original {
		t.Fatalf("input mutated: %q", params.Questions[0].Question)
	}
	if params.Questions[0].Options[0].Label != "A\r" {
		t.Fatalf("input option mutated: %q", params.Questions[0].Options[0].Label)
	}
}

// ── PrepareQuestionArguments (#244) ─────────────────────────────────────────

func TestPrepareQuestionArgumentsDecodesDoubleEncodedShapes(t *testing.T) {
	questionJSON := `{"question":"Q?","header":"H","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}`

	t.Run("questions as a JSON string", func(t *testing.T) {
		got, err := PrepareQuestionArguments(map[string]any{"questions": "[" + questionJSON + "]"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		questions, ok := got["questions"].([]any)
		if !ok || len(questions) != 1 {
			t.Fatalf("questions = %#v", got["questions"])
		}
		object := questions[0].(map[string]any)
		if object["header"] != "H" {
			t.Fatalf("decoded question = %#v", object)
		}
	})

	t.Run("top-level element as a JSON string", func(t *testing.T) {
		got, err := PrepareQuestionArguments(map[string]any{"questions": []any{questionJSON}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		questions := got["questions"].([]any)
		object, ok := questions[0].(map[string]any)
		if !ok || object["question"] != "Q?" {
			t.Fatalf("decoded element = %#v", questions[0])
		}
	})

	t.Run("options as a JSON string", func(t *testing.T) {
		got, err := PrepareQuestionArguments(map[string]any{"questions": []any{map[string]any{
			"question": "Q?",
			"options":  `[{"label":"A","description":"a"},{"label":"B","description":"b"}]`,
		}}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		object := got["questions"].([]any)[0].(map[string]any)
		options, ok := object["options"].([]any)
		if !ok || len(options) != 2 {
			t.Fatalf("decoded options = %#v", object["options"])
		}
		if options[0].(map[string]any)["label"] != "A" {
			t.Fatalf("option = %#v", options[0])
		}
	})

	t.Run("well-formed args pass through", func(t *testing.T) {
		input := map[string]any{"questions": []any{map[string]any{"question": "Q?"}}}
		got, err := PrepareQuestionArguments(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, input) {
			t.Fatalf("well-formed args changed: %#v", got)
		}
	})

	t.Run("unparseable string passes through for validation to report", func(t *testing.T) {
		got, err := PrepareQuestionArguments(map[string]any{"questions": "not json"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["questions"] != "not json" {
			t.Fatalf("string changed: %#v", got["questions"])
		}
	})

	t.Run("string decoding to a non-array passes through", func(t *testing.T) {
		got, err := PrepareQuestionArguments(map[string]any{"questions": `{"a":1}`})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["questions"] != `{"a":1}` {
			t.Fatalf("string changed: %#v", got["questions"])
		}
	})
}

// ── decodeQuestionParams ────────────────────────────────────────────────────

func TestDecodeQuestionParamsToleratesMissingFields(t *testing.T) {
	params := decodeQuestionParams(map[string]any{
		"questions": []any{
			map[string]any{
				"question": "Q1?",
				"options": []any{
					map[string]any{"label": "A", "description": "a"},
					map[string]any{"label": "B", "description": "b", "preview": "code"},
				},
			},
			"not an object",
		},
	})
	if len(params.Questions) != 2 {
		t.Fatalf("questions = %d", len(params.Questions))
	}
	if params.Questions[0].Header != "" || params.Questions[0].MultiSelect {
		t.Fatalf("missing fields decoded as %#v", params.Questions[0])
	}
	if params.Questions[0].Options[0].Preview != nil {
		t.Fatalf("absent preview decoded as %v", params.Questions[0].Options[0].Preview)
	}
	if params.Questions[0].Options[1].Preview == nil || *params.Questions[0].Options[1].Preview != "code" {
		t.Fatalf("preview decoded as %v", params.Questions[0].Options[1].Preview)
	}
}

func TestDecodeQuestionParamsMissingQuestions(t *testing.T) {
	params := decodeQuestionParams(map[string]any{})
	if len(params.Questions) != 0 {
		t.Fatalf("questions = %d", len(params.Questions))
	}
}

// ── multiSelect string coercion lives in the schema defaults ────────────────

func TestDecodeQuestionParamsReadsMultiSelectFlag(t *testing.T) {
	params := decodeQuestionParams(map[string]any{"questions": []any{map[string]any{
		"question":    "Q?",
		"multiSelect": true,
		"options":     []any{map[string]any{"label": "A", "description": "a"}},
	}}})
	if !params.Questions[0].MultiSelect {
		t.Fatal("multiSelect not read")
	}
}

// ── Schema ─────────────────────────────────────────────────────────────────

func TestQuestionParamsSchemaBounds(t *testing.T) {
	schema := QuestionParamsSchema()
	questions := schema["properties"].(map[string]any)["questions"].(map[string]any)
	if questions["minItems"] != MinQuestions || questions["maxItems"] != MaxQuestions {
		t.Fatalf("questions bounds = %v/%v", questions["minItems"], questions["maxItems"])
	}
	item := questions["items"].(map[string]any)
	options := item["properties"].(map[string]any)["options"].(map[string]any)
	if options["minItems"] != MinOptions || options["maxItems"] != MaxOptions {
		t.Fatalf("options bounds = %v/%v", options["minItems"], options["maxItems"])
	}
	header := item["properties"].(map[string]any)["header"].(map[string]any)
	if header["maxLength"] != MaxHeaderLength {
		t.Fatalf("header maxLength = %v", header["maxLength"])
	}
	label := options["items"].(map[string]any)["properties"].(map[string]any)["label"].(map[string]any)
	if label["maxLength"] != MaxLabelLength {
		t.Fatalf("label maxLength = %v", label["maxLength"])
	}
	multiSelect := item["properties"].(map[string]any)["multiSelect"].(map[string]any)
	if multiSelect["default"] != false {
		t.Fatalf("multiSelect default = %v", multiSelect["default"])
	}
	required := item["required"].([]string)
	if !containsString(required, "question") || !containsString(required, "header") || !containsString(required, "options") {
		t.Fatalf("required = %v", required)
	}
	if required := options["items"].(map[string]any)["required"].([]string); !containsString(required, "label") {
		t.Fatalf("option required = %v", required)
	}
}

func TestSchemaDescriptionsMatchTSWording(t *testing.T) {
	schema := QuestionParamsSchema()
	questions := schema["properties"].(map[string]any)["questions"].(map[string]any)
	if got := questions["description"]; got != "Questions to ask the user (1-4 questions)" {
		t.Fatalf("questions description = %q", got)
	}
	header := questions["items"].(map[string]any)["properties"].(map[string]any)["header"].(map[string]any)
	if !strings.HasPrefix(header["description"].(string), "MAX 16 CHARACTERS — hard limit") {
		t.Fatalf("header description = %q", header["description"])
	}
	label := questions["items"].(map[string]any)["properties"].(map[string]any)["options"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["label"].(map[string]any)
	if !strings.HasPrefix(label["description"].(string), "MAX 60 CHARACTERS — hard limit") {
		t.Fatalf("label description = %q", label["description"])
	}
}
