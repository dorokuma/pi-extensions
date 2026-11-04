package main

import (
	"os"
	"testing"
)

// helperArgv builds argv for re-executing this test binary as the child
// command; scenario selects the scripted behavior in TestHelperProcess.
func helperArgv(scenario string) []string {
	return []string{os.Args[0], "-test.run=TestHelperProcess", "--", scenario}
}

// TestHelperProcess is not a real test; it is the scripted child command,
// activated only when VEF_HELPER=1 is set (guarding normal `go test` runs).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("VEF_HELPER") != "1" {
		return
	}
	scenario := os.Args[len(os.Args)-1]
	switch scenario {
	case "fail-boom": // exits 3 with verbose go-test-style output
		println("=== RUN TestX")
		println("--- FAIL: TestX (0.01s)")
		println("boom: assertion failed")
		os.Exit(3)
	case "ok-boom": // prints expected text but exits 0 (false green)
		println("boom: nothing failed")
		os.Exit(0)
	case "fail-leak": // matches expect AND reject
		println("boom: leaked secret")
		os.Exit(5)
	case "fail-notests": // nonzero but empty test run; the `2 errors`
		// noise below must NOT be counted as executed test cases.
		println("pkg\t[no test files]")
		println("build failed: 2 errors")
		os.Exit(1)
	}
	os.Exit(0)
}

// runWith wraps run() injecting the helper guard env for children.
func runWith(t *testing.T, args ...string) int {
	t.Helper()
	t.Setenv("VEF_HELPER", "1") // inherited by the child via exec
	return run(append([]string{"verifier-expect-fail"}, args...))
}

func runChildArgv(t *testing.T, scenario string, flags ...string) []string {
	t.Helper()
	return append(flags, append([]string{"--"}, helperArgv(scenario)...)...)
}

func TestAllConditionsPass(t *testing.T) {
	got := runWith(t, runChildArgv(t, "fail-boom",
		"--expect-re", "boom", "--reject-re", "secret", "--min-tests", "1")...)
	if got != 0 {
		t.Fatalf("exit = %d, want 0", got)
	}
}

func TestChildExitZeroFalseGreen(t *testing.T) {
	got := runWith(t, runChildArgv(t, "ok-boom", "--expect-re", "boom")...)
	if got != 1 {
		t.Fatalf("exit = %d, want 1 (false green)", got)
	}
}

func TestExpectReMissPassthrough(t *testing.T) {
	got := runWith(t, runChildArgv(t, "fail-boom", "--expect-re", "zzz-not-found")...)
	if got != 3 {
		t.Fatalf("exit = %d, want passthrough 3", got)
	}
}

func TestRejectReBlocks(t *testing.T) {
	got := runWith(t, runChildArgv(t, "fail-leak", "--expect-re", "boom", "--reject-re", "secret")...)
	if got != 5 {
		t.Fatalf("exit = %d, want passthrough 5", got)
	}
}

func TestMinTests(t *testing.T) {
	if got := runWith(t, runChildArgv(t, "fail-boom", "--expect-re", "FAIL", "--min-tests", "1")...); got != 0 {
		t.Fatalf("met: exit = %d, want 0", got)
	}
	if got := runWith(t, runChildArgv(t, "fail-notests", "--expect-re", "no test", "--min-tests", "1")...); got != 1 {
		t.Fatalf("empty run: exit = %d, want passthrough 1", got)
	}
}

func TestArgErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"missing expect-re", []string{"--", "true"}, 2},
		{"bad regex", []string{"--expect-re", "[", "--", "true"}, 2},
		{"min-tests zero", []string{"--expect-re", "x", "--min-tests", "0", "--", "true"}, 2},
		{"no command", []string{"--expect-re", "x"}, 2},
		{"enoent child", []string{"--expect-re", "x", "--", "/nonexistent/vef-cmd-xyz"}, 127},
		{"enoent bare name", []string{"--expect-re", "x", "--", "vef-no-such-cmd-xyz"}, 127},
	}
	for _, c := range cases {
		if got := runWith(t, c.args...); got != c.want {
			t.Errorf("%s: exit = %d, want %d", c.name, got, c.want)
		}
	}
}
