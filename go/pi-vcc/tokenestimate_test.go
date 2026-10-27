package pivcc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Tests for src/core/token-estimate.ts and src/core/compact-args.ts.

// ── token-estimate ──────────────────────────────────────────────────────────

func TestCalibrateCharsPerToken(t *testing.T) {
	tests := []struct {
		name      string
		chars     float64
		tokens    float64
		hasTokens bool
		wantMode  TokenEstimateMode
		wantCPT   float64
	}{
		{"no usage data falls back to heuristic", 4000, 0, false, ModeHeuristic, DefaultCharsPerToken},
		{"zero tokens falls back", 4000, 0, true, ModeHeuristic, DefaultCharsPerToken},
		{"zero chars falls back", 0, 1000, true, ModeHeuristic, DefaultCharsPerToken},
		{"in-range ratio is used", 4000, 1000, true, ModeCalibrated, 4},
		{"ratio below the floor is clamped up", 1000, 1000, true, ModeCalibrated, MinCharsPerToken},
		{"ratio above the ceiling is clamped down", 8000, 1000, true, ModeCalibrated, MaxCharsPerToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CalibrateCharsPerToken(test.chars, test.tokens, test.hasTokens)
			if got.Mode != test.wantMode || got.CharsPerToken != test.wantCPT {
				t.Fatalf("CalibrateCharsPerToken = (%v, %v), want (%v, %v)",
					got.Mode, got.CharsPerToken, test.wantMode, test.wantCPT)
			}
		})
	}
	calibrated := CalibrateCharsPerToken(5000, 1000, true)
	if !calibrated.HasRawCharsPerToken || calibrated.RawCharsPerToken != 5 {
		t.Fatalf("calibration lost the raw ratio: %#v", calibrated)
	}
}

func TestEstimateTokensFromChars(t *testing.T) {
	if got := EstimateTokensFromChars(101, 4); got != 26 {
		t.Fatalf("EstimateTokensFromChars(101, 4) = %d, want ceil(25.25)=26", got)
	}
	if got := EstimateTokensFromChars(100, 4); got != 25 {
		t.Fatalf("EstimateTokensFromChars(100, 4) = %d", got)
	}
	// A zero or negative ratio falls back to the default of 4.
	if got := EstimateTokensFromChars(40, 0); got != 10 {
		t.Fatalf("EstimateTokensFromChars(40, 0) = %d, want 10", got)
	}
}

func TestEstimateMessageContentChars(t *testing.T) {
	tests := []struct {
		name    string
		content any
		want    int
	}{
		{"string content", "hello", 5},
		{"empty", nil, 0},
		{"text parts", []any{
			map[string]any{"type": "text", "text": "ab"},
			map[string]any{"type": "text", "text": "cde"},
		}, 5},
		{"thinking counts", []any{map[string]any{"type": "thinking", "thinking": "abc"}}, 3},
		{"image is a fixed 4800 chars", []any{map[string]any{"type": "image"}}, ImageContentChars},
		{"tool call counts name and arguments", []any{map[string]any{
			"type": "toolCall", "name": "bash",
			"arguments": map[string]any{"command": "ls"},
		}}, len("bash") + len(`{"command":"ls"}`)},
		{"unknown part falls back to text", []any{
			map[string]any{"type": "mystery", "text": "keep me"},
		}, 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EstimateMessageContentChars(test.content); got != test.want {
				t.Fatalf("EstimateMessageContentChars = %d, want %d", got, test.want)
			}
		})
	}
	// Astral characters count as two UTF-16 units, as JS length does.
	if got := EstimateMessageContentChars("\U0001F600"); got != 2 {
		t.Fatalf("astral char counted as %d, want 2", got)
	}
}

// ── compact-args ────────────────────────────────────────────────────────────

