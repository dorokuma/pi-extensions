// Package ctxorchestrate implements the ctx-orchestrate extension for PiG in Go.
//
// The extension is the "end-state facility" of the codemode rollout:
//   - ctx_search_digest is a composite tool (exposure=namespace codemode) that
//     runs several ctx_fs rg searches concurrently, dedupes hits across patterns
//     and aggregates them per file, so the frequent multi-step ctx_fs retrieval
//     flow costs one call instead of many.
//   - A tool_result hook puts a physical cap on codemode's text() reply, so a
//     script cannot flood the transcript.
//
// It spawns no process, registers no command and touches no settings file: one
// registerTool and one tool_result handler are the whole surface.
//
// Alignment with the TypeScript implementation (extensions/ctx-orchestrate/src/ctx-orchestrate.ts):
//   - TS pi.registerTool({...}) → sdk.Extension.RegisterTool(sdk.ToolDefinition{...})
//   - TS pi.on("tool_result", ...) → sdk.Extension.OnToolResult
//   - TS ctx.executeTool(name, args, {signal}) → sdk.Context.ExecuteTool(name, args, nil);
//     a nil signal keeps the calling request's cancellation, which is what the
//     TS version passes.
//   - TS returns {content:[{type:"text",text}], details, structuredContent, isError}
//     → sdk.ToolResult{Content, Details, StructuredContent, IsError}.
//
// Differences from the TS version (and SDK gaps) are listed in README.md.
package ctxorchestrate

import (
	"fmt"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// NamespaceName is the codemode namespace the tool is grouped under (the tool is
// described by describeNamespace()). TS: NAMESPACE_NAME.
const NamespaceName = "ctxmode"

// namespace is the codemode group of ctx_search_digest. The instructions are the
// usage guidance codemode's describeNamespace() returns.
func namespace() *sdk.ToolNamespace {
	return &sdk.ToolNamespace{
		Name:        NamespaceName,
		Description: "ctxmode 组合工具：把多路 ctx_fs 检索压成一次调用，自带跨模式去重、按文件聚合与部分失败降级。",
		Instructions: strings.Join([]string{
			"ctx_search_digest 是组合工具：内部并发多个 ctx_fs rg 并自行聚合，调用方只拿摘要与结构化结果（outputSchema → structuredContent）。",
			"中间过程克制：不要在脚本里 text()/console.log 大段原始输出；用返回值做判断即可。",
			"最终报告完整：结束脚本前给出结构化结论（计数 + 关键路径 + 一句下一步）。",
			"失败容忍：部分失败不进异常，而是落在 structuredContent.errors / .failures / isError；先看这些字段再决定重试或降级。",
			"codemode 回传有 ~3000 字符硬顶（治理钩子），超了会被物理截断并附警告。",
		}, "\n"),
	}
}

// searchDigestSchema is the tool's argument schema. TS: searchDigestSchema (typebox
// Type.Object with one required member).
func searchDigestSchema() sdk.Schema {
	return sdk.Schema{
		"type": "object",
		"properties": map[string]any{
			"patterns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "One or more rg patterns (literal or regex) searched concurrently.",
			},
			"path": map[string]any{"type": "string", "description": "Search root under the workspace."},
			"glob": map[string]any{"type": "string", "description": "File glob filter, e.g. *.ts."},
			"max_hits": map[string]any{
				"type":        "number",
				"description": fmt.Sprintf("Hit budget per pattern (default %d, hard cap %d).", hitsDefault, hitsHard),
			},
			"include_hidden": map[string]any{"type": "boolean", "description": "Also search hidden directories and files."},
			"ignore_case":    map[string]any{"type": "boolean"},
			"context":        map[string]any{"type": "number", "description": "Context lines around each hit."},
			"literal":        map[string]any{"type": "boolean"},
		},
		"required": []any{"patterns"},
	}
}

// searchDigestOutputSchema is the schema of the tool's structuredContent. TS: searchDigestOutput.
func searchDigestOutputSchema() sdk.Schema {
	stringList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return sdk.Schema{
		"type": "object",
		"properties": map[string]any{
			"patterns": stringList,
			"files": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
						"hits": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"line":     map[string]any{"type": "number"},
									"excerpt":  map[string]any{"type": "string"},
									"patterns": stringList,
								},
								"required": []any{"line", "excerpt", "patterns"},
							},
						},
					},
					"required": []any{"path", "hits"},
				},
			},
			"summary": map[string]any{"type": "string"},
			"errors": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{"type": "string"},
						"error":   map[string]any{"type": "string"},
					},
					"required": []any{"pattern", "error"},
				},
			},
		},
		"required": []any{"patterns", "files", "summary"},
	}
}

// handleToolResult is the tool_result hook. It never fails the host: a panic or
// an unexpected payload leaves the result untouched, as the TS hook returning
// undefined does.
func handleToolResult(_ sdk.Context, data map[string]any) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			diagLog(fmt.Sprintf("tool_result hook failed (ignored): %v", recovered))
			result, err = nil, nil
		}
	}()
	return CapCodemodeResult(data), nil
}

// Extension constructs and returns the ctx-orchestrate extension instance for PiG.
func Extension() *sdk.Extension {
	e := sdk.New("ctx-orchestrate")

	readOnly := true
	e.RegisterTool(sdk.ToolDefinition{
		Name:  "ctx_search_digest",
		Label: "CTX Search Digest",
		Description: "Run several ctx_fs rg searches concurrently, dedupe hits and aggregate them per file → " +
			"{patterns, files:[{path,hits:[{line,excerpt,patterns}]}], summary}. Read-only.",
		PromptSnippet: "Multi-pattern search aggregated per file",
		PromptGuidelines: []string{
			"Use ctx_search_digest for several patterns at once instead of one rg call per pattern.",
			"Keep patterns concrete (identifiers, not '.*'); it returns excerpts, not file reads.",
		},
		Parameters:   searchDigestSchema(),
		OutputSchema: searchDigestOutputSchema(),
		Exposure:     sdk.ToolExposureCodemode,
		Namespace:    namespace(),
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: &readOnly},
		Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			return executeSearchDigest(params, executeToolRunner(ctx, "ctx_fs")), nil
		},
	})

	e.OnToolResult(handleToolResult)

	diagLog(fmt.Sprintf(
		"registered ctx_search_digest (exposure=codemode, namespace=%s) and codemode text cap governance (cap=%d, enabled=%t)",
		NamespaceName, TextCap(), GovernEnabled(),
	))
	return e
}
