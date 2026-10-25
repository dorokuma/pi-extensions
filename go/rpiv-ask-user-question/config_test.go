package rpivaskuserquestion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testConfigEnv builds an injectable configEnv rooted at a temp home.
type testEnv struct {
	env  map[string]string
	home string
}

func newTestEnv(t *testing.T, files map[string]string) *testEnv {
	t.Helper()
	home := t.TempDir()
	for relative, content := range files {
		path := filepath.Join(home, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return &testEnv{env: map[string]string{}, home: home}
}

func (e *testEnv) configEnv() configEnv {
	return configEnv{
		getenv: func(key string) string {
			if value, ok := e.env[key]; ok {
				return value
			}
			// Pin HOME so filepath.Join home resolution is deterministic.
			if key == "HOME" {
				return e.home
			}
			return ""
		},
		homeDir:  func() string { return e.home },
		readFile: os.ReadFile,
		stat:     os.Stat,
		warn:     func(string) {},
	}
}

func (e *testEnv) set(key, value string) { e.env[key] = value }

// ── isValidCollapseKeySpec ──────────────────────────────────────────────────

func TestIsValidCollapseKeySpec(t *testing.T) {
	valid := []string{
		"ctrl+]", "alt+o", "ctrl+shift+h", "f9", "ctrl+pagedown", "a", "1",
		"escape", "enter", "tab", "space", "backspace", "delete", "insert",
		"clear", "home", "end", "pageup", "pagedown", "up", "down", "left", "right",
		"f1", "f12", "super+k", "ctrl+alt+shift+super+a", "-", "_", "!", "/", "?", "]", "\\", "ctrl+\\",
	}
	for _, spec := range valid {
		if !isValidCollapseKeySpec(spec) {
			t.Fatalf("spec %q rejected", spec)
		}
	}

	invalid := []string{
		"", "+", "+a", "a+", "a++b", "ctr+]", // typo: unknown modifier
		"ctrl+ctrl+a", // repeated modifier
		"shift+a+b",   // two base keys
		"f13", "F1",   // case-sensitive; uppercase is not canonical
		"ctrl+PageUp",
		"page-up",
		"ctrl+",
		"meta+a", // not a known modifier
		"ctrl+€", // multi-byte base that is not a named key
	}
	for _, spec := range invalid {
		if isValidCollapseKeySpec(spec) {
			t.Fatalf("spec %q accepted", spec)
		}
	}
}

// ── resolveCollapseKey ─────────────────────────────────────────────────────

func TestResolveCollapseKey(t *testing.T) {
	cases := []struct {
		name  string
		value *string
		want  string
	}{
		{"unset", nil, DefaultCollapseKey},
		{"empty after trim", strPtr("   "), DefaultCollapseKey},
		{"off", strPtr("off"), CollapseKeyOff},
		{"off with surrounding space", strPtr("  OFF  "), CollapseKeyOff},
		{"valid spec lowercased", strPtr("  ALT+O  "), "alt+o"},
		{"valid default", strPtr("ctrl+]"), "ctrl+]"},
		{"invalid spec falls back", strPtr("ctr+]"), DefaultCollapseKey},
		{"latin american shifted brace", strPtr("ctrl+}"), "ctrl+}"},
		// The TS character class ends in a literal backslash, so a layout where `]`
		// needs a modifier can bind ctrl+\ instead of silently falling back.
		{"backslash base", strPtr("ctrl+\\"), "ctrl+\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AskUserQuestionConfig{CollapseKey: tc.value}
			if got := ResolveCollapseKey(cfg); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := ResolveCollapseKey(nil); got != DefaultCollapseKey {
		t.Fatalf("nil config = %q", got)
	}
}

// ── resolveNotesKey ────────────────────────────────────────────────────────

func TestResolveNotesKey(t *testing.T) {
	cases := []struct {
		name  string
		value *string
		want  string
	}{
		{"unset", nil, DefaultNotesKey},
		{"empty", strPtr("  "), DefaultNotesKey},
		{"single ascii char", strPtr("m"), "m"},
		{"single char keeps case", strPtr("N"), "N"},
		{"single cyrillic char", strPtr("т"), "т"},
		{"combo spec", strPtr("Ctrl+N"), "ctrl+n"},
		{"combo spec pageup", strPtr("ctrl+pageup"), "ctrl+pageup"},
		// Reserved navigation keys would shadow routing.
		{"reserved space", strPtr("space"), DefaultNotesKey},
		{"reserved enter", strPtr("enter"), DefaultNotesKey},
		{"reserved return", strPtr("return"), DefaultNotesKey},
		{"reserved tab", strPtr("tab"), DefaultNotesKey},
		{"reserved escape", strPtr("escape"), DefaultNotesKey},
		{"reserved esc", strPtr("esc"), DefaultNotesKey},
		{"reserved up", strPtr("up"), DefaultNotesKey},
		{"reserved down", strPtr("down"), DefaultNotesKey},
		{"reserved left", strPtr("left"), DefaultNotesKey},
		{"reserved right", strPtr("right"), DefaultNotesKey},
		{"invalid spec", strPtr("nope+n"), DefaultNotesKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AskUserQuestionConfig{NotesKey: tc.value}
			if got := ResolveNotesKey(cfg); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := ResolveNotesKey(nil); got != DefaultNotesKey {
		t.Fatalf("nil config = %q", got)
	}
}

// A reserved key is only reserved as a bare spec, not as a modifier combo.
func TestResolveNotesKeyReservedOnlyBare(t *testing.T) {
	cfg := &AskUserQuestionConfig{NotesKey: strPtr("ctrl+tab")}
	if got := ResolveNotesKey(cfg); got != "ctrl+tab" {
		t.Fatalf("ctrl+tab rejected: %q", got)
	}
}

// ── formatKeySpecForDisplay / formatNotesKeyForDisplay ─────────────────────

func TestFormatKeySpecForDisplay(t *testing.T) {
	cases := map[string]string{
		"ctrl+]":           "Ctrl+]",
		"alt+o":            "Alt+O",
		"f9":               "F9",
		"ctrl+pagedown":    "Ctrl+PageDown",
		"ctrl+pageup":      "Ctrl+PageUp",
		"ctrl+shift+h":     "Ctrl+Shift+H",
		"a":                "A",
		"ctrl+alt+super+x": "Ctrl+Alt+Super+X",
	}
	for spec, want := range cases {
		if got := FormatKeySpecForDisplay(spec); got != want {
			t.Fatalf("FormatKeySpecForDisplay(%q) = %q, want %q", spec, got, want)
		}
	}
}

func TestFormatNotesKeyForDisplay(t *testing.T) {
	if got := FormatNotesKeyForDisplay("n"); got != "n" {
		t.Fatalf("single char = %q (must show as configured)", got)
	}
	if got := FormatNotesKeyForDisplay("т"); got != "т" {
		t.Fatalf("single cyrillic = %q", got)
	}
	if got := FormatNotesKeyForDisplay("ctrl+n"); got != "Ctrl+N" {
		t.Fatalf("spec = %q", got)
	}
}

// ── validateGuidanceFields ─────────────────────────────────────────────────

func TestValidateGuidanceFields(t *testing.T) {
	t.Run("all valid", func(t *testing.T) {
		got := validateGuidanceFields(map[string]any{
			"promptSnippet":    "snippet",
			"promptGuidelines": []any{"a", "b"},
			"description":      "desc",
		})
		if got == nil {
			t.Fatal("nil result")
		}
		if got.PromptSnippet != "snippet" || got.Description != "desc" {
			t.Fatalf("result = %#v", got)
		}
		if len(got.PromptGuidelines) != 2 || got.PromptGuidelines[1] != "b" {
			t.Fatalf("guidelines = %v", got.PromptGuidelines)
		}
	})

	t.Run("non-object input", func(t *testing.T) {
		for _, value := range []any{nil, "text", 42, []any{"a"}} {
			if got := validateGuidanceFields(value); got != nil {
				t.Fatalf("input %#v gave %#v", value, got)
			}
		}
	})

	t.Run("empty fields are dropped", func(t *testing.T) {
		got := validateGuidanceFields(map[string]any{
			"promptSnippet":    "",
			"promptGuidelines": []any{},
			"description":      "",
		})
		if got != nil {
			t.Fatalf("result = %#v", got)
		}
	})

	t.Run("guidelines with an empty string are dropped", func(t *testing.T) {
		got := validateGuidanceFields(map[string]any{"promptGuidelines": []any{"a", ""}})
		if got != nil {
			t.Fatalf("result = %#v", got)
		}
	})

	t.Run("guidelines with a non-string are dropped", func(t *testing.T) {
		got := validateGuidanceFields(map[string]any{"promptGuidelines": []any{"a", 2}})
		if got != nil {
			t.Fatalf("result = %#v", got)
		}
	})

	t.Run("wrong types are ignored", func(t *testing.T) {
		got := validateGuidanceFields(map[string]any{"promptSnippet": 5, "description": true})
		if got != nil {
			t.Fatalf("result = %#v", got)
		}
	})
}

// ── config path resolution ─────────────────────────────────────────────────

func TestConfigPathHonoursXDG(t *testing.T) {
	env := newTestEnv(t, nil)
	e := env.configEnv()

	// Unset → ~/.config.
	if got := e.configPath(ConfigPackageName); got != filepath.Join(env.home, ".config", ConfigPackageName, "config.json") {
		t.Fatalf("default path = %q", got)
	}
	// Relative and "~user" forms route to the default.
	for _, xdg := range []string{"relative/path", "~someone/config", "   "} {
		env.set("XDG_CONFIG_HOME", xdg)
		if got := e.configPath(ConfigPackageName); got != filepath.Join(env.home, ".config", ConfigPackageName, "config.json") {
			t.Fatalf("xdg %q gave %q", xdg, got)
		}
	}
	// Absolute is used verbatim.
	env.set("XDG_CONFIG_HOME", "/custom/xdg")
	if got := e.configPath(ConfigPackageName); got != "/custom/xdg/"+ConfigPackageName+"/config.json" {
		t.Fatalf("absolute xdg gave %q", got)
	}
	// Bare "~" expands to home.
	env.set("XDG_CONFIG_HOME", "~")
	if got := e.configPath(ConfigPackageName); got != filepath.Join(env.home, ConfigPackageName, "config.json") {
		t.Fatalf("tilde xdg gave %q", got)
	}
	// "~/x" expands.
	env.set("XDG_CONFIG_HOME", "~/xdg")
	if got := e.configPath(ConfigPackageName); got != filepath.Join(env.home, "xdg", ConfigPackageName, "config.json") {
		t.Fatalf("tilde path gave %q", got)
	}
}

func TestLegacyConfigPathIgnoresXDG(t *testing.T) {
	env := newTestEnv(t, nil)
	env.set("XDG_CONFIG_HOME", "/custom/xdg")
	e := env.configEnv()
	if got := e.legacyConfigPath(ConfigPackageName); got != filepath.Join(env.home, ".config", ConfigPackageName, "config.json") {
		t.Fatalf("legacy path = %q", got)
	}
}

func TestLoadConfigReadsXDGFile(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json": `{"collapseKey":"alt+o","notesKey":"m"}`,
	})
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.CollapseKey == nil || *cfg.CollapseKey != "alt+o" {
		t.Fatalf("collapseKey = %v", cfg.CollapseKey)
	}
	if cfg.NotesKey == nil || *cfg.NotesKey != "m" {
		t.Fatalf("notesKey = %v", cfg.NotesKey)
	}
}

// A malformed XDG file warns and yields {} — it must NOT silently fall back to
// the legacy file (corruption is surfaced, not masked).
func TestLoadConfigMalformedXDGDoesNotFallBack(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json":        `{not json`,
		".config/" + ConfigPackageName + "/config.legacy.json": `{"notesKey":"legacy"}`,
	})
	warnings := 0
	e := env.configEnv()
	e.warn = func(string) { warnings++ }
	cfg := e.loadConfigWithConfigEnv()
	if warnings != 1 {
		t.Fatalf("warnings = %d", warnings)
	}
	if cfg.NotesKey != nil {
		t.Fatalf("fallback leaked: %v", cfg.NotesKey)
	}
}

// With XDG set and no XDG file, the always-legacy ~/.config path is read.
func TestLoadConfigFallsBackToLegacyPathWhenXDGFileMissing(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json": `{"notesKey":"legacy"}`,
	})
	env.set("XDG_CONFIG_HOME", "/nonexistent/xdg")
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.NotesKey == nil || *cfg.NotesKey != "legacy" {
		t.Fatalf("notesKey = %v", cfg.NotesKey)
	}
}

