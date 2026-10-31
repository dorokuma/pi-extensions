package herdsman

// Acknowledgement and delivery-queue machinery: the part of index.ts that
// decides what may be confirmed to the daemon and what must keep blocking the
// watermark. The explanations in the TS source are the contract (the daemon
// confirms by a monotonic watermark, so acking a larger id silently confirms
// every smaller one); they are kept next to the branches they justify.

import (
	"sort"
	"strings"
)

// deliveryPrefix mirrors the TS `confirmableDeliveryPrefix` result.
type deliveryPrefix struct {
	DeliveryQueue        []AgentEventWireRecord
	ConfirmablePrefix    []AgentEventWireRecord
	BlockedByMissingTurn bool
}

// watermarkBarrier mirrors the TS `unconfirmedWatermarkBarrier` result.
type watermarkBarrier struct {
	Blocked int
	EventID int
	Reason  string
}

// confirmableDeliveryPrefix mirrors the TS helper.
func (e *HerdsmanExtension) confirmableDeliveryPrefix(turnProducedFinalResponse bool) deliveryPrefix {
	deliveryQueue := e.unackedDeliveredAscending()
	prefix := make([]AgentEventWireRecord, 0, len(deliveryQueue))
	blockedByMissingTurn := false
	for _, event := range deliveryQueue {
		if e.wakeAwaitingConsumption[event.ID] || e.wakeRetryableEventIDs[event.ID] {
			break
		}
		if !e.wakeConsumptionObserved[event.ID] && !turnProducedFinalResponse {
			blockedByMissingTurn = true
			break
		}
		// Only the live projection may hold the cursor open. The delivery queue
		// keeps the snapshot of the *first* delivery, whose `attempts` is already
		// 1, so falling back to it would make every stranded queued event
		// permanently unconfirmable and `agent_settled` could never recover it.
		if pending := e.findPendingByID(event.ID); pending != nil && pending.Attempts != nil && *pending.Attempts > 0 {
			continue
		}
		prefix = append(prefix, event)
	}
	return deliveryPrefix{
		DeliveryQueue:        deliveryQueue,
		ConfirmablePrefix:    prefix,
		BlockedByMissingTurn: blockedByMissingTurn,
	}
}

// unconfirmedWatermarkBarrier mirrors the TS helper.
func (e *HerdsmanExtension) unconfirmedWatermarkBarrier() *watermarkBarrier {
	blockedSet := map[int]bool{}
	for id := range e.wakeAwaitingConsumption {
		blockedSet[id] = true
	}
	for id := range e.wakeRetryableEventIDs {
		blockedSet[id] = true
	}
	blocked := len(blockedSet)
	for _, queued := range e.unackedDeliveredAscending() {
		if e.wakeRetryableEventIDs[queued.ID] {
			reason := "dead-letter-backoff"
			if e.wakeDeadLetterAttempts[queued.ID] >= WakeDeadLetterRetryLimit {
				reason = "dead-letter-budget-exhausted"
			}
			return &watermarkBarrier{Blocked: blocked, EventID: queued.ID, Reason: reason}
		}
		if e.wakeAwaitingConsumption[queued.ID] {
			return &watermarkBarrier{Blocked: blocked, EventID: queued.ID, Reason: "awaiting-consumption"}
		}
	}
	return nil
}

// noteWakeWatermarkHoldBack mirrors the TS helper: once per state, and again
// after the note interval.
func (e *HerdsmanExtension) noteWakeWatermarkHoldBack(barrier *watermarkBarrier) {
	key := formatInt(int64(barrier.EventID)) + ":" + barrier.Reason + ":" + formatInt(int64(barrier.Blocked))
	now := e.nowMS()
	if e.lastWakeBarrierNote != nil &&
		e.lastWakeBarrierNote.key == key &&
		now-e.lastWakeBarrierNote.at < WakeBarrierNoteIntervalMS {
		return
	}
	e.lastWakeBarrierNote = &barrierNote{at: now, key: key}
	logInfo("[herdsman] wake acknowledgement held eventId=" + formatInt(int64(barrier.EventID)) +
		" reason=" + barrier.Reason + " blocked=" + formatInt(int64(barrier.Blocked)) +
		" · the daemon confirms by watermark (id <= ?), so no larger id may be acknowledged while this one is unconfirmed")
}

