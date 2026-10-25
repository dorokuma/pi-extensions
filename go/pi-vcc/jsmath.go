package pivcc

import (
	"math"
	"sort"
	"time"
)

// Small shared helpers: JavaScript's `Math.log`, a millisecond clock, and the
// score-ordered sort the recall search uses.

// jsLog is JavaScript's `Math.log` (natural logarithm).
func jsLog(value float64) float64 {
	return math.Log(value)
}

// nowMillis is `Date.now()`: milliseconds since the Unix epoch.
func nowMillis() int64 {
	return time.Now().UnixMilli()
}

// scoredHit is one candidate hit with its BM25 score.
type scoredHit struct {
	hit   SearchHit
	score float64
}

// stableSortByScore orders hits by score, highest first, keeping the original
// order for equal scores — the same result V8's stable Array.prototype.sort
// gives the TS original.
func stableSortByScore(scored []scoredHit) {
	sort.SliceStable(scored, func(a, b int) bool {
		return scored[a].score > scored[b].score
	})
}
