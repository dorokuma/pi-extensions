// Package excludetoolsguard implements the exclude-tools-guard extension for PiG in Go.
//
// Background & Motivation:
// Global tool exclusion guard:
// Ensures that in all sessions (interactive TUI, --print, -p, --mode json, RPC, session resume),
// tools like grep, find, ls, bash are strictly excluded, enforcing use of CodeGraph, ctx_fs, and ctx_run.
//
// In subagent sessions (C1), additionally excludes main-agent dedicated tools jev_check and ask_user_question,
// and clears main-agent dedicated APPEND_SYSTEM.md (<addendum> block):
//
//	Although dispatch layers use --exclude-tools and --append-system-prompt '', side channels like
//	`herdr agent start --kind pi` or running bin/pi directly omit these flags, so this extension acts as
//	a runtime fail-closed guard based on session identity.
//
// Alignment & Differences with TypeScript Implementation:
// 1. Identity Resolution (SUBAGENT-IDENTITY block):
//   - TS: Connects to /root/.config/herdr/herdr.sock using node:net, parses JSON-RPC reply, inspects /proc/<pid>/stat and /proc/<pid>/cmdline.
//   - Go: Implements unix socket protocol and kernel inspection using Go standard library net, os, and bufio (Linux/macOS only).
//     Zero-configuration fail-closed semantics: any failure, timeout, or missing proof resolves as subagent.
//     Host anchoring is derived from the process topology only: the subprocess form is recognised from
//     the extension transport environment (PIG_EXT_SOCKET for an isolated process, a PIG_EXT_SOCKET_<NAME>
//     entry for every packed cell member, PIG_EXT_PACKED_CELL / PIG_EXT_ACTIVE_MEMBERS for the cell
//     itself), and the host is the first agent-runtime ancestor above self (subprocess) or self itself
//     (fused). Pane naming (HERDR_PANE_ID) is never a criterion; it is only the herdr lookup key and
//     auxiliary evidence in the reason string.
//
// 2. Tool Exclusion:
//   - TS: EXCLUDED_TOOLS = Set(["grep", "find", "ls", "bash"]), SUBAGENT_EXCLUDED_TOOLS = Set(["jev_check", "ask_user_question"]).
//     Filters currentActive = pi.getActiveTools(); if length changed, calls pi.setActiveTools(filtered).
//   - Go: FilterActiveTools pure function filters slice while strictly preserving relative ordering.
//     EnforceToolExclusions only calls ctx.SetActiveTools when the active set actually changes ("only removes, never adds").
//
// 3. System Prompt Sanitization:
//   - TS: On before_agent_start, if isSubagent, clears appendSystemPrompt and replaces <addendum> block in forceSystemPrompt.
//   - Go: CleanPromptOptions and StripAddendumBlock reproduce the exact replaceAll semantics:
//     first removes "\n\n<addendum>...\n</addendum>", then "<addendum>...\n</addendum>".
//
// 4. Hook Registration:
//   - TS: pi.on("session_start", ...), pi.on("before_agent_start", ...)
//   - Go: e.OnEvent(sdk.EventSessionStart, ...), e.OnEvent(sdk.EventBeforeAgentStart, ...)
package excludetoolsguard

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

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// ===== SUBAGENT-IDENTITY CONSTANTS =====

// HerdrSocketFile is the unix domain socket path to herdr server.
// Hardcoded constant: HERDR_SOCKET_PATH is not read from environment to prevent spoofing.
const HerdrSocketFile = "/root/.config/herdr/herdr.sock"

// IdentityQueryTimeout is the single socket query timeout (1500ms).
// Queries that time out are treated as subagents (fail-closed) and logged once
// (deduplicated via sync.Once); the budget is deliberately NOT extended and no
// blocking retry layer is stacked on top.
const IdentityQueryTimeout = 1500 * time.Millisecond

// IdentityAncestorMaxHops is the maximum traversal depth in /proc ancestry chain (defense against cycles/malformed stat).
const IdentityAncestorMaxHops = 32

