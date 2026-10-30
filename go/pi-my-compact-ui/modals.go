package compactui

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports src/modals.ts (L1563-2080): the detail modal and the
// inspect selector.
//
// Both modals are RemoteComponents in the PiG sense: the host opens a focused
// overlay and routes keystrokes to HandleInput, and the extension sends the
// rendered lines back. The port keeps PT's render/scroll/key behaviour
// verbatim and replaces the pi-tui OverlayHandle with overlayCloser (an
// interface whose hide() closes the overlay) plus the uiContext seam, so the
// whole lifecycle is drivable in a unit test.

// detailModalComponent is DetailModalComponent (L1563-1810).
type detailModalComponent struct {
	tui   *runtimeState
	entry groupEntry

	mu               sync.Mutex
	scrollTop        int
	followingEnd     bool
	lastContentLines int
	lastInnerHeight  int
	pollTimer        *animTimer
	polling          bool
	disposed         bool
}

// newDetailModal is `new DetailModalComponent(entry)` (L1570-1573).
func (r *runtimeState) newDetailModal(entry groupEntry) *detailModalComponent {
	modal := &detailModalComponent{tui: r, entry: entry, followingEnd: true, lastInnerHeight: 10}
	modal.startPollingIfNeeded()
	return modal
}

// isEntryActive is isEntryActive (L1574-1582): a thinking run that still
// streams or a tool call that is still pending.
func (m *detailModalComponent) isEntryActive() bool {
	m.mu.Lock()
	entry := m.entry
	tui := m.tui
	m.mu.Unlock()
	if tui == nil {
		return false
	}
	// The poll loop runs on its own goroutine while event handlers finalize the
	// thinking run and apply tool results, so the live state is read through the
	// runtime lock -- never unlocked, straight off the entry.
	return tui.isModalEntryActive(entry)
}

// isModalEntryActive is isEntryActive for a caller outside the modal lock: it
// reads the live fields (think.active, tool.owner, the result/isPartial state
// toolStatus decides on) as one snapshot under mu. The caller must not hold mu.
func (r *runtimeState) isModalEntryActive(entry groupEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch entry.kind {
	case "thinking":
		return entry.think != nil && entry.think.active
	case "tool":
		if entry.tool == nil {
			return false
		}
		return toolStatus(entry.tool, entry.tool.owner) == "pending"
	}
	return false
}

// startPollingIfNeeded is startPollingIfNeeded (L1583-1592): re-render every
// spinner interval while the entry is live, and stop once it is not.
func (m *detailModalComponent) startPollingIfNeeded() {
	if !m.isEntryActive() {
		return
	}
	interval := spinnerMs
	if m.entry.kind == "thinking" {
		interval = thinkingSpinnerIntervalMs
	}
	timer := &animTimer{stop: make(chan struct{}), done: make(chan struct{})}
	m.mu.Lock()
	m.pollTimer = timer
	m.polling = true
	m.mu.Unlock()
	go func() {
		<-timer.stop
		close(timer.done)
	}()
	go m.pollLoop(timer, interval)
}

// pollLoop is the setInterval body of L1585-1591.
func (m *detailModalComponent) pollLoop(timer *animTimer, intervalMs int) {
	ticker := newTicker(intervalMs)
	defer ticker.stop()
	for {
		select {
		case <-timer.stop:
			return
		case <-ticker.c:
			m.tui.requestFrame()
			if !m.isEntryActive() {
				m.mu.Lock()
				if m.pollTimer == timer {
					m.pollTimer = nil
					m.polling = false
				}
				m.mu.Unlock()
				timer.stopTimer()
				return
			}
		}
	}
}

// dispose is dispose (L1593-1599).
func (m *detailModalComponent) dispose() {
	m.mu.Lock()
	timer := m.pollTimer
	m.pollTimer = nil
	m.polling = false
	m.disposed = true
	m.mu.Unlock()
	timer.stopTimer()
}

// invalidate is invalidate (L1600-1601).
func (m *detailModalComponent) invalidate() {}

