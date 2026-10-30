// Package pienv implements the pi-env extension for PiG in Go.
//
// Background & Motivation:
// In upstream Pi, pi-env reads an "env" key from settings.json and sets the
// values as process environment variables at startup. It honors Pi's settings
// hierarchy (project-level .pi/settings.json overrides global ~/.pi/agent/settings.json),
// supports $VAR and ${VAR} interpolation (with $$ escaping to a literal $),
// registers a custom message renderer for "pi-env", filters custom messages
// from the LLM context, registers an /env slash command, and presents warnings
// on session_start if non-scalar values, unresolved variables, or existing
// environment variables are overridden.
package pienv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	// ConfigKey is the settings key inspected for environment variable definitions.
	ConfigKey = "env"

	// MessageType is the custom message type registered and filtered by pi-env.
	MessageType = "pi-env"

	// TrackerKey stores the JSON array of keys managed by this extension across reloads.
	TrackerKey = "_PI_EXT_ENV_KEYS"

	// OverridesKey stores the JSON array of existing system environment variables overridden by this extension.
	OverridesKey = "_PI_EXT_ENV_OVERRIDES"
)

// envVarPattern matches $$, ${VAR}, or $VAR where VAR matches [A-Za-z_][A-Za-z0-9_]*.
// Matches the TS implementation's ENV_VAR_PATTERN.
var envVarPattern = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// EnvReport records variables set for one settings source scope ("global" or "project").
type EnvReport struct {
	Source    string            `json:"source"`
	Keys      []string          `json:"keys,omitempty"`
	Variables map[string]string `json:"variables"`
}

// ApplyResult contains the outcome of applying environment variables from settings.
type ApplyResult struct {
	Reports        []EnvReport
	UnresolvedVars []string
	OverriddenVars []string
	NonScalarKeys  []string
}

// ThemeRenderer models a terminal theme styling foreground colors, matching sdk.UITheme.
type ThemeRenderer interface {
	Fg(token, text string) string
}

// ResolveValue resolves $VAR and ${VAR} references in value using lookup.
// $$ escapes to a literal $. If a referenced variable is not defined in lookup,
// it is added to missing and replaced with an empty string.
func ResolveValue(value string, lookup func(string) (string, bool)) (string, []string) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var missing []string
	resolved := envVarPattern.ReplaceAllStringFunc(value, func(match string) string {
		if match == "$$" {
			return "$"
		}
		var varName string
		if strings.HasPrefix(match, "${") && strings.HasSuffix(match, "}") {
			varName = match[2 : len(match)-1]
		} else if strings.HasPrefix(match, "$") {
			varName = match[1:]
		}
		val, exists := lookup(varName)
		if !exists {
			missing = append(missing, varName)
			return ""
		}
		return val
	})
	return resolved, missing
}

// ExtractEnv extracts the "env" object map from settings if it is a non-empty map.
// Returns nil if the key is missing, empty, or not an object.
// 注：仅供测试/保留兼容（运行时已切换为保序的 ParseOrderedSettingsJSON / LoadOrderedSettings）。
func ExtractEnv(settings map[string]any) map[string]any {
	if settings == nil {
		return nil
	}
	raw, ok := settings[ConfigKey]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case *OrderedEnv:
		if v == nil || v.Len() == 0 {
			return nil
		}
		return v.ToMap()
	case map[string]any:
		if len(v) == 0 {
			return nil
		}
		return v
	}
	return nil
}

// ExtractOrderedEnv extracts an *OrderedEnv from settings (either a map[string]any or *OrderedEnv).
// 注：仅供测试/保留兼容。
func ExtractOrderedEnv(settings any) *OrderedEnv {
	if settings == nil {
		return nil
	}
	switch s := settings.(type) {
	case *OrderedEnv:
		return s
	case map[string]any:
		raw, ok := s[ConfigKey]
		if !ok || raw == nil {
			return nil
		}
		return ToOrderedEnv(raw)
	}
	return nil
}

// IsScalar checks whether a value is a string, number, or boolean.
// Objects, arrays, and null return false.
func IsScalar(value any) bool {
	if value == nil {
		return false
	}
	switch value.(type) {
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		json.Number:
		return true
	default:
		return false
	}
}

