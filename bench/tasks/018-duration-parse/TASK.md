`Parse` in duration.go reads durations such as "90s", "1h30m", "2d",
"1d12h", "250ms" and "1.5h". A duration is one or more number-and-unit
parts; units are d (24 hours), h, m, s and ms; numbers may have a decimal
part; parts may be separated by spaces ("1h 30m"). Empty input, a number
without a unit, a unit without a number and unknown units are errors. Fix
it so the tests pass. Do not change the tests.
