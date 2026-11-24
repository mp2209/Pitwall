package money

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		total int64
		n     int
		want  []int64
	}{
		{100, 3, []int64{34, 33, 33}},
		{5, 3, []int64{2, 2, 1}},
		{99, 1, []int64{99}},
		{0, 2, []int64{0, 0}},
		{1001, 4, []int64{251, 250, 250, 250}},
	}
	for _, c := range cases {
		got, err := Split(c.total, c.n)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%d, %d) = %v, %v; want %v", c.total, c.n, got, err, c.want)
		}
	}
}

func TestSplitErrors(t *testing.T) {
	if _, err := Split(100, 0); err == nil {
		t.Error("n = 0 should fail")
	}
	if _, err := Split(-1, 2); err == nil {
		t.Error("a negative total should fail")
	}
}

func TestFormat(t *testing.T) {
	cases := map[int64]string{1234: "$12.34", 5: "$0.05", 0: "$0.00", -5: "-$0.05", -1234: "-$12.34", 100000: "$1000.00"}
	for cents, want := range cases {
		if got := Format(cents); got != want {
			t.Errorf("Format(%d) = %q, want %q", cents, got, want)
		}
	}
}
