package moshihooks

import (
	"reflect"
	"testing"
	"time"
)

func envFunc(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func runFunc(output string, calls *[][]string) func(string, []string, time.Duration) string {
	return func(command string, args []string, _ time.Duration) string {
		if calls != nil {
			*calls = append(*calls, append([]string{command}, args...))
		}
		return output
	}
}

func TestResolveTerminalContextTmux(t *testing.T) {
	var calls [][]string
	context := resolveTerminalContext(envFunc(map[string]string{
		"TMUX":      "/tmp/tmux-0/default,4242,0",
		"TMUX_PANE": "%3",
	}), runFunc("work\t2\n", &calls))

	if context.TerminalKind != "tmux" {
		t.Fatalf("TerminalKind = %q, want tmux", context.TerminalKind)
	}
	if context.TmuxSession != "work" || context.TmuxWindow != "2" {
		t.Fatalf("tmux session/window = %q/%q, want work/2", context.TmuxSession, context.TmuxWindow)
	}
	if context.TmuxPane != "%3" {
		t.Fatalf("TmuxPane = %q, want %%3", context.TmuxPane)
	}
	if context.TmuxSocket != "/tmp/tmux-0/default" {
		t.Fatalf("TmuxSocket = %q, want /tmp/tmux-0/default", context.TmuxSocket)
	}
	want := []string{"tmux", "display-message", "-p", "-t", "%3", "#S\t#I"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("tmux probe = %v, want [%v]", calls, want)
	}
}

func TestResolveTerminalContextTmuxProbeFailure(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"TMUX":      "/tmp/tmux-0/default,4242,0",
		"TMUX_PANE": "%3",
	}), runFunc("", nil))

	if context.TmuxSession != "" || context.TmuxWindow != "" {
		t.Fatalf("tmux session/window = %q/%q, want empty", context.TmuxSession, context.TmuxWindow)
	}
	// No mux session name means no terminal kind: the daemon resolves the pane
	// from tmuxPane/tmuxSocket.
	if context.TerminalKind != "" {
		t.Fatalf("TerminalKind = %q, want empty", context.TerminalKind)
	}
	var calls [][]string
	_ = resolveTerminalContext(envFunc(map[string]string{"TMUX": "x,1,0"}), runFunc("work\t1", &calls))
	want := []string{"tmux", "display-message", "-p", "#S\t#I"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("tmux probe without pane = %v, want [%v]", calls, want)
	}
}

// herdr-core (hc) stamps HC_TERMINAL_ID / HC_TERMINAL_SOCKET; a pig session
// inside a hc terminal reports a herdr-shaped identity.
func TestResolveTerminalContextHc(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"HC_TERMINAL_ID":     "t12t",
		"HC_TERMINAL_SOCKET": "/run/user/0/herdr-core/default.sock",
		"HC_SCOPE":           "scope-1",
	}), runFunc("", nil))

	if context.TerminalKind != "herdr" {
		t.Fatalf("TerminalKind = %q, want herdr", context.TerminalKind)
	}
	if context.HerdrPane != "t12t" {
		t.Fatalf("HerdrPane = %q, want t12t (HC_TERMINAL_ID)", context.HerdrPane)
	}
	if context.HerdrSession != "default" {
		t.Fatalf("HerdrSession = %q, want default (session from socket path)", context.HerdrSession)
	}
	// The hc variables ride muxEnv verbatim (TS muxEnvKeys parity); HC_SCOPE is
	// not in the registry list and must not leak.
	if context.MuxEnv["HC_TERMINAL_ID"] != "t12t" ||
		context.MuxEnv["HC_TERMINAL_SOCKET"] != "/run/user/0/herdr-core/default.sock" {
		t.Fatalf("MuxEnv = %v, want HC_TERMINAL_ID + HC_TERMINAL_SOCKET", context.MuxEnv)
	}
	if _, ok := context.MuxEnv["HC_SCOPE"]; ok {
		t.Fatalf("MuxEnv leaked HC_SCOPE: %v", context.MuxEnv)
	}
	// A socket without a .sock basename yields no session name.
	noSession := resolveTerminalContext(envFunc(map[string]string{
		"HC_TERMINAL_ID":     "t12t",
		"HC_TERMINAL_SOCKET": "/run/user/0/herdr-core/not-a-sock",
	}), runFunc("", nil))
	if noSession.HerdrSession != "" || noSession.TerminalKind != "herdr" {
		t.Fatalf("hc without session socket = %+v, want empty session + herdr kind", noSession)
	}
}

// tmux 探到 session 时压过 hc 终端：terminalKind 保持 tmux，herdr 字段留空。
func TestResolveTerminalContextTmuxBeatsHc(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"TMUX":           "/tmp/tmux-0/default,4242,0",
		"TMUX_PANE":      "%7",
		"HC_TERMINAL_ID": "t12t",
		"ZELLIJ":         "0",
	}), runFunc("work\t1\n", nil))
	if context.TerminalKind != "tmux" {
		t.Fatalf("TerminalKind = %q, want tmux (tmux > hc > zellij)", context.TerminalKind)
	}
	// herdrPane 仍由 HC_TERMINAL_ID 填入（上下文字段独立收集，不随 kind 清零），
	// 但 muxEnv 仍携带 hc 变量、tmux 字段完整。
	if context.HerdrPane != "t12t" {
		t.Fatalf("HerdrPane = %q, want t12t (field fill is independent of kind)", context.HerdrPane)
	}
	if context.TmuxSession != "work" || context.TmuxPane != "%7" {
		t.Fatalf("tmux fields = %q/%q, want work/%%7", context.TmuxSession, context.TmuxPane)
	}
}