// ScalarToString coerces a scalar value to string, matching String() in TypeScript.
func ScalarToString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

// OrderedEnv preserves the declaration / insertion order of environment variable keys.
type OrderedEnv struct {
	keys   []string
	values map[string]any
}

// NewOrderedEnv creates an empty OrderedEnv.
func NewOrderedEnv() *OrderedEnv {
	return &OrderedEnv{
		values: make(map[string]any),
	}
}

// NewOrderedEnvFromPairs creates an OrderedEnv from alternating key, value arguments.
// 注：仅供测试/保留兼容。
func NewOrderedEnvFromPairs(pairs ...any) *OrderedEnv {
	o := NewOrderedEnv()
	for i := 0; i+1 < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			continue
		}
		o.Set(k, pairs[i+1])
	}
	return o
}

// Set sets a key-value pair, preserving key order if newly inserted.
func (o *OrderedEnv) Set(key string, value any) {
	if o.values == nil {
		o.values = make(map[string]any)
	}
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Get retrieves the value associated with key.
func (o *OrderedEnv) Get(key string) (any, bool) {
	if o == nil || o.values == nil {
		return nil, false
	}
	v, ok := o.values[key]
	return v, ok
}

// Keys returns a copy of the ordered keys.
func (o *OrderedEnv) Keys() []string {
	if o == nil {
		return nil
	}
	res := make([]string, len(o.keys))
	copy(res, o.keys)
	return res
}

// Len returns the number of keys.
func (o *OrderedEnv) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// ToMap returns a shallow copy of values as a plain Go map.
func (o *OrderedEnv) ToMap() map[string]any {
	if o == nil || o.values == nil {
		return nil
	}
	m := make(map[string]any, len(o.values))
	for k, v := range o.values {
		m[k] = v
	}
	return m
}

// ParseOrderedSettingsJSON parses a settings.json file content in strict mode,
// extracting only the top-level "env" key as an *OrderedEnv preserving key order.
// If the "env" key is missing, null, not an object, or an empty object, it returns nil, nil.
// It will NEVER fall back to treating the top-level settings object as an environment map.
func ParseOrderedSettingsJSON(data []byte) (*OrderedEnv, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	val, err := decodeOrderedJSONValue(dec)
	if err != nil {
		return nil, err
	}

	obj, ok := val.(*OrderedEnv)
	if !ok || obj == nil {
		return nil, nil
	}

	rawEnv, ok := obj.Get(ConfigKey)
	if !ok || rawEnv == nil {
		return nil, nil
	}

	envObj, ok := rawEnv.(*OrderedEnv)
	if !ok || envObj == nil || envObj.Len() == 0 {
		return nil, nil
	}

	return envObj, nil
}

// ParseOrderedEnvJSON parses raw JSON data directly as an environment mapping (*OrderedEnv),
// preserving key order.
//
// 此入口按裸 env 对象解析，仅用于直接传 env 映射 JSON 的调用（如测试或显式 API）。
// 若传入对象包含顶层 "env" 键，则优先提取该子对象；若无 "env" 键，则回退将顶层对象直接视为环境变量映射。
// 读取 settings.json 文件的严格模式（无 "env" 键时绝不注入任何内容）请使用 ParseOrderedSettingsJSON。
func ParseOrderedEnvJSON(data []byte) (*OrderedEnv, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	val, err := decodeOrderedJSONValue(dec)
	if err != nil {
		return nil, err
	}

	obj, ok := val.(*OrderedEnv)
	if !ok || obj == nil {
		return nil, nil
	}

	// Check if this is a settings object containing an "env" key
	if rawEnv, ok := obj.Get(ConfigKey); ok {
		if envObj, ok := rawEnv.(*OrderedEnv); ok && envObj.Len() > 0 {
			return envObj, nil
		}
		return nil, nil
	}

	// Fallback: treat the top-level object directly as a raw env mapping.
	if obj.Len() == 0 {
		return nil, nil
	}
	return obj, nil
}

func decodeOrderedJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			res := NewOrderedEnv()
			for dec.More() {
				kTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kTok.(string)
				if !ok {
					return nil, fmt.Errorf("expected string key, got %T", kTok)
				}
				val, err := decodeOrderedJSONValue(dec)
				if err != nil {
					return nil, err
				}
				res.Set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return res, nil
		} else if t == '[' {
			var items []any
			for dec.More() {
				item, err := decodeOrderedJSONValue(dec)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return items, nil
		}
	case json.Number:
		return t, nil
	case string, bool, nil:
		return t, nil
	}
	return tok, nil
}

