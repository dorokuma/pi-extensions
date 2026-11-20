// Package emphasis is a Go port of johnlindquist/pi-emphasis (v0.2.0, commit
// f884b736c489944ee778ab951c7f78580f137b02): display-only emphasis for
// assistant replies in PiG's terminal.
//
//	**anchor term**   bold keeps its weight and takes a theme color (link blue by default)
//	✓ / ✗             take the success and error colors
//	[[Blocked]]       becomes a rounded status pill colored by its word, with that word's icon
//
// Stored messages, copied text, and HTML exports keep the original Markdown.
// In the terminal interface it also adds a reply_style section to the system
// prompt, telling the model how to write these (PI_EMPHASIS_GUIDANCE=0 turns
// that off).
package emphasis

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Colorize wraps text in a color (upstream's Theme.fg style colorizer).
type Colorize func(text string) string

// PillTone is the color family a [[word]] pill takes.
type PillTone string

const (
	ToneSuccess PillTone = "success"
	ToneError   PillTone = "error"
	ToneWarning PillTone = "warning"
	ToneDanger  PillTone = "danger"
	ToneMode    PillTone = "mode"
	ToneOwner   PillTone = "owner"
	ToneNeutral PillTone = "neutral"
)

// PillStyle holds the opening escapes for one pill tone: the label colors and
// the cap color.
type PillStyle struct {
	Label string
	Cap   string
}

// Palette is the colorizer set Decorate draws with (upstream's Palette).
type Palette struct {
	// Bold wraps bold text in a color.
	Bold Colorize
	// Passed colors ✓ and ✔.
	Passed Colorize
	// Failed colors ✗ and ✘.
	Failed Colorize
	// Pills holds the opening escapes for each pill tone.
	Pills map[PillTone]PillStyle
	// Arrow colors →.
	Arrow Colorize
	// Tree colors ├─ and └─ connectors.
	Tree Colorize
	// Heading is the mdHeading foreground escape a heading-line pill
	// reopens together with the bold weight when more title text follows
	// (the host's headingInline opened both before the line reached
	// Decorate; a pill's own SGR close drops them). Empty leaves the
	// reopen to the weight alone.
	Heading string
	// RoundCaps draws rounded Nerd Font end caps; false draws square chips
	// with a space on each side.
	RoundCaps bool
}

// ThemeLike is the minimal slice of PiG's sdk.UITheme used to build a
// palette. sdk.UITheme satisfies it; tests substitute a fake.
type ThemeLike interface {
	// Fg colors text with a token's foreground (panics on unknown tokens).
	Fg(color, text string) string
	// GetFgAnsi returns the token's foreground escape, or an error.
	GetFgAnsi(color string) (string, error)
	// GetBgAnsi returns the token's background escape, or an error.
	GetBgAnsi(color string) (string, error)
}

// VocabularyEntry lists one tone's words. Core are the words agents are told
// to write; Also are accepted and colored the same.
type VocabularyEntry struct {
	Tone PillTone
	Core []string
	Also []string
}

// Vocabulary lists each tone's words in upstream VOCABULARY insertion order
// (error, warning, success, danger, mode, owner). A slice, not a map: Go map
// iteration order is randomized and ReplyGuidance must render verbatim.
var Vocabulary = []VocabularyEntry{
	{ToneError, []string{"Failed", "Blocked", "Broken", "Unsafe"}, []string{"fail", "failing", "error", "rejected", "down"}},
	{ToneWarning, []string{"Needs you", "Decision", "Unverified", "Partial", "Skipped", "In progress", "Pending", "At risk"}, []string{"needs review", "waiting", "review", "todo", "stale", "draft"}},
	{ToneSuccess, []string{"Done", "Passed", "Observed", "Fixed", "Live", "Recommended", "Keep"}, []string{"pass", "ready", "merged", "safe", "verified", "shipped", "ok", "approved", "complete", "completed", "finished", "healthy"}},
	{ToneDanger, []string{"Costs money", "Destructive", "Irreversible"}, nil},
	{ToneMode, []string{"Brainstorm", "Investigate", "Preview", "Implement", "Qualify"}, nil},
	{ToneOwner, []string{"Owned", "Shared", "Foreign"}, nil},
}

// ContextWords are soft blue like any unknown label, but have an icon and a
// place in the guidance.
var ContextWords = []string{"Inferred", "New", "Changed", "Optional"}

// toneWords is TONE_WORDS: each tone's lowercased words (core + also), built
// once from Vocabulary.
var toneWords = func() map[PillTone][]string {
	m := make(map[PillTone][]string, len(Vocabulary))
	for _, entry := range Vocabulary {
		words := make([]string, 0, len(entry.Core)+len(entry.Also))
		for _, w := range entry.Core {
			words = append(words, strings.ToLower(w))
		}
		for _, w := range entry.Also {
			words = append(words, strings.ToLower(w))
		}
		m[entry.Tone] = words
	}
	return m
}()

// Font Awesome glyphs from the Nerd Font set: one cell wide in every terminal.
const (
	glyphCheck = "\uf00c"
	glyphCross = "\uf00d"
	glyphClock = "\uf017"
)

// iconEntry keeps insertion order so ReplyGuidance renders the label and
// action word lists verbatim (upstream Object.keys order).
type iconEntry struct {
	word string
	icon string
}

// pillIcons is the icon each known pill word carries (upstream PILL_ICONS).
// Words without an entry stay text-only.
var pillIcons = []iconEntry{
	{"done", glyphCheck}, {"passed", glyphCheck}, {"pass", glyphCheck}, {"complete", glyphCheck}, {"completed", glyphCheck}, {"ok", glyphCheck}, {"verified", glyphCheck},
	{"observed", "\uf06e"}, {"recommended", "\uf005"}, {"fixed", "\uf0ad"}, {"live", "\uf135"}, {"shipped", "\uf135"}, {"approved", "\uf164"},
	{"failed", glyphCross}, {"fail", glyphCross}, {"failing", glyphCross}, {"error", glyphCross}, {"rejected", glyphCross},
	{"blocked", "\uf05e"}, {"broken", "\uf127"}, {"unsafe", "\uf132"}, {"down", "\uf063"},
	{"needs you", "\uf007"}, {"decision", "\uf126"}, {"unverified", "\uf128"}, {"at risk", "\uf071"},
	{"in progress", "\uf110"}, {"pending", glyphClock}, {"waiting", glyphClock}, {"partial", "\uf042"}, {"skipped", "\uf04e"},
	{"inferred", "\uf0eb"}, {"new", "\uf067"}, {"changed", "\uf040"}, {"optional", "\uf10c"}, {"costs money", "\uf155"},
	{"destructive", "\uf1f8"}, {"irreversible", "\uf023"}, {"review", "\uf06e"}, {"stale", "\uf1da"}, {"draft", "\uf044"}, {"safe", "\uf132"},
	{"brainstorm", "\uf0eb"}, {"investigate", "\uf002"}, {"preview", "\uf06e"}, {"implement", "\uf0ad"}, {"qualify", "\uf0c3"},
	{"owned", "\uf007"}, {"shared", "\uf0c0"}, {"foreign", "\uf023"},
	{"keep", "\uf08d"}, {"ready", glyphCheck}, {"merged", "\uf126"}, {"healthy", "\uf004"}, {"todo", "\uf0ae"},
}

var pillIconByWord = func() map[string]string {
	m := make(map[string]string, len(pillIcons))
	for _, e := range pillIcons {
		m[e.word] = e.icon
	}
	return m
}()

// synonymEntry keeps SYNONYMS insertion order so the `known` word list is
// built without iterating a map.
type synonymEntry struct {
	from string
	to   string
}

// synonymEntries are the words that display as one canonical word, so the
// same state always reads the same (upstream SYNONYMS, insertion order).
var synonymEntries = []synonymEntry{
	{"complete", "Done"}, {"completed", "Done"}, {"ok", "Done"}, {"finished", "Done"}, {"pass", "Passed"},
	{"fail", "Failed"}, {"failing", "Failed"}, {"error", "Failed"}, {"waiting", "Pending"}, {"needs review", "Review"},
}

// synonyms is the lookup map built from synonymEntries.
var synonyms = func() map[string]string {
	m := make(map[string]string, len(synonymEntries))
	for _, e := range synonymEntries {
		m[e.from] = e.to
	}
	return m
}()

// labelIcons is the icon a heading or a line's leading bold label takes from
// its word (upstream LABEL_ICONS, in Object.keys order).
var labelIcons = []iconEntry{
	{"finding", "\uf002"}, {"terminal", "\uf120"}, {"folder", "\uf07b"}, {"file", "\uf15b"}, {"branch", "\uf126"}, {"review", "\uf06e"},
	{"ideas", "\uf0eb"}, {"cause", "\uf0e7"}, {"fix", "\uf0ad"}, {"risk", "\uf071"}, {"limits", "\uf071"}, {"next", "\uf061"}, {"rollback", "\uf0e2"},
	{"undo", "\uf0e2"}, {"options", "\uf0ca"}, {"result", "\uf11e"}, {"why", "\uf059"}, {"how", "\uf013"}, {"cost", "\uf155"}, {"rule", "\uf0e3"},
	{"example", "\uf121"}, {"checked", "\uf00c"}, {"pills", "\uf02b"}, {"colors", "\uf1fc"}, {"icons", "\uf005"}, {"scoreboard", "\uf080"},
	{"verdict", "\uf0e3"}, {"effect", "\uf140"}, {"evidence", "\uf06e"}, {"plan", "\uf0ae"}, {"status", "\uf012"}, {"decision", "\uf126"},
}

var labelIconByWord = func() map[string]string {
	m := make(map[string]string, len(labelIcons))
	for _, e := range labelIcons {
		m[e.word] = e.icon
	}
	return m
}()

// actionIcons are the lead-in words that name the action a pill calls for
// (upstream ACTION_ICONS). With a colon they take an icon anywhere in a line.
var actionIcons = []iconEntry{
	{"check", "\uf046"}, {"do", "\uf0a4"}, {"next", "\uf061"}, {"unblock", "\uf09c"}, {"left", "\uf141"}, {"run", "\uf04b"}, {"guard", "\uf132"}, {"undo", "\uf0e2"}, {"see", "\uf08e"},
}

var actionIconByWord = func() map[string]string {
	m := make(map[string]string, len(actionIcons))
	for _, e := range actionIcons {
		m[e.word] = e.icon
	}
	return m
}()

// actionLabel matches **word:** with word one of the action lead-ins
// (upstream ACTION_LABEL, case-insensitive).
var actionLabel = regexp.MustCompile(`(?i)(\*\*)(` + func() string {
	words := make([]string, 0, len(actionIcons))
	for _, e := range actionIcons {
		words = append(words, e.word)
	}
	return strings.Join(words, "|")
}() + `)(:\*\*)`)

// ownIcon matches a label opening with its own Private Use Area glyph, which
// wins over the table's icon (upstream OWN_ICON; RE2 form of
// [\ue000-\uf8ff]|[\u{f0000}-\u{ffffd}]).
var ownIcon = regexp.MustCompile(`^\s*([\x{e000}-\x{f8ff}\x{f0000}-\x{ffffd}])\s*`)

// known is every word that resolves a label to a canonical key, ordered by
// length descending with upstream's stable Set order (TONE_WORDS flat, then
// PILL_ICONS keys, then SYNONYMS keys). splitLabel picks the first hit, so
// longer words always win over their prefixes.
var known = func() []string {
	seen := map[string]bool{}
	var words []string
	add := func(w string) {
		if !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
	}
	for _, entry := range Vocabulary {
		for _, w := range entry.Core {
			add(strings.ToLower(w))
		}
		for _, w := range entry.Also {
			add(strings.ToLower(w))
		}
	}
	for _, e := range pillIcons {
		add(e.word)
	}
	for _, e := range synonymEntries {
		add(e.from)
	}
	// JS .sort((a, b) => b.length - a.length) is stable: equal lengths keep
	// insertion order. sort.SliceStable mirrors it.
	sort.SliceStable(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
	return words
}()

// labelDetail is a pill label read as its word: the icon it carries, the
// display word and the canonical lookup key.
type labelDetail struct {
	icon string
	word string
	key  string
}

// asciiLower folds only 'A'..'Z' to 'a'..'z'. Every vocabulary-table key
// is pure ASCII, so ASCII-only folding resolves them identically to
// strings.ToLower while guaranteeing len(asciiLower(s)) == len(s): the
// folded byte length stays a valid byte offset into the original string,
// which strings.ToLower cannot promise (U+212A folds to 1 byte, U+0130 to
// 1, and slicing the original at that length would cut a multi-byte rune).
// Non-ASCII words simply do not match an ASCII key — the difference from
// Unicode folding is unreachable through a pure-ASCII table.
func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// splitLabel reads a pill label: a known word may be followed by detail
// ("Passed 12/12", "Failed: 3 tests") (upstream splitLabel).
func splitLabel(label string) labelDetail {
	var icon string
	text := label
	if m := ownIcon.FindStringSubmatch(text); m != nil {
		icon = m[1]
		text = text[len(m[0]):]
	}
	// JS .replace(/\s+/g, " ").trim(): collapse whitespace runs, then trim.
	// strings.Fields covers the same set for these labels (plus Unicode space
	// separators, which cannot appear inside a 32-byte pill label in practice
	// and are harmless to normalize).
	text = strings.Join(strings.Fields(text), " ")
	lower := asciiLower(text)
	var knownWord string
	for _, k := range known {
		if lower == k {
			knownWord = k
			break
		}
		if strings.HasPrefix(lower, k) && len(lower) > len(k) {
			if c := lower[len(k)]; c == ' ' || c == ':' {
				knownWord = k
				break
			}
		}
	}
	canonical := synonyms[knownWord]
	key := lower
	switch {
	case canonical != "":
		key = asciiLower(canonical)
	case knownWord != "":
		key = knownWord
	}
	word := text
	if canonical != "" && knownWord != "" {
		word = canonical + text[len(knownWord):]
	}
	if icon == "" {
		icon = pillIconByWord[key]
	}
	return labelDetail{icon: icon, word: word, key: key}
}

// ToneOf returns the tone a pill label takes (upstream toneOf).
func ToneOf(label string) PillTone {
	key := splitLabel(label).key
	// Upstream checks in this fixed order.
	for _, tone := range []PillTone{ToneDanger, ToneError, ToneWarning, ToneSuccess, ToneMode, ToneOwner} {
		for _, w := range toneWords[tone] {
			if key == w {
				return tone
			}
		}
	}
	return ToneNeutral
}

// parseFenceOpen returns the pure fence run (backticks or tildes, no
// leading spaces) of a line that opens a CommonMark fenced code block, or
// "" when the line does not. A backtick fence whose info string contains a
// backtick is not a fence open at all (CommonMark §4.5 and the host's
// parseMarkdownFenceOpen): such a line is a single-line inline code span —
// the shape the host's Mermaid converter emits — and goes to decorateLine,
// where protectedSpans keeps the span verbatim. A tilde fence's info string
// is unrestricted.
func parseFenceOpen(line string) string {
	m := fenceOpenRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	if m[1][0] == '`' && strings.ContainsRune(m[2], '`') {
		return ""
	}
	return m[1]
}

// isFenceClose reports whether line closes the open fence run, a verbatim
// port of the host's isMarkdownFenceClose (tui/markdown.go:1055-1062): at
// most 3 leading spaces, the opener's own character repeated at least the
// opener's length, then any mix of '~' and '`', then spaces only — the
// trailing trim accepts ASCII ' ' alone, so a close line ending in a tab
// keeps the block open.
func isFenceClose(line, openFence string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || !strings.HasPrefix(trimmed, strings.Repeat(string(openFence[0]), len(openFence))) {
		return false
	}
	rest := strings.TrimLeft(trimmed[len(openFence):], "~`")
	return strings.Trim(rest, " ") == ""
}

// ansiClassBgToFg matches \x1b[(4|10)([0-7])m, the basic/bright background
// classes (upstream bgToFg's second replace).
var ansiClassBgToFg = regexp.MustCompile("\x1b\\[(4|10)([0-7])m")

// bgToFg turns a background escape into the same color as a foreground, for
// pill caps (upstream bgToFg).
func bgToFg(bg string) string {
	fg := strings.ReplaceAll(bg, "\x1b[48;", "\x1b[38;")
	return ansiClassBgToFg.ReplaceAllStringFunc(fg, func(m string) string {
		sub := ansiClassBgToFg.FindStringSubmatch(m)
		if sub[1] == "4" {
			return "\x1b[3" + sub[2] + "m"
		}
		return "\x1b[9" + sub[2] + "m"
	})
}

// solid builds a pill style in the terminal's own color (upstream solid).
func solid(code int) PillStyle {
	return PillStyle{
		Label: "\x1b[" + strconv.Itoa(code) + "m\x1b[7m",
		Cap:   "\x1b[" + strconv.Itoa(code) + "m",
	}
}

// CreatePalette builds the colorizer set from a theme (upstream
// createPalette). boldColor defaults to "mdLink"; PI_EMPHASIS_ROUND_CAPS is
// read here, as upstream reads process.env at palette build time.
//
// PiG compatibility notes:
//   - UITheme.GetBgAnsi returns ("\x1b[49m", nil) under theme "system" when
//     the terminal reports no panel colors: the error, empty and 49m cases
//     all fall back to reverse video, with labelFg "text" upgraded to
//     "accent" so the chip keeps its tone color.
//   - GetFgAnsi errors leave that tone uncolored instead of panicking: PiG's
//     UITheme.Fg panics on unknown tokens, so every Fg call is preceded by a
//     GetFgAnsi validation.
func CreatePalette(theme ThemeLike, boldColor ...string) Palette {
	bold := "mdLink"
	if len(boldColor) > 0 && boldColor[0] != "" {
		bold = boldColor[0]
	}
	fg := func(name string) Colorize {
		return func(text string) string { return theme.Fg(name, text) }
	}
	tone := func(labelFg, bg string) PillStyle {
		bgAnsi, bgErr := theme.GetBgAnsi(bg)
		if bgErr != nil || bgAnsi == "" || bgAnsi == "\x1b[49m" {
			// A theme without its own panel colors draws pills in reverse video.
			fgName := labelFg
			if fgName == "text" {
				fgName = "accent"
			}
			fgAnsi, _ := theme.GetFgAnsi(fgName)
			return PillStyle{Label: fgAnsi + "\x1b[7m", Cap: fgAnsi}
		}
		fgAnsi, _ := theme.GetFgAnsi(labelFg)
		return PillStyle{Label: bgAnsi + fgAnsi, Cap: bgToFg(bgAnsi)}
	}
	// The bold token is validated before Fg ever sees it: an unknown
	// PI_EMPHASIS_COLOR falls back to "mdLink", and a theme that cannot
	// resolve even that leaves bold uncolored (nil) instead of panicking.
	if ansi, err := theme.GetFgAnsi(bold); err != nil || ansi == "" {
		if ansi, err := theme.GetFgAnsi("mdLink"); err == nil && ansi != "" {
			bold = "mdLink"
		} else {
			bold = ""
		}
	}
	return Palette{
		Bold:   safeFg(theme, fg, bold),
		Passed: safeFg(theme, fg, "success"),
		Failed: safeFg(theme, fg, "error"),
		Pills: map[PillTone]PillStyle{
			ToneSuccess: tone("success", "toolSuccessBg"),
			ToneError:   tone("error", "toolErrorBg"),
			ToneWarning: tone("warning", "searchMatchBg"),
			ToneNeutral: tone("text", "selectedBg"),
			// Solid chips in the terminal's own red, cyan and blue: louder than
			// the panel-colored tones.
			ToneDanger: solid(31),
			ToneMode:   solid(36),
			ToneOwner:  solid(34),
		},
		Arrow: safeFg(theme, fg, "accent"),
		// mdHeading resolves to the foreground escape a heading pill
		// restores after its reset; an unresolvable token leaves Heading
		// empty instead of panicking.
		Heading: func() string {
			if ansi, err := theme.GetFgAnsi("mdHeading"); err == nil {
				return ansi
			}
			return ""
		}(),
		// PiG's UITheme.Fg("dim") appends the merged close "\x1b[22;39m", which
		// ends an outer bold the host will not reopen. dimFg builds the
		// colorizer from GetFgAnsi so the close is written split
		// ("\x1b[22m\x1b[39m") when the token is faint, leaving decorateLine's
		// reopen() in charge of restoring bold.
		Tree:      dimFg(theme),
		RoundCaps: os.Getenv("PI_EMPHASIS_ROUND_CAPS") != "0",
	}
}

// safeFg returns fg(name) when the token resolves; otherwise nil (text stays
// uncolored). PiG's Fg panics on unknown tokens, so it is never called before
// GetFgAnsi validates the name.
func safeFg(theme ThemeLike, fg func(string) Colorize, name string) Colorize {
	if name == "" {
		return nil
	}
	if ansi, err := theme.GetFgAnsi(name); err == nil && ansi != "" {
		return fg(name)
	}
	return nil
}

// dimFg colors with the "dim" token. PiG's UITheme.Fg closes a faint token
// with the merged "\x1b[22;39m", which ends an outer bold the host will not
// reopen; a faint dim is therefore self-composed from GetFgAnsi with the
// close written split ("\x1b[22m\x1b[39m"), the same codes in a form an
// outer bold span can reopen after. A non-faint dim goes through Fg, which
// emits the identical escape pair. A token that does not resolve leaves text
// uncolored.
func dimFg(theme ThemeLike) Colorize {
	ansi, err := theme.GetFgAnsi("dim")
	if err != nil || ansi == "" {
		return nil
	}
	if strings.HasSuffix(ansi, "\x1b[2m") {
		return func(text string) string { return ansi + text + "\x1b[22m\x1b[39m" }
	}
	return func(text string) string { return theme.Fg("dim", text) }
}

var (
	// headingRe matches a Markdown heading line (h1-h6, Pi already colors them).
	headingRe = regexp.MustCompile(`^ {0,3}#{1,6}\s`)
	// headingSplit splits a heading into hashes and title (upstream's inline
	// /^( {0,3}#{1,6}\s+)(.+)$/ replace).
	headingSplit = regexp.MustCompile(`^( {0,3}#{1,6}\s+)(.+)$`)
	// inlineRe matches escaped bold markers, bold markers, or a [[pill]] of
	// 1-32 characters (upstream INLINE). Prose no longer consumes it;
	// proseScanRe does.
	inlineRe = regexp.MustCompile(`(\\*)(\*\*|__)|\[\[([^\[\]\n]{1,32})\]\]`)
	// proseScanRe is INLINE extended with the single-rune marks and the
	// [├└]── tree connector, so one left-to-right pass sees every token in
	// position order: a mark inside a bold span is colored while *inBold
	// already holds the opener, which a mark pass running before the bold
	// scan never saw (submatches: slashes, bold marker, pill label, mark,
	// tree connector).
	proseScanRe = regexp.MustCompile(`(\\*)(\*\*|__)|\[\[([^\[\]\n]{1,32})\]\]|([✓✔✗✘→])|([├└]──)`)
	// leadLabelRe matches a line's leading bold label, after an optional list
	// marker or tree connector (upstream LEAD_LABEL).
	leadLabelRe = regexp.MustCompile(`^(\s*(?:[-*+]|\d+\.)?\s*(?:[├└]──\s*)?\*\*)([^*` + "`" + `\n]{1,32})(\*\*)`)
	// fenceOpenRe matches a CommonMark fenced-code-block opening: up to 3
	// leading spaces, a run of >= 3 backticks or tildes, and the rest of the
	// line as the info string. The fence run is group 1 (upstream FENCE's
	// capture — the match itself must never be used, it carries the indent).
	fenceOpenRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	// ansiRe matches any SGR escape (tests and helpers strip with it).
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;a-zA-Z-]*m`)
)

// maxDecorateLineBytes caps the single-line input the emphasis decorator
// will scan: a line longer than this passes through byte-identical, a
// display-only safety guard that bounds the worst-case decoration cost of
// any single line regardless of the adversarial shape inside it. The
// fence state machine still reads the line, so an over-cap fence marker
// keeps opening/closing the block.
const maxDecorateLineBytes = 8192

// Decorate applies colors to assistant Markdown, leaving code blocks, inline
// code and headings alone (upstream decorate). An empty input returns empty
// in O(1).
func Decorate(markdown string, palette Palette) string {
	if markdown == "" {
		return ""
	}
	open, close := "", ""
	if palette.Bold != nil {
		open, close = splitColorize(palette.Bold)
	}
	var out strings.Builder
	out.Grow(len(markdown) + 64)
	var fence string
	for i, line := range strings.Split(markdown, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		if fence != "" {
			if isFenceClose(line, fence) {
				fence = ""
			}
			out.WriteString(line)
		} else if openFence := parseFenceOpen(line); openFence != "" {
			fence = openFence
			out.WriteString(line)
		} else if len(line) > maxDecorateLineBytes {
			// A single line over the byte cap is display-only: it passes
			// through untouched so no adversarial shape inside it can
			// stall the decoration path. The fence checks above still
			// ran, so the state machine is unaffected.
			out.WriteString(line)
		} else if headingRe.MatchString(line) {
			out.WriteString(decorateHeading(line, palette))
		} else {
			out.WriteString(decorateLine(line, palette, open, close))
		}
	}
	return out.String()
}

// splitColorize derives open/close from a colorizer by marking the wrap point
// with a NUL sentinel, as upstream decorate does (palette.bold("\u0000")).
func splitColorize(bold Colorize) (open, close string) {
	const sentinel = "\x00"
	wrapped := bold(sentinel)
	if idx := strings.Index(wrapped, sentinel); idx >= 0 {
		return wrapped[:idx], wrapped[idx+len(sentinel):]
	}
	return wrapped, ""
}

// decorateHeading colors one heading line: the host already paints the
// heading bold in mdHeading, so only the word icon and the [[pill]]s are
// added here. The title's icon comes first (upstream withLabelIcon on the
// split title), then the title is cut at the same protected fragments prose
// obeys — a `code span` or [label](url) keeps [[...]] literal — and every
// remaining [[label]] of 1–32 non-bracket characters renders as pill(). A
// pill's own SGR close drops the heading weight and foreground the host
// opened, so when more title text follows it the pair is reopened with
// "\x1b[1m" + palette.Heading (empty Heading restores the weight alone); a
// pill ending the line takes no reopen. Prose tokens the host's
// headingInline handles — **bold**, →, ✓, ├── — stay untouched here.
func decorateHeading(line string, palette Palette) string {
	return headingSplit.ReplaceAllStringFunc(line, func(m string) string {
		sub := headingSplit.FindStringSubmatch(m)
		title := withLabelIcon(sub[2])
		if palette.Pills == nil {
			return sub[1] + title
		}
		spans := protectedSpans(title)
		var sb strings.Builder
		sb.Grow(len(title) + 32)
		proseStart := 0
		for _, s := range spans {
			// A pill ending this fragment still sits inside the title (the
			// protected span follows it), so the restore applies there too.
			sb.WriteString(headingPills(title[proseStart:s.start], palette, true))
			sb.WriteString(title[s.start:s.end])
			proseStart = s.end
		}
		sb.WriteString(headingPills(title[proseStart:], palette, false))
		return sub[1] + sb.String()
	})
}

// headingPills renders the [[label]]s in one unprotected heading fragment.
// A nonempty label of 1–32 characters becomes a pill; when more title text
// follows the pill — inside the fragment, or (more) in a later fragment —
// "\x1b[1m" + palette.Heading reopens the bold weight and mdHeading
// foreground the host's headingInline opened (the same reopen()
// decorateProse applies inside a bold span). A pill ending the title takes
// no reopen: the host restores nothing at line end either.
func headingPills(fragment string, palette Palette, more bool) string {
	var sb strings.Builder
	sb.Grow(len(fragment) + 32)
	pos := 0
	for _, m := range inlineRe.FindAllStringSubmatchIndex(fragment, -1) {
		sb.WriteString(fragment[pos:m[0]])
		match := fragment[m[0]:m[1]]
		// m[6:8] is group 3, the [[label]]; empty means the match was a
		// bold marker or escaped asterisks, which the host's headingInline
		// owns and which pass through verbatim.
		if m[6] < 0 || strings.TrimSpace(fragment[m[6]:m[7]]) == "" {
			sb.WriteString(match)
		} else {
			reopen := ""
			if m[1] < len(fragment) || more {
				reopen = "\x1b[1m" + palette.Heading
			}
			sb.WriteString(pill(fragment[m[6]:m[7]], palette) + reopen)
		}
		pos = m[1]
	}
	sb.WriteString(fragment[pos:])
	return sb.String()
}

// withLabelIcon prefixes a known label with its icon, unless it already opens
// with a glyph (upstream withLabelIcon).
func withLabelIcon(label string) string {
	if ownIcon.MatchString(label) || os.Getenv("PI_EMPHASIS_PILL_ICONS") == "0" {
		return label
	}
	// Upstream: label.replace(/[:\s]+$/, "").trim().toLowerCase()
	word := asciiLower(strings.TrimSpace(strings.TrimRight(label, ":\t\n\v\f\r ")))
	var icon string
	if i, ok := labelIconByWord[word]; ok {
		icon = i
	} else if strings.HasSuffix(strings.TrimRight(label, " \t\n\v\f\r"), ":") {
		icon = actionIconByWord[word]
	}
	if icon == "" {
		return label
	}
	return icon + " " + label
}

// pill renders one [[label]] as a colored chip (upstream pill).
func pill(label string, palette Palette) string {
	style := palette.Pills[ToneOf(label)]
	d := splitLabel(label)
	text := d.word
	if d.icon != "" && d.word != "" && os.Getenv("PI_EMPHASIS_PILL_ICONS") != "0" {
		text = d.icon + " " + d.word
	}
	if text == "" {
		text = strings.TrimSpace(label)
	}
	// No-break spaces keep a two-word pill on one line when the host wraps
	// the paragraph.
	text = strings.ReplaceAll(text, " ", " ")
	left, right := "", ""
	pad := " "
	if palette.RoundCaps {
		left, right, pad = "", "", ""
	}
	capL, capR := "", ""
	if left != "" {
		capL = style.Cap + left + "\x1b[39m"
		capR = style.Cap + right + "\x1b[39m"
	}
	// pad stays inside the \x1b[1m … \x1b[22;27m\x1b[39m\x1b[49m span so a
	// line-end wrap cannot clip the padding of a square chip before the
	// reset.
	return capL + style.Label + "\x1b[1m" + pad + text + pad + "\x1b[22;27m\x1b[39m\x1b[49m" + capR
}

// span is a protected [start, end) byte range of a line: its content is
// emitted verbatim, with only the bold color restored after it when the line
// is inside a bold span.
type span struct {
	start, end int
}

// protectedSpans cuts a line into the fragments that must not receive
// injected ANSI, scanned left to right (first construct wins):
//  1. CommonMark code spans of any equal-length backtick run — the host's
//     built-in Mermaid converter runs before this extension and wraps
//     diagrams that already carry ANSI in multi-backtick code spans
//     (same exact-length scan as go/no-tables' splitByInlineCode);
//  2. inline Markdown links/images !?[label](url) — a bare \x1b[ injected
//     inside [label] leaves an unpaired [ and breaks the host's
//     markedInlineLink parse.
func protectedSpans(line string) []span {
	var spans []span
	// One linkCache per line: the positional memos are pure functions
	// of (line, offset), so repeated link probes at later '[' starts
	// share every suffix a previous probe already walked.
	rc := newLinkCache()
	n := len(line)
	i := 0
	for i < n {
		if line[i] == '`' {
			start := i
			for i < n && line[i] == '`' {
				i++
			}
			openerLen := i - start
			j := i
			closerStart := -1
			for j < n && line[j] != '\n' {
				if line[j] == '`' {
					cStart := j
					for j < n && line[j] == '`' {
						j++
					}
					if j-cStart == openerLen {
						closerStart = cStart
						break
					}
				} else {
					j++
				}
			}
			if closerStart >= 0 {
				end := closerStart + openerLen
				spans = append(spans, span{start, end})
				i = end
				continue
			}
			continue
		}
		if line[i] == '[' || (line[i] == '!' && i+1 < n && line[i+1] == '[') {
			if l := matchInlineLink(line, i, rc); l > 0 {
				spans = append(spans, span{i, i + l})
				i += l
				continue
			}
		}
		i++
	}
	return spans
}

