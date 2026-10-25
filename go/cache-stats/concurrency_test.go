package cachestats

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Ports the review-driven regressions of round 2 (must fix 1 and must fix 4):
// the footer/lifecycle shared-state race, and the removed model-endpoint cache.

// TestModelEndpointsReadEveryTime is the must-fix-4 regression. The endpoint
// lookup must consult the live registry on every observation: a cache keyed by
// (provider, id) that also memoized a failed/empty lookup pinned the scope to
// "skipped" for the whole session, and a same-named model whose baseUrl/api
// changed kept hashing the old endpoint into the scope key.
func TestModelEndpointsReadEveryTime(t *testing.T) {
	var reads int
	// The registry starts empty (not yet populated), then answers, then the
	// same (provider, id) moves to a different endpoint.
	responses := [][]map[string]any{
		{},
		{{"provider": "audit", "id": "offline", "api": "openai-completions", "baseUrl": "https://a.invalid/v1"}},
		{{"provider": "audit", "id": "offline", "api": "anthropic-messages", "baseUrl": "https://b.invalid/v1"}},
	}
	source := func() ([]map[string]any, error) {
		index := reads
		reads++
		if index < len(responses) {
			return responses[index], nil
		}
		return responses[len(responses)-1], nil
	}

	// 1. Empty registry: unanswered, not a permanent verdict.
	api, baseURL, found, err := modelEndpointsFrom(source, "audit", "offline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found || api != nil || baseURL != nil {
		t.Fatalf("an empty registry must answer unanswered: found=%v api=%v baseUrl=%v", found, api, baseURL)
	}

	// 2. The registry becomes readable: the very next observation sees it.
	api, baseURL, found, _ = modelEndpointsFrom(source, "audit", "offline")
	if !found || api == nil || *api != "openai-completions" || baseURL == nil || *baseURL != "https://a.invalid/v1" {
		t.Fatalf("a later read must see the populated registry: found=%v api=%v baseUrl=%v", found, api, baseURL)
	}

	// 3. Same (provider, id), new endpoint: reflected immediately.
	api, baseURL, found, _ = modelEndpointsFrom(source, "audit", "offline")
	if !found || api == nil || *api != "anthropic-messages" || baseURL == nil || *baseURL != "https://b.invalid/v1" {
		t.Fatalf("an endpoint change must be visible at once: found=%v api=%v baseUrl=%v", found, api, baseURL)
	}

	if reads != 3 {
		t.Fatalf("registry reads = %d, want 3 (one per observation, never cached)", reads)
	}
}

// TestModelEndpointsErrorNotSticky guards the failure half of must fix 4: a
// registry error must not be remembered either.
func TestModelEndpointsErrorNotSticky(t *testing.T) {
	fail := true
	source := func() ([]map[string]any, error) {
		if fail {
			return nil, errors.New("host transport down")
		}
		return []map[string]any{{"provider": "p", "id": "m", "baseUrl": "https://ok.invalid"}}, nil
	}
	if _, _, _, err := modelEndpointsFrom(source, "p", "m"); err == nil {
		t.Fatal("a registry error must be reported")
	}
	fail = false
	api, baseURL, found, err := modelEndpointsFrom(source, "p", "m")
	if err != nil || !found {
		t.Fatalf("the next read must retry: err=%v found=%v", err, found)
	}
	if baseURL == nil || *baseURL != "https://ok.invalid" {
		t.Fatalf("baseUrl = %v", baseURL)
	}
	if api != nil {
		t.Fatalf("a model without an api field must stay nil, got %v", *api)
	}
}

