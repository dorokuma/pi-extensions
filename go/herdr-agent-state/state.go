package herdragentstate

import (
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// AgentState represents herdr agent states.
type AgentState string

const (
	StateIdle    AgentState = "idle"
	StateWorking AgentState = "working"
	StateBlocked AgentState = "blocked"
)

// QueuedState holds the pending state and message to deliver.
// Matches TS QueuedState (lines 59-63).
type QueuedState struct {
	State   string
	Message *string
	Seq     uint64
}

// StateManager orchestrates agent status machine, serialization queue,
// and Pi/PiG lifecycle events.
type StateManager struct {
	mu     sync.Mutex
	client *Client

	// State machine fields matching TS lines 184-189
	agentActive    bool
	blockedCount   int
	blockedMessage string
	lastState      *string
	lastMessage    *string
	rootSession    bool

	// Session references matching TS lines 66-67
	currentAgentSessionPath *string
	currentAgentSessionID   *string

	// Serialization queue matching TS lines 149-150
	sendInFlight bool
	queuedState  *QueuedState

	// Optional channel used in unit tests to observe drain completion
	drainNotifyChan chan struct{}
}

// NewStateManager creates a new StateManager.
func NewStateManager(client *Client) *StateManager {
	return &StateManager{
		client: client,
	}
}

// SetDrainNotifyChan injects a notification channel for tests to wait for queue draining.
func (sm *StateManager) SetDrainNotifyChan(ch chan struct{}) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.drainNotifyChan = ch
}

func stringPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// getSessionRefLocked extracts current session ref while holding sm.mu.
func (sm *StateManager) getSessionRefLocked() SessionRef {
	var ref SessionRef
	if sm.currentAgentSessionPath != nil {
		p := *sm.currentAgentSessionPath
		ref.Path = &p
	}
	if sm.currentAgentSessionID != nil {
		id := *sm.currentAgentSessionID
		ref.ID = &id
	}
	return ref
}

// SessionInfoProvider abstracts the Context properties needed by herdr-agent-state lifecycle handlers.
type SessionInfoProvider interface {
	Mode() string
	IsIdle() (bool, error)
	GetSessionFile() (*string, error)
	GetSessionID() (string, error)
}

type realSessionContext struct {
	ctx sdk.Context
}

func (r realSessionContext) Mode() string {
	return r.ctx.Mode()
}

func (r realSessionContext) IsIdle() (bool, error) {
	return r.ctx.IsIdle()
}

func (r realSessionContext) GetSessionFile() (*string, error) {
	return r.ctx.SessionManager().GetSessionFile()
}

func (r realSessionContext) GetSessionID() (string, error) {
	return r.ctx.SessionManager().GetSessionID()
}

// UpdateSessionRef updates session references from the given Context.
// Matches TS lines 74-92 (updateSessionRef).
func (sm *StateManager) UpdateSessionRef(ctx sdk.Context) {
	sm.updateSessionRefCore(realSessionContext{ctx: ctx})
}

func (sm *StateManager) updateSessionRefCore(provider SessionInfoProvider) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Session file: TS lines 75-84
	sm.currentAgentSessionPath = nil
	if file, err := provider.GetSessionFile(); err == nil && file != nil && *file != "" {
		if IsAbsolutePosixOrWin32(*file) {
			f := *file
			sm.currentAgentSessionPath = &f
		}
	}

	// 2. Session ID: TS lines 86-91
	sm.currentAgentSessionID = nil
	if id, err := provider.GetSessionID(); err == nil && len(id) > 0 {
		sm.currentAgentSessionID = &id
	}
}

// DesiredState computes the next target agent state and message.
// Matches TS lines 191-199 (desiredState).
func (sm *StateManager) DesiredState() (string, *string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.desiredStateLocked()
}

func (sm *StateManager) desiredStateLocked() (string, *string) {
	if sm.blockedCount > 0 {
		var msg *string
		if sm.blockedMessage != "" {
			m := sm.blockedMessage
			msg = &m
		}
		return string(StateBlocked), msg
	}
	if sm.agentActive {
		return string(StateWorking), nil
	}
	return string(StateIdle), nil
}

// PublishState checks whether the state has changed (or force is true) and queues it.
// Matches TS lines 201-209 (publishState).
func (sm *StateManager) PublishState(force bool) {
	sm.mu.Lock()
	state, message := sm.desiredStateLocked()

	if !force && sm.lastState != nil && *sm.lastState == state && stringPtrEqual(sm.lastMessage, message) {
		sm.mu.Unlock()
		return
	}

	s := state
	sm.lastState = &s
	if message != nil {
		m := *message
		sm.lastMessage = &m
	} else {
		sm.lastMessage = nil
	}

	// Queue state: TS lines 152-157 (queueState)
	sm.queuedState = &QueuedState{
		State:   state,
		Message: message,
		Seq:     NextReportSeq(),
	}

	if sm.sendInFlight {
		sm.mu.Unlock()
		return
	}

	sm.sendInFlight = true
	sm.mu.Unlock()

	go sm.drainLoop()
}