// isJSWhitespace reports whether r belongs to JavaScript's \s set
// (WhiteSpace + LineTerminator), the set markedInlineLink's \s* skips at
// the link tail — identical to go/no-tables' isJSWhitespace.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// skipJSSpace advances i past a JS \s* run (markedInlineLink's \s*).
func skipJSSpace(s string, i int) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isJSWhitespace(r) {
			break
		}
		i += size
	}
	return i
}

// trimJSSpaceRight cuts trailing JS whitespace (the right half of
// widthx.JSTrim; host link() re-trims raw after the paren truncation).
func trimJSSpaceRight(s string) string {
	return strings.TrimRightFunc(s, isJSWhitespace)
}

// matchInlineLink returns the byte length of the inline Markdown link or
// image s opens with, or 0. It is a direct port of the host's
// markedInlineLink regexp + link() + outputLink() (tui/markdown_marked.go
// :163, :711-790), evaluated without regexp2 — pure standard library,
// bounded backtracking:
//
//	^!?\[((?:\[(?:\\[\s\S]|[^\[\]\\])*\]|\\[\s\S]|`+(?!`)[^`]*?`+(?!`)|``+(?=\])|[^\[\]\\`])*?)\]
//	\(\s*(<(?:\\.|[^\n<>\\])+>|[^ \t\n\x00-\x1f]+|(?=\)))
//	(?:(?:[ \t]+(?:\n[ \t]*)?|\n[ \t]*)("(?:\\"?|[^"\\])*"|'(?:\\'?|[^'\\])*'|\((?:\\\)?|[^)\\])*\)))?\s*\)
//
// The label alternation has five branches tried in order at each position;
// lazy `*?` plus backtracking is reproduced by labelScan, which on each
// backtick run first tries branch 3 (codespan: continue after the next
// backtick run) then branch 4 (“+ before ']': close the label there) —
// the only reachable backtrack shapes, because branches 1, 2 and 5 each
// consume a fixed rune/rune-pair and the label then resumes at a fixed
// offset. Branch 5 excludes a literal '`' or ']', so an unclosed single
// backtick or a top-level ']' always ends the label: an unmatched ` means
// no link at all, and ']' calls matchLinkTail once.
//
// A bold color injected inside any of these leaves an unpaired '[' or '('
// that breaks the host's link parse, so the whole construct stays verbatim.
func matchInlineLink(s string, base int, rc *linkCache) int {
	i := base
	if i < len(s) && s[i] == '!' {
		i++
	}
	isImage := i > base
	labelStart := i
	if i >= len(s) || s[i] != '[' {
		return 0
	}
	i++
	end := labelScan(s, isImage, labelStart, i, rc)
	if end <= 0 {
		return 0
	}
	return end - base
}

