package herdsman

// decodeStreamMessage mirrors the TS `DaemonStreamMessage` narrowing in
// `client.onStreamMessage = handler`: the params object is read per method, and
// a malformed params object yields a message with no payload rather than a
// panic.
func decodeStreamMessage(record map[string]any) DaemonStreamMessage {
	method, _ := record["method"].(string)
	params := asRecord(record["params"])
	message := DaemonStreamMessage{Method: method}
	switch method {
	case "agent.event":
		if raw, ok := params["event"].(map[string]any); ok {
			event := decodeAgentEvent(raw)
			message.Event = &event
		}
	case "agent.context.changed":
		message.HerdrSession, _ = params["herdrSessionName"].(string)
		message.WorkspaceID, _ = params["workspaceId"].(string)
		if raw, ok := params["context"].(map[string]any); ok {
			context := decodeContextSnapshot(raw)
			message.Context = &context
		}
	case "agent.orchestrator.changed":
		if raw, ok := params["change"].(map[string]any); ok {
			change := decodeOrchestratorChanged(raw)
			message.Change = &change
		}
	}
	return message
}

// decodeAgentEvent reads one wire record out of a decoded JSON object. The
// stream path is the one place where a hand-built map has to become the typed
// record, so the optional fields are read with presence checks.
func decodeAgentEvent(raw map[string]any) AgentEventWireRecord {
	event := AgentEventWireRecord{
		AgentID:          optionalString(raw, "agentId"),
		Attempts:         optionalInt(raw, "attempts"),
		LastAttemptAt:    optionalInt64(raw, "lastAttemptAt"),
		NextAttemptAt:    optionalInt64(raw, "nextAttemptAt"),
		LastFailureCode:  optionalString(raw, "lastFailureCode"),
		CreatedAt:        optionalString(raw, "createdAt"),
		HerdrSessionName: optionalString(raw, "herdrSessionName"),
		PaneGeneration:   optionalString(raw, "paneGeneration"),
		PaneID:           optionalString(raw, "paneId"),
		Payload:          raw["payload"],
		Status:           optionalString(raw, "status"),
		TerminalID:       optionalString(raw, "terminalId"),
		WorkspaceID:      optionalString(raw, "workspaceId"),
	}
	event.Type, _ = raw["type"].(string)
	if id, ok := raw["id"].(float64); ok {
		event.ID = int(id)
	}
	if history, ok := raw["compactHistory"].(map[string]any); ok {
		decoded := decodeCompactHistory(history)
		event.CompactHistory = &decoded
	}
	return event
}

// decodeCompactHistory reads the compactHistory object.
func decodeCompactHistory(raw map[string]any) CompactAgentHistory {
	return CompactAgentHistory{
		LastAssistantMessage: decodeCompactText(raw["lastAssistantMessage"]),
		LastUserMessage:      decodeCompactText(raw["lastUserMessage"]),
		LastToolResult:       decodeCompactTool(raw["lastToolResult"]),
		UpdatedAt:            optionalString(raw, "updatedAt"),
	}
}

// decodeCompactText reads `{ text?: string | null }`; a null is "present but
// null", which the TS treats like an absent message body.
func decodeCompactText(value any) *CompactText {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &CompactText{Text: optionalString(raw, "text")}
}

// decodeCompactTool reads `{ text?, toolName? }`.
func decodeCompactTool(value any) *CompactTool {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &CompactTool{Text: optionalString(raw, "text"), ToolName: optionalString(raw, "toolName")}
}

// decodeContextSnapshot reads an `agent.context.changed` snapshot.
func decodeContextSnapshot(raw map[string]any) AgentWorkspaceContextSnapshot {
	snapshot := AgentWorkspaceContextSnapshot{}
	snapshot.HerdrSessionName, _ = raw["herdrSessionName"].(string)
	snapshot.UpdatedAt, _ = raw["updatedAt"].(string)
	snapshot.WorkspaceID, _ = raw["workspaceId"].(string)
	if agents, ok := raw["agents"].([]any); ok {
		snapshot.Agents = make([]AgentContextListItem, 0, len(agents))
		for _, item := range agents {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			listItem := AgentContextListItem{
				Agent:       optionalString(entry, "agent"),
				AgentStatus: optionalString(entry, "agentStatus"),
				ID:          optionalString(entry, "id"),
				Name:        optionalString(entry, "name"),
				PaneID:      optionalString(entry, "paneId"),
				TerminalID:  optionalString(entry, "terminalId"),
			}
			if history, ok := entry["history"].(map[string]any); ok {
				decoded := decodeCompactHistory(history)
				listItem.History = &decoded
			}
			snapshot.Agents = append(snapshot.Agents, listItem)
		}
	}
	return snapshot
}

