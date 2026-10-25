package rpivaskuserquestion

import (
	"fmt"
	"sync"
	"testing"
)

// fakeBus is an in-process stand-in for the SDK event bus: subscribers are keyed
// by channel and emits fan out in subscription order.
type fakeBus struct {
	mu       sync.Mutex
	handlers map[string][]func(payload any) error
	emitted  []emittedEvent
}

type emittedEvent struct {
	Channel string
	Payload any
}

func newFakeBus() *fakeBus {
	return &fakeBus{handlers: map[string][]func(payload any) error{}}
}

func (b *fakeBus) On(channel string, handler func(payload any) error) (func(), error) {
	b.mu.Lock()
	b.handlers[channel] = append(b.handlers[channel], handler)
	b.mu.Unlock()
	return func() {}, nil
}

func (b *fakeBus) Emit(channel string, payload any) error {
	b.mu.Lock()
	b.emitted = append(b.emitted, emittedEvent{Channel: channel, Payload: payload})
	handlers := append([]func(payload any) error{}, b.handlers[channel]...)
	b.mu.Unlock()
	for _, handler := range handlers {
		if err := handler(payload); err != nil {
			return err
		}
	}
	return nil
}

// verdicts returns the result-event payloads in emission order.
func (b *fakeBus) verdicts() []AnswerResultEventPayload {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]AnswerResultEventPayload, 0, len(b.emitted))
	for _, event := range b.emitted {
		if event.Channel != AskUserAnswerResultEvent {
			continue
		}
		out = append(out, event.Payload.(AnswerResultEventPayload))
	}
	return out
}

func newTestChannel(t *testing.T) (*fakeBus, AnswerChannel) {
	t.Helper()
	bus := newFakeBus()
	channel, err := RegisterAnswerChannel(bus.On, bus.Emit)
	if err != nil {
		t.Fatalf("RegisterAnswerChannel: %v", err)
	}
	return bus, channel
}

func TestEventChannelNames(t *testing.T) {
	// Channel names are immutable: the stability policy forbids renaming.
	cases := map[string]string{
		AskUserPromptEvent:       "rpiv:ask-user:prompt",
		AskUserBlockedEvent:      "rpiv:ask-user:blocked",
		AskUserAnswerEvent:       "rpiv:ask-user:answer",
		AskUserAnswerResultEvent: "rpiv:ask-user:answer-result",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("channel = %q, want %q", got, want)
		}
	}
}

func TestAnswerChannelAcceptsAValidAnswer(t *testing.T) {
	bus, channel := newTestChannel(t)
	questions := []QuestionData{twoOptions("Auth", "Which?")}
	open := channel.Open("call-1", questions, AnswerHostTUI)

	if err := bus.Emit(AskUserAnswerEvent, map[string]any{
		"toolCallId": "call-1",
		"requestId":  "req-9",
		"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	verdicts := bus.verdicts()
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d", len(verdicts))
	}
	verdict := verdicts[0]
	if !verdict.OK || verdict.Code != nil {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.RequestID == nil || *verdict.RequestID != "req-9" {
		t.Fatalf("requestId = %v", verdict.RequestID)
	}
	if verdict.ToolCallID == nil || *verdict.ToolCallID != "call-1" {
		t.Fatalf("toolCallId = %v", verdict.ToolCallID)
	}
	settled := open.Settled()
	if settled == nil || settled.Cancelled || len(settled.Answers) != 1 {
		t.Fatalf("settled = %#v", settled)
	}
}

func TestAnswerChannelRejectsByCode(t *testing.T) {
	questions := []QuestionData{twoOptions("Auth", "Which?")}

	cases := []struct {
		name     string
		payload  any
		code     AskUserAnswerRejectCode
		reason   string
		prepare  func(AnswerChannel) *OpenQuestionnaire
		answered bool
	}{
		{
			name:    "payload is not an object",
			payload: "plain string",
			code:    RejectInvalidPayload,
			reason:  "payload must be an object",
		},
		{
			name:    "requestId is not a string",
			payload: map[string]any{"toolCallId": "call-1", "requestId": 12},
			code:    RejectInvalidPayload,
			reason:  "requestId must be a string",
		},
		{
			name:    "toolCallId is missing",
			payload: map[string]any{"requestId": "r"},
			code:    RejectInvalidPayload,
			reason:  "toolCallId must be a string",
		},
		{
			name:    "toolCallId is not a string",
			payload: map[string]any{"toolCallId": 7},
			code:    RejectInvalidPayload,
			reason:  "toolCallId must be a string",
		},
		{
			name:    "unknown tool call",
			payload: map[string]any{"toolCallId": "nope"},
			code:    RejectNotFound,
			reason:  "no open questionnaire for tool call nope",
		},
		{
			name:    "rpc host refuses external answers",
			payload: map[string]any{"toolCallId": "rpc-1", "answers": []any{}},
			code:    RejectUnsupportedHost,
			reason:  "this host shows the questions in its own dialogs",
			prepare: func(channel AnswerChannel) *OpenQuestionnaire {
				return channel.Open("rpc-1", questions, AnswerHostRPC)
			},
		},
		{
			name: "invalid answer is refused",
			payload: map[string]any{
				"toolCallId": "call-1",
				"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{9}}},
			},
			code:   RejectInvalidAnswer,
			reason: "question 0: option index 9 out of range (0..1)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus, channel := newTestChannel(t)
			if tc.prepare != nil {
				tc.prepare(channel)
			} else {
				channel.Open("call-1", questions, AnswerHostTUI)
			}
			if err := bus.Emit(AskUserAnswerEvent, tc.payload); err != nil {
				t.Fatalf("emit: %v", err)
			}
			verdicts := bus.verdicts()
			if len(verdicts) != 1 {
				t.Fatalf("verdicts = %d (%+v)", len(verdicts), verdicts)
			}
			if verdicts[0].OK {
				t.Fatal("expected a rejection")
			}
			if verdicts[0].Code == nil || *verdicts[0].Code != tc.code {
				t.Fatalf("code = %v, want %q", verdicts[0].Code, tc.code)
			}
			if verdicts[0].Reason == nil || *verdicts[0].Reason != tc.reason {
				t.Fatalf("reason = %v, want %q", verdicts[0].Reason, tc.reason)
			}
		})
	}
}

// First answer wins: a second inbound answer is refused rather than reported as
// applied.
func TestAnswerChannelFirstAnswerWins(t *testing.T) {
	bus, channel := newTestChannel(t)
	questions := []QuestionData{twoOptions("Auth", "Which?")}
	channel.Open("call-1", questions, AnswerHostTUI)

	first := map[string]any{
		"toolCallId": "call-1",
		"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
	}
	second := map[string]any{
		"toolCallId": "call-1",
		"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{1}}},
	}
	_ = bus.Emit(AskUserAnswerEvent, first)
	_ = bus.Emit(AskUserAnswerEvent, second)

	verdicts := bus.verdicts()
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %d", len(verdicts))
	}
	if !verdicts[0].OK {
		t.Fatalf("first verdict = %+v", verdicts[0])
	}
	if verdicts[1].OK || verdicts[1].Code == nil || *verdicts[1].Code != RejectNotFound {
		t.Fatalf("second verdict = %+v", verdicts[1])
	}
	if verdicts[1].Reason == nil || *verdicts[1].Reason != "no open questionnaire for tool call call-1" {
		t.Fatalf("second reason = %v", verdicts[1].Reason)
	}
}

