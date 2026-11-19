package emphasis

import (
	"os"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Version is the Go port's release, per ../../VERSIONING.md.
const Version = "0.1.0"

// Extension is the factory a generated PiG runner calls (upstream's default
// emphasis(pi) export). It wires the same three hooks:
//   - session_start captures the live theme (TUI only);
//   - before_agent_start adds the reply_style system-prompt section;
//   - the Markdown transformer recolors assistant replies.
func Extension() *sdk.Extension {
	e := sdk.New("pi-emphasis")
	st := &emphasisState{}
	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		if ctx.Mode() != "tui" {
			// A session switching away from the TUI (or starting non-TUI
			// after one) drops the theme, the cached palette and any
			// lingering OnThemeChange subscription, so no stale colors or
			// reply_style rules survive into a non-TUI session.
			st.clearTheme()
			return nil, nil
		}
		// Recolor immediately on /theme switches instead of waiting for the
		// next transform's fingerprint check. The session's generation is
		// taken with the binding so a subscription that arrives after a
		// concurrent clearTheme/setThemeFunc never lands on a stale state.
		sessionGen := st.setThemeFunc(func() sdk.UITheme { return ctx.UITheme() })
		unsub, _ := ctx.OnThemeChange(func(_ sdk.Context, _ sdk.UITheme) { st.invalidate() })
		st.setUnsubTheme(sessionGen, unsub)
		return nil, nil
	})
	e.OnEvent(sdk.EventBeforeAgentStart, func(_ sdk.Context, data map[string]any) (any, error) {
		options, _ := data["systemPromptOptions"].(map[string]any)
		if options == nil {
			return nil, nil
		}
		sections, ok := options["sections"].(*sdk.SystemPromptSections)
		if !ok || sections == nil {
			return nil, nil
		}
		// The rules travel with the renderer: only a session that draws pills
		// is told to write them.
		if st.hasTheme() && os.Getenv("PI_EMPHASIS_GUIDANCE") != "0" {
			sections.Set("reply_style", ReplyGuidance())
		} else {
			sections.Delete("reply_style")
		}
		return nil, nil
	})
	e.MarkdownTransformer(func(markdown string, context sdk.MarkdownTransformContext) string {
		return st.transform(markdown, context)
	})
	return e
}

// emphasisState is the extension instance's mutable state. The host may call
// the transformer and the event handlers concurrently, so every field is
// guarded by mu. Two orthogonal generation counters supersede one shared
// one, because an invalidate() is not a session change:
//   - sessionGen advances only when the session binding changes
//     (setThemeFunc, clearTheme); it is the generation setUnsubTheme checks,
//     so a theme-change invalidate between setThemeFunc and the store of a
//     just-registered OnThemeChange unsubscribe does not retire the current
//     session's subscription.
//   - paletteGen advances on every mutation that affects the palette cache
//     (setThemeFunc, clearTheme, invalidate); paletteFor's lock-free build
//     writes s.built back only when it still holds.
type emphasisState struct {
	mu         sync.RWMutex
	sessionGen uint64
	paletteGen uint64
	themeFunc  func() sdk.UITheme
	unsubTheme func()
	built      *builtPalette
	// onBuildPalette, when non-nil, is invoked once per palette build between
	// the read-lock snapshot and CreatePalette. Production leaves it nil; a
	// test sets it to park a build across a concurrent mutation.
	onBuildPalette func()
	// onAfterThemeRead, when non-nil, is invoked once per transform between
	// theme := f() and paletteFor. Production leaves it nil; a test parks a
	// transform across a concurrent rebind to prove the paletteGen snapshot
	// travels with the theme read.
	onAfterThemeRead func()
}

// builtPalette caches the palette of one theme fingerprint.
type builtPalette struct {
	fingerprint string
	palette     Palette
}

// setThemeFunc binds a session's theme reader. It also releases the
// previous OnThemeChange subscription so repeated session_start events
// (startup, /new, resume) cannot accumulate callbacks on the host feed. It
// returns the generation this binding was installed under, which the caller
// hands back to setUnsubTheme so a subscription registered after a
// concurrent state change is released instead of stored.
func (s *emphasisState) setThemeFunc(f func() sdk.UITheme) uint64 {
	s.mu.Lock()
	s.sessionGen++
	s.paletteGen++
	old := s.unsubTheme
	s.unsubTheme = nil
	s.themeFunc = f
	s.built = nil
	sessionGen := s.sessionGen
	s.mu.Unlock()
	if old != nil {
		old()
	}
	return sessionGen
}