// labelScan continues a label scan from offset i (s[labelStart] is the
// outer '[' or '!' and the label opened at labelStart's bracket). It
// returns the byte offset just past the whole match, or 0.
func labelScan(s string, isImage bool, labelStart, i int, rc *linkCache) int {
	for i < len(s) {
		switch s[i] {
		case '[':
			// Branch 1: \[(?:\\[\s\S]|[^\[\]\\])*\] — a nested pair.
			// Backticks are ordinary runes inside; escapes swallow one
			// rune each. An unclosed pair (or a second '[' inside, which
			// the inner [^\[\]\\] also excludes) fails the whole match:
			// no other branch can start at a literal '['.
			j := i + 1
			closed := false
			for j < len(s) {
				switch s[j] {
				case '\\':
					_, size := utf8.DecodeRuneInString(s[j+1:])
					j += 1 + size
					continue
				case '[':
					return 0
				case ']':
					closed = true
					j++
				}
				if closed {
					break
				}
				_, size := utf8.DecodeRuneInString(s[j:])
				j += size
			}
			if !closed {
				return 0
			}
			i = j
		case '\\':
			// Branch 2: \\[\s\S] — swallow one rune; a lone trailing
			// backslash matches nothing (branch 5 excludes '\').
			if i+1 >= len(s) {
				return 0
			}
			_, size := utf8.DecodeRuneInString(s[i+1:])
			i += 1 + size
		case ']':
			// Branch 5 excludes a literal ']': the label closes here.
			return matchLinkTail(s, isImage, labelStart, i, rc)
		case '`':
			start := i
			for i < len(s) && s[i] == '`' {
				i++
			}
			run := i - start
			// Branch 3: `+(?!`)[^`]*?`+(?!`) — [^`]*? cannot cross a
			// backtick, so the only codespan candidate is the next run
			// [cStart:cEnd); continuing from cEnd reproduces the regexp
			// (later candidates are re-reached identically when that
			// continuation hits another backtick run).
			cStart, cEnd := -1, -1
			for j := i; j < len(s); j++ {
				if s[j] == '`' {
					cStart = j
					for j < len(s) && s[j] == '`' {
						j++
					}
					cEnd = j
					break
				}
			}
			if cStart >= 0 {
				if l := labelScan(s, isImage, labelStart, cEnd, rc); l > 0 {
					return l
				}
			}
			// Branch 4: ``+(?=\]) — only reachable for run >= 2 (a single
			// backtick always took branch 3 above when any closer run
			// existed). The lookahead keeps ']' for the label close.
			if run >= 2 && i < len(s) && s[i] == ']' {
				return matchLinkTail(s, isImage, labelStart, i, rc)
			}
			// Branch 5 excludes a literal '`': the run cannot stay in the
			// label, and no other alternative applies — fail.
			return 0
		default:
			// Branch 5: [^\[\]\\`] — one ordinary rune.
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
		}
	}
	return 0
}

