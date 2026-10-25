package pivcc

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Ports src/core/search-entries.ts: ranked recall search with BM25-lite
// scoring, a regex fast path, a relative noise floor and a hard cap.

// SearchHit is search-entries.ts SearchHit: a rendered entry plus its match
// context.
type SearchHit struct {
	RenderedEntry
	// Snippet is the context around the first matched term (only with a query).
	Snippet    string
	HasSnippet bool
	// MatchCount is how many query terms matched.
	MatchCount int
}

// SearchResult is search-entries.ts SearchResult.
type SearchResult struct {
	Hits           []SearchHit
	TotalBeforeCap int
	Truncated      bool
}

// FileTouch is search-entries.ts FileTouch.
type FileTouch struct {
	Index    int
	ToolName string
}

// TouchedFile is search-entries.ts TouchedFile.
type TouchedFile struct {
	Path    string
	Entries []FileTouch
}

// quantifierAt is search-entries.ts quantifierAt(): a quantifier starting at
// index i, if any, and whether it is unbounded.
func quantifierAt(pattern string, i int) (length int, unbounded bool) {
	if i >= len(pattern) {
		return 0, false
	}
	switch pattern[i] {
	case '+', '*':
		return 1, true
	case '{':
		end := strings.IndexByte(pattern[i:], '}')
		if end < 0 {
			return 0, false
		}
		end += i
		body := pattern[i+1 : end]
		if braceQuantifierRe.MatchString(body) {
			return end - i + 1, strings.HasSuffix(body, ",")
		}
	}
	return 0, false
}

// braceQuantifierRe is search-entries.ts's `/^\d+(,\d*)?$/`.
var braceQuantifierRe = mustCompile(`^\d+(,\d*)?$`)

// hasNestedQuantifier is search-entries.ts hasNestedQuantifier(): whether an
// unbounded quantifier applies to a group that already contains one, e.g.
// `(a+)+` or `(\w*)*`.
func hasNestedQuantifier(pattern string) bool {
	groups := make([]bool, 0, 4) // per open group: contains an unbounded quantifier
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' {
			i++
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
		case '(':
			groups = append(groups, false)
		case ')':
			inner := false
			if len(groups) > 0 {
				inner = groups[len(groups)-1]
				groups = groups[:len(groups)-1]
			}
			length, unbounded := quantifierAt(pattern, i+1)
			if inner && unbounded {
				return true
			}
			if len(groups) > 0 {
				groups[len(groups)-1] = groups[len(groups)-1] || inner || unbounded
			}
			i += length
		default:
			length, unbounded := quantifierAt(pattern, i)
			if unbounded && len(groups) > 0 {
				groups[len(groups)-1] = true
				i += length - 1
			}
		}
	}
	return false
}

// safeRegex is search-entries.ts safeRegex(): compile as a regex, falling back
// to an escaped literal. A pattern with nested unbounded quantifiers is treated
// as a literal rather than compiled.
func safeRegex(pattern string) *regexp.Regexp {
	if hasNestedQuantifier(pattern) {
		return compileIgnoreCase(escapeRegexp(pattern))
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return compileIgnoreCase(escapeRegexp(pattern))
	}
	return re
}

// searchBudgetMillis is search-entries.ts SEARCH_BUDGET_MS.
const searchBudgetMillis = 3000

// looksLikeRegexRe is search-entries.ts's `/[|*+?{}()[\]\\^$.]/`.
var looksLikeRegexRe = mustCompile(`[|*+?{}()\[\]\\^$.]`)

// looksLikeRegex is search-entries.ts looksLikeRegex().
func looksLikeRegex(query string) bool {
	return looksLikeRegexRe.MatchString(query)
}

// searchStopwords is search-entries.ts STOPWORDS.
var searchStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
	"will": true, "would": true, "could": true,
	"should": true, "may": true, "might": true, "can": true, "shall": true, "of": true, "in": true,
	"to": true, "for": true,
	"with": true, "on": true, "at": true, "from": true, "by": true, "as": true, "into": true,
	"through": true, "during": true,
	"before": true, "after": true, "above": true, "below": true, "between": true, "out": true,
	"off": true, "over": true,
	"under": true, "again": true, "further": true, "then": true, "once": true, "here": true,
	"there": true, "when": true,
	"where": true, "why": true, "how": true, "all": true, "both": true, "each": true, "few": true,
	"more": true, "most": true,
	"other": true, "some": true, "such": true, "no": true, "nor": true, "not": true, "only": true,
	"own": true, "same": true,
	"so": true, "than": true, "too": true, "very": true, "just": true, "about": true, "it": true,
	"its": true, "that": true,
	"this": true, "what": true, "which": true, "who": true, "whom": true, "these": true, "those": true,
}