func TestLoadConfigReadsXDGPathWhenPresent(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json": `{"notesKey":"legacy"}`,
		"xdg/" + ConfigPackageName + "/config.json":     `{"notesKey":"xdg"}`,
	})
	env.set("XDG_CONFIG_HOME", filepath.Join(env.home, "xdg"))
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.NotesKey == nil || *cfg.NotesKey != "xdg" {
		t.Fatalf("notesKey = %v", cfg.NotesKey)
	}
}

// ── isolated (PiG) paths ──────────────────────────────────────────────────

func TestLoadConfigPrefersPigHomeIsolatedPath(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		"pig/.config/" + ConfigPackageName + "/config.json": `{"notesKey":"pig-home"}`,
		".config/" + ConfigPackageName + "/config.json":     `{"notesKey":"native"}`,
	})
	env.set("PIG_HOME", filepath.Join(env.home, "pig"))
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.NotesKey == nil || *cfg.NotesKey != "pig-home" {
		t.Fatalf("notesKey = %v (isolated config not preferred)", cfg.NotesKey)
	}
}

func TestLoadConfigPrefersPigCodingAgentDir(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		"agent/extensions/" + ConfigPackageName + "/config.json": `{"notesKey":"agent-dir"}`,
		".config/" + ConfigPackageName + "/config.json":          `{"notesKey":"native"}`,
	})
	env.set("PIG_CODING_AGENT_DIR", filepath.Join(env.home, "agent"))
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.NotesKey == nil || *cfg.NotesKey != "agent-dir" {
		t.Fatalf("notesKey = %v", cfg.NotesKey)
	}
}

