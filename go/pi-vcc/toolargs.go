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

// ExtractPaths is tool-args.ts extractPaths(): every file a tool call names —
// its single path arg, else the string entries of a `paths` array (a multi-file
// edit tool). TS's `if (one)` is truthiness, so an empty path arg falls through
// to `paths`.
func ExtractPaths(args map[string]any) []string {
	if one, ok := extractPath(args); ok && one != "" {
		return []string{one}
	}
	paths, isArray := args["paths"].([]any)
	if !isArray {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, item := range paths {
		if path, isString := item.(string); isString && path != "" {
			out = append(out, path)
		}
	}
	return out
}

// summarizeToolArgs is tool-args.ts summarizeToolArgs(): the first of
// path / command / query, else the argument names. Extra paths of a multi-path
// call are counted behind the first one (`path=a +2`).
func summarizeToolArgs(args map[string]any) string {
	paths := ExtractPaths(args)
	if len(paths) > 0 && paths[0] != "" {
		return "path=" + paths[0] + morePathsSuffix(paths[1:])
	}
	if command, ok := args["command"].(string); ok {
		return "command=" + command
	}
	if query, ok := args["query"].(string); ok {
		return "query=" + query
	}
	return strings.Join(sortedKeys(args), ", ")
}

// morePathsSuffix is the `more.length ? ` +${more.length}` : ""` tail both
// summarizeToolArgs and brief.ts toolOneLiner put behind the first path.
func morePathsSuffix(more []string) string {
	if len(more) == 0 {
		return ""
	}
	return " +" + jsNumberString(float64(len(more)))
}
