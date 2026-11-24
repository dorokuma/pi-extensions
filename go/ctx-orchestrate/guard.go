package ctxorchestrate

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Codemode nested-call guard: a tool_call hook that confines every call a
// codemode script makes (parentToolCallId != "") to a positive read-only
// whitelist plus a layered call budget, while leaving every top-level call
// (parentToolCallId absent/"") untouched.
//
// Contract (verified against the host, not guessed):
//   - The hook runs inside the tool_call event; the SDK wraps the handler's
//     return in {"_pigToolCallResult": result} itself (pigsdk/extension.go:1630),
//     so a block is returned as map[string]any{"block": true, "reason": ...}
//     directly -- wrapping it again would decode to Block=false and fail open.
//   - Nested call ids are `<callerID>/<n>` (coding/nested_tool_calls.go:260).
//     A codemode script's direct calls carry parentToolCallId == the codemode
//     call's own toolCallId; a second hop (ctx_search_digest -> ctx_fs) carries
//     `<rootID>/<n>` and counts only against the total budget, not the
//     first-level one. PiG's own NestedCallLimits.MaxCalls=256 only truncates
//     the audit record, it does not stop execution -- the budget here is the
//     only physical cap.
//   - models.* (classify/generateImages/getModelsOfType/getAvailableOfType/
//     getModelOfType) are QuickJS globals that bypass tool_call entirely, so
//     they cannot be blocked per-call. The top-level codemode scan below is a
//     narrow anti-misuse rail (it only matches literal `models.<method>(`
//     text), not an adversarial boundary: a script can still reach the globals
//     through Unicode escapes or reflection. Physically disabling the globals
//     is an upstream PiG option (Options.DisableModels), tracked as a TODO.
//
// Return values: nil means "allow"; a map{"block":true,"reason":...} means the
// host rejects the call with that reason.

const (
	// budgetRingSize is the number of live codemode roots the tracker keeps.
	// Slots are recycled FIFO once full; an evicted root simply loses its
	// budget lineage and later calls fail closed as unknown-parent.
	budgetRingSize = 128
	// defaultMaxNestedCalls caps a script's direct (first-level) calls.
	defaultMaxNestedCalls = 16
	// minNestedCalls / maxNestedCalls bound CTX_ORCH_MAX_NESTED_CALLS.
	minNestedCalls = 1
	maxNestedCalls = 64
	// defaultMaxTotalNestedCalls caps every nested call under one root
	// (first-level script calls plus second-hop ctx_search_digest fan-out).
	defaultMaxTotalNestedCalls = 64
	// minTotalNestedCalls / maxTotalNestedCalls bound
	// CTX_ORCH_MAX_TOTAL_NESTED_CALLS.
	minTotalNestedCalls = 16
	maxTotalNestedCalls = 256
)

// sandboxModelsPattern is the narrow models.* rail: a literal `models.X(...)`
// call to one of the five codemode model globals. Deliberately not `\bmodels\b`
// (a plain `models` identifier in a path or comment is common and harmless).
var sandboxModelsPattern = regexp.MustCompile(`\bmodels\s*\.\s*(?:classify|generateImages|getModelsOfType|getAvailableOfType|getModelOfType)\b`)

// normalizedAction is the same normalization every routed action tool applies
// downstream (ctxmode/router.go and codegraph-go internal/server/tools.go):
// case-insensitive, surrounding whitespace ignored.
func normalizedAction(input map[string]any) string {
	action, _ := input["action"].(string)
	return strings.ToLower(strings.TrimSpace(action))
}

// hasActionCaseVariant reports whether input carries an "action"-equivalent key
// under a different spelling ("Action", "ACTION", ...). Go's encoding/json
// folds struct-tag names case-insensitively, so `{"action":"ls","Action":"rg"}`
// can be re-parsed downstream with a winner the whitelist never approved;
// any variant key is blocked outright (fail-closed).
func hasActionCaseVariant(input map[string]any) bool {
	for key := range input {
		if key != "action" && strings.EqualFold(key, "action") {
			return true
		}
	}
	return false
}

