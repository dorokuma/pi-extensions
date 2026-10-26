package compactui

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// ── constants (TS L50-69) ────────────────────────────────────────────────────

// spinnerFrames is SPINNER: the ten braille frames of the tool spinner.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerMs is SPINNER_MS.
const spinnerMs = 100

// thinkingSpinnerFrames is THINKING_SPINNER.rawFrames.
var thinkingSpinnerFrames = []string{"·", "•", "●", "•"}

// thinkingSpinnerIntervalMs is THINKING_SPINNER.intervalMs.
const thinkingSpinnerIntervalMs = 140

// thinkingSpinnerColors is THINKING_SPINNER.colorKeys.
var thinkingSpinnerColors = []string{"dim", "muted", "accent", "muted"}

// groupPaddingX is GROUP_PADDING_X.
const groupPaddingX = 1

// groupPaddingRight is GROUP_PADDING_RIGHT.
const groupPaddingRight = 2

// The agent work phasess setAgentWorkPhase records (L2200-2204, L2212-2410):
// the label the open group shows between streamed entries.
const (
	phaseWaitingResponse = "waiting-response"
	phaseWaitingOutput   = "waiting-output"
	phaseWorking         = "working"
)

// stripCacheMissPrefix is `.replace(/^cache miss\s*/i, "").replace(/^[:\s]+/, "")`
// (L963, L1723, L2005): the notice body without its label and separators.
func stripCacheMissPrefix(text string) string {
	trimmed := strings.TrimLeft(text, " \t\n\r\f\v")
	if len(trimmed) >= len("cache miss") && strings.EqualFold(trimmed[:len("cache miss")], "cache miss") {
		trimmed = strings.TrimLeft(trimmed[len("cache miss"):], " \t\n\r\f\v")
	}
	return strings.TrimLeft(trimmed, " :\t\n\r\f\v")
}

// ── helpers (TS L70-192) ─────────────────────────────────────────────────────

// shortenPath replaces the user's home directory prefix with "~" (L71-74).
func shortenPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// oneLine collapses whitespace and truncates to max JS UTF-16 code units
// (L75-82). The truncation follows JS: `"...".slice(0, max)` when max <= 3,
// otherwise text[:max-3] + "...". Lengths and slices are in UTF-16 units so a
// caller measuring a JS string sees the same cut; a negative max is the TS
// call that passes no limit at all.
func oneLine(value string, max int) string {
	text := strings.Join(strings.Fields(value), " ")
	if max >= 0 && utf16Len(text) > max {
		if max <= 3 {
			// TS: `"...".slice(0, max)` -- the literal dots, never the input.
			return utf16Slice("...", 0, max)
		}
		return utf16Slice(text, 0, max-3) + "..."
	}
	return text
}

// estimateTextTokens is Math.ceil(text.length / 4) (L83-85).
func estimateTextTokens(text string) int {
	return (utf16Len(text) + 3) / 4
}

// jsTenths rounds tokens/1000 to one decimal the way JS
// Number.prototype.toFixed(1) does. JS does *not* round the exact rational
// tokens/1000: it first computes the IEEE-754 double v = tokens / 1000 and then
// formats that double's precise binary value. A halfway decimal therefore does
// not always tie -- 2550/1000 is the double just below 2.55, so
// (2550/1000).toFixed(1) is "2.5" where exact rational arithmetic would say
// "2.6". The port reproduces the double exactly: float64(tokens)/1000 read back
// through big.Rat as the rational it denotes, then the ECMA-262 toFixed rule
// (the integer closest to x*10; an exact tie picks the larger integer).
func jsTenths(tokens int) int {
	negative := tokens < 0
	magnitude := tokens
	if negative {
		magnitude = -magnitude
	}
	exact := new(big.Rat).SetFloat64(float64(magnitude) / 1000.0)
	if exact == nil {
		// float64(magnitude)/1000 is finite for every int, so this is only
		// reachable with a non-finite input; fall back to exact rational
		// arithmetic rather than guessing.
		exact = new(big.Rat).SetInt64(int64(magnitude))
		exact.Quo(exact, big.NewRat(1000, 1))
	}
	// tenths = round(exact * 10), ties away from zero on the magnitude.
	num := new(big.Int).Mul(exact.Num(), big.NewInt(10))
	den := exact.Denom()
	quotient, remainder := new(big.Int).QuoRem(num, den, new(big.Int))
	tenths := quotient.Int64()
	if remainder.Sign() != 0 {
		doubled := new(big.Int).Lsh(remainder, 1)
		if doubled.CmpAbs(den) >= 0 {
			tenths++
		}
	}
	if negative {
		return int(-tenths)
	}
	return int(tenths)
}

