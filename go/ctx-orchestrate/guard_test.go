package ctxorchestrate

import (
	"fmt"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// noCtx is the zero host context: the guard never touches it.
var noCtx = sdk.Context{}

// Tests for the tool_call guard: the positive read-only whitelist, the layered
// call budget, the models.* rail and the fail-closed paths. A blocked call is
// map[string]any{"block": true, "reason": ...} -- never a hand-wrapped
// _pigToolCallResult envelope (the SDK adds that itself).

// blocked extracts the block reason handleToolCall returns for data, or ""
// when the call is allowed (result nil or no "block": true).
func blocked(t *testing.T, data map[string]any) string {
	t.Helper()
	result, err := handleToolCall(noCtx, data)
	t.Helper()
	if err != nil {
		t.Fatalf("handleToolCall error = %v", err)
	}
	if result == nil {
		return ""
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map[string]any", result)
	}
	if blocked, _ := payload["block"].(bool); !blocked {
		t.Fatalf("non-block result must be nil, got %+v", payload)
	}
	if _, wrapped := payload["_pigToolCallResult"]; wrapped {
		t.Fatal("result must never hand-wrap _pigToolCallResult (the SDK wraps it itself)")
	}
	reason, _ := payload["reason"].(string)
	if reason == "" {
		t.Fatalf("block without a reason: %+v", payload)
	}
	return reason
}

// nestedData builds one tool_call event of a nested call.
func nestedData(parent, tool string, input map[string]any) map[string]any {
	data := map[string]any{"parentToolCallId": parent, "toolName": tool, "toolCallId": parent + "/9"}
	if input != nil {
		data["input"] = input
	}
	return data
}

// resetBudget swaps the shared tracker for a fresh one sized by caps.
func resetBudget(t *testing.T, direct, total int) {
	t.Helper()
	old := guardBudget
	t.Cleanup(func() { guardBudget = old })
	guardBudget = &CallBudgetTracker{
		roots:   make(map[string]*budgetCounters),
		maxNest: direct,
		maxTot:  total,
	}
}

func TestToolCallTopLevelPassesOtherTools(t *testing.T) {
	cleanEnv(t)
	cases := []map[string]any{
		{"toolName": "read", "toolCallId": "c1"},
		{"toolName": "ctx_run", "toolCallId": "c2", "parentToolCallId": ""},
		{"toolName": "jev_check", "toolCallId": "c3", "parentToolCallId": nil},
		{"toolName": "bash", "toolCallId": "c4"},
	}
	for i, data := range cases {
		if reason := blocked(t, data); reason != "" {
			t.Fatalf("case %d: top-level %v blocked: %s", i, data["toolName"], reason)
		}
	}
}

func TestToolCallMalformedParentFailsClosed(t *testing.T) {
	cleanEnv(t)
	for _, raw := range []any{float64(3), []any{"x"}, map[string]any{}, true} {
		data := map[string]any{"parentToolCallId": raw, "toolName": "ctx_fs", "toolCallId": "c9"}
		reason := blocked(t, data)
		if reason == "" || !strings.Contains(reason, "fail-closed") {
			t.Fatalf("parentToolCallId %T(%v) must fail closed, got %q", raw, raw, reason)
		}
	}
}

func TestToolCallModelsRail(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CTX_ORCH_ALLOW_SANDBOX_MODELS", "")
	resetBudget(t, 4, 8)

	// A literal models.<method>( call is blocked.
	for _, code := range []string{
		`await models.classify(m, req)`,
		`models. generateImages (m, x)`,
		`let f = models.getModelsOfType("chat"); f();`,
		`models.getAvailableOfType(t)`,
		`models.getModelOfType(t, p, i)`,
	} {
		data := map[string]any{"toolName": "codemode", "toolCallId": "root-1", "input": map[string]any{"code": code}}
		if reason := blocked(t, data); reason == "" || !strings.Contains(reason, "models.*") {
			t.Fatalf("code %q must hit the models.* rail, got %q", code, reason)
		}
	}

	// Plain `models` identifiers (paths, variables, comments) are not a call.
	for _, code := range []string{
		`const models = await tools.ctx_fs({action:"rg", pattern:"models.json"}); return models;`,
		`// list all models under ./models`,
		`const m = myModels.classify ? null : 1;`,
		`return typeof m\\u006fdels;`,
	} {
		data := map[string]any{"toolName": "codemode", "toolCallId": "root-ok", "input": map[string]any{"code": code}}
		if reason := blocked(t, data); reason != "" {
			t.Fatalf("code %q must not hit the rail, got %q", code, reason)
		}
	}

	// CTX_ORCH_ALLOW_SANDBOX_MODELS=1 skips the rail entirely.
	t.Setenv("CTX_ORCH_ALLOW_SANDBOX_MODELS", "1")
	data := map[string]any{"toolName": "codemode", "toolCallId": "root-2", "input": map[string]any{"code": `models.classify(m, x)`}}
	if reason := blocked(t, data); reason != "" {
		t.Fatalf("opt-out env must skip the rail, got %q", reason)
	}
}

func TestNestedWhitelistMatrix(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 64, 256)
	guardBudget.Register("root")

	allow := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{"ctx_fs ls", "ctx_fs", map[string]any{"action": "ls"}},
		{"ctx_fs rg case+space", "ctx_fs", map[string]any{"action": " RG "}},
		{"ctx_fs related", "ctx_fs", map[string]any{"action": "related"}},
		{"ctx_fs resolve", "ctx_fs", map[string]any{"action": "resolve"}},
		{"ctx_git log", "ctx_git", map[string]any{"action": "log"}},
		{"ctx_kb search", "ctx_kb", map[string]any{"action": "search"}},
		{"ctx_kb doctor", "ctx_kb", map[string]any{"action": " DOCTOR"}},
		{"ctx_stats empty", "ctx_stats", nil},
		{"ctx_stats no action", "ctx_stats", map[string]any{}},
		{"codegraph node", "codegraph", map[string]any{"action": "Node"}},
		{"codegraph search_facts", "codegraph", map[string]any{"action": "search_facts"}},
		{"codegraph affected", "codegraph", map[string]any{"action": "affected"}},
		{"ctx_search_digest", "ctx_search_digest", map[string]any{"patterns": []any{"x"}}},
	}
	for _, tc := range allow {
		if reason := blocked(t, nestedData("root", tc.tool, tc.input)); reason != "" {
			t.Errorf("%s must pass, got %q", tc.name, reason)
		}
	}
}

