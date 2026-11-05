// Command jev-mcp is the pure-Go MCP stdio server that replaces
// jev-mcp.mjs for agy. It serves exactly one tool, jev_check, by calling
// jevgate.ExecuteCheck directly; the main/subagent identity verdict is the
// shared S1–S4 core of the jevgate package (jevgate.ResolveMCPAgentIdentity),
// applied per tools/list and tools/call exactly where the .mjs applied its
// resolveAgentIdentity.
//
// Protocol: JSON-RPC 2.0 over stdio, both newline-delimited frames and
// Content-Length headers, mirroring jev-mcp.mjs's drainBuffer. Requests are
// handled in the order they arrive; tools/call honours
// notifications/cancelled / $/cancelRequest through context cancellation.
//
// The lazy schema files agy reads before the first handshake
// (~/.gemini/antigravity-cli/mcp/jev/) are written on the first allowed
// tools/list — never at startup and never on a denial, so a rejected
// subagent's server instance leaves the global dirs untouched.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	jevgate "github.com/dorokuma/pi-extensions/go/jev-gate"
)

// version is injected at build time via -ldflags "-X main.version=X.Y.Z"
// (see VERSIONING.md); the source default is "dev".
var version = "dev"

// ---------------------------------------------------------------------------
// Tool metadata (FALLBACK_* of jev-mcp.mjs; identical to the TS extension).
// ---------------------------------------------------------------------------

const fallbackBaseDescription = "Jev / Clef（System One 分类器，非 LLM）做的 5 类结构化判定：契约 lint / 门禁扫描 / 回传分类 / 分歧提取 / 出处核验。" +
	" 一次调用并行回答固定问句，返回概率与写死阈值算出的结论。只做结构与分类判定，" +
	"不做计数、数值或日期比较，也不产出解释性文字。"

var fallbackGuidelines = []string{
	"Use jev_check task=contract_lint role=<role> on a subagent return to detect missing required fields before acting on it.",
	"Use jev_check task=gate_scan before releasing a change: it flags G1 evidence gaps, G2 unresolved must-fix, G3 fatal doubts, G4 leftovers missing from the follow-up list. The payload MUST include the current follow-up/leftover list, otherwise G4 cannot be judged.",
	"Use jev_check task=return_triage to classify a return. Its output is advisory: the anomaly direction may be trusted, but a 'normal' label never authorizes closing a pane.",
	"Use jev_check task=disagreement with payload = report A, a line of three dashes, then report B, when reviewer and oracle differ.",
	"Use jev_check task=source_audit on researcher output to flag unsourced claims.",
	"Never use jev_check for counting, arithmetic, dates, exact comparisons, or security boundaries; never send credentials or unsanitized environment data as payload.",
}

// jevCheckInputSchema mirrors jevgate's jevCheckSchema() (unexported) plus the
// additionalProperties:false the .mjs typebox shim emits. enum lists are built
// from the exported jevgate.Tasks / jevgate.Roles so they cannot drift.
func jevCheckInputSchema() map[string]any {
	taskEnum := make([]any, len(jevgate.Tasks))
	for i, task := range jevgate.Tasks {
		taskEnum[i] = string(task)
	}
	roleEnum := make([]any, len(jevgate.Roles))
	for i, role := range jevgate.Roles {
		roleEnum[i] = string(role)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"task": map[string]any{
				"type": "string",
				"description": strings.Join([]string{
					string(jevgate.TaskContractLint), string(jevgate.TaskGateScan), string(jevgate.TaskReturnTriage),
					string(jevgate.TaskDisagreement), string(jevgate.TaskSourceAudit),
				}, "|"),
				"enum": taskEnum,
			},
			"payload": map[string]any{
				"type": "string",
				"description": "要判定的文本（回传正文/报告本体）。经本地 Magpie 网关转发至上游分类器（Cloudflare Workers AI Clef）；" +
					"不得含凭据或未脱敏环境信息。",
			},
			"role": map[string]any{
				"type": "string",
				"description": "task=contract_lint 时必填：" + strings.Join([]string{
					string(jevgate.RoleScout), string(jevgate.RolePlanner), string(jevgate.RoleResearcher),
					string(jevgate.RoleWorker), string(jevgate.RoleVerifier), string(jevgate.RoleReviewer),
					string(jevgate.RoleOracle),
				}, "|"),
				"enum": roleEnum,
			},
		},
		"required": []any{"task", "payload"},
	}
}

