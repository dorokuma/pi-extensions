package compactui

import (
	"sync"
	"time"
)

// This file holds the runtime's render/timing/overlay plumbing: the pieces the
// TS original gets from the host (requestRender, setInterval) or keeps on the
// module-level runtime object (capturedTui, activeModalHandle). They are
// collected here so every host-facing call has a single, auditable site -- and
// so the "never call the host under mu" rule can be checked by reading one
// file.

// ticker is a single-consumer interval whose stop closes the channel.
type ticker struct {
	c     chan struct{}
	stopC chan struct{}
	once  *onceCloser
}

// onceCloser closes a channel exactly once.
type onceCloser struct{ ch chan struct{} }

// newTicker starts an interval of intervalMs.
func newTicker(intervalMs int) *ticker {
	t := &ticker{c: make(chan struct{}), stopC: make(chan struct{}), once: &onceCloser{ch: make(chan struct{})}}
	go func() {
		timer := time.NewTimer(time.Duration(intervalMs) * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-t.stopC:
				return
			case <-timer.C:
			}
			select {
			case t.c <- struct{}{}:
			case <-t.stopC:
				return
			}
			timer.Reset(time.Duration(intervalMs) * time.Millisecond)
		}
	}()
	return t
}

// stop ends the interval.
func (t *ticker) stop() {
	if t == nil {
		return
	}
	select {
	case <-t.once.ch:
		return
	default:
	}
	close(t.once.ch)
	close(t.stopC)
}

// requestFrame asks the captured TUI for a frame. Callers must not hold mu.
func (r *runtimeState) requestFrame() {
	r.mu.Lock()
	tui := r.capturedTui
	r.mu.Unlock()
	requestRender(tui)
}

// themeOf returns the current palette. Callers must not hold mu.
func (r *runtimeState) themeOf() theme {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentTheme
}

// terminalRows is `runtime.capturedTui?.terminal?.rows ?? process.stdout?.rows ?? 24`
// (L1657). The captured TUI answers first; without one the port falls back to
// the conventional 24 rows.
func (r *runtimeState) terminalRows() int {
	r.mu.Lock()
	tui := r.capturedTui
	r.mu.Unlock()
	if tui != nil {
		if rows := tui.terminalRows(); rows > 0 {
			return rows
		}
	}
	return 24
}

