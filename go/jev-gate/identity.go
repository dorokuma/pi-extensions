package jevgate

// ===== SUBAGENT-IDENTITY (fail-closed) =====
//
// Shared S1–S4 identity core for the PiG extension and the jev-mcp MCP server.
// The extension-facing rules are the Go-family port of the TS
// SUBAGENT-IDENTITY block that exclude-tools-guard.ts and jev-gate.ts share;
// the MCP-facing rules mirror jev-mcp.mjs (token-boundary argv, interpreter
// penetration, one-layer host cap). README.md lists where they differ.
//
// A session is the main agent only when every positive proof holds; any proof
// that is missing, unverifiable, failed or timed out resolves as subagent:
//   S1: HERDR_PANE_ID is non-empty AND ends with ":p1" — the herdr lookup key
//       plus the necessary main-pane condition a subagent pane cannot claim
//       (its pane id never carries the :p1 suffix).
//   S2: the running herdr server answers the pane query with a matching
//       shell_pid > 0. The socket path is a hard-coded constant, never read
//       from HERDR_SOCKET_PATH, which would be one more forgeable variable.
//
//   Under herdr-core (hc) — where HERDR_PANE_ID is absent and HC_TERMINAL_ID
//   is exported instead — S1/S2 take the hc shape while S3/S4 run unchanged
//   on the resolved pid:
//     S1: HC_TERMINAL_ID is non-empty AND matches ^t[0-9a-z]+$.
//     S2: the hc server answers terminal.get with a record whose address
//         echoes the terminal id, whose parent is null — the top-level-
//         terminal analogue of :p1, since a subagent spawned via
//         `hc run --bg` always carries its parent's address — and whose
//         pid > 0 becomes the shell_pid. The socket path is again a
//         hard-coded constant, never read from HC_TERMINAL_SOCKET.
//   S3: the shell_pid is a strict ancestor of this process, and every
//       ancestor's /proc cmdline is readable — an empty argv is fail-closed,
//       it can never sneak past the S4 scans.
//   S4: no negative launch marker (env or argv) and no second agent/tool
//       runtime sits on the ancestry between this process and the pane
//       shell, except — extension mode only — the one resolved host.
//
// Negative launch markers can only push the verdict toward subagent (there is
// no "fake it into main" direction): CTXMODE_ROLE non-empty, PI_SUBAGENT=1,
// a non-empty PI_ROLE, and the argv markers --agent, "--name role-",
// "/tmp/herdr-role-sessions", "/tmp/dispatch-" — scanned on this process's
// environment + bridged argv AND on every ancestor's /proc environ + cmdline.
// The -i/-p/--prompt/--print argument tokens are stripped before the argv
// scan (PiG consumption rule: no @file, no flag token except "---"-led text)
// so a main-agent prompt that merely mentions a dispatch path is not a marker.
//
// Two call modes share this core:
//   - extension mode (IsSubagentSession): the host is the first pi/pig
//     ancestor below the pane shell (subprocess form) or self (fused form);
//     nothing else agent-runtime-like may sit between hop 0 and the shell.
//   - MCP mode (ResolveMCPAgentIdentity): at most one host layer
//     (agy/antigravity or pi/pig) may appear in hop 1..shellIndex-1 and every
//     other hop must be that host — a second host layer or any unknown
//     intermediate (ctxmode, dispatch-agent, an extra shell, ...) is
//     fail-closed, mirroring jev-mcp.mjs.
//
// Residual surface (known, not covered here): anything that can put another
// process into pane p1's shell makes the tree look like "shell starts an
// agent directly". Closing that needs herdr to inject a non-inheritable
// credential at pane start.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// herdrSocketFile is the herdr server socket; hard-coded on purpose.
	herdrSocketFile = "/root/.config/herdr/herdr.sock"
	// HcSocketFile is the herdr-core server socket; hard-coded on purpose.
	// HC_TERMINAL_SOCKET is deliberately never read: it would be one more
	// forgeable variable. Tests may inject a path via IdentityEnv.HcSocketPath.
	HcSocketFile = "/run/user/0/herdr-core/default.sock"
	// identityQueryTimeout bounds one socket query; a timeout resolves as subagent.
	identityQueryTimeout = 1500 * time.Millisecond
	// identityAncestorMaxHops bounds the /proc walk (cycles, malformed stat).
	identityAncestorMaxHops = 32

	// socketEnvName is the isolated-extension transport variable (PiG ships the
	// extension out of process).
	socketEnvName = "PIG_EXT_SOCKET"
	// socketEnvNamePrefix prefixes a packed cell member's socket variable.
	socketEnvNamePrefix = "PIG_EXT_SOCKET_"
	// packedCellEnvName is the packed cell key of a cell process.
	packedCellEnvName = "PIG_EXT_PACKED_CELL"
	// activeMembersEnvName lists the packed cell's members.
	activeMembersEnvName = "PIG_EXT_ACTIVE_MEMBERS"
)

// AncestorProcess is one /proc entry of the ancestry chain.
type AncestorProcess struct {
	PID     int
	PPID    int
	Cmdline string
	// Argv keeps the \0 token boundaries of /proc/<pid>/cmdline so task text
	// cannot smuggle marker words across what a space-joined cmdline would
	// read as one token. resolveIdentity fills it from splitCmdlineTokens when
	// a test only injects Cmdline.
	Argv []string
	// Env is the process's own environment as parsed from /proc/<pid>/environ
	// (nil when unreadable); tests may inject it directly.
	Env map[string]string
}

