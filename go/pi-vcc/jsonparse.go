package pivcc

import (
	"bytes"
	"encoding/json"
)

// decodeJSONObject parses JSON text into a generic object. ok is false for
// malformed input or a non-object value, matching a JS parse that throws (or
// yields something other than an object).
func decodeJSONObject(data []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, false
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	return normalizeJSONNumbers(object), true
}

// normalizeJSONNumbers rewrites json.Number values as float64 so the rest of the
// port can read numbers the way JS does (`typeof x === "number"`).
func normalizeJSONNumbers(value any) map[string]any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, item := range object {
		object[key] = normalizeJSONValue(item)
	}
	return object
}

func normalizeJSONValue(value any) any {
	switch value := value.(type) {
	case json.Number:
		if number, err := value.Float64(); err == nil {
			return number
		}
		return value.String()
	case map[string]any:
		for key, item := range value {
			value[key] = normalizeJSONValue(item)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = normalizeJSONValue(item)
		}
		return value
	}
	return value
}