func TestNestedWhitelistRejects(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 64, 256)
	guardBudget.Register("root")

	cases := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		// The write/mutation surface and every non-whitelisted tool.
		{"ctx_run", "ctx_run", map[string]any{"action": "execute", "argv": []any{"id"}}},
		{"ctx_bg", "ctx_bg", map[string]any{"action": "wait"}},
		{"ctx_report", "ctx_report", map[string]any{"content": "x"}},
		{"jev_check", "jev_check", map[string]any{}},
		{"ask_user_question", "ask_user_question", map[string]any{}},
		{"vcc_recall", "vcc_recall", map[string]any{}},
		{"read", "read", map[string]any{"path": "/etc/passwd"}},
		{"edit", "edit", map[string]any{"path": "a"}},
		{"write", "write", map[string]any{"path": "a"}},
		{"bash", "bash", map[string]any{"command": "id"}},
		{"codemode itself", "codemode", map[string]any{"code": "1"}},
		{"future tool", "ctx_new_tool", map[string]any{}},
		// Write actions of whitelisted tools.
		{"ctx_kb index", "ctx_kb", map[string]any{"action": "index"}},
		{"ctx_kb purge", "ctx_kb", map[string]any{"action": "PURGE"}},
		{"ctx_kb fetch", "ctx_kb", map[string]any{"action": "fetch"}},
		{"ctx_kb empty action", "ctx_kb", map[string]any{"action": ""}},
		{"ctx_fs unknown", "ctx_fs", map[string]any{"action": "write"}},
		{"ctx_git push", "ctx_git", map[string]any{"action": "push"}},
		{"codegraph store_fact", "codegraph", map[string]any{"action": "store_fact"}},
		{"codegraph STORE_FACT", "codegraph", map[string]any{"action": " STORE_FACT "}},
		{"codegraph missing action", "codegraph", map[string]any{}},
		{"ctx_stats action", "ctx_stats", map[string]any{"action": "json"}},
	}
	for _, tc := range cases {
		reason := blocked(t, nestedData("root", tc.tool, tc.input))
		if reason == "" {
			t.Errorf("%s must be blocked", tc.name)
		}
	}

	// input that is not a map fails closed for the action tools.
	if reason := blocked(t, nestedData("root", "ctx_fs", nil)); reason == "" {
		t.Error("ctx_fs without input must be blocked")
	}
	data := nestedData("root", "codegraph", nil)
	data["input"] = "store_fact"
	if reason := blocked(t, data); reason == "" {
		t.Error("codegraph with a string input must be blocked")
	}
}

