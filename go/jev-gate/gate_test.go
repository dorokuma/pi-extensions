package jevgate

import (
	"math"
	"strings"
	"testing"
	"time"
)

// ptr returns a probability pointer for verdict fixtures.
func ptr(value float64) *float64 { return &value }

// TestFrozenThresholds pins the calibration values: a port that changes them is
// wrong even if every other test passes.
func TestFrozenThresholds(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"FieldMissingMax", FieldMissingMax, 0.35},
		{"FieldPresentMin", FieldPresentMin, 0.8},
		{"GateClearMax", GateClearMax, 0.35},
		{"GateFlagMin", GateFlagMin, 0.8},
		{"TriageNormalMin", TriageNormalMin, 0.95},
		{"TriageAnomalyMin", TriageAnomalyMin, 0.8},
	}
	for _, testCase := range cases {
		if testCase.got != testCase.want {
			t.Errorf("%s = %v, want %v (frozen calibration value)", testCase.name, testCase.got, testCase.want)
		}
	}
}

// TestTriage3Boundaries walks the two thresholds of the three-way split.
func TestTriage3Boundaries(t *testing.T) {
	lines := []Line{
		{ID: "below", P: ptr(0.34)},
		{ID: "at_lower", P: ptr(0.35)},
		{ID: "mid", P: ptr(0.79)},
		{ID: "at_upper", P: ptr(0.80)},
		{ID: "above", P: ptr(0.99)},
		{ID: "missing"},
	}
	yes, mid, no := triage3(lines, FieldMissingMax, FieldPresentMin)
	if got, want := strings.Join(yes, ","), "at_upper,above"; got != want {
		t.Errorf("yes = %q, want %q", got, want)
	}
	if got, want := strings.Join(mid, ","), "at_lower,mid"; got != want {
		t.Errorf("mid = %q, want %q", got, want)
	}
	if got, want := strings.Join(no, ","), "below,missing"; got != want {
		t.Errorf("no = %q, want %q", got, want)
	}
}

// TestVerdictContractLint covers the three bands and the message they render.
func TestVerdictContractLint(t *testing.T) {
	worker := RoleWorker
	answers := map[string]any{
		"has_completed_list":     0.9,
		"has_files_changed":      0.5,
		"has_baseline_statement": 0.1,
		// shows_verification_commands is deliberately absent.
	}
	verdict, err := verdictFor(TaskContractLint, answers, &worker)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	want := "contract: present 1/4 (has_completed_list) | MISSING(<0.35): has_baseline_statement, shows_verification_commands | UNCERTAIN(0.35-0.8, read the return yourself): has_files_changed"
	if len(verdict.Verdict) != 1 || verdict.Verdict[0] != want {
		t.Fatalf("verdict = %q, want %q", verdict.Verdict, want)
	}
	if verdict.Note == "" {
		t.Fatal("contract_lint must carry its calibration note")
	}
	if len(verdict.Lines) != 4 {
		t.Fatalf("lines = %d, want 4 (every predefined field is reported)", len(verdict.Lines))
	}
	if verdict.Lines[3].P != nil {
		t.Fatalf("missing field must report n/a, got %v", *verdict.Lines[3].P)
	}

	all := map[string]any{
		"has_completed_list":          0.9,
		"has_files_changed":           0.9,
		"has_baseline_statement":      0.9,
		"shows_verification_commands": 0.9,
	}
	verdict, err = verdictFor(TaskContractLint, all, &worker)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	if want := "contract: present 4/4 (has_completed_list, has_files_changed, has_baseline_statement, shows_verification_commands)"; verdict.Verdict[0] != want {
		t.Fatalf("verdict = %q, want %q", verdict.Verdict[0], want)
	}
}

// TestVerdictContractLintRequiresRole checks both missing and unknown roles.
func TestVerdictContractLintRequiresRole(t *testing.T) {
	if _, err := verdictFor(TaskContractLint, map[string]any{}, nil); err == nil {
		t.Fatal("contract_lint without role must fail")
	}
	unknown := Role("reviewer2")
	if _, err := verdictFor(TaskContractLint, map[string]any{}, &unknown); err == nil {
		t.Fatal("contract_lint with an unknown role must fail")
	}
}

