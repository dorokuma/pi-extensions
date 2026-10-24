package jevgate

// ===== SUBAGENT-IDENTITY (fail-closed) =====
//
// Port of the reviewed Go block of go/exclude-tools-guard/extension.go, which
// the TS authority shares byte-for-byte between exclude-tools-guard.ts and
// jev-gate.ts (TS: `SUBAGENT-IDENTITY-BEGIN .. END`). The semantics are the ones
// already accepted for the Go family; README.md lists where they differ from
// the TS block.
//
// A session is the main agent only when every positive proof holds; any proof
// that is missing, unverifiable, failed or timed out resolves as subagent:
//   S1: HERDR_PANE_ID is non-empty. It is only the lookup key for the herdr
//       query — pane naming is deliberately NOT an identity criterion (the TS
//       version also required the ":p1" suffix; see README).
//   S2: the running herdr server answers the pane query with a matching
//       shell_pid > 0. The socket path is a hard-coded constant, never read from
//       HERDR_SOCKET_PATH, which would be one more forgeable variable.
//   S3: the shell_pid is a strict ancestor of the host process. The host is the
//       first agent-runtime ancestor of an out-of-process extension, or the
//       process itself when the extension runs fused inside the host.
//   S4: no other agent runtime sits between the host and the pane shell.
//
// Negative launch markers can only push the verdict toward subagent (there is no
// "fake it into main" direction): PI_SUBAGENT=1, a non-empty PI_ROLE, and the
// argv markers "--name role-", "/tmp/herdr-role-sessions", "/tmp/dispatch-".
//
// Residual surface (known, not covered here): anything that can put another
// process into pane p1's shell makes the tree look like "shell starts an agent
// directly". Closing that needs herdr to inject a non-inheritable credential at
// pane start.

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

// readProcCmdline reads /proc/<pid>/cmdline (NUL separated) as one line.
func readProcCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	parts := strings.Split(string(data), "\x00")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

// readAncestorChain walks from pid outward, stopping at the first unreadable hop.
func readAncestorChain(pid, maxHops int, readStat func(int) (int, bool), readCmdline func(int) string) []AncestorProcess {
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
		chain = append(chain, AncestorProcess{PID: current, PPID: ppid, Cmdline: readCmdline(current)})
		if ppid == 0 || ppid == current {
			break
		}
		current = ppid
	}
	return chain
}

var agentRuntimePattern = regexp.MustCompile(`pi-coding-agent|bundle/cli\.js`)

// isAgentRuntimeCmdline reports whether a cmdline belongs to an agent runtime
// (pi, pig, ctxmode or a wrapper).
func isAgentRuntimeCmdline(cmdline string) bool {
	if cmdline == "" {
		return false
	}
	argv0 := strings.Split(cmdline, " ")[0]
	base := argv0
	if slash := strings.LastIndex(argv0, "/"); slash >= 0 {
		base = argv0[slash+1:]
	}
	if base == "pi" || base == "pig" || base == "ctxmode" {
		return true
	}
	return agentRuntimePattern.MatchString(cmdline)
}

