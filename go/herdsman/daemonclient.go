package herdsman

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// defaultReconnectDelaysMS mirrors DEFAULT_RECONNECT_DELAYS_MS.
var defaultReconnectDelaysMS = []int64{100, 250, 500, 1000}

// requestTimeoutMS mirrors REQUEST_TIMEOUT_MS.
const requestTimeoutMS = 15_000

// Client errors mirror the TS Error messages verbatim, because callers and
// tests match on the text.
var (
	ErrClientClosed       = errors.New("Herdsman daemon client is closed")
	ErrClientNotConnected = errors.New("Herdsman daemon client is not connected")
	ErrRequestTimeout     = errors.New("Herdsman daemon request timed out")
	ErrSocketClosed       = errors.New("Herdsman daemon socket closed")
	ErrIncompatible       = errors.New("Herdsman daemon version is incompatible; restart the session")
)

// HerdsmanDaemonClient mirrors the TS interface the extension programs against.
// It is an interface so tests can inject a fake daemon without a socket.
type HerdsmanDaemonClient interface {
	Close()
	ResetForSession()
	Request(method string, params any) (any, error)
	OnConnected() func() error
	SetOnConnected(handler func() error)
	OnDisconnected() func(error)
	SetOnDisconnected(handler func(error))
	OnStreamMessage() func(DaemonStreamMessage)
	SetOnStreamMessage(handler func(DaemonStreamMessage))
}

// clientState mirrors the TS `ClientState` union.
type clientState int

const (
	stateIdle clientState = iota
	stateConnecting
	stateConnected
	stateClosed
)

// pendingRequest mirrors the TS `PendingRequest`.
type pendingRequest struct {
	method  string
	reject  func(error)
	resolve func(any)
	timer   *time.Timer
}

// ReconnectingDaemonClient mirrors the TS class of the same name: a
// JSON-lines RPC client over a Unix socket that reconnects with a bounded
// backoff ladder and treats two optional methods as "old daemon" instead of
// "incompatible".
type ReconnectingDaemonClient struct {
	mu sync.Mutex

	socketPath       string
	reconnectDelays  []int64
	decoder          *JsonLineDecoder
	generation       int
	incompatible     bool
	nextID           int
	reconnectAttempt int
	reconnectTimer   *time.Timer
	socket           net.Conn
	state            clientState
	pending          map[string]*pendingRequest

	onConnected     func() error
	onDisconnected  func(error)
	onStreamMessage func(DaemonStreamMessage)

	// connectedNotifiedGen is the generation whose connection has already been
	// reported to `onConnected`. A client starts dialling in its constructor, so
	// the very first connection can be established before the caller installs its
	// callback; without this marker that session would never learn it is
	// connected (and never register its presence).
	connectedNotifiedGen int

	// streams carries daemon notifications from the read loop to the
	// dispatcher. The TS class emits them from the socket's data handler, where
	// an `await` inside a handler yields to the event loop instead of blocking
	// it. A Go handler that issues a request would block the very goroutine
	// that has to read the response, so delivery moves to its own goroutine and
	// the read loop only reads.
	streams *streamQueue

	// writeLine is the socket write, replaceable in tests.
	writeLine func(line string) error
	// now is the clock used by the request timeouts, replaceable in tests.
	now func() time.Time
}

// NewReconnectingDaemonClient mirrors `new ReconnectingDaemonClient(options)`:
// the connection attempt is scheduled on a microtask, i.e. right after the
// constructor returns.
//
// The client starts out idle: the queued attempt reserves the connecting state
// when it actually runs, so exactly one attempt ever owns the client. A
// ResetForSession issued right after the constructor either finds that attempt
// already reserved (and leaves it alone) or reserves its own — and the queued
// one then finds the client no longer idle and does nothing.
func NewReconnectingDaemonClient(socketPath string, reconnectDelaysMS []int64) *ReconnectingDaemonClient {
	delays := reconnectDelaysMS
	if len(delays) == 0 {
		delays = defaultReconnectDelaysMS
	}
	decoder, _ := NewJsonLineDecoder(DefaultMaxFrameBytes)
	client := &ReconnectingDaemonClient{
		socketPath:      socketPath,
		reconnectDelays: delays,
		decoder:         decoder,
		nextID:          1,
		state:           stateIdle,
		pending:         map[string]*pendingRequest{},
		now:             time.Now,
	}
	// queueMicrotask(() => this.#connect())
	go func() { client.connect() }()
	return client
}

