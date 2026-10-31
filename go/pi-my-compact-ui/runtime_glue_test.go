package compactui

import (
	"sync"
	"testing"
)

// TestTickerStopConcurrentNoPanic is the H13 regression: ticker.stop used to
// probe a once channel with a non-atomic select/default and then close both
// channels, so two concurrent stops could both fall into the default branch
// and panic with "close of closed channel". Stop must be idempotent under
// concurrency; a panic in any round fails this test outright.
func TestTickerStopConcurrentNoPanic(t *testing.T) {
	t.Run("SequentialDoubleStop", func(t *testing.T) {
		tk := newTicker(10)
		tk.stop()
		tk.stop()
	})
	t.Run("ConcurrentStops", func(t *testing.T) {
		const rounds = 200
		const goroutines = 16
		for i := 0; i < rounds; i++ {
			tk := newTicker(10)
			// A release barrier so all goroutines race the same stop window
			// instead of draining it one at a time as they are scheduled.
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for j := 0; j < goroutines; j++ {
				go func() {
					defer wg.Done()
					<-start
					tk.stop()
				}()
			}
			close(start)
			wg.Wait()
		}
	})
}
