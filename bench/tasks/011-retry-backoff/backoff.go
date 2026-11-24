// Package backoff computes retry delays.
package backoff

import "time"

// Delay returns how long to wait before retry number attempt.
func Delay(attempt int, base, max time.Duration) time.Duration {
	return base * time.Duration(1<<attempt)
}
