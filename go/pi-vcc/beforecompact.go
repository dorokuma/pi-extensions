package pivcc

import (
	"encoding/json"
	"os"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Ports the pure decision logic of src/hooks/before-compact.ts: cut selection,
// tail budgeting, smart keep-tail, auto-continue gating and the stats strings.
// The extension-facing wiring lives in extension.go.

// CompactionStats is before-compact.ts CompactionStats.
type CompactionStats struct {
	Summarized               int              `json:"summarized"`
	Kept                     int              `json:"kept"`
	KeptUserTurns            int              `json:"keptUserTurns"`
	TotalUserTurns           int              `json:"totalUserTurns"`
	RequestedKeepUserTurns   int              `json:"requestedKeepUserTurns"`
	KeepUserTurnsExplicit    bool             `json:"keepUserTurnsExplicit"`
	KeepFallbackToCompactAll bool             `json:"keepFallbackToCompactAll"`
	BudgetCut                string           `json:"budgetCut,omitempty"`
	KeptTokensEst            int              `json:"keptTokensEst"`
	SmartKeepAdjusted        bool             `json:"smartKeepAdjusted,omitempty"`
	SmartFromKeep            int              `json:"smartFromKeep,omitempty"`
	Reason                   CompactionReason `json:"reason,omitempty"`
	WillRetry                bool             `json:"willRetry,omitempty"`
}

// BudgetCutKind is before-compact.ts BudgetCutKind.
type BudgetCutKind string

const (
	// BudgetCutNoAnchor is a budget cut with no user-turn anchor.
	BudgetCutNoAnchor BudgetCutKind = "no_anchor"
	// BudgetCutOversizedTail is a budget cut because the kept tail was oversized.
	BudgetCutOversizedTail BudgetCutKind = "oversized_tail"
)

// OVERSIZED_TAIL_FACTOR is before-compact.ts OVERSIZED_TAIL_FACTOR.
const OversizedTailFactor = 2.5

// Smart tail thresholds (before-compact.ts MIN/MAX_SMART_TAIL_TOKENS).
const (
	MinSmartTailTokens = 5000
	MaxSmartTailTokens = 25000
)

// AutoContinueCustomType is before-compact.ts AUTO_CONTINUE_CUSTOM_TYPE.
const AutoContinueCustomType = "pi-vcc-auto-continue"

// PiSelfResumeVersion is before-compact.ts PI_SELF_RESUME_VERSION: the first pi
// that resumes the run by itself after an automatic compaction.
var PiSelfResumeVersion = [3]int{0, 84, 4}

// OwnCutCancelReason is before-compact.ts OwnCutCancelReason.
type OwnCutCancelReason string

const (
	// CancelNoLiveMessages means the live window holds nothing renderable.
	CancelNoLiveMessages OwnCutCancelReason = "no_live_messages"
	// CancelTooFewLiveMessages means there are two or fewer live messages.
	CancelTooFewLiveMessages OwnCutCancelReason = "too_few_live_messages"
)

// EntryWithMessage is before-compact.ts EntryWithMessage: one live entry and its
// agent message.
type EntryWithMessage struct {
	EntryID string
	Type    string
	Message map[string]any
}

// OwnCutResult is before-compact.ts OwnCutResult.
type OwnCutResult struct {
	OK bool
	// Reason is why the cut was cancelled (OK false).
	Reason OwnCutCancelReason
	// Messages are the messages to summarize.
	Messages []map[string]any
	// SelectedIDs are the entry ids parallel to Messages.
	SelectedIDs []string
	// FirstKeptEntryID is the id of the first kept entry ("" = compact all).
	FirstKeptEntryID         string
	CompactAll               bool
	KeptUserTurns            int
	TotalUserTurns           int
	RequestedKeepUserTurns   int
	KeepFallbackToCompactAll bool
	BudgetCut                BudgetCutKind
}

// parseVersionCore is before-compact.ts parseVersionCore(): [major, minor,
// patch] without prerelease/build suffixes, or ok=false.
func parseVersionCore(version string) ([3]int, bool) {
	trimmed := jsTrimSpace(version)
	if strings.HasPrefix(trimmed, "v") || strings.HasPrefix(trimmed, "V") {
		trimmed = trimmed[1:]
	}
	parts := strings.SplitN(trimmed, ".", 3)
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, part := range parts {
		digits := ""
		for j := 0; j < len(part); j++ {
			c := part[j]
			if c < '0' || c > '9' {
				break
			}
			digits += string(c)
		}
		if digits == "" {
			return [3]int{}, false
		}
		// Anything after the digits must be a prerelease/build marker, and only
		// on the last component; `/^v?\d+\.\d+\.\d+(?:[-+].*)?$/` allows no
		// other trailing characters.
		rest := part[len(digits):]
		if rest != "" && i != 2 {
			return [3]int{}, false
		}
		if rest != "" && !strings.HasPrefix(rest, "-") && !strings.HasPrefix(rest, "+") {
			return [3]int{}, false
		}
		value, ok := parseIntStrict(digits)
		if !ok {
			return [3]int{}, false
		}
		out[i] = value
	}
	return out, true
}

// ShouldScheduleAutoContinue is before-compact.ts shouldScheduleAutoContinue():
// both keys must turn — the user's setting and a pi old enough to need the
// fallback. An unreadable version fails safe to "no continue".
func ShouldScheduleAutoContinue(settingEnabled bool, piVersion string) bool {
	if !settingEnabled {
		return false
	}
	running, ok := parseVersionCore(piVersion)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if running[i] != PiSelfResumeVersion[i] {
			return running[i] < PiSelfResumeVersion[i]
		}
	}
	return false
}

