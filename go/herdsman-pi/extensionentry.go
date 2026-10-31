package herdsmanpi

// SDK adapter: the thin translation layer between the ported core
// (extension*.go) and PiG's subprocess SDK.
//
// The TS module registers its handlers directly on `pi.*` (one object where the
// host methods and the event subscriptions live side by side). The Go SDK splits
// them: registrations and event subscriptions hang off `*sdk.Extension`, while
// the host calls that need a per-call context (`sendMessage`, `appendEntry`,
// `ui.notify`, `ui.setStatus`, `abort`, the session accessors) come with the
// `sdk.Context` of the request that delivered the event. The adapter keeps the
// mapping in one place:
//
//	pi.on("session_start")            -> sdk.EventSessionStart
//	pi.on("session_shutdown")         -> sdk.EventSessionShutdown
//	pi.on("message_end")              -> sdk.EventMessageEnd
//	pi.on("tool_execution_start")     -> sdk.EventToolExecutionStart
//	pi.on("tool_result")              -> sdk.EventToolResult
//	pi.on("agent_start")              -> sdk.EventAgentStart
//	pi.on("context")                  -> sdk.EventContext (+ sdk.EventContextWithSystem)
//	pi.on("agent_settled")            -> sdk.EventAgentSettled
//	pi.registerCommand("herdsman")    -> e.Command (with argument completions)
//	pi.registerMessageRenderer(...)   -> e.MessageRenderer
//	pi.sendMessage                    -> ctx.SendCustomMessage
//	pi.appendEntry                    -> ctx.AppendEntry
//	pi.setSessionName                 -> ctx.SetSessionName
//
// Two things the TS does not have to care about live here:
//
//   - The core keeps one PiAPI and one Context for its whole life, while the
//     SDK hands a per-request context to every callback. The adapter therefore
//     remembers the newest context (sdkPiAPI.setContext) and resolves `abort`
//     and `isIdle` through it, so a background path (the `role_lost` wake, the
//     deferred wake's idle probe) reaches the host instead of a context whose
//     request was answered long ago.
//   - The SDK may run two callbacks at the same time (an emit and a command, or
//     two emits), so the lazily created core sits behind sdkCore's own mutex.

