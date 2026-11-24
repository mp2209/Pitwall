// Package stats computes summary statistics.
package stats

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Percentile returns the p-th percentile of xs.
func Percentile(xs []float64, p float64) (float64, error) {
	if len(xs) == 0 {
		return 0, errors.New("no data")
	}
	if p < 0 || p > 100 || math.IsNaN(p) {
		return 0, fmt.Errorf("percentile %v is outside 0..100", p)
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := p / 100 * float64(len(s)-1)
	lo, hi := int(math.Floor(rank)), int(math.Ceil(rank))
	return s[lo] + (rank-float64(lo))*(s[hi]-s[lo]), nil
}

// Median returns the 50th percentile of xs.
func Median(xs []float64) (float64, error) { return Percentile(xs, 50) }
