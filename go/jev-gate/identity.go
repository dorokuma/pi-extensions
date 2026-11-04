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
// The -i/-p/--prompt argument tokens are stripped before the argv scan so a
// main-agent prompt that merely mentions a dispatch path is not a marker.
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

// checkExecutableTokens reports whether the argv resolves — through env
// wrappers and interpreters — to a forbidden agent/tool runtime.
func checkExecutableTokens(tokens []string, depth int) bool {
	if depth > 8 || len(tokens) == 0 {
		return false
	}
	base := argv0Base(tokens)

	if base == "env" {
		// env KEY=VAL assignments and env's own flags are skipped before
		// recursing into the wrapped executable; "--" ends env option parsing.
		index := 1
		afterDashDash := false
		for index < len(tokens) {
			token := tokens[index]
			if isEnvAssignment(token) {
				index++
				continue
			}
			if !afterDashDash {
				if token == "--" {
					afterDashDash = true
					index++
					continue
				}
				if !strings.HasPrefix(token, "-") {
					break
				}
				// env -S/--split-string: the value is re-split and prepended.
				if token == "-S" || token == "--split-string" {
					splitValue := ""
					if index+1 < len(tokens) {
						splitValue = tokens[index+1]
					}
					rest := append(strings.Fields(splitValue), tokens[index+2:]...)
					if len(rest) == 0 {
						return false
					}
					return checkExecutableTokens(rest, depth+1)
				}
				if inline := inlineSplitValue(token); inline != "" {
					rest := append(strings.Fields(inline), tokens[index+1:]...)
					if len(rest) == 0 {
						return false
					}
					return checkExecutableTokens(rest, depth+1)
				}
				// -u/--unset and -C/--chdir consume their own argument token.
				if token == "-u" || token == "--unset" || token == "-C" || token == "--chdir" {
					index += 2
					continue
				}
				if strings.HasPrefix(token, "--unset=") || strings.HasPrefix(token, "--chdir=") ||
					(strings.HasPrefix(token, "-u") && len(token) > 2) ||
					(strings.HasPrefix(token, "-C") && len(token) > 2) {
					index++
					continue
				}
				// Any other env flag is boolean and takes no argument.
				index++
				continue
			}
			break
		}
		if index >= len(tokens) {
			return false
		}
		return checkExecutableTokens(tokens[index:], depth+1)
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

// inlineSplitValue extracts the embedded string of env's joined
// --split-string=... / -S... forms ("" when the token is not one).
func inlineSplitValue(token string) string {
	if strings.HasPrefix(token, "--split-string=") {
		return token[len("--split-string="):]
	}
	if strings.HasPrefix(token, "-S") && len(token) > 2 {
		return token[2:]
	}
	return ""
}

// executableTokenOf unwraps env wrappers (KEY=VAL, flags, -S/--split-string,
// -- terminator) and returns the token of the executable actually run.
func executableTokenOf(tokens []string, depth int) string {
	if depth > 8 || len(tokens) == 0 {
		return ""
	}
	base := argv0Base(tokens)
	if base == "env" {
		index := 1
		for index < len(tokens) {
			token := tokens[index]
			if token == "--" {
				index++
				break
			}
			if isEnvAssignment(token) || !strings.HasPrefix(token, "-") {
				break
			}
			if token == "-S" || token == "--split-string" {
				splitValue := ""
				if index+1 < len(tokens) {
					splitValue = tokens[index+1]
				}
				rest := append(strings.Fields(splitValue), tokens[index+2:]...)
				return executableTokenOf(rest, depth+1)
			}
			if token == "-u" || token == "--unset" || token == "-C" || token == "--chdir" {
				index += 2
				continue
			}
			index++
		}
		return executableTokenOf(tokens[index:], depth+1)
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

// StripPromptArgTokens removes -i, -p, --prompt and the one argument token
// that follows each, plus their = forms: a main agent's prompt text may
// legitimately contain "--agent" or "/tmp/dispatch-..." as task description,
// which is not launch evidence. Exported for the MCP layer and tests.
func StripPromptArgTokens(tokens []string) []string {
	structural := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token == "-i" || token == "-p" || token == "--prompt" {
			index++
			continue
		}
		if strings.HasPrefix(token, "-i=") || strings.HasPrefix(token, "-p=") ||
			strings.HasPrefix(token, "--prompt=") {
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
	ArgvCache   *ArgvCache
}

// defaultIdentityEnv is the production identity environment.
func defaultIdentityEnv() IdentityEnv {
	return IdentityEnv{
		Getenv:      os.Getenv,
		Args:        os.Args,
		PID:         os.Getpid(),
		SocketPath:  herdrSocketFile,
		Timeout:     identityQueryTimeout,
		ReadStat:    readProcPpid,
		ReadCmdline: readProcCmdline,
		ReadArgv:    readProcArgv,
		ReadEnviron: readProcEnviron,
		Environ:     os.Environ,
		QuerySocket: queryPaneShellPid,
		ArgvCache:   defaultArgvCache,
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

	// S1: the pane id is both the herdr lookup key and a necessary condition —
	// a main agent's pane id always ends with ":p1"; anything else cannot be
	// the main pane and is rejected before the socket query.
	paneID := strings.TrimSpace(env.Getenv("HERDR_PANE_ID"))
	if paneID == "" {
		return failClosed("S1: HERDR_PANE_ID unset or empty")
	}
	if !strings.HasSuffix(paneID, ":p1") {
		return failClosed(fmt.Sprintf("S1: HERDR_PANE_ID=%s is not a main pane (no \":p1\" suffix)", paneID))
	}

	// S2: ask the running herdr server.
	shellPID, ok := env.QuerySocket(env.SocketPath, paneID, env.Timeout)
	if !ok || shellPID <= 0 {
		return failClosed(fmt.Sprintf("S2: herdr pane query for %s failed, timed out or mismatched", paneID))
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
