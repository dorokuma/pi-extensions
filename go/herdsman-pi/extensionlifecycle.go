package herdsmanpi

// Connection-state application, role/scope state machine and lifecycle hooks:
// the second half of index.ts.

import (
	"sort"
	"strings"
)

// applyOptions mirrors the TS optional argument of
// `applyConnectionStateResponse`.
type applyOptions struct {
	NotifyReconnectLoss bool
}

// applyConnectionStateResponse mirrors the TS helper.
func (e *HerdsmanExtension) applyConnectionStateResponse(
	response *ConnectionStateResponse,
	ctx Context,
	options applyOptions,
) {
	reconnectingOwner := options.NotifyReconnectLoss && e.reconnectingFromOn
	scopeChanged := e.currentScope != nil &&
		(e.currentScope.HerdrSessionName != response.Presence.HerdrSessionName ||
			e.currentScope.WorkspaceID != response.Presence.WorkspaceID)
	if scopeChanged {
		e.resetForScopeChange(ctx)
	}
	e.currentScope = &CurrentScope{
		HerdrSessionName: response.Presence.HerdrSessionName,
		PaneID:           response.Presence.PaneID,
		TerminalID:       response.Presence.TerminalID,
		WorkspaceID:      response.Presence.WorkspaceID,
	}
	if !isLocalOwner(response) {
		e.loseRole(ctx, loseRoleOptions{})
		if reconnectingOwner && ctx != nil {
			message := "Herdsman is off"
			if response.State != nil && response.State.Owner != nil {
				message = "Herdsman is off · moved to " + response.State.Owner.PaneID
			}
			ctx.UI().Notify(message, NotifyInfo)
		}
		return
	}
	if !e.isOrchestrator {
		// The role changed hands: guards captured while another terminal owned
		// the workspace describe a state that no longer exists.
		e.scopeEpoch++
	}
	e.isOrchestrator = true
	e.reconnectingFromOn = false
	if e.deliveredBatch != nil {
		idle, known := false, false
		if ctx != nil {
			idle, known = ctx.IsIdle()
		}
		if e.deliveredBatch.Invalidated || !known || idle {
			// A batch invalidated by a genuine role/scope reset, or one whose wake
			// turn is no longer running, can no longer be acknowledged by a future
			// settlement.
			e.deliveredBatch = nil
		}
	}
	// `applyOwnerContext`: the latest context is only ours while we own the
	// workspace.
	if isLocalOwner(response) && response.Context != nil {
		copied := *response.Context
		e.latestContext = &copied
	} else {
		e.latestContext = nil
	}
	e.setHerdsmanUI(ctx)
	// The watermark first: a barrier the server has already settled is reaped
	// inside the sync instead of being kept as a barrier by it.
	ackedEventID := ackedEventIDOf(response)
	e.pruneAcknowledgedEvents(ackedEventID)
	serverAcked := 0
	if ackedEventID != nil {
		serverAcked = *ackedEventID
	}
	e.syncPendingEventsFromServer(response.Events, ctx, serverAcked)
	if len(e.pendingEvents) == 0 && e.wakeTimer != nil {
		e.cancelWakeTimer()
	}
	e.setHerdsmanUI(ctx)
	e.scheduleWakeLocked(ctx)
}

// loseRoleOptions mirrors the TS options object.
type loseRoleOptions struct {
	Abort             bool
	PreservePresented bool
}

// loseRole mirrors the TS helper.
func (e *HerdsmanExtension) loseRole(ctx Context, options loseRoleOptions) {
	// A role transition invalidates every guard captured under the previous
	// role, even when the connection and the scope survive it.
	e.scopeEpoch++
	if e.deliveredBatch != nil && !options.PreservePresented {
		e.deliveredBatch.Invalidated = true
		abortedBatch := e.deliveredBatch
		if options.Abort {
			for _, event := range abortedBatch.Events {
				if event.ID > e.failedWakeThroughID {
					e.failedWakeThroughID = event.ID
				}
			}
			// Owner lost mid-turn: drop the in-flight batch so it cannot gate the
			// next wake.
			e.deliveredBatch = nil
			e.wakeDeferredUntilSettled = false
		}
		if options.Abort && abortedBatch.HerdsmanTriggered && !abortedBatch.HasSubstantiveWork && ctx != nil {
			ctx.Abort()
		}
	}
	if e.wakeRequestedThroughID > 0 && e.wakeRequestedThroughID > e.failedWakeThroughID {
		e.failedWakeThroughID = e.wakeRequestedThroughID
	}
	e.cancelWake()
	e.clearAgentContext()
	e.isOrchestrator = false
	e.pendingEvents = []AgentEventWireRecord{}
	if !options.PreservePresented {
		e.clearDeliveryBookkeeping()
	}
	e.reconnectingFromOn = false
	e.setHerdsmanUI(ctx)
}

