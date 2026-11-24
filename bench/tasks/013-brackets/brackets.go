// Package brackets checks bracket nesting.
package brackets

// Balanced reports whether the brackets in s are balanced and nested.
func Balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		}
	}
	return depth == 0
}