// matchLinkTail matches `]\(\s* dest (sep title)? \s*\)` starting at the
// ']' at s[i]. It simulates the host in two stages, mirroring link()
// (tui/markdown_marked.go :711-755) exactly:
//
//  1. The markedInlineLink regexp exec: greedy alternation over the
//     destination (angle-wrapped / bare run / empty lookahead), the
//     optional separator+title, and the trailing \s*\), with the
//     regexp's backtracking reproduced by bounded rescans. This stage
//     yields the captured (destination, rawEnd) pair the regexp would
//     hand to link().
//  2. link()'s own post-checks (:725-750): the JSTrim angle-bracket
//     parity check, then markedFindClosingBracket over the captured
//     destination — whose -2 / >=0 verdicts reject or truncate the
//     match — and finally outputLink's label-must-not-hold-a-link
//     probe (:779-783), which the truncation path in the host code
//     also runs (link() does not return before outputLink).
//
// labelStart is the offset of the link's '[' for images and non-images
// alike, i its label close. Returns the byte offset just past the whole
// match, or 0.
func matchLinkTail(s string, isImage bool, labelStart, i int, rc *linkCache) int {
	rawEnd, destination, angleDest := matchLinkTailExec(s, i, rc)
	if rawEnd <= 0 {
		return 0
	}
	// Stage 2 — link() post-checks (:725-750). angleDest already
	// passed the angle parity check inside matchLinkTailExec (the
	// regexp only captures a closed <...> there); a non-angle
	// destination still runs the JSTrim'd '<' fallback check.
	if !angleDest && destination != "" {
		trimmed := strings.TrimFunc(destination, isJSWhitespace)
		if strings.HasPrefix(trimmed, "<") {
			// link() (:725-735): a '<'-prefixed destination is checked
			// as an angle URL and accepted verbatim — this branch is
			// if/else-exclusive with the findClosingBracket truncation
			// below, so a '<...>' that passes parity is never re-cut at
			// an inner ')'.
			if !strings.HasSuffix(trimmed, ">") {
				return 0
			}
			inner := trimmed[1 : len(trimmed)-1]
			slashes := len(inner) - len(strings.TrimRight(inner, `\`))
			if (slashes+1)%2 == 0 {
				return 0
			}
		} else {
			// link() (:737-749): findClosingBracket on the captured
			// destination; ')' inside it balances '(' (wiki-style
			// URLs), so only a ')' dipping the balance below 0
			// truncates. The host computes linkLen in RUNES as
			// start + len(labelRunes) + lastParenIndex with
			// start = 4 / 5 — i.e. it counts c+1 runes from just
			// after '(', assuming no \s* whitespace and no
			// multi-byte runes before or inside the destination.
			// c is a byte index here, so convert to runes and
			// reproduce the rune-count slice exactly, then JSTrim
			// the right edge.
			switch c := findClosingParen(destination); {
			case c == -2:
				return 0
			case c >= 0:
				start := 4
				if isImage {
					start = 5
				}
				// labelStart is the offset of the '[' for images and
				// non-images alike, so the label body is
				// s[labelStart+1:i] in both cases — never skip
				// another byte for the '!'.
				labelRunes := utf8.RuneCountInString(s[labelStart+1 : i])
				cRunes := utf8.RuneCountInString(destination[:c])
				linkLen := start + labelRunes + cRunes
				// linkLen counts runes from the candidate's own '!' or
				// '[' (the regexp-anchored string start), not from s[0].
				base := labelStart
				if isImage {
					base--
				}
				relEnd := runeByteOffset(s[base:], linkLen)
				rawEnd = base + len(trimJSSpaceRight(s[base:base+relEnd]))
			}
		}
	}

	// outputLink (:779-783): a non-image link whose label text itself
	// parses as an inline link is not a link — the inner one wins and
	// the outer '[' falls back to text. This probe runs on every path
	// out of link(), including the paren-truncated one above.
	if !isImage && labelHasInnerLink(s, labelStart, i, rc) {
		return 0
	}
	return rawEnd
}

// runeByteOffset returns the byte offset in s of rune index n, or
// len(s) when s holds fewer than n runes.
func runeByteOffset(s string, n int) int {
	b, cnt := 0, 0
	for b < len(s) && cnt < n {
		_, size := utf8.DecodeRuneInString(s[b:])
		b += size
		cnt++
	}
	return b
}

// matchLinkTailExec is stage 1 of matchLinkTail: it reproduces the
// markedInlineLink regexp's greedy exec over `\(\s* dest (sep title)? \s*\)`
// starting at the ']' at s[i], returning the byte offset where the whole
// match ends (rawEnd), the captured destination text, and whether it
// was the angle-wrapped alternative. rawEnd <= 0 means the regexp did
// not match at all.
//
// Destination alternation (regexp group 2, tried in order):
//
//  1. <(?:\\.|[^\n<>\\])+>   angle-wrapped, at least one rune inside
//  2. [^ \t\n\x00-\x1f]+      bare run of non-space non-control runes
//  3. (?=\))                  empty destination
//
// The bare run is the interesting one: [^ \t\n\x00-\x1f]+ is greedy and
// swallows ')' too, so the regexp backtracks it until the optional
// title group and trailing \s*\) can close.
func matchLinkTailExec(s string, i int, rc *linkCache) (rawEnd int, destination string, angleDest bool) {
	p := i + 1 // past ']'
	if p >= len(s) || s[p] != '(' {
		return 0, "", false
	}
	p++

	// Leading \s* and its backtracking. The regexp consumes JS
	// whitespace greedily after '('; when the whole then fails it yields
	// whitespace back from the right edge, and each yielded boundary is
	// a fresh starting point for the destination alternation — a
	// non-ASCII JS whitespace rune (NBSP, U+2028, ...) becomes legal
	// bare-run content ([^ \t\n\x00-\x1f]+ does not exclude it) while a
	// yielded ASCII space or tab becomes part of the title separator.
	// The engine tries boundaries from the greedy cut leftward, so the
	// leftmost successful wsStart wins. Sharing keeps the loop linear
	// (see linkCache): each continuous bare run is resolved once, its
	// tail verdict computed once, and the whitespace suffix after each
	// run's right edge is walked once for every start position that
	// ever queries it.
	wsStart := p
	var bounds []int
	for wsStart < len(s) {
		r, size := utf8.DecodeRuneInString(s[wsStart:])
		if !isJSWhitespace(r) {
			break
		}
		bounds = append(bounds, wsStart)
		wsStart += size
	}
	bounds = append(bounds, wsStart)
	for i := len(bounds) - 1; i >= 0; i-- {
		if end, dest, angle := matchDestFrom(s, bounds[i], rc); end > 0 {
			return end, dest, angle
		}
	}
	return 0, "", false
}

// linkCache carries the positional memos shared by every link-parse
// restart over one source string — both the \s* boundary restarts
// inside a single matchLinkTailExec call and the link probes at later
// '[' starts of the same line. Every entry is a pure function of the
// source string and its position key, so sharing can never change a
// result; it only removes rescans of suffixes a previous probe
// already walked:
//
//   - runs maps a bare-run start position to the run it opens: its
//     right end (the first bare-run terminator) and the rightmost ')'
//     inside it. Both are functions of the start position alone —
//     positions inside a run yield the same end, and the rightmost
//     ')' at or after a start is the first one the regexp's give-back
//     reaches;
//   - tailOK / tailFailedAt memoize the (sep title)?\s*\) tail verdict
//     per run end — the tail depends only on where the run ends, not
//     on where inside the run the destination started;
//   - wsEnd memoizes skipJSSpace per start position: different runs
//     end at different offsets inside the same JS whitespace suffix,
//     and without sharing each failing run end would re-skip the
//     suffix from its own offset;
//   - sepEnd memoizes the title separator's end ([ \t]+(\n[ \t]*)?
//     or \n[ \t]*) per start position — consecutive run ends can sit
//     one ASCII space apart and would otherwise re-scan the separator
//     suffix quadraticly;
//   - titleScan memoizes scanTitleClose per (start, closeCh): title
//     attempts that restart at the same quote through different
//     separators share one scan instead of re-decoding the whole
//     failing title.
type titleScanKey struct {
	start   int
	closeCh byte
}

// runRes is the positional resolution of one bare destination run.
type runRes struct {
	end        int // first bare-run terminator at or after the start
	rightParen int // rightmost ')' in [start, end), or -1
}

type linkCache struct {
	runs         map[int]runRes
	tailOK       map[int]int
	tailFailedAt map[int]bool
	wsEnd        map[int]int
	sepEnd       map[int]int
	titleScan    map[titleScanKey]int
	scratch      []int
}

func newLinkCache() *linkCache {
	return &linkCache{
		runs:         map[int]runRes{},
		tailOK:       map[int]int{},
		tailFailedAt: map[int]bool{},
		wsEnd:        map[int]int{},
		sepEnd:       map[int]int{},
		titleScan:    map[titleScanKey]int{},
	}
}

// runAt resolves the bare destination run [^ \t\n\x00-\x1f]+ starting
// at byte offset p. A miss scans right until the first terminator or
// an already-resolved position, tracking the rightmost ')' it passes,
// then stamps every decoded position with its own view of the run —
// each position is decoded at most once across all probes sharing the
// cache, so a line of adjacent pseudo-links or a \s* boundary loop
// never re-scans a suffix a previous start already walked.
func (rc *linkCache) runAt(s string, p int) runRes {
	if r, ok := rc.runs[p]; ok {
		return r
	}
	res := runRes{end: p, rightParen: -1}
	jumpRP := -1 // rightmost ')' known for positions at/after the walk's end
	rc.scratch = rc.scratch[:0]
	for res.end < len(s) {
		if r, ok := rc.runs[res.end]; ok {
			// A resolved position's rightParen covers [pos, r.end),
			// which lies right of everything walked so far — prefer it.
			if r.rightParen >= 0 {
				res.rightParen = r.rightParen
			}
			jumpRP = r.rightParen
			res.end = r.end
			break
		}
		if s[res.end] == ')' {
			res.rightParen = res.end
		}
		rc.scratch = append(rc.scratch, res.end)
		c := s[res.end]
		if c == ' ' || c == '\t' || c == '\n' || c < 0x20 {
			break
		}
		_, size := utf8.DecodeRuneInString(s[res.end:])
		res.end += size
	}
	// Stamp each decoded position with its own view: end is shared,
	// and rightParen is the rightmost ')' in [pos, res.end). Walking
	// the decoded positions backwards, the first ')' met is that
	// rightmost one; positions after it (and the terminator itself,
	// whose run is empty) keep the value already known beyond them.
	rp := jumpRP
	for i := len(rc.scratch) - 1; i >= 0; i-- {
		q := rc.scratch[i]
		if rp < 0 && s[q] == ')' {
			rp = q
		}
		rc.runs[q] = runRes{end: res.end, rightParen: rp}
	}
	return res
}

// wsSkip is skipJSSpace with the shared per-position memo. A miss
// scans right only until the first non-whitespace rune or an
// already-cached position, then stamps every rune start it decoded
// with that same end — each whitespace-rune start is decoded at most
// once no matter how many runs query into the same suffix. Identical
// result to skipJSSpace(s, p) in every case.
func (rc *linkCache) wsSkip(s string, p int) int {
	if end, ok := rc.wsEnd[p]; ok {
		return end
	}
	rc.scratch = rc.scratch[:0]
	rc.scratch = append(rc.scratch, p)
	end := p
	for end < len(s) {
		if e, ok := rc.wsEnd[end]; ok {
			end = e
			break
		}
		r, size := utf8.DecodeRuneInString(s[end:])
		if !isJSWhitespace(r) {
			break
		}
		end += size
		rc.scratch = append(rc.scratch, end)
	}
	// Every decoded rune start lies in the whitespace run ending at
	// end; a duplicate stamp of end itself is harmless.
	for _, q := range rc.scratch {
		rc.wsEnd[q] = end
	}
	return end
}

// sepSkip scans the title separator [ \t]+(\n[ \t]*)? or \n[ \t]*
// from offset p, memoized per start position: the consumed span is a
// pure function of (s, p) and consecutive run ends can query
// separators starting one ASCII space apart over the same whitespace
// suffix. A non-separator start returns p itself. Whether a separator
// is PRESENT stays the caller's check on s[p] (hasSep).
func (rc *linkCache) sepSkip(s string, p int) int {
	if q, ok := rc.sepEnd[p]; ok {
		return q
	}
	q := p
	if q < len(s) && (s[q] == ' ' || s[q] == '\t') {
		for q < len(s) && (s[q] == ' ' || s[q] == '\t') {
			q++
		}
		if q < len(s) && s[q] == '\n' {
			q++
			for q < len(s) && (s[q] == ' ' || s[q] == '\t') {
				q++
			}
		}
	} else if q < len(s) && s[q] == '\n' {
		q++
		for q < len(s) && (s[q] == ' ' || s[q] == '\t') {
			q++
		}
	}
	rc.sepEnd[p] = q
	return q
}

// matchDestFrom tries the destination alternation with its tail anchored
// at byte offset destStart, reproducing one \s* boundary of
// matchLinkTailExec. Order is the regexp's: angle-wrapped first, then the
// bare run, then the empty-destination lookahead.
func matchDestFrom(s string, destStart int, rc *linkCache) (int, string, bool) {
	// Alternative 1: <...> applies only when an unescaped '>' closes
	// it; when its tail cannot match, the regexp falls back to
	// alternative 2 with the '<' as an ordinary bare rune.
	if destStart < len(s) && s[destStart] == '<' {
		j := destStart + 1
		closed := false
	angleScan:
		for j < len(s) {
			switch s[j] {
			case '\\':
				// \\. consumes the backslash and the next rune as one
				// unit, and translateMarkedPattern (:109-110) makes '.'
				// exclude every JavaScript line terminator: when the
				// next rune is LF, CR, U+2028 or U+2029 the whole \\.
				// alternative fails, and [^\n<>\\] excludes the
				// backslash too — so the angle run ends right here
				// and the angle alternative cannot match.
				r, size := utf8.DecodeRuneInString(s[j+1:])
				if r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' {
					break angleScan
				}
				j += 1 + size
				continue
			case '>':
				closed = true
			case '\n', '<':
			}
			if closed || s[j] == '\n' || s[j] == '<' {
				break
			}
			_, size := utf8.DecodeRuneInString(s[j:])
			j += size
		}
		if closed && j-(destStart+1) >= 1 {
			// link() (:728-733): the captured angle destination is
			// JSTrim'ed; whitespace inside is impossible (the regexp
			// requires >=1 non-'\\' rune, and whitespace is a normal
			// [^\n<>\\] member), but the trailing-backslash parity
			// check still applies before the regexp is allowed to
			// keep it.
			dest := s[destStart : j+1]
			inner := dest[1 : len(dest)-1]
			slashes := len(inner) - len(strings.TrimRight(inner, `\`))
			if (slashes+1)%2 != 0 {
				// (sep title)? \s*\) after an angle destination: try
				// title first, then the bare \s*\) — the regexp's
				// alternation order. destStart stays at '<' so failure
				// falls through to the bare-run alternative below.
				tail := j + 1
				if end := matchTitleAndClose(s, tail, rc); end > 0 {
					return end, dest, true
				}
				if k := rc.wsSkip(s, tail); k < len(s) && s[k] == ')' {
					return k + 1, dest, true
				}
			}
		}
	}

	// Alternative 2/3: bare run then the title+close or \s*\) shapes.
	// runAt resolves the run positionally: a start inside a previously
	// resolved run shares that end, a start whose own first rune is a
	// terminator gets an empty run ending at itself, and each position
	// is decoded at most once across all probes sharing the cache.
	run := rc.runAt(s, destStart)
	runEnd := run.end
	if runEnd == destStart {
		// Alternative 3 (?=\)): ')' must follow immediately at this
		// \s* boundary.
		if destStart < len(s) && s[destStart] == ')' {
			return destStart + 1, "", false
		}
		return 0, "", false
	}
	// Greedy exec: first try (sep title)? \s*\) on the text after the
	// maximal bare run, then \s*\) directly after it — that is the
	// regexp's first success shape, and [^ \t\n\x00-\x1f]+ cannot
	// cross the ASCII space/control at s[runEnd], so \s*\) right there
	// is the unique greedy match. The outcome depends only on runEnd,
	// so it is resolved once per run and shared by every start in it.
	if end, ok := rc.tailOK[runEnd]; ok {
		return end, s[destStart:runEnd], false
	}
	if !rc.tailFailedAt[runEnd] {
		end := matchTitleAndClose(s, runEnd, rc)
		if end <= 0 {
			if k := rc.wsSkip(s, runEnd); k < len(s) && s[k] == ')' {
				end = k + 1
			}
		}
		if end > 0 {
			rc.tailOK[runEnd] = end
			return end, s[destStart:runEnd], false
		}
		rc.tailFailedAt[runEnd] = true
	}
	// Backtracking: the bare run gives characters back from its right
	// edge until \s*\) can close. Given-back characters feed only
	// `(sep title)?\s*\)`, which can start a separator solely on
	// whitespace (never present inside the run) or a ')' — so the
	// rightmost ')' inside the run is the only viable close, and it
	// is also the first one the engine tries. runAt resolves that
	// rightmost ')' per start position, so no give-back prefix is ever
	// re-scanned.
	closeIdx := run.rightParen
	if closeIdx < 0 {
		return 0, "", false
	}
	if closeIdx == destStart {
		// The run is ')' + non-')' runes: giving the whole run back
		// fails [^...]+'s minimum, so alternative 3's (?=\)) captures
		// the empty destination and the ')' at destStart closes via
		// \s*\). \s* may already have consumed whitespace between
		// '(' and this ')' — that is legal.
		return closeIdx + 1, "", false
	}
	return closeIdx + 1, s[destStart:closeIdx], false
}

// matchTitleAndClose tries the regexp's `(sep title)? \s*\)` tail at
// offset p, where sep is [ \t]+(\n[ \t]*)?|\n[ \t]* and title one of
// "..." / '...' / (...). It returns the byte offset just past the
// closing ')' on success, 0 when the tail cannot match there. The
// title close-quote candidates are tried left to right, and each is
// accepted only when \s*\) can close after it — the same order the
// engine's backtracking uses. The separator scan goes through
// rc.sepSkip so overlapping separator suffixes across run ends are
// not re-walked.
func matchTitleAndClose(s string, p int, rc *linkCache) int {
	hasSep := p < len(s) && (s[p] == ' ' || s[p] == '\t' || s[p] == '\n')
	q := rc.sepSkip(s, p)
	if hasSep && q < len(s) && (s[q] == '"' || s[q] == '\'' || s[q] == '(') {
		// Title: "(?:\\"?|[^"\\])*" | '(?:\\'?|[^'\\])*' | \((?:\\\)?|[^)\\])*\)
		// — content excludes the close char and the backslash, so a
		// '\\' reaches the scan only through \\<close>?: the backslash
		// greedily takes the close char as content, and on overall
		// failure the engine backtracks it to the lone backslash
		// ("<close>? matching empty), handing that close char to the
		// title end. (For the paren form the close is ')'.)
		quote := s[q]
		closeCh := quote
		if quote == '(' {
			closeCh = ')'
		}
		key := titleScanKey{start: q + 1, closeCh: closeCh}
		if end, ok := rc.titleScan[key]; ok {
			return end
		}
		end := scanTitleClose(s, q+1, closeCh, rc)
		rc.titleScan[key] = end
		return end
	}
	return 0
}

// scanTitleClose scans title content from offset start until the close
// char that lets \s*\) finish the link, returning the offset just past
// that ')' or 0. Its result is a pure function of (s, start, closeCh),
// so callers memoize it through rc.titleScan. It reproduces the
// regexp's (?:\\<close>?|[^<close>\\])* backtracking as a memoized
// state machine: at each position,
//
//   - '\\' may consume the next close char too (greedy \\<close>, tried
//     first via the recursive call) or stand alone as one content char
//     ("<close>? matching empty — the fallback when the greedy path
//     cannot complete the title); a '\\' not followed by the close
//     char only has the lone-backslash option, so "\\\\" is two
//     independent consumptions, not a paired escape;
//   - an unescaped close char is a title-close candidate: accepted
//     when \s*\) can finish after it, rejected otherwise — and the
//     whole path then fails, because every content alternative
//     excludes the closer ("(?:\\"?|[^"\\])*" etc. at :163);
//   - any other rune is content and advances one rune.
//
// failed memoizes positions whose subtree already failed, so each
// position is scanned at most once and the bounded backtrack stays
// linear even on inputs like "\\\"\\\"\\\"…". The \s* skip after
// each close candidate goes through rc.wsSkip, so a failing suffix
// shared by many runs is not re-walked per run.
func scanTitleClose(s string, start int, closeCh byte, rc *linkCache) int {
	failed := map[int]bool{}
	var scan func(j int) int
	scan = func(j int) int {
		for j < len(s) {
			if failed[j] {
				return 0
			}
			switch {
			case s[j] == '\\':
				if j+1 < len(s) && s[j+1] == closeCh {
					// Greedy \\<close>: content takes both runes.
					if end := scan(j + 2); end > 0 {
						return end
					}
					failed[j+2] = true
				}
				// "<close>? matched empty (or the next char is not
				// the close char): the backslash is content alone.
				j++
			case s[j] == closeCh:
				// Candidate title close: the regexp accepts the
				// first one that lets \s*\) finish. When it cannot,
				// this path fails outright — the closer cannot stay
				// inside the title as content.
				k := rc.wsSkip(s, j+1)
				if k < len(s) && s[k] == ')' {
					return k + 1
				}
				failed[j] = true
				return 0
			default:
				_, size := utf8.DecodeRuneInString(s[j:])
				j += size
			}
		}
		return 0
	}
	return scan(start)
}

// markedUnescapeBrackets is the host's outputLinkReplace
// (/\\([\[\]])/g → "$1", tui/markdown_marked.go:794-798): only the
// two bracket escapes lose their backslash.
func markedUnescapeBrackets(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\[`, `[`, `\]`, `]`).Replace(s)
}

// labelHasInnerLink emulates outputLink's textHasLink probe: the label
// s[labelStart+1:end] is unescaped the way outputLink does
// (markedUnescapeBrackets, /\\([\[\]])/g → "$1") into a fresh text
// bounded by the label's own ']', then the inline tokenizer is
// simulated over THAT text far enough to know whether a link token is
// emitted anywhere — the host sets l.linkEmitted and refuses the outer
// link (:779-783). Inside a label every '[' or '![' candidate either
// opens a real inline link or falls back to text; a backtick codespan
// swallows its content before the next '[' is considered
// (markedInlineCode), and an image token is consumed whole: outputLink
// only sets linkEmitted for non-image tokens (:782-788), so the '[' of
// a '![' is an image label, not a link start.
//
// The probe runs over the materialized unescaped view, not the raw
// line: a bracket kept by the unescape (\[ → [) is a fresh link start
// the host's tokenizer would see, and scanning a label-bounded copy
// can never reach past the label's own ']' into the outer destination
// — nor can its positional memos collide with the caller's, so the
// inner scan uses a fresh linkCache. maxDecorateLineBytes bounds the
// materialized label at 8192 bytes, so the copy is strictly bounded.
func labelHasInnerLink(s string, labelStart, end int, _ *linkCache) bool {
	text := markedUnescapeBrackets(s[labelStart+1 : end])
	innerRC := newLinkCache()
	for k := 0; k < len(text); k++ {
		switch {
		case text[k] == '$' || text[k] == '\\' && k+1 < len(text) && (text[k+1] == '(' || text[k+1] == '['):
			// nextInlineToken (:459-467) tries the LaTeX extension
			// before every other rule at '$' and '\\('/'\\[': a
			// recognized token's raw span is consumed whole, so a
			// [inner](u) shape inside it never reaches the link rule
			// and cannot set linkEmitted.
			if l := skipInlineLatex(text[k:]); l > 0 {
				k += l - 1
				continue
			}
			if text[k] == '\\' {
				k++ // an unconsumed '\\' takes the escape path below
			}
		case text[k] == '<':
			// The tag rule (:468-473, markedInlineTag :162) is tried
			// before link: a recognized tag's raw span — attribute
			// values included — is consumed whole.
			if l := skipInlineTag(text[k:]); l > 0 {
				k += l - 1
				continue
			}
		case text[k] == '!' && k+1 < len(text) && text[k+1] == '[':
			// Image token: link() emits it via outputLink without
			// setting linkEmitted (:782-788), so its whole raw span —
			// label and any inner links inside it — is consumed before
			// the next link-start probe.
			if l := matchInlineLink(text, k, innerRC); l > 0 {
				k += l - 1
			}
		case text[k] == '`':
			// markedInlineCode: a `+ run closes at the next equal-length
			// run; the codespan's content is not link-start text.
			runEnd := k
			for runEnd < len(text) && text[runEnd] == '`' {
				runEnd++
			}
			opener := runEnd - k
			c := runEnd
			found := false
			for c < len(text) {
				if text[c] == '`' {
					cs := c
					for c < len(text) && text[c] == '`' {
						c++
					}
					if c-cs == opener {
						found = true
						break
					}
				} else {
					c++
				}
			}
			if found {
				k = c - 1 // skip the codespan body
				continue
			}
			// Unclosed run: literal text, keep scanning.
		case text[k] == '\\':
			k++ // escaped rune is text, never a link start
		case text[k] == '[':
			// A non-image inner link emits linkEmitted: the host refuses
			// the outer link (:779-783). An inner image does not — a
			// '[' reached here never starts one (the '![' case above
			// consumed it whole), so it is a link candidate.
			if matchInlineLink(text, k, innerRC) > 0 {
				return true
			}
		}
	}
	return false
}

// skipInlineTag returns the byte length of the inline HTML tag s opens
// with, or 0 — the host's markedInlineTag (tui/markdown_marked.go :162,
// the Tokenizer.ts inline tag rule) evaluated without a regexp. The six
// alternatives, in order:
//
//	<!--(?:-?>|[\s\S]*?-->)          comment: "->" itself, else shortest "-->"
//	<\/[a-zA-Z][\w:-]*\s*>           closing tag
//	<[a-zA-Z][\w-]*(\s+attr(=v)?)*?\s*\/?>
//	<?[\s\S]*?\?>                   processing instruction
//	<![a-zA-Z]+\s[\s\S]*?>           declaration
//	<!\[CDATA\[[\s\S]*?\]\]>         CDATA
//
// The lazy "consume" bodies are index searches ("-->" cannot start inside
// its own first byte, so the leftmost unambiguous hit is the lazy match).
// `\s` is scanned against the JS whitespace set (isJSWhitespace) — the
// host translates the pattern's `\s` to markedJSWhitespace (:77).
func skipInlineTag(s string) int {
	switch {
	case strings.HasPrefix(s, "<!--"):
		rest := s[4:]
		// -?> matches ">" or "->" alone as the comment's whole
		// content; it is tried first, then the lazy "-->" body.
		if strings.HasPrefix(rest, ">") {
			return 5
		}
		if strings.HasPrefix(rest, "->") {
			return 6
		}
		if idx := strings.Index(rest, "-->"); idx >= 0 {
			return 4 + idx + 3
		}
		return 0
	case strings.HasPrefix(s, "</"):
		i := 2
		if i >= len(s) || !isASCIILetter(s[i]) {
			return 0
		}
		i++
		for i < len(s) && isTagNameChar(s[i]) {
			i++
		}
		i = skipJSSpace(s, i)
		if i < len(s) && s[i] == '>' {
			return i + 1
		}
		return 0
	case strings.HasPrefix(s, "<?"):
		if idx := strings.Index(s[2:], "?>"); idx >= 0 {
			return 2 + idx + 2
		}
		return 0
	case strings.HasPrefix(s, "<![CDATA["):
		if idx := strings.Index(s[9:], "]]>"); idx >= 0 {
			return 9 + idx + 3
		}
		return 0
	case strings.HasPrefix(s, "<!"):
		// <![a-zA-Z]+\s — the CDATA alternative above already ran, so
		// any "<![" reaching here is not CDATA and fails [a-zA-Z]+\s
		// on its '['; the general declaration is the only option left.
		i := 2
		for i < len(s) && isASCIILetter(s[i]) {
			i++
		}
		if i == 2 || i >= len(s) {
			return 0
		}
		if r, size := utf8.DecodeRuneInString(s[i:]); !isJSWhitespace(r) || size == 0 {
			return 0
		}
		if idx := strings.IndexByte(s[i:], '>'); idx >= 0 {
			return i + idx + 1
		}
		return 0
	}
	// <[a-zA-Z][\w-]*(?:\s+[a-zA-Z:_][\w.:-]*(?:\s*=\s*(".."|'..'|bare))?)*?\s*\/?>
	if len(s) < 3 || s[0] != '<' || !isASCIILetter(s[1]) {
		return 0
	}
	i := 2
	for i < len(s) && (s[i] == '_' || s[i] == '-' || isASCIILetter(s[i]) || s[i] >= '0' && s[i] <= '9') {
		i++
	}
	// The lazy attribute group is expanded as "advance until a candidate
	// close fails, then try to consume one attribute": an attribute that
	// fails mid-way makes the whole tag fail (the lazy body cannot skip
	// partial text), which is the regexp's lazy-then-backtrack result.
	for i < len(s) {
		if end := tryTagClose(s, i); end > 0 {
			return end
		}
		next, ok := scanTagAttr(s, i)
		if !ok {
			return 0
		}
		i = next
	}
	return 0
}

// tryTagClose applies the open tag's trailing \s*\/?> at offset i,
// returning the byte offset just past '>' or 0.
func tryTagClose(s string, i int) int {
	j := skipJSSpace(s, i)
	if j < len(s) && s[j] == '/' {
		j++
	}
	if j < len(s) && s[j] == '>' {
		return j + 1
	}
	return 0
}

// scanTagAttr consumes one attribute —
// \s+[a-zA-Z:_][\w.:-]*(\s*=\s*("[^"]*"|'[^']*'|[^\s"'=<>`]+))? —
// starting at offset i (which must be whitespace). It returns the offset
// after the attribute and true, or (0, false). The value alternation is
// quoted-double, quoted-single, then bare, in the regexp's order; a
// present '=' whose value matches none of the three fails the attribute.
func scanTagAttr(s string, i int) (int, bool) {
	j := i
	for j < len(s) {
		r, size := utf8.DecodeRuneInString(s[j:])
		if !isJSWhitespace(r) {
			break
		}
		j += size
	}
	if j == i || j >= len(s) {
		return 0, false // \s+ needs at least one whitespace rune
	}
	if c := s[j]; c != '_' && c != ':' && !isASCIILetter(c) {
		return 0, false
	}
	j++
	for j < len(s) && isTagAttrNameChar(s[j]) {
		j++
	}
	// Optional =value.
	k := skipJSSpace(s, j)
	if k >= len(s) || s[k] != '=' {
		return j, true
	}
	k++
	k = skipJSSpace(s, k)
	if k >= len(s) {
		return 0, false
	}
	switch s[k] {
	case '"':
		if idx := strings.IndexByte(s[k+1:], '"'); idx >= 0 {
			return k + 1 + idx + 1, true
		}
		return 0, false
	case '\'':
		if idx := strings.IndexByte(s[k+1:], '\''); idx >= 0 {
			return k + 1 + idx + 1, true
		}
		return 0, false
	default:
		// [^\s"'=<>`]+ — stops before a quote or '=' too: if the value
		// starts on one, it matches neither this branch nor a quoted
		// branch, so the attribute fails.
		e := k
		for e < len(s) {
			r, size := utf8.DecodeRuneInString(s[e:])
			if isJSWhitespace(r) || strings.ContainsRune("\"'=<>`", r) {
				break
			}
			e += size
		}
		if e == k {
			return 0, false
		}
		return e, true
	}
}

// isASCIILetter / isTagNameChar / isTagAttrNameChar cover the byte
// classes the tag pattern uses: [a-zA-Z], [\w:-] and [\w.:-].
func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isTagNameChar(c byte) bool {
	return c == '-' || c == ':' || c == '_' || isASCIILetter(c) || c >= '0' && c <= '9'
}

func isTagAttrNameChar(c byte) bool {
	return c == '-' || c == '.' || c == ':' || c == '_' || isASCIILetter(c) || c >= '0' && c <= '9'
}

// skipInlineLatex returns the byte length of the inline LaTeX token s
// opens with, or 0 — a port of the host's tokenizeInlineLatex
// (tui/markdown_latex.go:71-121). source is already known to start at a
// candidate opener ('$', '\\(' or '\\[').
func skipInlineLatex(s string) int {
	var opening, closing string
	switch {
	case strings.HasPrefix(s, "$$"):
		opening, closing = "$$", "$$"
	case strings.HasPrefix(s, "\\("):
		opening, closing = "\\(", "\\)"
	case strings.HasPrefix(s, "\\["):
		opening, closing = "\\[", "\\]"
	case strings.HasPrefix(s, "$"):
		// The single-'$' opener is refused when JS whitespace follows it
		// (reMdInlineDollarSpace over the first rune).
		if len(s) > 1 {
			if r, _ := utf8.DecodeRuneInString(s[1:]); isJSWhitespace(r) {
				return 0
			}
		}
		opening, closing = "$", "$"
	default:
		return 0
	}
	openLen := len(opening)

	closingIndex := mdFindClosingDelim(s, closing, openLen)
	if closingIndex >= 0 && opening == "$" {
		inner := s[openLen:closingIndex]
		after := s[closingIndex+1:]
		if textEndsJSSpace(inner) ||
			mdStartsDigit(after) ||
			(mdEnvVarInner(inner) && mdStartsIdentifier(after)) ||
			strings.Contains(inner, "`") {
			return 0
		}
	}

	if closingIndex < 0 {
		pendingSource := s[openLen:]
		if strings.HasPrefix(opening, "\\") || looksLikePendingDollarMath(pendingSource) {
			return len(s) // a pending token consumes the whole rest
		}
		return 0
	}

	text := s[openLen:closingIndex]
	if text == "" || strings.Contains(text, "\n") {
		return 0
	}
	return closingIndex + len(closing)
}

// mdFindClosingDelim ports the host's mdFindClosingDelimiter: the byte
// index of the first closing at or after start that does not follow an
// odd run of backslashes, or -1.
func mdFindClosingDelim(src, closing string, start int) int {
	for from := start; from <= len(src); {
		i := strings.Index(src[from:], closing)
		if i < 0 {
			return -1
		}
		idx := from + i
		backslashes := 0
		for p := idx - 1; p >= 0 && src[p] == '\\'; p-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return idx
		}
		from = idx + len(closing)
	}
	return -1
}

