// Package jevgate implements the Jev / Clef (System One classifier) gate for
// PiG in Go.
//
// It ports extensions/jev-gate/src/jev-gate.ts (1005 lines, deployed md5
// f4ff427bb51b6fa378fcfd356b81fa99) and does two things:
//
//  1. registers the classifier provider `magpie-jev` (default base URL
//     http://127.0.0.1:3425/v1, model `group/classifier`) so codemode's
//     models.classify() can reach the same classifier group;
//  2. registers the direct tool `jev_check` with its five fixed tasks
//     (contract_lint / gate_scan / return_triage / disagreement / source_audit).
//     Question tables and judge thresholds are frozen inside this package; the
//     caller only passes the text to judge.
//
// It spawns no process, registers no command, does not touch settings.json and
// never nests a pi tool call (no nestedCalls). Subagent sessions register
// nothing and clear the built-in `typesafe` classifier provider instead: a
// subagent must not stamp its own return.
//
// Boundaries of the classifier (official documentation — do not use it for
// anything else): English is the training language (Chinese works but must be
// self-assessed); it cannot count, do arithmetic, compare numbers or dates,
// and it degrades with irrelevant state, multi-hop reasoning, double negation
// and prompt injection. It is therefore a structural/classification signal,
// never a security boundary.
//
// Data egress: the payload goes to the local Magpie gateway and from there to
// the upstream Cloudflare Workers AI Clef classifier. Never send credentials or
// unsanitised environment data.
//
// Differences from the TypeScript version and the PiG gap this port closes are
// listed in README.md.
package jevgate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// isTask reports whether value is one of the five tasks (TS: TASKS.includes).
func isTask(value Task) bool {
	for _, task := range Tasks {
		if task == value {
			return true
		}
	}
	return false
}

// isRole reports whether value is one of the seven roles (TS: ROLES.includes).
func isRole(value Role) bool {
	for _, role := range Roles {
		if role == value {
			return true
		}
	}
	return false
}

