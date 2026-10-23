package picontextpruning

import (
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Candidate tracks a candidate message identified for pruning.
type Candidate struct {
	Index    int
	Tokens   int
	ToolName string
}

// EmptyStats initializes an empty PruneStats counter.
// Aligns directly with TS emptyStats.
func EmptyStats() PruneStats {
	return PruneStats{
		MessagesPruned:    0,
		TokensPruned:      0,
		TotalToolTokens:   0,
		MessagesProtected: 0,
	}
}

// IsPrunable determines whether a tool's output is eligible for pruning based on config allow/block lists.
// Aligns directly with TS isPrunable.
func IsPrunable(toolName string, config PruneConfig) bool {
	for _, protected := range config.ProtectedTools {
		if protected == toolName {
			return false
		}
	}
	if len(config.PrunableTools) == 0 {
		return true
	}
	for _, prunable := range config.PrunableTools {
		if prunable == toolName {
			return true
		}
	}
	return false
}

// FormatWithCommas formats an integer with thousands comma separators, mimicking JS toLocaleString().
func FormatWithCommas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	remainder := len(s) % 3
	if remainder > 0 {
		b.WriteString(s[:remainder])
	}
	for i := remainder; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// PruneMarker generates the replacement content marker for pruned tool outputs.
// Aligns directly with TS pruneMarker.
func PruneMarker(toolName string, tokens int) string {
	return fmt.Sprintf("[output pruned — ~%s tokens | %s]", FormatWithCommas(tokens), toolName)
}

// PruneToolOutputs executes the backward-scan pruning algorithm over a slice of messages.
// Returns the pruned message slice and pruning statistics.
// Aligns directly with TS pruneToolOutputs.
func PruneToolOutputs(messages []map[string]any, config PruneConfig) ([]map[string]any, PruneStats) {
	stats := EmptyStats()
	var candidates []Candidate
	userTurns := 0
	protectedTokens := 0
	prunableTokens := 0

	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		role, _ := message["role"].(string)
		if role == "user" {
			userTurns++
			continue
		}
		if role == "compactionSummary" {
			break
		}
		if role != "toolResult" {
			continue
		}

		toolName, _ := message["toolName"].(string)
		isError, _ := message["isError"].(bool)
		tokens := sdk.EstimateTokens(message)
		stats.TotalToolTokens += tokens

		if userTurns < config.ProtectedTurns || !IsPrunable(toolName, config) || isError {
			stats.MessagesProtected++
			continue
		}
		if protectedTokens < config.PruneProtect {
			protectedTokens += tokens
			stats.MessagesProtected++
			continue
		}
		prunableTokens += tokens
		candidates = append(candidates, Candidate{
			Index:    index,
			Tokens:   tokens,
			ToolName: toolName,
		})
	}

	if len(candidates) == 0 || prunableTokens < config.PruneMinimum {
		return messages, stats
	}

	newMessages := make([]map[string]any, len(messages))
	copy(newMessages, messages)

	for _, candidate := range candidates {
		orig := newMessages[candidate.Index]
		copied := make(map[string]any, len(orig)+1)
		for k, v := range orig {
			copied[k] = v
		}
		copied["content"] = []any{
			map[string]any{
				"type": "text",
				"text": PruneMarker(candidate.ToolName, candidate.Tokens),
			},
		}
		newMessages[candidate.Index] = copied
		stats.MessagesPruned++
		stats.TokensPruned += candidate.Tokens
	}

	return newMessages, stats
}

// PruneToolOutputsAny is a bridge helper accepting generic []any message slices (from wire protocol data).
func PruneToolOutputsAny(rawMessages []any, config PruneConfig) ([]any, PruneStats) {
	if len(rawMessages) == 0 {
		return rawMessages, EmptyStats()
	}

	converted := make([]map[string]any, len(rawMessages))
	for i, raw := range rawMessages {
		if m, ok := raw.(map[string]any); ok {
			converted[i] = m
		} else {
			converted[i] = map[string]any{}
		}
	}

	pruned, stats := PruneToolOutputs(converted, config)
	if stats.MessagesPruned == 0 {
		return rawMessages, stats
	}

	result := make([]any, len(pruned))
	for i, m := range pruned {
		result[i] = m
	}
	return result, stats
}
