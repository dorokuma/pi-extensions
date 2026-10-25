package rpivaskuserquestion

import (
	"strings"
	"testing"
)

// stripANSI removes SGR/OSC/APC sequences for readable assertions.
func stripANSI(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			out.WriteByte(text[i])
			continue
		}
		// Skip the whole escape sequence.
		i++
		if i >= len(text) {
			break
		}
		switch text[i] {
		case '[': // CSI: params then a final byte in @..~
			i++
			for i < len(text) && (text[i] < '@' || text[i] > '~') {
				i++
			}
		case ']': // OSC: until BEL or ST
			for i < len(text) && text[i] != 0x07 {
				if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
					i++
					break
				}
				i++
			}
		case '_': // APC: until ST
			for i < len(text) && !(text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\') {
				i++
			}
			i++
		}
	}
	return out.String()
}

func renderStrings(lines []string) string { return stripANSI(strings.Join(lines, "\n")) }

// ── WrappingSelect ────────────────────────────────────────────────────────

func TestWrappingSelectRenderNumbersAndCursor(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?")) // A, B, other
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	lines := selectView.Render(40)
	text := renderStrings(lines)

	if !strings.Contains(text, "❯ 1. A") {
		t.Fatalf("active row missing pointer/number:\n%s", text)
	}
	if !strings.Contains(text, "  2. B") {
		t.Fatalf("inactive row = %q\n%s", text, text)
	}
	if !strings.Contains(text, "  3. Type something.") {
		t.Fatalf("sentinel row missing:\n%s", text)
	}
	// Descriptions sit on continuation lines.
	if !strings.Contains(text, "  desc A") {
		t.Fatalf("description missing:\n%s", text)
	}
}

func TestWrappingSelectNumberPadding(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	total := 12
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{TotalItemsForNumbering: &total})
	text := renderStrings(selectView.Render(40))
	// Two-digit numbering pads to width 2: "❯  1. A".
	if !strings.Contains(text, "❯  1. A") {
		t.Fatalf("padding wrong:\n%s", text)
	}
}

func TestWrappingSelectNumberStartOffset(t *testing.T) {
	items := []WrappingSelectItem{{Kind: RowKindOption, Label: "x"}}
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{NumberStartOffset: 4})
	text := renderStrings(selectView.Render(20))
	if !strings.Contains(text, "5. x") {
		t.Fatalf("offset numbering wrong:\n%s", text)
	}
}

func TestWrappingSelectScrollInfo(t *testing.T) {
	labels := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		labels = append(labels, "opt"+itoa(i))
	}
	items := BuildItemsForQuestion(question("H", "Q?", labels...))
	selectView := NewWrappingSelect(items, 3, plainTheme, WrappingSelectOptions{})
	lines := selectView.Render(40)
	text := renderStrings(lines)
	if !strings.Contains(text, "(1/21)") {
		t.Fatalf("scroll info missing:\n%s", text)
	}
	// maxVisible caps the number of ITEMS in the window, not the rendered rows:
	// each option contributes a label row plus its description row.
	itemsShown := 0
	for _, line := range lines {
		if strings.Contains(stripANSI(line), ". ") {
			itemsShown++
		}
	}
	if itemsShown > 3 {
		t.Fatalf("rendered %d item rows for maxVisible 3", itemsShown)
	}
}

func TestWrappingSelectEmpty(t *testing.T) {
	selectView := NewWrappingSelect(nil, 5, plainTheme, WrappingSelectOptions{})
	if got := selectView.Render(20); len(got) != 0 {
		t.Fatalf("empty list rendered %#v", got)
	}
	if got := selectView.FocusedItemRowRange(20); got != [2]int{0, 0} {
		t.Fatalf("empty focus range = %#v", got)
	}
}

func TestWrappingSelectSelectionClamps(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})

	selectView.SetSelectedIndex(99)
	text := renderStrings(selectView.Render(40))
	// Row 3 is the Type-something. row: focused, it renders the inline editor with
	// an empty buffer, so assert on the pointer + number rather than its label.
	if !strings.Contains(text, "❯ 3.") {
		t.Fatalf("selection not clamped:\n%s", text)
	}
	if strings.Contains(text, "4.") {
		t.Fatalf("selection past the last row:\n%s", text)
	}
	selectView.SetSelectedIndex(-5)
	text = renderStrings(selectView.Render(40))
	if !strings.Contains(text, "❯ 1. A") {
		t.Fatalf("negative selection not clamped:\n%s", text)
	}
}

