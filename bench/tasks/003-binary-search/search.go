// Package search finds values in sorted slices.
package search

// Index returns the position of target in the sorted slice xs, or -1.
func Index(xs []int, target int) int {
	lo, hi := 0, len(xs)-1
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case xs[mid] == target:
			return mid
		case xs[mid] < target:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return -1
}
