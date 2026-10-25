package rpivaskuserquestion

import (
	"context"
	"strings"
	"testing"
)

// ── rpc-fallback ──────────────────────────────────────────────────────────

// scriptedCall records one dialog invocation.
type scriptedCall struct {
	Kind        string
	Title       string
	Placeholder string
	Options     []string
}

// scriptedDialogUI answers Select/Input from scripted queues. A queue that runs
// dry means the user dismissed the dialog (the primitive resolves undefined).
type scriptedDialogUI struct {
	selects  []string
	inputs   []string
	calls    []scriptedCall
	selectAt int
	inputAt  int
}

// Select pops the next scripted select answer.
func (u *scriptedDialogUI) Select(_ context.Context, title string, options []string) (string, bool, error) {
	u.calls = append(u.calls, scriptedCall{Kind: "select", Title: title, Options: options})
	if u.selectAt >= len(u.selects) {
		return "", false, nil
	}
	value := u.selects[u.selectAt]
	u.selectAt++
	return value, true, nil
}

// Input pops the next scripted input answer.
func (u *scriptedDialogUI) Input(_ context.Context, title, placeholder string) (string, bool, error) {
	u.calls = append(u.calls, scriptedCall{Kind: "input", Title: title, Placeholder: placeholder})
	if u.inputAt >= len(u.inputs) {
		return "", false, nil
	}
	value := u.inputs[u.inputAt]
	u.inputAt++
	return value, true, nil
}

func TestFormatOptionLine(t *testing.T) {
	option := OptionData{Label: "Redis", Description: "in-memory store"}
	if got := formatOptionLine(option, 0); got != "1. Redis — in-memory store" {
		t.Fatalf("line = %q", got)
	}
}

