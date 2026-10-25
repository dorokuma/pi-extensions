// Package cachestats ports /root/.pi/agent/extensions/cache-stats.ts to PiG's
// Go extension SDK.
//
// The extension observes prompt caching: it keeps per-run and all-session
// cache hit rates, renders an optional TUI footer (herdsman status, dual
// hit-rate diamonds, context occupancy, tokens/s, model), warns once per model
// that answered 400, and exposes /cache-stats with enable / disable / reset /
// prefix subcommands. It never rewrites a system prompt or a provider payload:
// the default mode is safe observation, and by default it also compares the
// system/developer/instructions/tools prefix of each provider request against
// the previous one for the same scope.
//
// The mapping to the TypeScript source and the places where the PiG SDK has no
// equivalent host accessor are documented in README.md.
package cachestats

import (
	"math"
	"os"
	"strconv"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// agentHost is the host surface the agent_end and turn_end handlers need:
// appending a session entry, re-capturing the footer and requesting a footer
// render. It is an interface so the unit tests can drive the lifecycle without
// a live SDK connection.
type agentHost interface {
	appendEntry(customType string, data any) error
	readFooter(state *instanceState)
	refreshFooter(state *instanceState, force bool)
}

// sdkAgentHost binds agentHost to a live SDK context.
type sdkAgentHost struct{ ctx sdk.Context }

func (h sdkAgentHost) appendEntry(customType string, data any) error {
	return h.ctx.AppendEntry(customType, data)
}

func (h sdkAgentHost) readFooter(state *instanceState) {
	readFooterCtx(state, captureFooterInput(h.ctx))
}

func (h sdkAgentHost) refreshFooter(state *instanceState, force bool) {
	refreshFooter(state, force)
}

// Extension constructs and returns the cache-stats extension for PiG. The
// registered surface is the one the TS factory registers (TS L1782-2074):
// thirteen event subscriptions and the cache-stats command.
func Extension() *sdk.Extension {
	e := sdk.New(logName)

	prefixDiagOn := true
	if raw, present := os.LookupEnv(prefixDiagEnv); present {
		prefixDiagOn = isEnabled(raw)
	}
	state := createInstanceState(prefixDiagOn)
	verbose := isEnabled(os.Getenv("PI_CACHE_STATS_VERBOSE"))
	footerOn := true
	if raw, present := os.LookupEnv(piCacheStatsFooterEnv); present {
		footerOn = isEnabled(raw)
	}

	// ── 2. before_provider_request: read-only prefix snapshot ──
	e.OnEvent(sdk.EventBeforeProviderRequest, func(ctx sdk.Context, event map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		// TPS timing: clear half-recorded streaming stamps from a
		// failed/retried request, then mark the start of this request.
		applyProviderRequestTiming(state)
		if prefixDiagEnabled(state) {
			func() {
				defer func() {
					if recover() != nil {
						skipPrefixDiagnostics(state)
					}
				}()
				observePrefixDiagnostics(state, event["payload"], captureScopeIdentity(ctx))
			}()
		}
		return nil, nil
	})

	// ── 3. after_provider_response: record unclassified 400s ──
	e.OnEvent(sdk.EventAfterProviderResponse, func(ctx sdk.Context, event map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		provider, id, modelKnown := safeModelIdentity(ctx)
		if !modelKnown {
			return nil, nil
		}
		if status, ok := jsNumberOrNil(event["status"]); ok && status == 400 {
			key := provider + "/" + id
			// Check and set under the lock, notify outside it: Notify reaches the
			// host and must never run while instanceState.mu is held.
			state.mu.Lock()
			first := !state.unknown400[key]
			if first {
				state.unknown400[key] = true
			}
			state.mu.Unlock()
			if first {
				ctx.Notify(
					escapeForNotify("["+logName+"] "+key+" returned 400; reason unknown (Pi exposes status/headers only). Cache strategy unchanged."),
					"warning",
				)
			}
		}
		return nil, nil
	})

	// ── 3b. agent_start: reset the liveRun accumulator for this agent run ──
	e.OnEvent(sdk.EventAgentStart, func(_ sdk.Context, _ map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		state.mu.Lock()
		state.liveRun = emptyLiveRun()
		state.mu.Unlock()
		return nil, nil
	})

	// ── 4. agent_end: collect cache stats for this instance ──
	e.OnEvent(sdk.EventAgentEnd, func(ctx sdk.Context, event map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		applyAgentEnd(state, sdkAgentHost{ctx: ctx}, event, verbose)
		return nil, nil
	})

	// ── 4b. live footer updates during long runs ──
	e.OnEvent(sdk.EventMessageUpdate, func(_ sdk.Context, event map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		applyMessageUpdate(state, event["assistantMessageEvent"], int64(nowMillis()))
		return nil, nil
	})

	e.OnEvent(sdk.EventMessageEnd, func(_ sdk.Context, event map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		applyMessageEnd(state, event["message"], int64(nowMillis()))
		return nil, nil
	})

	e.OnEvent(sdk.EventTurnEnd, func(ctx sdk.Context, event map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		applyTurnEndEvent(state, sdkAgentHost{ctx: ctx}, event)
		return nil, nil
	})

	e.OnEvent(sdk.EventToolExecutionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		readFooterCtx(state, captureFooterInput(ctx))
		refreshFooter(state, false)
		return nil, nil
	})

	e.OnEvent(sdk.EventToolExecutionEnd, func(ctx sdk.Context, _ map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		readFooterCtx(state, captureFooterInput(ctx))
		refreshFooter(state, false)
		return nil, nil
	})

	// ── 6. session_start: reset this instance, install the footer only when
	// enabled + TUI ──
	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		resetRunState(state)
		readFooterCtx(state, captureFooterInput(ctx))
		if runtimeEnabled(state) && shouldInstallFooter(footerOn, ctx.Mode()) {
			installFooter(ctx, state)
		}
		return nil, nil
	})

	// ── 6b. footer refresh triggers ──
	e.OnEvent(sdk.EventModelSelect, func(ctx sdk.Context, _ map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		readFooterCtx(state, captureFooterInput(ctx))
		refreshFooter(state, true)
		return nil, nil
	})
	e.OnEvent(sdk.EventThinkingLevelSelect, func(ctx sdk.Context, _ map[string]any) (any, error) {
		if !runtimeEnabled(state) {
			return nil, nil
		}
		readFooterCtx(state, captureFooterInput(ctx))
		setThinking(state, safeThinkingLevel(ctx))
		refreshFooter(state, true)
		return nil, nil
	})

	// ── 7. /cache-stats command ──
	e.RegisterCommand("cache-stats", sdk.CommandOptions{
		Description: "Cache observer: enable/disable/stats/reset/prefix",
		Handler: func(ctx sdk.Context, args string) error {
			handleCommand(state, footerOn, args, ctx)
			return nil
		},
	})

	return e
}