// With the isolated variables set but no isolated file, the native lookup order
// applies unchanged.
func TestLoadConfigFallsThroughIsolationWhenFileMissing(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json": `{"notesKey":"native"}`,
	})
	env.set("PIG_HOME", filepath.Join(env.home, "empty-pig"))
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.NotesKey == nil || *cfg.NotesKey != "native" {
		t.Fatalf("notesKey = %v", cfg.NotesKey)
	}
}

func TestIsolatedConfigPaths(t *testing.T) {
	env := newTestEnv(t, nil)
	env.set("PIG_HOME", "/pig")
	env.set("PIG_CODING_AGENT_DIR", "/agent")
	paths := env.configEnv().isolatedConfigPaths(ConfigPackageName)
	want := []string{
		"/agent/extensions/" + ConfigPackageName + "/config.json",
		"/pig/.config/" + ConfigPackageName + "/config.json",
		"/pig/config/" + ConfigPackageName + "/config.json",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}

	// With neither variable set there are no isolated candidates.
	if got := newTestEnv(t, nil).configEnv().isolatedConfigPaths(ConfigPackageName); len(got) != 0 {
		t.Fatalf("paths = %v", got)
	}
}

// ── JSON shape tolerance ──────────────────────────────────────────────────

func TestLoadConfigToleratesNonObjectJSON(t *testing.T) {
	for _, content := range []string{`"hello"`, `42`, `null`, `[1,2]`, `true`} {
		env := newTestEnv(t, map[string]string{
			".config/" + ConfigPackageName + "/config.json": content,
		})
		cfg := env.configEnv().loadConfigWithConfigEnv()
		if cfg.CollapseKey != nil || cfg.NotesKey != nil || cfg.Guidance != nil {
			t.Fatalf("content %q produced %#v", content, cfg)
		}
	}
}

