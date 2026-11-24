// Package roman converts integers to Roman numerals.
package roman

import (
	"fmt"
	"strings"
)

var numerals = []struct {
	value  int
	symbol string
}{
	{1000, "M"}, {500, "D"}, {100, "C"}, {50, "L"}, {10, "X"}, {5, "V"}, {1, "I"},
}

// ToRoman converts n (1 to 3999) to a Roman numeral.
func ToRoman(n int) (string, error) {
	if n <= 0 || n > 3999 {
		return "", fmt.Errorf("out of range: %d", n)
	}
	var b strings.Builder
	for _, num := range numerals {
		for n >= num.value {
			b.WriteString(num.symbol)
			n -= num.value
		}
	}
	return b.String(), nil
}