// markDisconnected mirrors the TS helper.
func (e *HerdsmanExtension) markDisconnected(ctx Context) {
	// The connection is gone: guards captured on it are dead even though the
	// client object may be reused by the reconnect that follows. A registration
	// left in flight belongs to the dead connection too, so the next connect can
	// start a fresh one instead of waiting for a marker that nothing will clear.
	e.connectionEpoch++
	e.registrationInFlight = nil
	reconnectingFromOn := e.reconnectingFromOn || e.isOrchestrator
	// Reconnects are not scope resets: keep the presentation guard and the
	// in-flight batch.
	e.loseRole(ctx, loseRoleOptions{Abort: false, PreservePresented: true})
	e.reconnectingFromOn = reconnectingFromOn
	e.setHerdsmanUI(ctx)
}

// resetForScopeChange mirrors the TS helper.
func (e *HerdsmanExtension) resetForScopeChange(ctx Context) {
	// A scope reset invalidates every guard captured under the previous scope.
	e.scopeEpoch++
	e.clearAgentContext()
	if e.deliveredBatch != nil && e.deliveredBatch.HerdsmanTriggered && !e.deliveredBatch.HasSubstantiveWork && ctx != nil {
		ctx.Abort()
	}
	if e.deliveredBatch != nil {
		e.deliveredBatch.Invalidated = true
	}
	e.deliveredBatch = nil
	e.cancelWake()
	e.failedWakeThroughID = 0
	e.pendingEvents = []AgentEventWireRecord{}
	e.clearDeliveryBookkeeping()
	e.completedPanes = map[CompletedPaneKey]int{}
	e.syncCompletedPaneKeys()
	e.setHerdsmanUI(ctx)
}

// handleAgentEvent mirrors the TS helper.
func (e *HerdsmanExtension) handleAgentEvent(event AgentEventWireRecord, ctx Context) {
	if !e.isOrchestrator || e.currentScope == nil || event.TerminalID == nil || *event.TerminalID == "" {
		return
	}
	if *event.TerminalID == e.currentScope.TerminalID {
		return
	}
	e.addPendingEvents([]AgentEventWireRecord{event}, ctx)
	if e.pi != nil {
		_ = e.pi.AppendEntry(agentEventEntryType, event)
	}
	e.scheduleWakeLocked(ctx)
}

// refreshAfterRoleGain mirrors the TS helper.
//
// The caller must NOT hold e.mu: the re-read is a blocking request and the TS
// awaits it in an async handler, i.e. with the role change already applied and
// no lock held. `handleStreamMessage` hands over here once it unlocked.
func (e *HerdsmanExtension) refreshAfterRoleGain(ctx Context) {
	e.mu.Lock()
	if e.closed || e.client == nil || !e.connected || e.currentScope == nil {
		e.mu.Unlock()
		return
	}
	client := e.client
	guard := e.rpcGuardLocked()
	e.mu.Unlock()

	raw, err := client.Request("agent.orchestrator.get", map[string]any{})

	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		// Reconnect handling owns transport failures.
		return
	}
	if !e.rpcGuardValid(guard) {
		// A reconnect, a scope reset or another role change took over while the
		// page was in flight: that path owns the state now.
		return
	}
	if response := decodeConnectionStateResponse(raw); response != nil {
		e.applyConnectionStateResponse(response, ctx, applyOptions{})
	}
}

