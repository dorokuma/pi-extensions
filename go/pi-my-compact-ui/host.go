package compactui

import (
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file is the seam between the ported logic and the PiG SDK. Everything
// here is host-facing and therefore subject to the two hard rules of the task:
//
//	1. Every host call is wrapped so a zero sdk.Context{} cannot panic. The SDK
//	   Context is a struct whose methods dereference c.ext; a zero value panics
//	   on the first host call, and the ported event handlers must survive it.
//	2. No host call runs while r.mu is held. Each wrapper snapshots what it
//	   needs under the lock, releases it, and only then calls the host.

// sdkUI is the uiContext implementation over a live sdk.Context.
type sdkUI struct {
	ctx sdk.Context
}

func (u sdkUI) setHiddenThinkingLabel(label string) {
	defer func() { _ = recover() }()
	_ = u.ctx.SetHiddenThinkingLabel(label)
}

func (u sdkUI) onTerminalInput(handler func(data string) terminalInputVerdict) func() {
	defer func() { _ = recover() }()
	unsubscribe, err := u.ctx.OnTerminalInput(func(data string) sdk.TerminalInputResult {
		verdict := handler(data)
		return sdk.TerminalInputResult{Consume: verdict.consume}
	})
	if err != nil || unsubscribe == nil {
		return func() {}
	}
	return unsubscribe
}

func (u sdkUI) setWidgetRenderer(key string, render func(width int) []string) {
	defer func() { _ = recover() }()
	_ = u.ctx.SetWidgetRenderer(key, render)
}

func (u sdkUI) notify(message, level string) {
	defer func() { _ = recover() }()
	u.ctx.Notify(message, level)
}

func (u sdkUI) hasUI() (has bool) {
	defer func() {
		if recover() != nil {
			has = false
		}
	}()
	return u.ctx.HasUI()
}

func (u sdkUI) custom(factory remoteComponentFactory, options any) (result any, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = nil
		}
	}()
	comp := factory(nil)
	if comp == nil {
		return nil, nil
	}
	sdkOptions := sdk.RemoteOverlayOptions{
		Overlay: true,
		OverlayOptions: &sdk.OverlayOptions{
			Anchor: "center",
		},
	}
	if opts, ok := options.(customOverlayOptions); ok {
		sdkOptions.OverlayOptions.Width = sdk.OverlayPercent(float64(opts.widthPercent))
		sdkOptions.OverlayOptions.MaxHeight = sdk.OverlayPercent(float64(opts.maxHeightPercent))
		if opts.anchor != "" {
			sdkOptions.OverlayOptions.Anchor = opts.anchor
		}
		if opts.onHandle != nil {
			sdkOptions.OnHandle = func(handle *sdk.OverlayHandle) {
				if handle != nil {
					opts.onHandle(sdkOverlayCloser{handle: handle})
				}
			}
		}
	}
	return u.ctx.Custom(adapterFor(comp), sdkOptions)
}

// sdkOverlayCloser adapts the SDK's overlay handle to overlayCloser.
type sdkOverlayCloser struct{ handle *sdk.OverlayHandle }

func (c sdkOverlayCloser) hide() {
	defer func() { _ = recover() }()
	if c.handle != nil {
		_ = c.handle.Hide()
	}
}

// sdkComponentAdapter renders a ported component through the SDK.
type sdkComponentAdapter struct {
	built remoteComponent

	// The SDK never invokes the port's factory with a real TUI (Custom takes
	// the component itself), so on the ctx.ui.Custom branch capturedTui stays
	// nil and runtime.requestFrame is a no-op. The SDK-side equivalent is
	// RemoteComponentInvalidator: while the overlay is mounted it installs its
	// own requestRender through SetInvalidate, and clears it with nil before
	// disposal. The adapter forwards that callback into the runtime's
	// requestFrame path so a modal's poll tick produces a real repaint on this
	// branch too.
	rt *runtimeState

	mu         sync.Mutex
	invalidate func()
}

// adapterFor wraps comp and binds it to the runtime it renders through, so a
// later SetInvalidate can register the frame requester on the right runtime.
func adapterFor(comp remoteComponent) *sdkComponentAdapter {
	adapter := &sdkComponentAdapter{built: comp}
	switch c := comp.(type) {
	case *customRootComponent:
		adapter.rt = c.tui
	case *inspectSelectorModal:
		adapter.rt = c.tui
	case *detailModalComponent:
		adapter.rt = c.tui
	}
	return adapter
}

// SetInvalidate implements sdk.RemoteComponentInvalidator. The SDK installs
// its overlay requestRender while the component is mounted (extension.go
// remoteOverlay.start) and clears it with nil on stop; implementations must
// replace the callback and treat nil as detach. The callback is registered on
// the runtime as the requestFrame fallback for the no-capturedTui branch, so
// the detail/selector poll loops repaint through the overlay's own invalidator
// instead of a full requestRender they cannot reach.
func (a *sdkComponentAdapter) SetInvalidate(fn func()) {
	a.mu.Lock()
	a.invalidate = fn
	a.mu.Unlock()
	rt := a.rt
	if rt == nil {
		return
	}
	rt.mu.Lock()
	if fn != nil {
		rt.customInvalidate = a.invalidateNow
		rt.customInvalidateOwner = a
	} else if rt.customInvalidateOwner == a {
		rt.customInvalidate = nil
		rt.customInvalidateOwner = nil
	}
	rt.mu.Unlock()
}

// invalidateNow is the callback the runtime's requestFrame reaches on the
// Custom branch: it nudges the SDK overlay's requestRender, which is what
// repaints the remote frame. It is a no-op before the overlay mounts and after
// it stops (the SDK clears the callback with SetInvalidate(nil)).
func (a *sdkComponentAdapter) invalidateNow() {
	a.mu.Lock()
	fn := a.invalidate
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Render implements sdk.RemoteComponent.
func (a *sdkComponentAdapter) Render(width int) []string {
	if a.built == nil {
		return nil
	}
	return a.built.Render(width)
}

// HandleInput implements sdk.RemoteComponent.
func (a *sdkComponentAdapter) HandleInput(data string) (sdk.RemoteComponentResult, error) {
	if a.built == nil {
		return sdk.RemoteComponentResult{}, nil
	}
	done, value := a.built.HandleInput(data)
	return sdk.RemoteComponentResult{Done: done, Value: value}, nil
}

// close releases the component.
func (a *sdkComponentAdapter) close() {
	if a.built == nil {
		return
	}
	if disposer, ok := a.built.(interface{ dispose() }); ok {
		disposer.dispose()
	}
}

// Dispose implements sdk.RemoteComponentDisposer.
func (a *sdkComponentAdapter) Dispose() { a.close() }

// safeCall runs a host-facing closure and swallows a panic caused by a zero or
// stale sdk.Context. It never runs under mu: every caller snapshots first.
func safeCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// uiContextFor returns the host UI surface for this call: the one captured at
// session_start when there is one (so a command handler reaches the same host
// the events do), otherwise an sdk.Context-backed one. A zero sdk.Context has
// no host at all, and then there is nothing to call -- nil rather than an
// adapter that would panic on first use.
func (r *runtimeState) uiContextFor(ctx sdk.Context) uiContext {
	r.mu.Lock()
	captured := r.activeUIContext
	r.mu.Unlock()
	if captured != nil {
		return captured
	}
	hasHost := func() (has bool) {
		defer func() {
			if recover() != nil {
				has = false
			}
		}()
		return ctx.HasUI()
	}()
	if !hasHost {
		return nil
	}
	return sdkUI{ctx: ctx}
}
