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
	// agentSource and agentDisplayName are deliberately reported as "pi", not
	// "pig": moshi-hook's phone-side Chat View parser only recognizes the
	// registered agent roster (~20 sources), and "pi" is already registered,
	// so masquerading as pi reuses the phone app's piFamilyTranscriptEngine
	// and its Tier B rendering. The display name stays "Pi" for the same
	// reason — the daemon and app render it verbatim. transcriptPath and the
	// herdr pane fields still point at this pig session's real state, so Chat
	// View opens the right transcript (TS pig L20-28 / TS L18-19).
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
	Model string
	// TranscriptPath holds authoritative resolutions only — a stage-5
	// cwd guess never lands in state because sessionIsChild reads this
	// field as its last fallback.
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
	// stateMu guards sessions, transcriptChildCache and sessionTranscripts.
	// handleEvent holds it for the whole normalization so one session's
	// sequence stays consistent.
	stateMu              sync.Mutex
	sessions             = map[string]*SessionState{}
	transcriptChildCache = map[string]bool{}
	// sessionTranscripts caches the transcript path each session resolved
	// through authoritative sources (stage 1/2 of resolveTranscriptPath) or
	// a stage-4 session-id match, so a later event that arrives without one
	// still knows where the session's transcript lives. Stage-5 cwd guesses
	// are never stored here — they may name another session's file and would
	// shadow the real one once it appears. resolveTranscriptPath is the only
	// writer: handleEvent records its verdict in TranscriptPathGuessed
	// instead of re-caching the value
	// (TS pig: state.transcriptPath, set inside transcriptPath()).
	sessionTranscripts = map[string]string{}
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
	sessionTranscripts = map[string]string{}
	stateMu.Unlock()
}

// pigSessionsRoot returns the root directory that holds one subdirectory per
// session cwd: <agentDir>/sessions, the same layout pi uses under
// ~/.pi/agent/sessions (TS pig: pigSessionsRoot). The agent dir resolves like
// PiG's node shim (runtime-node/shims/pig-config.mjs):
//
//  1. PIG_EXT_HOST_INFO — a JSON object the host hands extensions; its
//     agentDir field wins. The env is parsed directly (no sdk.AgentDir
//     global cache) so t.Setenv keeps tests isolated.
//  2. PIG_CODING_AGENT_DIR, then PI_CODING_AGENT_DIR.
//  3. PIG_HOME, then XDG_CONFIG_HOME/pig, then ~/.pig — these are config
//     roots, so "agent/sessions" is appended.
func pigSessionsRoot() string {
	if hostInfo := os.Getenv("PIG_EXT_HOST_INFO"); hostInfo != "" {
		var parsed struct {
			AgentDir string `json:"agentDir"`
		}
		if err := json.Unmarshal([]byte(hostInfo), &parsed); err == nil && parsed.AgentDir != "" {
			return filepath.Join(parsed.AgentDir, "sessions")
		}
	}
	if envDir := firstString(os.Getenv("PIG_CODING_AGENT_DIR"), os.Getenv("PI_CODING_AGENT_DIR")); envDir != "" {
		return filepath.Join(envDir, "sessions")
	}
	xdg := ""
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		xdg = filepath.Join(xdgConfig, "pig")
	}
	home := ""
	if homeDir, err := os.UserHomeDir(); err == nil {
		home = filepath.Join(homeDir, ".pig")
	}
	configRoot := firstString(os.Getenv("PIG_HOME"), xdg, home)
	if configRoot == "" {
		return ""
	}
	return filepath.Join(configRoot, "agent", "sessions")
}

// encodeSessionCwd encodes a cwd exactly like PiG's
// session.DefaultDirForCWD: the leading / or \ goes away, every remaining
// / \ : becomes -, and the result is wrapped in "--" (TS pig:
// encodeSessionCwd).
func encodeSessionCwd(cwd string) string {
	safe := cwd
	if len(safe) > 0 && (safe[0] == '/' || safe[0] == '\\') {
		safe = safe[1:]
	}
	var builder strings.Builder
	for _, r := range safe {
		if r == '/' || r == '\\' || r == ':' {
			builder.WriteByte('-')
		} else {
			builder.WriteRune(r)
		}
	}
	return "--" + builder.String() + "--"
}

