package autocontinue

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Persist mirrors TS PersistShape (auto-continue.ts:558-562). Field order and
// names match the JSON TS writes with JSON.stringify(state, null, 2).
type Persist struct {
	Enabled     bool `json:"enabled"`
	MaxAttempts int  `json:"maxAttempts"`
	DelayMs     int  `json:"delayMs"`
}

// DefaultPersist mirrors TS defaultPersist (auto-continue.ts:573-575).
func DefaultPersist() Persist {
	return Persist{Enabled: true, MaxAttempts: DefaultMax, DelayMs: DefaultDelayMs}
}

// SettingsPath resolves the preference file.
//
// TS settingsPath (auto-continue.ts:564-566) is
// `join(homedir(), ".pi", "agent", "auto-continue.json")`. This port resolves
// the same file under the agent directory resolveAgentDir returns:
// PIG_EXT_HOST_INFO.agentDir, then PI_/PIG_CODING_AGENT_DIR, then
// $PIG_HOME/agent, else ~/.pig/agent — never ~/.pi, so a production or
// isolated PiG run can no longer create /root/.pi/agent/auto-continue.json.
func SettingsPath() string {
	if dir := resolveAgentDir(); dir != "" {
		return filepath.Join(dir, "auto-continue.json")
	}
	return ""
}

// clampInt mirrors TS clampInt (auto-continue.ts:568-571): a non-finite value
// falls back, everything else is floored and clamped.
func clampInt(n float64, min, max, fallback int) int {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return fallback
	}
	return int(math.Min(float64(max), math.Max(float64(min), math.Floor(n))))
}

// stringField reads a present-but-possibly-null JSON field: `has` is
// JS's "property exists" and a missing property is `undefined`.
func stringField(parsed map[string]any, key string) (any, bool) {
	value, ok := parsed[key]
	return value, ok
}

// ParsePersist decodes the preference file's contents, mirroring TS loadPersist
// (auto-continue.ts:577-591): anything unreadable or not a JSON object is a
// parse failure, and every individual field is coerced the way TS's
// `Number(parsed.field)` plus clampInt does.
func ParsePersist(raw string) (Persist, bool) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed == nil {
		// JSON.parse throws, or the value is null and reading .enabled throws;
		// both land in TS's catch.
		return DefaultPersist(), false
	}
	fallback := DefaultPersist()
	out := Persist{
		// TS: `parsed.enabled !== false` — true unless the value is exactly false.
		Enabled: true,
	}
	if value, ok := parsed["enabled"]; ok {
		if b, isBool := value.(bool); isBool && !b {
			out.Enabled = false
		}
	}
	maxAttempts := math.NaN()
	if value, ok := stringField(parsed, "maxAttempts"); ok {
		if number, isNumber := jsNumber(value); isNumber {
			maxAttempts = number
		}
	}
	out.MaxAttempts = clampInt(maxAttempts, MinAttempts, MaxAttemptsLimit, fallback.MaxAttempts)
	delay := math.NaN()
	if value, ok := stringField(parsed, "delayMs"); ok {
		if number, isNumber := jsNumber(value); isNumber {
			delay = number
		}
	}
	out.DelayMs = clampInt(delay, 0, MaxDelayMs, fallback.DelayMs)
	return out, true
}

// loadPersist mirrors TS loadPersist (auto-continue.ts:577): on any failure the
// defaults are written back and returned.
func loadPersist(path string) Persist {
	if path == "" {
		return DefaultPersist()
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if parsed, ok := ParsePersist(string(raw)); ok {
			return parsed
		}
	}
	fallback := DefaultPersist()
	savePersist(path, fallback)
	return fallback
}

// savePersist mirrors TS savePersist (auto-continue.ts:593-597):
// `${JSON.stringify(state, null, 2)}\n`.
func savePersist(path string, state Persist) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(body, '\n'), 0o644)
}

// envEnabledOverride mirrors TS envEnabledOverride (auto-continue.ts:599-606).
func envEnabledOverride(lookup func(string) (string, bool)) *bool {
	raw, ok := lookup("PI_AUTO_CONTINUE")
	if !ok {
		return nil
	}
	value := strings.ToLower(jsTrim(raw))
	switch value {
	case "":
		return nil
	case "0", "false", "off", "no":
		no := false
		return &no
	case "1", "true", "on", "yes":
		yes := true
		return &yes
	}
	return nil
}

// envInt mirrors TS envInt (auto-continue.ts:607-613): unset or blank keeps the
// fallback, a non-numeric value keeps the fallback, and a valid number is
// floored and clamped.
func envInt(lookup func(string) (string, bool), name string, fallback, min, max int) int {
	raw, ok := lookup(name)
	if !ok || jsTrim(raw) == "" {
		return fallback
	}
	number, isNumber := jsNumber(raw)
	if !isNumber {
		return fallback
	}
	return clampInt(number, min, max, fallback)
}
