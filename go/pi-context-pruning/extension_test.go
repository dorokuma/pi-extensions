package picontextpruning

import (
	"reflect"
	"strings"
	"testing"
)

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in       int
		expected string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1.0K"},
		{1050, "1.1K"},
		{1250, "1.3K"},
		{1350, "1.4K"},
		{1500, "1.5K"},
		{9950, "9.9K"},
		{20000, "20.0K"},
		{999950, "1000.0K"},
		{1000000, "1.0M"},
		{1050000, "1.1M"},
		{1250000, "1.3M"},
		{2500000, "2.5M"},
	}

	for _, tc := range cases {
		actual := FormatTokens(tc.in)
		if actual != tc.expected {
			t.Errorf("FormatTokens(%d) expected %q, got %q", tc.in, tc.expected, actual)
		}
	}
}

func TestFormatStatsMessage(t *testing.T) {
	// Zero passes
	msg0 := FormatStatsMessage(0, EmptyStats(), nil)
	if msg0 != "No pruning has occurred yet this session." {
		t.Fatalf("expected initial no-prune message, got %q", msg0)
	}

	// Active passes without last pass
	stats1 := PruneStats{
		MessagesPruned:    3,
		TokensPruned:      15000,
		TotalToolTokens:   30000,
		MessagesProtected: 2,
	}
	msg1 := FormatStatsMessage(1, stats1, nil)
	if !strings.Contains(msg1, "Prune passes: 1") || !strings.Contains(msg1, "Tokens pruned: ~15.0K") {
		t.Fatalf("unexpected stats message format: %q", msg1)
	}

	// Active passes with last pass
	lastPass := PruneStats{
		MessagesPruned: 1,
		TokensPruned:   5000,
	}
	msg2 := FormatStatsMessage(2, stats1, &lastPass)
	if !strings.Contains(msg2, "Last pass: 1 messages / ~5.0K tokens") {
		t.Fatalf("expected last pass summary in message, got %q", msg2)
	}
}

func TestFormatConfigMessage(t *testing.T) {
	defaultMsg := FormatConfigMessage(DEFAULT_CONFIG)
	if !strings.Contains(defaultMsg, "Protected tools: (none)") ||
		!strings.Contains(defaultMsg, "Prunable tools: (all non-protected)") {
		t.Fatalf("unexpected default config message: %q", defaultMsg)
	}

	customCfg := PruneConfig{
		Enabled:        false,
		PruneMinimum:   10000,
		PruneProtect:   20000,
		ProtectedTurns: 4,
		ProtectedTools: []string{"bash", "write"},
		PrunableTools:  []string{"read"},
	}
	customMsg := FormatConfigMessage(customCfg)
	if !strings.Contains(customMsg, "Protected tools: bash, write") ||
		!strings.Contains(customMsg, "Prunable tools: read") ||
		!strings.Contains(customMsg, "Enabled: false") {
		t.Fatalf("unexpected custom config message: %q", customMsg)
	}
}

func TestCommandHandlers(t *testing.T) {
	state := NewSessionState(DEFAULT_CONFIG)

	var lastStatusKey, lastStatusText string
	setStatus := func(key, text string) {
		lastStatusKey = key
		lastStatusText = text
	}

	var lastNotifyMsg, lastNotifyLevel string
	notify := func(msg, level string) {
		lastNotifyMsg = msg
		lastNotifyLevel = level
	}

	// 1. /prune
	err := HandleCommandPrune(state, setStatus, notify)
	if err != nil {
		t.Fatalf("HandleCommandPrune returned error: %v", err)
	}
	if !state.ForceNextPrune {
		t.Fatalf("expected ForceNextPrune == true")
	}
	if lastStatusKey != "context-pruning" || lastStatusText != "🔪 forced prune pending..." {
		t.Fatalf("unexpected status after /prune: %s=%s", lastStatusKey, lastStatusText)
	}
	if lastNotifyLevel != "info" || lastNotifyMsg != "Forced pruning queued for the next model call." {
		t.Fatalf("unexpected notify after /prune: %s (level: %s)", lastNotifyMsg, lastNotifyLevel)
	}

	// 2. /prune-toggle
	err = HandleCommandPruneToggle(state, setStatus, notify)
	if err != nil {
		t.Fatalf("HandleCommandPruneToggle returned error: %v", err)
	}
	if state.Config.Enabled != false {
		t.Fatalf("expected Enabled to be toggled to false")
	}
	if lastStatusText != "🔪 context pruning disabled" {
		t.Fatalf("unexpected status text after toggle: %q", lastStatusText)
	}
	if lastNotifyMsg != "Context pruning disabled" {
		t.Fatalf("unexpected notify message after toggle: %q", lastNotifyMsg)
	}

	// 3. /prune-stats (no passes yet)
	err = HandleCommandPruneStats(state, notify)
	if err != nil {
		t.Fatalf("HandleCommandPruneStats returned error: %v", err)
	}
	if lastNotifyMsg != "No pruning has occurred yet this session." {
		t.Fatalf("unexpected notify for /prune-stats: %q", lastNotifyMsg)
	}

	// 4. /prune-config
	err = HandleCommandPruneConfig(state, notify)
	if err != nil {
		t.Fatalf("HandleCommandPruneConfig returned error: %v", err)
	}
	if !strings.Contains(lastNotifyMsg, "Context Pruning Config") {
		t.Fatalf("unexpected notify for /prune-config: %q", lastNotifyMsg)
	}
}