// filterStopwords is search-entries.ts filterStopwords().
func filterStopwords(terms []string) []string {
	meaningful := make([]string, 0, len(terms))
	for _, term := range terms {
		if searchStopwords[strings.ToLower(term)] || jsLen(term) <= 1 {
			continue
		}
		meaningful = append(meaningful, term)
	}
	if len(meaningful) > 0 {
		return meaningful
	}
	return terms
}

const (
	bm25K = 1.2
	bm25B = 0.75
	// bm25RelativeFloor is search-entries.ts BM25_RELATIVE_FLOOR.
	bm25RelativeFloor = 0.2
	// searchResultCap is search-entries.ts SEARCH_RESULT_CAP.
	searchResultCap = 50
	// toolArgsBudget is search-entries.ts TOOL_ARGS_BUDGET.
	toolArgsBudget = 2000
	// recallToolName is the recall tool's own name, excluded from search.
	recallToolName = "vcc_recall"
)

// term is one compiled query term.
type term struct {
	text   string
	re     *regexp.Regexp
	global *regexp.Regexp
}

func compileTerms(terms []string) []term {
	out := make([]term, 0, len(terms))
	for _, text := range terms {
		re := safeRegex(text)
		out = append(out, term{text: text, re: re, global: compileIgnoreCase(re.String()[len("(?i)"):])})
	}
	return out
}

func (t term) freqOf(text string) int {
	return len(t.global.FindAllStringIndex(text, -1))
}

// wordCount is search-entries.ts wordCount().
func wordCount(doc string) int {
	return len(jsSplitWhitespace(doc))
}

// bm25Context is search-entries.ts BM25Context.
type bm25Context struct {
	n       int
	avgDL   float64
	df      map[string]int
	matched [][]term
}

func buildBM25Context(docs []string, docLens []int, terms []term, checkBudget func() error) (*bm25Context, error) {
	n := len(docs)
	df := make(map[string]int, len(terms))
	matched := make([][]term, n)
	totalLen := 0
	for i := 0; i < n; i++ {
		if err := checkBudget(); err != nil {
			return nil, err
		}
		totalLen += docLens[i]
		here := make([]term, 0, len(terms))
		for _, t := range terms {
			if t.re.MatchString(docs[i]) {
				df[t.text]++
				here = append(here, t)
			}
		}
		matched[i] = here
	}
	avg := 0.0
	if n > 0 {
		avg = float64(totalLen) / float64(n)
	}
	return &bm25Context{n: n, avgDL: avg, df: df, matched: matched}, nil
}

func bm25Score(doc string, dl int, terms []term, ctx *bm25Context) float64 {
	score := 0.0
	for _, t := range terms {
		tf := t.freqOf(doc)
		if tf == 0 {
			continue
		}
		docFreq := ctx.df[t.text]
		idf := jsLog((float64(ctx.n-docFreq)+0.5)/(float64(docFreq)+0.5) + 1)
		lengthNorm := 1 - bm25B + bm25B*float64(dl)/ctx.avgDL
		tfNorm := float64(tf) * (bm25K + 1) / (float64(tf) + bm25K*lengthNorm)
		score += idf * tfNorm
	}
	return score
}

// lineSnippet is search-entries.ts lineSnippet(): ±contextLines around the first
// matching line.
func lineSnippet(text string, re *regexp.Regexp, contextLines int) (string, bool) {
	lines := jsSplitLines(text)
	matchIdx := -1
	for i, line := range lines {
		if re.MatchString(line) {
			matchIdx = i
			break
		}
	}
	if matchIdx == -1 {
		return "", false
	}
	start := matchIdx - contextLines
	if start < 0 {
		start = 0
	}
	end := matchIdx + contextLines + 1
	if end > len(lines) {
		end = len(lines)
	}
	parts := make([]string, 0, end-start+2)
	if start > 0 {
		parts = append(parts, "...("+jsNumberString(float64(start))+" lines above)")
	}
	parts = append(parts, lines[start:end]...)
	if end < len(lines) {
		parts = append(parts, "...("+jsNumberString(float64(len(lines)-end))+" lines below)")
	}
	return strings.Join(parts, "\n"), true
}

// toolCallArgsText is search-entries.ts toolCallArgsText(): every toolCall's
// argument text, bounded once in aggregate and excluding the recall tool's own
// arguments.
func toolCallArgsText(content any) string {
	parts := toolCallParts(content)
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		name, _ := part["name"].(string)
		if strings.ToLower(name) == recallToolName {
			continue
		}
		args, _ := part["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if text := ExtractToolCallArgsText(args); text != "" {
			texts = append(texts, text)
		}
	}
	return Clip(strings.Join(texts, "\n"), toolArgsBudget)
}