// textEndsJSSpace is reMdTrailingSpace: JS whitespace at the end.
func textEndsJSSpace(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	return isJSWhitespace(r)
}

// mdStartsDigit is the host's mdStartsWithDigit (/^\d/, ASCII only).
func mdStartsDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// mdStartsIdentifier is the host's mdStartsWithIdentifier
// (/^[A-Za-z_][A-Za-z0-9_]*/: the first character decides).
func mdStartsIdentifier(s string) bool {
	return s != "" && (s[0] == '_' || s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

// mdEnvVarInner is the host's reMdEnvVarInner,
// /^[A-Z_][A-Z0-9_]*(?:[^A-Za-z0-9_\s])?$/ with JS \s.
func mdEnvVarInner(s string) bool {
	runes := []rune(s)
	if len(runes) == 0 {
		return false
	}
	i := 0
	if c := runes[0]; c != '_' && (c < 'A' || c > 'Z') {
		return false
	}
	i++
	for i < len(runes) && (runes[i] == '_' || runes[i] >= 'A' && runes[i] <= 'Z' || runes[i] >= '0' && runes[i] <= '9') {
		i++
	}
	if i == len(runes) {
		return true
	}
	if i == len(runes)-1 {
		c := runes[i]
		return c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !isJSWhitespace(c)
	}
	return false
}

// pendingDollarMathRe is the host's reMdPendingDollarMath
// (markdown_latex.go:24), unchanged.
var pendingDollarMathRe = regexp.MustCompile(`\\[A-Za-z]+|[_^=+*/<>()\[\]|±≤≥≠≈∈→⇒∞∫∑√-]`)

// looksLikePendingDollarMath is the host's looksLikePendingDollarMath.
func looksLikePendingDollarMath(source string) bool {
	return pendingDollarMathRe.MatchString(source)
}

// findClosingParen ports the host's markedFindClosingBracket
// (helpers.ts:126-149, tui/markdown_marked.go:822-838) for the '(' / ')'
// pair over a byte string:
//
//	-1  no unescaped ')' dips the balance below 0 — keep the whole run
//	-2  a '(' stayed open at the end — the match fails
//	>=0 byte index of the first ')' below balance 0 — truncate there
func findClosingParen(s string) int {
	if !strings.Contains(s, ")") {
		return -1
	}
	level := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // escape: swallow the next byte too
		case '(':
			level++
		case ')':
			level--
			if level < 0 {
				return i
			}
		}
	}
	if level > 0 {
		return -2
	}
	return -1
}

