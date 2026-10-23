package picontextpruning

import (
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// SessionState encapsulates runtime session states for context pruning.
type SessionState struct {
	mu             sync.Mutex
	Config         PruneConfig
	SessionStats   PruneStats
	LastPass       *PruneStats
	PrunePasses    int
	ForceNextPrune bool
}

// NewSessionState constructs a new SessionState initialized with config.
func NewSessionState(initialConfig PruneConfig) *SessionState {
	return &SessionState{
		Config:       initialConfig,
		SessionStats: EmptyStats(),
	}
}

// Reset resets the session statistics and state for a new session.
func (s *SessionState) Reset(config PruneConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Config = config
	s.SessionStats = EmptyStats()
	s.LastPass = nil
	s.PrunePasses = 0
	s.ForceNextPrune = false
}

// toFixed1 formats a non-negative float64 with 1 decimal digit conforming to ECMA-262 Number.prototype.toFixed(1).
// Uses exact rational arithmetic to match JavaScript's half-up rounding (e.g., 1.25 -> "1.3").
func toFixed1(x float64) string {
	ratX := new(big.Rat).SetFloat64(x)
	ratScaled := new(big.Rat).Mul(ratX, big.NewRat(10, 1))

	num := ratScaled.Num()
	denom := ratScaled.Denom()

	twoNum := new(big.Int).Mul(num, big.NewInt(2))
	numerator := new(big.Int).Add(twoNum, denom)
	twoDenom := new(big.Int).Mul(denom, big.NewInt(2))
	n := new(big.Int).Quo(numerator, twoDenom)

	nStr := n.String()
	if len(nStr) <= 1 {
		zeros := strings.Repeat("0", 2-len(nStr))
		nStr = zeros + nStr
	}
	k := len(nStr)
	return nStr[:k-1] + "." + nStr[k-1:]
}

// FormatTokens converts a token count into a compact human-readable string (e.g. 1.2K, 3.4M).
// Aligns directly with TS formatTokens (${(value / 1_000_000).toFixed(1)}M or ${(value / 1_000).toFixed(1)}K).
func FormatTokens(value int) string {
	if value >= 1_000_000 {
		return toFixed1(float64(value)/1_000_000.0) + "M"
	}
	if value >= 1_000 {
		return toFixed1(float64(value)/1_000.0) + "K"
	}
	return strconv.Itoa(value)
}

// FormatStatsMessage formats session pruning metrics for the /prune-stats command.
// Aligns directly with TS /prune-stats handler output formatting.
func FormatStatsMessage(prunePasses int, sessionStats PruneStats, lastPass *PruneStats) string {
	if prunePasses == 0 {
		return "No pruning has occurred yet this session."
	}
	lines := []string{
		"Context Pruning Stats",
		fmt.Sprintf("Prune passes: %d", prunePasses),
		fmt.Sprintf("Messages pruned: %d", sessionStats.MessagesPruned),
		fmt.Sprintf("Tokens pruned: ~%s", FormatTokens(sessionStats.TokensPruned)),
		fmt.Sprintf("Messages protected: %d", sessionStats.MessagesProtected),
	}
	if lastPass != nil {
		lines = append(lines, fmt.Sprintf("Last pass: %d messages / ~%s tokens", lastPass.MessagesPruned, FormatTokens(lastPass.TokensPruned)))
	}
	return strings.Join(lines, "\n")
}

// FormatConfigMessage formats current active configuration for the /prune-config command.
// Aligns directly with TS /prune-config handler output formatting.
func FormatConfigMessage(config PruneConfig) string {
	protectedToolsStr := "(none)"
	if len(config.ProtectedTools) > 0 {
		protectedToolsStr = strings.Join(config.ProtectedTools, ", ")
	}
	prunableToolsStr := "(all non-protected)"
	if len(config.PrunableTools) > 0 {
		prunableToolsStr = strings.Join(config.PrunableTools, ", ")
	}
	lines := []string{
		"Context Pruning Config",
		fmt.Sprintf("Enabled: %v", config.Enabled),
		fmt.Sprintf("Prune minimum: %d", config.PruneMinimum),
		fmt.Sprintf("Prune protect: %d", config.PruneProtect),
		fmt.Sprintf("Protected turns: %d", config.ProtectedTurns),
		fmt.Sprintf("Protected tools: %s", protectedToolsStr),
		fmt.Sprintf("Prunable tools: %s", prunableToolsStr),
	}
	return strings.Join(lines, "\n")
}

// HandleSessionStart handles session_start lifecycle event.
func HandleSessionStart(state *SessionState, setStatus func(key, text string), cwd string, settings map[string]any) (any, error) {
	cfg := LoadConfig(cwd)
	if settings != nil {
		cfg = MergeConfig(cfg, settings)
	}
	state.Reset(cfg)
	if !cfg.Enabled && setStatus != nil {
		setStatus("context-pruning", "🔪 context pruning disabled")
	}
	return nil, nil
}

// HandleContextEvent handles context event, applying pruning when enabled or forced.
func HandleContextEvent(state *SessionState, setStatus func(key, text string), data map[string]any) (any, error) {
	if data == nil {
		return nil, nil
	}

	state.mu.Lock()
	enabled := state.Config.Enabled
	force := state.ForceNextPrune
	if !enabled && !force {
		state.mu.Unlock()
		return nil, nil
	}
	effective := state.Config
	if force {
		effective.Enabled = true
		effective.PruneMinimum = 0
	}
	// 与 TS index.ts:30-31 顺序一致：先消费 forceNextPrune（即便后续消息列表为空也已消费）
	state.ForceNextPrune = false
	state.mu.Unlock()

	rawMessages, ok := data["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, nil
	}

	newMessages, stats := PruneToolOutputsAny(rawMessages, effective)
	if stats.MessagesPruned == 0 {
		if stats.TotalToolTokens > 0 && setStatus != nil {
			setStatus("context-pruning", fmt.Sprintf("🔪 %s scanned", FormatTokens(stats.TotalToolTokens)))
		}
		return nil, nil
	}

	state.mu.Lock()
	state.SessionStats.MessagesPruned += stats.MessagesPruned
	state.SessionStats.TokensPruned += stats.TokensPruned
	state.SessionStats.TotalToolTokens = stats.TotalToolTokens
	state.SessionStats.MessagesProtected = stats.MessagesProtected
	statsCopy := stats
	state.LastPass = &statsCopy
	state.PrunePasses++
	totalPruned := state.SessionStats.TokensPruned
	state.mu.Unlock()

	if setStatus != nil {
		setStatus("context-pruning", fmt.Sprintf("🔪 ~%s pruned", FormatTokens(totalPruned)))
	}
	return map[string]any{"messages": newMessages}, nil
}

// HandleCommandPrune handles /prune command.
func HandleCommandPrune(state *SessionState, setStatus func(key, text string), notify func(message, level string)) error {
	state.mu.Lock()
	state.ForceNextPrune = true
	state.mu.Unlock()

	if setStatus != nil {
		setStatus("context-pruning", "🔪 forced prune pending...")
	}
	if notify != nil {
		notify("Forced pruning queued for the next model call.", "info")
	}
	return nil
}

// HandleCommandPruneToggle handles /prune-toggle command.
func HandleCommandPruneToggle(state *SessionState, setStatus func(key, text string), notify func(message, level string)) error {
	state.mu.Lock()
	state.Config.Enabled = !state.Config.Enabled
	enabled := state.Config.Enabled
	state.mu.Unlock()

	status := "disabled"
	if enabled {
		status = "enabled"
	}
	if setStatus != nil {
		setStatus("context-pruning", fmt.Sprintf("🔪 context pruning %s", status))
	}
	if notify != nil {
		notify(fmt.Sprintf("Context pruning %s", status), "info")
	}
	return nil
}

// HandleCommandPruneStats handles /prune-stats command.
func HandleCommandPruneStats(state *SessionState, notify func(message, level string)) error {
	state.mu.Lock()
	passes := state.PrunePasses
	stats := state.SessionStats
	last := state.LastPass
	state.mu.Unlock()

	msg := FormatStatsMessage(passes, stats, last)
	if notify != nil {
		notify(msg, "info")
	}
	return nil
}

// HandleCommandPruneConfig handles /prune-config command.
func HandleCommandPruneConfig(state *SessionState, notify func(message, level string)) error {
	state.mu.Lock()
	cfg := state.Config
	state.mu.Unlock()

	msg := FormatConfigMessage(cfg)
	if notify != nil {
		notify(msg, "info")
	}
	return nil
}

// Extension constructs and returns the pi-context-pruning extension for PiG.
func Extension() *sdk.Extension {
	cwd, _ := os.Getwd()
	cfg := LoadConfig(cwd)
	state := NewSessionState(cfg)

	e := sdk.New("pi-context-pruning")

	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		var settings map[string]any
		if s, err := ctx.GetSettings(); err == nil {
			settings = s
		}
		return HandleSessionStart(state, ctx.SetStatus, ctx.Cwd(), settings)
	})

	e.OnEvent(sdk.EventContext, func(ctx sdk.Context, data map[string]any) (any, error) {
		return HandleContextEvent(state, ctx.SetStatus, data)
	})

	e.Command("prune", "Force pruning on the next model call while preserving protected turns and budget", func(ctx sdk.Context, args string) error {
		return HandleCommandPrune(state, ctx.SetStatus, ctx.Notify)
	})

	e.Command("prune-toggle", "Toggle context pruning for this session", func(ctx sdk.Context, args string) error {
		return HandleCommandPruneToggle(state, ctx.SetStatus, ctx.Notify)
	})

	e.Command("prune-stats", "Show context-pruning statistics for this session", func(ctx sdk.Context, args string) error {
		return HandleCommandPruneStats(state, ctx.Notify)
	})

	e.Command("prune-config", "Show the active context-pruning configuration", func(ctx sdk.Context, args string) error {
		return HandleCommandPruneConfig(state, ctx.Notify)
	})

	return e
}
