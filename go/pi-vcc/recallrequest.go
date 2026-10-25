package pivcc

import (
	"strconv"
	"strings"
)

// Ports src/core/recall-request.ts: repair tool arguments and turn them into the
// one action a recall call runs.

// DrillTargetPtr is `NonNullable<ReturnType<typeof parseDrillDown>>`.
type DrillTargetPtr = *DrillTarget

// RecallActionKind names the actions a recall call can run.
type RecallActionKind string

const (
	// ActionDrill is a `#N:path` drill-down.
	ActionDrill RecallActionKind = "drill"
	// ActionTouched lists the files worked on.
	ActionTouched RecallActionKind = "touched"
	// ActionExpand returns full text of entries.
	ActionExpand RecallActionKind = "expand"
	// ActionRange returns entries in order.
	ActionRange RecallActionKind = "range"
	// ActionSearch runs a ranked search.
	ActionSearch RecallActionKind = "search"
	// ActionRecent lists the most recent entries.
	ActionRecent RecallActionKind = "recent"
)

// RecallAction is recall-request.ts RecallAction: exactly one per call.
type RecallAction struct {
	Kind RecallActionKind
	// Drill is set for ActionDrill.
	Drill DrillTargetPtr
	// Page applies to touched, range and search.
	Page int
	// Indices applies to expand.
	Indices []int
	// Range applies to range.
	Range []int
	// Query applies to search and recent.
	Query string
}

// RecallRequest is recall-request.ts RecallRequest.
type RecallRequest struct {
	Scope  RecallScope
	Action RecallAction
	// Ignored names params the action did not use.
	Ignored []string
	// Dropped names values normalizeToolParams could not repair.
	Dropped []string
}

// RecallToolParams is recall-request.ts RecallToolParams.
type RecallToolParams struct {
	Query  any
	Expand any
	Range  any
	Page   any
	Scope  any
	Mode   any
}

// DroppedKey is recall-request.ts DROPPED_KEY: where normalizeToolParams records
// values it had to drop.
const DroppedKey = "_vccDropped"

// knownParams is recall-request.ts KNOWN_PARAMS.
var knownParams = map[string]bool{
	"query": true, "range": true, "expand": true, "page": true, "scope": true, "mode": true,
}