// TestVerdictGateScan covers flagged, uncertain and unanswered gates.
func TestVerdictGateScan(t *testing.T) {
	answers := map[string]any{
		"g1_evidence_missing_fields": 0.9,
		"g1_criterion_not_executed":  0.1,
		"g2_unresolved_must_fix":     0.5,
		"g4_leftovers_unrecorded":    0.1,
	}
	verdict, err := verdictFor(TaskGateScan, answers, nil)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	want := []string{
		"gates FLAGGED(>=0.8): g1_evidence_missing_fields",
		"gates UNCERTAIN(0.35-0.8, confirm manually): g2_unresolved_must_fix",
		"no answer for: g3_unresolved_fatal_doubt (confirm manually)",
	}
	if strings.Join(verdict.Verdict, "\n") != strings.Join(want, "\n") {
		t.Fatalf("verdict = %q, want %q", verdict.Verdict, want)
	}

	weird := Role("worker") // an irrelevant role must not change gate_scan
	clean, err := verdictFor(TaskGateScan, map[string]any{
		"g1_evidence_missing_fields": 0.1,
		"g1_criterion_not_executed":  0.1,
		"g2_unresolved_must_fix":     0.1,
		"g3_unresolved_fatal_doubt":  0.1,
		"g4_leftovers_unrecorded":    0.1,
	}, &weird)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	if clean.Verdict[0] != "gates: nothing above 0.8" {
		t.Fatalf("clean verdict = %q", clean.Verdict[0])
	}
}

// TestVerdictReturnTriage walks the 0.95 / 0.80 decision points.
func TestVerdictReturnTriage(t *testing.T) {
	choiceAnswer := func(label string, p float64) map[string]any {
		return map[string]any{
			"choice":        label,
			"probabilities": map[string]any{"normal_delivery": 0.05, label: p},
			"confidence":    0.9,
		}
	}
	cases := []struct {
		name string
		kind any
		want string
	}{
		{"normal_at_threshold", choiceAnswer("normal_delivery", 0.95), "triage: normal_delivery (p=0.95)"},
		{"normal_below", choiceAnswer("normal_delivery", 0.94), "triage: UNCERTAIN (normal_delivery p=0.94 < 0.95) → treat as anomaly"},
		{"anomaly_at_threshold", choiceAnswer("provider_error", 0.8), "triage: provider_error (p=0.80)"},
		{"anomaly_below", choiceAnswer("provider_error", 0.79), "triage: UNCERTAIN (top=provider_error p=0.79 < 0.8) → treat as anomaly"},
		{"no_answer", nil, "triage: no answer → treat as anomaly (宁可误留)"},
		{"malformed", map[string]any{"choice": "normal_delivery"}, "triage: no answer → treat as anomaly (宁可误留)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			answers := map[string]any{"states_conclusion": 0.9}
			if testCase.kind != nil {
				answers["kind"] = testCase.kind
			}
			verdict, err := verdictFor(TaskReturnTriage, answers, nil)
			if err != nil {
				t.Fatalf("verdictFor: %v", err)
			}
			if len(verdict.Verdict) != 1 || verdict.Verdict[0] != testCase.want {
				t.Fatalf("verdict = %q, want %q", verdict.Verdict, testCase.want)
			}
		})
	}
}

// TestVerdictDisagreement covers the relation label plus the three material bands.
func TestVerdictDisagreement(t *testing.T) {
	answers := map[string]any{
		"relation": map[string]any{
			"choice":        "agrees",
			"probabilities": map[string]any{"agrees": 0.98, "b_raises_new": 0.01},
			"confidence":    0.9,
		},
		"has_material_disagreement": 0.9,
		"same_finding_set":          0.95,
	}
	verdict, err := verdictFor(TaskDisagreement, answers, nil)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	want := []string{"relation: agrees (p=0.98)", "material disagreement: YES (p=0.90)"}
	if strings.Join(verdict.Verdict, "|") != strings.Join(want, "|") {
		t.Fatalf("verdict = %q, want %q", verdict.Verdict, want)
	}

	uncertain := map[string]any{"has_material_disagreement": 0.5}
	verdict, err = verdictFor(TaskDisagreement, uncertain, nil)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	want = []string{"relation: no answer", "material disagreement: UNCERTAIN (p=0.50) → read both reports"}
	if strings.Join(verdict.Verdict, "|") != strings.Join(want, "|") {
		t.Fatalf("verdict = %q, want %q", verdict.Verdict, want)
	}

	none := map[string]any{"has_material_disagreement": 0.1}
	verdict, err = verdictFor(TaskDisagreement, none, nil)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	if verdict.Verdict[1] != "material disagreement: no (p=0.10)" {
		t.Fatalf("verdict = %q", verdict.Verdict)
	}

	missing := map[string]any{}
	verdict, err = verdictFor(TaskDisagreement, missing, nil)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	if verdict.Verdict[1] != "material disagreement: no answer" {
		t.Fatalf("verdict = %q", verdict.Verdict)
	}
}