// Extension transport environment (see ref/PiG coding/extension/host/subprocess/doc.go:11-12).
// An isolated extension process receives PIG_EXT_SOCKET; a packed cell process receives one
// PIG_EXT_SOCKET_<NAME> per member instead and never receives PIG_EXT_SOCKET.
const (
	// socketEnvName is the isolated-extension transport variable.
	socketEnvName = "PIG_EXT_SOCKET"
	// socketEnvNamePrefix prefixes the per-member socket variable of a packed cell
	// (runtimecell.SocketEnvName, go_packed.go:844-846: "PIG_EXT_SOCKET_" + the name with
	// non-[A-Za-z0-9_] upper-cased to "_"; this component's key is
	// PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD).
	socketEnvNamePrefix = "PIG_EXT_SOCKET_"
	// packedCellEnvName is the packed cell key, written only into a packed cell process
	// (subprocess/packed_go.go:495).
	packedCellEnvName = "PIG_EXT_PACKED_CELL"
	// activeMembersEnvName lists the packed cell's members (subprocess/packed_go.go:497).
	activeMembersEnvName = "PIG_EXT_ACTIVE_MEMBERS"
)

// ===== EXCLUDED TOOLS CONSTANTS =====

// excludedTools defines tools excluded in all sessions (main & subagent).
var excludedTools = map[string]bool{
	"grep": true,
	"find": true,
	"ls":   true,
	"bash": true,
}

// subagentExcludedTools defines tools additionally excluded in subagent sessions.
// jev_check is the gating signal for main agents; ask_user_question is the user inquiry channel.
var subagentExcludedTools = map[string]bool{
	"jev_check":         true,
	"ask_user_question": true,
}

// ExcludedTools returns a read-only copy of tools excluded in all sessions.
func ExcludedTools() map[string]bool {
	cp := make(map[string]bool, len(excludedTools))
	for k, v := range excludedTools {
		cp[k] = v
	}
	return cp
}

// SubagentExcludedTools returns a read-only copy of tools additionally excluded in subagent sessions.
func SubagentExcludedTools() map[string]bool {
	cp := make(map[string]bool, len(subagentExcludedTools))
	for k, v := range subagentExcludedTools {
		cp[k] = v
	}
	return cp
}

// ===== TOOL FILTERING =====

// FilterActiveTools filters active tool names according to whether the session is a subagent.
// Main agent excludes: grep, find, ls, bash.
// Subagent additionally excludes: jev_check, ask_user_question.
// Preserves the original ordering of remaining tools ("only removes, never adds").
// Returns the filtered list and a boolean indicating whether the active list changed.
func FilterActiveTools(currentActive []string, isSubagent bool) ([]string, bool) {
	filtered := make([]string, 0, len(currentActive))
	changed := false
	for _, tool := range currentActive {
		if excludedTools[tool] {
			changed = true
			continue
		}
		if isSubagent && subagentExcludedTools[tool] {
			changed = true
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered, changed
}

// ToolManager represents the minimal tool querying and modification interface from sdk.Context.
type ToolManager interface {
	GetActiveTools() ([]string, error)
	SetActiveTools([]string)
}

// EnforceToolExclusions retrieves current active tools from ctx, filters them,
// and invokes SetActiveTools only if the active set changed.
// "No change means SetActiveTools is not called."
// If GetActiveTools fails, it does NOT pretend filtering succeeded, logs a diagnostic
// to stderr when PI_IDENTITY_DEBUG=1, and returns the error without panicking.
func EnforceToolExclusions(ctx ToolManager, isSubagent bool) (bool, error) {
	currentActive, err := ctx.GetActiveTools()
	if err != nil {
		if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "[subagent-identity] warning: GetActiveTools failed: %v\n", err)
		}
		return isSubagent, err
	}
	filtered, changed := FilterActiveTools(currentActive, isSubagent)
	if changed {
		ctx.SetActiveTools(filtered)
		if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "[subagent-identity] debug: SetActiveTools applied: %v (excluded %d tools)\n", filtered, len(currentActive)-len(filtered))
		}
	}
	return isSubagent, nil
}

// ===== SYSTEM PROMPT SANITIZATION =====

// StripAddendumBlock removes <addendum>\n...\n</addendum> blocks from forceSystemPrompt.
// Matches TS replaceAll semantics:
// first replaces "\n\n<addendum>...\n</addendum>" with ""
// then replaces "<addendum>...\n</addendum>" with ""
func StripAddendumBlock(forcePrompt, leakedAppend string) string {
	if leakedAppend == "" || forcePrompt == "" {
		return forcePrompt
	}
	addendumBlock := fmt.Sprintf("<addendum>\n%s\n</addendum>", leakedAppend)
	withNewlines := "\n\n" + addendumBlock
	res := strings.ReplaceAll(forcePrompt, withNewlines, "")
	res = strings.ReplaceAll(res, addendumBlock, "")
	return res
}

