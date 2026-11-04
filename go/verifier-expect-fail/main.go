// Command verifier-expect-fail is a purely mechanical wrapper for
// double-snapshot diffing and counterexample probing: it runs a command that
// is *expected* to fail and passes only when all four conditions hold:
//
//  1. the child actually started and exited with a non-zero code
//     (signal termination counts as 128+sig);
//  2. the merged child output (stdout+stderr) matches --expect-re;
//  3. either --reject-re was not given, or the output does not match it;
//  4. either --min-tests was not given, or the mechanically parsed executed
//     test-case count is >= N.
//
// The child runs as raw argv — never through a shell — with stdout/stderr
// tee'd to the wrapper's own streams in real time while being captured into a
// mutex-protected merged buffer.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Re-used regexes for the mechanical test-count parser (condition 4).
var (
	// Go: `=== RUN <name>` (verbose mode) ...
	reGoRun = regexp.MustCompile(`(?m)^=== RUN\s+\S+`)
	// ... and `--- FAIL:/PASS: <name>` (non-verbose `go test` still prints
	// `--- FAIL: TestX` for failing cases; all-green or build-failed runs
	// print neither, so `[no test files]` yields 0). max() of the two
	// counts approximates executed cases.
	reGoRes = regexp.MustCompile(`(?m)^---\s+(?:FAIL|PASS):\s+\S+`)
	// Python unittest: `Ran 5 tests`
	reUnittest = regexp.MustCompile(`(?m)^\s*Ran (\d+) tests?\b`)
	// pytest bottom summary line: `== 3 failed, 5 passed in 0.01s ==`.
	// Anchored to the `in <secs>s` trailer so stray lines like
	// `build failed: 2 errors` never feed the counter.
	rePytestSum = regexp.MustCompile(`(?m)^=+\s+.*\bin\s+\d+(?:\.\d+)?s\b.*=+\s*$`)
	// cargo test bottom summary: `test result: FAILED. 5 passed; 2 failed;`
	reCargoSum = regexp.MustCompile(`(?m)^test result:\s+(?:ok|FAILED)\.\s+(\d+)\s+passed;\s+(\d+)\s+failed\b`)
	// Item inside an anchored pytest summary line: `N passed|failed|error(s)`.
	reCountItem = regexp.MustCompile(`(\d+)\s+(?:passed|failed|errors?)\b`)
)

// syncBuffer is a mutex-protected buffer collecting merged child output
// written by the stdout and stderr copy goroutines concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func main() {
	os.Exit(run(os.Args))
}

// run implements the full CLI; returns the process exit code.
func run(argv []string) int {
	fs := flag.NewFlagSet("verifier-expect-fail", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var expectRe, rejectRe string
	var minTests int
	fs.StringVar(&expectRe, "expect-re", "", "regular expression the merged child output MUST match (required)")
	fs.StringVar(&rejectRe, "reject-re", "", "regular expression the merged child output MUST NOT match")
	fs.IntVar(&minTests, "min-tests", 0, "minimum number of executed test cases parsed from output")
	if err := fs.Parse(argv[1:]); err != nil {
		return 2 // flag package already printed the error
	}

	if strings.TrimSpace(expectRe) == "" {
		fmt.Fprintln(os.Stderr, "verifier-expect-fail: --expect-re is required and must be non-empty")
		return 2
	}
	expect, err := regexp.Compile(expectRe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifier-expect-fail: invalid --expect-re: %v\n", err)
		return 2
	}
	var reject *regexp.Regexp
	if rejectRe != "" {
		reject, err = regexp.Compile(rejectRe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verifier-expect-fail: invalid --reject-re: %v\n", err)
			return 2
		}
	}
	minTestsSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "min-tests" {
			minTestsSet = true
		}
	})
	if minTestsSet && minTests < 1 {
		fmt.Fprintf(os.Stderr, "verifier-expect-fail: --min-tests must be >= 1, got %d\n", minTests)
		return 2
	}

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "verifier-expect-fail: missing command after --")
		return 2
	}

	started, exit, out := runChild(cmdArgs)
	if !started {
		return exit // startup failure already reported; 127 ENOENT / 126 other
	}

	match := expect.Find(out)
	conds := []struct {
		name string
		ok   bool
	}{
		{"inner-exit-nonzero", exit != 0},
		{"expect-re", match != nil},
		{"reject-re", reject == nil || !reject.Match(out)},
	}
	tests := 0
	if minTestsSet {
		tests = parseTestCount(out)
		conds = append(conds, struct {
			name string
			ok   bool
		}{fmt.Sprintf("min-tests(%d>=%d)", tests, minTests), tests >= minTests})
	}

	var failed []string
	for _, c := range conds {
		if !c.ok {
			failed = append(failed, c.name)
		}
	}
	if len(failed) == 0 {
		snip := ""
		if match != nil {
			snip = " match=" + shortSnippet(string(match))
		}
		extra := ""
		if minTestsSet {
			extra = fmt.Sprintf(" tests=%d", tests)
		}
		fmt.Fprintf(os.Stderr, "verifier-expect-fail: PASS inner_exit=%d%s%s\n", exit, snip, extra)
		return 0
	}
	fmt.Fprintf(os.Stderr, "verifier-expect-fail: FAIL inner_exit=%d failed=[%s]\n", exit, strings.Join(failed, ","))
	if exit != 0 {
		return exit // transparently propagate the real non-zero child code
	}
	return 1 // child exited 0 unexpectedly (false green)
}

