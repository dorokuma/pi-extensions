package ctxmode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const cancelledMessage = "ctxmode tool call cancelled (user pressed esc)"
const stoppedMessage = "ctxmode client was stopped; create a new client (e.g. /ctxmode-start) instead"

var toolNames = []string{"ctx_run", "ctx_fs", "ctx_git", "ctx_kb", "ctx_bg", "ctx_stats"}

type mcpResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Instructions *string `json:"instructions"`
}
type reply struct {
	result mcpResult
	err    error
}
type pendingRequest struct {
	done    chan reply
	written chan struct{} // closed once the request line is on the wire (or the write failed)
	proc    bridgeProc    // connection this request was written to
}
type startFlight struct {
	done chan struct{}
	err  error
}

// bridgeProc allows the same disposal contract to be exercised with fake streams.
type bridgeProc interface {
	Input() io.WriteCloser
	Output() io.ReadCloser
	Stderr() io.ReadCloser
	Signal(os.Signal) error
	Done() <-chan struct{}
}
type commandProc struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout, stderr io.ReadCloser
	done           chan struct{}
	intentional    bool // guarded by the owning client's mutex
}

func (p *commandProc) Input() io.WriteCloser    { return p.stdin }
func (p *commandProc) Output() io.ReadCloser    { return p.stdout }
func (p *commandProc) Stderr() io.ReadCloser    { return p.stderr }
func (p *commandProc) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
func (p *commandProc) Done() <-chan struct{}    { return p.done }

// CtxmodeClient owns a single connection and serializes state and wire writes.
// Process identity checks isolate old readers/waiters from a replacement process.
type CtxmodeClient struct {
	mu                   sync.Mutex
	writeMu              sync.Mutex // serializes stdin writes; never held together with mu by writers
	workdir              string
	proc                 bridgeProc
	requestID            int
	pending              map[int]*pendingRequest
	initialized, stopped bool
	instructions         *string
	starting             *startFlight
	restartAttempts      int
	restartTimer         *time.Timer
	stderrBuffer         []string
	stderrCarry          string
}

func NewCtxmodeClient(workdir string) *CtxmodeClient {
	return &CtxmodeClient{workdir: workdir, pending: make(map[int]*pendingRequest)}
}
func (c *CtxmodeClient) GetTools() []string { return append([]string(nil), toolNames...) }
func (c *CtxmodeClient) GetServerInstructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.instructions == nil {
		return ""
	}
	return *c.instructions
}

