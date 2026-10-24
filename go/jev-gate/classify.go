package jevgate

// The classifier implementation registered with the magpie-jev provider.
//
// Why this exists (PiG gap, PLAN hard rule 6 — do not patch PiG):
// the TS extension registers only a classifier *model* with `api:
// "typesafe-system-one"` and relies on pi-ai's built-in transport, which
// resolves by API name for any provider. PiG resolves a provider's classifier
// implementation per provider (coding/extension/model_registry.go
// ProviderConfigInput.Classifiers, internal/codingagent/native_provider_composer.go
// composeNativeProvider: `base.Classify` comes from the provider's own
// Classifiers map), and a custom provider id such as `magpie-jev` has no
// built-in entry — models.classify() would answer "Provider magpie-jev does not
// support classification". Registering the same wire protocol through the SDK's
// classifiers callback is the widest equivalent surface the SDK offers; it keeps
// models.classify() working against the local Magpie gateway.
//
// Wire parity with PiG's ai.ClassifyTypesafeSystemOne
// (ref/PiG/ai/system_one.go: typesafeSystemOneTransport):
//   - URL: <baseUrl with trailing slashes removed>/systemone
//   - body: { model, state, questions } (public bool questions are sent as noul
//     by the caller's own table; this extension never sends `type:"bool"`)
//   - response: { answers, usage } either directly or inside `result`.
//
// jev_check itself never uses this path: it calls the endpoint directly
// (jev.go), exactly as the TS version does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// classifyTypesafeSystemOne is the ProviderClassifyFunc registered under
// `classifiers["typesafe-system-one"]`.
func classifyTypesafeSystemOne(model map[string]any, request sdk.ClassifierContext, options sdk.ProviderOperationOptions) (sdk.ClassifierResult, error) {
	return classifyWith(DefaultJevAPI(), model, request, options)
}

// classifyWith runs one classification against the given endpoint.
func classifyWith(api JevAPI, model map[string]any, request sdk.ClassifierContext, options sdk.ProviderOperationOptions) (sdk.ClassifierResult, error) {
	providerID, _ := model["provider"].(string)
	modelID, _ := model["id"].(string)
	baseURL := strings.TrimRight(stringOf(model["baseUrl"]), "/")
	if baseURL == "" {
		baseURL = api.Config.BaseURL
	}

	questions := request.Questions
	if questions == nil {
		questions = sdk.NewOrderedObject()
	}
	payload, err := json.Marshal(struct {
		Model     string `json:"model"`
		State     any    `json:"state"`
		Questions any    `json:"questions"`
	}{Model: modelID, State: request.State, Questions: questions})
	if err != nil {
		return sdk.ClassifierResult{}, err
	}

	key, err := resolveKeyOrError(baseURL, api.Lookup)
	if err != nil {
		return sdk.ClassifierResult{}, err
	}

	ctx := options.Signal
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, api.Config.Timeout)
	defer cancel()
	endpoint := baseURL + "/systemone"
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return sdk.ClassifierResult{}, err
	}
	httpRequest.Header.Set("authorization", "Bearer "+key)
	httpRequest.Header.Set("content-type", "application/json")
	client := api.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return sdk.ClassifierResult{}, transportError(err, api.Config)
	}
	defer response.Body.Close()
	raw, err := readBoundedBody(response.Body)
	if err != nil {
		if errors.Is(err, errResponseBodyTooLarge) {
			return sdk.ClassifierResult{}, err
		}
		return sdk.ClassifierResult{}, transportError(err, api.Config)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return sdk.ClassifierResult{}, fmt.Errorf("HTTP %d from %s: %s", response.StatusCode, endpoint, oneLine(string(raw), 300))
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return sdk.ClassifierResult{}, fmt.Errorf("non-JSON response from %s: %s", endpoint, oneLine(string(raw), 200))
	}
	envelope, ok := isRecord(parsed)
	if !ok {
		return sdk.ClassifierResult{}, fmt.Errorf("non-object JSON response from %s: %s", endpoint, oneLine(string(raw), 200))
	}
	body := unwrapSystemOneBody(envelope)
	answers, ok := isRecord(body["answers"])
	if !ok || len(answers) == 0 {
		return sdk.ClassifierResult{}, fmt.Errorf("response carried no answers: %s", oneLine(string(raw), 200))
	}
	// Validate the answers before they are handed back: a response whose keys are
	// all present but whose answers are structurally broken must not surface as a
	// successful ("stop") classification.
	if err := validateClassifierAnswers(questions, answers); err != nil {
		return sdk.ClassifierResult{}, err
	}

	pairs := make([]any, 0, len(answers)*2)
	for _, id := range questions.Keys() {
		answer, exists := answers[id]
		if !exists || answer == nil {
			return sdk.ClassifierResult{}, fmt.Errorf("incomplete answers: missing key %q", id)
		}
		pairs = append(pairs, id, answer)
	}

	stopReason := "stop"
	if requestCtx.Err() != nil {
		if ctx.Err() != nil {
			stopReason = "aborted"
		} else {
			stopReason = "error"
		}
	}
	result := sdk.ClassifierResult{
		API:        classifierAPI,
		Provider:   providerID,
		Model:      modelID,
		Answers:    sdk.NewOrderedObject(pairs...),
		Usage:      classifierUsage(body, envelope, model),
		StopReason: stopReason,
		Timestamp:  time.Now().UnixMilli(),
	}
	if stopReason != "stop" {
		result.ErrorMessage = "classification did not complete"
	}
	return result, nil
}

