// Package pitinyfish implements tool rendering for pi-tinyfish.
package pitinyfish

import (
	"fmt"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func renderSimpleCall(name string, value any) []string {
	var raw string
	switch v := value.(type) {
	case string:
		raw = v
	case []any:
		var items []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				items = append(items, s)
			}
		}
		raw = strings.Join(items, " | ")
	case []string:
		raw = strings.Join(v, " | ")
	default:
		if value != nil {
			raw = fmt.Sprint(value)
		}
	}

	display := raw
	runes := []rune(raw)
	if len(runes) > 80 {
		display = string(runes[:77]) + "..."
	}
	if display == "" {
		display = "(empty)"
	}

	return []string{fmt.Sprintf("%s %s", name, display)}
}

// RenderSearchCall renders the tinyfish_search call header.
func RenderSearchCall(_ sdk.Context, args map[string]any, _ sdk.ToolRenderContext, _ int) ([]string, error) {
	return renderSimpleCall("tinyfish_search", args["query"]), nil
}

// RenderFetchCall renders the tinyfish_fetch call header.
func RenderFetchCall(_ sdk.Context, args map[string]any, _ sdk.ToolRenderContext, _ int) ([]string, error) {
	val := args["url"]
	if val == nil || val == "" {
		val = args["urls"]
	}
	return renderSimpleCall("tinyfish_fetch", val), nil
}

func extractFirstText(result sdk.ToolRenderResult) string {
	for _, block := range result.Content {
		if t, ok := block["type"].(string); ok && t == "text" {
			if s, ok := block["text"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func asMap(val any) map[string]any {
	if m, ok := val.(map[string]any); ok {
		return m
	}
	return nil
}

func asInt(val any) int {
	switch v := val.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// RenderSearchResult renders the tinyfish_search result.
func RenderSearchResult(_ sdk.Context, result sdk.ToolRenderResult, options sdk.ToolRenderResultOptions, _ sdk.ToolRenderContext, _ int) ([]string, error) {
	return renderTinyFishResult(result, options, "search")
}

// RenderFetchResult renders the tinyfish_fetch result.
func RenderFetchResult(_ sdk.Context, result sdk.ToolRenderResult, options sdk.ToolRenderResultOptions, _ sdk.ToolRenderContext, _ int) ([]string, error) {
	return renderTinyFishResult(result, options, "fetch")
}

func renderTinyFishResult(result sdk.ToolRenderResult, options sdk.ToolRenderResultOptions, defaultKind string) ([]string, error) {
	details := asMap(result.Details)
	text := extractFirstText(result)

	phase, _ := details["phase"].(string)
	if options.IsPartial || phase != "" {
		if text == "" {
			text = "TinyFish working..."
		}
		return []string{text}, nil
	}

	errStr, _ := details["error"].(string)
	if errStr != "" || strings.HasPrefix(text, "Error:") {
		lines := strings.Split(text, "\n")
		firstLine := lines[0]
		if firstLine == "" {
			firstLine = errStr
		}
		if firstLine == "" {
			firstLine = "TinyFish error"
		}
		return []string{firstLine}, nil
	}

	truncatedNotice := ""
	if truncMap := asMap(details["truncation"]); truncMap != nil {
		if isTrunc, ok := truncMap["truncated"].(bool); ok && isTrunc {
			outBytes := asInt(truncMap["outputBytes"])
			totBytes := asInt(truncMap["totalBytes"])
			truncatedNotice = fmt.Sprintf(" [truncated: %s of %s]", sdk.FormatSize(outBytes), sdk.FormatSize(totBytes))
		}
	}

	kind, _ := details["kind"].(string)
	if kind == "" {
		kind = defaultKind
	}

	if !options.Expanded {
		if kind == "search" {
			returned := asInt(details["returnedResults"])
			total := asInt(details["totalResults"])
			querySuffix := ""
			if q, ok := details["query"].(string); ok && q != "" {
				querySuffix = fmt.Sprintf(" for %s", q)
			}
			line := fmt.Sprintf("%d/%d results%s%s", returned, total, querySuffix, truncatedNotice)
			return []string{line}, nil
		}

		if kind == "fetch" {
			successful := asInt(details["successful"])
			urlCount := asInt(details["urlCount"])
			errCount := 0
			if errList, ok := details["errors"].([]any); ok {
				errCount = len(errList)
			} else if errList, ok := details["errors"].([]FetchError); ok {
				errCount = len(errList)
			}
			errSuffix := ""
			if errCount > 0 {
				errSuffix = fmt.Sprintf(", %d errors", errCount)
			}
			line := fmt.Sprintf("%d/%d fetched%s%s", successful, urlCount, errSuffix, truncatedNotice)
			return []string{line}, nil
		}

		lines := strings.Split(text, "\n")
		lineCount := len(lines)
		if text == "" {
			lineCount = 0
		}
		line := fmt.Sprintf("done (%d lines)%s", lineCount, truncatedNotice)
		return []string{line}, nil
	}

	if text == "" {
		return []string{""}, nil
	}
	return strings.Split(text, "\n"), nil
}