// handleRoleChange mirrors the TS helper. The bool reports that this Pi just
// took the workspace over and its state must be re-read from the daemon; the
// caller does that with e.mu released (see `handleStreamMessage`).
func (e *HerdsmanExtension) handleRoleChange(change *AgentOrchestratorChanged, ctx Context) bool {
	if change == nil || e.currentScope == nil {
		return false
	}
	terminalID := e.currentScope.TerminalID
	if terminalID == "" {
		return false
	}
	wasOwner := change.Previous.Owner != nil && change.Previous.Owner.TerminalID == terminalID
	isOwner := change.Current.Owner != nil && change.Current.Owner.TerminalID == terminalID
	if isOwner && change.Current.Owner != nil {
		scopeChanged := e.currentScope.HerdrSessionName != change.Current.HerdrSessionName ||
			e.currentScope.WorkspaceID != change.Current.WorkspaceID
		if scopeChanged {
			e.resetForScopeChange(ctx)
		}
		e.currentScope = &CurrentScope{
			HerdrSessionName: change.Current.HerdrSessionName,
			PaneID:           change.Current.Owner.PaneID,
			TerminalID:       terminalID,
			WorkspaceID:      change.Current.WorkspaceID,
		}
		gainedRole := !e.isOrchestrator
		if gainedRole {
			// Guards captured before the takeover were issued under the other
			// owner's state.
			e.scopeEpoch++
		}
		e.isOrchestrator = true
		e.reconnectingFromOn = false
		e.setHerdsmanUI(ctx)
		return gainedRole || scopeChanged
	}
	if !wasOwner {
		return false
	}
	paneID := "unknown"
	if e.currentScope != nil && e.currentScope.PaneID != "" {
		paneID = e.currentScope.PaneID
	} else if change.Previous.Owner != nil && change.Previous.Owner.PaneID != "" {
		paneID = change.Previous.Owner.PaneID
	}
	e.currentScope = &CurrentScope{
		HerdrSessionName: change.Current.HerdrSessionName,
		PaneID:           paneID,
		TerminalID:       terminalID,
		WorkspaceID:      change.Current.WorkspaceID,
	}
	e.loseRole(ctx, loseRoleOptions{Abort: true})
	if !e.roleMutationInFlight {
		message := "Herdsman is off"
		if change.Current.Owner != nil {
			message = "Herdsman is off · moved to " + change.Current.Owner.PaneID
		}
		if ctx != nil {
			ctx.UI().Notify(message, NotifyInfo)
		}
	}
	return false
}

// handleStreamMessage mirrors the TS helper.
func (e *HerdsmanExtension) handleStreamMessage(message DaemonStreamMessage) {
	var refresh bool
	e.mu.Lock()
	ctx := e.activeContext
	switch message.Method {
	case "agent.event":
		if message.Event != nil {
			e.handleAgentEvent(*message.Event, ctx)
		}
		e.mu.Unlock()
		return
	case "agent.context.changed":
		if e.isOrchestrator && e.currentScope != nil &&
			e.currentScope.HerdrSessionName == message.HerdrSession &&
			e.currentScope.WorkspaceID == message.WorkspaceID {
			if message.Context != nil {
				copied := *message.Context
				e.latestContext = &copied
			} else {
				e.latestContext = nil
			}
		}
		e.mu.Unlock()
		return
	}
	refresh = e.handleRoleChange(message.Change, ctx)
	e.mu.Unlock()
	// The re-read runs after the unlock: the daemon round trip must never park
	// the core mutex, or the notification path that answers on that same socket
	// cannot make progress.
	if refresh {
		e.refreshAfterRoleGain(ctx)
	}
}

