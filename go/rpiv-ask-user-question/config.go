package rpivaskuserquestion

// Config layer: ports config.ts (v2.15.0) plus the parts of
// @juicesharp/rpiv-config it consumes (loadJsonConfigWithLegacyFallback,
// validateGuidanceFields, readEnvVar, configPath resolution).
//
// Behaviour notes vs the TS original:
//   - Everything that touches the filesystem or the process environment takes
//     an explicit configEnv so tests can inject a home directory and env map.
//     The package-level helpers ([loadConfig]) bind the real process.
//   - Isolated (PiG) lookups are tried FIRST and only when PIG_HOME or
//     PIG_CODING_AGENT_DIR is set; with neither set the lookup order is exactly
//     the TS order (XDG path, then the always-legacy ~/.config path).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ConfigPackageName is the config directory every lookup is keyed by.
const ConfigPackageName = "rpiv-ask-user-question"

// DefaultCollapseKey is the overlay collapse/expand shortcut when unset.
const DefaultCollapseKey = "ctrl+]"

// CollapseKeyOff disables the collapse shortcut entirely.
const CollapseKeyOff = "off"

// DefaultNotesKey is the notes-editor key when unset.
const DefaultNotesKey = "n"

// GuidanceFields mirrors the shared rpiv-config interface 1:1.
type GuidanceFields struct {
	PromptSnippet    string
	PromptGuidelines []string
	Description      string
}

// AskUserQuestionConfig mirrors config.ts's AskUserQuestionConfig. Pointer
// fields keep "absent" distinguishable from "present but empty", which the
// resolvers depend on (an empty collapseKey falls back to the default).
type AskUserQuestionConfig struct {
	Guidance    *GuidanceFields
	CollapseKey *string
	NotesKey    *string
}

// configEnv is the injectable view of the process the config layer reads.
type configEnv struct {
	getenv   func(string) string
	homeDir  func() string
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	warn     func(string)
}

// osConfigEnv binds configEnv to the real process.
func osConfigEnv() configEnv {
	return configEnv{
		getenv:   os.Getenv,
		homeDir:  osHomeDir,
		readFile: os.ReadFile,
		stat:     os.Stat,
		warn:     func(string) {}, // the TS original writes to stderr; PiG has no stderr channel.
	}
}

// osHomeDir mirrors node's os.homedir().
func osHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// readEnvVar mirrors rpiv-config's readEnvVar: trim, and treat empty as unset.
func (e configEnv) readEnvVar(key string) (string, bool) {
	value := strings.TrimSpace(e.getenv(key))
	return value, value != ""
}

// expandTilde mirrors rpiv-config's expandTilde: only bare "~" and "~/…"
// expand; "~user" forms are returned unchanged.
func (e configEnv) expandTilde(p string) string {
	if p == "~" {
		return e.homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(e.homeDir(), p[2:])
	}
	return p
}

// defaultConfigDir is ~/.config, the single source of the ".config" literal.
func (e configEnv) defaultConfigDir() string {
	return filepath.Join(e.homeDir(), ".config")
}

// resolveConfigDir honors XDG_CONFIG_HOME with the XDG rules rpiv-config
// documents: unset/blank/relative → default; "~"/"~/…" expand then must be
// absolute; "~user…" is not expanded and therefore routes to the default.
func (e configEnv) resolveConfigDir() string {
	xdg, ok := e.readEnvVar("XDG_CONFIG_HOME")
	if !ok {
		return e.defaultConfigDir()
	}
	expanded := e.expandTilde(xdg)
	if filepath.IsAbs(expanded) {
		return expanded
	}
	return e.defaultConfigDir()
}

// configPath is <resolveConfigDir()>/<name>/config.json.
func (e configEnv) configPath(name string) string {
	return filepath.Join(e.resolveConfigDir(), name, "config.json")
}

// legacyConfigPath is the always-legacy ~/.config/<name>/config.json path.
//
// This is rpiv-config's own `legacyConfigPath` verbatim: `join(defaultConfigDir(),
// name, file)` with `defaultConfigDir() === join(homedir(), ".config")` — the
// pre-XDG location an operator keeps after pointing XDG_CONFIG_HOME elsewhere. It
// is NOT a `~/.pi/agent/extensions/<name>/config.json` path: rpiv-config keeps the
// Pi-native layer orthogonal on purpose (see its `resolveConfigDir` boundary note).
func (e configEnv) legacyConfigPath(name string) string {
	return filepath.Join(e.defaultConfigDir(), name, "config.json")
}

