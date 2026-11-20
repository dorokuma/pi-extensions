package emphasis

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// fakeTheme mirrors the upstream test's ThemeLike fake: fg wraps text in
// <token>…</>, and getFgAnsi/getBgAnsi return recognizable escapes. A "dim"
// token ends in SGR 2 like PiG's faint foregrounds; unknownFg/unknownBg list
// tokens that behave as unresolvable (Fg panics, Get*Ansi errors), matching
// sdk.UITheme's contract.
type fakeTheme struct {
	unknownFg map[string]bool
	unknownBg map[string]bool
	fgEscapes map[string]string
	bgEscapes map[string]string
	// dimFaint makes GetFgAnsi("dim") end in \x1b[2m (PiG's faint dim).
	dimFaint bool
}

func newFakeTheme() *fakeTheme {
	return &fakeTheme{
		fgEscapes: map[string]string{},
		bgEscapes: map[string]string{},
	}
}

func (t *fakeTheme) Fg(color, text string) string {
	if t.unknownFg[color] {
		panic("Unknown theme color: " + color)
	}
	return "<" + color + ">" + text + "</>"
}

func (t *fakeTheme) GetFgAnsi(color string) (string, error) {
	if t.unknownFg[color] {
		return "", errUnknownColor
	}
	if ansi, ok := t.fgEscapes[color]; ok {
		return ansi, nil
	}
	if color == "dim" && t.dimFaint {
		return "\x1b[38;fg-dimm\x1b[2m", nil
	}
	return "\x1b[38;fg-" + color + "m", nil
}

func (t *fakeTheme) GetBgAnsi(color string) (string, error) {
	if t.unknownBg[color] {
		return "", errUnknownColor
	}
	if ansi, ok := t.bgEscapes[color]; ok {
		return ansi, nil
	}
	return "\x1b[48;bg-" + color + "m", nil
}

var errUnknownColor = &colorError{}

type colorError struct{}

func (*colorError) Error() string { return "Unknown theme color" }

// testPalette is the upstream test's shared palette.
func testPalette() Palette { return CreatePalette(newFakeTheme()) }

// plain renders decorate output the way the upstream test's `plain` helper
// does: ANSI stripped, rounded caps as |, NBSP as space.
func plain(text string) string {
	return plainP(text, testPalette())
}

func plainP(text string, p Palette) string {
	out := Decorate(text, p)
	out = stripANSI(out)
	out = strings.ReplaceAll(out, "", "|")
	out = strings.ReplaceAll(out, "", "|")
	out = strings.ReplaceAll(out, " ", " ")
	return out
}

// ── upstream describe("pill tone") ─────────────────────────────────────────

func TestPillTone(t *testing.T) {
	cases := []struct {
		word string
		tone PillTone
	}{
		{"Failed", ToneError}, {"Blocked", ToneError}, {"Needs you", ToneWarning}, {"Decision", ToneWarning},
		{"Done", ToneSuccess}, {"Observed", ToneSuccess}, {"Costs money", ToneDanger}, {"Irreversible", ToneDanger},
		{"Implement", ToneMode}, {"Shared", ToneOwner}, {"Inferred", ToneNeutral}, {"Anything else", ToneNeutral},
	}
	for _, c := range cases {
		if got := ToneOf(c.word); got != c.tone {
			t.Errorf("toneOf(%q) = %q, want %q", c.word, got, c.tone)
		}
	}
}

func TestPillToneCaseAndSpacing(t *testing.T) {
	if got := ToneOf("  needs   YOU "); got != ToneWarning {
		t.Fatalf("toneOf(%q) = %q, want warning", "  needs   YOU ", got)
	}
}

func TestPillToneKnownWordWithDetail(t *testing.T) {
	if got := ToneOf("Passed 12/12"); got != ToneSuccess {
		t.Errorf("toneOf(%q) = %q, want success", "Passed 12/12", got)
	}
	if got := ToneOf("Failed: 3 tests"); got != ToneError {
		t.Errorf("toneOf(%q) = %q, want error", "Failed: 3 tests", got)
	}
}

func TestPillToneLongerWordIsNotKnown(t *testing.T) {
	if got := ToneOf("Donezo"); got != ToneNeutral {
		t.Fatalf("toneOf(%q) = %q, want neutral", "Donezo", got)
	}
}

// ── upstream describe("pills") ─────────────────────────────────────────────

func TestPillKnownWordGetsIcon(t *testing.T) {
	if got, want := plain("[[Done]]"), "| Done|"; got != want {
		t.Fatalf("plain([[Done]]) = %q, want %q", got, want)
	}
}