// registerPresence mirrors the TS helper.
func (e *HerdsmanExtension) registerPresence(ctx Context) {
	if e.registrationInFlight != nil {
		return
	}
	client := e.client
	launchIdentity := e.launchIdentity
	subscriberID := e.subscriberID
	sessionRef := e.sessionRef
	if client == nil || launchIdentity == nil || subscriberID == nil {
		return
	}
	if sessionRef == nil || sessionRef.Value == "" {
		logWarn("[herdsman-pi] Pi session file is unavailable for Herdsman presence")
		return
	}
	done := make(chan struct{})
	e.registrationInFlight = done
	paneID := launchIdentity.PaneID
	workspaceID := launchIdentity.WorkspaceID
	if e.currentScope != nil {
		if e.currentScope.PaneID != "" {
			paneID = e.currentScope.PaneID
		}
		if e.currentScope.WorkspaceID != "" {
			workspaceID = e.currentScope.WorkspaceID
		}
	}
	params := map[string]any{
		"herdrSocketPath": launchIdentity.HerdrSocketPath,
		"paneId":          paneID,
		"sessionRef": map[string]any{
			"agent":  sessionRef.Agent,
			"kind":   sessionRef.Kind,
			"source": sessionRef.Source,
			"value":  sessionRef.Value,
		},
		"subscriberId":   *subscriberID,
		"subscriberKind": "pi",
		"workspaceId":    workspaceID,
	}
	// The registration is a blocking round trip on a goroutine: the identity it
	// runs under is fixed here, while the lock that created the marker is still
	// held. Capturing it later (inside finishRegistration, after it queued for
	// the lock) would let a registration that was started for a dead connection
	// borrow the epoch and subscriber of the live one.
	regEpoch := e.connectionEpoch
	regSub := *subscriberID
	go func() {
		defer close(done)
		e.finishRegistration(client, ctx, params, done, regEpoch, regSub)
	}()
}