// actionToolInput enforces the common shape of the four action-routed
// whitelist tools: input must be a map carrying a non-empty string "action"
// and no case-variant action key. It returns (input, normalized action, ok).
func actionToolInput(data map[string]any) (map[string]any, string, bool) {
	input, ok := data["input"].(map[string]any)
	if !ok || input == nil {
		return nil, "", false
	}
	if hasActionCaseVariant(input) {
		return nil, "", false
	}
	action := normalizedAction(input)
	if action == "" {
		return nil, "", false
	}
	return input, action, true
}

// whitelistActionSets is the positive read-only whitelist: every action a
// nested call may name, per tool. ctx_stats and ctx_search_digest are not in
// the table because they take no routed action.
var whitelistActionSets = map[string]map[string]bool{
	"ctx_fs":  {"ls": true, "glob": true, "stat": true, "rg": true, "resolve": true, "related": true},
	"ctx_git": {"status": true, "diff": true, "log": true},
	"ctx_kb":  {"search": true, "stats": true, "doctor": true},
	"codegraph": {
		"explore": true, "search": true, "files": true, "node": true,
		"callers": true, "callees": true, "impact": true, "status": true,
		"affected": true, "communities": true, "search_facts": true,
	},
}

// checkNestedWhitelist is the default-deny gate, evaluated before any budget
// is spent (a rejected call must not burn the caller's allowance). It returns
// the block reason, or "" to allow.
func checkNestedWhitelist(toolName string, data map[string]any) string {
	switch toolName {
	case "ctx_fs", "ctx_git", "ctx_kb", "codegraph":
		_, action, ok := actionToolInput(data)
		if !ok || !whitelistActionSets[toolName][action] {
			return fmt.Sprintf("[ctx-orchestrate] codemode 沙盒仅允许只读白名单工具，已阻断 %s（action 缺失/非法或不在只读集内）；请改在顶层直连调用。", toolName)
		}
		return ""
	case "ctx_stats":
		// ctx_stats takes no action. A present input must be a map (anything
		// else fails closed), and every key is checked: a non-empty/non-nil
		// "action" value is malformed, and any case-variant duplicate key
		// ("Action", "ACTION", ...) is blocked outright -- the same JSON
		// case-folding bypass the action-routed tools refuse.
		rawInput, exists := data["input"]
		if exists && rawInput != nil {
			input, isMap := rawInput.(map[string]any)
			if !isMap {
				return "[ctx-orchestrate] codemode 沙盒仅允许只读白名单工具，已阻断 ctx_stats（input 非对象，fail-closed）；请改在顶层直连调用。"
			}
			for key, value := range input {
				if strings.EqualFold(key, "action") {
					if key != "action" || (value != nil && value != "") {
						return "[ctx-orchestrate] codemode 沙盒仅允许只读白名单工具，已阻断 ctx_stats（不接受 action 字段）；请改在顶层直连调用。"
					}
				}
			}
		}
		return ""
	case "ctx_search_digest":
		return ""
	default:
		return fmt.Sprintf("[ctx-orchestrate] codemode 沙盒仅允许只读白名单工具（ctx_fs/ctx_git/ctx_stats/ctx_kb 只读动作/codegraph 只读动作/ctx_search_digest），已阻断 %s；请改在顶层直连调用。", toolName)
	}
}

// clampBudgetEnv parses an env budget: unusable or out-of-range values fall
// back to fallback; an in-range integer wins.
func clampBudgetEnv(raw string, fallback, lo, hi int) int {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return fallback
	}
	v := int(math.Floor(n))
	if v < lo || v > hi {
		return fallback
	}
	return v
}

// MaxNestedCalls is the effective first-level cap: CTX_ORCH_MAX_NESTED_CALLS
// clamped to [minNestedCalls, maxNestedCalls], default defaultMaxNestedCalls.
func MaxNestedCalls() int {
	return clampBudgetEnv(os.Getenv("CTX_ORCH_MAX_NESTED_CALLS"), defaultMaxNestedCalls, minNestedCalls, maxNestedCalls)
}