// setUnsubTheme stores the unsubscribe of the subscription session_start
// just made, releasing the one it replaces (idempotent per the SDK). The
// store only lands when the state is still the generation sessionGen and a
// theme binding exists: a subscription registered while clearTheme or a
// newer setThemeFunc interleaved between it and setUnsubTheme would
// otherwise linger on the host's theme feed, so it is released immediately.
func (s *emphasisState) setUnsubTheme(sessionGen uint64, unsub func()) {
	s.mu.Lock()
	stale := s.sessionGen != sessionGen || s.themeFunc == nil
	var old func()
	if !stale {
		old = s.unsubTheme
		s.unsubTheme = unsub
	}
	s.mu.Unlock()
	if stale {
		if unsub != nil {
			unsub()
		}
		return
	}
	if old != nil {
		old()
	}
}

// clearTheme drops the session's theme binding and its subscription: a
// session_start in a non-TUI mode must not keep drawing with the theme of a
// TUI session that came before it.
func (s *emphasisState) clearTheme() {
	s.mu.Lock()
	s.sessionGen++
	s.paletteGen++
	old := s.unsubTheme
	s.unsubTheme = nil
	s.themeFunc = nil
	s.built = nil
	s.mu.Unlock()
	if old != nil {
		old()
	}
}

func (s *emphasisState) hasTheme() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.themeFunc != nil
}

// invalidate drops the cached palette so the next transform rebuilds it
// from the current theme. Bumping paletteGen also retires any palette a
// concurrent transform is still building against the old theme outside the
// lock: its guarded write-back sees the generation moved and does not
// resurrect the stale colors. sessionGen is untouched: an invalidate must
// not retire the session's theme subscription.
func (s *emphasisState) invalidate() {
	s.mu.Lock()
	s.paletteGen++
	s.built = nil
	s.mu.Unlock()
}

// transform is the registered MarkdownTransformerFunc (upstream
// registerMarkdownTransformer callback).
func (s *emphasisState) transform(markdown string, context sdk.MarkdownTransformContext) string {
	if markdown == "" {
		return markdown // O(1) empty fast path
	}
	if context.MessageType != "assistant" {
		return markdown
	}
	s.mu.RLock()
	f := s.themeFunc
	snapPaletteGen := s.paletteGen
	onAfterTheme := s.onAfterThemeRead
	s.mu.RUnlock()
	if f == nil {
		return markdown
	}
	theme := f()
	if onAfterTheme != nil {
		onAfterTheme()
	}
	palette, ok := s.paletteFor(theme, snapPaletteGen)
	if !ok {
		// The host has not sent a usable palette yet (no mdLink token): hand
		// the Markdown back untouched rather than panic on Fg.
		return markdown
	}
	return Decorate(markdown, palette)
}

// paletteFor returns the cached palette for theme, rebuilding it when the
// theme fingerprint changed (name + color mode + the tokens this port draws
// with). snapPaletteGen is the paletteGen the caller snapshotted together
// with its theme read: the write-back below must compare against that
// earlier snapshot, not a fresh read, or a rebind landing between
// theme := f() and this call would let the stale theme's build overwrite
// the newer cache. The second return is false when the theme cannot
// resolve the configured bold color.
func (s *emphasisState) paletteFor(theme sdk.UITheme, snapPaletteGen uint64) (Palette, bool) {
	fingerprint := themeFingerprint(theme)
	s.mu.RLock()
	if s.built != nil && s.built.fingerprint == fingerprint {
		p := s.built.palette
		s.mu.RUnlock()
		return p, true
	}
	onBuild := s.onBuildPalette
	s.mu.RUnlock()
	if onBuild != nil {
		onBuild()
	}

	boldColor := os.Getenv("PI_EMPHASIS_COLOR")
	if boldColor == "" {
		boldColor = "mdLink"
	}
	// CreatePalette validates the token through GetFgAnsi before Fg ever sees
	// it (an unknown PI_EMPHASIS_COLOR falls back to "mdLink"); a nil Bold
	// means the host has not sent a usable palette yet, and the transform
	// hands the Markdown back untouched rather than panic on Fg.
	palette := CreatePalette(theme, boldColor)
	if palette.Bold == nil {
		return Palette{}, false
	}
	// The write-back only lands when the state is still the generation the
	// build started under and a session binding exists: invalidate or a
	// clearTheme/new setThemeFunc interleaved during the lock-free build
	// bumps gen, and without the guard this call would overwrite the newer
	// cache (or resurrect a cleared state) with the old theme's palette.
	s.mu.Lock()
	if s.paletteGen == snapPaletteGen && s.themeFunc != nil {
		s.built = &builtPalette{fingerprint: fingerprint, palette: palette}
	}
	s.mu.Unlock()
	return palette, true
}