// Unfocused lists draw no pointer on any row.
func TestWrappingSelectUnfocused(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	selectView.SetFocused(false)
	if text := renderStrings(selectView.Render(40)); strings.Contains(text, "❯") {
		t.Fatalf("pointer drawn on an unfocused list:\n%s", text)
	}
}

// A confirmed row shows the check mark, and the custom row overrides its label.
func TestWrappingSelectConfirmedMark(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	selectView.SetFocused(false)
	index := 1
	selectView.SetConfirmedIndex(&index, nil)
	text := renderStrings(selectView.Render(40))
	if !strings.Contains(text, "2. B ✔") {
		t.Fatalf("confirmed mark missing:\n%s", text)
	}

	override := "my custom"
	otherIndex := 2
	selectView.SetConfirmedIndex(&otherIndex, &override)
	text = renderStrings(selectView.Render(40))
	if !strings.Contains(text, "my custom ✔") {
		t.Fatalf("label override missing:\n%s", text)
	}

	selectView.SetConfirmedIndex(nil, nil)
	if text := renderStrings(selectView.Render(40)); strings.Contains(text, "✔") {
		t.Fatalf("confirmed mark not cleared:\n%s", text)
	}
}

// An in-flight draft that differs from the confirmed custom answer suppresses the
// mark so the pending draft is not presented as committed.
func TestWrappingSelectDraftSuppressesConfirmedMark(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	selectView.SetFocused(false)

	override := "saved"
	otherIndex := 2
	selectView.SetConfirmedIndex(&otherIndex, &override)
	selectView.SetInputBuffer("different draft")
	text := renderStrings(selectView.Render(40))
	if strings.Contains(text, "✔") {
		t.Fatalf("pending draft still marked as committed:\n%s", text)
	}
	if !strings.Contains(text, "different draft") {
		t.Fatalf("draft not shown:\n%s", text)
	}

	// Matching the confirmed text restores the mark.
	selectView.SetInputBuffer("saved")
	text = renderStrings(selectView.Render(40))
	if !strings.Contains(text, "✔") {
		t.Fatalf("mark missing when the draft matches:\n%s", text)
	}
}

// The focused custom row renders the inline editor with a cursor.
func TestWrappingSelectInlineInputRow(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	selectView.SetSelectedIndex(2)
	selectView.SetInputBuffer("typing")
	lines := selectView.Render(40)
	joined := strings.Join(lines, "")
	if !strings.Contains(joined, cursorMarker) {
		t.Fatal("no cursor marker in the inline row")
	}
	if !strings.Contains(renderStrings(lines), "typing") {
		t.Fatalf("buffer text missing:\n%s", renderStrings(lines))
	}
}

func TestWrappingSelectFocusedItemRowRange(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})

	// Row 0 is option A: label (1) + description (1) = 2 rows.
	if got := selectView.FocusedItemRowRange(40); got != [2]int{0, 2} {
		t.Fatalf("range = %#v", got)
	}
	selectView.SetSelectedIndex(1)
	if got := selectView.FocusedItemRowRange(40); got != [2]int{2, 4} {
		t.Fatalf("range = %#v", got)
	}
	// The range must agree with Render's row count.
	selectView.SetSelectedIndex(2)
	rangeOf := selectView.FocusedItemRowRange(40)
	rendered := len(selectView.Render(40))
	if rangeOf[1] > rendered {
		t.Fatalf("range %#v exceeds %d rendered rows", rangeOf, rendered)
	}
}

// A long label wraps and each continuation line keeps the matching prefix.
func TestWrappingSelectWrapsLongLabels(t *testing.T) {
	long := strings.Repeat("word ", 20)
	q := twoOptions("H", "Q?")
	q.Options[0].Label = long
	q.Options[0].Description = ""
	items := BuildItemsForQuestion(q)
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	lines := selectView.Render(30)
	if len(lines) < 3 {
		t.Fatalf("expected wrapping, got %d line(s)", len(lines))
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(stripANSI(line), "❯") {
			t.Fatalf("pointer repeated on a continuation line: %q", line)
		}
	}
}

