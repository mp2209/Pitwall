package luhn

import "testing"

func TestValid(t *testing.T) {
	cases := map[string]bool{
		"4539 3195 0343 6467": true,
		"8273 1232 7352 0569": false,
		"059":                 true,
		"59":                  true,
		"091":                 true,
		"055 444 285":         true,
		"055a 444 285":        false,
		"0":                   false,
		" 0":                  false,
		"1":                   false,
		"055-444-285":         false,
		"79927398713":         true,
		"79927398710":         false,
	}
	for in, want := range cases {
		if got := Valid(in); got != want {
			t.Errorf("Valid(%q) = %v, want %v", in, got, want)
		}
	}
}
