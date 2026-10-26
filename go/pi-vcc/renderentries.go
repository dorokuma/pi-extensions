package pivcc

import "strings"

// Ports src/core/render-entries.ts: one session message rendered as a recall
// entry.

// RenderedEntry is render-entries.ts RenderedEntry.
type RenderedEntry struct {
	Index   int
	Role    string
	Summary string
	Files   []string
}

// toolCallParts is render-entries.ts toolCallParts(): every toolCall block of a
// message's content.
func toolCallParts(content any) []map[string]any {
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, raw := range parts {
		part, isObject := raw.(map[string]any)
		if !isObject {
			continue
		}
		if kind, _ := part["type"].(string); kind == "toolCall" {
			out = append(out, part)
		}
	}
	return out
}

// toolCalls renders a message's tool calls as `name(args-summary), …`.
func toolCalls(content any) string {
	parts := toolCallParts(content)
	rendered := make([]string, 0, len(parts))
	for _, part := range parts {
		name, _ := part["name"].(string)
		args, _ := part["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		rendered = append(rendered, name+"("+summarizeToolArgs(args)+")")
	}
	return strings.Join(rendered, ", ")
}

// extractFilesFromContent is render-entries.ts extractFilesFromContent():
// `toolCallParts(content).flatMap((c) => extractPaths(c.arguments))` — every
// path each tool call names.
func extractFilesFromContent(content any) []string {
	parts := toolCallParts(content)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		args, _ := part["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		out = append(out, ExtractPaths(args)...)
	}
	return out
}

// RenderMessage is render-entries.ts renderMessage().
func RenderMessage(msg map[string]any, index int, full bool) RenderedEntry {
	role := messageRole(msg)
	if role == "user" {
		text := TextOf(msg["content"])
		if !full {
			text = Clip(text, 300)
		}
		return RenderedEntry{Index: index, Role: "user", Summary: text}
	}
	if role == "toolResult" {
		text := TextOf(msg["content"])
		if !full {
			text = Clip(text, 200)
		}
		toolName, _ := msg["toolName"].(string)
		return RenderedEntry{Index: index, Role: "tool_result", Summary: "[" + toolName + "] " + text}
	}
	// bashExecution has command+output instead of content.
	if role == "bashExecution" {
		command, _ := msg["command"].(string)
		output, _ := msg["output"].(string)
		raw := "$ " + command + "\n" + output
		text := raw
		if !full {
			text = Clip(raw, 300)
		}
		return RenderedEntry{Index: index, Role: "bash", Summary: text}
	}
	text := TextOf(msg["content"])
	if !full {
		text = Clip(text, 300)
	}
	tools := toolCalls(msg["content"])
	files := extractFilesFromContent(msg["content"])
	summary := text
	if tools != "" {
		summary = tools + "\n" + text
	}
	entry := RenderedEntry{Index: index, Role: "assistant", Summary: summary}
	if len(files) > 0 {
		entry.Files = files
	}
	return entry
}