// isEnabled is the TS isEnabled: 1/true/yes/on, case-insensitive.
func isEnabled(val string) bool {
	normalized := strings.ToLower(strings.TrimSpace(val))
	return normalized == "1" || normalized == "true" || normalized == "yes" || normalized == "on"
}

// applyAgentEnd is the agent_end handler body (TS L1851-1885).
//
// The fallback scan of event.messages (extractAndNormalizeUsage) is pure over
// the event payload and runs before the lock. The counter writes, the bounded
// report append, and the liveRun reset happen under the lock; the host calls
// (appendEntry, readFooter, refreshFooter) happen after it is released.
func applyAgentEnd(state *instanceState, host agentHost, event map[string]any, verbose bool) {
	messages, _ := event["messages"].([]any)

	state.mu.Lock()
	state.snapshot.turns++
	turn := state.snapshot.turns
	input := state.liveRun.input
	output := state.liveRun.output
	cacheRead := state.liveRun.cacheRead
	cacheWrite := state.liveRun.cacheWrite
	denom := state.liveRun.hitDenom
	state.mu.Unlock()

	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 && len(messages) > 0 {
		fallback := extractAndNormalizeUsage(messages)
		input = fallback.input
		output = fallback.output
		cacheRead = fallback.cacheRead
		cacheWrite = fallback.cacheWrite
		denom = cacheHitDenomNumbers(input, cacheRead, cacheWrite)
		// The fallback accumulates to the snapshot so the current-run
		// aggregate can read this turn's data.
		state.mu.Lock()
		state.snapshot.totalInput += input
		state.snapshot.totalOutput += output
		state.snapshot.totalCacheWrite += cacheWrite
		if denom > 0 {
			state.snapshot.totalCacheRead += cacheRead
			state.snapshot.totalHitDenom += denom
		}
		state.mu.Unlock()
	}
	hitPct := aggregateHit(cacheRead, denom)

	state.mu.Lock()
	state.turnReports = append(state.turnReports, turnReport{
		turn:       turn,
		input:      input,
		output:     output,
		cacheRead:  cacheRead,
		cacheWrite: cacheWrite,
		denom:      denom,
		hitPct:     hitPct,
	})
	if len(state.turnReports) > turnReportLimit {
		state.turnReports = state.turnReports[len(state.turnReports)-turnReportLimit:]
	}
	state.mu.Unlock()

	if cacheRead > 0 || cacheWrite > 0 || verbose {
		entry := map[string]any{
			"turn":       turn,
			"input":      input,
			"cacheRead":  cacheRead,
			"cacheWrite": cacheWrite,
			"denom":      denom,
			"hitPct":     hitPctData(hitPct),
		}
		_ = host.appendEntry("cache-stats-turn", entry)
	}
	host.readFooter(state)
	host.refreshFooter(state, true)
	state.mu.Lock()
	state.liveRun = emptyLiveRun()
	state.mu.Unlock()
}