// queryPaneShellPid asks the herdr server for a pane's shell pid. Any failure
// (connect, timeout, non-JSON, mismatched reply, missing pane) is (0, false).
func queryPaneShellPid(socketPath, paneID string, timeout time.Duration) (int, bool) {

	deadline := time.Now().Add(timeout)
	dialTimeout := time.Until(deadline)
	if dialTimeout <= 0 {
		return 0, false
	}
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	request, err := json.Marshal(map[string]any{
		"id":     "pi-identity",
		"method": "pane.process_info",
		"params": map[string]any{"pane_id": paneID},
	})
	if err != nil {
		return 0, false
	}
	request = append(request, '\n')
	if _, err := conn.Write(request); err != nil {
		return 0, false
	}

	reader := bufio.NewReader(io.LimitReader(conn, 1048576))
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return 0, false
	}
	var reply struct {
		ID     string `json:"id"`
		Result struct {
			ProcessInfo struct {
				PaneID   any `json:"pane_id"`
				ShellPID any `json:"shell_pid"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return 0, false
	}
	if reply.ID != "pi-identity" {
		return 0, false
	}
	repliedPane, ok := reply.Result.ProcessInfo.PaneID.(string)
	if !ok || repliedPane != paneID {
		return 0, false
	}
	var shellPID int
	switch value := reply.Result.ProcessInfo.ShellPID.(type) {
	case float64:
		shellPID = int(value)
		if float64(shellPID) != value {
			return 0, false
		}
	case int:
		shellPID = value
	default:
		return 0, false
	}
	if shellPID <= 0 {
		return 0, false
	}
	return shellPID, true
}

// HcTerminalInfo is the herdr-core terminal record S2 needs: the terminal's
// own address, its parent address ("" = JSON null = top-level terminal, the
// hc analogue of herdr's :p1 main pane), and its shell pid.
type HcTerminalInfo struct {
	Address string
	Parent  string
	PID     int
}

// queryHcTerminalInfo asks the herdr-core server for a terminal's record via
// the JSON-RPC 2.0 terminal.get method. Any failure (connect, timeout,
// non-JSON, mismatched reply id, missing or non-object result, mismatched
// address, a "state" present but not "live", a "parent" key absent/
// non-string/non-null/empty-string, non-positive pid) is (zero, false).
//
// The result is decoded as a raw key map rather than a struct: Parent any
// conflates a missing "parent" key, parent:"" and parent:null into the same
// zero value, and only the JSON literal null marks a top-level terminal.
func queryHcTerminalInfo(socketPath, termID string, timeout time.Duration) (HcTerminalInfo, bool) {
	deadline := time.Now().Add(timeout)
	dialTimeout := time.Until(deadline)
	if dialTimeout <= 0 {
		return HcTerminalInfo{}, false
	}
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return HcTerminalInfo{}, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	request, err := json.Marshal(map[string]any{
		"id":      "pi-identity",
		"jsonrpc": "2.0",
		"method":  "terminal.get",
		"params":  map[string]any{"caller": termID, "terminal": termID},
	})
	if err != nil {
		return HcTerminalInfo{}, false
	}
	request = append(request, '\n')
	if _, err := conn.Write(request); err != nil {
		return HcTerminalInfo{}, false
	}

	reader := bufio.NewReader(io.LimitReader(conn, 1048576))
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return HcTerminalInfo{}, false
	}
	var reply struct {
		ID     string                     `json:"id"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return HcTerminalInfo{}, false
	}
	if reply.ID != "pi-identity" || reply.Result == nil {
		return HcTerminalInfo{}, false
	}
	var address string
	rawAddress, hasAddress := reply.Result["address"]
	if !hasAddress || json.Unmarshal(rawAddress, &address) != nil || address != termID {
		return HcTerminalInfo{}, false
	}
	// "state" is optional on the wire but fail-closed when present: a record
	// advertising anything but "live" ("finished", a wrong type, a bare
	// literal) can never prove a live terminal.
	if rawState, hasState := reply.Result["state"]; hasState {
		var state string
		if json.Unmarshal(rawState, &state) != nil || state != "live" {
			return HcTerminalInfo{}, false
		}
	}
	// "parent" is required and must be the JSON literal null (top-level
	// terminal) or a non-empty string (a `hc run --bg` parent's address).
	// A missing key, an empty string or any other type cannot prove the
	// top-level shape, so the whole record fails closed.
	rawParent, hasParent := reply.Result["parent"]
	if !hasParent {
		return HcTerminalInfo{}, false
	}
	var parent *string
	if err := json.Unmarshal(rawParent, &parent); err != nil {
		return HcTerminalInfo{}, false
	}
	info := HcTerminalInfo{Address: address}
	if parent != nil {
		if *parent == "" {
			return HcTerminalInfo{}, false
		}
		// A parent address (e.g. "t9t") marks a `hc run --bg` subagent
		// terminal; the caller treats it as fail-closed.
		info.Parent = *parent
	}
	var pidValue any
	rawPid, hasPid := reply.Result["pid"]
	if !hasPid || json.Unmarshal(rawPid, &pidValue) != nil {
		return HcTerminalInfo{}, false
	}
	switch pid := pidValue.(type) {
	case float64:
		info.PID = int(pid)
		if float64(info.PID) != pid {
			return HcTerminalInfo{}, false
		}
	case int:
		info.PID = pid
	default:
		return HcTerminalInfo{}, false
	}
	if info.PID <= 0 {
		return HcTerminalInfo{}, false
	}
	return info, true
}

// hcTerminalIDPattern is the herdr-core terminal address shape ("t12t").
// Only the env-provided terminal id itself is validated; socket parameters
// are never derived from it.
var hcTerminalIDPattern = regexp.MustCompile(`^t[0-9a-z]+$`)

// readProcPpid reads ppid from /proc/<pid>/stat (after the last ')').
func readProcPpid(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	stat := string(data)
	closeIndex := strings.LastIndex(stat, ")")
	if closeIndex < 0 || closeIndex+1 >= len(stat) {
		return 0, false
	}
	fields := strings.Fields(strings.TrimSpace(stat[closeIndex+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0, false
	}
	return ppid, true
}

// readProcArgv reads /proc/<pid>/cmdline as the \0-separated argv, dropping
// empty tokens ([] on error or for a kernel thread's empty cmdline).
func readProcArgv(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	argv := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			argv = append(argv, part)
		}
	}
	return argv
}

// readProcCmdline reads /proc/<pid>/cmdline (NUL separated) as one line.
func readProcCmdline(pid int) string {
	return strings.Join(readProcArgv(pid), " ")
}

// readProcEnviron parses /proc/<pid>/environ (NUL separated KEY=VALUE); nil on
// error (permission, exited process, kernel thread).
func readProcEnviron(pid int) map[string]string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil
	}
	env := make(map[string]string)
	for _, entry := range strings.Split(string(data), "\x00") {
		eq := strings.Index(entry, "=")
		if eq <= 0 {
			continue
		}
		env[entry[:eq]] = entry[eq+1:]
	}
	return env
}

// readAncestorChain walks from pid outward, stopping at the first unreadable
// hop. readArgv/readEnviron are optional: a nil reader leaves Argv/Env empty so
// resolveIdentity can fall back to splitCmdlineTokens(Cmdline) or skip the
// ancestor env scan for that hop.
func readAncestorChain(pid, maxHops int, readStat func(int) (int, bool), readCmdline func(int) string, readArgv func(int) []string, readEnviron func(int) map[string]string) []AncestorProcess {
	var chain []AncestorProcess
	seen := map[int]bool{}
	current := pid
	for hop := 0; hop < maxHops; hop++ {
		if current <= 0 || seen[current] {
			break
		}
		seen[current] = true
		ppid, ok := readStat(current)
		if !ok {
			break
		}
		entry := AncestorProcess{PID: current, PPID: ppid, Cmdline: readCmdline(current)}
		if readArgv != nil {
			entry.Argv = readArgv(current)
		}
		if readEnviron != nil {
			entry.Env = readEnviron(current)
		}
		chain = append(chain, entry)
		if ppid == 0 || ppid == current {
			break
		}
		current = ppid
	}
	return chain
}

// ---------------------------------------------------------------------------
// Token-aware cmdline classification (port of jev-mcp.mjs: splitCmdlineTokens,
// env/interpreter penetration, executable and runtime predicates).
// ---------------------------------------------------------------------------

var (
	// forbiddenExecutableBases is the set of executable basenames that must
	// never appear between this process and the pane shell. pi/pig are NOT
	// here: they are the host-runtime set (isPiPigTokens), which the MCP mode
	// counts separately and the extension mode resolves as the host anchor.
	forbiddenExecutableBases = map[string]bool{
		"ctxmode":        true,
		"dispatch-agent": true,
		"grok":           true,
	}
	// interpreterBases are executable basenames penetrated to find the script
	// actually being run (node/foo.js, env wrappers, versioned pythons/shells).
	interpreterBases = map[string]bool{
		"node": true, "nodejs": true, "bun": true, "deno": true,
		"python": true, "python3": true,
		"sh": true, "bash": true, "env": true,
	}
	// inlineTextFlags end the interpreter script-path scan: what follows is
	// inline text, not a script to classify.
	inlineTextFlags = map[string]bool{
		"-e": true, "--eval": true, "-c": true, "--command": true,
		"-p": true, "--print": true,
	}
	// flagsWithArgument consume one following token during the interpreter
	// script-path scan (the flag's own argument, not the script).
	flagsWithArgument = map[string]bool{
		"-r": true, "--require": true, "--import": true, "--loader": true,
		"--experimental-loader": true, "--conditions": true, "-C": true,
		"--input-type": true, "--watch-path": true,
		"--env-file": true, "--env-file-if-exists": true,
	}
)

var (
	// versionedPythonPattern matches python3.12-style interpreter names.
	versionedPythonPattern = regexp.MustCompile(`^python\d+(?:\.\d+)*$`)
	// shellVariantPattern matches bash/dash/zsh/ksh/ash-style shells.
	shellVariantPattern = regexp.MustCompile(`^(?:ba|da|z|k|a)?sh$`)
	// forbiddenPathPattern matches forbidden runtimes reached by path or
	// module name (mirrors jev-mcp.mjs FORBIDDEN_PATH_PATTERN).
	forbiddenPathPattern = regexp.MustCompile(`pi-coding-agent|bundle/cli\.js|(?:^|/)dispatch-agent$`)
	// agyPathPattern matches the antigravity-cli agy entry reached via a
	// node-wrapped script path.
	agyPathPattern = regexp.MustCompile(`/antigravity-cli/bin/agy$`)
)