// jsValueString renders a decoded JSON value the way String(value) does for the
// values an invalid task can carry.
func jsValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case float64:
		return jsNum(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// roleParam reads the optional role parameter: an absent or empty role is nil,
// any other string is kept even when it is not a known role (the contract_lint
// branch rejects those).
func roleParam(params map[string]any) *Role {
	raw, ok := params["role"].(string)
	if !ok || raw == "" {
		return nil
	}
	role := Role(raw)
	return &role
}

// ExecuteCheck runs one jev_check call against the production endpoint
// (TS: executeCheck).
func ExecuteCheck(params map[string]any) sdk.ToolResult {
	return executeCheck(DefaultJevAPI(), context.Background(), params)
}

// executeCheck runs one call with the given endpoint and cancellation.
func executeCheck(api JevAPI, ctx context.Context, params map[string]any) sdk.ToolResult {
	task := Task("")
	if raw, ok := params["task"].(string); ok {
		task = Task(raw)
	}
	role := roleParam(params)
	payload, _ := params["payload"].(string)

	if !isTask(task) {
		text := fmt.Sprintf("jev_check: invalid task %s; expected one of %s", jsValueString(params["task"]), taskList())
		return errorResult(text, map[string]any{"error": text})
	}
	if strings.TrimSpace(payload) == "" {
		text := "jev_check: payload is empty; nothing to judge"
		return errorResult(text, map[string]any{"error": text})
	}
	if task == TaskContractLint && (role == nil || !isRole(*role)) {
		text := fmt.Sprintf("jev_check: contract_lint requires role in %s", roleList())
		return errorResult(text, map[string]any{"error": text})
	}

	questions, err := questionsFor(task, role)
	if err != nil {
		return failedResult(task, err)
	}
	call, err := api.Call(ctx, payload, questions)
	if err != nil {
		return failedResult(task, err)
	}
	verdict, err := verdictFor(task, call.Answers, role)
	if err != nil {
		return failedResult(task, err)
	}

	model := call.Model
	if model == "" {
		model = api.Config.ModelID
	}
	head := []string{}
	headline := fmt.Sprintf("task=%s", string(task))
	if role != nil {
		headline += fmt.Sprintf(" role=%s", string(*role))
	}
	headline += " model=" + model
	head = append(head, headline, "verdict: "+strings.Join(verdict.Verdict, " | "))
	if verdict.Note != "" {
		head = append(head, "note: "+verdict.Note)
	}
	if call.Truncated != nil {
		head = append(head, fmt.Sprintf("TRUNCATED: sent %d/%d chars", call.Truncated.UsedChars, call.Truncated.OriginalChars))
	}
	head = append(head, "probs: "+fmtProbs(verdict.Lines))
	usageIn, usageOut := 0.0, 0.0
	if call.Usage != nil {
		usageIn, usageOut = call.Usage.Input, call.Usage.Output
	}
	head = append(head, fmt.Sprintf("usage: in=%s out=%s", jsNum(usageIn), jsNum(usageOut)))
	head = append(head, advisory)
	text := strings.Join(head, "\n")

	probabilities := make(map[string]any, len(verdict.Lines))
	for _, entry := range verdict.Lines {
		if entry.P == nil {
			probabilities[entry.ID] = nil
		} else {
			probabilities[entry.ID] = *entry.P
		}
	}
	structured := map[string]any{
		"task":          string(task),
		"model":         model,
		"verdict":       verdict.Verdict,
		"probabilities": probabilities,
		"usage":         nil,
		"advisory":      true,
	}
	if role != nil {
		structured["role"] = string(*role)
	}
	if call.Truncated != nil {
		structured["truncated"] = map[string]any{
			"originalChars": call.Truncated.OriginalChars,
			"usedChars":     call.Truncated.UsedChars,
		}
	}
	if call.Usage != nil {
		structured["usage"] = map[string]any{
			"input":  call.Usage.Input,
			"output": call.Usage.Output,
		}
	}

	summary := string(task)
	if len(verdict.Verdict) > 0 {
		summary = verdict.Verdict[0]
	}
	return toolResult(text, summary, structured, false)
}

// failedResult is the catch branch of executeCheck: the error message, capped at
// 400 characters.
func failedResult(task Task, err error) sdk.ToolResult {
	message := oneLine(err.Error(), 400)
	text := "jev_check failed: " + message
	return errorResult(text, map[string]any{
		"task":     string(task),
		"error":    message,
		"advisory": true,
	})
}

// errorResult is the shared isError shape of jev_check.
func errorResult(text string, structured map[string]any) sdk.ToolResult {
	return sdk.ToolResult{
		Content:           text,
		Details:           map[string]any{"summary": text},
		StructuredContent: structured,
		IsError:           true,
	}
}

// sdkContext adapts sdk.Context to context.Context so the HTTP call follows the
// request's cancellation.
type sdkContext struct{ ctx sdk.Context }

func (s sdkContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (s sdkContext) Done() <-chan struct{}       { return s.ctx.Done() }
func (s sdkContext) Err() error                  { return s.ctx.Err() }
func (s sdkContext) Value(any) any               { return nil }

// jevCheckSchema is the tool's argument schema (TS: the typebox Type.Object of
// registerTool).
func jevCheckSchema() sdk.Schema {
	taskEnum := make([]any, len(Tasks))
	for i, task := range Tasks {
		taskEnum[i] = string(task)
	}
	roleEnum := make([]any, len(Roles))
	for i, role := range Roles {
		roleEnum[i] = string(role)
	}
	return sdk.Schema{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": taskList(),
				"enum":        taskEnum,
			},
			"payload": map[string]any{
				"type": "string",
				"description": "要判定的文本（回传正文/报告本体）。经本地 Magpie 网关转发至上游分类器（Cloudflare Workers AI Clef）；" +
					"不得含凭据或未脱敏环境信息。",
			},
			"role": map[string]any{
				"type":        "string",
				"description": "task=contract_lint 时必填：" + roleList(),
				"enum":        roleEnum,
			},
		},
		"required": []any{"task", "payload"},
	}
}

