package moshihooks

import (
	"os"
	"testing"
	"time"
)

func TestSessionStartEnvelope(t *testing.T) {
	h := newHarness(t, map[string]string{"TMUX_PANE": "%7"})
	payload := payloadFor("SessionStart")

	envelope := h.deliverOne(payload)

	if envelope.Type != "session.update" {
		t.Fatalf("Type = %q, want session.update", envelope.Type)
	}
	if envelope.Source != "pi" || envelope.SessionID != "session-1" || envelope.EventName != "SessionStart" {
		t.Fatalf("identity fields = %+v", envelope)
	}
	if envelope.Cwd != "/work/moshi" || envelope.ProjectName != "moshi" {
		t.Fatalf("cwd/projectName = %q/%q", envelope.Cwd, envelope.ProjectName)
	}
	if envelope.ModelName != "group/smartass" {
		t.Fatalf("ModelName = %q", envelope.ModelName)
	}
	if envelope.RequestedAt != "2026-10-25T12:34:56.789Z" {
		t.Fatalf("RequestedAt = %q, want the frozen ISO timestamp", envelope.RequestedAt)
	}
	if envelope.MuxEnv == nil || len(envelope.MuxEnv) != 0 {
		t.Fatalf("MuxEnv = %v, want an empty object (never omitted or null)", envelope.MuxEnv)
	}
	if envelope.TmuxPane != "%7" {
		t.Fatalf("TmuxPane = %q, want %%7", envelope.TmuxPane)
	}
	// A category-less update is a silent state carrier.
	if envelope.Category != nil || envelope.Title != nil || envelope.Message != nil {
		t.Fatalf("SessionStart must publish no category/title/message: %+v", envelope)
	}
	if envelope.ContextRemaining != nil {
		t.Fatalf("ContextRemaining = %v, want absent", *envelope.ContextRemaining)
	}
}

func TestContextRemainingRounding(t *testing.T) {
	cases := []struct {
		name  string
		value float64
		want  *int
	}{
		{"rounded", 42.6, intPtr(43)},
		{"rounds up", 99.6, intPtr(100)},
		// baseEnvelope only rounds; the [1, 100] clamp belongs to
		// contextRemaining(ctx), which already produced this value.
		{"kept as sent", 150, intPtr(150)},
		{"zero is absent", 0, nil},
		{"negative is absent", -20, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			payload := payloadFor("SessionStart")
			payload.ContextRemaining = &test.value

			envelope := h.deliverOne(payload)

			switch {
			case test.want == nil && envelope.ContextRemaining != nil:
				t.Fatalf("ContextRemaining = %d, want absent", *envelope.ContextRemaining)
			case test.want != nil && envelope.ContextRemaining == nil:
				t.Fatalf("ContextRemaining absent, want %d", *test.want)
			case test.want != nil && *envelope.ContextRemaining != *test.want:
				t.Fatalf("ContextRemaining = %d, want %d", *envelope.ContextRemaining, *test.want)
			}
		})
	}
}

func TestUserPromptSubmitAnnouncesThenUpdates(t *testing.T) {
	h := newHarness(t, nil)
	payload := payloadFor("UserPromptSubmit")
	payload.Extra = map[string]any{"prompt": "  **fix**  the bug  "}

	first := h.deliverOne(payload)
	if first.EventName != "SessionStart" {
		t.Fatalf("first prompt EventName = %q, want SessionStart", first.EventName)
	}
	if first.Category == nil || *first.Category != "session_started" {
		t.Fatalf("Category = %v, want session_started", first.Category)
	}
	if first.Title == nil || *first.Title != "Pi session started" {
		t.Fatalf("Title = %v, want \"Pi session started\"", first.Title)
	}
	if first.Message == nil || *first.Message != "You: fix the bug" {
		t.Fatalf("Message = %v, want \"You: fix the bug\"", first.Message)
	}

	second := h.deliverOne(payload)
	if second.EventName != "UserPromptSubmit" {
		t.Fatalf("second prompt EventName = %q, want UserPromptSubmit", second.EventName)
	}
	if second.Title == nil || *second.Title != "Pi session started" {
		t.Fatalf("second Title = %v, want the previous event title", second.Title)
	}
}

func TestAgentStartPublishesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.deliverNothing(payloadFor("AgentStart"))
}

