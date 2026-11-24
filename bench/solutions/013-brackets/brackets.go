// Package brackets checks bracket nesting.
package brackets

// Balanced reports whether the brackets in s are balanced and nested.
func Balanced(s string) bool {
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	var stack []rune
	inString, escaped := false, false
	for _, r := range s {
		if inString {
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '(', '[', '{':
			stack = append(stack, r)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}
