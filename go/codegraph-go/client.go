package codegraphgo

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const stderrKeepLines = 20

var diagMu sync.Mutex

func diagLog(msg string) {
	line := "[codegraph-go] " + msg
	diagMu.Lock()
	defer diagMu.Unlock()
	if os.Getenv("CODEGRAPH_GO_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	h := homeDir()
	if h == "" {
		h = "/tmp"
	}
	dir := filepath.Join(h, ".pi", "agent", "logs")
	if os.MkdirAll(dir, 0755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "codegraph-go.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), line)
}

var uninterestingStderrRE = regexp.MustCompile(`(?i)\blevel=(?:INFO|DEBUG)\b|"level"\s*:\s*"(?:INFO|DEBUG)"`)

func isInterestingStderrLine(line string) bool { return !uninterestingStderrRE.MatchString(line) }

type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type MCPResult struct {
	Content      []MCPContent `json:"content"`
	Tools        []MCPTool    `json:"tools"`
	Instructions string       `json:"instructions"`
}
type mcpResponse struct {
	ID     *int      `json:"id"`
	Result MCPResult `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type response struct {
	result MCPResult
	err    error
}
type clientProcess struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	writeMu     sync.Mutex
	stderr      []string
	carry       string
	intentional bool
	done        chan struct{}
}
type startFlight struct {
	done chan struct{}
	err  error
}

// CodeGraphClient bridges newline-delimited JSON-RPC. Every mutable field is
// locked: PiG may run parallel tools, unlike the TS single-threaded event loop.
// Per-process identity prevents a retired process from cleaning up its successor.
type CodeGraphClient struct {
	mu                 sync.Mutex
	proc               *clientProcess
	requestID          int
	pending            map[int]chan response
	tools              []MCPTool
	initialized        bool
	serverInstructions string
	starting           *startFlight
	workdir            string
	workdirNote        string
	// Command injection is only used by the subprocess test fixture.
	command func(string, ...string) *exec.Cmd
}

func newCodeGraphClient(workdir, note string) *CodeGraphClient {
	return &CodeGraphClient{workdir: workdir, workdirNote: note, pending: map[int]chan response{}, command: exec.Command}
}
func (c *CodeGraphClient) start() error {
	c.mu.Lock()
	if c.proc != nil && c.initialized {
		c.mu.Unlock()
		return nil
	}
	if f := c.starting; f != nil {
		c.mu.Unlock()
		<-f.done
		return f.err
	}
	f := &startFlight{done: make(chan struct{})}
	c.starting = f
	c.mu.Unlock()
	err := c.doStart()
	c.mu.Lock()
	f.err = err
	c.starting = nil
	close(f.done)
	c.mu.Unlock()
	return err
}
func (c *CodeGraphClient) doStart() error {
	c.stop()
	bin := os.Getenv("CODEGRAPH_GO_BIN")
	if bin == "" {
		bin = "codegraph-go"
	}
	cmd := c.command(bin, "-workdir", c.workdir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return err
	}
	p := &clientProcess{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	c.mu.Lock()
	c.proc = p
	err = cmd.Start()
	c.mu.Unlock()
	if err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		diagLog("process error: " + err.Error())
		c.mu.Lock()
		c.cleanupLocked(p)
		c.mu.Unlock()
		close(p.done)
		return err
	}
	readDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() { defer close(readDone); c.readResponses(p, stdout) }()
	go func() { defer close(stderrDone); c.readStderr(p, stderr) }()
	go func() {
		// Drain pipes before Wait closes them (os/exec StdoutPipe contract).
		<-readDone
		<-stderrDone
		err := cmd.Wait()
		c.mu.Lock()
		if !p.intentional {
			if err == nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == -1) {
				signal := ""
				if s, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && s.Signaled() {
					signal = s.Signal().String()
				}
				code := "0"
				if err != nil {
					code = "null"
				}
				diagLog(fmt.Sprintf("unexpected exit code=%s signal=%s (silent to TUI; next call will restart)", code, signal))
				c.clearStderrBufferLocked(p)
			} else {
				diagLog(fmt.Sprintf("process exited abnormally code=%d signal=", cmd.ProcessState.ExitCode()))
				c.dumpStderrBufferLocked(p, true)
			}
		} else {
			c.clearStderrBufferLocked(p)
		}
		c.cleanupLocked(p)
		c.mu.Unlock()
		close(p.done)
	}()
	timeout := time.Duration(envNumber("CODEGRAPH_GO_START_TIMEOUT_MS", 30000)) * time.Millisecond
	initResult, err := c.sendRequest("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "pi-codegraph-go", "version": "0.4.0"}}, timeout)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.serverInstructions = initResult.Instructions
	c.mu.Unlock()
	c.sendNotification("notifications/initialized", nil)
	list, err := c.sendRequest("tools/list", map[string]any{}, timeout)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.proc != p {
		c.mu.Unlock()
		return errors.New("codegraph-go disconnected")
	}
	c.tools = list.Tools
	c.initialized = true
	c.mu.Unlock()
	diagLog(fmt.Sprintf("started workdir=%s (%s), %d tools, out≤%dc/%dL", c.workdir, c.workdirNote, len(list.Tools), outputChars(), outputLines()))
	return nil
}
func (c *CodeGraphClient) readResponses(p *clientProcess, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 64*1024*1024)
	for scanner.Scan() {
		var msg mcpResponse
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		c.mu.Lock()
		if c.proc == p {
			if ch := c.pending[*msg.ID]; ch != nil {
				delete(c.pending, *msg.ID)
				resp := response{result: msg.Result}
				if msg.Error != nil {
					resp.err = errors.New(msg.Error.Message)
				}
				ch <- resp
			}
		}
		c.mu.Unlock()
	}
}
func (c *CodeGraphClient) readStderr(p *clientProcess, r io.Reader) {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			c.consumeStderr(p, string(b[:n]))
		}
		if err != nil {
			return
		}
	}
}
func (c *CodeGraphClient) consumeStderr(p *clientProcess, text string) {
	if os.Getenv("CODEGRAPH_GO_DEBUG") != "" {
		diagLog(strings.TrimRightFunc(text, isJSSpace))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := strings.Split(p.carry+text, "\n")
	p.carry = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		c.pushStderrLineLocked(p, strings.TrimRightFunc(line, isJSSpace))
	}
}
func (c *CodeGraphClient) pushStderrLineLocked(p *clientProcess, line string) {
	if line == "" {
		return
	}
	p.stderr = append(p.stderr, line)
	if len(p.stderr) > stderrKeepLines {
		p.stderr = p.stderr[1:]
	}
}
func (c *CodeGraphClient) flushStderrCarryLocked(p *clientProcess) {
	c.pushStderrLineLocked(p, strings.TrimRightFunc(p.carry, isJSSpace))
	p.carry = ""
}
func (c *CodeGraphClient) clearStderrBufferLocked(p *clientProcess) {
	c.flushStderrCarryLocked(p)
	p.stderr = nil
}
func (c *CodeGraphClient) dumpStderrBufferLocked(p *clientProcess, force bool) {
	c.flushStderrCarryLocked(p)
	all := p.stderr
	p.stderr = nil
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
	diagLog(fmt.Sprintf("last stderr before exit (%d lines):", len(lines)))
	for _, line := range lines {
		diagLog("  " + line)
	}
}
func (c *CodeGraphClient) sendRequest(method string, params map[string]any, timeout time.Duration) (MCPResult, error) {
	c.mu.Lock()
	p := c.proc
	if p == nil {
		c.mu.Unlock()
		return MCPResult{}, errors.New("codegraph-go not running")
	}
	c.requestID++
	id := c.requestID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	c.write(p, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	// JS setTimeout treats non-positive delays as an immediate timer.
	timer := time.NewTimer(max(time.Millisecond, timeout))
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp.result, resp.err
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return MCPResult{}, fmt.Errorf("codegraph-go %s timed out after %dms", method, timeout/time.Millisecond)
	}
}
func (c *CodeGraphClient) write(p *clientProcess, msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	p.writeMu.Lock()
	_, err = p.stdin.Write(append(b, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		c.cleanupLocked(p)
		c.mu.Unlock()
	}
}
func (c *CodeGraphClient) sendNotification(method string, params map[string]any) {
	c.mu.Lock()
	p := c.proc
	c.mu.Unlock()
	if p == nil {
		return
	}
	if params == nil {
		params = map[string]any{}
	}
	c.write(p, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

var reconnectRE = regexp.MustCompile(`(?i)not running|disconnected|timed out`)

func (c *CodeGraphClient) callTool(name string, args map[string]any) (string, error) {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		if err := c.start(); err != nil {
			return "", err
		}
	}
	c.mu.Lock()
	initialized = c.initialized
	c.mu.Unlock()
	if !initialized {
		return "", errors.New("codegraph-go not initialized")
	}
	bounded := withToolDefaults(args)
	params := map[string]any{"name": name, "arguments": bounded}
	timeout := time.Duration(envNumber("CODEGRAPH_GO_REQUEST_TIMEOUT_MS", 120000)) * time.Millisecond
	result, err := c.sendRequest("tools/call", params, timeout)
	if err != nil && reconnectRE.MatchString(err.Error()) {
		if err = c.start(); err == nil {
			result, err = c.sendRequest("tools/call", params, timeout)
		}
	}
	if err != nil {
		return "", err
	}
	parts := []string{}
	for _, content := range result.Content {
		parts = append(parts, content.Text)
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = "no result"
	}
	return formatCleanText(compressToolText(text), wantsKeepCode(name, bounded)), nil
}
func (c *CodeGraphClient) getTools() []MCPTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MCPTool{}, c.tools...)
}
func (c *CodeGraphClient) getServerInstructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInstructions
}
func (c *CodeGraphClient) cleanupLocked(p *clientProcess) {
	if c.proc != p {
		return
	}
	for id, ch := range c.pending {
		ch <- response{err: errors.New("codegraph-go disconnected")}
		delete(c.pending, id)
	}
	c.proc = nil
	c.initialized = false
}
func (c *CodeGraphClient) stop() {
	c.mu.Lock()
	p := c.proc
	if p == nil {
		c.mu.Unlock()
		return
	}
	p.intentional = true
	c.clearStderrBufferLocked(p)
	c.cleanupLocked(p)
	c.mu.Unlock()
	p.writeMu.Lock()
	p.stdin.Close()
	p.writeMu.Unlock()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
}
func startClientAt(workdir, note string) (*CodeGraphClient, error) {
	c := newCodeGraphClient(workdir, note)
	if err := c.start(); err != nil {
		diagLog("failed to start: " + err.Error())
		return nil, err
	}
	return c, nil
}
