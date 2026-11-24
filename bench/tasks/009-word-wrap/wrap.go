// Package wrap breaks text into lines.
package wrap

import "strings"

// Wrap breaks text into lines of at most width characters.
func Wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Split(text, " ") {
		if len(line)+len(w) > width {
			lines = append(lines, line)
			line = ""
		}
		line += w + " "
	}
	return append(lines, line)
}
