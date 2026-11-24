// Package luhn validates numbers with the Luhn checksum.
package luhn

// Valid reports whether number passes the Luhn checksum.
func Valid(number string) bool {
	sum := 0
	for i, r := range number {
		d := int(r - '0')
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
