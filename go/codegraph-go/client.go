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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	stderrKeepLines = 20
	// The exit-diagnostic stderr tail is at most this many lines and this
	// many bytes in total (separators included), so a flooded daemon cannot
	// grow the tool-result error without bound.
	diagSummaryLines = 8
	diagSummaryBytes = 512
	// Each captured stderr line is rune-truncated so UTF-8 is never cut.
	diagLineRunes = 200
	// An unterminated stderr fragment may grow to this many bytes; beyond
	// that the sanitized head is committed as one truncated record and the
	// rest of the line is discarded up to the next newline (R1/R7).
	diagCarryBytes = 4096
	// After Wait() returns, readers get this long to drain what the child
	// wrote before exiting; descendants may hold the pipes open longer.
	drainGrace = 300 * time.Millisecond
	// Bounded retirement of an orphaned process whose readers ended
	// (stdout EOF / stderr loss) while the child stays alive: first a short
	// grace for the monitor to reap a naturally-exiting child, then the
	// same TERM/KILL bounds apply.
	orphanNudgeGrace = 200 * time.Millisecond
	// stop()/abortStart() bound termination: a SIGTERM grace, then SIGKILL
	// and a final reap bound so a wedged child cannot stall the caller.
	stopGrace = time.Second
	killGrace = 2 * time.Second
	// The diagnostic sink queue and the per-process stdin write queue are
	// bounded: backpressure drops records instead of blocking lifecycle
	// paths (R5) and new requests fail fast instead of queueing forever
	// behind a wedged write (R3).
	diagQueueDepth  = 256
	writeQueueDepth = 64
)

// diagQueue is the single bounded channel feeding the diagnostic sink.
// Enqueue never blocks: when full, the oldest pending record is dropped so
// the newest survives, and the drop count is reported once after draining.
var (
	diagQueue     = make(chan string, diagQueueDepth)
	diagSync      = make(chan chan struct{})
	diagDropped   atomic.Uint64
	diagDropDirty atomic.Bool
)

func init() { go diagSink() }

// diagOut is the DEBUG/drop-report destination — the parent process's own
// stderr by default. Tests swap it through diagSetOut (mutex-guarded) so an
// unread pipe can exercise sink backpressure without racing on os.Stderr.
var (
	diagOutMu sync.Mutex
	diagOut   io.Writer = os.Stderr
)

func diagSetOut(w io.Writer) {
	diagOutMu.Lock()
	diagOut = w
	diagOutMu.Unlock()
}

func diagWriteErr(msg string) {
	diagOutMu.Lock()
	fmt.Fprintln(diagOut, "[codegraph-go] "+msg)
	diagOutMu.Unlock()
}

// diagFlush drains everything queued so far: tests use it before tearing
// down an isolated HOME so the async sink cannot recreate files inside a
// TempDir after RemoveAll.
func diagFlush() {
	ack := make(chan struct{})
	select {
	case diagSync <- ack:
		<-ack
	case <-time.After(5 * time.Second):
	}
}

// diagSink is the only writer of the diagnostic outputs (log file under the
// agent dir, or the parent's stderr in DEBUG). It is a single goroutine for
// the life of the process — no per-line goroutines, no unbounded growth —
// and it holds no lock that lifecycle code can wait on, so a slow or
// unread sink can never stall stderr readers, monitors or stop() (R5). The
// parent's stderr is only written to in DEBUG, never closed or redirected.
func diagSink() {
	for {
		var msg string
		select {
		case ack := <-diagSync:
			for len(diagQueue) > 0 {
				diagWrite(<-diagQueue)
			}
			close(ack)
			continue
		case msg = <-diagQueue:
		}
		diagWrite(msg)
	}
}

func diagWrite(msg string) {
	if os.Getenv("CODEGRAPH_GO_DEBUG") != "" {
		diagWriteErr(msg)
	} else {
		// resolveAgentDir already covers host-info/env overrides and
		// ~/.pig/agent; it returns "" only when no home directory
		// resolves, where /tmp stands in (the TS `os.homedir() ||
		// "/tmp"` shape).
		base := resolveAgentDir()
		if base == "" {
			base = filepath.Join("/tmp", ".pig", "agent")
		}
		dir := filepath.Join(base, "logs")
		if os.MkdirAll(dir, 0755) == nil {
			if f, err := os.OpenFile(filepath.Join(dir, "codegraph-go.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				fmt.Fprintf(f, "%s [codegraph-go] %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)
				f.Close()
			}
		}
	}
	if diagDropDirty.CompareAndSwap(true, false) {
		if n := diagDropped.Swap(0); n > 0 {
			diagWriteErr(fmt.Sprintf("diagnostic log dropped %d records (sink backpressure)", n))
		}
	}
}

