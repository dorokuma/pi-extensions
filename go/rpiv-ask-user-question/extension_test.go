package rpivaskuserquestion

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// ── extension wiring ──────────────────────────────────────────────────────

// Extension() must build without an SDK connection: PiG calls the factory before
// the socket is up, and any panic here would keep the extension from loading.
func TestExtensionBuildsWithoutAHost(t *testing.T) {
	extension := Extension()
	if extension == nil {
		t.Fatal("Extension() returned nil")
	}
}

// Guidance overrides the built-in copy, and invalid guidance falls back.
func TestValidateGuidanceFieldsOverrides(t *testing.T) {
	t.Run("guidance overrides the defaults", func(t *testing.T) {
		guidance := ValidateGuidanceFields(&GuidanceFields{
			PromptSnippet:    "custom snippet",
			PromptGuidelines: []string{"custom guideline"},
			Description:      "custom description",
		})
		if guidance == nil || guidance.PromptSnippet != "custom snippet" {
			t.Fatalf("guidance = %#v", guidance)
		}
		if guidance.Description != "custom description" || len(guidance.PromptGuidelines) != 1 {
			t.Fatalf("guidance = %#v", guidance)
		}
	})

	t.Run("empty guidance yields nil", func(t *testing.T) {
		if got := ValidateGuidanceFields(&GuidanceFields{}); got != nil {
			t.Fatalf("guidance = %#v", got)
		}
		if got := ValidateGuidanceFields(nil); got != nil {
			t.Fatalf("nil guidance = %#v", got)
		}
	})

	t.Run("guidelines containing an empty string are dropped", func(t *testing.T) {
		guidance := ValidateGuidanceFields(&GuidanceFields{
			Description:      "kept",
			PromptGuidelines: []string{"ok", ""},
		})
		if guidance == nil || guidance.Description != "kept" {
			t.Fatalf("guidance = %#v", guidance)
		}
		if len(guidance.PromptGuidelines) != 0 {
			t.Fatalf("guidelines = %v", guidance.PromptGuidelines)
		}
	})

	t.Run("the validator does not alias the caller's slice", func(t *testing.T) {
		original := []string{"a"}
		guidance := ValidateGuidanceFields(&GuidanceFields{PromptGuidelines: original})
		guidance.PromptGuidelines[0] = "mutated"
		if original[0] != "a" {
			t.Fatal("guidance aliased the caller's slice")
		}
	})
}

// The config's guidance reaches the registered copy through LoadConfig.
func TestGuidanceComesFromConfig(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", ConfigPackageName)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := `{"guidance":{"promptSnippet":"from config","description":"desc from config"}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PIG_HOME", "")
	t.Setenv("PIG_CODING_AGENT_DIR", "")

	guidance := ValidateGuidanceFields(LoadConfig().Guidance)
	if guidance == nil || guidance.PromptSnippet != "from config" || guidance.Description != "desc from config" {
		t.Fatalf("guidance = %#v", guidance)
	}
}

// ── items ─────────────────────────────────────────────────────────────────

func TestBuildItemsByTab(t *testing.T) {
	items := BuildItemsByTab(singleParams())
	if len(items) != 1 {
		t.Fatalf("tabs = %d", len(items))
	}
	if len(items[0]) != 3 {
		t.Fatalf("rows = %d", len(items[0]))
	}
}

// ── percent parsing / size literals ───────────────────────────────────────

