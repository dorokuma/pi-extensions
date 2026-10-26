package pivcc

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Tests for the hook assembly in hook.go: what session_before_compact returns
// for a real session, and when it declines or cancels instead.

// hookBranch builds the branchEntries payload a session_before_compact event
// carries: three user turns' worth of live messages on one branch.
func hookBranch() []map[string]any {
	entries := transcript(3)
	// One assistant turn carries a tool call, so the summary has a file to
	// report and the stats have a tool call to count.
	entries[1]["message"].(map[string]any)["content"] = toolCallContent("call-1", "edit", map[string]any{
		"path": "/root/workspace/pi-extensions/go/pi-vcc/format.go", "content": "package pivcc",
	})
	return entries
}

// toolCallContent is an assistant message's content: one tool call.
func toolCallContent(id, name string, args map[string]any) any {
	return []any{map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": args}}
}

// hookEvent is the event data a session_before_compact request carries.
func hookEvent(reason string, branchEntries []map[string]any, customInstructions string, preparation map[string]any) map[string]any {
	data := map[string]any{
		"reason":        reason,
		"branchEntries": branchEntries,
	}
	if customInstructions != "" {
		data["customInstructions"] = customInstructions
	}
	if preparation != nil {
		data["preparation"] = preparation
	}
	return data
}

// compactionOf reads the compaction a successful handler returned.
func compactionOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if response["error"] != nil {
		t.Fatalf("session_before_compact failed: %#v", response["error"])
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want the compaction payload", response["result"])
	}
	compaction, ok := result["compaction"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want a compaction", result)
	}
	return compaction
}

// TestHook_SessionBeforeCompactProducesSummary is the happy path: /pi-vcc asks
// for a compaction and the hook answers with pi-vcc's summary and details.
func TestHook_SessionBeforeCompactProducesSummary(t *testing.T) {
	h := newHost(t)
	h.setState(true)
	response := h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"previousSummary": "",
		"tokensBefore":    float64(4000),
		"fileOps": map[string]any{
			"read":    []any{"/root/workspace/pi-extensions/go/pi-vcc/format.go"},
			"written": []any{"/root/workspace/pi-extensions/go/pi-vcc/report.go"},
		},
	}))
	compaction := compactionOf(t, response)

	summary, _ := compaction["summary"].(string)
	if summary == "" {
		t.Fatal("the hook returned no summary")
	}
	if !strings.Contains(summary, "[Early Session Requests]") {
		t.Fatalf("the summary has no early session requests:\n%s", summary)
	}
	details, _ := compaction["details"].(map[string]any)
	if details["compactor"] != CompactionDetailsCompactor {
		t.Fatalf("details = %#v, want the pi-vcc stamp", details)
	}
	if details["version"] != float64(CompactionDetailsVersion) {
		t.Fatalf("details.version = %v, want %d", details["version"], CompactionDetailsVersion)
	}
	if details["sourceMessageCount"] == nil {
		t.Fatalf("details = %#v, want sourceMessageCount", details)
	}
	// The kept tail's first entry id is reported so pi knows where to cut.
	if firstKept, _ := compaction["firstKeptEntryId"].(string); firstKept == "" {
		t.Fatalf("compaction = %#v, want firstKeptEntryId", compaction)
	}
	if compaction["tokensBefore"] != float64(4000) {
		t.Fatalf("tokensBefore = %v", compaction["tokensBefore"])
	}
	// The stats the /pi-vcc command's toast reads were recorded.
	state := extStateOf(h.ext)
	if _, ok := state.lastStatsRead(); !ok {
		t.Fatal("the hook recorded no stats")
	}
	if !state.getLastCompactWasPiVcc() {
		t.Fatal("the hook did not record that this was a /pi-vcc compaction")
	}
}

// TestHook_SessionBeforeCompactDeclinesWithoutOverride is the default path: a
// plain /compact is left to pi unless the settings say otherwise.
func TestHook_SessionBeforeCompactDeclinesWithoutOverride(t *testing.T) {
	h := newHost(t)
	h.setState(true)
	h.setSettings(map[string]any{"overrideDefaultCompaction": false})

	response := h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), "", nil))
	if response["error"] != nil {
		t.Fatalf("session_before_compact failed: %#v", response["error"])
	}
	if response["result"] != nil {
		t.Fatalf("result = %#v, want nil (decline to pi)", response["result"])
	}
}