// providerConfig is the registerProvider payload of the magpie-jev provider
// (TS: the object literal passed to pi.registerProvider).
func providerConfig(config Config, apiKey string) sdk.ProviderConfig {
	return sdk.ProviderConfig{
		"name":    "Magpie (Jev / Clef)",
		"baseUrl": config.BaseURL,
		"apiKey":  apiKey,
		"models": []any{
			map[string]any{
				"type":          "classifier",
				"id":            config.ModelID,
				"name":          "Classifier Group (Magpie)",
				"api":           classifierAPI,
				"baseUrl":       config.BaseURL,
				"input":         []any{"text"},
				"cost":          map[string]any{"input": 0.24, "output": 0, "cacheRead": 0, "cacheWrite": 0},
				"contextWindow": 65536,
			},
		},
		// PiG resolves a classifier implementation per provider; see classify.go.
		"classifiers": map[string]sdk.ProviderClassifyFunc{classifierAPI: classifyTypesafeSystemOne},
	}
}

// toolDefinition is the registerTool definition of jev_check.
func toolDefinition(api JevAPI) sdk.ToolDefinition {
	readOnly := true
	return sdk.ToolDefinition{
		Name:  "jev_check",
		Label: "JEV Check",
		Description: "Jev / Clef（System One 分类器，非 LLM）做的 5 类结构化判定：契约 lint / 门禁扫描 / 回传分类 / 分歧提取 / 出处核验。" +
			" 一次调用并行回答固定问句，返回概率与写死阈值算出的结论。只做结构与分类判定，" +
			"不做计数、数值或日期比较，也不产出解释性文字。",
		PromptSnippet: "Structured classifier checks (contract / gates / triage / disagreement / sourcing)",
		PromptGuidelines: []string{
			"Use jev_check task=contract_lint role=<role> on a subagent return to detect missing required fields before acting on it.",
			"Use jev_check task=gate_scan before releasing a change: it flags G1 evidence gaps, G2 unresolved must-fix, G3 fatal doubts, G4 leftovers missing from the follow-up list. The payload MUST include the current follow-up/leftover list, otherwise G4 cannot be judged.",
			"Use jev_check task=return_triage to classify a return. Its output is advisory: the anomaly direction may be trusted, but a 'normal' label never authorizes closing a pane.",
			"Use jev_check task=disagreement with payload = report A, a line of three dashes, then report B, when reviewer and oracle differ.",
			"Use jev_check task=source_audit on researcher output to flag unsourced claims.",
			"Never use jev_check for counting, arithmetic, dates, exact comparisons, or security boundaries; never send credentials or unsanitized environment data as payload.",
		},
		Parameters:  jevCheckSchema(),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: &readOnly},
		Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			return executeCheck(api, sdkContext{ctx: ctx}, params), nil
		},
	}
}

// Extension constructs and returns the jev-gate extension for PiG.
//
// Subagent sessions (fail-closed identity) register nothing and clear the
// built-in `typesafe` classifier provider: no magpie-jev provider, no jev_check
// tool, and no built-in classifier model to reach from codemode.
func Extension() *sdk.Extension {
	return newExtension(IsSubagentSession, DefaultJevAPI())
}

// newExtension wires the extension with an injected identity verdict and
// endpoint, so the factory can be tested without the production herdr socket.
func newExtension(isSubagent func() bool, api JevAPI) *sdk.Extension {
	e := sdk.New("jev-gate")

	if isSubagent() {
		// ProviderConfig with `models` replaces every model of that provider, so
		// an empty list clears the built-in classifier models.
		e.RegisterProvider(BuiltinClassifierProvider, sdk.ProviderConfig{"models": []any{}})
		return e
	}

	resolution := ResolveAPIKey(api.Config.BaseURL, api.Lookup)
	if !resolution.OK {
		detail := resolution.Detail
		if detail == "" {
			detail = "reason unknown"
		}
		fmt.Fprintf(os.Stderr,
			"[jev-gate] %s could not be resolved (%s) — jev_check will fail until it is configured\n",
			strings.Join(keyEnvNames, "/"), detail)
	}
	apiKey := resolution.Key
	if !resolution.OK {
		apiKey = "$" + keyEnvNames[0]
	}

	e.RegisterProvider(ProviderID, providerConfig(api.Config, apiKey))
	e.RegisterTool(toolDefinition(api))
	return e
}
