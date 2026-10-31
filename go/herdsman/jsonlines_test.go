package herdsman

// Port of test/unit/json-lines.test.ts (7 cases).

import (
	"errors"
	"strings"
	"testing"
)

func TestEncodeJSONLineFramesOneValuePerNewline(t *testing.T) {
	encoded, err := EncodeJSONLine(map[string]any{"id": 1, "method": "agent.events"})
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if encoded != `{"id":1,"method":"agent.events"}`+"\n" {
		t.Fatalf("unexpected frame %q", encoded)
	}
}

func TestDecoderHandlesFramesSplitAcrossChunks(t *testing.T) {
	decoder, err := NewJsonLineDecoder(0)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	if first, err := decoder.Push(`{"id":1`); err != nil || len(first) != 0 {
		t.Fatalf("first push returned %v, %v", first, err)
	}
	second, err := decoder.Push("}\n{\"id\":2}\n")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(second))
	}
	assertJSONEquals(t, second[0], map[string]any{"id": float64(1)})
	assertJSONEquals(t, second[1], map[string]any{"id": float64(2)})
}

func TestDecoderRejectsFramesAboveConfiguredLimit(t *testing.T) {
	decoder, err := NewJsonLineDecoder(4)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	_, err = decoder.Push("12345")
	if err == nil || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("expected a maximum-size failure, got %v", err)
	}
}

func TestDecoderRejectsUnterminatedFrameAboveDefaultLimit(t *testing.T) {
	decoder, err := NewJsonLineDecoder(0)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	_, err = decoder.Push(strings.Repeat("1", 1024*1024+1))
	var tooLarge *FrameTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected FrameTooLargeError, got %v", err)
	}
}

func TestDecoderParsesFrameJustBelowDefaultLimit(t *testing.T) {
	decoder, err := NewJsonLineDecoder(0)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	value := strings.Repeat("a", 1024*1024-20)
	encoded, err := EncodeJSONLine(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frames, err := decoder.Push(encoded)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(frames) != 1 || frames[0] != value {
		t.Fatalf("unexpected frames: %d", len(frames))
	}
}

func TestDecoderRejectsFrameExceedingLimitAcrossChunks(t *testing.T) {
	decoder, err := NewJsonLineDecoder(0)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	if _, err := decoder.Push(strings.Repeat("1", 512*1024)); err != nil {
		t.Fatalf("first push: %v", err)
	}
	_, err = decoder.Push(strings.Repeat("1", 512*1024+1))
	var tooLarge *FrameTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected FrameTooLargeError, got %v", err)
	}
}

func TestDecoderFlushesFinalFrameWithoutNewline(t *testing.T) {
	decoder, err := NewJsonLineDecoder(0)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	if frames, err := decoder.Push(`{"id":1}`); err != nil || len(frames) != 0 {
		t.Fatalf("push returned %v, %v", frames, err)
	}
	flushed, err := decoder.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(flushed) != 1 {
		t.Fatalf("expected 1 flushed frame, got %d", len(flushed))
	}
	assertJSONEquals(t, flushed[0], map[string]any{"id": float64(1)})
}

// assertJSONEquals compares a decoded JSON value against an expected shape.
func assertJSONEquals(t *testing.T, got any, want map[string]any) {
	t.Helper()
	record, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("decoded value is %#v", got)
	}
	if len(record) != len(want) {
		t.Fatalf("shape mismatch: %#v", record)
	}
	for key, expected := range want {
		if record[key] != expected {
			t.Fatalf("key %s: got %#v want %#v", key, record[key], expected)
		}
	}
}
