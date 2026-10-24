package ctxorchestrate

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Governance hook: a physical cap on the codemode tool's text() reply.
//
// Mechanism (verified against the host, not guessed):
//   - codemode is a built-in tool registered by createCodemodeToolDefinition():
//     name="codemode", exposure="model-only", defaultActive=false, its execute
//     is the QuickJS runner. An extension cannot wrap another extension's tool
//     definition; registerToolRenderer only changes rendering.
//   - The one seam that can rewrite another tool's reply is pi.on("tool_result"):
//     the handler may return {content, details, structuredContent, isError,
//     usage}, and a model-issued codemode call (no parentToolCallId) goes
//     through it.
//   - The runner itself only has max_output_tokens (default 10000 tokens ≈
//     40000 characters) and a script can raise it with // @options, so there is
//     no lower floor: getting down to ~3000 characters needs this hook.
//   - Replacing content without returning structuredContent drops the
//     structured content, and dropping details loses codemode's fullOutputPath /
//     calls record. Hence all four are passed through explicitly. The truncated
//     model cannot see details, so the warning marker carries fullOutputPath
//     when it exists: recovering the full output becomes one `read` instead of
//     re-running the script.
//   - A nested call (parentToolCallId set) feeds a script that reads its
//     structuredContent; truncating it would destroy data, so only top-level
//     codemode replies are governed.

const (
	// DefaultTextCap is the default codemode reply cap, in UTF-16 code units. TS: DEFAULT_TEXT_CAP.
	DefaultTextCap = 3000
	// MinTextCap is the floor of CTX_ORCH_TEXT_CAP. TS: Math.max(200, ...).
	MinTextCap = 200
	// GovernMinTotal is the total below which the hook does not touch a reply.
	GovernMinTotal = 200
	// markerReserve is the room held back for the warning marker, so the
	// truncated reply still fits under the cap. TS: cap - 180.
	markerReserve = 180
)

// maxTextCapFloat is the first float64 that no longer fits an int: an int is at
// most math.MaxInt = 2^63-1, while float64(math.MaxInt) rounds up to 2^63. Any
// parsed value at or above it is out of range for the conversion below.
var maxTextCapFloat = float64(math.MaxInt)

