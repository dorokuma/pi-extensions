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
// Seq is the sequence number of the report. It is assigned when the report is
// handed to the sending lifecycle (in the same critical section in which the
// report is queued there), so it is 0 while the report is still queued.
type QueuedState struct {
	State   string
	Message *string
	Seq     uint64
}

// outboxKind identifies the request kind carried by an outbox item.
type outboxKind int

const (
	// outboxSession carries a pane.report_agent_session request.
	outboxSession outboxKind = iota
	// outboxState carries a pane.report_agent request.
	outboxState
	// outboxRelease carries the pane.release_agent request.
	outboxRelease
)

// outboxItem is one request of the single sending lifecycle of this source.
//
// The sequence number is allocated under sm.mu in the same critical section in
// which the item is appended to the outbox, so the order in which the
// requests reach the wire is always the order of their sequence numbers.
type outboxItem struct {
	kind  outboxKind
	epoch uint64
	seq   uint64

	// session report payload
	sessionRef         SessionRef
	sessionStartSource *string
	resumeArgv         []string

	// state report payload
	state   string
	message *string

	// result carries the delivery outcome of a state report (true when Herdr
	// accepted it). It is buffered, so the dispatcher never blocks on it.
	result chan bool

	// done is closed once the item has been sent (or dropped).
	done chan struct{}
}

// StateManager orchestrates agent status machine, serialization queue,
// and Pi/PiG lifecycle events.
type StateManager struct {
	mu     sync.Mutex
	client *Client

	// sessionEpoch is the generation counter of the session that currently owns
	// this state machine. It is protected by sm.mu and is bumped whenever the
	// session lifecycle changes hands:
	//
	//   - handleSessionStartCore() bumps it and opens a new generation;
	//   - HandleSessionShutdown() bumps it and closes the generation.
	//
	// Every handler captures the epoch it observed before it leaves the lock and
	// re-checks it afterwards, so a handler that was overtaken by a session
	// switch (e.g. one still waiting for provider.IsIdle() while the session
	// was shut down) can no longer touch the state machine, publish a state or
	// send a session report for the generation it no longer belongs to.
	//
	// Epoch 0 means "no session lifecycle has run yet" and keeps the pure
	// state-machine unit tests (which drive PublishState directly) working.
	sessionEpoch uint64

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

	// agentStateReported records whether Herdr has already accepted an agent
	// state report from this source for the pane. Herdr refuses a resume command
	// ("resume_not_accepted") until the reporter holds the pane by reporting its
	// state first, so the flag is set only after a report is delivered (see
	// drainLoop) and reset by HandleSessionShutdown.
	agentStateReported bool

	// Serialization queue matching TS lines 149-150
	sendInFlight bool
	queuedState  *QueuedState

	// inFlightDone is closed by the running drainLoop right before it clears
	// sendInFlight, i.e. after the last in-flight SendReportAgent has fully
	// returned (retry window included). HandleSessionShutdown waits on it so
	// pane.release_agent is sent only once no report_agent remains in flight,
	// keeping release_agent.seq > any sent report_agent.seq and ensuring no
	// report lands at Herdr after the release. Created in PublishState just
	// before the drainLoop goroutine starts; nil while no drain is running.
	inFlightDone chan struct{}

	// outbox is the single sending lifecycle of this source: the session
	// report, the state reports and the final release are appended here
	// under sm.mu and written to the socket by one dispatcher goroutine, in
	// the order in which they were appended. A report can therefore never
	// overtake another one, and the sequence numbers that are allocated under
	// the lock describe exactly that order.
	outbox []*outboxItem

	// outboxCond wakes the dispatcher when an item was appended.
	outboxCond *sync.Cond

	// dispatcherRunning is true once the dispatcher goroutine was started.
	dispatcherRunning bool

	// activityTickets hands out a strictly increasing ticket to every agent
	// activity event (session_start, agent_start, agent_settled) when its
	// handler starts; activityApplied remembers the ticket of the last
	// applied decision.
	//
	// The handlers read the provider (IsIdle) outside the lock, so their
	// observation may be outdated by the time it is applied. A decision is
	// applied only when no newer activity event has been applied in the
	// meantime: an outdated idle observation can therefore never overwrite
	// the newer "working" state of a following agent_start.
	//
	// The tickets cover the agent activity only. The blocked counter keeps
	// its own bookkeeping, so a blocked update is never dropped by this
	// mechanism and the blocked priority is unchanged.
	activityTickets uint64
	activityApplied uint64

	// Optional channel used in unit tests to observe drain completion
	drainNotifyChan chan struct{}
}