func TestWrappingSelectSetNumbering(t *testing.T) {
	items := []WrappingSelectItem{{Kind: RowKindOption, Label: "x"}}
	selectView := NewWrappingSelect(items, 10, plainTheme, WrappingSelectOptions{})
	selectView.SetNumbering(2, 100)
	text := renderStrings(selectView.Render(20))
	if !strings.Contains(text, "3. x") {
		t.Fatalf("numbering not applied:\n%s", text)
	}
	// A zero total is clamped to 1.
	selectView.SetNumbering(0, 0)
	if text := renderStrings(selectView.Render(20)); !strings.Contains(text, "1. x") {
		t.Fatalf("zero total not clamped:\n%s", text)
	}
}

// ── OptionListView ────────────────────────────────────────────────────────

func TestOptionListViewProjectsProps(t *testing.T) {
	items := BuildItemsForQuestion(twoOptions("H", "Q?"))
	view := NewOptionListView(items, plainTheme)

	view.SetProps(OptionListViewProps{SelectedIndex: 1, Focused: true, InputBuffer: ""})
	text := renderStrings(view.Render(40))
	if !strings.Contains(text, "❯ 2. B") {
		t.Fatalf("selected row not drawn:\n%s", text)
	}

	view.SetProps(OptionListViewProps{
		SelectedIndex: 0,
		Focused:       true,
		Confirmed:     &ConfirmedIndicator{Index: 1},
	})
	text = renderStrings(view.Render(40))
	if !strings.Contains(text, "2. B ✔") {
		t.Fatalf("confirmed marker not drawn:\n%s", text)
	}

	view.SetProps(OptionListViewProps{SelectedIndex: 0, Focused: true})
	if text := renderStrings(view.Render(40)); strings.Contains(text, "✔") {
		t.Fatalf("confirmed marker not cleared:\n%s", text)
	}
}

func TestOptionListViewCapsVisibleRows(t *testing.T) {
	labels := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		labels = append(labels, "opt"+itoa(i))
	}
	view := NewOptionListView(BuildItemsForQuestion(question("H", "Q?", labels...)), plainTheme)
	// MAX_VISIBLE_OPTIONS caps the window; with descriptions each row is two
	// lines, plus the scroll-info line.
	rows := len(view.Render(40))
	if rows > 2*MaxVisibleOptions+1 {
		t.Fatalf("rendered %d rows, above the %d cap", rows, 2*MaxVisibleOptions+1)
	}
}

func TestOptionListViewInvalidateAndHandleInput(t *testing.T) {
	view := NewOptionListView(BuildItemsForQuestion(twoOptions("H", "Q?")), plainTheme)
	view.HandleInput("x")
	view.Invalidate()
	if len(view.Render(30)) == 0 {
		t.Fatal("no rows after invalidate")
	}
}

// ── MultiSelectView ───────────────────────────────────────────────────────

func TestMultiSelectViewRender(t *testing.T) {
	multi := multiQuestion("Which?", "A", "B")
	multi.Options[0].Description = "first option"
	view := NewMultiSelectView(themeForTests(), multi)

	view.SetProps(MultiSelectViewProps{
		Rows:      []MultiSelectRowProps{{Checked: true, Active: true}, {Checked: false, Active: false}},
		Other:     MultiSelectOtherRowProps{},
		NextLabel: SentinelNextLabel,
	})
	text := renderStrings(view.Render(40))
	if !strings.Contains(text, "[✔] A") {
		t.Fatalf("checked box missing:\n%s", text)
	}
	if !strings.Contains(text, "[ ] B") {
		t.Fatalf("unchecked box missing:\n%s", text)
	}
	if !strings.Contains(text, "first option") {
		t.Fatalf("description missing:\n%s", text)
	}
	if !strings.Contains(text, "Type something.") {
		t.Fatalf("custom row missing:\n%s", text)
	}
	if !strings.Contains(text, "Next") {
		t.Fatalf("next row missing:\n%s", text)
	}
}

