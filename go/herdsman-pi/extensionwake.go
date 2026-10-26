package herdsmanpi

// Wake injection and pending-queue synchronisation.
//
// Delivery is single-track: the wake content is handed to Pi exactly once, as a
// hidden follow-up message that triggers its own turn, and only when the
// orchestrator is idle. `agent_settled` is the primary close signal for a
// deferral; the backed-off poll below only keeps the decision alive while a turn
// runs (it is a poll-frequency reduction, not a deadline).

import (
	"sort"
	"time"
)

// addPendingEvents mirrors the TS helper.
func (e *HerdsmanExtension) addPendingEvents(events []AgentEventWireRecord, ctx Context) {
	byID := map[int]AgentEventWireRecord{}
	order := make([]int, 0, len(e.pendingEvents)+len(events))
	for _, event := range e.pendingEvents {
		if _, present := byID[event.ID]; !present {
			order = append(order, event.ID)
		}
		byID[event.ID] = event
	}
	addedNewEvent := false
	for _, event := range events {
		previous, present := byID[event.ID]
		// Only an event that survives the dead-letter barrier counts as new: the
		// server keeps re-listing dead-lettered events on every get, and treating
		// one as new would cancel the in-flight wake on every get.
		if !present && event.ID > e.failedWakeThroughID {
			addedNewEvent = true
		}
		if present {
			byID[event.ID] = mergeEvent(previous, event)
		} else {
			order = append(order, event.ID)
			byID[event.ID] = event
		}
	}
	e.pendingEvents = sortedFiltered(byID, e.failedWakeThroughID)
	e.setHerdsmanUI(ctx)
	if addedNewEvent && e.wakeTimer != nil {
		e.cancelWakeTimer()
	}
	e.scheduleWakeLocked(ctx)
}

// mergeEvent mirrors the TS `{ ...previous, ...event }`: server fields win, the
// local snapshot only fills keys the page omitted. The port reads "omitted" off
// the decoded record (a nil optional field), because the wire record cannot tell
// an absent key from an explicit null.
func mergeEvent(previous, event AgentEventWireRecord) AgentEventWireRecord {
	merged := event
	if merged.AgentID == nil {
		merged.AgentID = previous.AgentID
	}
	if merged.Attempts == nil {
		merged.Attempts = previous.Attempts
	}
	if merged.LastAttemptAt == nil {
		merged.LastAttemptAt = previous.LastAttemptAt
	}
	if merged.NextAttemptAt == nil {
		merged.NextAttemptAt = previous.NextAttemptAt
	}
	if merged.LastFailureCode == nil {
		merged.LastFailureCode = previous.LastFailureCode
	}
	if merged.CompactHistory == nil {
		merged.CompactHistory = previous.CompactHistory
	}
	if merged.CreatedAt == nil {
		merged.CreatedAt = previous.CreatedAt
	}
	if merged.HerdrSessionName == nil {
		merged.HerdrSessionName = previous.HerdrSessionName
	}
	if merged.PaneGeneration == nil {
		merged.PaneGeneration = previous.PaneGeneration
	}
	if merged.PaneID == nil {
		merged.PaneID = previous.PaneID
	}
	if merged.Payload == nil {
		merged.Payload = previous.Payload
	}
	if merged.Status == nil {
		merged.Status = previous.Status
	}
	if merged.TerminalID == nil {
		merged.TerminalID = previous.TerminalID
	}
	if merged.WorkspaceID == nil {
		merged.WorkspaceID = previous.WorkspaceID
	}
	if merged.Type == "" {
		merged.Type = previous.Type
	}
	return merged
}

