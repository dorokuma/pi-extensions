package autocontinue

import "testing"

func TestLooksTruncatedText(t *testing.T) {
	truncated := []string{
		"正文：",
		"正文，",
		"正文、",
		"正文；",
		"half width:",
		"half width,",
		"half width;",
		"a, ",           // trailing space is trimmed first
		"```go\ncode\n", // unclosed fence
		"正文\n# 标题",      // heading with no content
		"正文\n###",       // bare heading
		"正文\n-",         // empty list item
		"正文\n* ",        // empty list item, trailing space
		"正文\n+",         // empty list item
		"正文\n1.",        // empty ordered item
		"正文\n2)",        // empty ordered item, paren
		"省略……",          // ellipsis
		"wait...",       // three dots
		"   正文\n   1.",  // up to three spaces of indent still counts as a list item
	}
	for _, text := range truncated {
		if !LooksTruncatedText(text) {
			t.Errorf("LooksTruncatedText(%q) = false, want true", text)
		}
	}

	complete := []string{
		"",
		"   \n\t ",
		"正文。",
		"question?",
		"answer!",
		"（括注）",
		"```go\ncode\n```",
		"```a``` then 正文",
		"| a | b |\n| - | - |\n| 1 | 2 |",
		"   正文\n    1.", // four spaces of indent is not a list item under {0,3}
	}
	for _, text := range complete {
		if LooksTruncatedText(text) {
			t.Errorf("LooksTruncatedText(%q) = true, want false", text)
		}
	}
}

// stopFacts builds the injected facts; a zero means "no evidence", exactly like
// TS's undefined (positiveTs filters non-positive values).
func stopFacts(now, toolUse, userMessage, runStart float64) PrematureStopFacts {
	return PrematureStopFacts{
		LastToolUseTs:     &toolUse,
		LastUserMessageTs: &userMessage,
		RunStartTs:        &runStart,
		Now:               &now,
	}
}

func assistantStop(content any) map[string]any {
	return map[string]any{"role": "assistant", "stopReason": "stop", "content": content}
}

func thinkingOnly() map[string]any {
	return assistantStop([]any{map[string]any{"type": "thinking", "thinking": "hmm"}})
}

func textBlocksOf(text string) []any {
	return []any{map[string]any{"type": "text", "text": text}}
}