func TestParsePercent(t *testing.T) {
	cases := map[string]float64{
		"100%": 100,
		"50%":  50,
		"0%":   0,
		"100":  100,
		"%":    0,
		"":     0,
		"12x%": 0,
	}
	for input, want := range cases {
		if got := parsePercent(input); got != want {
			t.Fatalf("parsePercent(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestStringsTrimSuffix(t *testing.T) {
	if got := stringsTrimSuffix("100%", "%"); got != "100" {
		t.Fatalf("got %q", got)
	}
	if got := stringsTrimSuffix("100", "%"); got != "100" {
		t.Fatalf("got %q", got)
	}
	if got := stringsTrimSuffix("%", "%"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// ── result decoding across the SDK boundary ───────────────────────────────

func TestResultFromCustom(t *testing.T) {
	typed := QuestionnaireResult{Cancelled: true, Answers: []QuestionAnswer{}}
	if got := resultFromCustom(typed); got == nil || !got.Cancelled {
		t.Fatalf("typed = %#v", got)
	}
	if got := resultFromCustom(&typed); got == nil || !got.Cancelled {
		t.Fatalf("pointer = %#v", got)
	}
	wire := map[string]any{
		"answers":   []any{map[string]any{"questionIndex": 0.0, "question": "Q?", "kind": "custom", "answer": "typed"}},
		"cancelled": false,
	}
	if got := resultFromCustom(wire); got == nil || len(got.Answers) != 1 || *got.Answers[0].Answer != "typed" {
		t.Fatalf("wire = %#v", got)
	}
	for _, invalid := range []any{nil, "text", 42, map[string]any{}} {
		if got := resultFromCustom(invalid); got != nil {
			t.Fatalf("%#v decoded to %#v", invalid, got)
		}
	}
}

// ── envelopes ─────────────────────────────────────────────────────────────

func TestRejectWithoutUI(t *testing.T) {
	result := rejectWithoutUI()
	if result.Content[0].Text != errorNoUIDescription {
		t.Fatalf("text = %q", result.Content[0].Text)
	}
	if result.Details.Error == nil || *result.Details.Error != ErrorNoUI {
		t.Fatalf("error = %v", result.Details.Error)
	}
	if !result.Details.Cancelled || result.Details.Answers == nil {
		t.Fatalf("details = %#v", result.Details)
	}
}

func TestBuildErrorResult(t *testing.T) {
	result := buildErrorResult("Error: bad", ErrorReservedLabel)
	if result.Content[0].Text != "Error: bad" {
		t.Fatalf("text = %q", result.Content[0].Text)
	}
	if result.Details.Error == nil || *result.Details.Error != ErrorReservedLabel {
		t.Fatalf("error = %v", result.Details.Error)
	}
	if len(result.Details.Answers) != 0 || !result.Details.Cancelled {
		t.Fatalf("details = %#v", result.Details)
	}
}

// A rejected questionnaire keeps the user-facing error copy verbatim.
func TestErrorCopyConstants(t *testing.T) {
	if !strings.HasPrefix(errorNoUIDescription, "Error: UI not available") {
		t.Fatalf("no-UI copy = %q", errorNoUIDescription)
	}
	if !strings.Contains(errorNoCustomUI, "do NOT treat this as a decline") {
		t.Fatalf("no-custom-UI copy = %q", errorNoCustomUI)
	}
	if !strings.Contains(errorNoCustomUI, "plain chat text instead") {
		t.Fatalf("no-custom-UI copy = %q", errorNoCustomUI)
	}
}

// ── renderer entry points ─────────────────────────────────────────────────

func TestRenderAskCallWidth(t *testing.T) {
	params := singleParams()
	lines := renderAskCallWidth(&params, themeForTests(), 80)
	if len(lines) != 1 || !strings.Contains(renderStrings(lines), "Auth") {
		t.Fatalf("lines = %#v", lines)
	}
	// A zero width keeps the bare text rather than truncating to nothing.
	if got := renderAskCallWidth(&params, themeForTests(), 0); len(got) != 1 || !strings.Contains(renderStrings(got), "Ask") {
		t.Fatalf("zero-width lines = %#v", got)
	}
}

// ── external editor ───────────────────────────────────────────────────────

// Returning from the external editor repaints the questionnaire through the
// session's invalidate callback — it never raises a notification, and it is a
// no-op (not a crash) when no repaint path exists yet.
func TestHostTUIRequestRenderInvalidatesInsteadOfNotifying(t *testing.T) {
	renders := 0
	tui := hostTUI{requestRender: func() { renders++ }}
	tui.RequestRender(true)
	tui.RequestRender(false)
	if renders != 2 {
		t.Fatalf("repaint requests = %d, want 2", renders)
	}
	// The zero handle has no session to ask: the editor hand-back must not reach
	// for a host call it does not have.
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("RequestRender without a session panicked: %v", recovered)
			}
		}()
		hostTUI{}.RequestRender(true)
	}()
}

// ── collapse key listener ─────────────────────────────────────────────────

// The raw listener exists only for a presentation that hides the dialog, and only
// while the collapse key is on; every other case must report "not registered" so
// the session keeps the visible one-line collapsed row. Both branches return
// before touching the host, so a zero Context stands in for one.
func TestRegisterCollapseKeyListenerGate(t *testing.T) {
	session := func() *QuestionnaireSession { return nil }
	handle := func() *sdk.OverlayHandle { return nil }

	if remove := registerCollapseKeyListener(sdk.Context{}, DefaultCollapseKey, false, session, handle); remove != nil {
		remove()
		t.Fatal("listener registered for a presentation that never hides")
	}
	if remove := registerCollapseKeyListener(sdk.Context{}, CollapseKeyOff, true, session, handle); remove != nil {
		remove()
		t.Fatal("listener registered for the collapse key being off")
	}
}

// The mount's repaint seam must never run inside the session's mutex.
// mountQuestionnaire keeps `requestRepaint` for the external-editor hand-back only:
// the dialog itself repaints through the invalidate callback the SDK installs, so the
// session no longer receives a callback that runs from viewAdapter.Apply while commit
// holds s.mu — re-locking it on the same goroutine self-deadlocked. This drives the
// mount's whole repaint wiring (the editor hand-back closure, the input paths that
// project the view, the collapse toggle) with a repaint that renders synchronously.
func TestMountRepaintReachesTheSessionWithoutReenteringItsLock(t *testing.T) {
	var mu sync.Mutex
	var mounted *QuestionnaireSession
	var repaints int32

	// requestRepaint is mountQuestionnaire's closure, resolved through the same
	// mutex the SDK input loop reads the session through.
	requestRepaint := func() {
		mu.Lock()
		current := mounted
		mu.Unlock()
		if current != nil {
			current.RequestRender()
		}
	}
	// repaintNow is what a host repaint ends up doing: rendering the dialog
	// synchronously, exactly like the SDK's render loop does from the invalidate
	// callback it installs. It re-enters a locking entry point of the session, which
	// is the whole hazard: before the fix the mount passed its repaint closure
	// through to the session, and the session called it from viewAdapter.Apply while
	// commit/handleIgnoreInline still held s.mu.
	repaintNow := func() {
		mu.Lock()
		current := mounted
		mu.Unlock()
		if current == nil {
			return
		}
		atomic.AddInt32(&repaints, 1)
		if len(current.Render(80)) == 0 {
			t.Error("repaint painted no rows")
		}
	}

	config := sessionConfig(singleParams())
	config.RequestRender = repaintNow
	// The editor hand-back keeps the mount's repaint closure: what the dialog itself
	// repaints through is the invalidate callback below, never a closure the session
	// calls while it owns its mutex.
	config.EditInput = func(value string) (string, bool) {
		hostTUI{requestRender: requestRepaint}.RequestRender(true)
		return "edited text", true
	}
	session := NewQuestionnaireSession(config)
	mu.Lock()
	mounted = session
	mu.Unlock()
	session.SetInvalidate(repaintNow)

	done := make(chan struct{})
	go func() {
		defer close(done)
		session.HandleInput("\x1b[B") // nav → commit
		session.HandleInput("\x1b[B") // the Type-something. row → commit
		session.HandleInput("k")      // ActionIgnore → handleIgnoreInline
		session.HandleInput("\x1b[103;5u")
		session.ToggleCollapsedExternal()
		session.ToggleCollapsedExternal()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: the repaint request re-entered the session while it held s.mu")
	}
	if atomic.LoadInt32(&repaints) == 0 {
		t.Fatal("the mount's repaint never reached the invalidator")
	}
	if text := renderStrings(session.Render(80)); !strings.Contains(text, "edited text") {
		t.Fatalf("editor hand-back not committed:\n%s", text)
	}
}

// The one-shot hide hint names the key in display form.
func TestHiddenNotice(t *testing.T) {
	if got := hiddenNotice("ctrl+]"); got != "ask_user_question hidden — press Ctrl+] to reopen" {
		t.Fatalf("notice = %q", got)
	}
	if got := hiddenNotice("alt+o"); got != "ask_user_question hidden — press Alt+O to reopen" {
		t.Fatalf("notice = %q", got)
	}
}

func TestResolveEditorCommand(t *testing.T) {
	lookup := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	if got := ResolveEditorCommand(lookup(map[string]string{"VISUAL": "code", "EDITOR": "vim"}))(); got != "code" {
		t.Fatalf("command = %q (VISUAL must win)", got)
	}
	if got := ResolveEditorCommand(lookup(map[string]string{"EDITOR": "vim"}))(); got != "vim" {
		t.Fatalf("command = %q", got)
	}
	if got := ResolveEditorCommand(lookup(map[string]string{"VISUAL": "  ", "EDITOR": "vim"}))(); got != "vim" {
		t.Fatalf("blank VISUAL should fall through: %q", got)
	}
	if got := ResolveEditorCommand(lookup(nil))(); got != "" {
		t.Fatalf("no editor configured → %q", got)
	}
}

func TestSplitEditorCommand(t *testing.T) {
	editor, args := splitEditorCommand("code --wait")
	if editor != "code" || len(args) != 1 || args[0] != "--wait" {
		t.Fatalf("split = (%q,%v)", editor, args)
	}
	// Leading spaces are dropped; the rest keep their positions.
	editor, args = splitEditorCommand("  vim -u NONE")
	if editor != "vim" || len(args) != 2 || args[0] != "-u" || args[1] != "NONE" {
		t.Fatalf("split = (%q,%v)", editor, args)
	}
	if editor, args := splitEditorCommand("   "); editor != "" || args != nil {
		t.Fatalf("blank split = (%q,%v)", editor, args)
	}
}

func TestStripOneTrailingNewline(t *testing.T) {
	cases := map[string]string{
		"text\n":   "text",
		"text\r\n": "text",
		"text\n\n": "text\n",
		"text":     "text",
		"":         "",
		"\n":       "",
	}
	for input, want := range cases {
		if got := stripOneTrailingNewline(input); got != want {
			t.Fatalf("stripOneTrailingNewline(%q) = %q, want %q", input, got, want)
		}
	}
}

// fakeTUI records the lifecycle calls the editor flow makes.
type fakeTUI struct {
	stops   int
	starts  int
	renders int
}

func (f *fakeTUI) Stop()              { f.stops++ }
func (f *fakeTUI) Start()             { f.starts++ }
func (f *fakeTUI) RequestRender(bool) { f.renders++ }

func TestEditWithExternalEditor(t *testing.T) {
	tui := &fakeTUI{}
	var seenFile string
	edited, err := EditWithExternalEditor(tui, "vim", "original", func(editor string, args []string, file string) error {
		if editor != "vim" {
			t.Fatalf("editor = %q", editor)
		}
		seenFile = file
		// The editor rewrites the file; the flow reads it back.
		return os.WriteFile(file, []byte("replaced\n"), 0o600)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edited != "replaced" {
		t.Fatalf("edited = %q", edited)
	}
	if seenFile == "" || !strings.HasSuffix(seenFile, "answer.md") {
		t.Fatalf("temp file = %q", seenFile)
	}
	if tui.stops != 1 || tui.starts != 1 || tui.renders != 1 {
		t.Fatalf("tui lifecycle = %#v", tui)
	}
	// The temp directory is cleaned up.
	if _, err := os.Stat(filepath.Dir(seenFile)); err == nil {
		t.Fatalf("temp dir %q not cleaned up", filepath.Dir(seenFile))
	}
}

// A failing editor still restarts the TUI.
func TestEditWithExternalEditorFailureRestartsTUI(t *testing.T) {
	tui := &fakeTUI{}
	_, err := EditWithExternalEditor(tui, "vim", "x", func(string, []string, string) error {
		return &editorExitError{reason: "exit code 1"}
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if tui.starts != 1 {
		t.Fatalf("TUI not restarted after a failure: %#v", tui)
	}
}

// An empty editor command is rejected, and the TUI is never stopped.
func TestEditWithExternalEditorEmptyCommand(t *testing.T) {
	tui := &fakeTUI{}
	if _, err := EditWithExternalEditor(tui, "   ", "x", nil); err == nil {
		t.Fatal("empty command accepted")
	}
	if tui.stops != 0 {
		t.Fatalf("TUI stopped for an empty command: %#v", tui)
	}
}

// ── error text ────────────────────────────────────────────────────────────

func TestEditorExitErrorMessage(t *testing.T) {
	if got := (&editorExitError{reason: "exit code 2"}).Error(); got != "External editor exited with exit code 2" {
		t.Fatalf("message = %q", got)
	}
	if errEmptyEditorCommand.Error() != "External editor exited with External editor command is empty" {
		t.Fatalf("empty message = %q", errEmptyEditorCommand.Error())
	}
}

// ── key matching delegates to the SDK ────────────────────────────────────

func TestMatchesKey(t *testing.T) {
	cases := []struct {
		data string
		key  string
		want bool
	}{
		{"\r", "enter", true},
		{"\r", "escape", false},
		{"\x1b", "escape", true},
		{"\x1b[A", "up", true},
		{"\x1b[B", "down", true},
		{"\t", "tab", true},
		{" ", "space", true},
		{"\x1d", "ctrl+]", true},
		{"a", "] ", false},
		{"", "enter", false},
	}
	for _, tc := range cases {
		if got := MatchesKey(tc.data, tc.key); got != tc.want {
			t.Fatalf("MatchesKey(%q,%q) = %v, want %v", tc.data, tc.key, got, tc.want)
		}
	}
}

// The SDK keybinding manager answers the semantic names the router uses.
func TestSDKKeybindingsResolveDefaults(t *testing.T) {
	kb := SDKKeybindings{}
	// pi's defaults bind Enter to both confirm and submit.
	if !kb.Matches("\r", "tui.select.confirm") {
		t.Fatal("default confirm binding missing")
	}
	if !kb.Matches("\x1b", "tui.select.cancel") {
		t.Fatal("default cancel binding missing")
	}
	if kb.Matches("x", "tui.select.confirm") {
		t.Fatal("unrelated key matched confirm")
	}
}

func TestIsKeyReleaseAndRepeat(t *testing.T) {
	// A Kitty release event: ESC [ <code> ; <mods> : 3 u.
	if !IsKeyRelease("\x1b[93;1:3u") {
		t.Fatal("release event not recognized")
	}
	if !IsKeyRepeat("\x1b[93;1:2u") {
		t.Fatal("repeat event not recognized")
	}
	if IsKeyRelease("\x1b[93;1:1u") {
		t.Fatal("press event reported as release")
	}
	if IsKeyRelease("\x1b[A") || IsKeyRepeat("\x1b[A") {
		t.Fatal("legacy sequence reported as a Kitty event")
	}
}

// TestExtensionDefersBusSubscriptionToOnLoaded asserts the answer subscription is
// deferred to OnLoaded instead of being made inside the factory: a factory-time
// Events().On dials the host socket and waits on a synchronous host RPC, which
// deadlocks /reload when the host is still admitting an earlier member. This fake
// host accepts the connection and never answers it, so a factory-time
// subscription shows up as exactly that accepted connection.
func TestExtensionDefersBusSubscriptionToOnLoaded(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	t.Setenv("PIG_EXT_SOCKET", sockPath)

	done := make(chan *sdk.Extension, 1)
	go func() { done <- Extension() }()

	select {
	case extension := <-done:
		if extension == nil {
			t.Fatal("Extension() returned nil")
		}
	case conn := <-accepted:
		_ = conn.Close()
		t.Fatal("Extension() opened the host socket from the factory: the bus subscription must be deferred to OnLoaded")
	case <-time.After(5 * time.Second):
		t.Fatal("Extension() did not return: a factory-time host call blocks the reload")
	}
}

// ── frame IO for the wire-level host harness ────────────────────────────────

// readWireFrame reads one length-prefixed JSON frame off the wire, matching the
// SDK conn's own framing (protocol.go).
func readWireFrame(r io.Reader) (map[string]any, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// writeWireFrame writes one length-prefixed JSON frame.
func writeWireFrame(w io.Writer, msg map[string]any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	_, err = w.Write(append(header[:], data...))
	return err
}

// TestAnswerDispatchVerdictRidesTheDispatchContext drives the real Extension()
// over a net.Pipe through the genuine wire path: register → ready → the
// OnLoaded ctx.Events().On subscription → an events.dispatch request carrying a
// rpiv:ask-user:answer payload → the verdict emit back to the host. The verdict
// must be bound to the dispatch's own request context: the emit call frame's
// parent_request_id has to equal the dispatch request id. With the pre-fix
// answers.Handle(payload) (a constructor-bound Extension.Events().Emit) the
// frame carries an empty parent_request_id and this test fails. Harness shape
// follows go/ctxmode/test_helpers_test.go's sdkHost.
func TestAnswerDispatchVerdictRidesTheDispatchContext(t *testing.T) {
	t.Setenv("PIG_EXT_SOCKET", "")
	// A temp HOME keeps the extension's LoadConfig deterministic: the host's
	// config store must not leak into the run.
	t.Setenv("HOME", t.TempDir())

	hostEnd, extEnd := net.Pipe()
	defer func() { _ = hostEnd.Close() }()
	runDone := make(chan error, 1)
	go func() { runDone <- Extension().RunWithConn(extEnd) }()

	// No frame should take longer than this; a hang anywhere fails the test
	// instead of stalling the suite.
	deadline := func() { _ = hostEnd.SetDeadline(time.Now().Add(15 * time.Second)) }
	defer func() { _ = hostEnd.SetDeadline(time.Time{}) }()
	deadline()

	next := func() map[string]any {
		deadline()
		msg, err := readWireFrame(hostEnd)
		if err != nil {
			t.Fatalf("read host frame: %v", err)
		}
		return msg
	}
	replyCall := func(id string) {
		t.Helper()
		if err := writeWireFrame(hostEnd, map[string]any{
			"type": "call_result", "id": id,
			"call_result": map[string]any{"result": nil},
		}); err != nil {
			t.Fatalf("answer host call %s: %v", id, err)
		}
	}

	// Handshake: the extension registers first, the host answers ready.
	reg := next()
	if reg["type"] != "register" {
		t.Fatalf("first frame = %v, want register", reg)
	}
	if err := writeWireFrame(hostEnd, map[string]any{
		"type":  "ready",
		"ready": map[string]any{"cwd": t.TempDir(), "mode": "tui", "width": 80},
	}); err != nil {
		t.Fatalf("send ready: %v", err)
	}

	const dispatchID = "req-dispatch-1"
	var (
		handlerID   string
		dispatched  bool
		responded   bool
		emitSeen    bool
		emitParent  string
		emitVerdict map[string]any
	)
	// Serve the wire until the dispatch was answered and the verdict emit was
	// captured. request_state frames and stray pong/notify frames carry no
	// payload the test needs and are skipped; every call is answered so the
	// extension never stalls waiting for the host.
	for !responded || !emitSeen {
		msg := next()
		switch msg["type"] {
		case "call":
			call, _ := msg["call"].(map[string]any)
			method, _ := call["method"].(string)
			args, _ := call["args"].(map[string]any)
			callID, _ := msg["id"].(string)
			switch method {
			case "events.on":
				if channel, _ := args["channel"].(string); channel == AskUserAnswerEvent {
					handlerID, _ = args["handlerId"].(string)
					if handlerID == "" {
						t.Fatalf("events.on for %s carried no handlerId: %v", channel, args)
					}
				}
				replyCall(callID)
				if handlerID != "" && !dispatched {
					dispatched = true
					// A dispatch the Host sends while the extension loads is
					// served without waiting for OnLoaded to finish
					// (serveLoadingEnvelope / the loop's loadedDone gate), so
					// sending it right after answering events.on exercises the
					// same ordering the real host produces.
					payload, err := json.Marshal(map[string]any{
						"toolCallId": "call-1",
						"requestId":  "req-9",
						"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
					})
					if err != nil {
						t.Fatalf("marshal answer payload: %v", err)
					}
					dispatchArgs, err := json.Marshal(map[string]any{
						"handlerId": handlerID,
						"channel":   AskUserAnswerEvent,
						"json":      json.RawMessage(payload),
					})
					if err != nil {
						t.Fatalf("marshal dispatch args: %v", err)
					}
					if err := writeWireFrame(hostEnd, map[string]any{
						"type": "request", "id": dispatchID,
						"request": map[string]any{
							"method": "events.dispatch",
							"args":   json.RawMessage(dispatchArgs),
						},
					}); err != nil {
						t.Fatalf("send dispatch: %v", err)
					}
				}
			case "events.emit":
				emitSeen = true
				emitParent, _ = call["parent_request_id"].(string)
				emitVerdict, _ = args["json"].(map[string]any)
				if channel, _ := args["channel"].(string); channel != AskUserAnswerResultEvent {
					t.Fatalf("verdict emitted on %q, want %q", channel, AskUserAnswerResultEvent)
				}
				replyCall(callID)
			default:
				replyCall(callID)
			}
		case "response":
			if id, _ := msg["id"].(string); id == dispatchID {
				responded = true
				if resp, _ := msg["response"].(map[string]any); resp["error"] != nil {
					t.Fatalf("dispatch response carried an error: %v", resp["error"])
				}
			}
		default:
			// request_state / notify / pong frames need no action.
		}
	}

	// The verdict rode the dispatch's request context: an unbound emit would
	// carry an empty parent_request_id.
	if emitParent != dispatchID {
		t.Fatalf("verdict emit parent_request_id = %q, want %q (the dispatch request id)", emitParent, dispatchID)
	}
	// And the verdict content is the channel's not_found reply for the unknown
	// tool call, proof the real answer-channel ran end to end.
	if emitVerdict == nil {
		t.Fatal("events.emit carried no verdict payload")
	}
	if ok, _ := emitVerdict["ok"].(bool); ok {
		t.Fatalf("verdict ok = true, want the not_found reject for the unopened tool call: %v", emitVerdict)
	}
	if code, _ := emitVerdict["code"].(string); code != string(RejectNotFound) {
		t.Fatalf("verdict code = %q, want %q: %v", code, RejectNotFound, emitVerdict)
	}
	if reqID, _ := emitVerdict["requestId"].(string); reqID != "req-9" {
		t.Fatalf("verdict requestId = %q, want req-9: %v", reqID, emitVerdict)
	}

	if err := writeWireFrame(hostEnd, map[string]any{"type": "shutdown"}); err != nil {
		t.Fatalf("send shutdown: %v", err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("RunWithConn returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("extension did not stop after shutdown")
	}
}

// ── wire-level host: full ask_user_question flows over net.Pipe ──────────────

// wireHost plays the host for the real Extension() over a net.Pipe.
//
// Concurrency model: one pump goroutine owns the read side of the pipe. It
// classifies each frame, auto-answers extension→host calls with the canned
// call_result the flow needs, and pushes an immutable wireEvent onto the
// events channel. The test goroutine is the only consumer: it records the
// handler id, verdict emits, mounted custom components and request responses
// as it drains events, so every piece of observed state has exactly one
// writer and one reader. Nothing is ever polled or slept on — waitEvent only
// drains the channel.
type wireHost struct {
	t      *testing.T
	conn   net.Conn
	runErr chan error

	events chan wireEvent
	sendMu sync.Mutex

	// pendingCustom is owned by the pump alone: ui.custom calls the host holds
	// until the matching ui.custom.close notify arrives, keyed by component
	// key.
	pendingCustom map[string]string

	// Observed state, owned by the test goroutine (written only while draining
	// events).
	answerHandlerID string
	verdicts        []wireEmit
	responses       map[string]map[string]any
	mountedKeys     []string
}

// wireEvent is one classified frame the pump produced. Only the test
// goroutine reads it.
type wireEvent struct {
	kind string // "call", "response", "notify", "other"

	// call frames:
	method  string
	callID  string
	parent  string // parent_request_id
	timer   bool
	channel string         // events.on / events.emit channel
	handler string         // events.on handlerId
	json    map[string]any // events.emit decoded json
	key     string         // ui.custom key
	overlay bool           // ui.custom overlay flag

	// response frames:
	reqID string
	msg   map[string]any // the whole response frame

	// notify frames (ui.custom.close):
	closeKey string
}

// wireEmit is one events.emit call the extension made.
type wireEmit struct {
	channel string
	parent  string
	timer   bool
	json    map[string]any
}

// newWireHost builds the extension, starts it over a pipe and runs the
// register→ready handshake.
func newWireHost(t *testing.T) *wireHost {
	t.Helper()
	t.Setenv("PIG_EXT_SOCKET", "")
	t.Setenv("HOME", t.TempDir())

	// These flows exercise the external-answer path, which closes a mounted
	// dialog through its OverlayHandle — a path only an overlay presentation
	// has. Holding the inline slot makes every mount an overlay, the
	// presentation a second concurrent questionnaire would get in production.
	release := claimInlineSlot()
	if release == nil {
		t.Fatal("the inline slot was already held")
	}
	t.Cleanup(release)

	hostEnd, extEnd := net.Pipe()
	h := &wireHost{
		t:             t,
		conn:          hostEnd,
		runErr:        make(chan error, 1),
		events:        make(chan wireEvent, 256),
		responses:     map[string]map[string]any{},
		pendingCustom: map[string]string{},
	}
	go func() { h.runErr <- Extension().RunWithConn(extEnd) }()

	reg := h.nextFrame()
	if reg["type"] != "register" {
		t.Fatalf("first frame = %v, want register", reg)
	}
	tools, _ := reg["register"].(map[string]any)["tools"].([]any)
	found := false
	for _, tool := range tools {
		if name, _ := tool.(map[string]any)["name"].(string); name == ToolName {
			found = true
		}
	}
	if !found {
		t.Fatalf("register carried no %s tool: %v", ToolName, tools)
	}
	// Ready carries a real TUI session: hasUI through the state snapshot.
	state, err := json.Marshal(map[string]any{"hasUI": true})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	h.send(map[string]any{
		"type": "ready",
		"ready": map[string]any{
			"cwd": t.TempDir(), "mode": "tui", "width": 80, "height": 40,
			"state": json.RawMessage(state),
		},
	})
	go h.pump()
	t.Cleanup(h.shutdown)
	return h
}

func (h *wireHost) deadline() { _ = h.conn.SetDeadline(time.Now().Add(15 * time.Second)) }

func (h *wireHost) nextFrame() map[string]any {
	h.t.Helper()
	h.deadline()
	msg, err := readWireFrame(h.conn)
	if err != nil {
		h.t.Fatalf("read host frame: %v", err)
	}
	return msg
}

func (h *wireHost) send(msg map[string]any) {
	h.t.Helper()
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	if err := writeWireFrame(h.conn, msg); err != nil {
		h.t.Fatalf("write host frame %v: %v", msg, err)
	}
}

// pump is the single reader of the extension's frames. It auto-answers every
// extension→host call (so the extension never stalls on the host) and hands a
// classified wireEvent to the test. ui.custom is answered {ok:false}: the
// dialog stays mounted until the component itself emits ui.custom.close
// (which is how an external answer closes it), so waitCall then sees the
// pending call end with no result — the genuine "host could not take the
// answer" path is not what is under test here.
func (h *wireHost) pump() {
	defer close(h.events)
	for {
		h.deadline()
		msg, err := readWireFrame(h.conn)
		if err != nil {
			return
		}
		switch msg["type"] {
		case "call":
			call, _ := msg["call"].(map[string]any)
			args, _ := call["args"].(map[string]any)
			ev := wireEvent{
				kind:    "call",
				method:  stringArg(call, "method"),
				callID:  stringArg(msg, "id"),
				parent:  stringArg(call, "parent_request_id"),
				timer:   call["timer"] == true,
				channel: stringArg(args, "channel"),
				handler: stringArg(args, "handlerId"),
				json:    mapArg(args, "json"),
				key:     stringArg(args, "key"),
			}
			ev.overlay = args["overlay"] == true
			switch ev.method {
			case "ui.custom":
				// The host holds ui.custom until the component closes: its
				// call_result goes out when the matching ui.custom.close
				// notify arrives below. That is what keeps the dialog mounted
				// while an external answer settles it.
				h.pendingCustom[ev.key] = ev.callID
			case "ui.custom.control":
				// hide disposes the component: the host ends its ui.custom
				// call with no result, which is what frees the mount. The
				// control itself answers with the component's last state.
				h.writeRaw(map[string]any{
					"type": "call_result", "id": ev.callID,
					"call_result": map[string]any{"result": map[string]any{
						"hidden": false, "focused": false, "visible": true,
					}},
				})
				if stringArg(args, "action") == "hide" {
					if pendingID, ok := h.pendingCustom[ev.key]; ok {
						delete(h.pendingCustom, ev.key)
						h.writeRaw(map[string]any{
							"type": "call_result", "id": pendingID,
							"call_result": map[string]any{"result": nil},
						})
					}
				}
			default:
				h.writeRaw(map[string]any{
					"type": "call_result", "id": ev.callID,
					"call_result": map[string]any{"result": nil},
				})
			}
			select {
			case h.events <- ev:
			default:
			}
		case "response":
			ev := wireEvent{kind: "response", reqID: stringArg(msg, "id"), msg: msg}
			select {
			case h.events <- ev:
			default:
			}
		case "notify":
			n, _ := msg["notify"].(map[string]any)
			ev := wireEvent{kind: "notify", method: stringArg(n, "method")}
			if stringArg(n, "method") == "ui.custom.close" {
				args, _ := n["args"].(map[string]any)
				ev.closeKey = stringArg(args, "key")
				// The component closed: end the pending ui.custom call as the
				// host does, with no component result — the answer already
				// travelled the answer channel.
				if callID, ok := h.pendingCustom[ev.closeKey]; ok {
					delete(h.pendingCustom, ev.closeKey)
					h.writeRaw(map[string]any{
						"type": "call_result", "id": callID,
						"call_result": map[string]any{"result": nil},
					})
				}
			}
			select {
			case h.events <- ev:
			default:
			}
		default:
			select {
			case h.events <- wireEvent{kind: "other", msg: msg}:
			default:
			}
		}
	}
}

// writeRaw answers a call from the pump without t.Helper — a closed pipe
// must not fail a test that already finished its assertions.
func (h *wireHost) writeRaw(msg map[string]any) {
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	_ = writeWireFrame(h.conn, msg)
}

func stringArg(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func mapArg(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// drain folds one pump event into the test-side observed state.
func (h *wireHost) drain(ev wireEvent) {
	switch ev.kind {
	case "call":
		switch ev.method {
		case "events.on":
			if ev.channel == AskUserAnswerEvent && ev.handler != "" {
				h.answerHandlerID = ev.handler
			}
		case "events.emit":
			if ev.channel == AskUserAnswerResultEvent {
				h.verdicts = append(h.verdicts, wireEmit{
					channel: ev.channel, parent: ev.parent, timer: ev.timer, json: ev.json,
				})
			}
		case "ui.custom":
			h.mountedKeys = append(h.mountedKeys, ev.key)
			// Tell the extension the component mounted, so its OnHandle
			// callback gets the handle an external answer closes through.
			h.send(map[string]any{
				"type": "notify",
				"notify": map[string]any{
					"method": "ui.custom.opened",
					"args":   map[string]any{"key": ev.key, "hidden": false, "focused": true, "visible": true},
				},
			})
		}
	case "response":
		h.responses[ev.reqID] = ev.msg
	}
}

// waitEvent drains events until pred holds, applying each one first. It is
// the only way the test observes the extension — a direct read of observed
// state only ever runs from the test goroutine, so there is nothing to race.
func (h *wireHost) waitEvent(what string, pred func() bool) {
	h.t.Helper()
	if pred() {
		return
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-h.events:
			if !ok {
				h.t.Fatalf("the extension closed the connection while waiting for %s", what)
			}
			h.drain(ev)
			if pred() {
				return
			}
		case <-timer.C:
			h.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// sendToolCall issues one ask_user_question tool_call request.
func (h *wireHost) sendToolCall(requestID, toolCallID string, args map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		h.t.Fatalf("marshal tool args: %v", err)
	}
	h.send(map[string]any{
		"type": "request", "id": requestID,
		"request": map[string]any{
			"method":       "tool_call",
			"tool":         ToolName,
			"tool_call_id": toolCallID,
			"args":         json.RawMessage(raw),
		},
	})
}

// sendAnswerDispatch issues one events.dispatch of rpiv:ask-user:answer under
// its own request id. payload is the event payload, already a JSON value.
func (h *wireHost) sendAnswerDispatch(requestID string, payload any) {
	h.t.Helper()
	if h.answerHandlerID == "" {
		h.t.Fatal("no answer handler: the extension never subscribed events.on")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("marshal answer payload: %v", err)
	}
	dispatchArgs, err := json.Marshal(map[string]any{
		"handlerId": h.answerHandlerID,
		"channel":   AskUserAnswerEvent,
		"json":      json.RawMessage(raw),
	})
	if err != nil {
		h.t.Fatalf("marshal dispatch args: %v", err)
	}
	h.send(map[string]any{
		"type": "request", "id": requestID,
		"request": map[string]any{
			"method": "events.dispatch",
			"args":   json.RawMessage(dispatchArgs),
		},
	})
}

// sendCancel cancels a host request the way the host does.
func (h *wireHost) sendCancel(requestID, reason string) {
	h.t.Helper()
	h.send(map[string]any{
		"type":   "cancel",
		"cancel": map[string]any{"request_id": requestID, "reason": reason},
	})
}

func (h *wireHost) subscribed() bool { return h.answerHandlerID != "" }
func (h *wireHost) mounted() bool    { return len(h.mountedKeys) > 0 }

// emitFor returns the recorded verdict emit for one dispatch request id.
func (h *wireHost) emitFor(dispatchID string) *wireEmit {
	for i := range h.verdicts {
		emit := &h.verdicts[i]
		if emit.parent == dispatchID {
			return emit
		}
	}
	return nil
}

func (h *wireHost) sawResponse(id string) bool        { _, ok := h.responses[id]; return ok }
func (h *wireHost) response(id string) map[string]any { return h.responses[id] }

// shutdown asks the extension to stop and waits for the serve loop to end.
func (h *wireHost) shutdown() {
	h.writeRaw(map[string]any{"type": "shutdown"})
	select {
	case err := <-h.runErr:
		if err != nil {
			h.t.Errorf("RunWithConn returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Error("extension did not stop after shutdown")
	}
	_ = h.conn.Close()
}

// oneQuestionArgs is the tool_call args of a one-question questionnaire.
func oneQuestionArgs(question string) map[string]any {
	return map[string]any{
		"questions": []any{map[string]any{
			"question": question,
			"header":   "Q",
			"options": []any{
				map[string]any{"label": "A", "description": "desc A"},
				map[string]any{"label": "B", "description": "desc B"},
			},
		}},
	}
}

// answerPayload builds an inbound rpiv:ask-user:answer payload answering the
// single question with the given option index.
func answerPayload(toolCallID, requestID string, optionIndex int) map[string]any {
	return map[string]any{
		"toolCallId": toolCallID,
		"requestId":  requestID,
		"answers":    []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{optionIndex}}},
	}
}

// toolResultDetails decodes the details member of a tool_call response result.
func toolResultDetails(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	resp, _ := response["response"].(map[string]any)
	if resp == nil {
		t.Fatalf("response frame has no response member: %v", response)
	}
	if resp["error"] != nil {
		t.Fatalf("tool_call response carried an error: %v", resp["error"])
	}
	raw, _ := resp["result"].(json.RawMessage)
	if len(raw) == 0 {
		// The test rig decodes frames into map[string]any; the result arrives
		// as a map then.
		if m, ok := resp["result"].(map[string]any); ok {
			return mapArg(m, "details")
		}
		t.Fatalf("tool_call response carried no result: %v", resp)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return mapArg(result, "details")
}

// TestWireAnswerClosesTheQuestionnaire drives the full flow at the wire with a
// nil-constructor channel: tool_call → ui.custom mounts → an answer dispatch
// settles the questionnaire → the verdict rides the dispatch's request context
// and the tool result returns the accepted answer.
func TestWireAnswerClosesTheQuestionnaire(t *testing.T) {
	h := newWireHost(t)
	h.waitEvent("the answer-channel subscription", h.subscribed)

	h.sendToolCall("req-tool-1", "call-a", oneQuestionArgs("Pick one?"))
	// The TUI path mounts the questionnaire through ctx.ui.custom; the host
	// call reaching us proves the dialog mounted (and stayed pending).
	h.waitEvent("the ui.custom mount", h.mounted)

	h.sendAnswerDispatch("req-dispatch-a", answerPayload("call-a", "req-a", 0))
	h.waitEvent("the verdict emit of req-dispatch-a", func() bool {
		return h.emitFor("req-dispatch-a") != nil
	})
	h.waitEvent("the tool_call response", func() bool { return h.sawResponse("req-tool-1") })

	emit := h.emitFor("req-dispatch-a")
	if emit == nil {
		t.Fatal("no verdict emit rode req-dispatch-a")
	}
	if emit.timer {
		t.Fatalf("the verdict emit was detached (timer): %v", emit)
	}
	verdict := emit.json
	if ok, _ := verdict["ok"].(bool); !ok {
		t.Fatalf("verdict = %v, want ok:true", verdict)
	}
	if got := stringArg(verdict, "toolCallId"); got != "call-a" {
		t.Fatalf("verdict toolCallId = %q, want call-a: %v", got, verdict)
	}
	if got := stringArg(verdict, "requestId"); got != "req-a" {
		t.Fatalf("verdict requestId = %q, want req-a: %v", got, verdict)
	}

	details := toolResultDetails(t, h.response("req-tool-1"))
	if cancelled, _ := details["cancelled"].(bool); cancelled {
		t.Fatalf("tool result cancelled: %v", details)
	}
	answers, _ := details["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("answers = %v, want exactly one", details)
	}
	first, _ := answers[0].(map[string]any)
	if got := stringArg(first, "kind"); got != string(AnswerKindOption) {
		t.Fatalf("answer kind = %q, want option: %v", got, first)
	}
	if got := stringArg(first, "answer"); got != "A" {
		t.Fatalf("answer = %q, want the picked label A: %v", got, first)
	}
}

// TestWireConcurrentCallsAnswerByToolCallID opens two questionnaires
// concurrently (call-a and call-b, sent without a batch so both start at
// once) and answers them through independent dispatches in reverse order:
// call-b settles before call-a. Each verdict must ride its own dispatch
// request and each tool result must carry its own answer — zero cross-talk.
func TestWireConcurrentCallsAnswerByToolCallID(t *testing.T) {
	h := newWireHost(t)
	h.waitEvent("the answer-channel subscription", h.subscribed)

	h.sendToolCall("req-tool-a", "call-a", oneQuestionArgs("Question A?"))
	h.sendToolCall("req-tool-b", "call-b", oneQuestionArgs("Question B?"))
	h.waitEvent("both ui.custom mounts", func() bool { return len(h.mountedKeys) >= 2 })

	// Answer call-b first while call-a is still open: the dispatch is served
	// concurrently with the still-pending tool_call of call-a.
	h.sendAnswerDispatch("req-dispatch-b", answerPayload("call-b", "req-b", 1))
	h.waitEvent("the verdict emit of req-dispatch-b", func() bool {
		return h.emitFor("req-dispatch-b") != nil
	})
	h.sendAnswerDispatch("req-dispatch-a", answerPayload("call-a", "req-a", 0))
	h.waitEvent("the verdict emit of req-dispatch-a", func() bool {
		return h.emitFor("req-dispatch-a") != nil
	})
	h.waitEvent("both tool_call responses", func() bool {
		return h.sawResponse("req-tool-a") && h.sawResponse("req-tool-b")
	})

	for _, tc := range []struct {
		dispatchID, toolCallID, requestID string
		wantLabel                         string
	}{
		{"req-dispatch-a", "call-a", "req-a", "A"},
		{"req-dispatch-b", "call-b", "req-b", "B"},
	} {
		emit := h.emitFor(tc.dispatchID)
		if emit == nil {
			t.Fatalf("no verdict emit rode %s", tc.dispatchID)
		}
		if got := stringArg(emit.json, "toolCallId"); got != tc.toolCallID {
			t.Fatalf("verdict on %s carried toolCallId %q, want %s: %v", tc.dispatchID, got, tc.toolCallID, emit.json)
		}
		if got := stringArg(emit.json, "requestId"); got != tc.requestID {
			t.Fatalf("verdict on %s carried requestId %q, want %s: %v", tc.dispatchID, got, tc.requestID, emit.json)
		}
		if ok, _ := emit.json["ok"].(bool); !ok {
			t.Fatalf("verdict on %s = %v, want ok:true", tc.dispatchID, emit.json)
		}
	}

	for _, tc := range []struct {
		requestID, wantLabel string
	}{
		{"req-tool-a", "A"},
		{"req-tool-b", "B"},
	} {
		details := toolResultDetails(t, h.response(tc.requestID))
		answers, _ := details["answers"].([]any)
		if len(answers) != 1 {
			t.Fatalf("%s answers = %v, want exactly one", tc.requestID, details)
		}
		first, _ := answers[0].(map[string]any)
		if got := stringArg(first, "answer"); got != tc.wantLabel {
			t.Fatalf("%s answer = %q, want %q: cross-talk: %v", tc.requestID, got, tc.wantLabel, details)
		}
	}
}

// TestWireLateAnswerAfterCancelEmitsNotFound cancels the tool_call request
// after the questionnaire opened, then delivers a late answer for it. The
// registry already closed the questionnaire, so the dispatch must still emit
// its verdict — not_found, carrying the answer's requestId — and it must ride
// the dispatch's own request context even though the asking context is gone.
func TestWireLateAnswerAfterCancelEmitsNotFound(t *testing.T) {
	h := newWireHost(t)
	h.waitEvent("the answer-channel subscription", h.subscribed)

	h.sendToolCall("req-tool-1", "call-a", oneQuestionArgs("Pick one?"))
	h.waitEvent("the ui.custom mount", h.mounted)

	// The host aborts the tool call (the user interrupted the turn). The
	// handler's context is cancelled; the questionnaire closes with the
	// request's cleanup.
	h.sendCancel("req-tool-1", "user interrupt")
	h.waitEvent("the cancelled tool_call response", func() bool { return h.sawResponse("req-tool-1") })

	h.sendAnswerDispatch("req-dispatch-late", answerPayload("call-a", "req-late", 0))
	h.waitEvent("the verdict emit of req-dispatch-late", func() bool {
		return h.emitFor("req-dispatch-late") != nil
	})

	emit := h.emitFor("req-dispatch-late")
	if emit == nil {
		t.Fatal("no verdict emit rode req-dispatch-late")
	}
	if emit.timer {
		t.Fatalf("the verdict emit was detached (timer): %v", emit)
	}
	verdict := emit.json
	if ok, _ := verdict["ok"].(bool); ok {
		t.Fatalf("verdict ok = true, want the not_found reject: %v", verdict)
	}
	if got := stringArg(verdict, "code"); got != string(RejectNotFound) {
		t.Fatalf("verdict code = %q, want %q: %v", got, RejectNotFound, verdict)
	}
	if got := stringArg(verdict, "requestId"); got != "req-late" {
		t.Fatalf("verdict requestId = %q, want req-late: %v", got, verdict)
	}
	if got := stringArg(verdict, "toolCallId"); got != "call-a" {
		t.Fatalf("verdict toolCallId = %q, want call-a: %v", got, verdict)
	}
}

// TestWireInvalidPayloadEmitsInvalidPayloadVerdict dispatches malformed
// rpiv:ask-user:answer payloads — a missing toolCallId, and a non-object — and
// asserts each gets its own invalid_payload verdict bound to its own dispatch.
func TestWireInvalidPayloadEmitsInvalidPayloadVerdict(t *testing.T) {
	h := newWireHost(t)
	h.waitEvent("the answer-channel subscription", h.subscribed)

	h.sendAnswerDispatch("req-dispatch-bad-1", map[string]any{
		"requestId": "req-bad-1",
		"answers":   []any{map[string]any{"questionIndex": 0, "optionIndexes": []any{0}}},
	})
	h.waitEvent("the verdict emit of req-dispatch-bad-1", func() bool {
		return h.emitFor("req-dispatch-bad-1") != nil
	})

	// A non-object payload crosses the bus as its JSON value.
	h.sendAnswerDispatch("req-dispatch-bad-2", "not-an-object")
	h.waitEvent("the verdict emit of req-dispatch-bad-2", func() bool {
		return h.emitFor("req-dispatch-bad-2") != nil
	})

	emit := h.emitFor("req-dispatch-bad-1")
	if got := stringArg(emit.json, "code"); got != string(RejectInvalidPayload) {
		t.Fatalf("missing-toolCallId verdict = %v, want code %q", emit.json, RejectInvalidPayload)
	}
	if got := stringArg(emit.json, "requestId"); got != "req-bad-1" {
		t.Fatalf("verdict requestId = %q, want req-bad-1: %v", got, emit.json)
	}

	emit = h.emitFor("req-dispatch-bad-2")
	if emit.timer {
		t.Fatalf("the verdict emit was detached (timer): %v", emit)
	}
	if got := stringArg(emit.json, "code"); got != string(RejectInvalidPayload) {
		t.Fatalf("non-object verdict = %v, want code %q", emit.json, RejectInvalidPayload)
	}
}
