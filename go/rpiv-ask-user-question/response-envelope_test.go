package rpivaskuserquestion

import (
	"strings"
	"testing"
)

func answer(kind AnswerKind, text string) QuestionAnswer {
	return QuestionAnswer{QuestionIndex: 0, Question: "Q?", Kind: kind, Answer: strPtr(text)}
}

// ── FormatAnswerScalar ──────────────────────────────────────────────────────

func TestFormatAnswerScalar(t *testing.T) {
	cases := []struct {
		name   string
		answer QuestionAnswer
		want   string
	}{
		{
			name:   "option with text",
			answer: answer(AnswerKindOption, "A"),
			want:   "A",
		},
		{
			name:   "option with null answer",
			answer: QuestionAnswer{Kind: AnswerKindOption},
			want:   NoInputPlaceholder,
		},
		{
			name:   "custom with text",
			answer: answer(AnswerKindCustom, "typed"),
			want:   "typed",
		},
		{
			name:   "custom with empty text",
			answer: answer(AnswerKindCustom, ""),
			want:   NoInputPlaceholder,
		},
		{
			name: "multi with selections",
			answer: QuestionAnswer{
				Kind:     AnswerKindMulti,
				Selected: []string{"A", "B"},
			},
			want: "A, B",
		},
		{
			name: "multi with selections and custom",
			answer: QuestionAnswer{
				Kind:     AnswerKindMulti,
				Selected: []string{"A"},
				Answer:   strPtr("plus"),
			},
			want: "A, plus",
		},
		{
			name:   "multi with only custom",
			answer: QuestionAnswer{Kind: AnswerKindMulti, Answer: strPtr("solo")},
			want:   "solo",
		},
		{
			name:   "multi with nothing",
			answer: QuestionAnswer{Kind: AnswerKindMulti},
			want:   NoInputPlaceholder,
		},
		{
			name:   "multi with empty custom and no selections",
			answer: QuestionAnswer{Kind: AnswerKindMulti, Answer: strPtr("")},
			want:   NoInputPlaceholder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatAnswerScalar(tc.answer, FormatVariantEnvelope); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			// The variant is retained but unused: both variants agree.
			if got := FormatAnswerScalar(tc.answer, FormatVariantSummary); got != tc.want {
				t.Fatalf("summary variant got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoInputPlaceholderValue(t *testing.T) {
	if NoInputPlaceholder != "(no input)" {
		t.Fatalf("placeholder = %q", NoInputPlaceholder)
	}
}

// ── BuildToolResult ─────────────────────────────────────────────────────────

func TestBuildToolResultShape(t *testing.T) {
	result := BuildToolResult("hello", QuestionnaireResult{Answers: []QuestionAnswer{}})
	if len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text != "hello" {
		t.Fatalf("content = %#v", result.Content)
	}
	if result.Details.Cancelled {
		t.Fatal("details.cancelled = true")
	}
}

// ── BuildQuestionnaireResponse ──────────────────────────────────────────────

func TestBuildQuestionnaireResponseDeclines(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{twoOptions("H", "Q?")}}

	t.Run("nil result", func(t *testing.T) {
		result := BuildQuestionnaireResponse(nil, params)
		if result.Content[0].Text != DeclineMessage {
			t.Fatalf("text = %q", result.Content[0].Text)
		}
		if !result.Details.Cancelled {
			t.Fatal("cancelled = false")
		}
		if result.Details.Answers == nil || len(result.Details.Answers) != 0 {
			t.Fatalf("answers = %#v", result.Details.Answers)
		}
	})

	t.Run("cancelled result keeps its answers and note in details", func(t *testing.T) {
		note := "note"
		result := BuildQuestionnaireResponse(&QuestionnaireResult{
			Answers:    []QuestionAnswer{answer(AnswerKindOption, "A")},
			Cancelled:  true,
			GlobalNote: &note,
		}, params)
		if result.Content[0].Text != DeclineMessage {
			t.Fatalf("decline text changed: %q", result.Content[0].Text)
		}
		if len(result.Details.Answers) != 1 {
			t.Fatalf("answers = %#v", result.Details.Answers)
		}
		if result.Details.GlobalNote == nil || *result.Details.GlobalNote != "note" {
			t.Fatalf("globalNote = %v", result.Details.GlobalNote)
		}
	})

	t.Run("cancelled with an empty note omits it", func(t *testing.T) {
		empty := ""
		result := BuildQuestionnaireResponse(&QuestionnaireResult{Cancelled: true, GlobalNote: &empty}, params)
		if result.Details.GlobalNote != nil {
			t.Fatalf("globalNote = %v", result.Details.GlobalNote)
		}
	})
}

func TestBuildQuestionnaireResponseAnswered(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{twoOptions("Auth", "Which auth?")}}
	result := BuildQuestionnaireResponse(&QuestionnaireResult{
		Answers: []QuestionAnswer{{
			QuestionIndex: 0,
			Question:      "Which auth?",
			Kind:          AnswerKindOption,
			Answer:        strPtr("A"),
		}},
	}, params)

	want := `User has answered your questions: "Which auth?"="A". You can now continue with the user's answers in mind.`
	if result.Content[0].Text != want {
		t.Fatalf("text =\n%q\nwant\n%q", result.Content[0].Text, want)
	}
}

func TestBuildQuestionnaireResponseIncludesPreviewAndNotes(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{twoOptions("Auth", "Which auth?")}}
	result := BuildQuestionnaireResponse(&QuestionnaireResult{
		Answers: []QuestionAnswer{{
			QuestionIndex: 0,
			Question:      "Which auth?",
			Kind:          AnswerKindOption,
			Answer:        strPtr("A"),
			Preview:       strPtr("code"),
			Notes:         strPtr("my note"),
		}},
	}, params)

	want := `User has answered your questions: "Which auth?"="A". selected preview: code. user notes: my note. You can now continue with the user's answers in mind.`
	if result.Content[0].Text != want {
		t.Fatalf("text =\n%q\nwant\n%q", result.Content[0].Text, want)
	}
}

func TestBuildQuestionnaireResponseMultiQuestionOrder(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{
		twoOptions("A", "first?"),
		twoOptions("B", "second?"),
	}}
	// Out-of-order answers are emitted in question order.
	result := BuildQuestionnaireResponse(&QuestionnaireResult{
		Answers: []QuestionAnswer{
			{QuestionIndex: 1, Question: "second?", Kind: AnswerKindOption, Answer: strPtr("B")},
			{QuestionIndex: 0, Question: "first?", Kind: AnswerKindOption, Answer: strPtr("A")},
		},
	}, params)
	want := `User has answered your questions: "first?"="A". "second?"="B". You can now continue with the user's answers in mind.`
	if result.Content[0].Text != want {
		t.Fatalf("text =\n%q\nwant\n%q", result.Content[0].Text, want)
	}
}