// isolatedConfigPaths lists the PiG-isolated config directories, most specific
// first. Each entry is the directory that would hold <name>/config.json. These are
// a PiG addition on top of the TS lookup (which is XDG path, then the always-legacy
// ~/.config path): they let a PiG install keep its own config next to the PiG home
// instead of sharing the native one, and they are only candidates at all when the
// matching environment variable is set — with neither set the order is exactly the
// TS order.
//
//   - PIG_CODING_AGENT_DIR is Pi's agent dir (a ~/.pi/agent equivalent), so its
//     configs live under <dir>/extensions/<name>/config.json — the same shape as
//     the legacy Pi layout.
//   - PIG_HOME is the PiG home (a ~/.pig equivalent), so its configs live under
//     <home>/.config/<name>/config.json unless XDG-style nesting is implied
//     (<home>/config/<name>/config.json for the agent scope).
func (e configEnv) isolatedConfigPaths(name string) []string {
	var paths []string
	if dir, ok := e.readEnvVar("PIG_CODING_AGENT_DIR"); ok {
		paths = append(paths, filepath.Join(dir, "extensions", name, "config.json"))
	}
	if home, ok := e.readEnvVar("PIG_HOME"); ok {
		paths = append(paths,
			filepath.Join(home, ".config", name, "config.json"),
			filepath.Join(home, "config", name, "config.json"),
		)
	}
	return paths
}

// fileExists reports whether path names an existing file (any type, mirroring
// node's existsSync).
func (e configEnv) fileExists(path string) bool {
	_, err := e.stat(path)
	return err == nil
}

// loadJSONConfig mirrors loadJsonConfig: {} for a missing file, malformed JSON,
// or any non-object value (arrays included).
func (e configEnv) loadJSONConfig(path string) *AskUserQuestionConfig {
	if !e.fileExists(path) {
		return &AskUserQuestionConfig{}
	}
	raw, err := e.readFile(path)
	if err != nil {
		e.warn("rpiv-config: cannot read " + path + ", using default ({}) — " + err.Error())
		return &AskUserQuestionConfig{}
	}
	parsed, ok := parseJSONObject(raw)
	if !ok {
		e.warn("rpiv-config: invalid JSON at " + path + ", using default ({})")
		return &AskUserQuestionConfig{}
	}
	return configFromMap(parsed)
}

// parseJSONObject decodes raw JSON and requires a plain object at the top level.
func parseJSONObject(raw []byte) (map[string]any, bool) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	object, ok := decoded.(map[string]any)
	if !ok || object == nil {
		return nil, false
	}
	return object, true
}

// configFromMap reads the AskUserQuestionConfig fields out of a decoded object.
// Unknown keys are ignored, as the TS cast does.
func configFromMap(object map[string]any) *AskUserQuestionConfig {
	cfg := &AskUserQuestionConfig{}
	if guidance, present := object["guidance"]; present {
		fields := validateGuidanceFields(guidance)
		if fields != nil && !fields.empty() {
			cfg.Guidance = fields
		}
	}
	if raw, present := object["collapseKey"]; present {
		if s, ok := raw.(string); ok {
			cfg.CollapseKey = &s
		}
	}
	if raw, present := object["notesKey"]; present {
		if s, ok := raw.(string); ok {
			cfg.NotesKey = &s
		}
	}
	return cfg
}

func (g *GuidanceFields) empty() bool {
	return g.PromptSnippet == "" && len(g.PromptGuidelines) == 0 && g.Description == ""
}

// validateGuidanceFields mirrors rpiv-config's validateGuidanceFields: keep only
// non-empty string fields, and only a non-empty array of non-empty strings.
// Returns nil for a value with nothing to keep.
func validateGuidanceFields(fields any) *GuidanceFields {
	object, ok := fields.(map[string]any)
	if !ok || object == nil {
		return nil
	}
	result := &GuidanceFields{}
	if s, ok := object["promptSnippet"].(string); ok && len(s) > 0 {
		result.PromptSnippet = s
	}
	if raw, ok := object["promptGuidelines"].([]any); ok && len(raw) > 0 {
		guidelines := make([]string, 0, len(raw))
		allStrings := true
		for _, item := range raw {
			s, ok := item.(string)
			if !ok || len(s) == 0 {
				allStrings = false
				break
			}
			guidelines = append(guidelines, s)
		}
		if allStrings {
			result.PromptGuidelines = guidelines
		}
	}
	if s, ok := object["description"].(string); ok && len(s) > 0 {
		result.Description = s
	}
	if result.empty() {
		return nil
	}
	return result
}

// loadConfigWithConfigEnv mirrors loadJsonConfigWithLegacyFallback, extended
// with the isolated PiG lookup paths. The first existing file wins; a
// malformed file yields {} and never falls through to the next candidate — the TS
// rule that a corrupt XDG file is surfaced, not masked, holds for every candidate.
//
// Call timing mirrors the TS original: guidance is read once when the tool is
// registered (registerAskUserQuestionTool), while collapseKey/notesKey are read per
// execution through LoadConfig on the TUI path, so editing the file is picked up by
// the next questionnaire without a reload.
func (e configEnv) loadConfigWithConfigEnv() *AskUserQuestionConfig {
	candidates := append(e.isolatedConfigPaths(ConfigPackageName), e.configPath(ConfigPackageName))
	if e.fileExists(candidates[0]) {
		return e.loadJSONConfig(candidates[0])
	}
	// TS semantics: the XDG-aware path is tried first, then the legacy path.
	for _, path := range candidates {
		if e.fileExists(path) {
			return e.loadJSONConfig(path)
		}
	}
	return e.loadJSONConfig(e.legacyConfigPath(ConfigPackageName))
}

