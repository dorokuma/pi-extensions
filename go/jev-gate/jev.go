package jevgate

// The System One HTTP call of jev-gate (TS: callJev / unwrapSystemOneBody /
// validateAnswers).
//
// Protocol facts (verified 2026-10-16 against the local Magpie
// `group/classifier` route → cloudflare-jev/@cf/cloudflare/clef):
//   - POST {baseUrl}/systemone with `{ model, state, questions }` returns the
//     top-level `{ model, answers, usage: { input_tokens, output_tokens } }`;
//   - the Cloudflare Workers AI envelope `{ result: { ... } }` and the
//     third-party double envelope `{ result: { state: "Completed", result: {...} } }`
//     are accepted as well;
//   - question types `noul`, `choice` and `score` are accepted; the literal
//     `type: "bool"` is rejected downstream (400 AiError code 5006), so this
//     extension never sends it.
//
// Every malformed response fails closed: the answers must cover exactly the
// requested question keys, and each answer must carry a well-formed probability
// inside the closed interval [0, 1].

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
)

// maxResponseBodyBytes caps the System One response this extension buffers. A
// larger body fails closed instead of growing with whatever the endpoint sends.
const maxResponseBodyBytes = 4 << 20

// errResponseBodyTooLarge marks a response over maxResponseBodyBytes. It is not
// a transport failure, so the caller reports it verbatim.
var errResponseBodyTooLarge = errors.New("response body too large")

// readBoundedBody reads one response body up to maxResponseBodyBytes + 1 so an
// oversized body is detected without buffering it whole.
func readBoundedBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBodyBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", errResponseBodyTooLarge, maxResponseBodyBytes)
	}
	return raw, nil
}

// Truncation records an explicit payload cut (TS: JevCall.truncated).
type Truncation struct {
	OriginalChars int
	UsedChars     int
}

// JevUsage is the token usage reported upstream (TS: JevCall.usage).
type JevUsage struct {
	Input  float64
	Output float64
}

// JevCall is one classifier response (TS: JevCall).
type JevCall struct {
	Answers   map[string]any
	Usage     *JevUsage
	Model     string
	Truncated *Truncation
}

// HTTPDoer is the part of *http.Client the caller uses; tests inject an
// httptest-backed client or a failing one.
type HTTPDoer interface {
	Do(request *http.Request) (*http.Response, error)
}

// JevAPI is one configured System One endpoint plus its credential source.
type JevAPI struct {
	Config Config
	Lookup KeyLookup
	Client HTTPDoer
}

// DefaultJevAPI builds the production API from the process environment.
func DefaultJevAPI() JevAPI {
	return JevAPI{
		Config: ConfigFromEnv(os.Getenv),
		Lookup: DefaultKeyLookup(),
		Client: &http.Client{},
	}
}

// truncatePayload applies the JEV_MAX_STATE_CHARS cap (TS: callJev's
// truncation). A cap that is not a usable positive number disables truncation:
// the TS `state.length > NaN` comparison is always false too, and a negative
// JavaScript slice would silently drop the tail, which is not worth porting.
func truncatePayload(payload string, maxStateChars float64) (string, *Truncation) {
	if math.IsNaN(maxStateChars) || math.IsInf(maxStateChars, 0) || maxStateChars < 0 {
		return payload, nil
	}
	runes := []rune(payload)
	if float64(len(runes)) <= maxStateChars {
		return payload, nil
	}
	used := int(maxStateChars)
	truncated := &Truncation{OriginalChars: len(runes), UsedChars: used}
	return fmt.Sprintf("%s\n\n[TRUNCATED: payload was %d characters, only the first %s were sent]",
		string(runes[:used]), truncated.OriginalChars, jsNum(maxStateChars)), truncated
}

// unwrapSystemOneBody extracts the `{ answers, usage, model }` carrier of a
// response (TS: unwrapSystemOneBody).
func unwrapSystemOneBody(body any) map[string]any {
	object, ok := isRecord(body)
	if !ok {
		return map[string]any{}
	}
	if _, ok := isRecord(object["answers"]); ok {
		return object
	}
	if result, ok := isRecord(object["result"]); ok {
		if _, ok := isRecord(result["answers"]); ok {
			return result
		}
		if inner, ok := isRecord(result["result"]); ok {
			if _, ok := isRecord(inner["answers"]); ok {
				return inner
			}
		}
		return result
	}
	return object
}

// validateAnswers checks that the upstream answers cover the requested question
// keys and that every answer is structurally valid (TS: validateAnswers).
// It returns the first failure as an error; the caller fails closed.
func validateAnswers(questions Questions, answers map[string]any) error {
	for _, question := range questions {
		answer, exists := answers[question.ID]
		if !exists || answer == nil {
			return fmt.Errorf("incomplete answers: missing key %q", question.ID)
		}
		criteria := make([]string, len(question.Criteria))
		for index, criterion := range question.Criteria {
			criteria[index] = criterion.Label
		}
		if err := validateAnswer(question.Type, question.ID, answer, criteria); err != nil {
			return err
		}
	}
	return nil
}