func TestBuildQuestionnaireResponsePartialSubmit(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{
		twoOptions("A", "first?"),
		twoOptions("B", "second?"),
	}}
	result := BuildQuestionnaireResponse(&QuestionnaireResult{
		Answers: []QuestionAnswer{
			{QuestionIndex: 1, Question: "second?", Kind: AnswerKindOption, Answer: strPtr("B")},
		},
	}, params)

	want := `User has answered your questions: "second?"="B". unanswered: "first?". You can now continue with the user's answers in mind.`
	if result.Content[0].Text != want {
		t.Fatalf("text =\n%q\nwant\n%q", result.Content[0].Text, want)
	}
	if len(result.Details.Unanswered) != 1 || result.Details.Unanswered[0] != 0 {
		t.Fatalf("details.unanswered = %v", result.Details.Unanswered)
	}
}

func TestBuildQuestionnaireResponseGlobalNote(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{twoOptions("A", "first?")}}

	t.Run("note with no answers still yields the answered envelope", func(t *testing.T) {
		result := BuildQuestionnaireResponse(&QuestionnaireResult{
			Answers:    []QuestionAnswer{},
			GlobalNote: strPtr("just a note"),
		}, params)
		want := `User has answered your questions: unanswered: "first?". global note: just a note. You can now continue with the user's answers in mind.`
		if result.Content[0].Text != want {
			t.Fatalf("text =\n%q\nwant\n%q", result.Content[0].Text, want)
		}
	})

	t.Run("note rides after the answer segments", func(t *testing.T) {
		result := BuildQuestionnaireResponse(&QuestionnaireResult{
			Answers:    []QuestionAnswer{{QuestionIndex: 0, Question: "first?", Kind: AnswerKindCustom, Answer: strPtr("x")}},
			GlobalNote: strPtr("after"),
		}, params)
		if !strings.Contains(result.Content[0].Text, `"first?"="x". global note: after.`) {
			t.Fatalf("text = %q", result.Content[0].Text)
		}
	})
}

