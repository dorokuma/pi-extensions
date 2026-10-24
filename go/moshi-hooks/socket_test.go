package moshihooks

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveSocketPathOverride(t *testing.T) {
	t.Setenv("MOSHI_SOCKET_PATH", "/tmp/throwaway.sock")
	if got := ResolveSocketPath(); got != "/tmp/throwaway.sock" {
		t.Fatalf("ResolveSocketPath = %q, want the MOSHI_SOCKET_PATH override", got)
	}
}

func TestResolveSocketPathFromXDGRuntimeDir(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skipf("platform default applies on %s", runtime.GOOS)
	}
	t.Setenv("MOSHI_SOCKET_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got, want := ResolveSocketPath(), "/run/user/1000/moshi-hook.sock"; got != want {
		t.Fatalf("ResolveSocketPath = %q, want %q", got, want)
	}
}

func TestResolveSocketPathDefault(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skipf("platform default applies on %s", runtime.GOOS)
	}
	t.Setenv("MOSHI_SOCKET_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got, want := ResolveSocketPath(), "/tmp/moshi-hook.sock"; got != want {
		t.Fatalf("ResolveSocketPath = %q, want %q", got, want)
	}
}

// recordingSender builds a sender whose delivery records the session ids of the
// envelopes it sees, in delivery order.
func orderedSender(t *testing.T, deliver func(envelope *Envelope, index int)) (*Sender, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	var counter int64
	sender := newSender(func() string { return "" }, func(envelope *Envelope, _ string, _ time.Duration) {
		index := int(atomic.AddInt64(&counter, 1) - 1)
		if deliver != nil {
			deliver(envelope, index)
		}
		mu.Lock()
		seen = append(seen, envelope.SessionID)
		mu.Unlock()
	}, SendTimeout)
	t.Cleanup(func() { sender.Flush(2 * time.Second) })
	return sender, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

func TestSenderKeepsStrictOrder(t *testing.T) {
	sender, seen := orderedSender(t, nil)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		sender.Send(&Envelope{SessionID: id})
	}
	sender.Flush(2 * time.Second)

	got := seen()
	want := []string{"1", "2", "3", "4", "5"}
	if len(got) != len(want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	}
}

func TestSenderNeverRunsTwoDeliveriesAtOnce(t *testing.T) {
	var concurrent int32
	var violated int32
	sender, seen := orderedSender(t, func(_ *Envelope, _ int) {
		if atomic.AddInt32(&concurrent, 1) > 1 {
			atomic.StoreInt32(&violated, 1)
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
	})

	for index := 0; index < 8; index++ {
		sender.Send(&Envelope{SessionID: "id"})
	}
	sender.Flush(3 * time.Second)

	if atomic.LoadInt32(&violated) != 0 {
		t.Fatal("two envelopes were delivered concurrently; the queue must be serial")
	}
	if len(seen()) != 8 {
		t.Fatalf("delivered %d envelopes, want 8", len(seen()))
	}
}

func TestFlushWaitsForQueuedEnvelopes(t *testing.T) {
	release := make(chan struct{})
	sender, seen := orderedSender(t, func(_ *Envelope, _ int) { <-release })
	sender.Send(&Envelope{SessionID: "blocked"})

	flushed := make(chan struct{})
	go func() {
		sender.Flush(2 * time.Second)
		close(flushed)
	}()

	select {
	case <-flushed:
		t.Fatal("Flush returned while an envelope was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("Flush did not return after the delivery finished")
	}
	if len(seen()) != 1 || sender.Pending() != 0 {
		t.Fatalf("seen = %v, pending = %d", seen(), sender.Pending())
	}
}

func TestFlushHonoursItsTimeout(t *testing.T) {
	release := make(chan struct{})
	sender, _ := orderedSender(t, func(_ *Envelope, _ int) { <-release })
	sender.Send(&Envelope{SessionID: "stuck"})

	started := time.Now()
	sender.Flush(20 * time.Millisecond)
	elapsed := time.Since(started)
	close(release)

	if elapsed < 15*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("Flush returned after %v, want roughly the 20ms cap", elapsed)
	}
	if sender.Pending() != 1 {
		t.Fatalf("Pending = %d, want the stuck envelope still counted", sender.Pending())
	}
}

func TestFlushWithoutWorkReturnsImmediately(t *testing.T) {
	sender, _ := orderedSender(t, nil)
	started := time.Now()
	sender.Flush(5 * time.Second)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("empty Flush took %v", elapsed)
	}
}
