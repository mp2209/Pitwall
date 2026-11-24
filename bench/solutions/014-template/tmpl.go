// Package tmpl fills {{placeholders}} in text.
package tmpl

import (
	"fmt"
	"strings"
)

// Render replaces each {{key}} in t with vars[key].
func Render(t string, vars map[string]string) (string, error) {
	var b strings.Builder
	for {
		start := strings.Index(t, "{{")
		if start < 0 {
			b.WriteString(t)
			return b.String(), nil
		}
		end := strings.Index(t[start+2:], "}}")
		if end < 0 {
			return "", fmt.Errorf("unclosed placeholder at %q", t[start:])
		}
		key := strings.TrimSpace(t[start+2 : start+2+end])
		v, ok := vars[key]
		if !ok {
			return "", fmt.Errorf("no value for %q", key)
		}
		b.WriteString(t[:start])
		b.WriteString(v)
		t = t[start+2+end+2:]
	}
}