// fullText is search-entries.ts fullText(): the searchable text of a message.
func fullText(msg map[string]any) string {
	if messageRole(msg) == "bashExecution" {
		command, _ := msg["command"].(string)
		output, _ := msg["output"].(string)
		return command + " " + output
	}
	if messageRole(msg) == "toolResult" {
		toolName, _ := msg["toolName"].(string)
		if strings.ToLower(toolName) == recallToolName {
			return ""
		}
	}
	text := TextOf(msg["content"])
	argsText := toolCallArgsText(msg["content"])
	if argsText != "" {
		return text + "\n" + argsText
	}
	return text
}

// FileIndicator is search-entries.ts's getFileIndicators() element: one
// content-bearing file call of a message plus its non-empty content line count.
type FileIndicator struct {
	ToolName  string
	Path      string
	LineCount int
}

// GetFileIndicators is search-entries.ts getFileIndicators(): the file calls of
// one message with their non-empty content line counts.
func GetFileIndicators(msg map[string]any) []FileIndicator {
	out := make([]FileIndicator, 0, 2)
	parts := toolCallParts(msg["content"])
	for _, part := range parts {
		args, _ := part["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if !IsContentBearing(args) {
			continue
		}
		path := ""
		for _, key := range []string{"path", "filePath", "file_path", "file"} {
			if value, ok := args[key].(string); ok {
				path = value
				break
			}
		}
		totalText := ExtractToolCallText(args)
		nonEmpty := 0
		for _, line := range jsSplitLines(totalText) {
			if jsTrimSpace(line) != "" {
				nonEmpty++
			}
		}
		name, _ := part["name"].(string)
		out = append(out, FileIndicator{ToolName: name, Path: path, LineCount: nonEmpty})
	}
	return out
}

// GetTouchedFiles is search-entries.ts getTouchedFiles(): file operations
// aggregated across entries for mode:touched.
func GetTouchedFiles(messages []map[string]any, rendered []RenderedEntry) []TouchedFile {
	order := make([]string, 0, len(messages))
	byPath := make(map[string]*TouchedFile, len(messages))
	for i, msg := range messages {
		for _, fm := range GetFileIndicators(msg) {
			index := i
			if i < len(rendered) {
				index = rendered[i].Index
			}
			file, present := byPath[fm.Path]
			if !present {
				file = &TouchedFile{Path: fm.Path}
				byPath[fm.Path] = file
				order = append(order, fm.Path)
			}
			file.Entries = append(file.Entries, FileTouch{Index: index, ToolName: fm.ToolName})
		}
	}
	out := make([]TouchedFile, 0, len(order))
	for _, path := range order {
		out = append(out, *byPath[path])
	}
	return out
}

// capHits is search-entries.ts capHits(): bound hits to cap, reporting the
// pre-cap count so truncation is reported honestly.
func capHits(hits []SearchHit, cap int) SearchResult {
	total := len(hits)
	capped := hits
	if total > cap {
		capped = hits[:cap]
	}
	return SearchResult{Hits: capped, TotalBeforeCap: total, Truncated: len(capped) < total}
}

// SearchTuning is search-entries.ts SearchTuning.
type SearchTuning struct {
	RelativeFloor float64
	Cap           int
}