// TestHook_SessionBeforeCompactHonoursOverride pins the opposite setting: with
// overrideDefaultCompaction on, a plain /compact is handled by pi-vcc too.
func TestHook_SessionBeforeCompactHonoursOverride(t *testing.T) {
	h := newHost(t)
	h.setState(true)
	h.setSettings(map[string]any{"overrideDefaultCompaction": true})

	response := h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), "", nil))
	compaction := compactionOf(t, response)
	if summary, _ := compaction["summary"].(string); summary == "" {
		t.Fatal("no summary for an overridden default compaction")
	}
}

// TestHook_SessionBeforeCompactSkipsProvider pins the provider opt-out (#27):
// a provider in skipForProviders is left to pi, while /pi-vcc still runs.
func TestHook_SessionBeforeCompactSkipsProvider(t *testing.T) {
	h := newHost(t)
	h.setState(true)
	h.setModel("github-copilot", "gpt-5")
	h.setSettings(map[string]any{"skipForProviders": []any{"GitHub-Copilot"}})

	response := h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), "", nil))
	if response["result"] != nil {
		t.Fatalf("result = %#v, want nil for a skipped provider", response["result"])
	}

	// An explicit /pi-vcc bypasses the skip.
	response = h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, nil))
	compaction := compactionOf(t, response)
	if summary, _ := compaction["summary"].(string); summary == "" {
		t.Fatal("an explicit /pi-vcc produced no summary")
	}
}

// TestHook_SessionBeforeCompactCancelsNothingToCompact pins the cancel path: a
// session with no live messages cancels the compaction and warns.
func TestHook_SessionBeforeCompactCancelsNothingToCompact(t *testing.T) {
	h := newHost(t)
	h.setState(true)

	response := h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", nil, PiVccCompactInstruction, nil))
	if response["error"] != nil {
		t.Fatalf("session_before_compact failed: %#v", response["error"])
	}
	result, _ := response["result"].(map[string]any)
	if result == nil || result["cancel"] != true {
		t.Fatalf("result = %#v, want a cancel", response["result"])
	}
	h.waitForCalls("ui.notify", 1)
	if !strings.Contains(h.notes(), "Nothing to compact") {
		t.Fatalf("notifications = %q", h.notes())
	}
}

// TestHook_SessionCompactToastPinsStats pins the /compact path's success toast:
// only a compaction the persisted entry attributes to pi-vcc notifies, and the
// recorded stats are shown.
func TestHook_SessionCompactToastPinsStats(t *testing.T) {
	h := newHost(t)
	h.setState(true)

	// First let pi-vcc compact, so the stats exist.
	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), PiVccCompactInstruction, map[string]any{
		"tokensBefore": float64(4000),
	}))
	state := extStateOf(h.ext)
	state.setLastCompactWasPiVcc(false) // /compact, not /pi-vcc

	// fromExtension without a pi-vcc compaction entry: nothing happens.
	h.event(sdk.EventSessionCompact, map[string]any{"fromExtension": true, "reason": "manual"})
	if len(h.callsOf("ui.notify")) != 0 {
		t.Fatalf("notifications = %q, want none", h.notes())
	}

	// With the pi-vcc stamp on the compaction entry, the toast fires.
	h.event(sdk.EventSessionCompact, map[string]any{
		"fromExtension": true,
		"reason":        "manual",
		"compactionEntry": map[string]any{
			"details": map[string]any{"compactor": CompactionDetailsCompactor},
		},
	})
	h.waitForCalls("ui.notify", 1)
	if !strings.Contains(h.notes(), "pi-vcc: kept") {
		t.Fatalf("notifications = %q, want the compaction stats toast", h.notes())
	}

	// Not from an extension at all: no toast.
	before := len(h.callsOf("ui.notify"))
	h.event(sdk.EventSessionCompact, map[string]any{"reason": "manual"})
	if len(h.callsOf("ui.notify")) != before {
		t.Fatalf("notifications = %q, want no new toast", h.notes())
	}
}

