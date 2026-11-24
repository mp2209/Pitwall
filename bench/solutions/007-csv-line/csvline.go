// Package csvline parses single CSV records.
package csvline

import (
	"errors"
	"strings"
)

// ParseLine splits one CSV record into fields.
func ParseLine(line string) ([]string, error) {
	var fields []string
	var field strings.Builder
	inQuotes := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuotes && c == '"' && i+1 < len(line) && line[i+1] == '"':
			field.WriteByte('"')
			i++
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			fields = append(fields, field.String())
			field.Reset()
		default:
			field.WriteByte(c)
		}
	}
	if inQuotes {
		return nil, errors.New("unterminated quote")
	}
	return append(fields, field.String()), nil
}