// addendumSectionName is the system prompt section Pi builds from the appended text
// (ref/PiG internal/codingagent/prompts/coding.go:124-131). A custom section of this name is
// wrapped in the same <addendum>...</addendum> tag when the prompt renders.
const addendumSectionName = "addendum"

// DeleteAddendumSection removes the "addendum" entry from systemPromptOptions["sections"].
//
// RenderSystemPrompt builds the base sections from appendSystemPrompt and then overlays the
// custom sections (inproc/runner.go:2569-2582 -> prompts.ApplyCustomSystemPromptSections, which
// replaces or appends a non-empty custom section of the same name). Clearing appendSystemPrompt
// alone therefore does not remove an addendum that also exists as a custom section: it would be
// re-wrapped and emitted again. This closes that second path.
//
// The SDK hands before_agent_start a *sdk.SystemPromptSections (sdk.preparePromptOptions), so
// only that shape is mutated; any other shape is left untouched rather than guessed at.
// Returns true when a section was removed.
func DeleteAddendumSection(options map[string]any) bool {
	if options == nil {
		return false
	}
	sections, ok := options["sections"].(*sdk.SystemPromptSections)
	if !ok || sections == nil {
		return false
	}
	before := len(*sections)
	sections.Delete(addendumSectionName)
	return len(*sections) != before
}

// CleanPromptOptions cleans systemPromptOptions for subagent sessions.
//  1. Clears appendSystemPrompt (sets to "").
//  2. Deletes the addendum custom section, which the prompt renderer would otherwise re-wrap
//     and emit on its own.
//  3. If leaked appendSystemPrompt was non-empty and forceSystemPrompt is present,
//     strips the <addendum> block from forceSystemPrompt.
//
// Returns true if any modification was performed.
func CleanPromptOptions(options map[string]any) bool {
	if options == nil {
		return false
	}
	leakedAppend, _ := options["appendSystemPrompt"].(string)
	options["appendSystemPrompt"] = ""

	modified := (leakedAppend != "")
	if DeleteAddendumSection(options) {
		modified = true
	}
	if leakedAppend != "" {
		if rawForce, ok := options["forceSystemPrompt"]; ok && rawForce != nil {
			if forceStr, isStr := rawForce.(string); isStr {
				cleaned := StripAddendumBlock(forceStr, leakedAppend)
				if cleaned != forceStr {
					options["forceSystemPrompt"] = cleaned
					modified = true
				}
			}
		}
	}
	return modified
}

// ===== SUBAGENT IDENTITY RESOLUTION (fail-closed) =====

// AncestorProcess captures process metadata along the /proc ancestry chain.
type AncestorProcess struct {
	PID     int
	PPID    int
	Cmdline string
}

// QueryPaneShellPid queries the running herdr server via unix domain socket for pane process info.
// Fails closed (returns 0, false) on any network error, timeout, malformed JSON, mismatched pane_id, or shell_pid <= 0.
// Hard timeout: total query duration is strictly bounded by timeout (1500ms) measured from function entrance.
// Note: only unix domain socket is supported (Linux/macOS).
func QueryPaneShellPid(socketPath, paneID string, timeout time.Duration) (int, bool) {
	start := time.Now()
	deadline := start.Add(timeout)

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

	req := map[string]any{
		"id":     "pi-identity",
		"method": "pane.process_info",
		"params": map[string]any{
			"pane_id": paneID,
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return 0, false
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
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
	pID, ok := reply.Result.ProcessInfo.PaneID.(string)
	if !ok || pID != paneID {
		return 0, false
	}

	var shellPID int
	switch v := reply.Result.ProcessInfo.ShellPID.(type) {
	case float64:
		shellPID = int(v)
		if float64(shellPID) != v {
			return 0, false
		}
	case int:
		shellPID = v
	default:
		return 0, false
	}

	if shellPID <= 0 {
		return 0, false
	}
	return shellPID, true
}

// ReadProcPpid reads /proc/<pid>/stat and extracts ppid from after the final ')'.
// Returns (0, false) on missing file, unparseable stat, or negative ppid.
func ReadProcPpid(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	stat := string(data)
	closeIdx := strings.LastIndex(stat, ")")
	if closeIdx < 0 || closeIdx+1 >= len(stat) {
		return 0, false
	}
	rest := strings.TrimSpace(stat[closeIdx+1:])
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0, false
	}
	return ppid, true
}

// ReadProcCmdline reads /proc/<pid>/cmdline (NUL-separated), returning non-empty parts joined by space.
func ReadProcCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	parts := strings.Split(string(data), "\x00")
	var filtered []string
	for _, p := range parts {
		if len(p) > 0 {
			filtered = append(filtered, p)
		}
	}
	return strings.Join(filtered, " ")
}

