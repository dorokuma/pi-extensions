package herdsmanpi

// Port of test/unit/herdsman-pi-turn-signal.test.ts (8 cases).
//
// The TS injects a `SessionWriteProbe` to avoid touching a real session file;
// the Go probe is the same interface. The TS probe's async `sleep` is a plain
// synchronous sleep here, with the poll interval kept tiny so the suite stays
// fast (the durations are test-only; the production defaults are untouched).

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type probeState struct {
	mu      sync.Mutex
	content string
	sleeps  []int
}

func newProbeState(initial string) *probeState {
	return &probeState{content: initial}
}

func (p *probeState) ReadTail(_ string, chars int) *string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.content) <= chars {
		value := p.content
		return &value
	}
	value := p.content[len(p.content)-chars:]
	return &value
}

func (p *probeState) Size(_ string) *int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	size := int64(len(p.content))
	return &size
}

func (p *probeState) Sleep(ms int) {
	p.mu.Lock()
	p.sleeps = append(p.sleeps, ms)
	p.mu.Unlock()
}

func (p *probeState) grow(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.content += text
}

func (p *probeState) sleepCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sleeps)
}

func intRef(value int) *int { return &value }

func TestConfirmSessionWriteConfirmsAlreadyWrittenText(t *testing.T) {
	state := newProbeState(`{"message":{"role":"assistant","content":"final answer"}}`)
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: "final answer", Path: "/x", Probe: state,
	})
	if check.Confirmed != true || check.Reason != ReasonAlreadyWritten {
		t.Fatalf("unexpected check: %+v", check)
	}
	if state.sleepCount() != 0 {
		t.Fatalf("expected no polls, got %d", state.sleepCount())
	}
}

func TestConfirmSessionWriteConfirmsOnceTextReachesFile(t *testing.T) {
	state := newProbeState("user turn only\n")
	// Grow the file from another goroutine while the check polls.
	go func() {
		sleepOrFailSleep(15)
		state.grow(`{"message":{"role":"assistant","content":"final answer"}}`)
	}()
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: "final answer",
		Path:         "/x",
		PollMS:       intRef(2),
		TimeoutMS:    intRef(1000),
		Probe:        state,
	})
	if check.Confirmed != true || check.Reason != ReasonAlreadyWritten {
		t.Fatalf("unexpected check: %+v", check)
	}
}

func sleepOrFailSleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func TestConfirmSessionWriteDoesNotConfirmGrowthAlone(t *testing.T) {
	state := newProbeState("user turn only\n")
	// A tool result line lands after `message_end` but the final assistant text
	// is still missing: growth alone used to confirm the write.
	go func() {
		sleepOrFailSleep(5)
		state.grow(`{"type":"tool_result","content":"unrelated output"}` + "\n")
	}()
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: "final answer",
		Path:         "/x",
		PollMS:       intRef(2),
		TimeoutMS:    intRef(40),
		Probe:        state,
	})
	if check.Confirmed != false || check.Reason != ReasonTimeout {
		t.Fatalf("unexpected check: %+v", check)
	}
	if state.sleepCount() <= 1 {
		t.Fatalf("expected repeated polling, got %d sleeps", state.sleepCount())
	}
}

func TestConfirmSessionWriteMatchesJSONLEscapedTail(t *testing.T) {
	state := newProbeState("")
	state.grow(`{"message":{"role":"assistant","content":[{"text":"line one\nline two","type":"text"}]}}`)
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: "line one\nline two",
		Path:         "/x",
		Probe:        state,
	})
	if check.Confirmed != true || check.Reason != ReasonAlreadyWritten {
		t.Fatalf("unexpected check: %+v", check)
	}
	if state.sleepCount() != 0 {
		t.Fatalf("expected no polls, got %d", state.sleepCount())
	}
}

func TestConfirmSessionWriteResolvesUnconfirmedOnTimeout(t *testing.T) {
	state := newProbeState("no growth ever\n")
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: "final answer",
		Path:         "/x",
		PollMS:       intRef(2),
		TimeoutMS:    intRef(40),
		Probe:        state,
	})
	if check.Confirmed != false || check.Reason != ReasonTimeout {
		t.Fatalf("unexpected check: %+v", check)
	}
	if state.sleepCount() == 0 {
		t.Fatal("expected at least one poll")
	}
}

// unavailableProbe reports a missing session file.
type unavailableProbe struct{ inner SessionWriteProbe }

func (u unavailableProbe) ReadTail(path string, chars int) *string { return nil }
func (u unavailableProbe) Size(string) *int64                      { return nil }
func (u unavailableProbe) Sleep(int)                               {}

func TestConfirmSessionWriteFailsFastWhenFileUnavailable(t *testing.T) {
	state := newProbeState("anything")
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: "final answer", Path: "/x", Probe: unavailableProbe{inner: state},
	})
	if check.Confirmed != false || check.Reason != ReasonUnavailable {
		t.Fatalf("unexpected check: %+v", check)
	}
	if state.sleepCount() != 0 {
		t.Fatalf("expected no polls, got %d", state.sleepCount())
	}
}

func TestConfirmSessionWriteNeverMatchesRewrittenTail(t *testing.T) {
	// `no-tables` rewrites the transcript *after* `message_end`: a `### X`
	// heading becomes `**X**` on disk. Text-level confirmation can never match;
	// this pins the timeout behaviour.
	expectedText := strings.Repeat("a", 250) + "### X"
	body := strings.Repeat("a", 250) + "**X**"
	encoded, err := json.Marshal(map[string]any{"message": map[string]any{"role": "assistant", "content": body}})
	if err != nil {
		t.Fatal(err)
	}
	state := newProbeState(string(encoded))
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: expectedText,
		Path:         "/x",
		PollMS:       intRef(2),
		TimeoutMS:    intRef(40),
		Probe:        state,
	})
	if check.Confirmed != false || check.Reason != ReasonTimeout {
		t.Fatalf("unexpected check: %+v", check)
	}
	if state.sleepCount() <= 1 {
		t.Fatalf("expected repeated polling, got %d sleeps", state.sleepCount())
	}
}

func TestConfirmSessionWriteSurvivesSplitSurrogatePairCut(t *testing.T) {
	// `expectedText.slice(-200)` is UTF-16 based: a cut can land between the
	// high and low surrogate of a 4-byte emoji. Only "never throws" is pinned;
	// the candidate keeps a lone surrogate, which still occurs verbatim in the
	// disk tail here, so either reason is acceptable as long as it agrees with
	// `confirmed` and no poll happened on a present match.
	expectedText := strings.Repeat("x", 10) + "\U0001F600" + strings.Repeat("y", 199)
	encoded, err := json.Marshal(map[string]any{"message": map[string]any{"role": "assistant", "content": expectedText}})
	if err != nil {
		t.Fatal(err)
	}
	state := newProbeState(string(encoded))
	check := ConfirmSessionWrite(ConfirmSessionWriteInput{
		ExpectedText: expectedText, Path: "/x", Probe: state,
	})
	if check.Reason != ReasonAlreadyWritten && check.Reason != ReasonTimeout {
		t.Fatalf("unexpected reason %s", check.Reason)
	}
	if check.Confirmed != (check.Reason == ReasonAlreadyWritten) {
		t.Fatalf("confirmed/reason disagree: %+v", check)
	}
	if check.Reason == ReasonAlreadyWritten && state.sleepCount() != 0 {
		t.Fatalf("expected no polls on the immediate hit, got %d", state.sleepCount())
	}
}
