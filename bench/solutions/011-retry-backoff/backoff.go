// Package backoff computes retry delays.
package backoff

import "time"

// Delay returns how long to wait before retry number attempt.
func Delay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := base
	for i := 0; i < attempt; i++ {
		if d >= max/2 {
			return max // doubling would pass max, and could overflow
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}
