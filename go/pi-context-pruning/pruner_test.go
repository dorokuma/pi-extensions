package picontextpruning

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

var defaultText = strings.Repeat("x", 48000)

func makeUser(value string) map[string]any {
	return map[string]any{
		"role":      "user",
		"content":   value,
		"timestamp": int64(123456789),
	}
}

func makeTool(name string, value string, isError bool) map[string]any {
	return map[string]any{
		"role":       "toolResult",
		"toolName":   name,
		"toolCallId": "call_123",
		"content": []any{
			map[string]any{
				"type": "text",
				"text": value,
			},
		},
		"isError":   isError,
		"timestamp": int64(123456789),
	}
}

func makeConfig(fn func(c *PruneConfig)) PruneConfig {
	cfg := DEFAULT_CONFIG
	if fn != nil {
		fn(&cfg)
	}
	return cfg
}

func makeOldHistory(names []string) []map[string]any {
	msgs := []map[string]any{makeUser("old")}
	for _, name := range names {
		msgs = append(msgs, makeTool(name, defaultText, false))
	}
	msgs = append(msgs, makeUser("middle"), makeUser("latest"))
	return msgs
}

// TestDoesNotPruneBelowMinimum ports:
// "does not prune below minimum" from test/pruner.test.ts
func TestDoesNotPruneBelowMinimum(t *testing.T) {
	messages := makeOldHistory([]string{"read", "bash"})
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 100000
		c.PruneMinimum = 20000
	})
	_, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 0 {
		t.Fatalf("expected 0 messages pruned, got %d", stats.MessagesPruned)
	}
}

// TestPrunesBeyondProtectBudgetAndMinimum ports:
// "prunes beyond protect budget and minimum" from test/pruner.test.ts
func TestPrunesBeyondProtectBudgetAndMinimum(t *testing.T) {
	messages := makeOldHistory([]string{"read", "bash", "edit", "write"})
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 1
		c.PruneMinimum = 1
	})
	_, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned < 1 {
		t.Fatalf("expected at least 1 message pruned, got %d", stats.MessagesPruned)
	}
}

// TestCustomExtensionToolsArePrunableByDefault ports:
// "custom extension tools are prunable by default" from test/pruner.test.ts
func TestCustomExtensionToolsArePrunableByDefault(t *testing.T) {
	tools := []string{"codegraph_node", "ctx_execute", "tinyfish_fetch"}
	for _, name := range tools {
		if !IsPrunable(name, DEFAULT_CONFIG) {
			t.Fatalf("expected %s to be prunable by default", name)
		}
	}
}

