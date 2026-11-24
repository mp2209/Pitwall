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
	share := float64(totalCents) / float64(n)
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(share)
	}
	return out, nil
}

// Format renders cents as dollars.
func Format(cents int64) string {
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}
