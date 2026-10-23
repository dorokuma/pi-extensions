package geminisafety_test

import (
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	geminisafety "github.com/dorokuma/pi-extensions/go/gemini-safety"
)

// deepCopyJSON produces a deep clone via JSON serialization for immutable snapshot comparison.
func deepCopyJSON(t *testing.T, src any) any {
	t.Helper()
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	var dst any
	if err := json.Unmarshal(b, &dst); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	return dst
}

func TestRewritePayload_NonObjectOrNil(t *testing.T) {
	testCases := []struct {
		name  string
		input any
	}{
		{name: "nil payload", input: nil},
		{name: "integer payload", input: 42},
		{name: "string payload", input: "not-an-object"},
		{name: "slice payload", input: []any{"a", "b"}},
		{name: "boolean payload", input: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, modified := geminisafety.RewritePayload(tc.input)
			if modified {
				t.Fatalf("expected modified=false for %s, got true with res=%v", tc.name, res)
			}
			if res != nil {
				t.Fatalf("expected res=nil, got %v", res)
			}

			// Also verify HandleBeforeProviderRequest
			handlerRes, err := geminisafety.HandleBeforeProviderRequest(sdk.Context{}, map[string]any{"payload": tc.input})
			if err != nil {
				t.Fatalf("unexpected error from handler: %v", err)
			}
			if handlerRes != nil {
				t.Fatalf("expected handlerRes=nil, got %v", handlerRes)
			}
		})
	}

	t.Run("nil or empty event data", func(t *testing.T) {
		res, err := geminisafety.HandleBeforeProviderRequest(sdk.Context{}, nil)
		if err != nil || res != nil {
			t.Fatalf("expected (nil, nil) for nil data, got (%v, %v)", res, err)
		}

		res, err = geminisafety.HandleBeforeProviderRequest(sdk.Context{}, map[string]any{})
		if err != nil || res != nil {
			t.Fatalf("expected (nil, nil) for empty data, got (%v, %v)", res, err)
		}
	})
}

func TestRewritePayload_MissingOrInvalidContents(t *testing.T) {
	testCases := []struct {
		name    string
		payload map[string]any
	}{
		{
			name: "missing contents",
			payload: map[string]any{
				"model": "gemini-2.0-flash",
			},
		},
		{
			name: "contents is nil",
			payload: map[string]any{
				"model":    "gemini-2.0-flash",
				"contents": nil,
			},
		},
		{
			name: "contents is string",
			payload: map[string]any{
				"model":    "gemini-2.0-flash",
				"contents": "not-an-array",
			},
		},
		{
			name: "contents is map",
			payload: map[string]any{
				"model":    "gemini-2.0-flash",
				"contents": map[string]any{"role": "user"},
			},
		},
		{
			name: "OpenAI-shaped payload with messages instead of contents",
			payload: map[string]any{
				"model": "gemini-2.0-flash",
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, modified := geminisafety.RewritePayload(tc.payload)
			if modified {
				t.Fatalf("expected modified=false, got true with res=%v", res)
			}
			if res != nil {
				t.Fatalf("expected res=nil, got %v", res)
			}
		})
	}
}

func TestRewritePayload_ModelNotGeminiOrNonString(t *testing.T) {
	testCases := []struct {
		name    string
		payload map[string]any
	}{
		{
			name: "missing model key",
			payload: map[string]any{
				"contents": []any{map[string]any{"text": "hello"}},
			},
		},
		{
			name: "model is integer",
			payload: map[string]any{
				"model":    1234,
				"contents": []any{map[string]any{"text": "hello"}},
			},
		},
		{
			name: "model is claude",
			payload: map[string]any{
				"model":    "claude-3-5-sonnet",
				"contents": []any{map[string]any{"text": "hello"}},
			},
		},
		{
			name: "model is gpt-4o",
			payload: map[string]any{
				"model":    "gpt-4o",
				"contents": []any{map[string]any{"text": "hello"}},
			},
		},
		{
			name: "model is empty string",
			payload: map[string]any{
				"model":    "",
				"contents": []any{map[string]any{"text": "hello"}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, modified := geminisafety.RewritePayload(tc.payload)
			if modified {
				t.Fatalf("expected modified=false, got true with res=%v", res)
			}
			if res != nil {
				t.Fatalf("expected res=nil, got %v", res)
			}
		})
	}
}

func TestRewritePayload_GeminiNativeSuccess(t *testing.T) {
	inputPayload := map[string]any{
		"model": "models/gemini-2.0-flash",
		"contents": []any{
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"text": "Respond OK"},
				},
			},
		},
		"config": map[string]any{
			"temperature": 0.5,
			"topK":        40.0,
		},
		"extraField": "keep-me",
	}

	res, modified := geminisafety.RewritePayload(inputPayload)
	if !modified {
		t.Fatalf("expected modified=true, got false")
	}
	if res == nil {
		t.Fatalf("expected non-nil result")
	}

	// Verify top-level preservation
	if res["model"] != "models/gemini-2.0-flash" {
		t.Errorf("model mismatch: got %v", res["model"])
	}
	if res["extraField"] != "keep-me" {
		t.Errorf("extraField not preserved: got %v", res["extraField"])
	}

	// Verify config keys preservation
	cfg, ok := res["config"].(map[string]any)
	if !ok {
		t.Fatalf("res[config] is not a map[string]any: %T", res["config"])
	}
	if cfg["temperature"] != 0.5 {
		t.Errorf("config.temperature mismatch: got %v", cfg["temperature"])
	}
	if cfg["topK"] != 40.0 {
		t.Errorf("config.topK mismatch: got %v", cfg["topK"])
	}

	// Verify safetySettings
	safetySettings, ok := cfg["safetySettings"].([]any)
	if !ok {
		t.Fatalf("config.safetySettings is not []any: %T", cfg["safetySettings"])
	}
	if len(safetySettings) != 4 {
		t.Fatalf("expected exactly 4 safetySettings, got %d", len(safetySettings))
	}

	expectedCategories := []string{
		"HARM_CATEGORY_HARASSMENT",
		"HARM_CATEGORY_HATE_SPEECH",
		"HARM_CATEGORY_SEXUALLY_EXPLICIT",
		"HARM_CATEGORY_DANGEROUS_CONTENT",
	}

	for i, expCat := range expectedCategories {
		setting, ok := safetySettings[i].(map[string]any)
		if !ok {
			t.Fatalf("safetySettings[%d] is not map[string]any: %T", i, safetySettings[i])
		}
		if setting["category"] != expCat {
			t.Errorf("safetySettings[%d].category = %v, want %v", i, setting["category"], expCat)
		}
		if setting["threshold"] != "BLOCK_NONE" {
			t.Errorf("safetySettings[%d].threshold = %v, want BLOCK_NONE", i, setting["threshold"])
		}
	}

	// Also verify HandleBeforeProviderRequest
	eventData := map[string]any{
		"type":    "before_provider_request",
		"payload": inputPayload,
	}
	handlerRes, err := geminisafety.HandleBeforeProviderRequest(sdk.Context{}, eventData)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	handlerMap, ok := handlerRes.(map[string]any)
	if !ok {
		t.Fatalf("handler result is not map[string]any: %T", handlerRes)
	}
	handlerCfg := handlerMap["config"].(map[string]any)
	if len(handlerCfg["safetySettings"].([]any)) != 4 {
		t.Errorf("handler safetySettings length != 4")
	}
}