// ToOrderedEnv converts various input types (*OrderedEnv, OrderedEnv, map[string]any, []byte, string)
// to a canonical *OrderedEnv.
func ToOrderedEnv(v any) *OrderedEnv {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case *OrderedEnv:
		if val == nil || val.Len() == 0 {
			return nil
		}
		return val
	case OrderedEnv:
		if val.Len() == 0 {
			return nil
		}
		return &val
	case []byte:
		env, err := ParseOrderedEnvJSON(val)
		if err != nil || env == nil || env.Len() == 0 {
			return nil
		}
		return env
	case string:
		env, err := ParseOrderedEnvJSON([]byte(val))
		if err != nil || env == nil || env.Len() == 0 {
			return nil
		}
		return env
	case map[string]any:
		if len(val) == 0 {
			return nil
		}
		keys := sortedMapKeys(val)
		return &OrderedEnv{
			keys:   keys,
			values: val,
		}
	}
	return nil
}

// ApplyEnvExplicit applies global and project environment definitions using injected
// environment lookup, setenv, and unsetenv functions. This allows 100% deterministic,
// unit-testable execution without global side effects, preserving settings key order.
func ApplyEnvExplicit(
	globalEnvRaw, projectEnvRaw any,
	lookup func(string) (string, bool),
	setenv func(string, string) error,
	unsetenv func(string) error,
) ApplyResult {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if setenv == nil {
		setenv = os.Setenv
	}
	if unsetenv == nil {
		unsetenv = os.Unsetenv
	}

	globalEnv := ToOrderedEnv(globalEnvRaw)
	projectEnv := ToOrderedEnv(projectEnvRaw)

	// Merge: project overrides global, keeping declaration order
	var mergedKeys []string
	seenMerged := make(map[string]bool)

	if globalEnv != nil {
		for _, k := range globalEnv.Keys() {
			if !seenMerged[k] {
				seenMerged[k] = true
				mergedKeys = append(mergedKeys, k)
			}
		}
	}
	if projectEnv != nil {
		for _, k := range projectEnv.Keys() {
			if !seenMerged[k] {
				seenMerged[k] = true
				mergedKeys = append(mergedKeys, k)
			}
		}
	}

	getMergedValue := func(k string) (any, bool) {
		if projectEnv != nil {
			if v, ok := projectEnv.Get(k); ok {
				return v, true
			}
		}
		if globalEnv != nil {
			if v, ok := globalEnv.Get(k); ok {
				return v, true
			}
		}
		return nil, false
	}

	var nonScalarKeys []string
	var scalarKeys []string
	for _, k := range mergedKeys {
		v, _ := getMergedValue(k)
		if IsScalar(v) {
			scalarKeys = append(scalarKeys, k)
		} else {
			nonScalarKeys = append(nonScalarKeys, k)
		}
	}

	// Clean up stale vars from a previous load (e.g. after /reload with keys removed)
	var previousKeys []string
	if raw, ok := lookup(TrackerKey); ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &previousKeys)
	}
	var previousOverrides []string
	if raw, ok := lookup(OverridesKey); ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &previousOverrides)
	}

	scalarKeySet := make(map[string]bool, len(scalarKeys))
	for _, k := range scalarKeys {
		scalarKeySet[k] = true
	}
	for _, key := range previousKeys {
		if !scalarKeySet[key] {
			_ = unsetenv(key)
		}
	}

	var unresolvedVars []string
	var overriddenVars []string
	var reports []EnvReport

	// Process global
	if globalEnv != nil && globalEnv.Len() > 0 {
		vars := make(map[string]string)
		var reportKeys []string
		for _, key := range globalEnv.Keys() {
			value, _ := globalEnv.Get(key)
			if !IsScalar(value) {
				continue
			}
			strValue := ScalarToString(value)
			resolved, missing := ResolveValue(strValue, lookup)
			unresolvedVars = append(unresolvedVars, missing...)

			_, hasExisting := lookup(key)
			if (hasExisting && !contains(previousKeys, key)) || contains(previousOverrides, key) {
				overriddenVars = append(overriddenVars, key)
			}
			_ = setenv(key, resolved)
			vars[key] = resolved
			reportKeys = append(reportKeys, key)
		}
		if len(vars) > 0 {
			reports = append(reports, EnvReport{Source: "global", Keys: reportKeys, Variables: vars})
		}
	}

	// Process project (overrides global)
	if projectEnv != nil && projectEnv.Len() > 0 {
		vars := make(map[string]string)
		var reportKeys []string
		for _, key := range projectEnv.Keys() {
			value, _ := projectEnv.Get(key)
			if !IsScalar(value) {
				continue
			}
			strValue := ScalarToString(value)
			resolved, missing := ResolveValue(strValue, lookup)
			unresolvedVars = append(unresolvedVars, missing...)

			_, hasExisting := lookup(key)
			var inGlobal bool
			if globalEnv != nil {
				_, inGlobal = globalEnv.Get(key)
			}
			isOverridden := contains(overriddenVars, key)

			if (hasExisting && !contains(previousKeys, key) && !isOverridden && !inGlobal) ||
				(contains(previousOverrides, key) && !isOverridden) {
				overriddenVars = append(overriddenVars, key)
			}
			_ = setenv(key, resolved)
			vars[key] = resolved
			reportKeys = append(reportKeys, key)
		}
		if len(vars) > 0 {
			reports = append(reports, EnvReport{Source: "project", Keys: reportKeys, Variables: vars})
		}
	}

	// Track current keys and overrides for reload persistence
	if len(scalarKeys) > 0 {
		data, _ := json.Marshal(scalarKeys)
		_ = setenv(TrackerKey, string(data))
	} else {
		_ = unsetenv(TrackerKey)
	}

	if len(overriddenVars) > 0 {
		data, _ := json.Marshal(overriddenVars)
		_ = setenv(OverridesKey, string(data))
	} else {
		_ = unsetenv(OverridesKey)
	}

	return ApplyResult{
		Reports:        reports,
		UnresolvedVars: unresolvedVars,
		OverriddenVars: overriddenVars,
		NonScalarKeys:  nonScalarKeys,
	}
}

