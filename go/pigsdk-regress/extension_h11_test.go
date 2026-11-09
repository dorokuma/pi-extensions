package pigsdkregress

// Defensive-depth regression tests for the before_agent_start nil-data shape
// and H10, migrated out of go/pigsdk (sdk-check requires the mirror to stay
// byte-identical to the staged SDK plus declared local patches, so the tests
// moved here). The nil-data shape is not reachable through ordinary host
// payloads: a non-map args body fails preparePromptOptions before dispatch,
// and a stream payload that decodes to a non-map is normalized to an empty
// map by event_stream.go — so the nil-map write guard once kept in
// extension.go (H11) was unreachable defense and has been removed again
// (preparePromptOptions errors first on every reachable path; the guard
// could never run). These tests pin "no panic, upstream error response"
// regressions driven over a real net.Pipe conn; they do not claim a
// reproduced panic from a reachable host input. The H10 SetTheme nil-result
// branch is likewise unreachable over the wire today (a host frame with no
// call_result body is the synthetic shape used below).
//
// The file also carries the MCP disconnect and lifetime-watcher regression
// block, merged in from the former mcp_disconnect_test.go; that block's own
// header below explains the scenario and the two disciplines that keep it
// honest.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	sdkjson "github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// h11Host is the host end of a net.Pipe: it reads the frames the extension
// writes and answers them. Every read/write carries a deadline so a missing
// frame fails fast instead of hanging the test.
type h11Host struct {
	t    *testing.T
	conn net.Conn
}

