package wrap

import (
	"reflect"
	"testing"
)

func TestWrap(t *testing.T) {
	cases := []struct {
		text  string
		width int
		want  []string
	}{
		{"the quick brown fox", 10, []string{"the quick", "brown fox"}},
		{"the quick brown fox", 9, []string{"the quick", "brown fox"}},
		{"a  b\n\tc", 3, []string{"a b", "c"}},
		{"hi supercalifragilistic yo", 5, []string{"hi", "supercalifragilistic", "yo"}},
		{"abcde fghij", 5, []string{"abcde", "fghij"}},
		{"abcde fghij", 11, []string{"abcde fghij"}},
		{"  padded  ", 10, []string{"padded"}},
	}
	for _, c := range cases {
		if got := Wrap(c.text, c.width); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Wrap(%q, %d) = %q, want %q", c.text, c.width, got, c.want)
		}
	}
}

func TestEmpty(t *testing.T) {
	if got := Wrap("   ", 10); len(got) != 0 {
		t.Errorf("blank text gave %q", got)
	}
}
