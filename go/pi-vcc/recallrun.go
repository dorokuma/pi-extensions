package pivcc

import "strings"

// Ports src/core/recall-run.ts: the session view a recall call sees and the one
// action it runs.

const (
	// SearchPageSize is recall-run.ts SEARCH_PAGE_SIZE.
	SearchPageSize = 5
	// RangePageSize is recall-run.ts RANGE_PAGE_SIZE.
	RangePageSize = 20
	// RecentCount is recall-run.ts RECENT_COUNT.
	RecentCount = 25
	// PairedResultChars is recall-run.ts PAIRED_RESULT_CHARS: the clip for a tool
	// result shown alongside an expanded call.
	PairedResultChars = 4000
)

// offPathNote is recall-run.ts OFF_PATH_NOTE.
const offPathNote = "Nothing on the current conversation path; showing edited or retried branches (scope:'all').\n\n"

// RecallView is recall-run.ts RecallView: the session as one call sees it.
type RecallView struct {
	SessionFile  string
	Scope        RecallScope
	manager      LineageSessionManager
	lineageIDs   map[string]bool
	fullCache    *LoadedMessages
	partialCache *LoadedMessages
	wide         *RecallView
	turn         map[int]bool
	turnComputed bool
}

// OpenRecallView is recall-run.ts openRecallView().
func OpenRecallView(sessionFile string, scope RecallScope, manager LineageSessionManager) *RecallView {
	view := &RecallView{SessionFile: sessionFile, Scope: scope, manager: manager}
	if scope == ScopeLineage {
		view.lineageIDs = GetActiveLineageEntryIDs(manager)
	}
	return view
}

// Load is recall-run.ts RecallView.load(): the loaded (and cached) messages.
func (v *RecallView) Load(full bool) *LoadedMessages {
	if full {
		if v.fullCache == nil {
			loaded := LoadAllMessages(v.SessionFile, true, v.lineageIDs)
			v.fullCache = &loaded
		}
		return v.fullCache
	}
	if v.partialCache == nil {
		loaded := LoadAllMessages(v.SessionFile, false, v.lineageIDs)
		v.partialCache = &loaded
	}
	return v.partialCache
}

// Widen is recall-run.ts RecallView.widen(): the same session with scope:'all'.
func (v *RecallView) Widen() *RecallView {
	if v.Scope == ScopeAll {
		return v
	}
	if v.wide == nil {
		v.wide = OpenRecallView(v.SessionFile, ScopeAll, v.manager)
	}
	return v.wide
}

// CurrentTurn is recall-run.ts RecallView.currentTurn(): the `#N` of the turn in
// progress on the active path.
func (v *RecallView) CurrentTurn() map[int]bool {
	if v.turnComputed {
		return v.turn
	}
	v.turnComputed = true
	v.turn = findCurrentTurn(v.SessionFile, v.manager)
	return v.turn
}

// findCurrentTurn is recall-run.ts findCurrentTurn().
func findCurrentTurn(sessionFile string, manager LineageSessionManager) map[int]bool {
	none := map[int]bool{}
	var branch []LineageEntryLike
	if manager != nil {
		if entries, err := manager.GetBranch(); err == nil {
			branch = entries
		}
	}
	for i := len(branch) - 1; i >= 0; i-- {
		entry := branch[i]
		kind, _ := entry["type"].(string)
		if kind == "compaction" {
			return none
		}
		if kind == "message" {
			message, _ := entry["message"].(map[string]any)
			if messageRole(message) == "user" {
				break
			}
		}
	}
	loaded := LoadAllMessages(sessionFile, false, GetActiveLineageEntryIDs(manager))
	for i := len(loaded.Rendered) - 1; i >= 0; i-- {
		if loaded.Rendered[i].Role != "user" {
			continue
		}
		if i < len(loaded.Rendered)-1 {
			turn := make(map[int]bool, len(loaded.Rendered)-i)
			for _, entry := range loaded.Rendered[i:] {
				turn[entry.Index] = true
			}
			return turn
		}
		return none
	}
	return none
}

// InvalidExpandIndices is recall-run.ts invalidExpandIndices().
func InvalidExpandIndices(requested []int, available map[int]bool) []int {
	out := make([]int, 0, len(requested))
	for _, index := range requested {
		if !available[index] {
			out = append(out, index)
		}
	}
	return out
}

