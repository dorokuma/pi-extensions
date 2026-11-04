package jevgate_test

// Cross-extension Packed Cell regression (MF-2): PIG_HARNESS_ARGV_FILE is
// process-level state shared by every member of a packed cell. An earlier
// implementation deleted the variable inside each extension's argv bridge, so
// whichever extension resolved its identity first blinded every sibling that
// read it later (e.g. at EventSessionStart). These tests pin both init orders:
// whichever extension runs first must leave the variable readable, and both
// must still hit the ["--name","role-worker"] launch marker inside it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	excludetoolsguard "github.com/dorokuma/pi-extensions/go/exclude-tools-guard"
	jevgate "github.com/dorokuma/pi-extensions/go/jev-gate"
)

// packedCellArgvFixture installs a PIG_HARNESS_ARGV_FILE carrying the
// ["--name","role-worker"] launch marker and clears every other identity input
// so the only reason either extension can return "subagent" is that file.
func packedCellArgvFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "argv.json")
	if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
		t.Fatalf("write argv file: %v", err)
	}
	// Clear the real packed-cell environment this test process may run under
	// (CTXMODE_ROLE/PIG_EXT_* are set inside a live cell): any of them would
	// fail-closed for a different reason and hide what is being measured.
	for _, key := range os.Environ() {
		name, _, _ := strings.Cut(key, "=")
		switch {
		case strings.HasPrefix(name, "PIG_EXT_"):
			t.Setenv(name, "")
		case name == "CTXMODE_ROLE" || name == "PI_SUBAGENT" || name == "PI_ROLE" || name == "HERDR_PANE_ID":
			t.Setenv(name, "")
		}
	}
	t.Setenv("PIG_HARNESS_ARGV_FILE", path)
	return path
}

// assertBothHitArgvMarker asserts both extensions resolved as subagent BECAUSE
// of the argv bridge marker (not for any unrelated fail-closed reason), and
// that PIG_HARNESS_ARGV_FILE stayed readable afterwards.
func assertBothHitArgvMarker(t *testing.T, path, jevReason, guardReason string) {
	t.Helper()
	if got := os.Getenv("PIG_HARNESS_ARGV_FILE"); got != path {
		t.Fatalf("PIG_HARNESS_ARGV_FILE must stay readable for packed cell siblings, got %q", got)
	}
	if !strings.Contains(jevReason, "launch marker: argv has --name role-") {
		t.Fatalf("jev-gate reason = %q, want the argv bridge launch marker", jevReason)
	}
	if !strings.Contains(guardReason, "launch marker: argv has --name role-") {
		t.Fatalf("exclude-tools-guard reason = %q, want the argv bridge launch marker", guardReason)
	}
	// Negative-marker only: the file must never be able to elevate a session —
	// both verdicts went subagent, verified by the reasons above.
}

func TestPackedCell_ArgvFileSharedAcrossExtensions_JevFirst(t *testing.T) {
	path := packedCellArgvFixture(t)

	// Order 1: jev-gate initialises first (factory + identity), then
	// exclude-tools-guard resolves later (its EventSessionStart timing).
	extension := jevgate.Extension()
	if extension == nil || extension.Name() != "jev-gate" {
		t.Fatalf("jev-gate Extension() = %v", extension)
	}
	_, jevReason := jevgate.ResolveAgentIdentity()

	guardSub, guardReason := excludetoolsguard.ResolveAgentIdentity()
	if !guardSub {
		t.Fatal("exclude-tools-guard must resolve subagent via the shared argv file")
	}
	args, err := excludetoolsguard.LoadEffectiveArgs(os.Getenv, []string{"runner"})
	if err != nil {
		t.Fatalf("exclude-tools-guard LoadEffectiveArgs: %v", err)
	}
	if strings.Join(args, " ") != "runner --name role-worker" {
		t.Fatalf("exclude-tools-guard effective args = %v, must still read the shared file", args)
	}

	assertBothHitArgvMarker(t, path, jevReason, guardReason)
}

func TestPackedCell_ArgvFileSharedAcrossExtensions_GuardFirst(t *testing.T) {
	path := packedCellArgvFixture(t)

	// Order 2: exclude-tools-guard first (identity + direct bridge read),
	// then jev-gate — the reverse of order 1.
	guardSub, guardReason := excludetoolsguard.ResolveAgentIdentity()
	if !guardSub {
		t.Fatal("exclude-tools-guard must resolve subagent via the shared argv file")
	}
	args, err := excludetoolsguard.LoadEffectiveArgs(os.Getenv, []string{"runner"})
	if err != nil {
		t.Fatalf("exclude-tools-guard LoadEffectiveArgs: %v", err)
	}
	if strings.Join(args, " ") != "runner --name role-worker" {
		t.Fatalf("exclude-tools-guard effective args = %v, must still read the shared file", args)
	}

	jevSub, jevReason := jevgate.ResolveAgentIdentity()
	if !jevSub {
		t.Fatal("jev-gate must resolve subagent via the shared argv file")
	}

	assertBothHitArgvMarker(t, path, jevReason, guardReason)
}