// OnConnected/SetOnConnected mirror the TS public mutable fields.
func (c *ReconnectingDaemonClient) OnConnected() func() error { return c.onConnected }
func (c *ReconnectingDaemonClient) SetOnConnected(handler func() error) {
	c.mu.Lock()
	c.onConnected = handler
	fire := c.fireConnectedLocked()
	c.mu.Unlock()
	if fire != nil {
		// The connection predates the callback. Delivering it here (off the lock,
		// like every other callback) is what keeps the first connection of a
		// freshly built client from being missed.
		go func() { _ = fire() }()
	}
}

// fireConnectedLocked decides whether an already-established connection still
// owes its callback a notification. The caller holds c.mu and must run the
// returned handler after unlocking.
func (c *ReconnectingDaemonClient) fireConnectedLocked() func() error {
	handler := c.onConnected
	if handler == nil || c.state != stateConnected || c.connectedNotifiedGen == c.generation {
		return nil
	}
	c.connectedNotifiedGen = c.generation
	return handler
}

func (c *ReconnectingDaemonClient) OnDisconnected() func(error) { return c.onDisconnected }
func (c *ReconnectingDaemonClient) SetOnDisconnected(handler func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDisconnected = handler
}

func (c *ReconnectingDaemonClient) OnStreamMessage() func(DaemonStreamMessage) {
	return c.onStreamMessage
}
func (c *ReconnectingDaemonClient) SetOnStreamMessage(handler func(DaemonStreamMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onStreamMessage = handler
}

// ResetForSession mirrors the TS `resetForSession`.
func (c *ReconnectingDaemonClient) ResetForSession() {
	c.mu.Lock()
	if c.state == stateClosed {
		c.mu.Unlock()
		return
	}
	c.incompatible = false
	c.reconnectAttempt = 0
	if c.reconnectTimer != nil {
		// A backoff scheduled for the previous session must not dial into the new
		// one next to this session's own attempt.
		c.reconnectTimer.Stop()
		c.reconnectTimer = nil
	}
	fire := c.fireConnectedLocked()
	attemptInFlight := c.state == stateConnecting || c.state == stateConnected
	// The reservation is made in the same critical section that decided the
	// client is idle: between an unlocked check and an unlocked dial a reconnect
	// timer (or the constructor's queued attempt) could grab the lock and start
	// its own dial, and the two would then race for the socket. Whoever loses
	// finds the client no longer idle.
	var generation int
	var socketPath string
	var superseded net.Conn
	shouldDial := false
	if !attemptInFlight {
		generation, socketPath, superseded, shouldDial = c.beginConnectLocked()
	}
	c.mu.Unlock()
	if fire != nil {
		go func() { _ = fire() }()
	}
	if attemptInFlight {
		// An attempt already owns this session's connection. The TS client
		// reaches the same place through microtask ordering: the `resetForSession`
		// dial connects, and the queued constructor dial is the one that gets
		// superseded, so exactly one socket ever fires onConnected. Dialling again
		// here instead would supersede the running attempt and orphan whatever it
		// already has in flight -- and the orphaned request then waits for its
		// full timeout, which is how the extension appeared to hang.
		return
	}
	if !shouldDial {
		return
	}
	c.dialGeneration(generation, socketPath, superseded)
}

// Close mirrors the TS `close`.
func (c *ReconnectingDaemonClient) Close() {
	c.mu.Lock()
	if c.state == stateClosed {
		c.mu.Unlock()
		return
	}
	c.state = stateClosed
	c.generation += 1
	if c.reconnectTimer != nil {
		c.reconnectTimer.Stop()
	}
	c.reconnectTimer = nil
	socket := c.socket
	c.socket = nil
	c.mu.Unlock()
	if socket != nil {
		_ = socket.Close()
	}
	c.rejectAll(ErrClientClosed)
}

// Request mirrors the TS `request`. It blocks the caller like awaiting the
// returned promise would.
func (c *ReconnectingDaemonClient) Request(method string, params any) (any, error) {
	c.mu.Lock()
	switch {
	case c.state == stateClosed:
		c.mu.Unlock()
		return nil, ErrClientClosed
	case c.state != stateConnected || c.socket == nil:
		c.mu.Unlock()
		return nil, ErrClientNotConnected
	}
	id := "pi-" + formatInt(int64(c.nextID))
	c.nextID += 1
	socket := c.socket
	write := c.writeLine
	c.mu.Unlock()

	line, err := EncodeJSONLine(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}

	type outcome struct {
		value any
		err   error
	}
	// Buffered so a late completion after the timeout cannot leak a goroutine.
	result := make(chan outcome, 1)

	c.mu.Lock()
	timer := time.AfterFunc(requestTimeoutMS*time.Millisecond, func() {
		c.mu.Lock()
		_, tracked := c.pending[id]
		if tracked {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		// A response arriving later is ignored because the id is no longer
		// tracked: this keeps acks from hanging forever when the socket stays
		// open but the daemon is unresponsive.
		if tracked {
			result <- outcome{err: ErrRequestTimeout}
		}
	})
	c.pending[id] = &pendingRequest{
		method: method,
		timer:  timer,
		resolve: func(value any) {
			result <- outcome{value: value}
		},
		reject: func(rejectErr error) {
			result <- outcome{err: rejectErr}
		},
	}
	c.mu.Unlock()

	if write != nil {
		err = write(line)
	} else {
		err = writeAll(socket, line)
	}
	if err != nil {
		c.mu.Lock()
		entry, tracked := c.pending[id]
		if tracked {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if tracked {
			entry.timer.Stop()
		}
		return nil, err
	}

	out := <-result
	return out.value, out.err
}

// writeAll writes a full frame to the socket.
func writeAll(socket net.Conn, line string) error {
	payload := []byte(line)
	for len(payload) > 0 {
		written, err := socket.Write(payload)
		if err != nil {
			return err
		}
		payload = payload[written:]
	}
	return nil
}

// beginConnectLocked is the locked half of connect: it atomically validates
// that a new attempt may start and reserves the state (stateConnecting, a fresh
// generation, a fresh decoder) for it. The caller holds c.mu and must dial with
// the returned identity once it released the lock.
//
// The reservation is what keeps two attempts from ever overlapping: a timer
// callback that validated itself and a ResetForSession that arrived in the
// meantime cannot both reserve, so the loser exits without touching the socket
// the winner just connected.
func (c *ReconnectingDaemonClient) beginConnectLocked() (int, string, net.Conn, bool) {
	if c.state == stateClosed || c.incompatible {
		return 0, "", nil, false
	}
	if c.state != stateIdle {
		// An attempt is already reserved or already connected: this client is not
		// idle, so a late caller (a constructor's queued dial, a reconnect timer
		// whose lock window a ResetForSession raced through) must not supersede the
		// live connection.
		return 0, "", nil, false
	}
	c.state = stateConnecting
	c.generation += 1
	generation := c.generation
	if c.reconnectTimer != nil {
		// This attempt supersedes the scheduled one; leaving the timer armed
		// would dial a second socket behind this one's back.
		c.reconnectTimer.Stop()
		c.reconnectTimer = nil
	}
	decoder, _ := NewJsonLineDecoder(DefaultMaxFrameBytes)
	c.decoder = decoder
	socketPath := c.socketPath
	superseded := c.socket
	c.socket = nil
	return generation, socketPath, superseded, true
}

// dialGeneration is the unlocked half of connect: everything that blocks on the
// daemon, on the reserved identity of one attempt.
func (c *ReconnectingDaemonClient) dialGeneration(generation int, socketPath string, superseded net.Conn) {
	if superseded != nil {
		// A previous attempt's socket would otherwise stay open (its reader
		// stops on the generation bump) while its in-flight requests waited for
		// the timeout. Close it and fail them now.
		_ = superseded.Close()
		c.rejectAll(ErrClientReconnecting)
	}

	socket, err := net.Dial("unix", socketPath)
	if err != nil {
		// The TS createConnection reports the failure through the socket's
		// "error" event, which reaches the same disconnect path.
		c.handleDisconnect(err, generation)
		return
	}

	c.mu.Lock()
	if generation != c.generation || c.state == stateClosed {
		c.mu.Unlock()
		_ = socket.Close()
		return
	}
	c.socket = socket
	c.state = stateConnected
	c.reconnectAttempt = 0
	if c.reconnectTimer != nil {
		c.reconnectTimer.Stop()
		c.reconnectTimer = nil
	}
	// The reader and the dispatcher start BEFORE `onConnected`: a callback that
	// asks the daemon something has to be able to read the answer, and its
	// request would otherwise wait for the very loop that has to deliver it
	// (which only starts after the callback returns).
	streams := newStreamQueue()
	c.streams = streams
	handler := c.onConnected
	notify := handler != nil && c.connectedNotifiedGen != generation
	if notify {
		c.connectedNotifiedGen = generation
	}
	c.mu.Unlock()

	go c.streamLoop(streams, generation)
	go c.readLoop(socket, generation, streams)

	if notify {
		if handlerErr := handler(); handlerErr != nil {
			_ = socket.Close()
		}
	}
}

// connect mirrors the TS `#connect`.
func (c *ReconnectingDaemonClient) connect() {
	c.mu.Lock()
	generation, socketPath, superseded, ok := c.beginConnectLocked()
	c.mu.Unlock()
	if !ok {
		return
	}
	c.dialGeneration(generation, socketPath, superseded)
}

// streamQueue is an unbounded FIFO of daemon notifications. It is unbounded on
// purpose: the read loop must never block (a blocked reader would deadlock a
// handler waiting for a response) and must never drop a notification either,
// because event ids and role changes carry the delivery watermark.
type streamQueue struct {
	mu     sync.Mutex
	items  []DaemonStreamMessage
	wake   chan struct{}
	closed bool
}

func newStreamQueue() *streamQueue {
	return &streamQueue{wake: make(chan struct{}, 1)}
}

func (q *streamQueue) push(message DaemonStreamMessage) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, message)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// close ends the queue: a pop after it drains what is left and then reports
// done.
func (q *streamQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *streamQueue) pop() (DaemonStreamMessage, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			message := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return message, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return DaemonStreamMessage{}, false
		}
		<-q.wake
	}
}