func TestMultiSelectViewOtherRowStates(t *testing.T) {
	multi := multiQuestion("Which?", "A", "B")
	view := NewMultiSelectView(themeForTests(), multi)

	// Checked custom text renders in place of the sentinel label.
	view.SetProps(MultiSelectViewProps{
		Rows:      []MultiSelectRowProps{{}, {}},
		Other:     MultiSelectOtherRowProps{Checked: true, InputBuffer: "my answer"},
		NextLabel: SentinelNextLabel,
	})
	text := renderStrings(view.Render(40))
	if !strings.Contains(text, "[✔] my answer") {
		t.Fatalf("custom text not drawn:\n%s", text)
	}

	// Input mode renders the inline editor with a cursor.
	view.SetProps(MultiSelectViewProps{
		Rows:      []MultiSelectRowProps{{}, {}},
		Other:     MultiSelectOtherRowProps{Active: true, InputMode: true, InputBuffer: "typing"},
		NextLabel: SentinelNextLabel,
	})
	joined := strings.Join(view.Render(40), "")
	if !strings.Contains(joined, cursorMarker) {
		t.Fatal("no cursor marker in input mode")
	}
}

func TestMultiSelectViewNaturalHeightAndFocus(t *testing.T) {
	multi := multiQuestion("Which?", "A", "B")
	multi.Options[0].Description = ""
	multi.Options[1].Description = ""
	view := NewMultiSelectView(themeForTests(), multi)
	view.SetProps(MultiSelectViewProps{
		Rows:      []MultiSelectRowProps{{Active: true}, {}},
		Other:     MultiSelectOtherRowProps{},
		NextLabel: SentinelNextLabel,
	})
	height := view.NaturalHeight(40)
	if height != len(view.Render(40)) {
		t.Fatalf("naturalHeight = %d, render = %d", height, len(view.Render(40)))
	}
	if got := view.FocusedItemRowRange(40); got[0] != 0 {
		t.Fatalf("focused range = %#v", got)
	}

	// The Next row owns focus when active.
	view.SetProps(MultiSelectViewProps{
		Rows:       []MultiSelectRowProps{{}, {}},
		NextActive: true,
		NextLabel:  SentinelNextLabel,
	})
	rangeOf := view.FocusedItemRowRange(40)
	if rangeOf[0] == 0 && rangeOf[1] == 0 {
		t.Fatalf("Next row focus not reported: %#v", rangeOf)
	}
}

func TestMultiSelectViewCachesLayoutByWidth(t *testing.T) {
	view := NewMultiSelectView(themeForTests(), multiQuestion("Q?", "A", "B"))
	view.SetProps(MultiSelectViewProps{Rows: []MultiSelectRowProps{{}, {}}, NextLabel: SentinelNextLabel})
	first := view.Render(40)
	second := view.Render(40)
	if renderStrings(first) != renderStrings(second) {
		t.Fatal("repeated renders differ at the same width")
	}
	view.Invalidate()
	if renderStrings(view.Render(40)) != renderStrings(first) {
		t.Fatal("invalidate changed the output")
	}
	view.HandleInput("x")
}

// ── TabBar / SubmitPicker ─────────────────────────────────────────────────

func TestTabBarRender(t *testing.T) {
	bar := NewTabBar(themeForTests())
	bar.SetProps(TabBarProps{
		Tabs: []TabBarTab{
			{Label: "Auth", Answered: true, Active: true},
			{Label: "Features", Answered: false, Active: false},
		},
		Submit: TabBarSubmit{Active: false, AllAnswered: false},
	})
	lines := bar.Render(80)
	if len(lines) != 2 || lines[1] != "" {
		t.Fatalf("tab bar rows = %#v", lines)
	}
	text := renderStrings(lines)
	if !strings.Contains(text, "←") || !strings.Contains(text, "→") {
		t.Fatalf("arrows missing:\n%s", text)
	}
	if !strings.Contains(text, "■ Auth") {
		t.Fatalf("answered box missing:\n%s", text)
	}
	if !strings.Contains(text, "□ Features") {
		t.Fatalf("unanswered box missing:\n%s", text)
	}
	if !strings.Contains(text, "✓ Submit") {
		t.Fatalf("submit chip missing:\n%s", text)
	}
	bar.HandleInput("x")
	bar.Invalidate()
}

func TestTabBarTruncates(t *testing.T) {
	bar := NewTabBar(themeForTests())
	bar.SetProps(TabBarProps{Tabs: []TabBarTab{{Label: strings.Repeat("long", 30)}}})
	line := bar.Render(20)[0]
	if visibleWidth(line) > 20 {
		t.Fatalf("line width = %d", visibleWidth(line))
	}
}