// applyProviderRequestTiming clears half-recorded streaming stamps from a
// failed or retried request and stamps the start of this one (TS L1804-1807).
func applyProviderRequestTiming(state *instanceState) {
	stamp := int64(nowMillis())
	state.mu.Lock()
	defer state.mu.Unlock()
	clearTurnTimingLocked(state)
	state.providerRequestStartedAt = &stamp
}

// applyTurnEndEvent is the turn_end handler body (TS L1936-1955): accumulate
// the turn's usage, snapshot and consume the timing stamps, then refresh the
// footer unforced.
func applyTurnEndEvent(state *instanceState, host agentHost, event map[string]any) {
	if message, ok := event["message"]; ok && message != nil {
		recordTurnUsage(state, message)
	}
	applyTurnEnd(state, event["message"], int64(nowMillis()))
	host.readFooter(state)
	host.refreshFooter(state, false)
}

// hitPctData is the TS `hitPct: number | null` of the appended entry.
func hitPctData(hitPct *int) any {
	if hitPct == nil {
		return nil
	}
	return *hitPct
}

// handleCommand is the /cache-stats handler body (TS L2005-2052).
func handleCommand(state *instanceState, footerOn bool, args string, ctx sdk.Context) {
	raw := args
	trimmed := strings.TrimSpace(raw)
	parts := []string{""}
	if trimmed != "" {
		parts = strings.Fields(trimmed)
	}
	command := strings.ToLower(parts[0])

	switch command {
	case "disable":
		disableRuntime(state)
		uninstallFooter(ctx, state)
		ctx.Notify("["+logName+"] Disabled for this instance. Run /cache-stats enable to re-enable.", "info")
		return
	case "enable":
		enableRuntime(state)
		readFooterCtx(state, captureFooterInput(ctx))
		if shouldInstallFooter(footerOn, ctx.Mode()) {
			installFooter(ctx, state)
		}
		ctx.Notify("["+logName+"] Enabled.", "info")
		return
	case "reset":
		resetRunState(state)
		// Reset the live footer scope: only count entries appended after this.
		if entries, ok := safeEntries(ctx); ok {
			baseline := len(entries)
			state.mu.Lock()
			state.footerResetBaseline = &baseline
			state.mu.Unlock()
		}
		readFooterCtx(state, captureFooterInput(ctx))
		refreshFooter(state, true)
		ctx.Notify("["+logName+"] Current-run cache stats, unclassified 400 list, and prefix diagnostics reset.", "info")
		return
	case "prefix":
		notifyLines(ctx.Notify, prefixLines(state))
		return
	}
	notifyLines(ctx.Notify, statsLines(state, readSessionAggregate(ctx)))
}