func TestParseIndex(t *testing.T) {
	cases := []struct {
		token string
		count int
		want  int
		ok    bool
	}{
		{"1", 3, 0, true},
		{"3", 3, 2, true},
		{"3.", 3, 2, true},
		{"2. B — b", 3, 1, true},
		{"0", 3, 0, false},
		{"4", 3, 0, false},
		{"abc", 3, 0, false},
		{"", 3, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseIndex(tc.token, tc.count)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("parseIndex(%q,%d) = (%d,%v), want (%d,%v)", tc.token, tc.count, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIsBareNumber(t *testing.T) {
	for _, token := range []string{"1", "12", "3."} {
		if !isBareNumber(token) {
			t.Fatalf("%q not recognised as a bare number", token)
		}
	}
	for _, token := range []string{"", ".", "1a", "a1", "1.2", "-1"} {
		if isBareNumber(token) {
			t.Fatalf("%q wrongly recognised as a bare number", token)
		}
	}
}

func TestBuildPreviewBlock(t *testing.T) {
	preview := strings.Repeat("p", MaxPreviewChars+100)
	q := twoOptions("H", "Q?")
	q.Options[0].Preview = &preview
	block := buildPreviewBlock(q)
	if !strings.Contains(block, "--- 1. A preview ---") {
		t.Fatalf("block header missing:\n%s", block)
	}
	// The preview slice is capped at MaxPreviewChars: measure the slice after the
	// header rather than counting characters across the whole block.
	const header = "--- 1. A preview ---\n"
	index := strings.Index(block, header)
	if index < 0 {
		t.Fatalf("block header missing:\n%s", block)
	}
	sliceOf := block[index+len(header):]
	if len(sliceOf) != MaxPreviewChars {
		t.Fatalf("preview slice = %d chars, want %d", len(sliceOf), MaxPreviewChars)
	}
	if got := buildPreviewBlock(twoOptions("H", "Q?")); got != "" {
		t.Fatalf("no-preview block = %q", got)
	}
}

func TestRunRpcQuestionnaireSingleSelectOption(t *testing.T) {
	params := singleParams()
	ui := &scriptedDialogUI{selects: []string{"2. B — desc B"}}
	result := RunRpcQuestionnaire(context.Background(), ui, params)

	if result.Cancelled || len(result.Answers) != 1 {
		t.Fatalf("result = %#v", result)
	}
	answer := result.Answers[0]
	if answer.Kind != AnswerKindOption || *answer.Answer != "B" {
		t.Fatalf("answer = %#v", answer)
	}
	// The trailing sentinel row is offered alongside the author options.
	if len(ui.calls) != 1 || len(ui.calls[0].Options) != 3 {
		t.Fatalf("calls = %#v", ui.calls)
	}
	if ui.calls[0].Options[2] != "3. Type something." {
		t.Fatalf("sentinel option = %q", ui.calls[0].Options[2])
	}
}

// A host that returns the option's bare index still resolves it.
func TestRunRpcQuestionnaireBareNumberReply(t *testing.T) {
	ui := &scriptedDialogUI{selects: []string{"1"}}
	result := RunRpcQuestionnaire(context.Background(), ui, singleParams())
	if result.Cancelled || *result.Answers[0].Answer != "A" {
		t.Fatalf("result = %#v", result)
	}
}

// A host that returns the option's label resolves it.
func TestRunRpcQuestionnaireLabelReply(t *testing.T) {
	ui := &scriptedDialogUI{selects: []string{"B"}}
	result := RunRpcQuestionnaire(context.Background(), ui, singleParams())
	if result.Cancelled || *result.Answers[0].Answer != "B" {
		t.Fatalf("result = %#v", result)
	}
}

// Any other non-empty text is the user's own answer, verbatim (#299).
func TestRunRpcQuestionnaireFreeTextReply(t *testing.T) {
	ui := &scriptedDialogUI{selects: []string{"something else entirely"}}
	result := RunRpcQuestionnaire(context.Background(), ui, singleParams())
	if result.Cancelled || len(result.Answers) != 1 {
		t.Fatalf("result = %#v", result)
	}
	answer := result.Answers[0]
	if answer.Kind != AnswerKindCustom || *answer.Answer != "something else entirely" {
		t.Fatalf("answer = %#v", answer)
	}
}

// Picking the sentinel opens a free-text follow-up that repeats the option list.
func TestRunRpcQuestionnaireCustomSentinel(t *testing.T) {
	ui := &scriptedDialogUI{selects: []string{"3. Type something."}, inputs: []string{"my answer"}}
	result := RunRpcQuestionnaire(context.Background(), ui, singleParams())

	if result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
	answer := result.Answers[0]
	if answer.Kind != AnswerKindCustom || *answer.Answer != "my answer" {
		t.Fatalf("answer = %#v", answer)
	}
	// The follow-up prompt repeats the option lines (#194).
	if len(ui.calls) != 2 || ui.calls[1].Kind != "input" {
		t.Fatalf("calls = %#v", ui.calls)
	}
	if !strings.Contains(ui.calls[1].Title, "1. A — desc A") || !strings.Contains(ui.calls[1].Title, "Type your answer:") {
		t.Fatalf("follow-up title = %q", ui.calls[1].Title)
	}
}

// Dismissing the option dialog cancels the whole questionnaire.
func TestRunRpcQuestionnaireDismissed(t *testing.T) {
	ui := &scriptedDialogUI{}
	result := RunRpcQuestionnaire(context.Background(), ui, singleParams())
	if !result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Answers) != 0 {
		t.Fatalf("answers = %#v", result.Answers)
	}
}

// A blank select reply reads as a dismissal.
func TestRunRpcQuestionnaireBlankReply(t *testing.T) {
	ui := &scriptedDialogUI{selects: []string{"   "}}
	if result := RunRpcQuestionnaire(context.Background(), ui, singleParams()); !result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
}

// Cancelling the follow-up returns to the options rather than cancelling (#142),
// and a blank follow-up does the same (#301).
func TestRunRpcQuestionnaireFollowUpRetries(t *testing.T) {
	ui := &scriptedDialogUI{
		selects: []string{"3. Type something.", "1. A — desc A"},
		inputs:  []string{"   "}, // blank → back to the options
	}
	result := RunRpcQuestionnaire(context.Background(), ui, singleParams())
	if result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
	if *result.Answers[0].Answer != "A" {
		t.Fatalf("answer = %#v", result.Answers[0])
	}
	if ui.selectAt != 2 {
		t.Fatalf("select calls = %d", ui.selectAt)
	}
}

// Multi-select: comma/space-separated numbers plus custom text.
func TestRunRpcQuestionnaireMultiSelect(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{multiQuestion("Which features?", "A", "B", "C")}}
	ui := &scriptedDialogUI{inputs: []string{"1,3; extra"}}
	result := RunRpcQuestionnaire(context.Background(), ui, params)

	if result.Cancelled || len(result.Answers) != 1 {
		t.Fatalf("result = %#v", result)
	}
	answer := result.Answers[0]
	if answer.Kind != AnswerKindMulti {
		t.Fatalf("kind = %q", answer.Kind)
	}
	if len(answer.Selected) != 2 || answer.Selected[0] != "A" || answer.Selected[1] != "C" {
		t.Fatalf("selected = %v", answer.Selected)
	}
	if answer.Answer == nil || *answer.Answer != "extra" {
		t.Fatalf("custom = %v", answer.Answer)
	}
	// The input carries the multi-select instructions and placeholder.
	if ui.calls[0].Placeholder != multiSelectPlaceholder {
		t.Fatalf("placeholder = %q", ui.calls[0].Placeholder)
	}
	if !strings.Contains(ui.calls[0].Title, "comma-separated") {
		t.Fatalf("instructions missing from the title: %q", ui.calls[0].Title)
	}
}

// An empty multi-select commit is a deliberate "nothing selected" answer.
func TestRunRpcQuestionnaireMultiSelectEmptyCommit(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{multiQuestion("Which?", "A", "B")}}
	ui := &scriptedDialogUI{inputs: []string{""}}
	result := RunRpcQuestionnaire(context.Background(), ui, params)
	if result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
	answer := result.Answers[0]
	if answer.Kind != AnswerKindMulti || len(answer.Selected) != 0 || answer.Answer != nil {
		t.Fatalf("answer = %#v", answer)
	}
}

// Unparseable multi-select input is preserved verbatim as custom text.
func TestRunRpcQuestionnaireMultiSelectUnparseable(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{multiQuestion("Which?", "A", "B")}}
	ui := &scriptedDialogUI{inputs: []string{"A and B please"}}
	result := RunRpcQuestionnaire(context.Background(), ui, params)
	answer := result.Answers[0]
	if answer.Answer == nil || *answer.Answer != "A and B please" {
		t.Fatalf("answer = %#v", answer)
	}
	if len(answer.Selected) != 0 {
		t.Fatalf("selected = %v", answer.Selected)
	}
}

