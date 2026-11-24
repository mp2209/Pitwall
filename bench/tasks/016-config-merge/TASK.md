merge.go merges configuration maps. `Merge(base, override)` returns a new
map: nested maps merge key by key, any other value (slices included) from
override replaces the base value, and a nil value in override deletes that
key. Neither input may be modified, and changing the result must not change
the inputs either. `Layered` in layers.go applies several layers in order
using Merge. Fix it so the tests pass. Do not change the tests.
