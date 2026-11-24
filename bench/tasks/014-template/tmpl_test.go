package tmpl

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	vars := map[string]string{"name": "Ada", "greeting": "Hello", "a": "{{b}}", "b": "B"}
	cases := map[string]string{
		"Hello, {{name}}!":          "Hello, Ada!",
		"{{ greeting }}, {{name}}.": "Hello, Ada.",
		"{{name}}{{name}}":          "AdaAda",
		"no placeholders":           "no placeholders",
		"{{a}}":                     "{{b}}",
		"a { b } c":                 "a { b } c",
		"{{greeting}}":              "Hello",
	}
	for in, want := range cases {
		got, err := Render(in, vars)
		if err != nil || got != want {
			t.Errorf("Render(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestMissingValue(t *testing.T) {
	_, err := Render("Dear {{ title }} {{name}}", map[string]string{"name": "Ada"})
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("a missing value should be an error naming it, got %v", err)
	}
}

func TestUnclosed(t *testing.T) {
	if _, err := Render("Hello {{name", map[string]string{"name": "Ada"}); err == nil {
		t.Error("an unclosed placeholder should be an error")
	}
}
