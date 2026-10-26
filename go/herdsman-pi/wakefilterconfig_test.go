package herdsmanpi

// Port of test/unit/wake-filter-config.test.ts (8 cases).
//
// The TS drives `process.env.HERDSMAN_HOME` plus a per-test config.yaml; the Go
// port uses t.Setenv and the same temp-home layout, and passes the environment
// override map exactly like `loadWakeFilterConfig({ ... })` does.

import (
	"os"
	"path/filepath"
	"testing"
)

func useHomeWithConfig(t *testing.T, contents *string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDSMAN_HOME", dir)
	if contents != nil {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(*contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func configEquals(t *testing.T, got WakeFilterConfig, enabled bool, patterns []string) {
	t.Helper()
	if got.Enabled != enabled {
		t.Fatalf("enabled=%v want %v", got.Enabled, enabled)
	}
	if len(got.ExtraPatterns) != len(patterns) {
		t.Fatalf("patterns=%v want %v", got.ExtraPatterns, patterns)
	}
	for index := range patterns {
		if got.ExtraPatterns[index] != patterns[index] {
			t.Fatalf("patterns=%v want %v", got.ExtraPatterns, patterns)
		}
	}
}

func TestWakeFilterDefaultsWhenConfigAbsent(t *testing.T) {
	useHomeWithConfig(t, nil)
	configEquals(t, LoadWakeFilterConfig(map[string]*string{}), true, []string{})
}

func TestWakeFilterDefaultsWhenSectionAbsent(t *testing.T) {
	contents := "runtime:\n  db_path: data/state.db\n"
	useHomeWithConfig(t, &contents)
	configEquals(t, LoadWakeFilterConfig(map[string]*string{}), true, []string{})
}

func TestWakeFilterParsesDocumentedSubset(t *testing.T) {
	contents := "# leading comment\n" +
		"observability:\n" +
		"  telemetry:\n" +
		"    max_excerpt_bytes: 1024\n" +
		"wake:\n" +
		"  filter_upstream_errors: false # inline comment\n" +
		"  extra_upstream_error_patterns:\n" +
		"    - \"checkpoint-stall\"\n" +
		"    - /foo\\s+bar/i\n" +
		"runtime:\n" +
		"  db_path: data/state.db\n"
	useHomeWithConfig(t, &contents)
	configEquals(t, LoadWakeFilterConfig(map[string]*string{}), false,
		[]string{"checkpoint-stall", "/foo\\s+bar/i"})
}

func TestWakeFilterAcceptsExplicitTrueAndEmptyList(t *testing.T) {
	contents := "wake:\n  filter_upstream_errors: true\n  extra_upstream_error_patterns: []\n"
	useHomeWithConfig(t, &contents)
	configEquals(t, LoadWakeFilterConfig(map[string]*string{}), true, []string{})
}

func TestWakeFilterFallsBackOnMalformedYAML(t *testing.T) {
	for _, broken := range []string{
		"wake:\n      filter_upstream_errors: false\n",
		"wake:\n  filter_upstream_errors: maybe\n",
		"wake:\n  extra_upstream_error_patterns: [a, b]\n",
		"wake:\n  extra_upstream_error_patterns:\n\t- tabbed\n",
	} {
		contents := broken
		useHomeWithConfig(t, &contents)
		configEquals(t, LoadWakeFilterConfig(map[string]*string{}), true, []string{})
	}
}

func TestWakeFilterEnvironmentOverridesFile(t *testing.T) {
	contents := "wake:\n  filter_upstream_errors: true\n  extra_upstream_error_patterns:\n    - file-pattern\n"
	useHomeWithConfig(t, &contents)

	comma := "env-a,env-b"
	off := "0"
	configEquals(t, LoadWakeFilterConfig(map[string]*string{
		"HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS": &comma,
		"HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS":        &off,
	}), false, []string{"env-a", "env-b"})

	newline := "env-newline-a\nenv-newline-b"
	on := "1"
	configEquals(t, LoadWakeFilterConfig(map[string]*string{
		"HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS": &newline,
		"HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS":        &on,
	}), true, []string{"env-newline-a", "env-newline-b"})
}

func TestWakeFilterIgnoresUnsupportedEnvironmentValues(t *testing.T) {
	contents := "wake:\n  filter_upstream_errors: false\n"
	useHomeWithConfig(t, &contents)
	sometimes := "sometimes"
	configEquals(t, LoadWakeFilterConfig(map[string]*string{
		"HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS": &sometimes,
	}), false, []string{})
}

func TestWakeFilterEnvironmentCanForceFilterOff(t *testing.T) {
	contents := "wake:\n  filter_upstream_errors: true\n"
	useHomeWithConfig(t, &contents)
	off := "false"
	configEquals(t, LoadWakeFilterConfig(map[string]*string{
		"HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS": &off,
	}), false, []string{})
	on := "true"
	configEquals(t, LoadWakeFilterConfig(map[string]*string{
		"HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS": &on,
	}), true, []string{})
}
