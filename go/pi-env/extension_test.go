package pienv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// mockTheme implements ThemeRenderer for deterministic testing without ANSI codes.
type mockTheme struct{}

func (m mockTheme) Fg(token, text string) string {
	return "[" + token + "]" + text + "[/" + token + "]"
}

// 1. ResolveValue tests
func TestResolveValue(t *testing.T) {
	envMap := map[string]string{
		"FOO":   "bar",
		"NUM":   "123",
		"NAME":  "alice",
		"EMPTY": "",
	}
	lookup := func(key string) (string, bool) {
		v, ok := envMap[key]
		return v, ok
	}

	tests := []struct {
		name        string
		input       string
		wantStr     string
		wantMissing []string
	}{
		{
			name:        "literal string",
			input:       "hello world",
			wantStr:     "hello world",
			wantMissing: nil,
		},
		{
			name:        "bare variable",
			input:       "prefix_$FOO-suffix",
			wantStr:     "prefix_bar-suffix",
			wantMissing: nil,
		},
		{
			name:        "braced variable",
			input:       "user=${NAME}:8080",
			wantStr:     "user=alice:8080",
			wantMissing: nil,
		},
		{
			name:        "double dollar escape",
			input:       "price: $$100",
			wantStr:     "price: $100",
			wantMissing: nil,
		},
		{
			name:        "triple dollar escape followed by variable",
			input:       "$$$FOO",
			wantStr:     "$bar",
			wantMissing: nil,
		},
		{
			name:        "undefined variable becomes empty and reported",
			input:       "key=$UNKNOWN_KEY",
			wantStr:     "key=",
			wantMissing: []string{"UNKNOWN_KEY"},
		},
		{
			name:        "multiple undefined variables",
			input:       "$MISS1 and ${MISS2}",
			wantStr:     " and ",
			wantMissing: []string{"MISS1", "MISS2"},
		},
		{
			name:        "defined empty variable is not missing",
			input:       "val=$EMPTY",
			wantStr:     "val=",
			wantMissing: nil,
		},
		{
			name:        "non-identifier dollar is kept literal",
			input:       "cost $100 and $",
			wantStr:     "cost $100 and $",
			wantMissing: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, missing := ResolveValue(tc.input, lookup)
			if got != tc.wantStr {
				t.Errorf("ResolveValue(%q) = %q, want %q", tc.input, got, tc.wantStr)
			}
			if !reflect.DeepEqual(missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", missing, tc.wantMissing)
			}
		})
	}
}

func TestResolveValue_SelfReference(t *testing.T) {
	// If VAR exists before resolution, self-reference resolves to existing value
	lookup := func(key string) (string, bool) {
		if key == "PORT" {
			return "3000", true
		}
		return "", false
	}
	got, missing := ResolveValue("$PORT", lookup)
	if got != "3000" || len(missing) != 0 {
		t.Errorf("got (%q, %v), want (\"3000\", nil)", got, missing)
	}

	// If VAR does not exist, it resolves to empty string and is reported missing
	gotEmpty, missingEmpty := ResolveValue("$PORT", func(string) (string, bool) { return "", false })
	if gotEmpty != "" || len(missingEmpty) != 1 || missingEmpty[0] != "PORT" {
		t.Errorf("got (%q, %v), want (\"\", [\"PORT\"])", gotEmpty, missingEmpty)
	}
}

// 2. ExtractEnv tests
func TestExtractEnv(t *testing.T) {
	if ExtractEnv(nil) != nil {
		t.Errorf("ExtractEnv(nil) should be nil")
	}
	if ExtractEnv(map[string]any{}) != nil {
		t.Errorf("ExtractEnv({}) should be nil")
	}
	if ExtractEnv(map[string]any{"env": "not-a-map"}) != nil {
		t.Errorf("ExtractEnv(non-map) should be nil")
	}
	if ExtractEnv(map[string]any{"env": []any{"item"}}) != nil {
		t.Errorf("ExtractEnv(slice) should be nil")
	}
	if ExtractEnv(map[string]any{"env": map[string]any{}}) != nil {
		t.Errorf("ExtractEnv(empty map) should be nil")
	}

	valid := map[string]any{"env": map[string]any{"A": "1"}}
	extracted := ExtractEnv(valid)
	if extracted == nil || extracted["A"] != "1" {
		t.Errorf("ExtractEnv valid failed, got %v", extracted)
	}
}