func buildFullDescription() string {
	return fallbackBaseDescription + " Guidelines: " + strings.Join(fallbackGuidelines, " ")
}

func buildInstructionsMarkdown() string {
	lines := []string{
		"# jev — Jev / Clef (System One Classifier) Gate & Triage Check",
		"",
		fallbackBaseDescription,
		"",
		"## Tool map",
		"",
		"- **jev_check**: Structured classifier checks (`contract_lint` | `gate_scan` | `return_triage` | `disagreement` | `source_audit`). Required arguments: `task`, `payload` (and `role` when `task=contract_lint`).",
		"",
		"## Prompt guidelines",
		"",
	}
	for _, g := range fallbackGuidelines {
		lines = append(lines, "- "+g)
	}
	lines = append(lines,
		"",
		"## Data & credential safety warning",
		"",
		"- `payload` is sent to the local Magpie gateway (`http://127.0.0.1:3425/v1/systemone`) and forwarded to upstream Cloudflare Workers AI Clef.",
		"- `payload` MUST ONLY contain the subagent return body or report text itself; NEVER send credentials, secrets, tokens, or unsanitized environment data.",
		"- Main agent (`:p1`) only: subagents are strictly prohibited from calling `jev_check`.",
		"",
	)
	return strings.Join(lines, "\n")
}

func jevToolEntry() map[string]any {
	return map[string]any{
		"name":        "jev_check",
		"description": buildFullDescription(),
		"inputSchema": jevCheckInputSchema(),
		"annotations": map[string]any{"readOnlyHint": true},
	}
}

var jevInstructions = buildInstructionsMarkdown()

// jevLazySchemaJSON is JEV_LAZY_SCHEMA_JSON of the .mjs: the same object agy
// reads from the mcp/jev directory, compacted with \u003c/\u003e escapes for <,>,&.
func jevLazySchemaJSON() string {
	raw, err := json.Marshal(map[string]any{
		"name":        "jev_check",
		"description": buildFullDescription(),
		"parameters":  jevCheckInputSchema(),
	})
	if err != nil {
		return ""
	}
	// encoding/json escapes <, >, & to <, >, & already (HTML-safe),
	// matching the .mjs .replace chains.
	return string(raw)
}

var jevSchemaDirs = []string{
	"/root/.gemini/antigravity-cli/mcp/jev",
	"/root/.agy-pro-home/.gemini/antigravity-cli/mcp/jev",
}

// ensureLazySchemaFiles writes jev_check.json + instructions.md when missing or
// stale (TS: ensureLazySchemaFiles). Every write is atomic (same-directory
// temp file + rename) so a concurrent agy reader never sees a torn or empty
// file; every error is ignored (best effort).
func ensureLazySchemaFiles() {
	schemaJSON := jevLazySchemaJSON()
	for _, dir := range jevSchemaDirs {
		_ = writeFileAtomic(dir+"/jev_check.json", []byte(schemaJSON), 0o644)
		_ = writeFileAtomic(dir+"/instructions.md", []byte(jevInstructions), 0o644)
	}
}

// schemaWriteMu serialises the in-process writers of the lazy schema files
// (the OS-level atomicity against other processes comes from rename(2)).
var schemaWriteMu sync.Mutex

// writeFileAtomic replaces targetPath with content atomically: a no-op when
// the file already matches, otherwise a unique same-directory temp file is
// fully written (with Close errors checked) and renamed over the target, so a
// concurrent reader always sees either the old or the new complete file and a
// crashed writer never leaves a torn target behind.
func writeFileAtomic(targetPath string, content []byte, perm os.FileMode) error {
	schemaWriteMu.Lock()
	defer schemaWriteMu.Unlock()
	if existing, err := os.ReadFile(targetPath); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".jev-schema-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing (TS: sendRawFrame / sendResult / sendError).
// ---------------------------------------------------------------------------

// responseWriter serialises every reply under one mutex. The framing of a
// reply is the framing of the request it answers ("line" or
// "content-length"), passed explicitly — a shared mutable "current mode"
// would race once tools/call dispatches execute concurrently.
type responseWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *responseWriter) send(payload map[string]any, framingMode string) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if framingMode == "content-length" {
		fmt.Fprintf(w.out, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
	} else {
		w.out.Write(append(raw, '\n'))
	}
}

