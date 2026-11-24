package search

import "testing"

func TestFindsEveryElement(t *testing.T) {
	xs := []int{1, 3, 5, 7, 9, 11}
	for i, x := range xs {
		if got := Index(xs, x); got != i {
			t.Errorf("Index(%v, %d) = %d, want %d", xs, x, got, i)
		}
	}
}

func TestMissing(t *testing.T) {
	xs := []int{1, 3, 5, 7, 9}
	for _, x := range []int{0, 2, 4, 10} {
		if got := Index(xs, x); got != -1 {
			t.Errorf("Index(%v, %d) = %d, want -1", xs, x, got)
		}
	}
}

func TestSmallSlices(t *testing.T) {
	if got := Index(nil, 1); got != -1 {
		t.Errorf("empty slice: %d", got)
	}
	if got := Index([]int{5}, 5); got != 0 {
		t.Errorf("single element: %d", got)
	}
	if got := Index([]int{5}, 6); got != -1 {
		t.Errorf("single element, missing: %d", got)
	}
}
