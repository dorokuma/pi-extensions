package rpivaskuserquestion

import (
	"math"
	"testing"
)

func multiQuestion(text string, labels ...string) QuestionData {
	q := question("H", text, labels...)
	q.MultiSelect = true
	return q
}

func payload(entries ...map[string]any) map[string]any {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	return map[string]any{"answers": list}
}

func TestResolveExternalAnswerSingleSelectOption(t *testing.T) {
	preview := "code"
	q := twoOptions("Auth", "Which?")
	q.Options[0].Preview = &preview

	outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
		"questionIndex": 0,
		"optionIndexes": []any{0},
	}))
	if !outcome.OK {
		t.Fatalf("rejected: %+v", outcome)
	}
	result := outcome.Result
	if result.Cancelled || len(result.Answers) != 1 {
		t.Fatalf("result = %#v", result)
	}
	answer := result.Answers[0]
	if answer.Kind != AnswerKindOption || answer.Answer == nil || *answer.Answer != "A" {
		t.Fatalf("answer = %#v", answer)
	}
	if answer.Preview == nil || *answer.Preview != "code" {
		t.Fatalf("preview = %v", answer.Preview)
	}
}

func TestResolveExternalAnswerSingleSelectCustomText(t *testing.T) {
	outcome := ResolveExternalAnswer([]QuestionData{twoOptions("H", "Q?")}, payload(map[string]any{
		"questionIndex": 0,
		"text":          "my own",
	}))
	if !outcome.OK {
		t.Fatalf("rejected: %+v", outcome)
	}
	answer := outcome.Result.Answers[0]
	if answer.Kind != AnswerKindCustom || *answer.Answer != "my own" {
		t.Fatalf("answer = %#v", answer)
	}
}

func TestResolveExternalAnswerMultiSelect(t *testing.T) {
	q := multiQuestion("Which features?", "A", "B", "C")
	outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
		"questionIndex": 0,
		"optionIndexes": []any{2, 0},
		"text":          "extra",
	}))
	if !outcome.OK {
		t.Fatalf("rejected: %+v", outcome)
	}
	answer := outcome.Result.Answers[0]
	// Labels come back in authored order, not the order the emitter listed them.
	if len(answer.Selected) != 2 || answer.Selected[0] != "A" || answer.Selected[1] != "C" {
		t.Fatalf("selected = %v", answer.Selected)
	}
	if answer.Answer == nil || *answer.Answer != "extra" {
		t.Fatalf("answer = %v", answer.Answer)
	}
}

