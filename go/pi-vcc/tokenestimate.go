package pivcc

import (
	"encoding/json"
	"math"
)

// Ports src/core/token-estimate.ts.

const (
	// DefaultCharsPerToken is token-estimate.ts DEFAULT_CHARS_PER_TOKEN.
	DefaultCharsPerToken = 4
	// MinCharsPerToken is token-estimate.ts MIN_CHARS_PER_TOKEN.
	MinCharsPerToken = 2
	// MaxCharsPerToken is token-estimate.ts MAX_CHARS_PER_TOKEN.
	MaxCharsPerToken = 6
	// ImageContentChars is token-estimate.ts IMAGE_CONTENT_CHARS: the chars
	// attributed to one image part (~1200 tokens at 4 chars/token).
	ImageContentChars = 4800
)

// TokenEstimateMode is token-estimate.ts TokenEstimateMode.
type TokenEstimateMode string

const (
	// ModeHeuristic is the fixed chars-per-token ratio.
	ModeHeuristic TokenEstimateMode = "heuristic"
	// ModeCalibrated is a ratio derived from the session's own numbers.
	ModeCalibrated TokenEstimateMode = "calibrated"
)

// TokenEstimateCalibration is token-estimate.ts TokenEstimateCalibration.
type TokenEstimateCalibration struct {
	Mode                TokenEstimateMode
	CharsPerToken       float64
	SourceChars         float64
	HasSourceChars      bool
	SourceTokens        float64
	HasSourceTokens     bool
	RawCharsPerToken    float64
	HasRawCharsPerToken bool
}

// CalibrateCharsPerToken is token-estimate.ts calibrateCharsPerToken().
func CalibrateCharsPerToken(sourceChars float64, sourceTokens float64, hasSourceTokens bool) TokenEstimateCalibration {
	heuristic := TokenEstimateCalibration{Mode: ModeHeuristic, CharsPerToken: DefaultCharsPerToken}
	if !hasSourceTokens || sourceTokens <= 0 || sourceChars <= 0 {
		return heuristic
	}
	raw := sourceChars / sourceTokens
	if math.IsInf(raw, 0) || math.IsNaN(raw) || raw <= 0 {
		return heuristic
	}
	clamped := raw
	if clamped < MinCharsPerToken {
		clamped = MinCharsPerToken
	}
	if clamped > MaxCharsPerToken {
		clamped = MaxCharsPerToken
	}
	return TokenEstimateCalibration{
		Mode:                ModeCalibrated,
		CharsPerToken:       clamped,
		SourceChars:         sourceChars,
		HasSourceChars:      true,
		SourceTokens:        sourceTokens,
		HasSourceTokens:     true,
		RawCharsPerToken:    raw,
		HasRawCharsPerToken: true,
	}
}

// EstimateTokensFromChars is token-estimate.ts estimateTokensFromChars():
// Math.ceil(chars / charsPerToken).
func EstimateTokensFromChars(chars, charsPerToken float64) int {
	if charsPerToken <= 0 {
		charsPerToken = DefaultCharsPerToken
	}
	return int(math.Ceil(chars / charsPerToken))
}

// safeJSONStringify is token-estimate.ts safeJSONStringify(): JSON text, or ""
// when the value cannot be encoded (TS catches a circular-structure throw).
func safeJSONStringify(value any) string {
	if value == nil {
		return `""`
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

// EstimateMessageContentChars is token-estimate.ts
// estimateMessageContentChars(): the char length of a message's content, for
// both the string and content-parts shapes.
func EstimateMessageContentChars(content any) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return jsLen(value)
	case []any:
		total := 0
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch kind, _ := part["type"].(string); kind {
			case "text":
				if text, ok := part["text"].(string); ok {
					total += jsLen(text)
				}
			case "thinking":
				if thinking, ok := part["thinking"].(string); ok {
					total += jsLen(thinking)
				}
			case "toolCall":
				args := part["arguments"]
				if args == nil {
					args = part["input"]
				}
				argLength := 0
				if text, ok := args.(string); ok {
					argLength = jsLen(text)
				} else {
					argLength = jsLen(safeJSONStringify(args))
				}
				name, _ := part["name"].(string)
				total += jsLen(name) + argLength
			case "toolResult":
				if text, ok := part["content"].(string); ok {
					total += jsLen(text)
				} else {
					total += jsLen(safeJSONStringify(part["content"]))
				}
			case "image":
				total += ImageContentChars
			default:
				// Unknown part: fall back to any text field so we never undercount.
				if text, ok := part["text"].(string); ok {
					total += jsLen(text)
				}
			}
		}
		return total
	}
	return 0
}

// EstimateMessageContentTokens is token-estimate.ts
// estimateMessageContentTokens().
func EstimateMessageContentTokens(content any, charsPerToken float64) int {
	return EstimateTokensFromChars(float64(EstimateMessageContentChars(content)), charsPerToken)
}
