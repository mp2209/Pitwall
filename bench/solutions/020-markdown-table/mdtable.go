// Package mdtable renders Markdown tables.
package mdtable

import (
	"strings"
	"unicode/utf8"
)

// Table renders header and rows as a Markdown table.
func Table(header []string, rows [][]string) string {
	cell := func(cells []string, i int) string {
		if i >= len(cells) {
			return ""
		}
		return strings.ReplaceAll(cells[i], "|", "\\|")
	}
	widths := make([]int, len(header))
	for i := range header {
		widths[i] = max(3, utf8.RuneCountInString(cell(header, i)))
		for _, r := range rows {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell(r, i)))
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for i, w := range widths {
			c := cell(cells, i)
			b.WriteString(" " + c + strings.Repeat(" ", w-utf8.RuneCountInString(c)) + " |")
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
