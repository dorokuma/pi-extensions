package rpivaskuserquestion

// Ports rpc-fallback.ts (v2.15.0): the sequential native-dialog walker for RPC
// hosts.
//
// The canonical TUI path needs a real terminal. RPC-mode hosts (the editor
// pendant, ACP clients) report hasUI: true because the dialog sub-protocol
// works, but custom() resolves undefined without rendering. ui.Select and
// ui.Input ARE functional there, so this walker walks the questions sequentially
// with those primitives and produces the same QuestionnaireResult shapes the TUI
// path produces, feeding the shared envelope.
//
// Parity trade-offs vs the TUI, inherent to the select/input API surface: no
// side-by-side preview pane (previews are folded into the prompt title), no
// tabbed multi-question review (one dialog per question), and multi-select is a
// free-text numbers input instead of checkbox rows.

import (
	"context"
	"strconv"
	"strings"
)

// RPC dialog copy. Resolved through T() at dialog time so the live locale
// applies.
const (
	multiSelectInstructions = `Enter the numbers of all that apply, comma-separated (e.g. "1,3"). Add "; custom answer" to include your own text, or type only custom text.`
	customAnswerTitle       = "Type your answer:"
	multiSelectPlaceholder  = "1,3; custom answer"
)

// MaxPreviewChars is the longest preview slice folded into a select title before
// truncation.
const MaxPreviewChars = 600

// DialogUI is the dialog-primitive slice of the host UI this walker needs.
// Structural on purpose: the runtime gate that makes the shape trustworthy is
// [HasDialogUI].
type DialogUI interface {
	Select(ctx context.Context, title string, options []string) (string, bool, error)
	Input(ctx context.Context, title, placeholder string) (string, bool, error)
}

// HasDialogUI mirrors hasDialogUI: true when the host implements the
// select/input dialog primitives. With PiG the primitives are typed methods on
// the SDK context, so a non-nil host always qualifies.
func HasDialogUI(ui DialogUI) bool { return ui != nil }

// formatOptionLine mirrors formatOptionLine.
func formatOptionLine(option OptionData, index int) string {
	return itoa(index+1) + ". " + option.Label + " — " + option.Description
}

// parseIndex mirrors parseIndex: parse a user-entered "N…" token to a 0-based
// option index. Out-of-range and non-numeric fail the bounds check.
func parseIndex(token string, count int) (int, bool) {
	digits := leadingDigits(token)
	if digits == "" {
		return 0, false
	}
	value, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	index := value - 1
	if index < 0 || index >= count {
		return 0, false
	}
	return index, true
}

// leadingDigits returns the leading ASCII digit run.
func leadingDigits(token string) string {
	end := 0
	for end < len(token) && token[end] >= '0' && token[end] <= '9' {
		end++
	}
	return token[:end]
}

// buildPreviewBlock mirrors buildPreviewBlock: previews folded into the select
// title, since RPC has no side-by-side pane.
func buildPreviewBlock(question QuestionData) string {
	blocks := make([]string, 0, len(question.Options))
	for i, option := range question.Options {
		text := option.previewText()
		if text == "" {
			continue
		}
		if len(text) > MaxPreviewChars {
			text = text[:MaxPreviewChars]
		}
		blocks = append(blocks, "--- "+itoa(i+1)+". "+option.Label+" preview ---\n"+text)
	}
	if len(blocks) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(blocks, "\n\n")
}

// RunRpcQuestionnaire mirrors runRpcQuestionnaire. Dismissing a question's
// dialog cancels the whole questionnaire — mirroring Esc in the TUI — and the
// shared envelope emits the decline.
func RunRpcQuestionnaire(ctx context.Context, ui DialogUI, params QuestionParams) QuestionnaireResult {
	answers := make([]QuestionAnswer, 0, len(params.Questions))
	for qi, question := range params.Questions {
		header := ""
		if question.Header != "" {
			header = "[" + question.Header + "] "
		}
		var answer *QuestionAnswer
		var ok bool
		if question.MultiSelect {
			answer, ok = askMultiSelect(ctx, ui, question, qi, header)
		} else {
			answer, ok = askSingleSelect(ctx, ui, question, qi, header)
		}
		if !ok {
			return QuestionnaireResult{Answers: answers, Cancelled: true}
		}
		answers = append(answers, *answer)
	}
	return QuestionnaireResult{Answers: answers, Cancelled: false}
}