// decodeOrchestratorChanged reads an `agent.orchestrator.changed` change.
func decodeOrchestratorChanged(raw map[string]any) AgentOrchestratorChanged {
	change := AgentOrchestratorChanged{}
	change.Reason, _ = raw["reason"].(string)
	if current, ok := raw["current"].(map[string]any); ok {
		change.Current = decodeWireState(current)
	}
	if previous, ok := raw["previous"].(map[string]any); ok {
		change.Previous = decodeWireState(previous)
	}
	return change
}

// decodeWireState reads one `AgentOrchestratorWireState`.
func decodeWireState(raw map[string]any) AgentOrchestratorWireState {
	state := AgentOrchestratorWireState{}
	state.HerdrSessionName, _ = raw["herdrSessionName"].(string)
	state.UpdatedAt, _ = raw["updatedAt"].(string)
	state.WorkspaceID, _ = raw["workspaceId"].(string)
	// The socket path always decodes JSON numbers to float64, but an in-process
	// caller may hand an int; accepting both keeps the reader honest instead of
	// silently reading a watermark of 0.
	if acked := optionalInt(raw, "ackedEventId"); acked != nil {
		state.AckedEventID = *acked
	}
	if owner, ok := raw["owner"].(map[string]any); ok {
		decoded := AgentOrchestratorOwner{}
		decoded.PaneID, _ = owner["paneId"].(string)
		decoded.TerminalID, _ = owner["terminalId"].(string)
		state.Owner = &decoded
	}
	return state
}

// decodeConnectionStateResponse reads the `agent.orchestrator.get/register/set`
// result. A nil result yields nil so callers keep the TS `undefined` branch.
func decodeConnectionStateResponse(result any) *ConnectionStateResponse {
	raw, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	response := ConnectionStateResponse{}
	if changed, ok := raw["changed"].(bool); ok {
		response.Changed = &changed
	}
	response.AckedEventID = optionalInt(raw, "ackedEventId")
	if context, ok := raw["context"].(map[string]any); ok {
		decoded := decodeContextSnapshot(context)
		response.Context = &decoded
	}
	if events, ok := raw["events"].([]any); ok {
		response.Events = make([]AgentEventWireRecord, 0, len(events))
		for _, item := range events {
			event, ok := item.(map[string]any)
			if !ok {
				continue
			}
			response.Events = append(response.Events, decodeAgentEvent(event))
		}
	} else if typed, ok := raw["events"].([]AgentEventWireRecord); ok {
		// The in-process host path (and the tests) hand the typed slice over
		// directly; the TS sees plain objects there too.
		response.Events = append([]AgentEventWireRecord{}, typed...)
	}
	if presence, ok := raw["presence"].(map[string]any); ok {
		response.Presence = decodePresence(presence)
	}
	if state, ok := raw["state"].(map[string]any); ok {
		decoded := decodeWireState(state)
		response.State = &decoded
	}
	return &response
}

// decodePresence reads the `presence` object.
func decodePresence(raw map[string]any) PiPresence {
	presence := PiPresence{}
	presence.HerdrSessionName, _ = raw["herdrSessionName"].(string)
	presence.PaneID, _ = raw["paneId"].(string)
	presence.SubscriberID, _ = raw["subscriberId"].(string)
	presence.TerminalID, _ = raw["terminalId"].(string)
	presence.WorkspaceID, _ = raw["workspaceId"].(string)
	if connectedAt, ok := raw["connectedAt"].(float64); ok {
		presence.ConnectedAt = int64(connectedAt)
	}
	return presence
}

// optionalString reads an optional string field; a non-string is treated as
// absent, the same way the TS optional chain yields undefined.
func optionalString(raw map[string]any, key string) *string {
	value, ok := raw[key].(string)
	if !ok {
		return nil
	}
	return &value
}

// optionalInt reads an optional integer field.
func optionalInt(raw map[string]any, key string) *int {
	switch value := raw[key].(type) {
	case float64:
		converted := int(value)
		return &converted
	case int:
		converted := value
		return &converted
	default:
		return nil
	}
}

// optionalInt64 reads an optional millisecond timestamp.
func optionalInt64(raw map[string]any, key string) *int64 {
	switch value := raw[key].(type) {
	case float64:
		converted := int64(value)
		return &converted
	case int64:
		converted := value
		return &converted
	default:
		return nil
	}
}