// TestScopeKeyDistinguishesEndpoints proves must fix 4 at the level the bug was
// observable: two observations of the same session/provider/model but different
// endpoints must not share a comparison scope.
func TestScopeKeyDistinguishesEndpoints(t *testing.T) {
	salt := [16]byte{9, 9, 9}
	first, ok := scopeKey(salt, scope("s", "p", "openai-completions", "m", "https://a.invalid/v1"))
	if !ok {
		t.Fatal("a complete identity must produce a key")
	}
	second, ok := scopeKey(salt, scope("s", "p", "anthropic-messages", "m", "https://b.invalid/v1"))
	if !ok {
		t.Fatal("a complete identity must produce a key")
	}
	if first == second {
		t.Fatalf("different endpoints share scope key %q", first)
	}
}

// TestFooterRenderConcurrentWithLifecycle is the must-fix-1 regression: the SDK
// renders the footer from a background goroutine (surface.go:100-123), so
// renderFooter races the lifecycle and command handlers that write the same
// state. Run under -race this fails on the unlocked implementation; it also
// asserts the renderer never observes a torn footerHitCache (a cache entry whose
// key does not match the values it claims).
func TestFooterRenderConcurrentWithLifecycle(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "race-model"
	entries := []any{}
	var mu sync.Mutex
	appendEntry := func(usage map[string]any) {
		mu.Lock()
		entries = append(entries, assistantEntry(usage))
		mu.Unlock()
	}
	readEntries := func() ([]any, bool) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]any, len(entries))
		copy(out, entries)
		return out, true
	}
	footerEntries(state, readEntries)

	var renderCount int64
	var wg sync.WaitGroup
	const workers = 4
	const iterations = 60

	// Renderers: the background-goroutine stand-ins.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				line := footerRender(state, plainTheme{}, map[string]string{"herdsman": "◆ Herdsman"}, 120)
				if line == "" {
					t.Errorf("footerRender produced an empty line")
					return
				}
				atomic.AddInt64(&renderCount, 1)
			}
		}()
	}

	// Lifecycle writers: turn_end / message_update / agent_start / agent_end and
	// the command handlers, all touching the same state.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			host := &fakeHost{}
			for n := 0; n < iterations; n++ {
				appendEntry(usageMap("input", float64(worker+1), "cacheRead", float64(n%7), "cacheWrite", 0))
				applyMessageUpdate(state, map[string]any{"type": "text_delta", "delta": "x"}, int64(n))
				applyMessageEnd(state, map[string]any{"role": "assistant"}, int64(n))
				applyTurnEndEvent(state, host, map[string]any{"message": assistantMessage(usageMap("input", 1, "cacheRead", 1, "cacheWrite", 0))})
				recordTurnUsage(state, assistantMessage(usageMap("input", 2, "cacheRead", 2, "cacheWrite", 0)))
				applyProviderRequestTiming(state)
				state.mu.Lock()
				state.liveRun = emptyLiveRun()
				state.mu.Unlock()
				applyAgentEnd(state, host, map[string]any{"messages": []any{}}, false)
				observePrefixDiagnostics(state, chatPayload("sys", nil, "hi"), scope("s", "p", "openai-completions", "m", "https://a.invalid"))
				_ = statsLines(state, sessionAggregate{aggregate: intPtr(50), available: true})
				_ = prefixLines(state)
				_ = prefixStatsLine(state)
			}
		}(i)
	}

	// Command-path writers: reset / enable / disable toggle the fields the
	// renderer and the other handlers read.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < iterations; n++ {
			readFooterCtx(state, footerReadInput{
				modelName:          fmt.Sprintf("model-%d", n),
				thinking:           strPtr("high"),
				hasSessionManager:  true,
				entries:            readEntries,
				hasGetContextUsage: true,
				getContextUsage:    func() (*sdkContextUsage, bool) { return nil, false },
				contextUsage:       sdkUsage(n, 1000, float64(n%100)),
			})
			enableRuntime(state)
			state.mu.Lock()
			baseline := n
			state.footerResetBaseline = &baseline
			state.footerResetEpoch++
			state.footerHitCache = nil
			state.mu.Unlock()
			disableRuntime(state)
			_ = runtimeEnabled(state)
			_ = prefixDiagEnabled(state)
			refreshFooter(state, n%2 == 0)
		}
	}()

	wg.Wait()

	if renderCount == 0 {
		t.Fatal("no renders ran")
	}

	// The dirty-check cache must never be left holding a key that contradicts
	// its own values (a torn write would show up as a key/value mismatch).
	state.mu.Lock()
	cache := state.footerHitCache
	resetEpoch := state.footerResetEpoch
	resetBaseline := state.footerResetBaseline
	state.mu.Unlock()
	if cache != nil {
		wantPrefix := fmt.Sprintf("%d|%s|", resetEpoch, baselineKey(resetBaseline))
		if len(cache.key) < len(wantPrefix) || cache.key[:len(wantPrefix)] != wantPrefix {
			t.Fatalf("footerHitCache key %q does not match epoch/baseline %q", cache.key, wantPrefix)
		}
		if cache.aggregate != nil && (*cache.aggregate < 0 || *cache.aggregate > 100) {
			t.Fatalf("footerHitCache aggregate out of range: %d", *cache.aggregate)
		}
		if cache.latest != nil && (*cache.latest < 0 || *cache.latest > 100) {
			t.Fatalf("footerHitCache latest out of range: %d", *cache.latest)
		}
	}
}

