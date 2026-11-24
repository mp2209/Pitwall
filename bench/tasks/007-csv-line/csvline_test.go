package csvline

import (
	"reflect"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a,b,c`, []string{"a", "b", "c"}},
		{`"a,b",c`, []string{"a,b", "c"}},
		{`"say ""hi""",x`, []string{`say "hi"`, "x"}},
		{`a,,c`, []string{"a", "", "c"}},
		{`a,`, []string{"a", ""}},
		{`""`, []string{""}},
		{`x,"multi word, with comma",42`, []string{"x", "multi word, with comma", "42"}},
	}
	for _, c := range cases {
		got, err := ParseLine(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseLine(%s) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestUnterminatedQuote(t *testing.T) {
	if _, err := ParseLine(`"abc,def`); err == nil {
		t.Error("an unterminated quote should be an error")
	}
}
