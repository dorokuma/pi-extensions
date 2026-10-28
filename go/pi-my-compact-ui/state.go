package compactui

import "sync"

// installedCounter counts the runtime states this process created. TS builds
// one module-level `runtime` object at import time (L8-49); Go builds one
// runtimeState per Extension instance (Extension()/NewExtension), so a test
// can run several instances side by side. Every view of the ported state goes
// through the struct the component was created from, never a package global.
var installedCounter int

// runtimeState is the port of createRuntimeState (L9-46). Every field name
// keeps the TS name so the mapping in README.md is mechanical.
//
// Locking model (the TS original is single-threaded, Go is not):
//   - mu guards the state itself: the maps/sets/slices the event handlers
//     mutate, and the plain counters.
//   - renderMu serialises every render, requestRender, timer tick and key
//     handler that can call back into the host. It is acquired *outside* mu so
//     a render can read the state and then call setWidget/requestRender
//     without any view of these locks nesting the host call under mu (see
//     README "并发与锁纪律"). Never call a host SDK method while holding mu.
type runtimeState struct {
	mu sync.Mutex
	// renderMu is the render/keypress serialisation lock described above.
	renderMu sync.Mutex

	// currentTheme is the host palette captured at session_start.
	currentTheme theme
	// clock is the wall-clock seam; nil reads time.Now.
	clock clockFn
	// maxGroupEntries is the loaded configuration (config.maxGroupEntries).
	maxGroupEntries int
	// config is the in-memory configuration (config of L790).
	config compactConfig
	// spinnerStartMs is the module-level `spinnerStart` of L62.
	spinnerStartMs int64
	// groupSeq seeds group ids (the Date.now()+random suffix of L812).
	groupSeq int
	// configPath is where /compact-ui-config saves.
	configPath string
	// acceptingEvents reports whether the host still delivers events.
	acceptingEvents bool
	// groupingInstalled / thinkingSuppressionInstalled / compactionStyleInstalled
	// record the three install steps of the TS factory (L2210-2212).
	groupingInstalled            bool
	thinkingSuppressionInstalled bool
	compactionStyleInstalled     bool

	// Unattached / active thinking bookkeeping.
	activeThinking           *thinkingEntry
	activeThinkingIndex      *int
	activeThinkingBlocks     map[int]string
	activeThinkingBlockOrder []thinkingBlock
	unattachedThinking       []*thinkingEntry
	thinkingEntrySeq         int
	noticeEntrySeq           int
	handledTextIndexes       map[int]bool
	sealedThinkingIndexes    map[int]bool
	pendingTextSeal          bool
	pendingTextOrdinal       *int
	lastSealedThinkingEntry  *thinkingEntry
	lastReportedReasoning    int
	lastActiveGroup          *toolGroupComponent
	lastStreamingComp        *assistantMessageComponent
	lastChatContainer        *container

	// Tool timing.
	toolStarts            map[string]int64
	toolDurations         map[string]float64
	interruptedToolCallID map[string]bool
	turnStartMs           int64
	agentWorkPhase        *string
	preparingByIndex      map[int]*preparingTool

	// Overlay bookkeeping.
	activeModalHandle     overlayCloser
	activeModalComponent  *detailModalComponent
	activeModalCloseFn    func(any)
	activeUIContext       uiContext
	activeSelectorHandle  overlayCloser
	activeSelectorComp    *inspectSelectorModal
	activeSelectorCloseFn func(any)
	// activeCustomRoot is the single ctx.ui.Custom root both modals share when
	// the host exposes no TUI handle: Enter switches its view instead of stacking
	// a second Custom session.
	activeCustomRoot *customRootComponent
	capturedTui      tuiHandle
	animTimer        *animTimer
	// animTicks holds the completion channel of every animation tick that has
	// already fired and is still executing. arming r.animTimer alone is not
	// enough for stopAnimation to wait: the tick clears r.animTimer as soon as it
	// fires and only then calls the host, so the in-flight channel is what keeps
	// stopAnimation from returning while a host call is still on the way.
	animTicks []chan struct{}
	groups    map[*toolGroupComponent]bool
	// groupOrder is the insertion order of groups. TS keeps runtime.groups in a
	// Set, which iterates in insertion order; a Go map does not, so every
	// traversal that carries meaning goes through groupsListLocked instead.
	groupOrder             []*toolGroupComponent
	assistantContentCont   map[*container]bool
	assistantContentStates map[*container]*assistantContentState
	groupAnchors           map[*toolGroupComponent]*groupAnchor
	// toolCardInvalidate holds the host Invalidate callback for each tool card
	// the ToolRenderer has drawn. When a tool joins a group, sibling cards are
	// invalidated so only the group's display tool keeps painting.
	toolCardInvalidate map[string]func()
}

// createRuntimeState is createRuntimeState (L9-46).
func createRuntimeState() *runtimeState {
	return &runtimeState{
		activeThinkingBlocks:   map[int]string{},
		unattachedThinking:     []*thinkingEntry{},
		handledTextIndexes:     map[int]bool{},
		sealedThinkingIndexes:  map[int]bool{},
		toolStarts:             map[string]int64{},
		toolDurations:          map[string]float64{},
		interruptedToolCallID:  map[string]bool{},
		preparingByIndex:       map[int]*preparingTool{},
		groups:                 map[*toolGroupComponent]bool{},
		groupOrder:             []*toolGroupComponent{},
		assistantContentCont:   map[*container]bool{},
		assistantContentStates: map[*container]*assistantContentState{},
		toolCardInvalidate:     map[string]func(){},
		groupAnchors:           map[*toolGroupComponent]*groupAnchor{},
	}
}