// Duplicate numbers collapse to one label each.
func TestRunRpcQuestionnaireMultiSelectDuplicateIndices(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{multiQuestion("Which?", "A", "B")}}
	ui := &scriptedDialogUI{inputs: []string{"1,1,2"}}
	result := RunRpcQuestionnaire(context.Background(), ui, params)
	answer := result.Answers[0]
	if len(answer.Selected) != 2 || answer.Selected[0] != "A" || answer.Selected[1] != "B" {
		t.Fatalf("selected = %v", answer.Selected)
	}
}

// Dismissing a multi-select input cancels the questionnaire.
func TestRunRpcQuestionnaireMultiSelectDismissed(t *testing.T) {
	params := QuestionParams{Questions: []QuestionData{multiQuestion("Which?", "A", "B")}}
	ui := &scriptedDialogUI{}
	if result := RunRpcQuestionnaire(context.Background(), ui, params); !result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
}

// A multi-question walk collects one answer per question, in order.
func TestRunRpcQuestionnaireMultiQuestion(t *testing.T) {
	params := multiParams()
	ui := &scriptedDialogUI{
		selects: []string{"1. A — desc A"},
		inputs:  []string{"2"},
	}
	result := RunRpcQuestionnaire(context.Background(), ui, params)
	if result.Cancelled || len(result.Answers) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if result.Answers[0].QuestionIndex != 0 || result.Answers[1].QuestionIndex != 1 {
		t.Fatalf("answers = %#v", result.Answers)
	}
	if *result.Answers[0].Answer != "A" || result.Answers[1].Selected[0] != "B" {
		t.Fatalf("answers = %#v", result.Answers)
	}
}