// ReadAncestorChain traces process ancestry starting from pid up to maxHops levels.
func ReadAncestorChain(pid int, maxHops int, readStat func(int) (int, bool), readCmdline func(int) string) []AncestorProcess {
	var chain []AncestorProcess
	seen := make(map[int]bool)
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
		cmdline := readCmdline(current)
		chain = append(chain, AncestorProcess{PID: current, PPID: ppid, Cmdline: cmdline})
		if ppid == 0 || ppid == current {
			break
		}
		current = ppid
	}
	return chain
}

var reAgentRuntime = regexp.MustCompile(`pi-coding-agent|bundle/cli\.js`)

// IsAgentRuntimeCmdline determines if a cmdline indicates an agent runtime process (pi, pig, ctxmode, or wrapper).
func IsAgentRuntimeCmdline(cmdline string) bool {
	if cmdline == "" {
		return false
	}
	parts := strings.Split(cmdline, " ")
	argv0 := parts[0]
	lastSlash := strings.LastIndex(argv0, "/")
	base := argv0
	if lastSlash >= 0 {
		base = argv0[lastSlash+1:]
	}
	if base == "pi" || base == "pig" || base == "ctxmode" {
		return true
	}
	return reAgentRuntime.MatchString(cmdline)
}

// ArgvCache caches parsed arguments from PIG_HARNESS_ARGV_FILE across turns within a session.
type ArgvCache struct {
	mu       sync.Mutex
	cached   bool
	fileArgs []string
}

// Reset clears the cached arguments.
func (c *ArgvCache) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = false
	c.fileArgs = nil
}

// Get returns the cached file arguments and true if previously cached.
func (c *ArgvCache) Get() ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.cached {
		return nil, false
	}
	res := make([]string, len(c.fileArgs))
	copy(res, c.fileArgs)
	return res, true
}

// Set stores the file arguments into cache.
func (c *ArgvCache) Set(args []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = true
	c.fileArgs = make([]string, len(args))
	copy(c.fileArgs, args)
}

var (
	defaultArgvCacheMu sync.Mutex
	defaultArgvCache   = &ArgvCache{}
)

// ResetDefaultArgvCache resets the package-level default argv cache for testing purposes.
func ResetDefaultArgvCache() {
	defaultArgvCacheMu.Lock()
	defer defaultArgvCacheMu.Unlock()
	defaultArgvCache.Reset()
}

// LoadEffectiveArgs merges baseArgs (typically os.Args) with arguments read from PIG_HARNESS_ARGV_FILE.
// In PiG, Go extension subprocesses are launched with empty arguments (only the binary path);
// PiG points PIG_HARNESS_ARGV_FILE to a JSON array of harness CLI arguments (aligned with
// runtime-node/process-identity.mjs:21-22 and harness_identity.go:24).
//
// Fail-closed policy (M2):
// If PIG_HARNESS_ARGV_FILE is set but the file cannot be read or contains invalid JSON,
// it returns an error so caller fails closed to subagent (defense against attacker tampering
// or suppressing argv-based negative markers like --name role-).
// Only when PIG_HARNESS_ARGV_FILE is completely unset/empty does it safely fall back to baseArgs.
//
// Post-use cleanup:
// After reading, unsetenv is invoked to delete PIG_HARNESS_ARGV_FILE from the environment
// (matching runtime-node/process-identity.mjs:22 `delete process.env.PIG_HARNESS_ARGV_FILE`),
// preventing the path from leaking to processes spawned by the extension.
//
// Caching across turns:
// Successful parse results are stored in the ArgvCache so subsequent turns within the same process
// (where PIG_HARNESS_ARGV_FILE has been unset) still retain effective arguments.
// Three states are distinguished deliberately:
//  1. env unset and nothing cached (legitimate fallback to os.Args) - NOT cached, so a later turn
//     that does see an env var still reads it.
//  2. env set and parse succeeded - cached; later turns reuse it after the unsetenv cleanup.
//  3. env set but read/parse failed - error is returned and the failure is NOT cached, so
//     fail-closed is re-evaluated on every call instead of being frozen into "always subagent".
func LoadEffectiveArgs(getenv func(string) string, unsetenv func(string) error, baseArgs []string) ([]string, error) {
	return LoadEffectiveArgsWithCache(getenv, unsetenv, baseArgs, defaultArgvCache)
}