// 3. IsScalar and ScalarToString tests
func TestIsScalarAndScalarToString(t *testing.T) {
	cases := []struct {
		val      any
		isScalar bool
		wantStr  string
	}{
		{"hello", true, "hello"},
		{true, true, "true"},
		{false, true, "false"},
		{42, true, "42"},
		{int64(100), true, "100"},
		{3.14, true, "3.14"},
		{8080.0, true, "8080"},
		{json.Number("9999"), true, "9999"},
		{nil, false, ""},
		{[]any{1, 2}, false, ""},
		{map[string]any{"k": "v"}, false, ""},
	}

	for _, c := range cases {
		gotScalar := IsScalar(c.val)
		if gotScalar != c.isScalar {
			t.Errorf("IsScalar(%v) = %v, want %v", c.val, gotScalar, c.isScalar)
		}
		if c.isScalar {
			gotStr := ScalarToString(c.val)
			if gotStr != c.wantStr {
				t.Errorf("ScalarToString(%v) = %q, want %q", c.val, gotStr, c.wantStr)
			}
		}
	}
}

// 4. Merge priority: project overrides global
func TestApplyEnvExplicit_MergePriority(t *testing.T) {
	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	global := map[string]any{
		"SHARED":   "global_val",
		"GLOBAL_1": "g1",
	}
	project := map[string]any{
		"SHARED":    "project_val",
		"PROJECT_1": "p1",
	}

	res := ApplyEnvExplicit(global, project, lookup, setenv, unsetenv)

	if mockEnv["SHARED"] != "project_val" {
		t.Errorf("SHARED = %q, want 'project_val'", mockEnv["SHARED"])
	}
	if mockEnv["GLOBAL_1"] != "g1" {
		t.Errorf("GLOBAL_1 = %q, want 'g1'", mockEnv["GLOBAL_1"])
	}
	if mockEnv["PROJECT_1"] != "p1" {
		t.Errorf("PROJECT_1 = %q, want 'p1'", mockEnv["PROJECT_1"])
	}

	// Verify reports count and sources
	if len(res.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(res.Reports))
	}
	if res.Reports[0].Source != "global" || res.Reports[0].Variables["SHARED"] != "global_val" {
		t.Errorf("unexpected global report: %+v", res.Reports[0])
	}
	if res.Reports[1].Source != "project" || res.Reports[1].Variables["SHARED"] != "project_val" {
		t.Errorf("unexpected project report: %+v", res.Reports[1])
	}
}

// 5. Illegal config / Non-scalar keys ignored with warnings
func TestApplyEnvExplicit_NonScalarIgnored(t *testing.T) {
	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	global := map[string]any{
		"VALID":     "abc",
		"OBJ_KEY":   map[string]any{"nested": "val"},
		"ARRAY_KEY": []any{1, 2, 3},
	}

	res := ApplyEnvExplicit(global, nil, lookup, setenv, unsetenv)

	if mockEnv["VALID"] != "abc" {
		t.Errorf("VALID = %q, want 'abc'", mockEnv["VALID"])
	}
	if _, exists := mockEnv["OBJ_KEY"]; exists {
		t.Errorf("OBJ_KEY should not be set in env")
	}
	if _, exists := mockEnv["ARRAY_KEY"]; exists {
		t.Errorf("ARRAY_KEY should not be set in env")
	}

	expectedNonScalar := []string{"ARRAY_KEY", "OBJ_KEY"}
	sort.Strings(res.NonScalarKeys)
	if !reflect.DeepEqual(res.NonScalarKeys, expectedNonScalar) {
		t.Errorf("NonScalarKeys = %v, want %v", res.NonScalarKeys, expectedNonScalar)
	}
}

// 6. Reload and tracker key cleanup
func TestApplyEnvExplicit_ReloadStaleKeyCleanup(t *testing.T) {
	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	// First load sets K1, K2
	firstConfig := map[string]any{
		"K1": "v1",
		"K2": "v2",
	}
	ApplyEnvExplicit(firstConfig, nil, lookup, setenv, unsetenv)

	if mockEnv["K1"] != "v1" || mockEnv["K2"] != "v2" {
		t.Fatalf("first load failed: %+v", mockEnv)
	}
	if mockEnv[TrackerKey] != `["K1","K2"]` {
		t.Fatalf("TrackerKey = %q, want '[\"K1\",\"K2\"]'", mockEnv[TrackerKey])
	}

	// Reload with K1 removed, only K2 remains
	secondConfig := map[string]any{
		"K2": "v2_updated",
	}
	ApplyEnvExplicit(secondConfig, nil, lookup, setenv, unsetenv)

	if _, exists := mockEnv["K1"]; exists {
		t.Errorf("K1 should have been deleted upon reload")
	}
	if mockEnv["K2"] != "v2_updated" {
		t.Errorf("K2 = %q, want 'v2_updated'", mockEnv["K2"])
	}
	if mockEnv[TrackerKey] != `["K2"]` {
		t.Errorf("TrackerKey = %q, want '[\"K2\"]'", mockEnv[TrackerKey])
	}

	// Reload with all keys removed
	ApplyEnvExplicit(nil, nil, lookup, setenv, unsetenv)
	if _, exists := mockEnv["K2"]; exists {
		t.Errorf("K2 should have been deleted when empty")
	}
	if _, exists := mockEnv[TrackerKey]; exists {
		t.Errorf("TrackerKey should be unset when empty")
	}
}

