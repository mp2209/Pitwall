// Package stats computes summary statistics.
package stats

import (
	"errors"
	"sort"
)

// Percentile returns the p-th percentile of xs.
func Percentile(xs []float64, p float64) (float64, error) {
	if len(xs) == 0 {
		return 0, errors.New("no data")
	}
	sort.Float64s(xs)
	i := int(p / 100 * float64(len(xs)))
	return xs[i], nil
}

// Median returns the 50th percentile of xs.
func Median(xs []float64) (float64, error) { return Percentile(xs, 50) }
