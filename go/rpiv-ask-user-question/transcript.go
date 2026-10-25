package rpivaskuserquestion

// Ports view/transcript.ts (v2.15.0): the tool call and result renderers the
// transcript shows.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Transcript layout constants.
const (
	transcriptIndent = "  "
	columnGap        = "  "
)

// recommendedSuffix is stripped from option labels in the receipt.
const recommendedSuffix = " (Recommended)"

// ToolResultView is the shape renderAskResult reads.
type ToolResultView struct {
	Content []ToolContent
	Details any
}

// widthLines renders lines laid out at render time, so wrapping follows the
// terminal width.
type widthLines struct {
	layout func(width int) []string
}

// Render runs the layout.
func (w widthLines) Render(width int) []string { return w.layout(width) }

// RenderAskCall mirrors renderAskCall: `Ask Format · Migration`, where the
// headers name the questions. The returned component lays the line out at the
// host's render width, exactly as the TS WidthLines component does — so a
// truncated width truncates, and a comfortable width shows the full line.
func RenderAskCall(args *QuestionParams, theme sdk.UITheme) widthLines {
	title := theme.Fg("toolTitle", theme.Bold("Ask"))
	headers := make([]string, 0)
	if args != nil {
		for _, question := range args.Questions {
			if question.Header != "" {
				headers = append(headers, question.Header)
			}
		}
	}
	text := title
	if len(headers) > 0 {
		text = title + " " + theme.Fg("accent", strings.Join(headers, " · "))
	}
	return widthLines{layout: func(width int) []string {
		return []string{truncateToWidth(text, width, "…", false)}
	}}
}

// RenderAskResultLines mirrors renderAskResult, returning the rendered lines.
// A failure shows `✗` with the first line of the model-facing message; a
// declined questionnaire shows a dimmed `✗ no answer`.
func RenderAskResultLines(
	result ToolResultView,
	args *QuestionParams,
	theme sdk.UITheme,
	expanded, isError bool,
	width int,
) []string {
	details, ok := asQuestionnaireResult(result.Details)
	if isError || (ok && details.Error != nil) {
		return []string{theme.Fg("error", "✗ "+firstTextLine(result))}
	}
	if !ok {
		return []string{theme.Fg("success", "✓")}
	}
	if details.Cancelled {
		return []string{theme.Fg("dim", "✗ "+T("transcript.no_answer", "no answer"))}
	}
	var questions []QuestionData
	if args != nil {
		questions = args.Questions
	}
	if len(questions) > 1 {
		return tableLines(*details, questions, theme, expanded, width)
	}
	var questionText string
	if len(questions) > 0 {
		questionText = questions[0].Question
	}
	return singleLines(*details, questionText, theme, expanded, width)
}

// RenderAskResult mirrors renderAskResult's component form.
func RenderAskResult(
	result ToolResultView,
	args *QuestionParams,
	theme sdk.UITheme,
	expanded, isError bool,
) widthLines {
	return widthLines{layout: func(width int) []string {
		return RenderAskResultLines(result, args, theme, expanded, isError, width)
	}}
}

// asQuestionnaireResult accepts either a decoded map (the wire form) or a
// QuestionResult value.
func asQuestionnaireResult(value any) (*QuestionnaireResult, bool) {
	switch typed := value.(type) {
	case *QuestionnaireResult:
		if typed == nil {
			return nil, false
		}
		return typed, true
	case QuestionnaireResult:
		copied := typed
		return &copied, true
	case map[string]any:
		return IsQuestionnaireResult(typed)
	}
	return nil, false
}

// singleLines mirrors singleLines.
func singleLines(
	details QuestionnaireResult,
	questionText string,
	theme sdk.UITheme,
	expanded bool,
	width int,
) []string {
	var answer *QuestionAnswer
	if len(details.Answers) > 0 {
		answer = &details.Answers[0]
	}
	lines := hanging(theme.Fg("success", "✓")+" ", answerText(answer, theme), width)
	detailQuestion := ""
	if expanded {
		detailQuestion = questionText
	}
	lines = append(lines, detailLines(answer, detailQuestion, transcriptIndent, theme, width)...)
	return append(lines, globalNoteLines(details, theme, width)...)
}