// sortedFiltered mirrors the TS
// `[...byId.values()].filter((event) => event.id > barrier).sort(by id)`.
func sortedFiltered(byID map[int]AgentEventWireRecord, barrier int) []AgentEventWireRecord {
	result := make([]AgentEventWireRecord, 0, len(byID))
	for _, event := range byID {
		if event.ID > barrier {
			result = append(result, event)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result
}

// syncPendingEventsFromServer mirrors the TS helper.
func (e *HerdsmanExtension) syncPendingEventsFromServer(serverEvents []AgentEventWireRecord, ctx Context, serverAckedEventID int) {
	serverByID := map[int]AgentEventWireRecord{}
	for _, event := range serverEvents {
		serverByID[event.ID] = event
	}
	inFlightIDs := map[int]bool{}
	var inFlightEvents []AgentEventWireRecord
	if e.deliveredBatch != nil {
		inFlightEvents = e.deliveredBatch.Events
		for _, event := range inFlightEvents {
			inFlightIDs[event.ID] = true
		}
	}
	previousByID := map[int]AgentEventWireRecord{}
	previousOrder := make([]int, 0, len(e.pendingEvents))
	for _, event := range e.pendingEvents {
		if _, present := previousByID[event.ID]; !present {
			previousOrder = append(previousOrder, event.ID)
		}
		previousByID[event.ID] = event
	}
	// The daemon's pending page is hard-capped at 100, so a page of exactly 100
	// cannot tell "exhausted" from "more remain".
	isTruncated := len(serverEvents) >= 100
	maxServerEventID := 0
	for _, event := range serverEvents {
		if event.ID > maxServerEventID {
			maxServerEventID = event.ID
		}
	}

	addedNewEvent := false
	nextPending := make([]AgentEventWireRecord, 0, len(serverEvents)+len(previousOrder))

	for _, serverEvent := range serverEvents {
		previous, present := previousByID[serverEvent.ID]
		if !present && serverEvent.ID > e.failedWakeThroughID {
			addedNewEvent = true
		}
		if present {
			nextPending = append(nextPending, mergeEvent(previous, serverEvent))
		} else {
			nextPending = append(nextPending, serverEvent)
		}
	}

	for _, previousID := range previousOrder {
		if _, present := serverByID[previousID]; present {
			continue
		}
		previousEvent := previousByID[previousID]
		_, queued := e.unackedDelivered[previousID]
		barrier := queued && (e.wakeAwaitingConsumption[previousID] || e.wakeRetryableEventIDs[previousID])
		// Only the server cursor settles an absent barrier.
		settledByServer := previousID <= serverAckedEventID
		if barrier && !settledByServer {
			nextPending = append(nextPending, previousEvent)
			continue
		}
		if settledByServer {
			continue
		}
		if inFlightIDs[previousID] || (isTruncated && previousID > maxServerEventID) {
			nextPending = append(nextPending, previousEvent)
		}
	}

	for _, inFlightEvent := range inFlightEvents {
		if _, present := serverByID[inFlightEvent.ID]; present {
			continue
		}
		if _, present := previousByID[inFlightEvent.ID]; present {
			continue
		}
		nextPending = append(nextPending, inFlightEvent)
	}

	kept := make([]AgentEventWireRecord, 0, len(nextPending))
	for _, event := range nextPending {
		if event.ID > e.failedWakeThroughID {
			kept = append(kept, event)
		}
	}
	sort.Slice(kept, func(left, right int) bool { return kept[left].ID < kept[right].ID })
	e.pendingEvents = kept

	e.setHerdsmanUI(ctx)
	if len(e.pendingEvents) == 0 && e.wakeTimer != nil {
		e.cancelWakeTimer()
	} else if addedNewEvent && e.wakeTimer != nil {
		e.cancelWakeTimer()
	}
	e.scheduleWakeLocked(ctx)
}

// wakeOwnerLocked reports whether the wake planted for `generation` and the
// owner scope is still the one this callback owns: same generation, still the
// orchestrator, same scope, and the session is not shut down. The caller holds
// e.mu.
func (e *HerdsmanExtension) wakeOwnerLocked(generation int, herdrSessionName, terminalID, workspaceID string) bool {
	return !e.closed && generation == e.wakeGeneration && e.isOrchestrator && e.currentScope != nil &&
		e.currentScope.HerdrSessionName == herdrSessionName &&
		e.currentScope.TerminalID == terminalID &&
		e.currentScope.WorkspaceID == workspaceID
}

// scheduleSilentUpstreamErrorAck mirrors the TS helper.
func (e *HerdsmanExtension) scheduleSilentUpstreamErrorAck(ctx Context, events []AgentEventWireRecord) {
	if e.wakeTimer != nil || e.wakeRequested {
		return
	}
	scope := e.currentScope
	if !e.isOrchestrator || scope == nil {
		return
	}
	generation := e.wakeGeneration
	ownerHerdrSessionName := scope.HerdrSessionName
	ownerTerminalID := scope.TerminalID
	ownerWorkspaceID := scope.WorkspaceID
	pending := append([]AgentEventWireRecord{}, events...)
	e.wakeTimer = e.timers.SetTimeout(time.Duration(WakeSettleMS)*time.Millisecond, func() {
		e.runSilentUpstreamErrorAck(ctx, pending, generation, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID)
	})
}

// runSilentUpstreamErrorAck is the timer body of the silent acknowledgement.
// The acknowledgements themselves block on the daemon, so the batch runs with
// e.mu released and the lock is only retaken to settle the bookkeeping.
func (e *HerdsmanExtension) runSilentUpstreamErrorAck(
	ctx Context,
	pending []AgentEventWireRecord,
	generation int,
	ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID string,
) {
	e.mu.Lock()
	e.wakeTimer = nil
	if !e.wakeOwnerLocked(generation, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID) {
		e.mu.Unlock()
		return
	}
	if e.deliveredBatch != nil || e.ackInFlight {
		e.mu.Unlock()
		return
	}
	if e.client == nil || !e.connected {
		e.mu.Unlock()
		return
	}
	batchGuard := e.rpcGuardLocked()
	if !e.rpcGuardValid(batchGuard) {
		// The cycle that scheduled this silent acknowledgement is gone: no id
		// may be confirmed for it.
		e.mu.Unlock()
		return
	}
	e.ackInFlight = true
	e.setHerdsmanUI(ctx)
	e.mu.Unlock()

	e.acknowledgeEventIDs(pending, false, ctx, batchGuard)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.ackInFlight = false
	if e.closed || e.client != batchGuard.client {
		// A reconnect owns the state now: it re-reads the pending page and
		// schedules the next wake itself.
		return
	}
	e.setHerdsmanUI(ctx)
	e.scheduleWakeLocked(ctx)
}

// scheduleDeferredWake mirrors the TS helper.
func (e *HerdsmanExtension) scheduleDeferredWake(ctx Context) {
	e.wakeDeferredUntilSettled = true
	e.setHerdsmanUI(ctx)
	if e.wakeTimer != nil {
		return
	}
	rung := e.wakeBusySpinRung
	if rung > len(WakeBusyBackoffMS)-1 {
		rung = len(WakeBusyBackoffMS) - 1
	}
	if rung+1 > len(WakeBusyBackoffMS)-1 {
		e.wakeBusySpinRung = len(WakeBusyBackoffMS) - 1
	} else {
		e.wakeBusySpinRung = rung + 1
	}
	delay := WakeBusyBackoffMS[rung]
	if delay > WakeBusyBackoffCapMS {
		delay = WakeBusyBackoffCapMS
	}
	e.wakeTimer = e.timers.SetTimeout(time.Duration(delay)*time.Millisecond, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.wakeTimer = nil
		e.scheduleWakeLocked(ctx)
	})
}

// ScheduleWake is the exported entry of the TS `scheduleWake` (the host and the
// tests call it through the same path).
func (e *HerdsmanExtension) ScheduleWake(ctx Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scheduleWakeLocked(ctx)
}

// scheduleWakeLocked mirrors the TS `scheduleWake`; the caller holds e.mu.
func (e *HerdsmanExtension) scheduleWakeLocked(ctx Context) {
	if ctx == nil || !e.isOrchestrator || e.currentScope == nil || e.pi == nil {
		return
	}
	if e.wakeTimer != nil || e.wakeRequested {
		return
	}
	projection := e.projectPendingOutcomes(e.pendingEvents)
	candidates := make([]AgentOutcome, 0, len(projection.Outcomes))
	for _, outcome := range projection.Outcomes {
		if outcome.EventID <= e.failedWakeThroughID {
			continue
		}
		if e.alreadyPresented(outcome.EventID) {
			continue
		}
		candidates = append(candidates, outcome)
	}
	suppressedEvents := make([]AgentEventWireRecord, 0, len(projection.SuppressedEventIDs))
	for _, eventID := range projection.SuppressedEventIDs {
		if eventID <= e.failedWakeThroughID || e.presentedEventIDs[eventID] {
			continue
		}
		if event := e.findPendingByID(eventID); event != nil {
			suppressedEvents = append(suppressedEvents, *event)
		}
	}
	sort.Slice(suppressedEvents, func(left, right int) bool {
		return suppressedEvents[left].ID < suppressedEvents[right].ID
	})

	wakeable := make([]AgentOutcome, 0, len(candidates))
	for _, outcome := range candidates {
		if e.isWakeableEvent(e.findPendingByID(outcome.EventID)) {
			wakeable = append(wakeable, outcome)
		}
	}
	_, injectableOutcomes := e.representableWakeOutcomes(wakeable)
	if len(injectableOutcomes) == 0 {
		// A suppressed upstream error that is now due must be silently
		// acknowledged before any backoff timer is planted: planting the timer
		// first would bounce the ack off its own entry guard.
		dueSuppressed := make([]AgentEventWireRecord, 0, len(suppressedEvents))
		for index := range suppressedEvents {
			event := suppressedEvents[index]
			if e.isWakeableEvent(&event) {
				dueSuppressed = append(dueSuppressed, event)
			}
		}
		// A silent acknowledgement is a watermark statement: it may not cross an
		// id that is still unconfirmed locally.
		barrier := e.unconfirmedWatermarkBarrier()
		ackableSuppressed := dueSuppressed
		if barrier != nil {
			ackableSuppressed = make([]AgentEventWireRecord, 0, len(dueSuppressed))
			for _, event := range dueSuppressed {
				if event.ID < barrier.EventID {
					ackableSuppressed = append(ackableSuppressed, event)
				}
			}
		}
		if len(ackableSuppressed) > 0 {
			e.scheduleSilentUpstreamErrorAck(ctx, ackableSuppressed)
			return
		}
		if barrier != nil && len(dueSuppressed) > 0 {
			e.noteWakeWatermarkHoldBack(barrier)
		}
		// No suppressed event is due: plant a backoff timer for the next future
		// `nextAttemptAt`, and for a dead letter's own retry deadline.
		candidateIDs := make([]int, 0, len(candidates)+len(suppressedEvents))
		for _, outcome := range candidates {
			candidateIDs = append(candidateIDs, outcome.EventID)
		}
		for _, event := range suppressedEvents {
			candidateIDs = append(candidateIDs, event.ID)
		}
		now := e.nowMS()
		var nextAttemptAt *int64
		for _, eventID := range candidateIDs {
			options := make([]*int64, 0, 2)
			if pending := e.findPendingByID(eventID); pending != nil {
				options = append(options, pending.NextAttemptAt)
			} else {
				options = append(options, nil)
			}
			if e.wakeRetryableEventIDs[eventID] {
				options = append(options, e.deadLetterRetryAt(eventID))
			} else {
				options = append(options, nil)
			}
			for _, value := range options {
				if value == nil || *value <= now {
					continue
				}
				if nextAttemptAt == nil || *value < *nextAttemptAt {
					copied := *value
					nextAttemptAt = &copied
				}
			}
		}
		if nextAttemptAt != nil {
			delay := time.Duration(*nextAttemptAt-now) * time.Millisecond
			e.wakeTimer = e.timers.SetTimeout(delay, func() {
				e.mu.Lock()
				defer e.mu.Unlock()
				e.wakeTimer = nil
				e.scheduleWakeLocked(ctx)
			})
		}
		return
	}

	// An in-flight batch owns the ack cursor and a busy orchestrator must not be
	// interrupted, so neither is woken immediately.
	inFlight := e.deliveredBatch != nil || e.ackInFlight
	if inFlight || isBusy(ctx) {
		e.scheduleDeferredWake(ctx)
		return
	}
	e.scheduleWakeTimer(ctx)
}

// isBusy reports `ctx.isIdle?.() === false`, i.e. the host answered "busy".
func isBusy(ctx Context) bool {
	idle, known := ctx.IsIdle()
	return known && !idle
}

// scheduleWakeTimer mirrors the TS `setTimeout(() => void startWake(), WAKE_SETTLE_MS)`
// tail of `scheduleWake`.
func (e *HerdsmanExtension) scheduleWakeTimer(ctx Context) {
	generation := e.wakeGeneration
	scope := e.currentScope
	ownerHerdrSessionName := scope.HerdrSessionName
	ownerTerminalID := scope.TerminalID
	ownerWorkspaceID := scope.WorkspaceID
	e.wakeTimer = e.timers.SetTimeout(time.Duration(WakeSettleMS)*time.Millisecond, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.startWake(ctx, generation, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID)
	})
}

