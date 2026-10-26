package pivcc

import "fmt"

// Ports src/core/normalize.ts: pi messages -> NormalizedBlocks.
//
// A message is the flat role-discriminated JSON object the wire carries, so the
// port reads it as a map exactly as the TS original reads its `message` object.

// normalizeOne is normalize.ts normalizeOne(). msgIndex/hasIndex are the entry's
// session-global index; hasIndex false is TS's `undefined`, which renders as no
// ref.
func normalizeOne(msg map[string]any, msgIndex int, hasIndex bool) []NormalizedBlock {
	role, _ := msg["role"].(string)

	switch role {
	case "user":
		blocks := make([]NormalizedBlock, 0, 2)
		text := Sanitize(TextOf(msg["content"]))
		// normalize.ts: `injected === true` marks an extension custom_message
		// that convertToLlm turned into a user message (hooks/before-compact.ts).
		injected, _ := msg["injected"].(bool)
		if text != "" {
			blocks = append(blocks, userBlockInjected(text, msgIndex, hasIndex, injected))
		}
		if content, ok := msg["content"].([]any); ok {
			for _, rawPart := range content {
				part, isObject := rawPart.(map[string]any)
				if !isObject {
					continue
				}
				if kind, _ := part["type"].(string); kind == "image" {
					mimeType, _ := part["mimeType"].(string)
					blocks = append(blocks, userBlock("[image: "+mimeType+"]", msgIndex, hasIndex))
				}
			}
		}
		if len(blocks) > 0 {
			return blocks
		}
		return []NormalizedBlock{userBlock("", msgIndex, hasIndex)}

	case "bashExecution":
		command, _ := msg["command"].(string)
		output, _ := msg["output"].(string)
		block := NormalizedBlock{
			Kind:        KindBash,
			Command:     command,
			Output:      output,
			SourceIndex: msgIndex,
			HasSource:   hasIndex,
		}
		// TS keeps `exitCode: number | undefined`; a JSON null decodes to nil
		// and is `undefined` there.
		if exit, ok := msg["exitCode"].(float64); ok {
			block.ExitCode = int(exit)
			block.HasExit = true
		} else if exit, ok := msg["exitCode"].(int); ok {
			block.ExitCode = exit
			block.HasExit = true
		}
		return []NormalizedBlock{block}

	case "toolResult":
		toolName, _ := msg["toolName"].(string)
		return []NormalizedBlock{{
			Kind:        KindToolResult,
			Name:        toolName,
			Text:        Sanitize(TextOf(msg["content"])),
			SourceIndex: msgIndex,
			HasSource:   hasIndex,
		}}

	case "assistant":
		content, present := msg["content"]
		if !present || content == nil {
			return nil
		}
		if text, isString := content.(string); isString {
			return []NormalizedBlock{{
				Kind:        KindAssistant,
				Text:        Sanitize(text),
				SourceIndex: msgIndex,
				HasSource:   hasIndex,
			}}
		}
		parts, isArray := content.([]any)
		if !isArray {
			return nil
		}
		blocks := make([]NormalizedBlock, 0, len(parts))
		for _, rawPart := range parts {
			part, isObject := rawPart.(map[string]any)
			if !isObject {
				continue
			}
			switch kind, _ := part["type"].(string); kind {
			case "text":
				text, _ := part["text"].(string)
				blocks = append(blocks, NormalizedBlock{
					Kind:        KindAssistant,
					Text:        Sanitize(text),
					SourceIndex: msgIndex,
					HasSource:   hasIndex,
				})
			case "toolCall":
				name, _ := part["name"].(string)
				args, _ := part["arguments"].(map[string]any)
				blocks = append(blocks, NormalizedBlock{
					Kind:        KindToolCall,
					Name:        name,
					Args:        args,
					SourceIndex: msgIndex,
					HasSource:   hasIndex,
				})
			}
		}
		return blocks
	}
	return nil
}

// Normalize is normalize.ts normalize(): every message expanded into blocks.
//
// sourceIndices, when non-nil, supplies the session-global `#N` index per input
// position; a negative entry stands for TS's `undefined` (no ref). When it is
// nil the legacy positional behaviour applies (the input position is the
// index), which callers without an index space rely on.
func Normalize(messages []map[string]any, sourceIndices []int) []NormalizedBlock {
	out := make([]NormalizedBlock, 0, len(messages))
	for i, msg := range messages {
		index := i
		hasIndex := true
		if sourceIndices != nil {
			if i < len(sourceIndices) {
				index = sourceIndices[i]
				hasIndex = index >= 0
			} else {
				hasIndex = false
			}
		}
		out = append(out, normalizeOne(msg, index, hasIndex)...)
	}
	return out
}

// messageRole reads a message's role, which every message needs.
func messageRole(msg map[string]any) string {
	role, _ := msg["role"].(string)
	return role
}

var _ = fmt.Sprintf