// drainLoop implements serialization queue draining.
// Matches TS lines 159-177 (drainStateQueue).
func (sm *StateManager) drainLoop() {
	for {
		sm.mu.Lock()
		if sm.queuedState == nil {
			sm.sendInFlight = false
			if sm.drainNotifyChan != nil {
				select {
				case sm.drainNotifyChan <- struct{}{}:
				default:
				}
			}
			sm.mu.Unlock()
			return
		}

		next := sm.queuedState
		sm.queuedState = nil
		sessionRef := sm.getSessionRefLocked()
		sm.mu.Unlock()

		// Send outside of lock
		sm.client.SendReportAgent(sessionRef, next.State, next.Message, next.Seq)
	}
}

// PrepareReportSession extracts sessionRef and allocates seq synchronously.
// Matches TS lines 114-132: nextReportSeq() is invoked synchronously during request preparation.
func (sm *StateManager) PrepareReportSession(sessionStartSource *string) (Request, bool) {
	sm.mu.Lock()
	sessionRef := sm.getSessionRefLocked()
	sm.mu.Unlock()

	return sm.client.BuildReportAgentSessionRequest(sessionRef, sessionStartSource)
}

// ReportSession sends pane.report_agent_session matching TS lines 114-132.
func (sm *StateManager) ReportSession(sessionStartSource *string) {
	if req, ok := sm.PrepareReportSession(sessionStartSource); ok {
		sm.client.SendRequest(req)
	}
}

// HandleHerdrBlocked handles "herdr:blocked" event bus message.
// Matches TS lines 211-227.
func (sm *StateManager) HandleHerdrBlocked(_ sdk.Context, data any) error {
	sm.mu.Lock()
	if !sm.rootSession {
		sm.mu.Unlock()
		return nil
	}
	sm.mu.Unlock()

	m, _ := data.(map[string]any)
	active, _ := m["active"].(bool)

	if !active {
		sm.mu.Lock()
		if sm.blockedCount > 0 {
			sm.blockedCount--
		}
		if sm.blockedCount == 0 {
			sm.blockedMessage = ""
		}
		sm.mu.Unlock()
		sm.PublishState(false)
		return nil
	}

	sm.mu.Lock()
	sm.blockedCount++
	if label, ok := m["label"].(string); ok {
		sm.blockedMessage = label
	} else {
		sm.blockedMessage = ""
	}
	sm.mu.Unlock()

	sm.PublishState(false)
	return nil
}

// HandleSessionStart handles session_start event.
// Matches TS lines 229-241.
func (sm *StateManager) HandleSessionStart(ctx sdk.Context, data map[string]any) (any, error) {
	var sessionStartSource *string
	if data != nil {
		if reason, ok := data["reason"].(string); ok && reason != "" {
			sessionStartSource = &reason
		}
	}
	sm.handleSessionStartCore(realSessionContext{ctx: ctx}, sessionStartSource)
	return nil, nil
}

func (sm *StateManager) handleSessionStartCore(provider SessionInfoProvider, sessionStartSource *string) {
	// TUI only: RPC/JSON/print modes are headless (no PTY herdr can display)
	if provider.Mode() != "tui" {
		return
	}

	sm.mu.Lock()
	sm.rootSession = true
	sm.mu.Unlock()

	sm.updateSessionRefCore(provider)

	// Synchronously prepare session report request, snapshotting sessionRef and assigning seq
	// before publishing next agent state.
	// This guarantees report_agent_session.seq < report_agent.seq without blocking host's session_start handler.
	if req, ok := sm.PrepareReportSession(sessionStartSource); ok {
		go sm.client.SendRequest(req)
	}

	// In TS: agentActive = ctx?.isIdle?.() === false
	isIdle, err := provider.IsIdle()
	sm.mu.Lock()
	sm.agentActive = (err == nil && !isIdle)
	sm.mu.Unlock()

	sm.PublishState(true)
}

// HandleAgentStart handles agent_start event.
// Matches TS lines 243-251.
func (sm *StateManager) HandleAgentStart(ctx sdk.Context, _ map[string]any) (any, error) {
	sm.handleAgentStartCore(realSessionContext{ctx: ctx})
	return nil, nil
}

func (sm *StateManager) handleAgentStartCore(provider SessionInfoProvider) {
	sm.mu.Lock()
	if !sm.rootSession {
		sm.mu.Unlock()
		return
	}
	sm.mu.Unlock()

	sm.updateSessionRefCore(provider)

	// Synchronously prepare session report request, snapshotting sessionRef and assigning seq
	// before publishing next agent state.
	// This guarantees report_agent_session.seq < report_agent.seq, matching TS synchronous execution.
	if req, ok := sm.PrepareReportSession(nil); ok {
		go sm.client.SendRequest(req)
	}

	sm.mu.Lock()
	sm.agentActive = true
	sm.mu.Unlock()

	sm.PublishState(false)
}

// HandleAgentSettled handles agent_settled event.
// Matches TS lines 253-260.
func (sm *StateManager) HandleAgentSettled(ctx sdk.Context, _ map[string]any) (any, error) {
	sm.handleAgentSettledCore(realSessionContext{ctx: ctx})
	return nil, nil
}

func (sm *StateManager) handleAgentSettledCore(provider SessionInfoProvider) {
	sm.mu.Lock()
	if !sm.rootSession {
		sm.mu.Unlock()
		return
	}
	sm.mu.Unlock()

	isIdle, err := provider.IsIdle()
	if err != nil || !isIdle {
		return
	}

	sm.mu.Lock()
	sm.agentActive = false
	sm.mu.Unlock()

	sm.PublishState(false)
}
