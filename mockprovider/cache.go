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