// scrollBy is scrollBy (L1602-1604).
func (m *detailModalComponent) scrollBy(delta int) {
	m.mu.Lock()
	next := m.scrollTop + delta
	m.mu.Unlock()
	m.scrollTo(next)
}

// scrollTo is scrollTo (L1605-1614).
func (m *detailModalComponent) scrollTo(newTop int) {
	m.mu.Lock()
	maxScroll := maxInt(0, m.lastContentLines-m.lastInnerHeight)
	if newTop < 0 {
		newTop = 0
	}
	if newTop > maxScroll {
		newTop = maxScroll
	}
	m.scrollTop = newTop
	m.followingEnd = m.scrollTop >= maxScroll
	m.mu.Unlock()
	m.tui.requestFrame()
}

// scrollToEnd is scrollToEnd (L1615-1619).
func (m *detailModalComponent) scrollToEnd() {
	m.mu.Lock()
	m.followingEnd = true
	m.scrollTop = maxInt(0, m.lastContentLines-m.lastInnerHeight)
	m.mu.Unlock()
	m.tui.requestFrame()
}

// handleInput is handleInput (L1620-1650): Esc/q close, j/k and the arrows
// scroll a line, PageUp/PageDown and the side arrows scroll a page, Home/End
// jump. It reports whether the modal **closed**, not whether the key was
// consumed: the host's RemoteComponentResult{Done: true} ends the overlay, so a
// navigation key must report false or the first arrow press would dismiss it.
func (m *detailModalComponent) handleInput(data string) bool {
	if matchesKey(data, "escape") || data == "q" || data == "Q" {
		m.tui.closeDetailModal()
		return true
	}
	if matchesKey(data, "up") || data == "k" || data == "K" {
		m.scrollBy(-1)
		return false
	}
	if matchesKey(data, "down") || data == "j" || data == "J" {
		m.scrollBy(1)
		return false
	}
	if matchesKey(data, "pageup") || matchesKey(data, "left") {
		m.scrollBy(-10)
		return false
	}
	if matchesKey(data, "pagedown") || matchesKey(data, "right") || matchesKey(data, "space") {
		m.scrollBy(10)
		return false
	}
	if matchesKey(data, "home") {
		m.scrollTo(0)
		return false
	}
	if matchesKey(data, "end") {
		m.scrollToEnd()
		return false
	}
	return false
}

// HandleInput implements remoteComponent: PT's boolean becomes the
// closed-verdict the host expects (Done: true ends the overlay).
func (m *detailModalComponent) HandleInput(data string) (bool, any) {
	return m.handleInput(data), nil
}

// entrySnapshot is an immutable copy of the fields a modal renders, taken
// under rt.mu. The modals hold live *thinkingEntry / *toolComponent pointers
// that message_update and tool_execution_end keep writing, so a render must not
// read them outside the lock: the snapshot is taken inside it and formatted
// outside.
type entrySnapshot struct {
	kind     string
	thinking thinkingSnapshot
	tool     toolSnapshot
	notice   string
}

// thinkingSnapshot is a thinking run's render fields.
type thinkingSnapshot struct {
	text   string
	tokens int
	active bool
}

// toolSnapshot is a tool call's render fields. status and elapsed are computed
// under the same lock, because toolStatus and toolElapsedLocked read the
// runtime state.
type toolSnapshot struct {
	name       string
	args       map[string]any
	status     string
	elapsed    string
	resultText string
	hasResult  bool
}

// snapshotEntry copies one group entry under rt.mu.
func (r *runtimeState) snapshotEntry(entry groupEntry) entrySnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := entrySnapshot{kind: entry.kind}
	switch entry.kind {
	case "thinking":
		if entry.think != nil {
			snap.thinking = thinkingSnapshot{
				text:   entry.think.text,
				tokens: entry.think.tokens,
				active: entry.think.active,
			}
		}
	case "notice":
		if entry.notice != nil {
			snap.notice = entry.notice.text
		}
	default:
		tool := entry.tool
		if tool != nil {
			snap.tool = toolSnapshot{
				name:       tool.toolName,
				args:       copyArgs(tool.args),
				status:     toolStatus(tool, tool.owner),
				elapsed:    r.toolElapsedLocked(tool),
				resultText: toolResultText(tool),
				hasResult:  tool.result != nil,
			}
		}
	}
	return snap
}