// RecallPagingHints is recall-run.ts RecallPagingHints: surface-specific
// wording (the tool says page:N, the command says /pi-vcc-recall … page:N).
type RecallPagingHints struct {
	OutOfRange func(query string, scope RecallScope, totalPages int, truncated bool) string
	NextPage   func(query string, scope RecallScope, page int) string
	// AroundHit points at the first hit's context; the command has none.
	AroundHit func(index int, scope RecallScope) string
	// SkipCurrentTurn leaves the turn in progress out of search (tool only).
	SkipCurrentTurn bool
}

// ignoredReason is recall-run.ts IGNORED_REASON.
var ignoredReason = map[RecallActionKind]string{
	ActionDrill:   "a #N:path query runs alone",
	ActionTouched: "mode:'touched' runs alone",
	ActionExpand:  "expand runs alone; send the rest in a separate call",
	ActionRange:   "range runs alone; send the rest in a separate call",
	ActionSearch:  "",
	ActionRecent:  "page applies to query or range results",
}

// IgnoredNote is recall-run.ts ignoredNote(): one line naming unused params.
func IgnoredNote(request RecallRequest) string {
	var b strings.Builder
	if len(request.Dropped) > 0 {
		b.WriteString("Ignored: " + strings.Join(request.Dropped, ", ") + ".\n\n")
	}
	if len(request.Ignored) > 0 {
		b.WriteString("Ignored: " + strings.Join(request.Ignored, ", ") + " (" + ignoredReason[request.Action.Kind] + ").\n\n")
	}
	return b.String()
}

// RunRecall is recall-run.ts runRecall().
func RunRecall(request RecallRequest, view *RecallView, hints RecallPagingHints) string {
	return IgnoredNote(request) + runAction(request, view, hints)
}

func runAction(request RecallRequest, view *RecallView, hints RecallPagingHints) string {
	action := request.Action
	scopeAll := view.Scope == ScopeAll

	switch action.Kind {
	case ActionDrill:
		// expandEntryFile loads unfiltered so #N stays aligned with the global
		// message index; scope only decides whether the entry is off-path.
		t := action.Drill
		offPath := false
		if !scopeAll {
			found := false
			for _, entry := range view.Load(false).Rendered {
				if entry.Index == t.Index {
					found = true
					break
				}
			}
			offPath = !found
		}
		text := ExpandEntryFile(view.SessionFile, t.Index, t.PathPattern, t.Full, t.Offset, t.HasOffset, t.Limit, t.HasLimit)
		if offPath {
			return offPathNote + text
		}
		return text

	case ActionTouched:
		return runTouched(action.Page, view)

	case ActionExpand:
		return runExpand(action.Indices, view)

	case ActionRange:
		return runRange(action.Range, action.Page, view)

	case ActionSearch:
		var skip map[int]bool
		if hints.SkipCurrentTurn {
			skip = view.CurrentTurn()
		}
		return runSearch(action.Query, action.Page, view, hints, skip)

	case ActionRecent:
		rendered := view.Load(false).Rendered
		recent := rendered
		if len(recent) > RecentCount {
			recent = recent[len(recent)-RecentCount:]
		}
		prefix := ""
		if scopeAll {
			prefix = "Scope: all\n\n"
		}
		return prefix + FormatRecallOutput(hitsOf(recent), action.Query, "", false, false)
	}
	return ""
}

func hitsOf(entries []RenderedEntry) []SearchHit {
	out := make([]SearchHit, 0, len(entries))
	for _, entry := range entries {
		out = append(out, SearchHit{RenderedEntry: entry})
	}
	return out
}