func TestBuildQuestionnaireResponseNoAnswersNoNoteDeclines(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{twoOptions("A", "first?")}}
	result := BuildQuestionnaireResponse(&QuestionnaireResult{Answers: []QuestionAnswer{}}, params)
	if result.Content[0].Text != DeclineMessage {
		t.Fatalf("text = %q", result.Content[0].Text)
	}
	if !result.Details.Cancelled {
		t.Fatal("no-segment result should be presented as declined")
	}
}

func TestBuildQuestionnaireResponseFullyAnsweredHasNoUnanswered(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{twoOptions("A", "first?")}}
	result := BuildQuestionnaireResponse(&QuestionnaireResult{
		Answers: []QuestionAnswer{{QuestionIndex: 0, Question: "first?", Kind: AnswerKindOption, Answer: strPtr("A")}},
	}, params)
	if result.Details.Unanswered != nil {
		t.Fatalf("unanswered = %v (want nil for byte-identical output)", result.Details.Unanswered)
	}
	if strings.Contains(result.Content[0].Text, "unanswered:") {
		t.Fatalf("text = %q", result.Content[0].Text)
	}
}

func TestBuildAnswerSegment(t *testing.T) {
	segment := BuildAnswerSegment(QuestionAnswer{
		QuestionIndex: 0,
		Question:      "Q?",
		Kind:          AnswerKindMulti,
		Selected:      []string{"A", "B"},
		Answer:        strPtr("custom"),
		Notes:         strPtr("note"),
	})
	if want := `"Q?"="A, B, custom". user notes: note.`; segment != want {
		t.Fatalf("segment = %q, want %q", segment, want)
	}

	// Empty preview/notes strings are dropped.
	empty := ""
	plain := BuildAnswerSegment(QuestionAnswer{
		Question: "Q?",
		Kind:     AnswerKindOption,
		Answer:   strPtr("A"),
		Preview:  &empty,
		Notes:    &empty,
	})
	if want := `"Q?"="A".`; plain != want {
		t.Fatalf("segment = %q, want %q", plain, want)
	}
}

func TestBuildUnansweredSegment(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{
		twoOptions("A", "first?"),
		twoOptions("B", "second?"),
	}}
	if got := BuildUnansweredSegment([]int{0, 1}, params); got != `unanswered: "first?", "second?".` {
		t.Fatalf("segment = %q", got)
	}
	// An out-of-range index renders as an empty question rather than panicking.
	if got := BuildUnansweredSegment([]int{9}, params); got != `unanswered: "".` {
		t.Fatalf("segment = %q", got)
	}
}

func TestEnvelopeCopyConstants(t *testing.T) {
	if DeclineMessage != "User declined to answer questions" {
		t.Fatalf("decline = %q", DeclineMessage)
	}
	if EnvelopePrefix != "User has answered your questions:" {
		t.Fatalf("prefix = %q", EnvelopePrefix)
	}
	if EnvelopeSuffix != "You can now continue with the user's answers in mind." {
		t.Fatalf("suffix = %q", EnvelopeSuffix)
	}
}