func TestLoadConfigReadsGuidance(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json": `{
			"guidance": {
				"promptSnippet": "s",
				"promptGuidelines": ["g1","g2"],
				"description": "d"
			},
			"collapseKey": "off"
		}`,
	})
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.Guidance == nil || cfg.Guidance.Description != "d" || len(cfg.Guidance.PromptGuidelines) != 2 {
		t.Fatalf("guidance = %#v", cfg.Guidance)
	}
	if ResolveCollapseKey(cfg) != CollapseKeyOff {
		t.Fatalf("collapseKey = %v", cfg.CollapseKey)
	}
}

// Unknown keys are ignored, and wrong-typed known keys are dropped.
func TestLoadConfigIgnoresUnknownAndMistypedKeys(t *testing.T) {
	env := newTestEnv(t, map[string]string{
		".config/" + ConfigPackageName + "/config.json": `{"extra":1,"collapseKey":5,"notesKey":true}`,
	})
	cfg := env.configEnv().loadConfigWithConfigEnv()
	if cfg.CollapseKey != nil || cfg.NotesKey != nil {
		t.Fatalf("mistyped keys read: %#v", cfg)
	}
}

func TestReadEnvVarTrims(t *testing.T) {
	env := newTestEnv(t, nil)
	env.set("SOME_KEY", "  value  ")
	e := env.configEnv()
	if got, ok := e.readEnvVar("SOME_KEY"); !ok || got != "value" {
		t.Fatalf("readEnvVar = (%q,%v)", got, ok)
	}
	env.set("SOME_KEY", "   ")
	if _, ok := e.readEnvVar("SOME_KEY"); ok {
		t.Fatal("whitespace-only value treated as set")
	}
	if _, ok := e.readEnvVar("ABSENT_KEY_XYZ"); ok {
		t.Fatal("absent key treated as set")
	}
}

func TestExpandTilde(t *testing.T) {
	env := newTestEnv(t, nil)
	e := env.configEnv()
	if got := e.expandTilde("~"); got != env.home {
		t.Fatalf("bare tilde = %q", got)
	}
	if got := e.expandTilde("~/x"); got != filepath.Join(env.home, "x") {
		t.Fatalf("~/x = %q", got)
	}
	if got := e.expandTilde("~user/x"); got != "~user/x" {
		t.Fatalf("~user = %q (must not expand)", got)
	}
	if got := e.expandTilde("/abs"); got != "/abs" {
		t.Fatalf("abs = %q", got)
	}
}

func TestConfigDefaultsArePinned(t *testing.T) {
	if DefaultCollapseKey != "ctrl+]" {
		t.Fatalf("default collapse key = %q", DefaultCollapseKey)
	}
	if DefaultNotesKey != "n" {
		t.Fatalf("default notes key = %q", DefaultNotesKey)
	}
	if CollapseKeyOff != "off" {
		t.Fatalf("off sentinel = %q", CollapseKeyOff)
	}
	if !strings.Contains(ErrorMsgReservedLabel, "Next") {
		t.Fatalf("reserved message = %q", ErrorMsgReservedLabel)
	}
}
