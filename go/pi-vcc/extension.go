package pivcc

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Ports index.ts plus the host-facing halves of src/hooks/before-compact.ts,
// src/commands/pi-vcc.ts, src/commands/vcc-recall.ts and src/tools/recall.ts.
//
// index.ts does five things; Extension() does the same five:
//
//	scaffoldSettings(); registerBeforeCompactHook(pi); registerPiVccCommand(pi);
//	registerVccRecallCommand(pi); registerRecallTool(pi);

// ExtensionName is the extension's identity. PiG matches it against the selected
// package/piglet path.
const ExtensionName = "pi-vcc"

// CompactionDetailsCompactor is the details.compactor stamp pi-vcc writes, and
// the value its own session_compact handler looks for.
const CompactionDetailsCompactor = "pi-vcc"

// CompactionDetailsVersion is the details.version: 2 means the summary's `#N`
// refs are session-global (the recall index space); version 1 summaries carried
// window-relative refs (issue #28).
const CompactionDetailsVersion = 2

// Ranked brief budget constants (before-compact.ts).
const (
	// RankedBriefBudgetTokens is the floor of the ranked brief's token budget.
	RankedBriefBudgetTokens = 1100
	// RankedBriefCeilingTokens is the ceiling of the size-relative budget.
	RankedBriefCeilingTokens = 2000
	// RankedBriefTokensPerBlock is the per-block slope of the size-relative budget.
	RankedBriefTokensPerBlock = 15
)

// RecallToolDescription is tools/recall.ts's `description` array joined with "\n".
const RecallToolDescription = "Recall earlier parts of this session, including anything dropped by compaction.\n" +
	"Use it before saying the context is gone.\n" +
	"\n" +
	"Pick one per call:\n" +
	"- query: keyword search (regex works), 5 results per page.\n" +
	"- range: [from, to]: entries #from..#to in order, 20 per page.\n" +
	"- expand: [N, ...]: full untruncated text of those entries.\n" +
	"- mode: 'touched': files worked on, with entry indices.\n" +
	"- query '#N:path': file content from entry N ('#N:path:full' for all lines).\n" +
	"\n" +
	"Add to any of them:\n" +
	"- page: next page of query, range or touched results.\n" +
	"- scope: 'lineage' (default) reads the current conversation path; 'all' also\n" +
	"  reads edited or retried branches.\n" +
	"\n" +
	"#N is the number shown in summary refs like (#12) and in recall results.\n" +
	"Only the current session is searchable."

// RecallToolPromptSnippet is tools/recall.ts's promptSnippet.
const RecallToolPromptSnippet = "Recall earlier parts of this session before saying the context is gone. " +
	"One per call: query (search), range:[from, to] (entries in order), expand:[N] (full text), " +
	"mode:'touched' (files). #N is the number in summary refs like (#12)."

// PiVccCommandDescription is commands/pi-vcc.ts's command description.
const PiVccCommandDescription = "Compact conversation with pi-vcc structured summary"

// VccRecallCommandDescription is commands/vcc-recall.ts's command description.
const VccRecallCommandDescription = "Recall earlier parts of this session. Plain keywords work best; add scope:all to reach edited or retried turns."

// RecallToolName is the recall tool's registered name.
const RecallToolName = "vcc_recall"

// RecallToolSchema is tools/recall.ts's `parameters` TypeBox schema as the JSON
// Schema PiG accepts.
func RecallToolSchema() sdk.Schema {
	return sdk.Schema{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "What to recall, in plain keywords (e.g. 'redis cache decision'). Multi-word queries are ranked by relevance. A regex pattern also works. '#N:path' instead shows a file's content from entry N.",
			},
			"range": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "number"},
				"minItems":    2,
				"maxItems":    2,
				"description": "[from, to]: every entry from #from to #to (inclusive) in order, 20 per page. #N is the number in summary refs like (#12) and in recall results.",
			},
			"expand": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "number"},
				"description": "#N indices to return full untruncated content for (from summary refs like (#12) or recall results).",
			},
			"page": map[string]any{
				"type":        "number",
				"description": "Page number (1-based) for query, range or touched results. Default: 1.",
			},
			"scope": map[string]any{
				"type":        "string",
				"enum":        []any{"lineage", "all"},
				"description": "Default 'lineage' covers the active conversation path. Use 'all' to also reach messages from other branches, such as turns that were edited or retried.",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []any{"hybrid", "touched"},
				"description": "hybrid (default) = normal recall; touched = files worked on, by path, with entry indices. The touched list misses files written via apply_patch (paths inside the diff) or bash redirects.",
			},
		},
	}
}

