// Package ctxmode is the Go port of the Pi ctxmode MCP stdio bridge.
package ctxmode

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

func envString(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}

// envNumber mirrors TS `Number(process.env.X || DEFAULT)` per knob: "0" is a
// number (0) and must not fall back to DEFAULT, while an empty value — and any
// value JS turns into NaN (±Infinity included, which a Go int cannot hold) —
// falls back to DEFAULT. Environment knobs are read at use time, unlike TS
// module-level budgets.
func envNumber(key string, fallback int) int {
	s := os.Getenv(key)
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return fallback
	}
	return int(math.Max(math.MinInt32, math.Min(math.MaxInt32, n)))
}

// envPositiveNumber is envNumber for knobs whose zero or negative value would
// switch the mechanism off entirely — a handshake/request timeout of 0 fires
// before the peer can answer, leaving the client unusable. Only a positive value
// wins; "0", negatives and NaN-ish input all keep the default.
func envPositiveNumber(key string, fallback int) int {
	if n := envNumber(key, fallback); n > 0 {
		return n
	}
	return fallback
}

var (
	osc           = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	dcs           = regexp.MustCompile("(?s)\x1b[PX^_](?:.*?(?:\x07|\x1b\\\\)|.*)")
	csi           = regexp.MustCompile("\x1b\\[[0-9:;<=>?]*[ -/]*[@-~]")
	esc           = regexp.MustCompile("\x1b[\x20-\x2f]*[\x30-\x7e]")
	trailingSpace = regexp.MustCompile("(?m)[ \t]+$")
	blankLines    = regexp.MustCompile("\n{4,}")
	roleName      = regexp.MustCompile(`(?i)^role-([a-z0-9_-]+)-[0-9a-f]{8}$`)
)

// StripANSI preserves the source's ordered OSC, DCS/SOS/PM/APC, CSI, ESC passes.
func StripANSI(s string) string {
	for _, re := range []*regexp.Regexp{osc, dcs, csi, esc} {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

func clampUtf16Index(s []uint16, i int, towardStart bool) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	if s[i]&0xfc00 == 0xdc00 {
		if towardStart {
			return i - 1
		}
		return i + 1
	}
	return i
}

// CompressToolText budgets characters in UTF-16 code units, retaining exit tails.
func CompressToolText(text string) string {
	if text == "" {
		return text
	}
	charCap, lineCap := envNumber("CTXMODE_OUTPUT_CHARS", 50000), envNumber("CTXMODE_OUTPUT_LINES", 500)
	body := blankLines.ReplaceAllString(trailingSpace.ReplaceAllString(strings.ReplaceAll(StripANSI(text), "\r\n", "\n"), ""), "\n\n\n")
	lines := strings.Split(body, "\n")
	if len(lines) > lineCap {
		headN := max(40, int(math.Floor(float64(lineCap)*.75)))
		tailN := max(15, lineCap-headN-1)
		// Both ends are clamped at the source (min/max) so the slices can never run
		// out of range when headN+tailN exceeds len(lines) or omitted goes negative;
		// the overlap then simply repeats lines, matching TS slice() semantics.
		body = strings.Join(lines[:min(headN, len(lines))], "\n") + fmt.Sprintf("\n... (省略 %d 行；可缩小范围再查) ...\n", len(lines)-headN-tailN) + strings.Join(lines[max(0, len(lines)-tailN):], "\n")
	}
	units := utf16.Encode([]rune(body))
	if len(units) <= charCap {
		return body
	}
	// A zero cap still truncates: TS `body.length <= 0` is false for a non-empty
	// body, so head/tail collapse to the marker alone. The offsets are clamped
	// non-negative at the source as well (a negative cap would otherwise index
	// before the encoded body); the marker keeps TS's raw cap value.
	headN := max(0, int(math.Floor(float64(charCap)*.75)))
	tailN := max(0, charCap-headN)
	headEnd, tailStart := clampUtf16Index(units, headN, true), clampUtf16Index(units, len(units)-tailN, false)
	return string(utf16.Decode(units[:headEnd])) + fmt.Sprintf("\n... (输出达上限 %d 字，已截断) ...\n", charCap) + string(utf16.Decode(units[tailStart:]))
}

// ResolveRoleEnv leaves explicit role settings intact and infers dispatched roles.
func ResolveRoleEnv(argv []string, baseEnv map[string]string) map[string]string {
	if baseEnv["CTXMODE_ROLE"] != "" {
		return baseEnv
	}
	for i, arg := range argv {
		val := ""
		if arg == "--name" && i+1 < len(argv) {
			val = argv[i+1]
		} else if strings.HasPrefix(arg, "--name=") {
			val = strings.TrimPrefix(arg, "--name=")
		}
		if m := roleName.FindStringSubmatch(val); m != nil {
			role := strings.ToLower(m[1])
			out := make(map[string]string, len(baseEnv)+2)
			for k, v := range baseEnv {
				out[k] = v
			}
			out["CTXMODE_ROLE"] = role
			switch role {
			case "scout", "planner", "researcher", "reviewer", "oracle", "verifier":
				if baseEnv["CTXMODE_READONLY"] == "" {
					out["CTXMODE_READONLY"] = "1"
				}
			}
			return out
		}
	}
	return baseEnv
}

func roleEnviron() []string {
	base := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, _ := strings.Cut(entry, "=")
		base[k] = v
	}
	out := ResolveRoleEnv(os.Args, base)
	env := make([]string, 0, len(out))
	for k, v := range out {
		env = append(env, k+"="+v)
	}
	return env
}