// toolCallIDs is recall-run.ts toolCallIds(): the ids of a message's tool calls.
func toolCallIDs(msg map[string]any) []string {
	if messageRole(msg) != "assistant" {
		return nil
	}
	parts := toolCallParts(msg["content"])
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if id, ok := part["id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

// summaryOf is `res.summary` for a hit.
func summaryOf(hit SearchHit) string {
	return hit.Summary
}

// runExpand is recall-run.ts runExpand(): the full text of the requested
// entries, plus the tool results of an expanded tool call (matched by
// toolCallId, clipped to PairedResultChars).
func runExpand(indices []int, view *RecallView) string {
	pools := []*RecallView{view}
	if view.Scope != ScopeAll {
		pools = []*RecallView{view, view.Widen()}
	}

	type located struct {
		entry RenderedEntry
		raw   map[string]any
		v     *RecallView
		pos   int
	}
	lookup := func(index int) (located, bool) {
		for _, pool := range pools {
			loaded := pool.Load(true)
			for pos, entry := range loaded.Rendered {
				if entry.Index == index {
					return located{entry: entry, raw: loaded.RawMessages[pos], v: pool, pos: pos}, true
				}
			}
		}
		return located{}, false
	}

	found := make([]struct {
		index int
		hit   located
		ok    bool
	}, 0, len(indices))
	invalid := make([]string, 0, len(indices))
	for _, index := range indices {
		hit, ok := lookup(index)
		if !ok {
			invalid = append(invalid, jsNumberString(float64(index)))
		}
		found = append(found, struct {
			index int
			hit   located
			ok    bool
		}{index: index, hit: hit, ok: ok})
	}
	if len(invalid) > 0 {
		return "Cannot expand indices outside session history: " + strings.Join(invalid, ", ")
	}

	offPath := make([]string, 0, 2)
	for _, item := range found {
		if item.hit.v != view {
			offPath = append(offPath, "#"+jsNumberString(float64(item.index)))
		}
	}
	requested := make(map[int]bool, len(indices))
	for _, index := range indices {
		requested[index] = true
	}

	out := make([]SearchHit, 0, len(found))
	for _, item := range found {
		entry := item.hit.entry
		out = append(out, SearchHit{RenderedEntry: entry})
		ids := make(map[string]bool)
		for _, id := range toolCallIDs(item.hit.raw) {
			ids[id] = true
		}
		if len(ids) == 0 {
			continue
		}
		loaded := item.hit.v.Load(true)
		for j := item.hit.pos + 1; j < len(loaded.Rendered) && len(ids) > 0; j++ {
			raw := loaded.RawMessages[j]
			// A call's results come before the next assistant message; past it,
			// a reused id belongs to a later call.
			if messageRole(raw) == "assistant" {
				break
			}
			if messageRole(raw) != "toolResult" {
				continue
			}
			toolCallID, _ := raw["toolCallId"].(string)
			if !ids[toolCallID] {
				continue
			}
			delete(ids, toolCallID)
			res := loaded.Rendered[j]
			if requested[res.Index] {
				continue
			}
			summary := summaryOf(SearchHit{RenderedEntry: res})
			if jsLen(summary) > PairedResultChars {
				summary = jsSlice(summary, 0, PairedResultChars) + "\n...[result of #" +
					jsNumberString(float64(entry.Index)) + " clipped at " + jsNumberString(float64(PairedResultChars)) +
					" of " + jsNumberString(float64(jsLen(summary))) + " chars; expand:[" +
					jsNumberString(float64(res.Index)) + "] for all of it]"
			}
			clipped := res
			clipped.Summary = summary
			out = append(out, SearchHit{RenderedEntry: clipped})
		}
	}

	label := make([]string, 0, len(indices))
	for _, index := range indices {
		label = append(label, "#"+jsNumberString(float64(index)))
	}
	header := "Expanded " + strings.Join(label, ", ")
	note := ""
	if len(offPath) > 0 {
		verb := "are"
		if len(offPath) == 1 {
			verb = "is"
		}
		note = strings.Join(offPath, ", ") + " " + verb + " not on the current conversation path (edited or retried branch).\n\n"
	}
	prefix := ""
	if view.Scope == ScopeAll {
		prefix = "Scope: all\n\n"
	}
	return prefix + note + FormatRecallOutput(out, "", header, false, true)
}

// runRange is recall-run.ts runRange(): entries #from..#to in order,
// RangePageSize per page.
func runRange(rangePair []int, page int, view *RecallView) string {
	if len(rangePair) != 2 {
		return "Invalid range " + jsonLiteral(intSliceToAny(rangePair)) +
			": use range:[from, to] with two #N indices, from <= to."
	}
	from, to := rangePair[0], rangePair[1]
	if from < 0 || from > to {
		return "Invalid range " + jsonLiteral(intSliceToAny(rangePair)) +
			": use range:[from, to] with two #N indices, from <= to."
	}
	hi := to
	scopeAll := view.Scope == ScopeAll
	rendered := view.Load(false).Rendered
	entries := make([]RenderedEntry, 0, len(rendered))
	for _, entry := range rendered {
		if entry.Index >= from && entry.Index <= hi {
			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		if !scopeAll {
			wide := view.Widen()
			for _, entry := range wide.Load(false).Rendered {
				if entry.Index >= from && entry.Index <= hi {
					return offPathNote + runRange(rangePair, page, wide)
				}
			}
		}
		suffix := ""
		if len(rendered) > 0 {
			suffix = " (last entry is #" + jsNumberString(float64(rendered[len(rendered)-1].Index)) + ")"
		}
		return "No messages #" + jsNumberString(float64(from)) + "..#" + jsNumberString(float64(hi)) +
			" in session history" + suffix + "."
	}

	// A range past the end is read up to the last entry, and the header says so.
	lastIndex := rendered[len(rendered)-1].Index
	clamped := hi > lastIndex
	if clamped {
		hi = lastIndex
	}

	totalPages := ceilDiv(len(entries), RangePageSize)
	counts := jsNumberString(float64(len(entries))) + " messages"
	if scopeAll {
		counts += ", scope: all"
	}
	if clamped {
		counts += ", #" + jsNumberString(float64(hi)) + " is the last entry"
	}
	if page > totalPages {
		return "Page " + jsNumberString(float64(page)) + " is outside the available range 1-" +
			jsNumberString(float64(totalPages)) + " (#" + jsNumberString(float64(from)) + "..#" +
			jsNumberString(float64(hi)) + ": " + counts + "). Use a page between 1 and " +
			jsNumberString(float64(totalPages)) + "."
	}
	start := (page - 1) * RangePageSize
	end := start + RangePageSize
	if end > len(entries) {
		end = len(entries)
	}
	header := "Range #" + jsNumberString(float64(from)) + "..#" + jsNumberString(float64(hi)) + " (" + counts + ")"
	if totalPages > 1 {
		header = "Range #" + jsNumberString(float64(from)) + "..#" + jsNumberString(float64(hi)) + ", page " +
			jsNumberString(float64(page)) + "/" + jsNumberString(float64(totalPages)) + " (" + counts + ")"
	}
	footer := ""
	if page < totalPages {
		scopeSuffix := ""
		if scopeAll {
			scopeSuffix = " scope:'all'"
		}
		footer = "\n--- Use range:[" + jsNumberString(float64(from)) + ", " + jsNumberString(float64(hi)) +
			"] page:" + jsNumberString(float64(page+1)) + scopeSuffix + " for the next " +
			jsNumberString(float64(RangePageSize)) + " ---"
	}
	return FormatRecallOutput(hitsOf(entries[start:end]), "", header, false, true) + footer
}

func intSliceToAny(values []int) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, float64(value))
	}
	return out
}