// ArgvCache keeps the argv read from PIG_HARNESS_ARGV_FILE across turns of one
// process (the variable is unset after the first read).
type ArgvCache struct {
	// mu guards the cached argv itself.
	mu sync.Mutex
	// loadMu serialises one whole bridge load (cache probe, environment read,
	// file read, unset, cache fill): without it a concurrent caller can observe
	// the variable already unset while the cache is not yet filled, and would
	// silently fall back to the base argv, losing every negative launch marker.
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
// PIG_HARNESS_ARGV_FILE at the harness CLI arguments; the file is unset after
// being read so the path cannot leak to child processes.
//
// Fail-closed: a set but unreadable/unparseable file is an error, so the caller
// resolves as subagent instead of silently losing the argv markers.
func loadEffectiveArgs(getenv func(string) string, unsetenv func(string) error, baseArgs []string, cache *ArgvCache) ([]string, error) {
	if cache == nil {
		cache = defaultArgvCache
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if unsetenv == nil {
		unsetenv = os.Unsetenv
	}
	// The whole bridge runs as one critical section, including the cache probe: a
	// caller that probes outside the lock could read PIG_HARNESS_ARGV_FILE after a
	// first caller unset it but before that caller cached the argv, and would
	// return the base args (empty for a PiG subprocess) as if no bridge existed.
	cache.loadMu.Lock()
	defer cache.loadMu.Unlock()
	if cached, ok := cache.Get(); ok {
		return mergeArgs(baseArgs, cached), nil
	}
	argvFile := strings.TrimSpace(getenv("PIG_HARNESS_ARGV_FILE"))
	if argvFile == "" {
		return baseArgs, nil
	}
	defer func() { _ = unsetenv("PIG_HARNESS_ARGV_FILE") }()

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

// subagentLaunchMarker returns the first negative marker that matched.
func subagentLaunchMarker(getenv func(string) string, args []string) (string, bool) {
	if getenv("PI_SUBAGENT") == "1" {
		return "PI_SUBAGENT=1", true
	}
	if getenv("PI_ROLE") != "" {
		return "PI_ROLE is set", true
	}
	argv := strings.Join(args, " ")
	if strings.Contains(argv, "--name role-") {
		return "argv has --name role-", true
	}
	if strings.Contains(argv, "/tmp/herdr-role-sessions") {
		return "argv has /tmp/herdr-role-sessions", true
	}
	if strings.Contains(argv, "/tmp/dispatch-") {
		return "argv has /tmp/dispatch-", true
	}
	return "", false
}

// IdentityEnv is every input of one identity resolution. Tests fill it in;
// defaultIdentityEnv is the production value.
type IdentityEnv struct {
	Getenv      func(string) string
	Unsetenv    func(string) error
	Args        []string
	PID         int
	SocketPath  string
	Timeout     time.Duration
	ReadStat    func(int) (int, bool)
	ReadCmdline func(int) string
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
		Unsetenv:    os.Unsetenv,
		Args:        os.Args,
		PID:         os.Getpid(),
		SocketPath:  herdrSocketFile,
		Timeout:     identityQueryTimeout,
		ReadStat:    readProcPpid,
		ReadCmdline: readProcCmdline,
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

// resolveAgentIdentityWithEnv performs the fail-closed resolution and returns
// (isSubagent, reason).
func resolveAgentIdentityWithEnv(env IdentityEnv) (bool, string) {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Unsetenv == nil {
		env.Unsetenv = os.Unsetenv
	}
	if env.ReadStat == nil {
		env.ReadStat = readProcPpid
	}
	if env.ReadCmdline == nil {
		env.ReadCmdline = readProcCmdline
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

	effectiveArgs, err := loadEffectiveArgs(env.Getenv, env.Unsetenv, env.Args, env.ArgvCache)
	if err != nil {
		return true, fmt.Sprintf("argv bridge failed: %v", err)
	}
	if marker, hit := subagentLaunchMarker(env.Getenv, effectiveArgs); hit {
		return true, "launch marker: " + marker
	}

	// S1: the pane id is the herdr lookup key, not an identity criterion.
	paneID := strings.TrimSpace(env.Getenv("HERDR_PANE_ID"))
	if paneID == "" {
		return failClosed("S1: HERDR_PANE_ID unset or empty")
	}

	// S2: ask the running herdr server.
	shellPID, ok := env.QuerySocket(env.SocketPath, paneID, env.Timeout)
	if !ok || shellPID <= 0 {
		return failClosed(fmt.Sprintf("S2: herdr pane query for %s failed, timed out or mismatched", paneID))
	}

	// S3a: the pane shell must be in this ancestry.
	chain := readAncestorChain(env.PID, identityAncestorMaxHops, env.ReadStat, env.ReadCmdline)
	if len(chain) == 0 {
		return failClosed("S3: /proc ancestry for this process is unreadable")
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

	// S3b: anchor the host on process topology. Out of process, the first
	// agent-runtime ancestor is the host; fused, the process itself is the host.
	subprocessForm, subprocessProof := detectSubprocessForm(env.Getenv, env.Environ)
	hostIndex := -1
	if subprocessForm {
		for index := 0; index < shellIndex; index++ {
			if isAgentRuntimeCmdline(chain[index].Cmdline) {
				hostIndex = index
				break
			}
		}
		if hostIndex < 0 {
			return failClosed(fmt.Sprintf("S3: no agent-runtime ancestor for subprocess pid=%d below pane shell pid=%d (form: %s) [%s]",
				chain[0].PID, shellPID, subprocessProof, describeAncestorChain(chain)))
		}
	} else {
		if !isAgentRuntimeCmdline(chain[0].Cmdline) {
			return failClosed(fmt.Sprintf("S3: fused form but self pid=%d cmdline %q is not an agent runtime [%s]",
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

	// S4: no agent runtime between host and pane shell.
	for hop := hostIndex + 1; hop < shellIndex; hop++ {
		ancestor := chain[hop]
		if isAgentRuntimeCmdline(ancestor.Cmdline) {
			cmd := ancestor.Cmdline
			if len(cmd) > 60 {
				cmd = cmd[:60]
			}
			return failClosed(fmt.Sprintf("S4: agent runtime ancestor pid=%d %q between host and pane shell", ancestor.PID, cmd))
		}
	}

	return false, fmt.Sprintf("S1-S4 hold: pane=%s shell_pid=%d hop=%d", paneID, shellPID, shellIndex)
}

// resolveAgentIdentity resolves against the production environment.
func resolveAgentIdentity() (bool, string) {
	return resolveAgentIdentityWithEnv(defaultIdentityEnv())
}

// IsSubagentSession reports whether this session is a subagent, or cannot be
// proven to be the main agent (fail-closed).
func IsSubagentSession() bool {
	isSubagent, reason := resolveAgentIdentity()
	if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
		status := "main"
		if isSubagent {
			status = "subagent"
		}
		fmt.Fprintf(os.Stderr, "[subagent-identity] %s: %s\n", status, reason)
	}
	return isSubagent
}
