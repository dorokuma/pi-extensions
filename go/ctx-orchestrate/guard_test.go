package ctxorchestrate

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
// when the call is allowed (result nil or no "block": true). The tracker
// under test is guardTestBudget; resetBudget or setBudget installs it.
func blocked(t *testing.T, data map[string]any) string {
	t.Helper()
	result, err := handleToolCall(guardTestBudget, data)
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

// guardTestBudget is the tracker every helper in this file exercises; tests
// install their own with resetBudget / setBudget (restored by Cleanup).
var guardTestBudget = NewCallBudgetTracker()

// resetBudget installs a fresh tracker with fixed test caps (>0 overrides
// the env), restored by t.Cleanup.
func resetBudget(t *testing.T, direct, total int) {
	t.Helper()
	old := guardTestBudget
	t.Cleanup(func() { guardTestBudget = old })
	guardTestBudget = &CallBudgetTracker{
		roots:   make(map[string]*budgetCounters),
		maxNest: direct,
		maxTot:  total,
	}
}

// setBudget installs tracker, restored by t.Cleanup.
func setBudget(t *testing.T, tracker *CallBudgetTracker) {
	t.Helper()
	old := guardTestBudget
	t.Cleanup(func() { guardTestBudget = old })
	guardTestBudget = tracker
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
	guardTestBudget.Register("root")

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
	guardTestBudget.Register("root")

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
	guardTestBudget.Register("root")

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

// TestNestedCtxStatsFailsClosed pins the whole fail-closed surface of the
// ctx_stats branch: a present non-map input, a case-variant action key, and
// every non-empty/non-nil action value are blocked outright.
func TestNestedCtxStatsFailsClosed(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 64, 256)
	guardTestBudget.Register("root")

	cases := []struct {
		name  string
		input any
	}{
		{"string input", "json"},
		{"number input", float64(1)},
		{"array input", []any{"json"}},
		{"bool action", map[string]any{"action": true}},
		{"number action", map[string]any{"action": float64(1)}},
		{"case variant only", map[string]any{"Action": "json"}},
		{"empty case variant", map[string]any{"ACTION": ""}},
		{"nil case variant", map[string]any{"Action": nil}},
		{"variant beside empty action", map[string]any{"action": "", "Action": ""}},
		{"whitespace action", map[string]any{"action": "   "}},
	}
	for _, tc := range cases {
		data := nestedData("root", "ctx_stats", nil)
		data["input"] = tc.input
		if reason := blocked(t, data); reason == "" {
			t.Errorf("ctx_stats with %s must fail closed", tc.name)
		}
	}

	// A map input with an empty-string or nil literal "action" (and no case
	// variant) stays allowed, as before.
	for _, input := range []map[string]any{
		{"action": ""},
		{"action": nil},
	} {
		if reason := blocked(t, nestedData("root", "ctx_stats", input)); reason != "" {
			t.Errorf("ctx_stats with benign action input %v must pass, got %q", input, reason)
		}
	}
}

// TestTopLevelSearchDigestRegistersRoot (oracle S-3): a direct ctx_search_digest
// call registers its toolCallId as a budget root, so its own ctx_fs fan-out
// arrives as a nested call that passes and is counted.
func TestTopLevelSearchDigestRegistersRoot(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 8, 8)

	data := map[string]any{
		"toolName":   "ctx_search_digest",
		"toolCallId": "sd-1",
		"input":      map[string]any{"patterns": []any{"foo"}},
	}
	if reason := blocked(t, data); reason != "" {
		t.Fatalf("top-level ctx_search_digest must pass untouched, got %q", reason)
	}
	// Its second hop is a registered root's child: allowed and accounted.
	if reason := blocked(t, nestedData("sd-1/1", "ctx_fs", map[string]any{"action": "rg", "pattern": "foo"})); reason != "" {
		t.Fatalf("second hop under the digest root must pass, got %q", reason)
	}
	guardTestBudget.mu.Lock()
	total := guardTestBudget.roots["sd-1"].total
	guardTestBudget.mu.Unlock()
	if total != 1 {
		t.Fatalf("digest second hop total = %d, want 1", total)
	}
}