func TestParseKeepAndPrompt(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		wantKeep   int
		hasKeep    bool
		wantPrompt string
	}{
		{"empty", "", 0, false, ""},
		{"keep prefix with prompt", "keep:3 fix the tests", 3, true, "fix the tests"},
		{"keep prefix alone", "keep:2", 2, true, ""},
		{"keep token at the end", "summarize the auth work keep:4", 4, true, "summarize the auth work"},
		{"no keep token", "just compact", 0, false, "just compact"},
		{"keep-like word in the middle is not a token", "keep:3 is listed keep:5", 3, true, "is listed keep:5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseKeepAndPrompt(test.args)
			if got.KeepUserTurns != test.wantKeep || got.HasKeepUserTurns != test.hasKeep ||
				got.FollowUpPrompt != test.wantPrompt {
				t.Fatalf("ParseKeepAndPrompt(%q) = %+v", test.args, got)
			}
		})
	}
	// An explicit keep is flagged as such; the smart-tail boost honours it.
	if got := ParseKeepAndPrompt("keep:1"); !got.KeepUserTurnsExplicit {
		t.Fatal("keep:1 was not marked explicit")
	}
	if got := ParseKeepAndPrompt("no keep here"); got.KeepUserTurnsExplicit {
		t.Fatal("a keep-less argument was marked explicit")
	}
}

func TestParseKeepAndPrompt_HugeKeepClampsToSafeInteger(t *testing.T) {
	got := ParseKeepAndPrompt("keep:99999999999999999999")
	if !got.HasKeepUserTurns || got.KeepUserTurns != 9007199254740991 {
		t.Fatalf("ParseKeepAndPrompt(huge) = %+v", got)
	}
}

func TestBuildPiVccCustomInstructions(t *testing.T) {
	if got := BuildPiVccCustomInstructions(0, false); got != PiVccCompactInstruction {
		t.Fatalf("BuildPiVccCustomInstructions = %q", got)
	}
	if got := BuildPiVccCustomInstructions(3, true); got != PiVccCompactInstruction+" keep:3" {
		t.Fatalf("BuildPiVccCustomInstructions(3) = %q", got)
	}
}

// ── settings ────────────────────────────────────────────────────────────────

func TestLoadSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pi-vcc-config.json")
	raw := `{
  "overrideDefaultCompaction": false,
  "trackCommands": [" go ", "go", "", "docker"],
  "skipForProviders": ["openai", 42, "anthropic"],
  "skipCustomTypes": "ctx_kb://index/1",
  "debug": 1
}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadSettings(path)
	if got.OverrideDefaultCompaction {
		t.Fatal("overrideDefaultCompaction:false was not honoured")
	}
	// Non-truthy defaults stay for untouched keys.
	if !got.SmartKeepTail || !got.ContinueAfterThresholdCompact {
		t.Fatalf("untouched defaults were changed: %+v", got)
	}
	// A non-boolean truthy JSON value turns a boolean on (JS truthiness).
	if !got.Debug {
		t.Fatal("debug:1 should coerce to true")
	}
	// trackCommands: trimmed, blank and duplicate names dropped.
	wantTracks := []string{"go", "docker"}
	if len(got.TrackCommands) != len(wantTracks) {
		t.Fatalf("TrackCommands = %v, want %v", got.TrackCommands, wantTracks)
	}
	for i := range wantTracks {
		if got.TrackCommands[i] != wantTracks[i] {
			t.Fatalf("TrackCommands = %v, want %v", got.TrackCommands, wantTracks)
		}
	}
	// Non-string array elements are dropped.
	if len(got.SkipForProviders) != 2 || got.SkipForProviders[0] != "openai" {
		t.Fatalf("SkipForProviders = %v", got.SkipForProviders)
	}
	// A bare string array value fails closed to empty, never substring matching.
	if len(got.SkipCustomTypes) != 0 {
		t.Fatalf("SkipCustomTypes = %v, want empty", got.SkipCustomTypes)
	}
}

func TestLoadSettings_MissingFileIsDefaults(t *testing.T) {
	got := LoadSettings(filepath.Join(t.TempDir(), "absent.json"))
	if !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("LoadSettings(missing) = %+v", got)
	}
}

func TestLoadSettings_InvalidJSONIsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSettings(path); !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("LoadSettings(broken) = %+v", got)
	}
	// A JSON array is not an object either.
	if err := os.WriteFile(path, []byte("[1,2]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSettings(path); !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("LoadSettings(array) = %+v", got)
	}
}

func TestScaffoldSettings(t *testing.T) {
	dir := t.TempDir()

	// Missing file: created with the full default block.
	path := filepath.Join(dir, "sub", "pi-vcc-config.json")
	ScaffoldSettingsAt(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != defaultSettingsJSON {
		t.Fatalf("scaffolded file = %q, want the default block", data)
	}

	// Existing values survive; only missing keys are filled.
	if err := os.WriteFile(path, []byte(`{"debug":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ScaffoldSettingsAt(path)
	data, _ = os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("scaffolded JSON is invalid: %v\n%s", err, data)
	}
	if parsed["debug"] != true {
		t.Fatalf("scaffold overwrote an existing value: %s", data)
	}
	if _, present := parsed["smartKeepTail"]; !present {
		t.Fatalf("scaffold did not fill the missing key: %s", data)
	}

	// Invalid JSON is left untouched.
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	ScaffoldSettingsAt(path)
	after, _ := os.ReadFile(path)
	if string(after) != "{broken" {
		t.Fatalf("scaffold clobbered an invalid file: %q", after)
	}
}

