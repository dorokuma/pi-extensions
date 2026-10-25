package pivcc

import "strings"

// Ports src/core/filter-noise.ts: drop system noise and boilerplate user turns
// before extraction.

// noiseTools is filter-noise.ts NOISE_TOOLS: tool names whose calls and results
// never reach the summary.
var noiseTools = map[string]bool{
	"TodoWrite": true, "TodoRead": true, "ToolSearch": true, "WebSearch": true,
	"AskUser": true, "ExitSpecMode": true, "GenerateDroid": true,
}

// noiseStrings is filter-noise.ts NOISE_STRINGS.
var noiseStrings = []string{
	"Continue from where you left off.",
	"No response requested.",
	"IMPORTANT: TodoWrite was not called yet.",
}

// xmlWrapperRe is filter-noise.ts XML_WRAPPER_RE, retargeted at JS `.` (which
// never matches a line terminator) and with the backreference spelled out.
func xmlWrapperReFor(kind string) *mustRegexp {
	return newMustRegexp(`<(` + kind + `)[^>]*>[\s\S]*?</\1>`)
}

// xmlWrapperRe is the general form: any of the four wrapper element names,
// each closed by its own tag. RE2 has no backreferences, so the alternation is
// enumerated, which is equivalent for this fixed name set.
var xmlWrapperRe = mustCompile(
	`<(?:system-reminder|ide_opened_file|command-message|context-window-usage)[^>]*>` +
		`[\s\S]*?</(?:system-reminder|ide_opened_file|command-message|context-window-usage)>`,
)

// isNoiseUserBlock is filter-noise.ts isNoiseUserBlock().
func isNoiseUserBlock(text string) bool {
	trimmed := jsTrimSpace(text)
	for _, noise := range noiseStrings {
		if strings.Contains(trimmed, noise) {
			return true
		}
	}
	stripped := jsTrimSpace(xmlWrapperRe.ReplaceAllString(trimmed, ""))
	return len(stripped) == 0
}

// cleanUserText is filter-noise.ts cleanUserText().
func cleanUserText(text string) string {
	return jsTrimSpace(xmlWrapperRe.ReplaceAllString(text, ""))
}

// FilterNoise is filter-noise.ts filterNoise(): it drops noise tool calls and
// results, drops noise-only user blocks, and cleans the surviving user text.
// The cleaned user block keeps no source index, exactly as TS rebuilds it as
// `{ kind: "user", text: cleaned }`.
func FilterNoise(blocks []NormalizedBlock) []NormalizedBlock {
	out := make([]NormalizedBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Kind == KindToolCall && noiseTools[block.Name] {
			continue
		}
		if block.Kind == KindToolResult && noiseTools[block.Name] {
			continue
		}
		if block.Kind == KindUser {
			if isNoiseUserBlock(block.Text) {
				continue
			}
			cleaned := cleanUserText(block.Text)
			if cleaned == "" {
				continue
			}
			out = append(out, NormalizedBlock{Kind: KindUser, Text: cleaned})
			continue
		}
		out = append(out, block)
	}
	return out
}

var _ = xmlWrapperReFor
