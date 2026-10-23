package herdragentstate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

const (
	SourcePi = "herdr:pi"
	AgentPi  = "pi"
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

// SendRequestAttempt sends a single JSON request to the socket endpoint with the given timeout.
// Matches TS lines 22-48 (sendRequestAttempt):
// Connects to socket, writes JSON + "\n", waits for any data response, and closes socket.
// Returns true on successful response, false on error/close/timeout.
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

	// Wait for any data response from the server (TS: socket.on("data", () => finish(true)))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n > 0 {
		return true
	}
	if err != nil && err != io.EOF {
		return false
	}
	return false
}

// SendRequest attempts to deliver the request, first with timeouts[0] (500ms),
// then retrying with timeouts[1] (1500ms) on failure.
// Matches TS lines 50-55 (sendRequest).
func (c *Client) SendRequest(request any) {
	if len(c.timeouts) > 0 && c.SendRequestAttempt(request, c.timeouts[0]) {
		return
	}
	if len(c.timeouts) > 1 {
		c.SendRequestAttempt(request, c.timeouts[1])
	}
}

// BuildReportAgentSessionRequest constructs a pane.report_agent_session Request synchronously,
// allocating the monotonic sequence number immediately to preserve relative ordering with pane.report_agent.
// Returns (request, true) if a session reference exists, or (Request{}, false) if neither path nor id is present.
func (c *Client) BuildReportAgentSessionRequest(sessionRef SessionRef, sessionStartSource *string) (Request, bool) {
	// If no session reference is available, TS returns Promise.resolve() without sending
	if sessionRef.Path == nil && sessionRef.ID == nil {
		return Request{}, false
	}

	params := ReportAgentSessionParams{
		PaneID:             c.paneID,
		Source:             c.source,
		Agent:              c.agent,
		Seq:                NextReportSeq(),
		SessionStartSource: sessionStartSource,
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
func (c *Client) SendReportAgentSession(sessionRef SessionRef, sessionStartSource *string) {
	if req, ok := c.BuildReportAgentSessionRequest(sessionRef, sessionStartSource); ok {
		c.SendRequest(req)
	}
}

// SendReportAgent sends pane.report_agent matching TS lines 134-147.
func (c *Client) SendReportAgent(sessionRef SessionRef, state string, message *string, seq uint64) {
	params := ReportAgentParams{
		PaneID:  c.paneID,
		Source:  c.source,
		Agent:   c.agent,
		State:   state,
		Message: message,
		Seq:     seq,
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
	c.SendRequest(req)
}
