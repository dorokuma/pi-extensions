package autocontinue

import (
	"os"
	"path/filepath"
	"testing"
)

// lookupFrom builds a LookupEnv over a map, so tests never depend on the real
// process environment.
func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestDefaultPersist(t *testing.T) {
	got := DefaultPersist()
	want := Persist{Enabled: true, MaxAttempts: 20, DelayMs: 2500}
	if got != want {
		t.Fatalf("DefaultPersist() = %+v, want %+v", got, want)
	}
}

// TestSettingsPathIsolation pins the resolution order documented as difference
// D1: PIG_HOME wins (so an isolated PiG run never touches ~/.pi), otherwise the
// TS path.
func TestSettingsPathIsolation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIG_HOME", "")

	if got, want := SettingsPath(), filepath.Join(home, ".pi", "agent", "auto-continue.json"); got != want {
		t.Fatalf("SettingsPath() without PIG_HOME = %q, want %q", got, want)
	}

	isolated := t.TempDir()
	t.Setenv("PIG_HOME", isolated)
	if got, want := SettingsPath(), filepath.Join(isolated, "agent", "auto-continue.json"); got != want {
		t.Fatalf("SettingsPath() with PIG_HOME = %q, want %q", got, want)
	}
}

func TestLoadPersistWritesDefaultsWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "auto-continue.json")
	got := loadPersist(path)
	if got != DefaultPersist() {
		t.Fatalf("loadPersist(missing) = %+v, want defaults", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("defaults were not written: %v", err)
	}
	const want = "{\n  \"enabled\": true,\n  \"maxAttempts\": 20,\n  \"delayMs\": 2500\n}\n"
	if string(body) != want {
		t.Fatalf("written defaults = %q, want %q", string(body), want)
	}
}