// formatTokens is before-compact.ts formatTokens(): "1.2k" above 999.
func formatTokens(n int) string {
	if n >= 1000 {
		return jsFixed1(float64(n)/1000) + "k"
	}
	return jsNumberString(float64(n))
}

// FormatCompactionStats is before-compact.ts formatCompactionStats().
func FormatCompactionStats(stats CompactionStats) string {
	if stats.BudgetCut != "" {
		reason := "oversized tail"
		if stats.BudgetCut == string(BudgetCutNoAnchor) {
			reason = "no user anchor"
		}
		return "pi-vcc: kept ~" + formatTokens(stats.KeptTokensEst) + " tok tail (mid-turn cut, " + reason +
			"), summarized " + jsNumberString(float64(stats.Summarized)) + "."
	}
	notes := []string{"summarized " + jsNumberString(float64(stats.Summarized))}
	if stats.SmartKeepAdjusted {
		notes = append(notes, "smart-keep")
	}
	return "pi-vcc: kept " + jsNumberString(float64(stats.KeptUserTurns)) + "/" +
		jsNumberString(float64(stats.TotalUserTurns)) + " turns, ~" + formatTokens(stats.KeptTokensEst) + " tok (" +
		strings.Join(notes, ", ") + ")."
}

// ReadCompactionEventContext is before-compact.ts readCompactionEventContext():
// the event's reason and willRetry.
func ReadCompactionEventContext(event map[string]any) (CompactionReason, bool) {
	var reason CompactionReason
	if text, ok := event["reason"].(string); ok {
		switch text {
		case "manual", "threshold", "overflow":
			reason = CompactionReason(text)
		}
	}
	willRetry, _ := event["willRetry"].(bool)
	return reason, willRetry
}

// ParsedCompactionInstructions is before-compact.ts parseCompactionInstructions().
type ParsedCompactionInstructions struct {
	IsPiVcc               bool
	KeepUserTurns         int
	KeepUserTurnsExplicit bool
	FollowUpPrompt        string
	HasFollowUpPrompt     bool
}

// ParseCompactionInstructions is before-compact.ts parseCompactionInstructions().
func ParseCompactionInstructions(customInstructions string) ParsedCompactionInstructions {
	trimmed := jsTrimSpace(customInstructions)
	if trimmed == PiVccCompactInstruction {
		return ParsedCompactionInstructions{IsPiVcc: true, KeepUserTurns: 1}
	}
	keepPrefix := PiVccCompactInstruction + " "
	if strings.HasPrefix(trimmed, keepPrefix) {
		parsed := ParseKeepAndPrompt(trimmed[len(keepPrefix):])
		out := ParsedCompactionInstructions{IsPiVcc: true, KeepUserTurns: 1}
		if parsed.HasKeepUserTurns {
			out.KeepUserTurns = parsed.KeepUserTurns
		}
		out.KeepUserTurnsExplicit = parsed.KeepUserTurnsExplicit
		return out
	}
	parsed := ParseKeepAndPrompt(customInstructions)
	out := ParsedCompactionInstructions{KeepUserTurns: 1}
	if parsed.HasKeepUserTurns {
		out.KeepUserTurns = parsed.KeepUserTurns
	}
	out.KeepUserTurnsExplicit = parsed.KeepUserTurnsExplicit
	if parsed.FollowUpPrompt != "" {
		out.FollowUpPrompt = parsed.FollowUpPrompt
		out.HasFollowUpPrompt = true
	}
	return out
}

