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
	{1000, "M"}, {900, "CM"}, {500, "D"}, {400, "CD"}, {100, "C"}, {90, "XC"},
	{50, "L"}, {40, "XL"}, {10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"},
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