// addGroupLocked registers a group, keeping the insertion order the TS Set
// gives runtime.groups. The caller holds mu.
func (r *runtimeState) addGroupLocked(group *toolGroupComponent) {
	if group == nil || r.groups[group] {
		return
	}
	r.groups[group] = true
	r.groupOrder = append(r.groupOrder, group)
}

// deleteGroupLocked drops a group from both the membership map and the order.
// The caller holds mu.
func (r *runtimeState) deleteGroupLocked(group *toolGroupComponent) {
	if group == nil || !r.groups[group] {
		return
	}
	delete(r.groups, group)
	for i, existing := range r.groupOrder {
		if existing == group {
			r.groupOrder = append(r.groupOrder[:i], r.groupOrder[i+1:]...)
			break
		}
	}
}

// groupsListLocked returns the live groups in insertion order. The caller
// holds mu.
func (r *runtimeState) groupsListLocked() []*toolGroupComponent {
	out := make([]*toolGroupComponent, 0, len(r.groups))
	for _, group := range r.groupOrder {
		if r.groups[group] {
			out = append(out, group)
		}
	}
	return out
}

// resetGroupsLocked empties the group set, membership and order alike. The
// caller holds mu.
func (r *runtimeState) resetGroupsLocked() {
	r.groups = map[*toolGroupComponent]bool{}
	r.groupOrder = nil
}

// assistantContentState is the per-container patch state (L345 /
// getAssistantContentState): the historically staged thinking groups keyed by
// the ordinal of the visible text block they precede, the ordinal the next
// patched addChild consumed, and the turn divider placed before the final text
// block.
type assistantContentState struct {
	anchors         map[int]*toolGroupComponent
	nextTextOrdinal int
	finalDivider    *dividerPlacement
}

// dividerPlacement is `state.finalDivider` (L380).
type dividerPlacement struct {
	ordinal   int
	component *turnDividerComponent
}

// groupAnchor is the value of runtime.groupAnchors (L357).
type groupAnchor struct {
	container *container
	ordinal   int
}

// thinkingEntry is one thinking run. `owner` is the group that took the run
// (L168, L1146, L1183): null/absent stays a nil owner, which the TS code
// distinguishes from a foreign owner.
type thinkingEntry struct {
	kind        string // "thinking"
	id          int
	text        string
	tokens      int
	tokensExact bool
	active      bool
	owner       *toolGroupComponent
}

// toolEntry is one tool call inside a group (L168, L844): the tool component
// itself, plus the group bookkeeping fields PT puts on it.
type toolEntry struct {
	owner *toolGroupComponent
}

// noticeEntry is an absorbed `cache miss` notice (L1476).
type noticeEntry struct {
	kind       string // "notice"
	id         int
	noticeType string
	text       string
	owner      *toolGroupComponent
}

// preparingTool is the provisional entry PT creates when a tool call starts
// streaming (L730): it renders like a tool call but is not one yet.
//
// comp is the toolComponent view the group entry carries. TS hangs the
// provisional object itself on `{kind: "tool", tool: preparing}`, so every
// renderer reads a real tool object; the port keeps that shape by mounting a
// toolComponent with preparing/isPartial/receivingArgs set, so tool_group.go
// and modals.go never dereference a nil entry.tool while arguments stream in.
type preparingTool struct {
	toolName      string
	toolCallID    string
	args          map[string]any
	isPartial     bool
	preparing     bool
	receivingArgs bool
	contentIndex  int
	hasIndex      bool
	owner         *toolGroupComponent
	comp          *toolComponent
	// render state shared with the real tool it is absorbed into.
	groupedAt   *int64
	groupEndAt  *int64
	interrupted bool
	elapsedDur  *float64
}

// thinkingBlock is one indexed thinking block of the streaming message, kept
// in arrival order so the joined text matches the TS Map's insertion order.
type thinkingBlock struct {
	index int
	text  string
}

// reset clears the per-turn bookkeeping (message_start role user, agent_end).
func (r *runtimeState) resetTurnLocked() {
	r.activeThinking = nil
	r.activeThinkingIndex = nil
	r.activeThinkingBlocks = map[int]string{}
	r.unattachedThinking = nil
	r.lastSealedThinkingEntry = nil
	r.lastReportedReasoning = 0
	r.handledTextIndexes = map[int]bool{}
	r.sealedThinkingIndexes = map[int]bool{}
	r.pendingTextSeal = false
	r.pendingTextOrdinal = nil
}

// messageStartReset clears what an assistant message_start clears (L2404-2413).
func (r *runtimeState) assistantMessageStartLocked() {
	r.handledTextIndexes = map[int]bool{}
	r.sealedThinkingIndexes = map[int]bool{}
	r.lastSealedThinkingEntry = nil
	r.lastReportedReasoning = 0
	r.pendingTextSeal = false
	r.pendingTextOrdinal = nil
	r.lastStreamingComp = nil
	// A new assistant message starts a new stream, so a content index no longer
	// identifies a call of the previous message: the id is the only stable key
	// across messages (findGroupedTool only falls back to the index while a
	// stream has no id yet). Keeping the index match alive let a first frame
	// whose stream carries no id yet find the previous message's completed call
	// in the same slot and overwrite its arguments. A still-provisional entry
	// keeps its index: it belongs to the message that is still arriving.
	for _, group := range r.groupsListLocked() {
		if group.sealed {
			continue
		}
		for _, entry := range group.entries {
			if entry.kind == "tool" && entry.tool != nil && !entry.tool.preparing {
				entry.tool.hasIndex = false
			}
		}
	}
}
