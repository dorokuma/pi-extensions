package compactui

import "sync"

// This file ports PT's component tree (the pi-tui Container / Markdown / Text /
// Spacer / ToolExecutionComponent / AssistantMessageComponent objects PT
// subclasses and patches). PiG's SDK renders extensions in a subprocess: a
// component tree cannot cross the wire, so the port keeps the exact rendering
// and grouping logic in plain structs and hands the host only the strings each
// component renders (see README "TS 进程内原型补丁与 PiG 子进程协议的边界").

// component is one node of the ported tree. Render is called at a fixed width
// and returns the terminal lines; invalidate drops any cached render.
type component interface {
	Render(width int) []string
	Invalidate()
}

// container is pi-tui's Container: an ordered child list with add/remove/clear.
//
// children is guarded by its own mutex rather than runtimeState.mu, because the
// grouping layer splices the list from event handlers while the host draws it
// on the TUI goroutine. The lock is a leaf: never take runtimeState.mu while
// holding it. Reads go through snapshot, so a renderer never walks a slice an
// event handler is rewriting.
type container struct {
	mu       sync.Mutex
	children []component
	tui      *runtimeState
}

func newContainer(state *runtimeState) *container { return &container{tui: state} }

// snapshot copies the child list.
func (c *container) snapshot() []component {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]component(nil), c.children...)
}

// len is the child count.
func (c *container) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.children)
}

// at is the child at index, or nil when out of range.
func (c *container) at(index int) component {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.children) {
		return nil
	}
	return c.children[index]
}

// last is the final child, or nil when the list is empty.
func (c *container) last() component { return c.at(c.len() - 1) }

// addChild appends a child, mirroring Container.addChild.
func (c *container) addChild(child component) component {
	c.mu.Lock()
	c.children = append(c.children, child)
	c.mu.Unlock()
	return child
}

