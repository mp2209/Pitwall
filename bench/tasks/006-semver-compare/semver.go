// Package semver orders semantic versions.
package semver

import "strings"

// Compare returns -1, 0 or 1 as version a is lower than, equal to, or
// higher than version b.
func Compare(a, b string) int {
	return strings.Compare(a, b)
}