func TestBudgetFirstLevelCap(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 3, 10)
	guardTestBudget.Register("root")

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
	guardTestBudget.Register("root")

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
	guardTestBudget.Register("root")

	// A deeper-nested id matches the root when every trailing segment is
	// `/<ASCII digits>` and counts toward the total only.
	if reason := blocked(t, nestedData("root/2/4", "ctx_fs", map[string]any{"action": "ls"})); reason != "" {
		t.Fatalf("a numeric two-hop id under a registered root must pass, got %q", reason)
	}
	// An id that shares the string prefix but is not under root/ stays blocked.
	if reason := blocked(t, nestedData("rootx", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Error("an id merely sharing the prefix must not match")
	}
	// A non-numeric suffix is not a PiG nested id: fail closed, never counted.
	if reason := blocked(t, nestedData("root/abc", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Error("root/abc is not a nested id and must fail closed")
	}
	// Mixed numeric/non-numeric tails are rejected too.
	if reason := blocked(t, nestedData("root/2/abc", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Error("root/2/abc is not a nested id and must fail closed")
	}
	// An unknown parent fails closed.
	if reason := blocked(t, nestedData("unknown", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Error("an unregistered parent must fail closed")
	}
}

func TestBudgetLongestPrefixWins(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 1, 1)
	// Two registered roots in a prefix relationship: the longer one must win,
	// and the debit must land on root/9's ledger, not root's.
	guardTestBudget.Register("root")
	guardTestBudget.Register("root/9")
	if reason := blocked(t, nestedData("root/9/1", "ctx_fs", map[string]any{"action": "ls"})); reason != "" {
		t.Fatalf("longest-prefix match must find root/9, got %q", reason)
	}
	// root/9's total is now spent (maxTot=1): a further call under it blocks.
	if reason := blocked(t, nestedData("root/9", "ctx_fs", map[string]any{"action": "ls"})); reason == "" {
		t.Fatal("root/9's budget must be spent by the longer-prefix debit")
	}
	// The debit never touched root's ledger: its first-level call still passes.
	if reason := blocked(t, nestedData("root", "ctx_fs", map[string]any{"action": "ls"})); reason != "" {
		t.Fatalf("root's ledger must be untouched by the root/9 debit, got %q", reason)
	}
}

func TestBudgetRejectedCallDoesNotConsume(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 1, 2)
	guardTestBudget.Register("root")

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
	guardTestBudget.Register("root")
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
		t.Fatalf("first call must pass, got %q", reason)
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("the second call must hit the cap")
	}
	// A finished codemode call frees the slot for the next one.
	guardTestBudget.Release("root")
	guardTestBudget.Register("root-2")
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
	guardTestBudget.Register("root")

	// A top-level codemode tool_result releases the root even with
	// CTX_ORCH_GOVERN=0 (the release is independent of the cap).
	t.Setenv("CTX_ORCH_GOVERN", "0")
	if _, err := handleToolResult(guardTestBudget)(noCtx, map[string]any{
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
	guardTestBudget.Register("root")
	if _, err := handleToolResult(guardTestBudget)(noCtx, map[string]any{
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

// TestGuardFIFONoLIFOEviction is the oracle probe for the LIFO mis-eviction
// (S-1): with a released middle slot, two new roots must append FIFO and
// evict the oldest entry -- a LIFO tracker would reuse the freed slot and
// kick the freshly-registered live root instead.
func TestGuardFIFONoLIFOEviction(t *testing.T) {
	tracker := &CallBudgetTracker{roots: make(map[string]*budgetCounters), maxNest: 4, maxTot: 8}
	// Fill the queue to 255 so the last registration r-255 is the freshest.
	for i := 0; i < budgetRingSize+127; i++ {
		tracker.Register(fmt.Sprintf("r-%d", i))
	}
	// Free a middle slot, then register two fresh roots: the freed slot takes
	// live-a, and live-b evicts the oldest live root r-128 (strict FIFO).
	tracker.Release("r-127")
	tracker.Register("live-a")
	tracker.Register("live-b")
	if reason := tracker.debit("r-128"); reason == "" {
		t.Error("r-128 must be evicted (oldest FIFO entry), but still debits")
	}
	// The next-oldest root keeps its ledger until the queue fills again.
	if reason := tracker.debit("r-129"); reason != "" {
		t.Errorf("r-129 must still be live, got %q", reason)
	}
	// A LIFO tracker would have evicted live-a instead; it must still debit.
	if reason := tracker.debit("live-a"); reason != "" {
		t.Errorf("live-a must keep its ledger (FIFO eviction), got %q", reason)
	}
	if reason := tracker.debit("live-b"); reason != "" {
		t.Errorf("live-b must keep its ledger, got %q", reason)
	}
	tracker.mu.Lock()
	live := len(tracker.roots)
	tracker.mu.Unlock()
	if live != budgetRingSize {
		t.Fatalf("tracker holds %d roots, want %d", live, budgetRingSize)
	}
}

// TestBudgetDynamicCapsAfterConstruction (oracle S-2): a tracker built by
// NewCallBudgetTracker reads the env caps at every debit, so
// CTX_ORCH_MAX_NESTED_CALLS injected after construction (pi-env settings)
// applies immediately.
func TestBudgetDynamicCapsAfterConstruction(t *testing.T) {
	cleanEnv(t)
	setBudget(t, NewCallBudgetTracker()) // no fixed caps: live env reads
	guardTestBudget.Register("root")

	// The env arrives after construction, as pi-env injects settings.json.
	t.Setenv("CTX_ORCH_MAX_NESTED_CALLS", "2")
	for i := 0; i < 2; i++ {
		if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
			t.Fatalf("call %d within the late-injected cap must pass, got %q", i+1, reason)
		}
	}
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("the third call must hit the cap injected after construction")
	}
}

// TestToolExecutionEndReleasesRoot (oracle S-4): the tool_execution_end path
// frees the slot of a top-level codemode / ctx_search_digest call that never
// produced a tool_result (abort/interrupt); a nested end event never does.
func TestToolExecutionEndReleasesRoot(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 1, 1)
	guardTestBudget.Register("root")

	// A nested tool_execution_end must not release.
	releaseTopLevelRoot(guardTestBudget, map[string]any{
		"toolName": "ctx_fs", "toolCallId": "root/1", "parentToolCallId": "root",
	})
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason != "" {
		t.Fatalf("a nested execution end must not release, got %q", reason)
	}

	// A top-level codemode tool_execution_end releases even without a result.
	releaseTopLevelRoot(guardTestBudget, map[string]any{
		"toolName": "codemode", "toolCallId": "root",
	})
	if reason := blocked(t, nestedData("root", "ctx_stats", nil)); reason == "" {
		t.Fatal("the execution-end release must drop the root's ledger")
	}

	// A top-level ctx_search_digest releases the same way.
	guardTestBudget.Register("sd-1")
	releaseTopLevelRoot(guardTestBudget, map[string]any{
		"toolName": "ctx_search_digest", "toolCallId": "sd-1",
	})
	if reason := blocked(t, nestedData("sd-1", "ctx_fs", map[string]any{"action": "rg"})); reason == "" {
		t.Fatal("ctx_search_digest execution end must release its root")
	}
}

// TestAgentEndClearsBudget (oracle S-4): agent_end drops every leftover root
// so an aborted turn cannot leak slots into the next one.
func TestAgentEndClearsBudget(t *testing.T) {
	cleanEnv(t)
	resetBudget(t, 8, 8)
	guardTestBudget.Register("root-1")
	guardTestBudget.Register("root-2")
	guardTestBudget.Clear()
	for _, root := range []string{"root-1", "root-2"} {
		if reason := blocked(t, nestedData(root, "ctx_stats", nil)); reason == "" {
			t.Errorf("a root cleared at agent_end must fail closed: %s", root)
		}
	}
	guardTestBudget.mu.Lock()
	live := len(guardTestBudget.roots)
	queue := len(guardTestBudget.ring)
	guardTestBudget.mu.Unlock()
	if live != 0 || queue != 0 {
		t.Fatalf("Clear must empty both roots (%d) and the queue (%d)", live, queue)
	}
}

// TestCallBudgetTracker_Concurrent (oracle S-5): concurrent Register / debit /
// Release / Clear must be race-free under -race; afterwards, concurrent
// debits against fixed roots must never admit more than the caps and every
// admitted call must land on the ledger.
func TestCallBudgetTracker_Concurrent(t *testing.T) {
	cleanEnv(t)
	tracker := &CallBudgetTracker{roots: make(map[string]*budgetCounters), maxNest: 8, maxTot: 32}

	// Phase 1: lifecycle churn purely to provoke races.
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			root := fmt.Sprintf("root-%d", n%8)
			tracker.Register(root)
			_ = tracker.debit(root)
			tracker.Release(root)
			if n%16 == 0 {
				tracker.Clear()
			}
		}(i)
	}
	wg.Wait()

	// Phase 2: fixed roots, concurrent debits. An admitted call must be
	// reflected in the ledger exactly once, and no ledger may exceed a cap.
	const roots = 4
	for i := 0; i < roots; i++ {
		tracker.Register(fmt.Sprintf("c-%d", i))
	}
	var admitted [roots]atomic.Int64
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := n % roots
			root := fmt.Sprintf("c-%d", r)
			if reason := tracker.debit(root); reason == "" {
				admitted[r].Add(1)
			}
			// A deeper nested id of the same root shares the ledger.
			if reason := tracker.debit(root + "/7"); reason == "" {
				admitted[r].Add(1)
			}
		}(i)
	}
	wg.Wait()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for i := 0; i < roots; i++ {
		id := fmt.Sprintf("c-%d", i)
		counters, ok := tracker.roots[id]
		if !ok {
			t.Fatalf("fixed root %s lost from the ledger", id)
		}
		if got := admitted[i].Load(); int64(counters.total) != got {
			t.Fatalf("%s admitted %d calls but ledger total %d (lost update)", id, got, counters.total)
		}
		if counters.total > tracker.maxTot {
			t.Fatalf("%s total %d exceeds the total cap %d", id, counters.total, tracker.maxTot)
		}
		if counters.direct > tracker.maxNest {
			t.Fatalf("%s direct %d exceeds the first-level cap %d", id, counters.direct, tracker.maxNest)
		}
	}
	// Structural invariant: the FIFO queue and the ledger never diverge.
	if len(tracker.ring) != len(tracker.roots) {
		t.Fatalf("ring holds %d ids but roots has %d", len(tracker.ring), len(tracker.roots))
	}
	seen := make(map[string]bool, len(tracker.ring))
	for _, id := range tracker.ring {
		if seen[id] {
			t.Fatalf("%s queued twice", id)
		}
		seen[id] = true
		if _, ok := tracker.roots[id]; !ok {
			t.Fatalf("%s queued but has no ledger", id)
		}
	}
}
