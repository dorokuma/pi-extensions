package compactui

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// This file maps the host's event payloads (Pi's AgentSession events, as PiG
// delivers them in a map[string]any) onto the typed shapes the ported logic
// reads. PT receives real TypeScript objects; the port decodes the same JSON
// once, at the event boundary, so the rest of the port walks structs.

// contentItem is one block of an assistant message: a text block, a thinking
// block or a tool call.
type contentItem struct {
	typ      string // "text" | "thinking" | "toolCall"
	text     string
	thinking string
}

// assistantMessage is the assistant message an event carries.
type assistantMessage struct {
	role         string
	content      []contentItem
	usage        *usage
	stopReason   string
	errorMessage string
}

// usage is the message usage PT reads (`usage.reasoning`).
type usage struct {
	reasoning *int
}

// streamBlock is one tool-call block of a streaming message.
type streamBlock struct {
	id          string
	name        string
	typ         string
	arguments   map[string]any
	partialJSON string
	partialArgs string
	// text and thinking carry the block's own text for the text_ and
	// thinking_ branches of message_update (L2411-2460).
	text     string
	thinking string
}

// streamEvent is the assistantMessageEvent PT switches on.
type streamEvent struct {
	typ          string
	contentIndex *int
	content      string
	toolCall     *streamBlock
}

// contentBlock is one entry of the message content array, keeping both the
// discriminator and the streaming shape.
type contentBlock struct {
	typ   string
	block streamBlock
}

// decodeAssistantMessage decodes a host message map. ok is false when the
// value is not an object.
func decodeAssistantMessage(value any) (*assistantMessage, bool) {
	raw, ok := asObject(value)
	if !ok {
		return nil, false
	}
	message := &assistantMessage{role: asString(raw["role"])}
	if content, isList := asList(raw["content"]); isList {
		message.content = decodeContent(content)
	}
	if rawUsage, present := raw["usage"]; present {
		if usageObject, isObject := asObject(rawUsage); isObject {
			message.usage = &usage{reasoning: intOrNil(usageObject["reasoning"])}
		}
	}
	message.stopReason = asString(raw["stopReason"])
	message.errorMessage = asString(raw["errorMessage"])
	return message, true
}

// decodeContent decodes a content block list.
func decodeContent(list []any) []contentItem {
	out := make([]contentItem, 0, len(list))
	for _, entry := range list {
		block, ok := asObject(entry)
		if !ok {
			continue
		}
		item := contentItem{typ: asString(block["type"])}
		switch item.typ {
		case "thinking":
			item.thinking = asString(block["thinking"])
		case "text":
			item.text = asString(block["text"])
		}
		out = append(out, item)
	}
	return out
}

// decodeContentBlocks decodes the streaming message content, which is what
// locateStreamingToolCall indexes into.
func decodeContentBlocks(list []any) []contentBlock {
	out := make([]contentBlock, 0, len(list))
	for _, entry := range list {
		block, ok := asObject(entry)
		if !ok {
			out = append(out, contentBlock{})
			continue
		}
		item := contentBlock{typ: asString(block["type"])}
		item.block = streamBlock{
			id:          asString(block["id"]),
			name:        asString(block["name"]),
			typ:         item.typ,
			arguments:   objectOrNil(block["arguments"]),
			partialJSON: asString(block["partialJson"]),
			partialArgs: asString(block["partialArgs"]),
			text:        asString(block["text"]),
			thinking:    asString(block["thinking"]),
		}
		out = append(out, item)
	}
	return out
}

// decodeStreamEvent decodes event.assistantMessageEvent.
func decodeStreamEvent(value any) streamEvent {
	raw, ok := asObject(value)
	if !ok {
		return streamEvent{}
	}
	event := streamEvent{
		typ:          asString(raw["type"]),
		contentIndex: intOrNilInteger(raw["contentIndex"]),
		content:      asString(raw["content"]),
	}
	if toolCall, isObject := asObject(raw["toolCall"]); isObject {
		event.toolCall = &streamBlock{
			id:          asString(toolCall["id"]),
			name:        asString(toolCall["name"]),
			typ:         asString(toolCall["type"]),
			arguments:   objectOrNil(toolCall["arguments"]),
			partialJSON: asString(toolCall["partialJson"]),
			partialArgs: asString(toolCall["partialArgs"]),
		}
	}
	return event
}

// ── JSON value helpers ──────────────────────────────────────────────────────

// asObject converts a decoded JSON value to an object.
func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// objectOrNil converts to an object or nil (the TS `?? {}` default).
func objectOrNil(value any) map[string]any {
	if object, ok := asObject(value); ok {
		return object
	}
	return nil
}

// asList converts a decoded JSON value to a list.
func asList(value any) ([]any, bool) {
	list, ok := value.([]any)
	return list, ok
}

// asString converts a decoded JSON value to a string.
func asString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

// toNumber converts a decoded JSON number to float64.
func toNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// intOrNil converts a decoded JSON value to *int, realising Number(...) plus
// Number.isFinite. It truncates a fractional value, as JS `Number(x)` handed to
// a place that wants an integer does.
func intOrNil(value any) *int {
	number, ok := toNumber(value)
	if !ok {
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	rounded := int(number)
	return &rounded
}

// intOrNilInteger converts a decoded JSON value to *int only when it is an
// integer, realising Number.isInteger(contentIndex) (TS L2367, L2372): 0.5,
// NaN and Infinity all yield nil, which the callers read as "no index".
func intOrNilInteger(value any) *int {
	number, ok := toNumber(value)
	if !ok {
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	if math.Trunc(number) != number {
		return nil
	}
	rounded := int(number)
	return &rounded
}

// decodeJSONObject decodes a JSON document into an object.
func decodeJSONObject(raw []byte) (map[string]any, bool) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	return asObject(value)
}

// jsNumberString renders a number as JavaScript's String(number) does for the
// integral and short-fraction values tool arguments carry.
func jsNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "Infinity"
	}
	if math.IsInf(value, -1) {
		return "-Infinity"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// parseTimestampMillis parses an ISO-8601 timestamp into epoch milliseconds
// (new Date(value).getTime()), returning 0 for an unparsable value.
func parseTimestampMillis(text string) int64 {
	if text == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z0700"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

// wallClock is the production clock.
func wallClock() int64 { return time.Now().UnixMilli() }

// itoa is strconv.Itoa under the local name the ported code reads better with.
func itoa(value int) string { return strconv.Itoa(value) }
