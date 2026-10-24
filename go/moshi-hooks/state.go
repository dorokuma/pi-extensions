package moshihooks

// Event normalization and per-session bookkeeping, ported from moshi-hooks.ts
// (L370-520, L698-767).
//
// handleEvent is the in-module port of moshi-hook's
// internal/cli/hook_more_agents.go runPiFamilyHook: every host event is turned
// into at most one envelope, and a duplicate or a child session is dropped
// before anything reaches the socket.

import (
	"encoding/json"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// agentSource and agentDisplayName are baked in per agent family; this file
	// is generated for Pi (TS L18-19).
	agentSource      = "pi"
	agentDisplayName = "Pi"

	// stopCooldownSeconds suppresses a duplicate stop inside a short cooldown
	// once the turn completed (TS: recordTaskCompletion, L424-437).
	stopCooldownSeconds = 5.0

	// isoMillis is Date.prototype.toISOString's layout, which requestedAt
	// carries (TS: `new Date().toISOString()`, L517).
	isoMillis = "2006-01-02T15:04:05.000Z"
)

// now is the clock; tests replace it to pin timestamps.
var now = time.Now

// nowSeconds is the second-resolution clock the state machine compares with
// (TS: nowSeconds, L410).
func nowSeconds() float64 { return float64(now().UnixNano()) / 1e9 }

// SessionState is the per-session bookkeeping: the in-process equivalent of the
// CLI adapter's internal/agentstate files. The extension lives as long as the
// agent process, so plain maps carry state across events
// (TS: interface SessionState, L373).
type SessionState struct {
	Model                   string
	TranscriptPath          string
	SessionStartTime        float64
	FirstPromptPushedAt     float64
	LastUserPrompt          string
	LastEventTitle          string
	LastToolName            string
	PromptSequence          int
	CompletedPromptSequence int
	LastStopTime            float64
	LastStopSignature       string
}

var (
	// stateMu guards sessions and transcriptChildCache. handleEvent holds it for
	// the whole normalization so one session's sequence stays consistent.
	stateMu              sync.Mutex
	sessions             = map[string]*SessionState{}
	transcriptChildCache = map[string]bool{}
)

// stateFor returns the session's state, creating it on first sight
// (TS: stateFor, L389).
func stateFor(sessionID string) *SessionState {
	state, ok := sessions[sessionID]
	if !ok {
		state = &SessionState{}
		sessions[sessionID] = state
	}
	return state
}

// ResetSessionsForTest clears every session and the transcript lineage cache.
// Tests only.
func ResetSessionsForTest() {
	stateMu.Lock()
	sessions = map[string]*SessionState{}
	transcriptChildCache = map[string]bool{}
	stateMu.Unlock()
}

// promptSequenceForSubmit mirrors internal/agentstate.PromptSequenceForSubmit:
// reuse the in-flight turn's number, or advance when the previous turn already
// completed (TS L416).
func promptSequenceForSubmit(state *SessionState) int {
	if state.PromptSequence == 0 {
		return 1
	}
	if state.CompletedPromptSequence >= state.PromptSequence {
		return state.PromptSequence + 1
	}
	return state.PromptSequence
}

// recordTaskCompletion mirrors recordTaskCompletion in
// internal/cli/hook_more_agents.go: suppress duplicate stop events inside a
// short cooldown once the turn completed (TS L426).
func recordTaskCompletion(state *SessionState, signature string, at float64) bool {
	if signature == "" {
		signature = agentSource + "-complete"
	}
	turnAlreadyCompleted := state.PromptSequence == 0 || state.PromptSequence <= state.CompletedPromptSequence
	if turnAlreadyCompleted && state.LastStopTime > 0 && at-state.LastStopTime < stopCooldownSeconds &&
		signature == state.LastStopSignature {
		return false
	}
	state.LastStopTime = at
	state.LastStopSignature = signature
	if state.PromptSequence > 0 {
		state.CompletedPromptSequence = state.PromptSequence
	}
	return true
}

