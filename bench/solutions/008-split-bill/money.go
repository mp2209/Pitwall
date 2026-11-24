// Package money handles amounts in cents.
package money

import (
	"errors"
	"fmt"
)

// Split divides totalCents among n people as evenly as possible.
func Split(totalCents int64, n int) ([]int64, error) {
	if n <= 0 {
		return nil, errors.New("n must be positive")
	}
	if totalCents < 0 {
		return nil, errors.New("total must not be negative")
	}
	base, extra := totalCents/int64(n), totalCents%int64(n)
	out := make([]int64, n)
	for i := range out {
		out[i] = base
		if int64(i) < extra {
			out[i]++
		}
	}
	return out, nil
}

// Format renders cents as dollars.
func Format(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
