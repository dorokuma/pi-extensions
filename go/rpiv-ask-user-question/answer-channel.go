package rpivaskuserquestion

// Ports answer-channel.ts (v2.15.0): the inbound answer channel.
//
// One questionnaire may be resolved either by the dialog's own `done` callback
// or by an inbound `rpiv:ask-user:answer` event. Both paths go through settle,
// so the first answer wins and a later one is refused instead of reported as
// applied.

import (
	"sync"
)

// AnswerHost says who owns the prompts: "rpc" while the RPC dialog walker owns
// them — those cannot be answered from outside.
type AnswerHost string

// The two hosts.
const (
	AnswerHostTUI AnswerHost = "tui"
	AnswerHostRPC AnswerHost = "rpc"
)

// OpenQuestionnaire is one questionnaire an inbound answer may resolve. It is
// opened before the prompt event goes out and closed in execute's finally.
type OpenQuestionnaire struct {
	ToolCallID string
	Questions  []QuestionData
	// Host is mutated by execute() when the RPC walker takes over.
	Host AnswerHost

	mu      sync.Mutex
	result  *QuestionnaireResult
	deliver func(*QuestionnaireResult)
	// early carries an answer accepted while no dialog was mounted; the mount
	// races it so a host that mounts late closes the dialog on sight.
	early     chan *QuestionnaireResult
	earlyOnce sync.Once
}

// NewOpenQuestionnaire constructs an open questionnaire.
func NewOpenQuestionnaire(toolCallID string, questions []QuestionData, host AnswerHost) *OpenQuestionnaire {
	return &OpenQuestionnaire{
		ToolCallID: toolCallID,
		Questions:  questions,
		Host:       host,
		early:      make(chan *QuestionnaireResult, 1),
	}
}

// Settled returns the accepted result, or nil while still open.
func (o *OpenQuestionnaire) Settled() *QuestionnaireResult {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.result
}

// Attach hands over the mounted dialog's done callback; answers from now on
// close the dialog through it.
func (o *OpenQuestionnaire) Attach(done func(*QuestionnaireResult)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deliver = done
}

// Detach drops the mounted dialog's callback. The mount calls it on the way out so
// a later answer cannot reach a dialog that is already gone.
func (o *OpenQuestionnaire) Detach() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deliver = nil
}

// Settle applies the first answer; it returns false when this questionnaire is
// already settled (first answer wins).
func (o *OpenQuestionnaire) Settle(result *QuestionnaireResult) bool {
	o.mu.Lock()
	if o.result != nil {
		o.mu.Unlock()
		return false
	}
	o.result = result
	deliver := o.deliver
	o.mu.Unlock()

	if deliver != nil {
		deliver(result)
		return true
	}
	// No dialog yet: hand the answer to whoever mounts next.
	o.earlyOnce.Do(func() { o.early <- result })
	return true
}

// AnsweredEarly is the channel settle signals on when no dialog was mounted to
// take the answer. It is buffered so a settle before the mount never blocks.
func (o *OpenQuestionnaire) AnsweredEarly() <-chan *QuestionnaireResult { return o.early }

// SetHost records the owner of the prompts (execute switches to "rpc" when the
// TUI mount resolves undefined and the walker takes over).
func (o *OpenQuestionnaire) SetHost(host AnswerHost) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Host = host
}

// hostOf reads the host under the lock.
func (o *OpenQuestionnaire) hostOf() AnswerHost {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Host
}

// AnswerChannel is the registry of open questionnaires.
type AnswerChannel interface {
	Open(toolCallID string, questions []QuestionData, host AnswerHost) *OpenQuestionnaire
	Close(questionnaire *OpenQuestionnaire)
}

// answerChannel is the concrete registry bound to an event bus.
type answerChannel struct {
	mu   sync.Mutex
	open map[string]*OpenQuestionnaire
	emit func(channel string, payload any) error
}

// NewAnswerChannel builds a registry whose verdicts are published through emit.
func NewAnswerChannel(emit func(channel string, payload any) error) *answerChannel {
	return &answerChannel{open: make(map[string]*OpenQuestionnaire), emit: emit}
}