// 7. System environment variable override tracking
func TestApplyEnvExplicit_OverridesTracking(t *testing.T) {
	mockEnv := map[string]string{
		"SYSTEM_VAR": "host_orig",
	}
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	config := map[string]any{
		"SYSTEM_VAR": "overwritten",
		"NEW_VAR":    "fresh",
	}

	res := ApplyEnvExplicit(config, nil, lookup, setenv, unsetenv)

	if !reflect.DeepEqual(res.OverriddenVars, []string{"SYSTEM_VAR"}) {
		t.Errorf("OverriddenVars = %v, want ['SYSTEM_VAR']", res.OverriddenVars)
	}
	if mockEnv["SYSTEM_VAR"] != "overwritten" {
		t.Errorf("SYSTEM_VAR = %q, want 'overwritten'", mockEnv["SYSTEM_VAR"])
	}
	if mockEnv[OverridesKey] != `["SYSTEM_VAR"]` {
		t.Errorf("OverridesKey = %q, want '[\"SYSTEM_VAR\"]'", mockEnv[OverridesKey])
	}

	// Next reload retains SYSTEM_VAR as overridden even though previousKeys has it
	res2 := ApplyEnvExplicit(config, nil, lookup, setenv, unsetenv)
	if !reflect.DeepEqual(res2.OverriddenVars, []string{"SYSTEM_VAR"}) {
		t.Errorf("res2 OverriddenVars = %v, want ['SYSTEM_VAR']", res2.OverriddenVars)
	}
}

// 8. FormatReport tests
func TestFormatReport(t *testing.T) {
	theme := mockTheme{}

	// Empty reports
	emptyStr := FormatReport(theme, nil)
	wantEmpty := "[dim][env] No environment variables configured.[/dim]"
	if emptyStr != wantEmpty {
		t.Errorf("empty report = %q, want %q", emptyStr, wantEmpty)
	}

	// Non-empty reports
	reports := []EnvReport{
		{
			Source: "global",
			Variables: map[string]string{
				"A_KEY": "100",
			},
		},
	}
	repStr := FormatReport(theme, reports)
	expectedLines := []string{
		"[accent][env][/accent]",
		"[muted]  global[/muted]",
		// A_KEY matches the sensitive-key heuristic; value "100" is short so fully masked.
		"    [success]A_KEY[/success][dim]=[/dim][muted]…[/muted]",
	}
	wantRep := strings.Join(expectedLines, "\n")
	if repStr != wantRep {
		t.Errorf("repStr = %q, want %q", repStr, wantRep)
	}
}

// 8b. FormatReport masks values of sensitive keys (KEY/TOKEN/SECRET/...), not others
func TestFormatReport_SecretMasking(t *testing.T) {
	// fake: 19-char fixture built at runtime so no realistic credential literal
	// appears in the source (exercises the first4…last4 branch, len>16).
	fake := "s" + strings.Repeat("e", 17) + "c"
	report := EnvReport{
		Source: "global",
		Keys:   []string{"TINYFISH_API_KEY", "FOO", "MY_TOKEN", "EMPTY_SECRET"},
		Variables: map[string]string{
			"TINYFISH_API_KEY": fake,
			"FOO":              "bar",
			"MY_TOKEN":         "short",
			"EMPTY_SECRET":     "",
		},
	}

	out := FormatReport(nil, []EnvReport{report})

	lines := strings.Split(out, "\n")
	findLine := func(prefix string) string {
		for _, l := range lines {
			if strings.Contains(l, prefix) {
				return l
			}
		}
		return ""
	}

	full := report.Variables["TINYFISH_API_KEY"]
	wantMasked := full[:4] + "…" + full[len(full)-4:]

	if got := findLine("TINYFISH_API_KEY"); !strings.Contains(got, wantMasked) {
		t.Errorf("expected masked value %q in TINYFISH_API_KEY line, got %q\nfull report:\n%s", wantMasked, got, out)
	}
	if strings.Contains(out, full) {
		t.Errorf("full secret value leaked in report:\n%s", out)
	}
	if got := findLine("FOO"); !strings.Contains(got, "bar") {
		t.Errorf("non-sensitive FOO should render verbatim, got %q\nfull report:\n%s", got, out)
	}
	// Short sensitive value (<=16 chars) fully masked, no partial reveal.
	if got := findLine("MY_TOKEN"); strings.Contains(got, "short") || !strings.Contains(got, "…") {
		t.Errorf("short sensitive value should be fully masked, got %q\nfull report:\n%s", got, out)
	}
	if got := findLine("EMPTY_SECRET"); !strings.Contains(got, "(empty)") {
		t.Errorf("empty sensitive value should render '(empty)', got %q\nfull report:\n%s", got, out)
	}
}

