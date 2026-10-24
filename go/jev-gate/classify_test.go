package jevgate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// classifierRequest is the codemode-side classification request.
func classifierRequest() sdk.ClassifierContext {
	return sdk.ClassifierContext{
		State: map[string]any{"text": "report body"},
		Questions: sdk.NewOrderedObject(
			"risk", map[string]any{"type": "noul", "instructions": "Is there a risk?"},
			"kind", map[string]any{"type": "choice", "instructions": "Classify."},
		),
	}
}

// classifierModel is the model entry the provider registration declares.
func classifierModel(server *httptest.Server) map[string]any {
	return map[string]any{
		"provider": "magpie-jev",
		"id":       "group/classifier",
		"api":      classifierAPI,
		"baseUrl":  server.URL,
		"cost":     map[string]any{"input": 0.24, "output": 0},
	}
}

// TestClassifyTypesafeSystemOne pins the models.classify() transport.
func TestClassifyTypesafeSystemOne(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &body)
		if got := request.Header.Get("authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		if request.URL.Path != "/systemone" {
			t.Errorf("path = %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"model":"clef","answers":{"risk":{"type":"noul","noul":0.91},"kind":{"type":"choice","choice":"b_raises_new","probabilities":{"b_raises_new":0.93},"confidence":0.9}},"usage":{"input_tokens":100,"output_tokens":5}}`))
	}))
	defer server.Close()

	api := newTestAPI(server, 24000, 2*time.Second)
	result, err := classifyWith(api, classifierModel(server), classifierRequest(), sdk.ProviderOperationOptions{Signal: context.Background()})
	if err != nil {
		t.Fatalf("classifyWith: %v", err)
	}
	if result.API != classifierAPI || result.Provider != "magpie-jev" || result.Model != "group/classifier" {
		t.Fatalf("result = %+v", result)
	}
	if result.StopReason != "stop" || result.Timestamp == 0 {
		t.Fatalf("result = %+v", result)
	}
	if got := strings.Join(result.Answers.Keys(), ","); got != "risk,kind" {
		t.Fatalf("answers order = %q", got)
	}
	usage, _ := result.Usage["input"].(float64)
	if usage != 100 {
		t.Fatalf("usage = %v", result.Usage)
	}
	if total, _ := result.Usage["totalTokens"].(float64); total != 105 {
		t.Fatalf("usage = %v", result.Usage)
	}
	cost, _ := result.Usage["cost"].(map[string]any)
	if price, _ := cost["input"].(float64); price != 100*0.24/1e6 {
		t.Fatalf("cost = %v", result.Usage["cost"])
	}

	if body["model"] != "group/classifier" {
		t.Fatalf("request model = %v", body["model"])
	}
	state, _ := body["state"].(map[string]any)
	if state["text"] != "report body" {
		t.Fatalf("request state = %v", body["state"])
	}
	questions, _ := body["questions"].(map[string]any)
	if len(questions) != 2 {
		t.Fatalf("request questions = %v", body["questions"])
	}
}

// TestClassifyTypesafeSystemOneFailures covers the fail-closed branches.
func TestClassifyTypesafeSystemOneFailures(t *testing.T) {
	request := classifierRequest()
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"http_error", http.StatusBadGateway, `upstream down`, "HTTP 502 from"},
		{"not_json", http.StatusOK, `nope`, "non-JSON response from"},
		{"incomplete", http.StatusOK, `{"answers":{"risk":{"noul":0.5}}}`, `incomplete answers: missing key "kind"`},
		{"no_answers", http.StatusOK, `{"answers":{}}`, "response carried no answers"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := jsonServer(t, testCase.status, testCase.body)
			api := newTestAPI(server, 24000, 2*time.Second)
			_, err := classifyWith(api, classifierModel(server), request, sdk.ProviderOperationOptions{Signal: context.Background()})
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}

// TestClassifyWithoutUsage pins the absent-usage case.
func TestClassifyWithoutUsage(t *testing.T) {
	server := jsonServer(t, http.StatusOK, `{"answers":{"risk":{"noul":0.5},"kind":{"choice":"b_defers","probabilities":{"b_defers":0.9}}}}`)
	api := newTestAPI(server, 24000, 2*time.Second)
	result, err := classifyWith(api, classifierModel(server), classifierRequest(), sdk.ProviderOperationOptions{Signal: context.Background()})
	if err != nil {
		t.Fatalf("classifyWith: %v", err)
	}
	if result.Usage != nil {
		t.Fatalf("usage = %v, want nil", result.Usage)
	}
}