// diagLog enqueues one diagnostic record. Enqueue is non-blocking and can
// drop under backpressure, so calling it never stalls request, reader,
// monitor or stop paths (R5); dropped records are lost — the sink never
// claims lossless logging. Sanitization happens at CAPTURE, not here:
// untrusted free-form text (stderr lines, daemon RPC error messages) is
// masked before it is composed into a record, while structured fields
// composed by callers (workdir, bin, argv, exit codes, OS error strings)
// keep their true values — "脱敏" must never rewrite a real path (R2).
func diagLog(msg string) {
	select {
	case diagQueue <- msg:
		return
	default:
	}
	// Queue full: drop the oldest pending record so the newest (usually
	// the fatal line) still reaches the sink.
	select {
	case <-diagQueue:
		diagDropped.Add(1)
		diagDropDirty.Store(true)
	default:
	}
	select {
	case diagQueue <- msg:
	default:
		diagDropped.Add(1)
		diagDropDirty.Store(true)
	}
}

var uninterestingStderrRE = regexp.MustCompile(`(?i)\blevel=(?:INFO|DEBUG)\b|"level"\s*:\s*"(?:INFO|DEBUG)"`)

func isInterestingStderrLine(line string) bool { return !uninterestingStderrRE.MatchString(line) }

// Mask credential-shaped data before stderr excerpts or daemon free-form
// error text reach tool results or the diagnostic log. Sanitization runs on
// complete records only — never on a carry fragment that may be extended by
// the next chunk — and control characters are normalized BEFORE credential
// shapes are matched, so a "TO<KEN" zero-width splice cannot recombine into
// an unmasked key (R1). Only values are masked so failures stay
// attributable. Coverage is explicitly known-shape based — arbitrary
// unknown ciphertext is not claimed to be caught.
var (
	// C0/C1 controls, DEL, BOM/ZWSP and bidi overrides are stripped by rune
	// — never by byte — so UTF-8 stays intact and no terminal control
	// sequence can be assembled from a long stderr line. Newline and tab
	// survive (line splitting happens before this runs).
	diagCtlRE = regexp.MustCompile("[\x00-\x08\x0b-\x1f\u007f-\u009f\ufeff\u200b\u200c\u200d\u2060\u202a-\u202e\u2066-\u2069]")

	// KEY=VALUE / KEY:VALUE / KEY="v a l u e" / KEY='v a l u e'. Quoted
	// values may contain spaces; a quote that opens and never closes masks
	// the rest of the line — for BOTH quote kinds — instead of leaking the
	// tail word by word (R1).
	diagSecretRE = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:token|secret|password|passwd|api_?key|credential)[a-z0-9_]*)(["']?[ \t]*[:=][ \t]*)("[^"]*"|'[^']*'|"[^\n"]*|'[^\n']*|\S+)`)
	// Authorization headers of any scheme (Bearer, Basic, ...): the scheme
	// word stays visible so the context survives, the token is masked.
	diagAuthRE = regexp.MustCompile(`(?i)\b(authorization[ \t]*[:=][ \t]*[a-z]+[ \t]+)(\S+)`)
	// userinfo in URLs — scheme://user:password@host — masks the password
	// field only, up to the '@'.
	diagUserinfoRE = regexp.MustCompile(`\b([a-z][a-z0-9+.\-]*://[^/\s:@]+:)([^@\s]*)@`)
)

// normalizeDiagText strips control/bidi/zero-width characters FIRST so no
// credential shape can hide behind spliced control bytes; the result is
// safe to match credential patterns on and safe to emit (R1).
func normalizeDiagText(s string) string {
	return diagCtlRE.ReplaceAllString(s, "")
}

// sanitizeDiagText normalizes control characters, then masks known
// credential shapes. Call it on complete records only — a fragment that
// may still grow must stay raw until it is committed (R1).
func sanitizeDiagText(s string) string {
	s = normalizeDiagText(s)
	s = diagSecretRE.ReplaceAllString(s, "${1}${2}REDACTED")
	s = diagAuthRE.ReplaceAllString(s, "${1}REDACTED")
	s = diagUserinfoRE.ReplaceAllString(s, "${1}REDACTED@")
	return s
}

// truncateRunes bounds one stderr line for diagnostics without cutting a
// UTF-8 sequence.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// truncateBytesHead keeps the head of s within budget bytes without cutting
// a UTF-8 sequence; a trailing ellipsis marks the dropped tail. Used for
// over-long stderr fragments: the head carries the key context (credential
// shapes in it stay recognizable for masking), while the remainder of the
// line is discarded rather than re-assembled later (R1).
func truncateBytesHead(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(s) <= budget {
		return s
	}
	if budget <= len("…") {
		return s[:budget]
	}
	s = s[:budget-len("…")]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// truncateBytesTail keeps the tail of s within budget bytes without cutting
// a UTF-8 sequence; a leading ellipsis marks the dropped head. The newest
// bytes matter for a failure tail, so truncation drops from the front.
func truncateBytesTail(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(s) <= budget {
		return s
	}
	if budget <= len("…") {
		return s[len(s)-budget:]
	}
	s = s[len(s)-(budget-len("…")):]
	for len(s) > 0 && (s[0]&0xC0) == 0x80 {
		s = s[1:]
	}
	return "…" + s
}

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

// writeReq is one queued stdin write. deadline is the requester's absolute
// deadline, so queueing AND the actual write share the request budget: work
// that would outlive its own requester is dropped, and each Write is
// bounded by SetWriteDeadline (R3).
type writeReq struct {
	b        []byte
	deadline time.Time
}

type clientProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	writeMu  sync.Mutex // serializes the stdin writer; also guards quitClosed
	writeCh  chan writeReq
	quit     chan struct{} // closed by pipeTeardown to retire the writer
	quitOnce sync.Once
	stderr   []string
	carry    string // raw (normalized, un-sanitized) unterminated fragment
	// carryTrunc is set once the current line already committed its bounded
	// head; the remainder of that line is discarded up to the newline — no
	// re-assembly past the bound, no unbounded carry (R1).
	carryTrunc  bool
	intentional bool
	phase       string // spawn|initialize|tools/list|running, under c.mu
	exitErr     error  // this process's own exit/write diagnostic, under c.mu
	done        chan struct{}
}
type startFlight struct {
	done chan struct{}
	err  error
}

// exitError is the bounded diagnostic surfaced when the codegraph-go
// subprocess exits unexpectedly: startup phase or "running", exit
// code/signal, target workdir and a sanitized stderr tail. It replaces the
// bare "disconnected" so tool callers can explain the failure. The text
// keeps "disconnected" so callTool's reconnect regex still applies.
type exitError struct {
	phase   string
	code    string // decimal exit code; "unknown" when Wait reports none
	signal  string
	workdir string
	summary string
	wait    string // cmd.Wait() error text, e.g. "exit status 3"
}

func (e *exitError) Error() string {
	s := fmt.Sprintf("codegraph-go disconnected (process exited): phase=%s exit=%s", e.phase, e.code)
	if e.signal != "" {
		s += " signal=" + e.signal
	}
	s += " workdir=" + e.workdir
	if e.wait != "" {
		s += " wait=" + e.wait
	}
	if e.summary != "" {
		s += "\nstderr tail:\n" + e.summary
	}
	return s
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
	// lastExitErr keeps the most recent unexpected-exit diagnostic after
	// c.proc is already nil, so a caller that lost the identity race still
	// receives the real exit report instead of a bare "not running".
	lastExitErr error
	// Command injection is only used by the subprocess test fixture.
	command func(string, ...string) *exec.Cmd
	// testAfterMonitorWait, when set, runs inside the monitor after
	// cmd.Wait() returns and the readers are drained, before the exit
	// diagnostic is built: a deterministic barrier for the
	// reader→monitor→doStart interleave (R6). Test-only.
	testAfterMonitorWait func()
	// testBeforeIdentityCheck, when set, runs in doStart after the
	// tools/list response arrived and before the proc-identity check (R6).
	testBeforeIdentityCheck func()
	// testOrphanHook, when set, runs inside retireOrphaned after the
	// initial identity check and before the nudge grace (R6). Test-only.
	testOrphanHook func()
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
	// stdout/stderr use explicit os.Pipe files: Wait() never closes a
	// user-supplied file, so the monitor can reap the process promptly and
	// the readers still drain everything the child wrote before exiting.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		stdoutR.Close()
		stdoutW.Close()
		return err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	p := &clientProcess{cmd: cmd, stdin: stdin, phase: "spawn", done: make(chan struct{}),
		writeCh: make(chan writeReq, writeQueueDepth), quit: make(chan struct{})}
	c.mu.Lock()
	c.proc = p
	err = cmd.Start()
	c.mu.Unlock()
	if err != nil {
		stdin.Close()
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		p.quitOnce.Do(func() { close(p.quit) })
		// workdir/bin are structured fields — verbatim, never masked; the
		// wrapped OS error is free-form text handled by the same sinks (R2).
		spawnErr := fmt.Errorf("codegraph-go spawn failed (phase=spawn workdir=%s bin=%s): %w", c.workdir, bin, err)
		diagLog("process error: " + spawnErr.Error())
		c.mu.Lock()
		c.cleanupLocked(p, spawnErr)
		c.mu.Unlock()
		close(p.done)
		return spawnErr
	}
	// The parent side of both pipes is never written again; dropping the
	// write ends keeps our fd set honest (the child owns its own copies).
	stdoutW.Close()
	stderrW.Close()
	go c.requestWriter(p)
	readDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() { defer close(readDone); c.readResponses(p, stdoutR) }()
	go func() { defer close(stderrDone); c.readStderr(p, stderrR) }()
	go func() {
		// Reap as soon as the process exits — not when the pipes reach EOF,
		// which descendants may delay indefinitely — then give the readers a
		// bounded grace to drain the kernel buffers so the exit diagnostic
		// still sees the child's last stderr. Wait() here is the ONLY Wait
		// on this child; p.done closes exactly once when it is done (R4).
		waitErr := cmd.Wait()
		deadline := time.Now().Add(drainGrace)
		select {
		case <-stderrDone:
		case <-time.After(time.Until(deadline)):
		}
		select {
		case <-readDone:
		case <-time.After(time.Until(deadline)):
		}
		// Detach readers stuck behind a descendant that holds the pipes
		// open; the goroutines exit on read error instead of leaking.
		stdoutR.Close()
		stderrR.Close()
		if c.testAfterMonitorWait != nil {
			c.testAfterMonitorWait()
		}
		c.mu.Lock()
		if c.proc == p && !p.intentional {
			diag := c.buildExitDiagLocked(p, waitErr)
			p.exitErr = diag
			code := "0"
			signal := ""
			if d, ok := diag.(*exitError); ok {
				code = d.code
				signal = d.signal
			}
			if waitErr == nil {
				// Silent exit still restarts lazily on next call; stderr tail
				// rides the diagnostic instead of the TUI.
				diagLog(fmt.Sprintf("unexpected exit code=%s signal=%s (silent to TUI; next call will restart)", code, signal))
				c.clearStderrBufferLocked(p)
			} else {
				diagLog(fmt.Sprintf("process exited abnormally code=%s signal=%s", code, signal))
				c.dumpStderrBufferLocked(p, true)
			}
			c.cleanupLocked(p, diag)
		} else {
			// Intentional stop or already-retired identity: still record
			// THIS process's real Wait outcome so its exitError holders
			// report its own truth, never a successor's (R4/R6). An
			// intentional teardown does not overwrite lastExitErr.
			if p.exitErr == nil {
				p.exitErr = c.buildExitDiagLocked(p, waitErr)
				if !p.intentional {
					c.lastExitErr = p.exitErr
				}
			}
			c.clearStderrBufferLocked(p)
		}
		c.mu.Unlock()
		c.pipeTeardown(p)
		close(p.done)
	}()
	timeout := time.Duration(envNumber("CODEGRAPH_GO_START_TIMEOUT_MS", 30000)) * time.Millisecond
	c.mu.Lock()
	p.phase = "initialize"
	c.mu.Unlock()
	initResult, err := c.sendRequest("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "pi-codegraph-go", "version": "0.4.0"}}, timeout)
	if err != nil {
		// A rejected/timed-out handshake must not leak the child: mark the
		// stop intentional, drop the process reference, then bound its
		// termination. Callers never need a manual stop() after this.
		c.abortStart(p, err)
		return err
	}
	c.mu.Lock()
	c.serverInstructions = initResult.Instructions
	c.mu.Unlock()
	c.sendNotification("notifications/initialized", nil)
	c.mu.Lock()
	p.phase = "tools/list"
	c.mu.Unlock()
	list, err := c.sendRequest("tools/list", map[string]any{}, timeout)
	if err != nil {
		c.abortStart(p, err)
		return err
	}
	if c.testBeforeIdentityCheck != nil {
		c.testBeforeIdentityCheck()
	}
	c.mu.Lock()
	if c.proc != p {
		// The monitor won the race to retirement (the child died between
		// the tools/list response and this check). Surface THAT process's
		// own exit report — never a bare "disconnected", never a
		// successor's error (R6). exit=unknown is reported as-is when Wait
		// could not establish a code; nothing is fabricated.
		exitErr := p.exitErr
		if exitErr == nil {
			exitErr = c.lastExitErr
		}
		c.mu.Unlock()
		if exitErr != nil {
			return exitErr
		}
		return errors.New("codegraph-go disconnected")
	}
	c.tools = list.Tools
	c.initialized = true
	p.phase = "running"
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
					// The daemon's own error text is untrusted free-form
					// data; it is masked to the same diagnostic口径 as
					// stderr at capture (R2). The leaf-error shape is
					// unchanged, so caller string matching still works.
					resp.err = errors.New(sanitizeDiagText(msg.Error.Message))
				}
				ch <- resp
			}
		}
		c.mu.Unlock()
	}
	// stdout ended (EOF, read error, or detached by the monitor): if this
	// process is still the current one, its transport is gone — retire it
	// in bounded time so a live child is never left unreaped behind a dead
	// pipe (R4). No-op when the monitor or a successor already owns the
	// identity.
	go c.retireOrphaned(p)
}
func (c *CodeGraphClient) readStderr(p *clientProcess, r io.Reader) {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			c.consumeStderr(p, string(b[:n]))
		}
		if err != nil {
			break
		}
	}
	// Same orphan check as readResponses: stderr can end first when the
	// child closes its own stderr while staying alive (R4).
	go c.retireOrphaned(p)
}