// splitCmdlineTokens splits a literal cmdline on \0 and unquoted whitespace,
// honouring single/double quotes (a real /proc cmdline is already \0-separated
// and unquoted; this keeps a space-joined test cmdline's token semantics).
func splitCmdlineTokens(cmdline string) []string {
	if cmdline == "" {
		return nil
	}
	var tokens []string
	for _, chunk := range strings.Split(cmdline, "\x00") {
		var current strings.Builder
		started := false
		var quote rune
		for _, ch := range chunk {
			if quote != 0 {
				if ch == quote {
					quote = 0
					continue
				}
				current.WriteRune(ch)
				continue
			}
			if ch == '"' || ch == '\'' {
				quote = ch
				started = true
				continue
			}
			if ch == ' ' || ch == '\t' {
				if started {
					tokens = append(tokens, current.String())
					current.Reset()
					started = false
				}
				continue
			}
			current.WriteRune(ch)
			started = true
		}
		if started {
			tokens = append(tokens, current.String())
		}
	}
	return tokens
}

// argv0Base is the basename of the first cmdline token ("" on empty input).
func argv0Base(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	argv0 := tokens[0]
	if slash := strings.LastIndex(argv0, "/"); slash >= 0 {
		return argv0[slash+1:]
	}
	return argv0
}

func isForbiddenExecutablePath(token string) bool {
	if token == "" {
		return false
	}
	if forbiddenExecutableBases[argv0Base([]string{token})] {
		return true
	}
	return forbiddenPathPattern.MatchString(token)
}

func isInterpreterBase(base string) bool {
	if base == "" {
		return false
	}
	if interpreterBases[base] {
		return true
	}
	return versionedPythonPattern.MatchString(base) || shellVariantPattern.MatchString(base)
}

func isEnvAssignment(token string) bool {
	if token == "" {
		return false
	}
	eq := strings.Index(token, "=")
	if eq <= 0 {
		return false
	}
	for index, ch := range token[:eq] {
		if ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (index > 0 && ch >= '0' && ch <= '9') {
			continue
		}
		return false
	}
	return true
}

func isModuleFlagToken(token string) bool {
	return token == "-m" || token == "--module"
}

