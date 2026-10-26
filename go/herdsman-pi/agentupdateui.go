package herdsmanpi

import (
	"regexp"
	"strings"
)

// Port of packages/herdsman-pi/src/agent-update-ui.ts.
//
// The TS module builds a pi-tui `Box`/`Text` component pair and hands it to the
// host. PiG has no equivalent host component tree for the subprocess SDK: the
// message renderer receives `(ctx, message, options, width)` and answers with
// terminal lines. The port therefore keeps the TS text assembly (heading, row
// per visible outcome, omission line) and returns the rendered lines, with the
// Box padding/bg callback applied to each line. Differences from the TS are
// listed in README.md.

// CollapsedAgentUpdateLimit mirrors COLLAPSED_AGENT_UPDATE_LIMIT: how many
// outcome rows a collapsed card shows before the omission line.
const CollapsedAgentUpdateLimit = 3

// AgentUpdateMessageDetails mirrors the TS `AgentUpdateMessageDetails`.
type AgentUpdateMessageDetails struct {
	EventIDs []int
	Outcomes []AgentOutcome
}

// FooterStateKind mirrors the TS `HerdsmanFooterState` discriminant.
type FooterStateKind int

const (
	FooterOff FooterStateKind = iota
	FooterOn
	FooterReconnecting
)

// HerdsmanFooterState mirrors the TS `HerdsmanFooterState` union.
type HerdsmanFooterState struct {
	Kind        FooterStateKind
	UpdateCount int
}

// FooterOffState is `{ kind: "off" }`.
func FooterOffState() HerdsmanFooterState { return HerdsmanFooterState{Kind: FooterOff} }

// FooterOnState is `{ kind: "on"; updateCount }`.
func FooterOnState(updateCount int) HerdsmanFooterState {
	return HerdsmanFooterState{Kind: FooterOn, UpdateCount: updateCount}
}

// FooterReconnectingState is `{ kind: "reconnecting" }`.
func FooterReconnectingState() HerdsmanFooterState {
	return HerdsmanFooterState{Kind: FooterReconnecting}
}

// FormatHerdsmanFooterStatus mirrors `formatHerdsmanFooterStatus`. The nil
// return is the TS `undefined`: the caller clears the status line.
func FormatHerdsmanFooterStatus(state HerdsmanFooterState) *string {
	switch state.Kind {
	case FooterOff:
		return nil
	case FooterReconnecting:
		text := "◇ Herdsman · reconnecting"
		return &text
	}
	label := "◆ Herdsman"
	if state.UpdateCount == 0 {
		return &label
	}
	text := label + " · " + updateCountLabel(state.UpdateCount)
	return &text
}

// updateCountLabel mirrors `updateCountLabel`: the plural is only dropped at
// exactly one, so zero reads "0 agent updates".
func updateCountLabel(count int) string {
	if count == 1 {
		return "1 agent update"
	}
	return formatInt(int64(count)) + " agent updates"
}

