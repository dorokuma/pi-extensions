package ctxmode

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the recording MCP server. No Node dependency or external
// script installation is needed; child binaries never touch production files.
func TestMain(m *testing.M) {
	if os.Getenv("CTXMODE_TEST_HELPER") == "1" {
		runHelper()
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "ctxmode-go-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CTXMODE_DIAG_DIR", dir); err != nil {
		panic(err)
	}
	_ = os.Unsetenv("CTXMODE_DEBUG")
	_ = os.Unsetenv("CTXMODE_DIAG_STDERR")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
func runHelper() {
	if p := os.Getenv("CTXMODE_TEST_PIDLOG"); p != "" {
		appendLine(p, strconv.Itoa(os.Getpid()))
	}
	if p := os.Getenv("CTXMODE_TEST_PIDFILE"); p != "" {
		if err := os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			panic(err)
		}
	}
	if os.Getenv("CTXMODE_TEST_CRASH") == "1" {
		go func() { time.Sleep(400 * time.Millisecond); os.Exit(3) }()
	}
	if os.Getenv("CTXMODE_TEST_IGNORE_TERM") == "1" {
		ignoreTerm()
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if p := os.Getenv("CTXMODE_TEST_RECV"); p != "" {
			appendLine(p, line)
		}
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &req) != nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			if os.Getenv("CTXMODE_TEST_FAIL_INIT") == "1" {
				fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-32000,\"message\":\"initialize refused\"}}\n", req.ID)
				continue
			}
			if os.Getenv("CTXMODE_TEST_HANG_INIT") == "1" {
				continue
			}
			result = map[string]any{"instructions": "stub", "protocolVersion": "2024-11-05", "capabilities": map[string]any{}}
		case "tools/list":
			if os.Getenv("CTXMODE_TEST_FAIL_LIST") == "1" {
				fmt.Printf("{\"id\":%d,\"error\":{\"message\":\"list refused\"}}\n", req.ID)
				continue
			}
			result = map[string]any{"tools": []map[string]any{{"name": "ctx_run", "description": "stub", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			continue
		}
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if err != nil {
			panic(err)
		}
		fmt.Println(string(data))
	}
	if os.Getenv("CTXMODE_TEST_IGNORE_TERM") == "1" {
		select {}
	}
}
func ignoreTerm() { signal.Ignore(syscall.SIGTERM) }
func appendLine(p, line string) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	if _, err = f.WriteString(line + "\n"); err != nil {
		panic(err)
	}
	if err = f.Close(); err != nil {
		panic(err)
	}
}
func setupHelper(t *testing.T) (recv, pids string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	recv = filepath.Join(dir, "recv.jsonl")
	pids = filepath.Join(dir, "pids.log")
	for _, p := range []string{recv, pids} {
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{"CTXMODE_BIN": bin, "CTXMODE_TEST_HELPER": "1", "CTXMODE_TEST_RECV": recv, "CTXMODE_TEST_PIDLOG": pids, "CTXMODE_DISPOSE_WAIT_MS": "50", "GORACE": "atexit_sleep_ms=0"} {
		t.Setenv(k, v)
	}
	return
}
func fileText(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func countPids(t *testing.T, p string) int { return len(strings.Fields(fileText(t, p))) }
func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
func requireContains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q missing %q", s, sub)
	}
}
func requireError(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("error = %v, want %q", err, sub)
	}
}
func newClient() *CtxmodeClient { return NewCtxmodeClient("/tmp/fake-workdir") }
func logPath() string           { return filepath.Join(os.Getenv("CTXMODE_DIAG_DIR"), "ctxmode.log") }
func isolatedLog(t *testing.T)  { t.Helper(); t.Setenv("CTXMODE_DIAG_DIR", t.TempDir()) }
func withClient(t *testing.T) *CtxmodeClient {
	t.Helper()
	c := newClient()
	t.Cleanup(c.Stop)
	return c
}

// Fake proc/streams reproduce the original EventEmitter harness's stateful
// assertions while synchronizing access for race-detector validation.
type fakeStream struct {
	mu      sync.Mutex
	closed  bool
	writes  []string
	entered chan struct{} // closed on the first Write call, before any blocking
	block   chan struct{} // when non-nil, Write waits until it is closed
	once    sync.Once
}

func (s *fakeStream) Read([]byte) (int, error) { return 0, io.EOF }
func (s *fakeStream) Write(p []byte) (int, error) {
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, string(p))
	return len(p), nil
}
func (s *fakeStream) Close() error { s.mu.Lock(); defer s.mu.Unlock(); s.closed = true; return nil }
func (s *fakeStream) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

type fakeProc struct {
	in, out, err fakeStream
	done         chan struct{}
	signals      []string
	autoExit     bool
	once         sync.Once
}