// TestFooterRefreshNotCalledUnderLock is the deadlock half of must fix 1: the
// SDK's SetFooterRenderer renders synchronously and retires the outgoing
// renderer by waiting on its in-flight push, which calls renderFooter and takes
// state.mu. If refreshFooter invoked the callback while holding mu, the two
// would deadlock. This test makes the callback re-enter footerRender (exactly
// what the SDK does) and fails on a timeout.
func TestFooterRefreshNotCalledUnderLock(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "reentrant"
	state.footerSources = &footerSources{entries: func() ([]any, bool) { return []any{}, true }}

	// footerRefresh stands in for ctx.SetFooterRenderer: the SDK renders
	// synchronously inside the call, so the callback reads the state.
	state.footerRefresh = func(bool) {
		_ = footerRender(state, plainTheme{}, map[string]string{}, 120)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			refreshFooter(state, i%2 == 0)
			recordTurnUsage(state, assistantMessage(usageMap("input", 1, "cacheRead", 1)))
			readFooterCtx(state, footerReadInput{modelName: "reentrant", hasSessionManager: true, entries: func() ([]any, bool) { return []any{}, true }})
		}
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("refreshFooter deadlocked: the refresh callback ran while instanceState.mu was held")
	}
}

// TestInstallFooterDoesNotDeadlock pins the same rule at the install path: a
// SetFooterRenderer that renders synchronously must not be called while mu is
// held. The fake host models the SDK's behaviour: installFooter's renderer is
// invoked from inside the SetFooterRenderer call.
func TestInstallFooterDoesNotDeadlock(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "install"

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Drive the state writes the renderer reads, then render directly, in a
		// loop; any lock ordering that lets install hold mu across a render
		// deadlocks here.
		for i := 0; i < 30; i++ {
			state.mu.Lock()
			state.footerModelName = fmt.Sprintf("install-%d", i)
			state.mu.Unlock()
			_ = footerRender(state, plainTheme{}, map[string]string{}, 80)
			state.mu.Lock()
			state.footerHitCache = nil
			state.mu.Unlock()
		}
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("footer render deadlocked against the state lock")
	}
}

// timeoutAfter bounds the deadlock tests: a regression must fail the suite
// instead of hanging it forever.
func timeoutAfter() <-chan struct{} {
	channel := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Second)
		close(channel)
	}()
	return channel
}

// ── round 3 regressions ───────────────────────────────────────────────────────