// HookPayload is one normalized host event (TS: interface HookPayload, L487).
//
// Extra carries the per-hook fields the caller merges in (`prompt`,
// `last_assistant_message`), which win over the base fields exactly like the
// TS object spread `{...base, ...extra}`.
type HookPayload struct {
	HookEventName    string
	SessionID        string
	TranscriptPath   string
	Cwd              string
	Model            string
	ContextRemaining *float64
	AgentKind        string
	Extra            map[string]any
}

// field reads a payload property by its TS name.
func (p *HookPayload) field(key string) any {
	if p.Extra != nil {
		if value, ok := p.Extra[key]; ok {
			return value
		}
	}
	switch key {
	case "hook_event_name":
		return p.HookEventName
	case "session_id":
		return p.SessionID
	case "transcript_path":
		return p.TranscriptPath
	case "cwd":
		return p.Cwd
	case "model":
		return p.Model
	case "agent_kind":
		return p.AgentKind
	}
	return nil
}

// raw reads a payload property verbatim (used for tool_input).
func (p *HookPayload) raw(key string) any { return p.field(key) }

// str reads a payload property as a non-empty string.
func (p *HookPayload) str(key string) string { return firstString(p.field(key)) }

// sessionIsChild reports whether the session was created by fork/clone/
// newSession with lineage, in which case it stays out of the inbox
// (TS L451).
//
// With agentSource "pi" the first two branches are the ones this generated file
// can reach: the omp branch is keyed on agentSource and the third branch
// returns early for every other family.
func sessionIsChild(state *SessionState, payload *HookPayload) bool {
	if agentSource == "omp" {
		return payload.str("agent_kind") == "sub"
	}
	if agentSource != "pi" && agentSource != "omo" {
		return false
	}
	filePath := firstString(payload.TranscriptPath, state.TranscriptPath)
	if filePath == "" {
		return false
	}
	if cached, ok := transcriptChildCache[filePath]; ok {
		return cached
	}
	child, known := transcriptHeaderIsChild(filePath)
	if !known {
		// An unreadable or half-written header is retried on the next event.
		return false
	}
	transcriptChildCache[filePath] = child
	return child
}

// transcriptHeaderIsChild reads the first line of the session transcript and
// reports whether it names a parent session (TS L464). known is false when the
// header could not be read or parsed.
func transcriptHeaderIsChild(filePath string) (child bool, known bool) {
	file, err := os.Open(filePath)
	if err != nil {
		return false, false
	}
	defer file.Close()

	buffer := make([]byte, 16*1024)
	n, _ := file.Read(buffer)
	if n <= 0 {
		return false, false
	}
	text := string(buffer[:n])
	if index := strings.Index(text, "\n"); index >= 0 {
		text = text[:index]
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(text), &header); err != nil {
		return false, false
	}
	return firstString(header["parentSession"], header["parentSessionId"]) != "", true
}

// baseEnvelope builds the envelope every event starts from
// (TS: baseEnvelope, L498).
func baseEnvelope(payload *HookPayload, sessionID, eventName string) *Envelope {
	terminal := TerminalContextForProcess()
	muxEnv := terminal.MuxEnv
	if muxEnv == nil {
		muxEnv = map[string]string{}
	}
	envelope := &Envelope{
		Type:             "session.update",
		Source:           agentSource,
		SessionID:        sessionID,
		EventName:        eventName,
		Cwd:              payload.Cwd,
		ProjectName:      projectNameForCwd(terminal, payload.Cwd),
		TerminalKind:     terminal.TerminalKind,
		MuxEnv:           muxEnv,
		TmuxSession:      terminal.TmuxSession,
		TmuxWindow:       terminal.TmuxWindow,
		TmuxPane:         terminal.TmuxPane,
		TmuxSocket:       terminal.TmuxSocket,
		ZellijSession:    terminal.ZellijSession,
		ZellijPane:       terminal.ZellijPane,
		HerdrSession:     terminal.HerdrSession,
		HerdrPane:        terminal.HerdrPane,
		HerdrWorkspaceID: terminal.HerdrWorkspaceID,
		HerdrWorkspace:   terminal.HerdrWorkspace,
		HerdrTabID:       terminal.HerdrTabID,
		HerdrTab:         terminal.HerdrTab,
		ModelName:        payload.Model,
		TranscriptPath:   payload.TranscriptPath,
		RequestedAt:      now().UTC().Format(isoMillis),
	}
	if payload.ContextRemaining != nil && *payload.ContextRemaining > 0 {
		envelope.ContextRemaining = intPtr(int(math.Round(*payload.ContextRemaining)))
	}
	return envelope
}