func TestSubmitPickerRender(t *testing.T) {
	picker := NewSubmitPicker(themeForTests())
	if got := picker.NaturalHeight(80); got != 2 {
		t.Fatalf("naturalHeight = %d", got)
	}
	picker.SetProps(SubmitPickerProps{Rows: []SubmitPickerRow{{Active: true}, {}}})
	lines := picker.Render(40)
	if len(lines) != 2 {
		t.Fatalf("rows = %d", len(lines))
	}
	text := renderStrings(lines)
	if !strings.Contains(text, "❯ 1. Submit answers") {
		t.Fatalf("active submit row missing:\n%s", text)
	}
	if !strings.Contains(text, "2. Cancel") {
		t.Fatalf("cancel row missing:\n%s", text)
	}

	// Cancel focused.
	picker.SetProps(SubmitPickerProps{Rows: []SubmitPickerRow{{}, {Active: true}}})
	text = renderStrings(picker.Render(40))
	if !strings.Contains(text, "❯ 2. Cancel") {
		t.Fatalf("active cancel row missing:\n%s", text)
	}
	picker.HandleInput("x")
	picker.Invalidate()

	// Labels localize through the bridge.
	withLocale(t, "zh")
	text = renderStrings(picker.Render(40))
	if !strings.Contains(text, "提交答案") || !strings.Contains(text, "取消") {
		t.Fatalf("localized labels missing:\n%s", text)
	}
}

// ── scroll window ─────────────────────────────────────────────────────────

func TestComputeScrollStart(t *testing.T) {
	noFocus := ComputeScrollStart(nil, 0, 10, 40)
	if noFocus != 0 {
		t.Fatalf("no-focus start = %d", noFocus)
	}

	// A focus near the top keeps the window at the top.
	focus := [2]int{1, 3}
	if got := ComputeScrollStart(&focus, 0, 10, 40); got != 0 {
		t.Fatalf("top start = %d", got)
	}

	// A focus in the middle centres the item.
	mid := [2]int{20, 22}
	got := ComputeScrollStart(&mid, 0, 10, 40)
	if got <= 0 || got+10 > 40 {
		t.Fatalf("middle start = %d", got)
	}
	wantStart := 20 - (10-2)/2
	if got != wantStart {
		t.Fatalf("middle start = %d, want %d", got, wantStart)
	}

	// A focus at the end clamps to the last window.
	end := [2]int{39, 40}
	if got := ComputeScrollStart(&end, 0, 10, 40); got != 30 {
		t.Fatalf("end start = %d", got)
	}
}

func TestDecorateOverflow(t *testing.T) {
	theme := themeForTests()

	t.Run("both arrows", func(t *testing.T) {
		lines := []string{"a", "b", "c"}
		DecorateOverflow(lines, true, true, theme, nil)
		if lines[0] != overflowUp || lines[2] != overflowDown {
			t.Fatalf("lines = %#v", lines)
		}
	})

	t.Run("single row shows the combined arrow", func(t *testing.T) {
		lines := []string{"a"}
		DecorateOverflow(lines, true, true, theme, nil)
		if lines[0] != overflowBoth {
			t.Fatalf("lines = %#v", lines)
		}
	})

	t.Run("protected rows are never covered", func(t *testing.T) {
		lines := []string{"a", "b", "c"}
		protect := [2]int{0, 1}
		DecorateOverflow(lines, true, true, theme, &protect)
		if lines[0] != "a" {
			t.Fatalf("protected row overwritten: %#v", lines)
		}
		if lines[2] != overflowDown {
			t.Fatalf("bottom arrow missing: %#v", lines)
		}
	})

	t.Run("down on the first row only", func(t *testing.T) {
		lines := []string{"a", "b"}
		DecorateOverflow(lines, false, true, theme, nil)
		if lines[0] != "a" || lines[1] != overflowDown {
			t.Fatalf("lines = %#v", lines)
		}
	})
}

