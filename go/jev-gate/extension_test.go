package jevgate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// TestExecuteCheckInvalidTask pins the invalid-task branch and its shape.
func TestExecuteCheckInvalidTask(t *testing.T) {
	result := ExecuteCheck(map[string]any{"payload": "text"})
	if !result.IsError {
		t.Fatal("invalid task must be an error result")
	}
	want := "jev_check: invalid task undefined; expected one of " + taskList()
	if result.Content != want {
		t.Fatalf("content = %q, want %q", result.Content, want)
	}
	details, _ := result.Details.(map[string]any)
	if details["summary"] != want {
		t.Fatalf("details = %v", result.Details)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	if structured["error"] != want {
		t.Fatalf("structuredContent = %v", structured)
	}

	unknown := ExecuteCheck(map[string]any{"task": "judge", "payload": "text"})
	if !unknown.IsError || !strings.Contains(unknown.Content, "jev_check: invalid task judge") {
		t.Fatalf("unknown task = %q", unknown.Content)
	}
}

// TestExecuteCheckEmptyPayload pins the empty-payload branch.
func TestExecuteCheckEmptyPayload(t *testing.T) {
	result := ExecuteCheck(map[string]any{"task": "gate_scan", "payload": "   \n\t "})
	if !result.IsError {
		t.Fatal("empty payload must be an error result")
	}
	if result.Content != "jev_check: payload is empty; nothing to judge" {
		t.Fatalf("content = %q", result.Content)
	}
}

// TestExecuteCheckContractLintRole pins the role requirement.
func TestExecuteCheckContractLintRole(t *testing.T) {
	want := "jev_check: contract_lint requires role in " + roleList()
	for _, params := range []map[string]any{
		{"task": "contract_lint", "payload": "text"},
		{"task": "contract_lint", "payload": "text", "role": ""},
		{"task": "contract_lint", "payload": "text", "role": "wizard"},
	} {
		result := ExecuteCheck(params)
		if !result.IsError || result.Content != want {
			t.Fatalf("params %v → %q, want %q", params, result.Content, want)
		}
	}
}

// gateScanBody is one complete gate_scan answer set.
const gateScanBody = `{"model":"clef","answers":{` +
	`"g1_evidence_missing_fields":{"noul":0.92},` +
	`"g1_criterion_not_executed":{"noul":0.02},` +
	`"g2_unresolved_must_fix":{"noul":0.5},` +
	`"g3_unresolved_fatal_doubt":{"noul":0.03},` +
	`"g4_leftovers_unrecorded":{"noul":0.04}},` +
	`"usage":{"input_tokens":10,"output_tokens":2}}`

// TestExecuteCheckSuccess pins the rendered content and structured result.
func TestExecuteCheckSuccess(t *testing.T) {
	server := jsonServer(t, http.StatusOK, gateScanBody)
	result := executeCheck(newTestAPI(server, 24000, 2*time.Second), context.Background(),
		map[string]any{"task": "gate_scan", "payload": "report body"})
	if result.IsError {
		t.Fatalf("unexpected error: %q", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	if lines[0] != "task=gate_scan model=clef" {
		t.Fatalf("headline = %q", lines[0])
	}
	if lines[1] != "verdict: gates FLAGGED(>=0.8): g1_evidence_missing_fields | gates UNCERTAIN(0.35-0.8, confirm manually): g2_unresolved_must_fix" {
		t.Fatalf("verdict line = %q", lines[1])
	}
	if lines[2] != "note: 单向信任：命中即可阻断；中间带仅提示人工确认，避免对干净交付误报。" {
		t.Fatalf("note line = %q", lines[2])
	}
	wantProbs := "probs: g1_evidence_missing_fields=0.92, g1_criterion_not_executed=0.02, g2_unresolved_must_fix=0.50, g3_unresolved_fatal_doubt=0.03, g4_leftovers_unrecorded=0.04"
	if lines[3] != wantProbs {
		t.Fatalf("probs line = %q, want %q", lines[3], wantProbs)
	}
	if lines[4] != "usage: in=10 out=2" {
		t.Fatalf("usage line = %q", lines[4])
	}
	if lines[len(lines)-1] != advisory {
		t.Fatalf("last line = %q", lines[len(lines)-1])
	}

	structured, _ := result.StructuredContent.(map[string]any)
	if structured["task"] != "gate_scan" || structured["model"] != "clef" || structured["advisory"] != true {
		t.Fatalf("structured = %v", structured)
	}
	verdict, _ := structured["verdict"].([]string)
	if len(verdict) != 2 {
		t.Fatalf("structured verdict = %v", structured["verdict"])
	}
	probabilities, _ := structured["probabilities"].(map[string]any)
	if probabilities["g1_evidence_missing_fields"] != 0.92 {
		t.Fatalf("probabilities = %v", probabilities)
	}
	usage, _ := structured["usage"].(map[string]any)
	if usage["input"] != 10.0 || usage["output"] != 2.0 {
		t.Fatalf("structured usage = %v", structured["usage"])
	}
	details, _ := result.Details.(map[string]any)
	if details["summary"] != verdict[0] {
		t.Fatalf("details summary = %v", result.Details)
	}
}

// TestExecuteCheckContractLintSuccess pins the role echo and missing-field n/a.
func TestExecuteCheckContractLintSuccess(t *testing.T) {
	body := `{"answers":{"has_verdict":{"noul":0.95},"has_findings_with_location":{"noul":0.2},` +
		`"has_severity_levels":{"noul":0.9},"has_concrete_advice":{"noul":0.4}},` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	server := jsonServer(t, http.StatusOK, body)
	result := executeCheck(newTestAPI(server, 24000, 2*time.Second), context.Background(),
		map[string]any{"task": "contract_lint", "payload": "review", "role": "reviewer"})
	if result.IsError {
		t.Fatalf("unexpected error: %q", result.Content)
	}
	if !strings.Contains(result.Content, "task=contract_lint role=reviewer model=") {
		t.Fatalf("headline = %q", result.Content)
	}
	if !strings.Contains(result.Content, "MISSING(<0.35): has_findings_with_location") {
		t.Fatalf("missing field must be reported: %q", result.Content)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	if structured["role"] != "reviewer" {
		t.Fatalf("structured role = %v", structured["role"])
	}
}

// TestExecuteCheckTruncation pins the truncation headline and structured field.
func TestExecuteCheckTruncation(t *testing.T) {
	server := jsonServer(t, http.StatusOK, gateScanBody)
	api := newTestAPI(server, 5, 2*time.Second)
	result := executeCheck(api, context.Background(), map[string]any{"task": "gate_scan", "payload": "abcdefghij"})
	if !strings.Contains(result.Content, "TRUNCATED: sent 5/10 chars") {
		t.Fatalf("content = %q", result.Content)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	truncated, ok := structured["truncated"].(map[string]any)
	if !ok || truncated["originalChars"] != 10 || truncated["usedChars"] != 5 {
		t.Fatalf("structured truncated = %v", structured["truncated"])
	}
}

// TestExecuteCheckHTTPError pins the catch branch.
func TestExecuteCheckHTTPError(t *testing.T) {
	server := jsonServer(t, http.StatusTooManyRequests, "quota exhausted")
	result := executeCheck(newTestAPI(server, 24000, 2*time.Second), context.Background(),
		map[string]any{"task": "gate_scan", "payload": "report"})
	if !result.IsError {
		t.Fatal("HTTP failure must be an error result")
	}
	if !strings.HasPrefix(result.Content, "jev_check failed: HTTP 429 from ") ||
		!strings.Contains(result.Content, "quota exhausted") {
		t.Fatalf("content = %q", result.Content)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	if structured["task"] != "gate_scan" || structured["advisory"] != true || structured["error"] == nil {
		t.Fatalf("structured = %v", structured)
	}
	details, _ := result.Details.(map[string]any)
	if details["summary"] != result.Content {
		t.Fatalf("details = %v", result.Details)
	}
}

// TestJevCheckSchema pins every field of the parameter schema.
func TestJevCheckSchema(t *testing.T) {
	schema := jevCheckSchema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"task", "payload", "role"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("parameter %q missing", name)
		}
	}
	task, _ := properties["task"].(map[string]any)
	enum, _ := task["enum"].([]any)
	if len(enum) != 5 || enum[0] != "contract_lint" || enum[4] != "source_audit" {
		t.Fatalf("task enum = %v", task["enum"])
	}
	role, _ := properties["role"].(map[string]any)
	roleEnum, _ := role["enum"].([]any)
	if len(roleEnum) != 7 || roleEnum[0] != "scout" || roleEnum[6] != "oracle" {
		t.Fatalf("role enum = %v", role["enum"])
	}
	required, _ := schema["required"].([]any)
	if len(required) != 2 || required[0] != "task" || required[1] != "payload" {
		t.Fatalf("required = %v", schema["required"])
	}
}

// TestToolDefinition pins the registration fields the TS version sets.
func TestToolDefinition(t *testing.T) {
	definition := toolDefinition(DefaultJevAPI())
	if definition.Name != "jev_check" || definition.Label != "JEV Check" {
		t.Fatalf("definition = %+v", definition)
	}
	if !strings.Contains(definition.Description, "System One") {
		t.Fatalf("description = %q", definition.Description)
	}
	if definition.PromptSnippet != "Structured classifier checks (contract / gates / triage / disagreement / sourcing)" {
		t.Fatalf("prompt snippet = %q", definition.PromptSnippet)
	}
	if len(definition.PromptGuidelines) != 6 {
		t.Fatalf("guidelines = %d", len(definition.PromptGuidelines))
	}
	for _, guideline := range definition.PromptGuidelines {
		if !strings.Contains(guideline, "jev_check") {
			t.Fatalf("guideline must name the tool: %q", guideline)
		}
	}
	if definition.Annotations == nil || definition.Annotations.ReadOnlyHint == nil || !*definition.Annotations.ReadOnlyHint {
		t.Fatalf("annotations = %+v (the tool must be read-only)", definition.Annotations)
	}
	if definition.Execute == nil {
		t.Fatal("Execute must be set")
	}
}

// TestProviderConfig pins the registerProvider payload.
func TestProviderConfig(t *testing.T) {
	config := Config{BaseURL: "http://127.0.0.1:3425/v1", ModelID: "group/classifier"}
	payload := providerConfig(config, "magpie")
	if payload["name"] != "Magpie (Jev / Clef)" || payload["baseUrl"] != config.BaseURL || payload["apiKey"] != "magpie" {
		t.Fatalf("payload = %v", payload)
	}
	models, _ := payload["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %v", payload["models"])
	}
	model, _ := models[0].(map[string]any)
	if model["type"] != "classifier" || model["api"] != classifierAPI || model["id"] != "group/classifier" {
		t.Fatalf("model = %v", model)
	}
	if model["baseUrl"] != config.BaseURL || model["contextWindow"] != 65536 {
		t.Fatalf("model = %v", model)
	}
	cost, _ := model["cost"].(map[string]any)
	if cost["input"] != 0.24 {
		t.Fatalf("cost = %v", model["cost"])
	}
	classifiers, ok := payload["classifiers"].(map[string]sdk.ProviderClassifyFunc)
	if !ok || classifiers[classifierAPI] == nil {
		t.Fatalf("classifiers = %v", payload["classifiers"])
	}
	// The config must stay encodable for the register frame: callbacks are
	// stripped before the wire, so the rest has to be plain JSON.
	copy := sdk.ProviderConfig{}
	for key, value := range payload {
		if key == "classifiers" {
			continue
		}
		copy[key] = value
	}
	if _, err := json.Marshal(copy); err != nil {
		t.Fatalf("provider config is not encodable: %v", err)
	}
}

// TestNewExtensionBranches drives both factory branches without a host and
// without the production herdr socket (the identity verdict is injected).
func TestNewExtensionBranches(t *testing.T) {
	api := JevAPI{
		Config: Config{BaseURL: "http://127.0.0.1:3425/v1", ModelID: DefaultModelID, MaxStateChars: 24000, TimeoutMs: 2000, Timeout: 2 * time.Second},
		Lookup: KeyLookup{Getenv: func(name string) string {
			if name == "MAGPIE_API_KEY" {
				return "test-key"
			}
			return ""
		}},
		Client: &http.Client{},
	}

	mainAgent := newExtension(func() bool { return false }, api)
	if mainAgent == nil || mainAgent.Name() != "jev-gate" {
		t.Fatalf("main-agent extension = %v", mainAgent)
	}
	subagent := newExtension(func() bool { return true }, api)
	if subagent == nil || subagent.Name() != "jev-gate" {
		t.Fatalf("subagent extension = %v", subagent)
	}
}

// TestExecuteCheckConcurrent hammers one endpoint from several goroutines; run
// with -race this also covers the shared maps of the verdict builders.
func TestExecuteCheckConcurrent(t *testing.T) {
	server := jsonServer(t, http.StatusOK, gateScanBody)
	api := newTestAPI(server, 24000, 2*time.Second)
	var waitGroup sync.WaitGroup
	errorsFound := make(chan string, 64)
	for worker := 0; worker < 8; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for iteration := 0; iteration < 8; iteration++ {
				result := executeCheck(api, context.Background(), map[string]any{"task": "gate_scan", "payload": "report"})
				if result.IsError {
					errorsFound <- result.Content
					return
				}
			}
		}()
	}
	waitGroup.Wait()
	close(errorsFound)
	for message := range errorsFound {
		t.Fatalf("concurrent call failed: %s", message)
	}
}

// TestExecuteCheckRequestBodyIsSmall guards against a payload that is silently
// re-encoded per call (the body must be the caller's text, not a JSON dump).
func TestExecuteCheckRequestBodyIsSmall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		if !bytes.Contains(raw, []byte(`"state":"plain text"`)) {
			t.Errorf("request body = %s", raw)
		}
		_, _ = io.WriteString(writer, gateScanBody)
	}))
	defer server.Close()
	result := executeCheck(newTestAPI(server, 24000, 2*time.Second), context.Background(),
		map[string]any{"task": "gate_scan", "payload": "plain text"})
	if result.IsError {
		t.Fatalf("unexpected error: %q", result.Content)
	}
}
