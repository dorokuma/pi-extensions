package herdsmanpi

// Port of test/integration/herdsman-pi-daemon-client.test.ts (9 cases).
//
// Every case runs against an isolated fake daemon bound to a Unix socket under
// a private temp dir; nothing touches the production herdsman daemon. The
// socket path stays well under the 104-byte sun_path limit.

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type rpcClientMsg struct {
	ID     any            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// fakeDaemon is the in-test daemon: a Unix listener plus a JSON-lines protocol.
type fakeDaemon struct {
	t        *testing.T
	listener net.Listener
	socket   string
	mu       sync.Mutex
	conns    int
	// live holds the accepted connections so a test can push a notification.
	live      []net.Conn
	onMessage func(conn net.Conn, message rpcClientMsg)
	onConnect func(conn net.Conn)
	closed    chan struct{}
	writeMu   sync.Mutex
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	daemon := &fakeDaemon{t: t, listener: listener, socket: socket, closed: make(chan struct{})}
	go daemon.accept()
	t.Cleanup(func() { daemon.Close() })
	return daemon
}

func (d *fakeDaemon) accept() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns++
		d.live = append(d.live, conn)
		onMessage := d.onMessage
		onConnect := d.onConnect
		d.mu.Unlock()
		go d.serve(conn, onConnect, onMessage)
	}
}

func (d *fakeDaemon) serve(conn net.Conn, onConnect func(net.Conn), onMessage func(net.Conn, rpcClientMsg)) {
	decoder, err := NewJsonLineDecoder(DefaultMaxFrameBytes)
	if err != nil {
		return
	}
	if onConnect != nil {
		onConnect(conn)
	}
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := conn.Read(buffer)
		if read > 0 {
			frames, decodeErr := decoder.Push(string(buffer[:read]))
			if decodeErr != nil {
				_ = conn.Close()
				return
			}
			for _, frame := range frames {
				record, ok := frame.(map[string]any)
				if !ok {
					continue
				}
				message := rpcClientMsg{Method: stringOr(record["method"]), Params: mapOr(record["params"])}
				message.ID = record["id"]
				if onMessage != nil {
					onMessage(conn, message)
				}
			}
		}
		if readErr != nil {
			_ = conn.Close()
			return
		}
	}
}

func stringOr(value any) string {
	text, _ := value.(string)
	return text
}

func mapOr(value any) map[string]any {
	record, _ := value.(map[string]any)
	return record
}

func (d *fakeDaemon) Write(conn net.Conn, payload string) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, _ = conn.Write([]byte(payload + "\n"))
}

func (d *fakeDaemon) WriteJSON(conn net.Conn, value any) {
	line, err := EncodeJSONLine(value)
	if err != nil {
		return
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, _ = conn.Write([]byte(line))
}

func (d *fakeDaemon) SetOnMessage(handler func(net.Conn, rpcClientMsg)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onMessage = handler
}

func (d *fakeDaemon) SetOnConnect(handler func(net.Conn)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onConnect = handler
}

func (d *fakeDaemon) Connections() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conns
}

// DropLive closes the server side of every live connection, so connected
// clients see a disconnect (the daemon socket dropping under them).
func (d *fakeDaemon) DropLive() {
	d.mu.Lock()
	live := d.live
	d.live = nil
	d.mu.Unlock()
	for _, conn := range live {
		_ = conn.Close()
	}
}

func (d *fakeDaemon) Close() {
	select {
	case <-d.closed:
		return
	default:
	}
	close(d.closed)
	_ = d.listener.Close()
	if err := os.Remove(d.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		// A missing socket file only means the listener never got that far.
		_ = err
	}
}

// awaitCondition polls until the predicate holds (the TS `waitFor`).
func awaitCondition(t *testing.T, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition was not met: %s", what)
}