// parseTextCap turns a CTX_ORCH_TEXT_CAP value into the effective cap.
//
// It is total: every value that cannot describe a usable cap falls back to
// DefaultTextCap instead of letting a JavaScript-style float leak into an int
// conversion (Go's int(NaN) and int(1e19) are implementation-defined and wrap to
// the minimum int on amd64, which used to turn a nonsense cap into the 200 floor
// and over-truncate every reply). Blank, unparseable, NaN, ±Inf, <= 0 and
// out-of-int-range values all mean "unset"; a finite positive value is floored,
// then the MinTextCap floor applies.
func parseTextCap(raw string) int {
	if raw == "" {
		return DefaultTextCap
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return DefaultTextCap
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n >= maxTextCapFloat {
		return DefaultTextCap
	}
	return max(MinTextCap, int(math.Floor(n)))
}

// envFlag mirrors the TS envFlag: "1", "true", "yes" and "on" (any case, either
// side whitespace) are true; an unset or blank variable yields fallback; any
// other value is false.
func envFlag(name string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// TextCap is the effective reply cap: CTX_ORCH_TEXT_CAP, never below MinTextCap,
// and DefaultTextCap whenever the variable is unset or unusable (see parseTextCap).
func TextCap() int {
	return parseTextCap(os.Getenv("CTX_ORCH_TEXT_CAP"))
}

// GovernEnabled reports whether the hook acts: CTX_ORCH_GOVERN=0 turns it off.
func GovernEnabled() bool {
	return envFlag("CTX_ORCH_GOVERN", true)
}

// CapCodemodeResult applies the cap to one tool_result event. It returns nil when
// the reply is left alone (hook disabled, another tool, a nested call, nothing to
// shorten, or already under the cap). Otherwise it returns the result fields the
// host replaces: content plus the details/structuredContent/isError/usage the
// event carried, passed through explicitly.
//
// The event map is what Pig's tool_result payload decodes to, so the keys are the
// wire names: toolName, parentToolCallId, content, details, structuredContent,
// isError, usage. TS: capCodemodeResult.
func CapCodemodeResult(event map[string]any) map[string]any {
	if !GovernEnabled() {
		return nil
	}
	if name, _ := event["toolName"].(string); name != "codemode" {
		return nil
	}
	// A nested result only feeds the running script; do not govern it.
	if parent, ok := event["parentToolCallId"].(string); ok && parent != "" {
		return nil
	}
	blocks, ok := event["content"].([]any)
	if !ok || len(blocks) == 0 {
		return nil
	}

	total := 0
	for _, block := range blocks {
		if text, ok := textBlockText(block); ok {
			total += utf16Len(text)
		}
	}
	if total <= GovernMinTotal {
		return nil
	}
	cap := TextCap()
	if total <= cap {
		return nil
	}

	// Reserve room for the marker so the governed reply still fits under the cap.
	budget := max(0, cap-markerReserve)
	kept := make([]any, 0, len(blocks))
	remaining := budget
	dropped := 0
	for _, block := range blocks {
		text, isText := textBlockText(block)
		if !isText {
			kept = append(kept, block)
			continue
		}
		length := utf16Len(text)
		if length <= remaining {
			kept = append(kept, block)
			remaining -= length
			continue
		}
		if remaining > 0 {
			// A new block rather than an in-place edit: the host may still be
			// reading the blocks it handed over.
			if replaced, ok := withText(block, truncateUTF16(text, remaining)); ok {
				kept = append(kept, replaced)
			}
		}
		dropped += length - max(0, remaining)
		remaining = 0
	}

	// details.fullOutputPath is where the runner spooled the full output, when it
	// did. The marker names it so the model can read the rest instead of re-running.
	details := event["details"]
	fullOutputPath := ""
	if object, ok := details.(map[string]any); ok {
		if p, ok := object["fullOutputPath"].(string); ok {
			fullOutputPath = p
		}
	}
	marker := fmt.Sprintf("[…ctx-orchestrate 治理] codemode 回传 %d 字符超过硬顶 %d，已物理截断（丢弃约 %d 字符）。", total, cap, dropped)
	if fullOutputPath != "" {
		marker += fmt.Sprintf("完整输出已存至 %s，用 read 精确取片段（无需重跑）。", fullOutputPath)
	} else {
		marker += "请在脚本内自己收敛：只回摘要/计数/关键路径，需要全文就写入文件后用 read 精确取片段。"
	}

	lastText := -1
	for i, block := range kept {
		if _, ok := textBlockText(block); ok {
			lastText = i
		}
	}
	if lastText >= 0 {
		text, _ := textBlockText(kept[lastText])
		if replaced, ok := withText(kept[lastText], text+"\n"+marker); ok {
			kept[lastText] = replaced
		}
	} else {
		kept = append(kept, map[string]any{"type": "text", "text": marker})
	}

	diagLog(fmt.Sprintf("codemode result capped: total=%d cap=%d dropped=%d blocks=%d", total, cap, dropped, len(kept)))

	// The four members are passed through explicitly: losing details loses
	// fullOutputPath, and replacing content without structuredContent makes the
	// host drop the structured content. A member the event did not carry is left
	// out, as an undefined TS member is dropped by JSON.stringify.
	result := map[string]any{"content": kept}
	if value, ok := event["details"]; ok {
		result["details"] = value
	}
	if value, ok := event["structuredContent"]; ok {
		result["structuredContent"] = value
	}
	if value, ok := event["isError"]; ok {
		result["isError"] = value
	}
	if value, ok := event["usage"]; ok {
		result["usage"] = value
	}
	return result
}

// textBlockText reads a content block's text when it is a text block.
func textBlockText(block any) (string, bool) {
	object, ok := block.(map[string]any)
	if !ok {
		return "", false
	}
	if kind, _ := object["type"].(string); kind != "text" {
		return "", false
	}
	text, _ := object["text"].(string)
	return text, true
}

// withText returns a copy of a text block with its text replaced, keeping every
// other member (cacheControl and the like) as the TS spread kept it.
func withText(block any, text string) (map[string]any, bool) {
	object, ok := block.(map[string]any)
	if !ok {
		return nil, false
	}
	copied := make(map[string]any, len(object)+1)
	for key, value := range object {
		copied[key] = value
	}
	copied["type"] = "text"
	copied["text"] = text
	return copied, true
}