// TestErrorOutputsRemainProtected ports:
// "error outputs remain protected" from test/pruner.test.ts
func TestErrorOutputsRemainProtected(t *testing.T) {
	messages := []map[string]any{
		makeUser("old"),
		makeTool("ctx_execute", defaultText, true),
		makeTool("read", defaultText, false),
		makeUser("middle"),
		makeUser("latest"),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 0
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if isErr, ok := messages[1]["isError"].(bool); !ok || !isErr {
		t.Fatalf("expected messages[1].isError == true")
	}
	if stats.MessagesPruned != 1 {
		t.Fatalf("expected exactly 1 message pruned, got %d", stats.MessagesPruned)
	}
	// The pruned one must be the non-error tool (read at index 2)
	b, _ := json.Marshal(prunedMessages[2])
	if !strings.Contains(string(b), "output pruned") {
		t.Fatalf("expected messages[2] (read) to be pruned")
	}
	// The error tool (at index 1) must not be pruned
	b1, _ := json.Marshal(prunedMessages[1])
	if strings.Contains(string(b1), "output pruned") {
		t.Fatalf("expected messages[1] (ctx_execute error) not to be pruned")
	}
}

// TestTheLatestTwoUserTurnsRemainProtected ports:
// "the latest two user turns remain protected" from test/pruner.test.ts
func TestTheLatestTwoUserTurnsRemainProtected(t *testing.T) {
	messages := []map[string]any{
		makeUser("old"),
		makeTool("read", defaultText, false),
		makeUser("middle"),
		makeTool("bash", defaultText, false),
		makeUser("latest"),
		makeTool("edit", defaultText, false),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 0
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 1 {
		t.Fatalf("expected exactly 1 message pruned, got %d", stats.MessagesPruned)
	}
	// Index 1 (read) is before the last two user turns, so it gets pruned
	b, _ := json.Marshal(prunedMessages[1])
	if !strings.Contains(string(b), "output pruned") {
		t.Fatalf("expected messages[1] (read) to be pruned")
	}
	// Indices 3 and 5 are in the latest 2 user turns, so protected
	b3, _ := json.Marshal(prunedMessages[3])
	if strings.Contains(string(b3), "output pruned") {
		t.Fatalf("expected messages[3] to remain protected")
	}
	b5, _ := json.Marshal(prunedMessages[5])
	if strings.Contains(string(b5), "output pruned") {
		t.Fatalf("expected messages[5] to remain protected")
	}
}

// TestCompactionSummaryStopsTheBackwardScan ports:
// "compaction summary stops the backward scan" from test/pruner.test.ts
func TestCompactionSummaryStopsTheBackwardScan(t *testing.T) {
	summary := map[string]any{
		"role":         "compactionSummary",
		"summary":      "done",
		"tokensBefore": 1,
		"timestamp":    int64(123456789),
	}
	messages := []map[string]any{
		makeTool("read", defaultText, false),
		summary,
		makeUser("old"),
		makeTool("bash", defaultText, false),
		makeUser("middle"),
		makeUser("latest"),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 0
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 1 {
		t.Fatalf("expected exactly 1 message pruned, got %d", stats.MessagesPruned)
	}
	// messages[0] is behind compactionSummary, so it must not be pruned
	b0, _ := json.Marshal(prunedMessages[0])
	if strings.Contains(string(b0), "output pruned") {
		t.Fatalf("expected messages[0] behind compactionSummary not to be pruned")
	}
}

// TestForceEquivalentZeroMinimumStillPreservesTheProtectBudget ports:
// "force-equivalent zero minimum still preserves the protect budget" from test/pruner.test.ts
func TestForceEquivalentZeroMinimumStillPreservesTheProtectBudget(t *testing.T) {
	messages := makeOldHistory([]string{"read"})
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 100000
		c.PruneMinimum = 0
	})
	_, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 0 {
		t.Fatalf("expected 0 messages pruned due to protect budget, got %d", stats.MessagesPruned)
	}
}

// TestAllowlistAndProtectedListAreEnforced ports:
// "allowlist and protected list are enforced" from test/pruner.test.ts
func TestAllowlistAndProtectedListAreEnforced(t *testing.T) {
	cfg := makeConfig(func(c *PruneConfig) {
		c.PrunableTools = []string{"read"}
		c.ProtectedTools = []string{"bash"}
	})
	if !IsPrunable("read", cfg) {
		t.Fatalf("expected 'read' to be prunable")
	}
	if IsPrunable("bash", cfg) {
		t.Fatalf("expected 'bash' to be protected")
	}
	if IsPrunable("ctx_execute", cfg) {
		t.Fatalf("expected 'ctx_execute' not to be prunable when allowlist is active")
	}
}

// TestPruneMarker verifies the exact character sequence of the prune marker (including U+2014 em dash).
func TestPruneMarker(t *testing.T) {
	expected := "[output pruned — ~20,000 tokens | read]"
	got := PruneMarker("read", 20000)
	if got != expected {
		t.Fatalf("PruneMarker mismatch:\n got:      %q\n expected: %q", got, expected)
	}
}

// TestFormatWithCommas tests direct formatting with thousands comma separators.
func TestFormatWithCommas(t *testing.T) {
	cases := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{20000, "20,000"},
	}
	for _, tc := range cases {
		got := FormatWithCommas(tc.input)
		if got != tc.expected {
			t.Errorf("FormatWithCommas(%d) = %q, expected %q", tc.input, got, tc.expected)
		}
	}
}

