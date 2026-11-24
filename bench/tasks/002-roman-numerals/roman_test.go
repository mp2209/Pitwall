package roman

import "testing"

func TestToRoman(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "I"}, {3, "III"}, {4, "IV"}, {9, "IX"}, {14, "XIV"}, {40, "XL"}, {90, "XC"},
		{400, "CD"}, {944, "CMXLIV"}, {1994, "MCMXCIV"}, {2024, "MMXXIV"}, {3999, "MMMCMXCIX"},
	}
	for _, c := range cases {
		got, err := ToRoman(c.n)
		if err != nil || got != c.want {
			t.Errorf("ToRoman(%d) = %q, %v; want %q", c.n, got, err, c.want)
		}
	}
}

func TestOutOfRange(t *testing.T) {
	for _, n := range []int{0, -1, 4000} {
		if _, err := ToRoman(n); err == nil {
			t.Errorf("ToRoman(%d) should fail", n)
		}
	}
}
