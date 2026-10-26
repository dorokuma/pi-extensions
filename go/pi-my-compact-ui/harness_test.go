package compactui

import (
	"strings"
	"sync"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file holds the shared helpers of the Go test suite. The TS suite drives
// the extension through mock ctx objects; the SDK's Context is a struct whose
// methods reach a live host, so the Go suite drives the same code paths
// through the ported seams instead: a fake theme records every colour call, a
// fake TUI records frame requests, a fake uiContext records the host calls the
// modals make, and a fake clock makes spinner/elapsed output deterministic.

// recordingTheme records every fg/bold call and rewrites each style to a
// readable marker, so a rendered line shows both the token and the text.
type recordingTheme struct {
	mu    sync.Mutex
	calls []styleCall
}

type styleCall struct {
	kind  string
	token string
	text  string
}

func (t *recordingTheme) fg(token, text string) string {
	t.mu.Lock()
	t.calls = append(t.calls, styleCall{kind: "fg", token: token, text: text})
	t.mu.Unlock()
	return "[" + token + ":" + text + "]"
}

func (t *recordingTheme) bold(text string) string {
	t.mu.Lock()
	t.calls = append(t.calls, styleCall{kind: "bold", text: text})
	t.mu.Unlock()
	return "«" + text + "»"
}

func (t *recordingTheme) bg(token, text string) string {
	t.mu.Lock()
	t.calls = append(t.calls, styleCall{kind: "bg", token: token, text: text})
	t.mu.Unlock()
	return ""
}

func (t *recordingTheme) inverse(text string) string {
	t.mu.Lock()
	t.calls = append(t.calls, styleCall{kind: "inverse", text: text})
	t.mu.Unlock()
	return ""
}

// themeTokens lists the tokens the theme was asked for, in call order.
func (t *recordingTheme) themeTokens() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.calls))
	for _, call := range t.calls {
		if call.kind == "fg" {
			out = append(out, call.token)
		}
	}
	return out
}

// fakeTUI records requestRender calls and the overlay opens.
type fakeTUI struct {
	mu       sync.Mutex
	renders  int
	rows     int
	overlays []fakeOverlay
}

type fakeOverlay struct {
	comp      remoteComponent
	widthPct  int
	maxHeight int
	hidden    int
	handle    *fakeOverlayHandle
}

// fakeOverlayHandle records hide() calls.
type fakeOverlayHandle struct {
	mu    sync.Mutex
	hides int
}

func (h *fakeOverlayHandle) hide() {
	h.mu.Lock()
	h.hides++
	h.mu.Unlock()
}

func newFakeTUI(rows int) *fakeTUI { return &fakeTUI{rows: rows} }

func (f *fakeTUI) requestRender() {
	f.mu.Lock()
	f.renders++
	f.mu.Unlock()
}

func (f *fakeTUI) terminalRows() int { return f.rows }

func (f *fakeTUI) addOverlay(comp remoteComponent, widthPercent, maxHeightPercent int) overlayCloser {
	handle := &fakeOverlayHandle{}
	f.mu.Lock()
	f.overlays = append(f.overlays, fakeOverlay{comp: comp, widthPct: widthPercent, maxHeight: maxHeightPercent, handle: handle})
	f.mu.Unlock()
	return handle
}

// renderCount reports how many frames were requested.
func (f *fakeTUI) renderCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renders
}

// lastOverlay returns the most recently opened overlay.
func (f *fakeTUI) lastOverlay() (remoteComponent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.overlays) == 0 {
		return nil, false
	}
	return f.overlays[len(f.overlays)-1].comp, true
}

// fakeUI records the host UI calls a session_start or command makes.
type fakeUI struct {
	mu            sync.Mutex
	hiddenLabels  []string
	inputHandlers []func(string) terminalInputVerdict
	unsubscribed  int
	widgetKeys    []string
	notices       []fakeNotice
	customCalls   int
	customFn      func(remoteComponentFactory, any) (any, error)
	ui            bool
}

type fakeNotice struct {
	message string
	level   string
}

func newFakeUI() *fakeUI { return &fakeUI{ui: true} }

func (f *fakeUI) setHiddenThinkingLabel(label string) {
	f.mu.Lock()
	f.hiddenLabels = append(f.hiddenLabels, label)
	f.mu.Unlock()
}

func (f *fakeUI) onTerminalInput(handler func(string) terminalInputVerdict) func() {
	f.mu.Lock()
	f.inputHandlers = append(f.inputHandlers, handler)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.unsubscribed++
		f.mu.Unlock()
	}
}

func (f *fakeUI) setWidgetRenderer(key string, _ func(width int) []string) {
	f.mu.Lock()
	f.widgetKeys = append(f.widgetKeys, key)
	f.mu.Unlock()
}

func (f *fakeUI) notify(message, level string) {
	f.mu.Lock()
	f.notices = append(f.notices, fakeNotice{message: message, level: level})
	f.mu.Unlock()
}

