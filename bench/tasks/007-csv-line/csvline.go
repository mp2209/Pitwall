// Package csvline parses single CSV records.
package csvline

import "strings"

// ParseLine splits one CSV record into fields.
func ParseLine(line string) ([]string, error) {
	return strings.Split(line, ","), nil
}