func (c *CtxmodeClient) Start() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return errors.New(stoppedMessage)
	}
	if c.proc != nil && c.initialized {
		c.mu.Unlock()
		return nil
	}
	if flight := c.starting; flight != nil {
		c.mu.Unlock()
		<-flight.done
		return flight.err
	}
	flight := &startFlight{done: make(chan struct{})}
	c.starting = flight
	c.mu.Unlock()
	flight.err = c.doStart()
	c.mu.Lock()
	c.starting = nil
	close(flight.done)
	c.mu.Unlock()
	return flight.err
}
func (c *CtxmodeClient) doStart() (err error) {
	c.shutDownProc()
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return errors.New(stoppedMessage)
	}
	cmd := exec.Command(envString("CTXMODE_BIN", "ctxmode"), "-workdir", c.workdir)
	cmd.Env = roleEnviron()
	stdin, e := cmd.StdinPipe()
	if e != nil {
		c.mu.Unlock()
		return e
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		_ = stdin.Close()
		c.mu.Unlock()
		return e
	}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		c.mu.Unlock()
		return e
	}
	if e = cmd.Start(); e != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		DiagLog("process error: " + e.Error())
		c.scheduleRestartLocked()
		c.mu.Unlock()
		return e
	}
	p := &commandProc{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, done: make(chan struct{})}
	c.proc = p
	c.mu.Unlock()
	defer func() {
		if err != nil {
			c.shutDownProc()
		}
	}()
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); c.readOutput(p) }()
	go func() { defer readers.Done(); c.readStderr(p) }()
	go func() {
		readers.Wait()
		waitErr := cmd.Wait()
		close(p.done)
		code, signal := "0", ""
		if waitErr != nil {
			code = fmt.Sprint(cmd.ProcessState.ExitCode())
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				code = "null"
				signal = "SIG" + strings.ToUpper(status.Signal().String())
				switch status.Signal() {
				case syscall.SIGKILL:
					signal = "SIGKILL"
				case syscall.SIGTERM:
					signal = "SIGTERM"
				}
			}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.proc != p || p.intentional {
			return
		}
		c.handleProcExitLocked(code, signal)
	}()
	init, e := c.sendRequest(context.Background(), "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "pi-ctxmode", "version": "1.0.0"}}, envPositiveNumber("CTXMODE_START_TIMEOUT_MS", 15000))
	if e != nil {
		return e
	}
	c.mu.Lock()
	c.instructions = init.Instructions
	c.notifyLocked("notifications/initialized", map[string]any{})
	c.mu.Unlock()
	list, e := c.sendRequest(context.Background(), "tools/list", map[string]any{}, envPositiveNumber("CTXMODE_START_TIMEOUT_MS", 15000))
	if e != nil {
		return e
	}
	c.mu.Lock()
	if c.stopped || c.proc != p {
		c.mu.Unlock()
		return errors.New("ctxmode disconnected")
	}
	c.initialized = true
	c.mu.Unlock()
	names := make([]string, 0, len(list.Tools))
	for _, t := range list.Tools {
		names = append(names, t.Name)
	}
	DiagLog(fmt.Sprintf("started workdir=%s, tools: %s, out≤%dc/%dL", c.workdir, strings.Join(names, ", "), envNumber("CTXMODE_OUTPUT_CHARS", 50000), envNumber("CTXMODE_OUTPUT_LINES", 500)))
	return nil
}
func (c *CtxmodeClient) readOutput(p bridgeProc) {
	reader := bufio.NewReader(p.Output())
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			c.mu.Lock()
			if c.proc == p {
				c.acceptResponseLocked(line)
			}
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
func (c *CtxmodeClient) acceptResponseLocked(line string) {
	var msg struct {
		ID     *int      `json:"id"`
		Result mcpResult `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(line), &msg) != nil || msg.ID == nil {
		return
	}
	if entry := c.pending[*msg.ID]; entry != nil {
		delete(c.pending, *msg.ID)
		r := reply{result: msg.Result}
		if msg.Error != nil {
			r.err = errors.New(msg.Error.Message)
		}
		entry.done <- r
	}
}
func (c *CtxmodeClient) readStderr(p bridgeProc) {
	buf := make([]byte, 4096)
	for {
		n, err := p.Stderr().Read(buf)
		if n > 0 {
			c.mu.Lock()
			if c.proc == p {
				c.handleStderrChunkLocked(string(buf[:n]))
			}
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
func (c *CtxmodeClient) pushStderrLineLocked(line string) {
	if line == "" {
		return
	}
	c.stderrBuffer = append(c.stderrBuffer, line)
	if len(c.stderrBuffer) > 20 {
		c.stderrBuffer = c.stderrBuffer[1:]
	}
}
func (c *CtxmodeClient) handleStderrChunkLocked(text string) {
	if os.Getenv("CTXMODE_DEBUG") != "" {
		DiagLog(strings.TrimRight(text, " \t\r\n"))
	}
	lines := strings.Split(c.stderrCarry+text, "\n")
	c.stderrCarry = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		c.pushStderrLineLocked(strings.TrimRight(line, " \t\r\n"))
	}
}
func (c *CtxmodeClient) flushStderrCarryLocked() {
	c.pushStderrLineLocked(strings.TrimRight(c.stderrCarry, " \t\r\n"))
	c.stderrCarry = ""
}
func (c *CtxmodeClient) clearStderrBufferLocked() { c.flushStderrCarryLocked(); c.stderrBuffer = nil }
func (c *CtxmodeClient) dumpStderrBufferLocked(force bool) {
	c.flushStderrCarryLocked()
	all := c.stderrBuffer
	c.stderrBuffer = nil
	lines := []string{}
	for _, line := range all {
		if isInterestingStderrLine(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 && force {
		lines = all[max(0, len(all)-5):]
	}
	if len(lines) == 0 {
		return
	}
	DiagLog(fmt.Sprintf("last stderr before exit (%d lines):", len(lines)))
	for _, line := range lines {
		DiagLog("  " + line)
	}
}
func (c *CtxmodeClient) handleProcExitLocked(code, signal string) {
	if code == "0" {
		DiagLog(fmt.Sprintf("unexpected exit code=0 signal=%s (silent to TUI; auto-restart if enabled)", signal))
		c.clearStderrBufferLocked()
	} else {
		DiagLog(fmt.Sprintf("process exited abnormally code=%s signal=%s", code, signal))
		c.dumpStderrBufferLocked(true)
	}
	c.cleanupLocked()
	c.scheduleRestartLocked()
}
func (c *CtxmodeClient) cleanupLocked() {
	for id, entry := range c.pending {
		delete(c.pending, id)
		entry.done <- reply{err: errors.New("ctxmode disconnected")}
	}
	c.proc = nil
	c.initialized = false
}
func (c *CtxmodeClient) disposeProc(p bridgeProc) {
	_ = p.Input().Close()
	_ = p.Stderr().Close()
	_ = p.Output().Close()
	if err := p.Signal(syscall.SIGTERM); err != nil {
		if !errors.Is(err, os.ErrProcessDone) {
			DiagLog("dispose: kill(SIGTERM) 失败: " + err.Error())
		}
		return
	}
	timer := time.NewTimer(time.Duration(envNumber("CTXMODE_DISPOSE_WAIT_MS", 3000)) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.Done():
		return
	case <-timer.C:
	}
	select {
	case <-p.Done():
		return
	default:
	}
	if err := p.Signal(syscall.SIGKILL); err != nil {
		if !errors.Is(err, os.ErrProcessDone) {
			DiagLog("dispose: kill(SIGKILL) 失败: " + err.Error())
		}
		return
	}
	// Reap the killed child before returning; no orphan/zombie process leaks.
	<-p.Done()
}
func (c *CtxmodeClient) shutDownProc() {
	c.mu.Lock()
	p := c.proc
	if p == nil {
		c.mu.Unlock()
		return
	}
	if cmd, ok := p.(*commandProc); ok {
		cmd.intentional = true
	}
	c.mu.Unlock()
	c.disposeProc(p)
	c.mu.Lock()
	if c.proc == p {
		c.clearStderrBufferLocked()
		c.cleanupLocked()
	}
	c.mu.Unlock()
}
func (c *CtxmodeClient) Stop() {
	c.mu.Lock()
	c.stopped = true
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
	c.mu.Unlock()
	c.shutDownProc()
}
func (c *CtxmodeClient) scheduleRestartLocked() {
	if c.stopped {
		return
	}
	if c.restartAttempts >= 5 {
		DiagLog("gave up auto-restart after 5 attempts")
		return
	}
	if c.restartTimer != nil {
		return
	}
	delay := min(1000*(1<<c.restartAttempts), 15000)
	c.restartAttempts++
	DiagLog(fmt.Sprintf("auto-restart in %dms (attempt %d)", delay, c.restartAttempts))
	c.restartTimer = time.AfterFunc(time.Duration(delay)*time.Millisecond, func() {
		c.mu.Lock()
		c.restartTimer = nil
		stopped := c.stopped
		c.mu.Unlock()
		if stopped {
			return
		}
		if err := c.Start(); err != nil {
			DiagLog(fmt.Sprintf("auto-restart failed: %v", err))
			c.mu.Lock()
			c.scheduleRestartLocked()
			c.mu.Unlock()
		}
	})
}

// writeToProc serializes one JSON-RPC line onto a connection. Only the Write
// syscall sits under writeMu: a blocked pipe (full stdin) must not pin the state
// lock, so cancellation and cleanup still make progress while a write is stuck.
func (c *CtxmodeClient) writeToProc(proc bridgeProc, message any) error {
	in := proc.Input()
	if in == nil {
		return errors.New("ctxmode not running")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = in.Write(data)
	return err
}

func (c *CtxmodeClient) writeLocked(message any) error {
	if c.proc == nil || c.proc.Input() == nil {
		return errors.New("ctxmode not running")
	}
	return c.writeToProc(c.proc, message)
}
func (c *CtxmodeClient) notifyLocked(method string, params map[string]any) {
	if c.proc == nil {
		return
	}
	if err := c.writeLocked(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		DiagLog(fmt.Sprintf("notify %s failed: %v", method, err))
	}
}
func (c *CtxmodeClient) cancelRequestLocked(proc bridgeProc, id int, reason string) {
	// Process identity check: a cancellation for a request the replacement
	// connection never saw would be noise (TS's onAbort guards the same way).
	if proc == nil || c.proc != proc || id <= 0 {
		return
	}
	if err := c.writeLocked(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": reason}}); err != nil {
		DiagLog(fmt.Sprintf("cancel notify for request %d failed: %v", id, err))
	}
}

// cancelPendingLocked cancels a request that still owns its pending entry. The
// request line must precede the notification (TS ordering); while the write is
// still in flight — a blocked stdin — the notification is deferred to a
// goroutine so the caller never waits for the pipe to drain.
func (c *CtxmodeClient) cancelPendingLocked(entry *pendingRequest, id int, reason string) {
	select {
	case <-entry.written:
		c.cancelRequestLocked(entry.proc, id, reason)
	default:
		go func() {
			<-entry.written
			c.mu.Lock()
			c.cancelRequestLocked(entry.proc, id, reason)
			c.mu.Unlock()
		}()
	}
}

// sendRequest publishes a JSON-RPC request and waits for its response, the
// caller's ctx (Esc) or the per-request timeout. The request line is written off
// the state lock in its own goroutine: a full/blocked stdin pipe must never hide
// ctx.Done(), so an aborted call still returns at once and still tells the server
// to cancel the request.
func (c *CtxmodeClient) sendRequest(ctx context.Context, method string, params map[string]any, timeoutMS int) (mcpResult, error) {
	c.mu.Lock()
	if c.proc == nil {
		c.mu.Unlock()
		return mcpResult{}, errors.New("ctxmode not running")
	}
	proc := c.proc
	c.requestID++
	id := c.requestID
	entry := &pendingRequest{done: make(chan reply, 1), written: make(chan struct{}), proc: proc}
	c.pending[id] = entry
	c.mu.Unlock()
	go c.writeRequest(proc, id, method, params, entry)
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	// Cancellation watcher: ctx is observed in its own goroutine, and reqDone is
	// closed by defer on every return path so a call that settles on its own
	// (response or timeout) never strands the watcher on a ctx that is never
	// cancelled.
	ctxDone := make(chan struct{})
	reqDone := make(chan struct{})
	defer close(reqDone)
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				close(ctxDone)
			case <-reqDone:
			}
		}()
	}
	var reason string
	var endErr error
	select {
	case r := <-entry.done:
		return r.result, r.err
	case <-ctxDone:
		reason = "tool call aborted by user (esc)"
		endErr = errors.New(cancelledMessage)
	case <-timer.C:
		reason = fmt.Sprintf("client gave up after %dms", timeoutMS)
		endErr = fmt.Errorf("ctxmode %s timed out after %dms", method, timeoutMS)
	}
	c.mu.Lock()
	if c.pending[id] != nil {
		// Esc notifies before removal; timeout removes before notifying (TS ordering).
		if ctx.Err() != nil {
			c.cancelPendingLocked(entry, id, reason)
			delete(c.pending, id)
		} else {
			delete(c.pending, id)
			c.cancelPendingLocked(entry, id, reason)
		}
		c.mu.Unlock()
		return mcpResult{}, endErr
	}
	c.mu.Unlock()
	r := <-entry.done
	return r.result, r.err // response/cleanup already won; no late cancellation
}

// writeRequest puts the request line on the wire and marks entry.written, which
// also gates the cancel notification (so it can never precede the request). A
// failed write rejects the still-pending call with that error.
func (c *CtxmodeClient) writeRequest(proc bridgeProc, id int, method string, params map[string]any, entry *pendingRequest) {
	err := c.writeToProc(proc, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	close(entry.written)
	if err == nil {
		return
	}
	c.mu.Lock()
	pending := c.pending[id] == entry
	if pending {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if pending {
		entry.done <- reply{err: err}
	}
}
func timeoutForTool(name string, args map[string]any) int {
	from := 0
	switch n := args["timeout_ms"].(type) {
	case float64:
		if n > 0 {
			from = int(n)
		}
	case int:
		if n > 0 {
			from = n
		}
	}
	base := envPositiveNumber("CTXMODE_REQUEST_TIMEOUT_MS", 120000)
	action, _ := args["action"].(string)
	budget := func(fallback, buffer int) {
		if from == 0 {
			from = fallback
		}
		base = max(base, from+buffer)
	}
	switch {
	case name == "ctx_run" && action == "run_task":
		budget(300000, 30000)
	case name == "ctx_run":
		budget(120000, 30000)
	case name == "ctx_kb" && action == "fetch":
		budget(150000, 30000)
	case name == "ctx_bg" && action == "wait":
		budget(60000, 15000)
	}
	return min(base, 3660000)
}
func (c *CtxmodeClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return "", errors.New(cancelledMessage)
	}
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		if err := c.Start(); err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", errors.New(cancelledMessage)
		}
	}
	c.mu.Lock()
	initialized = c.initialized
	c.mu.Unlock()
	if !initialized {
		return "", errors.New("ctxmode not initialized")
	}
	result, err := c.sendRequest(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, timeoutForTool(name, args))
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "cancelled") || strings.Contains(msg, "timed out") {
			return "", err
		}
		if isDisconnectMessage(msg) {
			// Reconnect without replaying tools/call (execute/batch are not
			// idempotent), but never let that handshake swallow an Esc.
			if c.reconnect(ctx) {
				return "", errors.New(cancelledMessage)
			}
		}
		return "", err
	}
	c.mu.Lock()
	c.restartAttempts = 0
	c.mu.Unlock()
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		texts = append(texts, content.Text)
	}
	return CompressToolText(strings.Join(texts, "\n")), nil
}

// isDisconnectMessage reports errors that mean the connection is gone; only those
// trigger a reconnect (never a replay of the failed tools/call).
func isDisconnectMessage(msg string) bool {
	return strings.Contains(msg, "not running") || strings.Contains(msg, "disconnected")
}

// reconnect re-establishes the connection after a disconnect. The handshake runs
// in its own goroutine so a user Esc (ctx.Done) ends the call at once instead of
// waiting out up to two START_TIMEOUT_MS windows. When ctx wins the race the
// in-flight handshake child is disposed of rather than left to finish its own
// timeout: shutDownProc marks the process intentional (so no auto-restart) but
// never sets c.stopped, which stays reserved for an explicit /ctxmode-stop.
// Returns true when ctx won the race.
func (c *CtxmodeClient) reconnect(ctx context.Context) bool {
	started := make(chan error, 1)
	go func() { started <- c.Start() }()
	select {
	case err := <-started:
		if err != nil {
			DiagLog("reconnect after disconnect failed: " + err.Error())
		}
		return false
	case <-ctx.Done():
		// The handshake goroutine may have published its process after the
		// check below; shut it down again when it reports back.
		c.shutDownProc()
		go func() {
			if err := <-started; err != nil {
				DiagLog("reconnect after disconnect failed: " + err.Error())
			}
			c.shutDownProc()
		}()
		return true
	}
}