func TestParsePersist(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Persist
		wantErr bool
	}{
		{name: "full", raw: `{"enabled":false,"maxAttempts":5,"delayMs":100}`, want: Persist{false, 5, 100}},
		{name: "empty object falls back", raw: `{}`, want: DefaultPersist()},
		{name: "invalid json", raw: `{oops`, want: DefaultPersist(), wantErr: true},
		{name: "json null", raw: `null`, want: DefaultPersist(), wantErr: true},
		{name: "enabled only false", raw: `{"enabled":false}`, want: Persist{false, 20, 2500}},
		{name: "enabled truthy string stays true", raw: `{"enabled":"no"}`, want: DefaultPersist()},
		{name: "maxAttempts clamped low", raw: `{"maxAttempts":0}`, want: Persist{true, 1, 2500}},
		{name: "maxAttempts clamped high", raw: `{"maxAttempts":999}`, want: Persist{true, 20, 2500}},
		{name: "maxAttempts floored", raw: `{"maxAttempts":7.9}`, want: Persist{true, 7, 2500}},
		{name: "maxAttempts numeric string", raw: `{"maxAttempts":"12"}`, want: Persist{true, 12, 2500}},
		{name: "maxAttempts null becomes minimum", raw: `{"maxAttempts":null}`, want: Persist{true, 1, 2500}},
		{name: "maxAttempts garbage keeps fallback", raw: `{"maxAttempts":"abc"}`, want: DefaultPersist()},
		{name: "delayMs clamped low", raw: `{"delayMs":-5}`, want: Persist{true, 20, 0}},
		{name: "delayMs clamped high", raw: `{"delayMs":999999}`, want: Persist{true, 20, 60000}},
		{name: "delayMs zero survives", raw: `{"delayMs":0}`, want: Persist{true, 20, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParsePersist(tc.raw)
			if tc.wantErr && ok {
				t.Fatalf("ParsePersist(%q) reported success, want parse failure", tc.raw)
			}
			if !tc.wantErr && !ok {
				t.Fatalf("ParsePersist(%q) reported failure", tc.raw)
			}
			if got != tc.want {
				t.Fatalf("ParsePersist(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestLoadPersistRewritesUnparseableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-continue.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadPersist(path); got != DefaultPersist() {
		t.Fatalf("loadPersist(garbage) = %+v, want defaults", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\n  \"enabled\": true,\n  \"maxAttempts\": 20,\n  \"delayMs\": 2500\n}\n" {
		t.Fatalf("file was not rewritten with defaults: %q", string(raw))
	}
}

func TestSavePersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-continue.json")
	want := Persist{Enabled: false, MaxAttempts: 3, DelayMs: 0}
	savePersist(path, want)
	if got := loadPersist(path); got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestEnvEnabledOverride(t *testing.T) {
	cases := []struct {
		value string
		set   bool
		want  *bool
	}{
		{value: "", set: false},
		{value: "", set: true},
		{value: "   ", set: true},
		{value: "0", set: true, want: boolPtr(false)},
		{value: " FALSE ", set: true, want: boolPtr(false)},
		{value: "off", set: true, want: boolPtr(false)},
		{value: "no", set: true, want: boolPtr(false)},
		{value: "1", set: true, want: boolPtr(true)},
		{value: "TRUE", set: true, want: boolPtr(true)},
		{value: "on", set: true, want: boolPtr(true)},
		{value: "yes", set: true, want: boolPtr(true)},
		{value: "maybe", set: true},
	}
	for _, tc := range cases {
		env := map[string]string{}
		if tc.set {
			env["PI_AUTO_CONTINUE"] = tc.value
		}
		got := envEnabledOverride(lookupFrom(env))
		switch {
		case tc.want == nil && got != nil:
			t.Fatalf("envEnabledOverride(%q) = %v, want nil", tc.value, *got)
		case tc.want != nil && got == nil:
			t.Fatalf("envEnabledOverride(%q) = nil, want %v", tc.value, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Fatalf("envEnabledOverride(%q) = %v, want %v", tc.value, *got, *tc.want)
		}
	}
}

func TestEnvInt(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  int
	}{
		{name: "unset", want: 9},
		{name: "blank", value: "  ", set: true, want: 9},
		{name: "garbage", value: "abc", set: true, want: 9},
		{name: "plain", value: "5", set: true, want: 5},
		{name: "clamped low", value: "-3", set: true, want: 1},
		{name: "clamped high", value: "99", set: true, want: 20},
		{name: "floored", value: "4.8", set: true, want: 4},
		{name: "exponent", value: "1e1", set: true, want: 10},
		{name: "hex like JS Number", value: "0x10", set: true, want: 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.set {
				env["PI_AUTO_CONTINUE_MAX"] = tc.value
			}
			if got := envInt(lookupFrom(env), "PI_AUTO_CONTINUE_MAX", 9, 1, 20); got != tc.want {
				t.Fatalf("envInt(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestNewStateAppliesEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-continue.json")
	savePersist(path, Persist{Enabled: true, MaxAttempts: 20, DelayMs: 2500})

	state := NewState(Options{
		SettingsPath: path,
		LookupEnv: lookupFrom(map[string]string{
			"PI_AUTO_CONTINUE":          "0",
			"PI_AUTO_CONTINUE_MAX":      "3",
			"PI_AUTO_CONTINUE_DELAY_MS": "10",
		}),
	})
	if state.enabled {
		t.Fatal("PI_AUTO_CONTINUE=0 did not disable the extension")
	}
	if state.maxAttempts != 3 || state.delayMs != 10 {
		t.Fatalf("maxAttempts/delayMs = %d/%d, want 3/10", state.maxAttempts, state.delayMs)
	}
	// The override is not persisted on its own; only a command writes the file.
	if got := loadPersist(path); got != (Persist{true, 20, 2500}) {
		t.Fatalf("file changed without a command: %+v", got)
	}
}

func TestNewStatePersistedDisabledStaysDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-continue.json")
	savePersist(path, Persist{Enabled: false, MaxAttempts: 4, DelayMs: 100})

	state := NewState(Options{SettingsPath: path, LookupEnv: lookupFrom(nil)})
	if state.enabled {
		t.Fatal("persisted enabled=false was ignored")
	}
	if state.maxAttempts != 4 || state.delayMs != 100 {
		t.Fatalf("maxAttempts/delayMs = %d/%d, want 4/100", state.maxAttempts, state.delayMs)
	}
}

func boolPtr(v bool) *bool { return &v }
