// Package luhn validates numbers with the Luhn checksum.
package luhn

import "strings"

// Valid reports whether number passes the Luhn checksum.
func Valid(number string) bool {
	s := strings.ReplaceAll(number, " ", "")
	if len(s) < 2 {
		return false
	}
	sum := 0
	for i := 0; i < len(s); i++ {
		c := s[len(s)-1-i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
