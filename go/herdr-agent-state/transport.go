package herdragentstate

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// Default timeouts matching TS implementation: 500ms for 1st attempt, 1500ms for 2nd attempt.
const (
	DefaultInitialTimeout = 500 * time.Millisecond
	DefaultRetryTimeout   = 1500 * time.Millisecond
)

// maxResponseFrameBytes is the maximum size of a single Herdr response frame.
// A frame that grows beyond this limit is rejected instead of being buffered
// without bound.
const maxResponseFrameBytes = 64 * 1024

var (
	// errEmptyResponseFrame is returned when the peer answered with an empty
	// frame (an empty line) or closed the connection without any payload.
	errEmptyResponseFrame = errors.New("herdr: empty response frame")
	// errResponseFrameTooLong is returned when a response frame exceeds
	// maxResponseFrameBytes.
	errResponseFrameTooLong = errors.New("herdr: response frame too long")
)

const (
	// SourcePi identifies this integration to Herdr. It must NOT be a source
	// Herdr already owns: Herdr ships a built-in "pi" integration whose source is
	// "herdr:pi", and a report from that source is silently dropped (Herdr answers
	// {"result":{"type":"ok"}} but applies nothing, so the pane keeps
	// agent_status "unknown"). The official guidance is the same: "--source
	// identifies your integration. Keep it stable and unique. Don't start it with
	// herdr:, because Herdr's own integrations use that prefix."
	SourcePi = "pig"
	// AgentPi is the agent label reported to Herdr/gateway. It must be "pi"
	// (the moshi gateway's canonicalAgentName whitelist only recognizes the
	// 22 official agents including "pi", and rejects "pig"), whereas
	// SourcePi is the integration source and stays "pig" so Herdr does not
	// silently drop the report.
	AgentPi = "pi"
)

// Sequence generator matching TS: let reportSeq = Date.now() * 1000; reportSeq += 1;
var reportSeq atomic.Uint64

func init() {
	reportSeq.Store(uint64(time.Now().UnixMilli() * 1000))
}

// NextReportSeq atomically generates the next monotonic sequence number.
func NextReportSeq() uint64 {
	return reportSeq.Add(1)
}

// IsAbsolutePosixOrWin32 checks whether a path is absolute in posix or win32 format,
// precisely matching TS: path.posix.isAbsolute(file) || path.win32.isAbsolute(file).
func IsAbsolutePosixOrWin32(p string) bool {
	if p == "" {
		return false
	}
	// POSIX absolute: starts with '/'
	if p[0] == '/' {
		return true
	}
	// Win32 UNC path: starts with \\ or //
	if len(p) >= 2 && (p[0] == '\\' || p[0] == '/') && (p[1] == '\\' || p[1] == '/') {
		return true
	}
	// Win32 drive path: e.g. C:\ or C:/ or d:\
	if len(p) >= 3 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) &&
		p[1] == ':' && (p[2] == '/' || p[2] == '\\') {
		return true
	}
	return false
}

// ResolveSocketEndpoint resolves the socket endpoint string.
// Supported platforms: Linux/macOS (Unix domain socket).
// Windows named pipes (\\.\pipe\...) are not supported because the Go standard net package
// does not natively support Windows named pipes without external non-standard dependencies.
func ResolveSocketEndpoint(socketPath string) string {
	return socketPath
}

// GenerateRequestID generates a request ID matching TS:
// id: `${source}:${Date.now()}:${Math.random().toString(36).slice(2)}`
// or with session subprefix.
func GenerateRequestID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s:%d:%d", prefix, time.Now().UnixMilli(), time.Now().UnixNano()%1000000)
	}
	return fmt.Sprintf("%s:%d:%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(b))
}

// Request represents a JSON-RPC notification/request payload sent to herdr.
type Request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// SessionRef carries the active agent session references.
type SessionRef struct {
	Path *string
	ID   *string
}

