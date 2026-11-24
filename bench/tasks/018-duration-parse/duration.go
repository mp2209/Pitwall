// Package duration parses human-friendly durations.
package duration

import (
	"fmt"
	"time"
)

var units = map[string]time.Duration{
	"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second, "ms": time.Millisecond,
}

// Parse reads a duration like "1h30m" or "2d".
func Parse(s string) (time.Duration, error) {
	var total time.Duration
	num := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
		default:
			u, ok := units[string(r)]
			if !ok {
				return 0, fmt.Errorf("unknown unit %q", r)
			}
			total += time.Duration(num) * u
			num = 0
		}
	}
	return total, nil
}