func moduleValueFromToken(token string) (string, bool) {
	if strings.HasPrefix(token, "--module=") {
		return token[len("--module="):], true
	}
	if strings.HasPrefix(token, "-m=") {
		return token[len("-m="):], true
	}
	if strings.HasPrefix(token, "-m") && len(token) > 2 {
		return token[2:], true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// GNU env(1) option model (coreutils 9.7, verified against `env --help` /
// `env --version` and live probes on this host).
//
// Token phases are strict: [options] [NAME=VALUE]* [command args...] — the
// first non-option token switches to the operand phase for good, so
// `env FOO=1 -a m cmd` runs `-a` as the command, and `env --` still parses
// assignments after the terminator. `-S/--split-string` is the one exception
// with real bite: its string is re-split and the pieces are prepended to the
// remaining argv, then the whole thing is re-parsed through the same option
// phase (a shebang line carries options inside it).
//
// Long options accept unambiguous prefix abbreviations (`--argv` → `--argv0`,
// `--un` → `--unset`); ambiguous or unknown options make env(1) exit(125)
// without running anything, so the scan must return "no executable" rather
// than guessing — an ambiguous option must never consume the next token as
// its argument, and that token must never be read as the wrapped executable
// either (e.g. `env --argv mask-name ctxmode` resolves `--argv` → `--argv0`
// and consumes mask-name; it must NOT leave mask-name as the executable).
//
// envArgClass is the GNU argument class of one env option.
type envArgClass int

const (
	envArgNone     envArgClass = iota // takes no argument (`=value` → error 125)
	envArgRequired                    // needs a value: `=v` or the next token
	envArgOptional                    // only `=v` is a value; next token is free
	envArgTerminal                    // prints and exits: --help / --version
)

// envLongOptions is the long-option table of coreutils 9.7 env.
var envLongOptions = map[string]envArgClass{
	"argv0":                envArgRequired,
	"ignore-environment":   envArgNone,
	"null":                 envArgNone,
	"unset":                envArgRequired,
	"chdir":                envArgRequired,
	"split-string":         envArgRequired,
	"block-signal":         envArgOptional,
	"default-signal":       envArgOptional,
	"ignore-signal":        envArgOptional,
	"list-signal-handling": envArgNone,
	"debug":                envArgNone,
	"help":                 envArgTerminal,
	"version":              envArgTerminal,
}

// resolveEnvLongOption resolves the name part of a `--name[=value]` token to
// the full long option via GNU unambiguous-prefix matching (`--argv` →
// `--argv0`, `--un` → `--unset`). ok=false means the token is not a valid
// option — unknown or an ambiguous abbreviation (`--d`, `--ig`, `--a`): env
// exits(125) without executing anything in that shape, so the caller treats
// the whole hop as "no executable" and must not touch the following token.
func resolveEnvLongOption(name string) (string, envArgClass, bool) {
	if class, exact := envLongOptions[name]; exact {
		return name, class, true
	}
	var match string
	var class envArgClass
	matches := 0
	for candidate, candidateClass := range envLongOptions {
		if strings.HasPrefix(candidate, name) {
			match = candidate
			class = candidateClass
			matches++
		}
	}
	if matches == 1 {
		return match, class, true
	}
	return "", 0, false
}

// envOnlyOperands reports whether every remaining token is operand-phase
// filler — `-` (which itself begins the operand phase), `--`, or an
// assignment — i.e. env reaches the end without a command. Used only to
// decide whether `-0/--null` saw a command; anything option-shaped that
// remains is conservatively treated as a would-be command (env would error
// on `-0` + command anyway).
func envOnlyOperands(tail []string) bool {
	operandPhase := false
	for _, token := range tail {
		if !operandPhase && (token == "--" || token == "-") {
			operandPhase = true
			continue
		}
		if isEnvAssignment(token) {
			operandPhase = true
			continue
		}
		return false
	}
	return true
}

// envCommandTokens unwraps the option/assignment phases of env's argv tail
// (the tokens after `env` itself) and returns the command argv env would
// exec. ok=false means env cannot produce an executable at all: a missing
// required argument (`env -a`, `env --argv0`, cluster tail `-a`, `-u/-C/-S`
// at the end), an unknown or ambiguous long option, `=`-values on
// no-argument options, a terminal option, or `-0/--null` combined with a
// command — env itself errors out (or prints) in every one of those shapes,
// and pretending the hop resolves to anything would misclassify it. ok=true
// with an empty result is a clean "no command" invocation (`env FOO=1`).
//
// Token phases are strict: [options] [NAME=VALUE]* [command args...] — the
// first non-option token switches to the operand phase for good (verified:
// `env FOO=1 -a m cmd` execs `-a` as the command; `env -- FOO=1 cmd` still
// parses the assignment after `--`). `-S/--split-string` is the exception
// with real bite: its string is re-split by envSplitString (the GNU
// build_argv quote/escape rules, not strings.Fields) and the pieces replace
// its argument slot, then the stream is re-parsed through the option phase
// (a shebang line carries options inside it — verified: `-S '-a mask cmd'`
// honours the argv0 mask).
func envCommandTokens(tail []string, depth int) ([]string, bool) {
	if depth > 8 {
		return nil, false
	}
	optionsDone := false
	for len(tail) > 0 {
		token := tail[0]
		if !optionsDone && (token == "-" || token == "--") {
			// `--` ends the option phase; a lone `-` implies -i and — verified
			// on this host — ALSO begins the operand phase (`env - -a m cmd`
			// execs `-a`, `env - -` errors on the second `-`).
			optionsDone = true
			tail = tail[1:]
			continue
		}
		if optionsDone || isEnvAssignment(token) || !strings.HasPrefix(token, "-") {
			// Operand phase: assignments are consumed; the first other token
			// is the command. An assignment permanently ends option parsing
			// (env does not permute): `env FOO=1 -a m cmd` execs `-a`.
			if isEnvAssignment(token) {
				tail = tail[1:]
				optionsDone = true
				continue
			}
			return tail, true
		}
		if strings.HasPrefix(token, "--") {
			name := token[2:]
			inline := ""
			hasInline := false
			if eq := strings.Index(name, "="); eq >= 0 {
				inline, name, hasInline = name[eq+1:], name[:eq], true
			}
			if name == "" {
				return nil, false
			}
			resolved, class, ok := resolveEnvLongOption(name)
			if !ok {
				return nil, false
			}
			switch class {
			case envArgTerminal:
				// --help/--version print and exit: nothing is ever exec'd.
				return nil, false
			case envArgNone:
				if hasInline {
					// "option doesn't allow an argument" — env exits(125).
					return nil, false
				}
				if resolved == "null" {
					// --null prints the environment and refuses a command.
					if envOnlyOperands(tail[1:]) {
						return nil, true
					}
					return nil, false
				}
				tail = tail[1:]
				continue
			case envArgOptional:
				// Optional-argument options take their value only via `=`;
				// the next token stays free (it may be the command).
				tail = tail[1:]
				continue
			default: // envArgRequired
				if hasInline {
					if resolved == "split-string" {
						pieces, ok := envSplitString(inline)
						if !ok {
							return nil, false
						}
						tail = append(pieces, tail[1:]...)
						continue
					}
					tail = tail[1:]
					continue
				}
				if len(tail) < 2 {
					// Missing required argument: env exits(125), no exec.
					return nil, false
				}
				if resolved == "split-string" {
					pieces, ok := envSplitString(tail[1])
					if !ok {
						return nil, false
					}
					tail = append(pieces, tail[2:]...)
					continue
				}
				tail = tail[2:]
				continue
			}
		}
		// Short-option cluster: -a/-u/-C/-S take an argument — the rest of
		// the cluster when present, else the next token. The cluster loop
		// resolves how far the tail advances before it is consumed.
		cluster := token
		consume := 1 // 1 = just this token, 2 = this token + its argument token
		for pos := 1; pos < len(cluster); pos++ {
			switch cluster[pos] {
			case 'i', 'v':
				continue
			case '0':
				if envOnlyOperands(tail[1:]) {
					return nil, true
				}
				return nil, false
			case 'a', 'u', 'C':
				if pos+1 < len(cluster) {
					// The rest of the cluster is this letter's argument.
					pos = len(cluster)
					continue
				}
				if len(tail) < 2 {
					return nil, false // missing required argument
				}
				consume = 2
				pos = len(cluster)
			case 'S':
				if pos+1 < len(cluster) {
					pieces, ok := envSplitString(cluster[pos+1:])
					if !ok {
						return nil, false
					}
					tail = append(pieces, tail[1:]...)
				} else if len(tail) >= 2 {
					pieces, ok := envSplitString(tail[1])
					if !ok {
						return nil, false
					}
					tail = append(pieces, tail[2:]...)
				} else {
					return nil, false // missing required argument
				}
				// The split pieces re-enter the option phase in place.
				optionsDone = false
				consume = 0
				pos = len(cluster)
			default:
				return nil, false // unknown short option
			}
		}
		tail = tail[consume:]
	}
	return nil, true
}

// envSplitString splits one -S/--split-string value into argument tokens
// with the quote/escape rules of GNU coreutils build_argv (verified against
// `env --debug` on coreutils 9.7). ok=false mirrors an env error(125): a
// malformed -S string can never reach exec, so the env hop resolves to no
// executable instead of tokenising it wrongly.
//
// Verified semantics: single and double quotes group one argument and can
// abut (a'x'"y" = ax"y"...); unclosed quotes error; C-locale whitespace
// separates outside quotes; `#` starts a comment only where a separator
// precedes it (never inside quotes); `\c` truncates the rest of the string;
// outside quotes `\_` separates while inside double quotes it is a space;
// `\\`, `\"`, `\'`, `\#`, `\$` are literal and `\f \n \r \t \v` map to
// their control characters anywhere outside single quotes (single quotes
// additionally honour `\\` and `\'`, every other backslash stays
// literal); any other escape is an env error. `${VAR}` outside single
// quotes is GNU's expansion syntax: the model keeps the literal text as one
// opaque argument — the ancestor's environment is unavailable, and the
// token can never be mistaken for an option argument it masks. A bare `$`
// GNU rejects outright is a parse error here too.
func envSplitString(str string) ([]string, bool) {
	var args []string
	var cur strings.Builder
	// sep is true when no argument is in progress: at the start, after a
	// whitespace run or a `\_` separator, and right after an opening or
	// closing quote at an argument boundary.
	sep := true
	dq, sq := false, false
	for i := 0; i < len(str); {
		switch ch := str[i]; {
		case ch == '\'':
			if dq {
				cur.WriteByte(ch)
				i++
				continue
			}
			sq = !sq
			sep = false
			i++
			continue
		case ch == '"':
			if sq {
				cur.WriteByte(ch)
				i++
				continue
			}
			dq = !dq
			sep = false
			i++
			continue
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\v' || ch == '\f' || ch == '\r':
			if sq || dq {
				cur.WriteByte(ch)
				i++
				continue
			}
			if !sep {
				args = append(args, cur.String())
				cur.Reset()
				sep = true
			}
			i++
			continue
		case ch == '#':
			// '#' as first char after a separator terminates the string
			// (a comment); anywhere else it is literal.
			if sep {
				return args, true
			}
			cur.WriteByte(ch)
			i++
			continue
		case ch == '\\':
			// Backslash inside single quotes is literal unless it precedes
			// `\` or `'`, which GNU still escapes.
			if sq && i+1 < len(str) && str[i+1] != '\\' && str[i+1] != '\'' {
				cur.WriteByte(ch)
				i++
				continue
			}
			if i+1 >= len(str) {
				return nil, false // invalid backslash at end of string
			}
			switch next := str[i+1]; next {
			case '"', '#', '$', '\'', '\\':
				sep = false
				cur.WriteByte(next)
				i += 2
				continue
			case '_':
				if !dq {
					// `\_` outside double quotes is an argument separator.
					if !sep {
						args = append(args, cur.String())
						cur.Reset()
						sep = true
					}
					i += 2
					continue
				}
				sep = false
				cur.WriteByte(' ')
				i += 2
				continue
			case 'c':
				if dq {
					return nil, false // '\c' must not appear in double quotes
				}
				// '\c' truncates the rest of the -S string.
				if !sep {
					args = append(args, cur.String())
				}
				return args, true
			case 'f':
				sep = false
				cur.WriteByte('\f')
				i += 2
				continue
			case 'n':
				sep = false
				cur.WriteByte('\n')
				i += 2
				continue
			case 'r':
				sep = false
				cur.WriteByte('\r')
				i += 2
				continue
			case 't':
				sep = false
				cur.WriteByte('\t')
				i += 2
				continue
			case 'v':
				sep = false
				cur.WriteByte('\v')
				i += 2
				continue
			default:
				return nil, false // invalid escape sequence
			}
		case ch == '$':
			// ${VAR} is not expanded inside single quotes; outside them GNU
			// expands it into the current argument (an unset variable adds
			// nothing). The ancestor's environment is out of reach, so the
			// model keeps the `${...}` text as one opaque argument — the
			// same token count GNU produces for a set variable, and it can
			// never be mistaken for a masked option argument. A `$` GNU
			// rejects (not `${NAME}`) is a parse error here too.
			if sq {
				sep = false
				cur.WriteByte(ch)
				i++
				continue
			}
			if i+2 < len(str) && str[i+1] == '{' &&
				(str[i+2] == '_' || (str[i+2] >= 'A' && str[i+2] <= 'Z') || (str[i+2] >= 'a' && str[i+2] <= 'z')) {
				end := i + 3
				for end < len(str) && (str[end] == '_' ||
					(str[end] >= '0' && str[end] <= '9') ||
					(str[end] >= 'A' && str[end] <= 'Z') ||
					(str[end] >= 'a' && str[end] <= 'z')) {
					end++
				}
				if end < len(str) && str[end] == '}' {
					sep = false
					cur.WriteString(str[i : end+1])
					i = end + 1
					continue
				}
			}
			return nil, false // unsupported $ form: env errors out
		default:
			sep = false
			cur.WriteByte(ch)
			i++
		}
	}
	if dq || sq {
		return nil, false // no terminating quote in -S string
	}
	if !sep {
		args = append(args, cur.String())
	}
	return args, true
}

// checkExecutableTokens reports whether the argv resolves — through env
// wrappers and interpreters — to a forbidden agent/tool runtime.
func checkExecutableTokens(tokens []string, depth int) bool {
	if depth > 8 || len(tokens) == 0 {
		return false
	}
	base := argv0Base(tokens)

	if base == "env" {
		// Unwrap env's options and assignments down to the command it would
		// exec (F-1/M1/M2): the -a/--argv0 mask value and every other option
		// argument must never be mistaken for the wrapped executable, and an
		// env invocation that can never exec (missing option argument,
		// unknown/ambiguous option, terminal option) resolves to "no
		// executable" instead of guessing.
		residual, ok := envCommandTokens(tokens[1:], depth)
		if !ok || len(residual) == 0 {
			return false
		}
		return checkExecutableTokens(residual, depth+1)
	}

	if !isInterpreterBase(base) {
		return isForbiddenExecutablePath(tokens[0])
	}

	// An interpreter: scan forward for the first script-path token.
	for index := 1; index < len(tokens); index++ {
		token := tokens[index]
		if token == "--" {
			if index+1 < len(tokens) {
				return isForbiddenExecutablePath(tokens[index+1])
			}
			return false
		}
		if isModuleFlagToken(token) {
			if index+1 < len(tokens) {
				return isForbiddenExecutablePath(tokens[index+1])
			}
			return false
		}
		if value, ok := moduleValueFromToken(token); ok {
			return isForbiddenExecutablePath(value)
		}
		if inlineTextFlags[token] {
			return false
		}
		if flagsWithArgument[token] {
			index++
			continue
		}
		if strings.HasPrefix(token, "--") && strings.Contains(token, "=") {
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		return isForbiddenExecutablePath(token)
	}
	return false
}

// executableTokenOf unwraps env wrappers (KEY=VAL, flags, -S/--split-string,
// -- terminator) and returns the token of the executable actually run. The
// env hop shares envCommandTokens with checkExecutableTokens so the argv0
// mask, abbreviations and no-exec shapes can never drift apart: an env
// invocation that cannot exec resolves to "".
func executableTokenOf(tokens []string, depth int) string {
	if depth > 8 || len(tokens) == 0 {
		return ""
	}
	base := argv0Base(tokens)
	if base == "env" {
		residual, ok := envCommandTokens(tokens[1:], depth)
		if !ok || len(residual) == 0 {
			return ""
		}
		return executableTokenOf(residual, depth+1)
	}
	return tokens[0]
}

// isForbiddenTokens reports whether tokens run a forbidden agent/tool runtime
// (pi, ctxmode, dispatch-agent, grok, pi-coding-agent, bundle/cli.js).
func isForbiddenTokens(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	return checkExecutableTokens(tokens, 0)
}

func isAgyExecutableToken(token string) bool {
	if token == "" {
		return false
	}
	base := token
	if slash := strings.LastIndex(token, "/"); slash >= 0 {
		base = token[slash+1:]
	}
	if base == "agy" || base == "antigravity" {
		return true
	}
	return agyPathPattern.MatchString(token)
}

// isAgyTokens reports whether tokens run agy/antigravity (directly, env-wrapped
// or interpreter-wrapped). The interpreter branch honours the same flag table
// as checkExecutableTokens: "--" and -m/--module take their following token,
// inline-text flags (-c/-e/-p ...) end the script scan so `bash -c "agy"` can
// never pass off its command string as the agy host, and flags that consume an
// argument skip that argument.
func isAgyTokens(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	if isAgyExecutableToken(executableTokenOf(tokens, 0)) {
		return true
	}
	if isInterpreterBase(argv0Base(tokens)) {
		for index := 1; index < len(tokens); index++ {
			token := tokens[index]
			if token == "--" {
				if index+1 < len(tokens) {
					return isAgyExecutableToken(tokens[index+1])
				}
				return false
			}
			if isModuleFlagToken(token) {
				if index+1 < len(tokens) {
					return isAgyExecutableToken(tokens[index+1])
				}
				return false
			}
			if value, ok := moduleValueFromToken(token); ok {
				return isAgyExecutableToken(value)
			}
			if inlineTextFlags[token] {
				return false
			}
			if flagsWithArgument[token] {
				index++
				continue
			}
			if strings.HasPrefix(token, "--") && strings.Contains(token, "=") {
				continue
			}
			if strings.HasPrefix(token, "-") {
				continue
			}
			return isAgyExecutableToken(token)
		}
	}
	return false
}

// tokensOf prefers a hop's \0-separated Argv and falls back to the
// quote-aware tokenisation of its joined cmdline.
func tokensOf(entry AncestorProcess) []string {
	if len(entry.Argv) > 0 {
		return entry.Argv
	}
	return splitCmdlineTokens(entry.Cmdline)
}

var agentRuntimePattern = regexp.MustCompile(`pi-coding-agent|bundle/cli\.js`)

// ---------------------------------------------------------------------------
// Negative launch markers (env + argv, prompt arguments stripped).
// ---------------------------------------------------------------------------

// StripPromptArgTokens removes the prompt-carrying flags and the one
// argument token each may consume, so a main agent's prompt text that merely
// mentions "--agent" or "/tmp/dispatch-..." is not mistaken for a launch
// marker. Exported for the MCP layer and tests.
//
// Consumption rule (PiG, verified against ref/PiG/coding/cli/args.go:258-270,
// the "--print" || "-p" case): the next token counts as prompt text only when
// it does NOT start with "@" (an @file inclusion stays visible to the
// marker scan) and is NOT flag-shaped — where a token starting with "---"
// counts as prompt text, not a flag (PiG lets "---"-led positionals be
// messages). A plain "-"-prefixed token is an option and stays visible, so
// `pi -p --name role-x` still trips the marker scan (F-2).
//
// PiG's parser defines no -i/--prompt option (verified: no such case in
// args.go); both remain in the strip set for the agy-side flag family whose
// consumption semantics are unverified — the same conservative rule is
// applied so neither can ever swallow a structural marker token. The
// "=value" inline forms always carry their own argument.
func StripPromptArgTokens(tokens []string) []string {
	structural := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token == "-i" || token == "-p" || token == "--prompt" || token == "--print" {
			if index+1 < len(tokens) {
				next := tokens[index+1]
				if !strings.HasPrefix(next, "@") &&
					(!strings.HasPrefix(next, "-") || strings.HasPrefix(next, "---")) {
					index++
				}
			}
			continue
		}
		if strings.HasPrefix(token, "-i=") || strings.HasPrefix(token, "-p=") ||
			strings.HasPrefix(token, "--prompt=") || strings.HasPrefix(token, "--print=") {
			continue
		}
		structural = append(structural, token)
	}
	return structural
}

// HasSubagentCliFlags reports whether argv carries a dispatch launch marker:
// --agent <val> / --agent=*, "--name role-*" (separate or = form), or a token
// containing /tmp/herdr-role-sessions or /tmp/dispatch-. Prompt argument
// tokens are stripped first. Exported for the MCP layer and tests.
func HasSubagentCliFlags(tokens []string) bool {
	tokens = StripPromptArgTokens(tokens)
	for index, token := range tokens {
		if token == "--agent" {
			if index+1 < len(tokens) {
				return true
			}
			continue
		}
		if strings.HasPrefix(token, "--agent=") {
			return true
		}
		if token == "--name" {
			if index+1 < len(tokens) && strings.HasPrefix(tokens[index+1], "role-") {
				return true
			}
			continue
		}
		if strings.HasPrefix(token, "--name=role-") {
			return true
		}
		if strings.Contains(token, "/tmp/herdr-role-sessions") {
			return true
		}
		if strings.Contains(token, "/tmp/dispatch-") {
			return true
		}
	}
	return false
}

// negativeEnvMarker returns the first negative environment marker in env
// (nil-safe): CTXMODE_ROLE non-empty, PI_SUBAGENT=1, or PI_ROLE non-empty.
func negativeEnvMarker(env map[string]string) (string, bool) {
	if strings.TrimSpace(env["CTXMODE_ROLE"]) != "" {
		return "CTXMODE_ROLE=" + env["CTXMODE_ROLE"], true
	}
	if env["PI_SUBAGENT"] == "1" {
		return "PI_SUBAGENT=1", true
	}
	if strings.TrimSpace(env["PI_ROLE"]) != "" {
		return "PI_ROLE=" + env["PI_ROLE"], true
	}
	return "", false
}

// subagentLaunchMarker returns the first negative marker that matched on this
// process (environment + bridged argv). The argv half uses the token-boundary
// scan so prompt text is not mistaken for a launch flag.
func subagentLaunchMarker(getenv func(string) string, args []string) (string, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if marker, hit := negativeEnvMarker(map[string]string{
		"CTXMODE_ROLE": getenv("CTXMODE_ROLE"),
		"PI_SUBAGENT":  getenv("PI_SUBAGENT"),
		"PI_ROLE":      getenv("PI_ROLE"),
	}); hit {
		return marker, true
	}
	if marker, hit := argvMarkerDescription(args); hit {
		return marker, true
	}
	return "", false
}

// argvMarkerDescription mirrors HasSubagentCliFlags and reports a stable
// reason fragment per marker kind (kept identical to the previous join-based
// wording so diagnostics stay greppable).
func argvMarkerDescription(args []string) (string, bool) {
	tokens := StripPromptArgTokens(args)
	for index, token := range tokens {
		if token == "--agent" && index+1 < len(tokens) {
			return "argv has --agent", true
		}
		if strings.HasPrefix(token, "--agent=") {
			return "argv has --agent", true
		}
		if token == "--name" && index+1 < len(tokens) && strings.HasPrefix(tokens[index+1], "role-") {
			return "argv has --name role-", true
		}
		if strings.HasPrefix(token, "--name=role-") {
			return "argv has --name role-", true
		}
		if strings.Contains(token, "/tmp/herdr-role-sessions") {
			return "argv has /tmp/herdr-role-sessions", true
		}
		if strings.Contains(token, "/tmp/dispatch-") {
			return "argv has /tmp/dispatch-", true
		}
	}
	return "", false
}

// ArgvCache keeps the argv read from PIG_HARNESS_ARGV_FILE across turns of one
// process. The variable is deliberately NOT unset after the first read: inside
// one packed cell several extensions share the environment, and a sibling that
// resolves its identity later (e.g. at EventSessionStart) must still be able
// to read the file. The cache stays as defence in depth for a caller that
// arrives after the file variable was removed by the harness or a restart.
type ArgvCache struct {
	// mu guards the cached argv itself.
	mu sync.Mutex
	// loadMu serialises one whole bridge load (cache probe, environment read,
	// file read, cache fill): concurrent callers all observe either the
	// still-set variable or the filled cache, never a torn middle state.
	loadMu   sync.Mutex
	cached   bool
	fileArgs []string
}

// Reset clears the cache.
func (c *ArgvCache) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = false
	c.fileArgs = nil
}

// Get returns a copy of the cached argv.
func (c *ArgvCache) Get() ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.cached {
		return nil, false
	}
	return append([]string(nil), c.fileArgs...), true
}