// TestClientServesRequestsFromInsideAStreamHandler is the regression guard
// for the second deadlock shape: a handler that runs on the notification path
// and issues a request of its own. The read loop must stay free to read that
// response, so notification delivery happens off the reader.
func TestClientServesRequestsFromInsideAStreamHandler(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)

	pushed := false
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"method": message.Method}})
		if pushed || message.Method != "agent.list" {
			return
		}
		// Push the notification exactly once: the handler below answers with
		// another `agent.list`, which must not push again.
		pushed = true
		daemon.WriteJSON(conn, map[string]any{
			"method": "agent.orchestrator.changed",
			"params": map[string]any{
				"change": map[string]any{
					"current":  map[string]any{"herdrSessionName": "default", "owner": map[string]any{"paneId": "wB:p-1", "terminalId": "term_agent"}, "workspaceId": "wB"},
					"previous": map[string]any{"herdrSessionName": "default", "owner": nil, "workspaceId": "wB"},
				},
				"herdrSessionName": "default",
				"workspaceId":      "wB",
			},
		})
	})

	// The handler asks the daemon something while it is being notified. Before
	// delivery moved off the read loop this waited for a response that only the
	// blocked reader could deliver.
	handled := make(chan string, 4)
	client.SetOnStreamMessage(func(message DaemonStreamMessage) {
		if message.Method != "agent.orchestrator.changed" {
			return
		}
		result, err := client.Request("agent.list", map[string]any{})
		if err != nil {
			handled <- "error: " + err.Error()
			return
		}
		record, _ := result.(map[string]any)
		method, _ := record["method"].(string)
		handled <- method
	})

	awaitCondition(t, "connection", func() bool { return daemon.Connections() == 1 })
	if _, err := client.Request("agent.list", map[string]any{}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	select {
	case method := <-handled:
		if method != "agent.list" {
			t.Fatalf("handler's nested request answered %q", method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler's request deadlocked against the read loop")
	}
}

// TestClientDeliversNotificationsInOrderWithoutDropping pins the queue the
// dispatcher drains: arrival order, and every notification delivered.
func TestClientDeliversNotificationsInOrderWithoutDropping(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)

	const total = 25
	delivered := make(chan int, total)
	client.SetOnStreamMessage(func(message DaemonStreamMessage) {
		if message.Event != nil {
			delivered <- message.Event.ID
		}
	})
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{}})
		for id := 1; id <= total; id++ {
			daemon.WriteJSON(conn, map[string]any{
				"method": "agent.event",
				"params": map[string]any{"event": map[string]any{"id": id, "type": "agent.done", "payload": map[string]any{}}},
			})
		}
	})

	awaitCondition(t, "connection", func() bool { return daemon.Connections() == 1 })
	if _, err := client.Request("agent.list", map[string]any{}); err != nil {
		t.Fatalf("request: %v", err)
	}
	seen := make([]int, 0, total)
	for len(seen) < total {
		select {
		case id := <-delivered:
			seen = append(seen, id)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d notifications arrived: %v", len(seen), total, seen)
		}
	}
	for index, id := range seen {
		if id != index+1 {
			t.Fatalf("notification order broken at %d: %v", index, seen)
		}
	}
}

func TestClientConnectsHandlesResponsesAndStreams(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	connected := make(chan struct{}, 8)
	streams := make(chan DaemonStreamMessage, 8)
	client.SetOnConnected(func() error { connected <- struct{}{}; return nil })
	client.SetOnStreamMessage(func(message DaemonStreamMessage) { streams <- message })

	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{
			"id":     message.ID,
			"result": map[string]any{"method": message.Method},
		})
		daemon.WriteJSON(conn, map[string]any{
			"method": "agent.event",
			"params": map[string]any{"event": map[string]any{"id": 1, "payload": map[string]any{}, "type": "agent.done"}},
		})
	})

	awaitCondition(t, "onConnected", func() bool { return len(connected) == 1 })
	result, err := client.Request("agent.list", map[string]any{})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	record, ok := result.(map[string]any)
	if !ok || record["method"] != "agent.list" {
		t.Fatalf("unexpected result: %#v", result)
	}
	select {
	case stream := <-streams:
		if stream.Method != "agent.event" || stream.Event == nil || stream.Event.ID != 1 {
			t.Fatalf("unexpected stream: %+v", stream)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no stream message arrived")
	}
}

func TestClientDecodesContextChangeStreams(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	streams := make(chan DaemonStreamMessage, 4)
	client.SetOnStreamMessage(func(message DaemonStreamMessage) { streams <- message })

	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{}})
		daemon.WriteJSON(conn, map[string]any{
			"method": "agent.context.changed",
			"params": map[string]any{
				"context": map[string]any{
					"agents":           []any{},
					"herdrSessionName": "default",
					"updatedAt":        "2026-07-16T00:00:00.000Z",
					"workspaceId":      "wB",
				},
				"herdrSessionName": "default",
				"workspaceId":      "wB",
			},
		})
	})

	awaitCondition(t, "connection", func() bool { return clientConnected(client) })
	result, err := client.Request("agent.list", map[string]any{})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if record, ok := result.(map[string]any); !ok || len(record) != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	select {
	case stream := <-streams:
		if stream.Method != "agent.context.changed" || stream.Context == nil || stream.Context.WorkspaceID != "wB" {
			t.Fatalf("unexpected context stream: %+v", stream)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no context stream arrived")
	}
}

// clientConnected reports the private state through the exported API surface.
func clientConnected(client *ReconnectingDaemonClient) bool {
	_, err := client.Request("__probe__", map[string]any{})
	return err == nil || !errors.Is(err, ErrClientNotConnected)
}