// TestProtectedTurnsZero verifies that with ProtectedTurns=0, even tools in the newest turn are prunable.
func TestProtectedTurnsZero(t *testing.T) {
	messages := []map[string]any{
		makeUser("latest"),
		makeTool("read", defaultText, false),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.ProtectedTurns = 0
		c.PruneProtect = 0
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 1 {
		t.Fatalf("expected 1 message pruned with ProtectedTurns=0, got %d", stats.MessagesPruned)
	}
	b, _ := json.Marshal(prunedMessages[1])
	if !strings.Contains(string(b), "output pruned") {
		t.Fatalf("expected latest tool message to be pruned")
	}
}

// TestPruneProtectExactBoundary tests when pruneProtect exactly equals the tokens of the first scanned tool.
func TestPruneProtectExactBoundary(t *testing.T) {
	t1 := makeTool("read", defaultText, false)
	t2 := makeTool("bash", defaultText, false)
	tokens1 := sdk.EstimateTokens(t1)

	messages := []map[string]any{
		makeUser("old"),
		t1,
		t2,
		makeUser("middle"),
		makeUser("latest"),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = tokens1
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 1 {
		t.Fatalf("expected exactly 1 message pruned, got %d", stats.MessagesPruned)
	}
	// In backward scan, t2 (index 2) is checked first and consumes the protect budget (tokens1).
	// Then t1 (index 1) is checked, budget is full, so t1 is pruned.
	b1, _ := json.Marshal(prunedMessages[1])
	if !strings.Contains(string(b1), "output pruned") {
		t.Fatalf("expected messages[1] (read) to be pruned")
	}
	b2, _ := json.Marshal(prunedMessages[2])
	if strings.Contains(string(b2), "output pruned") {
		t.Fatalf("expected messages[2] (bash) to be protected by exact budget")
	}
}

// TestToolResultWithoutIsErrorField verifies that tool messages omitting isError are treated as non-error (prunable).
func TestToolResultWithoutIsErrorField(t *testing.T) {
	toolWithoutErr := map[string]any{
		"role":       "toolResult",
		"toolName":   "read",
		"toolCallId": "call_1",
		"content": []any{
			map[string]any{"type": "text", "text": defaultText},
		},
		// "isError" omitted
	}
	messages := []map[string]any{
		makeUser("old"),
		toolWithoutErr,
		makeUser("middle"),
		makeUser("latest"),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.PruneProtect = 0
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 1 {
		t.Fatalf("expected tool without isError field to be pruned, got %d", stats.MessagesPruned)
	}
	b, _ := json.Marshal(prunedMessages[1])
	if !strings.Contains(string(b), "output pruned") {
		t.Fatalf("expected message 1 to be pruned")
	}
}

// TestMultiTurnProtectionWindow tests that with >2 turns (4 turns), only the latest 2 turns remain protected.
func TestMultiTurnProtectionWindow(t *testing.T) {
	messages := []map[string]any{
		makeUser("turn 0"),
		makeTool("t0", defaultText, false),
		makeUser("turn 1"),
		makeTool("t1", defaultText, false),
		makeUser("turn 2"),
		makeTool("t2", defaultText, false),
		makeUser("turn 3"),
		makeTool("t3", defaultText, false),
	}
	cfg := makeConfig(func(c *PruneConfig) {
		c.ProtectedTurns = 2
		c.PruneProtect = 0
		c.PruneMinimum = 0
	})
	prunedMessages, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 2 {
		t.Fatalf("expected 2 messages pruned, got %d", stats.MessagesPruned)
	}
	// t0 (idx 1) and t1 (idx 3) pruned
	b0, _ := json.Marshal(prunedMessages[1])
	b1, _ := json.Marshal(prunedMessages[3])
	if !strings.Contains(string(b0), "output pruned") || !strings.Contains(string(b1), "output pruned") {
		t.Fatalf("expected turn 0 and turn 1 tools to be pruned")
	}
	// t2 (idx 5) and t3 (idx 7) protected
	b2, _ := json.Marshal(prunedMessages[5])
	b3, _ := json.Marshal(prunedMessages[7])
	if strings.Contains(string(b2), "output pruned") || strings.Contains(string(b3), "output pruned") {
		t.Fatalf("expected turn 2 and turn 3 tools to remain protected")
	}
}