// disableRuntime stops this instance and clears the prefix comparison, which
// is the state half of the disable subcommand (TS L2013-2018).
func disableRuntime(state *instanceState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.runtimeEnabled = false
	clearPrefixMemoryLocked(state)
}

// enableRuntime re-enables this instance (TS L2020-2022).
func enableRuntime(state *instanceState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.runtimeEnabled = true
}

// runtimeEnabled reports whether this instance is enabled.
func runtimeEnabled(state *instanceState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.runtimeEnabled
}

// prefixDiagEnabled reports whether prefix diagnostics are on. The env var is
// read once in Extension(), so this is a constant per instance, but it is read
// under the same lock as every other field for uniformity.
func prefixDiagEnabled(state *instanceState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.prefixDiagEnabled
}

// setThinking stores the last known thinking level for the footer.
func setThinking(state *instanceState, level *string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.footerThinking = level
}

// readSessionAggregate reads the all-session aggregate (the footer algorithm
// over the full entries) for the stats command. TS: readSessionAggregate
// (L2060-2081).
func readSessionAggregate(ctx sdk.Context) sessionAggregate {
	entries, ok := safeEntries(ctx)
	return sessionAggregateOf(entries, ok)
}

// sessionAggregate is the TS readSessionAggregate result.
type sessionAggregate struct {
	available bool
	aggregate *int
	read      float64
	denom     float64
}

// sessionAggregateOf scans entries with the footer algorithm.
func sessionAggregateOf(entries []any, ok bool) sessionAggregate {
	if !ok {
		return sessionAggregate{}
	}
	scan := scanLiveHitRates(entries, 0)
	var aggregate *int
	if scan.totalDenom > 0 && (scan.totalCacheRead > 0 || scan.totalCacheWrite > 0) {
		aggregate = aggregateHit(scan.totalCacheRead, scan.totalDenom)
	}
	return sessionAggregate{available: true, aggregate: aggregate, read: scan.totalCacheRead, denom: scan.totalDenom}
}

