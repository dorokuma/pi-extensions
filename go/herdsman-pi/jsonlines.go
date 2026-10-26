package herdsmanpi

import (
	"errors"
	"fmt"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// DefaultMaxFrameBytes is the decoder's default maximum frame size, mirroring
// the TS default `1024 * 1024`.
const DefaultMaxFrameBytes = 1024 * 1024

// ErrFrameTooLarge is the sentinel wrapped by FrameTooLargeError.
var ErrFrameTooLarge = errors.New("JSON Lines frame exceeds maximum size")

// FrameTooLargeError mirrors the TS `JsonLineFrameTooLargeError`: the message
// text is byte-identical so a caller matching on it sees the same string.
type FrameTooLargeError struct {
	MaxBytes int
}

func (e *FrameTooLargeError) Error() string {
	return fmt.Sprintf("JSON Lines frame exceeds maximum size of %d bytes", e.MaxBytes)
}

// Unwrap lets callers test with errors.Is(err, ErrFrameTooLarge).
func (e *FrameTooLargeError) Unwrap() error { return ErrFrameTooLarge }

// EncodeJSONLine mirrors the TS `encodeJsonLine`: one JSON value plus a
// trailing newline.
func EncodeJSONLine(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded) + "\n", nil
}

// JsonLineDecoder mirrors the TS `JsonLineDecoder`, including the check order:
// the accumulated buffer is measured *before* the split, so an over-long frame
// fails even when it arrived with a terminator.
//
// The buffer is a byte slice, not a string, because the TS limit counts UTF-8
// bytes (`Buffer.byteLength`) and a chunk boundary may split a multi-byte rune.
type JsonLineDecoder struct {
	buffer   []byte
	maxBytes int
}

// NewJsonLineDecoder mirrors `new JsonLineDecoder(maxBytes = 1024 * 1024)`. A
// non-positive maxBytes is an error, as TS throws a RangeError.
func NewJsonLineDecoder(maxBytes int) (*JsonLineDecoder, error) {
	if maxBytes == 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	if maxBytes < 0 {
		return nil, errors.New("JSON Lines maximum frame size must be a positive integer")
	}
	return &JsonLineDecoder{maxBytes: maxBytes}, nil
}

// MaxBytes reports the configured frame limit.
func (d *JsonLineDecoder) MaxBytes() int { return d.maxBytes }

// Push appends a chunk and returns every complete frame it completed.
func (d *JsonLineDecoder) Push(chunk string) ([]any, error) {
	d.buffer = append(d.buffer, chunk...)
	if len(d.buffer) > d.maxBytes {
		return nil, &FrameTooLargeError{MaxBytes: d.maxBytes}
	}

	// Split on the last newline: everything before it is complete frames and
	// the remainder (possibly empty) stays buffered.
	last := -1
	for index := len(d.buffer) - 1; index >= 0; index-- {
		if d.buffer[index] == '\n' {
			last = index
			break
		}
	}
	if last < 0 {
		return nil, nil
	}
	complete := d.buffer[:last]
	d.buffer = append([]byte{}, d.buffer[last+1:]...)

	var values []any
	start := 0
	for index := 0; index <= len(complete); index++ {
		if index != len(complete) && complete[index] != '\n' {
			continue
		}
		line := complete[start:index]
		start = index + 1
		// TS filters empty lines out after the split.
		if len(line) == 0 {
			continue
		}
		value, err := decodeJSONValue(line)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// Flush returns the final unterminated frame, if any.
func (d *JsonLineDecoder) Flush() ([]any, error) {
	if len(d.buffer) == 0 {
		return nil, nil
	}
	line := d.buffer
	d.buffer = nil
	value, err := decodeJSONValue(line)
	if err != nil {
		return nil, err
	}
	return []any{value}, nil
}

// decodeJSONValue decodes one frame with the SDK's JSON implementation so the
// decoded shapes match what the rest of the extension sees.
func decodeJSONValue(line []byte) (any, error) {
	var value any
	if err := json.Unmarshal(line, &value); err != nil {
		return nil, err
	}
	return value, nil
}
