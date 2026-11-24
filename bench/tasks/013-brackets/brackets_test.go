package brackets

import "testing"

func TestBalanced(t *testing.T) {
	cases := map[string]bool{
		"":                    true,
		"()":                  true,
		"([]{})":              true,
		"f(a[1], {b: 2})":     true,
		`{"key": [1, 2]}`:     true,
		"([)]":                false,
		")(":                  false,
		"((":                  false,
		"}":                   false,
		`"("`:                 true,
		`f("a)b")`:            true,
		`"\"("`:               true,
		`"\\"(`:               false,
		`print("{")}`:         false,
		`x = ["]", "[", "}"]`: true,
	}
	for in, want := range cases {
		if got := Balanced(in); got != want {
			t.Errorf("Balanced(%s) = %v, want %v", in, got, want)
		}
	}
}