func TestHandleSessionStart(t *testing.T) {
	state := NewSessionState(DEFAULT_CONFIG)
	state.PrunePasses = 5
	state.ForceNextPrune = true

	var lastStatusText string
	setStatus := func(key, text string) {
		lastStatusText = text
	}

	settings := map[string]any{
		"contextPruning": map[string]any{
			"enabled": false,
		},
	}

	_, err := HandleSessionStart(state, setStatus, "", settings)
	if err != nil {
		t.Fatalf("HandleSessionStart error: %v", err)
	}
	if state.PrunePasses != 0 {
		t.Fatalf("expected PrunePasses to be reset to 0, got %d", state.PrunePasses)
	}
	if state.ForceNextPrune != false {
		t.Fatalf("expected ForceNextPrune to be reset to false")
	}
	if state.Config.Enabled != false {
		t.Fatalf("expected Config.Enabled == false from merged settings")
	}
	if lastStatusText != "🔪 context pruning disabled" {
		t.Fatalf("expected disabled status set, got %q", lastStatusText)
	}
}

// TestContextRewritingComparison tests the exact before and after transformation of messages
// when a context event occurs, verifying the prompt rewrite and metrics.
func TestContextRewritingComparison(t *testing.T) {
	cfg := DEFAULT_CONFIG
	cfg.PruneProtect = 0
	cfg.PruneMinimum = 100

	state := NewSessionState(cfg)

	var recordedStatusKey, recordedStatusText string
	setStatus := func(key, text string) {
		recordedStatusKey = key
		recordedStatusText = text
	}

	// Build message history:
	// Turn 0: user("hello")
	// Turn 1: toolResult("read", 2000 chars text) -> eligible for pruning
	// Turn 2: user("second prompt")
	// Turn 3: toolResult("bash", 2000 chars text) -> protected because within last 2 user turns
	// Turn 4: user("latest prompt")
	longContent := strings.Repeat("A", 2000)

	rawMessages := []any{
		map[string]any{
			"role":    "user",
			"content": "hello",
		},
		map[string]any{
			"role":       "toolResult",
			"toolName":   "read",
			"toolCallId": "call_1",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": longContent,
				},
			},
			"isError": false,
		},
		map[string]any{
			"role":    "user",
			"content": "second prompt",
		},
		map[string]any{
			"role":       "toolResult",
			"toolName":   "bash",
			"toolCallId": "call_2",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": longContent,
				},
			},
			"isError": false,
		},
		map[string]any{
			"role":    "user",
			"content": "latest prompt",
		},
	}

	eventData := map[string]any{
		"messages": rawMessages,
	}

	// BEFORE REWRITE:
	// Message at index 1 has content text == longContent
	msg1Before := rawMessages[1].(map[string]any)
	content1Before := msg1Before["content"].([]any)[0].(map[string]any)["text"].(string)
	if content1Before != longContent {
		t.Fatalf("precondition failed: original content mismatch")
	}

	// EXECUTE CONTEXT HOOK
	res, err := HandleContextEvent(state, setStatus, eventData)
	if err != nil {
		t.Fatalf("HandleContextEvent error: %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil response modifying context")
	}

	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any result")
	}
	rewrittenMessages, ok := resMap["messages"].([]any)
	if !ok || len(rewrittenMessages) != len(rawMessages) {
		t.Fatalf("expected rewritten messages slice of length %d", len(rawMessages))
	}

	// AFTER REWRITE:
	// 1. Index 1 (tool "read") must be rewritten with pruning marker
	msg1After := rewrittenMessages[1].(map[string]any)
	blocks1After := msg1After["content"].([]any)
	if len(blocks1After) != 1 {
		t.Fatalf("expected 1 content block after rewrite")
	}
	text1After := blocks1After[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text1After, "[output pruned — ~") || !strings.Contains(text1After, "| read]") {
		t.Fatalf("expected prune marker on message 1, got %q", text1After)
	}

	// 2. Index 3 (tool "bash") must remain unchanged (protected turns)
	msg3After := rewrittenMessages[3].(map[string]any)
	text3After := msg3After["content"].([]any)[0].(map[string]any)["text"].(string)
	if text3After != longContent {
		t.Fatalf("expected message 3 to remain unchanged, got %q", text3After)
	}

	// 3. Status bar updated
	if recordedStatusKey != "context-pruning" || !strings.Contains(recordedStatusText, "pruned") {
		t.Fatalf("expected status bar update, got %s=%s", recordedStatusKey, recordedStatusText)
	}

	// 4. Session stats updated
	if state.PrunePasses != 1 {
		t.Fatalf("expected PrunePasses == 1, got %d", state.PrunePasses)
	}
	if state.SessionStats.MessagesPruned != 1 {
		t.Fatalf("expected 1 message pruned in session stats, got %d", state.SessionStats.MessagesPruned)
	}
	if state.SessionStats.TokensPruned <= 0 {
		t.Fatalf("expected TokensPruned > 0")
	}

	// 5. Original messages slice was not corrupted in-place
	if !reflect.DeepEqual(msg1Before["content"].([]any)[0].(map[string]any)["text"], longContent) {
		t.Fatalf("immutability check failed: original slice was mutated in-place")
	}
}

