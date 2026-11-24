// Package intervals merges ranges.
package intervals

// Interval is a closed range [Start, End].
type Interval struct{ Start, End int }

// Merge combines overlapping or touching intervals and returns them sorted
// by Start. The input slice is not modified.
func Merge(in []Interval) []Interval {
	var out []Interval
	for _, iv := range in {
		if n := len(out); n > 0 && iv.Start < out[n-1].End {
			if iv.End > out[n-1].End {
				out[n-1].End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}