// 8c. maskSecretValue boundary rules: 0 -> (empty); <=16 runes fully masked;
// >16 runes reveal only first4…last4 (never more than 8 chars of the value).
func TestMaskSecretValue_Boundaries(t *testing.T) {
	if got := maskSecretValue(""); got != "(empty)" {
		t.Errorf("empty value = %q, want '(empty)'", got)
	}

	fullyMasked := map[string]int{
		"9-char boundary":  9,
		"16-char boundary": 16,
		"short value":      8,
		"1 char":           1,
	}
	for name, n := range fullyMasked {
		val := strings.Repeat("p", n)
		if got := maskSecretValue(val); got != "…" {
			t.Errorf("%s: maskSecretValue(%d chars) = %q, want fully masked '…'", name, n, got)
		}
	}

	// 17-char boundary: first4…last4 reveal.
	val17 := "0123456789ABCDEFG"
	if got, want := maskSecretValue(val17), "0123…DEFG"; got != want {
		t.Errorf("17-char value = %q, want %q", got, want)
	}

	// Long token: first4…last4, and never more than 8 chars of the value visible.
	long := "s" + strings.Repeat("e", 17) + "c" // 19 chars
	masked := maskSecretValue(long)
	if want := "seee…eeec"; masked != want {
		t.Errorf("19-char value = %q, want %q", masked, want)
	}
	for n := 9; n <= len(long); n++ {
		for i := 0; i+n <= len(long); i++ {
			if strings.Contains(masked, long[i:i+n]) {
				t.Fatalf("masked %q leaks %d consecutive chars %q of secret", masked, n, long[i:i+n])
			}
		}
	}
}

// 8d. sensitiveEnvKey coverage: URL/URI/PASSCODE/CREDS markers (and existing
// PASS via PGPASS) mask values in the /env report; plainly non-sensitive keys
// render verbatim. Values are runtime-built fixtures, not real credentials.
func TestSensitiveEnvKey_Coverage(t *testing.T) {
	// 20-char fixture -> first4…last4 branch; never a real credential.
	fake := "x" + strings.Repeat("y", 18) + "z"
	wantMasked := fake[:4] + "…" + fake[len(fake)-4:]

	sensitive := []string{"DATABASE_URL", "PGPASS", "MY_PASSCODE", "AWS_CREDS"}
	nonSensitive := []string{"FOO", "EDITOR"}

	var keys []string
	vars := make(map[string]string)
	for _, k := range append(append([]string{}, sensitive...), nonSensitive...) {
		keys = append(keys, k)
		vars[k] = fake
	}
	out := FormatReport(nil, []EnvReport{{Source: "global", Keys: keys, Variables: vars}})

	lineOf := func(key string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), key+"=") {
				return l
			}
		}
		return ""
	}

	for _, k := range sensitive {
		got := lineOf(k)
		if got == "" {
			t.Fatalf("missing report line for %s\nfull report:\n%s", k, out)
		}
		if !strings.Contains(got, wantMasked) {
			t.Errorf("%s should be masked as %q, got %q\nfull report:\n%s", k, wantMasked, got, out)
		}
	}
	for _, k := range nonSensitive {
		got := lineOf(k)
		if got == "" {
			t.Fatalf("missing report line for %s\nfull report:\n%s", k, out)
		}
		if !strings.Contains(got, "="+fake) {
			t.Errorf("non-sensitive %s should render verbatim, got %q\nfull report:\n%s", k, got, out)
		}
	}
}

// 9. BuildSessionStartWarnings tests
func TestBuildSessionStartWarnings(t *testing.T) {
	theme := mockTheme{}

	// No warnings
	if BuildSessionStartWarnings(theme, nil, nil, nil) != "" {
		t.Errorf("expected empty string when no warnings")
	}

	// All 3 kinds of warnings
	warnStr := BuildSessionStartWarnings(
		theme,
		[]string{"bad_obj"},
		[]string{"MISS1", "MISS1"}, // duplicate to test unique()
		[]string{"EXISTING_KEY"},
	)

	expected := strings.Join([]string{
		"[accent][env][/accent]",
		"  [warning]ignoring non-scalar values:[/warning] bad_obj",
		"  [warning]unresolved variables:[/warning] MISS1",
		"  [warning]overriding existing variables:[/warning] EXISTING_KEY",
	}, "\n")

	if warnStr != expected {
		t.Errorf("warnStr = %q, want %q", warnStr, expected)
	}
}