// ReportAgentParams represents the params for "pane.report_agent".
type ReportAgentParams struct {
	PaneID           string  `json:"pane_id"`
	Source           string  `json:"source"`
	Agent            string  `json:"agent"`
	State            string  `json:"state"`
	Message          *string `json:"message,omitempty"`
	Seq              uint64  `json:"seq"`
	AgentSessionPath *string `json:"agent_session_path,omitempty"`
	AgentSessionID   *string `json:"agent_session_id,omitempty"`
	// ResumeArgv is the command Herdr runs in this pane to resume the session
	// after a server restart. Its first element must be a plain command name.
	ResumeArgv []string `json:"resume_argv,omitempty"`
}

// ReportAgentSessionParams represents the params for "pane.report_agent_session".
type ReportAgentSessionParams struct {
	PaneID             string  `json:"pane_id"`
	Source             string  `json:"source"`
	Agent              string  `json:"agent"`
	Seq                uint64  `json:"seq"`
	SessionStartSource *string `json:"session_start_source,omitempty"`
	AgentSessionPath   *string `json:"agent_session_path,omitempty"`
	AgentSessionID     *string `json:"agent_session_id,omitempty"`
	// ResumeArgv is the command Herdr runs in this pane to resume the session
	// after a server restart. Its first element must be a plain command name.
	ResumeArgv []string `json:"resume_argv,omitempty"`
}

// ReleaseAgentParams represents the params for "pane.release_agent".
// Required by the schema: pane_id / source / agent; seq is optional but recommended.
type ReleaseAgentParams struct {
	PaneID string `json:"pane_id"`
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Seq    uint64 `json:"seq"`
}

// response is the JSON envelope Herdr answers every request with.
// It covers both the success_response ({"id","result"}) and the
// error_response ({"id","error":{"code","message"}}) schemas.
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// DialerFunc abstracts the network dialer for test injection.
type DialerFunc func(network, address string, timeout time.Duration) (net.Conn, error)

// Client handles socket transmission to Herdr with retry logic.
type Client struct {
	endpoint string
	paneID   string
	source   string
	agent    string
	dialer   DialerFunc
	timeouts []time.Duration
}

// NewClient creates a new Client configured for socket communication.
func NewClient(endpoint, paneID string, dialer DialerFunc, timeouts []time.Duration) *Client {
	if len(timeouts) == 0 {
		timeouts = []time.Duration{DefaultInitialTimeout, DefaultRetryTimeout}
	}
	return &Client{
		endpoint: endpoint,
		paneID:   paneID,
		source:   SourcePi,
		agent:    AgentPi,
		dialer:   dialer,
		timeouts: timeouts,
	}
}

// ResumeCommand builds the resume vector Herdr runs in this pane after a server
// restart. It follows the official resume rules: the first element is a plain
// command name resolvable on PATH (never an absolute path), and the session is
// addressed through pig's own --session <path|id> option.
// It returns nil when the session reference carries neither a path nor an id.
func ResumeCommand(sessionRef SessionRef) []string {
	target := ""
	if sessionRef.Path != nil && *sessionRef.Path != "" {
		target = *sessionRef.Path
	} else if sessionRef.ID != nil && *sessionRef.ID != "" {
		target = *sessionRef.ID
	}
	if target == "" {
		return nil
	}
	return []string{"pig", "--session", target}
}

// SendRequestAttempt sends a single JSON request to the socket endpoint with the given timeout.
// Matches TS lines 22-48 (sendRequestAttempt):
// Connects to socket, writes JSON + "\n", waits for the server's response, and closes socket.
// Returns true only when Herdr answers with a success envelope whose id matches the request;
// a JSON error envelope, an id mismatch, or no answer at all returns false.
// Enforces a strict total attempt deadline <= timeout (dial + write + read).
func (c *Client) SendRequestAttempt(request any, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	start := time.Now()
	deadline := start.Add(timeout)

	remainDial := time.Until(deadline)
	if remainDial <= 0 {
		return false
	}

	var conn net.Conn
	var err error

	if c.dialer != nil {
		conn, err = c.dialer("unix", c.endpoint, remainDial)
	} else {
		conn, err = net.DialTimeout("unix", c.endpoint, remainDial)
	}
	if err != nil {
		return false
	}
	defer conn.Close()

	// Enforce the attempt deadline covering both write and read
	if err := conn.SetDeadline(deadline); err != nil {
		return false
	}

	data, err := json.Marshal(request)
	if err != nil {
		return false
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		return false
	}

	// Read the response frame and validate it against the official response schema.
	// The frame is read completely (up to the newline that terminates it), so a
	// reply that reaches the client in several segments is not mistaken for a
	// broken answer; the TS port only waited for any bytes (socket.on("data")),
	// which silently accepted error envelopes.
	frame, err := readResponseFrame(conn)
	if err != nil {
		return false
	}
	return validateResponse(frame, request)
}