// streamLoop delivers notifications one at a time, in arrival order, off the
// read loop. It ends when the read loop closes the queue (connection closed,
// superseded by a newer generation, or the client was closed).
func (c *ReconnectingDaemonClient) streamLoop(streams *streamQueue, generation int) {
	for {
		message, ok := streams.pop()
		if !ok {
			return
		}
		c.mu.Lock()
		stale := generation != c.generation || c.state == stateClosed
		handler := c.onStreamMessage
		c.mu.Unlock()
		if stale || handler == nil {
			continue
		}
		handler(message)
	}
}

// readLoop mirrors the socket's "data"/"error"/"close" handlers.
func (c *ReconnectingDaemonClient) readLoop(socket net.Conn, generation int, streams *streamQueue) {
	// The reader owns the queue: closing it here is what ends streamLoop.
	defer streams.close()
	buffer := make([]byte, 32*1024)
	for {
		read, err := socket.Read(buffer)
		if read > 0 {
			c.mu.Lock()
			stale := generation != c.generation || c.state == stateClosed
			c.mu.Unlock()
			if stale {
				return
			}
			if dataErr := c.handleData(string(buffer[:read]), streams); dataErr != nil {
				_ = socket.Close()
				c.handleDisconnect(dataErr, generation)
				return
			}
		}
		if err != nil {
			_ = socket.Close()
			c.handleDisconnect(ErrSocketClosed, generation)
			return
		}
	}
}

