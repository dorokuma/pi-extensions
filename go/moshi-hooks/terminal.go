package moshihooks

// Terminal and multiplexer context detection, ported from moshi-hooks.ts
// (L108-213).
//
// Panes are never resolved through a herdr / kitty / wezterm CLI here: the
// daemon resolves labels from the ids and environment variables below. The
// only process spawned is `tmux display-message` for the session/window names,
// exactly as the TS does.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tmuxProbeTimeout caps the `tmux display-message` call (TS: runForOutput
// timeout 200, L153).
const tmuxProbeTimeout = 200 * time.Millisecond

// TerminalContext is the pane identity attached to every envelope
// (TS: interface TerminalContext, L108).
type TerminalContext struct {
	TerminalKind     string
	TmuxSession      string
	TmuxWindow       string
	TmuxPane         string
	TmuxSocket       string
	ZellijSession    string
	ZellijPane       string
	HerdrSession     string
	HerdrPane        string
	HerdrWorkspaceID string
	HerdrWorkspace   string
	HerdrTabID       string
	HerdrTab         string
	// MuxEnv carries the mux-driver pane variables verbatim; the daemon's mux
	// drivers (internal/mux) detect panes from them.
	MuxEnv map[string]string
}

// MuxEnvKeys are the variables the daemon's mux drivers detect panes from;
// the TS renders this list from internal/mux's registry (L127). Kept in sync
// with the deployed TS list, which includes the herdr-core (hc) variables
// HC_SESSION / HC_TERMINAL_ID / HC_TERMINAL_SOCKET and the rex driver keys.
var MuxEnvKeys = []string{
	"HC_SESSION",
	"HC_TERMINAL_ID",
	"HC_TERMINAL_SOCKET",
	"REX_BLOCK",
	"REX_SERVER",
	"REX_SESSION",
	"TERN_PANE",
	"TERN_PANE_SOCKET",
	"TERN_WINDOW_KEY",
	"TUIOS_ENV",
	"TUIOS_PANE_ID",
	"TUIOS_SESSION",
	"TUIOS_SOCKET",
	"TUIOS_WINDOW_ID",
}

// tmuxSocketFromEnv keeps the socket pair before the first comma
// (TS: tmuxSocketFromEnv, L138).
func tmuxSocketFromEnv(value string) string {
	if value == "" {
		return ""
	}
	index := strings.Index(value, ",")
	if index > 0 {
		return value[:index]
	}
	return ""
}

// hcSessionFromSocket derives the herdr-core session name from
// HC_TERMINAL_SOCKET, whose shape is "<dir>/<session>.sock" (observed:
// /run/user/0/herdr-core/default.sock → "default"). Anything that is not a
// ".sock" basename yields "".
func hcSessionFromSocket(value string) string {
	base := value
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	const suffix = ".sock"
	if !strings.HasSuffix(base, suffix) || len(base) == len(suffix) {
		return ""
	}
	return base[:len(base)-len(suffix)]
}

// runForOutput runs a command and returns its stdout
// (TS: runForOutput, L129). Every failure yields the bytes already collected,
// which is what spawnSync gives the TS after a timeout kill.
func runForOutput(command string, args []string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, _ := exec.CommandContext(ctx, command, args...).Output()
	return string(output)
}

