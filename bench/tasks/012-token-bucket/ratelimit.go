// Package ratelimit is a token-bucket rate limiter.
package ratelimit

import "time"

// Clock returns the current time; tests pass a fake one.
type Clock func() time.Time

// Bucket holds up to capacity tokens and gains rate tokens per second.
type Bucket struct {
	capacity float64
	rate     float64
	tokens   float64
	last     time.Time
	now      Clock
}

// New returns a full bucket.
func New(capacity int, ratePerSecond float64, now Clock) *Bucket {
	return &Bucket{capacity: float64(capacity), rate: ratePerSecond, tokens: float64(capacity), last: now(), now: now}
}

// Allow takes n tokens if they are available and reports whether it did.
func (b *Bucket) Allow(n int) bool {
	t := b.now()
	elapsed := int(t.Sub(b.last).Seconds())
	b.tokens += float64(elapsed) * b.rate
	b.last = t
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}
