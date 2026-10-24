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

func TestResolveTerminalContextHerdr(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"HERDR_ENV":           "1",
		"HERDR_SESSION":       "main",
		"HERDR_PANE_ID":       "pane-1",
		"HERDR_WORKSPACE_ID":  "ws-9",
		"HERDR_TAB_ID":        "tab-4",
		"ZELLIJ_SESSION_NAME": "zellij-should-lose",
	}), runFunc("", nil))

	if context.TerminalKind != "herdr" {
		t.Fatalf("TerminalKind = %q, want herdr (herdr wins over zellij)", context.TerminalKind)
	}
	if context.HerdrSession != "main" || context.HerdrPane != "pane-1" ||
		context.HerdrWorkspaceID != "ws-9" || context.HerdrTabID != "tab-4" {
		t.Fatalf("herdr ids = %+v", context)
	}
	// Labels are resolved daemon-side, so both stay empty.
	if context.HerdrWorkspace != "" || context.HerdrTab != "" {
		t.Fatalf("herdr labels = %q/%q, want empty", context.HerdrWorkspace, context.HerdrTab)
	}
}

func TestResolveTerminalContextIgnoresHerdrIdsWithoutEnv(t *testing.T) {
	context := resolveTerminalContext(envFunc(map[string]string{
		"HERDR_PANE_ID": "pane-1",
	}), runFunc("", nil))

	if context.HerdrPane != "" || context.TerminalKind != "" {
		t.Fatalf("herdr without HERDR_ENV = %+v, want empty", context)
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
		"TERN_PANE":    "t-1",
		"TUIOS_ENV":    "1",
		"TUIOS_SOCKET": "",
	}), runFunc("", nil))

	want := map[string]string{"TERN_PANE": "t-1", "TUIOS_ENV": "1"}
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