// toIndex is recall-request.ts toIndex(): 12, "12", "#12", " 12 " -> 12.
func toIndex(value any) (int, bool) {
	switch value := value.(type) {
	case float64:
		if value == float64(int(value)) {
			return int(value), true
		}
		return 0, false
	case int:
		return value, true
	case string:
		trimmed := jsTrimSpace(value)
		if match := indexTokenRe.FindStringSubmatch(trimmed); match != nil {
			n, err := strconv.Atoi(match[1])
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// indexTokenRe is recall-request.ts's `/^#?(\d+)$/`.
var indexTokenRe = mustCompile(`^#?(\d+)$`)

// indexListRe is recall-request.ts's
// `/^\s*#?\d+(\s*[,\s]\s*#?\d+)*\s*$/`.
var indexListRe = mustCompile(`^` + jsSpace + `*#?\d+(` + jsSpace + `*[,\s]` + jsSpace + `*#?\d+)*` + jsSpace + `*$`)

// digitRunRe is the `/\d+/g` scan indexList uses.
var digitRunRe = mustCompile(`\d+`)

// indexList is recall-request.ts indexList(): "12", "#12, #15", "12 15" ->
// numbers, failing unless the whole string is #N tokens.
func indexList(s string) ([]int, bool) {
	trimmed := jsTrimSpace(s)
	trimmed = strings.TrimPrefix(trimmed, "[")
	trimmed = strings.TrimSuffix(trimmed, "]")
	if !indexListRe.MatchString(trimmed) {
		return nil, false
	}
	matches := digitRunRe.FindAllString(trimmed, -1)
	out := make([]int, 0, len(matches))
	for _, match := range matches {
		n, err := strconv.Atoi(match)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// rangeStringRe is recall-request.ts's
// `/^\[?\s*#?(\d+)\s*(?:(?:-|\.\.|,|to)\s*#?(\d+)\s*)?\]?$/i`.
var rangeStringRe = compileIgnoreCase(`^\[?` + jsSpace + `*#?(\d+)` + jsSpace + `*(?:(?:-|\.\.|,|to)` + jsSpace +
	`*#?(\d+)` + jsSpace + `*)?\]?$`)

// NormalizeToolParams is recall-request.ts normalizeToolParams(): repair the
// argument mistakes whose meaning is certain, and name the ones that cannot be
// repaired under DroppedKey.
func NormalizeToolParams(raw any) map[string]any {
	object, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	params := make(map[string]any, len(object))
	for key, value := range object {
		params[key] = value
	}
	dropped := make([]string, 0, 2)
	drop := func(key, why string) {
		dropped = append(dropped, key+" "+jsonLiteral(params[key])+" ("+why+")")
		delete(params, key)
	}

	for key := range object {
		if !knownParams[key] && key != DroppedKey {
			drop(key, "not a vcc_recall param")
		}
	}

	if value, present := params["page"]; present {
		if n, ok := toIndex(value); ok && n >= 1 {
			params["page"] = float64(n)
		} else {
			drop("page", "not a page number")
		}
	}

	if value, present := params["expand"]; present {
		var list []int
		valid := true
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				n, ok := toIndex(item)
				if !ok {
					valid = false
					break
				}
				list = append(list, n)
			}
		case string:
			var ok bool
			list, ok = indexList(typed)
			valid = ok
		default:
			n, ok := toIndex(typed)
			if ok {
				list = []int{n}
			} else {
				valid = false
			}
		}
		if valid {
			expanded := make([]any, 0, len(list))
			for _, n := range list {
				expanded = append(expanded, float64(n))
			}
			params["expand"] = expanded
		} else {
			drop("expand", "not #N indices")
		}
	}

	if value, present := params["range"]; present {
		var pair []int
		valid := true
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				n, ok := toIndex(item)
				if !ok {
					valid = false
					break
				}
				pair = append(pair, n)
			}
		case string:
			if match := rangeStringRe.FindStringSubmatch(typed); match != nil {
				from, _ := strconv.Atoi(match[1])
				to := from
				if match[2] != "" {
					to, _ = strconv.Atoi(match[2])
				}
				pair = []int{from, to}
			} else {
				valid = false
			}
		default:
			if n, ok := toIndex(typed); ok {
				pair = []int{n}
			} else {
				valid = false
			}
		}
		// A single index reads just that entry.
		if valid && len(pair) == 1 {
			pair = []int{pair[0], pair[0]}
		}
		if valid && len(pair) == 2 {
			from, to := pair[0], pair[1]
			if from > to {
				from, to = to, from
			}
			params["range"] = []any{float64(from), float64(to)}
		} else {
			drop("range", "not [from, to]")
		}
	}

	for _, key := range []string{"scope", "mode"} {
		if text, ok := params[key].(string); ok {
			params[key] = strings.ToLower(jsTrimSpace(text))
		}
	}
	if mode, _ := params["mode"].(string); (mode == "all" || mode == "lineage") && params["scope"] == nil {
		params["scope"] = mode
		delete(params, "mode")
	}
	if scope, present := params["scope"]; present {
		if text, _ := scope.(string); text != "lineage" && text != "all" {
			drop("scope", "use 'lineage' or 'all'")
		}
	}
	if mode, present := params["mode"]; present {
		if text, _ := mode.(string); text != "hybrid" && text != "touched" {
			drop("mode", "use 'touched' or leave it out")
		}
	}
	if query, present := params["query"]; present {
		if _, ok := query.(string); !ok {
			drop("query", "not text")
		}
	}

	if len(dropped) > 0 {
		items := make([]any, 0, len(dropped))
		for _, value := range dropped {
			items = append(items, value)
		}
		params[DroppedKey] = items
	}
	return params
}

// jsonLiteral renders a value for the "(why)" note the way JSON.stringify does.
func jsonLiteral(value any) string {
	if value == nil {
		return "null"
	}
	return safeJSONStringify(value)
}

// ParseToolRequest is recall-request.ts parseToolRequest(): the precedence lives
// here and only here — #N:path drill-down, then mode:'touched', then expand,
// then range, then a query search, then the most recent entries.
func ParseToolRequest(params map[string]any) RecallRequest {
	scope := NormalizeRecallScope(params["scope"])

	query, _ := params["query"].(string)
	trimmedQuery := jsTrimSpace(query)

	indices := make([]int, 0, 2)
	seen := make(map[int]bool)
	if raw, ok := params["expand"].([]any); ok {
		for _, item := range raw {
			if n, ok := toIndex(item); ok {
				if seen[n] {
					continue
				}
				seen[n] = true
				indices = append(indices, n)
			}
		}
	}

	hasRange := false
	var rangePair []int
	if raw, ok := params["range"].([]any); ok && len(raw) > 0 {
		hasRange = true
		for _, item := range raw {
			n, _ := toIndex(item)
			rangePair = append(rangePair, n)
		}
	}

	page := 1
	if raw, present := params["page"]; present {
		if n, ok := toIndex(raw); ok {
			page = n
		}
	}

	touched := NormalizeRecallMode(params["mode"]) == ModeTouched

	set := map[string]bool{
		"query":   trimmedQuery != "",
		"expand":  len(indices) > 0,
		"range":   hasRange,
		"page":    page > 1,
		"touched": touched,
	}

	var dropped []string
	if raw, ok := params[DroppedKey].([]any); ok && len(raw) > 0 {
		for _, item := range raw {
			dropped = append(dropped, jsToStringValue(item))
		}
	}

	req := func(action RecallAction, uses ...string) RecallRequest {
		order := []string{"query", "expand", "range", "page", "touched"}
		ignored := make([]string, 0, 2)
		for _, key := range order {
			if !set[key] {
				continue
			}
			used := false
			for _, use := range uses {
				if use == key {
					used = true
					break
				}
			}
			if used {
				continue
			}
			if key == "touched" {
				ignored = append(ignored, "mode")
			} else {
				ignored = append(ignored, key)
			}
		}
		out := RecallRequest{Scope: scope, Action: action, Dropped: dropped}
		if len(ignored) > 0 {
			out.Ignored = ignored
		}
		return out
	}

	if trimmedQuery != "" {
		if target := ParseDrillDown(trimmedQuery); target != nil {
			return req(RecallAction{Kind: ActionDrill, Drill: target}, "query")
		}
	}
	if touched {
		return req(RecallAction{Kind: ActionTouched, Page: page}, "touched", "page")
	}
	if set["expand"] {
		return req(RecallAction{Kind: ActionExpand, Indices: indices}, "expand")
	}
	if hasRange {
		return req(RecallAction{Kind: ActionRange, Range: rangePair, Page: maxInt(1, page)}, "range", "page")
	}
	if trimmedQuery != "" {
		return req(RecallAction{Kind: ActionSearch, Query: query, Page: maxInt(1, page)}, "query", "page")
	}
	return req(RecallAction{Kind: ActionRecent, Query: query})
}

// jsToStringValue is JS's `String(v)` for the values a dropped note holds.
func jsToStringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return safeJSONStringify(value)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// pageRe is recall-request.ts's `/\bpage:(\d+)\b/i`.
var pageRe = compileIgnoreCase(`\bpage:(\d+)\b`)

// ParseCommandRequest is recall-request.ts parseCommandRequest(): `/pi-vcc-recall
// <query> [scope:all] [page:N]`.
func ParseCommandRequest(args string) RecallRequest {
	scope, text := ParseRecallScope(jsTrimSpace(args))
	page := 1
	if match := pageRe.FindStringSubmatch(text); match != nil {
		if n, err := strconv.Atoi(match[1]); err == nil {
			page = maxInt(1, n)
		}
	}
	query := jsTrimSpace(pageRe.ReplaceAllString(text, ""))
	if query == "" {
		return RecallRequest{Scope: scope, Action: RecallAction{Kind: ActionRecent}}
	}
	return RecallRequest{Scope: scope, Action: RecallAction{Kind: ActionSearch, Query: query, Page: page}}
}