// themeFingerprint identifies the palette a theme produces: its name, the
// color mode, the escapes of every token CreatePalette reads, and the
// environment CreatePalette reads (PI_EMPHASIS_COLOR picks the bold token,
// PI_EMPHASIS_ROUND_CAPS picks the pill caps), so changing either one
// rebuilds the palette instead of drawing stale colors. The custom bold
// token is outside the fixed list, so when PI_EMPHASIS_COLOR names one its
// own resolved foreground is fingerprinted too: two themes that share the
// name and the fixed tokens but resolve "muted" (say) differently do not
// collide. Two themes with the same fingerprint draw identically.
func themeFingerprint(theme sdk.UITheme) string {
	var b strings.Builder
	b.WriteString(theme.Name)
	b.WriteByte('|')
	b.WriteString(theme.GetColorMode())
	b.WriteByte('|')
	if boldColor := os.Getenv("PI_EMPHASIS_COLOR"); boldColor != "" {
		b.WriteString(boldColor)
		b.WriteByte('=')
		ansi, _ := theme.GetFgAnsi(boldColor)
		b.WriteString(ansi)
	}
	b.WriteByte('|')
	b.WriteString(os.Getenv("PI_EMPHASIS_ROUND_CAPS"))
	for _, token := range []string{"mdLink", "success", "error", "warning", "accent", "dim", "text"} {
		ansi, _ := theme.GetFgAnsi(token)
		b.WriteByte('|')
		b.WriteString(ansi)
	}
	for _, token := range []string{"toolSuccessBg", "toolErrorBg", "searchMatchBg", "selectedBg"} {
		ansi, _ := theme.GetBgAnsi(token)
		b.WriteByte('|')
		b.WriteString(ansi)
	}
	return b.String()
}

// pills renders words as [[w]] joined by ", " (upstream pills()).
func pillsJoined(words []string) string {
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = "[[" + w + "]]"
	}
	return strings.Join(parts, ", ")
}