// consumeStderr splits raw stderr into lines. Fragments stay RAW in carry —
// sanitizing a carry that later chunks would extend can split a credential
// shape into one masked half and one leaked half, so masking only ever runs
// on complete committed records (R1). Control characters are normalized at
// ingest so "TO<KEN" splices cannot recombine into unmasked keys. A line
// over diagCarryBytes commits its normalized+sanitized head once and the
// remainder is discarded up to the newline: state is never forgotten AND
// never re-assembled past the bound.
func (c *CodeGraphClient) consumeStderr(p *clientProcess, text string) {
	text = normalizeDiagText(text)
	debug := os.Getenv("CODEGRAPH_GO_DEBUG") != ""
	c.mu.Lock()
	var mirror []string
	for len(text) > 0 {
		if p.carryTrunc {
			// Rest of an over-long line: discard to the next newline.
			i := strings.IndexByte(text, '\n')
			if i < 0 {
				break
			}
			p.carryTrunc = false
			text = text[i+1:]
			continue
		}
		i := strings.IndexByte(text, '\n')
		var line string
		var complete bool
		if i < 0 {
			line = p.carry + text
			text = ""
		} else {
			line = p.carry + text[:i]
			text = text[i+1:]
			complete = true
		}
		if len(line) > diagCarryBytes {
			// Over bound: commit the bounded head now — already normalized,
			// sanitized here on a complete record — and drop the rest of
			// this line. The mask applied to the head cannot be reopened;
			// what was masked is masked for good.
			if pushed := c.pushStderrLineLocked(p, sanitizeDiagText(strings.TrimRightFunc(truncateBytesHead(line, diagCarryBytes), isJSSpace))); pushed != "" && debug {
				mirror = append(mirror, pushed)
			}
			p.carry = ""
			p.carryTrunc = !complete
			continue
		}
		if complete {
			if pushed := c.pushStderrLineLocked(p, sanitizeDiagText(strings.TrimRightFunc(line, isJSSpace))); pushed != "" && debug {
				mirror = append(mirror, pushed)
			}
			p.carry = ""
		} else {
			p.carry = line
		}
	}
	c.mu.Unlock()
	// DEBUG mirrors the same committed (sanitized, rune-truncated) record
	// the ring holds — never raw chunks — and goes through the bounded
	// diagQueue, so an unread parent stderr cannot stall this reader (R5).
	for _, line := range mirror {
		diagLog(line)
	}
}