// 10. FilterContextMessages tests (filter customType=pi-env with role=custom)
func TestFilterContextMessages(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi there"},
		map[string]any{"role": "custom", "customType": "pi-env", "content": "env status message"},
		map[string]any{"role": "custom", "customType": "other-ext", "content": "other custom"},
		map[string]any{"role": "system", "content": "system prompt"},
	}

	filtered := FilterContextMessages(messages)

	if len(filtered) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(filtered))
	}

	roles := make([]string, len(filtered))
	for i, m := range filtered {
		roles[i] = m["role"].(string)
		if m["role"] == "custom" && m["customType"] == MessageType {
			t.Errorf("found customType=pi-env in filtered messages")
		}
	}

	expectedRoles := []string{"user", "assistant", "custom", "system"}
	if !reflect.DeepEqual(roles, expectedRoles) {
		t.Errorf("roles = %v, want %v", roles, expectedRoles)
	}
}

// 11. RenderMessage tests
func TestRenderMessageText(t *testing.T) {
	// Simple string
	text1 := RenderMessageText("line 1\nline 2")
	if text1 != "line 1\nline 2" {
		t.Errorf("text1 = %q", text1)
	}

	// Structured text parts
	parts := []any{
		map[string]any{"type": "text", "text": "part A"},
		map[string]any{"type": "image", "data": "base64"}, // non-text part ignored
		map[string]any{"type": "text", "text": "part B"},
	}
	text2 := RenderMessageText(parts)
	if text2 != "part A\npart B" {
		t.Errorf("text2 = %q, want 'part A\\npart B'", text2)
	}

	// MessageRendererHandler lines
	lines, err := MessageRendererHandler(sdk.Context{}, map[string]any{"content": text2}, sdk.MessageRenderOptions{}, 80)
	if err != nil {
		t.Fatalf("MessageRendererHandler err: %v", err)
	}
	if !reflect.DeepEqual(lines, []string{"part A", "part B"}) {
		t.Errorf("lines = %v, want ['part A', 'part B']", lines)
	}
}