// ResolveAgentDir discovers the active global agent directory where settings.json resides.
func ResolveAgentDir() string {
	if v := os.Getenv("PI_CODING_AGENT_DIR"); v != "" {
		return expandTilde(v)
	}
	if v := os.Getenv("PIG_CODING_AGENT_DIR"); v != "" {
		return expandTilde(v)
	}
	if v := os.Getenv("PIG_HOME"); v != "" {
		return filepath.Join(expandTilde(v), "agent")
	}
	home, err := os.UserHomeDir()
	if err == nil {
		piAgent := filepath.Join(home, ".pi", "agent")
		if _, err := os.Stat(piAgent); err == nil {
			return piAgent
		}
		pigAgent := filepath.Join(home, ".pig", "agent")
		if _, err := os.Stat(pigAgent); err == nil {
			return pigAgent
		}
		return piAgent
	}
	return ""
}

func readSettingsFile(filePath string) map[string]any {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	var res map[string]any
	if err := json.Unmarshal(data, &res); err != nil {
		return nil
	}
	return res
}

func readSettingsEnvFile(filePath string) *OrderedEnv {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	env, err := ParseOrderedSettingsJSON(data)
	if err != nil {
		return nil
	}
	return env
}

// LoadSettings loads global and project settings from disk as standard maps.
// 注：仅供测试/保留兼容（运行时已切换为保序的 LoadOrderedSettings）。
func LoadSettings(cwd string) (globalSettings, projectSettings map[string]any) {
	agentDir := ResolveAgentDir()
	if agentDir != "" {
		globalSettings = readSettingsFile(filepath.Join(agentDir, "settings.json"))
	}
	if cwd != "" {
		projectSettings = readSettingsFile(filepath.Join(cwd, ".pi", "settings.json"))
		if projectSettings == nil {
			projectSettings = readSettingsFile(filepath.Join(cwd, ".pig", "settings.json"))
		}
	}
	return globalSettings, projectSettings
}

