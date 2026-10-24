package jevgate

// Judging rules and output formatting of jev-gate.
//
// The thresholds below are FROZEN (2026-10-06 constructive calibration,
// re-checked against Clef on 2026-10-16). Porting must not change their
// semantics: Jev/Clef is systematically under-confident on existence
// questions, so the decision is three-way — only the two ends decide, and the
// middle band is always reported as "uncertain, read it yourself".

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	// ProviderID is the provider id registered with the host. It is deliberately
	// not "magpie": registerProvider replaces every model of the provider it
	// names, so an independent id cannot clobber the chat models of models.json
	// (TS: PROVIDER).
	ProviderID = "magpie-jev"
	// BuiltinClassifierProvider is the pi-ai built-in classifier provider that a
	// subagent session clears (TS: BUILTIN_CLASSIFIER_PROVIDER).
	BuiltinClassifierProvider = "typesafe"

	// DefaultBaseURL is the local Magpie gateway (TS: DEFAULT_BASE_URL).
	DefaultBaseURL = "http://127.0.0.1:3425/v1"
	// DefaultModelID is the Magpie classifier routing group (TS: DEFAULT_MODEL_ID).
	DefaultModelID = "group/classifier"
	// DefaultMaxStateChars caps the payload sent upstream (TS: MAX_STATE_CHARS default).
	DefaultMaxStateChars = 24000
	// DefaultTimeoutMs is the single-call timeout (TS: TIMEOUT_MS default).
	DefaultTimeoutMs = 60000
	// LocalGatewayKey is the literal token of the local Magpie gateway. It is not
	// a secret and matches models.json and magpie-image.ts (TS: LOCAL_GATEWAY_KEY).
	LocalGatewayKey = "magpie"
	// classifierAPI is the System One classifier API the registered model declares.
	classifierAPI = "typesafe-system-one"
)

// Credential environment variables, in resolution order (TS: KEY_ENV_NAMES).
var keyEnvNames = []string{"MAGPIE_API_KEY", "WINDHUB_API_KEY"}

// Frozen three-way thresholds (TS: FIELD_MISSING_MAX / FIELD_PRESENT_MIN /
// GATE_CLEAR_MAX / GATE_FLAG_MIN / TRIAGE_NORMAL_MIN / TRIAGE_ANOMALY_MIN).
const (
	// FieldMissingMax: contract-lint fields below this probability count as missing.
	FieldMissingMax = 0.35
	// FieldPresentMin: contract-lint fields at or above this probability count as present.
	FieldPresentMin = 0.8
	// GateClearMax: gate probabilities below this count as clean.
	GateClearMax = 0.35
	// GateFlagMin: gate probabilities at or above this count as flagged.
	GateFlagMin = 0.8
	// TriageNormalMin is the probability "normal delivery" needs; anything less is an anomaly.
	TriageNormalMin = 0.95
	// TriageAnomalyMin is the probability a concrete anomaly label needs.
	TriageAnomalyMin = 0.8
)

// advisory is appended to every successful result (TS: ADVISORY).
const advisory = "ADVISORY ONLY — this is a classifier signal, not a verdict. It never authorizes releasing a change " +
	"or closing a pane. The existing gate rules remain authoritative."

// Config is the environment-derived configuration of the extension
// (TS: BASE_URL / MODEL_ID / MAX_STATE_CHARS / TIMEOUT_MS module constants).
type Config struct {
	// BaseURL is the gateway base URL with trailing slashes removed.
	BaseURL string
	// ModelID is the classifier routing group.
	ModelID string
	// MaxStateChars caps the payload; NaN or a negative value disables truncation
	// (the TS `Number(env || 24000)` result is kept as a float for that reason).
	MaxStateChars float64
	// TimeoutMs is the numeric timeout used in error messages.
	TimeoutMs float64
	// Timeout is TimeoutMs after Node's setTimeout clamping.
	Timeout time.Duration
}