// 12. Real environment rollback test (guarantees t.Setenv isolation)
func TestApplyEnv_RealEnvRollback(t *testing.T) {
	tmpDir := t.TempDir()
	piDir := filepath.Join(tmpDir, ".pi")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatal(err)
	}

	settingsJSON := `{
		"env": {
			"TEST_ISOLATED_VAR": "val_123",
			"TEST_EXPAND": "$PREEXISTING_BASE/sub"
		}
	}`
	if err := os.WriteFile(filepath.Join(piDir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Isolate via t.Setenv
	emptyAgentDir := filepath.Join(tmpDir, "empty_agent")
	_ = os.MkdirAll(emptyAgentDir, 0o755)
	t.Setenv("PI_CODING_AGENT_DIR", emptyAgentDir)
	t.Setenv("PIG_CODING_AGENT_DIR", emptyAgentDir)
	t.Setenv("PIG_HOME", tmpDir)
	t.Setenv("PREEXISTING_BASE", "/opt/app")
	t.Setenv("TEST_ISOLATED_VAR", "")
	t.Setenv("TEST_EXPAND", "")
	t.Setenv(TrackerKey, "")
	t.Setenv(OverridesKey, "")

	res := ApplyEnv(tmpDir)

	if os.Getenv("TEST_ISOLATED_VAR") != "val_123" {
		t.Errorf("TEST_ISOLATED_VAR = %q, want 'val_123'", os.Getenv("TEST_ISOLATED_VAR"))
	}
	if os.Getenv("TEST_EXPAND") != "/opt/app/sub" {
		t.Errorf("TEST_EXPAND = %q, want '/opt/app/sub'", os.Getenv("TEST_EXPAND"))
	}
	if len(res.Reports) != 1 || res.Reports[0].Source != "project" {
		t.Errorf("unexpected reports: %+v", res.Reports)
	}
}

// 13. Factory Extension() smoke test with hermetic isolation
func TestExtensionSmoke(t *testing.T) {
	tmpDir := t.TempDir()
	emptyAgentDir := filepath.Join(tmpDir, "empty_agent")
	if err := os.MkdirAll(emptyAgentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PI_CODING_AGENT_DIR", emptyAgentDir)
	t.Setenv("PIG_CODING_AGENT_DIR", emptyAgentDir)
	t.Setenv("PIG_HOME", tmpDir)
	t.Setenv(TrackerKey, "")
	t.Setenv(OverridesKey, "")

	ext := Extension()
	if ext == nil {
		t.Fatal("Extension() returned nil")
	}
}

// 14. Key order: FormatReport output follows declaration/insertion order
func TestKeyOrder_ReportFormatOrder(t *testing.T) {
	theme := mockTheme{}
	report := EnvReport{
		Source: "global",
		Keys:   []string{"ZULU", "MIKE", "ALPHA"},
		Variables: map[string]string{
			"ZULU":  "z",
			"MIKE":  "m",
			"ALPHA": "a",
		},
	}
	formatted := FormatReport(theme, []EnvReport{report})
	expectedLines := []string{
		"[accent][env][/accent]",
		"[muted]  global[/muted]",
		"    [success]ZULU[/success][dim]=[/dim][muted]z[/muted]",
		"    [success]MIKE[/success][dim]=[/dim][muted]m[/muted]",
		"    [success]ALPHA[/success][dim]=[/dim][muted]a[/muted]",
	}
	want := strings.Join(expectedLines, "\n")
	if formatted != want {
		t.Errorf("FormatReport order mismatch.\ngot:\n%s\nwant:\n%s", formatted, want)
	}
}

// 15. Key order: BuildSessionStartWarnings warning line items preserve declaration order
func TestKeyOrder_WarningOrder(t *testing.T) {
	theme := mockTheme{}
	nonScalar := []string{"OBJ_Z", "OBJ_M", "OBJ_A"}
	unresolved := []string{"MISS_Z", "MISS_A", "MISS_Z"} // duplicate to verify FIFO uniqueness
	overridden := []string{"OVR_Z", "OVR_A"}

	warn := BuildSessionStartWarnings(theme, nonScalar, unresolved, overridden)
	expected := strings.Join([]string{
		"[accent][env][/accent]",
		"  [warning]ignoring non-scalar values:[/warning] OBJ_Z, OBJ_M, OBJ_A",
		"  [warning]unresolved variables:[/warning] MISS_Z, MISS_A",
		"  [warning]overriding existing variables:[/warning] OVR_Z, OVR_A",
	}, "\n")

	if warn != expected {
		t.Errorf("BuildSessionStartWarnings order mismatch.\ngot:\n%s\nwant:\n%s", warn, expected)
	}
}

// 16. Key order: Sibling reference counterexample
// In TS: {"ZPATH":"$ABASE/x","ABASE":"/opt"} -> ZPATH is evaluated first when ABASE is undefined,
// resulting in ZPATH="/x" and ABASE being recorded in unresolvedVars.
// If sorted alphabetically, ABASE is evaluated first, producing "/opt/x" (incorrect).
func TestKeyOrder_SiblingReferenceCounterexample(t *testing.T) {
	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	// Input with ZPATH before ABASE in JSON
	jsonContent := `{"ZPATH":"$ABASE/x","ABASE":"/opt"}`
	env, err := ParseOrderedEnvJSON([]byte(jsonContent))
	if err != nil {
		t.Fatalf("ParseOrderedEnvJSON err: %v", err)
	}

	res := ApplyEnvExplicit(env, nil, lookup, setenv, unsetenv)

	// Sibling reference before definition resolves to empty and marks ABASE unresolved
	if mockEnv["ZPATH"] != "/x" {
		t.Errorf("ZPATH = %q, want '/x' (if it became '/opt/x', key ordering was broken)", mockEnv["ZPATH"])
	}
	if mockEnv["ABASE"] != "/opt" {
		t.Errorf("ABASE = %q, want '/opt'", mockEnv["ABASE"])
	}
	if !contains(res.UnresolvedVars, "ABASE") {
		t.Errorf("expected ABASE in UnresolvedVars, got %v", res.UnresolvedVars)
	}
}

// 17. Key order: Global and project section order and two-stage override
func TestKeyOrder_GlobalAndProjectSections(t *testing.T) {
	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	globalJSON := `{"G_Z":"gz","G_A":"ga","SHARED":"g_val"}`
	projectJSON := `{"SHARED":"p_val","P_Z":"pz","P_A":"pa"}`

	gEnv, err := ParseOrderedEnvJSON([]byte(globalJSON))
	if err != nil {
		t.Fatal(err)
	}
	pEnv, err := ParseOrderedEnvJSON([]byte(projectJSON))
	if err != nil {
		t.Fatal(err)
	}

	res := ApplyEnvExplicit(gEnv, pEnv, lookup, setenv, unsetenv)

	if len(res.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(res.Reports))
	}

	// 1. Global report order
	if res.Reports[0].Source != "global" {
		t.Errorf("report 0 source = %q, want 'global'", res.Reports[0].Source)
	}
	wantGlobalKeys := []string{"G_Z", "G_A", "SHARED"}
	if !reflect.DeepEqual(res.Reports[0].Keys, wantGlobalKeys) {
		t.Errorf("global report Keys = %v, want %v", res.Reports[0].Keys, wantGlobalKeys)
	}

	// 2. Project report order
	if res.Reports[1].Source != "project" {
		t.Errorf("report 1 source = %q, want 'project'", res.Reports[1].Source)
	}
	wantProjectKeys := []string{"SHARED", "P_Z", "P_A"}
	if !reflect.DeepEqual(res.Reports[1].Keys, wantProjectKeys) {
		t.Errorf("project report Keys = %v, want %v", res.Reports[1].Keys, wantProjectKeys)
	}

	// 3. Two-stage override: project overrides global
	if mockEnv["SHARED"] != "p_val" {
		t.Errorf("SHARED = %q, want 'p_val'", mockEnv["SHARED"])
	}
}

// 18. Key order: Benchmark side-by-side comparison with Node TS output
func TestKeyOrder_SideBySideComparison(t *testing.T) {
	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	rawJSON := `{"ZPATH":"$ABASE/x","ABASE":"/opt","MIKE":"m","ZULU":"z"}`
	env, err := ParseOrderedEnvJSON([]byte(rawJSON))
	if err != nil {
		t.Fatal(err)
	}

	res := ApplyEnvExplicit(nil, env, lookup, setenv, unsetenv)
	theme := mockTheme{}
	report := FormatReport(theme, res.Reports)

	expectedReport := strings.Join([]string{
		"[accent][env][/accent]",
		"[muted]  project[/muted]",
		"    [success]ZPATH[/success][dim]=[/dim][muted]/x[/muted]",
		"    [success]ABASE[/success][dim]=[/dim][muted]/opt[/muted]",
		"    [success]MIKE[/success][dim]=[/dim][muted]m[/muted]",
		"    [success]ZULU[/success][dim]=[/dim][muted]z[/muted]",
	}, "\n")

	if report != expectedReport {
		t.Errorf("Report mismatch:\ngot:\n%s\nwant:\n%s", report, expectedReport)
	}
	if mockEnv["ZPATH"] != "/x" {
		t.Errorf("ZPATH = %q, want '/x'", mockEnv["ZPATH"])
	}
	if !reflect.DeepEqual(res.UnresolvedVars, []string{"ABASE"}) {
		t.Errorf("UnresolvedVars = %v, want ['ABASE']", res.UnresolvedVars)
	}

	t.Logf("\n=== Go Output ===\nZPATH=%s\nUnresolved=[\"%s\"]\nReport:\n%s",
		mockEnv["ZPATH"], strings.Join(res.UnresolvedVars, "\",\""), report)
}

// 19. Strict settings JSON regression tests (Must Fix)
func TestStrictSettings_NoEnvKeyZeroInjection(t *testing.T) {
	tmpDir := t.TempDir()
	piDir := filepath.Join(tmpDir, ".pi")
	if err := os.MkdirAll(piDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Isolate global agent dir with no env key
	globalAgentDir := filepath.Join(tmpDir, "global_agent")
	if err := os.MkdirAll(globalAgentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalAgentDir, "settings.json"), []byte(`{"theme":"solarized"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", globalAgentDir)
	t.Setenv("PIG_CODING_AGENT_DIR", "")
	t.Setenv("PIG_HOME", "")

	// Project settings completely lacking "env" key
	settingsJSON := `{"theme":"dark","defaultModel":"m"}`
	if err := os.WriteFile(filepath.Join(piDir, "settings.json"), []byte(settingsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	globalEnv, projectEnv := LoadOrderedSettings(tmpDir)
	if projectEnv != nil {
		t.Fatalf("expected projectEnv to be nil when 'env' key is absent, got %v", projectEnv.Keys())
	}
	if globalEnv != nil {
		t.Fatalf("expected globalEnv to be nil when 'env' key is absent, got %v", globalEnv.Keys())
	}

	res := ApplyEnvExplicit(globalEnv, projectEnv, lookup, setenv, unsetenv)

	// Clean up TrackerKey / OverridesKey for clean accounting
	delete(mockEnv, TrackerKey)
	delete(mockEnv, OverridesKey)

	report := FormatReport(nil, res.Reports)
	warnText := BuildSessionStartWarnings(nil, res.NonScalarKeys, res.UnresolvedVars, res.OverriddenVars)

	if len(mockEnv) != 0 {
		t.Errorf("expected zero env injections, got %v", mockEnv)
	}
	if len(res.Reports) != 0 {
		t.Errorf("expected zero reports, got %d", len(res.Reports))
	}
	if report != "[env] No environment variables configured." {
		t.Errorf("expected empty config report, got %q", report)
	}
	if len(res.NonScalarKeys) != 0 {
		t.Errorf("expected zero non-scalar warnings, got %v", res.NonScalarKeys)
	}
	if warnText != "" {
		t.Errorf("expected empty warnings, got %q", warnText)
	}

	t.Logf("\n=== settings.json 无 env 键时的注入结果 ===\nInjected keys: %v\nReports count: %d\nReport text: %s\nNonScalarKeys: %v\nWarnings: %q\n",
		mockEnv, len(res.Reports), report, res.NonScalarKeys, warnText)
}

func TestStrictSettings_EmptyAndNullEnvKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty env object", content: `{"theme":"dark","env":{}}`},
		{name: "null env key", content: `{"theme":"dark","env":null}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := ParseOrderedSettingsJSON([]byte(tc.content))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if env != nil {
				t.Fatalf("expected nil env, got %v", env.Keys())
			}

			mockEnv := make(map[string]string)
			lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
			setenv := func(k, v string) error { mockEnv[k] = v; return nil }
			unsetenv := func(k string) error { delete(mockEnv, k); return nil }

			res := ApplyEnvExplicit(nil, env, lookup, setenv, unsetenv)
			delete(mockEnv, TrackerKey)
			delete(mockEnv, OverridesKey)

			if len(mockEnv) != 0 {
				t.Errorf("expected 0 injected keys, got %v", mockEnv)
			}
			if len(res.Reports) != 0 {
				t.Errorf("expected 0 reports, got %d", len(res.Reports))
			}
			if len(res.NonScalarKeys) != 0 {
				t.Errorf("expected 0 non-scalar keys, got %v", res.NonScalarKeys)
			}
		})
	}
}