// statsLines builds the /cache-stats report. TS: showStats (L2083-2131).
//
// It reads the whole run snapshot and the report list, so it holds mu for the
// capture and builds the lines from local copies. prefixStatsLine takes the lock
// itself, so it is called outside this one.
func statsLines(state *instanceState, session sessionAggregate) []string {
	state.mu.Lock()
	snap := state.snapshot
	reports := append([]turnReport(nil), state.turnReports...)
	runtimeOn := state.runtimeEnabled
	resetBaseline := state.footerResetBaseline
	unknown := make([]string, 0, len(state.unknown400))
	for key := range state.unknown400 {
		unknown = append(unknown, key)
	}
	state.mu.Unlock()

	var agg *int
	if !(snap.totalCacheRead == 0 && snap.totalCacheWrite == 0) {
		agg = aggregateHit(snap.totalCacheRead, snap.totalHitDenom)
	}
	var tail *int
	if len(reports) >= 3 {
		last3 := reports[len(reports)-3:]
		hasCache := false
		for _, report := range last3 {
			if report.cacheRead > 0 || report.cacheWrite > 0 {
				hasCache = true
				break
			}
		}
		if hasCache {
			tailDenom := 0.0
			tailRead := 0.0
			for _, report := range last3 {
				if report.denom > 0 {
					tailDenom += report.denom
					tailRead += report.cacheRead
				}
			}
			tail = aggregateHit(tailRead, tailDenom)
		}
	}

	stateLabel := "disabled"
	if runtimeOn {
		stateLabel = "enabled"
	}
	aggLabel := "n/a"
	if agg != nil {
		aggLabel = strconv.Itoa(*agg) + "%"
	}
	sessionAggLabel := "n/a"
	if session.aggregate != nil {
		sessionAggLabel = strconv.Itoa(*session.aggregate) + "%"
	}
	scopeLabel := "all session, footer scope; "
	if resetBaseline != nil {
		scopeLabel = "all session, footer currently post-reset window; "
	}

	lines := []string{
		"State: " + stateLabel,
		"Turns: " + strconv.Itoa(snap.turns),
		"Aggregate hit: " + aggLabel + "  (current run; read=" + num(snap.totalCacheRead) + " / denom=" + num(snap.totalHitDenom) + ")",
		"Session aggregate: " + sessionAggLabel + "  (" + scopeLabel + "read=" + num(session.read) + " / denom=" + num(session.denom) + ")",
		"Cumulative: input=" + num(snap.totalInput) + "  output=" + num(snap.totalOutput) + "  cacheRead=" + num(snap.totalCacheRead) + "  cacheWrite=" + num(snap.totalCacheWrite),
		prefixStatsLine(state) + " (hook snapshot; chars ≠ tokens; not a cache verdict)",
	}
	if tail != nil {
		lines = append(lines, "Tail (last 3) hit: "+strconv.Itoa(*tail)+"%")
	}
	if len(unknown) > 0 {
		sortStrings(unknown)
		lines = append(lines, "Unclassified 400 models (strategy unchanged): "+strings.Join(unknown, ", "))
	}
	if len(reports) > 0 {
		lines = append(lines, "", "Per-turn (last "+strconv.Itoa(len(reports))+", bounded):")
		sessionHadInteraction := session.aggregate != nil
		for _, report := range reports {
			turnHit := "n/a"
			if sessionHadInteraction && report.denom > 0 {
				if report.hitPct != nil {
					turnHit = strconv.Itoa(*report.hitPct) + "%"
				}
			}
			lines = append(lines, "  T"+strconv.Itoa(report.turn)+": i="+num(report.input)+" r="+num(report.cacheRead)+" w="+num(report.cacheWrite)+" "+turnHit)
		}
	}
	return lines
}

// num renders an accumulated counter the way a JavaScript template literal
// does (`${value}` -> Number::toString), so it is exactly jsNumberString.
//
// It used to print plain notation for the whole integral range, which diverged
// from JS at the Number::toString threshold: `String(1e21)` is "1e+21", not
// "1000000000000000000000". jsNumberString already keeps the shortest
// round-tripping digits for the whole range and never converts to int64, so the
// accumulator at or above 2^63 cannot overflow onto one shared string either.
func num(value float64) string {
	return jsNumberString(value)
}

// jsNumberOrNil coerces a decoded value to a number, reporting false for a
// missing or non-numeric value.
func jsNumberOrNil(value any) (float64, bool) {
	if value == nil {
		return 0, false
	}
	n := jsNumber(value)
	if math.IsNaN(n) {
		return 0, false
	}
	return n, true
}

// ── SDK context adapters ─────────────────────────────────────────────────────

// captureFooterInput reads the footer-relevant fields of ctx, tolerating a
// stale session the way readFooterCtx's try/catch does.
func captureFooterInput(ctx sdk.Context) footerReadInput {
	input := footerReadInput{
		hasSessionManager:  true,
		entries:            func() ([]any, bool) { return safeEntries(ctx) },
		hasGetContextUsage: true,
		getContextUsage:    func() (*sdk.ContextUsage, bool) { return safeContextUsage(ctx) },
	}
	if info, ok := safeModelInfo(ctx); ok {
		input.modelName = info.Name
	}
	input.thinking = safeThinkingLevel(ctx)
	if usage, ok := safeContextUsage(ctx); ok {
		input.contextUsage = usage
	}
	return input
}

