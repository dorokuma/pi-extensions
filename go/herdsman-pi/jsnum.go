package herdsmanpi

import (
	"math"
	"strconv"
)

func formatInt(value int64) string { return strconv.FormatInt(value, 10) }

func formatFloat(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }

// jsNumber renders a float64 the way JS `String(number)` does for the values
// this extension can see (ids, counts, timestamps): integral values print
// without a fractional part and the non-finite values have their JS spellings.
func jsNumber(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return formatInt(int64(value))
	}
	return formatFloat(value)
}
