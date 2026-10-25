package rpivaskuserquestion

import (
	"strings"
	"testing"
)

// ── preview box renderer ──────────────────────────────────────────────────

func TestStripFenceMarkers(t *testing.T) {
	lines := []string{
		"```go",
		"func main() {}",
		"```",
		"\x1b[1m```\x1b[0m",
		"plain text",
		"inline `code` stays",
	}
	got := StripFenceMarkers(lines)
	if len(got) != 3 {
		t.Fatalf("stripped = %#v", got)
	}
	if got[0] != "func main() {}" || got[1] != "plain text" || got[2] != "inline `code` stays" {
		t.Fatalf("stripped = %#v", got)
	}
}

func TestStripFenceMarkersIgnoresOsc8Links(t *testing.T) {
	// An OSC-8 hyperlink wrapped around a fence marker is still a fence marker.
	line := "\x1b]8;;https://example.com\x07```\x1b]8;;\x07"
	if got := StripFenceMarkers([]string{line}); len(got) != 0 {
		t.Fatalf("stripped = %#v", got)
	}
}

func TestRenderBorderedBox(t *testing.T) {
	lines := RenderBorderedBox([]string{"hello", "world"}, 20, nil, 0)
	if len(lines) != 4 {
		t.Fatalf("rows = %d: %#v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[0], "╮") {
		t.Fatalf("top border = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "│ ") || !strings.HasSuffix(lines[1], " │") {
		t.Fatalf("content row = %q", lines[1])
	}
	if !strings.Contains(lines[1], "hello") {
		t.Fatalf("content lost: %q", lines[1])
	}
	if !strings.HasPrefix(lines[3], "╰") || !strings.HasSuffix(lines[3], "╯") {
		t.Fatalf("bottom border = %q", lines[3])
	}
	// Every row has the same visible width.
	want := visibleWidth(lines[0])
	for i, line := range lines {
		if got := visibleWidth(line); got != want {
			t.Fatalf("row %d width = %d, want %d (%q)", i, got, want, line)
		}
	}
}

func TestRenderBorderedBoxHiddenIndicator(t *testing.T) {
	lines := RenderBorderedBox([]string{"only"}, 30, nil, 7)
	last := lines[len(lines)-1]
	if !strings.Contains(last, "7 lines hidden") {
		t.Fatalf("bottom border = %q", last)
	}
	if !strings.Contains(last, "✂") {
		t.Fatalf("no scissors glyph: %q", last)
	}
	if !strings.HasPrefix(last, "╰") || !strings.HasSuffix(last, "╯") {
		t.Fatalf("corners lost: %q", last)
	}
}

func TestRenderBorderedBoxAppliesColor(t *testing.T) {
	colored := RenderBorderedBox([]string{"x"}, 10, func(s string) string { return "<" + s + ">" }, 0)
	for _, line := range colored {
		if !strings.HasPrefix(line, "<") || !strings.HasSuffix(line, ">") {
			t.Fatalf("line not colored: %q", line)
		}
	}
}

func TestComputeBoxDimensions(t *testing.T) {
	t.Run("floored at the minimum", func(t *testing.T) {
		got := ComputeBoxDimensions([]string{"ab"}, 80)
		if got.InnerWidth != BoxMinContentWidth {
			t.Fatalf("innerWidth = %d", got.InnerWidth)
		}
		if got.BoxWidth != BoxMinContentWidth+BorderHorizontalOverhead+2*BorderInnerPaddingHorizontal {
			t.Fatalf("boxWidth = %d", got.BoxWidth)
		}
	})

	t.Run("grows to the widest line", func(t *testing.T) {
		wide := strings.Repeat("x", 60)
		if got := ComputeBoxDimensions([]string{wide}, 80); got.InnerWidth != 60 {
			t.Fatalf("innerWidth = %d", got.InnerWidth)
		}
	})

	t.Run("clamped to the available width", func(t *testing.T) {
		wide := strings.Repeat("x", 200)
		if got := ComputeBoxDimensions([]string{wide}, 50); got.InnerWidth != 50 {
			t.Fatalf("innerWidth = %d", got.InnerWidth)
		}
	})

	t.Run("trailing whitespace does not widen the box", func(t *testing.T) {
		// The markdown renderer pads every line to the width; measuring must strip.
		if got := ComputeBoxDimensions([]string{"short" + strings.Repeat(" ", 100)}, 120); got.InnerWidth != BoxMinContentWidth {
			t.Fatalf("innerWidth = %d", got.InnerWidth)
		}
	})
}

// ── preview layout decider ────────────────────────────────────────────────

func TestDecideLayout(t *testing.T) {
	cases := []struct {
		terminal, pane int
		want           PreviewLayoutMode
	}{
		{120, 120, LayoutSideBySide},
		{100, 100, LayoutSideBySide},
		{99, 120, LayoutStacked},
		{120, 99, LayoutStacked},
		{80, 80, LayoutStacked},
		{0, 0, LayoutStacked},
	}
	for _, tc := range cases {
		if got := DecideLayout(tc.terminal, tc.pane); got != tc.want {
			t.Fatalf("DecideLayout(%d,%d) = %q, want %q", tc.terminal, tc.pane, got, tc.want)
		}
	}
}

func TestAdaptiveLeftWidth(t *testing.T) {
	items := []WrappingSelectItem{
		{Kind: RowKindOption, Label: "short"},
		{Kind: RowKindOption, Label: strings.Repeat("x", 40)},
	}
	got := AdaptiveLeftWidth(items, len(items), 200)
	if got < MinLeft {
		t.Fatalf("width %d below the floor", got)
	}
	if got > 200/2 {
		t.Fatalf("width %d above the ratio ceiling", got)
	}

	// No items still yields a usable column.
	if got := AdaptiveLeftWidth(nil, 0, 200); got != MinLeft {
		t.Fatalf("empty width = %d", got)
	}

	// A narrow pane cannot exceed the available width.
	if got := AdaptiveLeftWidth(items, len(items), 50); got < MinLeft {
		t.Fatalf("narrow width = %d", got)
	}
}

func TestCrossTabMaxLeftWidth(t *testing.T) {
	params := multiParams()
	itemsByTab := BuildItemsByTab(params)
	got := CrossTabMaxLeftWidth(params.Questions, itemsByTab, 200)
	// The widest tab wins.
	if got < MinLeft {
		t.Fatalf("width = %d", got)
	}
	if got != AdaptiveLeftWidth(itemsByTab[0], len(itemsByTab[0]), 200) &&
		got != AdaptiveLeftWidth(itemsByTab[1], len(itemsByTab[1]), 200) {
		t.Fatalf("width = %d is not one of the per-tab widths", got)
	}
}

func TestPreviewSourceWidth(t *testing.T) {
	preview := "short\n" + strings.Repeat("y", 30)
	q := twoOptions("H", "Q?")
	q.Options[0].Preview = &preview
	if got := PreviewSourceWidth(q); got != 30 {
		t.Fatalf("width = %d", got)
	}
	if got := PreviewSourceWidth(twoOptions("H", "Q?")); got != 0 {
		t.Fatalf("no-preview width = %d", got)
	}
}

func TestCrossTabPreviewBudget(t *testing.T) {
	preview := strings.Repeat("z", 80)
	q := twoOptions("H", "Q?")
	q.Options[0].Preview = &preview

	got := CrossTabPreviewBudget([]QuestionData{q}, 200)
	if got < MinPreviewWidth {
		t.Fatalf("budget = %d below the floor", got)
	}
	// Overhead is box border + inner padding + preview padding.
	want := 80 + BorderHorizontalOverhead + 2*BorderInnerPaddingHorizontal + PreviewPaddingLeft
	if got != want {
		t.Fatalf("budget = %d, want %d", got, want)
	}

	// No previews: the floor.
	if got := CrossTabPreviewBudget([]QuestionData{twoOptions("H", "Q?")}, 200); got < MinPreviewWidth {
		t.Fatalf("budget = %d", got)
	}
}

func TestCrossTabLeftWidthWithDonation(t *testing.T) {
	params := multiParams()
	itemsByTab := BuildItemsByTab(params)

	// At side-by-side widths the left column stays under the ratio ceiling.
	got := CrossTabLeftWidthWithDonation(params.Questions, itemsByTab, params.Questions, 200)
	if got < MinLeft {
		t.Fatalf("width = %d below the floor", got)
	}
	if got > 200/2 {
		t.Fatalf("width = %d above the ratio ceiling", got)
	}

	// It never starves the preview column at side-by-side widths.
	if got > 200-PreviewColumnGap-MinPreviewWidth {
		t.Fatalf("width = %d starves the preview column", got)
	}
}

func TestColumnWidthsFor(t *testing.T) {
	got := ColumnWidthsFor(120, 50)
	if got.LeftWidth != 50 || got.RightWidth != 120-50-PreviewColumnGap || got.Gap != PreviewColumnGap {
		t.Fatalf("widths = %#v", got)
	}
	// Extreme inputs keep both columns at least one column wide.
	extreme := ColumnWidthsFor(5, 100)
	if extreme.LeftWidth < 1 || extreme.RightWidth < 1 {
		t.Fatalf("extreme widths = %#v", extreme)
	}
}

func TestBodyWidthsFor(t *testing.T) {
	stacked := BodyWidthsFor(80, LayoutStacked, 30)
	if stacked.OptionsWidth != 80 || stacked.PreviewWidth != 80 {
		t.Fatalf("stacked = %#v", stacked)
	}
	side := BodyWidthsFor(120, LayoutSideBySide, 50)
	if side.OptionsWidth != 50 {
		t.Fatalf("side-by-side = %#v", side)
	}
	if side.PreviewWidth != 120-50-PreviewColumnGap-PreviewPaddingLeft {
		t.Fatalf("preview width = %d", side.PreviewWidth)
	}
}

// ── markdown renderer ─────────────────────────────────────────────────────

func TestMarkdownRendererPlain(t *testing.T) {
	renderer := markdownRenderer{styled: false}
	lines := renderer.render("# Heading\n\ntext", 40)
	if len(lines) != 3 {
		t.Fatalf("lines = %#v", lines)
	}
	if lines[0] != "Heading" {
		t.Fatalf("heading = %q", lines[0])
	}
	if lines[1] != "" {
		t.Fatalf("blank line = %q", lines[1])
	}
	if lines[2] != "text" {
		t.Fatalf("text = %q", lines[2])
	}
}

func TestMarkdownRendererBulletsAndRules(t *testing.T) {
	renderer := markdownRenderer{styled: false}
	lines := renderer.render("- one\n- two\n\n---\n1. first", 30)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "- one") || !strings.Contains(joined, "- two") {
		t.Fatalf("bullets = %#v", lines)
	}
	if !strings.Contains(joined, "─") {
		t.Fatalf("rule = %#v", lines)
	}
	if !strings.Contains(joined, "1. first") {
		t.Fatalf("ordered bullet = %#v", lines)
	}
}

// Fenced code keeps its markers (the caller strips them) and its body verbatim.
func TestMarkdownRendererFences(t *testing.T) {
	renderer := markdownRenderer{styled: false}
	lines := renderer.render("```go\nfunc main() {}\n```", 40)
	if len(lines) != 3 {
		t.Fatalf("lines = %#v", lines)
	}
	if lines[0] != "```go" || lines[2] != "```" {
		t.Fatalf("fences = %#v", lines)
	}
	// A long code line is not re-wrapped.
	renderer = markdownRenderer{styled: false}
	long := strings.Repeat("x", 100)
	lines = renderer.render("```\n"+long+"\n```", 10)
	if lines[1] != long {
		t.Fatalf("code line wrapped: %q", lines[1])
	}
}

func TestHeadingAndBulletAndRuleHelpers(t *testing.T) {
	if level, text, ok := heading("## Title"); !ok || level != 2 || text != "Title" {
		t.Fatalf("heading = (%d,%q,%v)", level, text, ok)
	}
	if _, _, ok := heading("#nospace"); ok {
		t.Fatal("heading without a space accepted")
	}
	if _, _, ok := heading("####### seven"); ok {
		t.Fatal("seven-hash heading accepted")
	}
	if _, _, ok := heading("#"); ok {
		t.Fatal("bare hash accepted")
	}
	if marker, text, ok := bullet("- item"); !ok || marker != "-" || text != "item" {
		t.Fatalf("bullet = (%q,%q,%v)", marker, text, ok)
	}
	if marker, text, ok := bullet("12. item"); !ok || marker != "12." || text != "item" {
		t.Fatalf("ordered bullet = (%q,%q,%v)", marker, text, ok)
	}
	if _, _, ok := bullet("plain"); ok {
		t.Fatal("plain text parsed as a bullet")
	}
	if !isRule("---") || !isRule("***") || !isRule("___") {
		t.Fatal("rule not recognised")
	}
	if isRule("--") || isRule("-x-") || isRule("abc") {
		t.Fatal("non-rule recognised")
	}
}

// ── markdown content cache ────────────────────────────────────────────────

func TestMarkdownContentCache(t *testing.T) {
	preview := "# Title\n\nbody"
	q := twoOptions("H", "Q?")
	q.Options[0].Preview = &preview

	cache := NewMarkdownContentCache(q, themeForTests())
	if !cache.HasAnyPreview() {
		t.Fatal("hasAnyPreview = false")
	}
	if !cache.Has(0) || cache.Has(1) {
		t.Fatal("Has() wrong")
	}

	body := cache.BodyFor(0, 30)
	if len(body) == 0 {
		t.Fatal("no body lines")
	}
	// Fence markers are stripped by the cache, so a fenced preview loses them.
	fenced := "```go\nx\n```"
	q2 := twoOptions("H", "Q?")
	q2.Options[1].Preview = &fenced
	cache2 := NewMarkdownContentCache(q2, themeForTests())
	body2 := cache2.BodyFor(1, 30)
	for _, line := range body2 {
		if strings.HasPrefix(line, "```") {
			t.Fatalf("fence marker survived: %#v", body2)
		}
	}
}

// A width change invalidates the cached render.
func TestMarkdownContentCacheWidthKeyed(t *testing.T) {
	preview := strings.Repeat("word ", 40)
	q := twoOptions("H", "Q?")
	q.Options[0].Preview = &preview
	cache := NewMarkdownContentCache(q, themeForTests())

	narrow := len(cache.BodyFor(0, 20))
	wide := len(cache.BodyFor(0, 200))
	if narrow <= wide {
		t.Fatalf("narrow rows = %d, wide rows = %d (expected the cache to re-render)", narrow, wide)
	}
	// Asking again at the same width returns the cached result.
	if again := len(cache.BodyFor(0, 200)); again != wide {
		t.Fatalf("cached render changed: %d != %d", again, wide)
	}
}

// An option without a preview renders the placeholder padded to the width.
func TestMarkdownContentCacheNoPreview(t *testing.T) {
	cache := NewMarkdownContentCache(twoOptions("H", "Q?"), themeForTests())
	body := cache.BodyFor(0, 40)
	if len(body) != 1 {
		t.Fatalf("body = %#v", body)
	}
	if !strings.Contains(body[0], NoPreviewText) {
		t.Fatalf("placeholder missing: %q", body[0])
	}
	if visible := visibleWidth(strings.TrimRight(body[0], " ")); visible != visibleWidth(NoPreviewText) {
		t.Fatalf("placeholder width = %d (%q)", visible, body[0])
	}
	if cache.HasAnyPreview() {
		t.Fatal("a question with no previews must report HasAnyPreview() == false")
	}
}