// readResponseFrame reads exactly one Herdr response frame from the
// connection.
//
// Herdr frames its responses with a newline, so the frame is read until that
// newline arrives: a reply split across several TCP segments is read
// completely instead of being validated half-way. A payload that is complete
// but not newline terminated is accepted when the peer closes the connection
// (EOF). An empty frame and a frame longer than maxResponseFrameBytes are
// rejected, so neither a truncated nor an oversized reply can be mistaken for
// a successful delivery.
func readResponseFrame(conn net.Conn) ([]byte, error) {
	reader := bufio.NewReaderSize(conn, 4096)
	frame := make([]byte, 0, 512)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(frame) == 0 {
					return nil, errEmptyResponseFrame
				}
				// The peer closed the connection after a complete payload
				// without a trailing newline: the frame is the payload.
				return frame, nil
			}
			return nil, err
		}
		if b == '\n' {
			if len(frame) == 0 {
				return nil, errEmptyResponseFrame
			}
			return frame, nil
		}
		frame = append(frame, b)
		if len(frame) > maxResponseFrameBytes {
			return nil, errResponseFrameTooLong
		}
	}
}

// validateResponse parses one Herdr response envelope and checks it against the request.
// It accepts the success_response ({"id","result"}) shape and rejects the
// error_response ({"id","error":{"code","message"}}) shape, an id mismatch,
// and an empty result. A non-JSON payload is treated as undeliverable rather than
// as success: the TS port's byte-count heuristic hid Herdr rejections.
func validateResponse(payload []byte, request any) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}

	var resp response
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return false
	}
	if resp.Error != nil {
		return false
	}
	if len(resp.Result) == 0 {
		return false
	}

	req, ok := request.(Request)
	if !ok {
		// Non-Request payloads (tests injecting raw structs) have no id to match.
		return true
	}
	return resp.ID == req.ID
}

// SendRequest attempts to deliver the request, first with timeouts[0] (500ms),
// then retrying with timeouts[1] (1500ms) on failure.
// Matches TS lines 50-55 (sendRequest).
// Returns true when at least one attempt passed validateResponse, i.e. Herdr
// answered with a matching success envelope; false when every attempt failed.
func (c *Client) SendRequest(request any) bool {
	if len(c.timeouts) > 0 && c.SendRequestAttempt(request, c.timeouts[0]) {
		return true
	}
	if len(c.timeouts) > 1 {
		return c.SendRequestAttempt(request, c.timeouts[1])
	}
	return false
}

// BuildReportAgentSessionRequest constructs a pane.report_agent_session Request synchronously,
// allocating the monotonic sequence number immediately to preserve relative ordering with pane.report_agent.
// Returns (request, true) if a session reference exists, or (Request{}, false) if neither path nor id is present.
// resumeArgv is attached only when the caller may attach it: Herdr answers
// "resume_not_accepted" until the source holds the pane by reporting agent state.
func (c *Client) BuildReportAgentSessionRequest(sessionRef SessionRef, sessionStartSource *string, resumeArgv []string) (Request, bool) {
	// If no session reference is available, TS returns Promise.resolve() without sending
	if sessionRef.Path == nil && sessionRef.ID == nil {
		return Request{}, false
	}
	return c.BuildReportAgentSessionRequestWithSeq(sessionRef, sessionStartSource, resumeArgv, NextReportSeq())
}