// Extension builds the pi-vcc extension. It mirrors index.ts: scaffold the
// config file, then register the compaction hook, the two commands and the
// recall tool.
func Extension() *sdk.Extension {
	ext := sdk.New(ExtensionName)
	ScaffoldSettings()
	RegisterBeforeCompactHook(ext)
	RegisterPiVccCommand(ext)
	RegisterVccRecallCommand(ext)
	RegisterRecallTool(ext)
	return ext
}

// sessionManagerAdapter adapts the SDK's session manager to the lineage
// interface the recall code needs.
type sessionManagerAdapter struct {
	ctx sdk.Context
}

func (a sessionManagerAdapter) GetBranch() ([]LineageEntryLike, error) {
	entries, err := a.ctx.GetBranch()
	if err != nil {
		return nil, err
	}
	out := make([]LineageEntryLike, 0, len(entries))
	for _, entry := range entries {
		out = append(out, branchEntryToMap(entry))
	}
	return out, nil
}

func (a sessionManagerAdapter) GetEntries() ([]LineageEntryLike, error) {
	raw, err := a.ctx.GetEntries()
	if err != nil {
		return nil, err
	}
	out := make([]LineageEntryLike, 0, len(raw))
	for _, entry := range raw {
		decoded, ok := decodeJSONObject(entry)
		if !ok {
			continue
		}
		out = append(out, LineageEntryLike(decoded))
	}
	return out, nil
}

// branchEntryToMap rewrites a decoded BranchEntry as the JSON object shape the
// pure port reads. The SDK flattens a message entry's nested `message` object
// into top-level fields, so the nested form is reconstructed here.
func branchEntryToMap(entry sdk.BranchEntry) map[string]any {
	out := map[string]any{"id": entry.ID, "type": entry.Type}
	if entry.FirstKeptEntryID != "" {
		out["firstKeptEntryId"] = entry.FirstKeptEntryID
	}
	if entry.Type != "message" {
		if entry.Content != "" {
			out["content"] = entry.Content
		}
		if entry.Content != "" {
			out["summary"] = entry.Content
		}
		return out
	}
	message := map[string]any{"role": entry.Role}
	switch {
	case entry.Role == "bashExecution":
		// The SDK exposes no bashExecution fields; the shape can only carry
		// what the wire held, which a branch entry does not.
	case entry.ToolName != "" || entry.ToolCallID != "":
		message["toolName"] = entry.ToolName
		message["toolCallId"] = entry.ToolCallID
		message["isError"] = entry.IsError
	}
	if entry.Content != "" {
		message["content"] = entry.Content
	}
	if entry.Thinking != "" {
		message["thinking"] = entry.Thinking
	}
	if len(entry.ToolCalls) > 0 {
		parts := make([]any, 0, len(entry.ToolCalls)+1)
		if entry.Content != "" {
			parts = append(parts, map[string]any{"type": "text", "text": entry.Content})
		}
		for _, call := range entry.ToolCalls {
			var args map[string]any
			if call.Args != "" {
				decoded, _ := decodeJSONObject([]byte(call.Args))
				args = decoded
			}
			parts = append(parts, map[string]any{
				"type":      "toolCall",
				"id":        call.ID,
				"name":      call.Name,
				"arguments": args,
			})
		}
		message["content"] = parts
	}
	out["message"] = message
	return out
}

// ── /pi-vcc ─────────────────────────────────────────────────────────────────

// RegisterPiVccCommand is commands/pi-vcc.ts registerPiVccCommand().
func RegisterPiVccCommand(ext *sdk.Extension) {
	state := extStateOf(ext)
	ext.Command("pi-vcc", PiVccCommandDescription, func(ctx sdk.Context, args string) error {
		parsed := ParseKeepAndPrompt(args)
		instructions := BuildPiVccCustomInstructions(parsed.KeepUserTurns, parsed.HasKeepUserTurns)
		ctx.CompactWithOptions(sdk.CompactOptions{
			CustomInstructions: instructions,
			OnComplete: func(_ map[string]any) {
				if stats, ok := state.lastStatsRead(); ok {
					// before-compact.ts scheduleCompactionStatsNotify: the toast
					// lands after the UI settles.
					notifyLater(ctx, FormatCompactionStats(stats), "info")
				} else {
					ctx.Notify("Compacted with pi-vcc", "info")
				}
				if parsed.FollowUpPrompt != "" {
					_ = ctx.SendUserMessage(parsed.FollowUpPrompt, "")
				}
			},
			OnError: func(err error) {
				message := err.Error()
				switch {
				case message == "Compaction cancelled" || message == "Already compacted":
					ctx.Notify("Nothing to compact", "warning")
				case strings.HasPrefix(message, "Nothing to compact"):
					// Pi throws this before any extension hook runs.
					ctx.Notify("Nothing to compact yet: the whole session still fits in the recent part Pi always keeps "+
						"(compaction.keepRecentTokens in Pi settings). Try again once the session is longer.", "warning")
				default:
					ctx.Notify("Compaction failed: "+message, "error")
				}
			},
		})
		return nil
	})
}

