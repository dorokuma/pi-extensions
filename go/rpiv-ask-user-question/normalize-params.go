package rpivaskuserquestion

// Ports tool/normalize-params.ts (v2.15.0): line-terminator normalization
// (#192) and the prepareArguments de-double-encoding hook (#244).

import (
	"encoding/json"
	"strings"
)

// normalizeLineTerminators mirrors normalizeLineTerminators: CRLF becomes LF
// and a lone CR is deleted — never a space (phantom gaps inside words) and
// never an LF (vertical fragmentation).
func normalizeLineTerminators(text string) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "")
}

// normalizeQuestionParams returns a copy of params with every user-facing
// string field (`question`, `header`, `options[].label`, `.description`,
// `.preview`) line-terminator-normalized. Pure: the input is never mutated, and
// an absent preview stays absent.
func normalizeQuestionParams(params QuestionParams) QuestionParams {
	questions := make([]QuestionData, 0, len(params.Questions))
	for _, question := range params.Questions {
		options := make([]OptionData, 0, len(question.Options))
		for _, option := range question.Options {
			normalized := OptionData{
				Label:       normalizeLineTerminators(option.Label),
				Description: normalizeLineTerminators(option.Description),
			}
			if option.Preview != nil {
				preview := normalizeLineTerminators(*option.Preview)
				normalized.Preview = &preview
			}
			options = append(options, normalized)
		}
		questions = append(questions, QuestionData{
			Question:    normalizeLineTerminators(question.Question),
			Header:      normalizeLineTerminators(question.Header),
			Options:     options,
			MultiSelect: question.MultiSelect,
		})
	}
	return QuestionParams{Questions: questions}
}

// PrepareQuestionArguments mirrors prepareQuestionArguments: undo
// double-encoded nested arguments before schema validation. Only a string at an
// array/object position is decoded, and only when it parses to the expected
// shape; anything else passes through so validation reports the real problem.
func PrepareQuestionArguments(args map[string]any) (map[string]any, error) {
	if args == nil {
		return args, nil
	}
	questions, ok := decodeJSONArray(args["questions"])
	if !ok {
		return args, nil
	}
	decoded := make([]any, 0, len(questions))
	for _, raw := range questions {
		question, ok := decodeJSONObject(raw)
		if !ok {
			decoded = append(decoded, raw)
			continue
		}
		options, hasOptions := decodeJSONArray(question["options"])
		if !hasOptions {
			decoded = append(decoded, question)
			continue
		}
		decodedOptions := make([]any, 0, len(options))
		for _, option := range options {
			if decodedOption, ok := decodeJSONObject(option); ok {
				decodedOptions = append(decodedOptions, decodedOption)
				continue
			}
			decodedOptions = append(decodedOptions, option)
		}
		question["options"] = decodedOptions
		decoded = append(decoded, question)
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		out[key] = value
	}
	out["questions"] = decoded
	return out, nil
}

// decodeJSONArray unwraps a JSON string holding an array; anything else is
// returned unchanged with ok=false.
func decodeJSONArray(value any) ([]any, bool) {
	text, isString := value.(string)
	if !isString {
		array, ok := value.([]any)
		return array, ok
	}
	parsed, ok := parseJSONValue(text)
	if !ok {
		return nil, false
	}
	array, ok := parsed.([]any)
	return array, ok
}

// decodeJSONObject unwraps a JSON string holding an object; anything else is
// returned unchanged with ok=false.
func decodeJSONObject(value any) (map[string]any, bool) {
	text, isString := value.(string)
	if !isString {
		object, ok := value.(map[string]any)
		return object, ok
	}
	parsed, ok := parseJSONValue(text)
	if !ok {
		return nil, false
	}
	object, ok := parsed.(map[string]any)
	return object, ok
}

// parseJSONValue decodes one JSON document, reporting failure as ok=false.
func parseJSONValue(text string) (any, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

// decodeQuestionParams converts a decoded tool-arguments map into QuestionParams
// with the same structural tolerance the TS cast has: missing or wrong-typed
// fields decode to their zero values (`multiSelect` → false), and `preview`
// stays nil when absent. The returned params are ready for
// normalizeQuestionParams + validateQuestionnaire.
func decodeQuestionParams(raw map[string]any) QuestionParams {
	params := QuestionParams{}
	rawQuestions, ok := raw["questions"].([]any)
	if !ok {
		return params
	}
	for _, item := range rawQuestions {
		object, ok := item.(map[string]any)
		if !ok {
			params.Questions = append(params.Questions, QuestionData{})
			continue
		}
		question := QuestionData{}
		if text, ok := object["question"].(string); ok {
			question.Question = text
		}
		if text, ok := object["header"].(string); ok {
			question.Header = text
		}
		if flag, ok := object["multiSelect"].(bool); ok {
			question.MultiSelect = flag
		}
		if rawOptions, ok := object["options"].([]any); ok {
			for _, rawOption := range rawOptions {
				optionObject, ok := rawOption.(map[string]any)
				if !ok {
					question.Options = append(question.Options, OptionData{})
					continue
				}
				option := OptionData{}
				if text, ok := optionObject["label"].(string); ok {
					option.Label = text
				}
				if text, ok := optionObject["description"].(string); ok {
					option.Description = text
				}
				if text, ok := optionObject["preview"].(string); ok {
					option.Preview = &text
				}
				question.Options = append(question.Options, option)
			}
		}
		params.Questions = append(params.Questions, question)
	}
	return params
}

// encodeQuestionParams converts QuestionParams back to the map form the tool
// envelope and event payloads use.
func encodeQuestionParams(params QuestionParams) map[string]any {
	questions := make([]any, 0, len(params.Questions))
	for _, question := range params.Questions {
		options := make([]any, 0, len(question.Options))
		for _, option := range question.Options {
			entry := map[string]any{
				"label":       option.Label,
				"description": option.Description,
			}
			if option.Preview != nil {
				entry["preview"] = *option.Preview
			}
			options = append(options, entry)
		}
		questions = append(questions, map[string]any{
			"question":    question.Question,
			"header":      question.Header,
			"multiSelect": question.MultiSelect,
			"options":     options,
		})
	}
	return map[string]any{"questions": questions}
}
