`Balanced` in brackets.go reports whether the (), [] and {} in a string are
balanced and properly nested: "([)]" and ")(" are not. Brackets inside
double-quoted strings don't count, and inside a string a backslash escapes
the next character, so `"\""` is one complete string. Fix it so the tests
pass. Do not change the tests.
