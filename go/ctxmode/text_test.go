package ctxmode

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// All twelve tests in stripansi.test.ts, in their original order.
func TestStripANSIOriginal(t *testing.T) {
	const e = "\x1b"
	const st = "\x1b\\" // TS ESC + "\\": ST is ESC followed by backslash.
	plain := "plain text 123 !@# $ % ^ & * ( ) [ ] { } < > ~ ` \\ | é ü 中文 \n\ttab"
	tests := []struct{ name, input, want string }{
		{"RIS", e + "c rest", " rest"},
		{"DECALN", e + "#8out", "out"},
		{"CharacterSet", e + "(Btext", "text"},
		{"PercentAndSaveRestore", e + "%Gx" + e + "7y" + e + "8z", "xyz"},
		{"DCS_ST", e + "P1$r0;1r" + st + "ok", "ok"},
		{"DCS_BEL", "a" + e + "P0;1|payload\x07b", "ab"},
		{"SOS_PM_APC", "a" + e + "X sos text " + st + "b" + e + "^ pm " + st + "c" + e + "_ apc " + st + "d", "abcd"},
		{"DCS_Unterminated", "head " + e + "P1;2|payload that never ends", "head "},
		{"OSC_BEL_ST", "a" + e + "]0;title\x07b", "ab"},
		{"CSI", "a" + e + "[31;1mred" + e + "[0m" + e + "[2J" + e + "[1;1Hb", "aredb"},
		{"Plain", plain, plain},
		{"Mixed", e + "[?25l" + e + "c" + e + "]8;;http://x\x07link" + e + "]8;;\x07" + e + "(B" + e + "[1mBOLD" + e + "[0m", "linkBOLD"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripANSI(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if tc.name == "OSC_BEL_ST" && StripANSI("a"+e+"]2;t"+st+"b") != "ab" {
				t.Fatal("OSC ST")
			}
		})
	}
}

// ctxmode.test.ts:98.
func TestCompressToolTextHeadTail(t *testing.T) {
	text := "HEAD_MARKER\n" + strings.Repeat("x", 60000) + "\n(exited with code 7)"
	out := CompressToolText(text)
	for _, sub := range []string{"HEAD_MARKER", "(exited with code 7)", "已截断"} {
		requireContains(t, out, sub)
	}
	if len(out) >= len(text) {
		t.Fatal("must compress")
	}
}

// ctxmode.test.ts:481.
func TestTimeoutForToolFetch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		min  int
	}{
		{"ctx_kb", map[string]any{"action": "fetch"}, 180000},
		{"ctx_kb", map[string]any{"action": "fetch", "timeout_ms": 200000}, 230000},
		{"ctx_run", map[string]any{"action": "run_task", "timeout_ms": 400000}, 430000},
	} {
		if got := timeoutForTool(tc.name, tc.args); got < tc.min {
			t.Fatalf("got %d, want >=%d", got, tc.min)
		}
	}
}

// ctxmode.test.ts:492.
func TestResolveRoleEnvOriginal(t *testing.T) {
	ro := ResolveRoleEnv([]string{"pi", "--name", "role-verifier-a1b2c3d4"}, map[string]string{})
	if ro["CTXMODE_ROLE"] != "verifier" || ro["CTXMODE_READONLY"] != "1" {
		t.Fatal(ro)
	}
	wk := ResolveRoleEnv([]string{"pi", "--name=role-worker-1234abcd"}, map[string]string{})
	if wk["CTXMODE_ROLE"] != "worker" || wk["CTXMODE_READONLY"] != "" {
		t.Fatal(wk)
	}
	kept := ResolveRoleEnv([]string{"pi", "--name", "role-verifier-a1b2c3d4"}, map[string]string{"CTXMODE_ROLE": "worker"})
	if kept["CTXMODE_ROLE"] != "worker" || kept["CTXMODE_READONLY"] != "" {
		t.Fatal(kept)
	}
	// P2-2: TS `if (baseEnv.CTXMODE_ROLE) return baseEnv` treats "" as unset, so a
	// present-but-empty CTXMODE_ROLE still resolves the role from --name.
	empty := ResolveRoleEnv([]string{"pi", "--name", "role-scout-1234abcd"}, map[string]string{"CTXMODE_ROLE": ""})
	if empty["CTXMODE_ROLE"] != "scout" || empty["CTXMODE_READONLY"] != "1" {
		t.Fatal(empty)
	}
}