// scheduleAnimation is scheduleAnimation (L769-785): one pending tick that
// keeps animating groups moving. The tick invalidates the host display tool
// card (or falls back to requestRender when none is registered) and re-arms
// itself through the next render, so a group that stops animating ends the
// chain. It deliberately avoids a blanket requestRender when a card hook
// exists: that full redraw races the host Working spinner (see the tick body).
func (r *runtimeState) scheduleAnimation() {
	r.mu.Lock()
	if r.animTimer != nil {
		r.mu.Unlock()
		return
	}
	timer := &animTimer{stop: make(chan struct{}), done: make(chan struct{})}
	// Registering the in-flight channel under the same lock that arms the timer
	// is what makes stopAnimation's wait exact: a stop that observes the armed
	// timer is guaranteed to observe this channel too.
	inflight := make(chan struct{})
	r.animTimer = timer
	r.animTicks = append(r.animTicks, inflight)
	delay := r.animationDelayLocked()
	r.mu.Unlock()
	go func() {
		// The tick is the only owner of its channel: it detaches the channel
		// from the global registry under mu and closes it afterwards. The
		// order matters. Every stopAnimation that entered before the detach
		// already holds a snapshot containing this channel and waits for the
		// close; one that enters after the detach can only do so because the
		// host call has already returned, so returning immediately is exact.
		// Closing first, or letting a caller clear the registry, would let a
		// second concurrent stopAnimation return while this tick is still
		// blocked in the host.
		defer func() {
			r.mu.Lock()
			for i, channel := range r.animTicks {
				if channel == inflight {
					r.animTicks = append(r.animTicks[:i:i], r.animTicks[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
			close(inflight)
		}()
		defer close(timer.done)
		select {
		case <-timer.stop:
			return
		case <-time.After(time.Duration(delay) * time.Millisecond):
		}
		r.mu.Lock()
		if r.animTimer == timer {
			r.animTimer = nil
		}
		// Prefer host tool-card Invalidate over capturedTui.requestRender.
		// A full requestRender races the host Working StatusIndicator's ~80ms
		// tick (interactive_layout.go tickStatusIndicators -> tui.Render) and
		// forces another walk of the conversation; that is what makes the
		// editor-border "Working" spinner stutter when compact-ui is on.
		// Invalidating only the display tool card marks it dirty so the host's
		// next (already scheduled) frame rebuilds just that card. Fall back to
		// requestRender only when no card Invalidate hook is registered (tests,
		// or a thinking-only group with no host tool card yet).
		groups := r.groupsListLocked()
		tui := r.capturedTui
		var invalidates []func()
		any := false
		for _, group := range groups {
			if !group.needsAnimationLocked() {
				continue
			}
			any = true
			group.invalidateLocked()
			// The display tool is the newest visible entry, which while a call
			// streams is still only a group entry (nothing mounted it in
			// children yet). Selecting the last mounted child instead would
			// leave the streaming entry's card -- the one that paints the tree
			// -- without a tick, and invalidate a card that no longer paints.
			displayID := group.displayCardIDLocked()
			if displayID == "" {
				continue
			}
			if fn := r.toolCardInvalidate[displayID]; fn != nil {
				invalidates = append(invalidates, fn)
			}
		}
		r.mu.Unlock()
		for _, fn := range invalidates {
			fn()
		}
		if any && len(invalidates) == 0 {
			requestRender(tui)
		}
	}()
}

// stopAnimation is the `clearTimeout(runtime.animTimer)` of session_shutdown
// and sealHistoricalGroups: it cancels the pending tick and waits for every
// tick that already fired, so no goroutine outlives the runtime holding its
// lock. Waiting covers the in-flight channels as well as the timer's own done:
// a tick that has fired has already cleared r.animTimer, so without the
// in-flight wait stopAnimation would return while that tick's requestRender
// was still running.
//
// The wait only snapshots r.animTicks and never clears it, because two callers
// can race here (the asynchronous seal of sealHistoricalGroupsFrom overlapping
// handleSessionShutdown). A stop that emptied r.animTicks would let the loser
// observe no timer and no ticks and return immediately, while the tick was
// still blocked in the host -- breaking the contract that no stopAnimation
// returns before every in-flight tick is done. With the snapshot, a caller
// that entered before the tick detached its channel waits for that close,
// while one entering after it finds an empty registry only because the host
// call is already over.
func (r *runtimeState) stopAnimation() {
	r.mu.Lock()
	timer := r.animTimer
	r.animTimer = nil
	inflight := append([]chan struct{}(nil), r.animTicks...)
	r.mu.Unlock()
	timer.stopTimer()
	if timer != nil {
		<-timer.done
	}
	for _, channel := range inflight {
		<-channel
	}
}

// ── overlay lifecycle (L2611-2810 / L1815-2070) ──────────────────────────────

// openDetailModal is openDetailModal (L1801-1830): close the previous detail
// modal, then show the selected entry's details.
func (r *runtimeState) openDetailModal(entry groupEntry) {
	r.closeDetailModal()
	modal := r.newDetailModal(entry)
	r.mu.Lock()
	root := r.activeCustomRoot
	r.activeModalComponent = modal
	tui := r.capturedTui
	ui := r.activeUIContext
	r.mu.Unlock()
	if root != nil {
		// The selector opened a single ctx.ui.Custom root; Enter switches that
		// root's view instead of stacking a second Custom session.
		root.show(modal)
		requestRender(tui)
		return
	}
	if tui != nil {
		handle := tui.addOverlay(modal, overlayWidthPercent85, overlayMaxHeightPercent80)
		r.mu.Lock()
		r.activeModalHandle = handle
		r.mu.Unlock()
		requestRender(tui)
		return
	}
	if ui != nil && ui.hasUI() {
		r.openCustomOverlay(modal, overlayWidthPercent85, overlayMaxHeightPercent80, func(handle overlayCloser) {
			r.mu.Lock()
			r.activeModalHandle = handle
			r.mu.Unlock()
		}, true)
	}
}

// closeDetailModal is closeDetailModal (L1831-1855).
func (r *runtimeState) closeDetailModal() {
	r.mu.Lock()
	modal := r.activeModalComponent
	r.activeModalComponent = nil
	handle := r.activeModalHandle
	r.activeModalHandle = nil
	root := r.activeCustomRoot
	done := r.activeModalCloseFn
	r.activeModalCloseFn = nil
	tui := r.capturedTui
	r.mu.Unlock()
	if modal != nil {
		modal.dispose()
	}
	if handle != nil {
		handle.hide()
	}
	if root != nil {
		// Closing the detail falls back to the selector that owns the root.
		root.showSelector()
		requestRender(tui)
		return
	}
	if done != nil {
		done(nil)
	}
	requestRender(tui)
}

// openInspectSelectorModal is openInspectSelectorModal (L2071-2104).
func (r *runtimeState) openInspectSelectorModal() {
	r.mu.Lock()
	hasModal := r.activeModalHandle != nil
	r.mu.Unlock()
	if hasModal {
		r.closeDetailModal()
	}
	r.closeInspectSelectorModal()
	modal := r.newInspectSelectorModal()
	r.mu.Lock()
	r.activeSelectorComp = modal
	tui := r.capturedTui
	ui := r.activeUIContext
	r.mu.Unlock()
	if tui != nil {
		handle := tui.addOverlay(modal, overlayWidthPercent85, overlayMaxHeightPercent70)
		r.mu.Lock()
		r.activeSelectorHandle = handle
		r.mu.Unlock()
		requestRender(tui)
		return
	}
	if ui != nil && ui.hasUI() {
		root := newCustomRoot(r, modal)
		r.openCustomOverlayRoot(root, modal, overlayWidthPercent85, overlayMaxHeightPercent70)
	}
}

// closeInspectSelectorModal is closeInspectSelectorModal (L2105-2122).
func (r *runtimeState) closeInspectSelectorModal() {
	r.mu.Lock()
	handle := r.activeSelectorHandle
	r.activeSelectorHandle = nil
	done := r.activeSelectorCloseFn
	r.activeSelectorCloseFn = nil
	modal := r.activeSelectorComp
	r.activeSelectorComp = nil
	root := r.activeCustomRoot
	r.activeCustomRoot = nil
	tui := r.capturedTui
	r.mu.Unlock()
	if handle != nil {
		handle.hide()
	}
	if done != nil {
		done(nil)
	}
	if modal != nil {
		modal.dispose()
	}
	if root != nil {
		root.dispose()
	}
	requestRender(tui)
}

// overlay geometry, shared by both modals (L1812, L1815, L2082).
const (
	overlayWidthPercent85     = 85
	overlayMaxHeightPercent80 = 80
	overlayMaxHeightPercent70 = 70
)

// openCustomOverlay is the ctx.ui.custom branch of openDetailModal /
// openInspectSelectorModal: it opens the component as a focused overlay and
// reports the handle once the host mounted it. The ui.custom call happens
// outside mu and must not block the caller (see the go statement below).
func (r *runtimeState) openCustomOverlay(comp remoteComponent, widthPercent, maxHeightPercent int, onHandle func(overlayCloser), isDetail bool) {
	r.mu.Lock()
	ui := r.activeUIContext
	r.mu.Unlock()
	if ui == nil {
		return
	}
	var closeFn func(any)
	factory := func(tui tuiHandle) remoteComponent {
		r.mu.Lock()
		r.capturedTui = tui
		if isDetail {
			r.activeModalCloseFn = closeFn
		} else {
			r.activeSelectorCloseFn = closeFn
		}
		r.mu.Unlock()
		return comp
	}
	closeFn = func(any) {}
	options := customOverlayOptions{overlay: true, anchor: "center", widthPercent: widthPercent, maxHeightPercent: maxHeightPercent, onHandle: onHandle}
	// Fire-and-forget like TS's un-awaited ctx.ui.custom(...). A synchronous
	// wait here from OnTerminalInput deadlocks the host input ticket: the pump
	// waits for the terminal_input verdict, Custom waits for Done, and keys
	// never reach the overlay (see report2.md).
	go func() {
		defer func() { _ = recover() }()
		_, _ = ui.custom(factory, options)
	}()
}

// openCustomOverlayRoot opens one ctx.ui.Custom root that owns both modal views.
// Enter on the selector switches the root's view, so the host never carries two
// nested Custom sessions.
func (r *runtimeState) openCustomOverlayRoot(root *customRootComponent, selector remoteComponent, widthPercent, maxHeightPercent int) {
	r.mu.Lock()
	ui := r.activeUIContext
	r.activeCustomRoot = root
	r.mu.Unlock()
	if ui == nil {
		return
	}
	var closeFn func(any)
	factory := func(tui tuiHandle) remoteComponent {
		r.mu.Lock()
		r.capturedTui = tui
		r.activeSelectorCloseFn = closeFn
		r.mu.Unlock()
		return root
	}
	closeFn = func(any) {}
	options := customOverlayOptions{overlay: true, anchor: "center", widthPercent: widthPercent, maxHeightPercent: maxHeightPercent, onHandle: func(handle overlayCloser) {
		r.mu.Lock()
		r.activeSelectorHandle = handle
		r.mu.Unlock()
	}}
	rootRef := root
	// Same fire-and-forget contract as openCustomOverlay / TS openInspectSelectorModal.
	go func() {
		defer func() { _ = recover() }()
		if _, err := ui.custom(factory, options); err != nil {
			r.mu.Lock()
			if r.activeCustomRoot == rootRef {
				r.activeCustomRoot = nil
			}
			r.mu.Unlock()
		}
	}()
}

// customRootComponent is the single ctx.ui.Custom root the two modals share when
// the host exposes no TUI handle (capturedTui == nil). It renders whichever view
// is active and routes input to it: a closing detail falls back to the selector,
// and only the selector's own Esc/q/Q ends the root.
type customRootComponent struct {
	tui      *runtimeState
	selector *inspectSelectorModal

	mu    sync.Mutex
	view  remoteComponent
	depth int
}

func newCustomRoot(tui *runtimeState, selector *inspectSelectorModal) *customRootComponent {
	root := &customRootComponent{tui: tui, selector: selector, view: selector}
	return root
}

// show switches the root's active view.
func (c *customRootComponent) show(view remoteComponent) {
	c.mu.Lock()
	c.view = view
	c.depth = 1
	c.mu.Unlock()
}

// showSelector returns the root to the selector view.
func (c *customRootComponent) showSelector() {
	c.mu.Lock()
	c.view = c.selector
	c.depth = 0
	c.mu.Unlock()
}

// current is the active view.
func (c *customRootComponent) current() remoteComponent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

// Render implements remoteComponent.
func (c *customRootComponent) Render(width int) []string {
	if view := c.current(); view != nil {
		return view.Render(width)
	}
	return nil
}

// HandleInput implements remoteComponent. A view that reports "closed" either
// ends the root (the selector's Esc/q/Q) or falls back to the selector (the
// detail's Esc/q/Q), so a navigation key never dismisses the overlay.
func (c *customRootComponent) HandleInput(data string) (bool, any) {
	view := c.current()
	if view == nil {
		return false, nil
	}
	c.mu.Lock()
	isSelector := c.depth == 0
	c.mu.Unlock()
	done, value := view.HandleInput(data)
	if !done {
		return false, value
	}
	if isSelector {
		c.tui.closeInspectSelectorModal()
		return true, value
	}
	c.showSelector()
	return false, value
}

// dispose releases the root's views.
func (c *customRootComponent) dispose() {
	c.mu.Lock()
	view := c.view
	c.view = nil
	c.depth = 0
	c.mu.Unlock()
	if disposer, ok := view.(interface{ dispose() }); ok {
		disposer.dispose()
	}
}

// customOverlayOptions is the serializable overlay request the port hands the
// uiContext seam (PT passes `{ overlay: true, overlayOptions: {...}, onHandle }`).
type customOverlayOptions struct {
	overlay          bool
	anchor           string
	widthPercent     int
	maxHeightPercent int
	onHandle         func(overlayCloser)
}
