`Percentile` in stats.go returns the p-th percentile (p from 0 to 100) of a
list of numbers, interpolating linearly between the two nearest ranks: the
rank is p/100 * (n-1) in the sorted list. It must not reorder the caller's
slice. An empty list or a p outside 0..100 is an error. `Median` uses it.
Today it panics on p=100, skips interpolation and sorts the input in
place. Fix it so the tests pass. Do not change the tests.
