package herdsmanpi

// Port of packages/herdsman-pi/src/index.ts (the extension body).
//
// The TS file is one closure per extension instance: `createHerdsmanPiExtension`
// registers a command and eleven event handlers, and everything else is a local
// helper over the mutable `state` object. This port keeps the same shape — one
// `Extension` struct with the same fields and helpers — but programs against the
// small host interfaces in extensionhost.go instead of `pi.*`/`ctx`, so the
// behaviour can be tested without a live host and the SDK adapter stays a thin
// translation layer.
//
// Every helper keeps the TS name (lowerCamelCase -> unexported Go name) and the
// TS comments that explain *why* a branch exists; the port deliberately does not
// restate them in different words, because they are the contract the tests pin.

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// Constants and exported knobs mirroring the TS module.
const (
	CommandUsage        = "Usage: /herdsman [on|off|status]"
	HerdrRequiredMsg    = "Herdsman requires a Herdr workspace"
	ReconnectingMessage = "Herdsman is reconnecting · try again shortly"

	// MaxAckAttempts mirrors MAX_ACK_ATTEMPTS.
	MaxAckAttempts = 5
	// AckBackoffCapMS mirrors ACK_BACKOFF_CAP_MS.
	AckBackoffCapMS = 30_000
	// KeepaliveIntervalMS mirrors KEEPALIVE_INTERVAL_MS.
	KeepaliveIntervalMS = 30_000
	// WakeBusySpinMS mirrors WAKE_BUSY_SPIN_MS: the first rung of the ladder.
	WakeBusySpinMS = 100
	// WakeBusyBackoffCapMS mirrors WAKE_BUSY_BACKOFF_CAP_MS.
	WakeBusyBackoffCapMS = 5_000
	// WakeDeadLetterRetryLimit mirrors WAKE_DEAD_LETTER_RETRY_LIMIT.
	WakeDeadLetterRetryLimit = 2
	// WakeBarrierNoteIntervalMS mirrors WAKE_BARRIER_NOTE_INTERVAL_MS.
	WakeBarrierNoteIntervalMS = 60_000
)

// WakeBusyBackoffMS mirrors WAKE_BUSY_BACKOFF_MS.
var WakeBusyBackoffMS = []int64{
	WakeBusySpinMS,
	200,
	500,
	1_000,
	2_000,
	WakeBusyBackoffCapMS,
}

// wakeDeadLetterRetryBackoffMS mirrors WAKE_DEAD_LETTER_RETRY_BACKOFF_MS.
var wakeDeadLetterRetryBackoffMS = []int64{5_000, 15_000}

// AckFailureClass mirrors the TS union.
type AckFailureClass string

const (
	AckTerminal  AckFailureClass = "terminal"
	AckResync    AckFailureClass = "resync"
	AckTransient AckFailureClass = "transient"
)

// ackFailureClassByCode mirrors ACK_FAILURE_CLASS_BY_CODE. The order is not part
// of the contract (a map lookup); the members are.
var ackFailureClassByCode = map[string]AckFailureClass{
	"ORCHESTRATOR_NOT_OWNER":           AckTerminal,
	"ORCHESTRATOR_EVENT_INVALIDATED":   AckTerminal,
	"ORCHESTRATOR_EVENT_FAILED":        AckTerminal,
	"ORCHESTRATOR_EVENT_ALREADY_ACKED": AckTerminal,
	"ORCHESTRATOR_EVENT_NOT_IN_SCOPE":  AckTerminal,
	"ORCHESTRATOR_EVENT_OUT_OF_ORDER":  AckResync,
	"ORCHESTRATOR_OWNER_REPLACED":      AckTerminal,
	"ORCHESTRATOR_EVENT_NOT_FOUND":     AckTerminal,
	"ORCHESTRATOR_BUSY":                AckTransient,
	"ORCHESTRATOR_CONNECTION_LOST":     AckTransient,
	"ORCHESTRATOR_RECONCILING":         AckTransient,
	"ORCHESTRATOR_ACK_TIMEOUT":         AckTransient,
	"event_invalidated":                AckTerminal,
}

// ackError is the shape classifyAckFailure reads: `{ code?: string; message? }`.
type ackError interface {
	error
	// AckCode is the optional structured code the daemon attaches.
	AckCode() string
}

// codeOf reads the structured code off an error, if it carries one.
func codeOf(err error) string {
	var coded ackError
	if errors.As(err, &coded) {
		return coded.AckCode()
	}
	return ""
}

// ErrNotConnected mirrors the TS `throw new Error("Herdsman Pi is not
// connected; cannot acknowledge notifications")`.
var ErrNotConnected = errors.New("Herdsman Pi is not connected; cannot acknowledge notifications")

// ErrClientReconnecting is what an in-flight request is rejected with when a
// newer connection attempt supersedes the socket it was written to.
var ErrClientReconnecting = errors.New("Herdsman daemon connection was replaced")