func (w *responseWriter) sendResult(id any, result any, framingMode string) {
	w.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, framingMode)
}

func (w *responseWriter) sendError(id any, code int, message string, framingMode string) {
	w.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}, framingMode)
}

// requestKey normalises a JSON-RPC id to the in-flight table key. The id
// carries a type tag so the numeric id 1 and the string id "1" — distinct
// JSON-RPC ids — never collapse onto one map entry (F-3).
func requestKey(id any) string {
	switch v := id.(type) {
	case string:
		return "s:" + v
	case float64:
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%T:%v", id, v)
	}
}

// ---------------------------------------------------------------------------
// Server state.
// ---------------------------------------------------------------------------

var (
	inFlightMu sync.Mutex
	inFlight   = map[string]*inFlightCall{}
	// inFlightWg tracks the tools/call workers: readLoop can keep reading
	// (and cancelling) while calls are in flight, and the EOF drain waits on
	// it with a bounded budget.
	inFlightWg sync.WaitGroup
)

// inFlightCall is one cancellable tools/call: cancelledByClient distinguishes
// a notifications/cancelled / $/cancelRequest abort (the client owns the
// cancellation and gets the "cancelled by MCP client" result) from an
// internal deadline or drain-cancel.
type inFlightCall struct {
	cancel            context.CancelFunc
	cancelledByClient atomic.Bool
}

// registerInFlight records one call under key and reports whether the key
// was free: a duplicate in-flight request id is refused by the caller so
// register/cancel/drop stay one-to-one (F-3).
func registerInFlight(key string, call *inFlightCall) bool {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	if _, exists := inFlight[key]; exists {
		return false
	}
	inFlight[key] = call
	return true
}

// dropInFlight removes the registration for key only while it still points
// at call: a stale cleanup path must never delete a newer registration that
// reused the id after this call was already dropped (Q4).
func dropInFlight(key string, call *inFlightCall) {
	inFlightMu.Lock()
	if inFlight[key] == call {
		delete(inFlight, key)
	}
	inFlightMu.Unlock()
}

func cancelInFlight(key string) {
	inFlightMu.Lock()
	call, ok := inFlight[key]
	inFlightMu.Unlock()
	if ok && call != nil {
		call.cancelledByClient.Store(true)
		call.cancel()
	}
}

// cancelAllInFlight cancels every in-flight call; used by the bounded EOF
// drain so the process can never hang on a request that outlives stdin.
func cancelAllInFlight() {
	inFlightMu.Lock()
	calls := make([]*inFlightCall, 0, len(inFlight))
	for _, call := range inFlight {
		calls = append(calls, call)
	}
	inFlightMu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

// ---------------------------------------------------------------------------
// Main-agent identity check (shared jevgate S1–S4 core, MCP mode).
//
// jevgate.ResolveMCPAgentIdentity is the fail-closed verdict the .mjs applied
// inline: negative launch markers on this process AND every ancestor (env +
// argv, prompt tokens stripped), the S1 ":p1" pane requirement, the S2 herdr
// socket query, and the S3/S4 ancestry proof with at most one host layer
// (agy/antigravity or pi/pig) between jev-mcp and the pane shell. The Go
// version ports the full socket/ancestry proof the .mjs comment deferred.
// ---------------------------------------------------------------------------

// resolveAgentIdentity evaluates the identity verdict for this request.
// ExecuteCheck itself performs no identity check (that lives in Extension());
// the gate is applied here, per request, exactly where the .mjs applied
// resolveAgentIdentity.
var resolveAgentIdentity = jevgate.ResolveMCPAgentIdentity

func toolErrorResult(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"details": map[string]any{"summary": text},
		"isError": true,
	}
}

// sdkResultToMCP converts a jevgate sdk.ToolResult into the MCP tools/call
// result shape (TS: the sendResult object built around `result`).
func sdkResultToMCP(content string, details any, structured any, isError bool) map[string]any {
	out := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": content}},
	}
	if details != nil {
		out["details"] = details
	}
	if structured != nil {
		out["structuredContent"] = structured
	}
	if isError {
		out["isError"] = true
	}
	return out
}