// BuildReportAgentSessionRequestWithSeq constructs a pane.report_agent_session
// Request with an explicitly allocated sequence number.
//
// The caller allocates the sequence number under its own lock, in the same
// critical section in which the request is handed to the sending lifecycle.
// That keeps the order of the requests on the wire identical to the order of
// their sequence numbers, even when several reports are produced concurrently.
// Returns (request, true) if a session reference exists, or (Request{}, false)
// if neither path nor id is present.
func (c *Client) BuildReportAgentSessionRequestWithSeq(sessionRef SessionRef, sessionStartSource *string, resumeArgv []string, seq uint64) (Request, bool) {
	// If no session reference is available, TS returns Promise.resolve() without sending
	if sessionRef.Path == nil && sessionRef.ID == nil {
		return Request{}, false
	}

	params := ReportAgentSessionParams{
		PaneID:             c.paneID,
		Source:             c.source,
		Agent:              c.agent,
		Seq:                seq,
		SessionStartSource: sessionStartSource,
		ResumeArgv:         resumeArgv,
	}

	// Priority: agent_session_path first, then agent_session_id
	if sessionRef.Path != nil {
		params.AgentSessionPath = sessionRef.Path
	} else if sessionRef.ID != nil {
		params.AgentSessionID = sessionRef.ID
	}

	return Request{
		ID:     GenerateRequestID(c.source + ":session"),
		Method: "pane.report_agent_session",
		Params: params,
	}, true
}

// SendReportAgentSession sends pane.report_agent_session matching TS lines 114-132.
func (c *Client) SendReportAgentSession(sessionRef SessionRef, sessionStartSource *string, resumeArgv []string) {
	if req, ok := c.BuildReportAgentSessionRequest(sessionRef, sessionStartSource, resumeArgv); ok {
		c.SendRequest(req)
	}
}

// BuildReleaseAgentRequest constructs a pane.release_agent Request.
// The schema requires pane_id / source / agent; seq keeps the release ordered
// after the last state report from the same source.
func (c *Client) BuildReleaseAgentRequest() Request {
	return c.BuildReleaseAgentRequestWithSeq(NextReportSeq())
}

// BuildReleaseAgentRequestWithSeq constructs a pane.release_agent Request with
// an explicitly allocated sequence number. The caller allocates the sequence
// number under its own lock, so the release keeps its place in the order of
// the reports that were sent before it.
func (c *Client) BuildReleaseAgentRequestWithSeq(seq uint64) Request {
	return Request{
		ID:     GenerateRequestID(c.source + ":release"),
		Method: "pane.release_agent",
		Params: ReleaseAgentParams{
			PaneID: c.paneID,
			Source: c.source,
			Agent:  c.agent,
			Seq:    seq,
		},
	}
}

// SendReleaseAgent sends pane.release_agent so Herdr clears this pane's agent
// name, state and resume command as soon as the agent exits.
func (c *Client) SendReleaseAgent() {
	c.SendReleaseAgentWithSeq(NextReportSeq())
}

// SendReleaseAgentWithSeq sends pane.release_agent with an explicitly
// allocated sequence number.
func (c *Client) SendReleaseAgentWithSeq(seq uint64) {
	c.SendRequest(c.BuildReleaseAgentRequestWithSeq(seq))
}

// SendReportAgent sends pane.report_agent matching TS lines 134-147.
// The state report is the one that holds the pane, so it always carries the
// resume command when a session reference is known.
// Returns true when at least one attempt passed validateResponse: only then has
// Herdr accepted the state report and this source may be considered to hold
// the pane (see state.go drainLoop / agentStateReported).
func (c *Client) SendReportAgent(sessionRef SessionRef, state string, message *string, seq uint64, resumeArgv []string) bool {
	params := ReportAgentParams{
		PaneID:     c.paneID,
		Source:     c.source,
		Agent:      c.agent,
		State:      state,
		Message:    message,
		Seq:        seq,
		ResumeArgv: resumeArgv,
	}

	// Apply session reference matching TS withSessionRef
	if sessionRef.Path != nil {
		params.AgentSessionPath = sessionRef.Path
	} else if sessionRef.ID != nil {
		params.AgentSessionID = sessionRef.ID
	}

	req := Request{
		ID:     GenerateRequestID(c.source),
		Method: "pane.report_agent",
		Params: params,
	}
	return c.SendRequest(req)
}
