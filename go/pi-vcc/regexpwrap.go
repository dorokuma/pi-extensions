package pivcc

import "regexp"

// mustRegexp wraps a compiled regexp so call sites can keep the TS shape of a
// regex constant (`RE.test(x)`, `RE.exec(x)`).
type mustRegexp struct {
	re *regexp.Regexp
}

// newMustRegexp compiles a pattern that must be valid.
func newMustRegexp(pattern string) *mustRegexp {
	return &mustRegexp{re: regexp.MustCompile(pattern)}
}