// captureScopeIdentity reads the comparison scope of ctx, resolving the model
// endpoint from the current model registry on every call.
//
// It deliberately keeps no cache. TS reads `model.api` / `model.baseUrl` off the
// live model object each time; caching the result by (provider, id) breaks two
// real cases: a registry that answers empty early (the model list is populated
// lazily) would stay empty for the whole session and pin every observation to
// "skipped", and a same-named model whose baseUrl/api changes (endpoint switch,
// proxy rotation, provider reconfiguration) would keep hashing the old endpoint
// into the scope key, cross-contaminating two different endpoints' comparison
// state. A lookup that the registry cannot answer therefore yields nil scope
// fields for this observation only and is retried on the next one (fail closed,
// never fail sticky).
func captureScopeIdentity(ctx sdk.Context) scopeIdentity {
	provider, id, ok := safeModelIdentity(ctx)
	if !ok {
		return scopeIdentity{}
	}
	session, sessionOK := safeSessionID(ctx)
	identity := scopeIdentity{
		provider: &provider,
		id:       &id,
	}
	if sessionOK {
		identity.session = &session
	}
	api, baseURL := lookupModelEndpoints(ctx, provider, id)
	identity.api = api
	identity.baseURL = baseURL
	return identity
}

// modelEndpointLookup returns the api and baseUrl of the (provider, id) model in
// the registry, describing why one is missing so the caller can report it.
type modelEndpointLookup struct {
	api        *string
	baseURL    *string
	foundModel bool
	err        error
}

// lookUpModelEndpoints resolves provider/id against the live registry.
func lookUpModelEndpoints(ctx sdk.Context, provider, id string) modelEndpointLookup {
	defer func() {
		// A stale session makes the registry call panic; treat it as unanswered.
		_ = recover()
	}()
	api, baseURL, found, err := modelEndpointsFrom(func() ([]map[string]any, error) {
		return ctx.ModelRegistry().GetAll()
	}, provider, id)
	return modelEndpointLookup{api: api, baseURL: baseURL, foundModel: found, err: err}
}

// modelEndpointSource is the model registry read the endpoint lookup performs:
// ctx.ModelRegistry().GetAll() in production, a plain function in the tests.
// It is a function type rather than an interface so the "never cache" rule is
// testable without a live SDK context (sdk.Context is an opaque handle the unit
// tests cannot construct).
type modelEndpointSource func() ([]map[string]any, error)

// modelEndpointResolver is the packaged lookup the observation path uses: it
// closes over one modelEndpointSource and answers the (provider, id) model's api
// and baseUrl on every call, or nils when the registry cannot answer.
type modelEndpointResolver func(provider, id string) (api, baseURL *string)

// buildModelEndpointResolver binds a registry source into the resolver the
// extension observes with. captureScopeIdentity builds it from
// ctx.ModelRegistry().GetAll() on every observation, so the production wrapper
// and the tests share this one code path (the tests pass a registry stand-in and
// move its contents between observations).
//
// It deliberately keeps no cache and no memo of failures: see
// captureScopeIdentity for the two production cases that made caching wrong.
func buildModelEndpointResolver(source modelEndpointSource) modelEndpointResolver {
	return func(provider, id string) (api, baseURL *string) {
		defer func() {
			// A stale session makes the registry call panic; treat it as an
			// unanswered lookup for this observation only.
			if recover() != nil {
				api, baseURL = nil, nil
			}
		}()
		api, baseURL, _, _ = modelEndpointsFrom(source, provider, id)
		return api, baseURL
	}
}