// tableLines mirrors tableLines: an aligned header/answer table.
func tableLines(
	details QuestionnaireResult,
	questions []QuestionData,
	theme sdk.UITheme,
	expanded bool,
	width int,
) []string {
	headerWidth := 0
	for _, question := range questions {
		if w := visibleWidth(question.Header); w > headerWidth {
			headerWidth = w
		}
	}
	answerIndent := repeatSpace(visibleWidth(transcriptIndent) + headerWidth + visibleWidth(columnGap))

	lines := make([]string, 0, len(questions)*2)
	for i, question := range questions {
		var answer *QuestionAnswer
		for j := range details.Answers {
			if details.Answers[j].QuestionIndex == i {
				answer = &details.Answers[j]
				break
			}
		}
		header := fillWidth(question.Header, headerWidth)
		row := hanging(transcriptIndent+theme.Fg("muted", header)+columnGap, answerText(answer, theme), width)
		lines = append(lines, row...)
		detailQuestion := ""
		if expanded {
			detailQuestion = question.Question
		}
		lines = append(lines, detailLines(answer, detailQuestion, answerIndent, theme, width)...)
	}
	return append(lines, globalNoteLines(details, theme, width)...)
}

// detailLines mirrors detailLines: the question text and chosen preview when
// expanded, then the answer's note.
func detailLines(
	answer *QuestionAnswer,
	questionText, indent string,
	theme sdk.UITheme,
	width int,
) []string {
	lines := make([]string, 0, 4)
	if questionText != "" {
		lines = append(lines, hanging(indent, theme.Fg("dim", questionText), width)...)
	}
	if questionText != "" && answer != nil && answer.Preview != nil && len(*answer.Preview) > 0 {
		for _, line := range StripFenceMarkers(splitLines(*answer.Preview)) {
			lines = append(lines, truncateToWidth(indent+theme.Fg("dim", "│ "+line), width, "…", false))
		}
	}
	if answer != nil && answer.Notes != nil && len(*answer.Notes) > 0 {
		lines = append(lines, hanging(indent, theme.Fg("dim", "✎ "+*answer.Notes), width)...)
	}
	return lines
}

// globalNoteLines mirrors globalNoteLines.
func globalNoteLines(details QuestionnaireResult, theme sdk.UITheme, width int) []string {
	if details.GlobalNote == nil || len(*details.GlobalNote) == 0 {
		return nil
	}
	return hanging(transcriptIndent, theme.Fg("dim", "✎ "+*details.GlobalNote), width)
}

// answerText mirrors answerText.
func answerText(answer *QuestionAnswer, theme sdk.UITheme) string {
	if answer == nil {
		return theme.Fg("dim", T("transcript.not_answered", "not answered"))
	}
	var typed string
	if answer.Answer != nil && *answer.Answer != "" {
		typed = `"` + *answer.Answer + `"`
	}
	var picks []string
	switch answer.Kind {
	case AnswerKindOption:
		if answer.Answer != nil {
			picks = append(picks, stripRecommended(*answer.Answer))
		}
	case AnswerKindCustom:
		if typed != "" {
			picks = append(picks, typed)
		}
	default:
		for _, label := range answer.Selected {
			picks = append(picks, stripRecommended(label))
		}
		if typed != "" {
			picks = append(picks, typed)
		}
	}
	joined := strings.Join(picks, ", ")
	if joined == "" {
		joined = NoInputPlaceholder
	}
	return theme.Fg("toolOutput", joined)
}

// stripRecommended mirrors label(): `text.replace(/\s*\(Recommended\)$/, "")`.
// The regex matches an optional run of whitespace followed by the literal suffix
// at end-of-string, so `"Use  (Recommended)"` becomes `"Use"` and a label that is
// only the suffix becomes the empty string.
func stripRecommended(text string) string {
	const suffix = "(Recommended)"
	if !strings.HasSuffix(text, suffix) {
		return text
	}
	body := text[:len(text)-len(suffix)]
	return strings.TrimRight(body, " \t")
}

// hanging mirrors hanging: `prefix` then `text` wrapped to the remaining width,
// continuation lines aligned under the text.
func hanging(prefix, text string, width int) []string {
	indent := visibleWidth(prefix)
	wrapped := wrapTextWithAnsi(text, maxInt(1, width-indent))
	lines := make([]string, 0, len(wrapped))
	for i, line := range wrapped {
		if i == 0 {
			lines = append(lines, prefix+line)
			continue
		}
		lines = append(lines, repeatSpace(indent)+line)
	}
	return lines
}

// firstTextLine mirrors firstTextLine.
func firstTextLine(result ToolResultView) string {
	text := ""
	for _, block := range result.Content {
		if block.Type == "text" {
			text = block.Text
			break
		}
	}
	lines := splitLines(text)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}