func (f *fakeUI) hasUI() bool { return f.ui }

func (f *fakeUI) custom(factory remoteComponentFactory, options any) (any, error) {
	f.mu.Lock()
	f.customCalls++
	fn := f.customFn
	f.mu.Unlock()
	if fn != nil {
		return fn(factory, options)
	}
	return nil, nil
}

// lastInputHandler returns the terminal input handler the extension installed.
func (f *fakeUI) lastInputHandler() func(string) terminalInputVerdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputHandlers) == 0 {
		return nil
	}
	return f.inputHandlers[len(f.inputHandlers)-1]
}

// ── runtime fixtures ───────────────────────────────────────────────────────

// testRuntime builds a runtime state with a deterministic clock and no host.
func testRuntime(t testing.TB) *runtimeState {
	t.Helper()
	r := createRuntimeState()
	r.spinnerStartMs = 0
	r.clock = func() int64 { return 0 }
	r.maxGroupEntries = defaultMaxGroupEntries
	r.configPath = t.TempDir() + "/compact-ui.json"
	r.currentTheme = nil
	return r
}

// withTheme installs a recording theme.
func (r *runtimeState) withTheme(th theme) {
	r.mu.Lock()
	r.currentTheme = th
	r.mu.Unlock()
}

// setClock pins the clock.
func (r *runtimeState) setClock(fn func() int64) {
	r.mu.Lock()
	r.clock = fn
	r.mu.Unlock()
}

// newTestTool builds a tool component with the given shape.
func newTestTool(id, name string, args map[string]any) *toolComponent {
	return &toolComponent{toolCallID: id, toolName: name, args: args, argsComplete: true}
}

// doneResult builds a successful tool result.
func doneResult(text string) *toolResult {
	return &toolResult{content: []contentItem{{typ: "text", text: text}}}
}

// succeeded marks a tool as finished successfully.
func (t *toolComponent) succeeded(text string) *toolComponent {
	t.result = doneResult(text)
	t.isPartial = false
	t.executionStarted = true
	return t
}

// failed marks a tool as finished with an error.
func (t *toolComponent) failed() *toolComponent {
	t.result = &toolResult{isError: true}
	t.isPartial = false
	t.executionStarted = true
	return t
}

// pending marks a tool as still running.
func (t *toolComponent) pending() *toolComponent {
	t.isPartial = true
	t.executionStarted = true
	return t
}

// newTestAssistant builds an assistant message component with a content
// container registered as an assistant content container.
func (r *runtimeState) newTestAssistant(content ...contentItem) *assistantMessageComponent {
	c := newContainer(r)
	assistant := &assistantMessageComponent{contentContainer: c, tui: r}
	assistant.lastMessage = &assistantMessage{role: "assistant", content: content}
	r.mu.Lock()
	r.assistantContentCont[c] = true
	r.getAssistantContentStateLocked(c)
	r.mu.Unlock()
	return assistant
}

// textItem builds a text content item.
func textItem(text string) contentItem { return contentItem{typ: "text", text: text} }

// thinkingItem builds a thinking content item.
func thinkingItem(text string) contentItem { return contentItem{typ: "thinking", thinking: text} }

// toolCallItem builds a toolCall content item.
func toolCallItem() contentItem { return contentItem{typ: "toolCall"} }

// joinLines joins rendered lines for assertion messages.
func joinLines(lines []string) string { return strings.Join(lines, "\n") }

// containsLine reports whether any rendered line contains want.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// runeLen counts runes (the visible-length proxy for ASCII-only fixtures).
func runeLen(text string) int { return len([]rune(text)) }

// sdkZeroContext is the zero sdk.Context: the value a caller without a host
// connection holds (every method dereferences a nil extension).
func sdkZeroContext() sdk.Context { return sdk.Context{} }

// eventHandlerName maps an event name to the runtime method that handles it,
// documenting the wiring a lifecycle test drives.

// mustEvent invokes an extension event handler registered under name. The
// factory registers handlers through e.OnEvent; a test that needs the handler
// body calls the runtime method directly, and this helper documents the
// mapping from event name to method for the lifecycle tests.
func eventHandlerName(event string) string {
	switch event {
	case sdk.EventSessionStart:
		return "handleSessionStart"
	case sdk.EventSessionShutdown:
		return "handleSessionShutdown"
	case sdk.EventToolExecutionStart:
		return "handleToolExecutionStart"
	case sdk.EventToolExecutionEnd:
		return "handleToolExecutionEnd"
	case sdk.EventMessageStart:
		return "handleMessageStart"
	case sdk.EventMessageEnd:
		return "handleMessageEnd"
	case sdk.EventMessageUpdate:
		return "handleMessageUpdate"
	case sdk.EventAgentEnd:
		return "handleAgentEnd"
	case sdk.EventAgentStart, sdk.EventTurnStart, sdk.EventTurnEnd:
		// The three work-phase events share setAgentWorkPhase (L2279-2298).
		return "setAgentWorkPhase"
	}
	return ""
}
