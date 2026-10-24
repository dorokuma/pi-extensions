package ctxorchestrate

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// ctx_search_digest: the composite ctxmode tool. It runs several ctx_fs rg
// searches concurrently, dedupes hits across patterns, aggregates them per file
// and returns one summary plus a structured result (outputSchema →
// structuredContent). Nested calls go through ctx.executeTool, so their output
// never reaches the transcript.

const (
	// hitsDefault / hitsHard are the default and hard per-pattern hit budgets. TS: HITS_DEFAULT / HITS_HARD.
	hitsDefault = 50
	hitsHard    = 500
	// concurrency bounds how many nested rg calls are in flight at once. TS: CONCURRENCY.
	concurrency = 4
	// maxContextLines is the largest context window forwarded to ctx_fs rg. TS: Math.min(..., 5).
	maxContextLines = 5
	// textFiles / textHits bound the model-facing digest text. TS: files.slice(0, 40), hits.slice(0, 5).
	textFiles = 40
	textHits  = 5
)

// RgHit is one hit line of a ctx_fs rg output.
type RgHit struct {
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

// RgFile groups the hits of one file.
type RgFile struct {
	Path string  `json:"path"`
	Hits []RgHit `json:"hits"`
}

// RgParse is the parsed form of one ctx_fs rg output. TS: RgParse.
type RgParse struct {
	// Files are the hits per file, in the order the output first named each file.
	Files []RgFile `json:"files"`
	// ReportedMatches is the `matches=N` the engine reported on its first line, or 0 when absent.
	ReportedMatches int `json:"-"`
	// Truncated is set when the engine said its output was cut (limit or capture cap).
	Truncated bool `json:"-"`
}

// SearchDigestHit is one aggregated hit: a file:line seen by one or more patterns.
type SearchDigestHit struct {
	Line     int      `json:"line"`
	Excerpt  string   `json:"excerpt"`
	Patterns []string `json:"patterns"`
}

// SearchDigestFile is the aggregated hits of one file.
type SearchDigestFile struct {
	Path string            `json:"path"`
	Hits []SearchDigestHit `json:"hits"`
}

// SearchDigestError is one pattern that failed or degraded.
type SearchDigestError struct {
	Pattern string `json:"pattern"`
	Error   string `json:"error"`
}

// SearchDigestOutput is the tool's structuredContent: the machine-readable
// digest a codemode script reads instead of the model-facing text.
type SearchDigestOutput struct {
	Patterns []string            `json:"patterns"`
	Files    []SearchDigestFile  `json:"files"`
	Summary  string              `json:"summary"`
	Errors   []SearchDigestError `json:"errors,omitempty"`
}

var (
	// reEngineHeader matches ctx_fs rg's first line (`engine=rg matches=7 files=2 ...`).
	reEngineHeader = regexp.MustCompile(`^engine=`)
	// reReadHint matches the `→ Read <path> (27KB - ...)` tip line.
	reReadHint = regexp.MustCompile(`^→` + jsWSClass + `*Read` + jsWSClass + `+`)
	// reCountsLine matches the `7 matches in 2 files (...)` line.
	reCountsLine = regexp.MustCompile(`^\d+ matches in \d+ files?`)
	// reFilesHeader matches the `Files (M=modified, ...)` header line.
	reFilesHeader = regexp.MustCompile(`^Files \(`)
	// reMoreMarker matches a `... (+2 more)` marker line.
	reMoreMarker = regexp.MustCompile(`^\.\.\. \(`)
	// reHitLine matches an indented `  55: excerpt` hit line.
	reHitLine = regexp.MustCompile(`^` + jsWSClass + `+(\d+):` + jsWSClass + `?(.*)$`)
	// reFileHeader matches a `path 4 matches (...)` file header line.
	reFileHeader = regexp.MustCompile(`^(\S.*?)\s+(\d+)\s+matches?\b`)
	// reBareHit matches a bare `file:line:content` line.
	reBareHit = regexp.MustCompile(`^([^` + jsWSInner + `():]+):(\d+):(.*)$`)
	// reNonWSStart is JavaScript's /^\S/: the line's first character is not whitespace.
	reNonWSStart = regexp.MustCompile(`^[^` + jsWSInner + `]`)
	// reReportedMatches / reTruncatedFlag read the engine's self-report on the first line.
	reReportedMatches = regexp.MustCompile(`\bmatches=(\d+)`)
	reTruncatedFlag   = regexp.MustCompile(`\btruncated=true\b`)
	// reCaptureTruncated is the engine's other way of saying the capture was cut.
	reCaptureTruncated = regexp.MustCompile(`(?i)capture truncated`)
)

// ParseRgOutput parses a ctx_fs rg output into per-file hits. It reads the
// grouped shape ctxmode emits
//
//	engine=rg matches=7 files=2 limit=3 truncated=false git=none indexed=...
//	→ Read .pi/agent/SYSTEM.md (27KB - use offset to read relevant section)
//	7 matches in 2 files (full set indexed, > first-screen limit 3). Retrieve details: ...
//	Files (M=modified, A=added, ??=untracked; then by match count):
//	.pi/agent/SYSTEM.md 4 matches (27KB - use offset to read relevant section)
//	  55: **codemode 灰度规则**
//	  ... (+2 more)
//
// and the bare `file:line:content` shape. maxPerFile caps the hits kept per file.
// TS: parseRgOutput.
func ParseRgOutput(text string, maxPerFile int) RgParse {
	result := RgParse{Files: []RgFile{}}
	if text == "" {
		return result
	}
	index := map[string]int{} // path → position in result.Files
	files := []RgFile{}
	var current = -1 // index of the file the last header named, -1 for none

	lines := strings.Split(text, "\n")
	head := ""
	if len(lines) > 0 {
		head = lines[0]
	}
	if m := reReportedMatches.FindStringSubmatch(head); m != nil {
		result.ReportedMatches = atoi(m[1])
	}
	if reTruncatedFlag.MatchString(head) || reCaptureTruncated.MatchString(text) {
		result.Truncated = true
	}

	for _, raw := range lines {
		line := reTrailingWS.ReplaceAllString(raw, "")
		if line == "" {
			continue
		}
		if reEngineHeader.MatchString(line) || reReadHint.MatchString(line) {
			continue
		}
		if reCountsLine.MatchString(line) || reFilesHeader.MatchString(line) || reMoreMarker.MatchString(line) {
			continue
		}

		// A hit line: `  55: excerpt` under the file header it belongs to.
		if hit := reHitLine.FindStringSubmatch(line); hit != nil && current >= 0 {
			if len(files[current].Hits) < maxPerFile {
				files[current].Hits = append(files[current].Hits, RgHit{Line: atoi(hit[1]), Excerpt: oneLine(hit[2], ExcerptCap)})
			}
			continue
		}

		// A line with no leading whitespace is either a file header or a bare hit.
		if reNonWSStart.MatchString(line) {
			if head2 := reFileHeader.FindStringSubmatch(line); head2 != nil {
				p := strings.TrimSpace(head2[1])
				if at, ok := index[p]; ok {
					current = at
				} else {
					files = append(files, RgFile{Path: p, Hits: []RgHit{}})
					index[p] = len(files) - 1
					current = len(files) - 1
				}
				continue
			}
			if bare := reBareHit.FindStringSubmatch(line); bare != nil {
				p := bare[1]
				at, ok := index[p]
				if !ok {
					files = append(files, RgFile{Path: p, Hits: []RgHit{}})
					index[p] = len(files) - 1
					at = len(files) - 1
				}
				current = at
				if len(files[at].Hits) < maxPerFile {
					files[at].Hits = append(files[at].Hits, RgHit{Line: atoi(bare[2]), Excerpt: oneLine(bare[3], ExcerptCap)})
				}
			}
		}
	}
	result.Files = files
	return result
}

// atoi parses a run of ASCII digits matched by one of the regexes above.
func atoi(digits string) int {
	n := 0
	for _, c := range []byte(digits) {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// nestedOutcome is one nested tool call's normalized result. TS: NestedOutcome.
type nestedOutcome struct {
	ok      bool
	text    string
	isError bool
	err     string
}

// nestedRunner runs one nested ctx_fs call. Production passes ctx.ExecuteTool;
// tests pass a fake so no real rg and no external binary is ever needed.
type nestedRunner func(args map[string]any) nestedOutcome

// mapLimit runs fn over items with at most limit calls in flight, in chunks, and
// returns the results in item order. A chunk waits for all of its calls, so the
// nested tools are never all hit at once. No call can fail the batch: fn owns
// its own error handling (TS: Promise.allSettled per chunk). TS: mapLimit.
func mapLimit[T any, R any](items []T, limit int, fn func(item T, index int) R) []R {
	if limit < 1 {
		limit = 1
	}
	out := make([]R, 0, len(items))
	for start := 0; start < len(items); start += limit {
		end := min(start+limit, len(items))
		chunk := make([]R, end-start)
		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				chunk[i-start] = fn(items[i], i)
			}(i)
		}
		wg.Wait()
		out = append(out, chunk...)
	}
	return out
}

// executeSearchDigest is the tool body. run is the nested-call seam.
func executeSearchDigest(params map[string]any, run nestedRunner) sdk.ToolResult {
	patterns := []string{}
	if raw, ok := params["patterns"].([]any); ok {
		for _, item := range raw {
			p := toJSString(item)
			if strings.TrimSpace(p) != "" {
				patterns = append(patterns, p)
			}
		}
	}
	if len(patterns) == 0 {
		summary := "ctx_search_digest: no patterns given"
		return sdk.ToolResult{
			Content: summary,
			Details: map[string]any{"summary": summary},
			StructuredContent: SearchDigestOutput{
				Patterns: []string{},
				Files:    []SearchDigestFile{},
				Summary:  summary,
				Errors:   []SearchDigestError{{Pattern: "", Error: "patterns is empty"}},
			},
			IsError: true,
		}
	}

	maxHits := clampInt(params["max_hits"], hitsDefault, hitsHard)
	root := optStr(params["path"])
	glob := optStr(params["glob"])
	includeHidden := params["include_hidden"] == true
	ignoreCase := params["ignore_case"] == true
	literal := params["literal"] == true
	contextLines := 0
	if n, ok := positiveInt(params["context"]); ok {
		contextLines = int(math.Min(float64(n), maxContextLines))
	}

	// callArgs builds the ctx_fs rg arguments. retry adds include_hidden and the
	// `**` glob because hidden directories are only reachable through a glob in
	// ctx_fs rg (known behavior, mirrored from TS).
	callArgs := func(pattern string, retry bool) map[string]any {
		args := map[string]any{"action": "rg", "pattern": pattern, "limit": maxHits}
		if root != "" {
			args["path"] = root
		}
		if glob != "" {
			args["glob"] = glob
		}
		if retry {
			args["include_hidden"] = true
			args["glob"] = "**"
		} else if includeHidden {
			args["include_hidden"] = true
		}
		if ignoreCase {
			args["ignore_case"] = true
		}
		if literal {
			args["literal"] = true
		}
		if contextLines > 0 {
			args["context"] = contextLines
		}
		return args
	}

	type patternRun struct {
		pattern string
		outcome nestedOutcome
		parse   RgParse
	}
	perPattern := mapLimit(patterns, concurrency, func(pattern string, _ int) patternRun {
		first := run(callArgs(pattern, false))
		parse := ParseRgOutput(first.text, maxHits)
		// Degradation 1: a rooted search with zero hits and no explicit glob is
		// retried once with the hidden-inclusive `**` glob.
		if root != "" && len(parse.Files) == 0 && first.ok {
			retry := run(callArgs(pattern, true))
			if retry.ok {
				second := ParseRgOutput(retry.text, maxHits)
				if len(second.Files) > 0 {
					parse = second
					parse.Truncated = parse.Truncated || second.Truncated
				}
			}
		}
		return patternRun{pattern: pattern, outcome: first, parse: parse}
	})

	errors := []SearchDigestError{}
	for _, item := range perPattern {
		if !item.outcome.ok {
			reason := item.outcome.err
			if reason == "" {
				reason = "rg failed"
			}
			errors = append(errors, SearchDigestError{Pattern: item.pattern, Error: reason})
		}
	}

	// Dedupe and aggregate: one file:line hit by several patterns becomes one
	// entry carrying every pattern that saw it.
	type aggHit struct {
		line     int
		excerpt  string
		patterns []string
	}
	byPath := map[string]map[int]*aggHit{}
	var pathOrder []string
	totalHits := 0
	reported := 0
	anyTruncated := false
	for _, item := range perPattern {
		reported += item.parse.ReportedMatches
		anyTruncated = anyTruncated || item.parse.Truncated
		for _, file := range item.parse.Files {
			hits, ok := byPath[file.Path]
			if !ok {
				hits = map[int]*aggHit{}
				byPath[file.Path] = hits
				pathOrder = append(pathOrder, file.Path)
			}
			for _, hit := range file.Hits {
				if prev, ok := hits[hit.Line]; ok {
					if !containsString(prev.patterns, item.pattern) {
						prev.patterns = append(prev.patterns, item.pattern)
					}
					if utf16Len(prev.excerpt) < utf16Len(hit.Excerpt) {
						prev.excerpt = hit.Excerpt
					}
					continue
				}
				hits[hit.Line] = &aggHit{line: hit.Line, excerpt: hit.Excerpt, patterns: []string{item.pattern}}
				totalHits++
			}
		}
	}

	files := make([]SearchDigestFile, 0, len(pathOrder))
	for _, p := range pathOrder {
		agg := make([]*aggHit, 0, len(byPath[p]))
		for _, hit := range byPath[p] {
			agg = append(agg, hit)
		}
		sort.SliceStable(agg, func(i, j int) bool { return agg[i].line < agg[j].line })
		out := SearchDigestFile{Path: p, Hits: make([]SearchDigestHit, 0, len(agg))}
		for _, hit := range agg {
			out.Hits = append(out.Hits, SearchDigestHit{Line: hit.line, Excerpt: hit.excerpt, Patterns: hit.patterns})
		}
		files = append(files, out)
	}
	// Files by hit count desc, then path asc (JS localeCompare → byte compare here).
	sort.SliceStable(files, func(i, j int) bool {
		if len(files[i].Hits) != len(files[j].Hits) {
			return len(files[i].Hits) > len(files[j].Hits)
		}
		return files[i].Path < files[j].Path
	})

	summary := fmt.Sprintf("patterns=%d files=%d hits=%d", len(patterns), len(files), totalHits)
	if reported > 0 {
		summary += fmt.Sprintf(" (rg reported %d)", reported)
	}
	if len(errors) > 0 {
		names := make([]string, 0, len(errors))
		for _, e := range errors {
			names = append(names, e.Pattern)
		}
		summary += fmt.Sprintf("; %d pattern(s) degraded: %s", len(errors), strings.Join(names, ", "))
	}
	if anyTruncated {
		summary += "; rg output truncated — narrow path/glob or page with offset"
	}

	lines := []string{summary}
	for i, file := range files {
		if i >= textFiles {
			break
		}
		lines = append(lines, fmt.Sprintf("%s (%d)", file.Path, len(file.Hits)))
		for j, hit := range file.Hits {
			if j >= textHits {
				break
			}
			lines = append(lines, fmt.Sprintf("  %d: %s  [%s]", hit.Line, hit.Excerpt, strings.Join(hit.Patterns, ", ")))
		}
		if len(file.Hits) > textHits {
			lines = append(lines, fmt.Sprintf("  ... (+%d more hits)", len(file.Hits)-textHits))
		}
	}
	if len(files) > textFiles {
		lines = append(lines, fmt.Sprintf("... (+%d more files)", len(files)-textFiles))
	}

	var outErrors []SearchDigestError
	if len(errors) > 0 {
		outErrors = errors
	}
	return sdk.ToolResult{
		Content: strings.Join(lines, "\n"),
		Details: map[string]any{"summary": summary},
		StructuredContent: SearchDigestOutput{
			Patterns: patterns,
			Files:    files,
			Summary:  summary,
			Errors:   outErrors,
		},
		// Only a total failure is an error result: partial failures stay data.
		IsError: len(errors) == len(patterns),
	}
}

// containsString reports whether list holds name.
func containsString(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// executeToolRunner adapts ctx.ExecuteTool into a nestedRunner: tool failures
// never fail the digest, and a host/transport error or a panic becomes a
// degraded entry.
func executeToolRunner(ctx sdk.Context, name string) nestedRunner {
	return func(args map[string]any) (out nestedOutcome) {
		defer func() {
			if recovered := recover(); recovered != nil {
				out = nestedOutcome{ok: false, isError: true, err: oneLine(fmt.Sprint(recovered), errLineCap)}
			}
		}()
		outcome, err := ctx.ExecuteTool(name, args, nil)
		if err != nil {
			return nestedOutcome{ok: false, isError: true, err: oneLine(err.Error(), errLineCap)}
		}
		text := outcome.Result.Text()
		if outcome.IsError {
			msg := text
			if msg == "" {
				msg = "tool error"
			}
			return nestedOutcome{ok: false, isError: true, text: text, err: oneLine(msg, errLineCap)}
		}
		return nestedOutcome{ok: true, text: text}
	}
}
