package stats

import (
	"math"
	"reflect"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPercentile(t *testing.T) {
	xs := []float64{15, 20, 35, 40, 50}
	cases := map[float64]float64{0: 15, 25: 20, 40: 29, 50: 35, 90: 46, 100: 50}
	for p, want := range cases {
		got, err := Percentile(xs, p)
		if err != nil || !near(got, want) {
			t.Errorf("Percentile(%v, %v) = %v, %v; want %v", xs, p, got, err, want)
		}
	}
}

func TestDoesNotReorderInput(t *testing.T) {
	xs := []float64{3, 1, 2}
	if got, _ := Percentile(xs, 50); !near(got, 2) {
		t.Errorf("median of 3,1,2 = %v", got)
	}
	if !reflect.DeepEqual(xs, []float64{3, 1, 2}) {
		t.Errorf("input was reordered: %v", xs)
	}
}

func TestMedianAndSingle(t *testing.T) {
	if got, _ := Median([]float64{4, 1, 3, 2}); !near(got, 2.5) {
		t.Errorf("Median(4,1,3,2) = %v, want 2.5", got)
	}
	if got, _ := Percentile([]float64{7}, 90); !near(got, 7) {
		t.Errorf("one value: %v", got)
	}
}

func TestErrors(t *testing.T) {
	if _, err := Percentile(nil, 50); err == nil {
		t.Error("no data should fail")
	}
	for _, p := range []float64{-1, 101} {
		if _, err := Percentile([]float64{1, 2}, p); err == nil {
			t.Errorf("p = %v should fail", p)
		}
	}
}