// pigSessionDirForCwd is the per-cwd session directory; "" when the cwd or
// the sessions root is unknown (TS pig: pigSessionDirForCwd).
func pigSessionDirForCwd(cwd string) string {
	root := pigSessionsRoot()
	if cwd == "" || root == "" {
		return ""
	}
	return filepath.Join(root, encodeSessionCwd(cwd))
}

// newestJsonl is the newest *.jsonl in a directory; "" when none exists yet
// (session_start fires before the first transcript write — the file lands
// by the time UserPromptSubmit does) (TS pig: newestJsonl).
func newestJsonl(dir string) string {
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	var bestTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if best == "" || !info.ModTime().Before(bestTime) {
			bestTime = info.ModTime()
			best = filepath.Join(dir, entry.Name())
		}
	}
	return best
}

// findSessionFileByID scans the per-cwd session directories for
// <timestamp>_<sessionId>.jsonl. Session ids are unique per transcript, so
// this identifies the session's file even when ctx.cwd and the host's
// session-dir cwd disagree (a worker launched from a tab root but told to
// work in a worktree). Only the one-level scan of the sessions root runs;
// it stays cheap because the root holds one directory per project cwd
// (TS pig: findSessionFileById).
func findSessionFileByID(sessionID string) string {
	if sessionID == "" || sessionID == agentSource {
		return ""
	}
	root := pigSessionsRoot()
	if root == "" {
		return ""
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	suffix := "_" + sessionID + ".jsonl"
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		names, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, name := range names {
			if strings.Contains(name.Name(), suffix) {
				return filepath.Join(dir, name.Name())
			}
		}
	}
	return ""
}

// eventTranscriptPath is level 2 of the resolution chain: transcript fields a
// host may put straight onto the event payload (TS pig reads the same names
// off the ctx object, which carries them through here).
func eventTranscriptPath(event map[string]any) string {
	return firstString(
		eventString(event, "transcript_path"),
		eventString(event, "transcriptPath"),
		eventString(event, "sessionFile"),
		eventString(event, "session_file"),
		eventString(event, "sessionPath"),
	)
}

// eventString reads a non-empty string off an event map; nil-safe.
func eventString(event map[string]any, key string) string {
	if event == nil {
		return ""
	}
	value, _ := event[key].(string)
	return value
}