// displayControlPattern is the UI cleaner's control-character class. Unlike the
// wake excerpt cleaner it also clears C1 controls (U+0080-U+009F) completely.
var displayControlPattern = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]`)

// displaySpacePattern mirrors `/\s+/g` for the UI cleaner. JS `\s` matches the
// Unicode space separators too, so the class is spelled out (RE2's `\s` is
// ASCII-only).
var displaySpacePattern = regexp.MustCompile(jsSpaceClass + `+`)

// cleanDisplayText mirrors `cleanDisplayText`: strip VT sequences, drop control
// characters, collapse *all* whitespace (newlines included) to one space, trim.
func cleanDisplayText(value string) string {
	cleaned := stripVTControlCharacters(value)
	cleaned = displayControlPattern.ReplaceAllString(cleaned, "")
	cleaned = displaySpacePattern.ReplaceAllString(cleaned, " ")
	return strings.TrimSpace(cleaned)
}

// recordOf is the TS `record` helper of this module: objects pass, arrays and
// primitives become an empty record.
func recordOf(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// isAgentOutcome mirrors the TS `isAgentOutcome` type guard. It validates the
// decoded details, which may come from a replayed session entry this extension
// never wrote, so every field is checked before use.
func isAgentOutcome(value any) (AgentOutcome, bool) {
	candidate := value
	if record, ok := candidate.(map[string]any); ok {
		agent, agentOK := record["agent"].(string)
		eventID, eventIDOK := jsNumberAsInt(record["eventId"])
		kind, _ := record["kind"].(string)
		terminalID, terminalIDOK := record["terminalId"].(string)
		text, textOK := record["text"].(string)
		if !agentOK || !eventIDOK || !terminalIDOK || !textOK {
			return AgentOutcome{}, false
		}
		switch kind {
		case "blocked", "completed", "failed":
		default:
			return AgentOutcome{}, false
		}
		// `name === undefined || name === null || typeof name === "string"`
		var name *string
		if raw, present := record["name"]; present && raw != nil {
			converted, ok := raw.(string)
			if !ok {
				return AgentOutcome{}, false
			}
			name = &converted
		}
		// `paneId === null || typeof paneId === "string"` (undefined passes the
		// second branch, since `typeof undefined !== "string"` would reject it —
		// but the TS guard has no `undefined` case, so a missing paneId rejects
		// the outcome. Keep that literal behaviour.)
		rawPane, panePresent := record["paneId"]
		var paneID *string
		switch {
		case !panePresent:
			return AgentOutcome{}, false
		case rawPane == nil:
		default:
			converted, ok := rawPane.(string)
			if !ok {
				return AgentOutcome{}, false
			}
			paneID = &converted
		}
		// `reason === undefined || typeof reason === "string"`
		var reason *string
		if raw, present := record["reason"]; present {
			converted, ok := raw.(string)
			if !ok {
				return AgentOutcome{}, false
			}
			reason = &converted
		}
		outcome := AgentOutcome{
			Agent:      agent,
			EventID:    eventID,
			Kind:       kind,
			Name:       name,
			PaneID:     paneID,
			TerminalID: terminalID,
			Text:       text,
		}
		if reason != nil {
			outcome.Reason = reason
		}
		return outcome, true
	}
	return AgentOutcome{}, false
}

// jsNumberAsInt accepts a JSON number the way `typeof value === "number"`
// does and narrows it to an int for the event id.
func jsNumberAsInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}

// MessageDetails mirrors the TS `messageDetails`: the numeric event ids and the
// outcomes that pass the guard, with everything else dropped.
func MessageDetails(value any) AgentUpdateMessageDetails {
	details := recordOf(value)
	result := AgentUpdateMessageDetails{EventIDs: []int{}, Outcomes: []AgentOutcome{}}
	if rawIDs, ok := details["eventIds"].([]any); ok {
		for _, raw := range rawIDs {
			if id, ok := jsNumberAsInt(raw); ok {
				result.EventIDs = append(result.EventIDs, id)
			}
		}
	}
	if rawOutcomes, ok := details["outcomes"].([]any); ok {
		for _, raw := range rawOutcomes {
			if outcome, ok := isAgentOutcome(raw); ok {
				result.Outcomes = append(result.Outcomes, outcome)
			}
		}
	}
	return result
}

// UITheme is the theme surface the renderer needs (TS `ThemeLike`).
type UITheme interface {
	BG(color, text string) string
	Bold(text string) string
	FG(color, text string) string
}

// plainTheme is a ThemeLike whose methods are the identity, which is what the
// footer and the collapsed-heading tests render with.
type plainTheme struct{}

func (plainTheme) BG(_, text string) string { return text }
func (plainTheme) Bold(text string) string  { return text }
func (plainTheme) FG(_, text string) string { return text }

var _ UITheme = plainTheme{}

// RenderOptions mirrors the TS `RenderOptions`.
type RenderOptions struct {
	Expanded bool
}

// RenderAgentUpdateMessageLines mirrors `renderAgentUpdateMessage` and returns
// the lines the host draws.
//
// The TS returns a `Box(1, 1, bg)` that holds one `Text`: padding one column
// left and right of every line and one blank line above and below, with the
// background callback applied to every line it emits (including the blanks).
// The ported lines carry that shape so a host that only knows how to print
// lines gets the same card.
func RenderAgentUpdateMessageLines(detailsValue any, options RenderOptions, theme UITheme) []string {
	details := MessageDetails(detailsValue)
	count := len(details.Outcomes)
	if count == 0 {
		count = len(details.EventIDs)
	}
	heading := theme.FG("customMessageLabel", "◆ "+theme.Bold("Herdsman")) +
		theme.FG("muted", " "+updateCountLabel(count))

	visibleOutcomes := details.Outcomes
	if !options.Expanded && len(visibleOutcomes) > CollapsedAgentUpdateLimit {
		visibleOutcomes = visibleOutcomes[:CollapsedAgentUpdateLimit]
	}

	rows := make([]string, 0, len(visibleOutcomes)*2)
	for _, outcome := range visibleOutcomes {
		completed := outcome.Kind == "completed"
		color := "warning"
		glyph := "!"
		if completed {
			color = "success"
			glyph = "✓"
		}
		agent := outcome.Agent
		summary := strings.Join([]string{
			theme.FG(color, glyph),
			theme.Bold(AgentIdentityLabel(agent, outcome.Name)),
			theme.FG(color, outcome.Kind),
			theme.FG("muted", cleanDisplayText(textOrEmpty(outcome.PaneID))),
		}, " ")
		if !options.Expanded {
			rows = append(rows, summary)
			continue
		}
		cleanedResponse := cleanDisplayText(outcome.Text)
		cleanedReason := ""
		if outcome.Kind == "failed" {
			cleanedReason = cleanDisplayText(textOrEmpty(outcome.Reason))
		}
		response := "No final response"
		switch {
		case len(cleanedResponse) > 0:
			response = cleanedResponse
		case len(cleanedReason) > 0:
			response = cleanedReason
		}
		rows = append(rows, summary, theme.FG("muted", "  Last response  "+response))
	}

	hiddenCount := len(details.Outcomes) - len(visibleOutcomes)
	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, heading)
	lines = append(lines, rows...)
	if hiddenCount > 0 {
		lines = append(lines, theme.FG("muted", "… "+formatInt(int64(hiddenCount))+" more"))
	}

	// Box(1, 1, bg): vertical padding of one line, horizontal padding of one
	// column, background callback over every emitted line.
	padded := make([]string, 0, len(lines)+2)
	padded = append(padded, theme.BG("customMessageBg", ""))
	for _, line := range lines {
		padded = append(padded, theme.BG("customMessageBg", " "+line+" "))
	}
	padded = append(padded, theme.BG("customMessageBg", ""))
	return padded
}

// RenderAgentUpdateMessage is the TS-call-site equivalent with the identity
// theme: the text a plain host would show, one line per row.
func RenderAgentUpdateMessage(detailsValue any, options RenderOptions) string {
	return strings.Join(RenderAgentUpdateMessageLines(detailsValue, options, plainTheme{}), "\n")
}