func TestIsPrematureStopShape(t *testing.T) {
	// now, toolUse, userMessage, runStart: gates pass with a recent tool use and
	// a stale user message.
	const now = 10_000_000.0
	gatesPass := stopFacts(now, now-1_000, now-200_000, 0)

	cases := []struct {
		name       string
		message    map[string]any
		facts      PrematureStopFacts
		wantOK     bool
		wantReason string
	}{
		{name: "nil message", message: nil, facts: gatesPass, wantReason: "no-assistant"},
		{name: "user role", message: map[string]any{"role": "user"}, facts: gatesPass, wantReason: "no-assistant"},
		{
			name:       "error stop is not this gate",
			message:    map[string]any{"role": "assistant", "stopReason": "error"},
			facts:      gatesPass,
			wantReason: "stopReason:error",
		},
		{
			name:       "missing stopReason",
			message:    map[string]any{"role": "assistant"},
			facts:      gatesPass,
			wantReason: "stopReason:none",
		},
		{
			name:       "aborted is out of scope",
			message:    map[string]any{"role": "assistant", "stopReason": "aborted"},
			facts:      gatesPass,
			wantReason: "stopReason:aborted",
		},
		{
			name: "tool call present",
			message: assistantStop([]any{
				map[string]any{"type": "text", "text": "正文："},
				map[string]any{"type": "toolCall", "name": "x"},
			}),
			facts:      gatesPass,
			wantReason: "stop:has-tool-call",
		},
		{
			name:       "thinking only",
			message:    thinkingOnly(),
			facts:      gatesPass,
			wantOK:     true,
			wantReason: "stop:premature-thinking-only",
		},
		{
			name:       "empty content",
			message:    assistantStop([]any{}),
			facts:      gatesPass,
			wantOK:     true,
			wantReason: "stop:premature-empty-content",
		},
		{
			name:       "map content is not a block array",
			message:    assistantStop(map[string]any{"type": "text", "text": "正文"}),
			facts:      gatesPass,
			wantOK:     true,
			wantReason: "stop:premature-empty-content",
		},
		{
			name:       "complete text",
			message:    assistantStop(textBlocksOf("任务完成。")),
			facts:      gatesPass,
			wantReason: "stop:complete-text",
		},
		{
			name:       "truncated text",
			message:    assistantStop(textBlocksOf("接下来需要：")),
			facts:      gatesPass,
			wantOK:     true,
			wantReason: "stop:premature-truncated-text",
		},
		{
			name:       "string content is text",
			message:    assistantStop("接下来需要："),
			facts:      gatesPass,
			wantOK:     true,
			wantReason: "stop:premature-truncated-text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPrematureStop(tc.message, tc.facts)
			if got.OK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", got.OK, tc.wantOK, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestIsPrematureStopGateA(t *testing.T) {
	const now = 10_000_000.0
	const staleUser = now - 200_000 // gate B satisfied for every row

	cases := []struct {
		name       string
		facts      PrematureStopFacts
		wantOK     bool
		wantReason string
	}{
		{
			name:       "no activity evidence at all",
			facts:      stopFacts(now, 0, staleUser, 0),
			wantReason: "premature:no-activity-evidence",
		},
		{
			name:   "tool use exactly at the window edge still counts",
			facts:  stopFacts(now, now-ToolUseWindowMs, staleUser, 0),
			wantOK: true, wantReason: "stop:premature-thinking-only",
		},
		{
			name:       "tool use just outside the window",
			facts:      stopFacts(now, now-ToolUseWindowMs-1, staleUser, 0),
			wantReason: "premature:no-activity-evidence",
		},
		{
			name:       "run exactly 90s is not long enough",
			facts:      stopFacts(now, 0, staleUser, now-RunActiveMinMs),
			wantReason: "premature:no-activity-evidence",
		},
		{
			name:   "run just over 90s counts",
			facts:  stopFacts(now, 0, staleUser, now-RunActiveMinMs-1),
			wantOK: true, wantReason: "stop:premature-thinking-only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPrematureStop(thinkingOnly(), tc.facts)
			if got.OK != tc.wantOK || got.Reason != tc.wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", got.OK, got.Reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

func TestIsPrematureStopGateB(t *testing.T) {
	const now = 10_000_000.0
	recentTool := now - 1_000 // gate A satisfied for every row

	cases := []struct {
		name       string
		facts      PrematureStopFacts
		wantOK     bool
		wantReason string
	}{
		{
			name:       "no user message at all",
			facts:      stopFacts(now, recentTool, 0, 0),
			wantReason: "premature:user-message-too-recent",
		},
		{
			name:       "user message exactly 90s ago is too recent",
			facts:      stopFacts(now, recentTool, now-UserProgressStallMs, 0),
			wantReason: "premature:user-message-too-recent",
		},
		{
			name:   "user message just over 90s ago",
			facts:  stopFacts(now, recentTool, now-UserProgressStallMs-1, 0),
			wantOK: true, wantReason: "stop:premature-thinking-only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPrematureStop(thinkingOnly(), tc.facts)
			if got.OK != tc.wantOK || got.Reason != tc.wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", got.OK, got.Reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

func TestIsPrematureStopMessageFallbacks(t *testing.T) {
	const now = 10_000_000.0

	t.Run("tool use comes from messages", func(t *testing.T) {
		facts := PrematureStopFacts{
			Messages: []map[string]any{
				{"role": "toolResult", "timestamp": now - 1_000},
				{"role": "assistant", "timestamp": now - 500, "content": textBlocksOf("x")},
			},
			LastUserMessageTs: floatPtr(now - 200_000),
			Now:               floatPtr(now),
		}
		got := IsPrematureStop(thinkingOnly(), facts)
		if !got.OK || got.Reason != "stop:premature-thinking-only" {
			t.Fatalf("got (%v, %q)", got.OK, got.Reason)
		}
	})

	t.Run("assistant toolCall block counts as tool use", func(t *testing.T) {
		facts := PrematureStopFacts{
			Messages: []map[string]any{
				{"role": "assistant", "timestamp": now - 2_000, "content": []any{map[string]any{"type": "toolCall"}}},
			},
			LastUserMessageTs: floatPtr(now - 200_000),
			Now:               floatPtr(now),
		}
		if got := IsPrematureStop(thinkingOnly(), facts); !got.OK {
			t.Fatalf("toolCall activity was not used, got %q", got.Reason)
		}
	})

	t.Run("user message comes from messages", func(t *testing.T) {
		facts := PrematureStopFacts{
			Messages: []map[string]any{
				{"role": "user", "timestamp": now - 300_000, "content": "帮我做点事"},
			},
			LastToolUseTs: floatPtr(now - 1_000),
			Now:           floatPtr(now),
		}
		if got := IsPrematureStop(thinkingOnly(), facts); !got.OK {
			t.Fatalf("user message fallback failed, got %q", got.Reason)
		}
	})

	t.Run("the extension's own continue is not a user message", func(t *testing.T) {
		facts := PrematureStopFacts{
			Messages: []map[string]any{
				{"role": "user", "timestamp": now - 300_000, "content": "帮我做点事"},
				{"role": "user", "timestamp": now - 1_000, "content": []any{map[string]any{"type": "text", "text": " 继续 "}}},
			},
			LastToolUseTs: floatPtr(now - 500),
			Now:           floatPtr(now),
		}
		if got := IsPrematureStop(thinkingOnly(), facts); !got.OK {
			t.Fatalf("own continue refreshed gate B, got %q", got.Reason)
		}
	})

	t.Run("explicit timestamps win over messages", func(t *testing.T) {
		facts := PrematureStopFacts{
			Messages: []map[string]any{
				{"role": "toolResult", "timestamp": now - 1_000},
			},
			LastToolUseTs:     floatPtr(now - ToolUseWindowMs - 1),
			LastUserMessageTs: floatPtr(now - 200_000),
			Now:               floatPtr(now),
		}
		if got := IsPrematureStop(thinkingOnly(), facts); got.OK {
			t.Fatal("messages overrode the tracked toolUse timestamp")
		}
	})

	t.Run("no injected clock falls back to the wall clock", func(t *testing.T) {
		// Just check it does not panic and produces a verdict; the wall clock
		// makes the numbers non-deterministic, which is why the other rows inject Now.
		_ = IsPrematureStop(thinkingOnly(), PrematureStopFacts{})
	})
}

func TestLastTimestampHelpers(t *testing.T) {
	messages := []map[string]any{
		{"role": "toolResult", "timestamp": 100.0},
		{"role": "toolResult", "timestamp": 300.0},
		{"role": "assistant", "timestamp": 200.0, "content": []any{map[string]any{"type": "toolCall"}}},
		{"role": "user", "timestamp": 400.0, "content": "hi"},
		{"role": "user", "timestamp": 900.0, "content": ContinuePrompt},
	}
	if got := lastToolUseTsFromMessages(messages); got == nil || *got != 300 {
		t.Fatalf("lastToolUseTsFromMessages = %v, want 300", got)
	}
	if got := lastUserTsFromMessages(messages); got == nil || *got != 400 {
		t.Fatalf("lastUserTsFromMessages = %v, want 400", got)
	}
	if got := lastToolUseTsFromMessages(nil); got != nil {
		t.Fatalf("lastToolUseTsFromMessages(nil) = %v, want nil", *got)
	}
}

func TestPositiveTs(t *testing.T) {
	for _, value := range []float64{0, -1} {
		if got := positiveTs(&value); got != nil {
			t.Fatalf("positiveTs(%v) = %v, want nil", value, *got)
		}
	}
	value := 5.0
	if got := positiveTs(&value); got == nil || *got != 5 {
		t.Fatalf("positiveTs(5) = %v, want 5", got)
	}
	if got := positiveTs(nil); got != nil {
		t.Fatalf("positiveTs(nil) = %v, want nil", got)
	}
}
