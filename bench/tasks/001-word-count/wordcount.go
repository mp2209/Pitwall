// Package wordcount counts words in text.
package wordcount

import "strings"

// Count returns the number of words in s. Words are separated by
// whitespace.
func Count(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, " "))
}
