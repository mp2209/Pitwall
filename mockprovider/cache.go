package mockprovider

import (
	"hash"
	"hash/fnv"
	"sync"
)

// prefixCache remembers prompt prefixes by hash to simulate provider-side
// prompt caching.
type prefixCache struct {
	mu   sync.Mutex
	seen map[uint64]struct{}
}

const maxCacheEntries = 200_000

func newPrefixCache() *prefixCache { return &prefixCache{seen: map[uint64]struct{}{}} }

// point is a position in the prompt where a prefix may be cached.
type point struct {
	hash   uint64
	tokens int64
}

// prefixHasher hashes a prompt incrementally, piece by piece, so that the
// hash of every prefix is available.
type prefixHasher struct {
	h     hash.Hash64
	bytes int
}

func newPrefixHasher() *prefixHasher { return &prefixHasher{h: fnv.New64a()} }

func (p *prefixHasher) add(piece []byte) {
	_, _ = p.h.Write(piece)
	_, _ = p.h.Write([]byte{0})
	p.bytes += len(piece)
}

func (p *prefixHasher) point() point { return point{hash: p.h.Sum64(), tokens: tokens(p.bytes)} }

// lookbackPositions is how far back Anthropic searches from each breakpoint
// for an earlier cache entry.
const lookbackPositions = 20

// lookupBreakpoints simulates Anthropic prompt caching. positions are every
// block boundary in render order (tools, system, messages); breakpoints are
// the indexes of the positions carrying cache_control. Each breakpoint looks
// back up to 20 positions for a prefix an earlier request cached; entries
// are written only at breakpoints. It returns the tokens read from cache and
// the tokens newly written (from the end of the hit to the last breakpoint).
func (c *prefixCache) lookupBreakpoints(positions []point, breakpoints []int, minTokens int64) (read, written int64) {
	if len(breakpoints) == 0 {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > maxCacheEntries {
		c.seen = map[uint64]struct{}{}
	}
	for _, b := range breakpoints {
		for i := b; i >= 0 && i >= b-lookbackPositions; i-- {
			p := positions[i]
			if p.tokens < minTokens {
				break // shorter prefixes are never cached
			}
			if _, ok := c.seen[p.hash]; ok {
				read = max(read, p.tokens)
				break
			}
		}
	}
	for _, b := range breakpoints {
		if positions[b].tokens >= minTokens {
			c.seen[positions[b].hash] = struct{}{}
		}
	}
	if last := positions[breakpoints[len(breakpoints)-1]].tokens; last >= minTokens && last > read {
		written = last - read
	}
	return read, written
}

// lookup returns the longest cached prefix among points and, for writes,
// stores every point. It returns the cached tokens and the tokens newly
// written (from the end of the hit to the last point).
func (c *prefixCache) lookup(points []point, minTokens int64) (read, written int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > maxCacheEntries {
		c.seen = map[uint64]struct{}{}
	}
	var last int64
	for _, p := range points {
		if p.tokens < minTokens {
			continue
		}
		if _, ok := c.seen[p.hash]; ok && p.tokens > read {
			read = p.tokens
		}
		last = max(last, p.tokens)
	}
	for _, p := range points {
		if p.tokens >= minTokens {
			c.seen[p.hash] = struct{}{}
		}
	}
	if last > read {
		written = last - read
	}
	return read, written
}