// NormalizeKeepUserTurns is before-compact.ts normalizeKeepUserTurns(): a
// non-finite or negative request becomes 0.
func NormalizeKeepUserTurns(keepUserTurns int) int {
	if keepUserTurns < 0 {
		return 0
	}
	return keepUserTurns
}

// entryField reads an entry's string field.
func entryField(entry map[string]any, key string) string {
	text, _ := entry[key].(string)
	return text
}

// isSkippedCustom is before-compact.ts isSkippedCustom(): a custom_message the
// user listed in skipCustomTypes.
func isSkippedCustom(message map[string]any, skipCustomTypes []string) bool {
	if messageRole(message) != "custom" {
		return false
	}
	customType, _ := message["customType"].(string)
	for _, skipped := range skipCustomTypes {
		if skipped == customType {
			return true
		}
	}
	return false
}

// toLlmSafe is before-compact.ts toLlmSafe(): convertToLlm for one message, as
// the summarizer input path runs it. A message it drops yields nothing.
func toLlmSafe(message map[string]any) []map[string]any {
	if message == nil {
		return nil
	}
	return sdk.ConvertToLlm([]map[string]any{message})
}

// prefixHasRenderableContent is before-compact.ts prefixHasRenderableContent():
// whether the prefix contributes at least one block after the skipCustomTypes
// filter, convertToLlm and normalize.
func prefixHasRenderableContent(prefix []EntryWithMessage, skipCustomTypes []string) bool {
	messages := make([]map[string]any, 0, len(prefix))
	for _, item := range prefix {
		if isSkippedCustom(item.Message, skipCustomTypes) {
			continue
		}
		messages = append(messages, toLlmSafe(item.Message)...)
	}
	return len(Normalize(messages, nil)) > 0
}

// toLiveMessage is before-compact.ts toLiveMessage(): a non-message entry that
// carries LLM-context text becomes its agent-message form.
func toLiveMessage(entry map[string]any) map[string]any {
	kind := entryField(entry, "type")
	if kind == "message" {
		message, ok := entry["message"].(map[string]any)
		if !ok || message == nil {
			return nil
		}
		return message
	}
	if kind == "custom_message" {
		out := map[string]any{
			"role":       "custom",
			"customType": entry["customType"],
			"content":    entry["content"],
			"display":    entry["display"],
			"details":    entry["details"],
		}
		if timestamp := prefixTimestamp(entry); timestamp != nil {
			out["timestamp"] = timestamp
		}
		return out
	}
	if kind == "branch_summary" {
		out := map[string]any{
			"role":    "branchSummary",
			"summary": entry["summary"],
			"fromId":  entry["fromId"],
		}
		if timestamp := prefixTimestamp(entry); timestamp != nil {
			out["timestamp"] = timestamp
		}
		return out
	}
	return nil
}

// prefixTimestamp is `new Date(entry.timestamp).getTime()` for a present value.
func prefixTimestamp(entry map[string]any) any {
	value, present := entry["timestamp"]
	if !present || value == nil {
		return nil
	}
	return value
}

// CollectLiveMessages is before-compact.ts collectLiveMessages(): the entries of
// the live window, with the orphan-recovery rule for a missing firstKeptEntryId.
func CollectLiveMessages(branchEntries []map[string]any) []EntryWithMessage {
	// A context-edited history needs pi's buildSessionProjection, which the
	// subprocess SDK does not expose; the TS code defers to the hook error in
	// the same situation on an older host.
	for _, entry := range branchEntries {
		if entryField(entry, "type") == "context_edit" {
			return nil
		}
	}

	lastCompactionIdx := -1
	lastKeptID := ""
	for i := len(branchEntries) - 1; i >= 0; i-- {
		if entryField(branchEntries[i], "type") == "compaction" {
			lastCompactionIdx = i
			lastKeptID = entryField(branchEntries[i], "firstKeptEntryId")
			break
		}
	}

	hasPriorCompaction := lastCompactionIdx >= 0
	hasValidKeptID := lastKeptID != ""
	if hasValidKeptID {
		found := false
		for _, entry := range branchEntries {
			if entryField(entry, "id") == lastKeptID {
				found = true
				break
			}
		}
		hasValidKeptID = found
	}
	orphanRecovery := hasPriorCompaction && !hasValidKeptID

	live := make([]EntryWithMessage, 0, len(branchEntries))
	if orphanRecovery {
		for i := lastCompactionIdx + 1; i < len(branchEntries); i++ {
			entry := branchEntries[i]
			if entryField(entry, "type") == "compaction" {
				continue
			}
			if message := toLiveMessage(entry); message != nil {
				live = append(live, EntryWithMessage{EntryID: entryField(entry, "id"), Type: entryField(entry, "type"), Message: message})
			}
		}
		return live
	}

	foundKept := lastKeptID == ""
	for _, entry := range branchEntries {
		if !foundKept && entryField(entry, "id") == lastKeptID {
			foundKept = true
		}
		if !foundKept {
			continue
		}
		if entryField(entry, "type") == "compaction" {
			continue
		}
		if message := toLiveMessage(entry); message != nil {
			live = append(live, EntryWithMessage{EntryID: entryField(entry, "id"), Type: entryField(entry, "type"), Message: message})
		}
	}
	return live
}

