package herdsman

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// Timing constants mirroring turn-signal.ts.
const (
	TurnSignalTimeoutMS    = 3000
	TurnSignalPollMS       = 50
	TurnSignalTailChars    = 8000
	turnSignalCandidateMax = 200
)

// TurnCompletionCheckReason mirrors the TS union.
type TurnCompletionCheckReason string

const (
	ReasonAlreadyWritten TurnCompletionCheckReason = "already_written"
	ReasonTimeout        TurnCompletionCheckReason = "timeout"
	ReasonUnavailable    TurnCompletionCheckReason = "unavailable"
)

// TurnCompletionCheck mirrors the TS `TurnCompletionCheck`.
type TurnCompletionCheck struct {
	Confirmed bool
	Reason    TurnCompletionCheckReason
}

// SessionWriteProbe mirrors the TS `SessionWriteProbe`: the injection point the
// tests use to avoid touching a real session file.
type SessionWriteProbe interface {
	ReadTail(path string, chars int) *string
	Size(path string) *int64
	Sleep(ms int)
}

// ConfirmSessionWriteInput mirrors the TS `confirmSessionWrite` argument.
type ConfirmSessionWriteInput struct {
	ExpectedText string
	Path         string
	// PollMS/TimeoutMS of nil take the package defaults.
	PollMS    *int
	TimeoutMS *int
	// Probe of nil uses the real filesystem.
	Probe SessionWriteProbe
	// Now is the clock; nil uses time.Now. The TS uses Date.now().
	Now func() time.Time
}

// defaultProbe is the TS `defaultProbe`.
type defaultProbe struct{}

func (defaultProbe) ReadTail(path string, chars int) *string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := info.Size()
	start := size - int64(chars)
	if start < 0 {
		start = 0
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = fh.Close() }()
	length := size - start
	if length < 0 {
		length = 0
	}
	// The TS allocates `Math.max(0, size - start)` bytes and reads into them;
	// a short read yields a shorter string.
	buffer := make([]byte, length)
	read, err := fh.ReadAt(buffer, start)
	if err != nil && err != io.EOF {
		return nil
	}
	text := string(buffer[:read])
	return &text
}

func (defaultProbe) Size(path string) *int64 {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := info.Size()
	return &size
}

func (defaultProbe) Sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// ConfirmSessionWrite mirrors the TS `confirmSessionWrite`.
//
// Bounded confirmation that the final assistant message reached the Pi session
// file before the extension signals the daemon. The message may already be on
// disk when the `message_end` hook fires; otherwise the file tail is polled
// until the expected text (or its JSONL-escaped form) appears.
//
// A timeout (or an unavailable file) still resolves so the caller can signal
// with the actual status instead of hanging forever, and it reports
// `confirmed: false` honestly instead of fabricating a confirmation.
func ConfirmSessionWrite(input ConfirmSessionWriteInput) TurnCompletionCheck {
	probe := input.Probe
	if probe == nil {
		probe = defaultProbe{}
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	timeoutMS := TurnSignalTimeoutMS
	if input.TimeoutMS != nil {
		timeoutMS = *input.TimeoutMS
	}
	pollMS := TurnSignalPollMS
	if input.PollMS != nil {
		pollMS = *input.PollMS
	}
	expectedText := strings.TrimSpace(input.ExpectedText)
	deadline := now().Add(time.Duration(timeoutMS) * time.Millisecond)
	candidate := expectedText
	// TS `expectedText.length` counts UTF-16 code units, not bytes.
	if utf16Len(expectedText) > turnSignalCandidateMax {
		candidate = tailUnits(expectedText, turnSignalCandidateMax)
	}
	// The session file is JSONL, so the written text is escaped (`\n`, `\"`,
	// `\\`). Matching the raw candidate only would miss every multi-line final
	// message, so the escaped form of the same suffix is accepted as well.
	escapedCandidate := escapeJSONInner(candidate)

	containsText := func(tail *string) bool {
		if tail == nil || candidate == "" {
			return false
		}
		return strings.Contains(*tail, candidate) || strings.Contains(*tail, escapedCandidate)
	}

	if probe.Size(input.Path) == nil {
		return TurnCompletionCheck{Confirmed: false, Reason: ReasonUnavailable}
	}
	if containsText(probe.ReadTail(input.Path, TurnSignalTailChars)) {
		return TurnCompletionCheck{Confirmed: true, Reason: ReasonAlreadyWritten}
	}
	for now().Before(deadline) {
		probe.Sleep(pollMS)
		if containsText(probe.ReadTail(input.Path, TurnSignalTailChars)) {
			return TurnCompletionCheck{Confirmed: true, Reason: ReasonAlreadyWritten}
		}
	}
	return TurnCompletionCheck{Confirmed: false, Reason: ReasonTimeout}
}

// tailUnits returns the last `count` UTF-16 code units of value, which is what
// `expectedText.slice(-200)` does in JS. Cutting by bytes would split a multi-byte
// character (the TS test that covers this is "does not throw when the 200-char
// cut splits a 4-byte emoji surrogate pair"), and cutting by runes would keep
// more than 200 units for astral text.
func tailUnits(value string, count int) string {
	units := 0
	for index := len(value); index > 0; {
		r, size := lastRune(value[:index])
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > count {
			return value[index:]
		}
		units += width
		index -= size
	}
	return value
}

// escapeJSONInner mirrors `JSON.stringify(candidate).slice(1, -1)`: the JSON
// string body without its surrounding quotes.
func escapeJSONInner(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	text := string(encoded)
	if len(text) >= 2 {
		return text[1 : len(text)-1]
	}
	return value
}