// writeOffStrandedWakeDelivery mirrors the TS helper.
func (e *HerdsmanExtension) writeOffStrandedWakeDelivery(eventIDs []int, ctx Context) {
	if len(eventIDs) == 0 {
		return
	}
	for _, eventID := range eventIDs {
		delete(e.wakeAwaitingConsumption, eventID)
		e.wakeRetryableEventIDs[eventID] = true
		attempts := e.wakeDeadLetterAttempts[eventID]
		if attempts < WakeDeadLetterRetryLimit {
			e.wakeDeadLetterRetryAt[eventID] = e.nowMS() + deadLetterRetryBackoffMs(attempts)
		} else {
			delete(e.wakeDeadLetterRetryAt, eventID)
		}
	}
	attempts := make([]string, len(eventIDs))
	for index, eventID := range eventIDs {
		attempts[index] = formatInt(int64(e.wakeDeadLetterAttempts[eventID]))
	}
	logWarn("[herdsman] wake delivery dead-lettered eventIds=" + joinInts(eventIDs) +
		" automaticRetries=" + strings.Join(attempts, ",") +
		" limit=" + formatInt(int64(WakeDeadLetterRetryLimit)) +
		" · updates retained unacked in daemon: no consumption evidence, nothing acknowledged, and the ids stay eligible for a later wake")
	noun := "s"
	if len(eventIDs) == 1 {
		noun = ""
	}
	ctx.UI().Notify("Herdsman · "+formatInt(int64(len(eventIDs)))+" agent update"+noun+
		" could not be delivered by a wake turn · updates retained unacked in daemon: read the agent directly for the details, or hand this workspace to another terminal so the daemon delivers it there",
		NotifyWarning)
}

// acknowledgeEventIDs mirrors the TS helper: acknowledge in ascending id order,
// one RPC each, classifying the failure.
//
// The caller must NOT hold e.mu: each iteration blocks on the daemon and the
// core's mutex has to stay free for the notification path (which answers on
// that same socket) while it does.
func (e *HerdsmanExtension) acknowledgeEventIDs(events []AgentEventWireRecord, notify bool, ctx Context, batchGuard rpcGuard) {
	e.mu.Lock()
	// The whole batch runs under one identity. Capturing the guard here — while
	// the lock is held that also collected the batch — is what stops a later
	// event from borrowing the identity of a connection the batch was issued on
	// before a reconnect or a scope change replaced it.
	guard := batchGuard
	if !e.rpcGuardValid(guard) {
		// Nothing may be acknowledged: the connection, scope or role that this
		// batch belongs to is already gone.
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	ordered := append([]AgentEventWireRecord{}, events...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].ID < ordered[right].ID })
	for _, event := range ordered {
		if !e.acknowledgeOne(event, notify, ctx, batchGuard) {
			// The connection, scope or role moved on under the failed
			// acknowledgement: the remaining events belong to a cycle that no
			// longer exists, and acking them would confirm the wrong state.
			return
		}
	}
}