// userIndicesOf is the `role === "user"` position scan.
func userIndicesOf(live []EntryWithMessage) []int {
	out := make([]int, 0, len(live))
	for i, item := range live {
		if messageRole(item.Message) == "user" {
			out = append(out, i)
		}
	}
	return out
}

// messagesOf maps the live window's agent messages.
func messagesOf(live []EntryWithMessage) []map[string]any {
	out := make([]map[string]any, 0, len(live))
	for _, item := range live {
		out = append(out, item.Message)
	}
	return out
}

// idsOf maps the live window's entry ids.
func idsOf(live []EntryWithMessage) []string {
	out := make([]string, 0, len(live))
	for _, item := range live {
		out = append(out, item.EntryID)
	}
	return out
}

// BuildOwnCut is before-compact.ts buildOwnCut(): the cut pi-vcc makes itself,
// so it can summarize a window pi's own cut would leave empty.
func BuildOwnCut(branchEntries []map[string]any, keepUserTurns int, skipCustomTypes []string) OwnCutResult {
	normalizedKeep := NormalizeKeepUserTurns(keepUserTurns)
	live := CollectLiveMessages(branchEntries)

	if len(live) == 0 {
		return OwnCutResult{Reason: CancelNoLiveMessages}
	}
	if len(live) <= 2 {
		return OwnCutResult{Reason: CancelTooFewLiveMessages}
	}
	// A window with nothing renderable compiles to "" whichever cut is chosen,
	// so treat it as nothing to compact.
	if !prefixHasRenderableContent(live, skipCustomTypes) {
		return OwnCutResult{Reason: CancelNoLiveMessages}
	}

	userIndices := userIndicesOf(live)
	compactAll := func(fallback bool) OwnCutResult {
		return OwnCutResult{
			OK:                       true,
			Messages:                 messagesOf(live),
			SelectedIDs:              idsOf(live),
			FirstKeptEntryID:         "",
			CompactAll:               true,
			KeptUserTurns:            0,
			TotalUserTurns:           len(userIndices),
			RequestedKeepUserTurns:   normalizedKeep,
			KeepFallbackToCompactAll: fallback,
		}
	}

	if normalizedKeep <= 0 {
		return compactAll(false)
	}

	targetUserIdx := len(userIndices) - normalizedKeep
	cutIdx := -1
	if targetUserIdx >= 0 {
		cutIdx = userIndices[targetUserIdx]
	}
	if cutIdx <= 0 {
		return compactAll(true)
	}
	if !prefixHasRenderableContent(live[:cutIdx], skipCustomTypes) {
		return compactAll(true)
	}

	return OwnCutResult{
		OK:                       true,
		Messages:                 messagesOf(live[:cutIdx]),
		SelectedIDs:              idsOf(live[:cutIdx]),
		FirstKeptEntryID:         live[cutIdx].EntryID,
		KeptUserTurns:            len(userIndices) - targetUserIdx,
		TotalUserTurns:           len(userIndices),
		RequestedKeepUserTurns:   normalizedKeep,
		KeepFallbackToCompactAll: false,
	}
}