func TestScaffoldSettings_KnownKeysFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"zzz":"last","debug":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ScaffoldSettingsAt(path)
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Index(text, `"debug"`) > strings.Index(text, `"zzz"`) {
		t.Fatalf("unknown keys must render after the known ones:\n%s", text)
	}
	// The known keys appear in DEFAULT_SETTINGS order.
	lastIdx := -1
	for _, key := range settingsKeyOrder {
		idx := strings.Index(text, `"`+key+`"`)
		if idx < 0 {
			t.Fatalf("missing key %q:\n%s", key, text)
		}
		if idx < lastIdx {
			t.Fatalf("key %q is out of order:\n%s", key, text)
		}
		lastIdx = idx
	}
}

func TestSettingsPath_HonoursOverride(t *testing.T) {
	// AgentDir() prefers PIG_EXT_HOST_INFO.agentDir (what a PiG host injects into
	// every extension subprocess), so the env-var fallbacks asserted below only
	// win once that channel is cleared. Without this the test resolves to the
	// hosting agent's own directory and fails under a PiG-run test environment.
	t.Setenv("PIG_EXT_HOST_INFO", "")
	t.Setenv("PI_VCC_CONFIG_PATH", "/tmp/custom-pi-vcc.json")
	if got := SettingsPath(); got != "/tmp/custom-pi-vcc.json" {
		t.Fatalf("SettingsPath = %q", got)
	}
	t.Setenv("PI_VCC_CONFIG_PATH", "")
	t.Setenv("PIG_HOME", "/tmp/pighome")
	if got := SettingsPath(); got != filepath.Join("/tmp/pighome", "agent", "pi-vcc-config.json") {
		t.Fatalf("SettingsPath under PIG_HOME = %q", got)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/agentdir")
	if got := SettingsPath(); got != filepath.Join("/tmp/agentdir", "pi-vcc-config.json") {
		t.Fatalf("SettingsPath under PI_CODING_AGENT_DIR = %q", got)
	}
}

func TestAgentDir_ReadsPigExtHostInfo(t *testing.T) {
	t.Setenv("PI_VCC_CONFIG_PATH", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PIG_CODING_AGENT_DIR", "")
	t.Setenv("PIG_HOME", "")
	t.Setenv("PIG_EXT_HOST_INFO", `{"agentDir":"/tmp/pig-agent-from-hostinfo","configDirName":".pig","version":"0.5.2"}`)
	if got := AgentDir(); got != "/tmp/pig-agent-from-hostinfo" {
		t.Fatalf("AgentDir from PIG_EXT_HOST_INFO = %q", got)
	}
	if got := SettingsPath(); got != filepath.Join("/tmp/pig-agent-from-hostinfo", "pi-vcc-config.json") {
		t.Fatalf("SettingsPath from PIG_EXT_HOST_INFO = %q", got)
	}
}