// titled renders words with an uppercased first rune joined by ", "
// (upstream titled()).
func titledJoined(words []string) string {
	parts := make([]string, len(words))
	for i, w := range words {
		if w != "" {
			parts[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(parts, ", ")
}

func vocabulary(tone PillTone) VocabularyEntry {
	for _, entry := range Vocabulary {
		if entry.Tone == tone {
			return entry
		}
	}
	return VocabularyEntry{Tone: tone}
}

// ReplyGuidance is the reply-style rules given to the model, with every word
// list read from the tables that draw them (upstream replyGuidance, verbatim).
func ReplyGuidance() string {
	labelWords := make([]string, 0, len(labelIcons))
	for _, e := range labelIcons {
		labelWords = append(labelWords, e.word)
	}
	actionWords := make([]string, 0, len(actionIcons))
	for _, e := range actionIcons {
		actionWords = append(actionWords, e.word)
	}
	return `These rules shape replies written to the user in this terminal. It colors pills, adds icons, and cannot change text size.

- **Label every reply.** Start the first substantive line with a bold label, even for a one-line update, and give each subject in a multi-part reply a short heading or a bullet with a bold lead label. Write the label as a plain word; the terminal adds its icon. Never type icon glyphs. Labels with an icon: ` + titledJoined(labelWords) + `. Prefer these; any other label shows without an icon.
- **Shape follows information.** Use ` + "`├──`" + ` and ` + "`└──`" + ` for what belongs to what, ` + "`→`" + ` for a sequence, ` + "`✓`" + ` or ` + "`✗`" + ` beside a checked item. Write tree rows as indented lines directly under their parent with no list dash, one line per row. A one-line answer can stay one line.
- **Bold the anchor terms.** Treat the projects, components, choices, options, and statuses under discussion as the nodes of a graph, and bold them where they carry the point: a first mention in a section, a bullet's lead label such as **Cause:**, and the recommendation or decision the user must make. One to three bold spans per paragraph or bullet; bolding every noun erases the signal. Italicize sparingly, for contrast and meaning-changing words such as *not*, *only*, or *unverified*.
- **Pills are required, not optional.** A pill is ` + "`[[Word]]`" + ` beside the subject it describes; the terminal colors it and adds its icon from the word. A pill names a state or an ask, never an adjective. Use exactly these words so the same state always reads the same:
  - **Red, something is wrong:** ` + pillsJoined(vocabulary(ToneError).Core) + `. Every failure and blocker carries one.
  - **Yellow, look here:** ` + pillsJoined(vocabulary(ToneWarning).Core) + `. [[Needs you]] is an action only the user can take; [[Decision]] is a call only the user makes. Every ask and every unchecked claim carries one.
  - **Green, settled:** ` + pillsJoined(vocabulary(ToneSuccess).Core) + `.
  - **Solid red, consequences:** ` + pillsJoined(vocabulary(ToneDanger).Core) + `. Required beside any action that has one, before taking it.
  - **Cyan, kind of work:** open every substantive reply with the kind of work you understood the request to be: ` + pillsJoined(vocabulary(ToneMode).Core) + `. Brainstorm is options and a recommendation, Investigate is evidence, Preview and Implement are a working result, Qualify is rigorous proof.
  - **Blue, ownership:** ` + pillsJoined(vocabulary(ToneOwner).Core) + ` beside any program, file, service, or configuration you changed or propose to change outside the task's own files: created by this task, also used by others, or belonging to someone else.
  - **Soft blue, context:** ` + pillsJoined(ContextWords) + `, and any other short label.
- **Pill placement.** Mark evidence claims with [[Observed]] (witnessed in the real system), [[Inferred]] (reasoned from other signals), or [[Unverified]] (not checked). A known word may carry short detail: [[Passed 12/12]], [[Failed: 3 tests]]. One pill per bullet or table cell, with two exceptions: a sequence line joined by ` + "`→`" + `, and a scoreboard, which is one line of counts such as [[Done 6]] [[Failed 1]] [[Needs you 1]] under the opening sentence of a long report. In a long reply, repeat every [[Needs you]] and [[Decision]] in a closing line. Never put pills in headings, and do not invent new status words.
- **Every attention pill carries its action.** Follow the pill with the one fact the user needs to act on it, introduced by a bold lead-in word ending in a colon. These lead-ins get an icon: ` + titledJoined(actionWords) + `. Required for every red, yellow, and solid red pill and for [[Inferred]]; optional for green ones.
  - [[Unverified]] means you *could not* check. If you can run the check yourself, run it and report the result instead. Otherwise give the fastest check and who can run it: "the new color on your screen. **Check:** reload and look at the first pill."
  - [[Needs you]]: the exact action and what it unblocks. "**Do:** sign in to the staging dashboard; then I can read the deploy log."
  - [[Decision]]: the options, your pick, and what you will do meanwhile. "Backfill, or make the column optional? I'd backfill. **Next:** I'll prepare it as a dry run."
  - [[Failed]] or [[Broken]]: the cause and what you are doing about it. "Migration stopped on 41 empty rows. **Next:** preparing a backfill."
  - [[Blocked]]: what blocks it and the smallest thing that unblocks it. "No access to the billing repository. **Unblock:** read access for this session."
  - [[Partial]] or [[In progress]]: what remains. "3 of 5 pages converted. **Left:** pricing and checkout."
  - [[Skipped]]: why, and how to run it anyway. "Load test, which you waived. **Run:** ` + "`bun run load`" + `."
  - [[At risk]] or [[Unsafe]]: what would make it fail and the guard against it. "Breaks if billing reads the old column. **Guard:** keep both columns for one release."
  - [[Costs money]]: the amount, before acting. "About $4 for 20 video generations."
  - [[Destructive]] or [[Irreversible]]: what is lost and how to get it back, or that nothing can. "Deletes 41 order rows. **Undo:** none; export them first."
  - [[Shared]] or [[Foreign]]: who else is affected. "Every session picks this up on its next reload."
  - [[Inferred]]: what it was inferred from. "From the last three deploy logs; I did not reproduce it."
  - [[Done]], [[Fixed]], or [[Live]]: where to see it. "**See:** the address, file, or command that shows it."
- **Headings and code.** Use ` + "`##`" + ` headings; ` + "`###`" + ` and deeper show their hash marks. Reserve inline code for literals the user may copy or search (paths, commands, exact identifiers); otherwise name subjects in plain words. Blockquotes render dim: asides only, never a caveat the user must see.
- **Copyable text stays clean.** Keep pills out of paths, commands, code, literal output, and machine-readable data. Omit pills and tree shapes when the user asks for plain text, ASCII, or an exact format.`
}
