package compactui

import (
	"strconv"
	"strings"
	"sync"
)

// This file holds the locking accessors the renderers use. The TS original is
// single-threaded and reads `runtime.<field>` directly; the port routes every
// read through one of these helpers so the same code compiles under -race.
//
// Lock discipline (also in README "并发与锁纪律"):
//   - Every *Locked helper assumes the caller holds r.mu (read or write).
//   - The non-Locked helpers take r.mu themselves for the duration of the read
//     and must not be called from inside a critical section.
//   - No host SDK call (SetWidgetRenderer, OnTerminalInput, Notify, Custom,
//     UITheme, ...) ever runs while r.mu is held: render paths copy what they
//     need under mu, release it, and only then format and hand text to the
//     host.

// nowMs is the clock seam: production reads the wall clock, tests replace it.
func (r *runtimeState) nowMs() int64 {
	r.mu.Lock()
	clock := r.clock
	r.mu.Unlock()
	if clock == nil {
		return wallClock()
	}
	return clock()
}

// nowMsLocked is nowMs for a caller that already holds mu (read or write). It
// never takes the lock: a render runs under mu and must not re-enter it.
func (r *runtimeState) nowMsLocked() int64 {
	if r.clock == nil {
		return wallClock()
	}
	return r.clock()
}

// setThinkingBlock records the indexed thinking block text of the streaming
// message: `runtime.activeThinkingBlocks.set(index, text)` (L2262), keeping the
// Map's insertion order for the join in updateActiveThinkingTokens.
func (r *runtimeState) setThinkingBlock(index int, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, block := range r.activeThinkingBlockOrder {
		if block.index == index {
			r.activeThinkingBlockOrder[i].text = text
			return
		}
	}
	r.activeThinkingBlockOrder = append(r.activeThinkingBlockOrder, thinkingBlock{index: index, text: text})
}

// clockFn is the injected clock.
type clockFn func() int64

func (r *runtimeState) themeLocked() theme {
	return r.currentTheme
}

func (r *runtimeState) agentWorkPhaseLocked() *string {
	if r.agentWorkPhase == nil {
		return nil
	}
	phase := *r.agentWorkPhase
	return &phase
}

func (r *runtimeState) lastActiveGroupLocked() *toolGroupComponent { return r.lastActiveGroup }

func (r *runtimeState) maxGroupEntriesLocked() int {
	if r.maxGroupEntries <= 0 {
		return defaultMaxGroupEntries
	}
	return r.maxGroupEntries
}

// hasDurationLocked is `runtime.toolDurations.has(id)`.
func (r *runtimeState) hasDurationLocked(id string) bool {
	_, present := r.toolDurations[id]
	return present
}

// rememberToolStartLocked is rememberToolStart (L560-563).
func (r *runtimeState) rememberToolStartLocked(toolCallID string, at int64) {
	if toolCallID == "" {
		return
	}
	if _, present := r.toolStarts[toolCallID]; present {
		return
	}
	if _, present := r.toolDurations[toolCallID]; present {
		return
	}
	r.toolStarts[toolCallID] = at
}

// rememberToolStart is rememberToolStart without the caller's lock.
func (r *runtimeState) rememberToolStart(toolCallID string, at int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rememberToolStartLocked(toolCallID, at)
}

// animTimer is the single scheduled animation tick (L770). It carries its own
// stop channel so session_shutdown can cancel a pending tick without leaking a
// goroutine (the TS clearTimeout equivalent).
type animTimer struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// stopTimer cancels a pending tick.
func (t *animTimer) stopTimer() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
}

// tuiHandle is the captured TUI: the port only needs requestRender, the
// terminal height and the overlay opener. TS stores the host object
// (runtime.capturedTui); the port stores this narrow interface, never a host
// object, so a render can run without holding a host handle.
type tuiHandle interface {
	requestRender()
	terminalRows() int
	addOverlay(comp remoteComponent, widthPercent, maxHeightPercent int) overlayCloser
}

// overlayCloser is the overlay handle PT keeps (activeModalHandle /
// activeSelectorHandle): hide() dismisses the overlay.
type overlayCloser interface {
	hide()
}

// uiContext is the host UI surface the modals need, mirroring the parts of
// ctx.ui PT uses. It is an interface so a test can drive the modal lifecycle
// without a live SDK connection, and so no host callback is stored as an
// sdk.Context whose zero value would hide a nil dereference.
type uiContext interface {
	setHiddenThinkingLabel(label string)
	onTerminalInput(handler func(data string) terminalInputVerdict) func()
	setWidgetRenderer(key string, render func(width int) []string)
	notify(message, level string)
	hasUI() bool
	custom(factory remoteComponentFactory, options any) (any, error)
}

// terminalInputVerdict is the handler verdict: `{ consume?: boolean }`
// (TerminalInputResult).
type terminalInputVerdict struct {
	consume bool
}

// remoteComponentFactory builds the component a custom overlay shows.
type remoteComponentFactory func(tui tuiHandle) remoteComponent

// remoteComponent is a renderable overlay component with input handling.
type remoteComponent interface {
	Render(width int) []string
	HandleInput(data string) (done bool, value any)
}

// ── small JS-compatible helpers ─────────────────────────────────────────────

// jsString is String(value) for the JSON shapes PT reads.
func jsString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

// argsString is `args?.key || fallback`, the accessor toolSummary uses.
func argsString(args map[string]any, key, fallback string) string {
	if args == nil {
		return fallback
	}
	value, present := args[key]
	if !present || value == nil {
		return fallback
	}
	text := jsString(value)
	if text == "" {
		return fallback
	}
	return text
}

// utf16Len is the JavaScript string length (UTF-16 code units).
func utf16Len(text string) int {
	n := 0
	for _, r := range text {
		if r > 0xFFFF {
			n += 2
			continue
		}
		n++
	}
	return n
}

// utf16Slice is text.slice(start, end) over UTF-16 code units.
func utf16Slice(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	var b strings.Builder
	pos := 0
	for _, r := range text {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if pos >= end {
			break
		}
		if pos >= start && pos+width <= end {
			b.WriteRune(r)
		} else if pos >= start && pos+width > end {
			// A surrogate pair straddling the cut is not emitted, as JS slicing
			// a lone half yields an unpaired surrogate the port cannot encode.
			break
		}
		pos += width
	}
	return b.String()
}