// modelEndpointsFrom resolves the (provider, id) model's api and baseUrl from
// one fresh read of the registry. It reports whether the model was present, so
// a caller can distinguish "unknown model" from "model without endpoints".
func modelEndpointsFrom(source modelEndpointSource, provider, id string) (api, baseURL *string, found bool, err error) {
	models, getAllErr := source()
	if getAllErr != nil {
		return nil, nil, false, getAllErr
	}
	for _, model := range models {
		modelProvider, _ := model["provider"].(string)
		modelID, _ := model["id"].(string)
		if modelProvider != provider || modelID != id {
			continue
		}
		if value, ok := model["api"].(string); ok && value != "" {
			api = &value
		}
		if value, ok := model["baseUrl"].(string); ok && value != "" {
			baseURL = &value
		}
		return api, baseURL, true, nil
	}
	return nil, nil, false, nil
}

// lookupModelEndpoints reads the model's api and baseUrl from the host model
// registry. The SDK's GetModelInfo exposes neither, so the registry is the
// only source; both stay nil when the registry cannot answer.
//
// Every call re-reads the registry: the result must never be memoized, because
// both a not-yet-populated registry and a changed endpoint are legitimate
// consecutive answers (see captureScopeIdentity).
func lookupModelEndpoints(ctx sdk.Context, provider, id string) (api, baseURL *string) {
	defer func() {
		if recover() != nil {
			api, baseURL = nil, nil
		}
	}()
	return buildModelEndpointResolver(func() ([]map[string]any, error) {
		return ctx.ModelRegistry().GetAll()
	})(provider, id)
}

// safeEntries reads the session entries as []any, reporting false when the
// host cannot answer (the TS Array.isArray guard's false branch).
func safeEntries(ctx sdk.Context) (entries []any, ok bool) {
	defer func() {
		if recover() != nil {
			entries, ok = nil, false
		}
	}()
	list, err := ctx.SessionManager().GetEntries()
	if err != nil || list == nil {
		return nil, false
	}
	out := make([]any, 0, len(list))
	for _, entry := range list {
		out = append(out, entry)
	}
	return out, true
}

// safeContextUsage reads the context usage, reporting false when the host
// cannot answer or has no usable value.
func safeContextUsage(ctx sdk.Context) (usage *sdk.ContextUsage, ok bool) {
	defer func() {
		if recover() != nil {
			usage, ok = nil, false
		}
	}()
	value, err := ctx.GetContextUsage()
	if err != nil || value == nil {
		return nil, false
	}
	return value, true
}

// safeThinkingLevel reads ctx.thinkingLevel.
func safeThinkingLevel(ctx sdk.Context) (level *string) {
	defer func() {
		if recover() != nil {
			level = nil
		}
	}()
	value, err := ctx.GetThinkingLevel()
	if err != nil {
		return nil
	}
	return &value
}

// safeModelInfo reads the active model metadata.
func safeModelInfo(ctx sdk.Context) (info *sdk.ModelInfo, ok bool) {
	defer func() {
		if recover() != nil {
			info, ok = nil, false
		}
	}()
	value, err := ctx.GetModelInfo()
	if err != nil || value == nil {
		return nil, false
	}
	return value, true
}

// safeModelIdentity reads the model provider and id the way modelKey does,
// reporting false when the session has no model.
func safeModelIdentity(ctx sdk.Context) (provider, id string, ok bool) {
	defer func() {
		if recover() != nil {
			provider, id, ok = "", "", false
		}
	}()
	provider = ctx.ModelProvider()
	id = ctx.Model()
	if provider == "" && id == "" {
		return "", "", false
	}
	return provider, id, true
}