// copyArgs shallow-copies an argument map, so formatting it cannot observe a
// concurrent replacement of the map the event handlers write.
func copyArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		out[key] = value
	}
	return out
}

// renderWidth is render (L1651-1800).
func (m *detailModalComponent) renderWidth(width int) []string {
	th := m.tui.themeOf()
	bold := func(text string) string { return boldOf(th, text) }
	borderFg := func(text string) string { return fgOf(th, "borderAccent", fgOf(th, "border", text)) }
	frame := toolSpinnerFrame(m.tui.nowMs(), m.tui.spinnerStartMs)
	termHeight := m.tui.terminalRows()
	maxInnerHeight := maxInt(5, termHeight*8/10-2)
	innerWidth := maxInt(10, width-4)

	m.mu.Lock()
	entry := m.entry
	scrollTop := m.scrollTop
	followingEnd := m.followingEnd
	m.mu.Unlock()
	// Everything the body reads is a value snapshot taken under rt.mu.
	snap := m.tui.snapshotEntry(entry)

	title := ""
	contentLines := make([]string, 0, 16)
	switch snap.kind {
	case "thinking":
		think := snap.thinking
		var spin *spinnerFrame
		if think.active {
			value := thinkingSpinnerFrame(m.tui.nowMs(), m.tui.spinnerStartMs)
			spin = &value
		}
		icon := "✓"
		color := "thinkingText"
		if spin != nil {
			icon = spin.frame
			color = spin.color
		}
		title = fgOf(th, color, icon) + " " + fgOf(th, "toolTitle", "Thinking") + " " + fgOf(th, "muted", thinkingTokenLabel(&thinkingEntry{text: think.text, tokens: think.tokens}))
		rawText := strings.TrimSpace(think.text)
		if rawText == "" {
			contentLines = append(contentLines, fgOf(th, "dim", "(thinking...)"))
		} else {
			for _, rawLine := range strings.Split(rawText, "\n") {
				wrapped := tui.WrapTextWithAnsi(rawLine, innerWidth)
				if len(wrapped) == 0 {
					contentLines = append(contentLines, "")
					continue
				}
				for _, line := range wrapped {
					contentLines = append(contentLines, fgOf(th, "thinkingText", line))
				}
			}
		}
	case "notice":
		icon := fgOf(th, "warning", "!")
		title = icon + " " + fgOf(th, "toolTitle", bold("cache miss"))
		content := noticeContent(snap.notice)
		if content == "" {
			content = snap.notice
		}
		for _, rawLine := range strings.Split(content, "\n") {
			for _, line := range tui.WrapTextWithAnsi(rawLine, innerWidth) {
				contentLines = append(contentLines, fgOf(th, "dim", line))
			}
		}
	default:
		tool := snap.tool
		status := tool.status
		icon := "✓"
		color := "success"
		if status == "pending" {
			icon = frame
			color = "accent"
		} else if status == "error" {
			icon = "✗"
			color = "error"
		}
		elapsed := "(" + tool.elapsed + ")"
		title = fgOf(th, color, icon) + " " + fgOf(th, "toolTitle", bold(tool.name)) + " " + fgOf(th, "muted", elapsed)
		switch {
		case tool.name == "bash" || tool.name == "powershell" || argsString(tool.args, "command", "") != "":
			command := argsString(tool.args, "command", "...")
			for _, line := range tui.WrapTextWithAnsi("$ "+command, innerWidth) {
				contentLines = append(contentLines, fgOf(th, "accent", line))
			}
		case tool.args != nil && argvJoined(tool.args["argv"]) != "":
			for _, line := range tui.WrapTextWithAnsi("$ "+argvJoined(tool.args["argv"]), innerWidth) {
				contentLines = append(contentLines, fgOf(th, "accent", line))
			}
		case tool.args != nil && argsString(tool.args, "path", "") != "":
			contentLines = append(contentLines, fgOf(th, "dim", "Path:")+" "+argsString(tool.args, "path", ""))
		case tool.args != nil:
			if encoded, err := json.MarshalIndent(tool.args, "", "  "); err == nil {
				for _, line := range strings.Split(string(encoded), "\n") {
					for _, wrapped := range tui.WrapTextWithAnsi(line, innerWidth) {
						contentLines = append(contentLines, fgOf(th, "dim", wrapped))
					}
				}
			} else {
				contentLines = append(contentLines, fgOf(th, "dim", argsStringOf(tool.args)))
			}
		}
		contentLines = append(contentLines, "")
		contentLines = append(contentLines, fgOf(th, "dim", "── Output ──"))
		resultText := tool.resultText
		if resultText != "" {
			for _, rawLine := range strings.Split(resultText, "\n") {
				wrapped := tui.WrapTextWithAnsi(rawLine, innerWidth)
				if len(wrapped) == 0 {
					contentLines = append(contentLines, "")
					continue
				}
				for _, line := range wrapped {
					contentLines = append(contentLines, fgOf(th, "toolOutput", line))
				}
			}
		} else if status == "pending" {
			contentLines = append(contentLines, fgOf(th, "dim", "(running, waiting for output...)"))
		} else {
			contentLines = append(contentLines, fgOf(th, "dim", "(no output)"))
		}
	}

	totalLines := len(contentLines)
	innerHeight := minInt(maxInt(totalLines, 5), maxInnerHeight)
	maxScroll := maxInt(0, totalLines-innerHeight)
	if followingEnd {
		scrollTop = maxScroll
	} else {
		scrollTop = minInt(maxInt(0, scrollTop), maxScroll)
	}
	m.mu.Lock()
	m.lastContentLines = totalLines
	m.lastInnerHeight = innerHeight
	m.scrollTop = scrollTop
	m.mu.Unlock()

	closeHint := "[Esc to close]"
	visClose := tui.VisibleWidth(closeHint)
	maxTitleWidth := maxInt(10, width-visClose-12)
	clampedTitle := truncateToWidthEllipsis(title, maxTitleWidth, "...")
	visTitle := tui.VisibleWidth(clampedTitle)
	fillerLen := maxInt(1, width-visTitle-visClose-8)
	topBorder := borderFg("┌─ ") + clampedTitle + borderFg(" "+strings.Repeat("─", fillerLen)+" ") + fgOf(th, "muted", closeHint) + borderFg(" ─┐")
	result := []string{truncateToWidthEllipsis(topBorder, width, "")}
	slice := make([]string, 0, innerHeight)
	for i := scrollTop; i < scrollTop+innerHeight && i < totalLines; i++ {
		if i >= 0 {
			slice = append(slice, contentLines[i])
		}
	}
	for len(slice) < innerHeight {
		slice = append(slice, "")
	}
	showScrollbar := totalLines > innerHeight
	thumbY := -1
	if showScrollbar && maxScroll > 0 {
		thumbY = int(roundHalfUp(float64(scrollTop) / float64(maxScroll) * float64(innerHeight-1)))
	}
	for i := 0; i < innerHeight; i++ {
		line := ""
		if i < len(slice) {
			line = slice[i]
		}
		vis := tui.VisibleWidth(line)
		truncated := line
		if vis > innerWidth {
			truncated = truncateToWidthEllipsis(line, innerWidth, "...")
		}
		pad := maxInt(0, innerWidth-tui.VisibleWidth(truncated))
		rightChar := borderFg("│")
		if showScrollbar && i == thumbY {
			rightChar = fgOf(th, "scrollbarThumb", "█")
		}
		result = append(result, borderFg("│ ")+truncated+strings.Repeat(" ", pad)+" "+rightChar)
	}
	scrollInfo := ""
	if totalLines > innerHeight {
		scrollInfo = " [" + itoa(scrollTop+1) + "-" + itoa(minInt(totalLines, scrollTop+innerHeight)) + "/" + itoa(totalLines) + "] "
	}
	visInfo := tui.VisibleWidth(scrollInfo)
	botFiller := maxInt(1, width-2-visInfo)
	botBorder := borderFg("└"+strings.Repeat("─", botFiller)) + fgOf(th, "dim", scrollInfo) + borderFg("┘")
	result = append(result, truncateToWidthEllipsis(botBorder, width, ""))
	return result
}