// pushStderrLineLocked appends one sanitized line to the stderr ring and
// returns the stored (rune-truncated) record so mirrors use the identical
// text; "" when the line was empty.
func (c *CodeGraphClient) pushStderrLineLocked(p *clientProcess, line string) string {
	if line == "" {
		return ""
	}
	line = truncateRunes(line, diagLineRunes)
	p.stderr = append(p.stderr, line)
	if len(p.stderr) > stderrKeepLines {
		p.stderr = p.stderr[1:]
	}
	return line
}
func (c *CodeGraphClient) flushStderrCarryLocked(p *clientProcess) {
	// Flushing commits the raw carry as a complete record — sanitized here,
	// at the only point it is guaranteed not to grow further (R1).
	c.pushStderrLineLocked(p, sanitizeDiagText(strings.TrimRightFunc(p.carry, isJSSpace)))
	p.carry = ""
	p.carryTrunc = false
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

// stderrDiagSummaryLocked renders the current stderr ring into a bounded,
// sanitized tail for error messages. Unlike dumpStderrBufferLocked it does
// NOT consume the ring, so the diagnostic log still sees the same lines.
// Selection mirrors the log dump: interesting lines first, falling back to
// the raw tail so the fatal message is never filtered out.
func (c *CodeGraphClient) stderrDiagSummaryLocked(p *clientProcess) string {
	c.flushStderrCarryLocked(p)
	all := p.stderr
	candidates := []string{}
	for _, line := range all {
		if isInterestingStderrLine(line) {
			candidates = append(candidates, line)
		}
	}
	if len(candidates) == 0 {
		candidates = all
	}
	// Take the tail while keeping line order; every byte — separators
	// included — counts against the budget so the summary can never
	// exceed diagSummaryBytes regardless of line sizes. The newest line
	// always participates (usually the actual failure), even when it
	// alone exceeds the budget.
	picked := []string{}
	total := 0
	for i := len(candidates) - 1; i >= 0 && len(picked) < diagSummaryLines; i-- {
		line := candidates[i]
		add := len(line)
		if len(picked) > 0 {
			add++ // the '\n' separator it joins with
		}
		if total+add > diagSummaryBytes && len(picked) > 0 {
			break
		}
		total += add
		picked = append([]string{line}, picked...)
	}
	return truncateBytesTail(strings.Join(picked, "\n"), diagSummaryBytes)
}

// buildExitDiagLocked constructs the process-exit diagnostic carried to
// pending callers and the caller of start(). Code is the decimal exit code,
// "0" for a clean exit, and "unknown" when Wait reports no exit code — it
// is never fabricated for signaled/unreleased states, where signal= and
// wait= carry the truth instead. workdir is a structured field and stays
// verbatim; only the daemon-provided stderr tail passes through
// sanitization, which already happened at capture (R1/R2).
func (c *CodeGraphClient) buildExitDiagLocked(p *clientProcess, waitErr error) error {
	code := "0"
	signal := ""
	wait := ""
	if waitErr != nil {
		code = "unknown"
		wait = waitErr.Error()
	}
	if cmd := p.cmd; cmd != nil && cmd.ProcessState != nil {
		if ec := cmd.ProcessState.ExitCode(); ec >= 0 {
			code = strconv.Itoa(ec)
		}
		if s, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && s.Signaled() {
			signal = s.Signal().String()
		}
	}
	phase := p.phase
	if phase == "" {
		phase = "unknown"
	}
	return &exitError{phase: phase, code: code, signal: signal, workdir: c.workdir, summary: c.stderrDiagSummaryLocked(p), wait: wait}
}
func (c *CodeGraphClient) sendRequest(method string, params map[string]any, timeout time.Duration) (MCPResult, error) {
	c.mu.Lock()
	p := c.proc
	if p == nil {
		// If the process just died, the identity race must not mask its exit
		// report behind a bare "not running".
		exitErr := c.lastExitErr
		c.mu.Unlock()
		if exitErr != nil {
			return MCPResult{}, exitErr
		}
		return MCPResult{}, errors.New("codegraph-go not running")
	}
	c.requestID++
	id := c.requestID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, err := json.Marshal(msg)
	if err != nil {
		// Marshal failure must not park its pending entry either.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return MCPResult{}, err
	}
	// The request deadline covers queueing AND the actual stdin write: the
	// writer drops work whose deadline already passed and bounds each Write
	// with SetWriteDeadline, while this timer returns control to the caller
	// on time either way — a full child pipe can no longer park the
	// request forever behind writeMu (R3).
	deadline := time.Now().Add(max(time.Millisecond, timeout))
	timer := time.NewTimer(max(time.Millisecond, timeout))
	defer timer.Stop()
	if werr := c.enqueueWrite(p, writeReq{b: append(b, '\n'), deadline: deadline}); werr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return MCPResult{}, werr
	}
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

// enqueueWrite offers one request to the process's stdin writer and returns
// nil once it is queued or written. A nil writeCh means a hand-built fixture
// process without a writer goroutine — the write happens synchronously so
// its error reaches the caller. On any failure path the process is retired
// inside bounded time and the caller gets its real diagnostic (R3/R4).
func (c *CodeGraphClient) enqueueWrite(p *clientProcess, req writeReq) error {
	if p.writeCh == nil {
		if err := c.writeNow(p, req); err != nil {
			return c.writeFailed(p, err)
		}
		return nil
	}
	// Fast path: queue has room right now.
	select {
	case p.writeCh <- req:
		return nil
	default:
	}
	// Slow path: wait for queue space, the writer's retirement, or this
	// request's own deadline — queueing is inside the deadline too (R3).
	wait := time.Until(req.deadline)
	if wait <= 0 {
		return c.writeFailed(p, errors.New("stdin write queue did not drain before the request deadline"))
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case p.writeCh <- req:
		return nil
	case <-p.quit:
		return c.exitErrFor(p)
	case <-t.C:
		return c.writeFailed(p, errors.New("stdin write queue did not drain before the request deadline"))
	}
}

// requestWriter is the single stdin writer for p. Every queued item carries
// its requester's absolute deadline: expired work is dropped, and
// SetWriteDeadline keeps one wedged Write from outliving its request — a
// full child pipe unblocks within the request timeout instead of hanging
// forever on writeMu (R3). Exits when pipeTeardown closes p.quit.
func (c *CodeGraphClient) requestWriter(p *clientProcess) {
	failed := false
	for {
		select {
		case <-p.quit:
			return
		case req := <-p.writeCh:
			if failed || time.Now().After(req.deadline) {
				continue // requester already timed out / transport dead
			}
			if err := c.writeNow(p, req); err != nil {
				failed = true
				c.writeFailed(p, err)
			}
		}
	}
}

// writeNow performs one bounded write. The deadline is installed inside
// writeMu so it covers exactly this request's write, not a successor's.
func (c *CodeGraphClient) writeNow(p *clientProcess, req writeReq) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if d, ok := p.stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		// Deadline scoped to THIS request; cleared after the write so the
		// pipe returns to blocking mode for whoever comes next.
		_ = d.SetWriteDeadline(req.deadline)
		defer func() { _ = d.SetWriteDeadline(time.Time{}) }()
	}
	_, err := p.stdin.Write(req.b)
	return err
}