// finishRegistration is the `.then/.catch/.finally` chain of `registerPresence`.
//
// It runs on the registration goroutine and releases e.mu around both daemon
// requests: presence registration blocks for as long as the daemon takes, and
// the stream dispatcher and the host handlers need the lock while it does.
// `registrationInFlight` is cleared on every path — including the early exits
// before the request — by the top-level defer; a leaked in-flight marker here
// is what made the extension stop registering after one bad response.
//
// `regEpoch` and `regSub` are the connection epoch and subscriber the
// registration was created for; every checkpoint below re-validates them, so a
// registration whose cycle was replaced while it waited neither issues a stale
// request nor applies its response to the live state.
func (e *HerdsmanExtension) finishRegistration(client HerdsmanDaemonClient, ctx Context, params map[string]any, done chan struct{}, regEpoch int, regSub string) {
	// `.finally`: whatever happens below, a later connect must be able to run a
	// fresh registration. The marker is only cleared when it is still this
	// registration's own, so a newer registration's marker is never dropped.
	defer func() {
		e.mu.Lock()
		if e.registrationInFlight == done {
			e.registrationInFlight = nil
		}
		e.mu.Unlock()
	}()
	e.mu.Lock()
	if !e.registrationCurrentLocked(client, done, regEpoch, regSub) {
		// A session shutdown, a reconnect, a newer registration or a replaced
		// subscriber superseded this registration while it waited for the lock:
		// its params describe a cycle that is already gone.
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	raw, err := client.Request("agent.orchestrator.register", params)

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.registrationCurrentLocked(client, done, regEpoch, regSub) {
		// A session shutdown, a reconnect or a newer client superseded this
		// registration, so its response no longer describes the live connection.
		return
	}
	if err != nil {
		e.stopKeepalive()
		e.connected = false
		message := ""
		if strings.Contains(strings.ToLower(err.Error()), "incompatible") {
			message = err.Error()
		}
		if message != "" && ctx != nil {
			ctx.UI().Notify(message, NotifyError)
		}
		e.markDisconnected(ctx)
		return
	}
	e.connected = true
	e.startKeepalive(client)
	response := decodeConnectionStateResponse(raw)
	currentPaneID := e.launchIdentity.PaneID
	if e.currentScope != nil && e.currentScope.PaneID != "" {
		currentPaneID = e.currentScope.PaneID
	}
	if e.envValue("HERDSMAN_AUTO_ON") == "1" && !e.autoClaimAttempted &&
		isMainOrchestratorPane(currentPaneID) &&
		!isDispatchedRoleSession(e.sessionRef.Value, e.env) {
		e.autoClaimAttempted = true
		if !isLocalOwner(response) && (response == nil || response.State == nil || response.State.Owner == nil) {
			if !e.registrationCurrentLocked(client, done, regEpoch, regSub) {
				return
			}
			e.roleMutationInFlight = true
			e.mu.Unlock()
			claimed, claimErr := client.Request("agent.orchestrator.set", map[string]any{"enabled": true})
			e.mu.Lock()
			e.roleMutationInFlight = false
			if !e.registrationCurrentLocked(client, done, regEpoch, regSub) {
				return
			}
			if claimErr == nil {
				if decoded := decodeConnectionStateResponse(claimed); decoded != nil {
					response = decoded
				}
			}
		}
	}
	if response != nil {
		e.applyConnectionStateResponse(response, ctx, applyOptions{NotifyReconnectLoss: true})
	}
}

// handleCommand mirrors the `/herdsman` handler.
//
// The caller must NOT hold e.mu: every daemon read/write below blocks, and the
// lock stays free for the notification path while it does.
func (e *HerdsmanExtension) handleCommand(args string, ctx Context) {
	value := strings.TrimSpace(args)
	action := value
	if action == "" {
		action = "status"
	}
	if action != "on" && action != "off" && action != "status" {
		ctx.UI().Notify(CommandUsage, NotifyWarning)
		return
	}

	e.mu.Lock()
	if e.launchIdentity == nil {
		e.mu.Unlock()
		ctx.UI().Notify(HerdrRequiredMsg, NotifyError)
		return
	}
	if e.closed || e.client == nil || !e.connected || e.currentScope == nil {
		e.mu.Unlock()
		ctx.UI().Notify(ReconnectingMessage, NotifyWarning)
		return
	}
	client := e.client
	guard := e.rpcGuardLocked()
	claim := action == "on"
	status := action == "status"
	if !status {
		e.roleMutationInFlight = true
	}
	e.mu.Unlock()

	var raw any
	var err error
	if status {
		raw, err = client.Request("agent.orchestrator.get", map[string]any{})
	} else {
		raw, err = client.Request("agent.orchestrator.set", map[string]any{"enabled": claim})
	}

	e.mu.Lock()
	if !status {
		e.roleMutationInFlight = false
	}
	if err != nil {
		e.mu.Unlock()
		ctx.UI().Notify(err.Error(), NotifyError)
		return
	}
	if !e.connectionStillLive(guard) {
		e.mu.Unlock()
		return
	}
	response := decodeConnectionStateResponse(raw)
	if response != nil {
		e.applyConnectionStateResponse(response, ctx, applyOptions{})
	}
	e.mu.Unlock()
	if response != nil {
		notifyLocalStatus(response, ctx)
	}
}

// CompleteCommand mirrors the TS `getArgumentCompletions` for `/herdsman`.
func CompleteCommand(prefix string) []map[string]string {
	items := make([]map[string]string, 0, 3)
	for _, value := range []string{"on", "off", "status"} {
		if strings.HasPrefix(value, prefix) {
			items = append(items, map[string]string{"label": value, "value": value})
		}
	}
	return items
}

// ── lifecycle ───────────────────────────────────────────────────────────────

// shouldSkipDaemonClient mirrors the TS `shouldSkipDaemonClient`: whether
// this `session_start` must not open a daemon connection at all.
//
// Two shapes qualify, and both are one-shot or session-less runs where a
// daemon client can only ever be a liability:
//   - no session file (`--no-session`): there is no path to register presence
//     against, and `agent.orchestrator.register` rejects, so the client would
//     spend the rest of the run in a reconnect loop;
//   - a non-interactive `print`/`json` run: the process is about to exit and
//     nothing will ever read the stream, so the connection outlives the run
//     and holds the event loop open.
//
// HasUI is only honoured as an explicit false (Pi's own signal for a
// non-interactive mode); an absent field means "unknown" and keeps the old
// behaviour. The mode is read from the event first and the context second
// because Pi reports it on both. The SDK pins "no reported mode" to "print",
// so a nil-mode context here is one the host never described — unknown, like
// the absent TS field, not non-interactive.
func shouldSkipDaemonClient(ctx Context, eventMode *string, sessionRef *AgentSessionRef) bool {
	if sessionRef == nil || sessionRef.Value == "" {
		return true
	}
	if hasUI, known := ctx.HasUI(); known && !hasUI {
		return true
	}
	mode := ""
	modeKnown := false
	if eventMode != nil && *eventMode != "" {
		mode = *eventMode
		modeKnown = true
	}
	if !modeKnown {
		mode, modeKnown = ctx.Mode()
	}
	return modeKnown && (mode == "print" || mode == "json")
}

// HandleSessionStart mirrors the `session_start` handler. `eventMode` is the
// mode the event itself reported (the SDK hands the whole `data` map); nil
// means the event did not carry one and the context is asked instead.
func (e *HerdsmanExtension) HandleSessionStart(ctx Context, eventMode *string) {
	e.mu.Lock()
	// A new session is a new connection and a new scope identity: every guard
	// captured before it is stale, and a registration left in flight belongs to
	// the session that is being replaced.
	e.connectionEpoch++
	e.scopeEpoch++
	e.registrationInFlight = nil
	e.activeContext = ctx
	e.autoClaimAttempted = false
	sessionID := ctx.SessionID()
	e.subscriberID = &sessionID
	e.sessionRef = &AgentSessionRef{
		Agent:  "pig",
		Kind:   "path",
		Source: "herdr:pig",
		Value:  ctx.SessionFile(),
	}
	e.launchIdentity = herdrLaunchIdentity(e.env)
	if e.launchIdentity == nil {
		e.connected = false
		e.loseRole(ctx, loseRoleOptions{})
		e.mu.Unlock()
		return
	}
	if shouldSkipDaemonClient(ctx, eventMode, e.sessionRef) {
		// Nothing to register against: an ephemeral `--no-session` run has no
		// session file to report, and a `print`/`json` run is a one-shot
		// non-interactive process whose only remaining work is to exit. Building
		// a client here is what kept `pi --print` hanging on `herdsman.sock`.
		e.stopKeepalive()
		if e.client != nil {
			e.client.Close()
		}
		e.client = nil
		e.connected = false
		e.loseRole(ctx, loseRoleOptions{})
		e.mu.Unlock()
		return
	}
	e.stopKeepalive()
	if e.client != nil {
		e.client.Close()
	}
	var client HerdsmanDaemonClient
	if e.options.ClientFactory != nil {
		client = e.options.ClientFactory()
	} else {
		client = NewReconnectingDaemonClient(DefaultSocketPath(), nil)
	}
	// The core points at this client before the callbacks go on: a client built
	// by the factory above may already be connected (its first dial runs on its
	// own goroutine), and the client then delivers the missed `onConnected`
	// notification as soon as the callback is installed -- on another goroutine,
	// where it must find this client as the live one.
	e.client = client
	client.SetOnConnected(func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.activeContext == nil || e.client != client {
			return nil
		}
		// A fresh connection is a fresh connection epoch, exactly like
		// markDisconnected, HandleSessionStart and HandleSessionShutdown
		// already do: every guard and every registration captured for the
		// previous cycle is stale from here on, so a registration goroutine
		// that queued for this lock before the callback must not borrow this
		// cycle's identity.
		e.connectionEpoch++
		e.registerPresence(e.activeContext)
		return nil
	})
	client.SetOnDisconnected(func(error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.client != client {
			return
		}
		e.stopKeepalive()
		e.connected = false
		e.markDisconnected(e.activeContext)
	})
	client.SetOnStreamMessage(e.handleStreamMessage)
	e.mu.Unlock()

	// Handlers go on before ResetForSession: the TS assigns them after
	// `resetForSession?.()`, but there the connection attempt is a microtask and
	// the socket's "connect" event can only fire later. The Go client can dial
	// synchronously inside ResetForSession, so registering first keeps the TS
	// guarantee that the very first connection triggers the registration.
	//
	// The dial runs with e.mu released: both the connect and the disconnect
	// branch of `connect` invoke this extension's own callbacks inline on the
	// calling goroutine, and those callbacks take e.mu. Go mutexes are not
	// reentrant, so holding it here self-deadlocks the session start -- which is
	// exactly how the extension hung when the host loaded it.
	client.ResetForSession()
}