// LoadOrderedSettings loads global and project env settings preserving key order in strict mode.
// It inspects only the "env" key in settings.json files; missing or empty "env" keys produce nil.
func LoadOrderedSettings(cwd string) (globalEnv, projectEnv *OrderedEnv) {
	agentDir := ResolveAgentDir()
	if agentDir != "" {
		globalEnv = readSettingsEnvFile(filepath.Join(agentDir, "settings.json"))
	}
	if cwd != "" {
		projectEnv = readSettingsEnvFile(filepath.Join(cwd, ".pi", "settings.json"))
		if projectEnv == nil {
			projectEnv = readSettingsEnvFile(filepath.Join(cwd, ".pig", "settings.json"))
		}
	}
	return globalEnv, projectEnv
}

// ApplyEnv loads settings files from disk and sets process environment variables preserving key order.
func ApplyEnv(cwd string) ApplyResult {
	globalEnv, projectEnv := LoadOrderedSettings(cwd)
	return ApplyEnvExplicit(globalEnv, projectEnv, os.LookupEnv, os.Setenv, os.Unsetenv)
}

// sensitiveEnvKey reports whether key looks like a credential-bearing name
// (contains KEY/TOKEN/SECRET/PASSWORD/PASS/AUTH/CREDENTIAL/CREDS/JWT/SESSION/
// DSN/PRIVATE/BEARER/URL/URI/PASSCODE, case-insensitive). Such values are
// masked in the /env report to avoid rendering secrets into the session.
// URL/URI intentionally catch connection-string keys that may embed
// user:pass@ credentials (DATABASE_URL, REDIS_URL, MONGO_URI, ...).
// Substring matching may over-trigger on non-sensitive keys (e.g. AUTHOR,
// OAUTH_URL, BASE_URL, CALLBACK_URI, PUBLIC_URL); that trades a slightly
// noisy report for never leaking a credential, which is the safe direction.
func sensitiveEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "AUTH", "CREDENTIAL", "CREDS", "JWT", "SESSION", "DSN", "PRIVATE", "BEARER", "URL", "URI", "PASSCODE"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// maskSecretValue renders a secret for the /env report without exposing enough
// of it to be enumerable. Values of 16 runes or fewer are fully masked — short
// passwords/tokens get no reveal at all. Only longer values render as
// `first4…last4` (e.g. abcd…wxyz) for identification. Empty values render as
// `(empty)`.
func maskSecretValue(val string) string {
	runes := []rune(val)
	switch {
	case len(runes) == 0:
		return "(empty)"
	case len(runes) <= 16:
		return "…"
	default:
		return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
	}
}

// FormatReport styles the environment report for /env command display, preserving key order.
func FormatReport(theme ThemeRenderer, reports []EnvReport) string {
	if len(reports) == 0 {
		if theme != nil {
			return theme.Fg("dim", "[env] No environment variables configured.")
		}
		return "[env] No environment variables configured."
	}

	var sb strings.Builder
	if theme != nil {
		sb.WriteString(theme.Fg("accent", "[env]"))
	} else {
		sb.WriteString("[env]")
	}
	sb.WriteByte('\n')

	for _, report := range reports {
		if theme != nil {
			sb.WriteString(theme.Fg("muted", fmt.Sprintf("  %s", report.Source)))
		} else {
			sb.WriteString(fmt.Sprintf("  %s", report.Source))
		}
		sb.WriteByte('\n')

		keys := report.Keys
		if len(keys) == 0 {
			keys = sortedMapKeys(report.Variables)
		}
		for _, key := range keys {
			val := report.Variables[key]
			if sensitiveEnvKey(key) {
				val = maskSecretValue(val)
			}
			if theme != nil {
				sb.WriteString(fmt.Sprintf("    %s%s%s\n",
					theme.Fg("success", key),
					theme.Fg("dim", "="),
					theme.Fg("muted", val),
				))
			} else {
				sb.WriteString(fmt.Sprintf("    %s=%s\n", key, val))
			}
		}
	}
	return strings.TrimRight(sb.String(), " \t\r\n")
}