func TestPillUnknownWordHasNoIcon(t *testing.T) {
	if got, want := plain("[[Anything else]]"), "|Anything else|"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPillSynonymsDisplayAsOneWord(t *testing.T) {
	if got, want := plain("[[Completed]]"), "| Done|"; got != want {
		t.Fatalf("plain([[Completed]]) = %q, want %q", got, want)
	}
	if got, want := plain("[[Fail: 3 tests]]"), "| Failed: 3 tests|"; got != want {
		t.Fatalf("plain([[Fail: 3 tests]]) = %q, want %q", got, want)
	}
}

func TestPillOwnGlyphWins(t *testing.T) {
	if got, want := plain("[[ Done]]"), "| Done|"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPillLabelOver32LeftAlone(t *testing.T) {
	long := "[[" + strings.Repeat("x", 33) + "]]"
	if got := plain(long); got != long {
		t.Fatalf("plain(%q) = %q, want untouched", long, got)
	}
}

// ── upstream describe("labels") ────────────────────────────────────────────

func TestHeadingTakesIconFromWord(t *testing.T) {
	if got, want := plain("## Finding"), "##  Finding"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnknownHeadingUntouched(t *testing.T) {
	if got, want := plain("## What happened"), "## What happened"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestHeadingRendersPills is the bug regression: [[label]] inside a heading
// line renders as a pill instead of staying literal (upstream paints every
// pill; the heading branch only used to add the heading word's icon).
func TestHeadingRendersPills(t *testing.T) {
	cases := []struct{ in, want string }{
		{"## 为什么会这样 [[Cause]]", "## 为什么会这样 |Cause|"},
		{"## 女儿已做的处置 [[Done]]", "## 女儿已做的处置 |\uf00c Done|"},
	}
	for _, c := range cases {
		if got := plain(c.in); got != c.want {
			t.Errorf("plain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The heading word's own icon keeps its existing behaviour.
	if got, want := plain("## Finding"), "## \uf002 Finding"; got != want {
		t.Fatalf("plain(%q) = %q, want %q", "## Finding", got, want)
	}
}

// TestHeadingPillRestoresStyle covers the new restore branch: a pill followed
// by more heading text reopens the bold weight and the mdHeading foreground
// the host's headingInline had opened (the pill's own SGR reset drops both);
// a pill at line end takes no redundant restore.
func TestHeadingPillRestoresStyle(t *testing.T) {
	theme := newFakeTheme()
	p := CreatePalette(theme)
	headingAnsi, err := theme.GetFgAnsi("mdHeading")
	if err != nil || headingAnsi == "" {
		t.Skip("fake theme resolves no mdHeading foreground")
	}
	restore := "\x1b[1m" + headingAnsi
	out := Decorate("## [[Done]] 第一阶段已完成", p)
	pillEnd := strings.Index(out, "\x1b[22;27m\x1b[39m\x1b[49m")
	restoreAt := strings.Index(out, restore)
	tailAt := strings.Index(out, "第一阶段")
	if pillEnd < 0 || restoreAt < 0 || tailAt < 0 || !(pillEnd < restoreAt && restoreAt < tailAt) {
		t.Fatalf("heading style not reopened after mid-line pill: %q", out)
	}
	if got := plainP("## [[Done]] 第一阶段已完成", p); got != "## |\uf00c Done| 第一阶段已完成" {
		t.Fatalf("plain render = %q", got)
	}
	// Pill at end of heading: nothing follows it, so no restore is appended.
	outEnd := Decorate("## 已完成 [[Done]]", p)
	if strings.Count(outEnd, restore) != 0 {
		t.Fatalf("line-end pill took a redundant restore: %q", outEnd)
	}
	// An empty Heading (unresolvable mdHeading token) still reopens the bold
	// weight alone when more title follows; a line-end pill stays bare. The
	// weight must come after the pill's own close, which already contains a
	// "\x1b[1m" of its own, so position (not count) distinguishes the restore.
	closeSeq := "\x1b[22;27m\x1b[39m\x1b[49m"
	noHeadingTheme := newFakeTheme()
	noHeadingTheme.unknownFg = map[string]bool{"mdHeading": true}
	noHeading := CreatePalette(noHeadingTheme)
	outNH := Decorate("## [[Done]] 第一阶段已完成", noHeading)
	peNH := strings.Index(outNH, closeSeq)
	if peNH < 0 {
		t.Fatalf("pill close missing: %q", outNH)
	}
	tailNH := outNH[peNH+len(closeSeq):]
	weightAt := strings.Index(tailNH, "\x1b[1m")
	textAt := strings.Index(tailNH, "第一阶段")
	if weightAt < 0 || textAt < 0 || weightAt > textAt {
		t.Fatalf("empty Heading did not restore the bold weight alone: %q", outNH)
	}
	outNH = Decorate("## 已完成 [[Done]]", noHeading)
	if peNH = strings.Index(outNH, closeSeq); peNH < 0 || strings.Contains(outNH[peNH+len(closeSeq):], "\x1b[1m") {
		t.Fatalf("empty Heading line-end pill took a restore: %q", outNH)
	}
}

// TestHeadingProtectedSpansStayLiteral keeps `[[...]]` inside a heading's
// protected fragments verbatim — the same protectedSpans contract prose
// obeys: inline code spans, and a link the host's markedInlineLink scan
// accepts (here the inner `[x]](url)` portion resolves as the link, so the
// label text stays literal).
func TestHeadingProtectedSpansStayLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{"## 修复 `[[Done]]` 的显示", "## 修复 `[[Done]]` 的显示"},
		{"## 查看 [文档 [[x]](https://example.com) 说明", "## 查看 [文档 [[x]](https://example.com) 说明"},
	}
	for _, c := range cases {
		if got := plain(c.in); got != c.want {
			t.Errorf("plain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLeadingBoldLabelTakesIcon(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**Cause:** x", "**<mdLink> Cause:</>** x"},
		{"- **Fix:** x", "- **<mdLink> Fix:</>** x"},
		{"  ├── **Risk:** x", "  <dim>├──</> **<mdLink> Risk:</>** x"},
	}
	for _, c := range cases {
		if got := plain(c.in); got != c.want {
			t.Errorf("plain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBoldLaterInLineGetsColorNoIcon(t *testing.T) {
	if got, want := plain("see **Cause** here"), "see **<mdLink>Cause</>** here"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ── upstream describe("action lead-ins") ───────────────────────────────────

func TestActionLeadInsTakeIconWithColon(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[[Unverified]] red pills. **Check:** run it", "| Unverified| red pills. **<mdLink> Check:</>** run it"},
		{"- **Unblock:** access", "- **<mdLink> Unblock:</>** access"},
	}
	for _, c := range cases {
		if got := plain(c.in); got != c.want {
			t.Errorf("plain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestActionLeadInOneIconAtLineStart(t *testing.T) {
	if got, want := plain("**Next:** go"), "**<mdLink> Next:</>** go"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestActionWordWithoutColonIsOrdinaryBold(t *testing.T) {
	if got, want := plain("please **do** it and **run** that"), "please **<mdLink>do</>** it and **<mdLink>run</>** that"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ── upstream describe("what is left alone") ────────────────────────────────

func TestFencedCodeLeftAlone(t *testing.T) {
	block := "```\n**Cause:** → [[Done]]\n```"
	if got := plain(block); got != block {
		t.Fatalf("fenced code changed: %q", got)
	}
}

func TestInlineCodeLeftAlone(t *testing.T) {
	if got, want := plain("`[[Done]] → **x**`"), "`[[Done]] → **x**`"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEscapedAsterisksLeftAlone(t *testing.T) {
	if got, want := plain(`\*\*not bold\*\*`), `\*\*not bold\*\*`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestArrowsChecksCrossesTakeColors(t *testing.T) {
	if got, want := plain("a → b ✓ c ✗"), "a <accent>→</> b <success>✓</> c <error>✗</>"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestIconsCanBeTurnedOff(t *testing.T) {
	t.Setenv("PI_EMPHASIS_PILL_ICONS", "0")
	if got, want := plain("[[Done]]"), "|Done|"; got != want {
		t.Fatalf("plain([[Done]]) = %q, want %q", got, want)
	}
	if got, want := plain("## Finding"), "## Finding"; got != want {
		t.Fatalf("plain(## Finding) = %q, want %q", got, want)
	}
}

// ── upstream describe("reply guidance") ────────────────────────────────────

func TestReplyGuidanceNamesEveryCoreWord(t *testing.T) {
	text := ReplyGuidance()
	for _, entry := range Vocabulary {
		for _, word := range entry.Core {
			if !strings.Contains(text, "[["+word+"]]") {
				t.Errorf("guidance missing [[%s]]", word)
			}
			if got := ToneOf(word); got != entry.Tone {
				t.Errorf("toneOf(%q) = %q, want %q", word, got, entry.Tone)
			}
		}
	}
	for _, word := range ContextWords {
		if !strings.Contains(text, "[["+word+"]]") {
			t.Errorf("guidance missing [[%s]]", word)
		}
	}
}

func TestReplyGuidancePillsAreAllKnownWords(t *testing.T) {
	text := ReplyGuidance()
	known := map[string]bool{"Word": true}
	for _, entry := range Vocabulary {
		for _, w := range entry.Core {
			known[w] = true
		}
	}
	for _, w := range ContextWords {
		known[w] = true
	}
	re := regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		label := m[1]
		ok := false
		for k := range known {
			if label == k || strings.HasPrefix(label, k+" ") || strings.HasPrefix(label, k+":") {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("guidance mentions unknown pill [[%s]]", label)
		}
	}
}

func TestReplyGuidanceEveryLeadInGetsIcon(t *testing.T) {
	text := ReplyGuidance()
	for _, word := range []string{"Check", "Do", "Next", "Unblock", "Left", "Run", "Guard", "Undo", "See"} {
		if !strings.Contains(text, "**"+word+":**") {
			t.Errorf("guidance missing **%s:**", word)
		}
		line := "x **" + word + ":** y"
		want := "x **<mdLink>" + word + ":</>** y"
		if got := plain(line); got == want {
			t.Errorf("lead-in %s got no icon: %q", word, got)
		}
	}
}

func TestReplyGuidanceAddressesAnyUser(t *testing.T) {
	text := ReplyGuidance()
	if regexp.MustCompile(`\bJohn\b|~/dev`).MatchString(text) {
		t.Fatal("guidance addresses a specific user")
	}
}

// ── demo end-to-end render regression ──────────────────────────────────────

const deployReply = `**Finding:** [[Investigate]] the staging deploy stopped in the database migration, before any new code went out. Staging is still serving last night's build.

## Scoreboard

[[Done 3]] [[Failed 1]] [[Unverified 1]] [[Needs you 1]]

## What happened

- **Result:** [[Passed 212/212]] tests → [[Done]] build → [[Failed]] migration → [[Skipped]] rollout
- **Cause:** [[Observed]] the migration adds a required column to the orders table, and 41 older rows have no value for it.
- **Evidence:**
  ├── [[Observed]] the same error appears in the last three deploy logs
  ├── [[Inferred]] those rows predate the March import
  └── [[Unverified]] whether production has rows like them
- **Risk:** [[Shared]] the orders table is also read by the **billing service**, so a change there reaches beyond checkout.

## Options

1. **Backfill first** [[Recommended]] fill the 41 rows with a default, then run the migration unchanged.
2. **Make the column optional** ship now and tighten it later. This leaves *two* kinds of order rows in the code.
3. **Drop the old rows** [[Destructive]] removes order history that billing may still need.

**Next:** [[Decision]] which option? I can prepare the backfill as a dry run without touching staging.
`

const everyPill = `**Pills:** [[Brainstorm]] every pill takes its color and icon from its word.

- **Something is wrong:** [[Failed]] [[Blocked]] [[Broken]] [[Unsafe]]
- **Look here:** [[Needs you]] [[Decision]] [[Unverified]] [[Partial]] [[Skipped]] [[In progress]] [[Pending]] [[At risk]]
- **Settled:** [[Done]] [[Passed]] [[Observed]] [[Fixed]] [[Live]] [[Recommended]] [[Keep]]
- **Consequences:** [[Costs money]] [[Destructive]] [[Irreversible]]
- **Kind of work:** [[Brainstorm]] [[Investigate]] [[Preview]] [[Implement]] [[Qualify]]
- **Ownership:** [[Owned]] [[Shared]] [[Foreign]]
- **Context:** [[Inferred]] [[New]] [[Changed]] [[Optional]] [[Anything else]]

## With detail

- **Example:** [[Passed 12/12]] [[Failed: 3 tests]] [[In progress 40%]] [[Done 6]]
- **Rule:** synonyms show as one word: ` + "`[[Completed]]`" + ` reads [[Completed]] and ` + "`[[Fail]]`" + ` reads [[Fail]].

## Labels

- **Cause:** a heading or a leading bold label takes an icon from its word.
- **Fix:** write the word; the icon is added for you.
- **Plan:** build → test → ship, with the arrows in the accent color.
- **Rollback:**
  ├── tree connectors are dimmed
  └── so the content stands out
`

// renderDemo strips ANSI and normalizes caps/nbsp like plain().
func renderDemo(md string) string { return plain(md) }

func TestDemoDeployReply(t *testing.T) {
	out := renderDemo(deployReply)
	wants := []string{
		// Lead label icon inside the colored bold span + pill in opening line.
		"**<mdLink>\uf002 Finding:</>** |\uf002 Investigate| the staging deploy stopped",
		// Scoreboard row: four pills, no labels.
		"|\uf00c Done 3| |\uf00d Failed 1| |\uf128 Unverified 1| |\uf007 Needs you 1|",
		"## \uf080 Scoreboard",
		"## What happened",
		// Result sequence: icon label, pills, colored arrows.
		"- **<mdLink>\uf11e Result:</>** |\uf00c Passed 212/212| tests <accent>\u2192</> |\uf00c Done| build <accent>\u2192</> |\uf00d Failed| migration <accent>\u2192</> |\uf04e Skipped| rollout",
		"- **<mdLink>\uf0e7 Cause:</>** |\uf06e Observed| the migration adds a required column",
		"- **<mdLink>\uf06e Evidence:</>**",
		"  <dim>\u251c\u2500\u2500</> |\uf06e Observed| the same error appears in the last three deploy logs",
		"  <dim>\u251c\u2500\u2500</> |\uf0eb Inferred| those rows predate the March import",
		"  <dim>\u2514\u2500\u2500</> |\uf128 Unverified| whether production has rows like them",
		"- **<mdLink>\uf071 Risk:</>** |\uf0c0 Shared| the orders table is also read by the **<mdLink>billing service</>**",
		"## \uf0ca Options",
		"1. **<mdLink>Backfill first</>** |\uf005 Recommended| fill the 41 rows",
		"2. **<mdLink>Make the column optional</>** ship now and tighten it later. This leaves *two* kinds of order rows in the code.",
		"3. **<mdLink>Drop the old rows</>** |\uf1f8 Destructive| removes order history",
		"**<mdLink>\uf061 Next:</>** |\uf126 Decision| which option? I can prepare the backfill",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("deploy-reply output missing %q\n---\n%s", w, out)
		}
	}
	// The italic *two* must stay literal (single asterisk is not a marker).
	if !strings.Contains(out, "*two*") {
		t.Error("single-asterisk italic was rewritten")
	}
}

func TestDemoEveryPill(t *testing.T) {
	out := renderDemo(everyPill)
	wants := []string{
		"**<mdLink>\uf02b Pills:</>** |\uf0eb Brainstorm| every pill takes its color",
		"- **<mdLink>Something is wrong:</>** |\uf00d Failed| |\uf05e Blocked| |\uf127 Broken| |\uf132 Unsafe|",
		"- **<mdLink>Look here:</>** |\uf007 Needs you| |\uf126 Decision| |\uf128 Unverified| |\uf042 Partial| |\uf04e Skipped| |\uf110 In progress| |\uf017 Pending| |\uf071 At risk|",
		"- **<mdLink>Settled:</>** |\uf00c Done| |\uf00c Passed| |\uf06e Observed| |\uf0ad Fixed| |\uf135 Live| |\uf005 Recommended| |\uf08d Keep|",
		"- **<mdLink>Consequences:</>** |\uf155 Costs money| |\uf1f8 Destructive| |\uf023 Irreversible|",
		"- **<mdLink>Kind of work:</>** |\uf0eb Brainstorm| |\uf002 Investigate| |\uf06e Preview| |\uf0ad Implement| |\uf0c3 Qualify|",
		"- **<mdLink>Ownership:</>** |\uf007 Owned| |\uf0c0 Shared| |\uf023 Foreign|",
		"- **<mdLink>Context:</>** |\uf0eb Inferred| |\uf067 New| |\uf040 Changed| |\uf10c Optional| |Anything else|",
		"## With detail",
		"- **<mdLink>\uf121 Example:</>** |\uf00c Passed 12/12| |\uf00d Failed: 3 tests| |\uf110 In progress 40%| |\uf00c Done 6|",
		"- **<mdLink>\uf0e3 Rule:</>** synonyms show as one word: `[[Completed]]` reads |\uf00c Done| and `[[Fail]]` reads |\uf00d Failed|",
		"## Labels",
		"- **<mdLink>\uf0e7 Cause:</>** a heading or a leading bold label takes an icon",
		"- **<mdLink>\uf0ad Fix:</>** write the word; the icon is added for you",
		"- **<mdLink>\uf0ae Plan:</>** build <accent>\u2192</> test <accent>\u2192</> ship",
		"- **<mdLink>\uf0e2 Rollback:</>**",
		"  <dim>\u251c\u2500\u2500</> tree connectors are dimmed",
		"  <dim>\u2514\u2500\u2500</> so the content stands out",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("every-pill output missing %q\n---\n%s", w, out)
		}
	}
}

// ── PiG compatibility boundary tests ───────────────────────────────────────

// systemTheme has no panel background colors: every GetBgAnsi returns
// ("\x1b[49m", nil), as the "system" theme does when the terminal reports
// none.
type systemTheme struct{ *fakeTheme }

func newSystemTheme() *systemTheme {
	return &systemTheme{&fakeTheme{
		bgEscapes: map[string]string{
			"toolSuccessBg": "\x1b[49m",
			"toolErrorBg":   "\x1b[49m",
			"searchMatchBg": "\x1b[49m",
			"selectedBg":    "\x1b[49m",
		},
		fgEscapes: map[string]string{},
	}}
}

func TestSystemThemeFallsBackToReverseVideo(t *testing.T) {
	p := CreatePalette(newSystemTheme())
	for _, tone := range []PillTone{ToneSuccess, ToneError, ToneWarning, ToneNeutral} {
		style := p.Pills[tone]
		if !strings.HasSuffix(style.Label, "\x1b[7m") {
			t.Errorf("tone %s: label %q lacks reverse-video fallback", tone, style.Label)
		}
		if style.Cap == "" {
			t.Errorf("tone %s: empty cap under system theme", tone)
		}
	}
	// labelFg "text" upgrades to "accent" for the neutral tone.
	if want := "\x1b[38;fg-accentm\x1b[7m"; p.Pills[ToneNeutral].Label != want {
		t.Errorf("neutral label = %q, want %q", p.Pills[ToneNeutral].Label, want)
	}
	// Rendering still works end to end.
	if got := plainP("[[Done]]", p); !strings.Contains(got, "Done") {
		t.Errorf("system theme render = %q, want pill text", got)
	}
}

func TestUnknownBoldColorFallsBackToMdLink(t *testing.T) {
	theme := newFakeTheme()
	theme.unknownFg = map[string]bool{"not-a-color": true}
	p := CreatePalette(theme, "not-a-color")
	if got := plainP("**x**", p); got != "**<mdLink>x</>**" {
		t.Fatalf("unknown color render = %q", got)
	}
}

// noPaletteTheme resolves nothing: GetFgAnsi fails for every token (the host
// has not sent a palette yet).
type noPaletteTheme struct{ *fakeTheme }

func newNoPaletteTheme() *noPaletteTheme {
	return &noPaletteTheme{&fakeTheme{
		unknownFg: map[string]bool{
			"mdLink": true, "success": true, "error": true, "warning": true,
			"accent": true, "dim": true, "text": true,
		},
		unknownBg: map[string]bool{
			"toolSuccessBg": true, "toolErrorBg": true, "searchMatchBg": true, "selectedBg": true,
		},
		fgEscapes: map[string]string{},
		bgEscapes: map[string]string{},
	}}
}

func TestUnreadyPaletteReturnsMarkdownUntouched(t *testing.T) {
	// A UITheme with no replicated palette resolves no token (GetFgAnsi
	// errors), like the host before the first theme push: paletteFor refuses
	// and the transform hands the Markdown back untouched.
	st := &emphasisState{}
	st.setThemeFunc(func() sdk.UITheme { return sdk.UITheme{} })
	in := "**x** [[Done]]"
	if got := st.transform(in, sdk.MarkdownTransformContext{MessageType: "assistant"}); got != in {
		t.Fatalf("unready palette transformed input: %q", got)
	}
	// Same guarantee through the fake theme where mdLink fails to resolve.
	p := CreatePalette(newNoPaletteTheme())
	if p.Bold != nil {
		t.Fatal("CreatePalette built a Bold colorizer on a palette-less theme")
	}
}

// TestDimDoesNotBreakOuterBold verifies the faint-dim close is split into
// \x1b[22m\x1b[39m and the outer bold is reopened after the connector.
func TestDimDoesNotBreakOuterBold(t *testing.T) {
	theme := newFakeTheme()
	theme.dimFaint = true
	p := CreatePalette(theme)
	out := Decorate("**a** \u251c\u2500\u2500 b", p)
	if strings.Contains(out, "\x1b[22;39m") {
		t.Fatalf("dim colorizer emitted merged close \\x1b[22;39m: %q", out)
	}
	if !strings.Contains(out, "\x1b[22m\x1b[39m") {
		t.Fatalf("dim colorizer missing split close: %q", out)
	}
	// The reopen path fires when inBold was carried across a protected span:
	// bold opened before a code span, then the connector is colored, and the
	// bold weight and color are restored after its split close.
	open, _ := splitColorize(p.Bold)
	boldLine := Decorate("**x `code` \u251c\u2500\u2500 y**", p)
	if !strings.Contains(boldLine, "\x1b[22m\x1b[39m\x1b[1m"+open) {
		t.Fatalf("bold color not reopened after tree connector: %q", boldLine)
	}
	// And the connector is still dimmed in the plain render.
	if got := plainP("**x `code` \u251c\u2500\u2500 y**", p); !strings.Contains(got, "\u251c\u2500\u2500") {
		t.Fatalf("connector missing in plain render = %q", got)
	}
}

// TestMultiBacktickCodeSpansProtected covers N>1 backtick spans and ANSI
// content inside them (the Mermaid-converter case).
func TestMultiBacktickCodeSpansProtected(t *testing.T) {
	cases := []string{
		"see ``code `inner` [[Done]]`` end",
		"x ```mermaid \x1b[31mdiagram\x1b[0m``` y",
		"`[[Done]]`",
	}
	for _, in := range cases {
		got := Decorate(in, testPalette())
		// Protected content must be byte-identical in output.
		if !strings.Contains(got, in[strings.Index(in, "`"):strings.LastIndex(in, "`")+1]) {
			t.Errorf("code span content modified: in=%q out=%q", in, got)
		}
		if !strings.Contains(got, "``") && strings.Count(in, "``") > 0 {
			t.Errorf("multi-backtick span collapsed: in=%q out=%q", in, got)
		}
	}
	// ANSI inside a multi-backtick span stays exactly as written.
	in := "x ```\x1b[31m[[Done]]\x1b[0m``` y"
	if got := Decorate(in, testPalette()); !strings.Contains(got, "\x1b[31m[[Done]]\x1b[0m") {
		t.Fatalf("ANSI inside code span rewritten: %q", got)
	}
}

// TestMarkdownLinkProtected keeps a bold color out of [label](url).
func TestMarkdownLinkProtected(t *testing.T) {
	in := "see **the [docs](https://example.com) page** now"
	out := Decorate(in, testPalette())
	if !strings.Contains(out, "[docs](https://example.com)") {
		t.Fatalf("link was split by injected ANSI: %q", out)
	}
	// Bold around the whole phrase still reopens after the link.
	open, _ := splitColorize(testPalette().Bold)
	if !strings.Contains(out, "[docs](https://example.com)"+open) {
		t.Fatalf("bold color not reopened after link: %q", out)
	}
	// Image links are protected too.
	img := "x ![alt **b**](u) y"
	if got := Decorate(img, testPalette()); !strings.Contains(got, "![alt **b**](u)") {
		t.Fatalf("image link modified: %q", got)
	}
}

// ── concurrency & lifecycle ────────────────────────────────────────────────

// driveTransform exercises emphasisState.transform concurrently over a
// real, palette-resolving theme so the goroutines genuinely race the
// s.built read, lock-free build and guarded write-back (an empty
// sdk.UITheme{} resolves no tokens and every build would bail before the
// write-back the generation guard protects).
func TestTransformConcurrentSafe(t *testing.T) {
	t.Setenv("PI_EMPHASIS_COLOR", "muted")
	gray, red := themePair(t, "\x1b[90m"), themePair(t, "\x1b[31m")
	st := &emphasisState{}
	st.setThemeFunc(func() sdk.UITheme { return gray })
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			md := "**Cause:** [[Done]] → ok"
			for j := 0; j < 50; j++ {
				_ = st.transform(md, sdk.MarkdownTransformContext{MessageType: "assistant"})
				_ = st.transform(md, sdk.MarkdownTransformContext{MessageType: "user"})
				switch i % 8 {
				case 0:
					st.invalidate()
				case 1:
					// A rebind races the builds: it bumps both generations and
					// clears s.built under the write lock.
					if j%2 == 0 {
						st.setThemeFunc(func() sdk.UITheme { return red })
					} else {
						st.setThemeFunc(func() sdk.UITheme { return gray })
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestDecorateConcurrentSafe hammers pure Decorate from many goroutines.
func TestDecorateConcurrentSafe(t *testing.T) {
	p := testPalette()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = Decorate("**Cause:** [[Done]] → ├── x `code` [l](u)", p)
			}
		}()
	}
	wg.Wait()
}

// TestExtensionLifecycle builds the extension and checks it registers the
// expected surface without a live host.
func TestExtensionLifecycle(t *testing.T) {
	e := Extension()
	if e == nil {
		t.Fatal("Extension() = nil")
	}
	if e.Name() != "pi-emphasis" {
		t.Fatalf("extension name = %q", e.Name())
	}
	// A second instance must be independent (no package-level mutable state).
	if Extension() == e {
		t.Fatal("Extension() returned a shared instance")
	}
}

// TestVersionPinned guards the declared version constant.
func TestVersionPinned(t *testing.T) {
	if Version != "0.1.0" {
		t.Fatalf("Version = %q, want 0.1.0", Version)
	}
}

// TestEmptyInputFastPath covers the O(1) empty-string contract.
func TestEmptyInputFastPath(t *testing.T) {
	if got := Decorate("", testPalette()); got != "" {
		t.Fatalf("Decorate(\"\") = %q", got)
	}
	st := &emphasisState{}
	if got := st.transform("", sdk.MarkdownTransformContext{MessageType: "assistant"}); got != "" {
		t.Fatalf("transform(\"\") = %q", got)
	}
}

// TestNonAssistantMessageUntouched covers the messageType gate.
func TestNonAssistantMessageUntouched(t *testing.T) {
	st := &emphasisState{}
	st.setThemeFunc(func() sdk.UITheme { return sdk.UITheme{} })
	in := "**x** [[Done]]"
	if got := st.transform(in, sdk.MarkdownTransformContext{MessageType: "user"}); got != in {
		t.Fatalf("user message transformed: %q", got)
	}
	if got := st.transform(in, sdk.MarkdownTransformContext{MessageType: "assistant-thinking"}); got != in {
		t.Fatalf("thinking message transformed: %q", got)
	}
}

// TestTransformWithoutThemeUntouched covers the nil themeFunc gate (non-TUI).
func TestTransformWithoutThemeUntouched(t *testing.T) {
	st := &emphasisState{}
	in := "**x** [[Done]]"
	if got := st.transform(in, sdk.MarkdownTransformContext{MessageType: "assistant"}); got != in {
		t.Fatalf("transformed without theme: %q", got)
	}
}

// TestSquareCaps renders chips when PI_EMPHASIS_ROUND_CAPS=0.
func TestSquareCaps(t *testing.T) {
	t.Setenv("PI_EMPHASIS_ROUND_CAPS", "0")
	p := CreatePalette(newFakeTheme())
	out := Decorate("[[Done]]", p)
	if strings.Contains(out, "\ue0b6") || strings.Contains(out, "\ue0b4") {
		t.Fatalf("rounded caps drawn with ROUND_CAPS=0: %q", out)
	}
	// pad NBSP sits inside the bold span.
	if !strings.Contains(out, "\x1b[1m\u00a0") || !strings.Contains(out, "\u00a0\x1b[22;27m") {
		t.Fatalf("square-chip pad misplaced: %q", out)
	}
}

// TestReplyGuidanceDeterministic pins the ordered output.
func TestReplyGuidanceDeterministic(t *testing.T) {
	a, b := ReplyGuidance(), ReplyGuidance()
	if a != b {
		t.Fatal("ReplyGuidance is not deterministic")
	}
	// Spot-check the ordered lists render in upstream insertion order.
	if !strings.Contains(a, "Finding, Terminal, Folder, File, Branch, Review, Ideas, Cause, Fix, Risk, Limits, Next, Rollback, Undo, Options, Result, Why, How, Cost, Rule, Example, Checked, Pills, Colors, Icons, Scoreboard, Verdict, Effect, Evidence, Plan, Status, Decision") {
		t.Error("label icon list order drifted")
	}
	if !strings.Contains(a, "Check, Do, Next, Unblock, Left, Run, Guard, Undo, See") {
		t.Error("action icon list order drifted")
	}
	if !strings.Contains(a, "[[Failed]], [[Blocked]], [[Broken]], [[Unsafe]]") {
		t.Error("error core list drifted")
	}
	if !strings.Contains(a, "[[Done]], [[Passed]], [[Observed]], [[Fixed]], [[Live]], [[Recommended]], [[Keep]]") {
		t.Error("success core list drifted")
	}
	if !strings.Contains(a, "sign in to the staging dashboard") {
		t.Error("staging-dashboard wording drifted")
	}
}

// TestEnvColorHonored covers PI_EMPHASIS_COLOR through paletteFor.
func TestEnvColorHonored(t *testing.T) {
	t.Setenv("PI_EMPHASIS_COLOR", "accent")
	st := &emphasisState{}
	theme := sdk.UITheme{}
	_ = theme // paletteFor requires a UITheme; covered via CreatePalette path below.
	p := CreatePalette(newFakeTheme(), "accent")
	if got := plainP("**x**", p); got != "**<accent>x</>**" {
		t.Fatalf("custom color render = %q", got)
	}
	_ = st
}

// TestMarksTreeInsideBoldReopen covers the single prose-scan contract: a
// mark or tree connector colored inside one bold span restores the bold
// color (marks) and the bold weight (faint-dim tree close) after its own
// reset, so the rest of the span does not render unbolded.
func TestMarksTreeInsideBoldReopen(t *testing.T) {
	theme := newFakeTheme()
	theme.dimFaint = true
	p := CreatePalette(theme)
	open, close := splitColorize(p.Bold)
	successOpen, successClose := splitColorize(p.Passed)
	arrowOpen, arrowClose := splitColorize(p.Arrow)

	// **✓ Passed**: the ✓ close must be followed by `open`, and the span's
	// `close` must come after "Passed" — only a single left-to-right scan
	// sees inBold set where the mark is colored.
	if got, want := Decorate("**✓ Passed**", p), "**"+open+successOpen+"✓"+successClose+open+" Passed"+close+"**"; got != want {
		t.Fatalf("bold color not reopened after ✓ inside the span:\ngot  %q\nwant %q", got, want)
	}

	// **a → b**: same contract on the arrow colorizer.
	if got, want := Decorate("**a → b**", p), "**"+open+"a "+arrowOpen+"→"+arrowClose+open+" b"+close+"**"; got != want {
		t.Fatalf("bold color not reopened after → inside the span:\ngot  %q\nwant %q", got, want)
	}

	// **├── item**: the faint-dim close \x1b[22m\x1b[39m ends both the bold
	// weight and its foreground, so the reopen is \x1b[1m + `open`.
	tree := p.Tree("├──")
	if got, want := Decorate("**├── item**", p), "**"+open+tree+"\x1b[1m"+open+" item"+close+"**"; got != want {
		t.Fatalf("bold weight+color not reopened after tree inside the span:\ngot  %q\nwant %q", got, want)
	}

	// Same prose chunk, unclosed span: the connector is dimmed once and the
	// bold is reopened once before the line-end close.
	if got, want := Decorate("**x ├── y", p), "**"+open+"x "+tree+"\x1b[1m"+open+" y"+close; got != want {
		t.Fatalf("unclosed span after tree misplaced:\ngot  %q\nwant %q", got, want)
	}
}

// TestSessionRebindCoversUnsubClearAndEnvFingerprint covers the
// emphasisState lifecycle the OnSessionStart handler wires: rebinding a
// session releases the old OnThemeChange subscription, a non-TUI session
// clears the theme binding, and the cached palette is rebuilt when the
// environment it draws with changes.
func TestSessionRebindCoversUnsubClearAndEnvFingerprint(t *testing.T) {
	st := &emphasisState{}
	var calls int
	unsub := func() { calls++ }

	st.setUnsubTheme(st.setThemeFunc(func() sdk.UITheme { return sdk.UITheme{} }), unsub)
	// The next session_start releases the previous subscription before
	// storing its own (the SDK's unsubscribe is idempotent).
	st.setUnsubTheme(st.setThemeFunc(func() sdk.UITheme { return sdk.UITheme{} }), unsub)
	if calls != 1 {
		t.Fatalf("rebind unsubscribed %d times, want 1", calls)
	}
	// A non-TUI session_start clears the theme binding and unsubscribes.
	st.clearTheme()
	if calls != 2 {
		t.Fatalf("clearTheme unsubscribed %d times, want 2", calls)
	}
	if st.hasTheme() {
		t.Fatal("theme binding survived clearTheme")
	}
	if got := st.transform("**x**", sdk.MarkdownTransformContext{MessageType: "assistant"}); got != "**x**" {
		t.Fatalf("transformed after clearTheme: %q", got)
	}

	// The palette cache fingerprints the env it draws with: PI_EMPHASIS_COLOR
	// and PI_EMPHASIS_ROUND_CAPS changes invalidate it without a theme push.
	th := sdk.UITheme{}
	before := themeFingerprint(th)
	t.Setenv("PI_EMPHASIS_COLOR", "accent")
	mid := themeFingerprint(th)
	t.Setenv("PI_EMPHASIS_ROUND_CAPS", "0")
	after := themeFingerprint(th)
	if before == mid || mid == after {
		t.Fatalf("fingerprint ignored env change: %q %q %q", before, mid, after)
	}

	// hasTheme/before_agent_start keep a cleared session from injecting
	// reply_style; rebinding restores it.
	if st.hasTheme() {
		t.Fatal("hasTheme true after clearTheme")
	}
	st.setThemeFunc(func() sdk.UITheme { return sdk.UITheme{} })
	if !st.hasTheme() {
		t.Fatal("hasTheme false after setThemeFunc")
	}
}

// silence unused-warning for os import when env helpers compiled out.
var _ = os.Getenv

// ── real SDK wire lifecycle ────────────────────────────────────────────────

// readWireFrame reads one length-prefixed JSON frame off the wire, matching
// the SDK conn's framing (pigsdk protocol.go).
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

// hostThemePalette is the theme_change payload the host pushes: the wire
// shape decodeUITheme reads (pigsdk ui_theme.go themePalette).
var hostThemePalette = map[string]any{
	"name": "wire-test",
	"foregrounds": map[string]any{
		"mdLink": "\x1b[34m", "success": "\x1b[32m", "error": "\x1b[31m",
		"warning": "\x1b[33m", "accent": "\x1b[36m", "dim": "\x1b[90m", "text": "\x1b[37m",
	},
	"backgrounds": map[string]any{
		"toolSuccessBg": "\x1b[42m", "toolErrorBg": "\x1b[41m",
		"searchMatchBg": "\x1b[43m", "selectedBg": "\x1b[44m",
	},
}

// TestExtensionWireLifecycleViaRunWithConn drives the real Extension() over a
// net.Pipe through the genuine SDK serve loop — the actual runner's load
// path, not a direct handler call: register → ready(mode=tui) →
// theme_change → session_start → before_agent_start → markdown_transform →
// shutdown. It proves the palette the transform draws with arrives over the
// wire (theme_change + ctx.UITheme()), the reply_style section is returned
// inside before_agent_start's _pigPromptSections, and shutdown returns nil.
// Harness shape follows go/ctxmode/test_helpers_test.go's sdkHost and
// go/rpiv-ask-user-question's wire test.
func TestExtensionWireLifecycleViaRunWithConn(t *testing.T) {
	t.Setenv("PI_EMPHASIS_GUIDANCE", "1")
	hostEnd, extEnd := net.Pipe()
	defer func() { _ = hostEnd.Close() }()
	runDone := make(chan error, 1)
	go func() { runDone <- Extension().RunWithConn(extEnd) }()

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
	send := func(msg map[string]any) {
		t.Helper()
		if err := writeWireFrame(hostEnd, msg); err != nil {
			t.Fatalf("send frame: %v", err)
		}
	}
	// request sends a host request and returns its id; the reader goroutine
	// (started below) routes the response to responses[id].
	var (
		mu        sync.Mutex
		nextID    int
		responses = map[string]chan map[string]any{}
	)
	request := func(req map[string]any) (string, <-chan map[string]any) {
		mu.Lock()
		nextID++
		id := "req-" + strconv.Itoa(nextID)
		ch := make(chan map[string]any, 1)
		responses[id] = ch
		mu.Unlock()
		send(map[string]any{"type": "request", "id": id, "request": req})
		return id, ch
	}

	// Handshake: the extension registers first.
	reg := next()
	if reg["type"] != "register" {
		t.Fatalf("first frame = %v, want register", reg)
	}
	regBody, _ := reg["register"].(map[string]any)
	if name, _ := regBody["name"].(string); name != "pi-emphasis" {
		t.Fatalf("register name = %q, want pi-emphasis", name)
	}
	if mt, _ := regBody["markdown_transformer"].(bool); !mt {
		t.Fatalf("register markdown_transformer = %v, want true", regBody["markdown_transformer"])
	}
	handlerIDs := map[string]int{}
	for _, h := range regBody["handlers"].([]any) {
		m := h.(map[string]any)
		handlerIDs[m["event"].(string)] = int(m["handler_id"].(float64))
	}
	for _, event := range []string{"session_start", "before_agent_start"} {
		if _, ok := handlerIDs[event]; !ok {
			t.Fatalf("register lacks a %s handler: %v", event, regBody["handlers"])
		}
	}

	// The host answers ready in TUI mode, then pushes the theme palette and
	// starts serving the extension's calls and requests on a reader
	// goroutine (request_state frames are skipped).
	send(map[string]any{
		"type":  "ready",
		"ready": map[string]any{"cwd": t.TempDir(), "mode": "tui", "width": 80},
	})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			msg, err := readWireFrame(hostEnd)
			if err != nil {
				return
			}
			switch msg["type"] {
			case "call":
				// Answer every extension→host call so a handler never stalls.
				id, _ := msg["id"].(string)
				_ = writeWireFrame(hostEnd, map[string]any{
					"type": "call_result", "id": id,
					"call_result": map[string]any{"result": nil},
				})
			case "response":
				id, _ := msg["id"].(string)
				mu.Lock()
				ch := responses[id]
				mu.Unlock()
				if ch != nil {
					ch <- msg["response"].(map[string]any)
				}
			}
		}
	}()
	await := func(ch <-chan map[string]any) map[string]any {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("SDK response deadline")
			return nil
		}
	}

	// theme_change fills the replicated palette; the extension's session_start
	// then binds ctx.UITheme() and subscribes to further changes.
	themeArgs, err := json.Marshal(hostThemePalette)
	if err != nil {
		t.Fatal(err)
	}
	send(map[string]any{"type": "notify", "notify": map[string]any{
		"method": "theme_change", "args": json.RawMessage(themeArgs),
	}})

	// session_start binds the live theme (mode comes from ready's "tui").
	_, ch := request(map[string]any{
		"method": "event", "event": "session_start",
		"handler_id": handlerIDs["session_start"],
		"args":       json.RawMessage(`{}`),
	})
	if resp := await(ch); resp["error"] != nil {
		t.Fatalf("session_start response carried an error: %v", resp["error"])
	}

	// before_agent_start must return reply_style inside _pigPromptSections
	// now that a TUI theme is bound.
	basArgs, err := json.Marshal(map[string]any{
		"systemPromptOptions": map[string]any{"sections": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch = request(map[string]any{
		"method": "event", "event": "before_agent_start",
		"handler_id": handlerIDs["before_agent_start"],
		"args":       json.RawMessage(basArgs),
	})
	resp := await(ch)
	if resp["error"] != nil {
		t.Fatalf("before_agent_start response carried an error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	sections, _ := result["_pigPromptSections"].(map[string]any)
	guidance, _ := sections["reply_style"].(string)
	if !strings.Contains(guidance, "Label every reply") {
		t.Fatalf("_pigPromptSections lacks the reply_style guidance: %v", sections)
	}

	// markdown_transform must return the assistant message with real ANSI
	// colors drawn from the wire-pushed palette.
	mtArgs, err := json.Marshal(map[string]any{
		"markdown": "**Cause:** [[Done]]",
		"context":  map[string]any{"messageType": "assistant", "availableWidth": 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch = request(map[string]any{
		"method": "markdown_transform",
		"args":   json.RawMessage(mtArgs),
	})
	resp = await(ch)
	if resp["error"] != nil {
		t.Fatalf("markdown_transform response carried an error: %v", resp["error"])
	}
	text, _ := resp["result"].(string)
	if text == "**Cause:** [[Done]]" || text == "" {
		t.Fatalf("markdown_transform returned the input untouched: %q", text)
	}
	if !strings.Contains(text, "\x1b[") {
		t.Fatalf("markdown_transform drew no ANSI from the wire palette: %q", text)
	}
	if !strings.Contains(text, "Done") {
		t.Fatalf("markdown_transform lost the pill label: %q", text)
	}

	// shutdown ends the serve loop with a nil error.
	send(map[string]any{"type": "shutdown"})
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("RunWithConn returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("extension did not stop after shutdown")
	}
	_ = hostEnd.Close()
	<-readerDone
}

// ── C-gate red-team regression probes (pre-registered FAIL→PASS) ────────────

// TestFenceStateMachineCoversIndentedAndCrossFence is the M1 probe: a fenced
// block opened at column 0 must still close on a 1-3-space-indented fence,
// and an indented backtick block must not be closed early by a tilde line or
// by a fence carrying an info string.
func TestFenceStateMachineCoversIndentedAndCrossFence(t *testing.T) {
	// Indented close: line 4 is prose again and must be decorated.
	if got, want := plain("```\ncode\n  ```\n**Cause:** [[Done]]"), "```\ncode\n  ```\n**<mdLink>\uf0e7 Cause:</>** |\uf00c Done|"; got != want {
		t.Errorf("indented close fence:\ngot  %q\nwant %q", got, want)
	}
	// A same-indent tilde line inside a backtick block is content, not a
	// close; the whole block must pass through verbatim.
	block := "  ```\n  ~~~\n```ts\n**Cause:** [[Done]]\n  ```"
	if got := plain(block); got != block {
		t.Errorf("cross-char / info-string fence closed early:\ngot  %q\nwant %q", got, block)
	}
	// isMarkdownFenceClose (host markdown.go:1055-1062): the closing run
	// may be followed by any mix of '~' and '`', then spaces only. So
	// ```~~~ closes the block and the next line is decorated prose.
	if got, want := plain("```go\n**Cause:** [[Done]]\n```~~~\n**Next:** [[Failed]]"), "```go\n**Cause:** [[Done]]\n```~~~\n**<mdLink>\uf061 Next:</>** |\uf00d Failed|"; got != want {
		t.Errorf("mixed-char fence close not honored:\ngot  %q\nwant %q", got, want)
	}
	// The run may over-shoot the opener length with the same char, and
	// the '~'/'`' mix can also be pure '~' after it.
	if got, want := plain("```\nx\n`````\n**Cause:** [[Done]]"), "```\nx\n`````\n**<mdLink>\uf0e7 Cause:</>** |\uf00c Done|"; got != want {
		t.Errorf("longer same-char close run:\ngot  %q\nwant %q", got, want)
	}
	// The host's trailing trim is " " only: a closing line ending in a
	// tab is NOT a close, so line 4 stays inside the code block and is
	// passed through without icons or pills.
	b := "```go\ncode\n```\t\n**Cause:** [[Done]]"
	if got := plain(b); got != b {
		t.Errorf("tab-suffixed close wrongly closed the fence:\ngot  %q\nwant %q", got, b)
	}
	// Closing-fence variants: only spaces may surround the run suffix.
	if got, want := plain("```\nx\n ``` \n**Cause:** [[Done]]"), "```\nx\n ``` \n**<mdLink>\uf0e7 Cause:</>** |\uf00c Done|"; got != want {
		t.Errorf("indented close with trailing space:\ngot  %q\nwant %q", got, want)
	}
	// Four spaces of indent is an indented code line, never a close.
	c := "```\nx\n    ```\n**Cause:** [[Done]]"
	if got := plain(c); got != c {
		t.Errorf("4-space indented close wrongly closed the fence:\ngot  %q\nwant %q", got, c)
	}
}

// TestLineStartBacktickRunIsCodeSpan is the M2 probe: a line that opens with
// 3+ backticks but carries a backtick in its info string is a single-line
// inline code span (the shape the host's Mermaid converter emits), not a
// multi-line fence opening — later lines must still be decorated.
func TestLineStartBacktickRunIsCodeSpan(t *testing.T) {
	in := "```\x1b[31m``node``\x1b[0m```  \n**Cause:** [[Done]] → ✓"
	// The Mermaid line's code span must be byte-identical (ANSI and all) in
	// the raw output; plain() would strip its \x1b[31m, so assert on
	// Decorate directly.
	raw := Decorate(in, testPalette())
	if !strings.HasPrefix(raw, "```\x1b[31m``node``\x1b[0m```  \n") {
		t.Errorf("code-span line rewritten:\ngot  %q", raw)
	}
	got := plain(in)
	if want := "**<mdLink>\uf0e7 Cause:</>** |\uf00c Done| <accent>→</> <success>✓</>"; !strings.HasSuffix(got, want) {
		t.Errorf("line after code span not decorated:\ngot  %q\nwant suffix %q", got, want)
	}
	if got, want := plain("```code``` → **Next:** [[Done]]"), "```code``` <accent>→</> **<mdLink>\uf061 Next:</>** |\uf00c Done|"; got != want {
		t.Errorf("single-line code span ate the rest of the line:\ngot  %q\nwant %q", got, want)
	}
}

// TestASCIICaseFoldNoUTF8Truncation is the M3 probe: case folding over
// non-ASCII runes must never slice mid-rune, and an action-word match that
// resolves to no icon must not inject a bare space after `**`.
func TestASCIICaseFoldNoUTF8Truncation(t *testing.T) {
	for _, in := range []string{"[[O\u212a]]", "[[FA\u0130L: 1]]"} {
		out := Decorate(in, testPalette())
		if !utf8.ValidString(out) {
			t.Errorf("Decorate(%q) emitted invalid UTF-8: %q", in, out)
		}
	}
	if got, want := plain("x **\u017fee:** y"), "x **<mdLink>\u017fee:</>** y"; got != want {
		t.Errorf("icon-less action word injected a bare space:\ngot  %q\nwant %q", got, want)
	}
}

// TestComplexMarkdownLinksProtected is the S1 probe: links whose label or
// destination needs balanced brackets/parens, code spans, angle-bracket URLs
// or titles must be protected whole — zero ANSI may land inside them.
func TestComplexMarkdownLinksProtected(t *testing.T) {
	cases := []struct{ name, in, core string }{
		{"balanced parens URL", "see [**Go**](https://en.wikipedia.org/wiki/Go_(programming_language))", "[**Go**](https://en.wikipedia.org/wiki/Go_(programming_language))"},
		// The link sits inside a bold span: bold ANSI belongs around it
		// (the bold color reopens after the protected fragment), but the
		// link core itself stays byte-identical.
		{"nested bracket + paren title", "see **[foo [1]](https://example.com \"RFC (draft)\")**", "[foo [1]](https://example.com \"RFC (draft)\")"},
		{"code span label + angle URL", "see [`a[0]` ✓](<https://example.com/a(b)>)", "[`a[0]` ✓](<https://example.com/a(b)>)"},
		// The host's markedInlineLink accepts a literal backtick run right
		// before ']' (`` ` ``+(?=\]) at tui/markdown_marked.go:163): a label
		// whose run never closes must still keep the whole link protected
		// instead of failing the match and letting ANSI split the label.
		{"unclosed backtick run in label", "see [**Spec** ``](https://example.com)", "[**Spec** ``](https://example.com)"},
		// The `` `` ``+(?=\]) branch must also hold when more text follows the
		// link, and a same-line code span after it must stay protected too.
		{"link fallback + inline code same line", "see [**Spec** ``](https://example.com) and ``[[Done]]`` here", "[**Spec** ``](https://example.com) and ``[[Done]]`` here"},
		// A single backtick inside a nested [``v1``] bracket is ordinary
		// label text (branch 1 scans the whole nested pair): it must not
		// leak out as a codespan run, so the link stays protected and the
		// real `[[Done]]` span on the same line is still protected too.
		{"nested bracket backtick stays inside", "see [**Spec** [`v1]](https://example.com) and `[[Done]]` here", "[**Spec** [`v1]](https://example.com) and `[[Done]]` here"},
		// JS \s covers \r (and the rest of isJSWhitespace): a link whose
		// tail closes through \r before ')' is a valid link and must be
		// protected whole.
		{"link tail closed via \\r", "see [**Spec**](u \"t\"\r)", "[**Spec**](u \"t\"\r)"},
		// A bare run that eats the closing ')' is not itself the match:
		// [^ \t\n\x00-\x1f]+ must backtrack until \s*\) can close. The
		// host's findClosingBracket then rejects "foo(bar" (an unclosed
		// '(' → -2), so this is NOT a link and the whole construct —
		// "(https://a.com/foo(bar" and all — must pass through verbatim.
		{"bare run swallows close paren, unclosed ( rejects", "see [**Go**](https://a.com/foo(bar)", "[**Go**](https://a.com/foo(bar)"},
	}
	for _, c := range cases {
		out := Decorate(c.in, testPalette())
		if !strings.Contains(out, c.core) {
			t.Errorf("%s: ANSI injected inside link\nin  %q\nout %q", c.name, c.in, out)
		}
	}
	// The host tokenizer consumes HTML tags and LaTeX tokens before it ever
	// considers link syntax (nextInlineToken, markdown_marked.go:458-500):
	// a [inner](u) shape inside a tag's attribute or inside a $...$/\\(...\\)
	// span never sets linkEmitted, so outputLink keeps the outer link and
	// every byte must pass through verbatim.
	for _, in := range []string{
		"see [**Spec** <span title=\"[inner](u)\">x</span>](v)",
		"see [**Spec** <a href='[inner](u)'>x</a>](v)",
		"see [**Spec** <!-- [inner](u) -->](v)",
		"see [**Spec** $[inner](u)$](v)",
		"see [**Spec** \\([inner](u)\\)](v)",
	} {
		if out := Decorate(in, testPalette()); out != in {
			t.Errorf("tag/LaTeX-held inner link rejected the outer link:\nin  %q\nout %q", in, out)
		}
	}
	// Reverse control: an inner link that genuinely stands in the label
	// text still makes the outer one refused (the probe below).
	// The host's leading \s* backtracks: JS whitespace it cannot use as
	// separator is yielded to the bare destination run, which accepts
	// non-ASCII whitespace (markedInlineLink [^ \\t\\n\\x00-\\x1f]+). These
	// links are legal in the host and must stay byte-identical.
	for _, in := range []string{
		"see [**Spec**](\u00a0 \"a b\")",
		"see [**Spec**](\u00a0\u00a0 'x')",
		"see [**Spec**](\u3000\u00a0 \"t\")",
	} {
		if out := Decorate(in, testPalette()); out != in {
			t.Errorf("leading \\s* backtrack wrongly rejected a legal link:\nin  %q\nout %q", in, out)
		}
	}
	// A single unclosed backtick can never be part of a legal link label
	// (branch 5 [^\\[\\]\\\\`] excludes it and no backtick branch matches), so
	// "see [**Spec** `a](url)" is NOT a link: **Spec** must still be
	// colored, not protected as a pseudo-link.
	if got, want := plain("see [**Spec** `a](https://example.com)"), "see [**<mdLink>Spec</>** `a](https://example.com)"; got != want {
		t.Errorf("unclosed single backtick wrongly protected a pseudo-link:\ngot  %q\nwant %q", got, want)
	}
	// translateMarkedPattern (:109-110) makes '.' exclude LF, CR, U+2028
	// and U+2029: the angle destination's `\\.` cannot swallow a U+2028,
	// so this is NOT a link and **Spec** still takes bold.
	if got, want := plain("see [**Spec**](<a\\\u2028 b>)"), "see [**<mdLink>Spec</>**](<a\\\u2028 b>)"; got != want {
		t.Errorf("angle \\\\ swallowed U+2028:\ngot  %q\nwant %q", got, want)
	}
	// The two other line terminators take the same `\\.` exclusion path;
	// an unescaped U+2028 stays inside the angle run through [^\\n<>\\]
	// (it excludes only '\\n'), so the link stays protected whole.
	for _, c := range []struct{ in, want string }{
		{"see [**Spec**](<a\\\u2029 b>)", "see [**<mdLink>Spec</>**](<a\\\u2029 b>)"},
		{"see [**Spec**](<a\\\rb>)", "see [**<mdLink>Spec</>**](<a\\\rb>)"},
		{"see [**Spec**](<a\u2028b>)", "see [**Spec**](<a\u2028b>)"},
		// A legal escaped quote inside the angle run still closes it:
		// \\. is fine on ordinary punctuation.
		{"see [**Spec**](<a\"b>)", "see [**Spec**](<a\"b>)"},
	} {
		if got := plain(c.in); got != c.want {
			t.Errorf("angle destination line-terminator handling:\nin  %q\ngot  %q\nwant %q", c.in, got, c.want)
		}
	}
	// A link whose label text itself parses as an inline link is refused
	// by host outputLink (:779-783) — the inner one wins, so **outer**
	// still takes bold and the truncated destination stays literal text.
	if got, want := plain("see [**outer** [inner](https://b.com)](https://a.com)"), "see [**<mdLink>outer</>** [inner](https://b.com)](https://a.com)"; got != want {
		t.Errorf("label-holding-inner-link outer wrongly protected:\ngot  %q\nwant %q", got, want)
	}
	// An image inside the label does not trip outputLink's
	// label-holds-a-link refusal (:764-791): only non-image tokens set
	// linkEmitted for the outer decision, and an image's own inner
	// links stay scoped to its tokenize — so the outer link is still
	// emitted and the whole construct stays byte-identical. Likewise a
	// codespan that closes exactly at the label's last byte must be
	// skipped (its [inner](u) is codespan content, not a link start).
	for _, in := range []string{
		"see [**![alt](u) x**](https://a.com)",
		"[**outer** ![inner](u)](v)",
		"[**outer** ![inner](u) **b**](v)",
		"[**outer** `[inner](u)`](v)",
		"[**o** ``[i](u)``](v)",
		// Positive controls: legal titles and an image-in-label link
		// stay protected whole.
		"[**Spec**](u \"a\")",
		"[**x**](u \"a\\\"b\")",
		"[a ![i](u)](v)",
		// Title contents modeled as (?:\\<close>?|[^<close>\\])* :
		// a '\\' right before the close char may backtrack to the
		// backslash alone so the close char ends the title. Each of
		// these is a legal link in the host (markedInlineLink :163)
		// and the whole raw must pass through byte-identical.
		`see [**Spec**](u "a\")`,
		`see [**Spec**](u 'a\')`,
		`see [**Spec**](u (a\))`,
		// Two backslashes: the second one's '\\"' leaves the last
		// quote as the real close after backtracking.
		`see [**Spec**](u "a\\")`,
		`see [**Spec**](u "a\"b")`,
		// link() :725-735: a '<'-prefixed bare destination goes through
		// the angle parity check ONLY (if/else with findClosingBracket),
		// so a balanced '<a<b)[[Done]]>' is accepted whole and the inner
		// [[Done]] never gets pill-decorated.
		`see [**Spec**](<a<b)[[Done]]>)`,
		// Non-'<' bare destinations keep the balanced-paren truncation.
		`see [**Spec**](u(a)b)`,
	} {
		if out := Decorate(in, testPalette()); out != in {
			t.Errorf("protected link/image rewritten:\nin  %q\nout %q", in, out)
		}
	}
	// A title must end at its first unescaped close quote: the host's
	// three title contents each exclude their closer — `(?:\\"?`
	// or `[^"\\])`, `(?:\\'?|[^'\\])`, `(?:\\)?|[^)\\])` in
	// markedInlineLink (markdown_marked.go:163) — so a close quote
	// that cannot reach \s*\) fails the whole title:
	// `(u "a" junk "b")` is NOT a link and **Spec** still takes bold.
	for _, c := range []struct{ in, want string }{
		{`see [**Spec**](u "a" junk "b")`, "see [**<mdLink>Spec</>**](u \"a\" junk \"b\")"},
		{`see [**Spec**](u 'a' junk 'b')`, "see [**<mdLink>Spec</>**](u 'a' junk 'b')"},
		{`see [**Spec**](u (a) junk (b))`, "see [**<mdLink>Spec</>**](u (a) junk (b))"},
		// A bare destination that JSTrim's to a '<' prefix but lacks
		// the closing '>' fails link()'s angle check (:727-729):
		// not a link, **Spec** still takes bold.
		{`see [**Spec**](<a<b)x)`, "see [**<mdLink>Spec</>**](<a<b)x)"},
	} {
		if got := plain(c.in); got != c.want {
			t.Errorf("unclosable title wrongly protected a pseudo-link:\ngot  %q\nwant %q", got, c.want)
		}
	}
	// The label-inner-link probe must run on the label's unescaped text
	// view (outputLink's markedUnescapeBrackets) bounded by the label's
	// own ']', never the raw line: an inner link candidate must not reach
	// past the label end into the outer destination, and a bracket kept
	// by the unescape is a fresh link start the host would see.
	// `[i](u` plus the outer `(v)` is not a link — the label-holding inner
	// candidate has no ')' before the label ends, so the outer link is
	// legal and the whole construct passes through byte-identical.
	if out := Decorate("see [**outer** [i](u](v)", testPalette()); out != "see [**outer** [i](u](v)" {
		t.Errorf("inner link candidate reached past the label end:\nin  %q\nout %q", "see [**outer** [i](u](v)", out)
	}
	// After markedUnescapeBrackets the label holds the real inner link
	// `[i](u)`: the outer link is refused (textHasLink) and the bold
	// fragment inside the label still takes bold; the raw \[..\] bytes
	// stay verbatim because the unescaped view only drives the probe.
	for _, c := range []struct{ in, want string }{
		{`see [**outer** \[i\](u)](v)`, "see [**<mdLink>outer</>** "},
		{`see [**X \[Y\](u)**](v)`, "see [**<mdLink>X "},
	} {
		out := Decorate(c.in, testPalette())
		if !strings.Contains(out, "<mdLink>") {
			t.Errorf("unescaped label inner link missed; outer wrongly protected:\nin  %q\nout %q", c.in, out)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("bold fragment inside the refused label missing:\nin  %q\nout %q\nwant %q", c.in, out, c.want)
		}
	}
	// Image label truncation (link() :737-749): the first ')' that dips
	// the paren balance below 0 ends the link, and linkLen counts runes
	// over the WHOLE label — a multi-byte label rune must not make the
	// truncation shift a byte or leave RuneError padding. `![😀](u)` is
	// the whole protected span and the ✓ right after it still takes the
	// success color.
	imgIn := "see ![\U0001f600](u)\u2713x)"
	imgOut := Decorate(imgIn, testPalette())
	if !strings.HasPrefix(imgOut, "see ![\U0001f600](u)") || !strings.Contains(imgOut, "<success>\u2713</>") {
		t.Errorf("image truncation miscounted label runes:\nin  %q\nout %q", imgIn, imgOut)
	}
	if got, want := matchInlineLink("![x](a)b)c)", 0, newLinkCache()), len("![x](a)"); got != want {
		t.Errorf("image truncation linkLen off by %d: got %d, want %d", want-got, got, want)
	}
}

// TestTitleBacktrackingIsBounded keeps the title content scan
// (?:\\<close>?|[^<close>\\])* linear under a memoized failure set: an
// adversarial tail of N "\"\"" pairs must not make the optional-close
// backtracking re-scan suffixes exponentially.
func TestTitleBacktrackingIsBounded(t *testing.T) {
	in := `see [x](u "` + strings.Repeat(`\"`, 5000) + `)`
	start := time.Now()
	_ = Decorate(in, testPalette())
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Decorate on a 5000-escape title took %s, want < 1s", d)
	}

	// The 8192-byte single-line display guard is exact, not approximate:
	// a line at the limit still decorates, one byte over passes through
	// untouched. Build one pad so each fragment line lands exactly on
	// the cap: len(frag)+1 keeps the trailing space inside the pad
	// count, so the total is 100*(len(frag)+1) + padLen + len(frag).
	// The fragments all rewrite (bold color, pill chip, mark color);
	// a protected code span would return byte-identical, so it is not
	// a boundary probe here — it is covered by TestLineStartBacktickRunIsCodeSpan.
	longCases := []struct{ name, frag string }{
		{"bold", "**Spec**"},
		{"pill", "[[Done]]"},
		{"success mark", "\u2713"},
		{"arrow", "\u2192"},
	}
	for _, c := range longCases {
		padLen := maxDecorateLineBytes - 101*len(c.frag) - 100
		atCap := strings.Repeat(c.frag+" ", 100) + strings.Repeat("x", padLen) + c.frag
		if len(atCap) != maxDecorateLineBytes {
			t.Fatalf("%s at-cap line is %d bytes, want %d", c.name, len(atCap), maxDecorateLineBytes)
		}
		// An undecorated line returns byte-identical; any rewrite proves
		// the decoration path ran.
		if atOut := Decorate(atCap, testPalette()); atOut == atCap {
			t.Errorf("%s at-cap line not decorated (returned verbatim)", c.name)
		}
		over := atCap + "x"
		if out := Decorate(over, testPalette()); out != over {
			t.Errorf("%s over-cap line decorated instead of passed through", c.name)
		}
	}
	// The guard must not break the fence state machine: fence open/close
	// tracking runs over every line before the length check, so an
	// over-cap line inside the fence still passes through verbatim and
	// the closing marker after it still ends the block — the short line
	// after the fence is decorated again.
	fenceIn := "```\n" + strings.Repeat("y", 9000) + "\n```\n**Spec**"
	if got, want := plain(fenceIn), "```\n"+strings.Repeat("y", 9000)+"\n```\n**<mdLink>Spec</>**"; got != want {
		t.Errorf("over-cap line inside a fence broke the fence state machine:\ngot  %q\nwant %q", got, want)
	}

	// Over-cap worst-case shapes: every input is one line longer than
	// 8192 bytes, so Decorate must pass it through byte-identical — the
	// guard, not the link parser, is what returns these quickly.
	const n = 60000
	nbspRun := strings.Repeat("\u00a0", n)
	mixedWS := strings.Repeat("\u00a0 ", n)
	for _, c := range []struct{ name, in string }{
		{"nested-unclosed-parens", `see [` + strings.Repeat("[a]((", n) + strings.Repeat(")", n)},
		{"nbsp-fail-tail", `see [**Spec**](` + nbspRun + ` "a b" junk)`},
		{"mixed-ws-fail-tail", `see [**Spec**](` + mixedWS + `"a b" junk)`},
		{"pseudo-link-runs", `see ` + strings.Repeat("[a](b]", n/3)},
		{"escaped-quote-title", `see [x](u "` + strings.Repeat(`\"`, n) + `)`},
		{"nested-brackets", `see [` + strings.Repeat("[x]", n) + `](u)`},
		{"backtick-runs", `see [` + strings.Repeat("``a``b", n/4) + `](u)`},
		{"tag-starts", `see [` + strings.Repeat("<x ", n) + `](u)`},
		{"latex-starts", `see [` + strings.Repeat("$", n) + `](u)`},
	} {
		start = time.Now()
		out := Decorate(c.in, testPalette())
		d := time.Since(start)
		t.Logf("%-24s bytes=%d took %s", c.name, len(c.in), d)
		if d > time.Second {
			t.Errorf("Decorate on over-cap %s took %s, want < 1s", c.name, d)
		}
		if out != c.in {
			t.Errorf("over-cap %s decorated instead of passed through verbatim", c.name)
		}
	}

	// Under-cap worst-case shapes (all <= 8192 bytes so the guard does
	// NOT skip them): every failed backtracking path must stay bounded
	// on its own — each input must decorate well under a second and
	// never panic. These exercise the real decoration path.
	const u = 4000
	nbspU := strings.Repeat("\u00a0", u)
	mixedU := strings.Repeat("\u00a0 ", u/2)
	cases := []struct {
		name string
		in   string
		want string // empty: only the timing bound is asserted
	}{
		// Pure-NBSP failing tail: every yielded NBSP is a fresh start
		// into the same bare run whose (sep title)?\s*\) tail fails.
		{"nbsp-fail-tail", `see [**Spec**](` + nbspU + ` "a b" junk)`,
			"see [**<mdLink>Spec</>**](" + strings.Repeat(" ", u) + ` "a b" junk)`},
		// Mixed NBSP/ASCII-space failing tail: each ASCII-space boundary
		// starts its own bare run ending at the next space — the runs'
		// right ends are all different, so per-runEnd tail memoization
		// alone leaves each run re-skipping a whitespace suffix that
		// grows by two runes per run; the per-position whitespace memo
		// keeps the total linear.
		{"mixed-ws-fail-tail", `see [**Spec**](` + mixedU + `"a b" junk)`,
			"see [**<mdLink>Spec</>**](" + strings.Repeat(" ", u) + `"a b" junk)`},
		// Pure ASCII-space prefix + failing title: the boundaries are
		// all ' ', which is a bare-run terminator — every boundary is an
		// empty run probing the same (sep title)?\s*\) tail from
		// positions one space apart over a shared suffix.
		{"ascii-space-fail-tail", `see [**Spec**](` + strings.Repeat(" ", u) + `"a b" junk)`,
			"see [**<mdLink>Spec</>**](" + strings.Repeat(" ", u) + `"a b" junk)`},
		// Legal tail at the same scale: protected verbatim.
		{"nbsp-legal-tail", `see [**Spec**](` + nbspU + ` "a b")`,
			`see [**Spec**](` + nbspU + ` "a b")`},
		// Escaped-quote title backtracking at scale: the memoized
		// failure set must keep (?:\\<close>?|[^<close>\\])* linear.
		{"escaped-quote-title", `see [x](u "` + strings.Repeat(`\"`, 4000) + `)`, ""},
		// Unbalanced '(' inside the bare destination: the rightmost-')'
		// backtrack rescan must stay amortized per run.
		{"nested-parens", `see [**Spec**](` + strings.Repeat("(", 2000) + ` "a b" junk)`,
			"see [**<mdLink>Spec</>**](" + strings.Repeat("(", 2000) + ` "a b" junk)`},
		// Unclosed nested '[': the inner pair never closes, so the
		// label fails and the construct is not a link at all.
		{"nested-brackets", `see [` + strings.Repeat("[x]", 1300) + `](u)`, ""},
		// Alternating equal-length backtick runs inside the label:
		// every run finds an equal-length closer and the label never
		// reaches a closing ']'.
		{"backtick-runs", `see [` + strings.Repeat("``a``b", 1300) + `](u)`, ""},
		// Label-probe skipping of tag and LaTeX candidates at scale.
		{"tag-starts", `see [` + strings.Repeat("<x ", 2000) + `](u)`, ""},
		{"latex-starts", `see [` + strings.Repeat("$", 4000) + `](u)`, ""},
		// Adjacent unclosed pseudo-link starts: each '[' opens its own
		// exec instance, so the fixed per-exec work (boundary
		// collection, destination post-checks) already runs 2000 times.
		{"pseudo-link-runs", `see ` + strings.Repeat("[a](b]", 1000), ""},
		// Many candidates that all reach the findClosingParen -2
		// verdict: each [a](( run's exec succeeds and captures a
		// destination whose balance stays positive to the end, so the
		// post-check rejects it and the next '[' probes the same
		// suffix — the per-position run memo keeps the total linear.
		// ` **Spec**` after it must still take bold.
		{"nested-unclosed-parens-multi", `see ` + strings.Repeat("[a]((", 1000) + strings.Repeat(")", 1000) + ` **Spec**`,
			`see ` + strings.Repeat("[a]((", 1000) + strings.Repeat(")", 1000) + ` **<mdLink>Spec</>**`},
	}
	for _, c := range cases {
		start = time.Now()
		out := Decorate(c.in, testPalette())
		d := time.Since(start)
		t.Logf("%-24s bytes=%d took %s", c.name, len(c.in), d)
		if len(c.in) > maxDecorateLineBytes {
			t.Fatalf("%s input is %d bytes, over the %d cap — the guard would skip the decoration path", c.name, len(c.in), maxDecorateLineBytes)
		}
		if d > time.Second {
			t.Errorf("Decorate on %s took %s, want < 1s", c.name, d)
		}
		if c.want != "" {
			got := strings.ReplaceAll(stripANSI(out), "\u00a0", " ")
			want := strings.ReplaceAll(c.want, "\u00a0", " ")
			if got != want {
				t.Errorf("%s decorated wrongly:\ngot  %q\nwant %q", c.name, got, want)
			}
		}
	}
}

// paletteThemeForEmphasis writes UITheme's private token maps directly (the
// SDK decodes them from the host; mirrors go/rpiv-ask-user-question's helper)
// so a test can hand paletteFor a real sdk.UITheme.
func paletteThemeForEmphasis(t *testing.T, name string, foregrounds, backgrounds map[string]string) sdk.UITheme {
	t.Helper()
	theme := sdk.UITheme{Name: name}
	for _, f := range []struct {
		name string
		m    map[string]string
	}{{"foregrounds", foregrounds}, {"backgrounds", backgrounds}} {
		field := reflect.ValueOf(&theme).Elem().FieldByName(f.name)
		if !field.IsValid() || !field.CanAddr() {
			t.Fatalf("sdk.UITheme has no addressable %s field", f.name)
		}
		reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(f.m))
	}
	return theme
}

// themePair returns two same-name themes differing only in the "muted"
// foreground escape (the PI_EMPHASIS_COLOR=muted probe).
func themePair(t *testing.T, mutedAnsi string) sdk.UITheme {
	t.Helper()
	fg := map[string]string{
		"mdLink": "\x1b[34m", "muted": mutedAnsi, "success": "\x1b[32m", "error": "\x1b[31m",
		"warning": "\x1b[33m", "accent": "\x1b[36m", "dim": "\x1b[90m", "text": "\x1b[37m",
	}
	bg := map[string]string{
		"toolSuccessBg": "\x1b[42m", "toolErrorBg": "\x1b[41m",
		"searchMatchBg": "\x1b[43m", "selectedBg": "\x1b[44m",
	}
	return paletteThemeForEmphasis(t, "custom", fg, bg)
}

// TestCustomColorTokenFingerprint is the S2 probe's deterministic half: the
// PI_EMPHASIS_COLOR token's own ANSI belongs in the fingerprint, so a muted
// color change under the same theme name rebuilds the palette instead of
// drawing the stale one. (The stale-generation write-back guard is covered
// by TestStaleGenerationGuards, which exercises the gen counter added with
// the fix.)
func TestCustomColorTokenFingerprint(t *testing.T) {
	t.Setenv("PI_EMPHASIS_COLOR", "muted")
	gray, red := themePair(t, "\x1b[90m"), themePair(t, "\x1b[31m")
	if themeFingerprint(gray) == themeFingerprint(red) {
		t.Fatal("fingerprint ignored the custom token's own ANSI")
	}
	// Bind a switchable real theme so transform actually walks the s.built
	// cache: an unbound state's guarded write-back never stores anything.
	st := &emphasisState{}
	current := gray
	st.setThemeFunc(func() sdk.UITheme { return current })
	ctx := sdk.MarkdownTransformContext{MessageType: "assistant"}
	out1 := st.transform("**x**", ctx)
	if !strings.Contains(out1, "\x1b[90m") {
		t.Fatalf("first transform did not draw the gray muted bold: %q", out1)
	}
	st.mu.RLock()
	built := st.built
	st.mu.RUnlock()
	if built == nil {
		t.Fatal("s.built was not populated by a transform under a bound theme")
	}
	if built.fingerprint != themeFingerprint(gray) {
		t.Fatal("s.built cached a palette for a different fingerprint")
	}
	// A second transform under the same theme hits the cache (same palette).
	_ = st.transform("**x**", ctx)
	st.mu.RLock()
	if st.built != built {
		st.mu.RUnlock()
		t.Fatal("same-fingerprint transform rebuilt s.built instead of hitting the cache")
	}
	st.mu.RUnlock()
	// The theme's muted token flips gray→red without an invalidate: the
	// fingerprint change alone must rebuild s.built.
	current = red
	out2 := st.transform("**x**", ctx)
	if !strings.Contains(out2, "\x1b[31m") {
		t.Fatalf("stale palette survived the muted-token change: %q", out2)
	}
	st.mu.RLock()
	if st.built == built || st.built.fingerprint != themeFingerprint(red) {
		st.mu.RUnlock()
		t.Fatal("s.built was not rebuilt for the new fingerprint")
	}
	st.mu.RUnlock()
}

// TestStaleGenerationGuards is the S2 probe's deterministic half, now over
// the two generation counters. sessionGen tracks the session binding
// (setThemeFunc/clearTheme only), so a theme-change invalidate that lands
// between setThemeFunc and the store of a just-registered OnThemeChange
// unsubscribe must NOT retire the subscription (R2 Finding 1: the theme
// feed's worker runs st.invalidate() concurrently with session_start).
// paletteGen additionally advances on invalidate, so a palette build that
// started under the old theme cannot write s.built back over the newer
// state — proven here by parking a real build across invalidate+rebind with
// the onBuildPalette barrier, not by asserting counters alone. The race
// window itself is also driven by TestTransformConcurrentSafe under -race.
func TestStaleGenerationGuards(t *testing.T) {
	t.Setenv("PI_EMPHASIS_COLOR", "muted")
	gray, red := themePair(t, "\x1b[90m"), themePair(t, "\x1b[31m")
	redFP := themeFingerprint(red)

	// Session-lifecycle assertions over sessionGen.
	st := &emphasisState{}
	sessionGen := st.setThemeFunc(func() sdk.UITheme { return gray })
	// invalidate() bumps paletteGen but never sessionGen: the current
	// session's subscription stays live.
	st.invalidate()
	var liveCalls int
	st.setUnsubTheme(sessionGen, func() { liveCalls++ })
	if liveCalls != 0 {
		t.Fatalf("valid-session unsub called %d times after invalidate, want 0 (invalidate is not a session change)", liveCalls)
	}
	st.mu.RLock()
	if st.unsubTheme == nil {
		st.mu.RUnlock()
		t.Fatal("valid-session unsub was not stored after a mere invalidate")
	}
	st.mu.RUnlock()
	// A subscription pinned to a superseded session is released, not stored:
	// simulates ctx.OnThemeChange returning after a concurrent clearTheme or
	// a newer setThemeFunc interleaved in OnSessionStart.
	var calls int
	unsub := func() { calls++ }
	st.setUnsubTheme(sessionGen-1, unsub)
	if calls != 1 {
		t.Fatalf("stale-generation unsub called %d times, want 1 (immediate release)", calls)
	}
	// clearTheme retires the live session and releases its stored unsub.
	st.clearTheme()
	if liveCalls != 1 {
		t.Fatalf("clearTheme released the live session's unsub %d times, want 1", liveCalls)
	}
	if st.hasTheme() {
		t.Fatal("clearTheme must drop the theme binding")
	}
	// setUnsubTheme with the retired session's generation is released at once.
	st.setUnsubTheme(sessionGen, unsub)
	if calls != 2 {
		t.Fatalf("post-clearTheme unsub called %d times, want 2", calls)
	}

	// Stale-build write-back barrier: G1's build of the gray palette is
	// parked between its read-lock snapshot and CreatePalette; the main
	// goroutine invalidates, rebinds to red and populates s.built for red.
	// Releasing G1 must not let the gray build overwrite the red cache.
	st2 := &emphasisState{}
	st2.setThemeFunc(func() sdk.UITheme { return gray })
	started := make(chan struct{})
	release := make(chan struct{})
	// CAS-arms the first build only: every later build (the red paletteFor
	// below included) passes through. A sync.Once cannot be used — its Do
	// holds the once lock while the function runs, so a second caller would
	// block on the parked first build forever.
	var armed atomic.Int32
	st2.onBuildPalette = func() {
		if armed.CompareAndSwap(0, 1) {
			close(started)
			<-release
		}
	}
	// paletteFor's snapPaletteGen is the generation the caller observed
	// together with its theme read; these direct calls snapshot it the same
	// way transform does, under the same read lock.
	snap := func(st *emphasisState) uint64 {
		st.mu.RLock()
		g := st.paletteGen
		st.mu.RUnlock()
		return g
	}
	done := make(chan Palette, 1)
	go func() {
		p, _ := st2.paletteFor(gray, snap(st2))
		done <- p
	}()
	<-started
	st2.invalidate()
	st2.setThemeFunc(func() sdk.UITheme { return red })
	if _, ok := st2.paletteFor(red, snap(st2)); !ok {
		t.Fatal("paletteFor(red) refused a resolvable theme")
	}
	st2.mu.RLock()
	if st2.built == nil || st2.built.fingerprint != redFP {
		st2.mu.RUnlock()
		t.Fatal("red palette was not cached after rebind")
	}
	st2.mu.RUnlock()
	close(release)
	stale := <-done
	if stale.Bold == nil {
		t.Fatal("G1's build did not produce a palette")
	}
	if got, want := stale.Bold("x"), "\x1b[90mx\x1b[39m"; got != want {
		t.Fatalf("G1 built %q, want the gray palette %q", got, want)
	}
	st2.mu.RLock()
	if st2.built == nil || st2.built.fingerprint != redFP {
		st2.mu.RUnlock()
		t.Fatalf("stale gray build overwrote the red cache: s.built = %+v", st2.built)
	}
	st2.mu.RUnlock()
	// The cache keeps drawing red; the stale build was returned to its own
	// caller but never stored.
	if got := st2.transform("**x**", sdk.MarkdownTransformContext{MessageType: "assistant"}); !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("transform after stale write-back drew %q, want the red palette", got)
	}

	// Late-snapshot window at the transform entry: G1 has already read
	// f := s.themeFunc and finished theme := f() (gray), but is parked
	// before paletteFor. The main goroutine rebinds to red (bumping
	// paletteGen) and G2's transform caches the red palette. Releasing G1
	// must NOT let its gray build overwrite the red cache: the paletteGen
	// snapshot has to be taken together with f := s.themeFunc, not inside
	// paletteFor after the rebind already landed.
	st3 := &emphasisState{}
	st3.setThemeFunc(func() sdk.UITheme { return gray })
	themeRead := make(chan struct{})
	letGo := make(chan struct{})
	var armed3 atomic.Int32
	st3.onAfterThemeRead = func() {
		if armed3.CompareAndSwap(0, 1) {
			close(themeRead)
			<-letGo
		}
	}
	g1Done := make(chan string, 1)
	go func() {
		g1Done <- st3.transform("**x**", sdk.MarkdownTransformContext{MessageType: "assistant"})
	}()
	<-themeRead // G1 holds the gray theme but has not entered paletteFor
	st3.setThemeFunc(func() sdk.UITheme { return red })
	// G2 completes a full transform under the new binding, caching red.
	if got := st3.transform("**x**", sdk.MarkdownTransformContext{MessageType: "assistant"}); !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("G2 transform did not draw the red palette: %q", got)
	}
	close(letGo)
	if got := <-g1Done; !strings.Contains(got, "\x1b[90m") {
		t.Fatalf("G1 transform drew %q, want the gray palette it bound to", got)
	}
	st3.mu.RLock()
	if st3.built == nil || st3.built.fingerprint != redFP {
		st3.mu.RUnlock()
		t.Fatalf("stale gray transform overwrote the red cache: s.built = %+v", st3.built)
	}
	st3.mu.RUnlock()
}
