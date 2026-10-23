package picontextpruning

// PruneConfig defines runtime configuration parameters for proactive context pruning.
// Aligns directly with TS interface PruneConfig.
type PruneConfig struct {
	Enabled        bool     `json:"enabled"`
	PruneMinimum   int      `json:"pruneMinimum"`
	PruneProtect   int      `json:"pruneProtect"`
	ProtectedTurns int      `json:"protectedTurns"`
	ProtectedTools []string `json:"protectedTools"`
	PrunableTools  []string `json:"prunableTools"`
}

// PruneStats captures token and message count statistics for a single pass or aggregated across a session.
// Aligns directly with TS interface PruneStats.
type PruneStats struct {
	MessagesPruned    int `json:"messagesPruned"`
	TokensPruned      int `json:"tokensPruned"`
	TotalToolTokens   int `json:"totalToolTokens"`
	MessagesProtected int `json:"messagesProtected"`
}

// ContextPruningSettings represents the top-level container structure in settings.json.
// Aligns with TS interface ContextPruningSettings.
type ContextPruningSettings struct {
	ContextPruning map[string]any `json:"contextPruning,omitempty"`
}