// TestVerdictSourceAudit covers the sourcing bands.
func TestVerdictSourceAudit(t *testing.T) {
	answers := map[string]any{
		"all_claims_sourced":  0.9,
		"has_verifiable_urls": 0.2,
		"marks_unverified":    0.5,
	}
	verdict, err := verdictFor(TaskSourceAudit, answers, nil)
	if err != nil {
		t.Fatalf("verdictFor: %v", err)
	}
	want := "sourcing: ok 1/3 | FLAGGED(<0.35): has_verifiable_urls | UNCERTAIN(0.35-0.8, verify yourself): marks_unverified"
	if len(verdict.Verdict) != 1 || verdict.Verdict[0] != want {
		t.Fatalf("verdict = %q, want %q", verdict.Verdict, want)
	}
}

// TestVerdictUnknownTask pins the unknown-task error.
func TestVerdictUnknownTask(t *testing.T) {
	if _, err := verdictFor(Task("nope"), map[string]any{}, nil); err == nil {
		t.Fatal("unknown task must fail")
	}
}

// TestQuestionsFor pins the question ids and types of every task.
func TestQuestionsFor(t *testing.T) {
	cases := []struct {
		task Task
		role *Role
		ids  []string
	}{
		{TaskContractLint, rolePtrFor(RoleReviewer), []string{"has_verdict", "has_findings_with_location", "has_severity_levels", "has_concrete_advice"}},
		{TaskGateScan, nil, []string{"g1_evidence_missing_fields", "g1_criterion_not_executed", "g2_unresolved_must_fix", "g3_unresolved_fatal_doubt", "g4_leftovers_unrecorded"}},
		{TaskReturnTriage, nil, []string{"kind", "states_conclusion"}},
		{TaskDisagreement, nil, []string{"has_material_disagreement", "same_finding_set", "relation"}},
		{TaskSourceAudit, nil, []string{"all_claims_sourced", "has_verifiable_urls", "marks_unverified"}},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.task), func(t *testing.T) {
			questions, err := questionsFor(testCase.task, testCase.role)
			if err != nil {
				t.Fatalf("questionsFor: %v", err)
			}
			if got := strings.Join(questions.Keys(), ","); got != strings.Join(testCase.ids, ",") {
				t.Fatalf("ids = %q, want %q", got, strings.Join(testCase.ids, ","))
			}
			for _, question := range questions {
				if question.Type != "noul" && question.Type != "choice" {
					t.Fatalf("%s has unsupported type %q", question.ID, question.Type)
				}
				if question.Type == "choice" && len(question.Criteria) == 0 {
					t.Fatalf("%s is a choice question without criteria", question.ID)
				}
			}
		})
	}

	if _, err := questionsFor(TaskContractLint, nil); err == nil {
		t.Fatal("contract_lint without role must fail")
	}
	if _, err := questionsFor(Task("bogus"), nil); err == nil {
		t.Fatal("unknown task must fail")
	}
}

func rolePtrFor(role Role) *Role { return &role }

// TestQuestionWireShape pins the JSON sent upstream: noul carries instructions,
// choice carries the ordered criteria object.
func TestQuestionWireShape(t *testing.T) {
	questions, err := questionsFor(TaskReturnTriage, nil)
	if err != nil {
		t.Fatalf("questionsFor: %v", err)
	}
	encoded, err := questions.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := `{"kind":{"type":"choice","instructions":"Classify this subagent return. Choose the single best label.","criteria":{` +
		`"normal_delivery":"Normal delivery: it states a completion conclusion and contains the required outputs.",` +
		`"provider_error":"Provider or upstream error: an error status, upstream failure or aborted run.",` +
		`"empty_return":"Empty or missing conclusion: no substantive body, or the required output is absent.",` +
		`"no_artifact_idle":"Idle with no artifact: text is present but the task goal was never reached.",` +
		`"truncated":"Visible truncation: the report breaks off and a required part is missing.",` +
		`"exec_timeout":"Execution timeout: it says a step could not run within the time limit.",` +
		`"refusal":"Refusal: the model or provider declined to perform the task.",` +
		`"evidence_incomplete":"Evidence incomplete: the verifier could not produce complete evidence."}},` +
		`"states_conclusion":{"type":"noul","instructions":"Does the text state an explicit completion conclusion and contain the required outputs of its role?"}}`
	if string(encoded) != want {
		t.Fatalf("wire questions =\n%s\nwant\n%s", encoded, want)
	}
}

