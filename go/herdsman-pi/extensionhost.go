package herdsmanpi

// Host surface and timer abstraction the extension core programs against.
//
// The TS extension receives an `ExtensionAPI` (`pi.*`) and a per-call `ctx` from
// the host. PiG's subprocess SDK has the same split, but neither half is a
// plain interface: `pi.sendMessage` is a host call, `ctx.ui.setStatus` is a host
// call, and the event payloads arrive as `map[string]any`. Keeping the ported
// state machine on a small interface is what lets the ported tests drive it
// without a live host — the SDK adapter (extension.go) implements it on top of
// sdk.Context, and the test harness implements it in memory.

import (
	"sync"
	"time"
)

// PiAPI is the extension-registration surface (the TS `PiApi`).
type PiAPI interface {
	// AppendEntry writes one session entry (TS `appendEntry?`).
	AppendEntry(customType string, data any) error
	// Register message renderer, command and tool registrations happen at
	// construction time through the SDK adapter; they are not part of the host
	// surface the state machine touches.
	SendMessage(message CustomMessage, options SendMessageOptions) error
	SetSessionName(name string) error
}

// CustomMessage mirrors the TS sendMessage message object.
type CustomMessage struct {
	Content    string
	CustomType string
	Details    any
	Display    bool
}

// SendMessageOptions mirrors the TS `{ deliverAs?, triggerTurn? }`; the nullable
// booleans keep "unset" and "explicitly false" apart, as the TS does.
type SendMessageOptions struct {
	DeliverAs   *string
	TriggerTurn *bool
}

// NotifyLevel mirrors the TS `"error" | "info" | "warning"` union.
type NotifyLevel string

const (
	NotifyError   NotifyLevel = "error"
	NotifyInfo    NotifyLevel = "info"
	NotifyWarning NotifyLevel = "warning"
)

// ContextUI is the `ctx.ui` surface.
type ContextUI interface {
	Notify(message string, level NotifyLevel)
	// SetStatus writes (or, with a nil value, clears) one status line.
	SetStatus(key string, value *string)
}

// Context is the TS `PiContext`: what the host hands each handler.
type Context interface {
	// Abort interrupts the run in progress; nil-able in the TS.
	Abort()
	// IsIdle reports whether the orchestrator is idle. The bool says whether the
	// host can answer at all (`isIdle?: () => boolean` in the TS).
	IsIdle() (idle bool, known bool)
	SessionFile() string
	SessionID() string
	// HasUI reports whether a dialog-capable UI is attached to this run. The
	// bool says whether the host reported one at all (`hasUI?: boolean` in the
	// TS): false,false means "unknown" and must keep the old behaviour.
	HasUI() (hasUI bool, known bool)
	// Mode reports the run mode (`"tui" | "rpc" | "json" | "print"`). The bool
	// says whether the host named one: "" means unknown, like an absent TS
	// field, so a context that never reports a mode stays interactive.
	Mode() (mode string, known bool)
	UI() ContextUI
}

// Timer is one scheduled callback.
type Timer interface {
	Stop() bool
}

// Timers schedules callbacks. Production uses a real clock; the ported tests
// install a manual clock so the ladders can be walked without sleeping.
type Timers interface {
	SetTimeout(delay time.Duration, callback func()) Timer
	SetInterval(interval time.Duration, callback func()) Timer
	Now() time.Time
}

// realTimers is the production clock.
type realTimers struct{}

func (realTimers) SetTimeout(delay time.Duration, callback func()) Timer {
	return time.AfterFunc(delay, callback)
}

func (realTimers) SetInterval(interval time.Duration, callback func()) Timer {
	timer := time.NewTicker(interval)
	stopped := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopped:
				return
			case <-timer.C:
				callback()
			}
		}
	}()
	return &intervalTimer{ticker: timer, stopped: stopped}
}

func (realTimers) Now() time.Time { return time.Now() }

type intervalTimer struct {
	once    sync.Once
	stopped chan struct{}
	ticker  *time.Ticker
}

func (t *intervalTimer) Stop() bool {
	stopped := false
	t.once.Do(func() {
		t.ticker.Stop()
		close(t.stopped)
		stopped = true
	})
	return stopped
}
