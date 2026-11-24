`Valid` in luhn.go checks a number with the Luhn algorithm: starting from
the rightmost digit, double every second digit (subtracting 9 when the
result is over 9) and check that the sum is divisible by 10. Spaces may
appear between digits; any other non-digit makes the number invalid, and so
does having fewer than two digits. Fix it so the tests pass. Do not change
the tests.
