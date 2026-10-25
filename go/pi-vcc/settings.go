package pivcc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Ports src/core/settings.ts.

// DefaultCharsPerToken etc. are token-estimate.ts constants used by settings'
// neighbour modules; they live in tokenestimate.go.

// PiVccSettings is settings.ts PiVccSettings.
type PiVccSettings struct {
	// OverrideDefaultCompaction makes pi-vcc handle every compaction.
	OverrideDefaultCompaction bool
	// SmartKeepTail boosts the default keep:1 tail when it is small.
	SmartKeepTail bool
	// ContinueAfterThresholdCompact permits pi-vcc's own continue after an
	// automatic compaction on pi versions that still need it.
	ContinueAfterThresholdCompact bool
	// Debug writes a debug snapshot to /tmp/pi-vcc-debug.json.
	Debug bool
	// SkipForProviders defers compaction to these providers (case-insensitive).
	SkipForProviders []string
	// SkipCustomTypes excludes custom_message entries from the summarizer input.
	SkipCustomTypes []string
	// TrackCommands lists command names to remember across compactions.
	TrackCommands []string
}

// DefaultSettings is settings.ts DEFAULT_SETTINGS. Key order matches the TS
// object so the scaffolded file reads the same.
func DefaultSettings() PiVccSettings {
	return PiVccSettings{
		OverrideDefaultCompaction:     true,
		SmartKeepTail:                 true,
		ContinueAfterThresholdCompact: true,
		Debug:                         false,
		SkipForProviders:              []string{},
		SkipCustomTypes:               []string{},
		TrackCommands:                 []string{},
	}
}

// defaultSettingsJSON is what scaffoldSettings writes for a fresh install: the
// TS `JSON.stringify(DEFAULT_SETTINGS, null, 2) + "\n"`.
const defaultSettingsJSON = `{
  "overrideDefaultCompaction": true,
  "smartKeepTail": true,
  "continueAfterThresholdCompact": true,
  "debug": false,
  "skipForProviders": [],
  "skipCustomTypes": [],
  "trackCommands": []
}
`

// settingsKeyOrder is the DEFAULT_SETTINGS key order, used both to scaffold
// missing keys and to preserve the file's own key order when rewriting it.
var settingsKeyOrder = []string{
	"overrideDefaultCompaction",
	"smartKeepTail",
	"continueAfterThresholdCompact",
	"debug",
	"skipForProviders",
	"skipCustomTypes",
	"trackCommands",
}

// SettingsPath is settings.ts settingsPath(): $PI_VCC_CONFIG_PATH when set,
// else the config file in pi's agent directory.
//
// pi's getAgentDir() honours $PI_CODING_AGENT_DIR; this port additionally
// honours PIG_HOME/agent and PIG_CODING_AGENT_DIR so an isolated PiG run never
// reads or writes the production ~/.pi/agent file (the migration's hard rule).
// The resolution order is documented in README.md (差异 D1).
func SettingsPath() string {
	if custom := strings.TrimSpace(os.Getenv("PI_VCC_CONFIG_PATH")); custom != "" {
		return custom
	}
	return filepath.Join(AgentDir(), "pi-vcc-config.json")
}

// AgentDir is pi's getAgentDir(): $PI_CODING_AGENT_DIR, else the isolated PiG
// home's agent dir, else ~/.pi/agent.
func AgentDir() string {
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return expandHome(dir)
	}
	if dir := strings.TrimSpace(os.Getenv("PIG_CODING_AGENT_DIR")); dir != "" {
		return expandHome(dir)
	}
	if home := strings.TrimSpace(os.Getenv("PIG_HOME")); home != "" {
		return filepath.Join(expandHome(home), "agent")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".pi", "agent")
	}
	return ""
}

// expandHome expands a leading "~" the way a shell would.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// readJSONObject is settings.ts readJson(): a parsed object, or nil when the
// file is missing, unreadable, or not a JSON object.
func readJSONObject(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil
	}
	return object
}

// coerceStringArray is settings.ts coerceStringArray(): only string elements
// survive, and a non-array value fails closed to empty.
func coerceStringArray(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, isString := item.(string); isString {
			out = append(out, s)
		}
	}
	return out
}

