package compactui

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkToolGroupRenderWidth measures a multi-entry animating group render.
// The Working spinner stutters when this work runs on every host frame; the
// cache keyed by spinner frame index must keep same-frame re-entry cheap.
func BenchmarkToolGroupRenderWidth(b *testing.B) {
	r, _, _ := startedRuntime(b)
	r.setClock(func() int64 { return 1_000 })
	r.spinnerStartMs = 0
	group := r.newToolGroup(0)
	think := &thinkingEntry{kind: "thinking", id: 1, text: "planning the change", tokens: 12, active: true, owner: group}
	tools := make([]*toolComponent, 8)
	for i := range tools {
		tools[i] = newTestTool(fmt.Sprintf("call-%d", i), "read", map[string]any{"path": fmt.Sprintf("/tmp/f%d", i)}).pending()
		tools[i].owner = group
	}
	r.mu.Lock()
	group.entries = []groupEntry{{kind: "thinking", think: think}}
	for _, tool := range tools {
		group.entries = append(group.entries, groupEntry{kind: "tool", tool: tool})
		group.children = append(group.children, tool)
	}
	r.addGroupLocked(group)
	r.agentWorkPhase = strPtr(phaseWorking)
	r.mu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Advance the fake clock across spinner frames so the cache misses
		// on frame changes the way a live animation tick would.
		ms := int64(1_000 + (i%10)*spinnerMs)
		r.setClock(func() int64 { return ms })
		_ = group.Render(100)
	}
}

// BenchmarkToolGroupRenderWidthSameFrame is the hot path after the fix: the
// host Working tick re-enters at the same spinner frame and must hit the cache.
func BenchmarkToolGroupRenderWidthSameFrame(b *testing.B) {
	r, _, _ := startedRuntime(b)
	r.setClock(func() int64 { return 1_000 })
	r.spinnerStartMs = 0
	group := r.newToolGroup(0)
	tool := newTestTool("call-1", "bash", map[string]any{"command": "sleep 1"}).pending()
	tool.owner = group
	r.mu.Lock()
	group.entries = []groupEntry{{kind: "tool", tool: tool}}
	group.children = []*toolComponent{tool}
	r.addGroupLocked(group)
	r.mu.Unlock()
	_ = group.Render(100) // warm cache

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = group.Render(100)
	}
}

func TestRenderCacheHitsSameSpinnerFrame(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.setClock(func() int64 { return 500 })
	r.spinnerStartMs = 0
	group := r.newToolGroup(0)
	tool := newTestTool("cache-1", "read", map[string]any{"path": "/etc/hosts"}).pending()
	tool.owner = group
	r.mu.Lock()
	group.entries = []groupEntry{{kind: "tool", tool: tool}}
	group.children = []*toolComponent{tool}
	r.addGroupLocked(group)
	r.mu.Unlock()

	first := group.Render(80)
	if len(first) == 0 {
		t.Fatal("expected rendered lines")
	}
	r.mu.Lock()
	cached := group.rendered
	r.mu.Unlock()
	if cached == nil {
		t.Fatal("render did not populate the cache")
	}
	_ = group.Render(80)
	r.mu.Lock()
	same := len(group.rendered) > 0 && &group.rendered[0] == &cached[0]
	r.mu.Unlock()
	if !same {
		t.Fatal("same-frame re-render rebuilt the cache slice")
	}

	// Advancing past one spinner interval must miss the cache.
	r.setClock(func() int64 { return 500 + int64(spinnerMs) })
	_ = group.Render(80)
	r.mu.Lock()
	newSpin := group.renderedSpinIndex
	r.mu.Unlock()
	if newSpin == 0 {
		t.Fatalf("spin index stayed 0 after advancing %dms", spinnerMs)
	}
}

func TestAnimationTickInvalidatesToolCardNotRequestRender(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 0 })
	group := r.newToolGroup(0)
	tool := newTestTool("anim-1", "read", map[string]any{"path": "/tmp/x"}).pending()
	tool.owner = group
	var invalidates atomic.Int32
	r.mu.Lock()
	group.entries = []groupEntry{{kind: "tool", tool: tool}}
	group.children = []*toolComponent{tool}
	r.addGroupLocked(group)
	r.toolCardInvalidate = map[string]func(){
		"anim-1": func() { invalidates.Add(1) },
	}
	r.mu.Unlock()

	before := tui.renderCount()
	r.scheduleAnimation()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if invalidates.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if invalidates.Load() == 0 {
		t.Fatal("animation tick did not invalidate the display tool card")
	}
	// Give a moment for a mistaken requestRender to land.
	time.Sleep(50 * time.Millisecond)
	if got := tui.renderCount(); got != before {
		t.Errorf("animation tick called requestRender (%d -> %d); want card Invalidate only", before, got)
	}
	r.stopAnimation()
}

func TestAnimationTickFallsBackToRequestRenderWithoutCardHook(t *testing.T) {
	r, _, tui := startedRuntime(t)
	r.setClock(func() int64 { return 0 })
	group := r.newToolGroup(0)
	group.entries = []groupEntry{{kind: "tool", tool: newTestTool("p", "read", nil).pending()}}
	r.mu.Lock()
	r.addGroupLocked(group)
	r.mu.Unlock()

	before := tui.renderCount()
	r.scheduleAnimation()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tui.renderCount() > before {
			r.stopAnimation()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.stopAnimation()
	t.Fatal("fallback requestRender never ran without a card Invalidate hook")
}