// TestFooterHitCacheNotWrittenAcrossReset is the round-3 must-fix-1 regression.
// The footer reads the session entries outside the lock, so a reset can land
// while they are being read. It used to write the result into footerHitCache
// under the *new* epoch, so a stale hit rate survived the reset for as long as
// the entry count stayed the same -- which it does, because the reset's own
// window is empty until a new entry is appended.
//
// The test reproduces exactly the arrangement that used to hide the bug: the
// entries in hand are the previous run's (same length as what the next render
// will see), and the footer has no live source afterwards. It also pins the
// *other* half of the same window: the fallback hit rates come from the run
// snapshot captured before the read, so that copy has to be re-read under the
// lock too (round-4 must-fix-1). Otherwise the blocked render returns the old
// run's 90% for one frame after the reset -- and reuses nothing, but shows a
// hit rate that no longer exists.
func TestFooterHitCacheNotWrittenAcrossReset(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "reset-race"

	// The previous run: two assistant entries with a 50% hit rate, and a run
	// snapshot whose own hit rate is a different, unmistakable 90%.
	stale := []any{
		assistantEntry(usageMap("input", 500, "cacheRead", 500, "cacheWrite", 0)),
		assistantEntry(usageMap("input", 500, "cacheRead", 500, "cacheWrite", 0)),
	}
	state.mu.Lock()
	state.snapshot.totalCacheRead = 90
	state.snapshot.totalCacheWrite = 10
	state.snapshot.totalHitDenom = 100
	state.mu.Unlock()

	release := make(chan struct{})
	readStarted := make(chan struct{})
	state.footerSources = &footerSources{entries: func() ([]any, bool) {
		// Announce the read and block inside it: the reset lands in this window,
		// which is exactly the interleaving under test (read -> reset -> write).
		close(readStarted)
		<-release
		return stale, true
	}}

	// blockedRender is the return value of the render that is blocked inside
	// entriesFn: the reset lands under it, so this is the render whose hit rates
	// used to come from the pre-reset snapshot.
	blockedRender := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		blockedRender <- footerRender(state, plainTheme{}, map[string]string{}, 120)
	}()

	<-readStarted
	// The reset: /cache-stats reset or session_start, both bump the epoch and
	// drop the sources. The stale entries are still identical in length to what
	// the state held, so an unguarded write would produce a cache key that the
	// next render matches.
	resetRunState(state)
	// The new run has a cache interaction of its own (25%), and the new epoch
	// already carries a cache entry keyed to a different window: the blocked
	// render must neither reuse that entry nor keep showing the 90%.
	state.mu.Lock()
	state.snapshot.totalCacheRead = 25
	state.snapshot.totalCacheWrite = 75
	state.snapshot.totalHitDenom = 100
	state.footerHitCache = &footerHitCache{key: hitCacheKey(state.footerResetEpoch, nil, 7), aggregate: intPtr(11), latest: intPtr(11)}
	state.mu.Unlock()
	close(release)

	var blocked string
	select {
	case blocked = <-blockedRender:
	case <-timeoutAfter():
		t.Fatal("footerRender deadlocked against the reset")
	}
	<-done

	// The blocked render fell through to the fallback, so it must render the
	// *post-reset* snapshot: the 25% the new run recorded, never the previous
	// run's 90% and never the mismatched cached 11%.
	if !strings.Contains(blocked, "◆ 25%") {
		t.Fatalf("the render that raced the reset = %q, want the post-reset 25%% hit rate", blocked)
	}
	if strings.Contains(blocked, "◆ 90%") {
		t.Fatalf("the render that raced the reset reused the pre-reset snapshot: %q", blocked)
	}
	if strings.Contains(blocked, "11%") {
		t.Fatalf("the render that raced the reset reused the mismatched new-epoch cache: %q", blocked)
	}

	state.mu.Lock()
	cache := state.footerHitCache
	epoch := state.footerResetEpoch
	state.mu.Unlock()
	if epoch == 0 {
		t.Fatal("the reset did not run; the test arrangement is wrong")
	}
	// The reset's own cache entry is the one the test planted; the blocked render
	// must not have replaced it with one computed from the stale entries.
	if cache == nil {
		t.Fatal("the blocked render must not drop the planted cache entry it did not compute")
	}
	if want := hitCacheKey(epoch, nil, 7); cache.key != want {
		t.Fatalf("footerHitCache key = %q, want the planted %q", cache.key, want)
	}

	// The next render sees the post-reset state with no live source, so it
	// still falls back to the post-reset snapshot (25%), not the stale 50%
	// the entries in hand would have produced.
	line := footerRender(state, plainTheme{}, map[string]string{}, 120)
	if !strings.Contains(line, "◆ 25%") || !strings.Contains(line, "◇ n/a") {
		t.Fatalf("the reset must clear the live hit rates in favour of the new snapshot, got %q", line)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	// The final render had no live source, so it must leave the planted entry
	// exactly as it found it instead of overwriting it from the stale entries.
	if state.footerHitCache == nil {
		t.Fatal("the post-reset render dropped the planted cache entry")
	}
	if want := hitCacheKey(epoch, nil, 7); state.footerHitCache.key != want {
		t.Fatalf("footerHitCache key = %q, want the planted %q", state.footerHitCache.key, want)
	}
}

