package autocontinue

import (
	"os"
	"testing"
)

// TestExtensionIdentity pins the registered name, which is also the notify/status
// tag (TS LOG / STATUS_KEY, auto-continue.ts:98-99).
func TestExtensionIdentity(t *testing.T) {
	// NewState resolves and loads the preference file while the factory builds;
	// PIG_HOME keeps that read inside a temporary home instead of ~/.pi.
	t.Setenv("PIG_HOME", t.TempDir())
	extension := Extension()
	if extension == nil {
		t.Fatal("Extension() returned nil")
	}
	if got := extension.Name(); got != "auto-continue" {
		t.Fatalf("Name() = %q, want %q", got, LogName)
	}
}

// TestExtensionFactoryIsolated makes sure building the extension does not touch
// the production preference file: PIG_HOME redirects it and the file is created
// inside that temporary home.
func TestExtensionFactoryIsolated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIG_HOME", home)

	if extension := Extension(); extension == nil {
		t.Fatal("Extension() returned nil")
	}
	// The factory loads the persisted preferences, which writes the defaults on
	// first sight. That file must live under PIG_HOME, never under ~/.pi.
	if _, err := os.Stat(home + "/agent/auto-continue.json"); err != nil {
		t.Fatalf("expected the isolated preference file: %v", err)
	}
}

// TestMessageMaps covers the event-payload conversion: non-object members are
// dropped, as TS's `m?.role` reads undefined for them.
func TestMessageMaps(t *testing.T) {
	input := []any{
		map[string]any{"role": "user"},
		"junk",
		nil,
		map[string]any{"role": "assistant"},
	}
	got := messageMaps(input)
	if len(got) != 2 {
		t.Fatalf("messageMaps kept %d entries, want 2", len(got))
	}
	if messageMaps("not a list") != nil {
		t.Fatal("a non-array payload must yield no messages")
	}
	if messageMaps(nil) != nil {
		t.Fatal("a nil payload must yield no messages")
	}
	if got := messageMap(map[string]any{"role": "assistant"}); roleOf(got) != "assistant" {
		t.Fatalf("messageMap lost the message: %+v", got)
	}
	if got := messageMap("nope"); got != nil {
		t.Fatalf("messageMap(non-object) = %+v, want nil", got)
	}
}

// TestTerminalInputVerdictIsNeverConsumed pins the "never consume" contract of
// handleTerminalInput (auto-continue.ts:804-806): pi's own ESC handling must
// keep working.
func TestTerminalInputVerdictIsNeverConsumed(t *testing.T) {
	h := newHarness(t, nil)
	h.state.HandleSessionStart(h.host)
	h.host.idle = false
	if got := h.state.HandleTerminalInput(BareEsc); got.Consume || got.Data != nil {
		t.Fatalf("verdict = %+v, want the zero value", got)
	}
}
