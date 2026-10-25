package rpivaskuserquestion

import (
	"strings"
	"testing"
)

// ── transcript ────────────────────────────────────────────────────────────

func TestRenderAskCall(t *testing.T) {
	theme := themeForTests()

	single := QuestionParams{Questions: []QuestionData{twoOptions("Auth", "Q?")}}
	lines := RenderAskCall(&single, theme).Render(80)
	if len(lines) != 1 {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.HasPrefix(renderStrings(lines), "Ask") {
		t.Fatalf("line = %q", lines[0])
	}
	if !strings.Contains(lines[0], "Auth") {
		t.Fatalf("header missing: %q", lines[0])
	}

	// Several headers join with the separator.
	multi := multiParams()
	lines = RenderAskCall(&multi, theme).Render(80)
	if !strings.Contains(lines[0], "Auth · Features") {
		t.Fatalf("line = %q", lines[0])
	}

	// No args -> just the title.
	if lines := RenderAskCall(nil, theme).Render(80); renderStrings(lines) != "Ask" {
		t.Fatalf("bare title = %q", lines[0])
	}

	// An empty header is dropped.
	noHeader := QuestionParams{Questions: []QuestionData{twoOptions("", "Q?")}}
	if lines := RenderAskCall(&noHeader, theme).Render(80); strings.Contains(lines[0], "·") {
		t.Fatalf("empty header rendered: %q", lines[0])
	}

	// A narrow width truncates with an ellipsis (the TS WidthLines behaviour).
	truncated := RenderAskCall(&single, theme).Render(3)
	if !strings.Contains(renderStrings(truncated), "…") {
		t.Fatalf("narrow line = %q", truncated[0])
	}
}

func TestRenderAskResultError(t *testing.T) {
	theme := themeForTests()
	result := ToolResultView{
		Content: []ToolContent{{Type: "text", Text: "Error: first line\nsecond line"}},
	}
	lines := RenderAskResultLines(result, nil, theme, false, true, 80)
	if len(lines) != 1 {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.HasPrefix(lines[0], "✗ Error: first line") {
		t.Fatalf("line = %q", lines[0])
	}
	if strings.Contains(lines[0], "second line") {
		t.Fatalf("second line leaked: %q", lines[0])
	}
}

// A details.error also renders as a failure, even when the call was not flagged.
func TestRenderAskResultErrorFromDetails(t *testing.T) {
	theme := themeForTests()
	code := ErrorNoUI
	result := ToolResultView{
		Content: []ToolContent{{Type: "text", Text: "Error: no ui"}},
		Details: map[string]any{
			"answers":   []any{},
			"cancelled": true,
			"error":     string(code),
		},
	}
	lines := RenderAskResultLines(result, nil, theme, false, false, 80)
	if !strings.HasPrefix(lines[0], "✗ Error: no ui") {
		t.Fatalf("line = %q", lines[0])
	}
}

// No recognisable details → a bare success check.
func TestRenderAskResultNoDetails(t *testing.T) {
	lines := RenderAskResultLines(ToolResultView{}, nil, themeForTests(), false, false, 80)
	if len(lines) != 1 || lines[0] != "✓" {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestRenderAskResultCancelled(t *testing.T) {
	theme := themeForTests()
	result := ToolResultView{
		Details: map[string]any{"answers": []any{}, "cancelled": true},
	}
	lines := RenderAskResultLines(result, nil, theme, false, false, 80)
	if len(lines) != 1 || !strings.Contains(lines[0], "no answer") {
		t.Fatalf("lines = %#v", lines)
	}
	// The cancelled line is dimmed (`no answer` localized when a locale is set).
	withLocale(t, "de")
	lines = RenderAskResultLines(result, nil, theme, false, false, 80)
	if !strings.Contains(lines[0], "keine Antwort") {
		t.Fatalf("localized cancel line = %q", lines[0])
	}
}

func TestRenderAskResultSingleAnswer(t *testing.T) {
	theme := themeForTests()
	params := singleParams()
	result := ToolResultView{Details: &QuestionnaireResult{
		Answers: []QuestionAnswer{{
			QuestionIndex: 0,
			Question:      "Which auth?",
			Kind:          AnswerKindOption,
			Answer:        strPtr("A (Recommended)"),
		}},
	}}

	// Collapsed: only the receipt row; the "(Recommended)" suffix is stripped.
	lines := RenderAskResultLines(result, &params, theme, false, false, 80)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "✓ ") {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.Contains(renderStrings(lines), "✓ A") {
		t.Fatalf("recommended suffix not stripped: %q", lines[0])
	}

	// Expanded: the question text appears as a dimmed detail row.
	lines = RenderAskResultLines(result, &params, theme, true, false, 80)
	text := renderStrings(lines)
	if !strings.Contains(text, "Which auth?") {
		t.Fatalf("expanded question missing:\n%s", text)
	}
	if !strings.HasPrefix(text, "✓ ") {
		t.Fatalf("receipt prefix lost:\n%s", text)
	}
}

// An expanded answer with a preview renders the preview with the vertical bar.
func TestRenderAskResultExpandedPreview(t *testing.T) {
	theme := themeForTests()
	params := singleParams()
	preview := "```go\nline\n```"
	result := ToolResultView{Details: &QuestionnaireResult{
		Answers: []QuestionAnswer{{
			QuestionIndex: 0,
			Question:      "Which auth?",
			Kind:          AnswerKindOption,
			Answer:        strPtr("A"),
			Preview:       &preview,
		}},
	}}
	text := renderStrings(RenderAskResultLines(result, &params, theme, true, false, 80))
	if !strings.Contains(text, "│ line") {
		t.Fatalf("preview bar missing:\n%s", text)
	}
	// Fence markers are stripped from the receipt.
	if strings.Contains(text, "```") {
		t.Fatalf("fence marker survived:\n%s", text)
	}
}

// Notes and the global note render with the pen glyph.
func TestRenderAskResultNotesAndGlobalNote(t *testing.T) {
	theme := themeForTests()
	params := singleParams()
	result := ToolResultView{Details: &QuestionnaireResult{
		Answers: []QuestionAnswer{{
			QuestionIndex: 0,
			Question:      "Which auth?",
			Kind:          AnswerKindOption,
			Answer:        strPtr("A"),
			Notes:         strPtr("mind the cost"),
		}},
		GlobalNote: strPtr("overall note"),
	}}
	text := renderStrings(RenderAskResultLines(result, &params, theme, false, false, 80))
	if !strings.Contains(text, "✎ mind the cost") {
		t.Fatalf("per-question note missing:\n%s", text)
	}
	if !strings.Contains(text, "✎ overall note") {
		t.Fatalf("global note missing:\n%s", text)
	}
}

func TestRenderAskResultMultiQuestionTable(t *testing.T) {
	theme := themeForTests()
	params := multiParams()
	result := ToolResultView{Details: &QuestionnaireResult{
		Answers: []QuestionAnswer{
			{QuestionIndex: 0, Question: "Which auth?", Kind: AnswerKindOption, Answer: strPtr("A")},
			{QuestionIndex: 1, Question: "Which features?", Kind: AnswerKindMulti, Selected: []string{"A", "B"}, Answer: strPtr("custom")},
		},
	}}
	lines := RenderAskResultLines(result, &params, theme, false, false, 80)
	if len(lines) != 2 {
		t.Fatalf("rows = %#v", lines)
	}
	text := renderStrings(lines)
	// Both headers are aligned in a column.
	if !strings.Contains(text, "Auth") || !strings.Contains(text, "Features") {
		t.Fatalf("headers missing:\n%s", text)
	}
	if !strings.Contains(text, "A, B, "+`"custom"`) {
		t.Fatalf("multi answer missing:\n%s", text)
	}
}

// A missing answer renders the "not answered" placeholder.
func TestRenderAskResultNotAnswered(t *testing.T) {
	theme := themeForTests()
	params := singleParams()
	result := ToolResultView{Details: &QuestionnaireResult{Answers: []QuestionAnswer{}}}
	text := renderStrings(RenderAskResultLines(result, &params, theme, false, false, 80))
	if !strings.Contains(text, "not answered") {
		t.Fatalf("placeholder missing:\n%s", text)
	}
}

// A long receipt wraps and its continuation lines align under the text.
func TestRenderAskResultWraps(t *testing.T) {
	theme := themeForTests()
	params := singleParams()
	result := ToolResultView{Details: &QuestionnaireResult{
		Answers: []QuestionAnswer{{
			QuestionIndex: 0,
			Question:      "Which auth?",
			Kind:          AnswerKindCustom,
			Answer:        strPtr(strings.Repeat("word ", 40)),
		}},
	}}
	lines := RenderAskResultLines(result, &params, theme, false, false, 40)
	if len(lines) < 2 {
		t.Fatalf("expected wrapping, got %d line(s)", len(lines))
	}
	for _, line := range lines {
		if visibleWidth(line) > 40 {
			t.Fatalf("line overflows the width: %q", line)
		}
	}
}

func TestStripRecommended(t *testing.T) {
	cases := map[string]string{
		"Use Redis (Recommended)":  "Use Redis",
		"Use Redis":                "Use Redis",
		"(Recommended)":            "",
		"Plain (recommended)":      "Plain (recommended)", // case-sensitive, as the TS regex
		"Use Redis  (Recommended)": "Use Redis",           // the regex swallows preceding whitespace
	}
	for input, want := range cases {
		if got := stripRecommended(input); got != want {
			t.Fatalf("stripRecommended(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFirstTextLine(t *testing.T) {
	result := ToolResultView{Content: []ToolContent{
		{Type: "image"},
		{Type: "text", Text: "first\nsecond"},
	}}
	if got := firstTextLine(result); got != "first" {
		t.Fatalf("firstTextLine = %q", got)
	}
	if got := firstTextLine(ToolResultView{}); got != "" {
		t.Fatalf("empty firstTextLine = %q", got)
	}
}

// RenderAskResult returns a width-laid-out component.
func TestRenderAskResultComponent(t *testing.T) {
	params := singleParams()
	component := RenderAskResult(ToolResultView{}, &params, themeForTests(), false, false)
	lines := component.Render(30)
	if len(lines) != 1 {
		t.Fatalf("lines = %#v", lines)
	}
}

// asQuestionnaireResult accepts both the wire map form and typed values.
func TestAsQuestionnaireResult(t *testing.T) {
	typed := QuestionnaireResult{Cancelled: true, Answers: []QuestionAnswer{}}
	if got, ok := asQuestionnaireResult(typed); !ok || !got.Cancelled {
		t.Fatalf("typed value = (%#v,%v)", got, ok)
	}
	if got, ok := asQuestionnaireResult(&typed); !ok || !got.Cancelled {
		t.Fatalf("typed pointer = (%#v,%v)", got, ok)
	}
	wire := map[string]any{"answers": []any{}, "cancelled": true}
	if got, ok := asQuestionnaireResult(wire); !ok || !got.Cancelled {
		t.Fatalf("wire map = (%#v,%v)", got, ok)
	}
	if _, ok := asQuestionnaireResult((*QuestionnaireResult)(nil)); ok {
		t.Fatal("nil pointer accepted")
	}
	if _, ok := asQuestionnaireResult(map[string]any{"answers": "x"}); ok {
		t.Fatal("malformed map accepted")
	}
	if _, ok := asQuestionnaireResult(42); ok {
		t.Fatal("integer accepted")
	}
}

// IsQuestionnaireResult's structural guard.
func TestIsQuestionnaireResult(t *testing.T) {
	valid, ok := IsQuestionnaireResult(map[string]any{
		"answers": []any{
			map[string]any{
				"questionIndex": 0.0,
				"question":      "Q?",
				"kind":          "option",
				"answer":        "A",
				"selected":      []any{"B"},
				"notes":         "n",
				"preview":       "p",
			},
		},
		"cancelled":  false,
		"globalNote": "g",
		"unanswered": []any{1.0},
		"error":      "no_ui",
	})
	if !ok {
		t.Fatal("valid result rejected")
	}
	if len(valid.Answers) != 1 {
		t.Fatalf("answers = %#v", valid.Answers)
	}
	answer := valid.Answers[0]
	if answer.Kind != AnswerKindOption || answer.Answer == nil || *answer.Answer != "A" {
		t.Fatalf("answer = %#v", answer)
	}
	if len(answer.Selected) != 1 || answer.Notes == nil || answer.Preview == nil {
		t.Fatalf("optional fields = %#v", answer)
	}
	if valid.GlobalNote == nil || *valid.GlobalNote != "g" {
		t.Fatalf("globalNote = %v", valid.GlobalNote)
	}
	if len(valid.Unanswered) != 1 || valid.Unanswered[0] != 1 {
		t.Fatalf("unanswered = %v", valid.Unanswered)
	}
	if valid.Error == nil || *valid.Error != ErrorNoUI {
		t.Fatalf("error = %v", valid.Error)
	}

	for _, invalid := range []any{
		nil,
		"string",
		map[string]any{},
		map[string]any{"answers": []any{}},
		map[string]any{"answers": []any{}, "cancelled": "no"},
		map[string]any{"cancelled": true},
	} {
		if _, ok := IsQuestionnaireResult(invalid); ok {
			t.Fatalf("%#v accepted", invalid)
		}
	}
}

// jsonInt reads both the float64 form encoding/json produces and plain ints.
func TestJSONInt(t *testing.T) {
	if got, ok := jsonInt(3.0); !ok || got != 3 {
		t.Fatalf("float = (%d,%v)", got, ok)
	}
	if got, ok := jsonInt(3); !ok || got != 3 {
		t.Fatalf("int = (%d,%v)", got, ok)
	}
	if _, ok := jsonInt(1.5); ok {
		t.Fatal("fractional value accepted")
	}
	if _, ok := jsonInt("3"); ok {
		t.Fatal("string accepted")
	}
}

// encodeQuestionParams round-trips through decodeQuestionParams.
func TestEncodeDecodeQuestionParams(t *testing.T) {
	preview := "p"
	original := QuestionParams{Questions: []QuestionData{{
		Question:    "Q?",
		Header:      "H",
		MultiSelect: true,
		Options: []OptionData{
			{Label: "A", Description: "a", Preview: &preview},
			{Label: "B", Description: "b"},
		},
	}}}
	decoded := decodeQuestionParams(encodeQuestionParams(original))
	if len(decoded.Questions) != 1 {
		t.Fatalf("questions = %d", len(decoded.Questions))
	}
	got := decoded.Questions[0]
	if got.Question != "Q?" || got.Header != "H" || !got.MultiSelect {
		t.Fatalf("question = %#v", got)
	}
	if len(got.Options) != 2 || got.Options[0].Preview == nil || *got.Options[0].Preview != "p" {
		t.Fatalf("options = %#v", got.Options)
	}
	if got.Options[1].Preview != nil {
		t.Fatalf("absent preview became %v", got.Options[1].Preview)
	}
}