// ConfigFromEnv reads the configuration from the process environment.
// Semantics follow the TS module constants: blank MAGPIE_BASE_URL falls back to
// WINDHUB_BASE_URL and then the default; trailing slashes are stripped; the
// numeric variables go through JavaScript Number() so a non-numeric value is
// NaN rather than zero.
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	baseURL := strings.TrimSpace(getenv("MAGPIE_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(getenv("WINDHUB_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	modelID := strings.TrimSpace(getenv("JEV_MODEL_ID"))
	if modelID == "" {
		modelID = DefaultModelID
	}

	maxStateChars := jsNumberOr(getenv("JEV_MAX_STATE_CHARS"), DefaultMaxStateChars)
	timeoutMs := jsNumberOr(getenv("JEV_TIMEOUT_MS"), DefaultTimeoutMs)
	return Config{
		BaseURL:       baseURL,
		ModelID:       modelID,
		MaxStateChars: maxStateChars,
		TimeoutMs:     timeoutMs,
		Timeout:       nodeSetTimeout(timeoutMs),
	}
}

// jsNumberOr is `Number(env || fallback)`: an empty (or unset) variable uses the
// fallback, anything else goes through JavaScript Number().
func jsNumberOr(raw string, fallback float64) float64 {
	if raw == "" {
		return fallback
	}
	return jsNumber(raw)
}

// jsNumber reproduces JavaScript Number(string) for the inputs these variables
// can carry: surrounding whitespace is ignored, empty is 0, the Infinity tokens
// are infinite, a hex literal is its integer value, and anything else is NaN.
func jsNumber(raw string) float64 {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0
	}
	switch text {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if value, err := strconv.ParseFloat(text, 64); err == nil {
		return value
	}
	if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0X") {
		if value, err := strconv.ParseUint(text[2:], 16, 64); err == nil {
			return float64(value)
		}
	}
	return math.NaN()
}