// Set stores the file argv.
func (c *ArgvCache) Set(args []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = true
	c.fileArgs = append([]string(nil), args...)
}

// defaultArgvCache is the process-wide cache of the harness argv file.
var defaultArgvCache = &ArgvCache{}

// loadEffectiveArgs merges os.Args-style base arguments with the harness argv
// file. PiG starts a Go extension subprocess with no arguments and points
// PIG_HARNESS_ARGV_FILE at the harness CLI arguments.
//
// The variable is deliberately left in the environment after the read: packed
// cell members share the process environment, and a sibling extension must be
// able to read the same file whenever it resolves its identity. It is only an
// additional NEGATIVE marker source — it never elevates a session to main.
//
// Fail-closed: a set but unreadable/unparseable file is an error, so the caller
// resolves as subagent instead of silently losing the argv markers.
func loadEffectiveArgs(getenv func(string) string, baseArgs []string, cache *ArgvCache) ([]string, error) {
	if cache == nil {
		cache = defaultArgvCache
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	cache.loadMu.Lock()
	defer cache.loadMu.Unlock()
	if cached, ok := cache.Get(); ok {
		return mergeArgs(baseArgs, cached), nil
	}
	argvFile := strings.TrimSpace(getenv("PIG_HARNESS_ARGV_FILE"))
	if argvFile == "" {
		return baseArgs, nil
	}

	data, err := os.ReadFile(argvFile)
	if err != nil {
		if getenv("PI_IDENTITY_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "[subagent-identity] warning: failed to read PIG_HARNESS_ARGV_FILE %q: %v\n", argvFile, err)
		}
		return nil, fmt.Errorf("read PIG_HARNESS_ARGV_FILE %q failed: %w", argvFile, err)
	}
	var fileArgs []string
	if err := json.Unmarshal(data, &fileArgs); err != nil {
		if getenv("PI_IDENTITY_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "[subagent-identity] warning: failed to parse PIG_HARNESS_ARGV_FILE %q: %v\n", argvFile, err)
		}
		return nil, fmt.Errorf("parse PIG_HARNESS_ARGV_FILE %q failed: %w", argvFile, err)
	}
	cache.Set(fileArgs)
	return mergeArgs(baseArgs, fileArgs), nil
}

// mergeArgs unions both argument lists, keeping first-seen order and dropping
// duplicates.
func mergeArgs(baseArgs, fileArgs []string) []string {
	seen := make(map[string]bool, len(baseArgs)+len(fileArgs))
	merged := make([]string, 0, len(baseArgs)+len(fileArgs))
	for _, list := range [][]string{baseArgs, fileArgs} {
		for _, arg := range list {
			if seen[arg] {
				continue
			}
			seen[arg] = true
			merged = append(merged, arg)
		}
	}
	return merged
}

// IdentityEnv is every input of one identity resolution. Tests fill it in;
// defaultIdentityEnv is the production value.
type IdentityEnv struct {
	Getenv      func(string) string
	Args        []string
	PID         int
	SocketPath  string
	Timeout     time.Duration
	ReadStat    func(int) (int, bool)
	ReadCmdline func(int) string
	// ReadArgv reads /proc/<pid>/cmdline as tokens (nil keeps a hop's Argv as
	// injected, or derives it from Cmdline via splitCmdlineTokens).
	ReadArgv func(int) []string
	// ReadEnviron reads /proc/<pid>/environ (nil keeps a hop's injected Env).
	ReadEnviron func(int) map[string]string
	// Environ enumerates the environment for the PIG_EXT_SOCKET_<NAME> keys,
	// whose names are not known in advance.
	Environ     func() []string
	QuerySocket func(socketPath, paneID string, timeout time.Duration) (int, bool)
	// HcSocketPath overrides the herdr-core socket for tests; the production
	// value is the hard-coded HcSocketFile, never HC_TERMINAL_SOCKET.
	HcSocketPath string
	// QueryHcSocket is the herdr-core terminal.get query. It is a separate
	// field from QuerySocket because the hc record also carries the parent
	// address — the top-level check QuerySocket's (pid, ok) shape cannot
	// express.
	QueryHcSocket func(socketPath, termID string, timeout time.Duration) (HcTerminalInfo, bool)
	ArgvCache     *ArgvCache
}

// defaultIdentityEnv is the production identity environment.
func defaultIdentityEnv() IdentityEnv {
	return IdentityEnv{
		Getenv:        os.Getenv,
		Args:          os.Args,
		PID:           os.Getpid(),
		SocketPath:    herdrSocketFile,
		Timeout:       identityQueryTimeout,
		ReadStat:      readProcPpid,
		ReadCmdline:   readProcCmdline,
		ReadArgv:      readProcArgv,
		ReadEnviron:   readProcEnviron,
		Environ:       os.Environ,
		QuerySocket:   queryPaneShellPid,
		HcSocketPath:  HcSocketFile,
		QueryHcSocket: queryHcTerminalInfo,
		ArgvCache:     defaultArgvCache,
	}
}

// detectSubprocessForm reports whether the extension runs out of process and
// which environment entry proves it: PIG_EXT_SOCKET (isolated process),
// PIG_EXT_PACKED_CELL / PIG_EXT_ACTIVE_MEMBERS (packed cell), or any non-empty
// PIG_EXT_SOCKET_<NAME> (packed cell member — the ordinary Go shape).
func detectSubprocessForm(getenv func(string) string, environ func() []string) (bool, string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, name := range []string{socketEnvName, packedCellEnvName, activeMembersEnvName} {
		if strings.TrimSpace(getenv(name)) != "" {
			return true, name
		}
	}
	if environ == nil {
		return false, ""
	}
	for _, entry := range environ() {
		key, _, hasValue := strings.Cut(entry, "=")
		if !hasValue || !strings.HasPrefix(key, socketEnvNamePrefix) {
			continue
		}
		if strings.TrimSpace(getenv(key)) != "" {
			return true, key
		}
	}
	return false, ""
}

// identityWarnOnce deduplicates the single stderr warning emitted when identity
// cannot be verified. The 1500ms budget is never extended and no retry layer is
// added: one line per process is the whole observability contract.
var identityWarnOnce sync.Once

// reportIdentityWarn emits at most one warning line per process.
func reportIdentityWarn(reason string) {
	identityWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[subagent-identity] warning: identity not verified, fail-closed to subagent: %s\n", reason)
	})
	if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "[subagent-identity] debug: identity not verified (%s)\n", reason)
	}
}