// runChild executes cmdArgs as raw argv (no shell), inheriting env/cwd/stdin.
// stdout/stderr go through io.MultiWriter to both our own streams (live tee)
// and the mutex-protected merged buffer; os/exec manages the copy goroutines
// internally and Wait waits for them, so no piped tail can be lost.
// Returns started=false when the child never launched (exit: 127 ENOENT,
// 126 for other startup failures).
func runChild(cmdArgs []string) (started bool, exit int, out []byte) {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin

	var merged syncBuffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &merged)
	cmd.Stderr = io.MultiWriter(os.Stderr, &merged)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "verifier-expect-fail: cannot start %q: %v\n", cmdArgs[0], err)
		code := 126
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			code = 127 // ENOENT: command not found, distinct from a failed run
		}
		return false, code, nil
	}
	err := cmd.Wait()

	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
		// code stays 0
	case errors.As(err, &ee):
		code = ee.ExitCode() // -1 when terminated by signal
		if code < 0 {
			sig := 1
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				sig = int(ws.Signal())
			}
			code = 128 + sig // signal termination → conventional 128+sig
		}
	default:
		code = 126
	}
	return true, code, merged.Bytes()
}

// parseTestCount mechanically extracts the executed test-case count from
// common runner output. Returns 0 when nothing is recognized — e.g.
// `[no test files]` or `no tests to run` — so --min-tests catches empty runs.
func parseTestCount(out []byte) int {
	s := string(out)
	best := 0
	if n := len(reGoRun.FindAllStringIndex(s, -1)); n > best {
		best = n
	}
	if n := len(reGoRes.FindAllStringIndex(s, -1)); n > best {
		best = n
	}
	for _, m := range reUnittest.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > best {
			best = n
		}
	}
	// Anchored bottom summaries only — a bare `N passed/failed/error(s)`
	// on an arbitrary line (e.g. `build failed: 2 errors`) must NOT count.
	for _, m := range rePytestSum.FindAllString(s, -1) {
		sum := 0
		for _, it := range reCountItem.FindAllStringSubmatch(m, -1) {
			if n, err := strconv.Atoi(it[1]); err == nil {
				sum += n
			}
		}
		if sum > best {
			best = sum
		}
	}
	for _, m := range reCargoSum.FindAllStringSubmatch(s, -1) {
		sum := 0
		for _, g := range m[1:3] {
			if n, err := strconv.Atoi(g); err == nil {
				sum += n
			}
		}
		if sum > best {
			best = sum
		}
	}
	return best
}

func shortSnippet(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	if r := []rune(s); len(r) > 60 { // runes, not bytes: keep valid UTF-8
		s = string(r[:57]) + "..."
	}
	return fmt.Sprintf("%q", s)
}