func TestAgentEndCompletionCooldownAndNewTurn(t *testing.T) {
	h := newHarness(t, nil)
	prompt := payloadFor("UserPromptSubmit")
	prompt.Extra = map[string]any{"prompt": "do the thing"}
	h.deliverOne(prompt)

	completion := payloadFor("AgentEnd")
	completion.Extra = map[string]any{"last_assistant_message": "**All done**"}

	envelope := h.deliverOne(completion)
	if envelope.Category == nil || *envelope.Category != "task_complete" {
		t.Fatalf("Category = %v, want task_complete", envelope.Category)
	}
	if envelope.Title == nil || *envelope.Title != "All done" {
		t.Fatalf("Title = %v, want \"All done\"", envelope.Title)
	}
	if envelope.Message == nil || *envelope.Message != "You: do the thing" {
		t.Fatalf("Message = %v, want the last user prompt", envelope.Message)
	}

	// The same stop inside the cooldown is a duplicate.
	h.deliverNothing(completion)

	// A different turn's text is not a duplicate.
	other := payloadFor("AgentEnd")
	other.Extra = map[string]any{"last_assistant_message": "Something else"}
	h.deliverOne(other)

	// Past the cooldown the same signature publishes again.
	h.advance(6 * time.Second)
	again := h.deliverOne(other)
	if again.Title == nil || *again.Title != "Something else" {
		t.Fatalf("Title after cooldown = %v", again.Title)
	}
}

func TestAgentEndWithoutAssistantTextUsesDefaultTitle(t *testing.T) {
	h := newHarness(t, nil)
	envelope := h.deliverOne(payloadFor("AgentEnd"))
	if envelope.Title == nil || *envelope.Title != "Pi complete" {
		t.Fatalf("Title = %v, want \"Pi complete\"", envelope.Title)
	}
}

func TestTurnEndIsACompletion(t *testing.T) {
	h := newHarness(t, nil)
	envelope := h.deliverOne(payloadFor("TurnEnd"))
	if envelope.EventName != "TurnEnd" || envelope.Category == nil || *envelope.Category != "task_complete" {
		t.Fatalf("TurnEnd envelope = %+v", envelope)
	}
}

func TestSessionEndEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	envelope := h.deliverOne(payloadFor("SessionEnd"))
	if envelope.Type != "session.closed" {
		t.Fatalf("Type = %q, want session.closed", envelope.Type)
	}
	if envelope.Category == nil || *envelope.Category != "session_ended" {
		t.Fatalf("Category = %v, want session_ended", envelope.Category)
	}
	if envelope.Title == nil || *envelope.Title != "Pi session ended" {
		t.Fatalf("Title = %v, want \"Pi session ended\"", envelope.Title)
	}
}

func TestChildSessionStaysOutOfTheInbox(t *testing.T) {
	h := newHarness(t, nil)
	path := writeTranscript(t, `{"type":"session","parentSession":"parent-1"}`)
	withPath := func(eventName string) *HookPayload {
		payload := payloadFor(eventName)
		payload.TranscriptPath = path
		return payload
	}

	h.deliverNothing(withPath("UserPromptSubmit"))
	h.deliverNothing(withPath("SessionEnd"))
	h.deliverNothing(withPath("AgentEnd"))

	// A lineage child still announces its start (the pane takeover is intended).
	if envelope := h.deliverOne(withPath("SessionStart")); envelope.EventName != "SessionStart" {
		t.Fatalf("child SessionStart EventName = %q", envelope.EventName)
	}
}

func TestTranscriptLineageIsCachedPerPath(t *testing.T) {
	h := newHarness(t, nil)
	path := writeTranscript(t, `{"type":"session","parentSessionId":"parent-1"}`)
	payload := payloadFor("UserPromptSubmit")
	payload.TranscriptPath = path

	h.deliverNothing(payload)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	// The verdict is cached for the path, so a later unreadable read keeps it.
	h.deliverNothing(payload)
}

func TestUnreadableTranscriptIsNotAChild(t *testing.T) {
	h := newHarness(t, nil)
	payload := payloadFor("UserPromptSubmit")
	payload.TranscriptPath = "/nonexistent/session.jsonl"
	payload.Extra = map[string]any{"prompt": "hello"}

	if envelope := h.deliverOne(payload); envelope.EventName != "SessionStart" {
		t.Fatalf("EventName = %q, want SessionStart (unknown lineage is not a child)", envelope.EventName)
	}
}