func TestNestedActionCaseVariantFailsClosed(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 64, 256)
	guardBudget.Register("root")

	for _, tool := range []string{"ctx_fs", "ctx_git", "ctx_kb", "codegraph"} {
		data := nestedData("root", tool, map[string]any{"action": "ls", "Action": "store_fact"})
		if tool == "ctx_fs" || tool == "ctx_git" || tool == "ctx_kb" {
			data["input"].(map[string]any)["Action"] = "nope"
		}
		if reason := blocked(t, data); reason == "" {
			t.Errorf("%s with a case-variant action key must fail closed", tool)
		}
	}
}

func TestBudgetFirstLevelCap(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 3, 10)
	guardBudget.Register("root")

	for i := 0; i < 3; i++ {
		if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
			t.Fatalf("call %d within the cap must pass, got %q", i+1, reason)
		}
	}
	reason := blocked(t, nestedData("root", "ctx_stats", nil))
	if reason == "" || !strings.Contains(reason, "上限") {
		t.Fatalf("call %d beyond the first-level cap must block, got %q", 4, reason)
	}
}

func TestBudgetSecondHopCountsOnlyTotal(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 2, 4)
	guardBudget.Register("root")

	// Two direct calls fill the first-level cap.
	for i := 0; i < 2; i++ {
		if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
			t.Fatalf("direct call %d blocked: %q", i+1, reason)
		}
	}
	// A second hop (ctx_search_digest's own ctx_fs) still passes: it counts
	// only the total, not the first-level cap.
	if reason := blocked(t, nestedData("root/1", "ctx_fs", map[string]any{"action": "rg"})); reason != "" {
		t.Fatalf("second hop must not consume the first-level cap, got %q", reason)
	}
	if reason := blocked(t, nestedData("root/2", "ctx_fs", map[string]any{"action": "rg"})); reason != "" {
		t.Fatalf("second hop within the total cap must pass, got %q", reason)
	}
	// Total = 2 direct + 2 second hop = cap: the next call, either level, blocks.
	if reason := blocked(t, nestedData("root/3", "ctx_fs", map[string]any{"action": "rg"})); reason == "" {
		t.Fatal("the total cap must stop a further second hop")
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("the total cap must stop a further direct call")
	}
}

func TestBudgetNumericSuffixFallback(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 8, 8)
	guardBudget.Register("root")

	// A deeper-nested id peels trailing /<number> segments to find the root.
	data := nestedData("root/2/4", "ctx_fs", map[string]any{"action": "ls"})
	if reason := blocked(t, data); reason != "" {
		t.Fatalf("nested id under a registered root must pass via suffix peel, got %q", reason)
	}
	// An id that shares the string prefix but is not under root/ stays blocked.
	if reason := blocked(t, nestedData("rootx", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Error("an id merely sharing the prefix must not match")
	}
	// A non-numeric tail is never peeled: it stays a prefix match instead
	// (parent "root/abc" is a child of the registered root, so it counts
	// toward the total only).
	if reason := blocked(t, nestedData("root/abc", "ctx_fs", map[string]any{"action": "ls"})); reason != "" {
		t.Errorf("a non-numeric suffix is a second-hop prefix match, must pass: %q", reason)
	}
	// An unknown parent fails closed.
	if reason := blocked(t, nestedData("unknown", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Error("an unregistered parent must fail closed")
	}
}

func TestBudgetLongestPrefixWins(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 8, 8)
	// Two registered roots in a prefix relationship: the longer one must win.
	guardBudget.Register("root")
	guardBudget.Register("root/9")
	if reason := blocked(t, nestedData("root/9/1", "ctx_fs", map[string]any{"action": "ls"})); reason != "" {
		t.Fatalf("longest-prefix match must find root/9, got %q", reason)
	}
	t.Cleanup(func() { guardBudget.Release("root"); guardBudget.Release("root/9") })
}