// handleRPC processes one decoded JSON-RPC object (TS: handleRpcObject). The
// reply adopts the framing of the raw frame it was parsed from.
func handleRPC(w *responseWriter, msg map[string]any, framingMode string) {
	id, hasID := msg["id"]
	method, _ := msg["method"].(string)

	if method == "" {
		if hasID {
			w.sendError(id, -32600, "Invalid Request: missing method", framingMode)
		}
		return
	}

	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	switch method {
	case "notifications/initialized":
		if hasID {
			w.sendResult(id, map[string]any{}, framingMode)
		}
		return
	case "notifications/cancelled", "$/cancelRequest":
		if target, ok := params["requestId"]; ok {
			cancelInFlight(requestKey(target))
		} else if target, ok := params["id"]; ok {
			cancelInFlight(requestKey(target))
		}
		if hasID {
			w.sendResult(id, map[string]any{}, framingMode)
		}
		return
	}

	if !hasID {
		return
	}

	switch method {
	case "initialize":
		protocolVersion := "2024-11-05"
		if v, ok := params["protocolVersion"].(string); ok && strings.TrimSpace(v) != "" {
			protocolVersion = strings.TrimSpace(v)
		}
		w.sendResult(id, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":    jevInstructions,
			"serverInfo":      map[string]any{"name": "jev", "version": version},
		}, framingMode)
	case "ping":
		w.sendResult(id, map[string]any{}, framingMode)
	case "tools/list":
		if isSubagent, reason := resolveAgentIdentity(); isSubagent {
			if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
				fmt.Fprintf(os.Stderr, "[jev-mcp] tools/list denied (%s)\n", reason)
			}
			// A denied subagent gets an empty list and nothing is written to the
			// global schema dirs (no async fire-and-forget write either).
			w.sendResult(id, map[string]any{"tools": []any{}}, framingMode)
			return
		}
		// The lazy schema files must exist before the tools/list response is
		// visible: agy reads them as soon as the list arrives, so the write
		// cannot trail the reply (Q5). The denied branch above never reaches
		// this — a rejected instance still writes nothing.
		ensureLazySchemaFiles()
		w.sendResult(id, map[string]any{"tools": []any{jevToolEntry()}}, framingMode)
	case "tools/call":
		handleToolsCall(w, id, params, framingMode)
	default:
		w.sendError(id, -32601, "Method not found: "+method, framingMode)
	}
}

func handleToolsCall(w *responseWriter, id any, params map[string]any, framingMode string) {
	if isSubagent, reason := resolveAgentIdentity(); isSubagent {
		deny := "jev_check denied: 仅限主代理（:p1）调用，子代理严禁自审自批。（" + reason + "）"
		w.sendResult(id, toolErrorResult(deny), framingMode)
		return
	}
	name, _ := params["name"].(string)
	if name != "jev_check" {
		w.sendResult(id, toolErrorResult(fmt.Sprintf("jev-mcp: unknown tool %q", name)), framingMode)
		return
	}
	rawArgs, _ := params["arguments"].(map[string]any)
	if rawArgs == nil {
		rawArgs = map[string]any{}
	}

	// The call executes on its own goroutine so readLoop can keep reading
	// stdin: a notifications/cancelled or $/cancelRequest that arrives while
	// the HTTP call is in flight reaches cancelInFlight and interrupts the
	// underlying request through ctx.
	ctx, cancel := context.WithCancel(context.Background())
	call := &inFlightCall{cancel: cancel}
	key := requestKey(id)
	if !registerInFlight(key, call) {
		// A second request reusing an in-flight id is refused outright:
		// registering it would overwrite the first entry, and the first
		// worker's dropInFlight would then delete the new registration,
		// leaving the second call unreachable for cancellation (F-3).
		cancel()
		w.sendResult(id, toolErrorResult(fmt.Sprintf("jev-mcp: duplicate in-flight request id %v", id)), framingMode)
		return
	}
	inFlightWg.Add(1)
	go func() {
		defer inFlightWg.Done()
		defer func() {
			dropInFlight(key, call)
			cancel()
		}()
		result := jevgate.ExecuteCheckContext(ctx, rawArgs)
		// The id is unregistered BEFORE the reply is published to the client:
		// once the response bytes are visible the id must already be reusable,
		// otherwise a client reusing the id right after seeing its reply would
		// be refused as a duplicate (Q4). The deferred drop stays as a
		// belt-and-braces cleanup and is idempotent — it only deletes the
		// entry while it still points at this call, so it can never remove a
		// newer registration that legitimately reused the freed id.
		dropInFlight(key, call)
		if call.cancelledByClient.Load() {
			w.sendResult(id, toolErrorResult("jev_check failed: cancelled by MCP client"), framingMode)
			return
		}
		w.sendResult(id, sdkResultToMCP(result.Content, result.Details, result.StructuredContent, result.IsError), framingMode)
	}()
}

