package jevgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// newTestAPI builds a JevAPI against a test server with an injected credential.
func newTestAPI(server *httptest.Server, maxStateChars float64, timeout time.Duration) JevAPI {
	return JevAPI{
		Config: Config{
			BaseURL:       server.URL,
			ModelID:       DefaultModelID,
			MaxStateChars: maxStateChars,
			TimeoutMs:     float64(timeout / time.Millisecond),
			Timeout:       timeout,
		},
		Lookup: KeyLookup{Getenv: func(name string) string {
			if name == "MAGPIE_API_KEY" {
				return "test-key"
			}
			return ""
		}},
		Client: server.Client(),
	}
}

// jsonServer answers every request with one fixed body and status.
func jsonServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// triageQuestions is the question table used by the HTTP tests.
func triageQuestions(t *testing.T) Questions {
	t.Helper()
	questions, err := questionsFor(TaskReturnTriage, nil)
	if err != nil {
		t.Fatalf("questionsFor: %v", err)
	}
	return questions
}

// TestCallJevAcceptedEnvelopes covers the top-level and wrapped carriers.
func TestCallJevAcceptedEnvelopes(t *testing.T) {
	answers := `{"kind":{"choice":"provider_error","probabilities":{"normal_delivery":0.02,"provider_error":0.95},"confidence":0.9},` +
		`"states_conclusion":{"type":"noul","noul":0.1}}`
	cases := []struct {
		name string
		body string
	}{
		{"top_level", `{"model":"clef","answers":` + answers + `,"usage":{"input_tokens":120,"output_tokens":8}}`},
		{"result_wrapper", `{"result":{"model":"clef","answers":` + answers + `,"usage":{"input_tokens":120,"output_tokens":8}}}`},
		{"double_wrapper", `{"result":{"state":"Completed","result":{"model":"clef","answers":` + answers + `,"usage":{"input_tokens":120,"output_tokens":8}}}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := jsonServer(t, http.StatusOK, testCase.body)
			call, err := newTestAPI(server, 24000, 2*time.Second).Call(context.Background(), "payload", triageQuestions(t))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if call.Model != "clef" {
				t.Fatalf("model = %q", call.Model)
			}
			if call.Usage == nil || call.Usage.Input != 120 || call.Usage.Output != 8 {
				t.Fatalf("usage = %+v", call.Usage)
			}
			choice, ok := choiceOfMap(call.Answers, "kind")
			if !ok || choice.Label != "provider_error" {
				t.Fatalf("answers = %v", call.Answers)
			}
		})
	}
}

// TestCallJevUsageAliases pins `u.a ?? u.b ?? u.c` and the || 0 fallback.
func TestCallJevUsageAliases(t *testing.T) {
	template := `{"answers":{"kind":{"choice":"provider_error","probabilities":{"provider_error":0.95}},"states_conclusion":{"noul":0.1}},"usage":%s}`
	cases := []struct {
		usage string
		in    float64
		out   float64
	}{
		{`{"input_tokens":5,"output_tokens":6}`, 5, 6},
		{`{"prompt_tokens":7,"completion_tokens":8}`, 7, 8},
		{`{"input":9,"output":10}`, 9, 10},
		{`{"input_tokens":"11","output":"12"}`, 11, 12},
		{`{"input_tokens":"abc","output_tokens":null}`, 0, 0},
		{`{}`, 0, 0},
	}
	for _, testCase := range cases {
		server := jsonServer(t, http.StatusOK, fmt.Sprintf(template, testCase.usage))
		call, err := newTestAPI(server, 24000, 2*time.Second).Call(context.Background(), "payload", triageQuestions(t))
		if err != nil {
			t.Fatalf("Call(%s): %v", testCase.usage, err)
		}
		if call.Usage == nil || call.Usage.Input != testCase.in || call.Usage.Output != testCase.out {
			t.Fatalf("usage %s = %+v, want in=%v out=%v", testCase.usage, call.Usage, testCase.in, testCase.out)
		}
	}
}

// TestCallJevRequestShape pins the request the extension sends.
func TestCallJevRequestShape(t *testing.T) {
	var captured struct {
		method      string
		path        string
		auth        string
		contentType string
		body        map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		captured.method = request.Method
		captured.path = request.URL.Path
		captured.auth = request.Header.Get("authorization")
		captured.contentType = request.Header.Get("content-type")
		_ = json.Unmarshal(raw, &captured.body)
		_, _ = io.WriteString(writer, `{"answers":{"g1_evidence_missing_fields":{"noul":0.1}}}`)
	}))
	defer server.Close()

	questions, err := questionsFor(TaskGateScan, nil)
	if err != nil {
		t.Fatalf("questionsFor: %v", err)
	}
	// Only one answer is returned, so the call fails on completeness after the
	// request has been observed.
	_, _ = newTestAPI(server, 24000, 2*time.Second).Call(context.Background(), "the payload", questions)

	if captured.method != http.MethodPost {
		t.Fatalf("method = %q", captured.method)
	}
	if captured.path != "/systemone" {
		t.Fatalf("path = %q", captured.path)
	}
	if captured.auth != "Bearer test-key" {
		t.Fatalf("authorization = %q", captured.auth)
	}
	if !strings.HasPrefix(captured.contentType, "application/json") {
		t.Fatalf("content-type = %q", captured.contentType)
	}
	if captured.body["model"] != DefaultModelID {
		t.Fatalf("model = %v", captured.body["model"])
	}
	if captured.body["state"] != "the payload" {
		t.Fatalf("state = %v", captured.body["state"])
	}
	questionsBody, ok := captured.body["questions"].(map[string]any)
	if !ok || len(questionsBody) != 5 {
		t.Fatalf("questions = %v", captured.body["questions"])
	}
	first, ok := questionsBody["g1_evidence_missing_fields"].(map[string]any)
	if !ok || first["type"] != "noul" || first["instructions"] == "" {
		t.Fatalf("question wire shape = %v", questionsBody["g1_evidence_missing_fields"])
	}
}

// TestOversizedResponseBodyFailsClosed pins the bounded response read of both
// transports: a body above maxResponseBodyBytes is an explicit error, never a
// silently truncated parse.
func TestOversizedResponseBodyFailsClosed(t *testing.T) {
	oversized := strings.Repeat("a", maxResponseBodyBytes+1)
	t.Run("jev_call", func(t *testing.T) {
		server := jsonServer(t, http.StatusOK, oversized)
		_, err := newTestAPI(server, 24000, 2*time.Second).Call(context.Background(), "payload", triageQuestions(t))
		if err == nil || !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("error = %v, want %v", err, errResponseBodyTooLarge)
		}
	})
	t.Run("classify", func(t *testing.T) {
		server := jsonServer(t, http.StatusOK, oversized)
		_, err := classifyWith(newTestAPI(server, 24000, 2*time.Second), classifierModel(server), classifierRequest(), sdk.ProviderOperationOptions{Signal: context.Background()})
		if err == nil || !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("error = %v, want %v", err, errResponseBodyTooLarge)
		}
	})
}

// TestCallJevFailures covers the HTTP and payload failure branches.
func TestCallJevFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"rate_limited", http.StatusTooManyRequests, `{"error":"daily quota exhausted"}`, "HTTP 429 from"},
		{"server_error", http.StatusInternalServerError, `boom`, "HTTP 500 from"},
		{"not_json", http.StatusOK, `<html>nope</html>`, "non-JSON response from"},
		{"json_null", http.StatusOK, `null`, "non-object JSON response from"},
		{"json_array", http.StatusOK, `[]`, "non-object JSON response from"},
		{"no_answers", http.StatusOK, `{"model":"clef","answers":{}}`, "response carried no answers"},
		{"answers_not_object", http.StatusOK, `{"answers":[]}`, "response carried no answers"},
		{"incomplete", http.StatusOK, `{"answers":{"states_conclusion":{"noul":0.1}}}`, `incomplete answers: missing key "kind"`},
		{"invalid_noul", http.StatusOK, `{"answers":{"kind":{"choice":"provider_error","probabilities":{"provider_error":0.95}},"states_conclusion":{"noul":"0.1"}}}`, `invalid noul answer for "states_conclusion"`},
		{"invalid_choice", http.StatusOK, `{"answers":{"kind":{"choice":"unknown_label","probabilities":{"unknown_label":0.95}},"states_conclusion":{"noul":0.1}}}`, `invalid choice answer for "kind"`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := jsonServer(t, testCase.status, testCase.body)
			_, err := newTestAPI(server, 24000, 2*time.Second).Call(context.Background(), "payload", triageQuestions(t))
			if err == nil {
				t.Fatal("call must fail")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), testCase.wantErr)
			}
		})
	}
}

// TestCallJevRateLimitedBody pins the one-line detail of a non-2xx response.
func TestCallJevRateLimitedBody(t *testing.T) {
	server := jsonServer(t, http.StatusTooManyRequests, "  quota\nexhausted  ")
	_, err := newTestAPI(server, 24000, 2*time.Second).Call(context.Background(), "payload", triageQuestions(t))
	if err == nil || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("error = %v", err)
	}
}

// TestCallJevTimeout pins the abort message of the configured timeout.
func TestCallJevTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	start := time.Now()
	_, err := newTestAPI(server, 24000, 40*time.Millisecond).Call(context.Background(), "payload", triageQuestions(t))
	if err == nil || err.Error() != "timeout after 40ms" {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

// TestCallJevCallerCancellation pins the caller-abort message.
func TestCallJevCallerCancellation(t *testing.T) {
	server := jsonServer(t, http.StatusOK, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newTestAPI(server, 24000, 2*time.Second).Call(ctx, "payload", triageQuestions(t))
	if err == nil || err.Error() != "aborted by caller" {
		t.Fatalf("error = %v", err)
	}
}

// TestCallJevMissingCredential pins the fail-closed credential error.
func TestCallJevMissingCredential(t *testing.T) {
	api := JevAPI{
		Config: Config{BaseURL: "https://windhub.example/v1", ModelID: DefaultModelID, MaxStateChars: 24000, TimeoutMs: 2000, Timeout: 2 * time.Second},
		Lookup: KeyLookup{
			Getenv:   func(string) string { return "" },
			ReadFile: func(string) ([]byte, error) { return nil, errors.New("no such file") },
			HomeDir:  func() (string, error) { return "/home/test", nil },
		},
	}
	_, err := api.Call(context.Background(), "payload", triageQuestions(t))
	if err == nil || !strings.HasPrefix(err.Error(), "missing MAGPIE_API_KEY/WINDHUB_API_KEY") {
		t.Fatalf("error = %v", err)
	}
}

// TestCallJevTruncatesPayload pins the explicit cut and the call's record of it.
func TestCallJevTruncatesPayload(t *testing.T) {
	var state string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			State string `json:"state"`
		}
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &body)
		state = body.State
		_, _ = io.WriteString(writer, `{"answers":{"kind":{"choice":"provider_error","probabilities":{"provider_error":0.95}},"states_conclusion":{"noul":0.1}}}`)
	}))
	defer server.Close()

	call, err := newTestAPI(server, 5, 2*time.Second).Call(context.Background(), "abcdefghij", triageQuestions(t))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.HasPrefix(state, "abcde") || !strings.Contains(state, "[TRUNCATED: payload was 10 characters, only the first 5 were sent]") {
		t.Fatalf("state = %q", state)
	}
	if call.Truncated == nil || call.Truncated.OriginalChars != 10 || call.Truncated.UsedChars != 5 {
		t.Fatalf("truncation = %+v", call.Truncated)
	}
}

// TestUnwrapSystemOneBody pins the carrier selection.
func TestUnwrapSystemOneBody(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"top_level", map[string]any{"answers": map[string]any{"a": 1}, "model": "top"}, "top"},
		{"result", map[string]any{"result": map[string]any{"answers": map[string]any{"a": 1}, "model": "result"}}, "result"},
		{"double", map[string]any{"result": map[string]any{"result": map[string]any{"answers": map[string]any{"a": 1}, "model": "inner"}}}, "inner"},
		{"result_without_answers", map[string]any{"result": map[string]any{"model": "empty"}}, "empty"},
		{"not_an_object", "text", ""},
		{"null", nil, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			unwrapped := unwrapSystemOneBody(testCase.body)
			model, _ := unwrapped["model"].(string)
			if model != testCase.want {
				t.Fatalf("model = %q, want %q", model, testCase.want)
			}
		})
	}
}

// TestValidateAnswers pins the fail-closed answer validation.
func TestValidateAnswers(t *testing.T) {
	scoreQuestion := Questions{{ID: "s", Type: "score", Criteria: []Criterion{{Label: "a"}, {Label: "b"}, {Label: "c"}}}}
	if err := validateAnswers(scoreQuestion, map[string]any{"s": map[string]any{"score": 2, "confidence": 0.9}}); err != nil {
		t.Fatalf("valid score rejected: %v", err)
	}
	cases := []struct {
		name      string
		questions Questions
		answers   map[string]any
		wantErr   string
	}{
		{"missing", triageQuestions(t), map[string]any{"kind": map[string]any{"choice": "provider_error", "probabilities": map[string]any{"provider_error": 0.9}}}, `incomplete answers: missing key "states_conclusion"`},
		{"null_answer", triageQuestions(t), map[string]any{"kind": nil, "states_conclusion": map[string]any{"noul": 0.1}}, `incomplete answers: missing key "kind"`},
		{"score_too_high", scoreQuestion, map[string]any{"s": map[string]any{"score": 3}}, `invalid score answer for "s"`},
		{"score_negative", scoreQuestion, map[string]any{"s": map[string]any{"score": -1}}, `invalid score answer for "s"`},
		{"score_not_number", scoreQuestion, map[string]any{"s": map[string]any{"score": "1"}}, `invalid score answer for "s"`},
		{"unsupported_type", Questions{{ID: "x", Type: "bool"}}, map[string]any{"x": map[string]any{"noul": 0.5}}, `unsupported question type "bool" for "x"`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateAnswers(testCase.questions, testCase.answers)
			if err == nil || err.Error() != testCase.wantErr {
				t.Fatalf("error = %v, want %q", err, testCase.wantErr)
			}
		})
	}
}