// searchBefore is recall-run.ts searchBefore(): search with the current turn
// left out of the corpus, not just out of the results.
func searchBefore(view *RecallView, query string, skip map[int]bool) (SearchResult, error) {
	loaded := view.Load(false)
	if len(skip) == 0 {
		return SearchEntriesDetailed(loaded.Rendered, loaded.RawMessages, query, SearchTuning{})
	}
	entries := make([]RenderedEntry, 0, len(loaded.Rendered))
	messages := make([]map[string]any, 0, len(loaded.RawMessages))
	for i, entry := range loaded.Rendered {
		if skip[entry.Index] {
			continue
		}
		entries = append(entries, entry)
		messages = append(messages, loaded.RawMessages[i])
	}
	return SearchEntriesDetailed(entries, messages, query, SearchTuning{})
}

// runSearch is recall-run.ts runSearch().
func runSearch(query string, page int, view *RecallView, hints RecallPagingHints, skip map[int]bool) string {
	result, err := searchBefore(view, query, skip)
	if err != nil {
		return err.Error()
	}
	if len(result.Hits) == 0 && view.Scope == ScopeLineage {
		wide := view.Widen()
		wideResult, wideErr := searchBefore(wide, query, skip)
		if wideErr != nil {
			return wideErr.Error()
		}
		if len(wideResult.Hits) > 0 {
			return offPathNote + runSearch(query, page, wide, hints, skip)
		}
	}
	if len(result.Hits) == 0 && len(skip) > 0 {
		loaded := view.Load(false)
		inTurn := 0
		if all, err := SearchEntriesDetailed(loaded.Rendered, loaded.RawMessages, query, SearchTuning{}); err == nil {
			for _, hit := range all.Hits {
				if skip[hit.Index] {
					inTurn++
				}
			}
		}
		if inTurn > 0 {
			verb := "es are"
			if inTurn == 1 {
				verb = " is"
			}
			return "No earlier matches for \"" + query + "\"; its " + jsNumberString(float64(inTurn)) + " match" +
				verb + " in the current turn, which is already in your context."
		}
	}

	// Single source of truth for page count: hits.length.
	totalPages := ceilDiv(len(result.Hits), SearchPageSize)
	scopeSuffix := ""
	if view.Scope == ScopeAll {
		scopeSuffix = " (scope: all)"
	}
	truncationNote := ""
	if result.Truncated {
		truncationNote = " — showing " + jsNumberString(float64(len(result.Hits))) + " of " +
			jsNumberString(float64(result.TotalBeforeCap)) + " matches, refine your query for more precise results"
	}

	if len(result.Hits) > 0 && page > totalPages {
		return "Page " + jsNumberString(float64(page)) + " is outside the available range 1-" +
			jsNumberString(float64(totalPages)) + " (" + jsNumberString(float64(len(result.Hits))) + " matches" +
			scopeSuffix + truncationNote + "). " + hints.OutOfRange(query, view.Scope, totalPages, result.Truncated)
	}

	start := (page - 1) * SearchPageSize
	if start > len(result.Hits) {
		start = len(result.Hits)
	}
	end := start + SearchPageSize
	if end > len(result.Hits) {
		end = len(result.Hits)
	}
	pageHits := result.Hits[start:end]
	header := jsNumberString(float64(len(result.Hits))) + " matches" + scopeSuffix + truncationNote
	if totalPages > 1 {
		header = "Page " + jsNumberString(float64(page)) + "/" + jsNumberString(float64(totalPages)) + " (" +
			jsNumberString(float64(len(result.Hits))) + " total matches" + scopeSuffix + truncationNote + ")"
	}
	around := ""
	if len(pageHits) > 0 && hints.AroundHit != nil {
		around = hints.AroundHit(pageHits[0].Index, view.Scope)
	}
	footer := ""
	if page < totalPages {
		footer = hints.NextPage(query, view.Scope, page+1)
	}
	return FormatRecallOutput(pageHits, query, header, true, true) + around + footer
}