// removeChild removes a child, mirroring Container.removeChild.
func (c *container) removeChild(child component) {
	c.mu.Lock()
	for i, existing := range c.children {
		if existing == child {
			c.children = append(c.children[:i], c.children[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

// clear drops every child.
func (c *container) clear() {
	c.mu.Lock()
	c.children = nil
	c.mu.Unlock()
}

// indexOf returns the position of child, or -1.
func (c *container) indexOf(child component) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, existing := range c.children {
		if existing == child {
			return i
		}
	}
	return -1
}

// contains reports whether child is a direct child.
func (c *container) contains(child component) bool { return c.indexOf(child) >= 0 }

// insertAt inserts child at index, clamping to the list bounds.
func (c *container) insertAt(index int, child component) {
	c.spliceInsert(index, 0, child)
}

// spliceInsert replaces the count children from start with comps
// (Array.splice(start, count, ...items)).
func (c *container) spliceInsert(start, count int, comps ...component) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if start > len(c.children) {
		start = len(c.children)
	}
	if count < 0 {
		count = 0
	}
	if start+count > len(c.children) {
		count = len(c.children) - start
	}
	tail := append([]component(nil), c.children[start+count:]...)
	c.children = append(c.children[:start], comps...)
	c.children = append(c.children, tail...)
}

// spliceRange removes count children starting at start (Array.splice(start, count)).
func (c *container) spliceRange(start, count int) {
	c.mu.Lock()
	if start < 0 || start >= len(c.children) || count <= 0 {
		c.mu.Unlock()
		return
	}
	if start+count > len(c.children) {
		count = len(c.children) - start
	}
	c.children = append(c.children[:start], c.children[start+count:]...)
	c.mu.Unlock()
}

// truncate keeps the first n children.
func (c *container) truncate(n int) {
	c.mu.Lock()
	if n < 0 {
		n = 0
	}
	if n > len(c.children) {
		n = len(c.children)
	}
	c.children = c.children[:n]
	c.mu.Unlock()
}

// Replace renders the children joined, as Container.render does. The child
// list is snapshotted, because an event handler may be splicing it while the
// host draws.
func (c *container) Replace(width int) []string {
	children := c.snapshot()
	lines := []string{}
	for _, child := range children {
		lines = append(lines, child.Render(width)...)
	}
	return lines
}

// Render implements component.
func (c *container) Render(width int) []string { return c.Replace(width) }

// Invalidate implements component.
func (c *container) Invalidate() {
	for _, child := range c.snapshot() {
		child.Invalidate()
	}
}

// spacer is pi-tui's Spacer: n blank lines.
type spacer struct{ lines int }

func newSpacer(n int) *spacer { return &spacer{lines: n} }

func (s *spacer) Render(int) []string {
	out := make([]string, s.lines)
	return out
}

func (s *spacer) Invalidate() {}

// textComponent is pi-tui's Text: a plain (already styled) string.
type textComponent struct{ text string }

func (t *textComponent) Render(int) []string { return []string{t.text} }

func (t *textComponent) Invalidate() {}

// markdownComponent is pi-tui's Markdown. defaultTextStyle marks the
// "Thinking..." placeholder Markdown a collapsed native thinking block renders
// (isNativeThinkingWidget, L543-551). It is a pointer so a test can build the
// exact same shape the host builds.
type markdownComponent struct {
	text             string
	defaultTextStyle bool
}

func (m *markdownComponent) Render(int) []string { return []string{m.text} }

func (m *markdownComponent) Invalidate() {}

// assistantMessageComponent is @earendil-works/pi-coding-agent's
// AssistantMessageComponent: the streaming assistant message. contentContainer
// holds the message's rendered children (text Markdowns, tool executions,
// native thinking widgets); lastMessage is the message it was last updated
// with; isStreaming reports the live tail.
type assistantMessageComponent struct {
	contentContainer *container
	lastMessage      *assistantMessage
	isStreaming      bool
	hideThinking     bool
	hiddenThinking   string
	live             bool
	streamingComp    bool
	// tui is the runtime the component was created under, for the patched paths.
	tui *runtimeState
}

// Render renders the content container.
func (a *assistantMessageComponent) Render(width int) []string {
	if a.contentContainer == nil {
		return nil
	}
	return a.contentContainer.Render(width)
}

// Invalidate invalidates the content container.
func (a *assistantMessageComponent) Invalidate() {
	if a.contentContainer != nil {
		a.contentContainer.Invalidate()
	}
}

// toolResult is a finished tool execution result.
type toolResult struct {
	isError   bool
	content   []contentItem
	timestamp int64
}

// toolComponent is @earendil-works/pi-coding-agent's ToolExecutionComponent:
// one tool call. The port keeps the fields PT reads and writes off it
// (_groupedAt / _groupEndAt / _contentIndex / _receivingArgs /
// _elapsedDuration / _interrupted / argsComplete / executionStarted /
// _preparing) plus the render state the host would own.
type toolComponent struct {
	toolCallID       string
	toolName         string
	args             map[string]any
	result           *toolResult
	isPartial        bool
	executionStarted bool
	argsComplete     bool

	// group bookkeeping, exactly the TS underscore fields.
	owner         *toolGroupComponent
	groupedAt     *int64
	groupEndAt    *int64
	contentIndex  int
	hasIndex      bool
	receivingArgs *bool
	elapsedDur    *float64
	interrupted   bool
	preparing     bool

	rendered []string
	tui      *runtimeState
}

// SetRendered records the host-rendered body of an ungrouped tool call (the
// ported renderer hides a grouped tool; see hideGroupedToolRender).
func (t *toolComponent) SetRendered(lines []string) { t.rendered = lines }

// Render is the tool's own body: empty once the tool belongs to a group
// (hideGroupedToolRender, L1129-1139), otherwise what the host rendered.
func (t *toolComponent) Render(int) []string {
	if t.tui != nil {
		t.tui.mu.Lock()
		owner := t.owner
		t.tui.mu.Unlock()
		if owner != nil {
			return nil
		}
	}
	return t.rendered
}

// Invalidate drops the cached host render.
func (t *toolComponent) Invalidate() {}

// compactionHeaderComponent is PT's CompactionHeaderComponent (L1512-1536).
type compactionHeaderComponent struct {
	tokensBefore int
	tui          *runtimeState
}

// Render renders the compaction line, compacting it when the width is short.
func (h *compactionHeaderComponent) Render(width int) []string {
	return h.renderWidth(width)
}

func (h *compactionHeaderComponent) Invalidate() {}

// turnDividerComponent is PT's TurnDividerComponent (L360-385).
type turnDividerComponent struct {
	timeLabel string
	tui       *runtimeState
}

func (d *turnDividerComponent) Invalidate() {}