func newFakeProc() *fakeProc              { return &fakeProc{done: make(chan struct{})} }
func (p *fakeProc) Input() io.WriteCloser { return &p.in }
func (p *fakeProc) Output() io.ReadCloser { return &p.out }
func (p *fakeProc) Stderr() io.ReadCloser { return &p.err }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Signal(s os.Signal) error {
	p.signals = append(p.signals, s.String())
	if p.autoExit || s.String() == "killed" {
		p.once.Do(func() { close(p.done) })
	}
	return nil
}
func recordingClient() (*CtxmodeClient, *fakeProc) {
	c := newClient()
	p := newFakeProc()
	c.proc = p
	c.initialized = true
	return c, p
}
func callAsync(c *CtxmodeClient, ctx context.Context, name string, args map[string]any) <-chan reply {
	done := make(chan reply, 1)
	go func() {
		text, err := c.CallTool(ctx, name, args)
		r := reply{err: err}
		r.result.Content = append(r.result.Content, struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", text})
		done <- r
	}()
	return done
}
func respond(c *CtxmodeClient, id int, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"id": id, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
	c.acceptResponseLocked(string(data))
}
func pendingCount(c *CtxmodeClient) int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.pending) }
func decodeRequest(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// sdkHost exercises actual factory event/command/tool registration over PiG's
// length-prefixed IPC, including late RegisterTool and host-side cancellation.
type sdkHost struct {
	t             *testing.T
	conn          net.Conn
	writeMu       sync.Mutex
	mu            sync.Mutex
	next          int
	responses     map[string]chan map[string]any
	handlers      map[string]int
	tools         map[string]map[string]any
	notifications []string
	done          chan error
	readerDone    chan struct{}
}

func newSDKHost(t *testing.T) *sdkHost {
	t.Helper()
	host, ext := net.Pipe()
	h := &sdkHost{t: t, conn: host, responses: map[string]chan map[string]any{}, handlers: map[string]int{}, tools: map[string]map[string]any{}, done: make(chan error, 1), readerDone: make(chan struct{})}
	go func() { h.done <- Extension().RunWithConn(ext) }()
	reg := h.readFrame()
	for _, v := range reg["register"].(map[string]any)["handlers"].([]any) {
		m := v.(map[string]any)
		h.handlers[m["event"].(string)] = int(m["handler_id"].(float64))
	}
	h.send(map[string]any{"type": "ready", "ready": map[string]any{"cwd": t.TempDir(), "mode": "rpc", "width": 80}})
	go func() {
		defer close(h.readerDone)
		for {
			msg, err := readFrame(h.conn)
			if err != nil {
				return
			}
			switch msg["type"] {
			case "call":
				call := msg["call"].(map[string]any)
				method := call["method"].(string)
				if method == "registerTool" {
					def := call["args"].(map[string]any)
					h.mu.Lock()
					h.tools[def["name"].(string)] = def
					h.mu.Unlock()
				}
				if method == "ui.notify" {
					args := call["args"].(map[string]any)
					h.mu.Lock()
					h.notifications = append(h.notifications, fmt.Sprint(args))
					h.mu.Unlock()
				}
				h.send(map[string]any{"type": "call_result", "id": msg["id"], "call_result": map[string]any{"result": nil}})
			case "response":
				h.mu.Lock()
				ch := h.responses[msg["id"].(string)]
				h.mu.Unlock()
				if ch != nil {
					ch <- msg["response"].(map[string]any)
				}
			}
		}
	}()
	t.Cleanup(func() {
		h.send(map[string]any{"type": "shutdown"})
		select {
		case err := <-h.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("SDK shutdown deadline")
		}
		_ = h.conn.Close()
		<-h.readerDone
	})
	return h
}
func readFrame(r io.Reader) (map[string]any, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var msg map[string]any
	err := json.Unmarshal(data, &msg)
	return msg, err
}
func (h *sdkHost) readFrame() map[string]any {
	h.t.Helper()
	msg, err := readFrame(h.conn)
	if err != nil {
		h.t.Fatal(err)
	}
	return msg
}
func (h *sdkHost) send(msg map[string]any) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		h.t.Error(err)
		return
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err = h.conn.Write(append(header[:], data...)); err != nil {
		h.t.Error(err)
	}
}
func (h *sdkHost) request(method, name string, args any) (string, <-chan map[string]any) {
	h.mu.Lock()
	h.next++
	id := strconv.Itoa(h.next)
	ch := make(chan map[string]any, 1)
	h.responses[id] = ch
	h.mu.Unlock()
	req := map[string]any{"method": method, "args": args}
	if method == "event" {
		req["event"] = name
		req["handler_id"] = h.handlers[name]
	} else {
		req["tool"] = name
	}
	h.send(map[string]any{"type": "request", "id": id, "request": req})
	return id, ch
}
func (h *sdkHost) event(name string) map[string]any {
	_, ch := h.request("event", name, map[string]any{})
	return h.await(ch)
}
func (h *sdkHost) await(ch <-chan map[string]any) map[string]any {
	h.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(6 * time.Second):
		h.t.Fatal("SDK response deadline")
		return nil
	}
}
func (h *sdkHost) tool(name string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tools[name]
}
func responseText(r map[string]any) string {
	result, _ := r["result"].(map[string]any)
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		return content[0].(map[string]any)["text"].(string)
	}
	return fmt.Sprint(r)
}