// handleEvent normalizes one event and queues at most one envelope
// (TS: handleEvent, L532).
func handleEvent(payload *HookPayload) {
	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	state := stateFor(sessionID)
	isChild := sessionIsChild(state, payload)

	switch payload.HookEventName {
	case "SessionStart":
		// A child session's start would repoint the pane at a session whose
		// transcript the daemon cannot serve — but only OMP subagents run in the
		// parent process (agentSource check), and this file is generated for Pi.
		if isChild && agentSource == "omp" {
			return
		}
		if payload.Model != "" {
			state.Model = payload.Model
		}
		if payload.TranscriptPath != "" {
			state.TranscriptPath = payload.TranscriptPath
		}
		state.SessionStartTime = nowSeconds()
		if state.PromptSequence > state.CompletedPromptSequence {
			state.CompletedPromptSequence = state.PromptSequence
		}
		// A category-less update is a silent state carrier: the daemon persists
		// the transcript path / model / pane for Chat View without publishing
		// anything — the first prompt announces the session.
		SendEnvelope(baseEnvelope(payload, sessionID, "SessionStart"))

	case "SessionEnd":
		if isChild {
			return
		}
		envelope := baseEnvelope(payload, sessionID, "SessionEnd")
		envelope.Type = "session.closed"
		envelope.Category = stringPtr("session_ended")
		envelope.Title = stringPtr(agentDisplayName + " session ended")
		SendEnvelope(envelope)

	case "UserPromptSubmit":
		if isChild {
			return
		}
		state.LastUserPrompt = plainTextClip(displayUserPrompt(payload.str("prompt")), 256)
		state.PromptSequence = promptSequenceForSubmit(state)
		alreadyPushed := state.FirstPromptPushedAt != 0 && state.FirstPromptPushedAt >= state.SessionStartTime
		if !alreadyPushed {
			state.FirstPromptPushedAt = nowSeconds()
		}
		title := agentDisplayName + " session started"
		if alreadyPushed && state.LastEventTitle != "" {
			title = state.LastEventTitle
		}
		eventName := "SessionStart"
		if alreadyPushed {
			eventName = "UserPromptSubmit"
		}
		envelope := baseEnvelope(payload, sessionID, eventName)
		envelope.Category = stringPtr("session_started")
		envelope.Title = stringPtr(title)
		envelope.Message = stringPtr(formatUserPromptDetail(state.LastUserPrompt))
		state.LastEventTitle = title
		SendEnvelope(envelope)

	case "AgentStart":
		return

	case "PermissionRequest":
		// Pi owns the approval prompt and decision; Moshi mirrors the waiting
		// state so the daemon's TUI bridge can send a remote decision into the
		// native prompt.
		if isChild && agentSource == "omp" {
			return
		}
		toolName := payload.str("tool_name")
		state.LastToolName = toolName
		title := agentDisplayName + " needs approval"
		envelope := baseEnvelope(payload, sessionID, "PermissionRequest")
		actionID := payload.str("tool_use_id")
		if actionID == "" {
			actionID = newSessionID()
		}
		envelope.ActionID = stringPtr(actionID)
		envelope.Phase = stringPtr("waitingForApproval")
		envelope.Category = stringPtr("approval_required")
		envelope.ToolName = stringPtr(toolName)
		envelope.Title = stringPtr(title)
		envelope.Subtitle = stringPtr("Answer in terminal")
		envelope.Message = stringPtr(plainTextClip(firstString(payload.str("reason"), payload.str("description")), 256))
		state.LastEventTitle = title
		SendEnvelope(envelope)

	case "PlanReview":
		// The native plan review is open. The plan rides along as the Chat View
		// card; without an action id no approval bar opens.
		if isChild {
			return
		}
		plan := payload.str("plan")
		if plan == "" {
			return
		}
		title := "Review " + agentDisplayName + "'s plan"
		prompt := &InteractivePrompt{Kind: "plan", Plan: plan}
		if toolUseID := payload.str("tool_use_id"); toolUseID != "" {
			prompt.ToolUseID = toolUseID
		}
		if planFilePath := payload.str("plan_file_path"); planFilePath != "" {
			prompt.PlanFilePath = planFilePath
		}
		envelope := baseEnvelope(payload, sessionID, "PlanReview")
		envelope.Category = stringPtr("approval_required")
		envelope.ToolName = stringPtr("exit_plan_mode")
		envelope.Title = stringPtr(title)
		envelope.Subtitle = stringPtr("Answer in terminal")
		envelope.Message = stringPtr(plainTextClip(planSummary(plan), 256))
		envelope.InteractivePrompt = prompt
		state.LastToolName = "exit_plan_mode"
		state.LastEventTitle = title
		SendEnvelope(envelope)

	case "QuestionAsked":
		// The native Ask dialog is open. Its questions ride along as the Chat
		// View card; without an action id no approval bar opens.
		if isChild {
			return
		}
		questions := askQuestions(payload.raw("tool_input"))
		if len(questions) == 0 {
			return
		}
		title := agentDisplayName + " has a question"
		prompt := &InteractivePrompt{Kind: "question", Questions: questions}
		if toolUseID := payload.str("tool_use_id"); toolUseID != "" {
			prompt.ToolUseID = toolUseID
		}
		envelope := baseEnvelope(payload, sessionID, "QuestionAsked")
		envelope.Category = stringPtr("approval_required")
		envelope.ToolName = stringPtr("ask")
		envelope.Title = stringPtr(title)
		envelope.Subtitle = stringPtr("Answer in terminal")
		envelope.Message = stringPtr(plainTextClip(questions[0].Question, 256))
		envelope.InteractivePrompt = prompt
		state.LastToolName = "ask"
		state.LastEventTitle = title
		SendEnvelope(envelope)

	case "PlanResolved":
		// Category-less: the daemon retires the plan card and publishes nothing.
		SendEnvelope(baseEnvelope(payload, sessionID, "PlanResolved"))

	case "QuestionResolved":
		// Category-less: the daemon retires this ask's card, and only it. Its
		// toolUseId keeps a resolution handled before its question from leaving
		// that question pending.
		envelope := baseEnvelope(payload, sessionID, "QuestionResolved")
		if toolUseID := payload.str("tool_use_id"); toolUseID != "" {
			envelope.InteractivePrompt = &InteractivePrompt{Kind: "question", ToolUseID: toolUseID}
		}
		SendEnvelope(envelope)

	case "PermissionResolved":
		if isChild && agentSource == "omp" {
			return
		}
		title := agentDisplayName + " resumed"
		envelope := baseEnvelope(payload, sessionID, "PermissionResolved")
		// The daemon closes the open approval card by its action id.
		if actionID := payload.str("tool_use_id"); actionID != "" {
			envelope.ActionID = stringPtr(actionID)
		}
		envelope.Category = stringPtr("session_started")
		envelope.Title = stringPtr(title)
		envelope.Message = stringPtr(formatUserPromptDetail(state.LastUserPrompt))
		envelope.ToolName = stringPtr(state.LastToolName)
		state.LastEventTitle = title
		SendEnvelope(envelope)

	case "AgentEnd", "TurnEnd":
		if isChild {
			return
		}
		assistant := payload.str("last_assistant_message")
		if !recordTaskCompletion(state, plainTextClip(assistant, 200), nowSeconds()) {
			return
		}
		title := plainTextClip(assistant, 80)
		if title == "" {
			title = agentDisplayName + " complete"
		}
		envelope := baseEnvelope(payload, sessionID, payload.HookEventName)
		envelope.Category = stringPtr("task_complete")
		envelope.Title = stringPtr(title)
		envelope.Message = stringPtr(formatUserPromptDetail(state.LastUserPrompt))
		state.LastEventTitle = title
		SendEnvelope(envelope)

	default:
		SendEnvelope(baseEnvelope(payload, sessionID, payload.HookEventName))
	}
}