// TestFooterHitCacheStillWrittenWithoutReset is the control for the test above:
// with no reset landing mid-read the very same arrangement must still populate
// footerHitCache, so the guard is not "never cache".
func TestFooterHitCacheStillWrittenWithoutReset(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "no-reset"
	entries := []any{assistantEntry(usageMap("input", 500, "cacheRead", 500, "cacheWrite", 0))}
	footerEntries(state, func() ([]any, bool) { return entries, true })

	line := footerRender(state, plainTheme{}, map[string]string{}, 120)
	if !strings.Contains(line, "◆ 50%") {
		t.Fatalf("footer = %q, want a live 50%% hit rate", line)
	}
	state.mu.Lock()
	cache := state.footerHitCache
	epoch := state.footerResetEpoch
	state.mu.Unlock()
	if cache == nil {
		t.Fatal("a render with a stable epoch must populate footerHitCache")
	}
	want := hitCacheKey(epoch, nil, len(entries))
	if cache.key != want {
		t.Fatalf("cache key = %q, want %q", cache.key, want)
	}
}

// sdkStubHost records the renderer installs the production footer path makes.
// Its SetFooterRenderer renders synchronously, exactly as the SDK does: it
// renders through the renderer it was just handed, and retiring the outgoing
// renderer would have to wait for its in-flight push, which renders and takes
// instanceState.mu.
type sdkStubHost struct {
	installs int
	nils     int
	renders  int
}

func (h *sdkStubHost) setFooterRenderer(state *instanceState, t *testing.T, render func(width int) []string) error {
	t.Helper()
	// Both the install and the uninstall path enter here, so this is the one
	// place that proves the production path never calls SetFooterRenderer while
	// holding instanceState.mu. A failure to acquire means the caller holds it:
	// that is the regression, reported as such instead of deadlocking.
	if !state.mu.TryLock() {
		t.Fatalf("setFooterRendererFn invoked while state.mu is held")
	}
	state.mu.Unlock()
	h.installs++
	if render == nil {
		h.nils++
		return nil
	}
	// The SDK renders the renderer it was handed before returning, so this
	// renderer runs inside the call, with instanceState.mu released. Render it
	// for real and require a line back: no recover() here, because swallowing a
	// panic is exactly what used to hide the host reads in installFooter's
	// closure panicking before footerRender ever reached
	// captureFooterSnapshot, leaving this path unproven.
	lines := render(120)
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("renderer returned %d line(s) %q, want exactly one non-empty line", len(lines), lines)
	}
	h.renders++
	return nil
}