func TestPermissionRequestCarriesAnActionID(t *testing.T) {
	h := newHarness(t, nil)
	payload := payloadFor("PermissionRequest")
	payload.Extra = map[string]any{"tool_name": "bash", "reason": "**rm** -rf build"}

	envelope := h.deliverOne(payload)
	if envelope.ActionID == nil || len(*envelope.ActionID) != 32 {
		t.Fatalf("ActionID = %v, want a generated 32-hex id", envelope.ActionID)
	}
	if envelope.Phase == nil || *envelope.Phase != "waitingForApproval" {
		t.Fatalf("Phase = %v", envelope.Phase)
	}
	if envelope.Category == nil || *envelope.Category != "approval_required" {
		t.Fatalf("Category = %v", envelope.Category)
	}
	if envelope.ToolName == nil || *envelope.ToolName != "bash" {
		t.Fatalf("ToolName = %v", envelope.ToolName)
	}
	if envelope.Subtitle == nil || *envelope.Subtitle != "Answer in terminal" {
		t.Fatalf("Subtitle = %v", envelope.Subtitle)
	}
	if envelope.Message == nil || *envelope.Message != "rm -rf build" {
		t.Fatalf("Message = %v, want the clipped reason", envelope.Message)
	}

	withID := payloadFor("PermissionRequest")
	withID.Extra = map[string]any{"tool_name": "bash", "tool_use_id": "call-7", "description": "needs a shell"}
	second := h.deliverOne(withID)
	if second.ActionID == nil || *second.ActionID != "call-7" {
		t.Fatalf("ActionID = %v, want call-7", second.ActionID)
	}
	if second.Message == nil || *second.Message != "needs a shell" {
		t.Fatalf("Message = %v, want the description fallback", second.Message)
	}
}

func TestPlanReviewCarriesThePlanCard(t *testing.T) {
	h := newHarness(t, nil)
	payload := payloadFor("PlanReview")
	payload.Extra = map[string]any{
		"plan":           "# Ship it\n\nstep one",
		"tool_use_id":    "call-9",
		"plan_file_path": "local://plan.md",
	}

	envelope := h.deliverOne(payload)
	if envelope.ToolName == nil || *envelope.ToolName != "exit_plan_mode" {
		t.Fatalf("ToolName = %v, want exit_plan_mode", envelope.ToolName)
	}
	if envelope.Title == nil || *envelope.Title != "Review Pi's plan" {
		t.Fatalf("Title = %v", envelope.Title)
	}
	if envelope.Message == nil || *envelope.Message != "Ship it" {
		t.Fatalf("Message = %v, want the plan's first prose line", envelope.Message)
	}
	if envelope.InteractivePrompt == nil {
		t.Fatal("InteractivePrompt is nil")
	}
	prompt := envelope.InteractivePrompt
	if prompt.Kind != "plan" || prompt.Plan != "# Ship it\n\nstep one" ||
		prompt.ToolUseID != "call-9" || prompt.PlanFilePath != "local://plan.md" {
		t.Fatalf("InteractivePrompt = %+v", prompt)
	}

	empty := payloadFor("PlanReview")
	empty.Extra = map[string]any{"plan": ""}
	h.deliverNothing(empty)
}

func TestQuestionAskedCarriesTheAskCard(t *testing.T) {
	h := newHarness(t, nil)
	payload := payloadFor("QuestionAsked")
	payload.Extra = map[string]any{
		"tool_use_id": "call-3",
		"tool_input": map[string]any{
			"questions": []any{
				map[string]any{
					"header":   "Pick one",
					"question": "**Which** branch?",
					"multi":    true,
					"options": []any{
						map[string]any{"label": "main", "description": "stable", "preview": "p"},
						map[string]any{"label": "dev"},
						map[string]any{"description": "no label"},
					},
					"recommended": float64(0),
				},
			},
		},
	}

	envelope := h.deliverOne(payload)
	if envelope.ToolName == nil || *envelope.ToolName != "ask" {
		t.Fatalf("ToolName = %v, want ask", envelope.ToolName)
	}
	if envelope.Message == nil || *envelope.Message != "Which branch?" {
		t.Fatalf("Message = %v", envelope.Message)
	}
	prompt := envelope.InteractivePrompt
	if prompt == nil || prompt.Kind != "question" || prompt.ToolUseID != "call-3" || len(prompt.Questions) != 1 {
		t.Fatalf("InteractivePrompt = %+v", prompt)
	}
	question := prompt.Questions[0]
	if question.Header != "Pick one" || !question.MultiSelect || len(question.Options) != 2 {
		t.Fatalf("Question = %+v", question)
	}
	if !question.HasPreview || question.RecommendedIndex == nil || *question.RecommendedIndex != 0 {
		t.Fatalf("Question extras = %+v", question)
	}
	if question.Options[0].Description != "stable" || question.Options[1].Description != "" {
		t.Fatalf("Options = %+v", question.Options)
	}

	noQuestions := payloadFor("QuestionAsked")
	noQuestions.Extra = map[string]any{"tool_input": map[string]any{}}
	h.deliverNothing(noQuestions)
}