// HandleSessionShutdown mirrors the `session_shutdown` handler.
func (e *HerdsmanExtension) HandleSessionShutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	// The session is over: the connection and the scope identity die with it.
	e.connectionEpoch++
	e.scopeEpoch++
	e.registrationInFlight = nil
	e.stopKeepalive()
	e.connected = false
	e.loseRole(e.activeContext, loseRoleOptions{})
	e.deliveredBatch = nil
	e.presentedEventIDs = map[int]bool{}
	e.unackedDelivered = map[int]AgentEventWireRecord{}
	if e.client != nil {
		e.client.Close()
	}
	e.client = nil
	e.activeContext = nil
	e.closed = true
}

// signalTurnCompletion mirrors the TS helper.
func (e *HerdsmanExtension) signalTurnCompletion(expectedText string) {
	client := e.client
	scope := e.currentScope
	var sessionPath string
	if e.sessionRef != nil {
		sessionPath = e.sessionRef.Value
	}
	if client == nil || !e.connected || scope == nil || sessionPath == "" {
		return
	}
	probe := e.options.ConfirmSessionWrite
	if probe == nil {
		probe = func(text, path string) TurnCompletionCheck {
			return ConfirmSessionWrite(ConfirmSessionWriteInput{ExpectedText: text, Path: path})
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		check := probe(expectedText, sessionPath)
		params := map[string]any{
			"confirmed":        check.Confirmed,
			"herdrSessionName": scope.HerdrSessionName,
			"paneId":           scope.PaneID,
			"terminalId":       scope.TerminalID,
			"workspaceId":      scope.WorkspaceID,
		}
		if expectedText != "" {
			params["expectedText"] = expectedText
		}
		if _, err := client.Request("agent.turn.completed", params); err != nil {
			logWarn("[herdsman-pi] turn completion signal failed reason=" + string(check.Reason) + " error=" + err.Error())
		}
	}()
	if e.options.OnTurnCompletionSignal != nil {
		e.options.OnTurnCompletionSignal(finished)
	}
}

// HandleMessageEnd mirrors the `message_end` handler.
func (e *HerdsmanExtension) HandleMessageEnd(event map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	message := recordOf(event["message"])
	if messageRole(message) == "custom" {
		if customType, _ := message["customType"].(string); customType == wakeContextCustomType {
			e.observeWakeConsumption(message)
		}
	}
	if messageRole(message) != "assistant" {
		return
	}
	stopReason := textOrEmpty(stringValue(message["stopReason"]))
	if e.deliveredBatch != nil {
		e.deliveredBatch.AbortedByUser = stopReason == "aborted"
		e.deliveredBatch.AssistantFinalSucceeded = stopReason == "stop" || stopReason == "length"
		if contentIsPresent(message["content"]) {
			e.deliveredBatch.HasSubstantiveWork = true
		}
	}
	if stopReason == "stop" || stopReason == "length" {
		e.signalTurnCompletion(assistantMessageText(message))
	}
}

// observeWakeConsumption is the consumption-evidence branch of `message_end`.
func (e *HerdsmanExtension) observeWakeConsumption(message map[string]any) {
	consumedEventIDs := wakeConsumedEventIds(message)
	if len(consumedEventIDs) == 0 {
		awaiting := sortedSetKeys(e.wakeAwaitingConsumption)
		awaitingText := "none"
		if len(awaiting) > 0 {
			awaitingText = joinInts(awaiting)
		}
		detailKeys := make([]string, 0, len(recordOf(message["details"])))
		for key := range recordOf(message["details"]) {
			detailKeys = append(detailKeys, key)
		}
		sort.Strings(detailKeys)
		keysText := "none"
		if len(detailKeys) > 0 {
			keysText = strings.Join(detailKeys, ",")
		}
		customType, _ := message["customType"].(string)
		logWarn("[herdsman-pi] wake consumption evidence unusable customType=" + customType +
			" awaiting=" + awaitingText + " detailsKeys=" + keysText + " · no event confirmed by this message")
	}
	for _, eventID := range consumedEventIDs {
		delete(e.wakeAwaitingConsumption, eventID)
		delete(e.wakeRetryableEventIDs, eventID)
		e.presentedEventIDs[eventID] = true
		e.wakeSuppressedEventIDs[eventID] = true
		if _, queued := e.unackedDelivered[eventID]; queued {
			e.wakeConsumptionObserved[eventID] = true
		}
	}
}

// HandleAgentStart mirrors the `agent_start` handler.
func (e *HerdsmanExtension) HandleAgentStart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Deliberately no clearing here: only consumption evidence writes a delivery
	// off.
	if e.runActive {
		return
	}
	e.runActive = true
}

