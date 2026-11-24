`Bucket` in ratelimit.go is a token bucket. It starts full, holds at most
capacity tokens, and refills continuously at rate tokens per second, so half
a second at 2 tokens/s adds one token. `Allow(n)` takes n tokens if they are
there. Today partial seconds are lost and a long idle period lets the bucket
overfill. Fix it so the tests pass. Do not change the tests.