// LoadSettings is settings.ts loadSettings(). The path is injectable for tests;
// LoadSettingsDefault uses SettingsPath().
func LoadSettings(path string) PiVccSettings {
	merged := DefaultSettings()
	parsed := readJSONObject(path)
	if parsed == nil {
		return merged
	}
	merged = mergeSettings(merged, parsed)
	// A blind spread would leak a malformed array value (e.g. a bare string,
	// where .includes becomes substring matching) into the provider check.
	merged.SkipForProviders = coerceStringArray(parsed["skipForProviders"])
	merged.SkipCustomTypes = coerceStringArray(parsed["skipCustomTypes"])
	// Blank names would match every command; duplicates would repeat lines.
	merged.TrackCommands = dedupeTrimmedNonEmpty(coerceStringArray(parsed["trackCommands"]))
	return merged
}

// LoadSettingsDefault loads the settings from SettingsPath().
func LoadSettingsDefault() PiVccSettings {
	return LoadSettings(SettingsPath())
}

// mergeSettings is `{ ...DEFAULT_SETTINGS, ...parsed }` with each boolean field
// coerced the way TS's truthiness leaves it when the key is present: a
// non-boolean value reaches the boolean field as JSON's own truthiness, so only
// a literal false (or another falsy JSON value) turns it off.
func mergeSettings(base PiVccSettings, parsed map[string]any) PiVccSettings {
	if value, present := parsed["overrideDefaultCompaction"]; present {
		base.OverrideDefaultCompaction = jsonTruthy(value)
	}
	if value, present := parsed["smartKeepTail"]; present {
		base.SmartKeepTail = jsonTruthy(value)
	}
	if value, present := parsed["continueAfterThresholdCompact"]; present {
		base.ContinueAfterThresholdCompact = jsonTruthy(value)
	}
	if value, present := parsed["debug"]; present {
		base.Debug = jsonTruthy(value)
	}
	return base
}

// jsonTruthy is JS truthiness for a JSON value.
func jsonTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case float64:
		return value != 0
	case string:
		return value != ""
	}
	return true
}

// dedupeTrimmedNonEmpty is `[...new Set(arr.map(s => s.trim()).filter(Boolean))]`.
func dedupeTrimmedNonEmpty(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		trimmed := jsTrimSpace(item)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// ScaffoldSettings is settings.ts scaffoldSettings(), at SettingsPath():
//   - missing file  -> created with the full default block,
//   - invalid JSON  -> untouched,
//   - valid object  -> missing default keys filled in, existing values kept.
//
// Errors are swallowed: TS's scaffoldSettings is best-effort so a broken file
// never crashes extension load.
func ScaffoldSettings() {
	ScaffoldSettingsAt(SettingsPath())
}

// ScaffoldSettingsAt is ScaffoldSettings for an explicit path (tests).
func ScaffoldSettingsAt(path string) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			_ = os.WriteFile(path, []byte(defaultSettingsJSON), 0o644)
		}
		return
	}
	parsed := readJSONObject(path)
	if parsed == nil {
		return // don't clobber
	}
	changed := false
	for _, key := range settingsKeyOrder {
		if _, present := parsed[key]; !present {
			parsed[key] = defaultSettingsValue(key)
			changed = true
		}
	}
	if !changed {
		return
	}
	_ = os.WriteFile(path, []byte(marshalOrderedSettings(parsed)), 0o644)
}

// defaultSettingsValue is DEFAULT_SETTINGS[key] as a JSON value.
func defaultSettingsValue(key string) any {
	switch key {
	case "overrideDefaultCompaction", "smartKeepTail", "continueAfterThresholdCompact":
		return true
	case "debug":
		return false
	case "skipForProviders", "skipCustomTypes", "trackCommands":
		return []any{}
	}
	return nil
}

// marshalOrderedSettings writes the known keys first in DEFAULT_SETTINGS order,
// then any unknown keys in sorted order, 2-space indented, with a trailing
// newline — the TS `JSON.stringify(next, null, 2) + "\n"` shape.
func marshalOrderedSettings(parsed map[string]any) string {
	var b strings.Builder
	b.WriteString("{\n")
	written := 0
	writeField := func(key string, value any) {
		if written > 0 {
			b.WriteString(",\n")
		}
		written++
		b.WriteString("  ")
		b.WriteString(quoteJSONString(key))
		b.WriteString(": ")
		b.WriteString(indentJSON(value, 2))
	}
	for _, key := range settingsKeyOrder {
		if value, present := parsed[key]; present {
			writeField(key, value)
		}
	}
	rest := make([]string, 0, len(parsed))
	for key := range parsed {
		if !isSettingsKey(key) {
			rest = append(rest, key)
		}
	}
	sortStrings(rest)
	for _, key := range rest {
		writeField(key, parsed[key])
	}
	if written > 0 {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func isSettingsKey(key string) bool {
	for _, known := range settingsKeyOrder {
		if known == key {
			return true
		}
	}
	return false
}
