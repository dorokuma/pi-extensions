package rpivaskuserquestion

import (
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