// failClosed reports the reason once and returns the fail-closed verdict.
func failClosed(reason string) (bool, string) {
	reportIdentityWarn(reason)
	return true, reason
}

// describeAncestorChain renders the chain for diagnostics (oldest last).
func describeAncestorChain(chain []AncestorProcess) string {
	parts := make([]string, 0, len(chain))
	for _, entry := range chain {
		cmd := entry.Cmdline
		if len(cmd) > 40 {
			cmd = cmd[:40]
		}
		if cmd != "" {
			parts = append(parts, fmt.Sprintf("%d(%s)", entry.PID, cmd))
		} else {
			parts = append(parts, strconv.Itoa(entry.PID))
		}
	}
	return strings.Join(parts, " <- ")
}

// fillIdentityDefaults installs the production readers for every field the
// caller left unset. ReadArgv/ReadEnviron default to the real /proc readers
// only when the caller did not inject its own ReadStat/ReadCmdline: a test
// chain built from synthetic pids must never reach out to the host's real
// /proc/<synthetic_pid>/cmdline or environ — a nil reader leaves the hop's
// injected Argv/Env (or the Cmdline fallback) authoritative instead.
func fillIdentityDefaults(env *IdentityEnv) {
	customProcReaders := env.ReadStat != nil || env.ReadCmdline != nil
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.ReadStat == nil {
		env.ReadStat = readProcPpid
	}
	if env.ReadCmdline == nil {
		env.ReadCmdline = readProcCmdline
	}
	if env.ReadArgv == nil && !customProcReaders {
		env.ReadArgv = readProcArgv
	}
	if env.ReadEnviron == nil && !customProcReaders {
		env.ReadEnviron = readProcEnviron
	}
	if env.Environ == nil {
		env.Environ = os.Environ
	}
	if env.QuerySocket == nil {
		env.QuerySocket = queryPaneShellPid
	}
	if env.HcSocketPath == "" {
		env.HcSocketPath = HcSocketFile
	}
	if env.QueryHcSocket == nil {
		env.QueryHcSocket = queryHcTerminalInfo
	}
	if env.Timeout <= 0 {
		env.Timeout = identityQueryTimeout
	}
	if env.SocketPath == "" {
		env.SocketPath = herdrSocketFile
	}
}

