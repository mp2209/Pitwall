// Package mdtable renders Markdown tables.
package mdtable

import "strings"

// Table renders header and rows as a Markdown table.
func Table(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for i, w := range widths {
			b.WriteString(" " + cells[i] + strings.Repeat(" ", w-len(cells[i])) + " |")
		}
		b.WriteString("\n")
	}
	line(header)
	seps := make([]string, len(widths))
	for i, w := range widths {
		seps[i] = strings.Repeat("-", w)
	}
	line(seps)
	for _, r := range rows {
		line(r)
	}
	return b.String()
}