// BuildSessionStartWarnings generates formatted warning messages for session_start.
func BuildSessionStartWarnings(theme ThemeRenderer, nonScalarKeys, unresolvedVars, overriddenVars []string) string {
	var warnings []string
	if len(nonScalarKeys) > 0 {
		label := "ignoring non-scalar values:"
		if theme != nil {
			label = theme.Fg("warning", label)
		}
		warnings = append(warnings, fmt.Sprintf("%s %s", label, strings.Join(unique(nonScalarKeys), ", ")))
	}
	if len(unresolvedVars) > 0 {
		label := "unresolved variables:"
		if theme != nil {
			label = theme.Fg("warning", label)
		}
		warnings = append(warnings, fmt.Sprintf("%s %s", label, strings.Join(unique(unresolvedVars), ", ")))
	}
	if len(overriddenVars) > 0 {
		label := "overriding existing variables:"
		if theme != nil {
			label = theme.Fg("warning", label)
		}
		warnings = append(warnings, fmt.Sprintf("%s %s", label, strings.Join(unique(overriddenVars), ", ")))
	}

	if len(warnings) == 0 {
		return ""
	}

	header := "[env]"
	if theme != nil {
		header = theme.Fg("accent", header)
	}

	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteByte('\n')
	for _, w := range warnings {
		sb.WriteString("  ")
		sb.WriteString(w)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), " \t\r\n")
}

// RenderMessageText extracts plain text from a message content object,
// supporting string and array of typed text parts.
func RenderMessageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if txt, ok := m["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}

// MessageRendererHandler splits customType=pi-env message content into terminal lines.
func MessageRendererHandler(_ sdk.Context, message map[string]any, _ sdk.MessageRenderOptions, _ int) ([]string, error) {
	rawContent := message["content"]
	text := RenderMessageText(rawContent)
	return strings.Split(text, "\n"), nil
}

// FilterContextMessages excludes custom messages with customType "pi-env" from LLM context.
func FilterContextMessages(messages []any) []map[string]any {
	var kept []map[string]any
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		customType, _ := msg["customType"].(string)
		if role == "custom" && customType == MessageType {
			// Exclude from context
			continue
		}
		kept = append(kept, msg)
	}
	return kept
}

// Extension constructs and returns the pi-env extension instance for PiG.
func Extension() *sdk.Extension {
	e := sdk.New("pi-env")

	// Apply env vars immediately at load time (before providers initialize)
	cwd, _ := os.Getwd()
	initResult := ApplyEnv(cwd)

	// 1. Register custom message renderer
	e.MessageRenderer(MessageType, MessageRendererHandler)

	// 2. Filter custom messages from LLM context
	e.OnEvent(sdk.EventContext, func(_ sdk.Context, data map[string]any) (any, error) {
		rawMessages, ok := data["messages"].([]any)
		if !ok {
			return nil, nil
		}
		filtered := FilterContextMessages(rawMessages)
		return map[string]any{"messages": filtered}, nil
	})

	// 3. Register /env slash command
	e.Command("env", "Show configured environment variables", func(ctx sdk.Context, _ string) error {
		cmdCwd := ctx.Cwd()
		if cmdCwd == "" {
			cmdCwd, _ = os.Getwd()
		}
		res := ApplyEnv(cmdCwd)
		reportText := FormatReport(ctx.UITheme(), res.Reports)
		return ctx.SendMessage(MessageType, reportText, true, sdk.SendMessageOptions{})
	})

	// 4. Show warnings on session start (no values — may contain secrets)
	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		if !ctx.HasUI() {
			return nil, nil
		}
		warnText := BuildSessionStartWarnings(ctx.UITheme(), initResult.NonScalarKeys, initResult.UnresolvedVars, initResult.OverriddenVars)
		if warnText != "" {
			_ = ctx.SendMessage(MessageType, warnText, true, sdk.SendMessageOptions{})
			ctx.Notify("[env] Warnings encountered during initialization", "warning")
		}
		return nil, nil
	})

	return e
}

// Helpers

func unique(items []string) []string {
	seen := make(map[string]bool)
	var res []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			res = append(res, item)
		}
	}
	return res
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func expandTilde(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