// A dialog answer and an inbound answer race through the same settle gate.
func TestOpenQuestionnaireSettleIsFirstWins(t *testing.T) {
	open := NewOpenQuestionnaire("call-1", []QuestionData{twoOptions("H", "Q?")}, AnswerHostTUI)

	delivered := make([]*QuestionnaireResult, 0)
	var mu sync.Mutex
	open.Attach(func(result *QuestionnaireResult) {
		mu.Lock()
		delivered = append(delivered, result)
		mu.Unlock()
	})

	first := &QuestionnaireResult{Answers: []QuestionAnswer{answer(AnswerKindOption, "A")}}
	second := &QuestionnaireResult{Answers: []QuestionAnswer{answer(AnswerKindOption, "B")}}

	if !open.Settle(first) {
		t.Fatal("first settle refused")
	}
	if open.Settle(second) {
		t.Fatal("second settle accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 || delivered[0] != first {
		t.Fatalf("delivered = %#v", delivered)
	}
	if open.Settled() != first {
		t.Fatal("settled value is not the first answer")
	}
}

// An answer accepted before any dialog is mounted goes to AnsweredEarly.
func TestOpenQuestionnaireAnsweredEarly(t *testing.T) {
	open := NewOpenQuestionnaire("call-1", []QuestionData{twoOptions("H", "Q?")}, AnswerHostTUI)
	result := &QuestionnaireResult{Answers: []QuestionAnswer{answer(AnswerKindOption, "A")}}

	if !open.Settle(result) {
		t.Fatal("settle refused")
	}
	select {
	case got := <-open.AnsweredEarly():
		if got != result {
			t.Fatalf("early answer = %#v", got)
		}
	default:
		t.Fatal("no early answer delivered")
	}
}

// Detach stops delivery to a dialog that already went away.
func TestOpenQuestionnaireDetach(t *testing.T) {
	open := NewOpenQuestionnaire("call-1", nil, AnswerHostTUI)
	delivered := 0
	open.Attach(func(*QuestionnaireResult) { delivered++ })
	open.Detach()
	open.Settle(&QuestionnaireResult{Cancelled: true})
	if delivered != 0 {
		t.Fatalf("delivered after detach = %d", delivered)
	}
	// The result is still recorded for the early path.
	if open.Settled() == nil {
		t.Fatal("settled result lost after detach")
	}
}

// Close only forgets the exact questionnaire registered under an id, so a later
// Open under the same id survives an older Close.
func TestAnswerChannelCloseOnlyEvictsItsOwnEntry(t *testing.T) {
	channel := NewAnswerChannel(nil)
	first := channel.Open("call-1", nil, AnswerHostTUI)
	second := channel.Open("call-1", nil, AnswerHostTUI)

	channel.Close(first)

	channel.mu.Lock()
	current := channel.open["call-1"]
	channel.mu.Unlock()
	if current != second {
		t.Fatalf("close evicted the wrong questionnaire")
	}
	channel.Close(second)
	channel.mu.Lock()
	_, present := channel.open["call-1"]
	channel.mu.Unlock()
	if present {
		t.Fatal("closed questionnaire still registered")
	}
}

// Concurrent inbound answers and a concurrent dialog settle must not double
// deliver, and the race detector must stay quiet.
func TestAnswerChannelConcurrentAnswers(t *testing.T) {
	bus, channel := newTestChannel(t)
	questions := []QuestionData{twoOptions("Auth", "Which?")}
	open := channel.Open("call-1", questions, AnswerHostTUI)

	var settlements int
	var mu sync.Mutex
	open.Attach(func(*QuestionnaireResult) {
		mu.Lock()
		settlements++
		mu.Unlock()
	})

	const writers = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(index int) {
			defer wg.Done()
			_ = bus.Emit(AskUserAnswerEvent, map[string]any{
				"toolCallId": "call-1",
				"answers": []any{map[string]any{
					"questionIndex": 0,
					"optionIndexes": []any{index % 2},
				}},
			})
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if settlements != 1 {
		t.Fatalf("settlements = %d, want exactly 1", settlements)
	}
	accepted := 0
	for _, verdict := range bus.verdicts() {
		if verdict.OK {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted verdicts = %d, want exactly 1", accepted)
	}
}

func TestAnswerChannelTwoConcurrentQuestionnaires(t *testing.T) {
	bus, channel := newTestChannel(t)
	questions := []QuestionData{twoOptions("Auth", "Which?")}
	openA := channel.Open("call-a", questions, AnswerHostTUI)
	openB := channel.Open("call-b", questions, AnswerHostTUI)

	_ = bus.Emit(AskUserAnswerEvent, map[string]any{
		"toolCallId": "call-a",
		"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
	})
	// Answering A must not settle B.
	if openA.Settled() == nil {
		t.Fatal("A not settled")
	}
	if openB.Settled() != nil {
		t.Fatal("B settled by A's answer")
	}
}

func TestAnswerChannelSwitchesToUnsupportedHost(t *testing.T) {
	bus, channel := newTestChannel(t)
	questions := []QuestionData{twoOptions("Auth", "Which?")}
	open := channel.Open("call-1", questions, AnswerHostTUI)
	// execute() flips the host to rpc when the walker takes over.
	open.SetHost(AnswerHostRPC)

	_ = bus.Emit(AskUserAnswerEvent, map[string]any{
		"toolCallId": "call-1",
		"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
	})
	verdicts := bus.verdicts()
	if len(verdicts) != 1 || verdicts[0].Code == nil || *verdicts[0].Code != RejectUnsupportedHost {
		t.Fatalf("verdicts = %+v", verdicts)
	}
}

// The verdict carries no requestId when the request had none, and carries the
// requestId on a rejection too.
func TestAnswerChannelVerdictEchoesRequestID(t *testing.T) {
	bus, channel := newTestChannel(t)
	channel.Open("call-1", []QuestionData{twoOptions("H", "Q?")}, AnswerHostTUI)

	_ = bus.Emit(AskUserAnswerEvent, map[string]any{"toolCallId": "call-1", "answers": []any{}})
	verdicts := bus.verdicts()
	if verdicts[0].RequestID != nil {
		t.Fatalf("requestId = %v", verdicts[0].RequestID)
	}

	_ = bus.Emit(AskUserAnswerEvent, map[string]any{"toolCallId": "call-1", "requestId": "r-1", "answers": []any{}})
	verdicts = bus.verdicts()
	if verdicts[1].RequestID == nil || *verdicts[1].RequestID != "r-1" {
		t.Fatalf("requestId = %v", verdicts[1].RequestID)
	}
}

// A payload that is a JSON array is not an object.
func TestAnswerChannelRejectsArrayPayload(t *testing.T) {
	bus, channel := newTestChannel(t)
	channel.Open("call-1", []QuestionData{twoOptions("H", "Q?")}, AnswerHostTUI)
	if err := bus.Emit(AskUserAnswerEvent, []any{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	verdicts := bus.verdicts()
	if len(verdicts) != 1 || verdicts[0].OK {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if *verdicts[0].Code != RejectInvalidPayload {
		t.Fatalf("code = %v", *verdicts[0].Code)
	}
}

func TestAnswerChannelConcurrentOpenClose(t *testing.T) {
	channel := NewAnswerChannel(nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			open := channel.Open(fmt.Sprintf("call-%d", index), nil, AnswerHostTUI)
			channel.Close(open)
		}(i)
	}
	wg.Wait()
}
