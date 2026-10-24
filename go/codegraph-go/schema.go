package codegraphgo

import sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

func toolDefinition() sdk.ToolDefinition {
	return sdk.ToolDefinition{
		Name: "codegraph", Label: "CodeGraph",
		Description:   `CodeGraph: code search & call-graph analysis. Actions: search(pattern,[glob],[path]), files(glob,[path]), explore([query],[path]), callees(name,[file]), callers(name,[file]), impact(name,[file]), node(file|[name],[line]), status([path]), affected(files,[depth]), communities([path]), store_fact(targetFile,content), search_facts([query],[targetFile]). Common params: path, glob, max.`,
		PromptSnippet: "Code search & call-graph analysis via unified action router",
		PromptGuidelines: []string{
			"Use codegraph with action='explore' FIRST for codebase/symbol overview. Empty query = project overview.",
			"Use action='search' for literal code search (file:line results); set regex=true for regex matching, no_ignore=true to include .gitignore'd files.",
			"Use action='files' to list files by glob.",
			"Use action='callees'/'callers'/'impact' for call-graph analysis.",
			"Use action='node' for full symbol detail (location, signature, deps) or file reading with line numbers.",
			"Use action='affected' after editing to find tests to run.",
			"Use action='communities' for global architecture/module structure questions.",
			"Use action='store_fact'/'search_facts' for cross-session knowledge persistence.",
			"Use action='status' when results are unexpectedly empty to check index health.",
			"When the user names a project under a home workdir, pass path= that project yourself.",
		},
		Parameters: sdk.Schema{"type": "object", "required": []string{"action"}, "properties": sdk.Schema{
			"action":       sdk.Schema{"type": "string", "description": "Action to perform", "enum": []string{"search", "files", "explore", "callees", "callers", "impact", "node", "status", "affected", "communities", "store_fact", "search_facts"}},
			"pattern":      sdk.Schema{"type": "string", "description": "search: literal text to find (default); set regex=true for regular expression semantics"},
			"name":         sdk.Schema{"type": "string", "description": "callees/callers/impact/node: symbol name"},
			"file":         sdk.Schema{"type": "string", "description": "node/callees/callers/impact: file path or basename to pin"},
			"query":        sdk.Schema{"type": "string", "description": "explore/search_facts: symbol or free-text / search term"},
			"path":         sdk.Schema{"type": "string", "description": "Most actions: subdirectory or project name (home mode)"},
			"glob":         sdk.Schema{"type": "string", "description": "search/files/callees/callers/impact: file glob filter, e.g. \"*.go\""},
			"max":          sdk.Schema{"type": "number", "description": "search/files/explore/callees/callers/impact/communities/search_facts: result cap"},
			"ignore_case":  sdk.Schema{"type": "boolean", "description": "search: case-insensitive search"},
			"regex":        sdk.Schema{"type": "boolean", "description": "search: treat pattern as a regular expression (default false: literal match)"},
			"no_ignore":    sdk.Schema{"type": "boolean", "description": "search: include .gitignore'd files (default false: ignore rules are respected)"},
			"line":         sdk.Schema{"type": "number", "description": "node: pin definition at/around this line"},
			"includeCode":  sdk.Schema{"type": "boolean", "description": "node: include source body (default false)"},
			"symbolsOnly":  sdk.Schema{"type": "boolean", "description": "node: symbol map + dependents only"},
			"offset":       sdk.Schema{"type": "number", "description": "node: 1-based start line"},
			"limit":        sdk.Schema{"type": "number", "description": "node: max lines (cap 2000)"},
			"skipCode":     sdk.Schema{"type": "boolean", "description": "explore: omit source code (default true)"},
			"depth":        sdk.Schema{"type": "number", "description": "affected: max dependency traversal depth (default 5, max 10)"},
			"filter":       sdk.Schema{"type": "string", "description": "affected: custom glob to identify test files"},
			"minSize":      sdk.Schema{"type": "number", "description": "communities: minimum community size (default 3)"},
			"targetFile":   sdk.Schema{"type": "string", "description": "store_fact/search_facts: target file path"},
			"targetSymbol": sdk.Schema{"type": "string", "description": "store_fact/search_facts: target symbol"},
			"targetLine":   sdk.Schema{"type": "number", "description": "store_fact: target line number"},
			"content":      sdk.Schema{"type": "string", "description": "store_fact: fact content"},
			"author":       sdk.Schema{"type": "string", "description": "store_fact: author"},
			"supersedes":   sdk.Schema{"type": "number", "description": "store_fact: fact id to replace"},
			"status":       sdk.Schema{"type": "string", "description": "search_facts: filter by status (default 'active')"},
			"projectPath":  sdk.Schema{"type": "string", "description": "absolute path inside a project (nearest .codegraph/)"},
			"files":        sdk.Schema{"type": "array", "items": sdk.Schema{"type": "string"}, "description": "affected: list of changed source files"},
		}},
	}
}
