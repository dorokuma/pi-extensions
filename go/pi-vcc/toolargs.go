package pivcc

import "strings"

// Ports src/core/tool-args.ts.

// extractPath is tool-args.ts extractPath(): the first path-keyed string
// argument, or "" plus false when none is present.
func extractPath(args map[string]any) (string, bool) {
	for _, key := range PathKeys {
		if value, ok := args[key].(string); ok {
			return value, true
		}
	}
	return "", false
}

// summarizeToolArgs is tool-args.ts summarizeToolArgs(): the first of
// path / command / query, else the argument names.
func summarizeToolArgs(args map[string]any) string {
	if path, ok := extractPath(args); ok {
		return "path=" + path
	}
	if command, ok := args["command"].(string); ok {
		return "command=" + command
	}
	if query, ok := args["query"].(string); ok {
		return "query=" + query
	}
	return strings.Join(sortedKeys(args), ", ")
}