// TestContextDisabledWithForcePrune tests:
// 1. empty message list consumes ForceNextPrune;
// 2. enabled=false + ForceNextPrune still prunes with effective pruneMinimum=0;
// 3. force is consumed and subsequent calls do not prune.
func TestContextDisabledWithForcePrune(t *testing.T) {
	cfg := DEFAULT_CONFIG
	cfg.Enabled = false
	cfg.PruneMinimum = 20000 // default minimum is 20,000 tokens
	cfg.PruneProtect = 0

	state := NewSessionState(cfg)

	// Sub-test 1: Empty message list consumes ForceNextPrune
	// 与 TS index.ts:30-32 语义一致：空消息列表也消费 forceNextPrune
	state.ForceNextPrune = true
	emptyEvent := map[string]any{
		"messages": []any{},
	}
	res, err := HandleContextEvent(state, nil, emptyEvent)
	if err != nil {
		t.Fatalf("HandleContextEvent error on empty messages: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on empty messages")
	}
	if state.ForceNextPrune != false {
		t.Fatalf("expected ForceNextPrune to be consumed by empty messages")
	}

	// Sub-test 2: Enabled=false with ForceNextPrune prunes even when tokens < PruneMinimum (minimum=0 behavior)
	state.ForceNextPrune = true
	// Create messages with small output (e.g. 500 chars ~ 125 tokens, which is < 20000 minimum)
	smallContent := strings.Repeat("x", 500)
	rawMessages := []any{
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{
			"role":       "toolResult",
			"toolName":   "read",
			"toolCallId": "call_1",
			"content": []any{
				map[string]any{"type": "text", "text": smallContent},
			},
			"isError": false,
		},
		map[string]any{"role": "user", "content": "u2"},
		map[string]any{"role": "user", "content": "u3"},
	}
	eventData := map[string]any{"messages": rawMessages}

	res, err = HandleContextEvent(state, nil, eventData)
	if err != nil {
		t.Fatalf("HandleContextEvent error: %v", err)
	}
	if res == nil {
		t.Fatalf("expected pruning to happen when ForceNextPrune is set even if Enabled=false")
	}
	resMap := res.(map[string]any)
	msgs := resMap["messages"].([]any)
	toolMsg := msgs[1].(map[string]any)
	text := toolMsg["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "[output pruned") {
		t.Fatalf("expected tool message to be pruned, got %q", text)
	}

	// Sub-test 3: Force was consumed; second call does NOT force pruning
	if state.ForceNextPrune != false {
		t.Fatalf("expected ForceNextPrune to be false after execution")
	}
	res2, err := HandleContextEvent(state, nil, eventData)
	if err != nil {
		t.Fatalf("HandleContextEvent second call error: %v", err)
	}
	if res2 != nil {
		t.Fatalf("expected second call with Enabled=false and ForceNextPrune=false to return nil, got %v", res2)
	}
}