// handleDisconnect mirrors the TS `#handleDisconnect`.
func (c *ReconnectingDaemonClient) handleDisconnect(disconnectErr error, generation int) {
	c.mu.Lock()
	// The report comes from a reader that belongs to one connection: when a
	// newer attempt already took over, its failure describes the past and must
	// not reset the live state or plant a reconnect behind the new socket.
	if generation != c.generation || c.state == stateClosed || c.incompatible {
		c.mu.Unlock()
		return
	}
	c.state = stateIdle
	c.socket = nil
	onDisconnected := c.onDisconnected
	attempt := c.reconnectAttempt
	delays := c.reconnectDelays
	c.mu.Unlock()

	c.rejectAll(disconnectErr)
	if onDisconnected != nil {
		onDisconnected(disconnectErr)
	}

	c.mu.Lock()
	// The callbacks above run with the lock released: a session reset, a close
	// or a newer attempt in the meantime owns the reconnection now.
	if generation != c.generation || c.state == stateClosed || c.incompatible {
		c.mu.Unlock()
		return
	}
	index := attempt
	if index >= len(delays) {
		index = len(delays) - 1
	}
	delay := int64(1000)
	if index >= 0 && index < len(delays) {
		delay = delays[index]
	}
	c.reconnectAttempt = attempt + 1
	if c.reconnectTimer != nil {
		c.reconnectTimer.Stop()
	}
	// The callback re-checks everything under the lock before it acts: a
	// ResetForSession, a connect or a Close that happened while the backoff was
	// armed has advanced the generation (or planted its own timer), and this
	// stale callback must then exit without clearing the newer timer or dialling
	// a second socket behind the live one.
	var timer *time.Timer
	timer = time.AfterFunc(time.Duration(delay)*time.Millisecond, func() {
		c.mu.Lock()
		if generation != c.generation || c.state != stateIdle || c.incompatible || c.reconnectTimer != timer {
			c.mu.Unlock()
			return
		}
		c.reconnectTimer = nil
		// The validation and the reservation share one critical section: a
		// ResetForSession that ran while this callback waited for the lock has
		// already moved the state on, and beginConnectLocked then refuses
		// instead of superseding the connection it just established.
		newGeneration, socketPath, superseded, ok := c.beginConnectLocked()
		c.mu.Unlock()
		if !ok {
			return
		}
		c.dialGeneration(newGeneration, socketPath, superseded)
	})
	c.reconnectTimer = timer
	c.mu.Unlock()
}