// handshake pipes the extension, serves register→ready, and returns the host
// end, the parsed register frame (which carries the declared handlers and
// their handler_ids, replacing the old in-package read of ext.eventNextID),
// and a stop that shuts the extension down. t.Cleanup closes the host end.
func handshake(t *testing.T, ext *sdk.Extension) (host *h11Host, register map[string]any, stop func()) {
	t.Helper()
	hostEnd, extEnd := net.Pipe()
	host = &h11Host{t: t, conn: hostEnd}
	t.Cleanup(func() { hostEnd.Close() })
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(extEnd) }()
	reg := host.readFrame()
	if reg["register"] == nil {
		t.Fatalf("first frame is not a register frame: %#v", reg)
	}
	register, _ = reg["register"].(map[string]any)
	host.sendFrame(map[string]any{"type": "ready",
		"ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})
	return host, register, func() {
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

// handlerID returns the handler_id the register frame declared for event.
func handlerID(t *testing.T, register map[string]any, event string) int {
	t.Helper()
	handlers, _ := register["handlers"].([]any)
	for _, h := range handlers {
		def, _ := h.(map[string]any)
		if def["event"] == event {
			id, ok := def["handler_id"].(float64)
			if !ok {
				t.Fatalf("register handler for %q has no handler_id: %#v", event, def)
			}
			return int(id)
		}
	}
	t.Fatalf("register frame has no handler for %q: %#v", event, register["handlers"])
	return 0
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
// whose data map decoded to nil gets upstream's preparePromptOptions error
// ("before_agent_start requires systemPromptOptions") before the
// data["systemPrompt"] write — not a panic. The removed H11 guard could never
// run on this path because preparePromptOptions rejects a nil data map first.
func TestBeforeAgentStartNilDataNoPanic(t *testing.T) {
	ext := sdk.New("h11-nil-data")
	ext.OnEvent("before_agent_start", func(ctx sdk.Context, data map[string]any) (any, error) {
		return nil, nil
	})
	host, register, stop := handshake(t, ext)
	defer stop()

	// args "null" decodes data to a nil map; preparePromptOptions rejects it
	// before the write the H11 guard covers.
	host.sendFrame(map[string]any{"type": "request", "id": "req-1",
		"request": map[string]any{
			"method":     "event",
			"event":      "before_agent_start",
			"handler_id": handlerID(t, register, "before_agent_start"),
			"args":       json.RawMessage(`null`),
		}})
	if msg := frameError(t, host.readResponse("req-1")); !strings.Contains(msg, "systemPromptOptions") || strings.Contains(msg, "handler panicked") || strings.Contains(msg, "nil map") {
		t.Fatalf("before_agent_start with nil data: want preparePromptOptions error, got %q", msg)
	}
}

// TestBeforeAgentStartStreamNilDataNoPanic exercises the stream-decoded shape:
// a keyframe payload that is not a map is normalized to an empty map by
// event_stream.go, so the dispatch never sees nil data — the H11 nil-map
// branch was unreachable on this path too and has been removed. noPanic
// regression.
func TestBeforeAgentStartStreamNilDataNoPanic(t *testing.T) {
	ext := sdk.New("h11-nil-stream")
	ext.OnEvent("before_agent_start", func(ctx sdk.Context, data map[string]any) (any, error) {
		return nil, nil
	})
	host, register, stop := handshake(t, ext)
	defer stop()

	host.sendFrame(map[string]any{"type": "request", "id": "req-stream",
		"request": map[string]any{
			"method":     "event",
			"event":      "before_agent_start",
			"handler_id": handlerID(t, register, "before_agent_start"),
			"args":       json.RawMessage(`["not","a","map"]`),
			"stream":     map[string]any{"seq": 1, "key": "k"},
		}})
	if msg := frameError(t, host.readResponse("req-stream")); msg == "" || strings.Contains(msg, "handler panicked") || strings.Contains(msg, "nil map") {
		t.Fatalf("streamed before_agent_start: want upstream decode/prepare error, got %q", msg)
	}
}

// TestSetThemeNilResultNoPanic pins H10: when the host answers a call with a
// frame that carries no call_result body (result == nil, err == nil), SetTheme
// must report a failure string instead of dereferencing the nil error. The
// Context is captured inside an event handler — the public API has no
// constructor for a connected Context.
func TestSetThemeNilResultNoPanic(t *testing.T) {
	ext := sdk.New("h10-nil-result")
	var ctx sdk.Context
	gotCtx := make(chan struct{}, 1)
	ext.OnEvent("session_start", func(c sdk.Context, data map[string]any) (any, error) {
		ctx = c
		select {
		case gotCtx <- struct{}{}:
		default:
		}
		return nil, nil
	})
	host, register, stop := handshake(t, ext)
	defer stop()

	host.sendFrame(map[string]any{"type": "request", "id": "req-start",
		"request": map[string]any{
			"method":     "event",
			"event":      "session_start",
			"handler_id": handlerID(t, register, "session_start"),
			"args":       json.RawMessage(`{}`),
		}})
	host.readResponse("req-start")
	select {
	case <-gotCtx:
	case <-time.After(5 * time.Second):
		t.Fatal("session_start handler did not capture the Context")
	}

	resultCh := make(chan string, 1)
	go func() {
		ok, msg := ctx.SetTheme("dark")
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

// Disconnect regression tests for the MCP extension session-graph callbacks
// (go/pigsdk/mcp_extension_options.go, a declared PIGSDK_LOCAL_PATCHES entry).
// The staged SDK's receive poll, send context and credentials refresh-lock
// waits outlive the host connection: with the host gone they hold the
// request's handler goroutine (or a detached WithRefreshLock goroutine)
// forever, so stopRequests hits its shutdown deadline or the lock leaks. The
// staged SDK also never closes a transport on disconnect: its callback id
// stays registered, its listeners stay attached and the events the transport
// still emits keep growing the queue. Each test below parks one of those
// waits over a real net.Pipe, drops the host end, and requires the wait to
// release (and, for a transport, Close to run). Against the unpatched
// snapshot every test fails: RunWithConn returns "extension handlers did not
// stop before the shutdown deadline" (≈2s), WithRefreshLock never returns,
// or the transport's Close never runs.
//
// TestMcpTransportCloseStopsWatcher exercises the complementary shape: the
// host closes the transport over a still-live connection. The shim's
// lifetime watcher must then exit via release's done signal; otherwise one
// watcher goroutine leaks per create/close cycle (and the transport must
// not be closed a second time when the connection finally drops).
//
// Two disciplines keep the scenario honest:
//   - Once a test's target request is in flight, a pump goroutine keeps
//     reading every frame the extension writes. net.Pipe writes block until
//     read, so an unread request_state/response frame stalls the handler
//     before it reaches the wait and the test would silently cover "entered
//     after the connection died" instead of "woken from the wait".
//   - The host end is dropped only after the wait is observed parked:
//     either the test double's own channel (a Send entered, a held lock's fn
//     waiting) or, for a wait inside the SDK, the handler goroutine's stack
//     showing the function parked in its select (waitParked). No sleeps
//     stand in for that synchronization.
// Hangs the tests inject run on test-controlled channels, so cleanup unblocks
// them and waits for the goroutines to exit; nothing outlives the test.
// Only exported API is used, so the same source compiles against both the
// patched SDK and the staged snapshot.

// mcpHandshake is handshake without the stop helper: these tests drop the
// host end themselves and observe RunWithConn's return on done.
func mcpHandshake(t *testing.T, ext *sdk.Extension) (host *h11Host, register map[string]any, done chan error) {
	t.Helper()
	hostEnd, extEnd := net.Pipe()
	host = &h11Host{t: t, conn: hostEnd}
	t.Cleanup(func() { hostEnd.Close() })
	done = make(chan error, 1)
	go func() { done <- ext.RunWithConn(extEnd) }()
	reg := host.readFrame()
	if reg["register"] == nil {
		t.Fatalf("first frame is not a register frame: %#v", reg)
	}
	register, _ = reg["register"].(map[string]any)
	host.sendFrame(map[string]any{"type": "ready",
		"ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})
	return host, register, done
}

// serveLoaderNew answers the sessionGraph.new call the extension's
// NewDefaultResourceLoader makes, and returns the MCP option wire map that
// carries the callback ids (createTransport, credentials, ...).
func serveLoaderNew(t *testing.T, host *h11Host) map[string]any {
	t.Helper()
	for {
		msg := host.readFrame()
		call, _ := msg["call"].(map[string]any)
		if call == nil || call["method"] != "sessionGraph.new" {
			continue
		}
		callArgs, _ := call["args"].(map[string]any)
		list, _ := callArgs["args"].([]any)
		if len(list) == 0 {
			t.Fatalf("sessionGraph.new call has no args: %#v", callArgs)
		}
		wire, _ := list[0].(map[string]any)
		factories, _ := wire["extensionFactories"].([]any)
		if len(factories) == 0 {
			t.Fatalf("sessionGraph.new wire has no extensionFactories: %#v", wire)
		}
		factory, _ := factories[0].(map[string]any)
		options, _ := factory["options"].(map[string]any)
		host.sendFrame(map[string]any{"type": "call_result", "id": msg["id"],
			"call_result": map[string]any{"result": map[string]any{"handle": "L1"}}})
		return options
	}
}

// sendCallback sends a sessionGraph.callback request running callback cb
// with the given arguments, under request id id.
func sendCallback(host *h11Host, id string, cb any, args ...any) {
	host.sendFrame(map[string]any{"type": "request", "id": id,
		"request": map[string]any{"method": "sessionGraph.callback",
			"args": map[string]any{"id": cb, "args": args}}})
}

// responseResult returns the decoded result object of a response frame.
func responseResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	respObj, _ := resp["response"].(map[string]any)
	if respObj == nil {
		t.Fatalf("response frame missing response object: %#v", resp)
	}
	if errInfo, _ := respObj["error"].(map[string]any); errInfo != nil {
		t.Fatalf("response is an error: %#v", errInfo)
	}
	result, _ := respObj["result"].(map[string]any)
	if result == nil {
		t.Fatalf("response has no result object: %#v", respObj)
	}
	return result
}

// startMCPExtension brings up an extension whose session_start handler builds
// a DefaultResourceLoader with CreateMcpExtension(opts), and returns the host
// end, RunWithConn's result channel and the MCP callback wire map.
func startMCPExtension(t *testing.T, opts sdk.McpExtensionOptions) (host *h11Host, done chan error, options map[string]any) {
	t.Helper()
	loaderErr := make(chan error, 1)
	ext := sdk.New("mcp-disconnect")
	ext.OnEvent("session_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
		_, err := sdk.NewDefaultResourceLoader(ctx, sdk.DefaultResourceLoaderOptions{
			Cwd:                t.TempDir(),
			AgentDir:           t.TempDir(),
			ExtensionFactories: []any{sdk.CreateMcpExtension(opts)},
		})
		loaderErr <- err
		return nil, nil
	})
	host, register, done := mcpHandshake(t, ext)
	host.sendFrame(map[string]any{"type": "request", "id": "req-start",
		"request": map[string]any{
			"method":     "event",
			"event":      "session_start",
			"handler_id": handlerID(t, register, "session_start"),
			"args":       json.RawMessage(`{}`),
		}})
	options = serveLoaderNew(t, host)
	if msg := frameError(t, host.readResponse("req-start")); msg != "" {
		t.Fatalf("session_start failed: %s", msg)
	}
	if err := <-loaderErr; err != nil {
		t.Fatalf("NewDefaultResourceLoader: %v", err)
	}
	return host, done, options
}

// hostPump drains every frame the extension writes so a frame the test does
// not consume (a request_state, a response whose callback is still parked)
// never stalls a handler on net.Pipe's write-until-read. Start it before
// sending the request whose parked wait the test observes; stop() closes the
// host end and joins the reader.
type hostPump struct {
	host   *h11Host
	frames chan map[string]any
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
}

// startPump begins draining host.conn on a goroutine. Frames are delivered
// on p.frames in arrival order; a frame is never lost because p.frames is
// full (the reader parks, it does not drop). The host end is closed on
// stop(), not by the pump: dropping the connection mid-test is the test's
// own step.
func startPump(host *h11Host) *hostPump {
	p := &hostPump{
		host:   host,
		frames: make(chan map[string]any, 16),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		for {
			msg, ok := readFrameOK(host.conn)
			if !ok {
				return
			}
			select {
			case p.frames <- msg:
			case <-p.quit:
				return
			}
		}
	}()
	return p
}

// readFrameOK reads one length-prefixed frame like h11Host.readFrame, but
// reports read errors instead of failing the test: the pump runs on its own
// goroutine (where t.Fatal is illegal) and reads on a connection the test
// closes. No deadline — a closed connection ends a blocked read.
func readFrameOK(conn net.Conn) (map[string]any, bool) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, false
	}
	data := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, false
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, false
	}
	return msg, true
}

// next returns the next drained frame, or fails after the timeout. Tests
// that only need a specific frame use waitResponse/skipUntil instead.
func (p *hostPump) next(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case msg, ok := <-p.frames:
		if !ok {
			t.Fatal("frame pump ended")
		}
		return msg
	case <-time.After(timeout):
		t.Fatal("pump delivered no frame")
		return nil
	}
}

// skipUntil returns the first frame pred accepts, consuming the ones before
// it (request_state notices, other responses, ...).
func (p *hostPump) skipUntil(t *testing.T, pred func(map[string]any) bool, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-p.frames:
			if !ok {
				t.Fatal("frame pump ended")
			}
			if pred(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("no matching frame arrived")
			return nil
		}
	}
}

// waitResponse returns the response frame for id, skipping other frames.
func (p *hostPump) waitResponse(t *testing.T, id string) map[string]any {
	t.Helper()
	return p.skipUntil(t, func(m map[string]any) bool {
		return m["type"] == "response" && m["id"] == id
	}, 5*time.Second)
}

// stop closes the host end and joins the reader. The net.Pipe half a
// handshake Cleanup also closes is closed here too (Close is idempotent).
func (p *hostPump) stop() {
	p.once.Do(func() {
		close(p.quit)
		p.host.conn.Close()
		<-p.done
	})
}

// waitParked blocks until some goroutine's stack holds include while every
// frame stack that also holds it does not hold exclude — i.e. a goroutine
// parked inside the function include names (for the disconnect tests: the
// SDK wait the test targets). This is the deterministic signal that the
// wait has begun before the test drops the connection: without it the drop
// can precede the wait and the test covers "entered after disconnect"
// instead of "woken by disconnect". Stack inspection is the only handle the
// wire does not give, so the tests pin the function names of the local
// patch; the same source still runs against the staged snapshot because the
// parked function names are identical there (the patch adds select cases,
// it does not rename the functions). Polling yields the scheduler, it does
// not sleep on a timer.
func waitParked(t *testing.T, include, exclude string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<22)
	for {
		n := runtime.Stack(buf, true)
		for _, block := range strings.Split(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(block, "\n")
			if !strings.Contains(header, "[select") && !strings.Contains(header, "[chan") {
				continue
			}
			if !strings.Contains(block, include) {
				continue
			}
			if exclude != "" && strings.Contains(block, exclude) {
				continue
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no goroutine parked in %s", include)
		}
		runtime.Gosched()
	}
}

// waitGone is waitParked's complement for leak assertions: it blocks until no
// goroutine's stack mentions needle at all — i.e. the goroutine running it
// exited. The assertion itself is still stack inspection (no exported handle
// names the watcher), and polling only yields the scheduler.
func waitGone(t *testing.T, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<22)
	for {
		n := runtime.Stack(buf, true)
		if !strings.Contains(string(buf[:n]), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine still present: %s", needle)
		}
		runtime.Gosched()
	}
}