// MaxTotalNestedCalls is the effective total cap:
// CTX_ORCH_MAX_TOTAL_NESTED_CALLS clamped to [minTotalNestedCalls,
// maxTotalNestedCalls], default defaultMaxTotalNestedCalls.
func MaxTotalNestedCalls() int {
	return clampBudgetEnv(os.Getenv("CTX_ORCH_MAX_TOTAL_NESTED_CALLS"), defaultMaxTotalNestedCalls, minTotalNestedCalls, maxTotalNestedCalls)
}

// budgetCounters are the per-root tallies.
type budgetCounters struct {
	// direct counts first-level calls (parentToolCallId == rootID).
	direct int
	// total counts every nested call under the root (direct + second hop).
	total int
	// totalOnly marks a root registered by a top-level ctx_search_digest
	// call: its own ctx_fs fan-out arrives as first-level nested calls
	// under its toolCallId -- the same quota shape as the second hop of a
	// codemode -> ctx_search_digest -- so only the total cap applies and
	// direct is never touched.
	totalOnly bool
}

// CallBudgetTracker counts nested calls per codemode root. One mutex makes
// lookup + increment + FIFO eviction atomic (the QuickJS fan-out enters the
// hook concurrently through Promise.all); the queue bounds memory to
// budgetRingSize live roots and is enough for any realistic session.
type CallBudgetTracker struct {
	mu    sync.Mutex
	roots map[string]*budgetCounters
	// ring is the registration queue, oldest first (FIFO). Register appends
	// and a full queue evicts ring[0]; there is no LIFO reuse slot.
	ring []string
	// maxNest/maxTot are test overrides: >0 fixes the cap, 0 (the
	// NewCallBudgetTracker default) re-reads the env at every debit so
	// pi-env-injected settings apply even when the tracker outlives init().
	maxNest int
	maxTot  int
}

// NewCallBudgetTracker builds a tracker that reads the env caps live.
func NewCallBudgetTracker() *CallBudgetTracker {
	return &CallBudgetTracker{roots: make(map[string]*budgetCounters)}
}

// effectiveCaps resolves the caps for one debit: a positive test override
// wins, otherwise the env is re-read so CTX_ORCH_MAX_NESTED_CALLS /
// CTX_ORCH_MAX_TOTAL_NESTED_CALLS injected after construction still apply.
func (t *CallBudgetTracker) effectiveCaps() (maxNest, maxTot int) {
	maxNest, maxTot = t.maxNest, t.maxTot
	if maxNest <= 0 {
		maxNest = MaxNestedCalls()
	}
	if maxTot <= 0 {
		maxTot = MaxTotalNestedCalls()
	}
	return maxNest, maxTot
}

// Register records a top-level codemode call as a budget root, bound by
// both the first-level and the total cap.
func (t *CallBudgetTracker) Register(rootID string) {
	t.register(rootID, false)
}

// RegisterTotalOnly records a top-level ctx_search_digest call as a
// total-only budget root: its ctx_fs fan-out debits only the total cap,
// matching the codemode -> ctx_search_digest second-hop quota.
func (t *CallBudgetTracker) RegisterTotalOnly(rootID string) {
	t.register(rootID, true)
}

// register records rootID as a budget root. A reused toolCallId resets
// that root's counters without re-queueing it (its FIFO position is
// kept); a fresh id is appended, evicting the oldest entry first when
// the queue is full.
func (t *CallBudgetTracker) register(rootID string, totalOnly bool) {
	if rootID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.roots[rootID]; !ok {
		if len(t.ring) >= budgetRingSize {
			evicted := t.ring[0]
			delete(t.roots, evicted)
			t.ring = t.ring[1:]
		}
		t.ring = append(t.ring, rootID)
	}
	t.roots[rootID] = &budgetCounters{totalOnly: totalOnly}
}