// TestProbOf covers bare numbers, objects, and every rejected shape.
func TestProbOf(t *testing.T) {
	cases := []struct {
		name   string
		answer any
		want   float64
		ok     bool
	}{
		{"number", 0.75, 0.75, true},
		{"zero", 0.0, 0, true},
		{"one", 1.0, 1, true},
		{"above_one", 1.01, 0, false},
		{"negative", -0.01, 0, false},
		{"nan", math.NaN(), 0, false},
		{"string", "0.9", 0, false},
		{"bool", true, 0, false},
		{"null", nil, 0, false},
		{"object_probability", map[string]any{"probability": 0.6}, 0.6, true},
		{"object_noul", map[string]any{"noul": 0.4}, 0.4, true},
		{"object_probability_wins", map[string]any{"probability": 0.2, "noul": 0.9}, 0.2, true},
		{"object_probability_invalid", map[string]any{"probability": "0.2", "noul": 0.9}, 0, false},
		{"object_empty", map[string]any{}, 0, false},
		{"array", []any{0.5}, 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := probOf(testCase.answer)
			if ok != testCase.ok || (ok && got != testCase.want) {
				t.Fatalf("probOf(%v) = %v,%v want %v,%v", testCase.answer, got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

// TestChoiceOf covers the structural requirements of a choice answer.
func TestChoiceOf(t *testing.T) {
	valid := map[string]any{
		"choice":        "agrees",
		"probabilities": map[string]any{"agrees": 0.9, "b_defers": 0.1},
	}
	choice, ok := choiceOf(valid)
	if !ok || choice.Label != "agrees" || choice.P != 0.9 || len(choice.Probs) != 2 {
		t.Fatalf("choiceOf(valid) = %+v,%v", choice, ok)
	}
	cases := []struct {
		name   string
		answer any
	}{
		{"not_object", "agrees"},
		{"missing_choice", map[string]any{"probabilities": map[string]any{"agrees": 0.9}}},
		{"blank_choice", map[string]any{"choice": "  ", "probabilities": map[string]any{"agrees": 0.9}}},
		{"missing_probabilities", map[string]any{"choice": "agrees"}},
		{"probabilities_not_object", map[string]any{"choice": "agrees", "probabilities": []any{0.9}}},
		{"probability_out_of_range", map[string]any{"choice": "agrees", "probabilities": map[string]any{"agrees": 1.5}}},
		{"probability_not_chosen", map[string]any{"choice": "agrees", "probabilities": map[string]any{"b_defers": 0.9}}},
		{"confidence_invalid", map[string]any{"choice": "agrees", "probabilities": map[string]any{"agrees": 0.9}, "confidence": "high"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, ok := choiceOf(testCase.answer); ok {
				t.Fatalf("choiceOf(%v) accepted an invalid answer", testCase.answer)
			}
		})
	}
}

// TestOneLine pins whitespace collapsing and truncation.
func TestOneLine(t *testing.T) {
	if got := oneLine("  a \n\t b  ", 300); got != "a b" {
		t.Fatalf("oneLine collapse = %q", got)
	}
	if got := oneLine("abcdef", 3); got != "abc…" {
		t.Fatalf("oneLine truncate = %q", got)
	}
	if got := oneLine("中文中文", 2); got != "中文…" {
		t.Fatalf("oneLine must cut on a character boundary, got %q", got)
	}
}

// TestConfigFromEnv pins the environment semantics of the TS module constants.
func TestConfigFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	defaults := ConfigFromEnv(env(nil))
	if defaults.BaseURL != DefaultBaseURL || defaults.ModelID != DefaultModelID {
		t.Fatalf("defaults = %+v", defaults)
	}
	if defaults.MaxStateChars != DefaultMaxStateChars || defaults.TimeoutMs != DefaultTimeoutMs {
		t.Fatalf("numeric defaults = %+v", defaults)
	}
	if defaults.Timeout != 60*time.Second {
		t.Fatalf("timeout = %s", defaults.Timeout)
	}

	magpieWins := ConfigFromEnv(env(map[string]string{
		"MAGPIE_BASE_URL":     "http://127.0.0.1:3425/v1///",
		"WINDHUB_BASE_URL":    "https://windhub.example/v1",
		"JEV_MODEL_ID":        " group/classifier ",
		"JEV_MAX_STATE_CHARS": "10",
		"JEV_TIMEOUT_MS":      "250",
	}))
	if magpieWins.BaseURL != "http://127.0.0.1:3425/v1" {
		t.Fatalf("base url = %q", magpieWins.BaseURL)
	}
	if magpieWins.ModelID != "group/classifier" {
		t.Fatalf("model = %q", magpieWins.ModelID)
	}
	if magpieWins.MaxStateChars != 10 || magpieWins.TimeoutMs != 250 || magpieWins.Timeout != 250*time.Millisecond {
		t.Fatalf("numeric config = %+v", magpieWins)
	}

	windhubOnly := ConfigFromEnv(env(map[string]string{"WINDHUB_BASE_URL": "https://windhub.example/v1"}))
	if windhubOnly.BaseURL != "https://windhub.example/v1" {
		t.Fatalf("windhub fallback = %q", windhubOnly.BaseURL)
	}

	// Number("abc") is NaN: the cap disables truncation and Node's setTimeout
	// clamps the unusable delay to 1ms.
	invalid := ConfigFromEnv(env(map[string]string{"JEV_MAX_STATE_CHARS": "abc", "JEV_TIMEOUT_MS": "abc"}))
	if !math.IsNaN(invalid.MaxStateChars) {
		t.Fatalf("invalid cap = %v, want NaN", invalid.MaxStateChars)
	}
	if invalid.Timeout != time.Millisecond {
		t.Fatalf("invalid timeout = %s, want 1ms", invalid.Timeout)
	}
	if got := ConfigFromEnv(env(map[string]string{"JEV_TIMEOUT_MS": "0"})); got.Timeout != time.Millisecond {
		t.Fatalf("zero timeout = %s, want 1ms", got.Timeout)
	}
	if got := jsNumber("0x10"); got != 16 {
		t.Fatalf("jsNumber(0x10) = %v", got)
	}
	if got := jsNumber("Infinity"); !math.IsInf(got, 1) {
		t.Fatalf("jsNumber(Infinity) = %v", got)
	}
}

// TestTruncatePayload pins the explicit cut and its marker.
func TestTruncatePayload(t *testing.T) {
	state, truncated := truncatePayload("short", 24000)
	if state != "short" || truncated != nil {
		t.Fatalf("short payload = %q, %+v", state, truncated)
	}
	state, truncated = truncatePayload("abcdefghij", 4)
	if truncated == nil || truncated.OriginalChars != 10 || truncated.UsedChars != 4 {
		t.Fatalf("truncation = %+v", truncated)
	}
	want := "abcd\n\n[TRUNCATED: payload was 10 characters, only the first 4 were sent]"
	if state != want {
		t.Fatalf("state = %q, want %q", state, want)
	}
	if state, truncated := truncatePayload("abcdefghij", math.NaN()); truncated != nil || state != "abcdefghij" {
		t.Fatalf("NaN cap must disable truncation: %q, %+v", state, truncated)
	}
	state, truncated = truncatePayload("abcdefghij", 0)
	if truncated == nil || state == "abcdefghij" {
		t.Fatalf("zero cap must truncate to nothing: %q, %+v", state, truncated)
	}
}

// TestFormatHelpers pins the number rendering of the verdict strings.
func TestFormatHelpers(t *testing.T) {
	if got := formatP2(0.9); got != "0.90" {
		t.Fatalf("formatP2 = %q", got)
	}
	if got := jsNum(0.8); got != "0.8" {
		t.Fatalf("jsNum = %q", got)
	}
	lines := []Line{{ID: "a", P: ptr(0.93)}, {ID: "b"}}
	if got := fmtProbs(lines); got != "a=0.93, b=n/a" {
		t.Fatalf("fmtProbs = %q", got)
	}
}