// NewStateManager creates a new StateManager.
func NewStateManager(client *Client) *StateManager {
	sm := &StateManager{
		client: client,
	}
	sm.outboxCond = sync.NewCond(&sm.mu)
	return sm
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

// updateSessionRefCoreForEpoch refreshes the session references for the given
// generation. The provider is queried outside the lock (it may block on I/O);
// the generation and the root session are re-checked under the lock before the
// result is stored, so a handler that was overtaken by a session switch cannot
// overwrite the references of the new session.
// Returns false when the caller is stale and nothing was written.
func (sm *StateManager) updateSessionRefCoreForEpoch(provider SessionInfoProvider, epoch uint64) bool {
	// Session file: TS lines 75-84
	var path *string
	if file, err := provider.GetSessionFile(); err == nil && file != nil && *file != "" {
		if IsAbsolutePosixOrWin32(*file) {
			f := *file
			path = &f
		}
	}

	// Session ID: TS lines 86-91
	var id *string
	if sid, err := provider.GetSessionID(); err == nil && len(sid) > 0 {
		i := sid
		id = &i
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.rootSession || sm.sessionEpoch != epoch {
		return false
	}
	sm.currentAgentSessionPath = path
	sm.currentAgentSessionID = id
	return true
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
	defer sm.mu.Unlock()

	// A manager that never ran the session lifecycle (sessionEpoch == 0) keeps
	// the legacy behaviour so the pure state-machine unit tests can drive it
	// directly. Once a session has been started or shut down, a state may only
	// be published from inside the current root session.
	if !sm.rootSession && sm.sessionEpoch > 0 {
		return
	}

	sm.publishStateLocked(force, sm.sessionEpoch)
}

// publishStateLocked computes the desired state, updates the deduplication
// memory and queues the state for delivery. It must be called with sm.mu held.
//
// epoch is the session generation the caller observed; a mismatch means the
// caller was overtaken by a session switch and the publication is dropped.
// Publishing under the same lock acquisition as the state mutation closes the
// window in which a concurrent shutdown could reset the state machine between
// the mutation and the publication.
func (sm *StateManager) publishStateLocked(force bool, epoch uint64) {
	if epoch != sm.sessionEpoch {
		return
	}

	state, message := sm.desiredStateLocked()

	if !force && sm.lastState != nil && *sm.lastState == state && stringPtrEqual(sm.lastMessage, message) {
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
	// The sequence number is not allocated here: it is allocated when the
	// report is handed to the sending lifecycle, in the same critical section
	// in which it is queued there, so the order of the reports on the wire
	// matches the order of their sequence numbers.
	sm.queuedState = &QueuedState{
		State:   state,
		Message: message,
	}

	sm.startDrainLocked()
}

// startDrainLocked starts the drain goroutine when no drain is running and a
// state is queued. It must be called with sm.mu held.
func (sm *StateManager) startDrainLocked() {
	if sm.sendInFlight || sm.queuedState == nil {
		return
	}
	sm.sendInFlight = true
	done := make(chan struct{})
	sm.inFlightDone = done
	go sm.drainLoop(done, sm.sessionEpoch)
}

// finishDrainLocked closes the completion channel of the drain identified by
// done and clears the in-flight bookkeeping for it. It must be called with
// sm.mu held. The in-flight flags are only cleared when they still belong to
// this drain, so a drain that was already superseded by a newer one cannot
// release the newer drain's bookkeeping.
func (sm *StateManager) finishDrainLocked(done chan struct{}) {
	if sm.inFlightDone == done {
		sm.sendInFlight = false
		sm.inFlightDone = nil
	}
	select {
	case <-done:
		// already closed
	default:
		close(done)
	}
	if sm.drainNotifyChan != nil {
		select {
		case sm.drainNotifyChan <- struct{}{}:
		default:
		}
	}
}

// drainStaleLocked reports whether the drain of the given generation must stop:
// the session generation moved on (a new session started or the session was
// shut down) or the manager left the root session.
func (sm *StateManager) drainStaleLocked(epoch uint64) bool {
	if sm.sessionEpoch != epoch {
		return true
	}
	return sm.sessionEpoch > 0 && !sm.rootSession
}

// restartDrainLocked starts a new drain when the aborted generation left a
// queued state behind that belongs to the current generation (e.g. a new
// session started while the previous drain was still in flight). It must be
// called with sm.mu held.
func (sm *StateManager) restartDrainLocked() {
	if sm.sendInFlight || sm.queuedState == nil {
		return
	}
	if sm.sessionEpoch > 0 && !sm.rootSession {
		return
	}
	sm.startDrainLocked()
}

// drainLoop implements serialization queue draining.
// Matches TS lines 159-177 (drainStateQueue).
// done is closed exactly once, right before sendInFlight is cleared and after
// the last in-flight state report has fully returned, so a concurrent
// HandleSessionShutdown can wait for the send to end before releasing the pane.
// epoch is the session generation the drain belongs to: once the generation
// moves on, the drain abandons the queue instead of reporting a state for a
// session that no longer exists.
//
// The drain does not write to the socket itself: it hands the report to the
// shared sending lifecycle and waits for the delivery outcome, so the state
// reports, the session reports and the final release share one send order.
func (sm *StateManager) drainLoop(done chan struct{}, epoch uint64) {
	for {
		sm.mu.Lock()
		if sm.drainStaleLocked(epoch) || sm.queuedState == nil {
			sm.finishDrainLocked(done)
			sm.restartDrainLocked()
			sm.mu.Unlock()
			return
		}

		next := sm.queuedState
		sm.queuedState = nil
		// The sequence number is allocated in the same critical section in
		// which the report is handed to the sending lifecycle, so the order
		// of the reports on the wire matches the order of their sequence
		// numbers.
		next.Seq = NextReportSeq()
		accepted := make(chan bool, 1)
		sm.enqueueOutboxLocked(&outboxItem{
			kind:    outboxState,
			epoch:   epoch,
			seq:     next.Seq,
			state:   next.State,
			message: next.Message,
			result:  accepted,
		})
		sm.mu.Unlock()

		// Wait for the delivery outcome outside the lock: the network I/O
		// must never run while sm.mu is held.
		ok := <-accepted

		sm.mu.Lock()
		if sm.drainStaleLocked(epoch) {
			// The session was shut down (or replaced) while this report was in
			// flight: the state machine was already reset, so the pane must not
			// be marked as held, the abandoned queue is not drained further, and
			// the waiting shutdown is released to send pane.release_agent.
			sm.finishDrainLocked(done)
			sm.restartDrainLocked()
			sm.mu.Unlock()
			return
		}
		if ok {
			// Only once Herdr has accepted at least one state report does this
			// source hold the pane (hook_authority established); from then on
			// PrepareReportSession may attach resume_argv.
			sm.agentStateReported = true
		}
		sm.mu.Unlock()
	}
}

// enqueueOutboxLocked appends an item to the sending lifecycle and wakes the
// dispatcher. It must be called with sm.mu held.
func (sm *StateManager) enqueueOutboxLocked(item *outboxItem) {
	if sm.outboxCond == nil {
		sm.outboxCond = sync.NewCond(&sm.mu)
	}
	sm.outbox = append(sm.outbox, item)
	if !sm.dispatcherRunning {
		sm.dispatcherRunning = true
		go sm.dispatchLoop()
	}
	sm.outboxCond.Signal()
}

// dispatchLoop is the single sending goroutine of this source. It writes the
// queued requests to the socket one after another, so a session report, a
// state report and the final release never overtake each other.
//
// The goroutine is started once and stays alive for the lifetime of the
// manager: every request goes through it, so no report is ever sent by an
// untracked goroutine.
func (sm *StateManager) dispatchLoop() {
	for {
		sm.mu.Lock()
		for len(sm.outbox) == 0 {
			sm.outboxCond.Wait()
		}
		item := sm.outbox[0]
		sm.outbox = sm.outbox[1:]
		sm.mu.Unlock()

		sm.dispatchItem(item)
	}
}

// dispatchItem sends one queued request. The staleness check and the payload
// snapshot happen under the lock; the network I/O runs outside of it.
func (sm *StateManager) dispatchItem(item *outboxItem) {
	switch item.kind {
	case outboxSession:
		sm.mu.Lock()
		if !sm.sessionReportAllowedLocked(item.epoch) {
			// The session this report belongs to is gone: it must not be
			// delivered any more.
			sm.mu.Unlock()
			sm.finishOutboxItem(item, false)
			return
		}
		req, ok := sm.client.BuildReportAgentSessionRequestWithSeq(item.sessionRef, item.sessionStartSource, item.resumeArgv, item.seq)
		sm.mu.Unlock()
		if !ok {
			sm.finishOutboxItem(item, false)
			return
		}
		sm.client.SendRequest(req)
		sm.finishOutboxItem(item, true)

	case outboxState:
		sm.mu.Lock()
		if sm.drainStaleLocked(item.epoch) {
			sm.mu.Unlock()
			sm.finishOutboxItem(item, false)
			return
		}
		sessionRef := sm.getSessionRefLocked()
		resumeArgv := ResumeCommand(sessionRef)
		sm.mu.Unlock()
		accepted := sm.client.SendReportAgent(sessionRef, item.state, item.message, item.seq, resumeArgv)
		sm.finishOutboxItem(item, accepted)

	case outboxRelease:
		sm.mu.Lock()
		if sm.sessionEpoch != item.epoch || sm.rootSession {
			// A new session took over this manager in the meantime: an
			// outdated release would wipe the pane hold of the new session,
			// so it is dropped instead of being sent.
			sm.mu.Unlock()
			sm.finishOutboxItem(item, false)
			return
		}
		sm.mu.Unlock()
		sm.client.SendReleaseAgentWithSeq(item.seq)
		sm.finishOutboxItem(item, true)
	}
}

// finishOutboxItem reports the outcome of an item: the delivery result of a
// state report is handed to the waiting drain and the completion signal of
// the item is closed.
func (sm *StateManager) finishOutboxItem(item *outboxItem, ok bool) {
	if item.result != nil {
		item.result <- ok
	}
	if item.done != nil {
		close(item.done)
	}
}

// queueSessionReportLocked snapshots the payload of a pane.report_agent_session
// and hands it to the sending lifecycle. The snapshot and the sequence number
// are taken under the same lock, so the session report is always sequenced
// before the state report that follows it, and the resume command is attached
// exactly when the pane is already held.
// It must be called with sm.mu held.
func (sm *StateManager) queueSessionReportLocked(sessionStartSource *string, epoch uint64) bool {
	if !sm.sessionReportAllowedLocked(epoch) {
		return false
	}
	sessionRef := sm.getSessionRefLocked()
	if sessionRef.Path == nil && sessionRef.ID == nil {
		return false
	}
	var resumeArgv []string
	if sm.agentStateReported {
		resumeArgv = ResumeCommand(sessionRef)
	}
	sm.enqueueOutboxLocked(&outboxItem{
		kind:               outboxSession,
		epoch:              epoch,
		seq:                NextReportSeq(),
		sessionRef:         sessionRef,
		sessionStartSource: sessionStartSource,
		resumeArgv:         resumeArgv,
	})
	return true
}

// sessionReportAllowedLocked reports whether a session report of the given
// generation may still be delivered: the generation must be the current one
// and the manager must be inside a session (generation 0 means that the
// session lifecycle never ran, which keeps the legacy behaviour).
// It must be called with sm.mu held.
func (sm *StateManager) sessionReportAllowedLocked(epoch uint64) bool {
	if sm.sessionEpoch != epoch {
		return false
	}
	return sm.rootSession || sm.sessionEpoch == 0
}

// prepareSessionReportForEpoch hands the pane.report_agent_session request of
// the given generation to the sending lifecycle. The request is never sent by
// an untracked goroutine: it is queued behind the reports that were queued
// before it.
func (sm *StateManager) prepareSessionReportForEpoch(sessionStartSource *string, epoch uint64) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.queueSessionReportLocked(sessionStartSource, epoch)
}

// enqueueReleaseForEpoch hands the pane.release_agent request to the sending
// lifecycle and returns a channel that is closed once the release has been
// sent (or dropped).
//
// The release is queued behind every request that was queued before it, so
// Herdr receives it strictly after the last in-flight session/state report of
// this session: no report can land after the release and re-lock the pane.
// The release is dropped when a new session has taken over the manager by the
// time it is dispatched, so an outdated release cannot wipe the pane hold of
// the new session.
func (sm *StateManager) enqueueReleaseForEpoch(epoch uint64) <-chan struct{} {
	item := &outboxItem{kind: outboxRelease, epoch: epoch, done: make(chan struct{})}
	sm.mu.Lock()
	item.seq = NextReportSeq()
	sm.enqueueOutboxLocked(item)
	sm.mu.Unlock()
	return item.done
}

// issueActivityTicketLocked hands out the ticket of an agent activity event.
// It must be called with sm.mu held.
func (sm *StateManager) issueActivityTicketLocked() uint64 {
	sm.activityTickets++
	return sm.activityTickets
}

// applyAgentActivity applies an agent activity decision (the agentActive flag)
// for the given generation and publishes the resulting state.
//
// The decision is dropped when a newer activity event has already been
// applied: the handlers read the provider outside the lock, so their
// observation can be outdated by the time it is applied, and an outdated idle
// observation must not overwrite the newer "working" state.
func (sm *StateManager) applyAgentActivity(epoch, ticket uint64, active bool, force bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.rootSession || sm.sessionEpoch != epoch {
		return
	}
	if ticket < sm.activityApplied {
		return
	}
	sm.activityApplied = ticket
	sm.agentActive = active
	sm.publishStateLocked(force, epoch)
}

// PrepareReportSession extracts sessionRef and allocates seq synchronously.
// Matches TS lines 114-132: nextReportSeq() is invoked synchronously during request preparation.
// The sequence number is allocated under the lock, so it belongs to the request
// that is built from this snapshot.
// The resume command is attached only once this source holds the pane; Herdr
// answers "resume_not_accepted" otherwise.
func (sm *StateManager) PrepareReportSession(sessionStartSource *string) (Request, bool) {
	sm.mu.Lock()
	if !sm.rootSession && sm.sessionEpoch > 0 {
		sm.mu.Unlock()
		return Request{}, false
	}
	sessionRef := sm.getSessionRefLocked()
	holds := sm.agentStateReported
	var seq uint64
	if sessionRef.Path != nil || sessionRef.ID != nil {
		seq = NextReportSeq()
	}
	sm.mu.Unlock()

	var resumeArgv []string
	if holds {
		resumeArgv = ResumeCommand(sessionRef)
	}

	return sm.client.BuildReportAgentSessionRequestWithSeq(sessionRef, sessionStartSource, resumeArgv, seq)
}

// ReportSession queues pane.report_agent_session in the sending lifecycle,
// matching TS lines 114-132. The request keeps its place in the order of the
// other reports of this source.
func (sm *StateManager) ReportSession(sessionStartSource *string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.queueSessionReportLocked(sessionStartSource, sm.sessionEpoch)
}

// HandleHerdrBlocked handles "herdr:blocked" event bus message.
// Matches TS lines 211-227.
// The blocked counter and the state publication happen under the same lock
// acquisition, so a concurrent session shutdown cannot slip in between them
// and leave a blocked count behind for a session that no longer exists.
func (sm *StateManager) HandleHerdrBlocked(_ sdk.Context, data any) error {
	m, _ := data.(map[string]any)
	active, _ := m["active"].(bool)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !sm.rootSession {
		return nil
	}

	if !active {
		if sm.blockedCount > 0 {
			sm.blockedCount--
		}
		if sm.blockedCount == 0 {
			sm.blockedMessage = ""
		}
		sm.publishStateLocked(false, sm.sessionEpoch)
		return nil
	}

	sm.blockedCount++
	if label, ok := m["label"].(string); ok {
		sm.blockedMessage = label
	} else {
		sm.blockedMessage = ""
	}
	sm.publishStateLocked(false, sm.sessionEpoch)
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

	// A new session opens a new generation and starts from a clean slate: every
	// handler that captured an older generation is stale from now on and may
	// not touch the state machine any more.
	sm.mu.Lock()
	sm.sessionEpoch++
	epoch := sm.sessionEpoch
	ticket := sm.issueActivityTicketLocked()
	sm.rootSession = true
	sm.queuedState = nil
	sm.agentStateReported = false
	sm.lastState = nil
	sm.lastMessage = nil
	sm.agentActive = false
	sm.blockedCount = 0
	sm.blockedMessage = ""
	sm.mu.Unlock()

	// The provider is queried outside the lock; the generation is re-checked
	// under the lock before the result is stored.
	if !sm.updateSessionRefCoreForEpoch(provider, epoch) {
		return
	}

	// The session report is handed to the sending lifecycle before the state
	// is published, so it is sequenced (and delivered) before the state report
	// of this session. This guarantees report_agent_session.seq <
	// report_agent.seq without blocking host's session_start handler.
	sm.prepareSessionReportForEpoch(sessionStartSource, epoch)

	// In TS: agentActive = ctx?.isIdle?.() === false
	// The idle state was read outside the lock: the decision is applied only
	// when no newer activity event has been applied in the meantime.
	isIdle, err := provider.IsIdle()
	sm.applyAgentActivity(epoch, ticket, err == nil && !isIdle, true)
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
	epoch := sm.sessionEpoch
	ticket := sm.issueActivityTicketLocked()
	sm.mu.Unlock()

	if !sm.updateSessionRefCoreForEpoch(provider, epoch) {
		return
	}

	// The session report is handed to the sending lifecycle before the state
	// is published, so it is sequenced (and delivered) before the state
	// report, matching the synchronous execution of the TS port. This
	// guarantees report_agent_session.seq < report_agent.seq.
	sm.prepareSessionReportForEpoch(nil, epoch)

	sm.applyAgentActivity(epoch, ticket, true, false)
}

// HandleAgentSettled handles agent_settled event.
// Matches TS lines 253-260.
func (sm *StateManager) HandleAgentSettled(ctx sdk.Context, _ map[string]any) (any, error) {
	sm.handleAgentSettledCore(realSessionContext{ctx: ctx})
	return nil, nil
}

// HandleSessionShutdown handles session_shutdown and releases the pane.
// The official contract requires a release on exit so Herdr clears the agent
// name, state and resume command immediately instead of waiting for the pane
// to fall back to its idle shell prompt.
// It serializes with the whole sending lifecycle: the state machine is reset
// first, then the handler waits (outside the lock) for the in-flight drain
// iteration to finish and queues the pane.release_agent behind every request
// that is still in flight. That guarantees the release is ordered strictly
// after every report_agent and report_agent_session this source sent and
// that no in-flight report can land at Herdr after the release, which would
// re-lock the pane.
// The shutdown closes the current generation, so handlers that are still in
// flight (e.g. waiting for provider.IsIdle()) can no longer publish a state
// after the release, and a release that was overtaken by a new session is
// dropped instead of wiping the pane hold of the new session.
func (sm *StateManager) HandleSessionShutdown(_ sdk.Context, _ map[string]any) (any, error) {
	sm.mu.Lock()
	if !sm.rootSession {
		sm.mu.Unlock()
		return nil, nil
	}

	// The shutdown closes the current generation and resets the state machine:
	// no stale report may be queued or sent after the release, and a later
	// session starts from a clean slate.
	sm.sessionEpoch++
	shutdownEpoch := sm.sessionEpoch

	sm.rootSession = false
	sm.queuedState = nil
	sm.agentStateReported = false
	sm.lastState = nil
	sm.lastMessage = nil
	sm.agentActive = false
	sm.blockedCount = 0
	sm.blockedMessage = ""

	// If a drainLoop iteration is in flight, wait for it to end (the in-flight
	// report must complete or fail first) before the release is queued.
	var done chan struct{}
	if sm.sendInFlight {
		done = sm.inFlightDone
	}
	sm.mu.Unlock()

	if done != nil {
		<-done
	}

	// The release is queued behind every request that is still in flight, so
	// Herdr receives it strictly after the last session/state report of this
	// session: no report can land after the release and re-lock the pane.
	// A new session that took over this manager in the meantime drops the
	// outdated release instead of letting it wipe the pane hold of the new
	// session.
	released := sm.enqueueReleaseForEpoch(shutdownEpoch)
	<-released
	return nil, nil
}

func (sm *StateManager) handleAgentSettledCore(provider SessionInfoProvider) {
	sm.mu.Lock()
	if !sm.rootSession {
		sm.mu.Unlock()
		return
	}
	epoch := sm.sessionEpoch
	ticket := sm.issueActivityTicketLocked()
	sm.mu.Unlock()

	isIdle, err := provider.IsIdle()
	if err != nil || !isIdle {
		return
	}

	// The idle state was read outside the lock, so the observation may be
	// outdated by now: it is applied only when no newer activity event has
	// been applied in the meantime. An outdated idle observation can thus
	// never overwrite the newer "working" state of a following agent_start.
	sm.applyAgentActivity(epoch, ticket, false, false)
}