// askSingleSelect mirrors askSingleSelect. ok=false means the user dismissed the
// option dialog (cancel the questionnaire).
//
// Hosts return the chosen option line, but some also let the user type a reply
// in place of picking and return that text (#299). A reply is therefore read as:
// an offered line → that option; a bare number → that option; a reply equal to an
// option label → that option; any other non-empty text → the user's own answer,
// verbatim. Cancelling the "Type something." follow-up returns to the options
// instead of cancelling the whole questionnaire (#142), and that input repeats
// the option list so the user can refer to it while typing (#194).
func askSingleSelect(
	ctx context.Context,
	ui DialogUI,
	question QuestionData,
	questionIndex int,
	header string,
) (*QuestionAnswer, bool) {
	options := make([]string, 0, len(question.Options)+1)
	for i, option := range question.Options {
		options = append(options, formatOptionLine(option, i))
	}
	options = append(options, itoa(len(question.Options)+1)+". "+DisplayLabel(SentinelOther))
	title := header + question.Question + buildPreviewBlock(question)

	for {
		chosen, ok, err := ui.Select(ctx, title, options)
		if err != nil || !ok {
			return nil, false
		}
		reply := strings.TrimSpace(chosen)
		if reply == "" {
			return nil, false
		}
		index, matched := resolveSingleSelectReply(chosen, reply, options, question)
		if !matched {
			return &QuestionAnswer{
				QuestionIndex: questionIndex,
				Question:      question.Question,
				Kind:          AnswerKindCustom,
				Answer:        strPtr(reply),
			}, true
		}
		if index < len(question.Options) {
			option := question.Options[index]
			answer := QuestionAnswer{
				QuestionIndex: questionIndex,
				Question:      question.Question,
				Kind:          AnswerKindOption,
				Answer:        strPtr(option.Label),
			}
			if option.hasPreview() {
				answer.Preview = strPtr(option.previewText())
			}
			return &answer, true
		}
		// "Type something." sentinel → free-text follow-up; cancelling it, or a
		// blank reply (#301), goes back to the options.
		repeatList := make([]string, 0, len(question.Options))
		for i, option := range question.Options {
			repeatList = append(repeatList, formatOptionLine(option, i))
		}
		typed, inputOK, err := ui.Input(ctx,
			header+question.Question+"\n\n"+strings.Join(repeatList, "\n")+"\n\n"+
				T("rpc.custom_answer_title", customAnswerTitle), "")
		if err != nil || !inputOK || strings.TrimSpace(typed) == "" {
			continue
		}
		return &QuestionAnswer{
			QuestionIndex: questionIndex,
			Question:      question.Question,
			Kind:          AnswerKindCustom,
			Answer:        strPtr(typed),
		}, true
	}
}

// resolveSingleSelectReply mirrors resolveSingleSelectReply: the option index for
// a select reply, or matched=false when the reply is the user's own text.
func resolveSingleSelectReply(
	chosen, reply string,
	lines []string,
	question QuestionData,
) (int, bool) {
	for i, line := range lines {
		if line == chosen {
			return i, true
		}
	}
	if isBareNumber(reply) {
		if index, ok := parseIndex(reply, len(lines)); ok {
			return index, true
		}
		return 0, false
	}
	for i, option := range question.Options {
		if option.Label == reply {
			return i, true
		}
	}
	return 0, false
}

// isBareNumber mirrors /^\d+\.?$/.
func isBareNumber(text string) bool {
	if text == "" {
		return false
	}
	body := text
	if strings.HasSuffix(body, ".") {
		body = body[:len(body)-1]
	}
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	return true
}

// askMultiSelect mirrors askMultiSelect. ok=false means the user dismissed the
// dialog (cancel the questionnaire).
func askMultiSelect(
	ctx context.Context,
	ui DialogUI,
	question QuestionData,
	questionIndex int,
	header string,
) (*QuestionAnswer, bool) {
	list := make([]string, 0, len(question.Options))
	for i, option := range question.Options {
		list = append(list, formatOptionLine(option, i))
	}
	value, ok, err := ui.Input(ctx,
		header+question.Question+"\n\n"+strings.Join(list, "\n")+"\n\n"+
			T("rpc.multi_instructions", multiSelectInstructions),
		multiSelectPlaceholder)
	if err != nil || !ok {
		return nil, false
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		// Deliberate empty commit — same as pressing "Next" with nothing toggled.
		return &QuestionAnswer{
			QuestionIndex: questionIndex,
			Question:      question.Question,
			Kind:          AnswerKindMulti,
			Selected:      []string{},
		}, true
	}

	parts := strings.SplitN(trimmed, ";", 2)
	selection := strings.TrimSpace(parts[0])
	custom := ""
	if len(parts) > 1 {
		custom = strings.TrimSpace(parts[1])
	}
	tokens := splitTokens(selection)

	indices := make([]int, 0, len(tokens))
	allNumeric := true
	for _, token := range tokens {
		if !isBareNumber(token) {
			allNumeric = false
			break
		}
		index, ok := parseIndex(token, len(question.Options))
		if !ok {
			allNumeric = false
			break
		}
		indices = append(indices, index)
	}

	if selection == "" || allNumeric {
		selected := make([]string, 0, len(indices))
		for _, index := range indices {
			label := question.Options[index].Label
			if !containsString(selected, label) {
				selected = append(selected, label)
			}
		}
		answer := QuestionAnswer{
			QuestionIndex: questionIndex,
			Question:      question.Question,
			Kind:          AnswerKindMulti,
			Selected:      selected,
		}
		if custom != "" {
			answer.Answer = strPtr(custom)
		}
		return &answer, true
	}
	// Preserve unparseable input verbatim as custom text rather than dropping it.
	return &QuestionAnswer{
		QuestionIndex: questionIndex,
		Question:      question.Question,
		Kind:          AnswerKindMulti,
		Answer:        strPtr(trimmed),
		Selected:      []string{},
	}, true
}

// splitTokens splits on commas and whitespace, dropping empties.
func splitTokens(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	return fields
}

// containsString is a slice membership test.
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