// TestFooterProductionPathRendersOutsideTheLock is the round-3 must-fix-3
// regression for the footer. It drives installFooter / refreshFooter /
// uninstallFooter through state.setFooterRendererFn -- the only seam the
// production path uses -- with a stub that renders inside the call the way the
// SDK does. Any of the three calling the hook while holding instanceState.mu
// fails here: the stub asserts the lock is free on entry (TryLock) and then
// renders for real, so a hook made under the lock is reported immediately
// instead of just deadlocking, and a renderer that panics before reaching
// footerRender is no longer swallowed (round-4 must-fix-2: the stub used to
// recover() the host-read panic a zero sdk.Context raised in installFooter's
// closure, which left the "render runs outside the lock" claim unproven).
func TestFooterProductionPathRendersOutsideTheLock(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "production-path"
	entries := []any{assistantEntry(usageMap("input", 500, "cacheRead", 500, "cacheWrite", 0))}
	footerEntries(state, func() ([]any, bool) { return entries, true })
	host := &sdkStubHost{}
	state.setFooterRendererFn = func(_ sdk.Context, render func(width int) []string) error {
		return host.setFooterRenderer(state, t, render)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		installFooter(sdk.Context{}, state)
		if host.installs != 1 || host.renders != 1 {
			t.Errorf("installFooter installs = %d, renders = %d; want 1, 1", host.installs, host.renders)
		}
		state.mu.Lock()
		installed := state.footerInstalled
		refresh := state.footerRefresh
		state.mu.Unlock()
		if !installed || refresh == nil {
			t.Errorf("installFooter left footerInstalled = %v, refresh = %v", installed, refresh != nil)
		}

		refreshFooter(state, true)
		if host.installs != 2 {
			t.Errorf("refreshFooter installs = %d, want 2 (a re-push)", host.installs)
		}

		uninstallFooter(sdk.Context{}, state)
		if host.nils != 1 {
			t.Errorf("uninstallFooter cleared the renderer %d times, want 1", host.nils)
		}
		state.mu.Lock()
		stillInstalled := state.footerInstalled
		staleRefresh := state.footerRefresh
		state.mu.Unlock()
		if stillInstalled || staleRefresh != nil {
			t.Errorf("uninstallFooter left footerInstalled = %v, refresh = %v", stillInstalled, staleRefresh != nil)
		}
		if host.renders != 2 {
			t.Errorf("the stub rendered %d times, want 2 (install + refresh)", host.renders)
		}
	}()

	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("installFooter/refreshFooter/uninstallFooter deadlocked: SetFooterRenderer ran while instanceState.mu was held")
	}
}

// TestFooterProductionPathRefreshReentrancyRepeats hammers the same production
// path while lifecycle writes run, so a lock-order regression shows up as a
// deadlock under -race as well as a wrong cache.
func TestFooterProductionPathRefreshReentrancyRepeats(t *testing.T) {
	state := createInstanceState(true)
	state.footerModelName = "reentrant"
	host := &sdkStubHost{}
	state.setFooterRendererFn = func(_ sdk.Context, render func(width int) []string) error {
		return host.setFooterRenderer(state, t, render)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		installFooter(sdk.Context{}, state)
		for i := 0; i < 50; i++ {
			footerEntries(state, func() ([]any, bool) {
				return []any{assistantEntry(usageMap("input", float64(i+1), "cacheRead", 1, "cacheWrite", 0))}, true
			})
			refreshFooter(state, i%2 == 0)
			recordTurnUsage(state, assistantMessage(usageMap("input", 1, "cacheRead", 1)))
			if i%10 == 0 {
				resetRunState(state)
			}
		}
		uninstallFooter(sdk.Context{}, state)
	}()

	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("the footer production path deadlocked under repeated refresh")
	}
}

