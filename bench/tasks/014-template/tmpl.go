// Package tmpl fills {{placeholders}} in text.
package tmpl

import "strings"

// Render replaces each {{key}} in t with vars[key].
func Render(t string, vars map[string]string) (string, error) {
	for k, v := range vars {
		t = strings.ReplaceAll(t, "{{"+k+"}}", v)
	}
	return t, nil
}