// ── /pi-vcc-recall ──────────────────────────────────────────────────────────

// commandPagingHints is commands/vcc-recall.ts COMMAND_PAGING_HINTS.
var commandPagingHints = RecallPagingHints{
	OutOfRange: func(query string, scope RecallScope, totalPages int, truncated bool) string {
		tail := ", or refine your query."
		if truncated {
			tail = "."
		}
		return "Use /pi-vcc-recall " + query + scopeArg(scope) + " page:N with N between 1 and " +
			jsNumberString(float64(totalPages)) + tail
	},
	NextPage: func(query string, scope RecallScope, page int) string {
		return "\n--- /pi-vcc-recall " + query + scopeArg(scope) + " page:" + jsNumberString(float64(page)) + " ---"
	},
	SkipCurrentTurn: false,
}

func scopeArg(scope RecallScope) string {
	if scope == ScopeAll {
		return " scope:all"
	}
	return ""
}

// toolPagingHints is tools/recall.ts TOOL_PAGING_HINTS.
var toolPagingHints = RecallPagingHints{
	OutOfRange: func(_ string, _ RecallScope, totalPages int, truncated bool) string {
		if truncated {
			return "Use a page between 1 and " + jsNumberString(float64(totalPages)) + "."
		}
		return "Use a page between 1 and " + jsNumberString(float64(totalPages)) + ", or refine your query."
	},
	NextPage: func(_ string, scope RecallScope, page int) string {
		suffix := ""
		if scope == ScopeAll {
			suffix = " with scope:'all'"
		}
		return "\n--- Use page:" + jsNumberString(float64(page)) + suffix + " for more results ---"
	},
	SkipCurrentTurn: true,
	AroundHit: func(n int, scope RecallScope) string {
		from := n - 3
		if from < 0 {
			from = 0
		}
		suffix := ""
		if scope == ScopeAll {
			suffix = " (with scope:'all')"
		}
		return "\n--- Read around a hit: range:[" + jsNumberString(float64(from)) + ", " + jsNumberString(float64(n+3)) +
			"]; full text: expand:[" + jsNumberString(float64(n)) + "]" + suffix + " ---"
	},
}

// RegisterVccRecallCommand is commands/vcc-recall.ts registerVccRecallCommand().
func RegisterVccRecallCommand(ext *sdk.Extension) {
	ext.Command("pi-vcc-recall", VccRecallCommandDescription, func(ctx sdk.Context, args string) error {
		sessionFile, err := ctx.GetSessionFile()
		if err != nil || sessionFile == nil || *sessionFile == "" {
			ctx.Notify("No session file available.", "error")
			return nil
		}
		request := ParseCommandRequest(args)
		view := OpenRecallView(*sessionFile, request.Scope, sessionManagerAdapter{ctx: ctx})
		content := RunRecall(request, view, commandPagingHints)
		trigger := true
		return ctx.SendMessage("vcc-recall", content, true, sdk.SendMessageOptions{TriggerTurn: &trigger})
	})
}

// ── vcc_recall tool ─────────────────────────────────────────────────────────

// RegisterRecallTool is tools/recall.ts registerRecallTool().
func RegisterRecallTool(ext *sdk.Extension) {
	ext.RegisterTool(sdk.ToolDefinition{
		Name:        RecallToolName,
		Label:       "VCC Recall",
		Description: RecallToolDescription,
		Parameters:  RecallToolSchema(),

		PromptSnippet: RecallToolPromptSnippet,
		// Runs before pi validates the schema, so repairable mistakes never
		// reach the agent as errors.
		PrepareArguments: func(params map[string]any) (map[string]any, error) {
			return NormalizeToolParams(params), nil
		},
		Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			sessionFile, err := ctx.GetSessionFile()
			if err != nil || sessionFile == nil || *sessionFile == "" {
				return sdk.ToolResult{Content: "No session file available."}, nil
			}
			request := ParseToolRequest(NormalizeToolParams(params))
			view := OpenRecallView(*sessionFile, request.Scope, sessionManagerAdapter{ctx: ctx})
			return sdk.ToolResult{Content: RunRecall(request, view, toolPagingHints)}, nil
		},
	})
}