// decorateLine colors one line outside headings and fences (upstream
// decorateLine). The lead bold label is rewritten first; the line is then
// cut at protected fragments and each prose chunk is colored, with inBold
// carried across protected cuts.
func decorateLine(line string, palette Palette, open, close string) string {
	if m := leadLabelRe.FindStringSubmatchIndex(line); m != nil {
		line = line[:m[4]] + withLabelIcon(line[m[4]:m[5]]) + line[m[6]:]
	}
	spans := protectedSpans(line)
	var sb strings.Builder
	sb.Grow(len(line) + 32)
	inBold := ""
	proseStart := 0
	for _, s := range spans {
		sb.WriteString(decorateProse(line[proseStart:s.start], palette, open, close, &inBold))
		protected := line[s.start:s.end]
		if inBold != "" {
			// A code span ends in a foreground reset; restore the bold color
			// after it (upstream keeps inBold across the split point).
			sb.WriteString(protected + open)
		} else {
			sb.WriteString(protected)
		}
		proseStart = s.end
	}
	sb.WriteString(decorateProse(line[proseStart:], palette, open, close, &inBold))
	// An unclosed span (streaming or literal asterisks) ends its color at the
	// line end.
	if inBold != "" {
		sb.WriteString(close)
	}
	return sb.String()
}

