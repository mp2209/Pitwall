`Delay` in backoff.go returns how long to wait before retry number attempt
(counting from 0): the base delay doubled once per attempt, but never more
than max, even for very large attempt numbers. A negative attempt counts as
0. Today it ignores max, overflows, and panics on negative attempts. Fix it
so the tests pass. Do not change the tests.