// askQuestions is the in-module port of interactivePromptQuestions in
// moshi-hook's internal/cli/hook_interactive_prompt.go, over OMP's ask
// arguments (TS: askQuestions, L700).
func askQuestions(input any) []Question {
	record, _ := input.(map[string]any)
	rawQuestions, _ := record["questions"].([]any)
	out := make([]Question, 0, len(rawQuestions))
	for _, item := range rawQuestions {
		rawQuestion, ok := item.(map[string]any)
		if !ok {
			continue
		}
		question := firstString(rawQuestion["question"])
		if question == "" {
			continue
		}
		options := []QuestionOption{}
		hasPreview := false
		rawOptions, _ := rawQuestion["options"].([]any)
		for _, option := range rawOptions {
			rawOption, ok := option.(map[string]any)
			if !ok {
				continue
			}
			label := firstString(rawOption["label"])
			if label == "" {
				continue
			}
			options = append(options, QuestionOption{Label: label, Description: firstString(rawOption["description"])})
			if firstString(rawOption["preview"]) != "" {
				hasPreview = true
			}
		}
		entry := Question{
			Header:      firstString(rawQuestion["header"]),
			Question:    question,
			MultiSelect: rawQuestion["multiSelect"] == true || rawQuestion["multi_select"] == true || rawQuestion["multi"] == true || rawQuestion["multiple"] == true,
			Options:     options,
			HasPreview:  hasPreview,
			ID:          firstString(rawQuestion["id"]),
		}
		if recommended, ok := rawQuestion["recommended"].(float64); ok &&
			recommended == math.Trunc(recommended) && recommended >= 0 && recommended < float64(len(options)) {
			entry.RecommendedIndex = intPtr(int(recommended))
		}
		out = append(out, entry)
	}
	return out
}