// HandleSubstantiveToolUse mirrors `tool_execution_start` and `tool_result`.
func (e *HerdsmanExtension) HandleSubstantiveToolUse() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deliveredBatch != nil {
		e.deliveredBatch.HasSubstantiveWork = true
	}
}

// HandleContext mirrors the `context` handler: it injects nothing and only
// drops herdsman's own context/wake entries from the incoming list.
func (e *HerdsmanExtension) HandleContext(messages []any) []any {
	kept := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if ok && isNormalHerdsmanContext(message) {
			continue
		}
		kept = append(kept, raw)
	}
	return kept
}

// HandleAgentSettled mirrors the `agent_settled` handler.
//
// The acknowledgement loop blocks on the daemon, so it runs with e.mu released
// (`runAck` below); every state transition around it still happens under the
// lock and in the TS order.
func (e *HerdsmanExtension) HandleAgentSettled(ctx Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runActive = false
	e.wakeBusySpinRung = 0
	// The settlement is what a deferral waited for: drop the pending poll so the
	// wake decision is taken now instead of one tick later.
	if e.wakeTimer != nil && !e.wakeRequested {
		e.wakeTimer.Stop()
		e.wakeTimer = nil
	}
	unconsumed := sortedSetKeys(e.wakeAwaitingConsumption)
	if len(unconsumed) > 0 {
		e.writeOffStrandedWakeDelivery(unconsumed, ctx)
	}
	failBatch := func() {
		ctx.UI().Notify("Herdsman couldn’t acknowledge agent updates · updates remain pending", NotifyWarning)
	}
	finishBatch := func() {
		e.ackInFlight = false
		e.wakeDeferredUntilSettled = false
		e.setHerdsmanUI(ctx)
		e.scheduleWakeLocked(ctx)
	}
	// runAck hands the acknowledgement loop over without the lock and restores
	// it before the caller touches the state again. The batch's guard is
	// captured under the same lock that collected the events, so every id of the
	// batch is confirmed under one identity.
	runAck := func(events []AgentEventWireRecord, batchGuard rpcGuard) {
		e.mu.Unlock()
		e.acknowledgeEventIDs(events, true, ctx, batchGuard)
		e.mu.Lock()
	}

	batch := e.deliveredBatch
	if batch == nil {
		e.wakeDeferredUntilSettled = false
		// A queue can outlive its batch: with no batch left, no turn can vouch for
		// an untracked delivery, so only consumption evidence confirms an id.
		stranded := e.confirmableDeliveryPrefix(false)
		if len(stranded.ConfirmablePrefix) > 0 && e.isOrchestrator && e.client != nil && e.connected {
			resumeAckInFlight := e.ackInFlight
			e.ackInFlight = true
			batchGuard := e.rpcGuardLocked()
			runAck(stranded.ConfirmablePrefix, batchGuard)
			e.ackInFlight = resumeAckInFlight
		}
		e.scheduleWakeLocked(ctx)
		return
	}

	stillOwner := e.isOrchestrator && e.currentScope != nil && e.currentScope.TerminalID == batch.OwnerTerminalID
	prefix := e.confirmableDeliveryPrefix(batch.AssistantFinalSucceeded || batch.AbortedByUser)
	reachable := stillOwner && e.client != nil && e.connected
	batchGuard := e.rpcGuardLocked()
	// Every id awaiting consumption was dead-lettered above, so nothing holds the
	// cursor open any more and the batch can be dropped.
	e.deliveredBatch = nil
	e.ackInFlight = true
	if !reachable || !e.rpcGuardValid(batchGuard) {
		if len(prefix.DeliveryQueue) > 0 {
			failBatch()
		}
		finishBatch()
		return
	}
	if prefix.BlockedByMissingTurn {
		failBatch()
	}
	if len(prefix.ConfirmablePrefix) == 0 {
		finishBatch()
		return
	}
	runAck(prefix.ConfirmablePrefix, batchGuard)
	finishBatch()
}