// startWake mirrors the TS `startWake` closure.
//
// The caller holds e.mu; the body releases it around the one blocking request
// it makes and returns with e.mu held again, so the wake never parks the core
// mutex on the daemon socket.
func (e *HerdsmanExtension) startWake(ctx Context, generation int, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID string) {
	if !e.wakeOwnerLocked(generation, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID) {
		e.wakeTimer = nil
		return
	}
	if isBusy(ctx) {
		e.wakeTimer = nil
		e.scheduleDeferredWake(ctx)
		return
	}

	var response *ConnectionStateResponse
	if client := e.client; client != nil {
		guard := e.rpcGuardLocked()
		e.mu.Unlock()
		raw, err := client.Request("agent.orchestrator.get", map[string]any{})
		e.mu.Lock()
		if err != nil {
			e.wakeTimer = nil
			// A failed load is only temporary: the batch stays pending.
			ctx.UI().Notify("Herdsman couldn’t load agent updates · updates remain pending", NotifyWarning)
			return
		}
		// The response belongs to the connection and scope it was asked for; a
		// reconnect, a scope reset or a role loss under the request drops it.
		if e.closed || e.client != guard.client ||
			!e.wakeOwnerLocked(generation, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID) {
			e.wakeTimer = nil
			return
		}
		response = decodeConnectionStateResponse(raw)
	}
	if response == nil {
		e.wakeTimer = nil
		return
	}
	e.applyConnectionStateResponse(response, ctx, applyOptions{})

	if !e.wakeOwnerLocked(generation, ownerHerdrSessionName, ownerTerminalID, ownerWorkspaceID) {
		e.wakeTimer = nil
		return
	}
	if isBusy(ctx) {
		e.wakeTimer = nil
		e.scheduleDeferredWake(ctx)
		return
	}

	batchEvents := append([]AgentEventWireRecord{}, e.pendingEvents...)
	sort.Slice(batchEvents, func(left, right int) bool { return batchEvents[left].ID < batchEvents[right].ID })
	batchProjection := e.projectPendingOutcomes(batchEvents)
	batchSuppressedIDs := map[int]bool{}
	for _, eventID := range batchProjection.SuppressedEventIDs {
		batchSuppressedIDs[eventID] = true
	}
	projectable := make([]AgentOutcome, 0, len(batchProjection.Outcomes))
	for _, outcome := range batchProjection.Outcomes {
		if outcome.EventID <= e.failedWakeThroughID {
			continue
		}
		if e.alreadyPresented(outcome.EventID) {
			continue
		}
		var event *AgentEventWireRecord
		for index := range batchEvents {
			if batchEvents[index].ID == outcome.EventID {
				event = &batchEvents[index]
				break
			}
		}
		if !e.isWakeableEvent(event) {
			continue
		}
		projectable = append(projectable, outcome)
	}
	batchLeadOutcomes, batchOutcomes := e.representableWakeOutcomes(projectable)
	if len(batchOutcomes) == 0 {
		e.wakeTimer = nil
		return
	}
	incomingIDs := map[int]bool{}
	for _, outcome := range batchOutcomes {
		incomingIDs[outcome.EventID] = true
	}
	incomingEvents := make([]AgentEventWireRecord, 0, len(batchOutcomes))
	for _, event := range batchEvents {
		if incomingIDs[event.ID] {
			incomingEvents = append(incomingEvents, event)
		}
	}
	previousBatch := e.deliveredBatch
	e.wakeTimer = nil
	e.wakeRequested = true
	e.wakeRequestedThroughID = batchOutcomes[len(batchOutcomes)-1].EventID

	wakeContent := FormatAgentOutcomeUpdates(batchOutcomes)
	// Single line, injection path only.
	idleDescription := "unknown"
	if _, known := ctx.IsIdle(); known {
		idleDescription = boolText(!isBusy(ctx))
	}
	logInfo("[herdsman-pi] wake inject deliverAs=followUp triggerTurn=true runActive=" + boolText(e.runActive) +
		" isIdle=" + idleDescription +
		" eventIds=" + formatInt(int64(batchOutcomes[0].EventID)) + "-" + formatInt(int64(batchOutcomes[len(batchOutcomes)-1].EventID)) +
		" count=" + formatInt(int64(len(batchOutcomes))))

	detailsEventIDs := make([]any, 0, len(batchEvents))
	for _, event := range batchEvents {
		if batchSuppressedIDs[event.ID] {
			continue
		}
		detailsEventIDs = append(detailsEventIDs, event.ID)
	}
	presentedEventIDs := make([]any, 0, len(batchOutcomes))
	for _, outcome := range batchOutcomes {
		presentedEventIDs = append(presentedEventIDs, outcome.EventID)
	}
	deliverAs := "followUp"
	triggerTurn := true
	sendErr := e.pi.SendMessage(CustomMessage{
		Content:    wakeContent,
		CustomType: wakeContextCustomType,
		Details: map[string]any{
			"eventIds":          detailsEventIDs,
			"presentedEventIds": presentedEventIDs,
		},
		Display: false,
	}, SendMessageOptions{DeliverAs: &deliverAs, TriggerTurn: &triggerTurn})
	if sendErr != nil {
		// Nothing was sent, so nothing is awaiting consumption on its account:
		// the ids only enter the awaiting set once Pi accepted the message.
		e.deliveredBatch = nil
		e.wakeRequested = false
		return
	}
	for _, outcome := range batchOutcomes {
		if len(batchLeadOutcomes) == 0 && e.wakeRetryableEventIDs[outcome.EventID] {
			e.wakeDeadLetterAttempts[outcome.EventID]++
		}
		e.wakeAwaitingConsumption[outcome.EventID] = true
		delete(e.wakeRetryableEventIDs, outcome.EventID)
		delete(e.wakeDeadLetterRetryAt, outcome.EventID)
	}
	batch := &DeliveredBatch{
		Events:            e.mergeUnackedDelivered(incomingEvents),
		Invalidated:       false,
		OwnerTerminalID:   ownerTerminalID,
		HerdsmanTriggered: true,
	}
	if previousBatch != nil {
		batch.AbortedByUser = previousBatch.AbortedByUser
		batch.AssistantFinalSucceeded = previousBatch.AssistantFinalSucceeded
		batch.HasSubstantiveWork = previousBatch.HasSubstantiveWork
	}
	e.deliveredBatch = batch
	e.wakeRequested = false
	e.wakeRequestedThroughID = 0
	e.wakeBusySpinRung = 0
}

// boolText renders a bool the way `String(value)` does.
func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