// jsTenthsString renders a tenths count as "d.d" (with a leading minus for a
// negative count, which is the shape JS would print).
func jsTenthsString(tenths int) string {
	sign := ""
	if tenths < 0 {
		sign = "-"
		tenths = -tenths
	}
	return sign + strconv.Itoa(tenths/10) + "." + strconv.Itoa(tenths%10)
}

// jsRoundHalfUp is Math.round for a non-negative value: floor(x + 0.5).
func jsRoundHalfUp(value float64) int64 {
	return int64(math.Floor(value + 0.5))
}

// formatTokenK is formatTokenK (L86-92): 0.0K / <0.1K / %.1fK / %dK.
func formatTokenK(tokens int) string {
	if tokens <= 0 {
		return "0.0K"
	}
	if tokens < 100 {
		return "<0.1K"
	}
	value := float64(tokens) / 1e3
	if value < 100 {
		return jsTenthsString(jsTenths(tokens)) + "K"
	}
	return strconv.FormatInt(jsRoundHalfUp(value), 10) + "K"
}

// thinkingTokenLabel is thinkingTokenLabel (L93-101): "(123t)" below 1000
// tokens, otherwise "(1.2k)" / "(123k)" with a lower-case k.
func thinkingTokenLabel(entry *thinkingEntry) string {
	tokens := 0
	if entry != nil && entry.tokens > 0 {
		tokens = entry.tokens
	}
	if tokens < 1000 {
		return fmt.Sprintf("(%dt)", tokens)
	}
	k := float64(tokens) / 1e3
	if k < 100 {
		return "(" + jsTenthsString(jsTenths(tokens)) + "k)"
	}
	return "(" + strconv.FormatInt(jsRoundHalfUp(k), 10) + "k)"
}

// toolSummary is toolSummary (L102-133): the short name and payload shown for
// a tool call, per tool family, with the generic first-string-argument
// fallback.
func toolSummary(name string, args map[string]any) toolSummaryValue {
	switch name {
	case "bash", "powershell":
		return toolSummaryValue{name: name, content: oneLine(argsString(args, "command", "..."), -1)}
	case "read":
		return toolSummaryValue{name: "read", content: shortenPath(argsString(args, "path", "..."))}
	case "write", "edit":
		return toolSummaryValue{name: name, content: shortenPath(argsString(args, "path", "..."))}
	case "find":
		return toolSummaryValue{name: "find", content: oneLine(argsString(args, "pattern", ""), -1) + " in " + shortenPath(argsString(args, "path", "."))}
	case "grep":
		return toolSummaryValue{name: "grep", content: oneLine(argsString(args, "pattern", ""), -1) + " in " + shortenPath(argsString(args, "path", "."))}
	case "ls":
		return toolSummaryValue{name: "ls", content: shortenPath(argsString(args, "path", "."))}
	case "web_search":
		return toolSummaryValue{name: "web_search", content: oneLine(argsString(args, "query", "..."), -1)}
	case "subagent":
		// TS: `oneLine(args?.agent || args?.task || "...")` -- an empty or
		// blank agent name falls through to the task description.
		agent := strings.TrimSpace(argsString(args, "agent", ""))
		if agent != "" {
			return toolSummaryValue{name: "subagent", content: oneLine(agent, -1)}
		}
		return toolSummaryValue{name: "subagent", content: oneLine(argsString(args, "task", "..."), -1)}
	case "codemode":
		return toolSummaryValue{name: "codemode", content: oneLine(argsString(args, "code", "..."), -1)}
	default:
		// TS prefers path/query/name/description/url. PiG sessions often use
		// ctx_run (command/argv/commands/action/code) instead of bash, so the
		// fallback also surfaces those fields for a one-line card summary.
		for _, key := range []string{"path", "query", "name", "description", "url", "command", "code"} {
			if value, ok := args[key]; ok && value != nil {
				if text := strings.TrimSpace(jsString(value)); text != "" {
					return toolSummaryValue{name: name, content: oneLine(text, -1)}
				}
			}
		}
		if joined := argvJoined(args["argv"]); joined != "" {
			return toolSummaryValue{name: name, content: oneLine(joined, -1)}
		}
		if cmd := firstCommandsSummary(args["commands"]); cmd != "" {
			return toolSummaryValue{name: name, content: oneLine(cmd, -1)}
		}
		if action := strings.TrimSpace(argsString(args, "action", "")); action != "" {
			return toolSummaryValue{name: name, content: oneLine(action, -1)}
		}
		return toolSummaryValue{name: name, content: oneLine("...", -1)}
	}
}

