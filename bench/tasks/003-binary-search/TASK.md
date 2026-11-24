`Index` in search.go should return the position of a value in a sorted
slice, or -1 when it isn't there. It misses some values that are present.
Fix it so the tests pass. Do not change the tests.
