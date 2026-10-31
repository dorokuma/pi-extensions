package sdk

// Regression test for H11: a before_agent_start event dispatched with a nil
// decoded data map must not panic on the `data["systemPrompt"] = ...` write
// (assignment to entry in nil map). The dispatch is driven over a real
// net.Pipe conn so the recover boundary and respond path are the real ones.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// h11Host reads frames off one end of a net.Pipe and captures the response the
// extension writes for the event request.
type h11Host struct {
	t    *testing.T
	conn net.Conn
}

func newH11Host(t *testing.T) (*h11Host, net.Conn) {
	t.Helper()
	hostEnd, extEnd := net.Pipe()
	return &h11Host{t: t, conn: hostEnd}, extEnd
}

func (h *h11Host) readFrame() map[string]any {
	h.t.Helper()
	var header [4]byte
	if _, err := readFullH11(h.conn, header[:]); err != nil {
		h.t.Fatalf("read frame header: %v", err)
	}
	data := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := readFullH11(h.conn, data); err != nil {
		h.t.Fatalf("read frame body: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		h.t.Fatalf("decode frame: %v", err)
	}
	return msg
}

func readFullH11(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestBeforeAgentStartNilDataNoPanic sends a before_agent_start event request
// whose args decode to a nil map. The SDK must answer with an error response
// (its preparePromptOptions guard), not a "handler panicked" panic recovery.
func TestBeforeAgentStartNilDataNoPanic(t *testing.T) {
	host, extEnd := newH11Host(t)
	defer host.conn.Close()

	ext := New("h11-nil-data")
	var handlerID int
	ext.OnEvent("before_agent_start", func(ctx Context, data map[string]any) (any, error) {
		return nil, nil
	})
	// The registered handler id the host will dispatch on.
	handlerID = ext.eventNextID

	done := make(chan error, 1)
	go func() { done <- ext.runWithConn(extEnd, nil, "") }()

	// Read the register frame.
	reg := host.readFrame()
	if _, ok := reg["register"]; !ok {
		t.Fatalf("first frame is not a register frame: %#v", reg)
	}
	// Ready so the extension leaves its wait.
	host.sendFrame(map[string]any{"type": "ready", "ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})

	// Fire the event with args that decode to nil ("null").
	host.sendFrame(map[string]any{
		"type": "request",
		"id":   "req-1",
		"request": map[string]any{
			"method":     "event",
			"event":      "before_agent_start",
			"handler_id": handlerID,
			"args":       json.RawMessage(`null`),
		},
	})

	// Expect a response frame for req-1 that is an error, not a panic.
	deadline := time.Now().Add(5 * time.Second)
	var resp map[string]any
	for time.Now().Before(deadline) {
		msg := host.readFrame()
		if msg["type"] == "response" && msg["id"] == "req-1" {
			resp = msg
			break
		}
	}
	if resp == nil {
		t.Fatal("no response frame for the before_agent_start request")
	}
	respObj, _ := resp["response"].(map[string]any)
	if respObj == nil {
		t.Fatalf("response frame missing response object: %#v", resp)
	}
	errInfo, _ := respObj["error"].(map[string]any)
	if errInfo == nil {
		t.Fatalf("expected an error response, got: %#v", respObj)
	}
	msg, _ := errInfo["message"].(string)
	if strings.Contains(msg, "handler panicked") || strings.Contains(msg, "nil map") {
		t.Fatalf("before_agent_start with nil data panicked: %q", msg)
	}

	// Shutdown so runWithConn returns.
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

// TestBeforeAgentStartStreamNilDataNoPanic exercises the stream-decoded path of
// H11: a before_agent_start event delivered as an event stream whose decoded
// payload is not a map leaves req.streamData nil, and the dispatch must not
// panic writing data["systemPrompt"].
func TestBeforeAgentStartStreamNilDataNoPanic(t *testing.T) {
	host, extEnd := newH11Host(t)
	defer host.conn.Close()

	ext := New("h11-nil-stream")
	ext.OnEvent("before_agent_start", func(ctx Context, data map[string]any) (any, error) {
		return nil, nil
	})
	handlerID := ext.eventNextID

	done := make(chan error, 1)
	go func() { done <- ext.runWithConn(extEnd, nil, "") }()

	reg := host.readFrame()
	if _, ok := reg["register"]; !ok {
		t.Fatalf("first frame is not a register frame: %#v", reg)
	}
	host.sendFrame(map[string]any{"type": "ready", "ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})

	// Stream keyframe whose args decode to a JSON array — cloneStreamValue does
	// not produce a map, so streamData stays nil (the H11 nil-map shape).
	host.sendFrame(map[string]any{
		"type": "request",
		"id":   "req-stream",
		"request": map[string]any{
			"method":     "event",
			"event":      "before_agent_start",
			"handler_id": handlerID,
			"args":       json.RawMessage(`["not","a","map"]`),
			"stream":     map[string]any{"seq": 1, "key": "k"},
		},
	})

	deadline := time.Now().Add(5 * time.Second)
	var resp map[string]any
	for time.Now().Before(deadline) {
		msg := host.readFrame()
		if msg["type"] == "response" && msg["id"] == "req-stream" {
			resp = msg
			break
		}
	}
	if resp == nil {
		t.Fatal("no response frame for the streamed before_agent_start request")
	}
	respObj, _ := resp["response"].(map[string]any)
	if respObj == nil {
		t.Fatalf("response frame missing response object: %#v", resp)
	}
	if errInfo, _ := respObj["error"].(map[string]any); errInfo != nil {
		msg, _ := errInfo["message"].(string)
		if strings.Contains(msg, "handler panicked") || strings.Contains(msg, "nil map") {
			t.Fatalf("streamed before_agent_start panicked on nil data: %q", msg)
		}
	}

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

func (h *h11Host) sendFrame(msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		h.t.Fatalf("marshal frame: %v", err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := h.conn.Write(append(header[:], data...)); err != nil {
		h.t.Fatalf("write frame: %v", err)
	}
}

// TestSetThemeNilResultNoPanic pins H10: when the host answers a call with a
// frame that carries no call_result body (result == nil, err == nil), SetTheme
// must report a failure string instead of dereferencing the nil error.
func TestSetThemeNilResultNoPanic(t *testing.T) {
	host, extEnd := newH11Host(t)
	defer host.conn.Close()

	ext := New("h10-nil-result")
	done := make(chan error, 1)
	go func() { done <- ext.runWithConn(extEnd, nil, "") }()

	reg := host.readFrame()
	if _, ok := reg["register"]; !ok {
		t.Fatalf("first frame is not a register frame: %#v", reg)
	}
	host.sendFrame(map[string]any{"type": "ready", "ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})

	ctx := Context{ext: ext}
	resultCh := make(chan string, 1)
	go func() {
		ok, msg := ctx.SetTheme("dark")
		resultCh <- fmt.Sprintf("ok=%v msg=%q", ok, msg)
	}()

	// Read the ui.setTheme call and answer it with a frame that has NO
	// call_result payload at all: waitCall receives nil,nil.
	var callID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.readFrame()
		if msg["type"] == "call" {
			call := msg["call"].(map[string]any)
			if call["method"] == "ui.setTheme" {
				callID, _ = msg["id"].(string)
				break
			}
		}
	}
	if callID == "" {
		t.Fatal("extension never issued the ui.setTheme call")
	}
	// call_result envelope with an absent CallResult body.
	host.sendFrame(map[string]any{"type": "call_result", "id": callID})

	select {
	case out := <-resultCh:
		if !strings.Contains(out, "ok=false") {
			t.Fatalf("SetTheme result = %s, want ok=false", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetTheme did not return (possible panic or deadlock)")
	}

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