// resolveIdentity is the shared S1–S4 walk. Negative markers (environment and
// argv) and the S1/S2/S3 positive proofs run identically for both modes; the
// ancestor scan then branches on extension vs MCP host topology.
func resolveIdentity(env IdentityEnv, mcpMode bool) (bool, string) {
	fillIdentityDefaults(&env)

	effectiveArgs, err := loadEffectiveArgs(env.Getenv, env.Args, env.ArgvCache)
	if err != nil {
		return true, fmt.Sprintf("argv bridge failed: %v", err)
	}
	if marker, hit := subagentLaunchMarker(env.Getenv, effectiveArgs); hit {
		return true, "launch marker: " + marker
	}

	// S1/S2 come in two shapes. With HERDR_PANE_ID set, the herdr shape: the
	// pane id is both the herdr lookup key and a necessary condition — a main
	// agent's pane id always ends with ":p1"; anything else cannot be the
	// main pane and is rejected before the socket query. Under herdr-core
	// (HERDR_PANE_ID absent, HC_TERMINAL_ID exported), the hc shape: the
	// terminal id is the hc lookup key, terminal.get must return a live
	// top-level record (a "state" present but not "live" fails; parent
	// must be the JSON literal null — the hc analogue of the :p1 main
	// pane, since a `hc run --bg` subagent always carries its parent's
	// address) and its pid becomes the shell_pid S3/S4 run against.
	var shellPID int
	paneID := strings.TrimSpace(env.Getenv("HERDR_PANE_ID"))
	if paneID == "" {
		termID := strings.TrimSpace(env.Getenv("HC_TERMINAL_ID"))
		if termID == "" {
			return failClosed("S1: HERDR_PANE_ID unset or empty")
		}
		if !hcTerminalIDPattern.MatchString(termID) {
			return failClosed(fmt.Sprintf("S1: HC_TERMINAL_ID=%s is not a terminal address (no ^t[0-9a-z]+$ match)", termID))
		}
		info, ok := env.QueryHcSocket(env.HcSocketPath, termID, env.Timeout)
		if !ok || info.Address != termID || info.PID <= 0 {
			return failClosed(fmt.Sprintf("S2: hc terminal.get for %s failed, timed out or mismatched", termID))
		}
		if info.Parent != "" {
			return failClosed(fmt.Sprintf("S1: HC_TERMINAL_ID=%s has parent=%s (subagent terminal)", termID, info.Parent))
		}
		shellPID = info.PID
		// Reuse paneID as the diagnostic label below; the value is the
		// terminal address.
		paneID = termID
	} else {
		if !strings.HasSuffix(paneID, ":p1") {
			return failClosed(fmt.Sprintf("S1: HERDR_PANE_ID=%s is not a main pane (no \":p1\" suffix)", paneID))
		}

		// S2: ask the running herdr server.
		pid, ok := env.QuerySocket(env.SocketPath, paneID, env.Timeout)
		if !ok || pid <= 0 {
			return failClosed(fmt.Sprintf("S2: herdr pane query for %s failed, timed out or mismatched", paneID))
		}
		shellPID = pid
	}

	// S3: the pane shell must be in this ancestry, and every hop's argv must be
	// readable — an empty cmdline fails closed, it must not slip past S4.
	chain := readAncestorChain(env.PID, identityAncestorMaxHops, env.ReadStat, env.ReadCmdline, env.ReadArgv, env.ReadEnviron)
	if len(chain) == 0 {
		return failClosed("S3: /proc ancestry for this process is unreadable")
	}
	for index := range chain {
		if len(chain[index].Argv) == 0 {
			chain[index].Argv = splitCmdlineTokens(chain[index].Cmdline)
		}
	}
	shellIndex := -1
	for index, entry := range chain {
		if entry.PID == shellPID {
			shellIndex = index
			break
		}
	}
	if shellIndex < 0 {
		return failClosed(fmt.Sprintf("S3: pane %s shell_pid=%d is not in this process ancestry [%s]", paneID, shellPID, describeAncestorChain(chain)))
	}
	for hop := 0; hop <= shellIndex; hop++ {
		entry := chain[hop]
		if len(entry.Argv) == 0 && strings.TrimSpace(entry.Cmdline) == "" {
			return failClosed(fmt.Sprintf("S3: ancestor pid=%d at hop=%d has an empty or unreadable cmdline", entry.PID, hop))
		}
	}

	// The negative-marker scan covers this process's own environment + bridged
	// argv (done above) and every ancestor hop 0..shellIndex-1: the environ and
	// cmdline of an ancestor can reveal the launch just as well.
	for hop := 0; hop < shellIndex; hop++ {
		entry := chain[hop]
		// Hop 0's own process environment is already checked above via env.Getenv;
		// its /proc environ adds nothing new but an injected Env is honoured.
		ancestorEnv := entry.Env
		if ancestorEnv == nil && hop == 0 {
			ancestorEnv = map[string]string{
				"CTXMODE_ROLE": env.Getenv("CTXMODE_ROLE"),
				"PI_SUBAGENT":  env.Getenv("PI_SUBAGENT"),
				"PI_ROLE":      env.Getenv("PI_ROLE"),
			}
		}
		if marker, hit := negativeEnvMarker(ancestorEnv); hit {
			return failClosed(fmt.Sprintf("S4: ancestor pid=%d at hop=%d has %s", entry.PID, hop, marker))
		}
		tokens := tokensOf(entry)
		if HasSubagentCliFlags(tokens) {
			cmd := entry.Cmdline
			if len(cmd) > 60 {
				cmd = cmd[:60]
			}
			return failClosed(fmt.Sprintf("S4: subagent CLI flag in ancestor pid=%d %q", entry.PID, cmd))
		}
	}

	if mcpMode {
		return resolveMCPHostTopology(chain, shellIndex, paneID, shellPID)
	}
	return resolveExtensionHostTopology(env, chain, shellIndex, paneID, shellPID)
}

