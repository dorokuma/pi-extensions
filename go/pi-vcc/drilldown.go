package pivcc

import (
	"strconv"
	"strings"
)

// Ports src/core/drill-down.ts: resolve `#N:path[:suffix]` to a tool call's
// file content.

// DrillTarget is the parsed form of a drill-down query.
type DrillTarget struct {
	Index       int
	PathPattern string
	Full        bool
	Offset      int
	HasOffset   bool
	Limit       int
	HasLimit    bool
}

// drillDownRe is drill-down.ts DRILLDOWN_PATTERN:
// /^#(\d+):(.+?)(?::(full|\d+(?::\d+)?))?$.
//
// Go's RE2 does support a lazy `+?`, so the pattern carries over verbatim: the
// path group expands only as far as needed for the optional suffix to match.
// `(.+)` would instead make the suffix group dead, since the greedy group can
// always absorb it.
var drillDownRe = mustCompile(`^#(\d+):(.+?)(?::(full|\d+(?::\d+)?))?$`)

// ParseDrillDown is drill-down.ts parseDrillDown(): nil when the query is not a
// drill-down pattern. The regex's lazy path group already separates the path
// from the trailing `:full` / `:N[:M]` suffix, so the groups are used directly.
func ParseDrillDown(query string) *DrillTarget {
	match := drillDownRe.FindStringSubmatch(query)
	if match == nil {
		return nil
	}
	index, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	target := &DrillTarget{Index: index, PathPattern: match[2]}
	suffix := match[3]
	if suffix == "" {
		return target
	}
	if suffix == "full" {
		target.Full = true
		return target
	}
	parts := strings.Split(suffix, ":")
	if offset, err := strconv.Atoi(parts[0]); err == nil {
		target.Offset = offset
		target.HasOffset = true
		if len(parts) > 1 {
			if limit, err := strconv.Atoi(parts[1]); err == nil {
				target.Limit = limit
				target.HasLimit = true
			}
		}
	}
	return target
}

// contentBearingCall is drill-down.ts ContentBearingCall.
type contentBearingCall struct {
	Name       string
	Path       string
	Content    string
	HasContent bool
	OldText    string
	HasOldText bool
	NewText    string
	HasNewText bool
	Edits      []map[string]any
	HasEdits   bool
}

// findContentBearingCalls is drill-down.ts findContentBearingCalls().
func findContentBearingCalls(content any) []contentBearingCall {
	results := make([]contentBearingCall, 0, 2)
	for _, part := range toolCallParts(content) {
		args, _ := part["arguments"].(map[string]any)
		if args == nil {
			continue
		}
		if !IsContentBearing(args) {
			continue
		}
		path, ok := extractPath(args)
		if !ok {
			continue
		}
		name, _ := part["name"].(string)
		call := contentBearingCall{Name: name, Path: path}
		if text, ok := args["content"].(string); ok {
			call.Content = text
			call.HasContent = true
		}
		if edits, ok := args["edits"].([]any); ok {
			for _, edit := range edits {
				block, isObject := edit.(map[string]any)
				if !isObject {
					continue
				}
				call.Edits = append(call.Edits, block)
			}
			call.HasEdits = true
		}
		if oldText, ok := args["oldText"].(string); ok && !call.HasEdits {
			call.OldText = oldText
			call.HasOldText = true
		}
		if newText, ok := args["newText"].(string); ok && !call.HasEdits {
			call.NewText = newText
			call.HasNewText = true
		}
		results = append(results, call)
	}
	return results
}

// drillPreviewLimit and drillMaxFullBytes are drill-down.ts's display limits.
const (
	drillPreviewLimit = 30
	drillMaxFullBytes = 50 * 1024
)

