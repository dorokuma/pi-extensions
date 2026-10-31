package sdk

// Defensive-depth regression tests for H10/H11. The nil-data shape behind H11
// is not reachable through ordinary host payloads — a non-map args body fails
// preparePromptOptions before dispatch, and a stream payload that decodes to a
// non-map is normalized to an empty map by event_stream.go — so the nil-map
// write guard in extension.go is belt-and-braces. The H10 SetTheme nil-result
// branch is likewise unreachable over the wire today (a host frame with no
// call_result body is the synthetic shape used below). These tests pin
// "no panic" regressions driven over a real net.Pipe conn; they do not claim
// a reproduced panic from a reachable host input.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// h11Host is the host end of a net.Pipe: it reads the frames the extension
// writes and answers them. Every read/write carries a deadline so a missing
// frame fails fast instead of hanging the test.
type h11Host struct {
	t    *testing.T
	conn net.Conn
}

// handshake pipes the extension, serves register→ready, and returns a stop
// that shuts the extension down. t.Cleanup closes the host end.
func handshake(t *testing.T, ext *Extension) (host *h11Host, stop func()) {
	t.Helper()
	hostEnd, extEnd := net.Pipe()
	host = &h11Host{t: t, conn: hostEnd}
	t.Cleanup(func() { hostEnd.Close() })
	done := make(chan error, 1)
	go func() { done <- ext.runWithConn(extEnd, nil, "") }()
	if reg := host.readFrame(); reg["register"] == nil {
		t.Fatalf("first frame is not a register frame: %#v", reg)
	}
	host.sendFrame(map[string]any{"type": "ready",
		"ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})
	return host, func() {
		host.sendFrame(map[string]any{"type": "shutdown"})
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("extension exited with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("extension did not stop on shutdown")
		}
	}
}

func (h *h11Host) readFrame() map[string]any {
	h.t.Helper()
	var header [4]byte
	h.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(h.conn, header[:]); err != nil {
		h.t.Fatalf("read frame header: %v", err)
	}
	data := make([]byte, binary.BigEndian.Uint32(header[:]))
	h.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(h.conn, data); err != nil {
		h.t.Fatalf("read frame body: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		h.t.Fatalf("decode frame: %v", err)
	}
	return msg
}

func (h *h11Host) sendFrame(msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		h.t.Fatalf("marshal frame: %v", err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	h.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := h.conn.Write(append(header[:], data...)); err != nil {
		h.t.Fatalf("write frame: %v", err)
	}
}

// readResponse returns the response frame for id (other frames are skipped; a
// missing one fails on the read deadline).
func (h *h11Host) readResponse(id string) map[string]any {
	h.t.Helper()
	for {
		msg := h.readFrame()
		if msg["type"] == "response" && msg["id"] == id {
			return msg
		}
	}
}

// frameError returns the response's error message, "" when the frame is not an
// error response.
func frameError(t *testing.T, resp map[string]any) string {
	t.Helper()
	respObj, _ := resp["response"].(map[string]any)
	if respObj == nil {
		t.Fatalf("response frame missing response object: %#v", resp)
	}
	if errInfo, _ := respObj["error"].(map[string]any); errInfo != nil {
		msg, _ := errInfo["message"].(string)
		return msg
	}
	return ""
}

// TestBeforeAgentStartNilDataNoPanic pins that a before_agent_start dispatch
// whose data map decoded to nil cannot reach the `data["systemPrompt"]` write:
// the SDK answers preparePromptOptions' error, not a panic recovery.
func TestBeforeAgentStartNilDataNoPanic(t *testing.T) {
	ext := New("h11-nil-data")
	ext.OnEvent("before_agent_start", func(ctx Context, data map[string]any) (any, error) {
		return nil, nil
	})
	host, stop := handshake(t, ext)
	defer stop()

	// args "null" decodes data to a nil map; preparePromptOptions rejects it
	// before the write the H11 guard covers.
	host.sendFrame(map[string]any{"type": "request", "id": "req-1",
		"request": map[string]any{
			"method":     "event",
			"event":      "before_agent_start",
			"handler_id": ext.eventNextID,
			"args":       json.RawMessage(`null`),
		}})
	if msg := frameError(t, host.readResponse("req-1")); strings.Contains(msg, "handler panicked") || strings.Contains(msg, "nil map") {
		t.Fatalf("before_agent_start with nil data panicked: %q", msg)
	}
}

// TestBeforeAgentStartStreamNilDataNoPanic exercises the stream-decoded shape:
// a keyframe payload that is not a map is normalized to an empty map by
// event_stream.go, so the dispatch never sees nil data. noPanic regression.
func TestBeforeAgentStartStreamNilDataNoPanic(t *testing.T) {
	ext := New("h11-nil-stream")
	ext.OnEvent("before_agent_start", func(ctx Context, data map[string]any) (any, error) {
		return nil, nil
	})
	host, stop := handshake(t, ext)
	defer stop()

	host.sendFrame(map[string]any{"type": "request", "id": "req-stream",
		"request": map[string]any{
			"method":     "event",
			"event":      "before_agent_start",
			"handler_id": ext.eventNextID,
			"args":       json.RawMessage(`["not","a","map"]`),
			"stream":     map[string]any{"seq": 1, "key": "k"},
		}})
	if msg := frameError(t, host.readResponse("req-stream")); strings.Contains(msg, "handler panicked") || strings.Contains(msg, "nil map") {
		t.Fatalf("streamed before_agent_start panicked on nil data: %q", msg)
	}
}

// TestSetThemeNilResultNoPanic pins H10: when the host answers a call with a
// frame that carries no call_result body (result == nil, err == nil), SetTheme
// must report a failure string instead of dereferencing the nil error.
func TestSetThemeNilResultNoPanic(t *testing.T) {
	ext := New("h10-nil-result")
	host, stop := handshake(t, ext)
	defer stop()

	resultCh := make(chan string, 1)
	go func() {
		ok, msg := (Context{ext: ext}).SetTheme("dark")
		resultCh <- fmt.Sprintf("ok=%v msg=%q", ok, msg)
	}()

	// Read the ui.setTheme call and answer it with no call_result body at all:
	// waitCall receives nil,nil.
	var callID string
	for {
		msg := host.readFrame()
		if call, _ := msg["call"].(map[string]any); call != nil && call["method"] == "ui.setTheme" {
			callID, _ = msg["id"].(string)
			break
		}
	}
	host.sendFrame(map[string]any{"type": "call_result", "id": callID})

	select {
	case out := <-resultCh:
		if !strings.Contains(out, "ok=false") {
			t.Fatalf("SetTheme result = %s, want ok=false", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetTheme did not return (possible panic or deadlock)")
	}
}