// requireCleanRun asserts RunWithConn returns nil: every handler released
// before the shutdown deadline. The unpatched waits hold their goroutine past
// it, so RunWithConn returns the shutdown-deadline error instead.
func requireCleanRun(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWithConn after disconnect: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunWithConn did not return after the host disconnected")
	}
}

// mcpDisconnectTransport emits nothing; its Send parks on the send context so
// a send callback stays in flight until ctx ends. closedCh reports that
// Close ran: the patched shim closes the transport when the connection ends;
// the unpatched one never does. closeCalls counts Close invocations so a test
// can assert the host's close op and a later disconnect release run Close
// exactly once in total.
type mcpDisconnectTransport struct {
	sdk.McpTransportEvents
	sendStarted chan struct{}
	closeOnce   sync.Once
	closedCh    chan struct{}
	closeCalls  atomic.Int32
}

func newMcpDisconnectTransport() *mcpDisconnectTransport {
	return &mcpDisconnectTransport{sendStarted: make(chan struct{}, 1), closedCh: make(chan struct{})}
}

func (m *mcpDisconnectTransport) Start() error { return nil }
func (m *mcpDisconnectTransport) Close() error {
	m.closeCalls.Add(1)
	m.closeOnce.Do(func() { close(m.closedCh) })
	return nil
}
func (m *mcpDisconnectTransport) Send(ctx context.Context, _ sdkjson.RawMessage) error {
	select {
	case m.sendStarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// requireTransportClosed asserts the disconnect closed the transport. The
// unpatched shim ends its receive poll on disconnect but leaves the
// transport, its listeners and its registry entry behind, so Close never
// runs and this fails before RunWithConn is even reached.
func requireTransportClosed(t *testing.T, tr *mcpDisconnectTransport) {
	t.Helper()
	select {
	case <-tr.closedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("transport Close did not run after the host disconnected")
	}
}

// createTransport runs the createTransport callback and returns the transport
// callback id its response names.
func createTransport(t *testing.T, p *hostPump, host *h11Host, options map[string]any) string {
	t.Helper()
	cb, _ := options["createTransport"].(string)
	if cb == "" {
		t.Fatalf("MCP wire options have no createTransport callback: %#v", options)
	}
	sendCallback(host, "r-ct", cb,
		map[string]any{"name": "s", "config": map[string]any{}}, t.TempDir(), "")
	result := responseResult(t, p.waitResponse(t, "r-ct"))
	transportID, _ := result["transport"].(string)
	if transportID == "" {
		t.Fatalf("createTransport response names no transport callback: %#v", result)
	}
	return transportID
}

// TestMcpReceiveReleasesOnDisconnect parks the transport's receive long poll
// and drops the host connection. Patched, the poll releases on the serve
// lifetime and reports a close event, and the shim's release closes the
// transport, detaches its listeners and unregisters the callback; unpatched
// the poll waits on s.wake forever and the handler goroutine holds shutdown
// past the deadline.
func TestMcpReceiveReleasesOnDisconnect(t *testing.T) {
	transport := newMcpDisconnectTransport()
	host, done, options := startMCPExtension(t, sdk.McpExtensionOptions{
		CreateTransport: func(sdk.McpServerEntry, string, *sdk.McpAuthProvider) (sdk.McpTransport, error) {
			return transport, nil
		},
	})
	p := startPump(host)
	defer p.stop()
	transportID := createTransport(t, p, host, options)
	sendCallback(host, "r-recv", transportID, "receive")
	waitParked(t, ".(*mcpTransportShim).receive(", "")
	host.conn.Close()
	requireTransportClosed(t, transport)
	requireCleanRun(t, done)
}

// TestMcpSendReleasesOnDisconnect parks a send inside the transport's Send and
// drops the host connection. Patched, the send context derives from the serve
// lifetime so Send returns ctx.Err(); unpatched it derives from Background and
// Send waits forever. Send entering is observed on sendStarted (the wait the
// context cancels is the transport's own), and the disconnect closes the
// transport as in the receive test.
func TestMcpSendReleasesOnDisconnect(t *testing.T) {
	transport := newMcpDisconnectTransport()
	host, done, options := startMCPExtension(t, sdk.McpExtensionOptions{
		CreateTransport: func(sdk.McpServerEntry, string, *sdk.McpAuthProvider) (sdk.McpTransport, error) {
			return transport, nil
		},
	})
	p := startPump(host)
	defer p.stop()
	transportID := createTransport(t, p, host, options)
	sendCallback(host, "r-send", transportID, "send", 7, map[string]any{"m": 1}, false)
	select {
	case <-transport.sendStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("send callback did not reach the transport's Send")
	}
	waitParked(t, ".(*mcpDisconnectTransport).Send(", "")
	host.conn.Close()
	requireTransportClosed(t, transport)
	requireCleanRun(t, done)
}

// TestMcpTransportCloseStopsWatcher runs the host's explicit close op on a
// still-live connection. The transport's Close must run exactly once — the
// close op answers it — and the shim's lifetime watcher must exit: before the
// patch it stayed parked on s.lifetime.Done(), leaking one goroutine per
// create/close cycle (the connection stays alive). Dropping the host end
// afterwards must not run Close a second time (release runs once) and
// RunWithConn must still exit cleanly.
func TestMcpTransportCloseStopsWatcher(t *testing.T) {
	transport := newMcpDisconnectTransport()
	host, done, options := startMCPExtension(t, sdk.McpExtensionOptions{
		CreateTransport: func(sdk.McpServerEntry, string, *sdk.McpAuthProvider) (sdk.McpTransport, error) {
			return transport, nil
		},
	})
	p := startPump(host)
	defer p.stop()
	transportID := createTransport(t, p, host, options)

	// The watcher parks in the shim's lifetime select as soon as the
	// transport exists; the close op can only run after it is observed,
	// so a missed release would provably leave a live goroutine behind.
	waitParked(t, ".(*mcpTransportShim).watchLifetime(", "")
	sendCallback(host, "r-close", transportID, "close")
	resp := p.waitResponse(t, "r-close")
	if msg := frameError(t, resp); msg != "" {
		t.Fatalf("transport close op failed: %s", msg)
	}
	if got := transport.closeCalls.Load(); got != 1 {
		t.Fatalf("transport Close ran %d times after the close op, want 1", got)
	}
	waitGone(t, ".(*mcpTransportShim).watchLifetime(")

	// The connection drop releases the shim again through the watcher-free
	// path; releaseOnce keeps Close single-shot.
	host.conn.Close()
	requireCleanRun(t, done)
	if got := transport.closeCalls.Load(); got != 1 {
		t.Fatalf("transport Close ran %d times in total, want 1", got)
	}
}

// mcpCredStore is an McpOAuthCredentialStore yielding one mcpLockStore.
type mcpCredStore struct{ server *mcpLockStore }

func (s *mcpCredStore) ForServer(string, string) (sdk.McpOAuthServerStore, error) {
	return s.server, nil
}
func (s *mcpCredStore) Tokens(string, string) (*sdk.McpOAuthTokens, error) { return nil, nil }
func (s *mcpCredStore) Remove(string, string) (bool, error)                { return false, nil }

// mcpLockStore is an McpOAuthServerStore whose WithRefreshLock shape the test
// chooses: "" runs fn and returns what it returns, "acquire" parks before
// running fn, "afterFn" runs fn and parks after it returns. The parks wait on
// test-controlled channels — never a bare select{} — so cleanup unblocks
// them and joined reports WithRefreshLock's goroutine exited; no goroutine a
// test parks outlives the test.
type mcpLockStore struct {
	mode     string        // "", "acquire" or "afterFn"
	entered  chan struct{} // closed when the park begins (acquire, afterFn)
	unblock  chan struct{} // closed by cleanup to end the park
	released chan struct{} // closed when WithRefreshLock returns
	joined   chan struct{} // closed when WithRefreshLock's goroutine exited
}

func newMcpLockStore(t *testing.T, mode string) *mcpLockStore {
	t.Helper()
	s := &mcpLockStore{
		mode:     mode,
		entered:  make(chan struct{}),
		unblock:  make(chan struct{}),
		released: make(chan struct{}),
		joined:   make(chan struct{}),
	}
	// Cleanup unparks a parked WithRefreshLock and requires its goroutine to
	// exit: if the goroutine was never started the store is already quiet, and
	// if it was started but never returns the test failed anyway and this
	// surfaces the leak.
	t.Cleanup(func() {
		select {
		case <-s.released:
			return
		default:
		}
		select {
		case <-s.entered:
		case <-time.After(5 * time.Second):
			// WithRefreshLock was never entered; nothing to release.
			return
		}
		close(s.unblock)
		select {
		case <-s.joined:
		case <-time.After(5 * time.Second):
			t.Error("WithRefreshLock's goroutine did not exit after unblock")
		}
	})
	return s
}

func (s *mcpLockStore) park() {
	close(s.entered)
	<-s.unblock
}

// errAcquireReleased is what the "acquire" shape returns once cleanup
// unparks it: the refresh lock never ran fn, as a real failed acquire would.
var errAcquireReleased = errors.New("test released the parked acquire")

func (s *mcpLockStore) Load() (sdk.McpOAuthState, error) { return nil, nil }
func (s *mcpLockStore) Save(sdk.McpOAuthState) error     { return nil }
func (s *mcpLockStore) WithRefreshLock(fn func() error) error {
	defer close(s.released)
	defer close(s.joined)
	if s.mode == "acquire" {
		s.park() // still acquiring the refresh lock
		return errAcquireReleased
	}
	if s.mode == "afterFn" {
		_ = fn() // fn ran; the lock holder parks before returning
		s.park()
		return nil
	}
	return fn()
}

// credentialsCallback returns the credentials callback id, then locks and
// returns (storeID, lockID) for a forServer+lock pair.
func credentialsCallback(t *testing.T, options map[string]any) string {
	t.Helper()
	cb, _ := options["credentials"].(string)
	if cb == "" {
		t.Fatalf("MCP wire options have no credentials callback: %#v", options)
	}
	return cb
}

func forServer(t *testing.T, p *hostPump, host *h11Host, cb string) string {
	t.Helper()
	sendCallback(host, "r-for", cb, "forServer", "srv", "https://h")
	storeID, _ := responseResult(t, p.waitResponse(t, "r-for"))["store"].(string)
	if storeID == "" {
		t.Fatal("forServer response names no store")
	}
	return storeID
}

func holdLock(t *testing.T, p *hostPump, host *h11Host, cb, storeID string) string {
	t.Helper()
	sendCallback(host, "r-lock", cb, "lock", storeID)
	lockID, _ := responseResult(t, p.waitResponse(t, "r-lock"))["lock"].(string)
	if lockID == "" {
		t.Fatal("lock response names no refresh lock")
	}
	return lockID
}

// TestMcpRefreshLockReleasesOnDisconnect holds a refresh lock (the host's
// "lock" ran the store's WithRefreshLock, whose fn waits for "unlock") and
// drops the connection. Patched, fn's wait releases on the serve lifetime and
// WithRefreshLock returns; unpatched it waits on held.unlock forever and the
// goroutine keeps holding the caller's lock. The parked wait is observed
// inside the store, so the connection drop provably precedes neither fn's
// entry nor its wait.
func TestMcpRefreshLockReleasesOnDisconnect(t *testing.T) {
	store := newMcpLockStore(t, "")
	host, done, options := startMCPExtension(t, sdk.McpExtensionOptions{
		Credentials: &mcpCredStore{server: store},
	})
	p := startPump(host)
	defer p.stop()
	cb := credentialsCallback(t, options)
	storeID := forServer(t, p, host, cb)
	holdLock(t, p, host, cb, storeID)
	waitParked(t, ".(*mcpLockStore).WithRefreshLock(", "")
	host.conn.Close()
	select {
	case <-store.released:
	case <-time.After(5 * time.Second):
		t.Fatal("WithRefreshLock did not release its fn wait on disconnect")
	}
	requireCleanRun(t, done)
}

// TestMcpRefreshLockAcquireReleasesOnDisconnect parks the "lock" callback
// inside the store's WithRefreshLock acquire and drops the connection.
// Patched, the callback's wait releases on the serve lifetime; unpatched it
// holds the handler goroutine past the shutdown deadline. The acquire park is
// on the store's unblock channel, so cleanup lets WithRefreshLock run its fn
// (which returns the disconnect error the dead lifetime already selected) and
// joins its goroutine.
func TestMcpRefreshLockAcquireReleasesOnDisconnect(t *testing.T) {
	store := newMcpLockStore(t, "acquire")
	host, done, options := startMCPExtension(t, sdk.McpExtensionOptions{
		Credentials: &mcpCredStore{server: store},
	})
	p := startPump(host)
	defer p.stop()
	cb := credentialsCallback(t, options)
	storeID := forServer(t, p, host, cb)
	sendCallback(host, "r-lock", cb, "lock", storeID)
	// The lock callback parks in mcpCredentialsShim.lock's select; its own
	// WithRefreshLock goroutine parks on the store's unblock instead.
	waitParked(t, ".(*mcpCredentialsShim).lock(", "")
	host.conn.Close()
	requireCleanRun(t, done)
}

// TestMcpRefreshUnlockReleasesOnDisconnect answers the held lock's "unlock"
// while the store's WithRefreshLock never returns after fn, then drops the
// connection. Patched, the unlock callback's wait on WithRefreshLock's return
// releases on the serve lifetime; unpatched it holds the handler goroutine
// past the shutdown deadline. The unlock callback parks in
// mcpCredentialsShim.serve's select — the stack is distinguished from the
// lock-op's park in lock() by excluding the latter's frame.
func TestMcpRefreshUnlockReleasesOnDisconnect(t *testing.T) {
	store := newMcpLockStore(t, "afterFn")
	host, done, options := startMCPExtension(t, sdk.McpExtensionOptions{
		Credentials: &mcpCredStore{server: store},
	})
	p := startPump(host)
	defer p.stop()
	cb := credentialsCallback(t, options)
	storeID := forServer(t, p, host, cb)
	lockID := holdLock(t, p, host, cb, storeID)
	sendCallback(host, "r-unlock", cb, "unlock", storeID, lockID, "", false)
	waitParked(t, ".(*mcpCredentialsShim).serve(", ".(*mcpCredentialsShim).lock(")
	host.conn.Close()
	requireCleanRun(t, done)
}