// validateAnswer checks one answer against its question type and the known
// labels of a choice/score question. Both transports (the direct call of this
// file and the registered classifier) share it, so one answer shape cannot be
// accepted on one path and rejected on the other. An unknown type fails closed.
func validateAnswer(questionType, id string, answer any, criteria []string) error {
	switch questionType {
	case "noul":
		if _, ok := probOf(answer); !ok {
			return fmt.Errorf("invalid noul answer for %q", id)
		}
	case "choice":
		choice, ok := choiceOf(answer)
		if !ok {
			return fmt.Errorf("invalid choice answer for %q", id)
		}
		if len(criteria) > 0 {
			known := false
			for _, label := range criteria {
				if label == choice.Choice {
					known = true
					break
				}
			}
			if !known {
				return fmt.Errorf("invalid choice answer for %q", id)
			}
		}
	case "score":
		object, isObject := isRecord(answer)
		score, hasScore := numberValue(object["score"])
		valid := isObject && hasScore && !math.IsNaN(score) && !math.IsInf(score, 0)
		if valid && len(criteria) > 0 {
			valid = score >= 0 && score <= float64(len(criteria)-1)
		}
		if !valid {
			return fmt.Errorf("invalid score answer for %q", id)
		}
	default:
		return fmt.Errorf("unsupported question type %q for %q", questionType, id)
	}
	return nil
}

// transportError restates a failed request the way the TS version reports it:
// the abort reason for a timeout or a caller cancellation, the transport error
// otherwise.
func transportError(err error, config Config) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timeout after %sms", jsNum(config.TimeoutMs))
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("aborted by caller")
	}
	return err
}

// Call posts one classifier request and returns the validated response
// (TS: callJev). ctx cancels the call; the configured timeout is applied on top.
func (api JevAPI) Call(ctx context.Context, payload string, questions Questions) (JevCall, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key, err := resolveKeyOrError(api.Config.BaseURL, api.Lookup)
	if err != nil {
		return JevCall{}, err
	}

	state, truncated := truncatePayload(payload, api.Config.MaxStateChars)
	body, err := json.Marshal(struct {
		Model     string    `json:"model"`
		State     string    `json:"state"`
		Questions Questions `json:"questions"`
	}{Model: api.Config.ModelID, State: state, Questions: questions})
	if err != nil {
		return JevCall{}, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, api.Config.Timeout)
	defer cancel()
	endpoint := api.Config.BaseURL + "/systemone"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return JevCall{}, err
	}
	request.Header.Set("authorization", "Bearer "+key)
	request.Header.Set("content-type", "application/json")

	client := api.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return JevCall{}, transportError(err, api.Config)
	}
	defer response.Body.Close()
	raw, err := readBoundedBody(response.Body)
	if err != nil {
		if errors.Is(err, errResponseBodyTooLarge) {
			return JevCall{}, err
		}
		return JevCall{}, transportError(err, api.Config)
	}
	rawText := string(raw)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return JevCall{}, fmt.Errorf("HTTP %d from %s: %s", response.StatusCode, endpoint, oneLine(rawText, 300))
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return JevCall{}, fmt.Errorf("non-JSON response from %s: %s", endpoint, oneLine(rawText, 200))
	}
	envelope, ok := isRecord(parsed)
	if !ok {
		return JevCall{}, fmt.Errorf("non-object JSON response from %s: %s", endpoint, oneLine(rawText, 200))
	}
	payloadBody := unwrapSystemOneBody(envelope)
	answers, ok := isRecord(payloadBody["answers"])
	if !ok || len(answers) == 0 {
		return JevCall{}, fmt.Errorf("response carried no answers: %s", oneLine(rawText, 200))
	}
	if err := validateAnswers(questions, answers); err != nil {
		return JevCall{}, err
	}

	usage := &JevUsage{}
	if values, ok := isRecord(payloadBody["usage"]); ok {
		usage.Input = coalesceNumber(values, "input_tokens", "prompt_tokens", "input")
		usage.Output = coalesceNumber(values, "output_tokens", "completion_tokens", "output")
	} else if values, ok := isRecord(envelope["usage"]); ok {
		usage.Input = coalesceNumber(values, "input_tokens", "prompt_tokens", "input")
		usage.Output = coalesceNumber(values, "output_tokens", "completion_tokens", "output")
	}

	model := ""
	if name, ok := payloadBody["model"].(string); ok {
		model = name
	} else if name, ok := envelope["model"].(string); ok {
		model = name
	}
	return JevCall{Answers: answers, Usage: usage, Model: model, Truncated: truncated}, nil
}

// coalesceNumber is `Number(u.a ?? u.b ?? u.c ?? 0) || 0`: the first present
// (non-null) member wins, and a value that is not a number counts as zero.
func coalesceNumber(values map[string]any, fields ...string) float64 {
	for _, field := range fields {
		raw, exists := values[field]
		if !exists || raw == nil {
			continue
		}
		number, ok := numberOrStringValue(raw)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			return 0
		}
		return number
	}
	return 0
}
