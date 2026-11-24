`ToRoman` in roman.go converts an integer from 1 to 3999 to a Roman numeral
and returns an error for anything outside that range. It gets some numbers
wrong: 4 must be "IV", 9 "IX", 40 "XL", 90 "XC", 400 "CD" and 900 "CM", so
1994 is "MCMXCIV". Fix it so the tests pass. Do not change the tests.