// safeSessionID reads the session identity for the comparison scope.
func safeSessionID(ctx sdk.Context) (id string, ok bool) {
	defer func() {
		if recover() != nil {
			id, ok = "", false
		}
	}()
	value, err := ctx.SessionManager().GetSessionID()
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

// footerRendererHook is the production SetFooterRenderer binding and the one
// seam the tests inject through: installFooter, uninstallFooter and the
// footerRefresh closure all reach the SDK only via this function, so a test that
// swaps state.setFooterRendererFn drives the real production path (a stub that
// renders synchronously inside the call, as the SDK does) instead of only the
// renderer body.
//
// It reads the field under the lock and calls the hook after releasing it: the
// hook may render, which takes the lock.
func footerRendererHook(state *instanceState, ctx sdk.Context, render func(width int) []string) error {
	state.mu.Lock()
	hook := state.setFooterRendererFn
	state.mu.Unlock()
	if hook == nil {
		return ctx.SetFooterRenderer(render)
	}
	return hook(ctx, render)
}

// installFooter registers the custom footer renderer. TS: installFooter
// (L616-703). The SDK has no force flag on a re-render, so requesting a render
// re-pushes the same renderer.
//
// The renderer closure must not hold instanceState.mu when it runs, and it must
// not be installed while mu is held: SetFooterRenderer renders synchronously and
// retires the outgoing renderer by waiting on its in-flight push, which (for our
// own renderer) holds the renderer mutex and calls renderFooter, which takes mu.
// Taking mu here and rendering there would deadlock the two against each other.
// So the state is only written under mu, and the renderer hook is called after
// it is released -- including from the refresh closure below, which is why
// refreshFooter itself never calls it under mu either.
func installFooter(ctx sdk.Context, state *instanceState) {
	render := func(width int) []string {
		theme, statuses := footerUIInputs(ctx)
		return []string{footerRender(state, theme, statuses, width)}
	}
	refresh := func(bool) { _ = footerRendererHook(state, ctx, render) }
	state.mu.Lock()
	state.footerRefresh = refresh
	state.footerInstalled = true
	state.mu.Unlock()
	_ = footerRendererHook(state, ctx, render)
}

// footerUIInputs reads the two host-provided inputs the renderer needs: the
// active theme (ctx.UITheme()) and the extension statuses
// (ctx.FooterData().ExtensionStatuses()). Both reach into the SDK host handle,
// so on a zero sdk.Context they panic on the nil host instead of answering; the
// renderer then falls back to unstyled text and no statuses, which is exactly
// what a host without a theme renders. Keeping the read here -- rather than
// inline in the closure -- means the production renderer always reaches
// footerRender, and therefore always takes instanceState.mu through
// captureFooterSnapshot, no matter how small the ctx it was handed.
func footerUIInputs(ctx sdk.Context) (theme themeStyler, statuses map[string]string) {
	return uiTheme{theme: safeUITheme(ctx)}, safeExtensionStatuses(ctx)
}

// safeUITheme reads ctx.UITheme(), yielding the zero sdk.UITheme when the host
// cannot answer (a zero sdk.Context has no host to read the palette from).
func safeUITheme(ctx sdk.Context) (value sdk.UITheme) {
	defer func() {
		if recover() != nil {
			value = sdk.UITheme{}
		}
	}()
	return ctx.UITheme()
}

// safeExtensionStatuses reads ctx.FooterData().ExtensionStatuses(), yielding no
// statuses when the host cannot answer.
func safeExtensionStatuses(ctx sdk.Context) (statuses map[string]string) {
	defer func() {
		if recover() != nil {
			statuses = nil
		}
	}()
	return ctx.FooterData().ExtensionStatuses()
}

// uninstallFooter restores the built-in footer and clears the captured footer
// state. TS: uninstallFooter (L874-883). SetFooterRenderer(nil) retires our
// renderer (waiting on any in-flight push), so it runs before mu is taken;
// meanwhile the renderer keeps working on whatever state was last snapshotted.
func uninstallFooter(ctx sdk.Context, state *instanceState) {
	_ = footerRendererHook(state, ctx, nil)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.footerRefresh = nil
	state.footerSources = nil
	state.footerContext = nil
	state.footerHitCache = nil
	clearTurnTimingLocked(state)
	state.lastTurnTps = nil
	state.footerInstalled = false
}
