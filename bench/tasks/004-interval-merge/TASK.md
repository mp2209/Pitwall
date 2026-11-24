`Merge` in intervals.go combines overlapping intervals. It should also merge
intervals that only touch ([1,2] and [2,3] become [1,3]), accept input in
any order, return the result sorted by start, and leave the caller's slice
unchanged. Fix it so the tests pass. Do not change the tests.
