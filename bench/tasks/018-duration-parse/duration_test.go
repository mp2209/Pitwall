package duration

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := map[string]time.Duration{
		"90s":        90 * time.Second,
		"1h30m":      90 * time.Minute,
		"1h 30m":     90 * time.Minute,
		"2d":         48 * time.Hour,
		"1d12h":      36 * time.Hour,
		"250ms":      250 * time.Millisecond,
		"1m30s500ms": 90*time.Second + 500*time.Millisecond,
		"1.5h":       90 * time.Minute,
		"0s":         0,
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestErrors(t *testing.T) {
	for _, in := range []string{"", " ", "90", "h", "5x", "1h-2m", "1.2.3s"} {
		if d, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, d)
		}
	}
}