func TestPlanResolvedAndQuestionResolvedPublishNoCategory(t *testing.T) {
	h := newHarness(t, nil)

	resolved := h.deliverOne(payloadFor("PlanResolved"))
	if resolved.Category != nil || resolved.InteractivePrompt != nil {
		t.Fatalf("PlanResolved envelope = %+v", resolved)
	}

	withID := payloadFor("QuestionResolved")
	withID.Extra = map[string]any{"tool_use_id": "call-5"}
	question := h.deliverOne(withID)
	if question.InteractivePrompt == nil || question.InteractivePrompt.Kind != "question" ||
		question.InteractivePrompt.ToolUseID != "call-5" {
		t.Fatalf("QuestionResolved prompt = %+v", question.InteractivePrompt)
	}
	if question.Category != nil {
		t.Fatalf("QuestionResolved Category = %v, want absent", *question.Category)
	}

	withoutID := h.deliverOne(payloadFor("QuestionResolved"))
	if withoutID.InteractivePrompt != nil {
		t.Fatalf("QuestionResolved without tool_use_id carries %+v", withoutID.InteractivePrompt)
	}
}

func TestPermissionResolvedResumesTheTurn(t *testing.T) {
	h := newHarness(t, nil)
	prompt := payloadFor("UserPromptSubmit")
	prompt.Extra = map[string]any{"prompt": "deploy it"}
	h.deliverOne(prompt)

	request := payloadFor("PermissionRequest")
	request.Extra = map[string]any{"tool_name": "bash", "tool_use_id": "call-1"}
	h.deliverOne(request)

	resolved := payloadFor("PermissionResolved")
	resolved.Extra = map[string]any{"tool_use_id": "call-1"}
	envelope := h.deliverOne(resolved)

	if envelope.ActionID == nil || *envelope.ActionID != "call-1" {
		t.Fatalf("ActionID = %v, want call-1", envelope.ActionID)
	}
	if envelope.Category == nil || *envelope.Category != "session_started" {
		t.Fatalf("Category = %v, want session_started", envelope.Category)
	}
	if envelope.Title == nil || *envelope.Title != "Pi resumed" {
		t.Fatalf("Title = %v", envelope.Title)
	}
	if envelope.ToolName == nil || *envelope.ToolName != "bash" {
		t.Fatalf("ToolName = %v, want the remembered tool", envelope.ToolName)
	}
	if envelope.Message == nil || *envelope.Message != "You: deploy it" {
		t.Fatalf("Message = %v", envelope.Message)
	}
}

func TestUnknownEventPassesThroughBaseEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	envelope := h.deliverOne(payloadFor("SomethingElse"))
	if envelope.EventName != "SomethingElse" || envelope.Category != nil {
		t.Fatalf("default envelope = %+v", envelope)
	}
}

func TestMissingSessionIDGetsAFreshOne(t *testing.T) {
	h := newHarness(t, nil)
	first := h.deliverOne(&HookPayload{HookEventName: "SessionStart", Cwd: "/work/moshi"})
	second := h.deliverOne(&HookPayload{HookEventName: "SessionStart", Cwd: "/work/moshi"})

	if len(first.SessionID) != 32 || len(second.SessionID) != 32 {
		t.Fatalf("session ids = %q/%q, want two 32-hex ids", first.SessionID, second.SessionID)
	}
	if first.SessionID == second.SessionID {
		t.Fatalf("two id-less sessions share %q", first.SessionID)
	}
}

func TestAskQuestions(t *testing.T) {
	input := map[string]any{
		"questions": []any{
			map[string]any{
				"question":    "Pick",
				"multiSelect": true,
				"options":     []any{map[string]any{"label": "a"}, map[string]any{"label": "b"}},
				"recommended": float64(1),
			},
			map[string]any{"question": "Second", "multiple": true, "recommended": float64(9)},
			map[string]any{"question": ""},
			"not an object",
		},
	}

	questions := askQuestions(input)
	if len(questions) != 2 {
		t.Fatalf("len(questions) = %d, want 2", len(questions))
	}
	if !questions[0].MultiSelect || questions[0].RecommendedIndex == nil || *questions[0].RecommendedIndex != 1 {
		t.Fatalf("first question = %+v", questions[0])
	}
	if !questions[1].MultiSelect {
		t.Fatalf("second question must honour `multiple`: %+v", questions[1])
	}
	if questions[1].RecommendedIndex != nil {
		t.Fatalf("out-of-range recommendation must be dropped: %+v", questions[1])
	}
	if questions[1].Options == nil {
		t.Fatal("Options must be an empty array, never null")
	}
	if got := askQuestions(nil); len(got) != 0 {
		t.Fatalf("askQuestions(nil) = %v, want empty", got)
	}
}