// argvJoined joins a ctx_run-style argv list into one summary line.
func argvJoined(value any) string {
	switch items := value.(type) {
	case []any:
		parts := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok || s == "" {
				continue
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, " ")
	case []string:
		parts := make([]string, 0, len(items))
		for _, s := range items {
			if s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// firstCommandsSummary picks a readable preview from a ctx_run commands list.
func firstCommandsSummary(value any) string {
	switch items := value.(type) {
	case []any:
		for _, item := range items {
			switch entry := item.(type) {
			case string:
				if text := strings.TrimSpace(entry); text != "" {
					return text
				}
			case map[string]any:
				if text := strings.TrimSpace(argsString(entry, "command", "")); text != "" {
					return text
				}
				if text := strings.TrimSpace(argsString(entry, "label", "")); text != "" {
					return text
				}
			}
		}
	case []string:
		for _, entry := range items {
			if text := strings.TrimSpace(entry); text != "" {
				return text
			}
		}
	}
	return ""
}

// toolSummaryValue is the `{ name, content }` pair toolSummary returns.
type toolSummaryValue struct {
	name    string
	content string
}

// toolStatus is toolStatus (L134-141): "pending", "error" or "success".
func toolStatus(tool *toolComponent, owner *toolGroupComponent) string {
	if tool == nil {
		return "pending"
	}
	if (tool.result != nil && tool.result.isError) || tool.interrupted {
		return "error"
	}
	group := tool.owner
	if group == nil {
		group = owner
	}
	if group != nil && group.sealed && tool.result == nil {
		return "error"
	}
	if tool.isPartial || (tool.executionStarted && tool.result == nil) {
		return "pending"
	}
	if tool.result != nil {
		return "success"
	}
	return "pending"
}

// formatWorkedTime is formatWorkedTime (L164-172): "3s" / "1m 20s" / "1h 5m".
func formatWorkedTime(elapsedMs int64) string {
	totalSec := int64(math.Max(1, math.Round(float64(elapsedMs)/1e3)))
	hours := totalSec / 3600
	minutes := totalSec % 3600 / 60
	seconds := totalSec % 60
	if hours > 0 {
		if minutes > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	}
	if minutes > 0 {
		if seconds > 0 {
			return fmt.Sprintf("%dm %ds", minutes, seconds)
		}
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", seconds)
}

// formatToolDuration is formatToolDuration (L173-185): "0ms" / "12ms" /
// "1.23s" / "12.3s".
func formatToolDuration(durationSec float64) string {
	safeSec := durationSec
	if math.IsNaN(safeSec) || math.IsInf(safeSec, 0) {
		safeSec = 0
	}
	if safeSec < 0 {
		safeSec = 0
	}
	ms := math.Round(safeSec * 1e3)
	if ms < 1e3 {
		return strconv.FormatInt(int64(ms), 10) + "ms"
	}
	if safeSec < 10 {
		return strconv.FormatFloat(safeSec, 'f', 2, 64) + "s"
	}
	return strconv.FormatFloat(safeSec, 'f', 1, 64) + "s"
}

// streamingToolArgs is streamingToolArgs (L142-155): the arguments of a tool
// call that is still streaming in. A complete `arguments` object wins; a
// partial JSON string is mapped onto the tool's first parameter.
func streamingToolArgs(block streamBlock) map[string]any {
	if len(block.arguments) > 0 {
		return block.arguments
	}
	partial := strings.TrimSpace(block.partialJSON)
	if partial == "" {
		if block.partialArgs != "" {
			partial = strings.TrimSpace(block.partialArgs)
		}
		if partial == "" {
			return map[string]any{}
		}
	}
	switch block.name {
	case "bash", "powershell":
		return map[string]any{"command": partial}
	case "read", "write", "edit", "ls":
		return map[string]any{"path": partial}
	case "codemode":
		return map[string]any{"code": partial}
	}
	return map[string]any{"query": partial}
}

// locateStreamingToolCall is locateStreamingToolCall (L156-172): the content
// index and block of the tool call a stream event belongs to.
func locateStreamingToolCall(content []contentBlock, streamEvent streamEvent) (int, streamBlock, bool) {
	if streamEvent.contentIndex != nil {
		index := *streamEvent.contentIndex
		if index >= 0 && index < len(content) && content[index].typ == "toolCall" {
			return index, content[index].block, true
		}
	}
	if streamEvent.toolCall != nil {
		fromEvent := *streamEvent.toolCall
		if fromEvent.typ == "toolCall" || fromEvent.name != "" {
			for i, item := range content {
				if item.typ == "toolCall" && fromEvent.id != "" && item.block.id == fromEvent.id {
					return i, content[i].block, true
				}
			}
			index := len(content)
			if streamEvent.contentIndex != nil {
				index = *streamEvent.contentIndex
			}
			return index, fromEvent, true
		}
	}
	return 0, streamBlock{}, false
}

// toolResultText is toolResultText (L173-175): the text blocks of a tool
// result joined by newlines.
func toolResultText(tool *toolComponent) string {
	if tool == nil || tool.result == nil {
		return ""
	}
	parts := make([]string, 0, len(tool.result.content))
	for _, item := range tool.result.content {
		if item.typ == "text" {
			parts = append(parts, item.text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// isCtrlI is isCtrlI (L186-192): Ctrl+I, strictly excluding the literal tab
// and key-release events. parseKey and matchesKey both come from the SDK's
// pi-tui port, so a Kitty CSI-u Ctrl+I press and the legacy 0x09 byte agree.
func isCtrlI(data string) bool {
	if isKeyRelease(data) {
		return false
	}
	if data == "\t" {
		return false
	}
	return parseKey(data) == "ctrl+i" || matchesKey(data, "ctrl+i")
}

// ── config (TS L786-807) ─────────────────────────────────────────────────────

// configFieldMaxGroupEntries and configField are the config keys.
const configField = "maxGroupEntries"

// defaultMaxGroupEntries is DEFAULT_CONFIG.maxGroupEntries.
const defaultMaxGroupEntries = 5

// compactConfig is the persisted configuration.
type compactConfig struct {
	MaxGroupEntries int
}

// defaultConfig is DEFAULT_CONFIG.
func defaultConfig() compactConfig { return compactConfig{MaxGroupEntries: defaultMaxGroupEntries} }

// configPathFor is configPath (L789) with the PIG_HOME red line: a non-empty
// PIG_HOME isolates the config under $PIG_HOME/agent, otherwise the path is
// ~/.pi/agent/compact-ui.json.
func configPathFor(env func(string) string, home func() (string, error)) string {
	if env != nil {
		if pigHome := env("PIG_HOME"); pigHome != "" {
			return filepath.Join(pigHome, "agent", "compact-ui.json")
		}
	}
	userHome := ""
	if home != nil {
		if resolved, err := home(); err == nil {
			userHome = resolved
		}
	}
	return filepath.Join(userHome, ".pi", "agent", "compact-ui.json")
}

// loadConfig is loadConfig (L797-807): decode the file, falling back to the
// defaults on any error.
func loadConfig(path string, read func(string) ([]byte, error)) compactConfig {
	if read == nil {
		return defaultConfig()
	}
	raw, err := read(path)
	if err != nil {
		return defaultConfig()
	}
	value, ok := decodeJSONObject(raw)
	if !ok {
		return defaultConfig()
	}
	entries, present := value[configField]
	if !present || entries == nil {
		return defaultConfig()
	}
	number, ok := toNumber(entries)
	if !ok {
		return defaultConfig()
	}
	return compactConfig{MaxGroupEntries: int(number)}
}

// saveConfig is saveConfig (L791-796): 2-space indented JSON plus a trailing
// newline, silently downgraded on any error.
func saveConfig(values compactConfig, path string, write func(string, []byte) error) {
	if write == nil {
		return
	}
	payload := marshalConfigJSON(values)
	_ = write(path, payload)
}

// marshalConfigJSON renders DEFAULT_CONFIG's shape with two-space indentation
// and a trailing newline, as JSON.stringify(values, null, 2) + "\n" does.
func marshalConfigJSON(values compactConfig) []byte {
	return []byte("{\n  \"" + configField + "\": " + strconv.Itoa(values.MaxGroupEntries) + "\n}\n")
}

// ── animation (TS L755-785) ──────────────────────────────────────────────────

// thinkingSpinnerFrame is thinkingSpinnerFrame (L756-762).
func thinkingSpinnerFrame(nowMs int64, startMs int64) spinnerFrame {
	index := 0
	if span := int64(thinkingSpinnerIntervalMs); span > 0 {
		index = int((nowMs-startMs)/span) % len(thinkingSpinnerFrames)
	}
	if index < 0 {
		index += len(thinkingSpinnerFrames)
	}
	return spinnerFrame{frame: thinkingSpinnerFrames[index], color: thinkingSpinnerColors[index]}
}

// spinnerFrame is the `{ frame, color }` pair.
type spinnerFrame struct {
	frame string
	color string
}

// toolSpinnerFrame is the SPINNER lookup renderCollapsed does (L1006).
func toolSpinnerFrame(nowMs int64, startMs int64) string {
	index := int((nowMs - startMs) / spinnerMs)
	index %= len(spinnerFrames)
	if index < 0 {
		index += len(spinnerFrames)
	}
	return spinnerFrames[index]
}

// disableMouseTracking is disableMouseTracking (L763-767): the four
// mouse-tracking disables a TUI turns off on exit.
const disableMouseTrackingSequence = "\x1b[?1006l\x1b[?1004l\x1b[?1002l\x1b[?1000l"

// animationDelay is scheduleAnimation's delay choice (L769-785): SPINNER_MS
// while any group has a pending tool, otherwise THINKING_SPINNER.intervalMs.
func (r *runtimeState) animationDelayLocked() int {
	for _, group := range r.groupsListLocked() {
		if group.hasPendingLocked() {
			return spinnerMs
		}
	}
	return thinkingSpinnerIntervalMs
}

// theme is the host presentation surface the renderers use. It is an
// interface so a test can record colour calls (the TS suite's tracking theme)
// and so a nil theme renders unstyled text, as `theme?.fg?.(...) ?? text`.
type theme interface {
	fg(color, text string) string
	bold(text string) string
	bg(color, text string) string
	inverse(text string) string
}

// plainTheme renders every style as its input.
type plainTheme struct{}

func (plainTheme) fg(_, text string) string   { return text }
func (plainTheme) bold(text string) string    { return text }
func (plainTheme) bg(_, text string) string   { return text }
func (plainTheme) inverse(text string) string { return text }

// sdkTheme adapts the host palette (sdk.UITheme) to theme. g.UnknownTheme
// counts the colours the host palette does not know, which the TS source
// renders unstyled because `theme.fg` returns the argument for an unknown
// token in the Node runtime's non-throwing path.
type sdkTheme struct {
	theme sdk.UITheme
}

func (t sdkTheme) fg(color, text string) string { return t.style("fg", color, text) }
func (t sdkTheme) bg(color, text string) string { return t.style("bg", color, text) }
func (t sdkTheme) bold(text string) string      { return t.style("bold", "", text) }
func (t sdkTheme) inverse(text string) string   { return t.style("inverse", "", text) }

// style calls one sdk.UITheme method, falling back to the unstyled text when
// the host palette has no such token (sdk.UITheme.Fg/Bg panic on an unknown
// colour when the palette is present).
func (t sdkTheme) style(kind, color, text string) (out string) {
	defer func() {
		if recover() != nil {
			out = text
		}
	}()
	switch kind {
	case "fg":
		return t.theme.Fg(color, text)
	case "bg":
		return t.theme.Bg(color, text)
	case "bold":
		return t.theme.Bold(text)
	default:
		return t.theme.Inverse(text)
	}
}

// fgOf applies a theme's foreground, or leaves the text alone without a theme
// (`theme?.fg?.(color, text) ?? text`).
func fgOf(th theme, color, text string) string {
	if th == nil {
		return text
	}
	return th.fg(color, text)
}

// boldOf applies a theme's bold, or leaves the text alone.
func boldOf(th theme, text string) string {
	if th == nil {
		return text
	}
	return th.bold(text)
}