func TestRewritePayload_NoExistingConfig(t *testing.T) {
	inputPayload := map[string]any{
		"model":    "gemini-1.5-pro",
		"contents": []any{"prompt"},
	}

	res, modified := geminisafety.RewritePayload(inputPayload)
	if !modified {
		t.Fatalf("expected modified=true")
	}
	cfg, ok := res["config"].(map[string]any)
	if !ok || cfg == nil {
		t.Fatalf("expected newly created config map")
	}
	settings, ok := cfg["safetySettings"].([]any)
	if !ok || len(settings) != 4 {
		t.Fatalf("expected 4 safety settings, got %v", cfg["safetySettings"])
	}
}

func TestRewritePayload_PreservesImmutability(t *testing.T) {
	origInput := map[string]any{
		"model": "gemini-2.5-flash",
		"contents": []any{
			map[string]any{"parts": []any{"test"}},
		},
		"config": map[string]any{
			"origKey": "origVal",
		},
	}

	snapshotBefore := deepCopyJSON(t, origInput)

	res, modified := geminisafety.RewritePayload(origInput)
	if !modified {
		t.Fatalf("expected modified=true")
	}

	snapshotAfter := deepCopyJSON(t, origInput)

	// Verify input was not mutated during execution
	if !reflect.DeepEqual(snapshotBefore, snapshotAfter) {
		t.Fatalf("input payload was mutated in place! Before=%v After=%v", snapshotBefore, snapshotAfter)
	}

	// Mutating output must not affect original input
	res["model"] = "mutated-model"
	resConfig := res["config"].(map[string]any)
	resConfig["origKey"] = "mutated-val"
	resSettings := resConfig["safetySettings"].([]any)
	resSettings[0].(map[string]any)["threshold"] = "MUTATED"

	origConfig := origInput["config"].(map[string]any)
	if origInput["model"] != "gemini-2.5-flash" {
		t.Errorf("original input model was modified through output")
	}
	if origConfig["origKey"] != "origVal" {
		t.Errorf("original input config.origKey was modified through output")
	}
	if _, has := origConfig["safetySettings"]; has {
		t.Errorf("original input config should not have safetySettings added in-place")
	}
}

func TestRewritePayload_OverwriteExistingSafetySettings(t *testing.T) {
	inputPayload := map[string]any{
		"model": "gemini-2.0-flash",
		"contents": []any{
			map[string]any{"parts": []any{"test"}},
		},
		"config": map[string]any{
			"existingOption": "someValue",
			"safetySettings": []any{
				map[string]any{
					"category":  "HARM_CATEGORY_HARASSMENT",
					"threshold": "BLOCK_LOW_AND_ABOVE",
				},
			},
		},
	}

	res, modified := geminisafety.RewritePayload(inputPayload)
	if !modified {
		t.Fatalf("expected modified=true")
	}

	cfg := res["config"].(map[string]any)
	if cfg["existingOption"] != "someValue" {
		t.Errorf("existing config option not preserved: %v", cfg["existingOption"])
	}

	settings := cfg["safetySettings"].([]any)
	if len(settings) != 4 {
		t.Fatalf("expected exactly 4 safetySettings after overwrite, got %d", len(settings))
	}

	for _, s := range settings {
		m := s.(map[string]any)
		if m["threshold"] != "BLOCK_NONE" {
			t.Errorf("expected threshold BLOCK_NONE, got %v", m["threshold"])
		}
	}
}

func TestExtensionFactory(t *testing.T) {
	ext := geminisafety.Extension()
	if ext == nil {
		t.Fatalf("expected non-nil Extension()")
	}
}