import (
	"errors"
	"fmt"
	"os"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// errNoHostContext is what a host call answers before the first callback ever
// delivered a context: there is nothing to call yet.
var errNoHostContext = errors.New("herdsman-pi: no Pi host context is available yet")

// sdkPiAPI adapts the Pi host to the PiAPI the core calls.
type sdkPiAPI struct {
	mu        sync.Mutex
	latestCtx *sdk.Context
}

// setContext remembers the newest live host context. A nil context never
// clears a known one.
func (a *sdkPiAPI) setContext(ctx *sdk.Context) {
	if ctx == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.latestCtx = ctx
}

// getContext returns the newest host context, or nil while no callback ran yet.
func (a *sdkPiAPI) getContext() *sdk.Context {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestCtx
}

func (a *sdkPiAPI) AppendEntry(customType string, data any) error {
	ctx := a.getContext()
	if ctx == nil {
		return errNoHostContext
	}
	return ctx.AppendEntry(customType, data)
}

func (a *sdkPiAPI) SendMessage(message CustomMessage, options SendMessageOptions) error {
	ctx := a.getContext()
	if ctx == nil {
		return errNoHostContext
	}
	opts := sdk.SendMessageOptions{}
	if options.DeliverAs != nil {
		opts.DeliverAs = *options.DeliverAs
	}
	if options.TriggerTurn != nil {
		opts.TriggerTurn = options.TriggerTurn
	}
	payload := sdk.CustomMessage{
		CustomType: message.CustomType,
		Content:    message.Content,
		Display:    message.Display,
		Details:    message.Details,
	}
	return ctx.SendCustomMessage(payload, opts)
}

func (a *sdkPiAPI) SetSessionName(name string) error {
	ctx := a.getContext()
	if ctx == nil {
		return errNoHostContext
	}
	return ctx.SetSessionName(name)
}

// agentController is the agent-control slice of a host request the core needs:
// interrupt the run in progress and ask whether the agent is idle. sdk.Context
// provides both, so the adapter no longer has to answer either locally.
type agentController interface {
	Abort()
	IsIdle() (bool, error)
}

// _ pins the claim: these two host calls exist on the SDK context.
var _ agentController = sdk.Context{}

// sdkController is the production agentController: the calls go out on the
// resolved host context.
type sdkController struct{ ctx sdk.Context }

func (c sdkController) Abort() { c.ctx.Abort() }

func (c sdkController) IsIdle() (bool, error) { return c.ctx.IsIdle() }

// sdkContext adapts sdk.Context to the Context the core reads.
//
// `Abort` and `IsIdle` are resolved through the adapter's newest context: the
// core holds one Context (the one from `session_start`, which is also what a
// stream event hands its state machine) and that request-scoped value cannot
// drive a later run on its own.
type sdkContext struct {
	api *sdkPiAPI
	ctx *sdk.Context
	// agent overrides the agent-control calls. Production leaves it nil, which
	// means "resolve the live sdk.Context"; tests inject a host they can watch.
	agent agentController
}

// agentControl is the context an agent-control call goes out on, or false when
// no host context is known yet.
func (c sdkContext) agentControl() (agentController, bool) {
	if c.agent != nil {
		return c.agent, true
	}
	if latest := c.api.getContext(); latest != nil {
		return sdkController{ctx: *latest}, true
	}
	if c.ctx == nil {
		return nil, false
	}
	return sdkController{ctx: *c.ctx}, true
}

func (c sdkContext) Abort() {
	// The upstream `ctx.abort?.()` interrupts the run in progress; the call has
	// to survive the request it was created in, because an idle wake is aborted
	// from a stream event rather than from a handler.
	if agent, ok := c.agentControl(); ok {
		agent.Abort()
	}
}

func (c sdkContext) IsIdle() (bool, bool) {
	agent, ok := c.agentControl()
	if !ok {
		// Nothing to ask: the core treats "unknown" exactly like the TS
		// `ctx.isIdle === undefined` case (the deferral ladder and
		// `agent_settled` decide).
		return false, false
	}
	idle, err := agent.IsIdle()
	if err != nil {
		// A host or transport failure is not an answer either.
		return false, false
	}
	return idle, true
}

func (c sdkContext) SessionFile() string {
	if c.ctx == nil {
		return ""
	}
	file, err := c.ctx.GetSessionFile()
	if err != nil || file == nil {
		return ""
	}
	return *file
}

func (c sdkContext) SessionID() string {
	if c.ctx == nil {
		return ""
	}
	id, err := c.ctx.GetSessionID()
	if err != nil {
		return ""
	}
	return id
}

// HasUI maps ctx.hasUI: the SDK reports it in the ready/state snapshot, so it
// is always known once a context exists. The second return value keeps the TS
// "absent means unknown" contract for the fake contexts the tests drive.
func (c sdkContext) HasUI() (bool, bool) {
	if c.ctx == nil {
		return false, false
	}
	return c.ctx.HasUI(), true
}

// Mode maps ctx.mode: the SDK carries the host's run mode from the ready
// message. An empty string would be "the host never said", but the SDK pins
// the same contract the TS reads: a host that names no mode is a non-UI run,
// so Mode() reports "print" for it — exactly the session this extension must
// not dial a daemon for.
func (c sdkContext) Mode() (string, bool) {
	if c.ctx == nil {
		return "", false
	}
	return c.ctx.Mode(), true
}

func (c sdkContext) UI() ContextUI { return sdkContextUI{ctx: c.ctx} }

// sdkContextUI adapts the context's UI calls. A context the core holds can
// outlive its request, so the calls are guarded instead of assumed live.
type sdkContextUI struct{ ctx *sdk.Context }

func (u sdkContextUI) Notify(message string, level NotifyLevel) {
	if u.ctx == nil {
		return
	}
	u.ctx.Notify(message, string(level))
}

func (u sdkContextUI) SetStatus(key string, value *string) {
	if u.ctx == nil {
		return
	}
	if value == nil {
		u.ctx.ClearStatus(key)
		return
	}
	u.ctx.SetStatus(key, *value)
}

// sdkCore owns the lazily created extension core of one SDK extension.
//
// The core is created on the first callback that needs it, because its `pi`
// side talks to the host and a context only exists inside a request. It is
// created once per extension instance so its state (delivery queue, dead
// letters) survives between events, exactly like the TS closure. The mutex
// guards the lazy field: two callbacks can run concurrently, and a plain
// `if core == nil { core = ... }` hands one of them a half-built pointer and
// races the field itself. `core` serialises its own state with e.mu, and the
// two are never held at once here, so they cannot deadlock each other.
type sdkCore struct {
	mu      sync.Mutex
	api     *sdkPiAPI
	core    *HerdsmanExtension
	options ExtensionOptions
}

func newSDKCore(options ExtensionOptions) *sdkCore {
	return &sdkCore{api: &sdkPiAPI{}, options: options}
}

// pi is the adapter the core calls; it is created with the holder, so reading
// it needs no lock.
func (h *sdkCore) pi() *sdkPiAPI { return h.api }

// ensure returns the core, creating it on first use.
func (h *sdkCore) ensure() *HerdsmanExtension {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.core != nil {
		return h.core
	}
	options := h.options
	if options.ClientFactory == nil {
		options.ClientFactory = func() HerdsmanDaemonClient {
			return NewReconnectingDaemonClient(DefaultSocketPath(), nil)
		}
	}
	h.core = NewExtension(h.api, options)
	return h.core
}

// get returns the core, or nil while no callback has created it yet.
func (h *sdkCore) get() *HerdsmanExtension {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.core
}

// begin is what every callback does first: it records the live host context on
// the adapter and returns the wrapper the core reads. `create` is set by the
// handlers that own the core's creation (`session_start` and the command).
func (h *sdkCore) begin(ctx sdk.Context, create bool) (*HerdsmanExtension, sdkContext) {
	pointer := &ctx
	h.pi().setContext(pointer)
	hosted := sdkContext{api: h.pi(), ctx: pointer}
	if create {
		return h.ensure(), hosted
	}
	return h.get(), hosted
}

// NewSDKExtension builds the PiG extension: the SDK adapter plus every
// registration the TS factory performs. It is the package entry point the
// generated runner calls.
func NewSDKExtension(options ExtensionOptions) *sdk.Extension {
	e := sdk.New(sdkLogName)
	holder := newSDKCore(options)

	e.RegisterCommand("herdsman", sdk.CommandOptions{
		Description: "Watch Herdsman agent updates in this Pi",
		GetArgumentCompletions: func(prefix string) ([]sdk.AutocompleteItem, error) {
			items := CompleteCommand(prefix)
			if len(items) == 0 {
				return nil, nil
			}
			completions := make([]sdk.AutocompleteItem, 0, len(items))
			for _, item := range items {
				completions = append(completions, sdk.AutocompleteItem{Label: item["label"], Value: item["value"]})
			}
			return completions, nil
		},
		Handler: func(ctx sdk.Context, args string) error {
			instance, hosted := holder.begin(ctx, true)
			instance.handleCommand(args, hosted)
			return nil
		},
	})

	e.MessageRenderer("herdsman-wake", func(ctx sdk.Context, message map[string]any, renderOptions sdk.MessageRenderOptions, width int) ([]string, error) {
		return RenderAgentUpdateMessageLines(message["details"], RenderOptions{Expanded: renderOptions.Expanded}, sdkTheme{ctx: ctx}), nil
	})

	e.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, hosted := holder.begin(ctx, true)
		// Pi reports the run mode on the event as well as the context; the event
		// wins when it carries one, exactly like the TS `event.mode ?? ctx.mode`.
		var eventMode *string
		if mode, ok := data["mode"].(string); ok && mode != "" {
			eventMode = &mode
		}
		instance.HandleSessionStart(hosted, eventMode)
		return nil, nil
	})
	e.OnEvent(sdk.EventSessionShutdown, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, _ := holder.begin(ctx, false)
		if instance == nil {
			return nil, nil
		}
		instance.HandleSessionShutdown()
		return nil, nil
	})
	e.OnEvent(sdk.EventMessageEnd, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, _ := holder.begin(ctx, false)
		if instance == nil {
			return nil, nil
		}
		instance.HandleMessageEnd(data)
		return nil, nil
	})
	e.OnEvent(sdk.EventToolExecutionStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, _ := holder.begin(ctx, false)
		if instance != nil {
			instance.HandleSubstantiveToolUse()
		}
		return nil, nil
	})
	e.OnEvent(sdk.EventToolResult, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, _ := holder.begin(ctx, false)
		if instance != nil {
			instance.HandleSubstantiveToolUse()
		}
		return nil, nil
	})
	e.OnEvent(sdk.EventAgentStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, _ := holder.begin(ctx, false)
		if instance != nil {
			instance.HandleAgentStart()
		}
		return nil, nil
	})
	e.OnEvent(sdk.EventAgentSettled, func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, hosted := holder.begin(ctx, false)
		if instance == nil {
			return nil, nil
		}
		instance.HandleAgentSettled(hosted)
		return nil, nil
	})
	contextHandler := func(ctx sdk.Context, data map[string]any) (any, error) {
		instance, _ := holder.begin(ctx, false)
		if instance == nil {
			return nil, nil
		}
		messages, _ := data["messages"].([]any)
		kept := instance.HandleContext(messages)
		if len(kept) == len(messages) {
			return nil, nil
		}
		return map[string]any{"messages": kept}, nil
	}
	e.OnEvent(sdk.EventContext, contextHandler)
	e.OnEvent(sdk.EventContextWithSystem, contextHandler)

	return e
}

// sdkTheme maps the renderer's theme colours onto the host palette.
type sdkTheme struct{ ctx sdk.Context }

func (t sdkTheme) Bold(text string) string { return "\x1b[1m" + text + "\x1b[0m" }

func (t sdkTheme) FG(color, text string) string {
	switch color {
	case "success":
		return "\x1b[32m" + text + "\x1b[0m"
	case "warning":
		return "\x1b[33m" + text + "\x1b[0m"
	case "customMessageLabel":
		return "\x1b[36m" + text + "\x1b[0m"
	default:
		return "\x1b[90m" + text + "\x1b[0m"
	}
}

func (t sdkTheme) BG(color, text string) string {
	if text == "" {
		return ""
	}
	return "\x1b[48;5;236m" + text + "\x1b[0m"
}

// Extension is the pinned entry point every PiG extension in this repository
// exposes (PiG's Go loader requires exactly `func Extension() *sdk.Extension`).
// The ported state machine type is named HerdsmanExtension so the two cannot
// collide.
func Extension() *sdk.Extension {
	return NewSDKExtension(ExtensionOptions{})
}

// ensure the SDK identity and the process environment are wired.
var (
	_ = os.Getenv
	_ = fmt.Sprintf
)