// decorateProse colors one unprotected chunk: action lead-ins are rewritten
// first (upstream inserts the icon in place), then a single left-to-right
// scan over bold markers, [[pills]], the ✓✔/✗✘/→ marks and [├└]── connectors
// colors each token where it stands. inBold is the open bold marker ("**" or
// "__") or "" outside a span; the scan updates it in place so a mark, pill
// or connector inside a bold span restores the bold color (and, where its
// close also ends the bold weight, the weight) after its own reset.
func decorateProse(prose string, palette Palette, open, close string, inBold *string) string {
	if prose == "" {
		return ""
	}
	if os.Getenv("PI_EMPHASIS_PILL_ICONS") != "0" {
		prose = actionLabel.ReplaceAllStringFunc(prose, func(m string) string {
			sub := actionLabel.FindStringSubmatch(m)
			// RE2's (?i) uses Unicode simple folding, so a word like "ſee"
			// (U+017F folds to 's') matches the pattern without being an
			// action word. An empty icon means the fold hit no table key:
			// the match returns verbatim instead of injecting a bare space
			// after `**`, which would break the bold delimiter.
			icon := actionIconByWord[asciiLower(sub[2])]
			if icon == "" {
				return m
			}
			return sub[1] + icon + " " + sub[2] + sub[3]
		})
	}
	return proseScanRe.ReplaceAllStringFunc(prose, func(match string) string {
		sub := proseScanRe.FindStringSubmatch(match)
		slashes, marker, label, mark, tree := sub[1], sub[2], sub[3], sub[4], sub[5]
		switch {
		case mark != "":
			// Marks colored inside a bold span restore the bold foreground
			// after their own reset (upstream appends `open` when inBold is
			// set); their close \x1b[39m leaves the weight alone.
			colorize := markColorize(mark, palette)
			if colorize == nil {
				return match
			}
			reopen := ""
			if *inBold != "" {
				reopen = open
			}
			return colorize(match) + reopen
		case tree != "":
			// A faint dim closes \x1b[22m\x1b[39m: both the bold weight and
			// its foreground are restored after the connector (upstream's
			// reopen()). A non-faint close keeps the weight and needs the
			// color only.
			if palette.Tree == nil {
				return match
			}
			reopen := ""
			if *inBold != "" {
				reopen = open
				if treeClosesBold(palette.Tree) {
					reopen = "\x1b[1m" + open
				}
			}
			return palette.Tree(match) + reopen
		case strings.HasPrefix(match, "[["):
			// Pills close bold and inverse; reopen restores the bold weight
			// and color (upstream reopen()).
			if palette.Pills == nil || strings.TrimSpace(label) == "" {
				return match
			}
			reopen := ""
			if *inBold != "" {
				reopen = "\x1b[1m" + open
			}
			return pill(label, palette) + reopen
		}
		if len(slashes)%2 == 1 {
			return match // escaped marker
		}
		if *inBold == "" {
			*inBold = marker
			return slashes + marker + open
		}
		if *inBold == marker {
			*inBold = ""
			return slashes + close + marker
		}
		return match
	})
}