// FindBudgetCutIndex is before-compact.ts findBudgetCutIndex(): walk the tail
// backwards to the token budget, then snap forward off any toolResult.
func FindBudgetCutIndex(live []EntryWithMessage, maxTokens int, charsPerToken float64) int {
	acc := 0
	crossed := -1
	for i := len(live) - 1; i >= 0; i-- {
		acc += EstimateMessageContentTokens(live[i].Message["content"], charsPerToken)
		if acc >= maxTokens {
			crossed = i
			break
		}
	}
	if crossed < 0 {
		return -1
	}
	start := crossed
	if start < 1 {
		start = 1
	}
	for j := start; j < len(live); j++ {
		if messageRole(live[j].Message) != "toolResult" {
			return j
		}
	}
	return -1
}

// ApplyTailBudgetOptions is before-compact.ts applyTailBudget's options.
type ApplyTailBudgetOptions struct {
	MaxTokens       int
	OversizedFactor float64
	CharsPerToken   float64
	SkipCustomTypes []string
}

// ApplyTailBudget is before-compact.ts applyTailBudget(): rescue default-path
// sessions whose user-turn-anchored tail is absent or oversized.
func ApplyTailBudget(branchEntries []map[string]any, cut OwnCutResult, opts ApplyTailBudgetOptions) OwnCutResult {
	if !cut.OK {
		return cut
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = MaxSmartTailTokens
	}
	factor := opts.OversizedFactor
	if factor == 0 {
		factor = OversizedTailFactor
	}
	live := CollectLiveMessages(branchEntries)
	renders := func(idx int) bool {
		if idx < 0 || idx > len(live) {
			return false
		}
		return prefixHasRenderableContent(live[:idx], opts.SkipCustomTypes)
	}
	userCount := func(items []EntryWithMessage) int {
		count := 0
		for _, item := range items {
			if messageRole(item.Message) == "user" {
				count++
			}
		}
		return count
	}

	budgetResult := func(idx int, kind BudgetCutKind) OwnCutResult {
		return OwnCutResult{
			OK:                       true,
			Messages:                 messagesOf(live[:idx]),
			SelectedIDs:              idsOf(live[:idx]),
			FirstKeptEntryID:         live[idx].EntryID,
			CompactAll:               false,
			KeptUserTurns:            userCount(live[idx:]),
			TotalUserTurns:           userCount(live),
			RequestedKeepUserTurns:   cut.RequestedKeepUserTurns,
			KeepFallbackToCompactAll: false,
			BudgetCut:                kind,
		}
	}

	// Case A: no user anchor -> compact-all. Re-cut to a token budget unless
	// the compact-all came from an explicit keep:0.
	if cut.CompactAll {
		if !cut.KeepFallbackToCompactAll {
			return cut
		}
		idx := FindBudgetCutIndex(live, maxTokens, opts.CharsPerToken)
		if idx < 0 || !renders(idx) {
			return cut
		}
		return budgetResult(idx, BudgetCutNoAnchor)
	}

	// Case B: oversized user-boundary tail.
	tailStart := len(cut.Messages)
	tailTokens := 0
	for i := tailStart; i < len(live); i++ {
		tailTokens += EstimateMessageContentTokens(live[i].Message["content"], opts.CharsPerToken)
	}
	if float64(tailTokens) <= float64(maxTokens)*factor {
		return cut
	}
	idx := FindBudgetCutIndex(live, maxTokens, opts.CharsPerToken)
	if idx <= tailStart {
		return cut
	}
	return budgetResult(idx, BudgetCutOversizedTail)
}

// ResolveSmartKeepOptions is before-compact.ts ResolveSmartKeepOptions.
type ResolveSmartKeepOptions struct {
	BranchEntries []map[string]any
	// RequestedKeepUserTurns is the user's keep:N, or -1 when unspecified.
	RequestedKeepUserTurns int
	// Explicit is true when the user typed keep:N.
	Explicit bool
	// SmartKeepTail is the setting toggle.
	SmartKeepTail bool
	// MinTokens and MaxTokens override the thresholds (tests).
	MinTokens     int
	MaxTokens     int
	CharsPerToken float64
}

// ResolveSmartKeepResult is before-compact.ts ResolveSmartKeepResult.
type ResolveSmartKeepResult struct {
	KeepUserTurns int
	SmartAdjusted bool
	FromKeep      int
}

