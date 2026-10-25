package cachestats

import (
	"math"
	"strconv"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file ports the custom footer of cache-stats.ts (TS L512-533 and
// L611-700 and L966-1055): the geometric icon set, the extension-status
// segment, the live dual hit-rate segment (◆ cumulative / ◇ latest), the
// context-usage segment with its occupancy colors, the tokens/s segment and
// the merged model segment.

// footerIcons is FOOTER_ICON: one geometric symbol family, no emoji mixin.
const (
	footerIconSheep   = "●"
	footerIconHit     = "◆"
	footerIconLatest  = "◇"
	footerIconContext = "▲"
	footerIconTPS     = "▸"
	footerIconModel   = "■"
)

// shouldInstallFooter installs the footer only when it is enabled and Pi runs
// in TUI mode. TS: shouldInstallFooter (L870-872).
func shouldInstallFooter(footerOn bool, mode string) bool {
	return footerOn && mode == "tui"
}

// baselineKey renders footerResetBaseline for the dirty-check cache key; a nil
// baseline is the TS null.
func baselineKey(baseline *int) string {
	if baseline == nil {
		return "null"
	}
	return strconv.Itoa(*baseline)
}

// hitCacheKey is the footerHitCache key: the run epoch, the reset baseline and
// the live entry count. It names the inputs the cached hit rates were computed
// from, so a reset, a baseline change or an appended entry all miss.
func hitCacheKey(epoch int, baseline *int, entries int) string {
	return strconv.Itoa(epoch) + "|" + baselineKey(baseline) + "|" + strconv.Itoa(entries)
}

// footerRender renders the single footer line. statuses is
// footerRenderSnapshot is one consistent read of the footer inputs. The footer
// renderer runs on a background goroutine (surface.go:100-123), so it takes
// this copy under instanceState.mu and then renders from the copy: building the
// string touches only local values, never live state, and never holds the lock
// across the theme/SDK work.
type footerRenderSnapshot struct {
	// hitRates is nil when the source could not be read and the snapshot had
	// no cache interaction, in which case both diamonds show n/a.
	hitRates     *hitRates
	context      *footerContext
	contextKnown bool
	tps          *string
	modelName    string
	thinking     *string
}

// captureFooterSnapshot reads the footer inputs under the lock, recomputing the
// live hit rates (and refreshing footerHitCache) when the dirty-check key
// changed. entriesFn and usageFn are copies of the state's source callbacks: the
// callbacks themselves are called after the lock is released, because they reach
// into the host session manager.
//
// The entries are read outside the lock, so a reset / session_start can land
// while they are being read. Both of those bump footerResetEpoch, so the
// computation only writes footerHitCache when the epoch still matches the one
// captured before the read: otherwise the entries in hand describe the previous
// run, and caching them under the new epoch would keep a stale hit rate alive
// for as long as the (necessarily equal) entry count did not change. Such a
// landing reset also drops the sources, so a mismatched capture renders the
// documented fallback (the current-run snapshot) instead of the torn entries --
// and it re-reads that snapshot under the lock it is already holding, because
// the copy taken before the read describes the run the reset replaced.
//
// The second generation input, footerResetBaseline, is covered the other way:
// it is re-read under the lock and folded into hitCacheKey, so a mid-read
// baseline drift cannot reuse the cached rates (the key misses) nor store one
// computed from the wrong window -- the rates are recomputed against the
// baseline that is current at write time.
func captureFooterSnapshot(state *instanceState) (snap footerRenderSnapshot, entriesFn func() ([]any, bool), usageFn func() (*sdk.ContextUsage, bool)) {
	state.mu.Lock()
	snap.modelName = state.footerModelName
	snap.thinking = state.footerThinking
	snap.tps = state.lastTurnTps
	snap.context = state.footerContext
	resetBaseline := state.footerResetBaseline
	hitCache := state.footerHitCache
	snapshotTotals := state.snapshot
	capturedEpoch := state.footerResetEpoch
	if state.footerSources != nil {
		entriesFn = state.footerSources.entries
		usageFn = state.footerSources.getContextUsage
	}
	state.mu.Unlock()

	// Read the entries outside the lock and then take it again only for the
	// cache compare/write, so a concurrent reset (which bumps the epoch) or a
	// concurrent render cannot observe a half-updated cache.
	var live hitRates
	liveHitComputed := false
	if entriesFn != nil {
		if entries, ok := entriesFn(); ok {
			state.mu.Lock()
			// Re-read the key inputs: a reset may have landed while the
			// entries were being read above. Both cache paths below are
			// gated on the epoch captured before that read.
			resetBaseline = state.footerResetBaseline
			resetEpoch := state.footerResetEpoch
			hitCache = state.footerHitCache
			switch {
			case resetEpoch != capturedEpoch:
				// resetRunState / session_start ran mid-read (it also cleared
				// footerSources and footerHitCache): the entries in hand belong
				// to the previous run, so neither reuse nor store them. The
				// caller falls through to the current-run snapshot -- which must
				// be the post-reset one: the snapshotTotals captured before the
				// read still describes the previous run, and rendering it would
				// show the old run's hit rate for one frame after the reset.
				// Every other input the fallback renders is refreshed the same
				// way, so this render shows the reset state consistently instead
				// of a new-run hit rate next to stale model/context/tps segments.
				snapshotTotals = state.snapshot
				snap.modelName = state.footerModelName
				snap.thinking = state.footerThinking
				snap.tps = state.lastTurnTps
				snap.context = state.footerContext
			case hitCache != nil && hitCache.key == hitCacheKey(resetEpoch, resetBaseline, len(entries)):
				live = hitRates{aggregate: hitCache.aggregate, latest: hitCache.latest}
				liveHitComputed = true
			default:
				live = computeLiveHitRates(entries, resetBaseline)
				state.footerHitCache = &footerHitCache{key: hitCacheKey(resetEpoch, resetBaseline, len(entries)), aggregate: live.aggregate, latest: live.latest}
				liveHitComputed = true
			}
			state.mu.Unlock()
		}
	}
	if liveHitComputed {
		snap.hitRates = &live
		return snap, entriesFn, usageFn
	}
	// Fallback: the current-run snapshot, used when no live source answered.
	fallback := hitRates{}
	if !(snapshotTotals.totalCacheRead == 0 && snapshotTotals.totalCacheWrite == 0) {
		fallback.aggregate = aggregateHit(snapshotTotals.totalCacheRead, snapshotTotals.totalHitDenom)
	}
	snap.hitRates = &fallback
	return snap, entriesFn, usageFn
}

// footerRender renders the single footer line. statuses is
// footerData.getExtensionStatuses(). TS: the render() body of installFooter
// (L617-700).
//
// It is the function the SDK calls from a background goroutine, so it takes a
// consistent snapshot under the lock and renders from that copy. It must never
// call back into the SDK while holding instanceState.mu (see instanceState).
func footerRender(state *instanceState, theme themeStyler, statuses map[string]string, width int) string {
	snap, _, usageFn := captureFooterSnapshot(state)
	parts := []string{}
	// 1. shepherd/herdsman status (only if results returned) - icon ●
	var sheep *string
	if value, ok := statuses["herdsman"]; ok {
		sheep = &value
	} else if value, ok := statuses["shepherd"]; ok {
		sheep = &value
	}
	if herdsmanStatus := formatHerdsmanStatus(sheep); herdsmanStatus != nil {
		parts = append(parts, theme.Fg("dim", footerIconSheep)+" "+theme.Fg("text", *herdsmanStatus))
	}

	// 2. cache hit rate (live dual display: cumulative ◆, latest ◇)
	var agg, latest *int
	if snap.hitRates != nil {
		agg = snap.hitRates.aggregate
		latest = snap.hitRates.latest
	}
	aggStr := theme.Fg("dim", "n/a")
	if agg != nil {
		aggStr = theme.Fg("text", strconv.Itoa(*agg)+"%")
	}
	latestStr := theme.Fg("dim", "n/a")
	if latest != nil {
		latestStr = theme.Fg("warning", strconv.Itoa(*latest)+"%")
	}
	parts = append(parts, theme.Fg("dim", footerIconHit)+" "+aggStr+" "+theme.Fg("dim", footerIconLatest)+" "+latestStr)

	// 3. context usage - icon ▲ (percent/compact contextWindow)
	context := snap.context
	if usageFn != nil {
		if raw, ok := usageFn(); ok && raw != nil {
			context = &footerContext{tokens: raw.Tokens, contextWindow: raw.ContextWindow, percent: raw.Percent}
		}
	}
	var contextPercent *float64
	if context != nil && context.percent != nil {
		contextPercent = context.percent
	}
	contextStr := theme.Fg("dim", "n/a")
	if contextPercent != nil {
		rounded := strconv.Itoa(int(math.Round(*contextPercent)))
		display := rounded + "%"
		if context != nil && context.contextWindow != 0 {
			display = rounded + "%/" + formatWindowSize(float64(context.contextWindow))
		}
		contextStr = contextSegment(theme, *contextPercent, display)
	}
	parts = append(parts, theme.Fg("dim", footerIconContext)+" "+contextStr)

	// 4. last-turn generation speed (estimate) - icon ▸ (tokens/s)
	tpsStr := theme.Fg("dim", "n/a")
	if snap.tps != nil {
		tpsStr = theme.Fg("text", *snap.tps+" t/s")
	}
	parts = append(parts, theme.Fg("dim", footerIconTPS)+" "+tpsStr)

	// 5. model name + thinking level - icon ■ (single segment, merged)
	if snap.modelName != "" {
		label := stripProvider(snap.modelName)
		if snap.thinking != nil && *snap.thinking != "" {
			label += " · " + *snap.thinking
		}
		parts = append(parts, theme.Fg("dim", footerIconModel)+" "+theme.Fg("text", label))
	}
	full := ""
	separator := theme.Fg("dim", " | ")
	for i, part := range parts {
		if i > 0 {
			full += separator
		}
		full += part
	}
	return truncateFooter(full, width, theme)
}
