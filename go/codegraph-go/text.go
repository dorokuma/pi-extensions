package codegraphgo

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// JS whitespace includes NBSP, BOM and Unicode line/paragraph separators.
const jsSpace = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

func isJSSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == ' ' || r == 0xa0 || r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
}
func envNumber(name string, fallback int) int {
	s := os.Getenv(name)
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n >= float64(math.MaxInt) || n <= float64(math.MinInt) {
		return fallback
	}
	return int(math.Floor(n))
}
func outputChars() int { return envNumber("CODEGRAPH_GO_OUTPUT_CHARS", 18000) }
func outputLines() int { return envNumber("CODEGRAPH_GO_OUTPUT_LINES", 260) }
func budget(kind string) (int, int) {
	defaults := map[string][2]int{"SEARCH": {70, 120}, "FILES": {100, 150}, "SYMBOL": {40, 80}, "EXPLORE": {30, 60}}
	v := defaults[kind]
	return envNumber("CODEGRAPH_GO_"+kind+"_MAX", v[0]), envNumber("CODEGRAPH_GO_"+kind+"_HARD", v[1])
}

// numberValue reads a value the way TS `typeof n === "number" && Number.isFinite(n)`
// sees it: an int/float or a JSON number literal (`encoding/json.Number`, which
// the SDK can hand over). A string is never a number, so it keeps the default.
func numberValue(n any) (float64, bool) {
	switch x := n.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		f := float64(x)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case jsonNumber:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

// jsonNumber is the interface of encoding/json.Number (and of the SDK's own
// json.Number), matched structurally so this package needs no extra import.
type jsonNumber interface{ Float64() (float64, error) }

// clampInt mirrors TS clampInt: a missing, non-numeric or below-1 value keeps
// def, above hard becomes hard. The clamp runs in float space *before* the int
// conversion, because int(math.Floor(1e30)) overflows into a negative int where
// TS saturates at hard.
func clampInt(n any, def, hard int) int {
	v := float64(def)
	if f, ok := numberValue(n); ok {
		v = math.Floor(f)
	}
	if v < 1 {
		v = float64(def)
	}
	if v > float64(hard) {
		return hard
	}
	return int(v)
}
func withToolDefaults(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	action, _ := out["action"].(string)
	switch action {
	case "search", "callers", "callees", "impact":
		kind := "SYMBOL"
		if action == "search" {
			kind = "SEARCH"
		}
		def, hard := budget(kind)
		if out["max_results"] != nil {
			out["max_results"] = clampInt(out["max_results"], def, hard)
		} else if out["max"] != nil {
			out["max"] = clampInt(out["max"], def, hard)
		} else {
			out["max_results"] = def
		}
	case "files":
		def, hard := budget("FILES")
		out["max"] = clampInt(out["max"], def, hard)
	case "explore":
		def, hard := budget("EXPLORE")
		if out["max"] != nil && !numericZero(out["max"]) {
			out["max"] = clampInt(out["max"], def, hard)
		} else {
			delete(out, "max")
		}
		if _, exists := out["skipCode"]; !exists {
			out["skipCode"] = true
		}
	case "node":
		if out["offset"] != nil {
			out["offset"] = clampInt(out["offset"], 1, 1000000)
		}
		if out["limit"] != nil {
			out["limit"] = clampInt(out["limit"], 1, 2000)
		}
		if _, exists := out["includeCode"]; !exists {
			out["includeCode"] = false
		}
	case "affected":
		out["depth"] = clampInt(out["depth"], 5, 10)
	}
	return out
}

// numericZero mirrors TS `value === 0`, so it shares numberValue's notion of a
// number; explore's `max=0` means "server decides" and must not gain a default.
func numericZero(v any) bool {
	f, ok := numberValue(v)
	return ok && f == 0
}
func wantsKeepCode(toolName string, args map[string]any) bool {
	if v, ok := args["skipCode"].(bool); ok && !v {
		return true
	}
	if v, ok := args["includeCode"].(bool); ok && v {
		return true
	}
	action, _ := args["action"].(string)
	if action == "node" || action == "read" {
		return true
	}
	if toolName == "" {
		toolName, _ = args["name"].(string)
	}
	hint := strings.ToLower(toolName)
	return hint == "node" || hint == "read"
}

var trailingSpaceRE = regexp.MustCompile(`(?m)[ \t]+$`)
var emptyLinesRE = regexp.MustCompile(`\n{4,}`)

func compressToolText(text string) string {
	if text == "" {
		return text
	}
	t := strings.ReplaceAll(text, "\r\n", "\n")
	t = trailingSpaceRE.ReplaceAllString(t, "")
	t = emptyLinesRE.ReplaceAllString(t, "\n\n\n")
	lineCap := outputLines()
	if lineCap <= 0 {
		lineCap = 220
	}
	lines := strings.Split(t, "\n")
	body := t
	if len(lines) > lineCap {
		head := max(40, int(math.Floor(float64(lineCap)*0.75)))
		tail := max(15, lineCap-head-1)
		omitted := len(lines) - head - tail
		// Array.slice tolerates caps below the minimum preserved head/tail.
		body = strings.Join(lines[:min(head, len(lines))], "\n") + fmt.Sprintf("\n... (省略 %d 行；可再查得更具体，或略提高 max) ...\n", omitted) + strings.Join(lines[max(0, len(lines)-tail):], "\n")
	}
	cap := outputChars()
	if cap <= 0 {
		cap = 14000
	}
	units := utf16.Encode([]rune(body))
	if len(units) <= cap {
		return body
	}
	truncAt := cap
	if truncAt > 0 && units[truncAt-1]&0xfc00 == 0xd800 {
		truncAt--
	}
	cut := truncAt
	for i := truncAt - 1; i >= 0; i-- {
		if units[i] == '\n' {
			if float64(i) > float64(cap)*0.7 {
				cut = i
			}
			break
		}
	}
	return string(utf16.Decode(units[:cut])) + fmt.Sprintf("\n... (输出达上限 %d 字，已截断；请缩小范围再查)", cap)
}

var fencedCodeRE = regexp.MustCompile("(?s)```.*?```")
var fallbackRE = regexp.MustCompile(`(?m)^([^:\n]+:\d+):.+$`)
var headingRE = regexp.MustCompile(`(?m)^#{1,6}` + jsSpace + `+`)
var boldRE = regexp.MustCompile(`\*\*([^*]+)\*\*`)
var italicRE = regexp.MustCompile(`\*([^*]+)\*`)
var inlineRE = regexp.MustCompile("`([^`]+)`")
var quoteRE = regexp.MustCompile(`(?m)^` + jsSpace + `*>` + jsSpace + `?`)

func formatCleanText(text string, keepCode bool) string {
	if text == "" {
		return text
	}
	t := text
	if !keepCode {
		t = fencedCodeRE.ReplaceAllStringFunc(t, func(block string) string {
			inner := block[3 : len(block)-3]
			code := inner
			if nl := strings.IndexByte(inner, '\n'); nl > 0 {
				code = inner[nl+1:]
			}
			count := 0
			for _, l := range strings.Split(code, "\n") {
				if strings.TrimFunc(l, isJSSpace) != "" {
					count++
				}
			}
			return fmt.Sprintf("[ %d 行代码已省略 ]", max(1, count))
		})
		t = fallbackRE.ReplaceAllString(t, "${1}")
	}
	t = headingRE.ReplaceAllString(t, "")
	t = boldRE.ReplaceAllString(t, "${1}")
	t = italicRE.ReplaceAllString(t, "${1}")
	t = inlineRE.ReplaceAllString(t, "${1}")
	t = trailingSpaceRE.ReplaceAllString(t, "")
	t = emptyLinesRE.ReplaceAllString(t, "\n\n\n")
	t = quoteRE.ReplaceAllString(t, "")
	return strings.TrimFunc(t, isJSSpace)
}