// TestHook_SessionCompactSendsPendingFollowUp pins the follow-up prompt a
// compaction request carries: it is sent once the compaction lands.
func TestHook_SessionCompactSendsPendingFollowUp(t *testing.T) {
	h := newHost(t)
	h.setState(true)

	// A plain /compact with a prompt argument: pi-vcc takes it over (the
	// default settings override the core compaction) and records the prompt.
	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), "keep:2 now run the tests", map[string]any{
		"tokensBefore": float64(4000),
	}))
	state := extStateOf(h.ext)
	if prompt, ok := state.takePendingFollowUp(); !ok || prompt != "now run the tests" {
		t.Fatalf("pending follow-up = %q (ok=%v), want the prompt argument", prompt, ok)
	}
	// Re-arm: the take above consumed the prompt the first compaction recorded,
	// so a second compaction request puts it back.
	h.event(sdk.EventSessionBeforeCompact, hookEvent("manual", hookBranch(), "keep:2 now run the tests", map[string]any{
		"tokensBefore": float64(4000),
	}))

	h.event(sdk.EventSessionCompact, map[string]any{
		"fromExtension": true,
		"reason":        "manual",
		"compactionEntry": map[string]any{
			"details": map[string]any{"compactor": CompactionDetailsCompactor},
		},
	})
	h.waitForCalls("sendUserMessage", 1)
	sends := h.callsOf("sendUserMessage")
	content, _ := sends[0].Args["content"].(string)
	if content != "now run the tests" {
		t.Fatalf("follow-up = %q, want the prompt argument", content)
	}
	// The pending prompt is consumed, not repeated.
	if _, ok := state.takePendingFollowUp(); ok {
		t.Fatal("the pending follow-up survived the compaction")
	}
}

// TestHook_PiVersionHonoursEnv pins the auto-continue gate's version source.
func TestHook_PiVersionHonoursEnv(t *testing.T) {
	t.Setenv("PI_VCC_PI_VERSION", " 0.84.4 ")
	if got := PiVersion(); got != "0.84.4" {
		t.Fatalf("PiVersion = %q, want the trimmed env value", got)
	}
	t.Setenv("PI_VCC_PI_VERSION", "   ")
	if got := PiVersion(); got != "1.0.0" {
		t.Fatalf("PiVersion = %q, want the PiG default for a blank value", got)
	}
}

// TestHook_ProviderSkippedIsCaseInsensitive pins the comparison the TS code
// makes: both sides are lower-cased.
func TestHook_ProviderSkippedIsCaseInsensitive(t *testing.T) {
	if !providerSkipped("GitHub-Copilot", []string{"github-copilot"}) {
		t.Fatal("a differently-cased provider was not skipped")
	}
	if !providerSkipped("anthropic", []string{"Anthropic"}) {
		t.Fatal("a differently-cased skip entry did not match")
	}
	if providerSkipped("anthropic", []string{"github-copilot"}) {
		t.Fatal("an unrelated provider was skipped")
	}
	if providerSkipped("", []string{"github-copilot"}) {
		t.Fatal("an unknown provider was skipped")
	}
}

// TestHook_CompactionEntryIsPiVcc pins the persisted-entry guard the
// session_compact handler reads.
func TestHook_CompactionEntryIsPiVcc(t *testing.T) {
	if !compactionEntryIsPiVcc(map[string]any{
		"compactionEntry": map[string]any{"details": map[string]any{"compactor": "pi-vcc"}},
	}) {
		t.Fatal("the pi-vcc stamp was not recognised")
	}
	for _, data := range []map[string]any{
		{},
		{"compactionEntry": map[string]any{}},
		{"compactionEntry": map[string]any{"details": map[string]any{"compactor": "other"}}},
		{"compactionEntry": "not an object"},
	} {
		if compactionEntryIsPiVcc(data) {
			t.Fatalf("%#v was read as a pi-vcc compaction", data)
		}
	}
}

// TestHook_DebugSnapshotWrittenWhenEnabled pins dbg(): the snapshot is written
// only when the setting is on, and it describes a real compaction.
func TestHook_DebugSnapshotWrittenWhenEnabled(t *testing.T) {
	path := t.TempDir() + "/pi-vcc-debug.json"
	WriteDebugSnapshot(path, false, map[string]any{"x": 1})
	if _, err := readFileForTest(path); err == nil {
		t.Fatal("a disabled debug setting wrote a snapshot")
	}
	WriteDebugSnapshot(path, true, map[string]any{"usedOwnCut": true})
	data, err := readFileForTest(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
	if decoded["usedOwnCut"] != true {
		t.Fatalf("snapshot = %#v", decoded)
	}
}
