package cachestats

import (
	"fmt"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file holds the shared helpers of the Go test suite. The TypeScript
// suite drives the extension through mock ctx objects (test/helpers.mjs
// mockContext); the SDK's Context has no mockable form, so the Go suite drives
// the same code paths directly: the pure helpers, the state transitions and
// the command output builders. The extension wiring itself is covered by the
// SDK IPC harness in extension_test.go.

func intPtr(value int) *int { return &value }

func floatPtr(value float64) *float64 { return &value }

func strPtr(value string) *string { return &value }

// usageMap builds a raw usage payload.
func usageMap(pairs ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

// assistantEntry builds a session entry carrying an assistant usage payload.
func assistantEntry(usage any) map[string]any {
	return map[string]any{
		"type":    "message",
		"message": map[string]any{"role": "assistant", "usage": usage},
	}
}

// roleEntry builds a session entry for an arbitrary message role.
func roleEntry(role string, usage any) map[string]any {
	message := map[string]any{"role": role}
	if usage != nil {
		message["usage"] = usage
	}
	return map[string]any{"type": "message", "message": message}
}

// typedEntry builds a non-message session entry (compaction / branch_summary).
func typedEntry(entryType string, usage any) map[string]any {
	out := map[string]any{"type": entryType}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

// anyList converts a variadic list of entries to the []any the scan takes.
func anyList(entries ...map[string]any) []any {
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	return out
}

// recordingTheme is the TS tracking theme: it records every fg call and
// rewrites each style to a readable marker.
type recordingTheme struct{ calls []styleCall }

type styleCall struct {
	token string
	text  string
}

func (t *recordingTheme) Fg(token, text string) string {
	t.calls = append(t.calls, styleCall{token: token, text: text})
	switch token {
	case "warning":
		return "[WARN:" + text + "]"
	case "error":
		return "[ERR:" + text + "]"
	case "text":
		return "[TXT:" + text + "]"
	case "dim":
		return "[DIM:" + text + "]"
	}
	return text
}

// plainTheme renders text unchanged, like the TS `{ fg: (_s, t) => t }`.
type plainTheme struct{}

func (plainTheme) Fg(_, text string) string { return text }

// fakeHost records the agent_end / turn_end side effects.
type fakeHost struct {
	entries []appendedEntry
	footer  int
	renders []bool
}

type appendedEntry struct {
	customType string
	data       map[string]any
}

func (h *fakeHost) appendEntry(customType string, data any) error {
	entry, _ := data.(map[string]any)
	h.entries = append(h.entries, appendedEntry{customType: customType, data: entry})
	return nil
}

func (h *fakeHost) readFooter(*instanceState) { h.footer++ }

func (h *fakeHost) refreshFooter(_ *instanceState, force bool) { h.renders = append(h.renders, force) }

// joinedStats is the TS dumpNotices equivalent over the stats lines: every
// line goes through escapeForNotify, as showStats notifies it.
func joinedStats(state *instanceState, entries []any) string {
	return notifyText(statsLines(state, sessionAggregateOf(entries, entries != nil)))
}

// joinedPrefix is the TS dumpNotices equivalent over the prefix lines.
func joinedPrefix(state *instanceState) string {
	return notifyText(prefixLines(state))
}

// notifyText applies the sanitizing escapeForNotify of the notify path.
func notifyText(lines []string) string {
	escaped := make([]string, 0, len(lines))
	for _, line := range lines {
		escaped = append(escaped, escapeForNotify(line))
	}
	return strings.Join(escaped, "\n")
}

// requireContains fails the test when text does not hold want.
func requireContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected %q to contain %q", text, want)
	}
}

// requireNotContains fails the test when text holds want.
func requireNotContains(t *testing.T, text, want string) {
	t.Helper()
	if strings.Contains(text, want) {
		t.Fatalf("expected %q not to contain %q", text, want)
	}
}

// requireInt fails the test unless got equals want.
func requireInt(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", label, got, want)
	}
}

// requireFloat fails the test unless got equals want.
func requireFloat(t *testing.T, label string, got, want float64) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// requireOptionalInt fails the test unless the optional integer matches.
func requireOptionalInt(t *testing.T, label string, got *int, want *int) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got != nil && want != nil && *got == *want:
		return
	}
	t.Fatalf("%s = %s, want %s", label, formatOptionalInt(got), formatOptionalInt(want))
}

func formatOptionalInt(value *int) string {
	if value == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *value)
}

// requireOptionalString fails the test unless the optional string matches.
func requireOptionalString(t *testing.T, label string, got *string, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got != nil && want != nil && *got == *want:
		return
	}
	t.Fatalf("%s = %v, want %v", label, got, want)
}

// tpsMessage builds an assistant message with the given output/reasoning.
func tpsMessage(output float64, reasoning any) map[string]any {
	usage := usageMap("output", output)
	if reasoning != nil {
		usage["reasoning"] = reasoning
	}
	return map[string]any{"role": "assistant", "usage": usage}
}

// footerEntries installs a session-entry source into the state.
func footerEntries(state *instanceState, entries func() ([]any, bool)) {
	state.footerSources = &footerSources{entries: entries}
}

// footerUsage installs a context-usage source into the state.
func footerUsage(state *instanceState, usage func() (*sdk.ContextUsage, bool)) {
	if state.footerSources == nil {
		state.footerSources = &footerSources{}
	}
	state.footerSources.getContextUsage = usage
}

// usageOf builds a sdk.ContextUsage fixture.
func usageOf(tokens int, window int, percent float64) *sdk.ContextUsage {
	return &sdk.ContextUsage{Tokens: &tokens, ContextWindow: window, Percent: &percent}
}