// validateClassifierAnswers applies the same fail-closed answer validation as
// the direct JEV call to the questions of a host classification request. The
// questions arrive as the wire objects the TS extension sends
// (`{type, instructions, criteria}`), so the type and the choice/score labels
// come from the request itself; a question of unknown type, or an answer that
// does not match its question's shape, is an error.
func validateClassifierAnswers(questions *sdk.OrderedObject, answers map[string]any) error {
	for _, id := range questions.Keys() {
		answer, exists := answers[id]
		if !exists || answer == nil {
			return fmt.Errorf("incomplete answers: missing key %q", id)
		}
		question, _ := questions.Get(id)
		rawType, _ := questionMember(question, "type")
		questionType, _ := rawType.(string)
		var criteria []string
		if rawCriteria, hasCriteria := questionMember(question, "criteria"); hasCriteria && rawCriteria != nil {
			labels, ok := criteriaLabels(rawCriteria)
			if !ok {
				return fmt.Errorf("malformed criteria for %q", id)
			}
			criteria = labels
		}
		if err := validateAnswer(questionType, id, answer, criteria); err != nil {
			return err
		}
	}
	return nil
}

// questionMember reads one member of a classifier question. Over the host
// bridge a question is a decoded JSON object (map[string]any); a Go caller can
// also pass the SDK's ordered object.
func questionMember(question any, key string) (any, bool) {
	switch typed := question.(type) {
	case map[string]any:
		value, ok := typed[key]
		return value, ok
	case *sdk.OrderedObject:
		return typed.Get(key)
	default:
		return nil, false
	}
}

// criteriaLabels lists the labels of a choice/score question's criteria object,
// in request order.
func criteriaLabels(criteria any) ([]string, bool) {
	switch typed := criteria.(type) {
	case map[string]any:
		labels := make([]string, 0, len(typed))
		for label := range typed {
			labels = append(labels, label)
		}
		return labels, true
	case *sdk.OrderedObject:
		return typed.Keys(), true
	default:
		return nil, false
	}
}

// classifierUsage normalises `{ input_tokens, output_tokens }` (with the usual
// aliases) and prices it from the model's declared cost, matching
// ai.ParseClassifierUsage.
func classifierUsage(body, envelope map[string]any, model map[string]any) map[string]any {
	values, ok := isRecord(body["usage"])
	if !ok {
		values, ok = isRecord(envelope["usage"])
	}
	if !ok {
		return nil
	}
	input := coalesceNumber(values, "input_tokens", "prompt_tokens", "input")
	output := coalesceNumber(values, "output_tokens", "completion_tokens", "output")
	cost := map[string]any{"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0, "total": 0.0}
	if declared, ok := isRecord(model["cost"]); ok {
		inputPrice, _ := numberValue(declared["input"])
		outputPrice, _ := numberValue(declared["output"])
		priceIn := input * inputPrice / 1e6
		priceOut := output * outputPrice / 1e6
		cost["input"] = priceIn
		cost["output"] = priceOut
		cost["total"] = priceIn + priceOut
	}
	return map[string]any{
		"input":       input,
		"output":      output,
		"cacheRead":   0.0,
		"cacheWrite":  0.0,
		"totalTokens": input + output,
		"cost":        cost,
	}
}

// stringOf reads a JSON string member, "" otherwise.
func stringOf(value any) string {
	text, _ := value.(string)
	return text
}