// resolveMCPHostTopology is the MCP-mode S4: between jev-mcp and the pane
// shell, at most one host layer (agy/antigravity or pi/pig) is allowed and
// every other hop must be that host. Forbidden tool runtimes, a second host
// layer, or any unknown intermediate fail closed (jev-mcp.mjs behaviour).
func resolveMCPHostTopology(chain []AncestorProcess, shellIndex int, paneID string, shellPID int) (bool, string) {
	hostLayers := 0
	for hop := 1; hop < shellIndex; hop++ {
		entry := chain[hop]
		tokens := tokensOf(entry)
		cmd := entry.Cmdline
		if len(cmd) > 60 {
			cmd = cmd[:60]
		}
		if isAgyTokens(tokens) || isPiPigTokens(tokens) {
			hostLayers++
			if hostLayers > 1 {
				return failClosed(fmt.Sprintf("S4: nested second host runtime ancestor pid=%d %q between this process and pane shell", entry.PID, cmd))
			}
			continue
		}
		if isForbiddenTokens(tokens) {
			return failClosed(fmt.Sprintf("S4: agent/tool runtime ancestor pid=%d %q between this process and pane shell", entry.PID, cmd))
		}
		return failClosed(fmt.Sprintf("S4: unexpected intermediate ancestor pid=%d %q between this process and pane shell", entry.PID, cmd))
	}
	return false, fmt.Sprintf("S1-S4 hold: pane=%s shell_pid=%d hop=%d host_layers=%d", paneID, shellPID, shellIndex, hostLayers)
}

// isPiPigTokens reports whether tokens run pi or pig (directly or
// env/interpreter-wrapped) — the host-runtime half the MCP mode counts apart
// from agy/antigravity.
func isPiPigTokens(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	executable := executableTokenOf(tokens, 0)
	base := executable
	if slash := strings.LastIndex(executable, "/"); slash >= 0 {
		base = executable[slash+1:]
	}
	if base == "pi" || base == "pig" {
		return true
	}
	if isInterpreterBase(argv0Base(tokens)) {
		for index := 1; index < len(tokens); index++ {
			token := tokens[index]
			if token == "--" {
				if index+1 < len(tokens) {
					return isPiPigOrAgentRuntimeToken(tokens[index+1])
				}
				return false
			}
			if isModuleFlagToken(token) {
				if index+1 < len(tokens) {
					return isPiPigOrAgentRuntimeToken(tokens[index+1])
				}
				return false
			}
			if value, ok := moduleValueFromToken(token); ok {
				return isPiPigOrAgentRuntimeToken(value)
			}
			if inlineTextFlags[token] {
				return false
			}
			if flagsWithArgument[token] {
				index++
				continue
			}
			if strings.HasPrefix(token, "--") && strings.Contains(token, "=") {
				continue
			}
			if strings.HasPrefix(token, "-") {
				continue
			}
			return isPiPigOrAgentRuntimeToken(token)
		}
	}
	return agentRuntimePattern.MatchString(executable)
}

// isPiPigOrAgentRuntimeToken reports whether a script-path token names pi/pig
// (by basename) or a forbidden agent runtime path.
func isPiPigOrAgentRuntimeToken(token string) bool {
	tokenBase := token
	if slash := strings.LastIndex(token, "/"); slash >= 0 {
		tokenBase = token[slash+1:]
	}
	if tokenBase == "pi" || tokenBase == "pig" {
		return true
	}
	return agentRuntimePattern.MatchString(token)
}

// resolveExtensionHostTopology is the extension-mode host anchoring + S4: the
// host is the first pi/pig ancestor below the pane shell (subprocess form) or
// self (fused form), and no other agent/tool runtime may sit between hop 0
// and the shell except the host itself.
func resolveExtensionHostTopology(env IdentityEnv, chain []AncestorProcess, shellIndex int, paneID string, shellPID int) (bool, string) {
	subprocessForm, subprocessProof := detectSubprocessForm(env.Getenv, env.Environ)
	hostIndex := -1
	if subprocessForm {
		// The host is the first pi/pig (main-agent) runtime below the pane
		// shell; ctxmode/dispatch-agent/grok are never a legitimate host.
		for index := 0; index < shellIndex; index++ {
			if isPiPigTokens(tokensOf(chain[index])) {
				hostIndex = index
				break
			}
		}
		if hostIndex < 0 {
			return failClosed(fmt.Sprintf("S3: no agent-runtime ancestor for subprocess pid=%d below pane shell pid=%d (form: %s) [%s]",
				chain[0].PID, shellPID, subprocessProof, describeAncestorChain(chain)))
		}
	} else {
		if !isPiPigTokens(tokensOf(chain[0])) {
			return failClosed(fmt.Sprintf("S3: fused form but self pid=%d cmdline %q is not a pi/pig agent runtime [%s]",
				chain[0].PID, chain[0].Cmdline, describeAncestorChain(chain)))
		}
		hostIndex = 0
	}
	if shellIndex < hostIndex {
		return failClosed(fmt.Sprintf("S3: pane shell_pid=%d at hop=%d is below host pid=%d at hop=%d (shell is a descendant of this process's host)",
			shellPID, shellIndex, chain[hostIndex].PID, hostIndex))
	}
	if shellIndex == hostIndex {
		return failClosed(fmt.Sprintf("S3: pane shell_pid=%d at hop=%d is the same process as host pid=%d (expected a strict ancestor, got self)",
			shellPID, shellIndex, chain[hostIndex].PID))
	}

	// S4: no agent/tool runtime between hop 0 and the pane shell other than the
	// resolved host (pi/pig/ctxmode/dispatch-agent/grok/agy/antigravity,
	// interpreter-penetrated).
	for hop := 0; hop < shellIndex; hop++ {
		if hop == hostIndex {
			continue
		}
		entry := chain[hop]
		tokens := tokensOf(entry)
		if isForbiddenTokens(tokens) || isAgyTokens(tokens) || isPiPigTokens(tokens) {
			cmd := entry.Cmdline
			if len(cmd) > 60 {
				cmd = cmd[:60]
			}
			return failClosed(fmt.Sprintf("S4: agent runtime ancestor pid=%d %q between host and pane shell", entry.PID, cmd))
		}
	}

	return false, fmt.Sprintf("S1-S4 hold: pane=%s shell_pid=%d hop=%d", paneID, shellPID, shellIndex)
}

// resolveAgentIdentityWithEnv resolves against the given environment in
// extension mode (kept for the identity tests).
func resolveAgentIdentityWithEnv(env IdentityEnv) (bool, string) {
	return resolveIdentity(env, false)
}

// ResolveAgentIdentity resolves against the production environment in
// extension mode and returns (isSubagent, reason).
func ResolveAgentIdentity() (bool, string) {
	return resolveIdentity(defaultIdentityEnv(), false)
}

// ResolveAgentIdentityWithEnv is the exported extension-mode resolver so
// sibling packages and cross-extension tests can exercise the same verdict
// with an injected environment.
func ResolveAgentIdentityWithEnv(env IdentityEnv) (bool, string) {
	return resolveIdentity(env, false)
}

// ResolveMCPAgentIdentity resolves the fail-closed identity for the jev-mcp
// MCP server against the production environment (MCP mode).
func ResolveMCPAgentIdentity() (bool, string) {
	return resolveIdentity(defaultIdentityEnv(), true)
}

// ResolveMCPAgentIdentityWithEnv is the MCP-mode resolver with an injected
// environment (tests).
func ResolveMCPAgentIdentityWithEnv(env IdentityEnv) (bool, string) {
	return resolveIdentity(env, true)
}

// IsSubagentSession reports whether this session is a subagent, or cannot be
// proven to be the main agent (fail-closed).
func IsSubagentSession() bool {
	isSubagent, reason := ResolveAgentIdentity()
	if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
		status := "main"
		if isSubagent {
			status = "subagent"
		}
		fmt.Fprintf(os.Stderr, "[subagent-identity] %s: %s\n", status, reason)
	}
	return isSubagent
}