// handleData mirrors the TS `#handleData`.
func (c *ReconnectingDaemonClient) handleData(chunk string, streams *streamQueue) error {
	c.mu.Lock()
	decoder := c.decoder
	c.mu.Unlock()
	messages, err := decoder.Push(chunk)
	if err != nil {
		return err
	}
	for _, message := range messages {
		c.handleMessage(message, streams)
	}
	return nil
}

// streamMethods are the three notification methods the daemon pushes.
var streamMethods = map[string]bool{
	"agent.event":                true,
	"agent.context.changed":      true,
	"agent.orchestrator.changed": true,
}

// handleMessage mirrors the TS `#handleMessage`.
func (c *ReconnectingDaemonClient) handleMessage(message any, streams *streamQueue) {
	record, ok := message.(map[string]any)
	if !ok {
		return
	}
	method, _ := record["method"].(string)
	if streamMethods[method] {
		// Handed to the dispatcher: the read loop must stay free to read the
		// responses a handler asks for.
		streams.push(decodeStreamMessage(record))
		return
	}

	rawID, hasID := record["id"]
	if !hasID || rawID == nil {
		return
	}
	id := idKey(rawID)
	if id == "" {
		return
	}

	c.mu.Lock()
	pending, tracked := c.pending[id]
	if !tracked {
		c.mu.Unlock()
		return
	}
	delete(c.pending, id)
	c.mu.Unlock()
	pending.timer.Stop()

	if rpcErr, hasError := record["error"].(map[string]any); hasError {
		code, _ := rpcErr["code"].(string)
		text, _ := rpcErr["message"].(string)
		if text == "" {
			text = "Herdsman RPC failed"
		}
		failure := &rpcFailure{Code: code, Message: text}
		pending.reject(failure)
		// Optional methods (turn completion signaling, keepalive ping) are
		// rejected like any other failure but must not mark the daemon
		// incompatible: an older daemon answers "Unknown method" and the
		// connection has to stay alive.
		if pending.method != "agent.turn.completed" && pending.method != "agent.ping" &&
			matchesIncompatibleMessage(text) {
			c.markIncompatible()
		}
		return
	}
	pending.resolve(record["result"])
}