// fakePlanContext is the session surface planFileOnDisk reads.
type fakePlanContext struct {
	cwd        string
	transcript string
	artifacts  string
}

func (f fakePlanContext) Cwd() string            { return f.cwd }
func (f fakePlanContext) TranscriptPath() string { return f.transcript }
func (f fakePlanContext) ArtifactsDir() string   { return f.artifacts }

func TestPlanFileOnDisk(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		context fakePlanContext
		want    string
	}{
		{"empty", "", fakePlanContext{cwd: "/work"}, ""},
		{"absolute", "/tmp/plan.md", fakePlanContext{cwd: "/work"}, "/tmp/plan.md"},
		{"relative to cwd", "plan.md", fakePlanContext{cwd: "/work/moshi"}, "/work/moshi/plan.md"},
		{"local with artifacts dir", "local://plan.md", fakePlanContext{artifacts: "/sessions/abc-artifacts"}, "/sessions/abc-artifacts/local/plan.md"},
		{"local falls back to transcript", "local://plan.md", fakePlanContext{transcript: "/sessions/abc.jsonl"}, "/sessions/abc/local/plan.md"},
		{"local without anything", "local://plan.md", fakePlanContext{}, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := planFileOnDisk(test.path, test.context); got != test.want {
				t.Fatalf("planFileOnDisk(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

func TestTranscriptHeaderIsChild(t *testing.T) {
	path := writeTranscript(t, `{"type":"session","parentSession":"parent-1"}`)
	child, known := transcriptHeaderIsChild(path)
	if !known || !child {
		t.Fatalf("child header = %v/%v, want true/true", child, known)
	}

	root := writeTranscript(t, `{"type":"session","id":"root"}`)
	child, known = transcriptHeaderIsChild(root)
	if !known || child {
		t.Fatalf("root header = %v/%v, want false/true", child, known)
	}

	if _, known := transcriptHeaderIsChild("/nonexistent/session.jsonl"); known {
		t.Fatal("unreadable transcript must be unknown, not a verdict")
	}
}

func TestRecordTaskCompletionSuppressesOnlyTheSameSignature(t *testing.T) {
	state := &SessionState{PromptSequence: 1}
	if !recordTaskCompletion(state, "done", 100) {
		t.Fatal("first completion must record")
	}
	if recordTaskCompletion(state, "done", 101) {
		t.Fatal("same signature inside the cooldown must be suppressed")
	}
	if !recordTaskCompletion(state, "other", 101) {
		t.Fatal("a different signature must record")
	}
	if !recordTaskCompletion(state, "other", 106) {
		t.Fatal("the same signature past the cooldown must record")
	}
	if state.CompletedPromptSequence != 1 {
		t.Fatalf("CompletedPromptSequence = %d, want 1", state.CompletedPromptSequence)
	}

	// An empty signature defaults to "pi-complete", so two empty-signature stops
	// inside the cooldown are duplicates.
	defaults := &SessionState{PromptSequence: 1}
	if !recordTaskCompletion(defaults, "", 300) {
		t.Fatal("first empty-signature completion must record")
	}
	if defaults.LastStopSignature != "pi-complete" {
		t.Fatalf("LastStopSignature = %q, want pi-complete", defaults.LastStopSignature)
	}
	if recordTaskCompletion(defaults, "", 301) {
		t.Fatal("empty signature inside the cooldown must be suppressed")
	}
}

func TestPromptSequenceForSubmit(t *testing.T) {
	cases := []struct {
		state SessionState
		want  int
	}{
		{SessionState{}, 1},
		{SessionState{PromptSequence: 3}, 3},
		{SessionState{PromptSequence: 3, CompletedPromptSequence: 3}, 4},
		{SessionState{PromptSequence: 3, CompletedPromptSequence: 4}, 4},
	}
	for _, test := range cases {
		if got := promptSequenceForSubmit(&test.state); got != test.want {
			t.Fatalf("promptSequenceForSubmit(%+v) = %d, want %d", test.state, got, test.want)
		}
	}
}