// LoadConfig reads the extension config from the real process.
func LoadConfig() *AskUserQuestionConfig {
	return osConfigEnv().loadConfigWithConfigEnv()
}

// specialKeys are the named keys pi-tui's matchesKey accepts. parseKeyId
// lowercases the id before matching, so lowercase spellings are canonical.
var specialKeys = func() map[string]struct{} {
	names := []string{
		"escape", "esc", "enter", "return", "tab", "space", "backspace",
		"delete", "insert", "clear", "home", "end", "pageup", "pagedown",
		"up", "down", "left", "right",
	}
	for i := 1; i <= 12; i++ {
		names = append(names, "f"+itoa(i))
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}()

// modifiers are the accepted modifier prefixes.
var modifiers = map[string]struct{}{
	"ctrl": {}, "shift": {}, "alt": {}, "super": {},
}

// allowedBaseKeys is the single-character base-key class from config.ts:
// /[a-z0-9_\-!@#$%^&*()|~`'":;,./<>?[\]{}=\\]/ after lowercasing — the trailing
// `\\` of that class is a literal backslash, so `ctrl+\` is a valid spec.
const allowedBaseKeys = "abcdefghijklmnopqrstuvwxyz0123456789_-!@#$%^&*()|~`'\":;,./<>?[]{}=\\"

// isValidCollapseKeySpec mirrors config.ts's strict KeyId grammar check: zero
// or more distinct modifiers, then a base key that is a single printable
// character or a named special key.
func isValidCollapseKeySpec(spec string) bool {
	if spec == "" {
		return false
	}
	if strings.HasPrefix(spec, "+") || strings.HasSuffix(spec, "+") || strings.Contains(spec, "++") {
		return false
	}
	parts := strings.Split(spec, "+")
	base := parts[len(parts)-1]
	modifierParts := parts[:len(parts)-1]
	seen := make(map[string]struct{}, len(modifierParts))
	for _, m := range modifierParts {
		if _, dup := seen[m]; dup {
			return false
		}
		seen[m] = struct{}{}
		if _, ok := modifiers[m]; !ok {
			return false
		}
	}
	if utf8Len(base) == 1 {
		return strings.ContainsRune(allowedBaseKeys, []rune(base)[0])
	}
	_, special := specialKeys[base]
	return special
}

// ResolveCollapseKey mirrors resolveCollapseKey.
func ResolveCollapseKey(cfg *AskUserQuestionConfig) string {
	if cfg == nil || cfg.CollapseKey == nil {
		return DefaultCollapseKey
	}
	raw := strings.ToLower(strings.TrimSpace(*cfg.CollapseKey))
	if raw == "" {
		return DefaultCollapseKey
	}
	if raw == CollapseKeyOff {
		return CollapseKeyOff
	}
	if isValidCollapseKeySpec(raw) {
		return raw
	}
	return DefaultCollapseKey
}

// notesKeyReserved are the keys the questionnaire already navigates with; a
// notes key on one of them would shadow it.
var notesKeyReserved = map[string]struct{}{
	"space": {}, "enter": {}, "return": {}, "tab": {}, "escape": {}, "esc": {},
	"up": {}, "down": {}, "left": {}, "right": {},
}

// ResolveNotesKey mirrors resolveNotesKey: a single character (any script) is
// used as typed; anything else must be a valid, non-reserved key spec.
func ResolveNotesKey(cfg *AskUserQuestionConfig) string {
	if cfg == nil || cfg.NotesKey == nil {
		return DefaultNotesKey
	}
	raw := strings.TrimSpace(*cfg.NotesKey)
	if raw == "" {
		return DefaultNotesKey
	}
	if utf8Len(raw) == 1 {
		return raw
	}
	spec := strings.ToLower(raw)
	if _, reserved := notesKeyReserved[spec]; reserved {
		return DefaultNotesKey
	}
	if isValidCollapseKeySpec(spec) {
		return spec
	}
	return DefaultNotesKey
}

// compoundKeyDisplay holds the only compound-word names in specialKeys —
// first-letter capitalization alone would render them "Pageup"/"Pagedown".
var compoundKeyDisplay = map[string]string{
	"pageup":   "PageUp",
	"pagedown": "PageDown",
}

// FormatKeySpecForDisplay mirrors formatKeySpecForDisplay: each "+"-part gets
// its first character uppercased. Display-only — never feed the result back
// into key matching.
func FormatKeySpecForDisplay(spec string) string {
	parts := strings.Split(spec, "+")
	for i, part := range parts {
		if display, ok := compoundKeyDisplay[part]; ok {
			parts[i] = display
			continue
		}
		runes := []rune(part)
		if len(runes) <= 1 {
			parts[i] = strings.ToUpper(part)
			continue
		}
		parts[i] = strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	return strings.Join(parts, "+")
}

// FormatNotesKeyForDisplay mirrors formatNotesKeyForDisplay: a single-character
// key shows as configured; a spec is display-cased.
func FormatNotesKeyForDisplay(notesKey string) string {
	if utf8Len(notesKey) == 1 {
		return notesKey
	}
	return FormatKeySpecForDisplay(notesKey)
}