// Extra boundary coverage: one case for each budget branch, including the
// source's minimum head/tail overlap for very small line caps.
func TestCompressionBoundaries(t *testing.T) {
	t.Setenv("CTXMODE_OUTPUT_LINES", "80")
	t.Setenv("CTXMODE_OUTPUT_CHARS", "50000")
	if got := CompressToolText("a \t\r\n\n\n\n\n\nb\t"); got != "a\n\n\nb" {
		t.Fatalf("cleanup %q", got)
	}
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "line"
	}
	out := CompressToolText(strings.Join(lines, "\n"))
	requireContains(t, out, "省略 21 行")
	t.Setenv("CTXMODE_OUTPUT_LINES", "1")
	requireContains(t, CompressToolText("a\nb"), "省略 -53 行")
	t.Setenv("CTXMODE_OUTPUT_LINES", "500")
	t.Setenv("CTXMODE_OUTPUT_CHARS", "6")
	out = CompressToolText("aaa😀bbb😀z")
	if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("split surrogate: %q", out)
	}
	if !strings.HasPrefix(out, "aaa\n") || !strings.HasSuffix(out, "\nz") {
		t.Fatalf("UTF16 clamp %q", out)
	}
	t.Setenv("CTXMODE_OUTPUT_CHARS", "4")
	if got := CompressToolText("😀😀"); got != "😀😀" {
		t.Fatalf("exact code-unit boundary %q (%d)", got, len(utf16.Encode([]rune(got))))
	}
}
func TestTimeoutAndRoleBoundaries(t *testing.T) {
	t.Setenv("CTXMODE_REQUEST_TIMEOUT_MS", "10")
	for _, tc := range []struct {
		name, action string
		n, want      int
	}{
		{"ctx_run", "run_task", 0, 330000}, {"ctx_run", "execute", 0, 150000}, {"ctx_bg", "wait", 0, 75000}, {"ctx_git", "status", 0, 10}, {"ctx_run", "execute", 3600001, 3630001}, {"ctx_kb", "fetch", 9999999, 3660000},
	} {
		if got := timeoutForTool(tc.name, map[string]any{"action": tc.action, "timeout_ms": tc.n}); got != tc.want {
			t.Fatalf("%+v got %d", tc, got)
		}
	}
	base := map[string]string{"CTXMODE_READONLY": "0"}
	got := ResolveRoleEnv([]string{"--name=role-REVIEWER-AB12CD34"}, base)
	if got["CTXMODE_ROLE"] != "reviewer" || got["CTXMODE_READONLY"] != "0" || len(base) != 1 {
		t.Fatal(got, base)
	}
	if got := ResolveRoleEnv([]string{"--name", "role-scout-invalid"}, map[string]string{}); len(got) != 0 {
		t.Fatal(got)
	}
}

// P2-1: the numeric knobs share envNumber, which mirrors
// `Number(process.env.X || DEFAULT)`: "0" is a number and stays 0, while only an
// empty value (unset and "" are the same to os.Getenv) or a NaN-ish value falls
// back to DEFAULT.
func TestEnvNumberZeroAndFallbackSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int
	}{
		{"empty", "", 7}, {"zero", "0", 0}, {"padded zero", " 0 ", 0}, {"negative", "-3", -3},
		{"exponent", "1e3", 1000}, {"garbage", "abc", 7}, {"NaN", "NaN", 7}, {"Infinity", "Infinity", 7},
	} {
		t.Setenv("CTXMODE_TEST_NUMBER", tc.value)
		if got := envNumber("CTXMODE_TEST_NUMBER", 7); got != tc.want {
			t.Fatalf("%s (%q) got %d, want %d", tc.name, tc.value, got, tc.want)
		}
	}
	// Knob level: "0" must reach the cap, not be replaced by the default.
	t.Setenv("CTXMODE_OUTPUT_CHARS", "0")
	if got := CompressToolText("aaaa"); !strings.Contains(got, "输出达上限 0 字") {
		t.Fatalf("char cap 0 not honoured: %q", got)
	}
	t.Setenv("CTXMODE_OUTPUT_CHARS", "50000")
	t.Setenv("CTXMODE_OUTPUT_LINES", "0")
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = "line"
	}
	// Line cap 500 would keep 60 lines verbatim; cap 0 means 40 head / 15 tail.
	requireContains(t, CompressToolText(strings.Join(lines, "\n")), "省略 5 行")
}

// R3 P2: a zero cap must still truncate. TS `body.length <= 0` is false for a
// non-empty body, so the head/tail slice collapses to the marker alone, and a
// zero line cap keeps the source's minimum 40 head / 15 tail window.
func TestCompressToolTextZeroCaps(t *testing.T) {
	t.Setenv("CTXMODE_OUTPUT_LINES", "500")
	t.Setenv("CTXMODE_OUTPUT_CHARS", "0")
	if got := CompressToolText("hello world"); got != "\n... (输出达上限 0 字，已截断) ...\n" {
		t.Fatalf("char cap 0 => %q", got)
	}
	// A negative cap must not index out of the encoded body; the marker keeps the
	// source's raw value (only the offsets are clamped to zero).
	t.Setenv("CTXMODE_OUTPUT_CHARS", "-3")
	if got := CompressToolText("hello world"); got != "\n... (输出达上限 -3 字，已截断) ...\n" {
		t.Fatalf("negative char cap => %q", got)
	}
	t.Setenv("CTXMODE_OUTPUT_CHARS", "50000")
	t.Setenv("CTXMODE_OUTPUT_LINES", "0")
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = "line"
	}
	got := CompressToolText(strings.Join(lines, "\n"))
	if strings.Count(got, "line") != 55 || !strings.Contains(got, "省略 5 行") {
		t.Fatalf("line cap 0 => %q", got)
	}
}