func TestClientRejectsInFlightWorkReconnectsAndServesLaterRequests(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	disconnects := make(chan error, 4)
	client.SetOnDisconnected(func(err error) { disconnects <- err })

	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		if message.Method == "hold" {
			_ = conn.Close()
			return
		}
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	awaitCondition(t, "first connection", func() bool { return daemon.Connections() == 1 })

	_, err := client.Request("hold", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "socket closed") {
		t.Fatalf("expected a socket-closed rejection, got %v", err)
	}
	awaitCondition(t, "reconnect", func() bool { return daemon.Connections() == 2 })
	select {
	case <-disconnects:
	case <-time.After(2 * time.Second):
		t.Fatal("no disconnect callback")
	}
	result, err := client.Request("later", map[string]any{})
	if err != nil {
		t.Fatalf("later request: %v", err)
	}
	if record, ok := result.(map[string]any); !ok || record["ok"] != true {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestClientSurvivesMalformedJSONAndReconnects(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	daemon.SetOnConnect(func(conn net.Conn) {
		if daemon.Connections() == 1 {
			daemon.Write(conn, "{not-json}")
		}
	})
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	awaitCondition(t, "second connection", func() bool { return daemon.Connections() >= 2 })
	result, err := client.Request("healthy", map[string]any{})
	if err != nil {
		t.Fatalf("request after reconnect: %v", err)
	}
	if record, ok := result.(map[string]any); !ok || record["ok"] != true {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestClientCloseStopsReconnectAndRejectsRequests(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, nil)
	t.Cleanup(client.Close)
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{}})
	})
	time.Sleep(20 * time.Millisecond)
	client.Close()
	if _, err := client.Request("agent.list", map[string]any{}); err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("expected a closed rejection, got %v", err)
	}
}

// optionalMethodCase drives the "old daemon answers Unknown method" contract.
func optionalMethodCase(t *testing.T, optional string, params map[string]any) {
	t.Helper()
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	disconnected := make(chan error, 4)
	client.SetOnDisconnected(func(err error) { disconnected <- err })

	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		if message.Method == optional {
			daemon.WriteJSON(conn, map[string]any{
				"error": map[string]any{"message": "Unknown method: " + message.Method},
				"id":    message.ID,
			})
			return
		}
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	awaitCondition(t, "connection", func() bool { return daemon.Connections() == 1 })

	_, err := client.Request(optional, params)
	if err == nil || !strings.Contains(err.Error(), "Unknown method: "+optional) {
		t.Fatalf("expected the unknown-method rejection, got %v", err)
	}
	// The connection must survive and keep serving required methods.
	if _, err := client.Request("agent.list", map[string]any{}); err != nil {
		t.Fatalf("required method after an optional-method failure: %v", err)
	}
	select {
	case err := <-disconnected:
		t.Fatalf("the client disconnected (%v)", err)
	default:
	}
	if got := daemon.Connections(); got != 1 {
		t.Fatalf("expected the connection to stay, got %d connections", got)
	}
}

func TestClientTreatsPingAsOptional(t *testing.T) {
	optionalMethodCase(t, "agent.ping", map[string]any{})
}

func TestClientTreatsTurnCompletedAsOptional(t *testing.T) {
	optionalMethodCase(t, "agent.turn.completed", map[string]any{"confirmed": true})
}

func TestClientMarksDaemonIncompatibleOnUnknownRequiredMethod(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	disconnected := make(chan error, 4)
	client.SetOnDisconnected(func(err error) { disconnected <- err })

	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		daemon.WriteJSON(conn, map[string]any{
			"error": map[string]any{"message": "Unknown method: " + message.Method},
			"id":    message.ID,
		})
	})
	awaitCondition(t, "connection", func() bool { return daemon.Connections() == 1 })

	_, err := client.Request("agent.orchestrator.get", map[string]any{})
	if err == nil || err.Error() != "Unknown method: agent.orchestrator.get" {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case err := <-disconnected:
		if !strings.Contains(err.Error(), "incompatible") {
			t.Fatalf("disconnect was not the incompatibility one: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no disconnect callback")
	}
	// The client stopped reconnecting.
	time.Sleep(40 * time.Millisecond)
	if got := daemon.Connections(); got != 1 {
		t.Fatalf("reconnected after an incompatibility (%d connections)", got)
	}
	if _, err := client.Request("agent.list", map[string]any{}); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected a not-connected rejection, got %v", err)
	}
}

func TestClientRejectsUnansweredRequestAndIgnoresLateResponse(t *testing.T) {
	daemon := newFakeDaemon(t)
	client := NewReconnectingDaemonClient(daemon.socket, []int64{5})
	t.Cleanup(client.Close)
	daemon.SetOnMessage(func(conn net.Conn, message rpcClientMsg) {
		if message.Method == "hang" {
			// Deliberately never answer.
			return
		}
		daemon.WriteJSON(conn, map[string]any{"id": message.ID, "result": map[string]any{"ok": true}})
	})
	connected := make(chan struct{}, 4)
	client.SetOnConnected(func() error { connected <- struct{}{}; return nil })
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("never connected")
	}

	started := time.Now()
	_, err := client.Request("hang", map[string]any{})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout, got %v", err)
	}
	// The production request timeout is 15s; this pins the contract without a
	// fake clock (the port has no injectable request timer).
	if elapsed < requestTimeoutMS*time.Millisecond-2*time.Second {
		t.Fatalf("timed out too early: %s", elapsed)
	}
	// The slot was removed, so later requests still work.
	result, err := client.Request("agent.list", map[string]any{})
	if err != nil {
		t.Fatalf("later request: %v", err)
	}
	if record, ok := result.(map[string]any); !ok || record["ok"] != true {
		t.Fatalf("unexpected result: %#v", result)
	}
}