// Open registers a questionnaire under its tool-call id.
func (c *answerChannel) Open(toolCallID string, questions []QuestionData, host AnswerHost) *OpenQuestionnaire {
	questionnaire := NewOpenQuestionnaire(toolCallID, questions, host)
	c.mu.Lock()
	c.open[toolCallID] = questionnaire
	c.mu.Unlock()
	return questionnaire
}

// Close forgets a questionnaire, but only when the map still holds that exact
// value (a later Open under the same id must not be evicted by an older close).
func (c *answerChannel) Close(questionnaire *OpenQuestionnaire) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.open[questionnaire.ToolCallID]; ok && current == questionnaire {
		delete(c.open, questionnaire.ToolCallID)
	}
}

// Handle processes one inbound `rpiv:ask-user:answer` payload and publishes
// exactly one verdict. It mirrors the listener registered by
// registerAnswerChannel; the order of the guards is part of the contract.
func (c *answerChannel) Handle(data any) {
	payload, isObject := isRecord(data)
	if !isObject {
		payload = map[string]any{}
	}

	requestID, hasRequestID := payload["requestId"].(string)
	if _, present := payload["requestId"]; present && !hasRequestID {
		c.reply(nil, nil, RejectInvalidPayload, "requestId must be a string")
		return
	}
	toolCallID, hasToolCallID := payload["toolCallId"].(string)

	if !isObject {
		c.reply(optString(requestID, hasRequestID), optString(toolCallID, hasToolCallID),
			RejectInvalidPayload, "payload must be an object")
		return
	}
	if !hasToolCallID {
		c.reply(optString(requestID, hasRequestID), nil, RejectInvalidPayload, "toolCallId must be a string")
		return
	}

	c.mu.Lock()
	target := c.open[toolCallID]
	c.mu.Unlock()

	if target == nil || target.Settled() != nil {
		c.reply(optString(requestID, hasRequestID), strPtr(toolCallID), RejectNotFound,
			"no open questionnaire for tool call "+toolCallID)
		return
	}
	if target.hostOf() == AnswerHostRPC {
		c.reply(optString(requestID, hasRequestID), strPtr(toolCallID), RejectUnsupportedHost,
			"this host shows the questions in its own dialogs")
		return
	}

	outcome := ResolveExternalAnswer(target.Questions, payload)
	if !outcome.OK {
		c.reply(optString(requestID, hasRequestID), strPtr(toolCallID), outcome.Code, outcome.Reason)
		return
	}
	if !target.Settle(outcome.Result) {
		c.reply(optString(requestID, hasRequestID), strPtr(toolCallID), RejectNotFound,
			"tool call "+toolCallID+" is already answered")
		return
	}
	c.reply(optString(requestID, hasRequestID), strPtr(toolCallID), "", "")
}

// reply publishes one verdict. An empty code means ok.
func (c *answerChannel) reply(requestID, toolCallID *string, code AskUserAnswerRejectCode, reason string) {
	verdict := AnswerResultEventPayload{OK: code == "", RequestID: requestID, ToolCallID: toolCallID}
	if code != "" {
		verdict.Code = &code
		verdict.Reason = strPtr(reason)
	}
	if c.emit != nil {
		_ = c.emit(AskUserAnswerResultEvent, verdict)
	}
}

// optString returns a pointer to the value when present.
func optString(value string, present bool) *string {
	if !present {
		return nil
	}
	return &value
}

// RegisterAnswerChannel subscribes the channel to `rpiv:ask-user:answer` and
// returns the registry execute() opens questionnaires in. on subscribes a bus
// handler; emit publishes an event.
func RegisterAnswerChannel(
	on func(channel string, handler func(payload any) error) (func(), error),
	emit func(channel string, payload any) error,
) (AnswerChannel, error) {
	channel := NewAnswerChannel(emit)
	_, err := on(AskUserAnswerEvent, func(payload any) error {
		channel.Handle(payload)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return channel, nil
}