// exitErrFor returns the diagnostic already recorded for p (or the most
// recent one), preferring the process's own exit report over a bare
// "disconnected" (R6).
func (c *CodeGraphClient) exitErrFor(p *clientProcess) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.exitErr != nil {
		return p.exitErr
	}
	if c.lastExitErr != nil {
		return c.lastExitErr
	}
	return errors.New("codegraph-go disconnected")
}

// writeFailed retires p after an unrecoverable stdin write error or queue
// starvation: pending requests get the real diagnostic, the process is
// terminated inside the same bounds as stop(), and a successor identity is
// never disturbed (R3/R4). Returns the error the requester should see —
// the process's own exit report when the monitor recorded one.
func (c *CodeGraphClient) writeFailed(p *clientProcess, cause error) error {
	c.mu.Lock()
	if p.exitErr == nil {
		p.exitErr = fmt.Errorf("codegraph-go disconnected (stdin write failed, phase=%s workdir=%s): %w", p.phase, c.workdir, cause)
	}
	werr := p.exitErr
	p.intentional = true // we own termination now; monitor skips re-diagnosis
	c.cleanupLocked(p, werr)
	c.mu.Unlock()
	diagLog(werr.Error())
	c.terminateProc(p)
	return werr
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
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return
	}
	_ = c.enqueueWrite(p, writeReq{b: append(b, '\n'), deadline: time.Now().Add(30 * time.Second)})
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

