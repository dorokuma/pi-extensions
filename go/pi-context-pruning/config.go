package picontextpruning

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

// DEFAULT_CONFIG provides the fallback settings for context pruning.
// Aligns directly with TS DEFAULT_CONFIG.
var DEFAULT_CONFIG = PruneConfig{
	Enabled:        true,
	PruneMinimum:   20000,
	PruneProtect:   40000,
	ProtectedTurns: 2,
	ProtectedTools: []string{},
	PrunableTools:  []string{},
}

func cloneStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneConfig(base PruneConfig) PruneConfig {
	return PruneConfig{
		Enabled:        base.Enabled,
		PruneMinimum:   base.PruneMinimum,
		PruneProtect:   base.PruneProtect,
		ProtectedTurns: base.ProtectedTurns,
		ProtectedTools: cloneStrings(base.ProtectedTools),
		PrunableTools:  cloneStrings(base.PrunableTools),
	}
}

// ValidInteger checks whether the given value represents a finite, non-negative integer.
// When value exceeds the representable range of int (such as 1e19 in JSON), it caps
// at math.MaxInt to match TS semantics (maintaining a large value that never triggers pruning)
// rather than overflowing into a negative number.
// Aligns directly with TS validInteger.
func ValidInteger(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		if v >= 0 {
			return v, true
		}
	case int64:
		if v >= 0 {
			if v > math.MaxInt {
				return math.MaxInt, true
			}
			return int(v), true
		}
	case int32:
		if v >= 0 {
			return int(v), true
		}
	case uint:
		if uint64(v) > math.MaxInt {
			return math.MaxInt, true
		}
		return int(v), true
	case uint64:
		if v > math.MaxInt {
			return math.MaxInt, true
		}
		return int(v), true
	case uint32:
		return int(v), true
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && math.Floor(v) == v && v >= 0 {
			if v >= float64(math.MaxInt) {
				return math.MaxInt, true
			}
			return int(v), true
		}
	case float32:
		fv := float64(v)
		if !math.IsNaN(fv) && !math.IsInf(fv, 0) && math.Floor(fv) == fv && fv >= 0 {
			if fv >= float64(math.MaxInt) {
				return math.MaxInt, true
			}
			return int(fv), true
		}
	}
	return 0, false
}

// ValidStrings checks whether the given value is an array of strings.
// Aligns directly with TS validStrings.
func ValidStrings(value any) ([]string, bool) {
	switch v := value.(type) {
	case []string:
		return cloneStrings(v), true
	case []any:
		res := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			res[i] = s
		}
		return res, true
	default:
		return nil, false
	}
}

func extractIncomingMap(settings any) map[string]any {
	if settings == nil {
		return nil
	}
	switch s := settings.(type) {
	case ContextPruningSettings:
		return s.ContextPruning
	case *ContextPruningSettings:
		if s == nil {
			return nil
		}
		return s.ContextPruning
	case map[string]any:
		if rawIncoming, ok := s["contextPruning"]; ok && rawIncoming != nil {
			if m, ok := rawIncoming.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

// MergeConfig merges user or project settings into a base PruneConfig.
// Invalid fields are rejected while valid existing fields are preserved.
// Aligns directly with TS mergeConfig.
func MergeConfig(base PruneConfig, settings any) PruneConfig {
	incoming := extractIncomingMap(settings)
	if incoming == nil {
		return cloneConfig(base)
	}

	next := cloneConfig(base)

	if enabledVal, ok := incoming["enabled"]; ok {
		if enabled, ok := enabledVal.(bool); ok {
			next.Enabled = enabled
		}
	}

	if pruneMinVal, ok := incoming["pruneMinimum"]; ok {
		if n, ok := ValidInteger(pruneMinVal); ok {
			next.PruneMinimum = n
		}
	}

	if pruneProtectVal, ok := incoming["pruneProtect"]; ok {
		if n, ok := ValidInteger(pruneProtectVal); ok {
			next.PruneProtect = n
		}
	}

	if protectedTurnsVal, ok := incoming["protectedTurns"]; ok {
		if n, ok := ValidInteger(protectedTurnsVal); ok {
			next.ProtectedTurns = n
		}
	}

	if protectedToolsVal, ok := incoming["protectedTools"]; ok {
		if s, ok := ValidStrings(protectedToolsVal); ok {
			next.ProtectedTools = s
		}
	}

	if prunableToolsVal, ok := incoming["prunableTools"]; ok {
		if s, ok := ValidStrings(prunableToolsVal); ok {
			next.PrunableTools = s
		}
	}

	return next
}

func readJSONSafe(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var res map[string]any
	if err := json.Unmarshal(data, &res); err != nil {
		return nil
	}
	return res
}

// resolveGlobalSettingsPath resolves the path to the global settings file,
// considering PIG_HOME when set, and falling back to ~/.pi/agent/settings.json.
func resolveGlobalSettingsPath() string {
	pigHome := os.Getenv("PIG_HOME")
	if pigHome != "" {
		p1 := filepath.Join(pigHome, "agent", "settings.json")
		if _, err := os.Stat(p1); err == nil {
			return p1
		}
		p2 := filepath.Join(pigHome, "settings.json")
		if _, err := os.Stat(p2); err == nil {
			return p2
		}
	}

	homeDir, err := os.UserHomeDir()
	if err == nil && homeDir != "" {
		return filepath.Join(homeDir, ".pi", "agent", "settings.json")
	}
	return ""
}

// LoadConfig loads and merges global settings and project settings from disk.
// Aligns directly with TS loadConfig.
func LoadConfig(cwd string) PruneConfig {
	var globalSettings map[string]any
	globalPath := resolveGlobalSettingsPath()
	if globalPath != "" {
		globalSettings = readJSONSafe(globalPath)
	}

	var projectSettings map[string]any
	if cwd != "" {
		projectPath := filepath.Join(cwd, ".pi", "settings.json")
		projectSettings = readJSONSafe(projectPath)
	}

	return MergeConfig(MergeConfig(DEFAULT_CONFIG, globalSettings), projectSettings)
}