func TestStrictSettings_OnlyEnvInjectedThemeExcluded(t *testing.T) {
	content := `{"theme":"dark","env":{"A":"1"}}`
	env, err := ParseOrderedSettingsJSON([]byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil env")
	}

	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	res := ApplyEnvExplicit(nil, env, lookup, setenv, unsetenv)
	delete(mockEnv, TrackerKey)
	delete(mockEnv, OverridesKey)

	if mockEnv["A"] != "1" {
		t.Errorf("A = %q, want '1'", mockEnv["A"])
	}
	if _, themeExists := mockEnv["theme"]; themeExists {
		t.Errorf("theme must NOT be injected into env")
	}
	if len(res.NonScalarKeys) != 0 {
		t.Errorf("expected 0 non-scalar keys, got %v", res.NonScalarKeys)
	}
	if len(res.Reports) != 1 || res.Reports[0].Variables["A"] != "1" {
		t.Errorf("unexpected reports: %+v", res.Reports)
	}
	if _, themeInReport := res.Reports[0].Variables["theme"]; themeInReport {
		t.Errorf("theme must NOT be present in reports")
	}
}

func TestStrictSettings_ExistingBehaviorPreserved(t *testing.T) {
	content := `{"theme":"dark","env":{"ZPATH":"$ABASE/x","ABASE":"/opt","PORT":8080}}`
	env, err := ParseOrderedSettingsJSON([]byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil env")
	}

	wantKeys := []string{"ZPATH", "ABASE", "PORT"}
	if !reflect.DeepEqual(env.Keys(), wantKeys) {
		t.Fatalf("env.Keys() = %v, want %v", env.Keys(), wantKeys)
	}

	mockEnv := make(map[string]string)
	lookup := func(k string) (string, bool) { v, ok := mockEnv[k]; return v, ok }
	setenv := func(k, v string) error { mockEnv[k] = v; return nil }
	unsetenv := func(k string) error { delete(mockEnv, k); return nil }

	res := ApplyEnvExplicit(nil, env, lookup, setenv, unsetenv)

	if mockEnv["ZPATH"] != "/x" {
		t.Errorf("ZPATH = %q, want '/x'", mockEnv["ZPATH"])
	}
	if mockEnv["ABASE"] != "/opt" {
		t.Errorf("ABASE = %q, want '/opt'", mockEnv["ABASE"])
	}
	if mockEnv["PORT"] != "8080" {
		t.Errorf("PORT = %q, want '8080'", mockEnv["PORT"])
	}
	if _, themeExists := mockEnv["theme"]; themeExists {
		t.Errorf("theme must NOT be injected into env")
	}

	if !reflect.DeepEqual(res.UnresolvedVars, []string{"ABASE"}) {
		t.Errorf("res.UnresolvedVars = %v, want ['ABASE']", res.UnresolvedVars)
	}

	theme := mockTheme{}
	report := FormatReport(theme, res.Reports)
	expectedReport := strings.Join([]string{
		"[accent][env][/accent]",
		"[muted]  project[/muted]",
		"    [success]ZPATH[/success][dim]=[/dim][muted]/x[/muted]",
		"    [success]ABASE[/success][dim]=[/dim][muted]/opt[/muted]",
		"    [success]PORT[/success][dim]=[/dim][muted]8080[/muted]",
	}, "\n")

	if report != expectedReport {
		t.Errorf("Report mismatch:\ngot:\n%s\nwant:\n%s", report, expectedReport)
	}
}
