// Package duration parses human-friendly durations.
package duration

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var units = map[string]time.Duration{
	"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second, "ms": time.Millisecond,
}

// Parse reads a duration like "1h30m" or "2d".
func Parse(s string) (time.Duration, error) {
	rest := strings.TrimSpace(s)
	if rest == "" {
		return 0, errors.New("empty duration")
	}
	var total time.Duration
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("expected a number in %q", s)
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("bad number in %q", s)
		}
		rest = rest[i:]
		j := 0
		for j < len(rest) && rest[j] >= 'a' && rest[j] <= 'z' {
			j++
		}
		u, ok := units[rest[:j]]
		if !ok {
			return 0, fmt.Errorf("bad unit in %q", s)
		}
		total += time.Duration(n * float64(u))
		rest = strings.TrimLeft(rest[j:], " ")
	}
	return total, nil
}