// TestScopeIdentityReadsLiveEndpointThroughProductionPath is the round-3
// must-fix-3 regression for the endpoint lookup. The previous round only called
// the pure modelEndpointsFrom; this one drives the production observation path
// (observePrefixDiagnostics over the resolver the extension actually builds from
// ctx.ModelRegistry()), so a memoizing wrapper reintroduced around it is caught.
func TestScopeIdentityReadsLiveEndpointThroughProductionPath(t *testing.T) {
	registry := &fakeModelRegistry{}
	resolver := buildModelEndpointResolver(registry.GetAll)
	identity := func() scopeIdentity {
		api, baseURL := resolver("audit", "offline")
		return scopeIdentity{
			provider: strPtr("audit"),
			id:       strPtr("offline"),
			api:      api,
			baseURL:  baseURL,
			session:  strPtr("test-session"),
		}
	}
	payload := func(system string) map[string]any { return chatPayload(system, nil, "hi") }

	// 1. Empty registry: the scope is incomplete, so the observation skips.
	state := createInstanceState(true)
	observePrefixDiagnostics(state, payload("V1"), identity())
	requireContains(t, joinedPrefix(state), "Status: skipped")

	// 2. The registry populates: the very next observation is a real baseline.
	registry.models = []map[string]any{{"provider": "audit", "id": "offline", "api": "openai-completions", "baseUrl": "https://a.invalid/v1"}}
	observePrefixDiagnostics(state, payload("V1"), identity())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observePrefixDiagnostics(state, payload("V1"), identity())
	requireContains(t, joinedPrefix(state), "Status: stable")

	// 3. The same model moves to another endpoint: the new scope must not
	// inherit the old endpoint's comparison state (it re-baselines instead of
	// reporting "stable" against a different endpoint).
	registry.models = []map[string]any{{"provider": "audit", "id": "offline", "api": "anthropic-messages", "baseUrl": "https://b.invalid/v1"}}
	observePrefixDiagnostics(state, payload("V1"), identity())
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// 4. And the api change alone must move the scope too.
	registry.models = []map[string]any{{"provider": "audit", "id": "offline", "api": "openai-responses", "baseUrl": "https://b.invalid/v1"}}
	observePrefixDiagnostics(state, payload("V1"), identity())
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// One registry read per observation, never a memo: five observations, five
	// reads (skipped, baseline, stable, endpoint change, api change).
	requireInt(t, "registry reads", registry.reads, 5)
}

// TestModelEndpointResolverNeverCaches pins the resolver the extension builds
// from ModelRegistry().GetAll(): one registry read per lookup, and neither an
// empty nor an errored read is remembered.
func TestModelEndpointResolverNeverCaches(t *testing.T) {
	registry := &fakeModelRegistry{}
	resolver := buildModelEndpointResolver(registry.GetAll)

	for i := 0; i < 3; i++ {
		api, baseURL := resolver("p", "m")
		if api != nil || baseURL != nil {
			t.Fatalf("read %d: an empty registry must answer unanswered, got api=%v baseUrl=%v", i, api, baseURL)
		}
	}
	requireInt(t, "registry reads while empty", registry.reads, 3)

	registry.models = []map[string]any{{"provider": "p", "id": "m", "api": "openai-completions", "baseUrl": "https://a.invalid"}}
	api, baseURL := resolver("p", "m")
	if api == nil || *api != "openai-completions" || baseURL == nil || *baseURL != "https://a.invalid" {
		t.Fatalf("api = %v, baseUrl = %v", api, baseURL)
	}

	registry.err = errors.New("host transport down")
	api, baseURL = resolver("p", "m")
	if api != nil || baseURL != nil {
		t.Fatalf("a registry error must answer unanswered, got api=%v baseUrl=%v", api, baseURL)
	}
	registry.err = nil
	api, baseURL = resolver("p", "m")
	if api == nil || baseURL == nil {
		t.Fatal("the failed read must not stick: the next read must retry")
	}
	requireInt(t, "registry reads total", registry.reads, 6)
}

// fakeModelRegistry stands in for ctx.ModelRegistry(): a registry whose contents
// (and failure mode) the test moves between observations.
type fakeModelRegistry struct {
	models []map[string]any
	err    error
	reads  int
}

func (r *fakeModelRegistry) GetAll() ([]map[string]any, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	return r.models, nil
}
