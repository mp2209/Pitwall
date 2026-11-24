package semver

import "testing"

// Each version is lower than the next.
var ordered = []string{
	"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
	"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.9.0", "1.10.0", "1.10.10", "2.0.0", "10.0.0",
}

func TestOrder(t *testing.T) {
	for i := 0; i+1 < len(ordered); i++ {
		a, b := ordered[i], ordered[i+1]
		if got := Compare(a, b); got != -1 {
			t.Errorf("Compare(%s, %s) = %d, want -1", a, b, got)
		}
		if got := Compare(b, a); got != 1 {
			t.Errorf("Compare(%s, %s) = %d, want 1", b, a, got)
		}
	}
}

func TestEqual(t *testing.T) {
	for _, p := range [][2]string{{"1.2.3", "1.2.3"}, {"v1.2.3", "1.2.3"}, {"1.0.0-rc.1", "v1.0.0-rc.1"}} {
		if got := Compare(p[0], p[1]); got != 0 {
			t.Errorf("Compare(%s, %s) = %d, want 0", p[0], p[1], got)
		}
	}
}