// Release drops the root's slot when the top-level codemode call finishes.
// It is called unconditionally on every codemode tool_result (never gated on
// GovernEnabled), so CTX_ORCH_GOVERN=0 cannot leak slots.
func (t *CallBudgetTracker) Release(rootID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.roots, rootID)
	for i, id := range t.ring {
		if id == rootID {
			t.ring = append(t.ring[:i], t.ring[i+1:]...)
			return
		}
	}
}

// Clear drops every registered root (agent_end: the turn's codemode calls
// are over, so any leftover slot would leak into the next turn).
func (t *CallBudgetTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roots = make(map[string]*budgetCounters)
	t.ring = nil
}

// isNumericSegments reports whether suffix is one or more `/<ASCII digits>`
// segments -- the only shape nested_tool_calls.go appends to a root id.
func isNumericSegments(suffix string) bool {
	if !strings.HasPrefix(suffix, "/") {
		return false
	}
	for _, segment := range strings.Split(suffix[1:], "/") {
		if segment == "" {
			return false
		}
		for i := 0; i < len(segment); i++ {
			if segment[i] < '0' || segment[i] > '9' {
				return false
			}
		}
	}
	return true
}

// debit spends one nested call for parentID: an exact registered-root match
// counts first-level + total (the first-level leg only when the root is not
// total-only), and a `rootID + "/" + <numeric segments>` match counts only
// the total (second hop). Anything else fails closed as an unknown parent.
// It returns the block reason when the root is unknown or a cap is
// exceeded, "" otherwise.
func (t *CallBudgetTracker) debit(parentID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	rootID, direct := t.matchLocked(parentID)
	if rootID == "" {
		return fmt.Sprintf("[ctx-orchestrate] 嵌套调用 parent=%q 未匹配任何已登记的 codemode 根调用，fail-closed 阻断；请改在顶层直连调用。", parentID)
	}
	counters := t.roots[rootID]
	maxNest, maxTot := t.effectiveCaps()
	nextTotal := counters.total + 1
	nextDirect := counters.direct
	if direct && !counters.totalOnly {
		nextDirect++
	}
	if direct && !counters.totalOnly && nextDirect > maxNest {
		return fmt.Sprintf("[ctx-orchestrate] 单脚本一级嵌套调用已达上限 %d（CTX_ORCH_MAX_NESTED_CALLS）：请拆分脚本或减少 ctx_search_digest 的 patterns 数量；确需更多调用请改在顶层直连。", maxNest)
	}
	if nextTotal > maxTot {
		return fmt.Sprintf("[ctx-orchestrate] 同一 codemode 根的嵌套调用总数已达上限 %d（CTX_ORCH_MAX_TOTAL_NESTED_CALLS，含 ctx_search_digest 二级 ctx_fs 扇出）：请拆分脚本或减少 patterns 数量。", maxTot)
	}
	counters.total = nextTotal
	if direct && !counters.totalOnly {
		counters.direct = nextDirect
	}
	return ""
}

// matchLocked finds the registered root of a nested parent id. Exact
// parentID == rootID wins (a first-level call); otherwise the longest
// registered root whose `root + "/"` prefix leads parentID wins, but only
// when the remaining suffix is one or more `/<ASCII digits>` segments --
// the sole shape PiG's nested_tool_calls.go generates, so `root/abc` never
// counts against root. The caller holds t.mu.
func (t *CallBudgetTracker) matchLocked(parentID string) (rootID string, direct bool) {
	if _, ok := t.roots[parentID]; ok {
		return parentID, true
	}
	best := ""
	for id := range t.roots {
		if len(id) > len(best) && strings.HasPrefix(parentID, id+"/") && isNumericSegments(parentID[len(id):]) {
			best = id
		}
	}
	if best != "" {
		return best, false
	}
	return "", false
}