// Render implements remoteComponent.
func (m *detailModalComponent) Render(width int) []string { return m.renderWidth(width) }

// argsString is the `String(args)` fallback of L1749.
func (t *toolComponent) argsString() string {
	return argsStringOf(t.args)
}

// argsStringOf is argsString over an argument map, for the render snapshots.
func argsStringOf(args map[string]any) string {
	if args == nil {
		return ""
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// inspectSelectorModal is InspectSelectorModal (L1815-2070).
type inspectSelectorModal struct {
	tui *runtimeState

	mu          sync.Mutex
	entries     []groupEntry
	selectedIdx int
	scrollTop   int
	visibleHgt  int
	pollTimer   *animTimer
	polling     bool
	disposed    bool
}

// newInspectSelectorModal is `new InspectSelectorModal()` (L1821-1823). The
// initial refresh is also what arms the poll loop when the list already
// holds a live entry.
func (r *runtimeState) newInspectSelectorModal() *inspectSelectorModal {
	modal := &inspectSelectorModal{tui: r, visibleHgt: 10}
	modal.refreshEntries(true)
	return modal
}

// hasLiveEntriesLocked reports whether any selector entry still streams or
// runs: the same predicate isModalEntryActive applies to a detail modal's
// single entry, lifted to the selector's whole list. The caller holds m.mu.
func (m *inspectSelectorModal) hasLiveEntriesLocked() bool {
	if m.tui == nil {
		return false
	}
	for _, entry := range m.entries {
		if m.tui.isModalEntryActive(entry) {
			return true
		}
	}
	return false
}

// startPollingIfNeeded is the selector's counterpart of the detail modal's
// startPollingIfNeeded (L1583-1592): while any listed entry is live, re-render
// every spinner interval so the row icons animate without waiting for a key
// event, and stop once every entry finished.
func (m *inspectSelectorModal) startPollingIfNeeded() {
	m.mu.Lock()
	if m.disposed || m.polling || !m.hasLiveEntriesLocked() {
		m.mu.Unlock()
		return
	}
	timer := &animTimer{stop: make(chan struct{}), done: make(chan struct{})}
	m.pollTimer = timer
	m.polling = true
	m.mu.Unlock()
	go func() {
		<-timer.stop
		close(timer.done)
	}()
	go m.pollLoop(timer)
}

// pollLoop is the setInterval body: it re-requests the frame and re-reads the
// entry list each tick, so a tool that starts *after* the selector opened is
// picked up and a drained list stops the loop.
func (m *inspectSelectorModal) pollLoop(timer *animTimer) {
	ticker := newTicker(spinnerMs)
	defer ticker.stop()
	for {
		select {
		case <-timer.stop:
			return
		case <-ticker.c:
			// refreshEntries commits the new list and requests the frame, so a
			// single call drives both halves of the tick.
			m.refreshEntries(false)
			m.mu.Lock()
			live := !m.disposed && m.hasLiveEntriesLocked()
			if !live && m.pollTimer == timer {
				m.pollTimer = nil
				m.polling = false
			}
			m.mu.Unlock()
			if !live {
				timer.stopTimer()
				return
			}
		}
	}
}

// refreshEntries is refreshEntries (L1824-1841): re-read the visible entries,
// keeping the selection in place or trailing the newest. The follow-tail
// decision is made on the current selection at commit time, inside the same
// critical section that writes it, so a key press that arrived while the new
// list was being gathered cannot be overridden by a stale wasAtEnd.
func (m *inspectSelectorModal) refreshEntries(isInitial bool) {
	entries := m.tui.allInspectableEntries()
	m.mu.Lock()
	wasAtEnd := isInitial || len(m.entries) == 0 || m.selectedIdx >= len(m.entries)-1
	m.entries = entries
	if len(m.entries) == 0 {
		m.selectedIdx = 0
		m.scrollTop = 0
	} else {
		if wasAtEnd {
			m.selectedIdx = len(m.entries) - 1
		} else {
			m.selectedIdx = maxInt(0, minInt(m.selectedIdx, len(m.entries)-1))
		}
		m.adjustScrollLocked()
	}
	m.mu.Unlock()
	// An event handler reaching refreshEntries on the Custom branch has no
	// captured TUI (its trailing requestRender no-ops), and a refresh that
	// leaves no live entry arms no poll loop to repaint either, so the entry
	// change must be pushed here. requestFrame covers both repaint paths:
	// the overlay invalidator the adapter installed through SetInvalidate,
	// and the captured TUI's requestRender.
	m.tui.requestFrame()
	// A refresh that leaves a live entry must (re)arm the poll loop: a tool
	// that starts while the selector is already open has to tick its spinner
	// too. This is a no-op while a loop is already armed or the modal is
	// disposed.
	m.startPollingIfNeeded()
}

// adjustScrollLocked is adjustScroll (L1842-1855).
func (m *inspectSelectorModal) adjustScrollLocked() {
	if len(m.entries) == 0 {
		m.scrollTop = 0
		return
	}
	if m.selectedIdx < m.scrollTop {
		m.scrollTop = m.selectedIdx
	} else if m.selectedIdx >= m.scrollTop+m.visibleHgt {
		m.scrollTop = m.selectedIdx - m.visibleHgt + 1
	}
	maxScroll := maxInt(0, len(m.entries)-m.visibleHgt)
	m.scrollTop = maxInt(0, minInt(m.scrollTop, maxScroll))
}

// handleInput is handleInput (L1856-1931). It reports whether the overlay
// **closed** (Esc/q/Q), not whether the key was consumed: RemoteComponentResult
// {Done: true} ends the overlay, so Up/Down/j/k/PageUp/PageDown/Home/End must
// report false or the first navigation press would dismiss the selector.
func (m *inspectSelectorModal) handleInput(data string) bool {
	if matchesKey(data, "escape") || data == "q" || data == "Q" {
		m.tui.closeInspectSelectorModal()
		return true
	}
	if matchesKey(data, "enter") || data == "\r" || data == "\n" {
		m.mu.Lock()
		selected := groupEntry{}
		if m.selectedIdx >= 0 && m.selectedIdx < len(m.entries) {
			selected = m.entries[m.selectedIdx]
		}
		m.mu.Unlock()
		if selected.kind != "" {
			// The detail opens beside the selector, exactly as the TS pair does:
			// the selector stays mounted and the detail closes back onto it.
			m.tui.openDetailModal(selected)
		}
		return false
	}
	m.mu.Lock()
	hasEntries := len(m.entries) > 0
	switch {
	case matchesKey(data, "up") || data == "k" || data == "K":
		if hasEntries {
			m.selectedIdx = maxInt(0, m.selectedIdx-1)
			m.adjustScrollLocked()
		}
	case matchesKey(data, "down") || data == "j" || data == "J":
		if hasEntries {
			m.selectedIdx = minInt(len(m.entries)-1, m.selectedIdx+1)
			m.adjustScrollLocked()
		}
	case matchesKey(data, "pageup"):
		if hasEntries {
			m.selectedIdx = maxInt(0, m.selectedIdx-m.visibleHgt)
			m.adjustScrollLocked()
		}
	case matchesKey(data, "pagedown"):
		if hasEntries {
			m.selectedIdx = minInt(len(m.entries)-1, m.selectedIdx+m.visibleHgt)
			m.adjustScrollLocked()
		}
	case matchesKey(data, "home"):
		if hasEntries {
			m.selectedIdx = 0
			m.adjustScrollLocked()
		}
	case matchesKey(data, "end"):
		if hasEntries {
			m.selectedIdx = len(m.entries) - 1
			m.adjustScrollLocked()
		}
	default:
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	m.tui.requestFrame()
	return false
}

// HandleInput implements remoteComponent: it reports whether the overlay closed.
func (m *inspectSelectorModal) HandleInput(data string) (bool, any) {
	return m.handleInput(data), nil
}

// snapshotSelectorEntry renders the row of one selector entry under rt.mu, so
// a live *toolComponent / *thinkingEntry is never formatted outside the lock.
func (m *inspectSelectorModal) snapshotRow(th theme, frame string, prefix string, entry groupEntry) (leftContent string, rightStr string, ellipsis string) {
	snap := m.tui.snapshotEntry(entry)
	switch snap.kind {
	case "tool":
		status := snap.tool.status
		icon := "✓"
		iconColor := "success"
		if status == "pending" {
			icon = frame
			iconColor = "accent"
		} else if status == "error" {
			icon = "✗"
			iconColor = "error"
		}
		summary := toolSummary(snap.tool.name, snap.tool.args)
		content := summary.content
		if content == "" {
			content = "..."
		}
		leftContent = prefix + fgOf(th, iconColor, icon) + " " + fgOf(th, "toolTitle", boldOf(th, summary.name)) + " " + fgOf(th, "dim", content)
		rightStr = fgOf(th, "muted", "("+snap.tool.elapsed+")")
		return leftContent, rightStr, fgOf(th, "dim", "...")
	case "notice":
		icon := fgOf(th, "warning", "!")
		leftContent = prefix + icon + " " + fgOf(th, "toolTitle", boldOf(th, "cache miss")) + " " + fgOf(th, "dim", noticeContent(snap.notice))
		return leftContent, "", fgOf(th, "dim", "...")
	default:
		think := snap.thinking
		var spin *spinnerFrame
		if think.active {
			value := thinkingSpinnerFrame(m.tui.nowMs(), m.tui.spinnerStartMs)
			spin = &value
		}
		icon := "✓"
		color := "thinkingText"
		if spin != nil {
			icon = spin.frame
			color = spin.color
		}
		text := strings.Join(strings.Fields(stripThinkingMarkup(strings.TrimSpace(think.text))), " ")
		if text == "" {
			text = "(thinking...)"
		}
		leftContent = prefix + fgOf(th, color, icon) + " " + fgOf(th, "toolTitle", "thinking") + " " + fgOf(th, "thinkingText", text)
		rightStr = fgOf(th, "muted", thinkingTokenLabel(&thinkingEntry{text: think.text, tokens: think.tokens}))
		return leftContent, rightStr, fgOf(th, "thinkingText", "...")
	}
}

// renderWidth is render (L1932-2060).
func (m *inspectSelectorModal) renderWidth(width int) []string {
	th := m.tui.themeOf()
	bold := func(text string) string { return boldOf(th, text) }
	borderFg := func(text string) string { return fgOf(th, "borderAccent", fgOf(th, "border", text)) }
	frame := toolSpinnerFrame(m.tui.nowMs(), m.tui.spinnerStartMs)
	termHeight := m.tui.terminalRows()
	maxInnerHeight := maxInt(5, termHeight*7/10-2)

	m.mu.Lock()
	innerHeight := 5
	if len(m.entries) > 0 {
		innerHeight = minInt(len(m.entries), maxInnerHeight)
	}
	m.visibleHgt = innerHeight
	m.adjustScrollLocked()
	entries := append([]groupEntry(nil), m.entries...)
	scrollTop := m.scrollTop
	selectedIdx := m.selectedIdx
	m.mu.Unlock()

	innerWidth := maxInt(10, width-4)
	lines := make([]string, 0, innerHeight+2)
	title := " " + fgOf(th, "accent", bold("Inspect")) + " " + fgOf(th, "dim", "(↑/↓ select · Enter view · Esc exit)") + " "
	visTitle := tui.VisibleWidth(title)
	topFiller := maxInt(1, width-2-visTitle)
	topBorder := borderFg("┌"+strings.Repeat("─", topFiller)) + title + borderFg("┐")
	lines = append(lines, truncateToWidthEllipsis(topBorder, width, ""))
	if len(entries) == 0 {
		emptyMsg := "  (no tool calls or thinking entries)"
		for i := 0; i < innerHeight; i++ {
			row := ""
			if i == innerHeight/2 {
				row = fgOf(th, "dim", emptyMsg)
			}
			pad := maxInt(0, innerWidth-tui.VisibleWidth(row))
			lines = append(lines, borderFg("│ ")+row+strings.Repeat(" ", pad)+borderFg(" │"))
		}
	} else {
		for i := 0; i < innerHeight; i++ {
			entryIdx := scrollTop + i
			if entryIdx < 0 || entryIdx >= len(entries) {
				lines = append(lines, borderFg("│ ")+strings.Repeat(" ", innerWidth)+borderFg(" │"))
				continue
			}
			entry := entries[entryIdx]
			isSelected := entryIdx == selectedIdx
			prefix := "  "
			if isSelected {
				prefix = fgOf(th, "accent", bold("▶ "))
			}
			// The row body is a value snapshot, so nothing here reads a live
			// component outside rt.mu.
			leftContent, rightStr, ellipsis := m.snapshotRow(th, frame, prefix, entry)
			maxLeft := maxInt(1, innerWidth-tui.VisibleWidth(rightStr)-1)
			leftTrunc := truncateToWidthEllipsis(leftContent, maxLeft, ellipsis)
			gap := maxInt(1, innerWidth-tui.VisibleWidth(leftTrunc)-tui.VisibleWidth(rightStr))
			rowLine := leftTrunc + strings.Repeat(" ", gap) + rightStr
			rowVis := tui.VisibleWidth(rowLine)
			if rowVis < innerWidth {
				rowLine += strings.Repeat(" ", innerWidth-rowVis)
			} else if rowVis > innerWidth {
				rowLine = truncateToWidthEllipsis(rowLine, innerWidth, "")
			}
			if isSelected {
				rowLine = highlightRow(th, rowLine)
			}
			lines = append(lines, borderFg("│ ")+rowLine+borderFg(" │"))
		}
	}
	countInfo := ""
	if len(entries) > 0 {
		countInfo = " [" + itoa(selectedIdx+1) + "/" + itoa(len(entries)) + "] "
	}
	visCount := tui.VisibleWidth(countInfo)
	botFiller := maxInt(1, width-2-visCount)
	botBorder := borderFg("└"+strings.Repeat("─", botFiller)) + fgOf(th, "dim", countInfo) + borderFg("┘")
	lines = append(lines, truncateToWidthEllipsis(botBorder, width, ""))
	return lines
}

// Render implements remoteComponent.
func (m *inspectSelectorModal) Render(width int) []string { return m.renderWidth(width) }

// highlightRow is the selected row treatment of L2032-2038.
func highlightRow(th theme, row string) string {
	if th == nil {
		return row
	}
	if styled := th.bg("selectedBg", row); styled != row {
		return styled
	}
	return th.inverse(row)
}

// dispose is dispose (L2061-2062): drop the poll timer the way the detail
// modal's dispose does, so the goroutine cannot outlive the overlay.
func (m *inspectSelectorModal) dispose() {
	m.mu.Lock()
	timer := m.pollTimer
	m.pollTimer = nil
	m.polling = false
	m.disposed = true
	m.mu.Unlock()
	timer.stopTimer()
}

// invalidate is invalidate (L2063-2064).
func (m *inspectSelectorModal) invalidate() {}

// allInspectableEntries is getAllInspectableEntries (L1804-1813).
func (r *runtimeState) allInspectableEntries() []groupEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	groups := r.groupsListLocked()
	out := make([]groupEntry, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.visibleEntriesLocked()...)
	}
	return out
}

// truncateToWidthEllipsis is pi-tui's truncateToWidth, where an empty ellipsis
// means "cut without a marker".
func truncateToWidthEllipsis(text string, width int, ellipsis string) string {
	if width < 1 {
		width = 1
	}
	return tui.TruncateToWidth(text, width, ellipsis, false)
}

// roundHalfUp is Math.round: halfway cases round up.
func roundHalfUp(value float64) float64 {
	return float64(jsRoundHalfUp(value))
}