// ClassifyAckFailure mirrors the TS `classifyAckFailure`.
func ClassifyAckFailure(err error) AckFailureClass {
	if err == nil {
		return AckTransient
	}
	if code := codeOf(err); code != "" {
		if class, found := ackFailureClassByCode[code]; found {
			return class
		}
		return AckTransient
	}
	message := err.Error()
	if matchesAny(message,
		`no longer pending`,
		`invalidated`,
		`Only the current orchestrator can acknowledge notifications`,
	) {
		return AckTerminal
	}
	if strings.Contains(message, "Only the next pending orchestrator event can be acknowledged") {
		return AckResync
	}
	return AckTransient
}

// matchesAny reports whether message contains one of the fixed substrings. The
// TS applies a case-insensitive regexp to these literals, so the comparison is
// folded here.
func matchesAny(message string, needles ...string) bool {
	lowered := strings.ToLower(message)
	for _, needle := range needles {
		if strings.Contains(lowered, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

// ackFailureCode mirrors the TS `ackFailureCode`: the code, else the message.
func ackFailureCode(err error) string {
	if code := codeOf(err); code != "" {
		return code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// ackBackoffMs mirrors the TS `ackBackoffMs`: 250ms doubling, capped.
func ackBackoffMs(attempts int) int64 {
	exponent := attempts - 1
	if exponent < 0 {
		exponent = 0
	}
	// 2**n overflows int64 well before the cap for absurd attempt counts; the
	// loop saturates instead of shifting into the sign bit.
	delay := int64(250)
	for index := 0; index < exponent; index++ {
		if delay >= AckBackoffCapMS {
			break
		}
		delay *= 2
	}
	if delay > AckBackoffCapMS {
		delay = AckBackoffCapMS
	}
	return delay
}

// DefaultHerdsmanHome mirrors the TS `defaultHerdsmanHome` in index.ts: the
// environment value verbatim (an empty value falls back), no absolute-path test.
func DefaultHerdsmanHome() string {
	if home := getenv("HERDSMAN_HOME"); home != "" {
		return home
	}
	return getenv("HOME") + "/.herdsman"
}

// DefaultSocketPath mirrors the TS `defaultSocketPath`.
func DefaultSocketPath() string {
	return strings.TrimSuffix(DefaultHerdsmanHome(), "/") + "/herdsman.sock"
}

// getenv indirection so tests can drive the process environment.
var getenv = func(key string) string {
	value, _ := lookupEnv(key)
	return value
}

// AgentSessionRef mirrors the TS `AgentSessionRef`.
type AgentSessionRef struct {
	Agent  string
	Kind   string
	Source string
	Value  string
}

// CurrentScope mirrors the TS `CurrentScope`.
type CurrentScope struct {
	HerdrSessionName string
	PaneID           string
	TerminalID       string
	WorkspaceID      string
}

// LaunchIdentity mirrors the TS `LaunchIdentity`.
type LaunchIdentity struct {
	HerdrSocketPath string
	PaneID          string
	WorkspaceID     string
}

// DeliveredBatch mirrors the TS `DeliveredBatch`.
type DeliveredBatch struct {
	AbortedByUser           bool
	AssistantFinalSucceeded bool
	Events                  []AgentEventWireRecord
	HasSubstantiveWork      bool
	Invalidated             bool
	OwnerTerminalID         string
	HerdsmanTriggered       bool
}

// ExtensionOptions mirrors the TS `ExtensionOptions`.
type ExtensionOptions struct {
	ClientFactory          func() HerdsmanDaemonClient
	OnTurnCompletionSignal func(completion <-chan struct{})
	OnStateExposed         func(state *HerdsmanExtension)
	WakeFilter             *WakeFilterConfig
	Timers                 Timers
	Env                    func(key string) (string, bool)
	// ConfirmSessionWrite is the turn-signal probe; nil uses the real one.
	ConfirmSessionWrite func(expectedText, path string) TurnCompletionCheck
}

// Extension is the ported `herdsmanPiExtension` closure: the state and helpers
// of one extension instance.
type HerdsmanExtension struct {
	pi         PiAPI
	timers     Timers
	env        func(key string) (string, bool)
	options    ExtensionOptions
	wakeFilter WakeFilterConfig

	// mu serialises the state machine. The TS is single-threaded; the port has
	// goroutines (timers, daemon callbacks, host requests), so every entry point
	// and every timer callback takes it.
	mu sync.Mutex

	autoClaimAttempted       bool
	client                   HerdsmanDaemonClient
	connected                bool
	currentScope             *CurrentScope
	deliveredBatch           *DeliveredBatch
	ackInFlight              bool
	failedWakeThroughID      int
	isOrchestrator           bool
	launchIdentity           *LaunchIdentity
	latestContext            *AgentWorkspaceContextSnapshot
	pendingEvents            []AgentEventWireRecord
	presentedEventIDs        map[int]bool
	reconnectingFromOn       bool
	registrationInFlight     chan struct{}
	roleMutationInFlight     bool
	runActive                bool
	sessionRef               *AgentSessionRef
	subscriberID             *string
	unackedDelivered         map[int]AgentEventWireRecord
	wakeDeferredUntilSettled bool
	wakeAwaitingConsumption  map[int]bool
	wakeConsumptionObserved  map[int]bool
	wakeSuppressedEventIDs   map[int]bool
	wakeSkipLogReasons       map[int]string
	wakeRetryableEventIDs    map[int]bool
	wakeDeadLetterAttempts   map[int]int
	wakeDeadLetterRetryAt    map[int]int64
	wakeBusySpinRung         int
	wakeRequested            bool
	wakeRequestedThroughID   int
	wakeTimer                Timer
	completedPaneKeys        map[string]bool

	completedPanes      map[CompletedPaneKey]int
	activeContext       Context
	keepaliveTimer      Timer
	wakeGeneration      int
	lastWakeBarrierNote *barrierNote
	closed              bool

	// connectionEpoch counts connection-level resets (session start/shutdown,
	// a disconnect, a fresh onConnected callback); scopeEpoch counts role and
	// scope mutations. Together they are the cheap identity of "the state a
	// blocking request was issued under": a response captured under an older
	// pair belongs to a dead cycle and must never be applied to the live one.
	connectionEpoch int
	scopeEpoch      int
}

// barrierNote mirrors the TS `lastWakeBarrierNote`.
type barrierNote struct {
	at  int64
	key string
}

// NewExtension builds the extension instance. The TS factory returns a function
// that the host calls with `pi`; this returns first and takes `pi` in Start so
// tests can create the state and a fake host in either order.
func NewExtension(pi PiAPI, options ExtensionOptions) *HerdsmanExtension {
	timers := options.Timers
	if timers == nil {
		timers = realTimers{}
	}
	env := options.Env
	if env == nil {
		env = lookupEnv
	}
	wakeFilter := DefaultWakeFilterConfig
	if options.WakeFilter != nil {
		wakeFilter = *options.WakeFilter
	} else {
		wakeFilter = loadWakeFilterFromEnv(env)
	}
	e := &HerdsmanExtension{
		pi:                      pi,
		timers:                  timers,
		env:                     env,
		options:                 options,
		wakeFilter:              wakeFilter,
		presentedEventIDs:       map[int]bool{},
		unackedDelivered:        map[int]AgentEventWireRecord{},
		wakeAwaitingConsumption: map[int]bool{},
		wakeConsumptionObserved: map[int]bool{},
		wakeSuppressedEventIDs:  map[int]bool{},
		wakeSkipLogReasons:      map[int]string{},
		wakeRetryableEventIDs:   map[int]bool{},
		wakeDeadLetterAttempts:  map[int]int{},
		wakeDeadLetterRetryAt:   map[int]int64{},
		completedPaneKeys:       map[string]bool{},
		completedPanes:          map[CompletedPaneKey]int{},
	}
	if options.OnStateExposed != nil {
		options.OnStateExposed(e)
	}
	return e
}

// envValue reads one environment value through the injected lookup.
func (e *HerdsmanExtension) envValue(key string) string {
	value, _ := e.env(key)
	return value
}

// rpcGuard is the identity a blocking daemon request was issued under. e.mu
// must never be held across `client.Request` (the notification path answers on
// that same socket and needs the lock), so the request runs with the lock
// released and its response is only applied while this snapshot still
// describes the live connection and scope.
//
// `wakeGeneration` deliberately stays out of it: that counter tracks wake
// *scheduling*, and a wake rescheduled while a request was in flight must not
// throw away an acknowledgement or a pending page the daemon already produced
// for the same scope.
type rpcGuard struct {
	client          HerdsmanDaemonClient
	scope           CurrentScope
	connectionEpoch int
	scopeEpoch      int
	subscriberID    string
}

// rpcGuardLocked snapshots the guard. The caller holds e.mu.
func (e *HerdsmanExtension) rpcGuardLocked() rpcGuard {
	guard := rpcGuard{
		client:          e.client,
		connectionEpoch: e.connectionEpoch,
		scopeEpoch:      e.scopeEpoch,
	}
	if e.currentScope != nil {
		guard.scope = *e.currentScope
	}
	if e.subscriberID != nil {
		guard.subscriberID = *e.subscriberID
	}
	return guard
}

// rpcGuardValid reports whether a response may still be applied to the live
// state of the active orchestrator. The caller holds e.mu.
func (e *HerdsmanExtension) rpcGuardValid(guard rpcGuard) bool {
	if !e.connectionStillLive(guard) {
		return false
	}
	// The role epoch is checked explicitly: a guard captured before this Pi
	// gained (or lost) the role describes a state that no longer exists.
	return e.isOrchestrator && e.scopeEpoch == guard.scopeEpoch
}

// connectionStillLive is rpcGuardValid without the role check: `/herdsman on`
// legitimately reads and writes the role while this Pi is not the orchestrator.
// The caller holds e.mu.
//
// The subscriber is part of the identity: a new session replaces the subscriber
// while the client object, the epoch pair and the scope can all survive it, and
// a command or response answered for the old subscriber describes a session
// this core no longer is.
func (e *HerdsmanExtension) connectionStillLive(guard rpcGuard) bool {
	if !e.closed && e.connected && e.client != nil && e.client == guard.client &&
		e.connectionEpoch == guard.connectionEpoch &&
		e.subscriberID != nil && *e.subscriberID == guard.subscriberID {
		return e.currentScope != nil && *e.currentScope == guard.scope
	}
	return false
}

// registrationCurrentLocked reports whether a presence registration is still the
// live one for the connection it was created for. The caller holds e.mu.
//
// A registration goroutine outlives the connection that started it: it is
// scheduled, waits for e.mu, then blocks on the daemon round trip. By the time
// it gets the lock the client may have reconnected, the subscriber may have
// been replaced, a newer registration may own the marker, or the extension may
// have shut down. Only the very identity captured by `registerPresence` --
// while it held the lock that created the marker -- may issue the request and
// apply its response.
func (e *HerdsmanExtension) registrationCurrentLocked(client HerdsmanDaemonClient, done chan struct{}, regEpoch int, regSub string) bool {
	return !e.closed &&
		e.client != nil &&
		e.client == client &&
		e.registrationInFlight == done &&
		e.connectionEpoch == regEpoch &&
		e.subscriberID != nil &&
		*e.subscriberID == regSub
}

// clearAgentContext mirrors the TS `clearAgentContext`.
func (e *HerdsmanExtension) clearAgentContext() {
	e.latestContext = nil
	e.runActive = false
}

// syncCompletedPaneKeys mirrors the TS `syncCompletedPaneKeys`.
func (e *HerdsmanExtension) syncCompletedPaneKeys() {
	e.completedPaneKeys = map[string]bool{}
	for key := range e.completedPanes {
		e.completedPaneKeys[string(key)] = true
	}
}

// projectPendingOutcomes mirrors the TS `projectPendingOutcomes`.
func (e *HerdsmanExtension) projectPendingOutcomes(events []AgentEventWireRecord) AgentOutcomeProjection {
	projection := ProjectAgentOutcomes(events, e.wakeFilter, e.completedPanes)
	e.syncCompletedPaneKeys()
	return projection
}

// ── keepalive ───────────────────────────────────────────────────────────────

func (e *HerdsmanExtension) stopKeepalive() {
	if e.keepaliveTimer == nil {
		return
	}
	e.keepaliveTimer.Stop()
	e.keepaliveTimer = nil
}

func (e *HerdsmanExtension) startKeepalive(client HerdsmanDaemonClient) {
	e.stopKeepalive()
	e.keepaliveTimer = e.timers.SetInterval(KeepaliveIntervalMS*time.Millisecond, func() {
		e.mu.Lock()
		current := e.client
		mutable := e.roleMutationInFlight
		closed := e.closed
		e.mu.Unlock()
		_ = mutable
		if closed || current == nil || current != client {
			return
		}
		// `void client.request(...).catch(() => undefined)`: the ping is optional
		// and its failure is never surfaced.
		_, _ = client.Request("agent.ping", map[string]any{})
	})
}

// ── UI ──────────────────────────────────────────────────────────────────────

// setHerdsmanUI mirrors the TS `setHerdsmanUi`.
func (e *HerdsmanExtension) setHerdsmanUI(ctx Context) {
	if ctx == nil {
		return
	}
	pendingOutcomeCount := len(ProjectAgentOutcomes(e.pendingEvents, e.wakeFilter, cloneCompletedPanes(e.completedPanes)).Outcomes)
	var footerState HerdsmanFooterState
	switch {
	case e.reconnectingFromOn:
		footerState = FooterReconnectingState()
	case e.isOrchestrator:
		footerState = FooterOnState(pendingOutcomeCount)
	default:
		footerState = FooterOffState()
	}
	ctx.UI().SetStatus("herdsman", FormatHerdsmanFooterStatus(footerState))
	var waitingForTurn *string
	if e.isOrchestrator && e.wakeDeferredUntilSettled && pendingOutcomeCount > 0 {
		text := "Herdsman · waiting for the current turn to end"
		waitingForTurn = &text
	}
	ctx.UI().SetStatus("herdsman-wake", waitingForTurn)
}

// cloneCompletedPanes copies the completion evidence for a read-only projection.
func cloneCompletedPanes(source map[CompletedPaneKey]int) map[CompletedPaneKey]int {
	clone := make(map[CompletedPaneKey]int, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// ── wake timers ─────────────────────────────────────────────────────────────

func (e *HerdsmanExtension) cancelWakeTimer() {
	e.wakeGeneration++
	if e.wakeTimer != nil {
		e.wakeTimer.Stop()
	}
	e.wakeTimer = nil
	e.wakeDeferredUntilSettled = false
	e.wakeBusySpinRung = 0
}

func (e *HerdsmanExtension) cancelWake() {
	e.cancelWakeTimer()
	e.wakeRequested = false
	e.wakeRequestedThroughID = 0
}

// ── delivery bookkeeping ────────────────────────────────────────────────────

// unackedDeliveredAscending mirrors the TS helper of the same name.
func (e *HerdsmanExtension) unackedDeliveredAscending() []AgentEventWireRecord {
	events := make([]AgentEventWireRecord, 0, len(e.unackedDelivered))
	for _, event := range e.unackedDelivered {
		events = append(events, event)
	}
	sort.Slice(events, func(left, right int) bool { return events[left].ID < events[right].ID })
	return events
}

// mergeUnackedDelivered mirrors the TS helper: merge, never substitute.
func (e *HerdsmanExtension) mergeUnackedDelivered(incoming []AgentEventWireRecord) []AgentEventWireRecord {
	for _, event := range incoming {
		if _, present := e.unackedDelivered[event.ID]; !present {
			e.unackedDelivered[event.ID] = event
		}
	}
	return e.unackedDeliveredAscending()
}

// dropUnackedDelivered mirrors the TS helper: the queue, the three id sets and
// the dead-letter bookkeeping describe the same ids and leave together.
func (e *HerdsmanExtension) dropUnackedDelivered(eventID int) {
	delete(e.unackedDelivered, eventID)
	delete(e.wakeAwaitingConsumption, eventID)
	delete(e.wakeConsumptionObserved, eventID)
	delete(e.wakeSuppressedEventIDs, eventID)
	delete(e.wakeRetryableEventIDs, eventID)
	delete(e.wakeDeadLetterAttempts, eventID)
	delete(e.wakeDeadLetterRetryAt, eventID)
}

// clearDeliveryBookkeeping mirrors the TS helper.
func (e *HerdsmanExtension) clearDeliveryBookkeeping() {
	carriedOver := sortedSetKeys(e.wakeAwaitingConsumption)
	for _, eventID := range carriedOver {
		e.wakeSuppressedEventIDs[eventID] = true
	}
	if len(carriedOver) > 0 {
		logInfo("[herdsman-pi] keeping " + formatInt(int64(len(carriedOver))) + " unconsumed wake event id(s) suppressed on a delivery reset eventIds=" +
			joinInts(carriedOver) + " · daemon redelivery for them is traded away to keep the transcript single-copy")
	}
	writtenOff := sortedSetKeys(e.wakeRetryableEventIDs)
	if len(writtenOff) > 0 {
		logInfo("[herdsman-pi] dropping " + formatInt(int64(len(writtenOff))) + " written-off wake event id(s) on a delivery reset eventIds=" +
			joinInts(writtenOff) + " · only the local dead-letter retry bookkeeping is dropped here; these ids do not join the suppression set, so if the daemon redelivers them the update may be injected again")
	}
	e.presentedEventIDs = map[int]bool{}
	e.unackedDelivered = map[int]AgentEventWireRecord{}
	e.wakeAwaitingConsumption = map[int]bool{}
	e.wakeConsumptionObserved = map[int]bool{}
	e.wakeSkipLogReasons = map[int]string{}
	e.wakeRetryableEventIDs = map[int]bool{}
	e.wakeDeadLetterAttempts = map[int]int{}
	e.wakeDeadLetterRetryAt = map[int]int64{}
	e.wakeBusySpinRung = 0
}

// sortedSetKeys returns the members of an int set in ascending order.
func sortedSetKeys(set map[int]bool) []int {
	keys := make([]int, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

// joinInts renders `[1,2,3]` as "1,2,3".
func joinInts(values []int) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = formatInt(int64(value))
	}
	return strings.Join(parts, ",")
}

// pruneAcknowledgedEvents mirrors the TS helper. The watermark sweeps the
// pending projection, the completion evidence, the delivery queue, the
// suppression set, the skip log and the dead-letter bookkeeping together.
func (e *HerdsmanExtension) pruneAcknowledgedEvents(ackedEventID *int) {
	if ackedEventID == nil {
		return
	}
	watermark := *ackedEventID
	kept := make([]AgentEventWireRecord, 0, len(e.pendingEvents))
	for _, event := range e.pendingEvents {
		if event.ID > watermark {
			kept = append(kept, event)
		}
	}
	e.pendingEvents = kept
	e.completedPanes = PruneCompletedPanes(e.completedPanes, ackedEventID)
	e.syncCompletedPaneKeys()
	for _, eventID := range sortedIntKeyMap(e.unackedDelivered) {
		if eventID <= watermark {
			e.dropUnackedDelivered(eventID)
		}
	}
	for _, eventID := range sortedSetKeys(e.wakeSuppressedEventIDs) {
		if eventID <= watermark {
			delete(e.wakeSuppressedEventIDs, eventID)
		}
	}
	for _, eventID := range sortedIntStringKeyMap(e.wakeSkipLogReasons) {
		if eventID <= watermark {
			delete(e.wakeSkipLogReasons, eventID)
		}
	}
	for _, eventID := range sortedSetKeys(e.wakeRetryableEventIDs) {
		if eventID <= watermark {
			delete(e.wakeRetryableEventIDs, eventID)
		}
	}
	for _, eventID := range sortedIntKeyMap(e.wakeDeadLetterAttempts) {
		if eventID <= watermark {
			delete(e.wakeDeadLetterAttempts, eventID)
			delete(e.wakeDeadLetterRetryAt, eventID)
		}
	}
}

// sortedIntKeyMap returns the keys of an id-keyed map in ascending order.
func sortedIntKeyMap[V any](source map[int]V) []int {
	keys := make([]int, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

// sortedIntStringKeyMap is sortedIntKeyMap for map[int]string.
func sortedIntStringKeyMap(source map[int]string) []int { return sortedIntKeyMap(source) }

// ── injection guards ────────────────────────────────────────────────────────

// noteSkippedWakeInjection mirrors the TS helper: once per id and reason. The
// helper is only reached for an id that is already presented, awaiting
// consumption or suppressed, so it reports "this id has been shown": the TS
// returns true here and `alreadyPresented` forwards that verdict, which is what
// keeps a daemon redelivery from being injected a second time.
func (e *HerdsmanExtension) noteSkippedWakeInjection(eventID int, reason string) bool {
	if previous, found := e.wakeSkipLogReasons[eventID]; !found || previous != reason {
		e.wakeSkipLogReasons[eventID] = reason
		logInfo("[herdsman-pi] wake injection skipped eventId=" + formatInt(int64(eventID)) + " reason=" + reason +
			" · update already presented (or still in flight) in this session, so a daemon redelivery is not shown twice")
	}
	return true
}

// alreadyPresented mirrors the TS helper.
func (e *HerdsmanExtension) alreadyPresented(eventID int) bool {
	if e.wakeRetryableEventIDs[eventID] {
		return false
	}
	if e.wakeAwaitingConsumption[eventID] {
		return e.noteSkippedWakeInjection(eventID, "awaiting")
	}
	if e.presentedEventIDs[eventID] {
		return e.noteSkippedWakeInjection(eventID, "presented")
	}
	if e.wakeSuppressedEventIDs[eventID] {
		return e.noteSkippedWakeInjection(eventID, "suppressed")
	}
	return false
}

// deadLetterRetryBackoffMs mirrors the TS helper.
func deadLetterRetryBackoffMs(attempts int) int64 {
	index := attempts
	if index < 0 {
		index = 0
	}
	if index > len(wakeDeadLetterRetryBackoffMS)-1 {
		index = len(wakeDeadLetterRetryBackoffMS) - 1
	}
	return wakeDeadLetterRetryBackoffMS[index]
}

// nowMS is Date.now() on the injected clock.
func (e *HerdsmanExtension) nowMS() int64 { return e.timers.Now().UnixMilli() }

// deadLetterRetryDue mirrors the TS helper.
func (e *HerdsmanExtension) deadLetterRetryDue(eventID int) bool {
	if !e.wakeRetryableEventIDs[eventID] {
		return false
	}
	if e.wakeDeadLetterAttempts[eventID] >= WakeDeadLetterRetryLimit {
		return false
	}
	return e.wakeDeadLetterRetryAt[eventID] <= e.nowMS()
}

// deadLetterRetryAt mirrors the TS helper.
func (e *HerdsmanExtension) deadLetterRetryAt(eventID int) *int64 {
	if !e.wakeRetryableEventIDs[eventID] {
		return nil
	}
	if e.wakeDeadLetterAttempts[eventID] >= WakeDeadLetterRetryLimit {
		return nil
	}
	value, present := e.wakeDeadLetterRetryAt[eventID]
	if !present {
		return nil
	}
	return &value
}

// representableWakeOutcomes mirrors the TS helper.
func (e *HerdsmanExtension) representableWakeOutcomes(candidates []AgentOutcome) (lead []AgentOutcome, outcomes []AgentOutcome) {
	lead = make([]AgentOutcome, 0, len(candidates))
	for _, outcome := range candidates {
		if !e.wakeRetryableEventIDs[outcome.EventID] {
			lead = append(lead, outcome)
		}
	}
	if len(lead) > 0 {
		return lead, candidates
	}
	outcomes = make([]AgentOutcome, 0, len(candidates))
	for _, outcome := range candidates {
		if e.deadLetterRetryDue(outcome.EventID) {
			outcomes = append(outcomes, outcome)
		}
	}
	return lead, outcomes
}

// isWakeableEvent mirrors the TS helper on the injected clock.
func (e *HerdsmanExtension) isWakeableEvent(event *AgentEventWireRecord) bool {
	if event == nil || event.NextAttemptAt == nil {
		return true
	}
	return *event.NextAttemptAt <= e.nowMS()
}

// findPendingByID mirrors `state.pendingEvents.find((pending) => pending.id === id)`.
func (e *HerdsmanExtension) findPendingByID(eventID int) *AgentEventWireRecord {
	for index := range e.pendingEvents {
		if e.pendingEvents[index].ID == eventID {
			return &e.pendingEvents[index]
		}
	}
	return nil
}

// attemptsOf mirrors `pending?.attempts ?? event.attempts ?? 0`.
func attemptsOf(pending *AgentEventWireRecord, event AgentEventWireRecord) int {
	if pending != nil && pending.Attempts != nil {
		return *pending.Attempts
	}
	if event.Attempts != nil {
		return *event.Attempts
	}
	return 0
}

// ── connection state helpers ────────────────────────────────────────────────

// isLocalOwner mirrors the TS `isLocalOwner`.
func isLocalOwner(response *ConnectionStateResponse) bool {
	if response == nil || response.State == nil || response.State.Owner == nil {
		return false
	}
	return response.State.Owner.TerminalID == response.Presence.TerminalID &&
		response.State.HerdrSessionName == response.Presence.HerdrSessionName &&
		response.State.WorkspaceID == response.Presence.WorkspaceID
}

// localStatusMessage mirrors the TS helper.
func localStatusMessage(response *ConnectionStateResponse) string {
	if !isLocalOwner(response) || response.State == nil || response.State.Owner == nil {
		return "Herdsman is off"
	}
	scope := response.Presence.HerdrSessionName + "/" + response.Presence.WorkspaceID
	return "Herdsman is watching agent updates · " + scope + " · " + response.State.Owner.PaneID
}

// notifyLocalStatus mirrors the TS helper.
func notifyLocalStatus(response *ConnectionStateResponse, ctx Context) {
	ctx.UI().Notify(localStatusMessage(response), NotifyInfo)
}

// ackedEventIDOf mirrors `response.state?.ackedEventId ?? response.ackedEventId`:
// a present state object always wins (its ackedEventId is even `0`), the
// top-level field is the fallback.
func ackedEventIDOf(response *ConnectionStateResponse) *int {
	if response == nil {
		return nil
	}
	if response.State != nil {
		value := response.State.AckedEventID
		return &value
	}
	if response.AckedEventID != nil {
		return response.AckedEventID
	}
	return nil
}

// herdrLaunchIdentity mirrors the TS helper.
func herdrLaunchIdentity(environ func(key string) (string, bool)) *LaunchIdentity {
	if value, _ := environ("HERDR_ENV"); value != "1" {
		return nil
	}
	rawSocketPath, _ := environ("HERDR_SOCKET_PATH")
	rawPaneID, _ := environ("HERDR_PANE_ID")
	rawWorkspaceID, _ := environ("HERDR_WORKSPACE_ID")
	socketPath, socketOK := stringValueOK(rawSocketPath)
	paneID, paneOK := stringValueOK(rawPaneID)
	workspaceID, workspaceOK := stringValueOK(rawWorkspaceID)
	if !socketOK || !paneOK || !workspaceOK {
		return nil
	}
	return &LaunchIdentity{HerdrSocketPath: socketPath, PaneID: paneID, WorkspaceID: workspaceID}
}

// lookupEnv is the default process-environment lookup.
func lookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

// stringValueOK is the TS `stringValue` as a presence test.
func stringValueOK(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	return value, true
}

// lookupEnv is the default process-environment lookup.
func lookupEnvImpl(key string) (string, bool) { return os.LookupEnv(key) }

// cleanContextPattern is the control/special class the context cleaner clears:
// C0 controls except \t \n, DEL, the C1 range, soft hyphen, Mongolian vowel
// separator, the zero-width set, the bidi embedding/override set, the word
// joiner set and BOM. NEL (U+0085) is deliberately absent: it is folded to LF
// by the next rule.
var cleanContextPattern = regexp.MustCompile("[\\x00-\\x08\\x0b\\x0c\\x0e-\\x1f\\x7f-\\x{84}\\x{86}-\\x{9f}\\x{ad}\\x{180e}\\x{200b}-\\x{200f}\\x{202a}-\\x{202e}\\x{2060}-\\x{2064}\\x{2066}-\\x{2069}\\x{feff}]")

// cleanContextText mirrors the TS `cleanContextText` in index.ts: the same
// newline-preserving scheme as `normalizeExcerpt`, with the wider control class
// this cleaner uses.
func cleanContextText(value string) string {
	cleaned := stripVTControlCharacters(value)
	cleaned = cleanContextPattern.ReplaceAllString(cleaned, "")
	cleaned = crlfPattern.ReplaceAllString(cleaned, "\n")
	cleaned = lineSeparatorPattern.ReplaceAllString(cleaned, "\n")
	cleaned = trailingSpacePattern.ReplaceAllString(cleaned, "")
	cleaned = blankLinesPattern.ReplaceAllString(cleaned, "\n\n")
	return strings.TrimSpace(cleaned)
}

// isMainOrchestratorPane mirrors the TS helper.
func isMainOrchestratorPane(paneID string) bool {
	return strings.HasSuffix(strings.TrimSpace(paneID), ":p1")
}

// isDispatchedRoleSession mirrors the TS helper.
func isDispatchedRoleSession(sessionPath string, environ func(key string) (string, bool)) bool {
	if role, _ := environ("CTXMODE_ROLE"); strings.TrimSpace(role) != "" {
		return true
	}
	return strings.HasPrefix(sessionPath, "/tmp/herdr-role-sessions/")
}

// ── hidden update formatting ────────────────────────────────────────────────

// sanitizeAndCleanContextText mirrors the TS helper.
func sanitizeAndCleanContextText(value string) string {
	preCleaned := cleanContextText(value)
	if preCleaned == "" {
		return ""
	}
	return cleanContextText(SanitizeText(preCleaned).Text)
}

// FormatHiddenAgentUpdates mirrors the TS `formatHiddenAgentUpdates`.
func FormatHiddenAgentUpdates(events []AgentEventWireRecord) string {
	lines := []string{agentUpdatesHeading}
	for _, event := range events {
		payload := asRecord(event.Payload)
		var history CompactAgentHistory
		if event.CompactHistory != nil {
			history = *event.CompactHistory
		}
		agent := "unknown"
		if value := stringValue(payload["agent"]); value != nil {
			agent = *value
		}
		identity := AgentIdentityLabel(agent, stringValue(payload["name"]))
		lastAssistant := ""
		if history.LastAssistantMessage != nil {
			lastAssistant = textOrEmpty(history.LastAssistantMessage.Text)
		}
		pane := "unknown"
		if event.PaneID != nil {
			pane = *event.PaneID
		}
		lines = append(lines, "- "+event.Type+" "+identity+" "+pane+"\n"+
			"  last assistant: "+sanitizeAndCleanContextText(lastAssistant)+"\n"+
			"  event: "+formatInt(int64(event.ID)))
	}
	return strings.Join(lines, "\n")
}

// isNormalHerdsmanContext mirrors the TS helper.
//
// A `toolResult` is never herdsman's own injection: herdsman only writes
// customType-tagged entries and marker-prefixed context blocks, and a tool
// result that merely *echoes* the marker (reading this repository's markers.go,
// grepping for the constant, a wake report quoted back) is evidence the model
// must still see. So tool results are always kept, and only herdsman's own
// messages are dropped.
func isNormalHerdsmanContext(message map[string]any) bool {
	if role, _ := message["role"].(string); role == "toolResult" {
		return false
	}
	customType, _ := message["customType"].(string)
	return customType == herdsmanAgentContextCustomType ||
		customType == herdsmanWakeQueuedCustomType ||
		contentStartsWithMarker(message["content"], agentContextMarker)
}

// contentStartsWithMarker mirrors the TS helper: only content that *starts*
// with the marker is herdsman's own injected block. A `Contains` test also
// swallowed tool results and assistant text that merely mention the marker.
func contentStartsWithMarker(content any, marker string) bool {
	switch typed := content.(type) {
	case string:
		return strings.HasPrefix(strings.TrimLeftFunc(typed, unicode.IsSpace), marker)
	case []any:
		for _, block := range typed {
			record := recordOf(block)
			if contentStartsWithMarker(record["text"], marker) || contentStartsWithMarker(record["content"], marker) {
				return true
			}
		}
	}
	return false
}

// wakeConsumedEventIds mirrors the TS helper: only `details.presentedEventIds`
// is evidence.
func wakeConsumedEventIds(message map[string]any) []int {
	presented, ok := recordOf(message["details"])["presentedEventIds"].([]any)
	if !ok {
		return nil
	}
	ids := make([]int, 0, len(presented))
	for _, raw := range presented {
		if id, ok := jsNumberAsInt(raw); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// assistantMessageText mirrors the TS helper.
func assistantMessageText(message map[string]any) string {
	text := textFromContent(message["content"])
	if text == nil {
		return ""
	}
	return SanitizeText(*text).Text
}

// messageRole / messageStopReason read the two assistant-message fields.
func messageRole(message map[string]any) string {
	value, _ := message["role"].(string)
	return value
}

// contentIsPresent mirrors the TS substantive-work content test:
// `content !== undefined && ((string && length > 0) || (array && length > 0))`.
func contentIsPresent(content any) bool {
	switch typed := content.(type) {
	case nil:
		return false
	case string:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}

// loadWakeFilterFromEnv reads config.yaml plus the environment override, through
// the injected lookup so the extension and its tests see the same values.
func loadWakeFilterFromEnv(environ func(key string) (string, bool)) WakeFilterConfig {
	environment := map[string]*string{}
	for _, key := range []string{filterEnv, extraPatternsEnv} {
		if value, ok := environ(key); ok {
			copied := value
			environment[key] = &copied
		} else {
			environment[key] = nil
		}
	}
	return LoadWakeFilterConfig(environment)
}

// jsonRecord decodes an SDK-decoded value into a record (helper for the
// adapter's event payloads).
func jsonRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// decodeConnectionResponse turns a raw RPC result into the typed response, so
// the core never has to read JSON maps.
func decodeConnectionResponse(result any) *ConnectionStateResponse {
	if result == nil {
		return nil
	}
	return decodeConnectionStateResponse(result)
}

// encodeParams is the SDK's JSON encoder, used where the core must hand a value
// back through the wire (the adapter only).
func encodeParams(value any) ([]byte, error) { return json.Marshal(value) }