// handleToolCall is the tool_call hook (sdk.EventToolCall). The SDK wraps
// the return value itself, so a block is map[string]any{"block": true,
// "reason": ...} -- never a hand-rolled _pigToolCallResult envelope.
//
// Top-level calls (parentToolCallId absent, nil or ""): only codemode and
// ctx_search_digest are inspected; every other tool returns nil untouched.
// A present-but-non-string parentToolCallId is a malformed payload and fails
// closed. Nested calls run the whitelist first (a rejected call must not burn
// budget) and then the layered budget. A panic on the nested path fails
// closed, unlike handleToolResult which fails open.
//
// budget is the per-extension-instance tracker (created in Extension()), so
// a packed-cell /reload or a second session never shares counters.
func handleToolCall(budget *CallBudgetTracker, data map[string]any) (result any, err error) {
	rawParent, present := data["parentToolCallId"]
	parentID, isString := rawParent.(string)
	if !present || rawParent == nil || (isString && parentID == "") {
		return handleTopLevelToolCall(budget, data)
	}
	if !isString {
		diagLog(fmt.Sprintf("tool_call guard: malformed parentToolCallId %T, fail-closed", rawParent))
		return map[string]any{"block": true, "reason": "[ctx-orchestrate] malformed parentToolCallId (non-string), fail-closed"}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			diagLog(fmt.Sprintf("tool_call guard panic (fail-closed): %v", recovered))
			result = map[string]any{"block": true, "reason": "[ctx-orchestrate] guard panic, fail-closed"}
			err = nil
		}
	}()

	toolName, _ := data["toolName"].(string)
	if reason := checkNestedWhitelist(toolName, data); reason != "" {
		diagLog(fmt.Sprintf("tool_call guard blocked %s: whitelist", toolName))
		return map[string]any{"block": true, "reason": reason}, nil
	}
	if reason := budget.debit(parentID); reason != "" {
		diagLog(fmt.Sprintf("tool_call guard blocked %s (parent=%s): budget", toolName, parentID))
		return map[string]any{"block": true, "reason": reason}, nil
	}
	return nil, nil
}

// handleTopLevelToolCall is the parentToolCallId-empty branch: every tool
// but codemode and ctx_search_digest passes 100% untouched. For codemode it
// runs the models.* rail and, on allow, registers the call id as a budget
// root so its nested calls have a ledger to count against. A top-level
// ctx_search_digest is registered as a total-only root: its own ctx_fs
// fan-out arrives as first-level nested calls under its toolCallId, the
// same quota shape as a codemode script's second hop.
func handleTopLevelToolCall(budget *CallBudgetTracker, data map[string]any) (any, error) {
	toolName, _ := data["toolName"].(string)
	if toolName != "codemode" && toolName != "ctx_search_digest" {
		return nil, nil
	}
	if toolName == "codemode" && !envFlag("CTX_ORCH_ALLOW_SANDBOX_MODELS", false) {
		input, _ := data["input"].(map[string]any)
		code, _ := input["code"].(string)
		if code != "" && sandboxModelsPattern.MatchString(code) {
			reason := "[ctx-orchestrate] codemode 沙盒已禁用 models.* 侧门接口（classify/generateImages/模型目录），以保持纯只读无副作用；如需模型调用请改在顶层直连（CTX_ORCH_ALLOW_SANDBOX_MODELS=1 可放开此护栏）。"
			diagLog("tool_call guard blocked codemode: models.* rail")
			return map[string]any{"block": true, "reason": reason}, nil
		}
	}
	toolCallID, _ := data["toolCallId"].(string)
	if toolName == "ctx_search_digest" {
		budget.RegisterTotalOnly(toolCallID)
	} else {
		budget.Register(toolCallID)
	}
	return nil, nil
}

// releaseTopLevelRoot frees the budget slot of a finished top-level codemode
// or ctx_search_digest call (parentToolCallId absent/empty). It is shared by
// the tool_result hook and the tool_execution_end event so an aborted or
// interrupted call cannot leak its slot; Release is idempotent.
func releaseTopLevelRoot(budget *CallBudgetTracker, data map[string]any) {
	toolName, _ := data["toolName"].(string)
	if toolName != "codemode" && toolName != "ctx_search_digest" {
		return
	}
	if parent, ok := data["parentToolCallId"].(string); ok && parent != "" {
		return
	}
	if id, _ := data["toolCallId"].(string); id != "" {
		budget.Release(id)
	}
}
