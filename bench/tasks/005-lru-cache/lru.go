// Package lru is a least-recently-used cache.
package lru

import "container/list"

type entry struct {
	key   string
	value int
}

// Cache holds at most capacity entries.
type Cache struct {
	capacity int
	order    *list.List // front is the most recently used
	items    map[string]*list.Element
}

// New returns an empty cache.
func New(capacity int) *Cache {
	return &Cache{capacity: capacity, order: list.New(), items: map[string]*list.Element{}}
}

// Get returns the value for key and whether it was present.
func (c *Cache) Get(key string) (int, bool) {
	el, ok := c.items[key]
	if !ok {
		return 0, false
	}
	return el.Value.(*entry).value, true
}

// Put stores value under key, evicting the least recently used entry if
// the cache is full.
func (c *Cache) Put(key string, value int) {
	if el, ok := c.items[key]; ok {
		el.Value.(*entry).value = value
		return
	}
	c.items[key] = c.order.PushFront(&entry{key, value})
	if c.order.Len() > c.capacity {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry).key)
	}
}

// Len returns the number of entries.
func (c *Cache) Len() int { return c.order.Len() }