func TestResolveExternalAnswerCancel(t *testing.T) {
	q := twoOptions("H", "Q?")

	t.Run("plain cancel", func(t *testing.T) {
		outcome := ResolveExternalAnswer([]QuestionData{q}, map[string]any{"cancel": true})
		if !outcome.OK || !outcome.Result.Cancelled || len(outcome.Result.Answers) != 0 {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	t.Run("cancel with an empty answers array", func(t *testing.T) {
		outcome := ResolveExternalAnswer([]QuestionData{q}, map[string]any{"cancel": true, "answers": []any{}})
		if !outcome.OK {
			t.Fatalf("rejected: %+v", outcome)
		}
	})

	t.Run("cancel with answers is refused", func(t *testing.T) {
		outcome := ResolveExternalAnswer([]QuestionData{q}, map[string]any{
			"cancel":  true,
			"answers": []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
		})
		if outcome.OK || outcome.Code != RejectInvalidPayload {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	t.Run("cancel carries a trimmed global note", func(t *testing.T) {
		outcome := ResolveExternalAnswer([]QuestionData{q}, map[string]any{
			"cancel":     true,
			"globalNote": "  spaced  ",
		})
		if !outcome.OK || outcome.Result.GlobalNote == nil || *outcome.Result.GlobalNote != "spaced" {
			t.Fatalf("note = %v", outcome.Result.GlobalNote)
		}
	})
}

func TestResolveExternalAnswerRejectsByCode(t *testing.T) {
	single := twoOptions("Auth", "Q?")
	multi := multiQuestion("Which?", "A", "B")
	blank := "   "
	empty := ""

	cases := []struct {
		name    string
		payload map[string]any
		code    AskUserAnswerRejectCode
		reason  string
	}{
		{
			name:    "cancel not boolean",
			payload: map[string]any{"cancel": "yes"},
			code:    RejectInvalidPayload,
			reason:  "cancel must be a boolean",
		},
		{
			name:    "globalNote not string",
			payload: map[string]any{"globalNote": 3},
			code:    RejectInvalidPayload,
			reason:  "globalNote must be a string",
		},
		{
			name:    "answers missing",
			payload: map[string]any{},
			code:    RejectInvalidPayload,
			reason:  "answers must be an array",
		},
		{
			name:    "answers not an array",
			payload: map[string]any{"answers": "nope"},
			code:    RejectInvalidPayload,
			reason:  "answers must be an array",
		},
		{
			name:    "entry not an object",
			payload: map[string]any{"answers": []any{"x"}},
			code:    RejectInvalidPayload,
			reason:  "each answer must be an object",
		},
		{
			name:    "questionIndex not an integer",
			payload: map[string]any{"answers": []any{map[string]any{"questionIndex": 1.5}}},
			code:    RejectInvalidPayload,
			reason:  "questionIndex must be an integer",
		},
		// NaN and the infinities are numbers, so only an explicit integer test
		// rejects them — a float→int conversion would hand back a garbage index.
		{
			name:    "questionIndex is NaN",
			payload: map[string]any{"answers": []any{map[string]any{"questionIndex": math.NaN()}}},
			code:    RejectInvalidPayload,
			reason:  "questionIndex must be an integer",
		},
		{
			name:    "questionIndex is infinite",
			payload: map[string]any{"answers": []any{map[string]any{"questionIndex": math.Inf(1)}}},
			code:    RejectInvalidPayload,
			reason:  "questionIndex must be an integer",
		},
		// An in-process int skips the JSON float64 decode, but it must hit the same
		// safety-integer bound: math.MaxInt is as unusable an index as 2^53+1.
		{
			name:    "questionIndex is an out-of-range in-process int",
			payload: map[string]any{"answers": []any{map[string]any{"questionIndex": int(math.MaxInt)}}},
			code:    RejectInvalidPayload,
			reason:  "questionIndex must be an integer",
		},
		// Each element is validated on its own: resolveEntry stops at the first
		// bad index, so a combined list would leave every later element untested.
		{
			name: "optionIndexes carries a fractional index",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{0.5},
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: optionIndexes must be an array of integers",
		},
		{
			name: "optionIndexes carries NaN",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{math.NaN()},
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: optionIndexes must be an array of integers",
		},
		{
			name: "optionIndexes carries +Inf",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{math.Inf(1)},
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: optionIndexes must be an array of integers",
		},
		{
			name: "optionIndexes carries -Inf",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{math.Inf(-1)},
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: optionIndexes must be an array of integers",
		},
		{
			name:    "questionIndex missing",
			payload: map[string]any{"answers": []any{map[string]any{"text": "x"}}},
			code:    RejectInvalidPayload,
			reason:  "questionIndex must be an integer",
		},
		{
			name: "questionIndex out of range",
			payload: map[string]any{"answers": []any{map[string]any{
				"questionIndex": 3, "optionIndexes": []any{0},
			}}},
			code:   RejectInvalidAnswer,
			reason: "questionIndex 3 out of range (0..0)",
		},
		{
			name: "answered twice",
			payload: map[string]any{"answers": []any{
				map[string]any{"questionIndex": 0, "optionIndexes": []any{0}},
				map[string]any{"questionIndex": 0, "optionIndexes": []any{1}},
			}},
			code:   RejectInvalidAnswer,
			reason: "question 0 answered twice",
		},
		{
			name:    "missing answer",
			payload: map[string]any{"answers": []any{}},
			code:    RejectInvalidAnswer,
			reason:  `no answer for question 0 ("Auth")`,
		},
		{
			name: "optionIndexes not integers",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{"0"},
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: optionIndexes must be an array of integers",
		},
		{
			name: "text not a string",
			payload: payload(map[string]any{
				"questionIndex": 0, "text": 7,
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: text must be a string",
		},
		{
			name: "notes not a string",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{0}, "notes": 1,
			}),
			code:   RejectInvalidPayload,
			reason: "question 0: notes must be a string",
		},
		{
			name: "option index out of range",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{5},
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: option index 5 out of range (0..1)",
		},
		{
			name: "negative option index",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{-1},
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: option index -1 out of range (0..1)",
		},
		{
			name: "repeated option index",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{0, 0},
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: option index repeated",
		},
		{
			name: "blank text",
			payload: payload(map[string]any{
				"questionIndex": 0, "text": blank,
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: text is blank",
		},
		{
			name: "nothing picked",
			payload: payload(map[string]any{
				"questionIndex": 0,
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: needs optionIndexes or text",
		},
		{
			name: "single select with both",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{0}, "text": "x",
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: single-select takes optionIndexes or text, not both",
		},
		{
			name: "single select with two options",
			payload: payload(map[string]any{
				"questionIndex": 0, "optionIndexes": []any{0, 1},
			}),
			code:   RejectInvalidAnswer,
			reason: "question 0: single-select takes one option, got 2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			questions := []QuestionData{single}
			if tc.name == "no answer for question 0" {
				questions = []QuestionData{multi, multiQuestion("Second?", "A", "B")}
			}
			outcome := ResolveExternalAnswer(questions, tc.payload)
			if outcome.OK {
				t.Fatal("expected a rejection")
			}
			if outcome.Code != tc.code {
				t.Fatalf("code = %q, want %q (reason %q)", outcome.Code, tc.code, outcome.Reason)
			}
			if outcome.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", outcome.Reason, tc.reason)
			}
		})
	}
	_ = empty
}

func TestResolveExternalAnswerTrimsNotes(t *testing.T) {
	q := twoOptions("H", "Q?")

	t.Run("blank notes are absent", func(t *testing.T) {
		outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
			"questionIndex": 0, "optionIndexes": []any{0}, "notes": "   ",
		}))
		if !outcome.OK {
			t.Fatalf("rejected: %+v", outcome)
		}
		if outcome.Result.Answers[0].Notes != nil {
			t.Fatalf("notes = %v", outcome.Result.Answers[0].Notes)
		}
	})

	t.Run("notes are trimmed", func(t *testing.T) {
		outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
			"questionIndex": 0, "optionIndexes": []any{0}, "notes": "  keep me \n",
		}))
		if outcome.Result.Answers[0].Notes == nil || *outcome.Result.Answers[0].Notes != "keep me" {
			t.Fatalf("notes = %v", outcome.Result.Answers[0].Notes)
		}
	})
}

func TestResolveExternalAnswerGlobalNote(t *testing.T) {
	q := twoOptions("H", "Q?")
	outcome := ResolveExternalAnswer([]QuestionData{q}, map[string]any{
		"answers":    []any{map[string]any{"questionIndex": 0, "text": "x"}},
		"globalNote": "  whole thing  ",
	})
	if !outcome.OK {
		t.Fatalf("rejected: %+v", outcome)
	}
	if outcome.Result.GlobalNote == nil || *outcome.Result.GlobalNote != "whole thing" {
		t.Fatalf("globalNote = %v", outcome.Result.GlobalNote)
	}
}

func TestResolveExternalAnswerMultiSelectEmptyCommit(t *testing.T) {
	q := multiQuestion("Which?", "A", "B")
	// An empty optionIndexes list with a blank text is still refused: the TUI
	// Next row with nothing checked commits through the dialog path, not here.
	outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
		"questionIndex": 0,
	}))
	if outcome.OK || outcome.Code != RejectInvalidAnswer {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResolveExternalAnswerMultiSelectCustomOnly(t *testing.T) {
	q := multiQuestion("Which?", "A", "B")
	outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
		"questionIndex": 0, "text": "only text",
	}))
	if !outcome.OK {
		t.Fatalf("rejected: %+v", outcome)
	}
	answer := outcome.Result.Answers[0]
	if answer.Kind != AnswerKindMulti || len(answer.Selected) != 0 || *answer.Answer != "only text" {
		t.Fatalf("answer = %#v", answer)
	}
}

// Every question must be answered: the resolution is all-or-nothing.
func TestResolveExternalAnswerRequiresEveryQuestion(t *testing.T) {
	questions := []QuestionData{
		twoOptions("A", "first?"),
		twoOptions("B", "second?"),
	}
	outcome := ResolveExternalAnswer(questions, payload(map[string]any{
		"questionIndex": 0, "optionIndexes": []any{0},
	}))
	if outcome.OK || outcome.Code != RejectInvalidAnswer {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Reason != `no answer for question 1 ("B")` {
		t.Fatalf("reason = %q", outcome.Reason)
	}
}

// An absent preview field on a picked option yields an answer without one.
func TestResolveExternalAnswerOptionWithoutPreview(t *testing.T) {
	q := twoOptions("H", "Q?")
	outcome := ResolveExternalAnswer([]QuestionData{q}, payload(map[string]any{
		"questionIndex": 0, "optionIndexes": []any{1},
	}))
	if !outcome.OK {
		t.Fatalf("rejected: %+v", outcome)
	}
	if outcome.Result.Answers[0].Preview != nil {
		t.Fatalf("preview = %v", outcome.Result.Answers[0].Preview)
	}
}
