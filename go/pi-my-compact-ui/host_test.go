package compactui

import "testing"

// withOwnThinkingRuns stands in for a 0.6.0 staged SDK Context that exposes
// OwnThinkingRuns (D214 / #384).
type withOwnThinkingRuns struct {
	calls []bool
}

func (w *withOwnThinkingRuns) OwnThinkingRuns(enabled bool) error {
	w.calls = append(w.calls, enabled)
	return nil
}

// withoutOwnThinkingRuns stands in for a 0.5.3 staged SDK Context that has no
// OwnThinkingRuns method. callOwnThinkingRuns must compile-time-ignore it and
// no-op at runtime.
type withoutOwnThinkingRuns struct{}

func TestCallOwnThinkingRunsWhenMethodPresent(t *testing.T) {
	w := &withOwnThinkingRuns{}
	callOwnThinkingRuns(w, true)
	callOwnThinkingRuns(w, false)
	if len(w.calls) != 2 || !w.calls[0] || w.calls[1] {
		t.Fatalf("OwnThinkingRuns calls = %v, want [true false]", w.calls)
	}
}

func TestCallOwnThinkingRunsWhenMethodAbsent(t *testing.T) {
	// Must not panic: soft probe finds no OwnThinkingRuns and skips the call.
	callOwnThinkingRuns(withoutOwnThinkingRuns{}, true)
	callOwnThinkingRuns(withoutOwnThinkingRuns{}, false)
	callOwnThinkingRuns(nil, true)
}
