package autocontinue

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// sdkHost adapts sdk.Context to the state machine's Host interface. It is the
// only place that talks to the SDK, so the state machine and its tests stay free
// of sdk types.
type sdkHost struct {
	ctx sdk.Context
}

func (h sdkHost) Notify(message, level string) { h.ctx.Notify(message, level) }

func (h sdkHost) SetStatus(key, text string) { h.ctx.SetStatus(key, text) }

func (h sdkHost) ClearStatus(key string) { h.ctx.ClearStatus(key) }

func (h sdkHost) IsIdle() (bool, error) { return h.ctx.IsIdle() }

func (h sdkHost) HasPendingMessages() (bool, error) { return h.ctx.HasPendingMessages() }

func (h sdkHost) GetEditorText() (string, error) { return h.ctx.GetEditorText() }

// SendUserMessage mirrors pi.sendUserMessage(CONTINUE_PROMPT): an empty
// deliverAs is the wire equivalent of JS's `undefined` (the host decodes
// SendUserMessageOptions.DeliverAs with `json:"deliverAs,omitempty"`), so the
// message is neither steer nor followUp.
func (h sdkHost) SendUserMessage(content string) error {
	return h.ctx.SendUserMessage(content, "")
}

func (h sdkHost) OnTerminalInput(handler func(data string) TerminalInput) (func(), error) {
	return h.ctx.OnTerminalInput(func(data string) sdk.TerminalInputResult {
		result := handler(data)
		return sdk.TerminalInputResult{Consume: result.Consume, Data: result.Data}
	})
}

// messageMaps converts an event's JSON array of messages into the map slice the
// state machine scans. Members that are not JSON objects are dropped: the TS
// scans read `m?.role` / `m?.timestamp`, which is undefined for them.
func messageMaps(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	messages := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if message, ok := item.(map[string]any); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

// messageMap reads an event's single-message field.
func messageMap(value any) map[string]any {
	message, _ := value.(map[string]any)
	return message
}

// Extension builds the auto-continue extension for PiG.
//
// Hook-for-hook mapping onto the TS default export
// (auto-continue.ts:632-1155) is documented in README.md.
func Extension() *sdk.Extension {
	e := sdk.New(LogName)
	state := NewState(Options{})

	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.HandleSessionStart(sdkHost{ctx})
		return nil, nil
	})

	e.OnEvent(sdk.EventSessionShutdown, func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.HandleSessionShutdown(sdkHost{ctx})
		return nil, nil
	})

	// R3: no scheduling while a compaction runs. session_compact only fires on
	// a normal finish; agent_start and the watchdog unlock the flag otherwise.
	e.OnEvent(sdk.EventSessionBeforeCompact, func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.HandleSessionBeforeCompact(sdkHost{ctx})
		return nil, nil
	})

	e.OnEvent(sdk.EventSessionCompact, func(_ sdk.Context, _ map[string]any) (any, error) {
		state.HandleSessionCompact()
		return nil, nil
	})

	e.OnEvent(sdk.EventAgentStart, func(_ sdk.Context, _ map[string]any) (any, error) {
		state.HandleAgentStart()
		return nil, nil
	})

	e.OnEvent(sdk.EventAfterProviderResponse, func(_ sdk.Context, data map[string]any) (any, error) {
		status, hasStatus := data["status"]
		if hasStatus {
			state.HandleAfterProviderResponse(status)
		}
		return nil, nil
	})

	// Gate A's activity signal: a tool starts or finishes. codemode's nested
	// calls raise these events too, and they count.
	e.OnEvent(sdk.EventToolExecutionStart, func(_ sdk.Context, _ map[string]any) (any, error) {
		state.HandleToolExecutionStart()
		return nil, nil
	})

	e.OnEvent(sdk.EventToolExecutionEnd, func(_ sdk.Context, _ map[string]any) (any, error) {
		state.HandleToolExecutionEnd()
		return nil, nil
	})

	e.OnEvent(sdk.EventAgentEnd, func(_ sdk.Context, data map[string]any) (any, error) {
		state.HandleAgentEnd(messageMaps(data["messages"]))
		return nil, nil
	})

	e.OnEvent(sdk.EventInput, func(_ sdk.Context, data map[string]any) (any, error) {
		source, _ := data["source"].(string)
		text, _ := data["text"].(string)
		state.HandleInput(source, text)
		return nil, nil
	})

	e.OnEvent(sdk.EventBeforeAgentStart, func(_ sdk.Context, data map[string]any) (any, error) {
		prompt, hasPrompt := data["prompt"].(string)
		state.HandleBeforeAgentStart(prompt, hasPrompt)
		return nil, nil
	})

	e.OnEvent(sdk.EventTurnEnd, func(ctx sdk.Context, data map[string]any) (any, error) {
		state.HandleTurnEnd(sdkHost{ctx}, messageMap(data["message"]))
		return nil, nil
	})

	e.OnEvent(sdk.EventAgentSettled, func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.HandleAgentSettled(sdkHost{ctx})
		return nil, nil
	})

	e.Command("auto-continue", "瞬时错误 / 早衰 stop 后自动发「继续」。用法: /auto-continue [on|off|status]", func(ctx sdk.Context, args string) error {
		return state.HandleCommand(sdkHost{ctx}, args)
	})

	return e
}