// LoadEffectiveArgsWithCache merges baseArgs with arguments read from PIG_HARNESS_ARGV_FILE,
// utilizing the provided ArgvCache (or default package cache if nil).
func LoadEffectiveArgsWithCache(getenv func(string) string, unsetenv func(string) error, baseArgs []string, cache *ArgvCache) ([]string, error) {
	if cache == nil {
		cache = defaultArgvCache
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if unsetenv == nil {
		unsetenv = os.Unsetenv
	}
	argvFile := strings.TrimSpace(getenv("PIG_HARNESS_ARGV_FILE"))
	if argvFile == "" {
		if cachedArgs, ok := cache.Get(); ok {
			return mergeArgs(baseArgs, cachedArgs), nil
		}
		return baseArgs, nil
	}

	defer func() {
		_ = unsetenv("PIG_HARNESS_ARGV_FILE")
	}()

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

func mergeArgs(baseArgs, fileArgs []string) []string {
	seen := make(map[string]bool, len(baseArgs)+len(fileArgs))
	combined := make([]string, 0, len(baseArgs)+len(fileArgs))
	for _, a := range baseArgs {
		if !seen[a] {
			seen[a] = true
			combined = append(combined, a)
		}
	}
	for _, a := range fileArgs {
		if !seen[a] {
			seen[a] = true
			combined = append(combined, a)
		}
	}
	return combined
}

// SubagentLaunchMarker inspects environment and argv for negative markers.
// If any matches, returns the marker description and true (immediately subagent).
func SubagentLaunchMarker(getenv func(string) string, args []string) (string, bool) {
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

// IdentityEnv encapsulates environmental inputs for testing and production identity resolution.
type IdentityEnv struct {
	Getenv      func(string) string
	Unsetenv    func(string) error
	Args        []string
	PID         int
	SocketPath  string
	Timeout     time.Duration
	ReadStat    func(int) (int, bool)
	ReadCmdline func(int) string
	// Environ lists the process environment as "KEY=VALUE" entries. It is only used to
	// enumerate PIG_EXT_SOCKET_<NAME> keys, whose names are not known in advance; every
	// value is re-read through Getenv, so an entry listed but empty is not proof.
	Environ     func() []string
	QuerySocket func(socketPath, paneID string, timeout time.Duration) (int, bool)
	ArgvCache   *ArgvCache
}

// DetectSubprocessForm reports whether this process runs the extension out of process
// (isolated subprocess or packed cell member) and which environment entry proves it.
//
// PiG transports an extension out of process in three shapes, and only the first sets
// PIG_EXT_SOCKET:
//   - isolated process: PIG_EXT_SOCKET and PIG_EXT_NAME (subprocess/host.go:3189, doc.go:11);
//   - packed cell member: one PIG_EXT_SOCKET_<NAME> per member, no PIG_EXT_SOCKET at all
//     (subprocess/packed_go.go:422, doc.go:11-12). A Go extension under PiG runs as a packed
//     cell member by default, so this is the ordinary production shape;
//   - packed cell process: PIG_EXT_PACKED_CELL and PIG_EXT_ACTIVE_MEMBERS
//     (subprocess/packed_go.go:495-497).
//
// Keys are matched by prefix against the enumerated environment and then re-validated through
// getenv: a listed key whose value is empty ("PIG_EXT_SOCKET_X=") is not proof, and the key
// is queried exactly as enumerated (Linux environment names are case-sensitive, so no case
// folding is applied).
func DetectSubprocessForm(getenv func(string) string, environ func() []string) (bool, string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if strings.TrimSpace(getenv(socketEnvName)) != "" {
		return true, socketEnvName
	}
	if strings.TrimSpace(getenv(packedCellEnvName)) != "" {
		return true, packedCellEnvName
	}
	if strings.TrimSpace(getenv(activeMembersEnvName)) != "" {
		return true, activeMembersEnvName
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

// DefaultIdentityEnv constructs standard production identity environment.
func DefaultIdentityEnv() IdentityEnv {
	return IdentityEnv{
		Getenv:      os.Getenv,
		Unsetenv:    os.Unsetenv,
		Args:        os.Args,
		PID:         os.Getpid(),
		SocketPath:  HerdrSocketFile,
		Timeout:     IdentityQueryTimeout,
		ReadStat:    ReadProcPpid,
		ReadCmdline: ReadProcCmdline,
		Environ:     os.Environ,
		QuerySocket: QueryPaneShellPid,
		ArgvCache:   defaultArgvCache,
	}
}

// identityWarnOnce deduplicates the single stderr warning emitted when identity cannot be
// verified (S1/S2/S3/S4) and resolution therefore fails closed to subagent.
// The 1500ms budget is never extended and no blocking retry layer is stacked on top:
// one line per process, that is the whole observability contract.
//
// Known limitation: PiG redirects an extension subprocess's stderr to a temporary file that it
// normally does not keep (ref/PiG coding/extension/host/subprocess/host.go:3238-3248, D56), so in the
// deployed shape this line may be invisible. It is a best-effort signal, not a delivery channel.
var identityWarnOnce sync.Once

// ResetIdentityWarnOnce resets identityWarnOnce for testing purposes.
func ResetIdentityWarnOnce() {
	identityWarnOnce = sync.Once{}
}

// reportIdentityWarn emits exactly one warning line per process for an identity that could not
// be verified (fail-closed to subagent). The branch (S1/S2/S3/S4) is named in reason.
// Additional detail is only printed when PI_IDENTITY_DEBUG=1.
func reportIdentityWarn(reason string) {
	identityWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[subagent-identity] warning: identity not verified, fail-closed to subagent: %s\n", reason)
	})
	if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "[subagent-identity] debug: identity not verified (%s)\n", reason)
	}
}

// failClosed logs the deduplicated warning for a failed identity check and returns the
// fail-closed verdict (subagent) with the caller's reason. Every fail-closed branch (S1-S4)
// returns through here, so a degraded identity is always observable once, never silently.
func failClosed(reason string) (bool, string) {
	reportIdentityWarn(reason)
	return true, reason
}

// describeAncestorChain renders the /proc ancestry chain for diagnostics (oldest-last).
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

// ResolveAgentIdentityWithEnv performs fail-closed identity resolution.
// Returns isSubagent (true = subagent or unverified) and reason string.
//
// Identity criteria (S1-S4), all of which must hold to be recognised as the main agent:
//   - S1: HERDR_PANE_ID must be non-empty (it is only the lookup key for the herdr query).
//     Pane NAMING is deliberately not a criterion: pane ids may be renamed/renumbered, so
//     "ends with :p1" says nothing about who owns the session. The pane id is carried into
//     the reason string as auxiliary evidence only.
//   - S2: the running herdr server must answer the pane query with a matching shell_pid.
//     Failure/timeout -> fail-closed subagent plus one deduplicated warning line.
//   - S3: the shell_pid must be a strict ancestor of the resolved host process.
//   - S4: no other agent runtime may sit between the host and the pane shell.
//
// Host resolution is anchored on the process topology, never on pane naming:
//   - subprocess form (DetectSubprocessForm: PIG_EXT_SOCKET, a PIG_EXT_SOCKET_<NAME> entry, or
//     PIG_EXT_PACKED_CELL / PIG_EXT_ACTIVE_MEMBERS): walk upward from self and take the FIRST
//     agent-runtime cmdline as the host; none below the pane shell -> fail-closed subagent.
//   - fused form (none of those): self is the host; an unrecognisable self cmdline
//     -> fail-closed subagent.
func ResolveAgentIdentityWithEnv(env IdentityEnv) (bool, string) {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Unsetenv == nil {
		env.Unsetenv = os.Unsetenv
	}
	if env.ReadStat == nil {
		env.ReadStat = ReadProcPpid
	}
	if env.ReadCmdline == nil {
		env.ReadCmdline = ReadProcCmdline
	}
	if env.Environ == nil {
		env.Environ = os.Environ
	}
	if env.QuerySocket == nil {
		env.QuerySocket = QueryPaneShellPid
	}
	if env.Timeout <= 0 {
		env.Timeout = IdentityQueryTimeout
	}
	if env.SocketPath == "" {
		env.SocketPath = HerdrSocketFile
	}

	// Merge os.Args with PIG_HARNESS_ARGV_FILE arguments if present.
	// Fail-closed policy (M2): if PIG_HARNESS_ARGV_FILE is set but read/parse fails,
	// resolve as subagent immediately.
	effectiveArgs, err := LoadEffectiveArgsWithCache(env.Getenv, env.Unsetenv, env.Args, env.ArgvCache)
	if err != nil {
		return true, fmt.Sprintf("argv bridge failed: %v", err)
	}

	// Negative launch markers
	if marker, hit := SubagentLaunchMarker(env.Getenv, effectiveArgs); hit {
		return true, fmt.Sprintf("launch marker: %s", marker)
	}

	// S1: HERDR_PANE_ID must be non-empty. It is only the lookup key handed to herdr.
	// Pane naming is NOT an identity criterion (see doc comment): a pane id such as
	// "w32:p23" is recorded as auxiliary evidence in the reason string and never decides
	// subagent-ness on its own.
	paneID := strings.TrimSpace(env.Getenv("HERDR_PANE_ID"))
	if paneID == "" {
		return failClosed("S1: HERDR_PANE_ID unset or empty")
	}

	// S2: query running herdr server for shell_pid.
	// Failure/timeout is fail-closed to subagent with exactly one deduplicated log line;
	// the 1500ms budget is not extended and no retry layer is added.
	shellPID, ok := env.QuerySocket(env.SocketPath, paneID, env.Timeout)
	if !ok || shellPID <= 0 {
		return failClosed(fmt.Sprintf("S2: herdr pane query for %s failed, timed out or mismatched", paneID))
	}

	// S3: /proc ancestry chain must contain shellPID
	chain := ReadAncestorChain(env.PID, IdentityAncestorMaxHops, env.ReadStat, env.ReadCmdline)
	if len(chain) == 0 {
		return failClosed("S3: /proc ancestry for this process is unreadable")
	}

	shellIndex := -1
	for idx, entry := range chain {
		if entry.PID == shellPID {
			shellIndex = idx
			break
		}
	}
	if shellIndex < 0 {
		return failClosed(fmt.Sprintf("S3: pane %s shell_pid=%d is not in this process ancestry [%s]", paneID, shellPID, describeAncestorChain(chain)))
	}

	// Host process topology determination.
	// Based on PiG architecture (ref/PiG/coding/extension/host/subprocess/host.go:3106 connectExt):
	// 1. Subprocess form: PiG does not run the extension inside the host process. An isolated
	//    extension process gets PIG_EXT_SOCKET (host.go:3189); a packed cell process gets one
	//    PIG_EXT_SOCKET_<NAME> per member and no PIG_EXT_SOCKET (packed_go.go:422, doc.go:11-12),
	//    plus PIG_EXT_PACKED_CELL / PIG_EXT_ACTIVE_MEMBERS (packed_go.go:495-497). Here chain[0]
	//    is the extension (or the packed-cell runner) and its agent-runtime ancestor
	//    (pi/pig/ctxmode) is the host.
	// 2. Fused (D31) / in-process form: the extension factory runs inside the host process
	//    itself via an in-memory pipe (inProcServe, host.go:3107) with none of those variables.
	//    Here chain[0] is the host process.
	//
	// Primary discriminator: DetectSubprocessForm, which must cover the packed cell shape --
	// Go extensions run as packed cell members by default, so checking PIG_EXT_SOCKET alone
	// leaves the packed runner unrecognisable and flips the real main agent to subagent.
	// Pane naming never participates, and the former "len(chain) >= 2" fallback is gone:
	// inferring the host from chain length alone mis-anchors the scan window and let
	// topologies such as "ext -> sh -> pi -> bash" be read as subagent.
	subprocessForm, subprocessProof := DetectSubprocessForm(env.Getenv, env.Environ)

	hostIndex := -1
	if subprocessForm {
		// Walk upward from self and take the FIRST agent-runtime cmdline as the host.
		// The scan is bounded by the pane shell: an agent runtime that only appears above
		// the pane shell cannot be this session's host.
		for idx := 0; idx < shellIndex; idx++ {
			if IsAgentRuntimeCmdline(chain[idx].Cmdline) {
				hostIndex = idx
				break
			}
		}
		if hostIndex < 0 {
			return failClosed(fmt.Sprintf("S3: no agent-runtime ancestor for subprocess pid=%d below pane shell pid=%d (form: %s) [%s]", chain[0].PID, shellPID, subprocessProof, describeAncestorChain(chain)))
		}
	} else {
		// Fused form: self is the host; an unrecognisable cmdline cannot be proven to be one.
		if !IsAgentRuntimeCmdline(chain[0].Cmdline) {
			return failClosed(fmt.Sprintf("S3: fused form but self pid=%d cmdline %q is not an agent runtime [%s]", chain[0].PID, chain[0].Cmdline, describeAncestorChain(chain)))
		}
		hostIndex = 0
	}

	if shellIndex < hostIndex {
		return failClosed(fmt.Sprintf("S3: pane shell_pid=%d at hop=%d is below host pid=%d at hop=%d (shell is a descendant of this process's host)", shellPID, shellIndex, chain[hostIndex].PID, hostIndex))
	}
	if shellIndex == hostIndex {
		return failClosed(fmt.Sprintf("S3: pane shell_pid=%d at hop=%d is the same process as host pid=%d (expected a strict ancestor, got self)", shellPID, shellIndex, chain[hostIndex].PID))
	}

	// S4: between host and shell_pid, no agent runtime (pi/pig/ctxmode) may appear.
	// Skips the host itself (starting from hostIndex + 1), scanning only intermediate ancestors.
	// In standard topologies (shell -> host -> [subprocess ext]), hostIndex+1 == shellIndex, so the
	// loop executes 0 times. If a rogue or wrapper agent runtime was spawned between shell and
	// host, it is detected here.
	for hop := hostIndex + 1; hop < shellIndex; hop++ {
		ancestor := chain[hop]
		if IsAgentRuntimeCmdline(ancestor.Cmdline) {
			cmd := ancestor.Cmdline
			if len(cmd) > 60 {
				cmd = cmd[:60]
			}
			return failClosed(fmt.Sprintf("S4: agent runtime ancestor pid=%d %q between host and pane shell", ancestor.PID, cmd))
		}
	}

	// The pane id is reported as auxiliary evidence only (pane naming is not a criterion).
	return false, fmt.Sprintf("S1-S4 hold: pane=%s shell_pid=%d hop=%d (aux: pane naming is auxiliary evidence only, not an identity criterion)", paneID, shellPID, shellIndex)
}

// ResolveAgentIdentity resolves identity using DefaultIdentityEnv.
func ResolveAgentIdentity() (bool, string) {
	return ResolveAgentIdentityWithEnv(DefaultIdentityEnv())
}

// IsSubagentSession returns true if current session is a subagent (or cannot be proven as main).
// Honors PI_IDENTITY_DEBUG=1 for diagnostic logging to stderr.
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

// ===== EXTENSION ENTRYPOINT & EVENT WIRING =====

var enforceWarnOnce sync.Once

func reportEnforceError(stage string, err error) {
	if err == nil {
		return
	}
	enforceWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[subagent-identity] warning: failed to enforce tool exclusions on %s: %v\n", stage, err)
	})
	if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "[subagent-identity] debug: failed to enforce tool exclusions on %s: %v\n", stage, err)
	}
}

// ResetEnforceWarnOnce resets the enforceWarnOnce sync.Once for testing purposes.
func ResetEnforceWarnOnce() {
	enforceWarnOnce = sync.Once{}
}

// Extension constructs and returns the exclude-tools-guard extension instance for PiG.
func Extension() *sdk.Extension {
	e := sdk.New("exclude-tools-guard")

	// session_start: enforce tool exclusions on session startup
	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		isSubagent := IsSubagentSession()
		if _, err := EnforceToolExclusions(ctx, isSubagent); err != nil {
			reportEnforceError("session_start", err)
		}
		return nil, nil
	})

	// before_agent_start: second defense at each turn start (against session resumption / mid-way activation)
	// and strips leaked appendSystemPrompt / <addendum> blocks from subagent sessions.
	e.OnEvent(sdk.EventBeforeAgentStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		isSubagent := IsSubagentSession()
		if _, err := EnforceToolExclusions(ctx, isSubagent); err != nil {
			reportEnforceError("before_agent_start", err)
		}
		if !isSubagent {
			return nil, nil
		}
		if data != nil {
			if options, ok := data["systemPromptOptions"].(map[string]any); ok && options != nil {
				CleanPromptOptions(options)
			}
		}
		return nil, nil
	})

	return e
}