// ---------------------------------------------------------------------------
// Framing / read loop (TS: drainBuffer).
// ---------------------------------------------------------------------------

const (
	// maxHeaderBytes bounds one Content-Length header block while it is still
	// incomplete: an unterminated header stream stops here, not at EOF.
	maxHeaderBytes = 1 << 20
	// maxFrameBytes bounds one raw JSON-RPC frame (and one newline-less line
	// buffer) before it is parsed. This is the protocol-layer OOM guard: the
	// JEV_MAX_STATE_CHARS budget only truncates the state text *inside* a
	// decoded frame, so a frame bigger than this is rejected outright — the
	// refusal is deliberate, not a truncation.
	maxFrameBytes = 16 << 20
)

// dispatchPayload parses one raw JSON-RPC frame (TS: dispatchRawPayload).
func dispatchPayload(w *responseWriter, raw string, framingMode string) {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		w.sendError(nil, -32700, "Parse error", framingMode)
		return
	}
	if batch, ok := parsed.([]any); ok {
		for _, item := range batch {
			if msg, ok := item.(map[string]any); ok {
				handleRPC(w, msg, framingMode)
			} else {
				w.sendError(nil, -32600, "Invalid Request", framingMode)
			}
		}
		return
	}
	if msg, ok := parsed.(map[string]any); ok {
		handleRPC(w, msg, framingMode)
		return
	}
	w.sendError(nil, -32600, "Invalid Request", framingMode)
}

// callTimeout is the per-request JEV_TIMEOUT_MS budget (the .mjs default for
// the MCP server is 15000ms).
func callTimeout() time.Duration {
	timeout := jevgate.ConfigFromEnv(os.Getenv).Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return timeout
}