func TestBudgetRejectedCallDoesNotConsume(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 1, 2)
	guardBudget.Register("root")

	// Whitelist rejections burn no budget: the single first-level slot must
	// still be there for a legitimate call.
	for i := 0; i < 3; i++ {
		if reason := blocked(t, nestedData("root", "ctx_run", map[string]any{"action": "execute"})); reason == "" {
			t.Fatal("ctx_run must be blocked")
		}
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
		t.Fatalf("rejected calls must not consume the budget, got %q", reason)
	}
}

func TestBudgetRelease(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 1, 1)
	guardBudget.Register("root")
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
		t.Fatalf("first call must pass, got %q", reason)
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("the second call must hit the cap")
	}
	// A finished codemode call frees the slot for the next one.
	guardBudget.Release("root")
	guardBudget.Register("root-2")
	if reason := blocked(t, nestedData("root-2", "ctx_stats", nil)); reason != "" {
		t.Fatalf("a fresh root must pass after release, got %q", reason)
	}
	// The released root's children fail closed (its ledger is gone).
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("calls under a released root must fail closed")
	}
}

func TestHandleToolResultReleasesBudget(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 1, 1)
	guardBudget.Register("root")

	// A top-level codemode tool_result releases the root even with
	// CTX_ORCH_GOVERN=0 (the release is independent of the cap).
	t.Setenv("CTX_ORCH_GOVERN", "0")
	if _, err := handleToolResult(noCtx, map[string]any{
		"toolName": "codemode", "toolCallId": "root",
		"content": []any{textBlock("ok")},
	}); err != nil {
		t.Fatalf("handleToolResult error = %v", err)
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("the released root's ledger must be gone")
	}
	// A nested result never releases (parentToolCallId set).
	resetBudget(t, 1, 1)
	guardBudget.Register("root")
	if _, err := handleToolResult(noCtx, map[string]any{
		"toolName": "ctx_fs", "toolCallId": "root/1", "parentToolCallId": "root",
		"content": []any{textBlock("ok")},
	}); err != nil {
		t.Fatalf("handleToolResult error = %v", err)
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
		t.Fatalf("a nested result must not release the root, got %q", reason)
	}
}

func TestBudgetEnvClamps(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", defaultMaxNestedCalls},
		{"in range", "8", 8},
		{"below range", "0", defaultMaxNestedCalls},
		{"above range", "999", defaultMaxNestedCalls},
		{"junk", "abc", defaultMaxNestedCalls},
		{"float", "7.9", 7},
	} {
		t.Run("nested/"+tc.name, func(t *testing.T) {
			t.Setenv("CTX_ORCH_MAX_NESTED_CALLS", tc.env)
			if got := MaxNestedCalls(); got != tc.want {
				t.Errorf("MaxNestedCalls() = %d, want %d", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", defaultMaxTotalNestedCalls},
		{"in range", "128", 128},
		{"below range", "4", defaultMaxTotalNestedCalls},
		{"above range", "9999", defaultMaxTotalNestedCalls},
		{"junk", "x", defaultMaxTotalNestedCalls},
	} {
		t.Run("total/"+tc.name, func(t *testing.T) {
			t.Setenv("CTX_ORCH_MAX_TOTAL_NESTED_CALLS", tc.env)
			if got := MaxTotalNestedCalls(); got != tc.want {
				t.Errorf("MaxTotalNestedCalls() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGuardRingEviction(t *testing.T) {
	tracker := &CallBudgetTracker{roots: make(map[string]*budgetCounters), maxNest: 4, maxTot: 8}
	for i := 0; i < budgetRingSize+5; i++ {
		tracker.Register(fmt.Sprintf("root-%d", i))
	}
	tracker.mu.Lock()
	live := len(tracker.roots)
	tracker.mu.Unlock()
	if live != budgetRingSize {
		t.Fatalf("tracker holds %d roots, the ring must cap at %d", live, budgetRingSize)
	}
	// The oldest roots were evicted; their calls fail closed.
	if reason := tracker.debit("root-0"); reason == "" {
		t.Error("an evicted root must fail closed")
	}
	// The newest roots still have a ledger.
	if reason := tracker.debit(fmt.Sprintf("root-%d", budgetRingSize+4)); reason != "" {
		t.Fatalf("a live root must pass, got %q", reason)
	}
}