// resolveTranscriptPath is the transcript resolution chain (TS pig:
// transcriptPath). MOSHI's Chat View tails this file, so it must be the real
// pig transcript — never a path under ~/.pi.
//
// Authoritative sources outrank the cache: a stage-4/5 guess made before the
// transcript landed on disk (e.g. session_start resolving to the previous
// session's file under the same cwd) must not shadow the real sessionFile
// when it arrives. Stages 1-3 are trusted and refresh the cache; stages 4-5
// are fallible guesses that are never cached.
//
//  1. sessionFile — the host sessionManager's getSessionFile(), the
//     authoritative answer; always wins and refreshes the cache.
//  2. event fields — transcript_path / transcriptPath / sessionFile /
//     session_file / sessionPath; also authoritative, refreshes the cache.
//  3. sessionTranscripts[sessionID] — a path already resolved and cached by
//     stage 1/2; consulted only when no authoritative value is present this
//     time.
//  4. findSessionFileByID(sessionID) — the session-id scan of the sessions
//     root; a real session match, cached.
//  5. newestJsonl(pigSessionDirForCwd(cwd)) — the newest transcript in this
//     cwd's session directory; a guess, returned but never cached.
//
// guessed reports whether the result is a fallible stage-5 guess (true) or a
// trusted stage 1-4 value (false). Callers use it to keep guesses out of both
// the cache and lineage detection.
func resolveTranscriptPath(sessionFile string, event map[string]any, sessionID, cwd string) (path string, guessed bool) {
	// Stage 1: the host session file is authoritative — take it verbatim and
	// record it so later events without one still resolve.
	if sessionFile != "" {
		stateMu.Lock()
		sessionTranscripts[sessionID] = sessionFile
		stateMu.Unlock()
		return sessionFile, false
	}
	// Stage 2: event-carried transcript fields are authoritative too — they
	// replace whatever an earlier guess cached.
	if fromEvent := eventTranscriptPath(event); fromEvent != "" {
		stateMu.Lock()
		sessionTranscripts[sessionID] = fromEvent
		stateMu.Unlock()
		return fromEvent, false
	}
	// Stage 3: the cache stages 1/2/4 fill. Send() calls us before
	// handleEvent takes stateMu, so take it here for the read — the critical
	// section is a single map lookup.
	stateMu.Lock()
	cached := sessionTranscripts[sessionID]
	stateMu.Unlock()
	if cached != "" {
		return cached, false
	}
	// Stage 4: a filename-bound session id is a real match (a wrongly-guessed
	// stage-5 file never reaches the cache, so the scan stays reachable once
	// the session's own transcript exists).
	if byID := findSessionFileByID(sessionID); byID != "" {
		stateMu.Lock()
		sessionTranscripts[sessionID] = byID
		stateMu.Unlock()
		return byID, false
	}
	// Stage 5: newest *.jsonl under this cwd is a heuristic only — the file
	// may belong to a different session that happens to share the cwd. It is
	// returned flagged as a guess so it is never cached, and a later
	// authoritative stage-1/2 value or a stage-4 id match can still correct
	// it.
	return newestJsonl(pigSessionDirForCwd(cwd)), true
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
//
// TranscriptPathGuessed records resolveTranscriptPath's verdict on
// TranscriptPath: false for a trusted stage 1-4 value, true for a stage-5 cwd
// guess. Only handleEvent reads it — the field never leaves the process.
type HookPayload struct {
	HookEventName         string
	SessionID             string
	TranscriptPath        string
	TranscriptPathGuessed bool
	Cwd                   string
	Model                 string
	ContextRemaining      *float64
	AgentKind             string
	Extra                 map[string]any
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
// (TS pig: sessionIsChild). This file is generated for Pi, so the omp
// agent_kind branch the generated TS carries for other families is gone.
func sessionIsChild(state *SessionState, payload *HookPayload, event map[string]any) bool {
	filePath := ""
	if !payload.TranscriptPathGuessed {
		// A stage-5 cwd guess may name another session's file: reading its
		// header would suppress this session as a false lineage child. Trusted
		// resolutions (stage 1-4) still identify the session on their own.
		filePath = payload.TranscriptPath
	}
	filePath = firstString(filePath, eventTranscriptPath(event), state.TranscriptPath)
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
// (TS: handleEvent, L532). event is the raw host event the payload was built
// from; sessionIsChild consults it as a transcript-field fallback.
func handleEvent(payload *HookPayload, event map[string]any) {
	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	state := stateFor(sessionID)
	if payload.TranscriptPath != "" && !payload.TranscriptPathGuessed {
		// Cache ownership stays with resolveTranscriptPath, which already
		// stored every trusted (non-guessed) value; this write only matters
		// for payloads built by hand, and it must never admit a stage-5
		// guess — that would resurrect the cached-guess shadowing the
		// stage-4 id scan was meant to fix.
		sessionTranscripts[sessionID] = payload.TranscriptPath
	}
	isChild := sessionIsChild(state, payload, event)

	switch payload.HookEventName {
	case "SessionStart":
		// A lineage child's start still announces itself: the pane takeover is
		// intended (the TS pig SessionStart branch has no isChild filter).
		if payload.Model != "" {
			state.Model = payload.Model
		}
		if payload.TranscriptPath != "" && !payload.TranscriptPathGuessed {
			// state.TranscriptPath stores authoritative values only — a
			// stage-5 guess never lands here: sessionIsChild falls back to
			// this field and would read a foreign session's header,
			// suppressing this session as a false lineage child.
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
		if isChild {
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
		if isChild {
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