// PlanContext is the session surface planFileOnDisk needs: ctx.cwd,
// sessionManager.getArtifactsDir and transcriptPath(ctx).
type PlanContext interface {
	Cwd() string
	TranscriptPath() string
	ArtifactsDir() string
}

// planFileOnDisk resolves OMP's plan path: local:// lives under the session's
// artifacts directory (the transcript path without .jsonl); anything else is
// relative to the session cwd (TS: planFileOnDisk, L739).
func planFileOnDisk(planFilePath string, ctx PlanContext) string {
	if planFilePath == "" {
		return ""
	}
	const localPrefix = "local://"
	if !strings.HasPrefix(planFilePath, localPrefix) {
		if isAbsolutePath(planFilePath) {
			return planFilePath
		}
		base := ctx.Cwd()
		if base == "" {
			base = processCwd()
		}
		return filepath.Join(base, planFilePath)
	}
	artifactsDir := ctx.ArtifactsDir()
	if artifactsDir == "" {
		if transcript := ctx.TranscriptPath(); strings.HasSuffix(transcript, ".jsonl") {
			artifactsDir = strings.TrimSuffix(transcript, ".jsonl")
		}
	}
	if artifactsDir == "" {
		return ""
	}
	return filepath.Join(artifactsDir, "local", planFilePath[len(localPrefix):])
}

// isAbsolutePath reports whether p is absolute in POSIX or Win32 form, as
// node's path.isAbsolute does on the current platform.
func isAbsolutePath(p string) bool {
	if p == "" {
		return false
	}
	if path.IsAbs(p) {
		return true
	}
	// Win32: a UNC prefix or a drive-letter prefix.
	if len(p) >= 2 && (p[0] == '\\' || p[0] == '/') && (p[1] == '\\' || p[1] == '/') {
		return true
	}
	if len(p) >= 3 {
		drive := p[0]
		if (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			if p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
				return true
			}
		}
	}
	return false
}

// processCwd is node's process.cwd().
func processCwd() string {
	directory, err := os.Getwd()
	if err != nil {
		return ""
	}
	return directory
}