// cleanupLocked drains pending requests with the exit diagnostic when the
// subprocess died unexpectedly, a write/spawn error for transport failures,
// or the bare "disconnected" for intentional stops.
func (c *CodeGraphClient) cleanupLocked(p *clientProcess, err error) {
	if c.proc != p {
		return
	}
	if err == nil {
		err = p.exitErr
	}
	if err == nil {
		err = errors.New("codegraph-go disconnected")
		c.lastExitErr = nil // intentional stop: the stale exit report is over
	} else {
		// Survive c.proc=nil so the next caller can still learn *why* the
		// process went away instead of a bare "not running".
		c.lastExitErr = err
	}
	for id, ch := range c.pending {
		ch <- response{err: err}
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
	c.cleanupLocked(p, nil)
	c.mu.Unlock()
	c.terminateProc(p)
}

// pipeTeardown releases p's stdin and retires its writer goroutine. stdin
// is closed FIRST — without waiting on writeMu — so a Write wedged on a
// full child pipe is released before the queue is wound down (R3/R4).
// Idempotent across every retirement path.
func (c *CodeGraphClient) pipeTeardown(p *clientProcess) {
	if p.stdin != nil {
		p.stdin.Close()
	}
	p.writeMu.Lock()
	if p.quit != nil {
		p.quitOnce.Do(func() { close(p.quit) })
	}
	p.writeMu.Unlock()
}

// retireOrphaned bounds the retirement of a process whose stdout or stderr
// ended while the child is still alive: a short grace lets the monitor
// reap a naturally-exiting child first, then the same TERM/KILL bounds
// apply. Only runs while p is still the current process — a successor
// identity is never touched, and only p's own PID is signaled (R4).
func (c *CodeGraphClient) retireOrphaned(p *clientProcess) {
	c.mu.Lock()
	if c.proc != p || p.intentional || p.cmd == nil || p.cmd.Process == nil {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if c.testOrphanHook != nil {
		c.testOrphanHook()
	}
	select {
	case <-p.done:
		return
	case <-time.After(orphanNudgeGrace):
	}
	c.mu.Lock()
	if c.proc != p || p.intentional {
		c.mu.Unlock()
		return
	}
	// Not marked intentional: the monitor still owns the real Wait outcome
	// and will record it into p.exitErr / lastExitErr when it reaps (R6).
	c.cleanupLocked(p, fmt.Errorf("codegraph-go disconnected (transport lost, phase=%s workdir=%s)", p.phase, c.workdir))
	c.mu.Unlock()
	c.terminateProc(p)
}

// terminateProc bounds the shutdown of a process this client owns. stdin
// and the write queue are torn down first — without blocking on a wedged
// writer — then SIGTERM gets a short grace before SIGKILL, and the reap
// (monitor close of p.done) is bounded too. Only p.cmd's own PID is
// signaled: no process group is created, so real external daemons are
// never touched. The reap responsibility stays with the monitor: clearing
// c.proc never drops it (R4).
func (c *CodeGraphClient) terminateProc(p *clientProcess) {
	if p == nil {
		return
	}
	c.pipeTeardown(p)
	if p.cmd == nil || p.cmd.Process == nil || p.cmd.ProcessState != nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(stopGrace):
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(killGrace):
		// Unkillable (e.g. D-state): give up waiting rather than block the
		// caller forever; the monitor goroutine still reaps on its own.
	}
}

// abortStart retires a half-started process whose handshake failed: pending
// requests are drained with the caller-visible error, c.proc is released,
// and the child is terminated inside the same bounds as stop() — a refused
// or timed-out start never leaks a running daemon.
func (c *CodeGraphClient) abortStart(p *clientProcess, err error) {
	c.mu.Lock()
	p.intentional = true
	c.cleanupLocked(p, err)
	c.mu.Unlock()
	c.terminateProc(p)
}
func startClientAt(workdir, note string) (*CodeGraphClient, error) {
	c := newCodeGraphClient(workdir, note)
	if err := c.start(); err != nil {
		diagLog("failed to start: " + err.Error())
		return nil, err
	}
	return c, nil
}