func TestWindowLines(t *testing.T) {
	theme := themeForTests()

	t.Run("fits unchanged", func(t *testing.T) {
		lines := []string{"a", "b"}
		focus := [2]int{0, 1}
		result := WindowLines(lines, 5, &focus, theme)
		if len(result.Lines) != 2 || result.Focus == nil || *result.Focus != focus {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("windows and remaps focus", func(t *testing.T) {
		lines := make([]string, 0, 40)
		for i := 0; i < 40; i++ {
			lines = append(lines, "row"+itoa(i))
		}
		focus := [2]int{20, 21}
		result := WindowLines(lines, 10, &focus, theme)
		if len(result.Lines) != 10 {
			t.Fatalf("window = %d rows", len(result.Lines))
		}
		if result.Focus == nil {
			t.Fatal("focus lost")
		}
		if result.Focus[0] < 0 || result.Focus[1] > 10 {
			t.Fatalf("focus not remapped into the window: %#v", *result.Focus)
		}
		// The focused row is still visible in the window.
		row := result.Lines[result.Focus[0]]
		if row != "row20" {
			t.Fatalf("row under focus = %q, want row20", row)
		}
	})
}

// ── presentation ──────────────────────────────────────────────────────────

func TestRowBudgets(t *testing.T) {
	if got := FullRowBudget(30); got != 30 {
		t.Fatalf("full budget = %d", got)
	}
	// 70% of 40 = 28, above the floor.
	if got := InlineRowBudget(40); got != 28 {
		t.Fatalf("inline budget(40) = %d", got)
	}
	// Small terminals hit the floor, then the terminal itself caps it.
	if got := InlineRowBudget(10); got != 10 {
		t.Fatalf("inline budget(10) = %d", got)
	}
	if got := InlineRowBudget(14); got != 12 {
		t.Fatalf("inline budget(14) = %d", got)
	}
}

func TestResolvePresentationClaimsTheSlot(t *testing.T) {
	resetInlineSlot(t)

	first := ResolvePresentation()
	if first.Overlay {
		t.Fatal("first presentation should be inline")
	}
	if first.Release == nil {
		t.Fatal("inline presentation must expose a release")
	}
	if first.RowBudget(100) != InlineRowBudget(100) {
		t.Fatal("inline presentation should use the inline budget")
	}

	// A second announcement degrades to the overlay.
	second := ResolvePresentation()
	if !second.Overlay || !second.HideOnCollapse {
		t.Fatalf("second presentation = %#v", second)
	}
	if second.RowBudget(100) != 100 {
		t.Fatal("overlay presentation should use the full budget")
	}

	// Releasing frees the slot, and release is idempotent.
	first.Release()
	first.Release()
	third := ResolvePresentation()
	if third.Overlay {
		t.Fatal("slot not released")
	}
	third.Release()
}

// resetInlineSlot clears the package-level slot flag between tests.
func resetInlineSlot(t *testing.T) {
	t.Helper()
	inlineSlotHeld = false
	t.Cleanup(func() { inlineSlotHeld = false })
}

func TestStackedOverlayPresentation(t *testing.T) {
	if !StackedOverlayPresentation.Overlay {
		t.Fatal("stacked presentation must be an overlay")
	}
	if StackedOverlayPresentation.HideOnCollapse {
		t.Fatal("stacked presentation must not hide on collapse")
	}
	if StackedOverlayPresentation.Release != nil {
		t.Fatal("stacked presentation owns no slot")
	}
}

func TestMustYieldInlineSlot(t *testing.T) {
	if MustYieldInlineSlot(nil) {
		t.Fatal("no probe should never yield")
	}
	if !MustYieldInlineSlot(func() bool { return true }) {
		t.Fatal("a visible overlay should force a yield")
	}
	if MustYieldInlineSlot(func() bool { return false }) {
		t.Fatal("no overlay should not yield")
	}
}

func TestRowBudgetForHost(t *testing.T) {
	inline := RowBudget(InlineRowBudget)
	if got := RowBudgetForHost(true, inline)(40); got != 40 {
		t.Fatalf("host-allocated budget = %d", got)
	}
	if got := RowBudgetForHost(false, inline)(40); got != InlineRowBudget(40) {
		t.Fatalf("default budget = %d", got)
	}
}

func TestYieldToOverlayIsACancelResult(t *testing.T) {
	if YieldToOverlay == nil || !YieldToOverlay.Cancelled || len(YieldToOverlay.Answers) != 0 {
		t.Fatalf("yield result = %#v", YieldToOverlay)
	}
}
