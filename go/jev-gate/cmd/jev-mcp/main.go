// Command jev-mcp is the pure-Go MCP stdio server that replaces
// jev-mcp.mjs for agy. It serves exactly one tool, jev_check, by calling
// jevgate.ExecuteCheck directly — the main/subagent identity verdict lives in
// jevgate.Extension(), which this entry point deliberately does not build;
// instead it applies the .mjs-flavoured resolveAgentIdentity (launch markers
// plus the ":p1" pane check) per tools/list and tools/call.
//
// Protocol: JSON-RPC 2.0 over stdio, both newline-delimited frames and
// Content-Length headers, mirroring jev-mcp.mjs's drainBuffer. Requests are
// handled in the order they arrive; tools/call honours
// notifications/cancelled / $/cancelRequest through context cancellation.
//
// The lazy schema files agy reads before the first handshake
// (~/.gemini/antigravity-cli/mcp/jev/) are written on startup, on every
// tools/list, and (best effort) on a denial, exactly like the .mjs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	jevgate "github.com/dorokuma/pi-extensions/go/jev-gate"
)

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
// stale (TS: ensureLazySchemaFiles). Best effort — every error is ignored.
func ensureLazySchemaFiles() {
	schemaJSON := jevLazySchemaJSON()
	for _, dir := range jevSchemaDirs {
		func() {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return
			}
			schemaPath := dir + "/jev_check.json"
			if data, err := os.ReadFile(schemaPath); err != nil || string(data) != schemaJSON {
				_ = os.WriteFile(schemaPath, []byte(schemaJSON), 0o644)
			}
			instrPath := dir + "/instructions.md"
			if data, err := os.ReadFile(instrPath); err != nil || string(data) != jevInstructions {
				_ = os.WriteFile(instrPath, []byte(jevInstructions), 0o644)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing (TS: sendRawFrame / sendResult / sendError).
// ---------------------------------------------------------------------------

type responseWriter struct {
	mu  sync.Mutex
	out io.Writer
	// current is the framing of the message being dispatched; parse errors and
	// notifications have no id, so it also answers "which framing do I reply in".
	current string
}

func (w *responseWriter) send(payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == "content-length" {
		fmt.Fprintf(w.out, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
	} else {
		w.out.Write(append(raw, '\n'))
	}
}

func (w *responseWriter) sendResult(id any, result any) {
	w.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (w *responseWriter) sendError(id any, code int, message string) {
	w.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// requestKey normalises a JSON-RPC id (number or string) to the map key the
// in-flight table uses (TS: String(id)).
func requestKey(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ---------------------------------------------------------------------------
// Server state.
// ---------------------------------------------------------------------------

var (
	inFlightMu sync.Mutex
	inFlight   = map[string]context.CancelFunc{}
)

func registerInFlight(key string, cancel context.CancelFunc) {
	inFlightMu.Lock()
	inFlight[key] = cancel
	inFlightMu.Unlock()
}

func dropInFlight(key string) {
	inFlightMu.Lock()
	delete(inFlight, key)
	inFlightMu.Unlock()
}

func cancelInFlight(key string) {
	inFlightMu.Lock()
	cancel, ok := inFlight[key]
	inFlightMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

// ---------------------------------------------------------------------------
// Main-agent identity check (port of the .mjs resolveAgentIdentity).
//
// jevgate.IsSubagentSession is the pi-flavoured verdict: its S3/S4 require an
// agent-runtime ancestor out of {pi, pig, ctxmode, pi-coding-agent,
// bundle/cli.js}. agy/antigravity is not in that list, so under the real agy
// main agent it fails closed — setting PIG_EXT_SOCKET would not help either
// (the subprocess form still needs such an ancestor). The check below ports
// the discriminating parts of the .mjs version instead: the negative launch
// markers (CTXMODE_ROLE / PI_SUBAGENT / PI_ROLE / subagent argv flags) plus
// the S1 ":p1" pane requirement. The S2-S4 socket/ancestry proof is not
// ported: jev-mcp is a dedicated process the main agent spawns, and the tool
// result stays advisory, never a security boundary.
// ---------------------------------------------------------------------------

// stripPromptArgTokens drops `-i` / `--prompt` and their argument, and any
// `--prompt=...` token: the main agent's prompt text may legitimately contain
// words like "--agent" or "/tmp/dispatch-..." (TS: stripPromptArgTokens).
func stripPromptArgTokens(tokens []string) []string {
	structural := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "-i" || token == "--prompt" {
			i++
			continue
		}
		if strings.HasPrefix(token, "--prompt=") {
			continue
		}
		structural = append(structural, token)
	}
	return structural
}

// hasSubagentCliFlags reports whether argv carries a dispatch marker
// (TS: hasSubagentCliFlags).
func hasSubagentCliFlags(tokens []string) bool {
	tokens = stripPromptArgTokens(tokens)
	for i, token := range tokens {
		if token == "--agent" {
			if i+1 < len(tokens) {
				return true
			}
			continue
		}
		if strings.HasPrefix(token, "--agent=") {
			return true
		}
		if token == "--name" {
			if i+1 < len(tokens) && strings.HasPrefix(tokens[i+1], "role-") {
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

// subagentLaunchMarker returns the first negative marker that matched
// (TS: subagentLaunchMarker).
func subagentLaunchMarker() (string, bool) {
	if v := strings.TrimSpace(os.Getenv("CTXMODE_ROLE")); v != "" {
		return "CTXMODE_ROLE=" + v, true
	}
	if os.Getenv("PI_SUBAGENT") == "1" {
		return "PI_SUBAGENT=1", true
	}
	if v := strings.TrimSpace(os.Getenv("PI_ROLE")); v != "" {
		return "PI_ROLE=" + v, true
	}
	if hasSubagentCliFlags(os.Args) {
		return "argv has subagent flags", true
	}
	return "", false
}

// isSubagentNow evaluates the identity verdict for this call. ExecuteCheck
// itself performs no identity check (that lives in Extension()); the gate is
// applied here, per request, exactly where the .mjs applied
// resolveAgentIdentity.
func resolveAgentIdentity() (bool, string) {
	if marker, hit := subagentLaunchMarker(); hit {
		return true, "launch marker: " + marker
	}
	paneID := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
	if paneID == "" {
		return true, "S1: HERDR_PANE_ID unset or empty"
	}
	if !strings.HasSuffix(paneID, ":p1") {
		return true, fmt.Sprintf("S1: HERDR_PANE_ID=%s is not a main pane (no \":p1\" suffix)", paneID)
	}
	return false, "main pane " + paneID + " (launch markers clear; S2-S4 not ported)"
}

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

// handleRPC processes one decoded JSON-RPC object (TS: handleRpcObject).
func handleRPC(w *responseWriter, msg map[string]any) {
	id, hasID := msg["id"]
	method, _ := msg["method"].(string)

	if method == "" {
		if hasID {
			w.sendError(id, -32600, "Invalid Request: missing method")
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
			w.sendResult(id, map[string]any{})
		}
		return
	case "notifications/cancelled", "$/cancelRequest":
		if target, ok := params["requestId"]; ok {
			cancelInFlight(requestKey(target))
		} else if target, ok := params["id"]; ok {
			cancelInFlight(requestKey(target))
		}
		if hasID {
			w.sendResult(id, map[string]any{})
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
			"serverInfo":      map[string]any{"name": "jev", "version": "1.0.0"},
		})
	case "ping":
		w.sendResult(id, map[string]any{})
	case "tools/list":
		if isSubagent, reason := resolveAgentIdentity(); isSubagent {
			if os.Getenv("PI_IDENTITY_DEBUG") == "1" {
				fmt.Fprintf(os.Stderr, "[jev-mcp] tools/list denied (%s)\n", reason)
			}
			w.sendResult(id, map[string]any{"tools": []any{}})
			go ensureLazySchemaFiles()
			return
		}
		w.sendResult(id, map[string]any{"tools": []any{jevToolEntry()}})
		ensureLazySchemaFiles()
	case "tools/call":
		handleToolsCall(w, id, params)
	default:
		w.sendError(id, -32601, "Method not found: "+method)
	}
}

func handleToolsCall(w *responseWriter, id any, params map[string]any) {
	if isSubagent, reason := resolveAgentIdentity(); isSubagent {
		deny := "jev_check denied: 仅限主代理（:p1）调用，子代理严禁自审自批。（" + reason + "）"
		w.sendResult(id, toolErrorResult(deny))
		return
	}
	name, _ := params["name"].(string)
	if name != "jev_check" {
		w.sendResult(id, toolErrorResult(fmt.Sprintf("jev-mcp: unknown tool %q", name)))
		return
	}
	rawArgs, _ := params["arguments"].(map[string]any)
	if rawArgs == nil {
		rawArgs = map[string]any{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	key := requestKey(id)
	registerInFlight(key, cancel)
	defer func() {
		dropInFlight(key)
		cancel()
	}()

	// jevgate.ExecuteCheck wraps executeCheck with context.Background(); to keep
	// notifications/cancelled honoured we run it under a goroutine and race
	// against ctx.Done(). The underlying HTTP call uses its own
	// JEV_TIMEOUT_MS-bounded context internally, so a cancelled wait never
	// leaks the process.
	type outcome struct {
		content    string
		details    any
		structured any
		isError    bool
	}
	done := make(chan outcome, 1)
	go func() {
		result := jevgate.ExecuteCheck(rawArgs)
		done <- outcome{
			content:    result.Content,
			details:    result.Details,
			structured: result.StructuredContent,
			isError:    result.IsError,
		}
	}()

	select {
	case r := <-done:
		w.sendResult(id, sdkResultToMCP(r.content, r.details, r.structured, r.isError))
	case <-ctx.Done():
		text := "jev_check failed: cancelled by MCP client"
		w.sendResult(id, toolErrorResult(text))
	}
}

// ---------------------------------------------------------------------------
// Framing / read loop (TS: drainBuffer).
// ---------------------------------------------------------------------------

const maxHeaderBytes = 1 << 20

// dispatchPayload parses one raw JSON-RPC frame (TS: dispatchRawPayload).
func dispatchPayload(w *responseWriter, raw string) {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		w.sendError(nil, -32700, "Parse error")
		return
	}
	if batch, ok := parsed.([]any); ok {
		for _, item := range batch {
			if msg, ok := item.(map[string]any); ok {
				handleRPC(w, msg)
			} else {
				w.sendError(nil, -32600, "Invalid Request")
			}
		}
		return
	}
	if msg, ok := parsed.(map[string]any); ok {
		handleRPC(w, msg)
		return
	}
	w.sendError(nil, -32600, "Invalid Request")
}

// readLoop consumes stdin frame by frame. Content-Length frames switch the
// reply framing; everything else stays newline-delimited (TS: framingMode).
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
	// EOF: any complete remainder is dispatched as one last line frame.
	if rest := strings.TrimSpace(string(carry)); rest != "" {
		w.current = "line"
		dispatchPayload(w, rest)
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
					w.current = "content-length"
					w.sendError(nil, -32700, "Header too large")
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
				w.current = "content-length"
				w.sendError(nil, -32700, "Invalid Content-Length header")
				continue
			}
			if len(carry) < headerEnd+sepLen+bodyLen {
				break
			}
			body := string(carry[headerEnd+sepLen : headerEnd+sepLen+bodyLen])
			carry = carry[headerEnd+sepLen+bodyLen:]
			w.current = "content-length"
			dispatchPayload(w, body)
			continue
		}

		nl := bytes.IndexByte(carry, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(string(carry[:nl]))
		carry = carry[nl+1:]
		if line != "" {
			w.current = "line"
			dispatchPayload(w, line)
		}
	}
	return carry
}

func main() {
	// JEV_TIMEOUT_MS default of the .mjs (15000 for the MCP server, where the
	// extension default is 60000).
	if strings.TrimSpace(os.Getenv("JEV_TIMEOUT_MS")) == "" {
		_ = os.Setenv("JEV_TIMEOUT_MS", "15000")
	}
	ensureLazySchemaFiles()
	w := &responseWriter{out: os.Stdout, current: "line"}
	readLoop(os.Stdin, w)
}
