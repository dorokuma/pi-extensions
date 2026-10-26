package compactui

import (
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// TestCtrlICustomDoesNotBlockTerminalInput reproduces the pig Ctrl+I freeze:
// OnTerminalInput used to call ui.custom synchronously; Custom waits for Done
// while the host input pump waits for the terminal_input verdict, so no key
// (Esc/arrows/Enter) can reach the overlay. The open must return before Custom
// finishes, matching TS's un-awaited ctx.ui.custom(...).
func TestCtrlICustomDoesNotBlockTerminalInput(t *testing.T) {
	r, _ := selectorRuntime(t)
	ui := newFakeUI()
	var entered atomic.Bool
	release := make(chan struct{})
	ui.customFn = func(factory remoteComponentFactory, options any) (any, error) {
		entered.Store(true)
		<-release
		return nil, nil
	}
	r.mu.Lock()
	r.capturedTui = nil
	r.activeUIContext = ui
	r.mu.Unlock()

	done := make(chan terminalInputVerdict, 1)
	go func() {
		done <- r.handleTerminalInput("\x1b[105;5u")
	}()

	select {
	case verdict := <-done:
		if !verdict.consume {
			t.Fatal("Ctrl+I must be consumed")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handleTerminalInput blocked on ui.custom; host input ticket would deadlock")
	}

	// Custom must still be in flight (blocked on release) — proving we did not wait.
	deadline := time.Now().Add(500 * time.Millisecond)
	for !entered.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !entered.Load() {
		t.Fatal("ui.custom was never started")
	}
	r.mu.Lock()
	root := r.activeCustomRoot
	r.mu.Unlock()
	if root == nil {
		t.Fatal("activeCustomRoot must be set before Custom returns")
	}

	// Esc while Custom is still open must close via activeCustomRoot path.
	verdict := r.handleTerminalInput("\x1b")
	if !verdict.consume {
		t.Fatal("Esc must be consumed while the Custom root is open")
	}

	close(release)
	ui.waitCustomCalls(t, 1)
}

// TestHiddenGroupedCardsReturnNoLines asserts folded non-display cards return
// an empty slice (len 0). Host ShellSelf renderSelfShell returns no rows when
// content is empty; Default shell always keeps Spacer(1), so a stale Default
// frame per hidden card is what gap.png's ~29 blank lines look like.
func TestHiddenGroupedCardsReturnNoLines(t *testing.T) {
	r, _ := modalRuntime(t)
	group := r.newToolGroup(0)
	var tools []*toolComponent
	for i := 0; i < 5; i++ {
		id := "call-" + itoa(i)
		tool := newTestTool(id, "read", map[string]any{"path": "/tmp/" + id}).succeeded("ok")
		group.addTool(tool)
		tools = append(tools, tool)
	}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.lastActiveGroup = group
	r.mu.Unlock()

	var blank int
	for i, tool := range tools {
		lines := r.toolRenderLines("read", tool.args, sdk.ToolRenderContext{
			ToolCallID:   tool.toolCallID,
			ArgsComplete: true,
		}, strPtr("ok"), false, 80, false)
		if i < len(tools)-1 {
			if len(lines) != 0 {
				t.Fatalf("hidden card %d lines = %v, want empty (no host rows)", i, lines)
			}
			blank++
		} else if len(lines) == 0 {
			t.Fatal("display card rendered no lines")
		} else if lines[0] == "" {
			// ShellSelf prepends its own blank; the group must not also lead with one.
			t.Fatalf("display tree starts with a blank line (would stack with ShellSelf): %q", lines[:min(3, len(lines))])
		}
	}
	if blank != 4 {
		t.Fatalf("hidden cards = %d, want 4", blank)
	}
}
