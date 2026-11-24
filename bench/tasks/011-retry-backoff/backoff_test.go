package backoff

import (
	"testing"
	"time"
)

func TestDelay(t *testing.T) {
	const base, max = 100 * time.Millisecond, 10 * time.Second
	cases := map[int]time.Duration{
		0: 100 * time.Millisecond, 1: 200 * time.Millisecond, 3: 800 * time.Millisecond,
		6: 6400 * time.Millisecond, 7: max, 50: max, 1000: max, -1: 100 * time.Millisecond,
	}
	for attempt, want := range cases {
		if got := Delay(attempt, base, max); got != want {
			t.Errorf("Delay(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestBaseAboveMax(t *testing.T) {
	if got := Delay(0, time.Minute, time.Second); got != time.Second {
		t.Errorf("Delay with base above max = %v, want 1s", got)
	}
}