// matchesIncompatibleMessage mirrors `/unknown|not found|unsupported|method/i`.
func matchesIncompatibleMessage(message string) bool {
	lowered := strings.ToLower(message)
	for _, needle := range []string{"unknown", "not found", "unsupported", "method"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// markIncompatible mirrors the TS incompatible branch.
func (c *ReconnectingDaemonClient) markIncompatible() {
	c.mu.Lock()
	c.incompatible = true
	c.state = stateIdle
	socket := c.socket
	c.socket = nil
	onDisconnected := c.onDisconnected
	c.mu.Unlock()
	if socket != nil {
		_ = socket.Close()
	}
	c.rejectAll(ErrIncompatible)
	if onDisconnected != nil {
		onDisconnected(ErrIncompatible)
	}
}

// rejectAll mirrors the TS `#rejectAll`.
func (c *ReconnectingDaemonClient) rejectAll(rejectErr error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]*pendingRequest{}
	c.mu.Unlock()
	for _, entry := range pending {
		entry.timer.Stop()
		entry.reject(rejectErr)
	}
}

// idKey renders a response id the way `String(message.id)` does: the daemon
// echoes the string id, but a numeric id must still find its slot.
func idKey(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return jsNumber(typed)
	case int:
		return formatInt(int64(typed))
	case int64:
		return formatInt(typed)
	case jsonNumber:
		return typed.String()
	case nil:
		return ""
	default:
		return jsValueString(value)
	}
}

// jsonNumber is the numeric interface the SDK's JSON decoder may use.
type jsonNumber interface{ String() string }

// rpcFailure mirrors the TS `Object.assign(new Error(...), { code })`: an error
// whose message is the daemon's message and which carries the daemon's code.
type rpcFailure struct {
	Code    string
	Message string
}

func (e *rpcFailure) Error() string { return e.Message }

// rpcErrorCode reads the code off an error the way the TS ack classifier does.
func rpcErrorCode(err error) string {
	var failure *rpcFailure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// asError mirrors the TS `asError`.
func asError(err error) error {
	return err
}

// parseJSONRecord decodes one JSON object, returning nil when the text is not
// an object.
func parseJSONRecord(text string) map[string]any {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil
	}
	record, _ := value.(map[string]any)
	return record
}

// A compile-time assertion that the concrete client satisfies the interface.
var _ HerdsmanDaemonClient = (*ReconnectingDaemonClient)(nil)

// ensure strconv stays referenced for numeric id variants the daemon may send.
var _ = strconv.Itoa