// markColorize picks the palette colorizer one single-rune mark takes.
func markColorize(mark string, palette Palette) Colorize {
	switch mark {
	case "✓", "✔":
		return palette.Passed
	case "✗", "✘":
		return palette.Failed
	}
	return palette.Arrow // →
}

// treeClosesBold reports whether the Tree colorizer's close ends an outer
// bold span: the faint-dim close starts \x1b[22m, the non-faint host close
// \x1b[39m does not. The probe is a NUL sentinel, as splitColorize marks the
// wrap point.
func treeClosesBold(colorize Colorize) bool {
	const sentinel = "\x00"
	wrapped := colorize(sentinel)
	if idx := strings.Index(wrapped, sentinel); idx >= 0 {
		return strings.HasPrefix(wrapped[idx+len(sentinel):], "\x1b[22m")
	}
	return false
}

// ColorBold is the earlier API name, kept for callers of the first release
// (upstream colorBold): a bare Colorize builds a palette with only Bold set.
func ColorBold(markdown string, palette any) string {
	switch p := palette.(type) {
	case Palette:
		return Decorate(markdown, p)
	case *Palette:
		if p == nil {
			return markdown
		}
		return Decorate(markdown, *p)
	case Colorize:
		return Decorate(markdown, Palette{Bold: p})
	case func(string) string:
		return Decorate(markdown, Palette{Bold: Colorize(p)})
	}
	return markdown
}

// stripANSI removes SGR escapes (test-side helper, kept unexported).
func stripANSI(text string) string {
	return ansiRe.ReplaceAllString(text, "")
}
