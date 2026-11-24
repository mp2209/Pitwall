`Wrap` in wrap.go breaks text into lines of at most width characters,
breaking only between words. Any run of whitespace (spaces, tabs, newlines)
counts as a single space, lines have no leading or trailing spaces, a word
longer than width gets a line of its own, and empty text gives no lines.
Fix it so the tests pass. Do not change the tests.