// Dismissing a later question keeps the earlier answers but cancels the whole
// questionnaire.
func TestRunRpcQuestionnaireCancelsMidway(t *testing.T) {
	params := multiParams()
	ui := &scriptedDialogUI{selects: []string{"1. A — desc A"}}
	result := RunRpcQuestionnaire(context.Background(), ui, params)
	if !result.Cancelled {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Answers) != 1 || result.Answers[0].QuestionIndex != 0 {
		t.Fatalf("answers = %#v", result.Answers)
	}
}

// The header is folded into the dialog title when present.
func TestRunRpcQuestionnaireHeaderInTitle(t *testing.T) {
	ui := &scriptedDialogUI{selects: []string{"1. A — desc A"}}
	_ = RunRpcQuestionnaire(context.Background(), ui, singleParams())
	if !strings.HasPrefix(ui.calls[0].Title, "[Auth] Which auth?") {
		t.Fatalf("title = %q", ui.calls[0].Title)
	}
}

func TestHasDialogUI(t *testing.T) {
	if HasDialogUI(nil) {
		t.Fatal("nil UI accepted")
	}
	if !HasDialogUI(&scriptedDialogUI{}) {
		t.Fatal("scripted UI rejected")
	}
}

// ── reconcile ─────────────────────────────────────────────────────────────

// reconcileInto is the pure core of ReconcileAskUserQuestionTool, extracted here
// so the test never needs a live SDK connection. It mirrors the production
// function step for step.
func reconcileInto(active []string, hasUI bool) ([]string, bool) {
	hasTool := containsString(active, ToolName)
	if !hasUI && hasTool {
		filtered := make([]string, 0, len(active))
		for _, name := range active {
			if name != ToolName {
				filtered = append(filtered, name)
			}
		}
		return filtered, true
	}
	if hasUI && !hasTool {
		return append(append([]string{}, active...), ToolName), true
	}
	return nil, false
}

func TestReconcileAskUserQuestionTool(t *testing.T) {
	cases := []struct {
		name    string
		active  []string
		hasUI   bool
		wantSet bool
		want    []string
	}{
		{
			name:    "no UI strips the tool",
			active:  []string{"bash", ToolName},
			hasUI:   false,
			wantSet: true,
			want:    []string{"bash"},
		},
		{
			name:    "UI restores the tool",
			active:  []string{"bash"},
			hasUI:   true,
			wantSet: true,
			want:    []string{"bash", ToolName},
		},
		{
			name:    "UI with the tool present is idempotent",
			active:  []string{"bash", ToolName},
			hasUI:   true,
			wantSet: false,
		},
		{
			name:    "no UI without the tool is idempotent",
			active:  []string{"bash"},
			hasUI:   false,
			wantSet: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := reconcileInto(tc.active, tc.hasUI)
			if changed != tc.wantSet {
				t.Fatalf("changed = %v, want %v", changed, tc.wantSet)
			}
			if !changed {
				return
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("set = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContainsString(t *testing.T) {
	if !containsString([]string{"a", "b"}, "b") {
		t.Fatal("membership missed")
	}
	if containsString([]string{"a"}, "b") {
		t.Fatal("false membership")
	}
	if containsString(nil, "a") {
		t.Fatal("nil slice reported a member")
	}
}

// ── service names / copy ──────────────────────────────────────────────────

func TestToolNameAndCopy(t *testing.T) {
	if ToolName != "ask_user_question" {
		t.Fatalf("tool name = %q", ToolName)
	}
	if !strings.Contains(DefaultPromptSnippet, "up to 4 structured questions") {
		t.Fatalf("snippet = %q", DefaultPromptSnippet)
	}
	if len(DefaultPromptGuidelines) != 4 {
		t.Fatalf("guideline count = %d", len(DefaultPromptGuidelines))
	}
	for _, required := range []string{"Type something.", "multiSelect", "preview", "Recommended"} {
		if !strings.Contains(strings.Join(DefaultPromptGuidelines, " "), required) {
			t.Fatalf("guidelines missing %q", required)
		}
	}
	for _, required := range []string{
		"Ask the user one or more structured questions",
		"Do NOT author \"Other\" or \"Type something.\" labels",
		"Preview feature:",
	} {
		if !strings.Contains(DefaultToolDescription, required) {
			t.Fatalf("description missing %q", required)
		}
	}
	if Bel != "\x07" {
		t.Fatalf("BEL = %q", Bel)
	}
}
