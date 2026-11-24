package intervals

import (
	"reflect"
	"testing"
)

func TestMerge(t *testing.T) {
	cases := []struct {
		in, want []Interval
	}{
		{[]Interval{{1, 3}, {2, 6}, {8, 10}}, []Interval{{1, 6}, {8, 10}}},
		{[]Interval{{8, 10}, {1, 3}, {2, 6}}, []Interval{{1, 6}, {8, 10}}},
		{[]Interval{{1, 2}, {2, 3}}, []Interval{{1, 3}}},
		{[]Interval{{1, 10}, {2, 3}, {4, 5}}, []Interval{{1, 10}}},
		{[]Interval{{5, 6}, {1, 2}, {3, 4}}, []Interval{{1, 2}, {3, 4}, {5, 6}}},
	}
	for _, c := range cases {
		if got := Merge(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Merge(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEmpty(t *testing.T) {
	if got := Merge(nil); len(got) != 0 {
		t.Errorf("Merge(nil) = %v", got)
	}
}

func TestInputUnchanged(t *testing.T) {
	in := []Interval{{8, 10}, {1, 3}, {2, 6}}
	Merge(in)
	if !reflect.DeepEqual(in, []Interval{{8, 10}, {1, 3}, {2, 6}}) {
		t.Errorf("input was modified: %v", in)
	}
}
