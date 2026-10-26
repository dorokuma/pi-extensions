package pivcc

import (
	"os"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Ports the host-facing half of src/hooks/before-compact.ts
// registerBeforeCompactHook().

// debugSnapshotPath is before-compact.ts's dbg target.
const debugSnapshotPath = "/tmp/pi-vcc-debug.json"

// RegisterBeforeCompactHook is before-compact.ts registerBeforeCompactHook().
// Host-level failures are reported through the event result rather than
// crashing the extension: the TS code lets its handlers throw, and pi turns a
// throwing session_before_compact into a failed compaction.
func RegisterBeforeCompactHook(ext *sdk.Extension) {
	state := stateFor(ext)

	// Filter the invisible-continue marker out of the LLM context payload so the
	// model just continues from the compaction summary (matched by customType
	// ONLY).
	ext.OnEvent(sdk.EventContext, func(_ sdk.Context, data map[string]any) (any, error) {
		rawMessages, _ := data["messages"].([]any)
		messages := make([]any, 0, len(rawMessages))
		changed := false
		for _, raw := range rawMessages {
			message, ok := raw.(map[string]any)
			if !ok {
				messages = append(messages, raw)
				continue
			}
			if messageRole(message) != "custom" {
				messages = append(messages, raw)
				continue
			}
			customType, _ := message["customType"].(string)
			if customType == AutoContinueCustomType {
				changed = true
				continue
			}
			messages = append(messages, raw)
		}
		if !changed {
			return nil, nil
		}
		return map[string]any{"messages": messages}, nil
	})

	ext.OnEvent(sdk.EventBeforeAgentStart, func(_ sdk.Context, _ map[string]any) (any, error) {
		state.clearPendingAutoContinue()
		return nil, nil
	})

	ext.OnEvent(sdk.EventSessionBeforeCompact, func(ctx sdk.Context, data map[string]any) (any, error) {
		return handleSessionBeforeCompact(ext, state, ctx, data), nil
	})

	// Fire the success toast for the /compact path only (delayed to let the UI
	// settle). /pi-vcc handles its own toast through onComplete.
	ext.OnEvent(sdk.EventSessionCompact, func(ctx sdk.Context, data map[string]any) (any, error) {
		reason, willRetry := ReadCompactionEventContext(data)
		fromExtension, _ := data["fromExtension"].(bool)
		if !fromExtension {
			return nil, nil
		}
		// The runner keeps the LAST non-null session_before_compact result, so
		// another compaction extension can win even when pi-vcc returned one.
		// Read the truth from the persisted entry instead of lifecycle state.
		if !compactionEntryIsPiVcc(data) {
			return nil, nil
		}
		followUp, hasFollowUp := state.takePendingFollowUp()
		if state.getLastCompactWasPiVcc() {
			return nil, nil // /pi-vcc handles its own toast via onComplete
		}
		if willRetry {
			return nil, nil
		}
		stats, ok := state.lastStatsRead()
		if !ok {
			return nil, nil
		}
		settings := LoadSettingsDefault()
		shouldContinue := (reason == ReasonThreshold || reason == ReasonOverflow) &&
			ShouldScheduleAutoContinue(settings.ContinueAfterThresholdCompact, PiVersion())
		notifyLater(ctx, FormatCompactionStats(stats), "info")
		if hasFollowUp && followUp != "" {
			_ = ctx.SendUserMessage(followUp, "")
		} else if shouldContinue {
			state.scheduleAutoContinue(ext, ctx)
		}
		return nil, nil
	})
}

// compactionEntryIsPiVcc is the TS
// `event.compactionEntry?.details?.compactor !== "pi-vcc"` guard, inverted.
func compactionEntryIsPiVcc(data map[string]any) bool {
	entry, ok := data["compactionEntry"].(map[string]any)
	if !ok {
		return false
	}
	details, ok := entry["details"].(map[string]any)
	if !ok {
		return false
	}
	compactor, _ := details["compactor"].(string)
	return compactor == CompactionDetailsCompactor
}

// handleSessionBeforeCompact is the body of the TS session_before_compact
// handler: pick the cut, build the summary, and either return it or cancel.
func handleSessionBeforeCompact(ext *sdk.Extension, state *extensionState, ctx sdk.Context, data map[string]any) any {
	reason, willRetry := ReadCompactionEventContext(data)
	settings := LoadSettingsDefault()

	customInstructions, _ := data["customInstructions"].(string)
	parsed := ParseCompactionInstructions(customInstructions)
	state.setPendingFollowUp("")
	if !parsed.IsPiVcc && !settings.OverrideDefaultCompaction {
		return nil
	}
	// Provider-level opt-out (#27): checked at call time because the model can
	// change mid-session. An explicit /pi-vcc bypasses the skip.
	provider := modelProviderOf(ctx)
	if !parsed.IsPiVcc && provider != "" && providerSkipped(provider, settings.SkipForProviders) {
		return nil
	}

	branchEntries := branchEntriesOf(data)
	preparation, _ := data["preparation"].(map[string]any)

	// Calibrate chars/token from the whole window plus the previous summary.
	calibrationCut := BuildOwnCut(branchEntries, 0, nil)
	calibrationMessageChars := 0
	if calibrationCut.OK {
		for _, message := range calibrationCut.Messages {
			calibrationMessageChars += EstimateMessageContentChars(message["content"])
		}
	}
	previousSummary, _ := preparation["previousSummary"].(string)
	calibrationSummaryChars := jsLen(previousSummary)
	tokensBefore, hasTokensBefore := numberField(preparation, "tokensBefore")
	calibration := CalibrateCharsPerToken(
		float64(calibrationMessageChars+calibrationSummaryChars),
		tokensBefore,
		hasTokensBefore,
	)

	// Smart keep-tail: boost the default keep when the tail is small. Explicit
	// keep:N from the user is always respected (the resolver no-ops).
	requested := -1
	if parsed.KeepUserTurnsExplicit {
		requested = parsed.KeepUserTurns
	}
	smartKeep := ResolveSmartKeepUserTurns(ResolveSmartKeepOptions{
		BranchEntries:          branchEntries,
		RequestedKeepUserTurns: requested,
		Explicit:               parsed.KeepUserTurnsExplicit,
		SmartKeepTail:          settings.SmartKeepTail,
		CharsPerToken:          calibration.CharsPerToken,
	})
	ownCut := BuildOwnCut(branchEntries, smartKeep.KeepUserTurns, settings.SkipCustomTypes)
	// Default path only: rescue autonomous / oversized-tail sessions with a
	// token-budget cut. An explicit keep:N is respected absolutely.
	if ownCut.OK && !parsed.KeepUserTurnsExplicit {
		ownCut = ApplyTailBudget(branchEntries, ownCut, ApplyTailBudgetOptions{
			CharsPerToken:   calibration.CharsPerToken,
			SkipCustomTypes: settings.SkipCustomTypes,
		})
	}

	if !ownCut.OK {
		fallbackToCore := !parsed.IsPiVcc && (reason == ReasonOverflow || willRetry)
		WriteDebugSnapshot(debugSnapshotPath, settings.Debug, cancelledDebugSnapshot(
			branchEntries, ownCut, reason, willRetry, parsed.IsPiVcc, fallbackToCore,
		))
		if fallbackToCore {
			return nil
		}
		if ctx.HasUI() {
			ctx.Notify(ReasonMessage(ownCut.Reason), "warning")
		}
		return map[string]any{"cancel": true}
	}

	state.setPendingFollowUp(parsed.FollowUpPrompt)

	// Filter user-declared customTypes right before the summarizer input. Cut
	// selection, calibration, firstKeptEntryId and kept-user-turn counting are
	// already fixed above — this only changes what the summarizer sees.
	agentMessages := ownCut.Messages
	agentSelectedIDs := ownCut.SelectedIDs
	if len(settings.SkipCustomTypes) > 0 {
		filteredMessages := make([]map[string]any, 0, len(agentMessages))
		filteredIDs := make([]string, 0, len(agentSelectedIDs))
		for i, message := range agentMessages {
			if isSkippedCustom(message, settings.SkipCustomTypes) {
				continue
			}
			filteredMessages = append(filteredMessages, message)
			filteredIDs = append(filteredIDs, agentSelectedIDs[i])
		}
		agentMessages = filteredMessages
		agentSelectedIDs = filteredIDs
	}
	firstKeptEntryID := ownCut.FirstKeptEntryID

	// Session-global indices for summary refs (issue #28).
	globalIndexByID := globalIndexFor(ctx)
	convertedMessages := make([]map[string]any, 0, len(agentMessages))
	sourceIndices := make([]int, 0, len(agentMessages))
	for i, message := range agentMessages {
		converted := toLlmSafe(message)
		if len(converted) == 0 {
			continue
		}
		convertedMessages = append(convertedMessages, summarizerInputMessage(message, converted[0]))
		sourceIndices = append(sourceIndices, lookupGlobalIndex(globalIndexByID, agentSelectedIDs[i]))
	}

	// Count kept messages and estimate tokens.
	keptEntries := keptMessageEntries(branchEntries, firstKeptEntryID)
	keptChars := 0
	for _, entry := range keptEntries {
		keptChars += EstimateMessageContentChars(entry["content"])
	}
	stats := CompactionStats{
		Summarized:               len(agentMessages),
		Kept:                     len(keptEntries),
		KeptUserTurns:            ownCut.KeptUserTurns,
		TotalUserTurns:           ownCut.TotalUserTurns,
		RequestedKeepUserTurns:   ownCut.RequestedKeepUserTurns,
		KeepUserTurnsExplicit:    parsed.KeepUserTurnsExplicit,
		KeepFallbackToCompactAll: ownCut.KeepFallbackToCompactAll,
		KeptTokensEst:            EstimateTokensFromChars(float64(keptChars), calibration.CharsPerToken),
		SmartKeepAdjusted:        smartKeep.SmartAdjusted,
		SmartFromKeep:            smartKeep.FromKeep,
		BudgetCut:                string(ownCut.BudgetCut),
		Reason:                   reason,
		WillRetry:                willRetry,
	}
	state.setLastStats(stats)

	fileOps := &FileOps{
		ReadFiles:     stringSliceField(preparation, "fileOps", "read"),
		ModifiedFiles: append(stringSliceField(preparation, "fileOps", "written"), stringSliceField(preparation, "fileOps", "edited")...),
	}
	summary := CompileRanked(RankedCompileInput{
		CompileInput: CompileInput{
			Messages:           convertedMessages,
			PreviousSummary:    previousSummary,
			HasPreviousSummary: previousSummary != "",
			FileOps:            fileOps,
			SourceIndices:      sourceIndices,
			TrackCommands:      settings.TrackCommands,
		},
		Ranking: &BriefRankingOptions{
			MaxBriefChars:        int(jsRound(float64(RankedBriefBudgetTokens) * calibration.CharsPerToken)),
			MaxBriefCharsCeiling: int(jsRound(float64(RankedBriefCeilingTokens) * calibration.CharsPerToken)),
			BriefCharsPerBlock:   int(jsRound(float64(RankedBriefTokensPerBlock) * calibration.CharsPerToken)),
		},
	})

	WriteDebugSnapshot(debugSnapshotPath, settings.Debug, compactionDebugSnapshot(
		ownCut, reason, willRetry, agentMessages, convertedMessages, firstKeptEntryID,
		branchEntries, tokensBefore, hasTokensBefore, calibration, summary,
	))

	details := PiVccCompactionDetails{
		Compactor:           CompactionDetailsCompactor,
		Version:             CompactionDetailsVersion,
		Sections:            SectionHeaders(summary),
		SourceMessageCount:  len(agentMessages),
		PreviousSummaryUsed: previousSummary != "",
		Reason:              reason,
		WillRetry:           willRetry,
	}
	state.setLastCompactWasPiVcc(parsed.IsPiVcc)

	return map[string]any{
		"compaction": map[string]any{
			"summary":          summary,
			"details":          details,
			"tokensBefore":     tokensBefore,
			"firstKeptEntryId": firstKeptEntryID,
		},
	}
}

// PiVersion is the running PiG version used for the auto-continue gate. PiG
// reports its own version, which is far past Pi's 0.84.4 self-resume version,
// so the fallback continue stays off unless a host explicitly asks for it.
//
// A host that wants the TS default can set PI_VCC_PI_VERSION; an unreadable
// value fails safe to "no continue", as the TS code does.
func PiVersion() string {
	if value := strings.TrimSpace(os.Getenv("PI_VCC_PI_VERSION")); value != "" {
		return value
	}
	return "1.0.0"
}

// modelProviderOf is `ctx.model.provider` with a nil-safety guard.
func modelProviderOf(ctx sdk.Context) (provider string) {
	defer func() {
		if recover() != nil {
			provider = ""
		}
	}()
	return ctx.ModelProvider()
}

// providerSkipped is `settings.skipForProviders.some(p => p.toLowerCase() === provider.toLowerCase())`.
func providerSkipped(provider string, skipForProviders []string) bool {
	lower := strings.ToLower(provider)
	for _, skipped := range skipForProviders {
		if strings.ToLower(skipped) == lower {
			return true
		}
	}
	return false
}

// branchEntriesOf decodes the event's branchEntries into the object form the
// pure port reads.
func branchEntriesOf(data map[string]any) []map[string]any {
	rawList, _ := data["branchEntries"].([]any)
	out := make([]map[string]any, 0, len(rawList))
	for _, raw := range rawList {
		if entry, ok := raw.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

// numberField reads a present numeric field.
func numberField(object map[string]any, key string) (float64, bool) {
	switch value := object[key].(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	}
	return 0, false
}

// stringSliceField reads a `string[]` at object[parent][child].
func stringSliceField(object map[string]any, parent, child string) []string {
	nested, ok := object[parent].(map[string]any)
	if !ok {
		return nil
	}
	rawList, ok := nested[child].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		if text, ok := raw.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// globalIndexFor is the `#N` map: the in-memory tree first, then the session
// file, else nil (fail-closed: no refs rather than window-relative ones).
func globalIndexFor(ctx sdk.Context) map[string]int {
	func() {
		defer func() { _ = recover() }()
	}()
	if entries, err := ctx.GetEntries(); err == nil && len(entries) > 0 {
		decoded := make([]map[string]any, 0, len(entries))
		for _, raw := range entries {
			if entry, ok := decodeJSONObject(raw); ok {
				decoded = append(decoded, entry)
			}
		}
		if len(decoded) > 0 {
			return BuildGlobalIndexByID(decoded)
		}
	}
	if sessionFile, err := ctx.GetSessionFile(); err == nil && sessionFile != nil && *sessionFile != "" {
		return LoadGlobalIndexByID(*sessionFile)
	}
	return nil
}

// lookupGlobalIndex is `globalIndexById?.get(id)`: -1 is TS's `undefined`.
func lookupGlobalIndex(index map[string]int, id string) int {
	if index == nil {
		return -1
	}
	if position, ok := index[id]; ok {
		return position
	}
	return -1
}

// keptMessageEntries is `branchEntries.slice(keptIdx).filter(e => e.type === "message")`.
func keptMessageEntries(branchEntries []map[string]any, firstKeptEntryID string) []map[string]any {
	keptIdx := -1
	for i, entry := range branchEntries {
		if entryField(entry, "id") == firstKeptEntryID {
			keptIdx = i
			break
		}
	}
	out := make([]map[string]any, 0, 8)
	if keptIdx < 0 {
		return out
	}
	for _, entry := range branchEntries[keptIdx:] {
		if entryField(entry, "type") != "message" {
			continue
		}
		message, ok := entry["message"].(map[string]any)
		if !ok {
			continue
		}
		out = append(out, message)
	}
	return out
}

// cancelledDebugSnapshot is the dbg payload of a cancelled compaction.
func cancelledDebugSnapshot(branchEntries []map[string]any, cut OwnCutResult, reason CompactionReason, willRetry, isPiVcc, fallbackToCore bool) map[string]any {
	live := CollectLiveMessages(branchEntries)
	roles := make([]string, 0, len(live))
	for _, item := range live {
		roles = append(roles, messageRole(item.Message))
	}
	userIndices := make([]int, 0, len(live))
	for i, role := range roles {
		if role == "user" {
			userIndices = append(userIndices, i)
		}
	}
	lastCompactionIdx := -1
	var lastCompaction map[string]any
	for i := len(branchEntries) - 1; i >= 0; i-- {
		if entryField(branchEntries[i], "type") == "compaction" {
			lastCompactionIdx = i
			lastCompaction = branchEntries[i]
			break
		}
	}
	var lastCompactionInfo any
	if lastCompaction != nil {
		firstKept := entryField(lastCompaction, "firstKeptEntryId")
		found := false
		if firstKept != "" {
			for _, entry := range branchEntries {
				if entryField(entry, "id") == firstKept {
					found = true
					break
				}
			}
		}
		lastCompactionInfo = map[string]any{
			"hasFirstKeptEntryId": firstKept != "",
			"foundInBranch":       found,
		}
	}
	roleSequence := roles
	if len(roles) > 30 {
		seq := make([]any, 0, 21)
		for _, role := range roles[:10] {
			seq = append(seq, role)
		}
		seq = append(seq, "...")
		for _, role := range roles[len(roles)-10:] {
			seq = append(seq, role)
		}
		roleSequence = nil
		return map[string]any{
			"cancelled":      !fallbackToCore,
			"fallbackToCore": fallbackToCore,
			"reason":         string(cut.Reason),
			"compaction":     map[string]any{"reason": string(reason), "willRetry": willRetry},
			"isPiVcc":        isPiVcc,
			"counts":         entryCounts(branchEntries, lastCompactionIdx),
			"liveMessages": map[string]any{
				"count":        len(roles),
				"userCount":    len(userIndices),
				"firstUserIdx": firstOrNil(userIndices),
				"lastUserIdx":  lastOrNil(userIndices),
				"roleSequence": seq,
			},
			"lastCompaction": lastCompactionInfo,
			"tail":           tailEntries(branchEntries, 5),
		}
	}
	return map[string]any{
		"cancelled":      !fallbackToCore,
		"fallbackToCore": fallbackToCore,
		"reason":         string(cut.Reason),
		"compaction":     map[string]any{"reason": string(reason), "willRetry": willRetry},
		"isPiVcc":        isPiVcc,
		"counts":         entryCounts(branchEntries, lastCompactionIdx),
		"liveMessages": map[string]any{
			"count":        len(roles),
			"userCount":    len(userIndices),
			"firstUserIdx": firstOrNil(userIndices),
			"lastUserIdx":  lastOrNil(userIndices),
			"roleSequence": roleSequence,
		},
		"lastCompaction": lastCompactionInfo,
		"tail":           tailEntries(branchEntries, 5),
	}
}

func firstOrNil(values []int) any {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func lastOrNil(values []int) any {
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}

func entryCounts(branchEntries []map[string]any, lastCompactionIdx int) map[string]any {
	messages := 0
	compactions := 0
	for _, entry := range branchEntries {
		switch entryField(entry, "type") {
		case "message":
			messages++
		case "compaction":
			compactions++
		}
	}
	var after any
	if lastCompactionIdx >= 0 {
		after = len(branchEntries) - lastCompactionIdx - 1
	}
	return map[string]any{
		"total":                      len(branchEntries),
		"messages":                   messages,
		"compactions":                compactions,
		"entriesAfterLastCompaction": after,
	}
}

func tailEntries(branchEntries []map[string]any, count int) []any {
	start := len(branchEntries) - count
	if start < 0 {
		start = 0
	}
	out := make([]any, 0, count)
	for _, entry := range branchEntries[start:] {
		item := map[string]any{"type": entryField(entry, "type")}
		if entryField(entry, "type") == "message" {
			message, _ := entry["message"].(map[string]any)
			item["role"] = messageRole(message)
			if message != nil {
				item["hasContent"] = message["content"] != nil
			}
		}
		out = append(out, item)
	}
	return out
}

// compactionDebugSnapshot is the dbg payload of a successful compaction.
func compactionDebugSnapshot(cut OwnCutResult, reason CompactionReason, willRetry bool, agentMessages, convertedMessages []map[string]any, firstKeptEntryID string, branchEntries []map[string]any, tokensBefore float64, hasTokensBefore bool, calibration TokenEstimateCalibration, summary string) map[string]any {
	cutIdx := -1
	for i, entry := range branchEntries {
		if entryField(entry, "id") == firstKeptEntryID {
			cutIdx = i
			break
		}
	}
	cutWindow := make([]any, 0, 6)
	if cutIdx >= 0 {
		start := cutIdx - 3
		if start < 0 {
			start = 0
		}
		end := cutIdx + 3
		if end > len(branchEntries) {
			end = len(branchEntries)
		}
		for _, entry := range branchEntries[start:end] {
			item := map[string]any{"id": entryField(entry, "id"), "type": entryField(entry, "type")}
			if entryField(entry, "type") == "message" {
				message, _ := entry["message"].(map[string]any)
				item["role"] = messageRole(message)
				if message != nil {
					item["preview"] = previewContent(message["content"])
				}
			}
			cutWindow = append(cutWindow, item)
		}
	}
	return map[string]any{
		"usedOwnCut":          true,
		"budgetCut":           string(cut.BudgetCut),
		"compaction":          map[string]any{"reason": string(reason), "willRetry": willRetry},
		"messagesToSummarize": len(agentMessages),
		"messagesPreviewHead": previewList(agentMessages, 3, false),
		"messagesPreviewTail": previewList(agentMessages, 3, true),
		"convertedMessages":   len(convertedMessages),
		"firstKeptEntryId":    firstKeptEntryID,
		"cutWindow":           cutWindow,
		"tokensBefore":        tokensBefore,
		"hasTokensBefore":     hasTokensBefore,
		"charsPerToken":       calibration.CharsPerToken,
		"tokenEstimateMode":   string(calibration.Mode),
		"summaryLength":       jsLen(summary),
		"summaryPreview":      jsSlice(summary, 0, 500),
		"sections":            SectionHeaders(summary),
	}
}

func previewList(messages []map[string]any, count int, fromTail bool) []any {
	items := messages
	if len(items) > count {
		if fromTail {
			items = items[len(items)-count:]
		} else {
			items = items[:count]
		}
	}
	out := make([]any, 0, len(items))
	for _, message := range items {
		out = append(out, map[string]any{"role": messageRole(message), "preview": previewContent(message["content"])})
	}
	return out
}