// resolveTerminalContext builds the context from the environment
// (TS: resolveTerminalContext, L144). getenv and run are injected so the
// detection is testable without a real tmux.
func resolveTerminalContext(getenv func(string) string, run func(string, []string, time.Duration) string) TerminalContext {
	tmuxPane := getenv("TMUX_PANE")
	tmuxSocket := tmuxSocketFromEnv(getenv("TMUX"))
	var tmuxSession, tmuxWindow string
	if getenv("TMUX") != "" {
		args := []string{"display-message", "-p"}
		if tmuxPane != "" {
			args = append(args, "-t", tmuxPane)
		}
		args = append(args, "#S\t#I")
		if text := jsTrim(run("tmux", args, tmuxProbeTimeout)); text != "" {
			parts := strings.SplitN(text, "\t", 2)
			tmuxSession = parts[0]
			if len(parts) > 1 {
				tmuxWindow = parts[1]
			}
		}
	}

	zellijSession := getenv("ZELLIJ_SESSION_NAME")
	zellijPane := getenv("ZELLIJ_PANE_ID")

	// Herdr stamps pane identity into every pane's environment; labels are
	// resolved daemon-side from these ids, so no herdr CLI runs in-process.
	inHerdr := getenv("HERDR_ENV") == "1"
	herdrSession := ""
	if inHerdr {
		herdrSession = getenv("HERDR_SESSION")
	}
	herdrPane, herdrWorkspaceID, herdrTabID := "", "", ""
	if inHerdr {
		herdrPane = getenv("HERDR_PANE_ID")
		herdrWorkspaceID = getenv("HERDR_WORKSPACE_ID")
		herdrTabID = getenv("HERDR_TAB_ID")
	}

	// herdr-core (hc) stamps HC_TERMINAL_ID / HC_TERMINAL_SOCKET instead of
	// HERDR_* variables; when the classic herdr env is absent a hc terminal is
	// still reported with terminalKind "herdr": the terminal id fills
	// herdrPane and the session name is parsed from the socket path. The
	// daemon resolves labels from those ids exactly as for classic herdr.
	hcTerminalID := getenv("HC_TERMINAL_ID")
	if hcTerminalID != "" && !inHerdr {
		herdrPane = hcTerminalID
		if socket := getenv("HC_TERMINAL_SOCKET"); socket != "" {
			herdrSession = hcSessionFromSocket(socket)
		}
	}

	terminalKind := ""
	switch {
	case tmuxSession != "":
		terminalKind = "tmux"
	case inHerdr || hcTerminalID != "":
		terminalKind = "herdr"
	case getenv("ZELLIJ") != "" || zellijSession != "" || zellijPane != "":
		terminalKind = "zellij"
	}

	muxEnv := map[string]string{}
	for _, key := range MuxEnvKeys {
		if value := getenv(key); value != "" {
			muxEnv[key] = value
		}
	}

	// herdrWorkspace / herdrTab stay empty: the daemon resolves the labels.
	return TerminalContext{
		TerminalKind:     terminalKind,
		TmuxSession:      tmuxSession,
		TmuxWindow:       tmuxWindow,
		TmuxPane:         tmuxPane,
		TmuxSocket:       tmuxSocket,
		ZellijSession:    zellijSession,
		ZellijPane:       zellijPane,
		HerdrSession:     herdrSession,
		HerdrPane:        herdrPane,
		HerdrWorkspaceID: herdrWorkspaceID,
		HerdrWorkspace:   "",
		HerdrTabID:       herdrTabID,
		HerdrTab:         "",
		MuxEnv:           muxEnv,
	}
}

var (
	terminalMu       sync.Mutex
	terminalResolved bool
	terminalCached   TerminalContext
)

// TerminalContextForProcess returns the process-level context, resolved once
// on first use (TS: `const terminalContext = resolveTerminalContext()`, L202).
func TerminalContextForProcess() TerminalContext {
	terminalMu.Lock()
	defer terminalMu.Unlock()
	if !terminalResolved {
		terminalCached = resolveTerminalContext(os.Getenv, runForOutput)
		terminalResolved = true
	}
	return terminalCached
}

// ResetTerminalContextForTest drops the cached context so the next call
// re-detects it. Tests only.
func ResetTerminalContextForTest() {
	terminalMu.Lock()
	terminalResolved = false
	terminalCached = TerminalContext{}
	terminalMu.Unlock()
}

// projectNameFromCwd is the cwd's last path component
// (TS: projectNameFromCwd, L204).
func projectNameFromCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	trimmed := trailingSeparators.ReplaceAllString(cwd, "")
	index := strings.LastIndexAny(trimmed, `/\`)
	if index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// projectNameForCwd prefers the multiplexer session name over the directory
// name (TS: projectNameForCwd, L211).
func projectNameForCwd(terminal TerminalContext, cwd string) string {
	return firstString(terminal.TmuxSession, terminal.HerdrSession, terminal.ZellijSession, projectNameFromCwd(cwd))
}
