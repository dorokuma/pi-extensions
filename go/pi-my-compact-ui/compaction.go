package compactui

import (
	"strconv"
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

// This file ports src/compaction.ts (L1512-1562): the custom compaction
// summary header and the summary component patch.
//
// The TS patches CompactionSummaryMessageComponent.prototype.updateDisplay so
// the host draws PT's header. A subprocess extension does not own that
// prototype, so the port renders the header itself (CompactionHeaderComponent)
// and exposes it through installedCompactionStyle; the patch bookkeeping keeps
// the TS names so the mapping is mechanical.

// renderWidth is CompactionHeaderComponent.render (L1518-1535): the wide form
// "›‹ Context compacted • 12,345 tokens → summary" and, when it does not fit,
// the short form "›‹ Compacted • 12.3K tok".
func (h *compactionHeaderComponent) renderWidth(width int) []string {
	var th theme
	if h.tui != nil {
		h.tui.mu.Lock()
		th = h.tui.themeLocked()
		h.tui.mu.Unlock()
	}
	tokensBefore := h.tokensBefore
	if tokensBefore < 0 {
		tokensBefore = 0
	}
	exact := groupThousands(tokensBefore)
	icon := fgOf(th, "success", "›‹")
	title := fgOf(th, "success", "Context compacted")
	detail := fgOf(th, "muted", " • "+exact+" tokens → summary")
	full := icon + " " + title + detail
	if tui.VisibleWidth(full) <= width {
		return []string{full}
	}
	compactTitle := fgOf(th, "success", "Compacted")
	compactDetail := fgOf(th, "muted", " • "+formatTokenK(tokensBefore)+" tok")
	return []string{tui.TruncateToWidth(icon+" "+compactTitle+compactDetail, maxInt(1, width), "...", false)}
}

// groupThousands is Number.prototype.toLocaleString() with the en-US default:
// "12345" -> "12,345".
func groupThousands(value int) string {
	digits := strconv.Itoa(value)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign = "-"
		digits = digits[1:]
	}
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// installCompactionSummaryRendering is installCompactionSummaryRendering
// (L1537-1562): it records the installed style and builds the header the
// summary renders. tokensBefore is the message's `tokensBefore` value; a
// non-finite or non-positive count renders as 0, as the TS
// `Number.isFinite(tokensBefore) && tokensBefore > 0 ? tokensBefore : 0` does.
func (r *runtimeState) installCompactionSummaryRendering(tokensBefore any) *compactionHeaderComponent {
	tokens := 0
	if number, ok := toNumber(tokensBefore); ok && number > 0 {
		tokens = int(number)
	}
	r.mu.Lock()
	r.compactionStyleInstalled = true
	r.mu.Unlock()
	return &compactionHeaderComponent{tokensBefore: tokens, tui: r}
}

// compactionSummaryRender is the patched updateDisplay (L1546-1554): the
// summary body PT draws, with paddingX = GROUP_PADDING_X, paddingY = 0 and no
// background.
func (r *runtimeState) compactionSummaryRender(tokensBefore any, width int) []string {
	header := r.installCompactionSummaryRendering(tokensBefore)
	inner := maxInt(1, width-2*groupPaddingX)
	padding := strings.Repeat(" ", groupPaddingX)
	lines := header.renderWidth(inner)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, padding+line)
	}
	return out
}

// tuiLen is tui.VisibleWidth under the short name the renderers use.
func tuiLen(text string) int { return tui.VisibleWidth(text) }

// truncateWidth is tui's truncateToWidth with upstream's default ellipsis and
// no padding.
func truncateWidth(text string, width int) string {
	return tui.TruncateToWidth(text, width, tui.DefaultEllipsis, false)
}