// hc 变量缺位时 zellij 仍兜底：hc > zellij 的优先级只对 hc 命中生效。
func TestResolveTerminalContextHcBeatsZellij(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"HC_TERMINAL_ID": "t12t",
		"ZELLIJ":         "0",
	}), runFunc("", nil))
	if context.TerminalKind != "herdr" {
		t.Fatalf("TerminalKind = %q, want herdr (hc > zellij)", context.TerminalKind)
	}
}

// 经典 HERDR_* 变量不再被读取：HERDR_ENV/HERDR_PANE_ID/HERDR_SESSION 在
// 没有 HC_TERMINAL_ID 的情况下不产生 herdr 上下文，Herdr* 字段恒为空。
func TestResolveTerminalContextIgnoresLegacyHerdr(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_PANE_ID":      "42",
		"HERDR_SESSION":      "legacy",
		"HERDR_WORKSPACE_ID": "ws-9",
		"HERDR_TAB_ID":       "tab-3",
	}), runFunc("", nil))
	if context.TerminalKind != "" {
		t.Fatalf("TerminalKind = %q, want empty (legacy HERDR_* ignored)", context.TerminalKind)
	}
	if context.HerdrPane != "" || context.HerdrSession != "" ||
		context.HerdrWorkspaceID != "" || context.HerdrWorkspace != "" ||
		context.HerdrTabID != "" || context.HerdrTab != "" {
		t.Fatalf("Herdr fields must all be empty, got %+v", context)
	}
	if len(context.MuxEnv) != 0 {
		t.Fatalf("MuxEnv must not pick up HERDR_* keys, got %v", context.MuxEnv)
	}
}

func TestResolveTerminalContextZellij(t *testing.T) {
	withSession := resolveTerminalContext(envFunc(map[string]string{
		"ZELLIJ_SESSION_NAME": "z",
		"ZELLIJ_PANE_ID":      "3",
	}), runFunc("", nil))
	if withSession.TerminalKind != "zellij" || withSession.ZellijSession != "z" || withSession.ZellijPane != "3" {
		t.Fatalf("zellij context = %+v", withSession)
	}

	envOnly := resolveTerminalContext(envFunc(map[string]string{"ZELLIJ": "0"}), runFunc("", nil))
	if envOnly.TerminalKind != "zellij" {
		t.Fatalf("TerminalKind = %q, want zellij from ZELLIJ alone", envOnly.TerminalKind)
	}
	if envOnly.ZellijSession != "" || envOnly.ZellijPane != "" {
		t.Fatalf("zellij ids = %q/%q, want empty", envOnly.ZellijSession, envOnly.ZellijPane)
	}
}

func TestResolveTerminalContextMuxEnv(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"TERN_PANE":          "t-1",
		"TUIOS_ENV":          "1",
		"TUIOS_SOCKET":       "",
		"HC_TERMINAL_ID":     "t9",
		"HC_TERMINAL_SOCKET": "/run/user/0/herdr-core/default.sock",
	}), runFunc("", nil))
	want := map[string]string{
		"TERN_PANE":          "t-1",
		"TUIOS_ENV":          "1",
		"HC_TERMINAL_ID":     "t9",
		"HC_TERMINAL_SOCKET": "/run/user/0/herdr-core/default.sock",
	}
	if !reflect.DeepEqual(context.MuxEnv, want) {
		t.Fatalf("MuxEnv = %v, want %v", context.MuxEnv, want)
	}
}

func TestTmuxSocketFromEnv(t *testing.T) {
	tests := map[string]string{
		"":                    "",
		"no-comma":            "",
		",1234,0":             "",
		"/tmp/tmux-0/def,1,0": "/tmp/tmux-0/def",
	}
	for in, want := range tests {
		if got := tmuxSocketFromEnv(in); got != want {
			t.Fatalf("tmuxSocketFromEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectNameFromCwd(t *testing.T) {
	tests := map[string]string{
		"":               "",
		"/":              "",
		"/work/moshi/":   "moshi",
		"/work/moshi///": "moshi",
		"moshi":          "moshi",
		`C:\work\moshi`:  "moshi",
		`C:\work\moshi\`: "moshi",
	}
	for cwd, want := range tests {
		if got := projectNameFromCwd(cwd); got != want {
			t.Fatalf("projectNameFromCwd(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestProjectNameForCwd(t *testing.T) {
	cases := []struct {
		name     string
		terminal TerminalContext
		want     string
	}{
		{"tmux wins", TerminalContext{TmuxSession: "t", HerdrSession: "h", ZellijSession: "z"}, "t"},
		{"herdr next", TerminalContext{HerdrSession: "h", ZellijSession: "z"}, "h"},
		{"zellij next", TerminalContext{ZellijSession: "z"}, "z"},
		{"cwd fallback", TerminalContext{}, "moshi"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := projectNameForCwd(test.terminal, "/work/moshi"); got != test.want {
				t.Fatalf("projectNameForCwd = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTerminalContextIsResolvedOnceAndResettable(t *testing.T) {
	t.Setenv("TMUX_PANE", "%1")
	ResetTerminalContextForTest()
	first := TerminalContextForProcess()
	if first.TmuxPane != "%1" {
		t.Fatalf("TmuxPane = %q, want %%1", first.TmuxPane)
	}

	t.Setenv("TMUX_PANE", "%2")
	if cached := TerminalContextForProcess(); cached.TmuxPane != "%1" {
		t.Fatalf("cached TmuxPane = %q, want %%1 (resolution must be cached)", cached.TmuxPane)
	}

	ResetTerminalContextForTest()
	if refreshed := TerminalContextForProcess(); refreshed.TmuxPane != "%2" {
		t.Fatalf("refreshed TmuxPane = %q, want %%2", refreshed.TmuxPane)
	}
	ResetTerminalContextForTest()
}