// readLoop consumes stdin frame by frame. Content-Length frames answer in
// Content-Length framing; everything else stays newline-delimited
// (TS: framingMode). At EOF the process does not cancel in-flight calls — a
// client that wrote its request and half-closed stdin must still get its
// reply — it waits for them inside one bounded drain budget, then cancels
// whatever is left and exits.
func readLoop(in io.Reader, w *responseWriter) {
	var carry []byte
	buf := make([]byte, 64*1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			carry = append(carry, buf[:n]...)
			carry = drain(carry, w)
		}
		if err != nil {
			break
		}
	}
	// EOF: the unterminated tail is dispatched as one last line frame, but
	// the same body bound applies — an EOF cannot launder an oversized frame
	// past the bound it would have hit mid-stream (a dangling \r at the
	// bound is part of a terminator that never arrived).
	if rest := strings.TrimSpace(string(carry)); rest != "" {
		if len(carry) > maxFrameBytes {
			w.sendError(nil, -32700, "Frame too large", "line")
		} else {
			dispatchPayload(w, rest, "line")
		}
	}
	// Bounded drain: give the in-flight calls the normal call budget plus a
	// small margin, then cancel the stragglers and wait briefly for their
	// cleanup so the process exit is always finite.
	drainTimeout := callTimeout() + 2*time.Second
	done := make(chan struct{})
	go func() {
		inFlightWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(drainTimeout):
		cancelAllInFlight()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// drain extracts every complete frame from carry (TS: drainBuffer) and returns
// the unconsumed tail.
func drain(carry []byte, w *responseWriter) []byte {
	for len(carry) > 0 {
		// Strip leading CR/LF between frames.
		lead := 0
		for lead < len(carry) && (carry[lead] == 0x0d || carry[lead] == 0x0a) {
			lead++
		}
		carry = carry[lead:]
		if len(carry) == 0 {
			break
		}

		preview := string(carry[:min(len(carry), 64)])
		if strings.HasPrefix(strings.ToLower(preview), "content-length") &&
			strings.Contains(preview, ":") {
			headerEnd := -1
			sepLen := 0
			crlfIdx := bytes.Index(carry, []byte("\r\n\r\n"))
			lfIdx := bytes.Index(carry, []byte("\n\n"))
			if crlfIdx >= 0 && (lfIdx < 0 || crlfIdx < lfIdx) {
				headerEnd, sepLen = crlfIdx, 4
			} else if lfIdx >= 0 {
				headerEnd, sepLen = lfIdx, 2
			}
			if headerEnd < 0 {
				if len(carry) > maxHeaderBytes {
					w.sendError(nil, -32700, "Header too large", "content-length")
					return nil
				}
				break
			}
			header := string(carry[:headerEnd])
			var bodyLen = -1
			for _, line := range strings.Split(header, "\n") {
				key, value, found := strings.Cut(strings.TrimSpace(line), ":")
				if found && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
					if v, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
						bodyLen = v
					}
				}
			}
			if bodyLen < 0 {
				carry = carry[headerEnd+sepLen:]
				w.sendError(nil, -32700, "Invalid Content-Length header", "content-length")
				continue
			}
			if bodyLen > maxFrameBytes {
				// The declared body alone is over the frame bound: drop the whole
				// buffered tail so a bogus length cannot pin a giant allocation or
				// poison the next frame.
				carry = nil
				w.sendError(nil, -32700, "Invalid Content-Length header", "content-length")
				break
			}
			// Subtraction, not addition: the missing-bytes comparison cannot
			// overflow no matter what bodyLen says.
			if len(carry)-headerEnd-sepLen < bodyLen {
				break
			}
			body := string(carry[headerEnd+sepLen : headerEnd+sepLen+bodyLen])
			carry = carry[headerEnd+sepLen+bodyLen:]
			dispatchPayload(w, body, "content-length")
			continue
		}

		nl := bytes.IndexByte(carry, '\n')
		if nl < 0 {
			// No terminator yet. The raw JSON body may be at most
			// maxFrameBytes, and a CRLF terminator contributes a CR before
			// the LF — so the buffer may legitimately reach
			// maxFrameBytes+1 only while its last byte is that pending CR.
			// Anything beyond that, or a non-CR byte in that slot, can never
			// become a deliverable frame: reject it now instead of letting a
			// dangling CR hold the buffer open forever (Q6).
			if len(carry) > maxFrameBytes+1 ||
				(len(carry) == maxFrameBytes+1 && carry[len(carry)-1] != '\r') {
				carry = nil
				w.sendError(nil, -32700, "Frame too large", "line")
			}
			break
		}
		// A complete line frame: the raw JSON body ends before the LF, minus
		// a CR when the terminator is CRLF — the terminator never counts
		// toward the frame body. bodyLen == maxFrameBytes is accepted (the
		// same bound the newline-less and Content-Length branches apply);
		// one byte more is refused before dispatch and the buffered tail is
		// dropped (Q6).
		bodyLen := nl
		if bodyLen > 0 && carry[bodyLen-1] == '\r' {
			bodyLen--
		}
		if bodyLen > maxFrameBytes {
			carry = nil
			w.sendError(nil, -32700, "Frame too large", "line")
			break
		}
		line := strings.TrimSpace(string(carry[:nl]))
		carry = carry[nl+1:]
		if line != "" {
			dispatchPayload(w, line, "line")
		}
	}
	return carry
}

func main() {
	for i := 1; i < len(os.Args); i++ {
		if arg := os.Args[i]; arg == "--version" || arg == "-version" {
			fmt.Println(version)
			return
		}
	}
	// JEV_TIMEOUT_MS default of the .mjs (15000 for the MCP server, where the
	// extension default is 60000).
	if strings.TrimSpace(os.Getenv("JEV_TIMEOUT_MS")) == "" {
		_ = os.Setenv("JEV_TIMEOUT_MS", "15000")
	}
	// The lazy schema files are written on the first allowed tools/list only:
	// a server that is launched but never lists tools (e.g. a denied
	// subagent's probe) must not touch the global schema dirs.
	w := &responseWriter{out: os.Stdout}
	readLoop(os.Stdin, w)
}
