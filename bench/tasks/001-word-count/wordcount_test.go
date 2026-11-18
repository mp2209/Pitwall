package wordcount

import "testing"

func TestCount(t *testing.T) {
	cases := map[string]int{
		"":                        0,
		"hello":                   1,
		"hello world":             2,
		"  leading and trailing ": 3,
		"tabs\tand\nnewlines":     3,
		"many    spaces   here":   3,
		"   ":                     0,
	}
	for in, want := range cases {
		if got := Count(in); got != want {
			t.Errorf("Count(%q) = %d, want %d", in, got, want)
		}
	}
}