// acknowledgeOne is the body of one iteration of the TS loop. It takes and
// releases e.mu itself and issues its request with the lock released. The bool
// reports whether the guard this acknowledgement was issued under is still the
// live one: false stops the loop, because the remaining events belong to a
// connection, scope or role that has already been replaced.
//
// The guard is the batch's own: it is captured once, before the first request,
// so every event of the batch is acknowledged under the same identity.
func (e *HerdsmanExtension) acknowledgeOne(event AgentEventWireRecord, notify bool, ctx Context, guard rpcGuard) bool {
	e.mu.Lock()
	client := e.client
	if !e.rpcGuardValid(guard) {
		// The batch's cycle is already gone: a missing client must never read as a
		// successful acknowledgement, and neither may a request issued under a
		// dead identity.
		e.mu.Unlock()
		return false
	}
	e.mu.Unlock()
	if client == nil {
		// A missing client must never read as a successful acknowledgement.
		return e.handleAckFailure(event, ErrNotConnected, guard, notify, ctx)
	}
	raw, err := client.Request("agent.notifications.ack", map[string]any{"eventId": event.ID})
	if err != nil {
		// The guard captured before the request is what tells the failure handler
		// whether the cycle it would write to still exists.
		return e.handleAckFailure(event, err, guard, notify, ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.rpcGuardValid(guard) {
		// The connection, scope or role moved on under the request: applying the
		// watermark of a dead delivery would prune the wrong events.
		return false
	}
	// The ack contract has no top-level field: the daemon answers
	// `{ acknowledged: true, state: toWireState(state) }`, so reading only
	// `state` here is also fail-closed.
	response := decodeConnectionStateResponse(raw)
	var ackedEventID *int
	if response != nil && response.State != nil {
		value := response.State.AckedEventID
		ackedEventID = &value
	}
	e.pruneAcknowledgedEvents(ackedEventID)
	e.removePendingEvent(event.ID)
	e.dropUnackedDelivered(event.ID)
	if event.ID > e.failedWakeThroughID {
		e.failedWakeThroughID = event.ID
	}
	e.setHerdsmanUI(ctx)
	return true
}

// removePendingEvent mirrors the TS
// `pendingEvents = pendingEvents.filter((pending) => pending.id !== eventId)`.
func (e *HerdsmanExtension) removePendingEvent(eventID int) {
	kept := make([]AgentEventWireRecord, 0, len(e.pendingEvents))
	for _, pending := range e.pendingEvents {
		if pending.ID != eventID {
			kept = append(kept, pending)
		}
	}
	e.pendingEvents = kept
}

// replacePendingEvent mirrors the TS
// `pendingEvents.map((pending) => pending.id === eventId ? next : pending)`.
func (e *HerdsmanExtension) replacePendingEvent(eventID int, next AgentEventWireRecord) {
	for index := range e.pendingEvents {
		if e.pendingEvents[index].ID == eventID {
			e.pendingEvents[index] = next
		}
	}
}

// AckCode lets ClassifyAckFailure read the structured code off a daemon RPC
// error (the TS `Object.assign(new Error(...), { code })`).
func (e *rpcFailure) AckCode() string { return e.Code }

// handleAckFailure mirrors the TS catch block of the acknowledgement loop.
//
// The attempt counter is read from the live projection, not from the delivery
// queue snapshot the caller iterated over: a stale base would pin the counter at
// 1 for good and make the MAX_ACK_ATTEMPTS dead-letter branch unreachable.
func (e *HerdsmanExtension) handleAckFailure(event AgentEventWireRecord, failure error, guard rpcGuard, notify bool, ctx Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.rpcGuardValid(guard) {
		// The failure arrived after a disconnect, a scope reset or a role change:
		// it belongs to a dead cycle. Touching the new cycle's pendingEvents,
		// unackedDelivered, failedWakeThroughID or isOrchestrator here — or
		// issuing a stale `agent.orchestrator.get` resync — would write a dead
		// cycle's state over a live one.
		return false
	}
	client := e.client
	failureCode := ackFailureCode(failure)
	classification := ClassifyAckFailure(failure)
	attempts := attemptsOf(e.findPendingByID(event.ID), event) + 1
	attemptedAt := e.nowMS()
	updated := event
	updated.Attempts = intPtr(attempts)
	updated.LastAttemptAt = int64Ptr(attemptedAt)
	updated.LastFailureCode = stringPtr(failureCode)
	e.replacePendingEvent(event.ID, updated)
	// Keep the delivery-queue entry in step with the live accounting.
	if _, present := e.unackedDelivered[event.ID]; present {
		e.unackedDelivered[event.ID] = updated
	}

	if classification == AckTerminal {
		e.removePendingEvent(event.ID)
		e.dropUnackedDelivered(event.ID)
		if event.ID > e.failedWakeThroughID {
			e.failedWakeThroughID = event.ID
		}
		if strings.Contains(strings.ToLower(failureCode), "only the current orchestrator can acknowledge notifications") {
			e.isOrchestrator = false
			logWarn("[herdsman] lost orchestrator ownership while acknowledging event " + formatInt(int64(event.ID)))
		} else {
			logWarn("[herdsman] terminal acknowledgement failure eventId=" + formatInt(int64(event.ID)) +
				" attempts=" + formatInt(int64(attempts)) + " code=" + failureCode)
		}
		e.setHerdsmanUI(ctx)
		return true
	}

	if attempts >= MaxAckAttempts {
		e.removePendingEvent(event.ID)
		e.dropUnackedDelivered(event.ID)
		if event.ID > e.failedWakeThroughID {
			e.failedWakeThroughID = event.ID
		}
		logWarn("[herdsman] acknowledgement moved to dead-letter eventId=" + formatInt(int64(event.ID)) +
			" attempts=" + formatInt(int64(attempts)) + " code=" + failureCode)
		e.setHerdsmanUI(ctx)
		return true
	}

	if classification == AckResync {
		withResync := updated
		next := attemptedAt + ackBackoffMs(attempts)
		withResync.NextAttemptAt = &next
		e.replacePendingEvent(event.ID, withResync)
		if client != nil {
			// The resync read is another blocking request: it runs with e.mu
			// released, exactly like the ack request above.
			e.mu.Unlock()
			raw, resyncErr := client.Request("agent.orchestrator.get", map[string]any{})
			e.mu.Lock()
			if !e.rpcGuardValid(guard) {
				// The resync read outlived the cycle that asked for it: the
				// reconnect, the scope change or the role change that happened
				// under it owns the state now, whatever the read answered.
				// Returning false stops the batch loop, so no later event of
				// it is acknowledged under a fresh identity.
				return false
			}
			if resyncErr != nil {
				logWarn("[herdsman] acknowledgement resync failed eventId=" + formatInt(int64(event.ID)) +
					" attempts=" + formatInt(int64(attempts)) + " code=" + ackFailureCode(resyncErr))
			} else {
				// Refresh pending data without applying the full connection
				// response: that helper schedules a new wake, which would make
				// this failed batch race with the current settlement.
				if response := decodeConnectionStateResponse(raw); response != nil {
					e.addPendingEvents(response.Events, ctx)
					e.pruneAcknowledgedEvents(ackedEventIDOf(response))
					e.setHerdsmanUI(ctx)
				}
			}
		}
		// The event stays pending with a backoff so the ack cursor can sweep it
		// later; presentedEventIDs keeps the guard until scope reset.
		return true
	}

	withBackoff := updated
	next := attemptedAt + ackBackoffMs(attempts)
	withBackoff.NextAttemptAt = &next
	e.replacePendingEvent(event.ID, withBackoff)
	if notify {
		ctx.UI().Notify("Herdsman couldn’t acknowledge agent updates · updates remain pending", NotifyWarning)
	}
	e.setHerdsmanUI(ctx)
	return true
}

// intPtr / int64Ptr / stringPtr are the small pointer helpers the wire records
// need (the TS fields are optional, so a nil means "absent").
func intPtr(value int) *int          { return &value }
func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }
func boolPtr(value bool) *bool       { return &value }