// TestClassifyRejectsMalformedAnswers pins the fail-closed answer validation of
// the classifier path: a response whose keys are all present but whose answers
// are structurally broken must be an error, never a "stop" result. A valid
// answer against a question table with criteria is the positive control.
func TestClassifyRejectsMalformedAnswers(t *testing.T) {
	choiceWithCriteria := sdk.ClassifierContext{
		State: map[string]any{"text": "report body"},
		Questions: sdk.NewOrderedObject(
			"kind", map[string]any{
				"type":         "choice",
				"instructions": "Classify.",
				"criteria":     map[string]any{"normal_delivery": "Normal.", "provider_error": "Upstream error."},
			},
		),
	}
	scoreWithCriteria := sdk.ClassifierContext{
		State: map[string]any{"text": "report body"},
		Questions: sdk.NewOrderedObject(
			"band", map[string]any{
				"type":         "score",
				"instructions": "Band.",
				"criteria":     map[string]any{"low": "Low.", "mid": "Mid.", "high": "High."},
			},
		),
	}
	unknownType := sdk.ClassifierContext{
		State: map[string]any{"text": "report body"},
		Questions: sdk.NewOrderedObject(
			"flag", map[string]any{"type": "bool", "instructions": "Flag."},
		),
	}
	validChoice := `"kind":{"choice":"b_defers","probabilities":{"b_defers":0.9}}`
	validNoul := `"risk":{"probability":0.5}`
	cases := []struct {
		name    string
		request sdk.ClassifierContext
		body    string
		wantErr string
	}{
		{"noul_probability_above_one", classifierRequest(), `{"answers":{` + validChoice + `,"risk":{"probability":1.5}}}`, `invalid noul answer for "risk"`},
		{"noul_probability_is_string", classifierRequest(), `{"answers":{` + validChoice + `,"risk":{"probability":"0.9"}}}`, `invalid noul answer for "risk"`},
		{"noul_answer_is_array", classifierRequest(), `{"answers":{` + validChoice + `,"risk":[]}}`, `invalid noul answer for "risk"`},
		{"choice_without_probabilities", classifierRequest(), `{"answers":{` + validNoul + `,"kind":{"choice":"b_defers"}}}`, `invalid choice answer for "kind"`},
		{"choice_probability_above_one", classifierRequest(), `{"answers":{` + validNoul + `,"kind":{"choice":"b_defers","probabilities":{"b_defers":1.5}}}}`, `invalid choice answer for "kind"`},
		{"choice_confidence_above_one", classifierRequest(), `{"answers":{` + validNoul + `,"kind":{"choice":"b_defers","probabilities":{"b_defers":0.9},"confidence":2}}}`, `invalid choice answer for "kind"`},
		{"choice_label_outside_criteria", choiceWithCriteria, `{"answers":{"kind":{"choice":"unknown_label","probabilities":{"unknown_label":0.9}}}}`, `invalid choice answer for "kind"`},
		{"score_above_range", scoreWithCriteria, `{"answers":{"band":{"score":3}}}`, `invalid score answer for "band"`},
		{"score_is_string", scoreWithCriteria, `{"answers":{"band":{"score":"1"}}}`, `invalid score answer for "band"`},
		{"unknown_question_type", unknownType, `{"answers":{"flag":{"noul":0.5}}}`, `unsupported question type "bool" for "flag"`},
		{"valid_choice_inside_criteria", choiceWithCriteria, `{"answers":{"kind":{"choice":"provider_error","probabilities":{"provider_error":0.93},"confidence":0.9}}}`, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := jsonServer(t, http.StatusOK, testCase.body)
			api := newTestAPI(server, 24000, 2*time.Second)
			result, err := classifyWith(api, classifierModel(server), testCase.request, sdk.ProviderOperationOptions{Signal: context.Background()})
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("valid answer rejected: %v", err)
				}
				if result.StopReason != "stop" {
					t.Fatalf("stopReason = %q", result.StopReason)
				}
				return
			}
			if err == nil {
				t.Fatalf("malformed answer accepted: stopReason=%q answers=%v", result.StopReason, result.Answers.Keys())
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), testCase.wantErr)
			}
			if result.StopReason == "stop" {
				t.Fatalf("malformed answer reported stopReason=%q", result.StopReason)
			}
		})
	}
}
