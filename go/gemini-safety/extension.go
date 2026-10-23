// Package geminisafety implements the gemini-safety extension for PiG in Go.
//
// Background & Motivation:
// In upstream Pi, gemini-safety pushes `safetySettings` (4 BLOCK_NONE categories)
// through to Google's *native* generateContent API (where the field is accepted).
// Google's OpenAI-compatible endpoint rejects safetySettings under any key name
// with HTTP 400, while the native `v1beta/...:streamGenerateContent` endpoint
// does accept `config.safetySettings`.
//
// Alignment & Differences with TypeScript Implementation:
// 1. Hook Registration:
//    - TS: pi.on("before_provider_request", (event) => ...)
//    - Go: e.OnEvent(sdk.EventBeforeProviderRequest, func(ctx sdk.Context, data map[string]any) (any, error) ...)
// 2. Event Data Shape:
//    - TS: receives `event` object with `event.payload` representing the outgoing request body.
//    - Go: receives `data map[string]any` over PiG's wire protocol, where `data["payload"]` holds the request payload map.
// 3. Return Value & Host Adoption:
//    - TS: returns a new object `{...payload, config: {...config, safetySettings}}` or `undefined` for no-op.
//    - Go: returns `(newPayload, nil)` when rewritten, or `(nil, nil)` for no-op (equivalent to TS `undefined`).
//    - PiG Host Behavior: Upon receiving a non-nil return value from the before_provider_request hook,
//      the host atomically replaces the outbound request payload with the returned map.
// 4. Immutability:
//    - Neither the TS nor Go implementation mutates the input payload map in-place; both return a new shallow-copied
//      payload with a new config map.
package geminisafety

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// SafetySettings defines Google's four content-filter categories, each with filtering switched off (BLOCK_NONE).
// Matches the TS implementation's SAFETY_SETTINGS constant.
var SafetySettings = []map[string]any{
	{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
	{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
	{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "BLOCK_NONE"},
	{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_NONE"},
}

// RewritePayload inspects a raw request payload and, if it matches a native Gemini payload,
// returns a new payload map with safetySettings injected under `config`.
// It returns (nil, false) if the payload does not qualify (non-object, contents is not an array,
// or model does not contain "gemini"), meaning no modification should occur.
// The original payload and config maps are never mutated in place.
func RewritePayload(raw any) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}
	payload, ok := raw.(map[string]any)
	if !ok || payload == nil {
		return nil, false
	}

	// Only the native Gemini payload has a `contents` array; OpenAI-shaped bodies
	// use `messages`, so this also excludes gemini models served over openai-completions.
	contents, ok := payload["contents"]
	if !ok || contents == nil {
		return nil, false
	}
	switch contents.(type) {
	case []any:
		// Expected slice type in deserialized JSON
	default:
		return nil, false
	}

	// Model must be a string and contain "gemini"
	model, ok := payload["model"].(string)
	if !ok || !strings.Contains(model, "gemini") {
		return nil, false
	}

	// Extract existing config or create a new empty map if not present/not an object
	var newConfig map[string]any
	if origConfig, ok := payload["config"].(map[string]any); ok && origConfig != nil {
		newConfig = make(map[string]any, len(origConfig)+1)
		for k, v := range origConfig {
			newConfig[k] = v
		}
	} else {
		newConfig = make(map[string]any, 1)
	}

	// Deep copy safety settings (array of 4 maps)
	settingsCopy := make([]any, len(SafetySettings))
	for i, s := range SafetySettings {
		m := make(map[string]any, len(s))
		for k, v := range s {
			m[k] = v
		}
		settingsCopy[i] = m
	}

	// Overwrite or set safetySettings in new config map (preserves all other keys in config)
	newConfig["safetySettings"] = settingsCopy

	// Shallow-copy top-level payload to preserve immutability of original map
	newPayload := make(map[string]any, len(payload))
	for k, v := range payload {
		newPayload[k] = v
	}
	newPayload["config"] = newConfig

	return newPayload, true
}

// HandleBeforeProviderRequest processes the before_provider_request event.
// It extracts the request payload from data["payload"], checks if rewriting is needed,
// and returns the modified payload or (nil, nil) for no-op.
func HandleBeforeProviderRequest(_ sdk.Context, data map[string]any) (any, error) {
	if data == nil {
		return nil, nil
	}
	rawPayload, ok := data["payload"]
	if !ok || rawPayload == nil {
		return nil, nil
	}

	newPayload, modified := RewritePayload(rawPayload)
	if !modified {
		return nil, nil
	}
	return newPayload, nil
}

// Extension constructs and returns the gemini-safety extension instance for PiG.
func Extension() *sdk.Extension {
	e := sdk.New("gemini-safety")

	e.OnEvent(sdk.EventBeforeProviderRequest, HandleBeforeProviderRequest)

	return e
}