// SearchEntriesDetailed is search-entries.ts searchEntriesDetailed().
func SearchEntriesDetailed(entries []RenderedEntry, messages []map[string]any, query string, tuning SearchTuning) (SearchResult, error) {
	if jsTrimSpace(query) == "" {
		return SearchResult{Hits: renderAll(entries), TotalBeforeCap: len(entries)}, nil
	}
	relativeFloor := tuning.RelativeFloor
	if relativeFloor == 0 {
		relativeFloor = bm25RelativeFloor
	}
	cap := tuning.Cap
	if cap == 0 {
		cap = searchResultCap
	}
	rawQuery := jsTrimSpace(query)
	deadline := nowMillis() + searchBudgetMillis
	nudges := 0
	checkBudget := func() error {
		nudges++
		// Checking the clock per entry would dominate the scan, so the
		// deadline is sampled every 64 entries (a documented deviation).
		if nudges%64 != 0 {
			return nil
		}
		if nowMillis() > deadline {
			return fmt.Errorf("Search aborted: query exceeded %dms. Use fewer or more specific words; "+
				"for a regex, avoid nested quantifiers such as (a+)+.", searchBudgetMillis)
		}
		return nil
	}

	if looksLikeRegex(rawQuery) {
		re := safeRegex(rawQuery)
		hits := make([]SearchHit, 0, len(entries))
		for i, entry := range entries {
			if err := checkBudget(); err != nil {
				return SearchResult{}, err
			}
			text := entry.Summary
			if i < len(messages) && messages[i] != nil {
				text = fullText(messages[i])
			}
			filePart := strings.Join(entry.Files, " ")
			hay := entry.Role + " " + text + " " + filePart
			if re.MatchString(hay) {
				hit := SearchHit{RenderedEntry: entry, MatchCount: 1}
				if snip, ok := lineSnippet(text, re, 2); ok {
					hit.Snippet = snip
					hit.HasSnippet = true
				}
				hits = append(hits, hit)
			}
		}
		if len(hits) > 0 {
			return capHits(hits, cap), nil
		}
	}

	rawTerms := jsSplitWhitespace(rawQuery)
	terms := filterStopwords(rawTerms)
	snipRe := snippetRegex(terms)

	docs := make([]string, 0, len(entries))
	for i, entry := range entries {
		text := entry.Summary
		if i < len(messages) && messages[i] != nil {
			text = fullText(messages[i])
		}
		filePart := strings.Join(entry.Files, " ")
		docs = append(docs, entry.Role+" "+text+" "+filePart)
	}

	compiled := compileTerms(terms)
	docLens := make([]int, len(docs))
	for i, doc := range docs {
		docLens[i] = wordCount(doc)
	}
	ctx, err := buildBM25Context(docs, docLens, compiled, checkBudget)
	if err != nil {
		return SearchResult{}, err
	}

	scored := make([]scoredHit, 0, len(entries))
	for i, entry := range entries {
		if err := checkBudget(); err != nil {
			return SearchResult{}, err
		}
		here := ctx.matched[i]
		if len(here) == 0 {
			continue
		}
		score := bm25Score(docs[i], docLens[i], here, ctx)
		text := entry.Summary
		if i < len(messages) && messages[i] != nil {
			text = fullText(messages[i])
		}
		hit := SearchHit{RenderedEntry: entry, MatchCount: len(here)}
		if snip, ok := lineSnippet(text, snipRe, 2); ok {
			hit.Snippet = snip
			hit.HasSnippet = true
		}
		scored = append(scored, scoredHit{hit: hit, score: score})
	}

	stableSortByScore(scored)

	effective := make(map[string]bool, len(terms))
	for _, t := range terms {
		effective[strings.ToLower(t)] = true
	}
	filtered := scored
	if len(effective) >= 2 {
		filtered = applyRelativeFloor(scored, relativeFloor)
	}
	hits := make([]SearchHit, 0, len(filtered))
	for _, item := range filtered {
		hits = append(hits, item.hit)
	}
	return capHits(hits, cap), nil
}

// renderAll turns entries into hits with no query.
func renderAll(entries []RenderedEntry) []SearchHit {
	out := make([]SearchHit, 0, len(entries))
	for _, entry := range entries {
		out = append(out, SearchHit{RenderedEntry: entry})
	}
	return out
}

// SearchEntries is search-entries.ts searchEntries(): the hits only.
func SearchEntries(entries []RenderedEntry, messages []map[string]any, query string) []SearchHit {
	result, err := SearchEntriesDetailed(entries, messages, query, SearchTuning{})
	if err != nil {
		return nil
	}
	return result.Hits
}

// applyRelativeFloor is search-entries.ts applyRelativeFloor().
func applyRelativeFloor(scored []scoredHit, floor float64) []scoredHit {
	if len(scored) == 0 {
		return scored
	}
	top := scored[0].score
	if top <= 0 {
		return scored
	}
	out := make([]scoredHit, 0, len(scored))
	for _, item := range scored {
		if item.score >= top*floor {
			out = append(out, item)
		}
	}
	return out
}

// snippetRegex is search-entries.ts snippetRegex(): any term, case-insensitive.
func snippetRegex(terms []string) *regexp.Regexp {
	alternatives := make([]string, 0, len(terms))
	for _, t := range terms {
		source := safeRegex(t).String()
		alternatives = append(alternatives, strings.TrimPrefix(source, "(?i)"))
	}
	re, err := regexp.Compile("(?i)" + strings.Join(alternatives, "|"))
	if err != nil {
		return compileIgnoreCase(escapeRegexp(strings.Join(terms, " ")))
	}
	return re
}

// utf8Len is the rune count, used where the TS code counts characters.
func utf8Len(s string) int { return utf8.RuneCountInString(s) }