// writesFileRe is recall-run.ts WRITES_FILE.
var writesFileRe = compileIgnoreCase(`write|edit|patch`)

// runTouched is recall-run.ts runTouched(): files worked on, plus a ready
// #N:path call for the first file on the page that was written.
func runTouched(page int, view *RecallView) string {
	loaded := view.Load(false)
	touched := GetTouchedFiles(loaded.RawMessages, loaded.Rendered)
	text := FormatTouchedOutput(touched, page, TouchedPageSize, "")
	currentPage := page
	if currentPage < 1 {
		currentPage = 1
	}
	start := (currentPage - 1) * TouchedPageSize
	if start > len(touched) {
		start = len(touched)
	}
	end := start + TouchedPageSize
	if end > len(touched) {
		end = len(touched)
	}
	for _, file := range touched[start:end] {
		var write *FileTouch
		for i := len(file.Entries) - 1; i >= 0; i-- {
			if writesFileRe.MatchString(file.Entries[i].ToolName) {
				write = &file.Entries[i]
				break
			}
		}
		if write == nil {
			continue
		}
		normalized := strings.ReplaceAll(file.Path, "\\", "/")
		parts := strings.Split(normalized, "/")
		name := parts[len(parts)-1]
		return text + "\n--- File content at an entry: query:'#" + jsNumberString(float64(write.Index)) + ":" + name + "' ---"
	}
	return text
}