// formatToolCallContent is drill-down.ts formatToolCallContent().
func formatToolCallContent(call contentBearingCall, entryIndex int, full bool, offset int, hasOffset bool, limit int, hasLimit bool) string {
	body := ""
	switch {
	case call.HasContent && call.Content != "":
		body = call.Content
	case call.HasEdits:
		blocks := make([]string, 0, len(call.Edits))
		for i, edit := range call.Edits {
			oldText, _ := edit["oldText"].(string)
			newText, _ := edit["newText"].(string)
			blocks = append(blocks, "--- edit "+jsNumberString(float64(i+1))+" ---\n"+oldText+"\n--- becomes ---\n"+newText)
		}
		body = strings.Join(blocks, "\n\n")
	case call.HasOldText && call.HasNewText && call.OldText != "":
		body = "--- old ---\n" + call.OldText + "\n--- new ---\n" + call.NewText
	default:
		body = "(no file content found in tool call arguments)"
	}

	allLines := jsSplitLines(body)
	totalLines := len(allLines)
	indexText := jsNumberString(float64(entryIndex))

	if full {
		// drill-down.ts gates on the UTF-8 byte length (`Buffer.byteLength(body,
		// "utf8") > MAX_FULL_BYTES`) but cuts with `body.slice(0, MAX_FULL_BYTES)`,
		// i.e. a UTF-16 code-unit slice. Slicing Go bytes instead would chop a
		// multi-byte character in half at the 50KB boundary, so the cut goes
		// through jsSlice; the shortfall stays a byte difference, exactly as TS's
		// `Buffer.byteLength(body, "utf8") - MAX_FULL_BYTES` reports it.
		if len(body) > drillMaxFullBytes {
			truncated := jsSlice(body, 0, drillMaxFullBytes)
			return "File: " + call.Path + "\nTool: " + call.Name + "\n\n" + truncated + "\n\n... (" +
				jsNumberString(float64(len(body)-drillMaxFullBytes)) + " more bytes — file exceeds 50KB display limit. Use #" +
				indexText + ":" + call.Path + ":" + jsNumberString(float64(drillPreviewLimit)) + " for next page.)"
		}
		return "File: " + call.Path + "\nTool: " + call.Name + "\n\n" + body
	}

	if hasOffset {
		startLine := offset
		if startLine < 0 {
			startLine = 0
		}
		maxLines := drillPreviewLimit
		if hasLimit {
			maxLines = limit
		}
		endLine := startLine + maxLines
		if endLine > totalLines {
			endLine = totalLines
		}
		// startLine is NOT clamped up to totalLines: drill-down.ts keeps the
		// caller's own value (`Math.max(0, offset)` only floors at 0), so an
		// out-of-range offset is reported back verbatim in the diagnostic below.
		// Slicing is what needs guarding — a window that starts past the end, or a
		// negative limit that puts endLine before startLine, simply shows nothing.
		var visible []string
		if startLine < totalLines && startLine < endLine {
			visible = allLines[startLine:endLine]
		}
		displayStart := startLine + 1

		if len(visible) == 0 {
			return "Offset " + jsNumberString(float64(startLine)) + " is beyond file length " +
				jsNumberString(float64(totalLines)) + ". Use #" + indexText + ":" + call.Path + " for the first " +
				jsNumberString(float64(drillPreviewLimit)) + " lines."
		}

		result := "File: " + call.Path + "\nTool: " + call.Name + "\nLines " + jsNumberString(float64(displayStart)) +
			"-" + jsNumberString(float64(endLine)) + " (of " + jsNumberString(float64(totalLines)) + "):\n\n"
		result += strings.Join(visible, "\n")
		if endLine < totalLines {
			result += "\n\n--- Use #" + indexText + ":" + call.Path + ":" + jsNumberString(float64(endLine)) + " or #" +
				indexText + ":" + call.Path + ":" + jsNumberString(float64(endLine)) + ":" +
				jsNumberString(float64(maxLines)) + " for next " + jsNumberString(float64(maxLines)) + " lines, #" +
				indexText + ":" + call.Path + ":full for complete ---"
		} else if offset > 0 {
			result += "\n\n(End of file)"
		}
		return result
	}

	if totalLines > drillPreviewLimit {
		preview := strings.Join(allLines[:drillPreviewLimit], "\n")
		return "File: " + call.Path + "\nTool: " + call.Name + "\n\n" + preview + "\n\n...(" +
			jsNumberString(float64(totalLines-drillPreviewLimit)) + " more lines — use #" + indexText + ":" + call.Path +
			":full for complete content, or #" + indexText + ":" + call.Path + ":" +
			jsNumberString(float64(drillPreviewLimit)) + " for next " + jsNumberString(float64(drillPreviewLimit)) + " lines)"
	}
	return "File: " + call.Path + "\nTool: " + call.Name + "\n\n" + body
}

// ExpandEntryFile is drill-down.ts expandEntryFile(): the drill-down's content
// for one entry, or a diagnostic naming why there is none.
func ExpandEntryFile(sessionFile string, entryIndex int, pathPattern string, full bool, offset int, hasOffset bool, limit int, hasLimit bool) string {
	loaded := LoadAllMessages(sessionFile, true, nil)
	pos := -1
	for i, entry := range loaded.Rendered {
		if entry.Index == entryIndex {
			pos = i
			break
		}
	}
	if pos < 0 {
		return "Entry #" + jsNumberString(float64(entryIndex)) + " not found in session history."
	}
	msg := loaded.RawMessages[pos]
	calls := findContentBearingCalls(msg["content"])
	indexText := jsNumberString(float64(entryIndex))

	// Special case: the "file" keyword selects the entry's file operations.
	if pathPattern == "file" {
		if len(calls) == 0 {
			return "No file content found in entry #" + indexText + "."
		}
		if len(calls) == 1 {
			return formatToolCallContent(calls[0], entryIndex, full, offset, hasOffset, limit, hasLimit)
		}
		items := make([]string, 0, len(calls))
		for _, call := range calls {
			items = append(items, "  [#"+indexText+":"+call.Path+"] "+call.Name+"("+call.Path+")")
		}
		return "Entry #" + indexText + " has " + jsNumberString(float64(len(calls))) + " file operations:\n" +
			strings.Join(items, "\n") + "\n\nUse #" + indexText + ":path to drill into a specific file."
	}

	matched := make([]contentBearingCall, 0, len(calls))
	for _, call := range calls {
		if strings.Contains(call.Path, pathPattern) {
			matched = append(matched, call)
		}
	}
	if len(matched) == 0 {
		return "No file content found in entry #" + indexText + " for \"" + pathPattern + "\"."
	}
	if len(matched) > 1 {
		items := make([]string, 0, len(matched))
		for _, call := range matched {
			items = append(items, "  [#"+indexText+":"+call.Path+"] "+call.Name+"("+call.Path+")")
		}
		return "Entry #" + indexText + " has " + jsNumberString(float64(len(matched))) +
			" file operations matching \"" + pathPattern + "\":\n" + strings.Join(items, "\n") +
			"\n\nUse #" + indexText + ":<more-specific-path> to drill into a specific file."
	}
	return formatToolCallContent(matched[0], entryIndex, full, offset, hasOffset, limit, hasLimit)
}