// tailTokensForKeep is before-compact.ts tailTokensForKeep(): the tail's token
// estimate, or ok=false when keep would trigger compact-all or cancel.
func tailTokensForKeep(branchEntries []map[string]any, keepUserTurns int, charsPerToken float64) (int, bool) {
	cut := BuildOwnCut(branchEntries, keepUserTurns, nil)
	if !cut.OK || cut.CompactAll {
		return 0, false
	}
	idx := -1
	for i, entry := range branchEntries {
		if entryField(entry, "id") == cut.FirstKeptEntryID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, false
	}
	chars := 0
	for _, entry := range branchEntries[idx:] {
		if entryField(entry, "type") != "message" {
			continue
		}
		message, _ := entry["message"].(map[string]any)
		var content any
		if message != nil {
			content = message["content"]
		}
		chars += EstimateMessageContentChars(content)
	}
	return EstimateTokensFromChars(float64(chars), charsPerToken), true
}

// ResolveSmartKeepUserTurns is before-compact.ts resolveSmartKeepUserTurns().
func ResolveSmartKeepUserTurns(opts ResolveSmartKeepOptions) ResolveSmartKeepResult {
	minTokens := opts.MinTokens
	if minTokens == 0 {
		minTokens = MinSmartTailTokens
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = MaxSmartTailTokens
	}
	baseKeep := opts.RequestedKeepUserTurns
	if baseKeep < 0 {
		baseKeep = 1
	}

	if opts.Explicit || !opts.SmartKeepTail {
		return ResolveSmartKeepResult{KeepUserTurns: baseKeep, FromKeep: baseKeep}
	}

	baseTokens, ok := tailTokensForKeep(opts.BranchEntries, baseKeep, opts.CharsPerToken)
	if !ok || baseTokens > minTokens {
		return ResolveSmartKeepResult{KeepUserTurns: baseKeep, FromKeep: baseKeep}
	}

	baseCut := BuildOwnCut(opts.BranchEntries, baseKeep, nil)
	totalUserTurns := 0
	if baseCut.OK {
		totalUserTurns = baseCut.TotalUserTurns
	}

	selected := baseKeep
	for k := baseKeep + 1; k <= totalUserTurns; k++ {
		tokens, ok := tailTokensForKeep(opts.BranchEntries, k, opts.CharsPerToken)
		if !ok || tokens > maxTokens {
			break
		}
		selected = k
	}
	return ResolveSmartKeepResult{KeepUserTurns: selected, SmartAdjusted: selected != baseKeep, FromKeep: baseKeep}
}

// reasonMessages is before-compact.ts REASON_MESSAGES.
var reasonMessages = map[OwnCutCancelReason]string{
	CancelNoLiveMessages:     "pi-vcc: Nothing to compact (no live messages)",
	CancelTooFewLiveMessages: "pi-vcc: Too few messages to compact",
}

// ReasonMessage is REASON_MESSAGES[reason].
func ReasonMessage(reason OwnCutCancelReason) string {
	return reasonMessages[reason]
}

// WriteDebugSnapshot is before-compact.ts dbg(): write the debug JSON when the
// setting is on. It never fails the caller.
func WriteDebugSnapshot(path string, enabled bool, data any) {
	if !enabled {
		return
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, encoded, 0o644)
}

// previewContent is before-compact.ts previewContent(): a 300-char preview of a
// message's content, for the debug snapshot.
func previewContent(content any) string {
	if text, ok := content.(string); ok {
		return jsSlice(text, 0, 300)
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	lines := make([]string, 0, len(parts))
	for _, rawPart := range parts {
		part, isObject := rawPart.(map[string]any)
		if !isObject {
			continue
		}
		switch kind, _ := part["type"].(string); kind {
		case "text":
			text, _ := part["text"].(string)
			lines = append(lines, text)
		case "toolCall":
			name, _ := part["name"].(string)
			lines = append(lines, "[toolCall:"+name+"]")
		case "thinking":
			lines = append(lines, "[thinking]")
		case "image":
			mimeType, _ := part["mimeType"].(string)
			lines = append(lines, "[image:"+mimeType+"]")
		default:
			lines = append(lines, "["+kind+"]")
		}
	}
	return jsSlice(strings.Join(lines, "\n"), 0, 300)
}

// SectionHeaders is `[...summary.matchAll(/^\[(.+?)\]/gm)].map(m => m[1])`: the
// section names a compiled summary carries.
var sectionHeaderRe = mustCompile(`(?m)^\[([^\n\r\x{2028}\x{2029}]+?)\]`)

// SectionHeaders returns the section names of a compiled summary, in order.
func SectionHeaders(summary string) []string {
	matches := sectionHeaderRe.FindAllStringSubmatch(summary, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match[1])
	}
	return out
}