// nodeSetTimeout clamps a delay the way Node's setTimeout does: any value that
// is not a usable delay (NaN, infinite, below 1ms, above the 32-bit limit) is
// replaced by 1ms, and the timer fires with that 1ms warning-free budget.
func nodeSetTimeout(ms float64) time.Duration {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 1 || ms > 2147483647 {
		return time.Millisecond
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// oneLine collapses runs of whitespace, trims, and truncates with an ellipsis
// (TS: oneLine). The truncation counts characters, not bytes, so a multi-byte
// payload is cut on a rune boundary.
func oneLine(s string, max int) string {
	text := strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(text) > max {
		runes := []rune(text)
		return string(runes[:max]) + "…"
	}
	return text
}

// isRecord narrows a decoded JSON value to a JSON object.
func isRecord(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// isValidProb is `typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 1`.
func isValidProb(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

// numberValue reads a JSON number (or a Go integer) as float64. Booleans,
// strings, null and objects are not numbers.
func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// numberOrStringValue is `Number(x)` for a JSON value: numbers pass through and
// numeric strings are parsed; everything else is not a number.
func numberOrStringValue(value any) (float64, bool) {
	if number, ok := numberValue(value); ok {
		return number, true
	}
	if text, ok := value.(string); ok {
		parsed := jsNumber(text)
		if math.IsNaN(parsed) {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// probOf reads the probability of a noul/bool answer (TS: probOf). A bare number
// is a probability; an object carries it under "probability" or "noul". Any
// value outside the closed interval [0, 1], any non-number, and a missing field
// all yield "not answered".
func probOf(answer any) (float64, bool) {
	if number, ok := numberValue(answer); ok {
		if isValidProb(number) {
			return number, true
		}
		return 0, false
	}
	object, ok := isRecord(answer)
	if !ok {
		return 0, false
	}
	if raw, exists := object["probability"]; exists {
		number, ok := numberValue(raw)
		return number, ok && isValidProb(number)
	}
	if raw, exists := object["noul"]; exists {
		number, ok := numberValue(raw)
		return number, ok && isValidProb(number)
	}
	return 0, false
}

// probOfMap is probOf(answers, id): the probability of one named answer, and
// false when the response has no usable answer for that id.
func probOfMap(answers map[string]any, id string) (float64, bool) {
	if answers == nil {
		return 0, false
	}
	answer, ok := answers[id]
	if !ok {
		return 0, false
	}
	return probOf(answer)
}

// Choice is a validated choice answer (TS: choiceOf's return value).
type Choice struct {
	Choice string
	Label  string
	P      float64
	Probs  map[string]float64
}

// choiceOf reads a choice answer (TS: choiceOf). It requires a non-empty
// `choice`, a `probabilities` object whose every entry is a probability in
// [0, 1], that object to carry the chosen label, and an optional `confidence`
// that must also be a probability. Anything else is "not answered".
func choiceOf(answer any) (Choice, bool) {
	object, ok := isRecord(answer)
	if !ok {
		return Choice{}, false
	}
	label, ok := object["choice"].(string)
	if !ok || strings.TrimSpace(label) == "" {
		return Choice{}, false
	}
	rawProbs, ok := isRecord(object["probabilities"])
	if !ok {
		return Choice{}, false
	}
	probs := make(map[string]float64, len(rawProbs))
	for key, raw := range rawProbs {
		number, ok := numberValue(raw)
		if !ok || !isValidProb(number) {
			return Choice{}, false
		}
		probs[key] = number
	}
	chosen, ok := probs[label]
	if !ok {
		return Choice{}, false
	}
	if raw, exists := object["confidence"]; exists {
		number, ok := numberValue(raw)
		if !ok || !isValidProb(number) {
			return Choice{}, false
		}
	}
	return Choice{Choice: label, Label: label, P: chosen, Probs: probs}, true
}

// choiceOfMap is choiceOf(answers, id).
func choiceOfMap(answers map[string]any, id string) (Choice, bool) {
	if answers == nil {
		return Choice{}, false
	}
	answer, ok := answers[id]
	if !ok {
		return Choice{}, false
	}
	return choiceOf(answer)
}

// Line is one reported probability; a nil P is "no answer" (TS: Line).
type Line struct {
	ID string
	P  *float64
}

// line builds a Line from an optional probability.
func line(id string, p float64, ok bool) Line {
	if !ok {
		return Line{ID: id}
	}
	value := p
	return Line{ID: id, P: &value}
}

// triage3 splits the lines into the three bands (TS: triage3): at or above upper
// is "yes", [lower, upper) is "uncertain", everything else (including a missing
// answer) is "no".
func triage3(lines []Line, lower, upper float64) (yes, mid, no []string) {
	yes, mid, no = []string{}, []string{}, []string{}
	for _, entry := range lines {
		switch {
		case entry.P != nil && *entry.P >= upper:
			yes = append(yes, entry.ID)
		case entry.P != nil && *entry.P >= lower && *entry.P < upper:
			mid = append(mid, entry.ID)
		default:
			no = append(no, entry.ID)
		}
	}
	return yes, mid, no
}

// formatP2 is Number.prototype.toFixed(2).
func formatP2(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// jsNum renders a number the way a JavaScript template string does for the
// constants used here (0.35, 0.8, 0.95, 100).
func jsNum(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// fmtProbs renders the "probs:" line (TS: fmtProbs).
func fmtProbs(lines []Line) string {
	parts := make([]string, len(lines))
	for i, entry := range lines {
		if entry.P == nil {
			parts[i] = entry.ID + "=n/a"
			continue
		}
		parts[i] = entry.ID + "=" + formatP2(*entry.P)
	}
	return strings.Join(parts, ", ")
}

// sortedKeys returns the keys of a probability map in a stable order. The TS
// version keeps the upstream JSON order; Go maps have none, so the display order
// is sorted here (documented in README.md, display-only).
func sortedKeys(probs map[string]float64) []string {
	keys := make([]string, 0, len(probs))
	for key := range probs {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Verdict is the outcome of one task (TS: verdictFor's return value).
type Verdict struct {
	Lines   []Line
	Verdict []string
	Note    string
}

// verdictFor turns the answers into the three-way verdict (TS: verdictFor).
func verdictFor(task Task, answers map[string]any, role *Role) (Verdict, error) {
	switch task {
	case TaskContractLint:
		if role == nil {
			return Verdict{}, fmt.Errorf("contract_lint requires role in %s", roleList())
		}
		fields, ok := contractFields[*role]
		if !ok {
			return Verdict{}, fmt.Errorf("contract_lint requires role in %s", roleList())
		}
		lines := make([]Line, 0, len(fields))
		for _, field := range fields {
			probability, ok := probOfMap(answers, field.ID)
			lines = append(lines, line(field.ID, probability, ok))
		}
		yes, mid, no := triage3(lines, FieldMissingMax, FieldPresentMin)
		text := fmt.Sprintf("contract: present %d/%d", len(yes), len(lines))
		if len(yes) > 0 {
			text += " (" + strings.Join(yes, ", ") + ")"
		}
		if len(no) > 0 {
			text += fmt.Sprintf(" | MISSING(<%s): %s", jsNum(FieldMissingMax), strings.Join(no, ", "))
		}
		if len(mid) > 0 {
			text += fmt.Sprintf(" | UNCERTAIN(%s-%s, read the return yourself): %s", jsNum(FieldMissingMax), jsNum(FieldPresentMin), strings.Join(mid, ", "))
		}
		return Verdict{
			Lines:   lines,
			Verdict: []string{text},
			Note:    "三档依据 2026-10-06 构造式校准：中间带只报不确定，不当作存在。",
		}, nil
	case TaskGateScan:
		lines := make([]Line, 0, len(gateQuestions))
		for _, question := range gateQuestions {
			probability, ok := probOfMap(answers, question.ID)
			lines = append(lines, line(question.ID, probability, ok))
		}
		yes, mid, _ := triage3(lines, GateClearMax, GateFlagMin)
		noAnswer := []string{}
		for _, entry := range lines {
			if entry.P == nil {
				noAnswer = append(noAnswer, entry.ID)
			}
		}
		verdict := []string{}
		if len(yes) > 0 {
			verdict = append(verdict, fmt.Sprintf("gates FLAGGED(>=%s): %s", jsNum(GateFlagMin), strings.Join(yes, ", ")))
		} else {
			verdict = append(verdict, fmt.Sprintf("gates: nothing above %s", jsNum(GateFlagMin)))
		}
		if len(mid) > 0 {
			verdict = append(verdict, fmt.Sprintf("gates UNCERTAIN(%s-%s, confirm manually): %s", jsNum(GateClearMax), jsNum(GateFlagMin), strings.Join(mid, ", ")))
		}
		if len(noAnswer) > 0 {
			verdict = append(verdict, fmt.Sprintf("no answer for: %s (confirm manually)", strings.Join(noAnswer, ", ")))
		}
		return Verdict{
			Lines:   lines,
			Verdict: verdict,
			Note:    "单向信任：命中即可阻断；中间带仅提示人工确认，避免对干净交付误报。",
		}, nil
	case TaskReturnTriage:
		kind, hasKind := choiceOfMap(answers, "kind")
		pConc, hasConc := probOfMap(answers, "states_conclusion")
		lines := []Line{line("states_conclusion", pConc, hasConc)}
		if hasKind {
			for _, key := range sortedKeys(kind.Probs) {
				lines = append(lines, line("kind:"+key, kind.Probs[key], true))
			}
		}
		var verdict []string
		switch {
		case !hasKind:
			verdict = []string{"triage: no answer → treat as anomaly (宁可误留)"}
		case kind.Label == "normal_delivery":
			if kind.P >= TriageNormalMin {
				verdict = []string{fmt.Sprintf("triage: normal_delivery (p=%s)", formatP2(kind.P))}
			} else {
				verdict = []string{fmt.Sprintf("triage: UNCERTAIN (normal_delivery p=%s < %s) → treat as anomaly", formatP2(kind.P), jsNum(TriageNormalMin))}
			}
		case kind.P >= TriageAnomalyMin:
			verdict = []string{fmt.Sprintf("triage: %s (p=%s)", kind.Label, formatP2(kind.P))}
		default:
			verdict = []string{fmt.Sprintf("triage: UNCERTAIN (top=%s p=%s < %s) → treat as anomaly", kind.Label, formatP2(kind.P), jsNum(TriageAnomalyMin))}
		}
		return Verdict{
			Lines:   lines,
			Verdict: verdict,
			Note:    "异常方向可直接采信；「正常」方向不作为关闭 pane 的依据（判据仍是既有四要素）。",
		}, nil
	case TaskDisagreement:
		relation, hasRelation := choiceOfMap(answers, "relation")
		pMaterial, hasMaterial := probOfMap(answers, "has_material_disagreement")
		pSame, hasSame := probOfMap(answers, "same_finding_set")
		lines := []Line{
			line("has_material_disagreement", pMaterial, hasMaterial),
			line("same_finding_set", pSame, hasSame),
		}
		if hasRelation {
			for _, key := range sortedKeys(relation.Probs) {
				lines = append(lines, line("relation:"+key, relation.Probs[key], true))
			}
		}
		relationText := "relation: no answer"
		if hasRelation {
			relationText = fmt.Sprintf("relation: %s (p=%s)", relation.Label, formatP2(relation.P))
		}
		var materialText string
		switch {
		case !hasMaterial:
			materialText = "material disagreement: no answer"
		case pMaterial >= GateFlagMin:
			materialText = fmt.Sprintf("material disagreement: YES (p=%s)", formatP2(pMaterial))
		case pMaterial >= GateClearMax:
			materialText = fmt.Sprintf("material disagreement: UNCERTAIN (p=%s) → read both reports", formatP2(pMaterial))
		default:
			materialText = fmt.Sprintf("material disagreement: no (p=%s)", formatP2(pMaterial))
		}
		return Verdict{Lines: lines, Verdict: []string{relationText, materialText}}, nil
	case TaskSourceAudit:
		lines := make([]Line, 0, len(sourceQuestions))
		for _, question := range sourceQuestions {
			probability, ok := probOfMap(answers, question.ID)
			lines = append(lines, line(question.ID, probability, ok))
		}
		yes, mid, no := triage3(lines, FieldMissingMax, FieldPresentMin)
		text := fmt.Sprintf("sourcing: ok %d/%d", len(yes), len(lines))
		if len(no) > 0 {
			text += fmt.Sprintf(" | FLAGGED(<%s): %s", jsNum(FieldMissingMax), strings.Join(no, ", "))
		}
		if len(mid) > 0 {
			text += fmt.Sprintf(" | UNCERTAIN(%s-%s, verify yourself): %s", jsNum(FieldMissingMax), jsNum(FieldPresentMin), strings.Join(mid, ", "))
		}
		return Verdict{Lines: lines, Verdict: []string{text}}, nil
	default:
		return Verdict{}, fmt.Errorf("unknown task %s", string(task))
	}
}

// toolResult builds the AgentToolResult shape of the TS version.
func toolResult(text string, summary string, structured map[string]any, isError bool) sdk.ToolResult {
	return sdk.ToolResult{
		Content:           text,
		Details:           map[string]any{"summary": summary},
		StructuredContent: structured,
		IsError:           isError,
	}
}
